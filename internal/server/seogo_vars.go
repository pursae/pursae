package server

import (
	"context"
	"html"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Replacement variables. Pursae SEO writes Rank Math's %var% syntax and accepts Yoast's %%var%% as
// well; names of both plugins are understood (%term% and %term_title%, %search_query% and
// %searchphrase%, %sitedesc% and %tagline%…).

// seogoCtx is what the variables and the head output are computed from: the main query of a
// request, or a post or term the editor previews.
type seogoCtx struct {
	s      *Server
	ctx    context.Context
	lb     *links.Builder
	Kind   string // single, page, front-page, home, category, tag, author, date, search, 404
	Post   *store.Post
	Term   *store.Term
	Author *store.User
	Search string
	Paged  int
	Page   int
	Pages  int
	Year   int
	Month  int
	Day    int
	Focus  string // the focus keyphrase while the editor previews unsaved values
	cats   []*store.Term

	PostType string // the post type of a post-type-archive view

	// Pursae Commerce views (siteQuery.Store): the store renders them as synthetic pages; these say
	// what they really are. Store is product, shop, product_cat, product_tag, product_attr, cart,
	// checkout or myaccount; StoreTax the archived taxonomy; StoreURL the unpaged canonical URL.
	Store    string
	StoreTax string
	StoreURL string

	// storeProduct is the Pursae Commerce Product node of a store product page; it joins the @graph.
	storeProduct map[string]any
}

func (s *Server) seogoCtxFor(ctx context.Context, q *siteQuery) *seogoCtx {
	sc := &seogoCtx{s: s, ctx: ctx, lb: s.links(ctx), Kind: q.Kind, Post: q.Post, Term: q.Term, Author: q.Author,
		Search: q.Search, Paged: q.Paged, Page: q.Page, Year: q.Year, Month: q.Month, Day: q.Day,
		PostType: q.PostType, Store: q.Store, StoreTax: q.StoreTax, StoreURL: q.StoreURL}
	if q.Kind == "home" && q.PostsPage != nil {
		sc.Post = q.PostsPage
	}
	if q.Store != "" {
		s.seogoStoreCtx(sc, q)
	}
	if sc.Post != nil {
		sc.Pages = len(splitPages(sc.Post.Content))
	}
	return sc
}

// seogoStoreCtx gives a Pursae Commerce view the object behind its synthetic page: a product's own
// meta, featured image, description and excerpt; a product term archive its term; the shop its page.
func (s *Server) seogoStoreCtx(sc *seogoCtx, q *siteQuery) {
	ctx := gooPllUnfiltered(sc.ctx) // the object is asked for by id, whatever its language
	switch {
	case q.StoreTerm > 0:
		if t, err := s.store.GetTerm(ctx, q.StoreTerm); err == nil {
			sc.Kind, sc.Term, sc.Post = "taxonomy", t, nil
		}
	case (q.Store == "product" || q.Store == "shop") && q.Post != nil && q.Post.ID > 0:
		real, err := s.store.GetPost(ctx, q.Post.ID)
		if err != nil {
			return
		}
		cp := *q.Post
		meta := map[string]any{}
		for k, v := range real.Meta {
			meta[k] = v
		}
		for k, v := range q.Post.Meta {
			meta[k] = v
		}
		cp.Meta, cp.FeaturedMediaID = meta, real.FeaturedMediaID
		if q.Store == "product" {
			cp.Type, cp.Content = "product", real.Content
			if cp.Excerpt == "" {
				cp.Excerpt = real.Excerpt
			}
		}
		sc.Post = &cp
	}
}

// storeArchive reports whether the context is the shop or a product taxonomy archive.
func (sc *seogoCtx) storeArchive() bool {
	return sc.Store == "shop" || sc.Store == "product_cat" || sc.Store == "product_tag" || sc.Store == "product_attr"
}

// storePrivate reports whether the context is the Cart, Checkout or My account page (never indexed,
// like WooCommerce's wc_page_noindex).
func (sc *seogoCtx) storePrivate() bool {
	return sc.Store == "cart" || sc.Store == "checkout" || sc.Store == "myaccount"
}

// singular reports whether the context describes one post or page.
func (sc *seogoCtx) singular() bool {
	return sc.Post != nil && (sc.Kind == "single" || sc.Kind == "page" || sc.Kind == "front-page" || (sc.Kind == "home" && sc.Post != nil))
}

// typeKey is the Search Appearance entry that applies.
func (sc *seogoCtx) typeKey() string {
	switch sc.Kind {
	case "single", "page":
		if sc.Post != nil {
			return sc.Post.Type
		}
	case "category":
		return "category"
	case "tag":
		return "post_tag"
	case "taxonomy":
		if sc.Term != nil {
			return sc.Term.Taxonomy
		}
		return sc.StoreTax
	case "post-type-archive":
		return "pta_" + sc.PostType
	case "front-page", "home":
		if sc.Kind == "home" && sc.Post != nil {
			return "page"
		}
		return "home"
	}
	return sc.Kind
}

func (sc *seogoCtx) categories() []*store.Term {
	if sc.cats == nil && sc.Post != nil && sc.Post.Type == "post" && sc.Post.ID > 0 {
		m, _ := sc.s.store.TermsForPosts(sc.ctx, []int64{sc.Post.ID}, "category")
		sc.cats = m[sc.Post.ID]
		if sc.cats == nil {
			sc.cats = []*store.Term{}
		}
	}
	return sc.cats
}

// primaryCategory is the post's primary category (Yoast/Rank Math meta) or its first one.
func (sc *seogoCtx) primaryCategory() *store.Term {
	cats := sc.categories()
	if len(cats) == 0 {
		return nil
	}
	if id, _ := strconv.ParseInt(seogoMeta(sc.Post.Meta, "primary_category"), 10, 64); id > 0 {
		for _, t := range cats {
			if t.ID == id {
				return t
			}
		}
	}
	return cats[0]
}

func (sc *seogoCtx) author() *store.User {
	if sc.Author != nil {
		return sc.Author
	}
	if sc.Post != nil && sc.Post.AuthorID > 0 {
		if u, err := sc.s.store.UserByID(sc.ctx, sc.Post.AuthorID); err == nil {
			sc.Author = u
		}
	}
	return sc.Author
}

// plainText is the text of HTML content: blocks, tags and shortcodes removed, whitespace collapsed.
func plainText(content string) string {
	t := wp.StripTags(stripBlocks(content))
	t = shortcodeRe.ReplaceAllString(t, "")
	return strings.Join(strings.Fields(html.UnescapeString(t)), " ")
}

var shortcodeRe = regexp.MustCompile(`\[/?[a-zA-Z0-9_-]+[^\]]*\]`)

// cut shortens text to at most n characters on a word boundary.
func cut(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	r := []rune(text)[:n]
	s := string(r)
	if i := strings.LastIndex(s, " "); i > n/2 {
		s = s[:i]
	}
	return strings.TrimRight(s, " ,.;:-") + "…"
}

func (sc *seogoCtx) excerpt(only bool) string {
	if sc.Post == nil {
		return ""
	}
	if sc.Post.Excerpt != "" {
		return plainText(sc.Post.Excerpt)
	}
	if only || sc.Post.Password != "" {
		return ""
	}
	return cut(plainText(sc.Post.Content), 156)
}

func (sc *seogoCtx) pageVar() string {
	n, total := sc.Paged, 0
	if sc.Post != nil && sc.Page > 1 {
		n, total = sc.Page, sc.Pages
	}
	if n <= 1 {
		return ""
	}
	sep := sc.s.seogoOpts().Separator
	if total > 1 {
		return sep + " Page " + strconv.Itoa(n) + " of " + strconv.Itoa(total)
	}
	return sep + " Page " + strconv.Itoa(n)
}

func (sc *seogoCtx) archiveDate() string {
	switch {
	case sc.Day != 0:
		return wp.FormatDate(time.Date(sc.Year, time.Month(sc.Month), sc.Day, 0, 0, 0, 0, time.UTC), sc.s.settings.String("date_format"))
	case sc.Month != 0:
		return monthName(sc.Month) + " " + strconv.Itoa(sc.Year)
	case sc.Year != 0:
		return strconv.Itoa(sc.Year)
	}
	return ""
}

// variable returns the value of one replacement variable and whether it is known.
func (sc *seogoCtx) variable(name string) (string, bool) {
	s := sc.s
	now := time.Now().In(s.loc())
	switch name {
	case "sep":
		return s.seogoOpts().Separator, true
	case "sitename":
		return s.seogoSiteName(sc.ctx), true
	case "sitedesc", "tagline":
		return s.seogoSiteDesc(sc.ctx), true
	case "title", "seo_title":
		switch {
		case sc.Post != nil:
			return sc.Post.Title, true
		case sc.Term != nil:
			return sc.Term.Name, true
		case sc.Kind == "author" && sc.author() != nil:
			return sc.author().DisplayName, true
		case sc.Kind == "post-type-archive" && sc.PostType != "":
			return seogoTypeLabels(sc.PostType).Name, true
		}
		return "", true
	case "parent_title":
		if sc.Post != nil && sc.Post.ParentID > 0 {
			if p, err := s.store.GetPost(sc.ctx, sc.Post.ParentID); err == nil && p.Status == "publish" {
				return p.Title, true // a private or draft parent's title is not public
			}
		}
		return "", true
	case "excerpt":
		return sc.excerpt(false), true
	case "excerpt_only":
		return sc.excerpt(true), true
	case "seo_description":
		if sc.Post != nil {
			return seogoMeta(sc.Post.Meta, "description"), true
		}
		return "", true
	case "category", "categories":
		var names []string
		for _, t := range sc.categories() {
			names = append(names, t.Name)
		}
		if sc.Term != nil && sc.Post == nil {
			return sc.Term.Name, true
		}
		return strings.Join(names, ", "), true
	case "primary_category":
		if t := sc.primaryCategory(); t != nil {
			return t.Name, true
		}
		return "", true
	case "tag", "tags":
		if sc.Post != nil && sc.Post.ID > 0 {
			m, _ := s.store.TermsForPosts(sc.ctx, []int64{sc.Post.ID}, "post_tag")
			var names []string
			for _, t := range m[sc.Post.ID] {
				names = append(names, t.Name)
			}
			return strings.Join(names, ", "), true
		}
		if sc.Term != nil {
			return sc.Term.Name, true
		}
		return "", true
	case "term", "term_title", "category_title":
		if sc.Term != nil {
			return sc.Term.Name, true
		}
		return "", true
	case "term_description", "category_description", "tag_description":
		if sc.Term != nil {
			return plainText(sc.Term.Description), true
		}
		return "", true
	case "name", "author", "post_author":
		if a := sc.author(); a != nil {
			return a.DisplayName, true
		}
		return "", true
	case "user_description":
		if a := sc.author(); a != nil {
			return a.Profile["description"], true
		}
		return "", true
	case "date":
		if sc.Kind == "date" {
			return sc.archiveDate(), true
		}
		if sc.Post != nil {
			return wp.FormatDate(sc.Post.Date().In(s.loc()), s.settings.String("date_format")), true
		}
		return "", true
	case "modified":
		if sc.Post != nil {
			return wp.FormatDate(sc.Post.ModifiedAt.In(s.loc()), s.settings.String("date_format")), true
		}
		return "", true
	case "currentdate":
		return wp.FormatDate(now, s.settings.String("date_format")), true
	case "currentyear":
		return strconv.Itoa(now.Year()), true
	case "currentmonth":
		return now.Month().String(), true
	case "currentday":
		return strconv.Itoa(now.Day()), true
	case "currenttime":
		return wp.FormatDate(now, s.settings.String("time_format")), true
	case "page":
		return sc.pageVar(), true
	case "pagenumber":
		n := sc.Paged
		if sc.Page > 1 {
			n = sc.Page
		}
		if n < 1 {
			n = 1
		}
		return strconv.Itoa(n), true
	case "pagetotal":
		if sc.Pages > 0 {
			return strconv.Itoa(sc.Pages), true
		}
		return "1", true
	case "searchphrase", "search_query":
		return sc.Search, true
	case "id":
		if sc.Post != nil {
			return strconv.FormatInt(sc.Post.ID, 10), true
		}
		return "", true
	case "focuskw", "keywords":
		if sc.Focus != "" {
			return seogoFocusList(sc.Focus)[0], true
		}
		if sc.Post != nil {
			if l := seogoFocusList(seogoMeta(sc.Post.Meta, "focus")); len(l) > 0 {
				return l[0], true
			}
		}
		return "", true
	case "pt_single", "pt_plural":
		typ := sc.PostType
		if sc.Post != nil {
			typ = sc.Post.Type
		}
		if typ != "" {
			l := seogoTypeLabels(typ)
			if name == "pt_single" {
				return l.Singular, true
			}
			return l.Name, true
		}
		return "", true
	case "url", "permalink":
		if sc.Store != "" && sc.StoreURL != "" {
			return sc.StoreURL, true
		}
		if sc.Post != nil && sc.Post.Status != "auto-draft" {
			return sc.lb.Permalink(sc.Post), true
		}
		return sc.homeURL(), true
	case "org_name":
		return s.seogoOrgName(sc.ctx), true
	case "filename":
		return "", true
	}
	return "", false
}

var seogoVarRe = regexp.MustCompile(`%([a-zA-Z_]+)%`)

// replace fills the variables of a template and tidies the result like both plugins do: no
// doubled spaces and no separator left dangling at either end or twice in a row.
func (sc *seogoCtx) replace(tpl string) string {
	if tpl == "" {
		return ""
	}
	tpl = yoastVars(tpl)
	out := seogoVarRe.ReplaceAllStringFunc(tpl, func(m string) string {
		if v, ok := sc.variable(strings.ToLower(m[1 : len(m)-1])); ok {
			return v
		}
		return m
	})
	return tidySep(out, sc.s.seogoOpts().Separator)
}

func tidySep(v, sep string) string {
	words := strings.Fields(v)
	var out []string
	for _, w := range words {
		if w == sep && (len(out) == 0 || out[len(out)-1] == sep) {
			continue
		}
		out = append(out, w)
	}
	for len(out) > 0 && out[len(out)-1] == sep {
		out = out[:len(out)-1]
	}
	return strings.Join(out, " ")
}

// imageFileVars fills %filename% (and the title variables) for Image SEO.
func (sc *seogoCtx) imageText(tpl, src string) string {
	name := path.Base(src)
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(name))
	name = scaledSuffixRe.ReplaceAllString(name, "")
	return strings.TrimSpace(strings.ReplaceAll(sc.replace(strings.ReplaceAll(yoastVars(tpl), "%filename%", "\x00")), "\x00", name))
}

