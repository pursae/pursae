package server

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// REST API: the "acf" object of posts, pages, ACF post types, users and terms (field groups with
// "Show in REST API" on), writable on posts. ?acf_format=standard gives the formatted values,
// the default ("light") the stored ones with IDs as numbers — ACF's two REST formats.

// acfgoRESTFields lists the fields of REST-enabled groups shown for an object.
func (s *Server) acfgoRESTFields(ctx context.Context, sc *acfScreen) []acfField {
	var out []acfField
	for _, g := range s.acfgoAllGroups(ctx, false) {
		if g.ShowInREST && s.acfgoMatchGroup(g, sc) {
			out = append(out, g.Fields...)
		}
	}
	return out
}

func (s *Server) acfgoRESTValues(c *gin.Context, o *acfObject, fields []acfField) map[string]any {
	ctx := c.Request.Context()
	standard := c.Query("acf_format") == "standard"
	if standard { // formatted related posts and users are filtered for the requester (acfgoRESTViewerFrom)
		ctx = context.WithValue(ctx, acfgoRESTViewerKey{}, &acfgoRESTViewer{u: restCurrent(c)})
	}
	out := map[string]any{}
	for _, f := range fields {
		if standard {
			s.acfgoLoadInto(ctx, o, out, f, "", true)
			continue
		}
		tmp := map[string]any{}
		s.acfgoLoadInto(ctx, o, tmp, f, "", false)
		for k, v := range tmp {
			out[k] = s.acfgoLight(ctx, f, k, v)
		}
	}
	return out
}

// acfgoRESTViewerKey marks a context that formats values for a REST response (?acf_format=standard). Unlike
// get_field() in a theme, related posts the requester may not read and private user columns are left out there,
// as the core REST API does.
type acfgoRESTViewerKey struct{}

type acfgoRESTViewer struct{ u *store.User } // u is nil for an anonymous request

func acfgoRESTViewerFrom(ctx context.Context) *acfgoRESTViewer {
	v, _ := ctx.Value(acfgoRESTViewerKey{}).(*acfgoRESTViewer)
	return v
}

// canReadPost is the core REST rule (rest_posts.go): published posts, or read_post on the post.
func (v *acfgoRESTViewer) canReadPost(p *store.Post) bool {
	return p.Status == "publish" || (v.u != nil && v.u.Can("read_post", p))
}

// acfgoLight is ACF's "light" REST format of a stored value: numbers, booleans and IDs typed.
func (s *Server) acfgoLight(ctx context.Context, f acfField, name string, v any) any {
	typ := acfgoStr(f["type"])
	if typ == "clone" && acfgoStr(f["display"]) != "group" {
		for _, cf := range s.acfgoCloneFields(ctx, f) {
			if acfgoStr(cf["name"]) == name {
				return s.acfgoLight(ctx, cf, name, v)
			}
		}
		return v
	}
	ids := func(x any) any {
		var out []any
		for _, i := range acfgoList(x) {
			if n := acfgoInt(i); n > 0 {
				out = append(out, n)
			}
		}
		if out == nil {
			return nil
		}
		return out
	}
	rows := func(subsFor func(row map[string]any) []acfField) any {
		var out []any
		for _, r := range acfgoList(v) {
			row, _ := r.(map[string]any)
			nr := map[string]any{}
			for k, x := range row {
				nr[k] = x
				for _, sf := range subsFor(row) {
					if acfgoStr(sf["name"]) == k || acfgoStr(sf["type"]) == "clone" {
						nr[k] = s.acfgoLight(ctx, sf, k, x)
					}
				}
			}
			out = append(out, nr)
		}
		if out == nil {
			return []any{}
		}
		return out
	}
	switch typ {
	case "true_false":
		return acfgoBool(v)
	case "number", "range":
		return s.acfgoFormat(ctx, f, v)
	case "image", "file":
		if n := acfgoInt(v); n > 0 {
			return n
		}
		return nil
	case "gallery", "relationship":
		return ids(v)
	case "post_object", "user", "taxonomy":
		l := ids(v)
		if typ == "taxonomy" {
			if ft := acfgoStr(f["field_type"]); ft != "radio" && ft != "select" {
				return l
			}
		} else if acfgoBool(f["multiple"]) {
			return l
		}
		if ll, ok := l.([]any); ok && len(ll) > 0 {
			return ll[0]
		}
		return nil
	case "repeater":
		return rows(func(map[string]any) []acfField { return acfgoSubFields(f) })
	case "flexible_content":
		return rows(func(row map[string]any) []acfField {
			return acfgoLayoutSubs(acfgoLayout(f, acfgoStr(row["acf_fc_layout"])))
		})
	case "group", "clone":
		m, _ := v.(map[string]any)
		subs := acfgoSubFields(f)
		if typ == "clone" {
			subs = s.acfgoCloneFields(ctx, f)
		}
		out := map[string]any{}
		for k, x := range m {
			out[k] = x
			for _, sf := range subs {
				if acfgoStr(sf["name"]) == k {
					out[k] = s.acfgoLight(ctx, sf, k, x)
				}
			}
		}
		return out
	}
	if v == "" {
		return nil
	}
	return v
}

