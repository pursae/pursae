package server

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The GraphQL schema of HeadlessGo, derived from the content types, for introspection (__schema,
// __type) and the playground; and the mutations.

// hgGQLRef is a type reference: a named type, or a LIST / NON_NULL wrapper.
type hgGQLRef struct {
	wrap string // "", LIST, NON_NULL
	name string
	of   *hgGQLRef
}

func gqlNamed(n string) *hgGQLRef      { return &hgGQLRef{name: n} }
func gqlList(r *hgGQLRef) *hgGQLRef    { return &hgGQLRef{wrap: "LIST", of: r} }
func gqlNonNull(r *hgGQLRef) *hgGQLRef { return &hgGQLRef{wrap: "NON_NULL", of: r} }

type hgGQLInput struct {
	name, desc string
	typ        *hgGQLRef
	def        *string
}

type hgGQLFieldDef struct {
	name, desc string
	args       []*hgGQLInput
	typ        *hgGQLRef
}

type hgGQLTypeDef struct {
	kind, name, desc string
	fields           []*hgGQLFieldDef
	inputs           []*hgGQLInput
	enum             []string
	possible         []string
}

type hgGQLSchema struct {
	types    map[string]*hgGQLTypeDef
	query    string
	mutation string
	// subscription is the root type of subscriptions ("" when there is no content type).
	subscription string
}

func (sc *hgGQLSchema) add(t *hgGQLTypeDef) *hgGQLTypeDef {
	if _, ok := sc.types[t.name]; !ok {
		sc.types[t.name] = t
	}
	return sc.types[t.name]
}

// hgGQLScalars are the scalar types and their descriptions.
var hgGQLScalars = [][2]string{
	{"ID", "A unique identifier (a document id)"}, {"String", "UTF-8 text"}, {"Int", "A 32-bit integer"}, {"Float", "A double-precision number"},
	{"Boolean", "true or false"}, {"Long", "A large integer, returned as a string"}, {"JSON", "Any JSON value"},
	{"Date", "A date: YYYY-MM-DD"}, {"DateTime", "A date-time: RFC 3339, UTC"}, {"Time", "A time of day: HH:MM:SS.mmm"},
}

func hgUpperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// hgGQLScalar is the GraphQL scalar of a scalar field ("" for the others).
func hgGQLScalar(f *hgField) string {
	switch f.Type {
	case "text", "email", "uid", "enumeration", "password":
		return "String"
	case "richtext":
		if f.Format == "markdown" {
			return "String"
		}
		return "JSON"
	case "number":
		switch f.Format {
		case "integer":
			return "Int"
		case "biginteger":
			return "Long"
		}
		return "Float"
	case "boolean":
		return "Boolean"
	case "date":
		return "Date"
	case "datetime":
		return "DateTime"
	case "time":
		return "Time"
	case "json":
		return "JSON"
	}
	return ""
}

// schema is the GraphQL schema of the whole content model: operations are validated against it, so
// a field the caller may not use is refused as forbidden, not as unknown.
func (ex *hgGQLExec) schema() *hgGQLSchema {
	if ex.sch == nil {
		ex.sch = ex.buildSchema(nil)
	}
	return ex.sch
}

// callerSchema is the schema the caller is shown (introspection, suggestions in validation errors):
// the types it may use and what they reach (components, relation targets), as in its OpenAPI
// document. The admin's playground (no caller) sees the whole model.
func (ex *hgGQLExec) callerSchema() *hgGQLSchema {
	if ex.a == nil {
		return ex.schema()
	}
	if ex.visSch == nil {
		ex.visSch = ex.buildSchema(ex.a)
	}
	return ex.visSch
}

// buildSchema builds the GraphQL schema of the current content model as a sees it (nil: all of it).
func (ex *hgGQLExec) buildSchema(a *hgAuth) *hgGQLSchema {
	reg := ex.s.hgReg(ex.c)
	can := func(sc *hgSchema, act string) bool { return a == nil || a.can(sc.UID, act) }
	var reach map[string]bool
	if a != nil {
		reach = hgReachable(reg, ex.s.hgVisibleTypes(ex.c, a))
	}
	g := &hgGQLSchema{types: map[string]*hgGQLTypeDef{}, query: "Query", mutation: "Mutation", subscription: "Subscription"}
	for _, sc := range hgGQLScalars {
		g.add(&hgGQLTypeDef{kind: "SCALAR", name: sc[0], desc: sc[1]})
	}
	str, intT, boolT := gqlNamed("String"), gqlNamed("Int"), gqlNamed("Boolean")
	g.add(&hgGQLTypeDef{kind: "ENUM", name: "PublicationStatus", desc: "Which version of entries to read or write", enum: []string{"DRAFT", "PUBLISHED"}})
	g.add(&hgGQLTypeDef{kind: "INPUT_OBJECT", name: "PaginationArg", inputs: []*hgGQLInput{{name: "page", typ: intT}, {name: "pageSize", typ: intT}, {name: "start", typ: intT}, {name: "limit", typ: intT}}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "Pagination", fields: []*hgGQLFieldDef{{name: "total", typ: intT}, {name: "page", typ: intT}, {name: "pageSize", typ: intT},
		{name: "pageCount", typ: intT}, {name: "start", typ: intT}, {name: "limit", typ: intT}}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "DeleteMutationResponse", fields: []*hgGQLFieldDef{{name: "documentId", typ: gqlNonNull(gqlNamed("ID"))}}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "EntryReference", desc: "An entry that was unpublished or deleted", fields: []*hgGQLFieldDef{
		{name: "documentId", typ: gqlNonNull(gqlNamed("ID"))}, {name: "locale", typ: str}}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "UploadFile", desc: "A file of the media library", fields: []*hgGQLFieldDef{
		{name: "id", typ: gqlNonNull(gqlNamed("ID"))}, {name: "name", typ: str}, {name: "title", typ: str}, {name: "alternativeText", typ: str}, {name: "caption", typ: str},
		{name: "width", typ: intT}, {name: "height", typ: intT}, {name: "formats", typ: gqlNamed("JSON")}, {name: "ext", typ: str}, {name: "mime", typ: str},
		{name: "size", typ: gqlNamed("Float")}, {name: "url", typ: str}, {name: "createdAt", typ: gqlNamed("DateTime")}, {name: "updatedAt", typ: gqlNamed("DateTime")}}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "WpPost", desc: "A published WordPress post or page", fields: []*hgGQLFieldDef{
		{name: "id", typ: gqlNonNull(gqlNamed("ID"))}, {name: "type", typ: str}, {name: "slug", typ: str}, {name: "title", typ: str}, {name: "excerpt", typ: str},
		{name: "link", typ: str}, {name: "date", typ: gqlNamed("DateTime")}, {name: "modified", typ: gqlNamed("DateTime")}, {name: "featuredMedia", typ: intT}}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "WpUser", desc: "A WordPress user", fields: []*hgGQLFieldDef{
		{name: "id", typ: gqlNonNull(gqlNamed("ID"))}, {name: "username", typ: str}, {name: "name", typ: str}}})
	g.addWPTypes()
	// Scalar filter inputs.
	for _, sc := range []string{"ID", "String", "Int", "Long", "Float", "Boolean", "Date", "DateTime", "Time", "JSON"} {
		name := sc + "FilterInput"
		t := &hgGQLTypeDef{kind: "INPUT_OBJECT", name: name}
		t.inputs = append(t.inputs, &hgGQLInput{name: "and", typ: gqlList(gqlNamed(name))}, &hgGQLInput{name: "or", typ: gqlList(gqlNamed(name))}, &hgGQLInput{name: "not", typ: gqlNamed(name)})
		for _, op := range []string{"eq", "eqi", "ne", "nei", "lt", "lte", "gt", "gte", "contains", "notContains", "containsi", "notContainsi", "startsWith", "startsWithi", "endsWith", "endsWithi"} {
			t.inputs = append(t.inputs, &hgGQLInput{name: op, typ: gqlNamed(sc)})
		}
		t.inputs = append(t.inputs, &hgGQLInput{name: "in", typ: gqlList(gqlNamed(sc))}, &hgGQLInput{name: "notIn", typ: gqlList(gqlNamed(sc))},
			&hgGQLInput{name: "between", typ: gqlList(gqlNamed(sc))}, &hgGQLInput{name: "null", typ: boolT}, &hgGQLInput{name: "notNull", typ: boolT})
		g.add(t)
	}
	// Filters of what relations and media point at, as the REST API accepts them. Every object
	// filter also takes null / notNull: "has none" / "has some" for a relation, media or component.
	nullOps := func(name string) []*hgGQLInput {
		return []*hgGQLInput{{name: "and", typ: gqlList(gqlNamed(name))}, {name: "or", typ: gqlList(gqlNamed(name))}, {name: "not", typ: gqlNamed(name)},
			{name: "null", typ: boolT}, {name: "notNull", typ: boolT}}
	}
	for name, cols := range map[string][]string{"UploadFileFiltersInput": {"name", "mime", "alternativeText", "caption"},
		"WpPostFiltersInput": {"slug", "title", "status"}, "WpTermFiltersInput": {"slug", "name"}, "WpUserFiltersInput": {"username", "slug", "name"}} {
		t := &hgGQLTypeDef{kind: "INPUT_OBJECT", name: name, inputs: []*hgGQLInput{{name: "id", typ: gqlNamed("IDFilterInput")}}}
		for _, c := range cols {
			t.inputs = append(t.inputs, &hgGQLInput{name: c, typ: gqlNamed("StringFilterInput")})
		}
		t.inputs = append(t.inputs, nullOps(name)...)
		g.add(t)
	}
	g.add(&hgGQLTypeDef{kind: "INPUT_OBJECT", name: "DynamicZoneFiltersInput", desc: "Dynamic zones can only be tested for being empty",
		inputs: []*hgGQLInput{{name: "null", typ: boolT}, {name: "notNull", typ: boolT}}})
	filterName := func(uid string) string {
		switch {
		case hgIsWPPost(uid):
			return "WpPostFiltersInput"
		case uid == "wp:user":
			return "WpUserFiltersInput"
		case hgIsWPTerm(uid):
			return "WpTermFiltersInput"
		}
		if t := reg.Get(uid); t != nil {
			return hgPascal(t) + "FiltersInput"
		}
		return ""
	}
	typeName := func(uid string) string {
		switch {
		case hgIsWPPost(uid):
			return "WpPost"
		case uid == "wp:user":
			return "WpUser"
		case hgIsWPTerm(uid):
			return "WpTerm"
		}
		if t := reg.Get(uid); t != nil {
			return hgPascal(t)
		}
		return "JSON"
	}
	// Object, filter and input types of the content types and components.
	all := append(reg.Components(), reg.Types()...)
	for _, sc := range all {
		if reach != nil && !reach[sc.UID] {
			continue
		}
		obj := &hgGQLTypeDef{kind: "OBJECT", name: hgPascal(sc), desc: sc.Description}
		filt := &hgGQLTypeDef{kind: "INPUT_OBJECT", name: hgPascal(sc) + "FiltersInput"}
		in := &hgGQLTypeDef{kind: "INPUT_OBJECT", name: hgPascal(sc) + "Input"}
		if sc.IsComponent() {
			obj.fields = append(obj.fields, &hgGQLFieldDef{name: "id", typ: gqlNamed("ID")})
			filt.inputs = append(filt.inputs, &hgGQLInput{name: "id", typ: gqlNamed("IDFilterInput")})
		} else {
			obj.fields = append(obj.fields, &hgGQLFieldDef{name: "documentId", typ: gqlNonNull(gqlNamed("ID"))})
			filt.inputs = append(filt.inputs, &hgGQLInput{name: "id", typ: gqlNamed("IDFilterInput")}, &hgGQLInput{name: "documentId", typ: gqlNamed("IDFilterInput")})
		}
		for i := range sc.Fields {
			f := &sc.Fields[i]
			if f.Private || f.Type == "password" { // writable, never returned
				in.inputs = append(in.inputs, &hgGQLInput{name: f.Name, typ: gqlNamed(defaultStr(hgGQLScalar(f), "JSON"))})
				continue
			}
			if s := hgGQLScalar(f); s != "" {
				obj.fields = append(obj.fields, &hgGQLFieldDef{name: f.Name, typ: gqlNamed(s)})
				filt.inputs = append(filt.inputs, &hgGQLInput{name: f.Name, typ: gqlNamed(s + "FilterInput")})
				in.inputs = append(in.inputs, &hgGQLInput{name: f.Name, typ: gqlNamed(s)})
				continue
			}
			var ref *hgGQLRef
			var args []*hgGQLInput
			switch f.Type {
			case "media":
				ref = gqlNamed("UploadFile")
				filt.inputs = append(filt.inputs, &hgGQLInput{name: f.Name, typ: gqlNamed("UploadFileFiltersInput")})
				in.inputs = append(in.inputs, &hgGQLInput{name: f.Name, typ: hgGQLMaybeList(f, gqlNamed("ID"))})
			case "relation":
				ref = gqlNamed(typeName(f.Target))
				if fn := filterName(f.Target); fn != "" {
					filt.inputs = append(filt.inputs, &hgGQLInput{name: f.Name, typ: gqlNamed(fn)})
				}
				if t := reg.Get(f.Target); t != nil {
					if f.multi() {
						args = []*hgGQLInput{{name: "filters", typ: gqlNamed(hgPascal(t) + "FiltersInput")}, {name: "sort", typ: gqlList(str)}}
					}
				}
				in.inputs = append(in.inputs, &hgGQLInput{name: f.Name, typ: hgGQLMaybeList(f, gqlNamed("ID"))})
			case "component":
				if c := reg.Get(f.Component); c != nil {
					ref = gqlNamed(hgPascal(c))
					filt.inputs = append(filt.inputs, &hgGQLInput{name: f.Name, typ: gqlNamed(hgPascal(c) + "FiltersInput")})
				}
				in.inputs = append(in.inputs, &hgGQLInput{name: f.Name, typ: hgGQLMaybeList(f, gqlNamed("JSON"))})
			case "dynamiczone":
				u := &hgGQLTypeDef{kind: "UNION", name: hgPascal(sc) + hgUpperFirst(f.Name) + "DynamicZone"}
				for _, cu := range f.Components {
					if c := reg.Get(cu); c != nil {
						u.possible = append(u.possible, hgPascal(c))
					}
				}
				g.add(u)
				ref = gqlNamed(u.name)
				filt.inputs = append(filt.inputs, &hgGQLInput{name: f.Name, typ: gqlNamed("DynamicZoneFiltersInput")})
				in.inputs = append(in.inputs, &hgGQLInput{name: f.Name, typ: gqlList(gqlNamed("JSON"))})
			}
			if ref == nil {
				continue
			}
			if f.multi() {
				ref = gqlList(ref)
			}
			obj.fields = append(obj.fields, &hgGQLFieldDef{name: f.Name, typ: ref, args: args})
		}
		if !sc.IsComponent() {
			dt := gqlNamed("DateTime")
			obj.fields = append(obj.fields, &hgGQLFieldDef{name: "createdAt", typ: dt}, &hgGQLFieldDef{name: "updatedAt", typ: dt}, &hgGQLFieldDef{name: "publishedAt", typ: dt})
			for _, n := range []string{"createdAt", "updatedAt", "publishedAt"} {
				filt.inputs = append(filt.inputs, &hgGQLInput{name: n, typ: gqlNamed("DateTimeFilterInput")})
			}
			if sc.Localized {
				obj.fields = append(obj.fields, &hgGQLFieldDef{name: "locale", typ: str}, &hgGQLFieldDef{name: "localizations", typ: gqlList(gqlNamed(hgPascal(sc)))})
				filt.inputs = append(filt.inputs, &hgGQLInput{name: "locale", typ: gqlNamed("StringFilterInput")})
			}
		}
		filt.inputs = append(filt.inputs, nullOps(filt.name)...)
		g.add(obj)
		g.add(filt)
		if !sc.IsComponent() {
			g.add(in)
		}
	}
	// Root types.
	q := &hgGQLTypeDef{kind: "OBJECT", name: "Query", desc: "Pursae Headless content"}
	m := &hgGQLTypeDef{kind: "OBJECT", name: "Mutation", desc: "Create, update, delete, publish and unpublish entries"}
	status := gqlNamed("PublicationStatus")
	sub := &hgGQLTypeDef{kind: "OBJECT", name: "Subscription", desc: "Live changes of entries, delivered as server-sent events or over a WebSocket (graphql-transport-ws)"}
	for _, sc := range reg.Types() {
		if reach != nil && !reach[sc.UID] {
			continue
		}
		p := hgPascal(sc)
		subArgs := []*hgGQLInput{{name: "locale", typ: str}}
		if sc.IsCollection() {
			subArgs = append([]*hgGQLInput{{name: "documentId", typ: gqlNamed("ID")}}, subArgs...)
		}
		// The rights hgGQLSubscribe asks for.
		subOK := a == nil || (a.Kind != "preview" && can(sc, map[bool]string{true: "find", false: "findOne"}[sc.IsSingle()]))
		for _, ev := range hgSubEvents {
			if !subOK || (ev.dp && !sc.DraftAndPublish) {
				continue
			}
			typ := gqlNamed(p)
			if ev.ref {
				typ = gqlNonNull(gqlNamed("EntryReference"))
			}
			sub.fields = append(sub.fields, &hgGQLFieldDef{name: hgCamel(sc.SingularName) + ev.suffix, desc: "An entry of " + sc.DisplayName + " was " + strings.ToLower(ev.suffix), args: subArgs, typ: typ})
		}
		read := []*hgGQLInput{{name: "locale", typ: str}, {name: "status", typ: status}}
		write := []*hgGQLInput{{name: "data", typ: gqlNonNull(gqlNamed(p + "Input"))}, {name: "locale", typ: str}, {name: "status", typ: status}}
		var docArgs []*hgGQLInput // the documentId argument of a collection's entry
		if sc.IsCollection() {
			docArgs = []*hgGQLInput{{name: "documentId", typ: gqlNonNull(gqlNamed("ID"))}}
		}
		locArg := append(append([]*hgGQLInput(nil), docArgs...), &hgGQLInput{name: "locale", typ: str})
		if sc.IsCollection() {
			list := append([]*hgGQLInput{{name: "filters", typ: gqlNamed(p + "FiltersInput")}, {name: "sort", typ: gqlList(str)},
				{name: "pagination", typ: gqlNamed("PaginationArg")}}, read...)
			if can(sc, "find") {
				conn := g.add(&hgGQLTypeDef{kind: "OBJECT", name: p + "EntityResponseCollection", fields: []*hgGQLFieldDef{
					{name: "nodes", typ: gqlNonNull(gqlList(gqlNonNull(gqlNamed(p))))}, {name: "pageInfo", typ: gqlNonNull(gqlNamed("Pagination"))}}})
				q.fields = append(q.fields, &hgGQLFieldDef{name: hgCamel(sc.PluralName), desc: "Entries of " + sc.DisplayName, args: list, typ: gqlNonNull(gqlList(gqlNamed(p)))},
					&hgGQLFieldDef{name: hgCamel(sc.PluralName) + "_connection", args: list, typ: gqlNamed(conn.name)})
			}
			if can(sc, "findOne") {
				q.fields = append(q.fields, &hgGQLFieldDef{name: hgCamel(sc.SingularName), desc: "One entry of " + sc.DisplayName, args: append(append([]*hgGQLInput(nil), docArgs...), read...), typ: gqlNamed(p)})
			}
			if can(sc, "create") {
				m.fields = append(m.fields, &hgGQLFieldDef{name: "create" + p, args: write, typ: gqlNamed(p)})
			}
		} else if can(sc, "find") {
			q.fields = append(q.fields, &hgGQLFieldDef{name: hgCamel(sc.SingularName), desc: sc.DisplayName, args: read, typ: gqlNamed(p)})
		}
		if can(sc, "update") {
			m.fields = append(m.fields, &hgGQLFieldDef{name: "update" + p, args: append(append([]*hgGQLInput(nil), docArgs...), write...), typ: gqlNamed(p)})
		}
		if can(sc, "delete") {
			m.fields = append(m.fields, &hgGQLFieldDef{name: "delete" + p, args: locArg, typ: gqlNamed("DeleteMutationResponse")})
		}
		if sc.DraftAndPublish && can(sc, "update") {
			m.fields = append(m.fields, &hgGQLFieldDef{name: "publish" + p, args: locArg, typ: gqlNamed(p)},
				&hgGQLFieldDef{name: "unpublish" + p, args: locArg, typ: gqlNamed(p)})
		}
	}
	q.fields = append(q.fields, g.wpQueryFields()...)
	if len(q.fields) == 0 {
		q.fields = append(q.fields, &hgGQLFieldDef{name: "_empty", desc: "No content types yet", typ: str})
	}
	g.add(q)
	if len(m.fields) > 0 {
		g.add(m)
	} else {
		g.mutation = ""
	}
	if len(sub.fields) > 0 {
		g.add(sub)
	} else {
		g.subscription = ""
	}
	return g
}

// hgReachable is the set of type and component uids the given types reach through components,
// dynamic zones and relations (the types themselves included).
func hgReachable(reg *hgRegistry, roots []*hgSchema) map[string]bool {
	out := map[string]bool{}
	var walk func(sc *hgSchema)
	walk = func(sc *hgSchema) {
		if sc == nil || out[sc.UID] {
			return
		}
		out[sc.UID] = true
		for _, f := range sc.Fields {
			switch f.Type {
			case "component":
				walk(reg.Get(f.Component))
			case "dynamiczone":
				for _, uid := range f.Components {
					walk(reg.Get(uid))
				}
			case "relation":
				walk(reg.Get(f.Target))
			}
		}
	}
	for _, sc := range roots {
		walk(sc)
	}
	return out
}

func hgGQLMaybeList(f *hgField, r *hgGQLRef) *hgGQLRef {
	if f.multi() {
		return gqlList(r)
	}
	return r
}

// Introspection objects -----------------------------------------------------------------------------

// hgIntro is an introspection object: its __typename and a resolver of its fields.
type hgIntro struct {
	typename string
	get      func(field string, args map[string]any) any
}

// intro projects introspection values by a selection set.
func (ex *hgGQLExec) intro(v any, sel []*hgGQLField) any {
	switch t := v.(type) {
	case *hgIntro:
		if t == nil {
			return nil
		}
		out := map[string]any{}
		for _, f := range hgGQLFlat(sel) {
			if f.name == "__typename" {
				out[f.key()] = t.typename
				continue
			}
			args := map[string]any{}
			for k, a := range f.args {
				args[k] = ex.resolve(a)
			}
			out[f.key()] = ex.intro(t.get(f.name, args), f.sel)
		}
		return out
	case []*hgIntro:
		if t == nil {
			return nil
		}
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, ex.intro(x, sel))
		}
		return out
	}
	return v
}

