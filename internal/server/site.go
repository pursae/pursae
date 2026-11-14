package server

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
)

// siteQuery is the parsed main query (WP_Query's query vars and conditionals).
type siteQuery struct {
	Kind      string // home, front-page, single, page, category, tag, taxonomy, author, date, search, post-type-archive, 404, feed
	PostType  string // the post type of a post-type-archive view
	Post      *store.Post
	Term      *store.Term
	Author    *store.User
	Year      int
	Month     int
	Day       int
	Search    string
	Paged     int
	Page      int    // page of a <!--nextpage--> split post (query var "page")
	CPage     int    // comment page (query var "cpage")
	Feed      string // "", rss2, atom, comments, comments-atom
	Preview   bool
	PostsPage *store.Post // page_for_posts when the blog lives on a page

	SearchProps string // unused: read only by the old page builder's files, deleted with them (R2)

	// Store, StoreTax and StoreTerm describe a Goocommerce view: product, shop, product_cat,
	// product_tag, product_attr, cart, checkout, myaccount; the archived term's taxonomy and id.
	// The store renders every view as a synthetic page (Kind "page"); these say what it really is.
	Store     string
	StoreTax  string
	StoreTerm int64
	// StoreURL is the unpaged canonical URL of a store view: the product permalink, the term link or
	// the shop URL ("" for a store search and for the Cart, Checkout and My account pages).
	StoreURL string

	// Forum is the Pursae Forum view a synthetic page shows (index, forum, topic, login, …; "" off
	// the forum) and ForumURL its unpaged canonical URL (forum_render.go).
	Forum    string
	ForumURL string
}

var (
	// The keyword has to be a whole path segment, like WordPress' rewrite rules ((.+?)/feed/…, (.+?)/page/…):
	// "chicken-feed" or "front-page/2" are slugs, not feeds or paged archives.
	pagedRe       = regexp.MustCompile(`^(?:(.*)/)?page/(\d+)$`)
	feedRe        = regexp.MustCompile(`^(?:(.*)/)?feed(?:/(rss2|rss|atom|rdf))?$`)
	dateRe        = regexp.MustCompile(`^(\d{4})(?:/(\d{1,2}))?(?:/(\d{1,2}))?$`)
	commentPageRe = regexp.MustCompile(`^(?:(.*)/)?comment-page-(\d+)$`)
	singlePageRe  = regexp.MustCompile(`^(.+)/(\d+)$`)
)

// verbosePageRulesRe is WP_Rewrite::init()'s test for use_verbose_page_rules.
var verbosePageRulesRe = regexp.MustCompile(`^[^%]*%(?:postname|category|tag|author)%`)

var errNotFound = errors.New("not found")

// ctxSiteQuery is the gin context key of the resolved main query ($wp_query for template tags).
const ctxSiteQuery = "wpgo.siteQuery"

// ctxSiteStatus is the gin context key of a status other than 200 for a synthetic page (a plugin form
// re-rendered with a validation error answers 422); renderSite uses it unless the view is a 404.
const ctxSiteStatus = "wpgo.siteStatus"

// siteQueryFrom returns the main query of the request, if the front end resolved one.
func siteQueryFrom(c *gin.Context) *siteQuery {
	if v, ok := c.Get(ctxSiteQuery); ok {
		q, _ := v.(*siteQuery)
		return q
	}
	return nil
}

