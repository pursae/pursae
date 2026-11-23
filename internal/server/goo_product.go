package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Products (WC_Product and its subclasses). A product is a post of type "product" (a variation one of
// type "product_variation" whose parent is the variable product) plus its goo_products row.
// Name, slug, status, description (content), short description (excerpt), image (featured media),
// menu order, reviews allowed (comment status) and the dates are the post's.

const (
	gooTypeSimple    = "simple"
	gooTypeVariable  = "variable"
	gooTypeGrouped   = "grouped"
	gooTypeExternal  = "external"
	gooTypeVariation = "variation"
)

// gooDownload is a downloadable file of a product.
type gooDownload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	File string `json:"file"`
}

// gooProductAttr is a product attribute (WC_Product_Attribute). Taxonomy attributes have Taxonomy
// "pa_<slug>" and options that are term slugs; custom attributes have a Name and free-text options.
type gooProductAttr struct {
	ID        int64    `json:"id"` // the global attribute's id, 0 for a custom attribute
	Name      string   `json:"name"`
	Taxonomy  string   `json:"taxonomy,omitempty"`
	Options   []string `json:"options"`
	Position  int      `json:"position"`
	Visible   bool     `json:"visible"`
	Variation bool     `json:"variation"`
}

// Key is the attribute's key in variation data and cart items: the taxonomy, or the sanitized name.
func (a gooProductAttr) Key() string {
	if a.Taxonomy != "" {
		return a.Taxonomy
	}
	return gooSanitizeKey(a.Name)
}

// gooProduct is a product or a variation.
type gooProduct struct {
	ID       int64
	ParentID int64
	Post     *store.Post

	Type              string
	Name              string
	Slug              string
	Status            string
	SKU               string
	RegularPrice      *float64
	SalePrice         *float64
	SaleFrom, SaleTo  *time.Time
	Price             *float64 // the stored active price (_price)
	MinPrice          *float64
	MaxPrice          *float64
	TaxStatus         string
	TaxClass          string
	ManageStock       bool
	StockQuantity     *int
	StockStatus       string
	Backorders        string
	LowStockAmount    *int
	SoldIndividually  bool
	Weight            string
	Length            string
	Width             string
	Height            string
	ShippingClassID   int64
	Virtual           bool
	Downloadable      bool
	Downloads         []gooDownload
	DownloadLimit     int
	DownloadExpiry    int
	PurchaseNote      string
	UpsellIDs         []int64
	CrossSellIDs      []int64
	Children          []int64
	ProductURL        string
	ButtonText        string
	Attributes        []gooProductAttr
	DefaultAttributes map[string]string
	VariationAttrs    map[string]string
	GalleryIDs        []int64
	ImageID           int64
	Featured          bool
	CatalogVisibility string
	TotalSales        int
	AverageRating     float64
	RatingCount       int
	Description       string // variations only; products use the post content
	Meta              map[string]any

	parent *gooProduct // a variation's parent, loaded on demand
}

const gooProductCols = `p.post_id, p.type, p.sku, p.regular_price::float8, p.sale_price::float8, p.sale_from, p.sale_to, p.price::float8,
	p.min_price::float8, p.max_price::float8, p.tax_status, p.tax_class, p.manage_stock, p.stock_quantity, p.stock_status, p.backorders,
	p.low_stock_amount, p.sold_individually, p.weight, p.length, p.width, p.height, coalesce(p.shipping_class_id,0), p.virtual,
	p.downloadable, p.downloads, p.download_limit, p.download_expiry, p.purchase_note, p.upsell_ids, p.cross_sell_ids, p.children,
	p.product_url, p.button_text, p.attributes, p.default_attributes, p.variation_attrs, p.gallery_ids, p.image_id, p.featured,
	p.catalog_visibility, p.total_sales, p.average_rating::float8, p.rating_count, p.description, p.meta`

func gooScanProduct(row pgx.Row) (*gooProduct, error) {
	p := &gooProduct{}
	var downloads, attrs, defAttrs, varAttrs, meta []byte
	err := row.Scan(&p.ID, &p.Type, &p.SKU, &p.RegularPrice, &p.SalePrice, &p.SaleFrom, &p.SaleTo, &p.Price,
		&p.MinPrice, &p.MaxPrice, &p.TaxStatus, &p.TaxClass, &p.ManageStock, &p.StockQuantity, &p.StockStatus, &p.Backorders,
		&p.LowStockAmount, &p.SoldIndividually, &p.Weight, &p.Length, &p.Width, &p.Height, &p.ShippingClassID, &p.Virtual,
		&p.Downloadable, &downloads, &p.DownloadLimit, &p.DownloadExpiry, &p.PurchaseNote, &p.UpsellIDs, &p.CrossSellIDs, &p.Children,
		&p.ProductURL, &p.ButtonText, &attrs, &defAttrs, &varAttrs, &p.GalleryIDs, &p.ImageID, &p.Featured,
		&p.CatalogVisibility, &p.TotalSales, &p.AverageRating, &p.RatingCount, &p.Description, &meta)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(downloads, &p.Downloads)
	_ = json.Unmarshal(attrs, &p.Attributes)
	_ = json.Unmarshal(defAttrs, &p.DefaultAttributes)
	_ = json.Unmarshal(varAttrs, &p.VariationAttrs)
	_ = json.Unmarshal(meta, &p.Meta)
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	if p.DefaultAttributes == nil {
		p.DefaultAttributes = map[string]string{}
	}
	if p.VariationAttrs == nil {
		p.VariationAttrs = map[string]string{}
	}
	return p, nil
}

