package server

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Goocommerce › Points and Rewards (module "points_rewards"): WooCommerce Points and Rewards. Customers
// earn points for purchases (per the earn ratio, or a product / category override), account sign-up
// and reviews, and redeem them for a discount on later orders. Every change is an entry of the
// goo_points_log ledger; the balance is their sum, cached in the user meta goo_points_balance.

const gptsKey = "points_rewards"

// gptsEventLabels describe the ledger entries (WC_Points_Rewards_Manager::event_type_description()).
var gptsEventLabels = map[string]string{
	"order-placed":     "Points earned for purchase",
	"order-cancelled":  "Points adjusted for cancelled order",
	"order-redeem":     "Points redeemed towards purchase",
	"order-refunded":   "Points refunded for cancelled order",
	"admin-adjustment": "Points adjusted by the store",
	"account-signup":   "Points earned for account signup",
	"product-review":   "Points earned for writing a review",
	"expire":           "Points expired",
}

// gptsEntry is a points log entry.
type gptsEntry struct {
	ID      int64
	UserID  int64
	Points  int
	Type    string
	OrderID int64
	Note    string
	Date    time.Time
}

// gptsLabel is the points label: "Point" / "Points".
func (s *Server) gptsLabel(n int) string {
	if n == 1 || n == -1 {
		return defaultStr(s.gooExtString(gptsKey, "points_label_singular"), "Point")
	}
	return defaultStr(s.gooExtString(gptsKey, "points_label_plural"), "Points")
}

// gptsRound applies the rounding setting to a points amount.
func (s *Server) gptsRound(v float64) int {
	switch s.gooExtString(gptsKey, "points_rounding") {
	case "floor":
		return int(math.Floor(v + 1e-9))
	case "ceil":
		return int(math.Ceil(v - 1e-9))
	}
	return int(math.Round(v))
}

// gptsCalc converts an amount to points with the earn ratio (calculate_points()).
func (s *Server) gptsCalc(amount float64) float64 {
	pts, per := s.gooExtFloat(gptsKey, "earn_points"), s.gooExtFloat(gptsKey, "earn_points_amount")
	if per <= 0 {
		return 0
	}
	return amount * pts / per
}

// gptsPointsValue is the discount a number of points is worth (calculate_points_value()).
func (s *Server) gptsPointsValue(points int) float64 {
	pts, val := s.gooExtFloat(gptsKey, "redeem_points"), s.gooExtFloat(gptsKey, "redeem_points_amount")
	if pts <= 0 {
		return 0
	}
	return gooRound(float64(points)*val/pts, s.gooDecimals())
}

// gptsPointsFor is the number of points a discount amount costs (rounded up).
func (s *Server) gptsPointsFor(amount float64) int {
	pts, val := s.gooExtFloat(gptsKey, "redeem_points"), s.gooExtFloat(gptsKey, "redeem_points_amount")
	if val <= 0 {
		return 0
	}
	return int(math.Ceil(amount*pts/val - 1e-9))
}

// gptsOverride reads a fixed / percentage points value ("10", "5%"): ok false when empty.
func gptsOverride(v string, price float64) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if strings.HasSuffix(v, "%") {
		pct, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, "%")), 64)
		if err != nil {
			return 0, false
		}
		return price * pct / 100, true
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// gptsProductPoints are the points a unit of a product earns at a price: the product's override,
// then its categories', then the earn ratio (WC_Points_Rewards_Product::get_points_earned_for_product_purchase()).
func (s *Server) gptsProductPoints(ctx context.Context, p *gooProduct, price float64) float64 {
	if p == nil {
		return 0
	}
	if v, ok := gptsOverride(gxaStr(p.Meta, "_wc_points_earned"), price); ok {
		return v
	}
	id := p.ID
	if p.Type == gooTypeVariation && p.ParentID != 0 {
		if parent := s.gooParent(ctx, p); parent != nil {
			if v, ok := gptsOverride(gxaStr(parent.Meta, "_wc_points_earned"), price); ok {
				return v
			}
		}
		id = p.ParentID
	}
	if cats := s.gooExtSettings(gptsKey)["category_points"]; cats != nil {
		if m, ok := cats.(map[string]any); ok && len(m) > 0 {
			if terms, err := s.store.TermsForPosts(ctx, []int64{id}, "product_cat"); err == nil {
				best, found := 0.0, false
				for _, t := range terms[id] {
					if v, ok := gptsOverride(gxaStr(m, itoa64(t.ID)), price); ok && (!found || v > best) {
						best, found = v, true
					}
				}
				if found {
					return best
				}
			}
		}
	}
	return s.gptsCalc(price)
}

// Ledger -------------------------------------------------------------------------------------------------

