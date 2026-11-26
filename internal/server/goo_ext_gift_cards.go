package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math/big"
	netmail "net/mail"
	"sort"
	"strconv"
	"strings"
	"time"
	"wpgo/internal/mail"

	"github.com/jackc/pgx/v5"
)

// Gift Cards (WooCommerce Gift Cards): a "gift-card" product type sold in fixed or custom amounts to a
// recipient; paid orders issue XXXX-XXXX-XXXX-XXXX codes that are e-mailed (now or on the delivery
// date). Codes are redeemed in the cart / checkout as negative fees capped at the order total
// (shipping and taxes included), debited when the order is placed and credited back when it is
// cancelled or refunded. Every balance change is logged in goo_gift_card_activity.

const (
	gcKey  = "gift_cards"
	gcType = "gift-card"
	// gcTaxClass is a tax class without rates: a gift card is a payment, it does not reduce taxes.
	gcTaxClass = "gift-card"
	gcFeePref  = "gift_card_"
)

func init() {
	gooRegisterExtension(&gooExtension{
		Key: gcKey, Name: "Gift Cards", Icon: "dashicons-tickets-alt", Category: "Payments & checkout",
		Description: "Sell prepaid digital gift cards that customers e-mail to friends and redeem in the cart or at checkout, with balances, expiry dates and an activity log.",
		Fields: append([]gooField{
			{Type: "title", Title: "Gift Cards"},
			{ID: "enable_cart", Title: "Cart page", Type: "checkbox", Default: true, Desc: "Show the &quot;Have a gift card?&quot; form on the cart page."},
			{ID: "enable_checkout", Title: "Checkout page", Type: "checkbox", Default: true, Desc: "Show the &quot;Have a gift card?&quot; form on the checkout page."},
			{ID: "account_cards", Title: "My Account", Type: "checkbox", Default: true, Desc: "Let customers add gift cards to their account and see their balances under My Account › Gift Cards."},
			{Type: "sectionend"},
		}, gcDesignFields()...),
		Pages: []gooExtMenuItem{{Page: "gift-cards", Title: "Gift Cards"}},
	})
	gooRegisterProductType(&gooProductTypeDef{
		Type: gcType, Label: "Gift card", Module: gcKey, Virtual: true,
		Purchasable: func(s *Server, ctx context.Context, p *gooProduct) bool {
			return s.gooExtOn(gcKey) && (p.Status == "publish" || p.Status == "private") && (len(gcAmounts(p)) > 0 || gxBool(p.Meta["_gift_card_custom_amount"]))
		},
		NeedsShipping: func(s *Server, ctx context.Context, p *gooProduct) bool { return false },
		AddToCartText: func(s *Server, p *gooProduct, single bool) string {
			if single {
				return "Add to cart"
			}
			return "Select amount"
		},
	})
	gooAddToCartHooks = append(gooAddToCartHooks, gcAddToCart)
	gooCartItemPriceHooks = append(gooCartItemPriceHooks, gcCartPrice)
	gooCartItemDataHooks = append(gooCartItemDataHooks, gcItemData)
	gooCartFeesHooks = append(gooCartFeesHooks, gcCartFees)
	gooPriceHTMLHooks = append(gooPriceHTMLHooks, gcPriceHTML)
	gooOn("order_created", 20, func(s *Server, ctx context.Context, arg any) { s.gcOrderCreated(ctx, arg.(*gooOrder)) })
	gooCheckoutReserveHooks = append(gooCheckoutReserveHooks, gcReserve)
	gooOn("order_status_changed", 20, func(s *Server, ctx context.Context, arg any) { s.gcOrderStatus(ctx, arg.(*gooStatusChange)) })
	gooPaidValueHooks = append(gooPaidValueHooks, func(s *Server, ctx context.Context, o *gooOrder) bool {
		if !s.gooExtOn(gcKey) {
			return false
		}
		for _, it := range o.ItemsOf("line_item") {
			if it.Meta["_goo_gift_card"] != nil {
				return true
			}
		}
		return false
	})
	gooOn("order_partially_refunded", 20, func(s *Server, ctx context.Context, arg any) { s.gcRefundCreditProp(ctx, arg.(*gooRefundEvent)) })
	gooOn("cart_emptied", 20, func(s *Server, ctx context.Context, arg any) {
		if cart, ok := arg.(*gooCart); ok && cart.sess.Data.Extra != nil {
			if _, had := cart.sess.Data.Extra[gcKey]; had {
				delete(cart.sess.Data.Extra, gcKey)
				cart.sess.touch()
			}
		}
	})
	gooOn("daily", 20, func(s *Server, ctx context.Context, _ any) { s.gcDaily(ctx) })
	gooRegisterEmail(&gooEmailDef{ID: "gift_card_received", Title: "Gift card received", Module: gcKey, Customer: true,
		Description: "Gift card e-mails are sent to gift card recipients when their gift card is issued, or on its delivery date.",
		Subject:     "You have received a {giftcard_amount} gift card from {sender}", Heading: "You have received a gift card!"})
	gooRegisterEmail(&gooEmailDef{ID: "gift_card_expiring", Title: "Gift card expiring", Module: gcKey, Customer: true,
		Description: "Gift card recipients get a reminder a week before a gift card with a remaining balance expires.",
		Subject:     "Your {giftcard_amount} gift card expires on {expires}", Heading: "Your gift card is about to expire"})
	gooOn("order_refunded", 20, func(s *Server, ctx context.Context, arg any) { s.gcRefundDisable(ctx, arg.(*gooRefundEvent)) })
}

