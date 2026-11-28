package plugins

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"wpgo/internal/wp"
)

// Without content:html a shortcode's output and what a content filter adds go through KSES.
func TestOutputIsSanitizedWithoutContentHTML(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	pkg := buildPackage(t, "internal/plugins/testdata/probe")
	var perms []Permission
	for _, p := range pkg.Manifest.Permissions {
		if p != PermContentHTML {
			perms = append(perms, p)
		}
	}
	p, err := rt.Load(context.Background(), pkg, perms, NewMemoryHost())
	if err != nil {
		t.Fatal(err)
	}
	got := rt.Hooks().ApplyFilters(context.Background(), "the_excerpt", `<form action="/x"><input name="q"></form><p>core</p>`).(string)
	if !strings.HasPrefix(got, `<form action="/x"><input name="q"></form><p>core</p>`) {
		t.Errorf("the core's markup before the change was touched: %q", got)
	}
	if strings.Contains(got, "<script") || strings.Contains(got, "onclick") || !strings.Contains(got, "<b>bold</b>") {
		t.Errorf("the plugin's markup was not sanitised: %q", got)
	}
	out, _ := rt.DoShortcode(context.Background(), "probe", map[string]string{"a": "1"}, "inner")
	if out != "" { // "<probe a=1|inner>" is one unknown tag: KSES drops it whole
		t.Errorf("shortcode output not sanitised: %q", out)
	}
	// Front-end scripts need content:html.
	if s := rt.EnqueuedAssets("front", "script"); len(s) != 0 {
		t.Errorf("front script enqueued without content:html: %+v", s)
	}
	if !logged(p, `"content:html"`) {
		t.Error("refused front script not logged")
	}

	// With content:html the output is the plugin's own.
	rt2 := newTestRuntime(t, Config{})
	loadPlugin(t, rt2, "internal/plugins/testdata/probe", NewMemoryHost())
	if got := rt2.Hooks().ApplyFilters(context.Background(), "the_excerpt", "x").(string); !strings.Contains(got, "<script>") {
		t.Errorf("content:html output was sanitised: %q", got)
	}
}

func TestAdminScriptNeedsAdminScripts(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	if s := rt.EnqueuedAssets("admin", "script"); len(s) != 0 {
		t.Fatalf("admin script enqueued without admin:scripts: %+v", s)
	}
	if !logged(p, `"admin:scripts"`) {
		dumpLogs(t, p)
		t.Error("refused admin script not logged")
	}
	if !PermAdminScripts.Valid() || !PermAdminScripts.Dangerous() || !strings.Contains(PermAdminScripts.Description(), "full control") {
		t.Error("admin:scripts must be a dangerous permission with a full-control warning")
	}
}