// gptsBalance is a user's points balance (the ledger's sum).
func (s *Server) gptsBalance(ctx context.Context, userID int64) int {
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT coalesce(sum(points),0) FROM goo_points_log WHERE user_id = $1`, userID).Scan(&n)
	return n
}

// gptsLog adds a ledger entry, consumes earned points first-in first-out for a debit and refreshes the
// cached balance.
func (s *Server) gptsLog(ctx context.Context, userID int64, points int, typ string, orderID int64, note string) error {
	if userID == 0 || points == 0 {
		return nil
	}
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO goo_points_log (user_id, points, type, order_id, note, used) VALUES ($1,$2,$3,$4,$5,$6)`,
		userID, points, typ, orderID, note, 0); err != nil {
		return err
	}
	if points < 0 {
		left := -points
		rows, err := s.store.Pool.Query(ctx, `SELECT id, points - used FROM goo_points_log WHERE user_id = $1 AND points > used AND points > 0 ORDER BY date, id`, userID)
		if err == nil {
			type rem struct {
				id int64
				n  int
			}
			var list []rem
			for rows.Next() {
				var r rem
				if rows.Scan(&r.id, &r.n) == nil {
					list = append(list, r)
				}
			}
			rows.Close()
			for _, r := range list {
				if left <= 0 {
					break
				}
				take := min(r.n, left)
				_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_points_log SET used = used + $2 WHERE id = $1`, r.id, take)
				left -= take
			}
		}
	}
	return s.gooSetUserMeta(ctx, userID, map[string]any{"goo_points_balance": float64(s.gptsBalance(ctx, userID))})
}

// gptsEntries lists ledger entries (all users when userID is 0), newest first.
func (s *Server) gptsEntries(ctx context.Context, userID int64, limit, offset int) ([]*gptsEntry, int) {
	where := "true"
	args := []any{}
	if userID != 0 {
		where = "user_id = $1"
		args = append(args, userID)
	}
	var total int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM goo_points_log WHERE `+where, args...).Scan(&total)
	rows, err := s.store.Pool.Query(ctx, `SELECT id, user_id, points, type, order_id, note, date FROM goo_points_log WHERE `+where+
		fmt.Sprintf(` ORDER BY date DESC, id DESC LIMIT %d OFFSET %d`, limit, offset), args...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()
	var out []*gptsEntry
	for rows.Next() {
		e := &gptsEntry{}
		if rows.Scan(&e.ID, &e.UserID, &e.Points, &e.Type, &e.OrderID, &e.Note, &e.Date) == nil {
			out = append(out, e)
		}
	}
	return out, total
}

// gptsDescription is an entry's event description.
func (s *Server) gptsDescription(ctx context.Context, e *gptsEntry) string {
	d := defaultStr(gptsEventLabels[e.Type], e.Type)
	if e.OrderID != 0 {
		d += ` (Order <a href="` + gooEsc(s.gooEndpointURL(ctx, "myaccount", "view-order", itoa64(e.OrderID))) + `">#` + itoa64(e.OrderID) + `</a>)`
	}
	if e.Type == "admin-adjustment" && e.Note != "" {
		d += `: ` + gooEsc(e.Note)
	}
	return d
}

// Orders ----------------------------------------------------------------------------------------------------

// gptsOrderPoints are the points an order earns (its line items with the earn rules).
func (s *Server) gptsOrderPoints(ctx context.Context, o *gooOrder) int {
	discounted := s.gooExtBool(gptsKey, "earn_on_discounted_price")
	total := 0.0
	for _, it := range o.ItemsOf("line_item") {
		if it.Quantity <= 0 {
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
		line := it.Subtotal
		if discounted {
			line = it.Total
		}
		if o.PricesIncludeTax {
			line += it.SubtotalTax
			if discounted {
				line = it.Total + it.TotalTax
			}
		}
		total += s.gptsProductPoints(ctx, p, line/it.Quantity) * it.Quantity
	}
	return s.gptsRound(total)
}

// gptsAward credits an order's points once (the order meta _wc_points_earned).
func (s *Server) gptsAward(ctx context.Context, o *gooOrder) {
	if o.CustomerID == 0 || o.Meta["_wc_points_earned"] != nil || o.Meta["_subscription_renewal"] != nil && !s.gooExtBool(gptsKey, "earn_on_renewals") {
		return
	}
	pts := s.gptsOrderPoints(ctx, o)
	o.Meta["_wc_points_earned"] = float64(pts)
	_ = s.gooSaveOrder(ctx, o, false)
	if pts > 0 {
		_ = s.gptsLog(ctx, o.CustomerID, pts, "order-placed", o.ID, "")
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Customer earned %d %s for purchase.", pts, s.gptsLabel(pts)), false, "")
	}
}

// gptsReverse takes back an order's earned points and, with refundRedeemed, returns its redeemed ones
// (cancelled / refunded; a failed order keeps them, see the order_status_changed hook).
func (s *Server) gptsReverse(ctx context.Context, o *gooOrder, refundRedeemed bool) {
	if o.CustomerID == 0 {
		return
	}
	changed := false
	if earned := gxaInt(o.Meta, "_wc_points_earned"); earned > 0 && o.Meta["_wc_points_earned_reversed"] == nil {
		_ = s.gptsLog(ctx, o.CustomerID, -earned, "order-cancelled", o.ID, "")
		o.Meta["_wc_points_earned_reversed"] = true
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("%d %s removed.", earned, s.gptsLabel(earned)), false, "")
		changed = true
	}
	if red := gxaInt(o.Meta, "_wc_points_redeemed"); refundRedeemed && red > 0 && o.Meta["_wc_points_logged_redemption"] == true && o.Meta["_wc_points_redeem_refunded"] == nil {
		_ = s.gptsLog(ctx, o.CustomerID, red, "order-refunded", o.ID, "")
		o.Meta["_wc_points_redeem_refunded"] = true
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("%d %s refunded to the customer.", red, s.gptsLabel(red)), false, "")
		changed = true
	}
	if changed {
		_ = s.gooSaveOrder(ctx, o, false)
	}
}

// gptsRedebit takes back the redeemed points that cancelling or refunding the order returned, when the
// order comes back (paid late, reactivated): it keeps its points discount, so the same points would
// otherwise pay twice. The balance check and the debit run under gptsReserve's per-customer lock. It
// returns an error when the balance no longer covers them; the flag then stays for a later retry.
func (s *Server) gptsRedebit(ctx context.Context, o *gooOrder) error {
	red := gxaInt(o.Meta, "_wc_points_redeemed")
	if o.CustomerID == 0 || red <= 0 || o.Meta["_wc_points_redeem_refunded"] == nil {
		return nil
	}
	conn, err := s.store.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	key := "goo_points:" + itoa64(o.CustomerID)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, key); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key)
	}()
	if s.gptsBalance(ctx, o.CustomerID) < red {
		return fmt.Errorf("The customer's %s balance no longer covers the %d %s this order redeemed.", strings.ToLower(s.gptsLabel(2)), red, s.gptsLabel(red))
	}
	if err := s.gptsLog(ctx, o.CustomerID, -red, "order-redeem", o.ID, "Order reactivated"); err != nil {
		return err
	}
	delete(o.Meta, "_wc_points_redeem_refunded")
	_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_orders SET meta = meta - '_wc_points_redeem_refunded' WHERE id = $1`, o.ID)
	_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("%d %s debited again.", red, s.gptsLabel(red)), false, "")
	return nil
}

// Redemption ----------------------------------------------------------------------------------------------

// gptsReserve debits the points a checkout redeems before the order is saved. The balance check and the
// debit run under a per-customer advisory lock, so two checkouts at once cannot both spend the same
// points (each would otherwise get the discount and leave the balance negative).
func gptsReserve(s *Server, ctx context.Context, _ *gooCart, o *gooOrder) (func(), error) {
	pts := gxaInt(o.Meta, "_wc_points_redeemed")
	if !s.gooExtOn(gptsKey) || o.CustomerID == 0 || pts <= 0 {
		return nil, nil
	}
	conn, err := s.store.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	key := "goo_points:" + itoa64(o.CustomerID)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, key); err != nil {
		return nil, err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key)
	}()
	if s.gptsBalance(ctx, o.CustomerID) < pts {
		return nil, fmt.Errorf("You no longer have enough %s for this discount. Please review your order.", strings.ToLower(s.gptsLabel(2)))
	}
	if err := s.gptsLog(ctx, o.CustomerID, -pts, "order-redeem", 0, ""); err != nil {
		return nil, err
	}
	o.Meta["_wc_points_logged_redemption"], o.Meta["_wc_points_reserved"] = true, true
	return func() {
		_ = s.gptsLog(context.WithoutCancel(ctx), o.CustomerID, pts, "order-refunded", 0, "Checkout did not complete.")
	}, nil
}

// gptsRedeemable is the discount and the points a user may redeem on a cart, at most want points
// (0 = as many as allowed).
func (s *Server) gptsRedeemable(ctx context.Context, cart *gooCart, userID int64, want int) (float64, int) {
	balance := s.gptsBalance(ctx, userID)
	if balance <= 0 || cart == nil {
		return 0, 0
	}
	pts := balance
	if want > 0 && want < pts && s.gooExtBool(gptsKey, "partial_redemption") {
		pts = want
	}
	discount := s.gptsPointsValue(pts)
	base := cart.CartContentsTotal
	if s.gooOptString("tax_display_cart") == "incl" {
		base += cart.CartContentsTax
	}
	discount = min(discount, base)
	// Per-product / per-category maximums: each line contributes at most its own maximum.
	if itemsCap, limited := s.gptsItemsCapMaxd(ctx, cart); limited {
		discount = min(discount, itemsCap)
	}
	// The cart maximum: a fixed amount or a percentage of the cart ("10%").
	if maxD, ok := gptsOverride(s.gooExtString(gptsKey, "max_discount"), base); ok && maxD > 0 {
		discount = min(discount, maxD)
	}
	if pct := s.gooExtFloat(gptsKey, "max_points_discount_percent"); pct > 0 {
		discount = min(discount, gooRound(base*pct/100, s.gooDecimals()))
	}
	if minD := s.gooExtFloat(gptsKey, "min_discount"); minD > 0 && discount < minD {
		return 0, 0
	}
	discount = gooRound(discount, s.gooDecimals())
	if discount <= 0 {
		return 0, 0
	}
	return discount, min(s.gptsPointsFor(discount), pts)
}

func gptsSessionPoints(sess *gooSession) (int, bool) {
	if sess == nil || sess.Data.Extra == nil {
		return 0, false
	}
	m, ok := sess.Data.Extra[gptsKey].(map[string]any)
	if !ok {
		return 0, false
	}
	return gxaInt(m, "points"), true
}

func (s *Server) gptsMessage(field string, points int, value float64) string {
	return strings.NewReplacer("{points}", strconv.Itoa(points), "{points_label}", s.gptsLabel(points),
		"{points_value}", string(s.gooPrice(value))).Replace(s.gooExtString(gptsKey, field))
}

func init() {
	gooRegisterExtension(&gooExtension{
		Key: gptsKey, Name: "Points and Rewards", Icon: "dashicons-awards", Category: "Customer engagement",
		Description: "Reward customers for purchases and other actions with points which can be redeemed for discounts on future orders. Set earn and redemption ratios, per-product and per-category points, and limits.",
		Fields:      gptsFields(),
		Pages:       []gooExtMenuItem{{Page: "points-and-rewards", Title: "Points & Rewards"}},
	})
	gooExtAdminPages["points-and-rewards"] = func(s *Server, c *gin.Context) { s.gptsAdmin(c) }

	gooProductPanels = append(gooProductPanels, &gooProductPanel{ID: "wc_points_rewards", Label: "Points", Module: gptsKey,
		Render: gptsPanelRender, Save: gptsPanelSave})

	gooOnHTML("single_before_add_to_cart", func(s *Server, c *gin.Context, p *gooProduct) template.HTML {
		if !s.gooExtOn(gptsKey) || p == nil {
			return ""
		}
		ctx := c.Request.Context()
		pts := s.gptsRound(s.gptsProductPoints(ctx, p, s.gooDisplayPrice(ctx, p)))
		if pts <= 0 || s.gooExtString(gptsKey, "single_product_message") == "" {
			return ""
		}
		return template.HTML(`<p class="points">` + strings.NewReplacer("{points}", strconv.Itoa(pts), "{points_label}", s.gptsLabel(pts)).Replace(s.gooExtString(gptsKey, "single_product_message")) + `</p>`)
	})

	for _, loc := range []string{"cart_before", "checkout_before_payment"} {
		gooOnPage(loc, func(s *Server, c *gin.Context) template.HTML {
			if !s.gooExtOn(gptsKey) {
				return ""
			}
			return template.HTML(s.gptsCartMessages(c))
		})
	}

	gooFrontRoutes = append(gooFrontRoutes, gptsFrontRoute)

	gooCartFeesHooks = append(gooCartFeesHooks, func(s *Server, ctx context.Context, cart *gooCart) {
		if !s.gooExtOn(gptsKey) {
			return
		}
		want, ok := gptsSessionPoints(cart.sess)
		u := currentUser(cart.c)
		if !ok || u == nil {
			return
		}
		discount, pts := s.gptsRedeemable(ctx, cart, u.ID, want)
		if discount <= 0 {
			return
		}
		f := cart.AddFee("wc_points_redemption", "Points redemption", -discount, false, "")
		f.Meta = map[string]any{"points": float64(pts)}
	})

	gooCheckoutHooks = append(gooCheckoutHooks, func(s *Server, ctx context.Context, cart *gooCart, o *gooOrder) error {
		if !s.gooExtOn(gptsKey) {
			return nil
		}
		for _, f := range cart.Fees {
			if f.ID == "wc_points_redemption" {
				o.Meta["_wc_points_redeemed"] = f.Meta["points"]
				o.Meta["_wc_points_redeemed_amount"] = -f.Total
			}
		}
		return nil
	})
	gooCheckoutReserveHooks = append(gooCheckoutReserveHooks, gptsReserve)
	gooOn("order_created", 50, func(s *Server, ctx context.Context, arg any) {
		o, ok := arg.(*gooOrder)
		if !ok || !s.gooExtOn(gptsKey) || o.CustomerID == 0 {
			return
		}
		if pts := gxaInt(o.Meta, "_wc_points_redeemed"); pts > 0 && o.Meta["_wc_points_reserved"] == true {
			// Checkout debited the points (gptsReserve) before the order had an id: link the entry.
			_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_points_log SET order_id = $1 WHERE id = (SELECT id FROM goo_points_log
				WHERE user_id = $2 AND type = 'order-redeem' AND order_id = 0 AND points = $3 ORDER BY id DESC LIMIT 1)`, o.ID, o.CustomerID, -pts)
			_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("%d %s redeemed for a %s discount.", pts, s.gptsLabel(pts),
				s.gooPricePlain(gxaFloat(o.Meta, "_wc_points_redeemed_amount"), o.Currency)), false, "")
		} else if pts > 0 && o.Meta["_wc_points_logged_redemption"] == nil {
			o.Meta["_wc_points_logged_redemption"] = true
			_ = s.gooSaveOrder(ctx, o, false)
			_ = s.gptsLog(ctx, o.CustomerID, -pts, "order-redeem", o.ID, "")
			_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("%d %s redeemed for a %s discount.", pts, s.gptsLabel(pts),
				s.gooPricePlain(gxaFloat(o.Meta, "_wc_points_redeemed_amount"), o.Currency)), false, "")
		}
	})
	gooOn("cart_emptied", 50, func(s *Server, ctx context.Context, arg any) {
		if cart, ok := arg.(*gooCart); ok && cart.sess != nil && cart.sess.Data.Extra != nil {
			if _, has := cart.sess.Data.Extra[gptsKey]; has {
				delete(cart.sess.Data.Extra, gptsKey)
				cart.sess.touch()
			}
		}
	})
	gooOn("order_status_changed", 50, func(s *Server, ctx context.Context, arg any) {
		ch, ok := arg.(*gooStatusChange)
		if !ok || !s.gooExtOn(gptsKey) {
			return
		}
		o := ch.Order
		switch {
		case containsStr(gooPaidStatuses, ch.To) && containsStr(gooPaidStatuses, o.Status):
			s.gptsAward(ctx, o)
		case ch.To == "cancelled" || ch.To == "refunded":
			s.gptsReverse(ctx, o, true)
		case ch.To == "failed":
			// Redeemed points stay spent: the order can still be paid (Pay for order) with their discount.
			s.gptsReverse(ctx, o, false)
		}
	})
	gooOn("customer_created", 50, func(s *Server, ctx context.Context, arg any) {
		if !s.gooExtOn(gptsKey) {
			return
		}
		var uid int64
		switch v := arg.(type) {
		case *gooNewCustomer:
			uid = v.UserID
		case int64:
			uid = v
		}
		if pts := s.gooExtInt(gptsKey, "account_signup_points"); pts > 0 && uid != 0 {
			_ = s.gptsLog(ctx, uid, pts, "account-signup", 0, "")
		}
	})
	gooOn("hourly", 50, func(s *Server, ctx context.Context, _ any) {
		if s.gooExtOn(gptsKey) {
			s.gptsReviewPoints(ctx)
		}
	})
	gooOn("daily", 50, func(s *Server, ctx context.Context, _ any) {
		if s.gooExtOn(gptsKey) {
			s.gptsReviewPoints(ctx)
			s.gptsExpire(ctx)
		}
	})

	gooAccountEndpoints = append(gooAccountEndpoints, &gooAccountEndpoint{Slug: "points-and-rewards", Title: "Points", Module: gptsKey, After: "orders", Render: gptsAccountRender})
}

