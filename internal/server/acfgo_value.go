package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"maps"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Values are stored exactly like ACF stores them: in the object's meta, the value under the field
// name and the field key under "_" + name; repeater rows as name = count and name_{i}_{sub};
// flexible content as name = [layout names]; group sub fields as name_{sub}; options as
// options_{name} in the site options. acfObject is the "post id" of ACF's API (123, "option",
// "user_1", "term_5", "comment_9", a block).

type acfObject struct {
	Kind   string // post, user, term, comment, attachment, option, block
	ID     int64
	Prefix string // options: the option name prefix ("options")
	Post   *store.Post
	meta   map[string]any
}

// get reads a meta value (for options: the <prefix>_<key> option).
//
// Options are the site's settings table, which also holds auth_secret, license and payment keys: any
// string post_id is an options prefix (get_field "secret" "auth" would read auth_secret), and a theme's
// acf-json may declare an options page with any post_id. So an option is read only when it is ACF's:
// its reference "_<prefix>_<key>" holds a field key (acfgoCommit always writes both); a reference read
// returns only a field key.
func (s *Server) acfgoGet(o *acfObject, key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	if o.Kind == "option" {
		ref := s.acfgoOptionRef(o.Prefix, strings.TrimPrefix(key, "_"))
		if ref == "" {
			return nil, false
		}
		if strings.HasPrefix(key, "_") {
			return ref, true
		}
		v := s.settings.Get(o.Prefix + "_" + key)
		return v, v != nil
	}
	v, ok := o.meta[key]
	return v, ok && v != nil
}

// acfgoOptionRef is the field key stored as the reference of option <prefix>_<name>, "" when there is
// none (the option, if any, is not an ACF value).
func (s *Server) acfgoOptionRef(prefix, name string) string {
	if ref, _ := s.settings.Get("_" + prefix + "_" + name).(string); strings.HasPrefix(ref, "field_") {
		return ref
	}
	return ""
}

// acfgoObject resolves ACF's post_id argument. current is the post of the loop, used when id is nil.
func (s *Server) acfgoObject(ctx context.Context, id any, current *store.Post) *acfObject {
	switch t := id.(type) {
	case *acfObject:
		return t
	case nil:
		if current != nil {
			return s.acfgoPostObject(current)
		}
		return nil
	case *store.Post:
		return s.acfgoPostObject(t)
	case *postView:
		if t != nil && t.Post != nil {
			return s.acfgoPostObject(t.Post)
		}
		if t != nil {
			return s.acfgoObject(ctx, t.ID, nil)
		}
		return nil
	case *siteView:
		if t != nil && t.Post != nil {
			return s.acfgoObject(ctx, t.Post, nil)
		}
		return nil
	case *store.User:
		// Theme templates get a public copy of the user without Meta; read the meta from the store and let
		// acfGetField decide which keys a template may see.
		if t == nil {
			return nil
		}
		if u := s.acfgoUserByID(ctx, t.ID); u != nil {
			return &acfObject{Kind: "user", ID: t.ID, meta: u.Meta}
		}
		return nil
	case *store.Term:
		return &acfObject{Kind: "term", ID: t.ID, meta: orEmptyMap(t.Meta)}
	case int, int64, float64:
		n := acfgoInt(t)
		if n <= 0 {
			return nil
		}
		p, err := s.store.GetPost(ctx, n)
		if err != nil {
			if a, aerr := s.store.GetAttachment(ctx, n); aerr == nil {
				return &acfObject{Kind: "attachment", ID: n, meta: orEmptyMap(a.Meta)}
			}
			return nil
		}
		return s.acfgoPostObject(p)
	case string:
		v := strings.TrimSpace(t)
		if v == "" {
			return s.acfgoObject(ctx, nil, current)
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return s.acfgoObject(ctx, n, nil)
		}
		if v == "option" || v == "options" {
			return &acfObject{Kind: "option", Prefix: "options"}
		}
		if i := strings.LastIndex(v, "_"); i > 0 {
			kind, num := v[:i], v[i+1:]
			n, err := strconv.ParseInt(num, 10, 64)
			if err == nil {
				switch kind {
				case "user":
					if u := s.acfgoUserByID(ctx, n); u != nil {
						return &acfObject{Kind: "user", ID: n, meta: u.Meta}
					}
					return nil
				case "comment":
					if cm, err := s.store.GetComment(ctx, n); err == nil {
						return &acfObject{Kind: "comment", ID: n, meta: orEmptyMap(cm.Meta)}
					}
					return nil
				case "attachment":
					if a, err := s.store.GetAttachment(ctx, n); err == nil {
						return &acfObject{Kind: "attachment", ID: n, meta: orEmptyMap(a.Meta)}
					}
					return nil
				case "menu_item":
					return s.acfgoMenuObject(ctx, "menu_item", n)
				case "nav_menu", "menu":
					return s.acfgoMenuObject(ctx, "menu", n)
				default: // term_5, category_5, genre_5
					if tm, err := s.store.GetTerm(ctx, n); err == nil && (kind == "term" || kind == tm.Taxonomy) {
						return &acfObject{Kind: "term", ID: n, meta: orEmptyMap(tm.Meta)}
					}
				}
			}
		}
		return &acfObject{Kind: "option", Prefix: v} // any other string is an options prefix, like ACF
	}
	return nil
}

func (s *Server) acfgoPostObject(p *store.Post) *acfObject {
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	return &acfObject{Kind: "post", ID: p.ID, Post: p, meta: p.Meta}
}

// Writes -------------------------------------------------------------------------------------

type acfgoAddTermsKey struct{}

// acfgoWithEditor marks a save as made by u: a taxonomy field's "new:<name>" input creates the term
// only for users who may manage terms (ACF's add_term ajax checks the taxonomy's manage_terms).
func acfgoWithEditor(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, acfgoAddTermsKey{}, u != nil && u.Can("manage_categories", nil))
}

func acfgoCanAddTerms(ctx context.Context) bool {
	ok, _ := ctx.Value(acfgoAddTermsKey{}).(bool)
	return ok
}

// acfWrite collects meta changes; deletes of keys that are also set are dropped.
type acfWrite struct {
	set   map[string]any
	del   map[string]bool
	terms map[string][]int64 // taxonomy fields with save_terms
}

func newACFWrite() *acfWrite {
	return &acfWrite{set: map[string]any{}, del: map[string]bool{}, terms: map[string][]int64{}}
}

