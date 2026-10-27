package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wpgo/internal/blocks"
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
	Format          string
	Meta            map[string]any
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

// PostBySlug finds a post of the given type by slug. With parentPath empty the parent is ignored.
func (s *Store) PostBySlug(ctx context.Context, typ, slug string) (*Post, error) {
	return scanPost(s.Pool.QueryRow(ctx, `SELECT `+postCols+` FROM posts
		WHERE type = $1 AND slug = $2 AND status NOT IN ('auto-draft','trash')
		ORDER BY (status = 'publish') DESC, id LIMIT 1`, typ, slug))
}

// PageByPath resolves a hierarchical path such as "parent/child" (get_page_by_path()).
func (s *Store) PageByPath(ctx context.Context, path string) (*Post, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	var parent int64
	var p *Post
	for _, slug := range parts {
		var err error
		p, err = scanPost(s.Pool.QueryRow(ctx, `SELECT `+postCols+` FROM posts
			WHERE type = 'page' AND slug = $1 AND coalesce(parent_id,0) = $2 AND status NOT IN ('auto-draft','trash')
			LIMIT 1`, slug, parent))
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

type PostQuery struct {
	Types       []string
	Statuses    []string
	AuthorID    int64
	ParentID    *int64
	Search      string
	TermIDs     []int64 // posts in any of these terms
	Year        int
	Month       int
	Day         int
	IDs         []int64
	ExcludeID   []int64
	OrderBy     string // date, title, modified, menu_order, id, comment_count
	Order       string // ASC or DESC
	Page        int
	PerPage     int // -1 for all
	StickyFirst bool
	Loc         *time.Location // site time zone for year/month/day filters
}

func (q *PostQuery) where(args *[]any) string {
	add := func(v any) string {
		*args = append(*args, v)
		return "$" + strconv.Itoa(len(*args))
	}
	var conds []string
	if len(q.Types) > 0 {
		conds = append(conds, "type = ANY("+add(q.Types)+")")
	}
	if len(q.Statuses) > 0 {
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
		conds = append(conds, "(search @@ plainto_tsquery('simple', "+add(q.Search)+") OR title ILIKE "+add("%"+q.Search+"%")+")")
	}
	if len(q.TermIDs) > 0 {
		conds = append(conds, "id IN (SELECT post_id FROM post_terms WHERE term_id = ANY("+add(q.TermIDs)+"))")
	}
	if len(q.IDs) > 0 {
		conds = append(conds, "id = ANY("+add(q.IDs)+")")
	}
	if len(q.ExcludeID) > 0 {
		conds = append(conds, "NOT (id = ANY("+add(q.ExcludeID)+"))")
	}
	if q.Year != 0 {
		tz := "UTC"
		if q.Loc != nil {
			tz = q.Loc.String()
		}
		local := "(published_at AT TIME ZONE " + add(tz) + ")"
		conds = append(conds, "extract(year FROM "+local+") = "+add(q.Year))
		if q.Month != 0 {
			conds = append(conds, "extract(month FROM "+local+") = "+add(q.Month))
		}
		if q.Day != 0 {
			conds = append(conds, "extract(day FROM "+local+") = "+add(q.Day))
		}
	}
	return strings.Join(conds, " AND ")
}

func (q *PostQuery) orderBy() string {
	dir := "DESC"
	if strings.EqualFold(q.Order, "asc") {
		dir = "ASC"
	}
	col := "coalesce(published_at, created_at)"
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

// QueryPosts is WP_Query for the parts the admin and the theme need.
func (s *Store) QueryPosts(ctx context.Context, q PostQuery) ([]*Post, int, error) {
	var args []any
	where := q.where(&args)
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	sql := `SELECT ` + postCols + ` FROM posts WHERE ` + where + ` ORDER BY ` + q.orderBy()
	if q.PerPage > 0 {
		page := q.Page
		if page < 1 {
			page = 1
		}
		sql += fmt.Sprintf(" LIMIT %d OFFSET %d", q.PerPage, (page-1)*q.PerPage)
	}
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	posts, err := scanPosts(rows)
	return posts, total, err
}

// StatusCounts returns the number of posts per status for a type, skipping auto-drafts.
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
	if slug == "" {
		return "", nil
	}
	cand := slug
	for i := 2; ; i++ {
		var taken bool
		err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM posts WHERE type=$1 AND coalesce(parent_id,0)=$2
			AND slug=$3 AND id<>$4 AND status NOT IN ('auto-draft','trash'))`, typ, parentID, cand, excludeID).Scan(&taken)
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

// prepare normalizes a post before writing: slug, block tree and publish date.
func (s *Store) prepare(ctx context.Context, p *Post) ([]byte, error) {
	if p.Status != "auto-draft" && p.Status != "draft" && p.Status != "pending" {
		if p.Slug == "" {
			p.Slug = wp.SanitizeTitle(p.Title)
			if p.Slug == "" {
				p.Slug = strconv.FormatInt(p.ID, 10)
			}
		}
	}
	if p.Slug != "" {
		slug, err := s.UniqueSlug(ctx, p.Type, p.ParentID, wp.SanitizeTitle(p.Slug), p.ID)
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
	if p.Format == "" {
		p.Format = "standard"
	}
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
	tree, err := s.prepare(ctx, p)
	if err != nil {
		return err
	}
	meta, _ := json.Marshal(p.Meta)
	return s.Pool.QueryRow(ctx, `INSERT INTO posts(type, author_id, parent_id, slug, title, excerpt, content, blocks,
		status, password, published_at, featured_media_id, menu_order, comment_status, ping_status, sticky, format, meta)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) RETURNING id, created_at, modified_at`,
		p.Type, nullable(p.AuthorID), nullable(p.ParentID), p.Slug, p.Title, p.Excerpt, p.Content, tree,
		p.Status, p.Password, p.PublishedAt, nullable(p.FeaturedMediaID), p.MenuOrder, p.CommentStatus, p.PingStatus,
		p.Sticky, p.Format, meta).Scan(&p.ID, &p.CreatedAt, &p.ModifiedAt)
}

// UpdatePost writes every editable field and stores a revision of the previous content when it changed.
func (s *Store) UpdatePost(ctx context.Context, p *Post, editorID int64) error {
	old, err := s.GetPost(ctx, p.ID)
	if err != nil {
		return err
	}
	tree, err := s.prepare(ctx, p)
	if err != nil {
		return err
	}
	recordOldSlug(p, old)
	meta, _ := json.Marshal(p.Meta)
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE posts SET author_id=$2, parent_id=$3, slug=$4, title=$5, excerpt=$6, content=$7,
			blocks=$8, status=$9, password=$10, published_at=$11, featured_media_id=$12, menu_order=$13,
			comment_status=$14, ping_status=$15, sticky=$16, format=$17, meta=$18, modified_at=now()
			WHERE id=$1`,
			p.ID, nullable(p.AuthorID), nullable(p.ParentID), p.Slug, p.Title, p.Excerpt, p.Content, tree,
			p.Status, p.Password, p.PublishedAt, nullable(p.FeaturedMediaID), p.MenuOrder, p.CommentStatus,
			p.PingStatus, p.Sticky, p.Format, meta)
		if err != nil {
			return err
		}
		changed := old.Title != p.Title || old.Content != p.Content || old.Excerpt != p.Excerpt
		if changed && old.Status != "auto-draft" {
			_, err = tx.Exec(ctx, `INSERT INTO post_revisions(post_id, author_id, title, excerpt, content)
				VALUES ($1,$2,$3,$4,$5)`, p.ID, nullable(editorID), p.Title, p.Excerpt, p.Content)
		}
		return err
	})
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
	args := []any{slug}
	sql := `SELECT ` + postCols + ` FROM posts WHERE type IN ('post','page') AND status = 'publish'
		AND jsonb_typeof(meta->'_wp_old_slug') = 'array' AND jsonb_exists(meta->'_wp_old_slug', $1)`
	if year != 0 {
		tz := "UTC"
		if loc != nil {
			tz = loc.String()
		}
		args = append(args, tz, year)
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
	tz := "UTC"
	if loc != nil {
		tz = loc.String()
	}
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT extract(day FROM published_at AT TIME ZONE $1)::int FROM posts
		WHERE type = 'post' AND status = 'publish' AND extract(year FROM published_at AT TIME ZONE $1) = $2
		AND extract(month FROM published_at AT TIME ZONE $1) = $3 ORDER BY 1`, tz, year, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []int
	for rows.Next() {
		var d int
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		days = append(days, d)
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

func (s *Store) DeletePost(ctx context.Context, id int64) error {
	termIDs, err := s.postTermIDs(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM posts WHERE id = $1`, id); err != nil {
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

// PublishDuePosts flips scheduled posts whose date has passed (the publish_future_post cron hook).
func (s *Store) PublishDuePosts(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE posts SET status = 'publish' WHERE status = 'future' AND published_at <= now()`)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() > 0 {
		_, err = s.Pool.Exec(ctx, `UPDATE terms t SET post_count = (SELECT count(*) FROM post_terms pt
			JOIN posts p ON p.id = pt.post_id WHERE pt.term_id = t.id AND p.status = 'publish')`)
	}
	return tag.RowsAffected(), err
}

type ArchiveMonth struct {
	Year, Month, Count int
}

// MonthlyArchives lists months with published posts, newest first (wp_get_archives()).
func (s *Store) MonthlyArchives(ctx context.Context, loc *time.Location) ([]ArchiveMonth, error) {
	rows, err := s.Pool.Query(ctx, `SELECT extract(year FROM published_at AT TIME ZONE $1)::int y,
		extract(month FROM published_at AT TIME ZONE $1)::int m, count(*) FROM posts
		WHERE type = 'post' AND status = 'publish' GROUP BY 1, 2 ORDER BY 1 DESC, 2 DESC`, loc.String())
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
	if p.PublishedAt == nil {
		return nil, ErrNotFound
	}
	cmp, dir := ">", "ASC"
	if previous {
		cmp, dir = "<", "DESC"
	}
	return scanPost(s.Pool.QueryRow(ctx, `SELECT `+postCols+` FROM posts WHERE type = $1 AND status = 'publish'
		AND (published_at `+cmp+` $2 OR (published_at = $2 AND id `+cmp+` $3)) ORDER BY published_at `+dir+`, id `+dir+` LIMIT 1`,
		p.Type, *p.PublishedAt, p.ID))
}
