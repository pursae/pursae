package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/store"
)

// Goocommerce › Subscriptions (module "subscriptions"): WooCommerce Subscriptions. A "subscription"
// product type with its billing schedule (price, interval, period, length, sign-up fee, free trial,
// purchase limit); the first payment is the sign-up fee plus the first period (unless there is a
// trial); a paid parent order creates the subscriptions (goo_subscriptions), the hourly job renews them
// through renewal orders (paid automatically by the test gateway, else invoiced), puts unpaid ones on
// hold after the grace period and expires them at their end date. Customers manage them under My
// Account › Subscriptions; the store manages them under Goocommerce › Subscriptions.

const gsubKey = "subscriptions"

// gsubStatuses are the subscription statuses (wcs_get_subscription_statuses()).
var gsubStatuses = []gooOpt{
	{"pending", "Pending"}, {"active", "Active"}, {"on-hold", "On hold"}, {"pending-cancel", "Pending cancellation"},
	{"cancelled", "Cancelled"}, {"expired", "Expired"},
}

func gsubStatusLabel(st string) string {
	for _, o := range gsubStatuses {
		if o.Value == st {
			return o.Label
		}
	}
	return st
}

var gsubPeriods = []gooOpt{{"day", "day"}, {"week", "week"}, {"month", "month"}, {"year", "year"}}

// gsubItem is a recurring line of a subscription.
type gsubItem struct {
	ProductID   int64   `json:"product_id"`
	VariationID int64   `json:"variation_id"`
	Name        string  `json:"name"`
	Qty         int     `json:"qty"`
	Price       float64 `json:"price"` // recurring unit price (entered terms)
	Total       float64 `json:"total"` // recurring line total excluding tax
	Tax         float64 `json:"tax"`
	TaxClass    string  `json:"tax_class"`
}

// gooSubscription is a WC_Subscription.
type gooSubscription struct {
	ID                 int64
	ParentOrderID      int64
	CustomerID         int64
	Status             string
	Period             string
	Interval           int
	Items              []gsubItem
	Currency           string
	RecurringTotal     float64
	RecurringTax       float64
	Start              time.Time
	TrialEnd           *time.Time
	NextPayment        *time.Time
	End                *time.Time
	LastOrderID        int64
	Billing            map[string]string
	Shipping           map[string]string
	PaymentMethod      string
	PaymentMethodTitle string
	Meta               map[string]any
	SyncDay            int // synchronised renewal day (gooSubsSyncDay), 0 = not synced
	DateCreated        time.Time
	DateModified       time.Time
}

const gsubCols = `id, parent_order_id, customer_id, status, billing_period, billing_interval, items, currency, recurring_total::float8,
	recurring_tax::float8, start_date, trial_end, next_payment, end_date, last_order_id, billing, shipping, payment_method,
	payment_method_title, meta, date_created, date_modified, sync_day`

func gsubScan(row pgx.Row) (*gooSubscription, error) {
	sub := &gooSubscription{}
	var items, billing, shipping, meta []byte
	err := row.Scan(&sub.ID, &sub.ParentOrderID, &sub.CustomerID, &sub.Status, &sub.Period, &sub.Interval, &items, &sub.Currency,
		&sub.RecurringTotal, &sub.RecurringTax, &sub.Start, &sub.TrialEnd, &sub.NextPayment, &sub.End, &sub.LastOrderID, &billing,
		&shipping, &sub.PaymentMethod, &sub.PaymentMethodTitle, &meta, &sub.DateCreated, &sub.DateModified, &sub.SyncDay)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(items, &sub.Items)
	_ = json.Unmarshal(billing, &sub.Billing)
	_ = json.Unmarshal(shipping, &sub.Shipping)
	_ = json.Unmarshal(meta, &sub.Meta)
	if sub.Meta == nil {
		sub.Meta = map[string]any{}
	}
	if sub.Billing == nil {
		sub.Billing = map[string]string{}
	}
	if sub.Shipping == nil {
		sub.Shipping = map[string]string{}
	}
	return sub, nil
}

// gsubGet is wcs_get_subscription().
func (s *Server) gsubGet(ctx context.Context, id int64) (*gooSubscription, error) {
	return gsubScan(s.store.Pool.QueryRow(ctx, `SELECT `+gsubCols+` FROM goo_subscriptions WHERE id = $1`, id))
}

// gsubQuery lists subscriptions.
type gsubQuery struct {
	Status        []string
	CustomerID    int64
	ParentOrderID int64
	ProductID     int64
	Search        string
	Due           bool  // active with next_payment <= now
	Ended         bool  // end_date <= now, still active / on-hold / pending-cancel candidates (the caller sets Status)
	PendingRenew  bool  // an unpaid renewal order is recorded (meta _pending_renewal)
	BeforeID      int64 // keyset paging: only subscriptions with id < BeforeID
	Page, PerPage int
}

func (s *Server) gsubList(ctx context.Context, q gsubQuery) ([]*gooSubscription, int, error) {
	conds := []string{"true"}
	var args []any
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if len(q.Status) > 0 {
		conds = append(conds, "status = ANY("+add(q.Status)+")")
	}
	if q.CustomerID != 0 {
		conds = append(conds, "customer_id = "+add(q.CustomerID))
	}
	if q.ParentOrderID != 0 {
		conds = append(conds, "parent_order_id = "+add(q.ParentOrderID))
	}
	if q.ProductID != 0 {
		conds = append(conds, "items @> "+add(fmt.Sprintf(`[{"product_id":%d}]`, q.ProductID))+"::jsonb")
	}
	if q.Due {
		conds = append(conds, "status = 'active' AND next_payment IS NOT NULL AND next_payment <= now()")
	}
	if q.Ended {
		conds = append(conds, "end_date IS NOT NULL AND end_date <= now()")
	}
	if q.PendingRenew {
		conds = append(conds, "meta ? '_pending_renewal'")
	}
	if q.BeforeID > 0 {
		conds = append(conds, "id < "+add(q.BeforeID))
	}
	if q.Search != "" {
		p := add("%" + strings.ToLower(q.Search) + "%")
		cond := "(lower(billing::text) LIKE " + p + " OR lower(items::text) LIKE " + p
		if n, err := strconv.ParseInt(strings.TrimPrefix(q.Search, "#"), 10, 64); err == nil {
			cond += " OR id = " + add(n)
		}
		conds = append(conds, cond+")")
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM goo_subscriptions WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	sql := `SELECT ` + gsubCols + ` FROM goo_subscriptions WHERE ` + where + ` ORDER BY id DESC`
	if q.PerPage > 0 {
		// Bounded here for every caller (admin ?paged= is not capped upstream), so the OFFSET cannot overflow.
		sql += fmt.Sprintf(" LIMIT %d OFFSET %d", q.PerPage, (min(max(q.Page, 1), store.MaxPage)-1)*q.PerPage)
	}
	rows, err := s.store.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*gooSubscription
	for rows.Next() {
		sub, err := gsubScan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, sub)
	}
	return out, total, rows.Err()
}

// gsubBatchSize is how many subscriptions the hourly job holds in memory at once.
var gsubBatchSize = 200

// gsubEach runs fn over the subscriptions matching q in id-descending batches (keyset paging, so fn may
// change the rows it is given without skipping any).
func (s *Server) gsubEach(ctx context.Context, q gsubQuery, fn func(*gooSubscription)) {
	q.Page, q.PerPage = 1, gsubBatchSize
	for {
		list, _, err := s.gsubList(ctx, q)
		if err != nil || len(list) == 0 {
			return
		}
		for _, sub := range list {
			fn(sub)
		}
		if len(list) < gsubBatchSize {
			return
		}
		q.BeforeID = list[len(list)-1].ID
	}
}

// gsubSave inserts or updates a subscription.
func (s *Server) gsubSave(ctx context.Context, sub *gooSubscription) error {
	if sub.Meta == nil {
		sub.Meta = map[string]any{}
	}
	if sub.Interval < 1 {
		sub.Interval = 1
	}
	items, _ := json.Marshal(sub.Items)
	billing, _ := json.Marshal(sub.Billing)
	shipping, _ := json.Marshal(sub.Shipping)
	meta, _ := json.Marshal(sub.Meta)
	if sub.ID == 0 {
		if sub.Start.IsZero() {
			sub.Start = time.Now()
		}
		err := s.store.Pool.QueryRow(ctx, `INSERT INTO goo_subscriptions (parent_order_id, customer_id, status, billing_period, billing_interval,
			items, currency, recurring_total, recurring_tax, start_date, trial_end, next_payment, end_date, last_order_id, billing, shipping,
			payment_method, payment_method_title, meta, sync_day) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
			RETURNING id, date_created, date_modified`,
			sub.ParentOrderID, sub.CustomerID, sub.Status, sub.Period, sub.Interval, items, sub.Currency, sub.RecurringTotal, sub.RecurringTax,
			sub.Start, sub.TrialEnd, sub.NextPayment, sub.End, sub.LastOrderID, billing, shipping, sub.PaymentMethod, sub.PaymentMethodTitle,
			meta, sub.SyncDay).Scan(&sub.ID, &sub.DateCreated, &sub.DateModified)
		if err == nil {
			s.gsubWebhook(ctx, "subscription.created", sub)
		}
		return err
	}
	_, err := s.store.Pool.Exec(ctx, `UPDATE goo_subscriptions SET customer_id=$2, status=$3, billing_period=$4, billing_interval=$5, items=$6,
		currency=$7, recurring_total=$8, recurring_tax=$9, start_date=$10, trial_end=$11, next_payment=$12, end_date=$13, last_order_id=$14,
		billing=$15, shipping=$16, payment_method=$17, payment_method_title=$18, meta=$19, sync_day=$20, date_modified=now() WHERE id=$1`,
		sub.ID, sub.CustomerID, sub.Status, sub.Period, sub.Interval, items, sub.Currency, sub.RecurringTotal, sub.RecurringTax, sub.Start,
		sub.TrialEnd, sub.NextPayment, sub.End, sub.LastOrderID, billing, shipping, sub.PaymentMethod, sub.PaymentMethodTitle, meta, sub.SyncDay)
	if err == nil {
		s.gsubWebhook(ctx, "subscription.updated", sub)
	}
	return err
}

// gsubAddNote adds a note to a subscription (kept in its meta, newest first).
func (s *Server) gsubAddNote(ctx context.Context, sub *gooSubscription, note string, customer bool, by string) {
	notes := gxaNoteList(sub.Meta["_notes"])
	n := map[string]any{"date": time.Now().UTC().Format(time.RFC3339), "content": note, "customer": customer, "by": by}
	all := append([]any{n}, func() []any {
		out := make([]any, 0, len(notes))
		for _, x := range notes {
			out = append(out, x)
		}
		return out
	}()...)
	sub.Meta["_notes"] = all
}