func (g *hgGQLSchema) schemaObj() *hgIntro {
	return &hgIntro{typename: "__Schema", get: func(field string, _ map[string]any) any {
		switch field {
		case "queryType":
			return g.typeObj(g.query)
		case "mutationType":
			if g.mutation == "" {
				return (*hgIntro)(nil)
			}
			return g.typeObj(g.mutation)
		case "subscriptionType":
			if g.subscription == "" {
				return (*hgIntro)(nil)
			}
			return g.typeObj(g.subscription)
		case "description":
			return nil
		case "types":
			names := make([]string, 0, len(g.types))
			for n := range g.types {
				names = append(names, n)
			}
			sort.Strings(names)
			out := []*hgIntro{}
			for _, n := range names {
				out = append(out, g.typeObj(n))
			}
			return out
		case "directives":
			cond := &hgGQLInput{name: "if", typ: gqlNonNull(gqlNamed("Boolean"))}
			out := []*hgIntro{}
			for _, d := range [][2]string{{"include", "Include this field or fragment only when the argument is true"}, {"skip", "Skip this field or fragment when the argument is true"}} {
				out = append(out, &hgIntro{typename: "__Directive", get: func(field string, _ map[string]any) any {
					switch field {
					case "name":
						return d[0]
					case "description":
						return d[1]
					case "locations":
						return []any{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}
					case "args":
						return []*hgIntro{g.inputObj(cond)}
					case "isRepeatable":
						return false
					}
					return nil
				}})
			}
			return out
		}
		return nil
	}}
}

