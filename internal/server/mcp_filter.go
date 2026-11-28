package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// mcpToolRoute is the REST route a tool reaches, with {placeholders} left as plain words.
func mcpToolRoute(t *mcpTool) string {
	return mcpRouteFor(t.Path)
}

// mcpRouteCaps are the capabilities a route's handlers need, by prefix (longest wins); a route
// without an entry is not filtered by capability (its handler checks per object).
var mcpRouteCaps = map[string]string{
	"/pursae/v1/plugins": "activate_plugins", "/pursae/v1/themes": "switch_themes", "/pursae/v1/cache": "manage_options",
	"/pursae/v1/seo": "manage_options", "/pursae/v1/options": "manage_options", "/pursae/v1/headless": "manage_options",
	"/wp/v2/settings": "manage_options", "/wp/v2/users": "list_users", "/pursae/v1/mcp/activity": "manage_options",
	"/pursae/v1/health": "manage_options", "/pursae/v1/csp": "manage_options", "/pursae/v1/backups": "manage_options", "/pursae/v1/updates": "update_plugins",
	"/wc/v3/products": "edit_products", "/wc/v3/orders": "edit_shop_orders", "/web-builder/v2": "edit_posts",
	"/wc/v3/reports": "view_woocommerce_reports", "/wp/v2/users/me": "read",
	// Same capabilities as the handlers (goo_rest_v3_*, rest_pursae_forum, forum_site_templates,
	// gontact_submit, seogo_editor), so tools/list does not show a subscriber admin-only tools
	// and their schemas.
	"/wc/v3/products/categories": "manage_product_terms", "/wc/v3/coupons": "edit_shop_coupons",
	"/wc/v3/settings": "manage_woocommerce", "/wc/v3/payment_gateways": "manage_woocommerce", "/wc/v3/shipping": "manage_woocommerce",
	"/pursae/v1/forum/forums": "manage_options", "/pursae/v1/forum/templates": "edit_theme_options",
	"/contact-form-7/v1": "edit_posts", "/seogo/v1": "edit_posts",
}

// mcpWriteCaps are the capabilities a tool that changes something (any method but GET) needs, by
// route prefix (longest wins), on top of mcpRouteCaps. A subscriber can read public posts and comment
// with the tools, but is not offered create_post, delete_media or moderate_comment, which its REST
// calls would refuse anyway. Routes whose handler decides per object (users/{id}: one's own profile)
// have no entry.
var mcpWriteCaps = map[string]string{
	"/wp/v2/posts": "edit_posts", "/wp/v2/pages": "edit_pages", "/wp/v2/media": "upload_files", "/pursae/v1/media": "upload_files",
	"/wp/v2/x": "manage_categories", "/wp/v2/menus": "edit_theme_options", "/wp/v2/menu-items": "edit_theme_options",
	"/wp/v2/comments/x": "moderate_comments", "/seogo/v1": "edit_posts", "/wp/v2/users": "create_users",
	"/wp/v2/users/x": "read", "/contact-form-7/v1": "publish_pages",
}

// mcpCapFor is the capability of the longest prefix of route in caps, or "".
func mcpCapFor(caps map[string]string, route string) string {
	best, cp := "", ""
	for p, k := range caps {
		if (route == p || strings.HasPrefix(route, p+"/")) && len(p) > len(best) {
			best, cp = p, k
		}
	}
	return cp
}

// mcpToolAvailable reports whether the caller can use a tool: the scopes of the credential cover its
// route, the user holds the capability it needs, and the built-in plugin it belongs to is active.
// tools/call checks this before running a tool; the REST layer checks the route again.
func (s *Server) mcpToolAvailable(c *gin.Context, t *mcpTool) bool {
	if t.special == "info" || t.special == "rest" {
		return true // site_info; rest_request (the REST layer checks the route it reaches)
	}
	route := mcpToolRoute(t)
	if !restScopeAllows(c, route) {
		return false
	}
	if strings.HasPrefix(route, "/seogo/v1") || strings.HasPrefix(route, "/pursae/v1/seo") {
		if !s.seogoOn() {
			return false
		}
	}
	if strings.HasPrefix(route, "/pursae/v1/headless") && !s.hgOn() {
		return false
	}
	if strings.HasPrefix(route, "/wc/v3") && !s.gooOn() {
		return false
	}
	if strings.HasPrefix(route, "/web-builder/v2") && !s.wbOn() && !t.listAlways {
		return false
	}
	if u := restCurrent(c); u != nil {
		if cp := mcpCapFor(mcpRouteCaps, route); cp != "" && !u.Can(cp, nil) {
			return false
		}
		if t.Method != http.MethodGet {
			if cp := mcpCapFor(mcpWriteCaps, route); cp != "" && !u.Can(cp, nil) {
				return false
			}
		}
	}
	return true
}