// gooNewProduct is a product with WooCommerce's defaults, not yet stored.
func gooNewProduct(typ string) *gooProduct {
	return &gooProduct{Type: typ, Status: "publish", TaxStatus: "taxable", StockStatus: "instock", Backorders: "no",
		DownloadLimit: -1, DownloadExpiry: -1, CatalogVisibility: "visible", Meta: map[string]any{},
		DefaultAttributes: map[string]string{}, VariationAttrs: map[string]string{}}
}

func (p *gooProduct) attachPost(post *store.Post) {
	p.Post = post
	p.ParentID = post.ParentID
	p.Name, p.Slug, p.Status = post.Title, post.Slug, post.Status
	if p.Type != gooTypeVariation {
		p.ImageID = post.FeaturedMediaID
	}
}

// gooGetProduct is wc_get_product().
func (s *Server) gooGetProduct(ctx context.Context, id int64) (*gooProduct, error) {
	post, err := s.store.GetPost(ctx, id)
	if err != nil {
		return nil, err
	}
	if post.Type != "product" && post.Type != "product_variation" {
		return nil, store.ErrNotFound
	}
	p, err := gooScanProduct(s.store.Pool.QueryRow(ctx, `SELECT `+gooProductCols+` FROM goo_products p WHERE p.post_id = $1`, id))
	if errors.Is(err, store.ErrNotFound) {
		// A product post without data (created by the core editor or an import): a simple product.
		p = gooNewProduct(gooTypeSimple)
		if post.Type == "product_variation" {
			p.Type = gooTypeVariation
		}
		p.ID = id
	} else if err != nil {
		return nil, err
	}
	p.attachPost(post)
	return p, nil
}

