package server

import (
	"context"
	"strings"

	"wpgo/internal/blocks"
)

// The Cart and Checkout blocks, hybrid: the block editor inserts and configures woocommerce/cart and
// woocommerce/checkout (attributes for the sections, headings and fields), and the front end renders
// them on the server with the cart and checkout page renderers of [goocommerce_cart] and
// [goocommerce_checkout], so there is no Store API React app. Pages imported from WordPress keep
// WooCommerce's block markup: the inner blocks (woocommerce/cart-cross-sells-block,
// woocommerce/checkout-order-note-block, …) are read as settings — a section whose inner block was
// removed is not shown — and blocks Pursae does not know are ignored. goocommerce/cart and
// goocommerce/checkout are aliases.

// gooCartView is what the cart page shows.
type gooCartView struct {
	CrossSells   bool   // the cross-sells under the cart
	Coupon       bool   // the coupon form (when coupons are enabled)
	EmptyMessage string // the empty cart notice ("" the default one)
	EmptyHTML    string // the Empty cart block's rendered content, shown instead of the notice
}

func gooDefaultCartView() gooCartView { return gooCartView{CrossSells: true, Coupon: true} }

// gooCheckoutView is what the checkout page shows.
type gooCheckoutView struct {
	LoginReminder bool // "Returning customer?" (when the store setting is on)
	Coupon        bool // "Have a coupon?" (when coupons are enabled)
	OrderNotes    bool // the order notes field
	ReturnToCart  bool // a link back to the cart under the order review
	// Headings ("" the defaults).
	BillingTitle, AdditionalTitle, OrderTitle string
}

func gooDefaultCheckoutView() gooCheckoutView {
	return gooCheckoutView{LoginReminder: true, Coupon: true, OrderNotes: true}
}

// gooCheckoutFieldAttrs maps the Checkout block's field attributes onto the checkout field options
// (hidden / optional / required): {show attribute, require attribute}.
var gooCheckoutFieldAttrs = map[string][2]string{
	"checkout_company_field":   {"showCompanyField", "requireCompanyField"},
	"checkout_address_2_field": {"showApartmentField", "requireApartmentField"},
	"checkout_phone_field":     {"showPhoneField", "requirePhoneField"},
}

func gooBlockBool(b *blocks.Block, k string) (bool, bool) {
	v, ok := b.Attrs[k].(bool)
	return v, ok
}

func gooBlockString(b *blocks.Block, k string) string {
	v, _ := b.Attrs[k].(string)
	return strings.TrimSpace(v)
}

// gooFindBlock is the first block named name in the tree under b (b excluded).
func gooFindBlock(b *blocks.Block, name string) *blocks.Block {
	for _, in := range b.InnerBlocks {
		if in.Name() == name {
			return in
		}
		if f := gooFindBlock(in, name); f != nil {
			return f
		}
	}
	return nil
}

// gooPruneUnknown is a copy of b without the woocommerce/* blocks no renderer is registered for
// (WooCommerce's React-only inner blocks), so they are skipped instead of leaving their saved
// placeholders.
func gooPruneUnknown(b *blocks.Block) *blocks.Block {
	out := *b
	out.InnerBlocks, out.InnerContent = nil, nil
	i := 0
	for _, chunk := range b.InnerContent {
		if chunk != nil {
			out.InnerContent = append(out.InnerContent, chunk)
			continue
		}
		if i >= len(b.InnerBlocks) {
			continue
		}
		in := b.InnerBlocks[i]
		i++
		if n := in.Name(); strings.HasPrefix(n, "woocommerce/") && !blocks.IsDynamic(n) {
			continue
		}
		out.InnerBlocks = append(out.InnerBlocks, gooPruneUnknown(in))
		out.InnerContent = append(out.InnerContent, nil)
	}
	return &out
}

// gooCartViewOf reads a Cart block: the inner blocks of WooCommerce's markup first, then the
// block's own attributes.
func gooCartViewOf(ctx context.Context, b *blocks.Block) gooCartView {
	v := gooDefaultCartView()
	if filled := gooFindBlock(b, "woocommerce/filled-cart-block"); filled != nil {
		v.CrossSells = gooFindBlock(filled, "woocommerce/cart-cross-sells-block") != nil
		if gooFindBlock(filled, "woocommerce/cart-totals-block") != nil {
			v.Coupon = gooFindBlock(filled, "woocommerce/cart-order-summary-coupon-form-block") != nil
		}
	}
	if empty := gooFindBlock(b, "woocommerce/empty-cart-block"); empty != nil && len(empty.InnerBlocks) > 0 {
		v.EmptyHTML = strings.TrimSpace(blocks.RenderInner(ctx, gooPruneUnknown(empty)))
	}
	if x, ok := gooBlockBool(b, "showCrossSells"); ok {
		v.CrossSells = x
	}
	if x, ok := gooBlockBool(b, "showCouponForm"); ok {
		v.Coupon = x
	}
	if m := gooBlockString(b, "emptyCartMessage"); m != "" {
		v.EmptyMessage, v.EmptyHTML = m, ""
	}
	return v
}

