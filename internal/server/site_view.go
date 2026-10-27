package server

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/blocks"
	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// siteView is the data the Twenty Sixteen templates receive.
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

	// GWE is set when the page renders through the Golementor theme shell (gowebeditor_theme.go).
	GWE *gweThemeView
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
	Categories, Tags                   []termLink
	CommentCount                       int
	CommentsOpen, HasPassword          bool
	CommentsURL                        string
	Thumbnail                          template.HTML
	ThumbnailURL                       string
}

func (s *Server) buildSiteView(c *gin.Context, q *siteQuery) (*siteView, error) {
	ctx := c.Request.Context()
	u := currentUser(c)
	lb := s.links(ctx)
	rc := &renderCtx{s: s, lb: lb, search: q.Search}
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
	s.gweThemeBegin(c, v) // Golementor: Site widgets read this view; previewed custom themes bring their kit
	v.IsFront = q.Paged <= 1 && (q.Kind == "front-page" || (q.Kind == "home" && q.PostsPage == nil))
	v.TitleIsH1 = q.Kind == "home" && q.PostsPage == nil
	v.IsHomeHeader = q.Kind == "home" && q.PostsPage != nil
	if q.PostsPage != nil {
		v.PostsPageTitle = q.PostsPage.Title
	}
	s.customizeView(c, v, cs)

	widgets, _ := s.store.WidgetsByArea(ctx, v.Theme.Slug)
	for area, ws := range widgets {
		v.Areas[area] = s.renderWidgetArea(ctx, ws)
	}
	v.Sidebar1 = v.Areas["sidebar-1"]
	for _, loc := range v.Theme.MenuLocations {
		if items, _ := s.store.MenuAt(ctx, loc.Slug); len(items) > 0 {
			s.resolveMenu(ctx, items, lb, q)
			v.Menus[loc.Slug] = items
		}
	}

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
		if p, err := s.store.GetPost(ctx, id); err == nil && p.Status == "publish" {
			v.PrivacyLink = template.HTML(fmt.Sprintf(`<a class="privacy-policy-link" href="%s" rel="privacy-policy">%s</a><span role="separator" aria-hidden="true"></span>`,
				html.EscapeString(lb.Permalink(p)), html.EscapeString(p.Title)))
		}
	}

	archiveTitle, plainTitle := q.archiveTitle()
	bodyClasses := []string{}

	switch q.Kind {
	case "single", "page", "front-page":
		v.Template = "single"
		if q.Post.Type == "page" {
			v.Template = "page"
		}
		pv := s.postView(ctx, c, q.Post, true, lb, u)
		v.Post = pv
		if q.Post.Type == "post" {
			v.PostNav = s.postNavigation(ctx, q.Post, lb)
		}
		if q.Post.CommentStatus == "open" || q.Post.CommentCount > 0 {
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
		} else {
			bodyClasses = append(bodyClasses, "post-template-default", "single", "single-post", "postid-"+itoa64(q.Post.ID), "single-format-"+q.Post.Format)
		}
	case "404":
		v.Template = "404"
		bodyClasses = append(bodyClasses, "error404")
	default:
		posts, total, perPage, err := s.mainPosts(ctx, q, u)
		if err != nil {
			return nil, err
		}
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
			case "author":
				bodyClasses = append(bodyClasses, "author", "author-"+q.Author.Nicename, "author-"+itoa64(q.Author.ID))
			case "date":
				bodyClasses = append(bodyClasses, "date")
			}
		}
		if q.Paged > 1 {
			bodyClasses = append(bodyClasses, "paged", "paged-"+itoa(q.Paged))
		}
	}

	if u != nil {
		bodyClasses = append(bodyClasses, "logged-in")
	}
	showBar := u != nil && cs == nil && !gwePreviewing(c) // the Customizer and Golementor previews have no toolbar
	if u != nil {
		if b, ok := u.Meta["show_admin_bar_front"].(bool); ok && !b {
			showBar = false // the user's "Show Toolbar" choice can hide it, never bring it back into a preview
		}
	}
	if showBar {
		bodyClasses = append(bodyClasses, "admin-bar", "no-customize-support")
	}
	bodyClasses = append(bodyClasses, customizeBodyClasses(v)...)
	bodyClasses = append(bodyClasses, s.gweBodyClasses(c, q)...)
	bodyClasses = append(bodyClasses, "wp-embed-responsive", "wp-theme-"+v.Theme.Slug)
	s.gweThemePrepare(c, v, &bodyClasses) // Golementor theme parts / custom theme (sets v.GWE)
	if v.Theme.Prepare != nil && v.GWE == nil {
		v.Theme.Prepare(s, c, v)
	}
	if v.Theme.BodyClasses != nil && v.GWE == nil {
		bodyClasses = append(bodyClasses, v.Theme.BodyClasses(s, v)...)
	}
	v.BodyClass = strings.Join(bodyClasses, " ")

	v.DocumentTitle = template.HTML(s.documentTitle(q, plainTitle))
	v.Head = s.wpHead(c, q, lb, showBar, v)
	v.BodyOpen = s.wpBodyOpen(c, v) // gap-widgets: wp_body_open (site_body_open.go)
	v.Footer = s.wpFooter(c, q, lb, showBar)
	if v.Theme.Footer != nil && v.GWE == nil {
		v.Footer += template.HTML(v.Theme.Footer(s, c, v))
	}
	if showBar {
		v.AdminBar = s.frontAdminBar(c, q, lb, u)
	}
	s.customizeFinish(c, v, cs)
	return v, nil
}

