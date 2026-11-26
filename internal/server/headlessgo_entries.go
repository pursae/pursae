package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

// HeadlessGo entries: one row per document and locale. data is the draft (the latest saved
// values); published_data is the published snapshot, NULL while the entry is not published. A
// type without draft & publish publishes on every save. Relations and media are stored as lists
// of ids (document ids of entries, numeric ids of posts, users and attachments) whatever their
// cardinality; components and dynamic zones are nested objects.

type hgEntry struct {
	ID          int64
	DocumentID  string
	TypeUID     string
	Locale      string
	Data        map[string]any
	Published   map[string]any
	PublishedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CreatedBy   int64
	UpdatedBy   int64
	// Scheduled publishing (types with draft & publish): the scheduler publishes the draft at
	// PublishAt and unpublishes at UnpublishAt; ScheduleError says why a scheduled publication failed.
	PublishAt     *time.Time
	UnpublishAt   *time.Time
	ScheduleError string
	// Review flow (types with ReviewWorkflow): see headlessgo_review.go.
	ReviewStatus string // "", pending, approved, rejected, as stored
	ReviewHash   string // hash of the draft the status was given for
	ReviewBy     int64
	ReviewNote   string
	ReviewAt     *time.Time
}

// Status is draft (never published), published, or modified (published, with newer draft changes).
func (e *hgEntry) Status() string {
	if e.Published == nil {
		return "draft"
	}
	a, _ := json.Marshal(e.Data)
	b, _ := json.Marshal(e.Published)
	if string(a) != string(b) {
		return "modified"
	}
	return "published"
}

// Version returns the values of a status: "draft" or "published" (nil when not published).
func (e *hgEntry) Version(status string) map[string]any {
	if status == "draft" {
		return e.Data
	}
	return e.Published
}

const hgEntryCols = `id, document_id, type_uid, locale, data, published_data, published_at, created_at, updated_at, created_by, updated_by, publish_at, unpublish_at, schedule_error, review_status, review_hash, review_by, review_note, review_at`

func hgScanEntry(row pgx.Row) (*hgEntry, error) {
	e := &hgEntry{}
	var d, p []byte
	if err := row.Scan(&e.ID, &e.DocumentID, &e.TypeUID, &e.Locale, &d, &p, &e.PublishedAt, &e.CreatedAt, &e.UpdatedAt, &e.CreatedBy, &e.UpdatedBy,
		&e.PublishAt, &e.UnpublishAt, &e.ScheduleError, &e.ReviewStatus, &e.ReviewHash, &e.ReviewBy, &e.ReviewNote, &e.ReviewAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(d, &e.Data)
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	if p != nil {
		_ = json.Unmarshal(p, &e.Published)
	}
	return e, nil
}

func (s *Server) hgGetEntry(ctx context.Context, uid, doc, locale string) (*hgEntry, error) {
	return hgScanEntry(s.store.Pool.QueryRow(ctx, `SELECT `+hgEntryCols+` FROM hg_entries WHERE type_uid = $1 AND document_id = $2 AND locale = $3`, uid, doc, locale))
}

// hgDocLocales lists the versions of a document: locale -> entry.
func (s *Server) hgDocLocales(ctx context.Context, uid, doc string) map[string]*hgEntry {
	out := map[string]*hgEntry{}
	rows, err := s.store.Pool.Query(ctx, `SELECT `+hgEntryCols+` FROM hg_entries WHERE type_uid = $1 AND document_id = $2`, uid, doc)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		if e, err := hgScanEntry(rows); err == nil {
			out[e.Locale] = e
		}
	}
	return out
}

// hgSingleDoc is the document id of a single type's entry ("" when it has none yet).
func (s *Server) hgSingleDoc(ctx context.Context, uid string) string {
	var doc string
	_ = s.store.Pool.QueryRow(ctx, `SELECT document_id FROM hg_entries WHERE type_uid = $1 ORDER BY id LIMIT 1`, uid).Scan(&doc)
	return doc
}

// hgEntryLocale is the locale column of a type's entries: "" without i18n.
func (s *Server) hgEntryLocale(ctx context.Context, sc *hgSchema, requested string) (string, error) {
	if !sc.Localized {
		return "", nil
	}
	if requested == "" {
		return s.hgDefaultLocale(ctx), nil
	}
	if !s.hgValidLocale(ctx, requested) {
		return "", fmt.Errorf("Unknown locale %q.", requested)
	}
	return requested, nil
}

const hgDocAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// hgNewDocumentID is a random 24-character document id.
func hgNewDocumentID() string {
	b := make([]byte, 24)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(hgDocAlphabet))))
		b[i] = hgDocAlphabet[n.Int64()]
	}
	if b[0] >= '0' && b[0] <= '9' {
		b[0] = 'a' + b[0] - '0'
	}
	return string(b)
}

// Validation --------------------------------------------------------------------------------------

