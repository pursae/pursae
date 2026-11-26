package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Pursae Forum v0.2.5, the design data of the forum lists: forum meta (icon, pastel colour, label,
// number, chips, tips, staff-only, moderators), topic tags, the sort tabs (?sort=) and tag filters
// (?tag=), site statistics, members of the week, similar topics and the forum picker.
//
//   forum posts.meta  _forum_icon _forum_color _forum_label _forum_number _forum_chips [] _forum_tips []
//                     _forum_staff_only _forum_moderators [user ids]
//   topic posts.meta  _forum_tags [slugs] _forum_tag_names [names]
//
// Nothing here is visitor-specific, so every list stays cacheable; ?sort= and ?tag= are cache key
// parameters while the plugin is on (cacheConfig), and an unknown sort is redirected to the
// canonical URL so arbitrary values do not fill the cache.

// forumColors are the pastel keys a forum may take (CSS: pf-c-<key>, --pf-<key>).
var forumColors = map[string]bool{"sky": true, "mint": true, "peach": true, "lilac": true, "grey": true}

// forumColorKeys are the pastel keys in the admin's order.
var forumColorKeys = []string{"sky", "mint", "peach", "lilac", "grey"}

const (
	forumMaxTags     = 5  // tags per topic
	forumTagMaxLen   = 30 // runes per tag
	forumMaxChips    = 8  // chips per forum
	forumMaxTips     = 8  // tips per forum
	forumNewHours    = 24 // a topic is "new" for a day
	forumMaxForumMod = 12 // moderators listed per forum
)

// forumSorts are the sort keys of topic lists, in tab order; "" means latest.
var forumSorts = []string{"latest", "popular", "unanswered", "solved"}

var forumSortLabels = map[string]string{"latest": "Latest", "popular": "Popular", "unanswered": "Unanswered", "solved": "Solved"}

// forumSortOf is a valid sort key ("latest" for "" ; ok false for an unknown value).
func forumSortOf(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "latest", true
	}
	if _, ok := forumSortLabels[raw]; ok {
		return raw, true
	}
	return "latest", false
}

// Forum meta -------------------------------------------------------------------------------------

// forumDesignInput is the design meta of a forum to save (forumSaveForum: nil keeps the stored meta).
type forumDesignInput struct {
	Icon, Color, Label, Number string
	Chips, Tips                []string
	StaffOnly                  bool
	Moderators                 []int64
}

