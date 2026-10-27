package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Revision is one row of post_revisions: the title, excerpt and content a post had after a save
// (wp_save_post_revision() stores the new content, so the latest revision equals the post).
type Revision struct {
	ID         int64
	PostID     int64
	AuthorID   int64
	Title      string
	Excerpt    string
	Content    string
	IsAutosave bool
	CreatedAt  time.Time
}

const revisionCols = `id, post_id, coalesce(author_id,0), title, excerpt, content, is_autosave, created_at`

func scanRevision(row pgx.Row) (*Revision, error) {
	r := &Revision{}
	err := row.Scan(&r.ID, &r.PostID, &r.AuthorID, &r.Title, &r.Excerpt, &r.Content, &r.IsAutosave, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// GetRevision loads one revision.
func (s *Store) GetRevision(ctx context.Context, id int64) (*Revision, error) {
	return scanRevision(s.Pool.QueryRow(ctx, `SELECT `+revisionCols+` FROM post_revisions WHERE id = $1`, id))
}

// ListRevisions returns a post's revisions, newest first (wp_get_post_revisions()); autosaves are
// skipped unless asked for.
func (s *Store) ListRevisions(ctx context.Context, postID int64, autosaves bool) ([]*Revision, error) {
	q := `SELECT ` + revisionCols + ` FROM post_revisions WHERE post_id = $1`
	if !autosaves {
		q += ` AND NOT is_autosave`
	}
	rows, err := s.Pool.Query(ctx, q+` ORDER BY created_at DESC, id DESC`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RestoreRevision copies a revision's title, excerpt and content back into its post
// (wp_restore_post_revision()); UpdatePost records the restored content as a new revision.
func (s *Store) RestoreRevision(ctx context.Context, rev *Revision, editorID int64) (*Post, error) {
	p, err := s.GetPost(ctx, rev.PostID)
	if err != nil {
		return nil, err
	}
	p.Title, p.Excerpt, p.Content = rev.Title, rev.Excerpt, rev.Content
	if err := s.UpdatePost(ctx, p, editorID); err != nil {
		return nil, err
	}
	return p, nil
}

// UsedFormats lists the post formats in use other than "standard", like the post_format terms
// WordPress keeps for them (the formats dropdown on edit.php).
func (s *Store) UsedFormats(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT format FROM posts WHERE type = 'post' AND format <> 'standard' AND format <> ''
		AND status NOT IN ('trash','auto-draft') ORDER BY format`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// StickyCount counts the sticky posts in the given statuses (the "Sticky" view of edit.php).
func (s *Store) StickyCount(ctx context.Context, statuses []string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE type = 'post' AND sticky AND status = ANY($1)`, statuses).Scan(&n)
	return n, err
}
