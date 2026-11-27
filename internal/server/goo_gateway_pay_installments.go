package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/pay"
)

// Installments chosen in the store, for bank virtual POS providers (pay.KindBank with
// pay.FeatureInstallments): the bank's hosted page does not ask, so the checkout offers the
// merchant's plan ("count:rate" entries) and sends the chosen count with StartRequest.Installment.
//
// A plan entry's rate is a surcharge in percent. It becomes a cart fee ("Installment surcharge (N
// installments)") while the gateway and the count are chosen, so the order is created with it and
// the order total, which is what the bank charges, already includes it. The rate applies to the whole
// amount the bank captures: items after discounts, other fees and shipping, with their taxes. The fee
// is added by a payment surcharge step of gooCart.Calculate (gooCartSurchargeHooks), after shipping
// and taxes, so it feeds into nothing else (shipping rates, free shipping, coupon minimums) and is
// not taxed.
//
// On the Pay for order page the order already exists: submitting the page with a count replaces the
// order's surcharge line and recalculates the order before the payment starts
// (gooPayOrderInstallmentApply); another gateway or a single payment removes the line.
//
// The Store API (headless clients) has no select: a count in the session is not applied to its
// calculations; its checkout takes an explicit count as payment_data "<gateway>_installment".

func init() {
	gooCartSurchargeHooks = append(gooCartSurchargeHooks, gooPayInstallmentFee)
	gooCheckoutHooks = append(gooCheckoutHooks, gooPayInstallmentCheck)
}

// gooPayInstCurrency is the only currency banks offer installments in.
const gooPayInstCurrency = "TRY"

// gooPayPlanEntry is one entry of an installment plan.
type gooPayPlanEntry struct {
	Count int
	Rate  float64 // percent added to the amount
}

// gooPayPlanParts splits a plan into its entries: new lines, semicolons and commas separate entries,
// except a comma between two digits that is not followed by the next entry's "count:", which is a
// decimal comma ("3:2,5").
func gooPayPlanParts(raw string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ';' }) {
		start := 0
		for i := 0; i < len(line); i++ {
			decimal := i > 0 && i+1 < len(line) && gooPayDigit(line[i-1]) && gooPayDigit(line[i+1]) && !gooPayPlanEntryStart(line[i+1:])
			if line[i] == ',' && !decimal {
				out = append(out, line[start:i])
				start = i + 1
			}
		}
		out = append(out, line[start:])
	}
	parts := out[:0]
	for _, p := range out {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// gooPayPlanEntryStart reports whether s starts with an entry's "count:" (after spaces).
func gooPayPlanEntryStart(s string) bool {
	s = strings.TrimLeft(s, " \t")
	i := 0
	for i < len(s) && gooPayDigit(s[i]) {
		i++
	}
	return i > 0 && strings.HasPrefix(strings.TrimLeft(s[i:], " \t"), ":")
}

func gooPayDigit(b byte) bool { return b >= '0' && b <= '9' }

// gooPayParseEntry reads one "count:rate" entry; problem says what is wrong with it.
func gooPayParseEntry(part string) (e gooPayPlanEntry, problem string) {
	cnt, rate, ok := strings.Cut(part, ":")
	if !ok {
		return e, "is not in the form count:rate"
	}
	n, err := strconv.Atoi(strings.TrimSpace(cnt))
	if err != nil {
		return e, "is not in the form count:rate"
	}
	if n < 2 || n > 12 {
		return e, "has a count outside 2 to 12"
	}
	r, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(rate), ",", "."), 64)
	if err != nil || math.IsNaN(r) || math.IsInf(r, 0) {
		return e, "has a rate that is not a number"
	}
	if r < 0 || r > 100 {
		return e, "has a rate outside 0 to 100"
	}
	return gooPayPlanEntry{Count: n, Rate: r}, ""
}

