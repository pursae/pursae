package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"slices"
	"sort"
	"strings"
	"time"
	"wpgo/internal/mail"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// Bookings (WooCommerce Bookings): a "booking" product type whose customers pick a date (and a time
// for hour / minute blocks), a duration and persons; availability rules, a capacity per block, buffers
// and cost rules decide what can be booked and for how much. A booking is held "in-cart" for an hour,
// becomes unpaid / pending-confirmation with the order, paid with the payment and complete after it
// ended. Resources (goo_ext_bookings_resources.go) add shared capacity, rules and costs.

const (
	bkKey  = "bookings"
	bkType = "booking"
)

var bkStatuses = []gooOpt{
	{"unpaid", "Unpaid"}, {"pending-confirmation", "Pending confirmation"}, {"confirmed", "Confirmed"}, {"paid", "Paid & Confirmed"},
	{"complete", "Complete"}, {"cancelled", "Cancelled"}, {"in-cart", "In cart"},
}

func bkStatusLabel(st string) string {
	for _, o := range bkStatuses {
		if o.Value == st {
			return o.Label
		}
	}
	return st
}

// bkHolding are the statuses that take a place in a block.
var bkHolding = []string{"unpaid", "pending-confirmation", "confirmed", "paid", "complete", "in-cart"}

func init() {
	gooRegisterExtension(&gooExtension{
		Key: bkKey, Name: "Bookings", Icon: "dashicons-calendar-alt", Category: "Store management",
		Description: "Sell appointments, reservations and rentals: bookable products with availability rules, durations, persons, costs, confirmations and reminders.",
		Fields: []gooField{
			{Type: "title", Title: "Booking settings"},
			{ID: "in_cart_minutes", Title: "Hold bookings in the cart", Type: "number", Default: 60, Min: "5", Desc: "Minutes a booking added to the cart keeps its place before it expires."},
			{ID: "reminder_days", Title: "Reminder", Type: "number", Default: 1, Min: "0", Desc: "Days before the booking the reminder e-mail is sent (0 turns reminders off)."},
			{ID: "calendar_first_day", Title: "Calendar starts on", Type: "select", Default: "1", Options: []gooOpt{{"1", "Monday"}, {"0", "Sunday"}}},
			{ID: "show_remaining", Title: "Show remaining places", Type: "checkbox", Default: true, Desc: "Show how many places are left in a time slot."},
			{ID: "auto_refund", Title: "Refund cancelled bookings", Type: "checkbox", Default: true, Desc: "When a paid booking is cancelled, refund its order line automatically if the payment method supports refunds. Otherwise the order gets a \"refund required\" note."},
			{Type: "sectionend"},
		},
		Pages: []gooExtMenuItem{{Page: "bookings", Title: "Bookings"}},
	})
	gooRegisterProductType(&gooProductTypeDef{
		Type: bkType, Label: "Bookable product", Module: bkKey, Virtual: true,
		Purchasable: func(s *Server, ctx context.Context, p *gooProduct) bool {
			return s.gooExtOn(bkKey) && (p.Status == "publish" || p.Status == "private")
		},
		NeedsShipping: func(s *Server, ctx context.Context, p *gooProduct) bool { return s.gooExtOn(bkKey) && !p.Virtual },
		AddToCartText: func(s *Server, p *gooProduct, single bool) string {
			if single {
				return "Book now"
			}
			return "Read more"
		},
	})
	gooAddToCartHooks = append(gooAddToCartHooks, bkAddToCart)
	gooCartItemPriceHooks = append(gooCartItemPriceHooks, bkCartPrice)
	gooCartItemDataHooks = append(gooCartItemDataHooks, bkItemData)
	gooCheckoutHooks = append(gooCheckoutHooks, bkCheckout)
	gooOn("order_created", 10, func(s *Server, ctx context.Context, arg any) { s.bkOrderCreated(ctx, arg.(*gooOrder)) })
	gooOn("order_status_changed", 10, func(s *Server, ctx context.Context, arg any) { s.bkOrderStatus(ctx, arg.(*gooStatusChange)) })
	gooOn("daily", 10, func(s *Server, ctx context.Context, _ any) { s.bkDaily(ctx) })
	gooOn("hourly", 10, func(s *Server, ctx context.Context, _ any) { s.bkHourly(ctx) })
	gooRegisterGateway(bkGateway{})
	for _, e := range []*gooEmailDef{
		{ID: "new_booking", Title: "New booking", Module: bkKey, Description: "New booking emails are sent to the admin for a new booking, and when a booking needs confirmation.",
			Subject: "[{site_title}] New booking #{booking_id}", Heading: "New booking"},
		{ID: "booking_confirmed", Title: "Booking confirmed", Module: bkKey, Customer: true, Description: "Booking confirmed emails are sent when the status of a booking goes to confirmed.",
			Subject: "Your {site_title} booking has been confirmed", Heading: "Booking confirmed"},
		{ID: "booking_reminder", Title: "Booking reminder", Module: bkKey, Customer: true, Description: "Booking reminders are sent to the customer to remind them of an upcoming booking.",
			Subject: "Booking reminder: your booking of {product_title} is coming up", Heading: "Booking reminder"},
		{ID: "booking_cancelled", Title: "Booking cancelled", Module: bkKey, Customer: true, Description: "Booking cancelled emails are sent when the status of a booking goes to cancelled.",
			Subject: "Your {site_title} booking has been cancelled", Heading: "Booking cancelled"},
		{ID: "admin_booking_cancelled", Title: "Admin booking cancelled", Module: bkKey, Description: "Booking cancelled emails are sent to the admin when the status of a booking goes to cancelled.",
			Subject: "[{site_title}] Booking #{booking_id} cancelled", Heading: "Booking cancelled"},
	} {
		gooRegisterEmail(e)
	}
}

// Product settings ------------------------------------------------------------------------------------

type bkRule struct {
	Type     string `json:"type"` // custom, months, weeks, days, time, time:1 … time:7
	From     string `json:"from"`
	To       string `json:"to"`
	Bookable bool   `json:"bookable"`
	Priority int    `json:"priority"`
}

type bkCostRule struct {
	Type     string  `json:"type"` // custom, months, weeks, days, time, time:N, persons, blocks
	From     string  `json:"from"`
	To       string  `json:"to"`
	BaseMod  string  `json:"base_modifier"` // plus, minus, times, divide, equals
	BaseCost float64 `json:"base_cost"`
	Mod      string  `json:"modifier"`
	Cost     float64 `json:"cost"`
}

type bkPersonType struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	BaseCost    float64 `json:"cost"`
	BlockCost   float64 `json:"block_cost"`
	Min         int     `json:"min"`
	Max         int     `json:"max"`
}

// bkConf is a bookable product's settings (the _wc_booking_* meta).
type bkConf struct {
	DurationType     string // fixed, customer
	Duration         int
	Unit             string // minute, hour, day, month
	MinDuration      int
	MaxDuration      int
	CalendarDisplay  string // "", always_visible
	RequiresConfirm  bool
	CanCancel        bool
	CancelLimit      int
	CancelUnit       string // month, day, hour, minute
	MaxPerBlock      int
	MinDate          int
	MinDateUnit      string
	MaxDate          int
	MaxDateUnit      string
	Buffer           int
	DefaultAvailable bool
	FirstBlockTime   string
	AllowOvernight   bool // hour/minute blocks may run past midnight
	Rules            []bkRule
	BaseCost         float64
	BlockCost        float64
	DisplayCost      float64
	CostRules        []bkCostRule
	HasPersons       bool
	MinPersons       int
	MaxPersons       int
	PersonCostMult   bool
	PersonQtyMult    bool
	HasPersonTypes   bool
	PersonTypes      []bkPersonType
	HasResources     bool
	ResourceLabel    string
	ResourceAssign   string // customer, automatic
	Resources        []gxbProdRes
}

func bkMeta(p *gooProduct, k string) any { return p.Meta["_wc_booking_"+k] }

