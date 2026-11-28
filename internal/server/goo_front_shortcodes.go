package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// The store's shortcodes (WC_Shortcodes): the page shortcodes, the product lists, the single
// product / add to cart ones and the widget shortcodes. Every goocommerce_* name also answers to
// its woocommerce_* alias, and WooCommerce's unprefixed product shortcodes keep their names.

// gooShortcodeNames are the canonical names; gooShortcodeAlias maps aliases to them.
var gooShortcodeNames = []string{
	"goocommerce_cart", "goocommerce_checkout", "goocommerce_my_account", "goocommerce_order_tracking", "goocommerce_mini_cart",
	"goocommerce_messages", "goocommerce_cart_link",
	"products", "product", "product_page", "product_category", "product_categories", "add_to_cart", "add_to_cart_url",
	"recent_products", "featured_products", "sale_products", "best_selling_products", "top_rated_products", "shop_messages",
	"product_attribute",
	"goocommerce_widget_price_filter", "goocommerce_widget_layered_nav", "goocommerce_widget_layered_nav_filters",
	"goocommerce_widget_rating_filter", "goocommerce_widget_product_categories", "goocommerce_widget_product_search",
	"goocommerce_widget_cart", "goocommerce_product_search",
}

func gooShortcodeAlias(tag string) string {
	if strings.HasPrefix(tag, "woocommerce_") {
		tag = "goocommerce_" + strings.TrimPrefix(tag, "woocommerce_")
	}
	if strings.HasPrefix(tag, "goocommerce_") {
		if rest := strings.TrimPrefix(tag, "goocommerce_"); containsStr([]string{"products", "product", "product_page", "product_category", "product_categories",
			"add_to_cart", "add_to_cart_url", "recent_products", "featured_products", "sale_products", "best_selling_products", "top_rated_products"}, rest) {
			return rest
		}
	}
	return tag
}

var gooShortcodeTagList = func() []string {
	out := []string{gooRenderTag}
	for _, n := range gooShortcodeNames {
		out = append(out, n)
		if strings.HasPrefix(n, "goocommerce_") {
			out = append(out, "woocommerce_"+strings.TrimPrefix(n, "goocommerce_"))
		} else if n != "shop_messages" && n != "product_attribute" {
			out = append(out, "goocommerce_"+n, "woocommerce_"+n)
		}
	}
	return out
}()

func gooShortcodeTags() []string { return gooShortcodeTagList }

// gooShortcodes expands the store's shortcodes in post content (and runs the modules' content filters).
func (s *Server) gooShortcodes(ctx context.Context, content string) string {
	if !s.gooOn() {
		return content
	}
	return s.gooExpandShortcodes(ctx, s.gooFilterContent(ctx, content))
}

// gooWidgetShortcodes expands the store's shortcodes in a block widget (a Shortcode, Paragraph or
// Custom HTML block holding [goocommerce_widget_cart], [goocommerce_widget_price_filter], …) — the
// widget_block_content filter WooCommerce relies on. The modules' post-content filters do not run.
func (s *Server) gooWidgetShortcodes(ctx context.Context, content string) string {
	if !s.gooOn() {
		return content
	}
	return s.gooExpandShortcodes(ctx, content)
}

func (s *Server) gooExpandShortcodes(ctx context.Context, content string) string {
	if !strings.Contains(content, "[") {
		return content
	}
	c := gooCtxGin(ctx)
	out := wp.DoShortcode(content, gooShortcodeTags(), func(tag string, attrs map[string]string, inner string) string {
		if tag == gooRenderTag {
			if v, ok := c.Get(gooCtxRender); ok {
				str, _ := v.(string)
				return str
			}
			return ""
		}
		return s.gooL10n(c, s.gooShortcode(c, gooShortcodeAlias(tag), attrs)) // the store's markup in the site language
	})
	if _, ok := c.Get(gooCtxSession); ok {
		s.gooSaveSession(c) // notices shown, cart recalculated: before the page is written
	}
	return out
}

