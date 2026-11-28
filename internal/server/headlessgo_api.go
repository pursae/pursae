package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// The delivery REST API: /wp-json/headlessgo/v1/{pluralApiId}[/{documentId}] for collection
// types, /wp-json/headlessgo/v1/{singularApiId} for single types, /wp/menus for WordPress menus
// and /graphql. Responses are {data, meta}; errors are {data: null, error: {status, name,
// message, details}}.

func hgErr(c *gin.Context, status int, name, msg string, details any) {
	if details == nil {
		details = map[string]any{}
	}
	c.Header("Cache-Control", "no-store")
	c.AbortWithStatusJSON(status, map[string]any{"data": nil, "error": map[string]any{"status": status, "name": name, "message": msg, "details": details}})
}

func hgNotFound(c *gin.Context) { hgErr(c, http.StatusNotFound, "NotFoundError", "Not Found", nil) }

func hgForbidden(c *gin.Context, a *hgAuth) {
	if a.Kind == "public" {
		hgErr(c, http.StatusForbidden, "ForbiddenError", "Forbidden", nil)
		return
	}
	hgErr(c, http.StatusForbidden, "ForbiddenError", "Forbidden", nil)
}

// hgFail answers a store or validation error.
func hgFail(c *gin.Context, err error) {
	var ve hgValidationError
	var qe *hgQueryError
	switch {
	case errors.As(err, &ve):
		msg := "Validation error"
		if len(ve) == 1 {
			msg = ve[0].Message
		} else if len(ve) > 1 {
			msg = strconv.Itoa(len(ve)) + " errors occurred"
		}
		hgErr(c, http.StatusBadRequest, "ValidationError", msg, map[string]any{"errors": []hgFieldError(ve)})
	case errors.As(err, &qe):
		hgErr(c, http.StatusBadRequest, "ValidationError", qe.msg, nil)
	case errors.Is(err, errHGNotFound):
		hgNotFound(c)
	case errors.Is(err, errHGNotApproved):
		hgErr(c, http.StatusBadRequest, "ValidationError", err.Error(), nil)
	case errors.Is(err, errHGConflict):
		hgErr(c, http.StatusConflict, "ConflictError", err.Error(), nil)
	default:
		hgErr(c, http.StatusInternalServerError, "ApplicationError", "Internal Server Error", nil)
	}
}

// Authentication -------------------------------------------------------------------------------------

// hgAuth is who is asking: the public role, an API token or a preview token.
type hgAuth struct {
	Kind    string // public, token, preview
	Token   *hgToken
	Public  map[string][]string
	PrevUID string
	PrevDoc string
	PrevLoc string // the locale a preview token reads ("" for a type without i18n)
	// A signed-in front-end user (Kind "user", headlessgo_users.go).
	UserID        int64
	Authenticated map[string][]string
	OwnerOnly     map[string][]string
}

func (a *hgAuth) can(uid, action string) bool {
	switch a.Kind {
	case hgUserKind:
		return containsStr(a.Public[uid], action) || containsStr(a.Authenticated[uid], action)
	case "preview": // its own type; other types (populate, relation filters) as the public role
		return (action == "find" || action == "findOne") && (uid == a.PrevUID || containsStr(a.Public[uid], action))
	case "token":
		switch a.Token.Kind {
		case "full-access":
			return true
		case "read-only":
			return action == "find" || action == "findOne"
		default:
			return containsStr(a.Token.Permissions[uid], action)
		}
	}
	return containsStr(a.Public[uid], action)
}

// reads reports whether the caller may read entries of a type (find or findOne): filtering,
// sorting and populating through a relation needs it on the target. nil is the admin (anything).
// A preview token reads its own document only: related entries (of its own type too, through a
// self-relation) only as the public role may read them.
func (a *hgAuth) reads(uid string) bool {
	if a != nil && a.Kind == "preview" {
		return containsStr(a.Public[uid], "find") || containsStr(a.Public[uid], "findOne")
	}
	return a == nil || a.can(uid, "find") || a.can(uid, "findOne")
}

// relatedDrafts reports whether drafts of related entries of a type may be read (filtered,
// sorted, populated) when the request reads drafts: only tokens allowed to read drafts, and the
// admin (nil). Other callers see related entries in their published version.
func (a *hgAuth) relatedDrafts(uid string) bool {
	return a == nil || (a.Kind == "token" && a.drafts(uid))
}

// previewLocale is the locale a read of type sc uses: the requested one, except that a preview
// token reads its own locale only (ok is false when another one is asked for).
func (a *hgAuth) previewLocale(sc *hgSchema, requested string) (string, bool) {
	if a.Kind != "preview" || !sc.Localized || a.PrevLoc == "" {
		return requested, true
	}
	if requested != "" && requested != a.PrevLoc {
		return "", false
	}
	return a.PrevLoc, true
}

// drafts reports whether status=draft may be read.
func (a *hgAuth) drafts(uid string) bool {
	switch a.Kind {
	case "preview":
		return uid == a.PrevUID
	case "token":
		return a.Token.Kind == "full-access" || a.Token.Drafts
	}
	return false
}

// hgAuthenticate reads the bearer token (an API token or a preview token).
func (s *Server) hgAuthenticate(c *gin.Context, st hgSettings) (*hgAuth, bool) {
	a := &hgAuth{Kind: "public", Public: st.Public}
	if strings.HasPrefix(strings.Trim(c.Param("rest"), "/"), "auth/") {
		return a, true // signing in: a stale token in the header must not stop it
	}
	tok := ""
	if h := c.GetHeader("Authorization"); h != "" {
		scheme, val, _ := strings.Cut(h, " ")
		if !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(val) == "" {
			hgErr(c, http.StatusUnauthorized, "UnauthorizedError", "Missing or invalid credentials", nil)
			return nil, false
		}
		tok = strings.TrimSpace(val)
	}
	previewOnly := false // API tokens only travel in the Authorization header, never in a URL
	if tok == "" {
		tok, previewOnly = c.GetHeader("X-Preview-Token"), true
	}
	if tok == "" {
		tok = c.Query("previewToken")
	}
	if tok == "" {
		return a, true
	}
	if !previewOnly && !strings.HasPrefix(tok, hgPreviewPrefix) && strings.Count(tok, ".") == 2 {
		// Not an API token: a user token (JWT).
		u := s.hgUserFromToken(c, st, tok)
		if u == nil {
			hgErr(c, http.StatusUnauthorized, "UnauthorizedError", "Missing or invalid credentials", nil)
			return nil, false
		}
		a.Kind, a.UserID, a.Authenticated, a.OwnerOnly = hgUserKind, u.ID, st.Authenticated, st.OwnerOnly
		return a, true
	}
	if previewOnly && !strings.HasPrefix(tok, hgPreviewPrefix) {
		hgErr(c, http.StatusUnauthorized, "UnauthorizedError", "Missing or invalid credentials", nil)
		return nil, false
	}
	if strings.HasPrefix(tok, hgPreviewPrefix) {
		uid, doc, loc, ok := s.hgCheckPreviewToken(st, tok)
		if !ok {
			hgErr(c, http.StatusUnauthorized, "UnauthorizedError", "Missing or invalid credentials", nil)
			return nil, false
		}
		a.Kind, a.PrevUID, a.PrevDoc, a.PrevLoc = "preview", uid, doc, loc
		return a, true
	}
	t := s.hgTokenByValue(c, tok)
	if t == nil {
		hgErr(c, http.StatusUnauthorized, "UnauthorizedError", "Missing or invalid credentials", nil)
		return nil, false
	}
	a.Kind, a.Token = "token", t
	return a, true
}

// Dispatch -------------------------------------------------------------------------------------------