// Product -----------------------------------------------------------------------------------------------

// gcAmounts are the product's fixed amounts, ascending.
func gcAmounts(p *gooProduct) []float64 {
	var out []float64
	for _, v := range strings.FieldsFunc(gxStr(p.Meta["_gift_card_amounts"]), func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '|' }) {
		if f := gxAtof(v); f > 0 {
			out = append(out, f)
		}
	}
	sort.Float64s(out)
	return out
}

// Cards ------------------------------------------------------------------------------------------------

type gcCard struct {
	ID            int64
	Code          string
	Initial       float64
	Balance       float64
	Currency      string
	OrderID       int64
	OrderItemID   int64
	Recipient     string
	RecipientName string
	Sender        string
	SenderEmail   string
	Message       string
	DeliverDate   *time.Time
	Delivered     bool
	Expires       *time.Time
	Status        string // active, disabled, redeemed (fully used)
	RedeemedBy    int64
	Created       time.Time
	Design        string // design key chosen at purchase; not stored on the card row (see gcCardDesign)
}

const gcCols = `id, code, initial::float8, balance::float8, currency, order_id, order_item_id, recipient, recipient_name, sender, sender_email, message,
	deliver_date, delivered, expires, status, redeemed_by, created`

func gcScan(row pgx.Row) (*gcCard, error) {
	g := &gcCard{}
	err := row.Scan(&g.ID, &g.Code, &g.Initial, &g.Balance, &g.Currency, &g.OrderID, &g.OrderItemID, &g.Recipient, &g.RecipientName, &g.Sender,
		&g.SenderEmail, &g.Message, &g.DeliverDate, &g.Delivered, &g.Expires, &g.Status, &g.RedeemedBy, &g.Created)
	return g, err
}

func (s *Server) gcQuery(ctx context.Context, where string, args ...any) []*gcCard {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+gcCols+` FROM goo_gift_cards WHERE `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*gcCard
	for rows.Next() {
		if g, err := gcScan(rows); err == nil {
			out = append(out, g)
		}
	}
	return out
}

func (s *Server) gcGet(ctx context.Context, id int64) (*gcCard, error) {
	return gcScan(s.store.Pool.QueryRow(ctx, `SELECT `+gcCols+` FROM goo_gift_cards WHERE id = $1`, id))
}

func (s *Server) gcByCode(ctx context.Context, code string) (*gcCard, error) {
	return gcScan(s.store.Pool.QueryRow(ctx, `SELECT `+gcCols+` FROM goo_gift_cards WHERE code = $1`, gcNormalize(code)))
}

// gcNormalize upper-cases a code and restores its dashes.
func gcNormalize(code string) string {
	code = strings.ToUpper(strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.TrimSpace(code)))
	if len(code) == 16 {
		return code[0:4] + "-" + code[4:8] + "-" + code[8:12] + "-" + code[12:16]
	}
	return code
}

const gcAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// gcNewCode is a random XXXX-XXXX-XXXX-XXXX code.
func gcNewCode() string {
	var b strings.Builder
	for i := 0; i < 16; i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(gcAlphabet))))
		b.WriteByte(gcAlphabet[n.Int64()])
	}
	return b.String()
}

// gcCreate inserts a card with a fresh code and logs its issue.
func (s *Server) gcCreate(ctx context.Context, g *gcCard, userID int64, note string) error {
	if g.Currency == "" {
		g.Currency = s.gooCurrency()
	}
	if g.Status == "" {
		g.Status = "active"
	}
	g.Balance = g.Initial
	var err error
	for i := 0; i < 5; i++ {
		g.Code = gcNewCode()
		err = s.store.Pool.QueryRow(ctx, `INSERT INTO goo_gift_cards (code, initial, balance, currency, order_id, order_item_id, recipient, recipient_name, sender,
			sender_email, message, deliver_date, delivered, expires, status, redeemed_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT (code) DO NOTHING RETURNING id, created`, g.Code, g.Initial, g.Balance, g.Currency, g.OrderID, g.OrderItemID, g.Recipient, g.RecipientName,
			g.Sender, g.SenderEmail, g.Message, g.DeliverDate, g.Delivered, g.Expires, g.Status, g.RedeemedBy).Scan(&g.ID, &g.Created)
		if !errors.Is(err, pgx.ErrNoRows) {
			break
		}
	}
	if err != nil {
		return err
	}
	return s.gcLog(ctx, g.ID, "issued", g.Initial, g.OrderID, userID, note)
}

func (s *Server) gcLog(ctx context.Context, cardID int64, typ string, amount float64, orderID, userID int64, note string) error {
	_, err := s.store.Pool.Exec(ctx, `INSERT INTO goo_gift_card_activity (card_id, type, amount, order_id, user_id, note) VALUES ($1,$2,$3,$4,$5,$6)`,
		cardID, typ, amount, orderID, userID, note)
	return err
}

type gcActivity struct {
	ID      int64
	CardID  int64
	Type    string
	Amount  float64
	OrderID int64
	UserID  int64
	Note    string
	Date    time.Time
}

