package server

import (
	"fmt"
	"sort"
	"strings"
)

// Query validation: before anything runs, the operation is checked against the generated schema
// the way the GraphQL specification's validation rules do, so a mistake is an error with its
// location instead of a null in the answer. Checked: fields the type does not have (with a "Did you
// mean" hint), a leaf with a sub-selection and an object without one, unknown arguments and
// missing required ones on every field, unknown fields inside input objects (filters, data,
// pagination; literals and variable values alike), enum literals, variables the operation does not
// declare, unknown fragments, fragment cycles, type conditions that are unknown or can never apply,
// and unknown directives or directive arguments. The whole document is checked, also the parts that
// @skip / @include leave out. Failures are answered with HTTP 400, no data and every problem found:
//
//	{"errors":[{"message":"Cannot query field \"titel\" on type \"Article\". Did you mean \"title\"?",
//	  "locations":[{"line":1,"column":12}],"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}
//
// Introspection (__schema, __type) is answered by its own code and is not walked; JSON scalars may
// carry a sub-selection (it picks keys of the value) and take any value.

const (
	hgGQLValidationCode = "GRAPHQL_VALIDATION_FAILED"
	hgGQLParseCode      = "GRAPHQL_PARSE_FAILED"
)

// hgGQLProblem is one validation error at a byte offset of the document (-1: no location).
type hgGQLProblem struct {
	msg string
	pos int
}

// hgGQLProblems turns problems into the errors array of a validation failure.
func hgGQLProblems(src string, list []hgGQLProblem) []any {
	out := make([]any, len(list))
	for i, p := range list {
		e := map[string]any{"message": p.msg, "extensions": map[string]any{"code": hgGQLValidationCode}}
		if p.pos >= 0 {
			e["locations"] = []any{hgGQLLocation(src, p.pos)}
		}
		out[i] = e
	}
	return out
}

// hgGQLRequestError is the response document of a request that failed before execution: a syntax
// error (with its location), or another problem with the request itself.
func hgGQLRequestError(err error) map[string]any {
	e := map[string]any{"message": err.Error()}
	if se, ok := err.(*hgGQLSyntaxError); ok {
		e["locations"] = []any{hgGQLLocation(se.src, se.pos)}
		e["extensions"] = map[string]any{"code": hgGQLParseCode}
	}
	return map[string]any{"errors": []any{e}}
}

type hgGQLValidator struct {
	g        *hgGQLSchema
	vis      *hgGQLSchema // what the caller is shown: "Did you mean" names only come from it
	doc      *hgGQLDoc
	op       *hgGQLOp
	vars     map[string]any // the request's variable values (defaults applied)
	errs     []hgGQLProblem
	seen     map[hgGQLProblem]bool
	visiting map[string]bool
	depth    int
	nodes    int // selections walked, fragments counted at every spread (hgGQLMaxNodes)
}

// hgGQLMaxNodes bounds the selections one operation may expand to. Fragments spread several times
// at several levels multiply (2 spreads × 30 levels is 2^30 fields from a 1 KB request), so both
// the validator and the expander count every selection they walk and give up past this.
const hgGQLMaxNodes = 10000

const hgGQLTooComplex = "The query is too complex."

// validateOp checks an operation of the document and returns every problem found.
func (ex *hgGQLExec) validateOp(doc *hgGQLDoc, op *hgGQLOp) []hgGQLProblem {
	g := ex.schema()
	root := map[string]string{"query": g.query, "mutation": g.mutation, "subscription": g.subscription}[op.kind]
	if root == "" || g.types[root] == nil {
		return []hgGQLProblem{{fmt.Sprintf("This schema has no %s type.", op.kind), -1}}
	}
	v := &hgGQLValidator{g: g, vis: ex.callerSchema(), doc: doc, op: op, vars: ex.vars, seen: map[hgGQLProblem]bool{}, visiting: map[string]bool{}}
	v.sel(root, op.sel, true)
	return v.errs
}

func (v *hgGQLValidator) fail(pos int, format string, a ...any) {
	p := hgGQLProblem{fmt.Sprintf(format, a...), pos}
	if !v.seen[p] { // a fragment spread twice reports its problems once
		v.seen[p] = true
		v.errs = append(v.errs, p)
	}
}

func hgGQLBase(r *hgGQLRef) string {
	for r.wrap != "" {
		r = r.of
	}
	return r.name
}

