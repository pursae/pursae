package server

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/blocks"
	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// siteView is the data the theme templates receive.
type siteView struct {
	Template string // index, single, page, archive, search, 404
	Q        *siteQuery

	SiteName       string
	Description    string
	HomeURL        string
	IsFront        bool // header: aria-current and <h1> site title
	TitleIsH1      bool
	DocumentTitle  template.HTML
	Head           template.HTML
	Footer         template.HTML
	BodyOpen       template.HTML // gap-widgets: wp_body_open() output (site_body_open.go)
	BodyClass      string
	AdminBar       template.HTML
	PrimaryMenu    template.HTML
	SocialMenu     template.HTML
	FooterPrimary  template.HTML
	FooterSocial   template.HTML
	HasMenus       bool
	Sidebar1       template.HTML
	ContentBottom  template.HTML
	ArchiveTitle   template.HTML
	ArchiveDesc    template.HTML
	SearchQuery    string
	SearchAction   string        // search form action: the home of the current language (searchFormAction)
	SearchHidden   template.HTML // hidden inputs of the search form (?lang= in query mode)
	Posts          []*postView
	Post           *postView
	Pagination     template.HTML
	PostNav        template.HTML
	Comments       *commentsView
	PrivacyLink    template.HTML
	CanPublish     bool
	IsHomeHeader   bool // index.php: home but not front page shows a screen-reader page title
	PostsPageTitle string

	// Theme layer: the active (or previewed) theme, its rendered widget areas, menus by location
	// and free-form data its Prepare hook adds for the templates.
	Theme    *Theme
	ThemeURI string
	// TotalPosts and TotalPages are found_posts and max_num_pages of the main query.
	TotalPosts, TotalPages int
	Areas                  map[string]template.HTML
	Menus                  map[string][]*store.MenuItem
	Data                   map[string]any
	ctx                    context.Context

	// Customizer (customize.go): theme mods of the theme being rendered, core's custom logo and site
	// icon markup, Additional CSS, and the changeset when this is a Customizer preview.
	ThemeMods  map[string]any
	CustomLogo template.HTML
	SiteIcon   template.HTML
	CustomCSS  string
	Customize  *czChangeset
}

type postView struct {
	ID           int64
	Type         string
	Title        template.HTML
	Permalink    string
	Content      template.HTML
	Excerpt      template.HTML
	ShowExcerpt  bool
	Class        string
	Sticky       bool
	ShowFeatured bool
	EntryMeta    template.HTML
	EditLink     template.HTML
	Biography    template.HTML
	ShowComments bool

	// Generic template-tag data for any theme.
	Post                               *store.Post
	Format                             string
	Date, Modified                     time.Time
	DateText, ModifiedText             string
	DateISO, ModifiedISO               string
	AuthorName, AuthorURL, AuthorEmail string
	AuthorBio                          string
	AuthorAvatar                       string // avatar URL (96px); AuthorEmail is kept empty for old templates
	Categories, Tags                   []termLink
	CommentCount                       int
	CommentsOpen, HasPassword          bool
	CommentsURL                        string
	Thumbnail                          template.HTML
	ThumbnailURL                       string
}

