package server

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
)

// The <head> output of Pursae SEO: title, meta description, robots, canonical, Open Graph, Twitter/X
// cards, webmaster verification and the JSON-LD graph (seogo_schema.go). While the plugin is on,
// wpHead prints this block instead of core's robots meta and canonical link.

// seogoTitle is the SEO title of a context (no HTML escaping).
func (sc *seogoCtx) title() string {
	o := sc.s.seogoOpts()
	tpl := ""
	switch {
	case sc.singular():
		tpl = seogoMeta(sc.Post.Meta, "title")
		if tpl == "" && sc.Kind == "front-page" {
			tpl = sc.s.seogoTr(sc.ctx, o.Types["home"].Title)
		}
	case sc.Term != nil:
		tpl = seogoMeta(sc.Term.Meta, "title")
	}
	if tpl == "" {
		tpl = sc.s.seogoTr(sc.ctx, o.Types[sc.typeKey()].Title)
	}
	if tpl == "" && sc.Kind == "post-type-archive" {
		tpl = "%pt_plural% Archive %page% %sep% %sitename%" // Rank Math's default
	}
	if tpl == "" {
		tpl = "%title% %page% %sep% %sitename%"
	}
	return sc.replace(tpl)
}

func (sc *seogoCtx) description() string {
	o := sc.s.seogoOpts()
	tpl := ""
	switch {
	case sc.singular():
		tpl = seogoMeta(sc.Post.Meta, "description")
		if tpl == "" && sc.Kind == "front-page" {
			tpl = sc.s.seogoTr(sc.ctx, o.Types["home"].Description)
		}
	case sc.Term != nil:
		tpl = seogoMeta(sc.Term.Meta, "description")
	}
	if tpl == "" {
		tpl = sc.s.seogoTr(sc.ctx, o.Types[sc.typeKey()].Description)
	}
	return cut(sc.replace(tpl), 320)
}

// robots returns the robots directives, index/noindex and follow/nofollow first.
func (sc *seogoCtx) robots(preview bool) []string {
	s := sc.s
	o := s.seogoOpts()
	if !s.settings.Bool("blog_public") || preview {
		return []string{"noindex", "nofollow"}
	}
	noindex := o.Types[sc.typeKey()].Noindex
	nofollow := false
	adv := ""
	var meta map[string]any
	switch {
	case sc.singular():
		meta = sc.Post.Meta
		if sc.Post.Status != "publish" {
			noindex = true
		}
	case sc.Term != nil:
		meta = sc.Term.Meta
		if o.NoindexEmptyTerms && sc.Term.Count == 0 {
			noindex = true
		}
	}
	if meta != nil {
		switch seogoMeta(meta, "noindex") {
		case "noindex":
			noindex = true
		case "index":
			noindex = false
		}
		nofollow = seogoMeta(meta, "nofollow") == "1"
		adv = seogoMeta(meta, "robots_adv")
	}
	if o.NoindexPaged && sc.Paged > 1 {
		noindex = true
	}
	if sc.Kind == "404" || sc.Kind == "search" {
		noindex = true
	}
	// Pursae Commerce: Cart, Checkout and My account are never indexed (wc_page_noindex); a store
	// search (a shop view without a canonical URL) is a search result.
	if sc.storePrivate() || (sc.storeArchive() && sc.StoreURL == "") {
		noindex = true
	}
	out := []string{map[bool]string{true: "noindex", false: "index"}[noindex], map[bool]string{true: "nofollow", false: "follow"}[nofollow]}
	for _, d := range strings.Split(adv, ",") {
		if d = strings.TrimSpace(d); d == "noarchive" || d == "nosnippet" || d == "noimageindex" {
			out = append(out, d)
		}
	}
	if !noindex {
		limits := map[string]string{}
		for _, d := range strings.Split(o.RobotsDefaults, ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(d), ":"); ok {
				limits[k] = v
			}
		}
		if meta != nil {
			for _, f := range []string{"max_snippet", "max_image_preview", "max_video_preview"} {
				if k, v := strings.ReplaceAll(f, "_", "-"), seogoMeta(meta, f); v != "" && seogoRobotsLimitOK(k, v) {
					limits[k] = v // an invalid value keeps the site default
				}
			}
		}
		for _, k := range []string{"max-snippet", "max-image-preview", "max-video-preview"} {
			if v, ok := limits[k]; ok && seogoRobotsLimitOK(k, v) {
				out = append(out, k+":"+v)
			}
		}
	}
	return out
}

