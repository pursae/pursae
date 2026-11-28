package server

import (
	"encoding/json"
	"html"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
	"wpgo/internal/wp"
)

// Pursae Commerce and Multi Language widgets of the Web Builder (contract §11.12): the shop.*,
// product.* and site.languages slots. Their settings are the DynSettings of the cell named by
// data-wb-item, read from the STORED doc (wbCellDefs), never from the snapshot; an id the doc does
// not have gets the widget defaults. Every value is generated here by the store's own markup
// functions (the same as the store pages and shortcodes), so the snapshot never carries store
// forms or data-* attributes the sanitizer would strip.
//
//   - product.*: the cell's product (DynSettings.Product, mapped to its translation in the request
//     language, published and visible to the visitor) or else the product being viewed.
//   - Cart, checkout and cart-page slots are per visitor: cart holders bypass Pursae Cache and
//     cart-fragments.js refreshes counts on cached pages; the cart and checkout page slots only
//     render on their own (no-store) store views.
//   - Assets: a fill that renders store markup sets ctxWBShop, so gooHead / gooFooter print the
//     store's styles and scripts (the canvas builds its head after the fill; Hello Pursae adds them
//     in wbRenderTheme).

// ctxWBShop marks a request whose Web Builder parts render store markup (gooIsStorePage).
const ctxWBShop = "wb.shop"

// wbCell is one item-bound widget cell of a stored doc.
type wbCell struct {
	widget string
	dyn    webbuilder.DynSettings
}

var wbCellCache struct {
	sync.Mutex
	m map[string]map[string]wbCell
}

// wbCellDefs reads the Commerce and language-switcher cells of a stored doc (cached per doc
// revision, like wbLoopDefs).
func wbCellDefs(d *store.WebBuilderDoc) map[string]wbCell {
	key := itoa64(d.PostID) + ":" + strconv.Itoa(d.Rev) + ":" + strconv.FormatInt(d.UpdatedAt.UnixNano(), 36)
	wbCellCache.Lock()
	if m, ok := wbCellCache.m[key]; ok {
		wbCellCache.Unlock()
		return m
	}
	wbCellCache.Unlock()
	out := map[string]wbCell{}
	var doc webbuilder.DocState
	if len(d.Doc) > 0 {
		if err := json.Unmarshal(d.Doc, &doc); err != nil {
			log.Printf("web builder: doc %d: %v", d.PostID, err)
		}
	}
	var walk func(s *webbuilder.Section, depth int)
	walk = func(s *webbuilder.Section, depth int) {
		if depth > webbuilder.MaxDepth+1 {
			return
		}
		for i := range s.Items {
			it := &s.Items[i]
			if it.ID != "" && webbuilder.IsItemWidget(it.Title) {
				if _, seen := out[it.ID]; !seen {
					c := wbCell{widget: it.Title}
					if it.Dyn != nil {
						c.dyn = *it.Dyn
					}
					out[it.ID] = c
				}
			}
			if it.Sub != nil {
				walk(it.Sub, depth+1)
			}
		}
	}
	for i := range doc.Sections {
		walk(&doc.Sections[i], 0)
	}
	wbCellCache.Lock()
	if wbCellCache.m == nil || len(wbCellCache.m) >= wbLoopSpecsMax {
		wbCellCache.m = map[string]map[string]wbCell{}
	}
	wbCellCache.m[key] = out
	wbCellCache.Unlock()
	return out
}

// wbItemSlot reports whether a slot belongs to the item-bound widgets of this file.
func wbItemSlot(slot webbuilder.SlotName) bool {
	n := string(slot)
	return strings.HasPrefix(n, "shop.") || strings.HasPrefix(n, "product.") || slot == webbuilder.SlotSiteLanguages
}

// wbLightShopSlots need no store stylesheet or script (a header cart icon on every page).
var wbLightShopSlots = map[webbuilder.SlotName]bool{
	webbuilder.SlotShopCartURL: true, webbuilder.SlotShopCheckoutURL: true,
	webbuilder.SlotShopCartCount: true, webbuilder.SlotShopCartSubtotal: true,
}

// wbProgramShop reports whether a compiled snapshot renders store markup that needs the store's
// assets.
func wbProgramShop(prog *webbuilder.Program) bool {
	for slot := range prog.Uses {
		n := string(slot)
		if (strings.HasPrefix(n, "shop.") || strings.HasPrefix(n, "product.")) && !wbLightShopSlots[slot] {
			return true
		}
	}
	return false
}

