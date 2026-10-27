package plugins

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var singlePost = map[string]any{"id": 42, "type": "post", "is_single": true}

func TestHelloGopherContentFilter(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	p := loadPlugin(t, rt, "examples/plugins/hello-gopher", host)
	ctx := context.Background()
	hooks := rt.Hooks()

	if !hooks.HasFilter("the_content") {
		dumpLogs(t, p)
		t.Fatal("the_content filter not registered")
	}

	// Option disabled: content unchanged.
	if got := hooks.ApplyFilters(ctx, "the_content", "<p>Hi</p>", singlePost); got != "<p>Hi</p>" {
		t.Fatalf("footer added while option disabled: %q", got)
	}

	host.SetOption("hello_gopher_footer", "1")
	got, ok := hooks.ApplyFilters(ctx, "the_content", "<p>Hi</p>", singlePost).(string)
	if !ok || !strings.HasPrefix(got, "<p>Hi</p>") || !strings.Contains(got, "Served by a Wasm plugin") {
		dumpLogs(t, p)
		t.Fatalf("unexpected filtered content: %#v", got)
	}

	// Archive views are left alone.
	archive := map[string]any{"id": 42, "type": "post", "is_single": false}
	if got := hooks.ApplyFilters(ctx, "the_content", "<p>Hi</p>", archive); got != "<p>Hi</p>" {
		t.Fatalf("footer added to a non-single view: %q", got)
	}
}

func TestHelloGopherAdminNotice(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	p := loadPlugin(t, rt, "examples/plugins/hello-gopher", host)

	rt.Hooks().DoAction(context.Background(), "admin_notices")
	notices := host.Notices()
	if len(notices) != 1 {
		dumpLogs(t, p)
		t.Fatalf("got %d notices, want 1", len(notices))
	}
	n := notices[0]
	if n.Plugin != "hello-gopher" || n.Type != "info" || !strings.HasPrefix(n.Message, "Hello, Gopher! ") {
		t.Fatalf("unexpected notice %+v", n)
	}
}

func TestHelloGopherWithoutNoticePermission(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	// The admin approved only content filtering: the admin_notices hook is
	// refused, the content filter still works.
	p := loadPlugin(t, rt, "examples/plugins/hello-gopher", host, PermContentFilter, PermOptionsRead)
	if rt.Hooks().HasAction("admin_notices") {
		t.Fatal("admin_notices registered without the admin:notices permission")
	}
	if !rt.Hooks().HasFilter("the_content") {
		dumpLogs(t, p)
		t.Fatal("the_content should still be registered")
	}
}

func TestWordCounter(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	loadPlugin(t, rt, "examples/plugins/word-counter", NewMemoryHost())

	stats := map[string]any{"chars": 30.0}
	post := map[string]any{"content": "<p>Hello <b>wasm</b> world, from Go!</p>"}
	got, ok := rt.Hooks().ApplyFilters(context.Background(), "wpgo_post_stats", stats, post).(map[string]any)
	if !ok {
		t.Fatalf("filter returned %T", got)
	}
	if got["word_count"] != 5.0 || got["chars"] != 30.0 {
		t.Fatalf("unexpected stats %v", got)
	}
}

func TestPermissionDenied(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	// Declared options:write, but the admin only granted options:read.
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", host, PermOptionsRead)
	got := rt.Hooks().ApplyFilters(context.Background(), "probe_update_option", "value")
	if got != "permission_denied" {
		dumpLogs(t, p)
		t.Fatalf("got %q, want permission_denied", got)
	}
	if _, ok := host.Option("probe_option"); ok {
		t.Fatal("option was written despite missing permission")
	}
	if d, _ := p.Disabled(); d {
		t.Fatal("permission errors must not count as plugin failures")
	}

	// With the permission granted the same call succeeds.
	rt2 := newTestRuntime(t, Config{})
	host2 := NewMemoryHost()
	loadPlugin(t, rt2, "internal/plugins/testdata/probe", host2, PermOptionsWrite)
	if got := rt2.Hooks().ApplyFilters(context.Background(), "probe_update_option", "value"); got != "ok" {
		t.Fatalf("got %q, want ok", got)
	}
	if v, _ := host2.Option("probe_option"); v != "value" {
		t.Fatalf("option = %v, want value", v)
	}
}