// typeObj is a named type's __Type (nil when unknown).
func (g *hgGQLSchema) typeObj(name string) *hgIntro {
	t := g.types[name]
	if t == nil {
		return nil
	}
	return &hgIntro{typename: "__Type", get: func(field string, _ map[string]any) any {
		switch field {
		case "kind":
			return t.kind
		case "name":
			return t.name
		case "description":
			if t.desc == "" {
				return nil
			}
			return t.desc
		case "fields":
			if t.kind != "OBJECT" && t.kind != "INTERFACE" {
				return []*hgIntro(nil)
			}
			out := []*hgIntro{}
			for _, f := range t.fields {
				out = append(out, g.fieldObj(f))
			}
			return out
		case "inputFields":
			if t.kind != "INPUT_OBJECT" {
				return []*hgIntro(nil)
			}
			out := []*hgIntro{}
			for _, in := range t.inputs {
				out = append(out, g.inputObj(in))
			}
			return out
		case "interfaces":
			if t.kind != "OBJECT" {
				return []*hgIntro(nil)
			}
			return []*hgIntro{}
		case "possibleTypes":
			if t.kind != "UNION" {
				return []*hgIntro(nil)
			}
			out := []*hgIntro{}
			for _, p := range t.possible {
				out = append(out, g.typeObj(p))
			}
			return out
		case "enumValues":
			if t.kind != "ENUM" {
				return []*hgIntro(nil)
			}
			out := []*hgIntro{}
			for _, v := range t.enum {
				out = append(out, &hgIntro{typename: "__EnumValue", get: func(field string, _ map[string]any) any {
					switch field {
					case "name":
						return v
					case "isDeprecated":
						return false
					}
					return nil
				}})
			}
			return out
		case "isOneOf":
			return false
		}
		return nil // ofType, specifiedByURL
	}}
}

