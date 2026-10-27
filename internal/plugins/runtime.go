package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Runtime errors.
var (
	ErrAlreadyLoaded  = errors.New("plugin is already loaded")
	ErrNotLoaded      = errors.New("plugin is not loaded")
	ErrPluginDisabled = errors.New("plugin is disabled")
)

// Config tunes a Runtime. Zero fields take the documented defaults.
type Config struct {
	// MemoryLimitPages caps each plugin's linear memory in 64 KiB pages.
	// Default 1024 (64 MiB).
	MemoryLimitPages uint32
	// CallTimeout bounds every hook callback. Default 2s.
	CallTimeout time.Duration
	// InitTimeout bounds instantiation plus wpgo_init. Default 10s.
	InitTimeout time.Duration
	// MaxFailures is the number of consecutive failed callbacks (trap,
	// timeout, panic, malformed response) after which a plugin is disabled.
	// Default 5.
	MaxFailures int
	// LogEntries is the size of each plugin's log ring buffer. Default 256.
	LogEntries int
	// MaxMessageSize caps JSON messages crossing the boundary. Default 8 MiB.
	MaxMessageSize uint32
	// MaxHTTPBody caps http_get response bodies. Default 4 MiB.
	MaxHTTPBody int
	// CacheDir, when set, persists compiled modules on disk; otherwise an
	// in-memory compilation cache is used.
	CacheDir string
	// CoreVersion is checked against the manifest's "requires" field.
	CoreVersion string
	// Logger, when set, receives a copy of every plugin log entry.
	Logger *slog.Logger
	// OnDisable is called (outside any lock) when a plugin is disabled after
	// repeated failures, so the core can persist the state or notify admins.
	OnDisable func(slug, reason string)
}

func (c *Config) setDefaults() {
	if c.MemoryLimitPages == 0 {
		c.MemoryLimitPages = 1024
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 2 * time.Second
	}
	if c.InitTimeout <= 0 {
		c.InitTimeout = 10 * time.Second
	}
	if c.MaxFailures <= 0 {
		c.MaxFailures = 5
	}
	if c.LogEntries <= 0 {
		c.LogEntries = 256
	}
	if c.MaxMessageSize == 0 {
		c.MaxMessageSize = 8 << 20
	}
	if c.MaxHTTPBody <= 0 {
		c.MaxHTTPBody = 4 << 20
	}
}

// Runtime hosts Wasm plugins in a single wazero runtime and connects them to
// a hook Registry. It is safe for concurrent use; calls into one plugin are
// serialised because a Wasm instance is single-threaded.
type Runtime struct {
	cfg   Config
	hooks *Registry
	wz    wazero.Runtime
	cache wazero.CompilationCache

	mu       sync.Mutex
	plugins  map[string]*Plugin
	byModule map[string]*Plugin // live module instance name -> plugin
	gen      uint64
}

// NewRuntime creates a runtime whose plugins register hooks in hooks.
func NewRuntime(ctx context.Context, hooks *Registry, cfg Config) (*Runtime, error) {
	if hooks == nil {
		return nil, errors.New("plugins: NewRuntime needs a hook registry")
	}
	cfg.setDefaults()
	cache := wazero.NewCompilationCache()
	if cfg.CacheDir != "" {
		c, err := wazero.NewCompilationCacheWithDir(cfg.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("plugins: compilation cache: %w", err)
		}
		cache = c
	}
	wz := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCompilationCache(cache).
		WithMemoryLimitPages(cfg.MemoryLimitPages).
		WithCloseOnContextDone(true))

	rt := &Runtime{
		cfg:      cfg,
		hooks:    hooks,
		wz:       wz,
		cache:    cache,
		plugins:  map[string]*Plugin{},
		byModule: map[string]*Plugin{},
	}
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, wz); err != nil {
		_ = wz.Close(ctx)
		return nil, fmt.Errorf("plugins: instantiate WASI: %w", err)
	}
	if err := rt.instantiateHostModule(ctx); err != nil {
		_ = wz.Close(ctx)
		return nil, err
	}
	return rt, nil
}

// Hooks returns the registry plugins register into.
func (rt *Runtime) Hooks() *Registry { return rt.hooks }

