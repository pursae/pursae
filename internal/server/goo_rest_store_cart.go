package server

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/store"
)

// The Store API's cart, checkout and order routes (wc/store/v1/cart…, /checkout, /order/{id}). They
// work on the storefront's own session (gooSessionFor / gooCartFor), so a cart filled through the
// API is the cart of the shop pages and the other way round.
//
// Mutating requests need the Nonce header (s.nonce(c, "wc_store_api"), also accepted as
// X-WC-Store-API-Nonce) or a Cart-Token: a signed token naming the session, for headless clients
// without cookies. Every response returns fresh Nonce, Nonce-Timestamp, Cart-Token and User-ID headers.

const gooStoreAPINonce = "wc_store_api"

// gooStoreAPIKey marks a request handled by the Store API (gin context key).
const gooStoreAPIKey = "goo_store_api" //nolint:gosec // G101: a request-context key, not a credential

func (s *Server) registerGooStoreCartREST() {
	r := func(pattern string, m map[string]restHandler) {
		wrapped := map[string]restHandler{}
		for method, h := range m {
			wrapped[method] = s.gooStore(h, method != http.MethodGet)
		}
		s.gooRoute(gooStoreNS, pattern, wrapped)
	}
	r("/cart", map[string]restHandler{http.MethodGet: s.gooStoreCartGet})
	r("/cart/add-item", map[string]restHandler{http.MethodPost: s.gooStoreAddItem})
	r("/cart/remove-item", map[string]restHandler{http.MethodPost: s.gooStoreRemoveItem})
	r("/cart/update-item", map[string]restHandler{http.MethodPost: s.gooStoreUpdateItem})
	r("/cart/apply-coupon", map[string]restHandler{http.MethodPost: s.gooStoreApplyCoupon})
	r("/cart/apply-gift-card", map[string]restHandler{http.MethodPost: s.gcStoreApply})
	r("/cart/remove-gift-card", map[string]restHandler{http.MethodPost: s.gcStoreRemove})
	r("/cart/remove-coupon", map[string]restHandler{http.MethodPost: s.gooStoreRemoveCoupon})
	r("/cart/update-customer", map[string]restHandler{http.MethodPost: s.gooStoreUpdateCustomer})
	r("/cart/select-shipping-rate", map[string]restHandler{http.MethodPost: s.gooStoreSelectRate})
	r("/cart/items", map[string]restHandler{http.MethodGet: s.gooStoreItems, http.MethodPost: s.gooStoreItemsCreate, http.MethodDelete: s.gooStoreItemsDelete})
	r("/cart/items/(?P<key>[\\w-]{3,})", map[string]restHandler{http.MethodGet: s.gooStoreItem, http.MethodPut: s.gooStoreItemUpdate, http.MethodDelete: s.gooStoreItemDelete})
	r("/cart/coupons", map[string]restHandler{http.MethodGet: s.gooStoreCoupons, http.MethodPost: s.gooStoreCouponsCreate, http.MethodDelete: s.gooStoreCouponsDelete})
	r("/cart/coupons/(?P<code>[^/]+)", map[string]restHandler{http.MethodGet: s.gooStoreCoupon, http.MethodDelete: s.gooStoreCouponDelete})
	r("/checkout", map[string]restHandler{http.MethodGet: s.gooStoreCheckoutGet, http.MethodPut: s.gooStoreCheckoutPut, http.MethodPost: s.gooStoreCheckoutPost})
	r("/order/(?P<id>[\\d]+)", map[string]restHandler{http.MethodGet: s.gooStoreOrderGet})
}

// gooStore wraps a Store API handler: the Cart-Token session, then the nonce check of writes.
func (s *Server) gooStore(h restHandler, mutating bool) restHandler {
	return func(c *gin.Context, req *restRequest) {
		c.Header("Access-Control-Expose-Headers", "X-WP-Total, X-WP-TotalPages, Link, Nonce, Nonce-Timestamp, Cart-Token, User-ID")
		c.Set(gooStoreAPIKey, true)
		if ck, err := c.Cookie(postPassCookie); err == nil { // gooPassLocked: product text behind a password
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), gooPostPassCtx{}, ck))
		}
		token := s.gooUseCartToken(c)
		if mutating && !token {
			nonce := c.GetHeader("Nonce")
			if nonce == "" {
				nonce = c.GetHeader("X-WC-Store-API-Nonce")
			}
			if nonce == "" {
				s.gooStoreHeaders(c)
				restErr(c, http.StatusUnauthorized, "woocommerce_rest_missing_nonce", "Missing the Nonce header. This endpoint requires a valid nonce.")
				return
			}
			if !s.checkNonceValue(c, nonce, gooStoreAPINonce) {
				s.gooStoreHeaders(c)
				restErr(c, http.StatusForbidden, "woocommerce_rest_invalid_nonce", "Nonce is invalid.")
				return
			}
		}
		h(c, req)
	}
}

// gooPostPassCtx carries the request's wp-postpass cookie to the Store API's product serializers.
type gooPostPassCtx struct{}

// gooPassLocked reports whether a password-protected product's text must be withheld: the request
// holds no wp-postpass cookie for its password (post_password_required()).
func (s *Server) gooPassLocked(ctx context.Context, post *store.Post) bool {
	if post == nil || post.Password == "" {
		return false
	}
	if gc, ok := ctx.(*gin.Context); ok {
		return s.passwordRequired(gc, post)
	}
	ck, _ := ctx.Value(gooPostPassCtx{}).(string)
	return ck == "" || !hmac.Equal([]byte(ck), []byte(s.postPassValue(post.Password)))
}

// Cart tokens ------------------------------------------------------------------------------------------

// gooCartToken signs the request's session key: base64url(key) "." signature (valid two days).
func (s *Server) gooCartToken(c *gin.Context) string {
	key := s.gooSessionFor(c).Key
	enc := base64.RawURLEncoding.EncodeToString([]byte(key))
	if strings.HasPrefix(key, "u:") {
		// A logged-in customer's session: bind the token to that user's auth session, so it stops
		// working on logout or a password change (both end the session token).
		return enc + "." + s.nonce(c, "wc_cart_token|"+key)
	}
	return enc + "." + s.auth.Nonce("wc_cart_token|"+key, 0, "")
}