// gooPayParsePlan reads "2:0, 3:2.5, 6:5.9" (commas, semicolons or new lines between entries; a dot or
// a comma as the decimal mark). Malformed entries, counts outside 2–12, negative rates or rates above
// 100 and repeated counts (the first one wins) are ignored; the settings save refuses them
// (gooPayCheckPlan). The result is sorted by count.
func gooPayParsePlan(raw string) []gooPayPlanEntry {
	var out []gooPayPlanEntry
	seen := map[int]bool{}
	for _, part := range gooPayPlanParts(raw) {
		e, problem := gooPayParseEntry(part)
		if problem != "" || seen[e.Count] {
			continue
		}
		seen[e.Count] = true
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count < out[j].Count })
	return out
}

// gooPayCheckPlan validates a plan as the merchant typed it and returns it normalised ("2:0, 3:2.5"),
// or an error naming the first bad entry.
func gooPayCheckPlan(raw string) (string, error) {
	plan := gooPayParsePlan(raw)
	seen := map[int]bool{}
	for _, part := range gooPayPlanParts(raw) {
		e, problem := gooPayParseEntry(part)
		if problem == "" && seen[e.Count] {
			problem = "repeats the count " + strconv.Itoa(e.Count)
		}
		if problem != "" {
			return "", fmt.Errorf("Installment plan: the entry \"%s\" %s. Write entries as count:rate, for example 3:2.5 (count 2 to 12, rate 0 to 100, each count once).", part, problem)
		}
		seen[e.Count] = true
	}
	parts := make([]string, len(plan))
	for i, e := range plan {
		parts[i] = strconv.Itoa(e.Count) + ":" + strconv.FormatFloat(e.Rate, 'f', -1, 64)
	}
	return strings.Join(parts, ", "), nil
}

// gooPayCheckInstallmentSettings validates the installment settings of a gateway's settings about to
// be saved (st) against the saved ones (old): the plan is normalised; a bad plan or minimum is put
// back to its saved value and reported, the other settings are left as they are.
func (s *Server) gooPayCheckInstallmentSettings(g gooGatewayDef, st, old map[string]any) error {
	pg, _ := g.(*gooPayGateway)
	if pg == nil || !pg.storeInstallments() {
		return nil
	}
	var msgs []string
	restore := func(k string) {
		if v, ok := old[k]; ok {
			st[k] = v
		} else {
			delete(st, k)
		}
	}
	if v, ok := st["installment_plan"]; ok {
		if norm, err := gooPayCheckPlan(gooValStr(v)); err != nil {
			restore("installment_plan")
			msgs = append(msgs, s.gooL10nText(context.Background(), err.Error()))
		} else {
			st["installment_plan"] = norm
		}
	}
	if v, ok := st["installment_min"]; ok {
		if raw := strings.TrimSpace(gooValStr(v)); raw != "" {
			if a := s.gooParseAmount(raw); a == nil || *a < 0 || math.IsNaN(*a) || math.IsInf(*a, 0) {
				restore("installment_min")
				msgs = append(msgs, s.gooL10nf(context.Background(), "Minimum amount for installments: \"%s\" is not an amount. Enter a number of 0 or more, or leave it empty.", raw))
			}
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, " "))
}

// storeInstallments reports whether the installment count is chosen in the store (bank POS).
func (g *gooPayGateway) storeInstallments() bool {
	return g.has(pay.FeatureInstallments) && g.spec.Kind == pay.KindBank
}

// installmentPlan is what the gateway offers for an amount (base, see gooPayInstBase) in a currency:
// nothing when installments are off, the currency is not TRY or the amount is under the minimum.
func (g *gooPayGateway) installmentPlan(s *Server, currency string, base float64) []gooPayPlanEntry {
	if !g.storeInstallments() || s.gooGatewaySetting(g.ID(), "installments") != "yes" || !strings.EqualFold(currency, gooPayInstCurrency) || base <= 0 {
		return nil
	}
	if v := s.gooParseAmount(s.gooGatewaySetting(g.ID(), "installment_min")); v != nil && base < *v {
		return nil
	}
	return gooPayParsePlan(s.gooGatewaySetting(g.ID(), "installment_plan"))
}

// installmentEntry is the plan entry for count n, if the gateway offers it for the amount.
func (g *gooPayGateway) installmentEntry(s *Server, currency string, base float64, n int) (gooPayPlanEntry, bool) {
	if n < 2 {
		return gooPayPlanEntry{}, false
	}
	for _, e := range g.installmentPlan(s, currency, base) {
		if e.Count == n {
			return e, true
		}
	}
	return gooPayPlanEntry{}, false
}

