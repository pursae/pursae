package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// The cart page (cart.php, cart-totals.php, cart-shipping.php, shipping-calculator.php), the
// mini-cart and header cart link, the add-to-cart / cart form handlers and the AJAX actions.

// Cart page -------------------------------------------------------------------------------------------------

func (s *Server) gooCartNonce(c *gin.Context) string { return s.nonce(c, "woocommerce-cart") }

// gooCartActionURL is the cart URL with an action (remove_item, undo_item, remove_coupon) and its nonce.
func (s *Server) gooCartActionURL(c *gin.Context, arg, value string) string {
	base := s.gooPageURL(c.Request.Context(), "cart")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + arg + "=" + url.QueryEscape(value) + "&_wpnonce=" + s.gooCartNonce(c)
}

// gooCartPageHTML is the [goocommerce_cart] shortcode.
func (s *Server) gooCartPageHTML(c *gin.Context) string {
	return s.gooCartPageView(c, gooDefaultCartView())
}

// gooCartPageView is the cart page with the sections v shows (the Cart block's settings).
func (s *Server) gooCartPageView(c *gin.Context, v gooCartView) string {
	ctx := c.Request.Context()
	cart := s.gooCartFor(c)
	var b strings.Builder
	b.WriteString(`<div class="woocommerce">`)
	b.WriteString(s.gooNoticesHTML(c))
	if cart.IsEmpty() {
		if v.EmptyHTML != "" { // the Empty cart block's own content
			b.WriteString(s.gooKeep(c, v.EmptyHTML) + `</div>`)
			return b.String()
		}
		msg := "Your cart is currently empty."
		if v.EmptyMessage != "" {
			msg = s.gooKeep(c, gooEsc(v.EmptyMessage))
		}
		b.WriteString(`<div class="wc-empty-cart-message"><div class="cart-empty woocommerce-info" role="status">` + msg + `</div></div>`)
		if s.gooPageID("shop") > 0 {
			b.WriteString(`<p class="return-to-shop"><a class="button wc-backward" href="` + gooEsc(s.gooShopURL(ctx)) + `">Return to shop</a></p>`)
		}
		b.WriteString(`</div>`)
		return b.String()
	}
	s.gooPrefetchCartImages(c, cart)
	b.WriteString(string(s.gooPageHTML(c, "cart_before")))
	cartURL := s.gooPageURL(ctx, "cart")
	b.WriteString(`<form class="woocommerce-cart-form" id="goo-cart-form" action="` + gooEsc(cartURL) + `" method="post">`)
	b.WriteString(`<table class="shop_table shop_table_responsive cart woocommerce-cart-form__contents" cellspacing="0"><thead><tr>` +
		`<th class="product-remove"><span class="screen-reader-text">Remove item</span></th><th class="product-thumbnail"><span class="screen-reader-text">Thumbnail image</span></th>` +
		`<th class="product-name">Product</th><th class="product-price">Price</th><th class="product-quantity">Quantity</th><th class="product-subtotal">Subtotal</th></tr></thead><tbody>`)
	for _, it := range cart.Items {
		if !s.gooCartItemVisible(ctx, it, "cart") {
			continue
		}
		p := it.product
		parentName := s.gooProductTitle(ctx, p)
		link := s.gooPermalink(ctx, p)
		b.WriteString(`<tr class="woocommerce-cart-form__cart-item cart_item">`)
		b.WriteString(`<td class="product-remove"><a role="button" href="` + gooEsc(s.gooCartActionURL(c, "remove_item", it.Key)) + `" class="remove" aria-label="Remove ` + gooEsc(parentName) +
			` from cart" data-product_id="` + itoa64(it.ProductID) + `" data-product_sku="` + gooEsc(p.SKU) + `" data-cart_item_key="` + it.Key + `">&times;</a></td>`)
		b.WriteString(`<td class="product-thumbnail"><a href="` + gooEsc(link) + `">` + s.gooLoopImageHTML(c, s.gooImageOf(ctx, p), "thumbnail", "attachment-woocommerce_thumbnail size-woocommerce_thumbnail") + `</a></td>`)
		b.WriteString(`<td class="product-name" data-title="Product"><a href="` + gooEsc(link) + `">` + gooEsc(parentName) + `</a>`)
		b.WriteString(s.gooItemDataHTML(ctx, it))
		if s.gooManagingStock(ctx, p) && s.gooBackordersAllowed(ctx, p) && s.gooStockQty(ctx, p) < it.Quantity && s.gooStockOwner(ctx, p).Backorders == "notify" {
			b.WriteString(`<p class="backorder_notification">Available on backorder</p>`)
		}
		b.WriteString(`</td>`)
		b.WriteString(`<td class="product-price" data-title="Price">` + string(s.gooPrice(cart.ItemPriceDisplay(it))) + `</td>`)
		_, hi := s.gooQtyLimits(ctx, p)
		b.WriteString(`<td class="product-quantity" data-title="Quantity">` + gooQuantityInput("cart["+it.Key+"][qty]", parentName, it.Quantity, 0, hi, "quantity_"+it.Key[:12], it.soldIndividually()) + `</td>`)
		b.WriteString(`<td class="product-subtotal" data-title="Subtotal">` + string(s.gooPrice(cart.ItemSubtotalDisplay(it))) + s.gooIncTaxNote(cart, it) + `</td>`)
		b.WriteString(`</tr>`)
	}
	b.WriteString(`<tr><td colspan="6" class="actions">`)
	if s.gooCouponsEnabled() && v.Coupon {
		b.WriteString(`<div class="coupon"><label for="coupon_code" class="screen-reader-text">Coupon:</label> <input type="text" name="coupon_code" class="input-text" id="coupon_code" value="" placeholder="Coupon code" /> ` +
			`<button type="submit" class="button" name="apply_coupon" value="Apply coupon">Apply coupon</button></div>`)
	}
	b.WriteString(`<button type="submit" class="button" name="update_cart" value="Update cart">Update cart</button>`)
	b.WriteString(`<input type="hidden" name="woocommerce-cart-nonce" value="` + s.gooCartNonce(c) + `" />`)
	b.WriteString(`</td></tr></tbody></table></form>`)
	b.WriteString(`<div class="cart-collaterals">`)
	if v.CrossSells {
		b.WriteString(s.gooCrossSellsHTML(c, cart))
	}
	b.WriteString(s.gooCartTotalsHTML(c, cart))
	b.WriteString(`</div>`)
	b.WriteString(string(s.gooPageHTML(c, "cart_after")))
	b.WriteString(`</div>`)
	s.gooSaveSession(c)
	return b.String()
}

