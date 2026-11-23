package webbuilder

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Dynamic snapshot engine (contract §11.5). A published snapshot is sanitized HTML whose
// elements may carry placeholder attributes (data-wb-slot, data-wb-if, data-wb-loop, …). Compile
// turns it into a Program once; Exec fills it per request from a Resolver that reads what the
// front request already computed. The engine is pure: it never queries anything itself.
//
//   - text slots replace the element's content with the escaped value;
//   - html slots replace it with HTML the resolver produced from a trusted core generator;
//   - attr slots write only SlotAttrs (URLs through the scheme check) and keep the content;
//   - data-wb-if / data-wb-if-not drop the element when the slot is empty / not empty;
//   - data-wb-loop / data-wb-menu-loc repeat their single data-wb-loop-item child per item,
//     show data-wb-loop-empty only without items, nest at most MaxLoopDepth deep and stop after
//     MaxLoopIterations items;
//   - unknown slots render empty and are never passed to the resolver;
//   - data-wb-text fills only the element's first non-blank direct text node (code bindings);
//   - a menu may hold several data-wb-loop-item templates: each item picks the one whose tokens
//     name it ("k:<menu item id>", ItemKeyer), else "parent" for an item with sub-items, else
//     "default", else the first; data-wb-menu-sub inside a template repeats its own templates per
//     sub-item (SubMenuer).
//
// A snapshot without placeholders compiles to its own text (no parse, no re-serialization).

// ErrExecTruncated is returned by Exec when loops stopped early because the output reached
// MaxExecBytes. The output is still well-formed HTML (every open element is closed).
var ErrExecTruncated = errors.New("webbuilder: dynamic output truncated")

// MaxExecBytes bounds the output of one Exec: loops stop adding items beyond it.
const MaxExecBytes = 16 << 20

// SlotArgs are the per-element settings a resolver may use. The engine validates them (contract
// §11.4 rules) before passing them on; an invalid value arrives as the zero value.
type SlotArgs struct {
	Tag    string // the element's tag name, lower case ("time", "img", "a", …)
	Format string // data-wb-format, DynFormatPattern; "" = the site's date_format
	Size   string // data-wb-size, one of DynImageSizes; "" = the resolver's default
	Tax    string // data-wb-tax, SlugPattern; "" = category
	Sep    string // data-wb-sep, ≤ MaxDynSepRunes runes; "" = the resolver's default
	Words  int    // data-wb-words, MinDynWords…MaxDynWords; 0 = not set
	Field  string // data-wb-field, FieldPattern: a custom field name; "" = none
	Item   string // data-wb-item, IDPattern: the cell whose stored DynSettings apply; "" = none
	Raw    bool   // data-wb-raw: the value as stored (code bindings, AttrRaw)
	Meta   bool   // data-wb-meta: a custom field may fall back to the post's own meta (code bindings, AttrMeta)
	Tree   bool   // data-wb-menu-tree: a code menu (top level only; ItemKeyer and SubMenuer items)
	// Dropdown: data-wb-menu-tree="dropdown", a builder nav's drop-down menu: Tree, and each
	// top-level item's sub-menu lists all its descendants depth first (one drop-down per item).
	Dropdown bool
}

// Resolver supplies slot values for one scope (the request, a loop item or a menu item). Item
// resolvers fall back to their parent for slots outside their own scope (site.*, post.* of
// the main post); the engine never does that itself.
//
// Values are "empty" when ok is false or the value is "" (Attrs: no entries). For a text slot on
// a <time> element the engine also asks Attrs for the same slot and writes only its datetime.
type Resolver interface {
	// Text returns plain text; the engine escapes it.
	Text(slot SlotName, a SlotArgs) (string, bool)
	// HTML returns trusted HTML (theContent, theExcerpt, paginateLinks, escaped term links, …).
	HTML(slot SlotName, a SlotArgs) (string, bool)
	// Attrs returns attributes; only SlotAttrs are written, URLs are scheme-checked.
	Attrs(slot SlotName, a SlotArgs) (map[string]string, bool)
	// Loop returns the items of the loop section id (its spec comes from the stored doc).
	Loop(id string, a SlotArgs) (LoopIter, bool)
	// Menu returns the top-level items of a menu location.
	Menu(location string, a SlotArgs) (LoopIter, bool)
}

