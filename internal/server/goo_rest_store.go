package server

import (
	"context"
	"html/template"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// The Store API's catalog routes (wc/store/v1/products…): WooCommerce Blocks' product, category,
// tag, attribute and review schemas, with prices in minor units and the currency fields.

func (s *Server) registerGooStoreREST() {
	get := func(h restHandler) map[string]restHandler {
		return map[string]restHandler{http.MethodGet: s.gooStore(h, false)}
	}
	s.gooRoute(gooStoreNS, "/products", get(s.gooStoreProducts))
	s.gooRoute(gooStoreNS, "/products/collection-data", get(s.gooStoreCollectionData))
	s.gooRoute(gooStoreNS, "/products/categories", get(s.gooStoreTerms("product_cat")))
	s.gooRoute(gooStoreNS, "/products/categories/(?P<id>[\\d]+)", get(s.gooStoreTerm("product_cat")))
	s.gooRoute(gooStoreNS, "/products/tags", get(s.gooStoreTerms("product_tag")))
	s.gooRoute(gooStoreNS, "/products/attributes", get(s.gooStoreAttributes))
	s.gooRoute(gooStoreNS, "/products/attributes/(?P<id>[\\d]+)", get(s.gooStoreAttribute))
	s.gooRoute(gooStoreNS, "/products/attributes/(?P<id>[\\d]+)/terms", get(s.gooStoreAttributeTerms))
	s.gooRoute(gooStoreNS, "/products/reviews", get(s.gooStoreReviews))
	s.gooRoute(gooStoreNS, "/products/(?P<id>[\\d]+)", get(s.gooStoreProduct))
	s.gooRoute(gooStoreNS, "/products/(?P<slug>[\\w-]+)", get(s.gooStoreProductBySlug))
}

// Currency -----------------------------------------------------------------------------------------------

// gooCurrencyFields are the Store API's currency_* fields of a price object.
func (s *Server) gooCurrencyFields(currency string) map[string]any {
	if currency == "" {
		currency = s.gooCurrency()
	}
	sym := strings.ReplaceAll(gooCurrencySymbol(currency), "&nbsp;", " ")
	prefix, suffix := "", ""
	switch s.gooOptString("currency_pos") {
	case "right":
		suffix = sym
	case "left_space":
		prefix = sym + " "
	case "right_space":
		suffix = " " + sym
	default:
		prefix = sym
	}
	return map[string]any{"currency_code": currency, "currency_symbol": sym, "currency_minor_unit": s.gooDecimals(),
		"currency_decimal_separator": s.gooOptString("price_decimal_sep"), "currency_thousand_separator": s.gooOptString("price_thousand_sep"),
		"currency_prefix": prefix, "currency_suffix": suffix}
}

// gooWithCurrency merges the currency fields into a price object.
func (s *Server) gooWithCurrency(m map[string]any, currency string) map[string]any {
	for k, v := range s.gooCurrencyFields(currency) {
		m[k] = v
	}
	return m
}

// gooMinor is an amount in minor units ("1050" for 10.50).
func (s *Server) gooMinor(v float64) string { return s.gooMinorUnits(v) }

// gooFromMinor converts minor units back to an amount.
func (s *Server) gooFromMinor(v string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return f / math.Pow(10, float64(s.gooDecimals())), true
}

// Images -------------------------------------------------------------------------------------------------

// gooImageJSON is an image of the Store API (id, src, thumbnail, srcset, sizes, name, alt).
func (s *Server) gooImageJSON(ctx context.Context, id int64) map[string]any {
	if id == 0 {
		return nil
	}
	a, err := s.store.GetAttachment(ctx, id)
	if err != nil {
		return nil
	}
	return s.gooImageJSONOf(ctx, a)
}

// gooImageJSONOf is gooImageJSON for an attachment already loaded.
func (s *Server) gooImageJSONOf(ctx context.Context, a *store.Attachment) map[string]any {
	src := s.uploadURL(ctx, a.FilePath)
	thumb := src
	var srcset []string
	for _, v := range a.Variants {
		u := s.uploadURL(ctx, v.FilePath)
		if v.Name == "woocommerce_thumbnail" || v.Name == "medium" && thumb == src {
			thumb = u
		}
		if v.Width > 0 {
			srcset = append(srcset, u+" "+strconv.Itoa(v.Width)+"w")
		}
	}
	if a.Width > 0 {
		srcset = append(srcset, src+" "+strconv.Itoa(a.Width)+"w")
	}
	return map[string]any{"id": a.ID, "src": src, "thumbnail": thumb, "srcset": strings.Join(srcset, ", "),
		"sizes": "(max-width: 300px) 100vw, 300px", "name": a.Title, "alt": a.Alt}
}

// gooProductImageIDs are the product image followed by the gallery.
func gooProductImageIDs(p *gooProduct) []int64 {
	var ids []int64
	if p.ImageID != 0 {
		ids = append(ids, p.ImageID)
	}
	for _, g := range p.GalleryIDs {
		if g != p.ImageID {
			ids = append(ids, g)
		}
	}
	return ids
}

// Price HTML ---------------------------------------------------------------------------------------------

// gooRESTPriceHTML is get_price_html(): the sale markup, a variable product's range, the display suffix.
func (s *Server) gooRESTPriceHTML(ctx context.Context, p *gooProduct) string {
	loc := s.gooBaseLocation()
	disp := func(v float64) float64 { return s.gooPriceToDisplay(ctx, p, v, loc) }
	switch p.Type {
	case gooTypeVariable, gooSubsTypeVariable, gooTypeGrouped:
		if p.MinPrice == nil {
			return ""
		}
		lo, hi := disp(*p.MinPrice), disp(*p.MinPrice)
		if p.MaxPrice != nil {
			hi = disp(*p.MaxPrice)
		}
		if lo != hi {
			return string(s.gooPrice(lo)) + ` <span aria-hidden="true">&ndash;</span> ` + string(s.gooPrice(hi))
		}
		return string(s.gooPrice(lo))
	}
	if !p.HasPrice() {
		return ""
	}
	if p.IsOnSale() && p.RegularPrice != nil {
		return `<del aria-hidden="true">` + string(s.gooPrice(disp(*p.RegularPrice))) + `</del> <ins>` + string(s.gooPrice(disp(p.PriceValue()))) + `</ins>`
	}
	return string(s.gooPrice(disp(p.PriceValue())))
}

// Store products -----------------------------------------------------------------------------------------

// gooStoreTermsJSON lists a product's terms of a taxonomy as {id, name, slug, link}.
func (s *Server) gooStoreTermsJSON(ctx context.Context, terms []*store.Term) []any {
	out := []any{}
	for _, t := range terms {
		out = append(out, map[string]any{"id": t.ID, "name": t.Name, "slug": t.Slug, "link": s.gooRESTTermLink(ctx, t)})
	}
	return out
}

// gooRESTTermLink is get_term_link() of a product taxonomy term.
func (s *Server) gooRESTTermLink(ctx context.Context, t *store.Term) string {
	base := map[string]string{"product_cat": s.gooOptString("category_base"), "product_tag": s.gooOptString("tag_base")}[t.Taxonomy]
	if base == "" {
		if t.Taxonomy == "product_cat" {
			base = "product-category"
		} else if t.Taxonomy == "product_tag" {
			base = "product-tag"
		} else {
			return s.links(ctx).HomeURL("/?taxonomy=" + t.Taxonomy + "&term=" + t.Slug)
		}
	}
	return s.links(ctx).HomeURL("/" + strings.Trim(base, "/") + "/" + t.Slug + "/")
}

// gooStoreProductJSON is the Store API's ProductSchema.
func (s *Server) gooStoreProductJSON(ctx context.Context, p *gooProduct, terms map[int64][]*store.Term) map[string]any {
	loc := s.gooBaseLocation()
	disp := func(v float64) string { return s.gooMinor(s.gooPriceToDisplay(ctx, p, v, loc)) }
	prices := map[string]any{"price": "", "regular_price": "", "sale_price": "", "price_range": nil}
	switch p.Type {
	case gooTypeVariable, gooSubsTypeVariable, gooTypeGrouped:
		if p.MinPrice != nil {
			prices["price"], prices["regular_price"], prices["sale_price"] = disp(*p.MinPrice), disp(*p.MinPrice), disp(*p.MinPrice)
			hi := *p.MinPrice
			if p.MaxPrice != nil {
				hi = *p.MaxPrice
			}
			prices["price_range"] = map[string]any{"min_amount": disp(*p.MinPrice), "max_amount": disp(hi)}
		}
	default:
		if p.HasPrice() {
			prices["price"] = disp(p.PriceValue())
			prices["sale_price"] = disp(p.PriceValue())
		}
		if p.RegularPrice != nil {
			prices["regular_price"] = disp(*p.RegularPrice)
		}
	}
	s.gooWithCurrency(prices, "")
	images := []any{}
	for _, id := range gooProductImageIDs(p) {
		if im := s.gooImageJSON(ctx, id); im != nil {
			images = append(images, im)
		}
	}
	var cats, tags []*store.Term
	attrTerms := map[string][]*store.Term{}
	for _, t := range terms[p.ID] {
		switch {
		case t.Taxonomy == "product_cat":
			cats = append(cats, t)
		case t.Taxonomy == "product_tag":
			tags = append(tags, t)
		case strings.HasPrefix(t.Taxonomy, "pa_"):
			attrTerms[t.Taxonomy] = append(attrTerms[t.Taxonomy], t)
		}
	}
	attrs := []any{}
	for _, a := range p.Attributes {
		if !a.Visible && !a.Variation {
			continue
		}
		var list []any
		if a.Taxonomy != "" {
			for _, slug := range a.Options {
				for _, t := range attrTerms[a.Taxonomy] {
					if t.Slug == slug {
						list = append(list, map[string]any{"id": t.ID, "name": t.Name, "slug": t.Slug, "default": p.DefaultAttributes[a.Key()] == t.Slug})
					}
				}
			}
		} else {
			for i, o := range a.Options {
				list = append(list, map[string]any{"id": i, "name": o, "slug": o, "default": p.DefaultAttributes[a.Key()] == o})
			}
		}
		attrs = append(attrs, map[string]any{"id": a.ID, "name": s.gooAttrLabel(ctx, a), "taxonomy": nilIfEmpty(a.Taxonomy), "has_variations": a.Variation, "terms": nonNilSlice(list)})
	}
	variations := []any{}
	if gooSubsVariableLike(p.Type) {
		vars, _ := s.gooVariations(ctx, p.ID, false)
		type varAttr struct {
			a     gooProductAttr
			label string
		}
		var vattrs []varAttr // labels once per product, not per variation
		if len(vars) > 0 {
			for _, a := range p.Attributes {
				if a.Variation {
					vattrs = append(vattrs, varAttr{a, s.gooAttrLabel(ctx, a)})
				}
			}
		}
		for _, v := range vars {
			var va []any
			for _, x := range vattrs {
				va = append(va, map[string]any{"name": x.label, "value": nilIfEmpty(v.VariationAttrs[x.a.Key()])})
			}
			variations = append(variations, map[string]any{"id": v.ID, "attributes": nonNilSlice(va)})
		}
	}
	purchasable := s.gooIsPurchasable(ctx, p)
	inStock := s.gooIsInStock(ctx, p)
	var lowStock any
	if q := s.gooStockQty(ctx, p); q >= 0 && q <= s.gooOptInt("notify_low_stock_amount") && q > 0 {
		lowStock = q
	}
	maxQty := 9999
	if q := s.gooStockQty(ctx, p); q >= 0 && !s.gooBackordersAllowed(ctx, p) {
		maxQty = q
	}
	if p.SoldIndividually {
		maxQty = 1
	}
	text, desc := "Add to cart", "Add to cart: &ldquo;"+gooEsc(p.Name)+"&rdquo;"
	addURL := "?add-to-cart=" + itoa64(p.ID)
	switch {
	case gooSubsVariableLike(p.Type):
		text, addURL = "Select options", s.gooPermalink(ctx, p)
	case p.Type == gooTypeGrouped:
		text, addURL = "View products", s.gooPermalink(ctx, p)
	case p.Type == gooTypeExternal:
		text, addURL = defaultStr(p.ButtonText, "Buy product"), p.ProductURL
	case !purchasable || !inStock:
		text, addURL = "Read more", s.gooPermalink(ctx, p)
	}
	if def := gooProductTypes[p.Type]; def != nil && def.AddToCartText != nil {
		text = def.AddToCartText(s, p, false)
	}
	stockText, stockClass := "", "in-stock"
	if !inStock {
		stockText, stockClass = "Out of stock", "out-of-stock"
	} else if p.StockStatus == "onbackorder" {
		stockText, stockClass = "Available on backorder", "available-on-backorder"
	} else if q := s.gooStockQty(ctx, p); q > 0 {
		stockText = strconv.Itoa(q) + " in stock"
	}
	desc2 := ""
	if p.Post != nil {
		desc2 = p.Post.Content
	}
	short := ""
	if p.Post != nil {
		short = p.Post.Excerpt
	}
	if p.Type == gooTypeVariation {
		desc2 = p.Description
	}
	desc2, short = s.gmemStoreProductText(ctx, p, desc2, short) // Memberships "view" restriction
	if parent := p.Post; p.Type == gooTypeVariation && p.ParentID != 0 {
		if pp, err := s.gooGetProduct(ctx, p.ParentID); err == nil {
			parent = pp.Post
		}
		if s.gooPassLocked(ctx, parent) {
			desc2, short = "", ""
		}
	} else if s.gooPassLocked(ctx, parent) {
		desc2, short = "", "" // a protected product's text stays behind its password, as on its page
	}
	return map[string]any{
		"id": p.ID, "name": p.Name, "slug": p.Slug, "parent": p.ParentID, "type": p.Type, "variation": s.gooVariationSummary(ctx, p),
		"permalink": s.gooPermalink(ctx, p), "sku": p.SKU, "short_description": short, "description": desc2,
		"on_sale": p.IsOnSale(), "prices": prices, "price_html": s.gooRESTPriceHTML(ctx, p),
		"average_rating": strconv.FormatFloat(p.AverageRating, 'f', 2, 64), "review_count": p.RatingCount, "images": images,
		"categories": s.gooStoreTermsJSON(ctx, cats), "tags": s.gooStoreTermsJSON(ctx, tags), "brands": []any{}, "attributes": attrs,
		"variations": variations, "grouped_products": nonNilSlice(p.Children), "has_options": gooSubsVariableLike(p.Type),
		"is_purchasable": purchasable, "is_in_stock": inStock, "is_on_backorder": p.StockStatus == "onbackorder",
		"low_stock_remaining": lowStock, "stock_availability": map[string]any{"text": stockText, "class": stockClass},
		"sold_individually": p.SoldIndividually,
		"add_to_cart": map[string]any{"text": text, "description": desc, "url": addURL, "single_text": "Add to cart",
			"minimum": 1, "maximum": maxQty, "multiple_of": 1},
		"extensions": s.paoStoreExtensions(ctx, p),
	}
}

func nilIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// gooStoreQuery reads the Store API's product filters into a catalog query.
func (s *Server) gooStoreQuery(c *gin.Context) gooProductQuery {
	ctx := c.Request.Context()
	page, per, _ := gooPage(c)
	q := gooProductQuery{Page: page, PerPage: per, Visibility: "catalog", Search: strings.TrimSpace(c.Query("search"))}
	if q.Search != "" {
		q.Visibility = "search"
	}
	q.IDs = gooQueryIDs(c, "include")
	q.Exclude = gooQueryIDs(c, "exclude")
	if t := c.Query("type"); t != "" {
		q.Types = strings.Split(t, ",")
	}
	if skus := c.Query("sku"); skus != "" {
		q.SKU = strings.Split(skus, ",")[0]
	}
	for _, key := range []string{"category", "tag"} {
		tax := map[string]string{"category": "product_cat", "tag": "product_tag"}[key]
		var ids []int64
		for _, v := range gooQueryList(c, key) {
			if id := gooInt(v); id > 0 {
				ids = append(ids, s.gooTermAndChildren(ctx, id)...)
			} else if t, err := s.store.TermBySlug(ctx, tax, v); err == nil {
				ids = append(ids, s.gooTermAndChildren(ctx, t.ID)...)
			}
		}
		if len(gooQueryList(c, key)) > 0 && len(ids) == 0 {
			ids = []int64{-1}
		}
		if key == "category" {
			q.Category = ids
		} else {
			q.Tag = ids
		}
	}
	if v, ok := gooQueryBool(c, "on_sale"); ok && v {
		q.OnSale = true
	}
	if v, ok := gooQueryBool(c, "featured"); ok && v {
		q.Featured = true
	}
	if v := c.Query("min_price"); v != "" {
		if f, ok := s.gooFromMinor(v); ok {
			q.MinPrice = &f
		}
	}
	if v := c.Query("max_price"); v != "" {
		if f, ok := s.gooFromMinor(v); ok {
			q.MaxPrice = &f
		}
	}
	if st := gooQueryList(c, "stock_status"); len(st) == 1 {
		q.StockStatus = st[0]
	} else if len(st) > 1 && !containsStr(st, "outofstock") {
		q.InStock = true
	}
	if parent := gooQueryIDs(c, "parent"); len(parent) == 1 {
		q.ParentID = &parent[0]
	}
	// attributes[0][attribute]=pa_color&attributes[0][slug][]=red (or term_id); also attribute / attribute_term.
	q.AttrTerms = s.gooQueryAttrTerms(c)
	switch c.DefaultQuery("orderby", "date") {
	case "price":
		q.OrderBy = "price"
		if strings.EqualFold(c.Query("order"), "desc") {
			q.OrderBy = "price-desc"
		}
	case "popularity", "rating", "title", "id", "include", "menu_order":
		q.OrderBy = c.Query("orderby")
	default:
		q.OrderBy = "date"
	}
	q.Order = c.DefaultQuery("order", map[bool]string{true: "asc", false: "desc"}[q.OrderBy == "title" || q.OrderBy == "menu_order" || q.OrderBy == "price"])
	return q
}

var gooAttrParamRe = regexp.MustCompile(`^attributes\[(\d+)\]\[(attribute|term_id|slug|operator)\](\[\d*\])?$`)

// gooQueryAttrTerms reads the attribute filters as taxonomy → term ids.
func (s *Server) gooQueryAttrTerms(c *gin.Context) map[string][]int64 {
	ctx := c.Request.Context()
	type filt struct {
		tax   string
		ids   []int64
		slugs []string
	}
	byIdx := map[string]*filt{}
	for k, vals := range c.Request.URL.Query() {
		m := gooAttrParamRe.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		f := byIdx[m[1]]
		if f == nil {
			f = &filt{}
			byIdx[m[1]] = f
		}
		for _, v := range vals {
			for _, part := range strings.Split(v, ",") {
				switch m[2] {
				case "attribute":
					f.tax = part
				case "term_id":
					if id := gooInt(part); id > 0 {
						f.ids = append(f.ids, id)
					}
				case "slug":
					f.slugs = append(f.slugs, part)
				}
			}
		}
	}
	if a, t := c.Query("attribute"), c.Query("attribute_term"); a != "" && t != "" {
		byIdx["x"] = &filt{tax: a, ids: gooIDList(t)}
	}
	out := map[string][]int64{}
	for _, f := range byIdx {
		if f.tax == "" {
			continue
		}
		ids := f.ids
		for _, slug := range f.slugs {
			if t, err := s.store.TermBySlug(ctx, f.tax, slug); err == nil {
				ids = append(ids, t.ID)
			}
		}
		if len(ids) == 0 {
			ids = []int64{-1}
		}
		out[f.tax] = append(out[f.tax], ids...)
	}
	return out
}

// gooStoreRespond writes a Store API response with the session saved and the Store API headers.
func (s *Server) gooStoreRespond(c *gin.Context, status int, v any) {
	s.gooSaveSession(c)
	s.gooStoreHeaders(c)
	restJSON(c, status, v)
}

func (s *Server) gooStoreProducts(c *gin.Context, req *restRequest) {
	ctx := withPluginUser(c.Request.Context(), currentUser(c)) // the viewer, for Memberships
	q := s.gooStoreQuery(c)
	if skus := c.Query("sku"); strings.Contains(skus, ",") {
		q.SKU = ""
		q.IDs = append(q.IDs, s.gooIDsBySKU(ctx, strings.Split(skus, ","))...)
	}
	list, total, err := s.gooQueryProducts(ctx, q)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	paginate(c, total, q.PerPage, q.Page)
	terms := s.gooRESTProductTerms(ctx, list)
	out := make([]any, 0, len(list))
	for _, p := range list {
		out = append(out, s.gooStoreProductJSON(ctx, p, terms))
	}
	s.gooStoreRespond(c, http.StatusOK, out)
}

func (s *Server) gooIDsBySKU(ctx context.Context, skus []string) []int64 {
	var ids []int64
	rows, err := s.store.Pool.Query(ctx, `SELECT post_id FROM goo_products WHERE sku = ANY($1)`, skus)
	if err != nil {
		return []int64{-1}
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return []int64{-1}
	}
	return ids
}

// gooRESTProductTerms loads the terms of products (and of variations' parents).
func (s *Server) gooRESTProductTerms(ctx context.Context, list []*gooProduct) map[int64][]*store.Term {
	ids := make([]int64, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	terms, _ := s.store.TermsForPosts(ctx, ids, "")
	if terms == nil {
		terms = map[int64][]*store.Term{}
	}
	return terms
}

func (s *Server) gooStoreProduct(c *gin.Context, req *restRequest) {
	ctx := withPluginUser(c.Request.Context(), currentUser(c)) // the viewer, for Memberships
	p, err := s.gooGetProduct(ctx, req.ID())
	if err == nil && s.gooHiddenProduct(c, p.ID) { // Memberships "hide completely"
		err = errNotFound
	}
	if err != nil || (p.Status != "publish" && !(p.Type == gooTypeVariation && p.Status == "private")) {
		restErr(c, http.StatusNotFound, "woocommerce_rest_product_invalid_id", "Invalid product ID.")
		return
	}
	s.gooStoreRespond(c, http.StatusOK, s.gooStoreProductJSON(ctx, p, s.gooRESTProductTerms(ctx, []*gooProduct{p})))
}

func (s *Server) gooStoreProductBySlug(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	var id int64
	_ = s.store.Pool.QueryRow(ctx, `SELECT id FROM posts WHERE type = 'product' AND slug = $1 AND status = 'publish'`, req.Params["slug"]).Scan(&id)
	req.Params["id"] = itoa64(id)
	s.gooStoreProduct(c, req)
}

// gooStoreTermJSON is the Store API's term schema (categories have image, review count and permalink).
func (s *Server) gooStoreTermJSON(ctx context.Context, t *store.Term) map[string]any {
	out := map[string]any{"id": t.ID, "name": t.Name, "slug": t.Slug, "description": t.Description, "parent": t.ParentID, "count": t.Count}
	if t.Taxonomy == "product_cat" {
		out["image"] = s.gooImageJSON(ctx, gooInt(t.Meta["thumbnail_id"]))
		var reviews int
		_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM comments cm JOIN post_terms pt ON pt.post_id = cm.post_id WHERE pt.term_id = $1
			AND cm.status = 'approved' AND cm.type IN ('review','comment') AND cm.meta ? 'rating'`, t.ID).Scan(&reviews)
		out["review_count"] = reviews
		out["permalink"] = s.gooRESTTermLink(ctx, t)
	}
	return out
}

func (s *Server) gooStoreTerms(tax string) restHandler {
	return func(c *gin.Context, req *restRequest) {
		ctx := c.Request.Context()
		hide, _ := gooQueryBool(c, "hide_empty")
		terms, _, err := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: tax, HideEmpty: hide, OrderBy: c.DefaultQuery("orderby", "name"), Order: c.DefaultQuery("order", "asc")})
		if err != nil {
			restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
			return
		}
		out := make([]any, 0, len(terms))
		for _, t := range terms {
			out = append(out, s.gooStoreTermJSON(ctx, t))
		}
		s.gooStoreRespond(c, http.StatusOK, out)
	}
}

func (s *Server) gooStoreTerm(tax string) restHandler {
	return func(c *gin.Context, req *restRequest) {
		ctx := c.Request.Context()
		t, err := s.store.GetTerm(ctx, req.ID())
		if err != nil || t.Taxonomy != tax {
			restErr(c, http.StatusNotFound, "woocommerce_rest_term_invalid", "Term does not exist.")
			return
		}
		s.gooStoreRespond(c, http.StatusOK, s.gooStoreTermJSON(ctx, t))
	}
}

func (s *Server) gooStoreAttributeJSON(ctx context.Context, a *gooAttribute) map[string]any {
	var count int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM terms WHERE taxonomy = $1`, a.Taxonomy()).Scan(&count)
	return map[string]any{"id": a.ID, "name": a.Name, "taxonomy": a.Taxonomy(), "type": a.Type, "order": a.OrderBy, "has_archives": a.HasArchives, "count": count}
}

func (s *Server) gooStoreAttributes(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	list, _ := s.gooAttributes(ctx)
	out := []any{}
	for _, a := range list {
		out = append(out, s.gooStoreAttributeJSON(ctx, a))
	}
	s.gooStoreRespond(c, http.StatusOK, out)
}

func (s *Server) gooAttributeByID(ctx context.Context, id int64) *gooAttribute {
	list, _ := s.gooAttributes(ctx)
	for _, a := range list {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (s *Server) gooStoreAttribute(c *gin.Context, req *restRequest) {
	a := s.gooAttributeByID(c.Request.Context(), req.ID())
	if a == nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_attribute_invalid", "Attribute does not exist.")
		return
	}
	s.gooStoreRespond(c, http.StatusOK, s.gooStoreAttributeJSON(c.Request.Context(), a))
}

func (s *Server) gooStoreAttributeTerms(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	a := s.gooAttributeByID(ctx, req.ID())
	if a == nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_attribute_invalid", "Attribute does not exist.")
		return
	}
	terms, _ := s.gooAttributeTerms(ctx, a.Taxonomy())
	out := []any{}
	for _, t := range terms {
		out = append(out, s.gooStoreTermJSON(ctx, t))
	}
	s.gooStoreRespond(c, http.StatusOK, out)
}

// Reviews ---------------------------------------------------------------------------------------------

// gooReview is a product review (a comment of type review with a rating).
type gooReview struct {
	*store.Comment
	ProductName string
	ProductSlug string
}

// gooQueryReviews lists reviews: status ("" = approved), product / category filters.
func (s *Server) gooQueryReviews(ctx context.Context, statuses []string, productIDs, categoryIDs, include, exclude []int64, search, orderBy, order string, page, per int) ([]*gooReview, int, error) {
	conds := []string{"cm.type IN ('review','comment')", "p.type = 'product'"}
	args := []any{}
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if len(statuses) == 0 {
		statuses = []string{"approved"}
	}
	if !containsStr(statuses, "all") {
		conds = append(conds, "cm.status = ANY("+add(statuses)+")")
	} else {
		conds = append(conds, "cm.status IN ('approved','pending')")
	}
	if len(productIDs) > 0 {
		conds = append(conds, "cm.post_id = ANY("+add(productIDs)+")")
	}
	if len(categoryIDs) > 0 {
		conds = append(conds, "cm.post_id IN (SELECT post_id FROM post_terms WHERE term_id = ANY("+add(categoryIDs)+"))")
	}
	if len(include) > 0 {
		conds = append(conds, "cm.id = ANY("+add(include)+")")
	}
	if len(exclude) > 0 {
		conds = append(conds, "NOT cm.id = ANY("+add(exclude)+")")
	}
	if search != "" {
		ph := add("%" + search + "%")
		conds = append(conds, "(cm.content ILIKE "+ph+" OR cm.author_name ILIKE "+ph+")")
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM comments cm JOIN posts p ON p.id = cm.post_id WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	dir := "DESC"
	if strings.EqualFold(order, "asc") {
		dir = "ASC"
	}
	col := "cm.created_at"
	switch orderBy {
	case "id":
		col = "cm.id"
	case "rating":
		col = "coalesce((cm.meta->>'rating')::numeric, 0)"
	case "product":
		col = "cm.post_id"
	}
	if per <= 0 {
		per = 10
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT cm.id, p.title, p.slug FROM comments cm JOIN posts p ON p.id = cm.post_id WHERE `+where+
		` ORDER BY `+col+` `+dir+`, cm.id `+dir+` LIMIT `+strconv.Itoa(per)+` OFFSET `+strconv.Itoa((max(page, 1)-1)*per), args...)
	if err != nil {
		return nil, 0, err
	}
	type row struct {
		id         int64
		name, slug string
	}
	var found []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.name, &r.slug) == nil {
			found = append(found, r)
		}
	}
	rows.Close()
	var out []*gooReview
	for _, r := range found {
		if cm, err := s.store.GetComment(ctx, r.id); err == nil {
			out = append(out, &gooReview{Comment: cm, ProductName: r.name, ProductSlug: r.slug})
		}
	}
	return out, total, nil
}

func (s *Server) gooStoreReviews(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	page, per, _ := gooPage(c)
	var cats []int64
	for _, id := range gooQueryIDs(c, "category_id") {
		cats = append(cats, s.gooTermAndChildren(ctx, id)...)
	}
	list, total, err := s.gooQueryReviews(ctx, nil, gooQueryIDs(c, "product_id"), cats, nil, nil, "", c.DefaultQuery("orderby", "date_gmt"), c.DefaultQuery("order", "desc"), page, per)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	paginate(c, total, per, page)
	out := make([]any, 0, len(list))
	for _, r := range list {
		var img any
		if p, err := s.gooGetProduct(ctx, r.PostID); err == nil {
			img = s.gooImageJSON(ctx, p.ImageID)
		}
		out = append(out, map[string]any{"id": r.ID, "date_created": s.gooDate(&r.CreatedAt), "formatted_date_created": r.CreatedAt.In(s.loc()).Format("January 2, 2006"),
			"date_created_gmt": gooDateGMT(&r.CreatedAt), "product_id": r.PostID, "product_name": r.ProductName,
			"product_permalink": s.links(ctx).HomeURL("/" + s.gooProductBase() + "/" + r.ProductSlug + "/"), "product_image": img,
			"reviewer": r.AuthorName, "review": template.HTML(r.Content), "rating": gooInt(r.Meta["rating"]), "verified": gooBool(r.Meta["verified"]),
			"reviewer_avatar_urls": s.gooAvatarURLs(ctx, r.AuthorEmail)})
	}
	s.gooStoreRespond(c, http.StatusOK, out)
}

func (s *Server) gooAvatarURLs(ctx context.Context, email string) map[string]string {
	return map[string]string{"24": s.avatarFor(ctx, email, 24), "48": s.avatarFor(ctx, email, 48), "96": s.avatarFor(ctx, email, 96)}
}

// Collection data ------------------------------------------------------------------------------------------

// gooStoreCollectionData is /products/collection-data: the price range, attribute counts, rating
// counts and stock status counts of the products matching the filters.
func (s *Server) gooStoreCollectionData(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	q := s.gooStoreQuery(c)
	q.Page, q.PerPage = 1, -1
	priceQ := q
	priceQ.MinPrice, priceQ.MaxPrice = nil, nil
	all, _, err := s.gooQueryProducts(ctx, priceQ)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	out := map[string]any{"price_range": nil, "attribute_counts": nil, "rating_counts": nil, "stock_status_counts": nil, "taxonomy_counts": nil}
	if v, _ := gooQueryBool(c, "calculate_price_range"); v {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, p := range all {
			mn, mx := p.Price, p.Price
			if p.MinPrice != nil {
				mn = p.MinPrice
			}
			if p.MaxPrice != nil {
				mx = p.MaxPrice
			}
			if mn != nil && *mn < lo {
				lo = *mn
			}
			if mx != nil && *mx > hi {
				hi = *mx
			}
		}
		if math.IsInf(lo, 1) {
			lo, hi = 0, 0
		}
		out["price_range"] = s.gooWithCurrency(map[string]any{"min_price": s.gooMinor(lo), "max_price": s.gooMinor(hi)}, "")
	}
	// The other counts use the price filter too.
	matched, _, _ := s.gooQueryProducts(ctx, q)
	ids := make([]int64, 0, len(matched))
	for _, p := range matched {
		ids = append(ids, p.ID)
	}
	var taxes []string
	for k, vals := range c.Request.URL.Query() {
		if strings.HasPrefix(k, "calculate_attribute_counts[") && strings.HasSuffix(k, "[taxonomy]") {
			taxes = append(taxes, vals...)
		}
	}
	taxes = append(taxes, gooQueryList(c, "calculate_attribute_counts")...)
	if len(taxes) > 0 {
		counts := []any{}
		rows, err := s.store.Pool.Query(ctx, `SELECT t.id, count(DISTINCT pt.post_id) FROM terms t JOIN post_terms pt ON pt.term_id = t.id
			WHERE t.taxonomy = ANY($1) AND pt.post_id = ANY($2) GROUP BY t.id ORDER BY t.id`, taxes, ids)
		if err == nil {
			for rows.Next() {
				var id int64
				var n int
				if rows.Scan(&id, &n) == nil {
					counts = append(counts, map[string]any{"term": id, "count": n})
				}
			}
			rows.Close()
		}
		out["attribute_counts"] = counts
	}
	if v, _ := gooQueryBool(c, "calculate_rating_counts"); v {
		byRating := map[int]int{}
		for _, p := range matched {
			if r := int(math.Round(p.AverageRating)); r > 0 {
				byRating[r]++
			}
		}
		var keys []int
		for k := range byRating {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		counts := []any{}
		for _, k := range keys {
			counts = append(counts, map[string]any{"rating": k, "count": byRating[k]})
		}
		out["rating_counts"] = counts
	}
	if v, _ := gooQueryBool(c, "calculate_stock_status_counts"); v {
		by := map[string]int{}
		for _, p := range matched {
			by[p.StockStatus]++
		}
		counts := []any{}
		for _, st := range []string{"instock", "outofstock", "onbackorder"} {
			if by[st] > 0 {
				counts = append(counts, map[string]any{"status": st, "count": by[st]})
			}
		}
		out["stock_status_counts"] = counts
	}
	s.gooStoreRespond(c, http.StatusOK, out)
}