func (s *Server) buildSiteView(c *gin.Context, q *siteQuery) (*siteView, error) {
	// A request-scoped memo of terms, authors, attachments and avatars (site_memo.go); on the request too,
	// so helpers that take the gin context share it.
	c.Request = c.Request.WithContext(withViewMemo(c.Request.Context()))
	ctx := c.Request.Context()
	u := currentUser(c)
	lb := s.links(ctx)
	rc := &renderCtx{s: s, lb: lb, search: q.Search, c: c, user: u, q: q, reqURI: c.Request.URL.RequestURI(), page: true}
	if u != nil {
		rc.logoutNonce = s.nonce(c, "log-out")
	}
	ctx = withRender(ctx, rc)
	cs := s.customizeChangeset(c)
	if cs != nil {
		s.customizeRequery(c, q, cs)
	}

	v := &siteView{Q: q, SiteName: s.settings.String("blogname"), Description: s.settings.String("blogdescription"),
		HomeURL: lb.HomeURL("/"), SearchQuery: q.Search, CanPublish: u.Can("publish_posts", nil),
		Areas: map[string]template.HTML{}, Menus: map[string][]*store.MenuItem{}, Data: map[string]any{}, ctx: ctx}
	v.Theme = s.previewTheme(c)
	if v.Theme == nil {
		v.Theme = s.activeTheme()
	}
	v.ThemeURI = v.Theme.URI()
	v.IsFront = q.Paged <= 1 && (q.Kind == "front-page" || (q.Kind == "home" && q.PostsPage == nil))
	v.TitleIsH1 = q.Kind == "home" && q.PostsPage == nil
	v.IsHomeHeader = q.Kind == "home" && q.PostsPage != nil
	if q.PostsPage != nil {
		v.PostsPageTitle = q.PostsPage.Title
	}
	s.customizeView(c, v, cs)

	// Widget areas: a theme that registers none (Twenty Twenty-Five, Hello Pursae) prints none, so the
	// widgets stored for it (a fresh install's sidebar-1) are not rendered.
	if len(v.Theme.WidgetAreas) > 0 {
		widgets, _ := s.store.WidgetsByArea(ctx, v.Theme.Slug)
		for area, ws := range widgets {
			v.Areas[area] = s.renderWidgetArea(ctx, ws)
		}
	}
	v.Sidebar1 = v.Areas["sidebar-1"]
	for _, loc := range v.Theme.MenuLocations {
		if items, _ := s.menuAt(ctx, cs, loc.Slug); len(items) > 0 {
			s.resolveMenu(ctx, items, lb, q)
			v.Menus[loc.Slug] = items
		}
	}

	s.golylangView(c, q, v, lb) // Golylang: strings, menus and widgets of the current language
	v.SearchAction, v.SearchHidden = searchFormAction(lb)
	primary, social := v.Menus["primary"], v.Menus["social"]
	if len(primary) > 0 {
		v.PrimaryMenu = renderMenu(primary, "menu-primary-menu-container", "primary-menu", "", "")
		v.FooterPrimary = v.PrimaryMenu
	}
	if len(social) > 0 {
		v.SocialMenu = renderMenu(social, "menu-social-container", "social-links-menu", `<span class="screen-reader-text">`, `</span>`)
		v.FooterSocial = v.SocialMenu
	}
	v.HasMenus = len(primary)+len(social) > 0

	if id := int64(s.settings.Int("wp_page_for_privacy_policy")); id > 0 {
		if p, err := s.store.GetPostNoContent(ctx, id); err == nil && p.Status == "publish" {
			v.PrivacyLink = template.HTML(fmt.Sprintf(`<a class="privacy-policy-link" href="%s" rel="privacy-policy">%s</a><span role="separator" aria-hidden="true"></span>`,
				html.EscapeString(lb.Permalink(p)), html.EscapeString(p.Title)))
		}
	}

	archiveTitle, plainTitle := q.archiveTitle()
	bodyClasses := []string{}

	rc.mainLoop = true // in_the_loop() && is_main_query(): the first content images are not lazy-loaded
	switch q.Kind {
	case "single", "page", "front-page":
		v.Template = "single"
		if q.Post.Type == "page" {
			v.Template = "page"
		}
		s.prefetchPosts(ctx, []*store.Post{q.Post})
		pv := s.postView(ctx, c, q.Post, true, lb, u)
		v.Post = pv
		if pt := siteCPT(q.Post.Type); q.Post.Type == "post" || (pt != nil && !pt.Hierarchical) {
			v.PostNav = s.postNavigation(ctx, q.Post, lb)
			cacheDep(ctx, "archive:"+q.Post.Type) // the previous/next links change with the type's posts
		}
		if q.Post.ParentID != 0 { // the permalink and breadcrumbs carry the ancestors' slugs and titles
			for _, a := range s.ancestors(ctx, q.Post) { // request memo: the permalinks walked it already
				cacheDep(ctx, "post:"+itoa64(a.ID))
			}
		}
		if s.commentsOpen(q.Post) || q.Post.CommentCount > 0 {
			v.Comments = s.commentsView(c, q.Post, lb, u)
		}
		if q.Post.Type == "page" {
			bodyClasses = append(bodyClasses, "page-template-default", "page", "page-id-"+itoa64(q.Post.ID))
			if q.Kind == "front-page" {
				bodyClasses = append([]string{"home"}, bodyClasses...)
			}
			if q.Post.ParentID != 0 {
				bodyClasses = append(bodyClasses, "page-child", "parent-pageid-"+itoa64(q.Post.ParentID))
			}
		} else if q.Post.Type == "post" {
			bodyClasses = append(bodyClasses, "post-template-default", "single", "single-post", "postid-"+itoa64(q.Post.ID), "single-format-"+store.CanonicalPostFormat(q.Post.Format))
		} else { // a custom post type: body_class() without post formats
			bodyClasses = append(bodyClasses, q.Post.Type+"-template-default", "single", "single-"+q.Post.Type, "postid-"+itoa64(q.Post.ID))
		}
	case "404":
		v.Template = "404"
		bodyClasses = append(bodyClasses, "error404")
	default:
		posts, total, perPage, err := s.mainPosts(ctx, q, u)
		if err != nil {
			return nil, err
		}
		// WP::handle_404(): an empty date archive, or any paged archive past its last page, is a 404
		// (status, noindex, the Pursae SEO 404 monitor). Page 1 of other archives stays a 200.
		if len(posts) == 0 && (q.Kind == "date" || q.Paged > 1) {
			*q = siteQuery{Kind: "404"}
			return s.buildSiteView(c, q)
		}
		s.prefetchPosts(ctx, posts) // the loop's terms, authors, featured images and avatars in one query each
		for _, p := range posts {
			pv := s.postView(ctx, c, p, false, lb, u)
			pv.ShowFeatured = p.Sticky && q.Kind == "home" && q.Paged <= 1
			if p.Sticky && !pv.ShowFeatured {
				pv.Class = strings.Replace(pv.Class, " sticky", "", 1) // only the blog's first page marks sticky posts
			}
			v.Posts = append(v.Posts, pv)
		}
		base := s.archiveBase(q, lb)
		v.TotalPosts, v.TotalPages = total, (total+perPage-1)/perPage
		v.Pagination = paginateLinks(lb, base, q.Paged, v.TotalPages)
		switch q.Kind {
		case "home":
			v.Template = "index"
			if q.PostsPage == nil {
				bodyClasses = append(bodyClasses, "home")
			}
			bodyClasses = append(bodyClasses, "blog")
		case "search":
			v.Template = "search"
			if len(posts) > 0 {
				bodyClasses = append(bodyClasses, "search", "search-results")
			} else {
				bodyClasses = append(bodyClasses, "search", "search-no-results")
			}
		default:
			v.Template = "archive"
			v.ArchiveTitle = template.HTML(archiveTitle)
			if q.Term != nil && q.Term.Description != "" {
				v.ArchiveDesc = template.HTML(wp.Autop(html.EscapeString(q.Term.Description)))
			}
			bodyClasses = append(bodyClasses, "archive")
			switch q.Kind {
			case "category":
				bodyClasses = append(bodyClasses, "category", "category-"+q.Term.Slug, "category-"+itoa64(q.Term.ID))
			case "tag":
				bodyClasses = append(bodyClasses, "tag", "tag-"+q.Term.Slug, "tag-"+itoa64(q.Term.ID))
			case "taxonomy":
				bodyClasses = append(bodyClasses, "tax", "tax-"+q.Term.Taxonomy, "term-"+q.Term.Slug)
				if q.Term.ID != 0 {
					bodyClasses = append(bodyClasses, "term-"+itoa64(q.Term.ID))
				}
			case "author":
				bodyClasses = append(bodyClasses, "author", "author-"+q.Author.Nicename, "author-"+itoa64(q.Author.ID))
			case "date":
				bodyClasses = append(bodyClasses, "date")
			case "post-type-archive":
				bodyClasses = append(bodyClasses, "post-type-archive", "post-type-archive-"+q.PostType)
			}
		}
		if q.Paged > 1 {
			bodyClasses = append(bodyClasses, "paged", "paged-"+itoa(q.Paged))
		}
	}
	rc.mainLoop = false

	if u != nil {
		bodyClasses = append(bodyClasses, "logged-in")
	}
	showBar := u != nil && cs == nil && !s.pllVisualOn(c) // the Customizer preview and visual translation (golylang_visual.go) have no toolbar
	if frontEndOnly(u) {
		showBar = false // front-end members have no wp-admin to link to (admin_access.go)
	}
	if u != nil {
		if b, ok := u.Meta["show_admin_bar_front"].(bool); ok && !b {
			showBar = false // the user's "Show Toolbar" choice can hide it, never bring it back into a preview
		}
	}
	if showBar {
		bodyClasses = append(bodyClasses, "admin-bar", "no-customize-support")
	}
	bodyClasses = append(bodyClasses, customizeBodyClasses(v)...)
	bodyClasses = append(bodyClasses, s.gooBodyClasses(c, q)...) // Goocommerce: woocommerce, woocommerce-page, single-product, …
	bodyClasses = append(bodyClasses, s.forumBodyClasses(q)...)  // Pursae Forum: pursae-forum pf-view-<view>
	bodyClasses = append(bodyClasses, "wp-embed-responsive", "wp-theme-"+v.Theme.Slug)
	if v.Theme.Prepare != nil {
		v.Theme.Prepare(s, c, v)
	}
	for _, area := range v.Areas { // a sidebar (recent posts, comments, archives, …) depends on "widgets"
		if area != "" {
			cacheDepWidgets(ctx)
			break
		}
	}
	if v.Theme.BodyClasses != nil {
		bodyClasses = append(bodyClasses, v.Theme.BodyClasses(s, v)...)
	}
	v.BodyClass = strings.Join(bodyClasses, " ")

	v.DocumentTitle = template.HTML(s.documentTitle(q, plainTitle))
	if t := s.seogoDocumentTitle(c, q); t != "" { // Pursae SEO title (seogo_head.go)
		v.DocumentTitle = t
	}
	v.Head = s.wpHead(c, q, lb, showBar, v)
	v.BodyOpen = s.wpBodyOpen(c, v) // gap-widgets: wp_body_open (site_body_open.go)
	v.Footer = s.wpFooter(c, q, lb, showBar)
	if v.Theme.Footer != nil {
		v.Footer += template.HTML(v.Theme.Footer(s, c, v))
	}
	if showBar {
		v.AdminBar = template.HTML(s.pllUC(c, string(s.frontAdminBar(c, q, lb, u)), 0)) // the toolbar is no page text (golylang_uc.go)
	}
	s.customizeFinish(c, v, cs)
	return v, nil
}