// applies tells whether a fragment on type cond can match objects of type parent.
func (v *hgGQLValidator) applies(parent, cond string) bool {
	if parent == cond {
		return true
	}
	pt, ct := v.g.types[parent], v.g.types[cond]
	if pt == nil || ct == nil {
		return false
	}
	switch {
	case pt.kind == "UNION" && ct.kind == "OBJECT":
		return containsStr(pt.possible, cond)
	case pt.kind == "OBJECT" && ct.kind == "UNION":
		return containsStr(ct.possible, parent)
	case pt.kind == "UNION" && ct.kind == "UNION":
		for _, p := range pt.possible {
			if containsStr(ct.possible, p) {
				return true
			}
		}
	}
	return false
}

// condition checks a fragment's type condition; it returns the type to validate the fragment's
// selection against, or "" when it cannot be used.
func (v *hgGQLValidator) condition(parent, cond, frag string, pos int) string {
	if cond == "" {
		return parent
	}
	ct := v.g.types[cond]
	if ct == nil {
		v.fail(pos, "Unknown type %q.", cond)
		return ""
	}
	if ct.kind != "OBJECT" && ct.kind != "UNION" {
		if frag != "" {
			v.fail(pos, "Fragment %q cannot condition on non composite type %q.", frag, cond)
		} else {
			v.fail(pos, "Fragment cannot condition on non composite type %q.", cond)
		}
		return ""
	}
	if !v.applies(parent, cond) {
		if frag != "" {
			v.fail(pos, "Fragment %q cannot be spread here as objects of type %q can never be of type %q.", frag, parent, cond)
		} else {
			v.fail(pos, "Fragment cannot be spread here as objects of type %q can never be of type %q.", parent, cond)
		}
		return ""
	}
	return cond
}

func (v *hgGQLValidator) sel(parent string, sel []*hgGQLField, root bool) {
	v.depth++
	defer func() { v.depth-- }()
	t := v.g.types[parent]
	if t == nil || v.depth > 40 || v.nodes > hgGQLMaxNodes {
		return
	}
	for _, f := range sel {
		if v.nodes++; v.nodes > hgGQLMaxNodes {
			v.fail(-1, hgGQLTooComplex)
			return
		}
		v.directives(f.dirs)
		if f.name == "..." {
			if f.spread != "" {
				fr := v.doc.frags[f.spread]
				if fr == nil {
					v.fail(f.pos, "Unknown fragment %q.", f.spread)
					continue
				}
				if v.visiting[fr.name] {
					v.fail(f.pos, "Cannot spread fragment %q within itself.", fr.name)
					continue
				}
				on := v.condition(parent, fr.on, fr.name, f.pos)
				if on == "" {
					continue
				}
				v.visiting[fr.name] = true
				v.sel(on, fr.sel, root && on == parent)
				delete(v.visiting, fr.name)
				continue
			}
			if on := v.condition(parent, f.on, "", f.pos); on != "" {
				v.sel(on, f.sel, root && on == parent)
			}
			continue
		}
		if f.name == "__typename" || (root && (f.name == "__schema" || f.name == "__type")) {
			continue
		}
		var fd *hgGQLFieldDef
		if t.kind == "OBJECT" {
			for _, x := range t.fields {
				if x.name == f.name {
					fd = x
					break
				}
			}
		}
		if fd == nil {
			msg := fmt.Sprintf("Cannot query field %q on type %q.", f.name, parent)
			if t.kind == "OBJECT" {
				var names []string
				if vt := v.vis.types[parent]; vt != nil {
					for _, x := range vt.fields {
						names = append(names, x.name)
					}
				}
				if s := hgGQLSuggest(f.name, names); s != "" {
					msg += fmt.Sprintf(" Did you mean %q?", s)
				}
			} else if t.kind == "UNION" {
				msg += " Use an inline fragment on one of the union's types to select its fields."
			}
			v.fail(f.pos, "%s", msg)
			continue
		}
		v.arguments(f, fd, parent)
		base := hgGQLBase(fd.typ)
		bt := v.g.types[base]
		composite := bt != nil && (bt.kind == "OBJECT" || bt.kind == "UNION")
		switch {
		case composite && len(f.sel) == 0:
			v.fail(f.pos, "Field %q of type %q must have a selection of subfields. Did you mean \"%s { ... }\"?", f.name, hgGQLRefString(fd.typ), f.name)
		case !composite && len(f.sel) > 0 && base != "JSON":
			v.fail(f.pos, "Field %q must not have a selection since type %q has no subfields.", f.name, hgGQLRefString(fd.typ))
		case composite:
			v.sel(base, f.sel, false)
		}
	}
}