// site serves every public URL that is not an admin, login or asset route.
func (s *Server) site(c *gin.Context) {
	if route, ok := c.GetQuery("rest_route"); ok && strings.Trim(c.Request.URL.Path, "/") == "" {
		s.rest.serve(c, route)
		return
	}
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && !s.gontactFallbackPost(c) && !s.gooAcceptsPost(c) && !s.forumAcceptsPost(c) { // Gontact Form without JS, Goocommerce forms, Pursae Forum forms
		c.Status(http.StatusNotFound)
		return
	}
	if s.adminsecAuthorQuery(c) { // AdminSecGo: /?author=N enumeration
		return
	}
	raw := strings.Trim(c.Request.URL.Path, "/")
	if s.seogoIntercept(c, raw) { // Pursae SEO: redirections, robots.txt, sitemaps (seogo_front.go)
		return
	}
	path := s.golylangSitePath(c, raw) // Golylang: without the language code
	if s.gooSiteIntercept(c, path) {   // Goocommerce: shop, products, store pages and forms (goo_front.go)
		return
	}
	if s.forumSiteIntercept(c, path) { // Pursae Forum: forum views and forms (forum_front.go)
		return
	}
	switch path {
	case "robots.txt":
		s.robots(c)
		return
	case "wp-sitemap.xml":
		s.sitemapIndex(c)
		return
	case "sitemap.xml":
		// Core sends the conventional address to its own index (WP_Sitemaps::redirect_sitemapxml).
		if s.settings.Bool("blog_public") {
			// Built from the slash-trimmed path (keeps a /fr/ language prefix): the raw one could start
			// with "//" and turn the Location into a protocol-relative off-site URL.
			c.Redirect(http.StatusMovedPermanently, "/"+strings.TrimSuffix(raw, "sitemap.xml")+"wp-sitemap.xml")
			return
		}
	case "favicon.ico":
		// No site icon is a 404 in WordPress; WordPress Go answers with the Gopher.
		if b, ct, err := s.brandFavicon(); err == nil {
			c.Header("Cache-Control", "public, max-age=3600") // a Hub logo replaces it (brand.go)
			brandImageHeaders(c)                              // the Hub's logo may be an SVG (brand.go)
			c.Data(http.StatusOK, ct, b)
			return
		}
		c.Status(http.StatusNotFound)
		return
	}
	if strings.HasPrefix(path, "wp-sitemap-") && strings.HasSuffix(path, ".xml") {
		s.sitemapPart(c, strings.TrimSuffix(strings.TrimPrefix(path, "wp-sitemap-"), ".xml"))
		return
	}
	if strings.HasPrefix(path, "wp-json") || strings.HasPrefix(path, "wp-content/") || strings.HasPrefix(path, "wp-includes/") || strings.HasPrefix(path, "wp-admin") {
		c.Status(http.StatusNotFound)
		return
	}

	q, err := s.resolve(c, path)
	if err != nil && !errors.Is(err, errNotFound) {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	if errors.Is(err, errNotFound) || q == nil {
		if target := s.oldSlugRedirect(c, path); target != "" { // wp_old_slug_redirect()
			c.Redirect(http.StatusMovedPermanently, target)
			return
		}
		q = &siteQuery{Kind: "404"}
	}
	if strings.HasPrefix(q.Kind, "redirect:") {
		c.Redirect(http.StatusMovedPermanently, strings.TrimPrefix(q.Kind, "redirect:"))
		return
	}
	if q.Post != nil && q.Page > 1 && q.Feed == "" {
		// A page number past the end of a <!--nextpage--> post is a 404 that redirect_canonical() sends to the post.
		if n := len(splitPages(q.Post.Content)); q.Page > n {
			c.Redirect(http.StatusMovedPermanently, s.links(c.Request.Context()).Permalink(q.Post))
			return
		}
	}
	// Golylang: the language of the queried post or term.
	s.golylangResolved(c, q)
	if target := s.canonicalRedirect(c, q, path); target != "" { // redirect_canonical()
		c.Redirect(http.StatusMovedPermanently, target)
		return
	}
	if s.seogoQueryRedirect(c, q) { // Pursae SEO: disabled author/date archives
		return
	}
	if s.gooGuardSiteQuery(c, q) { // Goocommerce Memberships: hidden or redirected restricted content
		return
	}
	c.Set(ctxSiteQuery, q)
	if q.Feed != "" {
		s.feed(c, q)
		return
	}
	s.renderSite(c, q)
}

// resolve maps a request to the main query: query vars first (they work with any
// permalink setting), then pretty permalink rules.
func (s *Server) resolve(c *gin.Context, path string) (*siteQuery, error) {
	ctx := c.Request.Context()
	v := c.Request.URL.Query()
	q := &siteQuery{Paged: 1}
	if n, _ := strconv.Atoi(v.Get("paged")); n > 1 {
		q.Paged = n
	}
	q.Preview = v.Get("preview") == "true"
	if n, _ := strconv.Atoi(v.Get("page")); n > 1 {
		q.Page = n
	}
	if n, _ := strconv.Atoi(v.Get("cpage")); n > 0 {
		q.CPage = n
	}
	if f := v.Get("feed"); f != "" {
		switch f {
		case "atom", "comments-atom":
			q.Feed = f
		case "comments-rss2", "comments":
			q.Feed = "comments"
		default:
			q.Feed = "rss2"
		}
		if v.Get("withcomments") == "1" && q.Feed == "atom" {
			q.Feed = "comments-atom"
		} else if v.Get("withcomments") == "1" {
			q.Feed = "comments"
		}
	}

	if aid, _ := strconv.ParseInt(v.Get("attachment_id"), 10, 64); aid > 0 {
		if a, err := s.store.GetAttachment(ctx, aid); err == nil {
			q.Kind = "redirect:" + s.uploadURL(c, a.FilePath)
			return q, nil
		}
		return nil, errNotFound
	}

	// "/page/N/" and "/feed/" with no slug before them belong to whatever the query
	// vars select ("/page/2/?taxonomy=genre&term=fiction" is links.Builder.Paged of a
	// query-var archive), so apply them before the query-var switch returns.
	if m := feedRe.FindStringSubmatch(path); m != nil && m[1] == "" {
		path = ""
		if q.Feed == "" {
			q.Feed = "rss2"
			if m[2] == "atom" {
				q.Feed = "atom"
			}
		}
	}
	if m := pagedRe.FindStringSubmatch(path); m != nil && m[1] == "" {
		path = ""
		q.Paged, _ = strconv.Atoi(m[2])
	}

	// Query-var URLs (plain permalinks).
	switch {
	case v.Get("p") != "" || v.Get("page_id") != "":
		id, _ := strconv.ParseInt(v.Get("p")+v.Get("page_id"), 10, 64)
		p, err := s.store.GetPost(ctx, id)
		if err != nil {
			return nil, errNotFound
		}
		return s.singular(c, q, p)
	case v.Get("s") != "" || v.Has("s"):
		q.Kind, q.Search = "search", strings.TrimSpace(v.Get("s"))
		return q, nil
	case v.Get("cat") != "":
		id, _ := strconv.ParseInt(v.Get("cat"), 10, 64)
		t, err := s.store.GetTerm(ctx, id)
		if err != nil || t.Taxonomy != "category" {
			return nil, errNotFound
		}
		q.Kind, q.Term = "category", t
		return q, nil
	case v.Get("tag") != "":
		t, err := s.store.TermBySlug(ctx, "post_tag", v.Get("tag"))
		if err != nil {
			return nil, errNotFound
		}
		q.Kind, q.Term = "tag", t
		return q, nil
	case v.Get("author") != "":
		id, _ := strconv.ParseInt(v.Get("author"), 10, 64)
		u, err := s.store.UserByID(ctx, id)
		if err != nil {
			return nil, errNotFound
		}
		q.Kind, q.Author = "author", u
		return q, nil
	case v.Get("post_format") != "":
		t := postFormatTerm(v.Get("post_format"))
		if t == nil {
			return nil, errNotFound
		}
		q.Kind, q.Term = "taxonomy", t
		return q, nil
	case v.Get("taxonomy") != "" && v.Get("term") != "" && siteTaxonomy(v.Get("taxonomy")) != nil:
		t, err := s.store.TermBySlug(ctx, v.Get("taxonomy"), v.Get("term"))
		if err != nil {
			return nil, errNotFound
		}
		q.Kind, q.Term = "taxonomy", t
		return q, nil
	case path == "" && v.Get("post_type") != "" && siteArchiveType(v.Get("post_type")) != nil:
		q.Kind, q.PostType = "post-type-archive", v.Get("post_type")
		return q, nil
	case v.Get("m") != "":
		m := v.Get("m")
		if len(m) >= 4 {
			q.Year, _ = strconv.Atoi(m[:4])
		}
		if len(m) >= 6 {
			q.Month, _ = strconv.Atoi(m[4:6])
		}
		if len(m) >= 8 {
			q.Day, _ = strconv.Atoi(m[6:8])
		}
		q.Kind = "date"
		return q, nil
	}

	if path == "" {
		return s.home(c, q)
	}

	// Pretty permalinks.
	if m := feedRe.FindStringSubmatch(path); m != nil {
		path = m[1]
		q.Feed = "rss2"
		if m[2] == "atom" {
			q.Feed = "atom"
		}
		if path == "comments" {
			path = ""
			if q.Feed == "atom" {
				q.Feed = "comments-atom"
			} else {
				q.Feed = "comments"
			}
		}
		if path == "" {
			return s.home(c, q)
		}
	}
	if m := pagedRe.FindStringSubmatch(path); m != nil {
		q.Paged, _ = strconv.Atoi(m[2])
		path = m[1]
		if path == "" {
			return s.home(c, q)
		}
	}
	if m := commentPageRe.FindStringSubmatch(path); m != nil {
		q.CPage, _ = strconv.Atoi(m[2])
		path = m[1]
	}
	sq, err := s.resolvePretty(c, q, path)
	if errors.Is(err, errNotFound) {
		// "/post-name/2/" is page 2 of a <!--nextpage--> post (the (.+?)/([0-9]{1,})/?$ rewrite rule).
		if m := singlePageRe.FindStringSubmatch(path); m != nil {
			if sq2, err2 := s.resolvePretty(c, &siteQuery{Paged: q.Paged, Preview: q.Preview, Feed: q.Feed, CPage: q.CPage}, m[1]); err2 == nil && sq2 != nil && sq2.Post != nil {
				sq2.Page, _ = strconv.Atoi(m[2])
				return sq2, nil
			}
		}
	}
	return sq, err
}

func (s *Server) resolvePretty(c *gin.Context, q *siteQuery, path string) (*siteQuery, error) {
	ctx := c.Request.Context()
	lb := s.links(ctx)
	catBase := strings.Trim(defaultStr(s.settings.String("category_base"), "category"), "/")
	tagBase := strings.Trim(defaultStr(s.settings.String("tag_base"), "tag"), "/")
	switch {
	case strings.HasPrefix(path, catBase+"/"):
		parts := strings.Split(strings.TrimPrefix(path, catBase+"/"), "/")
		t, err := s.store.TermBySlug(ctx, "category", parts[len(parts)-1])
		if err != nil {
			return nil, errNotFound
		}
		q.Kind, q.Term = "category", t
		return q, nil
	case strings.HasPrefix(path, tagBase+"/"):
		t, err := s.store.TermBySlug(ctx, "post_tag", strings.TrimPrefix(path, tagBase+"/"))
		if err != nil {
			return nil, errNotFound
		}
		q.Kind, q.Term = "tag", t
		return q, nil
	case strings.HasPrefix(path, "type/"): // post format archives (the post_format rewrite base)
		t := postFormatTerm(strings.TrimPrefix(path, "type/"))
		if t == nil {
			return nil, errNotFound
		}
		q.Kind, q.Term = "taxonomy", t
		return q, nil
	case strings.HasPrefix(path, "author/"):
		u, err := s.store.UserByNicename(ctx, strings.TrimPrefix(path, "author/"))
		if err != nil {
			return nil, errNotFound
		}
		q.Kind, q.Author = "author", u
		return q, nil
	case strings.HasPrefix(path, "search/"):
		raw, _ := url.PathUnescape(strings.TrimPrefix(path, "search/"))
		q.Kind, q.Search = "search", strings.ReplaceAll(raw, "+", " ")
		return q, nil
	}

	if sq, ok := s.resolveTypePath(c, q, path); ok {
		return sq, nil
	}

	if m, ok := links.NewMatcher(lb.Structure).Match(path); ok {
		var p *store.Post
		var err error
		switch {
		case m["post_id"] != "":
			id, _ := strconv.ParseInt(m["post_id"], 10, 64)
			p, err = s.store.GetPost(ctx, id)
		case m["postname"] != "":
			// use_verbose_page_rules: when the structure starts with %postname%, %category%, %tag% or
			// %author%, WordPress tries the page rules first, so a page wins over a post of the same shape.
			if verbosePageRulesRe.MatchString(lb.Structure) {
				if pg, perr := s.store.PageByPath(ctx, path); perr == nil && (pg.Status == "publish" || pg.Status == "private") {
					p = pg
					break
				}
			}
			p, err = s.store.PostBySlug(ctx, "post", m["postname"])
			if errors.Is(err, store.ErrNotFound) {
				// "/%postname%/" shares its shape with top-level pages.
				p, err = s.store.PageByPath(ctx, path)
			}
		}
		if err == nil && p != nil && (p.Type == "post" || p.Type == "page") {
			return s.singular(c, q, p)
		}
	}
	if m := dateRe.FindStringSubmatch(path); m != nil {
		q.Kind = "date"
		q.Year, _ = strconv.Atoi(m[1])
		q.Month, _ = strconv.Atoi(m[2])
		q.Day, _ = strconv.Atoi(m[3])
		return q, nil
	}
	if p, err := s.store.PageByPath(ctx, path); err == nil {
		return s.singular(c, q, p)
	}
	return nil, errNotFound
}

// Custom post types ----------------------------------------------------------------------------

// siteCPT is a public post type registered in Pursae Custom Fields that the front end routes, or nil.
// The core types, the Web Builder's templates and Pursae Commerce products (routed by goo_front.go)
// are never returned.
func siteCPT(typ string) *acfgoPostTypeDef {
	switch typ {
	case "", "post", "page", "attachment", "revision", "nav_menu_item", "wb_template", "product", "product_variation":
		return nil
	}
	acfgoRegistry.RLock()
	pt := acfgoRegistry.types[typ]
	acfgoRegistry.RUnlock()
	if pt == nil || !pt.Public {
		return nil
	}
	return pt
}

// siteViewableType is is_post_type_viewable() for the core's front end: posts, pages and public
// Pursae Custom Fields types.
func siteViewableType(typ string) bool {
	return typ == "post" || typ == "page" || siteCPT(typ) != nil
}

// siteSearchTypes are the post types a front search covers, like WP_Query's 'any' for is_search():
// posts, pages and the public Pursae Custom Fields types not excluded from search. Theme Builder
// templates and Goocommerce products never are (siteCPT refuses them).
func siteSearchTypes() []string {
	out := []string{"post", "page"}
	acfgoRegistry.RLock()
	names := make([]string, 0, len(acfgoRegistry.types))
	for name, pt := range acfgoRegistry.types {
		if !pt.ExcludeFromSearch {
			names = append(names, name)
		}
	}
	acfgoRegistry.RUnlock()
	sort.Strings(names)
	for _, name := range names {
		if siteCPT(name) != nil {
			out = append(out, name)
		}
	}
	return out
}

// siteCPTSlugs are the rewrite slug and archive slug of a routed post type ("" archive: none).
func siteCPTSlugs(pt *acfgoPostTypeDef) (base, archive string) {
	base = strings.Trim(defaultStr(pt.Slug, pt.Name), "/")
	if pt.HasArchive {
		archive = strings.Trim(defaultStr(pt.ArchiveSlug, base), "/")
	}
	return base, archive
}

// siteTypeBase is links.Builder.TypeBase.
func siteTypeBase(typ string) (string, string) {
	pt := siteCPT(typ)
	if pt == nil {
		return "", ""
	}
	return siteCPTSlugs(pt)
}

// siteTypeHierarchical is links.Builder.TypeHierarchical.
func siteTypeHierarchical(typ string) bool {
	pt := siteCPT(typ)
	return pt != nil && pt.Hierarchical
}

// siteArchiveType is the routed post type typ when it has an archive, else nil.
func siteArchiveType(typ string) *acfgoPostTypeDef {
	pt := siteCPT(typ)
	if pt == nil || !pt.HasArchive {
		return nil
	}
	return pt
}

// siteRoutedTypes lists the routed custom post types, sorted by name for a stable match order.
func siteRoutedTypes() []*acfgoPostTypeDef {
	acfgoRegistry.RLock()
	names := make([]string, 0, len(acfgoRegistry.types))
	for n := range acfgoRegistry.types {
		names = append(names, n)
	}
	acfgoRegistry.RUnlock()
	sort.Strings(names)
	var out []*acfgoPostTypeDef
	for _, n := range names {
		if pt := siteCPT(n); pt != nil {
			out = append(out, pt)
		}
	}
	return out
}

// resolveTypePath matches the rewrite rules of the routed post types: "<archive slug>" is the
// post type archive, "<slug>/<post>" (with ancestors for hierarchical types) a single entry.
// ok false lets the remaining rules (posts, dates, pages) try the path.
func (s *Server) resolveTypePath(c *gin.Context, q *siteQuery, path string) (*siteQuery, bool) {
	ctx := c.Request.Context()
	for _, pt := range siteRoutedTypes() {
		base, archive := siteCPTSlugs(pt)
		if archive != "" && path == archive {
			q.Kind, q.PostType = "post-type-archive", pt.Name
			return q, true
		}
		if base == "" || !strings.HasPrefix(path, base+"/") {
			continue
		}
		rest := strings.Trim(strings.TrimPrefix(path, base+"/"), "/")
		if rest == "" {
			continue
		}
		var p *store.Post
		var err error
		switch {
		case pt.Hierarchical: // a child lives under its ancestors' path
			p, err = s.typePostByPath(ctx, pt.Name, rest)
		case strings.Contains(rest, "/"):
			continue
		default:
			p, err = s.store.PostBySlug(ctx, pt.Name, rest)
		}
		if err != nil || p == nil || p.Type != pt.Name {
			continue
		}
		if sq, err := s.singular(c, q, p); err == nil {
			return sq, true
		}
		return nil, false
	}
	return nil, false
}

// typePostByPath is get_page_by_path() for a hierarchical custom post type.
func (s *Server) typePostByPath(ctx context.Context, typ, path string) (*store.Post, error) {
	var parent, id int64
	for _, slug := range strings.Split(path, "/") {
		if slug == "" {
			return nil, errNotFound
		}
		if err := s.store.Pool.QueryRow(ctx, `SELECT id FROM posts
			WHERE type = $1 AND slug = $2 AND coalesce(parent_id, 0) = $3 AND status NOT IN ('auto-draft', 'trash')
			ORDER BY (status = 'publish') DESC, id LIMIT 1`, typ, slug, parent).Scan(&id); err != nil {
			return nil, errNotFound
		}
		parent = id
	}
	return s.store.GetPost(ctx, id)
}

// splitPages is the <!--nextpage--> split of setup_postdata(): the block delimiters around the tag go,
// a leading tag is ignored and the remaining pages are returned in order.
func splitPages(content string) []string {
	if !strings.Contains(content, "<!--nextpage-->") {
		return []string{content}
	}
	content = strings.ReplaceAll(content, "\n<!--nextpage-->\n", "<!--nextpage-->")
	content = strings.ReplaceAll(content, "\n<!--nextpage-->", "<!--nextpage-->")
	content = strings.ReplaceAll(content, "<!--nextpage-->\n", "<!--nextpage-->")
	content = strings.ReplaceAll(content, "<!-- wp:nextpage -->", "")
	content = strings.ReplaceAll(content, "<!-- /wp:nextpage -->", "")
	content = strings.TrimPrefix(content, "<!--nextpage-->")
	return strings.Split(content, "<!--nextpage-->")
}

func (s *Server) home(c *gin.Context, q *siteQuery) (*siteQuery, error) {
	if s.settings.String("show_on_front") == "page" && q.Feed == "" {
		if id := s.golylangPage(c, int64(s.settings.Int("page_on_front"))); id > 0 {
			if p, err := s.store.GetPost(c, id); err == nil && p.Status == "publish" {
				if q.Page == 0 && q.Paged > 1 {
					q.Page = q.Paged // "/page/2/" is page 2 of a <!--nextpage--> front page (links.PostPageLink)
				}
				sq, err := s.singular(c, q, p)
				if sq != nil {
					sq.Kind = "front-page"
				}
				return sq, err
			}
		}
	}
	q.Kind = "home"
	return q, nil
}

// singular checks visibility the way WP_Query does: published posts for everyone,
// private posts and previews only for users who may read them.
func (s *Server) singular(c *gin.Context, q *siteQuery, p *store.Post) (*siteQuery, error) {
	u := currentUser(c)
	switch p.Status {
	case "publish":
	case "private":
		if !u.Can("read_post", p) {
			return nil, errNotFound
		}
	case "draft", "pending", "future", "auto-draft":
		if !q.Preview || !u.Can("edit_post", p) {
			return nil, errNotFound
		}
	default:
		return nil, errNotFound
	}
	if !siteViewableType(p.Type) {
		return nil, errNotFound
	}
	p = s.previewAutosave(c, q, p, u)
	q.Post = p
	q.Kind = "single"
	if p.Type == "page" {
		q.Kind = "page"
		if p.ID == s.golylangPage(c, int64(s.settings.Int("page_for_posts"))) && s.settings.String("show_on_front") == "page" {
			q.Kind, q.PostsPage, q.Post = "home", p, nil
		}
	}
	return q, nil
}

// previewAutosave is _set_preview(): a preview link of an autosave (preview_id + a valid
// post_preview_<id> nonce, from the editor's Preview button) shows the current user's autosave
// title, content and excerpt instead of the stored ones. The post is copied, never changed in place.
func (s *Server) previewAutosave(c *gin.Context, q *siteQuery, p *store.Post, u *store.User) *store.Post {
	if !q.Preview || c.Query("preview_id") != itoa64(p.ID) || u == nil || u.ID == 0 || !u.Can("edit_post", p) {
		return p
	}
	if !s.checkNonceValue(c, c.Query("preview_nonce"), "post_preview_"+itoa64(p.ID)) {
		return p
	}
	revs, err := s.revisions(c, p.ID, true, u.ID)
	// An autosave older than the post's last save is stale: the saved post is newer.
	if err != nil || len(revs) == 0 || revs[0].CreatedAt.Before(p.ModifiedAt) {
		return p
	}
	cp := *p
	cp.Title, cp.Content, cp.Excerpt = revs[0].Title, revs[0].Content, revs[0].Excerpt
	return &cp
}

// mainPosts runs the archive query for a non-singular request.
func (s *Server) mainPosts(ctx context.Context, q *siteQuery, u *store.User) ([]*store.Post, int, int, error) {
	perPage := s.settings.Int("posts_per_page")
	if perPage < 1 {
		perPage = 10
	}
	pq := store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, Page: q.Paged, PerPage: perPage, Loc: s.loc()}
	if u.Can("read_private_posts", nil) {
		pq.Statuses = append(pq.Statuses, "private")
	}
	switch q.Kind {
	case "home":
		// handled below
	case "category":
		pq.TermIDs = s.categoryWithDescendants(ctx, q.Term.ID)
	case "tag":
		pq.TermIDs = []int64{q.Term.ID}
	case "taxonomy":
		s.taxonomyQuery(ctx, q.Term, &pq)
	case "author":
		pq.AuthorID = q.Author.ID
	case "date":
		pq.Year, pq.Month, pq.Day = q.Year, q.Month, q.Day
	case "post-type-archive":
		pq.Types = []string{q.PostType}
	case "search":
		pq.Types = siteSearchTypes()
		pq.Search = q.Search
		pq.OrderBy = "relevance" // title matches first, then ts_rank, then newest
		// WP_Query::parse_search(): logged-out visitors do not search password-protected posts, or
		// search would reveal the hidden text word by word.
		pq.ExcludePassworded = u == nil
		if q.Search == "" {
			return nil, 0, perPage, nil
		}
	}
	pq.ExcludeID = append(pq.ExcludeID, s.gooListingExclude(withPluginUser(ctx, u), "post")...) // Goocommerce Memberships
	posts, total, err := s.store.QueryPosts(ctx, pq)
	if err == nil && q.Kind == "home" && q.Paged <= 1 {
		posts, err = s.prependSticky(ctx, posts, pq)
	}
	s.pllPrimePosts(ctx, posts) // Golylang: the loop's languages in one query
	return posts, total, perPage, err
}