func TestStorage(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	loadPlugin(t, rt, "internal/plugins/testdata/probe", host)
	if got := rt.Hooks().ApplyFilters(context.Background(), "probe_storage", "abc"); got != "stored:abc" {
		t.Fatalf("got %q", got)
	}
	if v, ok, _ := host.StorageGet(context.Background(), "probe", "k"); !ok || v != "abc" {
		t.Fatalf("storage not scoped to plugin: %q %v", v, ok)
	}
}

func TestInfiniteLoopIsStoppedByTimeout(t *testing.T) {
	rt := newTestRuntime(t, Config{CallTimeout: 200 * time.Millisecond})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	hooks := rt.Hooks()

	start := time.Now()
	got := hooks.ApplyFilters(context.Background(), "probe_loop", "unchanged")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("loop ran for %v", elapsed)
	}
	if got != "unchanged" {
		t.Fatalf("filter value changed to %q", got)
	}
	if !logged(p, "timed out") {
		dumpLogs(t, p)
		t.Fatal("timeout not logged")
	}
	// The module was closed by the timeout; the next call re-instantiates it.
	if got := hooks.ApplyFilters(context.Background(), "probe_echo", "hi"); got != "hi!" {
		dumpLogs(t, p)
		t.Fatalf("plugin did not recover after timeout: %q", got)
	}
	if !logged(p, "echo called") {
		t.Fatal("guest stderr not captured in the plugin log")
	}
}

func TestExitIsRecovered(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	hooks := rt.Hooks()
	if got := hooks.ApplyFilters(context.Background(), "probe_exit", "v"); got != "v" {
		t.Fatalf("got %q", got)
	}
	if got := hooks.ApplyFilters(context.Background(), "probe_echo", "v"); got != "v!" {
		dumpLogs(t, p)
		t.Fatalf("plugin did not recover after exit: %q", got)
	}
	if n := len(p.Hooks()); n != 12 {
		t.Fatalf("re-instantiation duplicated hooks: %d registered", n)
	}
	if n := len(hooks.Owners("probe_echo")); n != 1 {
		t.Fatalf("probe_echo has %d owners", n)
	}
}

