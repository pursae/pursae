package server

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/wp"
)

// The checkout (form-checkout.php, form-billing.php, form-shipping.php, review-order.php, payment.php),
// its AJAX actions (update_order_review, checkout), the order received page (thankyou.php), the order pay
// page (form-pay.php) and the order details shared with My Account (order-details.php).

// Form fields -------------------------------------------------------------------------------------------------

// gooFormField is woocommerce_form_field().
func (s *Server) gooFormField(f gooCheckoutField, value, countryFor string) string {
	classes := append([]string{"form-row"}, f.Class...)
	required := f.Required
	if strings.HasSuffix(f.Key, "_state") {
		required = gooStateRequired(countryFor)
	}
	if strings.HasSuffix(f.Key, "_postcode") && gooPostcodeOptional[countryFor] {
		required = false
	}
	if required {
		classes = append(classes, "validate-required")
	}
	switch {
	case f.Type == "email":
		classes = append(classes, "validate-email")
	case f.Type == "tel":
		classes = append(classes, "validate-phone")
	case strings.HasSuffix(f.Key, "_postcode"):
		classes = append(classes, "validate-postcode")
	case f.Type == "state":
		classes = append(classes, "validate-state")
	}
	label := gooEsc(f.Label)
	if required {
		label += `&nbsp;<abbr class="required" title="required">*</abbr>`
	} else {
		label += `&nbsp;<span class="optional">(optional)</span>`
	}
	reqAttr := ""
	if required {
		reqAttr = ` aria-required="true"`
	}
	var input string
	switch f.Type {
	case "country":
		countries := s.gooAllowedCountries()
		if strings.HasPrefix(f.Key, "shipping_") {
			countries = s.gooShippingCountries()
		}
		if len(countries) == 1 {
			input = `<strong>` + gooEsc(countries[0].Name) + `</strong><input type="hidden" name="` + f.Key + `" id="` + f.Key + `" value="` + countries[0].Code + `" autocomplete="country" class="country_to_state" readonly="readonly" />`
		} else {
			var b strings.Builder
			b.WriteString(`<select name="` + f.Key + `" id="` + f.Key + `" class="country_to_state country_select" autocomplete="country" data-placeholder="Select a country / region&hellip;" data-label="` + gooEsc(f.Label) + `"` + reqAttr + `><option value="">Select a country / region&hellip;</option>`)
			for _, cc := range countries {
				sel := ""
				if cc.Code == value {
					sel = ` selected='selected'`
				}
				b.WriteString(`<option value="` + cc.Code + `"` + sel + `>` + gooEsc(cc.Name) + `</option>`)
			}
			b.WriteString(`</select><noscript><button type="submit" name="woocommerce_checkout_update_totals" value="Update country / region">Update country / region</button></noscript>`)
			input = b.String()
		}
	case "state":
		input = gooStateInput(f.Key, f.Key, countryFor, value, f.Label, "", required)
	case "textarea":
		input = `<textarea name="` + f.Key + `" class="input-text" id="` + f.Key + `" placeholder="` + gooEsc(f.Placeholder) + `" rows="2" cols="5"` + reqAttr + `>` + gooEsc(value) + `</textarea>`
	default:
		typ := f.Type
		if typ == "" {
			typ = "text"
		}
		input = `<input type="` + typ + `" class="input-text" name="` + f.Key + `" id="` + f.Key + `" placeholder="` + gooEsc(f.Placeholder) + `" value="` + gooEsc(value) + `" autocomplete="` + gooEsc(f.Autocomplete) + `"` + reqAttr + ` />`
	}
	return `<p class="` + strings.Join(classes, " ") + `" id="` + f.Key + `_field" data-priority="` + strconv.Itoa(f.Priority) + `"><label for="` + f.Key + `" class="">` + label +
		`</label><span class="woocommerce-input-wrapper">` + input + `</span></p>`
}

// gooCheckoutValue is WC_Checkout::get_value(): the posted value, the session's, the account's.
func (s *Server) gooCheckoutValue(c *gin.Context, key string) string {
	if v, ok := c.Request.PostForm[key]; ok && len(v) > 0 {
		return v[0]
	}
	cu := s.gooSessionFor(c).Data.Customer
	typ, field, _ := strings.Cut(key, "_")
	var m map[string]string
	switch typ {
	case "billing":
		m = cu.Billing
	case "shipping":
		m = cu.Shipping
	}
	if v := m[field]; v != "" {
		return v
	}
	if u := currentUser(c); u != nil {
		if v, ok := u.Meta[key].(string); ok && v != "" {
			return v
		}
		switch key {
		case "billing_email":
			return u.Email
		case "billing_first_name":
			return u.Profile["first_name"]
		case "billing_last_name":
			return u.Profile["last_name"]
		}
	}
	if field == "country" {
		if def := s.gooOptString("default_customer_address"); def == "base" || def == "geolocation" || def == "geolocation_ajax" {
			return s.gooDefaultCountry(c)
		}
		if list := s.gooAllowedCountries(); len(list) == 1 {
			return list[0].Code
		}
	}
	if field == "state" && m["country"] == "" {
		if def := s.gooOptString("default_customer_address"); def == "base" {
			return s.gooBaseState()
		}
	}
	return ""
}

// gooAddressFieldsHTML prints the billing or shipping fields.
func (s *Server) gooAddressFieldsHTML(c *gin.Context, typ string, value func(string) string) string {
	var b strings.Builder
	country := value(typ + "_country")
	for _, f := range s.gooAddressFields(typ) {
		b.WriteString(s.gooFormField(f, value(f.Key), country))
	}
	return b.String()
}

