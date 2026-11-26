package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"wpgo/internal/plugins"
	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// Tests of the Web Builder hooks into the rest of the CMS (webbuilder_hooks.go): drift, WXR,
// Pursae Multi Language copies and Pursae Cache tags. The unit tests run without a database; the
// integration tests need PURSAE_TEST_ADMIN_DB / PURSAE_TEST_DB and reuse the REST test helpers.

// --- unit ------------------------------------------------------------------------------------------

func TestWebBuilderCopySanitizesSnapshot(t *testing.T) {
	const src, dst = int64(12), int64(99)
	in := `<div class="wb-page" data-wb-root="">` +
		`<p onclick="alert(document.cookie)">ok</p><script>alert(1)</script><a href="javascript:alert(2)">x</a>` +
		`<form data-wb-form method="post" action="https://ex.test/wp-json/web-builder/v2/forms/12/s1"></form></div>`
	out, err := wbSanitizeCopiedHTML(in, src, dst, true)
	if err != nil {
		t.Fatal(err)
	}
	low := strings.ToLower(out)
	if strings.Contains(low, "<script") || strings.Contains(out, "alert") || strings.Contains(out, "onclick") || strings.Contains(out, "javascript:") {
		t.Fatalf("dangerous markup kept: %s", out)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("safe text dropped: %s", out)
	}
	if strings.Contains(out, "/forms/12/") || !strings.Contains(out, "/web-builder/v2/forms/99/s1") {
		t.Fatalf("form action: %s", out)
	}
	plain, err := wbSanitizeCopiedHTML(`<div class="wb-page" data-wb-root=""><p>x</p></div>`, src, dst, false)
	if err != nil || !strings.Contains(plain, "<p>x</p>") {
		t.Fatalf("plain copy: %v %s", err, plain)
	}
}

func TestWebBuilderHooksRetargetForms(t *testing.T) {
	in := `<form data-wb-form method="post" action="https://ex.test/wp-json/web-builder/v2/forms/12/contact"></form>` +
		`<a href="https://ex.test/wp-json/web-builder/v2/forms/120/x">other post</a>`
	got := wbRetargetForms(in, 12, 99)
	if !strings.Contains(got, "/web-builder/v2/forms/99/contact") || strings.Contains(got, "/forms/12/") {
		t.Fatalf("form action not retargeted: %s", got)
	}
	if !strings.Contains(got, "/web-builder/v2/forms/120/x") {
		t.Fatalf("another post's id was changed: %s", got)
	}
	if wbRetargetForms("<p>no forms</p>", 1, 2) != "<p>no forms</p>" {
		t.Fatal("html without forms changed")
	}
}

func TestWebBuilderHooksURLReplacer(t *testing.T) {
	r := wbURLReplacer(map[string]string{
		"https://old.test/up/photo.jpg":         "https://new.test/up/a.jpg",
		"https://old.test/up/photo.jpg?size=lg": "https://new.test/up/a-large.jpg",
		"":                                      "never",
	})
	got := r.Replace(`{"src":"https://old.test/up/photo.jpg?size=lg","b":"https://old.test/up/photo.jpg"}`)
	if got != `{"src":"https://new.test/up/a-large.jpg","b":"https://new.test/up/a.jpg"}` {
		t.Fatalf("replace: %s", got)
	}
	// Image sizes follow the full-size file, as in the imported posts.content.
	if got := r.Replace(`"https://old.test/up/photo-1024x683.jpg"`); got != `"https://new.test/up/a-1024x683.jpg"` {
		t.Fatalf("sized variant: %s", got)
	}
	if wbURLReplacer(nil).Replace("x") != "x" {
		t.Fatal("empty replacer changed the input")
	}
}

func TestWebBuilderHooksImportPage(t *testing.T) {
	for in, want := range map[string]string{"": `{}`, "null": `{}`, "[1]": `{}`, "nope": `{}`, `{"title":"A"}`: `{"title":"A"}`} {
		if got := string(wbImportPage(in)); got != want {
			t.Errorf("%q → %s, want %s", in, got, want)
		}
	}
}

