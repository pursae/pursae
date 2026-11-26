package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A GraphQL endpoint over the same data as the REST API:
//
//	query Articles($q: String) { articles(filters: {title: {containsi: $q}}, sort: "views:desc", pagination: {pageSize: 5}, locale: "en", status: PUBLISHED) {
//	    ...Card author { name } blocks { __typename ... on ComponentBlocksQuote { text } } } }
//	fragment Card on Article { documentId title }
//
// Root query fields are a collection type's plural API id in camelCase (a list), its singular one
// with a documentId argument (one entry), a single type's singular API id, and
// <plural>_connection with { nodes pageInfo { total page pageSize pageCount } }. Mutations:
// create<Type>(data, locale, status), update<Type>(documentId, data, locale, status),
// delete<Type>(documentId, locale), publish<Type> / unpublish<Type>(documentId, locale) (single
// types without documentId). Arguments mirror the REST parameters (operators without the $).
// Variables (with defaults), aliases, named and inline fragments, @include / @skip, several
// operations with operationName, and introspection (__schema, __type, __typename) are supported,
// so GraphiQL-type clients work. Permissions are the REST API's.

// hgGQLRequest is one GraphQL request.
type hgGQLRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

func (s *Server) hgGraphQL(c *gin.Context, a *hgAuth, st hgSettings) {
	if c.Request.Method == http.MethodGet && hgIsWebSocket(c.Request) {
		s.hgGQLWebSocket(c, a, st)
		return
	}
	var req hgGQLRequest
	switch c.Request.Method {
	case http.MethodGet:
		req.Query = c.Query("query")
		req.OperationName = c.Query("operationName")
		if v := c.Query("variables"); v != "" {
			_ = json.Unmarshal([]byte(v), &req.Variables)
		}
	case http.MethodPost:
		raw, _ := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if strings.HasPrefix(c.ContentType(), "application/graphql") {
			req.Query = string(raw)
		} else if err := json.Unmarshal(raw, &req); err != nil {
			hgGQLError(c, http.StatusBadRequest, "The body must be JSON: {\"query\": \"...\"}")
			return
		}
	default:
		hgGQLError(c, http.StatusMethodNotAllowed, "Use GET or POST")
		return
	}
	if doc, err := hgGQLParse(req.Query); err == nil {
		if op, err := doc.operation(req.OperationName); err == nil && op.kind == "subscription" {
			s.hgGQLSubscribe(c, a, st, req, doc, op)
			return
		}
	}
	status, out := s.hgGQLExecute(c, a, st, req)
	c.Header("Cache-Control", "no-store")
	c.JSON(status, out)
}

// hgGQLExecute runs a request and returns the HTTP status and the response document.
func (s *Server) hgGQLExecute(c *gin.Context, a *hgAuth, st hgSettings, req hgGQLRequest) (int, map[string]any) {
	fail := func(status int, msg string) (int, map[string]any) {
		return status, map[string]any{"data": nil, "errors": []any{map[string]any{"message": msg}}}
	}
	doc, err := hgGQLParse(req.Query)
	if err != nil {
		return http.StatusBadRequest, hgGQLRequestError(err)
	}
	op, err := doc.operation(req.OperationName)
	if err != nil {
		return http.StatusBadRequest, hgGQLRequestError(err)
	}
	if op.kind == "subscription" {
		return fail(http.StatusBadRequest, "Subscriptions are delivered as server-sent events or over a WebSocket by the public endpoint; the playground cannot stream them")
	}
	if op.kind == "mutation" && c.Request.Method == http.MethodGet {
		return fail(http.StatusMethodNotAllowed, "Mutations need a POST request")
	}
	if op.kind == "mutation" {
		c.Request = c.Request.WithContext(context.WithoutCancel(c.Request.Context())) // a write runs to the end
	}
	ex, sel, status, errDoc := s.hgGQLPrepare(c, a, st, req, doc, op)
	if errDoc != nil {
		return status, errDoc
	}
	return http.StatusOK, ex.run(op, sel)
}

// hgGQLPrepare validates an operation against the schema and expands its fragments and directives.
// On failure it returns the HTTP status and the response document (no data, every problem found).
func (s *Server) hgGQLPrepare(c *gin.Context, a *hgAuth, st hgSettings, req hgGQLRequest, doc *hgGQLDoc, op *hgGQLOp) (*hgGQLExec, []*hgGQLField, int, map[string]any) {
	vars := map[string]any{}
	for k, v := range op.defaults {
		vars[k] = v
	}
	for k, v := range req.Variables {
		vars[k] = v
	}
	ex := &hgGQLExec{s: s, c: c, a: a, st: st, vars: vars}
	if errs := ex.validateOp(doc, op); len(errs) > 0 {
		return nil, nil, http.StatusBadRequest, map[string]any{"errors": hgGQLProblems(doc.src, errs)}
	}
	sel, err := doc.expand(op.sel, ex, 0, map[string]bool{})
	if err != nil {
		return nil, nil, http.StatusBadRequest, hgGQLRequestError(err)
	}
	rootType := map[string]string{"query": "Query", "mutation": "Mutation", "subscription": "Subscription"}[op.kind]
	root := hgGQLRootFlat(sel, rootType)
	if len(root) > hgGQLMaxRootFields {
		return nil, nil, http.StatusBadRequest, map[string]any{"errors": []any{map[string]any{"message": fmt.Sprintf("Operation has %d root fields (aliases included); the limit is %d.", len(root), hgGQLMaxRootFields)}}}
	}
	if a.Kind == "public" && st.NoPublicIntro {
		for _, f := range root {
			if f.name == "__schema" || f.name == "__type" {
				return nil, nil, http.StatusForbidden, map[string]any{"errors": []any{map[string]any{"message": "Introspection is disabled for callers without a token."}}}
			}
		}
	}
	return ex, root, 0, nil
}