// ItemKeyer is implemented by item resolvers whose item may pick its own template among several
// data-wb-loop-item children (a code section's menu: the design's own markup of each imported
// item). ItemKey is the token naming the item ("k:12"); HasChildren selects a "parent" template.
type ItemKeyer interface {
	ItemKey() string
	HasChildren() bool
}

// SubMenuer is implemented by menu item resolvers: the item's sub-items (data-wb-menu-sub).
type SubMenuer interface {
	SubMenu() (LoopIter, bool)
}

// LoopIter yields one Resolver per item. The engine calls Next at most MaxLoopIterations times
// per loop; a nil Resolver renders the item with the parent resolver.
type LoopIter interface {
	Next() (Resolver, bool)
}

// SliceIter is a LoopIter over a slice.
type SliceIter struct {
	Items []Resolver
	i     int
}

func (s *SliceIter) Next() (Resolver, bool) {
	if s.i >= len(s.Items) {
		return nil, false
	}
	s.i++
	return s.Items[s.i-1], true
}

// SlotSet is a set of slot names.
type SlotSet map[SlotName]bool

// Has reports whether the set contains slot.
func (s SlotSet) Has(slot SlotName) bool { return s[slot] }

// HasPrefix reports whether the set contains a slot whose name starts with p ("shop.").
func (s SlotSet) HasPrefix(p string) bool {
	for k, present := range s {
		if present && strings.HasPrefix(string(k), p) {
			return true
		}
	}
	return false
}

// Program is a compiled snapshot. It is immutable and safe for concurrent Exec.
type Program struct {
	segs []seg
	// Uses lists the known slots the snapshot reads (data-wb-slot, data-wb-if, data-wb-if-not),
	// so a resolver can skip expensive values (post.content) nobody shows.
	Uses SlotSet
	// Loops are the data-wb-loop section ids, in document order, unique.
	Loops []string
	// Menus are the data-wb-menu-loc locations, in document order, unique.
	Menus []string
	// HasDynamic is false for a snapshot without placeholders (one static segment).
	HasDynamic bool
}

// Static returns the program's text when it has no placeholders.
func (p *Program) Static() (string, bool) {
	if p.HasDynamic {
		return "", false
	}
	if len(p.segs) == 0 {
		return "", true
	}
	return p.segs[0].static, true
}

type segRole uint8

const (
	roleNormal segRole = iota
	roleItem           // the loop-item template of the enclosing loop element
	roleEmpty          // the loop-empty child of the enclosing loop element
)

type seg struct {
	static string
	el     *dynElem
	role   segRole
	keys   []string // roleItem: the template's tokens (data-wb-loop-item value)
}

type cond struct {
	slot  SlotName
	known bool
	not   bool // data-wb-if-not
}

type dynElem struct {
	tag     string
	attrs   []html.Attribute
	void    bool
	newline bool // pre/textarea/listing whose first child text starts with "\n"
	conds   []cond
	args    SlotArgs

	slot     SlotName
	slotMode SlotMode // "" = no slot; known slot otherwise
	unknown  bool     // data-wb-slot names an unknown slot: render empty

	loop    string // data-wb-loop section id ("" = none)
	menu    string // data-wb-menu-loc location
	isLoop  bool
	isMenu  bool
	badLoop bool   // invalid id / location: zero items
	part    int    // data-wb-loop-part: 0 none, 1 first half, 2 second half
	isSub   bool   // data-wb-menu-sub: the current menu item's sub-items
	sep     string // loops: the white space written between two items (itemSeparators)

	// ownText: this is the replaced text node of a data-wb-text element (lead and trail are the
	// node's surrounding white space); slot is the text slot.
	ownText     bool
	lead, trail string

	kids []seg
}

// Placeholder attributes that make an element dynamic.
var dynMarkers = []string{AttrSlot, AttrIf, AttrIfNot, AttrLoop, AttrMenuLoc, AttrText, AttrMenuSub}

var (
	dynFormatRE  = regexp.MustCompile(DynFormatPattern)
	slugRE       = regexp.MustCompile(SlugPattern)
	fieldRE      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	menuLocRE    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	classTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	dynSizeSet   = toSet(DynImageSizes)
	slotAttrSet  = toSet(SlotAttrs)
	ariaCurrent  = toSet([]string{"page", "step", "location", "date", "time", "true", "false"})
)

