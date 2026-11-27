package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// What real payment providers need around the gateway interface (goo_gateways.go):
//
//   - the callback route /wc-api/<gateway id>/ (and /?wc-api=<gateway id>), WooCommerce's WC_API: the
//     provider's webhook and the customer's return from a hosted payment page land there;
//   - a log of handled provider notifications, so a webhook delivered twice is processed once;
//   - saved payment methods (WC_Payment_Token): a provider reference and the card's brand / last four
//     digits for display. Card numbers never reach this server.

// gooAPIGateway is implemented by gateways that take callbacks on /wc-api/<id>/.
type gooAPIGateway interface {
	HandleAPI(s *Server, c *gin.Context)
}

// gooTokenGateway is implemented by gateways that save payment methods for later use.
type gooTokenGateway interface {
	// SetupURL starts adding a payment method on the provider's page and returns its URL. subID is the
	// subscription the new method is for (0: only save it to the account).
	SetupURL(s *Server, c *gin.Context, subID int64) (string, error)
	// DeleteToken removes the payment method at the provider.
	DeleteToken(s *Server, ctx context.Context, t *gooPaymentToken) error
	// TokenMode is the mode tokens are saved in now ("test" or "live").
	TokenMode(s *Server) string
}

// gooGatewayFieldsPatcher lets a gateway finish its settings fields with site data (the webhook URL).
type gooGatewayFieldsPatcher interface {
	PatchFields(s *Server, ctx context.Context, fields []gooField) []gooField
}

