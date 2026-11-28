package server

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Goocommerce's extension points: the Go counterpart of the WooCommerce actions and filters its
// premium extensions hook into. Modules (goo_ext_*.go) register in init(); every hook of a module
// checks s.gooExtOn(module) itself, so a switched-off module changes nothing.

// gooProductTypeDef is a product type beyond the core four (bundle, subscription, booking, gift card).
type gooProductTypeDef struct {
	Type   string
	Label  string
	Module string // the extension that must be on for the type to be offered
	// Purchasable / NeedsShipping override the core rules when set.
	Purchasable   func(s *Server, ctx context.Context, p *gooProduct) bool
	NeedsShipping func(s *Server, ctx context.Context, p *gooProduct) bool
	// AddToCartText is the loop / single button text.
	AddToCartText func(s *Server, p *gooProduct, single bool) string
	// Virtual reports whether the product type is always virtual (bookings, gift cards).
	Virtual bool
}

var gooProductTypes = map[string]*gooProductTypeDef{}

// gooRegisterProductType adds a product type (product_type_selector).
func gooRegisterProductType(d *gooProductTypeDef) { gooProductTypes[d.Type] = d }

// gooActionFunc is an action callback (do_action).
type gooActionFunc func(s *Server, ctx context.Context, arg any)

type gooAction struct {
	prio int
	fn   gooActionFunc
}

var gooActions = map[string][]gooAction{}

// gooOn registers an action: product_saved (*gooProduct), low_stock / no_stock (*gooProduct),
// order_created (*gooOrder), order_status_changed (*gooStatusChange), payment_complete (*gooOrder),
// order_refunded (*gooRefundEvent), customer_created (int64 user id), cart_emptied (*gooCart).
func gooOn(name string, prio int, fn gooActionFunc) {
	gooActions[name] = append(gooActions[name], gooAction{prio, fn})
	sort.SliceStable(gooActions[name], func(i, j int) bool { return gooActions[name][i].prio < gooActions[name][j].prio })
}

// gooFire runs an action's callbacks; a panicking callback is logged, not fatal.
func (s *Server) gooFire(ctx context.Context, name string, arg any) {
	for _, a := range gooActions[name] {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("goocommerce: action %s: %v", name, r)
				}
			}()
			a.fn(s, ctx, arg)
		}()
	}
	s.gooWebhookAction(ctx, name, arg) // "action.<name>" webhooks
}

// gooStatusChange is the argument of order_status_changed.
type gooStatusChange struct {
	Order    *gooOrder
	From, To string
}

// Cart filters --------------------------------------------------------------------------------------

// gooAddToCartHook validates an add-to-cart request and may add cart item data (Product Add-Ons'
// fields, a booking's date, a gift card's recipient). form is the posted form (nil from the Store API
// without one); returning an error rejects the request with that message.
type gooAddToCartHook func(s *Server, ctx context.Context, p *gooProduct, qty int, form map[string][]string, data map[string]any) error

var gooAddToCartHooks []gooAddToCartHook

// gooCartItemPriceHook adjusts a cart item's unit price (woocommerce_before_calculate_totals /
// set_price): add-on prices, bundle pricing, dynamic pricing rules. It runs in registration order.
type gooCartItemPriceHook func(s *Server, ctx context.Context, cart *gooCart, item *gooCartItem)

var gooCartItemPriceHooks []gooCartItemPriceHook

// gooCartFeesHook adds fees (or negative fees: gift cards, points) to a cart (woocommerce_cart_calculate_fees).
type gooCartFeesHook func(s *Server, ctx context.Context, cart *gooCart)

var gooCartFeesHooks []gooCartFeesHook

// gooCartItemDataHook lists an item's extra data for display (woocommerce_get_item_data): label → value.
type gooCartItemDataHook func(s *Server, ctx context.Context, item *gooCartItem) []gooKV

var gooCartItemDataHooks []gooCartItemDataHook

// gooKV is a label / value pair shown under a cart or order item.
type gooKV struct {
	Key, Value string
}

