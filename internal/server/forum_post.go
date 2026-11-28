package server

import (
	"context"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"wpgo/internal/cache"
	"wpgo/internal/i18n"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Posting, editing, moderation and member blocking on the forum front end (WP1). Every form is a
// plain POST (no JavaScript) to a forum URL carrying forum_action; success redirects to the result
// (POST, redirect, GET), a validation error re-renders the form with status 422.
//
// Posting rules: a logged-in member with forum_participate (not blocked, and confirmed when the
// settings require it); the forum must be a forum (not a category) and open, the topic open
// (moderators bypass both); title 3–120 characters, content 2–20000; flood interval and 30 posts an
// hour per member (moderators bypass); the Discussion screen's disallowed keys reject the post;
// optional first-post moderation.

const (
	forumTitleMin     = 3
	forumTitleMax     = 120
	forumContentMin   = 2
	forumContentMax   = 20000
	forumHourlyLimit  = 30
	forumSearchMaxLen = 100
)

// forumModOps are the moderation operations the front-end mod bar may send.
var forumModOps = map[string]bool{"stick": true, "unstick": true, "close": true, "open": true, "approve": true,
	"spam": true, "unspam": true, "trash": true, "restore": true}

// forumPostView is one post (a topic's opening post or a reply) as the forum/part-post partial draws
// it, with the page it belongs to (for translations, nonces and rights).
type forumPostView struct {
	Page                 *forumPage
	ID                   int64
	Lead                 bool // the topic's opening post
	Title                string
	Author               *forumAuthor
	Content              string
	CreatedAt            time.Time
	EditedAt             *time.Time
	Status               string
	Spam, Sticky, Closed bool
	Position             int
	IP                   string
	Permalink            string // the post's own link (#post-<id> on the topic page)
	ActionURL            string // where the mod bar posts (the topic URL)
	// v0.2.5.
	Likes     int  // likes of the post
	Liked     bool // the viewer likes it
	Solution  bool // the topic's accepted answer
	Solved    bool // the topic has an accepted answer
	CanLike   bool // the viewer may like it (a member who may post, not their own post)
	CanSolve  bool // the viewer may mark it as the solution (the topic's author or a moderator; replies only)
	CanReport bool // the viewer may report it
}

// forumHit is one search result or profile activity entry: a topic, or a reply with its topic.
type forumHit struct {
	Topic   *forumTopic
	Reply   *forumReply // nil for a topic
	Excerpt string      // plain text
	URL     string
	At      time.Time
	Author  *forumAuthor
}

// forumGroup is a block of the forum index: a category with its forums, or the top-level forums
// that sit in no category.
type forumGroup struct {
	Category *forumForum
	Forums   []*forumForum
}

// Page helpers the templates call ---------------------------------------------------------------

// CanEdit reports whether the viewer may edit post id (precomputed by the view).
func (p *forumPage) CanEdit(id int64) bool {
	if p == nil {
		return false
	}
	m, _ := p.Extra["can_edit"].(map[int64]bool)
	return m[id]
}

// EditURL is the edit form of post id.
func (p *forumPage) EditURL(id int64) string {
	if p == nil {
		return ""
	}
	base, _ := p.Extra["edit_base"].(string)
	if base == "" {
		return ""
	}
	return base + strconv.FormatInt(id, 10) + "/"
}

// TopicURL is the URL of the topic being viewed (where its forms post).
func (p *forumPage) TopicURL() string {
	if p == nil || p.Topic == nil {
		return ""
	}
	return p.Topic.URL
}

// LeadPost is the opening post of the topic being viewed.
func (p *forumPage) LeadPost() *forumPostView {
	if p == nil || p.Topic == nil {
		return nil
	}
	t := p.Topic
	v := &forumPostView{Page: p, ID: t.ID, Lead: true, Title: t.Title, Author: t.Author, Content: t.Content, CreatedAt: t.CreatedAt,
		EditedAt: t.EditedAt, Status: t.Status, Spam: t.Spam, Sticky: t.Sticky, Closed: t.Closed, IP: t.IP,
		Permalink: t.URL + "#post-" + strconv.FormatInt(t.ID, 10), ActionURL: t.URL}
	p.forumPostEngage(v, t.Likes, false)
	return v
}

// ReplyPost is reply r of the topic being viewed.
func (p *forumPage) ReplyPost(r *forumReply) *forumPostView {
	if p == nil || r == nil {
		return nil
	}
	v := &forumPostView{Page: p, ID: r.ID, Author: r.Author, Content: r.Content, CreatedAt: r.CreatedAt, EditedAt: r.EditedAt,
		Status: r.Status, Spam: r.Spam, IP: r.IP, Permalink: r.URL}
	if r.Status == "publish" && !r.Spam {
		v.Position = r.Position // a pending reply has no number yet (it would repeat its neighbour's)
	}
	if p.Topic != nil {
		v.ActionURL = p.Topic.URL
		base, _ := p.Extra["page_url"].(string)
		if base == "" {
			base = p.Topic.URL
		}
		v.Permalink = base + "#post-" + strconv.FormatInt(r.ID, 10)
	}
	p.forumPostEngage(v, r.Likes, r.Solution)
	return v
}

// FormValue is a re-filled form field.
func (p *forumPage) FormValue(k string) string {
	if p == nil || p.Form == nil {
		return ""
	}
	return p.Form[k]
}

// Groups splits the forum tree into index blocks: the top-level forums outside categories first,
// then each category with its forums.
func (p *forumPage) Groups() []forumGroup {
	if p == nil {
		return nil
	}
	return forumGroupsOf(p.Forums)
}

func forumGroupsOf(list []*forumForum) []forumGroup {
	var loose []*forumForum
	var out []forumGroup
	for _, f := range list {
		if f.Kind == "category" {
			out = append(out, forumGroup{Category: f, Forums: f.Children})
			continue
		}
		loose = append(loose, f)
	}
	if len(loose) > 0 {
		out = append([]forumGroup{{Forums: loose}}, out...)
	}
	return out
}

// Ago is "5 mins ago" in the viewer's language.
func (p *forumPage) Ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	var cat *i18n.Catalog
	if p != nil {
		cat = p.cat
	}
	return p.Tf("%s ago", forumTimeDiff(cat, t))
}

