package server

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"wpgo/internal/store"
)

// Pursae Forum levels, badges and member statistics (WP7).
//
// Points: every published, non-spam topic is worth TopicPoints and every reply ReplyPoints
// (3 and 1 by default). A member's level is the highest level whose Min the points reach. Badges are
// earned automatically by rule (topics, replies, posts, points, membership days, verified email,
// Google linked, moderator) or awarded by hand (users.meta forum_badges: [ids]). Nothing is stored per
// post: the statistics come from one grouped query for every author on a page (forumMemberStats), and
// levels and badges are computed from them on the fly.
//
//   option pursae_forum_levels  forumLevels  {"topic_points":3,"reply_points":1,"levels":[…]}
//   option pursae_forum_badges  []forumBadge (list order = display order)
//   users.meta forum_badges     ["founding-member", …] manual awards
//
// Every save purges the forum's cached pages (forumPurge).

const (
	forumLevelsOption = "pursae_forum_levels" // JSON forumLevels
	forumBadgesOption = "pursae_forum_badges" // JSON []forumBadge
	forumUMetaBadges  = "forum_badges"        // users.meta: manually awarded badge ids
	forumDefaultColor = "#2271b1"
	forumMaxLevels    = 30
	forumMaxBadges    = 100
)

// Badge rules.
const (
	forumRuleManual     = "manual"         // awarded by hand only
	forumRuleTopics     = "topics"         // published topics >= Threshold
	forumRuleReplies    = "replies"        // published replies >= Threshold
	forumRulePosts      = "posts"          // topics + replies >= Threshold
	forumRulePoints     = "points"         // points >= Threshold
	forumRuleMemberDays = "member_days"    // days since registration >= Threshold
	forumRuleVerified   = "verified_email" // the member confirmed the email address (or was never asked to)
	forumRuleGoogle     = "google_linked"  // a Google account is linked
	forumRuleModerator  = "moderator"      // the member can moderate the forum (forum_moderate)
	// v0.2.5.
	forumRuleSolutions = "solutions"   // accepted answers >= Threshold
	forumRuleLikes     = "likes"       // likes received >= Threshold
	forumRuleStreak    = "streak_days" // posted on Threshold consecutive days (longest run)
)

// forumBadgeRules are the rules in the admin's order; Threshold rules need N >= 1.
var forumBadgeRules = []struct {
	Key, Label string
	Threshold  bool
}{
	{forumRuleManual, "Awarded by hand", false},
	{forumRuleTopics, "Topics started", true},
	{forumRuleReplies, "Replies written", true},
	{forumRulePosts, "Posts (topics and replies)", true},
	{forumRulePoints, "Points", true},
	{forumRuleMemberDays, "Days since joining", true},
	{forumRuleVerified, "Verified email address", false},
	{forumRuleGoogle, "Google account linked", false},
	{forumRuleModerator, "Forum moderator", false},
	{forumRuleSolutions, "Accepted answers", true},
	{forumRuleLikes, "Likes received", true},
	{forumRuleStreak, "Days in a row with a post", true},
}

// forumRuleNeedsThreshold reports whether rule is a known rule that takes a threshold, and whether
// the rule is known at all.
func forumRuleInfo(rule string) (known, threshold bool) {
	for _, r := range forumBadgeRules {
		if r.Key == rule {
			return true, r.Threshold
		}
	}
	return false, false
}

// Types ---------------------------------------------------------------------------------------------

// forumLevel is a rank earned by points.
type forumLevel struct {
	ID       string `json:"id"`       // [a-z0-9-]{1,40}, unique; CSS pf-level--<id>
	Name     string `json:"name"`     // <= 40 runes
	Min      int    `json:"min"`      // points needed; the first level is always 0
	Icon     string `json:"icon"`     // forumIconPaths name or ""
	ImageID  int64  `json:"image_id"` // Media Library image instead of the icon (0 = none)
	Color    string `json:"color"`    // #rgb or #rrggbb
	ImageURL string `json:"-"`        // filled by forumLevelsLoad when ImageID resolves
	Rank     int    `json:"-"`        // v0.2.5: 1-based position in the list (forumLevelsLoad)
}

// forumLevels is the pursae_forum_levels option.
type forumLevels struct {
	TopicPoints    int          `json:"topic_points"`    // 0..1000, default 3
	ReplyPoints    int          `json:"reply_points"`    // 0..1000, default 1
	SolutionPoints int          `json:"solution_points"` // v0.2.5: 0..1000, default 10 (an accepted answer)
	LikePoints     int          `json:"like_points"`     // v0.2.5: 0..1000, default 1 (a like received)
	Levels         []forumLevel `json:"levels"`          // sorted by Min ascending, Levels[0].Min == 0
}

// forumBadge is an achievement shown next to a member's name.
type forumBadge struct {
	ID          string `json:"id"`          // [a-z0-9-]{1,40}, unique; CSS pf-badge--<id>
	Name        string `json:"name"`        // <= 60 runes
	Description string `json:"description"` // plain text <= 200 runes (title tooltip)
	Icon        string `json:"icon"`        // forumIconPaths name or ""
	ImageID     int64  `json:"image_id"`    // Media Library image instead of the icon (0 = none)
	Color       string `json:"color"`       // #rgb or #rrggbb
	Rule        string `json:"rule"`        // forumRule*
	Threshold   int    `json:"threshold"`   // N of a threshold rule (1..10,000,000), else 0
	Enabled     bool   `json:"enabled"`     // disabled badges are neither earned nor shown
	ImageURL    string `json:"-"`           // filled by forumBadges when ImageID resolves
}

