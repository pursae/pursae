package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/i18n"
	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Themes. A theme is a directory web/themes/<slug>/ with templates/ (Go html/template files that
// define "theme/index", "theme/single", "theme/page", "theme/archive", "theme/search" and "theme/404")
// and assets/ (served at /wp-content/themes/<slug>/), plus a Go file that registers a *Theme with
// the parts of functions.php that need code: enqueued styles and scripts, widget areas, menu
// locations, body classes and template tags.

// Theme describes an installed theme.
type Theme struct {
	Slug, Name, Version, Description string
	Author, AuthorURI, ThemeURI      string
	Tags                             []string
	Screenshot                       string // file under assets/, usually screenshot.png
	WidgetAreas                      []WidgetArea
	MenuLocations                    []MenuLocation
	// EditorStyles are stylesheets (relative to the theme URI) loaded into Gotenberg and TinyMCE,
	// like add_editor_style(); EditorBlockStyles go into the block editor canvas only.
	EditorStyles      []string
	EditorBlockStyles []string
	// Palette is the editor-color-palette theme support.
	Palette []map[string]string
	// PreloadFonts are font files (relative to the theme URI) wpgo-cache preloads to speed up first paint.
	PreloadFonts []string
	// Supports lists add_theme_support() features reported to the block editor (REST themes endpoint).
	Supports map[string]any
	// BlockStyles switches core's block styles to per-block loading (like a block theme with
	// should_load_separate_core_block_assets): wp_head no longer prints the whole block library, the
	// classic theme styles and the classic global styles; the core styles of the blocks a page uses
	// are inlined instead (blockStylesFor), except these blocks, which the theme's own stylesheet
	// carries. nil keeps the classic behaviour.
	BlockStyles []string
	// Settings are theme.json's settings (a theme with theme.json): the block editor gets its presets
	// and layout sizes, and EditorGlobalStyles (a stylesheet under assets/) as its global styles,
	// instead of a classic theme's 840px column and core's classic global styles.
	Settings           map[string]any
	EditorGlobalStyles string
	// Templates are the theme's page templates (file => "Template Name" header), get_page_templates().
	Templates map[string]string

	// HeadTop is printed first in wp_head (hooks at priority 0, e.g. a no-js class swap).
	HeadTop string
	// Head prints the theme's enqueued styles and scripts in wp_head, after core's block styles.
	Head func(s *Server, c *gin.Context, v *siteView) string
	// Footer prints scripts enqueued in the footer.
	Footer func(s *Server, c *gin.Context, v *siteView) string
	// Prepare fills theme-specific view data (template tags) once the core view is built.
	Prepare func(s *Server, c *gin.Context, v *siteView)
	// BodyClasses adds the theme's body_class filter output.
	BodyClasses func(s *Server, v *siteView) []string

	// Demos are a bundled theme's demo content sets (theme_demo_sets.go).
	Demos []themeDemoSet

	// Patterns is a JSON file under web/themes/<slug>/ with the theme's block patterns and pattern
	// categories as /wp/v2/block-patterns returns them (theme_patterns.go); "{{theme_uri}}" in a
	// pattern's content is the theme's URL.
	Patterns string

	// Package is set for a theme installed as a package (theme_package.go): its manifest and directory.
	Package *themePackage

	// Custom and ScreenshotURL are only set by the old page builder's saved themes; nothing in the
	// theme code reads Custom any more. Both go when that builder is deleted.
	Custom bool
	// ScreenshotURL overrides the screenshot file (an absolute upload URL).
	ScreenshotURL string
}

// ScreenshotSrc is the theme's screenshot URL (relative to the site).
func (t *Theme) ScreenshotSrc() string {
	if t.ScreenshotURL != "" {
		return t.ScreenshotURL
	}
	return t.URI() + "/" + t.Screenshot
}

// WidgetArea is register_sidebar().
type WidgetArea struct {
	ID, Name, Description                              string
	BeforeWidget, AfterWidget, BeforeTitle, AfterTitle string
}

// MenuLocation is register_nav_menus().
type MenuLocation struct {
	Slug, Name string
}

var (
	themesMu sync.RWMutex
	themes   = map[string]*Theme{}
)

// registerTheme adds a theme; theme files call it from init().
func registerTheme(t *Theme) {
	themesMu.Lock()
	themes[t.Slug] = t
	themesMu.Unlock()
}

// allThemes lists installed themes, sorted by name.
func allThemes() []*Theme {
	themesMu.RLock()
	defer themesMu.RUnlock()
	out := make([]*Theme, 0, len(themes))
	for _, t := range themes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func themeBySlug(slug string) *Theme {
	themesMu.RLock()
	defer themesMu.RUnlock()
	return themes[slug]
}

// themeFor is themeBySlug as this server sees it. The registry is shared by every Server in the
// process, except on a theme-preview sandbox (theme_preview.go): its package themes live in its own
// registry, so previewing a theme never installs it on the site, and it sees no package the site has
// installed; the bundled themes are shared.
func (s *Server) themeFor(slug string) *Theme {
	sb := s.sandbox
	if sb == nil {
		return themeBySlug(slug)
	}
	sb.mu.RLock()
	t := sb.themes[slug]
	sb.mu.RUnlock()
	if t != nil {
		return t
	}
	if t := themeBySlug(slug); t != nil && t.Package == nil {
		return t
	}
	return nil
}

// themeList is allThemes as this server sees it (see themeFor).
func (s *Server) themeList() []*Theme {
	sb := s.sandbox
	if sb == nil {
		return allThemes()
	}
	var out []*Theme
	for _, t := range allThemes() {
		if t.Package == nil {
			out = append(out, t)
		}
	}
	sb.mu.RLock()
	for _, t := range sb.themes {
		out = append(out, t)
	}
	sb.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// registerPackage puts a loaded package theme in this server's registry (registerThemePackage, or
// the sandbox's own).
func (s *Server) registerPackage(t *Theme) {
	sb := s.sandbox
	if sb == nil {
		registerThemePackage(t)
		return
	}
	sb.mu.Lock()
	sb.themes[t.Slug] = t
	sb.mu.Unlock()
}

// czSpecFor is the Customizer spec of a theme: a package carries its own (the registry's czSpecs
// entry is the same pointer on the site; a sandbox never writes czSpecs).
func czSpecFor(t *Theme) *czThemeSpec {
	if t == nil {
		return nil
	}
	if t.Package != nil {
		return t.Package.spec
	}
	czSpecsMu.RLock()
	defer czSpecsMu.RUnlock()
	return czSpecs[t.Slug]
}

// defaultTheme is WP_DEFAULT_THEME.
const defaultTheme = "twentytwentyfive"

// activeTheme is the theme in the stylesheet option, falling back to the default theme when that
// theme is not installed (or was deleted).
func (s *Server) activeTheme() *Theme {
	if t := s.installedTheme(s.settings.String("stylesheet")); t != nil {
		return t
	}
	return themeBySlug(defaultTheme)
}

// removedThemes are the bundled themes earlier versions shipped (Twenty Sixteen was the default).
var removedThemes = map[string]bool{"twentysixteen": true, "twentyseventeen": true, "twentytwenty": true, "twentytwentyone": true, "classic": true}

// migrateRemovedThemes moves a site whose active theme was one of the removed bundled themes to the
// default theme (activeTheme already falls back to it; this keeps the options and Themes screen
// consistent). Theme mods and widgets of the old theme are left in place.
func (s *Server) migrateRemovedThemes(ctx context.Context) {
	if !removedThemes[s.settings.String("stylesheet")] || s.installedTheme(s.settings.String("stylesheet")) != nil {
		return
	}
	t := themeBySlug(defaultTheme)
	if _, err := s.store.Pool.Exec(ctx, `UPDATE themes SET status = 'inactive' WHERE status = 'active'`); err != nil {
		log.Printf("themes: leaving a removed theme: %v", err)
		return
	}
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO themes (slug, version, status) VALUES ($1, $2, 'active')
		ON CONFLICT (slug) DO UPDATE SET status = 'active', version = EXCLUDED.version`, t.Slug, t.Version); err != nil {
		log.Printf("themes: activating %s: %v", t.Slug, err)
		return
	}
	if err := s.settings.Set(ctx, map[string]any{"template": t.Slug, "stylesheet": t.Slug, "current_theme": t.Name}); err != nil {
		log.Printf("themes: activating %s: %v", t.Slug, err)
	}
}

// themeURI is get_stylesheet_directory_uri().
func (t *Theme) URI() string { return "/wp-content/themes/" + t.Slug }

// widgetArea returns a registered area by id.
func (t *Theme) widgetArea(id string) *WidgetArea {
	for i := range t.WidgetAreas {
		if t.WidgetAreas[i].ID == id {
			return &t.WidgetAreas[i]
		}
	}
	return nil
}

// switchTheme is switch_theme(): it stores the new theme, carries widgets over to areas the new theme
// has not configured yet (like retrieve_widgets()) and purges the page cache.
func (s *Server) switchTheme(ctx context.Context, slug string) error {
	t := s.installedTheme(slug)
	if t == nil {
		return fmt.Errorf("theme %q is not installed", slug)
	}
	old := s.activeTheme()
	if err := s.ensureThemeWidgets(ctx, t, old); err != nil {
		return err
	}
	if _, err := s.store.Pool.Exec(ctx, `UPDATE themes SET status = 'inactive' WHERE status = 'active'`); err != nil {
		return err
	}
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO themes (slug, version, status) VALUES ($1, $2, 'active')
		ON CONFLICT (slug) DO UPDATE SET status = 'active', version = EXCLUDED.version`, t.Slug, t.Version); err != nil {
		return err
	}
	if err := s.settings.Set(ctx, map[string]any{"template": t.Slug, "stylesheet": t.Slug, "current_theme": t.Name}); err != nil {
		return err
	}
	s.configureCache() // preloads follow the theme; rebuilding also empties the cache
	if old != nil && old.Slug == wbHelloSlug && t.Slug != wbHelloSlug {
		s.wbForgetPreviousTheme(ctx) // Pursae Web Builder: leaving Hello Pursae ends what activation switched
	}
	if old == nil || old.Slug != t.Slug {
		s.asAudit(ctx, asEvent{Event: "theme_switched", Severity: "notice", Object: t.Name, Message: "Theme switched to " + t.Name + "."})
		s.hubEvent("theme_activate", "theme", t.Slug, t.Version) // statistics (hub_telemetry.go)
	}
	return nil
}

// ensureThemeWidgets registers the theme's widget areas and, when the theme has no widgets yet,
// copies the previous theme's widgets area by area (first area to first area, and so on).
func (s *Server) ensureThemeWidgets(ctx context.Context, t, from *Theme) error {
	ids := make([]int64, len(t.WidgetAreas))
	for i, a := range t.WidgetAreas {
		id, err := s.store.EnsureWidgetArea(ctx, t.Slug, a.ID, a.Name)
		if err != nil {
			return err
		}
		ids[i] = id
	}
	existing, err := s.store.WidgetsByArea(ctx, t.Slug)
	if err != nil || len(existing) > 0 || from == nil || from.Slug == t.Slug {
		return err
	}
	prev, err := s.store.WidgetsByArea(ctx, from.Slug)
	if err != nil {
		return err
	}
	declared := map[string]bool{}
	for i, a := range from.WidgetAreas {
		declared[a.ID] = true
		if i >= len(ids) {
			break
		}
		for n, w := range prev[a.ID] {
			if err := s.store.AddWidget(ctx, ids[i], w.Type, n, w.Settings); err != nil {
				return err
			}
		}
	}
	// Areas the previous theme kept without showing them (the install's sidebar-1 under the default
	// theme, which has no widget areas) go to the area of the same id, like retrieve_widgets().
	for i, a := range t.WidgetAreas {
		if declared[a.ID] || len(prev[a.ID]) == 0 {
			continue
		}
		for n, w := range prev[a.ID] {
			if err := s.store.AddWidget(ctx, ids[i], w.Type, n, w.Settings); err != nil {
				return err
			}
		}
	}
	return nil
}

// View helpers shared by themes ---------------------------------------------------

// termLink is a category or tag as theme templates print it.
type termLink struct {
	ID         int64
	Name, Slug string
	URL        string
}

// fillPostView adds the generic template-tag data every theme can use.
func (s *Server) fillPostView(ctx context.Context, pv *postView, p *store.Post, author *store.User, cats, tags []*store.Term, lb *links.Builder) {
	loc := s.loc()
	pv.Post = p
	pv.Format = store.CanonicalPostFormat(p.Format)
	pv.Date = p.Date().In(loc)
	pv.Modified = p.ModifiedAt.In(loc)
	df := s.settings.String("date_format")
	pv.DateText, pv.ModifiedText = wp.FormatDate(pv.Date, df), wp.FormatDate(pv.Modified, df)
	pv.DateISO, pv.ModifiedISO = pv.Date.Format(time.RFC3339), pv.Modified.Format(time.RFC3339)
	pv.CommentCount, pv.CommentsOpen = p.CommentCount, p.CommentStatus == "open"
	pv.CommentsURL = pv.Permalink + "#comments"
	pv.HasPassword = p.Password != ""
	if author != nil {
		// Templates get the avatar URL, never the author's e-mail (AuthorEmail stays empty).
		pv.AuthorName, pv.AuthorURL = author.DisplayName, lb.AuthorLink(author.ID, author.Nicename)
		pv.AuthorAvatar = s.avatarFor(ctx, author.Email, 96)
		pv.AuthorBio = author.Profile["description"]
	}
	for _, t := range cats {
		pv.Categories = append(pv.Categories, termLink{t.ID, t.Name, t.Slug, lb.TermLink(t)})
	}
	for _, t := range tags {
		pv.Tags = append(pv.Tags, termLink{t.ID, t.Name, t.Slug, lb.TermLink(t)})
	}
	if p.FeaturedMediaID > 0 {
		if a, err := s.attachment(ctx, p.FeaturedMediaID); err == nil { // request memo (site_memo.go)
			pv.ThumbnailURL = s.uploadURL(ctx, a.FilePath)
			pv.Thumbnail = s.attachmentImageAttrs(ctx, a, "post-thumbnail", "attachment-post-thumbnail size-post-thumbnail wp-post-image",
				thumbnailLoading(ctx))
		}
	}
}

// thumbnailLoading is the loading optimisation of a post thumbnail (wp_get_loading_optimization_attributes()):
// the page's first one is likely the largest paint and is not lazy-loaded (fetchpriority="high" unless
// a content image already has it), later ones are lazy-loaded; feeds get neither.
func thumbnailLoading(ctx context.Context) string {
	rc := renderFrom(ctx)
	if rc != nil && rc.feed {
		return ""
	}
	m := viewMemoFrom(ctx)
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.thumbSeen {
		m.thumbSeen = true
		if rc != nil && rc.priorityDone { // a content image above it already has the high priority
			return ""
		}
		return ` fetchpriority="high"`
	}
	return ` loading="lazy"`
}

// attachmentImage is wp_get_attachment_image(): the size closest to the requested one with srcset.
func (s *Server) attachmentImage(ctx context.Context, a *store.Attachment, size, class string) template.HTML {
	return s.attachmentImageAttrs(ctx, a, size, class, "")
}

// attachmentImageAttrs is attachmentImage with loading attributes (` loading="lazy"`, ` fetchpriority="high"`)
// after decoding="async", where WordPress prints them.
func (s *Server) attachmentImageAttrs(ctx context.Context, a *store.Attachment, size, class, loading string) template.HTML {
	src, w, h := s.uploadURL(ctx, a.FilePath), a.Width, a.Height
	if v := a.Variant(size); v != nil {
		src, w, h = s.uploadURL(ctx, v.FilePath), v.Width, v.Height
	} else if v := a.Variant("large"); v != nil && size != "full" {
		src, w, h = s.uploadURL(ctx, v.FilePath), v.Width, v.Height
	}
	var set []string
	set = append(set, fmt.Sprintf("%s %dw", s.uploadURL(ctx, a.FilePath), a.Width))
	for _, n := range []string{"medium", "medium_large", "large", "1536x1536", "2048x2048"} {
		if v := a.Variant(n); v != nil {
			set = append(set, fmt.Sprintf("%s %dw", s.uploadURL(ctx, v.FilePath), v.Width))
		}
	}
	return template.HTML(fmt.Sprintf(`<img width="%d" height="%d" src="%s" class="%s" alt="%s" decoding="async"%s srcset="%s" sizes="(max-width: %dpx) 100vw, %dpx" />`,
		w, h, html.EscapeString(src), class, html.EscapeString(a.Alt), loading, html.EscapeString(strings.Join(set, ", ")), w, w))
}

// Themes screen --------------------------------------------------------------------

type themeCard struct {
	*Theme
	Active        bool
	ScreenshotURL string
	ActivateURL   string
	PreviewURL    string
	// The active package theme's demo content (theme_demo.go): Import or Remove.
	DemoImportURL, DemoRemoveURL string
	// A newer version on Pursae Manager (hub_updates.go): Update now and the version.
	UpdateURL, NewVersion string
}

type themesData struct {
	Themes   []themeCard
	Active   themeCard
	Count    int
	Settings template.JS // window._wpThemeSettings for wp-admin/js/theme.js
}

// wpThemeJS is one entry of _wpThemeSettings.themes (wp_prepare_themes_for_js()).
type wpThemeJS struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Screenshot    []string        `json:"screenshot"`
	Description   string          `json:"description"`
	Author        string          `json:"author"`
	AuthorAndURI  string          `json:"authorAndUri"`
	Tags          string          `json:"tags"`
	Version       string          `json:"version"`
	CompatibleWP  bool            `json:"compatibleWP"`
	CompatiblePHP bool            `json:"compatiblePHP"`
	UpdateResp    map[string]bool `json:"updateResponse"`
	Parent        bool            `json:"parent"`
	Active        bool            `json:"active"`
	HasUpdate     bool            `json:"hasUpdate"`
	HasPackage    bool            `json:"hasPackage"`
	Update        string          `json:"update"`
	Autoupdate    map[string]any  `json:"autoupdate"`
	Actions       map[string]any  `json:"actions"`
	BlockTheme    bool            `json:"blockTheme"`
	// Demo content of the active package theme (theme_demo.go): the Import or the Remove URL.
	DemoImport string `json:"wpgoDemoImport,omitempty"`
	DemoRemove string `json:"wpgoDemoRemove,omitempty"`
	// An installed package with demo content: its preview with the demo (theme_preview.go).
	DemoPreview string `json:"wpgoDemoPreview,omitempty"`
	// A newer version on Pursae Manager: the Update now URL (hub_updates.go). hasPackage stays false so
	// theme.js leaves the link alone instead of calling wp.updates.
	UpdateURL string `json:"wpgoUpdate,omitempty"`
}

// themesScreen is themes.php.
func (s *Server) themesScreen(c *gin.Context) {
	if s.wbTemplatesPage(c) { // Appearance › Theme Builder (webbuilder_templates_admin.go)
		return
	}
	if s.wbLibraryPage(c) { // Web Builder › Theme Library (webbuilder_library_page.go)
		return
	}
	u := currentUser(c)
	if !u.Can("switch_themes", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit theme options on this site.")
		return
	}
	if c.Query("action") == "activate" {
		slug := c.Query("stylesheet")
		if !s.checkNonce(c, "switch-theme_"+slug) {
			s.denyNonce(c)
			return
		}
		if err := s.switchTheme(c, slug); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/themes?activated=true")
		return
	}
	if c.Query("action") == "upgrade-theme" { // a newer version from Pursae Manager (hub_updates.go)
		s.themeUpgrade(c)
		return
	}
	if c.Query("action") == "delete" { // delete_theme() (theme_delete.go)
		slug := c.Query("stylesheet")
		if !u.Can("delete_themes", nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to delete themes on this site.")
			return
		}
		if !s.checkNonce(c, "delete-theme_"+slug) {
			s.denyNonce(c)
			return
		}
		if err := s.deleteTheme(c, slug); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/themes?deleted=true")
		return
	}
	if c.Query("action") == "demo-content" { // a bundled theme's demo sets (theme_demo_sets.go)
		t := s.installedTheme(c.Query("stylesheet"))
		if !u.Can("import", nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to import content into this site.")
			return
		}
		if t == nil || len(t.Demos) == 0 {
			c.String(http.StatusBadRequest, "This theme has no demo content.")
			return
		}
		s.themeDemoChooser(c, t)
		return
	}
	if a := c.Query("action"); a == "import-demo" || a == "remove-demo" { // theme_demo.go
		slug := c.Query("stylesheet")
		if !u.Can("import", nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to import content into this site.")
			return
		}
		if !s.checkNonce(c, a+"_"+slug) {
			s.denyNonce(c)
			return
		}
		t := s.installedTheme(slug)
		if !themeHasDemo(t) {
			c.String(http.StatusBadRequest, "This theme has no demo content.")
			return
		}
		if len(t.Demos) > 0 {
			s.themeDemoAction(c, t, a)
			return
		}
		if a == "remove-demo" {
			if err := s.removeThemeDemo(c.Request.Context(), slug); err != nil {
				c.Redirect(http.StatusFound, "/wp-admin/themes?error="+url.QueryEscape("The demo content could not be removed: "+err.Error()))
				return
			}
			c.Redirect(http.StatusFound, "/wp-admin/themes?demo=removed")
			return
		}
		if s.activeTheme().Slug != slug {
			c.String(http.StatusBadRequest, "Activate the theme before importing its demo content.")
			return
		}
		rep, err := s.importThemeDemo(c.Request.Context(), u.ID, t)
		if err != nil {
			c.Redirect(http.StatusFound, "/wp-admin/themes?error="+url.QueryEscape("The demo content could not be imported: "+err.Error()))
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/themes?"+rep.query())
		return
	}
	active := s.activeTheme()
	demoURL := func(action, slug string) string {
		if t := themeBySlug(slug); t != nil && len(t.Demos) > 0 { // bundled sets: the chooser imports and removes
			return demoChooserURL(slug)
		}
		return "/wp-admin/themes?action=" + action + "&stylesheet=" + slug + "&_wpnonce=" + s.nonce(c, action+"_"+slug)
	}
	hasDemo := themeHasDemo(active) && u.Can("import", nil)
	demoImported := hasDemo && len(active.Demos) == 0 && s.demoImported(active.Slug)
	var d themesData
	var updates map[string]hubItem
	if u.Can("update_themes", nil) {
		updates = s.hubThemeUpdates(c)
	}
	for _, t := range s.installedThemes() {
		card := themeCard{Theme: t, Active: t.Slug == active.Slug, ScreenshotURL: t.ScreenshotSrc() + "?ver=" + t.Version,
			ActivateURL: "/wp-admin/themes?action=activate&stylesheet=" + t.Slug + "&_wpnonce=" + s.nonce(c, "switch-theme_"+t.Slug),
			PreviewURL:  "/?theme=" + t.Slug}
		if it, ok := updates[t.Slug]; ok {
			card.UpdateURL, card.NewVersion = s.themeUpdateURL(c, t.Slug), it.Version
		}
		if card.Active && hasDemo {
			if demoImported {
				card.DemoRemoveURL = demoURL("remove-demo", t.Slug)
			} else {
				card.DemoImportURL = demoURL("import-demo", t.Slug)
			}
		}
		if card.Active {
			d.Active = card
			d.Themes = append([]themeCard{card}, d.Themes...)
		} else {
			d.Themes = append(d.Themes, card)
		}
	}
	d.Count = len(d.Themes)
	var js []wpThemeJS
	for _, t := range d.Themes {
		author := template.HTMLEscapeString(t.Author)
		andURI := author
		if t.AuthorURI != "" {
			andURI = `<a href="` + template.HTMLEscapeString(t.AuthorURI) + `">` + author + `</a>`
		}
		customize := s.customizeURL()
		if !t.Active {
			customize = t.PreviewURL
		}
		j := wpThemeJS{ID: t.Slug, Name: template.HTMLEscapeString(t.Name), Screenshot: []string{strings.SplitN(t.ScreenshotURL, "?", 2)[0]},
			Description: template.HTMLEscapeString(t.Description), Author: author, AuthorAndURI: andURI,
			Tags: template.HTMLEscapeString(strings.Join(t.Tags, ", ")), Version: t.Version, CompatibleWP: true, CompatiblePHP: true,
			UpdateResp: map[string]bool{"compatibleWP": true, "compatiblePHP": true}, Active: t.Active,
			Autoupdate: map[string]any{"enabled": false, "supported": false, "forced": nil},
			Actions:    map[string]any{"activate": t.ActivateURL, "customize": customize, "delete": nil, "autoupdate": nil}}
		if t.UpdateURL != "" {
			name := template.HTMLEscapeString(t.Name)
			j.HasUpdate, j.UpdateURL = true, t.UpdateURL
			// One message, translated as a whole: themes.js shows it as it is.
			j.Update = `<p><strong>` + i18n.Sprintf(s.__(c, `There is a new version of %1$s available. <a href="%2$s" class="wpgo-update-theme" aria-label="%3$s">Update now</a> to version %4$s.`),
				name, template.HTMLEscapeString(t.UpdateURL), template.HTMLEscapeString(i18n.Sprintf(s.__(c, "Update %s now"), t.Name)), template.HTMLEscapeString(t.NewVersion)) + `</strong></p>`
		}
		if t.Active && hasDemo {
			if demoImported {
				j.DemoRemove = demoURL("remove-demo", t.Slug)
			} else {
				j.DemoImport = demoURL("import-demo", t.Slug)
			}
		}
		if t.Package != nil && t.Package.Manifest.Demo != "" && canPreview(u, false) {
			j.DemoPreview = previewURL(t.Slug, true)
		}
		if !t.Active && u.Can("delete_themes", nil) { // every inactive theme can be deleted (theme_delete.go)
			j.Actions["delete"] = "/wp-admin/themes?action=delete&stylesheet=" + t.Slug + "&_wpnonce=" + s.nonce(c, "delete-theme_"+t.Slug)
		}
		js = append(js, j)
	}
	settings, _ := json.Marshal(map[string]any{
		"themes": js,
		"settings": map[string]any{"canInstall": u.Can("install_themes", nil), "installURI": "/wp-admin/theme-install",
			"confirmDelete": "Are you sure you want to delete this theme?\n\nClick 'Cancel' to go back, 'OK' to confirm the delete.", "confirmRemoveDemo": "Remove the demo content? The pages, posts, images, categories, tags, menus and widgets it added are deleted permanently, and the settings it changed get their previous values back.", "adminUrl": "/wp-admin/"},
		"l10n": themesL10n(func(m string) string { return s.__(c, m) }),
	})
	d.Settings = template.JS(settings)
	a := s.pageAssets(c)
	a.Script("theme")
	// theme.js routes on themes.php; WordPress Go URLs have no .php.
	a.AddInline("theme", `wp.themes.Router = wp.themes.Router.extend({routes:{"themes?theme=:slug":"theme","themes?search=:query":"search","themes?s=:query":"search","themes":"themes","":"themes"},baseUrl:function(e){return "themes"+e;}});`)
	notices := append(append(s.themeLoadNotices(c), s.themeStaleEditsNotices(c)...), s.themeBackupNotices(c)...)
	if msg := c.Query("error"); msg != "" {
		notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(msg)), Dismissible: true})
	}
	if c.Query("activated") == "true" {
		notices = append(notices, notice{Type: "success", Message: template.HTML(`New theme activated. <a href="/">Visit site</a>`), Dismissible: true})
		if hasDemo && !demoImported && c.Query("demo") == "" && c.Query("error") == "" {
			notices = append(notices, notice{Type: "info", Message: template.HTML(`This theme comes with demo content. Import it? <a href="` +
				template.HTMLEscapeString(demoURL("import-demo", active.Slug)) + `" class="button button-primary wpgo-import-demo">Import Demo Content</a>`)})
		}
	}
	switch c.Query("demo") {
	case "imported":
		notices = append(notices, notice{Type: "success", Message: template.HTML(template.HTMLEscapeString(demoReportFromQuery(c.Query("counts")).summary()) +
			` <a href="/">Visit site</a>`), Dismissible: true})
	case "removed":
		notices = append(notices, notice{Type: "success", Message: "Demo content removed.", Dismissible: true})
	}
	if t := themeBySlug(c.Query("updated")); t != nil && t.Package != nil {
		notices = append(notices, notice{Type: "success", Message: template.HTML(template.HTMLEscapeString(t.Name) + " has been updated to version " +
			template.HTMLEscapeString(t.Version) + "."), Dismissible: true})
	}
	if c.Query("deleted") == "true" {
		notices = append(notices, notice{Type: "success", Message: "The theme has been deleted.", Dismissible: true})
	}
	if t := themeBySlug(c.Query("installed")); t != nil { // an uploaded package or a reinstalled bundled theme
		msg := template.HTMLEscapeString(t.Name) + " " + template.HTMLEscapeString(t.Version) + " has been installed."
		if t.Slug != active.Slug {
			msg += ` <a href="` + template.HTMLEscapeString("/wp-admin/themes?action=activate&stylesheet="+t.Slug+"&_wpnonce="+s.nonce(c, "switch-theme_"+t.Slug)) + `">Activate</a>`
		}
		notices = append(notices, notice{Type: "success", Message: template.HTML(msg), Dismissible: true})
	}
	s.adminPage(c, "themes", "Themes", "themes-php", "admin/themes", d, notices...)
}