func (s *Server) multiAuthor(ctx context.Context) (bool, error) {
	var n int
	err := s.store.Pool.QueryRow(ctx, `SELECT count(DISTINCT author_id) FROM posts WHERE type='post' AND status='publish'`).Scan(&n)
	return n > 1, err
}

func (s *Server) archiveBase(q *siteQuery, lb *links.Builder) string {
	switch q.Kind {
	case "category", "tag":
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
	}
	if q.PostsPage != nil {
		return lb.Permalink(q.PostsPage)
	}
	return lb.HomeURL("/")
}

// paginateLinks is the_posts_pagination() with Twenty Sixteen's arguments (mid_size 1, end_size 1).
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
	prev, _ := s.store.Adjacent(ctx, p, true)
	next, _ := s.store.Adjacent(ctx, p, false)
	if prev == nil && next == nil {
		return ""
	}
	var sb strings.Builder
	if prev != nil {
		fmt.Fprintf(&sb, `<div class="nav-previous"><a href="%s" rel="prev"><span class="meta-nav" aria-hidden="true">Previous</span> <span class="screen-reader-text">Previous post:</span> <span class="post-title">%s</span></a></div>`,
			html.EscapeString(lb.Permalink(prev)), wp.Texturize(wp.EscText(prev.Title)))
	}
	if next != nil {
		fmt.Fprintf(&sb, `<div class="nav-next"><a href="%s" rel="next"><span class="meta-nav" aria-hidden="true">Next</span> <span class="screen-reader-text">Next post:</span> <span class="post-title">%s</span></a></div>`,
			html.EscapeString(lb.Permalink(next)), wp.Texturize(wp.EscText(next.Title)))
	}
	return template.HTML(`
	<nav class="navigation post-navigation" aria-label="Posts">
		<h2 class="screen-reader-text">Post navigation</h2>
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
	if out, ok := s.gweContent(c, p); ok { // Golementor documents and the editor preview
		return out
	}
	content := p.Content
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
	if blocks.HasBlocks(content) {
		content = blocks.Render(ctx, content)
	} else {
		content = wp.Autop(wp.Texturize(content))
	}
	content = s.doShortcodes(ctx, content) // plugin shortcodes (do_shortcode at priority 11)
	out, _ := s.applyFilters(ctx, "the_content", content, map[string]any{"id": p.ID, "type": p.Type, "is_single": singular}).(string)
	return out
}

// theExcerpt is the_excerpt() with Twenty Sixteen's "Continue reading" more link.
func (s *Server) theExcerpt(p *store.Post, lb *links.Builder) string {
	if p.Password != "" {
		return "<p>There is no excerpt because this is a protected post.</p>\n"
	}
	if p.Excerpt != "" {
		return wp.Autop(wp.Texturize(p.Excerpt))
	}
	more := ` &hellip; <a href="` + html.EscapeString(lb.Permalink(p)) + `" class="more-link">Continue reading<span class="screen-reader-text"> "` + theTitle(p) + `"</span></a>`
	text := stripBlocks(p.Content)
	text, _, _, _ = wp.SplitMore(text)
	return wp.Autop(wp.TrimWords(text, 55, more))
}

func (s *Server) postView(ctx context.Context, c *gin.Context, p *store.Post, singular bool, lb *links.Builder, u *store.User) *postView {
	pv := &postView{ID: p.ID, Type: p.Type, Title: template.HTML(theTitle(p)), Permalink: lb.Permalink(p), Sticky: p.Sticky}
	pv.Content = template.HTML(s.theContent(ctx, c, p, singular, lb))
	pv.ShowExcerpt = p.Excerpt != "" || (renderFrom(ctx) != nil && renderFrom(ctx).search != "")
	if pv.ShowExcerpt {
		pv.Excerpt = template.HTML(s.theExcerpt(p, lb))
	}

	cats, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "category")
	tags, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "post_tag")
	classes := []string{"post-" + itoa64(p.ID), p.Type, "type-" + p.Type, "status-" + p.Status}
	if p.Type == "post" {
		classes = append(classes, "format-"+p.Format)
	}
	if p.Password != "" {
		classes = append(classes, "post-password-required")
	}
	if p.Sticky && !singular {
		classes = append(classes, "sticky")
	}
	classes = append(classes, "hentry")
	for _, t := range cats[p.ID] {
		classes = append(classes, "category-"+t.Slug)
	}
	for _, t := range tags[p.ID] {
		classes = append(classes, "tag-"+t.Slug)
	}
	pv.Class = strings.Join(classes, " ")

	author, _ := s.store.UserByID(ctx, p.AuthorID)
	pv.EntryMeta = s.entryMeta(ctx, p, author, cats[p.ID], tags[p.ID], singular, lb)
	s.fillPostView(ctx, pv, p, author, cats[p.ID], tags[p.ID], lb)
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
	return fmt.Sprintf(`<img alt='' src='%s' srcset='%s 2x' class='avatar avatar-%d photo' height='%d' width='%d' loading='lazy' decoding='async'/>`,
		html.EscapeString(s.avatarFor(ctx, email, size)), html.EscapeString(s.avatarFor(ctx, email, size*2)), size, size, size)
}

// entryMeta is twentysixteen_entry_meta().
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
	if p.Type == "post" && p.Format != "" && p.Format != "standard" {
		fmt.Fprintf(&sb, `<span class="entry-format"><span class="screen-reader-text">Format </span><a href="%s">%s</a></span>`,
			html.EscapeString(lb.TermLink(&store.Term{Taxonomy: "post_format", Slug: "post-format-" + p.Format})), strings.ToUpper(p.Format[:1])+p.Format[1:])
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
	if !singular && p.Password == "" && (p.CommentStatus == "open" || p.CommentCount > 0) {
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

// categorizedBlog is twentysixteen_categorized_blog(): category links show only when more than one category is in use.
func (s *Server) categorizedBlog(ctx context.Context) bool {
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM terms WHERE taxonomy='category' AND post_count > 0 LIMIT 2) t`).Scan(&n)
	return n > 1
}

// renderWidgetArea is dynamic_sidebar() with Twenty Sixteen's before/after_widget markup.
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
		fmt.Fprintf(&sb, "<section id=\"block-%d\" class=\"widget %s\">%s</section>", w.ID, classes, blocks.Render(ctx, content))
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
			classes := "menu-item menu-item-type-" + it.Kind + " menu-item-object-" + it.Object + " menu-item-" + itoa64(it.ID)
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
			fmt.Fprintf(&sb, `<li id="menu-item-%d" class="%s"><a href="%s"`, it.ID, classes, html.EscapeString(it.URL))
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