func init() {
	gooFrontRoutes = append(gooFrontRoutes, gooWCAPIRoute, gooTokenRoute)
	gooOn("daily", 50, func(s *Server, ctx context.Context, _ any) {
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_gateway_events WHERE received_at < now() - interval '60 days'`)
	})
}

// gooWCAPIName is the gateway id of a /wc-api/<id>/ or /?wc-api=<id> request ("" for other requests).
func gooWCAPIName(c *gin.Context) string {
	if n := c.Request.URL.Query().Get("wc-api"); n != "" {
		return strings.ToLower(n)
	}
	p := strings.Trim(c.Request.URL.Path, "/")
	if i := strings.Index(p, "wc-api/"); i == 0 || (i > 0 && p[i-1] == '/') {
		name := p[i+len("wc-api/"):]
		if j := strings.IndexByte(name, '/'); j >= 0 {
			name = name[:j]
		}
		return strings.ToLower(name)
	}
	return ""
}

// gooWCAPIURL is WC()->api_request_url(): the callback URL of a gateway.
func (s *Server) gooWCAPIURL(ctx context.Context, id string) string {
	return s.links(ctx).HomeURL("/wc-api/" + id + "/")
}

// gooWCAPIRoute serves the callback route. An unknown gateway answers like WooCommerce does: 400, "-1".
func gooWCAPIRoute(s *Server, c *gin.Context, path string) bool {
	name := gooWCAPIName(c)
	if name == "" {
		return false
	}
	gooNoStore(c)
	// Only gateways that are enabled, or configured but switched off (callbacks of payments already
	// in flight must still arrive), answer: an unconfigured gateway's handler is not reachable.
	if def := gooGatewayRegistry[name]; def != nil {
		if g, ok := def.(gooAPIGateway); ok && (s.gooGateway(name).Enabled() || s.gooGatewaySetupProblem(def) == "") {
			g.HandleAPI(s, c)
			return true
		}
	}
	c.String(http.StatusBadRequest, "-1")
	return true
}

// gooGatewayEventClaim records a provider notification; false means it was handled before.
func (s *Server) gooGatewayEventClaim(ctx context.Context, gateway, id, typ string) (bool, error) {
	tag, err := s.store.Pool.Exec(ctx, `INSERT INTO goo_gateway_events (gateway, event_id, event_type) VALUES ($1, $2, $3)
		ON CONFLICT (gateway, event_id) DO NOTHING`, gateway, id, typ)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// gooGatewayEventRelease forgets a notification whose handling failed, so the provider's retry runs.
func (s *Server) gooGatewayEventRelease(ctx context.Context, gateway, id string) {
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_gateway_events WHERE gateway = $1 AND event_id = $2`, gateway, id)
}

// gooOrderByTransaction finds the order a provider transaction paid.
func (s *Server) gooOrderByTransaction(ctx context.Context, gateway, txn string) (*gooOrder, error) {
	if txn == "" {
		return nil, fmt.Errorf("no transaction id")
	}
	var id int64
	if err := s.store.Pool.QueryRow(ctx, `SELECT id FROM goo_orders WHERE type = 'shop_order' AND payment_method = $1 AND transaction_id = $2
		ORDER BY id DESC LIMIT 1`, gateway, txn).Scan(&id); err != nil {
		return nil, err
	}
	return s.gooGetOrder(ctx, id)
}

// gooUserMetaString reads one key of a user's meta.
func (s *Server) gooUserMetaString(ctx context.Context, userID int64, key string) string {
	var v *string
	_ = s.store.Pool.QueryRow(ctx, `SELECT meta->>$2 FROM users WHERE id = $1`, userID, key).Scan(&v)
	if v == nil {
		return ""
	}
	return *v
}

// Payment tokens --------------------------------------------------------------------------------------

// gooPaymentToken is WC_Payment_Token_CC: a saved payment method of a customer.
type gooPaymentToken struct {
	ID          int64
	UserID      int64
	GatewayID   string
	Token       string // the provider's payment method id
	CustomerRef string // the provider's customer id
	Mode        string // test or live
	Type        string
	Brand       string
	Last4       string
	ExpMonth    int
	ExpYear     int
	Default     bool
	Created     time.Time
}

const gooTokenCols = `id, user_id, gateway_id, token, customer_ref, mode, type, brand, last4, exp_month, exp_year, is_default, created_at`

func gooScanToken(row interface{ Scan(...any) error }) (*gooPaymentToken, error) {
	t := &gooPaymentToken{}
	err := row.Scan(&t.ID, &t.UserID, &t.GatewayID, &t.Token, &t.CustomerRef, &t.Mode, &t.Type, &t.Brand, &t.Last4, &t.ExpMonth, &t.ExpYear, &t.Default, &t.Created)
	return t, err
}

// Label is the display name: "Visa ending in 4242 (expires 04/30)".
func (t *gooPaymentToken) Label() string {
	brand := t.Brand
	if brand == "" {
		brand = "Card"
	}
	out := strings.ToUpper(brand[:1]) + brand[1:]
	if t.Last4 != "" {
		out += " ending in " + t.Last4
	}
	if t.ExpMonth > 0 && t.ExpYear > 0 {
		out += fmt.Sprintf(" (expires %02d/%02d)", t.ExpMonth, t.ExpYear%100)
	}
	return out
}

// gooTokenMode is the mode a gateway saves tokens in.
func (s *Server) gooTokenMode(gateway string) string {
	if g, ok := gooGatewayRegistry[gateway].(gooTokenGateway); ok {
		return g.TokenMode(s)
	}
	return "live"
}

// gooUserTokens are a customer's saved payment methods of the gateways' current mode (gateway "": all).
func (s *Server) gooUserTokens(ctx context.Context, userID int64, gateway string) []*gooPaymentToken {
	if userID == 0 {
		return nil
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT `+gooTokenCols+` FROM goo_payment_tokens WHERE user_id = $1 AND ($2 = '' OR gateway_id = $2)
		ORDER BY is_default DESC, id DESC`, userID, gateway)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*gooPaymentToken
	for rows.Next() {
		t, err := gooScanToken(rows)
		if err != nil {
			return out
		}
		if gw := s.gooGateway(t.GatewayID); gw == nil || !gw.Enabled() || t.Mode != s.gooTokenMode(t.GatewayID) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// gooUserToken is one saved payment method of a customer (nil when it is not theirs).
func (s *Server) gooUserToken(ctx context.Context, userID, id int64) *gooPaymentToken {
	if userID == 0 || id == 0 {
		return nil
	}
	t, err := gooScanToken(s.store.Pool.QueryRow(ctx, `SELECT `+gooTokenCols+` FROM goo_payment_tokens WHERE id = $1 AND user_id = $2`, id, userID))
	if err != nil {
		return nil
	}
	return t
}

// gooSaveToken stores a payment method once (the provider's id is unique per gateway); the first
// method of a customer becomes the default.
func (s *Server) gooSaveToken(ctx context.Context, t *gooPaymentToken) error {
	if t.UserID == 0 || t.Token == "" {
		return nil
	}
	if t.Type == "" {
		t.Type = "CC"
	}
	_, err := s.store.Pool.Exec(ctx, `INSERT INTO goo_payment_tokens (user_id, gateway_id, token, customer_ref, mode, type, brand, last4, exp_month, exp_year, is_default)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			NOT EXISTS (SELECT 1 FROM goo_payment_tokens WHERE user_id = $1 AND is_default))
		ON CONFLICT (gateway_id, token) DO UPDATE SET brand = EXCLUDED.brand, last4 = EXCLUDED.last4, exp_month = EXCLUDED.exp_month,
			exp_year = EXCLUDED.exp_year, customer_ref = EXCLUDED.customer_ref
		WHERE goo_payment_tokens.user_id = EXCLUDED.user_id`,
		t.UserID, t.GatewayID, t.Token, t.CustomerRef, t.Mode, t.Type, t.Brand, t.Last4, t.ExpMonth, t.ExpYear)
	return err
}

func (s *Server) gooDeleteToken(ctx context.Context, t *gooPaymentToken) error {
	if _, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_payment_tokens WHERE id = $1`, t.ID); err != nil {
		return err
	}
	if t.Default {
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_payment_tokens SET is_default = true WHERE id = (SELECT max(id) FROM goo_payment_tokens WHERE user_id = $1)`, t.UserID)
	}
	return nil
}

func (s *Server) gooSetDefaultToken(ctx context.Context, t *gooPaymentToken) error {
	_, err := s.store.Pool.Exec(ctx, `UPDATE goo_payment_tokens SET is_default = (id = $2) WHERE user_id = $1`, t.UserID, t.ID)
	return err
}

// gooTokenGateways are the enabled gateways that save payment methods.
func (s *Server) gooTokenGateways() []*gooGatewayHandle {
	var out []*gooGatewayHandle
	for _, g := range s.gooGatewaysOrdered() {
		if _, ok := g.gooGatewayDef.(gooTokenGateway); ok && g.Enabled() && g.Supports("tokenization") {
			out = append(out, g)
		}
	}
	return out
}

// gooPaymentMethodsHTML is myaccount/payment-methods.php.
func (s *Server) gooPaymentMethodsHTML(c *gin.Context) string {
	ctx := c.Request.Context()
	u := currentUser(c)
	if u == nil {
		return ""
	}
	base := s.gooAccountURL(ctx, "payment-methods")
	nonce := s.nonce(c, "goo-payment-token")
	var b strings.Builder
	tokens := s.gooUserTokens(ctx, u.ID, "")
	if len(tokens) == 0 {
		b.WriteString(`<div class="woocommerce-Message woocommerce-Message--info woocommerce-info">No saved methods found.</div>`)
	} else {
		b.WriteString(`<table class="woocommerce-MyAccount-paymentMethods shop_table shop_table_responsive account-payment-methods-table"><thead><tr>` +
			`<th class="woocommerce-PaymentMethod woocommerce-PaymentMethod--method payment-method-method"><span class="nobr">Method</span></th>` +
			`<th class="woocommerce-PaymentMethod woocommerce-PaymentMethod--expires payment-method-expires"><span class="nobr">Expires</span></th>` +
			`<th class="woocommerce-PaymentMethod woocommerce-PaymentMethod--actions payment-method-actions"><span class="nobr">&nbsp;</span></th></tr></thead><tbody>`)
		for _, t := range tokens {
			def := ""
			if t.Default {
				def = " default-payment-method"
			}
			exp := ""
			if t.ExpMonth > 0 {
				exp = fmt.Sprintf("%02d/%02d", t.ExpMonth, t.ExpYear%100)
			}
			name := t.Brand
			if name == "" {
				name = "card"
			}
			b.WriteString(`<tr class="payment-method` + def + `"><td class="woocommerce-PaymentMethod woocommerce-PaymentMethod--method payment-method-method" data-title="Method">` +
				gooEsc(strings.ToUpper(name[:1])+name[1:]) + ` ending in ` + gooEsc(t.Last4) + `</td>` +
				`<td class="woocommerce-PaymentMethod woocommerce-PaymentMethod--expires payment-method-expires" data-title="Expires">` + gooEsc(exp) + `</td>` +
				`<td class="woocommerce-PaymentMethod woocommerce-PaymentMethod--actions payment-method-actions" data-title="&nbsp;">`)
			if !t.Default {
				b.WriteString(`<a href="` + gooEsc(base+"?goo_token_default="+itoa64(t.ID)+"&_wpnonce="+nonce) + `" class="button default">Make default</a> `)
			}
			b.WriteString(`<a href="` + gooEsc(base+"?goo_token_delete="+itoa64(t.ID)+"&_wpnonce="+nonce) + `" class="button delete">Delete</a></td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	}
	for _, g := range s.gooTokenGateways() {
		b.WriteString(`<a class="button" href="` + gooEsc(base+"?goo_token_add="+g.ID()+"&_wpnonce="+nonce) + `">Add payment method</a> `)
	}
	return b.String()
}

