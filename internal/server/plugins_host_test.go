package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"wpgo/internal/auth"
	"wpgo/internal/caps"
	"wpgo/internal/plugins"
	"wpgo/internal/store"
)

var (
	demoOnce sync.Once
	demoZip  []byte
	demoErr  string
)

// demoPackage builds examples/plugins/shortcode-demo for wasip1/wasm and packs it (unsigned).
func demoPackage(t *testing.T) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Wasm plugin build in -short mode")
	}
	demoOnce.Do(func() {
		dir := "../../examples/plugins/shortcode-demo"
		out := filepath.Join(t.TempDir(), "demo.wasm")
		cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./examples/plugins/shortcode-demo")
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			demoErr = string(output)
			return
		}
		wasm, _ := os.ReadFile(out)
		manifest, _ := os.ReadFile(filepath.Join(dir, "plugin.json"))
		css, _ := os.ReadFile(filepath.Join(dir, "assets", "style.css"))
		var buf bytes.Buffer
		if err := plugins.WritePackage(&buf, manifest, wasm, map[string][]byte{"assets/style.css": css}, nil); err != nil {
			demoErr = err.Error()
			return
		}
		demoZip = buf.Bytes()
	})
	if demoErr != "" {
		if strings.Contains(demoErr, "unsupported GOOS/GOARCH") {
			t.Skip(demoErr)
		}
		t.Fatal(demoErr)
	}
	return demoZip
}

