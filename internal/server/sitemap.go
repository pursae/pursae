package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Core sitemaps (wp-includes/sitemaps): /wp-sitemap.xml lists one sitemap per provider subtype
// and page, /wp-sitemap-{provider}-{subtype}-{n}.xml (users: /wp-sitemap-users-{n}.xml) holds up
// to sitemapMaxURLs URLs. Providers in WordPress order: posts (every public, viewable post type
// but attachments), taxonomies (every public, viewable taxonomy, empty terms hidden) and users
// (authors of published posts, unless account enumeration is blocked). Pursae SEO's sitemap
// module replaces all of this (seogoIntercept redirects wp-sitemap*.xml to its own index).

// sitemapMaxURLs is wp_sitemaps_get_max_urls(): URLs per sitemap page.
const sitemapMaxURLs = 2000

type sitemapEntry struct {
	Loc string
	Mod time.Time
}

// sitemapPostTypes are WP_Sitemaps_Posts::get_object_subtypes(): the public, registered post types
// without attachments, in registration order. content_types keeps rows of types whose plugin is
// off, so a row is not enough: posts and pages always, products while Pursae Commerce is on, and
// the routed public Pursae Custom Fields types (siteCPT).
func (s *Server) sitemapPostTypes(ctx context.Context) []string {
	var out []string
	rows, err := s.store.Pool.Query(ctx, `SELECT slug FROM content_types WHERE public AND slug <> 'attachment' ORDER BY id`)
	if err != nil {
		return []string{"post", "page"}
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		if rows.Scan(&slug) != nil {
			continue
		}
		switch {
		case slug == "post" || slug == "page":
		case slug == "product":
			if !s.gooOn() {
				continue
			}
		case siteCPT(slug) == nil:
			continue
		}
		out = append(out, slug)
	}
	return out
}

// sitemapTaxonomies are WP_Sitemaps_Taxonomies::get_object_subtypes(): the public, registered
// taxonomies (core ones, Pursae Commerce's while it is on, public Pursae Custom Fields ones).
func (s *Server) sitemapTaxonomies(ctx context.Context) []string {
	var out []string
	rows, err := s.store.Pool.Query(ctx, `SELECT slug FROM taxonomies WHERE public ORDER BY id`)
	if err != nil {
		return []string{"category", "post_tag"}
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		if rows.Scan(&slug) != nil {
			continue
		}
		if !s.sitemapTaxonomyRegistered(slug) {
			continue
		}
		out = append(out, slug)
	}
	return out
}

func (s *Server) sitemapTaxonomyRegistered(tax string) bool {
	switch {
	case tax == "category" || tax == "post_tag" || tax == "post_format":
		return true
	case s.sitemapGooTaxonomy(tax):
		return s.gooOn()
	}
	acfgoRegistry.RLock()
	tx := acfgoRegistry.taxes[tax]
	acfgoRegistry.RUnlock()
	return tx != nil && tx.Public
}

func (s *Server) sitemapGooTaxonomy(tax string) bool {
	return tax == "product_cat" || tax == "product_tag" || strings.HasPrefix(tax, "pa_")
}

// sitemapPostWhere is the posts query of the posts provider: published, no password, the type.
// QueryFilter (Golylang) narrows it to the request's language like QueryPosts does.
func sitemapPostWhere(ctx context.Context, typ string, args *[]any) string {
	add := func(v any) string {
		*args = append(*args, v)
		return "$" + strconv.Itoa(len(*args))
	}
	where := "type = " + add(typ) + " AND status = 'publish' AND coalesce(password, '') = ''"
	if store.QueryFilter != nil {
		if c := store.QueryFilter(ctx, "post", add); c != "" {
			where += " AND " + c
		}
	}
	return where
}

func pagesOf(n int) int { return int(math.Ceil(float64(n) / sitemapMaxURLs)) }