// forumTimeDiff is "5 mins" from t to now with the units in cat's language (English when nil).
func forumTimeDiff(cat *i18n.Catalog, t time.Time) string {
	var n func(string, string, int) string
	if cat != nil {
		n = cat.N
	}
	return wp.HumanTimeDiffN(t, time.Now(), n)
}

// Count is "1 reply" / "5 replies" style text.
func (p *forumPage) Count(n int, one, many string) string {
	if n == 1 {
		return p.Tf(one, n)
	}
	return p.Tf(many, n)
}

// Levels and badges (WP7): the chip helpers print the stored names; these translate the default
// names (and descriptions) into the viewer's language first. A custom name without a translation
// prints as typed.

// tx translates msg, keeping "" as "" (an empty msgid would return a catalog header).
func (p *forumPage) tx(msg string) string {
	if msg == "" {
		return ""
	}
	return p.T(msg)
}

// LevelChip is the chip of level l with its name translated ("" for nil).
func (p *forumPage) LevelChip(l *forumLevel) template.HTML {
	if l == nil {
		return ""
	}
	c := *l
	c.Name = p.tx(c.Name)
	return forumLevelHTML(&c)
}

// LevelName is l's translated name ("" for nil).
func (p *forumPage) LevelName(l *forumLevel) string {
	if l == nil {
		return ""
	}
	return p.tx(l.Name)
}

// forumBadgeFallbackIcon is drawn for a badge with neither an image nor a known icon where only the
// picture shows (icon-only rows, the profile list), so it never renders as an empty disc.
const forumBadgeFallbackIcon = "medal"

// BadgeChip is the chip of badge b with its name and description translated (an icon-only row:
// a badge without a picture gets the fallback icon).
func (p *forumPage) BadgeChip(b forumBadge) template.HTML {
	b.Name, b.Description = p.tx(b.Name), p.tx(b.Description)
	if b.ImageURL == "" && !forumIconValid(b.Icon) {
		b.Icon = forumBadgeFallbackIcon
	}
	return forumBadgeHTML(b)
}

// BadgeText is b's translated name and description.
func (p *forumPage) BadgeText(b forumBadge) [2]string {
	return [2]string{p.tx(b.Name), p.tx(b.Description)}
}

// FewBadges is a's first n badges (list order); MoreBadges the number left out.
func (p *forumPage) FewBadges(a *forumAuthor, n int) []forumBadge {
	if a == nil {
		return nil
	}
	if len(a.Badges) <= n {
		return a.Badges
	}
	return a.Badges[:n]
}

func (p *forumPage) MoreBadges(a *forumAuthor, n int) int {
	if a == nil || len(a.Badges) <= n {
		return 0
	}
	return len(a.Badges) - n
}

// BadgeIcon is b's picture at size px: its Media Library image, else its icon ("" for neither).
func (p *forumPage) BadgeIcon(b forumBadge, size int) template.HTML {
	if size <= 0 || size > 128 {
		size = 20
	}
	if b.ImageURL != "" {
		n := strconv.Itoa(size)
		return template.HTML(`<img class="pf-badge-item__img" src="` + html.EscapeString(b.ImageURL) + `" alt="" width="` + n + `" height="` + n +
			`" loading="lazy" decoding="async" />`)
	}
	if !forumIconValid(b.Icon) {
		return forumIcon(forumBadgeFallbackIcon, size)
	}
	return forumIcon(b.Icon, size)
}

// ChipStyle is the style attribute value carrying a badge or level colour (#hex validated).
func (p *forumPage) ChipStyle(color string) template.CSS {
	return template.CSS("--pf-chip:" + forumColor(color, forumDefaultColor))
}

// ProgressStyle is the style attribute value of a .pf-level-progress bar (0..100 %).
func (p *forumPage) ProgressStyle(n int) template.CSS {
	return template.CSS("--pf-progress:" + strconv.Itoa(min(max(n, 0), 100)) + "%")
}

// MemberAuthor is the author of a member list row (for forum_avatar).
func (p *forumPage) MemberAuthor(m *forumMember) *forumAuthor {
	if m == nil {
		return nil
	}
	return m.forumAuthor
}

// PointsToNext is how many points a still needs for the next level (0 at the top).
func (p *forumPage) PointsToNext(a *forumAuthor) int {
	if a == nil || a.NextLevel == nil {
		return 0
	}
	return max(a.NextLevel.Min-a.Points, 0)
}

// Rules ---------------------------------------------------------------------------------------------

// forumTextLen is the visible length of post HTML (tags stripped, entities decoded, trimmed).
func forumTextLen(content string) int {
	return utf8.RuneCountInString(strings.TrimSpace(html.UnescapeString(wp.StripTags(content))))
}

// forumBlockEnds keeps words of adjacent blocks apart when tags are stripped.
var forumBlockEnds = regexp.MustCompile(`(?i)</p>|<br|</li>|</blockquote>|</pre>|</h|</div>`)

// forumExcerpt is the first words of post HTML as plain text.
func forumExcerpt(content string, words int) string {
	content = forumBlockEnds.ReplaceAllString(content, " $0")
	f := strings.Fields(html.UnescapeString(wp.StripTags(content)))
	if len(f) <= words {
		return strings.Join(f, " ")
	}
	return strings.Join(f[:words], " ") + "…"
}

