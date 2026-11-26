package server

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// The cart (WC_Cart) and its totals (WC_Cart_Totals). A gooCart is built per request from the session
// and recalculated after every change: item prices (and the modules' price hooks), coupon discounts,
// line taxes, fees, shipping packages and rates, then the totals.

type gooCartItem struct {
	Key         string
	ProductID   int64
	VariationID int64
	Variation   map[string]string // attribute_pa_color → red
	Quantity    int
	Data        map[string]any

	product    *gooProduct // the variation for a variable product
	categories []int64     // product_cat term ids of the (parent) product

	// Price is the unit price in the entered terms (including tax when prices include tax),
	// after the modules' price hooks. Subtotal = Price × Quantity, before discounts.
	Price    float64
	Subtotal float64
	Discount float64 // in entered terms
	discount float64 // minor units

	LineSubtotal      float64 // excluding tax, before discounts
	LineSubtotalTax   float64
	LineTotal         float64 // excluding tax, after discounts
	LineTax           float64
	LineSubtotalTaxes map[int64]float64
	LineTaxes         map[int64]float64
	TaxClass          string
	Taxable           bool
}

// Product is the cart item's product (the variation for variable products).
func (it *gooCartItem) Product() *gooProduct { return it.product }

// gooFee is a cart fee (WC_Cart_Fees): positive or negative.
type gooFee struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Amount   float64           `json:"amount"`
	Taxable  bool              `json:"taxable"`
	TaxClass string            `json:"tax_class"`
	Total    float64           `json:"total"`
	Tax      float64           `json:"tax"`
	Taxes    map[int64]float64 `json:"taxes"`
	Meta     map[string]any    `json:"meta,omitempty"`
}

// gooPackageRates is a package with its calculated rates and the chosen one.
type gooPackageRates struct {
	Package *gooPackage
	Rates   []*gooShippingRate
	Chosen  *gooShippingRate
}

type gooCart struct {
	s    *Server
	c    *gin.Context
	sess *gooSession

	Items          []*gooCartItem
	AppliedCoupons []string
	coupons        []*gooCoupon
	Fees           []*gooFee
	Packages       []*gooPackageRates
	CouponTotals   map[string]float64 // entered terms
	CouponExTax    map[string]float64
	CouponTax      map[string]float64

	Subtotal, SubtotalTax              float64 // line subtotals (ex tax) and their tax
	CartContentsTotal, CartContentsTax float64
	DiscountTotal, DiscountTax         float64
	ShippingTotal, ShippingTax         float64
	FeeTotal, FeeTax                   float64
	TotalTax, Total                    float64
	Taxes                              map[int64]float64 // cart contents + fee taxes per rate
	ShippingTaxes                      map[int64]float64
	SurchargeBase                      float64 // the total before payment surcharges (gooCartSurchargeHooks)

	TaxLocation gooLocation
	rateCache   map[string][]gooTaxRate // tax rates looked up by Calculate, reused for the request
	errors      []string                // validation messages produced while loading (removed items, coupons)
}

func gooMD5(v string) string {
	sum := md5.Sum([]byte(v))
	return hex.EncodeToString(sum[:])
}

type gooCartCtxKey struct{}

// gooCartFromCtx is the cart an add-to-cart hook runs for.
func gooCartFromCtx(ctx context.Context) *gooCart {
	c, _ := ctx.Value(gooCartCtxKey{}).(*gooCart)
	return c
}