// Load compiles and instantiates a plugin package, runs its initialisation
// (wpgo_init) and wires the hooks it registers into the registry under its
// slug. granted is the set of permissions the administrator approved; the
// plugin gets the intersection of granted and declared permissions.
func (rt *Runtime) Load(ctx context.Context, pkg *Package, granted []Permission, host Host) (*Plugin, error) {
	if pkg == nil || pkg.Manifest == nil {
		return nil, errors.New("plugins: Load needs a package loaded with LoadPackage")
	}
	if host == nil {
		return nil, errors.New("plugins: Load needs a Host")
	}
	m := pkg.Manifest
	if err := m.CheckCompatible(rt.cfg.CoreVersion); err != nil {
		return nil, err
	}

	p := &Plugin{
		rt:        rt,
		manifest:  m,
		perms:     m.PermissionSet().Intersect(NewPermissionSet(granted...)),
		host:      host,
		regs:      map[RegisteredHook]HookID{},
		pkgAssets: pkg.Assets,
	}
	p.logs = newLogRing(rt.cfg.LogEntries, rt.logSink(m.Slug))

	rt.mu.Lock()
	if _, exists := rt.plugins[m.Slug]; exists {
		rt.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAlreadyLoaded, m.Slug)
	}
	rt.plugins[m.Slug] = p
	rt.mu.Unlock()

	fail := func(err error) (*Plugin, error) {
		rt.hooks.RemoveAll(m.Slug)
		if p.compiled != nil {
			_ = p.compiled.Close(ctx)
		}
		rt.mu.Lock()
		delete(rt.plugins, m.Slug)
		rt.mu.Unlock()
		return nil, fmt.Errorf("plugin %s: %w", m.Slug, err)
	}

	compiled, err := rt.wz.CompileModule(ctx, pkg.Wasm)
	if err != nil {
		return fail(fmt.Errorf("compile plugin.wasm: %w", err))
	}
	p.compiled = compiled
	if err := checkModuleShape(compiled); err != nil {
		return fail(err)
	}

	p.mu.Lock()
	err = p.instantiate(ctx)
	p.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	p.logs.add("info", "runtime", fmt.Sprintf("loaded %s %s with permissions %v", m.Slug, m.Version, p.perms.List()))
	return p, nil
}

// Unload removes the plugin's hooks and closes its module.
func (rt *Runtime) Unload(ctx context.Context, slug string) error {
	rt.mu.Lock()
	p, ok := rt.plugins[slug]
	delete(rt.plugins, slug)
	rt.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotLoaded, slug)
	}
	p.shutdown(ctx, "unloaded")
	if p.compiled != nil {
		return p.compiled.Close(ctx)
	}
	return nil
}

// Plugin returns a loaded (possibly disabled) plugin.
func (rt *Runtime) Plugin(slug string) (*Plugin, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	p, ok := rt.plugins[slug]
	return p, ok
}

// Plugins returns all loaded plugins sorted by slug.
func (rt *Runtime) Plugins() []*Plugin {
	rt.mu.Lock()
	out := make([]*Plugin, 0, len(rt.plugins))
	for _, p := range rt.plugins {
		out = append(out, p)
	}
	rt.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Slug() < out[j].Slug() })
	return out
}

// Close unloads every plugin and releases the wazero runtime.
func (rt *Runtime) Close(ctx context.Context) error {
	for _, p := range rt.Plugins() {
		_ = rt.Unload(ctx, p.Slug())
	}
	err := rt.wz.Close(ctx)
	if cerr := rt.cache.Close(ctx); err == nil {
		err = cerr
	}
	return err
}

// nextModuleName reserves a unique instance name for p.
func (rt *Runtime) nextModuleName(p *Plugin) string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.gen++
	name := fmt.Sprintf("%s#%d", p.Slug(), rt.gen)
	rt.byModule[name] = p
	return name
}

func (rt *Runtime) forgetModule(name string) {
	rt.mu.Lock()
	delete(rt.byModule, name)
	rt.mu.Unlock()
}

func (rt *Runtime) pluginForModule(name string) *Plugin {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.byModule[name]
}

func (rt *Runtime) logSink(slug string) func(LogEntry) {
	if rt.cfg.Logger == nil {
		return nil
	}
	return func(e LogEntry) {
		level := slog.LevelInfo
		switch e.Level {
		case "debug":
			level = slog.LevelDebug
		case "warning":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
		rt.cfg.Logger.Log(context.Background(), level, e.Message, "plugin", slug, "source", e.Source)
	}
}

// Required guest exports (see README.md for the ABI).
const (
	exportAlloc  = "wpgo_alloc"
	exportFree   = "wpgo_free"
	exportHandle = "wpgo_handle"
	exportInit   = "wpgo_init"
)

// checkModuleShape verifies imports and exports before instantiation so that
// authors get a clear error instead of a link failure.
func checkModuleShape(c wazero.CompiledModule) error {
	for _, fn := range c.ImportedFunctions() {
		mod, name, _ := fn.Import()
		if mod != hostModuleName && mod != wasi_snapshot_preview1.ModuleName {
			return fmt.Errorf("plugin.wasm imports %s.%s; only %q and %q imports are available",
				mod, name, hostModuleName, wasi_snapshot_preview1.ModuleName)
		}
	}
	exports := c.ExportedFunctions()
	if _, ok := exports["_initialize"]; !ok {
		if _, isCommand := exports["_start"]; isCommand {
			return errors.New("plugin.wasm is a WASI command module (it exports _start); " +
				"build it as a reactor, e.g. go build -buildmode=c-shared")
		}
	}
	want := map[string][2]int{ // name -> (#params, #results)
		exportAlloc: {1, 1}, exportFree: {2, 0}, exportHandle: {2, 1}, exportInit: {0, 0},
	}
	for name, shape := range want {
		def, ok := exports[name]
		if !ok {
			return fmt.Errorf("plugin.wasm does not export %s (is it built with the wpgo SDK?)", name)
		}
		if len(def.ParamTypes()) != shape[0] || len(def.ResultTypes()) != shape[1] {
			return fmt.Errorf("plugin.wasm export %s has the wrong signature", name)
		}
	}
	if def := exports[exportHandle]; def.ResultTypes()[0] != api.ValueTypeI64 {
		return fmt.Errorf("plugin.wasm export %s must return i64", exportHandle)
	}
	return nil
}
