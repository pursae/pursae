package server

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/textfold"
	"wpgo/internal/wp"
)

// Goocommerce › Orders: the HPOS orders list (page=orders, WooCommerce's admin.php?page=wc-orders) with
// views, filters, search and bulk actions, and the order editor (action=edit&id= / action=new).

// gooOrderRow is a row of the orders list (and of Home's recent orders).
type gooOrderRow struct {
	ID          int64
	Number      string
	Name        string
	EditURL     string
	Status      string
	StatusLabel string
	Date        template.HTML
	DateISO     string
	DateFull    string
	Billing     template.HTML
	ShipTo      template.HTML
	ShipVia     string
	Total       template.HTML
	Via         string
	Email       string
	Phone       string
	Items       []gooPreviewItem
	Note        string
	Trashed     bool
	Actions     []gooView
}

type gooPreviewItem struct {
	Name  string
	Qty   int
	Tax   template.HTML
	Total template.HTML
	Meta  string
}

// gooFormatAddress is WC_Order::get_formatted_billing_address(): lines joined by <br/>.
func gooFormatAddress(a map[string]string) template.HTML {
	if len(a) == 0 {
		return ""
	}
	var lines []string
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" {
			lines = append(lines, template.HTMLEscapeString(v))
		}
	}
	add(strings.TrimSpace(a["first_name"] + " " + a["last_name"]))
	add(a["company"])
	add(a["address_1"])
	add(a["address_2"])
	cityLine := strings.TrimSpace(a["city"])
	if st := a["state"]; st != "" {
		cityLine = strings.TrimSpace(cityLine + ", " + gooStateName(a["country"], st))
		cityLine = strings.TrimPrefix(cityLine, ", ")
	}
	if pc := a["postcode"]; pc != "" {
		cityLine = strings.TrimSpace(cityLine + " " + pc)
	}
	add(cityLine)
	if c := a["country"]; c != "" {
		add(gooCountryName(c))
	}
	return template.HTML(strings.Join(lines, "<br/>"))
}

// refunded is the order's refunded total (gooTotalsRefunded loads a page of them at once).
func (s *Server) gooOrderRowOf(ctx context.Context, o *gooOrder, refunded float64) *gooOrderRow {
	r := &gooOrderRow{ID: o.ID, Number: o.Number(), Name: o.BillingName(), Status: o.Status, StatusLabel: gooStatusLabel(o.Status),
		EditURL: gooAdminURL("orders", "action", "edit", "id", itoa64(o.ID)), Email: o.Billing["email"], Phone: o.Billing["phone"],
		Note: o.CustomerNote, Trashed: o.Status == "trash"}
	if r.Trashed {
		r.StatusLabel = "Trash"
		if prev := o.MetaString("_trash_status"); prev != "" {
			r.StatusLabel = gooStatusLabel(prev) + " (Trash)"
		}
	}
	if r.Name == "" {
		r.Name = o.Billing["company"]
	}
	if r.Name == "" && o.Billing["email"] != "" {
		r.Name = o.Billing["email"]
	}
	if r.Name == "" {
		r.Name = "Guest"
	}
	r.Date = template.HTML(s.gooAdminDate(o.DateCreated))
	r.DateISO = o.DateCreated.Format(time.RFC3339)
	r.DateFull = o.DateCreated.In(s.loc()).Format("January 2, 2006 3:04 pm")
	r.Billing = gooFormatAddress(o.Billing)
	if o.PaymentMethodTitle != "" {
		r.Via = o.PaymentMethodTitle
	}
	r.ShipTo = gooFormatAddress(o.Shipping)
	var ship []string
	for _, it := range o.ItemsOf("shipping") {
		ship = append(ship, it.Name)
	}
	r.ShipVia = strings.Join(ship, ", ")
	tot := string(s.gooPriceIn(o.Total, o.Currency))
	if refunded > 0 {
		tot = `<del aria-hidden="true">` + tot + `</del> <ins>` + string(s.gooPriceIn(o.Total-refunded, o.Currency)) + `</ins>`
	}
	r.Total = template.HTML(tot)
	for _, it := range o.ItemsOf("line_item") {
		r.Items = append(r.Items, gooPreviewItem{Name: it.Name, Qty: it.Qty(), Tax: s.gooPriceIn(it.TotalTax, o.Currency),
			Total: s.gooPriceIn(it.Total, o.Currency), Meta: gooItemMetaText(it)})
	}
	switch o.Status {
	case "pending", "on-hold":
		r.Actions = append(r.Actions, gooView{Label: "Processing", URL: "processing"})
		r.Actions = append(r.Actions, gooView{Label: "Complete", URL: "complete"})
	case "processing":
		r.Actions = append(r.Actions, gooView{Label: "Complete", URL: "complete"})
	}
	return r
}

// gooItemMetaText is the visible meta of an item ("Color: Red, Size: L").
func gooItemMetaText(it *gooOrderItem) string {
	var parts []string
	for _, kv := range gooItemDisplayMeta(it) {
		parts = append(parts, kv.Key+": "+kv.Value)
	}
	return strings.Join(parts, ", ")
}

// gooItemDisplayMeta is the item's displayed meta: the "_display" list stored at checkout, else the
// public meta keys (variation attributes, shipping "Items").
func gooItemDisplayMeta(it *gooOrderItem) []gooKV {
	var out []gooKV
	if list, ok := it.Meta["_display"].([]any); ok {
		for _, x := range list {
			if m, ok := x.(map[string]any); ok {
				k, _ := m["key"].(string)
				v, _ := m["value"].(string)
				out = append(out, gooKV{k, v})
			}
		}
		return out
	}
	keys := make([]string, 0, len(it.Meta))
	for k := range it.Meta {
		if strings.HasPrefix(k, "_") || k == "method_id" || k == "instance_id" || k == "rate_id" || k == "label" || k == "compound" ||
			k == "tax_amount" || k == "shipping_tax_amount" || k == "rate_percent" || k == "discount_amount" || k == "discount_amount_tax" || k == "coupon_id" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		label := k
		if strings.HasPrefix(k, "pa_") {
			label = strings.ToUpper(k[3:4]) + k[4:]
		}
		v := it.MetaString(k)
		if v == "" {
			continue
		}
		out = append(out, gooKV{label, v})
	}
	return out
}

// List -----------------------------------------------------------------------------------------------------

type gooOrdersListData struct {
	Rows      []*gooOrderRow
	Views     []gooView
	Status    string
	Search    string
	Month     string
	Months    []gooOpt
	Customer  string
	CustName  string
	Customers []gooOpt
	Pager     gooPager
	Bulk      []gooOpt
	Nonce     string
	IsTrash   bool
	NewURL    string
	BaseURL   string
	ExportURL string
}

// gooOrdersListQuery is the filter of the Orders list (status view, search, month, customer) as an order
// query; the list and its CSV export share it.
func (s *Server) gooOrdersListQuery(c *gin.Context) gooOrderQuery {
	status := strings.TrimPrefix(c.Query("status"), "wc-")
	q := gooOrderQuery{Search: strings.TrimSpace(c.Query("s"))}
	if status != "" && status != "all" {
		q.Status = []string{status}
	}
	if m := c.Query("m"); len(m) == 6 {
		if t, err := time.ParseInLocation("200601", m, s.loc()); err == nil {
			end := t.AddDate(0, 1, 0)
			q.After, q.Before = &t, &end
		}
	}
	if id, _ := strconv.ParseInt(c.Query("_customer_user"), 10, 64); id > 0 {
		q.CustomerID = id
	}
	return q
}