// arguments checks a field's arguments: known names, required ones present, values that fit.
func (v *hgGQLValidator) arguments(f *hgGQLField, fd *hgGQLFieldDef, parent string) {
	known := map[string]*hgGQLInput{}
	for _, a := range fd.args {
		known[a.name] = a
	}
	names := make([]string, 0, len(f.args))
	for k := range f.args {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		pos := f.pos
		if p, ok := f.argPos[k]; ok {
			pos = p
		}
		a := known[k]
		if a == nil {
			msg := fmt.Sprintf("Unknown argument %q on field \"%s.%s\".", k, parent, f.name)
			var argNames []string
			if vt := v.vis.types[parent]; vt != nil {
				for _, x := range vt.fields {
					if x.name == f.name {
						for _, a := range x.args {
							argNames = append(argNames, a.name)
						}
					}
				}
			}
			if s := hgGQLSuggest(k, argNames); s != "" {
				msg += fmt.Sprintf(" Did you mean %q?", s)
			}
			v.fail(pos, "%s", msg)
			continue
		}
		v.value(a.typ, f.args[k], pos, "")
	}
	for _, a := range fd.args {
		if _, ok := f.args[a.name]; !ok && a.typ.wrap == "NON_NULL" && a.def == nil {
			v.fail(f.pos, "Field %q argument %q of type %q is required, but it was not provided.", f.name, a.name, hgGQLRefString(a.typ))
		}
	}
}

// directives checks @skip / @include and their arguments.
func (v *hgGQLValidator) directives(dirs []hgGQLDir) {
	for _, d := range dirs {
		if d.name != "skip" && d.name != "include" {
			v.fail(d.pos, "Unknown directive \"@%s\".", d.name)
			continue
		}
		for k := range d.args {
			if k != "if" {
				v.fail(d.argPos[k], "Unknown argument %q on directive \"@%s\".", k, d.name)
			}
		}
		val, ok := d.args["if"]
		if !ok {
			v.fail(d.pos, "Directive \"@%s\" argument \"if\" of type \"Boolean!\" is required, but it was not provided.", d.name)
			continue
		}
		if name, isVar := val.(hgGQLVar); isVar && !v.op.defined[string(name)] {
			v.fail(d.argPos["if"], "Variable \"$%s\" is not defined.", name)
		}
	}
}

// value checks an argument value against its input type. Literal values and variable values are
// checked alike (prefix names the variable); scalar values are left to the resolvers, which accept
// the REST API's spellings (a relation takes an id, a list or {connect, disconnect, set}).
func (v *hgGQLValidator) value(typ *hgGQLRef, val any, pos int, prefix string) {
	if name, ok := val.(hgGQLVar); ok {
		if !v.op.defined[string(name)] {
			v.fail(pos, "Variable \"$%s\" is not defined.", name)
			return
		}
		if rv, ok := v.vars[string(name)]; ok && rv != nil {
			v.value(typ, rv, pos, fmt.Sprintf("Variable \"$%s\" got invalid value: ", name))
		}
		return
	}
	if typ.wrap == "NON_NULL" {
		if val == nil && prefix == "" {
			v.fail(pos, "Expected value of type %q, found null.", hgGQLRefString(typ))
			return
		}
		typ = typ.of
	}
	if val == nil {
		return
	}
	if typ.wrap == "LIST" {
		if list, ok := val.([]any); ok {
			for _, x := range list {
				v.value(typ.of, x, pos, prefix)
			}
			return
		}
		v.value(typ.of, val, pos, prefix) // a single value is a list of one
		return
	}
	t := v.g.types[typ.name]
	if t == nil {
		return
	}
	switch t.kind {
	case "INPUT_OBJECT":
		obj, ok := val.(map[string]any)
		if !ok {
			return
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var in *hgGQLInput
			for _, x := range t.inputs {
				if x.name == k {
					in = x
					break
				}
			}
			if in == nil {
				msg := fmt.Sprintf("%sField %q is not defined by type %q.", prefix, k, t.name)
				var names []string
				if vt := v.vis.types[t.name]; vt != nil {
					for _, x := range vt.inputs {
						names = append(names, x.name)
					}
				}
				if s := hgGQLSuggest(k, names); s != "" {
					msg += fmt.Sprintf(" Did you mean %q?", s)
				}
				v.fail(pos, "%s", msg)
				continue
			}
			v.value(in.typ, obj[k], pos, prefix)
		}
	case "ENUM":
		lit, ok := val.(hgGQLEnum)
		if !ok {
			return // strings are accepted as the REST API does
		}
		for _, e := range t.enum {
			if strings.EqualFold(e, string(lit)) {
				return
			}
		}
		v.fail(pos, "%sValue %q does not exist in %q enum.", prefix, string(lit), t.name)
	}
}

// hgGQLSuggest names the candidate closest to a misspelled name (at most two edits away).
func hgGQLSuggest(name string, candidates []string) string {
	best, bestD := "", 3
	for _, c := range candidates {
		if d := hgEditDistance(strings.ToLower(name), strings.ToLower(c)); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func hgEditDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
