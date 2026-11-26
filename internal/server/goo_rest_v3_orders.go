package server

import (
	"context"
	"net/http"
	"net/mail"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// wc/v3 orders (+ notes, refunds), coupons and customers.

func (s *Server) registerGooV3Orders() {
	orders := gooCapsAll("edit_shop_orders")
	s.gooV3("/orders", orders, map[string]restHandler{http.MethodGet: s.gooV3OrdersList, http.MethodPost: s.gooV3Create(s.gooV3OrderCreate)})
	s.gooV3("/orders/batch", orders, map[string]restHandler{http.MethodPost: func(c *gin.Context, req *restRequest) {
		s.gooBatch(c, req, gooBatchOps{Create: s.gooV3OrderCreate, Update: s.gooV3OrderUpdate, Delete: s.gooV3OrderDelete})
	}})
	s.gooV3("/orders/(?P<id>[\\d]+)", orders, map[string]restHandler{
		http.MethodGet: s.gooV3Get(func(c *gin.Context, id int64) (map[string]any, *gooAPIErr) {
			o, err := s.gooV3LoadOrder(c.Request.Context(), id)
			if err != nil {
				return nil, err
			}
			return s.gooV3OrderJSON(c.Request.Context(), o), nil
		}),
		http.MethodPut:    s.gooV3Update(s.gooV3OrderUpdate),
		http.MethodDelete: s.gooV3Delete(s.gooV3OrderDelete),
	})
	s.gooV3("/orders/(?P<order_id>[\\d]+)/notes", orders, map[string]restHandler{http.MethodGet: s.gooV3NotesList, http.MethodPost: s.gooV3NoteCreate})
	s.gooV3("/orders/(?P<order_id>[\\d]+)/notes/(?P<id>[\\d]+)", orders, map[string]restHandler{http.MethodGet: s.gooV3NoteGet, http.MethodDelete: s.gooV3NoteDelete})
	s.gooV3("/orders/(?P<order_id>[\\d]+)/refunds", orders, map[string]restHandler{http.MethodGet: s.gooV3RefundsList, http.MethodPost: s.gooV3RefundCreate})
	s.gooV3("/orders/(?P<order_id>[\\d]+)/refunds/(?P<id>[\\d]+)", orders, map[string]restHandler{http.MethodGet: s.gooV3RefundGet, http.MethodDelete: s.gooV3RefundDelete})
	s.gooV3("/refunds", orders, map[string]restHandler{http.MethodGet: s.gooV3AllRefunds})

	coupons := gooCapsAll("edit_shop_coupons")
	s.gooV3("/coupons", coupons, map[string]restHandler{http.MethodGet: s.gooV3CouponsList, http.MethodPost: s.gooV3Create(s.gooV3CouponCreate)})
	s.gooV3("/coupons/batch", coupons, map[string]restHandler{http.MethodPost: func(c *gin.Context, req *restRequest) {
		s.gooBatch(c, req, gooBatchOps{Create: s.gooV3CouponCreate, Update: s.gooV3CouponUpdate, Delete: s.gooV3CouponDelete})
	}})
	s.gooV3("/coupons/(?P<id>[\\d]+)", coupons, map[string]restHandler{
		http.MethodGet: s.gooV3Get(func(c *gin.Context, id int64) (map[string]any, *gooAPIErr) {
			cp, err := s.gooCouponByID(c.Request.Context(), id)
			if err != nil {
				return nil, gooErr(404, "woocommerce_rest_shop_coupon_invalid_id", "Invalid ID.")
			}
			return s.gooV3CouponJSON(c.Request.Context(), cp), nil
		}),
		http.MethodPut:    s.gooV3Update(s.gooV3CouponUpdate),
		http.MethodDelete: s.gooV3Delete(s.gooV3CouponDelete),
	})

	customers := gooCaps{"list_users", "edit_users", "delete_users"}
	s.gooV3("/customers", customers, map[string]restHandler{http.MethodGet: s.gooV3CustomersList, http.MethodPost: s.gooV3Create(s.gooV3CustomerCreate)})
	s.gooV3("/customers/batch", customers, map[string]restHandler{http.MethodPost: func(c *gin.Context, req *restRequest) {
		s.gooBatch(c, req, gooBatchOps{Create: s.gooV3CustomerCreate, Update: s.gooV3CustomerUpdate, Delete: s.gooV3CustomerDelete})
	}})
	s.gooV3("/customers/(?P<id>[\\d]+)", customers, map[string]restHandler{
		http.MethodGet: s.gooV3Get(func(c *gin.Context, id int64) (map[string]any, *gooAPIErr) {
			u, err := s.store.UserByID(c.Request.Context(), id)
			if err != nil {
				return nil, gooErr(404, "woocommerce_rest_invalid_id", "Invalid resource ID.")
			}
			return s.gooV3CustomerJSON(c, u), nil
		}),
		http.MethodPut:    s.gooV3Update(s.gooV3CustomerUpdate),
		http.MethodDelete: s.gooV3Delete(s.gooV3CustomerDelete),
	})
	s.gooV3("/customers/(?P<id>[\\d]+)/downloads", customers, map[string]restHandler{http.MethodGet: s.gooV3CustomerDownloads})
}

// Orders ------------------------------------------------------------------------------------------------

func (s *Server) gooV3LoadOrder(ctx context.Context, id int64) (*gooOrder, *gooAPIErr) {
	o, err := s.gooGetOrder(ctx, id)
	if err != nil || o.Type != "shop_order" {
		return nil, gooErr(http.StatusNotFound, "woocommerce_rest_shop_order_invalid_id", "Invalid ID.")
	}
	return o, nil
}

func gooItemTaxesJSON(t gooItemTaxes) []any {
	ids := map[string]bool{}
	for k := range t.Total {
		ids[k] = true
	}
	for k := range t.Subtotal {
		ids[k] = true
	}
	keys := make([]string, 0, len(ids))
	for k := range ids {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return gooInt(keys[i]) < gooInt(keys[j]) })
	out := []any{}
	for _, k := range keys {
		row := map[string]any{"id": gooInt(k), "total": gooFormatDecimal(t.Total[k], -1)}
		if t.Subtotal != nil {
			row["subtotal"] = gooFormatDecimal(t.Subtotal[k], -1)
		}
		out = append(out, row)
	}
	return out
}