func (s *Server) archiveBase(q *siteQuery, lb *links.Builder) string {
	switch q.Kind {
	case "category", "tag", "taxonomy":
		return lb.TermLink(q.Term)
	case "author":
		return lb.AuthorLink(q.Author.ID, q.Author.Nicename)
	case "date":
		switch {
		case q.Day != 0:
			return lb.DayLink(q.Year, q.Month, q.Day)
		case q.Month != 0:
			return lb.MonthLink(q.Year, q.Month)
		}
		return lb.YearLink(q.Year)
	case "search":
		return lb.SearchLink(q.Search)
	case "post-type-archive":
		if link := lb.PostTypeArchiveLink(q.PostType); link != "" {
			return link
		}
	}
	if q.PostsPage != nil {
		return lb.Permalink(q.PostsPage)
	}
	return lb.Front() // the language home with Pursae Multi Language: /tr/page/2/
}

// paginateLinks is the_posts_pagination() with mid_size 1 and end_size 1 (theme packages print it).
func paginateLinks(lb *links.Builder, base string, current, total int) template.HTML {
	if total < 2 {
		return ""
	}
	pageNum := `<span class="meta-nav screen-reader-text">Page </span>`
	var out []string
	if current > 1 {
		out = append(out, fmt.Sprintf(`<a class="prev page-numbers" href="%s">Previous page</a>`, html.EscapeString(lb.Paged(base, current-1))))
	}
	dots := false
	for n := 1; n <= total; n++ {
		switch {
		case n == current:
			out = append(out, fmt.Sprintf(`<span aria-current="page" class="page-numbers current">%s%d</span>`, pageNum, n))
			dots = true
		case n <= 1 || (n >= current-1 && n <= current+1) || n > total-1:
			out = append(out, fmt.Sprintf(`<a class="page-numbers" href="%s">%s%d</a>`, html.EscapeString(lb.Paged(base, n)), pageNum, n))
			dots = true
		case dots:
			out = append(out, `<span class="page-numbers dots">&hellip;</span>`)
			dots = false
		}
	}
	if current < total {
		out = append(out, fmt.Sprintf(`<a class="next page-numbers" href="%s">Next page</a>`, html.EscapeString(lb.Paged(base, current+1))))
	}
	return template.HTML(`
	<nav class="navigation pagination" aria-label="Posts pagination">
		<h2 class="screen-reader-text">Posts pagination</h2>
		<div class="nav-links">` + strings.Join(out, "\n") + `</div>
	</nav>`)
}