// hgFieldError is one validation problem, at a path of field names and list indexes.
type hgFieldError struct {
	Path    []string `json:"path"`
	Message string   `json:"message"`
	Name    string   `json:"name"`
}

type hgValidationError []hgFieldError

func (e hgValidationError) Error() string {
	var parts []string
	for _, fe := range e {
		if len(fe.Path) > 0 {
			parts = append(parts, strings.Join(fe.Path, ".")+": "+fe.Message)
		} else {
			parts = append(parts, fe.Message)
		}
	}
	return strings.Join(parts, " ")
}

// hgCoercer converts input values to stored ones for one save.
type hgCoercer struct {
	s        *Server
	ctx      context.Context
	reg      *hgRegistry
	required bool // check required fields (off for drafts of a draft & publish type)
	create   bool
	errs     hgValidationError
	inv      map[string]any // input of the root object's inverse relation fields (stored on the owners)
	// auth is the API caller writing (nil: the admin or an internal write). Media it may not
	// read (hgMediaReadable) cannot be linked.
	auth *hgAuth
	user *store.User // auth's WordPress user, when it is a signed-in front-end user
	// admin is the signed-in user of an admin-screen write (auth nil): it links media as core
	// /wp/v2/media lets it read them.
	admin *store.User
}

// mediaAllowed reports whether the writer may link an attachment (internal writes may link any).
func (h *hgCoercer) mediaAllowed(a *store.Attachment) bool {
	switch {
	case h.auth != nil:
		return h.s.hgMediaReadable(h.ctx, h.auth, h.user, a)
	case h.admin != nil:
		return mediaLibraryReader(h.admin) || h.s.attachmentPublic(h.ctx, a)
	}
	return true
}

func (h *hgCoercer) fail(path []string, format string, a ...any) {
	h.errs = append(h.errs, hgFieldError{Path: append([]string(nil), path...), Message: fmt.Sprintf(format, a...), Name: "ValidationError"})
}

var (
	hgUIDValueRe = regexp.MustCompile(`^[A-Za-z0-9\-_.~]*$`)
	hgDateRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	hgTimeRe     = regexp.MustCompile(`^(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,3}))?)?$`)
)

// object coerces the input of one object (an entry or a component value) of schema sc over the
// old stored values: input keys replace old ones, absent keys keep them.
func (h *hgCoercer) object(sc *hgSchema, in, old map[string]any, path []string, isRoot bool) map[string]any {
	out := map[string]any{}
	for k, v := range old {
		if sc.Field(k) != nil {
			out[k] = v
		}
	}
	for k := range in {
		if sc.Field(k) == nil && !hgReservedFields[k] {
			h.fail(append(path, k), "Invalid key %s", k)
		}
	}
	for i := range sc.Fields {
		f := &sc.Fields[i]
		fp := append(append([]string(nil), path...), f.Name)
		v, given := in[f.Name]
		if f.MappedBy != "" { // an inverse relation: nothing is stored here
			delete(out, f.Name)
			if given && isRoot {
				if h.inv == nil {
					h.inv = map[string]any{}
				}
				h.inv[f.Name] = v
			}
			continue
		}
		if !given {
			if _, had := out[f.Name]; !had && (h.create || !isRoot) && f.Default != nil {
				if dv, ok := h.value(sc, f, f.Default, nil, fp); ok && dv != nil {
					out[f.Name] = dv
				}
			}
			if f.Type == "uid" && f.TargetField != "" && hgEmpty(out[f.Name]) {
				if src, ok := out[f.TargetField].(string); ok && src != "" {
					out[f.Name] = hgSlugify(src)
				}
			}
			h.check(f, out[f.Name], fp)
			continue
		}
		var oldV any
		if old != nil {
			oldV = old[f.Name]
		}
		nv, ok := h.value(sc, f, v, oldV, fp)
		if !ok {
			continue
		}
		if nv == nil {
			delete(out, f.Name)
		} else {
			out[f.Name] = nv
		}
		if f.Type == "uid" && f.TargetField != "" && hgEmpty(out[f.Name]) {
			if src, ok := out[f.TargetField].(string); ok && src != "" {
				out[f.Name] = hgSlugify(src)
			}
		}
		h.check(f, out[f.Name], fp)
	}
	return out
}

func hgEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// check applies required, min and max to a stored value.
func (h *hgCoercer) check(f *hgField, v any, path []string) {
	if hgEmpty(v) {
		if f.Required && h.required && f.Type != "boolean" {
			h.fail(path, "This attribute is required")
		} else if f.Required && h.required && f.Type == "boolean" && v == nil {
			h.fail(path, "This attribute is required")
		}
		return
	}
	var n float64
	what := ""
	switch f.Type {
	case "text", "email", "uid", "password":
		s, _ := v.(string)
		if f.Type == "password" {
			return // checked on the plain value
		}
		n, what = float64(utf8.RuneCountInString(s)), "characters"
	case "richtext":
		if s, ok := v.(string); ok {
			n, what = float64(utf8.RuneCountInString(s)), "characters"
		} else {
			return
		}
	case "number":
		switch t := v.(type) {
		case float64:
			n = t
		case string:
			n, _ = strconv.ParseFloat(t, 64)
		}
		what = "value"
	case "component", "media", "relation", "dynamiczone":
		if list, ok := v.([]any); ok && f.multi() {
			n, what = float64(len(list)), "items"
		} else {
			return
		}
	default:
		return
	}
	if f.Min != nil && n < *f.Min {
		if what == "value" {
			h.fail(path, "This attribute must be greater than or equal to %s", hgNum(*f.Min))
		} else {
			h.fail(path, "This attribute must have at least %s %s", hgNum(*f.Min), what)
		}
	}
	if f.Max != nil && n > *f.Max {
		if what == "value" {
			h.fail(path, "This attribute must be less than or equal to %s", hgNum(*f.Max))
		} else {
			h.fail(path, "This attribute must have at most %s %s", hgNum(*f.Max), what)
		}
	}
}

func hgNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// value converts one input value; ok is false when it was rejected (the error is recorded).
func (h *hgCoercer) value(sc *hgSchema, f *hgField, v, old any, path []string) (any, bool) {
	if v == nil {
		if f.Type == "password" {
			return old, true
		}
		return nil, true
	}
	switch f.Type {
	case "text", "email", "uid", "enumeration":
		str, ok := v.(string)
		if !ok {
			h.fail(path, "This attribute must be a string")
			return nil, false
		}
		if f.Type == "uid" {
			str = strings.TrimSpace(str)
		}
		if str == "" {
			return nil, true
		}
		switch f.Type {
		case "email":
			if a, err := mail.ParseAddress(str); err != nil || a.Address != str {
				h.fail(path, "This attribute must be a valid email")
				return nil, false
			}
		case "uid":
			if !hgUIDValueRe.MatchString(str) {
				h.fail(path, "This attribute must match the regex %s", hgUIDValueRe.String())
				return nil, false
			}
		case "enumeration":
			if !containsStr(f.Enum, str) {
				h.fail(path, "This attribute must be one of the following values: %s", strings.Join(f.Enum, ", "))
				return nil, false
			}
		}
		if f.Regex != "" {
			if re, err := regexp.Compile(f.Regex); err == nil && !re.MatchString(str) {
				h.fail(path, "This attribute must match the regex %s", f.Regex)
				return nil, false
			}
		}
		return str, true
	case "password":
		str, ok := v.(string)
		if !ok {
			h.fail(path, "This attribute must be a string")
			return nil, false
		}
		if str == "" {
			return old, true // an empty password field keeps the stored hash
		}
		n := float64(utf8.RuneCountInString(str))
		if f.Min != nil && n < *f.Min {
			h.fail(path, "This attribute must have at least %s characters", hgNum(*f.Min))
			return nil, false
		}
		if f.Max != nil && n > *f.Max {
			h.fail(path, "This attribute must have at most %s characters", hgNum(*f.Max))
			return nil, false
		}
		hash, err := auth.HashPassword(str)
		if err != nil {
			h.fail(path, "The password could not be stored")
			return nil, false
		}
		return hash, true
	case "richtext":
		if f.Format == "markdown" {
			str, ok := v.(string)
			if !ok {
				h.fail(path, "This attribute must be a string")
				return nil, false
			}
			if str == "" {
				return nil, true
			}
			return str, true
		}
		return h.blocks(v, path)
	case "number":
		return h.number(f, v, path)
	case "boolean":
		switch t := v.(type) {
		case bool:
			return t, true
		case string:
			if t == "true" || t == "1" {
				return true, true
			}
			if t == "false" || t == "0" {
				return false, true
			}
			if t == "" {
				return nil, true
			}
		case float64:
			return t != 0, true
		}
		h.fail(path, "This attribute must be a boolean")
		return nil, false
	case "date", "datetime", "time":
		str, ok := v.(string)
		if !ok {
			h.fail(path, "This attribute must be a valid %s", f.Type)
			return nil, false
		}
		if str = strings.TrimSpace(str); str == "" {
			return nil, true
		}
		out, ok := hgNormTemporal(f.Type, str)
		if !ok {
			h.fail(path, "This attribute must be a valid %s", f.Type)
			return nil, false
		}
		return out, true
	case "json":
		return v, true
	case "media":
		return h.media(f, v, path)
	case "relation":
		return h.relation(sc, f, v, old, path)
	case "component":
		comp := h.reg.Get(f.Component)
		if comp == nil {
			return nil, false
		}
		if f.Repeatable {
			list, ok := v.([]any)
			if !ok {
				h.fail(path, "This attribute must be an array")
				return nil, false
			}
			oldList, _ := old.([]any)
			var out []any
			for i, item := range list {
				m, ok := item.(map[string]any)
				if !ok {
					h.fail(append(path, strconv.Itoa(i)), "This attribute must be an object")
					continue
				}
				out = append(out, h.component(comp, m, hgMatchOld(m, oldList), append(append([]string(nil), path...), strconv.Itoa(i))))
			}
			if out == nil {
				out = []any{}
			}
			return out, true
		}
		m, ok := v.(map[string]any)
		if !ok {
			h.fail(path, "This attribute must be an object")
			return nil, false
		}
		oldM, _ := old.(map[string]any)
		return h.component(comp, m, oldM, path), true
	case "dynamiczone":
		list, ok := v.([]any)
		if !ok {
			h.fail(path, "This attribute must be an array")
			return nil, false
		}
		oldList, _ := old.([]any)
		out := []any{}
		for i, item := range list {
			ip := append(append([]string(nil), path...), strconv.Itoa(i))
			m, ok := item.(map[string]any)
			if !ok {
				h.fail(ip, "This attribute must be an object")
				continue
			}
			cu, _ := m["__component"].(string)
			if !containsStr(f.Components, cu) {
				h.fail(ip, "The component %q is not allowed in this dynamic zone", cu)
				continue
			}
			comp := h.reg.Get(cu)
			if comp == nil {
				continue
			}
			var oldM map[string]any
			if om := hgMatchOld(m, oldList); om != nil && om["__component"] == cu {
				oldM = om
			}
			obj := h.component(comp, m, oldM, ip)
			obj["__component"] = cu
			out = append(out, obj)
		}
		return out, true
	}
	return v, true
}

