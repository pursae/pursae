package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Only real asset requests skip the rate limiter: any path with a query, or a non-GET, may
// reach a dynamic handler.
func TestT2GatesFirewallStaticExemption(t *testing.T) {
	cases := []struct {
		method, url string
		static      bool
	}{
		{http.MethodGet, "/wp-content/themes/x/style.css?ver=1", true},
		{http.MethodHead, "/wp-includes/js/x.js", true},
		{http.MethodGet, "/wp-admin/css/x.css", true},
		{http.MethodGet, "/a/b.png", true},
		{http.MethodGet, "/x.css?s=foo", false},
		{http.MethodPost, "/x.css?wc-ajax=add_to_cart", false},
		{http.MethodPost, "/x.css", false},
		{http.MethodPost, "/wp-content/uploads/a.png", false},
		{http.MethodGet, "/", false},
		{http.MethodGet, "/wp-json/wp/v2/posts", false},
		// #33: the asset trees are answered by the site router too when no file exists.
		{http.MethodGet, "/wp-content/?s=foo", false},
		{http.MethodGet, "/wp-content/x", false},
		{http.MethodGet, "/wp-includes/?p=1", false},
		{http.MethodGet, "/wp-content/a.css?s=foo", false},
		{http.MethodGet, "/wp-content/a.css?ver=1&s=foo", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.url, nil)
		if got := fwStaticRequest(r); got != tc.static {
			t.Errorf("%s %s: static=%v, want %v", tc.method, tc.url, got, tc.static)
		}
	}
}

// The maintenance gate opens the login screen and the CSP report endpoint only where they have
// a route; anything else falls to the site resolver, which serves ?s= and ?p= on any path.
func TestT2GatesMaintenanceAlwaysOpen(t *testing.T) {
	cases := []struct {
		method, path string
		open         bool
	}{
		{http.MethodGet, "/wp-login", true},
		{http.MethodPost, "/wp-login", true},
		{http.MethodGet, "/wp-login.php", true},
		{http.MethodGet, "/wp-login/x", false},
		{http.MethodGet, "/wp-login/", false},
		{http.MethodPost, cspReportPath, true},
		{http.MethodGet, cspReportPath, false},
		{http.MethodHead, cspReportPath, false},
		{http.MethodGet, "/wp-admin/edit.php", true},
		{http.MethodGet, "/wp-content/uploads/a.png", true},
	}
	for _, tc := range cases {
		if got := maintAlwaysOpen(tc.method, tc.path); got != tc.open {
			t.Errorf("%s %s: open=%v, want %v", tc.method, tc.path, got, tc.open)
		}
	}
}
