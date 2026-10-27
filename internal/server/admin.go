package server

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// adminView is the data every admin screen template receives.
type adminView struct {
	Title       string // page title without the " ‹ Site — WordPress" suffix
	BodyClass   string
	Screen      string // current file, e.g. "edit", used to highlight the menu
	Styles      []string
	Menu        []*menuTop
	User        *store.User
	Avatar26    string
	Avatar64    string
	SiteName    string
	HomeURL     string
	Notices     []notice
	Pending     int // comments awaiting moderation
	LogoutNonce string
	Data        any

	PageNow, TypeNow, AdminPage string // the pagenow/typenow/adminpage globals admin scripts read

	HeadScripts    template.HTML
	FooterScripts  template.HTML
	ExtraStyles    template.HTML
	ExtraBodyHTML  template.HTML // markup printed before the footer scripts (media templates, editor data)
	CacheBar       template.HTML // wpgo-cache toolbar node
	LateFooterHTML template.HTML // markup printed after the footer scripts (TinyMCE settings, link dialog)
	Help           *helpTab      // contextual Help panel (screen-meta), nil when the screen has none
	Color          adminColor    // the user's admin color scheme
}

type notice struct {
	Type        string // success, error, warning, info
	Message     template.HTML
	Dismissible bool
}

type menuTop struct {
	Slug        string // file, e.g. "edit"
	Title       template.HTML
	ID          string
	Icon        string
	Classes     string
	Current     bool
	Sub         []*menuSub
	Separator   bool
	First, Last bool
}

type menuSub struct {
	Slug    string
	Title   template.HTML
	Current bool
	First   bool
}

// menuDef mirrors wp-admin/menu: top-level item, capability and its submenu.
type menuDef struct {
	slug, title, cap, id, icon string
	sub                        []subDef
	separator                  bool
}

type subDef struct {
	slug, title, cap string
	match            []string // extra screens that highlight this item
}