// hgMatchOld finds the stored component a list item updates, by its id.
func hgMatchOld(m map[string]any, old []any) map[string]any {
	id, ok := m["id"].(float64)
	if !ok {
		return nil
	}
	for _, o := range old {
		if om, ok := o.(map[string]any); ok {
			if oid, _ := om["id"].(float64); oid == id {
				return om
			}
		}
	}
	return nil
}

// component coerces one component value; it keeps its stored id or gets a new one.
func (h *hgCoercer) component(comp *hgSchema, in, old map[string]any, path []string) map[string]any {
	saveCreate := h.create
	if old == nil {
		h.create = true // a new component gets its defaults
	}
	out := h.object(comp, in, old, path, false)
	h.create = saveCreate
	if old != nil {
		if id, ok := old["id"].(float64); ok {
			out["id"] = id
			return out
		}
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<31-1))
	out["id"] = float64(n.Int64() + 1)
	return out
}

// hgMaxSafeInt is 2^53: integers from here on are not all exact in a float64.
const hgMaxSafeInt = 1 << 53

func (h *hgCoercer) number(f *hgField, v any, path []string) (any, bool) {
	var str string
	switch t := v.(type) {
	case float64:
		if f.isInteger() && math.Abs(t) >= hgMaxSafeInt {
			// JSON numbers arrive as float64: this one may already have been rounded.
			msg := "This integer is too large to send as a JSON number"
			if f.Format == "biginteger" {
				msg += "; send it as a string"
			}
			h.fail(path, "%s", msg)
			return nil, false
		}
		str = strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		str = strings.TrimSpace(t)
		if str == "" {
			return nil, true
		}
	case json.Number:
		str = t.String()
	default:
		h.fail(path, "This attribute must be a number")
		return nil, false
	}
	if f.Format == "biginteger" {
		if _, ok := new(big.Int).SetString(str, 10); !ok {
			h.fail(path, "This attribute must be an integer")
			return nil, false
		}
		return str, true // stored and returned as a string, like the large integers of headless CMSs
	}
	n, err := strconv.ParseFloat(str, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		h.fail(path, "This attribute must be a number")
		return nil, false
	}
	if f.Format == "integer" && n != math.Trunc(n) {
		h.fail(path, "This attribute must be an integer")
		return nil, false
	}
	if f.Format == "integer" && math.Abs(n) >= hgMaxSafeInt {
		h.fail(path, "This integer is too large; use the biginteger format")
		return nil, false
	}
	return n, true
}

// hgNormTemporal stores dates as YYYY-MM-DD, times as HH:MM:SS.mmm and date-times as UTC
// RFC 3339 with milliseconds, so text order is time order.
func hgNormTemporal(kind, v string) (string, bool) {
	switch kind {
	case "date":
		if hgDateRe.MatchString(v) {
			if _, err := time.Parse("2006-01-02", v); err == nil {
				return v, true
			}
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.Format("2006-01-02"), true
		}
	case "time":
		m := hgTimeRe.FindStringSubmatch(v)
		if m == nil {
			return "", false
		}
		hh, _ := strconv.Atoi(m[1])
		mm, _ := strconv.Atoi(m[2])
		ss, _ := strconv.Atoi(defaultStr(m[3], "0"))
		ms := m[4]
		for len(ms) < 3 {
			ms += "0"
		}
		if hh > 23 || mm > 59 || ss > 59 {
			return "", false
		}
		return fmt.Sprintf("%02d:%02d:%02d.%s", hh, mm, ss, ms), true
	case "datetime":
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UTC().Format("2006-01-02T15:04:05.000Z"), true
			}
		}
	}
	return "", false
}

