package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"wpgo/internal/wp"
)

func TestShortcodeRegistrationAndExpansion(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	if got := p.Shortcodes(); len(got) != 1 || got[0] != "probe" {
		dumpLogs(t, p)
		t.Fatalf("shortcodes = %v, want [probe] (undeclared must be refused)", got)
	}
	if !logged(p, `shortcode "undeclared"`) {
		t.Error("refused shortcode not logged")
	}
	tags := rt.ShortcodeTags()
	content := wp.DoShortcode(`<p>[probe a="1" b=two]inner[/probe]</p> [probe /] [other]`, tags, func(tag string, attrs map[string]string, inner string) string {
		out, ok := rt.DoShortcode(context.Background(), tag, attrs, inner)
		if !ok {
			t.Fatalf("no owner for %s", tag)
		}
		return out
	})
	if content != `<probe a=1,b=two|inner> <probe |> [other]` {
		dumpLogs(t, p)
		t.Fatalf("expanded = %q", content)
	}
	if _, ok := rt.DoShortcode(context.Background(), "nobody", nil, ""); ok {
		t.Error("unknown tag reported as owned")
	}
}

func TestShortcodeNeedsContentFilterPermission(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost(), PermOptionsRead)
	if len(p.Shortcodes()) != 0 || len(rt.ShortcodeTags()) != 0 {
		t.Fatal("shortcode registered without content:filter")
	}
}