func (s *Server) postNavigation(ctx context.Context, p *store.Post, lb *links.Builder) template.HTML {
	prev, _ := s.gooAdjacent(ctx, p, true) // Goocommerce Memberships: hidden posts are skipped
	next, _ := s.gooAdjacent(ctx, p, false)
	if prev == nil && next == nil {
		return ""
	}
	var sb strings.Builder
	if prev != nil {
		fmt.Fprintf(&sb, `<div class="nav-previous"><a href="%s" rel="prev"><span class="meta-nav" aria-hidden="true">%s</span> <span class="screen-reader-text">%s</span> <span class="post-title">%s</span></a></div>`,
			html.EscapeString(lb.Permalink(prev)), s.frontE(ctx, "Previous"), s.frontE(ctx, "Previous post:"), s.pllUC(ctx, wp.Texturize(wp.EscText(prev.Title)), 0))
	}
	if next != nil {
		fmt.Fprintf(&sb, `<div class="nav-next"><a href="%s" rel="next"><span class="meta-nav" aria-hidden="true">%s</span> <span class="screen-reader-text">%s</span> <span class="post-title">%s</span></a></div>`,
			html.EscapeString(lb.Permalink(next)), s.frontE(ctx, "Next"), s.frontE(ctx, "Next post:"), s.pllUC(ctx, wp.Texturize(wp.EscText(next.Title)), 0))
	}
	return template.HTML(`
	<nav class="navigation post-navigation" aria-label="` + s.frontE(ctx, "Posts") + `">
		<h2 class="screen-reader-text">` + s.frontE(ctx, "Post navigation") + `</h2>
		<div class="nav-links">` + sb.String() + `</div>
	</nav>`)
}

// theTitle is get_the_title(): texturized, with "Protected:"/"Private:" prefixes.
func theTitle(p *store.Post) string {
	t := wp.Texturize(wp.EscText(p.Title))
	switch {
	case p.Password != "":
		t = "Protected: " + t
	case p.Status == "private":
		t = "Private: " + t
	}
	return t
}

