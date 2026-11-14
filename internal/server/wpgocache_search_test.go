package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Search result pages are served by Pursae Cache for anonymous visitors and dropped when a post is
// published, so the next search finds it; a logged-in user is never served a cached copy.
func TestPageCacheServesSearch(t *testing.T) {
	ts, c0, srv := newTestServer(t, false)
	ctx := context.Background()
	installClassic(t, c0, ts.URL)
	if err := srv.settings.Set(ctx, map[string]any{"wpgo_cache_enabled": true}); err != nil {
		t.Fatal(err)
	}
	srv.configureCache()

	u := ts.URL + "/?s=zebracorn"
	if st, body := pageCacheGet(t, u); st != "miss" || strings.Contains(body, "Zebracorn Facts") {
		t.Fatalf("first search: X-WPGo-Cache = %q, want miss without the post", st)
	}
	if st, _ := pageCacheGet(t, u); st != "hit" {
		t.Fatalf("second search: X-WPGo-Cache = %q, want hit", st)
	}

	if code, raw := restSend(t, c0, ts.URL, http.MethodPost, "/wp-json/wp/v2/posts",
		`{"title":"Zebracorn Facts","content":"All about the zebracorn.","status":"publish"}`); code != http.StatusCreated {
		t.Fatalf("create post: %d %s", code, raw)
	}
	st, body := pageCacheGet(t, u)
	if st != "miss" || !strings.Contains(body, "Zebracorn Facts") {
		t.Fatalf("search after publishing: X-WPGo-Cache = %q, found post = %v; want miss with the post", st, strings.Contains(body, "Zebracorn Facts"))
	}

	resp, err := c0.Get(u) // logged in
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if st := resp.Header.Get("X-WPGo-Cache"); st == "hit" {
		t.Fatalf("logged-in search: X-WPGo-Cache = %q, want no cached copy", st)
	}
}