// gooTokenRoute handles the links of the Payment methods page: add, delete, make default.
func gooTokenRoute(s *Server, c *gin.Context, path string) bool {
	q := c.Request.URL.Query()
	add, del, def := q.Get("goo_token_add"), q.Get("goo_token_delete"), q.Get("goo_token_default")
	if c.Request.Method != http.MethodGet || (add == "" && del == "" && def == "") {
		return false
	}
	ctx := c.Request.Context()
	u := currentUser(c)
	if u == nil {
		return false
	}
	back := s.gooAccountURL(ctx, "payment-methods")
	done := func(typ, msg string) bool {
		s.gooAddNotice(c, typ, msg)
		s.gooRedirect(c, back)
		return true
	}
	if !s.checkNonceValue(c, q.Get("_wpnonce"), "goo-payment-token") {
		return done("error", "Your session has expired. Please reload the page and try again.")
	}
	switch {
	case add != "":
		gw := s.gooGateway(add)
		if gw == nil || !gw.Enabled() {
			return done("error", "Invalid payment method.")
		}
		tg, ok := gw.gooGatewayDef.(gooTokenGateway)
		if !ok {
			return done("error", "Invalid payment method.")
		}
		target, err := tg.SetupURL(s, c, 0)
		if err != nil {
			return done("error", gooEsc(err.Error()))
		}
		s.gooRedirect(c, target)
		return true
	case del != "":
		t := s.gooUserToken(ctx, u.ID, gooAtoi64(del))
		if t == nil {
			return done("error", "Invalid payment method.")
		}
		if tg, ok := gooGatewayRegistry[t.GatewayID].(gooTokenGateway); ok {
			if err := tg.DeleteToken(s, ctx, t); err != nil {
				return done("error", "The payment method could not be deleted: "+gooEsc(err.Error()))
			}
		}
		if err := s.gooDeleteToken(ctx, t); err != nil {
			return done("error", gooEsc(err.Error()))
		}
		// Subscriptions that renewed with it fall back to manual renewals.
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_subscriptions SET meta = meta - '_payment_token' WHERE customer_id = $1 AND meta->>'_payment_token' = $2`, u.ID, t.Token)
		return done("success", "Payment method deleted.")
	default:
		t := s.gooUserToken(ctx, u.ID, gooAtoi64(def))
		if t == nil {
			return done("error", "Invalid payment method.")
		}
		_ = s.gooSetDefaultToken(ctx, t)
		return done("success", "This payment method was successfully set as your default.")
	}
}

// gooSavedTokenFieldsHTML is WC_Payment_Gateway::saved_payment_methods(): the customer's saved methods
// of a gateway as radio buttons, then "Use a new payment method".
func (s *Server) gooSavedTokenFieldsHTML(c *gin.Context, gateway string) string {
	u := currentUser(c)
	if u == nil {
		return ""
	}
	tokens := s.gooUserTokens(c.Request.Context(), u.ID, gateway)
	if len(tokens) == 0 {
		return ""
	}
	name := "wc-" + gateway + "-payment-token"
	var b strings.Builder
	b.WriteString(`<ul class="woocommerce-SavedPaymentMethods wc-saved-payment-methods" data-count="` + fmt.Sprint(len(tokens)) + `">`)
	for i, t := range tokens {
		checked := ""
		if t.Default || (i == 0 && !tokens[0].Default) {
			checked = " checked"
		}
		id := name + "-" + itoa64(t.ID)
		b.WriteString(`<li class="woocommerce-SavedPaymentMethods-token"><input id="` + id + `" type="radio" name="` + name + `" value="` + itoa64(t.ID) +
			`" style="width:auto;" class="woocommerce-SavedPaymentMethods-tokenInput"` + checked + `> <label for="` + id + `">` + gooEsc(t.Label()) + `</label></li>`)
	}
	b.WriteString(`<li class="woocommerce-SavedPaymentMethods-new"><input id="` + name + `-new" type="radio" name="` + name + `" value="new" style="width:auto;" class="woocommerce-SavedPaymentMethods-tokenInput"> ` +
		`<label for="` + name + `-new">Use a new payment method</label></li></ul>`)
	return b.String()
}