// gooItemMetaJSON is an order item's public meta with display keys / values.
func (s *Server) gooItemMetaJSON(ctx context.Context, it *gooOrderItem) []any {
	keys := make([]string, 0, len(it.Meta))
	for k := range it.Meta {
		if !strings.HasPrefix(k, "_") && !(it.Type == "shipping" && (k == "method_id" || k == "instance_id")) &&
			!(it.Type == "tax" || it.Type == "coupon") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := []any{}
	for i, k := range keys {
		v := it.Meta[k]
		dk, dv := k, gooStr(v)
		if strings.HasPrefix(k, "pa_") {
			a := gooProductAttr{Taxonomy: k}
			dk, dv = s.gooAttrLabel(ctx, a), s.gooAttrValueLabel(ctx, a, gooStr(v))
		}
		out = append(out, map[string]any{"id": i + 1, "key": k, "value": v, "display_key": dk, "display_value": dv})
	}
	return out
}

// gooV3OrderJSON is an order as WC_REST_Orders_Controller returns it.
func (s *Server) gooV3OrderJSON(ctx context.Context, o *gooOrder) map[string]any {
	dec := func(v float64) string { return s.gooDec(v) }
	lines := []any{}
	for _, it := range o.ItemsOf("line_item") {
		pid := it.ProductID
		if it.VariationID != 0 {
			pid = it.VariationID
		}
		sku, parentName := "", any(nil)
		img := map[string]any{"id": "", "src": ""}
		if p, err := s.gooGetProduct(ctx, pid); err == nil {
			sku = p.SKU
			if id := s.gooImageOf(ctx, p); id != 0 {
				if a, err := s.store.GetAttachment(ctx, id); err == nil {
					img = map[string]any{"id": id, "src": s.uploadURL(ctx, a.FilePath)}
				}
			}
			if p.Type == gooTypeVariation {
				parentName = s.gooProductTitle(ctx, p)
			}
		}
		price := 0.0
		if it.Quantity != 0 {
			price = it.Total / it.Quantity
		}
		lines = append(lines, map[string]any{"id": it.ID, "name": it.Name, "product_id": it.ProductID, "variation_id": it.VariationID,
			"quantity": it.Qty(), "tax_class": it.TaxClass, "subtotal": dec(it.Subtotal), "subtotal_tax": dec(it.SubtotalTax),
			"total": dec(it.Total), "total_tax": dec(it.TotalTax), "taxes": gooItemTaxesJSON(it.Taxes), "meta_data": s.gooItemMetaJSON(ctx, it),
			"sku": sku, "price": price, "image": img, "parent_name": parentName})
	}
	taxLines := []any{}
	for _, it := range o.ItemsOf("tax") {
		amt, _ := gooFloat(it.Meta["tax_amount"])
		ship, _ := gooFloat(it.Meta["shipping_tax_amount"])
		pct, _ := gooFloat(it.Meta["rate_percent"])
		taxLines = append(taxLines, map[string]any{"id": it.ID, "rate_code": it.Name, "rate_id": gooInt(it.Meta["rate_id"]), "label": gooStr(it.Meta["label"]),
			"compound": gooBool(it.Meta["compound"]), "tax_total": dec(amt), "shipping_tax_total": dec(ship), "rate_percent": pct, "meta_data": []any{}})
	}
	shipLines := []any{}
	for _, it := range o.ItemsOf("shipping") {
		shipLines = append(shipLines, map[string]any{"id": it.ID, "method_title": it.Name, "method_id": gooStr(it.Meta["method_id"]),
			"instance_id": gooStr(it.Meta["instance_id"]), "total": dec(it.Total), "total_tax": dec(it.TotalTax),
			"taxes": gooItemTaxesJSON(gooItemTaxes{Total: it.Taxes.Total}), "meta_data": s.gooItemMetaJSON(ctx, it)})
	}
	feeLines := []any{}
	for _, it := range o.ItemsOf("fee") {
		taxStatus := "taxable"
		if it.TotalTax == 0 && len(it.Taxes.Total) == 0 {
			taxStatus = "none"
		}
		feeLines = append(feeLines, map[string]any{"id": it.ID, "name": it.Name, "tax_class": it.TaxClass, "tax_status": taxStatus,
			"amount": dec(it.Total), "total": dec(it.Total), "total_tax": dec(it.TotalTax), "taxes": gooItemTaxesJSON(gooItemTaxes{Total: it.Taxes.Total}),
			"meta_data": s.gooItemMetaJSON(ctx, it)})
	}
	couponLines := []any{}
	for _, it := range o.ItemsOf("coupon") {
		couponLines = append(couponLines, map[string]any{"id": it.ID, "code": it.Name, "discount": dec(it.Total), "discount_tax": dec(it.TotalTax),
			"discount_type": "", "nominal_amount": 0, "free_shipping": false, "meta_data": []any{}})
	}
	refunds := []any{}
	if list, err := s.gooRefunds(ctx, o.ID); err == nil {
		for _, r := range list {
			refunds = append(refunds, map[string]any{"id": r.ID, "reason": r.RefundReason, "total": dec(r.Total)})
		}
	}
	paymentURL := ""
	if containsStr([]string{"pending", "failed"}, o.Status) {
		paymentURL = s.gooOrderPayURL(ctx, o)
	}
	return map[string]any{
		"id": o.ID, "parent_id": o.ParentID, "status": o.Status, "currency": o.Currency, "version": gooVersion,
		"prices_include_tax": o.PricesIncludeTax, "date_created": s.gooDate(&o.DateCreated), "date_modified": s.gooDate(&o.DateModified),
		"discount_total": dec(o.DiscountTotal), "discount_tax": dec(o.DiscountTax), "shipping_total": dec(o.ShippingTotal),
		"shipping_tax": dec(o.ShippingTax), "cart_tax": dec(o.CartTax), "total": dec(o.Total), "total_tax": dec(o.TotalTax),
		"customer_id": o.CustomerID, "order_key": o.OrderKey, "billing": gooAddressJSON(o.Billing, true), "shipping": gooAddressJSON(o.Shipping, false),
		"payment_method": o.PaymentMethod, "payment_method_title": o.PaymentMethodTitle, "transaction_id": o.TransactionID,
		"customer_ip_address": o.CustomerIP, "customer_user_agent": o.UserAgent, "created_via": o.CreatedVia, "customer_note": o.CustomerNote,
		"date_completed": s.gooDate(o.DateCompleted), "date_paid": s.gooDate(o.DatePaid), "cart_hash": o.CartHash, "number": o.Number(),
		"meta_data": gooMetaJSON(o.Meta), "line_items": lines, "tax_lines": taxLines, "shipping_lines": shipLines, "fee_lines": feeLines,
		"coupon_lines": couponLines, "refunds": refunds, "payment_url": paymentURL,
		"is_editable":      containsStr([]string{"pending", "on-hold", "auto-draft", "checkout-draft"}, o.Status),
		"needs_payment":    containsStr([]string{"pending", "failed"}, o.Status) && o.Total > 0,
		"needs_processing": s.gooOrderNeedsProcessing(ctx, o),
		"date_created_gmt": gooDateGMT(&o.DateCreated), "date_modified_gmt": gooDateGMT(&o.DateModified),
		"date_completed_gmt": gooDateGMT(o.DateCompleted), "date_paid_gmt": gooDateGMT(o.DatePaid),
		"currency_symbol": strings.ReplaceAll(gooCurrencySymbol(o.Currency), "&nbsp;", " "),
		"_links":          s.gooLinks(ctx, "orders", o.ID),
	}
}

// gooOrderLocation is the address taxes are based on for an order.
func (s *Server) gooOrderLocation(o *gooOrder) gooLocation {
	var a map[string]string
	switch s.gooOptString("tax_based_on") {
	case "base":
		return s.gooBaseLocation()
	case "billing":
		a = o.Billing
	default:
		a = o.Shipping
		if a["country"] == "" {
			a = o.Billing
		}
	}
	return gooLocation{Country: a["country"], State: a["state"], Postcode: a["postcode"], City: a["city"]}
}

func gooTaxKeys(m map[int64]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range m {
		out[itoa64(k)] = v
	}
	return out
}

// gooOrderTaxes recalculates an order's coupon discounts, item taxes and tax lines, then the totals
// (WC_Order::recalculate_coupons() + calculate_totals() for an order built through the API).
func (s *Server) gooOrderTaxes(ctx context.Context, o *gooOrder) {
	// Coupons: discounts over the line items (subtotal → total), with the coupons' product, category and
	// sale restrictions and the sequential setting, exactly like the admin's Recalculate.
	s.gooOrderApplyCoupons(ctx, o)
	s.gooOrderTaxesKeepTotals(ctx, o)
}

// gooOrderTaxesKeepTotals is WC_Order::calculate_totals(): item taxes, tax lines and totals from the
// line totals as they are (posted or stored discounts stay; coupons are not re-applied).
func (s *Server) gooOrderTaxesKeepTotals(ctx context.Context, o *gooOrder) {
	dp := s.gooDecimals()
	loc := s.gooOrderLocation(o)
	all := map[int64]float64{}
	ship := map[int64]float64{}
	for _, it := range o.Items {
		switch it.Type {
		case "line_item":
			it.Taxes = gooItemTaxes{Total: map[string]float64{}, Subtotal: map[string]float64{}}
			it.TotalTax, it.SubtotalTax = 0, 0
			pid := it.ProductID
			if it.VariationID != 0 {
				pid = it.VariationID
			}
			p, err := s.gooGetProduct(ctx, pid)
			if err != nil || p.TaxStatus != "taxable" {
				continue
			}
			rates := s.gooFindRates(ctx, s.gooTaxClassOf(ctx, p), loc, false)
			if len(rates) == 0 {
				continue
			}
			it.TaxClass = s.gooTaxClassOf(ctx, p)
			tt := gooRoundTaxes(gooCalcTax(it.Total, rates, false), dp)
			st := gooRoundTaxes(gooCalcTax(it.Subtotal, rates, false), dp)
			it.Taxes = gooItemTaxes{Total: gooTaxKeys(tt), Subtotal: gooTaxKeys(st)}
			it.TotalTax, it.SubtotalTax = gooSumTaxes(tt), gooSumTaxes(st)
			for k, v := range tt {
				all[k] += v
			}
		case "fee":
			if it.MetaString("_tax_status") == "none" {
				it.Taxes, it.TotalTax = gooItemTaxes{Total: map[string]float64{}}, 0
				continue
			}
			rates := s.gooFindRates(ctx, it.TaxClass, loc, false)
			tt := gooRoundTaxes(gooCalcTax(it.Total, rates, false), dp)
			it.Taxes, it.TotalTax = gooItemTaxes{Total: gooTaxKeys(tt)}, gooSumTaxes(tt)
			for k, v := range tt {
				all[k] += v
			}
		case "shipping":
			class := s.gooOptString("shipping_tax_class")
			if class == "inherit" {
				class = ""
			}
			rates := s.gooFindRates(ctx, class, loc, true)
			tt := gooRoundTaxes(gooCalcTax(it.Total, rates, false), dp)
			it.Taxes, it.TotalTax = gooItemTaxes{Total: gooTaxKeys(tt)}, gooSumTaxes(tt)
			for k, v := range tt {
				ship[k] += v
			}
		}
	}
	// Tax lines: one per rate.
	var kept []*gooOrderItem
	for _, it := range o.Items {
		if it.Type != "tax" {
			kept = append(kept, it)
		}
	}
	rateIDs := map[int64]bool{}
	for k := range all {
		rateIDs[k] = true
	}
	for k := range ship {
		rateIDs[k] = true
	}
	ids := make([]int64, 0, len(rateIDs))
	for k := range rateIDs {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		r := s.gooTaxRateByID(ctx, id)
		if r == nil {
			continue
		}
		kept = append(kept, &gooOrderItem{Type: "tax", Name: gooTaxRateCode(r), Meta: map[string]any{"rate_id": float64(id), "label": r.Name, "compound": r.Compound,
			"tax_amount": gooRound(all[id], dp), "shipping_tax_amount": gooRound(ship[id], dp), "rate_percent": r.Rate}})
	}
	o.Items = kept
	s.gooRecalculate(o)
}

// gooV3ApplyOrder writes the request fields onto an order (items, addresses, payment, meta).
func (s *Server) gooV3ApplyOrder(ctx context.Context, o *gooOrder, b map[string]any) *gooAPIErr {
	if v, ok := b["parent_id"]; ok {
		o.ParentID = gooInt(v)
	}
	if v, ok := b["currency"]; ok && gooStr(v) != "" {
		cur := strings.ToUpper(gooStr(v))
		if _, ok := gooCurrencies[cur]; !ok {
			return gooErr(400, "rest_invalid_param", "Invalid parameter(s): currency")
		}
		o.Currency = cur
	}
	if v, ok := b["customer_id"]; ok {
		id := gooInt(v)
		if id != 0 {
			if _, err := s.store.UserByID(ctx, id); err != nil {
				return gooErr(400, "woocommerce_rest_invalid_customer_id", "Customer ID is invalid.")
			}
		}
		o.CustomerID = id
	}
	if v, ok := b["billing"]; ok {
		gooApplyAddress(o.Billing, v)
		if e := o.Billing["email"]; e != "" {
			if _, err := mail.ParseAddress(e); err != nil {
				return gooErr(400, "rest_invalid_param", "Invalid parameter(s): billing")
			}
		}
	}
	if v, ok := b["shipping"]; ok {
		gooApplyAddress(o.Shipping, v)
	}
	for k, dst := range map[string]*string{"payment_method": &o.PaymentMethod, "payment_method_title": &o.PaymentMethodTitle,
		"transaction_id": &o.TransactionID, "customer_note": &o.CustomerNote, "customer_ip_address": &o.CustomerIP, "customer_user_agent": &o.UserAgent} {
		if v, ok := b[k]; ok {
			*dst = gooStr(v)
		}
	}
	if _, ok := b["payment_method_title"]; !ok && o.PaymentMethod != "" && o.PaymentMethodTitle == "" {
		if gw := s.gooGateway(o.PaymentMethod); gw != nil {
			o.PaymentMethodTitle = gw.Title()
		}
	}
	if v, ok := b["meta_data"]; ok {
		gooApplyMeta(o.Meta, v)
	}
	changed, linesChanged, couponsChanged := false, false, false
	byID := map[int64]*gooOrderItem{}
	for _, it := range o.Items {
		byID[it.ID] = it
	}
	remove := map[int64]bool{}
	// Line items.
	for _, x := range gooList(b["line_items"]) {
		m := gooObj(x)
		changed, linesChanged = true, true
		if id := gooInt(m["id"]); id > 0 {
			it := byID[id]
			if it == nil || it.Type != "line_item" {
				return gooErr(400, "woocommerce_rest_invalid_item_id", "Order item ID provided is not associated with order.")
			}
			if pv, ok := m["product_id"]; ok && pv == nil {
				remove[id] = true
				continue
			}
			if qv, ok := m["quantity"]; ok {
				q := gooInt(qv)
				if q <= 0 {
					remove[id] = true
					continue
				}
				unit, unitTotal := 0.0, 0.0
				if it.Quantity != 0 {
					unit, unitTotal = it.Subtotal/it.Quantity, it.Total/it.Quantity
				}
				it.Quantity = float64(q)
				it.Subtotal = gooRound(unit*it.Quantity, s.gooDecimals())
				it.Total = gooRound(unitTotal*it.Quantity, s.gooDecimals())
			}
			if v, ok := m["subtotal"]; ok {
				undiscounted := gooRound(it.Total-it.Subtotal, s.gooDecimals()) == 0
				it.Subtotal, _ = gooFloat(v)
				if _, ok := m["total"]; !ok && undiscounted {
					it.Total = it.Subtotal // a line without a discount keeps none
				}
			}
			if v, ok := m["total"]; ok {
				it.Total, _ = gooFloat(v)
			}
			if v, ok := m["name"]; ok {
				it.Name = gooStr(v)
			}
			gooApplyMeta(it.Meta, m["meta_data"])
			continue
		}
		it, aerr := s.gooV3NewLineItem(ctx, o, m)
		if aerr != nil {
			return aerr
		}
		o.Items = append(o.Items, it)
	}
	// Shipping lines.
	for _, x := range gooList(b["shipping_lines"]) {
		m := gooObj(x)
		changed = true
		it := byID[gooInt(m["id"])]
		if gooInt(m["id"]) > 0 && (it == nil || it.Type != "shipping") {
			return gooErr(400, "woocommerce_rest_invalid_item_id", "Order item ID provided is not associated with order.")
		}
		if it != nil {
			if mv, ok := m["method_id"]; ok && mv == nil {
				remove[it.ID] = true
				continue
			}
		} else {
			it = &gooOrderItem{Type: "shipping", Meta: map[string]any{}}
			o.Items = append(o.Items, it)
		}
		if v, ok := m["method_title"]; ok {
			it.Name = gooStr(v)
		}
		if v, ok := m["method_id"]; ok {
			it.Meta["method_id"] = gooStr(v)
		}
		if v, ok := m["instance_id"]; ok {
			it.Meta["instance_id"] = gooStr(v)
		}
		if v, ok := m["total"]; ok {
			it.Total, _ = gooFloat(v)
		}
		gooApplyMeta(it.Meta, m["meta_data"])
	}
	// Fee lines.
	for _, x := range gooList(b["fee_lines"]) {
		m := gooObj(x)
		changed = true
		it := byID[gooInt(m["id"])]
		if gooInt(m["id"]) > 0 && (it == nil || it.Type != "fee") {
			return gooErr(400, "woocommerce_rest_invalid_item_id", "Order item ID provided is not associated with order.")
		}
		if it != nil {
			if nv, ok := m["name"]; ok && nv == nil {
				remove[it.ID] = true
				continue
			}
		} else {
			if gooStr(m["name"]) == "" {
				return gooErr(400, "woocommerce_rest_invalid_fee_item", "Fee name is required.")
			}
			it = &gooOrderItem{Type: "fee", Meta: map[string]any{}}
			o.Items = append(o.Items, it)
		}
		if v, ok := m["name"]; ok {
			it.Name = gooStr(v)
		}
		if v, ok := m["total"]; ok {
			it.Total, _ = gooFloat(v)
		}
		if v, ok := m["tax_class"]; ok {
			it.TaxClass = strings.TrimPrefix(gooStr(v), "standard")
		}
		if v, ok := m["tax_status"]; ok {
			it.Meta["_tax_status"] = gooStr(v)
		}
		gooApplyMeta(it.Meta, m["meta_data"])
	}
	// Coupon lines.
	for _, x := range gooList(b["coupon_lines"]) {
		m := gooObj(x)
		changed, couponsChanged = true, true
		if id := gooInt(m["id"]); id > 0 {
			if it := byID[id]; it != nil && it.Type == "coupon" {
				if cv, ok := m["code"]; ok && cv == nil {
					remove[id] = true
				}
			}
			continue
		}
		code := strings.ToLower(strings.TrimSpace(gooStr(m["code"])))
		if code == "" {
			return gooErr(400, "woocommerce_rest_invalid_coupon", "Coupon code is required.")
		}
		if _, err := s.gooCouponByCode(ctx, code); err != nil {
			return gooErr(400, "woocommerce_rest_invalid_coupon", `Coupon "`+code+`" does not exist!`)
		}
		o.Items = append(o.Items, &gooOrderItem{Type: "coupon", Name: code, Meta: map[string]any{}})
	}
	if len(remove) > 0 {
		var kept []*gooOrderItem
		for _, it := range o.Items {
			if !remove[it.ID] || it.ID == 0 {
				kept = append(kept, it)
			}
		}
		o.Items = kept
	}
	if changed {
		// Line totals are re-derived from the coupons only when the coupons change (or lines change on an
		// order with coupons); otherwise the posted / stored line totals (manual discounts) are kept.
		if couponsChanged || (linesChanged && len(o.ItemsOf("coupon")) > 0) {
			s.gooOrderTaxes(ctx, o)
		} else {
			s.gooOrderTaxesKeepTotals(ctx, o)
		}
	}
	return nil
}

// gooV3NewLineItem builds a line item from {product_id, variation_id, quantity, subtotal, total, name, meta_data}.
func (s *Server) gooV3NewLineItem(ctx context.Context, o *gooOrder, m map[string]any) (*gooOrderItem, *gooAPIErr) {
	pid, vid := gooInt(m["product_id"]), gooInt(m["variation_id"])
	if sku := gooStr(m["sku"]); pid == 0 && sku != "" {
		_ = s.store.Pool.QueryRow(ctx, `SELECT post_id FROM goo_products WHERE sku = $1`, sku).Scan(&pid)
	}
	id := pid
	if vid != 0 {
		id = vid
	}
	p, err := s.gooGetProduct(ctx, id)
	if err != nil {
		return nil, gooErr(400, "woocommerce_rest_invalid_product_id", "Product ID provided is invalid.")
	}
	if p.Type == gooTypeVariation {
		pid, vid = p.ParentID, p.ID
	}
	qty := float64(1)
	if v, ok := m["quantity"]; ok {
		qty = float64(gooInt(v))
	}
	if qty <= 0 {
		return nil, gooErr(400, "woocommerce_rest_invalid_product_quantity", "Product quantity must be a positive float.")
	}
	unit := p.PriceValue()
	if s.gooPricesIncludeTax() {
		unit = s.gooPriceExcludingTax(ctx, p, unit, 1, s.gooOrderLocation(o))
	}
	it := &gooOrderItem{Type: "line_item", Name: s.gooProductTitle(ctx, p), ProductID: pid, VariationID: vid, Quantity: qty,
		TaxClass: s.gooTaxClassOf(ctx, p), Meta: map[string]any{}}
	it.Subtotal = gooRound(unit*qty, s.gooDecimals())
	if v, ok := m["subtotal"]; ok {
		it.Subtotal, _ = gooFloat(v)
	} else if v, ok := m["price"]; ok {
		f, _ := gooFloat(v)
		it.Subtotal = gooRound(f*qty, s.gooDecimals())
	}
	// total is the line after discounts (defaults to the subtotal), like WC's set_total().
	it.Total = it.Subtotal
	if v, ok := m["total"]; ok {
		it.Total, _ = gooFloat(v)
	}
	if p.Type == gooTypeVariation {
		var vals []string
		for k, v := range p.VariationAttrs {
			it.Meta[k] = v
		}
		if parent := s.gooParent(ctx, p); parent != nil {
			for _, a := range parent.Attributes {
				if v := p.VariationAttrs[a.Key()]; a.Variation && v != "" {
					vals = append(vals, s.gooAttrValueLabel(ctx, a, v))
				}
			}
		}
		if len(vals) > 0 {
			it.Name += " - " + strings.Join(vals, ", ")
		}
	}
	if v, ok := m["name"]; ok && gooStr(v) != "" {
		it.Name = gooStr(v)
	}
	gooApplyMeta(it.Meta, m["meta_data"])
	return it, nil
}

// gooV3OrderStatus applies status / set_paid after the order is saved (transitions run their hooks).
func (s *Server) gooV3OrderStatus(ctx context.Context, o *gooOrder, b map[string]any, by string) *gooAPIErr {
	if v, ok := b["status"]; ok {
		st := strings.TrimPrefix(gooStr(v), "wc-")
		if st != o.Status {
			if err := s.gooSetStatus(ctx, o, st, "", by); err != nil {
				return gooErr(400, "rest_invalid_param", "Invalid parameter(s): status")
			}
		}
	}
	if gooBool(b["set_paid"]) {
		if err := s.gooPaymentComplete(ctx, o, o.TransactionID); err != nil {
			return gooErr(500, "woocommerce_rest_cannot_edit", err.Error())
		}
	}
	return nil
}

func gooActorName(c *gin.Context) string {
	if u := gooV3Actor(c); u != nil {
		return u.DisplayName
	}
	return ""
}

func (s *Server) gooV3OrderCreate(c *gin.Context, b map[string]any) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	o := &gooOrder{Type: "shop_order", Status: "pending", Currency: s.gooCurrency(), PricesIncludeTax: s.gooPricesIncludeTax(),
		Billing: map[string]string{}, Shipping: map[string]string{}, CreatedVia: "rest-api", Meta: map[string]any{}}
	if aerr := s.gooV3ApplyOrder(ctx, o, b); aerr != nil {
		return nil, aerr
	}
	if len(o.Items) == 0 {
		s.gooRecalculate(o)
	}
	if err := s.gooSaveOrder(ctx, o, true); err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_create", err.Error())
	}
	s.gooFire(ctx, "order_created", o)
	if aerr := s.gooV3OrderStatus(ctx, o, b, gooActorName(c)); aerr != nil {
		return nil, aerr
	}
	o, _ = s.gooGetOrder(ctx, o.ID)
	return s.gooV3OrderJSON(ctx, o), nil
}