// gooIncTaxNote is the "(incl. tax)" / "(ex. tax)" note of a line whose display differs from entry.
func (s *Server) gooIncTaxNote(cart *gooCart, it *gooCartItem) string {
	if !s.gooTaxEnabled() || !it.Taxable || it.LineSubtotalTax == 0 {
		return ""
	}
	incl := s.gooOptString("tax_display_cart") == "incl"
	if incl && !s.gooPricesIncludeTax() {
		return ` <small class="tax_label">(incl. tax)</small>`
	}
	if !incl && s.gooPricesIncludeTax() {
		return ` <small class="tax_label">(ex. tax)</small>`
	}
	return ""
}

// gooItemDataHTML is wc_get_formatted_cart_item_data().
func (s *Server) gooItemDataHTML(ctx context.Context, it *gooCartItem) string {
	data := s.gooItemData(ctx, it)
	if len(data) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<dl class="variation">`)
	for _, kv := range data {
		cls := gooClassRe.ReplaceAllString(kv.Key, "-")
		b.WriteString(`<dt class="variation-` + gooEsc(cls) + `">` + gooEsc(kv.Key) + `:</dt><dd class="variation-` + gooEsc(cls) + `"><p>` + gooEsc(kv.Value) + `</p></dd>`)
	}
	b.WriteString(`</dl>`)
	return b.String()
}

// gooCrossSellsHTML is woocommerce_cross_sell_display().
func (s *Server) gooCrossSellsHTML(c *gin.Context, cart *gooCart) string {
	var ids []int64
	in := map[int64]bool{}
	for _, it := range cart.Items {
		in[it.ProductID] = true
	}
	ctx := c.Request.Context()
	for _, it := range cart.Items {
		parent := it.product
		if pp := s.gooParent(ctx, it.product); pp != nil {
			parent = pp
		}
		for _, id := range parent.CrossSellIDs {
			if !in[id] && !gooInt64In(id, ids) {
				ids = append(ids, id)
			}
		}
	}
	list := s.gooVisibleProducts(c, ids)
	if len(list) == 0 {
		return ""
	}
	if len(list) > 2 {
		list = list[:2]
	}
	return `<div class="cross-sells"><h2>You may be interested in&hellip;</h2>` + s.gooLoopHTML(c, list, 2) + `</div>`
}

// gooCartTotalsHTML is cart-totals.php.
func (s *Server) gooCartTotalsHTML(c *gin.Context, cart *gooCart) string {
	ctx := c.Request.Context()
	var b strings.Builder
	cls := "cart_totals"
	if cart.Customer().CalculatedShip {
		cls += " calculated_shipping"
	}
	b.WriteString(`<div class="` + cls + `"><h2>Cart totals</h2><table cellspacing="0" class="shop_table shop_table_responsive">`)
	b.WriteString(s.gooTotalsRows(c, cart, true))
	b.WriteString(`</table><div class="wc-proceed-to-checkout"><a href="` + gooEsc(s.gooPageURL(ctx, "checkout")) + `" class="checkout-button button alt wc-forward">Proceed to checkout</a></div></div>`)
	return b.String()
}

// gooTotalsRows are the rows shared by the cart totals and the checkout review table.
func (s *Server) gooTotalsRows(c *gin.Context, cart *gooCart, onCart bool) string {
	var b strings.Builder
	b.WriteString(`<tr class="cart-subtotal"><th>Subtotal</th><td data-title="Subtotal">` + string(s.gooPrice(cart.SubtotalDisplay())) + s.gooSubtotalTaxNote(cart) + `</td></tr>`)
	for _, code := range cart.AppliedCoupons {
		label := "Coupon: " + gooEsc(code)
		b.WriteString(`<tr class="cart-discount coupon-` + gooEsc(gooSlugRe.ReplaceAllString(strings.ToLower(code), "-")) + `"><th>` + label + `</th><td data-title="` + label + `">-` +
			string(s.gooPrice(cart.CouponDisplay(code))) + ` <a href="` + gooEsc(s.gooCartActionURL(c, "remove_coupon", code)) + `" class="woocommerce-remove-coupon" data-coupon="` + gooEsc(code) + `">[Remove]</a></td></tr>`)
	}
	if cart.NeedsShipping() {
		b.WriteString(s.gooShippingRows(c, cart, onCart))
	}
	incl := s.gooOptString("tax_display_cart") == "incl"
	for _, f := range cart.Fees {
		amt := f.Total
		if incl {
			amt += f.Tax
		}
		b.WriteString(`<tr class="fee"><th>` + gooEsc(f.Name) + `</th><td data-title="` + gooEsc(f.Name) + `">` + string(s.gooPrice(amt)) + `</td></tr>`)
	}
	if s.gooTaxEnabled() && !incl {
		if s.gooOptString("tax_total_display") == "itemized" {
			for _, t := range cart.TaxTotals() {
				b.WriteString(`<tr class="tax-rate tax-rate-` + gooEsc(strings.ToLower(t.Code)) + `"><th>` + gooEsc(t.Label) + `</th><td data-title="` + gooEsc(t.Label) + `">` + string(s.gooPrice(t.Amount)) + `</td></tr>`)
			}
		} else {
			b.WriteString(`<tr class="tax-total"><th>` + gooEsc(s.gooTaxLabel()) + `</th><td data-title="` + gooEsc(s.gooTaxLabel()) + `">` + string(s.gooPrice(cart.TotalTax)) + `</td></tr>`)
		}
	}
	total := string(s.gooPrice(cart.Total))
	if s.gooTaxEnabled() && incl && cart.TotalTax > 0 {
		var parts []string
		if s.gooOptString("tax_total_display") == "itemized" {
			for _, t := range cart.TaxTotals() {
				parts = append(parts, string(s.gooPrice(t.Amount))+" "+gooEsc(t.Label))
			}
		} else {
			parts = append(parts, string(s.gooPrice(cart.TotalTax))+" "+gooEsc(s.gooTaxLabel()))
		}
		total += ` <small class="includes_tax">(includes ` + strings.Join(parts, ", ") + `)</small>`
	}
	b.WriteString(`<tr class="order-total"><th>Total</th><td data-title="Total"><strong>` + total + `</strong></td></tr>`)
	return b.String()
}

// gooTaxLabel is WC_Countries::tax_or_vat().
func (s *Server) gooTaxLabel() string {
	switch s.gooBaseCountry() {
	case "AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR", "DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL", "PL", "PT", "RO", "SK", "SI", "ES", "SE", "GB", "TR", "IM", "MC":
		return "VAT"
	}
	return "Tax"
}

func (s *Server) gooSubtotalTaxNote(cart *gooCart) string {
	if !s.gooTaxEnabled() || cart.SubtotalTax == 0 {
		return ""
	}
	incl := s.gooOptString("tax_display_cart") == "incl"
	if incl && !s.gooPricesIncludeTax() {
		return ` <small class="tax_label">(incl. tax)</small>`
	}
	if !incl && s.gooPricesIncludeTax() {
		return ` <small class="tax_label">(ex. tax)</small>`
	}
	return ""
}

// gooShippingRows is cart-shipping.php for each package.
func (s *Server) gooShippingRows(c *gin.Context, cart *gooCart, onCart bool) string {
	var b strings.Builder
	for i, pr := range cart.Packages {
		name := "Shipping"
		if len(cart.Packages) > 1 {
			name = fmt.Sprintf("Shipping %d", i+1)
		}
		b.WriteString(`<tr class="woocommerce-shipping-totals shipping"><th>` + name + `</th><td data-title="` + name + `">`)
		formAttr := ""
		if onCart {
			formAttr = ` form="goo-cart-form"`
		}
		dest := cart.shippingDestination()
		switch {
		case len(pr.Rates) > 1:
			b.WriteString(`<ul id="shipping_method" class="woocommerce-shipping-methods">`)
			for _, r := range pr.Rates {
				id := "shipping_method_" + strconv.Itoa(i) + "_" + gooSlugRe.ReplaceAllString(strings.ToLower(r.ID), "")
				chk := ""
				if pr.Chosen != nil && pr.Chosen.ID == r.ID {
					chk = ` checked="checked"`
				}
				b.WriteString(`<li><input type="radio" name="shipping_method[` + strconv.Itoa(i) + `]" data-index="` + strconv.Itoa(i) + `" id="` + id + `" value="` + gooEsc(r.ID) + `" class="shipping_method"` + chk + formAttr + ` />` +
					`<label for="` + id + `">` + s.gooRateLabel(cart, r) + `</label></li>`)
			}
			b.WriteString(`</ul>`)
		case len(pr.Rates) == 1:
			r := pr.Rates[0]
			b.WriteString(`<ul id="shipping_method" class="woocommerce-shipping-methods"><li><input type="hidden" name="shipping_method[` + strconv.Itoa(i) + `]" data-index="` + strconv.Itoa(i) + `" id="shipping_method_` +
				strconv.Itoa(i) + `" value="` + gooEsc(r.ID) + `" class="shipping_method"` + formAttr + ` /><label for="shipping_method_` + strconv.Itoa(i) + `">` + s.gooRateLabel(cart, r) + `</label></li></ul>`)
		case dest.Country == "" || !cart.Customer().CalculatedShip && s.gooOptBool("shipping_cost_requires_address"):
			b.WriteString(`<p class="woocommerce-shipping-destination">Shipping options will be updated during checkout.</p>`)
		default:
			b.WriteString(`<p class="woocommerce-shipping-destination">No shipping options were found for <strong>` + gooEsc(gooFormatDest(dest)) + `</strong>.</p>`)
		}
		if onCart && len(pr.Rates) > 0 && dest.Country != "" {
			b.WriteString(`<p class="woocommerce-shipping-destination">Shipping to <strong>` + gooEsc(gooFormatDest(dest)) + `</strong>.</p>`)
		}
		if onCart && s.gooOptBool("enable_shipping_calc") {
			b.WriteString(s.gooShippingCalculator(c, cart))
		}
		b.WriteString(`</td></tr>`)
	}
	return b.String()
}

