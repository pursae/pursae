package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"wpgo/internal/store"
)

func TestSEOGoProductNodeNeverInventsReviewCount(t *testing.T) {
	s := &Server{}
	mk := func(meta map[string]any) ldNode {
		sc := &seogoCtx{Post: &store.Post{Title: "Mug", Meta: meta}}
		return s.seogoProductNode(sc, "https://x.test/mug/", "Mug", "", nil)
	}
	if n := mk(map[string]any{"_seogo_product_rating": "4.5"}); n["aggregateRating"] != nil {
		t.Errorf("a rating without a review count produced %v", n["aggregateRating"])
	}
	n := mk(map[string]any{"_seogo_product_rating": "4.5", "_seogo_product_review_count": "12"})
	if ar, _ := n["aggregateRating"].(ldNode); ar == nil || ar["reviewCount"] != "12" {
		t.Errorf("real review count lost: %v", n["aggregateRating"])
	}
}

func TestSEOGoScoreBand(t *testing.T) {
	for _, c := range []struct {
		v     string
		na    bool
		class string
	}{{"90", false, "good"}, {"71", false, "good"}, {"50", false, "ok"}, {"10", false, "bad"}, {"", false, "na"}, {"90", true, "na"}, {"x", false, "na"}} {
		if got, _ := seogoScoreBand(c.v, c.na); got != c.class {
			t.Errorf("seogoScoreBand(%q,%v) = %s, want %s", c.v, c.na, got, c.class)
		}
	}
}

// The store's Product (real price, stock, reviews) is part of the SEO @graph; no second JSON-LD block.
func TestSEOGoCommerceProductInGraph(t *testing.T) {
	base, c, srv := gooSite(t)
	seogoToggle(t, c, base, "activate")
	runner := gooMakeSimple(t, srv, "Runner", 90, nil)
	productURL := srv.gooPermalink(t.Context(), runner)
	_, page, _ := get(t, c, productURL)
	graph := seogoJSONLD(t, page)
	n := graphNode(graph, "Product")
	if n == nil || n["@id"] != productURL+"#product" || n["offers"] == nil {
		t.Fatalf("no Commerce Product with offers in the SEO graph: %v", n)
	}
	if strings.Count(page, `"@type":"Product"`) != 1 {
		t.Errorf("the product is described %d times in the page", strings.Count(page, `"@type":"Product"`))
	}
}

func TestSEOGoRedirectsCSVAnd404(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	seogoToggle(t, c, base, "activate")
	ctx := t.Context()
	// CSV import then export round trip (header skipped, code and match type kept).
	csv := "source,target,code,match_type\n/a-old/,/a-new/,302,exact\n/b-old/,/b-new/\n"
	if code, loc := seogoForm(t, c, base, "redirections", url.Values{"action": {"import"}, "csv": {csv}}); code != http.StatusFound || strings.Contains(loc, "seogo-error") {
		t.Fatalf("import: %d %s", code, loc)
	}
	rows, _ := srv.seogoRedirectRows(ctx, false)
	got := map[string]*seogoRedirectRow{}
	for i := range rows {
		got[strings.Trim(rows[i].Source, "/")] = rows[i]
	}
	if r := got["a-old"]; r == nil || r.Code != 302 || r.Target != "/a-new/" {
		t.Errorf("imported row a-old: %+v", r)
	}
	if r := got["b-old"]; r == nil || r.Code != 301 {
		t.Errorf("imported row b-old: %+v", r)
	}
	if code, _, loc := get(t, c, base+"/a-old/"); code != http.StatusFound || !strings.HasSuffix(loc, "/a-new/") {
		t.Errorf("imported redirect: %d %s", code, loc)
	}

	// 404 monitor: the ignore list skips matching URIs; "redirect all 404s" answers with the fallback.
	o := srv.seogoOpts().clone()
	o.MonitorIgnore = "ignored-part"
	_ = srv.seogoSave(ctx, o)
	get(t, c, base+"/ignored-part-xyz/")
	get(t, c, base+"/tracked-xyz/")
	deadline := time.Now().Add(3 * time.Second)
	for len(srv.seogo404Rows(ctx, "tracked-xyz")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("404 not logged")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(srv.seogo404Rows(ctx, "ignored-part-xyz")); n != 0 {
		t.Errorf("an ignored 404 was logged (%d rows)", n)
	}
	o = srv.seogoOpts().clone()
	o.Redirect404 = "/landing/"
	_ = srv.seogoSave(ctx, o)
	if code, _, loc := get(t, c, base+"/nothing-here-xyz/"); code != http.StatusMovedPermanently || loc != "/landing/" {
		t.Errorf("redirect all 404s: %d %s", code, loc)
	}
}

func TestSEOGoRoleManagerLimitsCaps(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	seogoToggle(t, c, ts.URL, "activate")
	u := &store.User{ID: 99, Roles: []string{"editor"}}
	if srv.seogoCan(u, "seogo_role_manager") {
		t.Error("an editor may manage roles by default")
	}
	o := srv.seogoOpts().clone()
	o.Modules["role_manager"] = true
	o.Roles = map[string][]string{"editor": {"seogo_redirections"}}
	_ = srv.seogoSave(t.Context(), o)
	if !srv.seogoCan(u, "seogo_redirections") || srv.seogoCan(u, "seogo_general") {
		t.Error("role manager does not grant exactly the configured capability")
	}
	if srv.seogoCan(nil, "seogo_redirections") {
		t.Error("anonymous user holds a capability")
	}
}

func TestSEOGoTermSlugRedirectAndListFilters(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	seogoToggle(t, c, base, "activate")
	ctx := t.Context()
	term := &store.Term{Name: "Guides", Slug: "guides", Taxonomy: "category"}
	if err := srv.store.InsertTerm(ctx, term); err != nil {
		t.Fatal(err)
	}
	old := *term
	term.Slug = "manuals"
	srv.seogoTermMoved(ctx, &old, term)
	oldPath := "/" + normPath(srv.seogoTermLoc(ctx, &old)[len(base):])
	rows, _ := srv.seogoRedirectRows(ctx, false)
	found := false
	for _, r := range rows {
		if strings.Trim(r.Source, "/") == strings.Trim(oldPath, "/") && r.Origin == "auto" && strings.Contains(r.Target, "manuals") {
			found = true
		}
	}
	if !found {
		t.Errorf("no automatic redirect for the old term slug %q: %+v", oldPath, rows)
	}

	// Posts list: SEO columns and the "No focus keyphrase" filter.
	_, body := restSend(t, c, base, http.MethodPost, "/wp-json/wp/v2/posts", `{"title":"Plain post","status":"publish","content":"<p>x</p>"}`)
	_ = body
	_, list, _ := get(t, c, base+"/wp-admin/edit")
	if !strings.Contains(list, `column-seogo-details`) || !strings.Contains(list, "Keyword:") || !strings.Contains(list, "seo_filter=nofocus") {
		t.Error("posts list lacks the SEO columns or the filter view")
	}
	_, filtered, _ := get(t, c, base+"/wp-admin/edit?seo_filter=nofocus")
	if !strings.Contains(filtered, "Plain post") {
		t.Error("the no-focus-keyphrase filter hides a post without a focus keyphrase")
	}
}