// installDemo installs and activates the demo plugin with all its permissions, as the Plugins
// screen does after the consent step.
func installDemo(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	srv.cfg.AllowUnsignedPlugins = true
	if err := srv.settings.Set(ctx, map[string]any{"plugin_allow_unsigned": true}); err != nil {
		t.Fatal(err)
	}
	m, err := srv.installPackage(ctx, demoPackage(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m.Permissions)
	if err := srv.loadPlugin(ctx, m.Slug, raw); err != nil {
		t.Fatal(err)
	}
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'active', permissions = $2 WHERE slug = $1`, m.Slug, raw)
	srv.plugins.hooks.DoAction(ctx, "init")
}

func restNonce(t *testing.T, srv *Server, c *http.Client, base string) string {
	t.Helper()
	u, _ := url.Parse(base)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == auth.CookieName {
			sess, err := srv.auth.Lookup(context.Background(), ck.Value)
			if err != nil {
				t.Fatal(err)
			}
			return srv.auth.Nonce("wp_rest", sess.UserID, sess.Token)
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func TestPluginShortcodeSettingsAssetsRestAndCron(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	installDemo(t, srv)
	ctx := context.Background()

	// A post using the shortcode: expanded on the front end, styled by the package asset,
	// greeting the logged-in reader (get_current_user) but not anonymous visitors.
	p := &store.Post{Type: "post", Status: "publish", Title: "Facts", Content: "<p>[gopher-facts count=\"2\"]</p>\n\n[recent-gophers]", AuthorID: 1}
	if err := srv.store.InsertPost(ctx, p); err != nil {
		t.Fatal(err)
	}
	link := srv.links(ctx).Permalink(p)
	code, body, _ := get(t, c, link)
	if code != http.StatusOK {
		t.Fatalf("post page %d", code)
	}
	for _, want := range []string{`<div class="gopher-facts gopher-facts--cards">`, "<h3>Did you know?</h3>", "Hello, admin!",
		`href='/wp-content/plugins/shortcode-demo/assets/style.css?ver=1.0.0'`, `<ul class="recent-gophers"><li><a href="`, ">Facts</a>"} {
		if !strings.Contains(body, want) {
			t.Errorf("post page misses %q", want)
		}
	}
	factsDiv := regexp.MustCompile(`(?s)<div class="gopher-facts.*?</div>`)
	if n := strings.Count(factsDiv.FindString(body), "<li>"); n != 2 {
		t.Errorf("expected 2 facts, got %d", n)
	}
	if strings.Contains(body, "[gopher-facts") || strings.Contains(body, "<p></p>") {
		t.Errorf("shortcode left in the content")
	}
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	if _, body, _ := get(t, anon, link); strings.Contains(body, "Hello, admin!") || !strings.Contains(body, "gopher-facts") {
		t.Errorf("anonymous view wrong")
	}

	// Package assets are served from the loaded package; nothing else is.
	resp, err := anon.Get(base + "/wp-content/plugins/shortcode-demo/assets/style.css")
	if err != nil {
		t.Fatal(err)
	}
	css, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") || !strings.Contains(string(css), ".gopher-facts") {
		t.Errorf("asset: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, bad := range []string{"/wp-content/plugins/shortcode-demo/assets/plugin.json", "/wp-content/plugins/shortcode-demo/assets/../plugin.wasm", "/wp-content/plugins/nope/assets/style.css"} {
		// Path traversal is refused by the core firewall (403) before the asset handler (404).
		if code, _, _ := get(t, anon, base+bad); code != http.StatusNotFound && code != http.StatusForbidden {
			t.Errorf("%s: %d, want 404 or 403", bad, code)
		}
	}

	// REST routes under the plugin namespace; core routes are untouched.
	code, body, _ = get(t, anon, base+"/wp-json/shortcode-demo/v1/facts")
	var facts struct {
		Facts []string `json:"facts"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &facts) != nil || len(facts.Facts) != 5 {
		t.Fatalf("facts: %d %s", code, body)
	}
	if code, body, _ := get(t, anon, base+"/wp-json/shortcode-demo/v1/facts/2"); code != http.StatusOK || !strings.Contains(body, `"id":2`) {
		t.Errorf("fact 2: %d %s", code, body)
	}
	if code, body, _ := get(t, anon, base+"/wp-json/shortcode-demo/v1/facts/99"); code != http.StatusNotFound || !strings.Contains(body, "rest_not_found") {
		t.Errorf("fact 99: %d %s", code, body)
	}
	if code, body, _ := get(t, anon, base+"/wp-json/shortcode-demo/v1/nothing"); code != http.StatusNotFound || !strings.Contains(body, "rest_no_route") {
		t.Errorf("unknown route: %d %s", code, body)
	}
	if code, _, _ := get(t, anon, base+"/wp-json/wp/v2/posts"); code != http.StatusOK {
		t.Errorf("core route broken: %d", code)
	}
	if _, body, _ := get(t, anon, base+"/wp-json/"); !strings.Contains(body, `"shortcode-demo/v1"`) || !strings.Contains(body, `"/shortcode-demo/v1/facts"`) {
		t.Errorf("plugin routes missing from the index")
	}
	postJSON := func(cl *http.Client, nonce string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, base+"/wp-json/shortcode-demo/v1/facts", strings.NewReader(`{"fact":"Gophers read JSON."}`))
		req.Header.Set("Content-Type", "application/json")
		if nonce != "" {
			req.Header.Set("X-WP-Nonce", nonce)
		}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := postJSON(anon, ""); code != http.StatusForbidden || !strings.Contains(body, "rest_forbidden") {
		t.Errorf("anonymous POST: %d %s", code, body)
	}
	if code, body := postJSON(c, ""); code != http.StatusForbidden {
		t.Errorf("cookie without nonce must be anonymous: %d %s", code, body)
	}
	if code, body := postJSON(c, restNonce(t, srv, c, base)); code != http.StatusCreated || !strings.Contains(body, `"id":6`) {
		t.Errorf("admin POST: %d %s", code, body)
	}
	if _, body, _ := get(t, anon, base+"/wp-json/shortcode-demo/v1/facts"); !strings.Contains(body, "Gophers read JSON.") {
		t.Errorf("added fact missing (transient not cleared?): %s", body)
	}

	// Settings › Gopher Facts: menu entry, form, save, and the shortcode reflects the values.
	code, body, _ = get(t, c, base+"/wp-admin/options-general?page=shortcode-demo")
	if code != http.StatusOK {
		t.Fatalf("settings page %d", code)
	}
	for _, want := range []string{`href="/wp-admin/options-general?page=shortcode-demo"`, ">Gopher Facts<", `name="plugin_shortcode_demo_heading"`, `value="Did you know?"`,
		`name="plugin_shortcode_demo_count" type="number"`, `name="plugin_shortcode_demo_greet" type="checkbox" id="plugin_shortcode_demo_greet" value="1" checked="checked"`,
		`<option value="cards" selected="selected">Cards</option>`, `<textarea name="plugin_shortcode_demo_footer"`, `value="plugin:shortcode-demo"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page misses %q", want)
		}
	}
	if code, _, _ := post(t, c, base+"/wp-admin/options", url.Values{"option_page": {"plugin:shortcode-demo"}, "_wpnonce": {"bad"}, "plugin_shortcode_demo_heading": {"x"}}); code != http.StatusForbidden {
		t.Errorf("bad nonce accepted: %d", code)
	}
	code, _, loc := post(t, c, base+"/wp-admin/options", url.Values{"option_page": {"plugin:shortcode-demo"}, "_wpnonce": {formNonce(t, body)},
		"plugin_shortcode_demo_heading": {"Facts!"}, "plugin_shortcode_demo_count": {"1"}, "plugin_shortcode_demo_style": {"plain"},
		"plugin_shortcode_demo_footer": {"bye"}, "plugin_shortcode_demo_level": {"ignored"}})
	if code != http.StatusFound || !strings.Contains(loc, "settings-updated=true") {
		t.Fatalf("save: %d %s", code, loc)
	}
	if v := srv.settings.Get(pluginOptionKey("shortcode-demo", "plugin_shortcode_demo_heading")); v != "Facts!" {
		t.Errorf("heading = %v", v)
	}
	if v := srv.settings.Get(pluginOptionKey("shortcode-demo", "plugin_shortcode_demo_greet")); v != false {
		t.Errorf("unchecked checkbox = %v", v)
	}
	if v := srv.settings.Get(pluginOptionKey("shortcode-demo", "plugin_shortcode_demo_count")); v != 1.0 {
		t.Errorf("count = %v (%T)", v, v)
	}
	_, body, _ = get(t, c, base+"/wp-admin/options-general?page=shortcode-demo&settings-updated=true")
	if !strings.Contains(body, "Settings saved.") || !strings.Contains(body, `<option value="plain" selected="selected">`) || !strings.Contains(body, `value="Facts!"`) {
		t.Errorf("saved values not shown: %s", regexp.MustCompile(`(?s)<form.*?</form>`).FindString(body))
	}
	_, body, _ = get(t, c, link)
	factsHTML := factsDiv.FindString(body)
	if !strings.Contains(factsHTML, "<h3>Facts!</h3>") || strings.Count(factsHTML, "<li>") != 2 || strings.Contains(factsHTML, "Hello, admin!") ||
		!strings.Contains(factsHTML, `gopher-facts--plain`) || !strings.Contains(factsHTML, `<p class="gopher-facts__footer">bye</p>`) {
		t.Errorf("shortcode ignores saved settings: %s", factsHTML)
	}
	// Without the count attribute the saved count applies.
	p2 := &store.Post{Type: "post", Status: "publish", Title: "Facts 2", Content: "[gopher-facts]", AuthorID: 1}
	if err := srv.store.InsertPost(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := get(t, c, srv.links(ctx).Permalink(p2)); strings.Count(factsDiv.FindString(body), "<li>") != 1 {
		t.Errorf("saved count not applied: %s", factsDiv.FindString(body))
	}
	if code, _, _ := get(t, c, base+"/wp-admin/options-general?page=unknown-plugin"); code != http.StatusOK {
		t.Errorf("unknown page should fall back to General: %d", code)
	}

	// The scheduled event is a jobs row with the plugin interval; running it fires the action.
	var rec string
	var payload []byte
	if err := srv.store.Pool.QueryRow(ctx, `SELECT recurrence, payload FROM jobs WHERE name = 'cron_shortcode_demo_tick' AND status = 'queued'`).Scan(&rec, &payload); err != nil {
		t.Fatalf("job row: %v", err)
	}
	if rec != "every:3600" || !strings.Contains(string(payload), `"plugin": "shortcode-demo"`) && !strings.Contains(string(payload), `"plugin":"shortcode-demo"`) {
		t.Errorf("job %s %s", rec, payload)
	}
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE jobs SET run_at = now() - interval '1 minute' WHERE name = 'cron_shortcode_demo_tick'`)
	srv.runDueJobs(ctx, "test", srv.jobHandlers())
	if v, ok, _ := (&pluginHost{s: srv}).StorageGet(ctx, "shortcode-demo", "ticks"); !ok || v != "1" {
		t.Errorf("cron action did not run: %q %v", v, ok)
	}
	var next time.Time
	var status string
	_ = srv.store.Pool.QueryRow(ctx, `SELECT run_at, status FROM jobs WHERE name = 'cron_shortcode_demo_tick'`).Scan(&next, &status)
	if status != "queued" || next.Before(time.Now().Add(50*time.Minute)) {
		t.Errorf("job not rescheduled an hour ahead: %s %s", status, next)
	}
	if n := countJobs(t, srv, "cron_shortcode_demo_tick"); n != 1 {
		t.Errorf("init re-schedule duplicated the job: %d rows", n)
	}

	// While the plugin is not running its event is skipped, not failed; deleting the plugin
	// removes its events, options and transients.
	_ = srv.plugins.rt.Unload(ctx, "shortcode-demo")
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE jobs SET run_at = now() - interval '1 minute' WHERE name = 'cron_shortcode_demo_tick'`)
	srv.runDueJobs(ctx, "test", srv.jobHandlers())
	var lastErr string
	_ = srv.store.Pool.QueryRow(ctx, `SELECT status, last_error FROM jobs WHERE name = 'cron_shortcode_demo_tick'`).Scan(&status, &lastErr)
	if status != "queued" || lastErr != "" {
		t.Errorf("event of an unloaded plugin: %s %q", status, lastErr)
	}
	h := &pluginHost{s: srv}
	_ = h.UpdateOption(ctx, "shortcode-demo", "shortcode_demo_secret", "k")
	_ = h.TransientSet(ctx, "shortcode-demo", "t", json.RawMessage(`1`), 0)
	srv.purgePluginData(ctx, "shortcode-demo")
	if n := countJobs(t, srv, "cron_shortcode_demo_tick"); n != 0 {
		t.Errorf("deleted plugin's events left: %d", n)
	}
	if v, _, _ := h.GetOption(ctx, "shortcode-demo", "shortcode_demo_secret"); v != nil {
		t.Errorf("deleted plugin's option left: %v", v)
	}
	if _, ok, _ := h.TransientGet(ctx, "shortcode-demo", "t"); ok {
		t.Error("deleted plugin's transient left")
	}
}

