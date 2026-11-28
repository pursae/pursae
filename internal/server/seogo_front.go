package server

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
)

// Front-end requests Pursae SEO answers itself: redirections, robots.txt, the XML sitemaps (which
// replace core's wp-sitemap.xml the way Yoast does), the IndexNow key file; plus the 404 monitor
// and the archives Search Appearance switches off.

// seogoIntercept runs first for every public GET/HEAD request; true means it answered.
func (s *Server) seogoIntercept(c *gin.Context, path string) bool {
	if !s.seogoOn() {
		return false
	}
	o := s.seogoOpts()
	if o.Modules["redirections"] && s.seogoRedirect(c) {
		return true
	}
	switch {
	case path == "robots.txt":
		s.seogoRobotsTxt(c)
		return true
	case o.IndexNowKey != "" && path == o.IndexNowKey+".txt" && o.Modules["indexnow"]:
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(o.IndexNowKey))
		return true
	}
	if !o.Modules["sitemap"] {
		return false
	}
	// The sitemaps list every language, so /fr/sitemap_index.xml is the same index (Pursae Multi Language).
	path = s.golylangSitePath(c, path)
	switch {
	case path == "sitemap_index.xml":
		s.seogoSitemapIndex(c)
		return true
	case path == "main-sitemap.xsl":
		c.Header("Cache-Control", "public, max-age=86400")
		c.Data(http.StatusOK, "text/xsl; charset=utf-8", []byte(seogoXSL))
		return true
	case path == "sitemap.xml" || path == "wp-sitemap.xml" || (strings.HasPrefix(path, "wp-sitemap") && strings.HasSuffix(path, ".xml")):
		c.Redirect(http.StatusMovedPermanently, s.links(c).HomeURL("/sitemap_index.xml"))
		return true
	case strings.HasSuffix(path, "-sitemap.xml") || seogoSitemapPagedRe.MatchString(path):
		return s.seogoSitemap(c, path)
	}
	return false
}

var seogoSitemapPagedRe = regexp.MustCompile(`^([a-z0-9_]+)-sitemap(\d+)\.xml$`)

// --- robots.txt --------------------------------------------------------------------------------

func (s *Server) seogoRobotsTxt(c *gin.Context) {
	o := s.seogoOpts()
	body := strings.TrimSpace(o.RobotsTxt)
	if body == "" {
		body = s.seogoRobotsDefault(c)
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(body+"\n"))
}

func (s *Server) seogoRobotsDefault(ctx context.Context) string {
	lb := s.links(ctx)
	if !s.settings.Bool("blog_public") {
		return "User-agent: *\nDisallow: /"
	}
	out := "User-agent: *\nDisallow: /wp-admin/\nAllow: /wp-admin/admin-ajax"
	if s.seogoOpts().Modules["sitemap"] {
		out += "\n\nSitemap: " + s.seogoHostURL(ctx, lb.HomeURL("/sitemap_index.xml")) // this host's index
	}
	return out
}

// --- XML sitemaps ------------------------------------------------------------------------------
//
// One index lists every sitemap: one per public post type (products and Pursae Custom Fields types
// included, the way core's wp-sitemap.xml does), per public taxonomy and the authors. With Pursae
// Multi Language in directory or query mode the sitemaps list every language's objects with their
// own URLs and hreflang alternates (Polylang's behaviour with Yoast); with a subdomain or a domain
// per language each host's sitemaps keep to that host's language. A sitemap page is
// built from one LIMIT/OFFSET query; the index only counts.

type seogoURL struct {
	Loc    string
	Mod    time.Time
	Images []string
	Alts   []seogoAlt // hreflang alternates (Pursae Multi Language)
}

type seogoAlt struct{ Lang, URL string }

// seogoAllLanguages reports whether one sitemap lists every language: Pursae Multi Language in
// directory or query mode. With a subdomain or a domain per language (force_lang 2/3) each host keeps
// to its own language, like Polylang filtering home_url per domain, since a sitemap may not list
// URLs of another host.
func (s *Server) seogoAllLanguages() bool {
	return s.golylangOn() && s.pllOpts().ForceLang <= 1
}

// seogoSitemapCtx is the context sitemaps query with: no language filter when one sitemap lists
// every language, else the request language.
func (s *Server) seogoSitemapCtx(ctx context.Context) context.Context {
	if s.golylangOn() && !s.seogoAllLanguages() {
		return ctx
	}
	return gooPllUnfiltered(ctx)
}

// seogoSitemapTypes are the sitemaps of the index, in order: post types, taxonomies, authors.
func (s *Server) seogoSitemapTypes(ctx context.Context) []string {
	o := s.seogoOpts()
	cand := append([]string{}, s.sitemapPostTypes(ctx)...)
	for _, t := range s.sitemapTaxonomies(ctx) {
		if t != "post_format" {
			cand = append(cand, t)
		}
	}
	if !s.asBlockAuthorSitemap() { // the core's user enumeration block covers the author sitemap too
		cand = append(cand, "author")
	}
	var out []string
	for _, t := range cand {
		if (t == "news" && o.Modules["news_sitemap"]) || (t == "video" && o.Modules["video_sitemap"]) {
			continue // /news-sitemap.xml and /video-sitemap.xml are the News and Video sitemaps then
		}
		if to := o.seogoType(t); to.Sitemap && !to.Noindex && !to.Disabled {
			out = append(out, t)
		}
	}
	return out
}

// seogoSitemapKind is how a sitemap type is listed: "post" (a post type), "term" or "author".
func (s *Server) seogoSitemapKind(ctx context.Context, typ string) string {
	if typ == "author" {
		return "author"
	}
	if slices.Contains(s.sitemapPostTypes(ctx), typ) {
		return "post"
	}
	return "term"
}

func idSet(list string) map[int64]bool {
	out := map[int64]bool{}
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil {
			out[n] = true
		}
	}
	return out
}

// seogoIndexable reports whether a published post belongs in sitemaps (no noindex, no password,
// not excluded, no canonical elsewhere). loc is the post's URL.
func (s *Server) seogoIndexable(o *seogoOptions, p *store.Post, loc string, excluded map[int64]bool) bool {
	if p.Status != "publish" || p.Password != "" || excluded[p.ID] {
		return false
	}
	switch seogoMeta(p.Meta, "noindex") {
	case "noindex":
		return false
	case "":
		if o.seogoType(p.Type).Noindex {
			return false
		}
	}
	if can := seogoMeta(p.Meta, "canonical"); can != "" && can != loc {
		return false
	}
	return true
}

// seogoStorePrivatePages are the Cart, Checkout and My account pages and their translations: never
// in a sitemap (WooCommerce keeps them out with wc_page_noindex).
func (s *Server) seogoStorePrivatePages(ctx context.Context) map[int64]bool {
	out := map[int64]bool{}
	if !s.gooOn() {
		return out
	}
	for _, key := range []string{"cart", "checkout", "myaccount"} {
		id := s.gooPageID(key)
		if id <= 0 {
			continue
		}
		out[id] = true
		if s.golylangOn() {
			for _, t := range s.pllTranslations(ctx, pllPost, id) {
				out[t] = true
			}
		}
	}
	return out
}

// seogoPostLoc is the URL of a post in a sitemap.
func (s *Server) seogoPostLoc(ctx context.Context, lb *links.Builder, p *store.Post) string {
	if p.Type == "product" {
		return s.gooPermalink(ctx, &gooProduct{ID: p.ID, Post: p, Slug: p.Slug, Status: p.Status, Type: "simple"})
	}
	return lb.Permalink(p)
}

