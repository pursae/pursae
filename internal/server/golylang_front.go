package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/blocks"
	"wpgo/internal/links"
	"wpgo/internal/store"
)

// Golylang's front end: translated site title and tagline, the menus of the current language, the
// language switcher (menu item, block, pll_the_languages), hreflang links, <html lang> and the pll_*
// template functions.

// pllAlternate is one hreflang link.
type pllAlternate struct {
	Lang *pllLanguage
	URL  string
}

// golylangView adapts the site view to the current language.
func (s *Server) golylangView(c *gin.Context, q *siteQuery, v *siteView, lb *links.Builder) {
	r := pllFrom(c)
	if r == nil || !r.Front || r.Lang == nil || !s.golylangOn() {
		return
	}
	ctx := c.Request.Context()
	l := r.Lang
	v.SiteName = s.pllTranslate(ctx, s.settings.String("blogname"), l.Slug)
	v.Description = s.pllTranslate(ctx, s.settings.String("blogdescription"), l.Slug)
	v.HomeURL = lb.Front()
	// Menus: the default language uses the core locations, the others their own (Manage Locations).
	if !l.IsDefault {
		o := s.pllOpts()
		for _, loc := range v.Theme.MenuLocations {
			delete(v.Menus, loc.Slug)
			if id := o.NavMenus[v.Theme.Slug][loc.Slug][l.Slug]; id > 0 {
				if items := s.pllMenuItems(ctx, id); len(items) > 0 {
					items = s.resolveMenu(ctx, items, lb, q)
					v.Menus[loc.Slug] = items
				}
			}
		}
	}
	for loc, items := range v.Menus {
		v.Menus[loc] = s.pllExpandSwitcher(c, q, items)
	}
	r.alternates = s.pllAlternates(c, q)
}