// forumStrList reads a JSON string array meta value.
func forumStrList(m map[string]any, k string) []string {
	arr, _ := m[k].([]any)
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// forumSanitizeDesign clamps a forum's design meta: a known icon and colour, short label and number,
// at most forumMaxChips chips and forumMaxTips tips, plain text everywhere.
func forumSanitizeDesign(in forumDesignInput) forumDesignInput {
	out := forumDesignInput{StaffOnly: in.StaffOnly}
	if ic := strings.TrimSpace(in.Icon); forumIconValid(ic) {
		out.Icon = ic
	}
	out.Color = forumColorValue(in.Color) // a pastel key or a custom #rrggbb (forum_color.go)
	out.Label = forumPlainLine(in.Label, 40)
	out.Number = forumPlainLine(in.Number, 8)
	seen := map[string]bool{}
	for _, ch := range in.Chips {
		ch = forumPlainLine(ch, forumTagMaxLen)
		slug := forumTagSlug(ch)
		if ch == "" || slug == "" || seen[slug] || len(out.Chips) >= forumMaxChips {
			continue
		}
		seen[slug] = true
		out.Chips = append(out.Chips, ch)
	}
	for _, tip := range in.Tips {
		if tip = forumPlainLine(tip, 200); tip != "" && len(out.Tips) < forumMaxTips {
			out.Tips = append(out.Tips, tip)
		}
	}
	out.Moderators = forumIDs(in.Moderators...)
	if len(out.Moderators) > forumMaxForumMod {
		out.Moderators = out.Moderators[:forumMaxForumMod]
	}
	return out
}

// forumDesignMeta is the posts.meta object of a design.
func forumDesignMeta(d forumDesignInput) map[string]any {
	chips, tips, mods := d.Chips, d.Tips, d.Moderators
	if chips == nil {
		chips = []string{}
	}
	if tips == nil {
		tips = []string{}
	}
	if mods == nil {
		mods = []int64{}
	}
	return map[string]any{forumMetaIcon: d.Icon, forumMetaColor: d.Color, forumMetaLabel: d.Label, forumMetaNumber: d.Number,
		forumMetaChips: chips, forumMetaTips: tips, forumMetaStaffOnly: d.StaffOnly, forumMetaModerators: mods}
}

// forumApplyDesign fills a forum's design fields from its post meta.
func (s *Server) forumApplyDesign(ctx context.Context, f *forumForum, meta map[string]any) {
	if ic := forumMetaStr(meta, forumMetaIcon); forumIconValid(ic) {
		f.Icon = ic
	}
	f.Color, f.ColorHex, f.ColorInk, f.ColorStyle = forumColorParts(forumMetaStr(meta, forumMetaColor))
	f.Label = forumMetaStr(meta, forumMetaLabel)
	f.Number = forumMetaStr(meta, forumMetaNumber)
	for _, ch := range forumStrList(meta, forumMetaChips) {
		if slug := forumTagSlug(ch); slug != "" {
			f.Chips = append(f.Chips, forumTag{Name: ch, Slug: slug, URL: f.URL + "?tag=" + url.QueryEscape(slug)})
		}
	}
	f.Tips = forumStrList(meta, forumMetaTips)
	f.StaffOnly = forumMetaBool(meta, forumMetaStaffOnly)
}

// forumCanStart reports whether a member with mod rights may start a topic in f.
func forumCanStart(f *forumForum, mod bool) bool {
	if f == nil || f.Kind != "forum" {
		return false
	}
	return mod || (!f.Closed && !f.StaffOnly)
}

// forumModeratorsOf are the moderators shown on a forum: its own list (_forum_moderators), else the
// site's forum moderators (administrators first), at most forumMaxForumMod.
func (s *Server) forumModeratorsOf(ctx context.Context, forumID int64) ([]*forumAuthor, error) {
	var raw []byte
	err := s.store.Pool.QueryRow(ctx, `SELECT coalesce(meta->'`+forumMetaModerators+`','[]') FROM posts WHERE id = $1 AND type = 'forum'`, forumID).Scan(&raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var ids []int64
	_ = json.Unmarshal(raw, &ids)
	ids = forumIDs(ids...)
	if len(ids) == 0 {
		rows, err := s.store.Pool.Query(ctx, `SELECT u.id FROM users u WHERE `+forumSQLModerator+` AND NOT `+forumSQLBlocked+`
			ORDER BY (EXISTS (SELECT 1 FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = u.id
				AND r.capabilities @> '{"manage_options":true}'::jsonb)) DESC, u.id LIMIT `+strconv.Itoa(forumMaxForumMod))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
	}
	authors, err := s.forumAuthors(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*forumAuthor, 0, len(ids))
	for _, id := range ids {
		if a := authors[id]; a != nil && !a.Blocked {
			out = append(out, a)
		}
	}
	return out, nil
}

// Tags --------------------------------------------------------------------------------------------

// forumTagSlug is a tag's slug (lowercase letters of any script, digits and dashes).
func forumTagSlug(name string) string {
	slug := wp.SanitizeTitle(strings.ToLower(strings.TrimSpace(name)))
	if r := []rune(slug); len(r) > 40 {
		slug = strings.Trim(string(r[:40]), "-")
	}
	if !forumValidSlug(slug) {
		return ""
	}
	return slug
}

// forumParseTags splits typed tags ("cron, wasm" or one per line): plain text, at most
// forumTagMaxLen runes each, distinct by slug, at most forumMaxTags. It returns names and slugs.
func forumParseTags(raw string) (names, slugs []string) {
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == ';' || r == '#' }) {
		name := forumPlainLine(part, forumTagMaxLen)
		slug := forumTagSlug(name)
		if name == "" || slug == "" || seen[slug] {
			continue
		}
		if len(slugs) >= forumMaxTags {
			break
		}
		seen[slug] = true
		names, slugs = append(names, name), append(slugs, slug)
	}
	return names, slugs
}