// seogoTermLoc is the URL of a term in a sitemap.
func (s *Server) seogoTermLoc(ctx context.Context, t *store.Term) string {
	if s.sitemapGooTaxonomy(t.Taxonomy) {
		return s.gooTermLink(ctx, t)
	}
	return s.links(ctx).TermLink(t)
}

var seogoAllImgRe = regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["']`)

// seogoPostImages are the image entries of a post: its featured image (from the page's batch) and
// the images of its content.
func (s *Server) seogoPostImages(ctx context.Context, p *store.Post, media map[int64]*store.Attachment) []string {
	var out []string
	seen := map[string]bool{}
	// The sitemap is public: the featured image and content images of a members-only post (or inside
	// members-only shortcodes) are left out like the content itself.
	p = s.seogoMembersPost(ctx, nil, p)
	if a := media[p.FeaturedMediaID]; a != nil && strings.HasPrefix(a.Mime, "image/") {
		u := s.uploadURL(ctx, a.FilePath)
		out = append(out, u)
		seen[u] = true
	}
	for _, m := range seogoAllImgRe.FindAllStringSubmatch(p.Content, -1) {
		u := html.UnescapeString(m[1])
		if strings.HasPrefix(u, "/") {
			u = s.links(ctx).HomeURL(u)
		}
		if !seen[u] && !strings.HasPrefix(u, "data:") {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// seogoTranslationGroups maps each object to its translations (language => id, itself included),
// for the objects that have at least one; nil while Pursae Multi Language is off.
func (s *Server) seogoTranslationGroups(ctx context.Context, k pllKind, ids []int64) map[int64]map[string]int64 {
	if !s.seogoAllLanguages() || len(ids) == 0 || len(s.pllActiveLanguages(ctx)) < 2 {
		return nil // a host per language: the other hosts' URLs stay out of this sitemap
	}
	tbl, col := k.table()
	rows, err := s.store.Pool.Query(ctx, `SELECT a.`+col+`, b.`+col+`, b.lang FROM `+tbl+` a JOIN `+tbl+` b ON b.grp = a.grp
		WHERE a.`+col+` = ANY($1)`, ids)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64]map[string]int64{}
	for rows.Next() {
		var id, other int64
		var lang string
		if rows.Scan(&id, &other, &lang) != nil {
			continue
		}
		if out[id] == nil {
			out[id] = map[string]int64{}
		}
		out[id][lang] = other
	}
	for id, g := range out {
		if len(g) < 2 {
			delete(out, id)
		}
	}
	return out
}

// seogoAltsFor turns a translation group into hreflang alternates; locs has the URL of every
// indexable member. Fewer than two languages give none.
func (s *Server) seogoAltsFor(ctx context.Context, group map[string]int64, locs map[int64]string) []seogoAlt {
	var out []seogoAlt
	for _, l := range s.pllActiveLanguages(ctx) {
		if id, ok := group[l.Slug]; ok && locs[id] != "" {
			out = append(out, seogoAlt{Lang: s.pllHreflang(ctx, l), URL: locs[id]})
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

// seogoSitemapChunks is the index entry data of one type: the last modification of each page.
func (s *Server) seogoSitemapChunks(ctx context.Context, typ string, per int) []time.Time {
	ctx = s.seogoSitemapCtx(ctx)
	var q string
	switch s.seogoSitemapKind(ctx, typ) {
	case "author":
		var n int
		var mod *time.Time
		if s.store.Pool.QueryRow(ctx, `SELECT count(DISTINCT author_id), max(modified_at) FROM posts WHERE type = 'post' AND status = 'publish'`).Scan(&n, &mod) != nil || n == 0 {
			return nil
		}
		out := make([]time.Time, (n+per-1)/per)
		if mod != nil {
			for i := range out {
				out[i] = *mod
			}
		}
		return out
	case "post":
		q = `SELECT ((rn - 1) / $2)::int, max(modified_at) FROM (SELECT modified_at, row_number() OVER (ORDER BY id) AS rn
			FROM posts WHERE type = $1 AND status = 'publish') x GROUP BY 1 ORDER BY 1`
	default:
		q = `SELECT ((rn - 1) / $2)::int, max(m) FROM (SELECT row_number() OVER (ORDER BY t.id) AS rn,
			(SELECT max(p.modified_at) FROM post_terms pt JOIN posts p ON p.id = pt.post_id WHERE pt.term_id = t.id AND p.status = 'publish') AS m
			FROM terms t WHERE t.taxonomy = $1 AND t.post_count > 0) x GROUP BY 1 ORDER BY 1`
	}
	rows, err := s.store.Pool.Query(ctx, q, typ, per)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var n int
		var mod *time.Time
		if rows.Scan(&n, &mod) != nil {
			continue
		}
		for len(out) < n {
			out = append(out, time.Time{})
		}
		if mod != nil {
			out = append(out, *mod)
		} else {
			out = append(out, time.Time{})
		}
	}
	if typ == "page" && len(out) == 0 && s.settings.String("show_on_front") != "page" {
		out = append(out, time.Time{}) // the home page alone
	}
	return out
}

// seogoSitemapURLs lists the URLs of one page of a sitemap type (page 1 is the first).
func (s *Server) seogoSitemapURLs(reqCtx context.Context, typ string, page int) []seogoURL {
	ctx := s.seogoSitemapCtx(reqCtx)
	o := s.seogoOpts()
	lb := s.links(reqCtx)
	per := o.SitemapPerPage
	var out []seogoURL
	switch s.seogoSitemapKind(ctx, typ) {
	case "post":
		posts, _, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{typ}, Statuses: []string{"publish"}, PerPage: per, Page: page, OrderBy: "id", Order: "asc", NoTotal: true})
		excluded := idSet(o.SitemapExclude)
		for _, id := range s.gooPublicExclude(ctx, []string{typ}) { // Memberships "hide completely"
			excluded[id] = true
		}
		if typ == "page" {
			for id := range s.seogoStorePrivatePages(ctx) {
				excluded[id] = true
			}
		}
		front := int64(0)
		if s.settings.String("show_on_front") == "page" {
			front = int64(s.settings.Int("page_on_front"))
		}
		if typ == "page" && front == 0 && page == 1 { // the home page leads the page sitemap, like Yoast
			if cur := s.pllCurrent(reqCtx); s.golylangOn() && !s.seogoAllLanguages() && cur != nil {
				out = append(out, seogoURL{Loc: s.pllHomeURL(reqCtx, cur)}) // this host's language only
			} else if s.golylangOn() && len(s.pllActiveLanguages(reqCtx)) > 0 {
				var homes []seogoAlt
				for _, l := range s.pllActiveLanguages(reqCtx) {
					homes = append(homes, seogoAlt{Lang: s.pllHreflang(reqCtx, l), URL: s.pllHomeURL(reqCtx, l)})
				}
				for _, h := range homes {
					u := seogoURL{Loc: h.URL}
					if len(homes) > 1 {
						u.Alts = homes
					}
					out = append(out, u)
				}
			} else {
				out = append(out, seogoURL{Loc: lb.HomeURL("/")})
			}
		}
		var keep []*store.Post
		locs := map[int64]string{}
		var mediaIDs []int64
		for _, p := range posts {
			loc := s.seogoPostLoc(reqCtx, lb, p)
			if p.ID == front && !s.golylangOn() {
				loc = lb.HomeURL("/")
			}
			if !s.seogoIndexable(o, p, loc, excluded) {
				continue
			}
			keep = append(keep, p)
			locs[p.ID] = loc
			if p.FeaturedMediaID > 0 {
				mediaIDs = append(mediaIDs, p.FeaturedMediaID)
			}
		}
		var media map[int64]*store.Attachment
		if o.SitemapImages && len(mediaIDs) > 0 {
			media, _ = s.store.GetAttachments(ctx, mediaIDs)
		}
		ids := make([]int64, 0, len(keep))
		for _, p := range keep {
			ids = append(ids, p.ID)
		}
		groups := s.seogoTranslationGroups(ctx, pllPost, ids)
		if len(groups) > 0 { // translations on other sitemap pages
			var missing []int64
			for _, g := range groups {
				for _, id := range g {
					if _, ok := locs[id]; !ok {
						missing = append(missing, id)
					}
				}
			}
			if len(missing) > 0 {
				others, _, _ := s.store.QueryPosts(ctx, store.PostQuery{IDs: missing, Statuses: []string{"publish"}, PerPage: -1, OrderBy: "id", Order: "asc", NoTotal: true})
				for _, p := range others {
					if loc := s.seogoPostLoc(reqCtx, lb, p); s.seogoIndexable(o, p, loc, excluded) {
						locs[p.ID] = loc
					}
				}
			}
		}
		for _, p := range keep {
			u := seogoURL{Loc: locs[p.ID], Mod: p.ModifiedAt}
			if o.SitemapImages {
				u.Images = s.seogoPostImages(reqCtx, p, media)
			}
			if g := groups[p.ID]; g != nil {
				u.Alts = s.seogoAltsFor(reqCtx, g, locs)
			}
			out = append(out, u)
		}
	case "term":
		terms, _, _ := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: typ, HideEmpty: true, OrderBy: "id", PerPage: per, Page: page})
		excluded := idSet(o.SitemapExcludeT)
		var keep []*store.Term
		locs := map[int64]string{}
		for _, t := range terms {
			if excluded[t.ID] || seogoMeta(t.Meta, "noindex") == "noindex" {
				continue
			}
			keep = append(keep, t)
			locs[t.ID] = s.seogoTermLoc(reqCtx, t)
		}
		ids := make([]int64, 0, len(keep))
		for _, t := range keep {
			ids = append(ids, t.ID)
		}
		mods := map[int64]time.Time{}
		if len(ids) > 0 {
			if rows, err := s.store.Pool.Query(ctx, `SELECT pt.term_id, max(p.modified_at) FROM post_terms pt JOIN posts p ON p.id = pt.post_id
				WHERE pt.term_id = ANY($1) AND p.status = 'publish' GROUP BY pt.term_id`, ids); err == nil {
				for rows.Next() {
					var id int64
					var mod time.Time
					if rows.Scan(&id, &mod) == nil {
						mods[id] = mod
					}
				}
				rows.Close()
			}
		}
		groups := s.seogoTranslationGroups(ctx, pllTerm, ids)
		if len(groups) > 0 {
			var missing []int64
			for _, g := range groups {
				for _, id := range g {
					if _, ok := locs[id]; !ok {
						missing = append(missing, id)
					}
				}
			}
			if len(missing) > 0 {
				others, _, _ := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: typ, HideEmpty: true, IDs: missing, PerPage: len(missing), Page: 1})
				for _, t := range others {
					if !excluded[t.ID] && seogoMeta(t.Meta, "noindex") != "noindex" {
						locs[t.ID] = s.seogoTermLoc(reqCtx, t)
					}
				}
			}
		}
		for _, t := range keep {
			u := seogoURL{Loc: locs[t.ID], Mod: mods[t.ID]}
			if g := groups[t.ID]; g != nil {
				u.Alts = s.seogoAltsFor(reqCtx, g, locs)
			}
			out = append(out, u)
		}
	case "author":
		if s.asBlockAuthorSitemap() {
			return nil
		}
		rows, err := s.store.Pool.Query(ctx, `SELECT u.id, u.nicename, max(p.modified_at) FROM users u JOIN posts p ON p.author_id = u.id
			WHERE p.type = 'post' AND p.status = 'publish' GROUP BY u.id, u.nicename ORDER BY u.id LIMIT $1 OFFSET $2`, per, (page-1)*per)
		if err == nil {
			for rows.Next() {
				var id int64
				var nicename string
				var mod time.Time
				if rows.Scan(&id, &nicename, &mod) == nil {
					out = append(out, seogoURL{Loc: lb.AuthorLink(id, nicename), Mod: mod})
				}
			}
			rows.Close()
		}
	}
	return out
}