// installmentFields are the settings of store-chosen installments.
func (g *gooPayGateway) storeInstallmentFields() []gooField {
	return []gooField{
		{ID: "installments", Title: "Installments", Type: "checkbox", Default: "no", Desc: "Offer installments",
			Tip: "The customer chooses the number of installments at checkout; the bank's payment page does not ask. Banks offer installments in Turkish lira only."},
		{ID: "installment_plan", Title: "Installment plan", Type: "textarea", Show: "installments", Holder: "2:0, 3:2.5, 6:5.9, 9:8.5, 12:11",
			Desc: "Entries <code>count:rate</code>, separated by commas or new lines. The count is 2 to 12; the rate is the percent added to the order total " +
				"(products after discounts, fees and shipping, with tax), 0 for no surcharge. The surcharge is not taxed. Only entries your bank agreement allows will be approved by the bank."},
		{ID: "installment_min", Title: "Minimum amount for installments", Type: "price", Show: "installments",
			Desc: "Installments are offered when the order total (before the surcharge) is at least this amount. Leave empty for no minimum."},
	}
}

// The surcharge fee -------------------------------------------------------------------------------------

// gooPayInstFeeID is the cart fee id (and so the order fee line's "_fee_id") of a gateway's surcharge.
func gooPayInstFeeID(gateway string, n int) string { return gateway + "_inst_" + strconv.Itoa(n) }

// gooPayInstFeeOf parses a fee id written by gooPayInstFeeID.
func gooPayInstFeeOf(id string) (gateway string, n int, ok bool) {
	i := strings.LastIndex(id, "_inst_")
	if i <= 0 || !strings.HasPrefix(id, "goo_") {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[i+len("_inst_"):])
	if err != nil || n < 2 || n > 12 {
		return "", 0, false
	}
	if _, isPay := gooGatewayRegistry[id[:i]].(*gooPayGateway); !isPay {
		return "", 0, false
	}
	return id[:i], n, true
}

// gooPayInstBase is the amount the surcharge rate applies to: the cart total before payment
// surcharges (items after discounts, other fees, shipping and all taxes).
func gooPayInstBase(cart *gooCart) float64 { return cart.SurchargeBase }

// gooPayInstSurcharge is the surcharge of a plan entry on base, rounded to the store's decimals.
func (s *Server) gooPayInstSurcharge(base float64, e gooPayPlanEntry) float64 {
	if e.Rate <= 0 || base <= 0 {
		return 0
	}
	return gooRound(base*e.Rate/100, s.gooDecimals())
}

// Session keys: the customer's choice, and the surcharge the last shown order review included.
const (
	gooPayInstChoiceKey = "goo_pay_installment"
	gooPayInstShownKey  = "goo_pay_installment_shown"
)

// gooPayInstField is the name of a gateway's installment select.
func gooPayInstField(gateway string) string { return gateway + "_installment" }

// gooPayInstForm is the checkout form posted in this request, nil when the request is not one.
func gooPayInstForm(c *gin.Context) url.Values {
	if c == nil || c.Request == nil {
		return nil
	}
	if f := c.Request.PostForm; f != nil && f.Has("payment_method") {
		return f
	}
	return nil
}