// bkConfOf reads a product's booking settings with WooCommerce Bookings' defaults.
func bkConfOf(p *gooProduct) *bkConf {
	str := func(k, def string) string { return defaultStr(gxStr(bkMeta(p, k)), def) }
	num := func(k string, def int) int {
		if v, ok := p.Meta["_wc_booking_"+k]; ok && gxStr(v) != "" {
			return int(gxNum(v))
		}
		return def
	}
	c := &bkConf{
		DurationType: str("duration_type", "fixed"), Duration: max(num("duration", 1), 1), Unit: str("duration_unit", "day"),
		MinDuration: max(num("min_duration", 1), 1), MaxDuration: num("max_duration", 1),
		CalendarDisplay: gxStr(bkMeta(p, "calendar_display_mode")), RequiresConfirm: gxBool(bkMeta(p, "requires_confirmation")),
		CanCancel: gxBool(bkMeta(p, "user_can_cancel")), CancelLimit: num("cancel_limit", 1), CancelUnit: str("cancel_limit_unit", "month"),
		MaxPerBlock: max(num("qty", 1), 1), MinDate: num("min_date", 0), MinDateUnit: str("min_date_unit", "day"),
		MaxDate: num("max_date", 12), MaxDateUnit: str("max_date_unit", "month"), Buffer: num("buffer_period", 0),
		DefaultAvailable: str("default_date_availability", "available") == "available", FirstBlockTime: gxStr(bkMeta(p, "first_block_time")), AllowOvernight: gxBool(bkMeta(p, "allow_overnight")),
		BaseCost: gxNum(bkMeta(p, "cost")), BlockCost: gxNum(bkMeta(p, "block_cost")), DisplayCost: gxNum(p.Meta["_wc_display_cost"]),
		HasPersons: gxBool(bkMeta(p, "has_persons")), MinPersons: max(num("min_persons_group", 1), 1), MaxPersons: num("max_persons_group", 0),
		PersonCostMult: gxBool(bkMeta(p, "person_cost_multiplier")), PersonQtyMult: gxBool(bkMeta(p, "person_qty_multiplier")),
		HasPersonTypes: gxBool(bkMeta(p, "has_person_types")),
	}
	if c.MaxDuration < c.MinDuration {
		c.MaxDuration = c.MinDuration
	}
	for _, m := range gxList(bkMeta(p, "availability")) {
		c.Rules = append(c.Rules, bkRule{Type: gxStr(m["type"]), From: gxStr(m["from"]), To: gxStr(m["to"]), Bookable: gxBool(m["bookable"]), Priority: int(gxNum(m["priority"]))})
	}
	for _, m := range gxList(bkMeta(p, "pricing")) {
		c.CostRules = append(c.CostRules, bkCostRule{Type: gxStr(m["type"]), From: gxStr(m["from"]), To: gxStr(m["to"]), BaseMod: gxStr(m["base_modifier"]),
			BaseCost: gxNum(m["base_cost"]), Mod: gxStr(m["modifier"]), Cost: gxNum(m["cost"])})
	}
	for _, m := range gxList(p.Meta["_wc_booking_person_types"]) {
		c.PersonTypes = append(c.PersonTypes, bkPersonType{ID: gxStr(m["id"]), Name: gxStr(m["name"]), Description: gxStr(m["description"]),
			BaseCost: gxNum(m["cost"]), BlockCost: gxNum(m["block_cost"]), Min: int(gxNum(m["min"])), Max: int(gxNum(m["max"]))})
	}
	if !c.HasPersonTypes || len(c.PersonTypes) == 0 {
		// One implicit person type, "Persons", without a cost of its own.
		c.PersonTypes = []bkPersonType{{ID: "persons", Name: "Persons"}}
	}
	gxbConfResources(p, c)
	return c
}

// blockMinutes is a block's length for minute / hour units.
func (c *bkConf) blockMinutes() int {
	switch c.Unit {
	case "hour":
		return c.Duration * 60
	case "minute":
		return c.Duration
	}
	return 0
}

// timed reports whether bookings have a time (hour / minute units).
func (c *bkConf) timed() bool { return c.Unit == "hour" || c.Unit == "minute" }

// blockEnd is the end of the block that starts at t.
func (c *bkConf) blockEnd(t time.Time) time.Time {
	switch c.Unit {
	case "month":
		return t.AddDate(0, c.Duration, 0)
	case "day":
		return t.AddDate(0, 0, c.Duration)
	}
	return t.Add(time.Duration(c.blockMinutes()) * time.Minute)
}