func countJobs(t *testing.T, srv *Server, name string) int {
	t.Helper()
	var n int
	_ = srv.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE name = $1`, name).Scan(&n)
	return n
}

func TestPluginPostsAPIAndTransientsOnDatabase(t *testing.T) {
	ts, _, srv := testServerBuilderOff(t)
	_ = ts
	ctx := context.Background()
	// The core host, called the way the runtime calls it after validation.
	h := &pluginHost{s: srv}
	_ = srv.settings.Set(ctx, map[string]any{"blogname": "t"})
	title, content := "From a plugin", "<p>safe</p><script>alert(1)</script><a href=\"javascript:x\">x</a>"
	status := "publish"
	created, err := h.InsertPost(withPluginElevated(ctx), "demo", plugins.PostInput{Title: &title, Content: &content, Status: &status, Meta: map[string]any{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || strings.Contains(created.Content, "<script") || strings.Contains(created.Content, "javascript:") || created.Slug != "from-a-plugin" || created.Link == "" {
		t.Fatalf("created %+v", created)
	}
	if created.Meta["k"] != "v" || len(created.Terms["category"]) != 1 {
		t.Errorf("meta/terms %+v %+v", created.Meta, created.Terms)
	}
	ex := "short"
	updated, err := h.UpdatePost(withPluginElevated(ctx), "demo", created.ID, plugins.PostInput{Excerpt: &ex, Meta: map[string]any{"k": nil, "n": 2}})
	if err != nil || updated.Excerpt != "short" || updated.Meta["k"] != nil || updated.Meta["n"] != 2.0 {
		t.Fatalf("updated %+v %v", updated, err)
	}
	list, err := h.QueryPosts(ctx, "demo", plugins.PostQuery{Search: "plugin", PerPage: 10, Page: 1})
	if err != nil || list.Total != 1 || list.Posts[0].ID != created.ID || list.Pages != 1 {
		t.Fatalf("query %+v %v", list, err)
	}
	if list, _ := h.QueryPosts(ctx, "demo", plugins.PostQuery{Type: "attachment", PerPage: 10}); list.Total != 0 {
		t.Error("other post types must be hidden")
	}
	page := "page"
	if _, err := h.InsertPost(ctx, "demo", plugins.PostInput{Title: &title, Type: &page}); err != nil {
		t.Errorf("page insert: %v", err)
	}
	if got, _ := h.GetPost(ctx, "demo", 99999); got != nil {
		t.Error("missing post returned")
	}

	// Transients on the transients table, with expiry.
	if err := h.TransientSet(ctx, "demo", "k", json.RawMessage(`{"a":1}`), time.Hour); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := h.TransientGet(ctx, "demo", "k"); !ok || string(v) != `{"a": 1}` && string(v) != `{"a":1}` {
		t.Errorf("transient %s %v", v, ok)
	}
	if _, ok, _ := h.TransientGet(ctx, "other", "k"); ok {
		t.Error("transients leak between plugins")
	}
	_ = h.TransientSet(ctx, "demo", "k", json.RawMessage(`1`), time.Second)
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE transients SET expires_at = now() - interval '1 second' WHERE key = 'plugin:demo:k'`)
	if _, ok, _ := h.TransientGet(ctx, "demo", "k"); ok {
		t.Error("expired transient returned")
	}
	_ = h.TransientDelete(ctx, "demo", "k")

	// Options: the plugin's own settings options are readable, other plugin_* options are not.
	_ = srv.settings.Set(ctx, map[string]any{pluginOptionKey("demo", "plugin_demo_x"): "1", pluginOptionKey("other", "plugin_other_x"): "2",
		"plugin_other_x": "2", "plugin_allow_unsigned": true, "gocdn_settings": "secret", "wp_mail_smtp": "secret"})
	if v, ok, _ := h.GetOption(ctx, "demo", "plugin_demo_x"); !ok || v != "1" {
		t.Error("own settings option hidden")
	}
	if _, ok, _ := h.GetOption(ctx, "demo", "plugin_other_x"); ok {
		t.Error("another plugin's option visible")
	}
	if _, ok, _ := h.GetOption(ctx, "demo", "plugin_allow_unsigned"); ok {
		t.Error("core plugin option visible")
	}
	if err := h.UpdateOption(ctx, "demo", "plugin_demo_x", "3"); err != nil {
		t.Error(err)
	}
	if err := h.UpdateOption(ctx, "demo", "plugin_other_x", "3"); err == nil {
		t.Error("wrote another plugin's option")
	}
	if v := srv.settings.Get(pluginOptionKey("demo", "plugin_demo_x")); v != "3" {
		t.Errorf("own option not stored in the plugin's namespace: %v", v)
	}
	// Reading is default-deny: built-in plugin options are hidden, public core options are not.
	for _, name := range []string{"gocdn_settings", "wp_mail_smtp", "admin_email", "default_role", "mailserver_pass"} {
		if _, ok, _ := h.GetOption(ctx, "demo", name); ok {
			t.Errorf("%s is readable by a plugin", name)
		}
	}
	if v, ok, _ := h.GetOption(ctx, "demo", "blogname"); !ok || v == nil {
		t.Error("public core option blogname hidden")
	}
	// A plugin slugged after a built-in option's first word reaches only its own namespace.
	if _, ok, _ := h.GetOption(ctx, "gocdn", "gocdn_settings"); ok {
		t.Error("a plugin slugged gocdn reads gocdn_settings")
	}
	if err := h.UpdateOption(ctx, "wp-mail", "wp_mail_smtp", "pwned"); err != nil {
		t.Fatal(err)
	}
	if v := srv.settings.Get("wp_mail_smtp"); v != "secret" {
		t.Errorf("a plugin slugged wp-mail overwrote wp_mail_smtp: %v", v)
	}
	if v, ok, _ := h.GetOption(ctx, "wp-mail", "wp_mail_smtp"); !ok || v != "pwned" {
		t.Errorf("the plugin does not read back its own option: %v", v)
	}
}