func (s *Server) hgAPI(c *gin.Context) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-cache")
	st := s.hgLoad()
	segs := strings.Split(strings.Trim(c.Param("rest"), "/"), "/")
	if len(segs) == 1 && segs[0] == "" {
		segs = nil
	}
	method := c.Request.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodOptions {
		c.Header("Allow", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Status(http.StatusNoContent)
		return
	}
	// Filters and _q go into SQL text parameters: PostgreSQL refuses NUL and invalid UTF-8 (a 500), so
	// such a query string is the client's mistake.
	if q, err := url.QueryUnescape(c.Request.URL.RawQuery); err == nil && (!utf8.ValidString(q) || strings.IndexByte(q, 0) >= 0) {
		hgErr(c, http.StatusBadRequest, "ValidationError", "The query string must be valid UTF-8 without NUL characters", nil)
		return
	}
	a, ok := s.hgAuthenticate(c, st)
	if !ok {
		return
	}
	if !s.hgRateAllow(c, a, st) {
		return
	}
	switch {
	case a.Kind == "preview":
		c.Set(hgCacheKey, "no-store")
	case a.Kind == "public" && st.CacheMaxAge > 0:
		c.Set(hgCacheKey, "public, max-age=0, s-maxage="+strconv.Itoa(st.CacheMaxAge))
		// Shared caches (Varnish, Fastly, Cloudflare) may keep the answer: tag it so Pursae Cache's purge
		// forwarding (wpgocache_proxy.go) drops it when content changes (hgPurge).
		prefix := "wpgo_"
		if pc := s.currentCache(); pc != nil {
			prefix = pc.Config().TagPrefix
		}
		c.Set(hgTagsKey, prefix+hgCacheTag)
	}
	if len(segs) == 0 {
		s.hgIndex(c, a)
		return
	}
	switch segs[0] {
	case "graphql":
		if !st.GraphQL {
			hgNotFound(c)
			return
		}
		s.hgGraphQL(c, a, st)
		return
	case "openapi.json", "typescript":
		if method != http.MethodGet || len(segs) != 1 {
			hgErr(c, http.StatusMethodNotAllowed, "MethodNotAllowedError", "Method Not Allowed", nil)
			return
		}
		s.hgOpenAPIEndpoint(c, a, segs[0] == "typescript")
		return
	case "auth":
		s.hgAuthAPI(c, a, st, segs[1:], method)
		return
	case "users":
		if len(segs) == 2 && segs[1] == "me" && method == http.MethodGet && a.Kind == hgUserKind {
			u, err := s.store.UserByID(c, a.UserID)
			if err != nil {
				hgNotFound(c)
				return
			}
			hgWriteJSON(c, http.StatusOK, hgUserJSON(u))
			return
		}
		hgNotFound(c)
		return
	case "wp":
		s.hgWP(c, a, segs[1:], method)
		return
	case "upload":
		s.hgUploadAPI(c, a, segs[1:], method)
		return
	}
	sc := s.hgReg(c).ByRoute(segs[0])
	if sc == nil {
		hgNotFound(c)
		return
	}
	if sc.IsSingle() {
		switch {
		case len(segs) == 1 && method == http.MethodGet:
			s.hgFindSingle(c, a, sc, st)
		case len(segs) == 1 && (method == http.MethodPut || method == http.MethodPatch || method == http.MethodPost):
			s.hgWrite(c, a, sc, "", "update")
		case len(segs) == 1 && method == http.MethodDelete:
			s.hgDelete(c, a, sc, "")
		case len(segs) == 2 && method == http.MethodPost && (segs[1] == "publish" || segs[1] == "unpublish"):
			s.hgPublishAPI(c, a, sc, "", segs[1])
		case len(segs) == 2 && method == http.MethodPost && segs[1] == "schedule":
			s.hgScheduleAPI(c, a, sc, "")
		default:
			hgErr(c, http.StatusMethodNotAllowed, "MethodNotAllowedError", "Method Not Allowed", nil)
		}
		return
	}
	switch {
	case len(segs) == 1 && method == http.MethodGet:
		s.hgFindMany(c, a, sc, st)
	case len(segs) == 1 && method == http.MethodPost:
		s.hgWrite(c, a, sc, "", "create")
	case len(segs) == 2 && method == http.MethodGet:
		s.hgFindOne(c, a, sc, segs[1], st)
	case len(segs) == 2 && (method == http.MethodPut || method == http.MethodPatch):
		s.hgWrite(c, a, sc, segs[1], "update")
	case len(segs) == 2 && method == http.MethodDelete:
		s.hgDelete(c, a, sc, segs[1])
	case len(segs) == 3 && method == http.MethodPost && (segs[2] == "publish" || segs[2] == "unpublish"):
		s.hgPublishAPI(c, a, sc, segs[1], segs[2])
	case len(segs) == 3 && method == http.MethodPost && segs[2] == "schedule":
		s.hgScheduleAPI(c, a, sc, segs[1])
	default:
		hgErr(c, http.StatusMethodNotAllowed, "MethodNotAllowedError", "Method Not Allowed", nil)
	}
}

// hgIndex lists the routes the caller may read.
func (s *Server) hgIndex(c *gin.Context, a *hgAuth) {
	var out []map[string]any
	for _, sc := range s.hgReg(c).Types() {
		if !a.can(sc.UID, "find") && !a.can(sc.UID, "findOne") {
			continue
		}
		route := "/" + hgNamespace + "/" + sc.PluralName
		if sc.IsSingle() {
			route = "/" + hgNamespace + "/" + sc.SingularName
		}
		out = append(out, map[string]any{"uid": sc.UID, "kind": sc.Kind, "displayName": sc.DisplayName, "route": route})
	}
	if out == nil {
		out = []map[string]any{}
	}
	hgWriteJSON(c, http.StatusOK, map[string]any{"data": out, "meta": map[string]any{"namespace": hgNamespace}})
}

// hgReadParams are the parsed read parameters of a request.
type hgReadParams struct {
	Auth     *hgAuth // who reads: relation filters and sorting check its permissions (nil: the admin)
	Status   string
	Locale   string // the locale column ("" without i18n)
	Filters  map[string]any
	Sort     []hgSortKey
	Page     hgPagination
	Populate *hgPop
	Fields   []string
	Budget   *hgReadBudget // what the request may still spend on queries (nil: no limit)
}

// The read budget bounds what one request (a REST read, a GraphQL operation, one subscription
// event) may cost: every find, inverse-relation lookup and populated entry counts, so nested
// populate through cyclic relations or thousands of GraphQL aliases end with a 400 instead of
// running millions of queries.
const (
	hgReadMaxQueries = 1000
	hgReadMaxObjects = 20000
)

type hgReadBudget struct{ queries, objs int }

func newHGReadBudget() *hgReadBudget { return &hgReadBudget{} }

var errHGTooExpensive = hgBad("The request is too expensive: it would run more than %d queries or load more than %d related entries. Populate fewer levels or request fewer entries.", hgReadMaxQueries, hgReadMaxObjects)

// query spends one query.
func (b *hgReadBudget) query() error {
	if b == nil {
		return nil
	}
	if b.queries++; b.queries > hgReadMaxQueries {
		return errHGTooExpensive
	}
	return nil
}

// objects spends n loaded entries.
func (b *hgReadBudget) objects(n int) error {
	if b == nil {
		return nil
	}
	if b.objs += n; b.objs > hgReadMaxObjects {
		return errHGTooExpensive
	}
	return nil
}