func gooFormatDest(l gooLocation) string {
	var parts []string
	for _, v := range []string{l.City, gooStateName(l.Country, l.State), l.Postcode, gooCountryName(l.Country)} {
		if strings.TrimSpace(v) != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, ", ")
}

// gooRateLabel is wc_cart_totals_shipping_method_label().
func (s *Server) gooRateLabel(cart *gooCart, r *gooShippingRate) string {
	label := gooEsc(r.Label)
	if cart.c != nil {
		label = gooEsc(s.gooT(cart.c, r.Label)) // Golylang: Languages › Translations
	}
	if r.Cost > 0 {
		label += `: ` + string(s.gooPrice(cart.RateDisplay(r)))
		if s.gooTaxEnabled() && gooSumTaxes(r.Taxes) > 0 {
			if s.gooOptString("tax_display_cart") == "incl" && !s.gooPricesIncludeTax() {
				label += ` <small class="tax_label">(incl. tax)</small>`
			}
		}
	}
	return label
}

// gooShippingCalculator is shipping-calculator.php.
func (s *Server) gooShippingCalculator(c *gin.Context, cart *gooCart) string {
	ctx := c.Request.Context()
	cu := cart.Customer()
	addr := cu.Shipping
	if addr["country"] == "" {
		addr = cu.Billing
	}
	country := addr["country"]
	if country == "" {
		country = s.gooBaseCountry()
	}
	var b strings.Builder
	b.WriteString(`<form class="woocommerce-shipping-calculator" action="` + gooEsc(s.gooPageURL(ctx, "cart")) + `" method="post">`)
	b.WriteString(`<a href="#" class="shipping-calculator-button" aria-expanded="false" aria-controls="shipping-calculator-form" role="button">Change address</a>`)
	b.WriteString(`<section class="shipping-calculator-form" id="shipping-calculator-form" style="display:none;">`)
	b.WriteString(`<p class="form-row form-row-wide" id="calc_shipping_country_field"><label for="calc_shipping_country" class="screen-reader-text">Country / region:</label>` +
		`<select name="calc_shipping_country" id="calc_shipping_country" class="country_to_state country_select" rel="calc_shipping_state"><option value="default">Select a country / region&hellip;</option>`)
	for _, cc := range s.gooShippingCountries() {
		sel := ""
		if cc.Code == country {
			sel = ` selected='selected'`
		}
		b.WriteString(`<option value="` + cc.Code + `"` + sel + `>` + gooEsc(cc.Name) + `</option>`)
	}
	b.WriteString(`</select></p>`)
	b.WriteString(`<p class="form-row form-row-wide" id="calc_shipping_state_field"><label for="calc_shipping_state" class="screen-reader-text">State / County:</label>`)
	b.WriteString(gooStateInput("calc_shipping_state", "calc_shipping_state", country, addr["state"], "State / County", "", false))
	b.WriteString(`</p>`)
	b.WriteString(`<p class="form-row form-row-wide" id="calc_shipping_city_field"><label for="calc_shipping_city" class="screen-reader-text">City:</label><input type="text" class="input-text" value="` + gooEsc(addr["city"]) + `" placeholder="City" name="calc_shipping_city" id="calc_shipping_city" /></p>`)
	b.WriteString(`<p class="form-row form-row-wide" id="calc_shipping_postcode_field"><label for="calc_shipping_postcode" class="screen-reader-text">Postcode / ZIP:</label><input type="text" class="input-text" value="` + gooEsc(addr["postcode"]) + `" placeholder="Postcode / ZIP" name="calc_shipping_postcode" id="calc_shipping_postcode" /></p>`)
	b.WriteString(`<p><button type="submit" name="calc_shipping" value="1" class="button">Update</button></p>`)
	b.WriteString(`<input type="hidden" name="woocommerce-shipping-calculator-nonce" value="` + s.nonce(c, "woocommerce-shipping-calculator") + `" />`)
	b.WriteString(`</section></form>`)
	return b.String()
}