// Plugins schedule from OnInit, which runs on every instantiation: an identical schedule must keep its
// next run, and replacing a schedule while its row is running must not leave two rows behind.
func TestPluginScheduleEventIdempotent(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	ctx := context.Background()
	h := &pluginHost{s: srv}
	ev := plugins.ScheduledEvent{Hook: "cron_idem_tick", Interval: 3600, Args: map[string]any{"a": 1}}
	if err := h.ScheduleEvent(ctx, "idem", ev); err != nil {
		t.Fatal(err)
	}
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE jobs SET run_at = now() + interval '5 minutes' WHERE name = 'cron_idem_tick'`)
	runAt := func() time.Time {
		var at time.Time
		_ = srv.store.Pool.QueryRow(ctx, `SELECT run_at FROM jobs WHERE name = 'cron_idem_tick'`).Scan(&at)
		return at
	}
	before := runAt()
	if err := h.ScheduleEvent(ctx, "idem", ev); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, srv, "cron_idem_tick"); n != 1 || !runAt().Equal(before) {
		t.Errorf("re-scheduling the same event: %d rows, run_at %s -> %s", n, before, runAt())
	}
	// A running row replaced by a new interval: one row, and the old run cannot be re-queued.
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE jobs SET status = 'running', locked_by = 'w' WHERE name = 'cron_idem_tick'`)
	ev.Interval = 7200
	if err := h.ScheduleEvent(ctx, "idem", ev); err != nil {
		t.Fatal(err)
	}
	var rec, status string
	_ = srv.store.Pool.QueryRow(ctx, `SELECT recurrence, status FROM jobs WHERE name = 'cron_idem_tick'`).Scan(&rec, &status)
	if n := countJobs(t, srv, "cron_idem_tick"); n != 1 || rec != "every:7200" || status != "queued" {
		t.Errorf("replacing a running schedule: %d rows, %s %s", n, rec, status)
	}
	// Changed args replace the schedule too.
	ev.Args = map[string]any{"a": 2}
	if err := h.ScheduleEvent(ctx, "idem", ev); err != nil {
		t.Fatal(err)
	}
	var args string
	_ = srv.store.Pool.QueryRow(ctx, `SELECT payload->'args'->>'a' FROM jobs WHERE name = 'cron_idem_tick'`).Scan(&args)
	if n := countJobs(t, srv, "cron_idem_tick"); n != 1 || args != "2" {
		t.Errorf("changed args: %d rows, a=%s", n, args)
	}
	// Unschedule removes a running row as well.
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE jobs SET status = 'running' WHERE name = 'cron_idem_tick'`)
	if err := h.UnscheduleEvent(ctx, "idem", "cron_idem_tick"); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, srv, "cron_idem_tick"); n != 0 {
		t.Errorf("unschedule left %d rows", n)
	}
}

// demoPackageVersion repacks the demo plugin with another manifest version.
func demoPackageVersion(t *testing.T, version string) []byte {
	t.Helper()
	data := demoPackage(t)
	pkg, err := plugins.LoadPackage(bytes.NewReader(data), int64(len(data)), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	manifest := bytes.Replace(pkg.ManifestJSON, []byte(`"version": "1.0.0"`), []byte(`"version": "`+version+`"`), 1)
	extras := map[string][]byte{}
	for k, v := range pkg.Assets {
		extras["assets/"+strings.TrimPrefix(k, "assets/")] = v
	}
	var buf bytes.Buffer
	if err := plugins.WritePackage(&buf, manifest, pkg.Wasm, extras, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Installing a new version of an active plugin (upload, marketplace, REST) loads the new code at
// once, so the running version always matches the installed one.
func TestPluginUploadOfActivePluginReloadsIt(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	installDemo(t, srv)
	ctx := context.Background()
	m, updated, err := srv.installAndReload(ctx, demoPackageVersion(t, "1.0.1"), pluginSourceUpload, "", false)
	if err != nil || !updated {
		t.Fatalf("install new version: %v updated=%v", err, updated)
	}
	p, ok := srv.plugins.rt.Plugin(m.Slug)
	if !ok || p.Version() != "1.0.1" {
		t.Fatalf("running version not reloaded: ok=%v", ok)
	}
	if disabled, _ := p.Disabled(); disabled || !srv.plugins.hooks.HasFilter("the_content") && len(p.Shortcodes()) == 0 {
		t.Fatal("reloaded plugin has no registrations")
	}
	var status, version string
	_ = srv.store.Pool.QueryRow(ctx, `SELECT status, version FROM plugins WHERE slug = $1`, m.Slug).Scan(&status, &version)
	if status != "active" || version != "1.0.1" {
		t.Fatalf("row after reload: %s %s", status, version)
	}
	if _, err := os.Stat(filepath.Join(srv.plugins.dir, m.Slug+".zip.prev")); err != nil {
		t.Fatalf("previous package not kept: %v", err)
	}
	if got := pluginInstalledRedirect(m, true); !strings.Contains(got, "updated=") {
		t.Fatalf("redirect %q", got)
	}
}

// Posts a plugin creates go through the same save chain as every other save path: with Pursae
// Multi Language active they get the default language.
func TestPluginInsertPostRunsAfterPostSave(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	golylangToggle(t, c, base, "activate")
	pllAddLanguage(t, c, base, "English", "en_US", "en", "us")
	ctx := context.Background()
	h := &pluginHost{s: srv}
	title, status := "From a plugin", "publish"
	pd, err := h.InsertPost(withPluginElevated(ctx), "shortcode-demo", plugins.PostInput{Title: &title, Status: &status})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.pllObjLang(ctx, pllPost, pd.ID); got != "en" {
		t.Fatalf("plugin-inserted post language %q, want en", got)
	}
}

// One-off events are keyed by (hook, args): "one reminder per order" must not overwrite each other, and a
// recurring schedule of the same hook leaves them alone.
func TestPluginSingleEventsKeyedByArgs(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	ctx := context.Background()
	h := &pluginHost{s: srv}
	at := time.Now().Add(time.Hour).Unix()
	for _, id := range []int{1, 2} {
		if err := h.ScheduleEvent(ctx, "single", plugins.ScheduledEvent{Hook: "cron_single_x", At: at, Args: map[string]any{"order": id}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countJobs(t, srv, "cron_single_x"); n != 2 {
		t.Fatalf("two one-off events with different args: %d rows", n)
	}
	// Same args again replaces, not duplicates.
	if err := h.ScheduleEvent(ctx, "single", plugins.ScheduledEvent{Hook: "cron_single_x", At: at + 60, Args: map[string]any{"order": 1}}); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, srv, "cron_single_x"); n != 2 {
		t.Fatalf("same args replaced wrongly: %d rows", n)
	}
	if err := h.ScheduleEvent(ctx, "single", plugins.ScheduledEvent{Hook: "cron_single_x", Interval: 3600}); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, srv, "cron_single_x"); n != 3 {
		t.Fatalf("recurring event must not remove one-offs: %d rows", n)
	}
	if err := h.UnscheduleEventArgs(ctx, "single", "cron_single_x", map[string]any{"order": 2}); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, srv, "cron_single_x"); n != 2 {
		t.Fatalf("unschedule by args: %d rows", n)
	}
	if err := h.UnscheduleEvent(ctx, "single", "cron_single_x"); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, srv, "cron_single_x"); n != 0 {
		t.Fatalf("unschedule all: %d rows", n)
	}
}

func TestPluginScriptTag(t *testing.T) {
	a := plugins.PluginAsset{Plugin: "p", Version: "1", Asset: plugins.Asset{Handle: "my-lib_x", Src: "a.js",
		Data: map[string]any{"t": "</script><b>"}}}
	got := pluginScriptTag(a, "p-my-lib_x", "front", false)
	if !strings.Contains(got, "window.myLibXData = ") || strings.Contains(got, "</script><b>") {
		t.Errorf("data block: %s", got)
	}
	if !strings.Contains(got, `-js" defer>`) {
		t.Errorf("front head script should default to defer: %s", got)
	}
	if got := pluginScriptTag(a, "p-x", "front", true); strings.Contains(got, "defer") {
		t.Errorf("footer script deferred: %s", got)
	}
	if got := pluginScriptTag(a, "p-x", "admin", false); strings.Contains(got, "defer") {
		t.Errorf("admin script deferred by default: %s", got)
	}
	a.Asset.Strategy, a.Asset.Module = "async", true
	if got := pluginScriptTag(a, "p-x", "front", false); !strings.Contains(got, `type="module" async>`) {
		t.Errorf("module async: %s", got)
	}
}

