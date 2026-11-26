package server

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// Payment gateways (WC_Payment_Gateway). The interface lets real providers be added later; the built-in
// ones are WooCommerce's offline methods (direct bank transfer, check, cash on delivery) and a test
// gateway that approves or declines without a provider.

// gooPaymentResult is process_payment()'s array: result "success" with a redirect, or "failure".
type gooPaymentResult struct {
	Result   string `json:"result"`
	Redirect string `json:"redirect"`
	Message  string `json:"message,omitempty"`
}

type gooGatewayDef interface {
	ID() string
	// MethodTitle / MethodDescription are shown in Settings › Payments.
	MethodTitle() string
	MethodDescription() string
	// FormFields are the gateway's settings (enabled, title, description, …).
	FormFields() []gooField
	// Supports lists features: "products", "refunds".
	Supports(feature string) bool
	// PaymentFields renders the fields under the method on the checkout (card form, instructions).
	PaymentFields(s *Server, c *gin.Context) string
	// Validate checks the posted payment fields.
	Validate(s *Server, c *gin.Context) error
	// ProcessPayment takes the payment for an order created by the checkout.
	ProcessPayment(s *Server, c *gin.Context, o *gooOrder) (gooPaymentResult, error)
	// ProcessRefund refunds through the provider.
	ProcessRefund(s *Server, ctx context.Context, o *gooOrder, amount float64, reason string) error
	// ThankYou prints on the order received page (bank details, instructions).
	ThankYou(s *Server, ctx context.Context, o *gooOrder) string
	// Available reports whether the gateway can be used for a cart (cash on delivery's shipping methods).
	Available(s *Server, c *gin.Context, cart *gooCart) bool
}

// gooOrderAvailableGateway is implemented by gateways whose availability depends on the order being
// paid on the Pay for order page (WC_Gateway_COD::is_available reads the order-pay order there).
type gooOrderAvailableGateway interface {
	AvailableForOrder(s *Server, ctx context.Context, o *gooOrder) bool
}

// gooRenewalGateway is implemented by gateways that can charge a stored payment token without the
// customer present (WC_Gateway::scheduled_subscription_payment). A gateway that implements it and reports
// Supports("subscriptions") gets automatic subscription renewals; any other gateway renews manually.
type gooRenewalGateway interface {
	// ProcessRenewal charges the renewal order o with the token saved on the subscription
	// (order/subscription meta "_payment_token") and returns the provider's transaction id.
	ProcessRenewal(s *Server, ctx context.Context, o *gooOrder, token string) (string, error)
	// RenewalNeedsToken reports whether an empty token means the renewal cannot be automatic.
	RenewalNeedsToken() bool
}

// errGooPaymentPending is returned (wrapped) by ProcessRenewal when the provider accepted the charge
// but has not settled it yet (a bank debit, a slow card network): the renewal order goes on hold and
// the provider's notification completes or fails it later.
var errGooPaymentPending = errors.New("payment pending")

var gooGatewayRegistry = map[string]gooGatewayDef{}

// gooRegisterGateway adds a gateway (woocommerce_payment_gateways).
func gooRegisterGateway(g gooGatewayDef) { gooGatewayRegistry[g.ID()] = g }

// gooGatewaySettings is the gateway's saved settings over its field defaults.
func (s *Server) gooGatewaySettings(id string) map[string]any {
	g := gooGatewayRegistry[id]
	out := map[string]any{}
	if g != nil {
		for _, f := range g.FormFields() {
			if f.Default != nil {
				out[f.ID] = f.Default
			}
		}
	}
	for k, v := range s.gooOptMap(id + "_settings") {
		out[k] = v
	}
	if g != nil {
		for _, f := range g.FormFields() { // secrets are stored sealed (gooSealGatewaySecrets)
			if v, ok := out[f.ID].(string); ok && f.Type == "password" {
				out[f.ID] = s.revealSecret(v)
			}
		}
	}
	return out
}