// gooPayInstChoice is the gateway and count the customer chose: from the checkout form posted in this
// request (stored in the session for later calculations), else from the session.
func gooPayInstChoice(cart *gooCart) (string, int) {
	sess := cart.sess
	if f := gooPayInstForm(cart.c); f != nil {
		gw := f.Get("payment_method")
		n, _ := strconv.Atoi(strings.TrimSpace(f.Get(gooPayInstField(gw))))
		if sess.Data.Extra == nil {
			sess.Data.Extra = map[string]any{}
		}
		v := gw + ":" + strconv.Itoa(n)
		if old, _ := sess.Data.Extra[gooPayInstChoiceKey].(string); old != v {
			sess.Data.Extra[gooPayInstChoiceKey] = v
			sess.touch()
		}
		return gw, n
	}
	if cart.c != nil && cart.c.GetBool(gooStoreAPIKey) {
		// The Store API shows no select, so a count chosen on the checkout page is not applied silently.
		if _, ok := sess.Data.Extra[gooPayInstChoiceKey]; ok {
			delete(sess.Data.Extra, gooPayInstChoiceKey)
			sess.touch()
		}
		return sess.Data.ChosenPayment, 0
	}
	v, _ := sess.Data.Extra[gooPayInstChoiceKey].(string)
	gw, cnt, _ := strings.Cut(v, ":")
	if gw == "" || gw != sess.Data.ChosenPayment {
		return sess.Data.ChosenPayment, 0
	}
	n, _ := strconv.Atoi(cnt)
	return gw, n
}

// gooPayInstallmentFee adds the surcharge of the chosen installment count to the cart.
func gooPayInstallmentFee(s *Server, ctx context.Context, cart *gooCart) {
	if cart.sess == nil {
		return
	}
	gw, n := gooPayInstChoice(cart)
	shown := ""
	if g, _ := gooGatewayRegistry[gw].(*gooPayGateway); g != nil && n >= 2 && gooPayCartHasGoods(cart) {
		base := gooPayInstBase(cart)
		if e, ok := g.installmentEntry(s, s.gooCurrency(), base, n); ok {
			if fee := s.gooPayInstSurcharge(base, e); fee > 0 {
				cart.AddFee(gooPayInstFeeID(gw, n), s.gooL10nf(ctx, "Installment surcharge (%s installments)", strconv.Itoa(n)), fee, false, "")
				shown = gooPayInstFeeID(gw, n) + ":" + strconv.FormatFloat(fee, 'f', -1, 64)
			}
		}
	}
	// Any calculation other than the order submission itself is what the customer is shown (the
	// checkout page, its order review refresh): remember which surcharge it included.
	if f := gooPayInstForm(cart.c); f == nil || !f.Has("woocommerce-process-checkout-nonce") {
		if cart.sess.Data.Extra == nil {
			cart.sess.Data.Extra = map[string]any{}
		}
		if old, _ := cart.sess.Data.Extra[gooPayInstShownKey].(string); old != shown {
			cart.sess.Data.Extra[gooPayInstShownKey] = shown
			cart.sess.touch()
		}
	}
}

// gooPayCartHasGoods reports whether the cart has something to pay while fees are added.
func gooPayCartHasGoods(cart *gooCart) bool {
	return cart.CartContentsTotal+cart.CartContentsTax > 0
}

// gooPayInstallmentCheck refuses an order submission whose surcharge the customer was not shown (the
// payment method or count changed without the order review being refreshed). The failed submission
// refreshes the review, so the next one goes through with the total on screen.
func gooPayInstallmentCheck(s *Server, ctx context.Context, cart *gooCart, o *gooOrder) error {
	if cart == nil {
		return nil
	}
	cur := ""
	for _, f := range cart.Fees {
		if _, _, ok := gooPayInstFeeOf(f.ID); ok {
			cur = f.ID + ":" + strconv.FormatFloat(f.Total, 'f', -1, 64)
		}
	}
	if cur == "" || cart.sess == nil {
		return nil
	}
	if shown, _ := cart.sess.Data.Extra[gooPayInstShownKey].(string); shown == cur {
		return nil
	}
	return fmt.Errorf("%s", s.gooPayT(ctx, "The installment surcharge changed your order total. Please check the total and place the order again."))
}

// The choice at checkout and on the Pay for order page ----------------------------------------------------