func (s *Server) gooV3OrderUpdate(c *gin.Context, id int64, b map[string]any) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	o, aerr := s.gooV3LoadOrder(ctx, id)
	if aerr != nil {
		return nil, aerr
	}
	if aerr := s.gooV3ApplyOrder(ctx, o, b); aerr != nil {
		return nil, aerr
	}
	if err := s.gooSaveOrder(ctx, o, true); err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_edit", err.Error())
	}
	statusChange := false
	if v, ok := b["status"]; ok && strings.TrimPrefix(gooStr(v), "wc-") != o.Status {
		statusChange = true
	}
	if aerr := s.gooV3OrderStatus(ctx, o, b, gooActorName(c)); aerr != nil {
		return nil, aerr
	}
	o, _ = s.gooGetOrder(ctx, o.ID)
	if !statusChange && !gooBool(b["set_paid"]) {
		s.gooWebhookOrder(ctx, "order.updated", o)
	}
	s.gooPurgeOrder(ctx, o)
	return s.gooV3OrderJSON(ctx, o), nil
}

func (s *Server) gooV3OrderDelete(c *gin.Context, id int64, force bool) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	o, aerr := s.gooV3LoadOrder(ctx, id)
	if aerr != nil {
		return nil, aerr
	}
	obj := s.gooV3OrderJSON(ctx, o)
	if force {
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_order_items WHERE order_id IN (SELECT id FROM goo_orders WHERE id = $1 OR parent_id = $1)`, id)
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_order_notes WHERE order_id = $1`, id)
		if _, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_orders WHERE id = $1 OR (parent_id = $1 AND type = 'shop_order_refund')`, id); err != nil {
			return nil, gooErr(500, "woocommerce_rest_cannot_delete", err.Error())
		}
	} else {
		if o.Status == "trash" {
			return nil, gooErr(http.StatusGone, "woocommerce_rest_already_trashed", "The order has already been deleted.")
		}
		o.Meta["_trash_status"] = o.Status
		o.Status = "trash"
		if err := s.gooSaveOrder(ctx, o, false); err != nil {
			return nil, gooErr(500, "woocommerce_rest_cannot_delete", err.Error())
		}
		obj["status"] = "trash"
	}
	s.gooPurgeOrder(ctx, o)
	s.gooWebhookFire(ctx, "order.deleted", id, map[string]any{"id": id})
	return obj, nil
}

func (s *Server) gooV3OrdersList(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	page, per, offset := gooPage(c)
	q := gooOrderQuery{Search: c.Query("search"), Order: c.DefaultQuery("order", "desc"), Page: page, PerPage: per}
	if st := gooQueryList(c, "status"); len(st) > 0 && !containsStr(st, "any") {
		for i := range st {
			st[i] = strings.TrimPrefix(st[i], "wc-")
		}
		q.Status = st
	}
	guests := false
	if v := c.Query("customer"); v != "" {
		q.CustomerID = gooInt(v)
		guests = q.CustomerID == 0
	}
	gmt := gooBool(c.Query("dates_are_gmt"))
	if v := c.Query("after"); v != "" {
		q.After, _ = s.gooParseDate(v, gmt)
	}
	if v := c.Query("before"); v != "" {
		q.Before, _ = s.gooParseDate(v, gmt)
	}
	switch c.DefaultQuery("orderby", "date") {
	case "id", "include":
		q.OrderBy = "id"
	case "modified":
		q.OrderBy = "modified"
	default:
		q.OrderBy = "date"
	}
	extra := []string{}
	args := []any{}
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if guests {
		extra = append(extra, "customer_id = 0")
	}
	if pid := gooInt(c.Query("product")); pid > 0 {
		extra = append(extra, "id IN (SELECT order_id FROM goo_order_items WHERE product_id = "+add(pid)+" OR variation_id = "+add(pid)+")")
	}
	if inc := gooQueryIDs(c, "include"); len(inc) > 0 {
		extra = append(extra, "id = ANY("+add(inc)+")")
	}
	if exc := gooQueryIDs(c, "exclude"); len(exc) > 0 {
		extra = append(extra, "NOT id = ANY("+add(exc)+")")
	}
	if par := gooQueryIDs(c, "parent"); len(par) > 0 {
		extra = append(extra, "parent_id = ANY("+add(par)+")")
	}
	if v := c.Query("modified_after"); v != "" {
		if t, ok := s.gooParseDate(v, gmt); ok && t != nil {
			extra = append(extra, "date_modified > "+add(*t))
		}
	}
	if len(extra) > 0 {
		ids := []int64{-1}
		if rows, err := s.store.Pool.Query(ctx, `SELECT id FROM goo_orders WHERE type = 'shop_order' AND `+strings.Join(extra, " AND "), args...); err == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
		}
		q.IDs = ids
	}
	if offset >= 0 {
		q.Page, q.PerPage = 1, offset+per
	}
	list, total, err := s.gooQueryOrders(ctx, q)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	if offset >= 0 {
		if offset < len(list) {
			list = list[offset:]
		} else {
			list = nil
		}
	}
	paginate(c, total, per, page)
	out := make([]any, 0, len(list))
	for _, o := range list {
		out = append(out, s.gooV3OrderJSON(ctx, o))
	}
	restJSON(c, http.StatusOK, out)
}

// Notes ------------------------------------------------------------------------------------------------

func (s *Server) gooV3NoteJSON(ctx context.Context, n *gooOrderNote) map[string]any {
	author := n.AddedBy
	if author == "" {
		author = "Pursae Commerce"
	}
	return map[string]any{"id": n.ID, "author": author, "date_created": s.gooDate(&n.CreatedAt), "date_created_gmt": gooDateGMT(&n.CreatedAt),
		"note": n.Content, "customer_note": n.IsCustomer, "added_by_user": n.AddedBy != "",
		"_links": restLinks(s.gooRESTURL(ctx, gooV3NS+"/orders/"+itoa64(n.OrderID)+"/notes/"+itoa64(n.ID)), s.gooRESTURL(ctx, gooV3NS+"/orders/"+itoa64(n.OrderID)+"/notes"), nil)}
}

func (s *Server) gooV3NotesList(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	o, aerr := s.gooV3LoadOrder(ctx, gooInt(req.Params["order_id"]))
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	notes, _ := s.gooOrderNotes(ctx, o.ID, false)
	typ := c.DefaultQuery("type", "any")
	out := []any{}
	for _, n := range notes {
		if typ == "customer" && !n.IsCustomer || typ == "internal" && n.IsCustomer {
			continue
		}
		out = append(out, s.gooV3NoteJSON(ctx, n))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3NoteByID(ctx context.Context, orderID, id int64) (*gooOrderNote, *gooAPIErr) {
	notes, _ := s.gooOrderNotes(ctx, orderID, false)
	for _, n := range notes {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, gooErr(404, "woocommerce_rest_invalid_id", "Invalid resource ID.")
}

func (s *Server) gooV3NoteCreate(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	o, aerr := s.gooV3LoadOrder(ctx, gooInt(req.Params["order_id"]))
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	b := gooFormBody(c, req)
	note := wp.KsesPost(strings.TrimSpace(gooStr(b["note"])))
	if strings.TrimSpace(note) == "" {
		restErr(c, http.StatusBadRequest, "woocommerce_rest_shop_order_note_invalid", "Invalid note.")
		return
	}
	by := ""
	if gooBool(b["added_by_user"]) {
		by = gooActorName(c)
	}
	if err := s.gooAddOrderNote(ctx, o, note, gooBool(b["customer_note"]), by); err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_api_cannot_create_order_note", "Cannot create order note, please try again.")
		return
	}
	var id int64
	_ = s.store.Pool.QueryRow(ctx, `SELECT max(id) FROM goo_order_notes WHERE order_id = $1`, o.ID).Scan(&id)
	n, aerr := s.gooV3NoteByID(ctx, o.ID, id)
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	restJSON(c, http.StatusCreated, s.gooV3NoteJSON(ctx, n))
}

func (s *Server) gooV3NoteGet(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	n, aerr := s.gooV3NoteByID(ctx, gooInt(req.Params["order_id"]), req.ID())
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	restJSON(c, http.StatusOK, s.gooV3NoteJSON(ctx, n))
}

func (s *Server) gooV3NoteDelete(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	if force, _ := gooQueryBool(c, "force"); !force && !gooBool(req.Body["force"]) {
		restErr(c, http.StatusNotImplemented, "woocommerce_rest_trash_not_supported", "Webhooks do not support trashing.")
		return
	}
	n, aerr := s.gooV3NoteByID(ctx, gooInt(req.Params["order_id"]), req.ID())
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	obj := s.gooV3NoteJSON(ctx, n)
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_order_notes WHERE id = $1`, n.ID)
	restJSON(c, http.StatusOK, obj)
}