// unitLabel is "2 hours", "1 day".
func bkUnitLabel(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func bkAddUnits(t time.Time, n int, unit string) time.Time {
	switch unit {
	case "month":
		return t.AddDate(0, n, 0)
	case "week":
		return t.AddDate(0, 0, 7*n)
	case "day":
		return t.AddDate(0, 0, n)
	case "hour":
		return t.Add(time.Duration(n) * time.Hour)
	case "minute":
		return t.Add(time.Duration(n) * time.Minute)
	}
	return t
}

// Rules -----------------------------------------------------------------------------------------------

func bkMinutesOf(hm string) int {
	h, m, ok := strings.Cut(strings.TrimSpace(hm), ":")
	if !ok {
		return -1
	}
	return gxAtoi(h)*60 + gxAtoi(m)
}

// bkInRange checks v in [from, to] with wrap-around (months 11–2, days 6–1).
func bkInRange(v, from, to int) bool {
	if from <= to {
		return v >= from && v <= to
	}
	return v >= from || v <= to
}

func bkWeekday(t time.Time) int { // Monday 1 … Sunday 7
	d := int(t.Weekday())
	if d == 0 {
		return 7
	}
	return d
}

// bkDateMatch is a date-type rule (custom, months, weeks, days) against a day.
func bkDateMatch(typ, from, to string, t time.Time) (matched, isDateRule bool) {
	switch typ {
	case "custom":
		d := t.Format("2006-01-02")
		return d >= from && d <= to, true
	case "months":
		return bkInRange(int(t.Month()), gxAtoi(from), gxAtoi(to)), true
	case "weeks":
		_, w := t.ISOWeek()
		return bkInRange(w, gxAtoi(from), gxAtoi(to)), true
	case "days":
		return bkInRange(bkWeekday(t), gxAtoi(from), gxAtoi(to)), true
	}
	return false, false
}

// bkTimeRule reports whether a time rule (time, time:N) concerns the block [s, e) and how: inside
// the rule's hours (for "bookable" rules) or overlapping them (for "not bookable" rules).
func bkTimeMatch(r bkRule, s, e time.Time) bool {
	if strings.HasPrefix(r.Type, "time:") {
		if gxAtoi(strings.TrimPrefix(r.Type, "time:")) != bkWeekday(s) {
			return false
		}
	} else if r.Type != "time" {
		return false
	}
	from, to := bkMinutesOf(r.From), bkMinutesOf(r.To)
	if from < 0 || to < 0 {
		return false
	}
	if to == 0 {
		to = 24 * 60
	}
	bs := s.Hour()*60 + s.Minute()
	be := bs + int(e.Sub(s).Minutes())
	if r.Bookable {
		return bs >= from && be <= to
	}
	return bs < to && be > from
}

// bookable reports whether the block [s, e) is bookable by the rules: the most important matching
// rule decides (lowest priority number; within a priority, the rule lower in the table).
func (c *bkConf) bookable(s, e time.Time) bool {
	if v, ok := gxbRulesDecide(c.Rules, c.timed(), s, e); ok {
		return v
	}
	return c.DefaultAvailable
}

// Costs -----------------------------------------------------------------------------------------------

func bkApplyMod(v float64, mod string, amount float64) float64 {
	switch mod {
	case "minus":
		return v - amount
	case "times":
		return v * amount
	case "divide":
		if amount != 0 {
			return v / amount
		}
		return v
	case "equals":
		return amount
	}
	return v + amount
}

// bkCostRuleMatch matches a cost rule against a block.
func bkCostRuleMatch(r bkCostRule, s, e time.Time, persons, blocks int, timed bool) bool {
	if m, isDate := bkDateMatch(r.Type, r.From, r.To, s); isDate {
		return m
	}
	switch {
	case r.Type == "persons":
		return bkInRange(persons, gxAtoi(r.From), max(gxAtoi(r.To), gxAtoi(r.From)))
	case r.Type == "blocks":
		return bkInRange(blocks, gxAtoi(r.From), max(gxAtoi(r.To), gxAtoi(r.From)))
	case r.Type == "time" || strings.HasPrefix(r.Type, "time:"):
		if !timed {
			return false
		}
		if strings.HasPrefix(r.Type, "time:") && gxAtoi(strings.TrimPrefix(r.Type, "time:")) != bkWeekday(s) {
			return false
		}
		bs := s.Hour()*60 + s.Minute()
		from, to := bkMinutesOf(r.From), bkMinutesOf(r.To)
		if to <= 0 {
			to = 24 * 60
		}
		return bs >= from && bs < to
	}
	return false
}

// gooBkCostWith is WC_Bookings_Cost_Calculation::calculate_booking_cost(): the base cost once, the
// block cost for every block, person type costs added (or every cost multiplied by the persons), the
// cost rules applied to each block they match. A resource's base and block costs are added to the
// product's.
func gooBkCostWith(c *bkConf, start time.Time, blocks int, persons map[string]int, resource int64) float64 {
	rb, rk := c.resourceCosts(resource)
	base, block := c.BaseCost+rb, c.BlockCost+rk
	total := 0
	for _, pt := range c.PersonTypes {
		n := bkClampPersons(persons[pt.ID])
		total = bkClampPersons(total + n)
		base += pt.BaseCost * float64(n)
		block += pt.BlockCost * float64(n)
	}
	if c.HasPersons && c.PersonCostMult && total > 0 {
		base *= float64(total)
		block *= float64(total)
	}
	baseApplied := map[int]bool{}
	sum := 0.0
	t := start
	for i := 0; i < blocks; i++ {
		e := c.blockEnd(t)
		bc := block
		for ri, r := range c.CostRules {
			if !bkCostRuleMatch(r, t, e, total, blocks, c.timed()) {
				continue
			}
			if r.Cost != 0 || r.Mod == "equals" {
				bc = bkApplyMod(bc, r.Mod, r.Cost)
			}
			if !baseApplied[ri] && (r.BaseCost != 0 || r.BaseMod == "equals") {
				base = bkApplyMod(base, r.BaseMod, r.BaseCost)
				baseApplied[ri] = true
			}
		}
		sum += bc
		t = e
	}
	cost := base + sum
	if cost < 0 {
		cost = 0
	}
	return cost
}

// bkFromCost is the "From:" price: the display cost, else the cheapest booking without rules.
func bkFromCost(c *bkConf) float64 {
	if c.DisplayCost > 0 {
		return c.DisplayCost
	}
	persons := map[string]int{}
	if c.HasPersons {
		for _, pt := range c.PersonTypes {
			persons[pt.ID] = pt.Min
		}
		n := 0
		for _, v := range persons {
			n += v
		}
		if n < c.MinPersons && len(c.PersonTypes) > 0 {
			persons[c.PersonTypes[0].ID] += c.MinPersons - n
		}
	}
	blocks := 1
	if c.DurationType == "customer" {
		blocks = c.MinDuration
	}
	base, block := c.BaseCost, c.BlockCost
	n := 0
	for _, pt := range c.PersonTypes {
		base += pt.BaseCost * float64(persons[pt.ID])
		block += pt.BlockCost * float64(persons[pt.ID])
		n += persons[pt.ID]
	}
	if len(c.Resources) > 0 { // the cheapest resource
		best := -1.0
		for _, r := range c.Resources {
			if v := r.BaseCost + r.BlockCost*float64(blocks); best < 0 || v < best {
				best = v
				base, block = c.BaseCost+r.BaseCost, c.BlockCost+r.BlockCost
				for _, pt := range c.PersonTypes {
					base += pt.BaseCost * float64(persons[pt.ID])
					block += pt.BlockCost * float64(persons[pt.ID])
				}
			}
		}
	}
	if c.HasPersons && c.PersonCostMult && n > 0 {
		base, block = base*float64(n), block*float64(n)
	}
	return base + block*float64(blocks)
}

// Availability ------------------------------------------------------------------------------------------

type bkHeld struct {
	Start, End time.Time
	Persons    int
}

// gooBkExistingErr lists the bookings that hold places between from and to (buffers included): those
// of every product holding the resource (resource 0: the product's bookings). The query error is
// returned, so a check that cannot read the bookings refuses instead of treating every place as free.
func (s *Server) gooBkExistingErr(ctx context.Context, productID, resource int64, from, to time.Time, exclude int64) ([]bkHeld, error) {
	mins := max(s.gooExtInt(bkKey, "in_cart_minutes"), 1)
	col, key := "product_id", productID
	if resource != 0 {
		col, key = "resource_id", resource
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT start_at, end_at, persons FROM goo_bookings WHERE `+col+` = $1 AND status = ANY($2)
		AND (status <> 'in-cart' OR date_created > now() - make_interval(mins => $3)) AND end_at > $4 AND start_at < $5 AND id <> $6`,
		key, bkHolding, mins, from, to, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bkHeld
	for rows.Next() {
		var h bkHeld
		var persons map[string]any
		if rows.Scan(&h.Start, &h.End, &persons) == nil {
			for _, v := range persons {
				h.Persons = bkClampPersons(h.Persons + bkClampPersons(int(min(max(gxNum(v), 0), bkMaxPersons))))
			}
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// bkBufferDur is the buffer period as a duration (in the product's unit).
func (c *bkConf) bufferDur() (time.Duration, int) {
	switch c.Unit {
	case "hour":
		return time.Duration(c.Buffer) * time.Hour, 0
	case "minute":
		return time.Duration(c.Buffer) * time.Minute, 0
	case "day":
		return 0, c.Buffer
	}
	return 0, 0
}

// bkUsed is the places used in the block [s, e) by the held bookings (buffers around them).
func (c *bkConf) used(held []bkHeld, s, e time.Time) int {
	d, days := c.bufferDur()
	n := 0
	for _, h := range held {
		hs, he := h.Start.Add(-d).AddDate(0, 0, -days), h.End.Add(d).AddDate(0, 0, days)
		if hs.Before(e) && he.After(s) {
			if c.PersonQtyMult {
				n += max(h.Persons, 1)
			} else {
				n++
			}
		}
	}
	return n
}

// bkWindow is the bookable window: now + min date … now + max date.
func (s *Server) bkWindow(c *bkConf) (time.Time, time.Time) {
	now := time.Now().In(s.gxLoc())
	from := bkAddUnits(now, c.MinDate, c.MinDateUnit)
	to := bkAddUnits(now, c.MaxDate, c.MaxDateUnit)
	if !c.timed() {
		y, m, d := from.Date()
		from = time.Date(y, m, d, 0, 0, 0, 0, from.Location())
	}
	return from, to
}

// gxbCheck is WC_Product_Booking::is_bookable(): the booking of blocks starting at start, for n
// persons, fits the window, the rules and the remaining places, checked against a resource (nil: the
// product alone): its rules, quantity and bookings.
func (s *Server) gxbCheck(ctx context.Context, p *gooProduct, c *bkConf, res *gooBkResource, start time.Time, blocks, persons int, exclude int64) error {
	var resID int64
	if res != nil {
		resID = res.ID
	}
	capacity := gxbCapacity(c, res)
	from, to := s.bkWindow(c)
	if start.Before(from) {
		return gooCartErr("The earliest booking possible is currently %s.", html.EscapeString(s.gxDate(from)+map[bool]string{true: " " + s.gxTime(from), false: ""}[c.timed()]))
	}
	if start.After(to) {
		return gooCartErr("The latest booking possible is currently %s.", html.EscapeString(s.gxDate(to)))
	}
	end := start
	for i := 0; i < blocks; i++ {
		end = c.blockEnd(end)
	}
	d, days := c.bufferDur()
	held, err := s.gooBkExistingErr(ctx, p.ID, resID, start.Add(-d).AddDate(0, 0, -days), end.Add(d).AddDate(0, 0, days), exclude)
	if err != nil {
		return gooCartErr("Availability cannot be checked right now. Please try again in a moment.")
	}
	need := 1
	if c.PersonQtyMult && persons > 0 {
		need = persons
	}
	t := start
	for i := 0; i < blocks; i++ {
		e := c.blockEnd(t)
		// Day and month blocks are checked day by day.
		if !c.timed() {
			for day := t; day.Before(e); day = day.AddDate(0, 0, 1) {
				de := day.AddDate(0, 0, 1)
				if !gxbBookable(c, res, day, de) {
					return gooCartErr("Sorry, the selected block is not available.")
				}
				if left := capacity - c.used(held, day, de); left < need {
					return bkPlacesErr(s, left, day, false)
				}
			}
		} else {
			if !gxbBookable(c, res, t, e) || (!c.AllowOvernight && t.Day() != e.Add(-time.Nanosecond).Day()) {
				return gooCartErr("Sorry, the selected block is not available.")
			}
			if left := capacity - c.used(held, t, e); left < need {
				return bkPlacesErr(s, left, t, true)
			}
		}
		t = e
	}
	return nil
}

func bkPlacesErr(s *Server, left int, t time.Time, timed bool) error {
	if left <= 0 {
		return gooCartErr("Sorry, the selected block is no longer available.")
	}
	when := s.gxDate(t)
	if timed {
		when += " " + s.gxTime(t)
	}
	return gooCartErr("There are a maximum of %d places remaining on %s.", left, html.EscapeString(when))
}

// bkSlot is a time slot of a day (hour / minute units).
type bkSlot struct {
	Time      string `json:"time"`
	End       string `json:"end"`
	Label     string `json:"label"`
	Available int    `json:"available"`
}

// bkSlots lists the day's blocks: bookable ones with their remaining places.
func (s *Server) bkSlots(ctx context.Context, p *gooProduct, c *bkConf, day time.Time) []bkSlot {
	return s.gooBkSlotsAll(ctx, p, c, day, 0)
}

// gxbSlots lists the day's blocks of a resource (nil: the product alone).
func (s *Server) gxbSlots(ctx context.Context, p *gooProduct, c *bkConf, res *gooBkResource, day time.Time) []bkSlot {
	var resID int64
	if res != nil {
		resID = res.ID
	}
	if !c.timed() {
		return nil
	}
	loc := s.gxLoc()
	y, m, d := day.Date()
	dayStart := time.Date(y, m, d, 0, 0, 0, 0, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)
	first := max(bkMinutesOf(c.FirstBlockTime), 0)
	from, to := s.bkWindow(c)
	held, err := s.gooBkExistingErr(ctx, p.ID, resID, dayStart.Add(-48*time.Hour), dayEnd.Add(48*time.Hour), 0)
	if err != nil {
		return nil
	}
	step := time.Duration(c.blockMinutes()) * time.Minute
	if step <= 0 {
		return nil
	}
	var out []bkSlot
	// Blocks are wall-clock minutes of the day built with time.Date, not elapsed time from midnight:
	// on a DST day (23 or 25 hours) the 09:00 block stays 09:00, and a time that does not exist
	// (the spring-forward gap) is skipped.
	for mins := first; ; mins += c.blockMinutes() {
		t := time.Date(y, m, d, 0, mins, 0, 0, loc)
		e := t.Add(step)
		if (e.After(dayEnd) && !c.AllowOvernight) || !t.Before(dayEnd) {
			break
		}
		if t.Hour()*60+t.Minute() != mins%1440 {
			continue
		}
		if t.Before(from) || t.After(to) || !gxbBookable(c, res, t, e) {
			continue
		}
		left := gxbCapacity(c, res) - c.used(held, t, e)
		if left <= 0 {
			continue
		}
		out = append(out, bkSlot{Time: t.Format("15:04"), End: e.Format("15:04"), Label: s.gxTime(t), Available: left})
	}
	return out
}

// Booking rows ------------------------------------------------------------------------------------------

type bkBooking struct {
	ID          int64
	ProductID   int64
	OrderID     int64
	OrderItemID int64
	CustomerID  int64
	Status      string
	Start, End  time.Time
	AllDay      bool
	Persons     map[string]any
	Cost        float64
	ResourceID  int64
	Reminded    bool
	Created     time.Time
}

const bkCols = `id, product_id, order_id, order_item_id, customer_id, status, start_at, end_at, all_day, persons, cost::float8, reminder_sent, date_created, resource_id`

func bkScan(row pgx.Row) (*bkBooking, error) {
	b := &bkBooking{}
	err := row.Scan(&b.ID, &b.ProductID, &b.OrderID, &b.OrderItemID, &b.CustomerID, &b.Status, &b.Start, &b.End, &b.AllDay, &b.Persons, &b.Cost, &b.Reminded, &b.Created, &b.ResourceID)
	if b.Persons == nil {
		b.Persons = map[string]any{}
	}
	return b, err
}

func (s *Server) bkGet(ctx context.Context, id int64) (*bkBooking, error) {
	return bkScan(s.store.Pool.QueryRow(ctx, `SELECT `+bkCols+` FROM goo_bookings WHERE id = $1`, id))
}

func (s *Server) bkQuery(ctx context.Context, where string, args ...any) []*bkBooking {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+bkCols+` FROM goo_bookings WHERE `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*bkBooking
	for rows.Next() {
		if b, err := bkScan(rows); err == nil {
			out = append(out, b)
		}
	}
	return out
}

func (s *Server) bkInsert(ctx context.Context, b *bkBooking) error {
	if b.Persons == nil {
		b.Persons = map[string]any{}
	}
	err := s.store.Pool.QueryRow(ctx, `INSERT INTO goo_bookings (product_id, order_id, order_item_id, customer_id, status, start_at, end_at, all_day, persons, cost, resource_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id, date_created`, b.ProductID, b.OrderID, b.OrderItemID, b.CustomerID, b.Status, b.Start, b.End,
		b.AllDay, b.Persons, b.Cost, b.ResourceID).Scan(&b.ID, &b.Created)
	if err == nil && b.Status != "in-cart" {
		s.gooFire(ctx, "booking_created", b) // a booking in a cart is not a booking yet
	}
	return err
}

func (s *Server) bkUpdate(ctx context.Context, b *bkBooking) error {
	_, err := s.store.Pool.Exec(ctx, `UPDATE goo_bookings SET product_id=$2, order_id=$3, order_item_id=$4, customer_id=$5, status=$6, start_at=$7, end_at=$8,
		all_day=$9, persons=$10, cost=$11, reminder_sent=$12, resource_id=$13, date_modified=now() WHERE id=$1`, b.ID, b.ProductID, b.OrderID, b.OrderItemID, b.CustomerID,
		b.Status, b.Start, b.End, b.AllDay, b.Persons, b.Cost, b.Reminded, b.ResourceID)
	if err == nil && b.Status != "in-cart" {
		s.gooFire(ctx, "booking_updated", b)
	}
	return err
}

// bkSetStatus changes a booking's status and sends the e-mails of the transition.
func (s *Server) bkSetStatus(ctx context.Context, b *bkBooking, to string) error {
	from := b.Status
	if from == to {
		return nil
	}
	b.Status = to
	if err := s.bkUpdate(ctx, b); err != nil {
		return err
	}
	// A paid booking cancelled while its order stays paid owes the customer money: refunded through
	// the order's refund flow when the gateway can refund (and the setting allows it), otherwise the
	// merchant is told to refund it by hand.
	var o *gooOrder
	if b.OrderID != 0 {
		o, _ = s.gooGetOrder(ctx, b.OrderID)
	}
	if o != nil {
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Booking #%d status changed from %s to %s.", b.ID, bkStatusLabel(from), bkStatusLabel(to)), false, "")
	}
	adminLine, customerLine := "", ""
	if to == "cancelled" && from == "paid" && o != nil && (o.Status == "processing" || o.Status == "completed") {
		amount, err := s.bkAutoRefund(ctx, o, b)
		switch {
		case err == nil && amount > 0:
			price := s.gooPricePlain(amount, o.Currency)
			adminLine = "This booking was already paid. " + price + " was refunded automatically to the customer's payment method."
			customerLine = "A refund of " + price + " has been issued to your original payment method."
		case err == nil:
			// Nothing left to refund on the booking's line (already refunded by hand).
		default:
			adminLine = "This booking was already paid. A refund is required: please refund it on the order if the customer is entitled to one."
			_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Booking #%d was cancelled after payment: a refund is required (%s).", b.ID, err.Error()), false, "")
		}
	}
	switch to {
	case "confirmed":
		s.bkMail(ctx, "booking_confirmed", b)
	case "cancelled":
		if from != "in-cart" {
			s.bkMailWith(ctx, "booking_cancelled", b, customerLine)
			s.bkMailWith(ctx, "admin_booking_cancelled", b, adminLine)
		}
	}
	return nil
}

// errBkNoAutoRefund reasons (shown in the order note next to "a refund is required").
var (
	errBkRefundOff     = errors.New("automatic refunds are turned off")
	errBkRefundGateway = errors.New("the payment method cannot refund automatically")
)

// bkRefundDue is what is still refundable on the booking's order line: its total and tax minus what
// earlier refunds already returned for that line, never more than the order's unrefunded rest.
func (s *Server) bkRefundDue(ctx context.Context, o *gooOrder, b *bkBooking) (float64, *gooRefundLine) {
	var it *gooOrderItem
	for _, i := range o.Items {
		if i.Type == "line_item" && (i.ID == b.OrderItemID || (b.OrderItemID == 0 && bkOrderBookingID(i) == b.ID)) {
			it = i
			break
		}
	}
	if it == nil {
		return 0, nil
	}
	dp := s.gooDecimals()
	line := &gooRefundLine{ItemID: it.ID, Total: it.Total, TaxTotal: map[string]float64{}}
	for k, v := range it.Taxes.Total {
		line.TaxTotal[k] = v
	}
	refunds, _ := s.gooRefunds(ctx, o.ID)
	for _, r := range refunds {
		for _, ri := range r.Items {
			if gooRefundSource(o, ri) != it.ID {
				continue
			}
			line.Total += ri.Total // refund lines are negative
			for k, v := range ri.Taxes.Total {
				line.TaxTotal[k] += v
			}
		}
	}
	line.Total = max(gooRound(line.Total, dp), 0)
	due := line.Total
	for k, v := range line.TaxTotal {
		if v = gooRound(v, dp); v <= 0 {
			delete(line.TaxTotal, k)
			continue
		}
		line.TaxTotal[k] = v
		due += v
	}
	if line.Total == it.Total {
		line.Qty = int(it.Quantity)
	}
	if rest := gooRound(o.Total-s.gooTotalRefunded(ctx, o.ID), dp); due > rest {
		// Order-level refunds without lines already ate into it: refund the rest as a plain amount.
		due = max(rest, 0)
		line = nil
	}
	return gooRound(due, dp), line
}

// bkAutoRefund refunds a cancelled paid booking through the order refund flow (wc_create_refund with
// the gateway call). It returns the refunded amount; an error means the merchant has to refund by hand.
func (s *Server) bkAutoRefund(ctx context.Context, o *gooOrder, b *bkBooking) (float64, error) {
	if !s.gooExtBool(bkKey, "auto_refund") {
		return 0, errBkRefundOff
	}
	gw := s.gooGateway(o.PaymentMethod)
	if gw == nil || !gw.Supports("refunds") {
		return 0, errBkRefundGateway
	}
	amount, line := s.bkRefundDue(ctx, o, b)
	if amount <= 0 {
		return 0, nil
	}
	var lines []gooRefundLine
	if line != nil {
		lines = append(lines, *line)
	}
	if _, err := s.gooCreateRefund(ctx, o, amount, fmt.Sprintf("Booking #%d cancelled", b.ID), lines, false, true, 0); err != nil {
		return 0, err
	}
	return amount, nil
}

// bkPersonsTotal is the number of persons of a booking.
func bkPersonsTotal(m map[string]any) int {
	n := 0
	for _, v := range m {
		n += int(gxNum(v))
	}
	return n
}

// Cart ------------------------------------------------------------------------------------------------------

// bkParseRequest reads the booking form: start, number of blocks and persons per type.
func (s *Server) bkParseRequest(c *bkConf, form map[string][]string) (time.Time, int, map[string]int, error) {
	loc := s.gxLoc()
	date := gxForm(form, "wc_bookings_field_start_date")
	if date == "" {
		y, m, d := gxForm(form, "wc_bookings_field_start_date_year"), gxForm(form, "wc_bookings_field_start_date_month"), gxForm(form, "wc_bookings_field_start_date_day")
		if c.Unit == "month" && d == "" {
			d = "1"
		}
		if y != "" && m != "" && d != "" {
			date = fmt.Sprintf("%04d-%02d-%02d", gxAtoi(y), gxAtoi(m), gxAtoi(d))
		}
	}
	if c.Unit == "month" && len(date) == 7 {
		date += "-01"
	}
	day, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return time.Time{}, 0, nil, gooCartErr("Date is required - please choose one above")
	}
	start := day
	if c.Unit == "month" {
		start = time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, loc)
	}
	if c.timed() {
		tm := gxForm(form, "wc_bookings_field_start_date_time")
		if i := strings.Index(tm, "T"); i >= 0 { // an ISO date-time
			tm = tm[i+1:]
		}
		mins := bkMinutesOf(tm)
		if mins < 0 {
			return time.Time{}, 0, nil, gooCartErr("Please choose a time.")
		}
		start = time.Date(day.Year(), day.Month(), day.Day(), mins/60, mins%60, 0, 0, loc) // wall clock, DST-safe
	}
	blocks := 1
	if c.DurationType == "customer" {
		blocks = gxAtoi(gxForm(form, "wc_bookings_field_duration"))
		if blocks <= 0 {
			blocks = c.MinDuration
		}
		if blocks < c.MinDuration {
			return start, 0, nil, gooCartErr("The minimum duration is %s.", bkUnitLabel(c.MinDuration*c.Duration, c.Unit))
		}
		if blocks > c.MaxDuration {
			return start, 0, nil, gooCartErr("The maximum duration is %s.", bkUnitLabel(c.MaxDuration*c.Duration, c.Unit))
		}
	}
	persons := map[string]int{}
	if c.HasPersons {
		total := 0
		for i, pt := range c.PersonTypes {
			v := gxForm(form, "wc_bookings_field_persons_"+pt.ID)
			if v == "" && i == 0 {
				v = gxForm(form, "wc_bookings_field_persons")
			}
			n := gxAtoi(v)
			if n < 0 {
				n = 0
			}
			if n > bkPersonsCap(c) || total+n > bkPersonsCap(c) {
				return start, 0, nil, gooCartErr("The maximum persons per group is %d.", bkPersonsCap(c))
			}
			if pt.Min > 0 && n < pt.Min {
				return start, 0, nil, gooCartErr("The minimum %s per booking is %d.", html.EscapeString(pt.Name), pt.Min)
			}
			if pt.Max > 0 && n > pt.Max {
				return start, 0, nil, gooCartErr("The maximum %s per booking is %d.", html.EscapeString(pt.Name), pt.Max)
			}
			if n > 0 {
				persons[pt.ID] = n
			}
			total += n
		}
		if total < c.MinPersons {
			return start, 0, nil, gooCartErr("The minimum persons per group is %d.", c.MinPersons)
		}
		if c.MaxPersons > 0 && total > c.MaxPersons {
			return start, 0, nil, gooCartErr("The maximum persons per group is %d.", c.MaxPersons)
		}
	}
	return start, blocks, persons, nil
}

// bkMaxPersons caps person counts: an unchecked huge count would wrap the int sum below the
// max-persons and capacity checks and shrink the per-person cost multiplier.
const bkMaxPersons = 10000

// bkPersonsCap is the most persons one booking may hold: bkMaxPersons, or the product's
// max persons per group when that is smaller.
func bkPersonsCap(c *bkConf) int {
	if c.MaxPersons > 0 && c.MaxPersons < bkMaxPersons {
		return c.MaxPersons
	}
	return bkMaxPersons
}

// bkClampPersons keeps a person count within [0, bkMaxPersons].
func bkClampPersons(n int) int {
	return min(max(n, 0), bkMaxPersons)
}

func bkSumPersons(m map[string]int) int {
	n := 0
	for _, v := range m {
		n = bkClampPersons(n + bkClampPersons(v))
	}
	return n
}

// bkAddToCart validates the booking form and holds the booking ("in-cart").
func bkAddToCart(s *Server, ctx context.Context, p *gooProduct, qty int, form map[string][]string, data map[string]any) error {
	if !s.gooExtOn(bkKey) || p.Type != bkType {
		return nil
	}
	c := bkConfOf(p)
	start, blocks, persons, err := s.bkParseRequest(c, form)
	if err != nil {
		return err
	}
	ctx, unlock, err := s.bkLock(ctx, p, c)
	if err != nil {
		return err
	}
	defer unlock()
	resID, err := s.gooBkPickResource(ctx, p, c, gxbRequestedResource(form), start, blocks, bkSumPersons(persons), 0)
	if err != nil {
		return err
	}
	end := start
	for i := 0; i < blocks; i++ {
		end = c.blockEnd(end)
	}
	cost := gooRound(gooBkCostWith(c, start, blocks, persons, resID), s.gooDecimals())
	pm := map[string]any{}
	for k, v := range persons {
		pm[k] = float64(v)
	}
	b := &bkBooking{ProductID: p.ID, Status: "in-cart", Start: start, End: end, AllDay: !c.timed(), Persons: pm, Cost: cost, ResourceID: resID}
	if err := s.bkInsert(ctx, b); err != nil {
		return err
	}
	data["booking"] = map[string]any{
		"_booking_id": float64(b.ID), "_start_date": float64(start.Unix()), "_end_date": float64(end.Unix()), "_all_day": !c.timed(),
		"_date": start.Format("2006-01-02"), "_time": map[bool]string{true: start.Format("15:04"), false: ""}[c.timed()],
		"_duration": float64(blocks * c.Duration), "_duration_unit": c.Unit, "_persons": pm, "_cost": cost, "_resource_id": float64(resID),
	}
	return nil
}

// bkLock serialises the availability check and the insert of a booking for a product and the
// resources it books (resources are shared between products), so two visitors cannot both take the
// last place: session advisory locks on one connection, taken in a fixed (sorted) order. The returned
// context marks the locks as held, so a hook running inside the locked section that books again
// (an order created or paid there) does not wait on itself.
func (s *Server) bkLock(ctx context.Context, p *gooProduct, c *bkConf) (context.Context, func(), error) {
	if ctx.Value(bkLockHeld{}) != nil {
		return ctx, func() {}, nil
	}
	keys := []string{"goo_booking:p:" + itoa64(p.ID)}
	if c.HasResources {
		for _, r := range c.Resources {
			keys = append(keys, "goo_booking:r:"+itoa64(r.ID))
		}
	}
	sort.Strings(keys)
	keys = slices.Compact(keys)
	conn, err := s.store.Pool.Acquire(ctx)
	if err != nil {
		return ctx, nil, err
	}
	var held []string
	unlock := func() {
		for i := len(held) - 1; i >= 0; i-- {
			_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, held[i])
		}
		conn.Release()
	}
	for _, k := range keys {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, k); err != nil {
			unlock()
			return ctx, nil, err
		}
		held = append(held, k)
	}
	return context.WithValue(ctx, bkLockHeld{}, true), unlock, nil
}

type bkLockHeld struct{}

// bkItemBooking is a cart item's booking data.
func bkItemBooking(it *gooCartItem) map[string]any {
	m, _ := it.Data["booking"].(map[string]any)
	return m
}

// bkCartPrice sets a booking's cost and drops bookings whose hold expired.
func bkCartPrice(s *Server, ctx context.Context, cart *gooCart, it *gooCartItem) {
	if !s.gooExtOn(bkKey) {
		return
	}
	bd := bkItemBooking(it)
	if bd == nil {
		return
	}
	id := int64(gxNum(bd["_booking_id"]))
	var status string
	var created time.Time
	err := s.store.Pool.QueryRow(ctx, `SELECT status, date_created FROM goo_bookings WHERE id = $1`, id).Scan(&status, &created)
	mins := max(s.gooExtInt(bkKey, "in_cart_minutes"), 1)
	if err != nil || status == "cancelled" || (status == "in-cart" && time.Since(created) > time.Duration(mins)*time.Minute) {
		var keep []*gooCartItem
		for _, o := range cart.Items {
			if o != it {
				keep = append(keep, o)
			}
		}
		gxCartItemsSwap(cart, keep)
		cart.errors = append(cart.errors, fmt.Sprintf("A booking for %s has been removed from your cart due to inactivity.", html.EscapeString(it.product.Name)))
		return
	}
	it.Price = gxNum(bd["_cost"])
}

// bkItemData shows Booking Date / Time / Duration / Persons under the item.
func bkItemData(s *Server, ctx context.Context, it *gooCartItem) []gooKV {
	if !s.gooExtOn(bkKey) {
		return nil
	}
	bd := bkItemBooking(it)
	if bd == nil {
		return nil
	}
	start := time.Unix(int64(gxNum(bd["_start_date"])), 0).In(s.gxLoc())
	out := []gooKV{{"Booking Date", s.gxDate(start)}}
	if !gxBool(bd["_all_day"]) {
		out = append(out, gooKV{"Booking Time", s.gxTime(start)})
	}
	out = append(out, gooKV{"Duration", bkUnitLabel(int(gxNum(bd["_duration"])), gxStr(bd["_duration_unit"]))})
	if rid := int64(gxNum(bd["_resource_id"])); rid != 0 {
		label := "Resource"
		if it.product != nil {
			label = bkConfOf(it.product).resourceLabel()
		}
		out = append(out, gooKV{label, s.gooBkResourceName(ctx, rid)})
	}
	if pm, ok := bd["_persons"].(map[string]any); ok && len(pm) > 0 {
		names := map[string]string{}
		if it.product != nil {
			for _, pt := range bkConfOf(it.product).PersonTypes {
				names[pt.ID] = pt.Name
			}
		}
		keys := make([]string, 0, len(pm))
		for k := range pm {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, gooKV{defaultStr(names[k], "Persons"), gxStr(pm[k])})
		}
	}
	return out
}

// bkCartNeedsConfirmation reports whether the cart holds a booking that requires confirmation.
func bkCartNeedsConfirmation(s *Server, cart *gooCart) bool {
	for _, it := range cart.Items {
		if bkItemBooking(it) != nil && it.product != nil && bkConfOf(it.product).RequiresConfirm {
			return true
		}
	}
	return false
}

// bkCheckout requires "Check booking availability" for carts with bookings that need confirmation.
func bkCheckout(s *Server, ctx context.Context, cart *gooCart, o *gooOrder) error {
	if !s.gooExtOn(bkKey) || !bkCartNeedsConfirmation(s, cart) {
		return nil
	}
	if o.PaymentMethod != "" && o.PaymentMethod != bkGatewayID {
		return errors.New(`Your cart contains bookings that require confirmation: please choose "Check booking availability".`)
	}
	return nil
}

// Orders --------------------------------------------------------------------------------------------------

// bkOrderBookingID is a line item's booking id.
func bkOrderBookingID(it *gooOrderItem) int64 {
	if m, ok := it.Meta["_goo_booking"].(map[string]any); ok {
		return int64(gxNum(m["_booking_id"]))
	}
	return 0
}

// bkOrderCreated attaches the held bookings to the order: unpaid, or pending confirmation. Like every
// other availability change it runs under the booking lock: a hold that expired before the order was
// saved may have been booked by someone else meanwhile (an expired hold no longer counts toward
// capacity), and an order must not take over a booking another order already owns.
func (s *Server) bkOrderCreated(ctx context.Context, o *gooOrder) {
	if !s.gooExtOn(bkKey) {
		return
	}
	for _, it := range o.ItemsOf("line_item") {
		if id := bkOrderBookingID(it); id != 0 {
			s.bkAttachToOrder(ctx, o, it, id) // takes and releases the lock per booking
		}
	}
}

// bkAttachToOrder attaches one booking to a new order (see bkOrderCreated).
func (s *Server) bkAttachToOrder(ctx context.Context, o *gooOrder, it *gooOrderItem, id int64) {
	gone := func() {
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Booking #%d could not be attached: the booked time is no longer available. Please rebook or refund it.", id), false, "")
	}
	b, err := s.bkGet(ctx, id)
	if err != nil {
		gone() // the hourly job removed the expired hold
		return
	}
	p, err := s.gooGetProduct(ctx, b.ProductID)
	if err != nil {
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Booking #%d could not be attached: its product is missing.", id), false, "")
		return
	}
	c := bkConfOf(p)
	ctx, unlock, err := s.bkLock(ctx, p, c)
	if err != nil {
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Booking #%d could not be attached: the bookings could not be locked. Please check it.", id), false, "")
		return
	}
	defer unlock()
	if b, err = s.bkGet(ctx, id); err != nil {
		gone()
		return
	}
	already := b.OrderID == o.ID
	var prev *gooOrder
	switch {
	case already && (b.Status == "in-cart" || b.Status == "unpaid" || b.Status == "pending-confirmation"):
		// The same order again: idempotent.
	case b.Status == "in-cart" && b.OrderID == 0:
	case (b.Status == "unpaid" || b.Status == "pending-confirmation") && b.OrderID != 0:
		// A checkout retry that made a new order (the cart or the customer changed): the booking moves
		// from the abandoned unpaid order, never from a paid one or from another customer's.
		prev, err = s.gooGetOrder(ctx, b.OrderID)
		if err != nil || !slices.Contains([]string{"pending", "failed", "cancelled", "checkout-draft"}, prev.Status) ||
			(prev.CustomerID != 0 && prev.CustomerID != o.CustomerID) {
			_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Booking #%d could not be attached: it belongs to order #%d.", id, b.OrderID), false, "")
			return
		}
	default:
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Booking #%d could not be attached: its status is %s.", id, bkStatusLabel(b.Status)), false, "")
		return
	}
	// An expired hold no longer counts toward capacity, so its place may have been taken meanwhile:
	// re-checked as bkRevive does. Unpaid and pending bookings always count, so they need no re-check.
	mins := max(s.gooExtInt(bkKey, "in_cart_minutes"), 1)
	if b.Status == "in-cart" && time.Since(b.Created) > time.Duration(mins)*time.Minute && !s.bkStillFree(ctx, p, c, b) {
		_ = s.bkSetStatus(ctx, b, "cancelled")
		gone()
		s.bkMail(ctx, "admin_booking_cancelled", b)
		return
	}
	if prev != nil {
		_ = s.gooAddOrderNote(ctx, prev, fmt.Sprintf("Booking #%d moved to order #%d.", id, o.ID), false, "")
	}
	b.OrderID, b.OrderItemID, b.CustomerID = o.ID, it.ID, o.CustomerID
	b.Status = "unpaid"
	if c.RequiresConfirm {
		b.Status = "pending-confirmation"
	}
	_ = s.bkUpdate(ctx, b)
	if !already {
		s.bkMail(ctx, "new_booking", b)
	}
}

// bkOrderStatus follows the order: paid when it is paid, cancelled when it is cancelled.
func (s *Server) bkOrderStatus(ctx context.Context, ch *gooStatusChange) {
	if !s.gooExtOn(bkKey) {
		return
	}
	o := ch.Order
	for _, b := range s.bkQuery(ctx, `order_id = $1`, o.ID) {
		flag := fmt.Sprintf("_booking_cancelled_by_order_%d", b.ID)
		switch ch.To {
		case "processing", "completed":
			if b.Status == "unpaid" || b.Status == "confirmed" {
				_ = s.bkSetStatus(ctx, b, "paid")
			} else if b.Status == "cancelled" && o.Meta[flag] == true {
				s.bkRevive(ctx, o, b, flag)
			}
		case "cancelled", "refunded", "failed":
			if b.Status != "cancelled" && b.Status != "in-cart" {
				if s.bkSetStatus(ctx, b, "cancelled") == nil {
					// Remembered so a late payment of the order (Pay for order after a failed
					// payment, a late gateway callback, the admin) restores the booking.
					_, _ = s.gooClaimOrderFlag(ctx, o, flag, true)
				}
			} else if b.Status == "in-cart" {
				_ = s.bkSetStatus(ctx, b, "cancelled")
			}
		}
	}
}

// bkRevive restores a booking the order flow cancelled when the order is paid after all: the block is
// re-checked (under the booking lock, ignoring the booking itself) and the booking set back to paid;
// a block sold to someone else meanwhile is left cancelled with an order note and the admin e-mail so
// the merchant can rebook or refund.
func (s *Server) bkRevive(ctx context.Context, o *gooOrder, b *bkBooking, flag string) {
	p, err := s.gooGetProduct(ctx, b.ProductID)
	if err != nil {
		return
	}
	c := bkConfOf(p)
	ctx, unlock, err := s.bkLock(ctx, p, c)
	if err != nil {
		return
	}
	defer unlock()
	if !s.bkStillFree(ctx, p, c, b) {
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Booking #%d could not be restored: the booked time is no longer available. Please rebook or refund it.", b.ID), false, "")
		s.bkMail(ctx, "admin_booking_cancelled", b)
		return
	}
	if s.bkSetStatus(ctx, b, "paid") == nil {
		_, _ = s.gooClaimOrderFlag(ctx, o, flag, false)
	}
}

// bkStillFree reports whether a booking's blocks still have room for it next to the other bookings
// (capacity only: the booking window and rules applied when it was made).
func (s *Server) bkStillFree(ctx context.Context, p *gooProduct, c *bkConf, b *bkBooking) bool {
	var res *gooBkResource
	if b.ResourceID != 0 {
		if r, err := s.gooBkResource(ctx, b.ResourceID); err == nil {
			res = r
		}
	}
	need := 1
	if persons := int(gxaSumNums(b.Persons)); c.PersonQtyMult && persons > 0 {
		need = persons
	}
	capacity := gxbCapacity(c, res)
	d, days := c.bufferDur()
	held, err := s.gooBkExistingErr(ctx, p.ID, b.ResourceID, b.Start.Add(-d).AddDate(0, 0, -days), b.End.Add(d).AddDate(0, 0, days), b.ID)
	if err != nil {
		return false
	}
	for t := b.Start; t.Before(b.End); {
		e := c.blockEnd(t)
		if !e.After(t) {
			return false
		}
		if !c.timed() {
			for day := t; day.Before(e); day = day.AddDate(0, 0, 1) {
				if capacity-c.used(held, day, day.AddDate(0, 0, 1)) < need {
					return false
				}
			}
		} else if capacity-c.used(held, t, e) < need {
			return false
		}
		t = e
	}
	return true
}

func gxaSumNums(m map[string]any) float64 {
	var n float64
	for _, v := range m {
		n += gxNum(v)
	}
	return n
}

// bkDaily sends the reminders and completes the bookings that ended.
func (s *Server) bkDaily(ctx context.Context) {
	if !s.gooExtOn(bkKey) {
		return
	}
	if days := s.gooExtInt(bkKey, "reminder_days"); days > 0 {
		loc := s.gxLoc()
		now := time.Now().In(loc)
		// Catch up after a missed run: everything that has not started and falls on or before the day N days
		// from today (the reminder_sent flag keeps each booking to one e-mail).
		y, m, d := now.Date()
		until := time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, days+1)
		for _, b := range s.bkQuery(ctx, `status IN ('paid','confirmed') AND NOT reminder_sent AND start_at > $1 AND start_at < $2`, now, until) {
			s.bkMail(ctx, "booking_reminder", b)
			b.Reminded = true
			_ = s.bkUpdate(ctx, b)
		}
	}
	_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_bookings SET status = 'complete', date_modified = now() WHERE status = 'paid' AND end_at < now()`)
}

// bkHourly removes the in-cart bookings whose hold expired (wc-booking-remove-inactive-cart).
func (s *Server) bkHourly(ctx context.Context) {
	if !s.gooExtOn(bkKey) {
		return
	}
	mins := max(s.gooExtInt(bkKey, "in_cart_minutes"), 1)
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_bookings WHERE status = 'in-cart' AND date_created < now() - make_interval(mins => $1)`, mins)
}

// E-mails -------------------------------------------------------------------------------------------------

// bkCustomerEmail is the booking's customer e-mail: the order's billing e-mail, else the user's.
func (s *Server) bkCustomerEmail(ctx context.Context, b *bkBooking) (string, *gooOrder) {
	var o *gooOrder
	if b.OrderID != 0 {
		o, _ = s.gooGetOrder(ctx, b.OrderID)
	}
	if o != nil && o.Billing["email"] != "" {
		return o.Billing["email"], o
	}
	if b.CustomerID != 0 {
		if u, err := s.store.UserByID(ctx, b.CustomerID); err == nil {
			return u.Email, o
		}
	}
	return "", o
}

// bkMail sends a booking e-mail.
func (s *Server) bkMail(ctx context.Context, id string, b *bkBooking) {
	s.bkMailWith(ctx, id, b, "")
}

// bkMailWith is bkMail with an extra paragraph under the booking table (e.g. "refund required").
func (s *Server) bkMailWith(ctx context.Context, id string, b *bkBooking, extra string) {
	p, err := s.gooGetProduct(ctx, b.ProductID)
	if err != nil {
		return
	}
	to, o := s.bkCustomerEmail(ctx, b)
	var body strings.Builder
	intro := map[string]string{
		"new_booking":             "A new booking has been made. The details of the booking are as follows:",
		"booking_confirmed":       "Your booking has been confirmed. The details of your booking are shown below.",
		"booking_reminder":        "This is a reminder that your booking will take place soon. The details of your booking are shown below.",
		"booking_cancelled":       "We are sorry to say that your booking could not be confirmed and has been cancelled. The details of the cancelled booking can be found below.",
		"admin_booking_cancelled": "The following booking has been cancelled. The details of the cancelled booking can be found below.",
	}[id]
	if id == "new_booking" && b.Status == "pending-confirmation" {
		intro = "A booking has been made and is awaiting your approval. The details of this booking are as follows:"
	}
	body.WriteString(`<p>` + intro + `</p><table cellspacing="0" cellpadding="6" border="1" style="width:100%;border:1px solid #eee;border-collapse:collapse">`)
	row := func(k, v string) {
		body.WriteString(`<tr><th scope="row" style="text-align:left;border:1px solid #eee">` + html.EscapeString(k) + `</th><td style="text-align:left;border:1px solid #eee">` + html.EscapeString(v) + `</td></tr>`)
	}
	row("Booked Product", p.Name)
	row("Booking ID", itoa64(b.ID))
	row("Booking Start Date", s.bkWhen(b, b.Start))
	row("Booking End Date", s.bkWhen(b, b.End))
	if b.ResourceID != 0 {
		row(bkConfOf(p).resourceLabel(), s.gooBkResourceName(ctx, b.ResourceID))
	}
	if n := bkPersonsTotal(b.Persons); n > 0 {
		row("Persons", fmt.Sprint(n))
	}
	body.WriteString(`</table>`)
	if id == "booking_confirmed" && o != nil && o.Status == "pending" {
		body.WriteString(`<p>To pay for this booking please use the following link: <a href="` + html.EscapeString(s.gooOrderPayURL(ctx, o)) + `">Pay for booking</a></p>`)
	}
	if id == "new_booking" && b.Status == "pending-confirmation" {
		body.WriteString(`<p>This booking is awaiting your approval. Please check it and inform the customer if the date is available or not.</p>`)
	}
	if extra != "" {
		body.WriteString(`<p><strong>` + html.EscapeString(extra) + `</strong></p>`)
	}
	// The calendar file travels with the e-mail (and as a link for clients that strip attachments);
	// a cancellation carries it too so the calendar entry is marked cancelled.
	var att []mail.Attachment
	if id == "booking_confirmed" || id == "booking_reminder" || id == "new_booking" {
		body.WriteString(`<p>Add to calendar: <a href="` + html.EscapeString(s.bkICSURL(ctx, b)) + `">Download .ics</a></p>`)
	}
	if id != "admin_booking_cancelled" {
		att = append(att, mail.Attachment{Name: "booking-" + itoa64(b.ID) + ".ics", ContentType: "text/calendar; charset=utf-8", Data: []byte(s.bkICS(ctx, []*bkBooking{b}))})
	}
	s.gxSendEmail(ctx, id, []string{to}, map[string]string{"booking_id": itoa64(b.ID), "product_title": p.Name}, body.String(), att...)
}

// bkWhen prints a booking date (with the time for timed bookings).
func (s *Server) bkWhen(b *bkBooking, t time.Time) string {
	if b.AllDay {
		if t.Equal(b.End) { // the end of an all-day booking is the next midnight: show the last day
			t = t.AddDate(0, 0, -1)
		}
		return s.gxDate(t)
	}
	return s.gxDate(t) + ", " + s.gxTime(t)
}

// The "Check booking availability" gateway -------------------------------------------------------------

const bkGatewayID = "wc-booking-gateway"

type bkGateway struct{}

func (bkGateway) ID() string          { return bkGatewayID }
func (bkGateway) MethodTitle() string { return "Check booking availability" }
func (bkGateway) MethodDescription() string {
	return "Bookings that require confirmation are requested with this method: the order is placed without payment and paid once the booking is confirmed."
}
func (bkGateway) FormFields() []gooField {
	return []gooField{{ID: "enabled", Title: "Enable/Disable", Type: "checkbox", Default: "yes", Desc: "Enable booking confirmation requests"},
		{ID: "title", Title: "Title", Type: "text", Default: "Check booking availability"},
		{ID: "description", Title: "Description", Type: "textarea", Default: ""}}
}
func (bkGateway) Supports(f string) bool { return f == "products" }
func (bkGateway) PaymentFields(s *Server, c *gin.Context) string {
	return `<p>Your booking will be confirmed by the store. You will be asked to pay once it is confirmed.</p>`
}
func (bkGateway) Validate(s *Server, c *gin.Context) error { return nil }
func (bkGateway) ProcessPayment(s *Server, c *gin.Context, o *gooOrder) (gooPaymentResult, error) {
	ctx := c.Request.Context()
	// Not "checkout": the unpaid-order clean-up must not cancel a request waiting for confirmation.
	o.CreatedVia = "bookings"
	_ = s.gooSaveOrder(ctx, o, false)
	_ = s.gooAddOrderNote(ctx, o, "This order is awaiting confirmation from the shop manager.", false, "")
	s.gooCartFor(c).Empty()
	return gooPaymentResult{Result: "success", Redirect: s.gooOrderReceivedURL(ctx, o)}, nil
}
func (bkGateway) ProcessRefund(s *Server, ctx context.Context, o *gooOrder, amount float64, reason string) error {
	return errors.New("refunds are not supported")
}
func (bkGateway) ThankYou(s *Server, ctx context.Context, o *gooOrder) string {
	return `<p>We have received your booking request. You will be notified by e-mail once it is confirmed.</p>`
}
func (bkGateway) Available(s *Server, c *gin.Context, cart *gooCart) bool {
	return s.gooExtOn(bkKey) && cart != nil && bkCartNeedsConfirmation(s, cart)
}
