package server

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// A cancelled order's redeemed points are returned; paying it late takes them back, and an order whose
// points were spent meanwhile is held instead of paid.
func TestT2OrdersLatePayPoints(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	gxaOn(t, srv, gptsKey, true)
	u := gxaCustomer(t, srv, "late-points@example.test")
	start := srv.gptsBalance(ctx, u.ID)
	if err := srv.gptsLog(ctx, u.ID, 100, "admin-adjustment", 0, "test"); err != nil {
		t.Fatal(err)
	}
	p := gooMakeSimple(t, srv, "Points Mug", 10, func(p *gooProduct) { p.Virtual = true })
	o := gooTestOrder(t, srv, p, 1)
	o.CustomerID = u.ID
	o.Meta["_wc_points_redeemed"] = float64(40)
	o.Meta["_wc_points_logged_redemption"] = true
	if err := srv.gooSaveOrder(ctx, o, false); err != nil {
		t.Fatal(err)
	}
	_ = srv.gptsLog(ctx, u.ID, -40, "order-redeem", o.ID, "")
	bal := func() int { return srv.gptsBalance(ctx, u.ID) - start }

	if err := srv.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "cancelled", "Order cancelled by customer.", ""); err != nil {
		t.Fatal(err)
	}
	if bal() != 100 {
		t.Fatalf("cancel returned the points: balance %d", bal())
	}
	if err := srv.gooPaymentComplete(ctx, o, "late_1"); err != nil {
		t.Fatal(err)
	}
	got, _ := srv.gooGetOrder(ctx, o.ID)
	// 60 left after the debit, plus what the paid order earned.
	if got.Status != "processing" || bal() != 60+gxaInt(got.Meta, "_wc_points_earned") || got.Meta["_wc_points_redeem_refunded"] != nil {
		t.Fatalf("late payment: status %s, balance %d, meta %v", got.Status, bal(), got.Meta["_wc_points_redeem_refunded"])
	}

	// Cancelled again, the points spent elsewhere, then paid late: held, nothing double spent.
	if err := srv.gooSetStatus(ctx, got, "cancelled", "", ""); err != nil {
		t.Fatal(err)
	}
	if bal() != 100 {
		t.Fatalf("second cancel: balance %d", bal())
	}
	_ = srv.gptsLog(ctx, u.ID, -80, "order-redeem", 0, "")
	if err := srv.gooPaymentComplete(ctx, got, "late_2"); err != nil {
		t.Fatal(err)
	}
	got, _ = srv.gooGetOrder(ctx, o.ID)
	if got.Status != "on-hold" || bal() != 20 || got.Meta["_wc_points_redeem_refunded"] == nil {
		t.Fatalf("short balance: status %s, balance %d", got.Status, bal())
	}
	// Topped up, the merchant's change takes the points and goes through.
	_ = srv.gptsLog(ctx, u.ID, 30, "admin-adjustment", 0, "")
	if err := srv.gooSetStatus(ctx, got, "processing", "", "1"); err != nil {
		t.Fatal(err)
	}
	got, _ = srv.gooGetOrder(ctx, o.ID)
	if got.Status != "processing" || bal() != 10 {
		t.Fatalf("admin reactivation: status %s, balance %d", got.Status, bal())
	}
}

// A gift card credited back by a cancellation and spent elsewhere cannot pay the cancelled order again.
func TestT2OrdersLatePayGiftCard(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	if err := srv.gooSetExtension(ctx, gcKey, true); err != nil {
		t.Fatal(err)
	}
	g := &gcCard{Initial: 50, Balance: 50}
	if err := srv.gcCreate(ctx, g, 0, "test"); err != nil {
		t.Fatal(err)
	}
	balance := func() float64 { c, _ := srv.gcGet(ctx, g.ID); return c.Balance }
	p := gooMakeSimple(t, srv, "Card Mug", 10, func(p *gooProduct) { p.Virtual = true })

	use := func() *gooOrder {
		o := gooTestOrder(t, srv, p, 1)
		c, _ := srv.gcGet(ctx, g.ID)
		if err := srv.gcAdjust(ctx, c, -20, "used", o.ID, 0, "test"); err != nil {
			t.Fatal(err)
		}
		return o
	}

	// Normal flow: cancelled then paid late with the balance still there, it is debited again.
	o := use()
	if err := srv.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "cancelled", "", ""); err != nil {
		t.Fatal(err)
	}
	if balance() != 50 {
		t.Fatalf("cancel credit: %v", balance())
	}
	if err := srv.gooPaymentComplete(ctx, o, "late_gc_1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := srv.gooGetOrder(ctx, o.ID); got.Status != "processing" || balance() != 30 {
		t.Fatalf("late payment: %s, balance %v", got.Status, balance())
	}

	// Spent elsewhere after the cancellation: the late payment holds the order.
	o2 := use()
	if err := srv.gooSetStatusFrom(ctx, o2, gooNeedsPaymentStatuses, "cancelled", "", ""); err != nil {
		t.Fatal(err)
	}
	if balance() != 30 {
		t.Fatalf("second cancel credit: %v", balance())
	}
	c, _ := srv.gcGet(ctx, g.ID)
	_ = srv.gcAdjust(ctx, c, -25, "used", 0, 0, "spent elsewhere")
	if err := srv.gooPaymentComplete(ctx, o2, "late_gc_2"); err != nil {
		t.Fatal(err)
	}
	got, _ := srv.gooGetOrder(ctx, o2.ID)
	if got.Status != "on-hold" || balance() != 5 {
		t.Fatalf("spent card: status %s, balance %v", got.Status, balance())
	}
	if len(gcStatusCredit(got)) == 0 {
		t.Error("the unrecovered credit was dropped")
	}
}

// The hosted Checkout page expires, and cancelling the order closes it.
func TestT2OrdersStripeSessionExpiry(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	fake := newGooFakeStripe(t)
	gooStripeEnable(t, srv)
	p := gooMakeSimple(t, srv, "Stripe Mug", 12, func(p *gooProduct) { p.Virtual = true })
	c := gooTestCtx()
	if _, err := srv.gooCartFor(c).Add(ctx, p.ID, 1, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	form := map[string][]string{}
	for k, v := range gxCheckoutForm {
		form[k] = v
	}
	form["payment_method"] = []string{gooStripeID}
	res, o, errs := srv.gooProcessCheckout(c, form)
	if len(errs) > 0 || o == nil || res == nil || res.Result != "success" {
		t.Fatalf("checkout: %v %+v", errs, res)
	}
	exp, _ := strconv.ParseInt(fake.last("/v1/checkout/sessions").Form.Get("expires_at"), 10, 64)
	if d := time.Until(time.Unix(exp, 0)); d < 30*time.Minute || d > 24*time.Hour {
		t.Fatalf("session expires_at in %v", d)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	sid := gxaStr(o.Meta, "_stripe_session_id")
	if sid == "" {
		t.Fatal("no session id on the order")
	}
	if err := srv.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "cancelled", "Order cancelled by customer.", ""); err != nil {
		t.Fatal(err)
	}
	path := "/v1/checkout/sessions/" + sid + "/expire"
	for i := 0; i < 100 && fake.count(path) == 0; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if fake.count(path) != 1 {
		t.Fatalf("expire calls: %d", fake.count(path))
	}
	if got, _ := srv.gooGetOrder(ctx, o.ID); got.Status != "cancelled" {
		t.Fatalf("cancel: %s", got.Status)
	}
}
