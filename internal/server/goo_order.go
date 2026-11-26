package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wpgo/internal/store"
)

// Orders (WC_Order, High-Performance Order Storage): the order row, its items (line items, shipping,
// fees, coupons, tax lines), notes, status transitions with stock and coupon bookkeeping, refunds.

var gooOrderStatuses = []gooOpt{
	{"pending", "Pending payment"}, {"processing", "Processing"}, {"on-hold", "On hold"}, {"completed", "Completed"},
	{"cancelled", "Cancelled"}, {"refunded", "Refunded"}, {"failed", "Failed"}, {"checkout-draft", "Draft"},
}

// gooStatusLabel is wc_get_order_status_name().
func gooStatusLabel(st string) string {
	st = strings.TrimPrefix(st, "wc-")
	for _, o := range gooOrderStatuses {
		if o.Value == st {
			return o.Label
		}
	}
	return st
}

// gooPaidStatuses are wc_get_is_paid_statuses().
var gooPaidStatuses = []string{"processing", "completed"}

type gooOrderItem struct {
	ID          int64          `json:"id"`
	OrderID     int64          `json:"order_id"`
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	ProductID   int64          `json:"product_id"`
	VariationID int64          `json:"variation_id"`
	Quantity    float64        `json:"quantity"`
	TaxClass    string         `json:"tax_class"`
	Subtotal    float64        `json:"subtotal"`
	SubtotalTax float64        `json:"subtotal_tax"`
	Total       float64        `json:"total"`
	TotalTax    float64        `json:"total_tax"`
	Taxes       gooItemTaxes   `json:"taxes"`
	Meta        map[string]any `json:"meta_data"`
	Position    int            `json:"-"`
	origReduced any            // _reduced_stock as stored (gooOrderItem.keepsReduced)
	hasOrig     bool
}

// gooItemTaxes are an item's taxes per rate id.
type gooItemTaxes struct {
	Total    map[string]float64 `json:"total"`
	Subtotal map[string]float64 `json:"subtotal"`
}