// gooUseCartToken loads the session a valid Cart-Token names before anything reads the session.
func (s *Server) gooUseCartToken(c *gin.Context) bool {
	tok := strings.TrimSpace(c.GetHeader("Cart-Token"))
	if tok == "" {
		return false
	}
	enc, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || len(raw) == 0 || len(raw) > 64 {
		return false
	}
	key := string(raw)
	sess := &gooSession{Key: key}
	if uid, ok := strings.CutPrefix(key, "u:"); ok {
		// User sessions only for that same logged-in user, signed with their auth session.
		u := currentUser(c)
		if u == nil || u.ID <= 0 || strconv.FormatInt(u.ID, 10) != uid || !s.checkNonceValue(c, sig, "wc_cart_token|"+key) {
			return false
		}
		sess.user = gooInt(uid)
	} else if s.auth.VerifyNonce(sig, "wc_cart_token|"+key, 0, "") <= 0 {
		return false
	} else if len(key) != 32 || !gooHexOnly(key) {
		return false
	}
	if !s.gooLoadSession(c.Request.Context(), sess) {
		sess.isNew = true
	}
	if sess.Data.ChosenShipping == nil {
		sess.Data.ChosenShipping = map[string]string{}
	}
	if sess.Data.Customer.Billing == nil {
		sess.Data.Customer.Billing = map[string]string{}
	}
	if sess.Data.Customer.Shipping == nil {
		sess.Data.Customer.Shipping = map[string]string{}
	}
	c.Set(gooCtxSession, sess)
	return true
}