// gooGetProducts loads several products at once, keyed by id (missing ones are skipped).
func (s *Server) gooGetProducts(ctx context.Context, ids []int64) (map[int64]*gooProduct, error) {
	out := map[int64]*gooProduct{}
	if len(ids) == 0 {
		return out, nil
	}
	posts, _, err := s.store.QueryPosts(gooPllUnfiltered(ctx), store.PostQuery{Types: []string{"product", "product_variation"}, IDs: ids, Statuses: []string{"publish", "private", "draft", "pending", "future"}, PerPage: len(ids)})
	if err != nil {
		return nil, err
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT `+gooProductCols+` FROM goo_products p WHERE p.post_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	data := map[int64]*gooProduct{}
	for rows.Next() {
		p, err := gooScanProduct(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		data[p.ID] = p
	}
	rows.Close()
	for _, post := range posts {
		p := data[post.ID]
		if p == nil {
			p = gooNewProduct(gooTypeSimple)
			if post.Type == "product_variation" {
				p.Type = gooTypeVariation
			}
			p.ID = post.ID
		}
		p.attachPost(post)
		out[p.ID] = p
	}
	return out, nil
}

// gooSaveProduct writes the post and the product data, then syncs what depends on them (the active
// price, stock status, a variable parent's price range and stock status). New products get an id.
func (s *Server) gooSaveProduct(ctx context.Context, p *gooProduct, editorID int64) error {
	if p.Post == nil {
		p.Post = &store.Post{Type: "product", CommentStatus: "open", PingStatus: "closed", Meta: map[string]any{}}
		if p.Type == gooTypeVariation {
			p.Post.Type = "product_variation"
			p.Post.CommentStatus = "closed"
		}
	}
	post := p.Post
	post.Title, post.ParentID = p.Name, p.ParentID
	if p.Slug != "" {
		post.Slug = p.Slug
	}
	if p.Status != "" {
		post.Status = p.Status
	}
	if post.Status == "" {
		post.Status = "publish"
	}
	if p.Type != gooTypeVariation {
		post.FeaturedMediaID = p.ImageID
	}
	if post.AuthorID == 0 {
		post.AuthorID = editorID
	}
	var err error
	if post.ID == 0 {
		err = s.store.InsertPost(ctx, post)
	} else {
		err = s.store.UpdatePost(ctx, post, editorID)
	}
	if err != nil {
		return err
	}
	p.ID, p.Slug, p.Status = post.ID, post.Slug, post.Status
	s.gooNormalizeProduct(p)
	if err := s.gooWriteProductData(ctx, p); err != nil {
		return err
	}
	if gooSubsVariableLike(p.Type) {
		if err := s.gooSyncVariable(ctx, p); err != nil {
			return err
		}
	}
	if p.Type == gooTypeVariation && p.ParentID != 0 {
		if parent, err := s.gooGetProduct(ctx, p.ParentID); err == nil && gooSubsVariableLike(parent.Type) {
			if err := s.gooSyncVariable(ctx, parent); err != nil {
				return err
			}
		}
	}
	if p.Type == gooTypeGrouped {
		if err := s.gooSyncGrouped(ctx, p); err != nil {
			return err
		}
	}
	s.gooPurgeProduct(ctx, p)
	s.gooFire(ctx, "product_saved", p)
	return nil
}

// gooNormalizeProduct applies WooCommerce's validation before a save: the active price from the
// sale schedule, the stock status from the quantity, type-specific clean-ups.
func (s *Server) gooNormalizeProduct(p *gooProduct) {
	if p.TaxStatus == "" {
		p.TaxStatus = "taxable"
	}
	if p.Backorders == "" {
		p.Backorders = "no"
	}
	if p.CatalogVisibility == "" {
		p.CatalogVisibility = "visible"
	}
	if p.SalePrice != nil && p.RegularPrice != nil && *p.SalePrice >= *p.RegularPrice {
		p.SalePrice = nil // a sale price must be lower than the regular price
	}
	switch p.Type {
	case gooTypeExternal:
		p.ManageStock, p.StockQuantity, p.StockStatus, p.Virtual, p.Downloadable = false, nil, "instock", false, false
	case gooTypeGrouped:
		p.RegularPrice, p.SalePrice, p.ManageStock, p.StockQuantity = nil, nil, false, nil
	case gooTypeVariable, gooSubsTypeVariable:
		p.RegularPrice, p.SalePrice, p.SaleFrom, p.SaleTo = nil, nil, nil, nil
	}
	if !p.Downloadable {
		p.Downloads = nil
	}
	for i := range p.Downloads {
		if p.Downloads[i].ID == "" {
			p.Downloads[i].ID = gooRandomKey(16)
		}
	}
	if !gooSubsVariableLike(p.Type) && p.Type != gooTypeGrouped {
		p.Price = p.activePrice(time.Now())
	}
	if p.ManageStock && p.StockQuantity != nil && !gooSubsVariableLike(p.Type) && p.Type != gooTypeGrouped {
		p.StockStatus = s.gooStockStatusFor(*p.StockQuantity, p.Backorders)
	} else if p.ManageStock && p.StockQuantity == nil {
		zero := 0
		p.StockQuantity = &zero
		p.StockStatus = s.gooStockStatusFor(0, p.Backorders)
	}
	if !p.ManageStock && p.Type != gooTypeVariation {
		p.StockQuantity = nil
	}
	if p.StockStatus == "" {
		p.StockStatus = "instock"
	}
}

// gooStockStatusFor is the status a managed quantity implies (wc_update_product_stock_status logic).
func (s *Server) gooStockStatusFor(qty int, backorders string) string {
	if qty > s.gooOptInt("notify_no_stock_amount") {
		return "instock"
	}
	if backorders != "no" {
		return "onbackorder"
	}
	return "outofstock"
}

func (s *Server) gooWriteProductData(ctx context.Context, p *gooProduct) error {
	downloads, _ := json.Marshal(nonNilSlice(p.Downloads))
	attrs, _ := json.Marshal(nonNilSlice(p.Attributes))
	defAttrs, _ := json.Marshal(p.DefaultAttributes)
	varAttrs, _ := json.Marshal(p.VariationAttrs)
	meta, _ := json.Marshal(p.Meta)
	var shipClass any
	if p.ShippingClassID > 0 {
		shipClass = p.ShippingClassID
	}
	ids := func(v []int64) []int64 {
		if v == nil {
			return []int64{}
		}
		return v
	}
	_, err := s.store.Pool.Exec(ctx, `INSERT INTO goo_products (post_id, type, sku, regular_price, sale_price, sale_from, sale_to, price,
		min_price, max_price, tax_status, tax_class, manage_stock, stock_quantity, stock_status, backorders, low_stock_amount,
		sold_individually, weight, length, width, height, shipping_class_id, virtual, downloadable, downloads, download_limit,
		download_expiry, purchase_note, upsell_ids, cross_sell_ids, children, product_url, button_text, attributes,
		default_attributes, variation_attrs, gallery_ids, image_id, featured, catalog_visibility, description, meta, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,
		$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43, now())
		ON CONFLICT (post_id) DO UPDATE SET type=$2, sku=$3, regular_price=$4, sale_price=$5, sale_from=$6, sale_to=$7, price=$8,
		min_price=$9, max_price=$10, tax_status=$11, tax_class=$12, manage_stock=$13, stock_quantity=$14, stock_status=$15,
		backorders=$16, low_stock_amount=$17, sold_individually=$18, weight=$19, length=$20, width=$21, height=$22,
		shipping_class_id=$23, virtual=$24, downloadable=$25, downloads=$26, download_limit=$27, download_expiry=$28,
		purchase_note=$29, upsell_ids=$30, cross_sell_ids=$31, children=$32, product_url=$33, button_text=$34, attributes=$35,
		default_attributes=$36, variation_attrs=$37, gallery_ids=$38, image_id=$39, featured=$40, catalog_visibility=$41,
		description=$42, meta=$43, updated_at=now()`,
		p.ID, p.Type, strings.TrimSpace(p.SKU), p.RegularPrice, p.SalePrice, p.SaleFrom, p.SaleTo, p.Price,
		p.MinPrice, p.MaxPrice, p.TaxStatus, p.TaxClass, p.ManageStock, p.StockQuantity, p.StockStatus, p.Backorders, p.LowStockAmount,
		p.SoldIndividually, p.Weight, p.Length, p.Width, p.Height, shipClass, p.Virtual, p.Downloadable, downloads, p.DownloadLimit,
		p.DownloadExpiry, p.PurchaseNote, ids(p.UpsellIDs), ids(p.CrossSellIDs), ids(p.Children), p.ProductURL, p.ButtonText, attrs,
		defAttrs, varAttrs, ids(p.GalleryIDs), p.ImageID, p.Featured, p.CatalogVisibility, p.Description, meta)
	if err != nil {
		return err
	}
	// Product attributes that are taxonomies are also the product's terms (for layered nav filters).
	if p.Type != gooTypeVariation {
		for _, a := range p.Attributes {
			if a.Taxonomy == "" {
				continue
			}
			var termIDs []int64
			for _, slug := range a.Options {
				if t, err := s.store.TermBySlug(ctx, a.Taxonomy, slug); err == nil {
					termIDs = append(termIDs, t.ID)
				}
			}
			_ = s.store.SetPostTerms(ctx, p.ID, a.Taxonomy, termIDs)
		}
	}
	return nil
}

func nonNilSlice[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// gooSyncVariable is WC_Product_Variable::sync(): the price range and stock status from the variations.
func (s *Server) gooSyncVariable(ctx context.Context, p *gooProduct) error {
	vars, err := s.gooVariations(ctx, p.ID, true)
	if err != nil {
		return err
	}
	var minP, maxP *float64
	status := "outofstock"
	ids := make([]int64, 0, len(vars))
	for _, v := range vars {
		ids = append(ids, v.ID)
		if v.Status != "publish" {
			continue
		}
		if pr := v.activePrice(time.Now()); pr != nil {
			if minP == nil || *pr < *minP {
				minP = gooFloatPtr(*pr)
			}
			if maxP == nil || *pr > *maxP {
				maxP = gooFloatPtr(*pr)
			}
		}
		st := v.StockStatus
		if !v.ManageStock && p.ManageStock && p.StockQuantity != nil { // stock managed at the parent level
			st = s.gooStockStatusFor(*p.StockQuantity, p.Backorders)
		}
		if st == "instock" {
			status = "instock"
		} else if st == "onbackorder" && status != "instock" {
			status = "onbackorder"
		}
	}
	p.MinPrice, p.MaxPrice, p.Price, p.StockStatus, p.Children = minP, maxP, minP, status, ids
	_, err = s.store.Pool.Exec(ctx, `UPDATE goo_products SET min_price=$2, max_price=$3, price=$4, stock_status=$5, children=$6 WHERE post_id=$1`,
		p.ID, minP, maxP, minP, status, ids)
	return err
}

// gooSyncGrouped stores a grouped product's price range from its children.
func (s *Server) gooSyncGrouped(ctx context.Context, p *gooProduct) error {
	kids, err := s.gooGetProducts(ctx, p.Children)
	if err != nil {
		return err
	}
	var minP, maxP *float64
	for _, k := range kids {
		if pr := k.activePrice(time.Now()); pr != nil && k.Status == "publish" {
			if minP == nil || *pr < *minP {
				minP = gooFloatPtr(*pr)
			}
			if maxP == nil || *pr > *maxP {
				maxP = gooFloatPtr(*pr)
			}
		}
	}
	p.MinPrice, p.MaxPrice, p.Price = minP, maxP, minP
	_, err = s.store.Pool.Exec(ctx, `UPDATE goo_products SET min_price=$2, max_price=$3, price=$4 WHERE post_id=$1`, p.ID, minP, maxP, minP)
	return err
}

func gooFloatPtr(v float64) *float64 { return &v }

func gooIntPtr(v int) *int { return &v }

// gooVariations are a variable product's variations in menu order; withDisabled includes private ones.
func (s *Server) gooVariations(ctx context.Context, parentID int64, withDisabled bool) ([]*gooProduct, error) {
	statuses := []string{"publish"}
	if withDisabled {
		statuses = append(statuses, "private")
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT id FROM posts WHERE type = 'product_variation' AND parent_id = $1 AND status = ANY($2)
		ORDER BY menu_order, id`, parentID, statuses)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	m, err := s.gooGetProducts(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*gooProduct, 0, len(ids))
	for _, id := range ids {
		if v := m[id]; v != nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// gooParent loads (once) a variation's parent product.
func (s *Server) gooParent(ctx context.Context, p *gooProduct) *gooProduct {
	if p.Type != gooTypeVariation || p.ParentID == 0 {
		return nil
	}
	if p.parent == nil {
		p.parent, _ = s.gooGetProduct(ctx, p.ParentID)
	}
	return p.parent
}

// gooMatchVariation is WC_Product_Data_Store_CPT::find_matching_product_variation(): the first
// enabled variation whose attributes match (a variation's "" matches any value).
func (s *Server) gooMatchVariation(ctx context.Context, parent *gooProduct, attrs map[string]string) (*gooProduct, error) {
	vars, err := s.gooVariations(ctx, parent.ID, false)
	if err != nil {
		return nil, err
	}
	for _, v := range vars {
		ok := true
		for _, a := range parent.Attributes {
			if !a.Variation {
				continue
			}
			want := v.VariationAttrs[a.Key()]
			got := attrs[a.Key()]
			if got == "" {
				got = attrs["attribute_"+a.Key()]
			}
			if want != "" && !strings.EqualFold(want, got) {
				ok = false
				break
			}
			if got == "" {
				ok = false
				break
			}
		}
		if ok {
			return v, nil
		}
	}
	return nil, store.ErrNotFound
}

// Product state ----------------------------------------------------------------------------------

// onSaleAt is is_on_sale(): a sale price lower than the regular price inside the schedule.
func (p *gooProduct) onSaleAt(now time.Time) bool {
	if p.SalePrice == nil {
		return false
	}
	if p.RegularPrice != nil && *p.SalePrice >= *p.RegularPrice {
		return false
	}
	if p.SaleFrom != nil && now.Before(*p.SaleFrom) {
		return false
	}
	if p.SaleTo != nil && now.After(*p.SaleTo) {
		return false
	}
	return true
}

// IsOnSale reports whether the product (any variation of a variable product) is on sale now.
func (p *gooProduct) IsOnSale() bool { return p.onSaleAt(time.Now()) }

// activePrice is the price at a time: the sale price during the sale, else the regular price.
func (p *gooProduct) activePrice(now time.Time) *float64 {
	if p.onSaleAt(now) {
		return gooFloatPtr(*p.SalePrice)
	}
	if p.RegularPrice != nil {
		return gooFloatPtr(*p.RegularPrice)
	}
	return nil
}

// PriceValue is get_price() as a number (0 for no price).
func (p *gooProduct) PriceValue() float64 {
	if gooSubsVariableLike(p.Type) || p.Type == gooTypeGrouped {
		if p.Price != nil {
			return *p.Price
		}
		return 0
	}
	if v := p.activePrice(time.Now()); v != nil {
		return *v
	}
	return 0
}

// HasPrice reports whether get_price() is not empty.
func (p *gooProduct) HasPrice() bool {
	if gooSubsVariableLike(p.Type) || p.Type == gooTypeGrouped {
		return p.Price != nil
	}
	return p.activePrice(time.Now()) != nil
}

// IsPurchasable is is_purchasable(): published (or a published parent), with a price, not external
// (external products are bought elsewhere) and not grouped (their children are bought).
func (s *Server) gooIsPurchasable(ctx context.Context, p *gooProduct) bool {
	if p.Type == gooTypeExternal || p.Type == gooTypeGrouped {
		return false
	}
	if p.Status != "publish" && p.Status != "private" {
		return false
	}
	if p.Type == gooTypeVariation {
		parent := s.gooParent(ctx, p)
		// WC_Product_Variation::is_purchasable(): the parent must be published (a private one is
		// limited to those who can read it by the cart's mayBuyPrivate).
		if parent == nil || (parent.Status != "publish" && parent.Status != "private") || p.Status != "publish" {
			return false
		}
		// A variation of a module's variable type (variable subscriptions) needs the module on.
		if def := gooProductTypes[parent.Type]; def != nil && def.Module != "" && !s.gooExtOn(def.Module) {
			return false
		}
	}
	if def := gooProductTypes[p.Type]; def != nil && def.Purchasable != nil {
		return def.Purchasable(s, ctx, p)
	}
	return p.HasPrice()
}

// managing is the product whose stock is tracked: the variation, or its parent when the variation
// does not manage stock but the parent does (WC_Product_Variation::get_stock_managed_by_id).
func (s *Server) gooStockOwner(ctx context.Context, p *gooProduct) *gooProduct {
	if p.Type == gooTypeVariation && !p.ManageStock {
		if parent := s.gooParent(ctx, p); parent != nil && parent.ManageStock {
			return parent
		}
	}
	return p
}

// gooStockWriteOwner is the row a stock change is written to: the stock owner, or with Multi Language
// the canonical translation of it (gooPllStockCanonical), so every language takes from one counter.
func (s *Server) gooStockWriteOwner(ctx context.Context, p *gooProduct) *gooProduct {
	return s.gooPllStockCanonical(ctx, s.gooStockOwner(ctx, p))
}

// gooManagingStock is managing_stock(): the product (or the parent of a variation) tracks quantities.
func (s *Server) gooManagingStock(ctx context.Context, p *gooProduct) bool {
	if !s.gooManageStock() {
		return false
	}
	return s.gooStockOwner(ctx, p).ManageStock
}

// gooIsInStock is is_in_stock().
func (s *Server) gooIsInStock(ctx context.Context, p *gooProduct) bool {
	owner := s.gooStockOwner(ctx, p)
	if s.gooManagingStock(ctx, p) && owner.StockQuantity != nil {
		return s.gooStockStatusFor(*owner.StockQuantity, owner.Backorders) != "outofstock"
	}
	return p.StockStatus != "outofstock"
}

// gooBackordersAllowed is backorders_allowed().
func (s *Server) gooBackordersAllowed(ctx context.Context, p *gooProduct) bool {
	return s.gooStockOwner(ctx, p).Backorders != "no"
}

// gooHasEnoughStock is has_enough_stock(qty).
func (s *Server) gooHasEnoughStock(ctx context.Context, p *gooProduct, qty int) bool {
	if !s.gooManagingStock(ctx, p) || s.gooBackordersAllowed(ctx, p) {
		return true
	}
	owner := s.gooStockOwner(ctx, p)
	return owner.StockQuantity != nil && *owner.StockQuantity >= qty
}

// gooStockQty is get_stock_quantity() of the stock owner, -1 when not managed.
func (s *Server) gooStockQty(ctx context.Context, p *gooProduct) int {
	if !s.gooManagingStock(ctx, p) {
		return -1
	}
	owner := s.gooStockOwner(ctx, p)
	if owner.StockQuantity == nil {
		return 0
	}
	return *owner.StockQuantity
}

// gooNeedsShipping is needs_shipping(): not virtual.
func (s *Server) gooNeedsShipping(ctx context.Context, p *gooProduct) bool {
	if def := gooProductTypes[p.Type]; def != nil && def.NeedsShipping != nil {
		return def.NeedsShipping(s, ctx, p)
	}
	return !p.Virtual
}

// gooTaxClassOf is get_tax_class(): a variation's "parent" class is its parent's.
func (s *Server) gooTaxClassOf(ctx context.Context, p *gooProduct) string {
	if p.Type == gooTypeVariation && p.TaxClass == "parent" {
		if parent := s.gooParent(ctx, p); parent != nil {
			return parent.TaxClass
		}
		return ""
	}
	return p.TaxClass
}

// gooShippingClassOf is get_shipping_class_id(): a variation without one uses its parent's.
func (s *Server) gooShippingClassOf(ctx context.Context, p *gooProduct) int64 {
	if p.Type == gooTypeVariation && p.ShippingClassID == 0 {
		if parent := s.gooParent(ctx, p); parent != nil {
			return parent.ShippingClassID
		}
	}
	return p.ShippingClassID
}

// gooWeightOf is get_weight() with a variation's parent fallback, in the store's unit.
func (s *Server) gooWeightOf(ctx context.Context, p *gooProduct) float64 {
	w := p.Weight
	if w == "" && p.Type == gooTypeVariation {
		if parent := s.gooParent(ctx, p); parent != nil {
			w = parent.Weight
		}
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(w), 64)
	return f
}

// gooImageOf is the product image id: a variation's own image, else the parent's.
func (s *Server) gooImageOf(ctx context.Context, p *gooProduct) int64 {
	if p.ImageID != 0 {
		return p.ImageID
	}
	if parent := s.gooParent(ctx, p); parent != nil {
		return parent.ImageID
	}
	return 0
}

// gooProductName is get_name(); a variation is "Parent – Attr, Attr" like WooCommerce's titles.
func (s *Server) gooProductTitle(ctx context.Context, p *gooProduct) string {
	if p.Type != gooTypeVariation {
		return p.Name
	}
	if parent := s.gooParent(ctx, p); parent != nil {
		return parent.Name
	}
	return p.Name
}

// gooVariationSummary is the variation's attributes as "Color: Red, Size: L".
func (s *Server) gooVariationSummary(ctx context.Context, p *gooProduct) string {
	parent := s.gooParent(ctx, p)
	if parent == nil {
		return ""
	}
	var parts []string
	for _, a := range parent.Attributes {
		if !a.Variation {
			continue
		}
		v := p.VariationAttrs[a.Key()]
		if v == "" {
			continue
		}
		parts = append(parts, s.gooAttrLabel(ctx, a)+": "+s.gooAttrValueLabel(ctx, a, v))
	}
	return strings.Join(parts, ", ")
}

// gooAttrLabel is wc_attribute_label().
func (s *Server) gooAttrLabel(ctx context.Context, a gooProductAttr) string {
	if a.Taxonomy != "" {
		if ga := s.gooAttributeByTaxonomy(ctx, a.Taxonomy); ga != nil {
			return ga.Name
		}
		return strings.TrimPrefix(a.Taxonomy, "pa_")
	}
	return a.Name
}

// gooAttrValueLabel is a value's display name: the term name of a taxonomy attribute.
func (s *Server) gooAttrValueLabel(ctx context.Context, a gooProductAttr, v string) string {
	if a.Taxonomy != "" {
		if t, err := s.store.TermBySlug(ctx, a.Taxonomy, v); err == nil {
			return t.Name
		}
	}
	return v
}

// gooPermalink is get_permalink() of a product (a variation links to its parent with the attributes).
func (s *Server) gooPermalink(ctx context.Context, p *gooProduct) string {
	lb := s.links(ctx)
	if p.Type == gooTypeVariation {
		parent := s.gooParent(ctx, p)
		if parent == nil {
			return lb.HomeURL("/")
		}
		u := s.gooPermalink(ctx, parent)
		var q []string
		keys := make([]string, 0, len(p.VariationAttrs))
		for k := range p.VariationAttrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v := p.VariationAttrs[k]; v != "" {
				q = append(q, "attribute_"+k+"="+urlEscape(v))
			}
		}
		if len(q) > 0 {
			u += "?" + strings.Join(q, "&")
		}
		return u
	}
	if p.Status != "publish" || p.Slug == "" {
		return lb.HomeURL("/?post_type=product&p=" + itoa64(p.ID))
	}
	link := lb.HomeURL("/" + s.gooProductBase() + "/" + p.Slug + "/")
	if p.Post != nil {
		link = s.gooPllLink(ctx, "post", p.Post, link) // Golylang: the product's language
	}
	return link
}

// gooProductBase is the product permalink base ("product", or "shop" style bases set in Permalinks).
func (s *Server) gooProductBase() string {
	b := strings.Trim(s.gooOptString("product_base"), "/")
	if b == "" {
		return "product"
	}
	return b
}

// Stock -------------------------------------------------------------------------------------------

// gooUpdateStock is wc_update_product_stock(): an atomic change (delta) or set of the managed quantity,
// then the stock status, the variable parent and the low / no stock notifications. Returns the new quantity.
func (s *Server) gooUpdateStock(ctx context.Context, p *gooProduct, delta int, set bool) (int, error) {
	local := s.gooStockOwner(ctx, p)
	owner := s.gooPllStockCanonical(ctx, local)
	if !owner.ManageStock {
		return -1, nil
	}
	var old *int
	if owner.StockQuantity != nil {
		v := *owner.StockQuantity
		old = &v
	}
	qty, err := s.gooStockDelta(ctx, owner, delta, set, false, 0)
	if err != nil {
		return 0, err
	}
	if !set {
		v := qty - delta // the quantity this change started from
		old = &v
	}
	was := math.MaxInt32
	if old != nil {
		was = *old
	}
	s.gooStockAfter(ctx, owner, was, qty, !set && delta < 0)
	if local != owner { // the caller's copy (a translation) follows the shared counter
		local.StockQuantity, local.StockStatus = &qty, owner.StockStatus
	}
	return qty, nil
}

// errGooNoStock is gooStockDelta refusing to sell more than is in stock.
var errGooNoStock = errors.New("not enough stock")

// gooStockDelta changes (or, with set, sets) the stock owner's quantity in one UPDATE that also writes
// the matching stock status, so concurrent changes cannot leave a status that does not fit the
// quantity. strict refuses a change that would take the quantity below what other orders than holder
// have reserved (goo_reserved_stock; below zero without reservations) unless the product allows
// backorders (errGooNoStock): two orders cannot both take the last unit, and a payment cannot take a
// unit another checkout holds.
func (s *Server) gooStockDelta(ctx context.Context, owner *gooProduct, delta int, set, strict bool, holder int64) (int, error) {
	newQty := `coalesce(stock_quantity, 0) + $2`
	if set {
		newQty = `$2`
	}
	var qty int
	update := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}) error {
		return q.QueryRow(ctx, `UPDATE goo_products SET stock_quantity = `+newQty+`,
		stock_status = CASE WHEN NOT $3 THEN stock_status WHEN `+newQty+` > $4 THEN 'instock'
			WHEN backorders <> 'no' THEN 'onbackorder' ELSE 'outofstock' END, updated_at = now()
		WHERE post_id = $1 AND (NOT $5 OR backorders <> 'no' OR `+newQty+` >= (SELECT coalesce(sum(r.stock_quantity), 0)
			FROM goo_reserved_stock r WHERE r.product_id = $1 AND r.order_id <> $6 AND r.expires_at > now())) RETURNING stock_quantity`,
			owner.ID, delta, !gooSubsVariableLike(owner.Type), s.gooOptInt("notify_no_stock_amount"), strict, holder).Scan(&qty)
	}
	var err error
	if strict {
		// The row is locked first so that the UPDATE, a later statement, sees every reservation
		// committed before the lock was granted (gooReserveStock holds the same lock).
		err = pgx.BeginFunc(ctx, s.store.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM goo_products WHERE post_id = $1 FOR UPDATE`, owner.ID); err != nil {
				return err
			}
			return update(tx)
		})
	} else {
		err = update(s.store.Pool)
	}
	if errors.Is(err, pgx.ErrNoRows) && strict {
		return 0, errGooNoStock
	}
	return qty, err
}