// forumCleanTitle is a typed title on one line: whitespace runs (newlines, tabs) become one space
// and control characters go. Titles stay plain text; every template escapes them.
func forumCleanTitle(raw string) string {
	raw = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, raw)
	return strings.Join(strings.Fields(raw), " ")
}

// forumCheckTitle validates a topic title; "" when fine.
func forumCheckTitle(title string) string {
	n := utf8.RuneCountInString(title)
	switch {
	case n < forumTitleMin:
		return "The title is too short (at least 3 characters)."
	case n > forumTitleMax:
		return "The title is too long (at most 120 characters)."
	}
	return ""
}

// forumCheckContent validates the raw and the cleaned content; "" when fine.
func forumCheckContent(raw, clean string) string {
	if utf8.RuneCountInString(raw) > forumContentMax || utf8.RuneCountInString(clean) > forumContentMax {
		return "The text is too long (at most 20000 characters)."
	}
	if forumTextLen(clean) < forumContentMin {
		return "Please write a little more (at least 2 characters)."
	}
	return ""
}

// forumDisallowed reports whether the post hits the Discussion screen's disallowed keys (checked
// against the text, the member's names and email, the address and the user agent).
func (s *Server) forumDisallowed(c *gin.Context, u *store.User, parts ...string) bool {
	return s.forumKeysMatch(c, u, "disallowed_keys", parts...)
}

