package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"wpgo/internal/webbuilder"
)

func TestWBDocFollowStyle(t *testing.T) {
	st := &webbuilder.SiteSettings{Brand: "#0f9b6c", FontPair: "modern", Corners: "sharp", BodyFont: "Inter"}
	follow := json.RawMessage(`{"sections":[],"site":{"brand":"#4058e3","fontPair":"elegant","corners":"soft","headingFont":"Lora","global":true,"x":1}}`)
	var got struct {
		Site map[string]any `json:"site"`
	}
	if err := json.Unmarshal(wbDocFollowStyle(follow, st), &got); err != nil {
		t.Fatal(err)
	}
	if got.Site["brand"] != "#0f9b6c" || got.Site["fontPair"] != "modern" || got.Site["bodyFont"] != "Inter" || got.Site["global"] != true || got.Site["x"] != float64(1) {
		t.Errorf("following doc: %v", got.Site)
	}
	if _, ok := got.Site["headingFont"]; ok {
		t.Errorf("a font the style does not set stayed: %v", got.Site)
	}
	own := json.RawMessage(`{"sections":[],"site":{"brand":"#4058e3","fontPair":"elegant","corners":"soft"}}`)
	if string(wbDocFollowStyle(own, st)) != string(own) {
		t.Error("a doc that does not follow the style changed")
	}
	if string(wbDocFollowStyle(follow, nil)) != string(follow) {
		t.Error("no style changed the doc")
	}
	for name, raw := range map[string]string{
		"missing corners": `{"brand":"#000","fontPair":"modern"}`,
		"bad pair":        `{"brand":"#000","fontPair":"comic","corners":"soft"}`,
		"unsafe brand":    `{"brand":"url(javascript:x)","fontPair":"modern","corners":"soft"}`,
		"not an object":   `"blue"`,
	} {
		if _, err := wbParseSiteStyle([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWebBuilderSiteStyle(t *testing.T) {
	srv, _, admin := wbrStart(t)
	ctx := context.Background()
	const path = "/web-builder/v2/site-settings"
	if code, v, _ := admin.do(http.MethodGet, path, nil, nil); code != http.StatusOK || wbrObj(v)["site_style"] != nil {
		t.Fatalf("get: %d %v", code, v)
	}

	// Two current designs: one follows the site style, one has its own look.
	follow := wbrPost(t, srv, "page", "Follows", "publish", 1)
	own := wbrPost(t, srv, "page", "Own look", "publish", 1)
	for id, site := range map[int64]string{
		follow.ID: `{"brand":"#4058e3","fontPair":"elegant","corners":"soft","global":true}`,
		own.ID:    `{"brand":"#4058e3","fontPair":"elegant","corners":"soft"}`,
	} {
		if _, err := srv.store.Pool.Exec(ctx, `INSERT INTO web_builder_docs (post_id, doc, renderer) VALUES ($1, $2, $3)`,
			id, `{"sections":[],"site":`+site+`}`, webbuilder.RendererVersion); err != nil {
			t.Fatal(err)
		}
	}
	style := map[string]any{"brand": "#0f9b6c", "fontPair": "modern", "corners": "sharp"}
	code, v, _ := admin.do(http.MethodPut, path, map[string]any{"site_style": style}, nil)
	o := wbrObj(v)
	if code != http.StatusOK || wbrObj(o["site_style"])["brand"] != "#0f9b6c" || o["site_style_outdated"] != float64(1) {
		t.Fatalf("put: %d %v", code, v)
	}
	if code, v, _ := admin.do(http.MethodPut, path, map[string]any{"site_style": map[string]any{"brand": "red;}", "fontPair": "modern", "corners": "sharp"}}, nil); code != http.StatusBadRequest {
		t.Errorf("unsafe style: %d %v", code, v)
	}

	// The following design is outdated and loads with the style; the other is neither.
	_, v, _ = admin.do(http.MethodGet, "/web-builder/v2/outdated", nil, nil)
	items, _ := wbrObj(v)["items"].([]any)
	if len(items) != 1 || wbrObj(items[0])["id"] != float64(follow.ID) {
		t.Fatalf("outdated: %v", v)
	}
	_, v, _ = admin.do(http.MethodGet, wbrDocPath(follow.ID, ""), nil, nil)
	if site := wbrObj(wbrObj(wbrObj(v)["doc"])["site"]); site["brand"] != "#0f9b6c" || site["global"] != true {
		t.Errorf("following doc loads with %v", site)
	}
	_, v, _ = admin.do(http.MethodGet, wbrDocPath(own.ID, ""), nil, nil)
	if site := wbrObj(wbrObj(wbrObj(v)["doc"])["site"]); site["brand"] != "#4058e3" {
		t.Errorf("own doc loads with %v", site)
	}

	// Once the following design holds the style (published again), nothing is outdated.
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE web_builder_docs SET doc = $2 WHERE post_id = $1`, follow.ID,
		`{"sections":[],"site":{"brand":"#0f9b6c","fontPair":"modern","corners":"sharp","global":true}}`); err != nil {
		t.Fatal(err)
	}
	if _, v, _ = admin.do(http.MethodGet, "/web-builder/v2/outdated", nil, nil); wbrObj(v)["total"] != float64(0) {
		t.Errorf("after regenerate: %v", v)
	}
	// null removes the style.
	if code, v, _ := admin.do(http.MethodPut, path, map[string]any{"site_style": nil}, nil); code != http.StatusOK || wbrObj(v)["site_style"] != nil {
		t.Errorf("clear: %d %v", code, v)
	}
}
