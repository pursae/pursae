package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestMenusREST covers /wp/v2/menus, /wp/v2/menu-items and /wp/v2/menu-locations the way the
// navigation block and headless clients use them.
func TestMenusREST(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	if code, _, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Menu Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK {
		t.Fatalf("install: %d", code)
	}
	// Anonymous requests are refused: menus are not public in WordPress.
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	for _, path := range []string{"/wp/v2/menus", "/wp/v2/menu-items", "/wp/v2/menu-locations"} {
		if code, body, _ := get(t, anon, base+"/wp-json"+path); code != http.StatusUnauthorized || !strings.Contains(body, `"rest_cannot_view"`) {
			t.Errorf("anonymous %s: %d %s", path, code, body)
		}
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	_, nonce, _ := get(t, c, base+"/wp-admin/admin-ajax?action=rest-nonce")
	rest := func(method, path string, payload any) (int, map[string]any, []any) {
		t.Helper()
		var rd io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+"/wp-json"+path, rd)
		req.Header.Set("X-WP-Nonce", nonce)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: bad JSON %q", method, path, raw)
		}
		obj, _ := out.(map[string]any)
		list, _ := out.([]any)
		return resp.StatusCode, obj, list
	}
	home := base

	// Create a menu.
	code, menu, _ := rest("POST", "/wp/v2/menus", map[string]any{"name": "Main", "auto_add": true})
	if code != http.StatusCreated {
		t.Fatalf("create menu: %d %v", code, menu)
	}
	menuID := int64(menu["id"].(float64))
	if menu["name"] != "Main" || menu["slug"] != "main" || menu["auto_add"] != true || menu["description"] != "" {
		t.Errorf("menu = %v", menu)
	}
	if locs, _ := menu["locations"].([]any); len(locs) != 0 {
		t.Errorf("new menu locations = %v", locs)
	}
	links := menu["_links"].(map[string]any)
	if about := links["about"].([]any)[0].(map[string]any)["href"]; about != home+"/wp-json/wp/v2/taxonomies/nav_menu" {
		t.Errorf("menu about link = %v", about)
	}
	if code, body, _ := rest("POST", "/wp/v2/menus", map[string]any{"name": "main"}); code != http.StatusBadRequest || body["code"] != "menu_exists" {
		t.Errorf("duplicate menu: %d %v", code, body)
	}
	if code, body, _ := rest("POST", "/wp/v2/menus", map[string]any{"description": "x"}); code != http.StatusBadRequest || body["code"] != "rest_missing_callback_param" {
		t.Errorf("menu without name: %d %v", code, body)
	}

	// Items: a custom link, and a page under it.
	pool := getPool(t)
	var pageID int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM posts WHERE type = 'page' AND status = 'publish' ORDER BY id LIMIT 1`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	code, item, _ := rest("POST", "/wp/v2/menu-items", map[string]any{"menus": menuID, "title": "Home", "url": "https://example.test/", "classes": []string{"top", "home"}, "xfn": "me", "target": "_blank"})
	if code != http.StatusCreated {
		t.Fatalf("create item: %d %v", code, item)
	}
	customID := int64(item["id"].(float64))
	title := item["title"].(map[string]any)
	if title["raw"] != "Home" || title["rendered"] != "Home" || item["type"] != "custom" || item["type_label"] != "Custom Link" || item["object"] != "custom" ||
		item["url"] != "https://example.test/" || item["status"] != "publish" || item["menu_order"] != float64(1) || item["parent"] != float64(0) ||
		item["menus"] != float64(menuID) || item["target"] != "_blank" || item["invalid"] != false || item["object_id"] != float64(0) {
		t.Errorf("custom item = %v", item)
	}
	if cls, _ := item["classes"].([]any); len(cls) != 2 || cls[0] != "top" {
		t.Errorf("classes = %v", item["classes"])
	}
	if xfn, _ := item["xfn"].([]any); len(xfn) != 1 || xfn[0] != "me" {
		t.Errorf("xfn = %v", item["xfn"])
	}
	il := item["_links"].(map[string]any)
	if term := il["wp:term"].([]any)[0].(map[string]any); term["taxonomy"] != "nav_menu" || term["href"] != home+"/wp-json/wp/v2/menus?menu_items="+itoa64(customID) {
		t.Errorf("wp:term link = %v", term)
	}
	if about := il["about"].([]any)[0].(map[string]any)["href"]; about != home+"/wp-json/wp/v2/types/nav_menu_item" {
		t.Errorf("item about = %v", about)
	}
	code, pageItem, _ := rest("POST", "/wp/v2/menu-items", map[string]any{"menus": menuID, "type": "post_type", "object": "page", "object_id": pageID, "parent": customID})
	if code != http.StatusCreated {
		t.Fatalf("create page item: %d %v", code, pageItem)
	}
	pageItemID := int64(pageItem["id"].(float64))
	if pageItem["type_label"] != "Page" || pageItem["object"] != "page" || pageItem["object_id"] != float64(pageID) || pageItem["parent"] != float64(customID) ||
		pageItem["menu_order"] != float64(2) || pageItem["title"].(map[string]any)["raw"] != "Sample Page" || pageItem["url"] != home+"/sample-page/" {
		t.Errorf("page item = %v", pageItem)
	}
	if obj := pageItem["_links"].(map[string]any)["wp:menu-item-object"].([]any)[0].(map[string]any); obj["post_type"] != "page" || obj["href"] != home+"/wp-json/wp/v2/pages/"+itoa64(pageID) {
		t.Errorf("wp:menu-item-object = %v", obj)
	}
	// Validation.
	if code, body, _ := rest("POST", "/wp/v2/menu-items", map[string]any{"title": "x", "url": "https://example.test/"}); code != http.StatusBadRequest || body["code"] != "invalid_menu_id" {
		t.Errorf("item without menu: %d %v", code, body)
	}
	if code, body, _ := rest("POST", "/wp/v2/menu-items", map[string]any{"menus": menuID, "url": "https://example.test/"}); code != http.StatusBadRequest || body["code"] != "rest_title_required" {
		t.Errorf("custom item without title: %d %v", code, body)
	}
	if code, body, _ := rest("POST", "/wp/v2/menu-items", map[string]any{"menus": menuID, "type": "post_type", "object": "page", "object_id": 999999}); code != http.StatusBadRequest || body["code"] != "rest_post_invalid_id" {
		t.Errorf("item with bad post: %d %v", code, body)
	}
	if code, body, _ := rest("POST", "/wp/v2/menu-items", map[string]any{"menus": menuID, "type": "taxonomy", "object": "category", "object_id": 999999}); code != http.StatusBadRequest || body["code"] != "rest_term_invalid_id" {
		t.Errorf("item with bad term: %d %v", code, body)
	}
	if code, body, _ := rest("POST", "/wp/v2/menu-items", map[string]any{"menus": menuID, "title": "x", "url": "https://example.test/", "parent": 999999}); code != http.StatusBadRequest || body["code"] != "rest_post_invalid_parent" {
		t.Errorf("item with bad parent: %d %v", code, body)
	}

	// Listing by menu, ordered by menu_order; the menus filter is honoured.
	code, _, list := rest("GET", "/wp/v2/menu-items?menus="+itoa64(menuID)+"&per_page=100", nil)
	if code != http.StatusOK || len(list) != 2 || list[0].(map[string]any)["id"] != float64(customID) || list[1].(map[string]any)["id"] != float64(pageItemID) {
		t.Errorf("list items: %d %v", code, list)
	}
	if _, _, list := rest("GET", "/wp/v2/menu-items?menus=999999", nil); len(list) != 0 {
		t.Errorf("items of a missing menu: %v", list)
	}
	if _, _, list := rest("GET", "/wp/v2/menu-items?menus="+itoa64(menuID)+"&menu_order=2", nil); len(list) != 1 || list[0].(map[string]any)["id"] != float64(pageItemID) {
		t.Errorf("menu_order filter: %v", list)
	}
	// Update: move the page item to the top.
	code, updated, _ := rest("POST", "/wp/v2/menu-items/"+itoa64(pageItemID), map[string]any{"menu_order": 1, "parent": 0, "title": "Sample"})
	if code != http.StatusOK || updated["menu_order"] != float64(1) || updated["parent"] != float64(0) || updated["title"].(map[string]any)["raw"] != "Sample" {
		t.Errorf("update item: %d %v", code, updated)
	}
	if code, m, _ := rest("POST", "/wp/v2/menu-items/"+itoa64(customID), map[string]any{"menu_order": 2}); code != http.StatusOK || m["menu_order"] != float64(2) {
		t.Errorf("update custom item: %d %v", code, m)
	}
	if _, _, list := rest("GET", "/wp/v2/menu-items?menus="+itoa64(menuID)+"&orderby=menu_order", nil); len(list) != 2 || list[0].(map[string]any)["id"] != float64(pageItemID) {
		t.Errorf("order after update: %v", list)
	}
	if code, got, _ := rest("GET", "/wp/v2/menu-items/"+itoa64(customID), nil); code != http.StatusOK || got["id"] != float64(customID) {
		t.Errorf("get item: %d %v", code, got)
	}
	if code, body, _ := rest("GET", "/wp/v2/menu-items/999999", nil); code != http.StatusNotFound || body["code"] != "rest_post_invalid_id" {
		t.Errorf("missing item: %d %v", code, body)
	}

	// Menu locations come from the active theme and show the assigned menu.
	code, locs, _ := rest("GET", "/wp/v2/menu-locations", nil)
	if code != http.StatusOK {
		t.Fatalf("menu-locations: %d %v", code, locs)
	}
	theme := srv.activeTheme()
	if len(locs) != len(theme.MenuLocations) {
		t.Errorf("locations = %v, theme has %v", locs, theme.MenuLocations)
	}
	first := theme.MenuLocations[0]
	if l := locs[first.Slug].(map[string]any); l["name"] != first.Slug || l["description"] != first.Name || l["menu"] != float64(0) {
		t.Errorf("location %s = %v", first.Slug, l)
	}
	if err := srv.store.SetMenuLocations(context.Background(), map[string]int64{first.Slug: menuID}); err != nil {
		t.Fatal(err)
	}
	code, loc, _ := rest("GET", "/wp/v2/menu-locations/"+first.Slug, nil)
	if code != http.StatusOK || loc["menu"] != float64(menuID) {
		t.Errorf("assigned location: %d %v", code, loc)
	}
	if wm := loc["_links"].(map[string]any)["wp:menu"].([]any)[0].(map[string]any); wm["href"] != home+"/wp-json/wp/v2/menus/"+itoa64(menuID) {
		t.Errorf("wp:menu link = %v", wm)
	}
	if code, body, _ := rest("GET", "/wp/v2/menu-locations/nope", nil); code != http.StatusNotFound || body["code"] != "rest_menu_location_invalid" {
		t.Errorf("unknown location: %d %v", code, body)
	}
	// The menu now reports its location, and the menus collection filters work.
	if code, m, _ := rest("GET", "/wp/v2/menus/"+itoa64(menuID), nil); code != http.StatusOK || m["locations"].([]any)[0] != first.Slug {
		t.Errorf("menu with location: %d %v", code, m)
	}
	if _, _, list := rest("GET", "/wp/v2/menus?slug=main", nil); len(list) != 1 {
		t.Errorf("menus?slug: %v", list)
	}
	if _, _, list := rest("GET", "/wp/v2/menus?search=zzz", nil); len(list) != 0 {
		t.Errorf("menus?search: %v", list)
	}
	if _, _, list := rest("GET", "/wp/v2/menus?menu_items="+itoa64(customID), nil); len(list) != 1 || list[0].(map[string]any)["id"] != float64(menuID) {
		t.Errorf("menus?menu_items: %v", list)
	}
	// Locations can be set through the menu too; unknown ones are refused.
	if code, m, _ := rest("POST", "/wp/v2/menus/"+itoa64(menuID), map[string]any{"locations": []string{}}); code != http.StatusOK || len(m["locations"].([]any)) != 0 {
		t.Errorf("unassign through menu: %d %v", code, m)
	}
	if code, body, _ := rest("POST", "/wp/v2/menus/"+itoa64(menuID), map[string]any{"locations": []string{"no-such-location"}}); code != http.StatusBadRequest || body["code"] != "invalid_menu_location" {
		t.Errorf("bad location: %d %v", code, body)
	}

	// Deleting needs force.
	if code, body, _ := rest("DELETE", "/wp/v2/menu-items/"+itoa64(customID), nil); code != http.StatusNotImplemented || body["code"] != "rest_trash_not_supported" {
		t.Errorf("delete item without force: %d %v", code, body)
	}
	if code, body, _ := rest("DELETE", "/wp/v2/menu-items/"+itoa64(customID)+"?force=true", nil); code != http.StatusOK || body["deleted"] != true || body["previous"].(map[string]any)["id"] != float64(customID) {
		t.Errorf("delete item: %d %v", code, body)
	}
	if code, body, _ := rest("DELETE", "/wp/v2/menus/"+itoa64(menuID), nil); code != http.StatusNotImplemented || body["code"] != "rest_trash_not_supported" {
		t.Errorf("delete menu without force: %d %v", code, body)
	}
	if code, body, _ := rest("DELETE", "/wp/v2/menus/"+itoa64(menuID)+"?force=true", nil); code != http.StatusOK || body["deleted"] != true {
		t.Errorf("delete menu: %d %v", code, body)
	}
	if code, body, _ := rest("GET", "/wp/v2/menus/"+itoa64(menuID), nil); code != http.StatusNotFound || body["code"] != "rest_term_invalid" {
		t.Errorf("deleted menu: %d %v", code, body)
	}
	if _, _, list := rest("GET", "/wp/v2/menu-items?menus="+itoa64(menuID), nil); len(list) != 0 {
		t.Errorf("items survive their menu: %v", list)
	}
}