func adminMenu(pending int, golementor bool) []menuDef {
	comments := "Comments"
	if pending > 0 {
		comments = fmt.Sprintf(`Comments <span class="awaiting-mod count-%d"><span class="pending-count" aria-hidden="true">%d</span><span class="comments-in-moderation-text screen-reader-text">%d Comments in moderation</span></span>`, pending, pending, pending)
	} else {
		comments = `Comments <span class="awaiting-mod count-0"><span class="pending-count" aria-hidden="true">0</span><span class="comments-in-moderation-text screen-reader-text">0 Comments in moderation</span></span>`
	}
	menu := []menuDef{
		{slug: "", title: "Dashboard", cap: "read", id: "menu-dashboard", icon: "dashicons-dashboard", sub: []subDef{
			{slug: "", title: "Home", cap: "read"},
			{slug: "update-core", title: `Updates <span class="update-plugins count-0"><span class="update-count">0</span></span>`, cap: "update_core"},
		}},
		{separator: true, cap: "read"},
		{slug: "edit", title: "Posts", cap: "edit_posts", id: "menu-posts", icon: "dashicons-admin-post", sub: []subDef{
			{slug: "edit", title: "All Posts", cap: "edit_posts", match: []string{"post?post_type=post"}},
			{slug: "post-new", title: "Add Post", cap: "edit_posts"},
			{slug: "edit-tags?taxonomy=category", title: "Categories", cap: "manage_categories", match: []string{"term?taxonomy=category"}},
			{slug: "edit-tags?taxonomy=post_tag", title: "Tags", cap: "manage_categories", match: []string{"term?taxonomy=post_tag"}},
		}},
		{slug: "upload", title: "Media", cap: "upload_files", id: "menu-media", icon: "dashicons-admin-media", sub: []subDef{
			{slug: "upload", title: "Library", cap: "upload_files"},
			{slug: "media-new", title: "Add Media File", cap: "upload_files"},
		}},
		{slug: "edit?post_type=page", title: "Pages", cap: "edit_pages", id: "menu-pages", icon: "dashicons-admin-page", sub: []subDef{
			{slug: "edit?post_type=page", title: "All Pages", cap: "edit_pages", match: []string{"post?post_type=page"}},
			{slug: "post-new?post_type=page", title: "Add Page", cap: "edit_pages"},
		}},
		{slug: "edit-comments", title: comments, cap: "edit_posts", id: "menu-comments", icon: "dashicons-admin-comments"},
		{separator: true, cap: "read"},
		{slug: "gowebeditor-pro?tab=popups", title: "Golementor", cap: "edit_posts", id: "menu-gowebeditor", icon: "dashicons-layout", sub: []subDef{ // Golementor Pro
			{slug: "gowebeditor-pro?tab=popups", title: "Popups", cap: "edit_posts"},
			{slug: "gowebeditor-pro?tab=loops", title: "Loop Items", cap: "edit_posts"}, // gap-loops
			{slug: "gowebeditor-pro?tab=submissions", title: "Form Submissions", cap: "manage_options"},
			{slug: "gowebeditor-pro?tab=code", title: "Custom Code", cap: "manage_options"},
			{slug: "gowebeditor-pro?tab=fonts", title: "Custom Fonts", cap: "manage_options"},
			{slug: "gowebeditor-icons", title: "Custom Icons", cap: "manage_options"}, // gap-widgets
			{slug: "gowebeditor-pro?tab=roles", title: "Role Manager", cap: "manage_options"},
			{slug: "gowebeditor-pro?tab=settings", title: "Settings", cap: "manage_options"},
		}},
		{slug: "themes", title: "Appearance", cap: "switch_themes", id: "menu-appearance", icon: "dashicons-admin-appearance", sub: []subDef{
			{slug: "themes", title: "Themes", cap: "switch_themes"},
			{slug: "customize", title: "Customize", cap: "edit_theme_options"},
			{slug: "widgets", title: "Widgets", cap: "edit_theme_options"},
			{slug: "nav-menus", title: "Menus", cap: "edit_theme_options"},
			{slug: "gowebeditor-theme-builder", title: "Theme Builder", cap: "edit_theme_options"}, // Golementor
			{slug: "theme-editor", title: "Theme File Editor", cap: "edit_themes"},
		}},
		{slug: "plugins", title: `Plugins <span class="update-plugins count-0"><span class="plugin-count">0</span></span>`, cap: "activate_plugins", id: "menu-plugins", icon: "dashicons-admin-plugins", sub: []subDef{
			{slug: "plugins", title: "Installed Plugins", cap: "activate_plugins"},
			{slug: "plugin-install", title: "Add Plugin", cap: "install_plugins"},
			{slug: "plugin-editor", title: "Plugin File Editor", cap: "edit_plugins"},
		}},
		{slug: "users", title: "Users", cap: "list_users", id: "menu-users", icon: "dashicons-admin-users", sub: []subDef{
			{slug: "users", title: "All Users", cap: "list_users"},
			{slug: "user-new", title: "Add User", cap: "create_users"},
			{slug: "profile", title: "Profile", cap: "read"},
		}},
		{slug: "profile", title: "Profile", cap: "read", id: "menu-users", icon: "dashicons-admin-users", sub: []subDef{
			{slug: "profile", title: "Profile", cap: "read"},
		}},
		{slug: "tools", title: "Tools", cap: "edit_posts", id: "menu-tools", icon: "dashicons-admin-tools", sub: []subDef{
			{slug: "tools", title: "Available Tools", cap: "edit_posts"},
			{slug: "import", title: "Import", cap: "import"},
			{slug: "export", title: "Export", cap: "export"},
			{slug: "site-health", title: "Site Health", cap: "manage_options"},
			{slug: "export-personal-data", title: "Export Personal Data", cap: "manage_options"},
			{slug: "erase-personal-data", title: "Erase Personal Data", cap: "manage_options"},
		}},
		{slug: "admin?page=wpgo-cache", title: "WPGo Cache", cap: "manage_options", id: "toplevel_page_wpgo-cache", icon: "dashicons-performance", sub: []subDef{
			{slug: "admin?page=wpgo-cache", title: "Dashboard", cap: "manage_options"},
			{slug: "admin?page=wpgo-cache-settings", title: "Cache", cap: "manage_options"},
			{slug: "admin?page=wpgo-cache-optimize", title: "Page Optimization", cap: "manage_options"},
			{slug: "admin?page=wpgo-cache-toolbox", title: "Toolbox", cap: "manage_options"},
		}},
		{slug: "options-general", title: "Settings", cap: "manage_options", id: "menu-settings", icon: "dashicons-admin-settings", sub: []subDef{
			{slug: "options-general", title: "General", cap: "manage_options"},
			{slug: "options-writing", title: "Writing", cap: "manage_options"},
			{slug: "options-reading", title: "Reading", cap: "manage_options"},
			{slug: "options-discussion", title: "Discussion", cap: "manage_options"},
			{slug: "options-media", title: "Media", cap: "manage_options"},
			{slug: "options-permalink", title: "Permalinks", cap: "manage_options"},
			{slug: "options-privacy", title: "Privacy", cap: "manage_privacy_options"},
		}},
	}
	if golementor {
		return menu
	}
	// Golementor is off: its top-level menu and the Theme Builder submenu are not part of the admin.
	out := menu[:0]
	for _, d := range menu {
		if d.id == "menu-gowebeditor" {
			continue
		}
		if d.id == "menu-appearance" {
			subs := make([]subDef, 0, len(d.sub))
			for _, sd := range d.sub {
				if sd.slug != "gowebeditor-theme-builder" {
					subs = append(subs, sd)
				}
			}
			d.sub = subs
		}
		out = append(out, d)
	}
	return out
}