// gooStockAfter follows a stock change of the owner: its in-memory quantity and status, the variable
// parent's status, caches, the translations' stock and (notify) the low / no stock e-mails.
func (s *Server) gooStockAfter(ctx context.Context, owner *gooProduct, old, qty int, notify bool) {
	owner.StockQuantity = &qty
	if gooSubsVariableLike(owner.Type) {
		// Parent-managed stock of a variable product: the parent's status is synced from the variations.
		_ = s.gooSyncVariable(ctx, owner)
	} else {
		owner.StockStatus = s.gooStockStatusFor(qty, owner.Backorders)
		if owner.Type == gooTypeVariation {
			if parent := s.gooParent(ctx, owner); parent != nil {
				_ = s.gooSyncVariable(ctx, parent)
			}
		}
	}
	s.gooPurgeProduct(ctx, owner)
	s.gooPllStockSync(ctx, owner) // Golylang: translations share the stock (goo_golylang.go)
	if notify {
		s.gooStockNotifications(ctx, owner, &old, qty)
	}
}

// gooStockNotifications sends the low stock / no stock e-mails when a reduction crosses a threshold.
func (s *Server) gooStockNotifications(ctx context.Context, p *gooProduct, old *int, qty int) {
	noStock := s.gooOptInt("notify_no_stock_amount")
	low := s.gooOptInt("notify_low_stock_amount")
	if p.LowStockAmount != nil {
		low = *p.LowStockAmount
	}
	was := math.MaxInt32
	if old != nil {
		was = *old
	}
	switch {
	case qty <= noStock && was > noStock && s.gooOptBool("notify_no_stock"):
		s.gooFire(ctx, "no_stock", p)
	case qty <= low && was > low && s.gooOptBool("notify_low_stock"):
		s.gooFire(ctx, "low_stock", p)
	}
}

