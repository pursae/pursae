package server

import (
	"bytes"
	"context"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/cache"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Goocommerce's storefront: shop and product archives, the single product page, cart, checkout,
// order received, My Account, the mini-cart and the store's forms and AJAX actions.
//
// Store URLs render inside the active theme the way WooCommerce's theme-compatibility mode does:
// the main query is a page whose content is the store markup (a synthetic page for products and
// archives, the real Cart / Checkout / My account pages for those). The markup keeps WooCommerce's
// templates, classes and field names so themes made for WooCommerce style it.
//
//   goo_front.go           routing, rendering inside the theme, the core hooks, shared helpers
//   goo_front_loop.go      price / rating / stock HTML, the product loop, archives, filters, widgets
//   goo_front_single.go    the single product page, variations, tabs, reviews, structured data
//   goo_front_cart.go      cart page, mini-cart, add to cart, the cart forms and the AJAX actions
//   goo_front_checkout.go  checkout, order received, order pay, order details
//   goo_front_account.go   My Account, downloads, order tracking
//   goo_front_shortcodes.go the shortcodes

const (
	gooCtxView   = "goo.front.view"
	gooCtxRender = "goo.front.render"
	gooRenderTag = "goo_render"
)

// gooRenderMarker is the content of a synthetic store page: a shortcode block (no wpautop) that
// gooShortcodes replaces with the pre-rendered markup.
const gooRenderMarker = "<!-- wp:shortcode -->[" + gooRenderTag + "]<!-- /wp:shortcode -->"

// gooFrontView describes the store request being rendered (is_shop(), is_product(), is_cart() …).
type gooFrontView struct {
	Kind     string // shop, product, product_cat, product_tag, product_attr, cart, checkout, myaccount
	Product  *gooProduct
	Term     *store.Term
	Paged    int
	Search   string
	Endpoint string // canonical endpoint key: order-received, order-pay, orders, view-order, downloads, edit-address, edit-account, <module slug>
	Value    string
	BaseURL  string // the archive URL paging and ordering build on
	PageID   int64
}

func gooViewFrom(c *gin.Context) *gooFrontView {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(gooCtxView); ok {
		fv, _ := v.(*gooFrontView)
		return fv
	}
	return nil
}

// Core hooks -----------------------------------------------------------------------------------------

// gooPostMarkers are the form fields of the store's POST forms (the site accepts those POSTs).
var gooPostMarkers = []string{"add-to-cart", "goo_module", "goo_review", "woocommerce-cart-nonce", "woocommerce-shipping-calculator-nonce",
	"woocommerce-process-checkout-nonce", "woocommerce-login-nonce", "woocommerce-register-nonce", "woocommerce-edit-address-nonce",
	"save-account-details-nonce", "woocommerce-pay-nonce", "woocommerce-order-tracking-nonce", "woocommerce-apply-coupon-nonce",
	"woocommerce-lost-password-nonce", "woocommerce-reset-password-nonce"}

// gooAcceptsPost reports whether a POST to a site URL is one of the store's forms or AJAX actions.
func (s *Server) gooAcceptsPost(c *gin.Context) bool {
	if !s.gooOn() {
		return false
	}
	if gooAjaxAction(c) != "" || gooWCAPIName(c) != "" { // AJAX, payment provider callbacks (goo_gateway_api.go)
		return true
	}
	// Only form values are read here: 1 MB in memory, files beyond it spool to the temp directory.
	// The body is bounded by the largest store form first (a product add-on file plus the fields).
	if c.Request.Body != nil && c.Request.Body != http.NoBody {
		limit := max(int64(max(s.gooExtInt(paoKey, "max_upload_mb"), 1))<<20+2<<20, 8<<20)
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	}
	if err := c.Request.ParseMultipartForm(1 << 20); err != nil && c.Request.PostForm == nil { //nolint:gosec // G120: the body is bounded just above
		_ = c.Request.ParseForm()
	}
	for _, k := range gooPostMarkers {
		if _, ok := c.Request.PostForm[k]; ok {
			return true
		}
	}
	return false
}

// gooAjaxAction is the action of /?goo-ajax=… (and WooCommerce's /?wc-ajax=…).
func gooAjaxAction(c *gin.Context) string {
	q := c.Request.URL.Query()
	if a := q.Get("goo-ajax"); a != "" {
		return a
	}
	return q.Get("wc-ajax")
}

// gooSiteIntercept serves the store's URLs; false lets the core resolve the request.
func (s *Server) gooSiteIntercept(c *gin.Context, path string) bool {
	if !s.gooOn() {
		return false
	}
	for _, r := range gooFrontRoutes { // phase-2 modules first
		if r(s, c, path) {
			return true
		}
	}
	if a := gooAjaxAction(c); a != "" {
		s.gooAjax(c, a)
		return true
	}
	if s.gooHasSessionCookie(c) {
		c.Header(cache.HeaderControl, "no-store") // pages built for a visitor with a cart or notices stay private
	}
	q := c.Request.URL.Query()
	if q.Get("download_file") != "" && q.Get("key") != "" {
		s.gooDownload(c)
		return true
	}
	if c.Request.Method == http.MethodPost {
		if s.gooHandlePost(c, path) {
			return true
		}
	} else if q.Get("add-to-cart") != "" {
		s.gooAddToCartGET(c)
		return true
	} else if s.gooHandleGetAction(c) {
		return true
	}
	return s.gooFrontRoute(c, path)
}

// gooBodyClasses is the store's part of body_class().
func (s *Server) gooBodyClasses(c *gin.Context, q *siteQuery) []string {
	if !s.gooOn() {
		return nil
	}
	out := []string{"theme-" + s.activeTheme().Slug, "woocommerce-no-js"}
	fv := gooViewFrom(c)
	if fv == nil {
		if q != nil && q.Post != nil && s.gooHasShortcode(q.Post.Content) {
			out = append(out, "woocommerce-page")
		}
		return out
	}
	switch fv.Kind {
	case "shop":
		out = append(out, "woocommerce-shop", "woocommerce", "woocommerce-page")
		if fv.Search != "" {
			out = append(out, "search-results")
		}
	case "product":
		out = append(out, "product-template-default", "single", "single-product", "postid-"+itoa64(fv.Product.ID), "woocommerce", "woocommerce-page")
	case "product_cat", "product_tag", "product_attr":
		if fv.Term != nil {
			out = append(out, "archive", "tax-"+fv.Term.Taxonomy, "term-"+fv.Term.Slug, "term-"+itoa64(fv.Term.ID))
		}
		out = append(out, "woocommerce", "woocommerce-page")
	case "cart":
		out = append(out, "woocommerce-cart", "woocommerce-page")
	case "checkout":
		out = append(out, "woocommerce-checkout", "woocommerce-page")
		if fv.Endpoint == "order-received" {
			out = append(out, "woocommerce-order-received")
		}
		if fv.Endpoint == "order-pay" {
			out = append(out, "woocommerce-order-pay")
		}
	case "myaccount":
		out = append(out, "woocommerce-account", "woocommerce-page")
		if fv.Endpoint != "" {
			out = append(out, "woocommerce-"+fv.Endpoint)
		}
	}
	return out
}

// gooIsStorePage reports whether the request shows store markup (styles and scripts are printed):
// a store view, a page with store shortcodes, or a Web Builder page whose Commerce widgets render
// store markup (ctxWBShop, set by wbFill).
func (s *Server) gooIsStorePage(c *gin.Context, q *siteQuery) bool {
	if gooViewFrom(c) != nil || (c != nil && c.GetBool(ctxWBShop)) {
		return true
	}
	return q != nil && q.Post != nil && s.gooHasShortcode(q.Post.Content)
}

// gooHead prints the store's stylesheets (store pages and pages with store shortcodes) and the
// product's structured data.
func (s *Server) gooHead(c *gin.Context, q *siteQuery) string {
	if !s.gooOn() || !s.gooIsStorePage(c, q) {
		return ""
	}
	var b strings.Builder
	b.WriteString(gooStyleTags())
	if fv := gooViewFrom(c); fv != nil && fv.Kind == "product" && fv.Product != nil && !s.seogoOwnsProductSchema() {
		b.WriteString(s.gooProductJSONLD(c, fv.Product))
	}
	return b.String()
}

// gooStyleTags are the store's stylesheets.
func gooStyleTags() string {
	v := "?ver=" + gooAssetVer()
	return `<link rel="stylesheet" id="woocommerce-layout-css" href="` + gooAssets + `/css/goocommerce-layout.css` + v + `" media="all" />` + "\n" +
		`<link rel="stylesheet" id="woocommerce-smallscreen-css" href="` + gooAssets + `/css/goocommerce-smallscreen.css` + v + `" media="only screen and (max-width: 768px)" />` + "\n" +
		`<link rel="stylesheet" id="woocommerce-general-css" href="` + gooAssets + `/css/goocommerce.css` + v + `" media="all" />` + "\n" +
		`<noscript><style>.woocommerce-product-gallery{ opacity: 1 !important; }</style></noscript>` + "\n"
}

// gooFooter prints the store notice, the module footer markup and the store scripts.
func (s *Server) gooFooter(c *gin.Context, q *siteQuery) string {
	if !s.gooOn() {
		return ""
	}
	ctx := c.Request.Context()
	var b strings.Builder
	if s.gooOptBool("demo_store") {
		notice := s.gooT(ctx, s.gooOptString("demo_store_notice")) // Golylang: Languages › Translations
		if strings.TrimSpace(notice) == "" {
			notice = "This is a demo store for testing purposes &mdash; no orders shall be fulfilled."
		} else {
			s.gooKeep(c, wp.KsesPost(notice))
		}
		id := gooMD5(notice)[:12]
		b.WriteString(s.gooL10n(c, `<p role="complementary" aria-label="Store notice" class="woocommerce-store-notice demo_store" data-notice-id="`+id+`" style="display:none;">`+
			wp.KsesPost(notice)+` <a role="button" href="#" class="woocommerce-store-notice__dismiss-link">Dismiss</a></p>`) + "\n")
	}
	b.WriteString(string(s.gooPageHTML(c, "footer")))
	// woocommerce-no-js → woocommerce-js
	b.WriteString(`<script` + cspNonceAttr() + `>(function(){var c=document.body.className;c=c.replace(/woocommerce-no-js/,'woocommerce-js');document.body.className=c;})();</script>` + "\n")
	// The AJAX endpoints sit on the language home (Multi Language: /de/, de.example.com/ or /?lang=de),
	// so cart, checkout and order requests carry the visitor's language and stay same-origin.
	ajaxBase := s.links(ctx).Front()
	ajaxSep := "?"
	if strings.Contains(ajaxBase, "?") {
		ajaxSep = "&"
	} else if !strings.HasSuffix(ajaxBase, "/") {
		ajaxBase += "/"
	}
	params := map[string]any{
		"ajax_url":                         ajaxBase,
		"goo_ajax_url":                     ajaxBase + ajaxSep + "goo-ajax=%%endpoint%%",
		"wc_ajax_url":                      ajaxBase + ajaxSep + "wc-ajax=%%endpoint%%",
		"cart_url":                         s.gooPageURL(ctx, "cart"),
		"checkout_url":                     s.gooPageURL(ctx, "checkout"),
		"is_cart":                          false,
		"cart_redirect_after_add":          s.gooOptBool("cart_redirect_after_add"),
		"enable_ajax_add_to_cart":          s.gooOptBool("enable_ajax_add_to_cart"),
		"i18n_view_cart":                   "View cart",
		"items_cookie":                     gooItemsCookie,
		"hash_cookie":                      gooCartHashCook,
		"currency_symbol":                  gooCurrencySymbol(s.gooCurrency()),
		"i18n_no_matching_variations_text": "Sorry, no products matched your selection. Please choose a different combination.",
		"i18n_make_a_selection_text":       "Please select some product options before adding this product to your cart.",
		"i18n_unavailable_text":            "Sorry, this product is unavailable. Please choose a different combination.",
		"i18n_reset_alert_text":            "Your selection has been reset. Please select some product options before adding this product to your cart.",
		"i18n_required_rating_text":        "Please select a rating",
		"review_rating_required":           s.gooOptBool("review_rating_required"),
		"update_order_review_nonce":        s.nonce(c, "update-order-review"),
		"apply_coupon_nonce":               s.nonce(c, "apply-coupon"),
		"remove_coupon_nonce":              s.nonce(c, "remove-coupon"),
		"update_shipping_method_nonce":     s.nonce(c, "update-shipping-method"),
		"option_guest_checkout":            s.gooOptBool("enable_guest_checkout"),
		"i18n_checkout_error":              "There was an error processing your order. Please check for any charges in your payment method and review your order history before placing the order again.",
	}
	params["i18n_password_show"], params["i18n_password_hide"] = "Show password", "Hide password"
	for k, v := range params {
		if str, ok := v.(string); ok && strings.HasPrefix(k, "i18n_") {
			params[k] = s.gooL10nText(ctx, str)
		}
	}
	fv := gooViewFrom(c)
	if fv != nil && fv.Kind == "cart" {
		params["is_cart"] = true
	}
	raw, _ := jsonMarshalNoEscape(params)
	b.WriteString(`<script` + cspNonceAttr() + ` id="goocommerce-params">var goocommerce_params = ` + gooScriptSafe(raw) + `; var wc_add_to_cart_params = goocommerce_params;</script>` + "\n")
	b.WriteString(gooScriptTag("cart-fragments")) // header counts on cached pages
	if s.gooIsStorePage(c, q) {
		b.WriteString(s.gooStoreScripts(c, q))
	}
	return b.String()
}

func gooScriptTag(name string) string {
	return `<script` + cspNonceAttr() + ` src="` + gooAssets + `/js/frontend/` + name + `.js?ver=` + gooAssetVer() + `" id="goo-` + name + `-js" defer></script>` + "\n"
}

// gooStoreScripts are the scripts of a page that shows store markup (gooIsStorePage); the
// goocommerce_params script and cart-fragments come before them (gooFooter).
func (s *Server) gooStoreScripts(c *gin.Context, q *siteQuery) string {
	ctx := c.Request.Context()
	fv := gooViewFrom(c)
	var b strings.Builder
	js := func(name string) { b.WriteString(gooScriptTag(name)) }
	{
		js("woocommerce")
		js("add-to-cart")
		js("single-product")        // gallery, tabs, rating stars (also [product_page])
		js("add-to-cart-variation") // variation forms (also [product_page])
		if s.gooNeedsCountrySelect(c, q) {
			b.WriteString(`<script` + cspNonceAttr() + ` id="goo-country-select-params">var goo_country_select_params = ` + gooScriptSafe([]byte(s.gooStatesJSON(ctx))) + `;</script>` + "\n")
			js("country-select")
		}
		if fv != nil && fv.Kind == "cart" || q != nil && q.Post != nil && strings.Contains(q.Post.Content, "_cart]") {
			js("cart")
		}
		if fv != nil && fv.Kind == "checkout" || q != nil && q.Post != nil && strings.Contains(q.Post.Content, "_checkout]") {
			js("checkout")
		}
	}
	return b.String()
}

// gooNeedsCountrySelect: the cart (shipping calculator), checkout and address forms swap states.
func (s *Server) gooNeedsCountrySelect(c *gin.Context, q *siteQuery) bool {
	if fv := gooViewFrom(c); fv != nil && (fv.Kind == "cart" || fv.Kind == "checkout" || fv.Kind == "myaccount") {
		return true
	}
	if q != nil && q.Post != nil {
		for _, sc := range []string{"_cart]", "_checkout]", "_my_account]"} {
			if strings.Contains(q.Post.Content, sc) {
				return true
			}
		}
	}
	return false
}

var gooStatesOnce struct {
	sync.Once
	js string
}

// gooStatesJSON is wc_country_select_params: countries (country → {code: name}) and the prompt.
func (s *Server) gooStatesJSON(ctx context.Context) string {
	gooStatesOnce.Do(func() {
		m := map[string]map[string]string{}
		for cc, list := range gooStates {
			mm := map[string]string{}
			for _, st := range list {
				mm[st.Code] = st.Name
			}
			m[cc] = mm
		}
		raw, _ := jsonMarshalNoEscape(m)
		gooStatesOnce.js = string(raw)
	})
	prompt, _ := jsonMarshalNoEscape(s.gooL10nText(ctx, "Select an option…"))
	return `{"countries":` + gooStatesOnce.js + `,"i18n_select_state_text":` + string(prompt) + `}`
}

// gooCacheTags tags cached store pages so product and stock changes purge them.
func (s *Server) gooCacheTags(c *gin.Context, q *siteQuery) []string {
	if !s.gooOn() {
		return nil
	}
	fv := gooViewFrom(c)
	if fv == nil {
		if q != nil && q.Post != nil && s.gooHasShortcode(q.Post.Content) {
			return []string{"goo:shop"}
		}
		return nil
	}
	tags := []string{"goo:shop"}
	switch fv.Kind {
	case "product":
		tags = []string{"goo:product:" + itoa64(fv.Product.ID)}
		if terms, err := s.gooProductTerms(c, fv.Product.ID); err == nil {
			for _, t := range terms {
				tags = append(tags, "term:"+itoa64(t.ID))
			}
		}
	case "product_cat", "product_tag", "product_attr":
		if fv.Term != nil {
			tags = append(tags, "term:"+itoa64(fv.Term.ID))
		}
	}
	return tags
}

// Routing ---------------------------------------------------------------------------------------------

// gooPagePaths are the paths of store pages ("shop", "my-account"; "" when one has none), loaded with one query (the router checks them
// all on every front-end request).
func (s *Server) gooPagePaths(ctx context.Context, keys ...string) map[string]string {
	out := map[string]string{}
	ids := map[string]int64{}
	var list []int64
	for _, k := range keys {
		if id := s.gooPageIDIn(ctx, k); id > 0 {
			ids[k] = id
			list = append(list, id)
		}
	}
	if len(list) == 0 {
		return out
	}
	posts, err := s.store.PostsByIDs(ctx, list)
	if err != nil {
		return out
	}
	for k, id := range ids {
		out[k] = s.gooPagePathOf(ctx, posts[id])
	}
	return out
}

// gooPagePathOf is the router path of a published store page.
func (s *Server) gooPagePathOf(ctx context.Context, p *store.Post) string {
	if p == nil || p.Status != "publish" {
		return ""
	}
	u, err := url.Parse(s.gooPllSitePath(ctx, s.links(ctx).Permalink(p))) // Golylang: without the language part
	if err != nil || u.RawQuery != "" {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

func gooAtoi(v string) int { n, _ := strconv.Atoi(strings.TrimSpace(v)); return n }

func gooAtoi64(v string) int64 { n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64); return n }

// gooFrontRoute maps a GET to a store view.
func (s *Server) gooFrontRoute(c *gin.Context, path string) bool {
	ctx := c.Request.Context()
	s.gooCaptureAttribution(c)
	v := c.Request.URL.Query()
	paged := 1
	if n := gooAtoi(v.Get("paged")); n > 1 {
		paged = n
	}
	p := path
	if m := pagedRe.FindStringSubmatch(p); m != nil {
		paged, p = gooAtoi(m[2]), m[1]
	}
	if p == "" {
		switch {
		case v.Get("post_type") == "product":
			s.gooServeArchive(c, &gooFrontView{Kind: "shop", Paged: paged, Search: strings.TrimSpace(v.Get("s"))})
			return true
		case v.Get("product_cat") != "" || v.Get("product_tag") != "" || (v.Get("taxonomy") != "" && v.Get("term") != ""):
			tax, slug := "product_cat", v.Get("product_cat")
			if slug == "" && v.Get("product_tag") != "" {
				tax, slug = "product_tag", v.Get("product_tag")
			}
			if slug == "" {
				tax, slug = v.Get("taxonomy"), v.Get("term")
				if tax != "product_cat" && tax != "product_tag" && !strings.HasPrefix(tax, "pa_") {
					return false
				}
			}
			t, err := s.store.TermBySlug(ctx, tax, slug)
			if err != nil {
				return false
			}
			return s.gooServeTerm(c, t, paged)
		case v.Get("product") != "":
			if pr, err := s.store.PostBySlug(ctx, "product", v.Get("product")); err == nil {
				return s.gooServeProduct(c, pr.ID)
			}
			return false
		case v.Get("p") != "" || v.Get("page_id") != "":
			id := gooAtoi64(v.Get("p") + v.Get("page_id"))
			var typ string
			_ = s.store.Pool.QueryRow(ctx, `SELECT type FROM posts WHERE id = $1`, id).Scan(&typ)
			switch typ {
			case "product":
				return s.gooServeProduct(c, id)
			case "product_variation":
				if pr, err := s.gooGetProduct(ctx, id); err == nil && pr.Status == "publish" {
					c.Redirect(http.StatusMovedPermanently, s.gooPermalink(ctx, pr))
					return true
				}
				return false
			case "page":
				return s.gooServeStoreByID(c, id, paged)
			}
			return false
		}
		if v.Get("s") == "" && s.settings.String("show_on_front") == "page" && int64(s.settings.Int("page_on_front")) == s.gooPageID("shop") && s.gooPageID("shop") > 0 {
			s.gooServeArchive(c, &gooFrontView{Kind: "shop", Paged: paged, PageID: s.gooPageIDIn(ctx, "shop")})
			return true
		}
		return false
	}
	if base := s.gooProductBase(); strings.HasPrefix(p, base+"/") {
		slug := strings.Split(strings.TrimPrefix(p, base+"/"), "/")[0]
		if pr, err := s.store.PostBySlug(ctx, "product", slug); err == nil {
			return s.gooServeProduct(c, pr.ID)
		}
		return false
	}
	catBase := strings.Trim(defaultStr(s.gooOptString("category_base"), "product-category"), "/")
	if strings.HasPrefix(p, catBase+"/") {
		parts := strings.Split(strings.TrimPrefix(p, catBase+"/"), "/")
		t, err := s.store.TermBySlug(ctx, "product_cat", parts[len(parts)-1])
		if err != nil {
			return false
		}
		return s.gooServeTerm(c, t, paged)
	}
	tagBase := strings.Trim(defaultStr(s.gooOptString("tag_base"), "product-tag"), "/")
	if strings.HasPrefix(p, tagBase+"/") {
		t, err := s.store.TermBySlug(ctx, "product_tag", strings.TrimPrefix(p, tagBase+"/"))
		if err != nil {
			return false
		}
		return s.gooServeTerm(c, t, paged)
	}
	if t := s.gooAttributeArchive(ctx, p); t != nil {
		return s.gooServeTerm(c, t, paged)
	}
	paths := s.gooPagePaths(ctx, "shop", "cart", "checkout", "myaccount")
	if sp := paths["shop"]; sp != "" && p == sp {
		s.gooServeArchive(c, &gooFrontView{Kind: "shop", Paged: paged, PageID: s.gooPageIDIn(ctx, "shop")})
		return true
	}
	for _, key := range []string{"cart", "checkout", "myaccount"} {
		pp := paths[key]
		if pp == "" {
			continue
		}
		if path == pp {
			return s.gooServeStorePage(c, key, "", "")
		}
		if strings.HasPrefix(path, pp+"/") && key != "cart" {
			rest := strings.Split(strings.TrimPrefix(path, pp+"/"), "/")
			if ep, ok := s.gooEndpointKey(key, rest[0]); ok {
				val := ""
				if len(rest) > 1 {
					val, _ = url.PathUnescape(rest[1])
				}
				return s.gooServeStorePage(c, key, ep, val)
			}
		}
	}
	return false
}

// gooAttributeArchive resolves /<attribute_base>/<attribute>/<term>/ of attributes with archives.
func (s *Server) gooAttributeArchive(ctx context.Context, p string) *store.Term {
	base := strings.Trim(s.gooOptString("attribute_base"), "/")
	if base != "" {
		if !strings.HasPrefix(p, base+"/") {
			return nil
		}
		p = strings.TrimPrefix(p, base+"/")
	}
	parts := strings.Split(p, "/")
	if len(parts) != 2 {
		return nil
	}
	a := s.gooAttributeByTaxonomy(ctx, "pa_"+parts[0])
	if a == nil || !a.HasArchives {
		return nil
	}
	t, err := s.store.TermBySlug(ctx, a.Taxonomy(), parts[1])
	if err != nil {
		return nil
	}
	return t
}

// gooEndpointKey maps a URL segment of the checkout / My account page to its endpoint.
func (s *Server) gooEndpointKey(page, seg string) (string, bool) {
	if seg == "" {
		return "", false
	}
	var keys []string
	if page == "checkout" {
		for _, k := range []string{"order-received", "order-pay"} {
			slug := s.gooOptString("checkout_" + map[string]string{"order-received": "order_received", "order-pay": "pay"}[k] + "_endpoint")
			if defaultStr(slug, k) == seg {
				return k, true
			}
		}
		return "", false
	}
	keys = []string{"orders", "view-order", "downloads", "edit-address", "edit-account", "payment-methods", "lost-password", "customer-logout"}
	for _, k := range keys {
		slug := s.gooOptString("myaccount_" + strings.ReplaceAll(k, "-", "_") + "_endpoint")
		if k == "customer-logout" {
			slug = s.gooOptString("logout_endpoint")
		}
		if defaultStr(slug, k) == seg {
			return k, true
		}
	}
	for _, ep := range s.gooModuleEndpoints() {
		if ep.Slug == seg {
			return ep.Slug, true
		}
	}
	return "", false
}

// gooEndpointSlug is the URL segment of an endpoint key (the options rename them).
func (s *Server) gooEndpointSlug(key string) string {
	switch key {
	case "order-received":
		return defaultStr(s.gooOptString("checkout_order_received_endpoint"), key)
	case "order-pay":
		return defaultStr(s.gooOptString("checkout_pay_endpoint"), key)
	case "customer-logout":
		return defaultStr(s.gooOptString("logout_endpoint"), key)
	case "orders", "view-order", "downloads", "edit-address", "edit-account", "payment-methods", "lost-password":
		return defaultStr(s.gooOptString("myaccount_"+strings.ReplaceAll(key, "-", "_")+"_endpoint"), key)
	}
	return key
}

// gooModuleEndpoints are the My Account tabs of the switched-on modules.
func (s *Server) gooModuleEndpoints() []*gooAccountEndpoint {
	var out []*gooAccountEndpoint
	for _, ep := range gooAccountEndpoints {
		if ep.Module == "" || s.gooExtOn(ep.Module) {
			out = append(out, ep)
		}
	}
	return out
}

// gooServeStoreByID serves ?page_id= of the shop / cart / checkout / My account pages (plain permalinks).
func (s *Server) gooServeStoreByID(c *gin.Context, id int64, paged int) bool {
	if id <= 0 {
		return false
	}
	v := c.Request.URL.Query()
	if s.gooPageIs(c, id, "shop") { // Golylang: or a translation of it
		s.gooServeArchive(c, &gooFrontView{Kind: "shop", Paged: paged, PageID: id})
		return true
	}
	for _, key := range []string{"cart", "checkout", "myaccount"} {
		if !s.gooPageIs(c, id, key) {
			continue
		}
		for _, cand := range []string{"order-received", "order-pay", "orders", "view-order", "downloads", "edit-address", "edit-account", "customer-logout", "lost-password"} {
			if v.Has(s.gooEndpointSlug(cand)) {
				return s.gooServeStorePage(c, key, cand, v.Get(s.gooEndpointSlug(cand)))
			}
		}
		return s.gooServeStorePage(c, key, "", "")
	}
	return false
}

// gooNoStore marks a private store response uncacheable.
func gooNoStore(c *gin.Context) {
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "Wed, 11 Jan 1984 05:00:00 GMT")
}

// gooServeStorePage renders the Cart, Checkout or My account page (their shortcodes read the view).
func (s *Server) gooServeStorePage(c *gin.Context, key, endpoint, value string) bool {
	ctx := c.Request.Context()
	page, err := s.store.GetPost(ctx, s.gooPageIDIn(ctx, key))
	if err != nil || page.Status != "publish" {
		return false
	}
	if (key == "checkout" || key == "myaccount") && s.gooForceSSL(c) {
		return true
	}
	gooNoStore(c)
	kind := key
	fv := &gooFrontView{Kind: kind, Endpoint: endpoint, Value: value, PageID: page.ID}
	c.Set(gooCtxView, fv)
	u := currentUser(c)
	switch key {
	case "checkout":
		if endpoint == "" && s.gooCartFor(c).IsEmpty() {
			s.gooSaveSession(c)
			c.Redirect(http.StatusFound, s.gooPageURL(ctx, "cart"))
			return true
		}
	case "myaccount":
		switch endpoint {
		case "customer-logout":
			if s.gooLogout(c) {
				return true
			}
		case "lost-password":
			// The store's own form (gooLostPasswordHTML): sending visitors to wp-login.php would give
			// away a hidden login address. The reset key may be in the URL: keep it out of Referer.
			c.Header("Referrer-Policy", "no-referrer")
		case "payment-methods":
			if u == nil {
				break
			}
		}
		if u == nil && endpoint != "" && endpoint != "customer-logout" && endpoint != "lost-password" {
			fv.Endpoint, fv.Value = "", "" // logged out: the login form on every endpoint
		}
	}
	copy := *page
	if t := s.gooEndpointTitle(endpoint, value); t != "" && (u != nil || key == "checkout" || endpoint == "lost-password") {
		copy.Title = s.gooL10nText(ctx, t)
	}
	q := &siteQuery{Kind: "page", Post: &copy, Paged: 1, Store: key}
	c.Set(ctxSiteQuery, q)
	s.renderSite(c, q)
	return true
}

// gooEndpointTitle is wc_page_endpoint_title().
func (s *Server) gooEndpointTitle(endpoint, value string) string {
	switch endpoint {
	case "order-received":
		return "Order received"
	case "order-pay":
		return "Pay for order"
	case "orders":
		if n := gooAtoi(value); n > 1 {
			return "Orders (page " + strconv.Itoa(n) + ")"
		}
		return "Orders"
	case "view-order":
		return "Order #" + value
	case "downloads":
		return "Downloads"
	case "edit-address":
		return "Addresses"
	case "edit-account":
		return "Account details"
	case "payment-methods":
		return "Payment methods"
	case "lost-password":
		return "Lost password"
	case "view-subscription": // untitled detail page (no menu tab): WCS titles it like view-order
		if value != "" {
			return "Subscription #" + value
		}
		return "Subscription"
	}
	for _, ep := range s.gooModuleEndpoints() {
		if ep.Slug == endpoint {
			return ep.Title
		}
	}
	return ""
}

// gooServeProduct renders a single product inside the theme.
func (s *Server) gooServeProduct(c *gin.Context, id int64) bool {
	ctx := c.Request.Context()
	p, err := s.gooGetProduct(ctx, id)
	if err != nil || p.Type == gooTypeVariation {
		return false
	}
	u := currentUser(c)
	switch p.Status {
	case "publish":
	case "private":
		if !u.Can("read_post", p.Post) {
			return false
		}
	case "draft", "pending", "future":
		if c.Query("preview") != "true" || !u.Can("edit_post", p.Post) {
			return false
		}
	default:
		return false
	}
	// Plain ?p= / ?product= URLs of a published product redirect to the pretty permalink.
	if c.Request.URL.Path == "/" && p.Status == "publish" && s.links(ctx).Pretty() && c.Query("preview") == "" {
		c.Redirect(http.StatusMovedPermanently, s.gooPermalink(ctx, p))
		return true
	}
	if denied, handled := s.gooGuardProduct(c, p.ID); denied { // Memberships: hidden / redirected products
		return handled
	}
	if s.gooPllObjectLang(c, pllPost, p.ID) { // Golylang: the product's language (or its own URL)
		return true
	}
	ctx = c.Request.Context()
	fv := &gooFrontView{Kind: "product", Product: p}
	c.Set(gooCtxView, fv)
	post := s.gooSyntheticPost(ctx, p.ID, p.Name, s.gooProductBase()+"/"+p.Slug, p.Post)
	var body string
	if s.passwordRequired(c, p.Post) {
		// WooCommerce's single-product template prints get_the_password_form() instead of the product
		// until the visitor entered the password (wp-postpass cookie).
		c.Header("Cache-Control", "no-cache, must-revalidate, max-age=0")
		body = `<div class="woocommerce">` + s.passwordForm(p.Post, s.links(ctx)) + `</div>`
	} else {
		body = s.gooSingleProductHTML(c, p)
		post.Excerpt = wp.StripTags(p.Post.Excerpt)
	}
	s.gooRenderSynthetic(c, post, body, 1)
	return true
}

// gooServeTerm renders a product category / tag / attribute archive.
func (s *Server) gooServeTerm(c *gin.Context, t *store.Term, paged int) bool {
	if s.pllTranslatedTaxonomy(t.Taxonomy) && s.gooPllObjectLang(c, pllTerm, t.ID) { // Golylang: the term's language (or its own URL)
		return true
	}
	kind := "product_attr"
	switch t.Taxonomy {
	case "product_cat":
		kind = "product_cat"
	case "product_tag":
		kind = "product_tag"
	}
	s.gooServeArchive(c, &gooFrontView{Kind: kind, Term: t, Paged: paged})
	return true
}

// gooServeArchive renders the shop or a product taxonomy archive.
func (s *Server) gooServeArchive(c *gin.Context, fv *gooFrontView) {
	ctx := c.Request.Context()
	if fv.Paged < 1 {
		fv.Paged = 1
	}
	c.Set(gooCtxView, fv)
	title, slug, id := s.gooL10nText(ctx, "Shop"), "shop", s.gooPageIDIn(ctx, "shop")
	if fv.PageID > 0 && s.gooPageIs(ctx, fv.PageID, "shop") {
		id = fv.PageID
	}
	var src *store.Post
	if id > 0 {
		if pg, err := s.store.GetPost(ctx, id); err == nil {
			title, slug, src = pg.Title, pg.Slug, pg
		}
	}
	switch {
	case fv.Term != nil:
		title = fv.Term.Name
		u, _ := url.Parse(s.gooTermLink(ctx, fv.Term))
		slug = strings.Trim(u.Path, "/")
		id = fv.Term.ID
		src = nil
	case fv.Search != "":
		title = gooEsc(s.gooL10nText(ctx, "Search results: “"+fv.Search+"”"))
	}
	fv.BaseURL = s.gooArchiveBase(c, fv)
	body := s.gooArchiveHTML(c, fv)
	if id <= 0 {
		id = 0
	}
	post := s.gooSyntheticPost(ctx, id, html.UnescapeString(title), slug, src)
	if fv.Term != nil {
		post.Excerpt = fv.Term.Description
	}
	s.gooRenderSynthetic(c, post, body, fv.Paged)
}

// gooSyntheticPost is the page the theme renders for a store view.
func (s *Server) gooSyntheticPost(ctx context.Context, id int64, title, slug string, src *store.Post) *store.Post {
	p := &store.Post{ID: id, Type: "page", Title: title, Slug: slug, Status: "publish", CommentStatus: "closed", PingStatus: "closed",
		Content: gooRenderMarker, Meta: map[string]any{"_goo_synthetic": true}, Format: "standard"}
	now := time.Now()
	p.PublishedAt, p.ModifiedAt, p.CreatedAt = &now, now, now
	if src != nil {
		p.AuthorID, p.PublishedAt, p.ModifiedAt, p.CreatedAt = src.AuthorID, src.PublishedAt, src.ModifiedAt, src.CreatedAt
		if src.Type == "page" {
			p.ParentID, p.Slug = src.ParentID, src.Slug
		}
	}
	return p
}

// gooRenderSynthetic renders pre-built store markup as the content of a page of the active theme.
func (s *Server) gooRenderSynthetic(c *gin.Context, post *store.Post, body string, paged int) {
	c.Set(gooCtxRender, s.gooL10n(c, body))
	s.gooSaveSession(c)
	q := &siteQuery{Kind: "page", Post: post, Paged: paged}
	if fv := gooViewFrom(c); fv != nil {
		q.Store = fv.Kind
		if fv.Term != nil {
			q.StoreTax, q.StoreTerm = fv.Term.Taxonomy, fv.Term.ID
		}
		switch {
		case fv.Product != nil:
			q.StoreURL = s.gooPermalink(c.Request.Context(), fv.Product)
		case fv.Search == "":
			q.StoreURL = fv.BaseURL
		}
		if fv.Search != "" {
			c.Header("Cache-Control", "no-cache, must-revalidate, max-age=0")
		}
	}
	c.Set(ctxSiteQuery, q)
	s.renderSite(c, q)
}

// gooTermLink is get_term_link() of the product taxonomies.
func (s *Server) gooTermLink(ctx context.Context, t *store.Term) string {
	return s.gooPllLink(ctx, "term", t, s.gooTermLinkIn(ctx, t)) // Golylang: the term's language
}

func (s *Server) gooTermLinkIn(ctx context.Context, t *store.Term) string {
	lb := s.links(ctx)
	switch {
	case t.Taxonomy == "product_cat":
		if !lb.Pretty() {
			return lb.HomeURL("/?product_cat=" + url.QueryEscape(t.Slug))
		}
		path := t.Slug
		cur := t
		for i := 0; cur.ParentID != 0 && i < 20; i++ {
			parent, err := s.store.GetTerm(ctx, cur.ParentID)
			if err != nil {
				break
			}
			path = parent.Slug + "/" + path
			cur = parent
		}
		return lb.HomeURL(lb.Trailing("/" + strings.Trim(defaultStr(s.gooOptString("category_base"), "product-category"), "/") + "/" + path))
	case t.Taxonomy == "product_tag":
		if !lb.Pretty() {
			return lb.HomeURL("/?product_tag=" + url.QueryEscape(t.Slug))
		}
		return lb.HomeURL(lb.Trailing("/" + strings.Trim(defaultStr(s.gooOptString("tag_base"), "product-tag"), "/") + "/" + t.Slug))
	case strings.HasPrefix(t.Taxonomy, "pa_"):
		if a := s.gooAttributeByTaxonomy(ctx, t.Taxonomy); a != nil && a.HasArchives && lb.Pretty() {
			base := strings.Trim(s.gooOptString("attribute_base"), "/")
			if base != "" {
				base = "/" + base
			}
			return lb.HomeURL(lb.Trailing(base + "/" + a.Slug + "/" + t.Slug))
		}
		return lb.HomeURL("/?taxonomy=" + url.QueryEscape(t.Taxonomy) + "&term=" + url.QueryEscape(t.Slug))
	}
	return lb.TermLink(t)
}

// Templates ---------------------------------------------------------------------------------------------

// gooTplCache holds each Server's set: the template functions are bound to their Server, and a
// theme preview sandbox (its own Server in this process) has its own settings, store and files.
var gooTplCache struct {
	sync.Mutex
	sets map[*Server]*template.Template
}

// gooTemplates is the storefront template set (web/goocommerce/templates), reloaded in development.
func (s *Server) gooTemplates() *template.Template {
	gooTplCache.Lock()
	defer gooTplCache.Unlock()
	if gooTplCache.sets == nil {
		gooTplCache.sets = map[*Server]*template.Template{}
	}
	if set := gooTplCache.sets[s]; set != nil && s.cfg.WebDir == "" {
		return set
	}
	funcs := s.tmpl.coreFuncs(s)
	for k, f := range s.gooTplFuncs() {
		funcs[k] = f
	}
	set, err := parseTree(s.web, "goocommerce/templates", funcs)
	if err != nil {
		log.Printf("goocommerce templates: %v", err)
		if prev := gooTplCache.sets[s]; prev != nil {
			return prev
		}
		set = template.Must(template.New("").Parse(""))
	}
	gooTplCache.sets[s] = set
	return set
}

func (s *Server) gooTplFuncs() template.FuncMap {
	return template.FuncMap{
		"price":   func(v float64) template.HTML { return s.gooPrice(v) },
		"priceIn": func(v float64, cur string) template.HTML { return s.gooPriceIn(v, cur) },
		"html":    func(v string) template.HTML { return template.HTML(v) },
		"esc":     gooEsc,
		"neg":     func(v float64) float64 { return -v },
		"country": gooCountryName,
		"inc":     func(i int) int { return i + 1 },
		"fdate": func(t time.Time) string {
			return wp.FormatDate(t.In(s.loc()), s.settings.String("date_format"))
		},
		"iso": func(t time.Time) string { return t.Format(time.RFC3339) },
	}
}

// gooTpl renders a storefront template to HTML.
func (s *Server) gooTpl(name string, data any) string {
	var buf bytes.Buffer
	if err := s.gooTemplates().ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("goocommerce template %s: %v", name, err)
		return "<!-- template error: " + gooEsc(name) + " -->"
	}
	return buf.String()
}