// buildMenu filters the menu by capability and marks the current screen, like _wp_menu_output().
func buildMenu(u *store.User, screen string, pending int, golementor bool) []*menuTop {
	var out []*menuTop
	for _, d := range adminMenu(pending, golementor) {
		if d.separator {
			out = append(out, &menuTop{Separator: true})
			continue
		}
		// "Users" for list_users, otherwise the reduced "Profile" menu.
		if d.slug == "profile" && u.Can("list_users", nil) {
			continue
		}
		cap := d.cap
		if cap == "manage_privacy_options" {
			cap = "manage_options"
		}
		if !u.Can(cap, nil) {
			continue
		}
		top := &menuTop{Slug: d.slug, Title: template.HTML(d.title), ID: d.id, Icon: d.icon}
		for i, sd := range d.sub {
			c := sd.cap
			if c == "manage_privacy_options" {
				c = "manage_options"
			}
			if !u.Can(c, nil) {
				continue
			}
			ms := &menuSub{Slug: sd.slug, Title: template.HTML(sd.title), First: i == 0}
			if sd.slug == screen {
				ms.Current = true
			}
			for _, m := range sd.match {
				if m == screen {
					ms.Current = true
				}
			}
			if ms.Current {
				top.Current = true
			}
			top.Sub = append(top.Sub, ms)
		}
		if d.slug == screen {
			top.Current = true
		}
		out = append(out, top)
	}
	// Drop separators that ended up adjacent or trailing, and mark first/last items per group.
	var cleaned []*menuTop
	for i, m := range out {
		if m.Separator && (len(cleaned) == 0 || cleaned[len(cleaned)-1].Separator || i == len(out)-1) {
			continue
		}
		cleaned = append(cleaned, m)
	}
	for i, m := range cleaned {
		if m.Separator {
			continue
		}
		m.First = i == 0 || cleaned[i-1].Separator
		m.Last = i == len(cleaned)-1 || cleaned[i+1].Separator
	}
	return cleaned
}