// run executes a validated query or mutation and returns the response document.
func (ex *hgGQLExec) run(op *hgGQLOp, sel []*hgGQLField) map[string]any {
	out := map[string]any{}
	for _, f := range sel {
		var v any
		var err error
		if op.kind == "mutation" {
			v, err = ex.mutation(f)
		} else {
			v, err = ex.root(f)
		}
		if err != nil {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": hgGQLErrorMessage(err), "path": []string{f.key()}}}}
		}
		out[f.key()] = v
	}
	return map[string]any{"data": out}
}

// hgGQLErrorMessage is the message a GraphQL error shows the caller: the executor's and the
// store's own messages as they are, but never a database or network error (its text can name the
// database, its user or host): those are logged and answered with a generic message.
func hgGQLErrorMessage(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		if strings.HasPrefix(pe.Code, "22") { // data exception: a value the query could not use
			return "Invalid value in the arguments"
		}
	} else {
		var ce *pgconn.ConnectError
		var ne net.Error
		if !errors.As(err, &ce) && !errors.As(err, &ne) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, pgx.ErrNoRows) && !pgconn.Timeout(err) && !strings.Contains(err.Error(), "SQLSTATE") {
			return err.Error()
		}
	}
	log.Printf("headlessgo: graphql: %v", err)
	return "Internal server error"
}

// hgGQLRootFlat merges the root's inline fragments (on the root type, or without a condition).
// hgGQLMaxRootFields bounds the root fields (aliases count) of one operation, so a single request
// cannot fan out into dozens of paged queries.
const hgGQLMaxRootFields = 20