// pllMenuItems loads a menu by id, nested (store.MenuAt for a menu instead of a location).
func (s *Server) pllMenuItems(ctx context.Context, menuID int64) []*store.MenuItem {
	rows, err := s.store.Pool.Query(ctx, `SELECT id, coalesce(parent_id,0), kind, object, coalesce(target_id,0), url, label, target, css_classes
		FROM menu_items WHERE menu_id = $1 AND status = 'publish' ORDER BY sort_order, id`, menuID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var flat []*store.MenuItem
	for rows.Next() {
		it := &store.MenuItem{}
		if rows.Scan(&it.ID, &it.ParentID, &it.Kind, &it.Object, &it.TargetID, &it.URL, &it.Label, &it.Target, &it.Classes) == nil {
			flat = append(flat, it)
		}
	}
	byID := map[int64]*store.MenuItem{}
	for _, it := range flat {
		byID[it.ID] = it
	}
	var roots []*store.MenuItem
	for _, it := range flat {
		if p, ok := byID[it.ParentID]; ok {
			p.Children = append(p.Children, it)
		} else {
			roots = append(roots, it)
		}
	}
	return roots
}

// pllSwitcherArgs are pll_the_languages() arguments (the legacy names Polylang still accepts).
type pllSwitcherArgs struct {
	Dropdown            bool
	ShowNames           bool
	ShowFlags           bool
	DisplayNamesAs      string // name or slug
	ForceHome           bool
	HideCurrent         bool
	HideIfNoTranslation bool
	HideIfEmpty         bool
	PostID              int64
	Raw                 bool
}

func pllDefaultArgs() pllSwitcherArgs {
	return pllSwitcherArgs{ShowNames: true, DisplayNamesAs: "name", HideIfEmpty: true}
}

// pllSwitcherItem is one language of the switcher (the raw output of pll_the_languages).
type pllSwitcherItem struct {
	Lang           *pllLanguage
	URL            string
	Current        bool
	NoTranslation  bool
	First          bool
	Classes        string
	Label          string
	HasTranslation bool
}

// pllSwitcherItems builds the language list with the URL of the current page in each language.
func (s *Server) pllSwitcherItems(c *gin.Context, q *siteQuery, a pllSwitcherArgs) []pllSwitcherItem {
	ctx := c.Request.Context()
	cur := s.pllCurrent(ctx)
	var counts map[string]int
	if a.HideIfEmpty {
		counts = s.pllPublishedCounts(ctx)
	}
	var out []pllSwitcherItem
	first := true
	for _, l := range s.pllActiveLanguages(ctx) {
		if a.HideIfEmpty && counts[l.Slug] == 0 {
			continue
		}
		isCur := cur != nil && cur.Slug == l.Slug
		if isCur && a.HideCurrent {
			continue
		}
		u, has := "", false
		if a.PostID > 0 {
			if t := s.pllGet(ctx, pllPost, a.PostID, l.Slug); t > 0 {
				if p, err := s.store.GetPost(ctx, t); err == nil && p.Status == "publish" {
					u, has = s.links(ctx).Permalink(p), true
				}
			}
		} else if !a.ForceHome {
			u, has = s.pllTranslationURL(c, q, l)
		}
		if u == "" {
			if a.HideIfNoTranslation && !a.ForceHome {
				continue
			}
			u = s.pllHomeURL(ctx, l)
		}
		it := pllSwitcherItem{Lang: l, URL: u, Current: isCur, NoTranslation: !has && !a.ForceHome, First: first, HasTranslation: has}
		it.Label = l.Name
		if a.DisplayNamesAs == "slug" {
			it.Label = l.Slug
		}
		// Language names are no page text: known (not recorded, not listed in visual translation).
		pllMarkKnown(c, it.Label, "")
		if f := l.FlagEmoji(); f != "" {
			pllMarkKnown(c, f+" "+it.Label, "")
		}
		cls := []string{"lang-item", "lang-item-" + itoa64(l.ID), "lang-item-" + l.Slug}
		if isCur {
			cls = append(cls, "current-lang")
		}
		if it.NoTranslation {
			cls = append(cls, "no-translation")
		}
		if first {
			cls = append(cls, "lang-item-first")
		}
		it.Classes = strings.Join(cls, " ")
		first = false
		out = append(out, it)
	}
	return out
}

// pllPublishedCounts counts published posts and pages per language (hide_if_empty).
func (s *Server) pllPublishedCounts(ctx context.Context) map[string]int {
	out := map[string]int{}
	rows, err := s.store.Pool.Query(ctx, `SELECT coalesce(l.lang, $1), count(*) FROM posts p LEFT JOIN pll_post_langs l ON l.post_id = p.id
		WHERE p.type = ANY($2) AND p.status = 'publish' GROUP BY 1`, defaultSlug(s.pllDefault(ctx)), s.pllPostTypes())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var lang string
		var n int
		if rows.Scan(&lang, &n) == nil {
			out[lang] = n
		}
	}
	return out
}

// pllTranslationURL is the URL of the queried page in another language; has is false when the post or
// term has no published translation there (the switcher then links the language home).
func (s *Server) pllTranslationURL(c *gin.Context, q *siteQuery, l *pllLanguage) (string, bool) {
	ctx := c.Request.Context()
	lb := s.links(ctx)
	if q == nil {
		return "", false
	}
	if u, has, ok := s.gooPllTranslationURL(c, q, l); ok { // Goocommerce store views (goo_golylang.go)
		return u, has
	}
	if u, has, ok := s.forumPllTranslationURL(c, q, l); ok { // Pursae Forum views: the same forum path in l (forum_urls.go)
		return u, has
	}
	switch {
	case q.Kind == "front-page" || (q.Kind == "home" && q.PostsPage == nil):
		return s.pllHomeURL(ctx, l), true
	case q.Post != nil && s.pllTranslatedType(q.Post.Type):
		if t := s.pllGet(ctx, pllPost, q.Post.ID, l.Slug); t > 0 {
			if p, err := s.store.GetPost(ctx, t); err == nil && p.Status == "publish" {
				return lb.Permalink(p), true
			}
		}
		return "", false
	case q.PostsPage != nil:
		if t := s.pllGet(ctx, pllPost, q.PostsPage.ID, l.Slug); t > 0 {
			if p, err := s.store.GetPost(ctx, t); err == nil && p.Status == "publish" {
				return lb.Permalink(p), true
			}
		}
		return "", false
	case q.Term != nil && s.pllTranslatedTaxonomy(q.Term.Taxonomy):
		if t := s.pllGet(ctx, pllTerm, q.Term.ID, l.Slug); t > 0 {
			if tm, err := s.store.GetTerm(ctx, t); err == nil {
				return lb.TermLink(tm), true
			}
		}
		return "", false
	case q.Kind == "404":
		return "", false
	}
	// Archives (date, author, search) exist in every language: the same URL in l.
	o := s.pllOpts()
	cur := lb.Home + c.Request.URL.RequestURI()
	if r := pllFrom(c); r != nil && r.Lang != nil && o.TranslateSlugs {
		// The request path carries the current language's base (/fr/auteur/bob): turn it back into
		// the base itself first, so pllConvert can translate it into l (/de/autor/bob).
		if rel, ok := s.pllRelative(ctx, lb.Home, cur, o); ok {
			cur = lb.Home + s.pllTranslateBase(ctx, rel, r.Lang.Slug, false)
		}
	}
	return s.pllConvert(ctx, lb.Home, cur, l, "home", o), true
}

