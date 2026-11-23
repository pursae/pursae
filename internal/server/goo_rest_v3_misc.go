package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// wc/v3 reports, taxes (+ classes), settings, shipping zones (+ locations, methods), shipping
// methods, payment gateways, system status and data (countries, currencies, continents).

func (s *Server) registerGooV3Misc() {
	s.registerGooV3Webhooks()      // goo_rest_v3_webhooks.go
	s.registerGooV3Subscriptions() // goo_rest_v3_subscriptions.go
	s.registerGooBookingsREST()    // goo_rest_bookings.go
	s.registerGooGiftCardsREST()   // goo_ext_gift_cards_rest.go
	reports := gooCapsAll("view_woocommerce_reports")
	s.gooV3("/reports", reports, map[string]restHandler{http.MethodGet: s.gooV3Reports})
	s.gooV3("/reports/sales", reports, map[string]restHandler{http.MethodGet: s.gooV3ReportSales})
	s.gooV3("/reports/top_sellers", reports, map[string]restHandler{http.MethodGet: s.gooV3ReportTopSellers})
	s.gooV3("/reports/orders/totals", reports, map[string]restHandler{http.MethodGet: s.gooV3ReportOrdersTotals})
	s.gooV3("/reports/products/totals", reports, map[string]restHandler{http.MethodGet: s.gooV3ReportProductsTotals})
	s.gooV3("/reports/customers/totals", reports, map[string]restHandler{http.MethodGet: s.gooV3ReportCustomersTotals})
	s.gooV3("/reports/coupons/totals", reports, map[string]restHandler{http.MethodGet: s.gooV3ReportCouponsTotals})
	s.gooV3("/reports/reviews/totals", reports, map[string]restHandler{http.MethodGet: s.gooV3ReportReviewsTotals})

	mw := gooCapsAll("manage_woocommerce")
	s.gooV3("/taxes", mw, map[string]restHandler{http.MethodGet: s.gooV3TaxesList, http.MethodPost: s.gooV3Create(s.gooV3TaxCreate)})
	s.gooV3("/taxes/batch", mw, map[string]restHandler{http.MethodPost: func(c *gin.Context, req *restRequest) {
		s.gooBatch(c, req, gooBatchOps{Create: s.gooV3TaxCreate, Update: s.gooV3TaxUpdate, Delete: s.gooV3TaxDelete})
	}})
	s.gooV3("/taxes/(?P<id>[\\d]+)", mw, map[string]restHandler{
		http.MethodGet: s.gooV3Get(func(c *gin.Context, id int64) (map[string]any, *gooAPIErr) {
			r := s.gooTaxRateByID(c.Request.Context(), id)
			if r == nil {
				return nil, gooErr(404, "woocommerce_rest_invalid_id", "Invalid resource ID.")
			}
			return s.gooV3TaxJSON(c.Request.Context(), r), nil
		}),
		http.MethodPut:    s.gooV3Update(s.gooV3TaxUpdate),
		http.MethodDelete: s.gooV3Delete(s.gooV3TaxDelete),
	})
	s.gooV3("/taxes/classes", mw, map[string]restHandler{http.MethodGet: s.gooV3TaxClasses, http.MethodPost: s.gooV3TaxClassCreate})
	s.gooV3("/taxes/classes/(?P<slug>[\\w-]+)", mw, map[string]restHandler{http.MethodGet: s.gooV3TaxClassRates, http.MethodDelete: s.gooV3TaxClassDelete})

	s.gooV3("/settings", mw, map[string]restHandler{http.MethodGet: s.gooV3SettingGroups})
	s.gooV3("/settings/batch", mw, map[string]restHandler{http.MethodPost: s.gooV3SettingsBatchAll})
	s.gooV3("/settings/(?P<group>[\\w-]+)", mw, map[string]restHandler{http.MethodGet: s.gooV3SettingsGroup})
	s.gooV3("/settings/(?P<group>[\\w-]+)/batch", mw, map[string]restHandler{http.MethodPost: s.gooV3SettingsBatch})
	s.gooV3("/settings/(?P<group>[\\w-]+)/(?P<id>[\\w-]+)", mw, map[string]restHandler{http.MethodGet: s.gooV3SettingGet, http.MethodPut: s.gooV3SettingPut})

	s.gooV3("/shipping/zones", mw, map[string]restHandler{http.MethodGet: s.gooV3ZonesList, http.MethodPost: s.gooV3Create(s.gooV3ZoneCreate)})
	s.gooV3("/shipping/zones/(?P<id>[\\d]+)", mw, map[string]restHandler{
		http.MethodGet: s.gooV3Get(func(c *gin.Context, id int64) (map[string]any, *gooAPIErr) {
			z, err := s.gooZone(c.Request.Context(), id)
			if err != nil {
				return nil, gooErr(404, "woocommerce_rest_shipping_zone_invalid", "Resource does not exist.")
			}
			return s.gooV3ZoneJSON(c.Request.Context(), z), nil
		}),
		http.MethodPut:    s.gooV3Update(s.gooV3ZoneUpdate),
		http.MethodDelete: s.gooV3Delete(s.gooV3ZoneDelete),
	})
	s.gooV3("/shipping/zones/(?P<id>[\\d]+)/locations", mw, map[string]restHandler{http.MethodGet: s.gooV3ZoneLocations, http.MethodPut: s.gooV3ZoneLocationsPut})
	s.gooV3("/shipping/zones/(?P<zone_id>[\\d]+)/methods", mw, map[string]restHandler{http.MethodGet: s.gooV3ZoneMethods, http.MethodPost: s.gooV3ZoneMethodCreate})
	s.gooV3("/shipping/zones/(?P<zone_id>[\\d]+)/methods/(?P<id>[\\d]+)", mw, map[string]restHandler{
		http.MethodGet: s.gooV3ZoneMethodGet, http.MethodPut: s.gooV3ZoneMethodUpdate, http.MethodDelete: s.gooV3ZoneMethodDelete})
	s.gooV3("/shipping_methods", mw, map[string]restHandler{http.MethodGet: s.gooV3ShippingMethods})
	s.gooV3("/shipping_methods/(?P<id>[\\w-]+)", mw, map[string]restHandler{http.MethodGet: s.gooV3ShippingMethod})

	s.gooV3("/payment_gateways", mw, map[string]restHandler{http.MethodGet: s.gooV3Gateways})
	s.gooV3("/payment_gateways/(?P<id>[\\w-]+)", mw, map[string]restHandler{http.MethodGet: s.gooV3Gateway, http.MethodPut: s.gooV3GatewayUpdate})

	s.gooV3("/system_status", mw, map[string]restHandler{http.MethodGet: s.gooV3SystemStatus})
	s.gooV3("/system_status/tools", mw, map[string]restHandler{http.MethodGet: s.gooV3SystemTools})

	data := gooCapsAll("manage_woocommerce")
	s.gooV3("/data", data, map[string]restHandler{http.MethodGet: s.gooV3Data})
	s.gooV3("/data/continents", data, map[string]restHandler{http.MethodGet: s.gooV3Continents})
	s.gooV3("/data/continents/(?P<code>[\\w]+)", data, map[string]restHandler{http.MethodGet: s.gooV3Continent})
	s.gooV3("/data/countries", data, map[string]restHandler{http.MethodGet: s.gooV3Countries})
	s.gooV3("/data/countries/(?P<code>[\\w]+)", data, map[string]restHandler{http.MethodGet: s.gooV3Country})
	s.gooV3("/data/currencies", data, map[string]restHandler{http.MethodGet: s.gooV3Currencies})
	s.gooV3("/data/currencies/current", data, map[string]restHandler{http.MethodGet: s.gooV3CurrencyCurrent})
	s.gooV3("/data/currencies/(?P<code>[\\w]+)", data, map[string]restHandler{http.MethodGet: s.gooV3Currency})
}

// Reports ------------------------------------------------------------------------------------------------