func hgGQLRootFlat(sel []*hgGQLField, rootType string) []*hgGQLField {
	var out []*hgGQLField
	for _, f := range sel {
		if f.name == "..." {
			if f.on == "" || f.on == rootType {
				out = append(out, hgGQLRootFlat(f.sel, rootType)...)
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

func hgGQLError(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, map[string]any{"data": nil, "errors": []any{map[string]any{"message": msg}}})
}

// Parsing ---------------------------------------------------------------------------------------------

type hgGQLField struct {
	alias, name string
	args        map[string]any // values; variables are hgGQLVar
	sel         []*hgGQLField
	on          string // inline fragment type condition (name == "...")
	spread      string // fragment spread: the fragment's name (name == "...")
	dirs        []hgGQLDir
	pos         int            // byte offset of the field (or of "...") in the document
	argPos      map[string]int // byte offsets of the arguments
}

func (f *hgGQLField) key() string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

type hgGQLDir struct {
	name   string
	args   map[string]any
	pos    int
	argPos map[string]int
}

type hgGQLVar string
type hgGQLEnum string

type hgGQLOp struct {
	kind, name string
	defaults   map[string]any
	defined    map[string]bool // the variables the operation declares
	sel        []*hgGQLField
}

type hgGQLFrag struct {
	name, on string
	sel      []*hgGQLField
	pos      int
}

type hgGQLDoc struct {
	src   string
	ops   []*hgGQLOp
	frags map[string]*hgGQLFrag
}

// hgGQLSyntaxError is a document that does not parse; pos is where the parser stopped.
type hgGQLSyntaxError struct {
	msg string
	pos int
	src string
}

func (e *hgGQLSyntaxError) Error() string { return e.msg }

// hgGQLLocation turns a byte offset into the 1-based line and column of a GraphQL error location.
func hgGQLLocation(src string, pos int) map[string]any {
	if pos > len(src) {
		pos = len(src)
	}
	line, col := 1, 1
	for _, r := range src[:pos] {
		if r == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return map[string]any{"line": line, "column": col}
}

// operation picks the operation to run.
func (d *hgGQLDoc) operation(name string) (*hgGQLOp, error) {
	if name == "" {
		if len(d.ops) != 1 {
			return nil, fmt.Errorf("Must provide operationName if query contains multiple operations")
		}
		return d.ops[0], nil
	}
	for _, op := range d.ops {
		if op.name == name {
			return op, nil
		}
	}
	return nil, fmt.Errorf("Unknown operation named %q", name)
}

// expand replaces fragment spreads by inline fragments and drops what @skip / @include leave out.
func (d *hgGQLDoc) expand(sel []*hgGQLField, ex *hgGQLExec, depth int, visiting map[string]bool) ([]*hgGQLField, error) {
	if depth > 30 {
		return nil, fmt.Errorf("The query is nested too deeply")
	}
	var out []*hgGQLField
	for _, f := range sel {
		if ex.nodes++; ex.nodes > hgGQLMaxNodes {
			return nil, fmt.Errorf("%s", hgGQLTooComplex)
		}
		inc, err := ex.included(f.dirs)
		if err != nil {
			return nil, err
		}
		if !inc {
			continue
		}
		if f.spread != "" {
			fr := d.frags[f.spread]
			if fr == nil {
				return nil, fmt.Errorf("Unknown fragment %q", f.spread)
			}
			if visiting[fr.name] {
				return nil, fmt.Errorf("Cannot spread fragment %q within itself", fr.name)
			}
			visiting[fr.name] = true
			sub, err := d.expand(fr.sel, ex, depth+1, visiting)
			delete(visiting, fr.name)
			if err != nil {
				return nil, err
			}
			out = append(out, &hgGQLField{name: "...", on: fr.on, sel: sub})
			continue
		}
		cp := *f
		cp.dirs = nil
		if len(f.sel) > 0 {
			sub, err := d.expand(f.sel, ex, depth+1, visiting)
			if err != nil {
				return nil, err
			}
			cp.sel = sub
		}
		out = append(out, &cp)
	}
	return out, nil
}

// included evaluates @skip(if:) and @include(if:).
func (ex *hgGQLExec) included(dirs []hgGQLDir) (bool, error) {
	for _, d := range dirs {
		switch d.name {
		case "skip", "include":
			v, ok := ex.resolve(d.args["if"]).(bool)
			if !ok {
				return false, fmt.Errorf("Directive @%s needs a Boolean argument \"if\"", d.name)
			}
			if (d.name == "skip") == v {
				return false, nil
			}
		default:
			return false, fmt.Errorf("Unknown directive @%s", d.name)
		}
	}
	return true, nil
}

type hgGQLParser struct {
	src string
	pos int
}

func hgGQLParse(src string) (*hgGQLDoc, error) {
	p := &hgGQLParser{src: src}
	doc, err := p.document()
	if err != nil {
		return nil, &hgGQLSyntaxError{msg: err.Error(), pos: min(p.pos, len(src)), src: src}
	}
	return doc, nil
}

func (p *hgGQLParser) document() (*hgGQLDoc, error) {
	doc := &hgGQLDoc{src: p.src, frags: map[string]*hgGQLFrag{}}
	for {
		p.skip()
		if p.pos >= len(p.src) {
			break
		}
		if p.peek() == '{' {
			sel, err := p.selection(0)
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, &hgGQLOp{kind: "query", sel: sel})
			continue
		}
		kw := p.name()
		switch kw {
		case "query", "mutation", "subscription":
			op := &hgGQLOp{kind: kw, defaults: map[string]any{}, defined: map[string]bool{}}
			p.skip()
			if hgIsNameStart(p.peek()) {
				op.name = p.name()
				p.skip()
			}
			if p.peek() == '(' {
				if err := p.varDefs(op.defaults, op.defined); err != nil {
					return nil, err
				}
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			sel, err := p.selection(0)
			if err != nil {
				return nil, err
			}
			op.sel = sel
			doc.ops = append(doc.ops, op)
		case "fragment":
			fpos := p.pos - len(kw)
			p.skip()
			name := p.name()
			p.skip()
			if name == "" || name == "on" || p.name() != "on" {
				return nil, fmt.Errorf("Syntax error: expected fragment Name on Type")
			}
			p.skip()
			typ := p.name()
			if typ == "" {
				return nil, fmt.Errorf("Syntax error: expected a type condition")
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			sel, err := p.selection(0)
			if err != nil {
				return nil, err
			}
			if doc.frags[name] != nil {
				return nil, fmt.Errorf("There can be only one fragment named %q", name)
			}
			doc.frags[name] = &hgGQLFrag{name: name, on: typ, sel: sel, pos: fpos}
		default:
			if kw == "" {
				return nil, fmt.Errorf("Syntax error: unexpected %q", p.peek())
			}
			p.pos -= len(kw)
			return nil, fmt.Errorf("Syntax error: unexpected %q", kw)
		}
	}
	if len(doc.ops) == 0 {
		return nil, fmt.Errorf("Syntax error: expected a query")
	}
	return doc, nil
}

// varDefs reads ($a: Int = 1, $b: [String!]!) and keeps the default values.
func (p *hgGQLParser) varDefs(defaults map[string]any, defined map[string]bool) error {
	p.pos++ // (
	for {
		p.skip()
		if p.peek() == ')' {
			p.pos++
			return nil
		}
		if p.pos >= len(p.src) {
			return fmt.Errorf("Syntax error: unclosed variable definitions")
		}
		if err := p.expect('$'); err != nil {
			return err
		}
		name := p.name()
		if name == "" {
			return fmt.Errorf("Syntax error: expected a variable name")
		}
		defined[name] = true
		if err := p.expect(':'); err != nil {
			return err
		}
		if err := p.typeRef(0); err != nil {
			return err
		}
		p.skip()
		if p.peek() == '=' {
			p.pos++
			v, err := p.value(0)
			if err != nil {
				return err
			}
			defaults[name] = hgGQLConst(v)
		}
		if _, err := p.directives(); err != nil {
			return err
		}
	}
}

// hgGQLConst turns a parsed constant into a plain value (enums become strings).
func hgGQLConst(v any) any {
	switch t := v.(type) {
	case hgGQLEnum:
		return string(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = hgGQLConst(x)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			out[k] = hgGQLConst(x)
		}
		return out
	}
	return v
}

func (p *hgGQLParser) typeRef(depth int) error {
	if depth > 10 {
		return fmt.Errorf("Syntax error: the type is nested too deeply")
	}
	p.skip()
	if p.peek() == '[' {
		p.pos++
		if err := p.typeRef(depth + 1); err != nil {
			return err
		}
		if err := p.expect(']'); err != nil {
			return err
		}
	} else if p.name() == "" {
		return fmt.Errorf("Syntax error: expected a type")
	}
	p.skip()
	if p.peek() == '!' {
		p.pos++
	}
	return nil
}

// directives reads @name(args) ...
func (p *hgGQLParser) directives() ([]hgGQLDir, error) {
	var out []hgGQLDir
	for {
		p.skip()
		if p.peek() != '@' {
			return out, nil
		}
		d := hgGQLDir{pos: p.pos}
		p.pos++
		d.name = p.name()
		if d.name == "" {
			return nil, fmt.Errorf("Syntax error: expected a directive name")
		}
		p.skip()
		if p.peek() == '(' {
			args, pos, err := p.arguments(d.name)
			if err != nil {
				return nil, err
			}
			d.args, d.argPos = args, pos
		}
		out = append(out, d)
	}
}

func (p *hgGQLParser) arguments(of string) (map[string]any, map[string]int, error) {
	p.pos++ // (
	args, pos := map[string]any{}, map[string]int{}
	for {
		p.skip()
		if p.peek() == ')' {
			p.pos++
			return args, pos, nil
		}
		at := p.pos
		k := p.name()
		if k == "" {
			return nil, nil, fmt.Errorf("Syntax error in the arguments of %s", of)
		}
		if err := p.expect(':'); err != nil {
			return nil, nil, err
		}
		v, err := p.value(0)
		if err != nil {
			return nil, nil, err
		}
		args[k], pos[k] = v, at
	}
}

func hgIsNameStart(b byte) bool { return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func (p *hgGQLParser) peek() byte {
	if p.pos < len(p.src) {
		return p.src[p.pos]
	}
	return 0
}

// skip passes whitespace, commas and comments.
func (p *hgGQLParser) skip() {
	for p.pos < len(p.src) {
		ch := p.src[p.pos]
		if ch == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' || ch == 0xEF || ch == 0xBB || ch == 0xBF {
			p.pos++
			continue
		}
		break
	}
}

func (p *hgGQLParser) name() string {
	start := p.pos
	for p.pos < len(p.src) {
		ch := p.src[p.pos]
		if hgIsNameStart(ch) || (ch >= '0' && ch <= '9') {
			p.pos++
			continue
		}
		break
	}
	return p.src[start:p.pos]
}

func (p *hgGQLParser) expect(ch byte) error {
	p.skip()
	if p.peek() != ch {
		return fmt.Errorf("Syntax error: expected %q", ch)
	}
	p.pos++
	return nil
}

func (p *hgGQLParser) selection(depth int) ([]*hgGQLField, error) {
	if depth > 15 {
		return nil, fmt.Errorf("The query is nested too deeply")
	}
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var out []*hgGQLField
	for {
		p.skip()
		if p.peek() == '}' {
			p.pos++
			break
		}
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("Syntax error: unclosed selection set")
		}
		if strings.HasPrefix(p.src[p.pos:], "...") {
			f := &hgGQLField{name: "...", pos: p.pos}
			p.pos += 3
			p.skip()
			save := p.pos
			if n := p.name(); n == "on" {
				p.skip()
				f.on = p.name()
				if f.on == "" {
					return nil, fmt.Errorf("Syntax error: expected a type condition")
				}
			} else if n != "" {
				f.spread = n
			} else {
				p.pos = save // ... @dir { } or ... { }
			}
			dirs, err := p.directives()
			if err != nil {
				return nil, err
			}
			f.dirs = dirs
			if f.spread == "" {
				sel, err := p.selection(depth + 1)
				if err != nil {
					return nil, err
				}
				f.sel = sel
			}
			out = append(out, f)
			continue
		}
		if !hgIsNameStart(p.peek()) {
			return nil, fmt.Errorf("Syntax error: unexpected %q", p.peek())
		}
		f := &hgGQLField{pos: p.pos}
		f.name = p.name()
		p.skip()
		if p.peek() == ':' {
			p.pos++
			p.skip()
			f.alias, f.name = f.name, p.name()
			if f.name == "" {
				return nil, fmt.Errorf("Syntax error: expected a field name after the alias %s", f.alias)
			}
			p.skip()
		}
		if p.peek() == '(' {
			args, pos, err := p.arguments(f.name)
			if err != nil {
				return nil, err
			}
			f.args, f.argPos = args, pos
			p.skip()
		}
		dirs, err := p.directives()
		if err != nil {
			return nil, err
		}
		f.dirs = dirs
		p.skip()
		if p.peek() == '{' {
			sel, err := p.selection(depth + 1)
			if err != nil {
				return nil, err
			}
			f.sel = sel
		}
		out = append(out, f)
	}
	return out, nil
}

func (p *hgGQLParser) value(depth int) (any, error) {
	if depth > 20 {
		return nil, fmt.Errorf("The arguments are nested too deeply")
	}
	p.skip()
	ch := p.peek()
	switch {
	case ch == '$':
		p.pos++
		return hgGQLVar(p.name()), nil
	case ch == '"':
		return p.stringValue()
	case ch == '[':
		p.pos++
		list := []any{}
		for {
			p.skip()
			if p.peek() == ']' {
				p.pos++
				return list, nil
			}
			if p.pos >= len(p.src) {
				return nil, fmt.Errorf("Syntax error: unclosed list")
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
	case ch == '{':
		p.pos++
		obj := map[string]any{}
		for {
			p.skip()
			if p.peek() == '}' {
				p.pos++
				return obj, nil
			}
			k := p.name()
			if k == "" {
				return nil, fmt.Errorf("Syntax error in an object value")
			}
			if err := p.expect(':'); err != nil {
				return nil, err
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			obj[k] = v
		}
	case ch == '-' || (ch >= '0' && ch <= '9'):
		start := p.pos
		p.pos++
		for p.pos < len(p.src) && strings.IndexByte("0123456789.eE+-", p.src[p.pos]) >= 0 {
			p.pos++
		}
		lit := p.src[start:p.pos]
		n, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			return nil, fmt.Errorf("Syntax error: bad number")
		}
		if math.Abs(n) >= 1<<53 && !strings.ContainsAny(lit, ".eE") {
			// An integer a float64 cannot hold exactly: keep its digits (biginteger fields).
			if _, ok := new(big.Int).SetString(lit, 10); ok {
				return json.Number(lit), nil
			}
		}
		return n, nil
	case hgIsNameStart(ch):
		switch n := p.name(); n {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return hgGQLEnum(n), nil
		}
	}
	return nil, fmt.Errorf("Syntax error: unexpected %q", ch)
}

// stringValue reads a string literal: "..." with the spec's escape sequences, or a """block
// string""" (raw text, common indentation and blank first / last lines removed).
func (p *hgGQLParser) stringValue() (any, error) {
	if strings.HasPrefix(p.src[p.pos:], `"""`) {
		p.pos += 3
		var raw strings.Builder
		for {
			if p.pos >= len(p.src) {
				return nil, fmt.Errorf("Syntax error: unterminated string")
			}
			rest := p.src[p.pos:]
			if strings.HasPrefix(rest, `"""`) {
				p.pos += 3
				return hgGQLBlockString(raw.String()), nil
			}
			if strings.HasPrefix(rest, `\"""`) {
				raw.WriteString(`"""`)
				p.pos += 4
				continue
			}
			raw.WriteByte(p.src[p.pos])
			p.pos++
		}
	}
	p.pos++ // "
	var b strings.Builder
	for {
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("Syntax error: unterminated string")
		}
		ch := p.src[p.pos]
		switch ch {
		case '"':
			p.pos++
			return b.String(), nil
		case '\n', '\r':
			return nil, fmt.Errorf("Syntax error: unterminated string")
		case '\\':
		default:
			b.WriteByte(ch)
			p.pos++
			continue
		}
		p.pos++
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("Syntax error: unterminated string")
		}
		esc := p.src[p.pos]
		p.pos++
		switch esc {
		case '"', '\\', '/':
			b.WriteByte(esc)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, err := p.unicodeEscape()
			if err != nil {
				return nil, err
			}
			if utf16.IsSurrogate(r) {
				if r >= 0xDC00 || !strings.HasPrefix(p.src[p.pos:], `\u`) {
					return nil, fmt.Errorf("Syntax error: invalid Unicode escape sequence")
				}
				p.pos += 2
				lo, err := p.unicodeEscape()
				if err != nil {
					return nil, err
				}
				if r = utf16.DecodeRune(r, lo); r == utf8.RuneError {
					return nil, fmt.Errorf("Syntax error: invalid Unicode escape sequence")
				}
			}
			b.WriteRune(r)
		default:
			return nil, fmt.Errorf("Syntax error: invalid escape sequence \\%c", esc)
		}
	}
}

// unicodeEscape reads the code of a \u escape (after the u): four hex digits or {hex digits}.
func (p *hgGQLParser) unicodeEscape() (rune, error) {
	bad := fmt.Errorf("Syntax error: invalid Unicode escape sequence")
	hex := ""
	if strings.HasPrefix(p.src[p.pos:], "{") {
		end := strings.IndexByte(p.src[p.pos:], '}')
		if end < 2 || end > 9 {
			return 0, bad
		}
		hex = p.src[p.pos+1 : p.pos+end]
		p.pos += end + 1
	} else {
		if p.pos+4 > len(p.src) {
			return 0, bad
		}
		hex = p.src[p.pos : p.pos+4]
		p.pos += 4
	}
	n, err := strconv.ParseUint(hex, 16, 32)
	if err != nil || n > utf8.MaxRune || strings.ContainsAny(hex, "+-_xX") {
		return 0, bad
	}
	return rune(n), nil
}

// hgGQLBlockString is the value of a block string's raw text (the spec's BlockStringValue).
func hgGQLBlockString(raw string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n"), "\n")
	indent := -1
	for i, l := range lines {
		if i == 0 {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if n < len(l) && (indent < 0 || n < indent) {
			indent = n
		}
	}
	if indent > 0 {
		for i := 1; i < len(lines); i++ {
			if len(lines[i]) >= indent {
				lines[i] = lines[i][indent:]
			} else {
				lines[i] = ""
			}
		}
	}
	blank := func(l string) bool { return strings.Trim(l, " \t") == "" }
	for len(lines) > 0 && blank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// Execution ---------------------------------------------------------------------------------------------

type hgGQLExec struct {
	s    *Server
	c    *gin.Context
	a    *hgAuth
	st   hgSettings
	vars map[string]any
	sch  *hgGQLSchema // built on first use
	// visSch is the schema the caller is shown (callerSchema), built on first use.
	visSch *hgGQLSchema
	// nodes counts the selections expand produced (hgGQLMaxNodes).
	nodes int
	// budget is what the operation (or one subscription event) may still spend on queries.
	budget *hgReadBudget
}

// readBudget is the operation's read budget, shared by all its root fields.
func (ex *hgGQLExec) readBudget() *hgReadBudget {
	if ex.budget == nil {
		ex.budget = newHGReadBudget()
	}
	return ex.budget
}

// resolve replaces variables in an argument value.
func (ex *hgGQLExec) resolve(v any) any {
	switch t := v.(type) {
	case hgGQLVar:
		return ex.vars[string(t)]
	case hgGQLEnum:
		return string(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = ex.resolve(x)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			out[k] = ex.resolve(x)
		}
		return out
	}
	return v
}

// hgCamel turns a kebab-case API id into camelCase (blog-posts -> blogPosts).
func hgCamel(id string) string {
	parts := strings.Split(id, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// hgPascal is a GraphQL type name: Article, ComponentSharedSeo.
func hgPascal(sc *hgSchema) string {
	src := sc.SingularName
	prefix := ""
	if sc.IsComponent() {
		src, prefix = strings.ReplaceAll(sc.UID, ".", "-"), "Component"
	}
	var b strings.Builder
	b.WriteString(prefix)
	up := true
	for _, r := range src {
		if r == '-' || r == '_' {
			up = true
			continue
		}
		if up {
			b.WriteRune(unicode.ToUpper(r))
			up = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (ex *hgGQLExec) root(f *hgGQLField) (any, error) {
	switch f.name {
	case "__typename":
		return "Query", nil
	case "__schema":
		return ex.intro(ex.callerSchema().schemaObj(), f.sel), nil
	case "__type":
		name := hgGQLString(ex.resolve(f.args["name"]))
		return ex.intro(ex.callerSchema().typeObj(name), f.sel), nil
	}
	if strings.HasPrefix(f.name, "__") {
		return nil, fmt.Errorf("Cannot query field %q on type \"Query\"", f.name)
	}
	if v, ok, err := ex.wpRoot(f); ok {
		return v, err
	}
	reg := ex.s.hgReg(ex.c)
	name, connection := strings.CutSuffix(f.name, "_connection")
	for _, sc := range reg.Types() {
		switch {
		case sc.IsCollection() && hgCamel(sc.PluralName) == name:
			if !ex.a.can(sc.UID, "find") || (ex.a.Kind == "preview" && ex.a.PrevUID != sc.UID) {
				return nil, fmt.Errorf("Forbidden access")
			}
			return ex.list(sc, f, connection)
		case !connection && hgCamel(sc.SingularName) == name:
			if !ex.a.can(sc.UID, map[bool]string{true: "find", false: "findOne"}[sc.IsSingle()]) {
				return nil, fmt.Errorf("Forbidden access")
			}
			return ex.one(sc, f)
		}
	}
	return nil, fmt.Errorf("Cannot query field %q on type \"Query\"", f.name)
}

// params converts GraphQL arguments to REST read parameters.
func (ex *hgGQLExec) params(sc *hgSchema, f *hgGQLField, sel []*hgGQLField) (*hgReadParams, error) {
	p := &hgReadParams{Auth: ex.a, Status: "published", Budget: ex.readBudget()}
	args := map[string]any{}
	for k, v := range f.args {
		args[k] = ex.resolve(v)
	}
	if s, ok := args["status"].(string); ok && s != "" {
		switch strings.ToLower(s) {
		case "draft":
			p.Status = "draft"
		case "published":
		default:
			return nil, fmt.Errorf("Invalid status %q", s)
		}
	}
	if !sc.DraftAndPublish {
		p.Status = "published"
	}
	if p.Status == "draft" && !ex.a.drafts(sc.UID) {
		return nil, fmt.Errorf("Reading drafts needs a token allowed to read them")
	}
	loc, _ := args["locale"].(string)
	loc, ok := ex.a.previewLocale(sc, loc)
	if !ok {
		return nil, fmt.Errorf("Forbidden access")
	}
	l, err := ex.s.hgEntryLocale(ex.c, sc, loc)
	if err != nil {
		return nil, err
	}
	p.Locale = l
	if fl, ok := args["filters"].(map[string]any); ok {
		p.Filters = hgGQLFilters(fl)
	}
	if s := args["sort"]; s != nil {
		p.Sort, err = hgParseSort(hgGQLStrings(s))
		if err != nil {
			return nil, err
		}
	}
	pag := map[string]any{}
	if pm, ok := args["pagination"].(map[string]any); ok {
		for k, v := range pm {
			pag[k] = hgGQLString(v)
		}
	}
	if p.Page, err = hgParsePagination(pag, ex.st.DefaultPageSize, ex.st.MaxPageSize); err != nil {
		return nil, err
	}
	if p.Populate, err = ex.populate(sc, sel); err != nil {
		return nil, err
	}
	return p, nil
}

// hgGQLFilters prefixes operators with $ and turns values into the REST parser's strings.
func hgGQLFilters(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		key := k
		if hgOperators["$"+k] {
			key = "$" + k
		}
		switch t := v.(type) {
		case map[string]any:
			out[key] = hgGQLFilters(t)
		case []any:
			var list []any
			for _, x := range t {
				if xm, ok := x.(map[string]any); ok {
					list = append(list, hgGQLFilters(xm))
				} else {
					list = append(list, hgGQLString(x))
				}
			}
			out[key] = list
		default:
			out[key] = hgGQLString(v)
		}
	}
	return out
}

func hgGQLString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func hgGQLStrings(v any) any {
	if l, ok := v.([]any); ok {
		out := make([]string, len(l))
		for i, x := range l {
			out[i] = hgGQLString(x)
		}
		return out
	}
	return hgGQLString(v)
}

// flat merges inline fragments into one selection.
func hgGQLFlat(sel []*hgGQLField) []*hgGQLField {
	var out []*hgGQLField
	for _, f := range sel {
		if f.name == "..." {
			out = append(out, hgGQLFlat(f.sel)...)
		} else {
			out = append(out, f)
		}
	}
	return out
}

// populate derives what to load from a selection set. A relation, media, component or dynamic
// zone selected under an alias is loaded under its own key ("@" + alias), so aliases with
// different arguments get their own data; selections of the same response key are merged, and
// differing arguments for one response key are an error (the spec's OverlappingFieldsCanBeMerged).
func (ex *hgGQLExec) populate(sc *hgSchema, sel []*hgGQLField) (*hgPop, error) {
	pop := &hgPop{Populate: map[string]*hgPop{}}
	reg := ex.s.hgReg(ex.c)
	args := map[string]any{} // response key => the arguments it was first selected with
	for _, f := range hgGQLFlat(sel) {
		if f.name == "localizations" && sc.Localized {
			if pop.Populate["localizations"] == nil {
				pop.Populate["localizations"] = &hgPop{Populate: map[string]*hgPop{}}
			}
			continue
		}
		fd := sc.Field(f.name)
		if fd == nil || fd.scalar() || fd.Private {
			continue
		}
		key := hgGQLPopKey(f)
		resolved := ex.resolve(f.args)
		if m, ok := resolved.(map[string]any); ok && len(m) == 0 {
			resolved = nil
		}
		if prev, seen := args[key]; seen && !reflect.DeepEqual(prev, resolved) {
			return nil, fmt.Errorf("Fields %q conflict because they have differing arguments. Use different aliases on the fields to fetch both if this was intentional.", f.key())
		}
		args[key] = resolved
		var sub *hgPop
		var err error
		switch fd.Type {
		case "relation":
			if t := reg.Get(fd.Target); t != nil {
				if sub, err = ex.populate(t, f.sel); err != nil {
					return nil, err
				}
			} else {
				sub = &hgPop{Populate: map[string]*hgPop{}}
			}
			if f.args != nil {
				if fl, ok := ex.resolve(f.args["filters"]).(map[string]any); ok {
					sub.Filters = hgGQLFilters(fl)
				}
				if s := ex.resolve(f.args["sort"]); s != nil {
					sub.Sort, _ = hgParseSort(hgGQLStrings(s))
				}
			}
		case "component":
			if comp := reg.Get(fd.Component); comp != nil {
				if sub, err = ex.populate(comp, f.sel); err != nil {
					return nil, err
				}
			}
		case "dynamiczone":
			sub = &hgPop{Populate: map[string]*hgPop{}, On: map[string]*hgPop{}}
			for _, cu := range fd.Components {
				if comp := reg.Get(cu); comp != nil {
					var fsel []*hgGQLField
					for _, x := range f.sel {
						if x.name == "..." && (x.on == hgPascal(comp) || x.on == "") {
							fsel = append(fsel, x.sel...)
						} else if x.name != "..." {
							fsel = append(fsel, x)
						}
					}
					if sub.On[cu], err = ex.populate(comp, fsel); err != nil {
						return nil, err
					}
				}
			}
		default:
			sub = &hgPop{Populate: map[string]*hgPop{}}
		}
		if sub == nil {
			continue
		}
		if key != f.name {
			if pop.Alias == nil {
				pop.Alias = map[string]string{}
			}
			pop.Alias[key] = f.name
		}
		if prev := pop.Populate[key]; prev != nil {
			sub = hgMergePop(prev, sub)
		}
		pop.Populate[key] = sub
	}
	return pop, nil
}

// hgGQLPopKey is the populate key of a field: its name, or "@" + alias when it has an alias.
func hgGQLPopKey(f *hgGQLField) string {
	if f.alias != "" && f.alias != f.name {
		return "@" + f.alias
	}
	return f.name
}

// hgMergePop merges two populates of the same response key (both selections are loaded).
func hgMergePop(a, b *hgPop) *hgPop {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := *a
	out.All = a.All || b.All
	if a.Fields == nil || b.Fields == nil {
		out.Fields = nil
	} else {
		out.Fields = append([]string(nil), a.Fields...)
		for _, f := range b.Fields {
			if !containsStr(out.Fields, f) {
				out.Fields = append(out.Fields, f)
			}
		}
	}
	out.Populate = map[string]*hgPop{}
	for k, v := range a.Populate {
		out.Populate[k] = v
	}
	for k, v := range b.Populate {
		out.Populate[k] = hgMergePop(out.Populate[k], v)
	}
	if a.On != nil || b.On != nil {
		out.On = map[string]*hgPop{}
		for k, v := range a.On {
			out.On[k] = v
		}
		for k, v := range b.On {
			out.On[k] = hgMergePop(out.On[k], v)
		}
	}
	if a.Alias != nil || b.Alias != nil {
		out.Alias = map[string]string{}
		for k, v := range a.Alias {
			out.Alias[k] = v
		}
		for k, v := range b.Alias {
			out.Alias[k] = v
		}
	}
	return &out
}

func (ex *hgGQLExec) list(sc *hgSchema, f *hgGQLField, connection bool) (any, error) {
	sel := f.sel
	var nodesSel, pageSel []*hgGQLField
	if connection {
		for _, x := range f.sel {
			switch x.name {
			case "nodes":
				nodesSel = x.sel
			case "pageInfo":
				pageSel = x.sel
			}
		}
		sel = nodesSel
	}
	p, err := ex.params(sc, f, sel)
	if err != nil {
		return nil, err
	}
	o := hgFindOpts{}
	if ex.a.Kind == "preview" {
		o.Doc = ex.a.PrevDoc
	}
	if connection {
		p.Page.WithCount = true
	}
	list, total, err := ex.s.hgFind(ex.c, sc, p, o)
	if err != nil {
		return nil, err
	}
	ser := ex.s.hgSerializer(ex.c, ex.a, p)
	ser.prefetch(sc, list, p.Populate)
	items := []any{}
	for _, e := range list {
		obj, err := ser.entry(sc, e, p.Populate, nil)
		if err != nil {
			return nil, err
		}
		items = append(items, ex.project(sc, obj, sel))
	}
	if !connection {
		return items, nil
	}
	out := map[string]any{}
	for _, x := range f.sel {
		switch x.name {
		case "nodes":
			out[x.key()] = items
		case "pageInfo":
			m := p.Page.meta(total)
			pi := map[string]any{}
			for _, y := range pageSel {
				pi[y.key()] = m[y.name]
			}
			out[x.key()] = pi
		case "__typename":
			out[x.key()] = hgPascal(sc) + "EntityResponseCollection"
		}
	}
	return out, nil
}

func (ex *hgGQLExec) one(sc *hgSchema, f *hgGQLField) (any, error) {
	p, err := ex.params(sc, f, f.sel)
	if err != nil {
		return nil, err
	}
	doc := ""
	if sc.IsSingle() {
		doc = ex.s.hgSingleDoc(ex.c, sc.UID)
	} else {
		doc = hgGQLString(ex.resolve(f.args["documentId"]))
		if doc == "" {
			return nil, fmt.Errorf("The argument documentId is required")
		}
	}
	if ex.a.Kind == "preview" && (ex.a.PrevUID != sc.UID || (doc != ex.a.PrevDoc)) {
		return nil, fmt.Errorf("Forbidden access")
	}
	if doc == "" {
		return nil, nil
	}
	e := ex.s.hgOneEntry(ex.c, sc, doc, p)
	if e == nil {
		return nil, nil
	}
	obj, err := ex.s.hgSerializer(ex.c, ex.a, p).entry(sc, e, p.Populate, nil)
	if err != nil {
		return nil, err
	}
	return ex.project(sc, obj, f.sel), nil
}

// project keeps the selected keys of a serialized object (with aliases and __typename). Values
// selected more than once under one response key (fragments) are merged.
func (ex *hgGQLExec) project(sc *hgSchema, obj map[string]any, sel []*hgGQLField) map[string]any {
	reg := ex.s.hgReg(ex.c)
	out := map[string]any{}
	for _, f := range sel {
		if f.name == "..." {
			if sc != nil && (f.on == "" || f.on == hgPascal(sc)) {
				for k, v := range ex.project(sc, obj, f.sel) {
					hgGQLMergeInto(out, k, v)
				}
			}
			continue
		}
		if f.name == "__typename" {
			if sc != nil {
				out[f.key()] = hgPascal(sc)
			} else if _, ok := obj["mime"]; ok {
				out[f.key()] = "UploadFile"
			}
			continue
		}
		var fd *hgField
		if sc != nil {
			fd = sc.Field(f.name)
		}
		src := f.name
		if fd != nil && !fd.scalar() && !fd.Private {
			src = hgGQLPopKey(f) // loaded under the alias (populate)
		}
		v, ok := obj[src]
		if !ok {
			if _, set := out[f.key()]; !set {
				out[f.key()] = nil
			}
			continue
		}
		if len(f.sel) == 0 || sc == nil {
			if len(f.sel) > 0 {
				hgGQLMergeInto(out, f.key(), hgGQLProjectAny(v, f.sel))
			} else {
				out[f.key()] = v
			}
			continue
		}
		var sub *hgSchema
		if fd != nil {
			switch fd.Type {
			case "relation":
				sub = reg.Get(fd.Target)
			case "component":
				sub = reg.Get(fd.Component)
			}
		}
		if f.name == "localizations" {
			sub = sc
		}
		switch t := v.(type) {
		case map[string]any:
			hgGQLMergeInto(out, f.key(), ex.project(sub, t, f.sel))
		case []any:
			list := make([]any, 0, len(t))
			for _, x := range t {
				m, ok := x.(map[string]any)
				if !ok {
					continue
				}
				s2 := sub
				if fd != nil && fd.Type == "dynamiczone" {
					cu, _ := m["__component"].(string)
					s2 = reg.Get(cu)
				}
				list = append(list, ex.project(s2, m, f.sel))
			}
			hgGQLMergeInto(out, f.key(), list)
		default:
			out[f.key()] = v
		}
	}
	return out
}

// hgGQLMergeInto sets out[k] = v, merging objects (and lists of objects, item by item) that are
// already there: the same field selected twice, e.g. directly and through a fragment.
func hgGQLMergeInto(out map[string]any, k string, v any) {
	out[k] = hgGQLMergeValues(out[k], v)
}

func hgGQLMergeValues(old, v any) any {
	switch t := v.(type) {
	case map[string]any:
		if om, ok := old.(map[string]any); ok {
			for k, x := range t {
				om[k] = hgGQLMergeValues(om[k], x)
			}
			return om
		}
	case []any:
		if ol, ok := old.([]any); ok && len(ol) == len(t) {
			for i := range t {
				ol[i] = hgGQLMergeValues(ol[i], t[i])
			}
			return ol
		}
	}
	return v
}

// hgGQLProjectAny projects plain objects (media, WordPress objects) by selection.
func hgGQLProjectAny(v any, sel []*hgGQLField) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for _, f := range hgGQLFlat(sel) {
			out[f.key()] = hgGQLProjectAny(t[f.name], f.sel)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = hgGQLProjectAny(x, sel)
		}
		return out
	}
	return v
}