// gooStoreHeaders sets the Nonce, Cart-Token and User-ID headers of a Store API response.
func (s *Server) gooStoreHeaders(c *gin.Context) {
	c.Header("Nonce", s.nonce(c, gooStoreAPINonce))
	c.Header("Nonce-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	c.Header("Cart-Token", s.gooCartToken(c))
	var uid int64
	if u := currentUser(c); u != nil {
		uid = u.ID
	}
	c.Header("User-ID", itoa64(uid))
}

// Cart schema ------------------------------------------------------------------------------------------

var gooTagRe = regexp.MustCompile(`<[^>]*>`)

// gooPlainMsg is an engine notice as the Store API's plain-text error message.
func gooPlainMsg(msg string) string {
	return strings.TrimSpace(html.UnescapeString(gooTagRe.ReplaceAllString(msg, "")))
}

// gooStoreItemJSON is the CartItemSchema.
func (s *Server) gooStoreItemJSON(ctx context.Context, cart *gooCart, it *gooCartItem) map[string]any {
	p := it.Product()
	parent := p
	if p.Type == gooTypeVariation {
		if pp := s.gooParent(ctx, p); pp != nil {
			parent = pp
		}
	}
	unit := 0.0
	if it.Quantity > 0 {
		unit = it.LineSubtotal / float64(it.Quantity)
	}
	if s.gooOptString("tax_display_cart") == "incl" && it.Quantity > 0 {
		unit = (it.LineSubtotal + it.LineSubtotalTax) / float64(it.Quantity)
	}
	regular := unit
	if p.RegularPrice != nil && p.IsOnSale() {
		regular = *p.RegularPrice
	}
	prices := s.gooWithCurrency(map[string]any{"price": s.gooMinor(unit), "regular_price": s.gooMinor(regular), "sale_price": s.gooMinor(unit), "price_range": nil,
		"raw_prices": map[string]any{"precision": 6, "price": strconv.FormatInt(int64(unit*1e6+0.5), 10), "regular_price": strconv.FormatInt(int64(regular*1e6+0.5), 10),
			"sale_price": strconv.FormatInt(int64(unit*1e6+0.5), 10)}}, "")
	totals := s.gooWithCurrency(map[string]any{"line_subtotal": s.gooMinor(it.LineSubtotal), "line_subtotal_tax": s.gooMinor(it.LineSubtotalTax),
		"line_total": s.gooMinor(it.LineTotal), "line_total_tax": s.gooMinor(it.LineTax)}, "")
	variation := []any{}
	if p.Type == gooTypeVariation && parent != p {
		for _, a := range parent.Attributes {
			if !a.Variation {
				continue
			}
			if v := it.Variation["attribute_"+a.Key()]; v != "" {
				variation = append(variation, map[string]any{"raw_attribute": "attribute_" + a.Key(), "attribute": s.gooAttrLabel(ctx, a), "value": s.gooAttrValueLabel(ctx, a, v)})
			}
		}
	}
	itemData := []any{}
	for _, h := range gooCartItemDataHooks {
		for _, kv := range h(s, ctx, it) {
			itemData = append(itemData, map[string]any{"name": kv.Key, "value": kv.Value, "display": kv.Value})
		}
	}
	images := []any{}
	if imgID := s.gooImageOf(ctx, p); imgID != 0 {
		if a, err := s.gooLoopAttachment(cart.c, imgID); err == nil {
			images = append(images, s.gooImageJSONOf(ctx, a))
		}
	}
	maxQty := 9999
	if q := s.gooStockQty(ctx, p); q >= 0 && !s.gooBackordersAllowed(ctx, p) {
		maxQty = q
	}
	if p.SoldIndividually || parent.SoldIndividually {
		maxQty = 1
	}
	var lowStock any
	if q := s.gooStockQty(ctx, p); q > 0 && q <= s.gooOptInt("notify_low_stock_amount") {
		lowStock = q
	}
	short, desc := "", ""
	if parent.Post != nil {
		short, desc = parent.Post.Excerpt, parent.Post.Content
	}
	if p.Type == gooTypeVariation && p.Description != "" {
		desc = p.Description
	}
	if s.gooPassLocked(ctx, parent.Post) {
		short, desc = "", ""
	}
	return map[string]any{
		"key": it.Key, "id": p.ID, "type": p.Type, "quantity": it.Quantity,
		"quantity_limits": map[string]any{"minimum": 1, "maximum": maxQty, "multiple_of": 1, "editable": !(p.SoldIndividually || parent.SoldIndividually)},
		"name":            s.gooProductTitle(ctx, p), "short_description": short, "description": desc, "sku": p.SKU,
		"low_stock_remaining": lowStock, "backorders_allowed": s.gooBackordersAllowed(ctx, p),
		"show_backorder_badge": s.gooManagingStock(ctx, p) && s.gooStockQty(ctx, p) < it.Quantity && s.gooBackordersAllowed(ctx, p),
		"sold_individually":    p.SoldIndividually || parent.SoldIndividually, "permalink": s.gooPermalink(ctx, p), "images": images,
		"variation": variation, "item_data": itemData, "prices": prices, "totals": totals, "catalog_visibility": parent.CatalogVisibility,
		"extensions": map[string]any{},
	}
}

// gooStoreCartJSON is the CartSchema.
func (s *Server) gooStoreCartJSON(c *gin.Context, cart *gooCart) map[string]any {
	ctx := c.Request.Context()
	items := []any{}
	weight := 0.0
	var imgIDs []int64
	for _, it := range cart.Items {
		if id := s.gooImageOf(ctx, it.Product()); id > 0 {
			imgIDs = append(imgIDs, id)
		}
	}
	s.gooPrefetchImages(c, imgIDs)
	for _, it := range cart.Items {
		weight += s.gooWeightOf(ctx, it.Product()) * float64(it.Quantity)
		if !s.gooCartItemVisible(ctx, it, "store_api") { // e.g. a bundled item set to hide in the cart
			continue
		}
		items = append(items, s.gooStoreItemJSON(ctx, cart, it))
	}
	coupons := []any{}
	for _, cp := range cart.Coupons() {
		coupons = append(coupons, map[string]any{"code": cp.Code, "discount_type": cp.DiscountType,
			"totals": s.gooWithCurrency(map[string]any{"total_discount": s.gooMinor(cart.CouponExTax[cp.Code]), "total_discount_tax": s.gooMinor(cart.CouponTax[cp.Code])}, "")})
	}
	fees := []any{}
	for _, f := range cart.Fees {
		fees = append(fees, map[string]any{"id": f.ID, "name": f.Name, "totals": s.gooWithCurrency(map[string]any{"total": s.gooMinor(f.Total), "total_tax": s.gooMinor(f.Tax)}, "")})
	}
	cu := cart.Customer()
	rates := []any{}
	if cart.NeedsShipping() {
		for i, pr := range cart.Packages {
			pkgItems := []any{}
			for _, it := range pr.Package.Items {
				pkgItems = append(pkgItems, map[string]any{"key": it.Key, "name": s.gooProductTitle(ctx, it.Product()), "quantity": it.Quantity})
			}
			list := []any{}
			for _, r := range pr.Rates {
				meta := []any{}
				for k, v := range r.Meta {
					meta = append(meta, map[string]any{"key": k, "value": v})
				}
				list = append(list, s.gooWithCurrency(map[string]any{"rate_id": r.ID, "name": r.Label, "description": "", "delivery_time": "",
					"price": s.gooMinor(r.Cost), "taxes": s.gooMinor(gooSumTaxes(r.Taxes)), "instance_id": r.InstanceID, "method_id": r.MethodID,
					"meta_data": meta, "selected": pr.Chosen != nil && pr.Chosen.ID == r.ID}, ""))
			}
			d := pr.Package.Destination
			name := "Shipping"
			if len(cart.Packages) > 1 {
				name = "Shipment " + strconv.Itoa(i+1)
			}
			rates = append(rates, map[string]any{"package_id": i, "name": name,
				"destination": map[string]any{"address_1": cu.Shipping["address_1"], "address_2": cu.Shipping["address_2"], "city": d.City, "state": d.State, "postcode": d.Postcode, "country": d.Country},
				"items":       pkgItems, "shipping_rates": list})
		}
	}
	taxLines := []any{}
	for _, t := range cart.TaxTotals() {
		rate := strconv.FormatFloat(t.Rate, 'f', -1, 64) + "%"
		taxLines = append(taxLines, map[string]any{"name": t.Label, "price": s.gooMinor(t.Amount), "rate": rate})
	}
	errs := []any{}
	for _, e := range cart.Errors() {
		errs = append(errs, map[string]any{"code": "woocommerce_rest_cart_item_error", "message": gooPlainMsg(e)})
	}
	methods := []string{}
	for _, g := range s.gooAvailableGateways(c, cart) {
		methods = append(methods, g.ID())
	}
	shipping := cu.Shipping
	if !cu.ShipToDifferent && len(cu.Shipping) == 0 {
		shipping = cu.Billing
	}
	return map[string]any{
		"items": items, "coupons": coupons, "fees": fees,
		"totals": s.gooWithCurrency(map[string]any{
			"total_items": s.gooMinor(cart.Subtotal), "total_items_tax": s.gooMinor(cart.SubtotalTax),
			"total_fees": s.gooMinor(cart.FeeTotal), "total_fees_tax": s.gooMinor(cart.FeeTax),
			"total_discount": s.gooMinor(cart.DiscountTotal), "total_discount_tax": s.gooMinor(cart.DiscountTax),
			"total_shipping": s.gooMinor(cart.ShippingTotal), "total_shipping_tax": s.gooMinor(cart.ShippingTax),
			"total_price": s.gooMinor(cart.Total), "total_tax": s.gooMinor(cart.TotalTax), "tax_lines": taxLines}, ""),
		"shipping_address": gooAddressJSON(shipping, false), "billing_address": gooAddressJSON(cu.Billing, true),
		"needs_payment": cart.NeedsPayment(), "needs_shipping": cart.NeedsShipping(), "payment_requirements": []string{"products"},
		"has_calculated_shipping": cu.CalculatedShip, "shipping_rates": rates, "items_count": cart.Count(), "items_weight": weight,
		"cross_sells": s.gooStoreCrossSells(ctx, cart), "errors": errs, "payment_methods": methods, "extensions": s.gcStoreExtensions(c, cart),
	}
}

// gooStoreCrossSells are the cross-sells of the cart items (not already in the cart).
func (s *Server) gooStoreCrossSells(ctx context.Context, cart *gooCart) []any {
	in := map[int64]bool{}
	var ids []int64
	for _, it := range cart.Items {
		in[it.ProductID] = true
	}
	for _, it := range cart.Items {
		p := it.product
		if p != nil && p.Type == gooTypeVariation {
			p = p.parent
		}
		var err error
		if p == nil || p.ID != it.ProductID { // the loaded product (or its parent) is the item's ProductID; otherwise load it
			p, err = s.gooGetProduct(ctx, it.ProductID)
		}
		if err == nil {
			for _, id := range p.CrossSellIDs {
				if !in[id] {
					in[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	out := []any{}
	if len(ids) == 0 {
		return out
	}
	m, _ := s.gooGetProducts(ctx, ids)
	var list []*gooProduct
	for _, id := range ids {
		if p := m[id]; p != nil && p.Status == "publish" {
			list = append(list, p)
		}
	}
	terms := s.gooRESTProductTerms(ctx, list)
	for _, p := range list {
		out = append(out, s.gooStoreProductJSON(ctx, p, terms))
	}
	return out
}

func (s *Server) gooStoreCartRespond(c *gin.Context, status int, cart *gooCart) {
	s.gooStoreRespond(c, status, s.gooStoreCartJSON(c, cart))
}

// gooStoreCartErr answers a cart error with the current cart in data.cart, like WooCommerce.
func (s *Server) gooStoreCartErr(c *gin.Context, status int, code, msg string, cart *gooCart) {
	s.gooSaveSession(c)
	s.gooStoreHeaders(c)
	restErr(c, status, code, gooPlainMsg(msg), map[string]any{"cart": s.gooStoreCartJSON(c, cart)})
}

// Cart routes --------------------------------------------------------------------------------------------

func (s *Server) gooStoreCartGet(c *gin.Context, req *restRequest) {
	s.gooStoreCartRespond(c, http.StatusOK, s.gooCartFor(c))
}

// gooStoreAdd adds {id, quantity, variation: [{attribute, value}]} to the cart.
func (s *Server) gooStoreAdd(c *gin.Context, req *restRequest) (*gooCart, string, *gooAPIErr) {
	ctx := c.Request.Context()
	cart := s.gooCartFor(c)
	id := gooInt(req.Body["id"])
	if id == 0 {
		if v, ok := req.Str(c, "id"); ok {
			id = gooInt(v)
		}
	}
	qty := 1
	if v, ok := req.Body["quantity"]; ok {
		qty = int(gooInt(v))
	} else if v, ok := req.Str(c, "quantity"); ok {
		qty = int(gooInt(v))
	}
	var variation map[string]string
	for _, x := range gooList(req.Body["variation"]) {
		m := gooObj(x)
		attr := strings.TrimPrefix(gooStr(m["attribute"]), "attribute_")
		val := gooStr(m["value"])
		if attr == "" {
			continue
		}
		if variation == nil {
			variation = map[string]string{}
		}
		// The attribute may be a label ("Color") or a taxonomy / key ("pa_color"); values may be names.
		key := s.gooResolveAttrKey(ctx, id, attr)
		variation["attribute_"+key] = s.gooResolveAttrValue(ctx, key, val)
	}
	if _, err := s.gooGetProduct(ctx, id); err != nil {
		return cart, "", gooErr(http.StatusBadRequest, "woocommerce_rest_cart_invalid_product", "This product cannot be added to the cart.")
	}
	form := gooStoreBookingForm(req.Body)
	if gf := gcStoreForm(req.Body); gf != nil {
		form = gf
	}
	if form == nil {
		form = paoStoreForm(req.Body)
	}
	key, err := cart.Add(ctx, id, qty, 0, variation, form, nil)
	if err != nil {
		return cart, "", gooErr(http.StatusBadRequest, "woocommerce_rest_add_to_cart_error", err.Error())
	}
	return cart, key, nil
}

// gooResolveAttrKey maps an attribute label or key to the product's attribute key.
func (s *Server) gooResolveAttrKey(ctx context.Context, productID int64, attr string) string {
	p, err := s.gooGetProduct(ctx, productID)
	if err == nil && p.Type == gooTypeVariation {
		p = s.gooParent(ctx, p)
	}
	if p != nil {
		for _, a := range p.Attributes {
			if strings.EqualFold(a.Key(), attr) || strings.EqualFold(s.gooAttrLabel(ctx, a), attr) || strings.EqualFold(a.Name, attr) {
				return a.Key()
			}
		}
	}
	return attr
}

// gooResolveAttrValue maps a term name to its slug for taxonomy attributes.
func (s *Server) gooResolveAttrValue(ctx context.Context, key, val string) string {
	if strings.HasPrefix(key, "pa_") {
		if _, err := s.store.TermBySlug(ctx, key, val); err == nil {
			return val
		}
		if t, err := s.store.TermByName(ctx, key, val); err == nil {
			return t.Slug
		}
	}
	return val
}

func (s *Server) gooStoreAddItem(c *gin.Context, req *restRequest) {
	cart, _, err := s.gooStoreAdd(c, req)
	if err != nil {
		s.gooStoreCartErr(c, err.Status, err.Code, err.Message, cart)
		return
	}
	s.gooStoreCartRespond(c, http.StatusCreated, cart)
}

func (s *Server) gooStoreKey(c *gin.Context, req *restRequest) string {
	if k := req.Params["key"]; k != "" {
		return k
	}
	k, _ := req.Str(c, "key")
	return k
}

func (s *Server) gooStoreRemoveItem(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	if !cart.Remove(s.gooStoreKey(c, req)) {
		s.gooStoreCartErr(c, http.StatusConflict, "woocommerce_rest_cart_invalid_key", "Cart item no longer exists or is invalid.", cart)
		return
	}
	s.gooStoreCartRespond(c, http.StatusOK, cart)
}

func (s *Server) gooStoreUpdateItem(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	key := s.gooStoreKey(c, req)
	if cart.Item(key) == nil {
		s.gooStoreCartErr(c, http.StatusConflict, "woocommerce_rest_cart_invalid_key", "Cart item no longer exists or is invalid.", cart)
		return
	}
	qty, _ := req.Int(c, "quantity")
	if err := cart.SetQuantity(c.Request.Context(), key, int(qty)); err != nil {
		s.gooStoreCartErr(c, http.StatusBadRequest, "woocommerce_rest_cart_invalid_quantity", err.Error(), cart)
		return
	}
	s.gooStoreCartRespond(c, http.StatusOK, cart)
}

func (s *Server) gooStoreApplyCoupon(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	code, _ := req.Str(c, "code")
	if err := s.gooApplyCouponLimited(c, cart, code); err != nil {
		s.gooStoreCartErr(c, http.StatusBadRequest, "woocommerce_rest_cart_coupon_error", err.Error(), cart)
		return
	}
	s.gooStoreCartRespond(c, http.StatusOK, cart)
}

func (s *Server) gooStoreRemoveCoupon(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	code := req.Params["code"]
	if code == "" {
		code, _ = req.Str(c, "code")
	}
	code, _ = url.PathUnescape(code)
	if !cart.RemoveCoupon(code) {
		s.gooStoreCartErr(c, http.StatusBadRequest, "woocommerce_rest_cart_coupon_invalid_code", "Coupon cannot be removed because it is not already applied to the cart.", cart)
		return
	}
	s.gooStoreCartRespond(c, http.StatusOK, cart)
}

// gooStoreSetCustomer copies billing_address / shipping_address of a request into the session.
func (s *Server) gooStoreSetCustomer(c *gin.Context, b map[string]any) {
	sess := s.gooSessionFor(c)
	cu := &sess.Data.Customer
	if v, ok := b["billing_address"]; ok {
		gooApplyAddress(cu.Billing, v)
	}
	if v, ok := b["shipping_address"]; ok {
		gooApplyAddress(cu.Shipping, v)
		delete(cu.Shipping, "email")
		cu.ShipToDifferent = true
	} else if _, ok := b["billing_address"]; ok && !cu.ShipToDifferent {
		for _, k := range []string{"first_name", "last_name", "company", "address_1", "address_2", "city", "state", "postcode", "country", "phone"} {
			cu.Shipping[k] = cu.Billing[k]
		}
	}
	cu.CalculatedShip = true
	sess.touch()
}

func (s *Server) gooStoreUpdateCustomer(c *gin.Context, req *restRequest) {
	s.gooStoreSetCustomer(c, req.Body)
	cart := s.gooCartForRecalc(c)
	s.gooStoreCartRespond(c, http.StatusOK, cart)
}

func (s *Server) gooStoreSelectRate(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	rate, _ := req.Str(c, "rate_id")
	pkg, _ := req.Str(c, "package_id")
	if rate == "" {
		s.gooStoreCartErr(c, http.StatusBadRequest, "woocommerce_rest_cart_missing_rate_id", "Invalid Rate ID.", cart)
		return
	}
	if !cart.NeedsShipping() {
		s.gooStoreCartErr(c, http.StatusBadRequest, "woocommerce_rest_cart_shipping_disabled", "Shipping is disabled.", cart)
		return
	}
	found := false
	for i, pr := range cart.Packages {
		if pkg != "*" && pkg != "" && pkg != strconv.Itoa(i) {
			continue
		}
		for _, r := range pr.Rates {
			if r.ID == rate {
				found = true
				cart.ChooseShipping(i, rate)
			}
		}
	}
	if !found {
		s.gooStoreCartErr(c, http.StatusBadRequest, "woocommerce_rest_cart_invalid_rate_id", "Invalid Rate ID.", cart)
		return
	}
	s.gooStoreCartRespond(c, http.StatusOK, cart)
}

func (s *Server) gooStoreItems(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	out := []any{}
	for _, it := range cart.Items {
		out = append(out, s.gooStoreItemJSON(c.Request.Context(), cart, it))
	}
	s.gooStoreRespond(c, http.StatusOK, out)
}

func (s *Server) gooStoreItemsCreate(c *gin.Context, req *restRequest) {
	cart, key, err := s.gooStoreAdd(c, req)
	if err != nil {
		s.gooStoreCartErr(c, err.Status, err.Code, err.Message, cart)
		return
	}
	s.gooStoreRespond(c, http.StatusCreated, s.gooStoreItemJSON(c.Request.Context(), cart, cart.Item(key)))
}

func (s *Server) gooStoreItemsDelete(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	cart.Empty()
	s.gooStoreRespond(c, http.StatusOK, []any{})
}

func (s *Server) gooStoreItem(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	it := cart.Item(req.Params["key"])
	if it == nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_cart_invalid_key", "Cart item does not exist.")
		return
	}
	s.gooStoreRespond(c, http.StatusOK, s.gooStoreItemJSON(c.Request.Context(), cart, it))
}

func (s *Server) gooStoreItemUpdate(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	key := req.Params["key"]
	if cart.Item(key) == nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_cart_invalid_key", "Cart item does not exist.")
		return
	}
	if v, ok := req.Int(c, "quantity"); ok {
		if err := cart.SetQuantity(c.Request.Context(), key, int(v)); err != nil {
			s.gooStoreCartErr(c, http.StatusBadRequest, "woocommerce_rest_cart_invalid_quantity", err.Error(), cart)
			return
		}
	}
	it := cart.Item(key)
	if it == nil {
		s.gooStoreRespond(c, http.StatusOK, map[string]any{})
		return
	}
	s.gooStoreRespond(c, http.StatusOK, s.gooStoreItemJSON(c.Request.Context(), cart, it))
}

func (s *Server) gooStoreItemDelete(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	if !cart.Remove(req.Params["key"]) {
		restErr(c, http.StatusNotFound, "woocommerce_rest_cart_invalid_key", "Cart item does not exist.")
		return
	}
	s.gooSaveSession(c)
	s.gooStoreHeaders(c)
	c.Status(http.StatusNoContent)
}

func (s *Server) gooStoreCouponJSON(cart *gooCart, cp *gooCoupon) map[string]any {
	return map[string]any{"code": cp.Code, "discount_type": cp.DiscountType,
		"totals": s.gooWithCurrency(map[string]any{"total_discount": s.gooMinor(cart.CouponExTax[cp.Code]), "total_discount_tax": s.gooMinor(cart.CouponTax[cp.Code])}, "")}
}

func (s *Server) gooStoreCoupons(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	out := []any{}
	for _, cp := range cart.Coupons() {
		out = append(out, s.gooStoreCouponJSON(cart, cp))
	}
	s.gooStoreRespond(c, http.StatusOK, out)
}

func (s *Server) gooStoreCouponsCreate(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	code, _ := req.Str(c, "code")
	if err := s.gooApplyCouponLimited(c, cart, code); err != nil {
		s.gooStoreCartErr(c, http.StatusBadRequest, "woocommerce_rest_cart_coupon_error", err.Error(), cart)
		return
	}
	for _, cp := range cart.Coupons() {
		if strings.EqualFold(cp.Code, code) {
			s.gooStoreRespond(c, http.StatusCreated, s.gooStoreCouponJSON(cart, cp))
			return
		}
	}
	s.gooStoreRespond(c, http.StatusCreated, map[string]any{"code": code})
}

func (s *Server) gooStoreCouponsDelete(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	for _, code := range append([]string{}, cart.AppliedCoupons...) {
		cart.RemoveCoupon(code)
	}
	s.gooStoreRespond(c, http.StatusOK, []any{})
}

func (s *Server) gooStoreCoupon(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	code, _ := url.PathUnescape(req.Params["code"])
	for _, cp := range cart.Coupons() {
		if strings.EqualFold(cp.Code, code) {
			s.gooStoreRespond(c, http.StatusOK, s.gooStoreCouponJSON(cart, cp))
			return
		}
	}
	restErr(c, http.StatusNotFound, "woocommerce_rest_cart_coupon_invalid_code", "Coupon does not exist in the cart.")
}

func (s *Server) gooStoreCouponDelete(c *gin.Context, req *restRequest) {
	cart := s.gooCartFor(c)
	code, _ := url.PathUnescape(req.Params["code"])
	if !cart.RemoveCoupon(code) {
		restErr(c, http.StatusNotFound, "woocommerce_rest_cart_coupon_invalid_code", "Coupon does not exist in the cart.")
		return
	}
	s.gooSaveSession(c)
	s.gooStoreHeaders(c)
	c.Status(http.StatusNoContent)
}

// Checkout ------------------------------------------------------------------------------------------------

// gooStoreCheckoutJSON is the CheckoutSchema for an order (or the session's customer data without one).
func (s *Server) gooStoreCheckoutJSON(c *gin.Context, o *gooOrder, payment map[string]any) map[string]any {
	cu := s.gooSessionFor(c).Data.Customer
	out := map[string]any{"order_id": 0, "status": "checkout-draft", "order_key": "", "order_number": "", "customer_note": "", "customer_id": 0,
		"billing_address": gooAddressJSON(cu.Billing, true), "shipping_address": gooAddressJSON(cu.Shipping, false),
		"payment_method": s.gooSessionFor(c).Data.ChosenPayment, "payment_result": payment, "extensions": map[string]any{}}
	if u := currentUser(c); u != nil {
		out["customer_id"] = u.ID
	}
	if o != nil {
		out["order_id"], out["status"], out["order_key"], out["order_number"] = o.ID, o.Status, o.OrderKey, o.Number()
		out["customer_note"], out["customer_id"], out["payment_method"] = o.CustomerNote, o.CustomerID, o.PaymentMethod
		out["billing_address"], out["shipping_address"] = gooAddressJSON(o.Billing, true), gooAddressJSON(o.Shipping, false)
	}
	if payment == nil {
		out["payment_result"] = map[string]any{"payment_status": "", "payment_details": []any{}, "redirect_url": ""}
	}
	return out
}

func (s *Server) gooStoreCheckoutGet(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	var o *gooOrder
	if id := s.gooSessionFor(c).Data.OrderAwaitingPayment; id != 0 {
		o, _ = s.gooGetOrder(ctx, id)
	}
	if o == nil {
		o = s.gooStoreDraftOrder(c)
	}
	s.gooStoreRespond(c, http.StatusOK, s.gooStoreCheckoutJSON(c, o, nil))
}

// gooDraftKey is the session key of the Store API's draft order.
const gooDraftKey = "store_api_draft_order"

// gooStoreDraftOrder creates or refreshes the session's checkout-draft order from the cart, the way the
// block checkout does on load and on every update (StoreApi\Utilities\OrderController). Drafts are
// hidden from the order screens, become the real order when the checkout is placed and are deleted
// after a day by the daily job. An empty cart has no draft.
func (s *Server) gooStoreDraftOrder(c *gin.Context) *gooOrder {
	ctx := c.Request.Context()
	_, existed := c.Get("goo.cart")
	cart := s.gooCartFor(c)
	if cart.IsEmpty() {
		return nil
	}
	if existed { // a cart built just now was calculated from the current session
		cart.Calculate()
	}
	sess := s.gooSessionFor(c)
	var o *gooOrder
	if id := gooInt(sess.Data.Extra[gooDraftKey]); id != 0 {
		if prev, err := s.gooGetOrder(ctx, id); err == nil && prev.Status == "checkout-draft" {
			o = prev
		}
	}
	if o == nil {
		o = &gooOrder{Status: "checkout-draft", CreatedVia: "store-api", Meta: map[string]any{}}
	}
	cu := sess.Data.Customer
	o.Currency, o.PricesIncludeTax = s.gooCurrency(), s.gooPricesIncludeTax()
	o.Billing, o.Shipping = map[string]string{}, map[string]string{}
	for k, v := range cu.Billing {
		o.Billing[k] = v
	}
	for k, v := range cu.Shipping {
		o.Shipping[k] = v
	}
	if u := currentUser(c); u != nil {
		o.CustomerID = u.ID
	}
	o.PaymentMethod = sess.Data.ChosenPayment
	o.Items, o.CartHash = s.gooOrderItemsFromCart(ctx, cart), gooCartHash(sess.Data.Cart)
	o.DiscountTotal, o.DiscountTax = cart.DiscountTotal, cart.DiscountTax
	o.ShippingTotal, o.ShippingTax = cart.ShippingTotal, cart.ShippingTax
	o.CartTax = gooRound(gooSumTaxes(cart.Taxes), s.gooDecimals())
	o.TotalTax, o.Total = cart.TotalTax, cart.Total
	if o.ID == 0 {
		if err := s.gooSaveOrder(ctx, o, true); err != nil {
			return nil
		}
	} else {
		// The draft was read without a lock, and a checkout of this session (POST) may have placed it
		// since: the status is checked again under the row lock the checkout's claim also takes, so
		// this refresh never writes the session's (stale) cart and addresses over a placed order. Nor
		// does it start a new draft or touch the session then: the checkout's session changes win.
		err := pgx.BeginFunc(ctx, s.store.Pool, func(tx pgx.Tx) error {
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM goo_orders WHERE id = $1 FOR UPDATE`, o.ID).Scan(&status); err != nil {
				return err
			}
			if status != "checkout-draft" {
				return errGooDraftPlaced
			}
			return s.gooSaveOrderTx(ctx, tx, o, true)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Deleted meanwhile (the daily cleanup): a new draft replaces it.
			delete(sess.Data.Extra, gooDraftKey)
			sess.touch()
			return s.gooStoreDraftOrder(c)
		}
		if err != nil {
			return nil
		}
	}
	if gooInt(sess.Data.Extra[gooDraftKey]) != o.ID {
		if sess.Data.Extra == nil {
			sess.Data.Extra = map[string]any{}
		}
		sess.Data.Extra[gooDraftKey] = o.ID
		sess.touch()
	}
	return o
}

// errGooDraftPlaced stops a draft refresh that found its draft placed by a checkout meanwhile.
var errGooDraftPlaced = errors.New("goo: the checkout draft was placed")

// gooCleanupDrafts deletes checkout-draft orders untouched for a day.
func (s *Server) gooCleanupDrafts(ctx context.Context) {
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_orders WHERE status = 'checkout-draft' AND date_modified < now() - interval '1 day'`)
}

func (s *Server) gooStoreCheckoutPut(c *gin.Context, req *restRequest) {
	s.gooStoreSetCustomer(c, req.Body)
	sess := s.gooSessionFor(c)
	if pm := gooStr(req.Body["payment_method"]); pm != "" {
		sess.Data.ChosenPayment = pm
		sess.touch()
	}
	s.gooStoreRespond(c, http.StatusOK, s.gooStoreCheckoutJSON(c, s.gooStoreDraftOrder(c), nil))
}

// gooStoreCheckoutPost places the order: the request becomes the checkout form and runs through
// gooProcessCheckout (validation, account creation, order, payment).
func (s *Server) gooStoreCheckoutPost(c *gin.Context, req *restRequest) {
	b := req.Body
	if len(b) == 0 {
		// A form-encoded checkout: read the posted fields as the body.
		_ = c.Request.ParseForm()
		b = map[string]any{}
		for k, v := range c.Request.PostForm {
			if len(v) > 0 {
				b[k] = v[0]
			}
		}
	}
	cart := s.gooCartFor(c)
	form := url.Values{}
	billing := gooObj(b["billing_address"])
	for k, v := range billing {
		form.Set("billing_"+k, gooStr(v))
	}
	if ship := gooObj(b["shipping_address"]); len(ship) > 0 && cart.NeedsShipping() {
		differs := false
		for k, v := range ship {
			if k == "email" {
				continue
			}
			form.Set("shipping_"+k, gooStr(v))
			if gooStr(v) != gooStr(billing[k]) {
				differs = true
			}
		}
		if differs {
			form.Set("ship_to_different_address", "1")
		}
	}
	form.Set("payment_method", gooStr(b["payment_method"]))
	form.Set("order_comments", gooStr(b["customer_note"]))
	form.Set("terms", "1") // the block checkout validates the terms checkbox on the client
	if gooBool(b["create_account"]) {
		form.Set("createaccount", "1")
		form.Set("account_password", gooStr(b["customer_password"]))
	}
	for k, v := range s.gooSessionFor(c).Data.ChosenShipping {
		form.Set("shipping_method["+k+"]", v)
	}
	for _, x := range gooList(b["payment_data"]) {
		m := gooObj(x)
		if k := gooStr(m["key"]); k != "" {
			form.Set(k, gooStr(m["value"]))
		}
	}
	// Gateways read their fields with c.PostForm, so the request's form is the built one.
	c.Request.PostForm, c.Request.Form = form, form
	if cart.IsEmpty() {
		s.gooStoreRespondErr(c, http.StatusBadRequest, "woocommerce_rest_cart_empty", "Cannot create order from empty cart.")
		return
	}
	res, o, errs := s.gooProcessCheckout(c, form)
	if len(errs) == 1 && errs[0] == gooThrottledMsg {
		s.gooStoreRespondErr(c, http.StatusTooManyRequests, "woocommerce_rest_checkout_throttled", gooThrottledMsg)
		return
	}
	if len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, gooPlainMsg(e))
		}
		code := "woocommerce_rest_checkout_invalid"
		if o != nil {
			code = "woocommerce_rest_checkout_process_payment_error"
		}
		s.gooStoreRespondErr(c, http.StatusBadRequest, code, strings.Join(msgs, " "))
		return
	}
	if o != nil {
		o, _ = s.gooGetOrder(c.Request.Context(), o.ID)
	}
	payment := map[string]any{"payment_status": "success", "payment_details": []any{}, "redirect_url": ""}
	if res != nil {
		payment["redirect_url"] = res.Redirect
		if res.Result != "success" {
			payment["payment_status"] = "failure"
		}
	}
	if o != nil && o.Status == "pending" {
		payment["payment_status"] = "pending"
	}
	s.gooStoreRespond(c, http.StatusOK, s.gooStoreCheckoutJSON(c, o, payment))
}

func (s *Server) gooStoreRespondErr(c *gin.Context, status int, code, msg string) {
	s.gooSaveSession(c)
	s.gooStoreHeaders(c)
	restErr(c, status, code, msg)
}

// Order -------------------------------------------------------------------------------------------------

// gooStoreOrderGet is /order/{id}?key=…&billing_email=… (the order confirmation's data).
func (s *Server) gooStoreOrderGet(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	key := c.Query("key")
	if !s.gooOrderGetAllowed(c, key) {
		restErr(c, http.StatusTooManyRequests, "woocommerce_rest_order_throttled", gooThrottledMsg)
		return
	}
	o, err := s.gooGetOrder(ctx, req.ID())
	if err != nil || o.Type != "shop_order" || !gooOrderKeyMatches(o.OrderKey, key) {
		s.gooOrderGetFailed(c, key)
		restErr(c, http.StatusNotFound, "woocommerce_rest_invalid_order", "Invalid order ID or key provided.")
		return
	}
	u := currentUser(c)
	switch {
	case o.CustomerID != 0 && u != nil && u.ID == o.CustomerID:
	case o.CustomerID == 0 && strings.EqualFold(strings.TrimSpace(c.Query("billing_email")), o.Billing["email"]):
	default:
		s.gooOrderGetFailed(c, key)
		restErr(c, http.StatusUnauthorized, "woocommerce_rest_invalid_user", "This order belongs to a different customer.")
		return
	}
	s.gooStoreRespond(c, http.StatusOK, s.gooStoreOrderJSON(ctx, o))
}

// gooStoreOrderJSON is the Store API's OrderSchema.
func (s *Server) gooStoreOrderJSON(ctx context.Context, o *gooOrder) map[string]any {
	cur := o.Currency
	items := []any{}
	for _, it := range o.ItemsOf("line_item") {
		if !s.gooOrderItemVisible(ctx, it, "store_api") {
			continue
		}
		pid := it.ProductID
		if it.VariationID != 0 {
			pid = it.VariationID
		}
		var images []any
		sku, permalink, typ := "", "", "simple"
		if p, err := s.gooGetProduct(ctx, pid); err == nil {
			sku, permalink, typ = p.SKU, s.gooPermalink(ctx, p), p.Type
			if im := s.gooImageJSON(ctx, s.gooImageOf(ctx, p)); im != nil {
				images = append(images, im)
			}
		}
		unit := 0.0
		if it.Quantity != 0 {
			unit = it.Subtotal / it.Quantity
		}
		items = append(items, map[string]any{"key": itoa64(it.ID), "id": pid, "quantity": it.Qty(), "name": it.Name, "sku": sku, "permalink": permalink,
			"type": typ, "images": nonNilSlice(images), "variation": []any{}, "item_data": []any{},
			"prices":     s.gooWithCurrency(map[string]any{"price": s.gooMinor(unit), "regular_price": s.gooMinor(unit), "sale_price": s.gooMinor(unit)}, cur),
			"totals":     s.gooWithCurrency(map[string]any{"line_subtotal": s.gooMinor(it.Subtotal), "line_subtotal_tax": s.gooMinor(it.SubtotalTax), "line_total": s.gooMinor(it.Total), "line_total_tax": s.gooMinor(it.TotalTax)}, cur),
			"extensions": map[string]any{}})
	}
	coupons := []any{}
	for _, it := range o.ItemsOf("coupon") {
		coupons = append(coupons, map[string]any{"code": it.Name, "totals": s.gooWithCurrency(map[string]any{"total_discount": s.gooMinor(it.Total), "total_discount_tax": s.gooMinor(it.TotalTax)}, cur)})
	}
	fees := []any{}
	var feeTotal, feeTax float64
	for _, it := range o.ItemsOf("fee") {
		feeTotal += it.Total
		feeTax += it.TotalTax
		fees = append(fees, map[string]any{"key": itoa64(it.ID), "name": it.Name, "totals": s.gooWithCurrency(map[string]any{"total": s.gooMinor(it.Total), "total_tax": s.gooMinor(it.TotalTax)}, cur)})
	}
	var subtotalTax float64
	for _, it := range o.ItemsOf("line_item") {
		subtotalTax += it.SubtotalTax
	}
	taxLines := []any{}
	for _, it := range o.ItemsOf("tax") {
		amt, _ := gooFloat(it.Meta["tax_amount"])
		ship, _ := gooFloat(it.Meta["shipping_tax_amount"])
		rate, _ := gooFloat(it.Meta["rate_percent"])
		taxLines = append(taxLines, map[string]any{"name": gooStr(it.Meta["label"]), "price": s.gooMinor(amt + ship), "rate": strconv.FormatFloat(rate, 'f', -1, 64) + "%"})
	}
	refunded := s.gooTotalRefunded(ctx, o.ID)
	return map[string]any{"id": o.ID, "status": o.Status, "items": items, "coupons": coupons, "fees": fees,
		"totals": s.gooWithCurrency(map[string]any{"subtotal": s.gooMinor(o.Subtotal()), "total_discount": s.gooMinor(o.DiscountTotal),
			"total_shipping": s.gooMinor(o.ShippingTotal), "total_fees": s.gooMinor(feeTotal), "total_tax": s.gooMinor(o.TotalTax),
			"total_refund": s.gooMinor(refunded), "total_price": s.gooMinor(o.Total), "total_items": s.gooMinor(o.Subtotal()),
			"total_items_tax": s.gooMinor(subtotalTax), "total_fees_tax": s.gooMinor(feeTax), "total_discount_tax": s.gooMinor(o.DiscountTax),
			"total_shipping_tax": s.gooMinor(o.ShippingTax), "tax_lines": taxLines}, cur),
		"shipping_address": gooAddressJSON(o.Shipping, false), "billing_address": gooAddressJSON(o.Billing, true),
		"needs_payment": containsStr([]string{"pending", "failed"}, o.Status) && o.Total > 0, "needs_shipping": len(o.ItemsOf("shipping")) > 0,
		"payment_requirements": []string{"products"}, "errors": []any{}}
}