func (s *Server) seogoXMLHead(ctx context.Context) string {
	return xml.Header + `<?xml-stylesheet type="text/xsl" href="` + xmlEsc(s.seogoHostURL(ctx, s.links(ctx).HomeURL("/main-sitemap.xsl"))) + `"?>` + "\n"
}

// seogoHostURL moves a sitemap file URL to the request's host while each language has its own
// subdomain or domain (an index and its stylesheet stay on the host that serves them).
func (s *Server) seogoHostURL(ctx context.Context, link string) string {
	if !s.golylangOn() || s.seogoAllLanguages() {
		return link
	}
	l := s.pllCurrent(ctx)
	if l == nil {
		return link
	}
	return s.pllConvert(ctx, s.links(ctx).Home, link, l, "home", s.pllOpts())
}

func (s *Server) seogoSitemapIndex(c *gin.Context) {
	if !s.settings.Bool("blog_public") {
		c.Status(http.StatusNotFound)
		return
	}
	ctx := c.Request.Context()
	lb := s.links(ctx)
	o := s.seogoOpts()
	var sb strings.Builder
	sb.WriteString(s.seogoXMLHead(ctx) + `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, typ := range s.seogoSitemapTypes(ctx) {
		for i, last := range s.seogoSitemapChunks(ctx, typ, o.SitemapPerPage) {
			name := typ + "-sitemap.xml"
			if i > 0 {
				name = typ + "-sitemap" + strconv.Itoa(i+1) + ".xml"
			}
			fmt.Fprintf(&sb, "\t<sitemap>\n\t\t<loc>%s</loc>\n", xmlEsc(s.seogoHostURL(ctx, lb.HomeURL("/"+name))))
			if !last.IsZero() {
				fmt.Fprintf(&sb, "\t\t<lastmod>%s</lastmod>\n", last.UTC().Format(time.RFC3339))
			}
			sb.WriteString("\t</sitemap>\n")
		}
	}
	if o.Modules["news_sitemap"] {
		fmt.Fprintf(&sb, "\t<sitemap>\n\t\t<loc>%s</loc>\n\t</sitemap>\n", xmlEsc(s.seogoHostURL(ctx, lb.HomeURL("/news-sitemap.xml"))))
	}
	if o.Modules["video_sitemap"] {
		fmt.Fprintf(&sb, "\t<sitemap>\n\t\t<loc>%s</loc>\n\t</sitemap>\n", xmlEsc(s.seogoHostURL(ctx, lb.HomeURL("/video-sitemap.xml"))))
	}
	sb.WriteString("</sitemapindex>\n<!-- XML Sitemap generated by Pursae SEO -->\n")
	c.Header("X-Robots-Tag", "noindex, follow")
	c.Data(http.StatusOK, "text/xml; charset=UTF-8", []byte(sb.String()))
}

// seogoSitemap serves <type>-sitemap[N].xml, news-sitemap.xml and video-sitemap.xml.
func (s *Server) seogoSitemap(c *gin.Context, path string) bool {
	if !s.settings.Bool("blog_public") {
		return false
	}
	ctx := c.Request.Context()
	o := s.seogoOpts()
	typ, page := strings.TrimSuffix(path, "-sitemap.xml"), 1
	if m := seogoSitemapPagedRe.FindStringSubmatch(path); m != nil {
		typ = m[1]
		page, _ = strconv.Atoi(m[2])
	}
	if page == 1 && path == typ+"-sitemap.xml" { // a post type or taxonomy of the same name yields to these
		switch {
		case typ == "news" && o.Modules["news_sitemap"]:
			s.seogoNewsSitemap(c)
			return true
		case typ == "video" && o.Modules["video_sitemap"]:
			s.seogoVideoSitemap(c)
			return true
		}
	}
	if !slices.Contains(s.seogoSitemapTypes(ctx), typ) || page < 1 {
		return false
	}
	if page > len(s.seogoSitemapChunks(ctx, typ, o.SitemapPerPage)) {
		return false
	}
	urls := s.seogoSitemapURLs(ctx, typ, page) // a page whose URLs are all noindex stays an empty urlset
	var sb strings.Builder
	sb.WriteString(s.seogoXMLHead(ctx) + `<urlset xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:image="http://www.google.com/schemas/sitemap-image/1.1" xmlns:xhtml="http://www.w3.org/1999/xhtml" xsi:schemaLocation="http://www.sitemaps.org/schemas/sitemap/0.9 http://www.sitemaps.org/schemas/sitemap/0.9/sitemap.xsd http://www.google.com/schemas/sitemap-image/1.1 http://www.google.com/schemas/sitemap-image/1.1/sitemap-image.xsd" xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, u := range urls {
		fmt.Fprintf(&sb, "\t<url>\n\t\t<loc>%s</loc>\n", xmlEsc(u.Loc))
		if !u.Mod.IsZero() {
			fmt.Fprintf(&sb, "\t\t<lastmod>%s</lastmod>\n", u.Mod.UTC().Format(time.RFC3339))
		}
		for _, a := range u.Alts {
			fmt.Fprintf(&sb, "\t\t<xhtml:link rel=\"alternate\" hreflang=\"%s\" href=\"%s\" />\n", xmlEsc(a.Lang), xmlEsc(a.URL))
		}
		for _, img := range u.Images {
			fmt.Fprintf(&sb, "\t\t<image:image>\n\t\t\t<image:loc>%s</image:loc>\n\t\t</image:image>\n", xmlEsc(img))
		}
		sb.WriteString("\t</url>\n")
	}
	sb.WriteString("</urlset>\n<!-- XML Sitemap generated by Pursae SEO -->\n")
	c.Header("X-Robots-Tag", "noindex, follow")
	c.Data(http.StatusOK, "text/xml; charset=UTF-8", []byte(sb.String()))
	return true
}

// seogoPostLocale is the locale of a post: its Pursae Multi Language language, else the site's.
func (s *Server) seogoPostLocale(ctx context.Context, p *store.Post) string {
	if s.golylangOn() && s.pllTranslatedType(p.Type) {
		if l := s.pllLang(ctx, s.pllObjLang(ctx, pllPost, p.ID)); l != nil && l.Locale != "" {
			return l.Locale
		}
	}
	if l := s.settings.String("WPLANG"); l != "" {
		return l
	}
	return "en_US"
}

func (s *Server) seogoNewsSitemap(c *gin.Context) {
	reqCtx := c.Request.Context()
	ctx := s.seogoSitemapCtx(reqCtx)
	o := s.seogoOpts()
	lb := s.links(reqCtx)
	name := o.SitemapNewsTypes
	if name == "" {
		name = s.settings.String("blogname")
	}
	// Titles, links and dates only: the newest 1000 posts are loaded without their content.
	posts, _, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, PerPage: 1000, Page: 1, OrderBy: "date", Order: "desc", NoContent: true, NoTotal: true})
	excluded := idSet(o.SitemapExclude)
	for _, id := range s.gooPublicExclude(ctx, []string{"post"}) { // Memberships "hide completely"
		excluded[id] = true
	}
	var sb strings.Builder
	sb.WriteString(s.seogoXMLHead(ctx) + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:news="http://www.google.com/schemas/sitemap-news/0.9">` + "\n")
	cutoff := time.Now().Add(-48 * time.Hour)
	for _, p := range posts {
		loc := s.seogoPostLoc(reqCtx, lb, p)
		if p.PublishedAt == nil || p.PublishedAt.Before(cutoff) || !s.seogoIndexable(o, p, loc, excluded) {
			continue
		}
		locale := s.seogoPostLocale(ctx, p)
		fmt.Fprintf(&sb, "\t<url>\n\t\t<loc>%s</loc>\n\t\t<news:news>\n\t\t\t<news:publication>\n\t\t\t\t<news:name>%s</news:name>\n\t\t\t\t<news:language>%s</news:language>\n\t\t\t</news:publication>\n\t\t\t<news:publication_date>%s</news:publication_date>\n\t\t\t<news:title>%s</news:title>\n\t\t</news:news>\n\t</url>\n",
			xmlEsc(loc), xmlEsc(name), strings.SplitN(locale, "_", 2)[0], p.PublishedAt.UTC().Format(time.RFC3339), xmlEsc(p.Title))
	}
	sb.WriteString("</urlset>\n")
	c.Data(http.StatusOK, "text/xml; charset=UTF-8", []byte(sb.String()))
}

var (
	ytRe    = regexp.MustCompile(`(?i)(?:youtube\.com/(?:watch\?v=|embed/)|youtu\.be/)([A-Za-z0-9_-]{11})`)
	vimeoRe = regexp.MustCompile(`(?i)vimeo\.com/(?:video/)?(\d+)`)
	// videoFileRe finds self-hosted video files (<video src>, <source src>).
	videoFileRe = regexp.MustCompile(`(?i)<(?:video|source)\s[^>]*src=["']([^"']+\.(?:mp4|webm|ogv|m4v|mov)(?:\?[^"']*)?)["']`)
)