// acfgoRESTPost adds "acf" to a post's REST response (restPost hook).
func (s *Server) acfgoRESTPost(c *gin.Context, p *store.Post, out map[string]any) {
	if !s.acfgoOn() || strings.HasPrefix(p.Type, "acf-") {
		return
	}
	ctx := c.Request.Context()
	fields := s.acfgoRESTFields(ctx, s.acfgoPostScreen(ctx, p, restCurrent(c)))
	if len(fields) == 0 {
		return
	}
	out["acf"] = s.acfgoRESTValues(c, s.acfgoPostObject(p), fields)
}

// acfgoRESTTerm adds "acf" to a term's REST response.
func (s *Server) acfgoRESTTerm(c *gin.Context, t *store.Term, out map[string]any) {
	if !s.acfgoOn() {
		return
	}
	fields := s.acfgoRESTFields(c.Request.Context(), &acfScreen{Taxonomy: t.Taxonomy})
	if len(fields) == 0 {
		return
	}
	out["acf"] = s.acfgoRESTValues(c, &acfObject{Kind: "term", ID: t.ID, meta: orEmptyMap(t.Meta)}, fields)
}

// acfgoRESTUser adds "acf" to a user's REST response.
func (s *Server) acfgoRESTUser(c *gin.Context, u *store.User, out map[string]any) {
	if !s.acfgoOn() {
		return
	}
	fields := s.acfgoRESTFields(c.Request.Context(), &acfScreen{UserForm: "edit", UserRoles: u.Roles, UserID: u.ID})
	if len(fields) == 0 {
		return
	}
	out["acf"] = s.acfgoRESTValues(c, &acfObject{Kind: "user", ID: u.ID, meta: u.Meta}, fields)
}

// acfgoRESTInput maps a REST "acf" object (by field name) to field-key input for a post screen.
// u is the requester: groups located by current user or role apply to their writes as on the edit screen.
func (s *Server) acfgoRESTInput(ctx context.Context, raw any, p *store.Post, u *store.User) (map[string]any, []string) {
	return s.acfgoRESTInputFor(ctx, raw, s.acfgoPostScreen(ctx, p, u))
}

// acfgoRESTInputFor is acfgoRESTInput for any object: screen says which field groups apply to it.
func (s *Server) acfgoRESTInputFor(ctx context.Context, raw any, screen *acfScreen) (map[string]any, []string) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, nil
	}
	fields := s.acfgoRESTFields(ctx, screen)
	in := map[string]any{}
	var unknown []string
	for name, v := range m {
		found := false
		for _, f := range fields {
			if acfgoStr(f["name"]) == name || acfgoStr(f["key"]) == name {
				in[acfgoStr(f["key"])] = acfgoKeyed(ctx, s, f, v)
				found = true
				break
			}
		}
		if !found {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	return in, unknown
}

// acfgoKeyed converts name-keyed REST input of container fields into field-key input.
func acfgoKeyed(ctx context.Context, s *Server, f acfField, v any) any {
	byName := func(subs []acfField, row map[string]any) map[string]any {
		out := map[string]any{}
		for k, x := range row {
			if k == "acf_fc_layout" {
				out[k] = x
				continue
			}
			for _, sf := range subs {
				if acfgoStr(sf["name"]) == k || acfgoStr(sf["key"]) == k {
					out[acfgoStr(sf["key"])] = acfgoKeyed(ctx, s, sf, x)
				}
			}
		}
		return out
	}
	switch acfgoStr(f["type"]) {
	case "repeater", "flexible_content":
		var rows []any
		for _, r := range acfgoRows(v) {
			subs := acfgoSubFields(f)
			if acfgoStr(f["type"]) == "flexible_content" {
				subs = acfgoLayoutSubs(acfgoLayout(f, acfgoStr(r["acf_fc_layout"])))
			}
			rows = append(rows, byName(subs, r))
		}
		if rows == nil {
			return []any{}
		}
		return rows
	case "group":
		m, _ := v.(map[string]any)
		return byName(acfgoSubFields(f), m)
	case "clone":
		m, _ := v.(map[string]any)
		return byName(s.acfgoCloneFields(ctx, f), m)
	}
	return v
}

// acfgoRESTValidate checks the "acf" object of a REST write before the post is saved; false
// means an error response was sent.
func (s *Server) acfgoRESTValidate(c *gin.Context, body map[string]any, p *store.Post) bool {
	return s.acfgoRESTValidateFor(c, body, s.acfgoPostScreen(c.Request.Context(), p, restCurrent(c)))
}

// acfgoRESTValidateFor is acfgoRESTValidate for any object (user, term, comment, attachment).
func (s *Server) acfgoRESTValidateFor(c *gin.Context, body map[string]any, screen *acfScreen) bool {
	raw, ok := body["acf"]
	if !ok || !s.acfgoOn() {
		return true
	}
	in, unknown := s.acfgoRESTInputFor(c.Request.Context(), raw, screen)
	if len(unknown) > 0 {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): acf ("+strings.Join(unknown, ", ")+" is not a field of this object)")
		return false
	}
	if errs := s.acfgoValidate(c.Request.Context(), in); len(errs) > 0 {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): acf ("+errs[0].Message+")")
		return false
	}
	return true
}