// gooStateInput is the state field: a select for countries with states, else a text input.
func gooStateInput(name, id, country, value, label, class string, required bool) string {
	req := ""
	if required {
		req = ` aria-required="true"`
	}
	if states, ok := gooStates[country]; ok && len(states) > 0 {
		var b strings.Builder
		b.WriteString(`<select name="` + name + `" id="` + id + `" class="state_select ` + class + `" autocomplete="address-level1" data-placeholder="Select an option&hellip;" data-input-classes="" data-label="` + gooEsc(label) + `"` + req + `><option value="">Select an option&hellip;</option>`)
		for _, st := range states {
			sel := ""
			if st.Code == value {
				sel = ` selected='selected'`
			}
			b.WriteString(`<option value="` + gooEsc(st.Code) + `"` + sel + `>` + gooEsc(st.Name) + `</option>`)
		}
		b.WriteString(`</select>`)
		return b.String()
	}
	return `<input type="text" class="input-text ` + class + `" value="` + gooEsc(value) + `" placeholder="" name="` + name + `" id="` + id + `" autocomplete="address-level1" data-input-classes=""` + req + ` />`
}

// gooPrefetchCartImages loads the images of the cart items with one query (see gooLoopImageHTML).
func (s *Server) gooPrefetchCartImages(c *gin.Context, cart *gooCart) {
	ctx := c.Request.Context()
	var ids []int64
	for _, it := range cart.Items {
		if id := s.gooImageOf(ctx, it.product); id > 0 {
			ids = append(ids, id)
		}
	}
	s.gooPrefetchImages(c, ids)
}

// Mini cart -----------------------------------------------------------------------------------------------------

