package server

import (
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
)

// wpHead reproduces what wp_head() prints on a fresh install; the theme adds its enqueued assets.
func (s *Server) wpHead(c *gin.Context, q *siteQuery, lb *links.Builder, adminBar bool, v *siteView) template.HTML {
	site := html.EscapeString(v.SiteName)
	var sb strings.Builder
	if v.GWE == nil {
		sb.WriteString(v.Theme.HeadTop)
	}
	if s.settings.Bool("blog_public") && q.Kind != "404" && !q.Preview {
		sb.WriteString("<meta name='robots' content='max-image-preview:large' />\n")
	} else {
		sb.WriteString("<meta name='robots' content='noindex, nofollow' />\n")
	}
	fmt.Fprintf(&sb, "<link rel=\"alternate\" type=\"application/rss+xml\" title=\"%s &raquo; Feed\" href=\"%s\" />\n", site, html.EscapeString(lb.FeedLink("")))
	fmt.Fprintf(&sb, "<link rel=\"alternate\" type=\"application/rss+xml\" title=\"%s &raquo; Comments Feed\" href=\"%s\" />\n", site, html.EscapeString(lb.FeedLink("comments_rss2")))
	if (q.Kind == "single" || q.Kind == "page") && q.Post != nil && (s.commentsOpen(q.Post) || q.Post.PingStatus == "open" || q.Post.CommentCount > 0) {
		fmt.Fprintf(&sb, "<link rel=\"alternate\" type=\"application/rss+xml\" title=\"%s &raquo; %s Comments Feed\" href=\"%s\" />\n",
			site, html.EscapeString(q.Post.Title), html.EscapeString(lb.PostCommentsFeed(q.Post)))
	}
	if q.Term != nil {
		fmt.Fprintf(&sb, "<link rel=\"alternate\" type=\"application/rss+xml\" title=\"%s &raquo; %s %s Feed\" href=\"%s\" />\n",
			site, html.EscapeString(q.Term.Name), map[string]string{"category": "Category", "post_tag": "Tag"}[q.Term.Taxonomy],
			html.EscapeString(strings.TrimSuffix(lb.TermLink(q.Term), "/")+"/feed/"))
	}
	if adminBar {
		sb.WriteString("<link rel='stylesheet' id='dashicons-css' href='/wp-includes/css/dashicons.min.css' media='all' />\n")
		sb.WriteString("<link rel='stylesheet' id='admin-bar-css' href='/wp-includes/css/admin-bar.min.css' media='all' />\n")
		sb.WriteString("<link rel='stylesheet' id='wpgo-brand-css' href='/wp-includes/css/wpgo-brand.css?ver=" + Version + "' media='all' />\n")
		sb.WriteString("<style id='admin-bar-inline-css'>\n@media screen { html { margin-top: 32px !important; } }\n@media screen and ( max-width: 782px ) { html { margin-top: 46px !important; } }\n</style>\n")
	}
	sb.WriteString("<link rel='stylesheet' id='wp-block-library-css' href='/wp-includes/css/dist/block-library/style.min.css' media='all' />\n")
	sb.WriteString("<style id='wp-block-library-theme-inline-css'>\n" + s.webFile("wp-includes/css/dist/block-library/theme.min.css") + "\n</style>\n")
	sb.WriteString("<style id='classic-theme-styles-inline-css'>\n" + s.webFile("wp-includes/css/classic-themes.min.css") + "\n</style>\n")
	sb.WriteString("<style id='global-styles-inline-css'>\n" + s.webFile("wp-includes/css/global-styles-classic.css") + "\n</style>\n")
	if v.GWE != nil { // Golementor theme parts: Hello Golementor's stylesheet instead of the theme's own assets
		sb.WriteString(s.helloHead(c, v))
	} else if v.Theme.Head != nil {
		sb.WriteString(v.Theme.Head(s, c, v))
	}
	sb.WriteString("<link rel=\"https://api.w.org/\" href=\"" + html.EscapeString(lb.HomeURL("/wp-json/")) + "\" />\n")
	sb.WriteString("<meta name=\"generator\" content=\"WordPress Go " + Version + "\" />\n")
	if (q.Kind == "single" || q.Kind == "page" || q.Kind == "front-page") && q.Post != nil {
		fmt.Fprintf(&sb, "<link rel=\"canonical\" href=\"%s\" />\n", html.EscapeString(lb.Permalink(q.Post)))
		short := "/?p="
		if q.Post.Type == "page" {
			short = "/?page_id="
		}
		fmt.Fprintf(&sb, "<link rel='shortlink' href='%s' />\n", html.EscapeString(lb.HomeURL(short+itoa64(q.Post.ID))))
	}
	sb.WriteString(string(s.gweHead(c))) // Golementor frontend styles
	sb.WriteString(customizeHead(v))     // custom background, site icon, Additional CSS
	return template.HTML(sb.String())
}