// acfgoRESTSave stores the "acf" object of a REST write after the post is saved.
func (s *Server) acfgoRESTSave(c *gin.Context, body map[string]any, p *store.Post) {
	raw, ok := body["acf"]
	if !ok || !s.acfgoOn() {
		return
	}
	u := restCurrent(c)
	in, _ := s.acfgoRESTInput(c.Request.Context(), raw, p, u)
	_ = s.acfgoSaveInput(acfgoWithEditor(c.Request.Context(), u), s.acfgoPostObject(p), in, u != nil && u.Can("unfiltered_html", nil))
}

// acfgoRESTSaveFor stores the "acf" object of a REST write on a user, term, comment or attachment.
func (s *Server) acfgoRESTSaveFor(c *gin.Context, body map[string]any, screen *acfScreen, o *acfObject) {
	raw, ok := body["acf"]
	if !ok || !s.acfgoOn() || o == nil {
		return
	}
	in, _ := s.acfgoRESTInputFor(c.Request.Context(), raw, screen)
	u := restCurrent(c)
	_ = s.acfgoSaveInput(acfgoWithEditor(c.Request.Context(), u), o, in, u != nil && u.Can("unfiltered_html", nil))
}

// acfgoRESTComment adds "acf" to a comment's REST response.
func (s *Server) acfgoRESTComment(c *gin.Context, cm *store.Comment, p *store.Post, out map[string]any) {
	if !s.acfgoOn() {
		return
	}
	fields := s.acfgoRESTFields(c.Request.Context(), acfgoCommentScreen(p))
	if len(fields) == 0 {
		return
	}
	out["acf"] = s.acfgoRESTValues(c, &acfObject{Kind: "comment", ID: cm.ID, meta: orEmptyMap(cm.Meta)}, fields)
}

// acfgoRESTMedia adds "acf" to a media item's REST response.
func (s *Server) acfgoRESTMedia(c *gin.Context, a *store.Attachment, out map[string]any) {
	if !s.acfgoOn() {
		return
	}
	fields := s.acfgoRESTFields(c.Request.Context(), &acfScreen{Attachment: a.Mime})
	if len(fields) == 0 {
		return
	}
	out["acf"] = s.acfgoRESTValues(c, &acfObject{Kind: "attachment", ID: a.ID, meta: orEmptyMap(a.Meta)}, fields)
}

// acfgoCommentScreen is the comment edit screen of a comment on post p (the rule value is the post type).
func acfgoCommentScreen(p *store.Post) *acfScreen {
	if p == nil {
		return &acfScreen{Comment: "all"}
	}
	return &acfScreen{Comment: p.Type}
}

// Template functions --------------------------------------------------------------------------------

// acfgoRow is one row of have_rows: a repeater or flexible content row (or a group).
type acfgoRow struct {
	Index  int    // get_row_index(): 1-based
	Layout string // get_row_layout() of flexible content
	Values map[string]any
}

func (r acfgoRow) Sub(name string) any { return r.Values[name] }