func (s *Server) gcActivities(ctx context.Context, cardID int64) []gcActivity {
	rows, err := s.store.Pool.Query(ctx, `SELECT id, card_id, type, amount::float8, order_id, user_id, note, date FROM goo_gift_card_activity WHERE card_id = $1 ORDER BY date DESC, id DESC`, cardID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []gcActivity
	for rows.Next() {
		var a gcActivity
		if rows.Scan(&a.ID, &a.CardID, &a.Type, &a.Amount, &a.OrderID, &a.UserID, &a.Note, &a.Date) == nil {
			out = append(out, a)
		}
	}
	return out
}

// gcUsable checks that a card can pay: active, not expired, with a balance.
func (s *Server) gcUsable(g *gcCard) error {
	switch {
	case g.Status == "disabled":
		return errors.New("This gift card has been disabled.")
	case g.Expires != nil && time.Now().After(g.Expires.AddDate(0, 0, 1)):
		return errors.New("This gift card has expired.")
	case g.Balance <= 0:
		return errors.New("This gift card has no remaining balance.")
	}
	return nil
}

// gcAdjust changes a balance by delta (negative = debit), keeping the status in step, and logs it.
// The change is one relative UPDATE on the row, so a checkout debiting the same card meanwhile
// (gcReserve) is not overwritten by a balance read earlier; g gets the stored values back.
func (s *Server) gcAdjust(ctx context.Context, g *gcCard, delta float64, typ string, orderID, userID int64, note string) error {
	dp := s.gooDecimals()
	delta = gooRound(delta, dp)
	// The row is locked and its previous balance read in the same statement, so the log can show what
	// really changed when a debit is clamped at zero.
	var before float64
	if err := s.store.Pool.QueryRow(ctx, `UPDATE goo_gift_cards AS t SET balance = greatest(o.balance + $2::numeric, 0),
		status = CASE WHEN $3::bool THEN 'disabled' WHEN o.balance + $2::numeric <= 0 THEN 'redeemed' ELSE 'active' END
		FROM (SELECT id, balance FROM goo_gift_cards WHERE id = $1 FOR UPDATE) o
		WHERE t.id = o.id RETURNING t.balance::float8, t.status, o.balance::float8`, g.ID, delta, g.Status == "disabled").Scan(&g.Balance, &g.Status, &before); err != nil {
		return err
	}
	amount := g.Balance - before
	if amount < 0 {
		amount = -amount
	}
	return s.gcLog(ctx, g.ID, typ, gooRound(amount, dp), orderID, userID, note)
}

// Cart --------------------------------------------------------------------------------------------------

// gcAddToCart validates the amount and the recipient fields.
func gcAddToCart(s *Server, ctx context.Context, p *gooProduct, qty int, form map[string][]string, data map[string]any) error {
	if !s.gooExtOn(gcKey) || p.Type != gcType {
		return nil
	}
	amount := 0.0
	if v := gxForm(form, "wc_gc_giftcard_amount"); v != "" && v != "custom" {
		amount = gxAtof(v)
		ok := false
		for _, a := range gcAmounts(p) {
			if a == amount {
				ok = true
			}
		}
		if !ok {
			return gooCartErr("Please choose a valid gift card amount.")
		}
	} else if gxBool(p.Meta["_gift_card_custom_amount"]) {
		amount = gxAtof(gxForm(form, "wc_gc_giftcard_custom_amount"))
		mn, mx := gxNum(p.Meta["_gift_card_min"]), gxNum(p.Meta["_gift_card_max"])
		if amount <= 0 {
			return gooCartErr("Please enter a gift card amount.")
		}
		if mn > 0 && amount < mn {
			return gooCartErr("The minimum gift card amount is %s.", html.EscapeString(s.gooPricePlain(mn, s.gooCurrency())))
		}
		if mx > 0 && amount > mx {
			return gooCartErr("The maximum gift card amount is %s.", html.EscapeString(s.gooPricePlain(mx, s.gooCurrency())))
		}
	} else {
		return gooCartErr("Please choose a gift card amount.")
	}
	to := gxForm(form, "wc_gc_giftcard_to")
	if a, err := netmail.ParseAddress(to); err != nil || a.Address != to || !strings.Contains(to, "@") {
		return gooCartErr("Please enter a valid email address in the &quot;To&quot; field.")
	}
	from := gxForm(form, "wc_gc_giftcard_from")
	if from == "" {
		return gooCartErr("Please enter your name in the &quot;From&quot; field.")
	}
	msg := gxForm(form, "wc_gc_giftcard_message")
	if len([]rune(msg)) > 500 {
		return gooCartErr("The gift card message cannot be longer than 500 characters.")
	}
	deliver := gxForm(form, "wc_gc_giftcard_delivery")
	if deliver != "" {
		d, err := time.ParseInLocation("2006-01-02", deliver, s.gxLoc())
		if err != nil {
			return gooCartErr("Please enter a valid delivery date.")
		}
		now := time.Now().In(s.gxLoc())
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.gxLoc())
		if d.Before(today) {
			return gooCartErr("The delivery date cannot be in the past.")
		}
		if !d.After(today) {
			deliver = ""
		}
	}
	data["gift_card"] = map[string]any{"amount": gooRound(amount, s.gooDecimals()), "to": to, "to_name": gxForm(form, "wc_gc_giftcard_to_name"),
		"from": from, "message": msg, "deliver": deliver}
	if d := gxForm(form, "wc_gc_giftcard_design"); d != "" {
		allowed := false
		for _, k := range gcProductDesigns(p) {
			allowed = allowed || k == d
		}
		if !allowed {
			return gooCartErr("Please choose one of the available gift card designs.")
		}
		data["gift_card"].(map[string]any)["design"] = d
	}
	return nil
}