// MetaString reads a string meta value.
func (it *gooOrderItem) MetaString(k string) string {
	switch v := it.Meta[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// Qty is the quantity as an int.
func (it *gooOrderItem) Qty() int { return int(it.Quantity) }

type gooOrderNote struct {
	ID         int64
	OrderID    int64
	Content    string
	IsCustomer bool
	AddedBy    string
	CreatedAt  time.Time
}

type gooOrder struct {
	ID                 int64
	Type               string // shop_order, shop_order_refund
	ParentID           int64
	Status             string
	Currency           string
	PricesIncludeTax   bool
	CustomerID         int64
	Billing            map[string]string
	Shipping           map[string]string
	PaymentMethod      string
	PaymentMethodTitle string
	TransactionID      string
	CustomerIP         string
	UserAgent          string
	CustomerNote       string
	CreatedVia         string
	CartHash           string
	OrderKey           string
	DiscountTotal      float64
	DiscountTax        float64
	ShippingTotal      float64
	ShippingTax        float64
	CartTax            float64
	Total              float64
	TotalTax           float64
	RefundReason       string
	RefundedBy         int64
	StockReduced       bool
	DownloadsGranted   bool
	NewOrderEmailSent  bool
	Meta               map[string]any
	DateCreated        time.Time
	DateModified       time.Time
	DatePaid           *time.Time
	DateCompleted      *time.Time

	Items []*gooOrderItem

	orig *gooOrderSnap // the contested fields as stored (goo_order_state.go)
}

const gooOrderCols = `id, type, parent_id, status, currency, prices_include_tax, customer_id, billing, shipping, payment_method,
	payment_method_title, transaction_id, customer_ip, user_agent, customer_note, created_via, cart_hash, order_key,
	discount_total::float8, discount_tax::float8, shipping_total::float8, shipping_tax::float8, cart_tax::float8, total::float8,
	total_tax::float8, refund_reason, refunded_by, stock_reduced, downloads_granted, new_order_email_sent, meta, date_created,
	date_modified, date_paid, date_completed`

func gooScanOrder(row pgx.Row) (*gooOrder, error) {
	o := &gooOrder{}
	var billing, shipping, meta []byte
	err := row.Scan(&o.ID, &o.Type, &o.ParentID, &o.Status, &o.Currency, &o.PricesIncludeTax, &o.CustomerID, &billing, &shipping, &o.PaymentMethod,
		&o.PaymentMethodTitle, &o.TransactionID, &o.CustomerIP, &o.UserAgent, &o.CustomerNote, &o.CreatedVia, &o.CartHash, &o.OrderKey,
		&o.DiscountTotal, &o.DiscountTax, &o.ShippingTotal, &o.ShippingTax, &o.CartTax, &o.Total,
		&o.TotalTax, &o.RefundReason, &o.RefundedBy, &o.StockReduced, &o.DownloadsGranted, &o.NewOrderEmailSent, &meta, &o.DateCreated,
		&o.DateModified, &o.DatePaid, &o.DateCompleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(billing, &o.Billing)
	_ = json.Unmarshal(shipping, &o.Shipping)
	_ = json.Unmarshal(meta, &o.Meta)
	if o.Billing == nil {
		o.Billing = map[string]string{}
	}
	if o.Shipping == nil {
		o.Shipping = map[string]string{}
	}
	if o.Meta == nil {
		o.Meta = map[string]any{}
	}
	o.snapshot()
	return o, nil
}

// gooGetOrder is wc_get_order() with its items.
func (s *Server) gooGetOrder(ctx context.Context, id int64) (*gooOrder, error) {
	o, err := gooScanOrder(s.store.Pool.QueryRow(ctx, `SELECT `+gooOrderCols+` FROM goo_orders WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	o.Items, err = s.gooOrderItems(ctx, o.ID)
	return o, err
}

// gooOrderByKey finds an order by its key (order received / pay links of guests).
func (s *Server) gooOrderByKey(ctx context.Context, key string) (*gooOrder, error) {
	var id int64
	if err := s.store.Pool.QueryRow(ctx, `SELECT id FROM goo_orders WHERE order_key = $1 AND type = 'shop_order'`, key).Scan(&id); err != nil {
		return nil, store.ErrNotFound
	}
	return s.gooGetOrder(ctx, id)
}

func (s *Server) gooOrderItems(ctx context.Context, orderID int64) ([]*gooOrderItem, error) {
	all, err := s.gooOrderItemsOf(ctx, []int64{orderID})
	return all[orderID], err
}

// gooOrderItemsOf loads the items of several orders in one query, keyed by order id.
func (s *Server) gooOrderItemsOf(ctx context.Context, orderIDs []int64) (map[int64][]*gooOrderItem, error) {
	out := map[int64][]*gooOrderItem{}
	if len(orderIDs) == 0 {
		return out, nil
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT id, order_id, type, name, product_id, variation_id, quantity::float8, tax_class, subtotal::float8,
		subtotal_tax::float8, total::float8, total_tax::float8, taxes, meta, position FROM goo_order_items WHERE order_id = ANY($1) ORDER BY order_id, position, id`, orderIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		it := &gooOrderItem{}
		var taxes, meta []byte
		if err := rows.Scan(&it.ID, &it.OrderID, &it.Type, &it.Name, &it.ProductID, &it.VariationID, &it.Quantity, &it.TaxClass, &it.Subtotal,
			&it.SubtotalTax, &it.Total, &it.TotalTax, &taxes, &meta, &it.Position); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(taxes, &it.Taxes)
		_ = json.Unmarshal(meta, &it.Meta)
		if it.Meta == nil {
			it.Meta = map[string]any{}
		}
		it.snapshot()
		out[it.OrderID] = append(out[it.OrderID], it)
	}
	return out, rows.Err()
}

// ItemsOf filters the items by type (line_item, shipping, fee, coupon, tax).
func (o *gooOrder) ItemsOf(typ string) []*gooOrderItem {
	var out []*gooOrderItem
	for _, it := range o.Items {
		if it.Type == typ {
			out = append(out, it)
		}
	}
	return out
}

// Number is get_order_number(): the id, or the number the order was given elsewhere (_order_number,
// the meta WooCommerce's sequential order number plugins keep, which WP to Pursae sets to the order's
// WordPress number so imported orders keep the number their customers know).
func (o *gooOrder) Number() string {
	switch v := o.Meta[gooOrderNumberMeta].(type) {
	case string:
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	case float64:
		if v > 0 {
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return itoa64(o.ID)
}

// gooOrderNumberMeta is the order meta holding a number other than the id (Number).
const gooOrderNumberMeta = "_order_number"

// IsPaid is is_paid().
func (o *gooOrder) IsPaid() bool { return containsStr(gooPaidStatuses, o.Status) }

// BillingName is the formatted billing full name.
func (o *gooOrder) BillingName() string {
	return strings.TrimSpace(o.Billing["first_name"] + " " + o.Billing["last_name"])
}

// MetaString reads a string meta value.
func (o *gooOrder) MetaString(k string) string {
	if v, ok := o.Meta[k].(string); ok {
		return v
	}
	return ""
}

// Subtotal is get_subtotal(): line item subtotals (ex tax).
func (o *gooOrder) Subtotal() float64 {
	var t float64
	for _, it := range o.ItemsOf("line_item") {
		t += it.Subtotal
	}
	return t
}

// ItemCount is get_item_count().
func (o *gooOrder) ItemCount() int {
	n := 0
	for _, it := range o.ItemsOf("line_item") {
		n += it.Qty()
	}
	return n
}

// gooSaveOrder writes the order row (insert when ID is 0) and, with items, replaces its items.
// On an existing order the contested fields are written only when the caller changed them (see
// goo_order_state.go); o is refreshed with what the database then holds.
func (s *Server) gooSaveOrder(ctx context.Context, o *gooOrder, withItems bool) error {
	return pgx.BeginFunc(ctx, s.store.Pool, func(tx pgx.Tx) error { return s.gooSaveOrderTx(ctx, tx, o, withItems) })
}

// errGooOrderInProgress is returned by a checkout that lost the race for the order it meant to adopt
// (the session's checkout-draft or its order awaiting payment) to a concurrent checkout.
// gooDraftRetryAfter is how long after it was placed an unpaid order a stale draft key still points at
// counts as a retry (reused) rather than a double submit of the same placement (refused).
const gooDraftRetryAfter = 2 * time.Minute

var errGooOrderInProgress = errors.New("Your order is already being processed. Please wait or check your order history.")

// gooClaimDraftTx takes the checkout-draft order id for the checkout running in tx: it moves it to
// pending only while it is still a draft. A concurrent claim waits on the row lock and, once the first
// commits, matches nothing (false); a rolled-back save gives the draft back.
func (s *Server) gooClaimDraftTx(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
	var got int64
	err := tx.QueryRow(ctx, `UPDATE goo_orders SET status = 'pending' WHERE id = $1 AND status = 'checkout-draft' RETURNING id`, id).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// gooClaimAwaitingOrderTx locks an unpaid order a checkout reuses and checks it is the version the
// checkout read (date_modified): every save moves date_modified, so of two checkouts that read the same
// order only the first to save keeps it; the other sees a newer version (false).
func (s *Server) gooClaimAwaitingOrderTx(ctx context.Context, tx pgx.Tx, id int64, modified time.Time) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM goo_orders WHERE id = $1 AND date_modified = $2 AND status IN ('pending','failed') FOR UPDATE`,
		id, modified).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// gooSaveOrderTx is gooSaveOrder inside the caller's transaction.
func (s *Server) gooSaveOrderTx(ctx context.Context, tx pgx.Tx, o *gooOrder, withItems bool) error {
	if o.Type == "" {
		o.Type = "shop_order"
	}
	if o.Currency == "" {
		o.Currency = s.gooCurrency()
	}
	if o.Status == "" {
		o.Status = "pending"
	}
	if o.OrderKey == "" && o.Type == "shop_order" {
		o.OrderKey = "wc_order_" + gooRandomKey(13)
	}
	if o.Meta == nil {
		o.Meta = map[string]any{}
	}
	billing, _ := json.Marshal(o.Billing)
	shipping, _ := json.Marshal(o.Shipping)
	meta, _ := json.Marshal(o.Meta)
	email := strings.ToLower(strings.TrimSpace(o.Billing["email"]))
	var err error
	if o.ID == 0 {
		err = tx.QueryRow(ctx, `INSERT INTO goo_orders (type, parent_id, status, currency, prices_include_tax, customer_id, billing, shipping,
			billing_email, payment_method, payment_method_title, transaction_id, customer_ip, user_agent, customer_note, created_via, cart_hash,
			order_key, discount_total, discount_tax, shipping_total, shipping_tax, cart_tax, total, total_tax, refund_reason, refunded_by,
			stock_reduced, downloads_granted, new_order_email_sent, meta, date_paid, date_completed)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33)
			RETURNING id, date_created, date_modified`,
			o.Type, o.ParentID, o.Status, o.Currency, o.PricesIncludeTax, o.CustomerID, billing, shipping, email, o.PaymentMethod,
			o.PaymentMethodTitle, o.TransactionID, o.CustomerIP, o.UserAgent, o.CustomerNote, o.CreatedVia, o.CartHash, o.OrderKey,
			o.DiscountTotal, o.DiscountTax, o.ShippingTotal, o.ShippingTax, o.CartTax, o.Total, o.TotalTax, o.RefundReason, o.RefundedBy,
			o.StockReduced, o.DownloadsGranted, o.NewOrderEmailSent, meta, o.DatePaid, o.DateCompleted).Scan(&o.ID, &o.DateCreated, &o.DateModified)
		if err == nil {
			o.snapshot()
		}
	} else {
		// A copy built without loading the order (orig nil) writes everything, as before.
		changed := func(f func(*gooOrderSnap) bool) bool { return o.orig == nil || f(o.orig) }
		var storedMeta []byte
		err = tx.QueryRow(ctx, `UPDATE goo_orders SET status = CASE WHEN $33 THEN $2 ELSE status END, currency=$3, prices_include_tax=$4,
			customer_id=$5, billing=$6, shipping=$7, billing_email=$8, payment_method=$9, payment_method_title=$10,
			transaction_id = CASE WHEN $37 THEN $11 ELSE transaction_id END,
			customer_ip=$12, user_agent=$13, customer_note=$14, created_via=$15, cart_hash=$16, discount_total=$17, discount_tax=$18,
			shipping_total=$19, shipping_tax=$20, cart_tax=$21, total=$22, total_tax=$23, refund_reason=$24, refunded_by=$25,
			stock_reduced = CASE WHEN $34 THEN $26 ELSE stock_reduced END,
			downloads_granted = CASE WHEN $35 THEN $27 ELSE downloads_granted END, new_order_email_sent=$28,
			meta = ($29::jsonb - $36::text[]) || coalesce((SELECT jsonb_object_agg(e.key, e.value) FROM jsonb_each(goo_orders.meta) e
				WHERE e.key = ANY($36::text[])), '{}'::jsonb),
			date_paid = CASE WHEN $38 THEN $30 ELSE date_paid END, date_completed = CASE WHEN $39 THEN $31 ELSE date_completed END,
			date_created=$32, date_modified=now() WHERE id=$1
			RETURNING status, stock_reduced, downloads_granted, meta, transaction_id, date_paid, date_completed, date_modified`,
			o.ID, o.Status, o.Currency, o.PricesIncludeTax, o.CustomerID, billing, shipping, email, o.PaymentMethod, o.PaymentMethodTitle,
			o.TransactionID, o.CustomerIP, o.UserAgent, o.CustomerNote, o.CreatedVia, o.CartHash, o.DiscountTotal, o.DiscountTax,
			o.ShippingTotal, o.ShippingTax, o.CartTax, o.Total, o.TotalTax, o.RefundReason, o.RefundedBy, o.StockReduced, o.DownloadsGranted,
			o.NewOrderEmailSent, meta, o.DatePaid, o.DateCompleted, o.DateCreated,
			changed(func(sn *gooOrderSnap) bool { return sn.status != o.Status }),
			changed(func(sn *gooOrderSnap) bool { return sn.stockReduced != o.StockReduced }),
			changed(func(sn *gooOrderSnap) bool { return sn.downloadsGranted != o.DownloadsGranted }),
			o.keptBookkeepingKeys(),
			changed(func(sn *gooOrderSnap) bool { return sn.transactionID != o.TransactionID }),
			changed(func(sn *gooOrderSnap) bool { return !gooSameTime(sn.datePaid, o.DatePaid) }),
			changed(func(sn *gooOrderSnap) bool { return !gooSameTime(sn.dateCompleted, o.DateCompleted) }),
		).Scan(&o.Status, &o.StockReduced, &o.DownloadsGranted, &storedMeta, &o.TransactionID, &o.DatePaid, &o.DateCompleted, &o.DateModified)
		if err == nil {
			o.gooMergeBookkeeping(storedMeta)
			o.snapshot()
		}
	}
	if err != nil || !withItems {
		return err
	}
	// Items keep their ids across saves (refund lines point at them through _refunded_item_id):
	// stored items are updated in place, new ones inserted, the ones no longer on the order deleted.
	keep := []int64{}
	for i, it := range o.Items {
		it.OrderID, it.Position = o.ID, i
		if it.Meta == nil {
			it.Meta = map[string]any{}
		}
		taxes, _ := json.Marshal(it.Taxes)
		im, _ := json.Marshal(it.Meta)
		if it.ID != 0 {
			var red []byte
			err := tx.QueryRow(ctx, `UPDATE goo_order_items SET type=$3, name=$4, product_id=$5, variation_id=$6, quantity=$7, tax_class=$8,
				subtotal=$9, subtotal_tax=$10, total=$11, total_tax=$12, taxes=$13,
				meta = CASE WHEN $16 THEN ($14::jsonb - '_reduced_stock') || CASE WHEN goo_order_items.meta->'_reduced_stock' IS NOT NULL
					THEN jsonb_build_object('_reduced_stock', goo_order_items.meta->'_reduced_stock') ELSE '{}'::jsonb END ELSE $14::jsonb END,
				position=$15 WHERE id=$1 AND order_id=$2 RETURNING meta->'_reduced_stock'`,
				it.ID, o.ID, it.Type, it.Name, it.ProductID, it.VariationID, it.Quantity, it.TaxClass, it.Subtotal, it.SubtotalTax, it.Total,
				it.TotalTax, taxes, im, it.Position, it.keepsReduced()).Scan(&red)
			if err == nil {
				it.setReduced(red)
				it.snapshot()
				keep = append(keep, it.ID)
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO goo_order_items (order_id, type, name, product_id, variation_id, quantity, tax_class, subtotal,
			subtotal_tax, total, total_tax, taxes, meta, position) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
			o.ID, it.Type, it.Name, it.ProductID, it.VariationID, it.Quantity, it.TaxClass, it.Subtotal, it.SubtotalTax, it.Total, it.TotalTax,
			taxes, im, it.Position).Scan(&it.ID); err != nil {
			return err
		}
		it.snapshot()
		keep = append(keep, it.ID)
	}
	_, err = tx.Exec(ctx, `DELETE FROM goo_order_items WHERE order_id = $1 AND NOT (id = ANY($2))`, o.ID, keep)
	return err
}

// gooRecalculate is WC_Order::calculate_totals() for an order edited in the admin: line taxes are kept
// as entered, totals are summed from the items.
func (s *Server) gooRecalculate(o *gooOrder) {
	dp := s.gooDecimals()
	var cartTax, shipTax, shipTotal, fees, items, subtotal, subtotalTax float64
	for _, it := range o.Items {
		switch it.Type {
		case "line_item":
			items += it.Total
			cartTax += it.TotalTax
			subtotal += it.Subtotal
			subtotalTax += it.SubtotalTax
		case "fee":
			fees += it.Total
			cartTax += it.TotalTax
		case "shipping":
			shipTotal += it.Total
			shipTax += it.TotalTax
		}
	}
	o.DiscountTotal = gooRound(subtotal-items, dp)
	o.DiscountTax = gooRound(subtotalTax-cartTax, dp)
	if o.DiscountTax < 0 {
		o.DiscountTax = 0
	}
	o.CartTax, o.ShippingTax, o.ShippingTotal = gooRound(cartTax, dp), gooRound(shipTax, dp), gooRound(shipTotal, dp)
	o.TotalTax = gooRound(cartTax+shipTax, dp)
	o.Total = gooRound(items+fees+shipTotal+o.TotalTax, dp)
}

// Notes ---------------------------------------------------------------------------------------------

// gooAddOrderNote is WC_Order::add_order_note(); a customer note is e-mailed to the customer.
func (s *Server) gooAddOrderNote(ctx context.Context, o *gooOrder, note string, customer bool, by string) error {
	if strings.TrimSpace(note) == "" {
		return nil
	}
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO goo_order_notes (order_id, content, is_customer_note, added_by) VALUES ($1,$2,$3,$4)`,
		o.ID, note, customer, by); err != nil {
		return err
	}
	if customer {
		s.gooFire(ctx, "customer_note", &gooNoteEvent{Order: o, Note: note})
	}
	return nil
}

// gooNoteEvent is the argument of customer_note.
type gooNoteEvent struct {
	Order *gooOrder
	Note  string
}

// gooOrderNotes lists an order's notes, newest first; customerOnly for the customer's view.
func (s *Server) gooOrderNotes(ctx context.Context, orderID int64, customerOnly bool) ([]*gooOrderNote, error) {
	q := `SELECT id, order_id, content, is_customer_note, added_by, created_at FROM goo_order_notes WHERE order_id = $1`
	if customerOnly {
		q += ` AND is_customer_note`
	}
	rows, err := s.store.Pool.Query(ctx, q+` ORDER BY created_at DESC, id DESC`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*gooOrderNote
	for rows.Next() {
		n := &gooOrderNote{}
		if err := rows.Scan(&n.ID, &n.OrderID, &n.Content, &n.IsCustomer, &n.AddedBy, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Status -------------------------------------------------------------------------------------------

// gooSetStatus is WC_Order::update_status(): saves, records a note and runs the transition's
// bookkeeping (stock, coupons, downloads, e-mails through order_status_changed). The change starts
// from the status the database holds, under the order's lock (goo_order_state.go).
func (s *Server) gooSetStatus(ctx context.Context, o *gooOrder, to, note string, by string) error {
	return s.gooSetStatusFrom(ctx, o, nil, to, note, by)
}

// gooSetStatusFrom is gooSetStatus for an order whose current status is one of from (nil: any);
// when it no longer is (a payment arrived, the order was cancelled meanwhile) nothing changes and
// errGooStatusChanged is returned.
func (s *Server) gooSetStatusFrom(ctx context.Context, o *gooOrder, from []string, to, note string, by string) error {
	to = strings.TrimPrefix(to, "wc-")
	valid := false
	for _, st := range gooOrderStatuses {
		if st.Value == to {
			valid = true
		}
	}
	if !valid {
		return fmt.Errorf("invalid order status %q", to)
	}
	if o.ID == 0 {
		if err := s.gooSaveOrder(ctx, o, true); err != nil {
			return err
		}
	}
	return gooWithOrderLock(ctx, o.ID, func(ctx context.Context) error { return s.gooSetStatusLocked(ctx, o, from, to, note, by) })
}

// gooSetStatusLocked is the status change itself; the caller holds the order's lock.
func (s *Server) gooSetStatusLocked(ctx context.Context, o *gooOrder, from []string, to, note string, by string) error {
	if err := s.gooRefreshOrderState(ctx, o); err != nil {
		return err
	}
	prev := o.Status
	if from != nil && !containsStr(from, prev) {
		return errGooStatusChanged
	}
	if prev == to {
		if note != "" {
			return s.gooAddOrderNote(ctx, o, note, false, by)
		}
		return nil
	}
	// WooCommerce reduces stock and counts coupon uses when an order goes on hold or is paid. Both
	// are taken before the status is saved, each in one conditional UPDATE: when another order took
	// the last unit or the coupon's last use meanwhile, a change the system makes (a payment, by == "")
	// puts the order on hold with a note instead of selling what is not there. A change made by a
	// person or an API client is their decision, as in WooCommerce: stock may go below zero.
	var problems []string
	if to == "processing" || to == "completed" || to == "on-hold" {
		strict := by == ""
		if err := s.gooReduceStock(ctx, o, strict); err != nil {
			problems = append(problems, err.Error())
		}
		if err := s.gooRecordCouponUsage(ctx, o, true, strict); err != nil {
			problems = append(problems, err.Error())
		}
		// Points and gift card balances that a cancellation or refund returned are taken back: the
		// order keeps their discount. What can no longer be taken back holds the order like a stock
		// shortage, so a late payment cannot spend the same value twice. A person's change keeps its
		// status, with the note.
		var short []string
		if err := s.gptsRedebit(ctx, o); err != nil {
			short = append(short, err.Error())
		}
		if err := s.gcRedebit(ctx, o); err != nil {
			short = append(short, err.Error())
		}
		problems = append(problems, short...)
		if len(problems) > 0 && (strict || len(problems) > len(short)) {
			to = "on-hold"
		}
	}
	join := func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, " ")
	}
	held := join(problems...)
	if prev == to { // on hold already, and still short of stock or coupon uses
		return s.gooAddOrderNote(ctx, o, join(note, held), false, by)
	}
	o.Status = to
	now := time.Now()
	if containsStr(gooPaidStatuses, to) && o.DatePaid == nil {
		o.DatePaid = &now
	}
	if to == "completed" {
		o.DateCompleted = &now
	}
	// The row lock re-checks the status for another instance of the app changing it meanwhile.
	if err := pgx.BeginFunc(ctx, s.store.Pool, func(tx pgx.Tx) error {
		var cur string
		if err := tx.QueryRow(ctx, `SELECT status FROM goo_orders WHERE id = $1 FOR UPDATE`, o.ID).Scan(&cur); err != nil {
			return err
		}
		if cur != prev {
			return errGooStatusChanged
		}
		return s.gooSaveOrderTx(ctx, tx, o, false)
	}); err != nil {
		o.Status = prev
		return err
	}
	if to != "pending" && to != "checkout-draft" {
		// wc_release_stock_for_order(): paid or on hold, the stock is reduced instead; cancelled or
		// failed, it goes back to the shop.
		s.gooReleaseStock(ctx, o.ID)
	}
	_ = s.gooAddOrderNote(ctx, o, join(note, held, fmt.Sprintf("Order status changed from %s to %s.", gooStatusLabel(prev), gooStatusLabel(to))), false, by)
	s.gooStatusTransition(ctx, o, prev, to)
	s.gooFire(ctx, "order_status_changed", &gooStatusChange{Order: o, From: prev, To: to})
	s.gooPurgeOrder(ctx, o)
	return nil
}

// gooDownloadPermitted is WC_Order::is_download_permitted(): files are released on completed
// orders, and on processing ones too when "Grant access to downloadable products after payment"
// is on. On-hold (unpaid) orders never get access.
func (s *Server) gooDownloadPermitted(status string) bool {
	return status == "completed" || (status == "processing" && s.gooOptBool("downloads_grant_access_after_payment"))
}

// gooStatusTransition is WooCommerce's status hooks: stock is reduced when an order becomes
// processing / completed / on-hold (taken in gooSetStatusLocked) and restored when it is cancelled
// (or pending again); coupons count their uses likewise; downloads are granted when paid.
func (s *Server) gooStatusTransition(ctx context.Context, o *gooOrder, from, to string) {
	switch to {
	case "processing", "completed", "on-hold":
		if s.gooDownloadPermitted(to) {
			s.gooGrantDownloads(ctx, o)
		}
		if containsStr(gooPaidStatuses, to) {
			s.gooRecordSales(ctx, o)
		}
	case "cancelled", "pending", "failed":
		if to == "cancelled" || from == "on-hold" || from == "processing" {
			s.gooRestoreStock(ctx, o)
		}
		if to == "cancelled" {
			_ = s.gooRecordCouponUsage(ctx, o, false, false)
		}
	}
}

// gooStockShortage is the error of a stock reduction that would sell more than is in stock.
type gooStockShortage struct {
	Name      string
	Available int
}

func (e *gooStockShortage) Error() string {
	return fmt.Sprintf("Not enough units of %s are available in stock to fulfil this order (%d available).", e.Name, max(e.Available, 0))
}

// gooReduceStock is wc_reduce_stock_levels(): once per order (the stock_reduced flag is claimed in
// the database). strict reduces only what is in stock (unless the product allows backorders): when a
// line cannot be reduced, the lines reduced so far are put back and a *gooStockShortage is returned.
func (s *Server) gooReduceStock(ctx context.Context, o *gooOrder, strict bool) error {
	if !s.gooManageStock() {
		return nil
	}
	if claimed, err := s.gooClaimOrderFlag(ctx, o, "stock_reduced", true); err != nil || !claimed {
		return err
	}
	type reduced struct {
		it       *gooOrderItem
		p, owner *gooProduct
		qty      int
	}
	var done []reduced
	var failed []string
	for _, it := range o.ItemsOf("line_item") {
		if red, _ := it.Meta["_reduced_stock"].(float64); red > 0 {
			continue // reduced already (an item added in the admin)
		}
		pid := it.ProductID
		if it.VariationID != 0 {
			pid = it.VariationID
		}
		p, err := s.gooGetProduct(ctx, pid)
		if err != nil || !s.gooManagingStock(ctx, p) || it.Qty() <= 0 {
			continue
		}
		owner := s.gooStockWriteOwner(ctx, p)
		n, err := s.gooStockDelta(ctx, owner, -it.Qty(), false, strict, o.ID)
		if errors.Is(err, errGooNoStock) {
			for _, d := range done {
				_, _ = s.gooStockDelta(ctx, d.owner, d.it.Qty(), false, false, 0)
			}
			_, _ = s.gooClaimOrderFlag(ctx, o, "stock_reduced", false)
			avail := 0
			_ = s.store.Pool.QueryRow(ctx, `SELECT coalesce(stock_quantity, 0) FROM goo_products WHERE post_id = $1`, owner.ID).Scan(&avail)
			// Units other orders still hold are not available. Reporting the raw counter says "1 available"
			// when that 1 is reserved, and the order note then looks like the line can be fulfilled.
			held := 0
			_ = s.store.Pool.QueryRow(ctx, `SELECT coalesce(sum(stock_quantity), 0) FROM goo_reserved_stock
				WHERE product_id = $1 AND order_id <> $2 AND expires_at > now()`, owner.ID, o.ID).Scan(&held)
			return &gooStockShortage{Name: s.gooProductTitle(ctx, p), Available: max(0, avail-held)}
		}
		if err != nil {
			// wc_reduce_stock_levels(): the order stays marked reduced and the line keeps no
			// _reduced_stock (so it is not restored later), but the failure is told to the shop in an
			// order note instead of leaving the inventory silently wrong.
			log.Printf("goo: reduce stock order %d product %d: %v", o.ID, owner.ID, err)
			label := s.gooProductTitle(ctx, p)
			if p.SKU != "" {
				label += " (" + p.SKU + ")"
			}
			failed = append(failed, html.EscapeString(label))
			continue
		}
		done = append(done, reduced{it, p, owner, n})
	}
	var changes []string
	for _, d := range done {
		s.gooStockAfter(ctx, d.owner, d.qty+d.it.Qty(), d.qty, true)
		_ = s.gooSetItemReduced(ctx, d.it, float64(d.it.Qty()))
		label := s.gooProductTitle(ctx, d.p)
		if d.p.SKU != "" {
			label += " (" + d.p.SKU + ")"
		}
		changes = append(changes, fmt.Sprintf("%s %d&rarr;%d", html.EscapeString(label), d.qty+d.it.Qty(), d.qty))
		if d.qty < 0 {
			s.gooFire(ctx, "backorder", &gooBackorderEvent{Order: o, Product: d.p, Quantity: d.it.Qty()})
		}
	}
	if len(changes) > 0 {
		_ = s.gooAddOrderNote(ctx, o, "Stock levels reduced: "+strings.Join(changes, ", "), false, "")
	}
	for _, label := range failed {
		_ = s.gooAddOrderNote(ctx, o, "Unable to reduce stock for item "+label+".", false, "")
	}
	return nil
}

// gooBackorderEvent is the argument of backorder.
type gooBackorderEvent struct {
	Order    *gooOrder
	Product  *gooProduct
	Quantity int
}

// gooRestoreStock is wc_increase_stock_levels(): puts back what was reduced, once.
func (s *Server) gooRestoreStock(ctx context.Context, o *gooOrder) {
	if claimed, err := s.gooClaimOrderFlag(ctx, o, "stock_reduced", false); err != nil || !claimed {
		return
	}
	var changes []string
	for _, it := range o.ItemsOf("line_item") {
		red, _ := it.Meta["_reduced_stock"].(float64)
		if red <= 0 {
			continue
		}
		pid := it.ProductID
		if it.VariationID != 0 {
			pid = it.VariationID
		}
		p, err := s.gooGetProduct(ctx, pid)
		if err != nil {
			continue
		}
		n, err := s.gooUpdateStock(ctx, p, int(red), false)
		if err != nil {
			continue
		}
		_ = s.gooSetItemReduced(ctx, it, 0)
		changes = append(changes, fmt.Sprintf("%s %d&rarr;%d", html.EscapeString(s.gooProductTitle(ctx, p)), n-int(red), n))
	}
	if len(changes) > 0 {
		_ = s.gooAddOrderNote(ctx, o, "Stock levels increased: "+strings.Join(changes, ", "), false, "")
	}
}

// gooRecordCouponUsage counts (or uncounts) the order's coupon uses once (the flag is claimed in the
// database). strict counts a use only within the coupon's usage limits: when a coupon has none left
// the uses counted so far are taken back and an error is returned.
func (s *Server) gooRecordCouponUsage(ctx context.Context, o *gooOrder, increase, strict bool) error {
	if claimed, err := s.gooClaimOrderFlag(ctx, o, "_recorded_coupon_usage_counts", increase); err != nil || !claimed {
		return err
	}
	who := o.Billing["email"]
	if o.CustomerID != 0 {
		who = itoa64(o.CustomerID)
	}
	var keys []string // the customer's entries in used_by (gooCart.customerKeys)
	if o.CustomerID != 0 {
		keys = append(keys, itoa64(o.CustomerID))
	}
	if e := strings.ToLower(strings.TrimSpace(o.Billing["email"])); e != "" {
		keys = append(keys, e)
	}
	var counted []string
	for _, c := range o.ItemsOf("coupon") {
		if !increase {
			_ = s.gooDecreaseCouponUsage(ctx, c.Name, who)
			continue
		}
		ok, err := s.gooTryIncreaseCouponUsage(ctx, c.Name, who, keys, strict)
		if err != nil {
			continue
		}
		if !ok {
			for _, code := range counted {
				_ = s.gooDecreaseCouponUsage(ctx, code, who)
			}
			_, _ = s.gooClaimOrderFlag(ctx, o, "_recorded_coupon_usage_counts", false)
			return fmt.Errorf(`Usage limit for coupon "%s" has been reached.`, html.EscapeString(c.Name))
		}
		counted = append(counted, c.Name)
	}
	return nil
}

// gooRecordSales counts total_sales once per order.
func (s *Server) gooRecordSales(ctx context.Context, o *gooOrder) {
	if claimed, err := s.gooClaimOrderFlag(ctx, o, "_recorded_sales", true); err != nil || !claimed {
		return
	}
	for _, it := range o.ItemsOf("line_item") {
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_products SET total_sales = total_sales + $2 WHERE post_id = $1`, it.ProductID, it.Qty())
	}
}

// gooGrantDownloads is wc_downloadable_product_permissions(): once per order.
func (s *Server) gooGrantDownloads(ctx context.Context, o *gooOrder) {
	if claimed, err := s.gooClaimOrderFlag(ctx, o, "downloads_granted", true); err != nil || !claimed {
		return
	}
	for _, it := range o.ItemsOf("line_item") {
		pid := it.ProductID
		if it.VariationID != 0 {
			pid = it.VariationID
		}
		p, err := s.gooGetProduct(ctx, pid)
		if err != nil || !p.Downloadable {
			continue
		}
		for _, d := range p.Downloads {
			var remaining any
			if p.DownloadLimit > 0 {
				remaining = p.DownloadLimit * it.Qty()
			}
			var expires any
			if p.DownloadExpiry > 0 {
				expires = time.Now().AddDate(0, 0, p.DownloadExpiry)
			}
			_, _ = s.store.Pool.Exec(ctx, `INSERT INTO goo_download_permissions (download_id, product_id, order_id, order_key, user_email, user_id,
				downloads_remaining, access_expires) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, d.ID, p.ID, o.ID, o.OrderKey, o.Billing["email"], o.CustomerID, remaining, expires)
		}
	}
}

// gooPaymentStatuses are the statuses payment_complete() starts from
// (woocommerce_valid_order_statuses_for_payment_complete).
var gooPaymentStatuses = []string{"on-hold", "pending", "failed", "cancelled", "checkout-draft"}

// gooNeedsPaymentStatuses are the statuses an order can be paid from (woocommerce_valid_order_statuses_for_payment).
var gooNeedsPaymentStatuses = []string{"pending", "failed"}

// gooPaymentComplete is WC_Order::payment_complete(): the transaction id, then processing (or
// completed when nothing needs processing: only virtual downloadable items). The status is checked
// under the order's lock, so a payment and the unpaid-order job cannot both win.
func (s *Server) gooPaymentComplete(ctx context.Context, o *gooOrder, transactionID string) error {
	if o.ID == 0 {
		if err := s.gooSaveOrder(ctx, o, true); err != nil {
			return err
		}
	}
	return gooWithOrderLock(ctx, o.ID, func(ctx context.Context) error {
		if err := s.gooRefreshOrderState(ctx, o); err != nil {
			return err
		}
		if !containsStr(gooPaymentStatuses, o.Status) {
			return nil
		}
		if transactionID != "" {
			o.TransactionID = transactionID
		}
		now := time.Now()
		o.DatePaid = &now
		to := "processing"
		if !s.gooOrderNeedsProcessing(ctx, o) {
			to = "completed"
		}
		if err := s.gooSetStatusLocked(ctx, o, gooPaymentStatuses, to, "", ""); err != nil {
			return err
		}
		s.gooFire(ctx, "payment_complete", o)
		return nil
	})
}

// gooOrderHasDownloadable is WC_Order::has_downloadable_item().
func (s *Server) gooOrderHasDownloadable(ctx context.Context, o *gooOrder) bool {
	for _, it := range o.ItemsOf("line_item") {
		pid := it.ProductID
		if it.VariationID != 0 {
			pid = it.VariationID
		}
		p, err := s.gooGetProduct(ctx, pid)
		if err == nil && p.Downloadable {
			return true
		}
	}
	return false
}

// gooOrderReleasesValueOnPayment is true when moving the order to processing would hand out value
// before a payment is taken: a downloadable file (has_downloadable_item) or an item a module releases
// on payment (gooPaidValueHooks).
func (s *Server) gooOrderReleasesValueOnPayment(ctx context.Context, o *gooOrder) bool {
	if s.gooOrderHasDownloadable(ctx, o) {
		return true
	}
	for _, h := range gooPaidValueHooks {
		if h(s, ctx, o) {
			return true
		}
	}
	return false
}

// gooOrderNeedsProcessing is needs_processing(): an item that is not virtual and downloadable.
func (s *Server) gooOrderNeedsProcessing(ctx context.Context, o *gooOrder) bool {
	for _, it := range o.ItemsOf("line_item") {
		pid := it.ProductID
		if it.VariationID != 0 {
			pid = it.VariationID
		}
		p, err := s.gooGetProduct(ctx, pid)
		if err != nil {
			return true
		}
		if !(p.Virtual && p.Downloadable) {
			return true
		}
	}
	return false
}

// Refunds -------------------------------------------------------------------------------------------

// gooRefundLine is a line of a refund: an item, a quantity and amounts.
type gooRefundLine struct {
	ItemID   int64
	Qty      int
	Total    float64
	TaxTotal map[string]float64
}

// gooRefundEvent is the argument of order_refunded.
type gooRefundEvent struct {
	Order  *gooOrder
	Refund *gooOrder
}

// gooRefunds are an order's refunds, newest first.
func (s *Server) gooRefunds(ctx context.Context, orderID int64) ([]*gooOrder, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+gooOrderCols+` FROM goo_orders WHERE parent_id = $1 AND type = 'shop_order_refund' ORDER BY date_created DESC, id DESC`, orderID)
	if err != nil {
		return nil, err
	}
	var out []*gooOrder
	for rows.Next() {
		r, err := gooScanOrder(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	ids := make([]int64, len(out))
	for i, r := range out {
		ids[i] = r.ID
	}
	items, _ := s.gooOrderItemsOf(ctx, ids)
	for _, r := range out {
		r.Items = items[r.ID]
	}
	return out, nil
}

// gooOrderIDs are the ids of the orders.
func gooOrderIDs(orders []*gooOrder) []int64 {
	ids := make([]int64, len(orders))
	for i, o := range orders {
		ids[i] = o.ID
	}
	return ids
}

// gooTotalsRefunded is gooTotalRefunded for several orders in one query; orders without refunds are absent.
func (s *Server) gooTotalsRefunded(ctx context.Context, orderIDs []int64) map[int64]float64 {
	out := map[int64]float64{}
	if len(orderIDs) == 0 {
		return out
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT parent_id, coalesce(sum(-total),0)::float8 FROM goo_orders
		WHERE parent_id = ANY($1) AND type = 'shop_order_refund' GROUP BY parent_id`, orderIDs)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var t float64
		if rows.Scan(&id, &t) == nil {
			out[id] = t
		}
	}
	return out
}

// gooTotalRefunded is get_total_refunded().
func (s *Server) gooTotalRefunded(ctx context.Context, orderID int64) float64 {
	var t float64
	_ = s.store.Pool.QueryRow(ctx, `SELECT coalesce(sum(-total),0)::float8 FROM goo_orders WHERE parent_id = $1 AND type = 'shop_order_refund'`, orderID).Scan(&t)
	return t
}

// gooCreateRefund is wc_create_refund(): a refund order with negative lines, optional stock restock,
// the gateway's refund when apiRefund is set, and the order becomes refunded when fully refunded.
// Refunds of one order are serialized (the order lock), and the refunded total is read inside the
// lock, so a double-submitted form or a retried API call cannot refund more than the order total or
// call the gateway twice.
func (s *Server) gooCreateRefund(ctx context.Context, o *gooOrder, amount float64, reason string, lines []gooRefundLine, restock, apiRefund bool, by int64) (*gooOrder, error) {
	var r *gooOrder
	err := gooWithOrderLock(ctx, o.ID, func(ctx context.Context) error {
		var err error
		r, err = s.gooCreateRefundLocked(ctx, o, amount, reason, lines, restock, apiRefund, by)
		return err
	})
	return r, err
}

func (s *Server) gooCreateRefundLocked(ctx context.Context, o *gooOrder, amount float64, reason string, lines []gooRefundLine, restock, apiRefund bool, by int64) (*gooOrder, error) {
	dp := s.gooDecimals()
	amount = gooRound(amount, dp)
	already := s.gooTotalRefunded(ctx, o.ID)
	if amount <= 0 || amount > gooRound(o.Total-already, dp)+0.00001 {
		return nil, errors.New("Invalid refund amount.")
	}
	r := &gooOrder{Type: "shop_order_refund", ParentID: o.ID, Status: "completed", Currency: o.Currency, Total: -amount, RefundReason: reason,
		RefundedBy: by, Billing: map[string]string{}, Shipping: map[string]string{}, Meta: map[string]any{}}
	byID := map[int64]*gooOrderItem{}
	for _, it := range o.Items {
		byID[it.ID] = it
	}
	var taxTotal float64
	for _, l := range lines {
		it := byID[l.ItemID]
		if it == nil || (l.Qty == 0 && l.Total == 0 && len(l.TaxTotal) == 0) {
			continue
		}
		ri := &gooOrderItem{Type: it.Type, Name: it.Name, ProductID: it.ProductID, VariationID: it.VariationID, Quantity: float64(-l.Qty),
			TaxClass: it.TaxClass, Total: -l.Total, Subtotal: -l.Total, Meta: map[string]any{"_refunded_item_id": float64(it.ID)},
			Taxes: gooItemTaxes{Total: map[string]float64{}, Subtotal: map[string]float64{}}}
		for k, v := range l.TaxTotal {
			ri.Taxes.Total[k], ri.Taxes.Subtotal[k] = -v, -v
			ri.TotalTax -= v
			taxTotal += v
		}
		ri.SubtotalTax = ri.TotalTax
		r.Items = append(r.Items, ri)
	}
	r.TotalTax = -gooRound(taxTotal, dp)
	var gw *gooGatewayHandle
	if apiRefund && o.PaymentMethod != "" {
		gw = s.gooGateway(o.PaymentMethod)
		if gw == nil || !gw.Supports("refunds") {
			return nil, errors.New("The payment gateway for this order does not support automatic refunds.")
		}
	}
	// WooCommerce order: the refund record first, then the provider; a provider that rejects the
	// refund removes the record again, so a failed save can never leave money refunded without a record.
	if err := s.gooSaveOrder(ctx, r, true); err != nil {
		return nil, err
	}
	if gw != nil {
		if err := gw.ProcessRefund(s, ctx, o, amount, reason); err != nil {
			_ = s.gooDeleteRefund(ctx, r.ID)
			return nil, err
		}
		r.Meta["_refunded_payment"] = true
		if err := s.gooSaveOrder(ctx, r, false); err != nil {
			return nil, err
		}
	}
	if restock {
		var changes []string
		for _, l := range lines {
			it := byID[l.ItemID]
			if it == nil || it.Type != "line_item" || l.Qty <= 0 {
				continue
			}
			pid := it.ProductID
			if it.VariationID != 0 {
				pid = it.VariationID
			}
			// wc_restock_refunded_items(): only what was taken from stock (and not put back yet) returns.
			red, _ := it.Meta["_reduced_stock"].(float64)
			if red <= 0 {
				continue
			}
			qty := min(l.Qty, int(red))
			p, err := s.gooGetProduct(ctx, pid)
			if err != nil || !s.gooManagingStock(ctx, p) {
				continue
			}
			old := s.gooStockQty(ctx, p)
			n, err := s.gooUpdateStock(ctx, p, qty, false)
			if err != nil {
				continue
			}
			_ = s.gooSetItemReduced(ctx, it, red-float64(qty))
			changes = append(changes, fmt.Sprintf("%s %d&rarr;%d", html.EscapeString(s.gooProductTitle(ctx, p)), old, n))
		}
		if len(changes) > 0 {
			_ = s.gooAddOrderNote(ctx, o, "Item #"+itoa64(o.ID)+" stock increased: "+strings.Join(changes, ", "), false, "")
		}
	}
	note := fmt.Sprintf("Refunded %s", s.gooPricePlain(amount, o.Currency))
	if reason != "" {
		note += " &ndash; " + html.EscapeString(reason)
	}
	_ = s.gooAddOrderNote(ctx, o, note, false, "")
	if gooRound(already+amount, dp) >= gooRound(o.Total, dp) {
		_ = s.gooSetStatus(ctx, o, "refunded", "Order status set to refunded.", "")
		s.gooFire(ctx, "order_fully_refunded", &gooRefundEvent{Order: o, Refund: r})
	} else {
		s.gooFire(ctx, "order_partially_refunded", &gooRefundEvent{Order: o, Refund: r})
	}
	s.gooFire(ctx, "order_refunded", &gooRefundEvent{Order: o, Refund: r})
	return r, nil
}

// gooDeleteRefund removes a refund (the admin's "Delete refund").
func (s *Server) gooDeleteRefund(ctx context.Context, refundID int64) error {
	_, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_orders WHERE id = $1 AND type = 'shop_order_refund'`, refundID)
	return err
}

// Queries ----------------------------------------------------------------------------------------------

// gooOrderQuery is wc_get_orders() for the parts the admin, My Account and the REST API need.
type gooOrderQuery struct {
	Status     []string
	CustomerID int64
	Email      string
	Search     string
	After      *time.Time
	Before     *time.Time
	OrderBy    string // date, id, total
	Order      string
	Page       int
	PerPage    int
	IDs        []int64
}

func (s *Server) gooQueryOrders(ctx context.Context, q gooOrderQuery) ([]*gooOrder, int, error) {
	conds := []string{"type = 'shop_order'"}
	args := []any{}
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if len(q.Status) > 0 {
		conds = append(conds, "status = ANY("+add(q.Status)+")")
	} else {
		conds = append(conds, "status NOT IN ('trash', 'checkout-draft')")
	}
	if q.CustomerID != 0 {
		conds = append(conds, "customer_id = "+add(q.CustomerID))
	}
	if q.Email != "" {
		conds = append(conds, "billing_email = "+add(strings.ToLower(q.Email)))
	}
	if q.Search != "" {
		p := add("%" + strings.ToLower(q.Search) + "%")
		cond := "(lower(billing::text) LIKE " + p + " OR lower(shipping::text) LIKE " + p + " OR id IN (SELECT order_id FROM goo_order_items WHERE lower(name) LIKE " + p + ")"
		if n, err := strconv.ParseInt(strings.TrimPrefix(q.Search, "#"), 10, 64); err == nil {
			cond += " OR id = " + add(n)
		}
		if num := strings.TrimSpace(strings.TrimPrefix(q.Search, "#")); num != "" {
			cond += " OR meta->>'" + gooOrderNumberMeta + "' = " + add(num)
		}
		conds = append(conds, cond+")")
	}
	if q.After != nil {
		conds = append(conds, "date_created >= "+add(*q.After))
	}
	if q.Before != nil {
		conds = append(conds, "date_created < "+add(*q.Before))
	}
	if len(q.IDs) > 0 {
		conds = append(conds, "id = ANY("+add(q.IDs)+")")
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM goo_orders WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	col := "date_created"
	switch q.OrderBy {
	case "id":
		col = "id"
	case "total":
		col = "total"
	case "modified":
		col = "date_modified"
	}
	dir := "DESC"
	if strings.EqualFold(q.Order, "asc") {
		dir = "ASC"
	}
	sql := `SELECT ` + gooOrderCols + ` FROM goo_orders WHERE ` + where + ` ORDER BY ` + col + ` ` + dir + `, id ` + dir
	if q.PerPage > 0 {
		page := min(max(q.Page, 1), store.MaxPage) // admin callers pass ?paged= unbounded
		sql += fmt.Sprintf(" LIMIT %d OFFSET %d", q.PerPage, (page-1)*q.PerPage)
	}
	rows, err := s.store.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	var out []*gooOrder
	for rows.Next() {
		o, err := gooScanOrder(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		out = append(out, o)
	}
	rows.Close()
	ids := make([]int64, len(out))
	for i, o := range out {
		ids[i] = o.ID
	}
	items, _ := s.gooOrderItemsOf(ctx, ids)
	for _, o := range out {
		o.Items = items[o.ID]
	}
	return out, total, nil
}

// gooOrderStatusCounts counts orders per status (the admin list's views).
func (s *Server) gooOrderStatusCounts(ctx context.Context) map[string]int {
	out := map[string]int{}
	rows, err := s.store.Pool.Query(ctx, `SELECT status, count(*) FROM goo_orders WHERE type = 'shop_order' GROUP BY status`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if rows.Scan(&st, &n) == nil {
			out[st] = n
		}
	}
	return out
}

// gooPurgeOrder drops the cached pages an order affects (the products whose stock changed purge themselves).
func (s *Server) gooPurgeOrder(ctx context.Context, o *gooOrder) {
	s.purgeTags("goo:order:" + itoa64(o.ID))
}
