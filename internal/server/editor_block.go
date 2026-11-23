package server

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/i18n"
	"wpgo/internal/media"
	"wpgo/internal/store"
)

// Gotenberg is WordPress Go's name for the block editor: WordPress 7.1's own @wordpress
// packages (wp-includes/js/dist), bootstrapped exactly like wp-admin/edit-form-blocks.php,
// talking to the Go REST API.

//go:embed defaults/core-theme.json
var coreThemeJSON []byte

// editorFeatures is __experimentalFeatures: core theme.json settings with preset lists keyed by
// origin, plus the theme's editor-color-palette as the theme palette (a classic theme) or the theme's
// theme.json settings (Theme.Settings).
func editorFeatures(t *Theme) map[string]any {
	palette := t.Palette
	var tj struct {
		Settings map[string]any `json:"settings"`
	}
	_ = json.Unmarshal(coreThemeJSON, &tj)
	st := tj.Settings
	byOrigin := func(section, key string) {
		sec, ok := st[section].(map[string]any)
		if !ok {
			return
		}
		if list, ok := sec[key].([]any); ok {
			sec[key] = map[string]any{"default": list}
		}
	}
	byOrigin("color", "palette")
	byOrigin("color", "gradients")
	byOrigin("color", "duotone")
	byOrigin("typography", "fontSizes")
	byOrigin("typography", "fontFamilies")
	byOrigin("spacing", "spacingSizes")
	byOrigin("shadow", "presets")
	byOrigin("dimensions", "aspectRatios")
	if color, ok := st["color"].(map[string]any); ok {
		pal := color["palette"].(map[string]any)
		var theme []any
		for _, p := range palette {
			theme = append(theme, map[string]any{"name": p["name"], "slug": p["slug"], "color": p["color"]})
		}
		pal["theme"] = theme
	}
	if t.Settings != nil {
		mergeThemeSettings(st, t.Settings)
		return st
	}
	// Classic themes without theme.json: no layout support, no appearance tools.
	st["appearanceTools"] = false
	if layout, ok := st["layout"].(map[string]any); ok {
		delete(layout, "contentSize")
		delete(layout, "wideSize")
	}
	return st
}

// mergeThemeSettings applies a theme's theme.json settings to the core settings: preset lists go
// under the "theme" origin, other values replace core's.
func mergeThemeSettings(st, theme map[string]any) {
	presets := map[string]bool{"palette": true, "gradients": true, "duotone": true, "fontSizes": true, "fontFamilies": true,
		"spacingSizes": true, "presets": true, "aspectRatios": true}
	for section, v := range theme {
		tm, ok := v.(map[string]any)
		if !ok {
			st[section] = v
			continue
		}
		cm, ok := st[section].(map[string]any)
		if !ok {
			cm = map[string]any{}
			st[section] = cm
		}
		for k, val := range tm {
			if presets[k] {
				if _, isList := val.([]any); isList {
					byOrigin, ok := cm[k].(map[string]any)
					if !ok {
						byOrigin = map[string]any{}
						cm[k] = byOrigin
					}
					byOrigin["theme"] = val
					continue
				}
			}
			cm[k] = val
		}
	}
}

// editorGlobalStyles is the global stylesheet the block editor gets: the theme's (Theme.Settings)
// or core's for classic themes.
func (s *Server) editorGlobalStyles(t *Theme) string {
	if t.EditorGlobalStyles != "" {
		css := s.webFile("themes/" + t.Slug + "/assets/" + t.EditorGlobalStyles)
		if t.Slug == "twentytwentyfive" {
			css += "\n" + s.tt5SchemeCSS(s.themeMods(t.Slug))
		}
		return css
	}
	return s.webFile("wp-includes/css/global-styles-classic.css")
}

// preloadPath is one entry of block_editor_rest_api_preload().
type preloadPath struct {
	Path   string
	Method string
}

