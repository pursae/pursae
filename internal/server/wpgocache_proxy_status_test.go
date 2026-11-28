package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"wpgo/internal/config"
)

func TestCLICachePurgeReportsProxyFailure(t *testing.T) {
	// The test proxy listens on 127.0.0.1: a private purge target needs the operator switch.
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	ts, client, srv := newTestServer(t, false)
	installAndLogin(t, ts, client)
	cfg := srv.cfg
	cfg.DatabaseURL = config.Env("TEST_DB")

	for _, tc := range []struct {
		name   string
		status int
	}{
		{"accepted", http.StatusNoContent},
		{"not cached", http.StatusNotFound},
		{"rejected", http.StatusMethodNotAllowed},
		{"method-changing redirect", http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/final" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if tc.status == http.StatusFound {
					http.Redirect(w, r, "/final", tc.status)
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer proxy.Close()
			if err := srv.settings.Set(context.Background(), map[string]any{
				"wpgo_cache_server": "generic", "wpgo_cache_purge_url": proxy.URL,
			}); err != nil {
				t.Fatal(err)
			}

			var out, errs bytes.Buffer
			code := CLI(context.Background(), cfg, "pursae", []string{"cache", "purge", "--tag=home"},
				strings.NewReader(""), &out, &errs)
			if tc.status == http.StatusNoContent || tc.status == http.StatusNotFound {
				if code != 0 || !strings.Contains(out.String(), "Purge sent") {
					t.Fatalf("accepted purge: exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
				}
				return
			}
			if code == 0 || !strings.Contains(errs.String(), fmt.Sprintf("HTTP %d", tc.status)) || strings.Contains(out.String(), "Purge sent") {
				t.Fatalf("failed purge: exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
			}
		})
	}
}

func TestCLICacheURLPurgeAcceptsUncachedPage(t *testing.T) {
	// The test proxy listens on 127.0.0.1: a private purge target needs the operator switch.
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	ts, client, srv := newTestServer(t, false)
	installAndLogin(t, ts, client)
	cfg := srv.cfg
	cfg.DatabaseURL = config.Env("TEST_DB")
	var method, path string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		http.NotFound(w, r) // ngx_cache_purge: the URL is not in the cache
	}))
	defer proxy.Close()
	if err := srv.settings.Set(context.Background(), map[string]any{
		"wpgo_cache_server": "nginx", "wpgo_cache_purge_url": proxy.URL,
	}); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := CLI(context.Background(), cfg, "pursae", []string{"cache", "purge", "--url=/about/"},
		strings.NewReader(""), &out, &errs)
	if code != 0 || errs.Len() != 0 {
		t.Fatalf("uncached URL purge: exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
	if method != "PURGE" || path != "/about/" {
		t.Fatalf("proxy got %s %s, want PURGE /about/", method, path)
	}
}