func (w *acfWrite) put(name, key string, v any) {
	w.set[name] = v
	w.set["_"+name] = key
}

// acfgoCommit stores the collected changes in the object's meta.
func (s *Server) acfgoCommit(ctx context.Context, o *acfObject, w *acfWrite) error {
	var dels []string
	for k := range w.del {
		if _, ok := w.set[k]; !ok {
			dels = append(dels, k)
		}
	}
	sort.Strings(dels)
	if o.Kind == "option" {
		// Never overwrite or delete a setting that is not an ACF value (an options page whose post_id
		// makes <prefix>_<name> a core setting such as auth_secret): it exists without a reference.
		for _, k := range append(slices.Collect(maps.Keys(w.set)), dels...) {
			name := strings.TrimPrefix(k, "_")
			if s.settings.Get(o.Prefix+"_"+name) != nil && s.acfgoOptionRef(o.Prefix, name) == "" {
				return fmt.Errorf("acf-go: %s_%s is a site setting, not a field value", o.Prefix, name)
			}
		}
		vals := map[string]any{}
		for k, v := range w.set {
			if strings.HasPrefix(k, "_") {
				vals["_"+o.Prefix+"_"+k[1:]] = v
			} else {
				vals[o.Prefix+"_"+k] = v
			}
		}
		for _, k := range dels {
			if strings.HasPrefix(k, "_") {
				vals["_"+o.Prefix+"_"+k[1:]] = nil
			} else {
				vals[o.Prefix+"_"+k] = nil
			}
		}
		if len(vals) == 0 {
			return nil
		}
		return s.settings.Set(ctx, vals)
	}
	if o.Kind == "block" { // block data lives in the block's attributes: changed in memory only
		for _, k := range dels {
			delete(o.meta, k)
		}
		for k, v := range w.set {
			o.meta[k] = v
		}
		return nil
	}
	table := map[string]string{"post": "posts", "user": "users", "term": "terms", "comment": "comments", "attachment": "media",
		"menu": "menus", "menu_item": "menu_items"}[o.Kind]
	if table == "" {
		return fmt.Errorf("acf-go: cannot save values of a %s", o.Kind)
	}
	setJSON, _ := json.Marshal(w.set)
	if dels == nil {
		dels = []string{}
	}
	if _, err := s.store.Pool.Exec(ctx, `UPDATE `+table+` SET meta = (coalesce(meta, '{}'::jsonb) - $2::text[]) || $3::jsonb WHERE id = $1`, o.ID, dels, string(setJSON)); err != nil {
		return err
	}
	if o.meta != nil {
		for _, k := range dels {
			delete(o.meta, k)
		}
		var round map[string]any
		_ = json.Unmarshal(setJSON, &round)
		for k, v := range round {
			o.meta[k] = v
		}
	}
	if o.Kind == "post" {
		for tax, ids := range w.terms {
			_ = s.store.SetPostTerms(ctx, o.ID, tax, ids)
		}
	}
	return nil
}

// acfgoInputFor reads a field's input from a map keyed by field key or by field name.
func acfgoInputFor(in map[string]any, f acfField) (any, bool) {
	if in == nil {
		return nil, false
	}
	if v, ok := in[acfgoStr(f["key"])]; ok {
		return v, true
	}
	v, ok := in[acfgoStr(f["name"])]
	return v, ok
}