// blocks accepts the rich-text blocks format: a list of nodes with a type. A string becomes
// paragraphs (split on blank lines).
func (h *hgCoercer) blocks(v any, path []string) (any, bool) {
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, true
		}
		var out []any
		for _, para := range regexp.MustCompile(`\n\s*\n`).Split(strings.TrimSpace(t), -1) {
			out = append(out, map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": para}}})
		}
		return out, true
	case []any:
		for i, b := range t {
			m, ok := b.(map[string]any)
			if !ok {
				h.fail(append(path, strconv.Itoa(i)), "A block must be an object")
				return nil, false
			}
			if typ, _ := m["type"].(string); typ == "" {
				h.fail(append(path, strconv.Itoa(i)), "A block needs a type")
				return nil, false
			}
		}
		if len(t) == 0 {
			return nil, true
		}
		return t, true
	}
	h.fail(path, "This attribute must be a list of blocks")
	return nil, false
}

// hgMediaKind is the allowedTypes group of a MIME type.
func hgMediaKind(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "images"
	case strings.HasPrefix(mime, "video/"):
		return "videos"
	case strings.HasPrefix(mime, "audio/"):
		return "audios"
	}
	return "files"
}

// hgIDList reads one id or a list of ids (numbers, numeric strings or objects with an id).
func hgIDList(v any) ([]any, bool) {
	var list []any
	switch t := v.(type) {
	case []any:
		list = t
	default:
		list = []any{t}
	}
	var out []any
	for _, x := range list {
		switch t := x.(type) {
		case map[string]any:
			if id, ok := t["id"]; ok {
				x = id
			} else if d, ok := t["documentId"]; ok {
				x = d
			} else {
				return nil, false
			}
		}
		out = append(out, x)
	}
	return out, true
}

func hgToInt(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), t == math.Trunc(t) && t > 0
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n, err == nil && n > 0
	case json.Number:
		n, err := t.Int64()
		return n, err == nil && n > 0
	}
	return 0, false
}

