package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"wpgo/internal/store"
)

// noindex sent as "1"/true by a REST client was stored as "1" and ignored by the front end; og_image
// took a javascript: URL into og:image.
func TestSeogoNormalizesNoindexAndImageURL(t *testing.T) {
	for in, want := range map[string]string{"1": "noindex", "true": "noindex", "noindex": "noindex", "index": "index", "0": ""} {
		m := map[string]any{}
		seogoSetMeta(m, "noindex", in)
		if got := seogoMeta(m, "noindex"); got != want {
			t.Errorf("noindex %q stored as %q, want %q", in, got, want)
		}
	}
	if got := seogoMeta(map[string]any{"_seogo_noindex": "1"}, "noindex"); got != "noindex" {
		t.Errorf("legacy stored \"1\" reads %q", got)
	}
	for in, ok := range map[string]bool{"javascript:alert(1)": false, "JaVaScRiPt:alert(1)": false, "data:image/png;base64,AA": false,
		"https://example.com/a.png": true, "/wp-content/uploads/a.png": true, "//evil.test/a.png": false} {
		m := map[string]any{}
		seogoSetMeta(m, "og_image", in)
		if got := seogoMeta(m, "og_image"); (got == in) != ok {
			t.Errorf("og_image %q stored as %q", in, got)
		}
	}
	if got := seogoMeta(map[string]any{"_seogo_og_image": "javascript:alert(1)"}, "og_image"); got != "" {
		t.Errorf("stored javascript: og_image reads %q", got)
	}
}

