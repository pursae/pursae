package server

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The delivery API's query language: filters (with $and/$or/$not and filtering through
// relations and components), sort, pagination, populate and fields, given as bracketed query
// parameters (filters[title][$containsi]=go). Parsing is pure; hgQuery compiles filters and sort
// to SQL over the JSONB entry data.

// hgParseQS turns bracketed query parameters into a tree: maps for brackets, a string for one
// value and []string for a repeated key. a[0]=x&a[1]=y gives a map with numeric keys; hgList
// reads either form as a list.
func hgParseQS(vals url.Values) map[string]any {
	root := map[string]any{}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts := hgSplitKey(key)
		if len(parts) == 0 {
			continue
		}
		vs := vals[key]
		var leaf any = vs[len(vs)-1]
		if len(vs) > 1 {
			leaf = append([]string(nil), vs...)
		}
		node := root
		for i, p := range parts {
			if p == "" { // a[]=x: append
				p = strconv.Itoa(len(node))
			}
			if i == len(parts)-1 {
				if _, isMap := node[p].(map[string]any); isMap {
					break // a bracketed form already set this key: keep it
				}
				if p == strconv.Itoa(len(node)) && len(vs) > 1 { // a[]=x&a[]=y
					for _, v := range vs {
						node[strconv.Itoa(len(node))] = v
					}
					break
				}
				node[p] = leaf
				break
			}
			next, ok := node[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				node[p] = next
			}
			node = next
		}
	}
	return root
}

// hgSplitKey splits a[b][c] into a, b, c.
func hgSplitKey(k string) []string {
	i := strings.IndexByte(k, '[')
	if i < 0 {
		return []string{k}
	}
	parts := []string{k[:i]}
	rest := k[i:]
	for len(rest) > 0 {
		if rest[0] != '[' {
			parts[len(parts)-1] += rest // malformed: keep as text
			break
		}
		j := strings.IndexByte(rest, ']')
		if j < 0 {
			parts = append(parts, rest[1:])
			break
		}
		parts = append(parts, rest[1:j])
		rest = rest[j+1:]
	}
	return parts
}

// hgList reads a node as a list: a string, []string, or a map with numeric keys (in order).
func hgList(node any) []any {
	switch t := node.(type) {
	case nil:
		return nil
	case []any:
		return t
	case []string:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = v
		}
		return out
	case map[string]any:
		type kv struct {
			i int
			v any
		}
		var list []kv
		for k, v := range t {
			n, err := strconv.Atoi(k)
			if err != nil {
				return []any{t} // an object, not a list
			}
			list = append(list, kv{n, v})
		}
		sort.Slice(list, func(a, b int) bool { return list[a].i < list[b].i })
		out := make([]any, len(list))
		for i, x := range list {
			out[i] = x.v
		}
		return out
	default:
		return []any{t}
	}
}