// installmentSelect is the "Installments" select of PaymentFields: the checkout's (with surcharges) or
// the Pay for order page's (rate 0 only). Empty when nothing is offered.
func (g *gooPayGateway) installmentSelect(s *Server, c *gin.Context) string {
	if !g.storeInstallments() {
		return ""
	}
	ctx := c.Request.Context()
	type opt struct {
		n     int
		label string
		ok    bool
	}
	var opts []opt
	price := func(v float64, cur string) string { return html.UnescapeString(s.gooPricePlain(v, cur)) }
	label := func(n int, total float64, cur string) string {
		return fmt.Sprintf("%d × %s = %s", n, price(gooRound(total/float64(n), s.gooDecimals()), cur), price(total, cur))
	}
	chosen := 0
	if fv := gooViewFrom(c); fv != nil && fv.Endpoint == "order-pay" {
		o, err := s.gooGetOrder(ctx, gooAtoi64(fv.Value))
		if err != nil || !gooOrderKeyMatches(o.OrderKey, c.Query("key")) {
			return ""
		}
		if len(o.Items) == 0 {
			o.Items, _ = s.gooOrderItems(ctx, o.ID)
		}
		// The labels show the total the order will have with each count: submitting the page sets the
		// order's surcharge line to it (gooPayOrderInstallmentApply) before the payment starts.
		base := gooPayOrderInstBase(o)
		for _, e := range g.installmentPlan(s, o.Currency, base) {
			opts = append(opts, opt{n: e.Count, label: label(e.Count, gooRound(base+s.gooPayInstSurcharge(base, e), s.gooDecimals()), o.Currency), ok: true})
		}
		if gw, n, ok := gooPayOrderInstFee(o); ok && gw == g.ID() {
			chosen = n
		} else {
			chosen, _ = strconv.Atoi(gooPayPosted(c, gooPayInstField(g.ID())))
		}
	} else {
		cart := s.gooCartFor(c)
		cur := s.gooCurrency()
		base := gooPayInstBase(cart)
		for _, e := range g.installmentPlan(s, cur, base) {
			opts = append(opts, opt{n: e.Count, label: label(e.Count, gooRound(base+s.gooPayInstSurcharge(base, e), s.gooDecimals()), cur), ok: true})
		}
		if gw, n := gooPayInstChoice(cart); gw == g.ID() {
			chosen = n
		}
	}
	if len(opts) == 0 {
		return ""
	}
	name := gooPayInstField(g.ID())
	var b strings.Builder
	// The row is an "address-field" so the checkout script refreshes the order review (and so the
	// total with the surcharge) when the choice changes.
	b.WriteString(`<p class="form-row form-row-wide address-field update_totals_on_change goo-pay-installments"><label for="` + gooEsc(name) + `">` +
		gooEsc(s.gooPayT(ctx, "Installments")) + `</label><select name="` + gooEsc(name) + `" id="` + gooEsc(name) + `">`)
	b.WriteString(`<option value="0">` + gooEsc(s.gooPayT(ctx, "Single payment")) + `</option>`)
	for _, o := range opts {
		sel := ""
		if o.n == chosen {
			sel = ` selected="selected"`
		}
		b.WriteString(`<option value="` + strconv.Itoa(o.n) + `"` + sel + `>` + gooEsc(o.label) + `</option>`)
	}
	b.WriteString(`</select></p>`)
	return b.String()
}

// gooPayOrderInstFee is the gateway and count of the order's installment surcharge line.
func gooPayOrderInstFee(o *gooOrder) (string, int, bool) {
	for _, it := range o.Items {
		if it.Type == "fee" {
			if gw, n, ok := gooPayInstFeeOf(gxaStr(it.Meta, "_fee_id")); ok {
				return gw, n, true
			}
		}
	}
	return "", 0, false
}

// gooPayOrderInstBase is gooPayInstBase of an order: its total without its surcharge lines.
func gooPayOrderInstBase(o *gooOrder) float64 {
	base := o.Total
	for _, it := range o.Items {
		if it.Type == "fee" {
			if _, _, ok := gooPayInstFeeOf(gxaStr(it.Meta, "_fee_id")); ok {
				base -= it.Total + it.TotalTax
			}
		}
	}
	return max(base, 0)
}