// gsubUpdateStatus is WC_Subscription::update_status(): notes, dates and e-mails of the transition.
func (s *Server) gsubUpdateStatus(ctx context.Context, sub *gooSubscription, to, note, by string) error {
	from := sub.Status
	if from == to {
		if note != "" {
			s.gsubAddNote(ctx, sub, note, false, by)
			return s.gsubSave(ctx, sub)
		}
		return nil
	}
	now := time.Now()
	switch to {
	case "cancelled", "expired":
		sub.NextPayment = nil
		if sub.End == nil || sub.End.After(now) {
			sub.End = &now
		}
	case "pending-cancel":
		// The subscription stays usable until the end of the prepaid period.
		if sub.NextPayment != nil && sub.NextPayment.After(now) {
			end := *sub.NextPayment
			sub.End = &end
		} else {
			sub.End = &now
		}
		sub.NextPayment = nil
	case "active":
		// Paid, or reactivated by the store: nothing is owed any more (the customer's own Reactivate
		// is not offered while it is, gsubCustomerActions).
		delete(sub.Meta, "_unpaid_renewal")
		if from == "on-hold" && sub.NextPayment != nil && sub.NextPayment.Before(now) {
			// Reactivated after a suspension: the next payment is one period from now.
			np := gxaAddPeriod(now, sub.Period, sub.Interval)
			sub.NextPayment = &np
		}
	}
	sub.Status = to
	msg := fmt.Sprintf("Status changed from %s to %s.", gsubStatusLabel(from), gsubStatusLabel(to))
	if note != "" {
		msg = strings.TrimSpace(note + " " + msg)
	}
	s.gsubAddNote(ctx, sub, msg, false, by)
	if err := s.gsubSave(ctx, sub); err != nil {
		return err
	}
	parent, _ := s.gooGetOrder(ctx, sub.ParentOrderID)
	args := map[string]any{"subscription": sub}
	switch to {
	case "cancelled":
		if from != "pending-cancel" {
			_ = s.gooTriggerEmail(ctx, "cancelled_subscription", parent, args)
		}
	case "pending-cancel":
		_ = s.gooTriggerEmail(ctx, "cancelled_subscription", parent, args)
	case "expired":
		_ = s.gooTriggerEmail(ctx, "expired_subscription", parent, args)
	case "on-hold":
		_ = s.gooTriggerEmail(ctx, "suspended_subscription", parent, args)
	}
	s.gooFire(ctx, "subscription_status_changed", &gsubStatusChange{Sub: sub, From: from, To: to})
	return nil
}

// gsubStatusChange is the argument of the module's subscription_status_changed action (Memberships
// tied to subscriptions could follow it).
type gsubStatusChange struct {
	Sub      *gooSubscription
	From, To string
}

// Product type -----------------------------------------------------------------------------------------

// gsubSchedule is a subscription product's billing schedule (its _subscription_* meta).
type gsubSchedule struct {
	Price       float64
	Interval    int
	Period      string
	Length      int // periods, 0 = never expires
	SignUpFee   float64
	TrialLength int
	TrialPeriod string
	Limit       string // no, active, any
	SyncDay     int    // _subscription_payment_sync_date (week 1-7, month 1-28, year MMDD), 0 = not synced
}

func gsubScheduleOf(p *gooProduct) gsubSchedule {
	m := p.Meta
	if m == nil {
		m = map[string]any{}
	}
	sc := gsubSchedule{Price: p.PriceValue(), Interval: max(gxaInt(m, "_subscription_period_interval"), 1),
		Period: defaultStr(gxaStr(m, "_subscription_period"), "month"), Length: gxaInt(m, "_subscription_length"),
		SignUpFee: gxaFloat(m, "_subscription_sign_up_fee"), TrialLength: gxaInt(m, "_subscription_trial_length"),
		TrialPeriod: defaultStr(gxaStr(m, "_subscription_trial_period"), "day"), Limit: defaultStr(gxaStr(m, "_subscription_limit"), "no"),
		SyncDay: gooSubsSyncDayOf(m)}
	if !p.HasPrice() {
		sc.Price = gxaFloat(m, "_subscription_price")
	}
	return sc
}

// gsubIsSubscription reports whether a product (or a variation's parent) is a subscription.
func (s *Server) gsubIsSubscription(ctx context.Context, p *gooProduct) bool {
	if p == nil {
		return false
	}
	if p.Type == "subscription" {
		return true
	}
	if p.Type == gooTypeVariation {
		if parent := s.gooParent(ctx, p); parent != nil && parent.Type == "variable-subscription" {
			return true
		}
	}
	return false
}

// gsubPriceString is the subscription details after a price: " / month for 6 months with a 7-day free
// trial and a $5 sign-up fee" (wcs_price_string()).
func (s *Server) gsubPriceString(sc gsubSchedule) string {
	var b strings.Builder
	if sc.Interval == 1 {
		b.WriteString(" / " + sc.Period)
	} else {
		b.WriteString(" every " + gxaPeriodLabel(sc.Period, sc.Interval))
	}
	if sc.SyncDay > 0 && s.gooExtBool(gsubKey, "sync_payments") {
		b.WriteString(gooSubsSyncLabel(sc.Period, sc.SyncDay))
	}
	if sc.Length > 0 {
		b.WriteString(" for " + gxaPeriodLabel(sc.Period, sc.Length))
	}
	if sc.TrialLength > 0 {
		b.WriteString(fmt.Sprintf(" with a %d-%s free trial", sc.TrialLength, strings.TrimSuffix(sc.TrialPeriod, "s")))
	}
	if sc.SignUpFee > 0 {
		b.WriteString(" and a " + string(s.gooPrice(sc.SignUpFee)) + " sign-up fee")
	}
	return b.String()
}