func gptsFields() []gooField {
	return []gooField{
		{ID: "general_title", Title: "Points Settings", Type: "title", Desc: "Adjust the number of points customers earn and the value of points when redeemed."},
		{ID: "earn_points", Title: "Earn Points Conversion Rate: points", Type: "number", Default: 1.0, Min: "0", Step: "any", Desc: "Points earned…"},
		{ID: "earn_points_amount", Title: "Earn Points Conversion Rate: per amount", Type: "price", Default: "1", Desc: "…for every this much spent (e.g. 1 point per $1)."},
		{ID: "points_rounding", Title: "Earn Points Rounding Mode", Type: "select", Default: "round", Options: []gooOpt{{"round", "Round to nearest integer"}, {"floor", "Always round down"}, {"ceil", "Always round up"}}},
		{ID: "earn_on_discounted_price", Title: "Earn points on discounted price", Type: "checkbox", Default: true, Desc: "Points are calculated on the line total after coupons and discounts"},
		{ID: "redeem_points", Title: "Redemption Conversion Rate: points", Type: "number", Default: 100.0, Min: "0", Step: "any", Desc: "This many points…"},
		{ID: "redeem_points_amount", Title: "Redemption Conversion Rate: value", Type: "price", Default: "1", Desc: "…are worth this much discount (e.g. 100 points = $1)."},
		{ID: "partial_redemption", Title: "Partial Redemption", Type: "checkbox", Default: false, Desc: "Enable partial redemption (lets users enter how many points they wish to apply to the order)"},
		{ID: "min_discount", Title: "Minimum Points Discount", Type: "price", Default: "", Desc: "Set the minimum discount amount that can be redeemed on the cart. Leave blank for no minimum."},
		{ID: "max_discount", Title: "Maximum Points Discount", Type: "text", Default: "", Desc: "Set the maximum discount that can be redeemed on the cart: a fixed amount or a percentage of the cart (e.g. 10%). Leave blank for no maximum."},
		{ID: "max_points_discount_percent", Title: "Maximum Product Points Discount (%)", Type: "number", Default: "", Min: "0", Step: "any", Desc: "The maximum percentage of the order the points discount may cover. Leave blank for no limit."},
		{ID: "points_label_singular", Title: "Points Label (singular)", Type: "text", Default: "Point"},
		{ID: "points_label_plural", Title: "Points Label (plural)", Type: "text", Default: "Points"},
		{ID: "points_expiry_months", Title: "Points expire after (months)", Type: "number", Default: 0.0, Min: "0", Desc: "Earned points expire this many months after they were earned. 0 = never."},
		{ID: "earn_on_renewals", Title: "Subscription renewals", Type: "checkbox", Default: true, Desc: "Earn points for subscription renewal orders"},
		{ID: "general_end", Type: "sectionend"},
		{ID: "messages_title", Title: "Product / Cart / Checkout Messages", Type: "title", Desc: "Adjust the message by using {points} and {points_label} to show the points and their label; {points_value} is the discount value."},
		{ID: "single_product_message", Title: "Single Product Page Message", Type: "textarea", Default: "Purchase this product now and earn <strong>{points}</strong> {points_label}!"},
		{ID: "earn_points_message", Title: "Earn Points Cart/Checkout Message", Type: "textarea", Default: "Complete your order and earn <strong>{points}</strong> {points_label} for a discount on a future purchase"},
		{ID: "redeem_points_message", Title: "Redeem Points Cart/Checkout Message", Type: "textarea", Default: "Use <strong>{points}</strong> {points_label} for a <strong>{points_value}</strong> discount on this order!"},
		{ID: "messages_end", Type: "sectionend"},
		{ID: "actions_title", Title: "Points Earned for Actions", Type: "title", Desc: "Customers can also earn points for actions like creating an account or writing a product review."},
		{ID: "account_signup_points", Title: "Points earned for account signup", Type: "number", Default: 0.0, Min: "0"},
		{ID: "product_review_points", Title: "Points earned for writing a review", Type: "number", Default: 0.0, Min: "0"},
		{ID: "actions_end", Type: "sectionend"},
	}
}