// restPreload runs REST requests in-process with the editor's cookies and nonce and returns
// the map createPreloadingMiddleware() expects.
func (s *Server) restPreload(c *gin.Context, paths []preloadPath) map[string]any {
	out := map[string]any{}
	options := map[string]any{}
	nonce := s.nonce(c, "wp_rest")
	for _, p := range paths {
		req := httptest.NewRequest(p.Method, "/wp-json"+p.Path, nil).WithContext(c.Request.Context())
		for _, ck := range c.Request.Cookies() {
			req.AddCookie(ck)
		}
		req.Header.Set("X-WP-Nonce", nonce)
		req.Host = c.Request.Host
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		var body any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		headers := map[string]string{}
		for _, h := range []string{"Allow", "X-WP-Total", "X-WP-TotalPages", "Link"} {
			if v := rec.Header().Get(h); v != "" {
				headers[h] = v
			}
		}
		entry := map[string]any{"body": body, "headers": headers}
		if p.Method == http.MethodOptions {
			options[p.Path] = entry
		} else {
			out[p.Path] = entry
		}
	}
	out["OPTIONS"] = options
	return out
}

// useBlockEditor applies the built-in Classic Editor settings (Settings › Writing). Like
// use_block_editor_for_post_type(), a post type gets the block editor when it supports "editor"
// and is shown in REST: posts, pages, and Custom Fields post types with Show in REST.
func (s *Server) useBlockEditor(c *gin.Context, p *store.Post) bool {
	if p.Type == "wp_block" { // patterns are block content: the classic editor has no screen for them
		return true
	}
	if p.Type != "post" && p.Type != "page" && !acfgoBlockEditorType(p.Type) {
		return false
	}
	if !s.settings.Bool("native_classic_editor") {
		return true
	}
	def := s.settings.String("classic-editor-replace") != "classic"
	if !s.classicSwitchAllowed() {
		return def
	}
	if _, ok := c.GetQuery("classic-editor"); ok {
		return false
	}
	if _, ok := c.GetQuery("classic-editor__forget"); ok {
		return true
	}
	if v, ok := p.Meta["classic-editor-remember"].(string); ok && v != "" {
		return v == "block-editor"
	}
	if u := currentUser(c); u != nil {
		if v, ok := u.Meta["classic-editor-settings"].(string); ok && v != "" {
			return v == "block"
		}
	}
	return def
}