// forumKeysMatch reports whether one of the keys of the Discussion option (disallowed_keys or
// moderation_keys, one per line) occurs in the text, the member's names and email, the address or
// the user agent (case-insensitive), as wp_check_comment_disallowed_list and check_comment do.
func (s *Server) forumKeysMatch(c *gin.Context, u *store.User, option string, parts ...string) bool {
	keys := s.settings.String(option)
	if strings.TrimSpace(keys) == "" {
		return false
	}
	parts = append(parts, clientIP(c), c.Request.UserAgent())
	if u != nil {
		parts = append(parts, u.Login, u.Email, u.DisplayName)
	}
	hay := strings.ToLower(strings.Join(parts, "\n"))
	for _, k := range strings.Split(keys, "\n") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

// forumHeldForLinks reports whether the parts carry the Discussion screen's comment_max_links or
// more links (0 turns the check off). Each link counts once: an anchor's href and its URL are one
// match, so <a href="https://…"> is one link, not two.
func (s *Server) forumHeldForLinks(parts ...string) bool {
	max := s.settings.Int("comment_max_links")
	if max <= 0 {
		return false
	}
	n := 0
	for _, p := range parts {
		n += commentLinkCount(p)
	}
	return n >= max
}

// forumModerationHeld is bbp_check_for_moderation for a new topic or reply of a non-moderator:
// too many links, or a moderation key, holds the post for a moderator (pending) instead of
// publishing it. Links are counted on the cleaned content (what is stored and shown).
func (s *Server) forumModerationHeld(c *gin.Context, u *store.User, title, raw, clean string) bool {
	return s.forumHeldForLinks(clean) || s.forumKeysMatch(c, u, "moderation_keys", title, raw)
}

// forumThrottle is the flood and hourly limit of a member who is not a moderator: the message (a
// format taking the seconds to wait) when they must wait (status 429), "" otherwise. It fails
// closed: a database error refuses the post (err) rather than skipping the limits. The caller holds
// forumPostLock, so parallel posts of one member are checked one after the other.
func (s *Server) forumThrottle(ctx context.Context, u *store.User, st forumSettings) (string, int, error) {
	if u.Can(capForumModerate, nil) {
		return "", 0, nil
	}
	if st.FloodSeconds > 0 {
		last, err := s.forumLastPostAt(ctx, u.ID)
		if err != nil {
			return "", 0, err
		}
		if !last.IsZero() {
			if wait := time.Duration(st.FloodSeconds)*time.Second - time.Since(last); wait > 0 {
				secs := int(wait.Round(time.Second) / time.Second)
				if secs < 1 {
					secs = 1
				}
				return "You are posting too quickly. Please wait %s seconds and try again.", secs, nil
			}
		}
	}
	var n int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE author_id = $1 AND type IN ('topic','reply')
		AND created_at > now() - interval '1 hour'`, u.ID).Scan(&n); err != nil {
		return "", 0, err
	}
	if n >= forumHourlyLimit {
		return "You have reached the limit of 30 posts an hour. Please try again later.", 0, nil
	}
	return "", 0, nil
}

// forumPostLock takes the member's posting lock (a transaction-scoped advisory lock held until
// release): the flood check, the hourly count and the insert of one member run one request at a
// time, so a double submit or parallel requests cannot all pass the limits. Release after the insert.
func (s *Server) forumPostLock(ctx context.Context, userID int64) (release func(), err error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	release = func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }
	if err := forumXactLock(ctx, tx, forumLockPost, userID); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// forumCanEdit reports whether u may edit a post by authorID created at created: moderators always;
// the author while they may post, within the edit window, and not once it is in the trash.
func forumCanEdit(u *store.User, st forumSettings, authorID int64, created time.Time, status string, unverified bool) bool {
	if u == nil {
		return false
	}
	if u.Can(capForumModerate, nil) {
		return true
	}
	if u.ID != authorID || authorID == 0 || status == "trash" || !u.Can(capForumParticipate, nil) || (st.RequireVerify && unverified) {
		return false
	}
	return st.EditWindow > 0 && time.Since(created) < time.Duration(st.EditWindow)*time.Minute
}

// forumPostGate is the reason u cannot post right now ("" when they can), with its status.
func forumPostGate(u *store.User, st forumSettings) (int, string) {
	if !u.Can(capForumParticipate, nil) {
		return http.StatusForbidden, "Your account cannot post in the forums."
	}
	if st.RequireVerify && forumMetaBool(u.Meta, forumUMetaUnverified) {
		return http.StatusForbidden, "Please confirm your email address to start posting."
	}
	return 0, ""
}

// forumNoStore marks a response private (a page showing forms or a member's own state).
func forumNoStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header(cache.HeaderControl, "no-store")
}

// Queries ---------------------------------------------------------------------------------------------

// forumPublishedForumsSQL keeps topics (alias t) whose forum is published.
func forumPublishedForumsSQL(t string) string {
	return `EXISTS (SELECT 1 FROM posts f WHERE f.id = ` + t + `.parent_id AND f.type = 'forum' AND f.status = 'publish')`
}

// forumRecentTopics are the n most recently active published topics of published forums.
func (s *Server) forumRecentTopics(ctx context.Context, n int) ([]*forumTopic, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+forumPrefixed("p")+` FROM posts p WHERE p.type = 'topic' AND p.status = 'publish'
		AND `+forumNotSpam("p")+` AND `+forumPublishedForumsSQL("p")+`
		ORDER BY p.meta->>'`+forumMetaLastActive+`' DESC, p.id DESC LIMIT $1`, n) // posts_forum_recent
	if err != nil {
		return nil, err
	}
	posts, err := forumScanPosts(rows)
	if err != nil {
		return nil, err
	}
	return s.forumTopicsFrom(ctx, posts, false)
}

// forumActivity lists published topics and replies (newest first) of published topics in published
// forums: matching term (search) and/or by authorID (profile). It returns one page and the total.
func (s *Server) forumActivity(ctx context.Context, term string, authorID int64, page, per int) ([]*forumHit, int, error) {
	args := []any{}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	where := []string{"p.type IN ('topic','reply')", "p.status = 'publish'", forumNotSpam("p"), "t.status = 'publish'", forumNotSpam("t"),
		forumPublishedForumsSQL("t")}
	if term = strings.TrimSpace(term); term != "" {
		where = append(where, "p.search @@ websearch_to_tsquery('simple', "+store.FoldedArgSQL(arg(term))+")") // folded like posts.search (0105)
	}
	if authorID > 0 {
		where = append(where, "p.author_id = "+arg(authorID))
	}
	from := ` FROM posts p JOIN posts t ON t.type = 'topic' AND t.id = (CASE WHEN p.type = 'reply' THEN p.parent_id ELSE p.id END) WHERE ` +
		strings.Join(where, " AND ")
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page, per = forumPaging(page, per)
	rows, err := s.store.Pool.Query(ctx, `SELECT p.id, p.type, t.id`+from+` ORDER BY p.published_at DESC, p.id DESC LIMIT `+
		strconv.Itoa(per)+` OFFSET `+strconv.Itoa((page-1)*per), args...)
	if err != nil {
		return nil, 0, err
	}
	type ref struct {
		id, topic int64
		typ       string
	}
	var refs []ref
	var topicIDs, replyIDs []int64
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.id, &r.typ, &r.topic); err != nil {
			rows.Close()
			return nil, 0, err
		}
		refs = append(refs, r)
		topicIDs = append(topicIDs, r.topic)
		if r.typ == "reply" {
			replyIDs = append(replyIDs, r.id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	topics := map[int64]*forumTopic{}
	if ids := forumIDs(topicIDs...); len(ids) > 0 {
		trows, err := s.store.Pool.Query(ctx, `SELECT `+forumPrefixed("p")+` FROM posts p WHERE p.id = ANY($1)`, ids)
		if err != nil {
			return nil, 0, err
		}
		posts, err := forumScanPosts(trows)
		if err != nil {
			return nil, 0, err
		}
		list, err := s.forumTopicsFrom(ctx, posts, false)
		if err != nil {
			return nil, 0, err
		}
		for _, t := range list {
			topics[t.ID] = t
		}
	}
	replies := map[int64]*forumReply{}
	if len(replyIDs) > 0 {
		rrows, err := s.store.Pool.Query(ctx, `SELECT `+forumPrefixed("p")+`, `+forumPositionSQL+` FROM posts p WHERE p.id = ANY($1)`, replyIDs)
		if err != nil {
			return nil, 0, err
		}
		list, err := s.forumRepliesFrom(ctx, rrows, false)
		if err != nil {
			return nil, 0, err
		}
		for _, r := range list {
			replies[r.ID] = r
		}
	}
	st := s.forumLoad()
	out := make([]*forumHit, 0, len(refs))
	for _, r := range refs {
		t := topics[r.topic]
		if t == nil {
			continue
		}
		h := &forumHit{Topic: t, URL: t.URL, At: t.CreatedAt, Author: t.Author, Excerpt: forumExcerpt(t.Content, 40)}
		if r.typ == "reply" {
			rp := replies[r.id]
			if rp == nil {
				continue
			}
			h.Reply, h.At, h.Author, h.Excerpt = rp, rp.CreatedAt, rp.Author, forumExcerpt(rp.Content, 40)
			h.URL = s.forumTopicURL(ctx, t, s.forumReplyPage(ctx, rp, st.RepliesPerPage)) + "#post-" + strconv.FormatInt(rp.ID, 10)
		}
		out = append(out, h)
	}
	return out, total, nil
}

// forumReplyAnchor is the topic page holding reply r for viewer, with its #post-<id> anchor. A
// moderator's topic pages also list pending replies, so their page counts those too.
func (s *Server) forumReplyAnchor(ctx context.Context, t *forumTopic, r *forumReply, viewer *store.User) string {
	per := s.forumLoad().RepliesPerPage
	page := 0
	if viewer.Can(capForumModerate, nil) && per > 0 {
		if pos, err := s.forumModPosition(ctx, r.ID); err == nil && pos > 0 {
			page = (pos + per - 1) / per
		}
	}
	if page == 0 {
		page = s.forumReplyPage(ctx, r, per)
	}
	return s.forumTopicURL(ctx, t, page) + "#post-" + strconv.FormatInt(r.ID, 10)
}

// forumModPosition is a reply's 1-based position in a moderator's topic view: published and
// pending replies, no spam, in forumReplies' order.
func (s *Server) forumModPosition(ctx context.Context, replyID int64) (int, error) {
	var n int
	err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM posts p JOIN posts r2 ON r2.parent_id = p.parent_id
		WHERE p.id = $1 AND p.type = 'reply' AND r2.type = 'reply' AND r2.status IN ('publish','pending')
		AND coalesce(r2.meta->>'_forum_spam','') <> 'true' AND (r2.published_at, r2.id) <= (p.published_at, p.id)`, replyID).Scan(&n)
	return n, err
}