// hasPlaceholders is the static fast path: no marker text, no parse.
func hasPlaceholders(src string) bool {
	for _, m := range dynMarkers {
		if strings.Contains(src, m) {
			return true
		}
	}
	// HTML attribute names are case-insensitive, including in unsanitized fragments.
	if strings.ContainsAny(src, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		lower := strings.ToLower(src)
		for _, m := range dynMarkers {
			if strings.Contains(lower, m) {
				return true
			}
		}
	}
	return false
}

// Compile compiles sanitized snapshot HTML (SanitizeSnapshot output, or any fragment).
func Compile(src string) (*Program, error) {
	if !hasPlaceholders(src) {
		return staticProgram(src), nil
	}
	c := &compiler{p: &Program{Uses: SlotSet{}}, dyn: map[*html.Node]bool{}, loops: map[string]bool{}, menus: map[string]bool{}}
	var segs []seg
	if strings.Contains(src, AttrCodePage) || strings.Contains(src, codeHeadOpen) {
		// A snapshot with code sections: only the elements that carry placeholders (with what is
		// inside them) are parsed; every other byte stays the design's (the head template, the
		// shell's wrappers and the markup the front end matches as text).
		var err error
		if segs, err = c.spliced(src); err != nil {
			return nil, err
		}
	} else {
		nodes, err := parseFragment(src)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			c.mark(n)
		}
		for _, n := range nodes {
			segs = c.node(n, segs)
		}
	}
	c.p.segs = segs
	c.p.HasDynamic = false
	for _, s := range segs {
		if s.el != nil {
			c.p.HasDynamic = true
			break
		}
	}
	if !c.p.HasDynamic {
		// Marker text only inside text or attribute values (or on SVG): keep the source as is.
		return staticProgram(src), nil
	}
	return c.p, nil
}