func (s *Server) renderBlockEditor(c *gin.Context, p *store.Post, isNew bool) {
	u := currentUser(c)
	lb := s.links(c)
	theme := s.activeTheme()
	base := restBaseFor(p.Type)
	restPath := "/wp/v2/" + base + "/" + itoa64(p.ID)

	paths := []preloadPath{
		{"/wp/v2/types?context=view", http.MethodGet},
		{"/wp/v2/taxonomies?context=view", http.MethodGet},
		{restPath + "?context=edit", http.MethodGet},
		{"/wp/v2/types/" + p.Type + "?context=edit", http.MethodGet},
		{"/wp/v2/users/me", http.MethodGet},
		{"/wp/v2/media", http.MethodOptions},
		{"/wp/v2/pages", http.MethodOptions},
		{"/wp/v2/blocks", http.MethodOptions},
		{"/wp/v2/templates", http.MethodOptions},
		{restPath + "/autosaves?context=edit", http.MethodGet},
		{"/wp/v2/settings", http.MethodGet},
		{"/wp/v2/settings", http.MethodOptions},
		{"/wp/v2/global-styles/themes/" + theme.Slug + "?context=view", http.MethodGet},
		{"/wp/v2/global-styles/themes/" + theme.Slug + "/variations?context=view", http.MethodGet},
		{"/wp/v2/themes?context=edit&status=active", http.MethodGet},
		{"/wp/v2/block-patterns/categories", http.MethodGet},
		{"/?_fields=description,gmt_offset,home,image_max_bit_depth,image_sizes,image_size_threshold,image_strip_meta,name,site_icon,site_icon_url,site_logo,timezone_string,url,page_for_posts,page_on_front,show_on_front", http.MethodGet},
		{"/wp/v2/taxonomies?context=edit", http.MethodGet},
		{"/wp/v2/" + base, http.MethodOptions},
		{"/wp/v2/users/" + itoa64(p.AuthorID) + "?context=view&_fields=id,name", http.MethodGet},
	}
	preload := s.restPreload(c, paths)
	preloadJSON, _ := json.Marshal(preload)

	categories := []map[string]any{
		{"slug": "text", "title": "Text", "icon": nil}, {"slug": "media", "title": "Media", "icon": nil},
		{"slug": "design", "title": "Design", "icon": nil}, {"slug": "widgets", "title": "Widgets", "icon": nil},
		{"slug": "theme", "title": "Theme", "icon": nil}, {"slug": "embed", "title": "Embeds", "icon": nil},
		{"slug": "reusable", "title": "Patterns", "icon": nil},
	}
	for _, cat := range categories {
		cat["title"] = s.__(c, cat["title"].(string))
	}
	catJSON, _ := json.Marshal(categories)

	themeURI := lb.HomeURL(theme.URI() + "/")
	var editorStyle string
	for _, f := range theme.EditorStyles {
		if strings.HasSuffix(f, ".css") && !strings.HasPrefix(f, "fonts/") {
			editorStyle += s.webFile("themes/"+theme.Slug+"/assets/"+f) + "\n"
		}
	}
	if theme.Settings == nil { // a theme.json theme's canvas takes its global styles, background included, like the front
		editorStyle += s.webFile("wp-includes/css/wpgo-editor-neutral.css") + "\n"
	}
	resolved := ""
	canvas := []string{
		"/wp-includes/css/dist/block-library/style.min.css", "/wp-includes/css/dist/block-library/theme.min.css",
		"/wp-includes/css/dist/block-library/editor.min.css", "/wp-includes/css/dist/block-library/editor-elements.min.css",
		"/wp-includes/css/dist/block-editor/content.min.css", "/wp-includes/css/dist/block-library/common.min.css",
	}
	if theme.Settings == nil { // classic (non block) themes: WordPress centers blocks in an 840px column (wp-edit-post's classic.css)
		canvas = append(canvas, "/wp-includes/css/dist/edit-post/classic.min.css")
	}
	for _, f := range append(append([]string{}, theme.EditorStyles...), theme.EditorBlockStyles...) {
		if strings.HasPrefix(f, "fonts/") || strings.Contains(f, "genericons") || strings.Contains(f, "editor-blocks") || strings.Contains(f, "block") {
			canvas = append(canvas, theme.URI()+"/"+f)
		}
	}
	for _, href := range canvas {
		resolved += `<link rel="stylesheet" href="` + lb.HomeURL(href) + `" media="all" />` + "\n"
	}

	patterns := []map[string]any{}
	for _, pt := range corePatterns() {
		patterns = append(patterns, map[string]any{"name": pt["name"], "title": pt["title"], "content": pt["content"],
			"categories": pt["categories"], "description": pt["description"], "viewportWidth": pt["viewport_width"],
			"blockTypes": pt["block_types"], "postTypes": pt["post_types"], "inserter": pt["inserter"], "keywords": pt["keywords"]})
	}

	allowedMimes := map[string]string{}
	for k, v := range media.MimeTypes {
		allowedMimes[k] = v
	}
	settings := map[string]any{
		"alignWide": theme.Settings != nil, "allowedBlockTypes": true, "allowedMimeTypes": allowedMimes,
		"defaultEditorStyles": []map[string]any{{"css": s.webFile("wp-includes/css/dist/block-editor/default-editor-styles.min.css")}},
		"blockCategories":     categories, "isRTL": i18n.IsRTL(s.userLocale(c)), "imageDefaultSize": "large", "imageEditing": true,
		"imageDimensions": map[string]any{
			"thumbnail": map[string]any{"width": s.settings.Int("thumbnail_size_w"), "height": s.settings.Int("thumbnail_size_h"), "crop": s.settings.Bool("thumbnail_crop")},
			"medium":    map[string]any{"width": s.settings.Int("medium_size_w"), "height": s.settings.Int("medium_size_h"), "crop": false},
			"large":     map[string]any{"width": s.settings.Int("large_size_w"), "height": s.settings.Int("large_size_h"), "crop": false},
		},
		"imageSizes": []map[string]string{{"slug": "thumbnail", "name": s.__(c, "Thumbnail")}, {"slug": "medium", "name": s.__(c, "Medium")},
			{"slug": "large", "name": s.__(c, "Large")}, {"slug": "full", "name": s.__(c, "Full Size")}},
		"maxUploadFileSize":                maxUploadBytes,
		"__unstableGalleryWithImageBlocks": true,
		"__experimentalFeatures":           editorFeatures(theme),
		"colors":                           theme.Palette,
		"disableCustomColors":              false, "disableCustomFontSizes": false, "disableCustomGradients": false,
		"disableLayoutStyles": false, "enableCustomLineHeight": true, "enableCustomSpacing": false, "enableCustomUnits": true,
		"styles": []map[string]any{
			{"css": s.editorGlobalStyles(theme), "__unstableType": "presets", "isGlobalStyles": true},
			{"css": ".is-layout-flow > * + *{margin-block-start:1.2rem}body .is-layout-flex{display:flex}.is-layout-flex{flex-wrap:wrap;align-items:center}.is-layout-flex > :is(*, div){margin:0}", "__unstableType": "base-layout", "isGlobalStyles": true},
			{"css": editorStyle, "baseURL": themeURI + "css/", "__unstableType": "theme", "isGlobalStyles": false},
		},
		"__unstableResolvedAssets":    map[string]string{"styles": resolved, "scripts": ""},
		"__unstableIsBlockBasedTheme": false,
		"localAutosaveInterval":       15,
		"__experimentalDiscussionSettings": map[string]any{
			"commentOrder": s.settings.String("comment_order"), "commentsPerPage": s.settings.Int("comments_per_page"),
			"defaultCommentsPage": s.settings.String("default_comments_page"), "pageComments": s.settings.Bool("page_comments"),
			"threadComments": s.settings.Bool("thread_comments"), "threadCommentsDepth": s.settings.Int("thread_comments_depth"),
			"defaultCommentStatus": s.settings.String("default_comment_status"), "avatarURL": defaultAvatar,
		},
		"__experimentalDashboardLink":                    "/wp-admin/",
		"__experimentalAdditionalBlockPatterns":          patterns,
		"__experimentalAdditionalBlockPatternCategories": corePatternCategories(),
		"availableTemplates":                             s.editorPageTemplates(c, p),
		"disablePostFormats":                             false,
		"titlePlaceholder":                               s.__(c, "Add title"),
		"bodyPlaceholder":                                s.__(c, "Type / to choose a block"),
		"autosaveInterval":                               60,
		"richEditingEnabled":                             true,
		"postLock":                                       map[string]any{"isLocked": false, "activePostLock": ""},
		"postLockUtils":                                  map[string]string{"nonce": s.nonce(c, "lock-post_"+itoa64(p.ID)), "unlockNonce": s.nonce(c, "update-post_"+itoa64(p.ID)), "ajaxUrl": "/wp-admin/admin-ajax"},
		"supportsLayout":                                 theme.Settings != nil,
		"supportsTemplateMode":                           false,
		"enableCustomFields":                             false,
		"codeEditingEnabled":                             true,
		"canUpdateBlockBindings":                         u.Can("manage_options", nil),
	}
	if f := store.CanonicalPostFormat(p.Format); isNew && p.Type == "post" && f != "standard" {
		settings["template"] = [][]string{{"core/" + f}}
	}
	settingsJSON, _ := json.Marshal(settings)
	initial := map[string]any{}
	if isNew {
		initial["title"] = p.Title // "" unless prefilled (post_title, a Golylang duplicate)
		initial["content"] = p.Content
		initial["excerpt"] = p.Excerpt
	}
	initialJSON, _ := json.Marshal(initial)

	a := s.pageAssets(c)
	a.Script("heartbeat", "wp-edit-post", "wp-format-library").Style("wp-edit-post", "wp-format-library", "wp-block-library-theme", "wp-block-directory")
	a.AddInline("wp-api-fetch", "wp.apiFetch.use( wp.apiFetch.createPreloadingMiddleware( "+string(preloadJSON)+" ) );")
	a.AddInline("wp-blocks", "wp.blocks.setCategories( "+string(catJSON)+" );")
	a.AddBefore("wp-editor", `var _wpMetaBoxUrl = "/wp-admin/post?post=`+itoa64(p.ID)+`&action=edit&meta-box-loader=1";`)
	a.AddInline("heartbeat", "jQuery( function() {\n\twp.heartbeat.interval( 10 );\n} );")
	a.AddInline("wp-edit-post", `( function() {
	window._wpLoadBlockEditor = new Promise( function( resolve ) {
		wp.domReady( function() {
			resolve( wp.editPost.initializeEditor( 'editor', "`+p.Type+`", `+itoa64(p.ID)+`, `+string(settingsJSON)+`, `+string(initialJSON)+` ) );
		} );
	} );
} )();`)
	if u := s.classicSwitchURL(p); u != "" {
		a.Script("wp-plugins")
		a.AddInline("wp-edit-post", `wp.plugins.registerPlugin( 'wpgo-classic-editor-switch', { render: function() {
	return wp.element.createElement( wp.editor.PluginMoreMenuItem, { icon: 'editor-kitchensink', onClick: function() { window.location.href = `+strconvQuote(u)+`; } }, 'Switch to classic editor' );
} } );`)
	}
	s.golylangBlockEditor(c, p) // Golylang: the Languages panel
	s.enqueueMedia(c, p.ID)
	s.gontactEditorAssets(c, a)  // Gontact Form's contact form block
	s.gooCartBlocksEditor(c)     // Pursae Commerce's Cart and Checkout blocks (goo_blocks_cart_editor.go)
	s.pluginBlockEditorScript(c) // Wasm plugin blocks (plugins_ui.go)

	lab := labelsFor(p.Type)
	title := "Edit " + lab.Singular
	screen := "post?post_type=" + p.Type
	if isNew {
		title = lab.AddNew
		screen = "post-new"
		if p.Type == "page" || p.Type == "wp_block" {
			screen = "post-new?post_type=" + p.Type
		}
	}
	s.adminPage(c, screen, title, "post-php post-type-"+p.Type+" block-editor-page wp-embed-responsive is-fullscreen-mode", "admin/block-editor",
		map[string]any{"Title": title, "ClassicURL": s.classicSwitchURL(p), "MetaBoxes": s.pluginMetaBoxesBlockEditor(c, p)})
}

func (s *Server) classicSwitchURL(p *store.Post) string {
	if !s.classicSwitchAllowed() || p.Type == "wp_block" {
		return ""
	}
	return "/wp-admin/post?post=" + itoa64(p.ID) + "&action=edit&classic-editor"
}

func strconvQuote(v string) string { b, _ := json.Marshal(v); return string(b) }

// editorPageTemplates is the block editor's availableTemplates (get_page_templates() plus the
// default): the theme's page templates for a page, and the page's own template when the theme does
// not list it, so the Template panel shows it and saving keeps it.
func (s *Server) editorPageTemplates(c *gin.Context, p *store.Post) map[string]string {
	out := map[string]string{"": s.__(c, "Default template")}
	if p.Type != "page" {
		return out
	}
	tpl, _ := p.Meta["_wp_page_template"].(string)
	for _, o := range withPageTemplate(s.pageTemplates(), tpl) {
		out[o.Value] = o.Label
	}
	return out
}
