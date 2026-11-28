package server

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"wpgo/internal/caps"
	"wpgo/internal/store"
)

// tools/list offers a role only the tools its REST calls would allow (SEC28-004): a subscriber reads
// and comments, an author writes posts and media but does not moderate or manage users.
func TestMCPToolsFilteredByRole(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	visible := func(g caps.Grants) map[string]bool {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/wp-json/pursae/v1/mcp", nil)
		c.Set(restUserKey, &store.User{ID: 42, Login: "u", Roles: []string{"x"}, Caps: g})
		out := map[string]bool{}
		for _, tool := range mcpTools {
			if srv.mcpToolAvailable(c, tool) {
				out[tool.Name] = true
			}
		}
		return out
	}
	sub := visible(caps.Grants{"read": true})
	for _, name := range []string{"list_posts", "get_post", "list_comments", "create_comment", "get_me", "site_info"} {
		if !sub[name] {
			t.Errorf("subscriber lacks %s", name)
		}
	}
	for _, name := range []string{"create_post", "delete_post", "create_page", "upload_media", "delete_media", "replace_media_file",
		"moderate_comment", "delete_comment", "create_term", "create_menu", "create_user", "seo_update_meta", "list_plugins"} {
		if sub[name] {
			t.Errorf("subscriber offered %s", name)
		}
	}
	author := visible(caps.Grants{"read": true, "edit_posts": true, "publish_posts": true, "upload_files": true, "edit_published_posts": true, "delete_posts": true})
	for _, name := range []string{"create_post", "update_post", "upload_media", "delete_media"} {
		if !author[name] {
			t.Errorf("author lacks %s", name)
		}
	}
	for _, name := range []string{"create_page", "moderate_comment", "create_user", "create_term", "update_settings"} {
		if author[name] {
			t.Errorf("author offered %s", name)
		}
	}
}

// Commerce settings/coupons/gateways/shipping, forum administration, contact forms and SEO meta
// are not listed to a subscriber (SEC #10): tools/list uses the handlers' capabilities.
func TestMCPToolsListHidesAdminSiteTools(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	visible := func(g caps.Grants) map[string]bool {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/wp-json/pursae/v1/mcp", nil)
		c.Set(restUserKey, &store.User{ID: 42, Login: "u", Roles: []string{"x"}, Caps: g})
		out := map[string]bool{}
		for _, tool := range mcpTools {
			if srv.mcpToolAvailable(c, tool) {
				out[tool.Name] = true
			}
		}
		return out
	}
	known := map[string]bool{}
	for _, tool := range mcpTools {
		known[tool.Name] = true
	}
	sub := visible(caps.Grants{"read": true, "forum_participate": true})
	for _, name := range []string{"commerce_update_settings", "commerce_get_settings", "commerce_update_payment_gateway", "commerce_list_payment_gateways",
		"commerce_create_coupon", "commerce_create_shipping_zone", "commerce_create_category", "forum_list", "forum_create", "forum_update", "forum_delete",
		"forum_list_templates", "forum_set_template", "forum_delete_template", "contact_list_forms", "contact_create_form", "contact_update_form", "seo_get_meta"} {
		if sub[name] {
			t.Errorf("subscriber offered %s", name)
		}
	}
	author := visible(caps.Grants{"read": true, "edit_posts": true})
	if known["contact_list_forms"] && !author["contact_list_forms"] {
		t.Error("author (edit_posts) lacks contact_list_forms")
	}
	if author["contact_create_form"] {
		t.Error("author offered contact_create_form (needs publish_pages)")
	}
}