// gsubHasSubscription reports whether a user has a subscription to a product in the given statuses.
func (s *Server) gsubHasSubscription(ctx context.Context, userID, productID int64, statuses []string) bool {
	if userID == 0 {
		return false
	}
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM goo_subscriptions WHERE customer_id = $1 AND status = ANY($2)
		AND items @> $3::jsonb`, userID, statuses, fmt.Sprintf(`[{"product_id":%d}]`, productID)).Scan(&n)
	return n > 0
}

var gsubLiveStatuses = []string{"pending", "active", "on-hold", "pending-cancel"}

func init() {
	gooRegisterExtension(&gooExtension{
		Key: gsubKey, Name: "Subscriptions", Icon: "dashicons-update", Category: "Payments & checkout",
		Description: "Sell products and services with recurring payments: weekly, monthly or annual billing, sign-up fees, free trials and expiration dates, renewal orders, and a Subscriptions area in My Account where customers manage theirs.",
		Fields:      gsubFields(),
		Pages:       []gooExtMenuItem{{Page: "subscriptions", Title: "Subscriptions"}},
	})
	gooExtAdminPages["subscriptions"] = func(s *Server, c *gin.Context) { s.gsubAdmin(c) }

	gooRegisterProductType(&gooProductTypeDef{Type: "subscription", Label: "Simple subscription", Module: gsubKey,
		Purchasable: func(s *Server, ctx context.Context, p *gooProduct) bool {
			if !s.gooExtOn(gsubKey) || !p.HasPrice() {
				return false
			}
			sc := gsubScheduleOf(p)
			if u := gxaUser(ctx); u != nil && sc.Limit != "no" && !gooSubsSwitching(ctx) {
				statuses := gsubLiveStatuses
				if sc.Limit == "any" {
					statuses = append([]string{"cancelled", "expired"}, gsubLiveStatuses...)
				}
				if s.gsubHasSubscription(ctx, u.ID, p.ID, statuses) {
					return false
				}
			}
			return true
		},
		AddToCartText: func(s *Server, p *gooProduct, single bool) string {
			return defaultStr(s.gooExtString(gsubKey, "add_to_cart_button_text"), "Sign up now")
		},
	})

	gooProductPanels = append(gooProductPanels, &gooProductPanel{ID: "subscription_pricing", Label: "Subscription", Module: gsubKey,
		ShowFor: []string{"subscription", gooSubsTypeVariable}, Render: gsubPanelRender, Save: gsubPanelSave})

	gooPriceHTMLHooks = append(gooPriceHTMLHooks, func(s *Server, ctx context.Context, p *gooProduct, h template.HTML) template.HTML {
		if !s.gooExtOn(gsubKey) || p == nil {
			return h
		}
		switch {
		case p.Type == "subscription", p.Type == gooTypeVariation && s.gsubIsSubscription(ctx, p):
			return h + template.HTML(`<span class="subscription-details">`+s.gsubPriceString(gsubScheduleOf(p))+`</span>`)
		case p.Type == gooSubsTypeVariable:
			return s.gooSubsVariablePriceHTML(ctx, p, h)
		}
		return h
	})

	// Mixed checkout: subscriptions and other products may not share a cart when switched off.
	gooAddToCartHooks = append(gooAddToCartHooks, func(s *Server, ctx context.Context, p *gooProduct, qty int, form map[string][]string, data map[string]any) error {
		if !s.gooExtOn(gsubKey) || s.gooExtBool(gsubKey, "multiple_purchase") {
			return nil
		}
		cart := gxaCart(ctx)
		if cart == nil || cart.IsEmpty() {
			return nil
		}
		adding := s.gsubIsSubscription(ctx, p)
		for _, it := range cart.Items {
			if s.gsubIsSubscription(ctx, it.Product()) != adding {
				return gooCartErr("Products and subscriptions can not be purchased at the same time.")
			}
			if adding && it.ProductID != p.ID && it.ProductID != p.ParentID {
				return gooCartErr("A subscription can not be purchased at the same time as another subscription.")
			}
		}
		return nil
	})

	// The first payment: the sign-up fee plus the first period's price, unless there is a free trial.
	gooCartItemPriceHooks = append(gooCartItemPriceHooks, func(s *Server, ctx context.Context, cart *gooCart, it *gooCartItem) {
		if !s.gooExtOn(gsubKey) || !s.gsubIsSubscription(ctx, it.Product()) {
			return
		}
		if gooSubsSwitchData(it) != nil {
			// A switch: the prorated difference (and sign-up fee) of gooSubsSwitchQuote is due now.
			if q, err := s.gooSubsSwitchQuote(ctx, it, it.Price); err == nil {
				it.Price = q.Charge
			}
			return
		}
		sc := gsubScheduleOf(it.Product())
		frac, _ := s.gooSubsFirstPayment(it.Product(), sc, time.Now())
		it.Price = gooRound(it.Price*frac, s.gooDecimals()) + sc.SignUpFee
	})

	gooCartItemDataHooks = append(gooCartItemDataHooks, func(s *Server, ctx context.Context, it *gooCartItem) []gooKV {
		if !s.gooExtOn(gsubKey) || !s.gsubIsSubscription(ctx, it.Product()) {
			return nil
		}
		sc := gsubScheduleOf(it.Product())
		sc.Price = s.gooDisplayPrice(ctx, it.Product())
		return []gooKV{{Key: "Recurring", Value: s.gooPricePlain(sc.Price, s.gooCurrency()) + gsubPlain(s.gsubPriceString(sc))}}
	})

	for _, loc := range []string{"cart_after", "checkout_after_order_review"} {
		gooOnPage(loc, func(s *Server, c *gin.Context) template.HTML {
			if !s.gooExtOn(gsubKey) {
				return ""
			}
			return s.gsubRecurringTotals(c, s.gooCartFor(c))
		})
	}

	// A guest buying a subscription must create an account: refused before any account is made.
	gooCheckoutValidateHooks = append(gooCheckoutValidateHooks, func(s *Server, c *gin.Context, cart *gooCart, d *gooCheckoutData) []string {
		if !s.gooExtOn(gsubKey) || currentUser(c) != nil || d.CreateAccount {
			return nil
		}
		for _, it := range cart.Items {
			if s.gsubIsSubscription(c.Request.Context(), it.Product()) {
				return []string{"You must be logged in or create an account to purchase a subscription."}
			}
		}
		return nil
	})

	// A cart with a subscription needs an account; the order records each item's schedule.
	gooCheckoutHooks = append(gooCheckoutHooks, func(s *Server, ctx context.Context, cart *gooCart, o *gooOrder) error {
		if !s.gooExtOn(gsubKey) {
			return nil
		}
		has := false
		for _, it := range cart.Items {
			if s.gsubIsSubscription(ctx, it.Product()) {
				has = true
			}
		}
		if !has {
			return nil
		}
		if o.CustomerID == 0 {
			return errors.New("You must be logged in or create an account to purchase a subscription.")
		}
		o.Meta["_has_subscription"] = true
		lines := o.ItemsOf("line_item")
		for i, it := range cart.Items {
			if i >= len(lines) || !s.gsubIsSubscription(ctx, it.Product()) {
				continue
			}
			sc := gsubScheduleOf(it.Product())
			unit := s.gooDisplayPrice(ctx, it.Product())
			line := gooRound(unit*float64(it.Quantity), s.gooDecimals())
			tax := 0.0
			if it.Subtotal > 0 {
				tax = gooRound(it.LineSubtotalTax*line/it.Subtotal, s.gooDecimals())
			}
			if s.gooPricesIncludeTax() && tax > 0 {
				line = gooRound(line-tax, s.gooDecimals())
			}
			m := lines[i].Meta
			m["_subscription_recurring_amount"] = unit
			m["_subscription_recurring_line_total"] = line
			m["_subscription_recurring_line_tax"] = tax
			m["_subscription_period"] = sc.Period
			m["_subscription_interval"] = float64(sc.Interval)
			m["_subscription_length"] = float64(sc.Length)
			m["_subscription_trial_length"] = float64(sc.TrialLength)
			m["_subscription_trial_period"] = sc.TrialPeriod
			m["_subscription_sign_up_fee"] = sc.SignUpFee
			if sc.SyncDay > 0 && s.gooExtBool(gsubKey, "sync_payments") && sc.Period != "day" {
				m["_subscription_sync_day"] = float64(sc.SyncDay)
			}
		}
		return nil
	})

	gooOn("order_status_changed", 20, func(s *Server, ctx context.Context, arg any) {
		ch, ok := arg.(*gooStatusChange)
		if !ok || !s.gooExtOn(gsubKey) {
			return
		}
		s.gsubOrderStatusChanged(ctx, ch.Order, ch.From, ch.To)
	})
	gooOn("payment_complete", 20, func(s *Server, ctx context.Context, arg any) {
		if o, ok := arg.(*gooOrder); ok && s.gooExtOn(gsubKey) && containsStr(gooPaidStatuses, o.Status) {
			s.gooSubsApplySwitch(ctx, o)
			s.gsubCreateFromOrder(ctx, o)
		}
	})
	gooOn("hourly", 20, func(s *Server, ctx context.Context, _ any) {
		if s.gooExtOn(gsubKey) {
			s.gsubScheduler(ctx)
		}
	})

	gooAccountEndpoints = append(gooAccountEndpoints,
		&gooAccountEndpoint{Slug: "subscriptions", Title: "Subscriptions", Module: gsubKey, After: "orders", Render: gsubAccountList},
		&gooAccountEndpoint{Slug: "view-subscription", Title: "", Module: gsubKey, After: "subscriptions", Render: gsubAccountView})
	gooFrontRoutes = append(gooFrontRoutes, gsubFrontRoute)
	gooGatewayFilters = append(gooGatewayFilters, gsubGatewayFilter)
	gooPlaceOrderTextHooks = append(gooPlaceOrderTextHooks, func(s *Server, ctx context.Context, cart *gooCart) string {
		if s.gooExtOn(gsubKey) && s.gsubCartHas(ctx, cart) {
			return s.gooExtString(gsubKey, "order_button_text")
		}
		return ""
	})

	for _, e := range []*gooEmailDef{
		{ID: "new_renewal_order", Title: "New Renewal Order", Description: "New renewal order emails are sent when a subscription renewal payment is processed.",
			Subject: "[{site_title}] New subscription renewal order ({order_number}) - {order_date}", Heading: "New subscription renewal order"},
		{ID: "customer_renewal_invoice", Title: "Customer Renewal Invoice", Customer: true,
			Description: "Sent to a customer when the subscription is due for renewal and the renewal requires a manual payment. The email contains renewal order information and payment links.",
			Subject:     "Invoice for renewal order {order_number} from {order_date}", Heading: "Invoice for renewal order {order_number}"},
		{ID: "customer_processing_renewal_order", Title: "Processing Renewal order", Customer: true,
			Description: "This is an order notification sent to the customer after payment for a subscription renewal order is completed. It contains the renewal order details.",
			Subject:     "Your {site_title} renewal order receipt from {order_date}", Heading: "Thank you for your order"},
		{ID: "cancelled_subscription", Title: "Cancelled Subscription",
			Description: "Cancelled Subscription emails are sent when a customer's subscription is cancelled (either by a store manager, or by the customer).",
			Subject:     "[{site_title}] Subscription Cancelled", Heading: "Subscription Cancelled"},
		{ID: "expired_subscription", Title: "Expired Subscription",
			Description: "Expired Subscription emails are sent when a customer's subscription expires.",
			Subject:     "[{site_title}] Subscription Expired", Heading: "Subscription Expired"},
		{ID: "suspended_subscription", Title: "Suspended Subscription",
			Description: "Suspended Subscription emails are sent when a customer manually suspends their subscription.",
			Subject:     "[{site_title}] Subscription Suspended", Heading: "Subscription Suspended"},
	} {
		e.Module = gsubKey
		gooRegisterEmail(e)
	}
}

// gsubFields are Goocommerce › Settings › Subscriptions.
func gsubFields() []gooField {
	return []gooField{
		{ID: "button_text_title", Title: "Button Text", Type: "title"},
		{ID: "add_to_cart_button_text", Title: "Add to Cart Button Text", Type: "text", Default: "Sign up now", Desc: "A product displays a button with the text \"Add to cart\". By default, a subscription changes this to \"Sign up now\". You can customise the button text for subscriptions here."},
		{ID: "order_button_text", Title: "Place Order Button Text", Type: "text", Default: "Sign up now", Desc: "Use this field to customise the text displayed on the checkout button when an order contains a subscription."},
		{ID: "button_text_end", Type: "sectionend"},
		{ID: "renewals_title", Title: "Renewals", Type: "title"},
		{ID: "accept_manual_renewals", Title: "Manual Renewal Payments", Type: "checkbox", Default: false, Desc: "Accept Manual Renewals"},
		{ID: "turn_off_automatic_payments", Title: "Turn off Automatic Payments", Type: "checkbox", Default: false, Desc: "Turn off Automatic Payments (renewal orders are invoiced to the customer)"},
		{ID: "enable_early_renewal", Title: "Early Renewal", Type: "checkbox", Default: true, Desc: "Accept Early Renewal Payments (customers can renew from My Account)"},
		{ID: "renewal_grace_days", Title: "Renewal grace period", Type: "number", Default: 3.0, Min: "0", Desc: "Days an unpaid renewal order may stay pending before its subscription is placed on hold."},
		{ID: "renewals_end", Type: "sectionend"},
		{ID: "switching_title", Title: "Switching", Type: "title", Desc: "Allow subscribers to switch (upgrade or downgrade) between different subscriptions."},
		{ID: "allow_switching", Title: "Allow Switching", Type: "select", Default: "no", Options: []gooOpt{{"no", "Never"}, {"variable", "Between Subscription Variations"}, {"grouped", "Between Grouped Subscriptions"}, {"variable_grouped", "Between Both Variations & Grouped Subscriptions"}}},
		{ID: "apportion_recurring_price", Title: "Prorate Recurring Payment", Type: "select", Default: "no", Desc: "When switching to a subscription with a different recurring payment or billing period, should the price paid for the existing billing period be prorated when switching to the new subscription?",
			Options: []gooOpt{{"no", "Never"}, {"virtual-upgrade", "For Upgrades of Virtual Subscription Products Only"}, {"yes-upgrade", "For Upgrades of All Subscription Products"}, {"virtual", "For Upgrades & Downgrades of Virtual Subscription Products Only"}, {"yes", "For Upgrades & Downgrades of All Subscription Products"}}},
		{ID: "apportion_sign_up_fee", Title: "Prorate Sign up Fee", Type: "select", Default: "no", Desc: "When switching to a subscription with a sign up fee, you can require the customer pay only the gap between the existing subscription's sign up fee and the new subscription's sign up fee (if any).",
			Options: []gooOpt{{"no", "Never (do not charge a sign up fee)"}, {"full", "Never (charge the full sign up fee)"}, {"yes", "Always"}}},
		{ID: "apportion_length", Title: "Prorate Subscription Length", Type: "select", Default: "no", Desc: "When switching to a subscription with a length, you can take into account the payments already completed by the customer when determining how many payments the subscriber needs to make for the new subscription.",
			Options: []gooOpt{{"no", "Never"}, {"virtual", "For Virtual Subscription Products Only"}, {"yes", "For All Subscription Products"}}},
		{ID: "switch_button_text", Title: "Switch Button Text", Type: "text", Default: "Upgrade or Downgrade", Desc: "Customise the text displayed on the button next to the subscription on the subscriber's account page."},
		{ID: "switching_end", Type: "sectionend"},
		{ID: "sync_title", Title: "Synchronisation", Type: "title", Desc: "Align subscription renewal to a specific day of the week, month or year."},
		{ID: "sync_payments", Title: "Synchronise renewals", Type: "checkbox", Default: false, Desc: "Align Subscription Renewal Day"},
		{ID: "prorate_synced_payments", Title: "Prorate First Renewal", Type: "select", Default: "no", Desc: "If a subscription is synchronised to a specific day of the week, month or year, charge a prorated amount for the subscription at the time of sign up.",
			Options: []gooOpt{{"no", "Never (do not charge any recurring amount)"}, {"recurring", "Never (charge the full recurring amount at sign-up)"}, {"virtual", "For Virtual Subscription Products Only"}, {"yes", "For All Subscription Products"}}},
		{ID: "days_no_fee", Title: "Days with no fee", Type: "number", Default: 0.0, Min: "0", Desc: "Days before the synchronised renewal day during which a new subscriber is not charged a prorated amount up front; the first renewal is still the synchronised day."},
		{ID: "sync_end", Type: "sectionend"},
		{ID: "misc_title", Title: "Miscellaneous", Type: "title"},
		{ID: "max_customer_suspensions", Title: "Customer Suspensions", Type: "number", Default: 0.0, Min: "0", Desc: "Set a maximum number of times a customer can suspend their account for each billing period. Use 0 to turn off customer suspensions."},
		{ID: "cancel_at_period_end", Title: "Customer Cancellation", Type: "checkbox", Default: true, Desc: "Keep a cancelled subscription active (pending cancellation) until the end of the prepaid period"},
		{ID: "multiple_purchase", Title: "Mixed Checkout", Type: "checkbox", Default: true, Desc: "Allow multiple subscriptions and products to be purchased simultaneously."},
		{ID: "misc_end", Type: "sectionend"},
	}
}

func gsubPlain(h string) string {
	return strings.NewReplacer("&nbsp;", " ").Replace(gxaStripTags(h))
}

// gxaStripTags drops HTML tags (wp_strip_all_tags for the short strings built here).
func gxaStripTags(h string) string {
	var b strings.Builder
	in := false
	for _, r := range h {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Product panel ---------------------------------------------------------------------------------------

func gsubPanelRender(s *Server, c *gin.Context, p *gooProduct) template.HTML {
	sc := gsubScheduleOf(p)
	price := ""
	if p.RegularPrice != nil {
		price = gooFormatDecimal(*p.RegularPrice, s.gooDecimals())
	}
	var intervals, lengths []gooOpt
	for i := 1; i <= 6; i++ {
		label := "every"
		if i > 1 {
			label = "every " + gxaOrdinal(i)
		}
		intervals = append(intervals, gooOpt{strconv.Itoa(i), label})
	}
	lengths = append(lengths, gooOpt{"0", "Never expire"})
	for i := 1; i <= 24; i++ {
		lengths = append(lengths, gooOpt{strconv.Itoa(i), gxaPeriodLabel(sc.Period, i)})
	}
	cur := gooCurrencySymbol(s.gooCurrency())
	fee := ""
	if sc.SignUpFee > 0 {
		fee = gooFormatDecimal(sc.SignUpFee, s.gooDecimals())
	}
	var b strings.Builder
	b.WriteString(`<div class="options_group subscription_pricing show_if_subscription">`)
	b.WriteString(`<p class="form-field _subscription_price_fields _subscription_price_field"><label for="_subscription_price">Subscription price (` + cur + `)</label>`)
	b.WriteString(`<span class="wrap"><input type="text" id="_subscription_price" name="_subscription_price" class="wc_input_price wc_input_subscription_price" placeholder="e.g. 5.90" value="` + gooEsc(price) + `">`)
	b.WriteString(gxaSelect("_subscription_period_interval", "_subscription_period_interval", intervals, strconv.Itoa(sc.Interval), ` class="wc_input_subscription_period_interval"`))
	b.WriteString(gxaSelect("_subscription_period", "_subscription_period", gsubPeriods, sc.Period, ` class="wc_input_subscription_period last"`))
	b.WriteString(`</span></p>`)
	b.WriteString(`<p class="form-field _subscription_length_field"><label for="_subscription_length">Expire after</label>` +
		gxaSelect("_subscription_length", "_subscription_length", lengths, strconv.Itoa(sc.Length), ` class="wc_input_subscription_length select short"`) +
		`<span class="description">Automatically expire the subscription after this length of time. This length is in addition to any free trial or amount of time provided before a synchronised first renewal date.</span></p>`)
	b.WriteString(`<p class="form-field _subscription_sign_up_fee_field"><label for="_subscription_sign_up_fee">Sign-up fee (` + cur + `)</label>` +
		`<input type="text" class="wc_input_price wc_input_subscription_intial_price short" name="_subscription_sign_up_fee" id="_subscription_sign_up_fee" value="` + gooEsc(fee) + `" placeholder="e.g. 9.90">` +
		`<span class="description">Optionally include an amount to be charged at the outset of the subscription. The sign-up fee will be charged immediately, even if the product has a free trial or the payment dates are synced.</span></p>`)
	b.WriteString(s.gooSubsSyncField(sc, "_subscription_payment_sync_date", "", "form-field"))
	b.WriteString(`<p class="form-field _subscription_trial_length_field"><label for="_subscription_trial_length">Free trial</label><span class="wrap">` +
		`<input type="text" id="_subscription_trial_length" name="_subscription_trial_length" class="wc_input_subscription_trial_length" value="` + strconv.Itoa(sc.TrialLength) + `">` +
		gxaSelect("_subscription_trial_period", "_subscription_trial_period", []gooOpt{{"day", "days"}, {"week", "weeks"}, {"month", "months"}, {"year", "years"}}, sc.TrialPeriod, ` class="wc_input_subscription_trial_period last"`) +
		`</span><span class="description">An optional period of time to wait before charging the first recurring payment. Any sign up fee will still be charged at the outset of the subscription.</span></p>`)
	b.WriteString(`</div><div class="options_group limit_subscription show_if_subscription show_if_variable-subscription">`)
	b.WriteString(`<p class="form-field _subscription_limit_field"><label for="_subscription_limit">Limit subscription</label>` +
		gxaSelect("_subscription_limit", "_subscription_limit", []gooOpt{{"no", "Do not limit"}, {"active", "Limit to one active subscription"}, {"any", "Limit to one of any status"}}, sc.Limit, ` class="select short"`) +
		`<span class="description">Only allow a customer to have one subscription to this product.</span></p></div>`)
	return template.HTML(b.String())
}

func gsubPanelSave(s *Server, c *gin.Context, p *gooProduct) {
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	if p.Type == gooSubsTypeVariable {
		// A variable subscription's schedules are its variations'; the parent keeps the limit.
		limit := c.PostForm("_subscription_limit")
		if !containsStr([]string{"no", "active", "any"}, limit) {
			limit = "no"
		}
		p.Meta["_subscription_limit"] = limit
		return
	}
	if v := s.gooParseAmount(c.PostForm("_subscription_price")); v != nil {
		p.RegularPrice = v
		p.Meta["_subscription_price"] = *v
	} else {
		p.RegularPrice = nil
		delete(p.Meta, "_subscription_price")
	}
	num := func(k string, lo, hi int) float64 {
		n, _ := strconv.Atoi(strings.TrimSpace(c.PostForm(k)))
		return float64(min(max(n, lo), hi))
	}
	p.Meta["_subscription_period_interval"] = num("_subscription_period_interval", 1, 6)
	period := c.PostForm("_subscription_period")
	if !containsStr([]string{"day", "week", "month", "year"}, period) {
		period = "month"
	}
	p.Meta["_subscription_period"] = period
	gooSubsSaveSyncDay(p.Meta, period, c.PostForm("_subscription_payment_sync_date"), c.PostForm("_subscription_payment_sync_date_day"), c.PostForm("_subscription_payment_sync_date_month"))
	p.Meta["_subscription_length"] = num("_subscription_length", 0, 1000)
	fee := 0.0
	if v := s.gooParseAmount(c.PostForm("_subscription_sign_up_fee")); v != nil && *v > 0 {
		fee = *v
	}
	p.Meta["_subscription_sign_up_fee"] = fee
	p.Meta["_subscription_trial_length"] = num("_subscription_trial_length", 0, 1000)
	tp := c.PostForm("_subscription_trial_period")
	if !containsStr([]string{"day", "week", "month", "year"}, tp) {
		tp = "day"
	}
	p.Meta["_subscription_trial_period"] = tp
	limit := c.PostForm("_subscription_limit")
	if !containsStr([]string{"no", "active", "any"}, limit) {
		limit = "no"
	}
	p.Meta["_subscription_limit"] = limit
}

// Cart ---------------------------------------------------------------------------------------------------

// gsubRecurringTotals is the "Recurring totals" rows under the cart / order review totals.
func (s *Server) gsubRecurringTotals(c *gin.Context, cart *gooCart) template.HTML {
	if cart == nil {
		return ""
	}
	ctx := c.Request.Context()
	var rows strings.Builder
	n := 0
	for _, it := range cart.Items {
		if !s.gsubIsSubscription(ctx, it.Product()) {
			continue
		}
		n++
		sc := gsubScheduleOf(it.Product())
		unit := s.gooDisplayPrice(ctx, it.Product())
		line := unit * float64(it.Quantity)
		tax := 0.0
		if it.Subtotal > 0 && it.LineSubtotalTax > 0 {
			tax = gooRound(it.LineSubtotalTax*line/it.Subtotal, s.gooDecimals())
		}
		total := line
		if !s.gooPricesIncludeTax() {
			total += tax
		}
		_, start := s.gooSubsFirstPayment(it.Product(), sc, time.Now())
		if gooSubsSwitchData(it) != nil {
			if q, err := s.gooSubsSwitchQuote(ctx, it, unit); err == nil && q.Next != nil {
				start = *q.Next
			}
		}
		sched := sc
		sched.SignUpFee, sched.TrialLength = 0, 0
		rows.WriteString(`<tr class="cart-subtotal recurring-total"><th rowspan="1">Subtotal</th><td data-title="Subtotal">` +
			string(s.gooPrice(line)) + `<span class="subscription-details">` + s.gsubPriceString(sched) + `</span></td></tr>`)
		if tax > 0 {
			rows.WriteString(`<tr class="tax-total recurring-total"><th>Tax</th><td data-title="Tax">` + string(s.gooPrice(tax)) + `</td></tr>`)
		}
		rows.WriteString(`<tr class="order-total recurring-total"><th rowspan="1">Recurring total</th><td data-title="Recurring total"><strong>` +
			string(s.gooPrice(total)) + `</strong><span class="subscription-details">` + s.gsubPriceString(sched) + `</span>` +
			`<div class="first-payment-date"><small>First renewal: ` + s.gxaDate(&start) + `</small></div></td></tr>`)
	}
	if n == 0 {
		return ""
	}
	return template.HTML(`<div class="cart_totals recurring_totals"><h2>Recurring totals</h2><table cellspacing="0" class="shop_table shop_table_responsive"><tbody>` +
		rows.String() + `</tbody></table></div>`)
}

// Orders → subscriptions ----------------------------------------------------------------------------

// gsubDropPendingRenewal forgets an unpaid renewal order that will never be paid. A regular renewal
// puts an active subscription on hold (the period was not paid for; the customer can pay from a new
// renewal or the store reactivates it); an early renewal just leaves the subscription as it was.
// The dropped regular renewal is remembered (_unpaid_renewal) so the customer cannot Reactivate the
// on-hold subscription without paying (WooCommerce Subscriptions offers no Reactivate while the
// subscription needs payment); a paid renewal or the store's own reactivation clears it.
func (s *Server) gsubDropPendingRenewal(ctx context.Context, sub *gooSubscription, orderID int64, early bool, note string) {
	if gxaInt64(sub.Meta, "_pending_renewal") != orderID {
		return
	}
	delete(sub.Meta, "_pending_renewal")
	if !early {
		sub.Meta["_unpaid_renewal"] = true
	}
	if !early && sub.Status == "active" {
		_ = s.gsubUpdateStatus(ctx, sub, "on-hold", note, "")
		return
	}
	s.gsubAddNote(ctx, sub, note, false, "")
	_ = s.gsubSave(ctx, sub)
}

func (s *Server) gsubOrderStatusChanged(ctx context.Context, o *gooOrder, from, to string) {
	if subID := gxaInt64(o.Meta, "_subscription_renewal"); subID > 0 {
		sub, err := s.gsubGet(ctx, subID)
		if err != nil {
			return
		}
		switch to {
		case "processing", "completed":
			sub.LastOrderID = o.ID
			// The suspension allowance is per billing period: a paid renewal starts a new one.
			delete(sub.Meta, "_suspension_count")
			delete(sub.Meta, "_unpaid_renewal")
			if gxaInt64(sub.Meta, "_pending_renewal") == o.ID {
				delete(sub.Meta, "_pending_renewal")
			}
			if o.Meta["_subscription_renewal_early"] == true && sub.NextPayment != nil {
				np := gxaAddPeriod(*sub.NextPayment, sub.Period, sub.Interval)
				if sub.End == nil || np.Before(*sub.End) {
					sub.NextPayment = &np
				}
			}
			if sub.Status == "on-hold" || sub.Status == "pending" {
				_ = s.gsubUpdateStatus(ctx, sub, "active", fmt.Sprintf("Payment received for renewal order #%d.", o.ID), "")
			} else {
				s.gsubAddNote(ctx, sub, fmt.Sprintf("Payment received for renewal order #%d.", o.ID), false, "")
				_ = s.gsubSave(ctx, sub)
			}
			if to == "processing" {
				_ = s.gooTriggerEmail(ctx, "customer_processing_renewal_order", o, map[string]any{"subscription": sub})
			}
		case "failed":
			// A failed early renewal leaves the paid period alone (the order can still be paid).
			if sub.Status == "active" && o.Meta["_subscription_renewal_early"] != true {
				_ = s.gsubUpdateStatus(ctx, sub, "on-hold", fmt.Sprintf("Payment failed for renewal order #%d.", o.ID), "")
			}
		case "cancelled", "trash":
			s.gsubDropPendingRenewal(ctx, sub, o.ID, o.Meta["_subscription_renewal_early"] == true,
				fmt.Sprintf("Renewal order #%d was cancelled.", o.ID))
		}
		return
	}
	if containsStr(gooPaidStatuses, to) {
		s.gooSubsApplySwitch(ctx, o)
		s.gsubCreateFromOrder(ctx, o)
	}
	if to == "cancelled" || to == "refunded" {
		// A cancelled or fully refunded parent order cancels its live subscriptions, like WooCommerce
		// Subscriptions (cancel_subscriptions_for_order / maybe_cancel_subscription_on_full_refund).
		subs, _, _ := s.gsubList(ctx, gsubQuery{ParentOrderID: o.ID, Status: gsubLiveStatuses})
		for _, sub := range subs {
			_ = s.gsubUpdateStatus(ctx, sub, "cancelled", "Parent order "+strings.ToLower(gooStatusLabel(to))+".", "")
		}
	}
}

// gsubCreateFromOrder creates the subscriptions of a paid order (once): one per billing schedule.
func (s *Server) gsubCreateFromOrder(ctx context.Context, o *gooOrder) []*gooSubscription {
	if o.Type != "shop_order" || o.CustomerID == 0 {
		return nil
	}
	// payment_complete and the status hook (a gateway callback and the customer's return) can both
	// arrive at once: a per-order advisory lock makes the "already created?" check and the inserts one step.
	lockKey := "goo_subscription_parent:" + itoa64(o.ID)
	if conn, err := s.store.Pool.Acquire(ctx); err == nil {
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, lockKey); err == nil {
			defer func() {
				_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey)
			}()
		}
	}
	var exists bool
	_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM goo_subscriptions WHERE parent_order_id = $1)`, o.ID).Scan(&exists)
	if exists {
		return nil
	}
	if len(o.Items) == 0 {
		o.Items, _ = s.gooOrderItems(ctx, o.ID)
	}
	groups := map[string]*gooSubscription{}
	var order []string
	now := time.Now()
	for _, it := range o.ItemsOf("line_item") {
		if it.Meta["_subscription_period"] == nil || it.Meta["_subscription_switch"] != nil {
			continue // switch lines change their subscription (gooSubsApplySwitch)
		}
		period := gxaStr(it.Meta, "_subscription_period")
		interval := max(gxaInt(it.Meta, "_subscription_interval"), 1)
		length := gxaInt(it.Meta, "_subscription_length")
		trialLen := gxaInt(it.Meta, "_subscription_trial_length")
		trialPer := gxaStr(it.Meta, "_subscription_trial_period")
		syncDay := gxaInt(it.Meta, "_subscription_sync_day")
		key := fmt.Sprintf("%s|%d|%d|%d|%s|%d", period, interval, length, trialLen, trialPer, syncDay)
		sub := groups[key]
		if sub == nil {
			sub = &gooSubscription{ParentOrderID: o.ID, CustomerID: o.CustomerID, Status: "active", Period: period, Interval: interval,
				Currency: o.Currency, Start: now, Billing: o.Billing, Shipping: o.Shipping, PaymentMethod: o.PaymentMethod,
				PaymentMethodTitle: o.PaymentMethodTitle, LastOrderID: o.ID, Meta: map[string]any{}}
			if tok := gxaStr(o.Meta, "_payment_token"); tok != "" {
				sub.Meta["_payment_token"] = tok
			}
			if trialLen > 0 {
				te := gxaAddPeriod(now, trialPer, trialLen)
				sub.TrialEnd = &te
			}
			first, synced := gooSubsFirstRenewal(now, period, interval, sub.TrialEnd, syncDay, s.loc())
			if syncDay > 0 {
				sub.SyncDay = syncDay
			}
			sub.NextPayment = &first
			if length > 0 {
				end := gxaAddPeriod(now, period, length)
				if sub.TrialEnd != nil {
					end = gxaAddPeriod(*sub.TrialEnd, period, length)
				}
				if synced {
					// The time before a synchronised first renewal is in addition to the length.
					end = gxaAddPeriod(first, period, length)
				}
				sub.End = &end
				if !sub.NextPayment.Before(end) {
					sub.NextPayment = nil
				}
			}
			groups[key] = sub
			order = append(order, key)
		}
		sub.Items = append(sub.Items, gsubItem{ProductID: it.ProductID, VariationID: it.VariationID, Name: it.Name, Qty: it.Qty(),
			Price: gxaFloat(it.Meta, "_subscription_recurring_amount"), Total: gxaFloat(it.Meta, "_subscription_recurring_line_total"),
			Tax: gxaFloat(it.Meta, "_subscription_recurring_line_tax"), TaxClass: it.TaxClass})
	}
	// Recurring shipping follows the parent order's shipping lines (on the first subscription).
	var out []*gooSubscription
	for i, key := range order {
		sub := groups[key]
		for _, line := range sub.Items {
			sub.RecurringTotal += line.Total + line.Tax
			sub.RecurringTax += line.Tax
		}
		if i == 0 {
			var ship []any
			for _, sl := range o.ItemsOf("shipping") {
				ship = append(ship, map[string]any{"name": sl.Name, "total": sl.Total, "tax": sl.TotalTax, "method_id": gxaStr(sl.Meta, "method_id")})
				sub.RecurringTotal += sl.Total + sl.TotalTax
				sub.RecurringTax += sl.TotalTax
			}
			if len(ship) > 0 {
				sub.Meta["_shipping_lines"] = ship
			}
		}
		sub.RecurringTotal = gooRound(sub.RecurringTotal, s.gooDecimals())
		sub.RecurringTax = gooRound(sub.RecurringTax, s.gooDecimals())
		s.gsubAddNote(ctx, sub, fmt.Sprintf("Subscription created from order #%d. Status set to Active.", o.ID), false, "")
		if err := s.gsubSave(ctx, sub); err == nil {
			out = append(out, sub)
			_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Subscription #%d created.", sub.ID), false, "")
		}
	}
	return out
}

