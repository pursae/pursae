package server

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"wpgo/internal/pay"
)

func TestGooPayCheckPlan(t *testing.T) {
	for raw, want := range map[string]string{
		"":                           "",
		"6:5.9, 2:0\n3:2.5":          "2:0, 3:2.5, 6:5.9",
		"3:2,5, 6:5,9; 9 : 8.50":     "3:2.5, 6:5.9, 9:8.5",
		" 12:11 ,2:0,3:100":          "2:0, 3:100, 12:11",
		"2:0,\n\n4:1.25,":            "2:0, 4:1.25",
		"3:2,5,4:3":                  "3:2.5, 4:3",
		"10:0.0, 11:007, 2:0.000001": "2:0.000001, 10:0, 11:7",
	} {
		got, err := gooPayCheckPlan(raw)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", raw, got, err, want)
		}
	}
	for raw, entry := range map[string]string{
		"2:0, 3":        `"3" is not in the form`,
		"2:0, x:1":      `"x:1" is not in the form`,
		"1:0":           `"1:0" has a count outside 2 to 12`,
		"2:0, 13:1":     `"13:1" has a count outside`,
		"3:-1":          `"3:-1" has a rate outside 0 to 100`,
		"3:101":         `"3:101" has a rate outside`,
		"3:abc":         `"3:abc" has a rate that is not a number`,
		"3:NaN":         `"3:NaN" has a rate that is not a number`,
		"2:0, 3:1, 3:2": `"3:2" repeats the count 3`,
	} {
		if _, err := gooPayCheckPlan(raw); err == nil || !strings.Contains(err.Error(), entry) {
			t.Errorf("%q: %v, want %s", raw, err, entry)
		}
	}
}

func TestGooPayInstallmentSettingsSave(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	_, g := gooFakeBankRegister(t, srv, "2:0, 3:10", "100")
	saved := func() map[string]any { return srv.gooOptMap(gooFakeBankID + "_settings") }
	post := func(plan, minimum string) error {
		c := gooTestCtx()
		c.Request.PostForm = url.Values{
			"woocommerce_goo_fakebank_enabled": {"1"}, "woocommerce_goo_fakebank_title": {"New title"},
			"woocommerce_goo_fakebank_installments": {"1"}, "woocommerce_goo_fakebank_installment_plan": {plan},
			"woocommerce_goo_fakebank_installment_min": {minimum},
		}
		return srv.gooGatewayPost(c, gooFakeBankID)
	}

	// A good plan is stored normalised.
	if err := post("6:5,9\n3:2.5, 2:0", "250"); err != nil {
		t.Fatal(err)
	}
	if st := saved(); gooValStr(st["installment_plan"]) != "2:0, 3:2.5, 6:5.9" || gooValStr(st["installment_min"]) != "250" || gooValStr(st["title"]) != "New title" {
		t.Errorf("saved: %v", st)
	}

	// A bad plan or minimum keeps the saved value and is reported; the other fields are saved.
	err := post("2:0, 3:2.5, 20:1", "-5")
	if err == nil || !strings.Contains(err.Error(), `"20:1"`) || !strings.Contains(err.Error(), "Minimum amount") {
		t.Fatalf("bad input: %v", err)
	}
	if st := saved(); gooValStr(st["installment_plan"]) != "2:0, 3:2.5, 6:5.9" || gooValStr(st["installment_min"]) != "250" {
		t.Errorf("bad values saved: %v", st)
	}
	if err := post("2:0", "abc"); err == nil || !strings.Contains(err.Error(), `"abc" is not an amount`) {
		t.Errorf("bad minimum: %v", err)
	}
	if st := saved(); gooValStr(st["installment_plan"]) != "2:0" || gooValStr(st["installment_min"]) != "250" {
		t.Errorf("plan with a bad minimum: %v", st)
	}

	// The check itself, as the REST update uses it: a gateway without store installments is untouched.
	st := map[string]any{"installment_plan": "3:x"}
	if err := srv.gooPayCheckInstallmentSettings(g, st, map[string]any{}); err == nil || st["installment_plan"] != nil {
		t.Errorf("rest check: %v %v", err, st)
	}
	_, psp := gooFakePayRegister(t, srv)
	st = map[string]any{"installment_plan": "3:x"}
	if err := srv.gooPayCheckInstallmentSettings(psp, st, nil); err != nil || st["installment_plan"] != "3:x" {
		t.Errorf("psp: %v %v", err, st)
	}
	_ = ctx
}