// taxonomyQuery narrows an archive query to a term of a taxonomy other than category and post_tag:
// a post format (posts.format) or a term of a public Pursae Custom Fields taxonomy, listing the
// routed post types the taxonomy is registered for (hierarchical ones include child terms).
func (s *Server) taxonomyQuery(ctx context.Context, t *store.Term, pq *store.PostQuery) {
	if t.Taxonomy == "post_format" {
		pq.Format = strings.TrimPrefix(t.Slug, "post-format-")
		return
	}
	tx := siteTaxonomy(t.Taxonomy)
	if tx == nil {
		pq.IDs = []int64{0} // no longer registered: nothing
		return
	}
	var types []string
	for _, typ := range tx.ObjectTypes {
		if typ == "post" || typ == "page" || siteCPT(typ) != nil {
			types = append(types, typ)
		}
	}
	if len(types) == 0 {
		types = []string{"post"}
	}
	pq.Types = types
	ids := []int64{t.ID}
	if tx.Hierarchical {
		all, _ := s.store.HierarchicalTerms(ctx, t.Taxonomy, false)
		parent := map[int64]int64{}
		for _, x := range all {
			parent[x.ID] = x.ParentID
		}
		for _, x := range all {
			for p, n := x.ParentID, 0; p != 0 && n < 100; p, n = parent[p], n+1 {
				if p == t.ID {
					ids = append(ids, x.ID)
					break
				}
			}
		}
	}
	pq.TermIDs = ids
}