func (s *Server) gooV3Reports(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	out := []any{}
	for _, r := range []struct{ Slug, Desc string }{{"sales", "List of sales reports."}, {"top_sellers", "List of top sellers products."},
		{"orders/totals", "Orders totals."}, {"products/totals", "Products totals."}, {"customers/totals", "Customers totals."},
		{"coupons/totals", "Coupons totals."}, {"reviews/totals", "Reviews totals."}} {
		out = append(out, map[string]any{"slug": r.Slug, "description": r.Desc, "_links": map[string]any{"self": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/reports/"+r.Slug)}}}})
	}
	restJSON(c, http.StatusOK, out)
}

// gooRESTReportRange is the period of a report: period=week|month|last_month|year or date_min / date_max.
func (s *Server) gooRESTReportRange(c *gin.Context) (from, to time.Time) {
	loc := s.loc()
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	from, to = today.AddDate(0, 0, -6), today.AddDate(0, 0, 1)
	switch c.Query("period") {
	case "month":
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	case "last_month":
		to = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		from = to.AddDate(0, -1, 0)
	case "year":
		from = time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc)
	}
	if v := c.Query("date_min"); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, loc); err == nil {
			from = t
		}
	}
	if v := c.Query("date_max"); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, loc); err == nil {
			to = t.AddDate(0, 0, 1)
		}
	}
	return
}

var gooReportStatuses = []string{"completed", "processing", "on-hold", "refunded"}

func (s *Server) gooV3ReportSales(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	from, to := s.gooRESTReportRange(c)
	type day struct {
		Sales, Tax, Shipping, Discount float64
		Orders, Items, Customers       int
	}
	days := map[string]*day{}
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		days[d.Format("2006-01-02")] = &day{}
	}
	var sales, tax, shipping, discount, refunds float64
	var items, nOrders int
	// One aggregate per day of the site time zone: a year of orders is summed by the database instead of
	// being loaded with every item and refund.
	rows, err := s.store.Pool.Query(ctx, `SELECT to_char(o.date_created AT TIME ZONE $4, 'YYYY-MM-DD'), count(*),
		coalesce(sum(o.total), 0)::float8, coalesce(sum(o.total_tax), 0)::float8, coalesce(sum(o.shipping_total), 0)::float8,
		coalesce(sum(o.discount_total), 0)::float8,
		coalesce(sum((SELECT sum(trunc(i.quantity)) FROM goo_order_items i WHERE i.order_id = o.id AND i.type = 'line_item')), 0)::bigint,
		coalesce(sum((SELECT sum(-r.total) FROM goo_orders r WHERE r.parent_id = o.id AND r.type = 'shop_order_refund')), 0)::float8
		FROM goo_orders o WHERE o.type = 'shop_order' AND o.status = ANY($1) AND o.date_created >= $2 AND o.date_created < $3
		GROUP BY 1`, gooReportStatuses, from, to, store.PGZone(s.loc()))
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	for rows.Next() {
		var k string
		var r day
		var n, qty int64
		var refunded float64
		if err := rows.Scan(&k, &n, &r.Sales, &r.Tax, &r.Shipping, &r.Discount, &qty, &refunded); err != nil {
			rows.Close()
			restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
			return
		}
		r.Orders, r.Items = int(n), int(qty)
		days[k] = &r
		sales += r.Sales
		tax += r.Tax
		shipping += r.Shipping
		discount += r.Discount
		items += r.Items
		nOrders += r.Orders
		refunds += refunded
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	var newCustomers int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM users u JOIN user_roles ur ON ur.user_id = u.id JOIN roles r ON r.id = ur.role_id
		WHERE r.slug = 'customer' AND u.registered_at >= $1 AND u.registered_at < $2`, from, to).Scan(&newCustomers)
	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	totals := map[string]any{}
	for _, k := range keys {
		d := days[k]
		totals[k] = map[string]any{"sales": s.gooDec(d.Sales), "orders": d.Orders, "items": d.Items, "tax": s.gooDec(d.Tax),
			"shipping": s.gooDec(d.Shipping), "discount": s.gooDec(d.Discount), "customers": 0}
	}
	nDays := max(int(to.Sub(from).Hours()/24), 1)
	net := sales - tax - shipping - refunds
	restJSON(c, http.StatusOK, []any{map[string]any{
		"total_sales": s.gooDec(sales - refunds), "net_sales": s.gooDec(net), "average_sales": s.gooDec(net / float64(nDays)),
		"total_orders": nOrders, "total_items": items, "total_tax": s.gooDec(tax), "total_shipping": s.gooDec(shipping),
		"total_refunds": refunds, "total_discount": s.gooDec(discount), "totals_grouped_by": "day", "totals": totals,
		"total_customers": newCustomers,
		"_links":          map[string]any{"about": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/reports")}}},
	}})
}

func (s *Server) gooV3ReportTopSellers(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	from, to := s.gooRESTReportRange(c)
	rows, err := s.store.Pool.Query(ctx, `SELECT i.product_id, max(i.name), sum(i.quantity)::bigint AS qty FROM goo_order_items i JOIN goo_orders o ON o.id = i.order_id
		WHERE o.type = 'shop_order' AND o.status = ANY($1) AND i.type = 'line_item' AND o.date_created >= $2 AND o.date_created < $3
		GROUP BY i.product_id ORDER BY qty DESC, i.product_id LIMIT 12`, gooReportStatuses, from, to)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	type row struct {
		id   int64
		name string
		qty  int64
	}
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.name, &r.qty) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	out := []any{}
	for _, r := range list {
		name := r.name
		if p, err := s.gooGetProduct(ctx, r.id); err == nil {
			name = p.Name
		}
		out = append(out, map[string]any{"name": name, "product_id": r.id, "quantity": r.qty,
			"_links": map[string]any{"product": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/products/"+itoa64(r.id))}}}})
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3ReportOrdersTotals(c *gin.Context, req *restRequest) {
	counts := s.gooOrderStatusCounts(c.Request.Context())
	out := []any{}
	for _, st := range gooOrderStatuses {
		if st.Value == "checkout-draft" {
			continue
		}
		out = append(out, map[string]any{"slug": st.Value, "name": st.Label, "total": counts[st.Value]})
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3ReportProductsTotals(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	counts := map[string]int{}
	rows, err := s.store.Pool.Query(ctx, `SELECT coalesce(gp.type,'simple'), count(*) FROM posts p LEFT JOIN goo_products gp ON gp.post_id = p.id
		WHERE p.type = 'product' AND p.status NOT IN ('trash','auto-draft') GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var t string
			var n int
			if rows.Scan(&t, &n) == nil {
				counts[t] = n
			}
		}
		rows.Close()
	}
	out := []any{}
	for _, t := range []struct{ Slug, Name string }{{"external", "External/Affiliate product"}, {"grouped", "Grouped product"}, {"simple", "Simple product"}, {"variable", "Variable product"}} {
		out = append(out, map[string]any{"slug": t.Slug, "name": t.Name, "total": counts[t.Slug]})
	}
	for typ, def := range gooProductTypes {
		out = append(out, map[string]any{"slug": typ, "name": def.Label, "total": counts[typ]})
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3ReportCustomersTotals(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	var paying, all int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM users u JOIN user_roles ur ON ur.user_id = u.id JOIN roles r ON r.id = ur.role_id WHERE r.slug = 'customer'`).Scan(&all)
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(DISTINCT u.id) FROM users u JOIN user_roles ur ON ur.user_id = u.id JOIN roles r ON r.id = ur.role_id
		WHERE r.slug = 'customer' AND EXISTS (SELECT 1 FROM goo_orders o WHERE o.customer_id = u.id AND o.type = 'shop_order' AND o.status IN ('processing','completed'))`).Scan(&paying)
	restJSON(c, http.StatusOK, []any{
		map[string]any{"slug": "paying", "name": "Paying customer", "total": paying},
		map[string]any{"slug": "non_paying", "name": "Non-paying customer", "total": all - paying},
	})
}