// gooCartItemKey is WC_Cart::generate_cart_id().
func gooCartItemKey(productID, variationID int64, variation map[string]string, data map[string]any) string {
	parts := []string{itoa64(productID)}
	if variationID != 0 {
		parts = append(parts, itoa64(variationID))
	}
	if len(variation) > 0 {
		keys := make([]string, 0, len(variation))
		for k := range variation {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var vs []string
		for _, k := range keys {
			vs = append(vs, k+"="+variation[k])
		}
		parts = append(parts, strings.Join(vs, "&"))
	}
	if len(data) > 0 {
		raw, _ := json.Marshal(data) // encoding/json sorts map keys
		parts = append(parts, string(raw))
	}
	return gooMD5(strings.Join(parts, "_"))
}

// gooCartFor returns the request's cart (WC()->cart), calculated.
func (s *Server) gooCartFor(c *gin.Context) *gooCart {
	if v, ok := c.Get("goo.cart"); ok {
		return v.(*gooCart)
	}
	cart := &gooCart{s: s, c: c, sess: s.gooSessionFor(c)}
	cart.load(c.Request.Context())
	cart.Calculate()
	c.Set("goo.cart", cart)
	return cart
}

// gooCartForRecalc is gooCartFor followed by Calculate when the cart was built earlier in the request
// (before the caller changed the session); a cart built just now was calculated from the current session.
func (s *Server) gooCartForRecalc(c *gin.Context) *gooCart {
	_, existed := c.Get("goo.cart")
	cart := s.gooCartFor(c)
	if existed {
		cart.Calculate()
	}
	return cart
}

// load builds the items from the session, dropping products that cannot be bought anymore.
func (cart *gooCart) load(ctx context.Context) {
	s := cart.s
	stored := s.gooPllCartItems(ctx, cart.sess.Data.Cart) // Golylang: the products in the visitor's language
	ids := make([]int64, 0, len(stored)*2)
	for _, it := range stored {
		ids = append(ids, it.ProductID)
		if it.VariationID != 0 {
			ids = append(ids, it.VariationID)
		}
	}
	products, _ := s.gooGetProducts(ctx, ids)
	cats, _ := s.store.TermsForPosts(ctx, ids, "product_cat")
	var keep []gooStoredItem
	for _, st := range stored {
		pid := st.ProductID
		if st.VariationID != 0 {
			pid = st.VariationID
		}
		if st.Quantity <= 0 || st.Quantity > gooMaxLineQty { // never from Add / SetQuantity: a damaged or forged session
			cart.sess.touch()
			continue
		}
		p := products[pid]
		if p != nil && (!cart.mayBuyPrivate(p) || !cart.mayBuyPrivate(products[st.ProductID])) {
			p = nil
		}
		if p == nil || !s.gooIsPurchasable(ctx, p) {
			name := "A product"
			if p != nil {
				name = p.Name
			}
			cart.errors = append(cart.errors, fmt.Sprintf("%s has been removed from your cart because it can no longer be purchased. Please contact us if you need assistance.", html.EscapeString(name)))
			cart.sess.touch()
			continue
		}
		if parent := products[st.ProductID]; parent != nil && p.Type == gooTypeVariation {
			p.parent = parent
		}
		it := &gooCartItem{Key: st.Key, ProductID: st.ProductID, VariationID: st.VariationID, Variation: st.Variation, Quantity: st.Quantity, Data: st.Data, product: p}
		for _, t := range cats[st.ProductID] {
			it.categories = append(it.categories, t.ID)
			if t.ParentID != 0 {
				it.categories = append(it.categories, t.ParentID)
			}
		}
		if it.Data == nil {
			it.Data = map[string]any{}
		}
		cart.Items = append(cart.Items, it)
		keep = append(keep, st)
	}
	if len(keep) != len(stored) {
		cart.sess.Data.Cart = keep
	}
	for _, code := range cart.sess.Data.AppliedCoupons {
		if cp, err := s.gooCouponByCode(ctx, code); err == nil {
			cart.coupons = append(cart.coupons, cp)
			cart.AppliedCoupons = append(cart.AppliedCoupons, cp.Code)
		} else {
			cart.errors = append(cart.errors, fmt.Sprintf(`Coupon "%s" has been removed from your cart because it no longer exists.`, html.EscapeString(code)))
			cart.sess.touch()
		}
	}
	// An individual-use coupon stands alone. ApplyCoupon enforces that, but logging in merges the
	// visitor's coupons into the customer's, which could pair it with another one.
	if len(cart.coupons) > 1 {
		for _, cp := range cart.coupons {
			if cp.IndividualUse {
				cart.errors = append(cart.errors, fmt.Sprintf(`Sorry, coupon "%s" has already been applied and cannot be used in conjunction with other coupons.`, html.EscapeString(cp.Code)))
				cart.coupons, cart.AppliedCoupons = []*gooCoupon{cp}, []string{cp.Code}
				cart.sess.touch()
				break
			}
		}
	}
	cart.sess.Data.AppliedCoupons = cart.AppliedCoupons
}

// persist writes the items back into the session.
func (cart *gooCart) persist() {
	out := make([]gooStoredItem, 0, len(cart.Items))
	for _, it := range cart.Items {
		out = append(out, gooStoredItem{Key: it.Key, ProductID: it.ProductID, VariationID: it.VariationID, Variation: it.Variation, Quantity: it.Quantity, Data: it.Data})
	}
	cart.sess.Data.Cart = out
	cart.sess.Data.AppliedCoupons = cart.AppliedCoupons
	cart.sess.touch()
}

// IsEmpty is WC_Cart::is_empty().
func (cart *gooCart) IsEmpty() bool { return len(cart.Items) == 0 }

// Count is get_cart_contents_count().
func (cart *gooCart) Count() int {
	n := 0
	for _, it := range cart.Items {
		n += it.Quantity
	}
	return n
}

// Item finds an item by key.
func (cart *gooCart) Item(key string) *gooCartItem {
	for _, it := range cart.Items {
		if it.Key == key {
			return it
		}
	}
	return nil
}

// mayBuyPrivate is the status half of is_purchasable(): a private product is for sale only to users who
// may edit it (shop staff testing it), never to visitors who guess its ID.
func (cart *gooCart) mayBuyPrivate(p *gooProduct) bool {
	if p == nil || p.Status != "private" {
		return true
	}
	var u *store.User
	if cart.c != nil {
		u = currentUser(cart.c)
	}
	return u != nil && p.Post != nil && u.Can("edit_post", p.Post)
}

// gooMaxLineQty caps the quantity of a cart line. Quantities are machine integers: without a cap,
// adding MaxInt64 twice to a line wraps it to a negative quantity, a negative line total and a free
// (or discounted) order, with stock "returned" on payment.
const gooMaxLineQty = 1_000_000

// gooCartError is an add-to-cart / update failure with WooCommerce's message (HTML-safe).
type gooCartError struct{ Msg string }

func (e *gooCartError) Error() string { return e.Msg }

func gooCartErr(format string, a ...any) error { return &gooCartError{fmt.Sprintf(format, a...)} }

// Add is WC_Cart::add_to_cart(): validates the product, the variation, stock and the modules'
// add-to-cart hooks, then adds the quantity (or increases an existing line). Returns the item key.
func (cart *gooCart) Add(ctx context.Context, productID int64, qty int, variationID int64, variation map[string]string, form map[string][]string, data map[string]any) (string, error) {
	s := cart.s
	if qty <= 0 {
		return "", gooCartErr("Please choose a quantity of items to add to your cart.")
	}
	if qty > gooMaxLineQty {
		return "", gooCartErr("You cannot add that amount to the cart.")
	}
	p, err := s.gooGetProduct(ctx, productID)
	if err != nil || (p.Status != "publish" && p.Status != "private") || !cart.mayBuyPrivate(p) {
		return "", gooCartErr("Sorry, this product cannot be purchased.")
	}
	if p.Type == gooTypeVariation {
		variationID, productID = p.ID, p.ParentID
		if variation == nil {
			variation = map[string]string{}
		}
		for k, v := range p.VariationAttrs {
			if v != "" {
				variation["attribute_"+k] = v
			}
		}
		p, err = s.gooGetProduct(ctx, productID)
		if err != nil || (p.Status != "publish" && p.Status != "private") || !cart.mayBuyPrivate(p) {
			return "", gooCartErr("Sorry, this product cannot be purchased.")
		}
	}
	buy := p
	if gooSubsVariableLike(p.Type) {
		if variation == nil {
			variation = map[string]string{}
		}
		attrs := map[string]string{}
		for k, v := range variation {
			attrs[strings.TrimPrefix(k, "attribute_")] = v
		}
		var v *gooProduct
		if variationID != 0 {
			v, err = s.gooGetProduct(ctx, variationID)
			if err != nil || v.ParentID != p.ID {
				v = nil
			}
		}
		if v == nil {
			v, err = s.gooMatchVariation(ctx, p, attrs)
			if err != nil {
				var missing []string
				for _, a := range p.Attributes {
					if a.Variation && attrs[a.Key()] == "" {
						missing = append(missing, s.gooAttrLabel(ctx, a))
					}
				}
				if len(missing) > 0 {
					return "", gooCartErr("%s %s.", map[bool]string{true: "is a required field", false: "are required fields"}[len(missing) == 1], html.EscapeString(strings.Join(missing, ", ")))
				}
				return "", gooCartErr("Please choose product options&hellip;")
			}
		}
		v.parent = p
		variationID, buy = v.ID, v
		// The stored variation lists every variation attribute, with the chosen values for "any" ones.
		full := map[string]string{}
		for _, a := range p.Attributes {
			if !a.Variation {
				continue
			}
			val := v.VariationAttrs[a.Key()]
			if val == "" {
				val = attrs[a.Key()]
				if val == "" {
					return "", gooCartErr("%s is a required field.", html.EscapeString(s.gooAttrLabel(ctx, a)))
				}
			}
			full["attribute_"+a.Key()] = val
		}
		variation = full
	} else {
		variation, variationID = nil, 0
	}
	if !s.gooIsPurchasable(ctx, buy) {
		if p.Type == gooTypeExternal || p.Type == gooTypeGrouped {
			return "", gooCartErr("Sorry, this product cannot be purchased.")
		}
		return "", gooCartErr("Sorry, this product cannot be purchased.")
	}
	if !s.gooIsInStock(ctx, buy) {
		return "", gooCartErr("You cannot add &quot;%s&quot; to the cart because the product is out of stock.", html.EscapeString(p.Name))
	}
	if data == nil {
		data = map[string]any{}
	}
	hctx := context.WithValue(ctx, gooCartCtxKey{}, cart) // hooks find the cart with gooCartFromCtx
	for _, h := range gooAddToCartHooks {
		if err := h(s, hctx, buy, qty, form, data); err != nil {
			return "", err
		}
	}
	key := gooCartItemKey(productID, variationID, variation, data)
	existing := cart.Item(key)
	if buy.SoldIndividually || p.SoldIndividually {
		qty = 1
		if existing != nil {
			return "", gooCartErr(`<a href="%s" tabindex="1" class="button wc-forward">View cart</a> You cannot add another &quot;%s&quot; to your cart.`, html.EscapeString(s.gooPageURL(ctx, "cart")), html.EscapeString(p.Name))
		}
	}
	inCart := qty
	for _, it := range cart.Items { // quantities of the same stock owner already in the cart
		if it.product != nil && s.gooStockOwner(ctx, it.product).ID == s.gooStockOwner(ctx, buy).ID {
			inCart += it.Quantity
		}
	}
	if !s.gooHasEnoughStock(ctx, buy, inCart) {
		stock := s.gooStockQty(ctx, buy)
		if inCart-qty > 0 {
			return "", gooCartErr(`<a href="%s" tabindex="1" class="button wc-forward">View cart</a> You cannot add that amount to the cart &mdash; we have %d in stock and you already have %d in your cart.`, html.EscapeString(s.gooPageURL(ctx, "cart")), stock, inCart-qty)
		}
		return "", gooCartErr("You cannot add that amount of &quot;%s&quot; to the cart because there is not enough stock (%d remaining).", html.EscapeString(p.Name), stock)
	}
	if existing != nil {
		if existing.Quantity+qty > gooMaxLineQty {
			return "", gooCartErr("You cannot add that amount to the cart.")
		}
		existing.Quantity += qty
	} else {
		buy.parent = nil
		if buy.Type == gooTypeVariation {
			buy.parent = p
		}
		it := &gooCartItem{Key: key, ProductID: productID, VariationID: variationID, Variation: variation, Quantity: qty, Data: data, product: buy}
		if cats, err := s.store.TermsForPosts(ctx, []int64{productID}, "product_cat"); err == nil {
			for _, t := range cats[productID] {
				it.categories = append(it.categories, t.ID)
			}
		}
		cart.Items = append(cart.Items, it)
	}
	cart.persist()
	cart.Calculate()
	return key, nil
}

// soldIndividually is is_sold_individually() of the item: a variation inherits its parent's flag.
func (it *gooCartItem) soldIndividually() bool {
	return it.product != nil && (it.product.SoldIndividually || (it.product.parent != nil && it.product.parent.SoldIndividually))
}

// SetQuantity is WC_Cart::set_quantity(): 0 removes the item. Stock is checked.
func (cart *gooCart) SetQuantity(ctx context.Context, key string, qty int) error {
	it := cart.Item(key)
	if it == nil {
		return gooCartErr("Cart item not found.")
	}
	if qty <= 0 {
		cart.Remove(key)
		return nil
	}
	if qty > gooMaxLineQty {
		return gooCartErr("You cannot add that amount to the cart.")
	}
	if it.soldIndividually() && qty > 1 {
		return gooCartErr("You can only have 1 %s in your cart.", html.EscapeString(it.product.Name))
	}
	s := cart.s
	total := qty
	for _, o := range cart.Items {
		if o.Key != key && s.gooStockOwner(ctx, o.product).ID == s.gooStockOwner(ctx, it.product).ID {
			total += o.Quantity
		}
	}
	if !s.gooHasEnoughStock(ctx, it.product, total) {
		return gooCartErr("Sorry, we do not have enough &quot;%s&quot; in stock to fulfill your order (%d available). We apologize for any inconvenience caused.",
			html.EscapeString(s.gooProductTitle(ctx, it.product)), s.gooStockQty(ctx, it.product))
	}
	it.Quantity = qty
	cart.persist()
	cart.Calculate()
	return nil
}

// Remove is WC_Cart::remove_cart_item() (kept for "Undo?").
func (cart *gooCart) Remove(key string) bool {
	for i, it := range cart.Items {
		if it.Key == key {
			cart.sess.Data.Removed = append(cart.sess.Data.Removed, gooStoredItem{Key: it.Key, ProductID: it.ProductID, VariationID: it.VariationID, Variation: it.Variation, Quantity: it.Quantity, Data: it.Data})
			if len(cart.sess.Data.Removed) > 5 {
				cart.sess.Data.Removed = cart.sess.Data.Removed[len(cart.sess.Data.Removed)-5:]
			}
			cart.Items = append(cart.Items[:i:i], cart.Items[i+1:]...)
			cart.persist()
			cart.Calculate()
			return true
		}
	}
	return false
}

// Restore is WC_Cart::restore_cart_item() (the "Undo?" link).
func (cart *gooCart) Restore(ctx context.Context, key string) bool {
	for i, st := range cart.sess.Data.Removed {
		if st.Key == key {
			cart.sess.Data.Removed = append(cart.sess.Data.Removed[:i:i], cart.sess.Data.Removed[i+1:]...)
			cart.sess.Data.Cart = append(cart.sess.Data.Cart, st)
			cart.Items = nil
			cart.coupons, cart.AppliedCoupons = nil, nil
			cart.load(ctx)
			cart.persist()
			cart.Calculate()
			return true
		}
	}
	return false
}

// Empty is WC_Cart::empty_cart().
func (cart *gooCart) Empty() {
	cart.Items, cart.AppliedCoupons, cart.coupons = nil, nil, nil
	cart.sess.Data.ChosenShipping = map[string]string{}
	cart.sess.Data.OrderAwaitingPayment = 0
	cart.persist()
	cart.Calculate()
	cart.s.gooFire(cart.c.Request.Context(), "cart_emptied", cart)
}

// ApplyCoupon is WC_Cart::apply_coupon().
func (cart *gooCart) ApplyCoupon(ctx context.Context, code string) error {
	s := cart.s
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return gooCouponErr("Please enter a coupon code.")
	}
	if !s.gooCouponsEnabled() {
		return gooCouponErr("Coupons are disabled.")
	}
	cp, err := s.gooCouponByCode(ctx, code)
	if err != nil {
		return gooCouponErr(`Coupon "%s" does not exist!`, html.EscapeString(code))
	}
	for _, have := range cart.AppliedCoupons {
		if strings.EqualFold(have, cp.Code) {
			return gooCouponErr("Coupon code already applied!")
		}
	}
	if err := s.gooValidateCoupon(ctx, cp, cart); err != nil {
		return err
	}
	if cp.IndividualUse {
		cart.coupons, cart.AppliedCoupons = nil, nil
	}
	for _, have := range cart.coupons {
		if have.IndividualUse {
			return gooCouponErr(`Sorry, coupon "%s" has already been applied and cannot be used in conjunction with other coupons.`, html.EscapeString(have.Code))
		}
	}
	cart.coupons = append(cart.coupons, cp)
	cart.AppliedCoupons = append(cart.AppliedCoupons, cp.Code)
	cart.persist()
	cart.Calculate()
	return nil
}