// Shared helpers ----------------------------------------------------------------------------------------

// gooNoticesHTML is wc_print_notices(): the pending notices (and cart messages), taken once.
func (s *Server) gooNoticesHTML(c *gin.Context) string {
	var errs, msgs, infos []string
	if _, ok := c.Get("goo.cart"); ok || s.gooHasSessionCookie(c) {
		for _, e := range s.gooCartFor(c).Errors() {
			errs = append(errs, e)
		}
	}
	if s.gooHasSessionCookie(c) || currentUser(c) != nil || s.gooSessionLoaded(c) {
		for _, n := range s.gooTakeNotices(c) {
			switch n.Type {
			case "error":
				errs = append(errs, n.Msg)
			case "success":
				msgs = append(msgs, n.Msg)
			default:
				infos = append(infos, n.Msg)
			}
		}
	}
	if len(errs)+len(msgs)+len(infos) > 0 {
		c.Header(cache.HeaderControl, "no-store") // a visitor's notices never reach the page cache
		out := gooNoticesMarkup(errs, msgs, infos)
		// Kept for a Web Builder store template, which replaces the store body that took them
		// (wbShopNotices, wbProductWrap).
		c.Set(gooCtxNoticesTaken, c.GetString(gooCtxNoticesTaken)+out)
		return out
	}
	return gooNoticesMarkup(errs, msgs, infos)
}