func (s *Server) gooV3ReportCouponsTotals(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	counts := map[string]int{}
	rows, err := s.store.Pool.Query(ctx, `SELECT discount_type, count(*) FROM goo_coupons WHERE status <> 'trash' GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var t string
			var n int
			if rows.Scan(&t, &n) == nil {
				counts[t] = n
			}
		}
		rows.Close()
	}
	out := []any{}
	for _, t := range gooCouponTypes {
		out = append(out, map[string]any{"slug": t.Value, "name": t.Label, "total": counts[t.Value]})
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3ReportReviewsTotals(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	counts := map[int]int{}
	rows, err := s.store.Pool.Query(ctx, `SELECT (cm.meta->>'rating')::int, count(*) FROM comments cm JOIN posts p ON p.id = cm.post_id
		WHERE p.type = 'product' AND cm.status = 'approved' AND (cm.meta->>'rating') ~ '^[1-5]$' GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var r, n int
			if rows.Scan(&r, &n) == nil {
				counts[r] = n
			}
		}
		rows.Close()
	}
	out := []any{}
	for r := 1; r <= 5; r++ {
		star := "stars"
		if r == 1 {
			star = "star"
		}
		out = append(out, map[string]any{"slug": "rated_" + strconv.Itoa(r) + "_out_of_5", "name": "Rated " + strconv.Itoa(r) + " out of 5 " + star, "total": counts[r]})
	}
	restJSON(c, http.StatusOK, out)
}

// Taxes -------------------------------------------------------------------------------------------------

func (s *Server) gooV3TaxJSON(ctx context.Context, r *gooTaxRate) map[string]any {
	class := r.Class
	if class == "" {
		class = "standard"
	}
	return map[string]any{"id": r.ID, "country": r.Country, "state": r.State, "postcode": strings.Join(r.Postcodes, "; "), "city": strings.Join(r.Cities, "; "),
		"postcodes": nonNilSlice(r.Postcodes), "cities": nonNilSlice(r.Cities), "rate": strconv.FormatFloat(r.Rate, 'f', 4, 64),
		"name": r.Name, "priority": r.Priority, "compound": r.Compound, "shipping": r.Shipping, "order": r.Position, "class": class,
		"_links": s.gooLinks(ctx, "taxes", r.ID)}
}

func gooRESTSplitList(v any) []string {
	var out []string
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			if s := strings.TrimSpace(gooStr(x)); s != "" {
				out = append(out, strings.ToUpper(s))
			}
		}
	case string:
		for _, s := range strings.FieldsFunc(t, func(r rune) bool { return r == ';' || r == '\n' }) {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, strings.ToUpper(s))
			}
		}
	}
	return out
}

func (s *Server) gooV3ApplyTax(r *gooTaxRate, b map[string]any) *gooAPIErr {
	if v, ok := b["country"]; ok {
		r.Country = gooStr(v)
	}
	if v, ok := b["state"]; ok {
		r.State = gooStr(v)
	}
	for _, k := range []string{"postcode", "postcodes"} {
		if v, ok := b[k]; ok {
			r.Postcodes = gooRESTSplitList(v)
		}
	}
	for _, k := range []string{"city", "cities"} {
		if v, ok := b[k]; ok {
			r.Cities = gooRESTSplitList(v)
		}
	}
	if v, ok := b["rate"]; ok {
		f, ok := gooFloat(v)
		if !ok {
			return gooErr(400, "rest_invalid_param", "Invalid parameter(s): rate")
		}
		r.Rate = f
	}
	if v, ok := b["name"]; ok {
		r.Name = gooStr(v)
	}
	if v, ok := b["priority"]; ok {
		r.Priority = int(gooInt(v))
	}
	if v, ok := b["compound"]; ok {
		r.Compound = gooBool(v)
	}
	if v, ok := b["shipping"]; ok {
		r.Shipping = gooBool(v)
	}
	if v, ok := b["order"]; ok {
		r.Position = int(gooInt(v))
	}
	if v, ok := b["class"]; ok {
		cl := gooStr(v)
		if cl == "standard" {
			cl = ""
		}
		valid := false
		for _, tc := range s.gooTaxClasses() {
			if tc.Value == cl {
				valid = true
			}
		}
		if !valid {
			return gooErr(400, "rest_invalid_param", "Invalid parameter(s): class")
		}
		r.Class = cl
	}
	return nil
}

func (s *Server) gooV3TaxCreate(c *gin.Context, b map[string]any) (map[string]any, *gooAPIErr) {
	r := &gooTaxRate{Priority: 1, Shipping: true}
	if aerr := s.gooV3ApplyTax(r, b); aerr != nil {
		return nil, aerr
	}
	if err := s.gooSaveTaxRate(c.Request.Context(), r); err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_create", err.Error())
	}
	return s.gooV3TaxJSON(c.Request.Context(), r), nil
}

func (s *Server) gooV3TaxUpdate(c *gin.Context, id int64, b map[string]any) (map[string]any, *gooAPIErr) {
	r := s.gooTaxRateByID(c.Request.Context(), id)
	if r == nil {
		return nil, gooErr(404, "woocommerce_rest_invalid_id", "Invalid resource ID.")
	}
	if aerr := s.gooV3ApplyTax(r, b); aerr != nil {
		return nil, aerr
	}
	if err := s.gooSaveTaxRate(c.Request.Context(), r); err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_edit", err.Error())
	}
	return s.gooV3TaxJSON(c.Request.Context(), r), nil
}

func (s *Server) gooV3TaxDelete(c *gin.Context, id int64, force bool) (map[string]any, *gooAPIErr) {
	if !force {
		return nil, gooErr(http.StatusNotImplemented, "woocommerce_rest_trash_not_supported", "Taxes do not support trashing.")
	}
	r := s.gooTaxRateByID(c.Request.Context(), id)
	if r == nil {
		return nil, gooErr(404, "woocommerce_rest_invalid_id", "Invalid resource ID.")
	}
	obj := s.gooV3TaxJSON(c.Request.Context(), r)
	_, _ = s.store.Pool.Exec(c.Request.Context(), `DELETE FROM goo_tax_rates WHERE id = $1`, id)
	return obj, nil
}

