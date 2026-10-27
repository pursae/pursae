package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestGoWebEditor(t *testing.T) {
	ts, c := testServer(t)
	base := ts.URL
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Builder Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK || !strings.Contains(body, "Success!") {
		t.Fatalf("install: %d", code)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	var pageID string
	if err := getPool(t).QueryRow(context.Background(), `SELECT id::text FROM posts WHERE type='page' AND title='Sample Page'`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}

	// The pages list links to the builder.
	_, body, _ := get(t, c, base+"/wp-admin/edit?post_type=page")
	if !strings.Contains(body, "/wp-admin/post?post="+pageID+"&amp;action=elementor") || !strings.Contains(body, "Edit with Golementor") {
		t.Errorf("pages list has no Edit with Golementor action")
	}

	// The editor page: skeleton, ordered assets, Backbone templates and the inline configs.
	code, body, _ := get(t, c, base+"/wp-admin/post?post="+pageID+"&action=elementor")
	if code != http.StatusOK {
		t.Fatalf("editor: %d %.300s", code, body)
	}
	for _, want := range []string{`<div id="elementor-editor-wrapper">`, `id="elementor-preview-responsive-wrapper"`, "var ElementorConfig = {",
		"var elementorCommonConfig = {", `"initial_document":{`, `id="tmpl-elementor-panel"`, "/wp-content/plugins/gowebeditor/assets/js/editor.min.js",
		"wp.i18n.setLocaleData(", "<title>Edit “Sample Page” with Golementor</title>", `"url":"` + base + `/wp-admin/admin-ajax"`,
		"tinyMCEPreInit", `"kit_id":`} {
		if !strings.Contains(body, want) {
			t.Errorf("editor page misses %q", want)
		}
	}
	for _, bad := range []string{"@@", "localhost:8095", "load-scripts.php", "fonts.googleapis.com", "cdn.jsdelivr.net", "go.elementor.com", ">Elementor<"} {
		if strings.Contains(body, bad) {
			t.Errorf("editor page contains %q", bad)
		}
	}
	if n := strings.Count(body, `<script type="text/template" id="tmpl-`) + strings.Count(body, `<script type="text/html" id="tmpl-`); n < 180 {
		t.Errorf("editor has %d templates", n)
	}
	m := regexp.MustCompile(`"ajax":\{"nonce":"([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no elementor_ajax nonce")
	}
	nonce := m[1]
	rest := regexp.MustCompile(`"X-WP-Nonce":"([^"]+)"`).FindStringSubmatch(body)
	if rest == nil {
		t.Fatal("no REST nonce")
	}
	if code, _, _ := get(t, c, base+"/wp-content/plugins/gowebeditor/assets/js/editor.min.js"); code != http.StatusOK {
		t.Errorf("editor asset: %d", code)
	}

	ajax := func(actions string) (int, map[string]any) {
		t.Helper()
		resp, err := c.PostForm(base+"/wp-admin/admin-ajax", url.Values{"action": {"elementor_ajax"}, "_nonce": {nonce},
			"editor_post_id": {pageID}, "initial_document_id": {pageID}, "actions": {actions}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("ajax response: %v %.200s", err, b)
		}
		return resp.StatusCode, out
	}
	result := func(out map[string]any, id string) map[string]any {
		t.Helper()
		r, _ := out["data"].(map[string]any)["responses"].(map[string]any)[id].(map[string]any)
		if r == nil {
			t.Fatalf("no response %q in %v", id, out)
		}
		return r
	}

	code, out := ajax(`{"w":{"action":"get_widgets_config","data":{"exclude":{}}},"x":{"action":"no_such_action","data":{}}}`)
	if code != http.StatusOK {
		t.Fatalf("get_widgets_config: %d", code)
	}
	if w := result(out, "w"); w["success"] != true || w["data"].(map[string]any)["heading"] == nil {
		t.Errorf("widgets config has no heading")
	}
	if x := result(out, "x"); x["success"] != false || x["code"] != float64(400) || x["data"] != "Action not found." {
		t.Errorf("unknown action: %v", x)
	}
	resp, _ := c.PostForm(base+"/wp-admin/admin-ajax", url.Values{"action": {"elementor_ajax"}, "_nonce": {"bad"}, "actions": {`{"r":{"action":"get_revisions"}}`}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad nonce: %d", resp.StatusCode)
	}

	// Publish a container with a heading and a button.
	tree := `[{"id":"a1b2c3d","elType":"container","isInner":false,"settings":{"flex_direction":"row"},"elements":[` +
		`{"id":"b1b2c3d","elType":"widget","widgetType":"heading","isInner":false,"settings":{"title":"Built with Go","header_size":"h2","title_color":"#C0392B"},"elements":[]},` +
		`{"id":"c1b2c3d","elType":"widget","widgetType":"button","isInner":false,"settings":{"text":"Click me","link":{"url":"https://example.com","is_external":"","nofollow":"","custom_attributes":""}},"elements":[]}]}]`
	_, out = ajax(`{"save_builder":{"action":"save_builder","data":{"status":"publish","elements":` + tree + `,"settings":{"post_title":"Builder Page","post_status":"publish","hide_title":"yes"}}}}`)
	sb := result(out, "save_builder")
	if sb["success"] != true {
		t.Fatalf("save_builder: %v", sb)
	}
	data := sb["data"].(map[string]any)
	if data["status"] != "publish" || data["config"].(map[string]any)["document"].(map[string]any)["status"].(map[string]any)["value"] != "publish" {
		t.Errorf("save_builder response: %v", data)
	}
	if ids, _ := data["revisions_ids"].([]any); len(ids) < 2 {
		t.Errorf("save_builder revisions: %v", data["revisions_ids"])
	}

	var mode, title, typ string
	_ = getPool(t).QueryRow(context.Background(), `SELECT meta->>'_elementor_edit_mode', title, meta->>'_elementor_template_type' FROM posts WHERE id=$1::bigint`, pageID).Scan(&mode, &title, &typ)
	if mode != "builder" || title != "Builder Page" || typ != "wp-page" {
		t.Errorf("stored: mode=%q title=%q type=%q", mode, title, typ)
	}
	var stored string
	_ = getPool(t).QueryRow(context.Background(), `SELECT meta->>'_elementor_data' FROM posts WHERE id=$1::bigint`, pageID).Scan(&stored)
	if !strings.Contains(stored, `"title":"Built with Go","header_size":"h2"`) {
		t.Errorf("element tree not stored as sent (settings order): %.200s", stored)
	}

	_, out = ajax(`{"d":{"action":"get_document_config","data":{"id":` + pageID + `}},"r":{"action":"get_revisions","data":{}}}`)
	doc := result(out, "d")["data"].(map[string]any)
	els, _ := json.Marshal(doc["elements"])
	if !strings.Contains(string(els), `"Built with Go"`) || !strings.Contains(string(els), `"widgetType":"button"`) {
		t.Errorf("document elements: %s", els)
	}
	if st := doc["settings"].(map[string]any)["settings"].(map[string]any); st["post_title"] != "Builder Page" || st["hide_title"] != "yes" {
		t.Errorf("document settings: %v %v", st["post_title"], st["hide_title"])
	}
	// Widgets used in the document come with their full controls (the preview's handlers need the
	// frontend_available ones before get_widgets_config arrives); unused ones do not.
	dw, _ := doc["widgets"].(map[string]any)
	ctlCount := func(t string) int {
		m, _ := dw[t].(map[string]any)
		c, _ := m["controls"].(map[string]any)
		return len(c)
	}
	if ctlCount("button") == 0 || ctlCount("heading") == 0 || ctlCount("video") != 0 {
		t.Errorf("document widget controls: button=%d heading=%d video=%d", ctlCount("button"), ctlCount("heading"), ctlCount("video"))
	}
	revs, _ := result(out, "r")["data"].([]any)
	if len(revs) < 2 {
		t.Fatalf("revisions: %v", revs)
	}
	revID := revs[1].(map[string]any)["id"].(float64)
	_, out = ajax(`{"rd":{"action":"get_revision_data","data":{"id":` + jsonNum(revID) + `}}}`)
	if rd := result(out, "rd"); rd["success"] != true || !strings.Contains(gweJSONText(rd["data"]), "Built with Go") {
		t.Errorf("revision data: %v", rd)
	}

	// Templates: save a section, list it, load it with fresh ids.
	_, out = ajax(`{"t":{"action":"save_template","data":{"source":"local","type":"section","title":"My Section","content":` + strconvQuote(tree) + `}}}`)
	tpl := result(out, "t")
	if tpl["success"] != true {
		t.Fatalf("save_template: %v", tpl)
	}
	tid := jsonNum(tpl["data"].(map[string]any)["template_id"].(float64))
	_, out = ajax(`{"l":{"action":"get_library_data","data":{"filter_sources":["local"]}},"g":{"action":"get_template_data","data":{"source":"local","template_id":` + tid + `,"edit_mode":true}}}`)
	if l := gweJSONText(result(out, "l")["data"]); !strings.Contains(l, "My Section") {
		t.Errorf("library: %s", l)
	}
	if g := gweJSONText(result(out, "g")["data"]); !strings.Contains(g, "Built with Go") || strings.Contains(g, "a1b2c3d") {
		t.Errorf("template data (ids must be regenerated): %.300s", g)
	}

	// REST: kit globals and the current user.
	req, _ := http.NewRequest(http.MethodGet, base+"/wp-json/elementor/v1/globals", nil)
	req.Header.Set("X-WP-Nonce", rest[1])
	gr, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gb, _ := io.ReadAll(gr.Body)
	gr.Body.Close()
	if gr.StatusCode != http.StatusOK || !strings.Contains(string(gb), `"primary"`) {
		t.Errorf("globals: %d %s", gr.StatusCode, gb)
	}

	// Heartbeat hands the editor a fresh nonce.
	_, hb, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"heartbeat"}, "data[elementor_post_lock][post_ID]": {pageID}})
	if !strings.Contains(hb, `"elementorNonce"`) {
		t.Errorf("heartbeat: %s", hb)
	}

	// The front end shows the builder wrapper with the frontend assets.
	_, body, _ = getPage(t, c, base+"/?page_id="+pageID)
	for _, want := range []string{`class="elementor elementor-` + pageID, "gowebeditor/assets/css/frontend.min.css", "elementorFrontendConfig", "elementor-page-" + pageID} {
		if !strings.Contains(body, want) {
			t.Errorf("front end misses %q", want)
		}
	}

	// The preview iframe: empty wrapper, edit mode, never cached.
	pr, err := c.Get(base + "/?page_id=" + pageID + "&preview=true&elementor-preview=" + pageID + "&ver=1")
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := io.ReadAll(pr.Body)
	pr.Body.Close()
	if pr.StatusCode != http.StatusOK || !strings.Contains(pr.Header.Get("Cache-Control"), "no-store") {
		t.Errorf("preview: %d %q", pr.StatusCode, pr.Header.Get("Cache-Control"))
	}
	for _, want := range []string{`data-elementor-id="` + pageID + `"`, "jQuery.holdReady( true )", `"edit":true`, "editor-preview.min.css"} {
		if !strings.Contains(string(pb), want) {
			t.Errorf("preview misses %q", want)
		}
	}
	if strings.Contains(string(pb), `id="wpadminbar"`) {
		t.Error("preview shows the admin bar")
	}

	// Golementor Canvas drops the theme header and footer.
	_, out = ajax(`{"s":{"action":"save_builder","data":{"status":"publish","elements":` + tree + `,"settings":{"post_title":"Builder Page","template":"elementor_canvas"}}}}`)
	if result(out, "s")["success"] != true {
		t.Fatalf("save canvas: %v", out)
	}
	_, body, _ = getPage(t, c, base+"/?page_id="+pageID)
	if !strings.Contains(body, "elementor-template-canvas") || strings.Contains(body, `class="site-title"`) {
		t.Errorf("canvas template not applied")
	}
}

func jsonNum(f float64) string { b, _ := json.Marshal(int64(f)); return string(b) }

func gweJSONText(v any) string { b, _ := json.Marshal(v); return string(b) }