// Refunds ----------------------------------------------------------------------------------------------

func (s *Server) gooV3RefundJSON(ctx context.Context, r *gooOrder) map[string]any {
	lines := []any{}
	for _, it := range r.Items {
		price := 0.0
		if it.Quantity != 0 {
			price = it.Total / it.Quantity
		}
		lines = append(lines, map[string]any{"id": it.ID, "name": it.Name, "product_id": it.ProductID, "variation_id": it.VariationID,
			"quantity": it.Qty(), "tax_class": it.TaxClass, "subtotal": s.gooDec(it.Subtotal), "subtotal_tax": s.gooDec(it.SubtotalTax),
			"total": s.gooDec(it.Total), "total_tax": s.gooDec(it.TotalTax), "taxes": gooItemTaxesJSON(it.Taxes), "meta_data": s.gooItemMetaJSON(ctx, it),
			"refund_total": -it.Total, "price": price})
	}
	refunded, _ := r.Meta["_refunded_payment"].(bool)
	return map[string]any{"id": r.ID, "date_created": s.gooDate(&r.DateCreated), "date_created_gmt": gooDateGMT(&r.DateCreated),
		"amount": s.gooDec(-r.Total), "reason": r.RefundReason, "refunded_by": r.RefundedBy, "refunded_payment": refunded,
		"meta_data": gooMetaJSON(r.Meta), "line_items": lines,
		"_links": restLinks(s.gooRESTURL(ctx, gooV3NS+"/orders/"+itoa64(r.ParentID)+"/refunds/"+itoa64(r.ID)), s.gooRESTURL(ctx, gooV3NS+"/orders/"+itoa64(r.ParentID)+"/refunds"),
			map[string]any{"up": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/orders/"+itoa64(r.ParentID))}}})}
}

func (s *Server) gooV3RefundsList(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	o, aerr := s.gooV3LoadOrder(ctx, gooInt(req.Params["order_id"]))
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	list, _ := s.gooRefunds(ctx, o.ID)
	page := gooPaginateSlice(c, list)
	out := make([]any, 0, len(page))
	for _, r := range page {
		out = append(out, s.gooV3RefundJSON(ctx, r))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3AllRefunds(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	rows, err := s.store.Pool.Query(ctx, `SELECT `+gooOrderCols+` FROM goo_orders WHERE type = 'shop_order_refund' ORDER BY date_created DESC, id DESC`)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	var list []*gooOrder
	for rows.Next() {
		if r, err := gooScanOrder(rows); err == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	page := gooPaginateSlice(c, list)
	out := make([]any, 0, len(page))
	for _, r := range page {
		r.Items, _ = s.gooOrderItems(ctx, r.ID)
		out = append(out, s.gooV3RefundJSON(ctx, r))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3RefundByID(ctx context.Context, orderID, id int64) (*gooOrder, *gooAPIErr) {
	r, err := s.gooGetOrder(ctx, id)
	if err != nil || r.Type != "shop_order_refund" || r.ParentID != orderID {
		return nil, gooErr(404, "woocommerce_rest_invalid_id", "Invalid ID.")
	}
	return r, nil
}

func (s *Server) gooV3RefundGet(c *gin.Context, req *restRequest) {
	r, aerr := s.gooV3RefundByID(c.Request.Context(), gooInt(req.Params["order_id"]), req.ID())
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	restJSON(c, http.StatusOK, s.gooV3RefundJSON(c.Request.Context(), r))
}

func (s *Server) gooV3RefundDelete(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	if force, _ := gooQueryBool(c, "force"); !force && !gooBool(req.Body["force"]) {
		restErr(c, http.StatusNotImplemented, "woocommerce_rest_trash_not_supported", "Refunds do not support trashing.")
		return
	}
	r, aerr := s.gooV3RefundByID(ctx, gooInt(req.Params["order_id"]), req.ID())
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	obj := s.gooV3RefundJSON(ctx, r)
	if err := s.gooDeleteRefund(ctx, r.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_delete", err.Error())
		return
	}
	restJSON(c, http.StatusOK, obj)
}

// gooV3RefundCreate is POST /orders/{id}/refunds: {amount, reason, line_items[{id, quantity,
// refund_total, refund_tax[{id, refund_total}]}], api_refund, api_restock}.
func (s *Server) gooV3RefundCreate(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	o, aerr := s.gooV3LoadOrder(ctx, gooInt(req.Params["order_id"]))
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	b := gooFormBody(c, req)
	var lines []gooRefundLine
	lineSum := 0.0
	for _, x := range gooList(b["line_items"]) {
		m := gooObj(x)
		l := gooRefundLine{ItemID: gooInt(m["id"]), Qty: int(gooInt(m["quantity"])), TaxTotal: map[string]float64{}}
		l.Total, _ = gooFloat(m["refund_total"])
		for _, t := range gooList(m["refund_tax"]) {
			tm := gooObj(t)
			if v, ok := gooFloat(tm["refund_total"]); ok && v != 0 {
				l.TaxTotal[gooStr(tm["id"])] = v
				lineSum += v
			}
		}
		lineSum += l.Total
		lines = append(lines, l)
	}
	amount, ok := gooFloat(b["amount"])
	if !ok {
		amount = lineSum
	}
	if amount <= 0 {
		restErr(c, http.StatusBadRequest, "woocommerce_rest_invalid_order_refund", "Refund amount must be greater than zero.")
		return
	}
	apiRefund := true
	if v, ok := b["api_refund"]; ok {
		apiRefund = gooBool(v)
	}
	restock := true
	if v, ok := b["api_restock"]; ok {
		restock = gooBool(v)
	}
	by := gooActorID(c)
	if v, ok := b["refunded_by"]; ok {
		by = gooInt(v)
	}
	r, err := s.gooCreateRefund(ctx, o, amount, gooStr(b["reason"]), lines, restock, apiRefund, by)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_create_order_refund", err.Error())
		return
	}
	if v, ok := b["meta_data"]; ok {
		gooApplyMeta(r.Meta, v)
		_ = s.gooSaveOrder(ctx, r, false)
	}
	r, _ = s.gooGetOrder(ctx, r.ID)
	restJSON(c, http.StatusCreated, s.gooV3RefundJSON(ctx, r))
}

// Coupons ------------------------------------------------------------------------------------------------

func gooOptFloat(v *float64) string {
	if v == nil {
		return "0.00"
	}
	return gooFormatDecimal(*v, 2)
}

func gooOptIntJSON(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func (s *Server) gooV3CouponJSON(ctx context.Context, cp *gooCoupon) map[string]any {
	return map[string]any{"id": cp.ID, "code": cp.Code, "amount": gooFormatDecimal(cp.Amount, 2), "status": cp.Status,
		"date_created": s.gooDate(&cp.DateCreated), "date_created_gmt": gooDateGMT(&cp.DateCreated),
		"date_modified": s.gooDate(&cp.DateModified), "date_modified_gmt": gooDateGMT(&cp.DateModified),
		"discount_type": cp.DiscountType, "description": cp.Description, "date_expires": s.gooDate(cp.DateExpires), "date_expires_gmt": gooDateGMT(cp.DateExpires),
		"usage_count": cp.UsageCount, "individual_use": cp.IndividualUse, "product_ids": nonNilSlice(cp.ProductIDs),
		"excluded_product_ids": nonNilSlice(cp.ExcludedProductIDs), "usage_limit": gooOptIntJSON(cp.UsageLimit),
		"usage_limit_per_user": gooOptIntJSON(cp.UsageLimitPerUser), "limit_usage_to_x_items": gooOptIntJSON(cp.LimitUsageToXItems),
		"free_shipping": cp.FreeShipping, "product_categories": nonNilSlice(cp.ProductCategories),
		"excluded_product_categories": nonNilSlice(cp.ExcludedProductCategories), "exclude_sale_items": cp.ExcludeSaleItems,
		"minimum_amount": gooOptFloat(cp.MinimumAmount), "maximum_amount": gooOptFloat(cp.MaximumAmount),
		"email_restrictions": nonNilSlice(cp.EmailRestrictions), "used_by": nonNilSlice(cp.UsedBy), "meta_data": gooMetaJSON(cp.Meta),
		"_links": s.gooLinks(ctx, "coupons", cp.ID)}
}

func gooOptIntFrom(v any) *int {
	if v == nil || gooStr(v) == "" {
		return nil
	}
	n := int(gooInt(v))
	if n <= 0 {
		return nil
	}
	return &n
}

func (s *Server) gooV3ApplyCoupon(cp *gooCoupon, b map[string]any) *gooAPIErr {
	if v, ok := b["code"]; ok {
		cp.Code = gooStr(v)
	}
	if v, ok := b["amount"]; ok {
		cp.Amount, _ = gooFloat(v)
	}
	if v, ok := b["status"]; ok {
		cp.Status = gooStr(v)
	}
	if v, ok := b["discount_type"]; ok {
		t := gooStr(v)
		if t != "percent" && t != "fixed_cart" && t != "fixed_product" {
			return gooErr(400, "rest_invalid_param", "Invalid parameter(s): discount_type")
		}
		cp.DiscountType = t
	}
	if v, ok := b["description"]; ok {
		cp.Description = gooStr(v)
	}
	for _, k := range []string{"date_expires_gmt", "date_expires"} {
		if v, ok := b[k]; ok {
			if v == nil || gooStr(v) == "" {
				cp.DateExpires = nil
			} else if t, ok := s.gooParseDate(gooStr(v), k == "date_expires_gmt"); ok {
				cp.DateExpires = t
			} else {
				return gooErr(400, "rest_invalid_param", "Invalid parameter(s): "+k)
			}
			break
		}
	}
	bools := map[string]*bool{"individual_use": &cp.IndividualUse, "free_shipping": &cp.FreeShipping, "exclude_sale_items": &cp.ExcludeSaleItems}
	for k, dst := range bools {
		if v, ok := b[k]; ok {
			*dst = gooBool(v)
		}
	}
	ids := map[string]*[]int64{"product_ids": &cp.ProductIDs, "excluded_product_ids": &cp.ExcludedProductIDs,
		"product_categories": &cp.ProductCategories, "excluded_product_categories": &cp.ExcludedProductCategories}
	for k, dst := range ids {
		if v, ok := b[k]; ok {
			*dst = gooIDList(v)
		}
	}
	for k, dst := range map[string]**int{"usage_limit": &cp.UsageLimit, "usage_limit_per_user": &cp.UsageLimitPerUser, "limit_usage_to_x_items": &cp.LimitUsageToXItems} {
		if v, ok := b[k]; ok {
			*dst = gooOptIntFrom(v)
		}
	}
	for k, dst := range map[string]**float64{"minimum_amount": &cp.MinimumAmount, "maximum_amount": &cp.MaximumAmount} {
		if v, ok := b[k]; ok {
			*dst = gooRESTAmount(v)
			if *dst != nil && **dst == 0 {
				*dst = nil
			}
		}
	}
	if v, ok := b["email_restrictions"]; ok {
		cp.EmailRestrictions = nil
		for _, x := range gooList(v) {
			if e := strings.ToLower(strings.TrimSpace(gooStr(x))); e != "" {
				cp.EmailRestrictions = append(cp.EmailRestrictions, e)
			}
		}
	}
	if v, ok := b["meta_data"]; ok {
		gooApplyMeta(cp.Meta, v)
	}
	return nil
}

func (s *Server) gooV3CouponCreate(c *gin.Context, b map[string]any) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	cp := &gooCoupon{Status: "publish", DiscountType: "fixed_cart", Meta: map[string]any{}}
	if strings.TrimSpace(gooStr(b["code"])) == "" {
		return nil, gooErr(400, "rest_missing_callback_param", "Missing parameter(s): code")
	}
	if aerr := s.gooV3ApplyCoupon(cp, b); aerr != nil {
		return nil, aerr
	}
	if err := s.gooSaveCoupon(ctx, cp); err != nil {
		return nil, gooErr(400, "woocommerce_rest_coupon_code_already_exists", "The coupon code already exists")
	}
	cp, _ = s.gooCouponByID(ctx, cp.ID)
	obj := s.gooV3CouponJSON(ctx, cp)
	s.gooWebhookFire(ctx, "coupon.created", cp.ID, obj)
	return obj, nil
}

func (s *Server) gooV3CouponUpdate(c *gin.Context, id int64, b map[string]any) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	cp, err := s.gooCouponByID(ctx, id)
	if err != nil {
		return nil, gooErr(404, "woocommerce_rest_shop_coupon_invalid_id", "Invalid ID.")
	}
	if aerr := s.gooV3ApplyCoupon(cp, b); aerr != nil {
		return nil, aerr
	}
	if err := s.gooSaveCoupon(ctx, cp); err != nil {
		return nil, gooErr(400, "woocommerce_rest_coupon_code_already_exists", "The coupon code already exists")
	}
	cp, _ = s.gooCouponByID(ctx, cp.ID)
	obj := s.gooV3CouponJSON(ctx, cp)
	s.gooWebhookFire(ctx, "coupon.updated", cp.ID, obj)
	return obj, nil
}

func (s *Server) gooV3CouponDelete(c *gin.Context, id int64, force bool) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	cp, err := s.gooCouponByID(ctx, id)
	if err != nil {
		return nil, gooErr(404, "woocommerce_rest_shop_coupon_invalid_id", "Invalid ID.")
	}
	obj := s.gooV3CouponJSON(ctx, cp)
	if force {
		if _, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_coupons WHERE id = $1`, id); err != nil {
			return nil, gooErr(500, "woocommerce_rest_cannot_delete", err.Error())
		}
	} else {
		if cp.Status == "trash" {
			return nil, gooErr(http.StatusGone, "woocommerce_rest_already_trashed", "The coupon has already been deleted.")
		}
		cp.Meta["_wp_trash_meta_status"] = cp.Status
		cp.Status = "trash"
		_ = s.gooSaveCoupon(ctx, cp)
		obj["status"] = "trash"
	}
	s.gooWebhookFire(ctx, "coupon.deleted", id, map[string]any{"id": id})
	return obj, nil
}

func (s *Server) gooV3CouponsList(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	status := c.Query("status")
	if status == "any" {
		status = ""
	}
	list, _, err := s.gooQueryCoupons(ctx, status, c.Query("search"), 1, 100000)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	code := strings.ToLower(c.Query("code"))
	inc, exc := gooQueryIDs(c, "include"), gooQueryIDs(c, "exclude")
	var keep []*gooCoupon
	for _, cp := range list {
		if code != "" && cp.Code != code || len(inc) > 0 && !gooInt64In(cp.ID, inc) || gooInt64In(cp.ID, exc) {
			continue
		}
		keep = append(keep, cp)
	}
	if c.Query("orderby") == "id" || c.Query("order") == "asc" {
		sort.SliceStable(keep, func(i, j int) bool {
			if c.Query("orderby") == "id" {
				if strings.EqualFold(c.Query("order"), "asc") {
					return keep[i].ID < keep[j].ID
				}
				return keep[i].ID > keep[j].ID
			}
			return keep[i].DateCreated.Before(keep[j].DateCreated)
		})
	}
	pageList := gooPaginateSlice(c, keep)
	out := make([]any, 0, len(pageList))
	for _, cp := range pageList {
		out = append(out, s.gooV3CouponJSON(ctx, cp))
	}
	restJSON(c, http.StatusOK, out)
}

// Customers ------------------------------------------------------------------------------------------------

// userLevel is the WordPress user level (level_10 … level_0) implied by the user's capabilities.
func userLevel(u *store.User) int {
	if u == nil {
		return 0
	}
	for n := 10; n >= 0; n-- {
		if u.Can("level_"+strconv.Itoa(n), nil) {
			return n
		}
	}
	return 0
}

// gooCustomerCredentialAllowed reports whether actor may set target's password or e-mail, or delete
// the account. A scoped application password or OAuth grant needs the Users scope, and beyond the
// caller's own account it reaches only level-0 accounts (customers, subscribers), which is what shop
// integrations manage: any account with a role above that, an administrator above all, needs full
// access, so a scoped credential cannot take over staff accounts. Changing someone else also requires
// a role at least as high as the target's.
func gooCustomerCredentialAllowed(actor, target *store.User, scopes []string) bool {
	if actor == nil || target == nil {
		return false
	}
	if len(scopes) > 0 {
		if !slices.Contains(scopes, "users") {
			return false
		}
		if actor.ID != target.ID && userLevel(target) > 0 {
			return false
		}
	}
	if actor.ID == target.ID {
		return true
	}
	return userLevel(actor) >= userLevel(target)
}

// gooProtectedUserMetaKey reports whether k is user meta that core keeps as sign-in, security or
// account state (reset keys, Pursae Security, sessions, capabilities, forum verification and Google
// link, Web Builder trust). wc/v3 customers neither write nor show it.
func gooProtectedUserMetaKey(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	if k == "" || strings.HasPrefix(k, "_") {
		return true
	}
	switch k {
	case "password_reset", forumUMetaUnverified, forumUMetaVerify, forumUMetaGoogleSub, forumUMetaGoogleEmail,
		forumUMetaNoPassword, forumUMetaEmailVerified, "user_level", "primary_blog":
		return true
	}
	if _, ok := cliUserSecurityMetaKeys[k]; ok {
		return true
	}
	for _, p := range []string{"adminsec_", "session", "app_password", "application_password", "wp_"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return strings.Contains(k, "capabilit") || strings.Contains(k, "user_level") || strings.Contains(k, "password") ||
		strings.Contains(k, "2fa") || strings.Contains(k, "totp") || strings.Contains(k, "passkey")
}

// gooCustomerStats are a customer's paid orders count and total spent.
func (s *Server) gooCustomerStats(ctx context.Context, userID int64) (count int, spent float64) {
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(total),0)::float8 FROM goo_orders WHERE type = 'shop_order' AND customer_id = $1
		AND status IN ('processing','completed')`, userID).Scan(&count, &spent)
	return
}

func (s *Server) gooV3CustomerJSON(ctx context.Context, u *store.User) map[string]any {
	billing, shipping := map[string]string{}, map[string]string{}
	for _, k := range gooAddressKeys {
		if v, ok := u.Meta["billing_"+k].(string); ok {
			billing[k] = v
		}
		if v, ok := u.Meta["shipping_"+k].(string); ok {
			shipping[k] = v
		}
	}
	count, spent := s.gooCustomerStats(ctx, u.ID)
	var lastActive *time.Time
	if f, ok := u.Meta["_goo_last_active"].(float64); ok && f > 0 {
		t := time.Unix(int64(f), 0)
		lastActive = &t
	}
	modified := u.RegisteredAt
	if lastActive != nil {
		modified = *lastActive
	}
	meta := map[string]any{}
	for k, v := range u.Meta {
		if !strings.HasPrefix(k, "billing_") && !strings.HasPrefix(k, "shipping_") && !gooProtectedUserMetaKey(k) {
			meta[k] = v
		}
	}
	role := u.PrimaryRole()
	return map[string]any{"id": u.ID, "date_created": s.gooDate(&u.RegisteredAt), "date_created_gmt": gooDateGMT(&u.RegisteredAt),
		"date_modified": s.gooDate(&modified), "date_modified_gmt": gooDateGMT(&modified), "email": u.Email,
		"first_name": u.Profile["first_name"], "last_name": u.Profile["last_name"], "role": role, "username": u.Login,
		"billing": gooAddressJSON(billing, true), "shipping": gooAddressJSON(shipping, false), "is_paying_customer": count > 0,
		"orders_count": count, "total_spent": s.gooDec(spent), "avatar_url": s.avatarFor(ctx, u.Email, 96), "meta_data": gooMetaJSON(meta),
		"_links": s.gooLinks(ctx, "customers", u.ID)}
}

func (s *Server) gooV3ApplyCustomer(ctx context.Context, u *store.User, b map[string]any) (map[string]any, *gooAPIErr) {
	meta := map[string]any{}
	if v, ok := b["first_name"]; ok {
		u.Profile["first_name"] = gooStr(v)
	}
	if v, ok := b["last_name"]; ok {
		u.Profile["last_name"] = gooStr(v)
	}
	for _, typ := range []string{"billing", "shipping"} {
		addr := gooObj(b[typ])
		for k, v := range addr {
			if containsStr(gooAddressKeys, k) {
				meta[typ+"_"+k] = strings.TrimSpace(gooStr(v))
			}
		}
	}
	for _, x := range gooList(b["meta_data"]) {
		m := gooObj(x)
		if k := gooStr(m["key"]); !gooProtectedUserMetaKey(k) { // security state is never REST meta
			meta[k] = m["value"]
		}
	}
	return meta, nil
}

func (s *Server) gooV3CustomerCreate(c *gin.Context, b map[string]any) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	email := strings.TrimSpace(gooStr(b["email"]))
	if email == "" {
		return nil, gooErr(400, "rest_missing_callback_param", "Missing parameter(s): email")
	}
	first, last := gooStr(b["first_name"]), gooStr(b["last_name"])
	if bl := gooObj(b["billing"]); first == "" && bl != nil {
		first, last = gooStr(bl["first_name"]), gooStr(bl["last_name"])
	}
	id, err := s.gooCreateCustomer(ctx, email, strings.TrimSpace(gooStr(b["username"])), gooStr(b["password"]),
		map[string]string{"billing_first_name": first, "billing_last_name": last})
	if err != nil {
		code := "woocommerce_rest_customer_invalid"
		if strings.Contains(err.Error(), "already registered") {
			code = "registration-error-email-exists"
		}
		return nil, gooErr(400, code, gooPlainMsg(err.Error()))
	}
	u, err := s.store.UserByID(ctx, id)
	if err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_create", err.Error())
	}
	if u.Profile == nil {
		u.Profile = map[string]string{}
	}
	meta, aerr := s.gooV3ApplyCustomer(ctx, u, b)
	if aerr != nil {
		return nil, aerr
	}
	_ = s.store.UpdateUserProfile(ctx, u)
	if len(meta) > 0 {
		_ = s.gooSetUserMeta(ctx, id, meta)
	}
	u, _ = s.store.UserByID(ctx, id)
	return s.gooV3CustomerJSON(c, u), nil
}

func (s *Server) gooV3CustomerUpdate(c *gin.Context, id int64, b map[string]any) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	u, err := s.store.UserByID(ctx, id)
	if err != nil {
		return nil, gooErr(400, "woocommerce_rest_invalid_id", "Invalid resource ID.")
	}
	if u.Profile == nil {
		u.Profile = map[string]string{}
	}
	if v, ok := b["password"]; ok && gooStr(v) != "" { // Pursae Security password policy, before anything is saved
		if !gooCustomerCredentialAllowed(gooV3Actor(c), u, restScopes(c)) {
			return nil, gooErr(http.StatusForbidden, "woocommerce_rest_cannot_edit", "Sorry, you are not allowed to change this user's password.")
		}
		if msg := s.adminsecUserPasswordProblem(u, gooStr(v)); msg != "" {
			return nil, gooErr(400, "woocommerce_rest_customer_invalid_password", msg)
		}
	}
	if v, ok := b["email"]; ok {
		email := strings.TrimSpace(gooStr(v))
		if !strings.EqualFold(email, u.Email) && !gooCustomerCredentialAllowed(gooV3Actor(c), u, restScopes(c)) {
			return nil, gooErr(http.StatusForbidden, "woocommerce_rest_cannot_edit", "Sorry, you are not allowed to change this user's email address.")
		}
		if _, err := mail.ParseAddress(email); err != nil {
			return nil, gooErr(400, "woocommerce_rest_invalid_email", "Invalid email address.")
		}
		if other, err := s.store.UserByLoginOrEmail(ctx, email); err == nil && other.ID != u.ID {
			return nil, gooErr(400, "woocommerce_rest_customer_invalid_email", "An account is already registered with your email address.")
		}
		u.Email = email
		if err := s.store.UpdateUserFields(ctx, u); err != nil {
			return nil, gooErr(500, "woocommerce_rest_cannot_edit", err.Error())
		}
	}
	if v, ok := b["username"]; ok && gooStr(v) != u.Login {
		return nil, gooErr(400, "woocommerce_rest_customer_invalid_argument", "Username isn't editable.")
	}
	if v, ok := b["password"]; ok && gooStr(v) != "" {
		hash, err := auth.HashPassword(gooStr(v))
		if err == nil {
			if s.store.SetUserPassword(ctx, u.ID, hash) == nil {
				s.adminsecPasswordChanged(c, u)
				s.endSessionsAfterPasswordChange(ctx, nil, u.ID) // the API caller is never the customer's own browser session
			}
		}
	}
	meta, aerr := s.gooV3ApplyCustomer(ctx, u, b)
	if aerr != nil {
		return nil, aerr
	}
	_ = s.store.UpdateUserProfile(ctx, u)
	if len(meta) > 0 {
		_ = s.gooSetUserMeta(ctx, u.ID, meta)
	}
	u, _ = s.store.UserByID(ctx, id)
	obj := s.gooV3CustomerJSON(c, u)
	s.gooWebhookFire(ctx, "customer.updated", u.ID, obj)
	return obj, nil
}

func (s *Server) gooV3CustomerDelete(c *gin.Context, id int64, force bool) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	if !force {
		return nil, gooErr(http.StatusNotImplemented, "woocommerce_rest_trash_not_supported", "Customers do not support trashing.")
	}
	u, err := s.store.UserByID(ctx, id)
	if err != nil {
		return nil, gooErr(400, "woocommerce_rest_invalid_id", "Invalid resource id.")
	}
	if !gooCustomerCredentialAllowed(gooV3Actor(c), u, restScopes(c)) {
		return nil, gooErr(http.StatusForbidden, "woocommerce_rest_cannot_delete", "Sorry, you are not allowed to delete this user.")
	}
	obj := s.gooV3CustomerJSON(c, u)
	reassign := gooInt(c.Query("reassign"))
	if err := s.store.DeleteUser(ctx, id, reassign); err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_delete", "The resource cannot be deleted.")
	}
	s.gooWebhookFire(ctx, "customer.deleted", id, map[string]any{"id": id})
	return obj, nil
}

