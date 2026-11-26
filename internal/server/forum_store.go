package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/microcosm-cc/bluemonday"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Pursae Forum data access. Forums, topics and replies are rows of posts (types forum, topic and
// reply); counters live in posts.meta and are always recomputed from SQL (forumRecount), never
// incremented. Every write recounts its topic and forum and purges the forum cache tag.

var errForumNotFound = errors.New("forum: not found")

// forumNotSpam is the SQL condition "not flagged as spam" for alias a.
func forumNotSpam(a string) string {
	return "coalesce(" + a + ".meta->>'" + forumMetaSpam + "','') <> 'true'"
}

// forumSQLLiveParent keeps a topic, or a reply whose topic is published and not spam (alias p):
// replies inside a trashed or spammed topic count nowhere, as in forumActivity and forumRecount.
const forumSQLLiveParent = `(p.type <> 'reply' OR EXISTS (SELECT 1 FROM posts lt WHERE lt.id = p.parent_id AND lt.type = 'topic'
	AND lt.status = 'publish' AND coalesce(lt.meta->>'` + forumMetaSpam + `','') <> 'true'))`

// forumSQLInPublishedForum keeps a topic or reply (alias p) whose forum is published.
const forumSQLInPublishedForum = `EXISTS (SELECT 1 FROM posts ft JOIN posts ff ON ff.id = ft.parent_id AND ff.type = 'forum' AND ff.status = 'publish'
	WHERE ft.id = CASE WHEN p.type = 'reply' THEN p.parent_id ELSE p.id END AND ft.type = 'topic')`

// Meta helpers -----------------------------------------------------------------------------------

func forumMetaInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func forumMetaInt64(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func forumMetaStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func forumMetaBool(m map[string]any, k string) bool {
	switch v := m[k].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	case float64:
		return v != 0
	}
	return false
}

func forumMetaTime(m map[string]any, k string) time.Time {
	t, err := time.Parse(forumTimeLayout, forumMetaStr(m, k))
	if err != nil {
		return time.Time{}
	}
	return t
}

func forumMetaTimePtr(m map[string]any, k string) *time.Time {
	if t := forumMetaTime(m, k); !t.IsZero() {
		return &t
	}
	return nil
}

func forumStamp(t time.Time) string { return t.UTC().Format(forumTimeLayout) }

// forumIDs keeps the distinct non-zero ids.
func forumIDs(ids ...int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Forums ------------------------------------------------------------------------------------------

// forumFromPost maps a forum post (URL and counters filled, last topic and author left to the caller).
func (s *Server) forumFromPost(ctx context.Context, p *store.Post) *forumForum {
	f := &forumForum{ID: p.ID, ParentID: p.ParentID, Slug: p.Slug, Title: p.Title, Description: template.HTML(wp.KsesPost(p.Content)),
		Kind: forumMetaStr(p.Meta, forumMetaKind), Closed: forumMetaStr(p.Meta, forumMetaStatus) == "closed", Order: p.MenuOrder,
		TopicCount: forumMetaInt(p.Meta, forumMetaTopicCount), ReplyCount: forumMetaInt(p.Meta, forumMetaReplyCount),
		LastActive: forumMetaTime(p.Meta, forumMetaLastActive), LastTopicID: forumMetaInt64(p.Meta, forumMetaLastTopic)}
	if f.Kind != "category" {
		f.Kind = "forum"
	}
	f.URL = s.forumForumURL(ctx, f)
	s.forumApplyDesign(ctx, f, p.Meta)
	return f
}

// forumFill adds the last topic's title and URL and the last author to forums.
func (s *Server) forumFill(ctx context.Context, list []*forumForum, lastAuthor map[int64]int64) error {
	var topicIDs, authorIDs []int64
	for _, f := range list {
		topicIDs = append(topicIDs, f.LastTopicID)
		authorIDs = append(authorIDs, lastAuthor[f.ID])
	}
	topics, err := s.store.PostsByIDs(ctx, forumIDs(topicIDs...))
	if err != nil {
		return err
	}
	authors, err := s.forumAuthors(ctx, forumIDs(authorIDs...))
	if err != nil {
		return err
	}
	for _, f := range list {
		if t := topics[f.LastTopicID]; t != nil && t.Type == "topic" {
			f.LastTopicTitle = t.Title
			f.LastTopicURL = s.forumTopicURL(ctx, &forumTopic{ID: t.ID, Slug: t.Slug}, 1)
		}
		f.LastAuthor = authors[lastAuthor[f.ID]]
	}
	return nil
}

// forumList is the forum tree: published categories and forums by menu order, then title. A
// category's counters and last activity sum up its forums.
func (s *Server) forumList(ctx context.Context) ([]*forumForum, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+forumPostCols+` FROM posts WHERE type = 'forum' AND status = 'publish'
		ORDER BY menu_order, lower(title), id`)
	if err != nil {
		return nil, err
	}
	posts, err := forumScanPosts(rows)
	if err != nil {
		return nil, err
	}
	all := make([]*forumForum, 0, len(posts))
	byID := map[int64]*forumForum{}
	lastAuthor := map[int64]int64{}
	for _, p := range posts {
		f := s.forumFromPost(ctx, p)
		all = append(all, f)
		byID[f.ID] = f
		lastAuthor[f.ID] = forumMetaInt64(p.Meta, forumMetaLastAuthor)
	}
	var roots []*forumForum
	for _, f := range all {
		if parent := byID[f.ParentID]; parent != nil && parent.ParentID == 0 && f.ParentID != f.ID {
			parent.Children = append(parent.Children, f)
			continue
		}
		roots = append(roots, f)
	}
	// Categories sum up their forums.
	for _, f := range roots {
		if f.Kind != "category" {
			continue
		}
		for _, ch := range f.Children {
			f.TopicCount += ch.TopicCount
			f.ReplyCount += ch.ReplyCount
			if ch.LastActive.After(f.LastActive) {
				f.LastActive, f.LastTopicID = ch.LastActive, ch.LastTopicID
				lastAuthor[f.ID] = lastAuthor[ch.ID]
			}
		}
	}
	if err := s.forumFill(ctx, all, lastAuthor); err != nil {
		return nil, err
	}
	return roots, nil
}

// forumPostCols are store's post columns (forumScanPosts reads them).
const forumPostCols = `id, type, coalesce(author_id,0), coalesce(parent_id,0), slug, title, excerpt, content, status,
	coalesce(trashed_status,''), password, published_at, modified_at, created_at, coalesce(featured_media_id,0),
	menu_order, comment_status, ping_status, comment_count, sticky, format, meta`