// gsubCreateRenewal creates a renewal order for a subscription (wcs_create_renewal_order()).
func (s *Server) gsubCreateRenewal(ctx context.Context, sub *gooSubscription, early bool) (*gooOrder, error) {
	o := &gooOrder{Status: "pending", Currency: sub.Currency, PricesIncludeTax: false, CustomerID: sub.CustomerID,
		Billing: map[string]string{}, Shipping: map[string]string{}, PaymentMethod: sub.PaymentMethod, PaymentMethodTitle: sub.PaymentMethodTitle,
		CreatedVia: "subscription", Meta: map[string]any{"_subscription_renewal": float64(sub.ID)}}
	for k, v := range sub.Billing {
		o.Billing[k] = v
	}
	for k, v := range sub.Shipping {
		o.Shipping[k] = v
	}
	if early {
		o.Meta["_subscription_renewal_early"] = true
	}
	for _, it := range sub.Items {
		o.Items = append(o.Items, &gooOrderItem{Type: "line_item", Name: it.Name, ProductID: it.ProductID, VariationID: it.VariationID,
			Quantity: float64(it.Qty), TaxClass: it.TaxClass, Subtotal: it.Total, SubtotalTax: it.Tax, Total: it.Total, TotalTax: it.Tax,
			Meta: map[string]any{"_subscription_renewal": float64(sub.ID)}})
	}
	for _, sl := range gxaNoteList(sub.Meta["_shipping_lines"]) {
		o.Items = append(o.Items, &gooOrderItem{Type: "shipping", Name: gxaStr(sl, "name"), Total: gxaFloat(sl, "total"), TotalTax: gxaFloat(sl, "tax"),
			Meta: map[string]any{"method_id": gxaStr(sl, "method_id")}})
	}
	s.gooRecalculate(o)
	if err := s.gooSaveOrder(ctx, o, true); err != nil {
		return nil, err
	}
	_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Order created to record renewal of subscription #%d.", sub.ID), false, "")
	sub.Meta["_pending_renewal"] = float64(o.ID)
	sub.LastOrderID = o.ID
	s.gsubAddNote(ctx, sub, fmt.Sprintf("Renewal order #%d created.", o.ID), false, "")
	if err := s.gsubSave(ctx, sub); err != nil {
		return o, err
	}
	s.gooFire(ctx, "order_created", o)
	_ = s.gooTriggerEmail(ctx, "new_renewal_order", o, map[string]any{"subscription": sub})
	return o, nil
}

