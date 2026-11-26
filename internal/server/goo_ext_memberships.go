package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Goocommerce › Memberships (module "memberships"): WooCommerce Memberships. Membership plans grant
// access manually, with the purchase of a product or at account sign-up, for an unlimited, relative or
// fixed length. A plan restricts content (posts, pages and terms), may keep products for members only
// (viewing or purchasing) and gives members discounts. Members see their memberships, content and
// discounts under My Account › Members Area; the store manages members and plans in the admin.

const gmemKey = "memberships"

// gmemStatuses are the user membership statuses.
var gmemStatuses = []gooOpt{
	{"active", "Active"}, {"complimentary", "Complimentary"}, {"delayed", "Delayed"}, {"pending-cancel", "Pending Cancellation"},
	{"paused", "Paused"}, {"expired", "Expired"}, {"cancelled", "Cancelled"},
}

// gmemAccessStatuses give access to the plan's content and discounts.
var gmemAccessStatuses = []string{"active", "complimentary", "pending-cancel"}

func gmemValidStatus(st string) bool {
	for _, o := range gmemStatuses {
		if o.Value == st {
			return true
		}
	}
	return false
}

func gmemStatusLabel(st string) string {
	for _, o := range gmemStatuses {
		if o.Value == st {
			return o.Label
		}
	}
	return st
}

// gmemDiscount is a member discount rule.
type gmemDiscount struct {
	Target string  `json:"target"` // product, product_cat
	ID     int64   `json:"id"`
	Type   string  `json:"type"` // percent, fixed
	Amount float64 `json:"amount"`
}

// gmemRules are a plan's rules.
type gmemRules struct {
	ContentPostIDs  []int64 `json:"content_post_ids"`
	ContentTermIDs  []int64 `json:"content_term_ids"`
	ProductIDs      []int64 `json:"product_ids"`      // member-only products
	ProductTermIDs  []int64 `json:"product_term_ids"` // member-only product categories
	ProductRestrict string  `json:"product_restrict"` // purchase, view
	// ContentDrip delays restricted content: days after the membership start before an item opens.
	// Keys: "<post id>" for a post, "t<term id>" for a term.
	ContentDrip map[string]int `json:"content_drip,omitempty"`
	Discounts   []gmemDiscount `json:"discounts"`
}

// gooMembershipPlan is a WC_Memberships_Membership_Plan.
type gooMembershipPlan struct {
	ID           int64
	Name, Slug   string
	AccessMethod string // manual, purchase, signup
	ProductIDs   []int64
	AccessLength string // unlimited, specific, fixed
	AccessAmount int
	AccessPeriod string // days, weeks, months, years
	AccessEnd    *time.Time
	Rules        gmemRules
	Message      string
	Status       string
	DateCreated  time.Time
}

// gooUserMembership is a WC_Memberships_User_Membership.
type gooUserMembership struct {
	ID          int64
	UserID      int64
	PlanID      int64
	Status      string
	Start       time.Time
	End         *time.Time
	OrderID     int64
	ProductID   int64
	Notes       []map[string]any
	DateCreated time.Time
}

// HasAccess reports whether the membership gives access now.
func (m *gooUserMembership) HasAccess(now time.Time) bool {
	if !containsStr(gmemAccessStatuses, m.Status) || m.Start.After(now) {
		return false
	}
	return m.End == nil || m.End.After(now)
}

const gmemPlanCols = `id, name, slug, access_method, product_ids, access_length, access_amount, access_period, access_end, rules, message, status, date_created`

