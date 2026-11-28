package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/blocks"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// The single product page (content-single-product.php): gallery, summary, add-to-cart forms of every
// product type, meta, tabs (description, additional information, reviews, modules' tabs), upsells,
// related products, the review form and the product's structured data.

// renderPostContent is the_content's formatting of stored content: blocks, else wpautop.
func (s *Server) renderPostContent(ctx context.Context, content string) string {
	if blocks.HasBlocks(content) {
		return blocks.Render(ctx, content)
	}
	return wp.Autop(wp.Texturize(content))
}

// gooSingleProductHTML is the single product page markup.
func (s *Server) gooSingleProductHTML(c *gin.Context, p *gooProduct) string {
	ctx := c.Request.Context()
	fv := gooViewFrom(c)
	var b strings.Builder
	b.WriteString(`<div class="woocommerce">`)
	if fv != nil {
		b.WriteString(s.gooBreadcrumb(c, fv))
	}
	b.WriteString(s.gooNoticesHTML(c))
	b.WriteString(s.gooProductBody(c, p, s.gooThemeSupports()))
	b.WriteString(`</div>`)
	_ = ctx
	return b.String()
}

// gooProductBody is the product's div (shared with the [product_page] shortcode).
func (s *Server) gooProductBody(c *gin.Context, p *gooProduct, showTitle bool) string {
	ctx := c.Request.Context()
	var b strings.Builder
	b.WriteString(`<div id="product-` + itoa64(p.ID) + `" class="` + s.gooProductClasses(c, p, "") + `">`)
	if s.gooOnSale(c, p) {
		b.WriteString(`<span class="onsale">Sale!</span>`)
	}
	b.WriteString(s.gooGalleryHTML(c, p))
	b.WriteString(`<div class="summary entry-summary">`)
	if showTitle {
		b.WriteString(`<h1 class="product_title entry-title">` + s.pllUC(c, gooEsc(p.Name), 0) + `</h1>`)
	}
	b.WriteString(s.gooRatingBlockHTML(p))
	b.WriteString(`<p class="price">` + string(s.gooPriceHTML(c, p)) + `</p>`)
	if ex := strings.TrimSpace(p.Post.Excerpt); ex != "" {
		b.WriteString(`<div class="woocommerce-product-details__short-description">` + s.gooRenderContent(ctx, c, ex) + `</div>`)
	}
	b.WriteString(s.gooAddToCartForm(c, p))
	b.WriteString(s.gooProductMeta(c, p))
	b.WriteString(string(s.gooHTML(c, "single_summary_end", p)))
	b.WriteString(`</div>`)
	b.WriteString(s.gooTabsHTML(c, p))
	b.WriteString(s.gooUpsellsHTML(c, p))
	b.WriteString(s.gooRelatedHTML(c, p))
	b.WriteString(`</div>`)
	return b.String()
}

