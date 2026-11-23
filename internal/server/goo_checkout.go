package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/auth"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// The checkout (WC_Checkout): its fields, validation and process_checkout(): customer data into the
// session, an account when asked, the order created from the cart, then the gateway's payment.

// gooCheckoutField is a checkout form field (woocommerce_checkout_fields).
type gooCheckoutField struct {
	Key          string // billing_first_name …
	Label        string
	Type         string // text, email, tel, country, state, textarea, password, checkbox
	Required     bool
	Class        []string
	Autocomplete string
	Priority     int
	Placeholder  string
}

// gooAddressFields is WC_Countries::get_address_fields() for a type (billing / shipping).
func (s *Server) gooAddressFields(ctx context.Context, typ string) []gooCheckoutField {
	modes := s.gooCheckoutBlockFieldModes(ctx) // the Checkout block's field settings win over the options
	opt := func(key string) (show, req bool) {
		mode, ok := modes[key]
		if !ok {
			mode = s.gooOptString(key)
		}
		switch mode {
		case "hidden":
			return false, false
		case "required":
			return true, true
		}
		return true, false
	}
	var out []gooCheckoutField
	add := func(f gooCheckoutField) {
		f.Key = typ + "_" + f.Key
		out = append(out, f)
	}
	add(gooCheckoutField{Key: "first_name", Label: "First name", Type: "text", Required: true, Class: []string{"form-row-first"}, Autocomplete: "given-name", Priority: 10})
	add(gooCheckoutField{Key: "last_name", Label: "Last name", Type: "text", Required: true, Class: []string{"form-row-last"}, Autocomplete: "family-name", Priority: 20})
	if show, req := opt("checkout_company_field"); show {
		add(gooCheckoutField{Key: "company", Label: "Company name", Type: "text", Required: req, Class: []string{"form-row-wide"}, Autocomplete: "organization", Priority: 30})
	}
	add(gooCheckoutField{Key: "country", Label: "Country / Region", Type: "country", Required: true, Class: []string{"form-row-wide", "address-field", "update_totals_on_change"}, Autocomplete: "country", Priority: 40})
	add(gooCheckoutField{Key: "address_1", Label: "Street address", Type: "text", Required: true, Class: []string{"form-row-wide", "address-field"}, Autocomplete: "address-line1", Placeholder: "House number and street name", Priority: 50})
	if show, req := opt("checkout_address_2_field"); show {
		add(gooCheckoutField{Key: "address_2", Label: "Apartment, suite, unit, etc.", Type: "text", Required: req, Class: []string{"form-row-wide", "address-field"}, Autocomplete: "address-line2", Placeholder: "Apartment, suite, unit, etc. (optional)", Priority: 60})
	}
	add(gooCheckoutField{Key: "city", Label: "Town / City", Type: "text", Required: true, Class: []string{"form-row-wide", "address-field"}, Autocomplete: "address-level2", Priority: 70})
	add(gooCheckoutField{Key: "state", Label: "State / County", Type: "state", Required: true, Class: []string{"form-row-wide", "address-field"}, Autocomplete: "address-level1", Priority: 80})
	add(gooCheckoutField{Key: "postcode", Label: "Postcode / ZIP", Type: "text", Required: true, Class: []string{"form-row-wide", "address-field"}, Autocomplete: "postal-code", Priority: 90})
	if typ == "billing" {
		if show, req := opt("checkout_phone_field"); show {
			add(gooCheckoutField{Key: "phone", Label: "Phone", Type: "tel", Required: req, Class: []string{"form-row-wide"}, Autocomplete: "tel", Priority: 100})
		}
		add(gooCheckoutField{Key: "email", Label: "Email address", Type: "email", Required: true, Class: []string{"form-row-wide"}, Autocomplete: "email username", Priority: 110})
	}
	return out
}

// gooStateRequired reports whether a country needs a state (it has a state list).
func gooStateRequired(country string) bool {
	_, ok := gooStates[country]
	return ok
}

// gooPostcodeOptional are countries without postcodes in WooCommerce's locale.
var gooPostcodeOptional = map[string]bool{"AE": true, "AO": true, "BS": true, "BZ": true, "HK": true, "IE": false, "QA": true, "SA": false, "TT": true}

var gooPhoneRe = regexp.MustCompile(`^[\s\#0-9_\-\+\/\(\)\.]*$`)