// Product panel ----------------------------------------------------------------------------------------

func gptsPanelRender(s *Server, c *gin.Context, p *gooProduct) template.HTML {
	return template.HTML(`<div id="wc_points_rewards" class="options_group"><p class="form-field _wc_points_earned_field"><label for="_wc_points_earned">Points earned</label>` +
		`<input type="text" class="short" name="_wc_points_earned" id="_wc_points_earned" value="` + gooEsc(gxaStr(p.Meta, "_wc_points_earned")) + `" placeholder="Default"> ` +
		`<span class="description">This can be either a fixed number of points earned for purchasing this product, or a percentage which assigns points based on the price. For example, if you want to award points equal to double the price of the product, enter 200%. This setting modifies the global Points Conversion Rate and overrides any category value. Leave blank to use the default.</span></p>` +
		`<p class="form-field _wc_points_max_discount_field"><label for="_wc_points_max_discount">Maximum points discount</label><input type="text" class="short" name="_wc_points_max_discount" id="_wc_points_max_discount" value="` + gooEsc(gxaStr(p.Meta, "_wc_points_max_discount")) + `" placeholder="Default"> ` +
		`<span class="description">Enter either a fixed maximum discount amount or percentage which can be redeemed for this product. Leave blank to use the default.</span></p></div>`)
}