// pllExpandSwitcher replaces "#pll_switcher" custom links (the Language switcher menu item) with one item
// per language. The item's CSS classes choose the options: pll-show-flags, pll-hide-names, pll-hide-current,
// pll-force-home and pll-hide-if-no-translation.
func (s *Server) pllExpandSwitcher(c *gin.Context, q *siteQuery, items []*store.MenuItem) []*store.MenuItem {
	var out []*store.MenuItem
	for _, it := range items {
		if it.Kind == "custom" && strings.HasPrefix(it.URL, "#pll_switcher") {
			a := pllDefaultArgs()
			a.HideIfEmpty = false
			cls := " " + it.Classes + " "
			a.ShowFlags = strings.Contains(cls, " pll-show-flags ")
			a.ShowNames = !strings.Contains(cls, " pll-hide-names ")
			a.HideCurrent = strings.Contains(cls, " pll-hide-current ")
			a.ForceHome = strings.Contains(cls, " pll-force-home ")
			a.HideIfNoTranslation = strings.Contains(cls, " pll-hide-if-no-translation ")
			for _, li := range s.pllSwitcherItems(c, q, a) {
				label := li.Label
				if a.ShowFlags && li.Lang.FlagEmoji() != "" {
					if a.ShowNames {
						label = li.Lang.FlagEmoji() + " " + label
					} else {
						label = li.Lang.FlagEmoji()
					}
				}
				// One item per language, each with its own id (Polylang: "<item id>-<slug>").
				out = append(out, &store.MenuItem{ID: it.ID, Key: itoa64(it.ID) + "-" + li.Lang.Slug, ParentID: it.ParentID, Kind: "custom", Object: "custom", URL: li.URL, Label: label,
					Classes: strings.TrimSpace(li.Classes + " " + strings.TrimSpace(strings.NewReplacer("pll-show-flags", "", "pll-hide-names", "", "pll-hide-current", "", "pll-force-home", "", "pll-hide-if-no-translation", "").Replace(it.Classes)))})
			}
			continue
		}
		if len(it.Children) > 0 {
			it.Children = s.pllExpandSwitcher(c, q, it.Children)
		}
		out = append(out, it)
	}
	return out
}