var scaledSuffixRe = regexp.MustCompile(`(?i)\s+(\d+x\d+|scaled)$`)

// seogoSiteName is the site title in the request language (Pursae Multi Language strings).
func (s *Server) seogoSiteName(ctx context.Context) string {
	return s.seogoTr(ctx, s.settings.String("blogname"))
}

// seogoSiteDesc is the tagline in the request language.
func (s *Server) seogoSiteDesc(ctx context.Context) string {
	return s.seogoTr(ctx, s.settings.String("blogdescription"))
}

// seogoTr translates a site-wide text (the site title, a Search Appearance template) into the
// request language; the text itself outside a language.
func (s *Server) seogoTr(ctx context.Context, str string) string {
	if str == "" {
		return str
	}
	if l := s.pllCurrent(ctx); l != nil {
		return s.pllTranslate(ctx, str, l.Slug)
	}
	return str
}

// seogoTypeLabels are the labels of a post type (products included).
func seogoTypeLabels(typ string) typeLabels {
	if typ == "product" {
		return typeLabels{Type: "product", Name: "Products", Singular: "Product"}
	}
	return labelsFor(typ)
}

// seogoPllStrings are Pursae SEO's site-wide texts listed in Languages › Translations (group
// Pursae SEO): the Search Appearance title and description templates and the breadcrumb labels.
func (s *Server) seogoPllStrings() []pllRegistered {
	if !s.seogoOn() {
		return nil
	}
	o := s.seogoOpts()
	var out []pllRegistered
	add := func(name, str string) {
		if strings.TrimSpace(str) != "" {
			out = append(out, pllRegistered{Name: name, String: str, Group: "Pursae SEO"})
		}
	}
	keys := make([]string, 0, len(o.Types))
	for k := range o.Types {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add("Title: "+k, o.Types[k].Title)
		add("Description: "+k, o.Types[k].Description)
	}
	add("Breadcrumbs: home", o.BCHome)
	add("Breadcrumbs: prefix", o.BCPrefix)
	add("Breadcrumbs: archive prefix", o.BCArchive)
	add("Breadcrumbs: search prefix", o.BCSearch)
	add("Breadcrumbs: 404", o.BC404)
	return out
}