func (s *Server) seogoVideoSitemap(c *gin.Context) {
	reqCtx := c.Request.Context()
	ctx := s.seogoSitemapCtx(reqCtx)
	o := s.seogoOpts()
	lb := s.links(reqCtx)
	// Only posts that embed a video are loaded (matched in SQL first), at most 5000 of them, from
	// every sitemap post type.
	var posts []*store.Post
	var ids []int64
	types := s.sitemapPostTypes(ctx)
	if rows, err := s.store.Pool.Query(ctx, `SELECT id FROM posts WHERE type = ANY($1) AND status = 'publish'
		AND (content ILIKE '%youtu%' OR content ILIKE '%vimeo.com%' OR content ILIKE '%<video%') ORDER BY id LIMIT 5000`, types); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
	}
	if len(ids) > 0 {
		posts, _, _ = s.store.QueryPosts(ctx, store.PostQuery{IDs: ids, Types: types, Statuses: []string{"publish"}, PerPage: -1, OrderBy: "id", Order: "asc", NoTotal: true})
	}
	excluded := idSet(o.SitemapExclude)
	for _, id := range s.gooPublicExclude(ctx, types) { // Memberships "hide completely"
		excluded[id] = true
	}
	var sb strings.Builder
	sb.WriteString(s.seogoXMLHead(ctx) + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:video="http://www.google.com/schemas/sitemap-video/1.1">` + "\n")
	for _, p := range posts {
		// The sitemap is public: a members-only post loses its content (and with it its videos), and
		// members-only shortcode bodies stay out of the descriptions.
		p = s.seogoMembersPost(ctx, nil, p)
		if !strings.Contains(p.Content, "youtu") && !strings.Contains(p.Content, "vimeo.com") && !strings.Contains(strings.ToLower(p.Content), "<video") {
			continue
		}
		loc := s.seogoPostLoc(reqCtx, lb, p)
		if !s.seogoIndexable(o, p, loc, excluded) {
			continue
		}
		var vids []string
		seenVid := map[string]bool{}
		for _, m := range ytRe.FindAllStringSubmatch(p.Content, -1) {
			if seenVid["y"+m[1]] {
				continue
			}
			seenVid["y"+m[1]] = true
			vids = append(vids, fmt.Sprintf("\t\t<video:video>\n\t\t\t<video:thumbnail_loc>https://i.ytimg.com/vi/%s/hqdefault.jpg</video:thumbnail_loc>\n\t\t\t<video:title>%s</video:title>\n\t\t\t<video:description>%s</video:description>\n\t\t\t<video:player_loc>https://www.youtube.com/embed/%s</video:player_loc>\n\t\t</video:video>\n",
				m[1], xmlEsc(p.Title), xmlEsc(cut(plainText(p.Content), 2000)), m[1]))
		}
		thumb := ""
		if img := s.seogoAttachmentImage(ctx, p.FeaturedMediaID); img != nil {
			thumb = img.URL
		}
		for _, m := range vimeoRe.FindAllStringSubmatch(p.Content, -1) {
			if seenVid["v"+m[1]] {
				continue
			}
			seenVid["v"+m[1]] = true
			if thumb == "" {
				continue // thumbnail_loc is required: no featured image, no entry
			}
			vids = append(vids, fmt.Sprintf("\t\t<video:video>\n\t\t\t<video:thumbnail_loc>%s</video:thumbnail_loc>\n\t\t\t<video:title>%s</video:title>\n\t\t\t<video:description>%s</video:description>\n\t\t\t<video:player_loc>https://player.vimeo.com/video/%s</video:player_loc>\n\t\t</video:video>\n",
				xmlEsc(thumb), xmlEsc(p.Title), xmlEsc(cut(plainText(p.Content), 2000)), m[1]))
		}
		// Self-hosted files of core/video blocks (and <video> tags): content_loc, needs a thumbnail too.
		for _, m := range videoFileRe.FindAllStringSubmatch(p.Content, -1) {
			src := html.UnescapeString(m[1])
			if seenVid["f"+src] || thumb == "" {
				continue
			}
			seenVid["f"+src] = true
			vids = append(vids, fmt.Sprintf("\t\t<video:video>\n\t\t\t<video:thumbnail_loc>%s</video:thumbnail_loc>\n\t\t\t<video:title>%s</video:title>\n\t\t\t<video:description>%s</video:description>\n\t\t\t<video:content_loc>%s</video:content_loc>\n\t\t</video:video>\n",
				xmlEsc(thumb), xmlEsc(p.Title), xmlEsc(cut(plainText(p.Content), 2000)), xmlEsc(seogoAbsURL(lb.HomeURL("/"), src))))
		}
		if len(vids) > 0 {
			fmt.Fprintf(&sb, "\t<url>\n\t\t<loc>%s</loc>\n%s\t</url>\n", xmlEsc(loc), strings.Join(vids, ""))
		}
	}
	sb.WriteString("</urlset>\n")
	c.Data(http.StatusOK, "text/xml; charset=UTF-8", []byte(sb.String()))
}

// --- Redirections ------------------------------------------------------------------------------

type seogoRedirectRow struct {
	ID        int64
	Source    string
	Target    string
	Code      int
	MatchType string
	Status    string
	Category  string
	Origin    string
	Hits      int64
	LastHit   *time.Time
	CreatedAt time.Time
	re        *regexp.Regexp
}

// seogoRedirectRows loads the redirects (all of them, or only the active ones).
func (s *Server) seogoRedirectRows(ctx context.Context, activeOnly bool) ([]*seogoRedirectRow, error) {
	q := `SELECT id, source, target, code, match_type, status, category, origin, hits, last_hit_at, created_at FROM redirects`
	if activeOnly {
		q += ` WHERE status = 'active'`
	}
	rows, err := s.store.Pool.Query(ctx, q+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*seogoRedirectRow
	for rows.Next() {
		r := &seogoRedirectRow{}
		var code int16
		if err := rows.Scan(&r.ID, &r.Source, &r.Target, &code, &r.MatchType, &r.Status, &r.Category, &r.Origin, &r.Hits, &r.LastHit, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Code = int(code)
		if r.MatchType == "regex" {
			r.re, _ = regexp.Compile(r.Source)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// seogoRedirectCache keeps the active redirects in memory; any change clears it.
type seogoRedirectCache struct {
	rows   []*seogoRedirectRow
	loaded bool
}

// seogoRedirects is shared by every Server of the process (a theme preview sandbox is one), so it
// has its own lock: a Server's runtime lock would not keep two Servers apart.
var seogoRedirects = struct {
	sync.Mutex
	m map[*Server]*seogoRedirectCache
}{m: map[*Server]*seogoRedirectCache{}}

func (s *Server) seogoActiveRedirects(ctx context.Context) []*seogoRedirectRow {
	seogoRedirects.Lock()
	cache := seogoRedirects.m[s]
	seogoRedirects.Unlock()
	if cache != nil && cache.loaded {
		return cache.rows
	}
	rows, err := s.seogoRedirectRows(ctx, true)
	if err != nil {
		return nil
	}
	seogoRedirects.Lock()
	seogoRedirects.m[s] = &seogoRedirectCache{rows: rows, loaded: true}
	seogoRedirects.Unlock()
	return rows
}

func (s *Server) seogoRedirectsChanged() {
	seogoRedirects.Lock()
	delete(seogoRedirects.m, s)
	seogoRedirects.Unlock()
}

// normPath is a redirect source or request path without slashes at either end, decoded.
func normPath(p string) string {
	if u, err := url.Parse(p); err == nil && u.Path != "" && (u.Scheme != "" || strings.HasPrefix(p, "/")) {
		p = u.Path
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
	}
	return strings.Trim(p, "/")
}

// match reports whether a redirect applies to a request path (and the target with $n filled).
func (r *seogoRedirectRow) match(reqPath, reqWithQuery string) (string, bool) {
	src := normPath(r.Source)
	lp, lq := strings.ToLower(reqPath), strings.ToLower(reqWithQuery)
	ls := strings.ToLower(src)
	switch r.MatchType {
	case "regex":
		if r.re == nil {
			return "", false
		}
		for _, cand := range []string{"/" + reqWithQuery, reqWithQuery, "/" + reqPath, reqPath} {
			if m := r.re.FindStringSubmatchIndex(cand); m != nil {
				return string(r.re.ExpandString(nil, r.Target, cand, m)), true
			}
		}
		return "", false
	case "contains":
		return r.Target, ls != "" && strings.Contains(lq, ls)
	case "start":
		return r.Target, ls != "" && strings.HasPrefix(lq, ls)
	case "end":
		return r.Target, ls != "" && strings.HasSuffix(lq, ls)
	}
	return r.Target, ls == lp || ls == lq
}

// seogoRedirect answers a request a redirect matches.
func (s *Server) seogoRedirect(c *gin.Context) bool {
	reqPath := strings.Trim(c.Request.URL.Path, "/")
	withQuery := reqPath
	if c.Request.URL.RawQuery != "" {
		withQuery += "?" + c.Request.URL.RawQuery
	}
	for _, r := range s.seogoActiveRedirects(c.Request.Context()) {
		target, ok := r.match(reqPath, withQuery)
		if !ok {
			continue
		}
		s.asyncExec(`UPDATE redirects SET hits = hits + 1, last_hit_at = now() WHERE id = $1`, r.ID) // bounded queue (background.go)
		c.Header("X-Redirect-By", "Pursae SEO")
		c.Header("Cache-Control", "no-cache, must-revalidate, max-age=0")
		switch r.Code {
		case 410, 451:
			msg := map[int]string{410: "This content has been permanently removed.", 451: "This content is unavailable for legal reasons."}[r.Code]
			c.Data(r.Code, "text/html; charset=UTF-8", []byte(`<!DOCTYPE html><html lang="en-US"><head><meta charset="utf-8"><meta name="robots" content="noindex"><title>`+
				html.EscapeString(http.StatusText(r.Code)+" – "+s.settings.String("blogname"))+`</title></head><body><h1>`+http.StatusText(r.Code)+`</h1><p>`+msg+`</p></body></html>`))
		default:
			loc, ok := seogoSafeTarget(target)
			if !ok {
				// The site's own host is a valid destination even when Site URL is a loopback
				// address. Other private hosts stay refused.
				loc, ok = s.seogoSiteTarget(c, target)
			}
			if !ok {
				continue
			}
			if !strings.Contains(loc, "://") {
				loc = s.links(c).HomeURL(loc)
			}
			code := r.Code
			if code != 302 && code != 307 && code != 308 {
				code = 301
			}
			c.Redirect(code, loc)
		}
		return true
	}
	return false
}

// seogoAddRedirect stores a redirect (an existing source is updated).
func (s *Server) seogoAddRedirect(ctx context.Context, r *seogoRedirectRow) error {
	if r.MatchType == "" {
		r.MatchType = "exact"
	}
	if r.MatchType == "regex" {
		if _, err := regexp.Compile(r.Source); err != nil {
			return fmt.Errorf("invalid regular expression: %v", err)
		}
	} else {
		slash := strings.HasSuffix(r.Source, "/")
		r.Source = "/" + normPath(r.Source)
		if strings.Trim(r.Source, "/") == "" {
			return fmt.Errorf("the source URL is empty")
		}
		if slash && !strings.Contains(r.Source, "?") {
			r.Source += "/"
		}
	}
	if r.Code == 0 {
		r.Code = 301
	}
	if r.Status == "" {
		r.Status = "active"
	}
	if r.Origin == "" {
		r.Origin = "manual"
	}
	if r.Code != 410 && r.Code != 451 && strings.TrimSpace(r.Target) == "" {
		return fmt.Errorf("a destination URL is required for a %d redirect", r.Code)
	}
	var err error
	if r.ID > 0 {
		_, err = s.store.Pool.Exec(ctx, `UPDATE redirects SET source=$2, target=$3, code=$4, match_type=$5, status=$6, category=$7, updated_at=now() WHERE id=$1`,
			r.ID, r.Source, r.Target, r.Code, r.MatchType, r.Status, r.Category)
	} else {
		// RETURNING id: callers (the REST create route, MCP) answer with the new row's id.
		err = s.store.Pool.QueryRow(ctx, `INSERT INTO redirects (source, target, code, match_type, status, category, origin) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (source) DO UPDATE SET target=EXCLUDED.target, code=EXCLUDED.code, match_type=EXCLUDED.match_type, status=EXCLUDED.status, updated_at=now()
			RETURNING id`,
			r.Source, r.Target, r.Code, r.MatchType, r.Status, r.Category, r.Origin).Scan(&r.ID)
	}
	s.seogoRedirectsChanged()
	return err
}

// seogoAfterSave runs after a post is saved: the automatic redirect of a changed slug (Rank Math's
// "Auto Post Redirect", Yoast Premium's slug-change redirect) and IndexNow.
func (s *Server) seogoAfterSave(ctx context.Context, p *store.Post) {
	if !s.seogoOn() || p == nil {
		return
	}
	s.seogoOrphanCacheDrop()
	o := s.seogoOpts()
	if p.Status == "publish" && o.Modules["redirections"] && o.AutoRedirect {
		pathOf := func(link string) string {
			if u, err := url.Parse(link); err == nil && u.Path != "" {
				return u.Path
			}
			return "/" + normPath(link)
		}
		newPath := pathOf(s.seogoPostLoc(ctx, s.links(ctx), p))
		for _, old := range p.OldSlugs() {
			cp := *p
			cp.Slug = old
			src := pathOf(s.seogoPostLoc(ctx, s.links(ctx), &cp))
			if normPath(src) == normPath(newPath) || strings.Trim(src, "/") == "" {
				continue
			}
			var exists bool
			_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM redirects WHERE trim(both '/' from source) = $1)`, normPath(src)).Scan(&exists)
			if !exists {
				_ = s.seogoAddRedirect(ctx, &seogoRedirectRow{Source: src, Target: newPath, Code: 301, Origin: "auto"})
			}
		}
		// A redirect away from the post's current address would hide it: drop it.
		if tag, _ := s.store.Pool.Exec(ctx, `DELETE FROM redirects WHERE trim(both '/' from source) = $1 AND match_type = 'exact'`, normPath(newPath)); tag.RowsAffected() > 0 {
			s.seogoRedirectsChanged()
		}
	}
	if p.Status == "publish" && o.Modules["indexnow"] {
		// Only what the sitemap lists: public post types, no noindex/password/excluded/canonical elsewhere.
		loc := s.seogoPostLoc(ctx, s.links(ctx), p)
		if slices.Contains(s.sitemapPostTypes(ctx), p.Type) && s.seogoIndexable(o, p, loc, idSet(o.SitemapExclude)) {
			s.seogoIndexNow(ctx, []string{loc})
		}
	}
}