// get_field() / Web Builder slots formatted a relationship's private and draft posts for anonymous
// visitors (only REST filtered them).
func TestACFGoFormatHidesUnreadableRelatedPosts(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	ctx := context.Background()
	installAndLogin(t, ts, c)
	var ids []any
	for _, p := range []*store.Post{
		{Type: "post", Title: "Secret draft", Slug: "secret-draft", Status: "draft", AuthorID: 1, Meta: map[string]any{}},
		{Type: "post", Title: "Private memo", Slug: "private-memo", Status: "private", AuthorID: 1, Meta: map[string]any{}},
		{Type: "post", Title: "Public news", Slug: "public-news", Status: "publish", AuthorID: 1, Meta: map[string]any{}},
	} {
		if err := srv.store.InsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	for _, f := range []acfField{{"type": "relationship", "return_format": "object"}, {"type": "post_object", "return_format": "id", "multiple": 1}} {
		out, _ := json.Marshal(srv.acfgoFormat(ctx, f, ids))
		var list []any
		_ = json.Unmarshal(out, &list)
		if strings.Contains(string(out), "Secret") || strings.Contains(string(out), "Private") || len(list) != 1 {
			t.Fatalf("%v leaks unreadable posts: %s", f["type"], out)
		}
		if !strings.Contains(string(out), "Public news") && !strings.Contains(string(out), itoa64(ids[2].(int64))) {
			t.Fatalf("%v lost the published post: %s", f["type"], out)
		}
	}
	admin, _ := srv.store.UserByID(ctx, 1)
	if admin != nil {
		rctx := context.WithValue(ctx, renderKey{}, &renderCtx{user: admin})
		out, _ := json.Marshal(srv.acfgoFormat(rctx, acfField{"type": "relationship", "return_format": "object"}, ids))
		if !strings.Contains(string(out), "Private memo") {
			t.Fatalf("the administrator viewing the page lost the private post: %s", out)
		}
	}
}

// A repeater's stored row count and its input are bounded, and one save cannot write unbounded keys.
func TestACFGoRepeaterBounds(t *testing.T) {
	in := make([]any, acfgoMaxRows+50)
	for i := range in {
		in[i] = map[string]any{}
	}
	if n := len(acfgoRows(in)); n != acfgoMaxRows {
		t.Fatalf("rows = %d, want %d", n, acfgoMaxRows)
	}
	if n := acfgoStoredRows("2000000000"); n != acfgoMaxRows {
		t.Fatalf("stored rows = %d", n)
	}
	w := newACFWrite()
	for i := 0; i < acfgoMaxKeys; i++ {
		w.set[itoa64(int64(i))] = ""
	}
	if !w.full() {
		t.Fatal("write not full at acfgoMaxKeys")
	}
	_, _, srv := testServerBuilderOff(t)
	if err := srv.acfgoCommit(context.Background(), &acfObject{Kind: "post", ID: 1}, w); err == nil {
		t.Fatal("an oversized save was committed")
	}
}

// An unpublished flipbook embedded in another user's post stays hidden (and unpublished).
func TestFlipbookEmbedNeedsOwner(t *testing.T) {
	ts, c, srv := newTestServer(t, false)
	ctx := context.Background()
	installAndLogin(t, ts, c)
	if err := srv.settings.Set(ctx, map[string]any{flipbookSetting: true}); err != nil {
		t.Fatal(err)
	}
	var id int
	if err := srv.flipbookUpdate(ctx, func(st *flipbookStore) error {
		b := flipbookNew("Board pack", "/wp-content/uploads/2026/11/b.pdf", "b.pdf", 1, 1)
		b.ID = st.NextID
		st.NextID++
		st.Items = append(st.Items, b)
		id = b.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	authorID := asCreateUser(t, srv, "pen", "author", asStrongPass)
	mine := &store.Post{Type: "post", Title: "Mine", Slug: "mine", Status: "publish", AuthorID: authorID, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, mine); err != nil {
		t.Fatal(err)
	}
	sc := "[pursae_flipbook id=" + itoa64(int64(id)) + "]"
	out := srv.flipbookContent(context.WithValue(ctx, renderKey{}, &renderCtx{postID: mine.ID}), sc)
	if !strings.Contains(out, "not published") {
		t.Fatalf("another user's draft book rendered in an author's post: %.200s", out)
	}
	if b, _ := srv.flipbookGet(id); b.Published {
		t.Fatal("the book was published by an author's embed")
	}
	adminPost := &store.Post{Type: "post", Title: "Admin", Slug: "admin-post", Status: "publish", AuthorID: 1, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, adminPost); err != nil {
		t.Fatal(err)
	}
	out = srv.flipbookContent(context.WithValue(ctx, renderKey{}, &renderCtx{postID: adminPost.ID}), sc)
	if strings.Contains(out, "not published") {
		t.Fatal("the owner's own embed was refused")
	}
}

// edit-comments?p=<id> printed the title of a post the user may not read.
func TestCommentsFilterHidesUnreadablePostTitle(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	p := &store.Post{Type: "post", Title: "Quarterly layoffs plan", Slug: "layoffs", Status: "draft", AuthorID: 1, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, p); err != nil {
		t.Fatal(err)
	}
	asCreateUser(t, srv, "pen", "author", asStrongPass)
	ac := asNewClient()
	if code, _, _ := asLogin(t, ac, base, "/wp-login", "pen", asStrongPass); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	_, body, _ := get(t, ac, base+"/wp-admin/edit-comments?p="+itoa64(p.ID))
	if strings.Contains(body, "Quarterly layoffs plan") {
		t.Fatal("an author saw the title of the administrator's draft")
	}
	_, body, _ = get(t, c, base+"/wp-admin/edit-comments?p="+itoa64(p.ID))
	if !strings.Contains(body, "Quarterly layoffs plan") {
		t.Fatal("the administrator lost the filter's title")
	}
}

// A password-protected pattern's content.raw is not given to edit_posts users who cannot edit it.
func TestBlocksHideProtectedPatternRaw(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	p := &store.Post{Type: "wp_block", Title: "Locked", Slug: "locked", Status: "publish", Password: "pw", AuthorID: 1,
		Content: "<!-- wp:paragraph --><p>topsecretpattern</p><!-- /wp:paragraph -->", Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, p); err != nil {
		t.Fatal(err)
	}
	asCreateUser(t, srv, "pen", "author", asStrongPass)
	ac := asNewClient()
	if code, _, _ := asLogin(t, ac, base, "/wp-login", "pen", asStrongPass); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	nonce := restNonce(t, srv, ac, base)
	req, _ := http.NewRequest(http.MethodGet, base+"/wp-json/wp/v2/blocks?context=edit", nil)
	req.Header.Set("X-WP-Nonce", nonce)
	resp, err := ac.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	_, _ = io.Copy(&buf, resp.Body)
	if !strings.Contains(buf.String(), `"Locked"`) {
		t.Fatalf("pattern not listed: %.300s", buf.String())
	}
	if strings.Contains(buf.String(), "topsecretpattern") {
		t.Fatalf("an author got the raw content of a protected pattern: %.300s", buf.String())
	}
}