// Another instance changes the plugins table; this process follows after two ticks, and a plugin's
// lifecycle actions reach only that plugin.
func TestPluginSyncFollowsDatabaseAndLifecycleIsOwned(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	installDemo(t, srv)
	ctx := context.Background()
	pending := map[string]string{}

	// Deactivated elsewhere: unloaded on the second sighting only.
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'inactive' WHERE slug = 'shortcode-demo'`)
	srv.syncPluginsOnce(ctx, pending)
	if _, ok := srv.plugins.rt.Plugin("shortcode-demo"); !ok {
		t.Fatal("unloaded on the first sighting")
	}
	srv.syncPluginsOnce(ctx, pending)
	if _, ok := srv.plugins.rt.Plugin("shortcode-demo"); ok {
		t.Fatal("still loaded after the second sighting")
	}
	// Activated elsewhere: loaded.
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'active' WHERE slug = 'shortcode-demo'`)
	srv.syncPluginsOnce(ctx, pending)
	srv.syncPluginsOnce(ctx, pending)
	if _, ok := srv.plugins.rt.Plugin("shortcode-demo"); !ok {
		t.Fatal("not loaded after activation elsewhere")
	}
	// Deleted elsewhere: unloaded.
	_, _ = srv.store.Pool.Exec(ctx, `DELETE FROM plugins WHERE slug = 'shortcode-demo'`)
	srv.syncPluginsOnce(ctx, pending)
	srv.syncPluginsOnce(ctx, pending)
	if _, ok := srv.plugins.rt.Plugin("shortcode-demo"); ok {
		t.Fatal("still loaded after delete elsewhere")
	}
}