// forumTagsMeta is the posts.meta object storing tags.
func forumTagsMeta(names, slugs []string) map[string]any {
	if names == nil {
		names, slugs = []string{}, []string{}
	}
	return map[string]any{forumMetaTags: slugs, forumMetaTagNames: names}
}

// forumTagsOf reads a topic's tags (names and slugs, same order; a name falls back to its slug).
func (s *Server) forumTagsOf(ctx context.Context, meta map[string]any) []forumTag {
	slugs := forumStrList(meta, forumMetaTags)
	names := forumStrList(meta, forumMetaTagNames)
	out := make([]forumTag, 0, len(slugs))
	for i, slug := range slugs {
		name := slug
		if i < len(names) {
			name = names[i]
		}
		out = append(out, forumTag{Name: name, Slug: slug, URL: s.forumURL(ctx, "tag", slug)})
	}
	return out
}

// forumTagList is the tags of published topics in published forums with their topic counts, most used
// first (forumID > 0: that forum only); limit <= 0 lists up to 200.
func (s *Server) forumTagList(ctx context.Context, forumID int64, limit int) ([]forumTag, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	args := []any{limit}
	where := ""
	if forumID > 0 {
		args = append(args, forumID)
		where = " AND p.parent_id = $2"
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT t.slug, count(*),
		(array_agg(coalesce(p.meta->'`+forumMetaTagNames+`'->>(t.ord::int - 1), t.slug) ORDER BY p.id DESC))[1]
		FROM posts p, jsonb_array_elements_text(CASE WHEN jsonb_typeof(p.meta->'`+forumMetaTags+`') = 'array' THEN p.meta->'`+forumMetaTags+`' ELSE '[]'::jsonb END)
			WITH ORDINALITY AS t(slug, ord)
		WHERE p.type = 'topic' AND p.status = 'publish' AND `+forumNotSpam("p")+` AND `+forumPublishedForumsSQL("p")+where+`
		GROUP BY t.slug ORDER BY count(*) DESC, t.slug LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []forumTag
	for rows.Next() {
		var tg forumTag
		if err := rows.Scan(&tg.Slug, &tg.Count, &tg.Name); err != nil {
			return nil, err
		}
		tg.URL = s.forumURL(ctx, "tag", tg.Slug)
		out = append(out, tg)
	}
	return out, rows.Err()
}

// forumTagByName is a tag in use (its most recent name), errForumNotFound when no published topic has it.
func (s *Server) forumTagBySlug(ctx context.Context, slug string) (*forumTag, error) {
	if slug == "" || !forumValidSlug(slug) {
		return nil, errForumNotFound
	}
	var name string
	err := s.store.Pool.QueryRow(ctx, `SELECT coalesce(p.meta->'`+forumMetaTagNames+`'->>((SELECT i - 1 FROM jsonb_array_elements_text(p.meta->'`+forumMetaTags+`')
			WITH ORDINALITY AS t(s, i) WHERE t.s = $1 LIMIT 1)::int), $1)
		FROM posts p WHERE p.type = 'topic' AND p.status = 'publish' AND `+forumNotSpam("p")+` AND `+forumPublishedForumsSQL("p")+` AND p.meta->'`+forumMetaTags+`' ? $1
		ORDER BY p.id DESC LIMIT 1`, slug).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errForumNotFound
	}
	if err != nil {
		return nil, err
	}
	return &forumTag{Name: name, Slug: slug, URL: s.forumURL(ctx, "tag", slug)}, nil
}

// Topic enrichment ------------------------------------------------------------------------------------

