package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
	"wpgo/internal/wp"
)

// Theme Builder slots on the server (contract §11.5): the webbuilder.Resolver that fills a
// published snapshot's placeholders from the request's siteView, and the post loops.
//
// Values come from what buildSiteView already computed (v.Posts, v.Post, v.PostNav, v.Comments,
// v.Menus, archive title, search query, pagination). A custom loop (source query or related) adds
// one QueryPosts and, when a card shows terms, one batched TermsForPosts. Expensive values are
// produced only when the snapshot reads the slot (the engine asks for a slot only when an element
// uses it): theContent of a loop post, its featured image, its author.
//
// Escaping: text slots are escaped by the engine; html slots only return markup of trusted core
// generators (theTitle, theContent, paginateLinks, the theme's comments template) or values this
// file escapes itself (term links, excerpts); attr slots return raw values that the engine checks
// (SafeHref for href, ImageURLOK for src, …) before writing them.

// wbPrograms caches compiled snapshots: wbFill's by wbProgramKey, the others by
// webbuilder.ProgramKey (postID:rev:sha256(html)[:16]).
var wbPrograms = webbuilder.NewProgramCache(webbuilder.CompiledCacheSize)

// wbTrustSource is the stored snapshot of d, its stored scripts given the nonce placeholder when
// trust (cspTrustContent).
func wbTrustSource(d *store.WebBuilderDoc, trust bool) string {
	if trust {
		return cspTrustScripts(d.HTML)
	}
	return d.HTML
}

// wbTrustedHTML is cspTrustContent(ctx, d.HTML): the text of the snapshot's compiled program
// when it has no placeholders (a program without placeholders keeps its source as it is), else
// read for the response.
func (s *Server) wbTrustedHTML(ctx context.Context, d *store.WebBuilderDoc) string {
	trust := s.cspTrustPage(ctx)
	if wbDocKeyed(d) {
		prog, err := wbPrograms.CompileFor(wbProgramKey(d, trust), func() string { return wbTrustSource(d, trust) })
		if err == nil {
			if text, ok := prog.Static(); ok {
				return text
			}
		}
	}
	return wbTrustSource(d, trust)
}

// wbProgramKey identifies the snapshot wbFill compiles: the stored doc's revision (wbDocKey: a
// publish changes the html with a new revision and update time) and whether its stored scripts
// carry the nonce placeholder (the placeholder is the process's own, so the text is the same for
// every request with the same answer).
func wbProgramKey(d *store.WebBuilderDoc, trust bool) string {
	if trust {
		return "fill:" + wbDocKey(d) + ":trusted"
	}
	return "fill:" + wbDocKey(d)
}

// --- loop specs ----------------------------------------------------------------------------------

// wbLoopDef is a loop section of a stored doc.
type wbLoopDef struct {
	spec webbuilder.LoopSpec
	// typeSet: content.postType was given (related loops default to the current post's type).
	typeSet bool
}

// wbLoopSpecs caches the loop sections of stored docs, keyed "postID:rev:updated" (a publish
// bumps rev; the time keeps two databases of one process, as in tests, apart).
var wbLoopSpecs struct {
	sync.Mutex
	m map[string]map[string]wbLoopDef
}

const wbLoopSpecsMax = 512

// wbLoopDefs reads the loop sections of a stored doc (never the snapshot's attributes).
func wbLoopDefs(d *store.WebBuilderDoc) map[string]wbLoopDef {
	key := itoa64(d.PostID) + ":" + strconv.Itoa(d.Rev) + ":" + strconv.FormatInt(d.UpdatedAt.UnixNano(), 36)
	wbLoopSpecs.Lock()
	if m, ok := wbLoopSpecs.m[key]; ok {
		wbLoopSpecs.Unlock()
		return m
	}
	wbLoopSpecs.Unlock()
	out := map[string]wbLoopDef{}
	var doc webbuilder.DocState
	if len(d.Doc) > 0 {
		if err := json.Unmarshal(d.Doc, &doc); err != nil {
			log.Printf("web builder: doc %d: %v", d.PostID, err)
		}
	}
	var walk func(s *webbuilder.Section, depth int)
	walk = func(s *webbuilder.Section, depth int) {
		if s.Kind == webbuilder.KindLoop && s.ID != "" {
			if _, seen := out[s.ID]; !seen {
				out[s.ID] = wbLoopDef{spec: webbuilder.ParseLoopSpec(s.Content), typeSet: strings.TrimSpace(s.Content[webbuilder.LoopPostType]) != ""}
			}
		}
		if depth > webbuilder.MaxDepth+1 {
			return
		}
		for i := range s.Items {
			if sub := s.Items[i].Sub; sub != nil {
				walk(sub, depth+1)
			}
		}
	}
	for i := range doc.Sections {
		walk(&doc.Sections[i], 0)
	}
	// Loops bound to elements of code sections (codebind.go): a custom query unless set otherwise.
	for key, spec := range webbuilder.CodeLoops(&doc) {
		if _, seen := out[key]; !seen {
			out[key] = wbLoopDef{spec: spec, typeSet: true}
		}
	}
	wbLoopSpecs.Lock()
	if wbLoopSpecs.m == nil || len(wbLoopSpecs.m) >= wbLoopSpecsMax {
		wbLoopSpecs.m = map[string]map[string]wbLoopDef{}
	}
	wbLoopSpecs.m[key] = out
	wbLoopSpecs.Unlock()
	return out
}

// --- environment ---------------------------------------------------------------------------------