func TestRestRoutes(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	host.User = &UserInfo{ID: 1, Login: "admin", Roles: []string{"administrator"}, Caps: []string{"edit_posts"}}
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", host)
	routes := p.RestRoutes()
	if len(routes) != 1 || routes[0].Namespace != "probe/v1" || routes[0].Route != `/echo/(?P<id>\d+)` {
		dumpLogs(t, p)
		t.Fatalf("routes = %+v (wp/v2 must be refused)", routes)
	}
	if !logged(p, "namespace must be") {
		t.Error("refused namespace not logged")
	}
	if ns := rt.RestNamespaces(); len(ns) != 1 || ns[0] != "probe/v1" {
		t.Errorf("namespaces %v", ns)
	}
	owner, r, params, ok := rt.FindRestRoute("/probe/v1/echo/42")
	if !ok || owner != p || params["id"] != "42" || !r.Allows("POST") || r.Allows("DELETE") {
		t.Fatalf("FindRestRoute: ok=%v params=%v route=%+v", ok, params, r)
	}
	if _, _, _, ok := rt.FindRestRoute("/probe/v1/echo/x"); ok {
		t.Error("non-numeric id matched")
	}
	req := RestRequest{Method: "POST", Route: "/probe/v1/echo/42", Params: params, Query: map[string]string{"q": "1"},
		Body: map[string]any{"hello": "world"}, User: &UserInfo{Login: "admin"}}
	resp, err := p.ServeRest(context.Background(), *r, req)
	if err != nil {
		dumpLogs(t, p)
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || resp.Headers["X-Probe"] != "1" || body["id"] != "42" || body["method"] != "POST" || body["user"] != "admin" {
		t.Fatalf("response %d %s", resp.Status, resp.Body)
	}
	if b, _ := body["body"].(map[string]any); b["hello"] != "world" {
		t.Fatalf("body not passed: %v", body["body"])
	}
}

func TestPostsAPI(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", host)
	got := rt.Hooks().ApplyFilters(context.Background(), "probe_posts", "Hello posts")
	want := "id=1 title=Hello posts excerpt=ex status=publish meta=1 found=1 total=1"
	if got != want {
		dumpLogs(t, p)
		t.Fatalf("got %q, want %q", got, want)
	}
	post, _ := host.GetPost(context.Background(), "probe", 1)
	if post == nil || post.Content != "<p>body</p><script>x</script>" { // MemoryHost does not sanitise; the core does
		t.Fatalf("stored post %+v", post)
	}

	// Without posts:write the write calls fail with permission_denied and nothing is stored.
	rt2 := newTestRuntime(t, Config{})
	host2 := NewMemoryHost()
	loadPlugin(t, rt2, "internal/plugins/testdata/probe", host2, PermPostsRead)
	if got := rt2.Hooks().ApplyFilters(context.Background(), "probe_posts", "x"); !strings.Contains(got.(string), "permission_denied") {
		t.Fatalf("got %q", got)
	}
	if list, _ := host2.QueryPosts(context.Background(), "probe", PostQuery{Status: "any"}); list.Total != 0 {
		t.Fatal("post stored without posts:write")
	}
}

func TestCurrentUser(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	loadPlugin(t, rt, "internal/plugins/testdata/probe", host)
	if got := rt.Hooks().ApplyFilters(context.Background(), "probe_user", ""); got != "anonymous" {
		t.Fatalf("got %q", got)
	}
	host.User = &UserInfo{ID: 2, Login: "editor", Roles: []string{"editor"}, Caps: []string{"edit_posts"}}
	if got := rt.Hooks().ApplyFilters(context.Background(), "probe_user", ""); got != "editor:[editor]:true" {
		t.Fatalf("got %q", got)
	}
	rt2 := newTestRuntime(t, Config{})
	loadPlugin(t, rt2, "internal/plugins/testdata/probe", host, PermStorage)
	if got := rt2.Hooks().ApplyFilters(context.Background(), "probe_user", ""); !strings.Contains(got.(string), "permission_denied") {
		t.Fatalf("got %q without users:read", got)
	}
}

func TestScheduledEvents(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", host)
	hooks := rt.Hooks()
	if got := hooks.ApplyFilters(context.Background(), "probe_cron", "schedule"); got != "ok" {
		dumpLogs(t, p)
		t.Fatalf("schedule: %q", got)
	}
	ev := host.Events("probe")["cron_probe"]
	if ev.Hook != "cron_probe" || ev.Interval != 3600 || ev.Args["n"] != 1.0 {
		t.Fatalf("event %+v", ev)
	}
	if got := hooks.ApplyFilters(context.Background(), "probe_cron", "single"); got != "ok" {
		t.Fatalf("single: %q", got)
	}
	if ev := host.Events("probe")["cron_probe"]; ev.Interval != 0 || ev.At < time.Now().Unix()+3000 {
		t.Fatalf("single event %+v", ev)
	}
	if got := hooks.ApplyFilters(context.Background(), "probe_cron", "undeclared"); !strings.Contains(got.(string), "not declared") {
		t.Fatalf("undeclared hook: %q", got)
	}
	// The core fires the hook as an action with the payload as its argument.
	hooks.DoAction(context.Background(), "cron_probe", map[string]any{"plugin": "probe", "args": map[string]any{"n": 1}})
	if v, _, _ := host.StorageGet(context.Background(), "probe", "cron_args"); !strings.Contains(v, `"n":1`) {
		t.Fatalf("cron action did not run: %q", v)
	}
	if got := hooks.ApplyFilters(context.Background(), "probe_cron", "unschedule"); got != "ok" || len(host.Events("probe")) != 0 {
		t.Fatalf("unschedule: %q %v", got, host.Events("probe"))
	}
}

func TestSettingsPage(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	host := NewMemoryHost()
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", host)
	pages := rt.SettingsPages()
	if len(pages) != 1 || pages[0].Plugin != "probe" || pages[0].Page.MenuTitle != "Probe" || len(pages[0].Page.Fields) != 5 {
		dumpLogs(t, p)
		t.Fatalf("pages %+v", pages)
	}
	if f := pages[0].Page.Fields[0]; f.OptionName("probe") != "plugin_probe_greeting" || f.Default != "hi" {
		t.Fatalf("field %+v", f)
	}
	if got := rt.Hooks().ApplyFilters(context.Background(), "probe_settings", ""); got != "plugin_probe_greeting=default" {
		t.Fatalf("before save: %q", got)
	}
	host.SetOption("plugin_probe_greeting", "hello")
	if got := rt.Hooks().ApplyFilters(context.Background(), "probe_settings", ""); got != "plugin_probe_greeting=hello" {
		t.Fatalf("after save: %q", got)
	}
	rt2 := newTestRuntime(t, Config{})
	loadPlugin(t, rt2, "internal/plugins/testdata/probe", host, PermOptionsRead)
	if len(rt2.SettingsPages()) != 0 {
		t.Fatal("settings page registered without admin:settings")
	}
}

func TestTransients(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	now := time.Now()
	host := NewMemoryHost()
	host.Now = func() time.Time { return now }
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", host)
	if got := rt.Hooks().ApplyFilters(context.Background(), "probe_transient", "val"); got != "transient:val" {
		dumpLogs(t, p)
		t.Fatalf("got %q", got)
	}
	_ = host.TransientSet(context.Background(), "probe", "x", json.RawMessage(`1`), time.Minute)
	now = now.Add(2 * time.Minute)
	if _, found, _ := host.TransientGet(context.Background(), "probe", "x"); found {
		t.Fatal("expired transient still found")
	}
}

func TestEnqueuedAssets(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	assets := p.Assets()
	if len(assets) != 2 {
		dumpLogs(t, p)
		t.Fatalf("assets = %+v (missing.js must be refused)", assets)
	}
	if !logged(p, "missing.js") {
		t.Error("refused asset not logged")
	}
	front := rt.EnqueuedAssets("front", "style")
	if len(front) != 1 || front[0].Asset.Src != "probe.css" || front[0].Asset.Media != "all" || front[0].Version != "0.0.1" {
		t.Fatalf("front styles %+v", front)
	}
	if adm := rt.EnqueuedAssets("admin", "style"); len(adm) != 1 {
		t.Fatalf("admin styles %+v", adm)
	}
	if adm := rt.EnqueuedAssets("admin", "script"); len(adm) != 0 {
		t.Fatalf("admin scripts %+v", adm)
	}
	scripts := rt.EnqueuedAssets("front", "script")
	if len(scripts) != 1 || scripts[0].Asset.Src != "js/probe.js" || !scripts[0].Asset.InFooter {
		t.Fatalf("front scripts %+v", scripts)
	}
	if data, ok := p.PackageAsset("js/probe.js"); !ok || !strings.Contains(string(data), "probe") {
		t.Fatal("package asset not readable")
	}
	if _, ok := p.PackageAsset("../plugin.json"); ok {
		t.Fatal("path escape")
	}
}

func TestRegistrationsSurviveReinstantiation(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	p := loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	hooks := rt.Hooks()
	hooks.ApplyFilters(context.Background(), "probe_exit", "v") // kills the instance
	hooks.ApplyFilters(context.Background(), "probe_echo", "v") // re-instantiates, re-registers
	if len(p.Shortcodes()) != 1 || len(p.RestRoutes()) != 1 || len(p.Assets()) != 2 || p.SettingsPage() == nil {
		t.Fatalf("registrations duplicated or lost: %v %v %v", p.Shortcodes(), p.RestRoutes(), p.Assets())
	}
}

func TestDisabledPluginRegistrationsAreHidden(t *testing.T) {
	rt := newTestRuntime(t, Config{MaxFailures: 1})
	loadPlugin(t, rt, "internal/plugins/testdata/probe", NewMemoryHost())
	rt.Hooks().ApplyFilters(context.Background(), "probe_panic", "x")
	if len(rt.ShortcodeTags()) != 0 || len(rt.SettingsPages()) != 0 || len(rt.EnqueuedAssets("front", "style")) != 0 {
		t.Fatal("disabled plugin still exposes registrations")
	}
	if _, _, _, ok := rt.FindRestRoute("/probe/v1/echo/1"); ok {
		t.Fatal("disabled plugin still serves routes")
	}
}

func TestPackageAssets(t *testing.T) {
	manifest := []byte(`{"slug":"assets-demo","name":"A","version":"1.0.0","permissions":["content:filter"],"hooks":[],"shortcodes":["demo"]}`)
	wasm := []byte("\x00asm\x01\x00\x00\x00")
	pub, priv, _ := GenerateKey()
	extras := map[string][]byte{"assets/style.css": []byte("a{}"), "assets/img/logo.svg": []byte("<svg/>")}
	var buf bytes.Buffer
	if err := WritePackage(&buf, manifest, wasm, extras, priv); err != nil {
		t.Fatal(err)
	}
	pkg, err := LoadPackage(bytes.NewReader(buf.Bytes()), int64(buf.Len()), []ed25519.PublicKey{pub}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Assets) != 2 || string(pkg.Assets["img/logo.svg"]) != "<svg/>" || len(pkg.Manifest.Assets) != 2 {
		t.Fatalf("assets %v manifest %v", pkg.Assets, pkg.Manifest.Assets)
	}
	if !pkg.Verified() {
		t.Fatal("signature over the rewritten manifest did not verify")
	}

	// A tampered asset breaks the digest check.
	tampered := tamperZip(t, buf.Bytes(), "assets/style.css", []byte("b{}"))
	if _, err := LoadPackage(bytes.NewReader(tampered), int64(len(tampered)), []ed25519.PublicKey{pub}, true); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered asset accepted: %v", err)
	}
	// Disallowed extensions and paths are rejected when packing.
	for _, bad := range []string{"assets/run.php", "assets/../x.css", "assets/.hidden.css", "other/x.css"} {
		if err := WritePackage(&bytes.Buffer{}, manifest, wasm, map[string][]byte{bad: []byte("x")}, nil); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// An asset not listed in plugin.json is rejected when loading.
	unlisted := tamperZip(t, buf.Bytes(), "assets/extra.js", []byte("x"))
	if _, err := LoadPackage(bytes.NewReader(unlisted), int64(len(unlisted)), nil, false); err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Fatalf("unlisted asset accepted: %v", err)
	}
}