// spliced compiles the outermost elements carrying placeholders one by one (parsed in the context
// of their parent element) and keeps the source between them as it is.
func (c *compiler) spliced(src string) ([]seg, error) {
	root := parseSource(src)
	var segs []seg
	last := 0
	var walk func(n *srcNode) error
	walk = func(n *srcNode) error {
		for _, k := range n.kids {
			if k.tag == "" {
				continue
			}
			marked := false
			for _, a := range k.attrs {
				for _, m := range dynMarkers {
					if a.Key == m {
						marked = true
					}
				}
			}
			if !marked {
				if err := walk(k); err != nil {
					return err
				}
				continue
			}
			ctx := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
			if p := k.parent; p != nil && p.tag != "#root" && p.tag != "" {
				ctx = &html.Node{Type: html.ElementNode, Data: p.tag, DataAtom: atom.Lookup([]byte(p.tag))}
			}
			nodes, err := html.ParseFragment(strings.NewReader(src[k.start:k.end]), ctx)
			if err != nil {
				return err
			}
			segs = appendStatic(segs, src[last:k.start])
			for _, x := range nodes {
				c.mark(x)
			}
			for _, x := range nodes {
				segs = c.node(x, segs)
			}
			last = k.end
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return appendStatic(segs, src[last:]), nil
}

func staticProgram(src string) *Program {
	p := &Program{Uses: SlotSet{}}
	if src != "" {
		p.segs = []seg{{static: src}}
	}
	return p
}

type compiler struct {
	p     *Program
	dyn   map[*html.Node]bool
	loops map[string]bool
	menus map[string]bool
}

// isDynElement: an HTML element carrying a placeholder attribute (SVG content is always static).
func isDynElement(n *html.Node) bool {
	if n.Type != html.ElementNode || n.Namespace != "" {
		return false
	}
	for _, a := range n.Attr {
		if a.Namespace != "" {
			continue
		}
		for _, m := range dynMarkers {
			if a.Key == m {
				return true
			}
		}
	}
	return false
}

// mark computes, bottom-up, which nodes have a dynamic element in their subtree.
func (c *compiler) mark(n *html.Node) bool {
	d := false
	if n.Type == html.ElementNode && n.Namespace == "" {
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			if c.mark(k) {
				d = true
			}
		}
		if isDynElement(n) {
			d = true
		}
	}
	if d {
		c.dyn[n] = true
	}
	return d
}

func appendStatic(segs []seg, s string) []seg {
	if s == "" {
		return segs
	}
	if k := len(segs); k > 0 && segs[k-1].el == nil && segs[k-1].role == roleNormal {
		segs[k-1].static += s
		return segs
	}
	return append(segs, seg{static: s})
}

func renderStatic(n *html.Node) string {
	var b strings.Builder
	_ = html.Render(&b, n)
	return b.String()
}

func (c *compiler) node(n *html.Node, segs []seg) []seg {
	if !c.dyn[n] {
		return appendStatic(segs, renderStatic(n))
	}
	return append(segs, seg{el: c.element(n)})
}

var voidElements = toSet([]string{"area", "base", "br", "col", "embed", "hr", "img", "input", "keygen", "link", "meta", "param", "source", "track", "wbr"})

func (c *compiler) element(n *html.Node) *dynElem {
	e := &dynElem{tag: n.Data, void: voidElements[n.Data]}
	e.attrs = append(e.attrs, n.Attr...)
	e.args = slotArgs(n)
	for _, a := range n.Attr {
		if a.Namespace != "" {
			continue
		}
		switch a.Key {
		case AttrIf, AttrIfNot:
			name := SlotName(strings.TrimSpace(a.Val))
			_, known := Slots[name]
			e.conds = append(e.conds, cond{slot: name, known: known, not: a.Key == AttrIfNot})
			if known {
				c.p.Uses[name] = true
			}
		}
	}
	if v, ok := getAttr(n, AttrLoop); ok {
		e.isLoop = true
		e.loop = strings.TrimSpace(v)
		if !idPatternRE.MatchString(e.loop) {
			e.badLoop = true
		} else if !c.loops[e.loop] {
			c.loops[e.loop] = true
			c.p.Loops = append(c.p.Loops, e.loop)
		}
	} else if v, ok := getAttr(n, AttrMenuLoc); ok {
		e.isMenu = true
		e.menu = strings.TrimSpace(v)
		if !menuLocRE.MatchString(e.menu) {
			e.badLoop = true
		} else if !c.menus[e.menu] {
			c.menus[e.menu] = true
			c.p.Menus = append(c.p.Menus, e.menu)
		}
		switch p, _ := getAttr(n, AttrLoopPart); strings.TrimSpace(p) {
		case "1/2":
			e.part = 1
		case "2/2":
			e.part = 2
		}
	} else if hasAttr(n, AttrMenuSub) {
		e.isSub = true
	} else if v, ok := getAttr(n, AttrSlot); ok {
		name := SlotName(strings.TrimSpace(v))
		if mode, known := Slots[name]; known {
			e.slot, e.slotMode = name, mode
			c.p.Uses[name] = true
		} else {
			e.unknown = true
		}
	}
	if e.void {
		return e
	}
	if e.unknown || e.slotMode == SlotText || e.slotMode == SlotHTML {
		// The content is replaced: its children are never rendered.
		return e
	}
	if fc := n.FirstChild; fc != nil && fc.Type == html.TextNode && strings.HasPrefix(fc.Data, "\n") {
		switch n.Data {
		case "pre", "listing", "textarea":
			e.newline = true
		}
	}
	// data-wb-text: the first direct text node that is not blank takes the slot's value.
	var textNode *html.Node
	var textSlot SlotName
	if v, ok := getAttr(n, AttrText); ok {
		name := SlotName(strings.TrimSpace(v))
		if Slots[name] == SlotText {
			for k := n.FirstChild; k != nil; k = k.NextSibling {
				if k.Type == html.TextNode && strings.TrimSpace(k.Data) != "" {
					textNode, textSlot = k, name
					c.p.Uses[name] = true
					break
				}
			}
		}
	}
	seenItem, seenEmpty := false, false
	for k := n.FirstChild; k != nil; k = k.NextSibling {
		if k == textNode {
			t := k.Data
			body := strings.TrimSpace(t)
			i := strings.Index(t, body)
			e.kids = append(e.kids, seg{el: &dynElem{ownText: true, slot: textSlot, slotMode: SlotText, args: e.args,
				lead: t[:i], trail: t[i+len(body):]}})
			continue
		}
		if e.isLoop || e.isMenu || e.isSub {
			if k.Type == html.ElementNode && hasAttr(k, AttrLoopItem) {
				if seenItem && e.isLoop {
					continue // a post loop has exactly one card template
				}
				seenItem = true
				sg := c.roleSeg(k, roleItem)
				v, _ := getAttr(k, AttrLoopItem)
				sg.keys = strings.Fields(v)
				e.kids = append(e.kids, sg)
				continue
			}
			if k.Type == html.ElementNode && hasAttr(k, AttrLoopEmpty) {
				if seenEmpty {
					continue
				}
				seenEmpty = true
				e.kids = append(e.kids, c.roleSeg(k, roleEmpty))
				continue
			}
		}
		e.kids = c.node(k, e.kids)
	}
	if e.isLoop || e.isMenu || e.isSub {
		e.kids, e.sep = itemSeparators(e.kids)
	}
	return e
}

// itemSeparators keeps the source's white space between repeated items: the blank text right
// after the first item template separates every two items; blank text between templates (a code
// menu keeps each item's markup as a template) is dropped.
func itemSeparators(kids []seg) ([]seg, string) {
	first, last := -1, -1
	for i := range kids {
		if kids[i].role == roleItem {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return kids, ""
	}
	blank := func(s *seg) bool {
		return s.el == nil && s.role == roleNormal && strings.TrimSpace(s.static) == ""
	}
	sep := ""
	if first+1 < len(kids) && blank(&kids[first+1]) {
		sep = kids[first+1].static
	}
	out := kids[:0:0]
	for i := range kids {
		if i > first && i < last && blank(&kids[i]) {
			continue
		}
		out = append(out, kids[i])
	}
	return out, sep
}

func (c *compiler) roleSeg(n *html.Node, role segRole) seg {
	if !c.dyn[n] {
		return seg{static: renderStatic(n), role: role}
	}
	return seg{el: c.element(n), role: role}
}

// slotArgs reads and validates the data-wb-format|size|tax|sep|words|item settings of n.
func slotArgs(n *html.Node) SlotArgs {
	a := SlotArgs{Tag: strings.ToLower(n.Data)}
	if v, ok := getAttr(n, AttrFormat); ok && dynFormatRE.MatchString(v) {
		a.Format = v
	}
	if v, ok := getAttr(n, AttrSize); ok && dynSizeSet[v] {
		a.Size = v
	}
	if v, ok := getAttr(n, AttrTax); ok && slugRE.MatchString(v) {
		a.Tax = v
	}
	if v, ok := getAttr(n, AttrSep); ok && utf8.ValidString(v) && utf8.RuneCountInString(v) <= MaxDynSepRunes {
		a.Sep = v
	}
	if v, ok := getAttr(n, AttrField); ok && fieldRE.MatchString(v) {
		a.Field = v
	}
	if v, ok := getAttr(n, AttrItem); ok && idPatternRE.MatchString(v) {
		a.Item = v
	}
	if hasAttr(n, AttrRaw) {
		a.Raw = true
	}
	if hasAttr(n, AttrMeta) {
		a.Meta = true
	}
	if v, ok := getAttr(n, AttrMenuTree); ok {
		a.Tree = true
		a.Dropdown = strings.TrimSpace(v) == "dropdown"
	}
	if v, ok := getAttr(n, AttrWords); ok {
		if w, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && w >= MinDynWords && w <= MaxDynWords {
			a.Words = w
		}
	}
	return a
}

// ---------------------------------------------------------------------------------------------
// Exec.

// Exec writes the filled program to w. r may be nil (every slot empty, every loop without items).
func (p *Program) Exec(w *strings.Builder, r Resolver) error {
	if r == nil {
		r = nopResolver{}
	}
	x := &execer{w: w, limit: w.Len() + MaxExecBytes}
	x.segs(p.segs, r)
	if x.truncated {
		return ErrExecTruncated
	}
	return nil
}

// ExecString is Exec into a new string.
func (p *Program) ExecString(r Resolver) (string, error) {
	var b strings.Builder
	err := p.Exec(&b, r)
	return b.String(), err
}

type nopResolver struct{}

func (nopResolver) Text(SlotName, SlotArgs) (string, bool)             { return "", false }
func (nopResolver) HTML(SlotName, SlotArgs) (string, bool)             { return "", false }
func (nopResolver) Attrs(SlotName, SlotArgs) (map[string]string, bool) { return nil, false }
func (nopResolver) Loop(string, SlotArgs) (LoopIter, bool)             { return nil, false }
func (nopResolver) Menu(string, SlotArgs) (LoopIter, bool)             { return nil, false }

type execer struct {
	w         *strings.Builder
	depth     int
	limit     int
	truncated bool
}

func (x *execer) segs(ss []seg, r Resolver) {
	for i := range ss {
		if ss[i].role != roleNormal {
			continue // only rendered by the enclosing loop
		}
		x.seg(&ss[i], r)
	}
}

func (x *execer) seg(s *seg, r Resolver) {
	if s.el == nil {
		x.w.WriteString(s.static)
		return
	}
	x.elem(s.el, r)
}

// empty reports whether slot has no value for r.
func empty(slot SlotName, a SlotArgs, r Resolver) bool {
	switch Slots[slot] {
	case SlotText:
		v, ok := r.Text(slot, a)
		return !ok || v == ""
	case SlotHTML:
		v, ok := r.HTML(slot, a)
		return !ok || strings.TrimSpace(v) == ""
	case SlotAttr:
		m, ok := r.Attrs(slot, a)
		return !ok || len(m) == 0
	}
	return true
}

func (x *execer) elem(e *dynElem, r Resolver) {
	if e.ownText {
		v, ok := r.Text(e.slot, e.args)
		if !ok {
			v = ""
		}
		x.w.WriteString(e.lead)
		x.w.WriteString(html.EscapeString(v))
		x.w.WriteString(e.trail)
		return
	}
	for _, c := range e.conds {
		isEmpty := !c.known || empty(c.slot, e.args, r)
		if isEmpty != c.not {
			return
		}
	}
	switch {
	case e.isLoop || e.isMenu || e.isSub:
		x.loop(e, r)
		return
	case e.unknown:
		x.start(e, nil)
		x.end(e)
		return
	}
	switch e.slotMode {
	case SlotText:
		v, ok := r.Text(e.slot, e.args)
		if !ok {
			v = ""
		}
		var extra map[string]string
		if e.tag == "time" {
			if m, ok := r.Attrs(e.slot, e.args); ok && m["datetime"] != "" {
				extra = map[string]string{"datetime": m["datetime"]}
			}
		}
		x.start(e, extra)
		if !e.void {
			x.w.WriteString(html.EscapeString(v))
		}
		x.end(e)
	case SlotHTML:
		v, ok := r.HTML(e.slot, e.args)
		if !ok {
			v = ""
		}
		x.start(e, nil)
		if !e.void {
			x.w.WriteString(v)
		}
		x.end(e)
	case SlotAttr:
		m, ok := r.Attrs(e.slot, e.args)
		if (!ok || len(m) == 0) && e.tag == "img" {
			return // an image without a source is dropped
		}
		if !ok {
			m = nil
		}
		x.start(e, m)
		x.segs(e.kids, r)
		x.end(e)
	default:
		x.start(e, nil)
		x.segs(e.kids, r)
		x.end(e)
	}
}

func (x *execer) loop(e *dynElem, r Resolver) {
	var item, emptySeg *seg
	var templates []*seg
	for i := range e.kids {
		switch e.kids[i].role {
		case roleItem:
			if item == nil {
				item = &e.kids[i]
			}
			templates = append(templates, &e.kids[i])
		case roleEmpty:
			emptySeg = &e.kids[i]
		}
	}
	var items []Resolver
	if !e.badLoop && item != nil && x.depth < MaxLoopDepth {
		var it LoopIter
		var ok bool
		switch {
		case e.isLoop:
			it, ok = r.Loop(e.loop, e.args)
		case e.isSub:
			if sm, is := r.(SubMenuer); is {
				it, ok = sm.SubMenu()
			}
		default:
			it, ok = r.Menu(e.menu, e.args)
		}
		if ok && it != nil {
			for len(items) < MaxLoopIterations {
				ir, more := it.Next()
				if !more {
					break
				}
				if ir == nil {
					ir = r
				}
				items = append(items, ir)
			}
		}
	}
	if e.part != 0 {
		half := (len(items) + 1) / 2
		if e.part == 1 {
			items = items[:half]
		} else {
			items = items[half:]
		}
	}
	x.start(e, nil)
	x.depth++
	for i := range e.kids {
		k := &e.kids[i]
		switch k.role {
		case roleNormal:
			x.seg(k, r)
		case roleItem:
			if k != item {
				continue // the other templates render where the first one is
			}
			for n, ir := range items {
				if x.w.Len() >= x.limit {
					x.truncated = true
					break
				}
				if n > 0 && e.sep != "" {
					x.w.WriteString(e.sep)
				}
				x.seg(pickTemplate(templates, ir), ir)
			}
		case roleEmpty:
			if len(items) == 0 && emptySeg != nil {
				x.seg(emptySeg, r)
			}
		}
	}
	x.depth--
	x.end(e)
}

// pickTemplate chooses an item's template (see the file comment).
func pickTemplate(templates []*seg, ir Resolver) *seg {
	if len(templates) == 1 {
		return templates[0]
	}
	find := func(tok string) *seg {
		for _, s := range templates {
			for _, k := range s.keys {
				if k == tok {
					return s
				}
			}
		}
		return nil
	}
	if k, ok := ir.(ItemKeyer); ok {
		if key := k.ItemKey(); key != "" {
			if s := find(key); s != nil {
				return s
			}
		}
		if k.HasChildren() {
			if s := find("parent"); s != nil {
				return s
			}
		}
	}
	if s := find("default"); s != nil {
		return s
	}
	return templates[0]
}

var attrEscaper = strings.NewReplacer(`&`, "&amp;", `'`, "&#39;", `<`, "&lt;", `>`, "&gt;", `"`, "&#34;", "\r", "&#13;")

// start writes the start tag with extra slot attributes applied (only SlotAttrs, checked).
func (x *execer) start(e *dynElem, extra map[string]string) {
	attrs := e.attrs
	if len(extra) > 0 {
		attrs = mergeSlotAttrs(e.tag, e.attrs, extra)
	}
	w := x.w
	w.WriteByte('<')
	w.WriteString(e.tag)
	for _, a := range attrs {
		w.WriteByte(' ')
		if a.Namespace != "" {
			w.WriteString(a.Namespace)
			w.WriteByte(':')
		}
		w.WriteString(a.Key)
		w.WriteString(`="`)
		attrEscaper.WriteString(w, a.Val)
		w.WriteByte('"')
	}
	if e.void {
		w.WriteString("/>")
		return
	}
	w.WriteByte('>')
	if e.newline {
		w.WriteByte('\n')
	}
}

func (x *execer) end(e *dynElem) {
	if e.void {
		return
	}
	x.w.WriteString("</")
	x.w.WriteString(e.tag)
	x.w.WriteByte('>')
}

// mergeSlotAttrs applies extra to a copy of attrs: unknown names and unsafe values are skipped,
// class tokens are appended, every other attribute is replaced or added. Order is SlotAttrs order.
func mergeSlotAttrs(tag string, attrs []html.Attribute, extra map[string]string) []html.Attribute {
	out := append([]html.Attribute(nil), attrs...)
	for _, k := range SlotAttrs {
		raw, ok := extra[k]
		if !ok {
			continue
		}
		v, ok := slotAttrValue(tag, k, raw)
		if !ok {
			continue
		}
		idx := -1
		for i := range out {
			if out[i].Namespace == "" && out[i].Key == k {
				idx = i
				break
			}
		}
		if k == "class" && idx >= 0 {
			have := strings.Fields(out[idx].Val)
			set := toSet(have)
			for _, t := range strings.Fields(v) {
				if !set[t] {
					have = append(have, t)
					set[t] = true
				}
			}
			out[idx].Val = strings.Join(have, " ")
			continue
		}
		if idx >= 0 {
			out[idx].Val = v
		} else {
			out = append(out, html.Attribute{Key: k, Val: v})
		}
	}
	return out
}

// slotAttrValue checks one attribute value written by a slot.
func slotAttrValue(tag, key, v string) (string, bool) {
	if !slotAttrSet[key] {
		return "", false
	}
	switch key {
	case "src", "srcset", "sizes":
		// Image slots only write image attributes on images: on an iframe, video or embed the
		// value would replace a src the sanitizer vetted under that element's own rule.
		if tag != "img" && tag != "source" {
			return "", false
		}
	}
	switch key {
	case "href":
		return SafeHref(v)
	case "src":
		v = strings.TrimSpace(v)
		return v, ImageURLOK(v, tag == "img")
	case "srcset":
		return srcsetOK(v)
	case "sizes":
		return v, v != "" && !urlRefRE.MatchString(v) && CSSValueOK(v)
	case "width", "height":
		v = strings.TrimSpace(v)
		return v, numberishRE.MatchString(v)
	case "aria-current":
		v = strings.TrimSpace(v)
		return v, ariaCurrent[v]
	case "class":
		var toks []string
		for _, t := range strings.Fields(v) {
			if classTokenRE.MatchString(t) {
				toks = append(toks, t)
			}
		}
		return strings.Join(toks, " "), len(toks) > 0
	case "alt", "datetime":
		return v, utf8.ValidString(v)
	case "target":
		v = strings.TrimSpace(v)
		return v, v == "_blank" || v == "_self" || v == "_parent" || v == "_top"
	}

	return "", false
}

// ---------------------------------------------------------------------------------------------
// Compiled program cache.

// ProgramCache is an LRU of compiled programs. Compile keys them ProgramKey(postID, rev, html):
// snapshots without placeholders are never hashed or stored there (the static fast path costs
// one scan). CompileFor takes the caller's key instead and keeps static programs too, so a hit
// neither hashes nor scans the snapshot. The cache holds at most max programs and about
// CompiledCacheBytes of snapshot text.
type ProgramCache struct {
	mu       sync.Mutex
	max      int
	maxBytes int64
	bytes    int64
	ll       *list.List
	m        map[string]*list.Element
}

type cacheEntry struct {
	key  string
	p    *Program
	size int64
}

// NewProgramCache returns an LRU holding at most max programs (≤ 0 = CompiledCacheSize).
func NewProgramCache(max int) *ProgramCache {
	if max <= 0 {
		max = CompiledCacheSize
	}
	return &ProgramCache{max: max, maxBytes: CompiledCacheBytes, ll: list.New(), m: map[string]*list.Element{}}
}

// ProgramKey is "postID:rev:sha256(html)[:16]" (hex).
func ProgramKey(postID int64, rev int, src string) string {
	sum := sha256.Sum256([]byte(src))
	return strconv.FormatInt(postID, 10) + ":" + strconv.Itoa(rev) + ":" + hex.EncodeToString(sum[:])[:16]
}

// Compile returns the cached program of src, compiling it on a miss.
func (c *ProgramCache) Compile(postID int64, rev int, src string) (*Program, error) {
	if !hasPlaceholders(src) {
		return staticProgram(src), nil
	}
	key := ProgramKey(postID, rev, src)
	if p := c.get(key); p != nil {
		return p, nil
	}
	p, err := Compile(src)
	if err != nil {
		return nil, err
	}
	return c.put(key, p, int64(len(src))), nil
}

// CompileFor returns the program cached under key, compiling src() on a miss. key must identify
// the text src returns (a stored snapshot's post id, revision and update time, and whatever the
// caller changes in it): on a hit src is not called. Programs without placeholders are cached
// too. A failed compile is not cached.
func (c *ProgramCache) CompileFor(key string, src func() string) (*Program, error) {
	if p := c.get(key); p != nil {
		return p, nil
	}
	text := src()
	p, err := Compile(text)
	if err != nil {
		return nil, err
	}
	return c.put(key, p, int64(len(text))), nil
}

func (c *ProgramCache) get(key string) *Program {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheEntry).p
	}
	return nil
}

// put stores p under key (the program already stored there wins) and evicts the least recently
// used programs over the bounds. size is the length of the program's source.
func (c *ProgramCache) put(key string, p *Program, size int64) *Program {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheEntry).p
	}
	c.m[key] = c.ll.PushFront(&cacheEntry{key: key, p: p, size: size})
	c.bytes += size
	for c.ll.Len() > c.max || (c.bytes > c.maxBytes && c.ll.Len() > 1) {
		old := c.ll.Back()
		c.ll.Remove(old)
		e := old.Value.(*cacheEntry)
		delete(c.m, e.key)
		c.bytes -= e.size
	}
	return p
}

// Len is the number of cached programs.
func (c *ProgramCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Purge empties the cache.
func (c *ProgramCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.m = map[string]*list.Element{}
	c.bytes = 0
}