// theContent is the_content(): the <!--more--> split, block rendering and the classic filters.
func (s *Server) theContent(ctx context.Context, c *gin.Context, p *store.Post, singular bool, lb *links.Builder) string {
	if s.passwordRequired(c, p) {
		return fmt.Sprintf(`<form action="/wp-login?action=postpass" class="post-password-form" method="post"><input type="hidden" name="redirect_to" value="%s" /></p>
<p>This content is password-protected. To view it please enter your password below.</p>
<p><label for="pwbox-%d">Password: <input name="post_password" id="pwbox-%d" type="password" spellcheck="false" required size="20" /></label> <input type="submit" name="Submit" value="Enter" /></p></form>
`, html.EscapeString(lb.Permalink(p)), p.ID, p.ID)
	}
	if out, ok := s.wbContent(ctx, c, p, singular); ok { // Pursae Web Builder snapshot (webbuilder_front.go)
		return out
	}
	rc := renderFrom(ctx)
	// setup_postdata(): the <!--nextpage--> split and the requested page of it (feeds show page 1).
	pages := splitPages(p.Content)
	page := 1
	if rc != nil && rc.q != nil && rc.q.Post != nil && rc.q.Post.ID == p.ID && rc.q.Page > 1 && !rc.feed {
		page = rc.q.Page
		if page > len(pages) {
			page = len(pages)
		}
	}
	content := pages[page-1]
	if p.Type != gontactPostType { // form templates keep raw markup a lesser capability saves
		content = s.cspTrustContent(ctx, content) // the stored scripts of an unfiltered_html author
	}
	teaser, rest, moreText, hasMore := wp.SplitMore(content)
	if hasMore {
		if singular {
			content = teaser + `<span id="more-` + itoa64(p.ID) + `"></span>` + rest
		} else {
			teaser = strings.TrimSuffix(strings.TrimRight(teaser, " \n"), "<!-- wp:more -->")
			if moreText == "" {
				moreText = `Continue reading<span class="screen-reader-text"> "` + theTitle(p) + `"</span>`
			} else {
				moreText = html.EscapeString(moreText)
			}
			content = teaser + ` <a href="` + html.EscapeString(lb.Permalink(p)) + `#more-` + itoa64(p.ID) + `" class="more-link">` + moreText + `</a>`
		}
	}
	if rc != nil {
		prev := rc.postID
		rc.postID = p.ID
		defer func() { rc.postID = prev }()
	}
	if blocks.HasBlocks(content) {
		content = blocks.Render(ctx, content)
	} else {
		content = wp.Autop(wp.Texturize(content))
	}
	content = s.doShortcodes(ctx, content)      // plugin shortcodes (do_shortcode at priority 11)
	content = s.gontactShortcodes(ctx, content) // Gontact Form's [contact-form-7]
	content = s.seogoContent(ctx, content)      // Pursae SEO breadcrumb shortcodes and Image SEO
	content = s.flipbookContent(ctx, content)   // Pursae Flipbook: [pursae_flipbook id="N"]
	content = s.gooikeShortcodes(ctx, content)  // Gooike: [gooike_preferences], [gooike_cookie_table], …
	content = s.gooShortcodes(ctx, content)     // Goocommerce: [goocommerce_cart], [products], … (goo_front.go)
	content = s.forumShortcodes(ctx, content)   // Pursae Forum: the view body and [pursae_forum] (forum_render.go)
	content = s.filterContentTags(ctx, content) // wp_filter_content_tags (priority 12)
	content = s.convertSmilies(lb, content)     // convert_smilies (priority 20)
	out, _ := s.applyFilters(ctx, "the_content", content, map[string]any{"id": p.ID, "type": p.Type, "is_single": singular}).(string)
	if len(pages) > 1 && (rc == nil || !rc.feed) {
		out += s.linkPages(p, page, len(pages), lb)
	}
	return out
}

// convertSmilies applies convert_smilies() when the use_smilies option is on.
func (s *Server) convertSmilies(lb *links.Builder, content string) string {
	if !s.settings.Bool("use_smilies") {
		return content
	}
	return wp.ConvertSmilies(content, lb.HomeURL("/wp-includes/images/smilies/"))
}

// linkPages is wp_link_pages() with the arguments the active theme passes (the WordPress defaults for
// themes that do not override them).
func (s *Server) linkPages(p *store.Post, page, numpages int, lb *links.Builder) string {
	before, after, linkBefore, linkAfter, pagelink, separator := `<p class="post-nav-links">Pages:`, `</p>`, "", "", "%", " "
	var sb strings.Builder
	sb.WriteString(before)
	for i := 1; i <= numpages; i++ {
		link := linkBefore + strings.Replace(pagelink, "%", strconv.Itoa(i), 1) + linkAfter
		if i != page {
			link = `<a href="` + html.EscapeString(lb.PostPageLink(p, i)) + `" class="post-page-numbers">` + link + `</a>`
		} else {
			link = `<span class="post-page-numbers current" aria-current="page">` + link + `</span>`
		}
		if i == 1 {
			sb.WriteString(" ")
		} else {
			sb.WriteString(separator)
		}
		sb.WriteString(link)
	}
	sb.WriteString(after)
	return sb.String()
}