func forumScanPost(row pgx.Row, extra ...any) (*store.Post, error) {
	p := &store.Post{}
	var meta []byte
	dest := []any{&p.ID, &p.Type, &p.AuthorID, &p.ParentID, &p.Slug, &p.Title, &p.Excerpt, &p.Content, &p.Status,
		&p.TrashedStatus, &p.Password, &p.PublishedAt, &p.ModifiedAt, &p.CreatedAt, &p.FeaturedMediaID,
		&p.MenuOrder, &p.CommentStatus, &p.PingStatus, &p.CommentCount, &p.Sticky, &p.Format, &meta}
	err := row.Scan(append(dest, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errForumNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Meta = map[string]any{}
	_ = json.Unmarshal(meta, &p.Meta)
	return p, nil
}

func forumScanPosts(rows pgx.Rows) ([]*store.Post, error) {
	defer rows.Close()
	var out []*store.Post
	for rows.Next() {
		p, err := forumScanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) forumOne(ctx context.Context, where string, args ...any) (*forumForum, error) {
	p, err := forumScanPost(s.store.Pool.QueryRow(ctx, `SELECT `+forumPostCols+` FROM posts WHERE type = 'forum' AND status = 'publish' AND `+where+` ORDER BY id LIMIT 1`, args...))
	if err != nil {
		return nil, err
	}
	f := s.forumFromPost(ctx, p)
	if err := s.forumFill(ctx, []*forumForum{f}, map[int64]int64{f.ID: forumMetaInt64(p.Meta, forumMetaLastAuthor)}); err != nil {
		return nil, err
	}
	return f, nil
}

// forumBySlug is a published forum or category by slug (Children not filled).
func (s *Server) forumBySlug(ctx context.Context, slug string) (*forumForum, error) {
	return s.forumOne(ctx, "slug = $1", slug)
}

// forumByID is a published forum or category by id (Children not filled).
func (s *Server) forumByID(ctx context.Context, id int64) (*forumForum, error) {
	return s.forumOne(ctx, "id = $1", id)
}

// Topics ------------------------------------------------------------------------------------------

// forumTopicQuery filters forumTopics. Statuses defaults to publish; Spam selects the spam-flagged
// rows instead of the others. WithIP fills forumTopic.IP (moderator views only).
type forumTopicQuery struct {
	ForumID, AuthorID int64
	Search            string
	Statuses          []string // default publish
	Spam              bool
	Page, PerPage     int
	StickyFirst       bool
	WithIP            bool   // addition to the design: IP is filled only when asked (moderators)
	Sort              string // v0.2.5: latest (default) | popular | unanswered | solved
	Tag               string // v0.2.5: only topics with this tag slug
}

func forumPaging(page, perPage int) (int, int) {
	if perPage <= 0 {
		perPage = 20
	}
	if perPage > 200 {
		perPage = 200
	}
	if page < 1 {
		page = 1
	}
	if page > 100000 {
		page = 100000
	}
	return page, perPage
}

// forumTopicsFrom maps topic posts, with authors, last authors, recent participants, forum titles and
// design meta, tags, views, likes and the solved state (forumEnrichTopics).
func (s *Server) forumTopicsFrom(ctx context.Context, posts []*store.Post, withIP bool) ([]*forumTopic, error) {
	var authorIDs, forumIDList []int64
	for _, p := range posts {
		authorIDs = append(authorIDs, p.AuthorID, forumMetaInt64(p.Meta, forumMetaLastAuthor))
		forumIDList = append(forumIDList, p.ParentID)
	}
	forums, err := s.store.PostsByIDs(ctx, forumIDs(forumIDList...))
	if err != nil {
		return nil, err
	}
	out := make([]*forumTopic, 0, len(posts))
	metas := map[int64]map[string]any{}
	for _, p := range posts {
		t := &forumTopic{ID: p.ID, ForumID: p.ParentID, AuthorID: p.AuthorID, Slug: p.Slug, Title: p.Title, Content: p.Content,
			Status: p.Status, Spam: forumMetaBool(p.Meta, forumMetaSpam), Sticky: p.Sticky, Closed: p.CommentStatus == "closed",
			ReplyCount: forumMetaInt(p.Meta, forumMetaReplyCount), CreatedAt: p.Date(), LastActive: forumMetaTime(p.Meta, forumMetaLastActive),
			EditedAt: forumMetaTimePtr(p.Meta, forumMetaEdited), LastReplyID: forumMetaInt64(p.Meta, forumMetaLastReply)}
		if t.LastActive.IsZero() {
			t.LastActive = t.CreatedAt
		}
		t.URL = s.forumTopicURL(ctx, t, 1)
		if f := forums[p.ParentID]; f != nil && f.Type == "forum" {
			t.ForumTitle = f.Title
			t.ForumURL = s.forumURL(ctx, "f", f.Slug)
			if ic := forumMetaStr(f.Meta, forumMetaIcon); forumIconValid(ic) {
				t.ForumIcon = ic
			}
			t.ForumColor, t.ForumColorHex, t.ForumColorInk, t.ForumColorStyle = forumColorParts(forumMetaStr(f.Meta, forumMetaColor))
			t.ForumLabel = forumMetaStr(f.Meta, forumMetaLabel)
		}
		if withIP {
			t.IP = forumMetaStr(p.Meta, forumMetaIP)
		}
		metas[p.ID] = p.Meta
		out = append(out, t)
	}
	parts := s.forumEnrichTopics(ctx, out, metas)
	for _, ids := range parts {
		authorIDs = append(authorIDs, ids...)
	}
	authors, err := s.forumAuthors(ctx, forumIDs(authorIDs...))
	if err != nil {
		return nil, err
	}
	for i, p := range posts {
		t := out[i]
		t.Author, t.LastAuthor = authors[p.AuthorID], authors[forumMetaInt64(p.Meta, forumMetaLastAuthor)]
		for _, id := range parts[t.ID] {
			if a := authors[id]; a != nil {
				t.Participants = append(t.Participants, a)
			}
		}
	}
	return out, nil
}

// forumStatusesSQL is the status and spam condition of a forum query.
func forumStatusesSQL(statuses []string, spam bool, arg func(any) string) string {
	if len(statuses) == 0 {
		statuses = []string{"publish"}
	}
	cond := "p.status = ANY(" + arg(statuses) + ")"
	if spam {
		return cond + " AND coalesce(p.meta->>'" + forumMetaSpam + "','') = 'true'"
	}
	return cond + " AND " + forumNotSpam("p")
}

// forumTopics is one page of topics and the total. Topics sort by last activity; StickyFirst puts
// the forum's sticky topics first (sticky is per forum, there is no global super-sticky).
func (s *Server) forumTopics(ctx context.Context, q forumTopicQuery) ([]*forumTopic, int, error) {
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	where := []string{"p.type = 'topic'", forumStatusesSQL(q.Statuses, q.Spam, arg)}
	if q.ForumID > 0 {
		where = append(where, "p.parent_id = "+arg(q.ForumID))
	} else if q.AuthorID == 0 {
		where = append(where, forumPublishedForumsSQL("p")) // cross-forum lists: topics of published forums only
	}
	if q.AuthorID > 0 {
		where = append(where, "p.author_id = "+arg(q.AuthorID))
	}
	if term := strings.TrimSpace(q.Search); term != "" {
		where = append(where, "p.search @@ websearch_to_tsquery('simple', "+store.FoldedArgSQL(arg(term))+")") // folded like posts.search (0105)
	}
	if q.Tag != "" {
		where = append(where, "p.meta->'"+forumMetaTags+"' ? "+arg(q.Tag))
	}
	switch q.Sort {
	case "unanswered":
		where = append(where, "coalesce(p.meta->>'"+forumMetaReplyCount+"','0') = '0'")
	case "solved":
		where = append(where, "p.meta ? '"+forumMetaSolution+"'")
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM posts p WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page, per := forumPaging(q.Page, q.PerPage)
	// No NULLS LAST: create and recount always write last_active, and the ORDER BY must match
	// posts_forum_topics / posts_forum_recent (DESC = NULLS FIRST) for an index scan.
	order := "p.meta->>'" + forumMetaLastActive + "' DESC, p.id DESC"
	join := ""
	if q.Sort == "popular" { // views plus ten per reply, then activity
		join = " LEFT JOIN forum_topic_views v ON v.topic_id = p.id"
		order = "(coalesce(v.views,0) + 10 * coalesce((p.meta->>'" + forumMetaReplyCount + "')::bigint,0)) DESC, " + order
	}
	if q.StickyFirst && q.ForumID > 0 && (q.Sort == "" || q.Sort == "latest") { // sticky is per forum: no global super-sticky on cross-forum lists
		order = "p.sticky DESC, " + order
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT `+forumPrefixed("p")+` FROM posts p`+join+` WHERE `+cond+` ORDER BY `+order+
		fmt.Sprintf(" LIMIT %d OFFSET %d", per, (page-1)*per), args...)
	if err != nil {
		return nil, 0, err
	}
	posts, err := forumScanPosts(rows)
	if err != nil {
		return nil, 0, err
	}
	out, err := s.forumTopicsFrom(ctx, posts, q.WithIP)
	return out, total, err
}

// forumPrefixed is forumPostCols with a table alias.
func forumPrefixed(a string) string {
	return a + `.id, ` + a + `.type, coalesce(` + a + `.author_id,0), coalesce(` + a + `.parent_id,0), ` + a + `.slug, ` + a + `.title, ` +
		a + `.excerpt, ` + a + `.content, ` + a + `.status, coalesce(` + a + `.trashed_status,''), ` + a + `.password, ` + a + `.published_at, ` +
		a + `.modified_at, ` + a + `.created_at, coalesce(` + a + `.featured_media_id,0), ` + a + `.menu_order, ` + a + `.comment_status, ` +
		a + `.ping_status, ` + a + `.comment_count, ` + a + `.sticky, ` + a + `.format, ` + a + `.meta`
}

// forumTopicByID is a topic of any status (IP not filled: use forumIPOf for moderators).
func (s *Server) forumTopicByID(ctx context.Context, id int64) (*forumTopic, error) {
	p, err := forumScanPost(s.store.Pool.QueryRow(ctx, `SELECT `+forumPostCols+` FROM posts WHERE id = $1 AND type = 'topic'`, id))
	if err != nil {
		return nil, err
	}
	list, err := s.forumTopicsFrom(ctx, []*store.Post{p}, false)
	if err != nil {
		return nil, err
	}
	return list[0], nil
}

// forumIPOf is the address a topic or reply was posted from (moderators only). A missing post or a
// cancelled request is not worth reporting (a missing IP meta key is already "" via coalesce), but a
// real database fault is logged rather than shown as a blank IP indistinguishable from "no IP stored";
// the moderator page still renders either way.
func (s *Server) forumIPOf(ctx context.Context, postID int64) string {
	var ip string
	if err := s.store.Pool.QueryRow(ctx, `SELECT coalesce(meta->>'`+forumMetaIP+`','') FROM posts WHERE id = $1 AND type IN ('topic','reply')`, postID).Scan(&ip); err != nil && !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, context.Canceled) {
		log.Printf("forum: ip of post %d: %v", postID, err)
	}
	return ip
}

// Replies -----------------------------------------------------------------------------------------

// forumReplyQuery filters forumReplies (Statuses default publish; Newest reverses the order).
type forumReplyQuery struct {
	TopicID, AuthorID int64
	Search            string
	Statuses          []string
	Spam              bool
	Page, PerPage     int
	Newest            bool
	WithIP            bool // addition to the design: IP is filled only when asked (moderators)
}

// forumPositionSQL is a reply's 1-based position among its topic's published replies.
const forumPositionSQL = `(SELECT count(*) FROM posts r2 WHERE r2.type = 'reply' AND r2.parent_id = p.parent_id AND r2.status = 'publish'
	AND coalesce(r2.meta->>'_forum_spam','') <> 'true' AND (r2.published_at, r2.id) <= (p.published_at, p.id))`

func (s *Server) forumRepliesFrom(ctx context.Context, rows pgx.Rows, withIP bool) ([]*forumReply, error) {
	defer rows.Close()
	var posts []*store.Post
	var positions []int
	for rows.Next() {
		var pos int
		p, err := forumScanPost(rows, &pos)
		if err != nil {
			return nil, err
		}
		posts = append(posts, p)
		positions = append(positions, pos)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var authorIDs []int64
	for _, p := range posts {
		authorIDs = append(authorIDs, p.AuthorID)
	}
	authors, err := s.forumAuthors(ctx, forumIDs(authorIDs...))
	if err != nil {
		return nil, err
	}
	out := make([]*forumReply, 0, len(posts))
	for i, p := range posts {
		r := &forumReply{ID: p.ID, TopicID: p.ParentID, ForumID: forumMetaInt64(p.Meta, forumMetaForumID), AuthorID: p.AuthorID,
			Content: p.Content, Status: p.Status, Spam: forumMetaBool(p.Meta, forumMetaSpam), CreatedAt: p.Date(),
			EditedAt: forumMetaTimePtr(p.Meta, forumMetaEdited), Author: authors[p.AuthorID], Position: positions[i]}
		r.URL = s.forumReplyURL(ctx, r)
		if withIP {
			r.IP = forumMetaStr(p.Meta, forumMetaIP)
		}
		out = append(out, r)
	}
	if len(out) > 0 { // v0.2.5: likes, one query
		ids := make([]int64, len(out))
		for i, r := range out {
			ids[i] = r.ID
		}
		likes := s.forumLikeCounts(ctx, ids)
		for _, r := range out {
			r.Likes = likes[r.ID]
		}
	}
	return out, nil
}

// forumReplies is one page of replies (oldest first) and the total.
func (s *Server) forumReplies(ctx context.Context, q forumReplyQuery) ([]*forumReply, int, error) {
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	where := []string{"p.type = 'reply'", forumStatusesSQL(q.Statuses, q.Spam, arg)}
	if q.TopicID > 0 {
		where = append(where, "p.parent_id = "+arg(q.TopicID))
	}
	if q.AuthorID > 0 {
		where = append(where, "p.author_id = "+arg(q.AuthorID))
	}
	if term := strings.TrimSpace(q.Search); term != "" {
		where = append(where, "p.search @@ websearch_to_tsquery('simple', "+store.FoldedArgSQL(arg(term))+")") // folded like posts.search (0105)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM posts p WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page, per := forumPaging(q.Page, q.PerPage)
	dir := "ASC"
	if q.Newest {
		dir = "DESC"
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT `+forumPrefixed("p")+`, `+forumPositionSQL+` FROM posts p WHERE `+cond+
		` ORDER BY p.published_at `+dir+`, p.id `+dir+fmt.Sprintf(" LIMIT %d OFFSET %d", per, (page-1)*per), args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := s.forumRepliesFrom(ctx, rows, q.WithIP)
	return out, total, err
}

// forumReplyByID is a reply of any status (IP not filled).
func (s *Server) forumReplyByID(ctx context.Context, id int64) (*forumReply, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+forumPrefixed("p")+`, `+forumPositionSQL+` FROM posts p WHERE p.id = $1 AND p.type = 'reply'`, id)
	if err != nil {
		return nil, err
	}
	list, err := s.forumRepliesFrom(ctx, rows, false)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errForumNotFound
	}
	return list[0], nil
}

// forumReplyPage is the topic page (perPage replies each) that shows the reply.
func (s *Server) forumReplyPage(ctx context.Context, r *forumReply, perPage int) int {
	if r == nil {
		return 1
	}
	if perPage <= 0 {
		perPage = s.forumLoad().RepliesPerPage
	}
	pos := r.Position
	if pos <= 0 {
		if fresh, err := s.forumReplyByID(ctx, r.ID); err == nil {
			pos = fresh.Position
		}
	}
	if pos <= 1 {
		return 1
	}
	return int(math.Ceil(float64(pos) / float64(perPage)))
}

// Writes ------------------------------------------------------------------------------------------

// forumNewTopic is a topic to create; At zero = now (the demo importer sets it).
type forumNewTopic struct {
	ForumID, AuthorID int64
	Title, Content    string
	IP                string
	Pending           bool
	At                time.Time
	Tags              string // v0.2.5: typed tags (forumParseTags)
}

func forumNullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// forumSlugOf is a topic slug from its title (cosmetic: topic URLs carry the id).
func forumSlugOf(title string) string {
	slug := wp.SanitizeTitle(title)
	if r := []rune(slug); len(r) > 180 {
		slug = strings.Trim(string(r[:180]), "-")
	}
	if slug == "" {
		slug = "topic"
	}
	return slug
}

// forumCreateTopic stores a topic in a forum (kind forum, not a category) and recounts the forum.
// The caller checks permissions, the closed state and content rules; Content must be clean (forumClean).
func (s *Server) forumCreateTopic(ctx context.Context, in forumNewTopic) (*forumTopic, error) {
	var kind string
	err := s.store.Pool.QueryRow(ctx, `SELECT coalesce(meta->>'`+forumMetaKind+`','forum') FROM posts WHERE id = $1 AND type = 'forum' AND status = 'publish'`, in.ForumID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errForumNotFound
	}
	if err != nil {
		return nil, err
	}
	if kind == "category" {
		return nil, errors.New("forum: topics cannot be posted in a category")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, errors.New("forum: the topic needs a title")
	}
	at := in.At
	if at.IsZero() {
		at = time.Now()
	}
	status := "publish"
	if in.Pending {
		status = "pending"
	}
	m := map[string]any{forumMetaReplyCount: 0, forumMetaLastActive: forumStamp(at), forumMetaLastAuthor: in.AuthorID, forumMetaIP: in.IP}
	if names, slugs := forumParseTags(in.Tags); len(slugs) > 0 {
		for k, v := range forumTagsMeta(names, slugs) {
			m[k] = v
		}
	}
	meta, _ := json.Marshal(m)
	var id int64
	if err := s.store.Pool.QueryRow(ctx, `INSERT INTO posts (type, author_id, parent_id, slug, title, content, status, published_at, modified_at,
		created_at, comment_status, ping_status, meta) VALUES ('topic', $1, $2, $3, $4, $5, $6, $7, $7, $7, 'open', 'closed', $8) RETURNING id`,
		forumNullID(in.AuthorID), in.ForumID, forumSlugOf(title), title, in.Content, status, at, meta).Scan(&id); err != nil {
		return nil, err
	}
	s.forumLedger(ctx, in.AuthorID, "topic", id, 0, at)
	bg := s.forumAfterWrite(ctx, id, in.ForumID)
	return s.forumTopicByID(bg, id)
}

// forumNewReply is a reply to create; At zero = now.
type forumNewReply struct {
	TopicID, AuthorID int64
	Content, IP       string
	Pending           bool
	At                time.Time
}

// forumCreateReply stores a reply to a topic and recounts the topic and its forum. The caller checks
// permissions and the closed states; Content must be clean (forumClean).
func (s *Server) forumCreateReply(ctx context.Context, in forumNewReply) (*forumReply, error) {
	var forumID int64
	err := s.store.Pool.QueryRow(ctx, `SELECT coalesce(parent_id,0) FROM posts WHERE id = $1 AND type = 'topic'`, in.TopicID).Scan(&forumID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errForumNotFound
	}
	if err != nil {
		return nil, err
	}
	at := in.At
	if at.IsZero() {
		at = time.Now()
	}
	status := "publish"
	if in.Pending {
		status = "pending"
	}
	meta, _ := json.Marshal(map[string]any{forumMetaForumID: forumID, forumMetaIP: in.IP})
	var id int64
	if err := s.store.Pool.QueryRow(ctx, `INSERT INTO posts (type, author_id, parent_id, slug, title, content, status, published_at, modified_at,
		created_at, comment_status, ping_status, meta) VALUES ('reply', $1, $2, '', '', $3, $4, $5, $5, $5, 'closed', 'closed', $6) RETURNING id`,
		forumNullID(in.AuthorID), in.TopicID, in.Content, status, at, meta).Scan(&id); err != nil {
		return nil, err
	}
	s.forumLedger(ctx, in.AuthorID, "reply", id, 0, at)
	bg := s.forumAfterWrite(ctx, in.TopicID, forumID)
	return s.forumReplyByID(bg, id)
}

// forumParents is a topic's or reply's type, topic id and forum id.
func (s *Server) forumParents(ctx context.Context, postID int64) (typ string, topicID, forumID int64, err error) {
	var parent int64
	err = s.store.Pool.QueryRow(ctx, `SELECT type, coalesce(parent_id,0) FROM posts WHERE id = $1 AND type IN ('topic','reply')`, postID).Scan(&typ, &parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, 0, errForumNotFound
	}
	if err != nil {
		return "", 0, 0, err
	}
	if typ == "topic" {
		return typ, postID, parent, nil
	}
	err = s.store.Pool.QueryRow(ctx, `SELECT coalesce(parent_id,0) FROM posts WHERE id = $1`, parent).Scan(&forumID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return typ, parent, forumID, err
}

// forumUpdateContent edits a topic (title when non-nil) or reply and marks it edited.
func (s *Server) forumUpdateContent(ctx context.Context, postID int64, title *string, content string) error {
	typ, topicID, forumID, err := s.forumParents(ctx, postID)
	if err != nil {
		return err
	}
	edited, _ := json.Marshal(map[string]any{forumMetaEdited: forumStamp(time.Now())})
	if title != nil && typ == "topic" {
		t := strings.TrimSpace(*title)
		if t == "" {
			return errors.New("forum: the topic needs a title")
		}
		_, err = s.store.Pool.Exec(ctx, `UPDATE posts SET title = $2, slug = $3, content = $4, modified_at = now(), updated_at = now(),
			meta = coalesce(meta,'{}') || $5::jsonb WHERE id = $1`, postID, t, forumSlugOf(t), content, edited)
	} else {
		_, err = s.store.Pool.Exec(ctx, `UPDATE posts SET content = $2, modified_at = now(), updated_at = now(),
			meta = coalesce(meta,'{}') || $3::jsonb WHERE id = $1`, postID, content, edited)
	}
	if err != nil {
		return err
	}
	s.forumAfterWrite(ctx, topicID, forumID)
	return nil
}

// forumAfterWrite recounts what holds a post that was just written and purges the cached forum
// pages. Once the write committed it runs detached from the request (a member who closes the tab
// must not leave stale counters or cached pages hiding the post), and a recount error is logged,
// not returned: the post is saved, and telling the member otherwise invites a duplicate. It returns
// the detached context for the caller's reload.
func (s *Server) forumAfterWrite(ctx context.Context, topicID, forumID int64) context.Context {
	bg := context.WithoutCancel(ctx)
	if err := s.forumRecount(bg, topicID, forumID); err != nil {
		log.Printf("forum: recount topic %d forum %d: %v", topicID, forumID, err)
	}
	s.forumPurge(bg)
	return bg
}

// forumModerate applies a moderation op: stick|unstick and close|open (topics), approve, spam,
// unspam, trash and restore (topics and replies). Spam is status trash plus _forum_spam (posts have
// no spam status). The caller checks the actor's capability.
func (s *Server) forumModerate(ctx context.Context, postID int64, op string, actorID int64) error {
	typ, topicID, forumID, err := s.forumParents(ctx, postID)
	if err != nil {
		return err
	}
	mod, _ := json.Marshal(map[string]any{"_forum_mod": map[string]any{"op": op, "by": actorID, "at": forumStamp(time.Now())}})
	var q string
	switch op {
	case "stick", "unstick", "close", "open":
		if typ != "topic" {
			return fmt.Errorf("forum: %s applies to topics only", op)
		}
		switch op {
		case "stick":
			q = `UPDATE posts SET sticky = true WHERE id = $1`
		case "unstick":
			q = `UPDATE posts SET sticky = false WHERE id = $1`
		case "close":
			q = `UPDATE posts SET comment_status = 'closed' WHERE id = $1`
		case "open":
			q = `UPDATE posts SET comment_status = 'open' WHERE id = $1`
		}
	case "approve":
		q = `UPDATE posts SET status = 'publish' WHERE id = $1 AND status = 'pending'`
	case "spam":
		q = `UPDATE posts SET trashed_status = CASE WHEN status = 'trash' THEN trashed_status ELSE status END, status = 'trash',
			deleted_at = coalesce(deleted_at, now()), meta = coalesce(meta,'{}') || '{"_forum_spam": true}'::jsonb WHERE id = $1`
	case "trash":
		q = `UPDATE posts SET trashed_status = CASE WHEN status = 'trash' THEN trashed_status ELSE status END, status = 'trash',
			deleted_at = coalesce(deleted_at, now()) WHERE id = $1`
	case "unspam", "restore":
		q = `UPDATE posts SET status = CASE WHEN status = 'trash' THEN coalesce(nullif(trashed_status,''), 'publish') ELSE status END,
			trashed_status = NULL, deleted_at = NULL, meta = coalesce(meta,'{}') - '_forum_spam' WHERE id = $1`
	default:
		return fmt.Errorf("forum: unknown moderation op %q", op)
	}
	if err := pgx.BeginFunc(ctx, s.store.Pool, func(tx pgx.Tx) error { // the op and its record together
		if _, err := tx.Exec(ctx, q, postID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE posts SET meta = coalesce(meta,'{}') || $2::jsonb, updated_at = now() WHERE id = $1`, postID, mod)
		return err
	}); err != nil {
		return err
	}
	switch op {
	case "spam", "trash": // v0.2.5: the post's reports are dealt with; a removed accepted answer no longer solves its topic
		s.forumResolveReports(ctx, actorID, postID)
		if typ == "reply" {
			s.forumDropSolution(ctx, topicID, postID)
		}
	case "approve": // v0.2.5: subscribers hear about a reply once it is public
		if typ == "reply" {
			defer s.forumNotifyReply(context.WithoutCancel(ctx), postID)
		}
	}
	s.forumAfterWrite(ctx, topicID, forumID)
	return nil
}

// forumForumInput is a forum or category to create (ID 0) or update.
type forumForumInput struct {
	ID, ParentID                   int64
	Title, Slug, Description, Kind string
	Closed                         bool
	Order                          int
	Design                         *forumDesignInput // v0.2.5: icon, colour, label, chips …; nil keeps the stored meta
}

// forumSaveForum creates or updates a forum or category: unique slug among forums, at most two levels
// (a category holds forums; categories stay at the top).
func (s *Server) forumSaveForum(ctx context.Context, in forumForumInput, authorID int64) (int64, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return 0, errors.New("forum: the forum needs a name")
	}
	kind := in.Kind
	if kind == "" && in.ID > 0 { // an update without a kind keeps the current one
		err := s.store.Pool.QueryRow(ctx, `SELECT coalesce(meta->>'`+forumMetaKind+`','forum') FROM posts WHERE id = $1 AND type = 'forum'`, in.ID).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errForumNotFound
		}
		if err != nil {
			return 0, err
		}
	}
	if kind != "category" {
		kind = "forum"
	}
	if in.ParentID > 0 {
		if kind == "category" {
			return 0, errors.New("forum: a category cannot have a parent")
		}
		if in.ParentID == in.ID {
			return 0, errors.New("forum: a forum cannot be its own parent")
		}
		var pk string
		var pp int64
		err := s.store.Pool.QueryRow(ctx, `SELECT coalesce(meta->>'`+forumMetaKind+`','forum'), coalesce(parent_id,0) FROM posts WHERE id = $1 AND type = 'forum'`, in.ParentID).Scan(&pk, &pp)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("forum: the parent does not exist")
		}
		if err != nil {
			return 0, err
		}
		if pk != "category" || pp != 0 {
			return 0, errors.New("forum: the parent must be a top-level category")
		}
	}
	if in.ID > 0 && in.Kind != "" {
		var children int
		if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE type = 'forum' AND parent_id = $1`, in.ID).Scan(&children); err != nil {
			return 0, err
		}
		if children > 0 && kind != "category" {
			return 0, errors.New("forum: a category with forums cannot become a forum")
		}
		var topics int
		if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE type = 'topic' AND parent_id = $1`, in.ID).Scan(&topics); err != nil {
			return 0, err
		}
		if topics > 0 && kind == "category" {
			return 0, errors.New("forum: a forum with topics cannot become a category")
		}
	}
	base := wp.SanitizeTitle(in.Slug)
	if base == "" && in.ID > 0 { // an update without a slug keeps the forum's URL
		if err := s.store.Pool.QueryRow(ctx, `SELECT slug FROM posts WHERE id = $1 AND type = 'forum'`, in.ID).Scan(&base); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
	}
	if base == "" {
		base = wp.SanitizeTitle(title)
	}
	if r := []rune(base); len(r) > 180 {
		base = strings.Trim(string(r[:180]), "-")
	}
	if base == "" {
		base = "forum"
	}
	slug := base
	for i := 2; ; i++ {
		var taken bool
		if err := s.store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM posts WHERE type = 'forum' AND slug = $1 AND id <> $2)`, slug, in.ID).Scan(&taken); err != nil {
			return 0, err
		}
		if !taken {
			break
		}
		slug = base + "-" + strconv.Itoa(i)
		if i > 1000 {
			return 0, errors.New("forum: no free slug")
		}
	}
	status := "open"
	if in.Closed {
		status = "closed"
	}
	base0 := map[string]any{forumMetaKind: kind, forumMetaStatus: status}
	if in.Design != nil {
		for k, v := range forumDesignMeta(forumSanitizeDesign(*in.Design)) {
			base0[k] = v
		}
	}
	meta, _ := json.Marshal(base0)
	desc := wp.KsesPost(strings.TrimSpace(in.Description))
	var id int64
	if in.ID > 0 {
		tag, err := s.store.Pool.Exec(ctx, `UPDATE posts SET parent_id = $2, slug = $3, title = $4, content = $5, menu_order = $6,
			meta = coalesce(meta,'{}') || $7::jsonb, modified_at = now(), updated_at = now() WHERE id = $1 AND type = 'forum'`,
			in.ID, forumNullID(in.ParentID), slug, title, desc, in.Order, meta)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, errForumNotFound
		}
		id = in.ID
	} else {
		base0[forumMetaTopicCount], base0[forumMetaReplyCount] = 0, 0
		full, _ := json.Marshal(base0)
		if err := s.store.Pool.QueryRow(ctx, `INSERT INTO posts (type, author_id, parent_id, slug, title, content, status, published_at,
			menu_order, comment_status, ping_status, meta) VALUES ('forum', $1, $2, $3, $4, $5, 'publish', now(), $6, 'closed', 'closed', $7) RETURNING id`,
			forumNullID(authorID), forumNullID(in.ParentID), slug, title, desc, in.Order, full).Scan(&id); err != nil {
			return 0, err
		}
	}
	s.forumPurge(ctx)
	return id, nil
}

// forumDeleteForum deletes a forum or category; it refuses while topics (any status) or forums are in it.
func (s *Server) forumDeleteForum(ctx context.Context, id int64) error {
	var n int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE parent_id = $1 AND type IN ('topic','forum')`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("forum: the forum is not empty")
	}
	tag, err := s.store.Pool.Exec(ctx, `DELETE FROM posts WHERE id = $1 AND type = 'forum'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errForumNotFound
	}
	s.forumPurge(ctx)
	return nil
}

// forumRecount recomputes a topic's counters (reply count, last reply, last activity, last author)
// and then its forum's (topic and reply counts of published, non-spam items, last topic). forumID 0
// with a topic recounts the topic's forum; topicID 0 recounts only the forum.
func (s *Server) forumRecount(ctx context.Context, topicID, forumID int64) error {
	// One recount per topic and per forum at a time, each statement after the lock: a recount whose
	// snapshot predates a reply committed meanwhile can never overwrite the newer counts.
	return pgx.BeginFunc(ctx, s.store.Pool, func(tx pgx.Tx) error {
		return s.forumRecountTx(ctx, tx, topicID, forumID)
	})
}

// The advisory lock namespaces (pg_advisory_xact_lock(ns, id)) of the forum.
const (
	forumLockTopic int32 = 0x50460001 // a topic's recount
	forumLockForum int32 = 0x50460002 // a forum's recount
	forumLockPost  int32 = 0x50460003 // a member's flood check and post
)

// forumXactLock takes the transaction-scoped advisory lock ns/id (ids wrap into int4; a collision
// only serializes two unrelated writers).
func forumXactLock(ctx context.Context, tx pgx.Tx, ns int32, id int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int, ($2::bigint % 2147483647)::int)`, ns, id)
	return err
}

func (s *Server) forumRecountTx(ctx context.Context, tx pgx.Tx, topicID, forumID int64) error {
	if topicID > 0 {
		if err := forumXactLock(ctx, tx, forumLockTopic, topicID); err != nil {
			return err
		}
		var parent int64
		err := tx.QueryRow(ctx, `WITH r AS (
				SELECT id, author_id, published_at FROM posts r WHERE r.type = 'reply' AND r.parent_id = $1 AND r.status = 'publish' AND `+forumNotSpam("r")+`
			), last AS (SELECT id, author_id, published_at FROM r ORDER BY published_at DESC NULLS LAST, id DESC LIMIT 1)
			UPDATE posts t SET meta = (coalesce(t.meta,'{}') - '`+forumMetaLastReply+`') || jsonb_build_object(
				'`+forumMetaReplyCount+`', (SELECT count(*) FROM r),
				'`+forumMetaLastActive+`', to_char(coalesce((SELECT published_at FROM last), t.published_at, t.created_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
				'`+forumMetaLastAuthor+`', coalesce((SELECT author_id FROM last), t.author_id, 0))
				|| coalesce((SELECT jsonb_build_object('`+forumMetaLastReply+`', id) FROM last), '{}'::jsonb)
			WHERE t.id = $1 AND t.type = 'topic' RETURNING coalesce(t.parent_id, 0)`, topicID).Scan(&parent)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if forumID == 0 {
			forumID = parent
		}
	}
	if forumID <= 0 {
		return nil
	}
	if err := forumXactLock(ctx, tx, forumLockForum, forumID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `WITH t AS (
			SELECT id, meta FROM posts t WHERE t.type = 'topic' AND t.parent_id = $1 AND t.status = 'publish' AND `+forumNotSpam("t")+`
		), last AS (SELECT id, meta FROM t ORDER BY meta->>'`+forumMetaLastActive+`' DESC NULLS LAST, id DESC LIMIT 1)
		UPDATE posts f SET meta = (coalesce(f.meta,'{}') - '`+forumMetaLastTopic+`' - '`+forumMetaLastReply+`' - '`+forumMetaLastActive+`' - '`+forumMetaLastAuthor+`')
			|| jsonb_build_object(
				'`+forumMetaTopicCount+`', (SELECT count(*) FROM t),
				'`+forumMetaReplyCount+`', (SELECT count(*) FROM posts r WHERE r.type = 'reply' AND r.parent_id IN (SELECT id FROM t)
					AND r.status = 'publish' AND `+forumNotSpam("r")+`))
			|| coalesce((SELECT jsonb_strip_nulls(jsonb_build_object('`+forumMetaLastTopic+`', id,
				'`+forumMetaLastActive+`', meta->'`+forumMetaLastActive+`', '`+forumMetaLastReply+`', meta->'`+forumMetaLastReply+`',
				'`+forumMetaLastAuthor+`', meta->'`+forumMetaLastAuthor+`')) FROM last), '{}'::jsonb)
		WHERE f.id = $1 AND f.type = 'forum'`, forumID)
	return err
}

// Members -----------------------------------------------------------------------------------------

// forumAuthors loads members by id: names, avatar (Media Library, no Gravatar; initials fallback),
// profile URL, moderator and blocked flags, statistics, level and badges. Batched: one users query
// per 100, one avatar query, one stats query, whatever the number of authors.
func (s *Server) forumAuthors(ctx context.Context, ids []int64) (map[int64]*forumAuthor, error) {
	out, _, err := s.forumLoadAuthors(ctx, ids)
	return out, err
}

// forumLoadAuthors is forumAuthors that also returns the loaded users (roles and email, for the
// admin member list).
func (s *Server) forumLoadAuthors(ctx context.Context, ids []int64) (map[int64]*forumAuthor, map[int64]*store.User, error) {
	out := map[int64]*forumAuthor{}
	users := map[int64]*store.User{}
	ids = forumIDs(ids...)
	if len(ids) == 0 {
		return out, users, nil
	}
	blocked := map[int64]bool{}
	rows, err := s.store.Pool.Query(ctx, `SELECT user_id FROM user_capabilities WHERE user_id = ANY($1) AND capability = $2 AND effect = 'deny'`, ids, capForumParticipate)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			blocked[id] = true
		}
	}
	rows.Close()
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		list, _, err := s.store.QueryUsers(ctx, store.UserQuery{IDs: ids[start:end], PerPage: end - start, OrderBy: "id"})
		if err != nil {
			return nil, nil, err
		}
		for _, u := range list {
			users[u.ID] = u
		}
	}
	var avatarIDs []int64
	for _, u := range users {
		if id := avatarIDOf(u); id > 0 {
			avatarIDs = append(avatarIDs, id)
		}
	}
	photos := s.forumImageURLs(ctx, avatarIDs, 96)
	all := make([]*forumAuthor, 0, len(users))
	for id, u := range users {
		url := defaultAvatar
		if p := photos[avatarIDOf(u)]; p != "" {
			url = p
		}
		out[id] = s.forumAuthorBase(ctx, u, blocked[id], url)
		all = append(all, out[id])
	}
	if err := s.forumGamifyFill(ctx, all...); err != nil {
		return nil, nil, err
	}
	return out, users, nil
}

// forumAuthorOf maps a loaded user (grants loaded) to a forumAuthor; Blocked is the user's own deny
// exception on forum_participate (a role without the capability is not "blocked"). Statistics,
// level and badges are not loaded (forumGamifyFill adds them).
func (s *Server) forumAuthorOf(ctx context.Context, u *store.User) *forumAuthor {
	if u == nil {
		return nil
	}
	var denied bool
	if !u.Can(capForumParticipate, nil) {
		_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_capabilities WHERE user_id = $1 AND capability = $2 AND effect = 'deny')`, u.ID, capForumParticipate).Scan(&denied)
	}
	return s.forumAuthorWith(ctx, u, denied)
}

func (s *Server) forumAuthorWith(ctx context.Context, u *store.User, blocked bool) *forumAuthor {
	return s.forumAuthorBase(ctx, u, blocked, s.avatarFor(ctx, u.Email, 96))
}

// forumAuthorBase maps a user with its avatar URL already resolved.
func (s *Server) forumAuthorBase(ctx context.Context, u *store.User, blocked bool, avatarURL string) *forumAuthor {
	name := strings.TrimSpace(u.DisplayName)
	if name == "" {
		name = u.Login
	}
	return &forumAuthor{ID: u.ID, Login: u.Login, Nicename: u.Nicename, DisplayName: name, AvatarURL: avatarURL,
		ProfileURL: s.forumProfileURL(ctx, u.Nicename), Moderator: u.Can(capForumModerate, nil), Admin: u.Can("manage_options", nil), Blocked: blocked,
		AvatarID: avatarIDOf(u), HasPhoto: avatarURL != "" && avatarURL != defaultAvatar, Initials: forumInitials(name, u.Login),
		Hue: forumHue(u.ID)}
}

// forumUserStats counts a member's published topics and replies.
func (s *Server) forumUserStats(ctx context.Context, userID int64) (forumProfileStats, error) {
	var st forumProfileStats
	err := s.store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM posts p WHERE p.author_id = u.id AND p.type = 'topic' AND p.status = 'publish' AND `+forumNotSpam("p")+`),
		(SELECT count(*) FROM posts p WHERE p.author_id = u.id AND p.type = 'reply' AND p.status = 'publish' AND `+forumNotSpam("p")+` AND `+forumSQLLiveParent+`),
		u.registered_at FROM users u WHERE u.id = $1`, userID).Scan(&st.Topics, &st.Replies, &st.Registered)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, errForumNotFound
	}
	return st, err
}

// forumLastPostAt is when the member last posted a topic or reply, any status (flood check); zero
// when never.
func (s *Server) forumLastPostAt(ctx context.Context, userID int64) (time.Time, error) {
	var t *time.Time
	if err := s.store.Pool.QueryRow(ctx, `SELECT max(created_at) FROM posts WHERE author_id = $1 AND type IN ('topic','reply')`, userID).Scan(&t); err != nil {
		return time.Time{}, err
	}
	if t == nil {
		return time.Time{}, nil
	}
	return *t, nil
}

// forumHasPublished reports whether the member has a published topic or reply (first-post moderation).
func (s *Server) forumHasPublished(ctx context.Context, userID int64) (bool, error) {
	var ok bool
	err := s.store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM posts p WHERE p.author_id = $1 AND p.type IN ('topic','reply')
		AND p.status = 'publish' AND `+forumNotSpam("p")+` AND `+forumSQLLiveParent+`)`, userID).Scan(&ok)
	return ok, err
}