// forumWithMsg adds ?forum_msg=<code> to a URL (before its fragment).
func forumWithMsg(u, code string) string {
	frag := ""
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u, frag = u[:i], u[i:]
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "forum_msg=" + url.QueryEscape(code) + frag
}

// POST handlers ------------------------------------------------------------------------------------

// forumFrontPost handles new_topic, reply, edit, moderate, block and unblock. Visitors are sent to
// the login form; a bad nonce is 403.
func (s *Server) forumFrontPost(c *gin.Context, rt forumRoute, action string) {
	u := currentUser(c)
	if u == nil {
		back := c.Request.URL.Path
		if safeRedirect(back, "") == "" {
			back = forumLocalPath(s.forumURL(c.Request.Context()))
		}
		c.Redirect(http.StatusFound, s.forumURL(c.Request.Context(), "login")+"?redirect="+url.QueryEscape(back))
		return
	}
	if !s.checkNonce(c, forumNonce) {
		s.denyNonce(c)
		return
	}
	forumNoStore(c)
	switch action {
	case "new_topic":
		s.forumPostTopic(c, rt, u)
	case "reply":
		s.forumPostReply(c, rt, u)
	case "edit":
		s.forumPostEdit(c, rt, u)
	case "moderate":
		s.forumPostModerate(c, rt, u)
	case "block", "unblock":
		s.forumPostBlock(c, rt, u, action == "block")
	case "badge_award", "badge_revoke":
		s.forumPostBadge(c, rt, u, action == "badge_award")
	case "like", "unlike", "solve", "unsolve", "subscribe", "unsubscribe", "report":
		s.forumPostEngageAction(c, rt, u, action) // v0.2.5 (forum_engage_post.go)
	case "draft_delete":
		_ = s.forumSaveDraft(c.Request.Context(), u.ID, nil)
		c.Redirect(http.StatusFound, forumWithMsg(s.forumURL(c.Request.Context(), "new"), "draft_deleted"))
	default:
		c.String(http.StatusBadRequest, "Unknown forum action.")
	}
}

// forumFail re-renders a form view with an error (status 422, 403 or 429) and the typed values; a
// non-zero n fills the message's %s.
func (s *Server) forumFail(c *gin.Context, rt forumRoute, view string, status int, msg string, form map[string]string, n ...int) {
	p := s.forumNewPage(c, view, "")
	p.Error = p.T(msg)
	if len(n) > 0 && n[0] > 0 {
		p.Error = p.Tf(msg, n[0])
	}
	for k, v := range form {
		p.Form[k] = v
	}
	p.Extra["status"] = status
	if !s.forumServe(c, rt, p) && !c.Writer.Written() {
		s.forumNotFound(c)
	}
}

// forumPostTopic creates a topic in the forum of the new-topic (or forum) URL.
func (s *Server) forumPostTopic(c *gin.Context, rt forumRoute, u *store.User) {
	ctx := c.Request.Context()
	if rt.View != forumViewNewTopic && rt.View != forumViewForum {
		c.String(http.StatusBadRequest, "Unknown forum action.")
		return
	}
	rt.View = forumViewNewTopic
	st := s.forumLoad()
	title := forumCleanTitle(c.PostForm("title"))
	raw := c.PostForm("content")
	tags := c.PostForm("tags")
	notify := c.PostForm("notify") != ""
	form := map[string]string{"title": title, "content": raw, "tags": tags, "forum": c.PostForm("forum")}
	if notify {
		form["notify"] = "1"
	} else {
		form["notify"] = ""
	}
	// v0.2.5: the picker's forum field names the forum (/forums/new/ has none in its URL).
	var f *forumForum
	var err error
	if v := strings.TrimSpace(c.PostForm("forum")); v != "" {
		id, _ := strconv.ParseInt(v, 10, 64)
		f, err = s.forumByID(ctx, id)
	} else if rt.Slug != "" {
		f, err = s.forumBySlug(ctx, rt.Slug)
	} else {
		err = errForumNotFound
	}
	// v0.2.5: save a draft, or show a preview, instead of posting.
	if c.PostForm("draft") != "" {
		d := &forumDraft{Title: title, Content: raw, Tags: tags, Notify: notify}
		if f != nil {
			d.ForumID = f.ID
		}
		if code, msg := forumPostGate(u, st); code != 0 {
			s.forumFail(c, rt, forumViewNewTopic, code, msg, form)
			return
		}
		if serr := s.forumSaveDraft(ctx, u.ID, d); serr != nil {
			s.forumFail(c, rt, forumViewNewTopic, http.StatusServiceUnavailable, "Something went wrong. Please try again.", form)
			return
		}
		dest := s.forumURL(ctx, "new")
		if rt.Slug != "" {
			dest = s.forumURL(ctx, "f", rt.Slug, "new")
		}
		c.Redirect(http.StatusFound, forumWithMsg(dest, "draft_saved"))
		return
	}
	if err != nil && rt.Slug == "" && c.PostForm("preview") == "" {
		s.forumFail(c, rt, forumViewNewTopic, http.StatusUnprocessableEntity, "Please choose a forum.", form)
		return
	}
	if err != nil && c.PostForm("preview") == "" || (f != nil && f.Kind != "forum") {
		s.forumNotFound(c)
		return
	}
	if code, msg := forumPostGate(u, st); code != 0 {
		s.forumFail(c, rt, forumViewNewTopic, code, msg, form)
		return
	}
	if c.PostForm("preview") != "" {
		s.forumPreview(c, rt, forumViewNewTopic, u, raw, form)
		return
	}
	t, pending, no := s.forumStartTopic(c, u, forumTopicIn{Forum: f, Title: title, Raw: raw, Tags: tags, Form: true})
	if no != nil {
		s.forumFail(c, rt, forumViewNewTopic, no.Status, no.Msg, form, no.Secs)
		return
	}
	if notify {
		_ = s.forumSubscribe(ctx, u.ID, t.ID, true)
	}
	if forumDraftOf(u) != nil {
		_ = s.forumSaveDraft(ctx, u.ID, nil) // posted: the draft is done
	}
	dest := t.URL
	if pending {
		dest = forumWithMsg(dest, "topic_pending")
	}
	c.Redirect(http.StatusFound, dest)
}

