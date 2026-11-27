package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/pay"
	"wpgo/internal/pay/all"
)

// The hosted-payment bridge: one Commerce gateway per provider of internal/pay (PayTR, iyzico, the
// bank virtual POS systems, PayPal, …). A provider package knows its protocol; this file does the
// Commerce side once for all of them:
//
//	checkout     ProcessPayment records an attempt (goo_payment_attempts), asks the provider to start a
//	             hosted payment and sends the customer there: a redirect, or the bridge's own page
//	             (?pay=go) that auto-submits a form to the provider or shows the provider's iframe
//	callbacks    /wc-api/goo_<id>/?pay=return|fail|notify: the provider verifies, the bridge compares
//	             the proof with the attempt and the order, then moves the order (goo_gateway_pay_callback.go)
//	refunds      ProcessRefund through the provider, never retried when the outcome is unknown
//	resolver     an hourly job asks providers with a status API about attempts left open
//
// Card data never reaches this server: the card is typed on the provider's page or in its iframe.

func init() {
	for _, sp := range all.Specs() {
		gooRegisterGateway(&gooPayGateway{spec: sp})
	}
}

// gooPayGateway is a provider of internal/pay as a Commerce gateway. Its id is "goo_" + Spec.ID.
type gooPayGateway struct{ spec pay.Spec }

func (g *gooPayGateway) ID() string                { return "goo_" + g.spec.ID }
func (g *gooPayGateway) MethodTitle() string       { return g.spec.Title }
func (g *gooPayGateway) MethodDescription() string { return g.spec.Description }

func (g *gooPayGateway) has(f pay.Feature) bool {
	for _, x := range g.spec.Features {
		if x == f {
			return true
		}
	}
	return false
}

// installmentFields reports whether the settings offer an installment limit: PSPs whose hosted page
// lets the customer choose. Bank POS installments are chosen in the store (storeInstallmentFields).
func (g *gooPayGateway) installmentFields() bool {
	return g.has(pay.FeatureInstallments) && g.spec.Kind == pay.KindPSP
}

func (g *gooPayGateway) Info() gooGatewayInfo {
	f := make([]string, 0, len(g.spec.Features))
	for _, x := range g.spec.Features {
		f = append(f, string(x))
	}
	return gooGatewayInfo{Region: string(g.spec.Region), Kind: string(g.spec.Kind), Features: f, DocsURL: g.spec.DocsURL}
}

func (g *gooPayGateway) Supports(f string) bool {
	switch f {
	case "products":
		return true
	case "refunds":
		return g.has(pay.FeatureRefunds)
	}
	return false
}

func (g *gooPayGateway) FormFields() []gooField {
	f := gooGatewayBaseFields(g.spec.Title, g.defaultDescription())
	f[0].Desc = "Enable " + g.spec.Title
	out := append([]gooField{}, f[:3]...)
	out = append(out, gooField{ID: "testmode", Title: "Test mode", Type: "checkbox", Default: "yes", Desc: "Enable test mode",
		Tip: "Test mode uses the provider's test environment; no real money is charged."})
	for _, sf := range g.spec.Fields {
		gf := gooField{ID: sf.ID, Title: sf.Title, Type: sf.Type, Desc: gooEsc(sf.Desc)}
		if sf.Default != "" {
			gf.Default = sf.Default
		}
		if sf.Secret {
			// The settings screen never prints a saved password field back.
			gf.Type = "password"
			if gf.Desc != "" {
				gf.Desc += " "
			}
			gf.Desc += "A saved value is not shown again; leave the field empty to keep it."
		}
		for _, o := range sf.Options {
			gf.Options = append(gf.Options, gooOpt{o.Value, o.Label})
		}
		out = append(out, gf)
	}
	if g.installmentFields() {
		out = append(out,
			gooField{ID: "installments", Title: "Installments", Type: "checkbox", Default: "yes", Desc: "Offer installments on the payment page"},
			gooField{ID: "max_installment", Title: "Maximum installments", Type: "number", Default: "12", Min: "1", Step: "1", Show: "installments",
				Desc: "The highest number of installments the payment page offers (the provider's and the card's limits still apply)."})
	}
	if g.storeInstallments() {
		out = append(out, g.storeInstallmentFields()...)
	}
	return out
}