// wbShopEnv is the Commerce state of one fill.
type wbShopEnv struct {
	cells    map[string]wbCell
	products map[int64]*gooProduct // bound products by id (nil = not shown)
	catalog  bool                  // a catalog value rendered (cache tag goo:shop)
	langN    int
	notices  *string
}

func (e *wbSlotEnv) shopEnv() *wbShopEnv {
	if e.shop == nil {
		e.shop = &wbShopEnv{cells: wbCellDefs(e.doc), products: map[int64]*gooProduct{}}
	}
	return e.shop
}

// cell is the stored settings of the slot's cell (zero settings for an unknown id).
func (e *wbSlotEnv) cell(a webbuilder.SlotArgs) webbuilder.DynSettings {
	if a.Item == "" {
		return webbuilder.DynSettings{}
	}
	return e.shopEnv().cells[a.Item].dyn
}

func dynBool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func dynInt(p *float64, def, lo, hi int) int {
	if p == nil || *p < float64(lo) || *p > float64(hi) {
		return def
	}
	return int(*p)
}

// shopProduct is the product of a product.* slot: the cell's own product, else the product being
// viewed (nil when none or when the visitor may not see it).
func (e *wbSlotEnv) shopProduct(a webbuilder.SlotArgs) *gooProduct {
	sh := e.shopEnv()
	d := e.cell(a)
	if d.Product != nil && *d.Product >= 1 && *d.Product < 9e15 {
		id := int64(*d.Product)
		if p, ok := sh.products[id]; ok {
			return p
		}
		sh.products[id] = e.boundProduct(id)
		return sh.products[id]
	}
	if fv := gooViewFrom(e.c); fv != nil && fv.Kind == webbuilder.StoreKindProduct {
		return fv.Product
	}
	return nil
}

// boundProduct loads a product a cell names: its translation in the request language (Pursae
// Multi Language), published, not a variation and not hidden from the visitor (Memberships).
func (e *wbSlotEnv) boundProduct(id int64) *gooProduct {
	s := e.s
	if s.golylangOn() {
		if l := s.pllCurrent(e.ctx); l != nil {
			if t := s.pllGet(e.ctx, pllPost, id, l.Slug); t > 0 {
				id = t
			}
		}
	}
	if slices.Contains(s.gooListingExclude(e.ctx, "product"), id) {
		return nil
	}
	list, _, err := s.gooQueryProducts(e.ctx, gooProductQuery{IDs: []int64{id}, PerPage: 1})
	if err != nil || len(list) == 0 || list[0].Type == gooTypeVariation {
		return nil
	}
	return list[0]
}

// shopText fills the text slots.
func (e *wbSlotEnv) shopText(slot webbuilder.SlotName, a webbuilder.SlotArgs) (string, bool) {
	s := e.s
	if !s.gooOn() {
		return "", false
	}
	switch slot {
	case webbuilder.SlotShopArchiveTitle:
		if fv := gooViewFrom(e.c); wbStoreArchive(fv) && e.v.Q != nil && e.v.Q.Post != nil {
			t := html.UnescapeString(e.v.Q.Post.Title)
			return t, t != ""
		}
		return "", false
	}
	p := e.shopProduct(a)
	if p == nil {
		return "", false
	}
	e.shopEnv().catalog = true
	switch slot {
	case webbuilder.SlotProductTitle:
		return p.Name, p.Name != ""
	case webbuilder.SlotProductOnSale:
		if s.gooOnSale(e.c, p) {
			return s.gooL10nText(e.ctx, "Sale!"), true
		}
	case webbuilder.SlotProductSKU:
		return p.SKU, p.SKU != ""
	}
	return "", false
}

