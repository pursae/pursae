package server

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"html"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/web"
)

// Goocommerce is the native e-commerce plugin: a WooCommerce clone (products, cart, checkout, orders,
// coupons, taxes, shipping, payments, reports, the Store and REST APIs) with WooCommerce's premium
// extensions built in as modules (Goocommerce › Extensions). Like every native plugin it is off on
// a fresh install (native_goocommerce); while it is off its routes answer 404, its menus, shortcodes
// and assets are gone, and the products, orders and settings stay in the database.
//
// Front-end markup keeps WooCommerce's templates and CSS classes so themes made for it style the store.

const (
	gooSlug    = "goocommerce"
	gooSetting = "native_goocommerce"
	gooVersion = "1.0.0"
	gooAssets  = "/wp-content/plugins/goocommerce/assets"
	gooNonce   = "goocommerce-settings"
)

// gooOn reports whether Goocommerce is active: one cached option read.
func (s *Server) gooOn() bool { return s.settings.Bool(gooSetting) }

// gooOnly is the middleware of Goocommerce's own routes: 404 while the plugin is off.
func (s *Server) gooOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.gooOn() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// gooExtOn reports whether a premium extension module is switched on (Goocommerce › Extensions).
func (s *Server) gooExtOn(module string) bool {
	if !s.gooOn() {
		return false
	}
	on, _ := s.gooOptMap("extensions")[module].(bool)
	return on
}

// Pages ---------------------------------------------------------------------------------------------

// gooPages are the pages WooCommerce creates on activation (wc_create_pages), with their shortcodes.
var gooPages = []struct{ Key, Slug, Title, Content string }{
	{"shop", "shop", "Shop", ""},
	{"cart", "cart", "Cart", "<!-- wp:shortcode -->[goocommerce_cart]<!-- /wp:shortcode -->"},
	{"checkout", "checkout", "Checkout", "<!-- wp:shortcode -->[goocommerce_checkout]<!-- /wp:shortcode -->"},
	{"myaccount", "my-account", "My account", "<!-- wp:shortcode -->[goocommerce_my_account]<!-- /wp:shortcode -->"},
}

// gooPageID is wc_get_page_id(): shop, cart, checkout, myaccount, terms.
func (s *Server) gooPageID(key string) int64 { return int64(s.gooOptInt(key + "_page_id")) }

// gooPageURL is wc_get_page_permalink().
func (s *Server) gooPageURL(ctx context.Context, key string) string {
	if id := s.gooPageIDIn(ctx, key); id > 0 { // Golylang: the page in the current language (goo_golylang.go)
		if p, err := s.store.GetPost(ctx, id); err == nil && p.Status == "publish" {
			return s.links(ctx).Permalink(p)
		}
	}
	return s.links(ctx).HomeURL("/")
}

// gooShopURL is the shop page URL (the product archive).
func (s *Server) gooShopURL(ctx context.Context) string {
	if s.gooPageID("shop") > 0 {
		return s.gooPageURL(ctx, "shop")
	}
	return s.links(ctx).HomeURL("/?post_type=product")
}

// gooEndpointURL is wc_get_endpoint_url() on a store page.
func (s *Server) gooEndpointURL(ctx context.Context, page, endpoint, value string) string {
	u := strings.TrimRight(s.gooPageURL(ctx, page), "/")
	if endpoint == "" {
		return u + "/"
	}
	u += "/" + endpoint + "/"
	if value != "" {
		u += url.PathEscape(value) + "/"
	}
	return u
}

// gooAccountURL is wc_get_account_endpoint_url().
func (s *Server) gooAccountURL(ctx context.Context, endpoint string) string {
	if endpoint == "" || endpoint == "dashboard" {
		return s.gooPageURL(ctx, "myaccount")
	}
	ep := s.gooOptString("myaccount_" + strings.ReplaceAll(endpoint, "-", "_") + "_endpoint")
	if endpoint == "customer-logout" {
		ep = s.gooOptString("logout_endpoint")
	}
	if ep == "" {
		ep = endpoint
	}
	return s.gooEndpointURL(ctx, "myaccount", ep, "")
}