// wbSlotEnv is what the resolvers of one filled snapshot share.
type wbSlotEnv struct {
	s   *Server
	c   *gin.Context
	ctx context.Context
	v   *siteView
	lb  *links.Builder
	u   *store.User

	doc  *store.WebBuilderDoc
	prog *webbuilder.Program
	// own: the snapshot is the main post's own builder doc (its post.content would be itself).
	own bool

	main  *wbSlotPost // the singular post of the request, nil elsewhere
	users map[int64]*store.User
	loops bool // the snapshot has a post loop (cache tag wb-loop)
	// loopMemo: the posts of each loop id, resolved once per fill. A loop's posts depend only on
	// the doc and the request (never on the outer item of a nested loop), so every data-wb-loop
	// element of the same id, at any depth, shares one query.
	loopMemo map[string]wbLoopResult
	// publicTax: publicTaxonomy answers by taxonomy.
	publicTax map[string]bool

	logo     map[string]string
	logoDone bool
	// menusByID: menus a code section names by id ("menu-<id>"), loaded once per fill.
	menusByID map[int64][]*store.MenuItem

	shop *wbShopEnv // Commerce and language-switcher state (webbuilder_shop.go), lazy
}

// wbSlotPost is one post a resolver shows: the request's singular post, a main-loop item (both
// with the postView buildSiteView made) or a custom-loop item.
type wbSlotPost struct {
	p    *store.Post
	pv   *postView
	main bool

	content     *string
	terms       map[string][]*store.Term // by taxonomy; nil = not loaded
	author      *store.User
	authorDone  bool
	avatar      *string
	image       map[string]map[string]string // by size
	permalink   string
	permalinkOK bool
}

func newWBSlotEnv(s *Server, c *gin.Context, v *siteView, d *store.WebBuilderDoc, prog *webbuilder.Program, own bool) *wbSlotEnv {
	ctx := v.ctx
	if ctx == nil {
		ctx = c.Request.Context()
	}
	e := &wbSlotEnv{s: s, c: c, ctx: ctx, v: v, lb: s.links(ctx), u: currentUser(c), doc: d, prog: prog, own: own,
		users: map[int64]*store.User{}, loops: len(prog.Loops) > 0}
	if v.Post != nil && v.Q != nil && v.Q.Post != nil {
		switch v.Q.Kind {
		case "single", "page", "front-page", "attachment":
			e.main = &wbSlotPost{p: v.Q.Post, pv: v.Post, main: true}
		}
	}
	return e
}

// root is the resolver of the snapshot's top level: post.* is the singular post.
func (e *wbSlotEnv) root() *wbSlotResolver { return &wbSlotResolver{env: e, post: e.main} }

func (e *wbSlotEnv) user(id int64) *store.User {
	if id <= 0 {
		return nil
	}
	if u, ok := e.users[id]; ok {
		return u
	}
	u, err := e.s.store.UserByID(e.ctx, id)
	if err != nil {
		u = nil
	}
	e.users[id] = u
	return u
}

// --- resolver ------------------------------------------------------------------------------------

// wbSlotResolver resolves one scope: the snapshot (post = the singular post), a loop item (post)
// or a menu item (menu). Slots outside its scope go to the parent.
type wbSlotResolver struct {
	env  *wbSlotEnv
	post *wbSlotPost
	menu *store.MenuItem
	// menuDepth is the item's level in its menu (0 = top).
	menuDepth int
	// tree: an item of a code section's menu (data-wb-menu-tree): it picks its own template and
	// its sub-items fill the template's data-wb-menu-sub list (codebind.go).
	tree bool
	// dropdown: a top-level item of a builder nav's drop-down menu (data-wb-menu-tree="dropdown"):
	// its sub-menu lists all its descendants, depth first.
	dropdown bool
	parent   *wbSlotResolver
}

// ItemKey names the menu item for its template (webbuilder.ItemKeyer).
func (r *wbSlotResolver) ItemKey() string {
	if r.menu == nil || !r.tree {
		return ""
	}
	return "k:" + itoa64(r.menu.ID)
}

// HasChildren reports whether the menu item has sub-items (webbuilder.ItemKeyer).
func (r *wbSlotResolver) HasChildren() bool {
	return r.menu != nil && r.tree && len(r.menu.Children) > 0
}

// SubMenu is the item's sub-items (webbuilder.SubMenuer).
func (r *wbSlotResolver) SubMenu() (webbuilder.LoopIter, bool) {
	if r.menu == nil || !r.tree || len(r.menu.Children) == 0 {
		return nil, false
	}
	if r.dropdown {
		// One drop-down per top-level item: every level below it, depth first (the engine nests
		// loops only MaxLoopDepth deep, so a third level could not have a list of its own).
		var items []webbuilder.Resolver
		var walk func(list []*store.MenuItem, depth int)
		walk = func(list []*store.MenuItem, depth int) {
			for _, it := range list {
				if it == nil {
					continue
				}
				items = append(items, &wbSlotResolver{env: r.env, menu: it, menuDepth: depth, parent: r})
				if depth+1 < wbMenuMaxDepth && len(it.Children) > 0 {
					walk(it.Children, depth+1)
				}
			}
		}
		walk(r.menu.Children, r.menuDepth+1)
		return &webbuilder.SliceIter{Items: items}, true
	}
	items := make([]webbuilder.Resolver, 0, len(r.menu.Children))
	for _, it := range r.menu.Children {
		if it != nil {
			items = append(items, &wbSlotResolver{env: r.env, menu: it, menuDepth: r.menuDepth + 1, tree: true, parent: r})
		}
	}
	return &webbuilder.SliceIter{Items: items}, true
}

var _ webbuilder.Resolver = (*wbSlotResolver)(nil)

func (r *wbSlotResolver) nearestMenu() *store.MenuItem {
	for x := r; x != nil; x = x.parent {
		if x.menu != nil {
			return x.menu
		}
	}
	return nil
}

func (r *wbSlotResolver) nearestPost() *wbSlotPost {
	for x := r; x != nil; x = x.parent {
		if x.post != nil {
			return x.post
		}
	}
	return nil
}

