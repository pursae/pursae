package webbuilder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Doc parsing, validation and normalization (contract §8.1).
//
// The doc is the source of truth; the stored doc is the client's JSON, normalized at the JSON
// level so fields this server does not know survive:
//
//   - case variants of known keys ("Kind", "CSS") are removed, so Go and the browser read the
//     same value;
//   - without unfiltered_html, html sections, every content["html"] and every style.css are
//     removed (Stripped names what went);
//   - rich text values are re-sanitized to the rich.ts subset, bg.decorSvg to the SVG subset.
//
// Validation runs on the request as sent (paths in ValidationError point into it) and reports
// limits, ids, kinds, enums, URL schemes and CSS values.

// Errors of ParseDoc / ParseSection.
var (
	ErrDocTooLarge = errors.New("webbuilder: doc too large") // 413 wb_too_large
	ErrDocSyntax   = errors.New("webbuilder: doc is not valid JSON of the expected shape")
)

// DocOptions tune ParseDoc.
type DocOptions struct {
	// UnfilteredHTML: the author may keep html sections and custom CSS.
	UnfilteredHTML bool
	// PathPrefix is prepended to every ValidationError.Path (e.g. "/doc").
	PathPrefix string
	// Stored are documents stored before for the same post (the published doc, the saving user's
	// draft, a revision being restored), first match wins. Without UnfilteredHTML the privileged parts
	// of the doc are taken from them by section id instead of being dropped (see keepStored).
	Stored []json.RawMessage
}

// DocResult is a parsed doc.
type DocResult struct {
	Doc      DocState        // typed view of JSON
	JSON     json.RawMessage // normalized doc to store
	Stripped []string        // StrippedHTMLSections, StrippedCustomCSS
	Errors   []ValidationError
	// Kept: privileged parts (html or code sections, content.html, style.css) were taken from
	// DocOptions.Stored, so the doc holds markup an unfiltered_html author stored.
	Kept bool
}

// Valid reports whether the doc passed validation.
func (r *DocResult) Valid() bool { return len(r.Errors) == 0 }

// SectionResult is a parsed single section (library items).
type SectionResult struct {
	Section  *Section        // nil when the section itself was stripped (an html section)
	JSON     json.RawMessage // normalized section; "null" when stripped
	Stripped []string
	Errors   []ValidationError
}

func (r *SectionResult) Valid() bool { return len(r.Errors) == 0 }

// ParseDoc parses, validates and normalizes a DocState JSON document.
func ParseDoc(raw []byte, opts DocOptions) (*DocResult, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	canonDoc(root)
	v := newValidator(opts)
	canon, _ := json.Marshal(root)
	var typed DocState
	if err := json.Unmarshal(canon, &typed); err != nil {
		v.typeError(err)
	} else {
		v.doc(&typed, root)
	}
	n := &normalizer{opts: opts}
	if !opts.UnfilteredHTML && len(opts.Stored) > 0 {
		n.stored = newStoredParts(opts.Stored)
	}
	n.doc(root)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDocSyntax, err)
	}
	res := &DocResult{JSON: out, Stripped: n.strippedList(), Errors: v.errs, Kept: n.kept}
	if err := json.Unmarshal(out, &res.Doc); err != nil && len(res.Errors) == 0 {
		v.typeError(err)
		res.Errors = v.errs
	}
	return res, nil
}

// ParseSection parses one Section (a saved component).
func ParseSection(raw []byte, opts DocOptions) (*SectionResult, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	canonSection(root)
	v := newValidator(opts)
	canon, _ := json.Marshal(root)
	var typed Section
	if err := json.Unmarshal(canon, &typed); err != nil {
		v.typeError(err)
	} else {
		v.strings(root, v.prefix, 0)
		v.ids = map[string]bool{}
		v.section(&typed, v.prefix, 0)
	}
	n := &normalizer{opts: opts}
	res := &SectionResult{Errors: v.errs}
	if n.section(root) {
		out, _ := json.Marshal(root)
		res.JSON = out
		var s Section
		if err := json.Unmarshal(out, &s); err == nil {
			res.Section = &s
		}
	} else {
		res.JSON = json.RawMessage("null")
	}
	res.Stripped = n.strippedList()
	return res, nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	if len(raw) > MaxDocBytes {
		return nil, ErrDocTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDocSyntax, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing data", ErrDocSyntax)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: not an object", ErrDocSyntax)
	}
	return m, nil
}

// ---------------------------------------------------------------------------------------------
// Canonicalization: drop case variants of known keys (encoding/json matches field names
// case-insensitively, the browser does not).