// --- 404 monitor -------------------------------------------------------------------------------

// seogo404 logs a 404 and applies the "redirect all 404s" fallback; true means it answered.
func (s *Server) seogo404(c *gin.Context) bool {
	if !s.seogoOn() {
		return false
	}
	o := s.seogoOpts()
	if o.Modules["monitor"] {
		uri := c.Request.URL.RequestURI()
		skip := false
		for _, ig := range strings.Split(o.MonitorIgnore, "\n") {
			if ig = strings.TrimSpace(ig); ig != "" && strings.Contains(uri, ig) {
				skip = true
			}
		}
		if !skip && len(uri) < 2000 {
			ref, ua := c.Request.Referer(), c.Request.UserAgent()
			// Bounded queue (background.go); the table is trimmed to MonitorLimit by the hourly
			// seogo_404_trim job, not per request.
			s.asyncExec(`INSERT INTO seogo_404 (uri, referer, user_agent) VALUES ($1, $2, $3)
				ON CONFLICT (uri) DO UPDATE SET hits = seogo_404.hits + 1, last_seen = now(), referer = EXCLUDED.referer, user_agent = EXCLUDED.user_agent`, uri, cut(ref, 500), cut(ua, 500))
		}
	}
	if o.Modules["redirections"] && strings.TrimSpace(o.Redirect404) != "" {
		loc, ok := seogoSafeTarget(o.Redirect404)
		if !ok {
			return false
		}
		c.Redirect(http.StatusMovedPermanently, loc)
		return true
	}
	return false
}