func (r *wbSlotResolver) Text(slot webbuilder.SlotName, a webbuilder.SlotArgs) (string, bool) {
	e := r.env
	if wbItemSlot(slot) {
		return e.shopText(slot, a)
	}
	v := e.v
	switch slot {
	case webbuilder.SlotSiteTitle:
		return html.UnescapeString(v.SiteName), v.SiteName != ""
	case webbuilder.SlotSiteTagline:
		return html.UnescapeString(v.Description), v.Description != ""
	case webbuilder.SlotSiteYear:
		return strconv.Itoa(time.Now().In(e.s.loc()).Year()), true
	case webbuilder.SlotMenuLabel:
		if m := r.nearestMenu(); m != nil {
			l := wbPlain(m.Label)
			return l, l != ""
		}
		return "", false
	case webbuilder.SlotSearchQuery:
		if v.Q != nil && v.Q.Kind == "search" {
			return v.SearchQuery, v.SearchQuery != ""
		}
		return "", false
	case webbuilder.SlotSearchCount:
		if v.Q != nil && v.Q.Kind == "search" {
			return strconv.Itoa(v.TotalPosts), true
		}
		return "", false
	}
	sp := r.nearestPost()
	if sp == nil {
		return "", false
	}
	p := sp.p
	switch slot {
	case webbuilder.SlotPostField:
		return e.fieldText(p, a.Field, a.Meta)
	case webbuilder.SlotPostTitleText:
		// The title as stored (a design's own text: no texturizing, no title filters).
		t := strings.TrimSpace(html.UnescapeString(p.Title))
		return t, t != ""
	case webbuilder.SlotPostTermNames:
		tax := a.Tax
		if tax == "" {
			tax = "category"
		}
		if !e.publicTaxonomy(tax) {
			return "", false
		}
		sep := a.Sep
		if sep == "" {
			sep = ", "
		}
		var names []string
		for _, t := range sp.termsOf(e, tax) {
			names = append(names, html.UnescapeString(t.Name))
		}
		out := strings.Join(names, sep)
		return out, out != ""
	case webbuilder.SlotPostDate, webbuilder.SlotPostModified:
		t := p.Date()
		if slot == webbuilder.SlotPostModified {
			t = p.ModifiedAt
		}
		f := a.Format
		if f == "" {
			f = e.s.settings.String("date_format")
		}
		out := wp.FormatDate(t.In(e.s.loc()), f)
		return out, out != ""
	case webbuilder.SlotPostAuthor:
		if au := sp.authorOf(e); au != nil {
			return au.DisplayName, au.DisplayName != ""
		}
	case webbuilder.SlotPostAuthorBio:
		if au := sp.authorOf(e); au != nil {
			b := au.Profile["description"]
			return b, b != ""
		}
	case webbuilder.SlotPostCommentsCount:
		return strconv.Itoa(p.CommentCount), true
	}
	return "", false
}

func (r *wbSlotResolver) HTML(slot webbuilder.SlotName, a webbuilder.SlotArgs) (string, bool) {
	e := r.env
	if wbItemSlot(slot) {
		return e.shopHTML(slot, a)
	}
	v := e.v
	switch slot {
	case webbuilder.SlotArchiveTitle:
		t := e.archiveTitle()
		return t, t != ""
	case webbuilder.SlotArchiveDescription:
		if wbArchiveView(v) {
			return string(v.ArchiveDesc), v.ArchiveDesc != ""
		}
		return "", false
	case webbuilder.SlotLoopPagination:
		out := e.pagination()
		return out, out != ""
	}
	sp := r.nearestPost()
	if sp == nil {
		return "", false
	}
	p := sp.p
	switch slot {
	case webbuilder.SlotPostTitle:
		if sp.pv != nil {
			return string(sp.pv.Title), sp.pv.Title != ""
		}
		t := theTitle(p)
		return e.s.pluginTitle(e.ctx, e.s.pllUC(e.ctx, t, 0), p), t != "" // Multi Language user content, the_title
	case webbuilder.SlotPostContent:
		if sp.main && e.own {
			return "", false // the page's own doc: its content is this snapshot
		}
		if a.Raw && wbRawContent(p) && !e.memberHidden(p) {
			// A design's own markup imported as the post's content (a code binding): printed as
			// stored, as the design wrote it (the_content would texturize quotes and add <p>).
			return p.Content, strings.TrimSpace(p.Content) != ""
		}
		out := sp.contentOf(e)
		return out, strings.TrimSpace(out) != ""
	case webbuilder.SlotPostFieldHTML:
		if a.Field != "" && p.Password == "" && !e.memberHidden(p) {
			if v, ok := p.Meta[webbuilder.TrustedHTMLField+a.Field].(string); ok && strings.TrimSpace(v) != "" {
				return v, true // a site import's markup, protected meta (TrustedHTMLField)
			}
		}
		v, ok := e.fieldValue(p, a.Field, a.Meta).(string)
		if !ok || strings.TrimSpace(v) == "" {
			return "", false
		}
		return wp.KsesPost(v), true
	case webbuilder.SlotPostExcerpt:
		out := wbExcerpt(p, a.Words)
		return e.s.pllUC(e.ctx, out, 0), out != "" // Multi Language user content
	case webbuilder.SlotPostTerms:
		tax := a.Tax
		if tax == "" {
			tax = "category"
		}
		sep := a.Sep
		if sep == "" {
			sep = ", "
		}
		if !e.publicTaxonomy(tax) {
			return "", false // data-wb-tax comes from the snapshot: never print a private taxonomy
		}
		terms := sp.termsOf(e, tax)
		parts := make([]string, 0, len(terms))
		for _, t := range terms {
			parts = append(parts, `<a href="`+html.EscapeString(e.lb.TermLink(t))+`" rel="tag">`+html.EscapeString(t.Name)+`</a>`)
		}
		return strings.Join(parts, html.EscapeString(sep)), len(parts) > 0
	case webbuilder.SlotPostNav:
		if sp.main {
			return string(v.PostNav), v.PostNav != ""
		}
	case webbuilder.SlotPostComments:
		if sp.main {
			out := e.comments()
			return out, out != ""
		}
	}
	return "", false
}