// RemoveCoupon is WC_Cart::remove_coupon().
func (cart *gooCart) RemoveCoupon(code string) bool {
	for i, c := range cart.AppliedCoupons {
		if strings.EqualFold(c, code) {
			cart.AppliedCoupons = append(cart.AppliedCoupons[:i:i], cart.AppliedCoupons[i+1:]...)
			cart.coupons = append(cart.coupons[:i:i], cart.coupons[i+1:]...)
			cart.persist()
			cart.Calculate()
			return true
		}
	}
	return false
}

// Coupons are the applied coupons.
func (cart *gooCart) Coupons() []*gooCoupon { return cart.coupons }

// gooCartSurchargeHooks add payment surcharges (cart.AddFee, never taxable) once shipping and taxes
// are known; cart.SurchargeBase is the amount they apply to.
var gooCartSurchargeHooks []gooCartFeesHook

// AddFee is WC_Cart::add_fee(), for gooCartFeesHooks.
func (cart *gooCart) AddFee(id, name string, amount float64, taxable bool, taxClass string) *gooFee {
	f := &gooFee{ID: defaultStr(id, gooSanitizeKey(name)), Name: name, Amount: amount, Taxable: taxable, TaxClass: taxClass}
	cart.Fees = append(cart.Fees, f)
	return f
}