func gcItemCard(it *gooCartItem) map[string]any {
	m, _ := it.Data["gift_card"].(map[string]any)
	return m
}

// gcCartPrice sets the gift card's price to the chosen amount.
func gcCartPrice(s *Server, ctx context.Context, cart *gooCart, it *gooCartItem) {
	if !s.gooExtOn(gcKey) {
		return
	}
	if m := gcItemCard(it); m != nil {
		it.Price = gxNum(m["amount"])
	}
}

// gcItemData shows To / From / Message / Delivery Date under a gift card.
func gcItemData(s *Server, ctx context.Context, it *gooCartItem) []gooKV {
	if !s.gooExtOn(gcKey) {
		return nil
	}
	m := gcItemCard(it)
	if m == nil {
		return nil
	}
	to := gxStr(m["to"])
	if n := gxStr(m["to_name"]); n != "" {
		to = n + " <" + to + ">"
	}
	out := []gooKV{{"To", to}, {"From", gxStr(m["from"])}}
	if v := gxStr(m["message"]); v != "" {
		out = append(out, gooKV{"Message", v})
	}
	if v := gxStr(m["deliver"]); v != "" {
		if d, err := time.ParseInLocation("2006-01-02", v, s.gxLoc()); err == nil {
			out = append(out, gooKV{"Delivery Date", s.gxDate(d)})
		}
	}
	return out
}

// gcApplied is the session's gift card codes.
func gcApplied(sess *gooSession) []string {
	var out []string
	if sess.Data.Extra == nil {
		return nil
	}
	switch v := sess.Data.Extra[gcKey].(type) {
	case []any:
		for _, x := range v {
			if c := gxStr(x); c != "" {
				out = append(out, c)
			}
		}
	case []string:
		out = v
	}
	return out
}

func gcSetApplied(sess *gooSession, codes []string) {
	if sess.Data.Extra == nil {
		sess.Data.Extra = map[string]any{}
	}
	if len(codes) == 0 {
		delete(sess.Data.Extra, gcKey)
	} else {
		list := make([]any, len(codes))
		for i, c := range codes {
			list[i] = c
		}
		sess.Data.Extra[gcKey] = list
	}
	sess.touch()
}

// gcShippingEstimate is the shipping the cart will charge (cost and taxes of the chosen rate), worked
// out the way Calculate does after the fees (gift cards pay for shipping too).
func (s *Server) gcShippingEstimate(ctx context.Context, cart *gooCart) float64 {
	if !cart.NeedsShipping() {
		return 0
	}
	dest := cart.shippingDestination()
	if dest.Country == "" || !s.gooShipsTo(dest.Country) {
		return 0
	}
	cu := cart.sess.Data.Customer
	if s.gooOptBool("shipping_cost_requires_address") && !cu.CalculatedShip && cu.Billing["country"] == "" && cu.Shipping["country"] == "" {
		return 0
	}
	pkg := &gooPackage{Destination: dest, Coupons: cart.coupons, cart: cart}
	for _, it := range cart.Items {
		if s.gooNeedsShipping(ctx, it.product) {
			pkg.Items = append(pkg.Items, it)
			pkg.Contents += it.LineTotal
			pkg.ContentsTax += it.LineTax
			pkg.Subtotal += it.LineSubtotal
			if s.gooOptString("tax_display_cart") == "incl" {
				pkg.Subtotal += it.LineSubtotalTax
			}
		}
	}
	rates := s.gooPackageRates(ctx, pkg)
	if len(rates) == 0 {
		return 0
	}
	chosen := rates[0]
	for _, r := range rates {
		if r.ID == cart.sess.Data.ChosenShipping["0"] {
			chosen = r
		}
	}
	dp := s.gooDecimals()
	cost := gooRound(chosen.Cost, dp)
	if chosen.Taxable && s.gooTaxEnabled() {
		class := s.gooOptString("shipping_tax_class")
		if class == "inherit" {
			class = cart.inheritedShippingTaxClass()
		}
		loc := cart.taxLocation()
		if chosen.MethodID == "local_pickup" {
			loc = s.gooBaseLocation()
		}
		cost += gooRound(gooSumTaxes(gooCalcTax(cost, s.gooFindRates(ctx, class, loc, true), false)), dp)
	}
	return cost
}

// gcCartFees applies the session's gift cards as negative fees, capped at what is left to pay.
func gcCartFees(s *Server, ctx context.Context, cart *gooCart) {
	if !s.gooExtOn(gcKey) {
		return
	}
	codes := gcApplied(cart.sess)
	if len(codes) == 0 {
		return
	}
	remaining := cart.CartContentsTotal + gooSumTaxes(cart.Taxes)
	for _, f := range cart.Fees {
		remaining += f.Amount
		if f.Taxable {
			// approximate: the fee's taxes are added by Calculate after the hooks
			remaining += gooSumTaxes(gooCalcTax(f.Amount, s.gooFindRates(ctx, f.TaxClass, cart.TaxLocation, false), false))
		}
	}
	remaining += s.gcShippingEstimate(ctx, cart)
	dp := s.gooDecimals()
	var keep []string
	for _, code := range codes {
		g, err := s.gcByCode(ctx, code)
		if err != nil {
			cart.errors = append(cart.errors, fmt.Sprintf("Gift card %s has been removed from your cart because it no longer exists.", html.EscapeString(code)))
			continue
		}
		if err := s.gcUsable(g); err != nil {
			cart.errors = append(cart.errors, fmt.Sprintf("Gift card %s has been removed from your cart: %s", html.EscapeString(g.Code), html.EscapeString(err.Error())))
			continue
		}
		keep = append(keep, g.Code)
		amount := gooRound(min(g.Balance, max(remaining, 0)), dp)
		if amount <= 0 {
			continue
		}
		f := cart.AddFee(gcFeePref+strings.ToLower(strings.ReplaceAll(g.Code, "-", "")), "Gift card: "+g.Code, -amount, true, gcTaxClass)
		f.Meta = map[string]any{"gift_card_id": float64(g.ID), "gift_card_code": g.Code}
		remaining -= amount
	}
	if len(keep) != len(codes) {
		gcSetApplied(cart.sess, keep)
	}
}