func TestSanitizeChange(t *testing.T) {
	for _, c := range []struct{ in, out, want string }{
		{`<p>a</p>`, `<p>a</p><img src=x onerror=alert(1)>`, `<p>a</p><img src="x">`},
		{`<form><input></form>`, `<form><input></form><p>ok</p>`, `<form><input></form><p>ok</p>`},
		{`<a href="/x">t</a>`, `<a href="/x" onclick="e()">t</a>`, `<a href="/x">t</a>`},
		{`<p>one</p><p>two</p>`, `<p>one</p><iframe src=//e></iframe><p>two</p>`, `<p>one</p><p>two</p>`},
	} {
		got, ok := sanitizeChange(c.in, c.out)
		if !ok || got != c.want {
			t.Errorf("sanitizeChange(%q, %q) = %q, %v; want %q", c.in, c.out, got, ok, c.want)
		}
	}
	// A change inside a script cannot be sanitised as HTML: it is dropped.
	in := `<script>var a = 1;</script>`
	if got, ok := sanitizeChange(in, `<script>var a = 1;alert(2)</script>`); ok || got != in {
		t.Errorf("change inside a script kept: %q", got)
	}
	if got, ok := sanitizeChange(`<!-- x --><p>a</p>`, `<!-- x --><p>a</p><b>b</b>`); !ok || got != `<!-- x --><p>a</p><b>b</b>` {
		t.Errorf("closed comment before the change: %q %v", got, ok)
	}
	// Changes above and below (or around) the core's markup sanitise only what was added: the
	// form, the embed and the links of the original survive untouched.
	core := `<p>Intro</p><form action="/contact" method="post"><input name="n"><button>Send</button></form><iframe src="https://www.youtube.com/embed/x"></iframe><p><a href="https://example.com/">link</a> <a href="#">top</a></p>`
	for _, c := range []struct{ out, want string }{
		{`<p class="note">Top</p>` + core + `<p class="note">Bottom</p>`, `<p class="note">Top</p>` + core + `<p class="note">Bottom</p>`},
		{`<div class="wrap">` + core + `</div>`, `<div class="wrap">` + core + `</div>`},
		{`<p onclick="x()">Top</p>` + core + `<script>bad()</script>`, `<p>Top</p>` + core},
	} {
		got, ok := sanitizeChange(core, c.out)
		if !ok || got != c.want {
			t.Errorf("sanitizeChange around core:\n got %q %v\nwant %q", got, ok, c.want)
		}
	}
	// The original placed inside a tag, an attribute or a script is not text: the change is dropped.
	for _, out := range []string{`<a title="` + core + `">x</a>`, `<script>` + core + `</script>`, `<!--` + core + `-->`} {
		if got, ok := sanitizeChange(core, out); ok || got != core {
			t.Errorf("unsafe wrap kept: %q -> %q %v", out, got, ok)
		}
	}
	_ = wp.KsesPost
}

// A call that succeeds but uses most of its budget counts as a failure; a request that cannot get
// the instance in time gives up instead of queueing behind it.
func TestSlowCallsCountAndWaitersGiveUp(t *testing.T) {
	rt := newTestRuntime(t, Config{CallTimeout: 500 * time.Millisecond, MaxFailures: 2})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	hooks := rt.Hooks()

	done := make(chan any, 1)
	go func() { done <- hooks.ApplyFilters(context.Background(), "probe_slow", "v") }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	start := time.Now()
	if got := hooks.ApplyFilters(ctx, "probe_echo", "w"); got != "w" {
		t.Errorf("a waiter whose deadline passed got %q", got)
	}
	cancel()
	if waited := time.Since(start); waited > 300*time.Millisecond {
		t.Errorf("waiter blocked %v despite its deadline", waited)
	}
	if got := <-done; got != "v slow" {
		t.Fatalf("slow call result %q", got)
	}
	if d, _ := p.Disabled(); d {
		t.Fatal("disabled after one slow call")
	}
	if got := hooks.ApplyFilters(context.Background(), "probe_slow", "v"); got != "v slow" {
		t.Fatalf("second slow call %q", got)
	}
	if d, reason := p.Disabled(); !d || !strings.Contains(reason, "budget") {
		dumpLogs(t, p)
		t.Fatalf("two slow calls in a row did not disable the plugin: %q", reason)
	}
}

func TestLockCtxBusy(t *testing.T) {
	rt := newTestRuntime(t, Config{CallTimeout: 30 * time.Millisecond})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	p.lock()
	defer p.unlock()
	if err := p.lockCtx(context.Background()); !errors.Is(err, errBusy) {
		t.Fatalf("lockCtx on a busy plugin: %v", err)
	}
}

// A re-instantiated guest registers its hooks again; hooks the new instance no longer registers
// must not stay behind.
func TestReinstantiationDropsStaleHooks(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	host.SetOption("probe_conditional", true)
	loadPlugin(t, rt, "internal/plugins/testdata/probe", host)
	hooks := rt.Hooks()
	if got := hooks.ApplyFilters(context.Background(), "probe_conditional", "v"); got != "v conditional" {
		t.Fatalf("conditional hook: %q", got)
	}
	host.SetOption("probe_conditional", false)
	hooks.ApplyFilters(context.Background(), "probe_exit", "v") // kills the instance
	hooks.ApplyFilters(context.Background(), "probe_echo", "v") // re-instantiates
	if hooks.HasFilter("probe_conditional") {
		t.Fatal("a hook the new instance did not register is still wired")
	}
	if !hooks.HasFilter("probe_echo") {
		t.Fatal("hooks lost after re-instantiation")
	}
}

