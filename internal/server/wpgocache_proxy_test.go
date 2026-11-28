package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wpgo/internal/cache"
)

func TestProxyPurgeClientBlocksMetadata(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxyPurgeClient.Do(req); err == nil {
		t.Fatal("purge client dialed the link-local metadata address")
	}
}

func TestProxyPurgeRequest(t *testing.T) {
	ctx := context.Background()
	req, err := proxyPurgeRequest(ctx, "http://proxy:8080/purge", "wpgo_", cache.Purge{Tags: []string{"post:12", "home"}})
	if err != nil || req == nil {
		t.Fatalf("tags: %v %v", req, err)
	}
	if req.Method != "PURGE" || req.URL.String() != "http://proxy:8080/purge" {
		t.Fatalf("tags request = %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("Surrogate-Key"); got != "wpgo_post:12 wpgo_home" {
		t.Fatalf("Surrogate-Key = %q", got)
	}
	if got := req.Header.Get("Cache-Tag"); got != "wpgo_post:12,wpgo_home" {
		t.Fatalf("Cache-Tag = %q", got)
	}
	if got := req.Header.Get("X-LiteSpeed-Purge"); got != "tag=wpgo_post:12, tag=wpgo_home" {
		t.Fatalf("X-LiteSpeed-Purge = %q", got)
	}
	req, _ = proxyPurgeRequest(ctx, "http://proxy:8080/purge/", "wpgo_", cache.Purge{URL: "https://example.com/2026/hello/?x=1"})
	if req.URL.String() != "http://proxy:8080/purge/2026/hello/?x=1" {
		t.Fatalf("url request = %s", req.URL)
	}
	req, _ = proxyPurgeRequest(ctx, "http://proxy:8080/purge", "wpgo_", cache.Purge{All: true})
	if req.Header.Get("X-Purge-All") != "1" || req.Header.Get("X-LiteSpeed-Purge") != "*" {
		t.Fatalf("all headers = %v", req.Header)
	}
	if req, _ := proxyPurgeRequest(ctx, "http://proxy/", "wpgo_", cache.Purge{}); req != nil {
		t.Fatal("an empty purge produced a request")
	}
}

func TestProxyPurgeRequestPreservesEscapedPath(t *testing.T) {
	req, err := proxyPurgeRequest(context.Background(), "http://proxy:8080/pur%67e/", "wpgo_", cache.Purge{
		URL: "https://example.com/cat%61log/?page=2",
	})
	if err != nil || req == nil {
		t.Fatalf("URL purge: %v %v", req, err)
	}
	if got, want := req.URL.RequestURI(), "/pur%67e/cat%61log/?page=2"; got != want {
		t.Fatalf("request target = %q, want %q", got, want)
	}
}

func TestProxyPurgeConfigured(t *testing.T) {
	for _, tc := range []struct {
		h    cache.ServerHint
		u    string
		want bool
	}{
		{cache.HintGeneric, "http://127.0.0.1:8080/purge", true},
		{cache.HintNginx, "https://cdn.example.com", true},
		{cache.HintLiteSpeed, "", false},
		{cache.HintGeneric, "ftp://x", false},
		{cache.HintNone, "http://x", false},
		{cache.HintAuto, "http://x", false},
	} {
		if got := proxyPurgeConfigured(tc.h, tc.u); got != tc.want {
			t.Errorf("%s %q = %v, want %v", tc.h, tc.u, got, tc.want)
		}
	}
}

// A purge of the page cache reaches the configured front proxy, the settings map onto the engine,
// and "Enable Cache" no longer deactivates the plugin.
func TestPageCacheProxyPurgeAndSettings(t *testing.T) {
	// The test proxy listens on 127.0.0.1: a private purge target needs the operator switch.
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	ts, c0, srv := newTestServer(t, false)
	installClassic(t, c0, ts.URL)
	got := make(chan *http.Request, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r }))
	defer proxy.Close()
	ctx := context.Background()
	if err := srv.settings.Set(ctx, map[string]any{"wpgo_cache_enabled": true, "wpgo_cache_server": "generic",
		"wpgo_cache_purge_url": proxy.URL + "/purge", "wpgo_cache_query_strings": "lang\ncolor",
		"wpgo_cache_exclude_cookies": "mysess_*", "wpgo_cache_exclude_ua": "Monitor", "wpgo_cache_browser_ttl": 600,
		"wpgo_cache_redirect_ttl": 60, "wpgo_cache_max_mb": 32}); err != nil {
		t.Fatal(err)
	}
	srv.configureCache()
	cfg := srv.currentCache().Config()
	if cfg.ProxyMaxAge != 0 {
		t.Fatalf("ProxyMaxAge = %v with a purge URL set", cfg.ProxyMaxAge)
	}
	if !strings.Contains(strings.Join(cfg.CacheQueryStrings, ","), "lang") || len(cfg.ExcludeUserAgents) != 1 ||
		cfg.BrowserCacheTTL != 600*time.Second || cfg.RedirectTTL != time.Minute || cfg.MaxBytes != 32<<20 {
		t.Fatalf("settings not applied: %+v", cfg)
	}
	found := false
	for _, c := range cfg.ExcludeCookies {
		found = found || c == "mysess_*"
	}
	if !found {
		t.Fatalf("ExcludeCookies = %v", cfg.ExcludeCookies)
	}
	srv.currentCache().PurgeTags("post:7")
	deadline := time.After(3 * time.Second)
	for done := false; !done; { // rebuilding the cache purges everything first: skip those requests
		select {
		case r := <-got:
			if r.Header.Get("Surrogate-Key") == "" {
				continue
			}
			if r.Method != "PURGE" || r.Header.Get("Surrogate-Key") != "wpgo_post:7" {
				t.Fatalf("proxy got %s %v", r.Method, r.Header)
			}
			done = true
		case <-deadline:
			t.Fatal("the purge never reached the front proxy")
		}
	}

	// Without a purge URL the proxy lifetime is capped.
	_ = srv.settings.Set(ctx, map[string]any{"wpgo_cache_purge_url": ""})
	srv.configureCache()
	if got := srv.currentCache().Config().ProxyMaxAge; got != 300*time.Second {
		t.Fatalf("ProxyMaxAge = %v, want 300s", got)
	}

	// Switching the cache off keeps the plugin active.
	_ = srv.settings.Set(ctx, map[string]any{"wpgo_cache_serve": false})
	srv.configureCache()
	if srv.currentCache().Config().Enabled {
		t.Fatal("cache still enabled after wpgo_cache_serve=false")
	}
	if !srv.settings.Bool("wpgo_cache_enabled") {
		t.Fatal("plugin switch changed")
	}
}

// The crawler's list comes from the sitemap providers: home, posts, pages, terms and authors.
func TestWarmupURLsFromSitemap(t *testing.T) {
	ts, c0, srv := newTestServer(t, false)
	installClassic(t, c0, ts.URL)
	urls := srv.warmupURLs(context.Background())
	if len(urls) == 0 || urls[0] != srv.links(context.Background()).HomeURL("/") {
		t.Fatalf("urls = %v", urls)
	}
	seen := map[string]bool{}
	for _, u := range urls {
		if seen[u] {
			t.Fatalf("duplicate %s", u)
		}
		seen[u] = true
	}
}