// gooCreatePages creates the missing store pages and stores their ids.
func (s *Server) gooCreatePages(ctx context.Context, authorID int64) error {
	for _, pg := range gooPages {
		if id := s.gooPageID(pg.Key); id > 0 {
			if p, err := s.store.GetPost(ctx, id); err == nil && p.Status != "trash" {
				continue
			}
		}
		// Reuse a page that already carries the shortcode (or the slug of the shop).
		var existing int64
		if pg.Content != "" {
			short := pg.Content[strings.Index(pg.Content, "[") : strings.Index(pg.Content, "]")+1]
			_ = s.store.Pool.QueryRow(ctx, `SELECT id FROM posts WHERE type = 'page' AND status = 'publish' AND content LIKE '%' || $1 || '%' ORDER BY id LIMIT 1`, short).Scan(&existing)
		} else {
			_ = s.store.Pool.QueryRow(ctx, `SELECT id FROM posts WHERE type = 'page' AND status = 'publish' AND slug = $1 ORDER BY id LIMIT 1`, pg.Slug).Scan(&existing)
		}
		if existing == 0 {
			p := &store.Post{Type: "page", Slug: pg.Slug, Title: pg.Title, Content: pg.Content, Status: "publish", AuthorID: authorID,
				CommentStatus: "closed", PingStatus: "closed", Meta: map[string]any{}}
			if err := s.store.InsertPost(ctx, p); err != nil {
				return err
			}
			existing = p.ID
		}
		if err := s.gooSetOpts(ctx, map[string]any{pg.Key + "_page_id": float64(existing)}); err != nil {
			return err
		}
	}
	return nil
}

// Activation ------------------------------------------------------------------------------------------

const (
	gooDailyJob  = "goocommerce_daily"
	gooHourlyJob = "goocommerce_hourly"
	gooWeeklyJob = "goocommerce_weekly" // weekly store summary e-mail
)

// gooActivate switches the plugin on: pages, version, jobs.
func (s *Server) gooActivate(c *gin.Context) error {
	ctx := c.Request.Context()
	var uid int64
	if u := currentUser(c); u != nil {
		uid = u.ID
	}
	if err := s.gooCreatePages(ctx, uid); err != nil {
		return err
	}
	vals := map[string]any{gooSetting: true}
	if s.gooOptString("version") == "" {
		// First activation: the store e-mail sender and stock recipient default to the site's.
		vals[gooOptPrefix+"version"] = gooVersion
		vals[gooOptPrefix+"email_from_name"] = s.settings.String("blogname")
		vals[gooOptPrefix+"email_from_address"] = s.settings.String("admin_email")
		vals[gooOptPrefix+"stock_email_recipient"] = s.settings.String("admin_email")
	}
	if err := s.settings.Set(ctx, vals); err != nil {
		return err
	}
	for name, rec := range map[string]string{gooDailyJob: "daily", gooHourlyJob: "hourly", gooWeeklyJob: "weekly"} {
		_, _ = s.store.Pool.Exec(ctx, `INSERT INTO jobs(name, run_at, recurrence)
			SELECT $1, now() + interval '5 minutes', $2 WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE name = $1 AND recurrence <> '')`, name, rec)
	}
	s.configureCache() // the store pages join the cache exclusions; rebuilding also empties the cache
	return nil
}