// forumMemberStat is what levels and badges are computed from (forumMemberStats).
type forumMemberStat struct {
	UserID     int64
	Topics     int // published, non-spam
	Replies    int
	Points     int // Topics*TopicPoints + Replies*ReplyPoints
	Joined     time.Time
	LastActive time.Time // last published topic or reply; zero = never posted
	Verified   bool      // no pending forum email confirmation
	Google     bool      // forum_google_sub set
	Moderator  bool      // forum_moderate (roles + allow exceptions, minus deny)
	Blocked    bool      // a deny exception on forum_participate
	Manual     []string  // users.meta forum_badges
	// v0.2.5 (forumExtraStats).
	Solutions, Likes, Bonus, Streak int
}

// Defaults --------------------------------------------------------------------------------------------

func forumDefaultLevels() forumLevels {
	return forumLevels{TopicPoints: 3, ReplyPoints: 1, SolutionPoints: 10, LikePoints: 1, Levels: []forumLevel{
		{ID: "newcomer", Name: "Newcomer", Min: 0, Icon: "seedling", Color: "#8c8f94"},
		{ID: "member", Name: "Member", Min: 10, Icon: "leaf", Color: "#00a32a"},
		{ID: "regular", Name: "Regular", Min: 50, Icon: "star", Color: "#2271b1"},
		{ID: "contributor", Name: "Contributor", Min: 150, Icon: "bolt", Color: "#996800"},
		{ID: "veteran", Name: "Veteran", Min: 400, Icon: "medal", Color: "#d63638"},
		{ID: "legend", Name: "Legend", Min: 1000, Icon: "trophy", Color: "#7f54b3"},
	}}
}

func forumDefaultBadges() []forumBadge {
	return []forumBadge{
		{ID: "first-post", Name: "First Post", Description: "Wrote a first topic or reply.", Icon: "pencil", Color: "#2271b1", Rule: forumRulePosts, Threshold: 1, Enabled: true},
		{ID: "conversation-starter", Name: "Conversation Starter", Description: "Started 5 topics.", Icon: "chat", Color: "#00a32a", Rule: forumRuleTopics, Threshold: 5, Enabled: true},
		{ID: "helpful", Name: "Helpful", Description: "Wrote 50 replies.", Icon: "heart", Color: "#d63638", Rule: forumRuleReplies, Threshold: 50, Enabled: true},
		{ID: "prolific", Name: "Prolific", Description: "Wrote 250 posts.", Icon: "flame", Color: "#b32d2e", Rule: forumRulePosts, Threshold: 250, Enabled: true},
		{ID: "one-year-club", Name: "One Year Club", Description: "A member for a year.", Icon: "calendar", Color: "#996800", Rule: forumRuleMemberDays, Threshold: 365, Enabled: true},
		{ID: "verified", Name: "Verified", Description: "Confirmed the email address.", Icon: "verified", Color: "#00a32a", Rule: forumRuleVerified, Enabled: true},
		{ID: "moderator", Name: "Moderator", Description: "Keeps the forum friendly.", Icon: "shield", Color: "#2271b1", Rule: forumRuleModerator, Enabled: true},
		{ID: "founding-member", Name: "Founding Member", Description: "Here from the start.", Icon: "crown", Color: "#7f54b3", Rule: forumRuleManual, Enabled: true},
		{ID: "problem-solver", Name: "Problem Solver", Description: "Wrote 5 accepted answers.", Icon: "check", Color: "#00a32a", Rule: forumRuleSolutions, Threshold: 5, Enabled: true},
		{ID: "streak-7", Name: "7-Day Streak", Description: "Posted 7 days in a row.", Icon: "flame", Color: "#d63638", Rule: forumRuleStreak, Threshold: 7, Enabled: true},
	}
}

// Sanitizing ------------------------------------------------------------------------------------------

var (
	forumColorRe = regexp.MustCompile(`^#(?:[0-9a-f]{3}|[0-9a-f]{6})$`)
	forumIDRe    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)
)

// forumColor is c as a lowercase #hex colour, or def when it is not one.
func forumColor(c, def string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	if forumColorRe.MatchString(c) {
		return c
	}
	return def
}

// forumTruncRunes cuts s to n runes.
func forumTruncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// forumPlainLine is user text as one plain line: no control characters, collapsed spaces.
func forumPlainLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return forumTruncRunes(strings.Join(strings.Fields(s), " "), n)
}

// forumMakeID is a unique [a-z0-9-] id for name (want first when it is valid and free).
func forumMakeID(want, name, fallback string, used map[string]bool) string {
	id := strings.ToLower(strings.TrimSpace(want))
	if !forumIDRe.MatchString(id) || used[id] {
		var b strings.Builder
		dash := false
		for _, r := range strings.ToLower(name) {
			switch {
			case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
				b.WriteRune(r)
				dash = false
			case b.Len() > 0 && !dash:
				b.WriteByte('-')
				dash = true
			}
		}
		id = strings.Trim(forumTruncRunes(b.String(), 34), "-")
		if id == "" {
			id = fallback
		}
		base := id
		for n := 2; used[id] || !forumIDRe.MatchString(id); n++ {
			id = base + "-" + strconv.Itoa(n)
		}
	}
	used[id] = true
	return id
}

// forumSanitizeLevels clamps the levels setting: names required, unique ids and minimums, sorted,
// the first level at 0 points; an empty list becomes the defaults.
func forumSanitizeLevels(in forumLevels) forumLevels {
	clampPts := func(v int) int { return max(0, min(v, 1000)) }
	out := forumLevels{TopicPoints: clampPts(in.TopicPoints), ReplyPoints: clampPts(in.ReplyPoints),
		SolutionPoints: clampPts(in.SolutionPoints), LikePoints: clampPts(in.LikePoints)}
	list := make([]forumLevel, 0, len(in.Levels))
	for _, l := range in.Levels {
		l.Name = forumPlainLine(l.Name, 40)
		if l.Name == "" {
			continue
		}
		l.Min = max(0, min(l.Min, 10_000_000))
		if !forumIconValid(l.Icon) {
			l.Icon = ""
		}
		l.ImageID = max(l.ImageID, 0)
		l.Color = forumColor(l.Color, forumDefaultColor)
		l.ImageURL = ""
		list = append(list, l)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Min < list[j].Min })
	seenMin := map[int]bool{}
	used := map[string]bool{}
	for _, l := range list {
		if seenMin[l.Min] || len(out.Levels) >= forumMaxLevels {
			continue
		}
		seenMin[l.Min] = true
		l.ID = forumMakeID(l.ID, l.Name, "level", used)
		out.Levels = append(out.Levels, l)
	}
	if len(out.Levels) == 0 {
		out.Levels = forumDefaultLevels().Levels
	}
	out.Levels[0].Min = 0
	return out
}

