package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/store"
)

// Stripe (gateway id "goo_stripe"). The customer pays on Stripe's hosted Checkout page, so card data
// never reaches this server: the order keeps a PaymentIntent id, saved cards keep a PaymentMethod id.
//
//	checkout        ProcessPayment creates a Checkout Session and redirects to it (a saved card is
//	                charged with a PaymentIntent instead, with a redirect only when the bank asks for 3-D Secure)
//	return          /wc-api/goo_stripe/?order_id=…&key=…&session_id=… reads the session back and completes the order
//	webhook         POST /wc-api/goo_stripe/ (Stripe-Signature verified) does the same without the customer,
//	                and records refunds and disputes made in the Stripe Dashboard
//	refunds         the admin refund goes through /v1/refunds
//	subscriptions   the card is saved off-session at checkout; ProcessRenewal charges it
//
// The API is called with net/http (form-encoded, as Stripe's API takes it); no SDK.

const gooStripeID = "goo_stripe"

// gooStripeAPIBase is Stripe's API origin (the tests point it to a fake server).
var gooStripeAPIBase = "https://api.stripe.com"

// gooStripeAPIVersion pins the response shapes this code reads.
const gooStripeAPIVersion = "2024-06-20"

var gooStripeClient = &http.Client{Timeout: 25 * time.Second}

// gooStripeTolerance is how old a webhook's signed timestamp may be (Stripe's own default).
const gooStripeTolerance = 5 * time.Minute

type gooStripeGateway struct{}

func init() {
	gooRegisterGateway(gooStripeGateway{})
	gooOn("order_status_changed", 10, func(s *Server, ctx context.Context, arg any) {
		if ch, ok := arg.(*gooStatusChange); ok && ch.To == "cancelled" {
			s.gooStripeExpireSession(ctx, ch.Order)
		}
	})
}

// gooStripeSessionTTL is how long a hosted Checkout page stays open: the store's hold-stock time,
// clamped to what Stripe accepts (30 minutes to 24 hours, with a margin for clock skew).
func (s *Server) gooStripeSessionTTL() time.Duration {
	mins := s.gooOptInt("hold_stock_minutes")
	if mins <= 0 {
		mins = 60
	}
	return time.Duration(min(max(mins, 31), 24*60-5)) * time.Minute
}

// gooStripeMaxSessions bounds the Checkout sessions remembered per order (one per pay-for-order attempt).
const gooStripeMaxSessions = 10

// gooStripeSessionIDs are the order's Checkout sessions that may still be open: the remembered list
// and the latest one.
func gooStripeSessionIDs(o *gooOrder) []string {
	var out []string
	add := func(id string) {
		if id != "" && !strings.ContainsAny(id, "/?#") && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if list, ok := o.Meta["_stripe_session_ids"].([]any); ok {
		for _, v := range list {
			if id, ok := v.(string); ok {
				add(id)
			}
		}
	} else if list, ok := o.Meta["_stripe_session_ids"].([]string); ok {
		for _, id := range list {
			add(id)
		}
	}
	add(gxaStr(o.Meta, "_stripe_session_id"))
	return out
}

// gooStripeRememberSession records a new Checkout session on the order, keeping the newest few.
func gooStripeRememberSession(o *gooOrder, id string) {
	ids := append(gooStripeSessionIDs(o), id)
	if len(ids) > gooStripeMaxSessions {
		ids = ids[len(ids)-gooStripeMaxSessions:]
	}
	o.Meta["_stripe_session_ids"] = ids
	o.Meta["_stripe_session_id"] = id
}

// gooStripeExpireSession closes the open Checkout pages of a cancelled order (every pay-for-order
// attempt's, not only the last), so none can still be paid. Best effort and in the background: the
// cancellation never waits on or fails with Stripe.
func (s *Server) gooStripeExpireSession(ctx context.Context, o *gooOrder) {
	if o == nil || o.PaymentMethod != gooStripeID || s.gooStripeSecret() == "" {
		return
	}
	ids := gooStripeSessionIDs(o)
	if len(ids) == 0 {
		return
	}
	bg := context.WithoutCancel(ctx)
	safeGo("stripe-expire-session", func() {
		for _, sid := range ids {
			ctx, cancel := context.WithTimeout(bg, 10*time.Second)
			if _, err := s.gooStripeCall(ctx, http.MethodPost, "/v1/checkout/sessions/"+sid+"/expire", url.Values{}, ""); err != nil {
				log.Printf("stripe: expiring a checkout session of cancelled order %d: %q", o.ID, err.Error()) //nolint:gosec // G706: a numeric order id and Stripe's error, quoted by %q (no raw newlines)
			}
			cancel()
		}
	})
}

func (gooStripeGateway) ID() string          { return gooStripeID }
func (gooStripeGateway) MethodTitle() string { return "Stripe" }
func (gooStripeGateway) MethodDescription() string {
	return "Take card payments (and the other methods enabled in your Stripe account) on Stripe's hosted payment page. Card data never touches your site. Supports refunds, saved cards and automatic subscription renewals."
}
func (gooStripeGateway) FormFields() []gooField {
	f := gooGatewayBaseFields("Credit / debit card", "Pay securely with your card. You will be redirected to Stripe to complete the payment.")
	f[0].Desc = "Enable Stripe"
	return append(f[:3],
		gooField{ID: "testmode", Title: "Test mode", Type: "checkbox", Default: "yes", Desc: "Enable test mode", Tip: "Test mode uses the test API keys; no real money is charged."},
		gooField{ID: "test_secret_key", Title: "Test secret key", Type: "password", Holder: "sk_test_…", Desc: "Your Stripe test secret key (or a restricted key). A saved key is not shown again; leave the field empty to keep it."},
		gooField{ID: "test_webhook_secret", Title: "Test webhook secret", Type: "password", Holder: "whsec_…"},
		gooField{ID: "secret_key", Title: "Live secret key", Type: "password", Holder: "sk_live_…", Desc: "Your Stripe live secret key (or a restricted key)."},
		gooField{ID: "webhook_secret", Title: "Live webhook secret", Type: "password", Holder: "whsec_…"},
		gooField{ID: "saved_cards", Title: "Saved cards", Type: "checkbox", Default: "yes", Desc: "Enable payment via saved cards", Tip: "Customers can save a card at checkout and pay with it later. Card details are stored by Stripe, not by your store."},
	)
}

// PatchFields adds the webhook endpoint of this site to the webhook secret fields.
func (gooStripeGateway) PatchFields(s *Server, ctx context.Context, fields []gooField) []gooField {
	hint := `Add the endpoint <code>` + gooEsc(s.gooWCAPIURL(ctx, gooStripeID)) + `</code> under Developers &rsaquo; Webhooks in Stripe ` +
		`(events: <code>checkout.session.completed</code>, <code>checkout.session.async_payment_succeeded</code>, <code>checkout.session.async_payment_failed</code>, ` +
		`<code>payment_intent.succeeded</code>, <code>payment_intent.payment_failed</code>, <code>charge.refunded</code>, <code>charge.dispute.created</code>) and paste its signing secret here.`
	for i := range fields {
		if fields[i].ID == "test_webhook_secret" || fields[i].ID == "webhook_secret" {
			fields[i].Desc = hint
		}
	}
	return fields
}

func (gooStripeGateway) Supports(f string) bool {
	switch f {
	case "products", "refunds", "subscriptions", "tokenization":
		return true
	}
	return false
}

func (gooStripeGateway) RenewalNeedsToken() bool { return true }

// Settings -------------------------------------------------------------------------------------------

func (s *Server) gooStripeTest() bool { return s.gooGatewaySetting(gooStripeID, "testmode") == "yes" }

func (s *Server) gooStripeSecret() string {
	if s.gooStripeTest() {
		return strings.TrimSpace(s.gooGatewaySetting(gooStripeID, "test_secret_key"))
	}
	return strings.TrimSpace(s.gooGatewaySetting(gooStripeID, "secret_key"))
}

func (s *Server) gooStripeWebhookSecret() string {
	if s.gooStripeTest() {
		return strings.TrimSpace(s.gooGatewaySetting(gooStripeID, "test_webhook_secret"))
	}
	return strings.TrimSpace(s.gooGatewaySetting(gooStripeID, "webhook_secret"))
}

// SetupProblem is "" when the secret key of the current mode is saved.
func (gooStripeGateway) SetupProblem(s *Server) string {
	if s.gooStripeSecret() != "" {
		return ""
	}
	if s.gooStripeTest() {
		return "The test secret key is missing."
	}
	return "The live secret key is missing."
}

func (gooStripeGateway) TokenMode(s *Server) string {
	if s.gooStripeTest() {
		return "test"
	}
	return "live"
}

// gooStripeCustomerKey is the user meta key of the Stripe customer id (one per mode: test customers do
// not exist in live mode).
func (s *Server) gooStripeCustomerKey() string {
	if s.gooStripeTest() {
		return "_goo_stripe_customer_id_test"
	}
	return "_goo_stripe_customer_id"
}

// API client -----------------------------------------------------------------------------------------

// gooStripeError is an error response of the Stripe API.
type gooStripeError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *gooStripeError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("Stripe returned HTTP %d", e.Status)
}