// gooCheckoutData is the posted checkout (WC_Checkout::get_posted_data()).
type gooCheckoutData struct {
	Fields          map[string]string
	ShipToDifferent bool
	CreateAccount   bool
	Terms           bool
	TermsField      bool // the form showed the terms checkbox (terms-field)
	PaymentMethod   string
	OrderComments   string
	ShippingMethods map[string]string
}

// gooPostedCheckout reads the checkout form.
func gooPostedCheckout(form map[string][]string) *gooCheckoutData {
	get := func(k string) string {
		if v := form[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	d := &gooCheckoutData{Fields: map[string]string{}, ShippingMethods: map[string]string{}}
	for _, t := range []string{"billing", "shipping"} {
		for _, k := range gooAddressKeys {
			d.Fields[t+"_"+k] = get(t + "_" + k)
		}
	}
	d.Fields["account_username"], d.Fields["account_password"] = get("account_username"), get("account_password")
	d.ShipToDifferent = get("ship_to_different_address") != ""
	d.CreateAccount = get("createaccount") != ""
	d.Terms = get("terms") != ""
	d.TermsField = get("terms-field") != ""
	d.PaymentMethod = get("payment_method")
	d.OrderComments = get("order_comments")
	for k, v := range form {
		if idx, ok := gooPostedShippingIndex(k); ok && len(v) > 0 {
			d.ShippingMethods[strconv.Itoa(idx)] = v[0]
		}
	}
	return d
}

// gooUpdateCustomerFromCheckout copies posted addresses into the session (update_order_review).
func (s *Server) gooUpdateCustomerFromCheckout(c *gin.Context, d *gooCheckoutData) {
	sess := s.gooSessionFor(c)
	cu := &sess.Data.Customer
	for _, k := range gooAddressKeys {
		if v, ok := d.Fields["billing_"+k]; ok {
			cu.Billing[k] = v
		}
		if k != "email" && k != "phone" {
			if v, ok := d.Fields["shipping_"+k]; ok && d.ShipToDifferent {
				cu.Shipping[k] = v
			}
		}
	}
	if !d.ShipToDifferent {
		for _, k := range []string{"first_name", "last_name", "company", "address_1", "address_2", "city", "state", "postcode", "country"} {
			cu.Shipping[k] = cu.Billing[k]
		}
	}
	cu.ShipToDifferent = d.ShipToDifferent
	cu.CalculatedShip = true
	// Only the cart's packages: the rates themselves are checked when the cart is recalculated with the
	// posted address, but an unbounded set of indexes must not be stored in the session.
	packages := len(s.gooCartFor(c).Packages)
	for k, v := range d.ShippingMethods {
		if idx, err := strconv.Atoi(k); err == nil && idx < packages {
			sess.Data.ChosenShipping[k] = v
		}
	}
	if d.PaymentMethod != "" {
		sess.Data.ChosenPayment = d.PaymentMethod
	}
	sess.touch()
}

// gooValidateCheckout is WC_Checkout::validate_checkout(); returns the error notices.
func (s *Server) gooValidateCheckout(c *gin.Context, cart *gooCart, d *gooCheckoutData) []string {
	var errs []string
	u := currentUser(c)
	check := func(typ string) {
		prefix := map[string]string{"billing": "Billing", "shipping": "Shipping"}[typ]
		country := d.Fields[typ+"_country"]
		for _, f := range s.gooAddressFields(c.Request.Context(), typ) {
			v := d.Fields[f.Key]
			required := f.Required
			if strings.HasSuffix(f.Key, "_state") {
				required = gooStateRequired(country)
			}
			if strings.HasSuffix(f.Key, "_postcode") && gooPostcodeOptional[country] {
				required = false
			}
			if required && v == "" {
				errs = append(errs, fmt.Sprintf("<strong>%s</strong> is a required field.", s.gooFieldName(c, prefix, f.Label)))
				continue
			}
			if v == "" {
				continue
			}
			switch {
			case strings.HasSuffix(f.Key, "_email"):
				if _, err := mail.ParseAddress(v); err != nil || !strings.Contains(v, "@") {
					errs = append(errs, fmt.Sprintf("<strong>%s</strong> is not a valid email address.", s.gooFieldName(c, prefix, f.Label)))
				}
			case strings.HasSuffix(f.Key, "_phone"):
				if !gooPhoneRe.MatchString(v) {
					errs = append(errs, fmt.Sprintf("<strong>%s %s</strong> is not a valid phone number.", prefix, f.Label))
				}
			case strings.HasSuffix(f.Key, "_country"):
				ok := false
				list := s.gooAllowedCountries()
				if typ == "shipping" {
					list = s.gooShippingCountries()
				}
				for _, cc := range list {
					if cc.Code == v {
						ok = true
					}
				}
				if !ok {
					errs = append(errs, fmt.Sprintf("&quot;%s&quot; is not a valid country code.", html.EscapeString(v)))
				}
			case strings.HasSuffix(f.Key, "_state"):
				if states, ok := gooStates[country]; ok {
					found := false
					for _, st := range states {
						if strings.EqualFold(st.Code, v) || strings.EqualFold(st.Name, v) {
							d.Fields[f.Key] = st.Code
							found = true
						}
					}
					if !found {
						errs = append(errs, fmt.Sprintf("%s %s is not valid. Please enter one of the following: %s", prefix, f.Label, html.EscapeString(gooStateNames(country))))
					}
				}
			}
		}
	}
	check("billing")
	if cart.NeedsShipping() && d.ShipToDifferent && s.gooOptString("ship_to_destination") != "billing_only" {
		check("shipping")
	}
	if cart.IsEmpty() {
		errs = append(errs, fmt.Sprintf(`Sorry, your session has expired. <a href="%s" class="wc-backward">Return to shop</a>`, html.EscapeString(s.gooShopURL(c.Request.Context()))))
	}
	if cart.NeedsShipping() {
		if len(cart.Packages) == 0 || cart.Packages[0].Chosen == nil {
			if cart.Packages != nil && len(cart.Packages[0].Rates) == 0 {
				errs = append(errs, "No shipping method has been selected. Please double check your address, or contact us if you need any help.")
			} else {
				errs = append(errs, "No shipping method has been selected. Please double check your address, or contact us if you need any help.")
			}
		}
	}
	if cart.NeedsPayment() {
		gw := s.gooGateway(d.PaymentMethod)
		ok := false
		for _, a := range s.gooAvailableGateways(c, cart) {
			if a.ID() == d.PaymentMethod {
				ok = true
			}
		}
		if gw == nil || !ok {
			errs = append(errs, "Invalid payment method.")
		} else if err := gw.Validate(s, c); err != nil {
			errs = append(errs, err.Error())
		}
	}
	// WC_Checkout::validate_checkout(): the box is required when the form showed it (terms-field) and
	// the terms page is still published, so an unpublished page does not block every checkout.
	if d.TermsField && !d.Terms && s.gooTermsPage(c.Request.Context()) != nil {
		errs = append(errs, "Please read and accept the terms and conditions to proceed with your order.")
	}
	if u == nil {
		if d.CreateAccount && s.gooOptBool("enable_signup_and_login_from_checkout") || !s.gooOptBool("enable_guest_checkout") {
			if !s.gooOptBool("enable_signup_and_login_from_checkout") {
				errs = append(errs, "You must be logged in to checkout.")
			} else {
				if !s.gooOptBool("registration_generate_username") && d.Fields["account_username"] == "" {
					errs = append(errs, "Please enter a valid account username.")
				}
				if !s.gooOptBool("registration_generate_password") && d.Fields["account_password"] == "" {
					errs = append(errs, "Please enter an account password.")
				}
				if email := d.Fields["billing_email"]; email != "" {
					if _, err := s.store.UserByLoginOrEmail(c.Request.Context(), email); err == nil {
						errs = append(errs, `An account is already registered with your email address. <a href="#" class="showlogin">Please log in.</a>`)
					}
				}
			}
		}
	}
	// Coupons restricted to e-mails are checked against the billing e-mail now.
	for _, cp := range cart.Coupons() {
		if len(cp.EmailRestrictions) > 0 {
			emails := []string{d.Fields["billing_email"]}
			if u != nil {
				emails = append(emails, u.Email)
			}
			allowed := false
			for _, e := range emails {
				if e != "" && gooEmailAllowed(e, cp.EmailRestrictions) {
					allowed = true
				}
			}
			if !allowed {
				cart.RemoveCoupon(cp.Code)
				errs = append(errs, fmt.Sprintf(`Sorry, it seems the coupon "%s" is not yours - it has now been removed from your order.`, html.EscapeString(cp.Code)))
			}
		}
		if cp.UsageLimitPerUser != nil && *cp.UsageLimitPerUser > 0 {
			if err := s.gooValidateCoupon(c.Request.Context(), cp, cart); err != nil {
				cart.RemoveCoupon(cp.Code)
				errs = append(errs, err.Error())
			}
		}
	}
	for _, h := range gooCheckoutValidateHooks {
		errs = append(errs, h(s, c, cart, d)...)
	}
	// Stock may have changed since the items were added (WC_Cart::check_cart_item_stock()): the
	// quantities are summed per stock owner (get_cart_item_quantities() keyed by get_stock_managed_by_id()).
	need := map[int64]int{}
	for _, it := range cart.Items {
		if it.product != nil {
			need[s.gooStockOwner(c.Request.Context(), it.product).ID] += it.Quantity
		}
	}
	checked := map[int64]bool{}
	var customer int64
	if u := currentUser(c); u != nil {
		customer = u.ID
	}
	mine := gooVisitorOrders(cart.sess, customer)
	for _, it := range cart.Items {
		if it.product == nil {
			continue
		}
		owner := s.gooStockOwner(c.Request.Context(), it.product).ID
		if checked[owner] {
			continue
		}
		checked[owner] = true
		// Units other unpaid orders hold are not available (wc_get_held_stock_quantity(), excluding
		// the visitor's own orders: a retry after leaving the payment page takes their hold over).
		held := 0
		if s.gooManagingStock(c.Request.Context(), it.product) && !s.gooBackordersAllowed(c.Request.Context(), it.product) {
			held = s.gooHeldStock(c.Request.Context(), it.product, mine)
		}
		if !s.gooHasEnoughStock(c.Request.Context(), it.product, need[owner]+held) {
			errs = append(errs, fmt.Sprintf("Sorry, we do not have enough &quot;%s&quot; in stock to fulfill your order (%d available). We apologize for any inconvenience caused.",
				html.EscapeString(s.gooProductTitle(c.Request.Context(), it.product)), max(s.gooStockQty(c.Request.Context(), it.product)-held, 0)))
		}
	}
	return errs
}

func gooStateNames(country string) string {
	var names []string
	for _, st := range gooStates[country] {
		names = append(names, st.Name)
	}
	return strings.Join(names, ", ")
}

// gooProcessCheckout is WC_Checkout::process_checkout(). It returns the payment result (redirect)
// or the error notices.
func (s *Server) gooProcessCheckout(c *gin.Context, form map[string][]string) (*gooPaymentResult, *gooOrder, []string) {
	ctx := c.Request.Context()
	cart := s.gooCartFor(c)
	d := gooPostedCheckout(form)
	s.gooUpdateCustomerFromCheckout(c, d)
	cart.Calculate()
	// woocommerce_check_cart_items: a coupon, item or gift card dropped while the cart was
	// reloaded changes the total the customer saw, so stop and show why (the session already
	// lost the dropped entry, so the next attempt places the order at the new total).
	if ce := cart.Errors(); len(ce) > 0 {
		return nil, nil, ce
	}
	errs := s.gooValidateCheckout(c, cart, d)
	if len(errs) > 0 {
		return nil, nil, errs
	}
	u := currentUser(c)
	customerID := int64(0)
	if u != nil {
		customerID = u.ID
	} else if d.CreateAccount && s.gooOptBool("enable_signup_and_login_from_checkout") || !s.gooOptBool("enable_guest_checkout") {
		// The createaccount box counts only while the store lets customers sign up at checkout
		// (WC_Checkout::get_posted_data()): a posted createaccount=1 must not open accounts otherwise.
		id, err := s.gooCreateCustomer(ctx, d.Fields["billing_email"], d.Fields["account_username"], d.Fields["account_password"], d.Fields)
		if err != nil {
			return nil, nil, []string{err.Error()}
		}
		customerID = id
		if err := s.gooLoginCustomer(c, id); err != nil {
			return nil, nil, []string{err.Error()}
		}
	}
	o, err := s.gooCreateOrderFromCart(c, cart, d, customerID)
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	// Save the addresses to the customer's account.
	if customerID != 0 {
		s.gooSaveCustomerAddresses(ctx, customerID, o)
	}
	sess := s.gooSessionFor(c)
	sess.Data.OrderAwaitingPayment = o.ID
	if customerID == 0 {
		gooRememberGuestOrder(sess, o.ID)
	}
	sess.touch()
	if !cart.NeedsPayment() {
		if err := s.gooPaymentComplete(ctx, o, ""); err != nil {
			return nil, o, []string{err.Error()}
		}
		cart.Empty()
		return &gooPaymentResult{Result: "success", Redirect: s.gooOrderReceivedURL(ctx, o)}, o, nil
	}
	// wc_reserve_stock_for_order(): the order's stock is held while the customer pays, so another
	// checkout cannot sell the same last unit meanwhile. The order stays pending (a retry reuses it).
	// Holds of this visitor's earlier unpaid orders pass to this one (a retry after leaving the
	// payment page places a new order).
	s.gooReleaseOwnHolds(ctx, gooVisitorOrders(sess, customerID), o.ID)
	if err := s.gooReserveStock(ctx, o); err != nil {
		return nil, o, []string{err.Error()}
	}
	gw := s.gooGateway(d.PaymentMethod)
	res, err := gw.ProcessPayment(s, c, o)
	if err != nil || res.Result != "success" {
		msg := "There was an error processing your order. Please check for any charges in your payment method and review your order history before placing the order again."
		if err != nil {
			msg = err.Error()
		} else if res.Message != "" {
			msg = res.Message
		}
		return nil, o, []string{msg}
	}
	sess.Data.OrderAwaitingPayment = 0
	sess.touch()
	return &res, o, nil
}

// gooCreateOrderFromCart is WC_Checkout::create_order(): line items, fees, shipping, tax and coupon
// lines copied from the calculated cart. The order starts pending.
func (s *Server) gooCreateOrderFromCart(c *gin.Context, cart *gooCart, d *gooCheckoutData, customerID int64) (*gooOrder, error) {
	ctx := c.Request.Context()
	o := &gooOrder{Status: "pending", Currency: s.gooCurrency(), PricesIncludeTax: s.gooPricesIncludeTax(), CustomerID: customerID,
		Billing: map[string]string{}, Shipping: map[string]string{}, PaymentMethod: d.PaymentMethod, CustomerIP: c.ClientIP(),
		UserAgent: c.Request.UserAgent(), CustomerNote: d.OrderComments, CreatedVia: "checkout", Meta: map[string]any{}}
	for _, k := range gooAddressKeys {
		if v := d.Fields["billing_"+k]; v != "" {
			o.Billing[k] = v
		}
	}
	if cart.NeedsShipping() {
		src := "billing_"
		if d.ShipToDifferent && s.gooOptString("ship_to_destination") != "billing_only" {
			src = "shipping_"
		}
		for _, k := range []string{"first_name", "last_name", "company", "address_1", "address_2", "city", "state", "postcode", "country"} {
			if v := d.Fields[src+k]; v != "" {
				o.Shipping[k] = v
			}
		}
		if src == "billing_" && o.Billing["phone"] != "" {
			o.Shipping["phone"] = o.Billing["phone"]
		}
	}
	if gw := s.gooGateway(d.PaymentMethod); gw != nil {
		o.PaymentMethodTitle = gw.Title()
	}
	if !cart.NeedsPayment() {
		o.PaymentMethod, o.PaymentMethodTitle = "", ""
	}
	o.Items, o.CartHash = s.gooOrderItemsFromCart(ctx, cart), gooCartHash(cart.sess.Data.Cart)
	s.gooApplyAttribution(c, o)
	o.DiscountTotal, o.DiscountTax = cart.DiscountTotal, cart.DiscountTax
	o.ShippingTotal, o.ShippingTax = cart.ShippingTotal, cart.ShippingTax
	o.CartTax = gooRound(gooSumTaxes(cart.Taxes), s.gooDecimals())
	o.TotalTax, o.Total = cart.TotalTax, cart.Total
	for _, h := range gooCheckoutHooks {
		if err := h(s, ctx, cart, o); err != nil {
			return nil, err
		}
	}
	var releases []func()
	undo := func() {
		for _, r := range releases {
			r()
		}
	}
	for _, h := range gooCheckoutReserveHooks {
		release, err := h(s, ctx, cart, o)
		if err != nil {
			undo()
			return nil, err
		}
		if release != nil {
			releases = append(releases, release)
		}
	}
	// The Store API's checkout-draft order becomes this order (it keeps its id and creation date). Its
	// items are replaced by the cart's: the cart may have changed since the draft was saved, and the
	// totals above come from the cart.
	// A draft that is no longer one was placed by another checkout of this session. Just placed (a
	// double submit read the session before the first one cleared the key): that is the order being
	// placed, not a reason to place a second one from the same cart. Placed a while ago and still unpaid
	// with the same cart (a declined card, a cancelled payment page; a concurrent request can have
	// written the old key back into the session): a retry, which reuses it like the awaiting order
	// below. A draft that is gone (the daily cleanup) is simply replaced by a new order.
	fromDraft := false
	if sess := cart.sess; sess != nil && sess.Data.Extra[gooDraftKey] != nil {
		d, err := s.gooGetOrder(ctx, gooInt(sess.Data.Extra[gooDraftKey]))
		delete(sess.Data.Extra, gooDraftKey)
		sess.touch()
		if err == nil && d.Status != "checkout-draft" {
			retry := (d.Status == "failed" || (d.Status == "pending" && time.Since(d.DateModified) > gooDraftRetryAfter)) && gooReusableOrder(d, o)
			if !retry {
				undo()
				return nil, errGooOrderInProgress
			}
			sess.Data.OrderAwaitingPayment = d.ID
			err = errGooDraftPlaced // not adopted as a draft: the awaiting-order reuse below takes it
		}
		if err == nil {
			o.ID, o.OrderKey, o.DateCreated = d.ID, d.OrderKey, d.DateCreated
			fromDraft = true
		}
	}
	// WC_Checkout::create_order(): an unpaid order from the previous attempt with the same cart is
	// reused, so a declined card does not leave one order per retry.
	reused := false
	var prevModified time.Time
	if o.ID == 0 && cart.sess != nil && cart.sess.Data.OrderAwaitingPayment != 0 {
		if prev, err := s.gooGetOrder(ctx, cart.sess.Data.OrderAwaitingPayment); err == nil && gooReusableOrder(prev, o) {
			o.ID, o.OrderKey, o.DateCreated = prev.ID, prev.OrderKey, prev.DateCreated
			prevModified = prev.DateModified
			reused = true
		}
	}
	// The reads above are not a claim: sessions are not locked, so a double submit (or a retried
	// request) runs this twice for the same draft or awaiting order, each with its own reserved gift
	// card and points. The claim is made in the save's transaction, so only one request adopts the
	// order; the other undoes its reservations instead of saving over the winner's order.
	err := pgx.BeginFunc(ctx, s.store.Pool, func(tx pgx.Tx) error {
		switch {
		case fromDraft:
			ok, err := s.gooClaimDraftTx(ctx, tx, o.ID)
			if err != nil {
				return err
			}
			if !ok {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM goo_orders WHERE id = $1)`, o.ID).Scan(&exists); err != nil {
					return err
				}
				if exists {
					return errGooOrderInProgress
				}
				// Deleted by the daily cleanup between the read and the claim: a new order replaces it.
				o.ID, o.OrderKey, o.DateCreated = 0, "", time.Time{}
			}
		case reused:
			ok, err := s.gooClaimAwaitingOrderTx(ctx, tx, o.ID, prevModified)
			if err != nil {
				return err
			}
			if !ok {
				return errGooOrderInProgress
			}
		}
		return s.gooSaveOrderTx(ctx, tx, o, o.ID == 0 || reused || fromDraft)
	})
	if err != nil {
		undo()
		return nil, err
	}
	if reused {
		_ = s.gooAddOrderNote(ctx, o, "Order updated for a new payment attempt.", false, "")
		return o, nil
	}
	_ = s.gooAddOrderNote(ctx, o, "Order created.", false, "")
	s.gooFire(ctx, "order_created", o)
	return o, nil
}

// gooOrderItemsFromCart turns the calculated cart into order items.
func (s *Server) gooOrderItemsFromCart(ctx context.Context, cart *gooCart) []*gooOrderItem {
	var items []*gooOrderItem
	key := func(m map[int64]float64) map[string]float64 {
		out := map[string]float64{}
		for k, v := range m {
			out[itoa64(k)] = v
		}
		return out
	}
	for _, it := range cart.Items {
		meta := map[string]any{}
		for k, v := range it.Variation {
			meta[strings.TrimPrefix(k, "attribute_")] = v
		}
		for k, v := range it.Data {
			meta["_goo_"+k] = v
		}
		var display []any
		for _, kv := range s.gooItemData(ctx, it) {
			display = append(display, map[string]any{"key": kv.Key, "value": kv.Value})
		}
		if len(display) > 0 {
			meta["_display"] = display
		}
		name := s.gooProductTitle(ctx, it.product)
		if it.product.Type == gooTypeVariation {
			if sum := s.gooVariationSummary(ctx, it.product); sum != "" {
				name += " - " + strings.Join(func() []string {
					var vals []string
					for _, kv := range s.gooItemData(ctx, it)[:min(len(s.gooItemData(ctx, it)), 3)] {
						vals = append(vals, kv.Value)
					}
					return vals
				}(), ", ")
			}
		}
		items = append(items, &gooOrderItem{Type: "line_item", Name: name, ProductID: it.ProductID, VariationID: it.VariationID,
			Quantity: float64(it.Quantity), TaxClass: it.TaxClass, Subtotal: gooRound(it.LineSubtotal, s.gooDecimals()), SubtotalTax: it.LineSubtotalTax,
			Total: gooRound(it.LineTotal, s.gooDecimals()), TotalTax: it.LineTax, Taxes: gooItemTaxes{Total: key(it.LineTaxes), Subtotal: key(it.LineSubtotalTaxes)}, Meta: meta})
	}
	for _, f := range cart.Fees {
		items = append(items, &gooOrderItem{Type: "fee", Name: f.Name, TaxClass: f.TaxClass, Total: f.Total, TotalTax: f.Tax,
			Taxes: gooItemTaxes{Total: key(f.Taxes)}, Meta: map[string]any{"_fee_id": f.ID}})
	}
	for i, pr := range cart.Packages {
		if pr.Chosen == nil {
			continue
		}
		r := pr.Chosen
		var names []string
		for _, it := range pr.Package.Items {
			names = append(names, fmt.Sprintf("%s &times; %d", s.gooProductTitle(ctx, it.product), it.Quantity))
		}
		meta := map[string]any{"method_id": r.MethodID, "instance_id": itoa64(r.InstanceID), "Items": strings.Join(names, ", "), "_package": float64(i)}
		for k, v := range r.Meta {
			meta[k] = v
		}
		items = append(items, &gooOrderItem{Type: "shipping", Name: r.Label, Total: r.Cost, TotalTax: gooSumTaxes(r.Taxes),
			Taxes: gooItemTaxes{Total: key(r.Taxes)}, Meta: meta})
	}
	for _, tt := range cart.TaxTotals() {
		r := s.gooTaxRateByID(ctx, tt.RateID)
		pct := 0.0
		if r != nil {
			pct = r.Rate
		}
		items = append(items, &gooOrderItem{Type: "tax", Name: tt.Code, Meta: map[string]any{"rate_id": float64(tt.RateID), "label": tt.Label,
			"compound": tt.Compound, "tax_amount": gooRound(cart.Taxes[tt.RateID], s.gooDecimals()), "shipping_tax_amount": gooRound(cart.ShippingTaxes[tt.RateID], s.gooDecimals()),
			"rate_percent": pct}})
	}
	for _, cp := range cart.Coupons() {
		items = append(items, &gooOrderItem{Type: "coupon", Name: cp.Code, Total: cart.CouponExTax[cp.Code], TotalTax: cart.CouponTax[cp.Code],
			Meta: map[string]any{"discount_amount": cart.CouponExTax[cp.Code], "discount_amount_tax": cart.CouponTax[cp.Code], "coupon_id": float64(cp.ID)}})
	}
	return items
}

// gooOrderReceivedURL is WC_Order::get_checkout_order_received_url().
func (s *Server) gooOrderReceivedURL(ctx context.Context, o *gooOrder) string {
	return s.gooEndpointURL(ctx, "checkout", s.gooOptString("checkout_order_received_endpoint"), itoa64(o.ID)) + "?key=" + o.OrderKey
}

// gooOrderPayURL is WC_Order::get_checkout_payment_url().
func (s *Server) gooOrderPayURL(ctx context.Context, o *gooOrder) string {
	return s.gooEndpointURL(ctx, "checkout", s.gooOptString("checkout_pay_endpoint"), itoa64(o.ID)) + "?pay_for_order=true&key=" + o.OrderKey
}

// Customers -------------------------------------------------------------------------------------------

// gooCreateCustomer is wc_create_new_customer(): a user with the customer role.
func (s *Server) gooCreateCustomer(ctx context.Context, email, username, password string, fields map[string]string) (int64, error) {
	email = strings.TrimSpace(email)
	if _, err := mail.ParseAddress(email); err != nil || email == "" {
		return 0, fmt.Errorf("Please provide a valid email address.")
	}
	if _, err := s.store.UserByLoginOrEmail(ctx, email); err == nil {
		return 0, fmt.Errorf(`An account is already registered with your email address. <a href="#" class="showlogin">Please log in.</a>`)
	}
	username = strings.TrimSpace(username)
	if username == "" {
		username = s.gooGenerateUsername(ctx, email, fields["billing_first_name"], fields["billing_last_name"])
	}
	// validate_username(): the same rule as the core's registration and REST users.
	if !validUsername.MatchString(username) || len(username) > 60 {
		return 0, fmt.Errorf("Please enter a valid account username.")
	}
	if loginTaken, emailTaken, err := s.store.LoginOrEmailTaken(ctx, username, username); err != nil {
		return 0, err
	} else if loginTaken || (strings.Contains(username, "@") && emailTaken) {
		return 0, fmt.Errorf("An account is already registered with that username. Please choose another.")
	}
	generated := false
	if password == "" {
		password, generated = gooRandomKey(24), true
	} else if msg := s.adminsecPasswordProblem([]string{"customer"}, username, email, password); msg != "" { // Pursae Security password policy
		return 0, fmt.Errorf("%s", msg)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return 0, err
	}
	display := strings.TrimSpace(fields["billing_first_name"] + " " + fields["billing_last_name"])
	if display == "" {
		display = username
	}
	id, err := s.store.CreateUser(ctx, store.NewUser{Login: username, Email: email, PasswordHash: hash, DisplayName: display, Role: "customer"})
	if err != nil {
		return 0, err
	}
	if u, err := s.store.UserByID(ctx, id); err == nil {
		s.pluginUserAction(ctx, "user_register", u)
		if u.Profile == nil {
			u.Profile = map[string]string{}
		}
		u.Profile["first_name"], u.Profile["last_name"] = fields["billing_first_name"], fields["billing_last_name"]
		_ = s.store.UpdateUserProfile(ctx, u)
	}
	s.gooFire(ctx, "customer_created", &gooNewCustomer{UserID: id, Email: email, Login: username, PasswordGenerated: generated})
	return id, nil
}

// gooNewCustomer is the argument of customer_created (the "new account" e-mail).
type gooNewCustomer struct {
	UserID            int64
	Email, Login      string
	PasswordGenerated bool
}

// gooGenerateUsername is wc_create_new_customer_username().
func (s *Server) gooGenerateUsername(ctx context.Context, email, first, last string) string {
	base := wp.SanitizeTitle(strings.TrimSpace(first + last))
	if base == "" {
		base, _, _ = strings.Cut(email, "@")
		base = wp.SanitizeTitle(base)
	}
	// sanitize_user( $username, true ): logins stay ASCII even when the slug keeps other scripts.
	base = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return -1
	}, base)
	if base == "" {
		base = "customer"
	}
	name := base
	for i := 1; i < 1000; i++ {
		if _, err := s.store.UserByLoginOrEmail(ctx, name); err != nil {
			return name
		}
		name = fmt.Sprintf("%s%d", base, i)
	}
	return base + gooRandomKey(6)
}

// gooLoginCustomer logs a new customer in (wc_set_customer_auth_cookie()).
func (s *Server) gooLoginCustomer(c *gin.Context, userID int64) error {
	return s.loginUserCookie(c, userID, false)
}

// gooSaveCustomerAddresses stores the order's addresses as the customer's (billing_* / shipping_* meta).
func (s *Server) gooSaveCustomerAddresses(ctx context.Context, userID int64, o *gooOrder) {
	meta := map[string]any{"_goo_last_order": float64(o.ID), "_goo_last_active": float64(time.Now().Unix())}
	for _, k := range gooAddressKeys {
		if v := o.Billing[k]; v != "" {
			meta["billing_"+k] = v
		}
		if v := o.Shipping[k]; v != "" {
			meta["shipping_"+k] = v
		}
	}
	_ = s.gooSetUserMeta(ctx, userID, meta)
}

// gooSetUserMeta merges keys into a user's meta (update_user_meta for several keys).
func (s *Server) gooSetUserMeta(ctx context.Context, userID int64, meta map[string]any) error {
	raw, _ := json.Marshal(meta)
	_, err := s.store.Pool.Exec(ctx, `UPDATE users SET meta = coalesce(meta, '{}'::jsonb) || $2::jsonb WHERE id = $1`, userID, raw)
	return err
}

// gooLoginCustomer logs a customer in (wc_set_customer_auth_cookie()).
func (s *Server) loginUserCookie(c *gin.Context, userID int64, remember bool) error {
	sess, err := s.auth.Create(c, userID, remember, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		return err
	}
	s.setLoginCookie(c, sess.Token, remember, int(auth.SessionRemember.Seconds()))
	return nil
}

// gooReusableOrder reports whether prev (the session's order awaiting payment) can take the new
// attempt: still unpaid, same cart contents and same customer.
func gooReusableOrder(prev, next *gooOrder) bool {
	return (prev.Status == "pending" || prev.Status == "failed") && prev.CartHash != "" &&
		prev.CartHash == next.CartHash && prev.CustomerID == next.CustomerID
}
