package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"wpgo/internal/blocks"
	"wpgo/internal/config"
	"wpgo/internal/wp"
)

type Post struct {
	ID              int64
	Type            string
	AuthorID        int64
	ParentID        int64
	Slug            string
	Title           string
	Excerpt         string
	Content         string
	Status          string
	TrashedStatus   string
	Password        string
	PublishedAt     *time.Time
	ModifiedAt      time.Time
	CreatedAt       time.Time
	FeaturedMediaID int64
	MenuOrder       int
	CommentStatus   string
	PingStatus      string
	CommentCount    int
	Sticky          bool
	Format          string // a get_post_format_slugs() value; CanonicalPostFormat drops anything else
	Meta            map[string]any

	// PrevStatus is the status the post had before the last InsertPost / UpdatePost in this process
	// ("new" after an insert); it is not stored. The server reads it to fire transition_post_status.
	PrevStatus string `json:"-"`
}

// CanonicalPostFormat is get_post_format(): only the core post-format slugs are stored or printed.
// The bulk editor's "0", an empty value and any other string (including markup) are "standard".
func CanonicalPostFormat(format string) string {
	switch format {
	case "standard", "aside", "chat", "gallery", "link", "image", "quote", "status", "video", "audio":
		return format
	default:
		return "standard"
	}
}

func (p *Post) PostAuthorID() int64 { return p.AuthorID }
func (p *Post) PostStatus() string  { return p.Status }
func (p *Post) PostType() string    { return p.Type }

// Date is the post date for display: the publish date, or the creation date for floating drafts.
func (p *Post) Date() time.Time {
	if p.PublishedAt != nil {
		return *p.PublishedAt
	}
	return p.CreatedAt
}

const postCols = `id, type, coalesce(author_id,0), coalesce(parent_id,0), slug, title, excerpt, content, status,
	coalesce(trashed_status,''), password, published_at, modified_at, created_at, coalesce(featured_media_id,0),
	menu_order, comment_status, ping_status, comment_count, sticky, format, meta`

// postColsNoContent is postCols with empty excerpt and content (PostQuery.NoContent).
var postColsNoContent = strings.Replace(postCols, "excerpt, content,", "''::text, ''::text,", 1)

func scanPost(row pgx.Row) (*Post, error) {
	p := &Post{}
	var meta []byte
	err := row.Scan(&p.ID, &p.Type, &p.AuthorID, &p.ParentID, &p.Slug, &p.Title, &p.Excerpt, &p.Content, &p.Status,
		&p.TrashedStatus, &p.Password, &p.PublishedAt, &p.ModifiedAt, &p.CreatedAt, &p.FeaturedMediaID,
		&p.MenuOrder, &p.CommentStatus, &p.PingStatus, &p.CommentCount, &p.Sticky, &p.Format, &meta)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Meta = map[string]any{}
	_ = json.Unmarshal(meta, &p.Meta)
	return p, nil
}