// seogo404Trim keeps the 404 log at the monitor's limit (the hourly seogo_404_trim job).
func (s *Server) seogo404Trim(ctx context.Context) error {
	if !s.seogoOn() {
		return nil
	}
	o := s.seogoOpts()
	if o.MonitorLimit <= 0 {
		return nil
	}
	_, err := s.store.Pool.Exec(ctx, `DELETE FROM seogo_404 WHERE id IN (SELECT id FROM seogo_404 ORDER BY last_seen DESC OFFSET $1)`, o.MonitorLimit)
	return err
}

// seogoQueryRedirect sends disabled author and date archives to the home page (Yoast's
// "Show author archives" / "Show date archives" switched off). True means it answered.
func (s *Server) seogoQueryRedirect(c *gin.Context, q *siteQuery) bool {
	if !s.seogoOn() || q == nil || q.Feed != "" {
		return false
	}
	o := s.seogoOpts()
	if (q.Kind == "author" && o.Types["author"].Disabled) || (q.Kind == "date" && o.Types["date"].Disabled) {
		c.Redirect(http.StatusMovedPermanently, s.links(c).HomeURL("/"))
		return true
	}
	return false
}

// --- RSS ---------------------------------------------------------------------------------------

// seogoFeedContent adds the RSS before/after texts to a feed item.
func (s *Server) seogoFeedContent(ctx context.Context, p *store.Post, content string) string {
	if !s.seogoOn() {
		return content
	}
	o := s.seogoOpts()
	if o.RSSBefore == "" && o.RSSAfter == "" {
		return content
	}
	lb := s.links(ctx)
	sc := &seogoCtx{s: s, ctx: ctx, lb: lb, Kind: "single", Post: p}
	fill := func(tpl string) string {
		tpl = yoastVars(tpl)
		r := strings.NewReplacer(
			"%postlink%", `<a href="`+html.EscapeString(lb.Permalink(p))+`">`+html.EscapeString(p.Title)+`</a>`,
			"%bloglink%", `<a href="`+html.EscapeString(lb.HomeURL("/"))+`">`+html.EscapeString(s.settings.String("blogname"))+`</a>`,
			"%blogdesclink%", `<a href="`+html.EscapeString(lb.HomeURL("/"))+`">`+html.EscapeString(s.settings.String("blogname")+" - "+s.settings.String("blogdescription"))+`</a>`,
			"%authorlink%", func() string {
				if a := sc.author(); a != nil {
					return `<a href="` + html.EscapeString(lb.AuthorLink(a.ID, a.Nicename)) + `">` + html.EscapeString(a.DisplayName) + `</a>`
				}
				return ""
			}(),
		)
		return "<p>" + sc.replace(r.Replace(tpl)) + "</p>"
	}
	if o.RSSBefore != "" {
		content = fill(o.RSSBefore) + content
	}
	if o.RSSAfter != "" {
		content += fill(o.RSSAfter)
	}
	return content
}