func TestRepeatedFailuresDisablePlugin(t *testing.T) {
	disabled := make(chan string, 1)
	rt := newTestRuntime(t, Config{MaxFailures: 3, OnDisable: func(slug, _ string) { disabled <- slug }})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	hooks := rt.Hooks()

	for i := 0; i < 3; i++ {
		if got := hooks.ApplyFilters(context.Background(), "probe_panic", "safe"); got != "safe" {
			t.Fatalf("call %d: got %q", i, got)
		}
	}
	if d, reason := p.Disabled(); !d || !strings.Contains(reason, "probe panic") {
		dumpLogs(t, p)
		t.Fatalf("plugin not disabled (reason %q)", reason)
	}
	if hooks.HasFilter("probe_echo") {
		t.Fatal("hooks of a disabled plugin are still registered")
	}
	select {
	case slug := <-disabled:
		if slug != "probe" {
			t.Fatalf("OnDisable slug %q", slug)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnDisable not called")
	}
}

func TestUnloadRemovesHooks(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	loadPlugin(t, rt, "examples/plugins/hello-gopher", NewMemoryHost())
	rt.Hooks().AddFilter("the_content", 10, func(_ context.Context, v any, _ ...any) any { return v })

	if err := rt.Unload(context.Background(), "hello-gopher"); err != nil {
		t.Fatal(err)
	}
	if owners := rt.Hooks().Owners("the_content"); len(owners) != 1 || owners[0] != CoreOwner {
		t.Fatalf("owners after unload: %v", owners)
	}
	if _, ok := rt.Plugin("hello-gopher"); ok {
		t.Fatal("plugin still listed after unload")
	}
	if err := rt.Unload(context.Background(), "hello-gopher"); err == nil {
		t.Fatal("second unload should fail")
	}
}

func TestPluginAndCoreHookOrdering(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	host.SetOption("hello_gopher_footer", true)
	loadPlugin(t, rt, "examples/plugins/hello-gopher", host) // priority 10
	hooks := rt.Hooks()
	hooks.AddFilter("the_content", 5, func(_ context.Context, v any, _ ...any) any { return v.(string) + "[5]" })
	hooks.AddFilter("the_content", 20, func(_ context.Context, v any, _ ...any) any { return v.(string) + "[20]" })

	got := hooks.ApplyFilters(context.Background(), "the_content", "body", singlePost).(string)
	i5, iPlugin, i20 := strings.Index(got, "[5]"), strings.Index(got, "Served by"), strings.Index(got, "[20]")
	if !(i5 >= 0 && i5 < iPlugin && iPlugin < i20) {
		t.Fatalf("wrong order: %q", got)
	}
}

func TestLoadRejectsDuplicateSlug(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	loadPlugin(t, rt, "examples/plugins/word-counter", NewMemoryHost())
	pkg := buildPackage(t, "examples/plugins/word-counter")
	if _, err := rt.Load(context.Background(), pkg, nil, NewMemoryHost()); err == nil {
		t.Fatal("loading the same slug twice should fail")
	}
}

func TestLoadRejectsNewerCoreRequirement(t *testing.T) {
	rt := newTestRuntime(t, Config{CoreVersion: "0.0.9"})
	pkg := buildPackage(t, "examples/plugins/word-counter") // requires 0.1.0
	if _, err := rt.Load(context.Background(), pkg, nil, NewMemoryHost()); err == nil ||
		!strings.Contains(err.Error(), "requires wpgo 0.1.0") {
		t.Fatalf("expected version error, got %v", err)
	}
}

func logged(p *Plugin, substr string) bool {
	for _, e := range p.Logs() {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

// reentrantHost fires admin_notices again from inside AdminNotice, the way a
// careless core implementation might.
type reentrantHost struct {
	*MemoryHost
	hooks *Registry
}

func (h reentrantHost) AdminNotice(ctx context.Context, plugin string, n Notice) error {
	h.hooks.DoAction(ctx, "admin_notices")
	return h.MemoryHost.AdminNotice(ctx, plugin, n)
}

func TestReentrantHookIsSkipped(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := reentrantHost{MemoryHost: NewMemoryHost(), hooks: rt.Hooks()}
	p := loadPlugin(t, rt, "examples/plugins/hello-gopher", host)

	done := make(chan struct{})
	go func() {
		rt.Hooks().DoAction(context.Background(), "admin_notices")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("re-entrant hook deadlocked")
	}
	if n := len(host.Notices()); n != 1 {
		dumpLogs(t, p)
		t.Fatalf("got %d notices, want 1", n)
	}
	if !logged(p, "re-entrant") {
		t.Fatal("re-entrant skip not logged")
	}
}

func TestCommandModuleIsRejected(t *testing.T) {
	// Without -buildmode=c-shared Go produces a WASI command (_start), whose
	// runtime exits after main returns, so exports cannot be called later.
	wasm := buildPluginMode(t, "examples/plugins/word-counter", "default")
	pkg := buildPackage(t, "examples/plugins/word-counter")
	pkg.Wasm = wasm
	rt := newTestRuntime(t, Config{})
	_, err := rt.Load(context.Background(), pkg, nil, NewMemoryHost())
	if err == nil || !strings.Contains(err.Error(), "c-shared") {
		t.Fatalf("want a c-shared hint, got %v", err)
	}
}

func TestDistPackages(t *testing.T) {
	pubText, err := os.ReadFile(moduleRoot + "/examples/plugins/dist/dev-key.pub")
	if err != nil {
		t.Skip("examples/plugins/dist not built; run tools/build-plugins.sh")
	}
	pub, err := ParsePublicKey(string(pubText))
	if err != nil {
		t.Fatal(err)
	}
	zips, _ := filepath.Glob(moduleRoot + "/examples/plugins/dist/*.zip")
	rt := newTestRuntime(t, Config{})
	for _, z := range zips {
		pkg, err := LoadPackageFile(z, []ed25519.PublicKey{pub}, true)
		if err != nil {
			t.Fatalf("%s: %v", z, err)
		}
		if _, err := rt.Load(context.Background(), pkg, pkg.Manifest.Permissions, NewMemoryHost()); err != nil {
			t.Fatalf("%s: %v", z, err)
		}
	}
	if len(zips) > 0 && len(rt.Plugins()) != len(zips) {
		t.Fatalf("loaded %d of %d packages", len(rt.Plugins()), len(zips))
	}
}
