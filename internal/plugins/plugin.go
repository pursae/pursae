package plugins

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// RegisteredHook describes one hook a plugin subscribed to.
type RegisteredHook struct {
	Type     string `json:"type"` // "filter" or "action"
	Hook     string `json:"hook"`
	Priority int    `json:"priority"`
	Callback string `json:"callback"`
}

// Plugin is a loaded plugin instance.
type Plugin struct {
	rt       *Runtime
	manifest *Manifest
	perms    PermissionSet
	host     Host
	compiled wazero.CompiledModule
	logs     *logRing

	// mu serialises every entry into the guest and guards mod/modName.
	mu      sync.Mutex
	mod     api.Module
	modName string

	// state guards regs, failures and reason. It is never held while
	// calling into the guest.
	state    sync.Mutex
	regs     map[RegisteredHook]HookID
	failures int
	reason   string
	disabled atomic.Bool

	// Other registrations (registrations.go), guarded by state.
	shortcodes map[string]string // tag -> callback
	rest       []RestRoute
	settings   *SettingsPage
	assets     []Asset
	pkgAssets  map[string][]byte // files under assets/ in the package (read-only)
}

// Slug returns the plugin slug.
func (p *Plugin) Slug() string { return p.manifest.Slug }

// Manifest returns the plugin manifest.
func (p *Plugin) Manifest() *Manifest { return p.manifest }

// Permissions returns the effective (declared and granted) permissions.
func (p *Plugin) Permissions() []Permission { return p.perms.List() }

// HasPermission reports whether the plugin holds perm.
func (p *Plugin) HasPermission(perm Permission) bool { return p.perms.Has(perm) }

// Logs returns the plugin's recent log entries, oldest first.
func (p *Plugin) Logs() []LogEntry { return p.logs.list() }

// Disabled reports whether the plugin was disabled (after repeated failures
// or because it was unloaded) and why.
func (p *Plugin) Disabled() (bool, string) {
	if !p.disabled.Load() {
		return false, ""
	}
	p.state.Lock()
	defer p.state.Unlock()
	return true, p.reason
}

// Hooks returns the hooks the plugin registered.
func (p *Plugin) Hooks() []RegisteredHook {
	p.state.Lock()
	out := make([]RegisteredHook, 0, len(p.regs))
	for h := range p.regs {
		out = append(out, h)
	}
	p.state.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hook != out[j].Hook {
			return out[i].Hook < out[j].Hook
		}
		return out[i].Callback < out[j].Callback
	})
	return out
}

// instantiate creates a fresh module instance, runs _initialize (Go's reactor
// entry point) and wpgo_init. Callers hold p.mu.
func (p *Plugin) instantiate(parent context.Context) error {
	if p.mod != nil {
		_ = p.mod.Close(parent)
		p.rt.forgetModule(p.modName)
		p.mod = nil
	}
	name := p.rt.nextModuleName(p)
	cfg := wazero.NewModuleConfig().
		WithName(name).
		WithStartFunctions(). // run _initialize explicitly below
		WithStdout(&lineWriter{ring: p.logs, level: "info", source: "stdout"}).
		WithStderr(&lineWriter{ring: p.logs, level: "warning", source: "stderr"}).
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(rand.Reader)

	ctx, cancel := context.WithTimeout(withActive(parent, p), p.rt.cfg.InitTimeout)
	defer cancel()
	mod, err := p.rt.wz.InstantiateModule(ctx, p.compiled, cfg)
	if err != nil {
		p.rt.forgetModule(name)
		return fmt.Errorf("instantiate: %w", describeCallError(ctx, err))
	}
	for _, fn := range []string{"_initialize", exportInit} {
		f := mod.ExportedFunction(fn)
		if f == nil {
			continue
		}
		if _, err := f.Call(ctx); err != nil {
			_ = mod.Close(parent)
			p.rt.forgetModule(name)
			return fmt.Errorf("%s: %w", fn, describeCallError(ctx, err))
		}
	}
	p.mod, p.modName = mod, name
	return nil
}

