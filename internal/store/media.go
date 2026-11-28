package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Attachment is a media library item (an "attachment" post in WordPress terms).
type Attachment struct {
	ID          int64
	UploaderID  int64
	PostID      int64 // uploaded to / attached to
	Slug        string
	FilePath    string // relative to the uploads dir
	Mime        string
	FileSize    int64
	Width       int
	Height      int
	Alt         string
	Title       string
	Caption     string
	Description string
	MenuOrder   int
	Meta        map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Variants    []AttachmentVariant

	// Base holds the media row's own texts when AttachmentFilter replaced them with a
	// translation for the request (nil when the texts are the stored ones). UpdateAttachment
	// writes Base, never the overlaid texts.
	Base *AttachmentText
}

// AttachmentText is the translatable text of an attachment.
type AttachmentText struct {
	Alt, Title, Caption, Description string
}

// AttachmentFilter, when set, may overlay per-request texts (Pursae Multi Language: the
// visitor's language) on attachments just read by GetAttachment, GetAttachments and
// QueryAttachments. It must set Base before it changes a text and must not run for writes.
var AttachmentFilter func(ctx context.Context, list []*Attachment)

func filterAttachments(ctx context.Context, list []*Attachment) {
	if AttachmentFilter != nil && len(list) > 0 {
		AttachmentFilter(ctx, list)
	}
}

type AttachmentVariant struct {
	Name     string
	FilePath string
	Width    int
	Height   int
	Mime     string
	FileSize int64
}

func (a *Attachment) PostAuthorID() int64 { return a.UploaderID }
func (a *Attachment) PostStatus() string  { return "publish" }
func (a *Attachment) PostType() string    { return "attachment" }

// Variant returns the named sub-size, if it exists.
func (a *Attachment) Variant(name string) *AttachmentVariant {
	for i := range a.Variants {
		if a.Variants[i].Name == name {
			return &a.Variants[i]
		}
	}
	return nil
}

const attCols = `id, coalesce(uploader_id,0), coalesce(post_id,0), slug, file_path, mime_type, file_size, coalesce(width,0),
	coalesce(height,0), alt_text, title, caption, description, menu_order, meta, created_at, updated_at`

func scanAttachment(row pgx.Row) (*Attachment, error) {
	a := &Attachment{}
	var meta []byte
	err := row.Scan(&a.ID, &a.UploaderID, &a.PostID, &a.Slug, &a.FilePath, &a.Mime, &a.FileSize, &a.Width, &a.Height,
		&a.Alt, &a.Title, &a.Caption, &a.Description, &a.MenuOrder, &meta, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Meta = map[string]any{}
	_ = json.Unmarshal(meta, &a.Meta)
	return a, nil
}

func (s *Store) loadVariants(ctx context.Context, list []*Attachment) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]int64, len(list))
	byID := map[int64]*Attachment{}
	for i, a := range list {
		ids[i] = a.ID
		byID[a.ID] = a
	}
	rows, err := s.Pool.Query(ctx, `SELECT media_id, name, file_path, width, height, mime_type, file_size FROM media_variants
		WHERE media_id = ANY($1) ORDER BY width`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var v AttachmentVariant
		if err := rows.Scan(&id, &v.Name, &v.FilePath, &v.Width, &v.Height, &v.Mime, &v.FileSize); err != nil {
			return err
		}
		byID[id].Variants = append(byID[id].Variants, v)
	}
	return rows.Err()
}

func (s *Store) GetAttachment(ctx context.Context, id int64) (*Attachment, error) {
	a, err := scanAttachment(s.Pool.QueryRow(ctx, `SELECT `+attCols+` FROM media WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	if err := s.loadVariants(ctx, []*Attachment{a}); err != nil {
		return a, err
	}
	filterAttachments(ctx, []*Attachment{a})
	return a, nil
}

// GetAttachments loads several attachments with their sub-sizes in two queries; missing ids are absent.
func (s *Store) GetAttachments(ctx context.Context, ids []int64) (map[int64]*Attachment, error) {
	out := map[int64]*Attachment{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+attCols+` FROM media WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	var list []*Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out[a.ID] = a
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.loadVariants(ctx, list); err != nil {
		return out, err
	}
	filterAttachments(ctx, list)
	return out, nil
}

// InsertAttachment stores an attachment and its sub-sizes.
func (s *Store) InsertAttachment(ctx context.Context, a *Attachment) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return InsertAttachmentTx(ctx, tx, a)
	})
}