func (g *hgGQLSchema) refObj(r *hgGQLRef) *hgIntro {
	if r.wrap == "" {
		return g.typeObj(r.name)
	}
	return &hgIntro{typename: "__Type", get: func(field string, _ map[string]any) any {
		switch field {
		case "kind":
			return r.wrap
		case "ofType":
			return g.refObj(r.of)
		case "fields", "inputFields", "interfaces", "possibleTypes", "enumValues":
			return []*hgIntro(nil)
		}
		return nil
	}}
}

func (g *hgGQLSchema) fieldObj(f *hgGQLFieldDef) *hgIntro {
	return &hgIntro{typename: "__Field", get: func(field string, _ map[string]any) any {
		switch field {
		case "name":
			return f.name
		case "description":
			if f.desc == "" {
				return nil
			}
			return f.desc
		case "args":
			out := []*hgIntro{}
			for _, a := range f.args {
				out = append(out, g.inputObj(a))
			}
			return out
		case "type":
			return g.refObj(f.typ)
		case "isDeprecated":
			return false
		}
		return nil
	}}
}

func (g *hgGQLSchema) inputObj(in *hgGQLInput) *hgIntro {
	return &hgIntro{typename: "__InputValue", get: func(field string, _ map[string]any) any {
		switch field {
		case "name":
			return in.name
		case "description":
			if in.desc == "" {
				return nil
			}
			return in.desc
		case "type":
			return g.refObj(in.typ)
		case "defaultValue":
			if in.def == nil {
				return nil
			}
			return *in.def
		case "isDeprecated":
			return false
		}
		return nil
	}}
}

