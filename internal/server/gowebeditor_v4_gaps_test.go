package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Golementor v4 gaps: the Pro editor packages and cache busting in the shell, global-class style
// validation, the design-system sync stylesheet, display conditions on atomic elements, atomic
// form submissions through the forms back end, component element validation and the editor
// loading a newer autosave.
func TestGoWebEditorV4Gaps(t *testing.T) {
	ts, c := testServer(t)
	base := ts.URL
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Atomic Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"owner@example.test"},
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

	// --- editor shell ---
	_, body, _ := get(t, c, base+"/wp-admin/post?post="+pageID+"&action=elementor")
	for _, want := range []string{"pro/js/packages/editor-components-extended/editor-components-extended.min.js?ver=4.3.5-",
		"pro/js/packages/editor-controls-extended/editor-controls-extended.min.js", "pro/js/atomic-form-editor.min.js",
		"js/packages/editor-canvas/editor-canvas.min.js?ver=4.3.5-", "owner@example.test"} {
		if !strings.Contains(body, want) {
			t.Errorf("editor shell misses %q", want)
		}
	}
	for _, bad := range []string{"editor-canvas.min.js?ver=4.3.5\"", "lab@example.test", "cdn.jsdelivr.net"} {
		if strings.Contains(body, bad) {
			t.Errorf("editor shell contains %q", bad)
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
		req, _ := http.NewRequest(method, base+"/wp-json/"+path, r)
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
	if code, b := call(http.MethodGet, "elementor-pro/v1/license/tier-features", ""); code != http.StatusOK || !strings.Contains(b, `"form"`) || !strings.Contains(b, `"atomic-components"`) {
		t.Errorf("tier features: %d %s", code, b)
	}

	// --- global classes: upstream's style validation ---
	put := func(item string) (int, string) {
		return call(http.MethodPut, "elementor/v1/global-classes?context=frontend",
			`{"changes":{"added":["g-curl002"],"deleted":[],"modified":[],"order":true},"items":{"g-curl002":`+item+`},"order":["g-curl002"]}`)
	}
	for _, tc := range []struct{ item, msg string }{
		{`{"id":"g-curl002","type":"class","label":"Bad Label With Spaces!!","variants":[{"meta":{"breakpoint":null,"state":null},"props":{},"custom_css":null}]}`,
			"Invalid items: g-curl002.label: class_name_contains_spaces, g-curl002.meta.breakpoint: missing_or_invalid_value"},
		{`{"id":"g-curl002","type":"class","label":"ok-class","variants":[{"meta":{"breakpoint":"desktop","state":null},"props":{"font-size":{"$$type":"size","value":{"size":"big","unit":"px"}}},"custom_css":null}]}`,
			"Invalid items: g-curl002.variants[0].font-size: invalid_value"},
	} {
		code, b := put(tc.item)
		var e struct {
			Code, Message string
		}
		_ = json.Unmarshal([]byte(b), &e)
		if code != http.StatusBadRequest || e.Code != "invalid_items" || e.Message != tc.msg {
			t.Errorf("class validation: %d %s", code, b)
		}
	}

	// --- design-system sync ---
	if code, _ := call(http.MethodPost, "elementor/v1/design-system-sync/stylesheet", ""); code != http.StatusNoContent {
		t.Errorf("stylesheet with nothing synced: %d", code)
	}
	if code, b := put(`{"id":"g-curl002","type":"class","label":"heading-xl","sync_to_v3":true,"variants":[` +
		`{"meta":{"breakpoint":"desktop","state":null},"props":{"font-size":{"$$type":"size","value":{"size":48,"unit":"px"}},"font-weight":{"$$type":"string","value":"700"},"color":{"$$type":"color","value":"#111"}},"custom_css":null},` +
		`{"meta":{"breakpoint":"mobile","state":null},"props":{"font-size":{"$$type":"size","value":{"size":32,"unit":"px"}}},"custom_css":null}]}`); code != http.StatusNoContent {
		t.Fatalf("synced class: %d %s", code, b)
	}
	if code, b := call(http.MethodPost, "elementor/v1/variables/create", `{"type":"global-color-variable","label":"Brand","value":"#ff5500"}`); code != http.StatusCreated {
		t.Fatalf("variable: %d %s", code, b)
	} else {
		var v struct {
			Data struct {
				Variable  struct{ ID string } `json:"variable"`
				Watermark int                 `json:"watermark"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(b), &v)
		// "sync to v3" is set through the batch route (Variable entity's allowed fields).
		if code, b := call(http.MethodPost, "elementor/v1/variables/batch", `{"watermark":`+jsonNum(float64(v.Data.Watermark))+`,"operations":[{"type":"update","id":"`+
			v.Data.Variable.ID+`","variable":{"label":"Brand","value":"#ff5500","sync_to_v3":true}}]}`); code != http.StatusOK {
			t.Fatalf("variable sync: %d %s", code, b)
		}
	}
	code, b := call(http.MethodPost, "elementor/v1/design-system-sync/stylesheet", "")
	var dss struct {
		URL     string `json:"url"`
		Version int64  `json:"version"`
	}
	_ = json.Unmarshal([]byte(b), &dss)
	if code != http.StatusCreated || !strings.Contains(dss.URL, "/wp-content/uploads/elementor/design-system-sync/design-system-sync.css?ver=") || dss.Version == 0 {
		t.Fatalf("stylesheet: %d %s", code, b)
	}
	u, _ := url.Parse(dss.URL)
	_, css, _ := get(t, c, base+u.Path)
	if want := ":root { --e-global-color-v4-brand:var(--Brand); }\n:root { --e-global-typography-v4-heading-xl-font-size:48px; --e-global-typography-v4-heading-xl-font-weight:700; }" +
		"@media(max-width: 767px){:root { --e-global-typography-v4-heading-xl-font-size:32px; }}"; css != want {
		t.Errorf("design-system css:\n got %s\nwant %s", css, want)
	}

	// --- an atomic page: display conditions and a form ---
	formRaw, err := os.ReadFile("../builder/atomic/testdata/lab/form.json")
	if err != nil {
		t.Fatal(err)
	}
	var form []map[string]any
	_ = json.Unmarshal(formRaw, &form)
	form[0]["settings"] = map[string]any{"actions-after-submit": map[string]any{"$$type": "string-array",
		"value": []any{map[string]any{"$$type": "string", "value": "collect-submissions"}}}, "form-name": map[string]any{"$$type": "string", "value": "Contact"}}
	// the e-mail input is required
	for _, el := range form[0]["elements"].([]any) {
		m := el.(map[string]any)
		if m["id"] == "f2fa9e7" {
			m["settings"].(map[string]any)["required"] = map[string]any{"$$type": "boolean", "value": true}
		}
	}
	formJSON, _ := json.Marshal(form[0])
	hidden := `{"id":"h1dd3n1","elType":"widget","widgetType":"e-heading","settings":{"title":{"$$type":"escaped-html","value":"Members only"},` +
		`"display-conditions":{"$$type":"display-conditions","value":[{"$$type":"condition-group","value":[{"$$type":"string","value":"{\"condition\":\"login_status\",\"comparator\":\"is\",\"status\":\"logged_in\"}"}]}]}},"elements":[]}`
	tree := `[{"id":"d1v0b10","elType":"e-div-block","settings":{},"elements":[` + hidden + `]},` + string(formJSON) + `]`
	resp, err := c.PostForm(base+"/wp-admin/admin-ajax", url.Values{"action": {"elementor_ajax"}, "_nonce": {nonce[1]}, "editor_post_id": {pageID},
		"initial_document_id": {pageID}, "actions": {`{"s":{"action":"save_builder","data":{"status":"publish","elements":` + tree + `,"settings":{"post_status":"publish"}}}}`}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"success":true`) {
		t.Fatalf("save_builder: %.300s", raw)
	}
	_, body, _ = getPage(t, c, base+"/?page_id="+pageID)
	if !strings.Contains(body, "Members only") {
		t.Error("logged-in visitor does not see the conditional heading")
	}
	anon := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, base+"/?page_id="+pageID, nil)
	aresp, err := anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := io.ReadAll(aresp.Body)
	aresp.Body.Close()
	abody := string(ab)
	if strings.Contains(abody, "Members only") || aresp.Header.Get("X-WPGo-Cache") == "hit" {
		t.Error("anonymous visitor sees the conditional heading")
	}
	for _, want := range []string{`toolname="contact_a5901c6"`, `"atomicFormsSendForm":"`} {
		if !strings.Contains(abody, want) {
			t.Errorf("front end misses %q", want)
		}
	}
	fnonce := regexp.MustCompile(`"atomicFormsSendForm":"([^"]+)"`).FindStringSubmatch(abody)
	if fnonce == nil {
		t.Fatal("no form nonce")
	}
	send := func(values url.Values) map[string]any {
		t.Helper()
		values.Set("action", "elementor_pro_atomic_forms_send_form")
		values.Set("post_id", pageID)
		values.Set("form_id", "a5901c6")
		values.Set("form_name", "Contact")
		resp, err := anon.PostForm(base+"/wp-admin/admin-ajax", values)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if json.Unmarshal(b, &out) != nil {
			t.Fatalf("form reply: %d %.300s", resp.StatusCode, b)
		}
		return out
	}
	fields := func(email string) url.Values {
		return url.Values{"_nonce": {fnonce[1]},
			"form_fields[0][id]": {"2ba811f"}, "form_fields[0][type]": {"text"}, "form_fields[0][label]": {"First name"}, "form_fields[0][value]": {"Ada"},
			"form_fields[1][id]": {"f2fa9e7"}, "form_fields[1][type]": {"email"}, "form_fields[1][label]": {"Email"}, "form_fields[1][value]": {email},
			"form_fields[2][id]": {"bogus99"}, "form_fields[2][type]": {"text"}, "form_fields[2][label]": {"Injected"}, "form_fields[2][value]": {"x"}}
	}
	bad := fields("ada@example.test")
	bad.Set("_nonce", "nope")
	if out := send(bad); out["success"] != false {
		t.Errorf("bad nonce accepted: %v", out)
	}
	if out := send(fields("")); out["success"] != false {
		t.Errorf("missing required e-mail accepted: %v", out)
	}
	if out := send(fields("ada@example.test")); out["success"] != true {
		t.Errorf("submission: %v", out)
	}
	var n int
	var fj string
	if err := getPool(t).QueryRow(context.Background(), `SELECT count(*), max(fields::text) FROM gwe_form_submissions WHERE element_id = 'a5901c6' AND form_name = 'Contact'`).Scan(&n, &fj); err != nil || n != 1 ||
		!strings.Contains(fj, `"Ada"`) || !strings.Contains(fj, `ada@example.test`) || strings.Contains(fj, "Injected") {
		t.Errorf("stored submissions: %d %v %s", n, err, fj)
	}

	// --- components: element settings are validated like a component document save ---
	code, b = call(http.MethodPost, "elementor/v1/components", `{"status":"draft","items":[{"uid":"u-bad","title":"Broken","elements":[`+
		`{"id":"c1","elType":"e-div-block","settings":{"tag":{"$$type":"string","value":42}},"elements":[]}]}]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(b, `"code":"settings_validation_failed"`) || !strings.Contains(b, `Settings validation failed. tag: invalid_value`) {
		t.Errorf("component settings validation: %d %s", code, b)
	}

	// --- the editor loads a newer autosave of a published page ---
	resp, err = c.PostForm(base+"/wp-admin/admin-ajax", url.Values{"action": {"elementor_ajax"}, "_nonce": {nonce[1]}, "editor_post_id": {pageID},
		"initial_document_id": {pageID}, "actions": {`{"a":{"action":"save_builder","data":{"status":"autosave","elements":[{"id":"aut0sav","elType":"e-div-block","settings":{},"elements":[]}],"settings":{}}}}`}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, body, _ = get(t, c, base+"/wp-admin/post?post="+pageID+"&action=elementor")
	if !strings.Contains(body, `"id":"aut0sav"`) {
		t.Error("the editor does not open the newer autosave")
	}
}