// forumTopicIn is a new topic as a member sends it, from the site's form or the REST API.
type forumTopicIn struct {
	Forum            *forumForum
	Title, Raw, Tags string
	// Form: the site's form, whose comment traps (honeypot, time token) apply.
	Form bool
}

// forumRefusal is why a post was refused: the status and the message for the member (a format
// taking Secs when Secs is not 0).
type forumRefusal struct {
	Status int
	Msg    string
	Secs   int
}

// forumStartTopic runs every check a new topic passes, whatever sends it (the new-topic form and
// POST pursae/v1/forum/topics), and creates it: who may post (forum_participate, a confirmed email
// address when the forums ask for one), whether the forum takes new topics (closed and staff-only
// forums take them from moderators only), the form's traps, the member's HTML filter (forumClean),
// the title and text limits, the disallowed words, the flood and hourly limits, and the moderation
// hold (a first post, links, moderation words). pending reports a topic waiting for a moderator.
func (s *Server) forumStartTopic(c *gin.Context, u *store.User, in forumTopicIn) (t *forumTopic, pending bool, no *forumRefusal) {
	ctx := c.Request.Context()
	st := s.forumLoad()
	if code, msg := forumPostGate(u, st); code != 0 {
		return nil, false, &forumRefusal{Status: code, Msg: msg}
	}
	f := in.Forum
	mod := u.Can(capForumModerate, nil)
	if !forumCanStart(f, mod) {
		msg := "This forum is closed to new topics."
		if f != nil && f.StaffOnly && !f.Closed {
			msg = "Only the team starts topics in this forum."
		}
		return nil, false, &forumRefusal{Status: http.StatusForbidden, Msg: msg}
	}
	if in.Form && !s.commentTrapsPassed(c, 0) {
		return nil, false, &forumRefusal{Status: http.StatusUnprocessableEntity, Msg: "Your post could not be accepted. Please try again."}
	}
	title, raw := forumCleanTitle(in.Title), in.Raw
	clean := forumClean(u, raw)
	if msg := forumCheckTitle(title); msg != "" {
		return nil, false, &forumRefusal{Status: http.StatusUnprocessableEntity, Msg: msg}
	}
	if msg := forumCheckContent(raw, clean); msg != "" {
		return nil, false, &forumRefusal{Status: http.StatusUnprocessableEntity, Msg: msg}
	}
	if !mod && s.forumDisallowed(c, u, title, raw) {
		return nil, false, &forumRefusal{Status: http.StatusUnprocessableEntity, Msg: "Your post contains words that are not allowed here."}
	}
	release, err := s.forumPostLock(ctx, u.ID)
	if err != nil {
		return nil, false, &forumRefusal{Status: http.StatusServiceUnavailable, Msg: "Something went wrong. Please try again."}
	}
	defer release()
	msg, secs, err := s.forumThrottle(ctx, u, st)
	if err != nil {
		return nil, false, &forumRefusal{Status: http.StatusServiceUnavailable, Msg: "Something went wrong. Please try again."}
	}
	if msg != "" {
		return nil, false, &forumRefusal{Status: http.StatusTooManyRequests, Msg: msg, Secs: secs}
	}
	if st.ModerateFirstPost && !mod {
		// Fails closed: when the check errs, the post waits for a moderator.
		if ok, err := s.forumHasPublished(ctx, u.ID); err != nil || !ok {
			pending = true
		}
	}
	if !mod && !pending && s.forumModerationHeld(c, u, title, raw, clean) {
		pending = true
	}
	t, err = s.forumCreateTopic(ctx, forumNewTopic{ForumID: f.ID, AuthorID: u.ID, Title: title, Content: clean, IP: clientIP(c), Pending: pending, Tags: in.Tags})
	if err != nil {
		return nil, false, &forumRefusal{Status: http.StatusUnprocessableEntity, Msg: "The topic could not be saved. Please try again."}
	}
	return t, pending, nil
}