// A re-instantiation that fails (or whose caller gave up) must not leave an "active" plugin
// without hooks: the previous registrations stay, the failure counts, and the next call retries.
func TestFailedReinstantiationKeepsRegistrations(t *testing.T) {
	rt := newTestRuntime(t, Config{MaxFailures: 5})
	host := NewMemoryHost()
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", host)
	hooks := rt.Hooks()

	// Caller cancelled: re-instantiation runs detached and the plugin stays wired.
	hooks.ApplyFilters(context.Background(), "probe_exit", "v")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	hooks.ApplyFilters(cancelled, "probe_echo", "v")
	if !hooks.HasFilter("probe_echo") || len(p.Shortcodes()) != 1 || len(p.RestRoutes()) != 1 {
		t.Fatalf("registrations lost after a cancelled re-instantiation: %v %v", p.Shortcodes(), p.RestRoutes())
	}
	if got := hooks.ApplyFilters(context.Background(), "probe_echo", "v"); got != "v!" {
		t.Fatalf("echo after cancelled call: %q", got)
	}

	// wpgo_init fails: registrations are restored and the failure is counted.
	host.SetOption("probe_init_fail", true)
	hooks.ApplyFilters(context.Background(), "probe_exit", "v")
	p.state.Lock()
	before := p.failures
	p.state.Unlock()
	if got := hooks.ApplyFilters(context.Background(), "probe_echo", "v"); got != "v" {
		t.Fatalf("failed init must keep the value: %q", got)
	}
	p.state.Lock()
	after := p.failures
	p.state.Unlock()
	if after <= before {
		t.Fatalf("failed re-instantiation not counted: %d -> %d", before, after)
	}
	if !hooks.HasFilter("probe_echo") || len(p.Shortcodes()) != 1 || p.SettingsPage() == nil {
		t.Fatal("registrations lost after a failed re-instantiation")
	}
	host.SetOption("probe_init_fail", false)
	if got := hooks.ApplyFilters(context.Background(), "probe_echo", "v"); got != "v!" {
		t.Fatalf("retry after failed init: %q", got)
	}
}

// A stylesheet in the admin needs an admin permission; content:filter alone only reaches the
// front end.
func TestAdminStyleNeedsAdminPermission(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost(), PermContentFilter, PermContentHTML)
	if s := rt.EnqueuedAssets("admin", "style"); len(s) != 0 {
		t.Fatalf("admin style enqueued without an admin permission: %+v", s)
	}
	if !logged(p, "admin styles") {
		dumpLogs(t, p)
		t.Error("refused admin style not logged")
	}
	rt2 := newTestRuntime(t, Config{})
	loadPlugin(t, rt2, "internal/plugins/testdata/probe", NewMemoryHost(), PermContentFilter, PermAdminSettings)
	if s := rt2.EnqueuedAssets("admin", "style"); len(s) != 1 {
		t.Fatalf("admin style with admin:settings: %+v", s)
	}
}

// A callback that runs out its own budget is the plugin's fault and counts toward MaxFailures,
// even when the caller's deadline ends right after it (a REST request); a caller whose own
// deadline ends first does not count against the plugin.
func TestOwnBudgetTimeoutCountsCallerDeadlineDoesNot(t *testing.T) {
	rt := newTestRuntime(t, Config{CallTimeout: 150 * time.Millisecond, MaxFailures: 2})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	hooks := rt.Hooks()

	// The caller gives up first: not counted, however often it happens.
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		hooks.ApplyFilters(ctx, "probe_loop", "v")
		cancel()
	}
	if d, _ := p.Disabled(); d {
		t.Fatal("disabled for callers that gave up")
	}
	// The plugin's own budget runs out; the caller's deadline is later
	// but close (as servePluginRest's request deadline is to RestTimeout).
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		hooks.ApplyFilters(ctx, "probe_loop", "v")
		cancel()
	}
	if d, reason := p.Disabled(); !d || !strings.Contains(reason, "budget") {
		dumpLogs(t, p)
		t.Fatalf("two own-budget timeouts did not disable the plugin: %q", reason)
	}
}