// siteTaxonomy is a public taxonomy registered in Pursae Custom Fields that the front end routes
// (/?taxonomy=…&term=…), or nil. The core and Pursae Commerce taxonomies have their own routes.
func siteTaxonomy(tax string) *acfgoTaxonomyDef {
	switch tax {
	case "", "category", "post_tag", "post_format", "nav_menu", "product_cat", "product_tag":
		return nil
	}
	if strings.HasPrefix(tax, "pa_") {
		return nil
	}
	acfgoRegistry.RLock()
	tx := acfgoRegistry.taxes[tax]
	acfgoRegistry.RUnlock()
	if tx == nil || !tx.Public {
		return nil
	}
	return tx
}

// postFormatTerm is the post_format term of a format slug ("aside" or "post-format-aside"), or nil for
// "standard" and unknown formats: post formats live in posts.format, so the term is synthetic (id 0).
func postFormatTerm(slug string) *store.Term {
	f := strings.TrimPrefix(strings.Trim(slug, "/"), "post-format-")
	if f == "standard" || postFormatLabels[f] == "" {
		return nil
	}
	return &store.Term{Taxonomy: "post_format", Slug: "post-format-" + f, Name: postFormatLabels[f]}
}

// postFormatPlurals are the archive titles of post formats (get_the_archive_title()).
var postFormatPlurals = map[string]string{"aside": "Asides", "gallery": "Galleries", "image": "Images", "video": "Videos",
	"quote": "Quotes", "link": "Links", "status": "Statuses", "audio": "Audio", "chat": "Chats"}