// shopAttrs fills the attribute slots.
func (e *wbSlotEnv) shopAttrs(slot webbuilder.SlotName, a webbuilder.SlotArgs) (map[string]string, bool) {
	s := e.s
	if !s.gooOn() {
		return nil, false
	}
	switch slot {
	case webbuilder.SlotShopCartURL:
		return map[string]string{"href": s.gooPageURL(e.ctx, "cart")}, true
	case webbuilder.SlotShopCheckoutURL:
		return map[string]string{"href": s.gooPageURL(e.ctx, "checkout")}, true
	}
	p := e.shopProduct(a)
	if p == nil {
		return nil, false
	}
	e.shopEnv().catalog = true
	switch slot {
	case webbuilder.SlotProductURL:
		return map[string]string{"href": s.gooPermalink(e.ctx, p)}, true
	case webbuilder.SlotProductImage:
		if p.ImageID > 0 {
			if att, err := s.store.GetAttachment(e.ctx, p.ImageID); err == nil {
				size := a.Size
				if size == "" {
					size = "large"
				}
				m := s.wbImageAttrs(e.ctx, att, size, "")
				if m["alt"] == "" {
					m["alt"] = p.Name
				}
				return m, len(m) > 0
			}
		}
	}
	return nil, false
}

// wbStoreArchive: the shop or a product taxonomy archive.
func wbStoreArchive(fv *gooFrontView) bool {
	if fv == nil {
		return false
	}
	switch fv.Kind {
	case webbuilder.StoreKindShop, webbuilder.StoreKindProductCat, webbuilder.StoreKindProductTag, webbuilder.StoreKindProductAttr:
		return true
	}
	return false
}

