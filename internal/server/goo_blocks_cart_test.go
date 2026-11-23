package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"wpgo/internal/blocks"
)

// What WooCommerce 8.x saves on its Cart page: no cross-sells and no coupon form, an Empty cart
// block with the user's own blocks, a WooCommerce block Pursae does not render and one it has never
// heard of.
const wcImportedCart = `<!-- wp:woocommerce/cart -->
<div class="wp-block-woocommerce-cart alignwide is-loading"><!-- wp:woocommerce/filled-cart-block -->
<div class="wp-block-woocommerce-filled-cart-block"><!-- wp:woocommerce/cart-items-block -->
<div class="wp-block-woocommerce-cart-items-block"><!-- wp:woocommerce/cart-line-items-block -->
<div class="wp-block-woocommerce-cart-line-items-block"></div>
<!-- /wp:woocommerce/cart-line-items-block --></div>
<!-- /wp:woocommerce/cart-items-block -->

<!-- wp:woocommerce/cart-totals-block -->
<div class="wp-block-woocommerce-cart-totals-block"><!-- wp:woocommerce/cart-order-summary-block -->
<div class="wp-block-woocommerce-cart-order-summary-block"><!-- wp:woocommerce/cart-order-summary-subtotal-block -->
<div class="wp-block-woocommerce-cart-order-summary-subtotal-block"></div>
<!-- /wp:woocommerce/cart-order-summary-subtotal-block --></div>
<!-- /wp:woocommerce/cart-order-summary-block -->

<!-- wp:woocommerce/proceed-to-checkout-block -->
<div class="wp-block-woocommerce-proceed-to-checkout-block"></div>
<!-- /wp:woocommerce/proceed-to-checkout-block --></div>
<!-- /wp:woocommerce/cart-totals-block --></div>
<!-- /wp:woocommerce/filled-cart-block -->

<!-- wp:woocommerce/empty-cart-block -->
<div class="wp-block-woocommerce-empty-cart-block"><!-- wp:heading {"textAlign":"center"} -->
<h2 class="wp-block-heading has-text-align-center">Nothing in here yet!</h2>
<!-- /wp:heading -->

<!-- wp:woocommerce/product-new {"columns":4,"rows":1} /-->

<!-- wp:woocommerce/some-future-block -->
<div class="wp-block-woocommerce-some-future-block">FUTURE BLOCK</div>
<!-- /wp:woocommerce/some-future-block --></div>
<!-- /wp:woocommerce/empty-cart-block --></div>
<!-- /wp:woocommerce/cart -->`

// What WooCommerce saves on its Checkout page: a renamed billing step, no order notes, no "Return
// to cart" link, the coupon form, and the field attributes of WooCommerce before 8.x (company
// required, phone hidden).
const wcImportedCheckout = `<!-- wp:woocommerce/checkout {"showCompanyField":true,"requireCompanyField":true,"showPhoneField":false} -->
<div class="wp-block-woocommerce-checkout alignwide wc-block-checkout is-loading"><!-- wp:woocommerce/checkout-fields-block -->
<div class="wp-block-woocommerce-checkout-fields-block"><!-- wp:woocommerce/checkout-express-payment-block -->
<div class="wp-block-woocommerce-checkout-express-payment-block"></div>
<!-- /wp:woocommerce/checkout-express-payment-block -->

<!-- wp:woocommerce/checkout-contact-information-block -->
<div class="wp-block-woocommerce-checkout-contact-information-block"></div>
<!-- /wp:woocommerce/checkout-contact-information-block -->

<!-- wp:woocommerce/checkout-billing-address-block {"title":"Invoice address"} -->
<div class="wp-block-woocommerce-checkout-billing-address-block"></div>
<!-- /wp:woocommerce/checkout-billing-address-block -->

<!-- wp:woocommerce/checkout-payment-block -->
<div class="wp-block-woocommerce-checkout-payment-block"></div>
<!-- /wp:woocommerce/checkout-payment-block -->

<!-- wp:woocommerce/checkout-actions-block {"showReturnToCart":false} -->
<div class="wp-block-woocommerce-checkout-actions-block"></div>
<!-- /wp:woocommerce/checkout-actions-block --></div>
<!-- /wp:woocommerce/checkout-fields-block -->

<!-- wp:woocommerce/checkout-totals-block -->
<div class="wp-block-woocommerce-checkout-totals-block"><!-- wp:woocommerce/checkout-order-summary-block -->
<div class="wp-block-woocommerce-checkout-order-summary-block"><!-- wp:woocommerce/checkout-order-summary-coupon-form-block -->
<div class="wp-block-woocommerce-checkout-order-summary-coupon-form-block"></div>
<!-- /wp:woocommerce/checkout-order-summary-coupon-form-block --></div>
<!-- /wp:woocommerce/checkout-order-summary-block --></div>
<!-- /wp:woocommerce/checkout-totals-block --></div>
<!-- /wp:woocommerce/checkout -->`