// gsubManualRenewal reports whether a subscription's renewals are paid by the customer.
func (s *Server) gsubManualRenewal(sub *gooSubscription) bool {
	if s.gooExtBool(gsubKey, "turn_off_automatic_payments") || sub.Meta["_requires_manual_renewal"] == true {
		return true
	}
	gw := s.gooGateway(sub.PaymentMethod)
	if gw == nil || !gw.Supports("subscriptions") {
		return true
	}
	rg, ok := gw.gooGatewayDef.(gooRenewalGateway)
	if !ok {
		return true
	}
	return rg.RenewalNeedsToken() && gxaStr(sub.Meta, "_payment_token") == ""
}

// gsubCartHas reports whether the cart contains a subscription product.
func (s *Server) gsubCartHas(ctx context.Context, cart *gooCart) bool {
	if cart == nil {
		return false
	}
	for _, it := range cart.Items {
		if s.gsubIsSubscription(ctx, it.Product()) {
			return true
		}
	}
	return false
}

// gsubGatewayFilter keeps only the gateways that renew automatically for a cart with a subscription while
// "Accept Manual Renewals" is off (WC_Subscriptions_Payment_Gateways::get_available_payment_gateways).
func gsubGatewayFilter(s *Server, c *gin.Context, cart *gooCart, gws []*gooGatewayHandle) []*gooGatewayHandle {
	if !s.gooExtOn(gsubKey) || s.gooExtBool(gsubKey, "accept_manual_renewals") || s.gooExtBool(gsubKey, "turn_off_automatic_payments") ||
		!s.gsubCartHas(c.Request.Context(), cart) {
		return gws
	}
	var out []*gooGatewayHandle
	for _, g := range gws {
		if g.Supports("subscriptions") {
			out = append(out, g)
		}
	}
	return out
}

// gsubChargeRenewal takes the payment of a renewal order through the subscription's gateway with the
// stored payment token and returns the transaction id.
func (s *Server) gsubChargeRenewal(ctx context.Context, sub *gooSubscription, o *gooOrder) (string, error) {
	gw := s.gooGateway(sub.PaymentMethod)
	if gw == nil {
		return "", errors.New("payment method " + sub.PaymentMethod + " is not available")
	}
	rg, ok := gw.gooGatewayDef.(gooRenewalGateway)
	if !ok {
		return "", errors.New("payment method " + sub.PaymentMethod + " does not support automatic renewals")
	}
	return rg.ProcessRenewal(s, ctx, o, gxaStr(sub.Meta, "_payment_token"))
}

// gsubScheduler is the hourly subscription job: expirations, due renewals and the grace period.
func (s *Server) gsubScheduler(ctx context.Context) {
	now := time.Now()
	// End of a pending cancellation → cancelled; end date of others → expired.
	s.gsubEach(ctx, gsubQuery{Status: []string{"active", "on-hold", "pending-cancel"}, Ended: true}, func(sub *gooSubscription) {
		if sub.End == nil || sub.End.After(now) {
			return
		}
		if sub.Status == "pending-cancel" {
			_ = s.gsubUpdateStatus(ctx, sub, "cancelled", "", "")
		} else {
			_ = s.gsubUpdateStatus(ctx, sub, "expired", "", "")
		}
	})
	// Due renewals.
	s.gsubEach(ctx, gsubQuery{Due: true}, func(sub *gooSubscription) {
		if oid := gxaInt64(sub.Meta, "_pending_renewal"); oid != 0 {
			// An unpaid early renewal does not hold back the regular renewal: it is cancelled and the
			// due renewal order is created instead.
			o, err := s.gooGetOrder(ctx, oid)
			if err != nil || o.Meta["_subscription_renewal_early"] != true || !containsStr([]string{"pending", "failed"}, o.Status) {
				return
			}
			if sub = s.gsubCancelEarlyRenewal(ctx, sub, o, "Early renewal order superseded by the scheduled renewal."); sub == nil ||
				gxaInt64(sub.Meta, "_pending_renewal") != 0 || sub.NextPayment == nil {
				return
			}
		}
		_, _ = s.gsubRunRenewal(ctx, sub)
	})
	// Unpaid renewals past the grace period put their subscription on hold.
	grace := s.gooExtInt(gsubKey, "renewal_grace_days")
	s.gsubEach(ctx, gsubQuery{Status: []string{"active"}, PendingRenew: true}, func(sub *gooSubscription) {
		oid := gxaInt64(sub.Meta, "_pending_renewal")
		if oid == 0 {
			return
		}
		o, err := s.gooGetOrder(ctx, oid)
		if err != nil || o.Status == "cancelled" || o.Status == "trash" {
			// The renewal order was deleted or cancelled without the status hook (older data):
			// without this the subscription would stay active and never renew again.
			early := err == nil && o.Meta["_subscription_renewal_early"] == true
			s.gsubDropPendingRenewal(ctx, sub, oid, early, fmt.Sprintf("Renewal order #%d is gone or was cancelled.", oid))
			return
		}
		if !containsStr([]string{"pending", "failed"}, o.Status) {
			return
		}
		if o.Meta["_subscription_renewal_early"] == true {
			// An abandoned "Renew now" order: the current period is still paid for, so after the grace
			// period (at least a day, the customer may still be paying) the order is cancelled and the
			// subscription stays active and renews on its regular date.
			if now.Sub(o.DateCreated) >= time.Duration(max(grace, 1))*24*time.Hour {
				s.gsubCancelEarlyRenewal(ctx, sub, o, "Early renewal order was not paid.")
			}
			return
		}
		if o.Status == "failed" || now.Sub(o.DateCreated) >= time.Duration(grace)*24*time.Hour {
			_ = s.gsubUpdateStatus(ctx, sub, "on-hold", fmt.Sprintf("Renewal order #%d has not been paid.", o.ID), "")
		}
	})
}