// customerKeys identify the customer for per-user coupon limits: the user id and the billing e-mail.
func (cart *gooCart) customerKeys() []string {
	var out []string
	if u := currentUser(cart.c); u != nil {
		out = append(out, itoa64(u.ID), u.Email)
	}
	if e := cart.billingEmail(); e != "" {
		out = append(out, e)
	}
	return out
}

func (cart *gooCart) billingEmail() string {
	return strings.TrimSpace(cart.sess.Data.Customer.Billing["email"])
}

// displayedSubtotal is get_displayed_subtotal(): the subtotal including tax when the cart shows prices so.
func (cart *gooCart) displayedSubtotal() float64 {
	if cart.s.gooOptString("tax_display_cart") == "incl" {
		return cart.Subtotal + cart.SubtotalTax
	}
	return cart.Subtotal
}

// displayedContentsAfterDiscount is the contents total after discounts, in display terms.
func (cart *gooCart) displayedContentsAfterDiscount() float64 {
	if cart.s.gooOptString("tax_display_cart") == "incl" {
		return cart.CartContentsTotal + cart.CartContentsTax
	}
	return cart.CartContentsTotal
}

// NeedsShipping is WC_Cart::needs_shipping().
func (cart *gooCart) NeedsShipping() bool {
	if !cart.s.gooShippingEnabled() {
		return false
	}
	ctx := cart.c.Request.Context()
	for _, it := range cart.Items {
		if cart.s.gooNeedsShipping(ctx, it.product) {
			return true
		}
	}
	return false
}