// pllSwitcherHTML is pll_the_languages(): a list of <li> items or a dropdown.
func (s *Server) pllSwitcherHTML(c *gin.Context, q *siteQuery, a pllSwitcherArgs, id string) string {
	items := s.pllSwitcherItems(c, q, a)
	var b strings.Builder
	if a.Dropdown {
		fmt.Fprintf(&b, `<label class="screen-reader-text" for="%s">Choose a language</label><select name="%s" id="%s" class="pll-switcher-select">`, id, id, id)
		for _, it := range items {
			label := it.Label
			if a.ShowFlags && it.Lang.FlagEmoji() != "" {
				label = it.Lang.FlagEmoji() + " " + label
			}
			fmt.Fprintf(&b, `<option value="%s" lang="%s"%s>%s</option>`, html.EscapeString(it.URL), html.EscapeString(it.Lang.W3C()),
				map[bool]string{true: ` selected="selected"`}[it.Current], html.EscapeString(label))
		}
		fmt.Fprintf(&b, `</select><script%s>document.getElementById(%s).addEventListener('change',function(){location.href=this.value;});</script>`, cspNonceAttr(), strconvQuote(id))
		return b.String()
	}
	for _, it := range items {
		fmt.Fprintf(&b, `<li class="%s"><a lang="%s" hreflang="%s" href="%s"%s>`, it.Classes, html.EscapeString(it.Lang.W3C()), html.EscapeString(it.Lang.W3C()),
			html.EscapeString(it.URL), map[bool]string{true: ` aria-current="true"`}[it.Current])
		if a.ShowFlags && it.Lang.Flag != "" {
			b.WriteString(string(it.Lang.FlagHTML()))
		}
		if a.ShowNames || !a.ShowFlags {
			if a.ShowFlags && it.Lang.Flag != "" {
				b.WriteString(`<span style="margin-left:0.3em;">` + html.EscapeString(it.Label) + `</span>`)
			} else {
				b.WriteString(`<span>` + html.EscapeString(it.Label) + `</span>`)
			}
		}
		b.WriteString("</a></li>\n")
	}
	return b.String()
}