var (
	docKeys     = []string{"sections", "site"}
	sectionKeys = []string{"id", "kind", "name", "hidden", "content", "items", "style", "box", "els", "grid", "bg"}
	itemKeys    = []string{"id", "title", "text", "icon", "box", "href", "node", "parent", "tag", "lay", "sub", "dyn"}
	dynKeys     = []string{"tag", "link", "format", "size", "ratio", "fields", "sep", "tax", "location", "menuLayout", "burgerAt", "words", "prefix", "placeholder", "button", "empty",
		"dropdown", "flags", "names", "hideCurrent", "source", "columns", "limit", "orderBy", "order", "cats", "tags", "ids", "product", "paginate", "showPrice", "hideEmpty", "parent", "showTotal", "gallery"}
	styleKeys = []string{"palette", "align", "padding", "titleSize", "titleWeight", "letterSpacing", "titleColor", "bodySize", "bg", "hideOn", "cssId", "css", "anim", "motion"}
)

func dropCaseVariants(m map[string]any, known []string) {
	for k := range m {
		for _, kn := range known {
			if k != kn && strings.EqualFold(k, kn) {
				delete(m, k)
			}
		}
	}
}

func canonDoc(root map[string]any) {
	dropCaseVariants(root, docKeys)
	if list, ok := root["sections"].([]any); ok {
		for _, s := range list {
			if sm, ok := s.(map[string]any); ok {
				canonSection(sm)
			}
		}
	}
}