// NeedsPayment is WC_Cart::needs_payment().
func (cart *gooCart) NeedsPayment() bool { return cart.Total > 0 }

// Customer returns the session's customer data.
func (cart *gooCart) Customer() *gooCustomerData { return &cart.sess.Data.Customer }

// shippingDestination is the package destination: the shipping address, or the billing one.
func (cart *gooCart) shippingDestination() gooLocation {
	cu := cart.sess.Data.Customer
	addr := cu.Billing
	if cart.s.gooOptString("ship_to_destination") != "billing_only" && (cu.ShipToDifferent || cart.s.gooOptString("ship_to_destination") == "shipping") && cu.Shipping["country"] != "" {
		addr = cu.Shipping
	}
	if addr["country"] == "" && cu.Shipping["country"] != "" {
		addr = cu.Shipping
	}
	loc := gooLocation{Country: addr["country"], State: addr["state"], Postcode: addr["postcode"], City: addr["city"]}
	if loc.Country == "" && cart.s.gooOptString("default_customer_address") != "" {
		loc = gooLocation{Country: cart.s.gooDefaultCountry(cart.c), State: cart.s.gooBaseState()}
		if loc.Country != cart.s.gooBaseCountry() {
			loc.State = ""
		}
	}
	return loc
}

// taxLocation is WC_Customer::get_taxable_address().
func (cart *gooCart) taxLocation() gooLocation {
	s := cart.s
	if cart.sess.Data.Customer.IsVATExempt {
		return gooLocation{}
	}
	for _, chosen := range cart.sess.Data.ChosenShipping {
		if strings.HasPrefix(chosen, "local_pickup") { // local pickup is taxed at the store
			return s.gooBaseLocation()
		}
	}
	cu := cart.sess.Data.Customer
	var addr map[string]string
	switch s.gooOptString("tax_based_on") {
	case "base":
		return s.gooBaseLocation()
	case "billing":
		addr = cu.Billing
	default:
		addr = cu.Billing
		if cu.ShipToDifferent && cu.Shipping["country"] != "" {
			addr = cu.Shipping
		} else if addr["country"] == "" && cu.Shipping["country"] != "" {
			addr = cu.Shipping
		}
	}
	loc := gooLocation{Country: addr["country"], State: addr["state"], Postcode: addr["postcode"], City: addr["city"]}
	if loc.Country == "" {
		if s.gooOptString("default_customer_address") == "" {
			return gooLocation{}
		}
		return s.gooBaseLocation()
	}
	return loc
}