func scanPosts(rows pgx.Rows) ([]*Post, error) {
	defer rows.Close()
	var out []*Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPost(ctx context.Context, id int64) (*Post, error) {
	return scanPost(s.Pool.QueryRow(ctx, `SELECT `+postCols+` FROM posts WHERE id = $1`, id))
}

// GetPostNoContent is GetPost with Excerpt and Content left empty, for callers that only need the
// post's slug, title, parent, meta or link.
func (s *Store) GetPostNoContent(ctx context.Context, id int64) (*Post, error) {
	return scanPost(s.Pool.QueryRow(ctx, `SELECT `+postColsNoContent+` FROM posts WHERE id = $1`, id))
}

// PostsByIDs loads several posts (any status) in one query; missing ids are absent from the map.
func (s *Store) PostsByIDs(ctx context.Context, ids []int64) (map[int64]*Post, error) {
	return s.postsByIDs(ctx, ids, postCols)
}

// PostsByIDsNoContent is PostsByIDs with Excerpt and Content left empty (menus, link lists).
func (s *Store) PostsByIDsNoContent(ctx context.Context, ids []int64) (map[int64]*Post, error) {
	return s.postsByIDs(ctx, ids, postColsNoContent)
}

func (s *Store) postsByIDs(ctx context.Context, ids []int64, cols string) (map[int64]*Post, error) {
	out := map[int64]*Post{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+cols+` FROM posts WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	posts, err := scanPosts(rows)
	for _, p := range posts {
		out[p.ID] = p
	}
	return out, err
}

// PostBySlug finds a post of the given type by slug. With parentPath empty the parent is ignored.
func (s *Store) PostBySlug(ctx context.Context, typ, slug string) (*Post, error) {
	args := []any{typ, slug}
	prefer := slugPrefer(ctx, "post", &args)
	return scanPost(s.Pool.QueryRow(ctx, `SELECT `+postCols+` FROM posts
		WHERE type = $1 AND slug = $2 AND slug <> '' AND status NOT IN ('auto-draft','trash')
		ORDER BY `+prefer+`(status = 'publish') DESC, id LIMIT 1`, args...))
}

// PageByPath resolves a hierarchical path such as "parent/child" (get_page_by_path()).
func (s *Store) PageByPath(ctx context.Context, path string) (*Post, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	var parent int64
	var p *Post
	for _, slug := range parts {
		var err error
		args := []any{slug, parent}
		prefer := slugPrefer(ctx, "post", &args)
		p, err = scanPost(s.Pool.QueryRow(ctx, `SELECT `+postCols+` FROM posts
			WHERE type = 'page' AND slug = $1 AND coalesce(parent_id,0) = $2 AND status NOT IN ('auto-draft','trash')
			ORDER BY `+prefer+`id LIMIT 1`, args...))
		if err != nil {
			return nil, err
		}
		parent = p.ID
	}
	if p == nil {
		return nil, ErrNotFound
	}
	return p, nil
}

// Ancestors returns the parent chain of a post, root first.
func (s *Store) Ancestors(ctx context.Context, p *Post) ([]*Post, error) {
	var chain []*Post
	cur := p
	for i := 0; cur.ParentID != 0 && i < 20; i++ {
		parent, err := s.GetPost(ctx, cur.ParentID)
		if err != nil {
			return nil, err
		}
		chain = append([]*Post{parent}, chain...)
		cur = parent
	}
	return chain, nil
}

// SlugScope, when set, narrows the slug uniqueness checks (UniqueSlug and the term slugs) to the
// objects a slug may not be shared with: kind is "post" or "term", id the object (0 for a new one),
// add binds an argument. Golylang's Share slugs: the object's own language.
var SlugScope func(ctx context.Context, kind string, id int64, add func(v any) string) string

// SlugPrefer, when set, is an ORDER BY expression (with its trailing direction) that picks among
// objects sharing a slug: Golylang prefers the request language.
var SlugPrefer func(ctx context.Context, kind string, add func(v any) string) string

func bindArg(args *[]any) func(v any) string {
	return func(v any) string {
		*args = append(*args, v)
		return "$" + strconv.Itoa(len(*args))
	}
}

// slugScope is SlugScope as an " AND …" clause ("" when unset).
func slugScope(ctx context.Context, kind string, id int64, args *[]any) string {
	if SlugScope != nil {
		if c := SlugScope(ctx, kind, id, bindArg(args)); c != "" {
			return " AND " + c
		}
	}
	return ""
}

// slugPrefer is SlugPrefer as a leading ORDER BY term ("" when unset).
func slugPrefer(ctx context.Context, kind string, args *[]any) string {
	if SlugPrefer != nil {
		if o := SlugPrefer(ctx, kind, bindArg(args)); o != "" {
			return o + ", "
		}
	}
	return ""
}

// QueryFilter, when set, narrows QueryPosts and QueryTerms by request context (Golylang's language
// filter). kind is "post" or "term"; add binds an argument and returns its placeholder.
var QueryFilter QueryFilterFunc

type QueryFilterFunc func(ctx context.Context, kind string, add func(v any) string) string

func (f QueryFilterFunc) cond(ctx context.Context, kind string, args *[]any) string {
	if f == nil {
		return ""
	}
	c := f(ctx, kind, func(v any) string {
		*args = append(*args, v)
		return "$" + strconv.Itoa(len(*args))
	})
	if c == "" {
		return ""
	}
	return " AND " + c
}

type PostQuery struct {
	Types                    []string
	Statuses                 []string
	AuthorID                 int64
	ParentID                 *int64
	Search                   string
	searchArg, searchLikeArg string  // placeholders where() bound for Search, reused by the relevance order
	searched                 bool    // where() saw a search (positive or exclusion terms)
	TermIDs                  []int64 // posts in any of these terms
	Year                     int
	Month                    int
	Day                      int
	IDs                      []int64
	ExcludeID                []int64
	OrderBy                  string // date, title, modified, menu_order, id, comment_count
	Order                    string // ASC or DESC
	Page                     int
	PerPage                  int // -1 for all
	StickyFirst              bool
	Loc                      *time.Location // site time zone for year/month/day filters
	Format                   string         // post format slug (edit.php?post_format=aside)
	Sticky                   *bool          // only sticky (true) or only non-sticky (false) posts (edit.php?show_sticky=1)
	// ReadableBy limits the result to what that user may read without a capability of their own: published
	// posts (and private ones when ReadablePrivate) of anybody, and their own posts of any status. This is
	// WordPress' perm=readable, used by the list screens for users who cannot edit others' posts.
	ReadableBy      int64
	ReadablePrivate bool
	// ExcludePassworded leaves out password-protected posts (WP_Query::parse_search() for logged-out
	// visitors: searching their hidden text would reveal it).
	ExcludePassworded bool
	// NoContent leaves Excerpt and Content empty, for lists that only print titles and links
	// (the Page List block) and would otherwise load every body.
	NoContent bool
	// NoTotal skips the count(*) query: the returned total is len(posts), not the number of matches.
	// For callers that discard the total. A query without a limit or offset (PerPage <= 0, no Offset)
	// skips the count on its own, its total being exactly len(posts).
	NoTotal bool
	// REST collection filters (WP_REST_Posts_Controller::get_items()).
	AuthorIDs        []int64 // author__in
	ExcludeAuthorIDs []int64 // author__not_in
	ParentIDs        []int64 // post_parent__in
	ExcludeParentIDs []int64 // post_parent__not_in
	Slugs            []string
	// TermClauses is a tax_query: each clause matches posts in any of its terms (or, with Exclude, in
	// none of them); the clauses are AND-joined, or OR-joined when TermRelationOR.
	TermClauses    []TermClause
	TermRelationOR bool
	// After/Before compare published_at, ModifiedAfter/ModifiedBefore modified_at (exclusive, as
	// WP_Date_Query's after/before without inclusive).
	After, Before, ModifiedAfter, ModifiedBefore *time.Time
	// Offset, when set, replaces the page offset (WP_Query's offset).
	Offset *int
}

// TermClause is one tax_query clause of PostQuery.TermClauses.
type TermClause struct {
	IDs     []int64
	Exclude bool
}

func (q *PostQuery) where(args *[]any) string {
	add := func(v any) string {
		*args = append(*args, v)
		return "$" + strconv.Itoa(len(*args))
	}
	var conds []string
	// A single type or status is compared with "=" so the planner can walk posts_list_date
	// (type, status, published_at DESC, id DESC) in order instead of sorting.
	if len(q.Types) == 1 {
		conds = append(conds, "type = "+add(q.Types[0]))
	} else if len(q.Types) > 0 {
		conds = append(conds, "type = ANY("+add(q.Types)+")")
	}
	if q.ReadableBy != 0 {
		open := "status = 'publish'"
		if q.ReadablePrivate {
			open = "status IN ('publish', 'private')"
		}
		conds = append(conds, "("+open+" OR author_id = "+add(q.ReadableBy)+")")
	}
	if len(q.Statuses) == 1 {
		conds = append(conds, "status = "+add(q.Statuses[0]))
	} else if len(q.Statuses) > 0 {
		conds = append(conds, "status = ANY("+add(q.Statuses)+")")
	} else {
		conds = append(conds, "status NOT IN ('auto-draft','trash')")
	}
	if q.AuthorID != 0 {
		conds = append(conds, "author_id = "+add(q.AuthorID))
	}
	if q.ParentID != nil {
		conds = append(conds, "coalesce(parent_id,0) = "+add(*q.ParentID))
	}
	if q.Search != "" {
		q.searched = true
		var exclude []string
		q.Search, exclude = normalizeSearch(ValidText(q.Search))
		// "-term" excludes posts containing term, as in WordPress (plainto_tsquery would just drop the minus).
		for _, ex := range exclude {
			ph := FoldedArgSQL(add(ex))
			conds = append(conds, "NOT (search @@ plainto_tsquery('simple', "+ph+") OR "+FoldedTitleSQL("title")+" LIKE "+FoldedArgSQL(add(likeContains(ex)))+")")
		}
	}
	if q.Search != "" {
		// The term and the searched text are folded alike by the database (lower(), then ı, İ → i; migration
		// 0105), so "IŞIK" finds "ışık" whatever the database collation lower-cases I to.
		q.searchArg, q.searchLikeArg = FoldedArgSQL(add(q.Search)), FoldedArgSQL(add(likeContains(q.Search)))
		// search_stem is the same text through the site language's stemmer (migration 0101, NULL when none).
		conds = append(conds, "(search @@ plainto_tsquery('simple', "+q.searchArg+") OR search_stem @@ plainto_tsquery(wpgo_search_stem_config(), "+q.searchArg+
			") OR "+FoldedTitleSQL("title")+" LIKE "+q.searchLikeArg+")")
	}
	if q.ExcludePassworded {
		conds = append(conds, "coalesce(password, '') = ''")
	}
	if len(q.TermIDs) > 0 {
		conds = append(conds, "id IN (SELECT post_id FROM post_terms WHERE term_id = ANY("+add(q.TermIDs)+"))")
	}
	if len(q.AuthorIDs) > 0 {
		conds = append(conds, "author_id = ANY("+add(q.AuthorIDs)+")")
	}
	if len(q.ExcludeAuthorIDs) > 0 {
		conds = append(conds, "NOT (coalesce(author_id,0) = ANY("+add(q.ExcludeAuthorIDs)+"))")
	}
	if len(q.ParentIDs) > 0 {
		conds = append(conds, "coalesce(parent_id,0) = ANY("+add(q.ParentIDs)+")")
	}
	if len(q.ExcludeParentIDs) > 0 {
		conds = append(conds, "NOT (coalesce(parent_id,0) = ANY("+add(q.ExcludeParentIDs)+"))")
	}
	if len(q.Slugs) > 0 {
		conds = append(conds, "slug = ANY("+add(q.Slugs)+")")
	}
	if len(q.TermClauses) > 0 {
		var tc []string
		for _, cl := range q.TermClauses {
			in := "id IN (SELECT post_id FROM post_terms WHERE term_id = ANY(" + add(cl.IDs) + "))"
			if cl.Exclude {
				in = "NOT " + in
			}
			tc = append(tc, in)
		}
		join := " AND "
		if q.TermRelationOR {
			join = " OR "
		}
		conds = append(conds, "("+strings.Join(tc, join)+")")
	}
	if q.After != nil {
		conds = append(conds, "published_at > "+add(*q.After))
	}
	if q.Before != nil {
		conds = append(conds, "published_at < "+add(*q.Before))
	}
	if q.ModifiedAfter != nil {
		conds = append(conds, "modified_at > "+add(*q.ModifiedAfter))
	}
	if q.ModifiedBefore != nil {
		conds = append(conds, "modified_at < "+add(*q.ModifiedBefore))
	}
	if len(q.IDs) > 0 {
		conds = append(conds, "id = ANY("+add(q.IDs)+")")
	}
	if len(q.ExcludeID) > 0 {
		conds = append(conds, "NOT (id = ANY("+add(q.ExcludeID)+"))")
	}
	if q.Format != "" {
		conds = append(conds, "format = "+add(q.Format))
	}
	if q.Sticky != nil {
		conds = append(conds, "sticky = "+add(*q.Sticky))
	}
	if q.Year != 0 {
		// Date archives are a published_at range computed in the site time zone, so the index applies.
		from, to, ok := DateArchiveRange(q.Year, q.Month, q.Day, q.Loc)
		if !ok {
			conds = append(conds, "false")
		} else {
			conds = append(conds, "published_at >= "+add(from)+" AND published_at < "+add(to))
		}
		if q.Month == 0 && q.Day != 0 { // ?year=…&day=… without a month: that day of every month
			conds = append(conds, "extract(day FROM published_at AT TIME ZONE "+add(PGZone(q.Loc))+") = "+add(q.Day))
		}
	}
	return strings.Join(conds, " AND ")
}

func locOrUTC(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// PGZone is loc as a PostgreSQL AT TIME ZONE argument. A named (IANA) zone passes through; a fixed
// offset such as wp.SiteLocation's "UTC+3:00" is rewritten in POSIX form, whose sign is inverted
// ("UTC-03:00" is three hours east), because PostgreSQL reads "UTC+3:00" as three hours west.
func PGZone(loc *time.Location) string {
	loc = locOrUTC(loc)
	name := loc.String()
	if name != "Local" {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	_, off := time.Now().In(loc).Zone()
	sign := "-"
	if off < 0 {
		sign, off = "+", -off
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, off/3600, off%3600/60)
}

// DateArchiveRange is the [from, to) instant range of a year, month or day archive in loc (the
// day is ignored without a month). ok is false for a month or day that does not exist.
func DateArchiveRange(year, month, day int, loc *time.Location) (from, to time.Time, ok bool) {
	loc = locOrUTC(loc)
	if year < 1 || year > 9999 || month < 0 || month > 12 || day < 0 || day > 31 {
		return from, to, false
	}
	switch {
	case month == 0:
		from, _ = localDayBoundary(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC), loc)
		to, _ = localDayBoundary(time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC), loc)
	case day == 0:
		from, _ = localDayBoundary(time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC), loc)
		to, _ = localDayBoundary(time.Date(year, time.Month(month)+1, 1, 0, 0, 0, 0, time.UTC), loc)
	default:
		calendarDay := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		if calendarDay.Year() != year || calendarDay.Month() != time.Month(month) || calendarDay.Day() != day {
			return from, to, false
		}
		var exists bool
		from, exists = localDayBoundary(calendarDay, loc)
		if !exists {
			return from, to, false
		}
		to, _ = localDayBoundary(calendarDay.AddDate(0, 0, 1), loc)
	}
	return from, to, true
}

// localDayBoundary finds the first instant on or after a civil date. Midnight
// can be skipped by a timezone transition, including a transition skipping an
// entire date; time.Date may normalize such a midnight into the previous day.
func localDayBoundary(day time.Time, loc *time.Location) (time.Time, bool) {
	y, m, d := day.Date()
	guess := time.Date(y, m, d, 0, 0, 0, 0, loc)
	if gy, gm, gd := guess.Date(); gy == y && gm == m && gd == d {
		return guess, true
	}
	target := y*10000 + int(m)*100 + d
	lo, hi := guess.Add(-48*time.Hour).Unix(), guess.Add(48*time.Hour).Unix()
	for lo < hi {
		mid := lo + (hi-lo)/2
		v := time.Unix(mid, 0).In(loc)
		vy, vm, vd := v.Date()
		if vy*10000+int(vm)*100+vd < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	v := time.Unix(lo, 0).In(loc)
	vy, vm, vd := v.Date()
	return v, vy == y && vm == m && vd == d
}

// FoldedTitleSQL is the text column col lower-cased by the database and then i-folded: ı and İ become
// i and a combining dot above (U+0307, left after the i of İ by some lower-casings) is dropped. The fold
// runs after lower() so it holds whatever the database collation lower-cases I to (ı under a Turkish
// one). It matches the expression of the posts_title_trgm index (migration 0105) when col is a posts
// title. Compare it with LIKE against FoldedArgSQL of the term: both sides are folded by the same
// expression, in the same collation.
func FoldedTitleSQL(col string) string { return "translate(lower(" + col + "), 'ıİ̇', 'ii')" }

// FoldedArgSQL is a text query parameter (a placeholder such as $2) folded the same way as
// FoldedTitleSQL, for search terms matched against folded text or the folded search vectors.
func FoldedArgSQL(ph string) string { return "translate(lower(" + ph + "::text), 'ıİ̇', 'ii')" }

func (q *PostQuery) orderBy() string {
	dir := "DESC"
	if strings.EqualFold(q.Order, "asc") {
		dir = "ASC"
	}
	col := "coalesce(published_at, created_at)"
	if q.datedOnly() {
		col = "published_at" // published, private and scheduled posts always have a date (migration 0032)
	}
	if q.Search != "" && q.OrderBy == "relevance" {
		// WordPress 6.x puts title matches first; ts_rank uses the A/B/C weights of the search vector.
		return "(" + FoldedTitleSQL("title") + " LIKE " + q.searchLikeArg + ") DESC, (ts_rank(search, plainto_tsquery('simple', " + q.searchArg + ")) + coalesce(ts_rank(search_stem, plainto_tsquery(wpgo_search_stem_config(), " + q.searchArg + ")), 0)) DESC, " + col + " " + dir + ", id " + dir
	}
	switch q.OrderBy {
	case "title":
		col = "lower(title)"
	case "modified":
		col = "modified_at"
	case "menu_order":
		col = "menu_order"
	case "id":
		col = "id"
	case "comment_count":
		col = "comment_count"
	case "author":
		col = "author_id"
	}
	order := col + " " + dir + ", id " + dir
	if q.StickyFirst {
		order = "sticky DESC, " + order
	}
	return order
}

// datedOnly reports whether every status queried always carries published_at.
func (q *PostQuery) datedOnly() bool {
	if len(q.Statuses) == 0 || q.ReadableBy != 0 {
		return false
	}
	for _, st := range q.Statuses {
		if st != "publish" && st != "private" && st != "future" {
			return false
		}
	}
	return true
}

// MaxPage caps page numbers so LIMIT/OFFSET stays in range however large a requested page is
// (a page past the end is simply empty).
const MaxPage = 1_000_000

// limitOffset is the LIMIT/OFFSET clause of a 1-based page.
func limitOffset(perPage, page int) string {
	if page < 1 {
		page = 1
	}
	if page > MaxPage {
		page = MaxPage
	}
	offset := int64(page - 1)
	if perPage > 0 && offset > math.MaxInt64/int64(perPage) {
		offset = math.MaxInt64
	} else {
		offset *= int64(perPage)
	}
	return fmt.Sprintf(" LIMIT %d OFFSET %d", perPage, offset)
}

// QueryPosts is WP_Query for the parts the admin and the theme need.
func (s *Store) QueryPosts(ctx context.Context, q PostQuery) ([]*Post, int, error) {
	var args []any
	where := q.where(&args) + QueryFilter.cond(ctx, "post", &args)
	if q.searched {
		// Full-text search is the one public query whose cost the visitor controls; bound it.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, SearchTimeout)
		defer cancel()
	}
	var total int
	countRows := !q.NoTotal && (q.PerPage > 0 || q.Offset != nil)
	if countRows {
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE `+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	cols := postCols
	if q.NoContent {
		cols = postColsNoContent
	}
	sql := `SELECT ` + cols + ` FROM posts WHERE ` + where + ` ORDER BY ` + q.orderBy()
	switch {
	case q.Offset != nil:
		if q.PerPage > 0 {
			sql += fmt.Sprintf(" LIMIT %d", q.PerPage)
		}
		sql += fmt.Sprintf(" OFFSET %d", max(*q.Offset, 0))
	case q.PerPage > 0:
		sql += limitOffset(q.PerPage, q.Page)
	}
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	posts, err := scanPosts(rows)
	if !countRows {
		total = len(posts)
	}
	return posts, total, err
}

// StatusCounts returns the number of posts per status for a type, skipping auto-drafts.
// StatusCountsReadable counts like StatusCounts, but only the posts viewer may read without a capability of their
// own: published (and private when readablePrivate) posts of anybody and the viewer's own posts of any status.
func (s *Store) StatusCountsReadable(ctx context.Context, typ string, viewerID int64, readablePrivate bool) (map[string]int, error) {
	open := "status = 'publish'"
	if readablePrivate {
		open = "status IN ('publish', 'private')"
	}
	rows, err := s.Pool.Query(ctx, `SELECT status, count(*) FROM posts WHERE type = $1 AND status <> 'auto-draft'
		AND (`+open+` OR author_id = $2) GROUP BY status`, typ, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

func (s *Store) StatusCounts(ctx context.Context, typ string, authorID int64) (map[string]int, error) {
	q := `SELECT status, count(*) FROM posts WHERE type = $1 AND status <> 'auto-draft'`
	args := []any{typ}
	if authorID != 0 {
		q += ` AND author_id = $2`
		args = append(args, authorID)
	}
	rows, err := s.Pool.Query(ctx, q+` GROUP BY status`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// UniqueSlug is wp_unique_post_slug(): appends -2, -3 … until the slug is free among siblings.
func (s *Store) UniqueSlug(ctx context.Context, typ string, parentID int64, slug string, excludeID int64) (string, error) {
	return uniqueSlug(ctx, s.Pool, typ, parentID, slug, excludeID)
}

func uniqueSlug(ctx context.Context, q DBTX, typ string, parentID int64, slug string, excludeID int64) (string, error) {
	if slug == "" {
		return "", nil
	}
	cand := slug
	for i := 2; ; i++ {
		// Lock every candidate, including the -2, -3, … suffixes. Locking only the requested slug
		// lets one transaction take "hello-2" directly while another, having found "hello" taken,
		// also chooses "hello-2".
		if err := lockSlug(ctx, q, "post", typ, cand); err != nil {
			return "", err
		}
		var taken bool
		args := []any{typ, parentID, cand, excludeID}
		scope := slugScope(ctx, "post", excludeID, &args)
		err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM posts WHERE type=$1 AND coalesce(parent_id,0)=$2
			AND slug=$3 AND id<>$4 AND status NOT IN ('auto-draft','trash')`+scope+`)`, args...).Scan(&taken)
		if err != nil {
			return "", err
		}
		if !taken {
			return cand, nil
		}
		cand = slug + "-" + strconv.Itoa(i)
	}
}

func nullable(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// lockSlug serializes the unique-slug check and the write of one slug: since migration 0020 no
// index keeps slugs unique (Multi Language's Share slugs lets translations share one), so two
// concurrent saves of "hello" would otherwise both find it free. The lock is transaction scoped:
// q must be the transaction that also writes the row (on a bare pool it is released at once).
func lockSlug(ctx context.Context, q DBTX, kind, scope, slug string) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, kind+":"+scope+":"+slug)
	return err
}

func prepareIn(ctx context.Context, q DBTX, p *Post) ([]byte, error) {
	// Sanitize first: a slug of only punctuation or emoji is empty, and wp_insert_post() then falls
	// back to the title and the id just as for a slug never given.
	p.Slug = wp.SanitizeTitle(p.Slug)
	if p.Status != "auto-draft" && p.Status != "draft" && p.Status != "pending" {
		if p.Slug == "" {
			p.Slug = wp.SanitizeTitle(p.Title)
			if p.Slug == "" && p.ID != 0 { // a new post gets its id as slug once inserted (InsertPost)
				p.Slug = strconv.FormatInt(p.ID, 10)
			}
		}
	}
	if p.Slug != "" {
		// uniqueSlug advisory-locks each candidate (the requested slug and every -N suffix) for the
		// rest of this transaction, which is what makes the check and the insert atomic.
		slug, err := uniqueSlug(ctx, q, p.Type, p.ParentID, p.Slug, p.ID)
		if err != nil {
			return nil, err
		}
		p.Slug = slug
	}
	if (p.Status == "publish" || p.Status == "future" || p.Status == "private") && p.PublishedAt == nil {
		now := time.Now()
		p.PublishedAt = &now
	}
	if p.Status == "publish" && p.PublishedAt.After(time.Now().Add(time.Minute)) {
		p.Status = "future"
	}
	if p.Status == "future" && !p.PublishedAt.After(time.Now()) {
		p.Status = "publish"
	}
	if p.CommentStatus == "" {
		p.CommentStatus = "open"
	}
	if p.PingStatus == "" {
		p.PingStatus = "open"
	}
	p.Format = CanonicalPostFormat(p.Format)
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	var tree []byte
	if blocks.HasBlocks(p.Content) {
		tree, _ = json.Marshal(blocks.Parse(p.Content))
	}
	return tree, nil
}

// InsertPost creates a post and returns its id.
func (s *Store) InsertPost(ctx context.Context, p *Post) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return insertPostTx(ctx, tx, p) })
}

func insertPostTx(ctx context.Context, tx pgx.Tx, p *Post) error {
	tree, err := prepareIn(ctx, tx, p)
	if err != nil {
		return err
	}
	meta, _ := json.Marshal(p.Meta)
	if err := tx.QueryRow(ctx, `INSERT INTO posts(type, author_id, parent_id, slug, title, excerpt, content, blocks,
		status, password, published_at, featured_media_id, menu_order, comment_status, ping_status, sticky, format, meta)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) RETURNING id, created_at, modified_at`,
		p.Type, nullable(p.AuthorID), nullable(p.ParentID), p.Slug, p.Title, p.Excerpt, p.Content, tree,
		p.Status, p.Password, p.PublishedAt, nullable(p.FeaturedMediaID), p.MenuOrder, p.CommentStatus, p.PingStatus,
		p.Sticky, p.Format, meta).Scan(&p.ID, &p.CreatedAt, &p.ModifiedAt); err != nil {
		return err
	}
	if p.Slug == "" && p.Status != "auto-draft" && p.Status != "draft" && p.Status != "pending" {
		// A title with nothing slug-worthy: wp_insert_post() falls back to the post id.
		slug, err := uniqueSlug(ctx, tx, p.Type, p.ParentID, strconv.FormatInt(p.ID, 10), p.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE posts SET slug = $2 WHERE id = $1`, p.ID, slug); err != nil {
			return err
		}
		p.Slug = slug
	}
	p.PrevStatus = "new"
	return nil
}

// UpdatePost writes every editable field and stores a revision of the previous content when it changed.
func (s *Store) UpdatePost(ctx context.Context, p *Post, editorID int64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return UpdatePostTx(ctx, tx, p, editorID) })
}

// GetPostTx reads a post inside tx and locks its row until tx ends.
func GetPostTx(ctx context.Context, tx pgx.Tx, id int64) (*Post, error) {
	return scanPost(tx.QueryRow(ctx, `SELECT `+postCols+` FROM posts WHERE id = $1 FOR UPDATE`, id))
}

// UpdatePostTx is UpdatePost inside the caller's transaction: every read and write runs on tx, so
// the post commits or rolls back with the caller's other writes and no second connection is needed.
func UpdatePostTx(ctx context.Context, tx pgx.Tx, p *Post, editorID int64) error {
	// Lock the row before the meta column is replaced. A concurrent update (another save, or a
	// meta || patch) must wait until this write commits, so the two cannot each read the old meta
	// and then overwrite the other's keys.
	old, err := GetPostTx(ctx, tx, p.ID)
	if err != nil {
		return err
	}
	p.ParentID = checkHierarchyLoop(ctx, tx, "posts", p.ID, p.ParentID) // wp_insert_post_parent
	tree, err := prepareIn(ctx, tx, p)
	if err != nil {
		return err
	}
	recordOldSlug(p, old)
	p.PrevStatus = old.Status
	meta, _ := json.Marshal(p.Meta)
	_, err = tx.Exec(ctx, `UPDATE posts SET author_id=$2, parent_id=$3, slug=$4, title=$5, excerpt=$6, content=$7,
		blocks=$8, status=$9, password=$10, published_at=$11, featured_media_id=$12, menu_order=$13,
		comment_status=$14, ping_status=$15, sticky=$16, format=$17, meta=$18, modified_at=now()
		WHERE id=$1`,
		p.ID, nullable(p.AuthorID), nullable(p.ParentID), p.Slug, p.Title, p.Excerpt, p.Content, tree,
		p.Status, p.Password, p.PublishedAt, nullable(p.FeaturedMediaID), p.MenuOrder, p.CommentStatus,
		p.PingStatus, p.Sticky, p.Format, meta)
	if err != nil {
		return err
	}
	// terms.post_count counts published posts only: moving in or out of "publish" changes the
	// count of every term the post has (wp_transition_post_status → _update_term_count_on_transition_post_status).
	if (old.Status == "publish") != (p.Status == "publish") {
		if _, err = tx.Exec(ctx, `UPDATE terms t SET post_count = (SELECT count(*) FROM post_terms pt
			JOIN posts p ON p.id = pt.post_id WHERE pt.term_id = t.id AND p.status = 'publish')
			WHERE t.id IN (SELECT term_id FROM post_terms WHERE post_id = $1)`, p.ID); err != nil {
			return err
		}
	}
	changed := old.Title != p.Title || old.Content != p.Content || old.Excerpt != p.Excerpt
	if changed && old.Status != "auto-draft" {
		_, err = tx.Exec(ctx, `INSERT INTO post_revisions(post_id, author_id, title, excerpt, content)
			VALUES ($1,$2,$3,$4,$5)`, p.ID, nullable(editorID), p.Title, p.Excerpt, p.Content)
		if err == nil {
			err = pruneRevisions(ctx, tx, p.ID, RevisionsLimit())
		}
	}
	return err
}

// RevisionsLimit is how many revisions a post keeps (WP_POST_REVISIONS): PURSAE_POST_REVISIONS, default 25;
// a negative value keeps them all.
func RevisionsLimit() int {
	if v := strings.TrimSpace(config.Env("POST_REVISIONS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 25
}

// pruneRevisions deletes a post's oldest revisions beyond keep (autosaves are not counted). The newest
// revision always stays so the history never loses the current content.
func pruneRevisions(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, postID int64, keep int) error {
	if keep < 0 {
		return nil
	}
	if keep < 1 {
		keep = 1
	}
	_, err := q.Exec(ctx, `DELETE FROM post_revisions WHERE post_id = $1 AND NOT is_autosave AND id NOT IN
		(SELECT id FROM post_revisions WHERE post_id = $1 AND NOT is_autosave ORDER BY id DESC LIMIT $2)`, postID, keep)
	return err
}

// PruneRevisions trims every post to keep revisions (the "clean up old revisions" maintenance run) and
// returns how many rows it deleted.
func (s *Store) PruneRevisions(ctx context.Context, keep int) (int64, error) {
	if keep < 0 {
		return 0, nil
	}
	if keep < 1 {
		keep = 1
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM post_revisions r WHERE NOT r.is_autosave AND r.id IN
		(SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY post_id ORDER BY id DESC) AS n
			FROM post_revisions WHERE NOT is_autosave) x WHERE x.n > $1)`, keep)
	return tag.RowsAffected(), err
}

// PruneRevisionsTx trims one post to RevisionsLimit() inside the caller's transaction (a writer that
// inserts revisions itself, such as the Web Builder publish).
func PruneRevisionsTx(ctx context.Context, tx pgx.Tx, postID int64) error {
	return pruneRevisions(ctx, tx, postID, RevisionsLimit())
}

// RevisionStats counts the stored revisions and how many of them PruneRevisions(keep) would delete.
func (s *Store) RevisionStats(ctx context.Context, keep int) (total, excess int, err error) {
	if keep < 1 {
		keep = 1
	}
	err = s.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE n > $1) FROM
		(SELECT row_number() OVER (PARTITION BY post_id ORDER BY id DESC) AS n FROM post_revisions WHERE NOT is_autosave) x`, keep).Scan(&total, &excess)
	return
}

// OldSlugMeta is the post meta key WordPress uses for previous slugs (_wp_old_slug).
const OldSlugMeta = "_wp_old_slug"

// OldSlugs returns the previous slugs recorded in a post's meta.
func (p *Post) OldSlugs() []string {
	var out []string
	switch v := p.Meta[OldSlugMeta].(type) {
	case string:
		if v != "" {
			out = append(out, v)
		}
	case []any:
		for _, s := range v {
			if str, ok := s.(string); ok && str != "" {
				out = append(out, str)
			}
		}
	case []string:
		out = append(out, v...)
	}
	return out
}

// recordOldSlug is wp_check_for_changed_slugs(): when a published post's slug changes the previous
// one is kept in _wp_old_slug so wp_old_slug_redirect() can send old links to the new permalink.
func recordOldSlug(p, old *Post) {
	if old == nil || old.Slug == "" || old.Slug == p.Slug {
		return
	}
	if p.Status != "publish" && p.Status != "private" && p.Status != "future" {
		return
	}
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	slugs := p.OldSlugs()
	keep := slugs[:0]
	for _, s := range slugs {
		if s != p.Slug {
			keep = append(keep, s)
		}
	}
	found := false
	for _, s := range keep {
		if s == old.Slug {
			found = true
		}
	}
	if !found {
		keep = append(keep, old.Slug)
	}
	p.Meta[OldSlugMeta] = keep
}

// PostByOldSlug finds the published post or page that once used slug (wp_old_slug_redirect()). When a
// year, month or day is given the post's date has to match too, like the query vars do in WordPress.
func (s *Store) PostByOldSlug(ctx context.Context, slug string, year, month, day int, loc *time.Location) (*Post, error) {
	if slug == "" {
		return nil, ErrNotFound
	}
	// A containment test the posts_meta GIN index serves: every 404 runs this lookup, so it must not
	// scan the table. recordOldSlug always writes an array, and {"k":["x"]} only matches an array holding "x".
	probe, _ := json.Marshal(map[string]any{OldSlugMeta: []string{slug}})
	args := []any{string(probe)}
	sql := `SELECT ` + postCols + ` FROM posts WHERE type IN ('post','page') AND status = 'publish'
		AND meta @> $1::jsonb`
	if year != 0 {
		args = append(args, PGZone(loc), year)
		local := "(published_at AT TIME ZONE $2)"
		sql += " AND extract(year FROM " + local + ") = $3"
		if month != 0 {
			args = append(args, month)
			sql += " AND extract(month FROM " + local + ") = $4"
			if day != 0 {
				args = append(args, day)
				sql += " AND extract(day FROM " + local + ") = $5"
			}
		}
	}
	return scanPost(s.Pool.QueryRow(ctx, sql+` ORDER BY id LIMIT 1`, args...))
}

// HasPublishedPosts reports whether any post of the type is published (block_core_calendar_has_published_posts()).
func (s *Store) HasPublishedPosts(ctx context.Context, typ string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM posts WHERE type = $1 AND status = 'publish')`, typ).Scan(&ok)
	return ok, err
}

// PostDays lists the days of a month that have published posts, in the site time zone (get_calendar()).
func (s *Store) PostDays(ctx context.Context, year, month int, loc *time.Location) ([]int, error) {
	from, to, ok := DateArchiveRange(year, month, 0, loc)
	if !ok || month == 0 {
		return nil, nil
	}
	loc = locOrUTC(loc)
	rows, err := s.Pool.Query(ctx, `SELECT published_at FROM posts WHERE type = 'post' AND status = 'publish'
		AND published_at >= $1 AND published_at < $2 ORDER BY published_at`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []int
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		if d := t.In(loc).Day(); len(days) == 0 || days[len(days)-1] != d {
			days = append(days, d)
		}
	}
	return days, rows.Err()
}

// AdjacentMonth finds the nearest month before (previous) or after the given one that has published
// posts, as get_calendar() does for its previous/next links. ok is false when there is none.
func (s *Store) AdjacentMonth(ctx context.Context, year, month int, loc *time.Location, previous bool) (y, m int, ok bool, err error) {
	if loc == nil {
		loc = time.UTC
	}
	first := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc)
	var t *time.Time
	if previous {
		err = s.Pool.QueryRow(ctx, `SELECT published_at FROM posts WHERE type = 'post' AND status = 'publish'
			AND published_at < $1 ORDER BY published_at DESC LIMIT 1`, first).Scan(&t)
	} else {
		next := first.AddDate(0, 1, 0)
		err = s.Pool.QueryRow(ctx, `SELECT published_at FROM posts WHERE type = 'post' AND status = 'publish'
			AND published_at >= $1 ORDER BY published_at ASC LIMIT 1`, next).Scan(&t)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil || t == nil {
		return 0, 0, false, err
	}
	lt := t.In(loc)
	return lt.Year(), int(lt.Month()), true, nil
}

// TrashPost moves a post to the trash, remembering its status (wp_trash_post()).
func (s *Store) TrashPost(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE posts SET trashed_status = status, status = 'trash', deleted_at = now()
		WHERE id = $1 AND status <> 'trash'`, id)
	if err == nil {
		err = s.recountPostTerms(ctx, id)
	}
	return err
}

