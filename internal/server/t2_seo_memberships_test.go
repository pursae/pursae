package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// The SEO head and the image sitemap must not show Memberships-restricted content to viewers the
// front end hides it from.
func TestSeogoMembershipsHead(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	faq := `<!-- wp:seogo/faq {"questions":[{"id":"faq-question-1","question":"Is it secret?","answer":"Hiddenanswer here."}]} --><div class="wp-block-seogo-faq seogo-faq"><div class="seogo-faq-item" id="faq-question-1"><h3 class="seogo-faq-question">Is it secret?</h3><p class="seogo-faq-answer">Hiddenanswer here.</p></div></div><!-- /wp:seogo/faq -->`
	locked := &store.Post{Type: "post", Title: "Locked notes", Slug: "locked-notes", Status: "publish", AuthorID: 1, Meta: map[string]any{},
		Content: `<p>Velvetmoon secret text.</p><p>https://www.youtube.com/watch?v=LockedVid01</p><p><img src="/wp-content/uploads/secret-pic.jpg" alt=""></p>` + faq}
	open := &store.Post{Type: "post", Title: "Open notes", Slug: "open-notes", Status: "publish", AuthorID: 1, Meta: map[string]any{},
		Content: `<p>Sunnyday public text. [goocommerce_members_only]Clubword only.[/goocommerce_members_only]</p><p>https://www.youtube.com/watch?v=OpenVideo01</p><p><img src="/wp-content/uploads/open-pic.jpg" alt=""></p>` + faq}
	for _, p := range []*store.Post{locked, open} {
		if err := srv.store.InsertPost(ctx, p); err != nil {
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
	member := gxaCustomer(t, srv, "velvet-seo@example.test")
	if _, err := srv.gmemGrant(ctx, member.ID, plan, 0, 0, "test"); err != nil {
		t.Fatal(err)
	}
	stranger := gxaCustomer(t, srv, "stranger-seo@example.test")
	admin, err := srv.store.UserByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	head := func(p *store.Post, u *store.User) string {
		t.Helper()
		sc := srv.seogoCtxFor(ctx, &siteQuery{Kind: "single", Post: p})
		srv.seogoMembersCtx(sc, u)
		return srv.seogoHeadFor(sc, false)
	}
	for name, u := range map[string]*store.User{"anonymous": nil, "non-member": stranger} {
		h := head(locked, u)
		for _, leak := range []string{"Velvetmoon", "Hiddenanswer", "secret-pic.jpg"} {
			if strings.Contains(h, leak) {
				t.Errorf("%s: head of a restricted post shows %q", name, leak)
			}
		}
		if h := head(open, u); strings.Contains(h, "Clubword") || !strings.Contains(h, "Sunnyday") || !strings.Contains(h, "Hiddenanswer") {
			t.Errorf("%s: head of an open post with a members-only shortcode is wrong:\n%s", name, h)
		}
	}
	for name, u := range map[string]*store.User{"member": member, "admin": admin} {
		h := head(locked, u)
		for _, want := range []string{"Velvetmoon", "Hiddenanswer", "secret-pic.jpg"} {
			if !strings.Contains(h, want) {
				t.Errorf("%s: head of a restricted post lacks %q", name, want)
			}
		}
		if !strings.Contains(head(open, u), "Clubword") {
			t.Errorf("%s: members-only shortcode text missing from the head", name)
		}
	}

	imgs := strings.Join(srv.seogoPostImages(ctx, locked, nil), " ")
	if strings.Contains(imgs, "secret-pic.jpg") {
		t.Errorf("image sitemap lists a restricted post's content image: %s", imgs)
	}
	if imgs := strings.Join(srv.seogoPostImages(ctx, open, nil), " "); !strings.Contains(imgs, "open-pic.jpg") {
		t.Errorf("image sitemap lost an open post's image: %s", imgs)
	}

	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = httptest.NewRequest(http.MethodGet, "/video-sitemap.xml", nil)
	srv.seogoVideoSitemap(gc)
	vs := w.Body.String()
	for _, leak := range []string{"Velvetmoon", "LockedVid01", "Clubword"} {
		if strings.Contains(vs, leak) {
			t.Errorf("video sitemap shows members-only %q:\n%s", leak, vs)
		}
	}
	if !strings.Contains(vs, "OpenVideo01") || !strings.Contains(vs, "Sunnyday") {
		t.Errorf("video sitemap lost an open post's video:\n%s", vs)
	}

	// With Memberships off nothing changes.
	if err := srv.gooSetExtension(ctx, gmemKey, false); err != nil {
		t.Fatal(err)
	}
	if h := head(locked, nil); !strings.Contains(h, "Hiddenanswer") || !strings.Contains(h, "secret-pic.jpg") {
		t.Error("Memberships off: head lost content")
	}
}