// gooCheckoutHook validates the checkout before an order is created and may add order meta / items
// (woocommerce_after_checkout_validation, woocommerce_checkout_create_order).
type gooCheckoutHook func(s *Server, ctx context.Context, cart *gooCart, order *gooOrder) error

var gooCheckoutHooks []gooCheckoutHook

// gooCheckoutReserveHook takes what the order spends from a shared balance (gift cards, points) once the
// checkout hooks have passed, atomically, so two checkouts at once cannot both spend the same balance:
// it fails when the balance no longer covers the order. release gives the balance back when the order
// is not created after all.
type gooCheckoutReserveHook func(s *Server, ctx context.Context, cart *gooCart, order *gooOrder) (release func(), err error)

var gooCheckoutReserveHooks []gooCheckoutReserveHook

// Front-end output ----------------------------------------------------------------------------------

// gooHTMLHook prints markup at a template location for a product:
// "single_before_add_to_cart" (inside the form: add-on fields, booking form), "single_after_add_to_cart",
// "single_summary_end" (wishlist / compare buttons), "loop_after_item" (buttons under a catalog item),
// "single_meta_end".
type gooHTMLHook func(s *Server, c *gin.Context, p *gooProduct) template.HTML

var gooHTMLHooks = map[string][]gooHTMLHook{}

func gooOnHTML(location string, fn gooHTMLHook) {
	gooHTMLHooks[location] = append(gooHTMLHooks[location], fn)
}

func (s *Server) gooHTML(c *gin.Context, location string, p *gooProduct) template.HTML {
	var out template.HTML
	for _, fn := range gooHTMLHooks[location] {
		out += fn(s, c, p)
	}
	return out
}

// gooPageHook prints markup on a store page: "cart_before", "cart_after", "checkout_before_payment",
// "checkout_after_order_review", "thankyou", "footer" (quick view modal, compare bar).
type gooPageHook func(s *Server, c *gin.Context) template.HTML

var gooPageHooks = map[string][]gooPageHook{}

func gooOnPage(location string, fn gooPageHook) {
	gooPageHooks[location] = append(gooPageHooks[location], fn)
}

func (s *Server) gooPageHTML(c *gin.Context, location string) template.HTML {
	var out template.HTML
	for _, fn := range gooPageHooks[location] {
		out += fn(s, c)
	}
	return out
}

// gooAccountEndpoint is a My Account tab added by a module (Subscriptions, Memberships, Bookings,
// Wishlist, Points, Gift Cards).
type gooAccountEndpoint struct {
	Slug   string
	Title  string
	Module string
	After  string // the core endpoint it follows ("orders", "downloads", …)
	Render func(s *Server, c *gin.Context, arg string) template.HTML
}

var gooAccountEndpoints []*gooAccountEndpoint

// gooProductTab is a tab on the single product page (woocommerce_product_tabs).
type gooProductTab struct {
	Key      string
	Title    func(s *Server, c *gin.Context, p *gooProduct) string // "" hides the tab
	Priority int
	Render   func(s *Server, c *gin.Context, p *gooProduct) template.HTML
}

var gooProductTabs []*gooProductTab

// gooFrontRoute is a front-end URL handled by a module (/?goo-ajax=… actions, invoice downloads):
// Match receives the path without slashes and returns true when it served the request.
type gooFrontRoute func(s *Server, c *gin.Context, path string) bool

var gooFrontRoutes []gooFrontRoute

// gooEmailAttachHook returns attachments for an e-mail (PDF invoices): file name → content.
type gooEmailAttachHook func(s *Server, ctx context.Context, emailID string, o *gooOrder) map[string][]byte

var gooEmailAttachHooks []gooEmailAttachHook

// gooOrderActionHook adds entries to the admin order screen's "Order actions" box (Generate invoice…).
type gooOrderAction struct {
	ID, Label string
	Module    string
	Run       func(s *Server, c *gin.Context, o *gooOrder) error
}

var gooOrderActions []*gooOrderAction

