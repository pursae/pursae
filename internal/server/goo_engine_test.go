package server

import (
	"context"
	"html"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// gooSite installs a site, logs the admin in and activates Goocommerce from the Plugins screen.
func gooSite(t *testing.T) (string, *http.Client, *Server) {
	t.Helper()
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	_, body, _ := get(t, c, ts.URL+"/wp-admin/plugins")
	m := regexp.MustCompile(`href="(/wp-admin/plugins\?action=activate&amp;plugin=goocommerce[^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no Activate link for Pursae Commerce on the Plugins screen")
	}
	if code, _, loc := get(t, c, ts.URL+html.UnescapeString(m[1])); code != http.StatusFound || !activationOK(loc) {
		t.Fatalf("activate Pursae Commerce: %d %s", code, loc)
	}
	if !srv.gooOn() {
		t.Fatal("Pursae Commerce is not active after activation")
	}
	// Tests check out and try codes from one address many times: the per-IP limits are raised here
	// and tested on their own (gooRealThrottles).
	gooGuestCheckoutThrottles.Store(srv, newGooThrottle(1<<30, 10*time.Minute))
	gooGiftCardThrottles.Store(srv, newGooThrottle(1<<30, 10*time.Minute))
	return ts.URL, c, srv
}

// gooTestCtx is a gin context for engine calls outside a request (a visitor without cookies).
func gooTestCtx() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c
}

func gooAlmost(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0001 {
		t.Errorf("%s = %.4f, want %.4f", what, got, want)
	}
}

// gooMakeSimple creates a published simple product.
func gooMakeSimple(t *testing.T, srv *Server, name string, price float64, mut func(p *gooProduct)) *gooProduct {
	t.Helper()
	p := gooNewProduct(gooTypeSimple)
	p.Name, p.RegularPrice = name, gooFloatPtr(price)
	if mut != nil {
		mut(p)
	}
	if err := srv.gooSaveProduct(context.Background(), p, 1); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGooOffByDefault(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	if srv.gooOn() {
		t.Fatal("Pursae Commerce is active on a fresh install")
	}
	for _, u := range []string{"/wp-admin/goocommerce", "/wp-admin/goocommerce?page=orders", gooAssets + "/css/.keep"} {
		if code, _, _ := get(t, c, ts.URL+u); code != http.StatusNotFound {
			t.Errorf("%s while off: %d", u, code)
		}
	}
	if _, body, _ := get(t, c, ts.URL+"/wp-admin/"); strings.Contains(body, "toplevel_page_goocommerce") {
		t.Error("the Pursae Commerce menu shows while the plugin is off")
	}
}

func TestGooActivationPages(t *testing.T) {
	base, c, srv := gooSite(t)
	for _, k := range []string{"shop", "cart", "checkout", "myaccount"} {
		if srv.gooPageID(k) == 0 {
			t.Errorf("no %s page after activation", k)
		}
	}
	_, body, _ := get(t, c, base+"/wp-admin/")
	for _, want := range []string{"toplevel_page_goocommerce", "menu-posts-product", "goocommerce?page=orders", "edit-tags?taxonomy=product_cat&amp;post_type=product", "goocommerce?page=coupons"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin menu lacks %s", want)
		}
	}
	// A second activation reuses the pages.
	before := srv.gooPageID("cart")
	if err := srv.gooCreatePages(context.Background(), 1); err != nil || srv.gooPageID("cart") != before {
		t.Errorf("pages recreated: %v %d → %d", err, before, srv.gooPageID("cart"))
	}
}

func TestGooEngine(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	if err := srv.gooSetOpts(ctx, map[string]any{"calc_taxes": true, "default_country": "US:CA", "currency": "USD"}); err != nil {
		t.Fatal(err)
	}

	// Tax: CA 10% standard, plus a compound 1% county rate; reduced 5%.
	for _, r := range []*gooTaxRate{
		{Country: "US", State: "CA", Rate: 10, Name: "CA Tax", Priority: 1, Shipping: true},
		{Country: "US", State: "CA", Rate: 1, Name: "County", Priority: 2, Compound: true, Shipping: false},
		{Country: "US", Rate: 5, Name: "Reduced", Priority: 1, Class: "reduced-rate"},
	} {
		if err := srv.gooSaveTaxRate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rates := srv.gooFindRates(ctx, "", gooLocation{Country: "US", State: "CA", Postcode: "90210"}, false)
	if len(rates) != 2 {
		t.Fatalf("CA rates: %+v", rates)
	}
	ex := gooCalcTax(100, rates, false)
	gooAlmost(t, "exclusive tax", gooSumTaxes(ex), 10+1.1)
	in := gooCalcTax(111.1, rates, true)
	gooAlmost(t, "inclusive tax", gooSumTaxes(in), 11.1)
	if r := srv.gooFindRates(ctx, "", gooLocation{Country: "US", State: "NY"}, false); len(r) != 0 {
		t.Errorf("NY matched CA rates: %+v", r)
	}

	// Shipping: a US zone with flat rate (5 + 2 per item) and free shipping over 200.
	z := &gooZone{Name: "United States", Locations: []gooZoneLocation{{Code: "US", Type: "country"}}}
	if err := srv.gooSaveZone(ctx, z); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.gooAddMethod(ctx, z.ID, "flat_rate", map[string]any{"cost": "5 + 2 * [qty]"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.gooAddMethod(ctx, z.ID, "free_shipping", map[string]any{"requires": "min_amount", "min_amount": "200"}); err != nil {
		t.Fatal(err)
	}
	if v, ok := gooEvalCost(`10 + [fee percent="10" min_fee="3"]`, 2, 20); !ok || v != 13 {
		t.Errorf("fee expression = %v %v", v, ok)
	}

	// Products.
	shirt := gooMakeSimple(t, srv, "Shirt", 40, func(p *gooProduct) { p.ManageStock, p.StockQuantity = true, gooIntPtr(5) })
	book := gooMakeSimple(t, srv, "Book", 20, func(p *gooProduct) { p.TaxClass = "reduced-rate"; p.SalePrice = gooFloatPtr(15) })
	ebook := gooMakeSimple(t, srv, "E-book", 10, func(p *gooProduct) {
		p.Virtual, p.Downloadable = true, true
		p.Downloads = []gooDownload{{Name: "PDF", File: "https://example.test/e.pdf"}}
	})
	if !book.IsOnSale() || book.PriceValue() != 15 {
		t.Errorf("book sale: %v %v", book.IsOnSale(), book.PriceValue())
	}

	// Variable product with two variations.
	if err := srv.gooSaveAttribute(ctx, &gooAttribute{Name: "Color"}); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"Red", "Blue"} {
		if err := srv.store.InsertTerm(ctx, termFor("pa_color", col)); err != nil {
			t.Fatal(err)
		}
	}
	hoodie := gooNewProduct(gooTypeVariable)
	hoodie.Name = "Hoodie"
	hoodie.Attributes = []gooProductAttr{{Taxonomy: "pa_color", Options: []string{"red", "blue"}, Visible: true, Variation: true}}
	if err := srv.gooSaveProduct(ctx, hoodie, 1); err != nil {
		t.Fatal(err)
	}
	for i, col := range []string{"red", "blue"} {
		v := gooNewProduct(gooTypeVariation)
		v.ParentID, v.Name, v.RegularPrice = hoodie.ID, "Hoodie - "+col, gooFloatPtr(float64(50+10*i))
		v.VariationAttrs = map[string]string{"pa_color": col}
		v.ManageStock, v.StockQuantity = true, gooIntPtr(3)
		if err := srv.gooSaveProduct(ctx, v, 1); err != nil {
			t.Fatal(err)
		}
	}
	hoodie, _ = srv.gooGetProduct(ctx, hoodie.ID)
	if hoodie.MinPrice == nil || *hoodie.MinPrice != 50 || *hoodie.MaxPrice != 60 || len(hoodie.Children) != 2 {
		t.Errorf("variable sync: min %v max %v children %v", hoodie.MinPrice, hoodie.MaxPrice, hoodie.Children)
	}

	// Coupon: 10% off.
	if err := srv.gooSaveCoupon(ctx, &gooCoupon{Code: "TENOFF", DiscountType: "percent", Amount: 10}); err != nil {
		t.Fatal(err)
	}

	// A visitor's cart in California.
	c := gooTestCtx()
	cart := srv.gooCartFor(c)
	cart.sess.Data.Customer.Billing = map[string]string{"country": "US", "state": "CA", "postcode": "90210", "email": "buyer@example.test"}
	if _, err := cart.Add(ctx, shirt.ID, 2, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cart.Add(ctx, book.ID, 1, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cart.Add(ctx, hoodie.ID, 1, 0, map[string]string{"attribute_pa_color": "blue"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cart.Add(ctx, hoodie.ID, 1, 0, nil, nil, nil); err == nil {
		t.Error("a variable product without its options was added")
	}
	if _, err := cart.Add(ctx, shirt.ID, 4, 0, nil, nil, nil); err == nil {
		t.Error("more shirts than in stock were added")
	}
	// Subtotal: 80 + 15 + 60 = 155; taxes: shirt 80 → 8 + 0.88, hoodie 60 → 6 + 0.66, book 15 → 0.75.
	gooAlmost(t, "subtotal", cart.Subtotal, 155)
	gooAlmost(t, "subtotal tax", cart.SubtotalTax, 8.88+6.66+0.75)
	if len(cart.Packages) != 1 || cart.Packages[0].Chosen == nil || cart.Packages[0].Chosen.MethodID != "flat_rate" {
		t.Fatalf("shipping packages: %+v", cart.Packages)
	}
	// Flat rate: 5 + 2 × 4 items = 13, taxed 10% (the compound county rate is not for shipping).
	gooAlmost(t, "shipping", cart.ShippingTotal, 13)
	gooAlmost(t, "shipping tax", cart.ShippingTax, 1.3)
	if err := cart.ApplyCoupon(ctx, "tenoff"); err != nil {
		t.Fatal(err)
	}
	if err := cart.ApplyCoupon(ctx, "TENOFF"); err == nil {
		t.Error("a coupon was applied twice")
	}
	gooAlmost(t, "discount", cart.DiscountTotal, 15.5)
	gooAlmost(t, "contents total", cart.CartContentsTotal, 139.5)
	// Discounted taxes: shirt 72 → 7.2 + 0.792, hoodie 54 → 5.4 + 0.594, book 13.5 → 0.675.
	wantTax := gooRound(7.2+0.79, 2) + gooRound(5.4+0.59, 2) + 0.68 + 1.3
	gooAlmost(t, "total tax", cart.TotalTax, wantTax)
	gooAlmost(t, "total", cart.Total, 139.5+13+wantTax)

	// Checkout with the test gateway.
	if err := srv.gooSetOpts(ctx, map[string]any{"goo_test_settings": map[string]any{"enabled": "yes"}}); err != nil {
		t.Fatal(err)
	}
	form := map[string][]string{
		"billing_first_name": {"Ada"}, "billing_last_name": {"Lovelace"}, "billing_country": {"US"}, "billing_address_1": {"1 Main St"},
		"billing_city": {"Beverly Hills"}, "billing_state": {"CA"}, "billing_postcode": {"90210"}, "billing_phone": {"555-1234"},
		"billing_email": {"buyer@example.test"}, "payment_method": {"goo_test"},
	}
	cartTotal := cart.Total
	res, order, errs := srv.gooProcessCheckout(c, form)
	if len(errs) > 0 || res == nil || order == nil {
		t.Fatalf("checkout: %v", errs)
	}
	if !strings.Contains(res.Redirect, "/order-received/") {
		t.Errorf("redirect %s", res.Redirect)
	}
	o, _ := srv.gooGetOrder(ctx, order.ID)
	if o.Status != "processing" || o.TransactionID == "" || !o.StockReduced {
		t.Errorf("order after payment: status %s tx %q stock reduced %v", o.Status, o.TransactionID, o.StockReduced)
	}
	gooAlmost(t, "order total", o.Total, cartTotal)
	if len(o.ItemsOf("line_item")) != 3 || len(o.ItemsOf("shipping")) != 1 || len(o.ItemsOf("coupon")) != 1 || len(o.ItemsOf("tax")) != 3 {
		t.Errorf("order items: %d lines, %d shipping, %d coupons, %d taxes", len(o.ItemsOf("line_item")), len(o.ItemsOf("shipping")), len(o.ItemsOf("coupon")), len(o.ItemsOf("tax")))
	}
	if p, _ := srv.gooGetProduct(ctx, shirt.ID); p.StockQuantity == nil || *p.StockQuantity != 3 {
		t.Errorf("shirt stock after the order: %v", p.StockQuantity)
	}
	if cp, _ := srv.gooCouponByCode(ctx, "tenoff"); cp.UsageCount != 1 {
		t.Errorf("coupon usage %d", cp.UsageCount)
	}
	if !srv.gooCartFor(c).IsEmpty() {
		t.Error("the cart is not empty after the order")
	}

	// Refund one shirt with restock: partially refunded, stock back to 4.
	var shirtItem *gooOrderItem
	for _, it := range o.ItemsOf("line_item") {
		if it.ProductID == shirt.ID {
			shirtItem = it
		}
	}
	if _, err := srv.gooCreateRefund(ctx, o, 36, "damaged", []gooRefundLine{{ItemID: shirtItem.ID, Qty: 1, Total: 36}}, true, true, 1); err != nil {
		t.Fatal(err)
	}
	if p, _ := srv.gooGetProduct(ctx, shirt.ID); *p.StockQuantity != 4 {
		t.Errorf("shirt stock after the refund: %d", *p.StockQuantity)
	}
	gooAlmost(t, "refunded", srv.gooTotalRefunded(ctx, o.ID), 36)
	// Saving the order's items again keeps their ids, so the refund line still points at the shirt.
	o, _ = srv.gooGetOrder(ctx, o.ID)
	if err := srv.gooSaveOrder(ctx, o, true); err != nil {
		t.Fatal(err)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	if refunds, _ := srv.gooRefunds(ctx, o.ID); len(refunds) != 1 || len(refunds[0].ItemsOf("line_item")) != 1 {
		t.Fatalf("refunds: %d", len(refunds))
	} else {
		ref, _ := refunds[0].ItemsOf("line_item")[0].Meta["_refunded_item_id"].(float64)
		found := false
		for _, it := range o.ItemsOf("line_item") {
			found = found || (it.ID == int64(ref) && it.ProductID == shirt.ID)
		}
		if !found {
			t.Errorf("the refund line lost its order item (%v) after the items were saved again", ref)
		}
	}
	if _, err := srv.gooCreateRefund(ctx, o, o.Total, "", nil, false, false, 1); err == nil {
		t.Error("a refund above the remaining amount was accepted")
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	if _, err := srv.gooCreateRefund(ctx, o, gooRound(o.Total-36, 2), "", nil, false, false, 1); err != nil {
		t.Fatal(err)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	if o.Status != "refunded" {
		t.Errorf("status after the full refund: %s", o.Status)
	}

	// Cancelling an order restores its stock and its coupon use.
	c2 := gooTestCtx()
	cart2 := srv.gooCartFor(c2)
	cart2.sess.Data.Customer.Billing = map[string]string{"country": "US", "state": "NY"}
	if _, err := cart2.Add(ctx, shirt.ID, 1, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cart2.Add(ctx, ebook.ID, 1, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	gooAlmost(t, "NY tax", cart2.TotalTax, 0)
	form["billing_state"], form["billing_postcode"], form["payment_method"] = []string{"NY"}, []string{"10001"}, []string{"cheque"}
	_ = srv.gooSetOpts(ctx, map[string]any{"cheque_settings": map[string]any{"enabled": "yes"}})
	_, o2, errs := srv.gooProcessCheckout(c2, form)
	if len(errs) > 0 {
		t.Fatalf("cheque checkout: %v", errs)
	}
	o2, _ = srv.gooGetOrder(ctx, o2.ID)
	if o2.Status != "on-hold" || !o2.StockReduced {
		t.Errorf("cheque order: %s reduced %v", o2.Status, o2.StockReduced)
	}
	if err := srv.gooSetStatus(ctx, o2, "cancelled", "", ""); err != nil {
		t.Fatal(err)
	}
	if p, _ := srv.gooGetProduct(ctx, shirt.ID); *p.StockQuantity != 4 {
		t.Errorf("shirt stock after cancelling: %d", *p.StockQuantity)
	}
	notes, _ := srv.gooOrderNotes(ctx, o2.ID, false)
	if len(notes) < 3 {
		t.Errorf("order notes: %d", len(notes))
	}

	// Validation errors.
	c3 := gooTestCtx()
	cart3 := srv.gooCartFor(c3)
	_, _ = cart3.Add(ctx, book.ID, 1, 0, nil, nil, nil)
	_, _, errs = srv.gooProcessCheckout(c3, map[string][]string{"billing_email": {"nope"}, "billing_country": {"US"}, "billing_state": {"ZZ"}, "payment_method": {"bacs"}})
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"Billing First name</strong> is a required field", "not a valid email address", "Billing State / County is not valid", "Invalid payment method"} {
		if !strings.Contains(joined, want) {
			t.Errorf("checkout errors lack %q:\n%s", want, joined)
		}
	}
}

func TestGooCouponRules(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	a := gooMakeSimple(t, srv, "A", 30, nil)
	b := gooMakeSimple(t, srv, "B", 10, func(p *gooProduct) { p.SalePrice = gooFloatPtr(8) })
	limit := 1
	coupons := []*gooCoupon{
		{Code: "fixed5", DiscountType: "fixed_cart", Amount: 5},
		{Code: "prod2", DiscountType: "fixed_product", Amount: 2, ProductIDs: []int64{a.ID}},
		{Code: "min100", DiscountType: "percent", Amount: 50, MinimumAmount: gooFloatPtr(100)},
		{Code: "nosale", DiscountType: "percent", Amount: 50, ExcludeSaleItems: true, ProductIDs: []int64{b.ID}},
		{Code: "solo", DiscountType: "percent", Amount: 10, IndividualUse: true},
		{Code: "once", DiscountType: "fixed_cart", Amount: 1, UsageLimit: &limit, UsageCount: 0},
		{Code: "mine", DiscountType: "fixed_cart", Amount: 1, EmailRestrictions: []string{"*@example.org"}},
	}
	for _, cp := range coupons {
		if err := srv.gooSaveCoupon(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.gooSaveCoupon(ctx, &gooCoupon{Code: "FIXED5"}); err == nil {
		t.Error("a duplicate coupon code was saved")
	}
	c := gooTestCtx()
	cart := srv.gooCartFor(c)
	_, _ = cart.Add(ctx, a.ID, 2, 0, nil, nil, nil)
	_, _ = cart.Add(ctx, b.ID, 1, 0, nil, nil, nil)
	gooAlmost(t, "subtotal", cart.Subtotal, 68)
	if err := cart.ApplyCoupon(ctx, "min100"); err == nil || !strings.Contains(err.Error(), "minimum spend") {
		t.Errorf("min100: %v", err)
	}
	if err := cart.ApplyCoupon(ctx, "nosale"); err == nil {
		t.Error("nosale applied to a sale item")
	}
	if err := cart.ApplyCoupon(ctx, "fixed5"); err != nil {
		t.Fatal(err)
	}
	if err := cart.ApplyCoupon(ctx, "prod2"); err != nil {
		t.Fatal(err)
	}
	gooAlmost(t, "discount fixed5 + prod2", cart.DiscountTotal, 5+4)
	gooAlmost(t, "fixed5", cart.CouponTotals["fixed5"], 5)
	if err := cart.ApplyCoupon(ctx, "solo"); err != nil {
		t.Fatal(err)
	}
	if len(cart.AppliedCoupons) != 1 || cart.AppliedCoupons[0] != "solo" {
		t.Errorf("individual use coupon kept others: %v", cart.AppliedCoupons)
	}
	if err := cart.ApplyCoupon(ctx, "fixed5"); err == nil {
		t.Error("a coupon was added next to an individual-use one")
	}
	cart.RemoveCoupon("solo")
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE goo_coupons SET usage_count = 1 WHERE code = 'once'`)
	if err := cart.ApplyCoupon(ctx, "once"); err == nil || !strings.Contains(err.Error(), "usage limit") {
		t.Errorf("once: %v", err)
	}
	cart.sess.Data.Customer.Billing["email"] = "x@example.com"
	if err := cart.ApplyCoupon(ctx, "mine"); err == nil {
		t.Error("an e-mail restricted coupon applied to another e-mail")
	}
	cart.sess.Data.Customer.Billing["email"] = "x@example.org"
	if err := cart.ApplyCoupon(ctx, "mine"); err != nil {
		t.Errorf("mine for an allowed e-mail: %v", err)
	}
}

func TestGooShippingZones(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	eu := &gooZone{Name: "Europe", Position: 2, Locations: []gooZoneLocation{{Code: "EU", Type: "continent"}}}
	ca := &gooZone{Name: "California 90xxx", Position: 1, Locations: []gooZoneLocation{{Code: "US:CA", Type: "state"}, {Code: "90*", Type: "postcode"}}}
	for _, z := range []*gooZone{eu, ca} {
		if err := srv.gooSaveZone(ctx, z); err != nil {
			t.Fatal(err)
		}
	}
	for dest, want := range map[gooLocation]int64{
		{Country: "DE", Postcode: "10115"}:              eu.ID,
		{Country: "US", State: "CA", Postcode: "90210"}: ca.ID,
		{Country: "US", State: "CA", Postcode: "94016"}: 0,
		{Country: "JP"}: 0,
	} {
		if got := srv.gooMatchZone(ctx, dest); got.ID != want {
			t.Errorf("zone for %+v = %d (%s), want %d", dest, got.ID, got.Name, want)
		}
	}
	for pat, pc := range map[string]string{"90210...90299": "90250", "SW1A*": "sw1a 1aa", "12345": "12345"} {
		if !gooPostcodeMatch(pat, pc) {
			t.Errorf("%s does not match %s", pat, pc)
		}
	}
	if gooPostcodeMatch("90210...90299", "90300") {
		t.Error("range matched outside")
	}
}

func TestGooMoney(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	if got := string(srv.gooPrice(1234.5)); !strings.Contains(got, `<span class="woocommerce-Price-currencySymbol">$</span>1,234.50`) {
		t.Errorf("wc_price: %s", got)
	}
	_ = srv.gooSetOpts(ctx, map[string]any{"currency": "TRY", "currency_pos": "right_space", "price_thousand_sep": ".", "price_decimal_sep": ",", "price_num_decimals": 2})
	if got := srv.gooPricePlain(1234.5, "TRY"); got != "1.234,50 ₺" {
		t.Errorf("TRY price: %q", got)
	}
	if v := srv.gooParseAmount("1.234,50"); v == nil || *v != 1234.5 {
		t.Errorf("parse TRY amount: %v", v)
	}
}

func termFor(tax, name string) *store.Term {
	return &store.Term{Taxonomy: tax, Name: name, Slug: strings.ToLower(name)}
}

// Store shortcodes placed in a block widget (a Shortcode block in the sidebar) render on every page.
func TestGooBlockWidgetShortcodes(t *testing.T) {
	base, c, srv := gooSite(t)
	ctx := context.Background()
	installLibraryTheme(t, c, base, "inkwell", true) // a theme with a sidebar (the default theme has none)
	theme := srv.activeTheme().Slug
	area, err := srv.store.EnsureWidgetArea(ctx, theme, "sidebar-1", "Sidebar")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWidget(ctx, area, "block", 0, map[string]any{
		"content": "<!-- wp:shortcode -->\n[goocommerce_widget_product_search]\n<!-- /wp:shortcode -->"}); err != nil {
		t.Fatal(err)
	}
	page := bodyOf(anonGet(t, base+"/"))
	if strings.Contains(page, "[goocommerce_widget_product_search]") || !strings.Contains(page, "woocommerce-product-search") {
		t.Errorf("the block widget did not run the store shortcode")
	}
}