// shopHTML fills the html slots.
func (e *wbSlotEnv) shopHTML(slot webbuilder.SlotName, a webbuilder.SlotArgs) (string, bool) {
	s := e.s
	c := e.c
	if slot == webbuilder.SlotSiteLanguages {
		out := e.languages(a)
		return out, out != ""
	}
	if !s.gooOn() {
		return "", false
	}
	fv := gooViewFrom(c)
	l10n := func(v string) (string, bool) {
		if strings.TrimSpace(v) == "" {
			return "", false
		}
		return s.gooL10n(c, v), true
	}
	switch slot {
	case webbuilder.SlotShopProducts:
		e.shopEnv().catalog = true
		return l10n(s.gooProductsShortcode(c, "products", e.productsArgs(a)))
	case webbuilder.SlotShopCategories:
		e.shopEnv().catalog = true
		d := e.cell(a)
		args := map[string]string{"columns": strconv.Itoa(dynInt(d.Columns, 4, 1, webbuilder.MaxShopColumns)), "hide_empty": "1"}
		if !dynBool(d.HideEmpty, true) {
			args["hide_empty"] = "0"
		}
		if d.Limit != nil {
			args["number"] = strconv.Itoa(dynInt(d.Limit, 0, 1, webbuilder.MaxShopLimit))
		}
		switch {
		case d.Parent == "0":
			args["parent"] = "0"
		case d.Parent != "":
			if id, err := strconv.ParseInt(d.Parent, 10, 64); err == nil && id > 0 {
				args["parent"] = itoa64(e.termIn(id))
			}
		}
		if ids := webbuilder.ParseShopIDs(d.Cats, webbuilder.MaxShopIDs); len(ids) > 0 {
			args["ids"] = e.termList(ids)
		}
		return l10n(s.gooCategoriesShortcode(c, args))
	case webbuilder.SlotShopArchive:
		if wbStoreArchive(fv) {
			if v, ok := c.Get(gooCtxRender); ok {
				out, _ := v.(string)          // already localized (gooRenderSynthetic)
				c.Set(gooCtxNoticesTaken, "") // the archive markup carries the notices it took
				e.shopEnv().catalog = true
				return out, strings.TrimSpace(out) != ""
			}
		}
		return "", false
	case webbuilder.SlotShopArchiveDesc:
		if !wbStoreArchive(fv) || fv.Paged > 1 {
			return "", false
		}
		if fv.Term != nil {
			return l10n(wp.Autop(wp.KsesPost(fv.Term.Description)))
		}
		if fv.Search == "" {
			if id := s.gooPageIDIn(e.ctx, "shop"); id > 0 {
				if pg, err := s.store.GetPost(e.ctx, id); err == nil && pg.Status == "publish" && pg.Password == "" {
					return l10n(s.gooRenderContent(e.ctx, c, pg.Content))
				}
			}
		}
		return "", false
	case webbuilder.SlotShopBreadcrumb:
		if fv != nil && (wbStoreArchive(fv) || fv.Kind == webbuilder.StoreKindProduct) {
			return l10n(s.gooBreadcrumb(c, fv))
		}
		return "", false
	case webbuilder.SlotShopNotices:
		sh := e.shopEnv()
		if sh.notices == nil { // once per fill (data-wb-if asks twice)
			out := s.wbShopNotices(c)
			sh.notices = &out
		}
		return *sh.notices, wbHasShopNotices(*sh.notices)
	case webbuilder.SlotShopCartCount:
		return `<span class="wb-cart-count-n" data-goo-cart-count="">` + strconv.Itoa(s.gooCartFor(c).Count()) + `</span>`, true
	case webbuilder.SlotShopCartSubtotal:
		return `<span class="wb-cart-subtotal-n" data-goo-cart-subtotal="">` + string(s.gooPrice(s.gooCartFor(c).SubtotalDisplay())) + `</span>`, true
	case webbuilder.SlotShopMiniCart:
		return string(s.gooMiniCart(c)), true
	case webbuilder.SlotShopCartPage:
		if fv != nil && fv.Kind == webbuilder.StoreKindCart {
			return l10n(s.gooCartPageHTML(c))
		}
		return "", false
	case webbuilder.SlotShopCheckoutPage:
		if fv != nil && fv.Kind == webbuilder.StoreKindCheckout {
			return l10n(s.gooCheckoutPageHTML(c))
		}
		return "", false
	}
	p := e.shopProduct(a)
	if p == nil {
		return "", false
	}
	e.shopEnv().catalog = true
	switch slot {
	case webbuilder.SlotProductGallery:
		return l10n(s.gooGalleryHTML(c, p))
	case webbuilder.SlotProductPrice:
		return l10n(string(s.gooPriceHTML(c, p)))
	case webbuilder.SlotProductRating:
		return l10n(s.gooRatingBlockHTML(p))
	case webbuilder.SlotProductShortDesc:
		if p.Post != nil && strings.TrimSpace(p.Post.Excerpt) != "" && !s.passwordRequired(c, p.Post) {
			return l10n(`<div class="woocommerce-product-details__short-description">` + s.gooRenderContent(e.ctx, c, p.Post.Excerpt) + `</div>`)
		}
	case webbuilder.SlotProductDescription:
		// A password-protected product's text stays behind its password (wp-postpass cookie), as on
		// the product page, the [product_page] shortcode and the Store API.
		if p.Post != nil && !s.passwordRequired(c, p.Post) {
			return l10n(s.gooRenderContent(e.ctx, c, p.Post.Content))
		}
	case webbuilder.SlotProductAddToCart:
		return l10n(s.gooAddToCartForm(c, p))
	case webbuilder.SlotProductAddToCartButton:
		return l10n(s.gooLoopAddToCart(c, p, ""))
	case webbuilder.SlotProductMeta:
		return l10n(s.gooProductMeta(c, p))
	case webbuilder.SlotProductStock:
		return l10n(s.gooFrontStockHTML(c, p))
	case webbuilder.SlotProductTabs:
		if p.Post != nil && s.passwordRequired(c, p.Post) {
			return "", false // the Description tab is the product's content
		}
		return l10n(s.gooTabsHTML(c, p))
	case webbuilder.SlotProductRelated:
		return l10n(s.gooRelatedHTML(c, p))
	case webbuilder.SlotProductUpsells:
		return l10n(s.gooUpsellsHTML(c, p))
	}
	return "", false
}

// termIn maps a product term id to its translation in the request language.
func (e *wbSlotEnv) termIn(id int64) int64 {
	if e.s.golylangOn() {
		if l := e.s.pllCurrent(e.ctx); l != nil {
			if t := e.s.pllGet(e.ctx, pllTerm, id, l.Slug); t > 0 {
				return t
			}
		}
	}
	return id
}

func (e *wbSlotEnv) termList(ids []int64) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, itoa64(e.termIn(id)))
	}
	return strings.Join(out, ",")
}