// gcCartGiftCards lists the applied gift cards with the amount each pays.
func gcCartGiftCards(cart *gooCart) []*gooFee {
	var out []*gooFee
	for _, f := range cart.Fees {
		if strings.HasPrefix(f.ID, gcFeePref) {
			out = append(out, f)
		}
	}
	return out
}

// Orders -----------------------------------------------------------------------------------------------

// gcOrderFees are the order's gift card fee lines: card code → amount paid.
func gcOrderFees(o *gooOrder) map[string]float64 {
	out := map[string]float64{}
	for _, it := range o.ItemsOf("fee") {
		id := it.MetaString("_fee_id")
		if !strings.HasPrefix(id, gcFeePref) {
			continue
		}
		code := gcNormalize(strings.TrimPrefix(id, gcFeePref))
		out[code] += -(it.Total + it.TotalTax)
	}
	return out
}

// gcReserve debits the gift cards a checkout uses before the order is saved, each in one conditional
// UPDATE: a card whose balance no longer covers its share (another checkout spent it meanwhile) fails
// the checkout instead of discounting two orders with one balance.
func gcReserve(s *Server, ctx context.Context, _ *gooCart, o *gooOrder) (func(), error) {
	if !s.gooExtOn(gcKey) {
		return nil, nil
	}
	dp := s.gooDecimals()
	type debit struct {
		id     int64
		amount float64
	}
	var done []debit
	release := func() {
		for _, d := range done {
			_, _ = s.store.Pool.Exec(context.WithoutCancel(ctx), `UPDATE goo_gift_cards SET balance = balance + $2::numeric,
				status = CASE WHEN status = 'redeemed' THEN 'active' ELSE status END WHERE id = $1`, d.id, d.amount)
		}
	}
	for code, amount := range gcOrderFees(o) {
		amount = gooRound(amount, dp)
		if amount <= 0 {
			continue
		}
		var id int64
		err := s.store.Pool.QueryRow(ctx, `UPDATE goo_gift_cards SET balance = balance - $2::numeric,
			status = CASE WHEN balance - $2::numeric <= 0 THEN 'redeemed' ELSE status END
			WHERE code = $1 AND status = 'active' AND balance >= $2::numeric AND (expires IS NULL OR expires >= $3::date) RETURNING id`, code, amount, time.Now().In(s.gxLoc()).Format("2006-01-02")).Scan(&id)
		if err != nil {
			release()
			return nil, fmt.Errorf("The balance of gift card %s has changed. Please review your order.", html.EscapeString(code))
		}
		done = append(done, debit{id, amount})
	}
	if len(done) > 0 {
		o.Meta["_gc_debited"] = true
	}
	return release, nil
}

// gcOrderCreated records the debit of the gift cards used by the order (checkout reserved it already;
// an order made elsewhere is debited here).
func (s *Server) gcOrderCreated(ctx context.Context, o *gooOrder) {
	if !s.gooExtOn(gcKey) {
		return
	}
	reserved := o.Meta["_gc_debited"] == true
	for code, amount := range gcOrderFees(o) {
		g, err := s.gcByCode(ctx, code)
		if err != nil || amount <= 0 {
			continue
		}
		if reserved {
			_ = s.gcLog(ctx, g.ID, "used", gooRound(amount, s.gooDecimals()), o.ID, o.CustomerID, "")
		} else {
			_ = s.gcAdjust(ctx, g, -amount, "used", o.ID, o.CustomerID, "")
		}
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Gift card %s debited: %s.", g.Code, s.gooPricePlain(amount, o.Currency)), false, "")
	}
}

// gcOrderStatus issues the cards of a paid order and credits back the cards that paid for an order
// that was cancelled or refunded (what partial refunds have not credited back already). A failed order
// keeps its debit: it can still be paid from Pay for order with the gift card discount, so crediting
// the card back there let one balance pay for the order and then be spent again.
func (s *Server) gcOrderStatus(ctx context.Context, ch *gooStatusChange) {
	if !s.gooExtOn(gcKey) {
		return
	}
	o := ch.Order
	// Paid and on-hold orders were debited again in gooSetStatusLocked, before the status was saved.
	if !containsStr([]string{"cancelled", "refunded", "trash", "processing", "completed", "on-hold"}, ch.To) {
		if err := s.gcRedebit(ctx, o); err != nil {
			_ = s.gooAddOrderNote(ctx, o, err.Error(), false, "")
		}
	}
	switch ch.To {
	case "processing", "completed":
		s.gcEnableOrderCards(ctx, o)
		s.gcIssue(ctx, o)
	case "cancelled", "refunded":
		s.gcDisableOrderCards(ctx, o, nil, 0, "Order "+gooStatusLabel(ch.To))
		outstanding, ids := s.gcOutstandingRefx(ctx, o.ID)
		type due struct {
			id  int64
			amt float64
		}
		var list []due
		for _, id := range ids {
			list = append(list, due{id, outstanding[id]})
		}
		credited := gcStatusCredit(o)
		for _, d := range list {
			if g, err := s.gcGet(ctx, d.id); err == nil {
				if s.gcAdjust(ctx, g, d.amt, "refunded", o.ID, o.CustomerID, "Order "+gooStatusLabel(ch.To)) == nil {
					credited[itoa64(d.id)] = gooRound(gxNum(credited[itoa64(d.id)])+d.amt, s.gooDecimals())
				}
				_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Gift card %s credited: %s.", g.Code, s.gooPricePlain(d.amt, o.Currency)), false, "")
			}
		}
		if len(list) > 0 {
			s.gcSetStatusCredit(ctx, o, credited)
		}
	}
}