// gooPayDescFormat is the default method description; it is translated as a format, so a description
// left at its default speaks the store's language.
const gooPayDescFormat = "Pay securely with %s. You will complete the payment on the provider's secure page."

func (g *gooPayGateway) defaultDescription() string {
	return fmt.Sprintf(gooPayDescFormat, g.spec.Title)
}

// gooPayT translates a customer-facing sentence of the bridge: the merchant's own translation
// (Languages › Translations) first, then the bundled catalog of the store front's language.
func (s *Server) gooPayT(ctx context.Context, str string) string {
	if t := s.gooT(ctx, str); t != str {
		return t
	}
	return s.gooL10nText(ctx, str)
}

// PatchFields shows the notification URL to type into the provider's panel.
func (g *gooPayGateway) PatchFields(s *Server, ctx context.Context, fields []gooField) []gooField {
	return append(fields, gooField{ID: "notify_url", Title: "Notification URL", Type: "title",
		Desc: `If ` + gooEsc(g.spec.Title) + ` asks for a notification (callback) URL in its merchant panel, enter <code>` +
			gooEsc(s.gooPayNotifyURL(ctx, g)) + `</code>.`})
}

// Settings and the provider -----------------------------------------------------------------------------

// gooPayHTTP is the outbound client of the providers: a timeout, no proxy, no private addresses, and
// redirects only within the host that was called.
var gooPayHTTP = func() *http.Client {
	c := publicClient(30*time.Second, nil, "the payment provider address points at a private network")
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
			return errors.New("redirect to another host refused")
		}
		return nil
	}
	return c
}()

// config is the provider configuration from the saved settings; test picks the mode.
func (g *gooPayGateway) config(s *Server, test bool) pay.Config {
	vals := map[string]string{}
	for _, f := range g.spec.Fields {
		vals[f.ID] = strings.TrimSpace(s.gooGatewaySetting(g.ID(), f.ID))
	}
	return pay.Config{Values: vals, Test: test, HTTP: gooPayHTTP}
}

// provider builds the provider in the given mode.
func (g *gooPayGateway) provider(s *Server, test bool) (pay.Provider, error) {
	if g.spec.New == nil {
		return nil, errors.New("this payment method is not available")
	}
	p, err := g.spec.New(g.config(s, test))
	if err == nil && p == nil {
		err = errors.New("this payment method is not available")
	}
	return p, err
}

// SetupProblem is "" when the gateway is configured for its current mode.
func (g *gooPayGateway) SetupProblem(s *Server) string {
	cfg := g.config(s, s.gooGatewayTestMode(g.ID()))
	for _, f := range g.spec.Fields {
		if f.Required && cfg.Values[f.ID] == "" {
			// The admin sees this in the site language (the title is translated before it goes into the sentence).
			return s.gooL10nf(context.Background(), "The setting \"%s\" is missing.", s.gooL10nText(context.Background(), f.Title))
		}
	}
	if _, err := g.provider(s, cfg.Test); err != nil {
		return err.Error()
	}
	return ""
}

func (g *gooPayGateway) currencyOK(cur string) bool {
	if len(g.spec.Currencies) == 0 {
		return true
	}
	for _, c := range g.spec.Currencies {
		if strings.EqualFold(c, cur) {
			return true
		}
	}
	return false
}

func (g *gooPayGateway) Available(s *Server, c *gin.Context, cart *gooCart) bool {
	return g.currencyOK(s.gooCurrency()) && g.SetupProblem(s) == ""
}

// AvailableForOrder: the Pay for order page offers the gateway only in the order's currency.
func (g *gooPayGateway) AvailableForOrder(s *Server, ctx context.Context, o *gooOrder) bool {
	return g.currencyOK(o.Currency)
}