// pllAlternates are the hreflang links of the page: every language the page exists in. None on paged
// views, or when the page has a single language.
func (s *Server) pllAlternates(c *gin.Context, q *siteQuery) []pllAlternate {
	if q == nil || q.Kind == "404" || q.Kind == "search" || q.Paged > 1 || q.Page > 1 || q.Preview {
		return nil
	}
	var out []pllAlternate
	for _, l := range s.pllActiveLanguages(c) {
		if u, ok := s.pllTranslationURL(c, q, l); ok && u != "" {
			out = append(out, pllAlternate{l, u})
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

var pllHTMLTag = regexp.MustCompile(`(?s)<html\b[^>]*>`)
var pllLangAttr = regexp.MustCompile(`\slang="[^"]*"`)

// golylangHTML finishes a front-end page: <html lang> (and dir) of the current language, and the
// hreflang links before </head>.
func (s *Server) golylangHTML(c *gin.Context, page []byte) []byte {
	// Every return below goes through pllUCFinish: no user content sentinel (golylang_uc.go) is
	// ever written; the string pass must run before it. Sentinels are only emitted under the
	// condition of this first return (pllUCOn), so a page without the plugin stays as it is; the
	// first return still strips them (one bytes.Contains) in case the plugin was turned off while
	// this page rendered.
	r := pllFrom(c)
	if r == nil || !r.Front || r.Lang == nil || !s.golylangOn() {
		return pllUCFinish(page, false)
	}
	l := r.Lang
	loc := pllHTMLTag.FindIndex(page)
	if loc == nil {
		return s.pllFinishPage(c, page, l)
	}
	tag := string(page[loc[0]:loc[1]])
	attr := ` lang="` + html.EscapeString(l.W3C()) + `"`
	if l.RTL {
		attr += ` dir="rtl"`
	}
	if pllLangAttr.MatchString(tag) {
		tag = pllLangAttr.ReplaceAllString(tag, attr)
	} else {
		tag = strings.Replace(tag, "<html", "<html"+attr, 1)
	}
	var out bytes.Buffer
	out.Write(page[:loc[0]])
	out.WriteString(tag)
	rest := page[loc[1]:]
	if len(r.alternates) > 0 {
		var hl strings.Builder
		for _, a := range r.alternates {
			fmt.Fprintf(&hl, "<link rel=\"alternate\" href=\"%s\" hreflang=\"%s\" />\n", html.EscapeString(a.URL), html.EscapeString(s.pllHreflang(c, a.Lang)))
		}
		o := s.pllOpts()
		if q := siteQueryFrom(c); q != nil && (q.Kind == "front-page" || (q.Kind == "home" && q.PostsPage == nil)) && !o.HideDefault && o.ForceLang < 3 {
			fmt.Fprintf(&hl, "<link rel=\"alternate\" href=\"%s\" hreflang=\"x-default\" />\n", html.EscapeString(s.links(c).Home+"/"))
		}
		if i := bytes.Index(rest, []byte("</head>")); i >= 0 {
			out.Write(rest[:i])
			out.WriteString(hl.String())
			rest = rest[i:]
		}
	}
	out.Write(rest)
	return s.pllFinishPage(c, out.Bytes(), l)
}

// pllFinishPage is the end of golylangHTML: visual translation mode takes the page over; otherwise
// the string pass (golylang_apply.go) and the removal of the user content sentinels.
func (s *Server) pllFinishPage(c *gin.Context, page []byte, l *pllLanguage) []byte {
	if pllVisualPage != nil {
		if p, ok := pllVisualPage(s, c, page, l); ok {
			return p
		}
	}
	return pllUCFinish(s.pllApplyStrings(c, page, l, nil), false)
}

// pllHreflang drops the region of a locale unless two languages share the primary subtag (fr, en-GB/en-US).
func (s *Server) pllHreflang(ctx context.Context, l *pllLanguage) string {
	primary := strings.SplitN(l.Locale, "_", 2)[0]
	n := 0
	for _, o := range s.pllActiveLanguages(ctx) {
		if strings.SplitN(o.Locale, "_", 2)[0] == primary {
			n++
		}
	}
	if n > 1 {
		return l.W3C()
	}
	return primary
}

// golylangHomeURL is pll_home_url() of the front-end request language: the home page of /tr/ is
// http://site/tr/, not the site root. ok is false outside a language (plugin off, no languages).
func (s *Server) golylangHomeURL(ctx context.Context) (string, bool) {
	r := pllFrom(ctx)
	if r == nil || !r.Front || r.Lang == nil || !s.golylangOn() {
		return "", false
	}
	return s.pllHomeURL(ctx, r.Lang), true
}

// golylangURL moves a URL of the site home to the current request language (sitemap index entries).
func (s *Server) golylangURL(c *gin.Context, link string) string {
	l := s.pllCurrent(c)
	if l == nil {
		return link
	}
	root := s.links(c).Home
	return s.pllConvert(c, root, link, l, "home", s.pllOpts())
}

// Strings translations -------------------------------------------------------------------------

// pllRegistered is one translatable string of Languages > Translations.
type pllRegistered struct {
	Name, String, Group string
	Multiline           bool
	// Context is the gettext context (_x): the string is translated under Key(). Source is the
	// pll_string_sources source of a recorded string ("" for the built-in ones).
	Context, Source string
}

// Key is the pll_strings.original the string is translated under.
func (r pllRegistered) Key() string {
	if r.Context == "" {
		return r.String
	}
	return r.Context + "\x04" + r.String
}

var (
	pllStringsMu    sync.Mutex
	pllStringsCache = map[*Server]map[string]map[string]string{} // server => lang => original => translation
)

// pllStrings and pllRegisterString: golylang_strtable.go.

// pllTranslate is pll_translate_string(): the translation of a string in a language, the string itself
// when it has none.
func (s *Server) pllTranslate(ctx context.Context, str, lang string) string {
	if str == "" || lang == "" {
		return str
	}
	if t := s.pllStringMap(ctx, lang)[str]; t != "" {
		pllMarkKnown(ctx, t, str) // visual translation mode: the page shows t, the string is str
		return t
	}
	return str
}

func (s *Server) pllStringMap(ctx context.Context, lang string) map[string]string {
	m, _ := s.pllStringMapOK(ctx, lang)
	return m
}

// pllStringsGen counts pllFlushStrings calls (guarded by pllStringsMu): a table built from the
// strings must not be stored once a flush happened while it was built.
var pllStringsGen uint64

// pllStringMapOK is pllStringMap; ok is false when the strings could not be loaded (the result is
// then empty and nothing is cached, so the next call loads again). The load does not follow the
// request's cancellation: a visitor closing the tab must not leave every language untranslated.
func (s *Server) pllStringMapOK(ctx context.Context, lang string) (map[string]string, bool) {
	pllStringsMu.Lock()
	defer pllStringsMu.Unlock()
	if byLang := pllStringsCache[s]; byLang != nil {
		return byLang[lang], true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	byLang := map[string]map[string]string{}
	rows, err := s.store.Pool.Query(lctx, `SELECT lang, original, translation FROM pll_strings`)
	if err != nil {
		slog.Warn("golylang: loading string translations", "err", err)
		return nil, false
	}
	for rows.Next() {
		var l, o, t string
		if err := rows.Scan(&l, &o, &t); err != nil {
			rows.Close()
			slog.Warn("golylang: loading string translations", "err", err)
			return nil, false
		}
		if byLang[l] == nil {
			byLang[l] = map[string]string{}
		}
		byLang[l][o] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Warn("golylang: loading string translations", "err", err)
		return nil, false
	}
	pllStringsCache[s] = byLang
	return byLang[lang], true
}

func (s *Server) pllFlushStrings() {
	pllStringsMu.Lock()
	delete(pllStringsCache, s)
	delete(pllFrontTables, s) // the page string pass tables (golylang_apply.go)
	pllStringsGen++
	pllStringsMu.Unlock()
}

// pllSlugBases are the URL bases Translate slugs translates: name and base.
func (s *Server) pllSlugBases() [][2]string {
	out := append([][2]string{
		{"category", strings.Trim(defaultStr(s.settings.String("category_base"), "category"), "/")},
		{"post_tag", strings.Trim(defaultStr(s.settings.String("tag_base"), "tag"), "/")},
		{"author", "author"},
		{"search", "search"},
	}, s.gooPllSlugBases()...) // Goocommerce: product, product category and tag bases (goo_golylang.go)
	return append(out, s.pllCPTSlugBases()...)
}

// pllCPTSlugBases are the rewrite and archive slugs of the Pursae Custom Fields post types ticked
// in Languages > Settings (Custom post types), such as /portfolio/. The first URL segment is what
// gets translated, so a slug with a slash is left out.
func (s *Server) pllCPTSlugBases() [][2]string { return pllCPTBasesFor(s.pllOpts().PostTypes) }

func pllCPTBasesFor(types []string) [][2]string {
	var out [][2]string
	for _, typ := range types {
		pt := siteCPT(typ)
		if pt == nil {
			continue
		}
		base, archive := siteCPTSlugs(pt)
		if base != "" && !strings.Contains(base, "/") {
			out = append(out, [2]string{typ, base})
		}
		if archive != "" && archive != base && !strings.Contains(archive, "/") {
			out = append(out, [2]string{typ + "_archive", archive})
		}
	}
	return out
}

// pllTranslateBase swaps the first path segment of rel between a base and its translation in lang.
// toLang translates (links); otherwise the translated base is turned back into the base (requests).
func (s *Server) pllTranslateBase(ctx context.Context, rel, lang string, toLang bool) string {
	if !s.pllOpts().TranslateSlugs {
		return rel
	}
	trimmed := strings.TrimPrefix(rel, "/")
	seg, rest, _ := strings.Cut(trimmed, "/")
	tr := s.pllStringMap(ctx, lang)
	for _, b := range s.pllSlugBases() {
		base := b[1]
		if strings.Contains(base, "/") {
			continue
		}
		t := tr[base]
		if t == "" || t == base {
			continue
		}
		if toLang && seg == base {
			return "/" + t + "/" + rest
		}
		if !toLang && seg == t {
			return "/" + base + "/" + rest
		}
	}
	return rel
}

// Template functions ---------------------------------------------------------------------------

// golylangTemplateFuncs are the pll_* functions themes can call; they take the site view (".") first.
// While the plugin is off they keep working the way Polylang's function_exists() guards expect: no
// language, untranslated strings.
func (s *Server) golylangTemplateFuncs() template.FuncMap {
	ctxOf := func(v *siteView) context.Context {
		if v == nil || v.ctx == nil {
			return context.Background()
		}
		return v.ctx
	}
	field := func(l *pllLanguage, f []string) string {
		if l == nil {
			return ""
		}
		switch strings.Join(f, "") {
		case "locale":
			return l.Locale
		case "name":
			return l.Name
		case "w3c":
			return l.W3C()
		case "flag":
			return l.FlagEmoji()
		}
		return l.Slug
	}
	m := template.FuncMap{
		"pll_current_language": func(v *siteView, f ...string) string { return field(s.pllCurrent(ctxOf(v)), f) },
		"pll_default_language": func(v *siteView, f ...string) string {
			if !s.golylangOn() {
				return ""
			}
			return field(s.pllDefault(ctxOf(v)), f)
		},
		"pll__": func(v *siteView, str string) string { return s.pllThemeString(ctxOf(v), str) },
		"pll_e": func(v *siteView, str string) string { return s.pllThemeString(ctxOf(v), str) },
		"pll_register_string": func(v *siteView, name, str string, group ...string) string {
			if s.golylangOn() {
				s.pllRegisterString(ctxOf(v), name, str, strings.Join(group, ""), false)
			}
			return ""
		},
		"pll_translate_string": func(v *siteView, str, lang string) string {
			if !s.golylangOn() {
				return str
			}
			return s.pllTranslate(ctxOf(v), str, lang)
		},
		"pll_home_url": func(v *siteView, lang ...string) string {
			ctx := ctxOf(v)
			l := s.pllCurrent(ctx)
			if len(lang) > 0 {
				l = s.pllLang(ctx, lang[0])
			}
			if l == nil || !s.golylangOn() {
				return s.links(ctx).HomeURL("/")
			}
			return s.pllHomeURL(ctx, l)
		},
		"pll_languages_list": func(v *siteView) []string {
			if !s.golylangOn() {
				return nil
			}
			var out []string
			for _, l := range s.pllActiveLanguages(ctxOf(v)) {
				out = append(out, l.Slug)
			}
			return out
		},
		"pll_get_post": func(v *siteView, id int64, lang ...string) int64 {
			ctx := ctxOf(v)
			if !s.golylangOn() {
				return id
			}
			l := field(s.pllCurrent(ctx), nil)
			if len(lang) > 0 {
				l = lang[0]
			}
			return s.pllGet(ctx, pllPost, id, l)
		},
		"pll_get_term": func(v *siteView, id int64, lang ...string) int64 {
			ctx := ctxOf(v)
			if !s.golylangOn() {
				return id
			}
			l := field(s.pllCurrent(ctx), nil)
			if len(lang) > 0 {
				l = lang[0]
			}
			return s.pllGet(ctx, pllTerm, id, l)
		},
		"pll_the_languages": func(v *siteView, args ...string) template.HTML {
			if !s.golylangOn() || v == nil {
				return ""
			}
			rc := renderFrom(ctxOf(v))
			if rc == nil || rc.c == nil {
				return ""
			}
			a := pllParseArgs(args)
			return template.HTML(s.pllSwitcherHTML(rc.c, v.Q, a, "lang_choice_"+rc.uniqueID("")))
		},
	}
	for k, f := range s.pllGettextFuncs() { // __, _e, _x, _n, esc_html__, … (golylang_gettext.go)
		m[k] = f
	}
	return m
}

// pllThemeString is pll__(): registered on first use (under the active theme), translated into the
// current language.
func (s *Server) pllThemeString(ctx context.Context, str string) string {
	if !s.golylangOn() || str == "" {
		return str
	}
	source := "core"
	if t := s.activeTheme(); t != nil {
		source = "theme:" + t.Slug
	}
	s.pllCaptureString(ctx, str, "", pllDomainRegistered, source, pllRequestPath(ctx))
	if l := s.pllCurrent(ctx); l != nil {
		tr := s.pllTranslate(ctx, str, l.Slug)
		pllMarkKnown(ctx, tr, str)
		return tr
	}
	return str
}

// pllParseArgs reads "key=value" arguments of pll_the_languages (dropdown=1, show_flags=1 …).
func pllParseArgs(args []string) pllSwitcherArgs {
	a := pllDefaultArgs()
	for _, arg := range args {
		k, v, _ := strings.Cut(arg, "=")
		on := v == "1" || v == "true"
		switch strings.TrimSpace(k) {
		case "dropdown":
			a.Dropdown = on
		case "show_names":
			a.ShowNames = on
		case "show_flags":
			a.ShowFlags = on
		case "display_names_as":
			a.DisplayNamesAs = v
		case "force_home":
			a.ForceHome = on
		case "hide_current":
			a.HideCurrent = on
		case "hide_if_no_translation":
			a.HideIfNoTranslation = on
		case "hide_if_empty":
			a.HideIfEmpty = on
		case "post_id":
			a.PostID, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return a
}

// The Language switcher block (polylang/language-switcher), for posts, pages and block widgets.
func init() {
	blocks.RegisterDynamic("polylang/language-switcher", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc := renderFrom(ctx)
		if rc == nil || rc.s == nil || rc.c == nil || !rc.s.golylangOn() || len(rc.s.pllLanguages(ctx)) == 0 {
			return ""
		}
		a := pllDefaultArgs()
		attrs := map[string]any{}
		if raw, err := json.Marshal(b.Attrs); err == nil {
			_ = json.Unmarshal(raw, &attrs)
		}
		on := func(k string, def bool) bool {
			if v, ok := attrs[k].(bool); ok {
				return v
			}
			return def
		}
		a.Dropdown, a.ShowNames, a.ShowFlags = on("dropdown", false), on("show_names", true), on("show_flags", false)
		a.ForceHome, a.HideCurrent, a.HideIfNoTranslation = on("force_home", false), on("hide_current", false), on("hide_if_no_translation", false)
		id := "lang_choice_" + rc.uniqueID("")
		out := rc.s.pllSwitcherHTML(rc.c, rc.q, a, id)
		if a.Dropdown {
			return `<div class="wp-block-polylang-language-switcher">` + out + `</div>`
		}
		return `<ul class="wp-block-polylang-language-switcher">` + out + `</ul>`
	})
}

// pllSwitcherBlockScript registers the block in the editors (inserter, sidebar settings).
const pllSwitcherBlockScript = `( function() {
	if ( ! window.wp || ! wp.blocks || wp.blocks.getBlockType( 'polylang/language-switcher' ) ) { return; }
	var el = wp.element.createElement, ToggleControl = wp.components.ToggleControl, PanelBody = wp.components.PanelBody;
	var Inspector = ( wp.blockEditor || wp.editor ).InspectorControls;
	var opts = [ [ 'dropdown', 'Displays as a dropdown' ], [ 'show_names', 'Displays language names' ], [ 'show_flags', 'Displays flags' ],
		[ 'force_home', 'Forces link to front page' ], [ 'hide_current', 'Hides the current language' ], [ 'hide_if_no_translation', 'Hides languages with no translation' ] ];
	var attributes = {};
	opts.forEach( function( o ) { attributes[ o[0] ] = { type: 'boolean', default: o[0] === 'show_names' }; } );
	wp.blocks.registerBlockType( 'polylang/language-switcher', {
		apiVersion: 2, title: 'Language switcher', description: 'Add a language switcher to allow your visitors to select their preferred language.',
		icon: 'translation', category: 'widgets', attributes: attributes, supports: { html: false },
		edit: function( props ) {
			var controls = opts.map( function( o ) {
				return el( ToggleControl, { key: o[0], label: o[1], checked: !! props.attributes[ o[0] ], onChange: function( v ) { var a = {}; a[ o[0] ] = v; props.setAttributes( a ); } } );
			} );
			var bp = wp.blockEditor && wp.blockEditor.useBlockProps ? wp.blockEditor.useBlockProps() : {};
			return el( 'div', bp, el( Inspector, null, el( PanelBody, { title: 'Language switcher settings' }, controls ) ),
				el( 'div', { className: 'components-placeholder' }, el( 'span', { className: 'dashicons dashicons-translation' } ), ' Language switcher' ) );
		},
		save: function() { return null; }
	} );
} )();`