// sitemapPages is get_max_num_pages() of one provider subtype.
func (s *Server) sitemapPages(ctx context.Context, provider, subtype string) int {
	switch provider {
	case "posts":
		var args []any
		var n int
		if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE `+sitemapPostWhere(ctx, subtype, &args), args...).Scan(&n); err != nil {
			return 0
		}
		if subtype == "page" && n == 0 && s.settings.String("show_on_front") != "page" {
			n = 1 // the home page leads the pages sitemap
		}
		return pagesOf(n)
	case "taxonomies":
		_, n, err := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: subtype, HideEmpty: true, PerPage: 1, Page: 1})
		if err != nil {
			return 0
		}
		return pagesOf(n)
	case "users":
		if s.asBlockAuthorSitemap() { // AdminSecGo: user enumeration
			return 0
		}
		var n int
		if err := s.store.Pool.QueryRow(ctx, `SELECT count(DISTINCT p.author_id) FROM posts p
			WHERE p.type = ANY($1) AND p.status = 'publish'`, s.sitemapPostTypes(ctx)).Scan(&n); err != nil {
			return 0
		}
		return pagesOf(n)
	}
	return 0
}

// sitemapURLs is get_url_list() of one provider subtype and page.
func (s *Server) sitemapURLs(ctx context.Context, provider, subtype string, page int) []sitemapEntry {
	lb := s.links(ctx)
	offset := (page - 1) * sitemapMaxURLs
	var out []sitemapEntry
	switch provider {
	case "posts":
		var args []any
		where := sitemapPostWhere(ctx, subtype, &args)
		args = append(args, sitemapMaxURLs, offset)
		rows, err := s.store.Pool.Query(ctx, `SELECT id FROM posts WHERE `+where+` ORDER BY id
			LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
		if err != nil {
			return nil
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		// The home page leads the first pages sitemap when the front page shows the latest posts.
		if subtype == "page" && page == 1 && s.settings.String("show_on_front") != "page" {
			out = append(out, sitemapEntry{Loc: lb.Front()})
		}
		if len(ids) == 0 {
			return out
		}
		// Only links and dates are printed: up to 2000 posts per page are loaded without their content.
		// Memberships "hide completely": restricted entries are not listed for the (anonymous) public.
		posts, _, _ := s.store.QueryPosts(ctx, store.PostQuery{IDs: ids, Types: []string{subtype}, Statuses: []string{"publish"}, PerPage: -1, OrderBy: "id", Order: "asc", NoContent: true, NoTotal: true,
			ExcludeID: s.gooPublicExclude(ctx, []string{subtype})})
		for _, p := range posts {
			if p.Password != "" {
				continue
			}
			loc := ""
			if subtype == "product" {
				loc = s.gooPermalink(ctx, &gooProduct{ID: p.ID, Post: p, Slug: p.Slug, Status: p.Status, Type: "simple"})
			} else {
				loc = lb.Permalink(p)
			}
			out = append(out, sitemapEntry{Loc: loc, Mod: p.ModifiedAt})
		}
	case "taxonomies":
		terms, _, _ := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: subtype, HideEmpty: true, OrderBy: "id", PerPage: sitemapMaxURLs, Page: page})
		for _, t := range terms {
			if s.sitemapGooTaxonomy(subtype) {
				out = append(out, sitemapEntry{Loc: s.gooTermLink(ctx, t)})
			} else {
				out = append(out, sitemapEntry{Loc: lb.TermLink(t)})
			}
		}
	case "users":
		if s.asBlockAuthorSitemap() { // AdminSecGo: user enumeration
			return nil
		}
		rows, err := s.store.Pool.Query(ctx, `SELECT u.id, u.nicename FROM users u
			WHERE EXISTS (SELECT 1 FROM posts p WHERE p.author_id = u.id AND p.type = ANY($1) AND p.status = 'publish')
			ORDER BY u.id LIMIT $2 OFFSET $3`, s.sitemapPostTypes(ctx), sitemapMaxURLs, offset)
		if err != nil {
			return nil
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var nicename string
			if rows.Scan(&id, &nicename) == nil {
				out = append(out, sitemapEntry{Loc: lb.AuthorLink(id, nicename)})
			}
		}
	}
	return out
}

// sitemapIndexNames are the sitemap names (without wp-sitemap- and .xml) the index lists.
func (s *Server) sitemapIndexNames(ctx context.Context) []string {
	var names []string
	add := func(provider, subtype string) {
		pages := s.sitemapPages(ctx, provider, subtype)
		for n := 1; n <= pages; n++ {
			if subtype == "" {
				names = append(names, provider+"-"+strconv.Itoa(n))
			} else {
				names = append(names, provider+"-"+subtype+"-"+strconv.Itoa(n))
			}
		}
	}
	for _, t := range s.sitemapPostTypes(ctx) {
		add("posts", t)
	}
	for _, t := range s.sitemapTaxonomies(ctx) {
		add("taxonomies", t)
	}
	add("users", "")
	return names
}