// errReentrant is returned when a hook would call back into a plugin that is
// already executing on the same call chain.
var errReentrant = errors.New("re-entrant call skipped")

// handleRequest is the JSON sent to the guest's wpgo_handle export.
type handleRequest struct {
	Plugin   string `json:"plugin"`
	Callback string `json:"callback"`
	Hook     string `json:"hook"`
	Type     string `json:"type"`
	Args     []any  `json:"args"`
	Value    any    `json:"value,omitempty"`
}

// handleResponse is the JSON returned by wpgo_handle.
type handleResponse struct {
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

// call runs one hook callback inside the guest, applying the timeout and the
// failure accounting. It never panics.
func (p *Plugin) call(ctx context.Context, req *handleRequest) (json.RawMessage, error) {
	if p.disabled.Load() {
		return nil, ErrPluginDisabled
	}
	if isActive(ctx, p) {
		p.logs.add("debug", "runtime", fmt.Sprintf("skipped re-entrant call of %s on %s", req.Callback, req.Hook))
		return nil, errReentrant
	}
	if req.Args == nil {
		req.Args = []any{}
	}
	body, err := json.Marshal(req)
	if err != nil {
		p.logs.add("error", "runtime", fmt.Sprintf("%s: cannot encode hook arguments: %v", req.Hook, err))
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled.Load() {
		return nil, ErrPluginDisabled
	}
	value, err := p.callGuest(ctx, body)
	if err != nil {
		p.logs.add("error", "runtime", fmt.Sprintf("%s (%s) failed: %v", req.Hook, req.Callback, err))
		if ctx.Err() == nil { // the caller giving up is not the plugin's fault
			p.recordFailure(ctx, err)
		}
		return nil, err
	}
	p.state.Lock()
	p.failures = 0
	p.state.Unlock()
	return value, nil
}

// callGuest writes body into guest memory, calls wpgo_handle and decodes the
// response. Callers hold p.mu.
func (p *Plugin) callGuest(parent context.Context, body []byte) (value json.RawMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("host panic while calling plugin: %v", r)
		}
	}()
	if p.mod == nil || p.mod.IsClosed() {
		if p.mod != nil {
			p.logs.add("warning", "runtime", "module instance was terminated; re-instantiating")
		}
		if err := p.instantiate(parent); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(withActive(parent, p), p.rt.cfg.CallTimeout)
	defer cancel()

	mod := p.mod
	ptr, err := writeGuest(ctx, mod, body)
	if err != nil {
		return nil, describeCallError(ctx, err)
	}
	res, err := mod.ExportedFunction(exportHandle).Call(ctx, uint64(ptr), uint64(len(body)))
	if err != nil {
		return nil, describeCallError(ctx, err)
	}
	out, err := readGuest(ctx, mod, res[0], p.rt.cfg.MaxMessageSize, true)
	if err != nil {
		return nil, err
	}
	var resp handleResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("malformed wpgo_handle response: %w", err)
	}
	if resp.Error != "" {
		return nil, errors.New("plugin error: " + resp.Error)
	}
	return resp.Value, nil
}

// recordFailure counts a failed call and disables the plugin after
// MaxFailures consecutive failures. Callers hold p.mu.
func (p *Plugin) recordFailure(ctx context.Context, cause error) {
	p.state.Lock()
	p.failures++
	n := p.failures
	p.state.Unlock()
	if n < p.rt.cfg.MaxFailures {
		return
	}
	reason := fmt.Sprintf("disabled after %d consecutive failures; last error: %v", n, cause)
	p.disableLocked(context.WithoutCancel(ctx), reason)
	if p.rt.cfg.OnDisable != nil {
		go p.rt.cfg.OnDisable(p.Slug(), reason)
	}
}

// shutdown disables the plugin, removes its hooks and closes its module.
func (p *Plugin) shutdown(ctx context.Context, reason string) {
	p.markDisabled(reason)
	p.rt.hooks.RemoveAll(p.Slug())
	p.mu.Lock()
	defer p.mu.Unlock()
	p.disableLocked(ctx, reason)
}