// gooRatingBlockHTML is single-product/rating.php: the stars and the review link ("" while
// ratings are off or the product has none).
func (s *Server) gooRatingBlockHTML(p *gooProduct) string {
	if !s.gooOptBool("enable_review_rating") || p.RatingCount <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="woocommerce-product-rating">` + gooRatingHTML(p.AverageRating, p.RatingCount))
	if s.gooReviewsOpen(p) {
		n := strconv.Itoa(p.RatingCount)
		word := "customer reviews"
		if p.RatingCount == 1 {
			word = "customer review"
		}
		b.WriteString(`<a href="#reviews" class="woocommerce-review-link" rel="nofollow">(<span class="count">` + n + `</span> ` + word + `)</a>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// gooGalleryHTML is woocommerce_show_product_images().
func (s *Server) gooGalleryHTML(c *gin.Context, p *gooProduct) string {
	ctx := c.Request.Context()
	ids := []int64{}
	if p.ImageID > 0 {
		ids = append(ids, p.ImageID)
	}
	for _, id := range p.GalleryIDs {
		if id > 0 && id != p.ImageID {
			ids = append(ids, id)
		}
	}
	cls := "woocommerce-product-gallery woocommerce-product-gallery--columns-4 images"
	if len(ids) > 0 {
		cls = "woocommerce-product-gallery woocommerce-product-gallery--with-images woocommerce-product-gallery--columns-4 images"
	}
	var b strings.Builder
	b.WriteString(`<div class="` + cls + `" data-columns="4" style="opacity: 0; transition: opacity .25s ease-in-out;"><div class="woocommerce-product-gallery__wrapper">`)
	if len(ids) == 0 {
		b.WriteString(`<div class="woocommerce-product-gallery__image--placeholder">` + s.gooPlaceholderImg(ctx, "") + `</div>`)
	}
	s.gooPrefetchImages(c, ids)
	for i, id := range ids {
		a, err := s.gooLoopAttachment(c, id)
		if err != nil {
			continue
		}
		full := s.uploadURL(ctx, a.FilePath)
		thumb, _, _ := s.gooImageURLOf(ctx, a, "thumbnail")
		if thumb == "" {
			thumb = full
		}
		size, class := "large", "wp-post-image"
		if i > 0 {
			size, class = "thumbnail", ""
		}
		img := string(s.attachmentImage(ctx, a, size, class))
		img = strings.Replace(img, "<img ", `<img data-large_image="`+gooEsc(full)+`" data-large_image_width="`+strconv.Itoa(a.Width)+`" data-large_image_height="`+strconv.Itoa(a.Height)+`" title="`+gooEsc(a.Title)+`" `, 1)
		b.WriteString(`<div data-thumb="` + gooEsc(thumb) + `" data-thumb-alt="` + gooEsc(a.Alt) + `" class="woocommerce-product-gallery__image"><a href="` + gooEsc(full) + `">` + img + `</a></div>`)
	}
	b.WriteString(`</div></div>`)
	return b.String()
}

// gooProductMeta is meta.php: SKU, categories, tags.
func (s *Server) gooProductMeta(c *gin.Context, p *gooProduct) string {
	ctx := c.Request.Context()
	var b strings.Builder
	b.WriteString(`<div class="product_meta">`)
	if p.SKU != "" || gooSubsVariableLike(p.Type) {
		b.WriteString(`<span class="sku_wrapper">SKU: <span class="sku">` + gooEsc(defaultStr(p.SKU, "N/A")) + `</span></span>`)
	}
	terms, _ := s.gooProductTerms(c, p.ID)
	list := func(tax, one, many, cls string) {
		var links []string
		for _, t := range terms {
			if t.Taxonomy == tax {
				links = append(links, `<a href="`+gooEsc(s.gooTermLink(ctx, t))+`" rel="tag">`+gooEsc(t.Name)+`</a>`)
			}
		}
		if len(links) == 0 {
			return
		}
		label := one
		if len(links) > 1 {
			label = many
		}
		b.WriteString(` <span class="` + cls + `">` + label + ` ` + strings.Join(links, ", ") + `</span>`)
	}
	list("product_cat", "Category:", "Categories:", "posted_in")
	list("product_tag", "Tag:", "Tags:", "tagged_as")
	b.WriteString(string(s.gooHTML(c, "single_meta_end", p)))
	b.WriteString(`</div>`)
	return b.String()
}

// Add to cart forms --------------------------------------------------------------------------------------

// gooQtyLimits are the quantity input's min / max for a product (max 0 = unlimited).
func (s *Server) gooQtyLimits(ctx context.Context, p *gooProduct) (int, int) {
	max := 0
	if s.gooManagingStock(ctx, p) && !s.gooBackordersAllowed(ctx, p) {
		max = s.gooStockQty(ctx, p)
	}
	if p.SoldIndividually {
		max = 1
	}
	return 1, max
}

// gooAddToCartForm is woocommerce_template_single_add_to_cart() for each product type.
func (s *Server) gooAddToCartForm(c *gin.Context, p *gooProduct) string {
	ctx := c.Request.Context()
	action := s.gooPermalink(ctx, p)
	text := s.gooAddToCartText(c, p, true)
	var b strings.Builder
	switch p.Type {
	case gooTypeExternal:
		if p.ProductURL == "" {
			return ""
		}
		b.WriteString(`<form class="cart" action="` + gooEsc(p.ProductURL) + `" method="get">`)
		b.WriteString(string(s.gooHTML(c, "single_before_add_to_cart", p)))
		b.WriteString(`<button type="submit" class="single_add_to_cart_button button alt">` + gooEsc(text) + `</button>`)
		if u, err := url.Parse(p.ProductURL); err == nil {
			for k, vals := range u.Query() {
				for _, v := range vals {
					b.WriteString(`<input type="hidden" name="` + gooEsc(k) + `" value="` + gooEsc(v) + `" />`)
				}
			}
		}
		b.WriteString(`</form>`)
	case gooTypeGrouped:
		return s.gooGroupedForm(c, p, action)
	case gooTypeVariable, gooSubsTypeVariable:
		return s.gooVariableForm(c, p, action)
	default:
		b.WriteString(s.gooFrontStockHTML(c, p))
		if !s.gooIsPurchasable(ctx, p) || !s.gooIsInStock(ctx, p) {
			return b.String()
		}
		lo, hi := s.gooQtyLimits(ctx, p)
		b.WriteString(`<form class="cart" action="` + gooEsc(action) + `" method="post" enctype="multipart/form-data">`)
		b.WriteString(string(s.gooHTML(c, "single_before_add_to_cart", p)))
		qty := lo
		if v := gooAtoi(c.PostForm("quantity")); v > 0 {
			qty = v
		}
		b.WriteString(gooQuantityInput("quantity", p.Name, qty, lo, hi, "quantity_"+itoa64(p.ID), p.SoldIndividually))
		b.WriteString(`<button type="submit" name="add-to-cart" value="` + itoa64(p.ID) + `" class="single_add_to_cart_button button alt">` + gooEsc(text) + `</button>`)
		b.WriteString(`</form>`)
	}
	b.WriteString(string(s.gooHTML(c, "single_after_add_to_cart", p)))
	return b.String()
}

// gooGroupedForm is grouped.php.
func (s *Server) gooGroupedForm(c *gin.Context, p *gooProduct, action string) string {
	ctx := c.Request.Context()
	kids, _ := s.gooGetProducts(ctx, p.Children)
	var b strings.Builder
	b.WriteString(`<form class="cart grouped_form" action="` + gooEsc(action) + `" method="post" enctype="multipart/form-data">`)
	b.WriteString(string(s.gooHTML(c, "single_before_add_to_cart", p)))
	b.WriteString(`<table cellspacing="0" class="woocommerce-grouped-product-list group_table"><tbody>`)
	anyBuy := false
	for _, id := range p.Children {
		k := kids[id]
		if k == nil || k.Status != "publish" {
			continue
		}
		b.WriteString(`<tr id="product-` + itoa64(k.ID) + `" class="woocommerce-grouped-product-list-item ` + s.gooProductClasses(c, k, "") + `">`)
		b.WriteString(`<td class="woocommerce-grouped-product-list-item__quantity">`)
		buy := !gooSubsVariableLike(k.Type) && k.Type != gooTypeGrouped && k.Type != gooTypeExternal && s.gooIsPurchasable(ctx, k) && s.gooIsInStock(ctx, k)
		switch {
		case !buy:
			b.WriteString(s.gooLoopAddToCart(c, k, ""))
		case k.SoldIndividually:
			anyBuy = true
			b.WriteString(`<input type="checkbox" name="quantity[` + itoa64(k.ID) + `]" value="1" class="wc-grouped-product-add-to-cart-checkbox" id="quantity-` + itoa64(k.ID) + `" />` +
				`<label for="quantity-` + itoa64(k.ID) + `" class="screen-reader-text">Buy one of this item</label>`)
		default:
			anyBuy = true
			_, hi := s.gooQtyLimits(ctx, k)
			b.WriteString(strings.Replace(gooQuantityInput("quantity["+itoa64(k.ID)+"]", k.Name, -1, 0, hi, "quantity_"+itoa64(k.ID), false), `min="1"`, `min="0"`, 1))
		}
		b.WriteString(`</td><td class="woocommerce-grouped-product-list-item__label"><label for="product-` + itoa64(k.ID) + `"><a href="` + gooEsc(s.gooPermalink(ctx, k)) + `">` + gooEsc(k.Name) + `</a></label></td>`)
		b.WriteString(`<td class="woocommerce-grouped-product-list-item__price">` + string(s.gooPriceHTML(c, k)) + s.gooFrontStockHTML(c, k) + `</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	b.WriteString(`<input type="hidden" name="add-to-cart" value="` + itoa64(p.ID) + `" />`)
	if anyBuy {
		b.WriteString(`<button type="submit" class="single_add_to_cart_button button alt">` + gooEsc(s.gooAddToCartText(c, p, true)) + `</button>`)
	}
	b.WriteString(`</form>`)
	b.WriteString(string(s.gooHTML(c, "single_after_add_to_cart", p)))
	return b.String()
}

// gooVariationAttrs are the attributes used for variations, in order.
func gooVariationAttrs(p *gooProduct) []gooProductAttr {
	var out []gooProductAttr
	for _, a := range p.Attributes {
		if a.Variation {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

// gooVariableForm is variable.php: the attribute selects and data-product_variations.
func (s *Server) gooVariableForm(c *gin.Context, p *gooProduct, action string) string {
	ctx := c.Request.Context()
	vars := s.gooVariationsCached(c, p)
	var varImgs []int64
	for _, v := range vars {
		if v.ImageID > 0 {
			varImgs = append(varImgs, v.ImageID)
		} else if p.ImageID > 0 {
			varImgs = append(varImgs, p.ImageID)
		}
	}
	s.gooPrefetchImages(c, varImgs)
	var data []map[string]any
	for _, v := range vars {
		if d := s.gooVariationData(c, p, v); d != nil {
			data = append(data, d)
		}
	}
	raw, _ := json.Marshal(data)
	if len(data) == 0 {
		raw = []byte("false")
	}
	var b strings.Builder
	b.WriteString(`<form class="variations_form cart" action="` + gooEsc(action) + `" method="post" enctype="multipart/form-data" data-product_id="` + itoa64(p.ID) + `" data-product_variations="` + html.EscapeString(string(raw)) + `">`)
	b.WriteString(string(s.gooHTML(c, "single_before_add_to_cart", p)))
	if len(data) == 0 {
		b.WriteString(`<p class="stock out-of-stock">This product is currently out of stock and unavailable.</p>`)
	} else {
		b.WriteString(`<table class="variations" cellspacing="0" role="presentation"><tbody>`)
		attrs := gooVariationAttrs(p)
		for i, a := range attrs {
			key := "attribute_" + a.Key()
			selected := c.Query(key)
			if selected == "" {
				selected = p.DefaultAttributes[a.Key()]
			}
			label := s.gooAttrLabel(ctx, a)
			b.WriteString(`<tr><th class="label"><label for="` + gooEsc(a.Key()) + `">` + gooEsc(label) + `</label></th><td class="value">`)
			b.WriteString(`<select id="` + gooEsc(a.Key()) + `" class="" name="` + gooEsc(key) + `" data-attribute_name="` + gooEsc(key) + `" data-show_option_none="yes">`)
			b.WriteString(`<option value="">Choose an option</option>`)
			for _, opt := range s.gooAttrOptions(ctx, a) {
				sel := ""
				if strings.EqualFold(opt.Value, selected) {
					sel = ` selected='selected'`
				}
				b.WriteString(`<option value="` + gooEsc(opt.Value) + `" class="attached enabled"` + sel + `>` + gooEsc(opt.Label) + `</option>`)
			}
			b.WriteString(`</select>`)
			if i == len(attrs)-1 {
				b.WriteString(`<a class="reset_variations" href="#" aria-label="Clear options">Clear</a>`)
			}
			b.WriteString(`</td></tr>`)
		}
		b.WriteString(`</tbody></table><div class="reset_variations_alert screen-reader-text" role="alert" aria-live="polite" aria-relevant="all"></div>`)
		lo, _ := s.gooQtyLimits(ctx, p)
		b.WriteString(`<div class="single_variation_wrap"><div class="woocommerce-variation single_variation" role="alert" aria-relevant="additions"></div>`)
		b.WriteString(`<div class="woocommerce-variation-add-to-cart variations_button">`)
		b.WriteString(gooQuantityInput("quantity", p.Name, lo, lo, 0, "quantity_"+itoa64(p.ID), p.SoldIndividually))
		b.WriteString(`<button type="submit" class="single_add_to_cart_button button alt disabled wc-variation-selection-needed">` + gooEsc(s.gooAddToCartText(c, p, true)) + `</button>`)
		b.WriteString(`<input type="hidden" name="add-to-cart" value="` + itoa64(p.ID) + `" /><input type="hidden" name="product_id" value="` + itoa64(p.ID) + `" /><input type="hidden" name="variation_id" class="variation_id" value="0" />`)
		b.WriteString(`</div></div>`)
	}
	b.WriteString(`</form>`)
	b.WriteString(string(s.gooHTML(c, "single_after_add_to_cart", p)))
	return b.String()
}

// gooAttrOptions are an attribute's values with labels (taxonomy terms in the attribute's order).
func (s *Server) gooAttrOptions(ctx context.Context, a gooProductAttr) []gooOpt {
	var out []gooOpt
	if a.Taxonomy != "" {
		terms, _ := s.gooAttributeTerms(ctx, a.Taxonomy)
		for _, t := range terms {
			if containsStr(a.Options, t.Slug) {
				out = append(out, gooOpt{t.Slug, t.Name})
			}
		}
		return out
	}
	for _, o := range a.Options {
		out = append(out, gooOpt{o, o})
	}
	return out
}

// gooVariationData is WC_Product_Variable::get_available_variation() (nil when not visible).
func (s *Server) gooVariationData(c *gin.Context, parent, v *gooProduct) map[string]any {
	ctx := c.Request.Context()
	v.parent = parent
	active := s.gooActive(c, v)
	if active == nil {
		return nil // variations without a price are hidden
	}
	attrs := map[string]string{}
	for _, a := range gooVariationAttrs(parent) {
		attrs["attribute_"+a.Key()] = v.VariationAttrs[a.Key()]
	}
	display := s.gooToDisplay(c, v, *active)
	regular := display
	if v.RegularPrice != nil {
		regular = s.gooToDisplay(c, v, *v.RegularPrice)
	}
	lo, hi := s.gooQtyLimits(ctx, v)
	var maxQty any = ""
	if hi > 0 {
		maxQty = hi
	}
	priceHTML := ""
	if ph := s.gooPriceHTML(c, v); ph != "" {
		priceHTML = `<span class="price">` + string(ph) + `</span>`
	}
	img := map[string]any{}
	imgID := s.gooImageOf(ctx, v)
	if imgID > 0 {
		if a, err := s.gooLoopAttachment(c, imgID); err == nil {
			src, w, h := s.gooImageURLOf(ctx, a, "large")
			thumb, tw, th := s.gooImageURLOf(ctx, a, "thumbnail")
			img = map[string]any{"title": a.Title, "caption": a.Caption, "url": s.uploadURL(ctx, a.FilePath), "alt": a.Alt, "src": src, "srcset": "", "sizes": "",
				"full_src": s.uploadURL(ctx, a.FilePath), "full_src_w": a.Width, "full_src_h": a.Height, "gallery_thumbnail_src": thumb,
				"gallery_thumbnail_src_w": tw, "gallery_thumbnail_src_h": th, "thumb_src": thumb, "thumb_src_w": tw, "thumb_src_h": th, "src_w": w, "src_h": h}
		}
	}
	weight := v.Weight
	if weight == "" {
		weight = parent.Weight
	}
	weightHTML := "N/A"
	if weight != "" {
		weightHTML = weight + " " + s.gooOptString("weight_unit")
	}
	dims := s.gooDimensions(v, parent)
	if dims == "" {
		dims = "N/A"
	}
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	desc := ""
	if strings.TrimSpace(v.Description) != "" {
		desc = wp.Autop(wp.KsesPost(v.Description))
	}
	availability := s.gooL10n(c, s.gooFrontStockHTML(c, v))
	weightHTML, dims = s.gooL10nText(ctx, weightHTML), s.gooL10nText(ctx, dims)
	return map[string]any{
		"attributes":            attrs,
		"availability_html":     availability,
		"backorders_allowed":    s.gooBackordersAllowed(ctx, v),
		"dimensions":            map[string]string{"length": v.Length, "width": v.Width, "height": v.Height},
		"dimensions_html":       dims,
		"display_price":         display,
		"display_regular_price": regular,
		"image":                 img,
		"image_id":              imgID,
		"is_downloadable":       v.Downloadable,
		"is_in_stock":           s.gooIsInStock(ctx, v),
		"is_purchasable":        s.gooIsPurchasable(ctx, v),
		"is_sold_individually":  yn(v.SoldIndividually || parent.SoldIndividually),
		"is_virtual":            v.Virtual,
		"max_qty":               maxQty,
		"min_qty":               lo,
		"price_html":            priceHTML,
		"sku":                   defaultStr(v.SKU, parent.SKU),
		"variation_description": desc,
		"variation_id":          v.ID,
		"variation_is_active":   true,
		"variation_is_visible":  true,
		"weight":                weight,
		"weight_html":           weightHTML,
	}
}

// gooDimensions is wc_format_dimensions() ("" when none).
func (s *Server) gooDimensions(p, parent *gooProduct) string {
	l, w, h := p.Length, p.Width, p.Height
	if parent != nil {
		l, w, h = defaultStr(l, parent.Length), defaultStr(w, parent.Width), defaultStr(h, parent.Height)
	}
	var parts []string
	for _, d := range []string{l, w, h} {
		if strings.TrimSpace(d) != "" {
			parts = append(parts, d)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " &times; ") + " " + s.gooOptString("dimension_unit")
}

// Tabs ------------------------------------------------------------------------------------------------------

type gooProdTab struct {
	key, title string
	prio       int
	body       func() string
}

// gooTabsHTML is woocommerce_output_product_data_tabs().
func (s *Server) gooTabsHTML(c *gin.Context, p *gooProduct) string {
	ctx := c.Request.Context()
	var tabs []gooProdTab
	if strings.TrimSpace(p.Post.Content) != "" {
		tabs = append(tabs, gooProdTab{"description", "Description", 10, func() string {
			return `<h2>Description</h2>` + s.gooRenderContent(ctx, c, p.Post.Content)
		}})
	}
	if t := s.gooAttributesTable(c, p); t != "" {
		tabs = append(tabs, gooProdTab{"additional_information", "Additional information", 20, func() string { return `<h2>Additional information</h2>` + t }})
	}
	if s.gooReviewsOpen(p) {
		tabs = append(tabs, gooProdTab{"reviews", fmt.Sprintf("Reviews (%d)", p.RatingCount), 30, func() string { return s.gooReviewsHTML(c, p) }})
	}
	for _, mt := range gooProductTabs {
		title := ""
		if mt.Title != nil {
			title = mt.Title(s, c, p)
		}
		if title == "" {
			continue
		}
		tabs = append(tabs, gooProdTab{mt.Key, title, mt.Priority, func() string { return string(mt.Render(s, c, p)) }})
	}
	if len(tabs) == 0 {
		return ""
	}
	sort.SliceStable(tabs, func(i, j int) bool { return tabs[i].prio < tabs[j].prio })
	var b strings.Builder
	b.WriteString(`<div class="woocommerce-tabs wc-tabs-wrapper"><ul class="tabs wc-tabs" role="tablist">`)
	for i, t := range tabs {
		active := ""
		if i == 0 {
			active = " active"
		}
		b.WriteString(`<li role="presentation" class="` + gooEsc(t.key) + `_tab` + active + `" id="tab-title-` + gooEsc(t.key) + `"><a href="#tab-` + gooEsc(t.key) + `" role="tab" aria-controls="tab-` + gooEsc(t.key) + `">` + gooEsc(t.title) + `</a></li>`)
	}
	b.WriteString(`</ul>`)
	for _, t := range tabs {
		b.WriteString(`<div class="woocommerce-Tabs-panel woocommerce-Tabs-panel--` + gooEsc(t.key) + ` panel entry-content wc-tab" id="tab-` + gooEsc(t.key) + `" role="tabpanel" aria-labelledby="tab-title-` + gooEsc(t.key) + `">`)
		b.WriteString(t.body())
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// gooAttributesTable is wc_display_product_attributes() ("" when there is nothing to show).
func (s *Server) gooAttributesTable(c *gin.Context, p *gooProduct) string {
	ctx := c.Request.Context()
	type row struct{ key, label, value string }
	var rows []row
	if p.Type != gooTypeExternal && p.Type != gooTypeGrouped && !p.Virtual {
		if p.Weight != "" {
			rows = append(rows, row{"weight", "Weight", gooEsc(p.Weight + " " + s.gooOptString("weight_unit"))})
		}
		if d := s.gooDimensions(p, nil); d != "" {
			rows = append(rows, row{"dimensions", "Dimensions", d})
		}
	}
	for _, a := range p.Attributes {
		if !a.Visible {
			continue
		}
		var vals []string
		for _, opt := range s.gooAttrOptions(ctx, a) {
			v := gooEsc(opt.Label)
			if a.Taxonomy != "" {
				if ga := s.gooAttributeByTaxonomy(ctx, a.Taxonomy); ga != nil && ga.HasArchives {
					if t, err := s.store.TermBySlug(ctx, a.Taxonomy, opt.Value); err == nil {
						v = `<a href="` + gooEsc(s.gooTermLink(ctx, t)) + `" rel="tag">` + v + `</a>`
					}
				}
			}
			vals = append(vals, v)
		}
		if len(vals) == 0 {
			continue
		}
		rows = append(rows, row{"attribute_" + a.Key(), s.gooAttrLabel(ctx, a), "<p>" + strings.Join(vals, ", ") + "</p>"})
	}
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<table class="woocommerce-product-attributes shop_attributes" aria-label="Product Details">`)
	for _, r := range rows {
		b.WriteString(`<tr class="woocommerce-product-attributes-item woocommerce-product-attributes-item--` + gooEsc(r.key) + `"><th class="woocommerce-product-attributes-item__label" scope="row">` +
			gooEsc(r.label) + `</th><td class="woocommerce-product-attributes-item__value">` + r.value + `</td></tr>`)
	}
	b.WriteString(`</table>`)
	return b.String()
}

// Upsells, related -------------------------------------------------------------------------------------------

func (s *Server) gooVisibleProducts(c *gin.Context, ids []int64) []*gooProduct {
	if len(ids) == 0 {
		return nil
	}
	list, _, _ := s.gooQueryProducts(c.Request.Context(), gooProductQuery{IDs: ids, Visibility: "catalog", OrderBy: "include", PerPage: -1})
	return list
}

// gooUpsellsHTML is woocommerce_upsell_display().
func (s *Server) gooUpsellsHTML(c *gin.Context, p *gooProduct) string {
	list := s.gooVisibleProducts(c, p.UpsellIDs)
	if len(list) == 0 {
		return ""
	}
	return `<section class="up-sells upsells products"><h2>You may also like&hellip;</h2>` + s.gooLoopHTML(c, list, s.gooColumns()) + `</section>`
}

// gooRelatedHTML is woocommerce_output_related_products(): same categories or tags, random.
func (s *Server) gooRelatedHTML(c *gin.Context, p *gooProduct) string {
	terms, _ := s.gooProductTerms(c, p.ID)
	var ids []int64
	for _, t := range terms {
		if t.Taxonomy == "product_cat" || t.Taxonomy == "product_tag" {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	cols := s.gooColumns()
	list, _, _ := s.gooQueryProducts(c.Request.Context(), gooProductQuery{Category: ids, Exclude: append([]int64{p.ID}, p.UpsellIDs...),
		Visibility: "catalog", OrderBy: "rand", PerPage: cols, InStock: s.gooOptBool("hide_out_of_stock_items")})
	if len(list) == 0 {
		return ""
	}
	return `<section class="related products"><h2>Related products</h2>` + s.gooLoopHTML(c, list, cols) + `</section>`
}

// Reviews ---------------------------------------------------------------------------------------------------

// gooReviewsOpen: reviews are enabled in the settings and on the product.
func (s *Server) gooReviewsOpen(p *gooProduct) bool {
	return s.gooOptBool("enable_reviews") && p.Post != nil && p.Post.CommentStatus == "open"
}

// gooReviewsHTML is single-product-reviews.php.
func (s *Server) gooReviewsHTML(c *gin.Context, p *gooProduct) string {
	ctx := c.Request.Context()
	u := currentUser(c)
	pending := ""
	if u != nil {
		pending = u.Email
	} else if ck, err := c.Cookie("comment_author_email"); err == nil {
		pending, _ = url.QueryUnescape(ck)
	}
	all, _ := s.store.ApprovedComments(ctx, p.ID, "asc", pending)
	all = s.visiblePendingComments(c, all, u)
	var reviews []*store.Comment
	for _, cm := range all {
		if cm.Type == "review" || cm.Type == "comment" || cm.Type == "" {
			reviews = append(reviews, cm)
		}
	}
	approved := 0
	for _, r := range reviews {
		if r.Status == "approved" {
			approved++
		}
	}
	var b strings.Builder
	b.WriteString(`<div id="reviews" class="woocommerce-Reviews"><div id="comments">`)
	if approved > 0 {
		word := "reviews"
		if approved == 1 {
			word = "review"
		}
		b.WriteString(fmt.Sprintf(`<h2 class="woocommerce-Reviews-title">%d %s for <span>%s</span></h2>`, approved, word, gooEsc(p.Name)))
	} else {
		b.WriteString(`<h2 class="woocommerce-Reviews-title">Reviews</h2>`)
	}
	if len(reviews) > 0 {
		b.WriteString(`<ol class="commentlist">`)
		df := s.settings.String("date_format")
		for i, r := range reviews {
			parity := "even"
			if i%2 == 1 {
				parity = "odd"
			}
			cls := "review " + parity + " thread-" + parity + " depth-1"
			if r.UserID != 0 {
				cls = "review byuser comment-author-" + gooSlugRe.ReplaceAllString(strings.ToLower(r.AuthorName), "-") + " " + parity + " thread-" + parity + " depth-1"
			}
			b.WriteString(`<li class="` + cls + `" id="li-comment-` + itoa64(r.ID) + `"><div id="comment-` + itoa64(r.ID) + `" class="comment_container">`)
			b.WriteString(s.avatarImg(ctx, r.AuthorEmail, 60))
			b.WriteString(`<div class="comment-text">`)
			if rating := gooReviewRating(r); rating > 0 && s.gooOptBool("enable_review_rating") {
				b.WriteString(gooRatingHTML(float64(rating), 0))
			}
			if r.Status == "pending" {
				b.WriteString(`<p class="meta"><em class="woocommerce-review__awaiting-approval">Your review is awaiting approval</em></p>`)
			} else {
				b.WriteString(`<p class="meta"><strong class="woocommerce-review__author">` + s.pllUC(c, gooEsc(r.AuthorName), 0) + ` </strong>`)
				if v, _ := r.Meta["verified"].(bool); v && s.gooOptBool("review_rating_verification_label") {
					b.WriteString(`<em class="woocommerce-review__verified verified">(verified owner)</em> `)
				}
				t := r.CreatedAt.In(s.loc())
				b.WriteString(`<span class="woocommerce-review__dash">&ndash;</span> <time class="woocommerce-review__published-date" datetime="` + t.Format(time.RFC3339) + `">` + wp.FormatDate(t, df) + `</time></p>`)
			}
			// Reviews are comments: $allowedtags (KsesData) before texturize/autop, or a stored
			// <script> / on* handler is emitted as HTML. RelUGC matches comment_text().
			b.WriteString(`<div class="description">` + s.gooKeep(c, wp.Autop(wp.Texturize(wp.RelUGC(wp.KsesData(r.Content))))) + `</div></div></div></li>`)
		}
		b.WriteString(`</ol>`)
	} else {
		b.WriteString(`<p class="woocommerce-noreviews">There are no reviews yet.</p>`)
	}
	b.WriteString(`</div>`)
	email := ""
	if u != nil {
		email = u.Email
	}
	if s.gooOptBool("review_rating_verification_required") && !s.gooIsVerifiedOwner(ctx, email, gooUserID(u), p.ID) {
		b.WriteString(`<p class="woocommerce-verification-required">Only logged in customers who have purchased this product may leave a review.</p>`)
	} else {
		b.WriteString(s.gooReviewForm(c, p, approved == 0))
	}
	b.WriteString(`<div class="clear"></div></div>`)
	return b.String()
}

func gooUserID(u *store.User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}

func gooReviewRating(r *store.Comment) int {
	switch v := r.Meta["rating"].(type) {
	case string:
		return gooAtoi(v)
	case float64:
		return int(v)
	}
	return 0
}

// gooReviewForm is the review form (comment_form() with WooCommerce's arguments).
func (s *Server) gooReviewForm(c *gin.Context, p *gooProduct, first bool) string {
	ctx := c.Request.Context()
	u := currentUser(c)
	title := "Add a review"
	if first {
		title = "Be the first to review &ldquo;" + gooEsc(p.Name) + "&rdquo;"
	}
	var b strings.Builder
	b.WriteString(`<div id="review_form_wrapper"><div id="review_form"><div id="respond" class="comment-respond"><span id="reply-title" class="comment-reply-title" role="heading" aria-level="3">` + title + `</span>`)
	if s.settings.Bool("comment_registration") && u == nil {
		b.WriteString(`<p class="must-log-in">You must be <a href="` + gooEsc(s.gooPageURL(ctx, "myaccount")) + `">logged in</a> to post a review.</p></div></div></div>`)
		return b.String()
	}
	b.WriteString(`<form action="` + gooEsc(s.gooPermalink(ctx, p)) + `#reviews" method="post" id="commentform" class="comment-form">`)
	req := s.settings.Bool("require_name_email")
	if u == nil {
		b.WriteString(`<p class="comment-notes"><span id="email-notes">Your email address will not be published.</span> <span class="required-field-message">Required fields are marked <span class="required">*</span></span></p>`)
	} else {
		b.WriteString(`<p class="logged-in-as">Logged in as ` + gooEsc(u.DisplayName) + `.</p>`)
	}
	if s.gooOptBool("enable_review_rating") {
		reqMark, reqAttr := "", ""
		if s.gooOptBool("review_rating_required") {
			reqMark, reqAttr = `&nbsp;<span class="required">*</span>`, " required"
		}
		b.WriteString(`<div class="comment-form-rating"><label for="rating" id="comment-form-rating-label">Your rating` + reqMark + `</label>` +
			`<select name="rating" id="rating"` + reqAttr + `><option value="">Rate&hellip;</option><option value="5">Perfect</option><option value="4">Good</option><option value="3">Average</option><option value="2">Not that bad</option><option value="1">Very poor</option></select></div>`)
	}
	b.WriteString(`<p class="comment-form-comment"><label for="comment">Your review&nbsp;<span class="required">*</span></label><textarea id="comment" name="comment" cols="45" rows="8" required></textarea></p>`)
	if u == nil {
		mark, attr := "", ""
		if req {
			mark, attr = `&nbsp;<span class="required">*</span>`, " required"
		}
		b.WriteString(`<p class="comment-form-author"><label for="author">Name` + mark + `</label><input id="author" name="author" type="text" autocomplete="name" value="" size="30"` + attr + ` /></p>`)
		b.WriteString(`<p class="comment-form-email"><label for="email">Email` + mark + `</label><input id="email" name="email" type="email" autocomplete="email" value="" size="30"` + attr + ` /></p>`)
		b.WriteString(`<p class="comment-form-cookies-consent"><input id="wp-comment-cookies-consent" name="wp-comment-cookies-consent" type="checkbox" value="yes" /> <label for="wp-comment-cookies-consent">Save my name, email, and website in this browser for the next time I comment.</label></p>`)
	}
	b.WriteString(`<p class="form-submit"><input name="submit" type="submit" id="submit" class="submit" value="Submit" /><input type="hidden" name="comment_post_ID" value="` + itoa64(p.ID) + `" id="comment_post_ID" />` +
		`<input type="hidden" name="goo_review" value="1" /><input type="hidden" name="_wpnonce" value="` + s.nonce(c, "goo-review-"+itoa64(p.ID)) + `" /></p>`)
	b.WriteString(`</form></div></div></div>`)
	return b.String()
}

// gooSubmitReview handles the review form: validation, moderation, the rating meta and the average.
func (s *Server) gooSubmitReview(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	pid := gooAtoi64(c.PostForm("comment_post_ID"))
	p, err := s.gooGetProduct(ctx, pid)
	back := s.links(ctx).HomeURL("/")
	if err == nil {
		back = s.gooPermalink(ctx, p)
	}
	fail := func(msg string) {
		s.gooAddNotice(c, "error", msg)
		s.gooRedirect(c, back+"#reviews")
	}
	if err != nil || p.Status != "publish" || !s.gooReviewsOpen(p) {
		fail("Sorry, reviews are closed for this product.")
		return
	}
	if !s.checkNonceValue(c, c.PostForm("_wpnonce"), "goo-review-"+itoa64(p.ID)) {
		fail("Your session has expired. Please reload the page and try again.")
		return
	}
	if s.settings.Bool("comment_registration") && u == nil {
		fail("Sorry, you must be logged in to post a review.")
		return
	}
	cm := &store.Comment{PostID: p.ID, Content: strings.TrimSpace(c.PostForm("comment")), AuthorIP: clientIP(c), UserAgent: c.Request.UserAgent(), Type: "review", Meta: map[string]any{}}
	if u != nil {
		cm.UserID, cm.AuthorName, cm.AuthorEmail, cm.AuthorURL = u.ID, u.DisplayName, u.Email, commentAuthorURL(u.URL)
	} else {
		cm.AuthorName, cm.AuthorEmail = strings.TrimSpace(c.PostForm("author")), strings.TrimSpace(c.PostForm("email"))
		if s.settings.Bool("require_name_email") && (cm.AuthorName == "" || !strings.Contains(cm.AuthorEmail, "@")) {
			fail("<strong>Error:</strong> Please fill the required fields.")
			return
		}
	}
	verified := s.gooIsVerifiedOwner(ctx, cm.AuthorEmail, cm.UserID, p.ID)
	if s.gooOptBool("review_rating_verification_required") && !verified {
		fail("Only logged in customers who have purchased this product may leave a review.")
		return
	}
	rating := gooAtoi(c.PostForm("rating"))
	if s.gooOptBool("enable_review_rating") {
		if rating < 1 || rating > 5 {
			if s.gooOptBool("review_rating_required") {
				fail("Please select a rating.")
				return
			}
			rating = 0
		}
	} else {
		rating = 0
	}
	if cm.Content == "" {
		fail("<strong>Error:</strong> Please type your review text.")
		return
	}
	if !u.Can("unfiltered_html", nil) {
		cm.Content = wp.KsesData(cm.Content)
	}
	if dup, _ := s.store.IsDuplicateComment(ctx, cm); dup {
		fail("Duplicate review detected; it looks as though you&#8217;ve already said that!")
		return
	}
	if !u.Can("moderate_comments", nil) {
		if last, ok, _ := s.store.LastCommentAt(ctx, cm.AuthorIP, cm.AuthorEmail, cm.UserID); ok && time.Since(last) < commentFloodInterval {
			fail("You are posting reviews too quickly. Slow down.")
			return
		}
	}
	if rating > 0 {
		cm.Meta["rating"] = strconv.Itoa(rating)
	}
	cm.Meta["verified"] = verified
	cm.Status = s.commentStatus(c, cm, u)
	if err := s.store.InsertComment(ctx, cm); err != nil {
		fail("Could not save the review.")
		return
	}
	s.gooUpdateRating(ctx, p.ID)
	s.gooPurgeProduct(ctx, p)
	s.notifyComment(c, cm)
	if u == nil {
		maxAge := -1
		if c.PostForm("wp-comment-cookies-consent") == "yes" {
			maxAge = 30000000
		}
		http.SetCookie(c.Writer, &http.Cookie{Name: "comment_author_email", Value: url.QueryEscape(cm.AuthorEmail), Path: "/", MaxAge: maxAge, SameSite: http.SameSiteLaxMode})
		if maxAge < 0 {
			http.SetCookie(c.Writer, &http.Cookie{Name: "comment_author_email", Value: url.QueryEscape(cm.AuthorEmail), Path: "/", SameSite: http.SameSiteLaxMode})
		}
		if cm.Status == "pending" {
			s.rememberPendingComment(c, cm.ID, maxAge)
		}
	}
	if cm.Status == "pending" {
		s.gooAddNotice(c, "notice", "Your review is awaiting approval.")
	}
	s.gooRedirect(c, back+"#comment-"+itoa64(cm.ID))
}

// Structured data -------------------------------------------------------------------------------------------

// gooProductJSONLD is WC_Structured_Data's product markup.
func (s *Server) gooProductJSONLD(c *gin.Context, p *gooProduct) string {
	raw, _ := json.Marshal(s.gooProductLD(c, p))
	return `<script type="application/ld+json">` + gooScriptSafe(raw) + `</script>` + "\n"
}

// gooProductLD is the Product node (with @context) built from the store's own price, stock and reviews;
// Pursae SEO folds it into its @graph.
func (s *Server) gooProductLD(c *gin.Context, p *gooProduct) map[string]any {
	ctx := c.Request.Context()
	lb := s.links(ctx)
	link := s.gooPermalink(ctx, p)
	desc := wp.StripTags(p.Post.Excerpt)
	if strings.TrimSpace(desc) == "" {
		desc = wp.StripTags(stripBlocks(p.Post.Content))
	}
	if s.passwordRequired(c, p.Post) {
		desc = "" // a protected product's text stays behind its password
	}
	desc = strings.TrimSpace(html.UnescapeString(desc))
	d := map[string]any{"@context": "https://schema.org/", "@type": "Product", "@id": link + "#product", "name": p.Name, "url": link, "description": desc}
	if img, _, _ := s.gooImageURL(ctx, p.ImageID, "full"); img != "" {
		d["image"] = img
	}
	if p.SKU != "" {
		d["sku"] = p.SKU
	} else {
		d["sku"] = itoa64(p.ID)
	}
	avail := "http://schema.org/InStock"
	if !s.gooIsInStock(ctx, p) {
		avail = "http://schema.org/OutOfStock"
	} else if p.StockStatus == "onbackorder" {
		avail = "http://schema.org/BackOrder"
	}
	seller := map[string]any{"@type": "Organization", "name": s.settings.String("blogname"), "url": lb.HomeURL("/")}
	dp := s.gooDecimals()
	cur := s.gooCurrency()
	if gooSubsVariableLike(p.Type) || p.Type == gooTypeGrouped {
		var lo, hi *float64
		var n int
		var kids []*gooProduct
		if gooSubsVariableLike(p.Type) {
			kids = s.gooVariationsCached(c, p)
		} else {
			m, _ := s.gooGetProducts(ctx, p.Children)
			for _, k := range m {
				kids = append(kids, k)
			}
		}
		for _, k := range kids {
			if pr := s.gooActive(c, k); pr != nil {
				v := s.gooToDisplay(c, k, *pr)
				n++
				if lo == nil || v < *lo {
					lo = gooFloatPtr(v)
				}
				if hi == nil || v > *hi {
					hi = gooFloatPtr(v)
				}
			}
		}
		if lo != nil {
			if *lo == *hi {
				d["offers"] = []any{map[string]any{"@type": "Offer", "price": gooFormatDecimal(*lo, dp), "priceCurrency": cur, "availability": avail, "url": link, "seller": seller,
					"priceValidUntil": time.Now().AddDate(1, 0, 0).Format("2006") + "-12-31"}}
			} else {
				d["offers"] = []any{map[string]any{"@type": "AggregateOffer", "lowPrice": gooFormatDecimal(*lo, dp), "highPrice": gooFormatDecimal(*hi, dp), "offerCount": n,
					"priceCurrency": cur, "availability": avail, "url": link, "seller": seller}}
			}
		}
	} else if pr := s.gooActive(c, p); pr != nil {
		offer := map[string]any{"@type": "Offer", "price": gooFormatDecimal(s.gooToDisplay(c, p, *pr), dp), "priceCurrency": cur, "availability": avail, "url": link, "seller": seller,
			"priceValidUntil": time.Now().AddDate(1, 0, 0).Format("2006") + "-12-31"}
		if p.IsOnSale() && p.SaleTo != nil {
			offer["priceValidUntil"] = p.SaleTo.Format("2006-01-02")
		}
		d["offers"] = []any{offer}
	}
	if p.RatingCount > 0 && s.gooOptBool("enable_review_rating") {
		d["aggregateRating"] = map[string]any{"@type": "AggregateRating", "ratingValue": gooFmtRating(p.AverageRating), "reviewCount": p.RatingCount}
		if all, err := s.store.ApprovedComments(ctx, p.ID, "desc", ""); err == nil {
			var reviews []any
			for _, r := range all {
				if len(reviews) >= 5 {
					break
				}
				rating := gooReviewRating(r)
				if r.Status != "approved" || rating == 0 {
					continue
				}
				reviews = append(reviews, map[string]any{"@type": "Review", "reviewRating": map[string]any{"@type": "Rating", "bestRating": "5", "ratingValue": strconv.Itoa(rating), "worstRating": "1"},
					"author": map[string]any{"@type": "Person", "name": r.AuthorName}, "reviewBody": wp.StripTags(r.Content), "datePublished": r.CreatedAt.Format(time.RFC3339)})
			}
			if len(reviews) > 0 {
				d["review"] = reviews
			}
		}
	}
	return d
}