// categoryWithDescendants is a category id plus every descendant: category archives and feeds include
// child categories, like WP_Query's category__in with include_children.
func (s *Server) categoryWithDescendants(ctx context.Context, id int64) []int64 {
	ids := []int64{id}
	all, _ := s.store.HierarchicalTerms(ctx, "category", false)
	parent := map[int64]int64{}
	for _, t := range all {
		parent[t.ID] = t.ParentID
	}
	for _, t := range all {
		for p, n := t.ParentID, 0; p != 0 && n < 100; p, n = parent[p], n+1 {
			if p == id {
				ids = append(ids, t.ID)
				break
			}
		}
	}
	return ids
}

// prependSticky moves sticky posts to the top of the first blog page, adding the ones that are not on
// it (they come on top of posts_per_page), as WP_Query does for is_home() page 1.
func (s *Server) prependSticky(ctx context.Context, posts []*store.Post, pq store.PostQuery) ([]*store.Post, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT id FROM posts WHERE sticky AND type = 'post' AND status = ANY($1)`, pq.Statuses)
	if err != nil {
		return posts, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return posts, nil
	}
	sticky, _, err := s.store.QueryPosts(ctx, store.PostQuery{IDs: ids, ExcludeID: pq.ExcludeID, Types: []string{"post"}, Statuses: pq.Statuses, PerPage: -1, Page: 1, Loc: pq.Loc})
	if err != nil {
		return posts, err
	}
	isSticky := map[int64]bool{}
	for _, p := range sticky {
		isSticky[p.ID] = true
	}
	out := append([]*store.Post{}, sticky...)
	for _, p := range posts {
		if !isSticky[p.ID] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Server) renderSite(c *gin.Context, q *siteQuery) {
	v, err := s.buildSiteView(c, q)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusOK
	if n := c.GetInt(ctxSiteStatus); n >= 200 && n < 600 {
		status = n // a plugin form re-rendered with an error (Pursae Forum: 422)
	}
	if q.Kind == "404" {
		status = http.StatusNotFound
		if s.seogo404(c) { // Pursae SEO: 404 monitor and fallback redirect
			return
		}
	}
	// Search results are public pages like archives: Pursae Cache stores them for a short time (searchTTL)
	// and purges them with the "search" tag when content changes (wpgocache.go).
	if q.Preview {
		c.Header("Cache-Control", "no-cache, must-revalidate, max-age=0")
	}
	s.cacheTags(c, q)
	if s.wbRenderPage(c, v, status) { // Pursae Web Builder canvas (webbuilder_front.go)
		return
	}
	s.renderSiteTemplate(c, v, status)
}

// postPassCookie is the cookie that remembers an entered post password.
const postPassCookie = "wp-postpass"

func (s *Server) passwordRequired(c *gin.Context, p *store.Post) bool {
	if p.Password == "" {
		return false
	}
	ck, err := c.Cookie(postPassCookie)
	if err != nil {
		return true
	}
	return !hmac.Equal([]byte(ck), []byte(s.postPassValue(p.Password)))
}

// postPassValue is the wp-postpass cookie for a post password: a tick-free HMAC with the site secret, so
// it lasts as long as the cookie (WordPress keeps a hash of the password there) and stops matching as
// soon as the post's password changes.
func (s *Server) postPassValue(pw string) string {
	return s.auth.Sign("postpass|" + pw)
}

// postPass handles the post password form (wp-login.php?action=postpass).
func (s *Server) postPass(c *gin.Context) {
	pw := c.PostForm("post_password")
	http.SetCookie(c.Writer, &http.Cookie{Name: postPassCookie, Value: s.postPassValue(pw), Path: "/",
		MaxAge: 10 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.httpsRequest(c.Request)})
	c.Redirect(http.StatusFound, safeRedirect(refererPath(c), "/"))
}

func refererPath(c *gin.Context) string {
	r, err := url.Parse(c.Request.Referer())
	if err != nil || r.Path == "" {
		return "/"
	}
	if r.Host != "" && r.Host != c.Request.Host {
		return "/"
	}
	return r.RequestURI()
}

// documentTitle is wp_get_document_title() with title-tag support.
func (s *Server) documentTitle(q *siteQuery, archiveTitle string) string {
	site := s.settings.String("blogname")
	sep := " &#8211; "
	var parts []string
	switch q.Kind {
	case "404":
		parts = []string{"Page not found"}
	case "search":
		parts = []string{"Search Results for &#8220;" + html.EscapeString(q.Search) + "&#8221;"}
	case "front-page":
		parts = []string{}
	case "home":
		if q.PostsPage != nil {
			parts = []string{html.EscapeString(q.PostsPage.Title)}
		}
	case "single", "page":
		parts = []string{html.EscapeString(q.Post.Title)}
	default:
		parts = []string{archiveTitle}
	}
	if q.Paged > 1 {
		parts = append(parts, "Page "+strconv.Itoa(q.Paged))
	}
	if (q.Kind == "home" && q.PostsPage == nil) || q.Kind == "front-page" {
		parts = append([]string{html.EscapeString(site)}, parts...)
		if d := s.settings.String("blogdescription"); d != "" && q.Paged <= 1 {
			parts = append(parts, html.EscapeString(d))
		}
	} else {
		parts = append(parts, html.EscapeString(site))
	}
	return strings.Join(parts, sep)
}

func itoa(n int) string { return strconv.Itoa(n) }

func monthName(m int) string {
	if m < 1 || m > 12 {
		return ""
	}
	return time.Month(m).String()
}

func (q *siteQuery) archiveTitle() (string, string) {
	switch q.Kind {
	case "category":
		return "Category: <span>" + html.EscapeString(q.Term.Name) + "</span>", html.EscapeString(q.Term.Name)
	case "tag":
		return "Tag: <span>" + html.EscapeString(q.Term.Name) + "</span>", html.EscapeString(q.Term.Name)
	case "author":
		return "Author: <span>" + html.EscapeString(q.Author.DisplayName) + "</span>", html.EscapeString(q.Author.DisplayName)
	case "date":
		switch {
		case q.Day != 0:
			d := time.Date(q.Year, time.Month(q.Month), q.Day, 0, 0, 0, 0, time.UTC)
			label := d.Format("January 2, 2006")
			return "Day: <span>" + label + "</span>", label
		case q.Month != 0:
			label := monthName(q.Month) + " " + itoa(q.Year)
			return "Month: <span>" + label + "</span>", label
		default:
			return "Year: <span>" + itoa(q.Year) + "</span>", itoa(q.Year)
		}
	case "taxonomy":
		if q.Term.Taxonomy == "post_format" {
			t := postFormatPlurals[strings.TrimPrefix(q.Term.Slug, "post-format-")]
			return html.EscapeString(t), html.EscapeString(t)
		}
		label := q.Term.Taxonomy
		if tx := siteTaxonomy(q.Term.Taxonomy); tx != nil && tx.label("singular_name") != "" {
			label = tx.label("singular_name")
		}
		return html.EscapeString(label) + ": <span>" + html.EscapeString(q.Term.Name) + "</span>", html.EscapeString(q.Term.Name)
	case "post-type-archive":
		name := q.PostType
		if pt := siteCPT(q.PostType); pt != nil && pt.label("name") != "" {
			name = pt.label("name")
		}
		return "Archives: <span>" + html.EscapeString(name) + "</span>", html.EscapeString(name)
	}
	return "Archives", "Archives"
}

var _ = fmt.Sprintf
var _ template.HTML