// gsubRunRenewal is one scheduled renewal: it moves the next payment date on, creates the renewal
// order and either charges the stored payment method or invoices the customer. It returns the
// renewal order.
func (s *Server) gsubRunRenewal(ctx context.Context, sub *gooSubscription) (*gooOrder, error) {
	base := time.Now()
	if sub.NextPayment != nil {
		base = *sub.NextPayment
	}
	np := gxaAddPeriod(base, sub.Period, sub.Interval)
	if sub.End != nil && !np.Before(*sub.End) {
		sub.NextPayment = nil
	} else {
		sub.NextPayment = &np
	}
	o, err := s.gsubCreateRenewal(ctx, sub, false)
	if err != nil {
		return nil, err
	}
	if !s.gsubManualRenewal(sub) {
		txn, err := s.gsubChargeRenewal(ctx, sub, o)
		if errors.Is(err, errGooPaymentPending) {
			o.TransactionID = txn
			_ = s.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "on-hold", "Renewal payment is processing: "+gooEsc(err.Error()), "")
			return o, nil
		}
		if err == nil {
			err = s.gooPaymentComplete(ctx, o, txn)
		}
		if err != nil {
			_ = s.gooSetStatus(ctx, o, "failed", "Renewal payment failed: "+err.Error(), "")
			return o, err
		}
		return o, nil
	}
	_ = s.gooAddOrderNote(ctx, o, "Awaiting payment of the renewal order by the customer.", false, "")
	_ = s.gooTriggerEmail(ctx, "customer_renewal_invoice", o, map[string]any{"subscription": sub})
	return o, nil
}

// gsubCancelEarlyRenewal cancels an unpaid early renewal order and releases the subscription's
// pending renewal; it returns the subscription as stored afterwards (nil if it is gone).
func (s *Server) gsubCancelEarlyRenewal(ctx context.Context, sub *gooSubscription, o *gooOrder, note string) *gooSubscription {
	if o.Status != "cancelled" {
		_ = s.gooSetStatus(ctx, o, "cancelled", note, "")
	}
	fresh, err := s.gsubGet(ctx, sub.ID)
	if err != nil {
		return nil
	}
	// The order status hook drops the pending renewal; this covers a hook that did not run.
	s.gsubDropPendingRenewal(ctx, fresh, o.ID, true, fmt.Sprintf("Renewal order #%d was cancelled.", o.ID))
	return fresh
}

// Customer actions ----------------------------------------------------------------------------------------

// gsubCustomerActions are the actions a customer may take on a subscription (wcs_get_all_user_actions_for_subscription()).
func (s *Server) gsubCustomerActions(ctx context.Context, sub *gooSubscription) []gooOpt {
	var out []gooOpt
	maxSusp := s.gooExtInt(gsubKey, "max_customer_suspensions")
	switch sub.Status {
	case "active":
		if maxSusp > 0 && gxaInt(sub.Meta, "_suspension_count") < maxSusp {
			out = append(out, gooOpt{"on-hold", "Suspend"})
		}
	case "on-hold":
		// An unpaid renewal (pending, or cancelled and remembered as _unpaid_renewal) keeps the
		// subscription on hold until it is paid: the customer pays a new renewal instead.
		switch {
		case gxaInt64(sub.Meta, "_pending_renewal") != 0:
		case sub.Meta["_unpaid_renewal"] != nil:
			out = append(out, gooOpt{"renew", "Renew now"})
		default:
			out = append(out, gooOpt{"active", "Reactivate"})
		}
	}
	if containsStr([]string{"active", "on-hold", "pending"}, sub.Status) {
		out = append(out, gooOpt{"cancelled", "Cancel"})
	}
	if sub.Status == "active" && sub.NextPayment != nil && s.gooExtBool(gsubKey, "enable_early_renewal") && gxaInt64(sub.Meta, "_pending_renewal") == 0 {
		out = append(out, gooOpt{"renew", "Renew now"})
	}
	return out
}

// gsubActionURL is the link of a customer action (?subscription_id=…&change_subscription_to=…&_wpnonce=…).
func (s *Server) gsubActionURL(c *gin.Context, sub *gooSubscription, action string) string {
	base := s.gooEndpointURL(c.Request.Context(), "myaccount", "view-subscription", itoa64(sub.ID))
	nonce := s.nonce(c, "gsub-"+itoa64(sub.ID))
	if action == "renew" {
		return base + "?subscription_renewal_early=" + itoa64(sub.ID) + "&_wpnonce=" + nonce
	}
	return base + "?subscription_id=" + itoa64(sub.ID) + "&change_subscription_to=" + action + "&_wpnonce=" + nonce
}

// gsubFrontRoute handles the customer actions.
func gsubFrontRoute(s *Server, c *gin.Context, path string) bool {
	if !s.gooExtOn(gsubKey) {
		return false
	}
	if gsubFrontPost(s, c) {
		return true
	}
	idStr, to := c.Query("subscription_id"), c.Query("change_subscription_to")
	early := c.Query("subscription_renewal_early")
	if (idStr == "" || to == "") && early == "" {
		return false
	}
	ctx := c.Request.Context()
	if early != "" {
		idStr = early
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)
	u := currentUser(c)
	sub, err := s.gsubGet(ctx, id)
	back := s.gooAccountURL(ctx, "subscriptions")
	fail := func(msg string) bool {
		s.gooAddNotice(c, "error", msg)
		s.gooSaveSession(c)
		c.Redirect(302, back)
		return true
	}
	if err != nil || u == nil || sub.CustomerID != u.ID {
		return fail("That subscription does not exist. Please contact us if you need assistance.")
	}
	if !s.checkNonce(c, "gsub-"+itoa64(sub.ID)) {
		return fail("Security error. Please contact us if you need assistance.")
	}
	view := s.gooEndpointURL(ctx, "myaccount", "view-subscription", itoa64(sub.ID))
	allowed := false
	action := to
	if early != "" {
		action = "renew"
	}
	for _, a := range s.gsubCustomerActions(ctx, sub) {
		if a.Value == action {
			allowed = true
		}
	}
	if !allowed {
		back = view
		return fail("That subscription can not be changed to " + gooEsc(gsubStatusLabel(to)) + ". Please contact us if you need assistance.")
	}
	switch action {
	case "renew":
		// On hold for an unpaid renewal: a regular renewal for the unpaid period; otherwise early.
		o, err := s.gsubCreateRenewal(ctx, sub, sub.Status == "active")
		if err != nil {
			return fail(gooEsc(err.Error()))
		}
		c.Redirect(302, s.gooOrderPayURL(ctx, o))
		return true
	case "on-hold":
		sub.Meta["_suspension_count"] = float64(gxaInt(sub.Meta, "_suspension_count") + 1)
		_ = s.gsubUpdateStatus(ctx, sub, "on-hold", "Subscription put on hold by the subscriber.", "customer")
		s.gooAddNotice(c, "success", "Your subscription has been put on hold.")
	case "active":
		_ = s.gsubUpdateStatus(ctx, sub, "active", "Subscription reactivated by the subscriber.", "customer")
		s.gooAddNotice(c, "success", "Your subscription has been reactivated.")
	case "cancelled":
		target := "cancelled"
		if s.gooExtBool(gsubKey, "cancel_at_period_end") && sub.Status == "active" && sub.NextPayment != nil {
			target = "pending-cancel"
		}
		_ = s.gsubUpdateStatus(ctx, sub, target, "Subscription cancelled by the subscriber from their account page.", "customer")
		s.gooAddNotice(c, "success", "Your subscription has been cancelled.")
	}
	s.gooSaveSession(c)
	c.Redirect(302, view)
	return true
}

// My Account ------------------------------------------------------------------------------------------------

func (s *Server) gsubTotalString(sub *gooSubscription) string {
	return string(s.gooPriceIn(sub.RecurringTotal, sub.Currency)) + `<span class="subscription-details">` +
		s.gsubPriceString(gsubSchedule{Interval: sub.Interval, Period: sub.Period}) + `</span>`
}

// gsubAccountList is My Account › Subscriptions.
func gsubAccountList(s *Server, c *gin.Context, arg string) template.HTML {
	u := currentUser(c)
	if u == nil || !s.gooExtOn(gsubKey) {
		return ""
	}
	ctx := c.Request.Context()
	subs, _, _ := s.gsubList(ctx, gsubQuery{CustomerID: u.ID})
	if len(subs) == 0 {
		return template.HTML(`<div class="woocommerce-message woocommerce-message--info woocommerce-Message woocommerce-Message--info woocommerce-info">You have no active subscriptions. ` +
			`<a class="woocommerce-Button button" href="` + gooEsc(s.gooShopURL(ctx)) + `">Browse products</a></div>`)
	}
	var b strings.Builder
	b.WriteString(`<div class="woocommerce_account_subscriptions"><table class="my_account_subscriptions my_account_orders woocommerce-orders-table woocommerce-MyAccount-subscriptions shop_table shop_table_responsive woocommerce-orders-table--subscriptions">`)
	b.WriteString(`<thead><tr><th class="subscription-id order-number woocommerce-orders-table__header"><span class="nobr">Subscription</span></th>` +
		`<th class="subscription-status order-status woocommerce-orders-table__header"><span class="nobr">Status</span></th>` +
		`<th class="subscription-next-payment order-date woocommerce-orders-table__header"><span class="nobr">Next payment</span></th>` +
		`<th class="subscription-total order-total woocommerce-orders-table__header"><span class="nobr">Total</span></th>` +
		`<th class="subscription-actions order-actions woocommerce-orders-table__header">&nbsp;</th></tr></thead><tbody>`)
	for _, sub := range subs {
		view := s.gooEndpointURL(ctx, "myaccount", "view-subscription", itoa64(sub.ID))
		b.WriteString(`<tr class="order woocommerce-orders-table__row woocommerce-orders-table__row--status-` + gooEsc(sub.Status) + `">`)
		b.WriteString(`<td class="subscription-id order-number woocommerce-orders-table__cell" data-title="ID"><a href="` + gooEsc(view) + `">#` + itoa64(sub.ID) + `</a></td>`)
		b.WriteString(`<td class="subscription-status order-status woocommerce-orders-table__cell" data-title="Status">` + gooEsc(gsubStatusLabel(sub.Status)) + `</td>`)
		b.WriteString(`<td class="subscription-next-payment order-date woocommerce-orders-table__cell" data-title="Next Payment">` + s.gxaDate(sub.NextPayment) + `</td>`)
		b.WriteString(`<td class="subscription-total order-total woocommerce-orders-table__cell" data-title="Total">` + s.gsubTotalString(sub) + `</td>`)
		b.WriteString(`<td class="subscription-actions order-actions woocommerce-orders-table__cell"><a href="` + gooEsc(view) + `" class="woocommerce-button button view">View</a></td></tr>`)
	}
	b.WriteString(`</tbody></table></div>`)
	return template.HTML(b.String())
}