// plugin_allow_unsigned alone (a database or CLI write) does not accept unsigned packages: the server
// must also run with PURSAE_ALLOW_UNSIGNED_PLUGINS=1 (PLG27-001).
func TestUnsignedPluginsNeedEnvironmentOptIn(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	ctx := context.Background()
	if err := srv.settings.Set(ctx, map[string]any{"plugin_allow_unsigned": true}); err != nil {
		t.Fatal(err)
	}
	srv.cfg.AllowUnsignedPlugins = false
	if _, err := srv.installPackage(ctx, demoPackage(t)); err == nil {
		t.Fatal("an unsigned package installed with only the plugin_allow_unsigned option on")
	}
	srv.cfg.AllowUnsignedPlugins = true
	if _, err := srv.installPackage(ctx, demoPackage(t)); err != nil {
		t.Fatalf("with both switches on: %v", err)
	}
}

// A user who may install but not activate plugins cannot replace an active plugin's code.
func TestPluginUpdateOfActivePluginNeedsActivate(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	installDemo(t, srv)
	u := &store.User{ID: 99, Login: "installer", Caps: caps.Grants{"install_plugins": true}}
	ctx := withExtActor(context.Background(), u)
	if _, _, err := srv.installAndReload(ctx, demoPackageVersion(t, "1.0.1"), pluginSourceUpload, "", false); !errors.Is(err, errPluginReloadDenied) {
		t.Fatalf("active plugin replaced without activate_plugins: %v", err)
	}
}

// The Hub update path applies the same rule to a web/REST actor; a caller with no actor (CLI,
// background updates) is not refused by it.
func TestHubUpdateOfActivePluginNeedsActivate(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	installDemo(t, srv)
	u := &store.User{ID: 99, Login: "updater", Caps: caps.Grants{"update_plugins": true}}
	if _, err := srv.hubUpdatePlugin(withExtActor(context.Background(), u), "shortcode-demo"); !errors.Is(err, errPluginReloadDenied) {
		t.Fatalf("active plugin updated from the Hub without activate_plugins: %v", err)
	}
	if _, err := srv.hubUpdatePlugin(context.Background(), "shortcode-demo"); errors.Is(err, errPluginReloadDenied) {
		t.Fatal("an update with no acting user was refused by the activate rule")
	}
}