// UntrashPost restores a trashed post; like WordPress 5.6+ it comes back as a draft.
func (s *Store) UntrashPost(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE posts SET status = 'draft',
		trashed_status = NULL, deleted_at = NULL WHERE id = $1 AND status = 'trash'`, id)
	return err
}

// DeletePost deletes a post permanently. Like wp_delete_post(), its children of the same type and
// its attachments move up to its parent (the foreign key alone would make them top level).
func (s *Store) DeletePost(ctx context.Context, id int64) error {
	termIDs, err := s.postTermIDs(ctx, id)
	if err != nil {
		return err
	}
	if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE posts c SET parent_id = d.parent_id FROM posts d
			WHERE d.id = $1 AND c.parent_id = $1 AND (c.type = d.type OR c.type = 'attachment')`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM posts WHERE id = $1`, id)
		return err
	}); err != nil {
		return err
	}
	return s.RecountTerms(ctx, termIDs)
}

// CreateAutoDraft is what post-new.php does first, so the editor always has a post id.
func (s *Store) CreateAutoDraft(ctx context.Context, typ string, authorID int64) (*Post, error) {
	p := &Post{Type: typ, AuthorID: authorID, Status: "auto-draft", Title: "Auto Draft"}
	if err := s.InsertPost(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) Revisions(ctx context.Context, postID int64) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM post_revisions WHERE post_id=$1 AND NOT is_autosave`, postID).Scan(&n)
	return n, err
}