// seogoSafeTarget keeps a redirect on this site (safeRedirect) or on an http(s) URL with a host.
// Protocol-relative targets and javascript:, data: and vbscript: URLs are refused. A browser
// redirect is not a server request, so private (intranet) hosts are allowed.
func seogoSafeTarget(target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" || hasControlChar(target) || strings.Contains(target, `\`) || strings.HasPrefix(target, "//") {
		return "", false
	}
	low := strings.ToLower(target)
	for _, p := range []string{"javascript:", "data:", "vbscript:"} {
		if strings.HasPrefix(low, p) {
			return "", false
		}
	}
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return "", false
		}
		return u.String(), true
	}
	path := target
	if !strings.HasPrefix(path, "/") {
		path = "/" + strings.TrimLeft(path, "/")
	}
	cleaned := safeRedirect(path, "")
	if cleaned == "" {
		return "", false
	}
	// path.Clean drops a trailing slash. A directory redirect such as /landing/ keeps it
	// after the same checks safeRedirect already applied.
	if strings.HasSuffix(strings.SplitN(path, "?", 2)[0], "/") {
		base, query, ok := strings.Cut(cleaned, "?")
		if base != "/" && !strings.HasSuffix(base, "/") {
			base += "/"
		}
		if ok {
			cleaned = base + "?" + query
		} else {
			cleaned = base
		}
	}
	return cleaned, true
}

// seogoSiteTarget is an absolute http(s) URL on this site's host. seogoSafeTarget refuses every
// literal private address, which would also refuse a redirect to the site itself when Site URL is
// 127.0.0.1.
func (s *Server) seogoSiteTarget(c *gin.Context, target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" || hasControlChar(target) || strings.Contains(target, `\`) || strings.HasPrefix(target, "//") {
		return "", false
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", false
	}
	home, err := url.Parse(s.links(c).HomeURL("/"))
	if err != nil || !strings.EqualFold(u.Host, home.Host) {
		return "", false
	}
	return u.String(), true
}

// --- IndexNow ----------------------------------------------------------------------------------

// seogoIndexNowEndpoint is the shared IndexNow API (tests replace it).
var seogoIndexNowEndpoint = "https://api.indexnow.org/indexnow"

// indexNowClient dials only public addresses. A test endpoint on loopback may dial loopback;
// a public endpoint that resolves to a private address, or redirects there, is refused.
func indexNowClient(raw string) *http.Client {
	endpoint, _ := url.Parse(raw)
	allowLoop := false
	if endpoint != nil {
		if ip := net.ParseIP(endpoint.Hostname()); ip != nil && ip.IsLoopback() {
			allowLoop = true
		}
	}
	block := publicOnly(nil, "IndexNow address points at a private network")
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, c syscall.RawConn) error {
		if allowLoop {
			host, _, err := net.SplitHostPort(address)
			if err == nil {
				if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
					return nil
				}
			}
		}
		return block(network, address, c)
	}}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("unsupported redirect scheme")
			}
			if endpoint != nil && !strings.EqualFold(req.URL.Hostname(), endpoint.Hostname()) {
				if ip := net.ParseIP(req.URL.Hostname()); ip != nil && !publicIP(ip) {
					return errors.New("IndexNow redirect points at a private network")
				}
			}
			return nil
		},
	}
}

// IndexNow retries a submission that failed with a network error, 429 or 5xx.
var (
	seogoIndexNowAttempts = 3
	seogoIndexNowBackoff  = 30 * time.Second
)

const seogoIndexNowLogKey = "seogo_indexnow_log"

type seogoIndexNowLog struct {
	Time   time.Time
	URLs   []string
	Status string
}

var seogoIndexNowHistory = struct {
	sync.Mutex // shared by every Server of the process, like seogoRedirects
	m          map[*Server][]seogoIndexNowLog
}{m: map[*Server][]seogoIndexNowLog{}}