func gptsPanelSave(s *Server, c *gin.Context, p *gooProduct) {
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	for _, k := range []string{"_wc_points_earned", "_wc_points_max_discount"} {
		v := strings.TrimSpace(c.PostForm(k))
		if _, ok := gptsOverride(v, 0); ok {
			p.Meta[k] = v
		} else {
			delete(p.Meta, k)
		}
	}
}

// Cart messages and redemption ---------------------------------------------------------------------------

// gptsCartMessages are the earn and redeem notices above the cart / checkout.
func (s *Server) gptsCartMessages(c *gin.Context) string {
	ctx := c.Request.Context()
	cart := s.gooCartFor(c)
	if cart.IsEmpty() {
		return ""
	}
	var b strings.Builder
	earn := 0.0
	discounted := s.gooExtBool(gptsKey, "earn_on_discounted_price")
	for _, it := range cart.Items {
		line := it.LineSubtotal
		if discounted {
			line = it.LineTotal
		}
		if s.gooPricesIncludeTax() {
			line += it.LineSubtotalTax
		}
		if it.Quantity > 0 {
			earn += s.gptsProductPoints(ctx, it.Product(), line/float64(it.Quantity)) * float64(it.Quantity)
		}
	}
	if pts := s.gptsRound(earn); pts > 0 {
		b.WriteString(`<div class="woocommerce-info wc_points_rewards_earn_points">` + s.gptsMessage("earn_points_message", pts, 0) + `</div>`)
	}
	u := currentUser(c)
	if u == nil {
		return b.String()
	}
	if _, applied := gptsSessionPoints(cart.sess); applied {
		return b.String()
	}
	discount, pts := s.gptsRedeemable(ctx, cart, u.ID, 0)
	if discount <= 0 {
		return b.String()
	}
	b.WriteString(`<div class="woocommerce-info wc_points_redeem_earn_points"><form class="wc_points_rewards_apply_discount" action="" method="post">` +
		`<input type="hidden" name="goo_module" value="points_rewards"><input type="hidden" name="goo_points_redeem" value="1">` +
		`<input type="hidden" name="_wpnonce" value="` + s.nonce(c, "goo-points-redeem") + `">` + s.gptsMessage("redeem_points_message", pts, discount))
	if s.gooExtBool(gptsKey, "partial_redemption") {
		b.WriteString(` <input type="number" min="0" max="` + strconv.Itoa(pts) + `" name="wc_points_rewards_apply_discount_amount" class="input-text" placeholder="Points to apply" style="width:120px">`)
	}
	b.WriteString(` <input type="submit" class="button wc_points_rewards_apply_discount" name="wc_points_rewards_apply_discount" value="Apply Discount"></form></div>`)
	return b.String()
}