// gooMiniCart is woocommerce_mini_cart(): div.widget_shopping_cart_content.
func (s *Server) gooMiniCart(c *gin.Context) template.HTML {
	ctx := c.Request.Context()
	cart := s.gooCartFor(c)
	var b strings.Builder
	b.WriteString(`<div class="widget_shopping_cart_content">`)
	if cart.IsEmpty() {
		b.WriteString(`<p class="woocommerce-mini-cart__empty-message">No products in the cart.</p></div>`)
		return template.HTML(s.gooL10n(c, b.String()))
	}
	s.gooPrefetchCartImages(c, cart)
	b.WriteString(`<ul class="woocommerce-mini-cart cart_list product_list_widget">`)
	for _, it := range cart.Items {
		if !s.gooCartItemVisible(ctx, it, "mini_cart") {
			continue
		}
		p := it.product
		name := s.gooProductTitle(ctx, p)
		b.WriteString(`<li class="woocommerce-mini-cart-item mini_cart_item">`)
		b.WriteString(`<a role="button" href="` + gooEsc(s.gooCartActionURL(c, "remove_item", it.Key)) + `" class="remove remove_from_cart_button" aria-label="Remove ` + gooEsc(name) + ` from cart" data-product_id="` +
			itoa64(it.ProductID) + `" data-cart_item_key="` + it.Key + `" data-product_sku="` + gooEsc(p.SKU) + `" data-success_message="&ldquo;` + gooEsc(name) + `&rdquo; has been removed from your cart">&times;</a>`)
		b.WriteString(`<a href="` + gooEsc(s.gooPermalink(ctx, p)) + `">` + s.gooLoopImageHTML(c, s.gooImageOf(ctx, p), "thumbnail", "attachment-woocommerce_thumbnail size-woocommerce_thumbnail") + gooEsc(name) + `</a>`)
		b.WriteString(s.gooItemDataHTML(ctx, it))
		b.WriteString(`<span class="quantity">` + strconv.Itoa(it.Quantity) + ` &times; ` + string(s.gooPrice(cart.ItemPriceDisplay(it))) + `</span>`)
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul>`)
	b.WriteString(`<p class="woocommerce-mini-cart__total total"><strong>Subtotal:</strong> ` + string(s.gooPrice(cart.SubtotalDisplay())) + `</p>`)
	b.WriteString(`<p class="woocommerce-mini-cart__buttons buttons"><a href="` + gooEsc(s.gooPageURL(ctx, "cart")) + `" class="button wc-forward">View cart</a>` +
		`<a href="` + gooEsc(s.gooPageURL(ctx, "checkout")) + `" class="button checkout wc-forward">Checkout</a></p>`)
	b.WriteString(`</div>`)
	return template.HTML(s.gooL10n(c, b.String()))
}

// gooCartLink is the header cart link themes print (a.cart-contents with the amount and the count).
func (s *Server) gooCartLink(c *gin.Context) template.HTML {
	cart := s.gooCartFor(c)
	n := cart.Count()
	items := strconv.Itoa(n) + " items"
	if n == 1 {
		items = "1 item"
	}
	return template.HTML(s.gooL10n(c, `<a class="cart-contents" href="`+gooEsc(s.gooPageURL(c.Request.Context(), "cart"))+`" title="View your shopping cart">`+
		string(s.gooPrice(cart.SubtotalDisplay()))+` <span class="count">`+items+`</span></a>`))
}

// gooFragments are the cart fragments (woocommerce_add_to_cart_fragments).
func (s *Server) gooFragments(c *gin.Context) map[string]string {
	cart := s.gooCartFor(c)
	return map[string]string{
		"div.widget_shopping_cart_content": string(s.gooMiniCart(c)),
		"a.cart-contents":                  string(s.gooCartLink(c)),
		"span.goo-cart-count":              `<span class="goo-cart-count">` + strconv.Itoa(cart.Count()) + `</span>`,
		"span.goo-cart-subtotal":           `<span class="goo-cart-subtotal">` + string(s.gooPrice(cart.SubtotalDisplay())) + `</span>`,
	}
}

// Add to cart -------------------------------------------------------------------------------------------------

// gooAddToCartFromForm is WC_Form_Handler::add_to_cart_action(): adds the product of a posted /
// query form. It returns the added products (title → quantity) or an error message.
func (s *Server) gooAddToCartFromForm(c *gin.Context, form url.Values) ([]gooAdded, error) {
	ctx := c.Request.Context()
	pid := gooAtoi64(form.Get("add-to-cart"))
	if pid == 0 {
		pid = gooAtoi64(form.Get("product_id"))
	}
	p, err := s.gooGetProduct(ctx, pid)
	if err != nil {
		return nil, gooCartErr("Sorry, this product cannot be purchased.")
	}
	cart := s.gooCartFor(c)
	qty := 1
	if v := form.Get("quantity"); v != "" {
		qty = gooAtoi(v)
	}
	switch p.Type {
	case gooTypeGrouped:
		var added []gooAdded
		var errs []string
		any := false
		for _, kid := range p.Children {
			q := gooAtoi(form.Get("quantity[" + itoa64(kid) + "]"))
			if q <= 0 {
				continue
			}
			any = true
			if _, err := cart.Add(ctx, kid, q, 0, nil, form, nil); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			k, _ := s.gooGetProduct(ctx, kid)
			if k != nil {
				added = append(added, gooAdded{k.Name, q})
			}
		}
		if !any {
			return nil, gooCartErr("Please choose the quantity of items you wish to add to your cart&hellip;")
		}
		if len(errs) > 0 {
			for _, e := range errs[1:] {
				s.gooAddNotice(c, "error", e)
			}
			if len(added) == 0 {
				return nil, errors.New(errs[0])
			}
			s.gooAddNotice(c, "error", errs[0])
		}
		return added, nil
	case gooTypeVariable, gooSubsTypeVariable, gooTypeVariation:
		vid := gooAtoi64(form.Get("variation_id"))
		attrs := map[string]string{}
		for k, v := range form {
			if strings.HasPrefix(k, "attribute_") && len(v) > 0 {
				attrs[k] = v[0]
			}
		}
		if _, err := cart.Add(ctx, pid, qty, vid, attrs, form, nil); err != nil {
			return nil, err
		}
	default:
		if _, err := cart.Add(ctx, pid, qty, 0, nil, form, nil); err != nil {
			return nil, err
		}
	}
	name := p.Name
	if p.Type == gooTypeVariation {
		name = s.gooProductTitle(ctx, p)
	}
	return []gooAdded{{name, qty}}, nil
}

type gooAdded struct {
	Name string
	Qty  int
}

// gooAddedMessage is wc_add_to_cart_message().
func (s *Server) gooAddedMessage(c *gin.Context, added []gooAdded, showButton bool) string {
	var titles []string
	count := 0
	for _, a := range added {
		t := "&ldquo;" + gooEsc(a.Name) + "&rdquo;"
		if a.Qty > 1 {
			t = strconv.Itoa(a.Qty) + " &times; " + t
		}
		titles = append(titles, t)
		count += a.Qty
	}
	list := strings.Join(titles, " and ")
	if len(titles) > 2 {
		list = strings.Join(titles[:len(titles)-1], ", ") + " and " + titles[len(titles)-1]
	}
	verb := "has"
	if count > 1 {
		verb = "have"
	}
	msg := list + " " + verb + " been added to your cart."
	if showButton {
		msg += ` <a href="` + gooEsc(s.gooPageURL(c.Request.Context(), "cart")) + `" tabindex="1" class="button wc-forward">View cart</a>`
	} else {
		msg += ` <a href="` + gooEsc(s.gooShopURL(c.Request.Context())) + `" tabindex="1" class="button wc-forward">Continue shopping</a>`
	}
	return msg
}

// gooAddToCartGET handles ?add-to-cart=ID links on any URL.
func (s *Server) gooAddToCartGET(c *gin.Context) {
	ctx := c.Request.Context()
	form := c.Request.URL.Query()
	back := s.gooCurrentURL(c, "add-to-cart", "quantity", "variation_id", "_wpnonce")
	added, err := s.gooAddToCartFromForm(c, form)
	s.gooAfterAdd(c, added, err, back, ctx)
}

func (s *Server) gooAfterAdd(c *gin.Context, added []gooAdded, err error, back string, ctx context.Context) {
	if err != nil {
		s.gooAddNotice(c, "error", err.Error())
		s.gooRedirect(c, back)
		return
	}
	redirect := s.gooOptBool("cart_redirect_after_add")
	s.gooAddNotice(c, "success", s.gooAddedMessage(c, added, !redirect))
	if redirect {
		s.gooRedirect(c, s.gooPageURL(ctx, "cart"))
		return
	}
	s.gooRedirect(c, back)
}

// gooHandleGetAction handles the cart links (remove_item, undo_item, remove_coupon, order_again,
// cancel_order); true when it answered.
func (s *Server) gooHandleGetAction(c *gin.Context) bool {
	ctx := c.Request.Context()
	q := c.Request.URL.Query()
	cartURL := s.gooPageURL(ctx, "cart")
	switch {
	case q.Get("remove_item") != "":
		if !s.checkNonce(c, "woocommerce-cart") {
			return false
		}
		cart := s.gooCartFor(c)
		if it := cart.Item(q.Get("remove_item")); it != nil {
			name := s.gooProductTitle(ctx, it.product)
			cart.Remove(it.Key)
			msg := "&ldquo;" + gooEsc(name) + "&rdquo; removed."
			if s.gooIsInStock(ctx, it.product) {
				msg += ` <a href="` + gooEsc(s.gooCartActionURL(c, "undo_item", it.Key)) + `" class="restore-item">Undo?</a>`
			}
			s.gooAddNotice(c, "success", msg)
		}
		target := cartURL
		if ref := c.Request.Referer(); ref != "" && !strings.Contains(ref, "remove_item") {
			target = s.gooSafeURL(ctx, ref, cartURL)
		}
		if cart.IsEmpty() {
			target = cartURL
		}
		s.gooRedirect(c, target)
		return true
	case q.Get("undo_item") != "":
		if !s.checkNonce(c, "woocommerce-cart") {
			return false
		}
		cart := s.gooCartFor(c)
		if cart.Restore(ctx, q.Get("undo_item")) {
			if it := cart.Item(q.Get("undo_item")); it != nil {
				s.gooAddNotice(c, "success", "&ldquo;"+gooEsc(s.gooProductTitle(ctx, it.product))+"&rdquo; has been restored.")
			}
		}
		s.gooRedirect(c, cartURL)
		return true
	case q.Get("remove_coupon") != "":
		if !s.checkNonce(c, "woocommerce-cart") {
			return false
		}
		if s.gooCartFor(c).RemoveCoupon(q.Get("remove_coupon")) {
			s.gooAddNotice(c, "success", "Coupon has been removed.")
		}
		target := cartURL
		if ref := c.Request.Referer(); ref != "" && strings.Contains(ref, s.gooPageURL(ctx, "checkout")) {
			target = s.gooPageURL(ctx, "checkout")
		}
		s.gooRedirect(c, target)
		return true
	case q.Get("order_again") != "":
		if !s.checkNonce(c, "woocommerce-order_again") {
			return false
		}
		s.gooOrderAgain(c, gooAtoi64(q.Get("order_again")))
		return true
	case q.Get("cancel_order") != "" && q.Get("order") != "":
		s.gooCancelOrder(c)
		return true
	}
	return false
}

// Cart forms -------------------------------------------------------------------------------------------------------

// gooHandlePost dispatches the store's POST forms; true when it answered.
func (s *Server) gooHandlePost(c *gin.Context, path string) bool {
	f := c.Request.PostForm
	if f == nil {
		_ = c.Request.ParseForm()
		f = c.Request.PostForm
	}
	has := func(k string) bool { _, ok := f[k]; return ok }
	switch {
	case has("goo_module"):
		return false // a module form no module route took: the page renders normally
	case has("goo_review"):
		s.gooSubmitReview(c)
	case has("woocommerce-cart-nonce"):
		s.gooUpdateCartAction(c)
	case has("woocommerce-shipping-calculator-nonce"):
		s.gooShippingCalcAction(c)
	case has("woocommerce-process-checkout-nonce"):
		s.gooCheckoutPost(c)
	case has("woocommerce-apply-coupon-nonce"):
		s.gooApplyCouponPost(c)
	case has("woocommerce-login-nonce"):
		s.gooLoginPost(c)
	case has("woocommerce-register-nonce"):
		s.gooRegisterPost(c)
	case has("woocommerce-lost-password-nonce"):
		s.gooLostPasswordPost(c)
	case has("woocommerce-reset-password-nonce"):
		s.gooResetPasswordPost(c)
	case has("woocommerce-edit-address-nonce"):
		s.gooSaveAddressPost(c)
	case has("save-account-details-nonce"):
		s.gooSaveAccountPost(c)
	case has("woocommerce-pay-nonce"):
		s.gooPayPost(c)
	case has("woocommerce-order-tracking-nonce"):
		return false // the [goocommerce_order_tracking] shortcode reads the posted form
	case has("add-to-cart"):
		back := s.gooCurrentURL(c)
		if ref := c.Request.Referer(); ref != "" {
			back = s.gooSafeURL(c.Request.Context(), ref, back)
		}
		added, err := s.gooAddToCartFromForm(c, f)
		s.gooAfterAdd(c, added, err, back, c.Request.Context())
	default:
		return false
	}
	return true
}

// gooUpdateCartAction is WC_Form_Handler::update_cart_action().
func (s *Server) gooUpdateCartAction(c *gin.Context) {
	ctx := c.Request.Context()
	cartURL := s.gooPageURL(ctx, "cart")
	if !s.checkNonceValue(c, c.PostForm("woocommerce-cart-nonce"), "woocommerce-cart") {
		s.gooAddNotice(c, "error", "Your session has expired. Please reload the page and try again.")
		s.gooRedirect(c, cartURL)
		return
	}
	cart := s.gooCartFor(c)
	f := c.Request.PostForm
	if c.PostForm("apply_coupon") != "" || (c.PostForm("coupon_code") != "" && c.PostForm("update_cart") == "") {
		if err := s.gooApplyCouponLimited(c, cart, c.PostForm("coupon_code")); err != nil {
			s.gooAddNotice(c, "error", err.Error())
		} else {
			s.gooAddNotice(c, "success", "Coupon code applied successfully.")
		}
		s.gooRedirect(c, cartURL)
		return
	}
	changed := false
	cart.ChooseShippings(f)
	for _, it := range append([]*gooCartItem{}, cart.Items...) {
		raw, ok := f["cart["+it.Key+"][qty]"]
		if !ok || len(raw) == 0 {
			continue
		}
		qty := gooAtoi(raw[0])
		if qty == it.Quantity {
			continue
		}
		if err := cart.SetQuantity(ctx, it.Key, qty); err != nil {
			s.gooAddNotice(c, "error", err.Error())
			continue
		}
		changed = true
	}
	if c.PostForm("proceed") != "" {
		s.gooRedirect(c, s.gooPageURL(ctx, "checkout"))
		return
	}
	if changed || c.PostForm("update_cart") != "" {
		s.gooAddNotice(c, "success", "Cart updated.")
	}
	s.gooRedirect(c, cartURL)
}

// gooShippingCalcAction is WC_Shortcode_Cart::calculate_shipping().
func (s *Server) gooShippingCalcAction(c *gin.Context) {
	ctx := c.Request.Context()
	cartURL := s.gooPageURL(ctx, "cart")
	if !s.checkNonceValue(c, c.PostForm("woocommerce-shipping-calculator-nonce"), "woocommerce-shipping-calculator") {
		s.gooRedirect(c, cartURL)
		return
	}
	country := strings.TrimSpace(c.PostForm("calc_shipping_country"))
	state := strings.TrimSpace(c.PostForm("calc_shipping_state"))
	postcode := strings.ToUpper(strings.TrimSpace(c.PostForm("calc_shipping_postcode")))
	city := strings.TrimSpace(c.PostForm("calc_shipping_city"))
	if country == "" || country == "default" {
		s.gooAddNotice(c, "error", "Please enter a valid country / region.")
		s.gooRedirect(c, cartURL)
		return
	}
	sess := s.gooSessionFor(c)
	cu := &sess.Data.Customer
	for _, m := range []map[string]string{cu.Billing, cu.Shipping} {
		m["country"], m["state"], m["postcode"], m["city"] = country, state, postcode, city
	}
	cu.CalculatedShip = true
	sess.touch()
	s.gooCartForRecalc(c)
	s.gooAddNotice(c, "success", "Shipping costs updated.")
	s.gooRedirect(c, cartURL)
}

// gooApplyCouponPost is the checkout's coupon form without JavaScript.
func (s *Server) gooApplyCouponPost(c *gin.Context) {
	ctx := c.Request.Context()
	target := s.gooPageURL(ctx, "checkout")
	if s.checkNonceValue(c, c.PostForm("woocommerce-apply-coupon-nonce"), "woocommerce-cart") {
		if err := s.gooApplyCouponLimited(c, s.gooCartFor(c), c.PostForm("coupon_code")); err != nil {
			s.gooAddNotice(c, "error", err.Error())
		} else {
			s.gooAddNotice(c, "success", "Coupon code applied successfully.")
		}
	}
	s.gooRedirect(c, target)
}

// gooOrderAgain refills the cart with a completed order's items.
func (s *Server) gooOrderAgain(c *gin.Context, id int64) {
	ctx := c.Request.Context()
	u := currentUser(c)
	o, err := s.gooGetOrder(ctx, id)
	if err != nil || u == nil || o.CustomerID != u.ID || o.Status != "completed" {
		s.gooRedirect(c, s.gooPageURL(ctx, "myaccount"))
		return
	}
	cart := s.gooCartFor(c)
	cart.Empty()
	for _, it := range o.ItemsOf("line_item") {
		attrs := map[string]string{}
		for k, v := range it.Meta {
			if str, ok := v.(string); ok && !strings.HasPrefix(k, "_") {
				attrs["attribute_"+k] = str
			}
		}
		if _, err := cart.Add(ctx, it.ProductID, it.Qty(), it.VariationID, attrs, nil, nil); err != nil {
			s.gooAddNotice(c, "error", err.Error())
		}
	}
	s.gooAddNotice(c, "notice", "The cart has been filled with the items from your previous order.")
	s.gooRedirect(c, s.gooPageURL(ctx, "cart"))
}

// gooRememberGuestOrder keeps a guest order on the session that placed it, so that session can
// still cancel after OrderAwaitingPayment is cleared. The list is capped and stored as float64
// so a JSON round-trip matches what gooInt reads back.
func gooRememberGuestOrder(sess *gooSession, id int64) {
	if sess == nil || id == 0 {
		return
	}
	if sess.Data.Extra == nil {
		sess.Data.Extra = map[string]any{}
	}
	var ids []any
	if cur, ok := sess.Data.Extra["guest_orders"].([]any); ok {
		ids = append([]any{}, cur...)
	}
	for _, v := range ids {
		if gooInt(v) == id {
			sess.touch()
			return
		}
	}
	ids = append(ids, float64(id))
	if len(ids) > 20 {
		ids = ids[len(ids)-20:]
	}
	sess.Data.Extra["guest_orders"] = ids
	sess.touch()
}

// gooSessionOwnsOrder reports whether this session placed the guest order.
func gooSessionOwnsOrder(sess *gooSession, id int64) bool {
	if sess == nil || id == 0 {
		return false
	}
	if sess.Data.OrderAwaitingPayment == id {
		return true
	}
	raw, _ := sess.Data.Extra["guest_orders"].([]any)
	for _, v := range raw {
		if gooInt(v) == id {
			return true
		}
	}
	return false
}

// gooGuestMayCancel allows a guest cancel from the placing session or a matching billing e-mail.
func gooGuestMayCancel(sess *gooSession, o *gooOrder, emails ...string) bool {
	if o == nil {
		return false
	}
	if gooSessionOwnsOrder(sess, o.ID) {
		return true
	}
	bill := ""
	if o.Billing != nil {
		bill = strings.TrimSpace(o.Billing["email"])
	}
	if bill == "" {
		return false
	}
	for _, e := range emails {
		if e = strings.TrimSpace(e); e != "" && strings.EqualFold(e, bill) {
			return true
		}
	}
	return false
}

// gooCancelOrder is WC_Form_Handler::cancel_order().
func (s *Server) gooCancelOrder(c *gin.Context) {
	ctx := c.Request.Context()
	q := c.Request.URL.Query()
	target := s.gooSafeURL(ctx, q.Get("redirect"), s.gooPageURL(ctx, "myaccount"))
	o, err := s.gooOrderByKey(ctx, q.Get("order"))
	u := currentUser(c)
	if err != nil || o.ID != gooAtoi64(q.Get("order_id")) || !s.checkNonce(c, "woocommerce-cancel_order") || (o.CustomerID != 0 && (u == nil || u.ID != o.CustomerID)) {
		s.gooAddNotice(c, "error", "Invalid order.")
		s.gooRedirect(c, target)
		return
	}
	// A guest order key is not enough: the session that placed the order, or the billing e-mail,
	// has to match. Anonymous nonces are not unique to one visitor.
	if o.CustomerID == 0 && !gooGuestMayCancel(s.gooSessionFor(c), o, q.Get("email"), q.Get("billing_email")) {
		s.gooAddNotice(c, "error", "Invalid order.")
		s.gooRedirect(c, target)
		return
	}
	if err := s.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "cancelled", "Order cancelled by customer.", ""); err == nil {
		s.gooAddNotice(c, "notice", "Your order was cancelled.")
	} else {
		s.gooAddNotice(c, "error", "Your order can no longer be cancelled. Please contact us if you need assistance.")
	}
	s.gooRedirect(c, target)
}

// AJAX ------------------------------------------------------------------------------------------------------------

// gooAjax serves /?goo-ajax=<action> (and /?wc-ajax=<action>).
func (s *Server) gooAjax(c *gin.Context, action string) {
	ctx := c.Request.Context()
	gooNoStore(c)
	c.Header("X-Robots-Tag", "noindex")
	_ = c.Request.ParseForm()
	switch action {
	case "get_refreshed_fragments":
		s.gooJSONFragments(c, nil)
	case "add_to_cart":
		form := c.Request.PostForm
		if len(form) == 0 {
			form = c.Request.URL.Query()
		}
		if form.Get("add-to-cart") == "" {
			form.Set("add-to-cart", form.Get("product_id"))
		}
		pid := gooAtoi64(form.Get("product_id"))
		added, err := s.gooAddToCartFromForm(c, form)
		if err != nil {
			s.gooAddNotice(c, "error", err.Error())
			link := s.links(ctx).HomeURL("/")
			if p, e := s.gooGetProduct(ctx, pid); e == nil {
				link = s.gooPermalink(ctx, p)
			}
			s.gooSaveSession(c)
			c.JSON(http.StatusOK, gin.H{"error": true, "product_url": link})
			return
		}
		if s.gooOptBool("cart_redirect_after_add") {
			s.gooAddNotice(c, "success", s.gooAddedMessage(c, added, false))
		}
		s.gooJSONFragments(c, nil)
	case "remove_from_cart":
		cart := s.gooCartFor(c)
		if it := cart.Item(c.PostForm("cart_item_key")); it != nil {
			cart.Remove(it.Key)
			s.gooJSONFragments(c, nil)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": false})
	case "apply_coupon":
		if !s.checkNonceValue(c, c.PostForm("security"), "apply-coupon") {
			s.gooAjaxHTML(c, gooErrorList([]string{"Your session has expired. Please reload the page and try again."}))
			return
		}
		code := c.PostForm("coupon_code")
		if err := s.gooApplyCouponLimited(c, s.gooCartFor(c), code); err != nil {
			s.gooAjaxHTML(c, gooErrorList([]string{err.Error()}))
			return
		}
		s.gooAjaxHTML(c, gooNoticesMarkup(nil, []string{"Coupon code applied successfully."}, nil))
	case "remove_coupon":
		if !s.checkNonceValue(c, c.PostForm("security"), "remove-coupon") {
			s.gooAjaxHTML(c, gooErrorList([]string{"Your session has expired. Please reload the page and try again."}))
			return
		}
		if s.gooCartFor(c).RemoveCoupon(c.PostForm("coupon")) {
			s.gooAjaxHTML(c, gooNoticesMarkup(nil, []string{"Coupon has been removed."}, nil))
			return
		}
		s.gooAjaxHTML(c, gooErrorList([]string{"Sorry there was a problem removing this coupon."}))
	case "update_shipping_method":
		cart := s.gooCartFor(c)
		cart.ChooseShippings(c.Request.PostForm)
		s.gooAjaxHTML(c, s.gooCartTotalsHTML(c, cart))
	case "update_order_review":
		s.gooUpdateOrderReview(c)
	case "checkout":
		s.gooCheckoutAjax(c)
	case "get_variation":
		p, err := s.gooGetProduct(ctx, gooAtoi64(c.PostForm("product_id")))
		if err != nil || !gooSubsVariableLike(p.Type) {
			c.JSON(http.StatusOK, false)
			return
		}
		attrs := map[string]string{}
		for k, v := range c.Request.PostForm {
			if strings.HasPrefix(k, "attribute_") && len(v) > 0 {
				attrs[k] = v[0]
			}
		}
		v, err := s.gooMatchVariation(ctx, p, attrs)
		if err != nil {
			c.JSON(http.StatusOK, false)
			return
		}
		d := s.gooVariationData(c, p, v)
		if d == nil {
			c.JSON(http.StatusOK, false)
			return
		}
		c.JSON(http.StatusOK, d)
	case "get_cart_totals":
		s.gooAjaxHTML(c, s.gooCartTotalsHTML(c, s.gooCartFor(c)))
	default:
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "data": "unknown action"})
	}
}

func (s *Server) gooAjaxHTML(c *gin.Context, body string) {
	s.gooSaveSession(c)
	c.Data(http.StatusOK, "text/html; charset=UTF-8", []byte(s.gooL10n(c, body)))
}

// gooJSONFragments answers with the refreshed fragments and the cart hash.
func (s *Server) gooJSONFragments(c *gin.Context, extra gin.H) {
	frags := s.gooFragments(c)
	sess := s.gooSessionFor(c)
	hash := ""
	if len(sess.Data.Cart) > 0 {
		hash = gooCartHash(sess.Data.Cart)
	}
	s.gooSaveSession(c)
	out := gin.H{"fragments": frags, "cart_hash": hash}
	for k, v := range extra {
		out[k] = v
	}
	c.JSON(http.StatusOK, out)
}