// hgGQLRefString prints a type reference in SDL notation ([Article!]!).
func hgGQLRefString(r *hgGQLRef) string {
	switch r.wrap {
	case "LIST":
		return "[" + hgGQLRefString(r.of) + "]"
	case "NON_NULL":
		return hgGQLRefString(r.of) + "!"
	}
	return r.name
}

// RootFields lists the root fields of the playground's reference: "name(args): Type".
func (g *hgGQLSchema) RootFields(root string) []string {
	t := g.types[root]
	if t == nil {
		return nil
	}
	var out []string
	for _, f := range t.fields {
		var args []string
		for _, a := range f.args {
			args = append(args, a.name+": "+hgGQLRefString(a.typ))
		}
		sig := f.name
		if len(args) > 0 {
			sig += "(" + strings.Join(args, ", ") + ")"
		}
		out = append(out, sig+": "+hgGQLRefString(f.typ))
	}
	return out
}

// Mutations -------------------------------------------------------------------------------------------

func (ex *hgGQLExec) mutation(f *hgGQLField) (any, error) {
	if f.name == "__typename" {
		return "Mutation", nil
	}
	if ex.a.Kind == "preview" {
		return nil, fmt.Errorf("Forbidden access")
	}
	args := map[string]any{}
	for k, v := range f.args {
		args[k] = ex.resolve(v)
	}
	for _, sc := range ex.s.hgReg(ex.c).Types() {
		p := hgPascal(sc)
		var op string
		for _, o := range []string{"create", "update", "delete", "publish", "unpublish"} {
			if f.name == o+p {
				op = o
			}
		}
		if op == "" || (op == "create" && !sc.IsCollection()) || ((op == "publish" || op == "unpublish") && !sc.DraftAndPublish) {
			continue
		}
		perm := op
		if op == "publish" || op == "unpublish" {
			perm = "update"
		}
		if !ex.a.can(sc.UID, perm) {
			return nil, fmt.Errorf("Forbidden access")
		}
		doc := ""
		if sc.IsSingle() {
			doc = ex.s.hgSingleDoc(ex.c, sc.UID)
		} else if op != "create" {
			doc = hgGQLString(args["documentId"])
			if doc == "" {
				return nil, fmt.Errorf("The argument documentId is required")
			}
		}
		locArg, _ := args["locale"].(string)
		loc, err := ex.s.hgEntryLocale(ex.c, sc, locArg)
		if err != nil {
			return nil, err
		}
		if op != "create" && !ex.s.hgOwnerAllows(ex.c, ex.a, sc, doc, map[bool]string{true: "delete", false: "update"}[op == "delete"]) {
			return nil, fmt.Errorf("Forbidden access")
		}
		status := "draft"
		if st, _ := args["status"].(string); strings.EqualFold(st, "published") || !sc.DraftAndPublish {
			status = "published"
		}
		var e *hgEntry
		switch op {
		case "create", "update":
			data, ok := args["data"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("The argument data is required")
			}
			var events []string
			e, events, err = ex.s.hgSaveEntry(ex.c, sc, doc, data, hgSaveOpts{Locale: loc, Publish: status == "published", Auth: ex.a, UserID: ex.a.userID()})
			if err != nil {
				return nil, hgGQLWriteError(err)
			}
			for _, ev := range events {
				ex.s.hgEmit(ex.c, ev, sc, e)
			}
		case "delete":
			if doc == "" {
				return nil, fmt.Errorf("Not Found")
			}
			target := "*"
			if sc.Localized && locArg != "" && locArg != "*" {
				target = loc
			} else if !sc.Localized {
				target = ""
			}
			deleted, err := ex.s.hgDeleteEntries(ex.c, sc, doc, target, 0)
			if err != nil {
				return nil, hgGQLWriteError(err)
			}
			for _, d := range deleted {
				ex.s.hgEmit(ex.c, "entry.delete", sc, d)
			}
			return ex.intro(&hgIntro{typename: "DeleteMutationResponse", get: func(field string, _ map[string]any) any {
				if field == "documentId" {
					return doc
				}
				return nil
			}}, f.sel), nil
		case "publish":
			e, err = ex.s.hgPublishEntry(ex.c, sc, doc, loc, ex.a.userID())
			if err != nil {
				return nil, hgGQLWriteError(err)
			}
			status = "published"
			ex.s.hgEmit(ex.c, "entry.publish", sc, e)
		case "unpublish":
			e, err = ex.s.hgUnpublishEntry(ex.c, sc, doc, loc)
			if err != nil {
				return nil, hgGQLWriteError(err)
			}
			status = "draft"
			ex.s.hgEmit(ex.c, "entry.unpublish", sc, e)
		}
		pop, err := ex.populate(sc, f.sel)
		if err != nil {
			return nil, err
		}
		rp := &hgReadParams{Auth: ex.a, Status: status, Locale: loc, Populate: pop, Budget: ex.readBudget()}
		var fields []string
		if status == "draft" && (op == "unpublish" || (op == "update" && doc != "")) {
			written, _ := args["data"].(map[string]any)
			fields, rp.Populate = hgWriteView(ex.a, sc, written, rp.Populate)
		}
		obj, err := ex.s.hgSerializer(ex.c, ex.a, rp).entry(sc, e, rp.Populate, fields)
		if err != nil {
			return nil, err
		}
		return ex.project(sc, obj, f.sel), nil
	}
	return nil, fmt.Errorf("Cannot query field %q on type \"Mutation\"", f.name)
}

// hgGQLWriteError turns a save error into a GraphQL error message.
func hgGQLWriteError(err error) error {
	var ve hgValidationError
	if errors.As(err, &ve) {
		return fmt.Errorf("Validation error: %s", ve.Error())
	}
	if errors.Is(err, errHGNotFound) {
		return fmt.Errorf("Not Found")
	}
	return err
}