// gooSealGatewaySecrets seals the password fields (API keys, webhook signing secrets) of a gateway's
// settings option (<gateway id>_settings) before it is stored; other options pass through.
func (s *Server) gooSealGatewaySecrets(key string, v any) any {
	id, ok := strings.CutSuffix(key, "_settings")
	g := gooGatewayRegistry[id]
	m, isMap := v.(map[string]any)
	if !ok || g == nil || !isMap {
		return v
	}
	out := make(map[string]any, len(m))
	for k, x := range m {
		out[k] = x
	}
	for _, f := range g.FormFields() {
		if x, ok := out[f.ID].(string); ok && f.Type == "password" {
			out[f.ID] = s.sealSecret(x)
		}
	}
	return out
}

func (s *Server) gooGatewaySetting(id, key string) string {
	switch v := s.gooGatewaySettings(id)[key].(type) {
	case string:
		return v
	case bool:
		if v {
			return "yes"
		}
		return "no"
	}
	return ""
}

// gooGateway is a registered gateway by id.
func (s *Server) gooGateway(id string) *gooGatewayHandle {
	g := gooGatewayRegistry[id]
	if g == nil {
		return nil
	}
	return &gooGatewayHandle{gooGatewayDef: g, s: s}
}

// gooGatewayHandle binds a gateway to its settings.
type gooGatewayHandle struct {
	gooGatewayDef
	s *Server
}

func (g *gooGatewayHandle) Enabled() bool { return g.s.gooGatewaySetting(g.ID(), "enabled") == "yes" }
func (g *gooGatewayHandle) Title() string {
	return defaultStr(g.s.gooGatewaySetting(g.ID(), "title"), g.MethodTitle())
}
func (g *gooGatewayHandle) Description() string { return g.s.gooGatewaySetting(g.ID(), "description") }

// gooGatewaysOrdered lists every gateway in the saved order (Settings › Payments drag order).
func (s *Server) gooGatewaysOrdered() []*gooGatewayHandle {
	order := s.gooOptList("gateway_order")
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i + 1
	}
	var out []*gooGatewayHandle
	for id := range gooGatewayRegistry {
		out = append(out, s.gooGateway(id))
	}
	def := map[string]int{"bacs": 1, "cheque": 2, "cod": 3, "goo_stripe": 4, "goo_test": 5}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := pos[out[i].ID()], pos[out[j].ID()]
		if pi == 0 {
			pi = 100 + def[out[i].ID()]
		}
		if pj == 0 {
			pj = 100 + def[out[j].ID()]
		}
		if pi != pj {
			return pi < pj
		}
		return out[i].ID() < out[j].ID()
	})
	return out
}

// gooAvailableGateways is WC_Payment_Gateways::get_available_payment_gateways().
func (s *Server) gooAvailableGateways(c *gin.Context, cart *gooCart) []*gooGatewayHandle {
	var out []*gooGatewayHandle
	for _, g := range s.gooGatewaysOrdered() {
		if g.Enabled() && g.Available(s, c, cart) {
			out = append(out, g)
		}
	}
	for _, f := range gooGatewayFilters {
		out = f(s, c, cart, out)
	}
	return out
}

// gooAvailableGatewaysForOrder is the gateway list of the Pay for order page: the gateways available
// without a cart, narrowed by each gateway's rules for the order being paid.
func (s *Server) gooAvailableGatewaysForOrder(c *gin.Context, o *gooOrder) []*gooGatewayHandle {
	var out []*gooGatewayHandle
	for _, g := range s.gooAvailableGateways(c, nil) {
		if og, ok := g.gooGatewayDef.(gooOrderAvailableGateway); ok && !og.AvailableForOrder(s, c.Request.Context(), o) {
			continue
		}
		out = append(out, g)
	}
	return out
}