func (s *Server) gooAdminOrders(c *gin.Context) {
	switch c.Query("action") {
	case "edit", "new":
		s.gooAdminOrderEdit(c)
		return
	case "search_customers":
		s.gooAjaxSearchCustomers(c)
		return
	case "customer_details":
		s.gooAjaxCustomerDetails(c)
		return
	case "search_products":
		s.gooAjaxSearchProducts(c)
		return
	case "mark_status":
		s.gooOrderMarkStatus(c)
		return
	case "export_csv":
		s.gooExportOrdersCSV(c)
		return
	case "trash", "untrash", "delete":
		s.gooOrderRowAction(c)
		return
	}
	if c.Request.Method == http.MethodPost {
		s.gooOrdersBulk(c)
		return
	}
	ctx := c.Request.Context()
	status := strings.TrimPrefix(c.Query("status"), "wc-")
	d := &gooOrdersListData{Status: status, Search: strings.TrimSpace(c.Query("s")), Month: c.Query("m"), Customer: c.Query("_customer_user"),
		Nonce: s.nonce(c, gooStoreNonce), IsTrash: status == "trash", NewURL: gooAdminURL("orders", "action", "new")}
	q := s.gooOrdersListQuery(c)
	q.Page, q.PerPage = gooPaged(c), 20
	if id := q.CustomerID; id > 0 {
		if u, err := s.store.UserByID(ctx, id); err == nil {
			d.CustName = u.DisplayName + " (#" + itoa64(u.ID) + " – " + u.Email + ")"
		}
	}
	orders, total, err := s.gooQueryOrders(ctx, q)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	refunded := s.gooTotalsRefunded(ctx, gooOrderIDs(orders))
	for _, o := range orders {
		d.Rows = append(d.Rows, s.gooOrderRowOf(ctx, o, refunded[o.ID]))
	}
	d.BaseURL = gooAdminURL("orders", "status", c.Query("status"), "s", d.Search, "m", d.Month, "_customer_user", d.Customer)
	d.Pager = newGooPager(d.BaseURL, q.Page, q.PerPage, total)
	d.ExportURL = gooAdminURL("orders", "action", "export_csv", "status", c.Query("status"), "s", d.Search, "m", d.Month, "_customer_user", d.Customer, "_wpnonce", d.Nonce)

	counts := s.gooOrderStatusCounts(ctx)
	all := 0
	for st, n := range counts {
		if st != "trash" && st != "checkout-draft" {
			all += n
		}
	}
	d.Views = append(d.Views, gooView{Label: "All", URL: gooAdminURL("orders"), Count: all, Current: status == "" || status == "all"})
	for _, st := range gooOrderStatuses {
		if st.Value == "checkout-draft" || counts[st.Value] == 0 {
			continue
		}
		d.Views = append(d.Views, gooView{Label: st.Label, URL: gooAdminURL("orders", "status", "wc-"+st.Value), Count: counts[st.Value], Current: status == st.Value})
	}
	if counts["trash"] > 0 {
		d.Views = append(d.Views, gooView{Label: "Trash", URL: gooAdminURL("orders", "status", "trash"), Count: counts["trash"], Current: status == "trash"})
	}
	if d.IsTrash {
		d.Bulk = []gooOpt{{"untrash", "Restore"}, {"delete", "Delete permanently"}}
	} else {
		d.Bulk = []gooOpt{{"trash", "Move to Trash"}, {"mark_processing", "Change status to processing"}, {"mark_on-hold", "Change status to on-hold"},
			{"mark_completed", "Change status to completed"}, {"mark_cancelled", "Change status to cancelled"}}
	}
	// Months with orders.
	rows, err := s.store.Pool.Query(ctx, `SELECT DISTINCT to_char(date_created AT TIME ZONE $1, 'YYYYMM') m FROM goo_orders WHERE type = 'shop_order' ORDER BY m DESC`, store.PGZone(s.loc()))
	if err == nil {
		for rows.Next() {
			var m string
			if rows.Scan(&m) == nil {
				if t, err := time.Parse("200601", m); err == nil {
					d.Months = append(d.Months, gooOpt{m, t.Format("January 2006")})
				}
			}
		}
		rows.Close()
	}
	d.Customers = s.gooCustomerOptions(ctx)
	s.gooStoreRender(c, "goocommerce?page=orders", "wc-orders", "Orders", "admin/goo-orders", d)
}