// sitemapIndex is /wp-sitemap.xml.
func (s *Server) sitemapIndex(c *gin.Context) {
	if !s.settings.Bool("blog_public") {
		c.Status(http.StatusNotFound)
		return
	}
	ctx := c.Request.Context()
	lb := s.links(ctx)
	var sb strings.Builder
	sb.WriteString(xml.Header + `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	if r := pllFrom(c); r != nil && r.Front && s.golylangOn() && s.pllOpts().ForceLang <= 1 && strings.Trim(c.Request.URL.Path, "/") == "wp-sitemap.xml" && c.Query("lang") == "" {
		// Golylang: the root index (the one robots.txt names) lists every language's sitemaps, like
		// Polylang's multilingual sitemaps provider; /fr/wp-sitemap.xml keeps to French. With a subdomain
		// or a domain per language each host lists only its own language (PLL_Sitemaps_Domain).
		o := s.pllOpts()
		for _, l := range s.pllActiveLanguages(ctx) {
			lctx := context.WithValue(ctx, pllCtxKey{}, &pllReq{srv: s, Lang: l, Filter: true, Front: true, Path: r.Path})
			for _, name := range s.sitemapIndexNames(lctx) {
				fmt.Fprintf(&sb, "<sitemap><loc>%s</loc></sitemap>", html.EscapeString(s.pllConvert(ctx, lb.Home, lb.HomeURL("/wp-sitemap-"+name+".xml"), l, "home", o)))
			}
		}
	} else {
		for _, name := range s.sitemapIndexNames(ctx) {
			fmt.Fprintf(&sb, "<sitemap><loc>%s</loc></sitemap>", html.EscapeString(s.golylangURL(c, lb.HomeURL("/wp-sitemap-"+name+".xml")))) // Golylang: per language
		}
	}
	sb.WriteString("</sitemapindex>\n")
	c.Data(http.StatusOK, "application/xml; charset=UTF-8", []byte(sb.String()))
}

// parseSitemapName splits "posts-post-2", "taxonomies-product_cat-1" or "users-1" (an omitted
// page is page 1, like the wp-sitemap-{name}.xml rewrite rules).
func parseSitemapName(name string) (provider, subtype string, page int, ok bool) {
	provider, rest, _ := strings.Cut(name, "-")
	switch provider {
	case "posts", "taxonomies", "users":
	default:
		return "", "", 0, false
	}
	page = 1
	if i := strings.LastIndex(rest, "-"); i >= 0 || provider == "users" {
		num := rest
		if i >= 0 {
			num = rest[i+1:]
		}
		if n, err := strconv.Atoi(num); err == nil {
			if n < 1 {
				return "", "", 0, false
			}
			page = n
			if i >= 0 {
				rest = rest[:i]
			} else {
				rest = ""
			}
		}
	}
	subtype = rest
	if (provider == "users") != (subtype == "") {
		return "", "", 0, false
	}
	return provider, subtype, page, true
}

// sitemapPart serves /wp-sitemap-{name}.xml; an unknown subtype, an empty list or a page past the
// last is a 404 like WP_Sitemaps::render_sitemaps().
func (s *Server) sitemapPart(c *gin.Context, name string) {
	if !s.settings.Bool("blog_public") {
		c.Status(http.StatusNotFound)
		return
	}
	ctx := c.Request.Context()
	provider, subtype, page, ok := parseSitemapName(name)
	if ok {
		switch provider {
		case "posts":
			ok = slices.Contains(s.sitemapPostTypes(ctx), subtype)
		case "taxonomies":
			ok = slices.Contains(s.sitemapTaxonomies(ctx), subtype)
		}
	}
	var urls []sitemapEntry
	if ok {
		urls = s.sitemapURLs(ctx, provider, subtype, page)
	}
	if len(urls) == 0 {
		c.Status(http.StatusNotFound)
		return
	}
	var sb strings.Builder
	sb.WriteString(xml.Header + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, u := range urls {
		fmt.Fprintf(&sb, "<url><loc>%s</loc>", html.EscapeString(u.Loc))
		if !u.Mod.IsZero() {
			fmt.Fprintf(&sb, "<lastmod>%s</lastmod>", u.Mod.UTC().Format(time.RFC3339))
		}
		sb.WriteString("</url>")
	}
	sb.WriteString("</urlset>\n")
	c.Data(http.StatusOK, "application/xml; charset=UTF-8", []byte(sb.String()))
}
