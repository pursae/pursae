package server

import (
	"context"
	"net/http"
	"testing"
)

// Product meta_data may not write Web Builder trusted HTML (printed without KSES) or other core-internal
// keys, for any caller; ordinary and "_"-prefixed integration meta still round-trips.
func TestT2RestMetaProductTrustedMetaRefused(t *testing.T) {
	base, c, srv := gooSite(t)
	api := gooAdminAPI(t, base, c)
	for _, call := range []func(method, path string, body any) gooResp{api, func(method, path string, body any) gooResp {
		return gooReq(t, &http.Client{}, method, base+"/wp-json/wc/v3"+path, body, gooCommerceToken(t, base, c))
	}} {
		r := call("POST", "/products", map[string]any{"name": "Meta", "type": "simple", "meta_data": []any{
			map[string]any{"key": "_wb_html_desc", "value": "<script>alert(1)</script>"},
			map[string]any{"key": "_wb_theme", "value": "x"},
			map[string]any{"key": "_wp_page_template", "value": "evil.html"},
			map[string]any{"key": "_integration_ref", "value": "abc"},
			map[string]any{"key": "colour", "value": "red"},
		}})
		if r.Code != http.StatusCreated && r.Code != http.StatusOK {
			t.Fatalf("create: %d %s", r.Code, r.Raw)
		}
		p, err := srv.gooGetProduct(context.Background(), int64(gooInt(gooJ(r.Body, "id"))))
		if err != nil {
			t.Fatal(err)
		}
		got := p.Meta
		for _, k := range []string{"_wb_html_desc", "_wb_theme", "_wp_page_template"} {
			if _, ok := got[k]; ok {
				t.Errorf("product meta_data wrote protected key %q", k)
			}
		}
		if got["_integration_ref"] != "abc" || got["colour"] != "red" {
			t.Errorf("ordinary meta did not round-trip: %v", got)
		}
	}
	if !gooProtectedPostMetaKey("_WB_HTML_x") || gooProtectedPostMetaKey("_sku_ref") {
		t.Error("gooProtectedPostMetaKey")
	}
}