func (r *wbSlotResolver) Attrs(slot webbuilder.SlotName, a webbuilder.SlotArgs) (map[string]string, bool) {
	e := r.env
	if wbItemSlot(slot) {
		return e.shopAttrs(slot, a)
	}
	switch slot {
	case webbuilder.SlotSiteURL:
		// The home page in the request language (Pursae Multi Language), as themes link it.
		return map[string]string{"href": e.lb.Front()}, true
	case webbuilder.SlotSiteLogo:
		m := e.siteLogo()
		return m, len(m) > 0
	case webbuilder.SlotMenuLink:
		// A code section's menu item: the URL and target only; the design's own active-link rule
		// marks the current item (webbuilder.ApplyActive).
		m := r.nearestMenu()
		if m == nil {
			return nil, false
		}
		out := map[string]string{"href": m.URL}
		if m.Target != "" {
			out["target"] = m.Target
		}
		return out, true
	case webbuilder.SlotMenuURL:
		m := r.nearestMenu()
		if m == nil {
			return nil, false
		}
		out := map[string]string{"href": m.URL}
		var cls []string
		for x := r; x != nil; x = x.parent {
			if x.menu != nil {
				if x.menuDepth > 0 {
					cls = append(cls, "wb-menu-d"+strconv.Itoa(x.menuDepth))
				}
				break
			}
		}
		if len(m.Children) > 0 {
			cls = append(cls, "has-children")
		}
		if m.Current {
			out["aria-current"] = "page"
			cls = append(cls, "is-current")
		}
		if len(cls) > 0 {
			out["class"] = strings.Join(cls, " ")
		}
		return out, true
	}
	sp := r.nearestPost()
	if sp == nil {
		return nil, false
	}
	p := sp.p
	switch slot {
	case webbuilder.SlotPostFieldURL:
		u := e.fieldURL(p, a.Field, a.Meta)
		if u == "" {
			return nil, false
		}
		if a.Tag == "img" {
			return map[string]string{"src": u}, true
		}
		return map[string]string{"href": u}, true
	case webbuilder.SlotPostURL:
		return map[string]string{"href": sp.permalinkOf(e)}, true
	case webbuilder.SlotPostCommentsURL:
		return map[string]string{"href": sp.permalinkOf(e) + "#comments"}, true
	case webbuilder.SlotPostDate:
		return map[string]string{"datetime": p.Date().In(e.s.loc()).Format(time.RFC3339)}, true
	case webbuilder.SlotPostModified:
		return map[string]string{"datetime": p.ModifiedAt.In(e.s.loc()).Format(time.RFC3339)}, true
	case webbuilder.SlotPostAuthorURL:
		if au := sp.authorOf(e); au != nil {
			return map[string]string{"href": e.lb.AuthorLink(au.ID, au.Nicename)}, true
		}
	case webbuilder.SlotPostAvatar:
		if src := sp.avatarOf(e); src != "" {
			alt := ""
			if au := sp.authorOf(e); au != nil {
				alt = au.DisplayName
			}
			return map[string]string{"src": src, "alt": alt}, true
		}
	case webbuilder.SlotPostImage:
		if a.Raw {
			// A code binding: the image as the design shows it, its source and text only (the
			// design's markup sizes it); without a featured image the URL field data-wb-field names.
			if full := sp.imageOf(e, "full"); full["src"] != "" {
				return map[string]string{"src": full["src"], "alt": full["alt"]}, true
			}
			if u := e.fieldURL(p, a.Field, a.Meta); u != "" {
				out := map[string]string{"src": u}
				if alt, ok := e.fieldText(p, a.Field+"_alt", a.Meta); ok {
					out["alt"] = alt
				}
				return out, true
			}
			return nil, false
		}
		m := sp.imageOf(e, a.Size)
		if len(m) == 0 && a.Field != "" {
			if u := e.fieldURL(p, a.Field, a.Meta); u != "" {
				return map[string]string{"src": u}, true
			}
		}
		return m, len(m) > 0
	case webbuilder.SlotPostFieldAttrs:
		// The per-post attributes of one element: the fields <name>_href, <name>_src, … (a site
		// import keeps there what differs between the pages of a group, webbuilder.ConvertGroups).
		if a.Field == "" {
			return nil, false
		}
		out := map[string]string{}
		for _, k := range webbuilder.SlotAttrs {
			if v, ok := e.fieldValue(p, a.Field+"_"+strings.ReplaceAll(k, "-", "_"), a.Meta).(string); ok && v != "" {
				out[k] = v
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}

// memberHidden reports whether Memberships keeps p's content from the visitor: its fields and raw
// markup are part of that content, so they go where the_content's restriction message goes.
func (e *wbSlotEnv) memberHidden(p *store.Post) bool {
	return e.s != nil && p != nil && e.s.gmemHidesPost(e.ctx, e.u, p.ID)
}

// wbRawContent reports whether a post's content is printed as stored by a code binding: imported
// markup (it starts with an element and holds no blocks) of a post without a password.
func wbRawContent(p *store.Post) bool {
	c := strings.TrimSpace(p.Content)
	return p.Password == "" && strings.HasPrefix(c, "<") && !strings.Contains(c, "<!-- wp:")
}

func (r *wbSlotResolver) Loop(id string, _ webbuilder.SlotArgs) (webbuilder.LoopIter, bool) {
	posts, ok := r.env.loopPosts(id)
	if !ok {
		return nil, false
	}
	items := make([]webbuilder.Resolver, 0, len(posts))
	for _, sp := range posts {
		items = append(items, &wbSlotResolver{env: r.env, post: sp, parent: r})
	}
	return &webbuilder.SliceIter{Items: items}, true
}

// wbMenuMaxDepth is how many menu levels a menu-bound list shows (the top level plus sub-menus).
const wbMenuMaxDepth = 3

// Menu lists the items of a menu location depth first: each item is followed by its sub-menu
// items, up to wbMenuMaxDepth levels. A sub-item's link gets the classes wb-menu-d<level> (1 and
// up) and a parent's link has-children, so a card template (or the site CSS) can indent or
// fold the levels; the top level carries none and renders as before.
func (r *wbSlotResolver) Menu(location string, a webbuilder.SlotArgs) (webbuilder.LoopIter, bool) {
	menu := r.env.v.Menus[location]
	if len(menu) == 0 {
		menu = r.env.menuByID(webbuilder.MenuIDOfLoc(location))
	}
	if len(menu) == 0 {
		return nil, false
	}
	var items []webbuilder.Resolver
	if a.Tree {
		// A code section's menu: the top level; each item brings its sub-items (SubMenu).
		for _, it := range menu {
			if it != nil {
				items = append(items, &wbSlotResolver{env: r.env, menu: it, tree: true, dropdown: a.Dropdown, parent: r})
			}
		}
		return &webbuilder.SliceIter{Items: items}, true
	}
	var walk func(list []*store.MenuItem, depth int)
	walk = func(list []*store.MenuItem, depth int) {
		for _, it := range list {
			if it == nil {
				continue
			}
			items = append(items, &wbSlotResolver{env: r.env, menu: it, menuDepth: depth, parent: r})
			if depth+1 < wbMenuMaxDepth && len(it.Children) > 0 {
				walk(it.Children, depth+1)
			}
		}
	}
	walk(menu, 0)
	return &webbuilder.SliceIter{Items: items}, true
}

// menuByID loads a menu by id (a code section's menu binding), its URLs resolved and its
// current item marked as for a theme location.
func (e *wbSlotEnv) menuByID(id int64) []*store.MenuItem {
	if id <= 0 {
		return nil
	}
	if m, ok := e.menusByID[id]; ok {
		return m
	}
	if e.menusByID == nil {
		e.menusByID = map[int64][]*store.MenuItem{}
	}
	items, err := e.s.store.MenuTree(e.ctx, id)
	if err != nil {
		items = nil
	}
	if len(items) > 0 {
		items = e.s.resolveMenu(e.ctx, items, e.lb, e.v.Q)
	}
	e.menusByID[id] = items
	return items
}

// fieldValue is the formatted Pursae Custom Fields value of a post's field; nil when the plugin is
// off, the field is unset, or the post is password protected (its content is not shown either).
//
// The name must be a registered field (a field group defines it): markup can print the fields the
// site shows, never other meta of a post it can loop over (a plugin's or a user's non-underscore
// meta is not public just because it lacks the underscore; WordPress exposes meta only when it is
// registered with show_in_rest).
// meta (a code binding's element, webbuilder.AttrMeta, which the snapshot sanitizer drops from an
// author's markup) adds one more source: the post's own meta string for a name a site import
// stores for the parts of a design's page that are not its title or content
// (webbuilder.SiteFieldName: design_<n>, list<n>_<k> and their _href / _src / _alt companions),
// so an imported site keeps working with Pursae Custom Fields off.
func (e *wbSlotEnv) fieldValue(p *store.Post, name string, meta bool) any {
	if e.s == nil || p == nil || name == "" || p.Password != "" || strings.HasPrefix(name, "_") || e.memberHidden(p) {
		return nil
	}
	if v := e.s.acfRegisteredField(e.ctx, name, p); v != nil || !meta {
		return v
	}
	if !webbuilder.SiteFieldName(name) {
		return nil
	}
	if s, ok := p.Meta[name].(string); ok && s != "" {
		return s
	}
	return nil
}

// acfRegisteredField is get_field(name, post) formatted, for a name some field group defines
// only: nil for plain meta (get_field would return its raw value).
func (s *Server) acfRegisteredField(ctx context.Context, name string, p *store.Post) any {
	if !s.acfgoOn() {
		return nil
	}
	o := s.acfgoObject(ctx, p, nil)
	if o == nil || o.Kind == "block" {
		return nil
	}
	f := s.acfgoFind(ctx, o, name)
	if f == nil {
		return nil
	}
	return s.acfgoLoad(ctx, o, f, "", true)
}

// fieldText is the field as plain text (the engine escapes it); lists join with ", ".
func (e *wbSlotEnv) fieldText(p *store.Post, name string, meta bool) (string, bool) {
	var out string
	switch t := e.fieldValue(p, name, meta).(type) {
	case nil:
		return "", false
	case []any:
		var parts []string
		for _, x := range t {
			if d := acfgoDisplay(x); d != "" {
				parts = append(parts, d)
			}
		}
		out = strings.Join(parts, ", ")
	default:
		out = acfgoDisplay(t)
	}
	out = wbPlain(out)
	return out, out != ""
}

// fieldURL is a URL of the field: a plain URL string, or the url of a link or image field.
func (e *wbSlotEnv) fieldURL(p *store.Post, name string, meta bool) string {
	switch t := e.fieldValue(p, name, meta).(type) {
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		if u, ok := t["url"].(string); ok {
			return strings.TrimSpace(u)
		}
	}
	return ""
}

// --- values --------------------------------------------------------------------------------------

// wbArchiveView: the request lists posts (posts index, term, author, date, type archive, search).
func wbArchiveView(v *siteView) bool {
	if v == nil || v.Q == nil {
		return false
	}
	switch v.Q.Kind {
	case "home", "category", "tag", "taxonomy", "author", "date", "search", "post-type-archive":
		return true
	}
	return false
}

// archiveTitle is get_the_archive_title() for archive views (escaped HTML); the posts index shows
// the posts page title, search results "Search Results for: …".
func (e *wbSlotEnv) archiveTitle() string {
	v := e.v
	if !wbArchiveView(v) {
		return ""
	}
	switch v.Q.Kind {
	case "search":
		return "Search Results for: <span>" + html.EscapeString(v.SearchQuery) + "</span>"
	case "home":
		if v.Q.PostsPage != nil {
			return wp.Texturize(wp.EscText(v.Q.PostsPage.Title))
		}
		return "Posts"
	}
	if v.ArchiveTitle != "" {
		return string(v.ArchiveTitle)
	}
	t, _ := v.Q.archiveTitle()
	return t
}

// pagination is loop.pagination: the main query's links, in the mode of the snapshot's first
// main loop (numbers when it has none; none hides them). Singular views have no main pagination.
func (e *wbSlotEnv) pagination() string {
	v := e.v
	if !wbArchiveView(v) || v.TotalPages < 2 {
		return ""
	}
	mode := "numbers"
	defs := wbLoopDefs(e.doc)
	for _, id := range e.prog.Loops {
		if d, ok := defs[id]; ok && d.spec.Source == webbuilder.LoopSourceMain {
			mode = d.spec.Pagination
			break
		}
	}
	switch mode {
	case "none":
		return ""
	case "prev-next":
		return wbPrevNext(e.lb, e.s.archiveBase(v.Q, e.lb), max(v.Q.Paged, 1), v.TotalPages)
	}
	return string(v.Pagination)
}

// wbPrevNext is the_posts_pagination() reduced to the previous / next links.
func wbPrevNext(lb *links.Builder, base string, current, total int) string {
	var out []string
	if current > 1 {
		out = append(out, fmt.Sprintf(`<a class="prev page-numbers" href="%s">Previous page</a>`, html.EscapeString(lb.Paged(base, current-1))))
	}
	if current < total {
		out = append(out, fmt.Sprintf(`<a class="next page-numbers" href="%s">Next page</a>`, html.EscapeString(lb.Paged(base, current+1))))
	}
	if len(out) == 0 {
		return ""
	}
	return `
	<nav class="navigation pagination" aria-label="Posts pagination">
		<h2 class="screen-reader-text">Posts pagination</h2>
		<div class="nav-links">` + strings.Join(out, "\n") + `</div>
	</nav>`
}

// comments is the theme's comments template for the singular post ("" when the theme has none).
func (e *wbSlotEnv) comments() string {
	v := e.v
	if v.Comments == nil || v.Theme == nil {
		return ""
	}
	set := e.s.tmpl.getTheme(true, v.Theme.Slug)
	if set == nil || set.Lookup("comments") == nil {
		return ""
	}
	var b bytes.Buffer
	if err := set.ExecuteTemplate(&b, "comments", v.templateView()); err != nil { // public copy (site_view.go)
		log.Printf("web builder: comments template: %v", err)
		return ""
	}
	return b.String()
}

// siteLogo is the img attributes of the custom logo of the rendered theme.
func (e *wbSlotEnv) siteLogo() map[string]string {
	if e.logoDone {
		return e.logo
	}
	e.logoDone = true
	v := e.v
	if v.Theme == nil || !themeSupportsLogo(v.Theme) {
		return nil
	}
	mods := v.ThemeMods
	if mods == nil {
		mods = e.s.themeMods(v.Theme.Slug)
	}
	id := int64(modInt(mods, "custom_logo", 0))
	if id <= 0 {
		return nil
	}
	a, err := e.s.store.GetAttachment(e.ctx, id)
	if err != nil {
		return nil
	}
	e.logo = e.s.wbImageAttrs(e.ctx, a, "full", html.UnescapeString(v.SiteName))
	return e.logo
}

// wbImageAttrs is wp_get_attachment_image() as attributes: the size closest to the requested one
// (large when it has no such variant), srcset over the generated sizes, sizes and alt.
func (s *Server) wbImageAttrs(ctx context.Context, a *store.Attachment, size, altFallback string) map[string]string {
	if a == nil || a.FilePath == "" {
		return nil
	}
	src, w, h := s.uploadURL(ctx, a.FilePath), a.Width, a.Height
	if size != "full" {
		if vr := a.Variant(size); vr != nil {
			src, w, h = s.uploadURL(ctx, vr.FilePath), vr.Width, vr.Height
		} else if vr := a.Variant("large"); vr != nil {
			src, w, h = s.uploadURL(ctx, vr.FilePath), vr.Width, vr.Height
		}
	}
	out := map[string]string{"src": src}
	alt := a.Alt
	if alt == "" {
		alt = altFallback
	}
	out["alt"] = alt
	if w > 0 && h > 0 {
		out["width"], out["height"] = strconv.Itoa(w), strconv.Itoa(h)
	}
	if a.Width > 0 {
		set := []string{fmt.Sprintf("%s %dw", s.uploadURL(ctx, a.FilePath), a.Width)}
		for _, n := range []string{"medium", "medium_large", "large", "1536x1536", "2048x2048"} {
			if vr := a.Variant(n); vr != nil && vr.Width > 0 {
				set = append(set, fmt.Sprintf("%s %dw", s.uploadURL(ctx, vr.FilePath), vr.Width))
			}
		}
		if len(set) > 1 {
			out["srcset"] = strings.Join(set, ", ")
			if w > 0 {
				out["sizes"] = fmt.Sprintf("(max-width: %dpx) 100vw, %dpx", w, w)
			}
		}
	}
	return out
}

// wbExcerpt is the post's excerpt as escaped text: the manual excerpt or the content before
// <!--more-->, trimmed to words (55 when 0) with an ellipsis. Plain text, so it fits the
// paragraph the widget draws around it.
func wbExcerpt(p *store.Post, words int) string {
	if p.Password != "" {
		return "There is no excerpt because this is a protected post."
	}
	if words <= 0 {
		words = 55
	}
	src := p.Excerpt
	if src == "" {
		src, _, _, _ = wp.SplitMore(stripBlocks(p.Content))
		src = shortcodeRe.ReplaceAllString(src, "")
	}
	return strings.TrimSpace(wp.TrimWords(src, words, "&hellip;"))
}

func (sp *wbSlotPost) permalinkOf(e *wbSlotEnv) string {
	if sp.pv != nil {
		return sp.pv.Permalink
	}
	if !sp.permalinkOK {
		sp.permalink, sp.permalinkOK = e.lb.Permalink(sp.p), true
	}
	return sp.permalink
}

func (sp *wbSlotPost) authorOf(e *wbSlotEnv) *store.User {
	if !sp.authorDone {
		sp.author, sp.authorDone = e.user(sp.p.AuthorID), true
	}
	return sp.author
}

func (sp *wbSlotPost) avatarOf(e *wbSlotEnv) string {
	if sp.avatar == nil {
		src := ""
		if au := sp.authorOf(e); au != nil && au.Email != "" {
			if a := e.s.avatarFor(e.ctx, au.Email, 96); a != defaultAvatar {
				src = a
			}
		}
		sp.avatar = &src
	}
	return *sp.avatar
}

func (sp *wbSlotPost) imageOf(e *wbSlotEnv, size string) map[string]string {
	if size == "" {
		size = "large"
	}
	if m, ok := sp.image[size]; ok {
		return m
	}
	if sp.image == nil {
		sp.image = map[string]map[string]string{}
	}
	var m map[string]string
	if sp.p.FeaturedMediaID > 0 {
		if a, err := e.s.store.GetAttachment(e.ctx, sp.p.FeaturedMediaID); err == nil {
			m = e.s.wbImageAttrs(e.ctx, a, size, "")
		}
	}
	sp.image[size] = m
	return m
}

// contentOf is the_content() of the post: the postView's when buildSiteView made one, else
// rendered once on first use.
func (sp *wbSlotPost) contentOf(e *wbSlotEnv) string {
	if sp.pv != nil {
		return string(sp.pv.Content)
	}
	if sp.content == nil {
		out := e.s.pllUC(e.ctx, e.s.theContent(e.ctx, e.c, sp.p, false, e.lb), sp.p.ID) // Multi Language user content
		sp.content = &out
	}
	return *sp.content
}

// publicTaxonomy reports whether the terms of tax may be shown to visitors: the built-in
// public taxonomies, Pursae Commerce's product categories and tags, and public Pursae Custom
// Fields taxonomies.
func (e *wbSlotEnv) publicTaxonomy(tax string) bool {
	switch tax {
	case "category", "post_tag", "product_cat", "product_tag":
		return true
	}
	if v, ok := e.publicTax[tax]; ok {
		return v
	}
	pub := false
	if e.s != nil {
		for _, tx := range e.s.acfgoTaxonomies(e.ctx) {
			if tx.Name == tax {
				pub = tx.Public
				break
			}
		}
	}
	if e.publicTax == nil {
		e.publicTax = map[string]bool{}
	}
	e.publicTax[tax] = pub
	return pub
}

// termsOf is the post's terms of a taxonomy (loop items arrive with theirs batched).
func (sp *wbSlotPost) termsOf(e *wbSlotEnv, tax string) []*store.Term {
	if sp.terms == nil {
		sp.terms = map[string][]*store.Term{}
		if m, err := e.s.wbTermsForPosts(e.ctx, []int64{sp.p.ID}); err == nil {
			sp.terms = m[sp.p.ID]
		}
	}
	return sp.terms[tax]
}

// wbTermsForPosts is every term of the posts, by post and taxonomy, in one query
// (store.TermsForPosts reads one taxonomy).
func (s *Server) wbTermsForPosts(ctx context.Context, ids []int64) (map[int64]map[string][]*store.Term, error) {
	out := map[int64]map[string][]*store.Term{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT pt.post_id, t.id, t.taxonomy, coalesce(t.parent_id,0), t.slug, t.name
		FROM post_terms pt JOIN terms t ON t.id = pt.term_id WHERE pt.post_id = ANY($1)
		ORDER BY pt.post_id, t.taxonomy, pt.sort_order, lower(t.name)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		t := &store.Term{}
		if err := rows.Scan(&pid, &t.ID, &t.Taxonomy, &t.ParentID, &t.Slug, &t.Name); err != nil {
			return nil, err
		}
		if out[pid] == nil {
			out[pid] = map[string][]*store.Term{}
		}
		out[pid][t.Taxonomy] = append(out[pid][t.Taxonomy], t)
	}
	return out, rows.Err()
}

// --- loops ---------------------------------------------------------------------------------------

// loopPosts are the posts of the loop section id of the stored doc: the request's own posts
// (main), a custom query or the related posts of the singular post. ok is false when the doc has
// no such loop (a forged or stale data-wb-loop id).
func (e *wbSlotEnv) loopPosts(id string) ([]*wbSlotPost, bool) {
	if r, ok := e.loopMemo[id]; ok {
		return r.posts, r.ok
	}
	posts, ok := e.queryLoop(id)
	if e.loopMemo == nil {
		e.loopMemo = map[string]wbLoopResult{}
	}
	e.loopMemo[id] = wbLoopResult{posts, ok}
	return posts, ok
}

// wbLoopResult is one memoized loopPosts answer.
type wbLoopResult struct {
	posts []*wbSlotPost
	ok    bool
}

// queryLoop runs the query of loop id (see loopPosts).
func (e *wbSlotEnv) queryLoop(id string) ([]*wbSlotPost, bool) {
	def, ok := wbLoopDefs(e.doc)[id]
	if !ok {
		return nil, false
	}
	sp := def.spec
	if sp.Source == webbuilder.LoopSourceMain {
		if !wbArchiveView(e.v) {
			return nil, true
		}
		out := make([]*wbSlotPost, 0, len(e.v.Posts))
		for _, pv := range e.v.Posts {
			if pv.Post != nil {
				out = append(out, &wbSlotPost{p: pv.Post, pv: pv})
			}
		}
		return out, true
	}
	typ := sp.PostType
	var termIDs []int64
	exclude := []int64{}
	if sp.Source == webbuilder.LoopSourceRelated {
		if e.main == nil {
			return nil, true
		}
		if !def.typeSet {
			typ = e.main.p.Type
		}
		taxes := map[string]bool{}
		for _, t := range sp.Terms {
			taxes[t.Taxonomy] = true
		}
		m, err := e.s.wbTermsForPosts(e.ctx, []int64{e.main.p.ID})
		if err != nil {
			return nil, true
		}
		for tax, terms := range m[e.main.p.ID] {
			if taxes[tax] {
				for _, t := range terms {
					termIDs = append(termIDs, t.ID)
				}
			}
		}
		if len(termIDs) == 0 {
			return nil, true
		}
		exclude = append(exclude, e.main.p.ID)
	} else {
		for _, t := range sp.Terms {
			if t.ID > 0 {
				termIDs = append(termIDs, t.ID)
			}
		}
		if sp.ExcludeCurrent && e.main != nil {
			exclude = append(exclude, e.main.p.ID)
		}
	}
	if !siteViewableType(typ) {
		return nil, true
	}
	pq := store.PostQuery{Types: []string{typ}, Statuses: []string{"publish"}, AuthorID: sp.Author, TermIDs: termIDs,
		OrderBy: sp.OrderBy, Order: sp.Order, Page: 1, PerPage: sp.PerPage + sp.Offset, Loc: e.s.loc()}
	if sp.Parent > 0 && sp.Source == webbuilder.LoopSourceQuery {
		parent := sp.Parent
		pq.ParentID = &parent
	}
	if e.u.Can("read_private_posts", nil) {
		pq.Statuses = append(pq.Statuses, "private")
	}
	pq.ExcludeID = append(exclude, e.s.gooListingKinds(withPluginUser(e.ctx, e.u), pq.Types)...) // Goocommerce Memberships
	random := sp.OrderBy == "rand"
	if random {
		pq.OrderBy, pq.Order, pq.PerPage = "date", "desc", webbuilder.MaxLoopIterations
	}
	posts, _, err := e.s.store.QueryPosts(e.ctx, pq)
	if err != nil {
		log.Printf("web builder: loop %s: %v", id, err)
		return nil, true
	}
	if random {
		rand.Shuffle(len(posts), func(i, j int) { posts[i], posts[j] = posts[j], posts[i] })
	} else if sp.Offset > 0 {
		if sp.Offset >= len(posts) {
			posts = nil
		} else {
			posts = posts[sp.Offset:]
		}
	}
	if len(posts) > sp.PerPage {
		posts = posts[:sp.PerPage]
	}
	out := make([]*wbSlotPost, 0, len(posts))
	ids := make([]int64, 0, len(posts))
	for _, p := range posts {
		out = append(out, &wbSlotPost{p: p})
		ids = append(ids, p.ID)
	}
	if len(ids) > 0 && e.prog.Uses.Has(webbuilder.SlotPostTerms) {
		if m, err := e.s.wbTermsForPosts(e.ctx, ids); err == nil {
			for _, item := range out {
				item.terms = m[item.p.ID]
				if item.terms == nil {
					item.terms = map[string][]*store.Term{}
				}
			}
		}
	}
	return out, true
}

// --- filling -------------------------------------------------------------------------------------

// wbFilled is a filled snapshot.
type wbFilled struct {
	html  string
	loops bool // it has a post loop (cache tag wb-loop)
	// shop: it renders store markup (Commerce widgets beyond the cart icon: store assets);
	// catalog: it shows catalog data (cache tag goo:shop).
	shop, catalog bool
}

// wbFill fills a published snapshot over the request's view, then applies
// wp_filter_content_tags to it (images: dimensions, srcset, loading). own: d is the main post's
// own builder doc.
func (s *Server) wbFill(c *gin.Context, v *siteView, d *store.WebBuilderDoc, own bool) wbFilled {
	ctx := v.ctx
	if ctx == nil {
		ctx = c.Request.Context()
	}
	// The stored snapshot, its html sections' scripts given the nonce placeholder when they get it
	// (an unfiltered_html author's, csp_trusted.go), compiled once per doc revision.
	trust := s.cspTrustPage(ctx)
	src := func() string { return wbTrustSource(d, trust) }
	var prog *webbuilder.Program
	var err error
	if wbDocKeyed(d) {
		prog, err = wbPrograms.CompileFor(wbProgramKey(d, trust), src)
	} else { // a doc that was never stored: keyed by its text
		prog, err = wbPrograms.Compile(d.PostID, d.Rev, src())
	}
	if err != nil {
		log.Printf("web builder: compile %d: %v", d.PostID, err)
		return wbFilled{html: s.wbFormEnhance(c, d, s.wbSearchForms(ctx, s.wbContentTags(ctx, src())))}
	}
	if static, ok := prog.Static(); ok {
		return wbFilled{html: s.wbFormEnhance(c, d, s.wbSearchForms(ctx, s.wbContentTags(ctx, static)))}
	}
	env := newWBSlotEnv(s, c, v, d, prog, own)
	shop := s.gooOn() && wbProgramShop(prog)
	if shop {
		c.Set(ctxWBShop, true) // before the canvas builds its head (gooIsStorePage)
	}
	out, err := prog.ExecString(env.root())
	if err != nil {
		log.Printf("web builder: fill %d: %v", d.PostID, err)
	}
	// Store widgets may take notices or drop unbuyable cart items: save the session now, before
	// the body is written (store views and shortcodes save their own; a builder page or template
	// takes neither path). No-op without a loaded, changed session.
	s.gooSaveSession(c)
	return wbFilled{html: s.wbFormEnhance(c, d, s.wbSearchForms(ctx, s.wbContentTags(ctx, out))), loops: env.loops, shop: shop, catalog: env.shop != nil && env.shop.catalog}
}