// InsertAttachmentTx stores an attachment and its sub-sizes in a caller's transaction.
func InsertAttachmentTx(ctx context.Context, tx pgx.Tx, a *Attachment) error {
	meta, _ := json.Marshal(orEmpty(a.Meta))
	if err := tx.QueryRow(ctx, `INSERT INTO media(uploader_id, post_id, slug, file_path, mime_type, file_size, width, height,
		alt_text, title, caption, description, meta) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id, created_at, updated_at`,
		nullable(a.UploaderID), nullable(a.PostID), a.Slug, a.FilePath, a.Mime, a.FileSize, nullInt(a.Width), nullInt(a.Height),
		a.Alt, a.Title, a.Caption, a.Description, meta).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return err
	}
	for _, v := range a.Variants {
		if _, err := tx.Exec(ctx, `INSERT INTO media_variants(media_id, name, file_path, width, height, mime_type, file_size)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, a.ID, v.Name, v.FilePath, v.Width, v.Height, v.Mime, v.FileSize); err != nil {
			return err
		}
	}
	return nil
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// UpdateAttachment saves the editable fields (save-attachment / REST media update). An
// attachment read with translated texts (Base set) saves its base texts: a translation never
// overwrites the media row.
func (s *Store) UpdateAttachment(ctx context.Context, a *Attachment) error {
	meta, _ := json.Marshal(orEmpty(a.Meta))
	t := AttachmentText{Alt: a.Alt, Title: a.Title, Caption: a.Caption, Description: a.Description}
	if a.Base != nil {
		t = *a.Base
	}
	_, err := s.Pool.Exec(ctx, `UPDATE media SET post_id=$2, slug=$3, alt_text=$4, title=$5, caption=$6, description=$7,
		menu_order=$8, meta=$9 WHERE id=$1`, a.ID, nullable(a.PostID), a.Slug, t.Alt, t.Title, t.Caption, t.Description, a.MenuOrder, meta)
	return err
}

func (s *Store) DeleteAttachment(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM media WHERE id = $1`, id)
	return err
}

type AttachmentQuery struct {
	Search     string
	MimePrefix []string // "image", "audio", "video", or full types like "application/pdf"
	Year       int
	Month      int
	PostID     *int64 // uploaded to; 0 = unattached
	// PublicParentsOnly keeps unattached files and files of published posts without a password: what a visitor
	// could reach through the site anyway.
	PublicParentsOnly bool
	AuthorID          int64
	IDs               []int64
	Exclude           []int64
	ExcludeParentIDs  []int64 // files attached to these posts are left out (and not counted)
	OrderBy           string  // date, title, menuOrder, id
	Order             string
	Page              int
	PerPage           int
}