func (s *Server) hgParseRead(c *gin.Context, a *hgAuth, sc *hgSchema, st hgSettings) (*hgReadParams, bool) {
	qs := hgParseQS(c.Request.URL.Query())
	p := &hgReadParams{Auth: a, Status: "published", Budget: newHGReadBudget()}
	if v, _ := qs["status"].(string); v != "" {
		if v != "draft" && v != "published" {
			hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid status: use draft or published", nil)
			return nil, false
		}
		p.Status = v
	}
	if !sc.DraftAndPublish {
		p.Status = "published"
	}
	if p.Status == "draft" && !a.drafts(sc.UID) {
		hgErr(c, http.StatusForbidden, "ForbiddenError", "Reading drafts needs a token allowed to read them", nil)
		return nil, false
	}
	loc, _ := qs["locale"].(string)
	loc, ok := a.previewLocale(sc, loc)
	if !ok {
		hgForbidden(c, a)
		return nil, false
	}
	l, err := s.hgEntryLocale(c, sc, loc)
	if err != nil {
		hgErr(c, http.StatusBadRequest, "ValidationError", err.Error(), nil)
		return nil, false
	}
	p.Locale = l
	if f, ok := qs["filters"].(map[string]any); ok {
		p.Filters = f
	} else if _, ok := qs["filters"]; ok {
		hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid filters", nil)
		return nil, false
	}
	if _, ok := qs["_q"]; ok {
		q, isStr := qs["_q"].(string)
		if !isStr {
			hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid _q: use one search string", nil)
			return nil, false
		}
		if sf := hgSearchFilter(sc, q); sf != nil {
			if len(p.Filters) > 0 {
				p.Filters = map[string]any{"$and": []any{p.Filters, sf}}
			} else {
				p.Filters = sf
			}
		}
	}
	if p.Sort, err = hgParseSort(qs["sort"]); err != nil {
		hgFail(c, err)
		return nil, false
	}
	if p.Page, err = hgParsePagination(qs["pagination"], st.DefaultPageSize, st.MaxPageSize); err != nil {
		hgFail(c, err)
		return nil, false
	}
	if p.Populate, err = hgParsePopulate(qs["populate"]); err != nil {
		hgFail(c, err)
		return nil, false
	}
	p.Fields = hgStrings(qs["fields"])
	for _, f := range p.Fields {
		if _, sys := hgSystemFields[f]; sys {
			continue
		}
		fd := sc.Field(f)
		if fd == nil || fd.Private || !fd.scalar() {
			hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid key "+f+" in fields", nil)
			return nil, false
		}
	}
	return p, true
}

// hgFindOpts adds the restrictions of a find that are not query parameters.
type hgFindOpts struct {
	Doc   string // only this document (preview tokens)
	Extra string // raw SQL condition on e (admin status filters)
	NoLim bool
}