// acfgoTemplateFuncs are ACF's template functions for themes. The post comes from the template
// ("." in a loop, .Post, an ID, "option", "user_1", "term_5"); rows are ranged over:
//
//	{{range have_rows "slides" .Post}}{{get_sub_field "title" .}}{{end}}
func (s *Server) acfgoTemplateFuncs() template.FuncMap {
	ctx := context.Background
	target := func(args []any) any {
		if len(args) == 0 {
			return nil
		}
		return args[0]
	}
	format := func(args []any) bool {
		if len(args) > 1 {
			if b, ok := args[1].(bool); ok {
				return b
			}
		}
		return true
	}
	rowsOf := func(v any) []acfgoRow {
		var out []acfgoRow
		switch t := v.(type) {
		case []any:
			for i, r := range t {
				m, _ := r.(map[string]any)
				out = append(out, acfgoRow{Index: i + 1, Layout: acfgoStr(m["acf_fc_layout"]), Values: m})
			}
		case map[string]any: // group: one row
			out = append(out, acfgoRow{Index: 1, Values: t})
		}
		return out
	}
	get := func(name string, args ...any) any {
		if r, ok := target(args).(acfgoRow); ok {
			return r.Values[name]
		}
		return s.acfGetField(ctx(), name, target(args), format(args))
	}
	the := func(v any) template.HTML {
		switch t := v.(type) {
		case template.HTML:
			return t
		case nil:
			return ""
		case []any:
			var parts []string
			for _, x := range t {
				parts = append(parts, template.HTMLEscapeString(acfgoDisplay(x)))
			}
			return template.HTML(strings.Join(parts, ", "))
		}
		return template.HTML(template.HTMLEscapeString(acfgoDisplay(v)))
	}
	return template.FuncMap{
		"get_field": get,
		"the_field": func(name string, args ...any) template.HTML { return the(get(name, args...)) },
		"get_field_object": func(name string, args ...any) map[string]any {
			return s.acfGetFieldObject(ctx(), name, target(args), format(args))
		},
		"get_fields": func(args ...any) map[string]any { return s.acfGetFields(ctx(), target(args), format(args)) },
		"get_field_objects": func(args ...any) map[string]any {
			if !s.acfgoOn() {
				return nil
			}
			c := ctx()
			o := s.acfgoObject(c, target(args), nil)
			if o == nil {
				return nil
			}
			out := map[string]any{}
			for _, f := range s.acfgoObjectFields(c, o) {
				fo := map[string]any{}
				for k, v := range f {
					fo[k] = v
				}
				fo["value"] = s.acfgoLoad(c, o, f, "", true)
				out[acfgoStr(f["name"])] = fo
			}
			return out
		},
		"have_rows": func(name string, args ...any) []acfgoRow {
			if r, ok := target(args).(acfgoRow); ok {
				return rowsOf(r.Values[name])
			}
			return rowsOf(s.acfGetField(ctx(), name, target(args), true))
		},
		"get_sub_field":  func(name string, row acfgoRow) any { return row.Values[name] },
		"the_sub_field":  func(name string, row acfgoRow) template.HTML { return the(row.Values[name]) },
		"get_row_layout": func(row acfgoRow) string { return row.Layout },
		"get_row_index":  func(row acfgoRow) int { return row.Index },
		"get_row":        func(row acfgoRow) map[string]any { return row.Values },
		"acf_html":       the,
		"acfgoLabelOf": func(name string, labels any) string { // admin screens: labels.<name>
			return acfgoLabel(map[string]any{"labels": labels}, name)
		},
	}
}

// acfgoDisplay is how the_field() prints a value: strings as they are, arrays of choices by label,
// images and links by URL, posts by title.
func acfgoDisplay(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "1"
		}
		return ""
	case map[string]any:
		for _, k := range []string{"label", "url", "post_title", "name", "display_name", "value"} {
			if x, ok := t[k]; ok {
				return acfgoStr(x)
			}
		}
		return ""
	case nil:
		return ""
	}
	if s := acfgoStr(v); s != "" {
		return s
	}
	return fmt.Sprint(v)
}

// acfgoPluginPostACF is the "acf" object of a Wasm plugin's get_post (plugins_host.go): every
// field value of the post, formatted like get_fields(), when ACF-Go is active.
func (s *Server) acfgoPluginPostACF(ctx context.Context, p *store.Post) map[string]any {
	if !s.acfgoOn() {
		return nil
	}
	vals := s.acfGetFields(ctx, p, false)
	if len(vals) == 0 {
		return nil
	}
	return vals
}

// acfgoUserRESTScreen is the user edit (or add) screen ACF evaluates user_form / user_role rules on.
func acfgoUserRESTScreen(id int64, roles []string, form string) *acfScreen {
	return &acfScreen{UserForm: form, UserRoles: roles, UserID: id}
}