// forumUserByGoogleSub is the account linked to a Google subject.
func (s *Server) forumUserByGoogleSub(ctx context.Context, sub string) (*store.User, error) {
	if strings.TrimSpace(sub) == "" {
		return nil, errForumNotFound
	}
	var id int64
	err := s.store.Pool.QueryRow(ctx, `SELECT id FROM users WHERE meta ? '`+forumUMetaGoogleSub+`' AND meta->>'`+forumUMetaGoogleSub+`' = $1`, sub).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errForumNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.store.UserByID(ctx, id)
}

// forumSetUserMeta merges keys into users.meta and removes the del keys.
func (s *Server) forumSetUserMeta(ctx context.Context, userID int64, set map[string]any, del ...string) error {
	if set == nil {
		set = map[string]any{}
	}
	b, err := json.Marshal(set)
	if err != nil {
		return err
	}
	if del == nil {
		del = []string{}
	}
	_, err = s.store.Pool.Exec(ctx, `UPDATE users SET meta = (coalesce(meta,'{}') - $3::text[]) || $2::jsonb WHERE id = $1`, userID, b, del)
	return err
}

// Cache and content ---------------------------------------------------------------------------------

// forumPurge drops every cached page that shows forum data (forum views, [pursae_forum] pages and
// theme widgets carry the "forum" tag).
func (s *Server) forumPurge(ctx context.Context) {
	_ = ctx
	s.purgeTags(forumCacheTag)
}