// gsubAccountView is My Account › View subscription.
func gsubAccountView(s *Server, c *gin.Context, arg string) template.HTML {
	u := currentUser(c)
	if u == nil || !s.gooExtOn(gsubKey) {
		return ""
	}
	ctx := c.Request.Context()
	id, _ := strconv.ParseInt(strings.Trim(arg, "/"), 10, 64)
	sub, err := s.gsubGet(ctx, id)
	if err != nil || sub.CustomerID != u.ID {
		return template.HTML(`<div class="woocommerce-error">Invalid subscription. <a href="` + gooEsc(s.gooAccountURL(ctx, "subscriptions")) + `" class="wc-forward">My account</a></div>`)
	}
	var b strings.Builder
	b.WriteString(`<table class="shop_table subscription_details"><tbody>`)
	row := func(label, val string) {
		b.WriteString(`<tr><td>` + label + `</td><td>` + val + `</td></tr>`)
	}
	row("Status", gooEsc(gsubStatusLabel(sub.Status)))
	start := sub.Start
	row("Start date", s.gxaDate(&start))
	if last, err := s.gooGetOrder(ctx, sub.LastOrderID); err == nil {
		row("Last order date", s.gxaDate(&last.DateCreated))
	}
	if sub.TrialEnd != nil {
		row("Trial end date", s.gxaDate(sub.TrialEnd))
	}
	if sub.NextPayment != nil {
		row("Next payment date", s.gxaDate(sub.NextPayment))
	}
	if sub.End != nil {
		row("End date", s.gxaDate(sub.End))
	}
	if sub.PaymentMethodTitle != "" {
		manual := ""
		if s.gsubManualRenewal(sub) {
			manual = " (Manual Renewal)"
		}
		row("Payment", `<span data-is_manual="`+map[bool]string{true: "yes", false: "no"}[manual != ""]+`" class="subscription-payment-method">`+gooEsc(sub.PaymentMethodTitle)+manual+`</span>`)
	}
	if acts := s.gsubCustomerActions(ctx, sub); len(acts) > 0 {
		var btns []string
		for _, a := range acts {
			cls := gooSanitizeKey(a.Label)
			btns = append(btns, `<a href="`+gooEsc(s.gsubActionURL(c, sub, a.Value))+`" class="button `+cls+`">`+gooEsc(a.Label)+`</a>`)
		}
		row("Actions", strings.Join(btns, " "))
	}
	b.WriteString(`</tbody></table>`)
	// Totals.
	b.WriteString(`<header><h2>Subscription totals</h2></header><table class="shop_table order_details"><thead><tr><th class="product-name">Product</th><th class="product-total">Total</th></tr></thead><tbody>`)
	sched := gsubSchedule{Interval: sub.Interval, Period: sub.Period}
	for idx, it := range sub.Items {
		b.WriteString(`<tr class="order_item"><td class="product-name">` + gooEsc(it.Name) + ` <strong class="product-quantity">&times;&nbsp;` + strconv.Itoa(it.Qty) + `</strong>` + s.gooSubsSwitchLink(c, sub, idx) + `</td>` +
			`<td class="product-total">` + string(s.gooPriceIn(it.Total, sub.Currency)) + `<span class="subscription-details">` + s.gsubPriceString(sched) + `</span></td></tr>`)
	}
	b.WriteString(`</tbody><tfoot>`)
	if sub.RecurringTax > 0 {
		b.WriteString(`<tr><th scope="row">Tax:</th><td>` + string(s.gooPriceIn(sub.RecurringTax, sub.Currency)) + `</td></tr>`)
	}
	b.WriteString(`<tr><th scope="row">Total:</th><td>` + s.gsubTotalString(sub) + `</td></tr></tfoot></table>`)
	// Related orders.
	b.WriteString(`<header><h2>Related orders</h2></header>` + s.gsubRelatedOrdersTable(ctx, sub, func(o *gooOrder) string {
		return s.gooEndpointURL(ctx, "myaccount", "view-order", itoa64(o.ID))
	}))
	// Addresses.
	b.WriteString(`<section class="woocommerce-customer-details"><h2 class="woocommerce-column__title">Billing address</h2><address>`)
	for _, k := range []string{"first_name", "last_name", "company", "address_1", "address_2", "city", "state", "postcode", "country", "email", "phone"} {
		if v := sub.Billing[k]; v != "" {
			b.WriteString(gooEsc(v) + `<br>`)
		}
	}
	b.WriteString(`</address></section>`)
	b.WriteString(s.gsubAccountExtras(c, sub))
	return template.HTML(b.String())
}

// gsubRelatedOrders are the parent and renewal orders of a subscription.
func (s *Server) gsubRelatedOrders(ctx context.Context, sub *gooSubscription) []*gooOrder {
	rows, err := s.store.Pool.Query(ctx, `SELECT id FROM goo_orders WHERE type = 'shop_order' AND (id = $1 OR (meta->>'_subscription_renewal')::numeric = $2
		OR (meta->>'_subscription_switch')::numeric = $2) ORDER BY id DESC`, sub.ParentOrderID, sub.ID)
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
	var out []*gooOrder
	for _, id := range ids {
		if o, err := s.gooGetOrder(ctx, id); err == nil {
			out = append(out, o)
		}
	}
	return out
}