// url is the canonical URL of the context ("" for search results and 404s).
func (sc *seogoCtx) url() string {
	lb := sc.lb
	switch {
	case sc.Store == "product" && sc.StoreURL != "":
		return sc.StoreURL
	case sc.storeArchive():
		// Never the synthetic page's permalink: its id may be a term id (?page_id=<term>, or the home
		// URL when it equals page_on_front).
		if sc.StoreURL == "" {
			return ""
		}
		if sc.Paged > 1 {
			return lb.Paged(sc.StoreURL, sc.Paged)
		}
		return sc.StoreURL
	}
	switch sc.Kind {
	case "post-type-archive":
		u := lb.PostTypeArchiveLink(sc.PostType)
		if u != "" && sc.Paged > 1 {
			return lb.Paged(u, sc.Paged)
		}
		return u
	case "single", "page":
		u := lb.Permalink(sc.Post)
		if sc.Page > 1 {
			u = lb.PostPageLink(sc.Post, sc.Page)
		}
		return u
	case "front-page":
		return sc.homeURL()
	case "home":
		base := sc.homeURL()
		if sc.Post != nil {
			base = lb.Permalink(sc.Post)
		}
		if sc.Paged > 1 {
			return lb.Paged(base, sc.Paged)
		}
		return base
	case "category", "tag", "taxonomy":
		if sc.Paged > 1 {
			return lb.Paged(lb.TermLink(sc.Term), sc.Paged)
		}
		return lb.TermLink(sc.Term)
	case "author":
		if a := sc.author(); a != nil {
			u := lb.AuthorLink(a.ID, a.Nicename)
			if sc.Paged > 1 {
				return lb.Paged(u, sc.Paged)
			}
			return u
		}
	case "date":
		var u string
		switch {
		case sc.Day != 0:
			u = lb.DayLink(sc.Year, sc.Month, sc.Day)
		case sc.Month != 0:
			u = lb.MonthLink(sc.Year, sc.Month)
		default:
			u = lb.YearLink(sc.Year)
		}
		if sc.Paged > 1 {
			return lb.Paged(u, sc.Paged)
		}
		return u
	}
	return ""
}

// absURL makes a site-relative URL (/wp-content/…) absolute: og:image and schema URLs must be.
func (sc *seogoCtx) absURL(u string) string {
	if strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//") {
		return sc.lb.HomeURL(u)
	}
	return u
}

// homeURL is the site home of the request language (Pursae Multi Language), else the site root.
func (sc *seogoCtx) homeURL() string {
	if u, ok := sc.s.golylangHomeURL(sc.ctx); ok {
		return u
	}
	return sc.lb.HomeURL("/")
}

func (sc *seogoCtx) canonical() string {
	if sc.singular() {
		if v := seogoMeta(sc.Post.Meta, "canonical"); v != "" {
			return v
		}
	}
	if sc.Term != nil {
		if v := seogoMeta(sc.Term.Meta, "canonical"); v != "" {
			return v
		}
	}
	return sc.url()
}

// seogoImage is an image for social cards and the schema graph.
type seogoImage struct {
	URL           string
	Width, Height int
	Mime, Alt     string
	ID            int64
}