func TestWebBuilderHooksCacheTags(t *testing.T) {
	s := &Server{}
	built := &store.Post{ID: 5, Meta: map[string]any{webbuilder.PostMarkerMeta: "1"}}
	plain := &store.Post{ID: 6, Meta: map[string]any{}}
	if got := s.wbCacheTags(nil, &siteQuery{Kind: "page", Post: built}); !reflect.DeepEqual(got, []string{wbCacheTag}) {
		t.Errorf("builder page tags: %v", got)
	}
	for name, q := range map[string]*siteQuery{
		"plain page": {Kind: "page", Post: plain},
		"feed":       {Kind: "single", Post: built, Feed: "rss2"},
		"archive":    {Kind: "category"},
		"nil":        nil,
	} {
		if got := s.wbCacheTags(nil, q); len(got) != 0 {
			t.Errorf("%s: tags %v", name, got)
		}
	}
}

// --- integration -----------------------------------------------------------------------------------

// wbhPublish publishes a fixture doc to a new page through the REST API and returns the page.
func wbhPublish(t *testing.T, srv *Server, admin *wbrClient, title, slug, fixture string) *store.Post {
	t.Helper()
	p := wbrPost(t, srv, "page", title, "draft", 1)
	doc, render := wbrFixture(t, fixture)
	// The fixtures were rendered for post 42; the editor renders for the real post.
	if h, ok := render["html"].(string); ok {
		render["html"] = strings.ReplaceAll(h, "https://example.com/wp-json/web-builder/v2/forms/42/", admin.base+"/wp-json/web-builder/v2/forms/"+itoa64(p.ID)+"/")
	}
	page := map[string]any{"title": title, "slug": slug, "description": "", "indexable": true}
	if code, v, _ := admin.do(http.MethodPost, wbrDocPath(p.ID, "/publish"), wbrPublishBody(doc, render, 0, "publish", page), nil); code != http.StatusOK {
		t.Fatalf("publish %s: %d %v", fixture, code, v)
	}
	got, err := srv.store.GetPost(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func wbhDoc(t *testing.T, srv *Server, id int64) *store.WebBuilderDoc {
	t.Helper()
	d, err := srv.store.WebBuilder().Doc(context.Background(), id)
	if err != nil {
		t.Fatalf("doc of %d: %v", id, err)
	}
	return d
}

// wbhSaveCore saves a page through the core REST API (/wp/v2/pages, what the block editor does).
func wbhSaveCore(t *testing.T, admin *wbrClient, id int64, body map[string]any) {
	t.Helper()
	if code, v, _ := admin.do(http.MethodPost, "/wp/v2/pages/"+itoa64(id), body, nil); code != http.StatusOK {
		t.Fatalf("core save: %d %v", code, v)
	}
}

func wbhJSONEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%v: %s", err, a)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return reflect.DeepEqual(x, y)
}

func TestWebBuilderHooksDrift(t *testing.T) {
	srv, base, admin := wbrStart(t)
	ctx := context.Background()
	p := wbhPublish(t, srv, admin, "Drift", "drift", "initial")
	if d := wbhDoc(t, srv, p.ID); d.Detached || d.ContentHash != store.WebBuilderContentHash(p.Content) {
		t.Fatalf("fresh publish: detached=%v hash ok=%v", d.Detached, d.ContentHash == store.WebBuilderContentHash(p.Content))
	}

	// A core save that leaves posts.content alone (title only) keeps the doc attached.
	wbhSaveCore(t, admin, p.ID, map[string]any{"title": "Drift renamed"})
	if wbhDoc(t, srv, p.ID).Detached {
		t.Fatal("a title-only save detached the doc")
	}
	// So does saving the very same content back.
	wbhSaveCore(t, admin, p.ID, map[string]any{"content": p.Content})
	if wbhDoc(t, srv, p.ID).Detached {
		t.Fatal("saving the unchanged content detached the doc")
	}

	// Editing the content outside the builder detaches it.
	wbhSaveCore(t, admin, p.ID, map[string]any{"content": p.Content + "<p>Edited in the block editor.</p>"})
	if !wbhDoc(t, srv, p.ID).Detached {
		t.Fatal("an outside content edit did not detach the doc")
	}
	// The REST document and the editor boot config report it.
	if _, v, _ := admin.do(http.MethodGet, wbrDocPath(p.ID, ""), nil, nil); wbrObj(v)["detached"] != true {
		t.Errorf("document: detached = %v", wbrObj(v)["detached"])
	}
	if code, page, _ := wbOpen(t, admin.c, base, p.ID); code == http.StatusOK {
		if boot, _ := wbBootOf(t, page); wbPath(boot, "document.detached") != true {
			t.Errorf("boot config: detached = %v", wbPath(boot, "document.detached"))
		}
	} else {
		t.Errorf("editor: %d", code)
	}
	// A later save that restores the builder's content does not re-attach: only a publish does.
	wbhSaveCore(t, admin, p.ID, map[string]any{"content": p.Content})
	if !wbhDoc(t, srv, p.ID).Detached {
		t.Fatal("a core save cleared detached")
	}

	// Publishing without reattach is refused; with reattach the doc is attached again.
	doc, render := wbrFixture(t, "initial")
	rev := wbhDoc(t, srv, p.ID).Rev
	if code, v, _ := admin.do(http.MethodPost, wbrDocPath(p.ID, "/publish"), wbrPublishBody(doc, render, rev, "publish", nil), nil); code != http.StatusConflict || wbrCode(v) != webbuilder.ErrDetached {
		t.Fatalf("publish on a detached page: %d %v", code, v)
	}
	body := wbrPublishBody(doc, render, rev, "publish", nil)
	body["reattach"] = true
	if code, v, _ := admin.do(http.MethodPost, wbrDocPath(p.ID, "/publish"), body, nil); code != http.StatusOK {
		t.Fatalf("reattach publish: %d %v", code, v)
	}
	after, _ := srv.store.GetPost(ctx, p.ID)
	if d := wbhDoc(t, srv, p.ID); d.Detached || d.ContentHash != store.WebBuilderContentHash(after.Content) {
		t.Fatalf("after reattach: detached=%v", d.Detached)
	}

	// Saves through a Wasm plugin (host API) are outside the builder too.
	if _, err := (&pluginHost{s: srv}).UpdatePost(withPluginElevated(ctx), "", p.ID, wbhPluginInput(after.Content+"<p>plugin</p>")); err != nil {
		t.Fatalf("plugin update: %v", err)
	}
	if !wbhDoc(t, srv, p.ID).Detached {
		t.Fatal("a plugin content edit did not detach the doc")
	}

	// A post without a doc is not touched.
	plain := wbrPost(t, srv, "page", "Plain", "publish", 1)
	if detached, err := srv.wbDetectDrift(ctx, plain.ID); err != nil || detached {
		t.Fatalf("plain post: %v %v", detached, err)
	}
}

func TestWebBuilderHooksWXRRoundTrip(t *testing.T) {
	srv, base, admin := wbrStart(t)
	ctx := context.Background()
	a := wbhPublish(t, srv, admin, "Round Trip", "round-trip", "kinds")
	b := wbhPublish(t, srv, admin, "Was Edited", "was-edited", "initial")
	wbhSaveCore(t, admin, b.ID, map[string]any{"content": b.Content + "<p>outside</p>"})
	docA, docB := wbhDoc(t, srv, a.ID), wbhDoc(t, srv, b.ID)
	if !docB.Detached {
		t.Fatal("setup: b is not detached")
	}

	code, xml, _ := get(t, admin.c, base+"/wp-admin/export?_wpnonce="+exportNonce(t, admin.c, base)+"&download=true&content=all")
	if code != http.StatusOK {
		t.Fatalf("export: %d", code)
	}
	for _, want := range []string{wbWXRDocMeta, wbWXRPageMeta, wbWXRDetachedMeta, "<wp:meta_key><![CDATA[" + webbuilder.PostMarkerMeta + "]]></wp:meta_key>"} {
		if !strings.Contains(xml, want) {
			t.Fatalf("export lacks %s", want)
		}
	}
	if strings.Count(xml, wbWXRDetachedMeta) != 1 {
		t.Errorf("only the detached page carries %s", wbWXRDetachedMeta)
	}
	if strings.Contains(xml, "data-wb-") {
		t.Error("the export carries the rendered snapshot")
	}

	// Import into a site that no longer has the pages.
	for _, id := range []int64{a.ID, b.ID} {
		if err := srv.store.DeletePost(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// One more item whose doc is broken: it imports as an ordinary page.
	broken := strings.Replace(xml, "</channel>", `<item><title>Broken</title><wp:post_id>987654</wp:post_id><wp:post_name><![CDATA[broken]]></wp:post_name>`+
		`<wp:status><![CDATA[publish]]></wp:status><wp:post_type><![CDATA[page]]></wp:post_type><content:encoded><![CDATA[<p>kept</p>]]></content:encoded>`+
		`<wp:postmeta><wp:meta_key><![CDATA[_web_builder]]></wp:meta_key><wp:meta_value><![CDATA[1]]></wp:meta_value></wp:postmeta>`+
		`<wp:postmeta><wp:meta_key><![CDATA[_web_builder_doc]]></wp:meta_key><wp:meta_value><![CDATA[{"sections":[{"id":"x","kind":"evil"}]}]]></wp:meta_value></wp:postmeta>`+
		`</item></channel>`, 1)
	body := wbhImport(t, srv, admin, base, broken)
	if !strings.Contains(body, "The Pursae Web Builder design of “Broken” could not be imported") {
		t.Errorf("import report lacks the broken doc line")
	}

	check := func(slug string, want *store.WebBuilderDoc, detached bool) {
		t.Helper()
		p, err := srv.store.PostBySlug(ctx, "page", slug)
		if err != nil {
			t.Fatalf("%s not imported: %v", slug, err)
		}
		d := wbhDoc(t, srv, p.ID)
		if !wbhJSONEqual(t, d.Doc, want.Doc) || !wbhJSONEqual(t, d.Page, want.Page) {
			t.Errorf("%s: doc or page changed in the round trip", slug)
		}
		if d.Renderer != 0 || d.HTML != "" || d.Rev != 1 {
			t.Errorf("%s: renderer=%d html=%d rev=%d, want a snapshot-less row", slug, d.Renderer, len(d.HTML), d.Rev)
		}
		if d.Detached != detached || d.ContentHash != store.WebBuilderContentHash(p.Content) {
			t.Errorf("%s: detached=%v (want %v), hash ok=%v", slug, d.Detached, detached, d.ContentHash == store.WebBuilderContentHash(p.Content))
		}
		if p.Meta[webbuilder.PostMarkerMeta] != "1" {
			t.Errorf("%s: marker meta = %v", slug, p.Meta[webbuilder.PostMarkerMeta])
		}
		for _, k := range []string{wbWXRDocMeta, wbWXRPageMeta, wbWXRDetachedMeta} {
			if _, ok := p.Meta[k]; ok {
				t.Errorf("%s: %s left in posts.meta", slug, k)
			}
		}
	}
	check("round-trip", docA, false)
	check("was-edited", docB, true)

	p, err := srv.store.PostBySlug(ctx, "page", "broken")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.WebBuilder().Doc(ctx, p.ID); err != store.ErrNotFound {
		t.Errorf("broken doc: %v", err)
	}
	if _, ok := p.Meta[webbuilder.PostMarkerMeta]; ok || p.Content != "<p>kept</p>" {
		t.Errorf("broken: marker %v content %q", p.Meta, p.Content)
	}
}

func wbhImport(t *testing.T, srv *Server, admin *wbrClient, base, xml string) string {
	t.Helper()
	return wxrImportBody(t, admin.c, srv, base, xml)
}

func TestWebBuilderHooksGolylangCopy(t *testing.T) {
	srv, _, admin := wbrStart(t)
	ctx := context.Background()
	src := wbhPublish(t, srv, admin, "Contact", "contact", "variants-form")
	from := wbhDoc(t, srv, src.ID)
	if !strings.Contains(from.HTML, "/web-builder/v2/forms/"+itoa64(src.ID)+"/") {
		t.Fatalf("setup: the form fixture has no form action for the source: %.300s", from.HTML)
	}
	lang := &pllLanguage{Slug: "de", Name: "Deutsch", Locale: "de_DE"}

	// Duplicate content: the translation gets the doc, the snapshot and the marker.
	dst := &store.Post{Type: "page", Title: "Kontakt", Slug: "kontakt", Status: "draft", AuthorID: 1, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, dst); err != nil {
		t.Fatal(err)
	}
	srv.pllCopyForTranslation(ctx, src, dst, lang, true)
	got, _ := srv.store.GetPost(ctx, dst.ID)
	d := wbhDoc(t, srv, dst.ID)
	if !wbhJSONEqual(t, d.Doc, from.Doc) || d.CSS != from.CSS || d.Renderer != from.Renderer || d.Rev != 1 || d.Detached {
		t.Errorf("copy: rev=%d renderer=%d detached=%v", d.Rev, d.Renderer, d.Detached)
	}
	if strings.Contains(d.HTML, "/forms/"+itoa64(src.ID)+"/") || !strings.Contains(d.HTML, "/web-builder/v2/forms/"+itoa64(dst.ID)+"/") {
		t.Error("the copy's forms still post to the source page")
	}
	if d.ContentHash != store.WebBuilderContentHash(got.Content) || got.Content != src.Content {
		t.Error("the copy's content_hash does not match its content")
	}
	if got.Meta[webbuilder.PostMarkerMeta] != "1" || dst.Meta[webbuilder.PostMarkerMeta] != "1" {
		t.Errorf("marker: stored %v, in memory %v", got.Meta[webbuilder.PostMarkerMeta], dst.Meta[webbuilder.PostMarkerMeta])
	}
	var page map[string]any
	_ = json.Unmarshal(d.Page, &page)
	if page["slug"] != got.Slug || page["title"] != got.Title {
		t.Errorf("page settings of the copy: %v", page)
	}
	// A later save of the translation in the block editor detaches only the translation.
	wbhSaveCore(t, admin, dst.ID, map[string]any{"content": "<p>Übersetzt</p>"})
	if !wbhDoc(t, srv, dst.ID).Detached || wbhDoc(t, srv, src.ID).Detached {
		t.Error("drift of the translation")
	}

	// Without Duplicate content nothing builder-related is copied, not even the marker.
	empty := &store.Post{Type: "page", Title: "Leer", Slug: "leer", Status: "draft", AuthorID: 1, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, empty); err != nil {
		t.Fatal(err)
	}
	srv.pllCopyForTranslation(ctx, src, empty, lang, false)
	if _, err := srv.store.WebBuilder().Doc(ctx, empty.ID); err != store.ErrNotFound {
		t.Errorf("a translation without Duplicate content got a doc: %v", err)
	}
	if e, _ := srv.store.GetPost(ctx, empty.ID); e.Meta[webbuilder.PostMarkerMeta] != nil {
		t.Error("a translation without Duplicate content got the marker")
	}
	// Synchronizing custom fields never carries the marker over either.
	if !pllMetaSkipped(webbuilder.PostMarkerMeta) || !pllMetaSkipped(wbWXRDocMeta) {
		t.Error("builder meta is synchronized between translations")
	}
}

func TestWebBuilderHooksCachePurge(t *testing.T) {
	srv, base, admin := wbrStart(t)
	if !srv.settings.Bool("wpgo_cache_enabled") {
		t.Skip("Pursae Cache is off")
	}
	built := wbhPublish(t, srv, admin, "Cached", "cached", "initial")
	plain := wbrPost(t, srv, "page", "Plain cached", "publish", 1)
	builtURL, plainURL := base+"/"+built.Slug+"/", base+"/"+plain.Slug+"/"
	for _, u := range []string{builtURL, plainURL} {
		_ = bodyOf(anonGet(t, u))
		if r := anonGet(t, u); r.Header.Get("X-WPGo-Cache") != "hit" {
			t.Fatalf("%s not cached: %d %q", u, r.StatusCode, r.Header.Get("X-WPGo-Cache"))
		}
	}
	srv.wbPurgePages()
	if r := anonGet(t, builtURL); r.Header.Get("X-WPGo-Cache") == "hit" {
		t.Error("the builder page survived wbPurgePages")
	}
	if r := anonGet(t, plainURL); r.Header.Get("X-WPGo-Cache") != "hit" {
		t.Error("wbPurgePages purged a page without a doc")
	}
}

func wbhPluginInput(content string) plugins.PostInput { return plugins.PostInput{Content: &content} }