// forumPostReply adds a reply to the topic of the URL.
func (s *Server) forumPostReply(c *gin.Context, rt forumRoute, u *store.User) {
	ctx := c.Request.Context()
	if rt.View != forumViewTopic {
		c.String(http.StatusBadRequest, "Unknown forum action.")
		return
	}
	t, err := s.forumTopicByID(ctx, rt.ID)
	if err != nil || t.Status != "publish" || t.Spam {
		s.forumNotFound(c)
		return
	}
	f, err := s.forumByID(ctx, t.ForumID)
	mod := u.Can(capForumModerate, nil)
	if err != nil && !mod {
		s.forumNotFound(c)
		return
	}
	st := s.forumLoad()
	raw := c.PostForm("content")
	notify := c.PostForm("notify") != ""
	form := map[string]string{"content": raw}
	if notify {
		form["notify"] = "1"
	}
	if code, msg := forumPostGate(u, st); code != 0 {
		s.forumFail(c, rt, forumViewTopic, code, msg, form)
		return
	}
	if c.PostForm("preview") != "" { // v0.2.5
		s.forumPreview(c, rt, forumViewTopic, u, raw, form)
		return
	}
	if !mod && (t.Closed || (f != nil && f.Closed)) {
		s.forumFail(c, rt, forumViewTopic, http.StatusForbidden, "This topic is closed to new replies.", form)
		return
	}
	if !s.commentTrapsPassed(c, 0) {
		s.forumFail(c, rt, forumViewTopic, http.StatusUnprocessableEntity, "Your post could not be accepted. Please try again.", form)
		return
	}
	clean := forumClean(u, raw)
	if msg := forumCheckContent(raw, clean); msg != "" {
		s.forumFail(c, rt, forumViewTopic, http.StatusUnprocessableEntity, msg, form)
		return
	}
	if !mod && s.forumDisallowed(c, u, raw) {
		s.forumFail(c, rt, forumViewTopic, http.StatusUnprocessableEntity, "Your post contains words that are not allowed here.", form)
		return
	}
	release, err := s.forumPostLock(ctx, u.ID)
	if err != nil {
		s.forumFail(c, rt, forumViewTopic, http.StatusServiceUnavailable, "Something went wrong. Please try again.", form)
		return
	}
	defer release()
	msg, secs, err := s.forumThrottle(ctx, u, st)
	if err != nil {
		s.forumFail(c, rt, forumViewTopic, http.StatusServiceUnavailable, "Something went wrong. Please try again.", form)
		return
	}
	if msg != "" {
		s.forumFail(c, rt, forumViewTopic, http.StatusTooManyRequests, msg, form, secs)
		return
	}
	pending := false
	if st.ModerateFirstPost && !mod {
		// Fails closed: when the check errs, the post waits for a moderator.
		if ok, err := s.forumHasPublished(ctx, u.ID); err != nil || !ok {
			pending = true
		}
	}
	if !mod && !pending && s.forumModerationHeld(c, u, "", raw, clean) {
		pending = true
	}
	r, err := s.forumCreateReply(ctx, forumNewReply{TopicID: t.ID, AuthorID: u.ID, Content: clean, IP: clientIP(c), Pending: pending})
	if err != nil {
		s.forumFail(c, rt, forumViewTopic, http.StatusUnprocessableEntity, "The reply could not be saved. Please try again.", form)
		return
	}
	if notify {
		_ = s.forumSubscribe(ctx, u.ID, t.ID, true)
	}
	if !pending {
		s.forumNotifyReply(context.WithoutCancel(ctx), r.ID) // v0.2.5: the topic's followers (queued mail jobs)
	}
	if pending {
		c.Redirect(http.StatusFound, forumWithMsg(t.URL, "reply_pending"))
		return
	}
	c.Redirect(http.StatusFound, s.forumReplyAnchor(ctx, t, r, u))
}