func (s *Server) gooV3CustomersList(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	page, per, _ := gooPage(c)
	q := store.UserQuery{Search: c.Query("search"), Page: page, PerPage: per, IDs: gooQueryIDs(c, "include"), Exclude: gooQueryIDs(c, "exclude")}
	switch role := c.DefaultQuery("role", "customer"); role {
	case "all":
	default:
		q.Role = role
	}
	switch c.DefaultQuery("orderby", "name") {
	case "id":
		q.OrderBy = "id"
	case "registered_date":
		q.OrderBy = "registered"
	case "include":
		q.OrderBy = "id"
	default:
		q.OrderBy = "name"
	}
	q.Order = c.DefaultQuery("order", "asc")
	if email := c.Query("email"); email != "" {
		q.Search = email
	}
	list, total, err := s.store.QueryUsers(ctx, q)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	paginate(c, total, per, page)
	out := make([]any, 0, len(list))
	for _, u := range list {
		if email := c.Query("email"); email != "" && !strings.EqualFold(u.Email, email) {
			continue
		}
		out = append(out, s.gooV3CustomerJSON(c, u))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3CustomerDownloads(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	rows, err := s.store.Pool.Query(ctx, `SELECT d.download_id, d.product_id, d.order_id, d.order_key, coalesce(d.user_email, ''), coalesce(d.downloads_remaining::text, ''), d.access_expires
		FROM goo_download_permissions d WHERE d.user_id = $1 ORDER BY d.order_id`, req.ID())
	if err != nil {
		restJSON(c, http.StatusOK, []any{})
		return
	}
	type row struct {
		dlID, key, email, remaining string
		pid, oid                    int64
		expires                     *time.Time
	}
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.dlID, &r.pid, &r.oid, &r.key, &r.email, &r.remaining, &r.expires) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	out := []any{}
	for _, r := range list {
		name, fileName, file := "", "", ""
		if p, err := s.gooGetProduct(ctx, r.pid); err == nil {
			name = s.gooProductTitle(ctx, p)
			for _, d := range p.Downloads {
				if d.ID == r.dlID {
					fileName, file = d.Name, d.File
				}
			}
		}
		remaining := any("unlimited")
		if r.remaining != "" {
			remaining = r.remaining
		}
		expires := any("never")
		if r.expires != nil {
			expires = s.gooDate(r.expires)
		}
		out = append(out, map[string]any{"download_id": r.dlID, "download_url": s.links(ctx).HomeURL("/?download_file=" + itoa64(r.pid) + "&order=" + urlEscape(r.key) + "&email=" + urlEscape(r.email) + "&key=" + urlEscape(r.dlID)),
			"product_id": r.pid, "product_name": name, "download_name": fileName, "order_id": r.oid, "order_key": r.key,
			"downloads_remaining": remaining, "access_expires": expires, "access_expires_gmt": expires, "file": map[string]any{"name": fileName, "file": file}})
	}
	restJSON(c, http.StatusOK, out)
}