// RevisionCounts is Revisions for many posts in one query; posts without revisions are absent.
func (s *Store) RevisionCounts(ctx context.Context, postIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(postIDs) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT post_id, count(*) FROM post_revisions WHERE post_id = ANY($1) AND NOT is_autosave GROUP BY post_id`, postIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// PublishDuePosts flips scheduled posts whose date has passed (the publish_future_post cron hook).
// It runs every scheduler tick: the posts_future index keeps it off the posts table, and only the terms
// of the posts it published are recounted.
func (s *Store) PublishDuePosts(ctx context.Context) (int64, error) {
	ids, err := s.PublishDuePostIDs(ctx)
	return int64(len(ids)), err
}

// PublishDuePostIDs is PublishDuePosts returning the ids of the posts it published, so the caller can
// run the save/transition side effects (webhooks, plugins, SEO) for each of them.
func (s *Store) PublishDuePostIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.Pool.Query(ctx, `UPDATE posts SET status = 'publish' WHERE status = 'future' AND published_at <= now() RETURNING id`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var termIDs []int64
	trows, err := s.Pool.Query(ctx, `SELECT DISTINCT term_id FROM post_terms WHERE post_id = ANY($1)`, ids)
	if err != nil {
		return ids, err
	}
	termIDs, err = pgx.CollectRows(trows, pgx.RowTo[int64])
	if err != nil {
		return ids, err
	}
	return ids, s.RecountTerms(ctx, termIDs)
}

type ArchiveMonth struct {
	Year, Month, Count int
}

// MonthlyArchives lists months with published posts, newest first (wp_get_archives()).
func (s *Store) MonthlyArchives(ctx context.Context, loc *time.Location) ([]ArchiveMonth, error) {
	rows, err := s.Pool.Query(ctx, `SELECT extract(year FROM published_at AT TIME ZONE $1)::int y,
		extract(month FROM published_at AT TIME ZONE $1)::int m, count(*) FROM posts
		WHERE type = 'post' AND status = 'publish' GROUP BY 1, 2 ORDER BY 1 DESC, 2 DESC`, PGZone(loc))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArchiveMonth
	for rows.Next() {
		var a ArchiveMonth
		if err := rows.Scan(&a.Year, &a.Month, &a.Count); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Adjacent returns the previous or next published post by date (get_adjacent_post()).
func (s *Store) Adjacent(ctx context.Context, p *Post, previous bool) (*Post, error) {
	return s.AdjacentExcept(ctx, p, previous, nil)
}

// AdjacentExcept is Adjacent skipping the given ids (posts hidden from the visitor), narrowed by
// QueryFilter (the request language).
func (s *Store) AdjacentExcept(ctx context.Context, p *Post, previous bool, exclude []int64) (*Post, error) {
	if p.PublishedAt == nil {
		return nil, ErrNotFound
	}
	cmp, bound, dir := ">", ">=", "ASC"
	if previous {
		cmp, bound, dir = "<", "<=", "DESC"
	}
	if exclude == nil {
		exclude = []int64{}
	}
	// QueryFilter keeps to the request language (Polylang's pll_post_langs join on get_adjacent_post()).
	args := []any{p.Type, *p.PublishedAt, p.ID, exclude}
	filter := QueryFilter.cond(ctx, "post", &args)
	// The navigation prints only titles and links: no excerpt or content. "published_at <= $2" (">=" for
	// next) is implied by the OR below; spelled out, it gives the planner a range on posts_list_date.
	return scanPost(s.Pool.QueryRow(ctx, `SELECT `+postColsNoContent+` FROM posts WHERE type = $1 AND status = 'publish'
		AND published_at `+bound+` $2
		AND (published_at `+cmp+` $2 OR (published_at = $2 AND id `+cmp+` $3)) AND NOT (id = ANY($4))`+filter+`
		ORDER BY published_at `+dir+`, id `+dir+` LIMIT 1`, args...))
}

// duplicateSkipMeta are the meta keys a copy does not take: edit locks, old slugs and trash state
// belong to the original.
var duplicateSkipMeta = map[string]bool{"_edit_lock": true, "_edit_last": true, "_wp_old_slug": true, "_wp_old_date": true,
	"_wp_desired_post_slug": true, "_encloseme": true, "_pingme": true}

// DuplicatePost copies a post into a new draft by authorID titled title: content, excerpt, parent,
// order, format, discussion settings, featured image, meta, terms, the Pursae Web Builder doc and
// (Pursae Multi Language) the language, in a translation group of its own. Comments, revisions,
// the password, stickiness and the publish date are not copied. One transaction.
func (s *Store) DuplicatePost(ctx context.Context, src *Post, authorID int64, title string) (*Post, error) {
	meta := map[string]any{}
	for k, v := range src.Meta {
		if duplicateSkipMeta[k] || strings.HasPrefix(k, "_wp_trash_meta") {
			continue
		}
		meta[k] = v
	}
	p := &Post{Type: src.Type, AuthorID: authorID, ParentID: src.ParentID, Title: title, Excerpt: src.Excerpt,
		Content: src.Content, Status: "draft", FeaturedMediaID: src.FeaturedMediaID, MenuOrder: src.MenuOrder,
		CommentStatus: src.CommentStatus, PingStatus: src.PingStatus, Format: src.Format, Meta: meta}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := insertPostTx(ctx, tx, p); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO post_terms (post_id, term_id, sort_order)
			SELECT $2, term_id, sort_order FROM post_terms WHERE post_id = $1`, src.ID, p.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO web_builder_docs (post_id, doc, page, html, css, fonts, features, renderer, rev,
				content_hash, detached, snapshot_user, snapshot_unfiltered)
			SELECT $2, doc, page, html, css, fonts, features, renderer, 0, content_hash, detached, snapshot_user, snapshot_unfiltered
			FROM web_builder_docs WHERE post_id = $1`, src.ID, p.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO pll_post_langs (post_id, lang, grp)
			SELECT $2, lang, nextval('pll_translation_groups') FROM pll_post_langs WHERE post_id = $1`, src.ID, p.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Search limits: a visitor-controlled search is bounded in length, in the number of terms and in time, so one
// request cannot build a huge tsquery/LIKE scan (anonymous search DoS). Excess input is dropped, not rejected.
const (
	searchMaxRunes = 200
	searchMaxTerms = 32
)

// SearchTimeout bounds a QueryPosts call that searches.
var SearchTimeout = 5 * time.Second

// normalizeSearch caps the search string and splits off WordPress-style "-term" exclusions. The returned search
// keeps the remaining terms in order, deduplicated; a quoted phrase is kept as typed.
func normalizeSearch(s string) (string, []string) {
	if r := []rune(s); len(r) > searchMaxRunes {
		s = string(r[:searchMaxRunes])
	}
	seen := map[string]bool{}
	var keep, exclude []string
	for _, w := range strings.Fields(s) {
		lw := strings.ToLower(w)
		if seen[lw] {
			continue
		}
		if len(seen) >= searchMaxTerms {
			break
		}
		seen[lw] = true
		if len(w) > 1 && w[0] == '-' {
			exclude = append(exclude, w[1:])
			continue
		}
		keep = append(keep, w)
	}
	return strings.Join(keep, " "), exclude
}