func gooGatewayBaseFields(title, desc string) []gooField {
	return []gooField{
		{ID: "enabled", Title: "Enable/Disable", Type: "checkbox", Default: "no", Desc: "Enable " + title},
		{ID: "title", Title: "Title", Type: "text", Default: title, Desc: "This controls the title which the user sees during checkout."},
		{ID: "description", Title: "Description", Type: "textarea", Default: desc, Desc: "Payment method description that the customer will see on your checkout."},
		{ID: "instructions", Title: "Instructions", Type: "textarea", Default: "", Desc: "Instructions that will be added to the thank you page and emails."},
	}
}

// gooOfflineGateway is the shape shared by BACS, check and cash on delivery: the order goes on-hold
// (processing for cash on delivery of physical goods) and the customer pays outside the site.
// A cash-on-delivery order that contains a downloadable file stays on-hold, as WooCommerce does,
// so the file is not released before the payment is taken.
type gooOfflineGateway struct {
	id, title, desc, methodDesc string
	status                      string
	extra                       []gooField
}

func (g *gooOfflineGateway) ID() string                { return g.id }
func (g *gooOfflineGateway) MethodTitle() string       { return g.title }
func (g *gooOfflineGateway) MethodDescription() string { return g.methodDesc }
func (g *gooOfflineGateway) FormFields() []gooField {
	return append(gooGatewayBaseFields(g.title, g.desc), g.extra...)
}
func (g *gooOfflineGateway) Supports(f string) bool { return f == "products" }
func (g *gooOfflineGateway) PaymentFields(s *Server, c *gin.Context) string {
	return s.gooT(c, s.gooGatewaySetting(g.id, "description")) // Golylang: Languages › Translations
}
func (g *gooOfflineGateway) Validate(s *Server, c *gin.Context) error { return nil }
func (g *gooOfflineGateway) ProcessRefund(s *Server, ctx context.Context, o *gooOrder, amount float64, reason string) error {
	return errors.New("refunds are not supported by this payment method")
}
func (g *gooOfflineGateway) ThankYou(s *Server, ctx context.Context, o *gooOrder) string {
	out := ""
	if ins := s.gooT(ctx, s.gooGatewaySetting(g.id, "instructions")); ins != "" {
		out += "<p>" + strings.ReplaceAll(gooEsc(ins), "\n", "<br>") + "</p>"
	}
	if g.id == "bacs" {
		out += s.gooBACSDetails()
	}
	return out
}
func (g *gooOfflineGateway) Available(s *Server, c *gin.Context, cart *gooCart) bool {
	if g.id != "cod" || cart == nil {
		return true
	}
	// Cash on delivery: only for the selected shipping methods, and virtual orders when enabled.
	if !cart.NeedsShipping() {
		return s.gooGatewaySetting("cod", "enable_for_virtual") == "yes"
	}
	methods := s.gooGatewaySettings("cod")["enable_for_methods"]
	list, _ := methods.([]any)
	if len(list) == 0 {
		return true
	}
	for _, chosen := range cart.sess.Data.ChosenShipping {
		for _, m := range list {
			ms, _ := m.(string)
			if ms != "" && (chosen == ms || strings.HasPrefix(chosen, ms+":")) {
				return true
			}
		}
	}
	return false
}

// AvailableForOrder applies the cash-on-delivery rules of Available to an order on the Pay for order
// page: virtual orders only when enabled, and only the selected shipping methods of the order's
// shipping lines (method_id:instance_id), as WC_Gateway_COD::is_available does there.
func (g *gooOfflineGateway) AvailableForOrder(s *Server, ctx context.Context, o *gooOrder) bool {
	if g.id != "cod" {
		return true
	}
	needsShipping := false
	if s.gooShippingEnabled() {
		for _, it := range o.ItemsOf("line_item") {
			pid := it.ProductID
			if it.VariationID != 0 {
				pid = it.VariationID
			}
			if p, err := s.gooGetProduct(ctx, pid); err == nil && s.gooNeedsShipping(ctx, p) {
				needsShipping = true
				break
			}
		}
	}
	if !needsShipping {
		return s.gooGatewaySetting("cod", "enable_for_virtual") == "yes"
	}
	list, _ := s.gooGatewaySettings("cod")["enable_for_methods"].([]any)
	if len(list) == 0 {
		return true
	}
	for _, it := range o.ItemsOf("shipping") {
		mid := gxStr(it.Meta["method_id"])
		if mid == "" {
			continue
		}
		rate := mid
		if inst := gxStr(it.Meta["instance_id"]); inst != "" {
			rate += ":" + inst
		}
		for _, m := range list {
			if ms, _ := m.(string); ms != "" && (rate == ms || strings.HasPrefix(rate, ms+":")) {
				return true
			}
		}
	}
	return false
}