// gooFakeBankShipping is a TRY store with 10% tax on items and shipping in CA and a flat rate of 10.
func gooFakeBankShipping(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	if err := srv.gooSetOpts(ctx, map[string]any{"calc_taxes": true, "default_country": "US:CA"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.gooSetOpts(ctx, map[string]any{"calc_taxes": false}) })
	if err := srv.gooSaveTaxRate(ctx, &gooTaxRate{Country: "US", State: "CA", Rate: 10, Name: "CA Tax", Priority: 1, Shipping: true}); err != nil {
		t.Fatal(err)
	}
	z := &gooZone{Name: "US", Locations: []gooZoneLocation{{Code: "US", Type: "country"}}}
	if err := srv.gooSaveZone(ctx, z); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.gooAddMethod(ctx, z.ID, "flat_rate", map[string]any{"cost": "10"}); err != nil {
		t.Fatal(err)
	}
}

// gooFakeBankShipCart is a fresh customer in CA with 2 × 100.00 of a shipped product.
func gooFakeBankShipCart(t *testing.T, srv *Server, name string) *gin.Context {
	t.Helper()
	p := gooMakeSimple(t, srv, name, 100, nil)
	c := gooTestCtx()
	sess := srv.gooSessionFor(c)
	sess.Data.Customer.Billing = map[string]string{"country": "US", "state": "CA", "postcode": "90210"}
	sess.Data.Customer.Shipping = map[string]string{"country": "US", "state": "CA", "postcode": "90210"}
	if _, err := srv.gooCartFor(c).Add(context.Background(), p.ID, 2, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGooPayInstallmentShipping(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	bank, _ := gooFakeBankRegister(t, srv, "2:0, 3:10", "")
	gooFakeBankShipping(t, srv)

	// Items 200 + tax 20, shipping 10 + tax 1: the 10% surcharge is on 231.
	c := gooFakeBankShipCart(t, srv, "Ship Lamp")
	cart := gooFakeBankReview(srv, c, gooFakeBankForm(gooFakeBankID, 3, false))
	f := gooFakeBankFee(cart)
	if f == nil || f.Total != 23.1 || f.Tax != 0 || cart.ShippingTotal != 10 || cart.ShippingTax != 1 || cart.TotalTax != 21 || cart.Total != 254.1 {
		t.Fatalf("cart: fee %+v shipping %v+%v tax %v total %v", f, cart.ShippingTotal, cart.ShippingTax, cart.TotalTax, cart.Total)
	}
	// The Store API's cart totals agree.
	tot := srv.gooStoreCartJSON(c, cart)["totals"].(map[string]any)
	if tot["total_price"] != "25410" || tot["total_fees"] != "2310" || tot["total_fees_tax"] != "0" || tot["total_tax"] != "2100" {
		t.Errorf("store api totals: %v", tot)
	}

	res, o, errs := gooFakeBankSubmit(srv, c, gooFakeBankForm(gooFakeBankID, 3, true))
	if o == nil || len(errs) > 0 || res == nil || res.Result != "success" {
		t.Fatalf("checkout: %+v %v", res, errs)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	fees := o.ItemsOf("fee")
	if o.Total != 254.1 || o.TotalTax != 21 || o.ShippingTotal != 10 || len(fees) != 1 || fees[0].Total != 23.1 || fees[0].TotalTax != 0 {
		t.Fatalf("order: total %v tax %v shipping %v fees %+v", o.Total, o.TotalTax, o.ShippingTotal, fees)
	}
	if st := bank.lastStart; st.Amount != 25410 || st.Installment != 3 {
		t.Errorf("start: %d × %d", st.Amount, st.Installment)
	}
	// The order's own totals are consistent with its lines.
	cp := *o
	srv.gooRecalculate(&cp)
	if cp.Total != o.Total || cp.TotalTax != o.TotalTax {
		t.Errorf("recalculated: %v %v", cp.Total, cp.TotalTax)
	}

	// The Store API does not apply a count from the session silently.
	c2 := gooFakeBankShipCart(t, srv, "Ship Rug")
	gooFakeBankReview(srv, c2, gooFakeBankForm(gooFakeBankID, 3, false))
	c2.Request.PostForm = nil
	c2.Set(gooStoreAPIKey, true)
	cart = srv.gooCartFor(c2)
	cart.Calculate()
	if gooFakeBankFee(cart) != nil || cart.Total != 231 {
		t.Errorf("store api with a session count: total %v", cart.Total)
	}
	if _, ok := srv.gooSessionFor(c2).Data.Extra[gooPayInstChoiceKey]; ok {
		t.Error("the session count is kept")
	}
}

func TestGooPayOrderPayInstallments(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	bank, g := gooFakeBankRegister(t, srv, "2:0, 3:10, 6:20", "")
	gooFakeBankShipping(t, srv)

	// A single-payment order of 231 (items, shipping, taxes).
	c := gooFakeBankShipCart(t, srv, "Pay Lamp")
	gooFakeBankReview(srv, c, gooFakeBankForm(gooFakeBankID, 0, false))
	_, o, errs := gooFakeBankSubmit(srv, c, gooFakeBankForm(gooFakeBankID, 0, true))
	if o == nil || len(errs) > 0 {
		t.Fatalf("checkout: %v", errs)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	if o.Total != 231 {
		t.Fatalf("order total %v", o.Total)
	}

	// The Pay for order select offers the whole plan, with the totals the order will have.
	pc := gooTestCtx()
	pc.Request = pc.Request.WithContext(ctx)
	pc.Request.URL.RawQuery = "key=" + o.OrderKey
	pc.Set(gooCtxView, &gooFrontView{Endpoint: "order-pay", Value: itoa64(o.ID)})
	sel := g.installmentSelect(srv, pc)
	if !strings.Contains(sel, `value="3"`) || !strings.Contains(sel, `value="6"`) || !strings.Contains(sel, "254.10") || !strings.Contains(sel, "277.20") {
		t.Errorf("select: %s", sel)
	}

	pay := func(gateway string, n string) *gooOrder {
		t.Helper()
		pc := gooTestCtx()
		pc.Request.PostForm = url.Values{"payment_method": {gateway}, gateway + "_installment": {n}}
		cur, _ := srv.gooGetOrder(ctx, o.ID)
		if err := srv.gooPayOrderInstallmentApply(pc, cur, gateway); err != nil {
			t.Fatalf("apply %s %s: %v", gateway, n, err)
		}
		if gateway == gooFakeBankID {
			if res, err := g.ProcessPayment(srv, pc, cur); err != nil || res.Result != "success" {
				t.Fatalf("process: %+v %v", res, err)
			}
		}
		cur, _ = srv.gooGetOrder(ctx, o.ID)
		return cur
	}
	instFees := func(o *gooOrder) (n int, total float64) {
		for _, it := range o.ItemsOf("fee") {
			if _, _, ok := gooPayInstFeeOf(gxaStr(it.Meta, "_fee_id")); ok {
				n++
				total += it.Total
			}
		}
		return
	}

	// 3 installments: the order gets the fee line and the new total; Start is sent that amount.
	o2 := pay(gooFakeBankID, "3")
	if n, fee := instFees(o2); n != 1 || fee != 23.1 || o2.Total != 254.1 || o2.TotalTax != 21 {
		t.Fatalf("3: %d fees %v, total %v tax %v", n, fee, o2.Total, o2.TotalTax)
	}
	if st := bank.lastStart; st.Amount != 25410 || st.Installment != 3 {
		t.Errorf("start 3: %d × %d", st.Amount, st.Installment)
	}
	// The page now shows the order with the fee, and the labels still show the right totals.
	if sel := g.installmentSelect(srv, pc); !strings.Contains(sel, `value="3" selected`) || !strings.Contains(sel, "277.20") || !strings.Contains(sel, "254.10") {
		t.Errorf("select after 3: %s", sel)
	}
	// 6 installments: one fee line only, on the same base.
	o2 = pay(gooFakeBankID, "6")
	if n, fee := instFees(o2); n != 1 || fee != 46.2 || o2.Total != 277.2 {
		t.Fatalf("6: %d fees %v, total %v", n, fee, o2.Total)
	}
	if st := bank.lastStart; st.Amount != 27720 || st.Installment != 6 {
		t.Errorf("start 6: %d × %d", st.Amount, st.Installment)
	}
	// 2 installments at rate 0: no fee, the count is still sent.
	o2 = pay(gooFakeBankID, "2")
	if n, _ := instFees(o2); n != 0 || o2.Total != 231 || bank.lastStart.Installment != 2 || bank.lastStart.Amount != 23100 {
		t.Fatalf("2: %d fees, total %v, start %d × %d", n, o2.Total, bank.lastStart.Amount, bank.lastStart.Installment)
	}
	// Single payment and another gateway remove the fee.
	pay(gooFakeBankID, "3")
	if o2 = pay(gooFakeBankID, "0"); o2.Total != 231 || bank.lastStart.Installment != 0 {
		t.Errorf("single payment: total %v installment %d", o2.Total, bank.lastStart.Installment)
	}
	pay(gooFakeBankID, "3")
	if o2 = pay("cod", "3"); o2.Total != 231 {
		t.Errorf("another gateway: total %v", o2.Total)
	}
	if n, _ := instFees(o2); n != 0 {
		t.Errorf("another gateway: %d fee lines", n)
	}

	// A paid order is not touched.
	o2.Status = "processing"
	if err := srv.gooSaveOrder(ctx, o2, false); err != nil {
		t.Fatal(err)
	}
	pc2 := gooTestCtx()
	pc2.Request.PostForm = url.Values{"payment_method": {gooFakeBankID}, gooFakeBankID + "_installment": {"3"}}
	paid, _ := srv.gooGetOrder(ctx, o.ID)
	if err := srv.gooPayOrderInstallmentApply(pc2, paid, gooFakeBankID); err == nil {
		t.Error("a paid order was accepted")
	}
	if paid, _ = srv.gooGetOrder(ctx, o.ID); paid.Total != 231 || len(paid.ItemsOf("fee")) != 0 {
		t.Errorf("paid order changed: total %v", paid.Total)
	}
}

// A payment started on the Pay for order page before the customer picked another installment count is
// accepted on its own count: the order's surcharge line goes back to it. Anything else stays on hold.
func TestGooPayOrderPayStaleInstallmentAttempt(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	_, g := gooFakeBankRegister(t, srv, "2:0, 3:10, 6:20", "")
	gooFakeBankShipping(t, srv)

	newOrder := func(name string) *gooOrder {
		t.Helper()
		c := gooFakeBankShipCart(t, srv, name)
		gooFakeBankReview(srv, c, gooFakeBankForm(gooFakeBankID, 0, false))
		_, o, errs := gooFakeBankSubmit(srv, c, gooFakeBankForm(gooFakeBankID, 0, true))
		if o == nil || len(errs) > 0 {
			t.Fatalf("checkout: %v", errs)
		}
		return o
	}
	start := func(o *gooOrder, n string) *gooPayAttempt {
		t.Helper()
		pc := gooTestCtx()
		pc.Request.PostForm = url.Values{"payment_method": {gooFakeBankID}, gooFakeBankID + "_installment": {n}}
		cur, _ := srv.gooGetOrder(ctx, o.ID)
		if err := srv.gooPayOrderInstallmentApply(pc, cur, gooFakeBankID); err != nil {
			t.Fatalf("apply %s: %v", n, err)
		}
		if res, err := g.ProcessPayment(srv, pc, cur); err != nil || res.Result != "success" {
			t.Fatalf("process: %+v %v", res, err)
		}
		as := gooFakePayAttempts(t, srv, o.ID)
		return as[len(as)-1]
	}
	paid := func(a *gooPayAttempt, amount int64) {
		t.Helper()
		if err := srv.gooPayLocked(ctx, a, func(ctx context.Context, db gooPayDB, a *gooPayAttempt) error {
			return srv.gooPayApply(ctx, db, g, a, &pay.Result{Status: pay.StatusSuccess, TxnID: "txn_" + a.Ref, PaidAmount: amount, Currency: a.Currency})
		}); err != nil {
			t.Fatalf("apply payment: %v", err)
		}
	}
	instFee := func(o *gooOrder) (n int, fee float64) {
		for _, it := range o.ItemsOf("fee") {
			if _, cnt, ok := gooPayInstFeeOf(gxaStr(it.Meta, "_fee_id")); ok {
				n, fee = cnt, fee+it.Total
			}
		}
		return
	}

	// 3 installments started, then 6 chosen; the bank page of 3 is paid.
	o := newOrder("Stale Lamp")
	a3 := start(o, "3")
	start(o, "6")
	if cur, _ := srv.gooGetOrder(ctx, o.ID); cur.Total != 277.2 {
		t.Fatalf("total after 6: %v", cur.Total)
	}
	paid(a3, a3.Amount)
	got, _ := srv.gooGetOrder(ctx, o.ID)
	if n, fee := instFee(got); !got.IsPaid() || got.Total != 254.1 || n != 3 || fee != 23.1 {
		t.Errorf("paid on 3: paid %v status %s total %v, fee %d × %v", got.IsPaid(), got.Status, got.Total, n, fee)
	}
	if gooFakePayNotes(t, srv, o.ID, "set back to 3 installments") != 1 {
		t.Error("no note about the surcharge")
	}

	// A single payment started, then 3 installments chosen: the single payment is accepted without a fee.
	o = newOrder("Stale Rug")
	a0 := start(o, "0")
	start(o, "3")
	paid(a0, a0.Amount)
	got, _ = srv.gooGetOrder(ctx, o.ID)
	if n, _ := instFee(got); !got.IsPaid() || got.Total != 231 || n != 0 {
		t.Errorf("paid single: paid %v status %s total %v, fee lines %d", got.IsPaid(), got.Status, got.Total, n)
	}

	// The plan no longer offers the attempt's count: held for review, the order is not rewritten.
	o = newOrder("Stale Chair")
	a6 := start(o, "6")
	start(o, "3")
	gooFakeBankRegister(t, srv, "2:0, 3:10", "")
	paid(a6, a6.Amount)
	got, _ = srv.gooGetOrder(ctx, o.ID)
	if n, _ := instFee(got); got.IsPaid() || got.Status != "on-hold" || n != 3 {
		t.Errorf("plan changed: paid %v status %s, fee count %d", got.IsPaid(), got.Status, n)
	}
}