func TestManifestShortcodesAndAssets(t *testing.T) {
	base := `{"slug":"x-y","name":"X","version":"1.0.0","permissions":[%s],"hooks":[],"shortcodes":[%s]%s}`
	cases := []struct {
		perms, tags, extra string
		wantErr            string
	}{
		{`"content:filter"`, `"a"`, ``, ""},
		{``, `"a"`, ``, "content:filter"},
		{`"content:filter"`, `"A"`, ``, "invalid"},
		{`"content:filter"`, `"a","a"`, ``, "twice"},
		{`"content:filter"`, ``, `,"assets":{"x.css":"00"}`, "digest"},
		{`"content:filter"`, ``, `,"assets":{"x.exe":"` + strings.Repeat("a", 64) + `"}`, "extension"},
	}
	for _, c := range cases {
		_, err := ParseManifest([]byte(strings.Replace(strings.Replace(strings.Replace(base, "%s", c.perms, 1), "%s", c.tags, 1), "%s", c.extra, 1)))
		if c.wantErr == "" && err != nil {
			t.Errorf("%v: unexpected %v", c, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%v: got %v, want %q", c, err, c.wantErr)
		}
	}
}

// tamperZip rewrites a zip, replacing or adding the file name with data.
func tamperZip(t *testing.T, src []byte, name string, data []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	done := false
	for _, f := range zr.File {
		w, _ := zw.Create(f.Name)
		if f.Name == name {
			_, _ = w.Write(data)
			done = true
			continue
		}
		rc, _ := f.Open()
		_, _ = io.Copy(w, rc)
		rc.Close()
	}
	if !done {
		w, _ := zw.Create(name)
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	return out.Bytes()
}