func (g *gooOfflineGateway) ProcessPayment(s *Server, c *gin.Context, o *gooOrder) (gooPaymentResult, error) {
	ctx := c.Request.Context()
	if o.Total > 0 {
		note := "Awaiting " + strings.ToLower(g.title) + " payment"
		status := g.status
		if g.id == "cod" {
			note = "Payment to be made upon delivery."
			// WC_Gateway_COD: has_downloadable_item() keeps the order on-hold. Processing would
			// count as paid and, with "Grant access after payment", release the files immediately;
			// the same holds for gift cards and memberships, which are issued on processing.
			if s.gooOrderReleasesValueOnPayment(ctx, o) {
				status = "on-hold"
			}
		}
		if err := s.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, status, note, ""); err != nil {
			return gooPaymentResult{Result: "failure"}, err
		}
	} else if err := s.gooPaymentComplete(ctx, o, ""); err != nil {
		return gooPaymentResult{Result: "failure"}, err
	}
	s.gooCartFor(c).Empty()
	return gooPaymentResult{Result: "success", Redirect: s.gooOrderReceivedURL(ctx, o)}, nil
}

// gooBACSDetails prints the bank accounts of the direct bank transfer gateway.
func (s *Server) gooBACSDetails() string {
	accounts, _ := s.gooGatewaySettings("bacs")["accounts"].([]any)
	if len(accounts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<section class="woocommerce-bacs-bank-details"><h2 class="wc-bacs-bank-details-heading">Our bank details</h2>`)
	for _, a := range accounts {
		m, _ := a.(map[string]any)
		str := func(k string) string { v, _ := m[k].(string); return v }
		if name := str("account_name"); name != "" {
			b.WriteString(`<h3 class="wc-bacs-bank-details-account-name">` + gooEsc(name) + `:</h3>`)
		}
		b.WriteString(`<ul class="wc-bacs-bank-details order_details bacs_details">`)
		for _, f := range [][2]string{{"bank_name", "Bank"}, {"account_number", "Account number"}, {"sort_code", "Sort code"}, {"iban", "IBAN"}, {"bic", "BIC"}} {
			if v := str(f[0]); v != "" {
				b.WriteString(`<li class="` + f[0] + `">` + f[1] + `: <strong>` + gooEsc(v) + `</strong></li>`)
			}
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(`</section>`)
	return b.String()
}

// gooTestGateway approves payments without a provider (or declines when the customer types "decline"
// as the test card holder): for trying the checkout, and the model for real gateways.
type gooTestGateway struct{}

func (gooTestGateway) ID() string          { return "goo_test" }
func (gooTestGateway) MethodTitle() string { return "Test payments" }
func (gooTestGateway) MethodDescription() string {
	return "Take test payments without a payment provider: every payment succeeds, except when the card holder is \"decline\". Refunds are simulated."
}
func (gooTestGateway) FormFields() []gooField {
	f := gooGatewayBaseFields("Test payment", "Pay with the test gateway. No money is charged.")
	return append(f[:3], gooField{ID: "status", Title: "Order status after payment", Type: "select", Default: "processing",
		Options: []gooOpt{{"processing", "Processing (payment complete)"}, {"on-hold", "On hold"}}})
}
func (gooTestGateway) Supports(f string) bool {
	return f == "products" || f == "refunds" || f == "subscriptions"
}

// ProcessRenewal: the test gateway renews without a stored token; the token "decline" fails.
func (gooTestGateway) ProcessRenewal(s *Server, ctx context.Context, o *gooOrder, token string) (string, error) {
	if token == "decline" {
		return "", errors.New("the test gateway declined the renewal")
	}
	return "test_" + gooRandomKey(16), nil
}
func (gooTestGateway) RenewalNeedsToken() bool { return false }
func (gooTestGateway) PaymentFields(s *Server, c *gin.Context) string {
	return gooEsc(s.gooT(c, s.gooGatewaySetting("goo_test", "description"))) +
		`<fieldset id="goo_test-cc-form" class="wc-credit-card-form wc-payment-form"><p class="form-row form-row-wide"><label for="goo_test-card-holder">Card holder</label>` +
		`<input id="goo_test-card-holder" class="input-text" type="text" name="goo_test_card_holder" autocomplete="cc-name" placeholder="Any name"></p></fieldset>`
}
func (gooTestGateway) Validate(s *Server, c *gin.Context) error { return nil }
func (gooTestGateway) ProcessPayment(s *Server, c *gin.Context, o *gooOrder) (gooPaymentResult, error) {
	ctx := c.Request.Context()
	if strings.EqualFold(strings.TrimSpace(c.PostForm("goo_test_card_holder")), "decline") {
		_ = s.gooSetStatus(ctx, o, "failed", "Test payment declined.", "")
		return gooPaymentResult{Result: "failure", Message: "The test payment was declined."}, errors.New("The test payment was declined.")
	}
	tx := "test_" + gooRandomKey(16)
	if s.gooGatewaySetting("goo_test", "status") == "on-hold" {
		o.TransactionID = tx
		if err := s.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "on-hold", "Test payment authorized.", ""); err != nil {
			return gooPaymentResult{Result: "failure"}, err
		}
	} else if err := s.gooPaymentComplete(ctx, o, tx); err != nil {
		return gooPaymentResult{Result: "failure"}, err
	}
	s.gooCartFor(c).Empty()
	return gooPaymentResult{Result: "success", Redirect: s.gooOrderReceivedURL(ctx, o)}, nil
}
func (gooTestGateway) ProcessRefund(s *Server, ctx context.Context, o *gooOrder, amount float64, reason string) error {
	return nil
}
func (gooTestGateway) ThankYou(s *Server, ctx context.Context, o *gooOrder) string { return "" }
func (gooTestGateway) Available(s *Server, c *gin.Context, cart *gooCart) bool     { return true }

func init() {
	gooRegisterGateway(&gooOfflineGateway{id: "bacs", title: "Direct bank transfer", status: "on-hold",
		desc:       "Make your payment directly into our bank account. Please use your Order ID as the payment reference. Your order will not be shipped until the funds have cleared in our account.",
		methodDesc: "Take payments in person via BACS. More commonly known as direct bank/wire transfer.",
		extra:      []gooField{{ID: "accounts", Title: "Account details", Type: "bacs_accounts"}}})
	gooRegisterGateway(&gooOfflineGateway{id: "cheque", title: "Check payments", status: "on-hold",
		desc:       "Please send a check to Store Name, Store Street, Store Town, Store State / County, Store Postcode.",
		methodDesc: "Take payments in person via checks. This offline gateway can also be useful to test purchases."})
	gooRegisterGateway(&gooOfflineGateway{id: "cod", title: "Cash on delivery", status: "processing",
		desc:       "Pay with cash upon delivery.",
		methodDesc: "Have your customers pay with cash (or by other means) upon delivery.",
		extra: []gooField{
			{ID: "enable_for_methods", Title: "Enable for shipping methods", Type: "multiselect", Desc: "If COD is only available for certain methods, set it up here. Leave blank to enable for all methods."},
			{ID: "enable_for_virtual", Title: "Accept for virtual orders", Type: "checkbox", Default: "yes", Desc: "Accept COD if the order is virtual"},
		}})
	gooRegisterGateway(gooTestGateway{})
}