// forumFormat renders stored post content: sanitized again (defence in depth), rel="nofollow ugc"
// on links, paragraphs.
func forumFormat(content string) template.HTML {
	return template.HTML(wp.Autop(wp.RelUGC(wp.KsesPost(content))))
}

// forumClean sanitizes what a member typed: post HTML for users with unfiltered_html, the forum
// member tag set (forumMemberPolicy) for everyone else, with rel="nofollow ugc" on every link.
func forumClean(u *store.User, raw string) string {
	raw = strings.TrimSpace(raw)
	if u.Can("unfiltered_html", nil) {
		return strings.TrimSpace(wp.KsesPost(raw))
	}
	return strings.TrimSpace(wp.RelUGC(forumMemberPolicy().Sanitize(strings.ReplaceAll(raw, "\x00", ""))))
}

// forumCodeLangRe is the only class a member may set: a language-xxx hint on <code> (Devboard and
// highlighters read it).
var forumCodeLangRe = regexp.MustCompile(`^language-[a-z0-9][a-z0-9+#._-]{0,29}$`)

// forumMemberPolicy is what members without unfiltered_html may post: wp_kses_data()'s comment set
// plus what a forum needs for code and structure (paragraphs, line breaks, <pre>, <code
// class="language-xxx">, blockquote, lists, kbd, hr). No style, no other class or id, no images, no
// iframes, scripts, forms or event handlers; links keep the allowed protocols only.
var forumMemberPolicy = sync.OnceValue(func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "pre", "code", "kbd", "samp", "blockquote", "ul", "ol", "li", "hr",
		"strong", "b", "em", "i", "u", "s", "strike", "del", "ins", "q", "cite", "abbr", "acronym", "sub", "sup", "mark", "small", "a")
	p.AllowAttrs("href", "title").OnElements("a")
	p.AllowAttrs("title").OnElements("abbr", "acronym")
	p.AllowAttrs("cite").OnElements("blockquote", "q")
	p.AllowAttrs("datetime").OnElements("del", "ins")
	p.AllowAttrs("start").Matching(bluemonday.Integer).OnElements("ol")
	p.AllowAttrs("class").Matching(forumCodeLangRe).OnElements("code")
	p.AllowURLSchemes(wp.AllowedProtocols...)
	p.AllowRelativeURLs(true)
	p.RequireParseableURLs(true)
	return p
})