// gcStatusCreditKey is the order meta holding what cancelling or refunding the order credited back to
// each card (card id → amount), so paying the order again later debits it again.
const gcStatusCreditKey = "_gc_status_credit"

func gcStatusCredit(o *gooOrder) map[string]any {
	out := map[string]any{}
	if m, ok := o.Meta[gcStatusCreditKey].(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// gcSetStatusCredit stores (or with an empty map removes) the order's status credit without saving
// the rest of the order.
func (s *Server) gcSetStatusCredit(ctx context.Context, o *gooOrder, m map[string]any) {
	if o.Meta == nil {
		o.Meta = map[string]any{}
	}
	if len(m) == 0 {
		delete(o.Meta, gcStatusCreditKey)
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_orders SET meta = meta - $2::text WHERE id = $1`, o.ID, gcStatusCreditKey)
		return
	}
	o.Meta[gcStatusCreditKey] = m
	b, _ := json.Marshal(m)
	_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_orders SET meta = jsonb_set(meta, ARRAY[$2::text], $3::jsonb) WHERE id = $1`, o.ID, gcStatusCreditKey, string(b))
}

// gcRedebit takes back what a cancellation or refund credited to the cards when the order comes back
// (paid late, reactivated by the admin): the order keeps its gift card discount, so without this one
// balance would pay twice. Each debit is a conditional UPDATE like gcReserve; a card that no longer
// covers it keeps its credit entry (retried later) and is reported in the returned error.
func (s *Server) gcRedebit(ctx context.Context, o *gooOrder) error {
	credited := gcStatusCredit(o)
	if len(credited) == 0 {
		return nil
	}
	var short []string
	dp := s.gooDecimals()
	for key, v := range credited {
		id, _ := strconv.ParseInt(key, 10, 64)
		amt := gooRound(gxNum(v), dp)
		if id == 0 || amt <= 0 {
			delete(credited, key)
			continue
		}
		var code string
		err := s.store.Pool.QueryRow(ctx, `UPDATE goo_gift_cards SET balance = balance - $2::numeric,
			status = CASE WHEN balance - $2::numeric <= 0 THEN 'redeemed' ELSE status END
			WHERE id = $1 AND status = 'active' AND balance >= $2::numeric RETURNING code`, id, amt).Scan(&code)
		if err != nil {
			if g, gerr := s.gcGet(ctx, id); gerr == nil {
				code = g.Code
			}
			short = append(short, fmt.Sprintf("Gift card %s could not be debited again (%s): its balance no longer covers it. Please review the order.",
				code, s.gooPricePlain(amt, o.Currency)))
			continue
		}
		_ = s.gcLog(ctx, id, "used", amt, o.ID, o.CustomerID, "Order reactivated")
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Gift card %s debited again: %s.", code, s.gooPricePlain(amt, o.Currency)), false, "")
		delete(credited, key)
	}
	s.gcSetStatusCredit(ctx, o, credited)
	if len(short) > 0 {
		return errors.New(strings.Join(short, " "))
	}
	return nil
}

// gcIssue creates the order's gift cards (once per unit) and sends those due today.
func (s *Server) gcIssue(ctx context.Context, o *gooOrder) {
	for _, it := range o.ItemsOf("line_item") {
		m, ok := it.Meta["_goo_gift_card"].(map[string]any)
		if !ok {
			continue
		}
		var have int
		_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM goo_gift_cards WHERE order_item_id = $1`, it.ID).Scan(&have)
		var expires *time.Time
		if p, err := s.gooGetProduct(ctx, it.ProductID); err == nil {
			if days := int(gxNum(p.Meta["_gift_card_expiration_days"])); days > 0 {
				e := time.Now().In(s.gxLoc()).AddDate(0, 0, days)
				expires = &e
			}
		}
		var deliver *time.Time
		if d, err := time.ParseInLocation("2006-01-02", gxStr(m["deliver"]), s.gxLoc()); err == nil {
			deliver = &d
		}
		for i := have; i < it.Qty(); i++ {
			g := &gcCard{Initial: gxNum(m["amount"]), Currency: o.Currency, OrderID: o.ID, OrderItemID: it.ID, Recipient: gxStr(m["to"]),
				RecipientName: gxStr(m["to_name"]), Sender: gxStr(m["from"]), SenderEmail: o.Billing["email"], Message: gxStr(m["message"]),
				DeliverDate: deliver, Expires: expires, Design: gxStr(m["design"])}
			if err := s.gcCreate(ctx, g, o.CustomerID, fmt.Sprintf("Order #%d", o.ID)); err != nil {
				continue
			}
			_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Gift card %s issued to %s.", g.Code, g.Recipient), false, "")
			if deliver == nil || !deliver.After(time.Now()) {
				s.gcDeliver(ctx, g)
			}
		}
	}
}

// gcDeliver e-mails a card to its recipient.
func (s *Server) gcDeliver(ctx context.Context, g *gcCard) {
	amount := s.gooPricePlain(g.Initial, g.Currency)
	var b strings.Builder
	hello := "Hi there,"
	if g.RecipientName != "" {
		hello = "Hi " + html.EscapeString(g.RecipientName) + ","
	}
	fmt.Fprintf(&b, `<p>%s</p><p>%s sent you a gift card worth <strong>%s</strong>!</p>`, hello, html.EscapeString(g.Sender), html.EscapeString(amount))
	if g.Message != "" {
		b.WriteString(`<blockquote style="border-left:3px solid #7f54b3;margin:16px 0;padding:8px 16px;font-style:italic">` + strings.ReplaceAll(html.EscapeString(g.Message), "\n", "<br>") + `</blockquote>`)
	}
	key := s.gcCardDesign(ctx, g)
	d := gcDesignFor(key)
	img := ""
	if u := s.gcDesignImageURL(ctx, key); u != "" {
		img = `<img src="` + html.EscapeString(u) + `" alt="" style="max-width:100%;height:auto;border-radius:6px;margin-bottom:12px">`
	}
	fmt.Fprintf(&b, `<div style="border:2px dashed %s;background:%s;border-radius:8px;padding:20px;text-align:center;margin:20px 0">%s<div style="font-size:13px;color:#888;text-transform:uppercase;letter-spacing:1px">%s gift card code</div>`+
		`<div style="font-size:24px;font-weight:bold;letter-spacing:2px;color:#333;margin:8px 0">%s</div><div style="font-size:18px;color:%s">%s</div>`, d.Accent, d.Bg, img, html.EscapeString(d.Label), g.Code, d.Accent, html.EscapeString(amount))
	if g.Expires != nil {
		fmt.Fprintf(&b, `<div style="font-size:12px;color:#888;margin-top:6px">Expires on %s</div>`, html.EscapeString(s.gxDate(*g.Expires)))
	}
	b.WriteString(`</div>`)
	fmt.Fprintf(&b, `<p><a href="%s">Print your gift card</a> (it is also attached to this e-mail as a PDF)</p>`, html.EscapeString(s.gcPrintURL(ctx, g)))
	fmt.Fprintf(&b, `<p>To use your gift card, enter the code in the &quot;Have a gift card?&quot; field of the cart or checkout at <a href="%s">%s</a>.</p>`,
		html.EscapeString(s.gooShopURL(ctx)), html.EscapeString(s.settings.String("blogname")))
	if s.gxSendEmail(ctx, "gift_card_received", []string{g.Recipient}, map[string]string{"giftcard_amount": amount, "sender": g.Sender, "giftcard_code": g.Code}, b.String(),
		mail.Attachment{Name: "gift-card-" + g.Code + ".pdf", ContentType: "application/pdf", Data: s.gcPDF(ctx, g)}) {
		g.Delivered = true
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_gift_cards SET delivered = true WHERE id = $1`, g.ID)
	}
}

// gcDaily sends the cards whose delivery date came.
func (s *Server) gcDaily(ctx context.Context) {
	if !s.gooExtOn(gcKey) {
		return
	}
	now := time.Now().In(s.gxLoc())
	for _, g := range s.gcQuery(ctx, `NOT delivered AND status <> 'disabled' AND deliver_date IS NOT NULL AND deliver_date <= $1`, now.Format("2006-01-02")) {
		s.gcDeliver(ctx, g)
	}
	s.gcExpiryReminders(ctx, now)
}

// gcExpiryReminders e-mails the recipient of a delivered card with a balance a week before it expires
// (once per card: the reminder is recorded in the activity log).
func (s *Server) gcExpiryReminders(ctx context.Context, now time.Time) {
	today := now.Format("2006-01-02")
	for _, g := range s.gcQuery(ctx, `delivered AND status = 'active' AND balance > 0 AND expires IS NOT NULL AND expires >= $1::date AND expires <= $1::date + 7
		AND NOT EXISTS (SELECT 1 FROM goo_gift_card_activity a WHERE a.card_id = goo_gift_cards.id AND a.type = 'reminder')`, today) {
		balance := s.gooPricePlain(g.Balance, g.Currency)
		body := fmt.Sprintf(`<p>Hi there,</p><p>Your gift card <strong>%s</strong> still has a balance of <strong>%s</strong> and expires on %s.</p>`+
			`<p>Enter the code in the &quot;Have a gift card?&quot; field of the cart or checkout at <a href="%s">%s</a> before then.</p>`,
			html.EscapeString(g.Code), html.EscapeString(balance), html.EscapeString(s.gxDate(*g.Expires)),
			html.EscapeString(s.gooShopURL(ctx)), html.EscapeString(s.settings.String("blogname")))
		if s.gxSendEmail(ctx, "gift_card_expiring", []string{g.Recipient}, map[string]string{"giftcard_amount": balance, "expires": s.gxDate(*g.Expires), "giftcard_code": g.Code}, body) {
			_ = s.gcLog(ctx, g.ID, "reminder", 0, 0, 0, "Expiry reminder sent")
		}
	}
}

// gcDisableOrderCards disables the gift cards an order sold (a refunded or cancelled order must not leave
// spendable codes behind): all of them, or with limit > 0 that many of the item's cards (items != nil
// restricts to those order items). A card that was already partly spent gets an order note so the
// merchant can review it.
func (s *Server) gcDisableOrderCards(ctx context.Context, o *gooOrder, items map[int64]bool, limit int, reason string) {
	cards := s.gcQuery(ctx, `order_id = $1 AND status <> 'disabled' ORDER BY id DESC`, o.ID)
	n := 0
	for _, g := range cards {
		if items != nil && !items[g.OrderItemID] {
			continue
		}
		if limit > 0 && n >= limit {
			break
		}
		if _, err := s.store.Pool.Exec(ctx, `UPDATE goo_gift_cards SET status = 'disabled' WHERE id = $1`, g.ID); err != nil {
			continue
		}
		n++
		_ = s.gcLog(ctx, g.ID, "manual", 0, o.ID, 0, "Disabled: "+strings.ToLower(reason))
		note := fmt.Sprintf("Gift card %s disabled (%s).", g.Code, strings.ToLower(reason))
		if g.Balance < g.Initial {
			note += fmt.Sprintf(" It was already partly spent (balance %s of %s): please review.", s.gooPricePlain(g.Balance, g.Currency), s.gooPricePlain(g.Initial, g.Currency))
		}
		_ = s.gooAddOrderNote(ctx, o, note, false, "")
	}
}

// gcEnableOrderCards re-enables the cards an order sold that a cancellation or refund disabled, when the
// order is paid / reactivated again (cards a merchant disabled by hand stay disabled).
func (s *Server) gcEnableOrderCards(ctx context.Context, o *gooOrder) {
	for _, g := range s.gcQuery(ctx, `order_id = $1 AND status = 'disabled' AND (SELECT note FROM goo_gift_card_activity a WHERE a.card_id = goo_gift_cards.id ORDER BY id DESC LIMIT 1) LIKE 'Disabled: order %'`, o.ID) {
		g.Status = "active"
		if s.gcAdjust(ctx, g, 0, "manual", o.ID, 0, "Enabled: order reactivated") == nil {
			_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Gift card %s enabled again.", g.Code), false, "")
		}
	}
}

// gcRefundDisable disables the cards of gift card items a refund returns (quantity or the full line total);
// a refund of the whole order is handled by the status change.
func (s *Server) gcRefundDisable(ctx context.Context, ev *gooRefundEvent) {
	if !s.gooExtOn(gcKey) || ev == nil || ev.Order == nil || ev.Refund == nil {
		return
	}
	o := ev.Order
	for _, ri := range ev.Refund.ItemsOf("line_item") {
		src := gooRefundSource(o, ri)
		var it *gooOrderItem
		for _, x := range o.Items {
			if x.ID == src {
				it = x
			}
		}
		if it == nil || it.Meta["_goo_gift_card"] == nil {
			continue
		}
		n := int(-ri.Quantity)
		if n <= 0 {
			if -ri.Total < it.Total-0.0001 {
				continue
			}
			n = it.Qty()
		}
		s.gcDisableOrderCards(ctx, o, map[int64]bool{it.ID: true}, n, fmt.Sprintf("Refund #%d of the order item", ev.Refund.ID))
	}
}

// gcOutstandingRefx is what each card paid for the order and has not been credited back yet: card id → amount.
func (s *Server) gcOutstandingRefx(ctx context.Context, orderID int64) (map[int64]float64, []int64) {
	rows, err := s.store.Pool.Query(ctx, `SELECT card_id, sum(CASE WHEN type = 'used' THEN amount ELSE -amount END)::float8
		FROM goo_gift_card_activity WHERE order_id = $1 AND type IN ('used', 'refunded') GROUP BY card_id ORDER BY card_id`, orderID)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	out := map[int64]float64{}
	var ids []int64
	for rows.Next() {
		var id int64
		var amt float64
		if rows.Scan(&id, &amt) == nil && amt > 0.0001 {
			out[id] = amt
			ids = append(ids, id)
		}
	}
	return out, ids
}

// gcRefundCreditProp credits the gift cards of a partially refunded order in proportion to the refund:
// refunding a share of what the order charged restores the same share of what each card paid, up to
// what the card has not been credited back yet. A full refund credits the rest (gcOrderStatus).
func (s *Server) gcRefundCreditProp(ctx context.Context, ev *gooRefundEvent) {
	if !s.gooExtOn(gcKey) || ev == nil || ev.Order == nil || ev.Refund == nil {
		return
	}
	o := ev.Order
	refund := -ev.Refund.Total
	if refund <= 0 || o.Total <= 0 {
		return
	}
	paid := gcOrderFees(o)
	if len(paid) == 0 {
		return
	}
	share := min(refund/o.Total, 1)
	outstanding, ids := s.gcOutstandingRefx(ctx, o.ID)
	dp := s.gooDecimals()
	for _, id := range ids {
		g, err := s.gcGet(ctx, id)
		if err != nil {
			continue
		}
		amt := gooRound(min(paid[g.Code]*share, outstanding[id]), dp)
		if amt <= 0 {
			continue
		}
		_ = s.gcAdjust(ctx, g, amt, "refunded", o.ID, ev.Refund.RefundedBy, fmt.Sprintf("Partial refund #%d", ev.Refund.ID))
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Gift card %s credited: %s.", g.Code, s.gooPricePlain(amt, o.Currency)), false, "")
	}
}