func (h *hgCoercer) media(f *hgField, v any, path []string) (any, bool) {
	list, ok := hgIDList(v)
	if !ok {
		h.fail(path, "This attribute must be a media id or a list of media ids")
		return nil, false
	}
	if !f.Multiple && len(list) > 1 {
		h.fail(path, "This attribute accepts a single file")
		return nil, false
	}
	out := []any{}
	for _, x := range list {
		id, ok := hgToInt(x)
		if !ok {
			h.fail(path, "Invalid media id")
			return nil, false
		}
		a, err := h.s.store.GetAttachment(h.ctx, id)
		if err != nil {
			h.fail(path, "The media %d does not exist", id)
			return nil, false
		}
		if !h.mediaAllowed(a) {
			h.fail(path, "The media %d does not exist", id) // as if missing: its existence is not public either
			return nil, false
		}
		if len(f.Allowed) > 0 && !containsStr(f.Allowed, hgMediaKind(a.Mime)) {
			h.fail(path, "The file type of media %d is not allowed here", id)
			return nil, false
		}
		out = append(out, float64(id))
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, true
}

// relation reads a relation value: an id, a list, or {connect, disconnect, set}. Entries are
// referenced by document id (a numeric entry id is mapped to its document); posts, pages and
// users by their numeric id.
func (h *hgCoercer) relation(sc *hgSchema, f *hgField, v, old any, path []string) (any, bool) {
	var cur []any
	if ol, ok := old.([]any); ok {
		cur = append(cur, ol...)
	}
	resolve := func(raw any) ([]any, bool) {
		list, ok := hgIDList(raw)
		if !ok {
			h.fail(path, "Invalid relation value")
			return nil, false
		}
		var out []any
		for _, x := range list {
			id, ok := h.relTarget(f, x)
			if !ok {
				h.fail(path, "Relation target %v of %s was not found", x, f.Target)
				return nil, false
			}
			out = append(out, id)
		}
		return out, true
	}
	var ids []any
	if m, ok := v.(map[string]any); ok && (m["connect"] != nil || m["disconnect"] != nil || m["set"] != nil) {
		if set, ok := m["set"]; ok {
			r, ok := resolve(set)
			if !ok {
				return nil, false
			}
			cur = r
		}
		if dis, ok := m["disconnect"]; ok {
			r, ok := resolve(dis)
			if !ok {
				return nil, false
			}
			var keep []any
			for _, x := range cur {
				if !hgAnyIn(x, r) {
					keep = append(keep, x)
				}
			}
			cur = keep
		}
		if con, ok := m["connect"]; ok {
			r, ok := resolve(con)
			if !ok {
				return nil, false
			}
			if !f.multi() {
				cur = nil
			}
			for _, x := range r {
				if !hgAnyIn(x, cur) {
					cur = append(cur, x)
				}
			}
		}
		ids = cur
	} else {
		r, ok := resolve(v)
		if !ok {
			return nil, false
		}
		ids = r
	}
	var uniq []any
	for _, x := range ids {
		if !hgAnyIn(x, uniq) {
			uniq = append(uniq, x)
		}
	}
	if !f.multi() && len(uniq) > 1 {
		h.fail(path, "This relation accepts a single entry")
		return nil, false
	}
	if len(uniq) == 0 {
		return nil, true
	}
	return uniq, true
}

func hgAnyIn(x any, list []any) bool {
	for _, y := range list {
		if fmt.Sprint(x) == fmt.Sprint(y) {
			return true
		}
	}
	return false
}

// relTarget resolves one relation reference to its stored id.
func (h *hgCoercer) relTarget(f *hgField, x any) (any, bool) {
	ctx := h.ctx
	switch {
	case hgIsWPPost(f.Target):
		id, ok := hgToInt(x)
		if !ok {
			return nil, false
		}
		typ, _ := hgWPPostType(f.Target)
		p, err := h.s.store.GetPost(ctx, id)
		if err != nil || p.Type != typ {
			return nil, false
		}
		return float64(id), true
	case hgIsWPTerm(f.Target):
		id, ok := hgToInt(x)
		if !ok {
			return nil, false
		}
		tax, _ := hgWPTaxonomy(f.Target)
		if t, err := h.s.store.GetTerm(ctx, id); err != nil || t.Taxonomy != tax {
			return nil, false
		}
		return float64(id), true
	case f.Target == "wp:user":
		id, ok := hgToInt(x)
		if !ok {
			return nil, false
		}
		if _, err := h.s.store.UserByID(ctx, id); err != nil {
			return nil, false
		}
		return float64(id), true
	}
	var doc string
	if str, ok := x.(string); ok && str != "" {
		if _, isNum := hgToInt(str); !isNum {
			doc = str
		}
	}
	if doc == "" {
		id, ok := hgToInt(x)
		if !ok {
			return nil, false
		}
		if h.s.store.Pool.QueryRow(ctx, `SELECT document_id FROM hg_entries WHERE id = $1 AND type_uid = $2`, id, f.Target).Scan(&doc) != nil {
			return nil, false
		}
		return doc, true
	}
	var exists bool
	_ = h.s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hg_entries WHERE document_id = $1 AND type_uid = $2)`, doc, f.Target).Scan(&exists)
	return doc, exists
}

// Saving ------------------------------------------------------------------------------------------

// hgCheckUnique checks unique (and uid) fields and the one-sided relations (one-to-one and
// one-to-many: a target belongs to one entry) against the other documents of the type.
func (s *Server) hgCheckUnique(ctx context.Context, sc *hgSchema, doc, locale string, data map[string]any) hgValidationError {
	var errs hgValidationError
	for _, f := range sc.Fields {
		v, ok := data[f.Name]
		if !ok || hgEmpty(v) {
			continue
		}
		if f.Unique || f.Type == "uid" {
			raw, _ := json.Marshal(map[string]any{f.Name: v})
			var taken bool
			_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hg_entries WHERE type_uid = $1 AND locale = $2 AND document_id <> $3
				AND (data @> $4::jsonb OR published_data @> $4::jsonb))`, sc.UID, locale, doc, raw).Scan(&taken)
			if taken {
				errs = append(errs, hgFieldError{Path: []string{f.Name}, Message: "This attribute must be unique", Name: "ValidationError"})
			}
		}
		if f.Type == "relation" && (f.Relation == "oneToOne" || f.Relation == "oneToMany") {
			list, _ := v.([]any)
			for _, x := range list {
				raw, _ := json.Marshal(map[string]any{f.Name: []any{x}})
				var taken bool
				_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hg_entries WHERE type_uid = $1 AND locale = $2 AND document_id <> $3
					AND data @> $4::jsonb)`, sc.UID, locale, doc, raw).Scan(&taken)
				if taken {
					errs = append(errs, hgFieldError{Path: []string{f.Name}, Message: fmt.Sprintf("%v is already related to another entry", x), Name: "ValidationError"})
				}
			}
		}
	}
	return errs
}

// hgSaveOpts are the options of a create or update.
type hgSaveOpts struct {
	Locale  string // the entry locale column ("" for a type without i18n)
	Publish bool   // also publish (ignored without draft & publish: always published)
	UserID  int64
	// Auth is the API caller (nil: the admin). Writing an inverse relation edits the owning
	// entries, so it needs update access to the owning type too.
	Auth *hgAuth
	// User is the signed-in user of an admin-screen write (with Auth nil): the owning entries an
	// inverse relation edits must be theirs unless they may edit others' posts.
	User *store.User
}

var errHGNotFound = errors.New("not found")

// hgSaveEntry creates (doc == "") or updates a document's locale version, creating that
// version when the document exists in another locale only. It returns the saved entry and the
// webhook events to send.
//
// An update is a read-merge-write: it only applies when the stored version is still the one it
// read (updated_at), otherwise it is merged again over the newer version, so concurrent API writes
// to different attributes of one entry do not undo each other.
func (s *Server) hgSaveEntry(ctx context.Context, sc *hgSchema, doc string, input map[string]any, o hgSaveOpts) (*hgEntry, []string, error) {
	for try := 0; ; try++ {
		e, events, err := s.hgSaveEntryOnce(ctx, sc, doc, input, o)
		if !errors.Is(err, errHGConflict) || try >= 4 {
			return e, events, err
		}
	}
}

// errHGConflict: the entry changed between reading and writing it (hgSaveEntry tries again).
var errHGConflict = errors.New("The entry was changed by another request at the same time; try again.")

func (s *Server) hgSaveEntryOnce(ctx context.Context, sc *hgSchema, doc string, input map[string]any, o hgSaveOpts) (*hgEntry, []string, error) {
	reg := s.hgReg(ctx)
	var existing *hgEntry
	create := doc == ""
	if !create {
		e, err := s.hgGetEntry(ctx, sc.UID, doc, o.Locale)
		if err == nil {
			existing = e
		} else if len(s.hgDocLocales(ctx, sc.UID, doc)) == 0 {
			return nil, nil, errHGNotFound
		}
	} else {
		doc = hgNewDocumentID()
	}
	publish := o.Publish || !sc.DraftAndPublish
	if publish && sc.ReviewWorkflow && sc.DraftAndPublish {
		// New values cannot have been approved: save the draft, get it approved, then publish it.
		return nil, nil, errHGNotApproved
	}
	h := &hgCoercer{s: s, ctx: ctx, reg: reg, required: publish, create: existing == nil, auth: o.Auth}
	if o.Auth != nil {
		h.user = s.hgWPUser(ctx, o.Auth)
	} else {
		h.admin = o.User
	}
	var old map[string]any
	if existing != nil {
		old = existing.Data
	}
	data := h.object(sc, input, old, nil, true)
	invChanges := h.inverse(sc, doc, o.Locale)
	if o.Auth != nil {
		for _, ch := range invChanges {
			if !o.Auth.can(ch.owner.UID, "update") || o.Auth.ownerOnly(ch.owner.UID, "update") {
				h.fail([]string{ch.inverse}, "Changing this relation edits %s entries: it needs update access to %s", ch.owner.DisplayName, ch.owner.DisplayName)
			}
		}
	} else if u := o.User; u != nil && !u.Can("edit_others_posts", nil) {
		for _, ch := range invChanges {
			for _, d := range append(append([]string(nil), ch.add...), ch.remove...) {
				if !s.hgOwnsDoc(ctx, u, ch.owner, d, ch.locale, false) {
					h.fail([]string{ch.inverse}, "Changing this relation edits %s entries of other users", ch.owner.DisplayName)
					break
				}
			}
		}
	}
	if len(h.errs) > 0 {
		return nil, nil, h.errs
	}
	hookAction := "update"
	if existing == nil {
		hookAction = "create"
	}
	if publish {
		hookAction = "publish"
	}
	hookDoc := doc
	if existing == nil {
		hookDoc = ""
	}
	var herrs hgValidationError
	if data, herrs = s.hgValidateHook(ctx, sc, h, hookDoc, o.Locale, hookAction, data, old); len(herrs) > 0 {
		return nil, nil, herrs
	}
	if errs := s.hgCheckUnique(ctx, sc, doc, o.Locale, data); len(errs) > 0 {
		return nil, nil, errs
	}
	raw, _ := json.Marshal(data)
	var e *hgEntry
	var err error
	if existing == nil {
		var pub any
		var pubAt *time.Time
		if publish {
			pub = raw
			now := time.Now()
			pubAt = &now
		}
		e, err = hgScanEntry(s.store.Pool.QueryRow(ctx, `INSERT INTO hg_entries(document_id, type_uid, locale, data, published_data, published_at, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING `+hgEntryCols, doc, sc.UID, o.Locale, raw, pub, pubAt, o.UserID))
	} else if publish {
		e, err = hgScanEntry(s.store.Pool.QueryRow(ctx, `UPDATE hg_entries SET data = $2, published_data = $2, published_at = now(), publish_at = NULL, schedule_error = '',
			updated_at = now(), updated_by = $3 WHERE id = $1 AND updated_at = $4 RETURNING `+hgEntryCols, existing.ID, raw, o.UserID, existing.UpdatedAt))
	} else {
		e, err = hgScanEntry(s.store.Pool.QueryRow(ctx, `UPDATE hg_entries SET data = $2, updated_at = now(), updated_by = $3
			WHERE id = $1 AND updated_at = $4 RETURNING `+hgEntryCols, existing.ID, raw, o.UserID, existing.UpdatedAt))
	}
	if errors.Is(err, pgx.ErrNoRows) && existing != nil {
		return nil, nil, errHGConflict
	}
	if err != nil {
		return nil, nil, err
	}
	if err := s.hgApplyInverse(ctx, doc, invChanges, hgEntryLabel(sc, e.Data, doc), o.UserID); err != nil {
		return nil, nil, err
	}
	action := "save"
	if existing == nil {
		action = "create"
	}
	if publish && sc.DraftAndPublish {
		action = "publish"
	}
	s.hgRecordVersion(ctx, e, action, o.UserID)
	s.hgRecordLinkHistory(ctx, sc, e, old, o.UserID)
	events := []string{"entry.update"}
	if existing == nil {
		events = []string{"entry.create"}
	}
	if publish && sc.DraftAndPublish {
		events = append(events, "entry.publish")
	}
	return e, events, nil
}

// hgPublishEntry publishes the draft of a document's locale version (validating required fields).
func (s *Server) hgPublishEntry(ctx context.Context, sc *hgSchema, doc, locale string, userID int64) (*hgEntry, error) {
	// The row stays locked from the review and validation checks to the copy, so a save landing in
	// between cannot put a draft live that was never approved or validated.
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := hgScanEntry(tx.QueryRow(ctx, `SELECT `+hgEntryCols+` FROM hg_entries WHERE type_uid = $1 AND document_id = $2 AND locale = $3 FOR UPDATE`, sc.UID, doc, locale))
	if err != nil {
		return nil, errHGNotFound
	}
	if err := hgReviewGate(sc, e); err != nil {
		return nil, err
	}
	h := &hgCoercer{s: s, ctx: ctx, reg: s.hgReg(ctx), required: true}
	h.object(sc, map[string]any{}, e.Data, nil, true)
	if len(h.errs) > 0 {
		return nil, h.errs
	}
	// A manual publication replaces a pending scheduled one: the scheduler would otherwise put
	// whatever draft exists at that time live.
	pe, err := hgScanEntry(tx.QueryRow(ctx, `UPDATE hg_entries SET published_data = data, published_at = now(), publish_at = NULL, updated_by = $2, schedule_error = ''
		WHERE id = $1 RETURNING `+hgEntryCols, e.ID, userID))
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	s.hgRecordVersion(ctx, pe, "publish", userID)
	return pe, nil
}

// hgUnpublishEntry keeps only the draft.
func (s *Server) hgUnpublishEntry(ctx context.Context, sc *hgSchema, doc, locale string) (*hgEntry, error) {
	if !sc.DraftAndPublish {
		return nil, errors.New("This content type does not use draft & publish.")
	}
	e, err := hgScanEntry(s.store.Pool.QueryRow(ctx, `UPDATE hg_entries SET published_data = NULL, published_at = NULL, unpublish_at = NULL
		WHERE type_uid = $1 AND document_id = $2 AND locale = $3 RETURNING `+hgEntryCols, sc.UID, doc, locale))
	if err != nil {
		return nil, errHGNotFound
	}
	s.hgRecordVersion(ctx, e, "unpublish", 0)
	return e, nil
}

// hgDiscardDraft resets the draft to the published version.
func (s *Server) hgDiscardDraft(ctx context.Context, sc *hgSchema, doc, locale string) error {
	e, err := hgScanEntry(s.store.Pool.QueryRow(ctx, `UPDATE hg_entries SET data = published_data, updated_at = now()
		WHERE type_uid = $1 AND document_id = $2 AND locale = $3 AND published_data IS NOT NULL RETURNING `+hgEntryCols, sc.UID, doc, locale))
	if err != nil {
		return errHGNotFound
	}
	s.hgRecordVersion(ctx, e, "discard", 0)
	return nil
}

// hgDeleteEntries deletes a document's version in one locale, or in every locale when locale is
// "*". It returns the deleted entries. userID (0: an API call) is recorded in the history of the
// entries that lose their links to the document when no version of it is left.
func (s *Server) hgDeleteEntries(ctx context.Context, sc *hgSchema, doc, locale string, userID int64) ([]*hgEntry, error) {
	q := `DELETE FROM hg_entries WHERE type_uid = $1 AND document_id = $2 AND locale = $3 RETURNING ` + hgEntryCols
	args := []any{sc.UID, doc, locale}
	if locale == "*" {
		q = `DELETE FROM hg_entries WHERE type_uid = $1 AND document_id = $2 RETURNING ` + hgEntryCols
		args = args[:2]
	}
	rows, err := s.store.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*hgEntry
	for rows.Next() {
		if e, err := hgScanEntry(rows); err == nil {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, errHGNotFound
	}
	rows.Close()
	for _, e := range out {
		s.hgRecordDeletedOwner(ctx, sc, e, userID)
	}
	if err := s.hgUnlinkDocument(ctx, sc.UID, doc, hgEntryLabel(sc, out[0].Data, doc), userID); err != nil {
		return out, err
	}
	return out, nil
}
