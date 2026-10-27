package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
)

// siteQuery is the parsed main query (WP_Query's query vars and conditionals).
type siteQuery struct {
	Kind      string // home, front-page, single, page, category, tag, author, date, search, 404, feed
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

	SearchProps string // gap-loops2: the Search widget's e_search_props (gowebeditor_loop.go)
}

var (
	pagedRe       = regexp.MustCompile(`^(.*?)/?page/(\d+)$`)
	feedRe        = regexp.MustCompile(`^(.*?)/?feed(?:/(rss2|rss|atom|rdf))?$`)
	dateRe        = regexp.MustCompile(`^(\d{4})(?:/(\d{1,2}))?(?:/(\d{1,2}))?$`)
	commentPageRe = regexp.MustCompile(`^(.*?)/?comment-page-(\d+)$`)
	singlePageRe  = regexp.MustCompile(`^(.+)/(\d+)$`)
)

var errNotFound = errors.New("not found")

// ctxSiteQuery is the gin context key of the resolved main query ($wp_query for template tags).
const ctxSiteQuery = "wpgo.siteQuery"

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
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.Status(http.StatusNotFound)
		return
	}
	path := strings.Trim(c.Request.URL.Path, "/")
	switch path {
	case "robots.txt":
		s.robots(c)
		return
	case "wp-sitemap.xml":
		s.sitemapIndex(c)
		return
	case "favicon.ico":
		// No site icon is a 404 in WordPress; WordPress Go answers with the Gopher.
		if b, err := fs.ReadFile(s.web, "wp-includes/images/wpgo/gopher-32.png"); err == nil {
			c.Header("Cache-Control", "public, max-age=86400")
			c.Data(http.StatusOK, "image/png", b)
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
	if target := s.canonicalRedirect(c, q, path); target != "" { // redirect_canonical()
		c.Redirect(http.StatusMovedPermanently, target)
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
		if s.golementorOn() {
			if q.SearchProps = v.Get("e_search_props"); q.SearchProps != "" && !s.gweSearchPropsOK(ctx, q.SearchProps) { // gap-loops2
				return nil, errNotFound
			}
		}
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

	if m, ok := links.NewMatcher(lb.Structure).Match(path); ok {
		var p *store.Post
		var err error
		switch {
		case m["post_id"] != "":
			id, _ := strconv.ParseInt(m["post_id"], 10, 64)
			p, err = s.store.GetPost(ctx, id)
		case m["postname"] != "":
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
		if id := int64(s.settings.Int("page_on_front")); id > 0 {
			if p, err := s.store.GetPost(c, id); err == nil && p.Status == "publish" {
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
	if p.Type != "post" && p.Type != "page" {
		return nil, errNotFound
	}
	q.Post = p
	q.Kind = "single"
	if p.Type == "page" {
		q.Kind = "page"
		if p.ID == int64(s.settings.Int("page_for_posts")) && s.settings.String("show_on_front") == "page" {
			q.Kind, q.PostsPage, q.Post = "home", p, nil
		}
	}
	return q, nil
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
		// Category archives include child categories, like WP_Query's category__in with include_children.
		ids := []int64{q.Term.ID}
		all, _ := s.store.HierarchicalTerms(ctx, "category", false)
		parent := map[int64]int64{}
		for _, t := range all {
			parent[t.ID] = t.ParentID
		}
		for _, t := range all {
			for p := t.ParentID; p != 0; p = parent[p] {
				if p == q.Term.ID {
					ids = append(ids, t.ID)
					break
				}
			}
		}
		pq.TermIDs = ids
	case "tag":
		pq.TermIDs = []int64{q.Term.ID}
	case "author":
		pq.AuthorID = q.Author.ID
	case "date":
		pq.Year, pq.Month, pq.Day = q.Year, q.Month, q.Day
	case "search":
		pq.Types = []string{"post", "page"}
		pq.Search = q.Search
		if q.Search == "" {
			return nil, 0, perPage, nil
		}
		if q.SearchProps != "" { // gap-loops2: narrowed by the Search widget (gowebeditor_loop.go)
			return s.gweSearchPropsPosts(ctx, q, u)
		}
	}
	posts, total, err := s.store.QueryPosts(ctx, pq)
	if err == nil && q.Kind == "home" && q.Paged <= 1 {
		posts, err = s.prependSticky(ctx, posts, pq)
	}
	return posts, total, perPage, err
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
	sticky, _, err := s.store.QueryPosts(ctx, store.PostQuery{IDs: ids, Types: []string{"post"}, Statuses: pq.Statuses, PerPage: -1, Page: 1, Loc: pq.Loc})
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
	if q.Kind == "404" {
		status = http.StatusNotFound
	}
	if q.Preview || q.Kind == "search" {
		c.Header("Cache-Control", "no-cache, must-revalidate, max-age=0")
	}
	s.cacheTags(c, q)
	if s.gweRenderTemplate(c, v, status) { // Golementor Canvas / Full Width page templates
		return
	}
	if s.gweRenderShell(c, v, status) { // Golementor custom theme / theme parts
		return
	}
	s.renderThemeTemplate(c, v.Theme.Slug, status, "theme/"+v.Template, v)
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
	return ck != s.auth.Nonce("postpass|"+p.Password, 0, "")
}

// postPass handles the post password form (wp-login.php?action=postpass).
func (s *Server) postPass(c *gin.Context) {
	pw := c.PostForm("post_password")
	http.SetCookie(c.Writer, &http.Cookie{Name: postPassCookie, Value: s.auth.Nonce("postpass|"+pw, 0, ""), Path: "/",
		MaxAge: 10 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode})
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

// documentTitle is wp_get_document_title() with Twenty Sixteen's title-tag support.
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
	}
	return "Archives", "Archives"
}

var _ = fmt.Sprintf
var _ template.HTML