// forumEnrichTopics fills the v0.2.5 fields of listed topics with one query each: views, likes of the
// opening post (demo seed included) and the recent participants (authors loaded by the caller: the
// ids are returned). Tags, the solution and IsNew come from the meta.
func (s *Server) forumEnrichTopics(ctx context.Context, out []*forumTopic, metas map[int64]map[string]any) (participants map[int64][]int64) {
	participants = map[int64][]int64{}
	if len(out) == 0 {
		return participants
	}
	ids := make([]int64, 0, len(out))
	byID := map[int64]*forumTopic{}
	now := time.Now()
	for _, t := range out {
		ids = append(ids, t.ID)
		byID[t.ID] = t
		m := metas[t.ID]
		t.Tags = s.forumTagsOf(ctx, m)
		if sol := forumMetaInt64(m, forumMetaSolution); sol > 0 {
			t.Solved, t.SolutionID = true, sol
		}
		t.IsNew = now.Sub(t.CreatedAt) < forumNewHours*time.Hour && t.CreatedAt.Before(now.Add(time.Minute))
		t.Likes = forumMetaInt(m, forumMetaLikesSeed)
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT v.topic_id, v.views FROM forum_topic_views v WHERE v.topic_id = ANY($1)`, ids)
	if err == nil {
		for rows.Next() {
			var id int64
			var n int
			if rows.Scan(&id, &n) == nil && byID[id] != nil {
				byID[id].Views = n
			}
		}
		rows.Close()
	}
	rows, err = s.store.Pool.Query(ctx, `SELECT post_id, count(*) FROM forum_likes WHERE post_id = ANY($1) GROUP BY post_id`, ids)
	if err == nil {
		for rows.Next() {
			var id int64
			var n int
			if rows.Scan(&id, &n) == nil && byID[id] != nil {
				byID[id].Likes += n
			}
		}
		rows.Close()
	}
	rows, err = s.store.Pool.Query(ctx, `SELECT parent_id, author_id FROM (
			SELECT DISTINCT ON (r.parent_id, r.author_id) r.parent_id, r.author_id, r.published_at FROM posts r
			WHERE r.type = 'reply' AND r.parent_id = ANY($1) AND r.status = 'publish' AND `+forumNotSpam("r")+` AND r.author_id IS NOT NULL
			ORDER BY r.parent_id, r.author_id, r.published_at DESC) x
		ORDER BY parent_id, published_at DESC`, ids)
	if err == nil {
		for rows.Next() {
			var tid, uid int64
			if rows.Scan(&tid, &uid) == nil && len(participants[tid]) < 4 {
				participants[tid] = append(participants[tid], uid)
			}
		}
		rows.Close()
	}
	return participants
}

// Sort tabs -------------------------------------------------------------------------------------------

// forumListURL is base with the sort and tag query (latest and no tag: base itself).
func forumListURL(base, sort, tag string) string {
	q := url.Values{}
	if sort != "" && sort != "latest" {
		q.Set("sort", sort)
	}
	if tag != "" {
		q.Set("tag", tag)
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// forumSortTabs are the sort tabs of a list at base (keeping tag); solved only where it matters.
func (p *forumPage) forumSortTabs(base, cur, tag string, withSolved bool) []forumSortLink {
	var out []forumSortLink
	for _, k := range forumSorts {
		if k == "solved" && !withSolved {
			continue
		}
		out = append(out, forumSortLink{Key: k, Label: p.T(forumSortLabels[k]), URL: forumListURL(base, k, tag), Current: k == cur})
	}
	return out
}

// forumListQuery reads ?sort= and ?tag= of a list request. ok is false when the URL must be
// redirected (an unknown sort): redirect is the canonical URL then.
func (s *Server) forumListQuery(c *gin.Context, base string) (sort, tag string, redirect string, ok bool) {
	rawSort := c.Query("sort")
	sort, valid := forumSortOf(rawSort)
	canonicalSort := sort
	if sort == "latest" {
		canonicalSort = ""
	}
	tag = strings.TrimSpace(c.Query("tag"))
	if tag != "" && !forumValidSlug(tag) {
		tag = forumTagSlug(tag)
	}
	if !valid || rawSort != canonicalSort || (c.Query("tag") != "" && tag != c.Query("tag")) {
		return sort, tag, forumListURL(base, sort, tag), false
	}
	return sort, tag, "", true
}

// Statistics --------------------------------------------------------------------------------------------

// forumStatsOf are the site totals: forums, topics, replies, members, solved topics, the week's new
// topics and members, and who is online.
func (s *Server) forumStatsOf(ctx context.Context) (*forumStats, error) {
	st := &forumStats{}
	err := s.store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM posts WHERE type = 'forum' AND status = 'publish' AND coalesce(meta->>'`+forumMetaKind+`','forum') <> 'category'),
		(SELECT count(*) FROM posts p WHERE p.type = 'topic' AND p.status = 'publish' AND `+forumNotSpam("p")+`),
		(SELECT count(*) FROM posts p WHERE p.type = 'reply' AND p.status = 'publish' AND `+forumNotSpam("p")+` AND `+forumSQLLiveParent+`),
		(SELECT count(*) FROM users),
		(SELECT count(*) FROM posts p WHERE p.type = 'topic' AND p.status = 'publish' AND `+forumNotSpam("p")+` AND p.meta ? '`+forumMetaSolution+`'),
		(SELECT count(*) FROM posts p WHERE p.type = 'topic' AND p.status = 'publish' AND `+forumNotSpam("p")+` AND p.created_at > now() - interval '7 days'),
		(SELECT count(*) FROM users WHERE registered_at > now() - interval '7 days')`).Scan(&st.Forums, &st.Topics, &st.Replies, &st.Members,
		&st.Solved, &st.NewTopicsWeek, &st.NewMembersWeek)
	if err != nil {
		return nil, err
	}
	if st.Topics > 0 {
		st.SolvedPct = st.Solved * 100 / st.Topics
	}
	st.Online, st.OnlineMembers = s.forumOnlineCount(0)
	return st, nil
}