// disableLocked disables the plugin; callers hold p.mu.
func (p *Plugin) disableLocked(ctx context.Context, reason string) {
	first := p.markDisabled(reason)
	p.rt.hooks.RemoveAll(p.Slug())
	if p.mod != nil {
		_ = p.mod.Close(ctx)
		p.rt.forgetModule(p.modName)
		p.mod = nil
	}
	if first {
		p.logs.add("error", "runtime", reason)
	}
}

func (p *Plugin) markDisabled(reason string) (first bool) {
	p.state.Lock()
	defer p.state.Unlock()
	if p.disabled.Load() {
		return false
	}
	p.reason = reason
	p.disabled.Store(true)
	return true
}

// registerHook wires a guest callback into the registry. Re-registrations of
// an identical hook (after the module is re-instantiated) are no-ops.
func (p *Plugin) registerHook(h RegisteredHook) error {
	p.state.Lock()
	defer p.state.Unlock()
	if p.disabled.Load() {
		return ErrPluginDisabled
	}
	if _, ok := p.regs[h]; ok {
		return nil
	}
	scope := p.rt.hooks.Owner(p.Slug())
	var id HookID
	if h.Type == "filter" {
		id = scope.AddFilter(h.Hook, h.Priority, p.filterFunc(h))
	} else {
		id = scope.AddAction(h.Hook, h.Priority, p.actionFunc(h))
	}
	p.regs[h] = id
	return nil
}

func (p *Plugin) filterFunc(h RegisteredHook) FilterFunc {
	return func(ctx context.Context, value any, args ...any) any {
		raw, err := p.call(ctx, &handleRequest{
			Plugin: p.Slug(), Callback: h.Callback, Hook: h.Hook, Type: h.Type, Args: args, Value: value,
		})
		if err != nil {
			return value
		}
		if len(raw) == 0 {
			p.logs.add("warning", "runtime", fmt.Sprintf("%s (%s) returned no value; keeping the original", h.Hook, h.Callback))
			return value
		}
		out, err := decodeLike(value, raw)
		if err != nil {
			p.logs.add("warning", "runtime", fmt.Sprintf("%s (%s) returned a value of the wrong type: %v", h.Hook, h.Callback, err))
			return value
		}
		return out
	}
}

func (p *Plugin) actionFunc(h RegisteredHook) ActionFunc {
	return func(ctx context.Context, args ...any) {
		_, _ = p.call(ctx, &handleRequest{
			Plugin: p.Slug(), Callback: h.Callback, Hook: h.Hook, Type: h.Type, Args: args,
		})
	}
}

// decodeLike decodes raw into a value of the same dynamic type as orig, so a
// filter that receives a string returns a string and a struct stays a struct.
func decodeLike(orig any, raw json.RawMessage) (any, error) {
	if orig == nil {
		var v any
		err := json.Unmarshal(raw, &v)
		return v, err
	}
	ptr := reflect.New(reflect.TypeOf(orig))
	if err := json.Unmarshal(raw, ptr.Interface()); err != nil {
		return nil, err
	}
	return ptr.Elem().Interface(), nil
}

// activeKey marks, in a context, the plugins currently executing on this call
// chain.
type activeKey struct{}

type activeSet struct {
	p    *Plugin
	next *activeSet
}

func withActive(ctx context.Context, p *Plugin) context.Context {
	cur, _ := ctx.Value(activeKey{}).(*activeSet)
	return context.WithValue(ctx, activeKey{}, &activeSet{p: p, next: cur})
}

func isActive(ctx context.Context, p *Plugin) bool {
	for s, _ := ctx.Value(activeKey{}).(*activeSet); s != nil; s = s.next {
		if s.p == p {
			return true
		}
	}
	return false
}

// describeCallError makes wazero errors readable, naming timeouts explicitly.
func describeCallError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out: %w", err)
	}
	return err
}
