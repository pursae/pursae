package plugins

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"wpgo/internal/cache"
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

	// sem (capacity 1) serialises every entry into the guest and guards
	// mod/modName. It is a channel rather than a mutex so that a request can
	// stop waiting when its context ends (lockCtx).
	sem     chan struct{}
	mod     api.Module
	modName string
	// exports caches the guest exports the runtime calls (callExport), for the module instance
	// in exportsMod. Guarded by sem like mod.
	exports    map[string]*cachedExport
	exportsMod api.Module

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

	// UI registrations (ui.go), guarded by state.
	adminPages     []AdminPage
	dashWidgets    []DashboardWidget
	metaBoxes      []MetaBox
	blocks         []BlockType
	builderWidgets []BuilderWidget
}

// Slug returns the plugin slug.
func (p *Plugin) Slug() string { return p.manifest.Slug }

// Manifest returns an independent snapshot of the plugin manifest.
func (p *Plugin) Manifest() *Manifest { return cloneManifest(p.manifest) }

func cloneManifest(m *Manifest) *Manifest {
	if m == nil {
		return nil
	}
	out := *m
	out.RequiresPlugins = slices.Clone(m.RequiresPlugins)
	out.Permissions = slices.Clone(m.Permissions)
	out.Hooks = slices.Clone(m.Hooks)
	out.Shortcodes = slices.Clone(m.Shortcodes)
	out.Assets = maps.Clone(m.Assets)
	return &out
}

// Permissions returns the effective (declared and granted) permissions.
func (p *Plugin) Permissions() []Permission { return p.perms.List() }

// HasPermission reports whether the plugin holds perm.
func (p *Plugin) HasPermission(perm Permission) bool { return p.perms.Has(perm) }

// HostLog adds an entry from the host (source "host") to the plugin's log.
func (p *Plugin) HostLog(level, msg string) { p.logs.add(level, "host", msg) }

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

// lock takes the plugin's instance lock unconditionally (load, unload).
func (p *Plugin) lock() { p.sem <- struct{}{} }

// unlock releases the instance lock.
func (p *Plugin) unlock() { <-p.sem }

// errBusy is returned when a hook gave up waiting for a plugin that was busy
// serving another request.
var errBusy = errors.New("plugin busy: another request is still running in it")

// lockCtx takes the instance lock unless ctx ends first or the wait exceeds
// the call timeout: a slow plugin then delays a request by at most one call
// timeout instead of queueing every request of the site behind it. One Wasm
// instance runs one call at a time; plugins may keep state in memory between
// calls, so there is deliberately no pool of instances.
func (p *Plugin) lockCtx(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	default:
	}
	t := time.NewTimer(p.rt.cfg.CallTimeout)
	defer t.Stop()
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return errBusy
	}
}

// instantiate creates a fresh module instance, runs _initialize (Go's reactor
// entry point) and wpgo_init. Callers hold the instance lock.
//
// A fresh instance starts with empty guest memory and registers its hooks,
// shortcodes, routes, settings page and assets again from wpgo_init, so the
// previous instance's registrations are dropped first: callback ids are
// generated by the guest and a re-instantiated guest may register a different
// set (for example depending on an option), which must not leave stale
// entries pointing at callbacks that no longer exist. When the new instance
// fails to start, the previous registrations are put back so the plugin stays
// reachable and the next call tries again (instead of an "active" plugin that
// no hook ever reaches).
func (p *Plugin) instantiate(parent context.Context) (err error) {
	if p.mod != nil {
		_ = p.mod.Close(parent)
		p.rt.forgetModule(p.modName)
		p.mod, p.exports, p.exportsMod = nil, nil, nil
	}
	snap := p.snapshotRegistrations()
	p.resetRegistrations()
	defer func() {
		if err != nil {
			p.restoreRegistrations(snap)
		}
	}()
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

// cachedExport is a guest export kept for repeated calls. wazero builds a new call engine, with
// its own stack (about 10 KiB), on every ExportedFunction; every host call needs wpgo_alloc for its
// response, so fetching the function per call cost an allocation of that size per host call.
type cachedExport struct {
	fn   api.Function
	busy bool // a call through fn is running
}

// callExport calls the guest export name of instance m through a function cached per instance.
// A call engine must not be re-entered while it runs (its stack is in use), so a nested call of
// the same export — a guest whose wpgo_alloc calls a host import, which calls wpgo_alloc again —
// gets a fresh function instead. Callers hold the instance lock (sem), and host functions run on
// the goroutine of the guest call that holds it, so the cache needs no other locking.
func (p *Plugin) callExport(ctx context.Context, m api.Module, name string, params ...uint64) ([]uint64, error) {
	if p.exportsMod != m {
		p.exports, p.exportsMod = map[string]*cachedExport{}, m
	}
	c := p.exports[name]
	if c == nil {
		f := m.ExportedFunction(name)
		if f == nil {
			return nil, fmt.Errorf("%s is not exported", name)
		}
		c = &cachedExport{fn: f}
		p.exports[name] = c
	}
	if c.busy {
		return m.ExportedFunction(name).Call(ctx, params...)
	}
	c.busy = true
	defer func() { c.busy = false }()
	return c.fn.Call(ctx, params...)
}

// errReentrant is returned when a hook would call back into a plugin that is
// already executing on the same call chain.
var errReentrant = errors.New("re-entrant call skipped")

// errReinstantiate wraps the error of a failed re-instantiation of a
// terminated module instance.
var errReinstantiate = errors.New("re-instantiate")

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

	if err := p.lockCtx(ctx); err != nil {
		// Waiting for the instance is not the plugin's fault (another request holds it), so it
		// does not count as a failure; the hook keeps its value.
		p.logs.add("warning", "runtime", fmt.Sprintf("%s (%s) skipped: %v", req.Hook, req.Callback, err))
		cache.MarkDegraded(ctx) // the page lacks this plugin's output: do not cache it
		return nil, err
	}
	defer p.unlock()
	if p.disabled.Load() {
		return nil, ErrPluginDisabled
	}
	value, took, err := p.callGuest(ctx, body, p.rt.cfg.budget(req))
	if err != nil {
		p.logs.add("error", "runtime", fmt.Sprintf("%s (%s) failed: %v", req.Hook, req.Callback, err))
		// The caller giving up is not the plugin's fault; a failed re-instantiation always is
		// (it runs detached from the caller's context), so repeated init failures end in a
		// visible "disabled" state instead of retrying forever.
		if ctx.Err() == nil || errors.Is(err, errReinstantiate) {
			p.recordFailure(ctx, err)
		}
		return nil, err
	}
	// A call that used most of its time budget blocked every other request to this plugin
	// for that long; it counts toward the failure limit like a timeout (the value is kept).
	if !p.rt.cfg.slowExempt(req) && took >= p.rt.cfg.slowCall() {
		slow := fmt.Errorf("%s (%s) took %s of its %s budget", req.Hook, req.Callback, took.Round(time.Millisecond), p.rt.cfg.CallTimeout)
		p.logs.add("warning", "runtime", slow.Error())
		p.recordFailure(ctx, slow)
		return value, nil
	}
	p.state.Lock()
	p.failures = 0
	p.state.Unlock()
	return value, nil
}