// Calculate is WC_Cart::calculate_totals().
func (cart *gooCart) Calculate() {
	s := cart.s
	ctx := cart.c.Request.Context()
	dp := s.gooDecimals()
	incl := s.gooPricesIncludeTax()
	roundAtSubtotal := s.gooOptBool("tax_round_at_subtotal")
	loc := cart.taxLocation()
	cart.TaxLocation = loc
	base := s.gooBaseLocation()
	if cart.rateCache == nil {
		cart.rateCache = map[string][]gooTaxRate{}
	}
	rateCache := cart.rateCache
	ratesFor := func(class string, l gooLocation, ship bool) []gooTaxRate {
		k := fmt.Sprintf("%s|%v|%v", class, l, ship)
		if r, ok := rateCache[k]; ok {
			return r
		}
		r := s.gooFindRates(ctx, class, l, ship)
		rateCache[k] = r
		return r
	}
	rnd := func(m map[int64]float64) map[int64]float64 {
		if roundAtSubtotal {
			return m
		}
		return gooRoundTaxes(m, dp)
	}

	// 1. Item prices and subtotals.
	for _, it := range cart.Items {
		p := it.product
		it.Price = p.PriceValue()
		it.TaxClass = s.gooTaxClassOf(ctx, p)
		it.Taxable = s.gooTaxEnabled() && p.TaxStatus == "taxable"
	}
	for _, h := range gooCartItemPriceHooks {
		for _, it := range cart.Items {
			h(s, ctx, cart, it)
		}
	}
	cart.dropInvalidLines()
	for _, it := range cart.Items {
		it.Subtotal = it.Price * float64(it.Quantity)
		it.LineSubtotalTaxes = map[int64]float64{}
		if !it.Taxable {
			it.LineSubtotal = it.Subtotal
			continue
		}
		rates := ratesFor(it.TaxClass, loc, false)
		if incl {
			baseRates := ratesFor(it.TaxClass, base, false)
			if !gooSameRates(baseRates, rates) {
				// Non-base location: remove the base taxes, add the customer's (woocommerce_adjust_non_base_location_prices).
				ex := it.Subtotal - gooSumTaxes(gooCalcTax(it.Subtotal, baseRates, true))
				it.LineSubtotalTaxes = rnd(gooCalcTax(ex, rates, false))
				it.Subtotal = ex + gooSumTaxes(it.LineSubtotalTaxes)
				it.LineSubtotal = ex
			} else {
				it.LineSubtotalTaxes = rnd(gooCalcTax(it.Subtotal, rates, true))
				it.LineSubtotal = it.Subtotal - gooSumTaxes(it.LineSubtotalTaxes)
			}
		} else {
			it.LineSubtotal = it.Subtotal
			it.LineSubtotalTaxes = rnd(gooCalcTax(it.Subtotal, rates, false))
		}
	}
	cart.Subtotal, cart.SubtotalTax = 0, 0
	for _, it := range cart.Items {
		cart.Subtotal += it.LineSubtotal
		it.LineSubtotalTax = gooSumTaxes(it.LineSubtotalTaxes)
		cart.SubtotalTax += it.LineSubtotalTax
	}

	// 2. Coupons: drop invalid ones, then discount the items.
	var valid []*gooCoupon
	var codes []string
	for _, cp := range cart.coupons {
		if err := s.gooValidateCoupon(ctx, cp, cart); err != nil {
			cart.errors = append(cart.errors, err.Error())
			cart.sess.touch()
			continue
		}
		valid = append(valid, cp)
		codes = append(codes, cp.Code)
	}
	cart.coupons, cart.AppliedCoupons = valid, codes
	cart.sess.Data.AppliedCoupons = codes
	s.gooApplyDiscounts(ctx, cart)

	// 3. Line totals and taxes after discounts.
	cart.Taxes = map[int64]float64{}
	cart.CartContentsTotal, cart.CartContentsTax = 0, 0
	for _, it := range cart.Items {
		entered := it.Subtotal - it.Discount
		it.LineTaxes = map[int64]float64{}
		if it.Taxable {
			rates := ratesFor(it.TaxClass, loc, false)
			if incl {
				it.LineTaxes = rnd(gooCalcTax(entered, rates, true))
				it.LineTotal = entered - gooSumTaxes(it.LineTaxes)
			} else {
				it.LineTotal = entered
				it.LineTaxes = rnd(gooCalcTax(entered, rates, false))
			}
		} else {
			it.LineTotal = entered
		}
		if !roundAtSubtotal {
			// Rounding per line. Rounding at subtotal keeps the lines unrounded, like their taxes, and
			// rounds the sums once (step 6), so prices entered with tax add up to the shelf total
			// (WC_Cart_Totals::round_item_subtotal is skipped).
			it.LineTotal = gooRound(it.LineTotal, dp)
			it.LineSubtotal = gooRound(it.LineSubtotal, dp)
		}
		it.LineTax = gooSumTaxes(it.LineTaxes)
		cart.CartContentsTotal += it.LineTotal
		cart.CartContentsTax += it.LineTax
		for k, v := range it.LineTaxes {
			cart.Taxes[k] += v
		}
	}
	cart.DiscountTotal = gooRound(cart.Subtotal-cart.CartContentsTotal, dp)
	cart.DiscountTax = gooRound(cart.SubtotalTax-cart.CartContentsTax, dp)
	// Per-coupon amounts excluding tax and their tax, proportional to the entered discount.
	cart.CouponExTax, cart.CouponTax = map[string]float64{}, map[string]float64{}
	var enteredDiscount float64
	for _, v := range cart.CouponTotals {
		enteredDiscount += v
	}
	for code, v := range cart.CouponTotals {
		if enteredDiscount > 0 {
			cart.CouponExTax[code] = gooRound(cart.DiscountTotal*v/enteredDiscount, dp)
			cart.CouponTax[code] = gooRound(cart.DiscountTax*v/enteredDiscount, dp)
		}
	}

	// 4. Fees.
	cart.Fees = nil
	for _, h := range gooCartFeesHooks {
		h(s, ctx, cart)
	}
	cart.FeeTotal, cart.FeeTax = 0, 0
	for _, f := range cart.Fees {
		f.Total = gooRound(f.Amount, dp)
		f.Taxes = map[int64]float64{}
		if f.Taxable && s.gooTaxEnabled() {
			f.Taxes = rnd(gooCalcTax(f.Total, ratesFor(f.TaxClass, loc, false), false))
		} else if f.Total < 0 && s.gooTaxEnabled() && cart.CartContentsTotal > 0 {
			// A negative fee reduces the taxes proportionally (WC_Cart_Totals::calculate_fee_totals).
			for k, v := range cart.Taxes {
				f.Taxes[k] = gooRound(f.Total*(v/cart.CartContentsTotal), dp)
			}
		}
		f.Tax = gooSumTaxes(f.Taxes)
		cart.FeeTotal += f.Total
		cart.FeeTax += f.Tax
		for k, v := range f.Taxes {
			cart.Taxes[k] += v
		}
	}

	// 5. Shipping.
	cart.Packages = nil
	cart.ShippingTotal, cart.ShippingTax = 0, 0
	cart.ShippingTaxes = map[int64]float64{}
	if cart.NeedsShipping() {
		dest := cart.shippingDestination()
		pkg := &gooPackage{Destination: dest, Coupons: cart.coupons, cart: cart}
		for _, it := range cart.Items {
			if s.gooNeedsShipping(ctx, it.product) {
				pkg.Items = append(pkg.Items, it)
				pkg.Contents += it.LineTotal
				pkg.ContentsTax += it.LineTax
				pkg.Subtotal += it.LineSubtotal
				if s.gooOptString("tax_display_cart") == "incl" {
					pkg.Subtotal += it.LineSubtotalTax
				}
			}
		}
		pr := &gooPackageRates{Package: pkg}
		hideUntilAddress := s.gooOptBool("shipping_cost_requires_address") && !cart.sess.Data.Customer.CalculatedShip && cart.sess.Data.Customer.Billing["country"] == "" && cart.sess.Data.Customer.Shipping["country"] == ""
		if dest.Country != "" && !hideUntilAddress && s.gooShipsTo(dest.Country) {
			pr.Rates = s.gooPackageRates(ctx, pkg)
		}
		chosen := cart.sess.Data.ChosenShipping["0"]
		for _, r := range pr.Rates {
			if r.ID == chosen {
				pr.Chosen = r
			}
		}
		if pr.Chosen == nil && len(pr.Rates) > 0 {
			pr.Chosen = pr.Rates[0]
			if cart.sess.Data.ChosenShipping["0"] != pr.Chosen.ID {
				cart.sess.Data.ChosenShipping["0"] = pr.Chosen.ID
				cart.sess.touch()
			}
		}
		// Rate taxes: the shipping tax class (or the items' class).
		shipClass := s.gooOptString("shipping_tax_class")
		if shipClass == "inherit" {
			shipClass = cart.inheritedShippingTaxClass()
		}
		for _, r := range pr.Rates {
			r.Cost = gooRound(r.Cost, dp)
			if r.Taxable && s.gooTaxEnabled() {
				tl := loc
				if r.MethodID == "local_pickup" {
					tl = base
				}
				r.Taxes = rnd(gooCalcTax(r.Cost, ratesFor(shipClass, tl, true), false))
			}
		}
		if pr.Chosen != nil {
			cart.ShippingTotal = pr.Chosen.Cost
			for k, v := range pr.Chosen.Taxes {
				cart.ShippingTaxes[k] += v
			}
			cart.ShippingTax = gooSumTaxes(pr.Chosen.Taxes)
		}
		cart.Packages = append(cart.Packages, pr)
	}

	// 6. Totals.
	if roundAtSubtotal {
		for k, v := range cart.Taxes {
			cart.Taxes[k] = gooRound(v, dp)
		}
		for k, v := range cart.ShippingTaxes {
			cart.ShippingTaxes[k] = gooRound(v, dp)
		}
		cart.CartContentsTax = gooRound(cart.CartContentsTax, dp)
		cart.ShippingTax = gooSumTaxes(cart.ShippingTaxes)
	}
	cart.TotalTax = gooRound(gooSumTaxes(cart.Taxes)+gooSumTaxes(cart.ShippingTaxes), dp)
	// Payment surcharges: fees on the whole amount (items, fees, shipping and taxes), added once
	// everything else is known, so they feed into no other calculation. They are not taxable.
	cart.SurchargeBase = max(gooRound(cart.CartContentsTotal+cart.FeeTotal+cart.ShippingTotal+cart.TotalTax, dp), 0)
	if n := len(cart.Fees); len(gooCartSurchargeHooks) > 0 {
		for _, h := range gooCartSurchargeHooks {
			h(s, ctx, cart)
		}
		for _, f := range cart.Fees[n:] {
			f.Taxable, f.Total, f.Taxes, f.Tax = false, gooRound(f.Amount, dp), map[int64]float64{}, 0
			cart.FeeTotal += f.Total
		}
	}
	cart.Total = gooRound(cart.CartContentsTotal+cart.FeeTotal+cart.ShippingTotal+cart.TotalTax, dp)
	if cart.Total < 0 {
		cart.Total = 0
	}
	cart.Subtotal, cart.SubtotalTax = gooRound(cart.Subtotal, dp), gooRound(cart.SubtotalTax, dp)
	cart.CartContentsTotal, cart.CartContentsTax = gooRound(cart.CartContentsTotal, dp), gooRound(cart.CartContentsTax, dp)
}