func (g *gooPayGateway) PaymentFields(s *Server, c *gin.Context) string {
	var b strings.Builder
	ctx := c.Request.Context()
	d := s.gooGatewaySetting(g.ID(), "description")
	if d == g.defaultDescription() {
		if t := s.gooT(ctx, d); t != d {
			d = t
		} else {
			d = s.gooL10nf(ctx, gooPayDescFormat, g.spec.Title)
		}
	} else {
		d = s.gooT(ctx, d)
	}
	if d != "" {
		b.WriteString(`<p>` + gooEsc(d) + `</p>`)
	}
	if s.gooGatewayTestMode(g.ID()) {
		b.WriteString(`<p class="goo-pay-testmode">` + gooEsc(s.gooL10nf(ctx, "TEST MODE: no real payment is taken. Use the test cards of %s on the next page.", g.spec.Title)) + `</p>`)
	}
	b.WriteString(g.installmentSelect(s, c))
	return b.String()
}

func (g *gooPayGateway) Validate(s *Server, c *gin.Context) error { return nil }

func (g *gooPayGateway) ThankYou(s *Server, ctx context.Context, o *gooOrder) string {
	switch o.Status {
	case "on-hold":
		return `<p>` + gooEsc(s.gooPayT(ctx, "Your payment is being reviewed. We will e-mail you as soon as it is confirmed.")) + `</p>`
	case "pending":
		return `<p>` + gooEsc(s.gooPayT(ctx, "We are waiting for the payment provider to confirm your payment. We will e-mail you as soon as it is confirmed.")) + `</p>`
	}
	return ""
}

// URLs ---------------------------------------------------------------------------------------------------

// gooPayNotifyURL is the provider's server-to-server notification URL: fixed per gateway, since some
// providers take one URL typed into their panel.
func (s *Server) gooPayNotifyURL(ctx context.Context, g *gooPayGateway) string {
	return s.gooWCAPIURL(ctx, g.ID()) + "?pay=notify"
}

// gooPayURL is a customer-facing callback URL of an attempt (go, return, fail, done).
func (s *Server) gooPayURL(ctx context.Context, gateway, kind, ref, key string) string {
	return s.gooWCAPIURL(ctx, gateway) + "?pay=" + kind + "&a=" + url.QueryEscape(ref) + "&k=" + url.QueryEscape(key)
}

// Money and the basket ------------------------------------------------------------------------------------

// gooPayMinor is an amount in minor units with the currency's decimals (pay.Decimals).
func gooPayMinor(amount float64, currency string) int64 {
	return int64(math.Round(amount * math.Pow10(pay.Decimals(currency))))
}

// gooPayItems is the basket of an order. The lines (whose totals already carry their discounts),
// shipping and fees, tax included, must add up to amount exactly; when they do not (rounding, a
// negative fee), the whole order is one line.
func (s *Server) gooPayItems(ctx context.Context, o *gooOrder, amount int64) []pay.Item {
	whole := []pay.Item{{ID: itoa64(o.ID), Name: "Order #" + o.Number(), Quantity: 1, UnitPrice: amount}}
	var items []pay.Item
	var sum int64
	add := func(it pay.Item) bool {
		if it.Quantity <= 0 || it.UnitPrice < 0 {
			return false
		}
		items = append(items, it)
		sum += int64(it.Quantity) * it.UnitPrice
		return true
	}
	for _, it := range o.Items {
		total := gooPayMinor(it.Total+it.TotalTax, o.Currency)
		switch it.Type {
		case "line_item":
			pid := it.ProductID
			if it.VariationID != 0 {
				pid = it.VariationID
			}
			virtual := false
			if p, err := s.gooGetProduct(ctx, pid); err == nil {
				virtual = p.Virtual
			}
			qty := it.Qty()
			pi := pay.Item{ID: itoa64(pid), Name: it.Name, Quantity: qty, Virtual: virtual}
			if qty > 0 && total%int64(qty) == 0 {
				pi.UnitPrice = total / int64(qty)
			} else {
				pi.Name, pi.Quantity, pi.UnitPrice = fmt.Sprintf("%s × %d", it.Name, qty), 1, total
			}
			if !add(pi) {
				return whole
			}
		case "shipping", "fee":
			if total == 0 {
				continue
			}
			if !add(pay.Item{ID: it.Type, Name: defaultStr(it.Name, it.Type), Quantity: 1, UnitPrice: total, Virtual: true}) {
				return whole
			}
		}
	}
	if len(items) == 0 || sum != amount {
		return whole
	}
	return items
}

