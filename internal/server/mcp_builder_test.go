package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// mcpRPC sends one JSON-RPC request to the MCP endpoint with an Application Password.
func mcpRPC(t *testing.T, base, pass, method string, params map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", base+"/wp-json/pursae/v1/mcp", bytes.NewReader(b))
	req.SetBasicAuth("admin", pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func mcpStructured(t *testing.T, tool string, res map[string]any) map[string]any {
	t.Helper()
	if res["isError"] == true {
		t.Fatalf("%s failed: %s", tool, mcpText(res))
	}
	m, _ := res["structuredContent"].(map[string]any)
	if m == nil {
		_ = json.Unmarshal([]byte(mcpText(res)), &m)
	}
	return m
}

func fetchPage(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url, resp.StatusCode)
	}
	return string(b)
}

// TestMCPWebBuilderSiteFlow builds a site through MCP the way an agent does: the builder is off and
// the tools say how to turn it on; a page built from catalog designs is published by the server
// renderer and served by the canvas; an HTML mock-up becomes editable free-form sections; section
// operations change the page; a menu, a forum and the prompts work end to end.
func TestMCPWebBuilderSiteFlow(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	pass, call := mcpTestAgent(t, base, c, "Site agent", nil)

	// Builder off: the tools are listed and say what to do.
	res := call("web_builder_build_page", map[string]any{"title": "Home", "sections": []any{map[string]any{"kind": "hero"}}})
	if res["isError"] != true || !strings.Contains(mcpText(res), "activate_plugin") || !strings.Contains(mcpText(res), "golementor") {
		t.Fatalf("builder off: %s", mcpText(res))
	}
	ov := mcpStructured(t, "site_overview", call("site_overview", nil))
	if wb, _ := ov["web_builder"].(map[string]any); wb["active"] != false || wb["server_render"] != true {
		t.Fatalf("overview web_builder: %v", ov["web_builder"])
	}
	if r := call("activate_plugin", map[string]any{"slug": "golementor"}); r["isError"] == true {
		t.Fatalf("activate builder: %s", mcpText(r))
	}

	// Catalog.
	cat := mcpStructured(t, "web_builder_catalog", call("web_builder_catalog", nil))
	if !strings.Contains(mcpJ(cat), `"kind":"hero"`) || cat["guide"] == nil {
		t.Fatalf("catalog: %.300s", mcpJ(cat))
	}
	hero := mcpStructured(t, "web_builder_catalog", call("web_builder_catalog", map[string]any{"kind": "hero"}))
	designs, _ := hero["designs"].([]any)
	if len(designs) < 2 {
		t.Fatalf("hero designs: %v", hero["designs"])
	}
	design := strAny(designs[1].(map[string]any)["id"])

	// Build a page from specs and make it the front page.
	page := mcpStructured(t, "web_builder_build_page", call("web_builder_build_page", map[string]any{
		"title": "Northwind Dental",
		"site":  map[string]any{"brand": "#0e7c66", "fontPair": "modern", "corners": "round"},
		"sections": []any{
			map[string]any{"kind": "hero", "design": design, "content": map[string]any{"title": "Calm, modern dentistry", "body": "Same-week appointments in the city centre.", "primary": "Book a visit", "eyebrow": "Northwind Dental"}},
			map[string]any{"kind": "features", "content": map[string]any{"title": "What we do", "eyebrow": "Care", "body": "Everything under one roof."},
				"items": []any{map[string]any{"title": "Check-ups", "text": "Gentle and thorough.", "icon": "shield"}, map[string]any{"title": "Whitening", "text": "Safe and quick.", "icon": "zap"}}},
			map[string]any{"kind": "faq", "content": map[string]any{"title": "Questions", "eyebrow": "FAQ"}, "items": []any{map[string]any{"title": "Do you take walk-ins?", "text": "Yes, <b>mornings</b> only."}}},
		},
		"front_page": true,
	}))
	id := int64(page["post_id"].(float64))
	if page["status"] != "publish" || page["front_page"] != true || strAny(page["url"]) == "" {
		t.Fatalf("build: %v", page)
	}
	pageHTML := fetchPage(t, strAny(page["url"]))
	for _, want := range []string{"Calm, modern dentistry", "Check-ups", "Do you take walk-ins?", "<b>mornings</b>", "data-wb-root"} {
		if !strings.Contains(pageHTML, want) {
			t.Errorf("published page lacks %q", want)
		}
	}
	if front := fetchPage(t, base+"/"); !strings.Contains(front, "Calm, modern dentistry") {
		t.Error("front page is not the built page")
	}

	// Read it back and edit it.
	got := mcpStructured(t, "web_builder_get_page", call("web_builder_get_page", map[string]any{"post_id": id}))
	secs, _ := got["sections"].([]any)
	if len(secs) != 3 {
		t.Fatalf("get_page sections: %d", len(secs))
	}
	faqID := strAny(secs[2].(map[string]any)["id"])
	heroID := strAny(secs[0].(map[string]any)["id"])
	edit := mcpStructured(t, "web_builder_edit_page", call("web_builder_edit_page", map[string]any{"post_id": id, "ops": []any{
		map[string]any{"op": "update", "section_id": heroID, "content": map[string]any{"title": "Gentle dentistry, done right"}},
		map[string]any{"op": "add", "after": heroID, "section": map[string]any{"kind": "cta", "content": map[string]any{"title": "New patients welcome", "body": "Register in two minutes.", "primary": "Register", "secondary": "Call us"}}},
		map[string]any{"op": "move", "section_id": faqID, "index": 0},
		map[string]any{"op": "duplicate", "section_id": faqID},
	}}))
	if n := len(edit["sections"].([]any)); n != 5 {
		t.Fatalf("after edit: %d sections", n)
	}
	pageHTML = fetchPage(t, strAny(edit["url"]))
	if !strings.Contains(pageHTML, "Gentle dentistry, done right") || !strings.Contains(pageHTML, "New patients welcome") || strings.Contains(pageHTML, "Calm, modern dentistry") {
		t.Error("edit did not reach the published page")
	}
	if r := call("web_builder_edit_page", map[string]any{"post_id": id, "ops": []any{map[string]any{"op": "remove", "section_id": "nope"}}}); r["isError"] != true {
		t.Error("removing an unknown section must fail")
	}
	if r := call("web_builder_build_page", map[string]any{"title": "Bad", "sections": []any{map[string]any{"kind": "hero", "design": "no-such-design"}}}); r["isError"] != true || !strings.Contains(mcpText(r), "designs:") {
		t.Errorf("bad design: %s", mcpText(r))
	}

	// Draft, then publish.
	dr := mcpStructured(t, "web_builder_edit_page", call("web_builder_edit_page", map[string]any{"post_id": id, "status": "draft",
		"ops": []any{map[string]any{"op": "update", "section_id": heroID, "content": map[string]any{"title": "Draft headline"}}}}))
	if dr["saved"] != "draft" || strings.Contains(fetchPage(t, base+"/?page_id="+itoa64(id)), "Draft headline") {
		t.Fatalf("draft leaked to the live page: %v", dr)
	}
	pub := mcpStructured(t, "web_builder_publish", call("web_builder_publish", map[string]any{"post_id": id}))
	if !strings.Contains(fetchPage(t, strAny(pub["url"])), "Draft headline") {
		t.Error("publish did not take the draft")
	}

	// HTML import.
	imp := mcpStructured(t, "web_builder_import_html", call("web_builder_import_html", map[string]any{"title": "Landing", "method": "quick", "html": `<html><head><style>
		.hero{background:#101828;color:#fff;padding:80px 40px;text-align:center}.cards{display:grid;grid-template-columns:repeat(2,1fr);gap:24px}
		.btn{background:#f04438;color:#fff;padding:12px 20px;border-radius:8px}</style></head><body>
		<header><strong>Acme</strong><nav><a href="/about">About</a> <a href="/contact">Contact</a></nav></header>
		<section class="hero"><h1>Ship faster</h1><p>With <b>less</b> work.</p><a class="btn" href="/start">Start now</a></section>
		<section class="cards"><div><h3>Fast</h3><p>Go inside.</p></div><div><h3>Safe</h3><p>Sandboxed.</p></div></section>
		<footer><p>© Acme</p></footer><script>alert(1)</script></body></html>`}))
	if len(imp["sections"].([]any)) != 4 {
		t.Fatalf("import sections: %v", imp["sections"])
	}
	pageHTML = fetchPage(t, strAny(imp["url"]))
	for _, want := range []string{"Ship faster", "Start now", "Sandboxed.", "© Acme"} {
		if !strings.Contains(pageHTML, want) {
			t.Errorf("imported page lacks %q", want)
		}
	}
	if strings.Contains(pageHTML, "alert(1)") {
		t.Error("imported page kept a script")
	}
	impPage := mcpStructured(t, "web_builder_get_page", call("web_builder_get_page", map[string]any{"post_id": imp["post_id"], "full": true}))
	if !strings.Contains(mcpJ(impPage), `"node":"button"`) || !strings.Contains(mcpJ(impPage), `"layout":"canvas"`) {
		t.Errorf("imported doc: %.400s", mcpJ(impPage))
	}

	// Menu in one call (twice: the second call refills, no copy).
	for range 2 {
		m := mcpStructured(t, "menu_build", call("menu_build", map[string]any{"name": "Main", "items": []any{
			map[string]any{"title": "Home", "page_id": id},
			map[string]any{"title": "More", "url": "/more", "children": []any{map[string]any{"title": "Landing", "page_id": imp["post_id"]}}},
		}}))
		if len(m["items"].([]any)) != 3 {
			t.Fatalf("menu items: %v", m["items"])
		}
	}
	var menus []any
	_ = json.Unmarshal([]byte(mcpText(call("list_menus", nil))), &menus)
	if len(menus) != 1 {
		t.Errorf("menu_build made %d menus", len(menus))
	}

	// Forum: off → told to activate; on → structure and a topic.
	if r := call("forum_list", nil); r["isError"] != true || !strings.Contains(mcpText(r), forumSlug) {
		t.Errorf("forum off: %s", mcpText(r))
	}
	if r := call("activate_plugin", map[string]any{"slug": forumSlug}); r["isError"] == true {
		t.Fatalf("activate forum: %s", mcpText(r))
	}
	cat1 := mcpStructured(t, "forum_create", call("forum_create", map[string]any{"title": "Community", "kind": "category"}))
	f1 := mcpStructured(t, "forum_create", call("forum_create", map[string]any{"title": "Introductions", "parent": cat1["id"]}))
	top := mcpStructured(t, "forum_create_topic", call("forum_create_topic", map[string]any{"forum_id": f1["id"], "title": "Welcome!", "content": "Say hello."}))
	if strAny(top["url"]) == "" || !strings.Contains(fetchPage(t, strAny(top["url"])), "Say hello.") {
		t.Errorf("topic: %v", top)
	}
	if r := call("update_options", map[string]any{"group": "forum", "settings": map[string]any{"title": "Northwind Community"}}); r["isError"] == true {
		t.Errorf("forum settings: %s", mcpText(r))
	}

	// Prompts.
	pl := mcpRPC(t, base, pass, "prompts/list", nil)
	if !strings.Contains(mcpJ(pl), "build_site") {
		t.Fatalf("prompts/list: %v", pl)
	}
	pg := mcpRPC(t, base, pass, "prompts/get", map[string]any{"name": "build_site", "arguments": map[string]any{"kind": "forum", "brief": "A woodworking community"}})
	if txt := mcpJ(pg); !strings.Contains(txt, "forum_create") || !strings.Contains(txt, "woodworking") {
		t.Errorf("prompts/get: %.300s", txt)
	}
	if e := mcpRPC(t, base, pass, "prompts/get", map[string]any{"name": "build_site"}); e["error"] == nil {
		t.Error("prompts/get without the required argument must fail")
	}
	init := mcpRPC(t, base, pass, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if !strings.Contains(mcpJ(init), "site_overview") {
		t.Error("initialize instructions do not mention site_overview")
	}
}

// A content-only password cannot reach the forum settings or the commerce tools.
func TestMCPSiteToolsScopes(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	pass, _ := mcpTestAgent(t, ts.URL, c, "Content agent", []string{"content"})
	list := mcpJ(mcpRPC(t, ts.URL, pass, "tools/list", nil))
	for _, want := range []string{"web_builder_build_page"} {
		if !strings.Contains(list, `"`+want+`"`) {
			t.Errorf("content scope should list %s", want)
		}
	}
	// forum_create needs manage_options (rest_pursae_forum.go), which the content scope does not
	// grant: its REST call is refused, so it is not listed either.
	for _, hidden := range []string{"update_options", "commerce_create_coupon", "activate_plugin", "forum_create"} {
		if strings.Contains(list, `"`+hidden+`"`) {
			t.Errorf("content scope lists %s", hidden)
		}
	}
}

func mcpJ(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A theme kit imported through MCP is published by the server renderer: templates live, pages served.
func TestMCPImportThemeKit(t *testing.T) {
	ts, c := testServer(t)
	installAndLogin(t, ts, c)
	_, call := mcpTestAgent(t, ts.URL, c, "Kit agent", nil)
	var kits []map[string]any
	raw := mcpText(call("web_builder_list_theme_kits", nil))
	if err := json.Unmarshal([]byte(raw), &kits); err != nil || len(kits) == 0 {
		var wrap map[string]any
		_ = json.Unmarshal([]byte(raw), &wrap)
		if l, ok := wrap["themes"].([]any); ok {
			for _, x := range l {
				kits = append(kits, x.(map[string]any))
			}
		}
	}
	if len(kits) == 0 {
		t.Skip("no theme kits in library/themes")
	}
	slug := strAny(kits[0]["slug"])
	out := mcpStructured(t, "web_builder_import_theme_kit", call("web_builder_import_theme_kit", map[string]any{"slug": slug}))
	if out["problems"] != nil {
		t.Fatalf("problems: %v", out["problems"])
	}
	if len(out["published_templates"].([]any)) == 0 {
		t.Fatalf("no template published: %v", out)
	}
	// testServer turns the builder on without the plugin activation's theme switch; the templates
	// render under Hello Pursae.
	if r := call("activate_theme", map[string]any{"slug": "hello-pursae"}); r["isError"] == true {
		t.Fatalf("activate Hello Pursae: %s", mcpText(r))
	}
	home := fetchPage(t, ts.URL+"/")
	if !strings.Contains(home, "data-wb-root") && !strings.Contains(home, "wb-page") {
		t.Errorf("front page does not use the kit's templates: %v\n%.1500s", out, home)
	}
}

// Tools › AI Access creates a full-access Application Password with the ready commands (shown once),
// the password works on the MCP endpoint, and the skill downloads as a zip.
func TestAIAccessConnectAndSkill(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	_, page, _ := get(t, c, ts.URL+"/wp-admin/ai-access")
	if !strings.Contains(page, "Create password and command") || !strings.Contains(page, "claude mcp add --transport http pursae "+ts.URL) {
		t.Fatal("AI Access lacks the connect form")
	}
	code, body, _ := post(t, c, ts.URL+"/wp-admin/ai-access", url.Values{"_wpnonce": {formNonce(t, page)}, "do": {"connect"}, "app_name": {"Claude Desktop"}})
	if code != 200 || !strings.Contains(body, `--header &#34;Authorization: Basic `) && !strings.Contains(body, `--header "Authorization: Basic `) {
		t.Fatalf("connect: %d %.300s", code, body)
	}
	m := regexp.MustCompile(`Application Password: ([a-zA-Z0-9 ]{24,})`).FindStringSubmatch(html.UnescapeString(body))
	if m == nil {
		t.Fatal("no password in the chat block")
	}
	pass := strings.TrimSpace(m[1])
	init := mcpRPC(t, ts.URL, pass, "tools/list", nil)
	if !strings.Contains(mcpJ(init), `"name":"activate_plugin"`) {
		t.Error("the new password is not full access")
	}
	// The same name again is refused, not a second password.
	if _, body, _ := post(t, c, ts.URL+"/wp-admin/ai-access", url.Values{"_wpnonce": {formNonce(t, page)}, "do": {"connect"}, "app_name": {"Claude Desktop"}}); !strings.Contains(body, "unique") {
		t.Error("a duplicate name must be refused")
	}
	resp, err := c.Get(ts.URL + "/wp-admin/ai-access?download=skill")
	if err != nil {
		t.Fatal(err)
	}
	zb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil {
		t.Fatalf("skill zip: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"pursae-site-builder/SKILL.md", "pursae-site-builder/scripts/pursae_mcp.py", "pursae-site-builder/references/playbook-store.md"} {
		if !names[want] {
			t.Errorf("zip lacks %s", want)
		}
	}
	res := mcpRPC(t, ts.URL, pass, "resources/read", map[string]any{"uri": "pursae-skill://pursae-site-builder/SKILL.md"})
	if !strings.Contains(mcpJ(res), "pursae-site-builder") {
		t.Errorf("skill resource: %.200s", mcpJ(res))
	}
	if _, prof, _ := get(t, c, ts.URL+"/wp-admin/profile"); !strings.Contains(prof, `href="/wp-admin/ai-access"`) {
		t.Error("profile does not point to AI Access")
	}
}

// Exact HTML import: the job page draws the design in sandboxed frames, the browser's measurements
// are posted back with the job token, and web_builder_import_finish publishes the page with its SEO.
func TestMCPImportHTMLExact(t *testing.T) {
	ts, c := testServer(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	_, call := mcpTestAgent(t, base, c, "Import agent", nil)
	if r := call("activate_plugin", map[string]any{"slug": seogoSlug}); r["isError"] == true {
		t.Fatalf("activate SEO: %s", mcpText(r))
	}
	src, err := os.ReadFile("../webbuilder/testdata/import/corporate.html")
	if err != nil {
		t.Fatal(err)
	}
	job := mcpStructured(t, "web_builder_import_html", call("web_builder_import_html", map[string]any{"title": "Aksoy", "method": "exact", "html": string(src) + "</script><script>alert(1)</script>"}))
	importURL, token := strAny(job["import_url"]), strAny(job["job"])
	if job["status"] != "waiting_for_browser" || !strings.Contains(importURL, "/web-builder-import/"+token) {
		t.Fatalf("job: %v", job)
	}
	resp, err := http.Get(importURL)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(string(page), `setAttribute('sandbox', 'allow-scripts')`) || !strings.Contains(csp, "frame-ancestors 'none'") ||
		resp.Header.Get("X-Robots-Tag") == "" || strings.Contains(string(page), "</script><script>alert(1)") {
		t.Fatalf("import page: %d csp=%q", resp.StatusCode, csp)
	}
	// Not finished yet: the tool says so after the wait.
	if r := mcpStructured(t, "web_builder_import_finish", call("web_builder_import_finish", map[string]any{"job": token, "wait": 1})); r["status"] != "waiting_for_browser" {
		t.Fatalf("early finish: %v", r)
	}
	fx, _ := os.ReadFile("../webbuilder/testdata/import/corporate.extract.json")
	if r, err := http.Post(importURL, "application/json", bytes.NewReader(fx)); err != nil || r.StatusCode != 200 {
		t.Fatalf("post extraction: %v %v", err, r)
	}
	if r, _ := http.Post(base+"/web-builder-import/"+strings.Repeat("0", 48), "application/json", bytes.NewReader(fx)); r.StatusCode != 404 {
		t.Errorf("unknown token: %d", r.StatusCode)
	}
	// Another user cannot finish someone else's import.
	_, other := mcpTestAgentAs(t, base, c, "editor2")
	if r := other("web_builder_import_finish", map[string]any{"job": token, "wait": 1}); r["isError"] != true {
		t.Error("another user finished the import")
	}
	out := mcpStructured(t, "web_builder_import_finish", call("web_builder_import_finish", map[string]any{"job": token}))
	if out["status"] != "publish" || !strings.Contains(strAny(out["method"]), "exact") {
		t.Fatalf("finish: %v", out)
	}
	html := fetchPage(t, strAny(out["url"]))
	for _, want := range []string{"<h1", "İşinizi koruyan", "Çalışma Alanlarımız", "color: rgb(201, 162, 75)", `content="Aksoy Hukuk Bürosu: ticaret`} {
		if !strings.Contains(html, want) {
			t.Errorf("published page lacks %q", want)
		}
	}
	if seo, _ := out["seo"].(map[string]any); seo == nil || !strings.Contains(strAny(seo["description"]), "Aksoy Hukuk") {
		t.Errorf("SEO fields: %v", out["seo"])
	}
	// One-time: the job is gone after finishing.
	if r := call("web_builder_import_finish", map[string]any{"job": token, "wait": 1}); r["isError"] != true {
		t.Error("a finished job can be finished again")
	}
	if r, _ := http.Get(importURL); r.StatusCode != 404 {
		t.Errorf("finished job page: %d", r.StatusCode)
	}
	// method quick still works without a browser.
	q := mcpStructured(t, "web_builder_import_html", call("web_builder_import_html", map[string]any{"title": "Quick", "method": "quick", "html": string(src)}))
	if q["status"] != "publish" {
		t.Errorf("quick import: %v", q)
	}
}

// mcpTestAgentAs creates an administrator login with an Application Password (made by the signed-in
// admin of c) and returns a tools/call helper for it.
func mcpTestAgentAs(t *testing.T, base string, c *http.Client, login string) (string, func(string, map[string]any) map[string]any) {
	t.Helper()
	return mcpTestAgentRole(t, base, c, login, "administrator")
}

// mcpTestAgentRole is mcpTestAgentAs for a user of the given role.
func mcpTestAgentRole(t *testing.T, base string, c *http.Client, login, role string) (string, func(string, map[string]any) map[string]any) {
	t.Helper()
	_, nonce, _ := get(t, c, base+"/wp-admin/admin-ajax?action=rest-nonce")
	do := func(method, path string, body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
		req.Header.Set("X-WP-Nonce", nonce)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	u := do("POST", "/wp-json/wp/v2/users", map[string]any{"username": login, "email": login + "@example.test", "password": "pw-" + login + "-123456", "roles": []string{role}})
	id := int64(0)
	if f, ok := u["id"].(float64); ok {
		id = int64(f)
	}
	if id == 0 {
		t.Fatalf("create user: %v", u)
	}
	ap := do("POST", "/wp-json/wp/v2/users/"+itoa64(id)+"/application-passwords", map[string]any{"name": "agent", "full_access": true})
	pass := strAny(ap["password"])
	if pass == "" {
		t.Fatalf("app password: %v", ap)
	}
	return pass, func(tool string, args map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
		req, _ := http.NewRequest("POST", base+"/wp-json/pursae/v1/mcp", bytes.NewReader(b))
		req.SetBasicAuth(login, pass)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		res, _ := out["result"].(map[string]any)
		if res == nil {
			t.Fatalf("%s: no result: %s", tool, raw)
		}
		return res
	}
}

// A partial spec never publishes the catalog's sample copy (Pursae's own demo text): text fields
// left out are emptied, sample items dropped, and the answer says which (human test B1).
func TestMCPBuildPageDropsSampleText(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	_, call := mcpTestAgent(t, ts.URL, c, "Site agent", nil)
	if r := call("activate_plugin", map[string]any{"slug": "golementor"}); r["isError"] == true {
		t.Fatalf("activate builder: %s", mcpText(r))
	}
	res := call("web_builder_build_page", map[string]any{"title": "Partial", "status": "publish", "sections": []any{
		map[string]any{"kind": "hero", "content": map[string]any{"title": "Hoş geldiniz İstanbul", "body": "Merhaba"}},
		map[string]any{"kind": "features", "content": map[string]any{"title": "Özellikler"}, "items": []any{map[string]any{"title": "Hızlı", "text": "Çok hızlı"}}},
	}})
	page := mcpStructured(t, "web_builder_build_page", res)
	if !strings.Contains(mcpJ(page), "emptied the sample text") || !strings.Contains(mcpJ(page), "eyebrow") {
		t.Errorf("no warning about the emptied fields: %v", page["warnings"])
	}
	html := fetchPage(t, strAny(page["url"]))
	for _, want := range []string{"Hoş geldiniz İstanbul", "Hızlı"} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, sample := range []string{"Early release", "One binary", "docker compose", "Keep your themes, plugins and dashboard", "PostgreSQL + JSONB"} {
		if strings.Contains(html, sample) {
			t.Errorf("published page shows sample text %q", sample)
		}
	}
}