// dropInvalidLines keeps a line total from ever being negative: a line whose quantity is outside
// 1…gooMaxLineQty (Add, SetQuantity and load never let one in, but a module's price hook may set
// it: bundled items follow their container) leaves the cart, and a negative unit price counts as 0.
func (cart *gooCart) dropInvalidLines() {
	keep := make([]*gooCartItem, 0, len(cart.Items))
	for _, it := range cart.Items {
		if it.Quantity < 1 || it.Quantity > gooMaxLineQty {
			name := "A product"
			if it.product != nil {
				name = it.product.Name
			}
			cart.errors = append(cart.errors, fmt.Sprintf("%s has been removed from your cart because its quantity is not valid.", html.EscapeString(name)))
			continue
		}
		if !(it.Price >= 0) { // negative or NaN
			it.Price = 0
		}
		keep = append(keep, it)
	}
	if len(keep) != len(cart.Items) {
		cart.Items = keep
		cart.persist()
	}
}

// inheritedShippingTaxClass is the shipping tax class from the cart items: standard when any item
// uses it, else the first class (in the tax classes order) found in the cart.
func (cart *gooCart) inheritedShippingTaxClass() string {
	found := map[string]bool{}
	for _, it := range cart.Items {
		if it.Taxable && cart.s.gooNeedsShipping(cart.c.Request.Context(), it.product) {
			found[it.TaxClass] = true
		}
	}
	if found[""] || len(found) == 0 {
		return ""
	}
	for _, c := range cart.s.gooTaxClasses() {
		if found[c.Value] {
			return c.Value
		}
	}
	return ""
}