func (s *Server) gooV3TaxesList(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	class := "*"
	if v, ok := c.GetQuery("class"); ok {
		class = v
		if class == "standard" {
			class = ""
		}
	}
	rates, err := s.gooTaxRates(ctx, class)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	if c.Query("orderby") == "id" {
		sort.SliceStable(rates, func(i, j int) bool { return rates[i].ID < rates[j].ID })
	}
	if strings.EqualFold(c.Query("order"), "desc") {
		for i, j := 0, len(rates)-1; i < j; i, j = i+1, j-1 {
			rates[i], rates[j] = rates[j], rates[i]
		}
	}
	page := gooPaginateSlice(c, rates)
	out := make([]any, 0, len(page))
	for i := range page {
		out = append(out, s.gooV3TaxJSON(ctx, &page[i]))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3TaxClassJSON(ctx context.Context, slug, name string) map[string]any {
	if slug == "" {
		slug = "standard"
	}
	return map[string]any{"slug": slug, "name": name, "_links": map[string]any{"collection": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/taxes/classes")}}}}
}

func (s *Server) gooV3TaxClasses(c *gin.Context, req *restRequest) {
	out := []any{}
	for _, tc := range s.gooTaxClasses() {
		out = append(out, s.gooV3TaxClassJSON(c.Request.Context(), tc.Value, tc.Label))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3TaxClassCreate(c *gin.Context, req *restRequest) {
	b := gooFormBody(c, req)
	name := strings.TrimSpace(gooStr(b["name"]))
	if name == "" {
		restErr(c, http.StatusBadRequest, "rest_missing_callback_param", "Missing parameter(s): name")
		return
	}
	slug := gooSanitizeKey(name)
	for _, tc := range s.gooTaxClasses() {
		if tc.Value == slug || slug == "standard" {
			restErr(c, http.StatusBadRequest, "woocommerce_rest_tax_class_exists", "Tax class already exists")
			return
		}
	}
	list := strings.TrimSpace(s.gooOptString("tax_classes"))
	if list != "" {
		list += "\n"
	}
	if err := s.gooSetOpts(c.Request.Context(), map[string]any{"tax_classes": list + name}); err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_create", err.Error())
		return
	}
	restJSON(c, http.StatusCreated, s.gooV3TaxClassJSON(c.Request.Context(), slug, name))
}

// gooV3TaxClassRates is GET /taxes/classes/{slug}: the rates of a class.
func (s *Server) gooV3TaxClassRates(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	slug := req.Params["slug"]
	if slug == "standard" {
		slug = ""
	}
	rates, _ := s.gooTaxRates(ctx, slug)
	out := []any{}
	for i := range rates {
		out = append(out, s.gooV3TaxJSON(ctx, &rates[i]))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3TaxClassDelete(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	if force, _ := gooQueryBool(c, "force"); !force && !gooBool(req.Body["force"]) {
		restErr(c, http.StatusNotImplemented, "woocommerce_rest_trash_not_supported", "Taxes do not support trashing.")
		return
	}
	slug := req.Params["slug"]
	var keep []string
	var name string
	for _, tc := range s.gooTaxClasses() {
		if tc.Value == "" {
			continue
		}
		if tc.Value == slug {
			name = tc.Label
			continue
		}
		keep = append(keep, tc.Label)
	}
	if name == "" {
		restErr(c, http.StatusNotFound, "woocommerce_rest_invalid_id", "Invalid resource id.")
		return
	}
	_ = s.gooSetOpts(ctx, map[string]any{"tax_classes": strings.Join(keep, "\n")})
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_tax_rates WHERE class = $1`, slug)
	restJSON(c, http.StatusOK, s.gooV3TaxClassJSON(ctx, slug, name))
}

// Settings -----------------------------------------------------------------------------------------------

// gooSettingGroup is a settings group of the REST API: a settings tab, or an e-mail's settings.
type gooSettingGroup struct {
	ID, Label, Desc, Parent string
	Fields                  []gooField
	Email                   string // e-mail id for email_<id> groups
}

// gooSettingGroups lists the groups: the settings tabs with fields, then one per e-mail.
func (s *Server) gooSettingGroups() []*gooSettingGroup {
	var out []*gooSettingGroup
	for _, t := range gooSettingsTabs() {
		g := &gooSettingGroup{ID: t.ID, Label: strings.ReplaceAll(t.Title, "&amp;", "&")}
		if t.ID == "checkout" {
			g.ID = "checkout"
		}
		for _, sec := range t.Sections {
			for _, f := range sec.Fields {
				if f.Type == "title" || f.Type == "sectionend" {
					if f.Type == "title" && g.Desc == "" {
						g.Desc = gooPlainMsg(f.Desc)
					}
					continue
				}
				g.Fields = append(g.Fields, f)
			}
		}
		if t.ID == "email" {
			g.ID = "email"
		}
		out = append(out, g)
	}
	for _, d := range gooEmailDefs {
		if d.Module != "" && !s.gooExtOn(d.Module) {
			continue
		}
		out = append(out, &gooSettingGroup{ID: "email_" + d.ID, Label: d.Title, Desc: d.Description, Parent: "email", Email: d.ID, Fields: gooRESTEmailFields(d)})
	}
	return out
}

// gooRESTEmailFields are the settings fields of an e-mail (WC_Email::init_form_fields()).
func gooRESTEmailFields(d *gooEmailDef) []gooField {
	var f []gooField
	if !d.Manual {
		f = append(f, gooField{ID: "enabled", Title: "Enable/Disable", Type: "checkbox", Default: "yes", Desc: "Enable this email notification"})
	}
	if !d.Customer {
		f = append(f, gooField{ID: "recipient", Title: "Recipient(s)", Type: "text", Default: "", Desc: "Enter recipients (comma separated) for this email."})
	}
	f = append(f,
		gooField{ID: "subject", Title: "Subject", Type: "text", Default: d.Subject},
		gooField{ID: "heading", Title: "Email heading", Type: "text", Default: d.Heading},
		gooField{ID: "additional_content", Title: "Additional content", Type: "textarea", Default: d.Additional, Desc: "Text to appear below the main email content."},
		gooField{ID: "email_type", Title: "Email type", Type: "select", Default: "html", Desc: "Choose which format of email to send.",
			Options: []gooOpt{{"plain", "Plain text"}, {"html", "HTML"}, {"multipart", "Multipart"}}},
	)
	return f
}

func (s *Server) gooSettingGroup(id string) *gooSettingGroup {
	for _, g := range s.gooSettingGroups() {
		if g.ID == id {
			return g
		}
	}
	return nil
}

func (s *Server) gooV3SettingGroups(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	out := []any{}
	subs := map[string][]string{}
	groups := s.gooSettingGroups()
	for _, g := range groups {
		if g.Parent != "" {
			subs[g.Parent] = append(subs[g.Parent], g.ID)
		}
	}
	for _, g := range groups {
		out = append(out, map[string]any{"id": g.ID, "label": g.Label, "description": g.Desc, "parent_id": g.Parent, "sub_groups": nonNilSlice(subs[g.ID]),
			"_links": map[string]any{"options": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/settings/"+g.ID)}}}})
	}
	restJSON(c, http.StatusOK, out)
}

// gooSettingValue is a setting's current value in WooCommerce's representation.
func (s *Server) gooSettingValue(g *gooSettingGroup, f gooField) any {
	if g.Email != "" {
		return s.gooEmailSettings(g.Email)[f.ID]
	}
	v := s.gooOpt(f.ID)
	switch t := v.(type) {
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case []any, []string:
		return s.gooOptList(f.ID)
	case nil:
		return ""
	}
	return v
}

func gooSettingDefault(f gooField) any {
	switch t := f.Default.(type) {
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	}
	return f.Default
}

func (s *Server) gooSettingJSON(c *gin.Context, g *gooSettingGroup, f gooField) map[string]any {
	id := f.ID
	if g.Email == "" {
		id = "woocommerce_" + f.ID
	}
	out := map[string]any{"id": id, "label": f.Title, "description": gooPlainMsg(f.Desc), "type": f.Type, "default": gooSettingDefault(f),
		"tip": f.Tip, "value": s.gooSettingValue(g, f), "group_id": g.ID,
		"_links": map[string]any{"self": []map[string]any{{"href": s.gooRESTURL(c.Request.Context(), gooV3NS+"/settings/"+g.ID+"/"+id)}},
			"collection": []map[string]any{{"href": s.gooRESTURL(c.Request.Context(), gooV3NS+"/settings/"+g.ID)}}}}
	if f.Holder != "" {
		out["placeholder"] = f.Holder
	}
	if len(f.Options) > 0 {
		opts := map[string]string{}
		for _, o := range f.Options {
			opts[o.Value] = strings.ReplaceAll(o.Label, "&amp;", "&")
		}
		out["options"] = opts
	}
	if f.Type == "single_select_country" || f.Type == "multi_select_countries" {
		opts := map[string]string{}
		for _, cc := range gooCountryOrder {
			if st := gooStates[cc.Code]; len(st) > 0 && f.Type == "single_select_country" {
				for _, x := range st {
					opts[cc.Code+":"+x.Code] = cc.Name + " — " + x.Name
				}
				continue
			}
			opts[cc.Code] = cc.Name
		}
		out["options"] = opts
	}
	return out
}

func (s *Server) gooV3SettingsGroup(c *gin.Context, req *restRequest) {
	g := s.gooSettingGroup(req.Params["group"])
	if g == nil {
		restErr(c, http.StatusNotFound, "rest_setting_setting_group_invalid", "Invalid setting group.")
		return
	}
	out := []any{}
	for _, f := range g.Fields {
		out = append(out, s.gooSettingJSON(c, g, f))
	}
	restJSON(c, http.StatusOK, out)
}

func gooFieldIn(g *gooSettingGroup, id string) (gooField, bool) {
	id = strings.TrimPrefix(id, "woocommerce_")
	for _, f := range g.Fields {
		if f.ID == id {
			return f, true
		}
	}
	return gooField{}, false
}

func (s *Server) gooV3SettingGet(c *gin.Context, req *restRequest) {
	g := s.gooSettingGroup(req.Params["group"])
	if g == nil {
		restErr(c, http.StatusNotFound, "rest_setting_setting_group_invalid", "Invalid setting group.")
		return
	}
	f, ok := gooFieldIn(g, req.Params["id"])
	if !ok {
		restErr(c, http.StatusNotFound, "rest_setting_setting_invalid", "Invalid setting.")
		return
	}
	restJSON(c, http.StatusOK, s.gooSettingJSON(c, g, f))
}

// gooSaveSetting validates and stores one setting value.
func (s *Server) gooSaveSetting(ctx context.Context, u *store.User, g *gooSettingGroup, f gooField, v any) *gooAPIErr {
	var val any
	switch f.Type {
	case "checkbox":
		on := gooBool(v)
		if g.Email != "" {
			val = map[bool]string{true: "yes", false: "no"}[on]
		} else if _, isBool := f.Default.(bool); isBool {
			val = on
		} else {
			val = map[bool]string{true: "yes", false: "no"}[on]
		}
	case "number", "single_select_page":
		n, ok := gooFloat(v)
		if !ok && gooStr(v) != "" {
			return gooErr(400, "rest_setting_value_invalid", "An invalid setting value was passed.")
		}
		val = n
	case "multiselect", "multi_select_countries":
		var list []any
		for _, x := range gooList(v) {
			list = append(list, gooStr(x))
		}
		if list == nil {
			list = []any{}
		}
		val = list
	case "select", "radio":
		str := gooStr(v)
		ok := len(f.Options) == 0
		for _, o := range f.Options {
			if o.Value == str {
				ok = true
			}
		}
		if !ok {
			return gooErr(400, "rest_setting_value_invalid", "An invalid setting value was passed.")
		}
		val = str
	default:
		val = gooStr(v)
	}
	if str, ok := val.(string); ok && f.Type == "textarea" {
		val = gooKsesTextarea(u, str)
	}
	if g.Email != "" {
		m := s.gooOptMap("email_" + g.Email + "_settings")
		cp := map[string]any{}
		for k, x := range m {
			cp[k] = x
		}
		cp[f.ID] = val
		if err := s.gooSetOpts(ctx, map[string]any{"email_" + g.Email + "_settings": cp}); err != nil {
			return gooErr(500, "rest_setting_value_invalid", err.Error())
		}
		return nil
	}
	if err := s.gooSetOpts(ctx, map[string]any{f.ID: val}); err != nil {
		return gooErr(500, "rest_setting_value_invalid", err.Error())
	}
	return nil
}

func (s *Server) gooV3SettingPut(c *gin.Context, req *restRequest) {
	g := s.gooSettingGroup(req.Params["group"])
	if g == nil {
		restErr(c, http.StatusNotFound, "rest_setting_setting_group_invalid", "Invalid setting group.")
		return
	}
	f, ok := gooFieldIn(g, req.Params["id"])
	if !ok {
		restErr(c, http.StatusNotFound, "rest_setting_setting_invalid", "Invalid setting.")
		return
	}
	b := gooFormBody(c, req)
	if aerr := s.gooSaveSetting(c.Request.Context(), currentUser(c), g, f, b["value"]); aerr != nil {
		gooFail(c, aerr)
		return
	}
	restJSON(c, http.StatusOK, s.gooSettingJSON(c, g, f))
}

func (s *Server) gooV3SettingsBatch(c *gin.Context, req *restRequest) {
	g := s.gooSettingGroup(req.Params["group"])
	if g == nil {
		restErr(c, http.StatusNotFound, "rest_setting_setting_group_invalid", "Invalid setting group.")
		return
	}
	var res []any
	for _, x := range gooList(req.Body["update"]) {
		m := gooObj(x)
		f, ok := gooFieldIn(g, gooStr(m["id"]))
		if !ok {
			res = append(res, map[string]any{"id": m["id"], "error": gooErrJSON(gooErr(404, "rest_setting_setting_invalid", "Invalid setting."))})
			continue
		}
		if aerr := s.gooSaveSetting(c.Request.Context(), currentUser(c), g, f, m["value"]); aerr != nil {
			res = append(res, map[string]any{"id": m["id"], "error": gooErrJSON(aerr)})
			continue
		}
		res = append(res, s.gooSettingJSON(c, g, f))
	}
	restJSON(c, http.StatusOK, map[string]any{"update": nonNilSlice(res)})
}

// gooV3SettingsBatchAll is POST /settings/batch: updates with group_id in each item.
func (s *Server) gooV3SettingsBatchAll(c *gin.Context, req *restRequest) {
	var res []any
	for _, x := range gooList(req.Body["update"]) {
		m := gooObj(x)
		g := s.gooSettingGroup(gooStr(m["group_id"]))
		if g == nil {
			res = append(res, map[string]any{"id": m["id"], "error": gooErrJSON(gooErr(404, "rest_setting_setting_group_invalid", "Invalid setting group."))})
			continue
		}
		f, ok := gooFieldIn(g, gooStr(m["id"]))
		if !ok {
			res = append(res, map[string]any{"id": m["id"], "error": gooErrJSON(gooErr(404, "rest_setting_setting_invalid", "Invalid setting."))})
			continue
		}
		if aerr := s.gooSaveSetting(c.Request.Context(), currentUser(c), g, f, m["value"]); aerr != nil {
			res = append(res, map[string]any{"id": m["id"], "error": gooErrJSON(aerr)})
			continue
		}
		res = append(res, s.gooSettingJSON(c, g, f))
	}
	restJSON(c, http.StatusOK, map[string]any{"update": nonNilSlice(res)})
}

// Shipping zones -------------------------------------------------------------------------------------------

func (s *Server) gooV3ZoneJSON(ctx context.Context, z *gooZone) map[string]any {
	base := s.gooRESTURL(ctx, gooV3NS+"/shipping/zones/"+itoa64(z.ID))
	return map[string]any{"id": z.ID, "name": z.Name, "order": z.Position,
		"_links": map[string]any{"self": []map[string]any{{"href": base}}, "collection": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/shipping/zones")}},
			"describedby": []map[string]any{{"href": base + "/locations"}}}}
}

func (s *Server) gooV3ZonesList(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	zones, err := s.gooZones(ctx, true)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	// "Locations not covered by your other zones" comes first, like WooCommerce.
	out := []any{}
	for _, z := range zones {
		if z.ID == 0 {
			out = append(out, s.gooV3ZoneJSON(ctx, z))
		}
	}
	for _, z := range zones {
		if z.ID != 0 {
			out = append(out, s.gooV3ZoneJSON(ctx, z))
		}
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3ZoneCreate(c *gin.Context, b map[string]any) (map[string]any, *gooAPIErr) {
	name := strings.TrimSpace(gooStr(b["name"]))
	if name == "" {
		return nil, gooErr(400, "rest_missing_callback_param", "Missing parameter(s): name")
	}
	z := &gooZone{Name: name, Position: int(gooInt(b["order"]))}
	if err := s.gooSaveZone(c.Request.Context(), z); err != nil {
		return nil, gooErr(500, "woocommerce_rest_shipping_zone_invalid", err.Error())
	}
	return s.gooV3ZoneJSON(c.Request.Context(), z), nil
}

func (s *Server) gooV3ZoneUpdate(c *gin.Context, id int64, b map[string]any) (map[string]any, *gooAPIErr) {
	if id == 0 {
		return nil, gooErr(403, "woocommerce_rest_shipping_zone_invalid_zone", "The \"locations not covered by your other zones\" zone cannot be updated.")
	}
	z, err := s.gooZone(c.Request.Context(), id)
	if err != nil {
		return nil, gooErr(404, "woocommerce_rest_shipping_zone_invalid", "Resource does not exist.")
	}
	if v, ok := b["name"]; ok {
		z.Name = gooStr(v)
	}
	if v, ok := b["order"]; ok {
		z.Position = int(gooInt(v))
	}
	if err := s.gooSaveZone(c.Request.Context(), z); err != nil {
		return nil, gooErr(500, "woocommerce_rest_shipping_zone_invalid", err.Error())
	}
	return s.gooV3ZoneJSON(c.Request.Context(), z), nil
}

func (s *Server) gooV3ZoneDelete(c *gin.Context, id int64, force bool) (map[string]any, *gooAPIErr) {
	if !force {
		return nil, gooErr(http.StatusNotImplemented, "woocommerce_rest_trash_not_supported", "Shipping zones do not support trashing.")
	}
	if id == 0 {
		return nil, gooErr(403, "woocommerce_rest_shipping_zone_invalid_zone", "The \"locations not covered by your other zones\" zone cannot be deleted.")
	}
	z, err := s.gooZone(c.Request.Context(), id)
	if err != nil {
		return nil, gooErr(404, "woocommerce_rest_shipping_zone_invalid", "Resource does not exist.")
	}
	obj := s.gooV3ZoneJSON(c.Request.Context(), z)
	if err := s.gooDeleteZone(c.Request.Context(), id); err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_delete", err.Error())
	}
	return obj, nil
}

func (s *Server) gooV3LocationsJSON(ctx context.Context, z *gooZone) []any {
	out := []any{}
	for _, l := range z.Locations {
		out = append(out, map[string]any{"code": l.Code, "type": l.Type,
			"_links": map[string]any{"collection": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/shipping/zones/"+itoa64(z.ID)+"/locations")}},
				"describes": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/shipping/zones/"+itoa64(z.ID))}}}})
	}
	return out
}

func (s *Server) gooV3ZoneLocations(c *gin.Context, req *restRequest) {
	z, err := s.gooZone(c.Request.Context(), req.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_shipping_zone_invalid", "Resource does not exist.")
		return
	}
	restJSON(c, http.StatusOK, s.gooV3LocationsJSON(c.Request.Context(), z))
}

// gooV3ZoneLocationsPut replaces a zone's locations. WooCommerce clients send a JSON array; the
// core REST parser only accepts objects, so the array is read from {"locations": [...]}, from a
// body whose content type is not application/json, or from form fields locations[0][code].
func (s *Server) gooV3ZoneLocationsPut(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	if req.ID() == 0 {
		restErr(c, http.StatusForbidden, "woocommerce_rest_shipping_zone_invalid_zone", "The \"locations not covered by your other zones\" zone cannot be updated.")
		return
	}
	z, err := s.gooZone(ctx, req.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_shipping_zone_invalid", "Resource does not exist.")
		return
	}
	list := gooList(req.Body["locations"])
	if list == nil && len(req.Body) == 0 && c.ContentType() != "application/json" {
		raw, _ := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		_ = json.Unmarshal(raw, &list)
	}
	z.Locations = nil
	for _, x := range list {
		m := gooObj(x)
		code := strings.TrimSpace(gooStr(m["code"]))
		if code == "" {
			continue
		}
		typ := gooStr(m["type"])
		if typ == "" {
			typ = "country"
		}
		if !containsStr([]string{"postcode", "state", "country", "continent"}, typ) {
			continue
		}
		z.Locations = append(z.Locations, gooZoneLocation{Code: code, Type: typ})
	}
	if err := s.gooSaveZone(ctx, z); err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_shipping_zone_invalid", err.Error())
		return
	}
	restJSON(c, http.StatusOK, s.gooV3LocationsJSON(ctx, z))
}

// gooV3MethodJSON is a zone's method instance with its settings.
func (s *Server) gooV3MethodJSON(ctx context.Context, m *gooMethodInstance) map[string]any {
	def := gooShippingMethods[m.MethodID]
	title, desc := m.MethodID, ""
	settings := map[string]any{}
	if def != nil {
		title, desc = def.Title, def.Description
		for _, f := range def.Fields {
			if f.Type == "title" {
				continue
			}
			field := map[string]any{"id": f.ID, "label": f.Title, "description": gooPlainMsg(f.Desc), "type": f.Type, "value": m.Setting(f.ID),
				"default": gooSettingDefault(f), "tip": f.Tip, "placeholder": f.Holder}
			if len(f.Options) > 0 {
				opts := map[string]string{}
				for _, o := range f.Options {
					opts[o.Value] = o.Label
				}
				field["options"] = opts
			}
			settings[f.ID] = field
		}
	}
	base := s.gooRESTURL(ctx, gooV3NS+"/shipping/zones/"+itoa64(m.ZoneID)+"/methods")
	return map[string]any{"id": m.InstanceID, "instance_id": m.InstanceID, "title": m.Title(), "order": m.Position, "enabled": m.Enabled,
		"method_id": m.MethodID, "method_title": title, "method_description": desc, "settings": settings,
		"_links": map[string]any{"self": []map[string]any{{"href": base + "/" + itoa64(m.InstanceID)}}, "collection": []map[string]any{{"href": base}},
			"describes": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/shipping/zones/"+itoa64(m.ZoneID))}}}}
}

func (s *Server) gooV3ZoneMethods(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	zid := gooInt(req.Params["zone_id"])
	if _, err := s.gooZone(ctx, zid); err != nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_shipping_zone_invalid", "Resource does not exist.")
		return
	}
	list, _ := s.gooMethodInstances(ctx, zid)
	out := []any{}
	for _, m := range list {
		out = append(out, s.gooV3MethodJSON(ctx, m))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3ZoneMethod(ctx context.Context, zoneID, id int64) (*gooMethodInstance, *gooAPIErr) {
	list, _ := s.gooMethodInstances(ctx, zoneID)
	for _, m := range list {
		if m.InstanceID == id {
			return m, nil
		}
	}
	return nil, gooErr(404, "woocommerce_rest_shipping_zone_method_invalid", "Resource does not exist.")
}

// gooV3ApplyMethod writes {enabled, order, settings{}} onto a method instance.
func gooV3ApplyMethod(m *gooMethodInstance, b map[string]any) {
	if v, ok := b["enabled"]; ok {
		m.Enabled = gooBool(v)
	}
	if v, ok := b["order"]; ok {
		m.Position = int(gooInt(v))
	}
	for k, v := range gooObj(b["settings"]) {
		m.Settings[k] = gooStr(v)
	}
}

func (s *Server) gooV3ZoneMethodCreate(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	zid := gooInt(req.Params["zone_id"])
	if _, err := s.gooZone(ctx, zid); err != nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_shipping_zone_invalid", "Resource does not exist.")
		return
	}
	b := gooFormBody(c, req)
	mid := gooStr(b["method_id"])
	if gooShippingMethods[mid] == nil {
		restErr(c, http.StatusBadRequest, "woocommerce_rest_shipping_zone_method_invalid", "Resource cannot be created.")
		return
	}
	m, err := s.gooAddMethod(ctx, zid, mid, nil)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_shipping_zone_method_invalid", err.Error())
		return
	}
	gooV3ApplyMethod(m, b)
	_ = s.gooSaveMethod(ctx, m)
	restJSON(c, http.StatusCreated, s.gooV3MethodJSON(ctx, m))
}

func (s *Server) gooV3ZoneMethodGet(c *gin.Context, req *restRequest) {
	m, aerr := s.gooV3ZoneMethod(c.Request.Context(), gooInt(req.Params["zone_id"]), req.ID())
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	restJSON(c, http.StatusOK, s.gooV3MethodJSON(c.Request.Context(), m))
}

func (s *Server) gooV3ZoneMethodUpdate(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	m, aerr := s.gooV3ZoneMethod(ctx, gooInt(req.Params["zone_id"]), req.ID())
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	gooV3ApplyMethod(m, gooFormBody(c, req))
	if err := s.gooSaveMethod(ctx, m); err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_shipping_zone_method_invalid", err.Error())
		return
	}
	restJSON(c, http.StatusOK, s.gooV3MethodJSON(ctx, m))
}

func (s *Server) gooV3ZoneMethodDelete(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	if force, _ := gooQueryBool(c, "force"); !force && !gooBool(req.Body["force"]) {
		restErr(c, http.StatusNotImplemented, "woocommerce_rest_trash_not_supported", "Shipping methods do not support trashing.")
		return
	}
	m, aerr := s.gooV3ZoneMethod(ctx, gooInt(req.Params["zone_id"]), req.ID())
	if aerr != nil {
		gooFail(c, aerr)
		return
	}
	obj := s.gooV3MethodJSON(ctx, m)
	_ = s.gooDeleteMethod(ctx, m.InstanceID)
	restJSON(c, http.StatusOK, obj)
}

func (s *Server) gooV3ShippingMethodJSON(ctx context.Context, d *gooShippingMethodDef) map[string]any {
	return map[string]any{"id": d.ID, "title": d.Title, "description": d.Description, "_links": s.gooLinksStr(ctx, "shipping_methods", d.ID)}
}

func (s *Server) gooLinksStr(ctx context.Context, collection, id string) map[string]any {
	col := s.gooRESTURL(ctx, gooV3NS+"/"+collection)
	return restLinks(col+"/"+id, col, nil)
}

func (s *Server) gooV3ShippingMethods(c *gin.Context, req *restRequest) {
	out := []any{}
	for _, d := range s.gooShippingMethodOrder() {
		out = append(out, s.gooV3ShippingMethodJSON(c.Request.Context(), d))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3ShippingMethod(c *gin.Context, req *restRequest) {
	d := gooShippingMethods[req.Params["id"]]
	if d == nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_shipping_method_invalid", "Resource does not exist.")
		return
	}
	restJSON(c, http.StatusOK, s.gooV3ShippingMethodJSON(c.Request.Context(), d))
}

// Payment gateways -------------------------------------------------------------------------------------------

func (s *Server) gooV3GatewayJSON(ctx context.Context, g *gooGatewayHandle, order int) map[string]any {
	settings := map[string]any{}
	vals := s.gooGatewaySettings(g.ID())
	for _, f := range g.FormFields() {
		if f.ID == "enabled" || f.ID == "description" || f.Type == "title" {
			continue
		}
		field := map[string]any{"id": f.ID, "label": f.Title, "description": gooPlainMsg(f.Desc), "type": f.Type, "value": gooStr(vals[f.ID]),
			"default": gooSettingDefault(f), "tip": f.Tip, "placeholder": f.Holder}
		if len(f.Options) > 0 {
			opts := map[string]string{}
			for _, o := range f.Options {
				opts[o.Value] = o.Label
			}
			field["options"] = opts
		}
		if f.Type == "bacs_accounts" {
			field["value"] = vals[f.ID]
		}
		settings[f.ID] = field
	}
	var supports []string
	for _, f := range []string{"products", "refunds", "tokenization", "subscriptions"} {
		if g.Supports(f) {
			supports = append(supports, f)
		}
	}
	return map[string]any{"id": g.ID(), "title": g.Title(), "description": g.Description(), "order": order, "enabled": g.Enabled(),
		"method_title": g.MethodTitle(), "method_description": g.MethodDescription(), "method_supports": nonNilSlice(supports),
		"settings": settings, "needs_setup": false, "post_install_scripts": []any{}, "settings_url": s.links(ctx).HomeURL("/wp-admin/goocommerce?page=settings&tab=checkout&section=" + g.ID()),
		"connection_url": nil, "setup_help_text": nil, "required_settings_keys": []any{},
		"_links": s.gooLinksStr(ctx, "payment_gateways", g.ID())}
}

func (s *Server) gooV3Gateways(c *gin.Context, req *restRequest) {
	out := []any{}
	for i, g := range s.gooGatewaysOrdered() {
		out = append(out, s.gooV3GatewayJSON(c.Request.Context(), g, i))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooGatewayOrderIndex(id string) (*gooGatewayHandle, int) {
	for i, g := range s.gooGatewaysOrdered() {
		if g.ID() == id {
			return g, i
		}
	}
	return nil, 0
}

func (s *Server) gooV3Gateway(c *gin.Context, req *restRequest) {
	g, i := s.gooGatewayOrderIndex(req.Params["id"])
	if g == nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_payment_gateway_invalid", "Resource does not exist.")
		return
	}
	restJSON(c, http.StatusOK, s.gooV3GatewayJSON(c.Request.Context(), g, i))
}

func (s *Server) gooV3GatewayUpdate(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	g, _ := s.gooGatewayOrderIndex(req.Params["id"])
	if g == nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_payment_gateway_invalid", "Resource does not exist.")
		return
	}
	b := gooFormBody(c, req)
	saved := map[string]any{}
	for k, v := range s.gooOptMap(g.ID() + "_settings") {
		saved[k] = v
	}
	if v, ok := b["enabled"]; ok {
		saved["enabled"] = map[bool]string{true: "yes", false: "no"}[gooBool(v)]
	}
	fields := map[string]gooField{}
	for _, f := range g.FormFields() {
		fields[f.ID] = f
	}
	u := currentUser(c)
	for _, k := range []string{"title", "description"} {
		if v, ok := b[k]; ok {
			saved[k] = gooStr(v)
			if fields[k].Type == "textarea" {
				saved[k] = gooKsesTextarea(u, gooStr(v))
			}
		}
	}
	for k, v := range gooObj(b["settings"]) {
		if _, ok := fields[k]; !ok {
			restErr(c, http.StatusBadRequest, "rest_setting_value_invalid", "An invalid setting value was passed.")
			return
		}
		if fields[k].Type == "bacs_accounts" {
			saved[k] = v
			continue
		}
		saved[k] = gooStr(v)
		if fields[k].Type == "textarea" {
			saved[k] = gooKsesTextarea(u, gooStr(v))
		}
	}
	vals := map[string]any{g.ID() + "_settings": saved}
	if v, ok := b["order"]; ok {
		pos := int(gooInt(v))
		var ids []string
		for _, x := range s.gooGatewaysOrdered() {
			if x.ID() != g.ID() {
				ids = append(ids, x.ID())
			}
		}
		pos = min(max(pos, 0), len(ids))
		ids = append(ids[:pos], append([]string{g.ID()}, ids[pos:]...)...)
		list := make([]any, len(ids))
		for i, id := range ids {
			list[i] = id
		}
		vals["gateway_order"] = list
	}
	if err := s.gooSetOpts(ctx, vals); err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_edit", err.Error())
		return
	}
	g, i := s.gooGatewayOrderIndex(g.ID())
	restJSON(c, http.StatusOK, s.gooV3GatewayJSON(ctx, g, i))
}

// System status -----------------------------------------------------------------------------------------

func (s *Server) gooV3SystemStatus(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	lb := s.links(ctx)
	var dbVersion string
	_ = s.store.Pool.QueryRow(ctx, `SHOW server_version`).Scan(&dbVersion)
	pages := []any{}
	for _, p := range []struct{ Key, Name, Short string }{{"shop", "Shop base", ""}, {"cart", "Cart", "[goocommerce_cart]"},
		{"checkout", "Checkout", "[goocommerce_checkout]"}, {"myaccount", "My account", "[goocommerce_my_account]"}, {"terms", "Terms and conditions", ""}} {
		id := s.gooPageID(p.Key)
		set, exists, visible, block, blockPresent := id > 0, false, false, "", false
		if p.Key == "cart" || p.Key == "checkout" {
			block = "woocommerce/" + p.Key
		}
		if post, err := s.store.GetPost(ctx, id); err == nil {
			exists, visible = true, post.Status == "publish"
			blockPresent = block != "" && gooPageHasStoreBlock(p.Key, post.Content)
		}
		pages = append(pages, map[string]any{"page_name": p.Name, "page_id": itoa64(id), "page_set": set, "page_exists": exists, "page_visible": visible,
			"shortcode": p.Short, "block": block, "shortcode_required": p.Short != "" && !blockPresent, "shortcode_present": p.Short != "" && exists && !blockPresent,
			"block_present": blockPresent, "block_required": false})
	}
	var active []any
	if s.gooOn() {
		active = append(active, map[string]any{"plugin": "goocommerce", "name": "Pursae Commerce", "version": gooVersion, "author_name": "Pursae", "network_activated": false})
	}
	restJSON(c, http.StatusOK, map[string]any{
		"environment": map[string]any{"home_url": lb.Home, "site_url": lb.Home, "version": gooVersion, "store_id": "", "wp_version": "Pursae",
			"wp_multisite": false, "wp_memory_limit": 0, "wp_debug_mode": false, "wp_cron": true, "language": s.settings.String("WPLANG"),
			"external_object_cache": nil, "server_info": "Go " + runtime.Version(), "php_version": "", "max_upload_size": 0,
			"default_timezone": s.settings.String("timezone_string"), "mysql_version": "", "mysql_version_string": "PostgreSQL " + dbVersion,
			"secure_connection": c.Request.TLS != nil, "hide_errors": true},
		"database":       map[string]any{"wc_database_version": gooVersion, "database_prefix": "goo_", "database_tables": map[string]any{}},
		"active_plugins": nonNilSlice(active), "inactive_plugins": []any{}, "dropins_mu_plugins": map[string]any{"dropins": []any{}, "mu_plugins": []any{}},
		"theme": map[string]any{"name": s.settings.String("template"), "has_woocommerce_support": true, "has_woocommerce_file": false, "has_outdated_templates": false, "overrides": []any{}},
		"settings": map[string]any{"api_enabled": true, "force_ssl": s.gooOptBool("force_ssl_checkout"), "currency": s.gooCurrency(),
			"currency_symbol": gooCurrencySymbol(s.gooCurrency()), "currency_position": s.gooOptString("currency_pos"),
			"thousand_separator": s.gooOptString("price_thousand_sep"), "decimal_separator": s.gooOptString("price_decimal_sep"),
			"number_of_decimals": s.gooDecimals(), "geolocation_enabled": s.gooOptString("default_customer_address") == "geolocation",
			"taxonomies": map[string]any{"external": "external", "grouped": "grouped", "simple": "simple", "variable": "variable"},
			"product_visibility_terms": map[string]any{"exclude-from-catalog": "exclude-from-catalog", "exclude-from-search": "exclude-from-search",
				"featured": "featured", "outofstock": "outofstock"}, "woocommerce_com_connected": "no"},
		"security": map[string]any{"secure_connection": c.Request.TLS != nil, "hide_errors": true},
		"pages":    pages, "post_type_counts": []any{},
	})
}

func (s *Server) gooV3SystemTools(c *gin.Context, req *restRequest) {
	restJSON(c, http.StatusOK, []any{
		map[string]any{"id": "clear_sessions", "name": "Clear customer sessions", "action": "Clear", "description": "This tool will delete all customer session data from the database, including current carts and saved carts in the database."},
		map[string]any{"id": "recount_terms", "name": "Term counts", "action": "Recount terms", "description": "This tool will recount product terms - useful when changing your settings in a way which hides products from the catalog."},
	})
}

// Data -----------------------------------------------------------------------------------------------------

func (s *Server) gooV3Data(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	out := []any{}
	for _, d := range []struct{ Slug, Desc string }{{"continents", "List of supported continents, countries, and states."},
		{"countries", "List of supported states in a given country."}, {"currencies", "List of supported currencies."}} {
		out = append(out, map[string]any{"slug": d.Slug, "description": d.Desc, "_links": map[string]any{"self": []map[string]any{{"href": s.gooRESTURL(ctx, gooV3NS+"/data/"+d.Slug)}}}})
	}
	restJSON(c, http.StatusOK, out)
}

func gooStatesJSON(country string) []any {
	out := []any{}
	for _, st := range gooStates[country] {
		out = append(out, map[string]any{"code": st.Code, "name": st.Name})
	}
	return out
}

func (s *Server) gooCountryJSON(ctx context.Context, code string) map[string]any {
	return map[string]any{"code": code, "name": gooCountryName(code), "states": gooStatesJSON(code),
		"_links": s.gooLinksStr(ctx, "data/countries", code)}
}

func (s *Server) gooContinentJSON(ctx context.Context, i int) map[string]any {
	ct := gooContinents[i]
	countries := []any{}
	for _, code := range ct.Countries {
		if _, ok := gooCountries[code]; !ok {
			continue
		}
		cur := gooCountryCurrency(code)
		countries = append(countries, map[string]any{"code": code, "name": gooCountryName(code), "currency_code": cur,
			"currency_pos": "left", "decimal_sep": ".", "thousand_sep": ",", "num_decimals": 2, "dimension_unit": "cm", "weight_unit": "kg",
			"states": gooStatesJSON(code)})
	}
	return map[string]any{"code": ct.Code, "name": ct.Name, "countries": countries, "_links": s.gooLinksStr(ctx, "data/continents", ct.Code)}
}

// gooCountryCurrency is the usual currency of a country (WooCommerce's locale-info), "" when unknown.
func gooCountryCurrency(code string) string {
	euro := "AD AT BE CY DE EE ES FI FR GR HR IE IT LT LU LV MC ME MT NL PT SI SK SM VA"
	switch {
	case strings.Contains(euro, code):
		return "EUR"
	}
	m := map[string]string{"US": "USD", "GB": "GBP", "CA": "CAD", "AU": "AUD", "NZ": "NZD", "JP": "JPY", "CN": "CNY", "IN": "INR", "BR": "BRL",
		"MX": "MXN", "TR": "TRY", "CH": "CHF", "SE": "SEK", "NO": "NOK", "DK": "DKK", "PL": "PLN", "CZ": "CZK", "HU": "HUF", "RO": "RON",
		"BG": "BGN", "RU": "RUB", "UA": "UAH", "ZA": "ZAR", "KR": "KRW", "SG": "SGD", "HK": "HKD", "TW": "TWD", "TH": "THB", "MY": "MYR",
		"ID": "IDR", "PH": "PHP", "VN": "VND", "AE": "AED", "SA": "SAR", "IL": "ILS", "EG": "EGP", "NG": "NGN", "KE": "KES", "AR": "ARS",
		"CL": "CLP", "CO": "COP", "PE": "PEN", "UY": "UYU", "PK": "PKR", "BD": "BDT", "LK": "LKR", "NP": "NPR", "IS": "ISK", "RS": "RSD"}
	return m[code]
}

func (s *Server) gooV3Continents(c *gin.Context, req *restRequest) {
	out := []any{}
	for i := range gooContinents {
		out = append(out, s.gooContinentJSON(c.Request.Context(), i))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3Continent(c *gin.Context, req *restRequest) {
	code := strings.ToUpper(req.Params["code"])
	for i, ct := range gooContinents {
		if ct.Code == code {
			restJSON(c, http.StatusOK, s.gooContinentJSON(c.Request.Context(), i))
			return
		}
	}
	restErr(c, http.StatusNotFound, "woocommerce_rest_data_invalid_location", "There are no locations matching these parameters.")
}

func (s *Server) gooV3Countries(c *gin.Context, req *restRequest) {
	out := []any{}
	for _, cc := range gooCountryOrder {
		out = append(out, s.gooCountryJSON(c.Request.Context(), cc.Code))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3Country(c *gin.Context, req *restRequest) {
	code := strings.ToUpper(req.Params["code"])
	if _, ok := gooCountries[code]; !ok {
		restErr(c, http.StatusNotFound, "woocommerce_rest_data_invalid_location", "There are no locations matching these parameters.")
		return
	}
	restJSON(c, http.StatusOK, s.gooCountryJSON(c.Request.Context(), code))
}

func (s *Server) gooCurrencyJSON(ctx context.Context, cur gooCurrency) map[string]any {
	return map[string]any{"code": cur.Code, "name": cur.Name, "symbol": strings.ReplaceAll(cur.Symbol, "&nbsp;", " "), "_links": s.gooLinksStr(ctx, "data/currencies", cur.Code)}
}

func (s *Server) gooV3Currencies(c *gin.Context, req *restRequest) {
	out := []any{}
	for _, cur := range gooCurrencyOrder {
		out = append(out, s.gooCurrencyJSON(c.Request.Context(), cur))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gooV3CurrencyCurrent(c *gin.Context, req *restRequest) {
	code := s.gooCurrency()
	cur, ok := gooCurrencies[code]
	if !ok {
		cur = gooCurrency{Code: code, Name: code, Symbol: code}
	}
	restJSON(c, http.StatusOK, s.gooCurrencyJSON(c.Request.Context(), cur))
}

func (s *Server) gooV3Currency(c *gin.Context, req *restRequest) {
	cur, ok := gooCurrencies[strings.ToUpper(req.Params["code"])]
	if !ok {
		restErr(c, http.StatusNotFound, "woocommerce_rest_data_invalid_currency", "There are no currencies matching these parameters.")
		return
	}
	restJSON(c, http.StatusOK, s.gooCurrencyJSON(c.Request.Context(), cur))
}