// gooCtxGin is the request of a render context (a detached one for content rendered elsewhere).
func gooCtxGin(ctx context.Context) *gin.Context {
	if rc := renderFrom(ctx); rc != nil && rc.c != nil {
		return rc.c
	}
	if c, ok := ctx.(*gin.Context); ok {
		return c
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", io.NopCloser(strings.NewReader(""))).WithContext(ctx)
	return c
}

func gooFrontAttr(a map[string]string, k, def string) string {
	if v, ok := a[k]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func gooAttrBool(a map[string]string, k string) bool {
	switch strings.ToLower(gooFrontAttr(a, k, "")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// gooShortcode renders one shortcode.
func (s *Server) gooShortcode(c *gin.Context, tag string, a map[string]string) string {
	ctx := c.Request.Context()
	switch tag {
	case "goocommerce_cart":
		return s.gooCartPageHTML(c)
	case "goocommerce_checkout":
		return s.gooCheckoutPageHTML(c)
	case "goocommerce_my_account":
		return s.gooAccountPageHTML(c)
	case "goocommerce_order_tracking":
		return s.gooOrderTrackingHTML(c)
	case "goocommerce_mini_cart", "goocommerce_widget_cart":
		return gooWidgetWrap("widget_shopping_cart", gooFrontAttr(a, "title", ""), string(s.gooMiniCart(c)))
	case "goocommerce_cart_link":
		return string(s.gooCartLink(c))
	case "shop_messages", "goocommerce_messages":
		return `<div class="woocommerce">` + s.gooNoticesHTML(c) + `</div>`
	case "goocommerce_widget_price_filter":
		return s.gooWidgetPriceFilter(c, gooFrontAttr(a, "title", ""))
	case "goocommerce_widget_layered_nav":
		return s.gooWidgetLayeredNav(c, gooFrontAttr(a, "attribute", ""), gooFrontAttr(a, "title", ""), gooFrontAttr(a, "query_type", "and"))
	case "goocommerce_widget_layered_nav_filters":
		return s.gooWidgetActiveFilters(c, gooFrontAttr(a, "title", ""))
	case "goocommerce_widget_rating_filter":
		return s.gooWidgetRatingFilter(c, gooFrontAttr(a, "title", ""))
	case "goocommerce_widget_product_categories":
		return s.gooWidgetCategories(c, gooFrontAttr(a, "title", ""), gooFrontAttr(a, "count", "1") != "0", gooFrontAttr(a, "hide_empty", "0") == "1")
	case "goocommerce_widget_product_search", "goocommerce_product_search":
		return gooWidgetWrap("widget_product_search", gooFrontAttr(a, "title", ""), s.gooProductSearchForm(c))
	case "product_page":
		p := s.gooShortcodeProduct(ctx, a)
		if p == nil || p.Status != "publish" || s.gooHiddenProduct(ctx, p.ID) {
			return ""
		}
		if p.Post != nil && s.passwordRequired(c, p.Post) {
			// content-single-product.php: post_password_required() prints the password form instead
			// of the product until the visitor entered the password (wp-postpass cookie).
			return `<div class="single-product" data-product-page-preselected-id="0"><div class="woocommerce">` + s.passwordForm(p.Post, s.links(ctx)) + `</div></div>`
		}
		return `<div class="single-product" data-product-page-preselected-id="0"><div class="woocommerce">` + s.gooProductBody(c, p, true) + `</div></div>`
	case "add_to_cart":
		p := s.gooShortcodeProduct(ctx, a)
		if p == nil || !s.gooShortcodeProductPublic(ctx, p) || s.gooHiddenProduct(ctx, p.ID) {
			return ""
		}
		style := gooFrontAttr(a, "style", "border:4px solid #ccc; padding: 12px;")
		out := `<p class="product woocommerce add_to_cart_inline ` + gooEsc(gooFrontAttr(a, "class", "")) + `" style="` + gooEsc(style) + `">`
		if gooFrontAttr(a, "show_price", "true") != "false" {
			out += string(s.gooPriceHTML(c, p)) + " "
		}
		return out + s.gooLoopAddToCart(c, p, "") + `</p>`
	case "add_to_cart_url":
		p := s.gooShortcodeProduct(ctx, a)
		if p == nil {
			return ""
		}
		return gooEsc(s.links(ctx).HomeURL("/?add-to-cart=" + itoa64(p.ID)))
	case "product_categories":
		return s.gooCategoriesShortcode(c, a)
	}
	// Product lists ([products] and its presets).
	a2 := map[string]string{}
	for k, v := range a {
		a2[k] = v
	}
	switch tag {
	case "product":
		if a2["id"] == "" && a2["sku"] == "" {
			return ""
		}
		if a2["id"] != "" {
			a2["ids"] = a2["id"]
		}
		if a2["sku"] != "" {
			a2["skus"] = a2["sku"]
		}
		a2["columns"], a2["limit"] = "1", "1"
	case "product_category":
		if a2["category"] == "" {
			return ""
		}
		setDef(a2, "limit", "12")
		setDef(a2, "columns", "4")
		setDef(a2, "orderby", "menu_order title")
	case "recent_products":
		setDef(a2, "limit", "12")
		setDef(a2, "orderby", "date")
		setDef(a2, "order", "DESC")
	case "featured_products":
		setDef(a2, "limit", "12")
		a2["visibility"] = "featured"
	case "sale_products":
		setDef(a2, "limit", "12")
		a2["on_sale"] = "true"
	case "best_selling_products":
		setDef(a2, "limit", "12")
		a2["best_selling"] = "true"
	case "top_rated_products":
		setDef(a2, "limit", "12")
		a2["top_rated"] = "true"
	case "product_attribute":
		if a2["attribute"] != "" {
			a2["attribute"] = "pa_" + strings.TrimPrefix(a2["attribute"], "pa_")
		}
	case "products":
	default:
		return ""
	}
	return s.gooProductsShortcode(c, tag, a2)
}

func setDef(m map[string]string, k, v string) {
	if strings.TrimSpace(m[k]) == "" {
		m[k] = v
	}
}

// gooShortcodeProductPublic reports whether a product (or a variation's parent) is published, so a
// shortcode never shows the price or add-to-cart button of a draft, private or trashed product.
func (s *Server) gooShortcodeProductPublic(ctx context.Context, p *gooProduct) bool {
	if p.Status != "publish" {
		return false
	}
	if p.Type == gooTypeVariation {
		parent := s.gooParent(ctx, p)
		return parent != nil && parent.Status == "publish"
	}
	return true
}

// gooShortcodeProduct finds the product of an id / sku attribute.
func (s *Server) gooShortcodeProduct(ctx context.Context, a map[string]string) *gooProduct {
	if id := gooAtoi64(a["id"]); id > 0 {
		if p, err := s.gooGetProduct(ctx, id); err == nil {
			return p
		}
	}
	if sku := strings.TrimSpace(a["sku"]); sku != "" {
		var id int64
		if s.store.Pool.QueryRow(ctx, `SELECT post_id FROM goo_products WHERE sku = $1 LIMIT 1`, sku).Scan(&id) == nil {
			if p, err := s.gooGetProduct(ctx, id); err == nil {
				return p
			}
		}
	}
	return nil
}

// gooProductsShortcode is WC_Shortcode_Products.
func (s *Server) gooProductsShortcode(c *gin.Context, tag string, a map[string]string) string {
	ctx := c.Request.Context()
	cols := gooAtoi(gooFrontAttr(a, "columns", "4"))
	if cols < 1 || cols > 6 {
		cols = 4
	}
	limit := gooAtoi(gooFrontAttr(a, "limit", gooFrontAttr(a, "per_page", "-1")))
	if rows := gooAtoi(a["rows"]); rows > 0 {
		limit = rows * cols
	}
	if limit == 0 {
		limit = -1
	}
	q := gooProductQuery{Visibility: "catalog", PerPage: limit, Page: 1, ForVisitor: true}
	orderby := strings.ToLower(gooFrontAttr(a, "orderby", "title"))
	if f := strings.Fields(orderby); len(f) > 0 {
		orderby = f[0]
	}
	order := strings.ToUpper(gooFrontAttr(a, "order", "ASC"))
	switch orderby {
	case "menu_order", "popularity", "rating", "date", "price", "title", "id", "rand":
		q.OrderBy = orderby
	case "price-desc":
		q.OrderBy = "price-desc"
	case "post__in":
		q.OrderBy = "include"
	default:
		q.OrderBy = "title"
	}
	q.Order = order
	if orderby == "price" && order == "DESC" {
		q.OrderBy, q.Order = "price-desc", ""
	}
	switch gooFrontAttr(a, "visibility", "visible") {
	case "featured":
		q.Featured = true
	case "search":
		q.Visibility = "search"
	case "hidden":
		q.Visibility = ""
	}
	if gooAttrBool(a, "on_sale") {
		q.OnSale = true
	}
	if gooAttrBool(a, "best_selling") {
		q.OrderBy, q.Order = "popularity", ""
	}
	if gooAttrBool(a, "top_rated") {
		q.OrderBy, q.Order = "rating", ""
	}
	if ids := gooFrontAttr(a, "ids", ""); ids != "" {
		for _, v := range strings.Split(ids, ",") {
			if id := gooAtoi64(v); id > 0 {
				q.IDs = append(q.IDs, id)
			}
		}
		if len(q.IDs) == 0 {
			return ""
		}
		if gooFrontAttr(a, "orderby", "") == "" {
			q.OrderBy = "include"
		}
	}
	if skus := gooFrontAttr(a, "skus", ""); skus != "" {
		list := strings.Split(skus, ",")
		for i := range list {
			list[i] = strings.TrimSpace(list[i])
		}
		rows, err := s.store.Pool.Query(ctx, `SELECT post_id FROM goo_products WHERE sku = ANY($1)`, list)
		ids := []int64{-1}
		if err == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
		}
		q.IDs = append(q.IDs, ids...)
	}
	termIDs := func(tax, slugs string) []int64 {
		out := []int64{-1}
		for _, sl := range strings.Split(slugs, ",") {
			sl = strings.TrimSpace(sl)
			if sl == "" {
				continue
			}
			var t *store.Term
			var err error
			if id := gooAtoi64(sl); id > 0 {
				t, err = s.store.GetTerm(ctx, id)
			} else {
				t, err = s.store.TermBySlug(ctx, tax, sl)
			}
			if err == nil {
				if tax == "product_cat" {
					out = append(out, s.gooTermAndChildren(ctx, t.ID)...)
				} else {
					out = append(out, t.ID)
				}
			}
		}
		return out
	}
	if cat := gooFrontAttr(a, "category", ""); cat != "" {
		ids := termIDs("product_cat", cat)
		if strings.EqualFold(gooFrontAttr(a, "cat_operator", "IN"), "NOT IN") {
			ex, _, _ := s.gooQueryProducts(ctx, gooProductQuery{Category: ids, PerPage: -1})
			for _, p := range ex {
				q.Exclude = append(q.Exclude, p.ID)
			}
		} else {
			q.Category = ids
		}
	}
	if tg := gooFrontAttr(a, "tag", ""); tg != "" {
		q.Tag = termIDs("product_tag", tg)
	}
	if attr := gooFrontAttr(a, "attribute", ""); attr != "" {
		tax := "pa_" + strings.TrimPrefix(attr, "pa_")
		q.AttrTerms = map[string][]int64{tax: termIDs(tax, gooFrontAttr(a, "terms", ""))}
		if gooFrontAttr(a, "terms", "") == "" {
			terms, _ := s.gooAttributeTerms(ctx, tax)
			ids := []int64{-1}
			for _, t := range terms {
				ids = append(ids, t.ID)
			}
			q.AttrTerms[tax] = ids
		}
	}
	paginate := gooAttrBool(a, "paginate") && limit > 0
	page := 1
	if paginate {
		if n := gooAtoi(c.Query("product-page")); n > 1 {
			page = n
		}
		if ob := c.Query("orderby"); ob != "" {
			for _, o := range gooCatalogOrderings {
				if o.Value == ob {
					q.OrderBy, q.Order = ob, ""
				}
			}
		}
		q.Page = page
	}
	products, total, err := s.gooQueryProducts(ctx, q)
	if err != nil {
		return ""
	}
	class := strings.TrimSpace("woocommerce columns-" + strconv.Itoa(cols) + " " + gooFrontAttr(a, "class", ""))
	var b strings.Builder
	b.WriteString(`<div class="` + gooEsc(class) + `">`)
	if len(products) == 0 {
		b.WriteString(`</div>`)
		if tag == "products" {
			return `<div class="` + gooEsc(class) + `"><p class="woocommerce-info">No products were found matching your selection.</p></div>`
		}
		return ""
	}
	if paginate {
		b.WriteString(s.gooResultCount(page, limit, total))
		b.WriteString(s.gooOrderingForm(c, &gooFrontView{Kind: "shop"}))
	}
	b.WriteString(s.gooLoopHTML(c, products, cols))
	if paginate {
		b.WriteString(s.gooPagination(c, "", page, (total+limit-1)/limit, "product-page"))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// gooCategoriesShortcode is [product_categories].
func (s *Server) gooCategoriesShortcode(c *gin.Context, a map[string]string) string {
	ctx := c.Request.Context()
	hide := gooFrontAttr(a, "hide_empty", "1") == "1"
	all, _ := s.store.HierarchicalTerms(ctx, "product_cat", hide)
	cols := gooAtoi(gooFrontAttr(a, "columns", "4"))
	if cols < 1 {
		cols = 4
	}
	var ids []int64
	for _, v := range strings.Split(gooFrontAttr(a, "ids", ""), ",") {
		if id := gooAtoi64(v); id > 0 {
			ids = append(ids, id)
		}
	}
	parent := int64(-1)
	if v := gooFrontAttr(a, "parent", ""); v != "" {
		parent = gooAtoi64(v)
	}
	var list []*store.Term
	for _, t := range all {
		if len(ids) > 0 && !gooInt64In(t.ID, ids) {
			continue
		}
		if parent >= 0 && t.ParentID != parent {
			continue
		}
		if t.Slug == "uncategorized" && t.Count == 0 {
			continue
		}
		list = append(list, t)
	}
	if n := gooAtoi(gooFrontAttr(a, "number", "0")); n > 0 && len(list) > n {
		list = list[:n]
	}
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="woocommerce columns-` + strconv.Itoa(cols) + `"><ul class="products columns-` + strconv.Itoa(cols) + `">`)
	for i, t := range list {
		b.WriteString(s.gooCategoryItem(c, t, i, cols))
	}
	b.WriteString(`</ul></div>`)
	return b.String()
}

// jsonNewEncoder is an encoder that keeps <, > and & as they are.
func jsonNewEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}
