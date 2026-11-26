package server

import (
	"context"
	"testing"
	"time"
)

// Cancelling an order closes every Checkout page opened for it, not only the last pay-for-order attempt's.
func TestT2NAccountsStripeExpiresEverySession(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	fake := newGooFakeStripe(t)
	gooStripeEnable(t, srv)
	p := gooMakeSimple(t, srv, "Stripe Cup", 12, func(p *gooProduct) { p.Virtual = true })
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
	o, _ = srv.gooGetOrder(ctx, o.ID)
	first := gxaStr(o.Meta, "_stripe_session_id")
	// A second attempt from the pay-for-order page opens another Checkout page.
	if res, err := (gooStripeGateway{}).ProcessPayment(srv, gooTestCtx(), o); err != nil || res.Result != "success" {
		t.Fatalf("second attempt: %v %+v", err, res)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	second := gxaStr(o.Meta, "_stripe_session_id")
	if first == "" || second == "" || first == second {
		t.Fatalf("sessions: %q %q", first, second)
	}
	if err := srv.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "cancelled", "Order cancelled by customer.", ""); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{first, second} {
		path := "/v1/checkout/sessions/" + sid + "/expire"
		for i := 0; i < 100 && fake.count(path) == 0; i++ {
			time.Sleep(50 * time.Millisecond)
		}
		if fake.count(path) != 1 {
			t.Fatalf("expire calls for %s: %d", sid, fake.count(path))
		}
	}
}

// The remembered list stays bounded.
func TestT2NAccountsStripeSessionListBounded(t *testing.T) {
	o := &gooOrder{Meta: map[string]any{}}
	for i := 0; i < gooStripeMaxSessions+5; i++ {
		gooStripeRememberSession(o, "cs_test_"+itoa64(int64(i)))
	}
	ids := gooStripeSessionIDs(o)
	if len(ids) != gooStripeMaxSessions || ids[len(ids)-1] != "cs_test_"+itoa64(int64(gooStripeMaxSessions+4)) {
		t.Fatalf("ids: %v", ids)
	}
}