// gptsFrontRoute handles the "Apply Discount" / remove forms (POST with goo_module=points_rewards).
func gptsFrontRoute(s *Server, c *gin.Context, path string) bool {
	if c.Request.Method != http.MethodPost || !s.gooExtOn(gptsKey) || (c.PostForm("goo_points_redeem") == "" && c.PostForm("goo_points_remove") == "") {
		return false
	}
	back := gooXCSafeReferer(c, s.gooPageURL(c.Request.Context(), "cart")) // same-site only: no open redirect
	u := currentUser(c)
	sess := s.gooSessionFor(c)
	if u == nil || !s.checkNonce(c, "goo-points-redeem") {
		s.gooAddNotice(c, "error", "Please log in to redeem your points.")
		s.gooSaveSession(c)
		c.Redirect(http.StatusFound, back)
		return true
	}
	if sess.Data.Extra == nil {
		sess.Data.Extra = map[string]any{}
	}
	if c.PostForm("goo_points_remove") != "" {
		delete(sess.Data.Extra, gptsKey)
		sess.touch()
		s.gooAddNotice(c, "success", "Points discount removed.")
		s.gooSaveSession(c)
		c.Redirect(http.StatusFound, back)
		return true
	}
	want, _ := strconv.Atoi(strings.TrimSpace(c.PostForm("wc_points_rewards_apply_discount_amount")))
	cart := s.gooCartFor(c)
	discount, _ := s.gptsRedeemable(c.Request.Context(), cart, u.ID, want)
	if discount <= 0 {
		s.gooAddNotice(c, "error", "You do not have enough "+gooEsc(s.gptsLabel(2))+" to redeem a discount on this order.")
	} else {
		sess.Data.Extra[gptsKey] = map[string]any{"points": float64(max(want, 0))}
		sess.touch()
		cart.Calculate()
		s.gooAddNotice(c, "success", "Discount applied for redeeming your "+gooEsc(s.gptsLabel(2))+"!")
	}
	s.gooSaveSession(c)
	c.Redirect(http.StatusFound, back)
	return true
}

// Jobs ---------------------------------------------------------------------------------------------------

// gptsReviewPoints credits approved product reviews of customers (once per product and customer).
func (s *Server) gptsReviewPoints(ctx context.Context) {
	pts := s.gooExtInt(gptsKey, "product_review_points")
	if pts <= 0 {
		return
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT DISTINCT c.user_id, c.post_id FROM comments c JOIN posts p ON p.id = c.post_id
		WHERE p.type = 'product' AND c.status = 'approved' AND c.deleted_at IS NULL AND c.user_id IS NOT NULL AND c.type IN ('review', 'comment')
		AND NOT EXISTS (SELECT 1 FROM goo_points_log l WHERE l.user_id = c.user_id AND l.type = 'product-review' AND l.note = 'product:' || c.post_id)`)
	if err != nil {
		return
	}
	type pair struct{ uid, pid int64 }
	var list []pair
	for rows.Next() {
		var p pair
		if rows.Scan(&p.uid, &p.pid) == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	for _, p := range list {
		_ = s.gptsLog(ctx, p.uid, pts, "product-review", 0, "product:"+itoa64(p.pid))
	}
}

// gptsExpire expires the unused points earned before the expiry period.
func (s *Server) gptsExpire(ctx context.Context) {
	months := s.gooExtInt(gptsKey, "points_expiry_months")
	if months <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, -months, 0)
	rows, err := s.store.Pool.Query(ctx, `SELECT user_id, sum(points - used) FROM goo_points_log WHERE points > 0 AND points > used AND date < $1 AND type <> 'expire' GROUP BY user_id`, cutoff)
	if err != nil {
		return
	}
	type exp struct {
		uid int64
		n   int
	}
	var list []exp
	for rows.Next() {
		var e exp
		if rows.Scan(&e.uid, &e.n) == nil && e.n > 0 {
			list = append(list, e)
		}
	}
	rows.Close()
	for _, e := range list {
		n := min(e.n, s.gptsBalance(ctx, e.uid))
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_points_log SET used = points WHERE user_id = $1 AND points > 0 AND date < $2`, e.uid, cutoff)
		if n > 0 {
			if _, err := s.store.Pool.Exec(ctx, `INSERT INTO goo_points_log (user_id, points, type, note) VALUES ($1,$2,'expire','')`, e.uid, -n); err == nil {
				_ = s.gooSetUserMeta(ctx, e.uid, map[string]any{"goo_points_balance": float64(s.gptsBalance(ctx, e.uid))})
			}
		}
	}
}

// My Account ------------------------------------------------------------------------------------------------