// gooAdminPanelHook prints a panel on the admin product data box ("product_options" after General,
// "product_tabs" adds tabs) and saves its fields. Modules use it for their product settings.
type gooProductPanel struct {
	ID, Label string
	Module    string
	ShowFor   []string // product types the tab shows for, nil = all
	Render    func(s *Server, c *gin.Context, p *gooProduct) template.HTML
	Save      func(s *Server, c *gin.Context, p *gooProduct)
}

var gooProductPanels []*gooProductPanel

// E-mails ------------------------------------------------------------------------------------------------

// gooEmailDef is a transactional e-mail (WC_Email): its settings live in the option
// "email_<id>_settings" ({enabled, recipient, subject, heading, additional_content, email_type}).
type gooEmailDef struct {
	ID          string
	Title       string
	Description string
	Customer    bool // sent to the customer; else to Recipient (default: the admin e-mail)
	Manual      bool // only sent from the order screen (customer invoice)
	Subject     string
	Heading     string
	Additional  string
	Module      string // the extension that provides it, "" for core
}

var gooEmailDefs []*gooEmailDef

func gooRegisterEmail(d *gooEmailDef) { gooEmailDefs = append(gooEmailDefs, d) }

// gooEmailDefByID finds a registered e-mail.
func gooEmailDefByID(id string) *gooEmailDef {
	for _, d := range gooEmailDefs {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// gooEmailSettings are an e-mail's settings over its defaults (enabled "yes"/"no", email_type html).
func (s *Server) gooEmailSettings(id string) map[string]string {
	out := map[string]string{"enabled": "yes", "email_type": "html", "recipient": "", "subject": "", "heading": "", "additional_content": ""}
	if d := gooEmailDefByID(id); d != nil {
		out["subject"], out["heading"], out["additional_content"] = d.Subject, d.Heading, d.Additional
		if !d.Customer {
			out["recipient"] = s.settings.String("admin_email")
		}
	}
	for k, v := range s.gooOptMap("email_" + id + "_settings") {
		if str, ok := v.(string); ok {
			out[k] = str
		}
	}
	return out
}

// gooEmailSender sends a registered e-mail for an order (set by goo_emails.go).
var gooEmailSender func(s *Server, ctx context.Context, id string, o *gooOrder, args map[string]any) error

// gooTriggerEmail sends an e-mail now (order actions: "Email invoice / order details to customer",
// "Resend new order notification").
func (s *Server) gooTriggerEmail(ctx context.Context, id string, o *gooOrder, args map[string]any) error {
	if gooEmailSender == nil {
		return nil
	}
	return gooEmailSender(s, ctx, id, o, args)
}

// gooGatewayFilter narrows the payment methods offered for a cart (woocommerce_available_payment_gateways);
// Subscriptions keeps only the gateways that can renew automatically when manual renewals are not accepted.
type gooGatewayFilter func(s *Server, c *gin.Context, cart *gooCart, gws []*gooGatewayHandle) []*gooGatewayHandle

var gooGatewayFilters []gooGatewayFilter

// gooPaidValueHook reports whether an order holds an item whose value is released once the order
// counts as paid (processing): a gift card that is issued and e-mailed, a membership that is granted.
// Offline gateways that take no payment at checkout (cash on delivery) keep such orders on-hold until
// the merchant records the payment, as WC_Gateway_COD does for downloadable files.
type gooPaidValueHook func(s *Server, ctx context.Context, o *gooOrder) bool

var gooPaidValueHooks []gooPaidValueHook

// gooPlaceOrderTextHook returns the checkout button text for a cart ("" keeps the next hook / default):
// woocommerce_order_button_text of Subscriptions and Pre-Orders.
type gooPlaceOrderTextHook func(s *Server, ctx context.Context, cart *gooCart) string

var gooPlaceOrderTextHooks []gooPlaceOrderTextHook

// gooPlaceOrderText is the label of the checkout's place order button.
func (s *Server) gooPlaceOrderText(ctx context.Context, cart *gooCart) string {
	for _, h := range gooPlaceOrderTextHooks {
		if t := strings.TrimSpace(h(s, ctx, cart)); t != "" {
			return t
		}
	}
	return "Place order"
}

// Content and price filters ----------------------------------------------------------------------------

// gooContentFilter changes post content on the front end (Memberships' restricted content, shortcodes).
type gooContentFilter func(s *Server, ctx context.Context, content string) string

var gooContentFilters []gooContentFilter

// gooFilterContent runs the content filters (called by the storefront's gooShortcodes).
func (s *Server) gooFilterContent(ctx context.Context, content string) string {
	for _, f := range gooContentFilters {
		content = f(s, ctx, content)
	}
	return content
}

// gooPriceHTMLHook changes a product's price HTML (woocommerce_get_price_html): "/ month" of a
// subscription, a bundle's "From:", member prices.
type gooPriceHTMLHook func(s *Server, ctx context.Context, p *gooProduct, h template.HTML) template.HTML

var gooPriceHTMLHooks []gooPriceHTMLHook

// gooFilterPriceHTML runs the price HTML hooks (called wherever the storefront prints get_price_html()).
func (s *Server) gooFilterPriceHTML(ctx context.Context, p *gooProduct, h template.HTML) template.HTML {
	for _, f := range gooPriceHTMLHooks {
		h = f(s, ctx, p, h)
	}
	return h
}

// gooProductPriceHook changes a product's catalog price (woocommerce_product_get_price): member and
// role prices of Memberships / Dynamic Pricing shown in the shop. It runs where the cart price hooks do
// not (catalog display); cart prices use gooCartItemPriceHooks.
type gooProductPriceHook func(s *Server, ctx context.Context, p *gooProduct, price float64) float64

var gooProductPriceHooks []gooProductPriceHook

// gooDisplayPrice is the product's active price after the catalog price hooks.
func (s *Server) gooDisplayPrice(ctx context.Context, p *gooProduct) float64 {
	v := p.PriceValue()
	for _, f := range gooProductPriceHooks {
		v = f(s, ctx, p, v)
	}
	return v
}

// gooCheckoutValidateHook adds checkout errors before an account or order is created
// (woocommerce_after_checkout_validation).
type gooCheckoutValidateHook func(s *Server, c *gin.Context, cart *gooCart, d *gooCheckoutData) []string

var gooCheckoutValidateHooks []gooCheckoutValidateHook

// Item visibility ------------------------------------------------------------------------------------------

// gooCartItemVisibleHook hides a cart line from a listing (woocommerce_cart_item_visible,
// woocommerce_widget_cart_item_visible, woocommerce_checkout_cart_item_visible): where is "cart",
// "mini_cart" or "checkout". The line still counts in the totals.
type gooCartItemVisibleHook func(s *Server, ctx context.Context, it *gooCartItem, where string) bool

var gooCartItemVisibleHooks []gooCartItemVisibleHook

// gooCartItemVisible reports whether a cart line is listed at where.
func (s *Server) gooCartItemVisible(ctx context.Context, it *gooCartItem, where string) bool {
	for _, f := range gooCartItemVisibleHooks {
		if !f(s, ctx, it, where) {
			return false
		}
	}
	return true
}

// gooOrderItemVisibleHook hides an order line from a listing (woocommerce_order_item_visible): where is
// "order_details", "order_pay", "email" or "invoice".
type gooOrderItemVisibleHook func(s *Server, ctx context.Context, it *gooOrderItem, where string) bool

var gooOrderItemVisibleHooks []gooOrderItemVisibleHook

// Listings and single views ----------------------------------------------------------------------------

// gooListingExcludeHook returns ids the visitor (gxaUser(ctx)) must not see listed: kind "product" for
// catalog listings (shop, archives, search, shortcodes, Store API), "post" for the blog's main query,
// search, feeds and the posts REST API (Memberships' "hide completely").
type gooListingExcludeHook func(s *Server, ctx context.Context, kind string) []int64

var gooListingExcludeHooks []gooListingExcludeHook

// gooListingExclude collects the ids hidden from the visitor's listings.
func (s *Server) gooListingExclude(ctx context.Context, kind string) []int64 {
	if !s.gooOn() {
		return nil
	}
	var out []int64
	for _, f := range gooListingExcludeHooks {
		out = append(out, f(s, ctx, kind)...)
	}
	return out
}

// gooListingKinds is the exclusion list for a query over post types (products use the "product" kind).
func (s *Server) gooListingKinds(ctx context.Context, types []string) []int64 {
	if !s.gooOn() {
		return nil
	}
	out := s.gooListingExclude(ctx, "post")
	for _, t := range types {
		if t == "product" {
			out = append(out, s.gooListingExclude(ctx, "product")...)
			break
		}
	}
	return out
}

// gooHiddenProduct reports whether a product (or a variation's parent) is hidden from the visitor.
func (s *Server) gooHiddenProduct(ctx context.Context, id int64) bool {
	ex := s.gooListingExclude(ctx, "product")
	if len(ex) == 0 {
		return false
	}
	var parent int64
	_ = s.store.Pool.QueryRow(ctx, `SELECT coalesce(parent_id, 0) FROM posts WHERE id = $1`, id).Scan(&parent)
	for _, x := range ex {
		if x == id || (parent != 0 && x == parent) {
			return true
		}
	}
	return false
}

// gooAdjacent is get_adjacent_post() without the posts hidden from the visitor (previous/next links).
func (s *Server) gooAdjacent(ctx context.Context, p *store.Post, previous bool) (*store.Post, error) {
	return s.store.AdjacentExcept(ctx, p, previous, s.gooListingKinds(ctx, []string{p.Type}))
}

// gooPublicExclude is gooListingKinds for an anonymous visitor: sitemaps are public and cached, so
// they leave out whatever a visitor without a membership may not see listed.
func (s *Server) gooPublicExclude(ctx context.Context, types []string) []int64 {
	return s.gooListingKinds(withPluginUser(ctx, nil), types)
}

// gooSingleAccessHook decides whether the visitor may open a single post, page or product: "" serves
// it, "404" answers not found, anything else is a URL to redirect to (Memberships' restriction modes).
type gooSingleAccessHook func(s *Server, c *gin.Context, postID int64, postType string) string

var gooSingleAccessHooks []gooSingleAccessHook

// gooSingleAccess runs the single access hooks (the first refusal wins).
func (s *Server) gooSingleAccess(c *gin.Context, postID int64, postType string) string {
	if !s.gooOn() {
		return ""
	}
	for _, f := range gooSingleAccessHooks {
		if r := f(s, c, postID, postType); r != "" {
			return r
		}
	}
	return ""
}

// gooGuardSiteQuery applies gooSingleAccess to the core's singular views: a refusal turns the query
// into a 404, a redirect answers the request (true).
func (s *Server) gooGuardSiteQuery(c *gin.Context, q *siteQuery) bool {
	if q == nil || q.Post == nil || (q.Kind != "single" && q.Kind != "page" && q.Kind != "front-page") {
		return false
	}
	switch r := s.gooSingleAccess(c, q.Post.ID, q.Post.Type); r {
	case "":
		return false
	case "404":
		*q = siteQuery{Kind: "404"}
		return false
	default:
		c.Header("Cache-Control", "no-store")
		c.Redirect(http.StatusFound, r)
		return true
	}
}

// gooGuardProduct applies gooSingleAccess to a single product page: (denied, handled) — a refusal
// lets the core answer 404 (handled false), a redirect answers the request (handled true).
func (s *Server) gooGuardProduct(c *gin.Context, productID int64) (bool, bool) {
	switch r := s.gooSingleAccess(c, productID, "product"); r {
	case "":
		return false, false
	case "404":
		return true, false
	default:
		c.Header("Cache-Control", "no-store")
		c.Redirect(http.StatusFound, r)
		return true, true
	}
}

// gooOrderItemVisible reports whether an order line is listed at where.
func (s *Server) gooOrderItemVisible(ctx context.Context, it *gooOrderItem, where string) bool {
	for _, f := range gooOrderItemVisibleHooks {
		if !f(s, ctx, it, where) {
			return false
		}
	}
	return true
}
