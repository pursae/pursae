package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"wpgo/internal/store"
)

// Menu items pointing at unpublished posts are dropped from the front end and the Headless menu,
// like WordPress's _invalid items: their titles and links must not leak.
func TestMenuDropsUnpublishedTargets(t *testing.T) {
	base, _, srv := hgSite(t)
	ctx := context.Background()
	pub := &store.Post{Type: "page", Title: "Public page", Slug: "public-page", Status: "publish", AuthorID: 1}
	if err := srv.store.InsertPost(ctx, pub); err != nil {
		t.Fatal(err)
	}
	id, err := srv.store.CreateMenu(ctx, "Main")
	if err != nil {
		t.Fatal(err)
	}
	loc := "primary"
	if locs := srv.activeTheme().MenuLocations; len(locs) > 0 {
		loc = locs[0].Slug
	}
	_ = srv.store.SetMenuLocations(ctx, map[string]int64{loc: id})
	_ = srv.store.AddMenuItem(ctx, &store.EditItem{MenuID: id, Kind: "post_type", Object: "page", TargetID: pub.ID, Status: "publish", Order: 1})
	for i, st := range []string{"draft", "private", "pending", "future", "trash"} {
		p := &store.Post{Type: "page", Title: "Secret " + st + " title", Slug: "secret-" + st, Status: st, AuthorID: 1}
		if st == "future" {
			when := time.Now().Add(72 * time.Hour)
			p.PublishedAt = &when
		}
		if err := srv.store.InsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
		_ = srv.store.AddMenuItem(ctx, &store.EditItem{MenuID: id, Kind: "post_type", Object: "page", TargetID: p.ID, Status: "publish", Order: 2 + i})
	}
	r := hgCall(t, base, "GET", "/wp/menus/"+loc, "", nil)
	if r.Code != 200 || !strings.Contains(r.Raw, "public-page") || strings.Contains(r.Raw, "secret") {
		t.Errorf("headless menu: %d %s", r.Code, r.Raw)
	}
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "Secret") {
		i := strings.Index(string(body), "Secret")
		t.Errorf("front-end menu leaks an unpublished target: %s", string(body)[max(0, i-300):i+100])
	}
	if !strings.Contains(string(body), "Public page") {
		t.Logf("front page does not print the menu (theme without the location?)")
	}
}
