package server

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// Add Themes › Library follows the Manager's library order ("position" on each theme), also within
// a category and in search results; themes without a place follow by name, and a Manager that
// sends no positions keeps the name order.
func TestManagerThemeLibraryOrder(t *testing.T) {
	base, c, srv, hub := managerThemeSite(t)
	hub.addTheme(t, "mgr-journal", "Manager Journal", "1.0.0", "blog", "public", false)
	hub.addTheme(t, "mgr-folio", "Manager Folio", "1.0.0", "portfolio", "public", false)
	hub.addTheme(t, "mgr-pro", "Manager Pro", "1.0.0", "blog", "public", false)

	order := func(query string, names ...string) {
		t.Helper()
		_, body, _ := get(t, c, base+"/wp-admin/theme-install"+query)
		at := -1
		for _, n := range names {
			i := strings.Index(body, ">"+n+"<")
			if i < 0 {
				t.Errorf("%s: no card for %s", query, n)
				return
			}
			if i < at {
				t.Errorf("%s: want the order %v", query, names)
				return
			}
			at = i
		}
	}
	// An older Manager: no positions, name order.
	order("", "Manager Folio", "Manager Journal", "Manager Pro")
	order("?browse=library&category=blog", "Manager Journal", "Manager Pro")

	hub.setPositions(map[string]int{"mgr-pro": 1, "mgr-journal": 2})
	srv.hubDropCaches(context.Background())
	order("", "Manager Pro", "Manager Journal", "Manager Folio")
	order("?browse=library&category=blog", "Manager Pro", "Manager Journal")
	order("?browse=library&search=manager", "Manager Pro", "Manager Journal", "Manager Folio")

	var slugs []string
	for _, lt := range srv.libraryThemes(context.Background()) {
		slugs = append(slugs, lt.Manifest.Slug)
	}
	if got := strings.Join(slugs, ","); got != "mgr-pro,mgr-journal,mgr-folio" {
		t.Errorf("libraryThemes order: %s", got)
	}
}

func TestLibraryLess(t *testing.T) {
	th := func(slug, name string, pos int) libraryTheme {
		lt := libraryTheme{Manifest: &themeManifest{Slug: slug, Name: name}}
		if pos >= 0 {
			lt.Item = &hubItem{Slug: slug, Position: pos}
		}
		return lt
	}
	list := []libraryTheme{th("zeta", "Zeta", 0), th("dir", "Dir", -1), th("beta", "beta", 2), th("alpha", "Alpha", 0), th("omega", "Omega", 1)}
	sort.SliceStable(list, func(i, j int) bool { return libraryLess(list[i], list[j]) })
	var got []string
	for _, lt := range list {
		got = append(got, lt.Manifest.Slug)
	}
	if want := "omega,beta,alpha,dir,zeta"; strings.Join(got, ",") != want {
		t.Errorf("order %v, want %s", got, want)
	}
}
