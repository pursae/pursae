package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"

	"wpgo/internal/plugins"
	"wpgo/internal/store"
)

func TestPluginFieldValues(t *testing.T) {
	cases := []struct {
		f       plugins.SettingsField
		raw     string
		present bool
		want    any
		ok      bool
	}{
		{plugins.SettingsField{Type: "checkbox"}, "", false, false, true},
		{plugins.SettingsField{Type: "checkbox"}, "1", true, true, true},
		{plugins.SettingsField{Type: "email"}, "a@example.com", true, "a@example.com", true},
		{plugins.SettingsField{Type: "email"}, "not mail", true, nil, false},
		{plugins.SettingsField{Type: "url"}, "javascript:alert(1)", true, nil, false},
		{plugins.SettingsField{Type: "color"}, "#ff0000", true, "#ff0000", true},
		{plugins.SettingsField{Type: "color"}, "red;x", true, nil, false},
		{plugins.SettingsField{Type: "date"}, "2026-11-12", true, "2026-11-12", true},
		{plugins.SettingsField{Type: "date"}, "12/11/2026", true, nil, false},
		{plugins.SettingsField{Type: "password"}, "", true, nil, false},
		{plugins.SettingsField{Type: "radio", Options: []plugins.SettingsOption{{Value: "a"}}}, "b", true, nil, false},
		{plugins.SettingsField{Type: "radio", Options: []plugins.SettingsOption{{Value: "a"}}}, "a", true, "a", true},
		{plugins.SettingsField{Type: "text"}, "a\nb", true, "a b", true},
		{plugins.SettingsField{Type: "text"}, "", false, nil, false},
		{plugins.SettingsField{Type: "image"}, " https://x.test/wp-content/uploads/a.svg ", true, "https://x.test/wp-content/uploads/a.svg", true},
		{plugins.SettingsField{Type: "image"}, "/wp-content/uploads/2026/11/a.png", true, "/wp-content/uploads/2026/11/a.png", true},
		{plugins.SettingsField{Type: "image"}, "", true, "", true},
		{plugins.SettingsField{Type: "image"}, "javascript:alert(1)", true, nil, false},
		{plugins.SettingsField{Type: "image"}, "data:image/svg+xml,<svg onload=alert(1)>", true, nil, false},
		{plugins.SettingsField{Type: "image"}, "//evil.test/a.png", true, nil, false},
		{plugins.SettingsField{Type: "image"}, `/\evil.test/a.png`, true, nil, false},
		{plugins.SettingsField{Type: "image"}, "a.png", true, nil, false},
	}
	for i, c := range cases {
		got, ok := pluginFieldValue(c.f, c.raw, c.present)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%d %s %q: got %v %v, want %v %v", i, c.f.Type, c.raw, got, ok, c.want, c.ok)
		}
	}
	row := pluginFieldRow("plugin_x_pw", plugins.SettingsField{ID: "pw", Label: "Key", Type: "password"}, "secret")
	if strings.Contains(row, "secret") || !strings.Contains(row, `type="password"`) {
		t.Errorf("password row leaks the value: %s", row)
	}
	row = pluginFieldRow("plugin_x_img", plugins.SettingsField{ID: "img", Label: `Icon "x"`, Type: "image"}, `https://x.test/a.svg?"><script>`)
	for _, want := range []string{`type="url" id="plugin_x_img" value="https://x.test/a.svg?&#34;&gt;&lt;script&gt;"`, `class="button pursae-media-field__select hide-if-no-js" data-title="Icon &#34;x&#34;"`, `<img class="pursae-media-field__preview" src="https://x.test/a.svg?&#34;&gt;&lt;script&gt;"`} {
		if !strings.Contains(row, want) {
			t.Errorf("image row misses %s:\n%s", want, row)
		}
	}
	if row = pluginFieldRow("plugin_x_img", plugins.SettingsField{ID: "img", Label: "Icon", Type: "image"}, nil); strings.Count(row, " hidden") != 2 {
		t.Errorf("empty image row should hide the preview and Remove: %s", row)
	}
	row = pluginFieldRow("plugin_x_r", plugins.SettingsField{ID: "r", Label: "R", Type: "radio", Options: []plugins.SettingsOption{{Value: "a", Label: "<A>"}}}, "a")
	if !strings.Contains(row, `value="a" checked="checked"`) || !strings.Contains(row, "&lt;A&gt;") {
		t.Errorf("radio row: %s", row)
	}
}