// theExcerpt is the_excerpt() with a "Continue reading" more link.
func (s *Server) theExcerpt(p *store.Post, lb *links.Builder) string {
	if p.Password != "" {
		return "<p>There is no excerpt because this is a protected post.</p>\n"
	}
	if p.Excerpt != "" {
		return s.convertSmilies(lb, wp.Autop(wp.Texturize(wp.KsesPost(p.Excerpt)))) // kses: excerpts stored before excerpt_save_pre
	}
	more := ` &hellip; <a href="` + html.EscapeString(lb.Permalink(p)) + `" class="more-link">Continue reading<span class="screen-reader-text"> "` + theTitle(p) + `"</span></a>`
	text := stripBlocks(p.Content)
	text, _, _, _ = wp.SplitMore(text)
	return s.convertSmilies(lb, wp.Autop(wp.TrimWords(text, 55, more)))
}

func (s *Server) postView(ctx context.Context, c *gin.Context, p *store.Post, singular bool, lb *links.Builder, u *store.User) *postView {
	pv := &postView{ID: p.ID, Type: p.Type, Title: template.HTML(s.pluginTitle(ctx, s.pllUC(ctx, theTitle(p), 0), p)), Permalink: lb.Permalink(p), Sticky: p.Sticky}
	pv.Content = template.HTML(s.pllUC(ctx, s.theContent(ctx, c, p, singular, lb), p.ID)) // Multi Language user content (golylang_uc.go)
	pv.ShowExcerpt = p.Excerpt != "" || (renderFrom(ctx) != nil && renderFrom(ctx).search != "")
	if pv.ShowExcerpt {
		pv.Excerpt = template.HTML(s.pllUC(ctx, s.theExcerpt(p, lb), 0))
	}

	cats, tags := s.postTerms(ctx, p.ID, "category"), s.postTerms(ctx, p.ID, "post_tag") // request memo (site_memo.go)
	classes := []string{"post-" + itoa64(p.ID), p.Type, "type-" + p.Type, "status-" + p.Status}
	if p.Type == "post" {
		classes = append(classes, "format-"+store.CanonicalPostFormat(p.Format))
	}
	if p.Password != "" {
		classes = append(classes, "post-password-required")
	}
	if p.Sticky && !singular {
		classes = append(classes, "sticky")
	}
	classes = append(classes, "hentry")
	for _, t := range cats {
		classes = append(classes, "category-"+t.Slug)
	}
	for _, t := range tags {
		classes = append(classes, "tag-"+t.Slug)
	}
	pv.Class = strings.Join(classes, " ")

	author := s.displayUser(ctx, p.AuthorID) // display fields only: no capabilities are checked for the author
	// Pursae Cache: the page shows these terms and the author; list blocks or shortcodes in the content
	// may show anything.
	for _, t := range append(append([]*store.Term{}, cats...), tags...) {
		cacheDep(ctx, "term:"+itoa64(t.ID))
	}
	cacheDep(ctx, "author:"+itoa64(p.AuthorID))
	if contentListsOthers(p.Content) {
		cacheDepWidgets(ctx)
	}
	pv.EntryMeta = s.entryMeta(ctx, p, author, cats, tags, singular, lb)
	s.fillPostView(ctx, pv, p, author, cats, tags, lb)
	if u.Can("edit_post", p) {
		pv.EditLink = template.HTML(fmt.Sprintf(`<a class="post-edit-link" href="%s">Edit<span class="screen-reader-text"> "%s"</span></a>`,
			html.EscapeString(links.EditLink(p.ID)), theTitle(p)))
	}
	if singular && p.Type == "post" && author != nil && author.Profile["description"] != "" {
		pv.Biography = template.HTML(fmt.Sprintf(`<div class="author-info">
	<div class="author-avatar">
		%s	</div><!-- .author-avatar -->

	<div class="author-description">
		<h2 class="author-title"><span class="author-heading">Author:</span> %s</h2>

		<p class="author-bio">
			%s			<a class="author-link" href="%s" rel="author">
				View all posts by %s			</a>
		</p><!-- .author-bio -->
	</div><!-- .author-description -->
</div><!-- .author-info -->`, s.avatarImg(ctx, author.Email, 42), html.EscapeString(author.DisplayName), html.EscapeString(author.Profile["description"]),
			html.EscapeString(lb.AuthorLink(author.ID, author.Nicename)), html.EscapeString(author.DisplayName)))
	}
	return pv
}

func (s *Server) avatarImg(ctx context.Context, email string, size int) string {
	if !s.settings.Bool("show_avatars") { // get_avatar() returns false when Show Avatars is off
		return ""
	}
	return fmt.Sprintf(`<img alt='' src='%s' srcset='%s 2x' class='avatar avatar-%d photo' height='%d' width='%d' loading='lazy' decoding='async'/>`,
		html.EscapeString(s.avatarFor(ctx, email, size)), html.EscapeString(s.avatarFor(ctx, email, size*2)), size, size, size)
}