// hgFind runs a find: the entries of a type in one locale and status matching the filters.
func (s *Server) hgFind(ctx context.Context, sc *hgSchema, p *hgReadParams, o hgFindOpts) ([]*hgEntry, int, error) {
	if err := p.Budget.query(); err != nil {
		return nil, 0, err
	}
	reg := s.hgReg(ctx)
	q := &hgQuery{reg: reg, sql: &hgSQL{}, status: p.Status, locale: p.Locale, a: p.Auth}
	if p.Locale == "" {
		q.locale = s.hgDefaultLocale(ctx)
	}
	where := "e.type_uid = " + q.sql.arg(sc.UID) + " AND e.locale = " + q.sql.arg(p.Locale)
	if p.Status != "draft" {
		where += " AND e.published_data IS NOT NULL"
	}
	if o.Doc != "" {
		where += " AND e.document_id = " + q.sql.arg(o.Doc)
	}
	if o.Extra != "" {
		where += " AND (" + o.Extra + ")"
	}
	if len(p.Filters) > 0 {
		f, err := q.filters(sc, p.Filters, q.versionCol("e"), "e")
		if err != nil {
			return nil, 0, err
		}
		where += " AND " + f
	}
	whereArgs := len(q.sql.args)
	order, err := q.orderBy(sc, p.Sort, "e") // may add arguments (sorting through relations)
	if err != nil {
		return nil, 0, err
	}
	total := 0
	if p.Page.WithCount {
		if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM hg_entries e WHERE `+where, q.sql.args[:whereArgs]...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	sqlq := `SELECT ` + hgPrefixCols("e") + ` FROM hg_entries e WHERE ` + where + ` ORDER BY ` + order
	if !o.NoLim {
		limit, offset := p.Page.limitOffset()
		sqlq += " LIMIT " + strconv.Itoa(limit) + " OFFSET " + strconv.Itoa(offset)
	}
	rows, err := s.store.Pool.Query(ctx, sqlq, q.sql.args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*hgEntry
	for rows.Next() {
		e, err := hgScanEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func hgPrefixCols(alias string) string {
	cols := strings.Split(hgEntryCols, ", ")
	for i := range cols {
		cols[i] = alias + "." + cols[i]
	}
	return strings.Join(cols, ", ")
}

func (s *Server) hgFindMany(c *gin.Context, a *hgAuth, sc *hgSchema, st hgSettings) {
	if !a.can(sc.UID, "find") || (a.Kind == "preview" && a.PrevUID != sc.UID) {
		hgForbidden(c, a)
		return
	}
	p, ok := s.hgParseRead(c, a, sc, st)
	if !ok {
		return
	}
	o := hgFindOpts{}
	if a.Kind == "preview" {
		o.Doc = a.PrevDoc
	}
	list, total, err := s.hgFind(c, sc, p, o)
	if err != nil {
		hgFail(c, err)
		return
	}
	ser := s.hgSerializer(c, a, p)
	ser.prefetch(sc, list, p.Populate)
	data := []any{}
	for _, e := range list {
		obj, err := ser.entry(sc, e, p.Populate, p.Fields)
		if err != nil {
			hgFail(c, err)
			return
		}
		data = append(data, obj)
	}
	hgWriteJSON(c, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{"pagination": p.Page.meta(total)}})
}

// hgOneEntry loads one document version in the requested status.
func (s *Server) hgOneEntry(c *gin.Context, sc *hgSchema, doc string, p *hgReadParams) *hgEntry {
	e, err := s.hgGetEntry(c, sc.UID, doc, p.Locale)
	if err != nil || e.Version(p.Status) == nil {
		return nil
	}
	return e
}

func (s *Server) hgFindOne(c *gin.Context, a *hgAuth, sc *hgSchema, doc string, st hgSettings) {
	if !a.can(sc.UID, "findOne") || (a.Kind == "preview" && (a.PrevUID != sc.UID || a.PrevDoc != doc)) {
		hgForbidden(c, a)
		return
	}
	p, ok := s.hgParseRead(c, a, sc, st)
	if !ok {
		return
	}
	s.hgRespondOne(c, a, sc, doc, p)
}

func (s *Server) hgRespondOne(c *gin.Context, a *hgAuth, sc *hgSchema, doc string, p *hgReadParams) {
	e := s.hgOneEntry(c, sc, doc, p)
	if e == nil {
		hgNotFound(c)
		return
	}
	if len(p.Filters) > 0 { // filters on a findOne narrow it, as on a find
		list, _, err := s.hgFind(c, sc, p, hgFindOpts{Doc: doc})
		if err != nil {
			hgFail(c, err)
			return
		}
		if len(list) == 0 {
			hgNotFound(c)
			return
		}
	}
	obj, err := s.hgSerializer(c, a, p).entry(sc, e, p.Populate, p.Fields)
	if err != nil {
		hgFail(c, err)
		return
	}
	hgWriteJSON(c, http.StatusOK, map[string]any{"data": obj, "meta": map[string]any{}})
}

func (s *Server) hgFindSingle(c *gin.Context, a *hgAuth, sc *hgSchema, st hgSettings) {
	if !a.can(sc.UID, "find") || (a.Kind == "preview" && a.PrevUID != sc.UID) {
		hgForbidden(c, a)
		return
	}
	p, ok := s.hgParseRead(c, a, sc, st)
	if !ok {
		return
	}
	doc := s.hgSingleDoc(c, sc.UID)
	if doc == "" {
		hgNotFound(c)
		return
	}
	s.hgRespondOne(c, a, sc, doc, p)
}

// Writes ---------------------------------------------------------------------------------------------

// hgBody reads {"data": {...}} of a write.
func hgBody(c *gin.Context) (map[string]any, bool) {
	if !strings.HasPrefix(c.ContentType(), "application/json") {
		hgErr(c, http.StatusBadRequest, "ValidationError", "The body must be JSON: {\"data\": {...}}", nil)
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 16<<20))
	if err != nil {
		hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid body", nil)
		return nil, false
	}
	var body map[string]any
	if !utf8.Valid(raw) || restJSONHasNUL(raw) || json.Unmarshal(raw, &body) != nil { // text columns hold neither
		hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid JSON body", nil)
		return nil, false
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		hgErr(c, http.StatusBadRequest, "ValidationError", "Missing \"data\" payload in the request body", nil)
		return nil, false
	}
	return data, true
}

// hgWrite creates or updates a document. ?status=published also publishes (a type without draft &
// publish always publishes); ?locale= writes that locale's version.
func (s *Server) hgWrite(c *gin.Context, a *hgAuth, sc *hgSchema, doc, action string) {
	if !a.can(sc.UID, action) || a.Kind == "preview" || (action == "update" && !s.hgOwnerAllows(c, a, sc, doc, "update")) {
		hgForbidden(c, a)
		return
	}
	data, ok := hgBody(c)
	if !ok {
		return
	}
	loc, err := s.hgEntryLocale(c, sc, c.Query("locale"))
	if err != nil {
		hgErr(c, http.StatusBadRequest, "ValidationError", err.Error(), nil)
		return
	}
	if sc.IsSingle() {
		doc = s.hgSingleDoc(c, sc.UID)
	}
	status := c.DefaultQuery("status", "draft")
	if status != "draft" && status != "published" {
		hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid status: use draft or published", nil)
		return
	}
	// Publishing is what POST /{documentId}/publish does, and that needs update: create alone may only
	// store a draft of a type with draft & publish.
	if status == "published" && sc.DraftAndPublish && action == "create" && !a.can(sc.UID, "update") {
		hgForbidden(c, a)
		return
	}
	e, events, err := s.hgSaveEntry(c, sc, doc, data, hgSaveOpts{Locale: loc, Publish: status == "published", Auth: a, UserID: a.userID()})
	if err != nil {
		hgFail(c, err)
		return
	}
	for _, ev := range events {
		s.hgEmit(c, ev, sc, e)
	}
	p := &hgReadParams{Auth: a, Status: "draft", Locale: loc, Populate: &hgPop{Populate: map[string]*hgPop{}}, Budget: newHGReadBudget()}
	if !sc.DraftAndPublish || status == "published" {
		p.Status = "published"
	}
	if qs := hgParseQS(c.Request.URL.Query()); qs["populate"] != nil {
		if pp, err := hgParsePopulate(qs["populate"]); err == nil {
			p.Populate = pp
		}
	}
	var fields []string
	if doc != "" && p.Status == "draft" {
		fields, p.Populate = hgWriteView(a, sc, data, p.Populate)
	}
	obj, err := s.hgSerializer(c, a, p).entry(sc, e, p.Populate, fields)
	if err != nil {
		hgFail(c, err)
		return
	}
	code := http.StatusOK
	if action == "create" || (sc.IsSingle() && doc == "") {
		code = http.StatusCreated
	}
	hgWriteJSON(c, code, map[string]any{"data": obj, "meta": map[string]any{}})
}

// hgWriteView narrows the answer of a write that returns an existing document's draft for a caller
// that may not read drafts: the draft may hold someone else's unpublished changes, so only the
// attributes the request wrote (and the entry's metadata) are shown. It returns the fields and the
// populate to serialize with (nil fields: everything). Only written scalar attributes come back: a
// written component or dynamic zone is merged over the stored draft (keys the request left out keep
// their draft values) and a relation written with connect/disconnect keeps the draft's other ids, so
// echoing them would show the unpublished changes this view hides.
func hgWriteView(a *hgAuth, sc *hgSchema, written map[string]any, pop *hgPop) ([]string, *hgPop) {
	if a == nil || !sc.DraftAndPublish || a.drafts(sc.UID) {
		return nil, pop
	}
	fields := []string{"createdAt", "updatedAt", "publishedAt", "locale"} // never empty: empty means all
	for name := range written {
		if f := sc.Field(name); f != nil && f.scalar() {
			fields = append(fields, name)
		}
	}
	return fields, &hgPop{Populate: map[string]*hgPop{}}
}

// hgDelete deletes a document: the given ?locale= version, or every locale (locale=* or none).
func (s *Server) hgDelete(c *gin.Context, a *hgAuth, sc *hgSchema, doc string) {
	if !a.can(sc.UID, "delete") || a.Kind == "preview" || !s.hgOwnerAllows(c, a, sc, doc, "delete") {
		hgForbidden(c, a)
		return
	}
	if sc.IsSingle() {
		doc = s.hgSingleDoc(c, sc.UID)
		if doc == "" {
			hgNotFound(c)
			return
		}
	}
	loc := "*"
	if l := c.Query("locale"); l != "" && l != "*" && sc.Localized {
		loc = l
	} else if !sc.Localized {
		loc = ""
	}
	deleted, err := s.hgDeleteEntries(c, sc, doc, loc, 0)
	if err != nil {
		hgFail(c, err)
		return
	}
	for _, e := range deleted {
		s.hgEmit(c, "entry.delete", sc, e)
	}
	c.Status(http.StatusNoContent)
}

// hgPublishAPI is POST /{plural}/{documentId}/publish|unpublish (needs update).
func (s *Server) hgPublishAPI(c *gin.Context, a *hgAuth, sc *hgSchema, doc, action string) {
	if !a.can(sc.UID, "update") || a.Kind == "preview" || !s.hgOwnerAllows(c, a, sc, doc, "update") {
		hgForbidden(c, a)
		return
	}
	if sc.IsSingle() {
		doc = s.hgSingleDoc(c, sc.UID)
	}
	loc, err := s.hgEntryLocale(c, sc, c.Query("locale"))
	if err != nil {
		hgErr(c, http.StatusBadRequest, "ValidationError", err.Error(), nil)
		return
	}
	var e *hgEntry
	status := "published"
	if action == "publish" {
		e, err = s.hgPublishEntry(c, sc, doc, loc, a.userID())
	} else {
		e, err = s.hgUnpublishEntry(c, sc, doc, loc)
		status = "draft"
	}
	if err != nil {
		if errors.Is(err, errHGNotFound) {
			hgNotFound(c)
			return
		}
		var ve hgValidationError
		if errors.As(err, &ve) {
			hgFail(c, err)
			return
		}
		hgErr(c, http.StatusBadRequest, "ValidationError", err.Error(), nil)
		return
	}
	s.hgEmit(c, "entry."+action, sc, e)
	p := &hgReadParams{Status: status, Locale: loc, Populate: &hgPop{Populate: map[string]*hgPop{}}}
	var fields []string
	if status == "draft" {
		fields, p.Populate = hgWriteView(a, sc, nil, p.Populate)
	}
	obj, err := s.hgSerializer(c, a, p).entry(sc, e, p.Populate, fields)
	if err != nil {
		hgFail(c, err)
		return
	}
	hgWriteJSON(c, http.StatusOK, map[string]any{"data": obj, "meta": map[string]any{}})
}

// hgScheduleAPI is POST /{plural}/{documentId}/schedule (needs update) with {"data": {"publishAt":
// "<RFC 3339>"|null, "unpublishAt": …}}: when the scheduler publishes and unpublishes the
// ?locale= version. null (or leaving a key out) clears it.
func (s *Server) hgScheduleAPI(c *gin.Context, a *hgAuth, sc *hgSchema, doc string) {
	if !a.can(sc.UID, "update") || a.Kind == "preview" || !s.hgOwnerAllows(c, a, sc, doc, "update") {
		hgForbidden(c, a)
		return
	}
	data, ok := hgBody(c)
	if !ok {
		return
	}
	if sc.IsSingle() {
		doc = s.hgSingleDoc(c, sc.UID)
	}
	loc, err := s.hgEntryLocale(c, sc, c.Query("locale"))
	if err != nil {
		hgErr(c, http.StatusBadRequest, "ValidationError", err.Error(), nil)
		return
	}
	var at [2]*time.Time
	for i, k := range []string{"publishAt", "unpublishAt"} {
		v, _ := data[k].(string)
		if v == "" {
			continue
		}
		t, ok := hgParseTime(v)
		if !ok {
			hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid date for "+k, nil)
			return
		}
		at[i] = &t
	}
	e, err := s.hgSetSchedule(c, sc, doc, loc, at[0], at[1])
	if err != nil {
		if errors.Is(err, errHGNotFound) {
			hgNotFound(c)
			return
		}
		hgErr(c, http.StatusBadRequest, "ValidationError", err.Error(), nil)
		return
	}
	out := map[string]any{"documentId": e.DocumentID, "publishAt": nil, "unpublishAt": nil}
	if sc.Localized {
		out["locale"] = e.Locale
	}
	if e.PublishAt != nil {
		out["publishAt"] = hgTime(*e.PublishAt)
	}
	if e.UnpublishAt != nil {
		out["unpublishAt"] = hgTime(*e.UnpublishAt)
	}
	hgWriteJSON(c, http.StatusOK, map[string]any{"data": out, "meta": map[string]any{}})
}

// Serialization --------------------------------------------------------------------------------------

type hgSer struct {
	s     *Server
	c     *gin.Context
	reg   *hgRegistry
	a     *hgAuth
	p     *hgReadParams
	media map[int64]*store.Attachment
	cur   *hgEntry // the entry being serialized (inverse relations are looked up from it)
	cache *hgSerCache
}

// hgSerCache is the request-lived loader state shared by a serializer and its sub-serializers:
// related entries (by target, status, locale and document), posts and users are read once per
// request instead of once per entry and relation.
type hgSerCache struct {
	ents  map[string]*hgEntry // nil value: looked up, not visible
	posts map[int64]*store.Post
	users map[int64]*store.User
	terms map[int64]*store.Term
	// The caller's WordPress user (hgWPUser), looked up once.
	caller     *store.User
	callerDone bool
}

// ctx is the serializer's context (webhook payloads have no request).
func (h *hgSer) ctx() context.Context {
	if h.c == nil {
		return context.Background()
	}
	return h.c
}

// wpUser is the caller's WordPress user: a signed-in front-end user, nil otherwise.
func (h *hgSer) wpUser() *store.User {
	ca := h.ca()
	if !ca.callerDone {
		ca.callerDone = true
		ca.caller = h.s.hgWPUser(h.ctx(), h.a)
	}
	return ca.caller
}

// hgWPUser is the WordPress user behind a caller: a signed-in front-end user; nil for API tokens,
// preview tokens and the public role.
func (s *Server) hgWPUser(ctx context.Context, a *hgAuth) *store.User {
	if a == nil || a.Kind != hgUserKind || a.UserID <= 0 {
		return nil
	}
	u, err := s.store.UserByID(ctx, a.UserID)
	if err != nil {
		return nil
	}
	return u
}

// hgMediaReadable reports whether a caller may read (and link) an attachment, as core
// /wp/v2/media decides it (mediaReadable): the admin (a == nil), full-access tokens and users
// who browse the whole library read every file; an upload token also reads its own uploads;
// everyone else reads unattached files and files of a published post without a password.
func (s *Server) hgMediaReadable(ctx context.Context, a *hgAuth, u *store.User, att *store.Attachment) bool {
	if att == nil {
		return false
	}
	if a == nil || a.fullAccess() || a.ownUpload(att) || mediaLibraryReader(u) {
		return true
	}
	return s.attachmentPublicFor(ctx, u, att)
}

func (h *hgSer) ca() *hgSerCache {
	if h.cache == nil {
		h.cache = &hgSerCache{ents: map[string]*hgEntry{}, posts: map[int64]*store.Post{}, users: map[int64]*store.User{}, terms: map[int64]*store.Term{}}
	}
	return h.cache
}

func (s *Server) hgSerializer(c *gin.Context, a *hgAuth, p *hgReadParams) *hgSer {
	return &hgSer{s: s, c: c, reg: s.hgReg(c), a: a, p: p, media: map[int64]*store.Attachment{}}
}

// entry serializes one entry in the request's status.
func (h *hgSer) entry(sc *hgSchema, e *hgEntry, pop *hgPop, fields []string) (map[string]any, error) {
	data := e.Version(h.p.Status)
	if data == nil {
		data = e.Data
	}
	saved := h.cur
	h.cur = e
	out, err := h.object(sc, data, pop, fields, 0)
	h.cur = saved
	if err != nil {
		return nil, err
	}
	out["id"] = e.ID
	out["documentId"] = e.DocumentID
	if hgWants(fields, "createdAt") {
		out["createdAt"] = hgTime(e.CreatedAt)
	}
	if hgWants(fields, "updatedAt") {
		out["updatedAt"] = hgTime(e.UpdatedAt)
	}
	if hgWants(fields, "publishedAt") {
		if e.PublishedAt != nil && (h.p.Status != "draft" || !sc.DraftAndPublish) {
			out["publishedAt"] = hgTime(*e.PublishedAt)
		} else {
			out["publishedAt"] = nil
		}
	}
	if sc.Localized && hgWants(fields, "locale") {
		out["locale"] = e.Locale
	}
	if pop != nil && sc.Localized {
		if _, ok := pop.Populate["localizations"]; ok {
			out["localizations"] = h.localizations(sc, e)
		}
	}
	return out, nil
}

func hgWants(fields []string, name string) bool { return len(fields) == 0 || containsStr(fields, name) }

func hgTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (h *hgSer) localizations(sc *hgSchema, e *hgEntry) []any {
	out := []any{}
	versions := h.s.hgDocLocales(h.c, sc.UID, e.DocumentID)
	var locs []string
	for l := range versions {
		locs = append(locs, l)
	}
	sort.Strings(locs)
	status := h.relStatus(sc.UID) // other locales' drafts are other versions: the same rule as related entries
	sub := h
	if status != h.p.Status {
		p := *h.p
		p.Status = status
		sub = &hgSer{s: h.s, c: h.c, reg: h.reg, a: h.a, p: &p, media: h.media, cache: h.ca()}
	}
	for _, l := range locs {
		v := versions[l]
		if l == e.Locale || v.Version(status) == nil {
			continue
		}
		obj, err := sub.entry(sc, v, nil, nil)
		if err == nil {
			out = append(out, obj)
		}
	}
	return out
}

// object serializes the attributes of an entry or a component value.
func (h *hgSer) object(sc *hgSchema, data map[string]any, pop *hgPop, fields []string, depth int) (map[string]any, error) {
	out := map[string]any{}
	if pop == nil {
		pop = &hgPop{Populate: map[string]*hgPop{}}
	}
	for name := range pop.Populate {
		if name == "localizations" && sc.Localized && depth == 0 {
			continue
		}
		fname := name
		if a, ok := pop.Alias[name]; ok {
			fname = a
		}
		f := sc.Field(fname)
		if f == nil || f.scalar() || f.Private {
			return nil, hgBad("Invalid key %s in populate", fname)
		}
	}
	// fill serializes one relation, media, component or dynamic zone attribute under key.
	fill := func(f *hgField, key string, sub *hgPop) error {
		if depth > 8 {
			return hgBad("Populate is nested too deeply")
		}
		v := data[f.Name]
		if f.MappedBy != "" {
			if h.cur == nil {
				return nil
			}
			if err := h.p.Budget.query(); err != nil {
				return err
			}
			v = hgAnyStrings(h.s.hgInverseIDs(h.c, f, h.cur.DocumentID, h.cur.Locale, h.relStatus(f.Target)))
		}
		val, keep, err := h.populated(sc, f, v, sub, depth)
		if err != nil {
			return err
		}
		if keep {
			out[key] = val
		}
		return nil
	}
	for i := range sc.Fields {
		f := &sc.Fields[i]
		if f.Private || f.Type == "password" {
			continue
		}
		if f.scalar() {
			if !hgWants(fields, f.Name) {
				continue
			}
			out[f.Name] = hgScalarOut(f, data[f.Name])
			continue
		}
		sub, explicit := pop.Populate[f.Name]
		if !explicit && !pop.All {
			continue
		}
		if !explicit {
			sub = &hgPop{Populate: map[string]*hgPop{}} // populate=*: one level
		}
		if err := fill(f, f.Name, sub); err != nil {
			return nil, err
		}
	}
	for key, name := range pop.Alias {
		if sub := pop.Populate[key]; sub != nil {
			if err := fill(sc.Field(name), key, sub); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// hgScalarOut is a scalar value as the API returns it (absent values are null).
func hgScalarOut(f *hgField, v any) any {
	if v == nil {
		return nil
	}
	if f.Type == "number" && f.isInteger() {
		if n, ok := v.(float64); ok {
			return int64(n)
		}
	}
	return v
}

// populated serializes a relation, media, component or dynamic zone; keep is false when the
// caller may not see it.
func (h *hgSer) populated(sc *hgSchema, f *hgField, v any, pop *hgPop, depth int) (any, bool, error) {
	switch f.Type {
	case "component":
		comp := h.reg.Get(f.Component)
		if comp == nil {
			return nil, false, nil
		}
		if f.Repeatable {
			out := []any{}
			for _, item := range hgList(v) {
				if m, ok := item.(map[string]any); ok {
					obj, err := h.component(comp, m, pop, depth)
					if err != nil {
						return nil, false, err
					}
					out = append(out, obj)
				}
			}
			return out, true, nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, true, nil
		}
		obj, err := h.component(comp, m, pop, depth)
		return obj, true, err
	case "dynamiczone":
		out := []any{}
		for _, item := range hgList(v) {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			cu, _ := m["__component"].(string)
			comp := h.reg.Get(cu)
			if comp == nil {
				continue
			}
			sub := &hgPop{Populate: map[string]*hgPop{}}
			if pop.On != nil {
				if on, ok := pop.On[cu]; ok {
					sub = on
				}
			} else {
				sub = pop
			}
			obj, err := h.component(comp, m, sub, depth)
			if err != nil {
				return nil, false, err
			}
			obj["__component"] = cu
			out = append(out, obj)
		}
		return out, true, nil
	case "media":
		var out []any
		for _, x := range hgList(v) {
			if id, ok := hgToInt(x); ok {
				if m := h.mediaObj(id, pop.Fields); m != nil {
					out = append(out, m)
				}
			}
		}
		if f.Multiple {
			if out == nil {
				out = []any{}
			}
			return out, true, nil
		}
		if len(out) == 0 {
			return nil, true, nil
		}
		return out[0], true, nil
	case "relation":
		list, keep, err := h.relation(f, hgList(v), pop, depth)
		if err != nil || !keep {
			return nil, keep, err
		}
		if f.multi() {
			if list == nil {
				list = []any{}
			}
			return list, true, nil
		}
		if len(list) == 0 {
			return nil, true, nil
		}
		return list[0], true, nil
	}
	return nil, false, nil
}

func (h *hgSer) component(comp *hgSchema, m map[string]any, pop *hgPop, depth int) (map[string]any, error) {
	obj, err := h.object(comp, m, pop, pop.Fields, depth+1)
	if err != nil {
		return nil, err
	}
	if id, ok := m["id"].(float64); ok {
		obj["id"] = int64(id)
	}
	return obj, nil
}

// relation loads related entries (in the request's status and locale), posts or users.
func (h *hgSer) relation(f *hgField, ids []any, pop *hgPop, depth int) ([]any, bool, error) {
	ctx := h.c
	switch {
	case hgIsWPPost(f.Target):
		typ, _ := hgWPPostType(f.Target)
		var out []any
		for _, x := range ids {
			id, ok := hgToInt(x)
			if !ok {
				continue
			}
			p, seen := h.ca().posts[id]
			if !seen {
				var err error
				if p, err = h.s.store.GetPost(ctx, id); err != nil {
					p = nil
				}
				h.ca().posts[id] = p
			}
			if p == nil || p.Status != "publish" || p.Type != typ {
				continue
			}
			out = append(out, h.s.hgPostObj(h.c, p, pop.Fields, h.wpUser()))
		}
		return out, true, nil
	case hgIsWPTerm(f.Target):
		tax, _ := hgWPTaxonomy(f.Target)
		var out []any
		for _, x := range ids {
			id, ok := hgToInt(x)
			if !ok {
				continue
			}
			t, seen := h.ca().terms[id]
			if !seen {
				var err error
				if t, err = h.s.store.GetTerm(ctx, id); err != nil {
					t = nil
				}
				h.ca().terms[id] = t
			}
			if t == nil || t.Taxonomy != tax {
				continue
			}
			out = append(out, hgPick(map[string]any{"id": t.ID, "name": t.Name, "slug": t.Slug, "description": t.Description,
				"count": t.Count, "parent": t.ParentID, "taxonomy": t.Taxonomy, "link": h.s.links(h.c).TermLink(t)}, pop.Fields))
		}
		return out, true, nil
	case f.Target == "wp:user":
		var out []any
		for _, x := range ids {
			id, ok := hgToInt(x)
			if !ok {
				continue
			}
			u, seen := h.ca().users[id]
			if !seen {
				var err error
				if u, err = h.s.store.UserByID(ctx, id); err != nil {
					u = nil
				}
				h.ca().users[id] = u
			}
			if u != nil && h.s.hgHidesUsers(h.a) && !h.s.hgUserPublished(ctx, u.ID) {
				continue // no names of accounts the site does not show (blocked user enumeration)
			}
			if u != nil {
				out = append(out, hgPick(map[string]any{"id": u.ID, "username": u.Nicename, "name": u.DisplayName}, pop.Fields))
			}
		}
		return out, true, nil
	}
	target := h.reg.Get(f.Target)
	if target == nil {
		return nil, false, nil
	}
	if !h.a.reads(target.UID) {
		return nil, false, nil // the caller may not read the target type: the attribute is left out
	}
	var docs []string
	for _, x := range ids {
		if d, ok := x.(string); ok {
			docs = append(docs, d)
		}
	}
	if len(docs) == 0 {
		return nil, true, nil
	}
	for _, fn := range pop.Fields {
		if _, sys := hgSystemFields[fn]; !sys {
			if fd := target.Field(fn); fd == nil || fd.Private || !fd.scalar() {
				return nil, false, hgBad("Invalid key %s in fields", fn)
			}
		}
	}
	status := h.relStatus(target.UID)
	if !target.DraftAndPublish {
		status = "published"
	}
	loc := ""
	if target.Localized {
		loc = h.p.Locale
		if loc == "" {
			loc = h.s.hgDefaultLocale(ctx)
		}
	}
	rp := &hgReadParams{Auth: h.a, Status: status, Locale: loc, Page: hgPagination{Page: 1, PageSize: 1000}, Budget: h.p.Budget}
	var list []*hgEntry
	if len(pop.Filters) == 0 && len(pop.Sort) == 0 {
		// Plain relation: documents already loaded in this request come from the cache, the rest in one query.
		if err := h.loadRel(target, docs, rp); err != nil {
			return nil, false, err
		}
		for _, d := range docs {
			if e := h.ca().ents[hgRelCacheKey(target.UID, status, loc, d)]; e != nil {
				list = append(list, e)
			}
		}
	} else {
		filters := map[string]any{"documentId": map[string]any{"$in": hgAnyStrings(docs)}}
		if len(pop.Filters) > 0 {
			filters = map[string]any{"$and": []any{filters, pop.Filters}}
		}
		rp.Filters, rp.Sort = filters, pop.Sort
		var err error
		list, _, err = h.s.hgFind(ctx, target, rp, hgFindOpts{NoLim: true})
		if err != nil {
			return nil, false, err
		}
		if len(pop.Sort) == 0 { // keep the stored order
			pos := map[string]int{}
			for i, d := range docs {
				pos[d] = i
			}
			sort.SliceStable(list, func(i, j int) bool { return pos[list[i].DocumentID] < pos[list[j].DocumentID] })
		}
	}
	sub := &hgSer{s: h.s, c: h.c, reg: h.reg, a: h.a, p: rp, media: h.media, cache: h.ca()}
	if err := h.p.Budget.objects(len(list)); err != nil {
		return nil, false, err
	}
	sub.prefetch(target, list, pop)
	var out []any
	for _, e := range list {
		obj, err := sub.entryAt(target, e, pop, depth+1)
		if err != nil {
			return nil, false, err
		}
		out = append(out, obj)
	}
	return out, true, nil
}

func hgRelCacheKey(uid, status, loc, doc string) string {
	return uid + "|" + status + "|" + loc + "|" + doc
}

// loadRel reads the not yet cached documents of target in one query (rp carries status, locale, budget).
func (h *hgSer) loadRel(target *hgSchema, docs []string, rp *hgReadParams) error {
	var need []string
	seen := map[string]bool{}
	for _, d := range docs {
		if seen[d] {
			continue
		}
		seen[d] = true
		if _, ok := h.ca().ents[hgRelCacheKey(target.UID, rp.Status, rp.Locale, d)]; !ok {
			need = append(need, d)
		}
	}
	if len(need) == 0 {
		return nil
	}
	q := *rp
	q.Filters = map[string]any{"documentId": map[string]any{"$in": hgAnyStrings(need)}}
	list, _, err := h.s.hgFind(h.c, target, &q, hgFindOpts{NoLim: true})
	if err != nil {
		return err
	}
	for _, d := range need {
		h.ca().ents[hgRelCacheKey(target.UID, rp.Status, rp.Locale, d)] = nil
	}
	for _, e := range list {
		h.ca().ents[hgRelCacheKey(target.UID, rp.Status, rp.Locale, e.DocumentID)] = e
	}
	return nil
}

// prefetch loads, for all entries of a list at once, the plain relations that pop populates, so
// serializing the entries one by one finds them in the cache (one query per relation field
// instead of one per entry and field). Anything it cannot prove safe is left to relation().
func (h *hgSer) prefetch(sc *hgSchema, list []*hgEntry, pop *hgPop) {
	if len(list) < 2 || pop == nil {
		return
	}
	for key, sub := range pop.Populate {
		name := key
		if n, ok := pop.Alias[key]; ok {
			name = n
		}
		f := sc.Field(name)
		if f == nil || f.Type != "relation" || sub == nil || len(sub.Filters) > 0 || len(sub.Sort) > 0 {
			continue
		}
		target := h.reg.Get(f.Target)
		if target == nil || !h.a.reads(target.UID) {
			continue
		}
		var docs []string
		for _, e := range list {
			data := e.Version(h.p.Status)
			if data == nil {
				data = e.Data
			}
			for _, x := range hgList(data[f.Name]) {
				if d, ok := x.(string); ok {
					docs = append(docs, d)
				}
			}
		}
		if len(docs) < 2 {
			continue
		}
		status := h.relStatus(target.UID)
		if !target.DraftAndPublish {
			status = "published"
		}
		loc := ""
		if target.Localized {
			loc = h.p.Locale
			if loc == "" {
				loc = h.s.hgDefaultLocale(h.c)
			}
		}
		rp := &hgReadParams{Auth: h.a, Status: status, Locale: loc, Page: hgPagination{Page: 1, PageSize: 1000}, Budget: h.p.Budget}
		_ = h.loadRel(target, docs, rp) // an error here resurfaces from relation()
	}
}

// relStatus is the status related entries of a type are shown in: the request's, except that a
// draft response (a draft read, or a write's answer) shows related drafts only to callers that
// may read them (hgAuth.relatedDrafts); others see the published versions.
func (h *hgSer) relStatus(uid string) string {
	if h.p.Status == "draft" && !h.a.relatedDrafts(uid) {
		return "published"
	}
	return h.p.Status
}

// entryAt serializes a related entry below the root.
func (h *hgSer) entryAt(sc *hgSchema, e *hgEntry, pop *hgPop, depth int) (map[string]any, error) {
	data := e.Version(h.p.Status)
	if data == nil {
		data = e.Data
	}
	saved := h.cur
	h.cur = e
	out, err := h.object(sc, data, pop, pop.Fields, depth)
	h.cur = saved
	if err != nil {
		return nil, err
	}
	out["id"], out["documentId"] = e.ID, e.DocumentID
	if hgWants(pop.Fields, "createdAt") {
		out["createdAt"], out["updatedAt"] = hgTime(e.CreatedAt), hgTime(e.UpdatedAt)
		if e.PublishedAt != nil && h.p.Status != "draft" {
			out["publishedAt"] = hgTime(*e.PublishedAt)
		} else {
			out["publishedAt"] = nil
		}
	}
	if sc.Localized {
		out["locale"] = e.Locale
	}
	return out, nil
}

func hgAnyStrings(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

func hgPick(m map[string]any, fields []string) map[string]any {
	if len(fields) == 0 {
		return m
	}
	out := map[string]any{"id": m["id"]}
	for _, f := range fields {
		if v, ok := m[f]; ok {
			out[f] = v
		}
	}
	return out
}

// mediaObj is a media library file in the API's upload format.
func (h *hgSer) mediaObj(id int64, fields []string) map[string]any {
	a, ok := h.media[id]
	if !ok {
		var err error
		a, err = h.s.store.GetAttachment(h.c, id)
		if err != nil {
			a = nil
		}
		h.media[id] = a
	}
	if a == nil || !h.s.hgMediaReadable(h.ctx(), h.a, h.wpUser(), a) {
		return nil // like core /wp/v2/media: files of drafts and protected posts stay hidden
	}
	return hgPick(h.s.hgMediaJSON(h.c, a), fields)
}

func (s *Server) hgMediaJSON(c context.Context, a *store.Attachment) map[string]any {
	formats := map[string]any{}
	for _, v := range a.Variants {
		formats[v.Name] = map[string]any{"name": path.Base(v.FilePath), "ext": path.Ext(v.FilePath), "mime": v.Mime, "width": v.Width,
			"height": v.Height, "size": float64(v.FileSize) / 1000, "url": s.uploadURL(c, v.FilePath)}
	}
	var w, hh any
	if a.Width > 0 {
		w, hh = a.Width, a.Height
	}
	name := path.Base(a.FilePath)
	return map[string]any{
		"id": a.ID, "name": name, "alternativeText": hgNilEmpty(a.Alt), "caption": hgNilEmpty(a.Caption), "title": a.Title,
		"width": w, "height": hh, "formats": formats, "ext": path.Ext(name), "mime": a.Mime, "size": float64(a.FileSize) / 1000,
		"url": s.uploadURL(c, a.FilePath), "createdAt": hgTime(a.CreatedAt), "updatedAt": hgTime(a.UpdatedAt),
	}
}

func hgNilEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// hgPostObj is a published post or page as a related object, for caller u (nil: a token or the
// public). Its excerpt is what core REST shows the same caller (restPost): KSES-sanitized, empty
// for a password-protected post the caller cannot edit, and Memberships-restricted.
func (s *Server) hgPostObj(c *gin.Context, p *store.Post, fields []string, u *store.User) map[string]any {
	m := map[string]any{"id": p.ID, "type": p.Type, "slug": p.Slug, "title": p.Title,
		"link": s.links(c).Permalink(p), "date": hgTime(p.Date()), "modified": hgTime(p.ModifiedAt), "featuredMedia": p.FeaturedMediaID}
	if len(fields) == 0 || containsStr(fields, "excerpt") {
		m["excerpt"] = s.hgPostExcerpt(c, p, u)
	}
	return hgPick(m, fields)
}

func (s *Server) hgPostExcerpt(ctx context.Context, p *store.Post, u *store.User) string {
	excerpt := wp.KsesPost(p.Excerpt)
	if u.Can("edit_post", p) {
		return excerpt
	}
	if p.Password != "" {
		return "" // the first words of a password-protected post are as private as the rest of it
	}
	_, excerpt = s.gooRESTRestrict(ctx, u, p, "", excerpt) // Goocommerce Memberships
	return excerpt
}

// WordPress menus --------------------------------------------------------------------------------------

// hgWP serves /wp/menus and /wp/menus/{id|location|slug}: the published items of a menu as a
// tree. Posts, pages and media are in the core REST API (/wp-json/wp/v2/posts, pages, media).
func (s *Server) hgWP(c *gin.Context, a *hgAuth, segs []string, method string) {
	if method != http.MethodGet {
		hgErr(c, http.StatusMethodNotAllowed, "MethodNotAllowedError", "Method Not Allowed", nil)
		return
	}
	if len(segs) == 0 || segs[0] != "menus" || len(segs) > 2 {
		hgNotFound(c)
		return
	}
	menus, err := s.store.Menus(c)
	if err != nil {
		hgFail(c, err)
		return
	}
	brief := hgMenuBrief
	if len(segs) == 1 {
		out := []any{}
		for _, m := range menus {
			out = append(out, brief(m))
		}
		var locs []any
		for _, l := range s.activeTheme().MenuLocations {
			locs = append(locs, map[string]any{"slug": l.Slug, "name": l.Name})
		}
		hgWriteJSON(c, http.StatusOK, map[string]any{"data": out, "meta": map[string]any{"locations": locs}})
		return
	}
	m, err := s.hgMenuFull(c, menus, segs[1])
	if err != nil {
		hgFail(c, err)
		return
	}
	if m == nil {
		hgNotFound(c)
		return
	}
	hgWriteJSON(c, http.StatusOK, map[string]any{"data": m, "meta": map[string]any{}})
}

func hgMenuBrief(m *store.Menu) map[string]any {
	var loc any
	if len(m.Locations) > 0 {
		loc = m.Locations[0]
	}
	return map[string]any{"id": m.ID, "name": m.Name, "slug": m.Slug, "location": loc, "locations": m.Locations, "count": m.Count}
}

// hgMenuFull finds a menu by id, theme location or slug and returns it with its published items as a
// tree (nil when there is none).
func (s *Server) hgMenuFull(c *gin.Context, menus []*store.Menu, key string) (map[string]any, error) {
	var menu *store.Menu
	for _, m := range menus {
		if itoa64(m.ID) == key || m.HasLocation(key) || m.Slug == key {
			menu = m
			if m.HasLocation(key) || itoa64(m.ID) == key {
				break
			}
		}
	}
	if menu == nil {
		return nil, nil
	}
	items, err := s.store.AllMenuItems(c, menu.ID)
	if err != nil {
		return nil, err
	}
	byParent := map[int64][]*store.EditItem{}
	for _, it := range items {
		if defaultStr(it.Status, "publish") != "publish" {
			continue
		}
		// Like the front end (resolveMenu): an item whose post is gone or not public is left out, so a
		// draft or private page's title and link are not served to anonymous clients.
		if it.Kind == "post_type" {
			if p, err := s.store.GetPost(c, it.TargetID); err != nil || (p.Status != "publish" && !(p.Type == "attachment" && p.Status == "inherit")) {
				continue
			}
		}
		byParent[it.ParentID] = append(byParent[it.ParentID], it)
	}
	var build func(parent int64, depth int) []any
	build = func(parent int64, depth int) []any {
		out := []any{}
		if depth > 10 {
			return out
		}
		for _, it := range byParent[parent] {
			var target any
			if it.Target != "" {
				target = it.Target
			}
			out = append(out, map[string]any{"id": it.ID, "title": it.Label, "url": s.menuItemURL(c, it), "target": target,
				"attrTitle": it.AttrTitle, "description": it.Description, "classes": splitWords(it.Classes), "type": defaultStr(it.Kind, "custom"),
				"object": defaultStr(it.Object, "custom"), "objectId": it.TargetID, "parent": it.ParentID, "order": it.Order,
				"children": build(it.ID, depth+1)})
		}
		return out
	}
	m := hgMenuBrief(menu)
	m["items"] = build(0, 0)
	return m, nil
}

// Rate limit and HTTP caching ---------------------------------------------------------------------

const hgCacheKey = "hg.cache"

// hgTagsKey holds the purge tag of a publicly cacheable answer; hgCacheTag is that tag (without the
// cache's prefix). One tag for the whole API: populated relations make any entry part of other
// types' answers, so a change purges every cached read.
const (
	hgTagsKey  = "hg.tags"
	hgCacheTag = "headless"
)

// hgPurge drops the cached Delivery API reads: in Pursae Cache and, through its purge forwarding,
// in a front proxy or CDN that kept them by Surrogate-Key / Cache-Tag.
func (s *Server) hgPurge() { s.purgeTags(hgCacheTag) }

type hgBucket struct {
	count  int
	window time.Time
}

var hgRateBuckets sync.Map // *Server => *hgRateState

type hgRateState struct {
	mu sync.Mutex
	m  map[string]*hgBucket
}

// hgRateAllow applies the fixed-window limit (settings: requests per minute) per token, or per
// client address for token-less and preview calls. It answers 429 itself when the limit is hit.
func (s *Server) hgRateAllow(c *gin.Context, a *hgAuth, st hgSettings) bool {
	if st.RateLimit <= 0 {
		return true
	}
	v, _ := hgRateBuckets.LoadOrStore(s, &hgRateState{m: map[string]*hgBucket{}})
	rs := v.(*hgRateState)
	key := "ip:" + clientIP(c)
	if a.Kind == "token" && a.Token != nil {
		key = "tok:" + strconv.FormatInt(a.Token.ID, 10)
	}
	if a.Kind == hgUserKind {
		key = "user:" + strconv.FormatInt(a.UserID, 10)
	}
	now := time.Now()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.m) > 10000 {
		for k, b := range rs.m {
			if now.Sub(b.window) >= time.Minute {
				delete(rs.m, k)
			}
		}
	}
	b := rs.m[key]
	if b == nil || now.Sub(b.window) >= time.Minute {
		b = &hgBucket{window: now}
		rs.m[key] = b
	}
	b.count++
	if b.count > st.RateLimit {
		wait := int(time.Minute.Seconds() - now.Sub(b.window).Seconds())
		if wait < 1 {
			wait = 1
		}
		c.Header("Retry-After", strconv.Itoa(wait))
		hgErr(c, http.StatusTooManyRequests, "RateLimitError", "Too many requests", nil)
		return false
	}
	return true
}

// hgWriteJSON writes a JSON response. Successful GET/HEAD answers carry a strong ETag of the body and
// are answered with 304 when If-None-Match matches, so clients revalidate without the body.
func hgWriteJSON(c *gin.Context, code int, obj any) {
	if cc, ok := c.Get(hgCacheKey); ok {
		c.Header("Cache-Control", cc.(string))
	}
	if tag := c.GetString(hgTagsKey); tag != "" && code == http.StatusOK {
		c.Header("Surrogate-Key", tag)
		c.Header("Cache-Tag", tag)
	}
	if code != http.StatusOK || (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) {
		c.JSON(code, obj)
		return
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		c.JSON(code, obj)
		return
	}
	sum := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	c.Header("ETag", etag)
	c.Header("Vary", "Authorization, Origin")
	for _, tag := range strings.Split(c.GetHeader("If-None-Match"), ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if tag == etag || tag == "*" {
			c.Status(http.StatusNotModified)
			return
		}
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}