func canonSection(s map[string]any) {
	dropCaseVariants(s, sectionKeys)
	if st, ok := s["style"].(map[string]any); ok {
		dropCaseVariants(st, styleKeys)
	}
	if items, ok := s["items"].([]any); ok {
		for _, it := range items {
			im, ok := it.(map[string]any)
			if !ok {
				continue
			}
			dropCaseVariants(im, itemKeys)
			if dm, ok := im["dyn"].(map[string]any); ok {
				dropCaseVariants(dm, dynKeys)
			}
			if sub, ok := im["sub"].(map[string]any); ok {
				canonSection(sub)
			}
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Normalization: privilege strip, rich text, decor SVG.

type normalizer struct {
	opts     DocOptions
	stripped map[string]bool
	stored   *storedParts // the privileged parts a user without unfiltered_html keeps
	kept     bool         // something was taken from stored
	depth    int          // of the section being normalized (0 = top level)
}

// Privileged parts kept on save.
//
// A user without unfiltered_html may not write html or code sections, content.html or style.css,
// but may edit a page that has them (an administrator imported a design, an editor fixes a typo).
// Their saves keep what is stored: keepStored, by section id, against the docs stored before
// (DocOptions.Stored, which only ever hold privileged parts written by an unfiltered_html author or
// kept this way):
//
//   - a privileged (html or code) section the doc lists by a stored privileged section's id is the
//     stored section, at the doc's position (only its hidden flag is the doc's); a code section only
//     at the top level. One the doc no longer lists is removed: removing and reordering are allowed,
//     editing is not;
//   - the content.html and style.css of any other section are the stored values of the section with
//     its id (none when nothing is stored);
//   - privileged parts with no stored counterpart are dropped as before.
//
// An incoming privileged part that differs from the stored one is reported as stripped.

// storedParts are the privileged parts of the stored docs by section id.
type storedParts struct {
	priv map[string]map[string]any    // html and code sections
	html map[string]string            // content.html of other sections
	css  map[string]string            // style.css
	rcpt map[string]map[string]string // a form's recipients (FormRecipientKeys)
}

func newStoredParts(docs []json.RawMessage) *storedParts {
	sp := &storedParts{priv: map[string]map[string]any{}, html: map[string]string{}, css: map[string]string{}, rcpt: map[string]map[string]string{}}
	seen := map[string]bool{}
	var walk func(s map[string]any)
	walk = func(s map[string]any) {
		id, _ := s["id"].(string)
		if id != "" && !seen[id] {
			seen[id] = true
			if k := s["kind"]; k == KindHTML || k == KindCode {
				sp.priv[id] = s
				return
			}
			if c, ok := s["content"].(map[string]any); ok {
				if h, ok := c["html"].(string); ok {
					sp.html[id] = h
				}
				for _, k := range FormRecipientKeys {
					if v, ok := c[k].(string); ok && v != "" {
						if sp.rcpt[id] == nil {
							sp.rcpt[id] = map[string]string{}
						}
						sp.rcpt[id][k] = v
					}
				}
			}
			if st, ok := s["style"].(map[string]any); ok {
				if css, ok := st["css"].(string); ok && strings.TrimSpace(css) != "" {
					sp.css[id] = css
				}
			}
		}
		items, _ := s["items"].([]any)
		for _, it := range items {
			if im, ok := it.(map[string]any); ok {
				if sub, ok := im["sub"].(map[string]any); ok {
					walk(sub)
				}
			}
		}
	}
	for _, raw := range docs {
		var root map[string]any
		if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
			continue
		}
		list, _ := root["sections"].([]any)
		for _, s := range list {
			if sm, ok := s.(map[string]any); ok {
				walk(sm)
			}
		}
	}
	return sp
}

// keepStored replaces s by the stored privileged section with its id; false when there is none.
func (n *normalizer) keepStored(s map[string]any) bool {
	if n.stored == nil {
		return false
	}
	id, _ := s["id"].(string)
	st := n.stored.priv[id]
	if st == nil || (st["kind"] == KindCode && n.depth > 0) {
		return false
	}
	if !jsonEqualExcept(s, st, "hidden") {
		n.strip(StrippedHTMLSections) // the edit is not kept
	}
	hidden, hasHidden := s["hidden"].(bool)
	clear(s)
	var cp map[string]any
	b, _ := json.Marshal(st)
	_ = json.Unmarshal(b, &cp)
	for k, v := range cp {
		s[k] = v
	}
	if hasHidden {
		s["hidden"] = hidden
	}
	n.kept = true
	return true
}

// jsonEqualExcept reports whether a and b are the same JSON object apart from the key skip.
func jsonEqualExcept(a, b map[string]any, skip string) bool {
	strip := func(m map[string]any) []byte {
		c := make(map[string]any, len(m))
		for k, v := range m {
			if k != skip {
				c[k] = v
			}
		}
		out, _ := json.Marshal(c)
		return out
	}
	return bytes.Equal(strip(a), strip(b))
}

func (n *normalizer) strip(what string) {
	if n.stripped == nil {
		n.stripped = map[string]bool{}
	}
	n.stripped[what] = true
}

func (n *normalizer) strippedList() []string {
	var out []string
	for k := range n.stripped {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (n *normalizer) doc(root map[string]any) {
	list, ok := root["sections"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(list))
	for _, s := range list {
		sm, ok := s.(map[string]any)
		if !ok {
			kept = append(kept, s)
			continue
		}
		if n.section(sm) {
			kept = append(kept, sm)
		}
	}
	root["sections"] = kept
}

// section normalizes s in place; false = drop the whole section.
func (n *normalizer) section(s map[string]any) bool {
	if !n.opts.UnfilteredHTML && n.keepStored(s) {
		return true // as stored (normalized when it was stored)
	}
	if !n.opts.UnfilteredHTML && (s["kind"] == KindHTML || s["kind"] == KindCode) {
		n.strip(StrippedHTMLSections)
		return false
	}
	id, _ := s["id"].(string)
	if s["kind"] == KindCode { // the design's own markup and local styles: kept as written (privileged)
		return true
	}
	delete(s, "local") // local styles belong to code sections only
	if content, ok := s["content"].(map[string]any); ok {
		for k, v := range content {
			str, ok := v.(string)
			if !ok {
				continue
			}
			if k == "html" {
				if !n.opts.UnfilteredHTML {
					if kept, ok := n.storedHTML(id); !ok || kept != str {
						n.strip(StrippedHTMLSections)
					}
					delete(content, k)
				}
				continue
			}
			if !isURLKey(k) && !isDataURI(str) && IsRich(str) {
				content[k] = SanitizeRich(str)
			}
		}
	}
	if !n.opts.UnfilteredHTML {
		n.keepRecipients(s, id)
		if h, ok := n.storedHTML(id); ok { // the stored content.html stays
			content, _ := s["content"].(map[string]any)
			if content == nil {
				content = map[string]any{}
				s["content"] = content
			}
			content["html"] = h
			n.kept = true
		}
	}
	if st, ok := s["style"].(map[string]any); ok && !n.opts.UnfilteredHTML {
		kept, has := n.storedCSS(id)
		if css, ok := st["css"]; ok {
			if str, _ := css.(string); strings.TrimSpace(str) != "" && str != kept {
				n.strip(StrippedCustomCSS)
			}
			delete(st, "css")
		}
		if has { // the stored custom CSS stays
			st["css"] = kept
			n.kept = true
		}
	}
	if bg, ok := s["bg"].(map[string]any); ok {
		if svg, ok := bg["decorSvg"].(string); ok && svg != "" {
			bg["decorSvg"] = SanitizeSVG(svg)
		}
	}
	if items, ok := s["items"].([]any); ok {
		for _, it := range items {
			im, ok := it.(map[string]any)
			if !ok {
				continue
			}
			for _, k := range []string{"title", "text"} {
				if str, ok := im[k].(string); ok && IsRich(str) && !isDataURI(str) {
					im[k] = SanitizeRich(str)
				}
			}
			if sub, ok := im["sub"].(map[string]any); ok {
				n.depth++
				keep := n.section(sub)
				n.depth--
				if !keep {
					delete(im, "sub")
				}
			}
		}
	}
	return true
}

// storedHTML is the stored content.html of the section id.
func (n *normalizer) storedHTML(id string) (string, bool) {
	if n.stored == nil || id == "" {
		return "", false
	}
	h, ok := n.stored.html[id]
	return h, ok
}

// FormRecipientKeys are the section content keys naming where a form's submissions are mailed.
// They are privileged like content.html: an author could otherwise route a form to themselves
// through a page or a library component that an administrator later publishes, so without
// UnfilteredHTML the stored values (by section id) stay and new ones are dropped.
var FormRecipientKeys = []string{FormKeyTo, FormKeyCc}

// StrippedRecipients names form recipients removed from a save (FormRecipientKeys).
const StrippedRecipients = "form-recipients"

// keepRecipients puts the stored recipients of section id back into s (none when nothing is stored).
func (n *normalizer) keepRecipients(s map[string]any, id string) {
	content, _ := s["content"].(map[string]any)
	var kept map[string]string
	if n.stored != nil && id != "" {
		kept = n.stored.rcpt[id]
	}
	for _, k := range FormRecipientKeys {
		str, _ := content[k].(string)
		if str != kept[k] && strings.TrimSpace(str) != "" {
			n.strip(StrippedRecipients)
		}
		if content != nil {
			delete(content, k)
		}
		if kept[k] != "" {
			if content == nil {
				content = map[string]any{}
				s["content"] = content
			}
			content[k] = kept[k] // plain text: not markup that would make the snapshot privileged (n.kept)
		}
	}
}

// StripFormRecipients removes FormRecipientKeys from every section of a stored section or doc
// (a library component saved before recipients were privileged).
func StripFormRecipients(raw json.RawMessage) json.RawMessage {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return raw
	}
	changed := false
	var walk func(s map[string]any)
	walk = func(s map[string]any) {
		if c, ok := s["content"].(map[string]any); ok {
			for _, k := range FormRecipientKeys {
				if _, ok := c[k]; ok {
					delete(c, k)
					changed = true
				}
			}
		}
		items, _ := s["items"].([]any)
		for _, it := range items {
			if im, ok := it.(map[string]any); ok {
				if sub, ok := im["sub"].(map[string]any); ok {
					walk(sub)
				}
			}
		}
	}
	if list, ok := root["sections"].([]any); ok {
		for _, s := range list {
			if sm, ok := s.(map[string]any); ok {
				walk(sm)
			}
		}
	} else {
		walk(root)
	}
	if !changed {
		return raw
	}
	out, _ := json.Marshal(root)
	return out
}

// storedCSS is the stored style.css of the section id.
func (n *normalizer) storedCSS(id string) (string, bool) {
	if n.stored == nil || id == "" {
		return "", false
	}
	css, ok := n.stored.css[id]
	return css, ok
}

// ---------------------------------------------------------------------------------------------
// Validation.

var (
	idRE         = regexp.MustCompile(IDPattern)
	contentKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	elKeyRE      = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,200}$`)
	cssIDRE      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	tagRE        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
	iconNameRE   = regexp.MustCompile(`^[A-Za-z0-9 _.-]{0,64}$`)
	fontNameRE   = regexp.MustCompile(`^@?[^'"\\;{}<>()]{0,100}$`)
	imageKeyRE   = regexp.MustCompile(`^(image[0-9]*|poster|avatar|logoImage|bgImage|photo[0-9]*)$`)
)

var enums = map[string]map[string]bool{
	"palette":     toSet([]string{"brand", "dark", "light", "soft", "sand"}),
	"secAlign":    toSet([]string{"left", "center"}),
	"device":      toSet([]string{"desktop", "tablet", "mobile"}),
	"anim":        toSet([]string{"none", "fade", "up", "zoom", "left"}),
	"fontPair":    toSet([]string{"elegant", "modern", "plain"}),
	"corners":     toSet([]string{"sharp", "soft", "round"}),
	"node":        toSet([]string{"box", "heading", "text", "button", "link", "image", "icon", "input", "divider"}),
	"elAlign":     toSet([]string{"left", "center", "right"}),
	"widthUnit":   toSet([]string{"px", "%", "vw"}),
	"maxUnit":     toSet([]string{"px", "%"}),
	"heightUnit":  toSet([]string{"px", "vh"}),
	"fit":         toSet([]string{"cover", "contain"}),
	"transform":   toSet([]string{"none", "uppercase", "lowercase", "capitalize"}),
	"sea":         toSet([]string{"start", "center", "end"}),
	"seas":        toSet([]string{"start", "center", "end", "stretch"}),
	"layDir":      toSet([]string{"row", "column", "grid"}),
	"layJustify":  toSet([]string{"start", "center", "end", "space-between"}),
	"contentW":    toSet([]string{"boxed", "full"}),
	"direction":   toSet([]string{"row", "column", "row-reverse", "column-reverse"}),
	"boxJustify":  toSet([]string{"start", "center", "end", "space-between", "space-around", "space-evenly"}),
	"boxAlign":    toSet([]string{"stretch", "start", "center", "end"}),
	"borderStyle": toSet([]string{"none", "solid", "dashed", "dotted"}),
	"shadow":      toSet([]string{"none", "sm", "md", "lg"}),
	"bgType":      toSet([]string{"color", "gradient", "image"}),
	"decor":       toSet([]string{"none", "globe", "rings", "waves", "blob", "dots", "grid", "custom"}),
	"decorAnim":   toSet([]string{"none", "spin", "float", "pulse"}),
	"entrance":    toSet([]string{"none", "fade", "fade-up", "fade-down", "fade-left", "fade-right", "zoom-in", "zoom-out", "slide-up", "slide-left", "slide-right", "blur", "flip", "bounce"}),
	"easing":      toSet([]string{"smooth", "ease", "ease-out", "ease-in-out", "spring", "linear"}),
	"hover":       toSet([]string{"none", "lift", "glow", "scale"}),
	"parallaxOn":  toSet([]string{"bg", "content"}),
	"hoverOn":     toSet([]string{"self", "items"}),
	"kind":        toSet(SectionKinds),
	"dynTag":      toSet(DynTags),
	"dynSize":     toSet(DynImageSizes),
	"dynField":    toSet(DynMetaFields),
	"menuLayout":  toSet(DynMenuLayouts),
	"shopSource":  toSet(ShopSources),
	"shopOrderBy": toSet(ShopOrderBys),
	"loopSource":  toSet(LoopSources),
	"loopOrderBy": toSet(LoopOrderBys),
	"loopOrder":   toSet(LoopOrders),
	"loopPaging":  toSet(LoopPaginations),
	"pageLayout":  toSet([]string{PageLayoutSite, PageLayoutCanvas}),
}

const maxValidationErrors = 100

type validator struct {
	opts      DocOptions
	prefix    string
	errs      []ValidationError
	ids       map[string]bool
	loopDepth int             // loop sections enclosing the one being validated
	codePaths map[string]bool // JSON pointers of code sections (longer content limits)
}

func newValidator(opts DocOptions) *validator {
	return &validator{opts: opts, prefix: opts.PathPrefix}
}

func (v *validator) add(path, code, msg string) {
	if len(v.errs) >= maxValidationErrors {
		return
	}
	v.errs = append(v.errs, ValidationError{Path: path, Code: code, Message: msg})
}

func (v *validator) typeError(err error) {
	var te *json.UnmarshalTypeError
	path := v.prefix
	if errors.As(err, &te) && te.Field != "" {
		path += "/" + strings.ReplaceAll(te.Field, ".", "/")
	}
	v.add(path, VErrEnum, "wrong JSON type: "+err.Error())
}

func (v *validator) enum(path, set, val string) {
	if val == "" {
		return
	}
	if !enums[set][val] {
		v.add(path, VErrEnum, fmt.Sprintf("unknown value %q", trunc(val, 40)))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func ptr(base string, parts ...any) string {
	var b strings.Builder
	b.WriteString(base)
	for _, p := range parts {
		b.WriteByte('/')
		switch x := p.(type) {
		case int:
			b.WriteString(strconv.Itoa(x))
		case string:
			b.WriteString(strings.ReplaceAll(strings.ReplaceAll(x, "~", "~0"), "/", "~1"))
		}
	}
	return b.String()
}

func (v *validator) doc(d *DocState, raw map[string]any) {
	if _, ok := raw["sections"]; !ok {
		v.add(ptr(v.prefix, "sections"), VErrRequired, "sections is required")
	}
	v.strings(raw, v.prefix, 0)
	if len(d.Sections) > MaxSections {
		v.add(ptr(v.prefix, "sections"), VErrTooMany, fmt.Sprintf("more than %d sections", MaxSections))
		return
	}
	v.ids = map[string]bool{}
	for i := range d.Sections {
		v.section(&d.Sections[i], ptr(v.prefix, "sections", i), 0)
	}
	v.site(&d.Site, ptr(v.prefix, "site"))
}

// strings checks every string (keys and values) for MaxStringBytes; content.html of an html
// section may be MaxHTMLSectionLen.
func (v *validator) strings(x any, path string, depth int) {
	if depth > 64 {
		v.add(path, VErrTooDeep, "JSON nested too deeply")
		return
	}
	switch t := x.(type) {
	case string:
		limit := MaxStringBytes
		if strings.HasSuffix(path, "/content/html") {
			limit = MaxHTMLSectionLen
			if v.codePaths[strings.TrimSuffix(path, "/content/html")] {
				limit = MaxCodeSectionLen
			}
		} else if strings.HasSuffix(path, "/content/head") && v.codePaths[strings.TrimSuffix(path, "/content/head")] {
			limit = MaxCodeSectionLen
		}
		if len(t) > limit {
			v.add(path, VErrTooLong, fmt.Sprintf("longer than %d bytes", limit))
		}
	case map[string]any:
		if t["kind"] == KindCode {
			if v.codePaths == nil {
				v.codePaths = map[string]bool{}
			}
			v.codePaths[path] = true
		}
		for k, val := range t {
			if len(k) > 256 {
				v.add(path, VErrTooLong, "key too long")
				continue
			}
			v.strings(val, ptr(path, k), depth+1)
		}
	case []any:
		for i, val := range t {
			v.strings(val, ptr(path, i), depth+1)
		}
	}
}

func (v *validator) section(s *Section, path string, depth int) {
	if depth > MaxDepth {
		v.add(path, VErrTooDeep, fmt.Sprintf("nested deeper than %d", MaxDepth))
		return
	}
	switch {
	case s.ID == "":
		v.add(ptr(path, "id"), VErrRequired, "id is required")
	case !idRE.MatchString(s.ID):
		v.add(ptr(path, "id"), VErrID, "bad id")
	case v.ids[s.ID]:
		v.add(ptr(path, "id"), VErrID, "duplicate section id")
	default:
		v.ids[s.ID] = true
	}
	if s.Kind == "" {
		v.add(ptr(path, "kind"), VErrRequired, "kind is required")
	} else if !enums["kind"][s.Kind] {
		v.add(ptr(path, "kind"), VErrKind, fmt.Sprintf("unknown kind %q", trunc(s.Kind, 40)))
	}
	switch s.Kind {
	case KindCode:
		if depth > 0 {
			v.add(ptr(path, "kind"), VErrKind, "a code section can only be a top-level section")
		}
		for _, m := range CheckCodeBinds(s.Content[ContentBinds]) {
			v.add(ptr(path, "content", ContentBinds), VErrEnum, m)
		}
		if s.Local != nil && v.opts.UnfilteredHTML {
			raw, _ := json.Marshal(s.Local)
			for _, p := range checkLocal(s.Local, len(raw)) {
				v.add(ptr(path, append([]any{"local"}, p.path...)...), VErrCSS, p.msg)
			}
		}
	case KindLoop:
		v.loopDepth++
		defer func() { v.loopDepth-- }()
		if v.loopDepth > MaxLoopDepth {
			v.add(path, VErrTooDeep, fmt.Sprintf("loops nested deeper than %d", MaxLoopDepth))
		}
		v.loop(s, path)
	case KindNav, KindFooter:
		v.binding(s, path)
	}
	if len(s.Items) > MaxItems {
		v.add(ptr(path, "items"), VErrTooMany, fmt.Sprintf("more than %d items", MaxItems))
		return
	}
	for k, val := range s.Content {
		kp := ptr(path, "content", k)
		if !contentKeyRE.MatchString(k) {
			v.add(kp, VErrEnum, "bad content key")
			continue
		}
		switch {
		case k == "html":
		case isHrefKey(k):
			if val != "" {
				if _, ok := SafeHref(val); !ok {
					v.add(kp, VErrURL, "link must be http(s), mailto, tel, #anchor or relative")
				}
			}
		case imageKeyRE.MatchString(k):
			v.imageOrCSS(kp, val)
		case k == "icon":
			v.icon(kp, val)
		}
	}
	itemIDs := map[string]bool{}
	for i := range s.Items {
		v.item(s, &s.Items[i], ptr(path, "items", i), depth, itemIDs)
	}
	v.style(&s.Style, ptr(path, "style"))
	if s.Box != nil {
		v.box(s.Box, ptr(path, "box"))
	}
	for k := range s.Els {
		es := s.Els[k]
		if !elKeyRE.MatchString(k) {
			v.add(ptr(path, "els", k), VErrEnum, "bad element key")
			continue
		}
		v.elStyle(&es, ptr(path, "els", k))
	}
	if g := s.Grid; g != nil {
		v.enum(ptr(path, "grid", "shadow"), "shadow", g.Shadow)
		v.css(ptr(path, "grid", "bg"), g.Bg)
		v.css(ptr(path, "grid", "borderColor"), g.BorderColor)
		v.css(ptr(path, "grid", "iconColor"), g.IconColor)
	}
	if b := s.Bg; b != nil {
		bp := ptr(path, "bg")
		v.enum(ptr(bp, "type"), "bgType", b.Type)
		v.enum(ptr(bp, "decor"), "decor", b.Decor)
		v.enum(ptr(bp, "decorAnim"), "decorAnim", b.DecorAnim)
		v.css(ptr(bp, "gradFrom"), b.GradFrom)
		v.css(ptr(bp, "gradTo"), b.GradTo)
		v.css(ptr(bp, "decorColor"), b.DecorColor)
		v.imageOrCSS(ptr(bp, "image"), b.Image)
	}
}

func (v *validator) item(s *Section, it *Item, path string, depth int, ids map[string]bool) {
	switch {
	case it.ID == "":
		v.add(ptr(path, "id"), VErrRequired, "id is required")
	case !idRE.MatchString(it.ID):
		v.add(ptr(path, "id"), VErrID, "bad id")
	case ids[it.ID]:
		v.add(ptr(path, "id"), VErrID, "duplicate item id")
	default:
		ids[it.ID] = true
	}
	if it.Parent != "" && !idRE.MatchString(it.Parent) {
		v.add(ptr(path, "parent"), VErrID, "bad parent id")
	}
	v.enum(ptr(path, "node"), "node", it.Node)
	if it.Tag != "" && !tagRE.MatchString(it.Tag) {
		v.add(ptr(path, "tag"), VErrEnum, "bad tag")
	}
	if it.Href != "" {
		if _, ok := SafeHref(it.Href); !ok {
			v.add(ptr(path, "href"), VErrURL, "link must be http(s), mailto, tel, #anchor or relative")
		}
	}
	v.icon(ptr(path, "icon"), it.Icon)
	// Image sources held in title (custom image/icon nodes) or text (layout image cells).
	if s.Kind == KindCustom && (it.Node == "image" || it.Node == "icon") && it.Title != "" {
		v.image(ptr(path, "title"), it.Title)
	}
	if s.Kind == KindLayout && it.Title == "image" && it.Text != "" {
		v.image(ptr(path, "text"), it.Text)
	}
	if it.Box != nil {
		v.enum(ptr(path, "box", "valign"), "sea", it.Box.Valign)
		v.enum(ptr(path, "box", "halign"), "sea", it.Box.Halign)
		v.css(ptr(path, "box", "bg"), it.Box.Bg)
	}
	if l := it.Lay; l != nil {
		lp := ptr(path, "lay")
		v.enum(ptr(lp, "dir"), "layDir", l.Dir)
		v.enum(ptr(lp, "align"), "seas", l.Align)
		v.enum(ptr(lp, "justify"), "layJustify", l.Justify)
		v.enum(ptr(lp, "self"), "seas", l.Self)
	}
	if it.Dyn != nil {
		v.dyn(it.Dyn, ptr(path, "dyn"))
	}
	if it.Sub != nil {
		v.section(it.Sub, ptr(path, "sub"), depth+1)
	}
}

func (v *validator) style(st *SectionStyle, path string) {
	v.enum(ptr(path, "palette"), "palette", st.Palette)
	v.enum(ptr(path, "align"), "secAlign", st.Align)
	v.enum(ptr(path, "anim"), "anim", st.Anim)
	for i, d := range st.HideOn {
		v.enum(ptr(path, "hideOn", i), "device", d)
	}
	v.css(ptr(path, "titleColor"), st.TitleColor)
	v.css(ptr(path, "bg"), st.Bg)
	if st.CSSID != "" && !cssIDRE.MatchString(st.CSSID) {
		v.add(ptr(path, "cssId"), VErrID, "CSS id must be a letter followed by letters, digits, - or _")
	}
	if st.CSS != "" && v.opts.UnfilteredHTML {
		if _, _, err := SanitizeCSS(st.CSS, CSSOptions{}); err != nil {
			v.add(ptr(path, "css"), VErrCSS, err.Error())
		}
	}
	if st.Motion != nil {
		v.motion(st.Motion, ptr(path, "motion"))
	}
}

func (v *validator) motion(m *MotionSettings, path string) {
	v.enum(ptr(path, "entrance"), "entrance", m.Entrance)
	v.enum(ptr(path, "easing"), "easing", m.Easing)
	v.enum(ptr(path, "hover"), "hover", m.Hover)
	v.enum(ptr(path, "parallaxTarget"), "parallaxOn", m.ParallaxTarget)
	v.enum(ptr(path, "hoverTarget"), "hoverOn", m.HoverTarget)
}

func (v *validator) box(b *ContainerBox, path string) {
	v.enum(ptr(path, "contentWidth"), "contentW", b.ContentWidth)
	v.enum(ptr(path, "widthUnit"), "widthUnit", b.WidthUnit)
	v.enum(ptr(path, "minHeightUnit"), "heightUnit", b.MinHeightUnit)
	v.enum(ptr(path, "direction"), "direction", b.Direction)
	v.enum(ptr(path, "justify"), "boxJustify", b.Justify)
	v.enum(ptr(path, "align"), "boxAlign", b.Align)
	v.enum(ptr(path, "borderStyle"), "borderStyle", b.BorderStyle)
	v.enum(ptr(path, "shadow"), "shadow", b.Shadow)
	v.css(ptr(path, "borderColor"), b.BorderColor)
}

func (v *validator) elStyle(e *ElStyle, path string) {
	v.enum(ptr(path, "align"), "elAlign", e.Align)
	v.enum(ptr(path, "widthUnit"), "widthUnit", e.WidthUnit)
	v.enum(ptr(path, "maxWidthUnit"), "maxUnit", e.MaxWidthUnit)
	v.enum(ptr(path, "heightUnit"), "heightUnit", e.HeightUnit)
	v.enum(ptr(path, "fit"), "fit", e.Fit)
	v.enum(ptr(path, "transform"), "transform", e.Transform)
	if e.Border != "" && !CSSValueOK(e.Border) && !BorderSidesOK(e.Border) {
		v.css(ptr(path, "border"), e.Border)
	}
	for k, val := range map[string]string{"color": e.Color, "bg": e.Bg, "iconColor": e.IconColor, "shadow": e.Shadow, "bgSize": e.BgSize, "bgPos": e.BgPos, "list": e.List} {
		v.css(ptr(path, k), val)
	}
	for from, to := range e.IconColors {
		if !CSSValueOK(from) || !CSSValueOK(to) {
			v.add(ptr(path, "iconColors", from), VErrCSS, "bad color")
		}
	}
	if e.Font != "" && !fontNameRE.MatchString(e.Font) {
		v.add(ptr(path, "font"), VErrEnum, "bad font name")
	}
	v.imageOrCSS(ptr(path, "bgImage"), e.BgImage)
	if e.Motion != nil {
		v.motion(e.Motion, ptr(path, "motion"))
	}
}

func (v *validator) site(s *SiteSettings, path string) {
	v.enum(ptr(path, "fontPair"), "fontPair", s.FontPair)
	v.enum(ptr(path, "corners"), "corners", s.Corners)
	v.css(ptr(path, "brand"), s.Brand)
	for k, f := range map[string]string{"headingFont": s.HeadingFont, "bodyFont": s.BodyFont} {
		if f != "" && !fontNameRE.MatchString(f) {
			v.add(ptr(path, k), VErrEnum, "bad font name")
		}
	}
}

func (v *validator) css(path, val string) {
	if val != "" && !CSSValueOK(val) {
		v.add(path, VErrCSS, "unsafe CSS value")
	}
}

func (v *validator) image(path, val string) {
	if val != "" && !ImageURLOK(val, true) {
		v.add(path, VErrURL, "image must be http(s), relative or a small data:image URI")
	}
}

// imageOrCSS: a plain URL, or a CSS image value (url(...), gradients).
func (v *validator) imageOrCSS(path, val string) {
	if val == "" {
		return
	}
	if strings.Contains(strings.ToLower(val), "url(") || strings.Contains(val, "gradient(") {
		v.css(path, val)
		return
	}
	v.image(path, val)
}

// icon: an icon name from the upstream set, or an image URL.
func (v *validator) icon(path, val string) {
	if val == "" {
		return
	}
	if looksLikeURL(val) {
		v.image(path, val)
		return
	}
	if !iconNameRE.MatchString(val) {
		v.add(path, VErrEnum, "bad icon name")
	}
}

func isHrefKey(k string) bool { return k == "href" || strings.HasSuffix(k, "Href") }

func isURLKey(k string) bool { return isHrefKey(k) || imageKeyRE.MatchString(k) }

func isDataURI(v string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "data:image/") && ImageURLOK(v, true)
}

func looksLikeURL(v string) bool {
	p := strings.TrimSpace(v)
	return strings.HasPrefix(p, "/") || strings.Contains(p, ":") || strings.HasPrefix(p, ".")
}