// previewTheme lets an administrator view the site in another installed theme (?theme=slug), like the
// Live Preview button, without switching it for visitors.
func (s *Server) previewTheme(c *gin.Context) *Theme {
	slug := c.Query("theme")
	if slug == "" {
		return nil
	}
	u := currentUser(c)
	if u == nil || !u.Can("switch_themes", nil) {
		return nil
	}
	return s.installedTheme(slug)
}

// themeVisible reports whether a theme may be listed, previewed or activated: every registered
// theme the site has not deleted, except Hello Pursae while Pursae Web Builder is off.
func (s *Server) themeVisible(t *Theme) bool {
	if t == nil || s.themeDeleted(t.Slug) {
		return false
	}
	// Hello Pursae comes with Pursae Web Builder: while the plugin is off it is not offered, unless the
	// site still runs it (an owner who chose it keeps it after deactivation).
	if t.Slug == wbHelloSlug && !s.wbOn() && s.settings.String("stylesheet") != wbHelloSlug {
		return false
	}
	return true
}

// installedThemes is allThemes without the deleted ones.
func (s *Server) installedThemes() []*Theme {
	all := s.themeList()
	out := all[:0]
	for _, t := range all {
		if s.themeVisible(t) {
			out = append(out, t)
		}
	}
	return out
}

// installedTheme is themeBySlug for the themes of this site (nil for a deleted theme).
func (s *Server) installedTheme(slug string) *Theme {
	if t := s.themeFor(slug); s.themeVisible(t) {
		return t
	}
	return nil
}

// themesL10n is theme.js's _wpThemeSettings.l10n in the admin's language (the "Add Theme" card,
// the search placeholder and the result counts are drawn by JavaScript, after the page is translated).
func themesL10n(tr func(string) string) map[string]string {
	l := map[string]string{"addNew": "Add Theme", "search": "Search installed themes", "themesFound": "Number of Themes found: %d",
		"noThemesFound": "No themes found. Try a different search.", "themeViewed": "Theme details: %s"}
	for k, v := range l {
		l[k] = tr(v)
	}
	return l
}