// wpFooter is wp_footer(): comment-reply on singular views with threaded comments, and the admin bar script.
func (s *Server) wpFooter(c *gin.Context, q *siteQuery, lb *links.Builder, adminBar bool) template.HTML {
	var sb strings.Builder
	if (q.Kind == "single" || q.Kind == "page" || q.Kind == "front-page") && q.Post != nil && s.commentsOpen(q.Post) && s.settings.Bool("thread_comments") {
		sb.WriteString("<script src=\"/wp-includes/js/comment-reply.min.js?ver=" + Version + "\" id=\"comment-reply-js\" async data-wp-strategy=\"async\"></script>\n")
	}
	if adminBar {
		sb.WriteString("<script src=\"/wp-includes/js/hoverintent-js.min.js?ver=2.2.1\" id=\"hoverintent-js-js\"></script>\n") // admin-bar.js dependency
		sb.WriteString("<script src=\"/wp-includes/js/admin-bar.min.js?ver=" + Version + "\" id=\"admin-bar-js\"></script>\n")
	}
	sb.WriteString(string(s.gweFooter(c))) // Golementor frontend scripts
	return template.HTML(sb.String())
}

// webFile returns an embedded asset as text (inline styles).
func (s *Server) webFile(name string) string {
	b, err := fs.ReadFile(s.web, name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// frontAdminBar is the toolbar shown on the site to logged-in users (wp_admin_bar_render()).
func (s *Server) frontAdminBar(c *gin.Context, q *siteQuery, lb *links.Builder, u *store.User) template.HTML {
	esc := html.EscapeString
	site := esc(s.settings.String("blogname"))
	var sb strings.Builder
	sb.WriteString(`<div id="wpadminbar" class="nojq nojs">
	<a class="screen-reader-shortcut" href="#wp-toolbar" tabindex="1">Skip to toolbar</a>
	<div class="quicklinks" id="wp-toolbar" role="navigation" aria-label="Toolbar">
		<ul role='menu' id='wp-admin-bar-root-default' class="ab-top-menu">`)
	sb.WriteString(`<li role='group' id='wp-admin-bar-wp-logo'><a class='ab-item' role="menuitem" href='/wp-admin/about'><span class="ab-icon" aria-hidden="true"></span><span class="screen-reader-text">About WordPress Go</span></a></li>`)
	fmt.Fprintf(&sb, `<li role='group' id='wp-admin-bar-site-name' class="menupop"><a class='ab-item' role="menuitem" aria-expanded="false" href='/wp-admin/'>%s</a><div class="ab-sub-wrapper"><ul role='menu' id='wp-admin-bar-site-name-default' class="ab-submenu">`, site)
	if u.Can("read", nil) {
		sb.WriteString(`<li role='group' id='wp-admin-bar-dashboard'><a class='ab-item' role="menuitem" href='/wp-admin/'>Dashboard</a></li>`)
	}
	sb.WriteString(`</ul>`)
	if u.Can("switch_themes", nil) {
		sb.WriteString(`<ul role='menu' id='wp-admin-bar-appearance' class="ab-submenu"><li role='group' id='wp-admin-bar-themes'><a class='ab-item' role="menuitem" href='/wp-admin/themes'>Themes</a></li><li role='group' id='wp-admin-bar-widgets'><a class='ab-item' role="menuitem" href='/wp-admin/widgets'>Widgets</a></li><li role='group' id='wp-admin-bar-menus'><a class='ab-item' role="menuitem" href='/wp-admin/nav-menus'>Menus</a></li></ul>`)
	}
	sb.WriteString(`</div></li>`)
	if u.Can("edit_theme_options", nil) {
		sb.WriteString(`<li role='group' id='wp-admin-bar-customize' class="hide-if-no-customize"><a class='ab-item' role="menuitem" href='/wp-admin/customize'>Customize</a></li>`)
	}
	if u.Can("moderate_comments", nil) {
		var pending int
		_ = s.store.Pool.QueryRow(c, `SELECT count(*) FROM comments WHERE status='pending'`).Scan(&pending)
		fmt.Fprintf(&sb, `<li role='group' id='wp-admin-bar-comments'><a class='ab-item' role="menuitem" href='/wp-admin/edit-comments'><span class="ab-icon" aria-hidden="true"></span><span class="ab-label awaiting-mod pending-count count-%d" aria-hidden="true">%d</span><span class="screen-reader-text comments-in-moderation-text">%d Comments in moderation</span></a></li>`, pending, pending, pending)
	}
	if u.Can("edit_posts", nil) {
		sb.WriteString(`<li role='group' id='wp-admin-bar-new-content' class="menupop"><a class='ab-item' role="menuitem" aria-expanded="false" href='/wp-admin/post-new'><span class="ab-icon" aria-hidden="true"></span><span class="ab-label">New</span></a><div class="ab-sub-wrapper"><ul role='menu' id='wp-admin-bar-new-content-default' class="ab-submenu"><li role='group' id='wp-admin-bar-new-post'><a class='ab-item' role="menuitem" href='/wp-admin/post-new'>Post</a></li>`)
		if u.Can("upload_files", nil) {
			sb.WriteString(`<li role='group' id='wp-admin-bar-new-media'><a class='ab-item' role="menuitem" href='/wp-admin/media-new'>Media</a></li>`)
		}
		if u.Can("edit_pages", nil) {
			sb.WriteString(`<li role='group' id='wp-admin-bar-new-page'><a class='ab-item' role="menuitem" href='/wp-admin/post-new?post_type=page'>Page</a></li>`)
		}
		if u.Can("create_users", nil) {
			sb.WriteString(`<li role='group' id='wp-admin-bar-new-user'><a class='ab-item' role="menuitem" href='/wp-admin/user-new'>User</a></li>`)
		}
		sb.WriteString(`</ul></div></li>`)
	}
	if q.Post != nil && u.Can("edit_post", q.Post) {
		label := "Edit Post"
		if q.Post.Type == "page" {
			label = "Edit Page"
		}
		fmt.Fprintf(&sb, `<li role='group' id='wp-admin-bar-edit'><a class='ab-item' role="menuitem" href='%s'>%s</a></li>`, esc(links.EditLink(q.Post.ID)), label)
	} else if q.Term != nil && u.Can("manage_categories", nil) {
		label := map[string]string{"category": "Edit Category", "post_tag": "Edit Tag"}[q.Term.Taxonomy]
		fmt.Fprintf(&sb, `<li role='group' id='wp-admin-bar-edit'><a class='ab-item' role="menuitem" href='/wp-admin/term?taxonomy=%s&amp;tag_ID=%d'>%s</a></li>`, q.Term.Taxonomy, q.Term.ID, label)
	}
	sb.WriteString(`</ul><ul role='menu' id='wp-admin-bar-top-secondary' class="ab-top-secondary ab-top-menu">`)
	sb.WriteString(`<li role='group' id='wp-admin-bar-search' class="admin-bar-search"><div class="ab-item ab-empty-item" tabindex="-1" role="menuitem"><form action="` + esc(lb.HomeURL("/")) + `" method="get" id="adminbarsearch"><input class="adminbar-input" name="s" id="adminbar-search" type="text" value="` + esc(q.Search) + `" maxlength="150" /><label for="adminbar-search" class="screen-reader-text">Search</label><input type="submit" class="adminbar-button" value="Search" /></form></div></li>`)
	fmt.Fprintf(&sb, `<li role='group' id='wp-admin-bar-my-account' class="menupop with-avatar"><a class='ab-item' role="menuitem" aria-expanded="false" href='/wp-admin/profile'>Howdy, <span class="display-name">%s</span><img alt='' src='%s' srcset='%s 2x' class='avatar avatar-26 photo' height='26' width='26' decoding='async'/></a><div class="ab-sub-wrapper"><ul role='menu' id='wp-admin-bar-user-actions' class="ab-submenu"><li role='group' id='wp-admin-bar-user-info'><a class='ab-item' tabindex="-1" role="menuitem" href='/wp-admin/profile'><img alt='' src='%s' srcset='%s 2x' class='avatar avatar-64 photo' height='64' width='64' decoding='async'/><span class='display-name'>%s</span><span class='display-name edit-profile'>Edit Profile</span></a></li><li role='group' id='wp-admin-bar-logout'><a class='ab-item' role="menuitem" href='/wp-login?action=logout&amp;_wpnonce=%s'>Log Out</a></li></ul></div></li>`,
		esc(u.DisplayName), esc(s.avatarURL(c, u.Email, 26)), esc(s.avatarURL(c, u.Email, 52)), esc(s.avatarURL(c, u.Email, 64)), esc(s.avatarURL(c, u.Email, 128)), esc(u.DisplayName), s.nonce(c, "log-out"))
	sb.WriteString(`</ul></div></div>`)
	return template.HTML(sb.String())
}