// gooDeactivate switches the plugin off; data and settings stay.
func (s *Server) gooDeactivate(ctx context.Context) error {
	if err := s.settings.Set(ctx, map[string]any{gooSetting: false}); err != nil {
		return err
	}
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM jobs WHERE name = ANY($1)`, []string{gooDailyJob, gooHourlyJob, gooWeeklyJob})
	s.configureCache()
	return nil
}

// gooPluginRow is Goocommerce's row on the Plugins screen.
func (s *Server) gooPluginRow() pluginRow {
	r := pluginRow{Slug: gooSlug, Name: "Pursae Commerce", Version: builtinPluginVersion, Like: "WooCommerce", Native: true, Active: s.gooOn(),
		Description: "Everything you need to sell online: products of every kind, cart, checkout, orders, coupons, taxes, shipping zones, payments, reports and a REST API, plus premium modules (subscriptions, memberships, bookings, bundles, add-ons, gift cards and more) you switch on when you need them. Compatible with WooCommerce themes and data shapes.",
		Author:      "Pursae"}
	if r.Active {
		r.SettingsURL = "/wp-admin/goocommerce?page=settings"
	}
	return r
}

// gooPluginAction is the Activate / Deactivate link of the Plugins screen.
func (s *Server) gooPluginAction(c *gin.Context, action string) {
	var err error
	if action == "activate" {
		err = s.gooActivate(c)
	} else {
		err = s.gooDeactivate(c)
	}
	if err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape(err.Error()))
		return
	}
	c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
}

// Admin menu --------------------------------------------------------------------------------------------

// gooAdminPages are the pages of /wp-admin/goocommerce?page=… and their capability.
var gooAdminPages = map[string]struct{ Title, Cap string }{
	"":           {"Home", "manage_woocommerce"},
	"orders":     {"Orders", "edit_shop_orders"},
	"customers":  {"Customers", "manage_woocommerce"},
	"reports":    {"Reports", "view_woocommerce_reports"},
	"settings":   {"Settings", "manage_woocommerce"},
	"status":     {"Status", "manage_woocommerce"},
	"extensions": {"Extensions", "manage_woocommerce"},
	"coupons":    {"Coupons", "edit_shop_coupons"},
	"marketing":  {"Marketing", "manage_woocommerce"},
	"attributes": {"Attributes", "manage_product_terms"},
	"reviews":    {"Reviews", "moderate_comments"},
	"analytics":  {"Analytics", "view_woocommerce_reports"},
	"themes":     {"Store Themes", "manage_woocommerce"}, // goo_admin_themes.go
}

// gooMenu adds WooCommerce's menus after Comments: Goocommerce, Products, Analytics, Marketing.
func (s *Server) gooMenu(menu []*menuTop, u *store.User, screen string) []*menuTop {
	if !s.gooOn() || u == nil || !(u.Can("manage_woocommerce", nil) || u.Can("edit_products", nil)) {
		return menu
	}
	sub := func(top *menuTop, slug, title, cap string, match ...string) {
		if !u.Can(cap, nil) {
			return
		}
		ms := &menuSub{Slug: slug, Title: template.HTML(title), First: len(top.Sub) == 0, Current: slug == screen}
		for _, m := range match {
			if m == screen {
				ms.Current = true
			}
		}
		top.Current = top.Current || ms.Current
		top.Sub = append(top.Sub, ms)
	}
	var tops []*menuTop
	pending := s.gooOrderStatusCounts(context.Background())["processing"]
	orders := "Orders"
	if pending > 0 {
		orders += ` <span class="awaiting-mod update-plugins count-` + strconv.Itoa(pending) + `"><span class="processing-count">` + strconv.Itoa(pending) + `</span></span>`
	}
	if u.Can("manage_woocommerce", nil) {
		top := &menuTop{Slug: "goocommerce", Title: "Pursae Commerce", ID: "toplevel_page_goocommerce", Icon: "dashicons-cart"}
		sub(top, "goocommerce", "Home", "manage_woocommerce")
		sub(top, "goocommerce?page=orders", orders, "edit_shop_orders")
		sub(top, "goocommerce?page=customers", "Customers", "manage_woocommerce")
		sub(top, "goocommerce?page=reports", "Reports", "view_woocommerce_reports")
		sub(top, "goocommerce?page=settings", "Settings", "manage_woocommerce")
		sub(top, "goocommerce?page=status", "Status", "manage_woocommerce")
		sub(top, "goocommerce?page=extensions", "Extensions", "manage_woocommerce")
		sub(top, "goocommerce?page=themes", "Store Themes", "manage_woocommerce")
		for _, m := range gooExtMenuItems {
			if s.gooExtOn(m.Module) {
				sub(top, "goocommerce?page="+m.Page, m.Title, "manage_woocommerce")
			}
		}
		tops = append(tops, top)
	}
	if u.Can("edit_products", nil) {
		top := &menuTop{Slug: "edit?post_type=product", Title: "Products", ID: "menu-posts-product", Icon: "dashicons-archive"}
		sub(top, "edit?post_type=product", "All Products", "edit_products")
		sub(top, "post-new?post_type=product", "Add New Product", "edit_products")
		sub(top, "edit-tags?taxonomy=product_cat&post_type=product", "Categories", "manage_product_terms", "edit-tags?taxonomy=product_cat")
		sub(top, "edit-tags?taxonomy=product_tag&post_type=product", "Tags", "manage_product_terms", "edit-tags?taxonomy=product_tag")
		sub(top, "goocommerce?page=attributes", "Attributes", "manage_product_terms")
		sub(top, "goocommerce?page=reviews", "Reviews", "moderate_comments")
		tops = append(tops, top)
	}
	if u.Can("view_woocommerce_reports", nil) && s.gooOptBool("analytics_enabled") {
		top := &menuTop{Slug: "goocommerce?page=analytics", Title: "Analytics", ID: "toplevel_page_goo-analytics", Icon: "dashicons-chart-bar"}
		for _, r := range []struct{ Path, Title string }{{"", "Overview"}, {"products", "Products"}, {"revenue", "Revenue"}, {"orders", "Orders"},
			{"variations", "Variations"}, {"categories", "Categories"}, {"coupons", "Coupons"}, {"taxes", "Taxes"}, {"downloads", "Downloads"},
			{"stock", "Stock"}, {"customers", "Customers"}, {"subscriptions", "Subscriptions"}, {"bookings", "Bookings"}, {"gift-cards", "Gift Cards"}} {
			if m, ok := gooAnalyticsModulePages[r.Path]; ok && !s.gooExtOn(m) {
				continue
			}
			slug := "goocommerce?page=analytics"
			if r.Path != "" {
				slug += "&path=" + r.Path
			}
			sub(top, slug, r.Title, "view_woocommerce_reports")
		}
		tops = append(tops, top)
	}
	if u.Can("edit_shop_coupons", nil) {
		top := &menuTop{Slug: "goocommerce?page=marketing", Title: "Marketing", ID: "toplevel_page_goo-marketing", Icon: "dashicons-megaphone"}
		sub(top, "goocommerce?page=marketing", "Overview", "manage_woocommerce")
		sub(top, "goocommerce?page=coupons", "Coupons", "edit_shop_coupons")
		tops = append(tops, top)
	}
	for _, t := range tops {
		if len(t.Sub) > 0 && !strings.HasPrefix(t.Slug, "goocommerce") && t.Sub[0].Slug != t.Slug {
			t.Slug = t.Sub[0].Slug
		}
	}
	// After Comments, in a group of its own (WooCommerce adds a separator at position 55).
	at := len(menu)
	for i, m := range menu {
		if m.ID == "menu-comments" {
			at = i + 1
			break
		}
	}
	block := append([]*menuTop{{Separator: true}}, tops...)
	out := append([]*menuTop{}, menu[:at]...)
	out = append(out, block...)
	out = append(out, menu[at:]...)
	// Re-mark first / last per group.
	for i, m := range out {
		if m.Separator {
			continue
		}
		m.First = i == 0 || out[i-1].Separator
		m.Last = i == len(out)-1 || out[i+1].Separator
	}
	return out
}

// gooExtMenuItem is a Goocommerce submenu entry of a module (Subscriptions, Bookings, …).
type gooExtMenuItem struct{ Module, Page, Title string }

var gooExtMenuItems []gooExtMenuItem

// gooExtAdminPages are module admin screens of /wp-admin/goocommerce?page=…
var gooExtAdminPages = map[string]func(s *Server, c *gin.Context){}

// Routes ---------------------------------------------------------------------------------------------

// registerGoocommerce wires the plugin's routes; every one answers 404 while it is off.
func (s *Server) registerGoocommerce(r *gin.Engine, admin *gin.RouterGroup) {
	s.static(r, gooAssets, "goocommerce/assets", s.gooOnly())
	admin.GET("/goocommerce", s.gooOnly(), s.gooAdmin)
	admin.POST("/goocommerce", s.gooOnly(), s.gooAdmin)
}

// gooAdmin dispatches /wp-admin/goocommerce?page=…
func (s *Server) gooAdmin(c *gin.Context) {
	u := currentUser(c)
	page := c.Query("page")
	if h, ok := gooExtAdminPages[page]; ok {
		if !u.Can("manage_woocommerce", nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
			return
		}
		h(s, c)
		return
	}
	def, ok := gooAdminPages[page]
	if !ok {
		c.String(http.StatusNotFound, "Sorry, you are not allowed to access this page.")
		return
	}
	if !u.Can(def.Cap, nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	switch page {
	case "":
		s.gooAdminHome(c)
	case "orders":
		s.gooAdminOrders(c)
	case "customers":
		s.gooAdminCustomers(c)
	case "reports":
		s.gooAdminReports(c)
	case "settings":
		s.gooAdminSettings(c)
	case "status":
		s.gooAdminStatus(c)
	case "extensions":
		s.gooAdminExtensions(c)
	case "coupons":
		s.gooAdminCoupons(c)
	case "marketing":
		s.gooAdminMarketing(c)
	case "attributes":
		s.gooAdminAttributes(c)
	case "reviews":
		s.gooAdminReviews(c)
	case "analytics":
		s.gooAdminAnalytics(c)
	case "themes":
		s.gooAdminThemes(c)
	}
}

// gooAdminScreen intercepts the core post screens for products (edit.php?post_type=product,
// post-new.php?post_type=product, post.php?post=<product>): true when it served the request.
func (s *Server) gooAdminScreen(c *gin.Context, screen string) bool {
	if !s.gooOn() {
		return false
	}
	switch screen {
	case "edit":
		if c.Query("post_type") == "product" {
			s.gooAdminProducts(c)
			return true
		}
	case "post-new":
		if c.Query("post_type") == "product" {
			s.gooAdminProductEdit(c, 0)
			return true
		}
	case "post":
		id, _ := strconv.ParseInt(c.Query("post"), 10, 64)
		if id == 0 {
			id, _ = strconv.ParseInt(c.PostForm("post_ID"), 10, 64)
		}
		if id > 0 {
			var typ string
			_ = s.store.Pool.QueryRow(c, `SELECT type FROM posts WHERE id = $1`, id).Scan(&typ)
			if typ == "product" {
				s.gooAdminProductEdit(c, id)
				return true
			}
		}
	}
	return false
}

// gooTaxLabels are the admin labels of the product taxonomies (taxLabelsFor).
func gooTaxLabels(tax string) (taxLabels, bool) {
	switch {
	case tax == "product_cat":
		return taxLabels{Tax: tax, Name: "Product categories", Singular: "Category", AddNew: "Add new category", Search: "Search categories",
			NotFound: "No categories found", ParentLabel: "Parent category", Screen: "edit-tags?taxonomy=product_cat&post_type=product",
			Description:  "Product categories for your store can be managed here. To change the order of categories on the front-end you can drag and drop to sort them.",
			Hierarchical: true, FilterParam: "product_cat"}, true
	case tax == "product_tag":
		return taxLabels{Tax: tax, Name: "Product tags", Singular: "Tag", AddNew: "Add new tag", Search: "Search tags", NotFound: "No tags found",
			Screen: "edit-tags?taxonomy=product_tag&post_type=product", FilterParam: "product_tag"}, true
	case tax == "product_shipping_class":
		return taxLabels{Tax: tax, Name: "Shipping classes", Singular: "Shipping class", AddNew: "Add new shipping class", Search: "Search shipping classes",
			NotFound: "No shipping classes found.", Screen: "edit-tags?taxonomy=product_shipping_class&post_type=product"}, true
	case strings.HasPrefix(tax, "pa_"):
		name := strings.TrimPrefix(tax, "pa_")
		return taxLabels{Tax: tax, Name: name, Singular: name, AddNew: "Add new " + name, Search: "Search " + name, NotFound: "No terms found",
			Screen: "edit-tags?taxonomy=" + tax + "&post_type=product", FilterParam: tax}, true
	}
	return taxLabels{}, false
}

// Cache ------------------------------------------------------------------------------------------------

// gooCacheExcludes adds the store's private pages and cookies to the page cache exclusions
// (cart, checkout and my account are never cached; a visitor with items in the cart, a store session or
// a wishlist bypasses the cache).
func (s *Server) gooCacheExcludes(paths, cookies []string) ([]string, []string) {
	if !s.gooOn() {
		return paths, cookies
	}
	ctx := context.Background()
	for _, key := range []string{"cart", "checkout", "myaccount"} {
		if id := s.gooPageID(key); id > 0 {
			for _, tid := range s.gooPllPagePaths(ctx, id) { // Golylang: every translation of the page
				if p, err := s.store.GetPost(ctx, tid); err == nil {
					if u, err := url.Parse(s.links(ctx).Permalink(p)); err == nil && u.Path != "" && u.Path != "/" {
						paths = append(paths, strings.TrimRight(u.Path, "/")+"*")
					}
				}
			}
		}
	}
	// The wishlist cookie outlives the session cookie (365 days against 48 hours): a guest who has one
	// sees their own list and hearts, so their pages are neither served from nor stored in the cache.
	return append(paths, "/?goo-ajax*", "/wp-json/wc/*"), append(cookies, gooItemsCookie, gooSessionCookie, gooWLCookie)
}

// gooPurgeProduct drops the cached pages showing a product: its page, the shop, its categories and tags.
func (s *Server) gooPurgeProduct(ctx context.Context, p *gooProduct) {
	if p == nil {
		return
	}
	tags := []string{"goo:product:" + itoa64(p.ID), "goo:shop", "post:" + itoa64(p.ID), "home", "search"}
	if p.ParentID != 0 {
		tags = append(tags, "goo:product:"+itoa64(p.ParentID), "post:"+itoa64(p.ParentID))
	}
	id := p.ID
	if p.Type == gooTypeVariation && p.ParentID != 0 {
		id = p.ParentID
	}
	for _, tax := range []string{"product_cat", "product_tag"} {
		terms, _ := s.store.TermsForPosts(ctx, []int64{id}, tax)
		for _, t := range terms[id] {
			tags = append(tags, "term:"+itoa64(t.ID))
		}
	}
	s.purgeTags(tags...)
}

// Jobs ----------------------------------------------------------------------------------------------------

// gooDaily is the daily job: expired sessions, scheduled sales, held stock, data retention.
func (s *Server) gooDaily(ctx context.Context) error {
	if !s.gooOn() {
		return nil
	}
	// A failed cleanup is logged and the other daily steps still run.
	if err := s.gooCleanupSessions(ctx); err != nil {
		slog.Warn("goocommerce: session cleanup", "err", err)
	}
	s.gooCleanupDrafts(ctx)
	if s.gooExtOn(paoKey) {
		s.paoCleanupUploads(ctx, 24*time.Hour)
	}
	s.gooScheduledSales(ctx)
	s.gooRetention(ctx)
	s.gooFire(ctx, "daily", nil)
	return nil
}

// gooHourly cancels unpaid orders whose held stock expired (wc_cancel_unpaid_orders) and deletes
// expired stock reservations.
func (s *Server) gooHourly(ctx context.Context) error {
	if !s.gooOn() {
		return nil
	}
	if mins := s.gooOptInt("hold_stock_minutes"); mins > 0 && s.gooManageStock() {
		rows, err := s.store.Pool.Query(ctx, `SELECT id FROM goo_orders WHERE type = 'shop_order' AND status = 'pending' AND created_via = 'checkout'
			AND date_modified < now() - make_interval(mins => $1)`, mins)
		if err == nil {
			var ids []int64
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				if o, err := s.gooGetOrder(ctx, id); err == nil {
					// Only while it is still unpaid: a payment that arrived since the query wins.
					_ = s.gooSetStatusFrom(ctx, o, []string{"pending"}, "cancelled", "Unpaid order cancelled - time limit reached.", "")
				}
			}
		}
	}
	s.gooCleanupReservedStock(ctx)
	s.gooFire(ctx, "hourly", nil)
	return nil
}

// gooScheduledSales refreshes the stored active price of products whose sale started or ended
// (wc_scheduled_sales), so catalog sorting and filters see the right price.
func (s *Server) gooScheduledSales(ctx context.Context) {
	rows, err := s.store.Pool.Query(ctx, `SELECT post_id FROM goo_products WHERE (sale_from IS NOT NULL AND sale_from <= now() AND sale_from > now() - interval '2 days')
		OR (sale_to IS NOT NULL AND sale_to <= now() AND sale_to > now() - interval '2 days')`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		p, err := s.gooGetProduct(ctx, id)
		if err != nil {
			continue
		}
		if p.SaleTo != nil && time.Now().After(*p.SaleTo) {
			p.SalePrice, p.SaleFrom, p.SaleTo = nil, nil, nil // an ended sale is cleared, like WooCommerce
		}
		p.Price = p.activePrice(time.Now())
		_ = s.gooWriteProductData(ctx, p)
		if p.Type == gooTypeVariation {
			if parent := s.gooParent(ctx, p); parent != nil {
				_ = s.gooSyncVariable(ctx, parent)
			}
		}
		s.gooPurgeProduct(ctx, p)
	}
}

// gooRetention applies Accounts & Privacy › Personal data retention.
func (s *Server) gooRetention(ctx context.Context) {
	s.gooDeleteInactiveCustomers(ctx)
	for status, key := range map[string]string{"pending": "trash_pending_orders", "failed": "trash_failed_orders", "cancelled": "trash_cancelled_orders"} {
		if days := s.gooOptInt(key); days > 0 {
			_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_orders SET status = 'trash', meta = meta || jsonb_build_object('_trash_status', status)
				WHERE type = 'shop_order' AND status = $1 AND date_modified < now() - make_interval(days => $2)`, status, days)
		}
	}
	if months := s.gooOptInt("anonymize_completed_orders"); months > 0 {
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_orders SET billing = '{}', shipping = '{}', billing_email = '', customer_ip = '', user_agent = '',
			meta = meta || '{"_anonymized":true}' WHERE type = 'shop_order' AND status = 'completed' AND NOT (meta ? '_anonymized')
			AND date_completed < now() - make_interval(months => $1)`, months)
	}
}

// Shared helpers --------------------------------------------------------------------------------------------

// gooEsc is esc_html().
func gooEsc(v string) string { return html.EscapeString(v) }

// gooAssetVer is the ?ver= of the store's CSS and JS: the version plus a hash of the bundled assets,
// so browsers holding last week's copy (assets are cached for a week) fetch the new files after an
// upgrade even when the version number stayed the same.
var gooAssetVer = sync.OnceValue(func() string {
	h := sha1.New()
	_ = fs.WalkDir(web.FS(""), "goocommerce/assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := fs.ReadFile(web.FS(""), p); err == nil {
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return gooVersion + "." + hex.EncodeToString(h.Sum(nil))[:8]
})