// The settings read from imported block markup, and the block's own attributes winning over them.
func TestGooCartBlockSettings(t *testing.T) {
	ctx := context.Background()
	cart := blocks.Parse(wcImportedCart)[0]
	v := gooCartViewOf(ctx, cart)
	if v.CrossSells || v.Coupon {
		t.Errorf("imported cart: %+v, want no cross-sells and no coupon form", v)
	}
	if !strings.Contains(v.EmptyHTML, "Nothing in here yet!") || strings.Contains(v.EmptyHTML, "FUTURE BLOCK") || strings.Contains(v.EmptyHTML, "wp:woocommerce") ||
		strings.Contains(v.EmptyHTML, "product-new") {
		t.Errorf("empty cart content: %q", v.EmptyHTML)
	}
	cart.Attrs = map[string]any{"showCrossSells": true, "showCouponForm": true, "emptyCartMessage": "Go shopping"}
	if v = gooCartViewOf(ctx, cart); !v.CrossSells || !v.Coupon || v.EmptyMessage != "Go shopping" || v.EmptyHTML != "" {
		t.Errorf("cart attributes: %+v", v)
	}
	if v := gooCartViewOf(ctx, blocks.Parse(`<!-- wp:woocommerce/cart /-->`)[0]); v != gooDefaultCartView() {
		t.Errorf("new cart block: %+v", v)
	}

	co := blocks.Parse(wcImportedCheckout)[0]
	cv := gooCheckoutViewOf(co)
	if cv.OrderNotes || cv.ReturnToCart || !cv.Coupon || !cv.LoginReminder || cv.BillingTitle != "Invoice address" {
		t.Errorf("imported checkout: %+v", cv)
	}
	modes := gooCheckoutFieldModesOf(co)
	if modes["checkout_company_field"] != "required" || modes["checkout_phone_field"] != "hidden" || len(modes) != 2 {
		t.Errorf("field modes: %v", modes)
	}
	co.Attrs["showOrderNotes"], co.Attrs["orderTitle"], co.Attrs["showCouponForm"] = true, "Summary", false
	if cv = gooCheckoutViewOf(co); !cv.OrderNotes || cv.OrderTitle != "Summary" || cv.Coupon {
		t.Errorf("checkout attributes: %+v", cv)
	}
	if cv := gooCheckoutViewOf(blocks.Parse(`<!-- wp:woocommerce/checkout /-->`)[0]); !cv.OrderNotes || !cv.Coupon || !cv.ReturnToCart || cv.BillingTitle != "" {
		t.Errorf("new checkout block: %+v", cv)
	}
	if !gooPageHasStoreBlock("cart", wcImportedCart) || !gooPageHasStoreBlock("checkout", `<!-- wp:goocommerce/checkout /-->`) || gooPageHasStoreBlock("cart", wcImportedCheckout) {
		t.Error("gooPageHasStoreBlock")
	}
}