// entryMeta is the classic entry meta (byline, date, format, categories, tags, comments link) theme packages print.
func (s *Server) entryMeta(ctx context.Context, p *store.Post, author *store.User, cats, tags []*store.Term, singular bool, lb *links.Builder) template.HTML {
	var sb strings.Builder
	if p.Type == "post" && author != nil {
		fmt.Fprintf(&sb, `<span class="byline">%s<span class="screen-reader-text">Author </span><span class="author vcard"><a class="url fn n" href="%s">%s</a></span></span>`,
			s.avatarImg(ctx, author.Email, 49), html.EscapeString(lb.AuthorLink(author.ID, author.Nicename)), html.EscapeString(author.DisplayName))
	}
	if p.Type == "post" {
		loc := s.loc()
		df := s.settings.String("date_format")
		pub := p.Date().In(loc)
		mod := p.ModifiedAt.In(loc)
		timeStr := fmt.Sprintf(`<time class="entry-date published updated" datetime="%s">%s</time>`, pub.Format(time.RFC3339), wp.FormatDate(pub, df))
		if pub.Unix() != mod.Unix() {
			timeStr = fmt.Sprintf(`<time class="entry-date published" datetime="%s">%s</time><time class="updated" datetime="%s">%s</time>`,
				pub.Format(time.RFC3339), wp.FormatDate(pub, df), mod.Format(time.RFC3339), wp.FormatDate(mod, df))
		}
		fmt.Fprintf(&sb, `<span class="posted-on"><span class="screen-reader-text">Posted on </span><a href="%s" rel="bookmark">%s</a></span>`,
			html.EscapeString(lb.Permalink(p)), timeStr)
	}
	if f := store.CanonicalPostFormat(p.Format); p.Type == "post" && f != "standard" {
		fmt.Fprintf(&sb, `<span class="entry-format"><span class="screen-reader-text">Format </span><a href="%s">%s</a></span>`,
			html.EscapeString(lb.TermLink(&store.Term{Taxonomy: "post_format", Slug: "post-format-" + f})), html.EscapeString(postFormatLabels[f]))
	}
	if p.Type == "post" {
		if len(cats) > 0 && s.categorizedBlog(ctx) {
			sb.WriteString(`<span class="cat-links"><span class="screen-reader-text">Categories </span>`)
			for i, t := range cats {
				if i > 0 {
					sb.WriteString(", ")
				}
				fmt.Fprintf(&sb, `<a href="%s" rel="category tag">%s</a>`, html.EscapeString(lb.TermLink(t)), html.EscapeString(t.Name))
			}
			sb.WriteString(`</span>`)
		}
		if len(tags) > 0 {
			sb.WriteString(`<span class="tags-links"><span class="screen-reader-text">Tags </span>`)
			for i, t := range tags {
				if i > 0 {
					sb.WriteString(", ")
				}
				fmt.Fprintf(&sb, `<a href="%s" rel="tag">%s</a>`, html.EscapeString(lb.TermLink(t)), html.EscapeString(t.Name))
			}
			sb.WriteString(`</span>`)
		}
	}
	if !singular && p.Password == "" && (s.commentsOpen(p) || p.CommentCount > 0) { // comments_open(): old posts may be closed
		sb.WriteString(`<span class="comments-link">`)
		link := lb.Permalink(p)
		switch {
		case p.CommentCount == 0:
			fmt.Fprintf(&sb, `<a href="%s#respond">Leave a comment<span class="screen-reader-text"> on %s</span></a>`, html.EscapeString(link), theTitle(p))
		case p.CommentCount == 1:
			fmt.Fprintf(&sb, `<a href="%s#comments">1 Comment<span class="screen-reader-text"> on %s</span></a>`, html.EscapeString(link), theTitle(p))
		default:
			fmt.Fprintf(&sb, `<a href="%s#comments">%d Comments<span class="screen-reader-text"> on %s</span></a>`, html.EscapeString(link), p.CommentCount, theTitle(p))
		}
		sb.WriteString(`</span>`)
	}
	return template.HTML(sb.String())
}