var seogoImgSrcRe = regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["']`)

// image picks the social image: the field's own value, the featured image, the first image of the
// content, the site default.
func (sc *seogoCtx) image(field string) *seogoImage {
	s := sc.s
	if sc.singular() || sc.Term != nil {
		meta := map[string]any(nil)
		if sc.Post != nil && sc.singular() {
			meta = sc.Post.Meta
		} else if sc.Term != nil {
			meta = sc.Term.Meta
		}
		if field != "" {
			if v := seogoMeta(meta, field); v != "" {
				return &seogoImage{URL: v}
			}
		}
		if v := seogoMeta(meta, "og_image"); v != "" && field == "tw_image" {
			return &seogoImage{URL: v}
		}
	}
	if sc.Post != nil && sc.singular() {
		if img := s.seogoAttachmentImage(sc.ctx, sc.Post.FeaturedMediaID); img != nil {
			return img
		}
		if m := seogoImgSrcRe.FindStringSubmatch(sc.Post.Content); m != nil {
			return &seogoImage{URL: sc.absURL(html.UnescapeString(m[1]))}
		}
	}
	o := s.seogoOpts()
	if (sc.Kind == "front-page" || (sc.Kind == "home" && sc.Post == nil)) && o.HomeOGImage != "" {
		return &seogoImage{URL: o.HomeOGImage}
	}
	if o.DefaultImage != "" {
		return &seogoImage{URL: o.DefaultImage}
	}
	return nil
}

func (s *Server) seogoAttachmentImage(ctx context.Context, id int64) *seogoImage {
	if id <= 0 {
		return nil
	}
	a, err := s.store.GetAttachment(ctx, id)
	if err != nil || !strings.HasPrefix(a.Mime, "image/") {
		return nil
	}
	return &seogoImage{URL: s.uploadURL(ctx, a.FilePath), Width: a.Width, Height: a.Height, Mime: a.Mime, Alt: a.Alt, ID: a.ID}
}

// readingTime is the estimated reading time in minutes (Yoast's "Est. reading time", 200 wpm).
func readingTime(text string) int {
	n := len(strings.Fields(text))
	m := (n + 199) / 200
	if m < 1 {
		m = 1
	}
	return m
}

// seogoLocale is the og:locale of a request: the Pursae Multi Language language, else the site language.
func (s *Server) seogoLocale(ctx context.Context) string {
	if l := s.pllCurrent(ctx); l != nil && l.Locale != "" {
		return l.Locale
	}
	if l := s.settings.String("WPLANG"); l != "" {
		return l
	}
	return "en_US"
}

// localeAlternates are the og:locale:alternate values: the other languages the page exists in
// (Pursae Multi Language's hreflang links), the way Polylang extends Yoast.
func (sc *seogoCtx) localeAlternates(current string) []string {
	r := pllFrom(sc.ctx)
	if r == nil || !sc.s.golylangOn() {
		return nil
	}
	var out []string
	for _, a := range r.alternates {
		if a.Lang != nil && a.Lang.Locale != "" && a.Lang.Locale != current {
			out = append(out, a.Lang.Locale)
		}
	}
	return out
}

// seogoHead is the Pursae SEO block of wp_head (empty while the plugin is off).
func (s *Server) seogoHead(c *gin.Context, q *siteQuery, lb *links.Builder) string {
	if !s.seogoOn() || q == nil {
		return ""
	}
	sc := s.seogoCtxFor(c.Request.Context(), q)
	sc.lb = lb
	if fv := gooViewFrom(c); fv != nil && fv.Kind == "product" && fv.Product != nil && s.gooOn() {
		sc.storeProduct = s.gooProductLD(c, fv.Product)
	}
	if p := sc.Post; p != nil && p.Password != "" && !currentUser(c).Can("edit_post", p) {
		// The head is public and cacheable: nothing in it (schema, image, reading time) may come from the body
		// of a password-protected post.
		cp := *p
		cp.Content, cp.Excerpt = "", ""
		sc.Post, sc.Pages = &cp, 1
	}
	s.seogoMembersCtx(sc, currentUser(c))
	return s.seogoHeadFor(sc, q.Preview)
}

// seogoMembersCtx keeps Memberships-restricted content out of a context's head (description, schema,
// image, reading time), the way the_content hides it from the same viewer.
func (s *Server) seogoMembersCtx(sc *seogoCtx, u *store.User) {
	if sc.Post == nil {
		return
	}
	if p := s.seogoMembersPost(sc.ctx, u, sc.Post); p != sc.Post {
		sc.Post = p
		sc.Pages = len(splitPages(p.Content))
	}
}

// seogoMembersPost is the post as viewer u may read it while Commerce Memberships is on: a restricted
// post loses its content (and its excerpt unless the plan shows excerpts), and members-only shortcode
// bodies the viewer may not see are dropped. Anything else returns p itself.
func (s *Server) seogoMembersPost(ctx context.Context, u *store.User, p *store.Post) *store.Post {
	if p == nil || !s.gooOn() || !s.gooExtOn(gmemKey) {
		return p
	}
	if ok, _ := s.gmemPostAccess(ctx, u, p.ID, s.gmemPostPlans(ctx, p.ID)); !ok {
		cp := *p
		cp.Content = ""
		// The featured image is part of the restricted post too: no og:image / schema thumbnail /
		// sitemap image from it. (An SEO social image set by hand stays: the editor chose it for sharing.)
		cp.FeaturedMediaID = 0
		if s.gooExtBool(gmemKey, "show_excerpts") {
			cp.Excerpt = s.gmemShortcodesFor(ctx, u, cp.Excerpt)
		} else {
			cp.Excerpt = ""
		}
		return &cp
	}
	if !strings.Contains(p.Content, "[goocommerce_") && !strings.Contains(p.Excerpt, "[goocommerce_") {
		return p
	}
	cp := *p
	cp.Content, cp.Excerpt = s.gmemShortcodesFor(ctx, u, cp.Content), s.gmemShortcodesFor(ctx, u, cp.Excerpt)
	return &cp
}

// seogoRobotsLimitOK accepts the values the robots limits can have: a number of characters / seconds
// (-1 = no limit) or an image preview size. Anything else (the fields are free text in post meta) is
// dropped instead of reaching the robots tag.
func seogoRobotsLimitOK(k, v string) bool {
	if k == "max-image-preview" {
		return v == "none" || v == "standard" || v == "large"
	}
	n, err := strconv.Atoi(v)
	return err == nil && n >= -1
}

// seogoHeadFor renders the head block of a context (also the REST get_head endpoint).
func (s *Server) seogoHeadFor(sc *seogoCtx, preview bool) string {
	o := s.seogoOpts()
	esc := html.EscapeString
	var sb strings.Builder
	sb.WriteString("<!-- This site is optimized with Pursae SEO " + builtinPluginVersion + " - Pursae -->\n")
	title := sc.title()
	desc := sc.description()
	if desc != "" {
		fmt.Fprintf(&sb, "<meta name=\"description\" content=\"%s\" />\n", esc(desc))
	}
	robots := sc.robots(preview)
	fmt.Fprintf(&sb, "<meta name=\"robots\" content=\"%s\" />\n", esc(strings.Join(robots, ", ")))
	noindex := robots[0] == "noindex"
	canonical := sc.canonical()
	if canonical != "" && !noindex {
		fmt.Fprintf(&sb, "<link rel=\"canonical\" href=\"%s\" />\n", esc(canonical))
	}
	// rel prev/next on paginated archives
	if (!sc.singular() || sc.Kind == "home" || sc.storeArchive()) && sc.Paged > 1 {
		paged := sc.Paged
		sc.Paged--
		if prev := sc.url(); prev != "" {
			fmt.Fprintf(&sb, "<link rel=\"prev\" href=\"%s\" />\n", esc(prev))
		}
		sc.Paged = paged
	}
	url := sc.url()
	if canonical != "" {
		url = canonical
	}
	if o.OpenGraph {
		locale := s.seogoLocale(sc.ctx)
		fmt.Fprintf(&sb, "<meta property=\"og:locale\" content=\"%s\" />\n", esc(locale))
		for _, alt := range sc.localeAlternates(locale) {
			fmt.Fprintf(&sb, "<meta property=\"og:locale:alternate\" content=\"%s\" />\n", esc(alt))
		}
		typ := "website"
		if sc.singular() && sc.Kind != "front-page" && sc.Kind != "home" && sc.Post.Type == "post" {
			typ = "article"
		} else if sc.Store == "product" && sc.singular() {
			typ = "product"
		} else if sc.Kind == "author" {
			typ = "profile"
		}
		fmt.Fprintf(&sb, "<meta property=\"og:type\" content=\"%s\" />\n", typ)
		ogTitle, ogDesc := title, desc
		if sc.singular() {
			if v := seogoMeta(sc.Post.Meta, "og_title"); v != "" {
				ogTitle = sc.replace(v)
			}
			if v := seogoMeta(sc.Post.Meta, "og_description"); v != "" {
				ogDesc = sc.replace(v)
			}
		}
		fmt.Fprintf(&sb, "<meta property=\"og:title\" content=\"%s\" />\n", esc(ogTitle))
		if ogDesc != "" {
			fmt.Fprintf(&sb, "<meta property=\"og:description\" content=\"%s\" />\n", esc(ogDesc))
		}
		if url != "" {
			fmt.Fprintf(&sb, "<meta property=\"og:url\" content=\"%s\" />\n", esc(url))
		}
		fmt.Fprintf(&sb, "<meta property=\"og:site_name\" content=\"%s\" />\n", esc(s.seogoSiteName(sc.ctx)))
		if typ == "article" {
			if o.FacebookURL != "" {
				fmt.Fprintf(&sb, "<meta property=\"article:publisher\" content=\"%s\" />\n", esc(o.FacebookURL))
			}
			if sc.Post.PublishedAt != nil {
				fmt.Fprintf(&sb, "<meta property=\"article:published_time\" content=\"%s\" />\n", sc.Post.PublishedAt.UTC().Format(time.RFC3339))
			}
			if sc.Post.PublishedAt == nil || sc.Post.ModifiedAt.After(*sc.Post.PublishedAt) {
				fmt.Fprintf(&sb, "<meta property=\"article:modified_time\" content=\"%s\" />\n", sc.Post.ModifiedAt.UTC().Format(time.RFC3339))
			}
		}
		if img := sc.image("og_image"); img != nil {
			fmt.Fprintf(&sb, "<meta property=\"og:image\" content=\"%s\" />\n", esc(img.URL))
			if img.Width > 0 && img.Height > 0 {
				fmt.Fprintf(&sb, "<meta property=\"og:image:width\" content=\"%d\" />\n<meta property=\"og:image:height\" content=\"%d\" />\n", img.Width, img.Height)
			}
			if img.Mime != "" {
				fmt.Fprintf(&sb, "<meta property=\"og:image:type\" content=\"%s\" />\n", esc(img.Mime))
			}
		}
		if o.FacebookAppID != "" {
			fmt.Fprintf(&sb, "<meta property=\"fb:app_id\" content=\"%s\" />\n", esc(o.FacebookAppID))
		}
	}
	if o.TwitterCards {
		card := o.TwitterCard
		if card == "" {
			card = "summary_large_image"
		}
		fmt.Fprintf(&sb, "<meta name=\"twitter:card\" content=\"%s\" />\n", esc(card))
		if sc.singular() {
			if v := seogoMeta(sc.Post.Meta, "tw_title"); v != "" {
				fmt.Fprintf(&sb, "<meta name=\"twitter:title\" content=\"%s\" />\n", esc(sc.replace(v)))
			} else if !o.OpenGraph {
				fmt.Fprintf(&sb, "<meta name=\"twitter:title\" content=\"%s\" />\n", esc(title))
			}
			if v := seogoMeta(sc.Post.Meta, "tw_description"); v != "" {
				fmt.Fprintf(&sb, "<meta name=\"twitter:description\" content=\"%s\" />\n", esc(sc.replace(v)))
			}
			if v := seogoMeta(sc.Post.Meta, "tw_image"); v != "" {
				fmt.Fprintf(&sb, "<meta name=\"twitter:image\" content=\"%s\" />\n", esc(v))
			}
		}
		if u := strings.TrimPrefix(o.TwitterUser, "@"); u != "" {
			fmt.Fprintf(&sb, "<meta name=\"twitter:site\" content=\"@%s\" />\n", esc(u))
		}
		if sc.singular() && sc.Post.Type == "post" && sc.Kind == "single" {
			if a := sc.author(); a != nil {
				fmt.Fprintf(&sb, "<meta name=\"twitter:label1\" content=\"Written by\" />\n<meta name=\"twitter:data1\" content=\"%s\" />\n", esc(a.DisplayName))
			}
			mins := readingTime(plainText(sc.Post.Content))
			fmt.Fprintf(&sb, "<meta name=\"twitter:label2\" content=\"Est. reading time\" />\n<meta name=\"twitter:data2\" content=\"%d minute%s\" />\n", mins, map[bool]string{true: "", false: "s"}[mins == 1])
		}
	}
	if sc.Kind == "front-page" || (sc.Kind == "home" && sc.Paged <= 1) {
		for _, v := range []struct{ key, name string }{{"google", "google-site-verification"}, {"bing", "msvalidate.01"},
			{"baidu", "baidu-site-verification"}, {"yandex", "yandex-verification"}, {"pinterest", "p:domain_verify"}, {"naver", "naver-site-verification"}} {
			if code := strings.TrimSpace(o.Verify[v.key]); code != "" {
				fmt.Fprintf(&sb, "<meta name=\"%s\" content=\"%s\" />\n", v.name, esc(seogoVerifyCode(code)))
			}
		}
	}
	if o.Modules["schema"] && sc.Kind != "404" {
		sb.WriteString(s.seogoSchemaScript(sc, title, desc))
	}
	sb.WriteString("<!-- / Pursae SEO -->\n")
	return sb.String()
}

var seogoVerifyContentRe = regexp.MustCompile(`content=["']([^"']+)["']`)

// seogoVerifyCode accepts either the bare code or the whole <meta> tag a search console shows.
func seogoVerifyCode(v string) string {
	if m := seogoVerifyContentRe.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return v
}

// seogoDocumentTitle replaces wp_get_document_title() while the plugin is on ("" otherwise).
func (s *Server) seogoDocumentTitle(c *gin.Context, q *siteQuery) template.HTML {
	if !s.seogoOn() || q == nil {
		return ""
	}
	t := s.seogoCtxFor(c.Request.Context(), q).title()
	if t == "" {
		return ""
	}
	return template.HTML(html.EscapeString(t))
}

// seogoQueryForPost is the main query a post would have on the front end (REST, previews, the
// editor's analysis), decided the way the front end decides it (home and singular in site.go): with a
// static front page, page_on_front and its Multi Language translations are the front page of their
// language, and page_for_posts and its translations are the blog index ("home" with PostsPage).
func (s *Server) seogoQueryForPost(ctx context.Context, p *store.Post) *siteQuery {
	q := &siteQuery{Kind: "single", Post: p, Paged: 1}
	if p.Type != "page" {
		return q
	}
	q.Kind = "page"
	if s.settings.String("show_on_front") != "page" {
		return q
	}
	switch {
	case s.seogoIsStaticPage(ctx, p, "page_on_front"):
		q.Kind = "front-page"
	case s.seogoIsStaticPage(ctx, p, "page_for_posts"):
		q.Kind, q.PostsPage, q.Post = "home", p, nil
	}
	return q
}

// seogoIsStaticPage reports whether p is the page a static page option (page_on_front,
// page_for_posts) names, or its translation in p's language: the page golylangPage gives the front
// end for that option in that language.
func (s *Server) seogoIsStaticPage(ctx context.Context, p *store.Post, option string) bool {
	id := int64(s.settings.Int(option))
	if id <= 0 || p.ID <= 0 {
		return false
	}
	if p.ID == id {
		return true
	}
	if !s.golylangOn() || !s.pllTranslatedType(p.Type) {
		return false
	}
	lang := s.pllObjLang(ctx, pllPost, p.ID)
	return lang != "" && s.pllFrontTranslations(ctx, id)[lang] == p.ID
}