// seogoIndexNow submits URLs in the background and keeps the last 20 results.
func (s *Server) seogoIndexNow(ctx context.Context, urls []string) {
	o := s.seogoOpts()
	if o.IndexNowKey == "" || len(urls) == 0 || !s.settings.Bool("blog_public") {
		return
	}
	lb := s.links(ctx)
	home, _ := url.Parse(lb.HomeURL("/"))
	if home == nil || home.Hostname() == "localhost" || home.Hostname() == "127.0.0.1" {
		if seogoIndexNowEndpoint == "https://api.indexnow.org/indexnow" {
			s.seogoIndexNowRecord(urls, "skipped: local site")
			return
		}
	}
	body, _ := json.Marshal(map[string]any{"host": home.Host, "key": o.IndexNowKey, "keyLocation": lb.HomeURL("/" + o.IndexNowKey + ".txt"), "urlList": urls})
	endpoint := seogoIndexNowEndpoint
	safeGo("seogo_front.seogoIndexNow", func() {
		cl := indexNowClient(endpoint)
		status := ""
		for attempt := 0; attempt < seogoIndexNowAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(seogoIndexNowBackoff * time.Duration(attempt))
			}
			retry := false
			resp, err := cl.Post(endpoint, "application/json; charset=utf-8", bytes.NewReader(body))
			if err != nil {
				status, retry = "error: "+err.Error(), true
			} else {
				status = resp.Status
				retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
				resp.Body.Close()
			}
			if !retry {
				break
			}
		}
		s.seogoIndexNowRecord(urls, status)
	})
}

func (s *Server) seogoIndexNowRecord(urls []string, status string) {
	seogoIndexNowHistory.Lock()
	defer seogoIndexNowHistory.Unlock()
	h := append([]seogoIndexNowLog{{Time: time.Now(), URLs: urls, Status: status}}, s.seogoIndexNowLoad()...)
	if len(h) > 20 {
		h = h[:20]
	}
	seogoIndexNowHistory.m[s] = h
	// Persist so a restart or a second process keeps the last submissions (best effort).
	if s.store != nil && s.store.Pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.settings.Set(ctx, map[string]any{seogoIndexNowLogKey: h})
	}
}

// seogoIndexNowLoad is the history: the process copy, else the persisted option. Callers hold the lock.
func (s *Server) seogoIndexNowLoad() []seogoIndexNowLog {
	if h, ok := seogoIndexNowHistory.m[s]; ok {
		return h
	}
	var h []seogoIndexNowLog
	if v := s.settings.Get(seogoIndexNowLogKey); v != nil {
		if raw, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(raw, &h)
		}
	}
	return h
}

func (s *Server) seogoIndexNowLog() []seogoIndexNowLog {
	seogoIndexNowHistory.Lock()
	defer seogoIndexNowHistory.Unlock()
	return append([]seogoIndexNowLog{}, s.seogoIndexNowLoad()...)
}

// sortedKeys returns a map's keys in order (templates).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seogoXSL styles the sitemaps in a browser like Yoast's main-sitemap.xsl.
const seogoXSL = `<?xml version="1.0" encoding="UTF-8"?>
<xsl:stylesheet version="2.0" xmlns:html="http://www.w3.org/TR/REC-html40" xmlns:image="http://www.google.com/schemas/sitemap-image/1.1" xmlns:sitemap="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xsl="http://www.w3.org/1999/XSL/Transform">
<xsl:output method="html" version="1.0" encoding="UTF-8" indent="yes"/>
<xsl:template match="/">
<html xmlns="http://www.w3.org/1999/xhtml">
<head>
<title>XML Sitemap</title>
<meta http-equiv="Content-Type" content="text/html; charset=utf-8" />
<style type="text/css">
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Oxygen-Sans,Ubuntu,Cantarell,"Helvetica Neue",sans-serif;color:#3c434a;margin:0}
#content{margin:0 auto;max-width:960px;padding:16px}
h1{font-size:24px;font-weight:400;margin:0 0 8px}
p.desc{color:#646970;margin:0 0 20px}
table{border:none;border-collapse:collapse;width:100%;font-size:13px}
th{background:#2271b1;color:#fff;text-align:left;padding:8px 12px;font-weight:600}
td{padding:8px 12px;border-bottom:1px solid #f0f0f1}
tr:nth-child(even) td{background:#f6f7f7}
a{color:#2271b1;text-decoration:none}a:hover{text-decoration:underline}
</style>
</head>
<body>
<div id="content">
<h1>XML Sitemap</h1>
<p class="desc">Generated by Pursae SEO. It is meant for search engines such as Google and Bing.</p>
<xsl:if test="count(sitemap:sitemapindex/sitemap:sitemap) &gt; 0">
<p class="desc">This XML Sitemap Index file contains <xsl:value-of select="count(sitemap:sitemapindex/sitemap:sitemap)"/> sitemaps.</p>
<table><tr><th>Sitemap</th><th>Last Modified</th></tr>
<xsl:for-each select="sitemap:sitemapindex/sitemap:sitemap">
<tr><td><a href="{sitemap:loc}"><xsl:value-of select="sitemap:loc"/></a></td><td><xsl:value-of select="sitemap:lastmod"/></td></tr>
</xsl:for-each>
</table>
</xsl:if>
<xsl:if test="count(sitemap:sitemapindex/sitemap:sitemap) &lt; 1">
<p class="desc">This XML Sitemap contains <xsl:value-of select="count(sitemap:urlset/sitemap:url)"/> URLs.</p>
<table><tr><th>URL</th><th>Images</th><th>Last Mod.</th></tr>
<xsl:for-each select="sitemap:urlset/sitemap:url">
<tr><td><a href="{sitemap:loc}"><xsl:value-of select="sitemap:loc"/></a></td><td><xsl:value-of select="count(image:image)"/></td><td><xsl:value-of select="sitemap:lastmod"/></td></tr>
</xsl:for-each>
</table>
</xsl:if>
</div>
</body>
</html>
</xsl:template>
</xsl:stylesheet>
`

// seogoAbsURL resolves a (possibly relative) URL against the home URL.
func seogoAbsURL(home, ref string) string {
	b, err := url.Parse(home)
	r, err2 := url.Parse(ref)
	if err != nil || err2 != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// seogoTermMoved redirects the old address of a category/tag/custom term to the new one when its slug
// (or parent) changed: the term counterpart of the post slug redirect in seogoAfterSave.
func (s *Server) seogoTermMoved(ctx context.Context, old, cur *store.Term) {
	if !s.seogoOn() || old == nil || cur == nil || old.Slug == cur.Slug && old.ParentID == cur.ParentID {
		return
	}
	o := s.seogoOpts()
	if !o.Modules["redirections"] || !o.AutoRedirect {
		return
	}
	pathOf := func(link string) string {
		if u, err := url.Parse(link); err == nil && u.Path != "" {
			return u.Path
		}
		return "/" + normPath(link)
	}
	src := pathOf(s.seogoTermLoc(ctx, old))
	dst := pathOf(s.seogoTermLoc(ctx, cur))
	if normPath(src) == normPath(dst) || strings.Trim(src, "/") == "" {
		return
	}
	var exists bool
	_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM redirects WHERE trim(both '/' from source) = $1)`, normPath(src)).Scan(&exists)
	if !exists {
		_ = s.seogoAddRedirect(ctx, &seogoRedirectRow{Source: src, Target: dst, Code: 301, Origin: "auto"})
	}
	// A redirect away from the term's current address would hide it: drop it.
	if tag, _ := s.store.Pool.Exec(ctx, `DELETE FROM redirects WHERE trim(both '/' from source) = $1 AND match_type = 'exact'`, normPath(dst)); tag.RowsAffected() > 0 {
		s.seogoRedirectsChanged()
	}
}