// screenStyles lists the extra stylesheets a screen loads after WordPress's core admin bundle
// (the wp-admin style handle: common, forms, list-tables, edit, media, themes, nav-menus, … — see
// partials/layout.html). Paths are relative to /wp-admin/assets/css/ unless they start with "/".
var screenStyles = map[string][]string{
	"admin/dashboard":            {"/wp-admin/css/site-health.min.css"},
	"admin/posts-list":           {},
	"admin/post-edit":            {},
	"admin/terms":                {},
	"admin/term-edit":            {},
	"admin/options":              {},
	"admin/profile":              {},
	"admin/not-yet":              {},
	"admin/media-grid":           {"pages/media.css"},
	"admin/media-list":           {"pages/media.css"},
	"admin/media-new":            {"pages/media.css"},
	"admin/attachment-edit":      {"pages/media.css"},
	"admin/plugins":              {},
	"admin/themes":               {},
	"admin/comments-list":        {"pages/comments.css"},
	"admin/comment-edit":         {"pages/comments.css"},
	"admin/users-list":           {"pages/users.css"},
	"admin/user-new":             {"pages/users.css"},
	"admin/user-delete":          {"pages/users.css"},
	"admin/update-core":          {},
	"admin/about":                {},
	"admin/tools":                {},
	"admin/options-privacy":      {},
	"admin/privacy-policy-guide": {},
	"admin/theme-install":        {"wpgo/theme-install.css"},
	"admin/site-health":          {"/wp-admin/css/site-health.min.css"},
	"admin/export":               {"wpgo/tools.css"},
	"admin/file-editor":          {},
	"admin/privacy-tool":         {},
	"admin/import":               {},
	"admin/plugin-install":       {"wpgo/plugin-install.css"},
	"admin/wpgo-cache":           {},
	// Golementor Theme Builder
	"admin/gowebeditor-theme-builder":    {},
	"admin/gowebeditor-theme-conditions": {},
}

// adminPage renders an admin screen with the shared chrome.
func (s *Server) adminPage(c *gin.Context, screen, title, bodyClass, tmpl string, data any, notices ...notice) {
	u := currentUser(c)
	counts, _ := s.store.CommentCounts(c)
	v := &adminView{
		Title:     title,
		BodyClass: bodyClass,
		Screen:    screen,
		Menu:      buildMenu(u, screen, counts["pending"], s.golementorOn()),
		User:      u,
		Avatar26:  s.avatarURL(c, u.Email, 26),
		Avatar64:  s.avatarURL(c, u.Email, 64),
		SiteName:  s.settings.String("blogname"),
		HomeURL:   s.links(c).HomeURL("/"),
		Notices:   append(append(flashNotices(c), s.pluginNotices(c)...), notices...),
		Pending:   counts["pending"],
		Data:      data,

		LogoutNonce: s.nonce(c, "log-out"),
	}
	for _, css := range screenStyles[tmpl] {
		if strings.HasPrefix(css, "/") {
			v.Styles = append(v.Styles, css)
		} else {
			v.Styles = append(v.Styles, "/wp-admin/assets/css/"+css)
		}
	}
	v.AdminPage = strings.Fields(bodyClass + " index-php")[0]
	v.PageNow = strings.TrimSuffix(v.AdminPage, "-php")
	if strings.Contains(bodyClass, "post-type-") {
		for _, cl := range strings.Fields(bodyClass) {
			if strings.HasPrefix(cl, "post-type-") {
				v.TypeNow = strings.TrimPrefix(cl, "post-type-")
				if v.PageNow == "edit" {
					v.PageNow = "edit-" + v.TypeNow
				} else if v.PageNow == "post" {
					v.PageNow = v.TypeNow
				}
			}
		}
	}
	v.Color = adminColorFor(u)
	if tmpl != "admin/block-editor" && tmpl != "admin/dashboard" {
		v.Help = helpFor(v.AdminPage, v.TypeNow)
	}
	// Menu folding is a user setting kept in the wp-settings-{uid} cookie by utils.js (setUserSetting('mfold')).
	if ck, err := c.Cookie("wp-settings-" + itoa64(u.ID)); err == nil && strings.Contains("&"+ck+"&", "&mfold=f&") {
		v.BodyClass += " folded"
	}
	v.CacheBar = s.cacheAdminBar(c, u)
	a := s.pageAssets(c)
	a.Script("common", "admin-bar", "utils", "svg-painter")
	v.ExtraStyles = s.styleTags(a)
	v.HeadScripts = template.HTML(s.importMap()) + s.scriptTags(c, a, false)
	v.FooterScripts = s.scriptTags(c, a, true)
	if extra, ok := c.Get("wpgo.bodyHTML"); ok {
		v.ExtraBodyHTML = extra.(template.HTML)
	}
	if late, ok := c.Get("wpgo.lateFooterHTML"); ok {
		v.LateFooterHTML = late.(template.HTML)
	}
	s.renderAdminTemplate(c, http.StatusOK, tmpl, v)
}