// Checkout page ------------------------------------------------------------------------------------------------

// gooCheckoutPageHTML is the [goocommerce_checkout] shortcode.
func (s *Server) gooCheckoutPageHTML(c *gin.Context) string {
	fv := gooViewFrom(c)
	if fv != nil && fv.Kind == "checkout" {
		switch fv.Endpoint {
		case "order-received":
			return `<div class="woocommerce">` + s.gooNoticesHTML(c) + s.gooThankYouHTML(c, gooAtoi64(fv.Value)) + `</div>`
		case "order-pay":
			return `<div class="woocommerce">` + s.gooNoticesHTML(c) + s.gooOrderPayHTML(c, gooAtoi64(fv.Value)) + `</div>`
		}
	}
	ctx := c.Request.Context()
	cart := s.gooCartFor(c)
	u := currentUser(c)
	var b strings.Builder
	b.WriteString(`<div class="woocommerce">`)
	b.WriteString(s.gooNoticesHTML(c))
	if cart.IsEmpty() {
		b.WriteString(`<div class="woocommerce-info" role="status">Checkout is not available whilst your cart is empty.</div>`)
		b.WriteString(`<p class="return-to-shop"><a class="button wc-backward" href="` + gooEsc(s.gooShopURL(ctx)) + `">Return to shop</a></p></div>`)
		return b.String()
	}
	checkoutURL := s.gooPageURL(ctx, "checkout")
	if u == nil && s.gooOptBool("enable_checkout_login_reminder") {
		b.WriteString(`<div class="woocommerce-form-login-toggle"><div class="woocommerce-info" role="status">Returning customer? <a href="#" class="showlogin">Click here to login</a></div></div>`)
		b.WriteString(s.gooLoginFormHTML(c, checkoutURL, true))
	}
	if s.gooCouponsEnabled() {
		b.WriteString(`<div class="woocommerce-form-coupon-toggle"><div class="woocommerce-info" role="status">Have a coupon? <a href="#" class="showcoupon">Click here to enter your code</a></div></div>`)
		b.WriteString(`<form class="checkout_coupon woocommerce-form-coupon" method="post" action="` + gooEsc(checkoutURL) + `" style="display:none" id="woocommerce-checkout-form-coupon"><p class="form-row-coupon-text">If you have a coupon code, please apply it below.</p>` +
			`<p class="form-row form-row-first"><label for="coupon_code" class="screen-reader-text">Coupon:</label><input type="text" name="coupon_code" class="input-text" placeholder="Coupon code" id="coupon_code" value="" /></p>` +
			`<p class="form-row form-row-last"><button type="submit" class="button" name="apply_coupon" value="Apply coupon">Apply coupon</button></p><div class="clear"></div>` +
			`<input type="hidden" name="woocommerce-apply-coupon-nonce" value="` + s.gooCartNonce(c) + `" /></form>`)
	}
	if u == nil && !s.gooOptBool("enable_guest_checkout") && !s.gooOptBool("enable_signup_and_login_from_checkout") {
		b.WriteString(`<div class="woocommerce-info" role="status">You must be logged in to checkout.</div></div>`)
		return b.String()
	}
	value := func(k string) string { return s.gooCheckoutValue(c, k) }
	b.WriteString(`<form name="checkout" method="post" class="checkout woocommerce-checkout" action="` + gooEsc(checkoutURL) + `" enctype="multipart/form-data" novalidate="novalidate">`)
	b.WriteString(`<div class="col2-set" id="customer_details"><div class="col-1"><div class="woocommerce-billing-fields">`)
	if !cart.NeedsShipping() || s.gooOptString("ship_to_destination") == "billing_only" {
		b.WriteString(`<h3>Billing &amp; Shipping</h3>`)
	} else {
		b.WriteString(`<h3>Billing details</h3>`)
	}
	b.WriteString(`<div class="woocommerce-billing-fields__field-wrapper">` + s.gooAddressFieldsHTML(c, "billing", value) + `</div></div>`)
	if u == nil && s.gooOptBool("enable_signup_and_login_from_checkout") {
		b.WriteString(`<div class="woocommerce-account-fields">`)
		guest := s.gooOptBool("enable_guest_checkout")
		if guest {
			chk := ""
			if c.PostForm("createaccount") != "" {
				chk = ` checked="checked"`
			}
			b.WriteString(`<p class="form-row form-row-wide create-account"><label class="woocommerce-form__label woocommerce-form__label-for-checkbox checkbox"><input class="woocommerce-form__input woocommerce-form__input-checkbox input-checkbox" id="createaccount"` + chk + ` type="checkbox" name="createaccount" value="1" /> <span>Create an account?</span></label></p>`)
		}
		var acc strings.Builder
		if !s.gooOptBool("registration_generate_username") {
			acc.WriteString(s.gooFormField(gooCheckoutField{Key: "account_username", Label: "Account username", Type: "text", Required: true, Autocomplete: "username", Placeholder: "Username"}, c.PostForm("account_username"), ""))
		}
		if !s.gooOptBool("registration_generate_password") {
			acc.WriteString(s.gooFormField(gooCheckoutField{Key: "account_password", Label: "Create account password", Type: "password", Required: true, Autocomplete: "new-password", Placeholder: "Password"}, "", ""))
		}
		if acc.Len() > 0 {
			b.WriteString(`<div class="create-account">` + acc.String() + `<div class="clear"></div></div>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div><div class="col-2"><div class="woocommerce-shipping-fields">`)
	if cart.NeedsShipping() && s.gooOptString("ship_to_destination") != "billing_only" {
		chk := ""
		if cart.Customer().ShipToDifferent || s.gooOptString("ship_to_destination") == "shipping" && c.Request.Method != http.MethodPost {
			chk = ` checked="checked"`
		}
		b.WriteString(`<h3 id="ship-to-different-address"><label class="woocommerce-form__label woocommerce-form__label-for-checkbox checkbox"><input id="ship-to-different-address-checkbox" class="woocommerce-form__input woocommerce-form__input-checkbox input-checkbox"` + chk + ` type="checkbox" name="ship_to_different_address" value="1" /> <span>Ship to a different address?</span></label></h3>`)
		b.WriteString(`<div class="shipping_address"><div class="woocommerce-shipping-fields__field-wrapper">` + s.gooAddressFieldsHTML(c, "shipping", value) + `</div></div>`)
	}
	b.WriteString(`</div><div class="woocommerce-additional-fields">`)
	if !cart.NeedsShipping() || s.gooOptString("ship_to_destination") == "billing_only" {
		b.WriteString(`<h3>Additional information</h3>`)
	}
	b.WriteString(`<div class="woocommerce-additional-fields__field-wrapper">` + s.gooFormField(gooCheckoutField{Key: "order_comments", Label: "Order notes", Type: "textarea", Class: []string{"notes"},
		Placeholder: "Notes about your order, e.g. special notes for delivery."}, c.PostForm("order_comments"), "") + `</div></div>`)
	b.WriteString(`</div></div>`)
	b.WriteString(`<h3 id="order_review_heading">Your order</h3>`)
	b.WriteString(`<div id="order_review" class="woocommerce-checkout-review-order">`)
	b.WriteString(s.gooReviewOrderHTML(c, cart))
	b.WriteString(s.gooPaymentHTML(c, cart))
	b.WriteString(`</div>`)
	b.WriteString(string(s.gooPageHTML(c, "checkout_after_order_review")))
	b.WriteString(`</form></div>`)
	s.gooSaveSession(c)
	return b.String()
}

// gooReviewOrderHTML is review-order.php.
func (s *Server) gooReviewOrderHTML(c *gin.Context, cart *gooCart) string {
	ctx := c.Request.Context()
	var b strings.Builder
	b.WriteString(`<table class="shop_table woocommerce-checkout-review-order-table"><thead><tr><th class="product-name">Product</th><th class="product-total">Subtotal</th></tr></thead><tbody>`)
	for _, it := range cart.Items {
		if !s.gooCartItemVisible(ctx, it, "checkout") {
			continue
		}
		b.WriteString(`<tr class="cart_item"><td class="product-name">` + gooEsc(s.gooProductTitle(ctx, it.product)) + `&nbsp;<strong class="product-quantity">&times;&nbsp;` + strconv.Itoa(it.Quantity) + `</strong>` +
			s.gooItemDataHTML(ctx, it) + `</td><td class="product-total">` + string(s.gooPrice(cart.ItemSubtotalDisplay(it))) + s.gooIncTaxNote(cart, it) + `</td></tr>`)
	}
	b.WriteString(`</tbody><tfoot>`)
	b.WriteString(s.gooTotalsRows(c, cart, false))
	b.WriteString(`</tfoot></table>`)
	return b.String()
}

// gooPaymentHTML is payment.php with the place order button.
func (s *Server) gooPaymentHTML(c *gin.Context, cart *gooCart) string {
	ctx := c.Request.Context()
	var b strings.Builder
	b.WriteString(`<div id="payment" class="woocommerce-checkout-payment">`)
	if cart.NeedsPayment() {
		gws := s.gooAvailableGateways(c, cart)
		chosen := s.gooSessionFor(c).Data.ChosenPayment
		if v := c.PostForm("payment_method"); v != "" {
			chosen = v
		}
		found := false
		for _, g := range gws {
			if g.ID() == chosen {
				found = true
			}
		}
		if !found && len(gws) > 0 {
			chosen = gws[0].ID()
		}
		b.WriteString(`<ul class="wc_payment_methods payment_methods methods">`)
		if len(gws) == 0 {
			b.WriteString(`<li class="woocommerce-notice woocommerce-notice--info woocommerce-info">Sorry, it seems that there are no available payment methods. Please contact us if you require assistance or wish to make alternate arrangements.</li>`)
		}
		for _, g := range gws {
			chk, style := "", ` style="display:none;"`
			if g.ID() == chosen {
				chk, style = ` checked='checked'`, ""
			}
			b.WriteString(`<li class="wc_payment_method payment_method_` + gooEsc(g.ID()) + `"><input id="payment_method_` + gooEsc(g.ID()) + `" type="radio" class="input-radio" name="payment_method" value="` +
				gooEsc(g.ID()) + `"` + chk + ` data-order_button_text="" /><label for="payment_method_` + gooEsc(g.ID()) + `">` + gooEsc(s.gooT(ctx, g.Title())) + `</label>`)
			if fields := g.PaymentFields(s, c); strings.TrimSpace(fields) != "" {
				if !strings.HasPrefix(strings.TrimSpace(fields), "<") {
					fields = wp.Autop(fields)
				}
				b.WriteString(`<div class="payment_box payment_method_` + gooEsc(g.ID()) + `"` + style + `>` + fields + `</div>`)
			}
			b.WriteString(`</li>`)
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(`<div class="form-row place-order"><noscript>Since your browser does not support JavaScript, or it is disabled, please ensure you click the <em>Update Totals</em> button before placing your order. You may be charged more than the amount stated above if you fail to do so.<br/><button type="submit" class="button alt" name="woocommerce_checkout_update_totals" value="Update totals">Update totals</button></noscript>`)
	b.WriteString(string(s.gooPageHTML(c, "checkout_before_payment")))
	b.WriteString(`<div class="woocommerce-terms-and-conditions-wrapper">`)
	if txt := strings.TrimSpace(s.gooOptString("checkout_privacy_policy_text")); txt != "" {
		b.WriteString(`<div class="woocommerce-privacy-policy-text">` + wp.Autop(s.gooPrivacyText(c, txt)) + `</div>`)
	}
	if tp := s.gooTermsPage(ctx); tp != nil {
		chk := ""
		if c.PostForm("terms") != "" {
			chk = ` checked="checked"`
		}
		b.WriteString(`<p class="form-row validate-required"><label class="woocommerce-form__label woocommerce-form__label-for-checkbox checkbox"><input type="checkbox" class="woocommerce-form__input woocommerce-form__input-checkbox input-checkbox" name="terms"` + chk + ` id="terms" />` +
			` <span class="woocommerce-terms-and-conditions-checkbox-text">I have read and agree to the website <a href="` + gooEsc(s.links(ctx).Permalink(tp)) + `" class="woocommerce-terms-and-conditions-link" target="_blank">` + gooEsc(s.gooL10nText(ctx, "terms and conditions")) + `</a></span>&nbsp;<abbr class="required" title="required">*</abbr></label>` +
			`<input type="hidden" name="terms-field" value="1" /></p>`)
	}
	b.WriteString(`</div>`)
	place := gooEsc(s.gooL10nText(ctx, s.gooPlaceOrderText(ctx, cart))) // the script puts it back when the payment method changes
	b.WriteString(`<button type="submit" class="button alt" name="woocommerce_checkout_place_order" id="place_order" value="` + place + `" data-value="` + place + `">` + place + `</button>`)
	b.WriteString(`<input type="hidden" name="woocommerce-process-checkout-nonce" value="` + s.nonce(c, "woocommerce-process_checkout") + `" />`)
	b.WriteString(`</div></div>`)
	return b.String()
}

// gooPrivacyText replaces [privacy_policy] with the privacy page link (wc_replace_policy_page_link_placeholders).
func (s *Server) gooPrivacyText(c *gin.Context, txt string) string {
	ctx := c.Request.Context()
	link := gooEsc(s.gooL10nText(ctx, "privacy policy"))
	if id := int64(s.settings.Int("wp_page_for_privacy_policy")); id > 0 {
		if p, err := s.store.GetPost(ctx, id); err == nil && p.Status == "publish" {
			link = `<a href="` + gooEsc(s.links(ctx).Permalink(p)) + `" class="woocommerce-privacy-policy-link" target="_blank">` + link + `</a>`
		}
	}
	return strings.ReplaceAll(wp.KsesPost(txt), "[privacy_policy]", link)
}

// Checkout actions -------------------------------------------------------------------------------------------------

// gooCheckoutForm is the posted checkout (an AJAX post_data string is parsed too).
func gooCheckoutForm(c *gin.Context) url.Values {
	f := c.Request.PostForm
	if pd := f.Get("post_data"); pd != "" {
		if parsed, err := url.ParseQuery(pd); err == nil {
			for k, v := range f {
				if k != "post_data" {
					parsed[k] = v
				}
			}
			c.Request.PostForm = parsed
			return parsed
		}
	}
	return f
}

// gooUpdateOrderReview is WC_AJAX::update_order_review().
func (s *Server) gooUpdateOrderReview(c *gin.Context) {
	form := gooCheckoutForm(c)
	if !s.checkNonceValue(c, form.Get("security"), "update-order-review") {
		c.JSON(http.StatusOK, gin.H{"result": "failure", "messages": s.gooL10n(c, gooErrorList([]string{"Your session has expired. Please reload the page and try again."})), "reload": true})
		return
	}
	cart := s.gooCartFor(c)
	if cart.IsEmpty() {
		c.JSON(http.StatusOK, gin.H{"result": "failure", "messages": s.gooL10n(c, gooErrorList([]string{"Sorry, your session has expired."})), "reload": true})
		return
	}
	d := gooPostedCheckout(form)
	s.gooUpdateCustomerFromCheckout(c, d)
	cart.Calculate()
	frags := map[string]string{
		".woocommerce-checkout-review-order-table": s.gooL10n(c, s.gooReviewOrderHTML(c, cart)),
		".woocommerce-checkout-payment":            s.gooL10n(c, s.gooPaymentHTML(c, cart)),
	}
	s.gooSaveSession(c)
	c.JSON(http.StatusOK, gin.H{"result": "success", "messages": "", "reload": false, "fragments": frags})
}

// gooCheckoutAjax is WC_AJAX::checkout(): JSON {result, redirect} or {result: failure, messages}.
func (s *Server) gooCheckoutAjax(c *gin.Context) {
	form := gooCheckoutForm(c)
	res, errs := s.gooRunCheckout(c, form)
	s.gooSaveSession(c)
	if len(errs) > 0 {
		c.JSON(http.StatusOK, gin.H{"result": "failure", "messages": s.gooL10n(c, gooErrorList(errs)), "refresh": false, "reload": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"result": res.Result, "redirect": res.Redirect})
}

// gooCheckoutPost is the checkout form posted without JavaScript.
func (s *Server) gooCheckoutPost(c *gin.Context) {
	ctx := c.Request.Context()
	form := c.Request.PostForm
	if form.Get("woocommerce_checkout_update_totals") != "" {
		d := gooPostedCheckout(form)
		s.gooUpdateCustomerFromCheckout(c, d)
		s.gooRedirect(c, s.gooPageURL(ctx, "checkout"))
		return
	}
	res, errs := s.gooRunCheckout(c, form)
	if len(errs) > 0 {
		for _, e := range errs {
			s.gooAddNotice(c, "error", e)
		}
		s.gooRedirect(c, s.gooPageURL(ctx, "checkout"))
		return
	}
	s.gooRedirect(c, res.Redirect)
}

// gooRunCheckout checks the nonce and processes the checkout.
func (s *Server) gooRunCheckout(c *gin.Context, form url.Values) (*gooPaymentResult, []string) {
	if !s.checkNonceValue(c, form.Get("woocommerce-process-checkout-nonce"), "woocommerce-process_checkout") {
		return nil, []string{"We were unable to process your order, please try again."}
	}
	if s.gooCartFor(c).IsEmpty() {
		return nil, []string{fmt.Sprintf(`Sorry, your session has expired. <a href="%s" class="wc-backward">Return to shop</a>`, html.EscapeString(s.gooShopURL(c.Request.Context())))}
	}
	res, _, errs := s.gooProcessCheckout(c, form)
	if len(errs) > 0 {
		return nil, errs
	}
	if res == nil {
		return nil, []string{"There was an error processing your order. Please check for any charges in your payment method and review your order history before placing the order again."}
	}
	return res, nil
}

// Order received --------------------------------------------------------------------------------------------------

// gooThankYouHTML is thankyou.php; a wrong key shows no order details.
func (s *Server) gooThankYouHTML(c *gin.Context, id int64) string {
	ctx := c.Request.Context()
	o, err := s.gooGetOrder(ctx, id)
	if err != nil || o.Type != "shop_order" || o.OrderKey == "" || c.Query("key") != o.OrderKey {
		return `<div class="woocommerce-order"><p class="woocommerce-notice woocommerce-notice--success woocommerce-thankyou-order-received">Thank you. Your order has been received.</p></div>`
	}
	u := currentUser(c)
	if o.CustomerID != 0 && (u == nil || (u.ID != o.CustomerID && !u.Can("edit_shop_orders", nil))) {
		// woocommerce_order_received_verify_known_shoppers: a registered customer's order shows its
		// details (addresses, e-mail, downloads) only to that customer, not to anyone holding the URL.
		out := `<div class="woocommerce-order"><p class="woocommerce-notice woocommerce-notice--success woocommerce-thankyou-order-received">Thank you. Your order has been received.</p>`
		if u == nil {
			out += `<div class="woocommerce-info">Please log in to your account to view this order.</div>` + s.gooLoginFormHTML(c, s.gooCurrentURL(c), false)
		} else {
			out += `<div class="woocommerce-info">This order belongs to a different account. Please log in with the account that placed it to view it.</div>`
		}
		return out + `</div>`
	}
	var b strings.Builder
	b.WriteString(`<div class="woocommerce-order">`)
	if o.Status == "failed" {
		b.WriteString(`<p class="woocommerce-notice woocommerce-notice--error woocommerce-thankyou-order-failed">Unfortunately your order cannot be processed as the originating bank/merchant has declined your transaction. Please attempt your purchase again.</p>`)
		b.WriteString(`<p class="woocommerce-notice woocommerce-notice--error woocommerce-thankyou-order-failed-actions"><a href="` + gooEsc(s.gooOrderPayURL(ctx, o)) + `" class="button pay">Pay</a>`)
		if u != nil {
			b.WriteString(`<a href="` + gooEsc(s.gooPageURL(ctx, "myaccount")) + `" class="button pay">My account</a>`)
		}
		b.WriteString(`</p>`)
	} else {
		b.WriteString(`<p class="woocommerce-notice woocommerce-notice--success woocommerce-thankyou-order-received">Thank you. Your order has been received.</p>`)
		b.WriteString(`<ul class="woocommerce-order-overview woocommerce-thankyou-order-details order_details">`)
		b.WriteString(`<li class="woocommerce-order-overview__order order">Order number: <strong>` + gooEsc(o.Number()) + `</strong></li>`)
		b.WriteString(`<li class="woocommerce-order-overview__date date">Date: <strong>` + wp.FormatDate(o.DateCreated.In(s.loc()), s.settings.String("date_format")) + `</strong></li>`)
		if u != nil && u.ID == o.CustomerID && o.Billing["email"] != "" {
			b.WriteString(`<li class="woocommerce-order-overview__email email">Email: <strong>` + gooEsc(o.Billing["email"]) + `</strong></li>`)
		}
		b.WriteString(`<li class="woocommerce-order-overview__total total">Total: <strong>` + string(s.gooPriceIn(o.Total, o.Currency)) + `</strong></li>`)
		if o.PaymentMethodTitle != "" {
			b.WriteString(`<li class="woocommerce-order-overview__payment-method method">Payment method: <strong>` + gooEsc(o.PaymentMethodTitle) + `</strong></li>`)
		}
		b.WriteString(`</ul>`)
	}
	if g := s.gooGateway(o.PaymentMethod); g != nil {
		b.WriteString(g.ThankYou(s, ctx, o))
	}
	c.Set("goo.order", o)
	b.WriteString(string(s.gooPageHTML(c, "thankyou")))
	b.WriteString(s.gooOrderDetailsHTML(c, o, false))
	b.WriteString(`</div>`)
	return b.String()
}

// gooOrderTotals is WC_Order::get_order_item_totals().
func (s *Server) gooOrderTotals(ctx context.Context, o *gooOrder) [][3]string {
	price := func(v float64) string { return string(s.gooPriceIn(v, o.Currency)) }
	incl := s.gooOptString("tax_display_cart") == "incl"
	var rows [][3]string
	sub := o.Subtotal()
	if incl {
		for _, it := range o.ItemsOf("line_item") {
			sub += it.SubtotalTax
		}
	}
	rows = append(rows, [3]string{"cart_subtotal", "Subtotal:", price(sub)})
	if o.DiscountTotal > 0 {
		d := o.DiscountTotal
		if incl {
			d += o.DiscountTax
		}
		rows = append(rows, [3]string{"discount", "Discount:", "-" + price(d)})
	}
	if ships := o.ItemsOf("shipping"); len(ships) > 0 {
		var names []string
		for _, sh := range ships {
			names = append(names, gooEsc(sh.Name))
		}
		amt := o.ShippingTotal
		if incl {
			amt += o.ShippingTax
		}
		v := price(amt)
		if amt == 0 {
			v = strings.Join(names, ", ")
		} else {
			v += `&nbsp;<small class="shipped_via">` + s.gooL10nf(ctx, "via %s", strings.Join(names, ", ")) + `</small>`
		}
		rows = append(rows, [3]string{"shipping", "Shipping:", v})
	}
	for _, f := range o.ItemsOf("fee") {
		amt := f.Total
		if incl {
			amt += f.TotalTax
		}
		rows = append(rows, [3]string{"fee_" + itoa64(f.ID), gooEsc(f.Name) + ":", price(amt)})
	}
	if s.gooTaxEnabled() && !incl {
		for _, t := range o.ItemsOf("tax") {
			label := t.MetaString("label")
			amt := 0.0
			if v, ok := t.Meta["tax_amount"].(float64); ok {
				amt += v
			}
			if v, ok := t.Meta["shipping_tax_amount"].(float64); ok {
				amt += v
			}
			rows = append(rows, [3]string{"tax_" + gooEsc(strings.ToLower(t.Name)), gooEsc(defaultStr(label, t.Name)) + ":", price(amt)})
		}
	}
	if o.Total > 0 && o.PaymentMethodTitle != "" {
		rows = append(rows, [3]string{"payment_method", "Payment method:", gooEsc(o.PaymentMethodTitle)})
	}
	total := price(o.Total)
	if s.gooTaxEnabled() && incl && o.TotalTax > 0 {
		total += ` <small class="includes_tax">(includes ` + price(o.TotalTax) + ` ` + s.gooTaxLabel() + `)</small>`
	}
	if refunded := s.gooTotalRefunded(ctx, o.ID); refunded > 0 {
		total = `<del aria-hidden="true">` + price(o.Total) + `</del> <ins>` + price(o.Total-refunded) + `</ins>`
	}
	rows = append(rows, [3]string{"order_total", "Total:", total})
	return rows
}

// gooOrderDetailsHTML is order-details.php + order-details-customer.php (+ the downloads table).
func (s *Server) gooOrderDetailsHTML(c *gin.Context, o *gooOrder, account bool) string {
	ctx := c.Request.Context()
	var b strings.Builder
	if o.IsPaid() || o.Status == "completed" {
		if dl := s.gooDownloadsTable(c, s.gooOrderDownloads(c, o)); dl != "" {
			b.WriteString(`<section class="woocommerce-order-downloads"><h2 class="woocommerce-order-downloads__title">Downloads</h2>` + dl + `</section>`)
		}
	}
	b.WriteString(`<section class="woocommerce-order-details"><h2 class="woocommerce-order-details__title">Order details</h2>`)
	b.WriteString(`<table class="woocommerce-table woocommerce-table--order-details shop_table order_details"><thead><tr><th class="woocommerce-table__product-name product-name">Product</th><th class="woocommerce-table__product-table product-total">Total</th></tr></thead><tbody>`)
	incl := s.gooOptString("tax_display_cart") == "incl"
	for _, it := range o.ItemsOf("line_item") {
		if !s.gooOrderItemVisible(ctx, it, "order_details") {
			continue
		}
		name := gooEsc(it.Name)
		pid := it.ProductID
		if p, err := s.gooGetProduct(ctx, pid); err == nil && p.Status == "publish" {
			name = `<a href="` + gooEsc(s.gooPermalink(ctx, p)) + `">` + name + `</a>`
		}
		b.WriteString(`<tr class="woocommerce-table__line-item order_item"><td class="woocommerce-table__product-name product-name">` + name + ` <strong class="product-quantity">&times;&nbsp;` + strconv.Itoa(it.Qty()) + `</strong>`)
		if disp, ok := it.Meta["_display"].([]any); ok && len(disp) > 0 {
			b.WriteString(`<ul class="wc-item-meta">`)
			for _, d := range disp {
				m, _ := d.(map[string]any)
				k, _ := m["key"].(string)
				v, _ := m["value"].(string)
				b.WriteString(`<li><strong class="wc-item-meta-label">` + gooEsc(k) + `:</strong> <p>` + gooEsc(v) + `</p></li>`)
			}
			b.WriteString(`</ul>`)
		}
		amt := it.Subtotal
		if incl {
			amt += it.SubtotalTax
		}
		b.WriteString(`</td><td class="woocommerce-table__product-total product-total">` + string(s.gooPriceIn(amt, o.Currency)) + `</td></tr>`)
		if o.IsPaid() {
			if p, err := s.gooGetProduct(ctx, max(it.VariationID, it.ProductID)); err == nil && strings.TrimSpace(p.PurchaseNote) != "" {
				b.WriteString(`<tr class="woocommerce-table__product-purchase-note product-purchase-note"><td colspan="2">` + wp.Autop(wp.KsesPost(p.PurchaseNote)) + `</td></tr>`)
			}
		}
	}
	b.WriteString(`</tbody><tfoot>`)
	for _, r := range s.gooOrderTotals(ctx, o) {
		b.WriteString(`<tr><th scope="row">` + r[1] + `</th><td>` + r[2] + `</td></tr>`)
	}
	if o.CustomerNote != "" {
		b.WriteString(`<tr><th>Note:</th><td>` + strings.ReplaceAll(gooEsc(o.CustomerNote), "\n", "<br/>") + `</td></tr>`)
	}
	b.WriteString(`</tfoot></table>`)
	if account && o.Status == "completed" {
		b.WriteString(`<p class="order-again"><a href="` + gooEsc(s.gooPageURL(ctx, "cart")+"?order_again="+itoa64(o.ID)+"&_wpnonce="+s.nonce(c, "woocommerce-order_again")) + `" class="button">Order again</a></p>`)
	}
	b.WriteString(`</section>`)
	b.WriteString(`<section class="woocommerce-customer-details">`)
	ship := len(o.Shipping) > 0 && o.Shipping["address_1"] != ""
	if ship {
		b.WriteString(`<section class="woocommerce-columns woocommerce-columns--2 woocommerce-columns--addresses col2-set addresses"><div class="woocommerce-column woocommerce-column--1 woocommerce-column--billing-address col-1">`)
	}
	b.WriteString(`<h2 class="woocommerce-column__title">Billing address</h2><address>` + defaultStr(gooFrontFormatAddress(o.Billing), "N/A"))
	if o.Billing["phone"] != "" {
		b.WriteString(`<p class="woocommerce-customer-details--phone">` + gooEsc(o.Billing["phone"]) + `</p>`)
	}
	if o.Billing["email"] != "" {
		b.WriteString(`<p class="woocommerce-customer-details--email">` + gooEsc(o.Billing["email"]) + `</p>`)
	}
	b.WriteString(`</address>`)
	if ship {
		b.WriteString(`</div><div class="woocommerce-column woocommerce-column--2 woocommerce-column--shipping-address col-2"><h2 class="woocommerce-column__title">Shipping address</h2><address>` + gooFrontFormatAddress(o.Shipping))
		if o.Shipping["phone"] != "" {
			b.WriteString(`<p class="woocommerce-customer-details--phone">` + gooEsc(o.Shipping["phone"]) + `</p>`)
		}
		b.WriteString(`</address></div></section>`)
	}
	b.WriteString(`</section>`)
	return b.String()
}

// Order pay ---------------------------------------------------------------------------------------------------------

// gooPayableOrder loads an order the visitor may pay (key check, ownership, status).
func (s *Server) gooPayableOrder(c *gin.Context, id int64, key string) (*gooOrder, string) {
	o, err := s.gooGetOrder(c.Request.Context(), id)
	if err != nil || o.Type != "shop_order" || key == "" || o.OrderKey != key {
		return nil, "Sorry, this order is invalid and cannot be paid for."
	}
	u := currentUser(c)
	if o.CustomerID != 0 && (u == nil || u.ID != o.CustomerID) {
		if u == nil {
			return nil, `Please log in to your account below to continue to the payment form.`
		}
		return nil, "This order&rsquo;s status is &ldquo;" + gooStatusLabel(o.Status) + "&rdquo;&mdash;it cannot be paid for. Please contact us if you need assistance."
	}
	if o.Status != "pending" && o.Status != "failed" {
		return nil, "This order&rsquo;s status is &ldquo;" + gooStatusLabel(o.Status) + "&rdquo;&mdash;it cannot be paid for. Please contact us if you need assistance."
	}
	return o, ""
}

// gooOrderPayHTML is form-pay.php.
func (s *Server) gooOrderPayHTML(c *gin.Context, id int64) string {
	ctx := c.Request.Context()
	o, msg := s.gooPayableOrder(c, id, c.Query("key"))
	if o == nil {
		out := `<ul class="woocommerce-error" role="alert"><li>` + msg + `</li></ul>`
		if currentUser(c) == nil && strings.HasPrefix(msg, "Please log in") {
			out += s.gooLoginFormHTML(c, s.gooCurrentURL(c), false)
		}
		return out
	}
	var b strings.Builder
	b.WriteString(`<form id="order_review" method="post" action="` + gooEsc(s.gooOrderPayURL(ctx, o)) + `"><table class="shop_table"><thead><tr><th class="product-name">Product</th><th class="product-quantity">Qty</th><th class="product-total">Totals</th></tr></thead><tbody>`)
	for _, it := range o.ItemsOf("line_item") {
		if !s.gooOrderItemVisible(ctx, it, "order_pay") {
			continue
		}
		b.WriteString(`<tr class="order_item"><td class="product-name">` + gooEsc(it.Name) + `</td><td class="product-quantity">&times;&nbsp;` + strconv.Itoa(it.Qty()) + `</td><td class="product-subtotal">` + string(s.gooPriceIn(it.Subtotal, o.Currency)) + `</td></tr>`)
	}
	b.WriteString(`</tbody><tfoot>`)
	for _, r := range s.gooOrderTotals(ctx, o) {
		if r[0] == "payment_method" {
			continue
		}
		b.WriteString(`<tr><th scope="row" colspan="2">` + r[1] + `</th><td class="product-total">` + r[2] + `</td></tr>`)
	}
	b.WriteString(`</tfoot></table><div id="payment">`)
	if o.Total > 0 {
		b.WriteString(`<ul class="wc_payment_methods payment_methods methods">`)
		gws := s.gooAvailableGatewaysForOrder(c, o)
		if len(gws) == 0 {
			b.WriteString(`<li class="woocommerce-notice woocommerce-notice--info woocommerce-info">Sorry, it seems that there are no available payment methods for your location. Please contact us if you require assistance or wish to make alternate arrangements.</li>`)
		}
		for i, g := range gws {
			chk := ""
			if i == 0 {
				chk = ` checked='checked'`
			}
			b.WriteString(`<li class="wc_payment_method payment_method_` + gooEsc(g.ID()) + `"><input id="payment_method_` + gooEsc(g.ID()) + `" type="radio" class="input-radio" name="payment_method" value="` + gooEsc(g.ID()) + `"` + chk + ` /><label for="payment_method_` + gooEsc(g.ID()) + `">` + gooEsc(s.gooT(ctx, g.Title())) + `</label>`)
			if f := g.PaymentFields(s, c); strings.TrimSpace(f) != "" {
				b.WriteString(`<div class="payment_box payment_method_` + gooEsc(g.ID()) + `">` + f + `</div>`)
			}
			b.WriteString(`</li>`)
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(`<div class="form-row"><input type="hidden" name="woocommerce_pay" value="1" /><input type="hidden" name="order_id" value="` + itoa64(o.ID) + `" /><input type="hidden" name="key" value="` + gooEsc(o.OrderKey) + `" />`)
	if s.gooTermsPage(c.Request.Context()) != nil {
		b.WriteString(`<p class="form-row validate-required"><label class="woocommerce-form__label woocommerce-form__label-for-checkbox checkbox"><input type="checkbox" class="woocommerce-form__input woocommerce-form__input-checkbox input-checkbox" name="terms" id="terms" /> <span class="woocommerce-terms-and-conditions-checkbox-text">I have read and agree to the website terms and conditions</span>&nbsp;<abbr class="required" title="required">*</abbr></label><input type="hidden" name="terms-field" value="1" /></p>`)
	}
	b.WriteString(`<button type="submit" class="button alt" id="place_order" value="Pay for order" data-value="Pay for order">Pay for order</button>`)
	b.WriteString(`<input type="hidden" name="woocommerce-pay-nonce" value="` + s.nonce(c, "woocommerce-pay") + `" /></div></div></form>`)
	return b.String()
}

// gooPayPost is WC_Form_Handler::pay_action().
func (s *Server) gooPayPost(c *gin.Context) {
	ctx := c.Request.Context()
	o, msg := s.gooPayableOrder(c, gooAtoi64(c.PostForm("order_id")), c.PostForm("key"))
	back := s.gooCurrentURL(c)
	if o == nil {
		s.gooAddNotice(c, "error", msg)
		s.gooRedirect(c, back)
		return
	}
	back = s.gooOrderPayURL(ctx, o)
	if !s.checkNonceValue(c, c.PostForm("woocommerce-pay-nonce"), "woocommerce-pay") {
		s.gooAddNotice(c, "error", "Your session has expired. Please reload the page and try again.")
		s.gooRedirect(c, back)
		return
	}
	if c.PostForm("terms-field") != "" && c.PostForm("terms") == "" && s.gooTermsPage(ctx) != nil {
		s.gooAddNotice(c, "error", "Please read and accept the terms and conditions to proceed with your order.")
		s.gooRedirect(c, back)
		return
	}
	if o.Total <= 0 {
		_ = s.gooPaymentComplete(ctx, o, "")
		s.gooRedirect(c, s.gooOrderReceivedURL(ctx, o))
		return
	}
	id := c.PostForm("payment_method")
	var gw *gooGatewayHandle
	for _, g := range s.gooAvailableGatewaysForOrder(c, o) {
		if g.ID() == id {
			gw = g
		}
	}
	if gw == nil {
		s.gooAddNotice(c, "error", "Invalid payment method.")
		s.gooRedirect(c, back)
		return
	}
	if err := gw.Validate(s, c); err != nil {
		s.gooAddNotice(c, "error", err.Error())
		s.gooRedirect(c, back)
		return
	}
	// The order's stock is held (again) while it is paid: a failed or expired hold must not let the
	// payment take units another checkout holds (WC_Form_Handler::pay_action()).
	if err := s.gooReserveStock(ctx, o); err != nil {
		s.gooAddNotice(c, "error", err.Error())
		s.gooRedirect(c, back)
		return
	}
	o.PaymentMethod, o.PaymentMethodTitle = gw.ID(), gw.Title()
	_ = s.gooSaveOrder(ctx, o, false)
	res, err := gw.ProcessPayment(s, c, o)
	if err != nil || res.Result != "success" {
		m := "There was an error processing your order. Please check for any charges in your payment method and review your order history before placing the order again."
		if err != nil {
			m = err.Error()
		} else if res.Message != "" {
			m = res.Message
		}
		s.gooAddNotice(c, "error", m)
		s.gooRedirect(c, back)
		return
	}
	s.gooRedirect(c, res.Redirect)
}

var _ = time.Now
