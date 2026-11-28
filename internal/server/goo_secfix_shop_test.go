package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"wpgo/internal/store"
)

// #48: a state posted by name prices the order like its code (tax and zones match codes).
func TestGooCheckoutStateNameSameTotal(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	gxaEnableGateways(t, srv)
	if err := srv.gooSetOpts(ctx, map[string]any{"calc_taxes": true, "tax_based_on": "billing"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.gooSaveTaxRate(ctx, &gooTaxRate{Country: "US", State: "TX", Rate: 10, Name: "TX Tax", Priority: 1}); err != nil {
		t.Fatal(err)
	}
	p := gooMakeSimple(t, srv, "State Lamp", 50, func(p *gooProduct) { p.Virtual = true })
	total := func(state string) float64 {
		c := gxaCtx(nil, http.MethodPost, "/", nil)
		if _, err := srv.gooCartFor(c).Add(c, p.ID, 1, 0, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		form := gxaCheckoutForm("state-"+state[:2]+"@example.test", "goo_test")
		form["billing_state"] = []string{state}
		_, o, errs := srv.gooProcessCheckout(c, form)
		if len(errs) > 0 || o == nil {
			t.Fatalf("checkout %s: %v", state, errs)
		}
		if o.Billing["state"] != "TX" {
			t.Errorf("stored state %q", o.Billing["state"])
		}
		return o.Total
	}
	byCode, byName := total("TX"), total("Texas")
	gooAlmost(t, "total by code", byCode, 55)
	gooAlmost(t, "total by name", byName, byCode)
}

// #5: a single-use coupon claimed by one checkout makes the next one fail before payment instead of
// placing a second discounted (on-hold) order.
func TestGooCheckoutSingleUseCouponRace(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	gxaEnableGateways(t, srv)
	p := gooMakeSimple(t, srv, "Coupon Mug", 30, func(p *gooProduct) { p.Virtual = true })
	cp := &gooCoupon{Code: "once10", DiscountType: "fixed_cart", Amount: 10, UsageLimit: gooIntPtr(1)}
	if err := srv.gooSaveCoupon(ctx, cp); err != nil {
		t.Fatal(err)
	}
	c1, c2 := gxaCtx(nil, http.MethodPost, "/", nil), gxaCtx(nil, http.MethodPost, "/", nil)
	cart1, cart2 := srv.gooCartFor(c1), srv.gooCartFor(c2)
	if _, err := cart1.Add(c1, p.ID, 1, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cart2.Add(c2, p.ID, 1, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Both carts hold the coupon before either checkout counts it (the race window).
	if err := cart1.ApplyCoupon(c1.Request.Context(), "once10"); err != nil {
		t.Fatal(err)
	}
	if err := cart2.ApplyCoupon(c2.Request.Context(), "once10"); err != nil {
		t.Fatal(err)
	}
	if _, o, errs := srv.gooProcessCheckout(c1, gxaCheckoutForm("first@example.test", "cheque")); len(errs) > 0 || o == nil {
		t.Fatalf("first checkout: %v", errs)
	}
	_, o2, errs := srv.gooProcessCheckout(c2, gxaCheckoutForm("second@example.test", "cheque"))
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, " "), "Usage limit") {
		t.Fatalf("second checkout with a used-up coupon: %v", errs)
	}
	if o2 != nil {
		if cur, err := srv.gooGetOrder(ctx, o2.ID); err == nil && cur.Status != "pending" {
			t.Errorf("losing order moved to %s", cur.Status)
		}
	}
	var used int
	if err := srv.store.Pool.QueryRow(ctx, `SELECT usage_count FROM goo_coupons WHERE code = 'once10'`).Scan(&used); err != nil || used != 1 {
		t.Errorf("usage_count = %d (%v)", used, err)
	}
	// Cheque orders go on hold awaiting payment: only the winner's may carry the discount.
	var placed int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM goo_orders WHERE type = 'shop_order' AND status NOT IN ('pending', 'checkout-draft') AND discount_total > 0`).Scan(&placed)
	if placed != 1 {
		t.Errorf("%d discounted orders placed, want 1", placed)
	}
}

// #19: guests place at most 10 orders in 10 minutes per IP.
func TestGooGuestCheckoutRateLimit(t *testing.T) {
	_, _, srv := gooSite(t)
	gooRealThrottles(srv)
	gxaEnableGateways(t, srv)
	p := gooMakeSimple(t, srv, "Flood Pen", 5, func(p *gooProduct) { p.Virtual = true })
	for i := 0; i < 11; i++ {
		c := gxaCtx(nil, http.MethodPost, "/", nil)
		if _, err := srv.gooCartFor(c).Add(c, p.ID, 1, 0, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		_, _, errs := srv.gooProcessCheckout(c, gxaCheckoutForm("flood@example.test", "cheque"))
		if i < 10 && len(errs) > 0 {
			t.Fatalf("checkout %d: %v", i, errs)
		}
		if i == 10 && (len(errs) != 1 || errs[0] != gooThrottledMsg) {
			t.Fatalf("11th guest checkout not limited: %v", errs)
		}
	}
	// A logged-in customer is not limited by the guests' count.
	u := gxaCustomer(t, srv, "loyal@example.test")
	c := gxaCtx(u, http.MethodPost, "/", nil)
	if _, err := srv.gooCartFor(c).Add(c, p.ID, 1, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, errs := srv.gooProcessCheckout(c, gxaCheckoutForm(u.Email, "cheque")); len(errs) > 0 {
		t.Fatalf("customer checkout: %v", errs)
	}
}

// #30: wrong gift card codes are limited per IP.
func TestGooGiftCardApplyThrottle(t *testing.T) {
	_, _, srv := gooSite(t)
	gooRealThrottles(srv)
	for i := 0; i < 10; i++ {
		c := gxaCtx(nil, http.MethodGet, "/", nil)
		if _, err := srv.gcApply(c, "NOPE-0000-0000-000"+itoa(i)); err == nil || err == errGooThrottled {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := srv.gcApply(gxaCtx(nil, http.MethodGet, "/", nil), "NOPE-1111"); err != errGooThrottled {
		t.Fatalf("11th wrong code: %v", err)
	}
}

// #59: nested members-only blocks are matched by depth; an unbalanced one fails closed.
func TestGmemShortcodeNesting(t *testing.T) {
	member := func(attrs string) bool { return strings.Contains(attrs, "gold") }
	nested := `A[goocommerce_members_only plans="silver"]S1[goocommerce_members_only plans="gold"]G[/goocommerce_members_only]S2[/goocommerce_members_only]B`
	if out := gmemApplyShortcodes(nested, member, false); out != "AB" {
		t.Errorf("non-member of the outer plan: %q", out)
	}
	nested2 := `A[goocommerce_members_only plans="gold"]G1[goocommerce_members_only plans="silver"]S[/goocommerce_members_only]G2[/goocommerce_members_only]B`
	if out := gmemApplyShortcodes(nested2, member, false); out != "AG1G2B" {
		t.Errorf("member of the outer plan only: %q", out)
	}
	if out := gmemApplyShortcodes(`A[goocommerce_members_only plans="silver"]secret`, member, false); out != "A" {
		t.Errorf("unbalanced block: %q", out)
	}
	if out := gmemStripShortcodes(nested); out != "AB" {
		t.Errorf("strip nested: %q", out)
	}
	if out := gmemApplyShortcodes(`[goocommerce_non_members]Join[/goocommerce_non_members]`, member, false); out != "Join" {
		t.Errorf("non-members block: %q", out)
	}
}

// #49 #50 #51: listings, nested renders and the Store API apply a post's or product's plans.
func TestGmemListingsAndStoreAPI(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	pass := gooMakeSimple(t, srv, "Pass", 50, func(p *gooProduct) { p.Virtual = true })
	secret := gooMakeSimple(t, srv, "Hidden Tea", 15, func(p *gooProduct) { p.Virtual = true })
	secret.Post.Content = "Tea secret recipe"
	news := &store.Post{Type: "post", Title: "Members news", Slug: "members-news-x", Content: "<p>The secret recipe.</p>", Status: "publish", AuthorID: 1, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, news); err != nil {
		t.Fatal(err)
	}
	plan := &gooMembershipPlan{Name: "Gold", AccessMethod: "purchase", ProductIDs: []int64{pass.ID},
		Rules: gmemRules{ContentPostIDs: []int64{news.ID}, ProductIDs: []int64{secret.ID}, ProductRestrict: "view"}}
	if err := srv.gmemSavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	gxaOn(t, srv, gmemKey, true)
	gxaSetExt(t, srv, gmemKey, map[string]any{"show_excerpts": false})

	if _, hidden := srv.gmemListingExcerpt(ctx, nil, news.ID); !hidden {
		t.Error("listing excerpt of restricted post shown")
	}
	c := gxaCtx(nil, http.MethodGet, "/feed/", nil)
	if ex := srv.feedExcerpt(ctx, c, news); strings.Contains(ex, "secret") {
		t.Errorf("feed excerpt leaks: %s", ex)
	}
	if msg, hidden := srv.gmemNestedRestricted(ctx, nil, news.ID, false); !hidden || strings.Contains(msg, "secret recipe") {
		t.Errorf("nested content: %v %s", hidden, msg)
	}
	desc, short := srv.gmemStoreProductText(ctx, secret, "Tea secret recipe", "Short")
	if strings.Contains(desc, "secret") || short != "" || !strings.Contains(desc, "restriction-message") {
		t.Errorf("store api text: %q %q", desc, short)
	}
	m := srv.gooStoreProductJSON(ctx, secret, nil)
	if strings.Contains(m["description"].(string), "secret") {
		t.Errorf("store api product description: %v", m["description"])
	}
	// show_excerpts on: the excerpt shows, the content still does not.
	gxaSetExt(t, srv, gmemKey, map[string]any{"show_excerpts": true})
	if _, hidden := srv.gmemListingExcerpt(ctx, nil, news.ID); hidden {
		t.Error("excerpt hidden with show_excerpts on")
	}
}

// #13: the customer's name is escaped where the e-mail's additional content (HTML) uses it.
func TestGooEmailPlaceholderEscaped(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	if err := srv.gooSetOpts(ctx, map[string]any{"email_customer_processing_order_settings": map[string]any{"additional_content": "Hi {order_billing_full_name}"}}); err != nil {
		t.Fatal(err)
	}
	o := &gooOrder{Status: "processing", Currency: "USD", Billing: map[string]string{"first_name": `<img src=x onerror=alert(1)>`, "last_name": "X", "email": "x@example.test"}, Meta: map[string]any{}}
	e, err := srv.gooRenderEmail(ctx, gooEmailDefByID("customer_processing_order"), srv.gooEmailSettings("customer_processing_order"), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.HTML, "<img src=x") || !strings.Contains(e.HTML, "&lt;img src=x") {
		t.Errorf("name not escaped in HTML: %s", e.HTML[strings.Index(e.HTML, "Hi "):][:120])
	}
	if !strings.Contains(e.Plain, "Hi <img src=x") {
		t.Errorf("plain text: %q", e.Plain)
	}
}

// gooRealThrottles puts back the production per-IP limits that gooSite raised.
func gooRealThrottles(srv *Server) {
	gooGuestCheckoutThrottles.Delete(srv)
	gooGiftCardThrottles.Delete(srv)
}