func (s *Server) gsubRelatedOrdersTable(ctx context.Context, sub *gooSubscription, link func(o *gooOrder) string) string {
	var b strings.Builder
	b.WriteString(`<table class="shop_table shop_table_responsive my_account_orders woocommerce-orders-table woocommerce-MyAccount-orders"><thead><tr>` +
		`<th class="order-number"><span class="nobr">Order</span></th><th class="order-date"><span class="nobr">Date</span></th>` +
		`<th class="order-status"><span class="nobr">Status</span></th><th class="order-total"><span class="nobr">Total</span></th><th class="order-relationship">Relationship</th></tr></thead><tbody>`)
	for _, o := range s.gsubRelatedOrders(ctx, sub) {
		rel := "Renewal Order"
		if o.ID == sub.ParentOrderID {
			rel = "Parent Order"
		} else if o.Meta["_subscription_switch"] != nil {
			rel = "Switch Order"
		}
		b.WriteString(`<tr class="order"><td class="order-number"><a href="` + gooEsc(link(o)) + `">#` + o.Number() + `</a></td>` +
			`<td class="order-date">` + s.gxaDate(&o.DateCreated) + `</td><td class="order-status">` + gooEsc(gooStatusLabel(o.Status)) + `</td>` +
			`<td class="order-total">` + string(s.gooPriceIn(o.Total, o.Currency)) + `</td><td class="order-relationship">` + rel + `</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	return b.String()
}

// Admin --------------------------------------------------------------------------------------------------

// gsubAdmin is Goocommerce › Subscriptions: the list and the edit screen.
func (s *Server) gsubAdmin(c *gin.Context) {
	if c.Query("action") == "new" {
		s.gsubAdminNew(c)
		return
	}
	if c.Query("action") == "edit" || c.PostForm("action") == "editsubscription" {
		s.gsubAdminEdit(c)
		return
	}
	if c.Query("action") == "export_csv" {
		s.gooExportSubscriptionsCSV(c)
		return
	}
	ctx := c.Request.Context()
	status := c.Query("status")
	search := strings.TrimSpace(c.Query("s"))
	q := gsubQuery{Search: search, Page: gxaPage(c), PerPage: 20}
	if status != "" {
		q.Status = []string{status}
	}
	subs, total, _ := s.gsubList(ctx, q)
	counts := map[string]int{}
	if rows, err := s.store.Pool.Query(ctx, `SELECT status, count(*) FROM goo_subscriptions GROUP BY status`); err == nil {
		for rows.Next() {
			var st string
			var n int
			if rows.Scan(&st, &n) == nil {
				counts[st] = n
			}
		}
		rows.Close()
	}
	base := "/wp-admin/goocommerce?page=subscriptions"
	var b strings.Builder
	b.WriteString(gxaViews(base, status, gsubStatuses, counts))
	b.WriteString(`<form method="get" action="/wp-admin/goocommerce"><input type="hidden" name="page" value="subscriptions">`)
	if status != "" {
		b.WriteString(`<input type="hidden" name="status" value="` + gooEsc(status) + `">`)
	}
	b.WriteString(`<p class="search-box"><label class="screen-reader-text" for="post-search-input">Search Subscriptions:</label><input type="search" id="post-search-input" name="s" value="` + gooEsc(search) + `"><input type="submit" id="search-submit" class="button" value="Search Subscriptions"></p></form>`)
	b.WriteString(`<div class="tablenav top">` + gxaPagination(base+"&status="+status+"&s="+search, q.Page, q.PerPage, total) + `<br class="clear"></div>`)
	b.WriteString(`<table class="wp-list-table widefat fixed striped posts"><thead><tr><th scope="col" class="manage-column column-status">Status</th>` +
		`<th scope="col" class="manage-column column-order_title column-primary">Subscription</th><th scope="col" class="manage-column column-order_items">Items</th>` +
		`<th scope="col" class="manage-column column-recurring_total">Total</th><th scope="col" class="manage-column column-start_date">Start Date</th>` +
		`<th scope="col" class="manage-column column-trial_end_date">Trial End</th><th scope="col" class="manage-column column-next_payment_date">Next Payment</th>` +
		`<th scope="col" class="manage-column column-last_payment_date">Last Order Date</th><th scope="col" class="manage-column column-end_date">End Date</th>` +
		`<th scope="col" class="manage-column column-orders">Orders</th></tr></thead><tbody id="the-list">`)
	if len(subs) == 0 {
		b.WriteString(`<tr class="no-items"><td class="colspanchange" colspan="10">No subscriptions found.</td></tr>`)
	}
	for _, sub := range subs {
		edit := base + "&action=edit&id=" + itoa64(sub.ID)
		var items []string
		for _, it := range sub.Items {
			items = append(items, gooEsc(it.Name)+` &times; `+strconv.Itoa(it.Qty))
		}
		var lastDate *time.Time
		if o, err := s.gooGetOrder(ctx, sub.LastOrderID); err == nil {
			lastDate = &o.DateCreated
		}
		start := sub.Start
		b.WriteString(`<tr id="post-` + itoa64(sub.ID) + `" class="type-shop_subscription status-wc-` + gooEsc(sub.Status) + `">`)
		b.WriteString(`<td class="status column-status"><mark class="subscription-status order-status status-` + gooEsc(sub.Status) + `"><span>` + gooEsc(gsubStatusLabel(sub.Status)) + `</span></mark></td>`)
		b.WriteString(`<td class="order_title column-order_title column-primary"><a href="` + gooEsc(edit) + `"><strong>#` + itoa64(sub.ID) + `</strong></a> for ` + s.gxaUserLabel(ctx, sub.CustomerID, false) + `</td>`)
		b.WriteString(`<td class="order_items column-order_items">` + strings.Join(items, "<br>") + `</td>`)
		b.WriteString(`<td class="recurring_total column-recurring_total">` + s.gsubTotalString(sub) + `<small class="meta">Via ` + gooEsc(defaultStr(sub.PaymentMethodTitle, "Manual Renewal")) + `</small></td>`)
		b.WriteString(`<td class="start_date column-start_date">` + s.gxaDate(&start) + `</td><td class="trial_end_date column-trial_end_date">` + s.gxaDate(sub.TrialEnd) + `</td>`)
		b.WriteString(`<td class="next_payment_date column-next_payment_date">` + s.gxaDate(sub.NextPayment) + `</td><td class="last_payment_date column-last_payment_date">` + s.gxaDate(lastDate) + `</td>`)
		b.WriteString(`<td class="end_date column-end_date">` + s.gxaDate(sub.End) + `</td><td class="orders column-orders">` + strconv.Itoa(len(s.gsubRelatedOrders(ctx, sub))) + `</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	head := `<h1 class="wp-heading-inline">Subscriptions</h1> <a href="` + base + `&action=new" class="page-title-action">Add Subscription</a> <a href="` +
		gooEsc(gooAdminURL("subscriptions", "action", "export_csv", "status", status, "s", search, "_wpnonce", s.nonce(c, gooStoreNonce))) + `" class="page-title-action">Export CSV</a>`
	if search != "" {
		head += `<span class="subtitle">Search results for: <strong>` + gooEsc(search) + `</strong></span>`
	}
	s.gxaAdminRender(c, "subscriptions", "Subscriptions", "goo-ext-subscriptions", head, b.String())
}

// gsubAdminEdit is the Edit Subscription screen.
func (s *Server) gsubAdminEdit(c *gin.Context) {
	ctx := c.Request.Context()
	id, _ := strconv.ParseInt(defaultStr(c.PostForm("id"), c.Query("id")), 10, 64)
	sub, err := s.gsubGet(ctx, id)
	if err != nil {
		gxaAdminRedirect(c, "subscriptions", "", "Subscription not found.", true)
		return
	}
	params := "action=edit&id=" + itoa64(sub.ID)
	if c.Request.Method == "POST" {
		if !s.checkNonce(c, "gsub-edit-"+itoa64(sub.ID)) {
			s.denyNonce(c)
			return
		}
		by := ""
		if u := currentUser(c); u != nil {
			by = u.DisplayName
		}
		if note := strings.TrimSpace(c.PostForm("subscription_note")); note != "" && c.PostForm("add_note") != "" {
			s.gsubAddNote(ctx, sub, gooEsc(note), c.PostForm("note_type") == "customer", by)
			_ = s.gsubSave(ctx, sub)
			gxaAdminRedirect(c, "subscriptions", params, "Note added.", false)
			return
		}
		if c.PostForm("process_renewal") != "" {
			msg, isErr := s.gsubAdminProcessRenewal(ctx, sub, by)
			gxaAdminRedirect(c, "subscriptions", params, msg, isErr)
			return
		}
		editErr := s.gsubAdminApplyEdits(c, sub, by)
		if np := c.PostForm("next_payment"); np != "" {
			t := s.gxaParseTime(np)
			if t == nil || t.Before(time.Now()) {
				gxaAdminRedirect(c, "subscriptions", params, "The next payment date must be in the future.", true)
				return
			}
			sub.NextPayment = t
		}
		if ed := c.PostForm("end"); ed != "" {
			sub.End = s.gxaParseTime(ed)
		} else if c.PostForm("end_never") != "" {
			sub.End = nil
		}
		if sub.End != nil && sub.NextPayment != nil && !sub.NextPayment.Before(*sub.End) {
			gxaAdminRedirect(c, "subscriptions", params, "The end date must be after the next payment date.", true)
			return
		}
		if to := c.PostForm("order_status"); to != "" && to != sub.Status {
			valid := false
			for _, st := range gsubStatuses {
				valid = valid || st.Value == to
			}
			if valid {
				if err := s.gsubUpdateStatus(ctx, sub, to, "Status changed by "+defaultStr(by, "the store manager")+".", by); err != nil {
					gxaAdminRedirect(c, "subscriptions", params, err.Error(), true)
					return
				}
			}
		}
		if err := s.gsubSave(ctx, sub); err != nil {
			gxaAdminRedirect(c, "subscriptions", params, err.Error(), true)
			return
		}
		if editErr != nil {
			gxaAdminRedirect(c, "subscriptions", params, editErr.Error(), true)
			return
		}
		gxaAdminRedirect(c, "subscriptions", params, "Subscription updated.", false)
		return
	}
	var b strings.Builder
	nonce := s.nonce(c, "gsub-edit-"+itoa64(sub.ID))
	b.WriteString(`<form method="post" action="/wp-admin/goocommerce?page=subscriptions&amp;action=edit&amp;id=` + itoa64(sub.ID) + `" id="post"><input type="hidden" name="_wpnonce" value="` + nonce + `"><input type="hidden" name="action" value="editsubscription"><input type="hidden" name="id" value="` + itoa64(sub.ID) + `">`)
	b.WriteString(`<div id="poststuff"><div id="post-body" class="metabox-holder columns-2"><div id="post-body-content">`)
	b.WriteString(`<div id="woocommerce-subscription-data" class="postbox"><div class="inside"><div class="panel woocommerce-order-data"><h2 class="woocommerce-order-data__heading">Subscription #` + itoa64(sub.ID) + ` details</h2>`)
	b.WriteString(`<p class="woocommerce-order-data__meta order_number">Parent order ` + gxaOrderLink(sub.ParentOrderID) + `</p>`)
	b.WriteString(`<div class="order_data_column_container"><div class="order_data_column"><h3>General</h3>`)
	b.WriteString(`<p class="form-field form-field-wide"><label for="customer_id">Customer:</label> ` + s.gxaUserLabel(ctx, sub.CustomerID, true) +
		`<br><input type="number" min="1" class="small-text" name="customer_id" id="customer_id" value="` + itoa64(sub.CustomerID) + `"> <span class="description">User ID. Change it to move the subscription to another customer.</span></p>`)
	b.WriteString(`<p class="form-field form-field-wide wc-order-status"><label for="order_status">Status:</label>` + gxaSelect("order_status", "order_status", gsubStatuses, sub.Status, ` class="wc-enhanced-select"`) + `</p>`)
	b.WriteString(`<p class="form-field"><label>Billing schedule:</label> ` + gooEsc(strings.TrimSpace(gsubPlain(s.gsubPriceString(gsubSchedule{Interval: sub.Interval, Period: sub.Period})))) + `</p>`)
	b.WriteString(`<p class="form-field"><label>Payment method:</label> ` + gooEsc(defaultStr(sub.PaymentMethodTitle, "Manual Renewal")) + `</p></div>`)
	b.WriteString(`<div class="order_data_column"><h3>Billing</h3>` + gsubAdminAddress("billing", sub.Billing) + `</div>`)
	b.WriteString(`<div class="order_data_column"><h3>Shipping</h3>` + gsubAdminAddress("shipping", sub.Shipping) + `</div></div></div></div></div>`)
	// Items.
	b.WriteString(`<div class="postbox"><h2 class="hndle"><span>Items</span></h2><div class="inside"><table class="woocommerce_order_items widefat"><thead><tr><th class="item">Item</th><th class="item_cost">Cost</th><th class="quantity">Qty</th><th class="line_cost">Total</th><th class="line_tax">Tax</th><th class="remove">Remove</th></tr></thead><tbody>`)
	for i, it := range sub.Items {
		idx := strconv.Itoa(i)
		b.WriteString(`<tr class="item"><td class="name">` + gooEsc(it.Name) + `</td><td class="item_cost"><input type="text" size="8" name="item_price[` + idx + `]" value="` + gooMoney(it.Price, s.gooDecimals()) + `"></td>` +
			`<td class="quantity">&times; <input type="number" min="1" class="small-text" name="item_qty[` + idx + `]" value="` + strconv.Itoa(it.Qty) + `"></td>` +
			`<td class="line_cost">` + string(s.gooPriceIn(it.Total, sub.Currency)) + `</td><td class="line_tax"><input type="text" size="6" name="item_tax[` + idx + `]" value="` + gooMoney(it.Tax, s.gooDecimals()) + `"></td>` +
			`<td class="remove"><input type="checkbox" name="item_remove[` + idx + `]" value="1" aria-label="Remove ` + gooEsc(it.Name) + `"></td></tr>`)
	}
	b.WriteString(`</tbody><tfoot><tr class="add-item"><td class="name"><label for="add_product">Add product (ID)</label> <input type="number" min="1" class="small-text" name="add_product" id="add_product"></td>` +
		`<td class="item_cost"></td><td class="quantity">&times; <input type="number" min="1" class="small-text" name="add_qty" value="1" aria-label="Quantity"></td>` +
		`<td class="line_cost"><input type="text" size="8" name="add_total" placeholder="product price" aria-label="Line total"></td><td class="line_tax"></td><td></td></tr></tfoot></table><p class="wc-order-totals">Recurring total: <strong>` + s.gsubTotalString(sub) + `</strong></p></div></div>`)
	// Related orders.
	b.WriteString(`<div class="postbox" id="subscription_renewal_orders"><h2 class="hndle"><span>Related Orders</span></h2><div class="inside">` +
		s.gsubRelatedOrdersTable(ctx, sub, func(o *gooOrder) string { return "/wp-admin/goocommerce?page=orders&action=edit&id=" + itoa64(o.ID) }) + `</div></div>`)
	b.WriteString(`</div><div id="postbox-container-1" class="postbox-container">`)
	// Schedule box.
	start := sub.Start
	b.WriteString(`<div id="woocommerce-subscription-schedule" class="postbox"><h2 class="hndle"><span>Schedule</span></h2><div class="inside"><div class="wcs-date-fields">`)
	b.WriteString(`<p><strong>Start date:</strong> ` + s.gxaDateTime(&start) + `</p>`)
	b.WriteString(`<p><strong>Trial end:</strong> ` + s.gxaDateTime(sub.TrialEnd) + `</p>`)
	b.WriteString(`<p><label for="next_payment"><strong>Next payment:</strong></label><br><input type="datetime-local" id="next_payment" name="next_payment" value="` + s.gxaInputDate(sub.NextPayment, true) + `"></p>`)
	b.WriteString(`<p><label for="end"><strong>End date:</strong></label><br><input type="datetime-local" id="end" name="end" value="` + s.gxaInputDate(sub.End, true) + `"> <label><input type="checkbox" name="end_never" value="1"> Never</label></p>`)
	b.WriteString(`</div></div></div>`)
	b.WriteString(`<div id="submitdiv" class="postbox"><h2 class="hndle"><span>Subscription actions</span></h2><div class="inside"><div class="submitbox"><div id="major-publishing-actions"><div id="publishing-action"><button type="submit" class="button save_order button-primary" name="save" value="Update">Update</button></div><div id="delete-action"><button type="submit" class="button" name="process_renewal" value="1">Process renewal</button></div><div class="clear"></div></div></div></div></div>`)
	// Notes.
	b.WriteString(`<div id="woocommerce-subscription-notes" class="postbox"><h2 class="hndle"><span>Subscription notes</span></h2><div class="inside"><ul class="order_notes">`)
	for _, n := range gxaNoteList(sub.Meta["_notes"]) {
		cls := "note system-note"
		if n["customer"] == true {
			cls = "note customer-note"
		}
		var when *time.Time
		if t, err := time.Parse(time.RFC3339, gxaStr(n, "date")); err == nil {
			when = &t
		}
		b.WriteString(`<li class="` + cls + `"><div class="note_content"><p>` + gxaStr(n, "content") + `</p></div><p class="meta"><abbr class="exact-date">` + s.gxaDateTime(when) + `</abbr>`)
		if by := gxaStr(n, "by"); by != "" {
			b.WriteString(` by ` + gooEsc(by))
		}
		b.WriteString(`</p></li>`)
	}
	b.WriteString(`</ul><div class="add_note"><p><label for="subscription_note">Add note</label><textarea name="subscription_note" id="subscription_note" class="input-text" cols="20" rows="5"></textarea></p>` +
		`<p>` + gxaSelect("note_type", "note_type", []gooOpt{{"", "Private note"}, {"customer", "Note to customer"}}, "", "") +
		` <button type="submit" name="add_note" value="1" class="add_note button">Add</button></p></div></div></div>`)
	b.WriteString(`</div></div></div></form>`)
	head := `<h1 class="wp-heading-inline">Edit Subscription</h1> <a href="/wp-admin/goocommerce?page=subscriptions" class="page-title-action">&larr; All subscriptions</a>`
	s.gxaAdminRender(c, "subscriptions", "Edit Subscription", "goo-ext-subscriptions", head, b.String())
}