// Global attributes ------------------------------------------------------------------------------

// gooAttribute is a global attribute (Products › Attributes): the taxonomy pa_<slug>.
type gooAttribute struct {
	ID          int64
	Name        string
	Slug        string
	Type        string
	OrderBy     string
	HasArchives bool
}

func (a *gooAttribute) Taxonomy() string { return "pa_" + a.Slug }

// gooAttributes is wc_get_attribute_taxonomies().
func (s *Server) gooAttributes(ctx context.Context) ([]*gooAttribute, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT id, name, slug, type, order_by, has_archives FROM goo_attributes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*gooAttribute
	for rows.Next() {
		a := &gooAttribute{}
		if err := rows.Scan(&a.ID, &a.Name, &a.Slug, &a.Type, &a.OrderBy, &a.HasArchives); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Server) gooAttributeByTaxonomy(ctx context.Context, tax string) *gooAttribute {
	a := &gooAttribute{}
	if err := s.store.Pool.QueryRow(ctx, `SELECT id, name, slug, type, order_by, has_archives FROM goo_attributes WHERE slug = $1`,
		strings.TrimPrefix(tax, "pa_")).Scan(&a.ID, &a.Name, &a.Slug, &a.Type, &a.OrderBy, &a.HasArchives); err != nil {
		return nil
	}
	return a
}

// gooSaveAttribute creates or updates a global attribute and registers its taxonomy.
func (s *Server) gooSaveAttribute(ctx context.Context, a *gooAttribute) error {
	defer gooPllFlushAttributes() // Golylang translates the attribute taxonomies
	a.Slug = strings.TrimPrefix(gooSanitizeKey(defaultStr(a.Slug, a.Name)), "pa_")
	if a.Slug == "" {
		return errors.New("Please, provide an attribute name and slug.")
	}
	if len(a.Slug) > 28 {
		return fmt.Errorf("Slug \"%s\" is too long (28 characters max). Shorten it, please.", a.Slug)
	}
	if a.Type == "" {
		a.Type = "select"
	}
	if a.OrderBy == "" {
		a.OrderBy = "menu_order"
	}
	var oldSlug string
	if a.ID != 0 {
		_ = s.store.Pool.QueryRow(ctx, `SELECT slug FROM goo_attributes WHERE id = $1`, a.ID).Scan(&oldSlug)
	}
	var taken bool
	_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM goo_attributes WHERE slug = $1 AND id <> $2)`, a.Slug, a.ID).Scan(&taken)
	if taken {
		return fmt.Errorf("Slug \"%s\" is already in use. Change it, please.", a.Slug)
	}
	if a.ID == 0 {
		if err := s.store.Pool.QueryRow(ctx, `INSERT INTO goo_attributes (name, slug, type, order_by, has_archives) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			a.Name, a.Slug, a.Type, a.OrderBy, a.HasArchives).Scan(&a.ID); err != nil {
			return err
		}
	} else if _, err := s.store.Pool.Exec(ctx, `UPDATE goo_attributes SET name=$2, slug=$3, type=$4, order_by=$5, has_archives=$6 WHERE id=$1`,
		a.ID, a.Name, a.Slug, a.Type, a.OrderBy, a.HasArchives); err != nil {
		return err
	}
	if oldSlug != "" && oldSlug != a.Slug {
		// Renaming moves the taxonomy and its terms (ON UPDATE CASCADE) and the products' attributes.
		if _, err := s.store.Pool.Exec(ctx, `UPDATE taxonomies SET slug = $2 WHERE slug = $1`, "pa_"+oldSlug, a.Taxonomy()); err != nil {
			return err
		}
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_products SET attributes = replace(attributes::text, $1, $2)::jsonb,
			variation_attrs = replace(variation_attrs::text, $1, $2)::jsonb, default_attributes = replace(default_attributes::text, $1, $2)::jsonb
			WHERE attributes::text LIKE '%' || $1 || '%' OR variation_attrs::text LIKE '%' || $1 || '%'`, `"pa_`+oldSlug+`"`, `"`+a.Taxonomy()+`"`)
	}
	_, err := s.store.Pool.Exec(ctx, `INSERT INTO taxonomies (slug, name, hierarchical, object_types, rest_base, public, is_builtin, labels)
		VALUES ($1, $2, false, '["product"]', '', $3, false, $4) ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name, public = EXCLUDED.public, labels = EXCLUDED.labels`,
		a.Taxonomy(), a.Name, a.HasArchives, fmt.Sprintf(`{"name":%q,"singular_name":%q}`, a.Name, a.Name))
	return err
}

// gooDeleteAttribute deletes a global attribute, its taxonomy and terms.
func (s *Server) gooDeleteAttribute(ctx context.Context, id int64) error {
	defer gooPllFlushAttributes()
	var slug string
	if err := s.store.Pool.QueryRow(ctx, `DELETE FROM goo_attributes WHERE id = $1 RETURNING slug`, id).Scan(&slug); err != nil {
		return err
	}
	_, err := s.store.Pool.Exec(ctx, `DELETE FROM taxonomies WHERE slug = $1`, "pa_"+slug)
	return err
}

// gooAttributeTerms are a taxonomy attribute's terms in the attribute's order.
func (s *Server) gooAttributeTerms(ctx context.Context, tax string) ([]*store.Term, error) {
	terms, _, err := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: tax})
	if err != nil {
		return nil, err
	}
	if a := s.gooAttributeByTaxonomy(ctx, tax); a != nil {
		switch a.OrderBy {
		case "id":
			sort.SliceStable(terms, func(i, j int) bool { return terms[i].ID < terms[j].ID })
		case "name", "name_num":
			sort.SliceStable(terms, func(i, j int) bool { return wp.SanitizeTitle(terms[i].Name) < wp.SanitizeTitle(terms[j].Name) })
		default:
			sort.SliceStable(terms, func(i, j int) bool {
				oi, _ := terms[i].Meta["order"].(float64)
				oj, _ := terms[j].Meta["order"].(float64)
				return oi < oj
			})
		}
	}
	return terms, nil
}