// flashNotices turns WordPress-style query flags (?message=1, ?updated=1, ...) into notices.
func flashNotices(c *gin.Context) []notice {
	var out []notice
	if m := c.Query("wpgo-cache-msg"); m != "" {
		out = append(out, notice{Type: "success", Message: template.HTML("<strong>WPGo Cache:</strong> " + template.HTMLEscapeString(m)), Dismissible: true})
	}
	if c.Query("settings-updated") == "true" {
		out = append(out, notice{Type: "success", Message: "<strong>Settings saved.</strong>", Dismissible: true})
	}
	if c.Query("updated") == "1" && c.Request.URL.Path == "/wp-admin/profile" {
		out = append(out, notice{Type: "success", Message: "<strong>Profile updated.</strong>", Dismissible: true})
	}
	return out
}

// notYet keeps every wp-admin URL inside the chrome while its screen is still being ported.
func (s *Server) notYet(c *gin.Context) {
	screen := c.Param("screen")
	if strings.Contains(screen, ".") {
		c.Status(http.StatusNotFound)
		return
	}
	title := screen
	title = strings.ToUpper(title[:1]) + strings.ReplaceAll(title[1:], "-", " ")
	s.adminPage(c, screen, title, "", "admin/not-yet", map[string]string{"Screen": screen})
}

// adminAjax answers the admin-ajax.php actions the editor and screens rely on.
func (s *Server) adminAjax(c *gin.Context) {
	action := c.Query("action")
	if action == "" {
		action = c.PostForm("action")
	}
	if s.themeFileAjax(c, action) {
		return
	}
	if s.imageEditorAjax(c, action) {
		return
	}
	if s.menuAjax(c, action) {
		return
	}
	if s.mediaAjax(c, action) {
		return
	}
	if s.gweAjax(c, action) { // Golementor: elementor_ajax, heartbeat, elementor_js_log
		return
	}
	switch action {
	case "rest-nonce":
		c.String(http.StatusOK, s.nonce(c, "wp_rest"))
	case "sample-permalink":
		s.samplePermalinkAjax(c)
	case "wp-remove-post-lock":
		c.String(http.StatusOK, "1")
	default:
		c.String(http.StatusBadRequest, "0")
	}
}

// pageAssets returns the scripts and styles enqueued for the current request.
func (s *Server) pageAssets(c *gin.Context) *assets {
	if v, ok := c.Get("wpgo.assets"); ok {
		return v.(*assets)
	}
	a := newAssets()
	c.Set("wpgo.assets", a)
	return a
}

// addBodyHTML queues markup printed at the end of the admin page, before the footer scripts.
func addBodyHTML(c *gin.Context, h template.HTML) {
	if v, ok := c.Get("wpgo.bodyHTML"); ok {
		h = v.(template.HTML) + h
	}
	c.Set("wpgo.bodyHTML", h)
}