// Members of the week --------------------------------------------------------------------------------

// forumWeeklyMembers are the n members who earned the most points in the last seven days (the ledger,
// counting only events whose post is still live), with their level and badges.
func (s *Server) forumWeeklyMembers(ctx context.Context, n int) ([]*forumWeekly, error) {
	if n <= 0 || n > 50 {
		n = 5
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT fp.user_id, sum(fp.points)::int FROM forum_points fp JOIN users u ON u.id = fp.user_id
		WHERE fp.created_at > now() - interval '7 days' AND NOT `+forumSQLBlocked+`
			AND (fp.post_id IS NULL OR EXISTS (SELECT 1 FROM posts p WHERE p.id = fp.post_id AND p.status = 'publish' AND `+forumNotSpam("p")+` AND `+forumSQLLiveParent+`))
		GROUP BY fp.user_id HAVING sum(fp.points) > 0 ORDER BY 2 DESC, 1 LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	type row struct {
		id    int64
		delta int
	}
	var list []row
	var ids []int64
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.delta); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
		ids = append(ids, r.id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	authors, err := s.forumAuthors(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*forumWeekly, 0, len(list))
	for _, r := range list {
		if a := authors[r.id]; a != nil {
			out = append(out, &forumWeekly{Author: a, Delta: r.delta})
		}
	}
	return out, nil
}

// Similar topics ------------------------------------------------------------------------------------

var forumWordRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

// forumSimilarTerms are the distinct words of a title worth matching (3+ runes, at most 8).
func forumSimilarTerms(title string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range forumWordRe.FindAllString(strings.ToLower(title), -1) {
		if len([]rune(w)) < 3 || seen[w] || forumStopWords[w] {
			continue
		}
		allDigits := true
		for _, r := range w {
			if !unicode.IsDigit(r) {
				allDigits = false
				break
			}
		}
		if allDigits {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) == 8 {
			break
		}
	}
	return out
}

// forumStopWords are frequent words that make every title look alike (English and Turkish).
var forumStopWords = map[string]bool{"the": true, "and": true, "for": true, "with": true, "how": true, "what": true, "why": true,
	"this": true, "that": true, "not": true, "are": true, "can": true, "does": true, "you": true, "from": true,
	"bir": true, "ile": true, "için": true, "neden": true, "nasıl": true, "ama": true, "gibi": true, "daha": true, "çok": true, "olan": true}

// forumSimilarTopics are up to n published topics like the title and tags (words in common, then
// shared tags), most relevant first; exclude is left out.
func (s *Server) forumSimilarTopics(ctx context.Context, title string, tags []string, exclude int64, n int) ([]*forumTopic, error) {
	if n <= 0 || n > 20 {
		n = 5
	}
	terms := forumSimilarTerms(title)
	if len(terms) == 0 && len(tags) == 0 {
		return nil, nil
	}
	if tags == nil {
		tags = []string{}
	}
	query := strings.Join(terms, " | ")
	folded := store.FoldedArgSQL("$1") // folded like posts.search (migration 0105)
	match := `((` + "$1::text <> '' AND p.search @@ to_tsquery('simple', " + folded + ")) OR p.meta->'" + forumMetaTags + `' ?| $2::text[])`
	rank := `(CASE WHEN $1::text <> '' THEN ts_rank(p.search, to_tsquery('simple', ` + folded + `)) ELSE 0 END)`
	rows, err := s.store.Pool.Query(ctx, `SELECT `+forumPrefixed("p")+` FROM posts p WHERE p.type = 'topic' AND p.status = 'publish'
		AND `+forumNotSpam("p")+` AND `+forumPublishedForumsSQL("p")+` AND p.id <> $3 AND `+match+`
		ORDER BY `+rank+` + 0.2 * (SELECT count(*) FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(p.meta->'`+forumMetaTags+`') = 'array'
			THEN p.meta->'`+forumMetaTags+`' ELSE '[]'::jsonb END) x WHERE x = ANY($2::text[])) DESC,
			p.meta->>'`+forumMetaLastActive+`' DESC, p.id DESC LIMIT $4`, query, tags, exclude, n)
	if err != nil {
		return nil, err
	}
	posts, err := forumScanPosts(rows)
	if err != nil {
		return nil, err
	}
	return s.forumTopicsFrom(ctx, posts, false)
}

// Forum picker ------------------------------------------------------------------------------------------

// forumPick is a forum of the new-topic picker.
type forumPick struct {
	Forum    *forumForum
	Category string // the category it sits in ("" = none)
	Disabled bool   // the member may not start a topic there (closed or staff-only)
	Checked  bool
}

// PickForums are the forums of the new-topic picker (every forum, categories flattened), the forum
// being posted to checked.
func (p *forumPage) PickForums() []forumPick {
	if p == nil {
		return nil
	}
	var sel int64
	if p.Forum != nil {
		sel = p.Forum.ID
	}
	if v, ok := p.Form["forum"]; ok && v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			sel = id
		}
	}
	var out []forumPick
	add := func(f *forumForum, cat string) {
		if f.Kind != "forum" {
			return
		}
		out = append(out, forumPick{Forum: f, Category: cat, Disabled: !forumCanStart(f, p.CanModerate), Checked: f.ID == sel})
	}
	for _, f := range p.Forums {
		if f.Kind == "category" {
			for _, ch := range f.Children {
				add(ch, f.Title)
			}
			continue
		}
		add(f, "")
	}
	return out
}