// gooStripeCall is one API request. form is sent form-encoded (POST) or as the query (GET); idem is
// an Idempotency-Key ("" for none). A network failure is returned as is: the outcome is then unknown.
func (s *Server) gooStripeCall(ctx context.Context, method, path string, form url.Values, idem string) (map[string]any, error) {
	key := s.gooStripeSecret()
	if key == "" {
		return nil, errors.New("Stripe is not configured: the secret key is missing.")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	target := gooStripeAPIBase + path
	var body io.Reader
	if method == http.MethodGet {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Stripe-Version", gooStripeAPIVersion)
	req.Header.Set("User-Agent", "Pursae-Commerce")
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
	}
	resp, err := gooStripeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach Stripe: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("could not read Stripe's response: %w", err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode >= 300 {
		e := &gooStripeError{Status: resp.StatusCode}
		if m := gooJSONMap(out["error"]); m != nil {
			e.Type, e.Code, e.Message = gxaStr(m, "type"), gxaStr(m, "code"), gxaStr(m, "message")
		}
		return out, e
	}
	if out == nil {
		return nil, errors.New("Stripe returned an unreadable response")
	}
	return out, nil
}

func gooJSONMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// gooStripeRef is the id of an API field that is either an id or an expanded object.
func gooStripeRef(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		return gxaStr(t, "id")
	}
	return ""
}

// Stripe ids go into URL paths: only the characters Stripe uses are let through.
func gooStripeIDOK(id string) bool {
	if id == "" || len(id) > 255 {
		return false
	}
	for _, r := range id {
		if !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

var gooStripeZeroDecimal = map[string]bool{"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true, "kmf": true, "krw": true, "mga": true,
	"pyg": true, "rwf": true, "ugx": true, "vnd": true, "vuv": true, "xaf": true, "xof": true, "xpf": true}
var gooStripeThreeDecimal = map[string]bool{"bhd": true, "jod": true, "kwd": true, "omr": true, "tnd": true}

// gooStripeAmount is an amount in the currency's smallest unit, as Stripe takes it.
func gooStripeAmount(amount float64, currency string) int64 {
	cur := strings.ToLower(currency)
	switch {
	case gooStripeZeroDecimal[cur]:
		return int64(math.Round(amount))
	case gooStripeThreeDecimal[cur]:
		return int64(math.Round(amount*100)) * 10 // the last digit must be 0
	}
	return int64(math.Round(amount * 100))
}

// gooStripeFromAmount is the reverse of gooStripeAmount.
func gooStripeFromAmount(minor float64, currency string) float64 {
	cur := strings.ToLower(currency)
	switch {
	case gooStripeZeroDecimal[cur]:
		return minor
	case gooStripeThreeDecimal[cur]:
		return minor / 1000
	}
	return minor / 100
}

// gooStripeCustomer is the Stripe customer of an order's customer, created on first use. A guest gets
// a customer only when the card must be kept (create).
func (s *Server) gooStripeCustomer(ctx context.Context, userID int64, email, name string, create bool) (string, error) {
	metaKey := s.gooStripeCustomerKey()
	if userID != 0 {
		if id := s.gooUserMetaString(ctx, userID, metaKey); id != "" {
			return id, nil
		}
	} else if !create {
		return "", nil
	}
	form := url.Values{}
	if email != "" {
		form.Set("email", email)
	}
	if name != "" {
		form.Set("name", name)
	}
	if userID != 0 {
		form.Set("metadata[user_id]", itoa64(userID))
	}
	form.Set("metadata[site]", s.links(ctx).HomeURL("/"))
	res, err := s.gooStripeCall(ctx, http.MethodPost, "/v1/customers", form, "")
	if err != nil {
		return "", err
	}
	id := gxaStr(res, "id")
	if id == "" {
		return "", errors.New("Stripe did not return a customer")
	}
	if userID != 0 {
		_ = s.gooSetUserMeta(ctx, userID, map[string]any{metaKey: id})
	}
	return id, nil
}

// gooStripeOrderHasSubscription reports whether paying the order must keep the card for renewals.
func gooStripeOrderHasSubscription(o *gooOrder) bool {
	if gxaInt64(o.Meta, "_subscription_renewal") > 0 {
		return true
	}
	for _, it := range o.ItemsOf("line_item") {
		if it.Meta["_subscription_period"] != nil {
			return true
		}
	}
	return false
}

// Checkout -------------------------------------------------------------------------------------------

func (gooStripeGateway) PaymentFields(s *Server, c *gin.Context) string {
	var b strings.Builder
	if d := s.gooT(c, s.gooGatewaySetting(gooStripeID, "description")); d != "" {
		b.WriteString(`<p>` + gooEsc(d) + `</p>`)
	}
	if s.gooStripeTest() {
		b.WriteString(`<p class="goo-stripe-testmode"><strong>TEST MODE</strong> &ndash; no real payment is taken. Use the card 4242 4242 4242 4242 with any future date and CVC on the next page.</p>`)
	}
	if s.gooGatewaySetting(gooStripeID, "saved_cards") == "yes" && currentUser(c) != nil {
		b.WriteString(s.gooSavedTokenFieldsHTML(c, gooStripeID))
		b.WriteString(`<p class="form-row woocommerce-SavedPaymentMethods-saveNew"><input id="wc-goo_stripe-new-payment-method" name="wc-goo_stripe-new-payment-method" type="checkbox" value="true" style="width:auto;"> ` +
			`<label for="wc-goo_stripe-new-payment-method" style="display:inline;">Save payment information to my account for future purchases.</label></p>`)
	}
	return b.String()
}

func (gooStripeGateway) Validate(s *Server, c *gin.Context) error {
	if v := c.PostForm("wc-goo_stripe-payment-token"); v != "" && v != "new" {
		u := currentUser(c)
		if u == nil || s.gooUserToken(c.Request.Context(), u.ID, gooAtoi64(v)) == nil {
			return errors.New("Invalid payment method. Please choose another one.")
		}
	}
	return nil
}

func (gooStripeGateway) Available(s *Server, c *gin.Context, cart *gooCart) bool {
	return s.gooStripeSecret() != ""
}

func (gooStripeGateway) ThankYou(s *Server, ctx context.Context, o *gooOrder) string {
	if o.Status == "on-hold" {
		return `<p>Your payment is being processed. We will e-mail you as soon as it is confirmed.</p>`
	}
	return ""
}

// gooStripeReturnURL is where Stripe sends the customer back to.
func (s *Server) gooStripeReturnURL(ctx context.Context, o *gooOrder) string {
	return s.gooWCAPIURL(ctx, gooStripeID) + "?order_id=" + itoa64(o.ID) + "&key=" + url.QueryEscape(o.OrderKey)
}

func gooStripeOrderMeta(form url.Values, prefix string, s *Server, ctx context.Context, o *gooOrder) {
	form.Set(prefix+"[order_id]", itoa64(o.ID))
	form.Set(prefix+"[order_key]", o.OrderKey)
	form.Set(prefix+"[site]", s.links(ctx).HomeURL("/"))
}

func (g gooStripeGateway) ProcessPayment(s *Server, c *gin.Context, o *gooOrder) (gooPaymentResult, error) {
	ctx := c.Request.Context()
	fail := func(err error) (gooPaymentResult, error) {
		log.Printf("goocommerce: stripe: order %d: %v", o.ID, err)
		var se *gooStripeError
		msg := "The payment could not be started. Please try again or choose another payment method."
		if errors.As(err, &se) && se.Type == "card_error" && se.Message != "" {
			msg = se.Message
		}
		return gooPaymentResult{Result: "failure", Message: msg}, errors.New(msg)
	}
	if len(o.Items) == 0 {
		o.Items, _ = s.gooOrderItems(ctx, o.ID)
	}
	u := currentUser(c)
	keep := gooStripeOrderHasSubscription(o)
	saveCards := s.gooGatewaySetting(gooStripeID, "saved_cards") == "yes"
	if !keep && saveCards && u != nil && o.CustomerID == u.ID && c.PostForm("wc-goo_stripe-new-payment-method") == "true" {
		keep = true
	}
	name := strings.TrimSpace(o.Billing["first_name"] + " " + o.Billing["last_name"])
	customer, err := s.gooStripeCustomer(ctx, o.CustomerID, o.Billing["email"], name, keep)
	if err != nil {
		return fail(err)
	}
	amount := strconv.FormatInt(gooStripeAmount(o.Total, o.Currency), 10)
	currency := strings.ToLower(o.Currency)
	desc := fmt.Sprintf("Order #%s", o.Number())

	// A saved card: charge it now; the bank may still ask the customer to authenticate.
	if v := c.PostForm("wc-goo_stripe-payment-token"); v != "" && v != "new" && u != nil && o.CustomerID == u.ID {
		t := s.gooUserToken(ctx, u.ID, gooAtoi64(v))
		if t == nil || t.GatewayID != gooStripeID || t.Mode != g.TokenMode(s) {
			return fail(errors.New("unknown saved payment method"))
		}
		form := url.Values{"amount": {amount}, "currency": {currency}, "payment_method": {t.Token}, "confirm": {"true"},
			"description": {desc}, "return_url": {s.gooStripeReturnURL(ctx, o)}}
		form.Set("customer", defaultStr(t.CustomerRef, customer))
		form.Set("payment_method_types[0]", "card")
		if keep {
			form.Set("setup_future_usage", "off_session")
		}
		gooStripeOrderMeta(form, "metadata", s, ctx, o)
		pi, err := s.gooStripeCall(ctx, http.MethodPost, "/v1/payment_intents", form, "")
		if err != nil {
			var se *gooStripeError
			if errors.As(err, &se) && se.Type == "card_error" {
				_ = s.gooSetStatusFrom(ctx, o, []string{"pending"}, "failed", "Stripe: "+gooEsc(se.Message), "")
			}
			return fail(err)
		}
		if gxaStr(pi, "status") == "requires_action" {
			if next := gxaStr(gooJSONMap(gooJSONMap(pi["next_action"])["redirect_to_url"]), "url"); next != "" {
				o.Meta["_stripe_intent_id"] = gxaStr(pi, "id")
				_ = s.gooSaveOrder(ctx, o, false)
				return gooPaymentResult{Result: "success", Redirect: next}, nil
			}
			return fail(errors.New("the saved card needs an authentication step that could not be started"))
		}
		if err := s.gooStripeApplyIntent(ctx, o, pi); err != nil {
			return fail(err)
		}
		if !o.IsPaid() && o.Status != "on-hold" {
			return fail(errors.New("the saved card was not charged: " + gxaStr(pi, "status")))
		}
		s.gooCartFor(c).Empty()
		return gooPaymentResult{Result: "success", Redirect: s.gooOrderReceivedURL(ctx, o)}, nil
	}

	// A new payment method: Stripe's hosted Checkout page.
	form := url.Values{"mode": {"payment"}, "client_reference_id": {itoa64(o.ID)},
		"success_url": {s.gooStripeReturnURL(ctx, o) + "&session_id={CHECKOUT_SESSION_ID}"},
		"cancel_url":  {s.gooOrderPayURL(ctx, o)}}
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", currency)
	form.Set("line_items[0][price_data][unit_amount]", amount)
	form.Set("line_items[0][price_data][product_data][name]", desc)
	form.Set("payment_intent_data[description]", desc)
	// The page closes when the unpaid order would be cancelled (the hold-stock time, within Stripe's
	// 30 minutes to 24 hours), so it cannot pay an order that is long gone.
	form.Set("expires_at", strconv.FormatInt(time.Now().Add(s.gooStripeSessionTTL()).Unix(), 10))
	gooStripeOrderMeta(form, "metadata", s, ctx, o)
	gooStripeOrderMeta(form, "payment_intent_data[metadata]", s, ctx, o)
	if customer != "" {
		form.Set("customer", customer)
	} else if o.Billing["email"] != "" {
		form.Set("customer_email", o.Billing["email"])
	}
	if keep {
		form.Set("payment_intent_data[setup_future_usage]", "off_session")
		form.Set("payment_method_types[0]", "card") // only cards renew off-session here
	}
	sess, err := s.gooStripeCall(ctx, http.MethodPost, "/v1/checkout/sessions", form, "")
	if err != nil {
		return fail(err)
	}
	target := gxaStr(sess, "url")
	if target == "" {
		return fail(errors.New("Stripe did not return a payment page"))
	}
	gooStripeRememberSession(o, gxaStr(sess, "id"))
	if customer != "" {
		o.Meta["_stripe_customer_id"] = customer
	}
	if keep && !gooStripeOrderHasSubscription(o) {
		o.Meta["_stripe_save_card"] = true
	}
	_ = s.gooSaveOrder(ctx, o, false)
	return gooPaymentResult{Result: "success", Redirect: target}, nil
}

// gooStripeApplyIntent brings an order in line with its PaymentIntent. It is called from the return
// URL, from the webhook and after an immediate charge, in any order and any number of times: a paid
// order is left alone, and the status changes go through the order's lock.
func (s *Server) gooStripeApplyIntent(ctx context.Context, o *gooOrder, pi map[string]any) error {
	id := gxaStr(pi, "id")
	if o.PaymentMethod != gooStripeID {
		return fmt.Errorf("order %d was not paid with Stripe", o.ID)
	}
	if o.IsPaid() || o.Status == "refunded" {
		return nil
	}
	switch gxaStr(pi, "status") {
	case "succeeded":
		want, cur := gooStripeAmount(o.Total, o.Currency), strings.ToLower(o.Currency)
		got := int64(gxaFloat(pi, "amount_received"))
		if got == 0 {
			got = int64(gxaFloat(pi, "amount"))
		}
		if got != want || !strings.EqualFold(gxaStr(pi, "currency"), cur) {
			// WooCommerce's PayPal check: a payment that does not match the order is not accepted as paid.
			o.TransactionID = id
			note := fmt.Sprintf("Stripe payment %s does not match the order total (received %s %s). Review the payment before fulfilling the order.",
				gooEsc(id), strconv.FormatFloat(gooStripeFromAmount(float64(got), gxaStr(pi, "currency")), 'f', -1, 64), gooEsc(strings.ToUpper(gxaStr(pi, "currency"))))
			if err := s.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "on-hold", note, ""); err != nil && !errors.Is(err, errGooStatusChanged) {
				return err
			}
			return nil
		}
		s.gooStripeKeepCard(ctx, o, pi)
		o.Meta["_stripe_intent_id"] = id
		if err := s.gooSaveOrder(ctx, o, false); err != nil {
			return err
		}
		if err := s.gooPaymentComplete(ctx, o, id); err != nil {
			return err
		}
		if o.IsPaid() && o.TransactionID == id {
			_ = s.gooAddOrderNote(ctx, o, "Stripe payment complete (PaymentIntent "+gooEsc(id)+").", false, "")
			s.gooStripeRenewalPaid(ctx, o)
		}
	case "processing":
		o.TransactionID = id
		if err := s.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "on-hold", "Stripe payment is processing (PaymentIntent "+gooEsc(id)+").", ""); err != nil && !errors.Is(err, errGooStatusChanged) {
			return err
		}
	case "requires_payment_method", "canceled":
		msg := gxaStr(gooJSONMap(pi["last_payment_error"]), "message")
		if msg == "" && gxaStr(pi, "status") == "requires_payment_method" {
			return nil // nothing was attempted yet
		}
		if err := s.gooSetStatusFrom(ctx, o, []string{"pending", "on-hold"}, "failed", "Stripe payment failed. "+gooEsc(msg), ""); err != nil && !errors.Is(err, errGooStatusChanged) {
			return err
		}
	}
	return nil
}

// gooStripeKeepCard writes the payment method of a PaymentIntent that was set up for later use onto
// the order ("_payment_token", copied to the subscription) and into the customer's saved cards.
func (s *Server) gooStripeKeepCard(ctx context.Context, o *gooOrder, pi map[string]any) {
	pmID, customer := gooStripeRef(pi["payment_method"]), gooStripeRef(pi["customer"])
	if gxaStr(pi, "setup_future_usage") != "off_session" || pmID == "" || customer == "" {
		return
	}
	o.Meta["_payment_token"] = pmID
	o.Meta["_stripe_customer_id"] = customer
	if o.CustomerID == 0 {
		return
	}
	pm := gooJSONMap(pi["payment_method"])
	if pm == nil && gooStripeIDOK(pmID) {
		pm, _ = s.gooStripeCall(ctx, http.MethodGet, "/v1/payment_methods/"+pmID, nil, "")
	}
	s.gooStripeSaveCard(ctx, o.CustomerID, customer, pmID, pm)
}

func (s *Server) gooStripeSaveCard(ctx context.Context, userID int64, customer, pmID string, pm map[string]any) {
	card := gooJSONMap(pm["card"])
	t := &gooPaymentToken{UserID: userID, GatewayID: gooStripeID, Token: pmID, CustomerRef: customer, Mode: gooStripeGateway{}.TokenMode(s),
		Brand: gxaStr(card, "brand"), Last4: gxaStr(card, "last4"), ExpMonth: int(gxaFloat(card, "exp_month")), ExpYear: int(gxaFloat(card, "exp_year"))}
	if err := s.gooSaveToken(ctx, t); err != nil {
		log.Printf("goocommerce: stripe: save card: %v", err)
	}
}

// gooStripeRenewalPaid: a renewal order paid by the customer with a new card moves the subscription
// to that card, so the next renewal is automatic again.
func (s *Server) gooStripeRenewalPaid(ctx context.Context, o *gooOrder) {
	subID, tok := gxaInt64(o.Meta, "_subscription_renewal"), gxaStr(o.Meta, "_payment_token")
	if subID == 0 || tok == "" {
		return
	}
	sub, err := s.gsubGet(ctx, subID)
	if err != nil || gxaStr(sub.Meta, "_payment_token") == tok && sub.PaymentMethod == gooStripeID {
		return
	}
	gw := s.gooGateway(gooStripeID)
	sub.PaymentMethod, sub.PaymentMethodTitle = gooStripeID, gw.Title()
	sub.Meta["_payment_token"] = tok
	sub.Meta["_stripe_customer_id"] = gxaStr(o.Meta, "_stripe_customer_id")
	delete(sub.Meta, "_requires_manual_renewal")
	_ = s.gsubSave(ctx, sub)
}

// Callbacks ------------------------------------------------------------------------------------------

// HandleAPI is /wc-api/goo_stripe/: Stripe's webhook (POST) and the customer's return (GET).
func (g gooStripeGateway) HandleAPI(s *Server, c *gin.Context) {
	if c.Request.Method == http.MethodPost {
		s.gooStripeWebhook(c)
		return
	}
	q := c.Request.URL.Query()
	if q.Get("setup") != "" {
		s.gooStripeSetupReturn(c)
		return
	}
	s.gooStripeReturn(c)
}

// gooStripeReturn completes the order when the customer comes back from Stripe. The session or
// PaymentIntent named in the URL is read from Stripe with the secret key and must belong to the order.
func (s *Server) gooStripeReturn(c *gin.Context) {
	ctx := c.Request.Context()
	q := c.Request.URL.Query()
	o, err := s.gooGetOrder(ctx, gooAtoi64(q.Get("order_id")))
	if err != nil || o.Type != "shop_order" || o.OrderKey == "" || subtle.ConstantTimeCompare([]byte(q.Get("key")), []byte(o.OrderKey)) != 1 ||
		o.PaymentMethod != gooStripeID {
		s.gooAddNotice(c, "error", "Sorry, this order is invalid and cannot be paid for.")
		s.gooRedirect(c, s.gooPageURL(ctx, "checkout"))
		return
	}
	var pi map[string]any
	switch sid, pid := q.Get("session_id"), q.Get("payment_intent"); {
	case gooStripeIDOK(sid):
		sess, err := s.gooStripeCall(ctx, http.MethodGet, "/v1/checkout/sessions/"+sid, url.Values{"expand[]": {"payment_intent.payment_method"}}, "")
		if err == nil && gxaStr(sess, "client_reference_id") == itoa64(o.ID) {
			pi = gooJSONMap(sess["payment_intent"])
		}
	case gooStripeIDOK(pid):
		res, err := s.gooStripeCall(ctx, http.MethodGet, "/v1/payment_intents/"+pid, url.Values{"expand[]": {"payment_method"}}, "")
		if err == nil && gooStripeIntentIsFor(res, o) {
			pi = res
		}
	}
	if pi != nil {
		if err := s.gooStripeApplyIntent(ctx, o, pi); err != nil {
			log.Printf("goocommerce: stripe: return for order %d: %v", o.ID, err)
		}
	}
	if o.IsPaid() || o.Status == "on-hold" {
		s.gooCartFor(c).Empty()
		s.gooRedirect(c, s.gooOrderReceivedURL(ctx, o))
		return
	}
	s.gooAddNotice(c, "error", "The payment was not completed. Please try again.")
	s.gooRedirect(c, s.gooOrderPayURL(ctx, o))
}

// gooStripeIntentIsFor reports whether a PaymentIntent was created for the order (by this site).
func gooStripeIntentIsFor(pi map[string]any, o *gooOrder) bool {
	md := gooJSONMap(pi["metadata"])
	return md != nil && gxaStr(md, "order_id") == itoa64(o.ID) && o.OrderKey != "" &&
		subtle.ConstantTimeCompare([]byte(gxaStr(md, "order_key")), []byte(o.OrderKey)) == 1
}

// gooStripeVerifySignature checks a Stripe-Signature header ("t=<unix>,v1=<hex hmac>[,v1=…]"): the
// HMAC-SHA256 of "<t>.<body>" with the endpoint's signing secret, and a timestamp within the tolerance
// (a replayed old delivery is refused).
func gooStripeVerifySignature(body []byte, header, secret string, now time.Time, tolerance time.Duration) error {
	if secret == "" {
		return errors.New("webhook secret is not configured")
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == "" || len(sigs) == 0 {
		return errors.New("malformed Stripe-Signature header")
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("malformed Stripe-Signature timestamp")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	want := mac.Sum(nil)
	match := false
	for _, sig := range sigs {
		got, err := hex.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			match = true
		}
	}
	if !match {
		return errors.New("signature mismatch")
	}
	if d := now.Sub(time.Unix(unix, 0)); d > tolerance || d < -tolerance {
		return errors.New("timestamp outside the tolerance")
	}
	return nil
}

// gooStripeWebhook is the webhook endpoint. Unsigned or badly signed requests get 400 and change
// nothing; an event id is handled once; a failure answers 500 so Stripe delivers the event again.
func (s *Server) gooStripeWebhook(c *gin.Context) {
	ctx := c.Request.Context()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		c.String(http.StatusBadRequest, "invalid payload")
		return
	}
	if err := gooStripeVerifySignature(body, c.GetHeader("Stripe-Signature"), s.gooStripeWebhookSecret(), time.Now(), gooStripeTolerance); err != nil {
		c.String(http.StatusBadRequest, "invalid signature")
		return
	}
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object map[string]any `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" || ev.Data.Object == nil {
		c.String(http.StatusBadRequest, "invalid payload")
		return
	}
	fresh, err := s.gooGatewayEventClaim(ctx, gooStripeID, ev.ID, ev.Type)
	if err != nil {
		c.String(http.StatusInternalServerError, "error")
		return
	}
	if !fresh {
		c.JSON(http.StatusOK, gin.H{"received": true, "duplicate": true})
		return
	}
	if err := s.gooStripeEvent(ctx, ev.Type, ev.Data.Object); err != nil {
		s.gooGatewayEventRelease(ctx, gooStripeID, ev.ID)
		log.Printf("goocommerce: stripe: webhook %s (%s): %v", ev.ID, ev.Type, err)
		c.String(http.StatusInternalServerError, "error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"received": true})
}

// gooNotFound reports whether a lookup failed only because the row does not exist (as opposed to
// the database being unreachable), which is the one failure an event may be acknowledged after.
func gooNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, store.ErrNotFound)
}

// gooStripeEventOrder is the order an event's object names in its metadata; nil when the object is
// not about an order of this site (another site on the same Stripe account). A database error is
// returned, so the event is not acknowledged and Stripe sends it again.
func (s *Server) gooStripeEventOrder(ctx context.Context, obj map[string]any) (*gooOrder, error) {
	md := gooJSONMap(obj["metadata"])
	id := gooAtoi64(gxaStr(md, "order_id"))
	if id == 0 {
		return nil, nil
	}
	o, err := s.gooGetOrder(ctx, id)
	if err != nil {
		if gooNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if o.Type != "shop_order" || o.PaymentMethod != gooStripeID || !gooStripeIntentIsFor(obj, o) {
		return nil, nil
	}
	return o, nil
}

// gooStripeEventOrderByIntent finds the order a charge event's payment intent paid; nil when no
// order of this site did. Like gooStripeEventOrder, only a database error is returned.
func (s *Server) gooStripeEventOrderByIntent(ctx context.Context, obj map[string]any) (*gooOrder, error) {
	ref := gooStripeRef(obj["payment_intent"])
	if ref == "" {
		return nil, nil
	}
	o, err := s.gooOrderByTransaction(ctx, gooStripeID, ref)
	if err != nil {
		if gooNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return o, nil
}

func (s *Server) gooStripeEvent(ctx context.Context, typ string, obj map[string]any) error {
	switch typ {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded", "checkout.session.async_payment_failed":
		if gxaStr(obj, "mode") == "setup" {
			if typ != "checkout.session.completed" {
				return nil
			}
			_, err := s.gooStripeApplySetup(ctx, obj, 0)
			return err
		}
		o, err := s.gooStripeEventOrder(ctx, obj)
		if err != nil {
			return err
		}
		pid := gooStripeRef(obj["payment_intent"])
		if o == nil || !gooStripeIDOK(pid) {
			return nil
		}
		pi, err := s.gooStripeCall(ctx, http.MethodGet, "/v1/payment_intents/"+pid, url.Values{"expand[]": {"payment_method"}}, "")
		if err != nil {
			return err
		}
		if !gooStripeIntentIsFor(pi, o) {
			return nil
		}
		return s.gooStripeApplyIntent(ctx, o, pi)
	case "payment_intent.succeeded", "payment_intent.payment_failed", "payment_intent.processing", "payment_intent.canceled":
		o, err := s.gooStripeEventOrder(ctx, obj)
		if err != nil || o == nil {
			return err
		}
		return s.gooStripeApplyIntent(ctx, o, obj)
	case "charge.refunded":
		o, err := s.gooStripeEventOrder(ctx, obj)
		if err != nil {
			return err
		}
		if o == nil {
			if o, err = s.gooStripeEventOrderByIntent(ctx, obj); err != nil || o == nil {
				return err
			}
		}
		// The order is brought to Stripe's refunded total: refunds made from the order screen are
		// already recorded (the record is written before Stripe is called), so only refunds made in
		// the Stripe Dashboard add a record, and a repeated event adds nothing.
		// The difference is taken inside the order lock, so two events (or an order-screen refund still
		// waiting for Stripe) cannot both read the same recorded total and record too much or too little.
		total := gooStripeFromAmount(gxaFloat(obj, "amount_refunded"), o.Currency)
		dp := s.gooDecimals()
		return gooWithOrderLock(ctx, o.ID, func(ctx context.Context) error {
			diff := gooRound(total-s.gooTotalRefunded(ctx, o.ID), dp)
			if diff <= 0 {
				return nil
			}
			_, err := s.gooCreateRefundLocked(ctx, o, diff, "Refunded in Stripe", nil, false, false, 0)
			return err
		})
	case "charge.dispute.created":
		o, err := s.gooStripeEventOrderByIntent(ctx, obj)
		if err != nil || o == nil {
			return err
		}
		note := "A dispute was opened for this payment in Stripe (" + gooEsc(gxaStr(obj, "reason")) + "). Respond to it in the Stripe Dashboard."
		if err := s.gooSetStatusFrom(ctx, o, []string{"processing", "completed"}, "on-hold", note, ""); err != nil && !errors.Is(err, errGooStatusChanged) {
			return err
		}
	}
	return nil
}

// Refunds --------------------------------------------------------------------------------------------

func (gooStripeGateway) ProcessRefund(s *Server, ctx context.Context, o *gooOrder, amount float64, reason string) error {
	if o.TransactionID == "" {
		return errors.New("This order has no Stripe payment to refund.")
	}
	form := url.Values{"amount": {strconv.FormatInt(gooStripeAmount(amount, o.Currency), 10)}}
	if strings.HasPrefix(o.TransactionID, "ch_") {
		form.Set("charge", o.TransactionID)
	} else {
		form.Set("payment_intent", o.TransactionID)
	}
	form.Set("metadata[order_id]", itoa64(o.ID))
	if reason != "" {
		if len(reason) > 480 {
			reason = reason[:480]
		}
		form.Set("metadata[reason]", reason)
	}
	// The same refund retried after a lost response carries the same key, so Stripe refunds it once
	// (the refund record is saved before this call, so the refunded total already includes it).
	idem := fmt.Sprintf("goo-refund-%d-%d-%s", o.ID, gooStripeAmount(s.gooTotalRefunded(ctx, o.ID), o.Currency), form.Get("amount"))
	res, err := s.gooStripeCall(ctx, http.MethodPost, "/v1/refunds", form, idem)
	if err != nil {
		return fmt.Errorf("Stripe refund failed: %w", err)
	}
	if st := gxaStr(res, "status"); st == "failed" || st == "canceled" {
		return errors.New("Stripe refund failed: " + defaultStr(gxaStr(res, "failure_reason"), st))
	}
	_ = s.gooAddOrderNote(ctx, o, "Refunded through Stripe (refund "+gooEsc(gxaStr(res, "id"))+").", false, "")
	return nil
}

// Renewals -------------------------------------------------------------------------------------------

// ProcessRenewal charges a renewal order with the saved card, without the customer (off-session).
// A network failure leaves the attempt counter alone, so a retry sends the same Idempotency-Key and
// cannot charge twice; a decline counts as an attempt, so a later retry is a new charge.
func (gooStripeGateway) ProcessRenewal(s *Server, ctx context.Context, o *gooOrder, token string) (string, error) {
	if token == "" || !gooStripeIDOK(token) {
		return "", errors.New("no saved Stripe payment method for this subscription")
	}
	customer := gxaStr(o.Meta, "_stripe_customer_id")
	if customer == "" {
		_ = s.store.Pool.QueryRow(ctx, `SELECT customer_ref FROM goo_payment_tokens WHERE gateway_id = $1 AND token = $2`, gooStripeID, token).Scan(&customer)
	}
	if customer == "" {
		pm, err := s.gooStripeCall(ctx, http.MethodGet, "/v1/payment_methods/"+token, nil, "")
		if err != nil {
			return "", err
		}
		if customer = gooStripeRef(pm["customer"]); customer == "" {
			return "", errors.New("the saved Stripe payment method is not attached to a customer")
		}
	}
	attempt := int(gxaFloat(o.Meta, "_stripe_renewal_attempts"))
	form := url.Values{"amount": {strconv.FormatInt(gooStripeAmount(o.Total, o.Currency), 10)}, "currency": {strings.ToLower(o.Currency)},
		"customer": {customer}, "payment_method": {token}, "off_session": {"true"}, "confirm": {"true"}, "error_on_requires_action": {"true"},
		"description": {fmt.Sprintf("Order #%s (subscription renewal)", o.Number())}}
	form.Set("payment_method_types[0]", "card")
	gooStripeOrderMeta(form, "metadata", s, ctx, o)
	pi, err := s.gooStripeCall(ctx, http.MethodPost, "/v1/payment_intents", form, fmt.Sprintf("goo-renewal-%d-%d", o.ID, attempt))
	bump := func() {
		o.Meta["_stripe_renewal_attempts"] = float64(attempt + 1)
		_ = s.gooSaveOrder(ctx, o, false)
	}
	if err != nil {
		var se *gooStripeError
		if errors.As(err, &se) && se.Status >= 400 && se.Status < 500 && se.Status != http.StatusConflict && se.Status != http.StatusTooManyRequests {
			bump()
		}
		return "", err
	}
	id := gxaStr(pi, "id")
	if st := gxaStr(pi, "status"); st != "succeeded" {
		bump()
		if st == "processing" {
			o.Meta["_stripe_intent_id"] = id
			_ = s.gooSaveOrder(ctx, o, false)
			return id, fmt.Errorf("Stripe payment %s is still processing; the order is completed when Stripe confirms it: %w", id, errGooPaymentPending)
		}
		return "", errors.New("the Stripe payment was not completed: " + st)
	}
	o.Meta["_stripe_intent_id"] = id
	o.Meta["_stripe_customer_id"] = customer
	_ = s.gooSaveOrder(ctx, o, false)
	return id, nil
}

// Saving a card without a purchase --------------------------------------------------------------------

// SetupURL starts a setup-mode Checkout Session: the customer enters a card on Stripe's page and it is
// saved to their account (and, with subID, becomes the subscription's payment method).
func (gooStripeGateway) SetupURL(s *Server, c *gin.Context, subID int64) (string, error) {
	ctx := c.Request.Context()
	u := currentUser(c)
	if u == nil {
		return "", errors.New("Please log in to add a payment method.")
	}
	customer, err := s.gooStripeCustomer(ctx, u.ID, u.Email, u.DisplayName, true)
	if err != nil {
		log.Printf("goocommerce: stripe: setup: %v", err)
		return "", errors.New("The payment method could not be added right now. Please try again later.")
	}
	back := s.gooAccountURL(ctx, "payment-methods")
	ret := s.gooWCAPIURL(ctx, gooStripeID) + "?setup=1&session_id={CHECKOUT_SESSION_ID}"
	form := url.Values{"mode": {"setup"}, "customer": {customer}, "success_url": {ret}, "cancel_url": {back}}
	form.Set("payment_method_types[0]", "card")
	form.Set("metadata[user_id]", itoa64(u.ID))
	form.Set("metadata[site]", s.links(ctx).HomeURL("/"))
	if subID != 0 {
		form.Set("metadata[subscription_id]", itoa64(subID))
		form.Set("cancel_url", s.gooEndpointURL(ctx, "myaccount", "view-subscription", itoa64(subID)))
	}
	sess, err := s.gooStripeCall(ctx, http.MethodPost, "/v1/checkout/sessions", form, "")
	if err != nil || gxaStr(sess, "url") == "" {
		log.Printf("goocommerce: stripe: setup: %v", err)
		return "", errors.New("The payment method could not be added right now. Please try again later.")
	}
	return gxaStr(sess, "url"), nil
}

// gooStripeApplySetup saves the card of a completed setup session. asUser (the logged-in customer on
// the return URL, 0 for the webhook) must be the user the session was created for. It returns the
// subscription that now pays with the card (0: none).
func (s *Server) gooStripeApplySetup(ctx context.Context, sess map[string]any, asUser int64) (int64, error) {
	md := gooJSONMap(sess["metadata"])
	userID := gooAtoi64(gxaStr(md, "user_id"))
	if userID == 0 || gxaStr(md, "site") != s.links(ctx).HomeURL("/") || (asUser != 0 && asUser != userID) {
		return 0, nil
	}
	if gxaStr(sess, "status") != "complete" {
		return 0, errors.New("the setup session is not complete")
	}
	customer := gooStripeRef(sess["customer"])
	if customer == "" || customer != s.gooUserMetaString(ctx, userID, s.gooStripeCustomerKey()) {
		return 0, nil
	}
	si := gooJSONMap(sess["setup_intent"])
	if si == nil {
		sid := gooStripeRef(sess["setup_intent"])
		if !gooStripeIDOK(sid) {
			return 0, errors.New("the setup session has no SetupIntent")
		}
		var err error
		if si, err = s.gooStripeCall(ctx, http.MethodGet, "/v1/setup_intents/"+sid, url.Values{"expand[]": {"payment_method"}}, ""); err != nil {
			return 0, err
		}
	}
	pmID := gooStripeRef(si["payment_method"])
	if gxaStr(si, "status") != "succeeded" || !gooStripeIDOK(pmID) {
		return 0, errors.New("the card was not saved")
	}
	pm := gooJSONMap(si["payment_method"])
	if pm == nil {
		pm, _ = s.gooStripeCall(ctx, http.MethodGet, "/v1/payment_methods/"+pmID, nil, "")
	}
	s.gooStripeSaveCard(ctx, userID, customer, pmID, pm)
	subID := gooAtoi64(gxaStr(md, "subscription_id"))
	if subID == 0 {
		return 0, nil
	}
	sub, err := s.gsubGet(ctx, subID)
	if err != nil {
		if gooNotFound(err) { // deleted since: nothing to move to the card
			return 0, nil
		}
		// The webhook already claimed the event: returning the error releases it, so Stripe sends it
		// again instead of the subscription silently keeping its old payment method.
		return 0, err
	}
	if sub.CustomerID != userID || sub.Status == "cancelled" || sub.Status == "expired" {
		return 0, nil
	}
	if sub.PaymentMethod == gooStripeID && gxaStr(sub.Meta, "_payment_token") == pmID {
		return subID, nil
	}
	sub.PaymentMethod, sub.PaymentMethodTitle = gooStripeID, s.gooGateway(gooStripeID).Title()
	sub.Meta["_payment_token"] = pmID
	sub.Meta["_stripe_customer_id"] = customer
	delete(sub.Meta, "_requires_manual_renewal")
	s.gsubAddNote(ctx, sub, "Payment method changed to "+sub.PaymentMethodTitle+" by the subscriber.", false, "customer")
	return subID, s.gsubSave(ctx, sub)
}

// gooStripeSetupReturn is the customer's return from a setup session.
func (s *Server) gooStripeSetupReturn(c *gin.Context) {
	ctx := c.Request.Context()
	back := s.gooAccountURL(ctx, "payment-methods")
	u := currentUser(c)
	sid := c.Query("session_id")
	if u == nil || !gooStripeIDOK(sid) {
		s.gooRedirect(c, back)
		return
	}
	sess, err := s.gooStripeCall(ctx, http.MethodGet, "/v1/checkout/sessions/"+sid, url.Values{"expand[]": {"setup_intent.payment_method"}}, "")
	var subID int64
	if err == nil {
		subID, err = s.gooStripeApplySetup(ctx, sess, u.ID)
	}
	if err != nil {
		log.Printf("goocommerce: stripe: setup return: %v", err)
		s.gooAddNotice(c, "error", "The payment method could not be saved. Please try again.")
		s.gooRedirect(c, back)
		return
	}
	if subID != 0 {
		s.gooAddNotice(c, "success", "Payment method updated.")
		s.gooRedirect(c, s.gooEndpointURL(ctx, "myaccount", "view-subscription", itoa64(subID)))
		return
	}
	s.gooAddNotice(c, "success", "Payment method successfully added.")
	s.gooRedirect(c, back)
}

// DeleteToken detaches the card from the Stripe customer; a card Stripe no longer knows is fine.
func (gooStripeGateway) DeleteToken(s *Server, ctx context.Context, t *gooPaymentToken) error {
	if !gooStripeIDOK(t.Token) || t.Mode != (gooStripeGateway{}).TokenMode(s) {
		return nil
	}
	_, err := s.gooStripeCall(ctx, http.MethodPost, "/v1/payment_methods/"+t.Token+"/detach", url.Values{}, "")
	var se *gooStripeError
	if errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusBadRequest) {
		return nil
	}
	return err
}