func TestDashboardLayoutExtra(t *testing.T) {
	u := &store.User{Meta: map[string]any{"meta-box-order_dashboard": map[string]any{"side": "plugin_x_w,dashboard_primary"}}}
	n, s, _, _ := dashboardLayout(u, false, "plugin_x_w", "plugin_x_v")
	if len(s) == 0 || s[0] != "plugin_x_w" || n[len(n)-1] != "plugin_x_v" {
		t.Fatalf("normal %v side %v", n, s)
	}
}

// uiDemoPackage builds examples/plugins/ui-demo and packs it (unsigned).
func uiDemoPackage(t *testing.T) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Wasm plugin build in -short mode")
	}
	out := filepath.Join(t.TempDir(), "ui.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./examples/plugins/ui-demo")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	wasm, _ := os.ReadFile(out)
	manifest, _ := os.ReadFile("../../examples/plugins/ui-demo/plugin.json")
	var buf bytes.Buffer
	if err := plugins.WritePackage(&buf, manifest, wasm, nil, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPluginUIAdminPageDashboardMetaBoxAndBlock(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	ctx := context.Background()
	srv.cfg.AllowUnsignedPlugins = true
	if err := srv.settings.Set(ctx, map[string]any{"plugin_allow_unsigned": true}); err != nil {
		t.Fatal(err)
	}
	m, err := srv.installPackage(ctx, uiDemoPackage(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m.Permissions)
	if err := srv.loadPlugin(ctx, m.Slug, raw); err != nil {
		t.Fatal(err)
	}
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'active', permissions = $2 WHERE slug = $1`, m.Slug, raw)

	// Admin page: in the Tools menu, a nonce-protected form, notices from the plugin.
	code, body, _ := get(t, c, base+"/wp-admin/admin?page=ui-demo")
	if code != http.StatusOK || !strings.Contains(body, `href="/wp-admin/admin?page=ui-demo"`) || !strings.Contains(body, `<input name="message"`) {
		t.Fatalf("admin page %d", code)
	}
	nonce := nonceRe.FindStringSubmatch(body)
	if nonce == nil {
		t.Fatal("no nonce on the admin page")
	}
	if code, _, _ := post(t, c, base+"/wp-admin/admin?page=ui-demo", url.Values{"message": {"no nonce"}}); code == http.StatusOK {
		t.Fatal("POST without a nonce was accepted")
	}
	code, body, _ = post(t, c, base+"/wp-admin/admin?page=ui-demo", url.Values{"_wpnonce": {nonce[1]}, "message": {"Board <b>up</b>"}})
	if code != http.StatusOK || !strings.Contains(body, "Message saved.") || !strings.Contains(body, "Board &lt;b&gt;up&lt;/b&gt;") {
		t.Fatalf("admin page POST %d", code)
	}
	if code, _, _ := get(t, c, base+"/wp-admin/admin?page=ui-demo.nope"); code != http.StatusForbidden {
		t.Errorf("unknown plugin page: %d", code)
	}

	// Dashboard widget.
	if _, body, _ := get(t, c, base+"/wp-admin/"); !strings.Contains(body, `id="plugin_ui_demo_board"`) || !strings.Contains(body, "Board &lt;b&gt;up") {
		t.Error("dashboard widget missing")
	}

	// Meta box in Pursae Editor, saved through the meta-box-loader POST.
	p := &store.Post{Type: "post", Status: "publish", Title: "Badge", AuthorID: 1,
		Content: `<!-- wp:ui-demo/badge {"text":"Go","style":"outline"} /-->`}
	if err := srv.store.InsertPost(ctx, p); err != nil {
		t.Fatal(err)
	}
	_, body, _ = get(t, c, base+"/wp-admin/post?post="+itoa64(p.ID)+"&action=edit")
	if !strings.Contains(body, `class="metabox-base-form"`) || !strings.Contains(body, `name="plugin_ui_demo_subtitle"`) || !strings.Contains(body, "setAvailableMetaBoxesPerLocation") || !strings.Contains(body, `"ui-demo/badge"`) {
		t.Fatal("block editor misses the plugin meta box or block")
	}
	nonce = nonceRe.FindStringSubmatch(body[strings.Index(body, `class="metabox-base-form"`):])
	form := url.Values{"_wpnonce": {nonce[1]}, "post_ID": {itoa64(p.ID)}, "post_type": {"post"}, pluginMetaBoxField: {"ui-demo:notes"},
		"plugin_ui_demo_subtitle": {"Hello"}, "plugin_ui_demo_mood": {"sleepy"}, "plugin_ui_demo_featured": {"1"}}
	if code, _, _ := post(t, c, base+"/wp-admin/post?post="+itoa64(p.ID)+"&action=edit&meta-box-loader=1", form); code != http.StatusOK {
		t.Fatalf("meta-box-loader %d", code)
	}
	got, _ := srv.store.GetPost(ctx, p.ID)
	if got.Meta["plugin_ui_demo_subtitle"] != "Hello" || got.Meta["plugin_ui_demo_mood"] != "sleepy" || got.Meta["plugin_ui_demo_featured"] != true || got.Title != "Badge" {
		t.Fatalf("meta = %v", got.Meta)
	}
	form.Set("plugin_ui_demo_mood", "evil")
	form.Del("plugin_ui_demo_featured")
	post(t, c, base+"/wp-admin/post?post="+itoa64(p.ID)+"&action=edit&meta-box-loader=1", form)
	got, _ = srv.store.GetPost(ctx, p.ID)
	if got.Meta["plugin_ui_demo_mood"] != "sleepy" || got.Meta["plugin_ui_demo_featured"] != false {
		t.Fatalf("invalid select stored or checkbox kept: %v", got.Meta)
	}

	// The dynamic block on the front end and through the block renderer.
	if _, body, _ := get(t, c, srv.links(ctx).Permalink(p)); !strings.Contains(body, `<span class="ui-demo-badge ui-demo-badge--outline">Go</span>`) {
		t.Error("block not rendered on the front end")
	}
	// The Web Builder widget renders through the runtime.
	if html, ok := srv.renderPluginWidget(ctx, "ui-demo/badge", map[string]any{"text": "WB"}); !ok || !strings.Contains(html, ">WB</span>") {
		t.Errorf("builder widget = %q %v", html, ok)
	}
}

// An untouched optional color field must save as "", not as the black a bare <input type="color">
// shows and posts. The form is submitted the way a browser does: every named control, with the
// value it renders.
func TestPluginColorFieldUntouchedStaysEmpty(t *testing.T) {
	f := plugins.SettingsField{ID: "accent", Label: "Accent", Type: "color"}
	submit := func(row string) (string, bool) {
		doc, err := xhtml.Parse(strings.NewReader("<table>" + row + "</table>"))
		if err != nil {
			t.Fatal(err)
		}
		var val string
		var found bool
		var walk func(n *xhtml.Node)
		walk = func(n *xhtml.Node) {
			if n.Type == xhtml.ElementNode && n.Data == "input" {
				attr := map[string]string{}
				for _, a := range n.Attr {
					attr[a.Key] = a.Val
				}
				if attr["name"] == "plugin_x_accent" {
					val, found = attr["value"], true
				} else if attr["name"] != "" {
					t.Errorf("unexpected named control %q", attr["name"])
				}
				if attr["type"] == "color" && attr["name"] != "" {
					t.Errorf("the native color input is posted: %s", row)
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)
		return val, found
	}
	for _, stored := range []any{nil, ""} {
		raw, present := submit(pluginFieldRow("plugin_x_accent", f, stored))
		if got, ok := pluginFieldValue(f, raw, present); !ok || got != "" {
			t.Errorf("stored %v: untouched color saved as %v (ok %v), want \"\"", stored, got, ok)
		}
	}
	row := pluginFieldRow("plugin_x_accent", f, "#AbC")
	if raw, present := submit(row); raw != "#AbC" || !present {
		t.Errorf("set color not posted back: %q %v", raw, present)
	}
	if !strings.Contains(row, `type="color" class="pursae-color-field__swatch hide-if-no-js" value="#aabbcc"`) {
		t.Errorf("swatch should show the stored colour: %s", row)
	}
	if row = pluginFieldRow("plugin_x_accent", f, `#fff" onclick="x`); strings.Contains(row, `" onclick`) {
		t.Errorf("value not escaped: %s", row)
	}
}