// forumSanitizeBadge checks one badge; used holds the ids taken by the other badges.
func forumSanitizeBadge(b forumBadge, used map[string]bool) (forumBadge, error) {
	b.Name = forumPlainLine(b.Name, 60)
	if b.Name == "" {
		return b, errors.New("Please enter a badge name.")
	}
	b.Description = forumPlainLine(b.Description, 200)
	known, threshold := forumRuleInfo(b.Rule)
	if !known {
		return b, errors.New("Please choose how the badge is earned.")
	}
	if threshold {
		if b.Threshold < 1 {
			return b, errors.New("Please enter a number of at least 1 for this rule.")
		}
		b.Threshold = min(b.Threshold, 10_000_000)
	} else {
		b.Threshold = 0
	}
	if !forumIconValid(b.Icon) {
		b.Icon = ""
	}
	b.ImageID = max(b.ImageID, 0)
	b.Color = forumColor(b.Color, forumDefaultColor)
	b.ImageURL = ""
	b.ID = forumMakeID(b.ID, b.Name, "badge", used)
	return b, nil
}

// Load and save ---------------------------------------------------------------------------------------

// forumOptionInto decodes JSON option key over dst; false when the option is not set.
func (s *Server) forumOptionInto(key string, dst any) bool {
	v := s.settings.Get(key)
	if v == nil {
		return false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// forumLevelsLoad is the levels setting (defaults while unset), sanitized; ImageURL is filled.
func (s *Server) forumLevelsLoad(ctx context.Context) forumLevels {
	l := forumDefaultLevels()
	var stored forumLevels
	stored.TopicPoints, stored.ReplyPoints = l.TopicPoints, l.ReplyPoints // keys the option lacks keep the defaults
	stored.SolutionPoints, stored.LikePoints = l.SolutionPoints, l.LikePoints
	if s.forumOptionInto(forumLevelsOption, &stored) {
		if stored.Levels == nil {
			stored.Levels = l.Levels
		}
		l = forumSanitizeLevels(stored)
	}
	ids := make([]int64, 0)
	for _, lv := range l.Levels {
		if lv.ImageID > 0 {
			ids = append(ids, lv.ImageID)
		}
	}
	if urls := s.forumImageURLs(ctx, ids, 32); len(urls) > 0 {
		for i := range l.Levels {
			l.Levels[i].ImageURL = urls[l.Levels[i].ImageID]
		}
	}
	for i := range l.Levels {
		l.Levels[i].Rank = i + 1
	}
	return l
}

// forumSaveLevels stores the levels setting (sanitized) and purges the forum pages.
func (s *Server) forumSaveLevels(ctx context.Context, l forumLevels) (forumLevels, error) {
	l = forumSanitizeLevels(l)
	if err := s.settings.Set(ctx, map[string]any{forumLevelsOption: l}); err != nil {
		return l, err
	}
	s.forumPurge(ctx)
	return l, nil
}

// forumBadges is the badge list (the defaults while unset), disabled badges included; ImageURL is
// filled.
func (s *Server) forumBadges(ctx context.Context) []forumBadge {
	list := forumDefaultBadges()
	var stored []forumBadge
	if s.forumOptionInto(forumBadgesOption, &stored) && stored != nil {
		used := map[string]bool{}
		list = list[:0:0]
		for _, b := range stored {
			if len(list) >= forumMaxBadges {
				break
			}
			if b2, err := forumSanitizeBadge(b, used); err == nil {
				list = append(list, b2)
			}
		}
	}
	var ids []int64
	for _, b := range list {
		if b.ImageID > 0 {
			ids = append(ids, b.ImageID)
		}
	}
	if urls := s.forumImageURLs(ctx, ids, 32); len(urls) > 0 {
		for i := range list {
			list[i].ImageURL = urls[list[i].ImageID]
		}
	}
	return list
}

// forumBadgeByID is the badge with that id.
func (s *Server) forumBadgeByID(ctx context.Context, id string) (forumBadge, bool) {
	for _, b := range s.forumBadges(ctx) {
		if b.ID == id {
			return b, true
		}
	}
	return forumBadge{}, false
}

// forumSaveBadges stores the whole badge list (order = display order); invalid badges are an error.
func (s *Server) forumSaveBadges(ctx context.Context, list []forumBadge) ([]forumBadge, error) {
	if len(list) > forumMaxBadges {
		return nil, errors.New("Too many badges.")
	}
	used := map[string]bool{}
	out := make([]forumBadge, 0, len(list))
	for _, b := range list {
		b2, err := forumSanitizeBadge(b, used)
		if err != nil {
			return nil, err
		}
		out = append(out, b2)
	}
	if err := s.settings.Set(ctx, map[string]any{forumBadgesOption: out}); err != nil {
		return nil, err
	}
	s.forumPurge(ctx)
	return out, nil
}

// forumSaveBadge adds a badge (b.ID empty or unknown) or replaces the badge with b.ID; it returns
// the stored badge (with its id).
func (s *Server) forumSaveBadge(ctx context.Context, b forumBadge) (forumBadge, error) {
	list := s.forumBadges(ctx)
	at := -1
	used := map[string]bool{}
	for i, x := range list {
		if b.ID != "" && x.ID == b.ID {
			at = i
			continue
		}
		used[x.ID] = true
	}
	b2, err := forumSanitizeBadge(b, used)
	if err != nil {
		return b, err
	}
	if at >= 0 {
		list[at] = b2
	} else {
		if len(list) >= forumMaxBadges {
			return b, errors.New("Too many badges.")
		}
		list = append(list, b2)
	}
	if _, err := s.forumSaveBadges(ctx, list); err != nil {
		return b, err
	}
	return b2, nil
}

// forumDeleteBadge removes a badge and every manual award of it.
func (s *Server) forumDeleteBadge(ctx context.Context, id string) error {
	list := s.forumBadges(ctx)
	out := list[:0:0]
	for _, b := range list {
		if b.ID != id {
			out = append(out, b)
		}
	}
	if len(out) == len(list) {
		return errForumNotFound
	}
	if _, err := s.forumSaveBadges(ctx, out); err != nil {
		return err
	}
	_, err := s.store.Pool.Exec(ctx, `UPDATE users SET meta = jsonb_set(meta, '{`+forumUMetaBadges+`}', (meta->'`+forumUMetaBadges+`') - $1::text)
		WHERE jsonb_typeof(meta->'`+forumUMetaBadges+`') = 'array' AND meta->'`+forumUMetaBadges+`' ? $1::text`, id)
	return err
}

// forumAward awards badge id by hand to the users (any badge, automatic ones included: a manual award
// shows the badge whatever the rule says). Unknown users are skipped.
func (s *Server) forumAward(ctx context.Context, badgeID string, userIDs ...int64) error {
	if _, ok := s.forumBadgeByID(ctx, badgeID); !ok {
		return errForumNotFound
	}
	ids := forumIDs(userIDs...)
	if len(ids) == 0 {
		return nil
	}
	_, err := s.store.Pool.Exec(ctx, `UPDATE users SET meta = jsonb_set(coalesce(meta,'{}'), '{`+forumUMetaBadges+`}',
		CASE WHEN jsonb_typeof(meta->'`+forumUMetaBadges+`') = 'array' THEN meta->'`+forumUMetaBadges+`' ELSE '[]'::jsonb END || to_jsonb($2::text))
		WHERE id = ANY($1) AND NOT coalesce(jsonb_typeof(meta->'`+forumUMetaBadges+`') = 'array' AND meta->'`+forumUMetaBadges+`' ? $2::text, false)`, ids, badgeID)
	if err == nil {
		s.forumPurge(ctx)
	}
	return err
}

// forumRevoke takes a manual award back (a badge the member earns by rule stays).
func (s *Server) forumRevoke(ctx context.Context, badgeID string, userIDs ...int64) error {
	ids := forumIDs(userIDs...)
	if len(ids) == 0 || badgeID == "" {
		return nil
	}
	_, err := s.store.Pool.Exec(ctx, `UPDATE users SET meta = jsonb_set(meta, '{`+forumUMetaBadges+`}', (meta->'`+forumUMetaBadges+`') - $2::text)
		WHERE id = ANY($1) AND jsonb_typeof(meta->'`+forumUMetaBadges+`') = 'array' AND meta->'`+forumUMetaBadges+`' ? $2::text`, ids, badgeID)
	if err == nil {
		s.forumPurge(ctx)
	}
	return err
}

// Computing -------------------------------------------------------------------------------------------

// forumLevelFor is the level points reach in levels (sorted ascending, as sanitized), the next
// level (nil at the top) and the progress toward it in percent (100 at the top).
func forumLevelFor(levels []forumLevel, points int) (cur forumLevel, next *forumLevel, progress int) {
	if len(levels) == 0 {
		return forumLevel{}, nil, 100
	}
	at := 0
	for i, l := range levels {
		if points >= l.Min {
			at = i
		}
	}
	cur = levels[at]
	if at+1 >= len(levels) {
		return cur, nil, 100
	}
	n := levels[at+1]
	span := n.Min - cur.Min
	progress = 0
	if span > 0 {
		progress = max(0, min(99, (points-cur.Min)*100/span))
	}
	return cur, &n, progress
}

// forumUserLevel is the level of a member with that many points.
func (s *Server) forumUserLevel(ctx context.Context, points int) forumLevel {
	l, _, _ := forumLevelFor(s.forumLevelsLoad(ctx).Levels, points)
	return l
}

// forumBadgeEarned reports whether st earns b by its rule (manual awards aside).
func forumBadgeEarned(b forumBadge, st *forumMemberStat, now time.Time) bool {
	switch b.Rule {
	case forumRuleTopics:
		return st.Topics >= b.Threshold
	case forumRuleReplies:
		return st.Replies >= b.Threshold
	case forumRulePosts:
		return st.Topics+st.Replies >= b.Threshold
	case forumRulePoints:
		return st.Points >= b.Threshold
	case forumRuleMemberDays:
		return !st.Joined.IsZero() && int(now.Sub(st.Joined).Hours()/24) >= b.Threshold
	case forumRuleVerified:
		return st.Verified
	case forumRuleGoogle:
		return st.Google
	case forumRuleModerator:
		return st.Moderator
	case forumRuleSolutions:
		return st.Solutions >= b.Threshold
	case forumRuleLikes:
		return st.Likes >= b.Threshold
	case forumRuleStreak:
		return st.Streak >= b.Threshold
	}
	return false
}

// forumBadgesEarned are the enabled badges of list st earns (by rule or manual award), in list order.
func forumBadgesEarned(list []forumBadge, st *forumMemberStat, now time.Time) []forumBadge {
	if st == nil {
		return nil
	}
	manual := map[string]bool{}
	for _, id := range st.Manual {
		manual[id] = true
	}
	var out []forumBadge
	for _, b := range list {
		if b.Enabled && (manual[b.ID] || forumBadgeEarned(b, st, now)) {
			out = append(out, b)
		}
	}
	return out
}

// SQL fragments over users alias u.
const (
	forumSQLUnverified = `coalesce(u.meta->>'` + forumUMetaUnverified + `','') IN ('true','1')`
	forumSQLGoogle     = `coalesce(u.meta->>'` + forumUMetaGoogleSub + `','') <> ''`
	forumSQLBlocked    = `EXISTS (SELECT 1 FROM user_capabilities uc WHERE uc.user_id = u.id AND uc.capability = '` + capForumParticipate + `' AND uc.effect = 'deny')`
	// forumSQLModerator is forum_moderate as caps.Resolve computes it: a role or an allow exception
	// grants it, a deny exception takes it away (store.capCondition).
	forumSQLModerator = `((EXISTS (SELECT 1 FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = u.id
			AND r.capabilities @> '{"` + capForumModerate + `":true}'::jsonb)
		OR EXISTS (SELECT 1 FROM user_capabilities uc WHERE uc.user_id = u.id AND uc.capability = '` + capForumModerate + `' AND uc.effect = 'allow'))
		AND NOT EXISTS (SELECT 1 FROM user_capabilities uc WHERE uc.user_id = u.id AND uc.capability = '` + capForumModerate + `' AND uc.effect = 'deny'))`
	forumSQLManual = `CASE WHEN jsonb_typeof(u.meta->'` + forumUMetaBadges + `') = 'array' THEN u.meta->'` + forumUMetaBadges + `' ELSE '[]'::jsonb END`
	// forumSQLPostStats groups the published, non-spam topics and replies by author.
	forumSQLPostStats = `SELECT p.author_id, count(*) FILTER (WHERE p.type = 'topic') AS topics, count(*) FILTER (WHERE p.type = 'reply') AS replies,
		max(p.created_at) AS last_at FROM posts p WHERE p.type IN ('topic','reply') AND p.status = 'publish' AND ` + "coalesce(p.meta->>'" + forumMetaSpam + "','') <> 'true' AND " + forumSQLLiveParent
)

// forumManualIDs reads a forum_badges JSON array (non-strings skipped).
func forumManualIDs(raw []byte) []string {
	var arr []any
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// forumMemberStats loads the statistics of many members with one query (unknown ids are absent).
func (s *Server) forumMemberStats(ctx context.Context, ids []int64) (map[int64]*forumMemberStat, error) {
	out := map[int64]*forumMemberStat{}
	ids = forumIDs(ids...)
	if len(ids) == 0 {
		return out, nil
	}
	lv := s.forumLevelsLoad(ctx)
	rows, err := s.store.Pool.Query(ctx, `SELECT u.id, u.registered_at, `+forumSQLUnverified+`, `+forumSQLGoogle+`, `+forumSQLModerator+`,
		`+forumSQLBlocked+`, `+forumSQLManual+`, coalesce(st.topics,0), coalesce(st.replies,0), st.last_at
		FROM users u LEFT JOIN (`+forumSQLPostStats+` AND p.author_id = ANY($1) GROUP BY p.author_id) st ON st.author_id = u.id
		WHERE u.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st forumMemberStat
		var unverified bool
		var manual []byte
		var last *time.Time
		if err := rows.Scan(&st.UserID, &st.Joined, &unverified, &st.Google, &st.Moderator, &st.Blocked, &manual, &st.Topics, &st.Replies, &last); err != nil {
			return nil, err
		}
		st.Verified = !unverified
		st.Manual = forumManualIDs(manual)
		if last != nil {
			st.LastActive = *last
		}
		st.Points = st.Topics*lv.TopicPoints + st.Replies*lv.ReplyPoints
		out[st.UserID] = &st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	extra, err := s.forumExtraStats(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, v := range extra {
		if st := out[id]; st != nil {
			st.Solutions, st.Likes, st.Bonus, st.Streak = v[0], v[1], v[2], v[3]
			st.Points += st.Solutions*lv.SolutionPoints + st.Likes*lv.LikePoints + st.Bonus
			st.Points = max(st.Points, 0)
		}
	}
	return out, nil
}

// forumGamifyFill sets the statistics, level and badges of the authors (one stats query, one load
// of the levels and badges for all of them).
func (s *Server) forumGamifyFill(ctx context.Context, authors ...*forumAuthor) error {
	var ids []int64
	for _, a := range authors {
		if a != nil {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	stats, err := s.forumMemberStats(ctx, ids)
	if err != nil {
		return err
	}
	levels := s.forumLevelsLoad(ctx).Levels
	badges := s.forumBadges(ctx)
	now := time.Now()
	for _, a := range authors {
		if a == nil || stats[a.ID] == nil {
			continue
		}
		forumApplyStat(a, stats[a.ID], levels, badges, now)
	}
	return nil
}

// forumApplyStat copies st and what it earns onto a.
func forumApplyStat(a *forumAuthor, st *forumMemberStat, levels []forumLevel, badges []forumBadge, now time.Time) {
	a.Topics, a.Replies, a.Points = st.Topics, st.Replies, st.Points
	a.Solutions, a.LikesReceived, a.Bonus, a.Streak = st.Solutions, st.Likes, st.Bonus, st.Streak
	a.Joined, a.LastActive = st.Joined, st.LastActive
	a.Verified, a.Google = st.Verified, st.Google
	cur, next, progress := forumLevelFor(levels, st.Points)
	a.Level, a.NextLevel, a.Progress = &cur, next, progress
	a.Badges = forumBadgesEarned(badges, st, now)
	a.Stats = true
}

// forumImageURLs are the URLs of Media Library images (one query), at the smallest generated size
// that covers size px at 2x; ids that are not images are absent.
func (s *Server) forumImageURLs(ctx context.Context, ids []int64, size int) map[int64]string {
	ids = forumIDs(ids...)
	if len(ids) == 0 {
		return nil
	}
	atts, err := s.store.GetAttachments(ctx, ids)
	if err != nil {
		return nil
	}
	out := map[int64]string{}
	for id, a := range atts {
		if a == nil || !strings.HasPrefix(a.Mime, "image/") {
			continue
		}
		cacheDep(ctx, "media:"+itoa64(id))
		out[id] = s.forumAttachmentURL(ctx, a, size)
	}
	return out
}

// forumAttachmentURL is avatarFor's size pick for a loaded attachment.
func (s *Server) forumAttachmentURL(ctx context.Context, a *store.Attachment, size int) string {
	best, bestW := a.FilePath, a.Width
	for _, v := range a.Variants {
		if v.Width >= size*2 && (bestW == 0 || v.Width < bestW) {
			best, bestW = v.FilePath, v.Width
		}
	}
	return s.uploadURL(ctx, best)
}

// Rendering -------------------------------------------------------------------------------------------

// forumChipHTML is the chip markup shared by badges and levels: an image or icon, then the name.
func forumChipHTML(class, id, color, icon, imageURL, name, title string) template.HTML {
	var b strings.Builder
	b.WriteString(`<span class="pf-badge-chip ` + class)
	if forumIDRe.MatchString(id) {
		if strings.Contains(class, "pf-level") {
			b.WriteString(" pf-level--" + id)
		} else {
			b.WriteString(" pf-badge--" + id)
		}
	}
	b.WriteString(`" style="--pf-chip:` + forumColor(color, forumDefaultColor) + `"`)
	if title != "" {
		b.WriteString(` title="` + template.HTMLEscapeString(title) + `"`)
	}
	b.WriteString(`>`)
	switch {
	case imageURL != "":
		b.WriteString(`<img class="pf-badge-chip__img" src="` + template.HTMLEscapeString(imageURL) + `" alt="" width="16" height="16" loading="lazy" decoding="async" />`)
	case icon != "":
		b.WriteString(string(forumIcon(icon, 14)))
	}
	b.WriteString(`<span class="pf-badge-chip__name">` + template.HTMLEscapeString(name) + `</span></span>`)
	return template.HTML(b.String())
}

// forumBadgeHTML is a badge chip: <span class="pf-badge-chip pf-badge pf-badge--<id>"
// style="--pf-chip:#hex" title="description">icon name</span>.
func forumBadgeHTML(b forumBadge) template.HTML {
	if b.Name == "" {
		return ""
	}
	return forumChipHTML("pf-badge", b.ID, b.Color, b.Icon, b.ImageURL, b.Name, b.Description)
}

// forumLevelHTML is a level chip: <span class="pf-badge-chip pf-level pf-level--<id>"
// style="--pf-chip:#hex">icon name</span>; "" for nil.
func forumLevelHTML(l *forumLevel) template.HTML {
	if l == nil || l.Name == "" {
		return ""
	}
	return forumChipHTML("pf-level", l.ID, l.Color, l.Icon, l.ImageURL, l.Name, "")
}

// Members ---------------------------------------------------------------------------------------------

// forumMemberQuery is a page of the member list (front /forums/members/ and the admin Members tab).
type forumMemberQuery struct {
	Search  string // name / nicename (front); + login and email (Admin)
	Filter  string // all|unverified|blocked|moderators|google (Admin only; the front lists all)
	Sort    string // newest|active|name|points|topics|replies|joined|last_active (default newest)
	Order   string // asc|desc; "" = the sort's natural order
	Page    int    // >= 1
	PerPage int    // 1..100, default 20
	Admin   bool   // admin scope: every member, the filters
	// Identity also searches login and email (Admin scope, for a viewer with list_users only: a
	// Forum Moderator must not learn addresses one character at a time from what matches).
	Identity bool
	// PostedOnly lists only members with a published post (the front sets it while the core blocks
	// user enumeration, the same rule as the profile pages).
	PostedOnly bool
}

// forumMember is a row of the member list: the author (statistics, level, badges filled) and the
// admin-only columns.
type forumMember struct {
	*forumAuthor
	Email   string   // Admin scope only
	Roles   []string // role slugs, primary first (Admin scope only)
	EditURL string   // /wp-admin/user-edit?user_id=N (Admin scope only)
}

// forumMemberSorts maps a sort key to its ORDER BY expression and natural direction (the only text
// that reaches ORDER BY; the order word is "ASC" or "DESC" from forumMembers itself).
var forumMemberSorts = map[string]struct {
	expr string
	desc bool
}{
	"newest":      {"u.registered_at", true},
	"joined":      {"u.registered_at", false},
	"name":        {"lower(coalesce(nullif(btrim(u.display_name),''), u.login))", false},
	"active":      {"(coalesce(st.topics,0) + coalesce(st.replies,0))", true},
	"topics":      {"coalesce(st.topics,0)", true},
	"replies":     {"coalesce(st.replies,0)", true},
	"points":      {"__points__", true},
	"last_active": {"st.last_at", true},
}

var forumMemberFilters = map[string]string{
	"all":        "true",
	"unverified": forumSQLUnverified,
	"blocked":    forumSQLBlocked,
	"moderators": forumSQLModerator,
	"google":     forumSQLGoogle,
}

// forumLikeEscaper escapes LIKE wildcards (with the default backslash escape).
var forumLikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// forumNormalizeMemberQuery fills defaults and drops unknown values.
func forumNormalizeMemberQuery(q forumMemberQuery) forumMemberQuery {
	if _, ok := forumMemberSorts[q.Sort]; !ok {
		q.Sort = "newest"
	}
	if q.Order != "asc" && q.Order != "desc" {
		q.Order = ""
	}
	if _, ok := forumMemberFilters[q.Filter]; !ok || !q.Admin {
		q.Filter = "all"
	}
	q.Search = forumTruncRunes(strings.TrimSpace(q.Search), 100)
	if q.PerPage <= 0 {
		q.PerPage = 20
	}
	q.PerPage = min(q.PerPage, 100)
	q.Page = max(1, min(q.Page, 100000))
	return q
}

// forumMembers lists members: one page of rows (statistics, level and badges filled) and the total.
func (s *Server) forumMembers(ctx context.Context, q forumMemberQuery) ([]*forumMember, int, error) {
	q = forumNormalizeMemberQuery(q)
	lv := s.forumLevelsLoad(ctx)
	args := []any{}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	where := []string{forumMemberFilters[q.Filter]}
	if !q.Admin {
		where = append(where, "NOT "+forumSQLBlocked)
	}
	if q.PostedOnly {
		where = append(where, "st.author_id IS NOT NULL")
	}
	if q.Search != "" {
		p := arg("%" + forumLikeEscaper.Replace(strings.ToLower(q.Search)) + "%")
		cols := []string{"lower(u.display_name)", "lower(u.nicename)"}
		if q.Admin && q.Identity {
			cols = append(cols, "lower(u.login)", "lower(u.email)")
		}
		var ors []string
		for _, c := range cols {
			ors = append(ors, c+" LIKE "+p)
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	sortBy := forumMemberSorts[q.Sort]
	expr := sortBy.expr
	if expr == "__points__" {
		expr = "greatest(coalesce(st.topics,0) * " + strconv.Itoa(lv.TopicPoints) + " + coalesce(st.replies,0) * " + strconv.Itoa(lv.ReplyPoints) +
			" + coalesce(ex.solutions,0) * " + strconv.Itoa(lv.SolutionPoints) + " + coalesce(ex.likes,0) * " + strconv.Itoa(lv.LikePoints) +
			" + coalesce(ex.bonus,0), 0)"
	}
	desc := sortBy.desc
	switch q.Order {
	case "asc":
		desc = false
	case "desc":
		desc = true
	}
	dir := " ASC NULLS FIRST"
	idDir := " ASC"
	if desc {
		dir, idDir = " DESC NULLS LAST", " DESC"
	}
	from := ` FROM users u LEFT JOIN (` + forumSQLPostStats + ` GROUP BY p.author_id) st ON st.author_id = u.id`
	if sortBy.expr == "__points__" { // v0.2.5: accepted answers, likes and manual points count too
		from += ` LEFT JOIN (` + forumSQLExtraStats + ` GROUP BY x.uid) ex ON ex.user_id = u.id`
	}
	from += ` WHERE ` + strings.Join(where, " AND ")
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT u.id`+from+` ORDER BY `+expr+dir+`, u.id`+idDir+
		` LIMIT `+strconv.Itoa(q.PerPage)+` OFFSET `+strconv.Itoa((q.Page-1)*q.PerPage), args...)
	if err != nil {
		return nil, 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	authors, users, err := s.forumLoadAuthors(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*forumMember, 0, len(ids))
	for _, id := range ids {
		a := authors[id]
		if a == nil {
			continue
		}
		m := &forumMember{forumAuthor: a}
		if q.Admin {
			if u := users[id]; u != nil {
				m.Email, m.Roles = u.Email, u.Roles
			}
			m.EditURL = "/wp-admin/user-edit?user_id=" + itoa64(id)
		}
		out = append(out, m)
	}
	return out, total, nil
}

// Admin overview ----------------------------------------------------------------------------------------

// forumOverviewDay is one day of the 30-day chart (site time zone).
type forumOverviewDay struct {
	Date                     time.Time // midnight, site time zone
	Members, Topics, Replies int
}

// forumOverview is the admin Overview tab's data.
type forumOverview struct {
	Forums, Categories, Topics, Replies, Members int // published forums / topics / replies (non-spam); all users
	PendingTopics, PendingReplies                int
	SpamTopics, SpamReplies                      int
	Pending, Spam                                int                // the sums
	Days                                         []forumOverviewDay // 30 days, oldest first, today last
	DayMax                                       int                // the largest day value (bar scale), >= 1
	NewMembers, NewTopics, NewReplies            int                // the 30-day sums
	Top                                          []*forumMember     // 10 members with the most points (and a post)
	Recent                                       []*forumHit        // 15 newest published topics and replies
}

// forumOverview gathers the Overview tab: totals, the moderation queue, a 30-day series, top
// contributors and recent activity.
func (s *Server) forumOverview(ctx context.Context) (*forumOverview, error) {
	o := &forumOverview{}
	spam := "coalesce(p.meta->>'" + forumMetaSpam + "','') = 'true'"
	notSpam := forumNotSpam("p")
	err := s.store.Pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE p.type = 'forum' AND p.status = 'publish' AND coalesce(p.meta->>'`+forumMetaKind+`','forum') <> 'category'),
		count(*) FILTER (WHERE p.type = 'forum' AND p.status = 'publish' AND p.meta->>'`+forumMetaKind+`' = 'category'),
		count(*) FILTER (WHERE p.type = 'topic' AND p.status = 'publish' AND `+notSpam+`),
		count(*) FILTER (WHERE p.type = 'reply' AND p.status = 'publish' AND `+notSpam+`),
		count(*) FILTER (WHERE p.type = 'topic' AND p.status = 'pending' AND `+notSpam+`),
		count(*) FILTER (WHERE p.type = 'reply' AND p.status = 'pending' AND `+notSpam+`),
		count(*) FILTER (WHERE p.type = 'topic' AND p.status = 'trash' AND `+spam+`),
		count(*) FILTER (WHERE p.type = 'reply' AND p.status = 'trash' AND `+spam+`),
		(SELECT count(*) FROM users)
		FROM posts p WHERE p.type IN ('forum','topic','reply')`).Scan(&o.Forums, &o.Categories, &o.Topics, &o.Replies,
		&o.PendingTopics, &o.PendingReplies, &o.SpamTopics, &o.SpamReplies, &o.Members)
	if err != nil {
		return nil, err
	}
	o.Pending, o.Spam = o.PendingTopics+o.PendingReplies, o.SpamTopics+o.SpamReplies

	// The 30-day series, bucketed by the site's current UTC offset.
	loc := s.loc()
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	first := today.AddDate(0, 0, -29)
	_, off := now.Zone()
	o.Days = make([]forumOverviewDay, 30)
	index := map[string]int{}
	for i := range o.Days {
		d := first.AddDate(0, 0, i)
		o.Days[i].Date = d
		index[d.Format("2006-01-02")] = i
	}
	shift := "make_interval(secs => " + strconv.Itoa(off) + ")"
	rows, err := s.store.Pool.Query(ctx, `SELECT 'm', to_char(registered_at AT TIME ZONE 'UTC' + `+shift+`, 'YYYY-MM-DD'), count(*)
		FROM users WHERE registered_at >= $1 GROUP BY 2
		UNION ALL
		SELECT p.type, to_char(p.created_at AT TIME ZONE 'UTC' + `+shift+`, 'YYYY-MM-DD'), count(*)
		FROM posts p WHERE p.type IN ('topic','reply') AND p.status = 'publish' AND `+notSpam+` AND p.created_at >= $1 GROUP BY 1, 2`, first.UTC())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, day string
		var n int
		if err := rows.Scan(&kind, &day, &n); err != nil {
			rows.Close()
			return nil, err
		}
		i, ok := index[day]
		if !ok {
			continue
		}
		switch kind {
		case "m":
			o.Days[i].Members += n
			o.NewMembers += n
		case "topic":
			o.Days[i].Topics += n
			o.NewTopics += n
		case "reply":
			o.Days[i].Replies += n
			o.NewReplies += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	o.DayMax = 1
	for _, d := range o.Days {
		o.DayMax = max(o.DayMax, d.Members, d.Topics+d.Replies)
	}

	if o.Top, _, err = s.forumMembers(ctx, forumMemberQuery{Sort: "points", PerPage: 10, Admin: true, PostedOnly: true}); err != nil {
		return nil, err
	}
	if o.Recent, _, err = s.forumActivity(ctx, "", 0, 1, 15); err != nil {
		return nil, err
	}
	return o, nil
}

// Member management (admin bulk and row actions) ---------------------------------------------------------

// forumSetBlocked blocks (a deny exception on forum_participate) or unblocks a member, under the same
// rules as the profile's mod bar (forumBlockGate): not yourself, never an administrator, a moderator
// only by an administrator.
func (s *Server) forumSetBlocked(ctx context.Context, actor *store.User, targetID int64, block bool) error {
	if actor == nil || !actor.Can(capForumModerate, nil) {
		return errors.New("Sorry, you are not allowed to block members.")
	}
	target, err := s.store.UserByID(ctx, targetID)
	if err != nil {
		return errForumNotFound
	}
	if code, msg := forumBlockGate(actor, target); code != 0 {
		return errors.New(msg)
	}
	exc, err := s.store.UserCapExceptions(ctx, target.ID)
	if err != nil {
		return err
	}
	access := s.userAccessKey(ctx, target.ID)
	if block {
		exc[capForumParticipate] = "deny"
	} else if exc[capForumParticipate] == "deny" {
		delete(exc, capForumParticipate)
	} else {
		return nil
	}
	if err := s.store.SetUserCapExceptions(ctx, target.ID, exc); err != nil {
		return err
	}
	// A blocked member's open sessions end, like any other access change (never the actor's: the
	// gate refuses self-blocks).
	s.endSessionsAfterAccessChange(ctx, nil, target.ID, access)
	s.forumPurge(ctx)
	return nil
}

// forumVerifyMembers marks members' email addresses as confirmed (an administrator vouches for them).
func (s *Server) forumVerifyMembers(ctx context.Context, ids ...int64) error {
	ids = forumIDs(ids...)
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.store.Pool.Exec(ctx, `UPDATE users SET meta = meta - $2::text[] WHERE id = ANY($1) AND meta ?| $2::text[]`,
		ids, []string{forumUMetaUnverified, forumUMetaVerify}); err != nil {
		return err
	}
	s.forumPurge(ctx)
	return nil
}

// forumSetForumRole gives a member the forum role role (forum_participant or forum_moderator) in
// place of the other forum role; the member's other roles stay (multi-role union). A member whose
// primary role was a forum role (or who had none) gets role as the primary role. Administrators
// only (manage_options); nobody changes their own forum role.
func (s *Server) forumSetForumRole(ctx context.Context, actor *store.User, targetID int64, role string) error {
	if actor == nil || !actor.Can("manage_options", nil) {
		return errors.New("Sorry, you are not allowed to change forum roles.")
	}
	if role != forumRoleParticipant && role != forumRoleModerator {
		return errors.New("Unknown forum role.")
	}
	if actor.ID == targetID {
		return errors.New("You cannot change your own forum role.")
	}
	target, err := s.store.UserByID(ctx, targetID)
	if err != nil {
		return errForumNotFound
	}
	isForum := func(r string) bool { return r == forumRoleParticipant || r == forumRoleModerator }
	primary := role
	var extra []string
	if len(target.Roles) > 0 && !isForum(target.Roles[0]) {
		primary = target.Roles[0]
		extra = append(extra, role)
	}
	for i, r := range target.Roles {
		if i == 0 || isForum(r) {
			continue
		}
		extra = append(extra, r)
	}
	access := s.userAccessKey(ctx, target.ID)
	if err := s.store.SetUserRoles(ctx, target.ID, primary, extra); err != nil {
		return err
	}
	// A role change ends the member's sessions, as everywhere else roles change.
	s.endSessionsAfterAccessChange(ctx, nil, target.ID, access)
	s.forumPurge(ctx)
	return nil
}