// OtherForums are the forums (categories flattened, categories left out) other than the one being
// viewed: the "other forums" box.
func (p *forumPage) OtherForums() []*forumForum {
	if p == nil {
		return nil
	}
	var cur int64
	if p.Forum != nil {
		cur = p.Forum.ID
	}
	var out []*forumForum
	for _, f := range p.Forums {
		if f.Kind == "category" {
			for _, ch := range f.Children {
				if ch.ID != cur {
					out = append(out, ch)
				}
			}
			continue
		}
		if f.ID != cur {
			out = append(out, f)
		}
	}
	return out
}

// AllForums are every forum of the tree, flattened (categories left out).
func (p *forumPage) AllForums() []*forumForum {
	if p == nil {
		return nil
	}
	var out []*forumForum
	for _, f := range p.Forums {
		if f.Kind == "category" {
			out = append(out, f.Children...)
			continue
		}
		out = append(out, f)
	}
	return out
}

// forumPllStrings are the forum's site-wide texts listed in Pursae Multi Language's Translations
// (group Pursae Forum): the forum title when the owner changed it.
func (s *Server) forumPllStrings() []pllRegistered {
	if !s.forumOn() {
		return nil
	}
	st := s.forumLoad()
	if st.Title == "" || st.Title == forumDefaults().Title {
		return nil
	}
	return []pllRegistered{{Name: "Forum title", String: st.Title, Group: "Pursae Forum"}}
}