// categorizedBlog reports whether more than one category is in use (category links show only then);
// once per request with a view memo.
func (s *Server) categorizedBlog(ctx context.Context) bool {
	m := viewMemoFrom(ctx)
	if m != nil {
		m.mu.Lock()
		v := m.categorized
		m.mu.Unlock()
		if v != nil {
			return *v
		}
	}
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM terms WHERE taxonomy='category' AND post_count > 0 LIMIT 2) t`).Scan(&n)
	ok := n > 1
	if m != nil {
		m.mu.Lock()
		m.categorized = &ok
		m.mu.Unlock()
	}
	return ok
}

// renderWidgetArea is dynamic_sidebar() with the default before/after_widget markup.
func (s *Server) renderWidgetArea(ctx context.Context, ws []*store.Widget) template.HTML {
	if len(ws) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, w := range ws {
		if w.Type != "block" {
			continue
		}
		content, _ := w.Settings["content"].(string)
		classes := "widget_block"
		if parsed := blocks.Parse(content); len(parsed) > 0 {
			classes = widgetBlockClass(parsed[0].Name())
		}
		out := s.gooWidgetShortcodes(ctx, blocks.Render(ctx, s.cspTrustContent(ctx, content))) // Goocommerce: store shortcodes in block widgets (goo_front_shortcodes.go)
		fmt.Fprintf(&sb, "<section id=\"block-%d\" class=\"widget %s\">%s</section>", w.ID, classes, out)
	}
	return template.HTML(sb.String())
}

func widgetBlockClass(name string) string {
	m := map[string]string{
		"core/paragraph": "widget_text", "core/calendar": "widget_calendar", "core/search": "widget_search",
		"core/html": "widget_custom_html", "core/archives": "widget_archive", "core/latest-posts": "widget_recent_entries",
		"core/latest-comments": "widget_recent_comments", "core/tag-cloud": "widget_tag_cloud", "core/categories": "widget_categories",
		"core/audio": "widget_media_audio", "core/video": "widget_media_video", "core/image": "widget_media_image",
		"core/gallery": "widget_media_gallery", "core/rss": "widget_rss", "core/navigation": "widget_nav_menu",
	}
	if c, ok := m[name]; ok {
		return "widget_block " + c
	}
	return "widget_block"
}

// renderMenu is wp_nav_menu() for a theme location.
func renderMenu(items []*store.MenuItem, container, class, before, after string) template.HTML {
	var sb strings.Builder
	var walk func(list []*store.MenuItem, depth int)
	walk = func(list []*store.MenuItem, depth int) {
		for _, it := range list {
			key := it.Key
			if key == "" {
				key = itoa64(it.ID)
			}
			classes := "menu-item menu-item-type-" + it.Kind + " menu-item-object-" + it.Object + " menu-item-" + html.EscapeString(key)
			if it.Current {
				classes += " current-menu-item"
				if it.Kind == "post_type" {
					classes += " current_page_item"
				}
			}
			if it.CurrentAncestor {
				classes += " current-menu-ancestor current-menu-parent"
			}
			if len(it.Children) > 0 {
				classes += " menu-item-has-children"
			}
			if it.Classes != "" {
				classes = it.Classes + " " + classes
			}
			// Menu item classes and URLs are free text (sanitize_html_class() / esc_url() in nav_menu_link_attributes).
			fmt.Fprintf(&sb, `<li id="menu-item-%s" class="%s"><a href="%s"`, html.EscapeString(key), html.EscapeString(classes), wp.EscURL(it.URL))
			if it.Target != "" {
				fmt.Fprintf(&sb, ` target="%s"`, html.EscapeString(it.Target))
			}
			if it.Current {
				sb.WriteString(` aria-current="page"`)
			}
			fmt.Fprintf(&sb, `>%s%s%s</a>`, before, html.EscapeString(it.Label), after)
			if len(it.Children) > 0 {
				sb.WriteString("\n<ul class=\"sub-menu\">\n")
				walk(it.Children, depth+1)
				sb.WriteString("</ul>\n")
			}
			sb.WriteString("</li>\n")
		}
	}
	walk(items, 0)
	return template.HTML(fmt.Sprintf(`<div class="%s"><ul id="menu-%s" class="%s">%s</ul></div>`, container, class, class, sb.String()))
}

// templateView is the copy of v that theme templates execute against. Templates (package themes are
// admin-installed and code-free) can read every exported field, so the store objects they reach carry
// only public data: no password hash, e-mail, raw meta, roles, capabilities or post password. Go code
// keeps the full objects on v.
func (v *siteView) templateView() *siteView {
	if v == nil {
		return nil
	}
	tv := *v
	if v.Q != nil {
		q := *v.Q
		q.Author = publicUser(q.Author)
		q.Post = publicPost(q.Post)
		q.PostsPage = publicPost(q.PostsPage)
		tv.Q = &q
	}
	tv.Post = v.Post.templateView()
	if v.Posts != nil {
		tv.Posts = make([]*postView, len(v.Posts))
		for i, pv := range v.Posts {
			tv.Posts[i] = pv.templateView()
		}
	}
	return &tv
}

func (pv *postView) templateView() *postView {
	if pv == nil {
		return nil
	}
	c := *pv
	c.Post = publicPost(pv.Post)
	c.AuthorEmail = ""
	return &c
}

// publicPost is p without its password (HasPassword / post_password_required say whether it has one).
func publicPost(p *store.Post) *store.Post {
	if p == nil || p.Password == "" {
		return p
	}
	c := *p
	c.Password = ""
	return &c
}

// publicUser keeps what WordPress shows of an author on the front end: the ids and names already in
// author URLs, the display name, website and the public profile fields.
func publicUser(u *store.User) *store.User {
	if u == nil {
		return nil
	}
	pub := &store.User{ID: u.ID, Login: u.Login, Nicename: u.Nicename, DisplayName: u.DisplayName, URL: u.URL,
		RegisteredAt: u.RegisteredAt}
	for _, k := range []string{"description", "first_name", "last_name", "nickname"} {
		if val, ok := u.Profile[k]; ok {
			if pub.Profile == nil {
				pub.Profile = map[string]string{}
			}
			pub.Profile[k] = val
		}
	}
	return pub
}