// acfgoRows turns repeater/flexible input into ordered rows (lists stay, maps are sorted by
// their row keys, acfcloneindex — the template row — is skipped).
func acfgoRows(in any) []map[string]any {
	var out []map[string]any
	switch t := in.(type) {
	case []any:
		for _, r := range t {
			if m, ok := r.(map[string]any); ok {
				out = append(out, m)
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			if !strings.HasPrefix(k, "acfcloneindex") {
				keys = append(keys, k)
			}
		}
		num := func(k string) (int, bool) {
			if row, ok := t[k].(map[string]any); ok { // acfgo-input.js keeps _order in DOM order
				if o, ok := row["_order"]; ok {
					n, err := strconv.Atoi(acfgoStr(o))
					return n, err == nil
				}
			}
			n, err := strconv.Atoi(strings.TrimPrefix(k, "row-"))
			return n, err == nil
		}
		sort.SliceStable(keys, func(i, j int) bool {
			a, aok := num(keys[i])
			b, bok := num(keys[j])
			if aok && bok {
				return a < b
			}
			if aok != bok {
				return aok
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			if m, ok := t[k].(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// acfgoUpdate stores one field's input under base+name (update_field()).
func (s *Server) acfgoUpdate(ctx context.Context, o *acfObject, w *acfWrite, f acfField, base string, in any, unfiltered bool) {
	name := base + acfgoStr(f["name"])
	key := acfgoStr(f["key"])
	switch acfgoStr(f["type"]) {
	case "tab", "accordion", "message":
		return
	case "repeater":
		rows := acfgoRows(in)
		old, _ := s.acfgoGet(o, name)
		for i := len(rows); i < int(acfgoInt(old)); i++ {
			for _, sf := range acfgoSubFields(f) {
				s.acfgoDeleteTree(o, w, sf, fmt.Sprintf("%s_%d_", name, i))
			}
		}
		for i, row := range rows {
			for _, sf := range acfgoSubFields(f) {
				v, _ := acfgoInputFor(row, sf)
				s.acfgoUpdate(ctx, o, w, sf, fmt.Sprintf("%s_%d_", name, i), v, unfiltered)
			}
		}
		w.put(name, key, len(rows))
	case "flexible_content":
		rows := acfgoRows(in)
		old, _ := s.acfgoGet(o, name)
		for i, ln := range acfgoStrList(old) {
			if l := acfgoLayout(f, ln); l != nil {
				for _, sf := range acfgoLayoutSubs(l) {
					s.acfgoDeleteTree(o, w, sf, fmt.Sprintf("%s_%d_", name, i))
				}
			}
		}
		var names []any
		for _, row := range rows {
			ln := acfgoStr(row["acf_fc_layout"])
			l := acfgoLayout(f, ln)
			if l == nil {
				continue
			}
			i := len(names)
			names = append(names, ln)
			for _, sf := range acfgoLayoutSubs(l) {
				v, _ := acfgoInputFor(row, sf)
				s.acfgoUpdate(ctx, o, w, sf, fmt.Sprintf("%s_%d_", name, i), v, unfiltered)
			}
		}
		if names == nil {
			w.put(name, key, "")
		} else {
			w.put(name, key, names)
		}
	case "group":
		m, _ := in.(map[string]any)
		for _, sf := range acfgoSubFields(f) {
			v, ok := acfgoInputFor(m, sf)
			if !ok && m == nil {
				continue
			}
			s.acfgoUpdate(ctx, o, w, sf, name+"_", v, unfiltered)
		}
		w.put(name, key, "")
	case "clone":
		m, _ := in.(map[string]any)
		sub := s.acfgoCloneBase(f, base)
		for _, sf := range s.acfgoCloneFields(ctx, f) {
			v, ok := acfgoInputFor(m, sf)
			if !ok {
				continue
			}
			s.acfgoUpdate(ctx, o, w, sf, sub, v, unfiltered)
		}
		if acfgoStr(f["display"]) == "group" {
			w.put(name, key, "")
		}
	default:
		v := s.acfgoSanitize(ctx, o, w, f, in, unfiltered)
		if base == "" {
			s.acfgoBidirectional(ctx, o, f, name, v)
		}
		w.put(name, key, v)
	}
}

// acfgoDeleteTree removes a field's stored value(s) (old repeater rows).
func (s *Server) acfgoDeleteTree(o *acfObject, w *acfWrite, f acfField, base string) {
	name := base + acfgoStr(f["name"])
	switch acfgoStr(f["type"]) {
	case "repeater":
		old, _ := s.acfgoGet(o, name)
		for i := 0; i < int(acfgoInt(old)); i++ {
			for _, sf := range acfgoSubFields(f) {
				s.acfgoDeleteTree(o, w, sf, fmt.Sprintf("%s_%d_", name, i))
			}
		}
	case "flexible_content":
		old, _ := s.acfgoGet(o, name)
		for i, ln := range acfgoStrList(old) {
			if l := acfgoLayout(f, ln); l != nil {
				for _, sf := range acfgoLayoutSubs(l) {
					s.acfgoDeleteTree(o, w, sf, fmt.Sprintf("%s_%d_", name, i))
				}
			}
		}
	case "group":
		for _, sf := range acfgoSubFields(f) {
			s.acfgoDeleteTree(o, w, sf, name+"_")
		}
	}
	w.del[name], w.del["_"+name] = true, true
}

func acfgoLayout(f acfField, name string) map[string]any {
	for _, l := range acfgoList(f["layouts"]) {
		if lm, ok := l.(map[string]any); ok && acfgoStr(lm["name"]) == name {
			return lm
		}
	}
	return nil
}

func acfgoLayoutSubs(l map[string]any) []acfField {
	var out []acfField
	for _, sf := range acfgoList(l["sub_fields"]) {
		if sm, ok := sf.(map[string]any); ok {
			out = append(out, sm)
		}
	}
	return out
}

// acfgoCloneFields resolves a clone field's "clone" setting (field keys and group keys) into the
// fields it shows, with prefixed labels when asked.
func (s *Server) acfgoCloneFields(ctx context.Context, f acfField) []acfField {
	var out []acfField
	for _, k := range acfgoStrList(f["clone"]) {
		if strings.HasPrefix(k, "group_") {
			if g := s.acfgoGroupByKey(ctx, k); g != nil {
				out = append(out, g.Fields...)
			}
			continue
		}
		if cf := s.acfgoFieldByKey(ctx, k); cf != nil && acfgoStr(cf["key"]) != acfgoStr(f["key"]) {
			out = append(out, cf)
		}
	}
	if acfgoBool(f["prefix_label"]) {
		cp := make([]acfField, len(out))
		for i, cf := range out {
			m := acfField{}
			for k, v := range cf {
				m[k] = v
			}
			m["label"] = acfgoStr(f["label"]) + " " + acfgoStr(cf["label"])
			cp[i] = m
		}
		out = cp
	}
	return out
}

// acfgoCloneBase is the name prefix of a clone's fields: the clone's name when "Prefix Field
// Names" is on, otherwise the clone's own base (seamless).
func (s *Server) acfgoCloneBase(f acfField, base string) string {
	if acfgoBool(f["prefix_name"]) {
		return base + acfgoStr(f["name"]) + "_"
	}
	return base
}

var acfgoHexColor = regexp.MustCompile(`^#?[0-9a-fA-F]{3,8}$|^rgba?\(`)

// acfgoSanitize turns submitted input into the stored value of a simple field.
func (s *Server) acfgoSanitize(ctx context.Context, o *acfObject, w *acfWrite, f acfField, in any, unfiltered bool) any {
	typ := acfgoStr(f["type"])
	multi := acfgoBool(f["multiple"])
	list := func() []any {
		var out []any
		for _, v := range acfgoList(in) {
			if sv := strings.TrimSpace(acfgoStr(v)); sv != "" {
				out = append(out, sv)
			}
		}
		if out == nil {
			return []any{}
		}
		return out
	}
	switch typ {
	case "true_false":
		if acfgoBool(acfgoScalar(in)) {
			return 1
		}
		return 0
	case "checkbox", "relationship", "gallery":
		l := list()
		if typ == "checkbox" && acfgoBool(f["save_custom"]) {
			s.acfgoSaveCustomChoices(ctx, f, l)
		}
		if len(l) == 0 {
			return ""
		}
		return l
	case "select", "post_object", "page_link", "user":
		if multi {
			l := list()
			if len(l) == 0 {
				return ""
			}
			return l
		}
		return strings.TrimSpace(acfgoStr(acfgoScalar(in)))
	case "taxonomy":
		var ids []int64
		var vals []any
		for _, v := range acfgoList(in) {
			if name, ok := strings.CutPrefix(acfgoStr(v), "new:"); ok && acfgoBool(f["add_term"]) && acfgoCanAddTerms(ctx) && strings.TrimSpace(name) != "" {
				tax := defaultStr(acfgoStr(f["taxonomy"]), "category")
				t, err := s.store.TermByName(ctx, tax, strings.TrimSpace(name))
				if err != nil {
					t = &store.Term{Taxonomy: tax, Name: strings.TrimSpace(name)}
					if s.store.InsertTerm(ctx, t) != nil {
						continue
					}
				}
				v = t.ID
			}
			if n := acfgoInt(v); n > 0 {
				ids = append(ids, n)
				vals = append(vals, strconv.FormatInt(n, 10))
			}
		}
		if acfgoBool(f["save_terms"]) && o.Kind == "post" {
			tax := acfgoStr(f["taxonomy"])
			w.terms[tax] = append(w.terms[tax], ids...)
		}
		ft := acfgoStr(f["field_type"])
		if ft == "radio" || ft == "select" {
			if len(vals) == 0 {
				return ""
			}
			return vals[0]
		}
		if vals == nil {
			return ""
		}
		return vals
	case "radio":
		v := acfgoStr(acfgoScalar(in))
		if acfgoBool(f["other_choice"]) && v == "other" {
			if m, ok := in.(map[string]any); ok {
				v = acfgoStr(m["other"])
			}
		}
		if acfgoBool(f["save_other_choice"]) && v != "" {
			s.acfgoSaveCustomChoices(ctx, f, []any{v})
		}
		return v
	case "link":
		m, _ := in.(map[string]any)
		if m == nil {
			if u := acfgoStr(in); u != "" {
				return map[string]any{"title": "", "url": u, "target": ""}
			}
			return ""
		}
		if acfgoStr(m["url"]) == "" {
			return ""
		}
		return map[string]any{"title": acfgoStr(m["title"]), "url": strings.TrimSpace(acfgoStr(m["url"])), "target": acfgoStr(m["target"])}
	case "google_map":
		var m map[string]any
		switch t := in.(type) {
		case map[string]any:
			m = t
		case string:
			_ = json.Unmarshal([]byte(t), &m)
		}
		if m == nil || (acfgoStr(m["lat"]) == "" && acfgoStr(m["address"]) == "") {
			return ""
		}
		out := map[string]any{}
		for k, v := range m {
			if k == "lat" || k == "lng" {
				out[k], _ = strconv.ParseFloat(acfgoStr(v), 64)
			} else if k == "zoom" {
				out[k] = acfgoInt(v)
			} else {
				out[k] = acfgoStr(v)
			}
		}
		return out
	case "icon_picker":
		switch t := in.(type) {
		case map[string]any:
			if acfgoStr(t["value"]) == "" {
				return ""
			}
			return map[string]any{"type": defaultStr(acfgoStr(t["type"]), "dashicons"), "value": acfgoStr(t["value"])}
		case string:
			if t == "" {
				return ""
			}
			typ := "dashicons"
			if strings.HasPrefix(t, "http") || strings.HasPrefix(t, "/") {
				typ = "url"
			}
			return map[string]any{"type": typ, "value": t}
		}
		return ""
	case "date_picker":
		v := strings.TrimSpace(acfgoStr(acfgoScalar(in)))
		return strings.ReplaceAll(v, "-", "") // 2026-10-28 → 20261028 (Ymd)
	case "date_time_picker":
		v := strings.TrimSpace(strings.Replace(acfgoStr(acfgoScalar(in)), "T", " ", 1))
		if len(v) == 16 {
			v += ":00"
		}
		return v
	case "time_picker":
		v := strings.TrimSpace(acfgoStr(acfgoScalar(in)))
		if len(v) == 5 {
			v += ":00"
		}
		return v
	case "color_picker":
		v := strings.TrimSpace(acfgoStr(acfgoScalar(in)))
		if v != "" && !acfgoHexColor.MatchString(v) {
			return ""
		}
		return v
	case "wysiwyg":
		v := strings.ReplaceAll(acfgoStr(acfgoScalar(in)), "\r\n", "\n")
		if !unfiltered {
			v = wp.KsesPost(v)
		}
		return v
	case "number", "range":
		v := strings.TrimSpace(acfgoStr(acfgoScalar(in)))
		if v == "" {
			return ""
		}
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return ""
		}
		return v
	case "image", "file":
		v := acfgoScalar(in)
		if m, ok := v.(map[string]any); ok { // REST may send an attachment object
			v = m["id"]
			if v == nil {
				v = m["ID"]
			}
		}
		if n := acfgoInt(v); n > 0 {
			return strconv.FormatInt(n, 10)
		}
		return ""
	default: // text, textarea, email, url, password, oembed
		v := acfgoStr(acfgoScalar(in))
		if typ == "textarea" {
			v = strings.ReplaceAll(v, "\r\n", "\n")
		}
		if typ == "email" || typ == "url" || typ == "oembed" {
			v = strings.TrimSpace(v)
		}
		return v
	}
}

// acfgoScalar is the first value of list input (a single select posts one value as a list).
func acfgoScalar(in any) any {
	if l, ok := in.([]any); ok {
		if len(l) == 0 {
			return ""
		}
		return l[0]
	}
	return in
}

// acfgoSaveCustomChoices adds "Allow Custom"/"Other" values to the field's choices (save_custom).
func (s *Server) acfgoSaveCustomChoices(ctx context.Context, f acfField, vals []any) {
	choices := acfgoChoices(f)
	known := map[string]bool{}
	for _, c := range choices {
		known[c.Value] = true
	}
	var added []string
	for _, v := range vals {
		if sv := acfgoStr(v); sv != "" && !known[sv] {
			added = append(added, sv)
		}
	}
	if len(added) == 0 {
		return
	}
	key := acfgoStr(f["key"])
	for _, g := range s.acfgoAllGroups(ctx, false) {
		if g.ID == 0 {
			continue
		}
		changed := false
		acfgoWalkFields(g.Fields, func(gf acfField) {
			if acfgoStr(gf["key"]) == key {
				cm := acfgoChoicesMap(gf)
				for _, a := range added {
					cm = append(cm, [2]string{a, a})
				}
				gf["choices"] = acfgoChoicesValue(cm)
				changed = true
			}
		})
		if changed {
			_ = s.acfgoSaveGroup(ctx, g)
			return
		}
	}
}

// Choices ------------------------------------------------------------------------------------

// acfgoChoicesMap reads a field's choices in order. ACF stores them as an object (value → label);
// JSON objects lose order, so ACF-Go stores a list of {value,label} pairs and reads both, and
// the "value : label" text of the editor.
func acfgoChoicesMap(f acfField) [][2]string {
	var out [][2]string
	switch t := f["choices"].(type) {
	case []any:
		for _, c := range t {
			switch cv := c.(type) {
			case map[string]any:
				out = append(out, [2]string{acfgoStr(cv["value"]), acfgoStr(cv["label"])})
			case []any:
				if len(cv) == 2 {
					out = append(out, [2]string{acfgoStr(cv[0]), acfgoStr(cv[1])})
				}
			default:
				out = append(out, [2]string{acfgoStr(cv), acfgoStr(cv)})
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, [2]string{k, acfgoStr(t[k])})
		}
	case string:
		out = acfgoParseChoices(t)
	}
	return out
}

func acfgoChoices(f acfField) []acfgoChoice {
	var out []acfgoChoice
	for _, c := range acfgoChoicesMap(f) {
		out = append(out, acfgoChoice{Value: c[0], Label: c[1]})
	}
	return out
}

func acfgoChoicesValue(cm [][2]string) []any {
	out := make([]any, len(cm))
	for i, c := range cm {
		out[i] = map[string]any{"value": c[0], "label": c[1]}
	}
	return out
}

// acfgoParseChoices reads the "value : label" lines of the Choices textarea.
func acfgoParseChoices(text string) [][2]string {
	var out [][2]string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		v, l, ok := strings.Cut(line, " : ")
		if !ok {
			out = append(out, [2]string{line, line})
			continue
		}
		out = append(out, [2]string{strings.TrimSpace(v), strings.TrimSpace(l)})
	}
	return out
}

func acfgoChoiceLabel(f acfField, v string) string {
	for _, c := range acfgoChoicesMap(f) {
		if c[0] == v {
			return c[1]
		}
	}
	return v
}

// Reads ------------------------------------------------------------------------------------------

// acfgoLoad is acf_get_value() (+ acf_format_value() when format is set) for a field stored under
// base+name.
func (s *Server) acfgoLoad(ctx context.Context, o *acfObject, f acfField, base string, format bool) any {
	name := base + acfgoStr(f["name"])
	switch acfgoStr(f["type"]) {
	case "tab", "accordion", "message":
		return nil
	case "repeater":
		raw, _ := s.acfgoGet(o, name)
		n := int(acfgoInt(raw))
		var rows []any
		for i := 0; i < n; i++ {
			row := map[string]any{}
			for _, sf := range acfgoSubFields(f) {
				s.acfgoLoadInto(ctx, o, row, sf, fmt.Sprintf("%s_%d_", name, i), format)
			}
			rows = append(rows, row)
		}
		if rows == nil && format {
			return nil
		}
		if rows == nil {
			return []any{}
		}
		return rows
	case "flexible_content":
		raw, _ := s.acfgoGet(o, name)
		var rows []any
		for i, ln := range acfgoStrList(raw) {
			l := acfgoLayout(f, ln)
			if l == nil {
				continue
			}
			row := map[string]any{"acf_fc_layout": ln}
			for _, sf := range acfgoLayoutSubs(l) {
				s.acfgoLoadInto(ctx, o, row, sf, fmt.Sprintf("%s_%d_", name, i), format)
			}
			rows = append(rows, row)
		}
		if rows == nil && format {
			return nil
		}
		if rows == nil {
			return []any{}
		}
		return rows
	case "group":
		row := map[string]any{}
		for _, sf := range acfgoSubFields(f) {
			s.acfgoLoadInto(ctx, o, row, sf, name+"_", format)
		}
		return row
	case "clone":
		row := map[string]any{}
		sub := s.acfgoCloneBase(f, base)
		for _, cf := range s.acfgoCloneFields(ctx, f) {
			s.acfgoLoadInto(ctx, o, row, cf, sub, format)
		}
		return row
	}
	v, ok := s.acfgoGet(o, name)
	if acfgoStr(f["type"]) == "taxonomy" && acfgoBool(f["load_terms"]) && o.Kind == "post" {
		tax := acfgoStr(f["taxonomy"])
		ts, _ := s.store.TermsForPosts(ctx, []int64{o.ID}, tax)
		var ids []any
		for _, t := range ts[o.ID] {
			ids = append(ids, strconv.FormatInt(t.ID, 10))
		}
		v, ok = ids, true
	}
	if !ok {
		v = f["default_value"]
		if acfgoStr(f["type"]) == "true_false" && v == nil {
			v = 0
		}
		if v == nil || v == "" {
			return nil // nothing stored and no default: get_field() returns null
		}
	}
	if !format {
		return v
	}
	return s.acfgoFormat(ctx, f, v)
}

// acfgoLoadInto puts a sub field's value into a row: by name, a seamless clone merged into the row.
func (s *Server) acfgoLoadInto(ctx context.Context, o *acfObject, row map[string]any, f acfField, base string, format bool) {
	switch acfgoStr(f["type"]) {
	case "tab", "accordion", "message":
		return
	case "clone":
		if acfgoStr(f["display"]) != "group" {
			sub := s.acfgoCloneBase(f, base)
			for _, cf := range s.acfgoCloneFields(ctx, f) {
				s.acfgoLoadInto(ctx, o, row, cf, sub, format)
			}
			return
		}
	}
	row[acfgoStr(f["name"])] = s.acfgoLoad(ctx, o, f, base, format)
}

// acfgoFormat is acf_format_value(): the value a theme gets.
func (s *Server) acfgoFormat(ctx context.Context, f acfField, v any) any {
	typ := acfgoStr(f["type"])
	rf := acfgoStr(f["return_format"])
	empty := v == nil || v == "" || (func() bool { l, ok := v.([]any); return ok && len(l) == 0 })()
	switch typ {
	case "true_false":
		return acfgoBool(v)
	case "number", "range":
		if empty {
			return nil
		}
		n, err := strconv.ParseFloat(acfgoStr(v), 64)
		if err != nil {
			return nil
		}
		if n == float64(int64(n)) {
			return int64(n)
		}
		return n
	case "textarea":
		sv := acfgoStr(v)
		switch acfgoStr(f["new_lines"]) {
		case "wpautop":
			return template.HTML(wp.Autop(html.EscapeString(sv)))
		case "br":
			return template.HTML(strings.ReplaceAll(html.EscapeString(sv), "\n", "<br />\n"))
		}
		return sv
	case "wysiwyg":
		if empty {
			return ""
		}
		return template.HTML(wp.Autop(wp.KsesPost(acfgoStr(v))))
	case "oembed":
		if empty {
			return ""
		}
		return template.HTML(acfgoEmbedHTML(acfgoStr(v), int(acfgoInt(f["width"])), int(acfgoInt(f["height"]))))
	case "image", "file":
		if empty {
			return nil
		}
		id := acfgoInt(v)
		switch rf {
		case "id":
			return id
		case "url":
			if a, err := s.store.GetAttachment(ctx, id); err == nil {
				return s.uploadURL(ctx, a.FilePath)
			}
			return nil
		}
		return s.acfgoAttachmentArray(ctx, id)
	case "gallery":
		var out []any
		for _, x := range acfgoList(v) {
			id := acfgoInt(x)
			switch rf {
			case "id":
				out = append(out, id)
			case "url":
				if a, err := s.store.GetAttachment(ctx, id); err == nil {
					out = append(out, s.uploadURL(ctx, a.FilePath))
				}
			default:
				if m := s.acfgoAttachmentArray(ctx, id); m != nil {
					out = append(out, m)
				}
			}
		}
		if out == nil {
			return nil
		}
		return out
	case "select", "checkbox", "radio", "button_group":
		one := func(x string) any {
			switch rf {
			case "label":
				return acfgoChoiceLabel(f, x)
			case "array":
				return map[string]any{"value": x, "label": acfgoChoiceLabel(f, x)}
			}
			return x
		}
		if typ == "checkbox" || (typ == "select" && acfgoBool(f["multiple"])) {
			var out []any
			for _, x := range acfgoList(v) {
				out = append(out, one(acfgoStr(x)))
			}
			if out == nil {
				if typ == "checkbox" {
					return nil
				}
				return nil
			}
			return out
		}
		if empty {
			return nil
		}
		return one(acfgoStr(acfgoScalar(v)))
	case "link":
		m, _ := v.(map[string]any)
		if m == nil {
			return nil
		}
		if rf == "url" {
			return acfgoStr(m["url"])
		}
		return map[string]any{"title": acfgoStr(m["title"]), "url": acfgoStr(m["url"]), "target": acfgoStr(m["target"])}
	case "post_object", "relationship", "page_link":
		var out []any
		for _, x := range acfgoList(v) {
			id := acfgoInt(x)
			if id <= 0 {
				if typ == "page_link" && acfgoStr(x) != "" { // archive URLs
					out = append(out, acfgoStr(x))
				}
				continue
			}
			p, err := s.store.GetPost(ctx, id)
			if err != nil || p.Status == "trash" {
				continue
			}
			rv := acfgoRESTViewerFrom(ctx)
			if rv != nil && !rv.canReadPost(p) { // REST: drafts, private and future posts stay hidden
				continue
			}
			switch {
			case typ == "page_link":
				out = append(out, s.links(ctx).Permalink(p))
			case rf == "id":
				out = append(out, id)
			default:
				m := s.acfgoPostMap(ctx, p)
				if rv != nil && p.Password != "" && !(rv.u != nil && rv.u.Can("edit_post", p)) {
					m["post_content"], m["post_excerpt"] = "", "" // password protected
				}
				out = append(out, m)
			}
		}
		if typ != "relationship" && !acfgoBool(f["multiple"]) {
			if len(out) == 0 {
				return nil
			}
			return out[0]
		}
		if out == nil {
			return nil
		}
		return out
	case "taxonomy":
		var out []any
		for _, x := range acfgoList(v) {
			id := acfgoInt(x)
			if id <= 0 {
				continue
			}
			if rf == "id" {
				out = append(out, id)
				continue
			}
			if t, err := s.store.GetTerm(ctx, id); err == nil {
				out = append(out, s.acfgoTermMap(ctx, t))
			}
		}
		ft := acfgoStr(f["field_type"])
		if ft == "radio" || ft == "select" {
			if len(out) == 0 {
				return nil
			}
			return out[0]
		}
		if out == nil {
			return nil
		}
		return out
	case "user":
		var out []any
		for _, x := range acfgoList(v) {
			id := acfgoInt(x)
			if rf == "id" {
				if id > 0 {
					out = append(out, id)
				}
				continue
			}
			if u := s.acfgoUserByID(ctx, id); u != nil {
				m := s.acfgoUserMap(ctx, u)
				if rv := acfgoRESTViewerFrom(ctx); rv != nil && !(rv.u != nil && (rv.u.ID == u.ID || rv.u.Can("list_users", nil))) {
					delete(m, "user_email") // REST: not public, like the core users endpoint
					delete(m, "user_registered")
				}
				out = append(out, m)
			}
		}
		if !acfgoBool(f["multiple"]) {
			if len(out) == 0 {
				return nil
			}
			return out[0]
		}
		if out == nil {
			return nil
		}
		return out
	case "date_picker":
		sv := acfgoStr(v)
		t, err := time.Parse("20060102", sv)
		if err != nil {
			if t, err = time.Parse("2006-01-02", sv); err != nil {
				return sv
			}
		}
		return wp.FormatDate(t, defaultStr(rf, "d/m/Y"))
	case "date_time_picker":
		t, err := time.Parse("2006-01-02 15:04:05", acfgoStr(v))
		if err != nil {
			return acfgoStr(v)
		}
		return wp.FormatDate(t, defaultStr(rf, "d/m/Y g:i a"))
	case "time_picker":
		t, err := time.Parse("15:04:05", acfgoStr(v))
		if err != nil {
			return acfgoStr(v)
		}
		return wp.FormatDate(t, defaultStr(rf, "g:i a"))
	case "color_picker":
		sv := acfgoStr(v)
		if rf == "array" && sv != "" {
			return acfgoColorArray(sv)
		}
		return sv
	case "icon_picker":
		m, _ := v.(map[string]any)
		if m == nil {
			return nil
		}
		if rf == "array" {
			return m
		}
		return acfgoStr(m["value"])
	case "google_map":
		if m, ok := v.(map[string]any); ok {
			return m
		}
		return nil
	}
	return v
}

func acfgoColorArray(v string) map[string]any {
	out := map[string]any{"red": 0, "green": 0, "blue": 0, "alpha": 1}
	if strings.HasPrefix(v, "#") {
		h := v[1:]
		if len(h) == 3 {
			h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
		}
		if len(h) >= 6 {
			r, _ := strconv.ParseInt(h[0:2], 16, 64)
			g, _ := strconv.ParseInt(h[2:4], 16, 64)
			b, _ := strconv.ParseInt(h[4:6], 16, 64)
			out["red"], out["green"], out["blue"] = r, g, b
		}
		return out
	}
	inner := v[strings.Index(v, "(")+1:]
	inner = strings.TrimSuffix(strings.TrimSpace(inner), ")")
	parts := strings.Split(inner, ",")
	for i, k := range []string{"red", "green", "blue", "alpha"} {
		if i < len(parts) {
			n, _ := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
			out[k] = n
		}
	}
	return out
}

var (
	acfgoYouTubeRe = regexp.MustCompile(`(?i)(?:youtube\.com/watch\?(?:.*&)?v=|youtu\.be/|youtube\.com/embed/|youtube\.com/shorts/)([\w-]{6,})`)
	acfgoVimeoRe   = regexp.MustCompile(`(?i)vimeo\.com/(?:video/)?(\d+)`)
)

// acfgoEmbedHTML is the oEmbed HTML of a URL for the providers that need no API call; anything
// else becomes a link (ACF stores the URL and embeds on output, like this).
func acfgoEmbedHTML(u string, width, height int) string {
	if width <= 0 {
		width = 640
	}
	if height <= 0 {
		height = 390
	}
	src := ""
	if m := acfgoYouTubeRe.FindStringSubmatch(u); m != nil {
		src = "https://www.youtube.com/embed/" + m[1] + "?feature=oembed"
	} else if m := acfgoVimeoRe.FindStringSubmatch(u); m != nil {
		src = "https://player.vimeo.com/video/" + m[1]
	}
	if src == "" {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return html.EscapeString(u)
		}
		return `<a href="` + html.EscapeString(u) + `">` + html.EscapeString(u) + `</a>`
	}
	return fmt.Sprintf(`<iframe title="Embedded content" width="%d" height="%d" src="%s" frameborder="0" allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture" allowfullscreen></iframe>`, width, height, html.EscapeString(src))
}

// acfgoAttachmentArray is acf_get_attachment(): the image/file array of the "Image Array" format.
func (s *Server) acfgoAttachmentArray(ctx context.Context, id int64) map[string]any {
	a, err := s.store.GetAttachment(ctx, id)
	if err != nil {
		return nil
	}
	typ, sub, _ := strings.Cut(a.Mime, "/")
	url := s.uploadURL(ctx, a.FilePath)
	m := map[string]any{"ID": a.ID, "id": a.ID, "title": a.Title, "filename": path.Base(a.FilePath), "filesize": a.FileSize, "url": url,
		"link": s.links(ctx).HomeURL("/?attachment_id=" + itoa64(a.ID)), "alt": a.Alt, "author": itoa64(a.UploaderID), "description": a.Description,
		"caption": a.Caption, "name": a.Slug, "status": "inherit", "uploaded_to": a.PostID, "date": a.CreatedAt.Format("2006-01-02 15:04:05"),
		"modified": a.UpdatedAt.Format("2006-01-02 15:04:05"), "menu_order": a.MenuOrder, "mime_type": a.Mime, "type": typ, "subtype": sub,
		"icon": mimeIcon(a.Mime)}
	if typ == "image" {
		m["width"], m["height"] = a.Width, a.Height
		sizes := map[string]any{}
		for _, name := range []string{"thumbnail", "medium", "medium_large", "large", "1536x1536", "2048x2048"} {
			sizes[name], sizes[name+"-width"], sizes[name+"-height"] = url, a.Width, a.Height
		}
		for _, v := range a.Variants {
			sizes[v.Name], sizes[v.Name+"-width"], sizes[v.Name+"-height"] = s.uploadURL(ctx, v.FilePath), v.Width, v.Height
		}
		m["sizes"] = sizes
	}
	return m
}

// acfgoPostMap is a WP_Post as a theme sees it (ACF's "Post Object" format), plus its permalink.
func (s *Server) acfgoPostMap(ctx context.Context, p *store.Post) map[string]any {
	loc := s.loc()
	return map[string]any{"ID": p.ID, "post_author": itoa64(p.AuthorID), "post_date": p.Date().In(loc).Format("2006-01-02 15:04:05"),
		"post_date_gmt": p.Date().UTC().Format("2006-01-02 15:04:05"), "post_content": p.Content, "post_title": p.Title, "post_excerpt": p.Excerpt,
		"post_status": p.Status, "comment_status": p.CommentStatus, "ping_status": p.PingStatus, "post_name": p.Slug,
		"post_modified": p.ModifiedAt.In(loc).Format("2006-01-02 15:04:05"), "post_parent": p.ParentID, "menu_order": p.MenuOrder,
		"post_type": p.Type, "comment_count": itoa64(int64(p.CommentCount)), "guid": s.links(ctx).HomeURL("/?p=" + itoa64(p.ID)),
		"permalink": s.links(ctx).Permalink(p), "filter": "raw"}
}

func (s *Server) acfgoTermMap(ctx context.Context, t *store.Term) map[string]any {
	return map[string]any{"term_id": t.ID, "name": t.Name, "slug": t.Slug, "term_group": 0, "term_taxonomy_id": t.ID, "taxonomy": t.Taxonomy,
		"description": t.Description, "parent": t.ParentID, "count": t.Count, "filter": "raw"}
}

func (s *Server) acfgoUserMap(ctx context.Context, u *store.User) map[string]any {
	return map[string]any{"ID": u.ID, "user_firstname": u.Profile["first_name"], "user_lastname": u.Profile["last_name"], "nickname": u.Profile["nickname"],
		"user_nicename": u.Nicename, "display_name": u.DisplayName, "user_email": u.Email, "user_url": u.URL,
		"user_registered": u.RegisteredAt.Format("2006-01-02 15:04:05"), "user_description": u.Profile["description"],
		"user_avatar": `<img alt="" src="` + html.EscapeString(s.avatarFor(ctx, u.Email, 96)) + `" class="avatar avatar-96 photo" height="96" width="96" />`}
}

// Public API (get_field and friends) -------------------------------------------------------------

// acfgoFind resolves a selector (field name or key) for an object: the _name reference first,
// then the groups shown for the object, then any field of that name.
func (s *Server) acfgoFind(ctx context.Context, o *acfObject, selector string) acfField {
	if strings.HasPrefix(selector, "field_") {
		if f := s.acfgoFieldByKey(ctx, selector); f != nil {
			return f
		}
	}
	if ref, ok := s.acfgoGet(o, "_"+selector); ok {
		if f := s.acfgoFieldByKey(ctx, acfgoStr(ref)); f != nil {
			if acfgoStr(f["name"]) == selector {
				return f
			}
			// A sub field reached through its full meta name (repeater_0_title).
			cp := acfField{}
			for k, v := range f {
				cp[k] = v
			}
			cp["name"] = selector
			return cp
		}
	}
	var fallback acfField
	for _, g := range s.acfgoAllGroups(ctx, false) {
		for _, f := range g.Fields {
			if acfgoStr(f["name"]) != selector {
				continue
			}
			if o != nil && s.acfgoMatchGroup(g, s.acfgoObjectScreen(ctx, o)) {
				return f
			}
			if fallback == nil {
				fallback = f
			}
		}
	}
	return fallback
}

// acfgoObjectScreen is the location screen an object is edited on (for field lookup by name).
func (s *Server) acfgoObjectScreen(ctx context.Context, o *acfObject) *acfScreen {
	switch o.Kind {
	case "post":
		if o.Post != nil {
			return s.acfgoPostScreen(ctx, o.Post, nil)
		}
	case "user":
		sc := &acfScreen{UserForm: "edit", UserID: o.ID}
		if u := s.acfgoUserByID(ctx, o.ID); u != nil {
			sc.UserRoles = u.Roles
		}
		return sc
	case "term":
		if t, err := s.store.GetTerm(ctx, o.ID); err == nil {
			return &acfScreen{Taxonomy: t.Taxonomy}
		}
	case "option":
		sc := &acfScreen{OptionsPage: "-"}
		for _, op := range s.acfgoOptionsPages(ctx) {
			if op.PostID == o.Prefix || (o.Prefix == "options" && op.PostID == "options") {
				sc.OptionsPage = op.MenuSlug
				break
			}
		}
		return sc
	case "comment":
		return &acfScreen{Comment: "all"}
	case "attachment":
		return &acfScreen{Attachment: "all"}
	case "block":
		return &acfScreen{Block: o.Prefix}
	case "menu", "menu_item":
		return s.acfgoMenuObjectScreen(ctx, o)
	}
	return &acfScreen{}
}

// acfGetField is get_field(): the formatted (or raw) value of a field of an object.
func (s *Server) acfGetField(ctx context.Context, selector string, id any, format bool) any {
	if !s.acfgoOn() {
		return nil
	}
	o := s.acfgoObject(ctx, id, nil)
	if o == nil {
		return nil
	}
	if o.Kind == "block" {
		return s.acfgoBlockValue(ctx, o, selector, format)
	}
	f := s.acfgoFind(ctx, o, selector)
	if acfgoPrivateMeta(o, selector) || (f != nil && acfgoPrivateMeta(o, acfgoStr(f["name"]))) {
		return nil
	}
	if f == nil {
		// User and comment meta holds sign-in and contact data, not only field values: theme templates
		// get raw meta there only for keys that are registered ACF fields.
		if o.Kind == "user" || o.Kind == "comment" {
			return nil
		}
		v, _ := s.acfgoGet(o, selector)
		return v
	}
	return s.acfgoLoad(ctx, o, f, "", format)
}

// acfgoPrivateMeta reports a user meta key that controls sign-in or permissions (reset keys, sessions,
// capabilities, security settings); get_field never returns it, even when a field shares its name.
func acfgoPrivateMeta(o *acfObject, key string) bool {
	if o == nil || o.Kind != "user" {
		return false
	}
	k := strings.ToLower(strings.TrimPrefix(key, "_"))
	switch k {
	case "password_reset", "new_email", "session_tokens", "sessions", "application_passwords", "forum_google_sub":
		return true
	}
	return strings.HasPrefix(k, "adminsec_") || strings.HasSuffix(k, "capabilities") || strings.HasSuffix(k, "user_level") ||
		strings.Contains(k, "password") || strings.Contains(k, "session") || strings.Contains(k, "token")
}

// acfGetFieldObject is get_field_object(): the field settings with "value".
func (s *Server) acfGetFieldObject(ctx context.Context, selector string, id any, format bool) map[string]any {
	if !s.acfgoOn() {
		return nil
	}
	o := s.acfgoObject(ctx, id, nil)
	if o == nil {
		return nil
	}
	f := s.acfgoFind(ctx, o, selector)
	if f == nil || acfgoPrivateMeta(o, selector) || acfgoPrivateMeta(o, acfgoStr(f["name"])) {
		return nil
	}
	out := map[string]any{}
	for k, v := range f {
		out[k] = v
	}
	out["value"] = s.acfgoLoad(ctx, o, f, "", format)
	return out
}

// acfGetFields is get_fields(): every top-level field value stored on the object, by name.
func (s *Server) acfGetFields(ctx context.Context, id any, format bool) map[string]any {
	if !s.acfgoOn() {
		return nil
	}
	o := s.acfgoObject(ctx, id, nil)
	if o == nil {
		return nil
	}
	out := map[string]any{}
	for _, f := range s.acfgoObjectFields(ctx, o) {
		s.acfgoLoadInto(ctx, o, out, f, "", format)
	}
	return out
}

// acfgoObjectFields lists the top-level fields an object has values for or is shown with.
func (s *Server) acfgoObjectFields(ctx context.Context, o *acfObject) []acfField {
	sc := s.acfgoObjectScreen(ctx, o)
	seen := map[string]bool{}
	var out []acfField
	for _, g := range s.acfgoAllGroups(ctx, false) {
		shown := s.acfgoMatchGroup(g, sc)
		for _, f := range g.Fields {
			name := acfgoStr(f["name"])
			if name == "" || seen[name] {
				continue
			}
			ref, has := s.acfgoGet(o, "_"+name)
			if shown || (has && acfgoStr(ref) == acfgoStr(f["key"])) {
				seen[name] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// acfUpdateField is update_field(): stores a value given like get_field returns it unformatted
// (sub field values by name or key).
func (s *Server) acfUpdateField(ctx context.Context, selector string, value any, id any) error {
	o := s.acfgoObject(ctx, id, nil)
	if o == nil {
		return fmt.Errorf("acf-go: unknown object %v", id)
	}
	f := s.acfgoFind(ctx, o, selector)
	if f == nil {
		return fmt.Errorf("acf-go: no field %q", selector)
	}
	w := newACFWrite()
	s.acfgoUpdate(ctx, o, w, f, "", value, true)
	return s.acfgoCommit(ctx, o, w)
}

// acfDeleteField is delete_field().
func (s *Server) acfDeleteField(ctx context.Context, selector string, id any) error {
	o := s.acfgoObject(ctx, id, nil)
	if o == nil {
		return fmt.Errorf("acf-go: unknown object %v", id)
	}
	w := newACFWrite()
	if f := s.acfgoFind(ctx, o, selector); f != nil {
		s.acfgoDeleteTree(o, w, f, "")
	} else {
		w.del[selector], w.del["_"+selector] = true, true
	}
	return s.acfgoCommit(ctx, o, w)
}