// gooPayOrderInstallmentApply gives an order paid on the Pay for order page the surcharge line of the
// count posted for gateway (none for a single payment, a 0-rate count or another gateway), replacing
// any earlier one, and recalculates the order. It runs under the order's lock and only while the order
// still awaits payment; o is refreshed with the saved order, so the payment starts with its total.
func (s *Server) gooPayOrderInstallmentApply(c *gin.Context, o *gooOrder, gateway string) error {
	failed := func(ctx context.Context, err error) error {
		log.Printf("goocommerce: order %d: setting the installment surcharge: %q", o.ID, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		return errors.New(s.gooPayT(ctx, "There was an error processing your order. Please try again."))
	}
	return gooWithOrderLock(c.Request.Context(), o.ID, func(ctx context.Context) error {
		cur, err := s.gooGetOrder(ctx, o.ID)
		if err != nil {
			return failed(ctx, err)
		}
		if (cur.Status != "pending" && cur.Status != "failed") || cur.OrderKey != o.OrderKey || cur.Total <= 0 {
			return errors.New(s.gooPayT(ctx, "This order cannot be paid for."))
		}
		if len(cur.Items) == 0 {
			if cur.Items, err = s.gooOrderItems(ctx, cur.ID); err != nil {
				return failed(ctx, err)
			}
		}
		base := gooPayOrderInstBase(cur)
		wantID, fee, n := "", 0.0, 0
		if g, _ := gooGatewayRegistry[gateway].(*gooPayGateway); g != nil && g.storeInstallments() {
			n, _ = strconv.Atoi(strings.TrimSpace(gooPayPosted(c, gooPayInstField(gateway))))
			if e, ok := g.installmentEntry(s, cur.Currency, base, n); ok {
				if fee = s.gooPayInstSurcharge(base, e); fee > 0 {
					wantID = gooPayInstFeeID(gateway, n)
				}
			}
		}
		keep := make([]*gooOrderItem, 0, len(cur.Items))
		var had []*gooOrderItem
		for _, it := range cur.Items {
			if _, _, ok := gooPayInstFeeOf(gxaStr(it.Meta, "_fee_id")); ok && it.Type == "fee" {
				had = append(had, it)
			} else {
				keep = append(keep, it)
			}
		}
		if (wantID == "" && len(had) == 0) || (len(had) == 1 && wantID != "" && gxaStr(had[0].Meta, "_fee_id") == wantID && had[0].Total == fee && had[0].TotalTax == 0) {
			*o = *cur
			return nil
		}
		cur.Items = keep
		if wantID != "" {
			cur.Items = append(cur.Items, &gooOrderItem{Type: "fee", Name: s.gooL10nf(ctx, "Installment surcharge (%s installments)", strconv.Itoa(n)),
				Quantity: 1, Total: fee, Meta: map[string]any{"_fee_id": wantID}})
		}
		s.gooRecalculate(cur)
		if err := s.gooSaveOrder(ctx, cur, true); err != nil {
			return failed(ctx, err)
		}
		*o = *cur
		return nil
	})
}

// orderInstallment is the installment count to send for an order (0: single payment). A surcharge
// line on the order names it; without one, the posted count must be a 0-rate entry of the plan (a
// count with a surcharge has its line by now: gooPayOrderInstallmentApply). The plan, the minimum and
// the currency are checked again either way.
func (g *gooPayGateway) orderInstallment(s *Server, c *gin.Context, o *gooOrder) int {
	if !g.storeInstallments() {
		return 0
	}
	base := gooPayOrderInstBase(o)
	if gw, n, ok := gooPayOrderInstFee(o); ok {
		if gw != g.ID() {
			return 0
		}
		if _, ok := g.installmentEntry(s, o.Currency, base, n); !ok {
			log.Printf("goocommerce: %s: order %d carries a surcharge for %d installments the plan no longer offers; paying in one", g.ID(), o.ID, n) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
			return 0
		}
		return n
	}
	n, _ := strconv.Atoi(strings.TrimSpace(gooPayPosted(c, gooPayInstField(g.ID()))))
	if e, ok := g.installmentEntry(s, o.Currency, base, n); ok && e.Rate == 0 {
		return n
	}
	return 0
}

// gooPayPosted is a posted value, read from the request's parsed form first (the checkout's AJAX
// post_data is merged into it), then through gin.
func gooPayPosted(c *gin.Context, key string) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if f := c.Request.PostForm; f != nil && f.Has(key) {
		return f.Get(key)
	}
	return c.PostForm(key)
}
