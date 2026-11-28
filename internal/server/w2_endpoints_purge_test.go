package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The purge URL may name a proxy on this machine or the private network (Varnish, nginx) only with
// PURSAE_ALLOW_PRIVATE_ENDPOINTS=1, and never a link-local or metadata address.
func TestW2EndpointsPurgeURLSaveTime(t *testing.T) {
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	for _, raw := range []string{"http://127.0.0.1:6081", "http://localhost/purge", "http://10.1.2.3/purge", "http://[::1]/"} {
		if err := proxyPurgeURLProblem(raw); err == nil || !strings.Contains(err.Error(), "PURSAE_ALLOW_PRIVATE_ENDPOINTS") {
			t.Errorf("%q without the switch: err = %v", raw, err)
		}
	}
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{"", true},
		{"http://127.0.0.1:6081", true},
		{"http://localhost/purge", true},
		{"http://varnish:80", true},
		{"http://10.1.2.3/purge", true},
		{"https://purge.example.com", true},
		{"http://169.254.169.254/latest", false},
		{"http://[fe80::1]/", false},
		{"http://[::ffff:169.254.169.254]/", false},
		{"http://100.100.100.200/", false},
		{"http://0.0.0.0/", false},
		{"gopher://127.0.0.1/", false},
		{"http://127.0.0.1/#frag", false},
	} {
		if err := proxyPurgeURLProblem(tc.raw); (err == nil) != tc.ok {
			t.Errorf("%q: err = %v, want ok=%v", tc.raw, err, tc.ok)
		}
	}
}

// The REST settings route (and the MCP tool on it) refuses a metadata purge URL.
func TestW2EndpointsPurgeURLREST(t *testing.T) {
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	ts, c, srv := newTestServer(t, false)
	installAndLogin(t, ts, c)
	_, nonce, _ := get(t, c, ts.URL+"/wp-admin/admin-ajax?action=rest-nonce")
	send := func(v string) int {
		b, _ := json.Marshal(map[string]any{"wpgo_cache_purge_url": v})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/wp-json/pursae/v1/cache/settings", bytes.NewReader(b))
		req.Header.Set("X-WP-Nonce", nonce)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := send("http://169.254.169.254/latest/meta-data"); code != http.StatusBadRequest {
		t.Fatalf("metadata purge URL: %d", code)
	}
	if got := srv.settings.String("wpgo_cache_purge_url"); got != "" {
		t.Fatalf("stored purge URL = %q", got)
	}
	if code := send("http://127.0.0.1:6081"); code != http.StatusBadRequest {
		t.Fatalf("loopback purge URL without the switch: %d", code)
	}
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	if code := send("http://127.0.0.1:6081"); code != http.StatusOK {
		t.Fatalf("local Varnish purge URL: %d", code)
	}
}

// At connect time the purge client refuses a loopback proxy reached through a host name unless
// PURSAE_ALLOW_PRIVATE_ENDPOINTS=1, refuses link-local addresses and never follows a redirect.
func TestW2EndpointsPurgeDialTime(t *testing.T) {
	var other atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other.Add(1) }))
	defer elsewhere.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bounce" {
			http.Redirect(w, r, elsewhere.URL+"/x", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	base := localhostURL(t, proxy)

	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	req, _ := http.NewRequest("PURGE", base+"/", nil)
	if err := sendProxyPurge(req); err == nil || !strings.Contains(err.Error(), "PURSAE_ALLOW_PRIVATE_ENDPOINTS") {
		t.Fatalf("loopback purge target without the switch: %v", err)
	}
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	req, _ = http.NewRequest("PURGE", base+"/", nil)
	if err := sendProxyPurge(req); err != nil {
		t.Fatalf("local proxy by host name: %v", err)
	}
	req, _ = http.NewRequest("PURGE", base+"/bounce", nil)
	if err := sendProxyPurge(req); err == nil || !strings.Contains(err.Error(), "HTTP 307") || other.Load() != 0 {
		t.Fatalf("redirect followed: err %v, other host hits %d", err, other.Load())
	}
	req, _ = http.NewRequest("PURGE", "http://[fe80::1]:6081/", nil)
	if err := sendProxyPurge(req); err == nil || !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("link-local purge target: %v", err)
	}
}
