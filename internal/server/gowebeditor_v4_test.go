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

// Golementor v4 (atomic): the editor has the atomic experiments on, global classes and variables
// round-trip through REST, and an atomic page saved with save_builder renders on the front end
// with its stylesheets.
func TestGoWebEditorV4(t *testing.T) {
	ts, c := testServer(t)
	base := ts.URL
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Atomic Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
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

	// Editor page: v4 experiments, atomic config, v2 panels, no upstream branding.
	code, body, _ := get(t, c, base+"/wp-admin/post?post="+pageID+"&action=elementor")
	if code != http.StatusOK {
		t.Fatalf("editor: %d", code)
	}
	for _, want := range []string{`"e_atomic_elements":true`, `"e_opt_in_v4":true`, `"atomic":{`, "editor-editing-panel", "editor-global-classes", "editor-variables",
		"editor-components", `"styles_schema":`} {
		if !strings.Contains(body, want) {
			t.Errorf("editor page misses %q", want)
		}
	}
	for _, bad := range []string{"localhost:8095", ">Elementor<", "@@"} {
		if strings.Contains(body, bad) {
			t.Errorf("editor page contains %q", bad)
		}
	}
	nonce := regexp.MustCompile(`"ajax":\{"nonce":"([^"]+)"`).FindStringSubmatch(body)
	rest := regexp.MustCompile(`"X-WP-Nonce":"([^"]+)"`).FindStringSubmatch(body)
	if nonce == nil || rest == nil {
		t.Fatal("no nonces")
	}
	call := func(method, path, payload string) (int, string) {
		t.Helper()
		var r io.Reader
		if payload != "" {
			r = strings.NewReader(payload)
		}
		req, _ := http.NewRequest(method, base+"/wp-json/elementor/v1/"+path, r)
		req.Header.Set("X-WP-Nonce", rest[1])
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// A global class (with a hover and a mobile variant) and a color variable.
	class := `{"id":"g-1a2b3c4","label":"brand-card","type":"class","variants":[` +
		`{"meta":{"breakpoint":"desktop","state":null},"props":{"padding":{"$$type":"size","value":{"size":16,"unit":"px"}},"background":{"$$type":"background","value":{"color":{"$$type":"color","value":"#fff7f0"}}}},"custom_css":null},` +
		`{"meta":{"breakpoint":"desktop","state":"hover"},"props":{"background":{"$$type":"background","value":{"color":{"$$type":"color","value":"#ffe0cc"}}}},"custom_css":null},` +
		`{"meta":{"breakpoint":"mobile","state":null},"props":{"padding":{"$$type":"size","value":{"size":8,"unit":"px"}}},"custom_css":null}]}`
	if code, b := call(http.MethodPut, "global-classes?context=frontend", `{"changes":{"added":["g-1a2b3c4"],"deleted":[],"modified":[],"order":true},"items":{"g-1a2b3c4":`+class+`},"order":["g-1a2b3c4"]}`); code != http.StatusNoContent {
		t.Fatalf("put global classes: %d %s", code, b)
	}
	if code, b := call(http.MethodGet, "global-classes", ""); code != http.StatusOK || !strings.Contains(b, `"label":"brand-card"`) {
		t.Errorf("global classes: %d %s", code, b)
	}
	code, b := call(http.MethodPost, "variables/create", `{"type":"global-color-variable","label":"brand","value":"#ff5500"}`)
	if code != http.StatusCreated {
		t.Fatalf("create variable: %d %s", code, b)
	}
	var created struct {
		Data struct {
			Variable struct {
				ID string `json:"id"`
			} `json:"variable"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(b), &created)
	varID := created.Data.Variable.ID
	if code, b := call(http.MethodGet, "variables/list", ""); code != http.StatusOK || !strings.Contains(b, varID) {
		t.Errorf("variables: %d %s", code, b)
	}

	// An atomic page: a div block with a heading (local style using the variable, the global class).
	tree := `[{"id":"d1v0b10","elType":"e-div-block","isInner":false,"settings":{"classes":{"$$type":"classes","value":["e-d1v0b10-aaaaaaa"]}},` +
		`"styles":{"e-d1v0b10-aaaaaaa":{"id":"e-d1v0b10-aaaaaaa","label":"local","type":"class","variants":[{"meta":{"breakpoint":"desktop","state":null},"props":{"padding":{"$$type":"dimensions","value":{"block-start":{"$$type":"size","value":{"size":40,"unit":"px"}},"block-end":{"$$type":"size","value":{"size":40,"unit":"px"}}}}},"custom_css":null},` +
		`{"meta":{"breakpoint":"tablet","state":null},"props":{"padding":{"$$type":"size","value":{"size":20,"unit":"px"}}},"custom_css":null}]}},` +
		`"elements":[{"id":"he4d1n9","elType":"widget","widgetType":"e-heading","isInner":false,"settings":{"classes":{"$$type":"classes","value":["e-he4d1n9-bbbbbbb","g-1a2b3c4"]},"title":{"$$type":"escaped-html","value":"Atomic <strong>Go</strong>"},"tag":{"$$type":"string","value":"h1"}},` +
		`"styles":{"e-he4d1n9-bbbbbbb":{"id":"e-he4d1n9-bbbbbbb","label":"local","type":"class","variants":[{"meta":{"breakpoint":"desktop","state":null},"props":{"color":{"$$type":"global-color-variable","value":"` + varID + `"},"font-size":{"$$type":"size","value":{"size":48,"unit":"px"}}},"custom_css":null}]}},"elements":[]}]}]`
	ajax := func(actions string) map[string]any {
		t.Helper()
		resp, err := c.PostForm(base+"/wp-admin/admin-ajax", url.Values{"action": {"elementor_ajax"}, "_nonce": {nonce[1]},
			"editor_post_id": {pageID}, "initial_document_id": {pageID}, "actions": {actions}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("ajax: %v %.200s", err, raw)
		}
		return out["data"].(map[string]any)["responses"].(map[string]any)
	}
	res := ajax(`{"s":{"action":"save_builder","data":{"status":"publish","elements":` + tree + `,"settings":{"post_title":"Atomic Page","post_status":"publish"}}},` +
		`"r":{"action":"render_atomic_element","data":{"data":{"id":"he4d1n9","elType":"widget","widgetType":"e-heading","settings":{"title":{"$$type":"escaped-html","value":"Live"}},"elements":[]}}}}`)
	if s := res["s"].(map[string]any); s["success"] != true {
		t.Fatalf("save_builder: %v", s)
	}
	r := res["r"].(map[string]any)
	if html, _ := r["data"].(map[string]any)["render"].(string); r["success"] != true || html != "\t\t\t<h2 data-interaction-id=\"he4d1n9\" class=\"e-heading-base e-default-h2\" data-e-type=\"widget\" data-id=\"he4d1n9\">Live</h2>\n\t\t" {
		t.Errorf("render_atomic_element: %v", r)
	}

	// The front end: atomic markup, the stylesheets and their CSS.
	_, body, _ = getPage(t, c, base+"/?page_id="+pageID)
	for _, want := range []string{
		`<div class="elementor-element elementor-element-d1v0b10 e-con e-atomic-element e-div-block-base e-default-div e-d1v0b10-aaaaaaa " data-id="d1v0b10" data-element_type="e-div-block"`,
		`<h1 data-interaction-id="he4d1n9" class="e-he4d1n9-bbbbbbb brand-card e-heading-base e-default-h1" data-e-type="widget" data-id="he4d1n9">Atomic <strong>Go</strong>`,
		"id='base-desktop-css'", "id='global-" + pageID + "-frontend-desktop-css'", "id='local-" + pageID + "-frontend-desktop-css'",
		"id='local-" + pageID + "-frontend-tablet-css'", "media='(max-width:1024px)'", "id='global-" + pageID + "-frontend-mobile-css'",
		`id="elementor-frontend-inline-css"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("front end misses %q", want)
		}
	}
	css := func(name string) string {
		t.Helper()
		code, b, _ := get(t, c, base+"/wp-content/uploads/elementor/css/"+name)
		if code != http.StatusOK {
			t.Errorf("%s: %d", name, code)
		}
		return b
	}
	if got, want := css("local-"+pageID+"-frontend-desktop.css"), `.elementor .e-he4d1n9-bbbbbbb{font-size:48px;color:var(--brand);}.elementor .e-d1v0b10-aaaaaaa{padding-block-start:40px;padding-block-end:40px;}`; got != want {
		t.Errorf("local css:\n got %s\nwant %s", got, want)
	}
	if got, want := css("global-"+pageID+"-frontend-desktop.css"), `.elementor .brand-card{padding:16px;background-color:#fff7f0;}.elementor .brand-card:hover,.elementor .brand-card:focus-visible{background-color:#ffe0cc;}`; got != want {
		t.Errorf("global css:\n got %s\nwant %s", got, want)
	}
	if got := css("base-desktop.css"); !strings.Contains(got, ".elementor .e-div-block-base{min-width:30px;padding:10px;display:block;}") {
		t.Errorf("base css: %.200s", got)
	}
	// The kit stylesheet defines the variables.
	m := regexp.MustCompile(`id='elementor-post-(\d+)-css' href='([^']+)'`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no kit stylesheet")
	}
	u, _ := url.Parse(m[2])
	if _, kit, _ := get(t, c, base+u.Path); !strings.Contains(kit, ":root { --brand:#ff5500; }") {
		t.Errorf("kit css lacks the variables: %.300s", kit)
	}
}