func gmemScanPlan(row pgx.Row) (*gooMembershipPlan, error) {
	p := &gooMembershipPlan{}
	var rules []byte
	err := row.Scan(&p.ID, &p.Name, &p.Slug, &p.AccessMethod, &p.ProductIDs, &p.AccessLength, &p.AccessAmount, &p.AccessPeriod, &p.AccessEnd,
		&rules, &p.Message, &p.Status, &p.DateCreated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(rules, &p.Rules)
	return p, nil
}

// gmemPlans lists the plans by name.
func (s *Server) gmemPlans(ctx context.Context) []*gooMembershipPlan {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+gmemPlanCols+` FROM goo_membership_plans WHERE status = 'publish' ORDER BY name, id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*gooMembershipPlan
	for rows.Next() {
		if p, err := gmemScanPlan(rows); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) gmemPlan(ctx context.Context, id int64) (*gooMembershipPlan, error) {
	return gmemScanPlan(s.store.Pool.QueryRow(ctx, `SELECT `+gmemPlanCols+` FROM goo_membership_plans WHERE id = $1`, id))
}

func (s *Server) gmemPlanBySlug(ctx context.Context, slug string) (*gooMembershipPlan, error) {
	return gmemScanPlan(s.store.Pool.QueryRow(ctx, `SELECT `+gmemPlanCols+` FROM goo_membership_plans WHERE slug = $1`, slug))
}

// gmemSavePlan inserts or updates a plan (a unique slug from the name when empty).
func (s *Server) gmemSavePlan(ctx context.Context, p *gooMembershipPlan) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("Please enter a plan name.")
	}
	if p.Slug == "" {
		p.Slug = wp.SanitizeTitle(p.Name)
	}
	p.Slug = wp.SanitizeTitle(p.Slug)
	base := p.Slug
	for i := 2; ; i++ {
		var other int64
		_ = s.store.Pool.QueryRow(ctx, `SELECT id FROM goo_membership_plans WHERE slug = $1`, p.Slug).Scan(&other)
		if other == 0 || other == p.ID {
			break
		}
		p.Slug = fmt.Sprintf("%s-%d", base, i)
	}
	if !containsStr([]string{"manual", "purchase", "signup"}, p.AccessMethod) {
		p.AccessMethod = "manual"
	}
	if !containsStr([]string{"unlimited", "specific", "fixed"}, p.AccessLength) {
		p.AccessLength = "unlimited"
	}
	if p.AccessPeriod == "" {
		p.AccessPeriod = "months"
	}
	if p.Status == "" {
		p.Status = "publish"
	}
	if p.ProductIDs == nil {
		p.ProductIDs = []int64{}
	}
	rules, _ := json.Marshal(p.Rules)
	if p.ID == 0 {
		return s.store.Pool.QueryRow(ctx, `INSERT INTO goo_membership_plans (name, slug, access_method, product_ids, access_length, access_amount,
			access_period, access_end, rules, message, status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id, date_created`,
			p.Name, p.Slug, p.AccessMethod, p.ProductIDs, p.AccessLength, p.AccessAmount, p.AccessPeriod, p.AccessEnd, rules, p.Message, p.Status).Scan(&p.ID, &p.DateCreated)
	}
	_, err := s.store.Pool.Exec(ctx, `UPDATE goo_membership_plans SET name=$2, slug=$3, access_method=$4, product_ids=$5, access_length=$6,
		access_amount=$7, access_period=$8, access_end=$9, rules=$10, message=$11, status=$12 WHERE id=$1`,
		p.ID, p.Name, p.Slug, p.AccessMethod, p.ProductIDs, p.AccessLength, p.AccessAmount, p.AccessPeriod, p.AccessEnd, rules, p.Message, p.Status)
	return err
}

// gmemEndFor is a new membership's end date from the plan's access length.
func (p *gooMembershipPlan) endFor(start time.Time) *time.Time {
	switch p.AccessLength {
	case "specific":
		if p.AccessAmount > 0 {
			t := gxaAddPeriod(start, p.AccessPeriod, p.AccessAmount)
			return &t
		}
	case "fixed":
		if p.AccessEnd != nil {
			t := *p.AccessEnd
			return &t
		}
	}
	return nil
}

// AccessLabel is "Unlimited", "1 year", "Until March 1, 2027".
func (s *Server) gmemAccessLabel(p *gooMembershipPlan) string {
	switch p.AccessLength {
	case "specific":
		return gxaPeriodLabel(p.AccessPeriod, p.AccessAmount)
	case "fixed":
		return "Until " + s.gxaDate(p.AccessEnd)
	}
	return "Unlimited"
}

const gmemUMCols = `id, user_id, plan_id, status, start_date, end_date, order_id, product_id, notes, date_created`

func gmemScanUM(row pgx.Row) (*gooUserMembership, error) {
	m := &gooUserMembership{}
	var notes []byte
	err := row.Scan(&m.ID, &m.UserID, &m.PlanID, &m.Status, &m.Start, &m.End, &m.OrderID, &m.ProductID, &notes, &m.DateCreated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(notes, &m.Notes)
	return m, nil
}

func (s *Server) gmemUM(ctx context.Context, id int64) (*gooUserMembership, error) {
	return gmemScanUM(s.store.Pool.QueryRow(ctx, `SELECT `+gmemUMCols+` FROM goo_user_memberships WHERE id = $1`, id))
}

// gmemUMList lists user memberships (userID / planID / status filters, 0 / "" for any).
func (s *Server) gmemUMList(ctx context.Context, userID, planID int64, status string) []*gooUserMembership {
	conds := []string{"true"}
	var args []any
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if userID != 0 {
		conds = append(conds, "user_id = "+add(userID))
	}
	if planID != 0 {
		conds = append(conds, "plan_id = "+add(planID))
	}
	if status != "" {
		conds = append(conds, "status = "+add(status))
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT `+gmemUMCols+` FROM goo_user_memberships WHERE `+strings.Join(conds, " AND ")+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*gooUserMembership
	for rows.Next() {
		if m, err := gmemScanUM(rows); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// gmemUMDue lists only the memberships the scheduler may have to act on: delayed ones whose start has
// come, ones whose end has passed, and (when reminders are on) ones ending within the reminder window.
func (s *Server) gmemUMDue(ctx context.Context, now time.Time, soon time.Duration) []*gooUserMembership {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+gmemUMCols+` FROM goo_user_memberships
		WHERE (status = 'delayed' AND start_date <= $1)
		   OR (end_date IS NOT NULL AND end_date <= $1 AND status IN ('active','complimentary','paused','pending-cancel'))
		   OR ($2 > 0 AND end_date IS NOT NULL AND end_date > $1 AND end_date <= $1 + ($2 * interval '1 second') AND status IN ('active','pending-cancel'))
		ORDER BY id DESC`, now, int64(soon/time.Second))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*gooUserMembership
	for rows.Next() {
		if m, err := gmemScanUM(rows); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// gmemSaveUM inserts or updates a user membership.
func (s *Server) gmemSaveUM(ctx context.Context, m *gooUserMembership) error {
	if m.Notes == nil {
		m.Notes = []map[string]any{}
	}
	notes, _ := json.Marshal(m.Notes)
	if m.ID == 0 {
		if m.Start.IsZero() {
			m.Start = time.Now()
		}
		err := s.store.Pool.QueryRow(ctx, `INSERT INTO goo_user_memberships (user_id, plan_id, status, start_date, end_date, order_id, product_id, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id, date_created`, m.UserID, m.PlanID, m.Status, m.Start, m.End, m.OrderID, m.ProductID, notes).Scan(&m.ID, &m.DateCreated)
		if err == nil {
			s.gooFire(ctx, "membership_created", m)
		}
		return err
	}
	_, err := s.store.Pool.Exec(ctx, `UPDATE goo_user_memberships SET status=$2, start_date=$3, end_date=$4, order_id=$5, product_id=$6, notes=$7 WHERE id=$1`,
		m.ID, m.Status, m.Start, m.End, m.OrderID, m.ProductID, notes)
	if err == nil {
		s.gooFire(ctx, "membership_updated", m)
	}
	return err
}

func (m *gooUserMembership) addNote(note string) {
	m.Notes = append([]map[string]any{{"date": time.Now().UTC().Format(time.RFC3339), "content": note}}, m.Notes...)
}

// gmemGrant gives a user access to a plan (wc_memberships_create_user_membership): an existing
// membership of the plan is renewed, else one is created.
func (s *Server) gmemGrant(ctx context.Context, userID int64, plan *gooMembershipPlan, orderID, productID int64, note string) (*gooUserMembership, error) {
	now := time.Now()
	list := s.gmemUMList(ctx, userID, plan.ID, "")
	// A membership that gives access now is renewed, whichever row it is (an older one can be
	// active while the newest was cancelled or is delayed); else the newest one is reactivated.
	for _, m := range list {
		if !m.HasAccess(now) {
			continue
		}
		if orderID != 0 && m.OrderID == orderID {
			return m, nil // already granted by this order (a cancelled grant is reactivated below)
		}
		if orderID != 0 {
			// A repeat purchase extends a relative access length from the current end date.
			if m.End != nil && plan.AccessLength == "specific" {
				m.End = plan.endFor(*m.End)
			}
			m.OrderID, m.ProductID = orderID, productID
			m.addNote(note)
			return m, s.gmemSaveUM(ctx, m)
		}
		return m, nil
	}
	if len(list) > 0 {
		m := list[0]
		m.Status, m.Start, m.End, m.OrderID, m.ProductID = "active", now, plan.endFor(now), orderID, productID
		m.addNote(defaultStr(note, "Membership reactivated."))
		err := s.gmemSaveUM(ctx, m)
		if err == nil {
			s.gmemMail(ctx, m, "membership_welcome")
		}
		return m, err
	}
	m := &gooUserMembership{UserID: userID, PlanID: plan.ID, Status: "active", Start: now, End: plan.endFor(now), OrderID: orderID, ProductID: productID}
	m.addNote(defaultStr(note, "Membership access granted."))
	err := s.gmemSaveUM(ctx, m)
	if err == nil {
		s.gmemMail(ctx, m, "membership_welcome")
	}
	return m, err
}

// gmemAccessPlans are the ids of the plans a user has access to now.
func (s *Server) gmemAccessPlans(ctx context.Context, userID int64) map[int64]bool {
	out := map[int64]bool{}
	if userID == 0 {
		return out
	}
	now := time.Now()
	for _, m := range s.gmemUMList(ctx, userID, 0, "") {
		if m.HasAccess(now) {
			out[m.PlanID] = true
		}
	}
	return out
}

// gmemIsManager reports whether the user bypasses restrictions (the store managers).
func gmemIsManager(u *store.User) bool { return u != nil && u.Can("manage_woocommerce", nil) }

// Restrictions ------------------------------------------------------------------------------------------

// gmemPostPlans are the plans restricting a post (by id or by one of its terms).
func (s *Server) gmemPostPlans(ctx context.Context, postID int64) []*gooMembershipPlan {
	if postID == 0 {
		return nil
	}
	plans := s.gmemPlans(ctx)
	var terms []int64
	needTerms := false
	for _, p := range plans {
		if len(p.Rules.ContentTermIDs) > 0 {
			needTerms = true
		}
	}
	if needTerms {
		if m, err := s.store.TermsForPosts(ctx, []int64{postID}, ""); err == nil {
			for _, t := range m[postID] {
				terms = append(terms, t.ID)
			}
		}
	}
	var out []*gooMembershipPlan
	for _, p := range plans {
		hit := gxaHasID(p.Rules.ContentPostIDs, postID)
		for _, t := range terms {
			hit = hit || gxaHasID(p.Rules.ContentTermIDs, t)
		}
		if hit {
			out = append(out, p)
		}
	}
	return out
}

// gmemDripDays is the days after the membership start a plan opens a post (0 = immediately): the
// shortest delay among the rules that restrict the post.
func gmemDripDays(p *gooMembershipPlan, postID int64, terms []int64) int {
	if len(p.Rules.ContentDrip) == 0 {
		return 0
	}
	best, found := 0, false
	consider := func(key string) {
		if d, ok := p.Rules.ContentDrip[key]; ok {
			if !found || d < best {
				best, found = d, true
			}
		}
	}
	consider(itoa64(postID))
	for _, t := range terms {
		consider("t" + itoa64(t))
	}
	if best < 0 {
		return 0
	}
	return best
}

// gmemPostAccess reports whether a user may read a restricted post now, honouring the drip schedule.
// When the user is a member but the content is still scheduled, availableAt is when it opens.
func (s *Server) gmemPostAccess(ctx context.Context, u *store.User, postID int64, plans []*gooMembershipPlan) (ok bool, availableAt *time.Time) {
	if len(plans) == 0 || gmemIsManager(u) {
		return true, nil
	}
	if u == nil {
		return false, nil
	}
	var terms []int64
	for _, p := range plans {
		if len(p.Rules.ContentDrip) > 0 {
			if m, err := s.store.TermsForPosts(ctx, []int64{postID}, ""); err == nil {
				for _, t := range m[postID] {
					terms = append(terms, t.ID)
				}
			}
			break
		}
	}
	now := time.Now()
	for _, p := range plans {
		days := gmemDripDays(p, postID, terms)
		for _, m := range s.gmemUMList(ctx, u.ID, p.ID, "") {
			if !m.HasAccess(now) {
				continue
			}
			at := m.Start.AddDate(0, 0, days)
			if !at.After(now) {
				return true, nil
			}
			if m.End != nil && !at.Before(*m.End) {
				continue // the membership ends before the content opens
			}
			if availableAt == nil || at.Before(*availableAt) {
				t := at
				availableAt = &t
			}
		}
	}
	return false, availableAt
}

// gmemProductPlans are the plans keeping a product for members (with the restriction mode).
func (s *Server) gmemProductPlans(ctx context.Context, p *gooProduct, mode string) []*gooMembershipPlan {
	if p == nil {
		return nil
	}
	id := p.ID
	if p.Type == gooTypeVariation && p.ParentID != 0 {
		id = p.ParentID
	}
	plans := s.gmemPlans(ctx)
	var cats []int64
	for _, pl := range plans {
		if len(pl.Rules.ProductTermIDs) > 0 && cats == nil {
			cats = []int64{}
			if m, err := s.store.TermsForPosts(ctx, []int64{id}, "product_cat"); err == nil {
				for _, t := range m[id] {
					cats = append(cats, t.ID)
				}
			}
		}
	}
	var out []*gooMembershipPlan
	for _, pl := range plans {
		pm := defaultStr(pl.Rules.ProductRestrict, "purchase")
		if mode == "view" && pm != "view" {
			continue
		}
		hit := gxaHasID(pl.Rules.ProductIDs, id)
		for _, t := range cats {
			hit = hit || gxaHasID(pl.Rules.ProductTermIDs, t)
		}
		if hit {
			out = append(out, pl)
		}
	}
	return out
}

// gmemCanAccess reports whether a user is a member of one of the plans (or manages the store).
func (s *Server) gmemCanAccess(ctx context.Context, u *store.User, plans []*gooMembershipPlan) bool {
	if len(plans) == 0 || gmemIsManager(u) {
		return true
	}
	if u == nil {
		return false
	}
	have := s.gmemAccessPlans(ctx, u.ID)
	for _, p := range plans {
		if have[p.ID] {
			return true
		}
	}
	return false
}

// gmemCanView reports whether a member-only product shows to the user (hidden per setting).
func (s *Server) gmemCanView(ctx context.Context, u *store.User, p *gooProduct) bool {
	if !s.gooExtOn(gmemKey) {
		return true
	}
	plans := s.gmemProductPlans(ctx, p, "view")
	if s.gooExtBool(gmemKey, "hide_restricted_products") {
		plans = s.gmemProductPlans(ctx, p, "")
	}
	return s.gmemCanAccess(ctx, u, plans)
}

// gmemGrantingProductsHTML lists links to the products that grant one of the plans.
func (s *Server) gmemGrantingProductsHTML(ctx context.Context, plans []*gooMembershipPlan) string {
	var ids []int64
	for _, p := range plans {
		if p.AccessMethod == "purchase" {
			ids = append(ids, p.ProductIDs...)
		}
	}
	products, _ := s.gooGetProducts(ctx, ids)
	var links []string
	for _, id := range ids {
		if p := products[id]; p != nil && p.Status == "publish" {
			links = append(links, `<a href="`+gooEsc(s.gooPermalink(ctx, p))+`">`+gooEsc(p.Name)+`</a>`)
		}
	}
	switch len(links) {
	case 0:
		return ""
	case 1:
		return links[0]
	}
	return strings.Join(links[:len(links)-1], ", ") + " or " + links[len(links)-1]
}

// gmemMessage is a restriction message with its merge tags ({products}, {login_url}, {date}).
func (s *Server) gmemMessage(ctx context.Context, kind string, plans []*gooMembershipPlan, u *store.User) string {
	msg := ""
	for _, p := range plans {
		if strings.TrimSpace(p.Message) != "" && kind == "content" {
			msg = p.Message
			break
		}
	}
	products := s.gmemGrantingProductsHTML(ctx, plans)
	if msg == "" {
		field := map[string]string{"content": "content_restricted_message", "purchase": "product_purchasing_restricted_message", "view": "product_viewing_restricted_message"}[kind]
		if products == "" {
			field += "_no_products"
		}
		msg = s.gooExtString(gmemKey, field)
	}
	login := s.loginPath()
	if s.gooPageID("myaccount") > 0 {
		login = s.gooPageURL(ctx, "myaccount")
	}
	if u != nil {
		msg = regexp.MustCompile(`,? ?or <a href="\{login_url\}">log in</a>[^.]*`).ReplaceAllString(msg, "")
	}
	return strings.NewReplacer("{products}", products, "{login_url}", gooEsc(login)).Replace(msg)
}

var (
	gmemMembersOnlyRe = regexp.MustCompile(`(?s)\[goocommerce_members_only([^\]]*)\](.*?)\[/goocommerce_members_only\]`)
	gmemNonMembersRe  = regexp.MustCompile(`(?s)\[goocommerce_non_members([^\]]*)\](.*?)\[/goocommerce_non_members\]`)
	gmemDiscountRe    = regexp.MustCompile(`\[goocommerce_member_discount([^\]]*)\]`)
	gmemPlansAttrRe   = regexp.MustCompile(`plans?\s*=\s*["']([^"']*)["']`)
)

// gmemShortcodePlans are the plans named by a shortcode's plans="" attribute (nil = any plan).
func (s *Server) gmemShortcodePlans(ctx context.Context, attrs string) []*gooMembershipPlan {
	m := gmemPlansAttrRe.FindStringSubmatch(attrs)
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return nil
	}
	var out []*gooMembershipPlan
	for _, ref := range strings.Split(m[1], ",") {
		ref = strings.TrimSpace(ref)
		if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
			if p, err := s.gmemPlan(ctx, id); err == nil {
				out = append(out, p)
			}
		} else if p, err := s.gmemPlanBySlug(ctx, ref); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// gmemIsMemberOf reports whether the user is a member of one of the plans (any plan when nil).
func (s *Server) gmemIsMemberOf(ctx context.Context, u *store.User, plans []*gooMembershipPlan) bool {
	if u == nil {
		return false
	}
	have := s.gmemAccessPlans(ctx, u.ID)
	if plans == nil {
		return len(have) > 0
	}
	for _, p := range plans {
		if have[p.ID] {
			return true
		}
	}
	return false
}

// gmemFilterContent restricts the content being rendered and runs the members shortcodes.
func gmemFilterContent(s *Server, ctx context.Context, content string) string {
	if !s.gooExtOn(gmemKey) {
		return content
	}
	u := gxaUser(ctx)
	if strings.Contains(content, "[goocommerce_") {
		content = gmemMembersOnlyRe.ReplaceAllStringFunc(content, func(m string) string {
			sm := gmemMembersOnlyRe.FindStringSubmatch(m)
			if s.gmemIsMemberOf(ctx, u, s.gmemShortcodePlans(ctx, sm[1])) || gmemIsManager(u) {
				return sm[2]
			}
			return ""
		})
		content = gmemNonMembersRe.ReplaceAllStringFunc(content, func(m string) string {
			sm := gmemNonMembersRe.FindStringSubmatch(m)
			if s.gmemIsMemberOf(ctx, u, s.gmemShortcodePlans(ctx, sm[1])) {
				return ""
			}
			return sm[2]
		})
		content = gmemDiscountRe.ReplaceAllStringFunc(content, func(string) string { return s.gmemDiscountsHTML(ctx, u) })
	}
	var postID int64
	if rc := renderFrom(ctx); rc != nil {
		postID = rc.postID
	}
	plans := s.gmemPostPlans(ctx, postID)
	ok, at := s.gmemPostAccess(ctx, u, postID, plans)
	if ok {
		return content
	}
	return s.gmemRestrictedContent(ctx, postID, plans, u, at)
}

// gmemStripShortcodes removes the members shortcodes with what they enclose from text an excerpt or a
// description is made of (strip_shortcodes() in wp_trim_excerpt()): excerpts are cut from the raw
// content, so "[goocommerce_members_only]…" would otherwise show its members-only text to everyone.
func gmemStripShortcodes(text string) string {
	if !strings.Contains(text, "[goocommerce_") {
		return text
	}
	text = gmemMembersOnlyRe.ReplaceAllString(text, "")
	text = gmemNonMembersRe.ReplaceAllString(text, "")
	return gmemDiscountRe.ReplaceAllString(text, "")
}

// gmemShortcodesFor resolves the members shortcodes of raw text for viewer u without rendering them:
// a members-only body stays only for its members (and managers), a non-members body only for
// non-members, and the discount table is dropped.
func (s *Server) gmemShortcodesFor(ctx context.Context, u *store.User, text string) string {
	if !strings.Contains(text, "[goocommerce_") {
		return text
	}
	text = gmemMembersOnlyRe.ReplaceAllStringFunc(text, func(m string) string {
		sm := gmemMembersOnlyRe.FindStringSubmatch(m)
		if gmemIsManager(u) || s.gmemIsMemberOf(ctx, u, s.gmemShortcodePlans(ctx, sm[1])) {
			return sm[2]
		}
		return ""
	})
	text = gmemNonMembersRe.ReplaceAllStringFunc(text, func(m string) string {
		sm := gmemNonMembersRe.FindStringSubmatch(m)
		if s.gmemIsMemberOf(ctx, u, s.gmemShortcodePlans(ctx, sm[1])) {
			return ""
		}
		return sm[2]
	})
	return gmemDiscountRe.ReplaceAllString(text, "")
}

// gooRESTRestrict is Memberships for the REST API: content.rendered is the_content in WordPress, so a
// restricted post shows a non-member the same restriction message as its page, and the excerpt only
// when the plan shows excerpts. The members-only shortcodes are applied too.
func (s *Server) gooRESTRestrict(ctx context.Context, u *store.User, p *store.Post, rendered, excerpt string) (string, string) {
	if !s.gooOn() || !s.gooExtOn(gmemKey) {
		return rendered, excerpt
	}
	rc := &renderCtx{s: s, lb: s.links(ctx), user: u, postID: p.ID}
	ctx = withRender(withPluginUser(ctx, u), rc)
	plans := s.gmemPostPlans(ctx, p.ID)
	if ok, _ := s.gmemPostAccess(ctx, u, p.ID, plans); !ok && !s.gooExtBool(gmemKey, "show_excerpts") {
		excerpt = ""
	}
	return gmemFilterContent(s, ctx, rendered), excerpt
}

// gmemRestrictsPost reports whether Memberships hides a post's content from the current visitor.
// Paths that bypass the_content (the Web Builder snapshot) use it to fall back to the theme, whose
// content filter shows the restriction message.
func (s *Server) gmemRestrictsPost(c *gin.Context, postID int64) bool {
	if !s.gooOn() || !s.gooExtOn(gmemKey) {
		return false
	}
	ctx := c.Request.Context()
	ok, _ := s.gmemPostAccess(ctx, currentUser(c), postID, s.gmemPostPlans(ctx, postID))
	return !ok
}

// gmemRestrictedContent is what a non-member sees instead of restricted content: the excerpt (per
// setting) and the restriction message.
func (s *Server) gmemRestrictedContent(ctx context.Context, postID int64, plans []*gooMembershipPlan, u *store.User, availableAt *time.Time) string {
	out := ""
	if s.gooExtBool(gmemKey, "show_excerpts") && postID != 0 {
		if p, err := s.store.GetPost(ctx, postID); err == nil {
			ex := strings.TrimSpace(p.Excerpt)
			if ex == "" {
				words := strings.Fields(gxaStripTags(p.Content))
				if len(words) > 55 {
					words = append(words[:55], "&hellip;")
				}
				ex = strings.Join(words, " ")
			}
			if ex != "" {
				out += `<div class="wc-memberships-content-excerpt"><p>` + ex + `</p></div>`
			}
		}
	}
	msg := s.gmemMessage(ctx, "content", plans, u)
	if availableAt != nil {
		msg = "This content is part of your membership and will be available on " + s.gxaInputDate(availableAt, false) + "."
	}
	if s.gooExtString(gmemKey, "restriction_mode") == "redirect" && availableAt == nil {
		if id := int64(s.gooExtInt(gmemKey, "redirect_page_id")); id > 0 {
			if p, err := s.store.GetPost(ctx, id); err == nil {
				msg += ` <a href="` + gooEsc(s.links(ctx).Permalink(p)) + `" class="button">` + gooEsc(p.Title) + `</a>`
			}
		}
	}
	return out + `<div class="woocommerce"><div class="woocommerce-info wc-memberships-restriction-message wc-memberships-message wc-memberships-content-restricted-message">` + msg + `</div></div>`
}

// Discounts ---------------------------------------------------------------------------------------------

// gmemMemberPrice is the best member price of a product for a user (ok false when no discount applies).
func (s *Server) gmemMemberPrice(ctx context.Context, u *store.User, p *gooProduct, price float64) (float64, bool) {
	if u == nil || p == nil || price <= 0 {
		return price, false
	}
	if p.IsOnSale() && !s.gooExtBool(gmemKey, "apply_discounts_to_sale_items") {
		return price, false
	}
	have := s.gmemAccessPlans(ctx, u.ID)
	if len(have) == 0 {
		return price, false
	}
	id := p.ID
	if p.Type == gooTypeVariation && p.ParentID != 0 {
		id = p.ParentID
	}
	var cats map[int64]bool
	best, found := price, false
	for _, pl := range s.gmemPlans(ctx) {
		if !have[pl.ID] {
			continue
		}
		for _, d := range pl.Rules.Discounts {
			applies := false
			switch d.Target {
			case "product":
				applies = d.ID == id || d.ID == p.ID
			case "product_cat":
				if cats == nil {
					cats = map[int64]bool{}
					if m, err := s.store.TermsForPosts(ctx, []int64{id}, "product_cat"); err == nil {
						for _, t := range m[id] {
							cats[t.ID] = true
							if t.ParentID != 0 {
								cats[t.ParentID] = true
							}
						}
					}
				}
				applies = cats[d.ID]
			}
			if !applies || d.Amount <= 0 {
				continue
			}
			v := price - d.Amount
			if d.Type == "percent" {
				v = price * (1 - d.Amount/100)
			}
			v = max(gooRound(v, s.gooDecimals()), 0)
			if v < best {
				best, found = v, true
			}
		}
	}
	return best, found
}

// gmemDiscountsHTML lists the member discounts of a user ([goocommerce_member_discount], Members Area).
func (s *Server) gmemDiscountsHTML(ctx context.Context, u *store.User) string {
	if u == nil {
		return ""
	}
	have := s.gmemAccessPlans(ctx, u.ID)
	var rows strings.Builder
	for _, pl := range s.gmemPlans(ctx) {
		if !have[pl.ID] {
			continue
		}
		for _, d := range pl.Rules.Discounts {
			title := ""
			link := ""
			if d.Target == "product" {
				if p, err := s.gooGetProduct(ctx, d.ID); err == nil {
					title, link = p.Name, s.gooPermalink(ctx, p)
				}
			} else if t, err := s.store.GetTerm(ctx, d.ID); err == nil {
				title, link = t.Name, s.links(ctx).HomeURL("/?product_cat="+t.Slug)
			}
			if title == "" {
				continue
			}
			amount := string(s.gooPrice(d.Amount))
			if d.Type == "percent" {
				amount = strconv.FormatFloat(d.Amount, 'f', -1, 64) + "%"
			}
			rows.WriteString(`<tr><td class="discount-title"><a href="` + gooEsc(link) + `">` + gooEsc(title) + `</a></td><td class="discount-amount">` + amount + `</td><td class="discount-plan">` + gooEsc(pl.Name) + `</td></tr>`)
		}
	}
	if rows.Len() == 0 {
		return ""
	}
	return `<table class="shop_table shop_table_responsive my_account_orders my_account_memberships my_membership_discounts"><thead><tr><th>Title</th><th>Discount</th><th>Plan</th></tr></thead><tbody>` + rows.String() + `</tbody></table>`
}

func init() {
	gooRegisterExtension(&gooExtension{
		Key: gmemKey, Name: "Memberships", Icon: "dashicons-groups", Category: "Customer engagement",
		Description: "Power your membership association, online magazine, elearning sites, and more with access control to content and products, and member discounts. Plans are granted by purchase, at sign-up or manually.",
		Fields:      gmemFields(),
		Pages:       []gooExtMenuItem{{Page: "memberships", Title: "Memberships"}, {Page: "membership-plans", Title: "Membership Plans"}},
	})
	gooExtAdminPages["memberships"] = func(s *Server, c *gin.Context) { s.gmemAdminMembers(c) }
	gooExtAdminPages["membership-plans"] = func(s *Server, c *gin.Context) { s.gmemAdminPlans(c) }

	gooContentFilters = append(gooContentFilters, gmemFilterContent)
	gooListingExcludeHooks = append(gooListingExcludeHooks, gmemListingExcludeRmode)
	gooSingleAccessHooks = append(gooSingleAccessHooks, gmemSingleAccessRmode)

	gooAddToCartHooks = append(gooAddToCartHooks, func(s *Server, ctx context.Context, p *gooProduct, qty int, form map[string][]string, data map[string]any) error {
		if !s.gooExtOn(gmemKey) {
			return nil
		}
		u := gxaUser(ctx)
		if plans := s.gmemProductPlans(ctx, p, ""); !s.gmemCanAccess(ctx, u, plans) {
			return gooCartErr("%s", s.gmemMessage(ctx, "purchase", plans, u))
		}
		return nil
	})

	gooCartItemPriceHooks = append(gooCartItemPriceHooks, func(s *Server, ctx context.Context, cart *gooCart, it *gooCartItem) {
		if !s.gooExtOn(gmemKey) {
			return
		}
		u := gxaUser(ctx)
		if u == nil && cart != nil && cart.c != nil {
			u = currentUser(cart.c)
		}
		if v, ok := s.gmemMemberPrice(ctx, u, it.Product(), it.Price); ok {
			it.Price = v
		}
	})
	gooProductPriceHooks = append(gooProductPriceHooks, func(s *Server, ctx context.Context, p *gooProduct, price float64) float64 {
		if !s.gooExtOn(gmemKey) {
			return price
		}
		if v, ok := s.gmemMemberPrice(ctx, gxaUser(ctx), p, price); ok {
			return v
		}
		return price
	})
	gooPriceHTMLHooks = append(gooPriceHTMLHooks, func(s *Server, ctx context.Context, p *gooProduct, h template.HTML) template.HTML {
		if !s.gooExtOn(gmemKey) || p == nil || !p.HasPrice() || p.Type == gooTypeVariable || p.Type == gooTypeGrouped {
			return h
		}
		price := p.PriceValue()
		v, ok := s.gmemMemberPrice(ctx, gxaUser(ctx), p, price)
		if !ok {
			return h
		}
		return template.HTML(`<del aria-hidden="true">` + string(s.gooPrice(price)) + `</del> <ins>` + string(s.gooPrice(v)) + `</ins>` +
			` <span class="wc-memberships-member-discount-message">Member price</span>`)
	})
	gooOnHTML("single_after_add_to_cart", func(s *Server, c *gin.Context, p *gooProduct) template.HTML {
		if !s.gooExtOn(gmemKey) {
			return ""
		}
		ctx := c.Request.Context()
		u := currentUser(c)
		if plans := s.gmemProductPlans(ctx, p, ""); !s.gmemCanAccess(ctx, u, plans) {
			return template.HTML(`<div class="woocommerce"><div class="woocommerce-info wc-memberships-restriction-message wc-memberships-message wc-memberships-product-purchasing-restricted-message">` +
				s.gmemMessage(ctx, "purchase", plans, u) + `</div></div>`)
		}
		return ""
	})

	gooPaidValueHooks = append(gooPaidValueHooks, func(s *Server, ctx context.Context, o *gooOrder) bool {
		if !s.gooExtOn(gmemKey) || o.CustomerID == 0 {
			return false
		}
		plans := s.gmemPlans(ctx)
		for _, it := range o.ItemsOf("line_item") {
			for _, p := range plans {
				if p.AccessMethod == "purchase" && (gxaHasID(p.ProductIDs, it.ProductID) || (it.VariationID != 0 && gxaHasID(p.ProductIDs, it.VariationID))) {
					return true
				}
			}
		}
		return false
	})
	gooOn("order_status_changed", 30, func(s *Server, ctx context.Context, arg any) {
		ch, ok := arg.(*gooStatusChange)
		if !ok || !s.gooExtOn(gmemKey) {
			return
		}
		switch {
		case containsStr(gooPaidStatuses, ch.To):
			s.gmemGrantFromOrder(ctx, ch.Order)
		case ch.To == "cancelled" || ch.To == "refunded":
			s.gmemRevokeFromOrder(ctx, ch.Order, ch.To)
		}
	})
	gooOn("order_partially_refunded", 30, func(s *Server, ctx context.Context, arg any) {
		if ev, ok := arg.(*gooRefundEvent); ok && s.gooExtOn(gmemKey) {
			s.gmemRevokeFromPartialRefund(ctx, ev.Order)
		}
	})
	gooOn("subscription_status_changed", 30, func(s *Server, ctx context.Context, arg any) {
		ch, ok := arg.(*gsubStatusChange)
		if !ok || !s.gooExtOn(gmemKey) {
			return
		}
		s.gmemFollowSubscription(ctx, ch)
	})
	gooOn("customer_created", 30, func(s *Server, ctx context.Context, arg any) {
		if !s.gooExtOn(gmemKey) {
			return
		}
		var uid int64
		switch v := arg.(type) {
		case *gooNewCustomer:
			uid = v.UserID
		case int64:
			uid = v
		}
		if uid == 0 {
			return
		}
		for _, p := range s.gmemPlans(ctx) {
			if p.AccessMethod == "signup" {
				_, _ = s.gmemGrant(ctx, uid, p, 0, 0, "Membership access granted at account registration.")
			}
		}
	})
	gooOn("hourly", 30, func(s *Server, ctx context.Context, _ any) {
		if s.gooExtOn(gmemKey) {
			s.gmemScheduler(ctx)
		}
	})

	for _, e := range []*gooEmailDef{
		{ID: "membership_welcome", Title: "Membership welcome", Customer: true,
			Description: "Sent to a customer when they are given access to a membership plan.",
			Subject:     "[{site_title}] Welcome to the {plan} membership", Heading: "Welcome to {plan}"},
		{ID: "membership_ending_soon", Title: "Membership ending soon", Customer: true,
			Description: "Sent to a member a few days before their membership ends (see Memberships settings).",
			Subject:     "[{site_title}] Your {plan} membership ends on {end_date}", Heading: "Your membership is ending soon"},
		{ID: "membership_ended", Title: "Membership ended", Customer: true,
			Description: "Sent to a member when their membership has ended.",
			Subject:     "[{site_title}] Your {plan} membership has ended", Heading: "Your membership has ended"},
	} {
		e.Module = gmemKey
		gooRegisterEmail(e)
	}

	gooAccountEndpoints = append(gooAccountEndpoints, &gooAccountEndpoint{Slug: "members-area", Title: "Memberships", Module: gmemKey, After: "orders", Render: gmemAccountRender})
}

// gmemFields are the Memberships settings.
func gmemFields() []gooField {
	return []gooField{
		{ID: "general_title", Title: "General", Type: "title"},
		{ID: "restriction_mode", Title: "Content restriction mode", Type: "select", Default: "hide_content",
			Options: []gooOpt{{"hide", "Hide completely"}, {"hide_content", "Hide content only"}, {"redirect", "Redirect to page"}},
			Desc:    "Specifies the way content is restricted: whether to show nothing, excerpts, or send to a landing page."},
		{ID: "redirect_page_id", Title: "Redirect page", Type: "number", Default: 0.0, Desc: "The page id non-members are sent to when the restriction mode is \"Redirect to page\"."},
		{ID: "show_excerpts", Title: "Show excerpts", Type: "checkbox", Default: true, Desc: "If enabled, an excerpt of the protected content will be displayed to non-members & search engines."},
		{ID: "hide_restricted_products", Title: "Restricted products", Type: "checkbox", Default: false, Desc: "Hide restricted products (if enabled, products restricted to members are hidden from the shop for non-members)"},
		{ID: "apply_discounts_to_sale_items", Title: "Inherit discounts", Type: "checkbox", Default: false, Desc: "Apply member discounts to products on sale"},
		{ID: "ending_soon_days", Title: "Ending soon reminder", Type: "number", Default: 7.0, Min: "0", Desc: "Days before a membership ends to e-mail the member a reminder. Use 0 to turn the reminder off."},
		{ID: "general_end", Type: "sectionend"},
		{ID: "messages_title", Title: "Restriction messages", Type: "title", Desc: "Merge tags: {products} (the products granting access), {login_url}."},
		{ID: "content_restricted_message", Title: "Content restricted (purchase required)", Type: "textarea",
			Default: `To access this content, you must purchase {products}, or <a href="{login_url}">log in</a> if you are a member.`},
		{ID: "content_restricted_message_no_products", Title: "Content restricted (membership required)", Type: "textarea",
			Default: `This content is only available to members.`},
		{ID: "product_viewing_restricted_message", Title: "Product viewing restricted (purchase required)", Type: "textarea",
			Default: `This product can only be viewed by members. To view or purchase this product, sign up by purchasing {products}, or <a href="{login_url}">log in</a> if you are a member.`},
		{ID: "product_viewing_restricted_message_no_products", Title: "Product viewing restricted (membership required)", Type: "textarea",
			Default: `This product can only be viewed by members.`},
		{ID: "product_purchasing_restricted_message", Title: "Product buying restricted (purchase required)", Type: "textarea",
			Default: `This product can only be purchased by members. To purchase this product, sign up by purchasing {products}, or <a href="{login_url}">log in</a> if you are a member.`},
		{ID: "product_purchasing_restricted_message_no_products", Title: "Product buying restricted (membership required)", Type: "textarea",
			Default: `This product can only be purchased by members.`},
		{ID: "messages_end", Type: "sectionend"},
	}
}

// gmemGrantFromOrder grants the plans bought with a paid order.
func (s *Server) gmemGrantFromOrder(ctx context.Context, o *gooOrder) {
	if o.CustomerID == 0 {
		return
	}
	plans := s.gmemPlans(ctx)
	for _, it := range o.ItemsOf("line_item") {
		for _, p := range plans {
			if p.AccessMethod != "purchase" {
				continue
			}
			if gxaHasID(p.ProductIDs, it.ProductID) || (it.VariationID != 0 && gxaHasID(p.ProductIDs, it.VariationID)) {
				// One grant per order and plan: the order flag is claimed atomically, so processing then
				// completed (or another order granted in between) never extends the access length twice.
				flag := gmemGrantFlag(p.ID)
				if claimed, err := s.gooClaimOrderFlag(ctx, o, flag, true); err != nil || !claimed {
					continue
				}
				if _, err := s.gmemGrant(ctx, o.CustomerID, p, o.ID, it.ProductID, fmt.Sprintf("Membership access granted from purchasing %s (Order #%d).", gooEsc(it.Name), o.ID)); err != nil {
					_, _ = s.gooClaimOrderFlag(ctx, o, flag, false)
					continue
				}
				_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Customer granted access to the %s membership plan.", gooEsc(p.Name)), false, "")
			}
		}
	}
}

// gmemGrantFlag is the order meta flag marking that the order granted (or extended) a plan.
func gmemGrantFlag(planID int64) string {
	return fmt.Sprintf("_membership_granted_%d", planID)
}

// gmemRevokeFromOrder cancels the memberships an order granted when the order is cancelled or
// refunded (WooCommerce Memberships), and releases the order's grant flags so paying it again grants
// access again.
func (s *Server) gmemRevokeFromOrder(ctx context.Context, o *gooOrder, to string) {
	if o.CustomerID == 0 {
		return
	}
	for _, m := range s.gmemUMList(ctx, o.CustomerID, 0, "") {
		if m.OrderID != o.ID || m.Status == "cancelled" || m.Status == "expired" {
			continue
		}
		m.addNote(fmt.Sprintf("Membership cancelled because Order #%d was %s.", o.ID, strings.ToLower(gooStatusLabel(to))))
		m.Status = "cancelled"
		now := time.Now()
		if m.End == nil || m.End.After(now) {
			m.End = &now
		}
		_ = s.gmemSaveUM(ctx, m)
	}
	for k, v := range o.Meta {
		if strings.HasPrefix(k, "_membership_granted_") && v == true {
			_, _ = s.gooClaimOrderFlag(ctx, o, k, false)
		}
	}
}

// gmemRevokeFromPartialRefund handles a partial refund: a membership the order granted is cancelled
// once the line of the product that granted it is refunded in full (its whole quantity or its whole
// total), and the plan's grant flag is released. Other lines' refunds, and partial refunds of the
// granting line, leave the membership alone.
func (s *Server) gmemRevokeFromPartialRefund(ctx context.Context, o *gooOrder) {
	if o == nil || o.CustomerID == 0 {
		return
	}
	var refunds []*gooOrder
	loaded := false
	for _, m := range s.gmemUMList(ctx, o.CustomerID, 0, "") {
		if m.OrderID != o.ID || m.ProductID == 0 || m.Status == "cancelled" || m.Status == "expired" {
			continue
		}
		if !loaded {
			refunds, _ = s.gooRefunds(ctx, o.ID)
			loaded = true
		}
		var qty, total, rQty, rTotal float64
		found := false
		for _, it := range o.ItemsOf("line_item") {
			if it.ProductID != m.ProductID && it.VariationID != m.ProductID {
				continue
			}
			found = true
			qty += it.Quantity
			total += it.Total
			for _, r := range refunds {
				for _, ri := range r.Items {
					if gooRefundSource(o, ri) == it.ID {
						rQty -= ri.Quantity
						rTotal -= ri.Total
					}
				}
			}
		}
		if !found || !((qty > 0 && rQty >= qty) || (total > 0 && rTotal >= total-0.005)) {
			continue
		}
		m.addNote(fmt.Sprintf("Membership cancelled because its product was refunded on Order #%d.", o.ID))
		m.Status = "cancelled"
		now := time.Now()
		if m.End == nil || m.End.After(now) {
			m.End = &now
		}
		if s.gmemSaveUM(ctx, m) == nil {
			_, _ = s.gooClaimOrderFlag(ctx, o, gmemGrantFlag(m.PlanID), false)
			_ = s.gooAddOrderNote(ctx, o, "Membership access removed: the membership product was refunded.", false, "")
		}
	}
}

// gmemFollowSubscription keeps the memberships granted by a subscription's parent or renewal orders in
// step with the subscription (WooCommerce Memberships for Subscriptions): on hold pauses them,
// reactivation resumes them, pending cancellation ends them with the prepaid period, and a cancelled
// or expired subscription cancels or expires them.
func (s *Server) gmemFollowSubscription(ctx context.Context, ch *gsubStatusChange) {
	sub := ch.Sub
	if sub == nil || sub.CustomerID == 0 {
		return
	}
	orders := map[int64]bool{sub.ParentOrderID: true}
	if rows, err := s.store.Pool.Query(ctx, `SELECT id FROM goo_orders WHERE (meta->>'_subscription_renewal')::numeric = $1
		OR (meta->>'_subscription_switch')::numeric = $1`, sub.ID); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				orders[id] = true
			}
		}
		rows.Close()
	}
	now := time.Now()
	for _, m := range s.gmemUMList(ctx, sub.CustomerID, 0, "") {
		if m.OrderID == 0 || !orders[m.OrderID] {
			continue
		}
		from, prevEnd := m.Status, ""
		switch ch.To {
		case "on-hold":
			if m.Status == "active" {
				m.Status = "paused"
			}
		case "active":
			if m.Status == "paused" || m.Status == "pending-cancel" {
				m.Status = "active"
			}
			if from == "pending-cancel" {
				m.End = gmemPrevEnd(m)
			}
		case "pending-cancel":
			if containsStr([]string{"active", "paused"}, m.Status) {
				m.Status = "pending-cancel"
				if m.End != nil {
					prevEnd = m.End.UTC().Format(time.RFC3339)
				}
				if sub.End != nil && (m.End == nil || sub.End.Before(*m.End)) {
					end := *sub.End
					m.End = &end
				}
			}
		case "cancelled", "expired":
			if !containsStr([]string{"cancelled", "expired"}, m.Status) {
				m.Status = ch.To
				if m.End == nil || m.End.After(now) {
					m.End = &now
				}
			}
		}
		if m.Status != from {
			m.addNote(fmt.Sprintf("Membership status changed from %s to %s because subscription #%d is now %s.",
				gmemStatusLabel(from), gmemStatusLabel(m.Status), sub.ID, strings.ToLower(gsubStatusLabel(ch.To))))
			if m.Status == "pending-cancel" {
				m.Notes[0]["prev_end"] = prevEnd // restored if the subscription is reactivated
			}
			_ = s.gmemSaveUM(ctx, m)
		}
	}
}

// gmemPrevEnd is the end date a membership had before its subscription went pending cancellation.
func gmemPrevEnd(m *gooUserMembership) *time.Time {
	for _, n := range m.Notes {
		v, ok := n["prev_end"].(string)
		if !ok {
			continue
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return &t
		}
		return nil
	}
	return m.End
}

// gmemScheduler activates delayed memberships, expires ended ones and sends the ending-soon and ended
// e-mails.
func (s *Server) gmemScheduler(ctx context.Context) {
	now := time.Now()
	soon := time.Duration(s.gooExtInt(gmemKey, "ending_soon_days")) * 24 * time.Hour
	for _, m := range s.gmemUMDue(ctx, now, soon) {
		switch {
		case m.Status == "delayed" && !m.Start.After(now):
			m.Status = "active"
			m.addNote("Membership status changed from Delayed to Active.")
			_ = s.gmemSaveUM(ctx, m)
		case m.End != nil && !m.End.After(now) && containsStr([]string{"active", "complimentary", "paused"}, m.Status):
			m.addNote("Membership status changed from " + gmemStatusLabel(m.Status) + " to Expired.")
			m.Status = "expired"
			_ = s.gmemSaveUM(ctx, m)
			s.gmemMail(ctx, m, "membership_ended")
		case m.End != nil && !m.End.After(now) && m.Status == "pending-cancel":
			m.Status = "cancelled"
			m.addNote("Membership status changed from Pending Cancellation to Cancelled.")
			_ = s.gmemSaveUM(ctx, m)
			s.gmemMail(ctx, m, "membership_ended")
		case soon > 0 && m.End != nil && m.End.After(now) && m.End.Sub(now) <= soon && containsStr([]string{"active", "pending-cancel"}, m.Status):
			s.gmemMail(ctx, m, "membership_ending_soon")
		}
	}
}

// gmemMail sends a membership e-mail to the member once per membership (and end date, so a renewal
// re-arms the reminder); the sent mark is a note on the membership.
func (s *Server) gmemMail(ctx context.Context, m *gooUserMembership, id string) {
	mark := id
	if m.End != nil {
		mark += "@" + m.End.UTC().Format(time.RFC3339)
	}
	for _, n := range m.Notes {
		if v, _ := n["mail"].(string); v == mark {
			return
		}
	}
	u, err := s.store.UserByID(ctx, m.UserID)
	if err != nil || u == nil || u.Email == "" {
		return
	}
	plan, err := s.gmemPlan(ctx, m.PlanID)
	if err != nil {
		return
	}
	end := ""
	if m.End != nil {
		end = s.gxaInputDate(m.End, false)
	}
	area := s.links(ctx).HomeURL("/my-account/")
	if s.gooPageID("myaccount") > 0 {
		area = s.gooPageURL(ctx, "myaccount")
	}
	body := `<p>Hi ` + gooEsc(defaultStr(u.DisplayName, u.Login)) + `,</p>`
	switch id {
	case "membership_welcome":
		body += `<p>You now have access to the <strong>` + gooEsc(plan.Name) + `</strong> membership.</p>`
	case "membership_ending_soon":
		body += `<p>Your <strong>` + gooEsc(plan.Name) + `</strong> membership ends on ` + gooEsc(end) + `. Renew it to keep your access.</p>`
	default:
		body += `<p>Your <strong>` + gooEsc(plan.Name) + `</strong> membership has ended.</p>`
	}
	body += `<p><a href="` + gooEsc(area) + `">View your memberships</a></p>`
	if s.gxSendEmail(ctx, id, []string{u.Email}, map[string]string{"plan": plan.Name, "end_date": end}, body) {
		m.addNote(fmt.Sprintf("%s e-mail sent.", defaultStr(gooEmailDefByID(id).Title, id)))
		m.Notes[0]["mail"] = mark
		_ = s.gmemSaveUM(ctx, m)
	}
}

// My Account ---------------------------------------------------------------------------------------------

// gmemAccountRender is My Account › Memberships (the Members Area): the list, or one membership's
// content, products and discounts (arg = the user membership id).
func gmemAccountRender(s *Server, c *gin.Context, arg string) template.HTML {
	u := currentUser(c)
	if u == nil || !s.gooExtOn(gmemKey) {
		return ""
	}
	ctx := c.Request.Context()
	id, _ := strconv.ParseInt(strings.Split(strings.Trim(arg, "/"), "/")[0], 10, 64)
	if id != 0 {
		m, err := s.gmemUM(ctx, id)
		if err != nil || m.UserID != u.ID {
			return template.HTML(`<div class="woocommerce-error">Invalid membership.</div>`)
		}
		return template.HTML(s.gmemMembersArea(ctx, u, m))
	}
	list := s.gmemUMList(ctx, u.ID, 0, "")
	if len(list) == 0 {
		return template.HTML(`<div class="woocommerce-message woocommerce-message--info woocommerce-Message woocommerce-Message--info woocommerce-info">There are no memberships yet.</div>`)
	}
	var b strings.Builder
	b.WriteString(`<table class="shop_table shop_table_responsive my_account_orders my_account_memberships woocommerce-MyAccount-memberships"><thead><tr>` +
		`<th class="membership-plan"><span class="nobr">Plan</span></th><th class="membership-start-date"><span class="nobr">Start</span></th>` +
		`<th class="membership-end-date"><span class="nobr">Expires</span></th><th class="membership-status"><span class="nobr">Status</span></th>` +
		`<th class="membership-actions">&nbsp;</th></tr></thead><tbody>`)
	for _, m := range list {
		plan, err := s.gmemPlan(ctx, m.PlanID)
		if err != nil {
			continue
		}
		start := m.Start
		expires := "Never"
		if m.End != nil {
			expires = s.gxaDate(m.End)
		}
		view := s.gooEndpointURL(ctx, "myaccount", "members-area", itoa64(m.ID))
		b.WriteString(`<tr class="membership"><td class="membership-plan" data-title="Plan"><a href="` + gooEsc(view) + `">` + gooEsc(plan.Name) + `</a></td>` +
			`<td class="membership-start-date" data-title="Start">` + s.gxaDate(&start) + `</td><td class="membership-end-date" data-title="Expires">` + expires + `</td>` +
			`<td class="membership-status" data-title="Status">` + gooEsc(gmemStatusLabel(m.Status)) + `</td>` +
			`<td class="membership-actions order-actions"><a href="` + gooEsc(view) + `" class="button view">View</a></td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	return template.HTML(b.String())
}

func (s *Server) gmemMembersArea(ctx context.Context, u *store.User, m *gooUserMembership) string {
	plan, err := s.gmemPlan(ctx, m.PlanID)
	if err != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="wc-memberships-members-area" data-member="` + itoa64(u.ID) + `" data-membership="` + itoa64(m.ID) + `"><h2>` + gooEsc(plan.Name) + `</h2>`)
	if !m.HasAccess(time.Now()) {
		b.WriteString(`<div class="woocommerce-info">This membership is ` + gooEsc(strings.ToLower(gmemStatusLabel(m.Status))) + `.</div>`)
	}
	// My Content.
	b.WriteString(`<section class="my-membership-content"><h3>My Content</h3>`)
	var rows strings.Builder
	for _, id := range plan.Rules.ContentPostIDs {
		if p, err := s.store.GetPost(ctx, id); err == nil && p.Status == "publish" {
			rows.WriteString(`<tr><td class="membership-content-title"><a href="` + gooEsc(s.links(ctx).Permalink(p)) + `">` + gooEsc(p.Title) + `</a></td><td class="membership-content-type">` + gooEsc(ucfirstStr(p.Type)) + `</td><td class="membership-content-accessible">Now</td></tr>`)
		}
	}
	if rows.Len() == 0 {
		b.WriteString(`<p>There is no content assigned to this membership.</p>`)
	} else {
		b.WriteString(`<table class="shop_table shop_table_responsive my_account_orders my_membership_content"><thead><tr><th>Title</th><th>Type</th><th>Accessible</th></tr></thead><tbody>` + rows.String() + `</tbody></table>`)
	}
	b.WriteString(`</section>`)
	// My Products.
	b.WriteString(`<section class="my-membership-products"><h3>My Products</h3>`)
	products, _ := s.gooGetProducts(ctx, plan.Rules.ProductIDs)
	rows.Reset()
	for _, id := range plan.Rules.ProductIDs {
		if p := products[id]; p != nil && p.Status == "publish" {
			rows.WriteString(`<tr><td class="membership-product-title"><a href="` + gooEsc(s.gooPermalink(ctx, p)) + `">` + gooEsc(p.Name) + `</a></td><td class="membership-product-price">` + string(s.gooPrice(s.gooDisplayPrice(ctx, p))) + `</td></tr>`)
		}
	}
	if rows.Len() == 0 {
		b.WriteString(`<p>There are no products assigned to this membership.</p>`)
	} else {
		b.WriteString(`<table class="shop_table shop_table_responsive my_account_orders my_membership_products"><thead><tr><th>Title</th><th>Price</th></tr></thead><tbody>` + rows.String() + `</tbody></table>`)
	}
	b.WriteString(`</section>`)
	// My Discounts.
	b.WriteString(`<section class="my-membership-discounts"><h3>My Discounts</h3>`)
	if d := s.gmemDiscountsHTML(ctx, u); d != "" && m.HasAccess(time.Now()) {
		b.WriteString(d)
	} else {
		b.WriteString(`<p>There are no discounts available for this membership.</p>`)
	}
	b.WriteString(`</section>`)
	// Manage.
	start := m.Start
	expires := "Never"
	if m.End != nil {
		expires = s.gxaDate(m.End)
	}
	b.WriteString(`<section class="my-membership-details"><h3>Manage</h3><table class="shop_table shop_table_responsive my_account_orders my_membership_settings"><tbody>` +
		`<tr><th>Status</th><td>` + gooEsc(gmemStatusLabel(m.Status)) + `</td></tr><tr><th>Start date</th><td>` + s.gxaDate(&start) + `</td></tr>` +
		`<tr><th>Expires</th><td>` + expires + `</td></tr></tbody></table></section></div>`)
	return b.String()
}

// Admin: members --------------------------------------------------------------------------------------------

func (s *Server) gmemAdminMembers(c *gin.Context) {
	ctx := c.Request.Context()
	if c.Request.Method == "POST" {
		if !s.checkNonce(c, "gmem-members") {
			s.denyNonce(c)
			return
		}
		switch c.PostForm("member_action") {
		case "add":
			u, err := s.gxaFindUser(ctx, c.PostForm("member"))
			if err != nil {
				gxaAdminRedirect(c, "memberships", "action=new", "No user found with that ID, username or email.", true)
				return
			}
			planID, _ := strconv.ParseInt(c.PostForm("plan_id"), 10, 64)
			plan, err := s.gmemPlan(ctx, planID)
			if err != nil {
				gxaAdminRedirect(c, "memberships", "action=new", "Please choose a membership plan.", true)
				return
			}
			for _, m := range s.gmemUMList(ctx, u.ID, plan.ID, "") {
				if m.HasAccess(time.Now()) {
					gxaAdminRedirect(c, "memberships", "action=edit&id="+itoa64(m.ID), "This user is already a member of "+plan.Name+".", true)
					return
				}
			}
			m := &gooUserMembership{UserID: u.ID, PlanID: plan.ID, Status: "active"}
			if st := c.PostForm("status"); st != "" {
				if !gmemValidStatus(st) {
					gxaAdminRedirect(c, "memberships", "action=new", "Invalid membership status.", true)
					return
				}
				m.Status = st
			}
			if st := s.gxaParseTime(c.PostForm("start_date")); st != nil {
				m.Start = *st
				if st.After(time.Now()) {
					m.Status = "delayed"
				}
			} else {
				m.Start = time.Now()
			}
			m.End = plan.endFor(m.Start)
			if ed := c.PostForm("end_date"); ed != "" {
				m.End = s.gxaParseTime(ed)
			}
			m.addNote("Membership created manually by " + defaultStr(currentUser(c).DisplayName, "an administrator") + ".")
			if err := s.gmemSaveUM(ctx, m); err != nil {
				gxaAdminRedirect(c, "memberships", "action=new", err.Error(), true)
				return
			}
			gxaAdminRedirect(c, "memberships", "action=edit&id="+itoa64(m.ID), "Membership created.", false)
			return
		case "edit", "cancel":
			id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
			m, err := s.gmemUM(ctx, id)
			if err != nil {
				gxaAdminRedirect(c, "memberships", "", "Membership not found.", true)
				return
			}
			to := c.PostForm("status")
			if c.PostForm("member_action") == "cancel" {
				to = "cancelled"
			}
			valid := false
			for _, st := range gmemStatuses {
				valid = valid || st.Value == to
			}
			if valid && to != m.Status {
				m.addNote("Membership status changed from " + gmemStatusLabel(m.Status) + " to " + gmemStatusLabel(to) + ".")
				m.Status = to
				if to == "cancelled" {
					now := time.Now()
					m.End = &now
				}
			}
			if c.PostForm("member_action") == "edit" {
				if c.PostForm("end_never") != "" {
					m.End = nil
				} else if ed := c.PostForm("end_date"); ed != "" {
					m.End = s.gxaParseTime(ed)
				}
				if note := strings.TrimSpace(c.PostForm("note")); note != "" {
					m.addNote(gooEsc(note))
				}
			}
			if err := s.gmemSaveUM(ctx, m); err != nil {
				gxaAdminRedirect(c, "memberships", "action=edit&id="+itoa64(m.ID), err.Error(), true)
				return
			}
			gxaAdminRedirect(c, "memberships", "action=edit&id="+itoa64(m.ID), "Membership updated.", false)
			return
		}
		gxaAdminRedirect(c, "memberships", "", "", false)
		return
	}
	nonce := s.nonce(c, "gmem-members")
	plans := s.gmemPlans(ctx)
	planOpts := []gooOpt{{"", "Select a plan"}}
	planName := map[int64]string{}
	for _, p := range plans {
		planOpts = append(planOpts, gooOpt{itoa64(p.ID), p.Name})
		planName[p.ID] = p.Name
	}
	var b strings.Builder
	switch c.Query("action") {
	case "new":
		b.WriteString(`<form method="post" action="/wp-admin/goocommerce?page=memberships"><input type="hidden" name="_wpnonce" value="` + nonce + `"><input type="hidden" name="member_action" value="add">` +
			`<table class="form-table"><tbody><tr><th scope="row"><label for="member">Member</label></th><td><input type="text" class="regular-text" id="member" name="member" placeholder="User ID, username or email" required></td></tr>` +
			`<tr><th scope="row"><label for="plan_id">Plan</label></th><td>` + gxaSelect("plan_id", "plan_id", planOpts, c.Query("plan"), "") + `</td></tr>` +
			`<tr><th scope="row"><label for="status">Status</label></th><td>` + gxaSelect("status", "status", gmemStatuses, "active", "") + `</td></tr>` +
			`<tr><th scope="row"><label for="start_date">Member since</label></th><td><input type="date" id="start_date" name="start_date"></td></tr>` +
			`<tr><th scope="row"><label for="end_date">Expires</label></th><td><input type="date" id="end_date" name="end_date"><p class="description">Leave empty to use the plan's access length.</p></td></tr>` +
			`</tbody></table><p class="submit"><button type="submit" class="button button-primary">Add Member</button></p></form>`)
		s.gxaAdminRender(c, "memberships", "Add Member", "goo-ext-memberships", `<h1 class="wp-heading-inline">Add Member</h1>`, b.String())
		return
	case "edit":
		id, _ := strconv.ParseInt(c.Query("id"), 10, 64)
		m, err := s.gmemUM(ctx, id)
		if err != nil {
			gxaAdminRedirect(c, "memberships", "", "Membership not found.", true)
			return
		}
		start := m.Start
		b.WriteString(`<form method="post" action="/wp-admin/goocommerce?page=memberships"><input type="hidden" name="_wpnonce" value="` + nonce + `"><input type="hidden" name="member_action" value="edit"><input type="hidden" name="id" value="` + itoa64(m.ID) + `">` +
			`<div id="poststuff"><div class="postbox"><h2 class="hndle"><span>Membership details</span></h2><div class="inside"><table class="form-table"><tbody>` +
			`<tr><th>Member</th><td>` + s.gxaUserLabel(ctx, m.UserID, true) + `</td></tr><tr><th>Plan</th><td>` + gooEsc(planName[m.PlanID]) + `</td></tr>` +
			`<tr><th><label for="status">Status</label></th><td>` + gxaSelect("status", "status", gmemStatuses, m.Status, "") + `</td></tr>` +
			`<tr><th>Member since</th><td>` + s.gxaDate(&start) + `</td></tr>` +
			`<tr><th><label for="end_date">Expires</label></th><td><input type="date" id="end_date" name="end_date" value="` + s.gxaInputDate(m.End, false) + `"> <label><input type="checkbox" name="end_never" value="1"> Never</label></td></tr>` +
			`<tr><th>Order</th><td>` + gxaOrderLink(m.OrderID) + `</td></tr>` +
			`<tr><th><label for="note">Add note</label></th><td><textarea id="note" name="note" rows="3" class="large-text"></textarea></td></tr>` +
			`</tbody></table></div></div></div><p class="submit"><button type="submit" class="button button-primary">Save Member</button></p></form>`)
		b.WriteString(`<form method="post" action="/wp-admin/goocommerce?page=memberships"><input type="hidden" name="_wpnonce" value="` + nonce + `"><input type="hidden" name="member_action" value="cancel"><input type="hidden" name="id" value="` + itoa64(m.ID) + `"><button type="submit" class="button cancel-membership">Cancel Membership</button></form>`)
		b.WriteString(`<h2>Membership notes</h2><ul class="order_notes">`)
		for _, n := range m.Notes {
			var when *time.Time
			if t, err := time.Parse(time.RFC3339, gxaStr(n, "date")); err == nil {
				when = &t
			}
			b.WriteString(`<li class="note"><div class="note_content"><p>` + gxaStr(n, "content") + `</p></div><p class="meta">` + s.gxaDateTime(when) + `</p></li>`)
		}
		b.WriteString(`</ul>`)
		s.gxaAdminRender(c, "memberships", "Edit Membership", "goo-ext-memberships",
			`<h1 class="wp-heading-inline">Edit Membership</h1> <a href="/wp-admin/goocommerce?page=memberships" class="page-title-action">&larr; Members</a>`, b.String())
		return
	}
	status := c.Query("status")
	planID, _ := strconv.ParseInt(c.Query("plan"), 10, 64)
	search := strings.ToLower(strings.TrimSpace(c.Query("s")))
	all := s.gmemUMList(ctx, 0, planID, "")
	counts := map[string]int{}
	var list []*gooUserMembership
	for _, m := range all {
		counts[m.Status]++
		if status != "" && m.Status != status {
			continue
		}
		if search != "" {
			u, err := s.store.UserByID(ctx, m.UserID)
			if err != nil || !(strings.Contains(strings.ToLower(u.Login+" "+u.Email+" "+u.DisplayName), search)) {
				continue
			}
		}
		list = append(list, m)
	}
	base := "/wp-admin/goocommerce?page=memberships"
	if planID != 0 {
		base += "&plan=" + itoa64(planID)
	}
	b.WriteString(gxaViews(base, status, gmemStatuses, counts))
	b.WriteString(`<form method="get" action="/wp-admin/goocommerce"><input type="hidden" name="page" value="memberships"><p class="search-box"><input type="search" name="s" value="` + gooEsc(search) + `"><input type="submit" class="button" value="Search Members"></p>` +
		`<div class="tablenav top"><div class="alignleft actions">` + gxaSelect("plan", "filter-by-plan", append([]gooOpt{{"", "All plans"}}, planOpts[1:]...), c.Query("plan"), "") +
		` <input type="submit" class="button" value="Filter"></div></div></form>`)
	b.WriteString(`<table class="wp-list-table widefat fixed striped posts"><thead><tr><th class="manage-column column-title column-primary">Member</th><th class="manage-column column-plan">Plan</th>` +
		`<th class="manage-column column-status">Status</th><th class="manage-column column-member-since">Member since</th><th class="manage-column column-expires">Expires</th></tr></thead><tbody id="the-list">`)
	if len(list) == 0 {
		b.WriteString(`<tr class="no-items"><td class="colspanchange" colspan="5">No Members found.</td></tr>`)
	}
	for _, m := range list {
		start := m.Start
		expires := "Never"
		if m.End != nil {
			expires = s.gxaDate(m.End)
		}
		edit := base + "&action=edit&id=" + itoa64(m.ID)
		b.WriteString(`<tr><td class="title column-title column-primary"><strong><a href="` + gooEsc(edit) + `">` + s.gxaUserLabel(ctx, m.UserID, false) + `</a></strong>` +
			`<div class="row-actions"><span class="edit"><a href="` + gooEsc(edit) + `">Edit</a></span></div></td><td class="plan column-plan">` + gooEsc(planName[m.PlanID]) + `</td>` +
			`<td class="status column-status"><mark class="membership-status status-` + gooEsc(m.Status) + `"><span>` + gooEsc(gmemStatusLabel(m.Status)) + `</span></mark></td>` +
			`<td class="member-since column-member-since">` + s.gxaDate(&start) + `</td><td class="expires column-expires">` + expires + `</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	s.gxaAdminRender(c, "memberships", "Members", "goo-ext-memberships",
		`<h1 class="wp-heading-inline">Members</h1> <a href="/wp-admin/goocommerce?page=memberships&amp;action=new" class="page-title-action">Add Member</a>`, b.String())
}

// Admin: plans ----------------------------------------------------------------------------------------------

func (s *Server) gmemAdminPlans(c *gin.Context) {
	ctx := c.Request.Context()
	if c.Request.Method == "GET" && c.Query("gmem_search") != "" {
		s.gmemSearchJSON(c)
		return
	}
	if c.Request.Method == "POST" {
		if !s.checkNonce(c, "gmem-plans") {
			s.denyNonce(c)
			return
		}
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		if c.PostForm("plan_action") == "delete" {
			if len(s.gmemUMList(ctx, 0, id, "")) > 0 {
				gxaAdminRedirect(c, "membership-plans", "", "A plan with members can not be deleted.", true)
				return
			}
			_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_membership_plans WHERE id = $1`, id)
			gxaAdminRedirect(c, "membership-plans", "", "Membership plan deleted.", false)
			return
		}
		p := &gooMembershipPlan{}
		if id != 0 {
			var err error
			if p, err = s.gmemPlan(ctx, id); err != nil {
				gxaAdminRedirect(c, "membership-plans", "", "Membership plan not found.", true)
				return
			}
		}
		err := s.gmemReadPlanForm(c, p)
		if err == nil {
			err = s.gmemSavePlan(ctx, p)
		}
		if err != nil {
			params := "action=new"
			if p.ID != 0 {
				params = "action=edit&id=" + itoa64(p.ID)
			}
			gxaAdminRedirect(c, "membership-plans", params, err.Error(), true)
			return
		}
		if c.PostForm("grant_existing") != "" && p.AccessMethod == "purchase" {
			n := s.gmemGrantPastPurchases(ctx, p)
			gxaAdminRedirect(c, "membership-plans", "action=edit&id="+itoa64(p.ID), fmt.Sprintf("Membership plan saved. %d existing customers granted access.", n), false)
			return
		}
		gxaAdminRedirect(c, "membership-plans", "action=edit&id="+itoa64(p.ID), "Membership plan saved.", false)
		return
	}
	nonce := s.nonce(c, "gmem-plans")
	var b strings.Builder
	if a := c.Query("action"); a == "new" || a == "edit" {
		p := &gooMembershipPlan{AccessMethod: "manual", AccessLength: "unlimited", AccessPeriod: "months", Rules: gmemRules{ProductRestrict: "purchase"}}
		title := "Add Membership Plan"
		if a == "edit" {
			id, _ := strconv.ParseInt(c.Query("id"), 10, 64)
			var err error
			if p, err = s.gmemPlan(ctx, id); err != nil {
				gxaAdminRedirect(c, "membership-plans", "", "Membership plan not found.", true)
				return
			}
			title = "Edit Membership Plan"
		}
		b.WriteString(s.gmemPlanForm(p, nonce))
		s.gxaAdminRender(c, "membership-plans", title, "goo-ext-memberships",
			`<h1 class="wp-heading-inline">`+title+`</h1> <a href="/wp-admin/goocommerce?page=membership-plans" class="page-title-action">&larr; Membership Plans</a>`, b.String())
		return
	}
	plans := s.gmemPlans(ctx)
	b.WriteString(`<table class="wp-list-table widefat fixed striped posts"><thead><tr><th class="manage-column column-title column-primary">Name</th><th class="manage-column column-slug">Slug</th>` +
		`<th class="manage-column column-access">Access from</th><th class="manage-column column-length">Length</th><th class="manage-column column-members">Members</th></tr></thead><tbody id="the-list">`)
	if len(plans) == 0 {
		b.WriteString(`<tr class="no-items"><td class="colspanchange" colspan="5">No Membership Plans found.</td></tr>`)
	}
	for _, p := range plans {
		var n int
		_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM goo_user_memberships WHERE plan_id = $1 AND status = ANY($2)`, p.ID, gmemAccessStatuses).Scan(&n)
		edit := "/wp-admin/goocommerce?page=membership-plans&action=edit&id=" + itoa64(p.ID)
		access := map[string]string{"manual": "Manual assignment only", "purchase": "Product(s) purchase", "signup": "User account registration"}[p.AccessMethod]
		b.WriteString(`<tr><td class="title column-title column-primary"><strong><a class="row-title" href="` + gooEsc(edit) + `">` + gooEsc(p.Name) + `</a></strong>` +
			`<div class="row-actions"><span class="edit"><a href="` + gooEsc(edit) + `">Edit</a> | </span><span class="members"><a href="/wp-admin/goocommerce?page=memberships&amp;plan=` + itoa64(p.ID) + `">Members</a> | </span>` +
			`<span class="trash"><form method="post" style="display:inline" action="/wp-admin/goocommerce?page=membership-plans"><input type="hidden" name="_wpnonce" value="` + nonce + `"><input type="hidden" name="plan_action" value="delete"><input type="hidden" name="id" value="` + itoa64(p.ID) + `"><button type="submit" class="button-link submitdelete">Delete</button></form></span></div></td>` +
			`<td class="slug column-slug">` + gooEsc(p.Slug) + `</td><td class="access column-access">` + access + `</td><td class="length column-length">` + s.gmemAccessLabel(p) + `</td>` +
			`<td class="members column-members"><a href="/wp-admin/goocommerce?page=memberships&amp;plan=` + itoa64(p.ID) + `">` + strconv.Itoa(n) + `</a></td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	s.gxaAdminRender(c, "membership-plans", "Membership Plans", "goo-ext-memberships",
		`<h1 class="wp-heading-inline">Membership Plans</h1> <a href="/wp-admin/goocommerce?page=membership-plans&amp;action=new" class="page-title-action">Add New</a>`, b.String())
}

func (s *Server) gmemPlanForm(p *gooMembershipPlan, nonce string) string {
	var b strings.Builder
	row := func(label, id, field, desc string) {
		b.WriteString(`<tr><th scope="row"><label for="` + id + `">` + label + `</label></th><td>` + field)
		if desc != "" {
			b.WriteString(`<p class="description">` + desc + `</p>`)
		}
		b.WriteString(`</td></tr>`)
	}
	b.WriteString(`<form method="post" action="/wp-admin/goocommerce?page=membership-plans" id="post"><input type="hidden" name="_wpnonce" value="` + nonce + `"><input type="hidden" name="id" value="` + itoa64(p.ID) + `">`)
	b.WriteString(`<div id="titlediv"><input type="text" name="post_title" size="30" value="` + gooEsc(p.Name) + `" id="title" placeholder="Plan name" required></div>`)
	b.WriteString(`<div id="poststuff"><div class="postbox" id="wc-memberships-membership-plan-data"><h2 class="hndle"><span>Membership Plan Data</span></h2><div class="inside">`)
	b.WriteString(`<h3>General</h3><table class="form-table"><tbody>`)
	row("Slug", "post_name", `<input type="text" id="post_name" name="post_name" value="`+gooEsc(p.Slug)+`" class="regular-text">`, "Used in shortcodes: [goocommerce_members_only plans=\"slug\"].")
	row("Grant access upon", "_access_method", gxaSelect("_access_method", "_access_method", []gooOpt{{"manual", "Manual assignment only"}, {"signup", "User account registration"}, {"purchase", "Product(s) purchase"}}, p.AccessMethod, ""), "")
	row("Products", "_product_ids", `<input type="text" id="_product_ids" name="_product_ids" data-gmem-search="product" value="`+gxaJoinIDs(p.ProductIDs)+`" class="regular-text" placeholder="Product IDs, comma separated">`,
		`Leave empty to only allow members you manually assign. <label><input type="checkbox" name="grant_existing" value="1"> Grant access to customers who already purchased these products</label>`)
	amount := ""
	if p.AccessAmount > 0 {
		amount = strconv.Itoa(p.AccessAmount)
	}
	row("Membership length", "_access_length",
		gxaSelect("_access_length", "_access_length", []gooOpt{{"unlimited", "unlimited"}, {"specific", "specific length"}, {"fixed", "fixed dates"}}, p.AccessLength, "")+
			` <input type="number" min="1" name="_access_length_amount" value="`+amount+`" style="width:70px"> `+
			gxaSelect("_access_length_period", "_access_length_period", []gooOpt{{"days", "day(s)"}, {"weeks", "week(s)"}, {"months", "month(s)"}, {"years", "year(s)"}}, p.AccessPeriod, "")+
			` <input type="date" name="_access_end_date" value="`+s.gxaInputDate(p.AccessEnd, false)+`">`, "Specific length: the amount and period. Fixed dates: the end date.")
	b.WriteString(`</tbody></table><h3>Restrict Content</h3><table class="form-table"><tbody>`)
	row("Posts and pages", "_content_post_ids", `<input type="text" id="_content_post_ids" name="_content_post_ids" data-gmem-search="post" value="`+gxaJoinIDs(p.Rules.ContentPostIDs)+`" class="regular-text" placeholder="Post / page IDs">`, "Content that is restricted to members of this plan.")
	row("Categories and tags", "_content_term_ids", `<input type="text" id="_content_term_ids" name="_content_term_ids" data-gmem-search="term" value="`+gxaJoinIDs(p.Rules.ContentTermIDs)+`" class="regular-text" placeholder="Term IDs (category, tag, product category)">`, "Every post in these terms is restricted.")
	row("Drip schedule", "_content_drip", `<input type="text" id="_content_drip" name="_content_drip" value="`+gooEsc(gmemDripString(p.Rules.ContentDrip))+`" class="regular-text" placeholder="123:7, t45:30">`,
		"Opens content only after a number of days of membership: <code>post ID:days</code> or <code>t + term ID:days</code>, comma separated. Content not listed opens immediately.")
	row("Restriction message", "_restriction_message", `<textarea id="_restriction_message" name="_restriction_message" rows="3" class="large-text">`+gooEsc(p.Message)+`</textarea>`, "Use a custom message for this plan's restricted content. Leave empty for the default message. Merge tags: {products}, {login_url}.")
	b.WriteString(`</tbody></table><h3>Restrict Products</h3><table class="form-table"><tbody>`)
	row("Member-only products", "_restricted_product_ids", `<input type="text" id="_restricted_product_ids" name="_restricted_product_ids" data-gmem-search="product" value="`+gxaJoinIDs(p.Rules.ProductIDs)+`" class="regular-text" placeholder="Product IDs">`, "")
	row("Member-only product categories", "_restricted_product_terms", `<input type="text" id="_restricted_product_terms" name="_restricted_product_terms" data-gmem-search="term" value="`+gxaJoinIDs(p.Rules.ProductTermIDs)+`" class="regular-text" placeholder="Product category IDs">`, "")
	row("Restrict", "_product_restrict", gxaSelect("_product_restrict", "_product_restrict", []gooOpt{{"purchase", "Purchasing"}, {"view", "Viewing (and purchasing)"}}, defaultStr(p.Rules.ProductRestrict, "purchase"), ""), "")
	b.WriteString(`</tbody></table><h3>Purchasing Discounts</h3><table class="widefat wc-memberships-rules discounts"><thead><tr><th>Type</th><th>Product / category ID</th><th>Discount</th><th>Amount</th></tr></thead><tbody>`)
	discounts := append([]gmemDiscount{}, p.Rules.Discounts...)
	for len(discounts) < len(p.Rules.Discounts)+3 {
		discounts = append(discounts, gmemDiscount{Target: "product", Type: "percent"})
	}
	for _, d := range discounts {
		id, amt := "", ""
		if d.ID != 0 {
			id = itoa64(d.ID)
		}
		if d.Amount != 0 {
			amt = strconv.FormatFloat(d.Amount, 'f', -1, 64)
		}
		b.WriteString(`<tr><td>` + gxaSelect("discount_target[]", "", []gooOpt{{"product", "Product"}, {"product_cat", "Product category"}}, d.Target, "") + `</td>` +
			`<td><input type="number" name="discount_id[]" value="` + id + `" style="width:100px"></td>` +
			`<td>` + gxaSelect("discount_type[]", "", []gooOpt{{"percent", "%"}, {"fixed", "Fixed amount"}}, d.Type, "") + `</td>` +
			`<td><input type="text" name="discount_amount[]" value="` + amt + `" style="width:100px"></td></tr>`)
	}
	b.WriteString(`</tbody></table></div></div></div><p class="submit"><button type="submit" class="button button-primary">Save Plan</button></p></form>`)
	b.WriteString(gmemSearchScript)
	return b.String()
}

// gmemReadPlanForm reads the plan editor.
func (s *Server) gmemReadPlanForm(c *gin.Context, p *gooMembershipPlan) error {
	p.Name = strings.TrimSpace(c.PostForm("post_title"))
	p.Slug = strings.TrimSpace(c.PostForm("post_name"))
	p.AccessMethod = c.PostForm("_access_method")
	p.ProductIDs = gxaParseIDs(c.PostForm("_product_ids"))
	p.AccessLength = c.PostForm("_access_length")
	p.AccessAmount, _ = strconv.Atoi(c.PostForm("_access_length_amount"))
	p.AccessPeriod = defaultStr(c.PostForm("_access_length_period"), "months")
	p.AccessEnd = s.gxaParseTime(c.PostForm("_access_end_date"))
	if p.AccessLength == "specific" && p.AccessAmount <= 0 {
		p.AccessLength = "unlimited"
	}
	if p.AccessLength == "fixed" && p.AccessEnd == nil {
		p.AccessLength = "unlimited"
	}
	p.Message = gooKsesTextarea(currentUser(c), strings.TrimSpace(c.PostForm("_restriction_message"))) // printed unescaped
	p.Rules.ContentPostIDs = gxaParseIDs(c.PostForm("_content_post_ids"))
	p.Rules.ContentTermIDs = gxaParseIDs(c.PostForm("_content_term_ids"))
	p.Rules.ProductIDs = gxaParseIDs(c.PostForm("_restricted_product_ids"))
	p.Rules.ProductTermIDs = gxaParseIDs(c.PostForm("_restricted_product_terms"))
	p.Rules.ProductRestrict = defaultStr(c.PostForm("_product_restrict"), "purchase")
	targets, ids, types, amounts := c.PostFormArray("discount_target[]"), c.PostFormArray("discount_id[]"), c.PostFormArray("discount_type[]"), c.PostFormArray("discount_amount[]")
	p.Rules.Discounts = nil
	for i := range ids {
		id, _ := strconv.ParseInt(strings.TrimSpace(ids[i]), 10, 64)
		var amount float64
		if i < len(amounts) {
			if v := s.gooParseAmount(amounts[i]); v != nil {
				amount = *v
			}
		}
		if id <= 0 || amount <= 0 {
			continue
		}
		d := gmemDiscount{ID: id, Amount: amount, Target: "product", Type: "percent"}
		if i < len(targets) && targets[i] == "product_cat" {
			d.Target = "product_cat"
		}
		if i < len(types) && types[i] == "fixed" {
			d.Type = "fixed"
		}
		p.Rules.Discounts = append(p.Rules.Discounts, d)
	}
	drip, err := gmemParseDrip(c.PostForm("_content_drip"))
	if err != nil {
		return err
	}
	p.Rules.ContentDrip = drip
	return s.gmemValidateRefs(c.Request.Context(), p)
}

// gmemGrantPastPurchases grants a purchase plan to the customers of earlier paid orders.
func (s *Server) gmemGrantPastPurchases(ctx context.Context, p *gooMembershipPlan) int {
	if len(p.ProductIDs) == 0 {
		return 0
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT DISTINCT o.id FROM goo_orders o JOIN goo_order_items i ON i.order_id = o.id
		WHERE o.type = 'shop_order' AND o.status = ANY($1) AND o.customer_id <> 0 AND i.type = 'line_item' AND (i.product_id = ANY($2) OR i.variation_id = ANY($2))`,
		gooPaidStatuses, p.ProductIDs)
	if err != nil {
		return 0
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		if o, err := s.gooGetOrder(ctx, id); err == nil {
			before := len(s.gmemUMList(ctx, o.CustomerID, p.ID, ""))
			s.gmemGrantFromOrder(ctx, o)
			if len(s.gmemUMList(ctx, o.CustomerID, p.ID, "")) > before {
				n++
			}
		}
	}
	return n
}

// Restriction modes ---------------------------------------------------------------------------------------

// gmemModeRmode is the content restriction mode: "hide", "hide_content" or "redirect" (redirect without
// a page falls back to "hide_content").
func (s *Server) gmemModeRmode() string {
	mode := defaultStr(s.gooExtString(gmemKey, "restriction_mode"), "hide_content")
	if mode == "redirect" && s.gooExtInt(gmemKey, "redirect_page_id") <= 0 {
		return "hide_content"
	}
	return mode
}

// gmemPlanItemsRmode are the ids a plan restricts: its content (posts and the posts of its terms) for
// kind "post", its member-only products for kind "product" (view-restricted only unless all is set).
func (s *Server) gmemPlanItemsRmode(ctx context.Context, p *gooMembershipPlan, kind string, all bool) []int64 {
	var ids, terms []int64
	switch kind {
	case "post":
		ids, terms = append(ids, p.Rules.ContentPostIDs...), p.Rules.ContentTermIDs
	case "product":
		if !all && defaultStr(p.Rules.ProductRestrict, "purchase") != "view" {
			return nil
		}
		ids, terms = append(ids, p.Rules.ProductIDs...), p.Rules.ProductTermIDs
	}
	if len(terms) > 0 {
		rows, err := s.store.Pool.Query(ctx, `SELECT DISTINCT post_id FROM post_terms WHERE term_id = ANY($1)`, terms)
		if err == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
		}
	}
	return ids
}

// gmemListingExcludeRmode hides restricted content and view-restricted products from the listings of
// non-members in the "hide completely" mode (and member-only products when "Restricted products" hides
// them). Items a plan of the visitor's also covers stay listed.
func gmemListingExcludeRmode(s *Server, ctx context.Context, kind string) []int64 {
	if !s.gooExtOn(gmemKey) {
		return nil
	}
	hideAllProducts := kind == "product" && s.gooExtBool(gmemKey, "hide_restricted_products")
	if s.gmemModeRmode() != "hide" && !hideAllProducts {
		return nil
	}
	u := gxaUser(ctx)
	if gmemIsManager(u) {
		return nil
	}
	var have map[int64]bool
	if u != nil {
		have = s.gmemAccessPlans(ctx, u.ID)
	}
	hidden, allowed := map[int64]bool{}, map[int64]bool{}
	for _, p := range s.gmemPlans(ctx) {
		for _, id := range s.gmemPlanItemsRmode(ctx, p, kind, hideAllProducts) {
			if have[p.ID] {
				allowed[id] = true
			} else {
				hidden[id] = true
			}
		}
	}
	var out []int64
	for id := range hidden {
		if !allowed[id] {
			out = append(out, id)
		}
	}
	return out
}

// gmemSingleAccessRmode answers a non-member's request for restricted content or a view-restricted
// product: 404 in the "hide completely" mode, the redirect page (with the requested URL in
// redirect_to) in the "redirect to page" mode.
func gmemSingleAccessRmode(s *Server, c *gin.Context, postID int64, postType string) string {
	if !s.gooExtOn(gmemKey) {
		return ""
	}
	ctx := c.Request.Context()
	u := currentUser(c)
	mode := s.gmemModeRmode()
	var plans []*gooMembershipPlan
	if postType == "product" {
		p, err := s.gooGetProduct(ctx, postID)
		if err != nil {
			return ""
		}
		if s.gooExtBool(gmemKey, "hide_restricted_products") {
			if !s.gmemCanView(ctx, u, p) {
				return "404" // hidden from the shop: the page is gone too
			}
		}
		plans = s.gmemProductPlans(ctx, p, "view")
		plans = append(plans, s.gmemPostPlans(ctx, postID)...)
	} else {
		plans = s.gmemPostPlans(ctx, postID)
	}
	if ok, at := s.gmemPostAccess(ctx, u, postID, plans); ok || at != nil {
		return "" // a member waiting for scheduled content sees the "available on" message
	}
	switch mode {
	case "hide":
		return "404"
	case "redirect":
		id := int64(s.gooExtInt(gmemKey, "redirect_page_id"))
		if id == postID {
			return ""
		}
		page, err := s.store.GetPost(ctx, id)
		if err != nil || page.Status != "publish" {
			return ""
		}
		target := s.links(ctx).Permalink(page)
		orig := s.links(ctx).HomeURL(c.Request.URL.RequestURI())
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		return target + sep + "redirect_to=" + url.QueryEscape(orig)
	}
	return ""
}

// Plan form helpers ---------------------------------------------------------------------------------------

// gmemParseDrip reads "123:7, t45:30" (post id or t+term id, days) into the drip rules.
func gmemParseDrip(v string) (map[string]int, error) {
	out := map[string]int{}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, d, ok := strings.Cut(part, ":")
		k, d = strings.TrimSpace(k), strings.TrimSpace(d)
		days, err := strconv.Atoi(d)
		id, err2 := strconv.ParseInt(strings.TrimPrefix(k, "t"), 10, 64)
		if !ok || err != nil || err2 != nil || days < 0 || id <= 0 {
			return nil, fmt.Errorf("Invalid drip schedule entry %q: use post ID:days or tTERMID:days.", part)
		}
		key := strconv.FormatInt(id, 10)
		if strings.HasPrefix(k, "t") {
			key = "t" + key
		}
		out[key] = days
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// gmemDripString is the drip rules as the form field shows them.
func gmemDripString(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+":"+strconv.Itoa(m[k]))
	}
	return strings.Join(parts, ", ")
}

// gmemValidateRefs rejects a plan that names products, posts or terms that do not exist, so a mistyped
// id is not accepted silently.
func (s *Server) gmemValidateRefs(ctx context.Context, p *gooMembershipPlan) error {
	var products, posts, terms []int64
	products = append(products, p.ProductIDs...)
	products = append(products, p.Rules.ProductIDs...)
	posts = append(posts, p.Rules.ContentPostIDs...)
	terms = append(terms, p.Rules.ContentTermIDs...)
	terms = append(terms, p.Rules.ProductTermIDs...)
	for k := range p.Rules.ContentDrip {
		if strings.HasPrefix(k, "t") {
			id, _ := strconv.ParseInt(k[1:], 10, 64)
			terms = append(terms, id)
		} else {
			id, _ := strconv.ParseInt(k, 10, 64)
			posts = append(posts, id)
		}
	}
	for _, d := range p.Rules.Discounts {
		if d.Target == "product_cat" {
			terms = append(terms, d.ID)
		} else {
			products = append(products, d.ID)
		}
	}
	missing := func(what string, ids []int64, exists func([]int64) map[int64]bool) error {
		have := exists(ids)
		var bad []string
		for _, id := range ids {
			if !have[id] {
				bad = append(bad, itoa64(id))
				have[id] = true
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("Unknown %s ID(s): %s.", what, strings.Join(bad, ", "))
		}
		return nil
	}
	postExists := func(types ...string) func([]int64) map[int64]bool {
		return func(ids []int64) map[int64]bool {
			out := map[int64]bool{}
			q := `SELECT id FROM posts WHERE id = ANY($1)`
			args := []any{ids}
			if len(types) > 0 {
				q += ` AND type = ANY($2)`
				args = append(args, types)
			}
			if rows, err := s.store.Pool.Query(ctx, q, args...); err == nil {
				for rows.Next() {
					var id int64
					if rows.Scan(&id) == nil {
						out[id] = true
					}
				}
				rows.Close()
			}
			return out
		}
	}
	if err := missing("product", products, postExists("product", "product_variation")); err != nil {
		return err
	}
	if err := missing("post", posts, postExists()); err != nil {
		return err
	}
	return missing("term", terms, func(ids []int64) map[int64]bool {
		out := map[int64]bool{}
		m, _ := s.store.TermsByIDs(ctx, ids)
		for id := range m {
			out[id] = true
		}
		return out
	})
}

// gmemSearchJSON answers the plan form's search boxes: id → label of the products, posts or terms
// matching ?term=.
func (s *Server) gmemSearchJSON(c *gin.Context) {
	ctx := c.Request.Context()
	term := strings.TrimSpace(c.Query("term"))
	out := map[string]string{}
	if len(term) < 2 {
		c.JSON(http.StatusOK, out)
		return
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(term) + "%"
	var rows pgx.Rows
	var err error
	switch c.Query("gmem_search") {
	case "term":
		rows, err = s.store.Pool.Query(ctx, `SELECT id, name || ' (' || taxonomy || ')' FROM terms WHERE name ILIKE $1 OR slug ILIKE $1 ORDER BY name LIMIT 20`, like)
	case "product":
		rows, err = s.store.Pool.Query(ctx, `SELECT id, title FROM posts WHERE type IN ('product','product_variation') AND status <> 'trash' AND title ILIKE $1 ORDER BY title LIMIT 20`, like)
	default:
		rows, err = s.store.Pool.Query(ctx, `SELECT id, title || ' (' || type || ')' FROM posts WHERE type NOT IN ('product','product_variation','revision','nav_menu_item','attachment') AND status <> 'trash' AND title ILIKE $1 ORDER BY title LIMIT 20`, like)
	}
	if err == nil {
		for rows.Next() {
			var id int64
			var label string
			if rows.Scan(&id, &label) == nil {
				out[itoa64(id)] = label
			}
		}
		rows.Close()
	}
	c.JSON(http.StatusOK, out)
}

// gmemSearchScript adds a search box under each ID field of the plan form; picking a result appends
// its ID to the field.
var gmemSearchScript = `<script` + cspNonceAttr() + `>(function(){document.querySelectorAll('input[data-gmem-search]').forEach(function(f){
var kind=f.getAttribute('data-gmem-search'),box=document.createElement('input');box.type='search';box.className='regular-text';box.placeholder='Search to add\u2026';
var list=document.createElement('ul');list.className='gmem-search-results';var t;f.parentNode.insertBefore(list,f.nextSibling);f.parentNode.insertBefore(box,f.nextSibling);
box.addEventListener('input',function(){clearTimeout(t);var q=box.value.trim();list.innerHTML='';if(q.length<2)return;
t=setTimeout(function(){fetch('/wp-admin/goocommerce?page=membership-plans&gmem_search='+kind+'&term='+encodeURIComponent(q),{credentials:'same-origin'}).then(function(r){return r.json()}).then(function(d){
list.innerHTML='';Object.keys(d).forEach(function(id){var li=document.createElement('li'),a=document.createElement('a');a.href='#';a.textContent=d[id]+' (#'+id+')';
a.addEventListener('click',function(e){e.preventDefault();var ids=f.value.split(',').map(function(x){return x.trim()}).filter(Boolean);if(ids.indexOf(id)<0)ids.push(id);f.value=ids.join(', ');list.innerHTML='';box.value='';});li.appendChild(a);list.appendChild(li);});});},250);});});})();</script>`