func (s *Store) QueryAttachments(ctx context.Context, q AttachmentQuery) ([]*Attachment, int, error) {
	var conds []string
	var args []any
	add := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	if q.Search != "" {
		p := add(likeContains(ValidText(q.Search)))
		conds = append(conds, "(title ILIKE "+p+" OR caption ILIKE "+p+" OR description ILIKE "+p+" OR file_path ILIKE "+p+" OR alt_text ILIKE "+p+")")
	}
	if len(q.MimePrefix) > 0 {
		var ors []string
		for _, m := range q.MimePrefix {
			if strings.Contains(m, "/") {
				ors = append(ors, "mime_type = "+add(m))
			} else {
				ors = append(ors, "mime_type LIKE "+add(m+"/%"))
			}
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	if q.Year > 0 {
		conds = append(conds, "extract(year FROM created_at) = "+add(q.Year))
		if q.Month > 0 {
			conds = append(conds, "extract(month FROM created_at) = "+add(q.Month))
		}
	}
	if q.PostID != nil {
		if *q.PostID == 0 {
			conds = append(conds, "post_id IS NULL")
		} else {
			conds = append(conds, "post_id = "+add(*q.PostID))
		}
	}
	if q.AuthorID > 0 {
		conds = append(conds, "uploader_id = "+add(q.AuthorID))
	}
	if q.PublicParentsOnly {
		conds = append(conds, "(post_id IS NULL OR post_id IN (SELECT id FROM posts WHERE status = 'publish' AND coalesce(password, '') = ''))")
	}
	if len(q.IDs) > 0 {
		conds = append(conds, "id = ANY("+add(q.IDs)+")")
	}
	if len(q.Exclude) > 0 {
		conds = append(conds, "NOT (id = ANY("+add(q.Exclude)+"))")
	}
	if len(q.ExcludeParentIDs) > 0 {
		conds = append(conds, "(post_id IS NULL OR NOT (post_id = ANY("+add(q.ExcludeParentIDs)+")))")
	}
	where := "TRUE"
	if len(conds) > 0 {
		where = strings.Join(conds, " AND ")
	}
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM media WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	col := "created_at"
	switch q.OrderBy {
	case "title":
		col = "lower(title)"
	case "menuOrder", "menu_order":
		col = "menu_order"
	case "id", "ID":
		col = "id"
	case "modified":
		col = "updated_at"
	}
	dir := "DESC"
	if strings.EqualFold(q.Order, "asc") {
		dir = "ASC"
	}
	sql := `SELECT ` + attCols + ` FROM media WHERE ` + where + ` ORDER BY ` + col + ` ` + dir + `, id ` + dir
	if q.PerPage > 0 {
		page := q.Page
		if page < 1 {
			page = 1
		}
		sql += limitOffset(q.PerPage, page)
	}
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	var list []*Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := s.loadVariants(ctx, list); err != nil {
		return list, total, err
	}
	filterAttachments(ctx, list)
	return list, total, nil
}

// AttachmentMonths lists months that have uploads, newest first.
func (s *Store) AttachmentMonths(ctx context.Context) ([]ArchiveMonth, error) {
	rows, err := s.Pool.Query(ctx, `SELECT extract(year FROM created_at)::int, extract(month FROM created_at)::int, count(*)
		FROM media GROUP BY 1, 2 ORDER BY 1 DESC, 2 DESC`)
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

// AttachmentTypeCounts returns how many attachments exist per top-level mime type.
func (s *Store) AttachmentTypeCounts(ctx context.Context) (map[string]int, int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT split_part(mime_type, '/', 1), count(*) FROM media GROUP BY 1`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string]int{}
	total := 0
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, 0, err
		}
		out[t] = n
		total += n
	}
	var unattached int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM media WHERE post_id IS NULL`).Scan(&unattached)
	out["unattached"] = unattached
	return out, total, rows.Err()
}

// ReplaceAttachmentFile stores a new main file (and its sub-sizes) for an attachment, as the image
// editor does when it saves an edited copy (wp_save_image()).
func (s *Store) ReplaceAttachmentFile(ctx context.Context, a *Attachment) error {
	meta, _ := json.Marshal(orEmpty(a.Meta))
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE media SET file_path=$2, mime_type=$3, file_size=$4, width=$5, height=$6, meta=$7 WHERE id=$1`,
			a.ID, a.FilePath, a.Mime, a.FileSize, a.Width, a.Height, meta); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM media_variants WHERE media_id = $1`, a.ID); err != nil {
			return err
		}
		for _, v := range a.Variants {
			if _, err := tx.Exec(ctx, `INSERT INTO media_variants(media_id, name, file_path, width, height, mime_type, file_size)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`, a.ID, v.Name, v.FilePath, v.Width, v.Height, v.Mime, v.FileSize); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReplaceAttachmentFiles is ReplaceAttachmentFile for a replaced file: in the same transaction the
// moved paths (old uploads path → the path that took its place) are recorded, earlier moves that
// pointed at a path that moved now point at its new target, and paths the attachment has again
// stop being redirects.
func (s *Store) ReplaceAttachmentFiles(ctx context.Context, a *Attachment, moves map[string]string, current []string) error {
	meta, _ := json.Marshal(orEmpty(a.Meta))
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE media SET file_path=$2, mime_type=$3, file_size=$4, width=$5, height=$6, meta=$7 WHERE id=$1`,
			a.ID, a.FilePath, a.Mime, a.FileSize, nullInt(a.Width), nullInt(a.Height), meta); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM media_variants WHERE media_id = $1`, a.ID); err != nil {
			return err
		}
		for _, v := range a.Variants {
			if _, err := tx.Exec(ctx, `INSERT INTO media_variants(media_id, name, file_path, width, height, mime_type, file_size)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`, a.ID, v.Name, v.FilePath, v.Width, v.Height, v.Mime, v.FileSize); err != nil {
				return err
			}
		}
		for from, to := range moves {
			if _, err := tx.Exec(ctx, `UPDATE media_moved_files SET target = $3 WHERE media_id = $1 AND target = $2`, a.ID, from, to); err != nil {
				return err
			}
		}
		for from, to := range moves {
			if from == to {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO media_moved_files(path, media_id, target) VALUES ($1,$2,$3)
				ON CONFLICT (path) DO UPDATE SET media_id = EXCLUDED.media_id, target = EXCLUDED.target, created_at = now()`,
				from, a.ID, to); err != nil {
				return err
			}
		}
		if len(current) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM media_moved_files WHERE path = ANY($1)`, current); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM media_moved_files WHERE media_id = $1 AND path = target`, a.ID)
		return err
	})
}

// MovedFile is a path an attachment no longer has after a replace, and where it went.
type MovedFile struct {
	Path    string
	MediaID int64
	Target  string
}

// MovedFiles lists the moved paths (of the given attachments; all when ids is nil).
func (s *Store) MovedFiles(ctx context.Context, ids []int64) ([]MovedFile, error) {
	q, args := `SELECT path, media_id, target FROM media_moved_files`, []any{}
	if ids != nil {
		q, args = q+` WHERE media_id = ANY($1)`, []any{ids}
	}
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MovedFile
	for rows.Next() {
		var m MovedFile
		if err := rows.Scan(&m.Path, &m.MediaID, &m.Target); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MovedFileTarget is the path a moved uploads path redirects to.
func (s *Store) MovedFileTarget(ctx context.Context, path string) (string, bool) {
	var t string
	if err := s.Pool.QueryRow(ctx, `SELECT target FROM media_moved_files WHERE path = $1`, path).Scan(&t); err != nil {
		return "", false
	}
	return t, true
}

// MovedPathsOfOthers lists which of paths are moved paths of attachments other than id.
func (s *Store) MovedPathsOfOthers(ctx context.Context, paths []string, id int64) (map[string]bool, error) {
	out := map[string]bool{}
	if len(paths) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT path FROM media_moved_files WHERE path = ANY($1) AND media_id <> $2`, paths, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// FileStateAttachments loads the attachments whose files carry a version or a focal point
// (meta file_version / focal_point), with their sub-sizes; ids limits them to some (nil: all).
func (s *Store) FileStateAttachments(ctx context.Context, ids []int64) ([]*Attachment, error) {
	q, args := `SELECT `+attCols+` FROM media WHERE (meta ? 'file_version' OR meta ? 'focal_point')`, []any{}
	if ids != nil {
		q, args = q+` AND id = ANY($1)`, []any{ids}
	}
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var list []*Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, s.loadVariants(ctx, list)
}

// SetAttachmentMeta merges keys into an attachment's meta (a nil value deletes the key) without
// touching the rest of the row, so it cannot undo a concurrent edit of the texts.
func (s *Store) SetAttachmentMeta(ctx context.Context, id int64, set map[string]any) error {
	add := map[string]any{}
	var del []string
	for k, v := range set {
		if v == nil {
			del = append(del, k)
		} else {
			add[k] = v
		}
	}
	raw, _ := json.Marshal(add)
	if del == nil {
		del = []string{}
	}
	_, err := s.Pool.Exec(ctx, `UPDATE media SET meta = (meta - $3::text[]) || $2::jsonb WHERE id = $1`, id, raw, del)
	return err
}

// ImageAttachmentIDs lists the ids of all image attachments (oldest first).
func (s *Store) ImageAttachmentIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM media WHERE mime_type LIKE 'image/%' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
