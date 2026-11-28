package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A password-protected product keeps its text behind the password: the product page shows the
// password form, the Store API leaves the descriptions empty until the wp-postpass cookie is sent.
func TestGooProtectedProductHidesText(t *testing.T) {
	base, c, srv := gooSite(t)
	ctx := context.Background()
	api := gooAdminAPI(t, base, c)
	r := api("POST", "/products", map[string]any{"name": "Locked", "type": "simple", "regular_price": "10",
		"description": "<p>SECRETBODY</p>", "short_description": "SECRETSHORT"})
	id := int64(gooInt(gooJ(r.Body, "id")))
	if id == 0 {
		t.Fatalf("create: %d %s", r.Code, r.Raw)
	}
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE posts SET password = 'pw1' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	get := func(path, cookie string) string {
		req, _ := http.NewRequest("GET", base+path, nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: postPassCookie, Value: cookie})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	page := get("/product/locked/", "")
	if strings.Contains(page, "SECRET") || !strings.Contains(page, "post_password") {
		i := strings.Index(page, "SECRET")
		t.Errorf("product page shows protected text or no password form: %d %s", i, page[max(0, i-400):min(len(page), i+100)])
	}
	for _, path := range []string{fmt.Sprintf("/wp-json/wc/store/v1/products/%d", id), "/wp-json/wc/store/v1/products"} {
		if b := get(path, ""); strings.Contains(b, "SECRET") {
			t.Errorf("%s leaks protected text: %s", path, b)
		}
	}
	if b := get(fmt.Sprintf("/wp-json/wc/store/v1/products/%d", id), srv.postPassValue("pw1")); !strings.Contains(b, "SECRETBODY") {
		t.Errorf("valid postpass cookie: %s", b)
	}
}
