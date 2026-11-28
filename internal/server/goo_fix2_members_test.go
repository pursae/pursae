package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"wpgo/internal/store"
)

// Memberships leaks outside the_content (security test 2026-11-28, round 2): custom fields, footnotes,
// linked posts, Web Builder fields, the SEO featured image, comments and the "hide completely" listings.
func TestGmemRestrictedPostLeaks(t *testing.T) {
	base, _, srv := gooSite(t)
	ctx := context.Background()
	secret := &store.Post{Type: "post", Title: "Members leak post", Slug: "members-leak", Content: "<p>Secret body text.</p>", Status: "publish",
		AuthorID: 1, CommentStatus: "open", Meta: map[string]any{"footnotes": `[{"content":"secret footnote"}]`}}
	page := &store.Post{Type: "page", Title: "Members leak page", Slug: "members-leak-page", Content: "<p>x</p>", Status: "publish", AuthorID: 1, Meta: map[string]any{}}
	for _, p := range []*store.Post{secret, page} {
		if err := srv.store.InsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	plan := &gooMembershipPlan{Name: "Leak", AccessMethod: "manual", AccessLength: "unlimited",
		Rules: gmemRules{ContentPostIDs: []int64{secret.ID, page.ID}}}
	if err := srv.gmemSavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	gxaOn(t, srv, gmemKey, true)
	gxaSetExt(t, srv, gmemKey, map[string]any{"show_excerpts": false})

	// REST: footnotes stay with the content.
	if code, body, _ := get(t, &http.Client{}, base+"/wp-json/wp/v2/posts/"+itoa64(secret.ID)); code != http.StatusOK || strings.Contains(body, "secret footnote") {
		t.Errorf("REST footnotes: %d %s", code, body)
	}
	// ACF relationship: the linked post's body is not handed to a non-member.
	out, _ := json.Marshal(srv.acfgoFormat(ctx, acfField{"type": "relationship", "return_format": "object"}, []any{secret.ID}))
	if strings.Contains(string(out), "Secret body") {
		t.Errorf("ACF relationship leaks: %s", out)
	}
	// Web Builder fields of a restricted post.
	if e := (&wbSlotEnv{s: srv, ctx: ctx}); !e.memberHidden(secret) {
		t.Error("Web Builder: restricted post's fields not hidden")
	}
	// SEO: no featured image for a restricted post.
	secret.FeaturedMediaID = 4242
	if p := srv.seogoMembersPost(ctx, nil, secret); p.FeaturedMediaID != 0 {
		t.Errorf("SEO featured image kept: %d", p.FeaturedMediaID)
	}
	// Comments: the front form is closed to non-members.
	if code, _, _ := post(t, &http.Client{}, base+"/wp-comments-post", url.Values{"comment_post_ID": {itoa64(secret.ID)}, "comment": {"hi"}, "author": {"V"}, "email": {"v@example.test"}}); code != http.StatusForbidden {
		t.Errorf("comment on restricted post: %d", code)
	}

	// "hide completely": the sitemap and the page list leave restricted entries out.
	if err := srv.gooSetOpts(ctx, map[string]any{"ext_" + gmemKey + "_settings": map[string]any{"restriction_mode": "hide"}}); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := get(t, &http.Client{}, base+"/wp-sitemap-posts-post-1.xml"); strings.Contains(body, "members-leak") {
		t.Errorf("sitemap lists hidden post: %s", body)
	}
	rctx := withRender(ctx, &renderCtx{s: srv, lb: srv.links(ctx)})
	if out := renderPageListBlock(rctx, pageListFallbackBlock(), ""); strings.Contains(out, "members-leak-page") {
		t.Errorf("page list shows hidden page: %s", out)
	}
	member := gxaCustomer(t, srv, "leak-member@example.test")
	if _, err := srv.gmemGrant(ctx, member.ID, plan, 0, 0, "test"); err != nil {
		t.Fatal(err)
	}
	mctx := withRender(ctx, &renderCtx{s: srv, lb: srv.links(ctx), user: member})
	if out := renderPageListBlock(mctx, pageListFallbackBlock(), ""); !strings.Contains(out, "members-leak-page") {
		t.Errorf("page list hides the page from its member: %s", out)
	}
}