// Cart and Checkout pages imported from WordPress with WooCommerce's blocks work: rendered on the
// server by the shortcode renderers with the blocks' settings, the checkout validated with the
// block's fields.
func TestGooCartCheckoutBlocksImported(t *testing.T) {
	base, _, srv := gooSite(t)
	ctx := context.Background()
	f := gooFrontCatalog(t, srv)
	_ = srv.gooSetOpts(ctx, map[string]any{"goo_test_settings": map[string]any{"enabled": "yes"}})
	z := &gooZone{Name: "US", Locations: []gooZoneLocation{{Code: "US", Type: "country"}}}
	_ = srv.gooSaveZone(ctx, z)
	_, _ = srv.gooAddMethod(ctx, z.ID, "flat_rate", map[string]any{"cost": "5"})
	f.mug.CrossSellIDs = []int64{f.shirt.ID}
	if err := srv.gooSaveProduct(ctx, f.mug, 1); err != nil {
		t.Fatal(err)
	}
	setPage := func(key, content string) {
		t.Helper()
		p, err := srv.store.GetPost(ctx, srv.gooPageID(key))
		if err != nil {
			t.Fatal(err)
		}
		p.Content = content
		if err := srv.store.UpdatePost(ctx, p, 1); err != nil {
			t.Fatal(err)
		}
		srv.purgePost(ctx, p)
	}
	noBlockLeft := func(what, body string) {
		t.Helper()
		for _, bad := range []string{"wp:woocommerce", "wp-block-woocommerce-filled-cart-block", "wp-block-woocommerce-checkout-fields-block", "FUTURE BLOCK", "is-loading"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s shows %q", what, bad)
			}
		}
	}
	setPage("cart", wcImportedCart)
	setPage("checkout", wcImportedCheckout)
	v := gooFrontClient()

	// The empty cart shows the Empty cart block's own content.
	_, body, _ := get(t, v, base+"/cart/")
	gooMustContain(t, "empty cart", body, `<div class="woocommerce">`, "Nothing in here yet!", "wp-block-woocommerce-empty-cart-block") // the wrapper make wp2p-test looks for
	noBlockLeft("empty cart", body)
	if strings.Contains(body, "Your cart is currently empty.") {
		t.Error("the default empty cart notice shows next to the block's content")
	}

	// A filled cart: the cart form without the sections the imported block left out.
	get(t, v, base+"/?add-to-cart="+itoa64(f.mug.ID))
	_, body, _ = get(t, v, base+"/cart/")
	gooMustContain(t, "cart", body, `id="goo-cart-form"`, `>Mug</a>`, `name="update_cart"`, "cart_totals")
	noBlockLeft("cart", body)
	if strings.Contains(body, `name="coupon_code"`) || strings.Contains(body, `class="cross-sells"`) {
		t.Error("the cart shows a section its block does not have")
	}
	// The block's attributes (Pursae's editor) bring them back.
	setPage("cart", `<!-- wp:woocommerce/cart {"showCouponForm":true,"showCrossSells":true} -->`+"\n"+`<div class="wp-block-woocommerce-cart alignwide is-loading"></div>`+"\n"+`<!-- /wp:woocommerce/cart -->`)
	_, body, _ = get(t, v, base+"/cart/")
	gooMustContain(t, "cart with attributes", body, `name="coupon_code"`, `class="cross-sells"`)

	// The checkout: the block's heading and sections; company required, phone hidden.
	_, body, _ = get(t, v, base+"/checkout/")
	gooMustContain(t, "checkout", body, `<form name="checkout" method="post" class="checkout woocommerce-checkout"`, "<h3>Invoice address</h3>",
		`id="billing_company"`, `showcoupon`, `id="place_order"`)
	noBlockLeft("checkout", body)
	for _, bad := range []string{`id="order_comments"`, `id="billing_phone"`, "goo-return-to-cart"} {
		if strings.Contains(body, bad) {
			t.Errorf("checkout shows %q", bad)
		}
	}
	nonce := gooHidden(t, body, "woocommerce-process-checkout-nonce")
	form := gooFrontCheckoutForm(nonce)
	form.Del("billing_phone")
	_, _, loc := post(t, v, base+"/checkout/", form)
	_, body, _ = get(t, v, loc)
	gooMustContain(t, "checkout without company", body, "Billing Company name</strong> is a required field")
	if strings.Contains(body, "Billing Phone") {
		t.Error("the phone field the block hides is still validated")
	}
	form.Set("billing_company", "Analytical Engines")
	code, _, loc := post(t, v, base+"/checkout/", form)
	if code != http.StatusFound || !strings.Contains(loc, "/checkout/order-received/") {
		t.Fatalf("checkout with the block's fields: %d %s", code, loc)
	}

	// The status report counts the blocks as the pages' content.
	var plain strings.Builder
	for _, sec := range srv.gooSystemStatus(gooTestCtx()) {
		for _, it := range sec.Items {
			plain.WriteString(it.Plain + "\n")
		}
	}
	if r := plain.String(); !strings.Contains(r, "✔ woocommerce/cart") || !strings.Contains(r, "✔ woocommerce/checkout") {
		t.Errorf("status report: %s", r)
	}
}

// The block editor registers the Cart and Checkout blocks, and their preview renders on the server.
func TestGooCartBlocksEditor(t *testing.T) {
	base, c, _ := gooSite(t)
	_, body, _ := get(t, c, base+"/wp-admin/post-new?post_type=page")
	gooMustContain(t, "editor", body, `register( 'woocommerce/cart'`, `register( 'woocommerce/checkout'`, "showCrossSells", "showOrderNotes",
		`'checkout_phone_field'`, `"fieldModes"`, "wc-block-checkout is-loading", "InnerBlocks.Content")
	code, out := restDo(t, c, base, http.MethodPost, "/wp-json/wp/v2/block-renderer/woocommerce/checkout?context=edit",
		`{"attributes":{"showOrderNotes":false}}`, true)
	if code != http.StatusOK || !strings.Contains(out, "woocommerce") {
		t.Fatalf("checkout preview: %d %s", code, out)
	}
	code, out = restDo(t, c, base, http.MethodGet, "/wp-json/wp/v2/block-renderer/woocommerce/cart?context=edit", "", false)
	if code != http.StatusOK || !strings.Contains(out, "cart-empty") {
		t.Fatalf("cart preview: %d %s", code, out)
	}
}
