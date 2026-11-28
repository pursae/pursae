package server

import (
	"net/http"
	"strings"
	"testing"
)

// QA findings B3, B4, B5, B7: WordPress' REST answers for bad collection params, missing
// referenced objects, term deletion without force and the namespace index.

func TestRESTCollectionParamValidation(t *testing.T) {
	s := secStart(t)
	bad := map[string]string{
		"/wp/v2/posts?orderby=foo":         "orderby",
		"/wp/v2/posts?per_page=1000":       "per_page",
		"/wp/v2/posts?per_page=0":          "per_page",
		"/wp/v2/posts?page=0":              "page",
		"/wp/v2/posts?order=sideways":      "order",
		"/wp/v2/pages?orderby=bogus":       "orderby",
		"/wp/v2/media?per_page=101":        "per_page",
		"/wp/v2/categories?orderby=foo":    "orderby",
		"/wp/v2/tags?order=up":             "order",
		"/wp/v2/users?orderby=password":    "orderby",
		"/wp/v2/comments?orderby=author":   "orderby",
		"/wp/v2/comments?page=abc":         "page",
		"/wp/v2/posts?orderby=foo&page=-1": "orderby",
	}
	for path, param := range bad {
		code, out, _, _ := s.admin.do("GET", path, nil)
		params, _ := out["data"].(map[string]any)["params"].(map[string]any)
		if code != http.StatusBadRequest || out["code"] != "rest_invalid_param" || params[param] == nil {
			t.Errorf("GET %s: %d %v, want 400 rest_invalid_param naming %s", path, code, out, param)
		}
	}
	for _, path := range []string{"/wp/v2/posts?orderby=title&order=asc&per_page=100&page=1", "/wp/v2/pages?orderby=menu_order&per_page=-1",
		"/wp/v2/categories?orderby=count&order=desc", "/wp/v2/users?orderby=name", "/wp/v2/comments?orderby=date_gmt", "/wp/v2/media?orderby=date&order=desc&per_page=24"} {
		if code, out, _, _ := s.admin.do("GET", path, nil); code != http.StatusOK {
			t.Errorf("GET %s: %d %v, want 200", path, code, out)
		}
	}
}

func TestRESTMissingReferences(t *testing.T) {
	s := secStart(t)
	cases := []struct {
		path string
		body map[string]any
		code string
	}{
		{"/wp/v2/posts", map[string]any{"title": "t", "author": 99999}, "rest_invalid_author"},
		{"/wp/v2/posts", map[string]any{"title": "t", "featured_media": 99999}, "rest_invalid_featured_media"},
		{"/wp/v2/pages", map[string]any{"title": "t", "parent": 99999}, "rest_post_invalid_id"},
		{"/wp/v2/categories", map[string]any{"name": "Child", "parent": 9999}, "rest_term_invalid"},
	}
	for _, tc := range cases {
		if code, out, _, _ := s.admin.do("POST", tc.path, tc.body); code != http.StatusBadRequest || out["code"] != tc.code {
			t.Errorf("POST %s %v: %d %v, want 400 %s", tc.path, tc.body, code, out, tc.code)
		}
	}
	// Zero still means "none".
	if code, out, _, _ := s.admin.do("POST", "/wp/v2/posts", map[string]any{"title": "t", "featured_media": 0}); code != http.StatusCreated {
		t.Errorf("featured_media 0: %d %v", code, out)
	}
}

func TestRESTTermDeleteNeedsForce(t *testing.T) {
	s := secStart(t)
	for _, tax := range []string{"categories", "tags"} {
		code, out, _, _ := s.admin.do("POST", "/wp/v2/"+tax, map[string]any{"name": "Gone " + tax})
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", tax, code, out)
		}
		path := "/wp/v2/" + tax + "/" + itoa64(int64(out["id"].(float64)))
		if code, out, _, _ := s.admin.do("DELETE", path, nil); code != http.StatusNotImplemented || out["code"] != "rest_trash_not_supported" {
			t.Errorf("DELETE %s without force: %d %v", path, code, out)
		}
		if code, _, _, _ := s.admin.do("GET", path, nil); code != http.StatusOK {
			t.Errorf("%s was deleted without force", path)
		}
		if code, out, _, _ := s.admin.do("DELETE", path+"?force=true", nil); code != http.StatusOK || out["deleted"] != true {
			t.Errorf("DELETE %s?force=true: %d %v", path, code, out)
		}
	}
}

func TestRESTNamespaceIndex(t *testing.T) {
	s := secStart(t)
	code, out, _, _ := s.admin.do("GET", "/wp/v2", nil)
	if code != http.StatusOK || out["namespace"] != "wp/v2" {
		t.Fatalf("GET /wp/v2: %d %v", code, out)
	}
	routes, _ := out["routes"].(map[string]any)
	if routes["/wp/v2/posts"] == nil || routes["/wp/v2"] == nil {
		t.Errorf("routes miss /wp/v2/posts or the namespace itself: %v", routes)
	}
	for p := range routes {
		if !strings.HasPrefix(p, "/wp/v2") {
			t.Errorf("foreign route %s in the wp/v2 index", p)
		}
	}
	if code, out, _, _ := s.anon().do("GET", "/oembed/1.0", nil); code != http.StatusOK || out["namespace"] != "oembed/1.0" {
		t.Errorf("GET /oembed/1.0: %d %v", code, out)
	}
	if code, _, _, _ := s.admin.do("GET", "/wp/v9", nil); code != http.StatusNotFound {
		t.Errorf("unknown namespace: %d, want 404", code)
	}
}