// gooCustomerOptions lists the registered users for the customer selects (customers first).
func (s *Server) gooCustomerOptions(ctx context.Context) []gooOpt {
	rows, err := s.store.Pool.Query(ctx, `SELECT u.id, u.display_name, u.email FROM users u ORDER BY
		(EXISTS (SELECT 1 FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = u.id AND r.slug = 'customer')) DESC, u.display_name LIMIT 500`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []gooOpt
	for rows.Next() {
		var id int64
		var name, email string
		if rows.Scan(&id, &name, &email) == nil {
			out = append(out, gooOpt{itoa64(id), name + " (#" + itoa64(id) + " – " + email + ")"})
		}
	}
	return out
}

// gooOrdersBulk runs a bulk action on the checked orders.
func (s *Server) gooOrdersBulk(c *gin.Context) {
	if !s.checkNonce(c, gooStoreNonce) {
		s.gooDeny(c)
		return
	}
	ctx := c.Request.Context()
	action := c.PostForm("action")
	if action == "" || action == "-1" {
		action = c.PostForm("action2")
	}
	back := defaultStr(c.PostForm("_wp_http_referer"), gooAdminURL("orders"))
	if !strings.HasPrefix(back, "/wp-admin/goocommerce") {
		back = gooAdminURL("orders")
	}
	n := 0
	by := s.gooAdminUserName(c)
	for _, raw := range c.PostFormArray("id[]") {
		id, _ := strconv.ParseInt(raw, 10, 64)
		o, err := s.gooGetOrder(ctx, id)
		if err != nil || o.Type != "shop_order" {
			continue
		}
		switch {
		case strings.HasPrefix(action, "mark_"):
			if s.gooSetStatus(ctx, o, strings.TrimPrefix(action, "mark_"), "Order status changed by bulk edit:", by) == nil {
				n++
			}
		case action == "trash":
			if s.gooTrashOrder(ctx, o) == nil {
				n++
			}
		case action == "untrash":
			if s.gooUntrashOrder(ctx, o) == nil {
				n++
			}
		case action == "delete":
			if s.gooDeleteOrder(ctx, o.ID) == nil {
				n++
			}
		}
	}
	msg := ""
	switch {
	case strings.HasPrefix(action, "mark_"):
		msg = plural(n, "1 order status changed.", "%d order statuses changed.")
	case action == "trash":
		msg = plural(n, "1 order moved to the Trash.", "%d orders moved to the Trash.")
	case action == "untrash":
		msg = plural(n, "1 order restored from the Trash.", "%d orders restored from the Trash.")
	case action == "delete":
		msg = plural(n, "1 order permanently deleted.", "%d orders permanently deleted.")
	}
	gooRedirect(c, back, msg, "")
}

func (s *Server) gooAdminUserName(c *gin.Context) string {
	if u := currentUser(c); u != nil {
		return u.DisplayName
	}
	return ""
}

func (s *Server) gooTrashOrder(ctx context.Context, o *gooOrder) error {
	if o.Status == "trash" {
		return nil
	}
	o.Meta["_trash_status"] = o.Status
	o.Status = "trash"
	if err := s.gooSaveOrder(ctx, o, false); err != nil {
		return err
	}
	s.gooWebhookFire(ctx, "order.deleted", o.ID, map[string]any{"id": o.ID})
	return nil
}

func (s *Server) gooUntrashOrder(ctx context.Context, o *gooOrder) error {
	if o.Status != "trash" {
		return nil
	}
	o.Status = defaultStr(o.MetaString("_trash_status"), "pending")
	delete(o.Meta, "_trash_status")
	if err := s.gooSaveOrder(ctx, o, false); err != nil {
		return err
	}
	s.gooWebhookOrder(ctx, "order.restored", o)
	return nil
}

// gooDeleteOrder deletes an order with its refunds (items, notes and permissions cascade).
func (s *Server) gooDeleteOrder(ctx context.Context, id int64) error {
	var status string
	_ = s.store.Pool.QueryRow(ctx, `SELECT status FROM goo_orders WHERE id = $1`, id).Scan(&status)
	_, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_orders WHERE id = $1 OR (parent_id = $1 AND type = 'shop_order_refund')`, id)
	if err == nil && status != "trash" { // a trashed order already said order.deleted
		s.gooWebhookFire(ctx, "order.deleted", id, map[string]any{"id": id})
	}
	return err
}

// gooOrderMarkStatus is the list's "Processing" / "Complete" action buttons (woocommerce_mark_order_status).
func (s *Server) gooOrderMarkStatus(c *gin.Context) {
	if !s.checkNonce(c, gooStoreNonce) {
		s.gooDeny(c)
		return
	}
	ctx := c.Request.Context()
	id, _ := strconv.ParseInt(c.Query("id"), 10, 64)
	o, err := s.gooGetOrder(ctx, id)
	if err != nil {
		gooRedirect(c, gooAdminURL("orders"), "", "Invalid order.")
		return
	}
	to := c.Query("status")
	if to == "complete" {
		to = "completed"
	}
	if err := s.gooSetStatus(ctx, o, to, "Order status changed from the orders list:", s.gooAdminUserName(c)); err != nil {
		gooRedirect(c, gooAdminURL("orders"), "", err.Error())
		return
	}
	gooRedirect(c, gooAdminURL("orders"), "Order status changed.", "")
}

// gooOrderRowAction is the Trash / Restore / Delete permanently link of a single order.
func (s *Server) gooOrderRowAction(c *gin.Context) {
	if !s.checkNonce(c, gooStoreNonce) {
		s.gooDeny(c)
		return
	}
	ctx := c.Request.Context()
	id, _ := strconv.ParseInt(c.Query("id"), 10, 64)
	o, err := s.gooGetOrder(ctx, id)
	if err != nil || o.Type != "shop_order" {
		gooRedirect(c, gooAdminURL("orders"), "", "Invalid order.")
		return
	}
	switch c.Query("action") {
	case "trash":
		_ = s.gooTrashOrder(ctx, o)
		gooRedirect(c, gooAdminURL("orders"), "1 order moved to the Trash.", "")
	case "untrash":
		_ = s.gooUntrashOrder(ctx, o)
		gooRedirect(c, gooAdminURL("orders", "status", "trash"), "1 order restored from the Trash.", "")
	case "delete":
		_ = s.gooDeleteOrder(ctx, o.ID)
		gooRedirect(c, gooAdminURL("orders", "status", "trash"), "1 order permanently deleted.", "")
	}
}

// AJAX helpers ------------------------------------------------------------------------------------------------

func (s *Server) gooAjaxSearchCustomers(c *gin.Context) {
	// Folded alike on both sides by the database (lower(), then ı and İ become i), so "IŞIK" finds
	// "Işık" on a Turkish site, which a plain ILIKE does not.
	term := "%" + strings.TrimSpace(c.Query("term")) + "%"
	ph := store.FoldedArgSQL("$1")
	rows, err := s.store.Pool.Query(c, `SELECT id, display_name, email FROM users WHERE `+store.FoldedTitleSQL("display_name")+` LIKE `+ph+`
		OR `+store.FoldedTitleSQL("email")+` LIKE `+ph+` OR `+store.FoldedTitleSQL("login")+` LIKE `+ph+` OR id::text = $2
		ORDER BY display_name LIMIT 30`, term, strings.TrimSpace(c.Query("term")))
	out := map[string]string{}
	if err == nil {
		for rows.Next() {
			var id int64
			var name, email string
			if rows.Scan(&id, &name, &email) == nil {
				out[itoa64(id)] = name + " (#" + itoa64(id) + " – " + email + ")"
			}
		}
		rows.Close()
	}
	c.JSON(http.StatusOK, out)
}

// gooCustomerAddresses reads a user's saved billing / shipping addresses (user meta billing_* / shipping_*).
func (s *Server) gooCustomerAddresses(ctx context.Context, userID int64) (map[string]string, map[string]string) {
	billing, shipping := map[string]string{}, map[string]string{}
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return billing, shipping
	}
	for _, k := range gooAddressKeys {
		if v, ok := u.Meta["billing_"+k].(string); ok {
			billing[k] = v
		}
		if v, ok := u.Meta["shipping_"+k].(string); ok {
			shipping[k] = v
		}
	}
	if billing["email"] == "" {
		billing["email"] = u.Email
	}
	if billing["first_name"] == "" && u.Profile != nil {
		billing["first_name"], billing["last_name"] = u.Profile["first_name"], u.Profile["last_name"]
	}
	return billing, shipping
}

func (s *Server) gooAjaxCustomerDetails(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	b, sh := s.gooCustomerAddresses(c, id)
	c.JSON(http.StatusOK, gin.H{"billing": b, "shipping": sh})
}

func (s *Server) gooAjaxSearchProducts(c *gin.Context) {
	out := map[string]string{}
	term := strings.TrimSpace(c.Query("term"))
	for _, o := range s.gooProductOptions(c, false) {
		if term == "" || textfold.Contains(o.Label, term) { // "ışık" finds "IŞIK Eklentisi"
			out[o.Value] = o.Label
		}
	}
	c.JSON(http.StatusOK, out)
}

// gooProductOptions lists purchasable products and variations (the "Add products" modal), or the
// downloadable ones (Grant access).
func (s *Server) gooProductOptions(ctx context.Context, downloadable bool) []gooOpt {
	q := `SELECT po.id FROM posts po JOIN goo_products p ON p.post_id = po.id WHERE po.type IN ('product','product_variation')
		AND po.status IN ('publish','private') AND p.type NOT IN ('variable','grouped','external')`
	if downloadable {
		q += ` AND p.downloadable`
	}
	rows, err := s.store.Pool.Query(ctx, q+` ORDER BY po.title, po.id LIMIT 500`)
	if err != nil {
		return nil
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	prods, _ := s.gooGetProducts(ctx, ids)
	var out []gooOpt
	for _, id := range ids {
		p := prods[id]
		if p == nil {
			continue
		}
		label := s.gooProductTitle(ctx, p)
		if p.Type == gooTypeVariation {
			if sum := s.gooVariationSummary(ctx, p); sum != "" {
				label += " – " + sum
			}
		}
		if p.SKU != "" {
			label += " (" + p.SKU + ")"
		}
		out = append(out, gooOpt{itoa64(p.ID), label})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// Order editor ----------------------------------------------------------------------------------------------

type gooOrderEditData struct {
	O           *gooOrder
	Attribution []gooOpt // Order attribution box (Value = label, Label = value)
	New         bool
	Nonce       string
	Title       string
	Statuses    []gooOpt
	Status      string
	Date        string
	Hour        string
	Minute      string
	Second      string
	Customer    string
	CustLabel   string
	ProfileURL  string
	OrdersURL   string
	Customers   []gooOpt
	BillingF    []gooAddrField
	ShippingF   []gooAddrField
	BillingHTML template.HTML
	ShipHTML    template.HTML
	Gateways    []gooOpt
	PayMethod   string
	PaidLine    string
	IP          string
	ViaLine     string
	PaymentURL  string
	PayInfo     template.HTML // hosted-payment attempts (goo_gateway_pay_admin.go)

	Lines     []*gooItemRow
	Shipping  []*gooItemRow
	Fees      []*gooItemRow
	Coupons   []gooCouponLine
	TaxCols   []gooTaxCol
	Refunds   []gooRefundRow
	Totals    []gooTotalLine
	Editable  bool
	Refunded  template.HTML
	HasRefund bool
	NetPay    template.HTML
	Available float64
	AvailFmt  template.HTML
	CanAPI    bool
	APILabel  string
	Products  []gooOpt
	TaxRates  []gooOpt
	Methods   []gooOpt

	Notes      []gooNoteRow
	Actions    []gooOpt
	Downloads  []gooDownloadRow
	DlProducts []gooOpt
	TrashURL   string
	Currency   string
	Symbol     string
}

type gooAddrField struct {
	Key, Name, Label, Value, Type, Class string
	Options                              []gooOpt
}

type gooTaxCol struct {
	RateID string
	Label  string
}

type gooItemRow struct {
	ID          int64
	Type        string
	Name        string
	ProductURL  string
	Thumb       template.HTML
	SKU         string
	VariationID int64
	Meta        []gooKV
	Cost        template.HTML
	Qty         string
	QtyRaw      int
	Subtotal    string
	Total       string
	SubtotalFmt template.HTML
	TotalFmt    template.HTML
	Discount    template.HTML
	Taxes       []gooItemTax
	RefundedQty int
	RefundedAmt template.HTML
	MethodID    string
	Methods     []gooOpt
}

type gooItemTax struct {
	RateID      string
	Total       string
	Subtotal    string
	TotalFmt    template.HTML
	RefundedFmt template.HTML
}

type gooCouponLine struct {
	Code, URL string
	Amount    template.HTML
}

type gooRefundRow struct {
	ID     int64
	Label  string
	Reason string
	Amount template.HTML
}

type gooTotalLine struct {
	Label string
	Value template.HTML
	Class string
}

type gooNoteRow struct {
	ID       int64
	Content  template.HTML
	Class    string
	Date     string
	By       string
	Customer bool
}

type gooDownloadRow struct {
	ID        int64
	Product   string
	File      string
	Remaining string
	Expires   string
	Count     int
}

// gooAdminOrderEdit serves the order editor (GET) and its form actions (POST).
func (s *Server) gooAdminOrderEdit(c *gin.Context) {
	ctx := c.Request.Context()
	var o *gooOrder
	if c.Query("action") == "new" {
		// Like WooCommerce, "Add order" starts an auto-draft order the screen edits.
		o = &gooOrder{Status: "checkout-draft", CreatedVia: "admin", Billing: map[string]string{}, Shipping: map[string]string{},
			Meta: map[string]any{"_admin_draft": true}, PricesIncludeTax: s.gooPricesIncludeTax()}
		if err := s.gooSaveOrder(ctx, o, false); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Redirect(http.StatusFound, gooAdminURL("orders", "action", "edit", "id", itoa64(o.ID)))
		return
	}
	id, _ := strconv.ParseInt(c.Query("id"), 10, 64)
	o, err := s.gooGetOrder(ctx, id)
	if err != nil || o.Type != "shop_order" {
		c.String(http.StatusNotFound, "You attempted to edit an order that does not exist. Perhaps it was deleted?")
		return
	}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, gooStoreNonce) {
			s.gooDeny(c)
			return
		}
		msg, errMsg := s.gooOrderEditPost(c, o)
		gooRedirect(c, gooAdminURL("orders", "action", "edit", "id", itoa64(o.ID)), msg, errMsg)
		return
	}
	d := s.gooOrderEditView(c, o)
	title := "Edit order"
	if d.New {
		title = "Add new order"
	}
	s.gooStoreRender(c, "goocommerce?page=orders", "wc-orders", title, "admin/goo-order-edit", d)
}

var gooBillingFieldKeys = []struct{ Key, Label, Type string }{
	{"first_name", "First name", "text"}, {"last_name", "Last name", "text"}, {"company", "Company", "text"},
	{"address_1", "Address line 1", "text"}, {"address_2", "Address line 2", "text"}, {"city", "City", "text"},
	{"postcode", "Postcode / ZIP", "text"}, {"country", "Country / Region", "country"}, {"state", "State / County", "state"},
	{"email", "Email address", "email"}, {"phone", "Phone", "tel"},
}

func (s *Server) gooAddrFields(prefix string, a map[string]string) []gooAddrField {
	var out []gooAddrField
	for _, f := range gooBillingFieldKeys {
		if prefix == "shipping" && f.Key == "email" {
			continue
		}
		af := gooAddrField{Key: f.Key, Name: "_" + prefix + "_" + f.Key, Label: f.Label, Value: a[f.Key], Type: f.Type, Class: "form-field " + f.Key + "_field"}
		switch f.Key {
		case "first_name", "address_1", "city", "country", "email":
			af.Class += " _" + prefix + "_" + f.Key + "_field"
		}
		if f.Key == "last_name" || f.Key == "address_2" || f.Key == "postcode" || f.Key == "state" || f.Key == "phone" {
			af.Class += " last"
		}
		if f.Type == "country" {
			af.Options = append(af.Options, gooOpt{"", "Select a country / region…"})
			for _, cn := range gooCountryOrder {
				af.Options = append(af.Options, gooOpt{cn.Code, cn.Name})
			}
		}
		if f.Type == "state" {
			if states := gooStates[a["country"]]; len(states) > 0 {
				af.Options = append(af.Options, gooOpt{"", "Select an option…"})
				for _, st := range states {
					af.Options = append(af.Options, gooOpt{st.Code, st.Name})
				}
			}
		}
		out = append(out, af)
	}
	return out
}

// gooOrderTaxRateIDs are the rate ids of the order's tax columns: its tax items, then any rate used by an item.
func gooOrderTaxRateIDs(o *gooOrder) []string {
	var ids []string
	seen := map[string]bool{}
	for _, t := range o.ItemsOf("tax") {
		id := t.MetaString("rate_id")
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, it := range o.Items {
		var keys []string
		for k := range it.Taxes.Total {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				ids = append(ids, k)
			}
		}
	}
	return ids
}

func (s *Server) gooOrderEditView(c *gin.Context, o *gooOrder) *gooOrderEditData {
	ctx := c.Request.Context()
	cur := o.Currency
	d := &gooOrderEditData{O: o, Nonce: s.nonce(c, gooStoreNonce), Currency: cur, Symbol: gooCurrencySymbol(cur)}
	d.New, _ = o.Meta["_admin_draft"].(bool)
	d.Attribution = gooAttributionRows(o)
	d.Title = "Order #" + o.Number() + " details"
	for _, st := range gooOrderStatuses {
		if st.Value != "checkout-draft" {
			d.Statuses = append(d.Statuses, gooOpt{"wc-" + st.Value, st.Label})
		}
	}
	d.Status = "wc-" + o.Status
	if d.New {
		d.Status = "wc-pending"
	}
	dc := o.DateCreated.In(s.loc())
	d.Date, d.Hour, d.Minute, d.Second = dc.Format("2006-01-02"), dc.Format("15"), dc.Format("04"), dc.Format("05")
	if o.CustomerID != 0 {
		d.Customer = itoa64(o.CustomerID)
		if u, err := s.store.UserByID(ctx, o.CustomerID); err == nil {
			d.CustLabel = u.DisplayName + " (#" + itoa64(u.ID) + " – " + u.Email + ")"
		}
		d.ProfileURL = "/wp-admin/user-edit?user_id=" + itoa64(o.CustomerID)
		d.OrdersURL = gooAdminURL("orders", "_customer_user", itoa64(o.CustomerID))
	}
	d.Customers = s.gooCustomerOptions(ctx)
	d.BillingF = s.gooAddrFields("billing", o.Billing)
	d.ShippingF = s.gooAddrFields("shipping", o.Shipping)
	d.BillingHTML = gooFormatAddress(o.Billing)
	d.ShipHTML = gooFormatAddress(o.Shipping)
	d.Gateways = []gooOpt{{"", "N/A"}}
	for _, g := range s.gooGatewaysOrdered() {
		d.Gateways = append(d.Gateways, gooOpt{g.ID(), g.Title()})
	}
	if o.PaymentMethod != "" && s.gooGateway(o.PaymentMethod) == nil {
		d.Gateways = append(d.Gateways, gooOpt{o.PaymentMethod, defaultStr(o.PaymentMethodTitle, o.PaymentMethod)})
	}
	d.Gateways = append(d.Gateways, gooOpt{"other", "Other"})
	d.PayMethod = o.PaymentMethod
	var meta []string
	if o.PaymentMethodTitle != "" {
		line := "Payment via " + o.PaymentMethodTitle
		if o.TransactionID != "" {
			line += " (" + o.TransactionID + ")"
		}
		meta = append(meta, line)
	}
	if o.DatePaid != nil {
		meta = append(meta, "Paid on "+o.DatePaid.In(s.loc()).Format("January 2, 2006 @ 3:04 pm"))
	}
	d.PaidLine = strings.Join(meta, ". ")
	if d.PaidLine != "" {
		d.PaidLine += "."
	}
	d.IP = o.CustomerIP
	d.PayInfo = s.gooPayOrderInfo(ctx, o)
	if o.Status == "pending" || o.Status == "failed" {
		d.PaymentURL = s.gooOrderPayURL(ctx, o)
	}

	// Items.
	d.Editable = !o.IsPaid() && o.Status != "refunded" || o.Status == "pending" || o.Status == "on-hold" || o.Status == "checkout-draft" || o.Status == "failed"
	rateIDs := gooOrderTaxRateIDs(o)
	labels := map[string]string{}
	for _, t := range o.ItemsOf("tax") {
		labels[t.MetaString("rate_id")] = defaultStr(t.MetaString("label"), t.Name)
	}
	for _, id := range rateIDs {
		lbl := labels[id]
		if lbl == "" {
			if n, _ := strconv.ParseInt(id, 10, 64); n > 0 {
				if r := s.gooTaxRateByID(ctx, n); r != nil {
					lbl = r.Name
				}
			}
		}
		d.TaxCols = append(d.TaxCols, gooTaxCol{RateID: id, Label: defaultStr(lbl, "Tax")})
	}
	refunds, _ := s.gooRefunds(ctx, o.ID)
	refQty, refTotal, refTax := map[int64]int{}, map[int64]float64{}, map[int64]map[string]float64{}
	for _, r := range refunds {
		for _, it := range r.Items {
			src := gooRefundSource(o, it)
			refQty[src] += -it.Qty()
			refTotal[src] += -it.Total
			if refTax[src] == nil {
				refTax[src] = map[string]float64{}
			}
			for k, v := range it.Taxes.Total {
				refTax[src][k] += -v
			}
		}
	}
	methodOpts := []gooOpt{{"", "N/A"}}
	for _, m := range s.gooShippingMethodOrder() {
		methodOpts = append(methodOpts, gooOpt{m.ID, m.Title})
	}
	methodOpts = append(methodOpts, gooOpt{"other", "Other"})
	d.Methods = methodOpts
	rowOf := func(it *gooOrderItem) *gooItemRow {
		r := &gooItemRow{ID: it.ID, Type: it.Type, Name: it.Name, VariationID: it.VariationID, Meta: gooItemDisplayMeta(it), QtyRaw: it.Qty(),
			Qty: strconv.FormatFloat(it.Quantity, 'f', -1, 64), Subtotal: s.gooFmtDec(it.Subtotal), Total: s.gooFmtDec(it.Total),
			SubtotalFmt: s.gooPriceIn(it.Subtotal, cur), TotalFmt: s.gooPriceIn(it.Total, cur), RefundedQty: refQty[it.ID]}
		if it.Type == "line_item" {
			unit := 0.0
			if it.Quantity != 0 {
				unit = it.Subtotal / it.Quantity
			}
			r.Cost = s.gooPriceIn(unit, cur)
			if disc := it.Subtotal - it.Total; disc > 0.00001 {
				r.Discount = s.gooPriceIn(disc, cur)
			}
			pid := it.ProductID
			if it.VariationID != 0 {
				pid = it.VariationID
			}
			if p, err := s.gooGetProduct(ctx, pid); err == nil {
				r.SKU = p.SKU
				editID := p.ID
				if p.ParentID != 0 {
					editID = p.ParentID
				}
				r.ProductURL = "/wp-admin/post?post=" + itoa64(editID) + "&action=edit"
				if img := s.gooImageOf(ctx, p); img > 0 {
					if a, err := s.store.GetAttachment(ctx, img); err == nil {
						r.Thumb = template.HTML(`<img width="150" height="150" src="` + template.HTMLEscapeString(s.uploadURL(ctx, a.FilePath)) + `" class="attachment-thumbnail size-thumbnail" alt="">`)
					}
				}
			}
		}
		if it.Type == "shipping" {
			r.MethodID = it.MetaString("method_id")
			r.Methods = methodOpts
		}
		if amt := refTotal[it.ID]; amt != 0 {
			r.RefundedAmt = s.gooPriceIn(-amt, cur)
		}
		for _, col := range d.TaxCols {
			t := gooItemTax{RateID: col.RateID}
			if v, ok := it.Taxes.Total[col.RateID]; ok {
				t.Total = s.gooFmtDec(v)
				t.TotalFmt = s.gooPriceIn(v, cur)
			}
			if v, ok := it.Taxes.Subtotal[col.RateID]; ok {
				t.Subtotal = s.gooFmtDec(v)
			}
			if v := refTax[it.ID][col.RateID]; v != 0 {
				t.RefundedFmt = s.gooPriceIn(-v, cur)
			}
			r.Taxes = append(r.Taxes, t)
		}
		return r
	}
	for _, it := range o.Items {
		switch it.Type {
		case "line_item":
			d.Lines = append(d.Lines, rowOf(it))
		case "shipping":
			d.Shipping = append(d.Shipping, rowOf(it))
		case "fee":
			d.Fees = append(d.Fees, rowOf(it))
		case "coupon":
			cl := gooCouponLine{Code: it.Name, Amount: s.gooPriceIn(it.Total, cur)}
			if cp, err := s.gooCouponByCode(ctx, it.Name); err == nil {
				cl.URL = gooAdminURL("coupons", "action", "edit", "id", itoa64(cp.ID))
			}
			d.Coupons = append(d.Coupons, cl)
		}
	}
	for _, r := range refunds {
		row := gooRefundRow{ID: r.ID, Amount: s.gooPriceIn(r.Total, cur), Reason: r.RefundReason,
			Label: "Refund #" + itoa64(r.ID) + " – " + r.DateCreated.In(s.loc()).Format("January 2, 2006 @ 3:04 pm")}
		if r.RefundedBy != 0 {
			if u, err := s.store.UserByID(ctx, r.RefundedBy); err == nil {
				row.Label += " by " + u.DisplayName
			}
		}
		d.Refunds = append(d.Refunds, row)
	}
	// Totals box (wc-order-totals).
	var fees, couponTotal float64
	for _, f := range o.ItemsOf("fee") {
		fees += f.Total
	}
	for _, cp := range o.ItemsOf("coupon") {
		couponTotal += cp.Total
	}
	d.Totals = append(d.Totals, gooTotalLine{Label: "Items Subtotal:", Value: s.gooPriceIn(o.Subtotal(), cur)})
	if len(d.Coupons) > 0 || o.DiscountTotal > 0 {
		d.Totals = append(d.Totals, gooTotalLine{Label: "Coupon(s):", Value: template.HTML("-" + string(s.gooPriceIn(o.DiscountTotal, cur)))})
	}
	if len(d.Fees) > 0 {
		d.Totals = append(d.Totals, gooTotalLine{Label: "Fees:", Value: s.gooPriceIn(fees, cur)})
	}
	if len(d.Shipping) > 0 {
		d.Totals = append(d.Totals, gooTotalLine{Label: "Shipping:", Value: s.gooPriceIn(o.ShippingTotal, cur)})
	}
	for _, t := range o.ItemsOf("tax") {
		amt := 0.0
		if v, ok := t.Meta["tax_amount"].(float64); ok {
			amt += v
		}
		if v, ok := t.Meta["shipping_tax_amount"].(float64); ok {
			amt += v
		}
		d.Totals = append(d.Totals, gooTotalLine{Label: defaultStr(t.MetaString("label"), t.Name) + ":", Value: s.gooPriceIn(amt, cur)})
	}
	d.Totals = append(d.Totals, gooTotalLine{Label: "Order Total:", Value: s.gooPriceIn(o.Total, cur), Class: "order-total"})
	refunded := s.gooTotalRefunded(ctx, o.ID)
	d.Refunded = template.HTML("-" + string(s.gooPriceIn(refunded, cur)))
	if refunded > 0 {
		d.HasRefund = true
		d.NetPay = s.gooPriceIn(o.Total-refunded, cur)
	}
	d.Available = gooRound(o.Total-refunded, s.gooDecimals())
	d.AvailFmt = s.gooPriceIn(d.Available, cur)
	if gw := s.gooGateway(o.PaymentMethod); gw != nil && gw.Supports("refunds") {
		d.CanAPI, d.APILabel = true, gw.Title()
	}
	d.Products = s.gooProductOptions(ctx, false)
	for _, cl := range s.gooTaxClasses() {
		rates, _ := s.gooTaxRates(ctx, cl.Value)
		for _, r := range rates {
			lbl := gooTaxRateCode(&r) + " (" + strconv.FormatFloat(r.Rate, 'f', -1, 64) + "%)"
			d.TaxRates = append(d.TaxRates, gooOpt{itoa64(r.ID), lbl})
		}
	}
	// Notes.
	notes, _ := s.gooOrderNotes(ctx, o.ID, false)
	for _, n := range notes {
		row := gooNoteRow{ID: n.ID, Content: template.HTML(wp.KsesPost(n.Content)), Customer: n.IsCustomer,
			Date: "added on " + n.CreatedAt.In(s.loc()).Format("January 2, 2006 at 3:04 pm"), By: n.AddedBy}
		row.Class = "note"
		if n.IsCustomer {
			row.Class += " customer-note"
		}
		if n.AddedBy == "" {
			row.Class += " system-note"
		}
		d.Notes = append(d.Notes, row)
	}
	// Order actions.
	d.Actions = []gooOpt{{"send_order_details", "Send order details to customer"}, {"send_order_details_admin", "Resend new order notification"},
		{"regenerate_download_permissions", "Regenerate download permissions"}}
	for _, a := range gooOrderActions {
		if a.Module == "" || s.gooExtOn(a.Module) {
			d.Actions = append(d.Actions, gooOpt{a.ID, a.Label})
		}
	}
	d.Actions = append(d.Actions, s.gooPayOrderActions(ctx, o)...)
	d.TrashURL = gooAdminURL("orders", "action", "trash", "id", itoa64(o.ID), "_wpnonce", d.Nonce)
	// Downloads.
	rows, err := s.store.Pool.Query(ctx, `SELECT id, download_id, product_id, downloads_remaining, access_expires, download_count
		FROM goo_download_permissions WHERE order_id = $1 ORDER BY product_id, id`, o.ID)
	if err == nil {
		type perm struct {
			id, pid int64
			dl      string
			rem     *int
			exp     *time.Time
			count   int
		}
		var perms []perm
		for rows.Next() {
			var p perm
			if rows.Scan(&p.id, &p.dl, &p.pid, &p.rem, &p.exp, &p.count) == nil {
				perms = append(perms, p)
			}
		}
		rows.Close()
		for _, p := range perms {
			row := gooDownloadRow{ID: p.id, Count: p.count, Remaining: "Unlimited", Expires: "Never"}
			if pr, err := s.gooGetProduct(ctx, p.pid); err == nil {
				row.Product = s.gooProductTitle(ctx, pr)
				for _, f := range pr.Downloads {
					if f.ID == p.dl {
						row.File = f.Name
					}
				}
			}
			if row.File == "" {
				row.File = p.dl
			}
			if p.rem != nil {
				row.Remaining = strconv.Itoa(*p.rem)
			}
			if p.exp != nil {
				row.Expires = p.exp.In(s.loc()).Format("2006-01-02")
			}
			d.Downloads = append(d.Downloads, row)
		}
	}
	d.DlProducts = s.gooProductOptions(ctx, true)
	return d
}

// gooOrderEditPost applies the order form and runs the pressed button (goo_do).
func (s *Server) gooOrderEditPost(c *gin.Context, o *gooOrder) (string, string) {
	ctx := c.Request.Context()
	do := c.PostForm("goo_do")
	by := s.gooAdminUserName(c)
	var uid int64
	if u := currentUser(c); u != nil {
		uid = u.ID
	}
	// Notes, refunds and permissions don't touch the rest of the form.
	switch {
	case do == "add_note":
		note := strings.TrimSpace(c.PostForm("order_note"))
		if note == "" {
			return "", "Please enter a note."
		}
		_ = s.gooAddOrderNote(ctx, o, strings.ReplaceAll(gooEsc(note), "\n", "<br>"), c.PostForm("order_note_type") == "customer", by)
		return "Note added.", ""
	case strings.HasPrefix(do, "delete_note:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(do, "delete_note:"), 10, 64)
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_order_notes WHERE id = $1 AND order_id = $2`, id, o.ID)
		return "Note deleted.", ""
	case do == "refund" || do == "refund_api":
		return s.gooOrderRefundPost(c, o, do == "refund_api", uid)
	case strings.HasPrefix(do, "delete_refund:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(do, "delete_refund:"), 10, 64)
		var parent int64
		_ = s.store.Pool.QueryRow(ctx, `SELECT parent_id FROM goo_orders WHERE id = $1 AND type = 'shop_order_refund'`, id).Scan(&parent)
		if parent != o.ID {
			return "", "Invalid refund."
		}
		_ = s.gooDeleteRefund(ctx, id)
		return "Refund deleted.", ""
	case do == "grant_access":
		n := 0
		for _, raw := range c.PostFormArray("grant_access_id[]") {
			pid, _ := strconv.ParseInt(raw, 10, 64)
			n += s.gooGrantProductAccess(ctx, o, pid)
		}
		if n == 0 {
			return "", "Could not grant access - the user may already have permission for this file or billing email is not set. Ensure the billing email is set, and the order has been saved."
		}
		return "Access granted.", ""
	case strings.HasPrefix(do, "revoke_access:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(do, "revoke_access:"), 10, 64)
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_download_permissions WHERE id = $1 AND order_id = $2`, id, o.ID)
		return "Access revoked.", ""
	}

	// Everything else saves the posted details and items first.
	s.gooApplyOrderForm(c, o)
	s.gooApplyItemEdits(c, o)
	msg, errMsg := "", ""
	switch {
	case do == "add_item":
		pid, _ := strconv.ParseInt(c.PostForm("add_item_id"), 10, 64)
		qty, _ := strconv.Atoi(c.PostForm("add_item_qty"))
		if err := s.gooOrderAddProduct(ctx, o, pid, max(qty, 1)); err != nil {
			errMsg = err.Error()
		} else {
			s.gooOrderCalcTaxes(ctx, o)
			msg = "Item added."
		}
	case do == "add_fee":
		raw := strings.TrimSpace(c.PostForm("add_fee_amount"))
		amt := 0.0
		name := ""
		if strings.HasSuffix(raw, "%") {
			pct := s.gooFloat(strings.TrimSuffix(raw, "%"))
			var base float64
			for _, it := range o.ItemsOf("line_item") {
				base += it.Total
			}
			amt = gooRound(base*pct/100, s.gooDecimals())
			name = strings.TrimSuffix(raw, "%") + "% fee"
		} else {
			amt = s.gooFloat(raw)
			name = s.gooPricePlain(amt, o.Currency) + " fee"
		}
		if raw == "" {
			errMsg = "Enter a fixed amount or percentage to apply as a fee."
			break
		}
		o.Items = append(o.Items, &gooOrderItem{Type: "fee", Name: name, Quantity: 1, Total: amt, Subtotal: amt, Meta: map[string]any{}})
		s.gooOrderCalcTaxes(ctx, o)
		msg = "Fee added."
	case do == "add_shipping":
		o.Items = append(o.Items, &gooOrderItem{Type: "shipping", Name: "Shipping", Quantity: 1, Meta: map[string]any{"method_id": ""}})
		msg = "Shipping line added."
	case do == "add_tax":
		rid, _ := strconv.ParseInt(c.PostForm("add_tax_rate"), 10, 64)
		r := s.gooTaxRateByID(ctx, rid)
		if r == nil {
			errMsg = "Invalid tax rate."
			break
		}
		exists := false
		for _, t := range o.ItemsOf("tax") {
			if t.MetaString("rate_id") == itoa64(rid) {
				exists = true
			}
		}
		if !exists {
			o.Items = append(o.Items, &gooOrderItem{Type: "tax", Name: gooTaxRateCode(r), Quantity: 1, Meta: map[string]any{"rate_id": float64(r.ID),
				"label": r.Name, "compound": r.Compound, "tax_amount": 0.0, "shipping_tax_amount": 0.0, "rate_percent": r.Rate}})
		}
		s.gooSumTaxItems(o)
		msg = "Tax rate added."
	case do == "recalculate":
		// WC's Recalculate is calculate_totals(): the line totals (edited by hand or discounted by the
		// coupons) are kept; only an order with coupons has its discounts re-derived from them.
		if len(o.ItemsOf("coupon")) > 0 {
			s.gooOrderApplyCoupons(ctx, o)
		}
		s.gooOrderCalcTaxes(ctx, o)
		msg = "Order totals recalculated."
	case do == "apply_coupon":
		code := strings.ToLower(strings.TrimSpace(c.PostForm("coupon_code")))
		cp, err := s.gooCouponByCode(ctx, code)
		if code == "" || err != nil {
			errMsg = fmt.Sprintf(`Coupon "%s" does not exist!`, code)
			break
		}
		if cp.DateExpires != nil && time.Now().After(*cp.DateExpires) {
			errMsg = "This coupon has expired."
			break
		}
		for _, it := range o.ItemsOf("coupon") {
			if strings.EqualFold(it.Name, cp.Code) {
				errMsg = "Coupon code already applied!"
			}
		}
		if errMsg != "" {
			break
		}
		o.Items = append(o.Items, &gooOrderItem{Type: "coupon", Name: cp.Code, Quantity: 1, Meta: map[string]any{"coupon_id": float64(cp.ID)}})
		s.gooOrderApplyCoupons(ctx, o)
		s.gooOrderCalcTaxes(ctx, o)
		_ = s.gooAddOrderNote(ctx, o, "Coupon applied: \""+gooEsc(cp.Code)+"\".", false, by)
		msg = "Coupon applied."
	case strings.HasPrefix(do, "remove_coupon:"):
		code := strings.TrimPrefix(do, "remove_coupon:")
		var keep []*gooOrderItem
		for _, it := range o.Items {
			if it.Type == "coupon" && strings.EqualFold(it.Name, code) {
				continue
			}
			keep = append(keep, it)
		}
		o.Items = keep
		s.gooOrderApplyCoupons(ctx, o)
		s.gooOrderCalcTaxes(ctx, o)
		_ = s.gooAddOrderNote(ctx, o, "Coupon removed: \""+gooEsc(code)+"\".", false, by)
		msg = "Coupon removed."
	case strings.HasPrefix(do, "delete_item:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(do, "delete_item:"), 10, 64)
		var keep []*gooOrderItem
		for _, it := range o.Items {
			if it.ID == id {
				if it.Type == "line_item" {
					s.gooRestoreItemStock(ctx, o, it)
				}
				continue
			}
			keep = append(keep, it)
		}
		o.Items = keep
		s.gooSumTaxItems(o)
		msg = "Item deleted."
	}
	s.gooRecalculate(o)
	if err := s.gooSaveOrder(ctx, o, true); err != nil {
		return "", err.Error()
	}
	if do != "save" && do != "" {
		return msg, errMsg
	}

	// Create / Update: the status change (stock, coupons, e-mails) and the order action.
	wasNew, _ := o.Meta["_admin_draft"].(bool)
	if wasNew {
		delete(o.Meta, "_admin_draft")
		o.Status = "pending"
		_ = s.gooSaveOrder(ctx, o, false)
		s.gooFire(ctx, "order_created", o)
	}
	to := strings.TrimPrefix(c.PostForm("order_status"), "wc-")
	if to != "" && to != o.Status {
		if err := s.gooSetStatus(ctx, o, to, "", by); err != nil {
			return "", err.Error()
		}
	}
	if act := c.PostForm("wc_order_action"); act != "" {
		if err := s.gooRunOrderAction(c, o, act); err != nil {
			return "", err.Error()
		}
		if m, _ := c.Get(gooOrderActionNoticeKey); m != nil && !wasNew {
			if txt, _ := m.(string); txt != "" {
				return txt, ""
			}
		}
	}
	if wasNew {
		return "Order created.", ""
	}
	return "Order updated.", ""
}

// gooApplyOrderForm reads the General / Billing / Shipping fields.
func (s *Server) gooApplyOrderForm(c *gin.Context, o *gooOrder) {
	if v := c.PostForm("order_date"); v != "" {
		h, _ := strconv.Atoi(c.PostForm("order_date_hour"))
		m, _ := strconv.Atoi(c.PostForm("order_date_minute"))
		sec, _ := strconv.Atoi(c.PostForm("order_date_second"))
		if t, err := time.ParseInLocation("2006-01-02", v, s.loc()); err == nil {
			o.DateCreated = time.Date(t.Year(), t.Month(), t.Day(), min(max(h, 0), 23), min(max(m, 0), 59), min(max(sec, 0), 59), 0, s.loc())
		}
	}
	if _, ok := c.GetPostForm("customer_user"); ok {
		o.CustomerID, _ = strconv.ParseInt(c.PostForm("customer_user"), 10, 64)
	}
	for _, f := range gooBillingFieldKeys {
		if v, ok := c.GetPostForm("_billing_" + f.Key); ok {
			o.Billing[f.Key] = strings.TrimSpace(v)
		}
		if f.Key == "email" {
			continue
		}
		if v, ok := c.GetPostForm("_shipping_" + f.Key); ok {
			o.Shipping[f.Key] = strings.TrimSpace(v)
		}
	}
	for _, m := range []map[string]string{o.Billing, o.Shipping} {
		for k, v := range m {
			if v == "" {
				delete(m, k)
			}
		}
	}
	if v, ok := c.GetPostForm("_payment_method"); ok {
		o.PaymentMethod = v
		if g := s.gooGateway(v); g != nil {
			o.PaymentMethodTitle = g.Title()
		} else if v == "other" || v == "" {
			o.PaymentMethodTitle = ""
			if v == "other" {
				o.PaymentMethodTitle = "Other"
			}
		}
	}
	if v, ok := c.GetPostForm("_transaction_id"); ok {
		o.TransactionID = strings.TrimSpace(v)
	}
	if v, ok := c.GetPostForm("customer_note"); ok {
		o.CustomerNote = v
	}
}

// gooApplyItemEdits reads the editable item fields of the items box (quantities, costs, taxes, names).
func (s *Server) gooApplyItemEdits(c *gin.Context, o *gooOrder) {
	form := c.Request.PostForm
	dp := s.gooDecimals()
	for _, it := range o.Items {
		id := itoa64(it.ID)
		if _, ok := form["order_item_id[]"]; !ok {
			return
		}
		if !containsStr(form["order_item_id[]"], id) {
			continue
		}
		if v, ok := form["order_item_name["+id+"]"]; ok && it.Type != "line_item" {
			it.Name = strings.TrimSpace(v[0])
		}
		if v, ok := form["order_item_qty["+id+"]"]; ok {
			if q, err := strconv.ParseFloat(strings.TrimSpace(v[0]), 64); err == nil {
				it.Quantity = q
			}
		}
		total := "line_total[" + id + "]"
		if it.Type == "shipping" {
			total = "shipping_cost[" + id + "]"
			if v, ok := form["shipping_method["+id+"]"]; ok {
				it.Meta["method_id"] = v[0]
			}
			if v, ok := form["shipping_method_title["+id+"]"]; ok {
				it.Name = strings.TrimSpace(v[0])
			}
		}
		if v, ok := form[total]; ok {
			it.Total = gooRound(s.gooFloat(v[0]), dp)
		}
		if v, ok := form["line_subtotal["+id+"]"]; ok {
			it.Subtotal = gooRound(s.gooFloat(v[0]), dp)
		} else if it.Type != "line_item" {
			it.Subtotal = it.Total
		}
		taxKey := "line_tax[" + id + "]["
		if it.Type == "shipping" {
			taxKey = "shipping_taxes[" + id + "]["
		}
		for k, v := range form {
			if rate, ok := strings.CutPrefix(k, taxKey); ok {
				rate = strings.TrimSuffix(rate, "]")
				if it.Taxes.Total == nil {
					it.Taxes.Total = map[string]float64{}
				}
				if strings.TrimSpace(v[0]) == "" {
					delete(it.Taxes.Total, rate)
				} else {
					it.Taxes.Total[rate] = gooRound(s.gooFloat(v[0]), dp)
				}
			}
			if rate, ok := strings.CutPrefix(k, "line_subtotal_tax["+id+"]["); ok {
				rate = strings.TrimSuffix(rate, "]")
				if it.Taxes.Subtotal == nil {
					it.Taxes.Subtotal = map[string]float64{}
				}
				it.Taxes.Subtotal[rate] = gooRound(s.gooFloat(v[0]), dp)
			}
		}
		it.TotalTax, it.SubtotalTax = 0, 0
		for _, v := range it.Taxes.Total {
			it.TotalTax += v
		}
		for _, v := range it.Taxes.Subtotal {
			it.SubtotalTax += v
		}
		if len(it.Taxes.Subtotal) == 0 {
			it.SubtotalTax = it.TotalTax
		}
	}
	s.gooSumTaxItems(o)
}

// gooOrderAddProduct adds a product line (Add item(s)); stock is reduced at once when the order's already was.
func (s *Server) gooOrderAddProduct(ctx context.Context, o *gooOrder, pid int64, qty int) error {
	p, err := s.gooGetProduct(ctx, pid)
	if err != nil {
		return fmt.Errorf("Invalid product.")
	}
	// Order lines are ex-tax: WC_Order::add_product() uses wc_get_price_excluding_tax().
	line := s.gooPriceExcludingTax(ctx, p, p.PriceValue(), qty, s.gooOrderTaxLocation(o))
	it := &gooOrderItem{Type: "line_item", Name: s.gooProductTitle(ctx, p), ProductID: p.ID, Quantity: float64(qty), TaxClass: s.gooTaxClassOf(ctx, p),
		Subtotal: gooRound(line, s.gooDecimals()), Meta: map[string]any{}}
	it.Total = it.Subtotal
	if p.Type == gooTypeVariation {
		it.ProductID, it.VariationID = p.ParentID, p.ID
		for k, v := range p.VariationAttrs {
			it.Meta[k] = v
		}
		if sum := s.gooVariationSummary(ctx, p); sum != "" {
			it.Name += " - " + sum
		}
	}
	if o.StockReduced && s.gooManagingStock(ctx, p) {
		if _, err := s.gooUpdateStock(ctx, p, -qty, false); err == nil {
			it.Meta["_reduced_stock"] = float64(qty)
		}
	}
	o.Items = append(o.Items, it)
	return nil
}

// gooRestoreItemStock puts back the stock a deleted line had reduced.
func (s *Server) gooRestoreItemStock(ctx context.Context, o *gooOrder, it *gooOrderItem) {
	red, _ := it.Meta["_reduced_stock"].(float64)
	if red <= 0 {
		return
	}
	pid := it.ProductID
	if it.VariationID != 0 {
		pid = it.VariationID
	}
	if p, err := s.gooGetProduct(ctx, pid); err == nil {
		_, _ = s.gooUpdateStock(ctx, p, int(red), false)
	}
}

// gooOrderTaxLocation is the order's tax location (Calculate tax based on).
func (s *Server) gooOrderTaxLocation(o *gooOrder) gooLocation {
	addr := o.Billing
	switch s.gooOptString("tax_based_on") {
	case "base":
		return s.gooBaseLocation()
	case "shipping":
		if o.Shipping["country"] != "" {
			addr = o.Shipping
		}
	}
	if addr["country"] == "" {
		return s.gooBaseLocation()
	}
	return gooLocation{Country: addr["country"], State: addr["state"], Postcode: addr["postcode"], City: addr["city"]}
}

// gooOrderCalcTaxes is WC_Order::calculate_taxes() for the admin's Recalculate: item taxes from the
// rates of the order's location (gooFindRates), then the tax lines.
func (s *Server) gooOrderCalcTaxes(ctx context.Context, o *gooOrder) {
	dp := s.gooDecimals()
	loc := s.gooOrderTaxLocation(o)
	conv := func(m map[int64]float64) map[string]float64 {
		out := map[string]float64{}
		for k, v := range m {
			out[itoa64(k)] = gooRound(v, dp)
		}
		return out
	}
	shipClass := s.gooOptString("shipping_tax_class")
	if shipClass == "inherit" {
		shipClass = ""
		for _, it := range o.ItemsOf("line_item") {
			if it.TaxClass != "" {
				shipClass = it.TaxClass
				break
			}
		}
	}
	for _, it := range o.Items {
		switch it.Type {
		case "line_item", "fee":
			taxable := true
			if it.Type == "line_item" {
				pid := it.ProductID
				if it.VariationID != 0 {
					pid = it.VariationID
				}
				if p, err := s.gooGetProduct(ctx, pid); err == nil {
					taxable = p.TaxStatus == "taxable" || p.TaxStatus == ""
				}
			}
			var rates []gooTaxRate
			if taxable {
				rates = s.gooFindRates(ctx, it.TaxClass, loc, false)
			}
			it.Taxes.Total = conv(gooCalcTax(it.Total, rates, false))
			it.Taxes.Subtotal = conv(gooCalcTax(it.Subtotal, rates, false))
		case "shipping":
			rates := s.gooFindRates(ctx, shipClass, loc, true)
			it.Taxes.Total = conv(gooCalcTax(it.Total, rates, false))
			it.Taxes.Subtotal = nil
		default:
			continue
		}
		it.TotalTax, it.SubtotalTax = 0, 0
		for _, v := range it.Taxes.Total {
			it.TotalTax += v
		}
		for _, v := range it.Taxes.Subtotal {
			it.SubtotalTax += v
		}
		if it.Type != "line_item" {
			it.SubtotalTax = it.TotalTax
		}
		it.TotalTax, it.SubtotalTax = gooRound(it.TotalTax, dp), gooRound(it.SubtotalTax, dp)
	}
	// Rebuild the tax lines from the item taxes.
	var keep []*gooOrderItem
	for _, it := range o.Items {
		if it.Type != "tax" {
			keep = append(keep, it)
		}
	}
	o.Items = keep
	used := map[string]bool{}
	var ids []string
	for _, it := range o.Items {
		for k := range it.Taxes.Total {
			if !used[k] {
				used[k] = true
				ids = append(ids, k)
			}
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		n, _ := strconv.ParseInt(id, 10, 64)
		r := s.gooTaxRateByID(ctx, n)
		if r == nil {
			r = &gooTaxRate{ID: n, Name: "Tax"}
		}
		o.Items = append(o.Items, &gooOrderItem{Type: "tax", Name: gooTaxRateCode(r), Quantity: 1, Meta: map[string]any{"rate_id": float64(r.ID),
			"label": r.Name, "compound": r.Compound, "rate_percent": r.Rate}})
	}
	s.gooSumTaxItems(o)
}

// gooSumTaxItems sets each tax line's tax_amount / shipping_tax_amount from the item taxes.
func (s *Server) gooSumTaxItems(o *gooOrder) {
	dp := s.gooDecimals()
	for _, t := range o.ItemsOf("tax") {
		id := t.MetaString("rate_id")
		var tax, ship float64
		for _, it := range o.Items {
			v := it.Taxes.Total[id]
			if it.Type == "shipping" {
				ship += v
			} else if it.Type == "line_item" || it.Type == "fee" {
				tax += v
			}
		}
		t.Meta["tax_amount"], t.Meta["shipping_tax_amount"] = gooRound(tax, dp), gooRound(ship, dp)
	}
}

// gooOrderApplyCoupons recomputes line totals from subtotals with the order's coupons (percent,
// fixed cart split by line subtotal, fixed product per unit), honouring product / category restrictions.
func (s *Server) gooOrderApplyCoupons(ctx context.Context, o *gooOrder) {
	dp := s.gooDecimals()
	lines := o.ItemsOf("line_item")
	for _, it := range lines {
		it.Total = it.Subtotal
	}
	sequential := s.gooOptBool("calc_discounts_sequentially")
	// When the order's prices include tax, coupon amounts are in tax-inclusive terms like in the cart
	// (WC_Discounts::set_items_from_order adds the subtotal tax): the discounts are worked out on gross
	// amounts and turned back into ex-tax amounts with each line's own tax factor.
	factor := map[*gooOrderItem]float64{}
	loc := s.gooOrderTaxLocation(o)
	for _, it := range lines {
		f := 1.0
		if o.PricesIncludeTax {
			pid := it.ProductID
			if it.VariationID != 0 {
				pid = it.VariationID
			}
			taxable := true
			if p, err := s.gooGetProduct(ctx, pid); err == nil {
				taxable = p.TaxStatus == "taxable" || p.TaxStatus == ""
			}
			if taxable {
				f += gooSumTaxes(gooCalcTax(1, s.gooFindRates(ctx, it.TaxClass, loc, false), false))
			}
		}
		factor[it] = f
	}
	for _, ci := range o.ItemsOf("coupon") {
		cp, err := s.gooCouponByCode(ctx, ci.Name)
		gone := err != nil
		if gone {
			// The coupon was deleted since the order was placed: its recorded discount stays, spread over
			// the lines by their share, so the coupon line and the line totals keep agreeing.
			if ci.Total <= 0 {
				continue
			}
			amount := ci.Total
			if o.PricesIncludeTax {
				amount += ci.TotalTax
			}
			cp = &gooCoupon{DiscountType: "fixed_cart", Amount: amount}
		}
		var eligible []*gooOrderItem
		var base float64
		for _, it := range lines {
			if gone || s.gooCouponValidForItem(ctx, cp, it) {
				eligible = append(eligible, it)
				if sequential {
					base += it.Total * factor[it]
				} else {
					base += it.Subtotal * factor[it]
				}
			}
		}
		type gooCouponShare struct {
			it     *gooOrderItem
			before float64
			disc   float64
		}
		var shares []gooCouponShare
		var rawSum, total float64
		for _, it := range eligible {
			f := factor[it]
			ref := it.Subtotal * f
			if sequential {
				ref = it.Total * f
			}
			var disc float64
			switch cp.DiscountType {
			case "percent":
				disc = ref * cp.Amount / 100
			case "fixed_product":
				disc = cp.Amount * it.Quantity
			default: // fixed_cart
				if base > 0 {
					disc = cp.Amount * ref / base
				}
			}
			raw := disc / f // gross → ex-tax
			if raw > it.Total {
				raw = it.Total
			}
			if raw < 0 {
				raw = 0
			}
			rounded := gooRound(raw, dp)
			shares = append(shares, gooCouponShare{it: it, before: it.Total, disc: rounded})
			rawSum += raw
			total += rounded
		}
		// Rounding each line's share drops or adds a cent (10.00 across three equal lines is
		// 3.33+3.33+3.33). The cart gives those cents back; Recalculate has to as well, or the
		// order total changes by a cent when nothing else did.
		unit := 1.0
		for i := 0; i < dp; i++ {
			unit /= 10
		}
		short := gooRound(gooRound(rawSum, dp)-total, dp)
		for gooRound(short, dp) != 0 {
			moved := false
			for i := range shares {
				if short >= unit && shares[i].before-shares[i].disc >= unit {
					shares[i].disc = gooRound(shares[i].disc+unit, dp)
					short = gooRound(short-unit, dp)
					total = gooRound(total+unit, dp)
					moved = true
				} else if short <= -unit && shares[i].disc >= unit {
					shares[i].disc = gooRound(shares[i].disc-unit, dp)
					short = gooRound(short+unit, dp)
					total = gooRound(total-unit, dp)
					moved = true
				}
				if gooRound(short, dp) == 0 {
					break
				}
			}
			if !moved {
				break
			}
		}
		for _, sh := range shares {
			sh.it.Total = gooRound(sh.before-sh.disc, dp)
		}
		ci.Total = gooRound(total, dp)
		ci.Subtotal = ci.Total
		ci.Meta["discount_amount"] = ci.Total
		if !gone {
			ci.Meta["coupon_id"] = float64(cp.ID)
		}
	}
}

func (s *Server) gooCouponValidForItem(ctx context.Context, cp *gooCoupon, it *gooOrderItem) bool {
	ids := s.gooCouponItemIDs(ctx, it.ProductID, it.VariationID)
	var cats []int64
	if terms, err := s.store.TermsForPosts(ctx, []int64{it.ProductID}, "product_cat"); err == nil {
		for _, t := range terms[it.ProductID] {
			cats = append(cats, t.ID)
			if t.ParentID > 0 { // like the cart: a parent category's coupon covers its children
				cats = append(cats, t.ParentID)
			}
		}
	}
	cats = s.gooCouponItemCats(ctx, cats)
	valid := len(cp.ProductIDs) == 0 && len(cp.ProductCategories) == 0
	for _, id := range ids {
		if gooInt64In(id, cp.ProductIDs) {
			valid = true
		}
		if gooInt64In(id, cp.ExcludedProductIDs) {
			return false
		}
	}
	for _, cat := range cats {
		if gooInt64In(cat, cp.ProductCategories) {
			valid = true
		}
		if gooInt64In(cat, cp.ExcludedProductCategories) {
			return false
		}
	}
	if cp.ExcludeSaleItems {
		pid := it.ProductID
		if it.VariationID != 0 {
			pid = it.VariationID
		}
		if p, err := s.gooGetProduct(ctx, pid); err == nil && p.IsOnSale() {
			return false
		}
	}
	return valid
}

// gooRefundSource finds the order item a refund line refunds: by _refunded_item_id, else (the order's
// items were rewritten since, which gives them new ids) by type, product and name.
func gooRefundSource(o *gooOrder, ri *gooOrderItem) int64 {
	if v, ok := ri.Meta["_refunded_item_id"].(float64); ok {
		for _, it := range o.Items {
			if it.ID == int64(v) {
				return it.ID
			}
		}
	}
	for _, it := range o.Items {
		if it.Type == ri.Type && it.ProductID == ri.ProductID && it.VariationID == ri.VariationID && it.Name == ri.Name {
			return it.ID
		}
	}
	return 0
}

// gooOrderRefundPost creates a refund from the refund UI.
func (s *Server) gooOrderRefundPost(c *gin.Context, o *gooOrder, api bool, uid int64) (string, string) {
	ctx := c.Request.Context()
	form := c.Request.PostForm
	var lines []gooRefundLine
	var sum float64
	for _, it := range o.Items {
		id := itoa64(it.ID)
		l := gooRefundLine{ItemID: it.ID, TaxTotal: map[string]float64{}}
		l.Qty, _ = strconv.Atoi(strings.TrimSpace(c.PostForm("refund_order_item_qty[" + id + "]")))
		l.Total = s.gooFloat(c.PostForm("refund_line_total[" + id + "]"))
		for k, v := range form {
			if rate, ok := strings.CutPrefix(k, "refund_line_tax["+id+"]["); ok {
				if amt := s.gooFloat(v[0]); amt != 0 {
					l.TaxTotal[strings.TrimSuffix(rate, "]")] = amt
				}
			}
		}
		if l.Qty == 0 && l.Total == 0 && len(l.TaxTotal) == 0 {
			continue
		}
		sum += l.Total
		for _, v := range l.TaxTotal {
			sum += v
		}
		lines = append(lines, l)
	}
	amount := sum
	if raw := strings.TrimSpace(c.PostForm("refund_amount")); raw != "" {
		amount = s.gooFloat(raw)
	}
	_, err := s.gooCreateRefund(ctx, o, amount, strings.TrimSpace(c.PostForm("refund_reason")), lines, c.PostForm("restock_refunded_items") != "", api, uid)
	if err != nil {
		return "", err.Error()
	}
	return "Refund created.", ""
}

// gooGrantProductAccess grants the order access to a downloadable product's files.
func (s *Server) gooGrantProductAccess(ctx context.Context, o *gooOrder, pid int64) int {
	p, err := s.gooGetProduct(ctx, pid)
	if err != nil || !p.Downloadable || o.Billing["email"] == "" {
		return 0
	}
	n := 0
	for _, f := range p.Downloads {
		var exists bool
		_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM goo_download_permissions WHERE order_id = $1 AND product_id = $2 AND download_id = $3)`, o.ID, p.ID, f.ID).Scan(&exists)
		if exists {
			continue
		}
		var remaining any
		if p.DownloadLimit > 0 {
			remaining = p.DownloadLimit
		}
		var expires any
		if p.DownloadExpiry > 0 {
			expires = time.Now().AddDate(0, 0, p.DownloadExpiry)
		}
		if _, err := s.store.Pool.Exec(ctx, `INSERT INTO goo_download_permissions (download_id, product_id, order_id, order_key, user_email, user_id,
			downloads_remaining, access_expires) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, f.ID, p.ID, o.ID, o.OrderKey, o.Billing["email"], o.CustomerID, remaining, expires); err == nil {
			n++
		}
	}
	return n
}

// gooRunOrderAction runs an entry of the Order actions box.
func (s *Server) gooRunOrderAction(c *gin.Context, o *gooOrder, act string) error {
	ctx := c.Request.Context()
	by := s.gooAdminUserName(c)
	if handled, err := s.gooPayRunOrderAction(c, o, act); handled {
		return err
	}
	switch act {
	case "send_order_details":
		if err := s.gooTriggerEmail(ctx, "customer_invoice", o, nil); err != nil {
			return err
		}
		return s.gooAddOrderNote(ctx, o, "Order details manually sent to customer.", false, by)
	case "send_order_details_admin":
		if err := s.gooTriggerEmail(ctx, "new_order", o, nil); err != nil {
			return err
		}
		return s.gooAddOrderNote(ctx, o, "Order details manually sent to admin.", false, by)
	case "regenerate_download_permissions":
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_download_permissions WHERE order_id = $1`, o.ID)
		_, _ = s.gooClaimOrderFlag(ctx, o, "downloads_granted", false)
		s.gooGrantDownloads(ctx, o)
		return nil
	}
	for _, a := range gooOrderActions {
		if a.ID == act && (a.Module == "" || s.gooExtOn(a.Module)) && a.Run != nil {
			return a.Run(s, c, o)
		}
	}
	return nil
}