// hgStrings reads a node as a list of strings, splitting comma lists.
func hgStrings(node any) []string {
	var out []string
	for _, v := range hgList(node) {
		if s, ok := v.(string); ok {
			for _, p := range strings.Split(s, ",") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// hgQueryError is a 400 Bad Request of the query language.
type hgQueryError struct{ msg string }

func (e *hgQueryError) Error() string { return e.msg }

func hgBad(format string, a ...any) error { return &hgQueryError{fmt.Sprintf(format, a...)} }

// Pagination ----------------------------------------------------------------------------------------

type hgPagination struct {
	Page, PageSize int
	Start, Limit   int
	Offset         bool // start/limit mode
	WithCount      bool
}

// hgMaxPage bounds pagination[page], so (page-1)*pageSize cannot overflow into a negative offset.
const hgMaxPage = 1_000_000_000

// hgParsePagination reads pagination[page|pageSize] or pagination[start|limit].
func hgParsePagination(node any, def, max int) (hgPagination, error) {
	p := hgPagination{Page: 1, PageSize: def, Limit: def, WithCount: true}
	m, _ := node.(map[string]any)
	if m == nil {
		return p, nil
	}
	num := func(k string) (int, bool, error) {
		v, ok := m[k]
		if !ok {
			return 0, false, nil
		}
		s, _ := v.(string)
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return 0, true, hgBad("Invalid pagination parameter %s", k)
		}
		return n, true, nil
	}
	page, hasPage, err := num("page")
	if err != nil {
		return p, err
	}
	size, hasSize, err := num("pageSize")
	if err != nil {
		return p, err
	}
	start, hasStart, err := num("start")
	if err != nil {
		return p, err
	}
	limit, hasLimit, err := num("limit")
	if err != nil {
		return p, err
	}
	if (hasPage || hasSize) && (hasStart || hasLimit) {
		return p, hgBad("Invalid pagination parameters. Expected either page & pageSize or start & limit")
	}
	if wc, ok := m["withCount"].(string); ok && (wc == "false" || wc == "0") {
		p.WithCount = false
	}
	if hasStart || hasLimit {
		p.Offset = true
		if start < 0 {
			return p, hgBad("Pagination start must be a positive integer")
		}
		p.Start = start
		if hasLimit {
			if limit <= 0 || limit > max {
				limit = max
			}
			p.Limit = limit
		}
		return p, nil
	}
	if hasPage {
		if page < 1 {
			return p, hgBad("Pagination page must be greater than 0")
		}
		if page > hgMaxPage {
			return p, hgBad("Pagination page must be at most %d", hgMaxPage)
		}
		p.Page = page
	}
	if hasSize {
		if size < 1 {
			return p, hgBad("Pagination pageSize must be greater than 0")
		}
		p.PageSize = min(size, max)
	}
	return p, nil
}

func (p hgPagination) limitOffset() (int, int) {
	if p.Offset {
		return p.Limit, p.Start
	}
	return p.PageSize, (p.Page - 1) * p.PageSize
}

// meta is the pagination object of meta.
func (p hgPagination) meta(total int) map[string]any {
	if p.Offset {
		m := map[string]any{"start": p.Start, "limit": p.Limit}
		if p.WithCount {
			m["total"] = total
		}
		return m
	}
	m := map[string]any{"page": p.Page, "pageSize": p.PageSize}
	if p.WithCount {
		pages := 0
		if p.PageSize > 0 {
			pages = (total + p.PageSize - 1) / p.PageSize
		}
		m["pageCount"], m["total"] = pages, total
	}
	return m
}

// Sort ----------------------------------------------------------------------------------------------

type hgSortKey struct {
	Field string
	Desc  bool
}

// hgParseSort reads sort=a:asc,b:desc, sort[0]=a:desc or sort[a]=desc.
func hgParseSort(node any) ([]hgSortKey, error) {
	var out []hgSortKey
	add := func(spec string) error {
		field, dir, _ := strings.Cut(strings.TrimSpace(spec), ":")
		field = strings.TrimSpace(field)
		if field == "" {
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(dir)) {
		case "", "asc":
			out = append(out, hgSortKey{Field: field})
		case "desc":
			out = append(out, hgSortKey{Field: field, Desc: true})
		default:
			return hgBad("Invalid sort order %q", dir)
		}
		return nil
	}
	if m, ok := node.(map[string]any); ok {
		if _, err := strconv.Atoi(hgFirstKey(m)); err != nil { // sort[title]=desc
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				d, _ := m[k].(string)
				if err := add(k + ":" + d); err != nil {
					return nil, err
				}
			}
			return out, nil
		}
	}
	for _, s := range hgStrings(node) {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func hgFirstKey(m map[string]any) string {
	for k := range m {
		return k
	}
	return ""
}

// Populate ------------------------------------------------------------------------------------------

// hgPop is what to populate below one attribute (or at the root).
type hgPop struct {
	All      bool              // populate=*: every relation, media, component and dynamic zone, one level
	Fields   []string          // fields of the populated objects (nil: all)
	Filters  map[string]any    // filters of a populated relation
	Sort     []hgSortKey       // sort of a populated relation
	Populate map[string]*hgPop // nested
	On       map[string]*hgPop // per component of a dynamic zone
	// Alias maps Populate keys that are not attribute names to the attribute they load (GraphQL:
	// the same attribute under several aliases, "@alias" => name); the value is serialized under
	// that key.
	Alias map[string]string
}

// hgParsePopulate reads populate in its string, list and object forms.
func hgParsePopulate(node any) (*hgPop, error) {
	p := &hgPop{Populate: map[string]*hgPop{}}
	if node == nil {
		return p, nil
	}
	addPath := func(path string) {
		cur := p
		for _, part := range strings.Split(path, ".") {
			if part == "" {
				continue
			}
			if part == "*" {
				cur.All = true
				continue
			}
			next := cur.Populate[part]
			if next == nil {
				next = &hgPop{Populate: map[string]*hgPop{}}
				cur.Populate[part] = next
			}
			cur = next
		}
	}
	m, isMap := node.(map[string]any)
	if isMap {
		if _, err := strconv.Atoi(hgFirstKey(m)); err == nil && len(m) > 0 {
			isMap = false // populate[0]=a&populate[1]=b
		}
	}
	if !isMap {
		for _, s := range hgStrings(node) {
			if s == "*" {
				p.All = true
			} else if s != "false" {
				addPath(s)
			}
		}
		return p, nil
	}
	for name, v := range m {
		switch t := v.(type) {
		case string:
			switch t {
			case "false", "0":
				continue
			case "*":
				p.Populate[name] = &hgPop{All: true, Populate: map[string]*hgPop{}}
			default:
				p.Populate[name] = &hgPop{Populate: map[string]*hgPop{}}
			}
		case map[string]any:
			sub, err := hgParsePopulateObject(t)
			if err != nil {
				return nil, err
			}
			p.Populate[name] = sub
		default:
			p.Populate[name] = &hgPop{Populate: map[string]*hgPop{}}
		}
	}
	return p, nil
}

// hgParsePopulateObject reads populate[x][populate|fields|filters|sort|on].
func hgParsePopulateObject(m map[string]any) (*hgPop, error) {
	p := &hgPop{Populate: map[string]*hgPop{}}
	for k, v := range m {
		switch k {
		case "populate":
			sub, err := hgParsePopulate(v)
			if err != nil {
				return nil, err
			}
			p.All, p.Populate = sub.All, sub.Populate
		case "fields":
			p.Fields = hgStrings(v)
		case "filters":
			f, ok := v.(map[string]any)
			if !ok {
				return nil, hgBad("Invalid filters in populate")
			}
			p.Filters = f
		case "sort":
			s, err := hgParseSort(v)
			if err != nil {
				return nil, err
			}
			p.Sort = s
		case "on":
			on, ok := v.(map[string]any)
			if !ok {
				return nil, hgBad("Invalid populate on")
			}
			p.On = map[string]*hgPop{}
			for cu, cv := range on {
				var sub *hgPop
				var err error
				if cm, ok := cv.(map[string]any); ok {
					sub, err = hgParsePopulateObject(cm)
				} else {
					sub, err = hgParsePopulate(cv)
				}
				if err != nil {
					return nil, err
				}
				p.On[cu] = sub
			}
		case "count":
		default:
			return nil, hgBad("Invalid populate parameter %s", k)
		}
	}
	return p, nil
}

// SQL compilation -----------------------------------------------------------------------------------

// hgSQL collects the arguments of a query being built.
type hgSQL struct {
	args  []any
	alias int
}

func (q *hgSQL) arg(v any) string {
	q.args = append(q.args, v)
	return "$" + strconv.Itoa(len(q.args))
}

func (q *hgSQL) next(prefix string) string {
	q.alias++
	return prefix + strconv.Itoa(q.alias)
}

// hgQuery compiles filters and sort of one request.
type hgQuery struct {
	reg    *hgRegistry
	sql    *hgSQL
	status string  // draft or published
	locale string  // the requested locale, for localized relation targets
	a      *hgAuth // the caller (nil: the admin): relation targets must be readable by it
	depth  int
	// opDepth bounds the operator nesting ($not, $and, $or) inside one attribute's or one
	// WordPress column filter, which ops() and table() compile recursively.
	opDepth int
}

// hgMaxFilterDepth is how deep filter objects, and separately the operators of one attribute,
// may nest: a request costs at most this many levels of compiling before a 400.
const hgMaxFilterDepth = 12

// enterOps counts one level of operator nesting; leave it with the returned function.
func (q *hgQuery) enterOps() (func(), error) {
	q.opDepth++
	if q.opDepth > hgMaxFilterDepth {
		q.opDepth--
		return nil, hgBad("Filters are nested too deeply")
	}
	return func() { q.opDepth-- }, nil
}

// target checks that the caller may read a relation's target type (key names the attribute in
// the error) and returns the status its entries are compiled in: a draft read sees related
// drafts only when the caller may read them, otherwise their published version.
func (q *hgQuery) target(target *hgSchema, key string) (string, error) {
	if !q.a.reads(target.UID) {
		return "", hgBad("Invalid key %s", key)
	}
	if q.status == "draft" && !q.a.relatedDrafts(target.UID) {
		return "published", nil
	}
	return q.status, nil
}

// in compiles fn with the status of a relation's target entries.
func (q *hgQuery) in(status string, fn func() (string, error)) (string, error) {
	saved := q.status
	q.status = status
	defer func() { q.status = saved }()
	return fn()
}

// hgWPPostCond limits a relation to WordPress posts or pages (alias a) to what anyone may read:
// published ones of the target's type. hgWPUserCols are the users' columns a relation filters
// and sorts on; the login name is not one of them (the API's username is the public nicename,
// and private attributes are not filterable).
func hgWPPostCond(a, target string) string {
	typ, ok := hgWPPostType(target)
	if !ok {
		typ = "post"
	}
	return a + ".status = 'publish' AND " + a + ".type = '" + typ + "'"
}

var hgWPTermCols = map[string][2]string{"id": {"id", "bigint"}, "slug": {"slug", "text"}, "name": {"name", "text"}}

var hgWPUserCols = map[string][2]string{"id": {"id", "bigint"}, "slug": {"nicename", "text"}, "name": {"display_name", "text"}}

// versionCol is the JSONB column of the requested status.
func (q *hgQuery) versionCol(alias string) string {
	if q.status == "draft" {
		return alias + ".data"
	}
	return alias + ".published_data"
}

// hgSystemFields maps the API's system attributes to entry columns.
var hgSystemFields = map[string]struct{ col, kind string }{
	"id": {"id", "bigint"}, "documentId": {"document_id", "text"}, "createdAt": {"created_at", "timestamptz"},
	"updatedAt": {"updated_at", "timestamptz"}, "publishedAt": {"published_at", "timestamptz"}, "locale": {"locale", "text"},
}

// filters compiles a filter object on objects of schema sc stored at col (a JSONB expression);
// alias is the entry row alias, "" inside components.
func (q *hgQuery) filters(sc *hgSchema, node map[string]any, col, alias string) (string, error) {
	q.depth++
	defer func() { q.depth-- }()
	if q.depth > hgMaxFilterDepth {
		return "", hgBad("Filters are nested too deeply")
	}
	keys := make([]string, 0, len(node))
	for k := range node {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := node[k]
		switch k {
		case "$and", "$or":
			var sub []string
			for _, item := range hgList(v) {
				m, ok := item.(map[string]any)
				if !ok {
					return "", hgBad("Invalid %s filter", k)
				}
				c, err := q.filters(sc, m, col, alias)
				if err != nil {
					return "", err
				}
				sub = append(sub, c)
			}
			if len(sub) == 0 {
				continue
			}
			op := " AND "
			if k == "$or" {
				op = " OR "
			}
			parts = append(parts, "("+strings.Join(sub, op)+")")
		case "$not":
			m, ok := v.(map[string]any)
			if !ok {
				return "", hgBad("Invalid $not filter")
			}
			c, err := q.filters(sc, m, col, alias)
			if err != nil {
				return "", err
			}
			parts = append(parts, "NOT ("+c+")")
		default:
			c, err := q.field(sc, k, v, col, alias)
			if err != nil {
				return "", err
			}
			parts = append(parts, c)
		}
	}
	if len(parts) == 0 {
		return "TRUE", nil
	}
	return strings.Join(parts, " AND "), nil
}

// field compiles the filter of one attribute.
func (q *hgQuery) field(sc *hgSchema, name string, v any, col, alias string) (string, error) {
	if sys, ok := hgSystemFields[name]; ok {
		if alias == "" {
			if name != "id" {
				return "", hgBad("Invalid key %s", name)
			}
			return q.ops("("+col+"->>'id')::numeric", "numeric", v)
		}
		if name == "publishedAt" && q.status == "draft" {
			return q.ops(alias+"."+sys.col, sys.kind, v)
		}
		return q.ops(alias+"."+sys.col, sys.kind, v)
	}
	f := sc.Field(name)
	if f == nil || f.Private || f.Type == "password" {
		return "", hgBad("Invalid key %s", name)
	}
	lit := "'" + name + "'" // field names are validated identifiers
	switch f.Type {
	case "component":
		comp := q.reg.Get(f.Component)
		m, ok := v.(map[string]any)
		if comp == nil || !ok {
			return "", hgBad("Invalid filter on component %s", name)
		}
		if only, ok := hgOnlyNullOps(m); ok {
			return q.emptyOps(col+"->"+lit, only)
		}
		if !f.Repeatable {
			return q.filters(comp, m, col+"->"+lit, "")
		}
		a := q.sql.next("c")
		inner, err := q.filters(comp, m, a+".v", "")
		if err != nil {
			return "", err
		}
		return "EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(" + col + "->" + lit + ") = 'array' THEN " + col + "->" + lit +
			" ELSE '[]'::jsonb END) AS " + a + "(v) WHERE " + inner + ")", nil
	case "dynamiczone":
		m, ok := v.(map[string]any)
		if only, isNull := hgOnlyNullOps(m); ok && isNull {
			return q.emptyOps(col+"->"+lit, only)
		}
		return "", hgBad("Dynamic zones cannot be filtered")
	case "relation", "media":
		if f.Type == "relation" && !strings.HasPrefix(f.Target, "wp:") && f.MappedBy == "" {
			if target := q.reg.Get(f.Target); target != nil && !q.a.reads(target.UID) {
				return "", hgBad("Invalid key %s", name)
			}
		}
		if f.MappedBy != "" {
			m, ok := v.(map[string]any)
			if !ok {
				m = map[string]any{"documentId": map[string]any{"$eq": v}}
			}
			return q.inverseFilter(f, m, alias)
		}
		m, ok := v.(map[string]any)
		if !ok {
			// filters[author]=<id>: the related id
			m = map[string]any{"id": map[string]any{"$eq": v}}
			if f.Type == "relation" && !strings.HasPrefix(f.Target, "wp:") {
				m = map[string]any{"documentId": map[string]any{"$eq": v}}
			}
		}
		if only, ok := hgOnlyNullOps(m); ok {
			return q.emptyOps(col+"->"+lit, only)
		}
		return q.relation(f, m, col+"->"+lit)
	}
	expr, kind := hgFieldExpr(f, col)
	return q.ops(expr, kind, v)
}

// hgFieldExpr is the SQL expression and comparison type of a scalar attribute.
func hgFieldExpr(f *hgField, col string) (string, string) {
	lit := "'" + f.Name + "'"
	switch f.Type {
	case "number":
		return "(" + col + "->>" + lit + ")::numeric", "numeric"
	case "boolean":
		return "(" + col + "->>" + lit + ")::boolean", "boolean"
	case "date", "datetime", "time":
		return col + "->>" + lit, f.Type
	}
	return col + "->>" + lit, "text"
}

// hgOnlyNullOps reports whether a filter object holds only $null / $notNull.
func hgOnlyNullOps(m map[string]any) (map[string]any, bool) {
	if len(m) == 0 {
		return nil, false
	}
	for k := range m {
		if k != "$null" && k != "$notNull" {
			return nil, false
		}
	}
	return m, true
}

// emptyOps compiles $null / $notNull on a JSONB list or object.
func (q *hgQuery) emptyOps(expr string, m map[string]any) (string, error) {
	empty := "(" + expr + " IS NULL OR " + expr + " = 'null'::jsonb OR " + expr + " = '[]'::jsonb OR " + expr + " = '{}'::jsonb)"
	var parts []string
	for _, k := range []string{"$null", "$notNull"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		want := hgTruthy(v)
		if k == "$notNull" {
			want = !want
		}
		if want {
			parts = append(parts, empty)
		} else {
			parts = append(parts, "NOT "+empty)
		}
	}
	return strings.Join(parts, " AND "), nil
}

func hgTruthy(v any) bool {
	s, _ := v.(string)
	return s == "true" || s == "1" || s == "" || v == true
}

// relation compiles a filter through a relation (or media) stored as a JSONB id list at expr.
func (q *hgQuery) relation(f *hgField, m map[string]any, expr string) (string, error) {
	ids := "jsonb_array_elements_text(CASE WHEN jsonb_typeof(" + expr + ") = 'array' THEN " + expr + " ELSE '[]'::jsonb END)"
	if f.Type == "media" {
		a := q.sql.next("m")
		inner, err := q.table(m, a, map[string][2]string{"id": {"id", "bigint"}, "name": {"title", "text"}, "mime": {"mime_type", "text"},
			"alternativeText": {"alt_text", "text"}, "caption": {"caption", "text"}})
		if err != nil {
			return "", err
		}
		visible := ""
		if q.a != nil && !q.a.fullAccess() {
			// Like the media fields (hgMediaReadable): files of drafts and protected posts are not
			// filterable, except a token's own uploads.
			own := ""
			if q.a.Kind == "token" && q.a.Token != nil {
				own = " OR " + a + ".meta->>'" + hgUploadTokenMeta + "' = " + q.sql.arg(strconv.FormatInt(q.a.Token.ID, 10))
			}
			visible = " AND (" + a + ".post_id IS NULL OR " + a + ".post_id IN (SELECT id FROM posts WHERE status = 'publish' AND coalesce(password, '') = '')" + own + ")"
		}
		return "EXISTS (SELECT 1 FROM media " + a + " WHERE " + a + ".id IN (SELECT (" + ids + ")::bigint)" + visible + " AND " + inner + ")", nil
	}
	if tax, ok := hgWPTaxonomy(f.Target); ok {
		a := q.sql.next("t")
		inner, err := q.table(m, a, hgWPTermCols)
		if err != nil {
			return "", err
		}
		return "EXISTS (SELECT 1 FROM terms " + a + " WHERE " + a + ".id IN (SELECT (" + ids + ")::bigint) AND " + a + ".taxonomy = " + q.sql.arg(tax) + " AND " + inner + ")", nil
	}
	switch {
	case hgIsWPPost(f.Target):
		a := q.sql.next("p")
		inner, err := q.table(m, a, map[string][2]string{"id": {"id", "bigint"}, "slug": {"slug", "text"}, "title": {"title", "text"}, "status": {"status", "text"}})
		if err != nil {
			return "", err
		}
		return "EXISTS (SELECT 1 FROM posts " + a + " WHERE " + a + ".id IN (SELECT (" + ids + ")::bigint) AND " + hgWPPostCond(a, f.Target) + " AND " + inner + ")", nil
	case f.Target == "wp:user":
		a := q.sql.next("u")
		inner, err := q.table(m, a, hgWPUserCols)
		if err != nil {
			return "", err
		}
		return "EXISTS (SELECT 1 FROM users " + a + " WHERE " + a + ".id IN (SELECT (" + ids + ")::bigint) AND " + inner + ")", nil
	}
	target := q.reg.Get(f.Target)
	if target == nil {
		return "", hgBad("Invalid relation %s", f.Name)
	}
	status, err := q.target(target, f.Name)
	if err != nil {
		return "", err
	}
	return q.in(status, func() (string, error) {
		a := q.sql.next("r")
		cond := a + ".type_uid = " + q.sql.arg(target.UID) + " AND " + a + ".document_id IN (SELECT " + ids + ")"
		if target.Localized {
			cond += " AND " + a + ".locale = " + q.sql.arg(q.locale)
		}
		if q.status != "draft" {
			cond += " AND " + a + ".published_data IS NOT NULL"
		}
		inner, err := q.filters(target, m, q.versionCol(a), a)
		if err != nil {
			return "", err
		}
		return "EXISTS (SELECT 1 FROM hg_entries " + a + " WHERE " + cond + " AND " + inner + ")", nil
	})
}

// table compiles a filter on a WordPress table's columns (posts, users, media).
func (q *hgQuery) table(m map[string]any, alias string, cols map[string][2]string) (string, error) {
	leave, err := q.enterOps()
	if err != nil {
		return "", err
	}
	defer leave()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := m[k]
		switch k {
		case "$and", "$or":
			var sub []string
			for _, item := range hgList(v) {
				mm, ok := item.(map[string]any)
				if !ok {
					return "", hgBad("Invalid %s filter", k)
				}
				c, err := q.table(mm, alias, cols)
				if err != nil {
					return "", err
				}
				sub = append(sub, c)
			}
			if len(sub) > 0 {
				op := " AND "
				if k == "$or" {
					op = " OR "
				}
				parts = append(parts, "("+strings.Join(sub, op)+")")
			}
			continue
		case "$not":
			mm, ok := v.(map[string]any)
			if !ok {
				return "", hgBad("Invalid $not filter")
			}
			c, err := q.table(mm, alias, cols)
			if err != nil {
				return "", err
			}
			parts = append(parts, "NOT ("+c+")")
			continue
		}
		c, ok := cols[k]
		if !ok {
			return "", hgBad("Invalid key %s", k)
		}
		sqlc, err := q.ops(alias+"."+c[0], c[1], v)
		if err != nil {
			return "", err
		}
		parts = append(parts, sqlc)
	}
	if len(parts) == 0 {
		return "TRUE", nil
	}
	return strings.Join(parts, " AND "), nil
}

// hgOperators are the comparison operators of a scalar filter.
var hgOperators = map[string]bool{"$eq": true, "$eqi": true, "$ne": true, "$nei": true, "$lt": true, "$lte": true, "$gt": true, "$gte": true,
	"$in": true, "$notIn": true, "$contains": true, "$notContains": true, "$containsi": true, "$notContainsi": true,
	"$startsWith": true, "$startsWithi": true, "$endsWith": true, "$endsWithi": true, "$null": true, "$notNull": true,
	"$between": true, "$not": true, "$and": true, "$or": true}

// ops compiles the operators applied to one scalar expression of type kind.
func (q *hgQuery) ops(expr, kind string, v any) (string, error) {
	leave, err := q.enterOps()
	if err != nil {
		return "", err
	}
	defer leave()
	m, ok := v.(map[string]any)
	if !ok {
		m = map[string]any{"$eq": v} // filters[title]=x
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, op := range keys {
		val := m[op]
		if !hgOperators[op] {
			return "", hgBad("Invalid operator %s", op)
		}
		var c string
		var err error
		switch op {
		case "$not":
			var inner string
			inner, err = q.ops(expr, kind, val)
			c = "NOT (" + inner + ")"
		case "$and", "$or":
			var sub []string
			for _, item := range hgList(val) {
				var s string
				s, err = q.ops(expr, kind, item)
				if err != nil {
					break
				}
				sub = append(sub, s)
			}
			sep := " AND "
			if op == "$or" {
				sep = " OR "
			}
			if len(sub) > 0 {
				c = "(" + strings.Join(sub, sep) + ")"
			} else {
				c = "TRUE"
			}
		default:
			c, err = q.op(expr, kind, op, val)
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, c)
	}
	if len(parts) == 0 {
		return "TRUE", nil
	}
	return "(" + strings.Join(parts, " AND ") + ")", nil
}

// typed converts a filter value to an SQL parameter of kind.
func (q *hgQuery) typed(kind string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		if l := hgList(v); len(l) == 1 {
			s, ok = l[0].(string)
		}
		if !ok {
			return "", hgBad("Invalid filter value")
		}
	}
	switch kind {
	case "bigint":
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			return "", hgBad("Invalid integer %q", s)
		}
		return q.sql.arg(s) + "::bigint", nil
	case "numeric":
		if !hgDecimalRe.MatchString(s) { // no NaN, Infinity or hex floats
			return "", hgBad("Invalid number %q", s)
		}
		return q.sql.arg(s) + "::numeric", nil
	case "boolean":
		switch s {
		case "true", "1":
			return "TRUE", nil
		case "false", "0":
			return "FALSE", nil
		}
		return "", hgBad("Invalid boolean %q", s)
	case "timestamptz":
		t, ok := hgParseTime(s)
		if !ok {
			return "", hgBad("Invalid date %q", s)
		}
		return q.sql.arg(t) + "::timestamptz", nil
	case "date", "datetime", "time":
		if n, ok := hgNormTemporal(kind, s); ok {
			s = n
		}
		return q.sql.arg(s), nil
	}
	return q.sql.arg(s), nil
}

// hgDecimalRe is a decimal number as a numeric filter value takes it.
var hgDecimalRe = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]{1,4})?$`)

func hgParseTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func hgLikeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// op compiles one operator.
func (q *hgQuery) op(expr, kind, op string, v any) (string, error) {
	text := expr
	if kind != "text" && kind != "date" && kind != "datetime" && kind != "time" {
		text = expr + "::text"
	}
	like := func(pattern string, ci, negate bool) (string, error) {
		s, ok := v.(string)
		if !ok {
			return "", hgBad("Invalid filter value for %s", op)
		}
		p := strings.ReplaceAll(pattern, "X", hgLikeEscape(s))
		kw := " LIKE "
		if ci {
			kw = " ILIKE "
		}
		c := text + kw + q.sql.arg(p)
		if negate {
			return "(" + expr + " IS NULL OR NOT (" + c + "))", nil
		}
		return c, nil
	}
	switch op {
	case "$eq", "$ne":
		if l, isList := v.([]string); isList && op == "$eq" {
			return q.op(expr, kind, "$in", l)
		}
		p, err := q.typed(kind, v)
		if err != nil {
			return "", err
		}
		if op == "$eq" {
			return expr + " = " + p, nil
		}
		return expr + " IS DISTINCT FROM " + p, nil
	case "$eqi", "$nei":
		s, ok := v.(string)
		if !ok {
			return "", hgBad("Invalid filter value for %s", op)
		}
		if op == "$eqi" {
			return "lower(" + text + ") = lower(" + q.sql.arg(s) + ")", nil
		}
		return "lower(" + text + ") IS DISTINCT FROM lower(" + q.sql.arg(s) + ")", nil
	case "$lt", "$lte", "$gt", "$gte":
		p, err := q.typed(kind, v)
		if err != nil {
			return "", err
		}
		return expr + map[string]string{"$lt": " < ", "$lte": " <= ", "$gt": " > ", "$gte": " >= "}[op] + p, nil
	case "$in", "$notIn":
		var ps []string
		for _, item := range hgList(v) {
			p, err := q.typed(kind, item)
			if err != nil {
				return "", err
			}
			ps = append(ps, p)
		}
		if len(ps) == 0 {
			if op == "$in" {
				return "FALSE", nil
			}
			return "TRUE", nil
		}
		if op == "$in" {
			return expr + " IN (" + strings.Join(ps, ", ") + ")", nil
		}
		return "(" + expr + " IS NULL OR " + expr + " NOT IN (" + strings.Join(ps, ", ") + "))", nil
	case "$contains":
		return like("%X%", false, false)
	case "$notContains":
		return like("%X%", false, true)
	case "$containsi":
		return like("%X%", true, false)
	case "$notContainsi":
		return like("%X%", true, true)
	case "$startsWith":
		return like("X%", false, false)
	case "$startsWithi":
		return like("X%", true, false)
	case "$endsWith":
		return like("%X", false, false)
	case "$endsWithi":
		return like("%X", true, false)
	case "$null", "$notNull":
		want := hgTruthy(v)
		if op == "$notNull" {
			want = !want
		}
		if want {
			return expr + " IS NULL", nil
		}
		return expr + " IS NOT NULL", nil
	case "$between":
		l := hgList(v)
		if len(l) == 1 {
			if s, ok := l[0].(string); ok && strings.Contains(s, ",") {
				parts := strings.SplitN(s, ",", 2)
				l = []any{parts[0], parts[1]}
			}
		}
		if len(l) != 2 {
			return "", hgBad("$between needs two values")
		}
		a, err := q.typed(kind, l[0])
		if err != nil {
			return "", err
		}
		b, err := q.typed(kind, l[1])
		if err != nil {
			return "", err
		}
		return expr + " BETWEEN " + a + " AND " + b, nil
	}
	return "", hgBad("Invalid operator %s", op)
}

// orderBy compiles sort keys (scalar attributes, system fields, and paths through single
// components and relations: author.name); the entry id breaks ties.
func (q *hgQuery) orderBy(sc *hgSchema, keys []hgSortKey, alias string) (string, error) {
	var parts []string
	for _, k := range keys {
		expr, err := q.sortExpr(sc, strings.Split(k.Field, "."), q.versionCol(alias), alias, 0)
		if err != nil {
			return "", err
		}
		if k.Desc {
			parts = append(parts, expr+" DESC NULLS LAST")
		} else {
			parts = append(parts, expr+" ASC NULLS LAST")
		}
	}
	parts = append(parts, alias+".id ASC")
	return strings.Join(parts, ", "), nil
}

// hgSearchFilter builds the filter behind the _q parameter: a case-insensitive "contains" over every
// public text-like attribute of the type (private attributes are never searched, so the search
// cannot be used to probe them), or'ed. It is nil for a blank query.
func hgSearchFilter(sc *hgSchema, q string) map[string]any {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	var or []any
	for _, f := range sc.Fields {
		if f.Private {
			continue
		}
		if f.Type == "text" || f.Type == "uid" || f.Type == "email" || f.Type == "enumeration" || (f.Type == "richtext" && f.Format == "markdown") {
			or = append(or, map[string]any{f.Name: map[string]any{"$containsi": q}})
		}
	}
	or = append(or, map[string]any{"documentId": map[string]any{"$eq": q}})
	return map[string]any{"$or": or}
}