// productsArgs are the [products] shortcode attributes of a shop-products cell.
func (e *wbSlotEnv) productsArgs(a webbuilder.SlotArgs) map[string]string {
	d := e.cell(a)
	args := map[string]string{
		"columns": strconv.Itoa(dynInt(d.Columns, 4, 1, webbuilder.MaxShopColumns)),
		"limit":   strconv.Itoa(dynInt(d.Limit, 8, 1, webbuilder.MaxShopLimit)),
		"orderby": "date",
		"order":   "DESC",
	}
	if slices.Contains(webbuilder.ShopOrderBys, d.OrderBy) {
		args["orderby"] = d.OrderBy
	}
	if d.Order == "asc" {
		args["order"] = "ASC"
	}
	switch d.Source {
	case "featured":
		args["visibility"] = "featured"
	case "sale":
		args["on_sale"] = "true"
	case "best-selling":
		args["best_selling"] = "true"
	case "top-rated":
		args["top_rated"] = "true"
	case "ids":
		ids := webbuilder.ParseShopIDs(d.IDs, webbuilder.MaxShopIDs)
		list := make([]string, 0, len(ids))
		for _, id := range ids {
			if e.s.golylangOn() {
				if l := e.s.pllCurrent(e.ctx); l != nil {
					if t := e.s.pllGet(e.ctx, pllPost, id, l.Slug); t > 0 {
						id = t
					}
				}
			}
			list = append(list, itoa64(id))
		}
		if len(list) == 0 {
			list = []string{"-1"} // no product: the "no products" notice, never the whole catalog
		}
		args["ids"] = strings.Join(list, ",")
		if d.OrderBy == "" {
			args["orderby"] = "post__in"
		}
	}
	if ids := webbuilder.ParseShopIDs(d.Cats, webbuilder.MaxShopIDs); len(ids) > 0 {
		args["category"] = e.termList(ids)
	}
	if ids := webbuilder.ParseShopIDs(d.Tags, webbuilder.MaxShopIDs); len(ids) > 0 {
		args["tag"] = e.termList(ids)
	}
	if dynBool(d.Paginate, false) {
		args["paginate"] = "true"
	}
	return args
}

// languages is the Pursae Multi Language switcher (pll_the_languages over the request's view).
func (e *wbSlotEnv) languages(a webbuilder.SlotArgs) string {
	s := e.s
	if !s.golylangOn() || len(s.pllLanguages(e.ctx)) == 0 || e.v.Q == nil {
		return ""
	}
	d := e.cell(a)
	args := pllDefaultArgs()
	args.Dropdown = dynBool(d.Dropdown, false)
	args.ShowFlags = dynBool(d.Flags, false)
	args.ShowNames = dynBool(d.Names, true)
	args.HideCurrent = dynBool(d.HideCurrent, false)
	sh := e.shopEnv()
	sh.langN++
	id := "wb_lang_" + itoa64(e.doc.PostID) + "_" + strconv.Itoa(sh.langN)
	out := s.pllSwitcherHTML(e.c, e.v.Q, args, id)
	if strings.TrimSpace(out) == "" {
		return ""
	}
	if args.Dropdown {
		return `<div class="wb-lang-switcher">` + out + `</div>`
	}
	return `<ul class="wb-lang-switcher">` + out + `</ul>`
}

func wbHasShopNotices(markup string) bool {
	markup = strings.TrimSpace(markup)
	return markup != "" && markup != gooNoticesMarkup(nil, nil, nil)
}

// wbShopNotices is the shop-notices widget: the notices the store body of this request already
// took (a template replaced that body), else the pending ones; printed once.
func (s *Server) wbShopNotices(c *gin.Context) string {
	out := c.GetString(gooCtxNoticesTaken)
	if out == "" {
		out = s.gooNoticesHTML(c)
	}
	c.Set(gooCtxNoticesTaken, "")
	return out
}

// wbProductWrap wraps the main part of a single product view in the product's element, as the
// store's own page does (div#product-<id>.product: the gallery, tabs and variation scripts look
// for it), with the notices the store body took when the part shows none.
func (s *Server) wbProductWrap(c *gin.Context, p *gooProduct, body string) string {
	notices := c.GetString(gooCtxNoticesTaken)
	c.Set(gooCtxNoticesTaken, "")
	return `<div class="woocommerce wb-shop">` + notices + `<div id="product-` + itoa64(p.ID) + `" class="` + s.gooProductClasses(c, p, "") + `">` +
		body + `</div></div>`
}

// wbArchiveWrap prints the notices a store archive template did not show.
func (s *Server) wbArchiveNotices(c *gin.Context, body string) string {
	notices := c.GetString(gooCtxNoticesTaken)
	if notices == "" {
		return body
	}
	c.Set(gooCtxNoticesTaken, "")
	return `<div class="woocommerce wb-shop">` + notices + `</div>` + body
}
