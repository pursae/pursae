package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"wpgo/internal/store"
)

// The comments of a Memberships-restricted post are part of the restricted content: the site-wide and
// per-post comment feeds, the REST comments routes and the post's own comment list do not show them to
// a non-member, while the store manager still sees them.
func TestT2NAccountsMembershipComments(t *testing.T) {
	base, admin, srv := gooSite(t)
	ctx := context.Background()
	locked := &store.Post{Type: "post", Title: "Locked talk", Slug: "locked-talk", Status: "publish", AuthorID: 1, Meta: map[string]any{}, Content: "<p>Members text.</p>", CommentStatus: "open"}
	open := &store.Post{Type: "post", Title: "Open talk", Slug: "open-talk", Status: "publish", AuthorID: 1, Meta: map[string]any{}, Content: "<p>Public text.</p>", CommentStatus: "open"}
	for _, p := range []*store.Post{locked, open} {
		if err := srv.store.InsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	secret := &store.Comment{PostID: locked.ID, AuthorName: "Mia", AuthorEmail: "mia@example.test", Content: "Velvetcomment members only", Type: "comment", Status: "approved"}
	public := &store.Comment{PostID: open.ID, AuthorName: "Pia", AuthorEmail: "pia@example.test", Content: "Sunnycomment for all", Type: "comment", Status: "approved"}
	for _, cm := range []*store.Comment{secret, public} {
		if err := srv.store.InsertComment(ctx, cm); err != nil {
			t.Fatal(err)
		}
	}
	plan := &gooMembershipPlan{Name: "Velvet", AccessMethod: "manual", AccessLength: "unlimited", Rules: gmemRules{ContentPostIDs: []int64{locked.ID}}}
	if err := srv.gmemSavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := srv.gooSetExtension(ctx, gmemKey, true); err != nil {
		t.Fatal(err)
	}
	lb := srv.links(ctx)
	path := func(abs string) string {
		u, err := url.Parse(abs)
		if err != nil {
			t.Fatal(err)
		}
		return base + u.RequestURI()
	}
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for name, u := range map[string]string{
		"site comment feed": path(lb.CommentsFeedLink("rss2")),
		"site atom feed":    path(lb.CommentsFeedLink("atom")),
		"post comment feed": path(lb.PostCommentsFeed(locked)),
		"post page":         path(lb.Permalink(locked)),
		"REST list":         base + "/wp-json/wp/v2/comments",
		"REST post list":    base + "/wp-json/wp/v2/comments?post=" + itoa64(locked.ID),
		"REST comment":      base + "/wp-json/wp/v2/comments/" + itoa64(secret.ID),
	} {
		_, body, _ := get(t, anon, u)
		if strings.Contains(body, "Velvetcomment") {
			t.Errorf("%s shows a restricted post's comment to a visitor", name)
		}
	}
	if _, body, _ := get(t, anon, path(lb.CommentsFeedLink("rss2"))); !strings.Contains(body, "Sunnycomment") {
		t.Error("site comment feed lost an open post's comment")
	}
	if _, body, _ := get(t, anon, base+"/wp-json/wp/v2/comments"); !strings.Contains(body, "Sunnycomment") {
		t.Error("REST list lost an open post's comment")
	}
	// The store manager (administrator) is not restricted.
	rc := newRESTClient(t, admin, base)
	if code, _, list, _ := rc.do("GET", "/wp/v2/comments?post="+itoa64(locked.ID), nil); code != http.StatusOK || len(list) != 1 {
		t.Errorf("administrator: %d %v", code, list)
	}
}