// gooShipsTo reports whether the store ships to a country.
func (s *Server) gooShipsTo(country string) bool {
	for _, c := range s.gooShippingCountries() {
		if c.Code == country {
			return true
		}
	}
	return false
}

// ChooseShipping stores the chosen rate of a package (the shipping method radio buttons).
func (cart *gooCart) ChooseShipping(pkg int, rateID string) {
	cart.sess.Data.ChosenShipping[strconv.Itoa(pkg)] = rateID
	cart.sess.touch()
	cart.Calculate()
}

// gooPostedShippingIndex parses a posted shipping_method[N] key into its package index.
func gooPostedShippingIndex(k string) (int, bool) {
	if !strings.HasPrefix(k, "shipping_method[") || !strings.HasSuffix(k, "]") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(k, "shipping_method["), "]"))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ChooseShippings applies the posted shipping_method[N] radio buttons (cart form, update_shipping_method)
// in one pass: only indexes of the cart's packages and rates offered for that package are kept, and the
// cart is recalculated once, as WC_AJAX::update_shipping_method does. It reports whether a choice changed.
func (cart *gooCart) ChooseShippings(form map[string][]string) bool {
	changed := false
	for k, v := range form {
		idx, ok := gooPostedShippingIndex(k)
		if !ok || len(v) == 0 || idx >= len(cart.Packages) {
			continue
		}
		for _, r := range cart.Packages[idx].Rates {
			if r.ID == v[0] {
				if key := strconv.Itoa(idx); cart.sess.Data.ChosenShipping[key] != v[0] {
					cart.sess.Data.ChosenShipping[key] = v[0]
					changed = true
				}
				break
			}
		}
	}
	if changed {
		cart.sess.touch()
		cart.Calculate()
	}
	return changed
}

// Errors returns and clears the messages produced while loading and calculating.
func (cart *gooCart) Errors() []string {
	e := cart.errors
	cart.errors = nil
	return e
}

// ItemData is the label/value list under an item (variation attributes, then the modules' data).
func (s *Server) gooItemData(ctx context.Context, it *gooCartItem) []gooKV {
	var out []gooKV
	if it.product != nil && it.product.Type == gooTypeVariation {
		parent := s.gooParent(ctx, it.product)
		if parent != nil {
			for _, a := range parent.Attributes {
				if !a.Variation {
					continue
				}
				v := it.Variation["attribute_"+a.Key()]
				if v == "" {
					continue
				}
				out = append(out, gooKV{s.gooAttrLabel(ctx, a), s.gooAttrValueLabel(ctx, a, v)})
			}
		}
	}
	for _, h := range gooCartItemDataHooks {
		out = append(out, h(s, ctx, it)...)
	}
	return out
}

// ItemSubtotalDisplay is WC_Cart::get_product_subtotal(): the line subtotal in display terms.
func (cart *gooCart) ItemSubtotalDisplay(it *gooCartItem) float64 {
	if cart.s.gooOptString("tax_display_cart") == "incl" {
		return it.LineSubtotal + it.LineSubtotalTax
	}
	return it.LineSubtotal
}

// ItemPriceDisplay is WC_Cart::get_product_price(): the unit price in display terms.
func (cart *gooCart) ItemPriceDisplay(it *gooCartItem) float64 {
	if it.Quantity == 0 {
		return 0
	}
	return cart.ItemSubtotalDisplay(it) / float64(it.Quantity)
}

// SubtotalDisplay is get_cart_subtotal()'s amount.
func (cart *gooCart) SubtotalDisplay() float64 { return cart.displayedSubtotal() }

// ShippingDisplay is the chosen shipping cost in display terms.
func (cart *gooCart) RateDisplay(r *gooShippingRate) float64 {
	if cart.s.gooOptString("tax_display_cart") == "incl" {
		return r.Cost + gooSumTaxes(r.Taxes)
	}
	return r.Cost
}

// CouponDisplay is the discount of a coupon in display terms.
func (cart *gooCart) CouponDisplay(code string) float64 {
	if cart.s.gooOptString("tax_display_cart") == "incl" {
		return cart.CouponExTax[code] + cart.CouponTax[code]
	}
	return cart.CouponExTax[code]
}

// TaxTotals are the tax lines (WC_Cart::get_tax_totals()): one per rate code, with amounts.
type gooTaxTotal struct {
	RateID   int64
	Code     string
	Label    string
	Amount   float64
	Compound bool
	Rate     float64 // the rate's percentage
}

func (cart *gooCart) TaxTotals() []gooTaxTotal {
	ctx := cart.c.Request.Context()
	all := map[int64]float64{}
	for k, v := range cart.Taxes {
		all[k] += v
	}
	for k, v := range cart.ShippingTaxes {
		all[k] += v
	}
	var out []gooTaxTotal
	for id, amt := range all {
		r := cart.s.gooTaxRateByID(ctx, id)
		if r == nil {
			continue
		}
		out = append(out, gooTaxTotal{RateID: id, Code: gooTaxRateCode(r), Label: r.Name, Amount: gooRound(amt, cart.s.gooDecimals()), Compound: r.Compound, Rate: r.Rate})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RateID < out[j].RateID })
	return out
}