// callGuest writes body into guest memory, calls wpgo_handle and decodes the
// response. Callers hold the instance lock.
// took is the time spent in wpgo_handle (a re-instantiation before it is not
// counted).
func (p *Plugin) callGuest(parent context.Context, body []byte, budget time.Duration) (value json.RawMessage, took time.Duration, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("host panic while calling plugin: %v", r)
		}
	}()
	if p.mod == nil || p.mod.IsClosed() {
		if p.mod != nil {
			p.logs.add("warning", "runtime", "module instance was terminated; re-instantiating")
		}
		// Detached from the request: a visitor disconnecting must not abort the plugin's
		// start-up halfway (instantiate applies its own InitTimeout).
		if err := p.instantiate(context.WithoutCancel(parent)); err != nil {
			return nil, 0, fmt.Errorf("%w: %w", errReinstantiate, err)
		}
	}
	ctx, cancel := context.WithTimeout(withActive(parent, p), budget)
	defer cancel()

	start := time.Now()
	mod := p.mod
	ptr, err := writeGuest(ctx, p, mod, body)
	if err != nil {
		return nil, time.Since(start), describeCallError(ctx, err)
	}
	res, err := p.callExport(ctx, mod, exportHandle, uint64(ptr), uint64(len(body)))
	took = time.Since(start)
	if err != nil {
		return nil, took, describeCallError(ctx, err)
	}
	out, err := readGuest(ctx, p, mod, res[0], p.rt.cfg.MaxMessageSize, true)
	if err != nil {
		return nil, took, err
	}
	var resp handleResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, took, fmt.Errorf("malformed wpgo_handle response: %w", err)
	}
	if resp.Error != "" {
		return nil, took, errors.New("plugin error: " + resp.Error)
	}
	return resp.Value, took, nil
}

// recordFailure counts a failed call and disables the plugin after
// MaxFailures consecutive failures (or slow calls). Callers hold the
// instance lock.
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
	p.removeHooks()
	p.lock()
	defer p.unlock()
	p.disableLocked(ctx, reason)
}

// disableLocked disables the plugin; callers hold the instance lock.
func (p *Plugin) disableLocked(ctx context.Context, reason string) {
	first := p.markDisabled(reason)
	p.removeHooks()
	if p.mod != nil {
		_ = p.mod.Close(ctx)
		p.rt.forgetModule(p.modName)
		p.mod, p.exports, p.exportsMod = nil, nil, nil
	}
	if first {
		p.logs.add("error", "runtime", reason)
	}
}

// removeHooks removes this instance's registrations. An overlapping Load may have registered
// new hooks under the same slug after Unload released the runtime's reservation.
func (p *Plugin) removeHooks() {
	p.state.Lock()
	defer p.state.Unlock()
	for _, id := range p.regs {
		p.rt.hooks.Remove(id)
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
		return p.sanitizeFilter(h.Hook, value, out)
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
