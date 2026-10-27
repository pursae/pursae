package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestGoWebEditorPro covers the upstream Pro port end to end: the editor loads with Pro's bundle,
// panels and no upgrade prompts; a page with posts, form, nav menu and a dynamic tag publishes and
// renders; a form submission is stored and e-mailed; a popup prints its markup; render_tags
// resolves a dynamic tag.
func TestGoWebEditorPro(t *testing.T) {
	ts, c := testServer(t)
	mailDir := filepath.Join(filepath.Dir(t.TempDir()), "mail") // mailer(): <uploads>/../mail
	base := ts.URL
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Pro Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK || !strings.Contains(body, "Success!") {
		t.Fatalf("install: %d", code)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	ctx := context.Background()
	pool := getPool(t)
	var pageID, helloID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM posts WHERE type='page' AND title='Sample Page'`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM posts WHERE type='post' ORDER BY id LIMIT 1`).Scan(&helloID); err != nil {
		t.Fatal(err)
	}
	var menuID string
	if err := pool.QueryRow(ctx, `INSERT INTO menus (slug, name, location) VALUES ('main', 'Main', 'primary') RETURNING id::text`).Scan(&menuID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO menu_items (menu_id, sort_order, kind, url, label) VALUES ($1, 1, 'custom', '/about/', 'About Us'),
		($1, 2, 'custom', '/contact/', 'Contact')`, menuID); err != nil {
		t.Fatal(err)
	}

	// --- the editor ---------------------------------------------------------------------------------
	code, body, _ := get(t, c, base+"/wp-admin/post?post="+pageID+"&action=elementor")
	if code != http.StatusOK {
		t.Fatalf("editor: %d", code)
	}
	for _, want := range []string{"/wp-content/plugins/gowebeditor/assets/pro/js/editor.min.js", "var elementorProEditorConfig = {",
		"var elementorAppProConfig = ", "window.ElementorProDisplayConditions", `id="tmpl-elementor-form-content"`,
		`id="tmpl-elementor-theme-builder-conditions-view"`, `"pro-elements":{"title":"Pro"`, `"displayConditions":{`,
		`"dynamicTags":{"tags":{`, `"post-title":{`, "pro/js/packages/editor-documents-extended/editor-documents-extended.min.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("editor page misses %q", want)
		}
	}
	for _, bad := range []string{"@@", "localhost:8095", "go.elementor.com", "Connect & Activate", "tmpl-elementor-pro-template-library-activate-license-button",
		"promotion_control", "Upgrade to Elementor", ">Elementor<", "Elementor Pro"} {
		// wp.i18n locale data keeps the original strings as lookup keys; their translations are rebranded.
		visible := regexp.MustCompile(`(?m)^.*wp\.i18n\.setLocaleData\(.*$`).ReplaceAllString(body, "")
		if strings.Contains(visible, bad) {
			t.Errorf("editor page contains %q", bad)
		}
	}
	m := regexp.MustCompile(`"ajax":\{"url":"[^"]*","nonce":"([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		m = regexp.MustCompile(`"ajax":\{"nonce":"([^"]+)"`).FindStringSubmatch(body)
	}
	if m == nil {
		t.Fatal("no elementor_ajax nonce")
	}
	nonce := m[1]
	ajax := func(editorID, actions string) map[string]any {
		t.Helper()
		resp, err := c.PostForm(base+"/wp-admin/admin-ajax", url.Values{"action": {"elementor_ajax"}, "_nonce": {nonce},
			"editor_post_id": {editorID}, "initial_document_id": {editorID}, "actions": {actions}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("ajax: %v %.300s", err, b)
		}
		return out
	}
	result := func(out map[string]any, id string) map[string]any {
		t.Helper()
		r, _ := out["data"].(map[string]any)["responses"].(map[string]any)[id].(map[string]any)
		if r == nil {
			t.Fatalf("no response %q in %.300v", id, out)
		}
		if r["success"] != true {
			t.Fatalf("%s failed: %v", id, r["data"])
		}
		return r
	}
	wc := result(ajax(pageID, `{"w":{"action":"get_widgets_config","data":{"exclude":{}}}}`), "w")["data"].(map[string]any)
	for _, w := range []string{"posts", "form", "nav-menu", "slides", "popup", "loop-grid", "breadcrumbs"} {
		if w == "popup" {
			continue
		}
		if wc[w] == nil {
			t.Errorf("widgets config has no %s", w)
		}
	}
	if nav, _ := wc["nav-menu"].(map[string]any); nav != nil {
		menu, _ := nav["controls"].(map[string]any)["menu"].(map[string]any)
		if menu == nil || menu["type"] != "select" || menu["default"] != menuID {
			t.Errorf("nav-menu menu control = %v", menu)
		}
	}

	// --- render_tags ----------------------------------------------------------------------------------
	key := base64.StdEncoding.EncodeToString([]byte("site-title")) + "-" + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(`{}`)))
	tags := result(ajax(pageID, `{"t":{"action":"render_tags","data":{"post_id":`+pageID+`,"tags":["`+key+`"]}}}`), "t")["data"].(map[string]any)
	if tags[key] != "Pro Site" {
		t.Errorf("render_tags site-title = %v", tags[key])
	}

	// --- a page with posts, form, nav menu and a dynamic heading -------------------------------------
	dyn := `[elementor-tag id=\"d1d1d1d\" name=\"site-title\" settings=\"%7B%7D\"]`
	elements := `[{"id":"c0c0c01","elType":"container","isInner":false,"settings":{},"elements":[
		{"id":"h0h0h01","elType":"widget","widgetType":"heading","settings":{"title":"Static","__dynamic__":{"title":"` + dyn + `"}},"elements":[]},
		{"id":"n0n0n01","elType":"widget","widgetType":"nav-menu","settings":{"menu":"` + menuID + `"},"elements":[]},
		{"id":"p0p0p01","elType":"widget","widgetType":"posts","settings":{"classic_posts_per_page":3},"elements":[]},
		{"id":"f0f0f01","elType":"widget","widgetType":"form","settings":{"form_name":"Contact","submit_actions":["email","save-to-database"],
			"email_to":"owner@example.test","email_subject":"New lead from [field id=\"name\"]",
			"form_fields":[{"_id":"a1","custom_id":"name","field_type":"text","field_label":"Name","required":"true"},
				{"_id":"a2","custom_id":"email","field_type":"email","field_label":"Email","required":"true"},
				{"_id":"a3","custom_id":"message","field_type":"textarea","field_label":"Message"}]},"elements":[]}
	]}]`
	var compact strings.Builder
	for _, line := range strings.Split(elements, "\n") {
		compact.WriteString(strings.TrimSpace(line))
	}
	save := ajax(pageID, `{"s":{"action":"save_builder","data":{"status":"publish","elements":`+compact.String()+`,"settings":{"post_title":"Sample Page","post_status":"publish"}}}}`)
	result(save, "s")
	code, page, _ := getPage(t, c, base+"/?page_id="+pageID)
	if code == http.StatusMovedPermanently || code == http.StatusFound {
		_, page, _ = get(t, c, base+"/sample-page/")
	}
	for _, want := range []string{
		`<h2 class="elementor-heading-title elementor-size-default">Pro Site</h2>`, // dynamic tag
		"elementor-nav-menu", "About Us", "elementor-posts-container", "Hello world", "elementor-form", `name="form_fields[email]"`,
		"/wp-content/plugins/gowebeditor/assets/pro/js/frontend.min.js", "pro/js/elements-handlers.min.js", "var ElementorProFrontendConfig",
		"pro/css/widget-nav-menu.min.css", "pro/css/widget-posts.min.css", "pro/css/widget-form.min.css", "smartmenus",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page misses %q", want)
		}
	}
	if strings.Contains(page, "unsupported element") {
		t.Errorf("page has unsupported elements")
	}

	// --- form submission ------------------------------------------------------------------------------
	resp, err := http.PostForm(base+"/wp-admin/admin-ajax", url.Values{"action": {"elementor_pro_forms_send_form"}, "post_id": {pageID},
		"form_id": {"f0f0f01"}, "referer_title": {"Sample Page"}, "referrer": {base + "/sample-page/"},
		"form_fields[name]": {"Ada Lovelace"}, "form_fields[email]": {"ada@example.test"}, "form_fields[message]": {"Hello\nfrom the form"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var fr struct {
		Success bool `json:"success"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &fr) != nil || !fr.Success || fr.Data.Message != "Your submission was successful." {
		t.Errorf("form submission: %s", b)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT fields::text FROM gwe_form_submissions WHERE element_id = 'f0f0f01'`).Scan(&stored); err != nil {
		t.Errorf("submission not stored: %v", err)
	} else if !strings.Contains(stored, "Ada Lovelace") {
		t.Errorf("stored submission: %s", stored)
	}
	found := false
	for i := 0; i < 20 && !found; i++ {
		files, _ := filepath.Glob(filepath.Join(mailDir, "*.eml"))
		for _, f := range files {
			raw, _ := os.ReadFile(f)
			if strings.Contains(string(raw), "owner@example.test") && strings.Contains(string(raw), "New lead from Ada Lovelace") {
				found = true
			}
		}
		if !found {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !found {
		t.Errorf("no form e-mail in %s", mailDir)
	}
	// required fields
	resp, err = http.PostForm(base+"/wp-admin/admin-ajax", url.Values{"action": {"elementor_pro_forms_send_form"}, "post_id": {pageID},
		"form_id": {"f0f0f01"}, "form_fields[name]": {""}, "form_fields[email]": {"not-an-email"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"success":false`) || !strings.Contains(string(b), "This field is required.") {
		t.Errorf("invalid submission: %s", b)
	}

	// --- popup ------------------------------------------------------------------------------------------
	popupData := `[{"id":"q0q0q01","elType":"container","settings":{},"elements":[{"id":"q0q0q02","elType":"widget","widgetType":"heading","settings":{"title":"Popup Hello"},"elements":[]}]}]`
	meta, _ := json.Marshal(map[string]any{"_elementor_edit_mode": "builder", "_elementor_template_type": "popup", "_elementor_data": popupData,
		"_elementor_page_settings": map[string]any{"width": map[string]any{"unit": "px", "size": 500}}, "_gwe_conditions": []string{"include/general"},
		"_elementor_popup_display_settings": map[string]any{"triggers": map[string]any{"page_load": "yes", "page_load_delay": 2}, "timing": map[string]any{}}})
	var popupID string
	if err := pool.QueryRow(ctx, `INSERT INTO posts (type, author_id, title, status, published_at, meta) VALUES ('elementor_library', 1, 'Welcome', 'publish', now(), $1::jsonb)
		RETURNING id::text`, string(meta)).Scan(&popupID); err != nil {
		t.Fatal(err)
	}
	_, page, _ = getPage(t, c, base+"/?p="+helloID)
	for _, want := range []string{`data-elementor-type="popup" data-elementor-id="` + popupID + `"`, "elementor-location-popup", "Popup Hello",
		"pro/css/conditionals/popup.min.css", "dialog.min.js", `&quot;page_load&quot;:&quot;yes&quot;`} {
		if !strings.Contains(page, want) {
			t.Errorf("popup page misses %q", want)
		}
	}

	// --- admin screens ----------------------------------------------------------------------------------
	for _, tab := range []string{"popups", "submissions", "code", "fonts", "roles", "settings"} {
		code, body, _ := get(t, c, base+"/wp-admin/gowebeditor-pro?tab="+tab)
		if code != http.StatusOK || !strings.Contains(body, "nav-tab-active") {
			t.Errorf("admin %s: %d", tab, code)
		}
		if tab == "submissions" && !strings.Contains(body, "Ada Lovelace") {
			t.Errorf("submissions screen misses the entry")
		}
		if tab == "popups" && !strings.Contains(body, "Welcome") {
			t.Errorf("popups screen misses the popup")
		}
	}
}