// forumEditTarget loads the topic or reply of an edit URL (with its topic).
func (s *Server) forumEditTarget(ctx context.Context, id int64) (t *forumTopic, r *forumReply, err error) {
	typ, topicID, _, err := s.forumParents(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	t, err = s.forumTopicByID(ctx, topicID)
	if err != nil {
		return nil, nil, err
	}
	if typ == "reply" {
		r, err = s.forumReplyByID(ctx, id)
		if err != nil {
			return nil, nil, err
		}
	}
	return t, r, nil
}

// forumPostEdit saves an edited topic or reply.
func (s *Server) forumPostEdit(c *gin.Context, rt forumRoute, u *store.User) {
	ctx := c.Request.Context()
	if rt.View != forumViewEdit {
		c.String(http.StatusBadRequest, "Unknown forum action.")
		return
	}
	t, r, err := s.forumEditTarget(ctx, rt.ID)
	if err != nil {
		s.forumNotFound(c)
		return
	}
	// The same visibility rule as the edit view: a post in a trashed or spammed topic (or forum)
	// is gone for its author too.
	if _, ferr := s.forumByID(ctx, t.ForumID); !forumTopicVisible(u, t, ferr == nil) {
		s.forumNotFound(c)
		return
	}
	st := s.forumLoad()
	author, created, status := t.AuthorID, t.CreatedAt, t.Status
	if r != nil {
		author, created, status = r.AuthorID, r.CreatedAt, r.Status
	}
	raw := c.PostForm("content")
	title := forumCleanTitle(c.PostForm("title"))
	form := map[string]string{"content": raw, "title": title}
	if !forumCanEdit(u, st, author, created, status, forumMetaBool(u.Meta, forumUMetaUnverified)) {
		s.forumFail(c, rt, forumViewEdit, http.StatusForbidden, "You can no longer edit this post.", form)
		return
	}
	mod := u.Can(capForumModerate, nil)
	clean := forumClean(u, raw)
	var titlePtr *string
	if r == nil {
		if msg := forumCheckTitle(title); msg != "" {
			s.forumFail(c, rt, forumViewEdit, http.StatusUnprocessableEntity, msg, form)
			return
		}
		titlePtr = &title
	}
	if msg := forumCheckContent(raw, clean); msg != "" {
		s.forumFail(c, rt, forumViewEdit, http.StatusUnprocessableEntity, msg, form)
		return
	}
	if !mod && s.forumDisallowed(c, u, title, raw) {
		s.forumFail(c, rt, forumViewEdit, http.StatusUnprocessableEntity, "Your post contains words that are not allowed here.", form)
		return
	}
	// The link limit and moderation keys hold new posts only (see the file comment): re-holding an
	// edited post would need a status change and a different redirect, so edits are left alone.
	if err := s.forumUpdateContent(ctx, rt.ID, titlePtr, clean); err != nil {
		s.forumFail(c, rt, forumViewEdit, http.StatusUnprocessableEntity, "The post could not be saved. Please try again.", form)
		return
	}
	if r != nil {
		c.Redirect(http.StatusFound, s.forumReplyAnchor(ctx, t, r, u))
		return
	}
	fresh, err := s.forumTopicByID(ctx, t.ID)
	if err != nil {
		fresh = t
	}
	c.Redirect(http.StatusFound, fresh.URL)
}

// forumPostModerate applies a mod bar operation to a topic or reply (moderators only).
func (s *Server) forumPostModerate(c *gin.Context, rt forumRoute, u *store.User) {
	ctx := c.Request.Context()
	if !u.Can(capForumModerate, nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to moderate the forums.")
		return
	}
	op := c.PostForm("op")
	id, _ := strconv.ParseInt(c.PostForm("post_id"), 10, 64)
	if !forumModOps[op] || id <= 0 {
		c.String(http.StatusBadRequest, "Unknown moderation action.")
		return
	}
	t, r, err := s.forumEditTarget(ctx, id)
	if err != nil {
		s.forumNotFound(c)
		return
	}
	if err := s.forumModerate(ctx, id, op, u.ID); err != nil {
		c.String(http.StatusBadRequest, "This action does not apply to this post.")
		return
	}
	dest := t.URL
	if r != nil {
		if fresh, err := s.forumReplyByID(ctx, r.ID); err == nil && fresh.Status == "publish" && !fresh.Spam {
			dest = s.forumReplyAnchor(ctx, t, fresh, u)
		} else if err == nil && fresh.Status == "pending" {
			dest = t.URL + "#post-" + strconv.FormatInt(r.ID, 10)
		}
	}
	c.Redirect(http.StatusFound, forumWithMsg(dest, "saved"))
}

// forumPostBlock blocks (deny exception on forum_participate) or unblocks the member of a profile
// URL. Moderators may block members; only administrators (manage_options) may block a moderator;
// nobody blocks themselves or an administrator.
func (s *Server) forumPostBlock(c *gin.Context, rt forumRoute, u *store.User, block bool) {
	ctx := c.Request.Context()
	if rt.View != forumViewProfile {
		c.String(http.StatusBadRequest, "Unknown forum action.")
		return
	}
	if !u.Can(capForumModerate, nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to block members.")
		return
	}
	found, err := s.store.UserByNicename(ctx, rt.Slug)
	if err != nil {
		s.forumNotFound(c)
		return
	}
	target, err := s.store.UserByID(ctx, found.ID)
	if err != nil {
		s.forumNotFound(c)
		return
	}
	if code, msg := forumBlockGate(u, target); code != 0 {
		c.String(code, msg)
		return
	}
	exc, err := s.store.UserCapExceptions(ctx, target.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, "The member could not be updated.")
		return
	}
	access := s.userAccessKey(ctx, target.ID)
	if block {
		exc[capForumParticipate] = "deny"
	} else if exc[capForumParticipate] == "deny" {
		delete(exc, capForumParticipate)
	}
	if err := s.store.SetUserCapExceptions(ctx, target.ID, exc); err != nil {
		c.String(http.StatusInternalServerError, "The member could not be updated.")
		return
	}
	// Blocking ends the member's open sessions, like any other access change.
	s.endSessionsAfterAccessChange(ctx, c, target.ID, access)
	s.forumPurge(ctx)
	msg := "unblocked"
	if block {
		msg = "blocked"
	}
	c.Redirect(http.StatusFound, forumWithMsg(s.forumProfileURL(ctx, target.Nicename), msg))
}

// forumPostBadge awards a badge to (or revokes a manual award from) the member of a profile URL.
// Administrators only (manage_options); a badge earned by its rule stays after a revoke.
func (s *Server) forumPostBadge(c *gin.Context, rt forumRoute, u *store.User, award bool) {
	ctx := c.Request.Context()
	if rt.View != forumViewProfile {
		c.String(http.StatusBadRequest, "Unknown forum action.")
		return
	}
	if !u.Can("manage_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to award badges.")
		return
	}
	found, err := s.store.UserByNicename(ctx, rt.Slug)
	if err != nil {
		s.forumNotFound(c)
		return
	}
	id := strings.TrimSpace(c.PostForm("badge"))
	if _, ok := s.forumBadgeByID(ctx, id); !ok {
		s.forumFail(c, rt, forumViewProfile, http.StatusUnprocessableEntity, "Please choose a badge.", nil)
		return
	}
	if award {
		err = s.forumAward(ctx, id, found.ID)
	} else {
		err = s.forumRevoke(ctx, id, found.ID)
	}
	if err != nil {
		s.forumFail(c, rt, forumViewProfile, http.StatusInternalServerError, "The badge could not be saved.", nil)
		return
	}
	msg := "badge_revoked"
	if award {
		msg = "badge_awarded"
	}
	c.Redirect(http.StatusFound, forumWithMsg(s.forumProfileURL(ctx, found.Nicename), msg))
}

// forumBlockGate is why actor may not block or unblock target (0 when they may).
func forumBlockGate(actor, target *store.User) (int, string) {
	switch {
	case actor == nil || target == nil:
		return http.StatusForbidden, "Sorry, you are not allowed to block members."
	case actor.ID == target.ID:
		return http.StatusForbidden, "You cannot block yourself."
	case target.Can("manage_options", nil):
		return http.StatusForbidden, "Administrators cannot be blocked."
	case target.Can(capForumModerate, nil) && !actor.Can("manage_options", nil):
		return http.StatusForbidden, "Only an administrator can block a moderator."
	}
	return 0, ""
}

// forumCanBlock reports whether the mod bar of a profile offers block / unblock.
func forumCanBlock(actor, target *store.User) bool {
	if actor == nil || !actor.Can(capForumModerate, nil) {
		return false
	}
	code, _ := forumBlockGate(actor, target)
	return code == 0
}
