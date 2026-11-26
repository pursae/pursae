package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// The RSS before/after texts go into every feed item as HTML: without unfiltered_html the options
// API refuses markup KSES would change and the SEO screen stores it filtered.
func TestT2NMiscSEORSSNeedsUnfiltered(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	lim := &store.User{ID: 2, Caps: map[string]bool{"manage_options": true}}
	full := &store.User{ID: 1, Caps: map[string]bool{"manage_options": true, "unfiltered_html": true}}
	ctxFor := func(u *store.User, form url.Values) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
		c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		c.Set(ctxUser, u)
		return c
	}
	evil := `<script>steal()</script><p>Read more</p>`
	patch := []byte(`{"rss_after": "<script>steal()</script><p>Read more</p>"}`)
	if err := optionGroups["seo"].save(srv, ctxFor(lim, nil), patch); err == nil {
		t.Error("the options API stored raw RSS markup for a user without unfiltered_html")
	}
	if strings.Contains(srv.seogoOpts().RSSAfter, "<script") {
		t.Fatalf("stored: %q", srv.seogoOpts().RSSAfter)
	}
	if err := optionGroups["seo"].save(srv, ctxFor(lim, nil), []byte(`{"rss_after": "<p>Thanks for reading %postlink%</p>"}`)); err != nil {
		t.Errorf("filtered markup refused: %v", err)
	}
	if err := optionGroups["seo"].save(srv, ctxFor(full, nil), patch); err != nil {
		t.Errorf("unfiltered_html user refused: %v", err)
	}

	form := url.Values{"tab": {"rss"}, "rss_before": {evil}, "rss_after": {""}}
	if _, err := srv.seogoAdminPost(ctxFor(lim, form), "general"); err != nil {
		t.Fatal(err)
	}
	if got := srv.seogoOpts().RSSBefore; strings.Contains(got, "<script") || !strings.Contains(got, "Read more") {
		t.Errorf("the SEO screen stored %q for a user without unfiltered_html", got)
	}
}