// gptsAccountRender is My Account › Points: the balance and the history.
func gptsAccountRender(s *Server, c *gin.Context, arg string) template.HTML {
	u := currentUser(c)
	if u == nil || !s.gooExtOn(gptsKey) {
		return ""
	}
	ctx := c.Request.Context()
	bal := s.gptsBalance(ctx, u.ID)
	var b strings.Builder
	b.WriteString(`<h2>My ` + gooEsc(s.gptsLabel(2)) + `</h2><p>You have <strong>` + strconv.Itoa(bal) + `</strong> ` + gooEsc(s.gptsLabel(bal)) + `</p>`)
	entries, _ := s.gptsEntries(ctx, u.ID, 50, 0)
	if len(entries) > 0 {
		b.WriteString(`<table class="shop_table my_account_points_rewards my_account_orders"><thead><tr><th class="points-rewards-event-description"><span class="nobr">Event</span></th>` +
			`<th class="points-rewards-event-date"><span class="nobr">Date</span></th><th class="points-rewards-event-points"><span class="nobr">` + gooEsc(s.gptsLabel(2)) + `</span></th></tr></thead><tbody>`)
		for _, e := range entries {
			d := e.Date
			b.WriteString(`<tr class="points-event"><td class="points-rewards-event-description">` + s.gptsDescription(ctx, e) + `</td>` +
				`<td class="points-rewards-event-date"><abbr>` + s.gxaDate(&d) + `</abbr></td><td class="points-rewards-event-points" width="1%">` + fmt.Sprintf("%+d", e.Points) + `</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	}
	return template.HTML(b.String())
}

// Admin ------------------------------------------------------------------------------------------------------

// gptsAdmin is Goocommerce › Points & Rewards: Manage Points, Points Log and category points.
func (s *Server) gptsAdmin(c *gin.Context) {
	ctx := c.Request.Context()
	if c.Request.Method == "POST" {
		if !s.checkNonce(c, "gpts-admin") {
			s.denyNonce(c)
			return
		}
		switch c.PostForm("points_action") {
		case "adjust":
			uid, _ := strconv.ParseInt(c.PostForm("user_id"), 10, 64)
			if uid == 0 {
				if u, err := s.gxaFindUser(ctx, c.PostForm("customer")); err == nil {
					uid = u.ID
				}
			}
			n, err := strconv.Atoi(strings.TrimSpace(c.PostForm("points")))
			if uid == 0 || err != nil || n == 0 {
				gxaAdminRedirect(c, "points-and-rewards", "", "Please choose a customer and enter a number of points.", true)
				return
			}
			if c.PostForm("mode") == "set" {
				n -= s.gptsBalance(ctx, uid)
			}
			by := ""
			if u := currentUser(c); u != nil {
				by = u.DisplayName
			}
			_ = s.gptsLog(ctx, uid, n, "admin-adjustment", 0, defaultStr(strings.TrimSpace(c.PostForm("note")), "by "+by))
			gxaAdminRedirect(c, "points-and-rewards", "", "Points balance updated.", false)
		case "categories":
			m := map[string]any{}
			for k, v := range c.Request.PostForm {
				if strings.HasPrefix(k, "cat_points[") && len(v) > 0 {
					id := strings.TrimSuffix(strings.TrimPrefix(k, "cat_points["), "]")
					if _, ok := gptsOverride(v[0], 0); ok {
						m[id] = strings.TrimSpace(v[0])
					}
				}
			}
			maxd := map[string]any{}
			for k, v := range c.Request.PostForm {
				if strings.HasPrefix(k, "cat_max_discount[") && len(v) > 0 {
					id := strings.TrimSuffix(strings.TrimPrefix(k, "cat_max_discount["), "]")
					if _, ok := gptsOverride(v[0], 0); ok {
						maxd[id] = strings.TrimSpace(v[0])
					}
				}
			}
			vals := s.gooExtSettings(gptsKey)
			vals["category_points"] = m
			vals["category_max_discount"] = maxd
			if err := s.gooSetOpts(ctx, map[string]any{"ext_" + gptsKey + "_settings": vals}); err != nil {
				gxaAdminRedirect(c, "points-and-rewards", "tab=categories", err.Error(), true)
				return
			}
			gxaAdminRedirect(c, "points-and-rewards", "tab=categories", "Category points saved.", false)
		default:
			gxaAdminRedirect(c, "points-and-rewards", "", "", false)
		}
		return
	}
	nonce := s.nonce(c, "gpts-admin")
	tab := defaultStr(c.Query("tab"), "manage")
	var b strings.Builder
	b.WriteString(`<nav class="nav-tab-wrapper woo-nav-tab-wrapper">`)
	for _, t := range []gooOpt{{"manage", "Manage Points"}, {"log", "Points Log"}, {"categories", "Category Points"}} {
		cls := "nav-tab"
		if t.Value == tab {
			cls += " nav-tab-active"
		}
		b.WriteString(`<a href="/wp-admin/goocommerce?page=points-and-rewards&amp;tab=` + t.Value + `" class="` + cls + `">` + t.Label + `</a>`)
	}
	b.WriteString(`</nav>`)
	customer, _ := strconv.ParseInt(c.Query("customer"), 10, 64)
	page := gxaPage(c)
	switch tab {
	case "log":
		entries, total := s.gptsEntries(ctx, customer, 20, (page-1)*20)
		base := "/wp-admin/goocommerce?page=points-and-rewards&tab=log"
		if customer != 0 {
			base += "&customer=" + itoa64(customer)
		}
		b.WriteString(`<form method="get" action="/wp-admin/goocommerce"><input type="hidden" name="page" value="points-and-rewards"><input type="hidden" name="tab" value="log">` +
			`<div class="tablenav top"><div class="alignleft actions"><input type="number" name="customer" placeholder="Customer ID" value="` + map[bool]string{true: itoa64(customer), false: ""}[customer != 0] + `"> <input type="submit" class="button" value="Filter"></div>` +
			gxaPagination(base, page, 20, total) + `</div></form>`)
		b.WriteString(`<table class="wp-list-table widefat fixed striped"><thead><tr><th class="column-primary">Customer</th><th>Points</th><th>Event</th><th>Order</th><th>Date</th></tr></thead><tbody>`)
		if len(entries) == 0 {
			b.WriteString(`<tr class="no-items"><td colspan="5">No points log entries found.</td></tr>`)
		}
		for _, e := range entries {
			d := e.Date
			b.WriteString(`<tr><td class="column-primary"><a href="` + gooEsc(base) + `&amp;customer=` + itoa64(e.UserID) + `">` + s.gxaUserLabel(ctx, e.UserID, false) + `</a></td>` +
				`<td>` + fmt.Sprintf("%+d", e.Points) + `</td><td>` + gooEsc(defaultStr(gptsEventLabels[e.Type], e.Type)) + map[bool]string{true: ": " + gooEsc(e.Note), false: ""}[e.Type == "admin-adjustment" && e.Note != ""] + `</td>` +
				`<td>` + gxaOrderLink(e.OrderID) + `</td><td>` + s.gxaDateTime(&d) + `</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	case "categories":
		cats, _, _ := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: "product_cat", OrderBy: "name", PerPage: 500})
		m, _ := s.gooExtSettings(gptsKey)["category_points"].(map[string]any)
		md, _ := s.gooExtSettings(gptsKey)["category_max_discount"].(map[string]any)
		b.WriteString(`<form method="post" action="/wp-admin/goocommerce?page=points-and-rewards"><input type="hidden" name="_wpnonce" value="` + nonce + `"><input type="hidden" name="points_action" value="categories">` +
			`<p>Points earned for products in a category: a fixed number of points or a percentage of the price (e.g. 200%). A product's own setting takes precedence. Leave blank to use the default.</p>` +
			`<p>Maximum points discount: the most a product of the category may be discounted with points, a fixed amount or a percentage of its price (e.g. 50%).</p>` +
			`<table class="wp-list-table widefat fixed striped"><thead><tr><th>Category</th><th>Points earned</th><th>Maximum points discount</th></tr></thead><tbody>`)
		for _, t := range cats {
			b.WriteString(`<tr><td>` + gooEsc(t.Name) + `</td><td><input type="text" name="cat_points[` + itoa64(t.ID) + `]" value="` + gooEsc(gxaStr(m, itoa64(t.ID))) + `" placeholder="Default"></td>` +
				`<td><input type="text" name="cat_max_discount[` + itoa64(t.ID) + `]" value="` + gooEsc(gxaStr(md, itoa64(t.ID))) + `" placeholder="Default"></td></tr>`)
		}
		b.WriteString(`</tbody></table><p class="submit"><button type="submit" class="button button-primary">Save category points</button></p></form>`)
	default:
		search := strings.TrimSpace(c.Query("s"))
		q := `SELECT u.id, coalesce(sum(l.points),0) FROM users u JOIN goo_points_log l ON l.user_id = u.id`
		args := []any{}
		if customer != 0 {
			q += ` WHERE u.id = $1`
			args = append(args, customer)
		} else if search != "" {
			q += ` WHERE lower(u.login || ' ' || u.email || ' ' || u.display_name) LIKE $1`
			args = append(args, "%"+strings.ToLower(search)+"%")
		}
		q += ` GROUP BY u.id ORDER BY 2 DESC, u.id LIMIT 200`
		type row struct {
			id  int64
			bal int
		}
		var list []row
		if rows, err := s.store.Pool.Query(ctx, q, args...); err == nil {
			for rows.Next() {
				var r row
				if rows.Scan(&r.id, &r.bal) == nil {
					list = append(list, r)
				}
			}
			rows.Close()
		}
		b.WriteString(`<form method="get" action="/wp-admin/goocommerce"><input type="hidden" name="page" value="points-and-rewards"><p class="search-box"><input type="search" name="s" value="` + gooEsc(search) + `"><input type="submit" class="button" value="Search Customers"></p></form>`)
		b.WriteString(`<table class="wp-list-table widefat fixed striped"><thead><tr><th class="column-primary">Customer</th><th>Points</th><th>Update</th></tr></thead><tbody>`)
		if len(list) == 0 {
			b.WriteString(`<tr class="no-items"><td colspan="3">No customers with points found.</td></tr>`)
		}
		for _, r := range list {
			b.WriteString(`<tr><td class="column-primary">` + s.gxaUserLabel(ctx, r.id, true) + ` <a href="/wp-admin/goocommerce?page=points-and-rewards&amp;tab=log&amp;customer=` + itoa64(r.id) + `">View log</a></td><td>` + strconv.Itoa(r.bal) + `</td>` +
				`<td><form method="post" action="/wp-admin/goocommerce?page=points-and-rewards"><input type="hidden" name="_wpnonce" value="` + nonce + `"><input type="hidden" name="points_action" value="adjust"><input type="hidden" name="mode" value="set"><input type="hidden" name="user_id" value="` + itoa64(r.id) + `">` +
				`<input type="number" name="points" value="` + strconv.Itoa(r.bal) + `" style="width:100px"> <button type="submit" class="button">Update</button></form></td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
		b.WriteString(`<h2>Adjust a customer's points</h2><form method="post" action="/wp-admin/goocommerce?page=points-and-rewards"><input type="hidden" name="_wpnonce" value="` + nonce + `"><input type="hidden" name="points_action" value="adjust">` +
			`<table class="form-table"><tbody><tr><th scope="row"><label for="customer">Customer</label></th><td><input type="text" id="customer" name="customer" class="regular-text" placeholder="User ID, username or email"></td></tr>` +
			`<tr><th scope="row"><label for="points">Points</label></th><td><input type="number" id="points" name="points"> ` + gxaSelect("mode", "", []gooOpt{{"add", "Add (or remove with a negative number)"}, {"set", "Set the balance to"}}, "add", "") + `</td></tr>` +
			`<tr><th scope="row"><label for="note">Note</label></th><td><input type="text" id="note" name="note" class="regular-text"></td></tr></tbody></table>` +
			`<p class="submit"><button type="submit" class="button button-primary">Adjust points</button></p></form>`)
	}
	s.gxaAdminRender(c, "points-and-rewards", "Points & Rewards", "goo-ext-points-rewards", `<h1 class="wp-heading-inline">Points &amp; Rewards</h1>`, b.String())
}

// gptsProductMaxd is a product's maximum points discount setting ("5", "50%"): its own, its parent's,
// then the most restrictive of its categories'; "" when none is set.
func (s *Server) gptsProductMaxd(ctx context.Context, p *gooProduct) string {
	if p == nil {
		return ""
	}
	if v := strings.TrimSpace(gxaStr(p.Meta, "_wc_points_max_discount")); v != "" {
		return v
	}
	id := p.ID
	if p.Type == gooTypeVariation && p.ParentID != 0 {
		if parent := s.gooParent(ctx, p); parent != nil {
			if v := strings.TrimSpace(gxaStr(parent.Meta, "_wc_points_max_discount")); v != "" {
				return v
			}
		}
		id = p.ParentID
	}
	m, _ := s.gooExtSettings(gptsKey)["category_max_discount"].(map[string]any)
	if len(m) == 0 {
		return ""
	}
	terms, err := s.store.TermsForPosts(ctx, []int64{id}, "product_cat")
	if err != nil {
		return ""
	}
	best, bestVal := "", math.Inf(1)
	for _, t := range terms[id] {
		v := strings.TrimSpace(gxaStr(m, itoa64(t.ID)))
		// compare on a 100 price: fixed amounts and percentages rank alike for typical prices
		if n, ok := gptsOverride(v, 100); ok && n < bestVal {
			best, bestVal = v, n
		}
	}
	return best
}

// gptsItemsCapMaxd is the most the cart's lines may be discounted with points: each line's maximum
// (fixed per unit or a percentage of its price, capped at the line) or the whole line when it has
// none. limited is false when no line has a maximum.
func (s *Server) gptsItemsCapMaxd(ctx context.Context, cart *gooCart) (float64, bool) {
	incl := s.gooOptString("tax_display_cart") == "incl"
	total, limited := 0.0, false
	for _, it := range cart.Items {
		line := it.LineTotal
		if incl {
			line += it.LineTax
		}
		qty := max(it.Quantity, 1)
		v := s.gptsProductMaxd(ctx, it.Product())
		capV, ok := gptsOverride(v, line/float64(qty))
		if !ok {
			total += line
			continue
		}
		limited = true
		total += min(max(capV, 0)*float64(qty), line)
	}
	return gooRound(total, s.gooDecimals()), limited
}