// gooCheckoutViewOf reads a Checkout block like gooCartViewOf.
func gooCheckoutViewOf(b *blocks.Block) gooCheckoutView {
	v := gooDefaultCheckoutView()
	v.ReturnToCart = true // the block's default (WooCommerce's showReturnToCart)
	if fields := gooFindBlock(b, "woocommerce/checkout-fields-block"); fields != nil {
		v.OrderNotes = gooFindBlock(fields, "woocommerce/checkout-order-note-block") != nil
		if bb := gooFindBlock(fields, "woocommerce/checkout-billing-address-block"); bb != nil {
			v.BillingTitle = gooBlockString(bb, "title")
		}
		if ab := gooFindBlock(fields, "woocommerce/checkout-additional-information-block"); ab != nil {
			v.AdditionalTitle = gooBlockString(ab, "title")
		}
		if act := gooFindBlock(fields, "woocommerce/checkout-actions-block"); act != nil {
			if x, ok := gooBlockBool(act, "showReturnToCart"); ok {
				v.ReturnToCart = x
			}
		}
	}
	if totals := gooFindBlock(b, "woocommerce/checkout-totals-block"); totals != nil {
		v.Coupon = gooFindBlock(totals, "woocommerce/checkout-order-summary-coupon-form-block") != nil
	}
	// The block's own attributes win (set in Pursae's editor, or by WooCommerce before 8.x).
	for attr, dst := range map[string]*bool{"showLoginReminder": &v.LoginReminder, "showCouponForm": &v.Coupon,
		"showOrderNotes": &v.OrderNotes, "showReturnToCart": &v.ReturnToCart} {
		if x, ok := gooBlockBool(b, attr); ok {
			*dst = x
		}
	}
	for attr, dst := range map[string]*string{"billingTitle": &v.BillingTitle, "additionalTitle": &v.AdditionalTitle, "orderTitle": &v.OrderTitle} {
		if t := gooBlockString(b, attr); t != "" {
			*dst = t
		}
	}
	return v
}

// gooCheckoutFieldModesOf is the field options a Checkout block sets (only the fields it has
// attributes for).
func gooCheckoutFieldModesOf(b *blocks.Block) map[string]string {
	out := map[string]string{}
	for opt, at := range gooCheckoutFieldAttrs {
		show, hasShow := gooBlockBool(b, at[0])
		req, hasReq := gooBlockBool(b, at[1])
		switch {
		case !hasShow && !hasReq:
			continue
		case hasShow && !show:
			out[opt] = "hidden"
		case req:
			out[opt] = "required"
		default:
			out[opt] = "optional"
		}
	}
	return out
}

func gooIsCheckoutBlock(name string) bool {
	return name == "woocommerce/checkout" || name == "goocommerce/checkout"
}

// gooFindCheckoutBlock is the first Checkout block of a page's content.
func gooFindCheckoutBlock(content string) *blocks.Block {
	if !strings.Contains(content, "wp:woocommerce/checkout") && !strings.Contains(content, "wp:goocommerce/checkout") {
		return nil
	}
	var walk func([]*blocks.Block) *blocks.Block
	walk = func(list []*blocks.Block) *blocks.Block {
		for _, b := range list {
			if gooIsCheckoutBlock(b.Name()) {
				return b
			}
			if f := walk(b.InnerBlocks); f != nil {
				return f
			}
		}
		return nil
	}
	return walk(blocks.Parse(content))
}

// gooCheckoutBlockFieldModes is the field settings of the Checkout block on the store's checkout
// page. They apply to the form and to its validation alike (and to My account's address forms, as
// the options do), so a field the block hides is never required.
func (s *Server) gooCheckoutBlockFieldModes(ctx context.Context) map[string]string {
	id := s.gooPageID("checkout")
	if id <= 0 || s.store == nil {
		return nil
	}
	p, err := s.store.GetPost(ctx, id)
	if err != nil {
		return nil
	}
	if b := gooFindCheckoutBlock(p.Content); b != nil {
		return gooCheckoutFieldModesOf(b)
	}
	return nil
}

// gooPageHasStoreBlock reports whether page content holds the Cart (key "cart") or Checkout block.
func gooPageHasStoreBlock(key, content string) bool {
	return strings.Contains(content, "<!-- wp:woocommerce/"+key+" ") || strings.Contains(content, "<!-- wp:woocommerce/"+key+" -->") ||
		strings.Contains(content, "<!-- wp:goocommerce/"+key+" ") || strings.Contains(content, "<!-- wp:goocommerce/"+key+" -->")
}

func registerGooCartBlocks(s *Server) {
	cart := func(ctx context.Context, b *blocks.Block, _ string) string {
		if !s.gooOn() {
			return ""
		}
		c := gooCtxGin(ctx)
		return s.gooL10n(c, s.gooCartPageView(c, gooCartViewOf(ctx, b)))
	}
	checkout := func(ctx context.Context, b *blocks.Block, _ string) string {
		if !s.gooOn() {
			return ""
		}
		c := gooCtxGin(ctx)
		return s.gooL10n(c, s.gooCheckoutPageView(c, gooCheckoutViewOf(b)))
	}
	// Containers: the inner blocks are settings, not markup, so they are not rendered.
	for _, p := range []string{"woocommerce/", "goocommerce/"} {
		blocks.RegisterContainer(p+"cart", cart)
		blocks.RegisterContainer(p+"checkout", checkout)
	}
}
