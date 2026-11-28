package server

import (
	"strings"
	"testing"
)

// A site template replaces a forum view (with the shared parts and the plugin's functions), is
// parse- and render-checked with a rollback, prints the whole main without forum.css when asked,
// needs unfiltered_html and edit_theme_options, and goes away with delete.
func TestForumSiteTemplates(t *testing.T) {
	ts, c := testServer(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	_, call := mcpTestAgent(t, base, c, "Forum design agent", nil)
	for _, slug := range []string{forumSlug, "golementor"} {
		if r := call("activate_plugin", map[string]any{"slug": slug}); r["isError"] == true {
			t.Fatalf("activate %s: %s", slug, mcpText(r))
		}
	}
	if r := call("activate_theme", map[string]any{"slug": wbHelloSlug}); r["isError"] == true { // the full-main path is Hello Pursae's
		t.Fatalf("activate Hello Pursae: %s", mcpText(r))
	}
	f := mcpStructured(t, "forum_create", call("forum_create", map[string]any{"title": "Lobby"}))
	if r := call("forum_create_topic", map[string]any{"forum_id": f["id"], "title": "First topic", "content": "Hello there."}); r["isError"] == true {
		t.Fatalf("topic: %s", mcpText(r))
	}
	page := func(path string) string {
		_, body, _ := get(t, c, base+path)
		return body
	}
	if body := page("/forums/"); !strings.Contains(body, `data-view="index"`) || !strings.Contains(body, "pursae-forum-css") {
		t.Fatalf("plugin index expected first")
	}

	// A template that does not parse, or fails while the view renders, is refused and rolled back.
	if r := call("forum_set_template", map[string]any{"view": "index", "source": "{{if}}"}); r["isError"] != true || !strings.Contains(mcpText(r), "does not parse") {
		t.Errorf("parse error accepted: %s", mcpText(r))
	}
	if r := call("forum_set_template", map[string]any{"view": "index", "source": "<p>{{.NoSuchField}}</p>"}); r["isError"] != true || !strings.Contains(mcpText(r), "rolled back") {
		t.Errorf("render error accepted: %s", mcpText(r))
	}
	if body := page("/forums/"); !strings.Contains(body, `data-view="index"`) {
		t.Error("the plugin's index is not back after the rollback")
	}
	if r := call("forum_set_template", map[string]any{"view": "parts", "source": "x", "main": true}); r["isError"] != true {
		t.Error("main accepted on the shared parts")
	}
	if r := call("forum_set_template", map[string]any{"view": "nope", "source": "x"}); r["isError"] != true {
		t.Error("unknown view accepted")
	}

	// Shared parts, plugin functions and the page data.
	parts := `{{define "t/row"}}<li class="row c-{{forum_cycle .ID "a" "b"}}"><a href="{{.URL}}">{{.Title}}</a></li>{{end}}`
	if r := call("forum_set_template", map[string]any{"view": "parts", "source": parts}); r["isError"] == true {
		t.Fatalf("parts: %s", mcpText(r))
	}
	index := `<main id="design-main" class="pf"><h1>Our board</h1><p class="n">{{.Totals.Topics}} topics</p><ul>{{range .Groups}}{{range .Forums}}{{template "t/row" .}}{{end}}{{end}}</ul></main>`
	if r := call("forum_set_template", map[string]any{"view": "index", "source": index, "main": true}); r["isError"] == true {
		t.Fatalf("index: %s", mcpText(r))
	}
	body := page("/forums/")
	for _, want := range []string{`<main id="design-main" class="pf">`, `<p class="n">1 topics</p>`, `>Lobby</a></li>`, `class="row c-`} {
		if !strings.Contains(body, want) {
			t.Errorf("site index lacks %q", want)
		}
	}
	if strings.Contains(body, "pursae-forum-css") || strings.Contains(body, `data-view="index"`) {
		t.Error("a main site template still loads forum.css or the plugin's markup")
	}
	if strings.Count(body, "<main") != 1 {
		t.Errorf("the theme wraps the design's main in its own: %d <main> elements", strings.Count(body, "<main"))
	}
	if strings.Contains(body, "hello-pursae-css") || strings.Contains(body, `class="page-header"`) {
		t.Error("Hello Pursae's stylesheet or page title is printed around a main site template")
	}
	// Other views keep the plugin's markup and stylesheet.
	if body := page("/forums/f/lobby/"); !strings.Contains(body, `data-view="forum"`) || !strings.Contains(body, "pursae-forum-css") {
		t.Error("the forum view lost the plugin's template")
	}
	list := mcpStructured(t, "forum_list_templates", call("forum_list_templates", nil))
	views, _ := list["views"].([]any)
	site := 0
	for _, v := range views {
		if m, _ := v.(map[string]any); m["site"] == true {
			site++
		}
	}
	if site != 2 {
		t.Errorf("site templates listed: %d, want 2 (%v)", site, list)
	}
	got := mcpStructured(t, "forum_get_template", call("forum_get_template", map[string]any{"view": "index"}))
	if got["source"] != index || got["main"] != true || !strings.Contains(strAny(got["default"]), `{{define "forum/index"}}`) {
		t.Errorf("get: %v", got)
	}

	// Only an administrator (unfiltered_html and edit_theme_options) writes them.
	// An author is not even offered the tool (tools/list follows the handler's capability).
	authorPass, _ := mcpTestAgentRole(t, base, c, "forumauthor", "author")
	if list := mcpJ(mcpRPC(t, base, authorPass, "tools/list", nil)); strings.Contains(list, `"forum_set_template"`) {
		t.Error("an author is offered forum_set_template")
	}

	if r := call("forum_delete_template", map[string]any{"view": "index"}); r["isError"] == true {
		t.Fatalf("delete: %s", mcpText(r))
	}
	if body := page("/forums/"); !strings.Contains(body, `data-view="index"`) || !strings.Contains(body, "pursae-forum-css") {
		t.Error("the plugin's index is not back after delete")
	}
}