func gooPayAddress(m map[string]string) pay.Address {
	return pay.Address{Name: strings.TrimSpace(m["first_name"] + " " + m["last_name"]), Line1: m["address_1"], Line2: m["address_2"],
		City: m["city"], State: m["state"], Postcode: m["postcode"], Country: strings.ToUpper(m["country"])}
}

// gooPayRef is a new attempt reference: 20 random characters of [A-Za-z0-9].
func gooPayRef() string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 0, 20)
	var buf [32]byte
	for len(out) < 20 {
		_, _ = rand.Read(buf[:])
		for _, b := range buf {
			if int(b) < 248 && len(out) < 20 { // 248 = 4 × 62: no modulo bias
				out = append(out, chars[int(b)%62])
			}
		}
	}
	return string(out)
}

// gooPayRefOK reports whether a value can be an attempt reference (pay.StartRequest.Ref).
func gooPayRefOK(ref string) bool {
	if len(ref) < 8 || len(ref) > 32 {
		return false
	}
	for _, r := range ref {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// Checkout -----------------------------------------------------------------------------------------------

// gooPayStart is the StartResult form / iframe kept on the attempt until the ?pay=go page renders it.
type gooPayStart struct {
	Form  *pay.Form  `json:"form,omitempty"`
	Embed *pay.Embed `json:"embed,omitempty"`
}

func (g *gooPayGateway) ProcessPayment(s *Server, c *gin.Context, o *gooOrder) (gooPaymentResult, error) {
	ctx := c.Request.Context()
	fail := func(code pay.Code) (gooPaymentResult, error) {
		msg := s.gooPayT(ctx, pay.Message(code))
		return gooPaymentResult{Result: "failure", Message: msg}, errors.New(msg)
	}
	if o.Total <= 0 {
		if err := s.gooPaymentComplete(ctx, o, ""); err != nil {
			return gooPaymentResult{Result: "failure"}, err
		}
		s.gooCartFor(c).Empty()
		return gooPaymentResult{Result: "success", Redirect: s.gooOrderReceivedURL(ctx, o)}, nil
	}
	if !g.currencyOK(o.Currency) {
		log.Printf("goocommerce: %s: order %d: currency %q is not supported", g.ID(), o.ID, o.Currency) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		return fail(pay.CodeInvalidRequest)
	}
	test := s.gooGatewayTestMode(g.ID())
	p, err := g.provider(s, test)
	if err != nil {
		log.Printf("goocommerce: %s: order %d: provider not configured: %q", g.ID(), o.ID, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		return fail(pay.CodeConfiguration)
	}
	if len(o.Items) == 0 {
		o.Items, _ = s.gooOrderItems(ctx, o.ID)
	}
	amount := gooPayMinor(o.Total, o.Currency)
	maxInst, inst := 0, g.orderInstallment(s, c, o)
	if g.installmentFields() {
		if s.gooGatewaySetting(g.ID(), "installments") != "yes" {
			maxInst = 1
		} else if n, err := strconv.Atoi(strings.TrimSpace(s.gooGatewaySetting(g.ID(), "max_installment"))); err == nil && n > 0 {
			maxInst = min(n, 36)
		}
	}
	a := &gooPayAttempt{OrderID: o.ID, Gateway: g.ID(), Ref: gooPayRef(), Amount: amount, Currency: strings.ToUpper(o.Currency), Installment: inst, Test: test}
	if g.storeInstallments() && gxaStr(o.Meta, "_goo_pay_ref") == "" {
		// The count this payment is started with; a confirmed payment overwrites it with the bank's.
		if inst > 0 {
			o.Meta["_goo_pay_installment"] = float64(inst)
		} else {
			delete(o.Meta, "_goo_pay_installment")
		}
		if err := s.gooSaveOrder(ctx, o, false); err != nil {
			log.Printf("goocommerce: %s: order %d: storing the installment count: %q", g.ID(), o.ID, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		}
	}
	if err := s.gooPayInsertAttempt(ctx, a); err != nil {
		log.Printf("goocommerce: %s: order %d: recording the attempt: %q", g.ID(), o.ID, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		return fail(pay.CodeUnknown)
	}
	ttl := s.gooOptInt("hold_stock_minutes")
	if ttl <= 0 {
		ttl = 60
	}
	billing := gooPayAddress(o.Billing)
	shipping := gooPayAddress(o.Shipping)
	if shipping.Line1 == "" {
		shipping = billing
	}
	locale := "en"
	if strings.HasPrefix(strings.ToLower(s.gooFrontLocale(ctx)), "tr") {
		locale = "tr"
	}
	userID := ""
	if o.CustomerID != 0 {
		userID = itoa64(o.CustomerID)
	}
	req := pay.StartRequest{
		Ref: a.Ref, OrderNumber: o.Number(), Amount: amount, Currency: a.Currency,
		// PSPs: the count is chosen on the provider's page (MaxInstallment limits it). Bank POS: the
		// count chosen in the store; the order total (Amount) already includes its surcharge.
		Installment: inst, MaxInstallment: maxInst,
		Description: pay.Trunc("Order #"+o.Number()+" – "+s.settings.String("blogname"), 120),
		Customer: pay.Customer{ID: userID, Email: o.Billing["email"], FirstName: o.Billing["first_name"], LastName: o.Billing["last_name"],
			Phone: o.Billing["phone"], IP: clientIP(c)},
		Billing: billing, Shipping: shipping, Items: s.gooPayItems(ctx, o, amount),
		ReturnURL: s.gooPayURL(ctx, g.ID(), "return", a.Ref, o.OrderKey),
		FailURL:   s.gooPayURL(ctx, g.ID(), "fail", a.Ref, o.OrderKey),
		NotifyURL: s.gooPayNotifyURL(ctx, g),
		Locale:    locale, ExpiresIn: time.Duration(max(ttl, 15)) * time.Minute,
	}
	res, err := p.Start(ctx, req)
	if err == nil && res == nil {
		err = errors.New("the provider returned no result")
	}
	if err == nil {
		err = g.checkStart(res)
	}
	if err != nil {
		code := pay.CodeUnknown
		var pe *pay.Error
		if errors.As(err, &pe) {
			code = pe.Code
			s.gooPayEndAttempt(ctx, a, string(pe.Code), pe.ProviderCode, pe.ProviderMessage)
		} else {
			s.gooPayEndAttempt(ctx, a, string(code), "", "")
		}
		log.Printf("goocommerce: %s: order %d attempt %s: start failed: %q", g.ID(), o.ID, a.Ref, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		return fail(code)
	}
	var start []byte
	if res.Form != nil || res.Embed != nil {
		start, _ = json.Marshal(gooPayStart{Form: res.Form, Embed: res.Embed})
	}
	if err := s.gooPayStarted(ctx, a, res.ProviderRef, start); err != nil {
		log.Printf("goocommerce: %s: order %d attempt %s: %q", g.ID(), o.ID, a.Ref, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		return fail(pay.CodeUnknown)
	}
	if res.Redirect != "" {
		cspAllowRedirect(c, res.Redirect)
		return gooPaymentResult{Result: "success", Redirect: res.Redirect}, nil
	}
	return gooPaymentResult{Result: "success", Redirect: s.gooPayURL(ctx, g.ID(), "go", a.Ref, o.OrderKey)}, nil
}