// gooCtxNoticesTaken holds the notices markup gooNoticesHTML took in this request and nothing
// printed yet through a Web Builder template.
const gooCtxNoticesTaken = "goo.notices.taken"

func gooNoticesMarkup(errs, msgs, infos []string) string {
	var b strings.Builder
	b.WriteString(`<div class="woocommerce-notices-wrapper">`)
	if len(errs) > 0 {
		b.WriteString(`<ul class="woocommerce-error" role="alert">`)
		for _, e := range errs {
			b.WriteString("<li>" + e + "</li>")
		}
		b.WriteString(`</ul>`)
	}
	for _, m := range msgs {
		b.WriteString(`<div class="woocommerce-message" role="alert">` + m + `</div>`)
	}
	for _, m := range infos {
		b.WriteString(`<div class="woocommerce-info" role="status">` + m + `</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// gooErrorList is the error notice markup of AJAX responses.
func gooErrorList(errs []string) string {
	return gooNoticesMarkup(errs, nil, nil)
}

func (s *Server) gooHasSessionCookie(c *gin.Context) bool {
	if _, err := c.Cookie(gooSessionCookie); err == nil {
		return true
	}
	return false
}

func (s *Server) gooSessionLoaded(c *gin.Context) bool {
	_, ok := c.Get(gooCtxSession)
	return ok
}

// gooRedirect sends the visitor on (after saving the session).
func (s *Server) gooRedirect(c *gin.Context, target string) {
	s.gooSaveSession(c)
	gooNoStore(c)
	c.Redirect(http.StatusFound, target)
}

// gooSafeURL keeps a requested redirect on this site (absolute URLs of the home origin or paths).
func (s *Server) gooSafeURL(ctx context.Context, target, fallback string) string {
	if target == "" {
		return fallback
	}
	home := s.links(ctx).HomeURL("/")
	if strings.HasPrefix(target, strings.TrimRight(home, "/")+"/") || target == strings.TrimRight(home, "/") {
		return target
	}
	return safeRedirect(target, fallback)
}

// gooCurrentURL is the request URL without the given query args.
func (s *Server) gooCurrentURL(c *gin.Context, drop ...string) string {
	u := *c.Request.URL
	q := u.Query()
	for _, k := range drop {
		q.Del(k)
	}
	u.RawQuery = q.Encode()
	return s.links(c.Request.Context()).HomeURL(u.RequestURI())
}

// gooHasShortcode reports whether content uses a store shortcode.
func (s *Server) gooHasShortcode(content string) bool {
	if !strings.Contains(content, "[") {
		return false
	}
	for _, t := range gooShortcodeTags() {
		if strings.Contains(content, "["+t+"]") || strings.Contains(content, "["+t+" ") {
			return true
		}
	}
	return false
}

// gooImageHTML is the product image (or the placeholder) at a size.
func (s *Server) gooImageHTML(ctx context.Context, id int64, size, class string) string {
	if id > 0 {
		if a, err := s.store.GetAttachment(ctx, id); err == nil {
			return string(s.attachmentImage(ctx, a, size, class))
		}
	}
	return s.gooPlaceholderImg(ctx, class)
}

// gooPlaceholderImg is wc_placeholder_img().
func (s *Server) gooPlaceholderImg(ctx context.Context, class string) string {
	if id := int64(s.gooOptInt("placeholder_image")); id > 0 {
		if a, err := s.store.GetAttachment(ctx, id); err == nil {
			return string(s.attachmentImage(ctx, a, "medium", class+" woocommerce-placeholder wp-post-image"))
		}
	}
	return `<img src="` + gooAssets + `/images/placeholder.svg" alt="Placeholder" width="300" height="300" class="woocommerce-placeholder wp-post-image ` + class + `" decoding="async" />`
}

// gooImageURL is the URL of an attachment size ("" when missing).
func (s *Server) gooImageURL(ctx context.Context, id int64, size string) (string, int, int) {
	if id <= 0 {
		return "", 0, 0
	}
	a, err := s.store.GetAttachment(ctx, id)
	if err != nil {
		return "", 0, 0
	}
	return s.gooImageURLOf(ctx, a, size)
}

// gooImageURLOf is gooImageURL for an attachment already loaded.
func (s *Server) gooImageURLOf(ctx context.Context, a *store.Attachment, size string) (string, int, int) {
	if size != "full" {
		if v := a.Variant(size); v != nil {
			return s.uploadURL(ctx, v.FilePath), v.Width, v.Height
		}
	}
	return s.uploadURL(ctx, a.FilePath), a.Width, a.Height
}

// gooFrontFormatAddress is WC_Countries::get_formatted_address() in the default format.
func gooFrontFormatAddress(a map[string]string) string {
	if len(a) == 0 {
		return ""
	}
	var lines []string
	if n := strings.TrimSpace(a["first_name"] + " " + a["last_name"]); n != "" {
		lines = append(lines, gooEsc(n))
	}
	for _, k := range []string{"company", "address_1", "address_2", "city"} {
		if v := strings.TrimSpace(a[k]); v != "" {
			lines = append(lines, gooEsc(v))
		}
	}
	st := strings.TrimSpace(strings.TrimSpace(gooStateName(a["country"], a["state"])) + " " + a["postcode"])
	if st != "" {
		lines = append(lines, gooEsc(st))
	}
	if a["country"] != "" {
		lines = append(lines, gooEsc(gooCountryName(a["country"])))
	}
	return strings.Join(lines, "<br/>")
}

// gooQuantityInput is woocommerce_quantity_input().
func gooQuantityInput(name, label string, value, min, max int, id string, hidden bool) string {
	if id == "" {
		id = "quantity_" + gooRandomKey(13)
	}
	if hidden || (max > 0 && min == max) {
		return `<div class="quantity hidden"><input type="hidden" id="` + id + `" class="qty" name="` + gooEsc(name) + `" value="` + strconv.Itoa(value) + `" /></div>`
	}
	maxAttr := ""
	if max > 0 {
		maxAttr = strconv.Itoa(max)
	}
	return `<div class="quantity"><label class="screen-reader-text" for="` + id + `">` + gooEsc(label) + ` quantity</label>` +
		`<input type="number" id="` + id + `" class="input-text qty text" name="` + gooEsc(name) + `" value="` + gooQtyValue(value) + `" aria-label="Product quantity" size="4" min="` +
		strconv.Itoa(min) + `" max="` + maxAttr + `" step="1" placeholder="" inputmode="numeric" autocomplete="off" /></div>`
}

func gooQtyValue(v int) string {
	if v < 0 {
		return ""
	}
	return strconv.Itoa(v)
}

// jsonMarshalNoEscape marshals without escaping <, > and & (data attributes are HTML-escaped anyway).
func jsonMarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := jsonNewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// gooScriptSafe makes JSON safe inside a <script> element.
func gooScriptSafe(raw []byte) string {
	return strings.NewReplacer("</", `<\/`, "<!--", `<\!--`).Replace(string(raw))
}

var (
	gooSlugRe  = regexp.MustCompile(`[^a-z0-9_-]+`)
	gooClassRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)
