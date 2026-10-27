package server

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/builder"
	"wpgo/internal/store"
)

// Front end: posts saved with Golementor render through builder.Render inside the active theme; the
// page head gets the frontend stylesheet, the kit and per-post CSS files and the widget styles the
// page uses, the footer gets elementorFrontendConfig and the frontend scripts. The page templates
// "Golementor Canvas" (elementor_canvas) and "Golementor Full Width" (elementor_header_footer)
// replace the theme's page layout. The editor's preview iframe (?elementor-preview=ID) is the theme
// page with an empty builder wrapper the editor fills in.

const (
	ctxGwePreview = "gwe.preview" // post id being previewed in the editor
	ctxGweResults = "gwe.results" // rendered builder documents on this page
)

type gweRendered struct {
	post *store.Post
	res  *builder.Result
}

// gwePreviewing reports whether this request is the editor's preview iframe.
func gwePreviewing(c *gin.Context) bool {
	if c == nil {
		return false
	}
	_, ok := c.Get(ctxGwePreview)
	return ok
}

// gwePreview serves ?elementor-preview=ID for users who may edit that post; other requests go on to
// the site.
func (s *Server) gwePreview(c *gin.Context) {
	raw := c.Query("elementor-preview")
	if raw == "" || !s.golementorOn() || (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) {
		c.Next()
		return
	}
	u := currentUser(c)
	id, _ := strconv.ParseInt(raw, 10, 64)
	p, err := s.store.GetPost(c, id)
	if err != nil || u == nil || !gweEditable(p) || !u.Can("edit_post", p) {
		c.Next()
		return
	}
	c.Abort()
	c.Set(ctxGwePreview, p.ID)
	q := &siteQuery{Kind: "single", Post: p, Preview: true, Paged: 1}
	if p.Type != "post" {
		q.Kind = "page"
	}
	v, err := s.buildSiteView(c, q)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	h := c.Writer.Header()
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
	// The editor drives the preview from the parent window; its templates compile with new Function().
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https:; media-src 'self' blob: https:; "+
		"style-src 'self' 'unsafe-inline'; font-src 'self' data:; script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
		"frame-src 'self' https:; object-src 'none'; base-uri 'self'; frame-ancestors 'self'; form-action 'self'")
	if s.gweRenderTemplate(c, v, http.StatusOK) {
		return
	}
	if s.gweRenderShell(c, v, http.StatusOK) { // Golementor custom theme / theme parts
		return
	}
	s.renderSiteTemplate(c, v, http.StatusOK)
}

// gweContent is the_content for Golementor documents; ok is false for other posts.
func (s *Server) gweContent(c *gin.Context, p *store.Post) (string, bool) {
	if !s.golementorOn() { // built pages show the plain-HTML fallback saved in posts.content
		return "", false
	}
	if c != nil {
		if id, ok := c.Get(ctxGwePreview); ok && id.(int64) == p.ID {
			s.gweRecord(c, p, nil)
			return fmt.Sprintf(`<div data-elementor-type="%s" data-elementor-id="%d" class="elementor elementor-%d" data-elementor-title="%s"></div>`,
				html.EscapeString(gweDocType(p)), p.ID, p.ID, html.EscapeString(gweTypeTitle(p))), true
		}
	}
	if !gweBuilt(p) {
		return "", false
	}
	res, err := builder.Render(gweDocument(p), s.gweRenderContext(c, false))
	if err != nil || res == nil {
		if err != nil {
			log.Printf("gowebeditor: render post %d: %v", p.ID, err)
		}
		res = &builder.Result{}
	}
	res.HTML, res.CSS = s.gweFixURLs(c, res.HTML), s.gweFixURLs(c, res.CSS)
	s.gweRecord(c, p, res)
	s.gweWriteCSS(p, res.CSS)
	out := res.HTML
	if strings.TrimSpace(out) == "" {
		out = fmt.Sprintf(`<div data-elementor-type="%s" data-elementor-id="%d" class="elementor elementor-%d"></div>`, html.EscapeString(gweDocType(p)), p.ID, p.ID)
	}
	return out, true
}

func gweTypeTitle(p *store.Post) string {
	switch gweDocType(p) {
	case "wp-page":
		return "Page"
	case "wp-post":
		return "Post"
	case "kit":
		return "Kit"
	}
	return "Template"
}

func (s *Server) gweRecord(c *gin.Context, p *store.Post, res *builder.Result) {
	if c == nil {
		return
	}
	var list []gweRendered
	if v, ok := c.Get(ctxGweResults); ok {
		list = v.([]gweRendered)
	}
	c.Set(ctxGweResults, append(list, gweRendered{post: p, res: res}))
}

func gweResults(c *gin.Context) []gweRendered {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(ctxGweResults); ok {
		return v.([]gweRendered)
	}
	return nil
}

// gweWriteCSS stores uploads/elementor/css/post-<id>.css when it changed.
func (s *Server) gweWriteCSS(p *store.Post, css string) {
	path := s.gweCSSPath(p.ID)
	if old, err := os.ReadFile(path); err == nil && string(old) == css {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if os.WriteFile(tmp, []byte(css), 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// gweCSSURL returns the URL of a post's CSS file with a cache-busting version.
func (s *Server) gweCSSURL(c *gin.Context, id int64) string {
	ver := "0"
	if st, err := os.Stat(s.gweCSSPath(id)); err == nil {
		ver = strconv.FormatInt(st.ModTime().Unix(), 10)
	}
	return s.links(c).HomeURL("/wp-content/uploads/elementor/css/post-" + itoa64(id) + ".css?ver=" + ver)
}

func (s *Server) gweClearCSS() {
	dir := filepath.Join(s.cfg.UploadsDir, "elementor", "css")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "post-") && strings.HasSuffix(e.Name(), ".css") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// gweKitCSS makes sure the kit stylesheet exists and returns its id (0 without a kit).
func (s *Server) gweKitCSS(c *gin.Context) int64 {
	kit, err := s.gweKit(c)
	if err != nil {
		return 0
	}
	path := s.gweCSSPath(kit.ID)
	if st, err := os.Stat(path); err != nil || st.ModTime().Before(kit.ModifiedAt) {
		s.gweWriteCSS(kit, s.gweFixURLs(c, builder.KitCSS(gweDocument(kit), s.gweRenderContext(c, false))+s.gweV4KitCSS(c))) // + Golementor v4 variables
	}
	return kit.ID
}

var gweStyleMapOnce sync.Once
var gweStyleMap map[string]string
var gwePreviewStyleList [][2]string

func (s *Server) gweStyles() (map[string]string, [][2]string) {
	gweStyleMapOnce.Do(func() {
		gweStyleMap = map[string]string{}
		for _, st := range gweData().previewStyles {
			gweStyleMap[st[0]] = st[1]
			if _, err := fs.Stat(s.web, "gowebeditor/"+st[1]); err == nil {
				gwePreviewStyleList = append(gwePreviewStyleList, st)
			}
		}
	})
	return gweStyleMap, gwePreviewStyleList
}

// gweStyleHref maps a frontend style handle to its file.
func (s *Server) gweStyleHref(handle string) string {
	if rel := s.gweProStyleHref(handle); rel != "" { // Golementor Pro widget and module styles
		return rel
	}
	m, _ := s.gweStyles()
	rel, ok := m[handle]
	if !ok {
		switch {
		case strings.HasPrefix(handle, "widget-"):
			rel = "css/" + handle + ".min.css"
		case strings.HasPrefix(handle, "e-animation-"):
			rel = "lib/animations/styles/" + strings.TrimPrefix(handle, "e-animation-") + ".min.css"
		default:
			return ""
		}
	}
	if _, err := fs.Stat(s.web, "gowebeditor/"+rel); err != nil {
		return ""
	}
	return rel
}

var gweScriptFiles = map[string]string{
	"swiper": "lib/swiper/v8/swiper.min.js", "jquery-numerator": "lib/jquery-numerator/jquery-numerator.min.js",
	"elementor-gallery": "lib/e-gallery/js/e-gallery.min.js", "share-link": "lib/share-link/share-link.min.js",
	"elementor-dialog": "lib/dialog/dialog.min.js", "imagesloaded": "/wp-includes/js/imagesloaded.min.js",
}

func gweLink(id, href string) string {
	return fmt.Sprintf("<link rel='stylesheet' id='%s-css' href='%s' media='all' />\n", id, html.EscapeString(href))
}

func gweScript(id, src string) string {
	return fmt.Sprintf("<script src=\"%s\" id=\"%s-js\"></script>\n", html.EscapeString(src), id)
}

// gweHead prints Golementor's stylesheets in wp_head.
func (s *Server) gweHead(c *gin.Context) template.HTML {
	if !s.golementorOn() {
		return ""
	}
	s.gweProRecordPopups(c) // Golementor Pro: popups print on every page their conditions match
	results := gweResults(c)
	if len(results) == 0 {
		return ""
	}
	assets := s.links(c).HomeURL(gweAssetsPath)
	ver := "?ver=" + gweVersion + "-" + Version
	var sb strings.Builder
	kitID := s.gweKitCSS(c)
	if gwePreviewing(c) {
		_, list := s.gweStyles()
		for _, st := range list {
			sb.WriteString(gweLink(st[0], assets+st[1]+ver))
			if st[0] == "elementor-frontend" && kitID > 0 {
				sb.WriteString(gweLink("elementor-post-"+itoa64(kitID), s.gweCSSURL(c, kitID)))
			}
		}
		sb.WriteString(s.gweV4HeadLinks(c)) // Golementor v4: atomic stylesheets (gowebeditor_v4.go)
		sb.WriteString(gweLink("gowebeditor-preview", assets+"css/gowebeditor-editor.css"+ver))
		// Golementor Site widgets (theme parts) in the preview
		sb.WriteString(gweLink("widget-theme-elements", assets+"css/widget-theme-elements.min.css"+ver))
		sb.WriteString(s.gweProHead(c, results, true))        // Golementor Pro preview styles
		sb.WriteString(s.gweFontsHead(c, results))            // gap-integrations: local Google Fonts
		sb.WriteString("<link rel='icon' href='data:,' />\n") // the iframe never needs /favicon.ico
		sb.WriteString(gweScript("jquery-core", "/wp-includes/js/jquery/jquery.min.js?ver=3.7.1"))
		sb.WriteString(gweScript("jquery-migrate", "/wp-includes/js/jquery/jquery-migrate.min.js?ver=3.4.1"))
		sb.WriteString("<script id=\"jquery-migrate-js-after\">\njQuery.holdReady( true );\n</script>\n")
		return template.HTML(sb.String())
	}
	sb.WriteString(gweLink("elementor-frontend", assets+"css/frontend.min.css"+ver))
	if kitID > 0 {
		sb.WriteString(gweLink("elementor-post-"+itoa64(kitID), s.gweCSSURL(c, kitID)))
	}
	sb.WriteString(s.gweV4HeadLinks(c)) // Golementor v4: atomic stylesheets (gowebeditor_v4.go)
	seen := map[string]bool{}
	for _, r := range results {
		if r.res == nil {
			continue
		}
		for _, h := range r.res.Styles {
			if seen[h] {
				continue
			}
			seen[h] = true
			if rel := s.gweStyleHref(h); rel != "" {
				sb.WriteString(gweLink(h, assets+rel+ver))
			}
		}
	}
	for _, r := range results {
		if r.res != nil && r.res.CSS != "" {
			sb.WriteString(gweLink("elementor-post-"+itoa64(r.post.ID), s.gweCSSURL(c, r.post.ID)))
		}
	}
	sb.WriteString(s.gweProHead(c, results, false)) // Golementor Pro: popup styles, custom code
	sb.WriteString(s.gweFontsHead(c, results))      // gap-integrations: local Google Fonts, PayPal form-action
	sb.WriteString(gweScript("jquery-core", "/wp-includes/js/jquery/jquery.min.js?ver=3.7.1"))
	sb.WriteString(gweScript("jquery-migrate", "/wp-includes/js/jquery/jquery-migrate.min.js?ver=3.4.1"))
	return template.HTML(sb.String())
}

// gweFooter prints elementorFrontendConfig and the frontend scripts in wp_footer.
func (s *Server) gweFooter(c *gin.Context) template.HTML {
	if !s.golementorOn() {
		return ""
	}
	results := gweResults(c)
	if len(results) == 0 {
		return ""
	}
	assets := s.links(c).HomeURL(gweAssetsPath)
	ver := "?ver=" + gweVersion + "-" + Version
	preview := gwePreviewing(c)
	var sb strings.Builder
	if preview {
		sb.WriteString(gweScript("wp-hooks", "/wp-includes/js/dist/hooks.min.js"))
		sb.WriteString(gweScript("wp-i18n", "/wp-includes/js/dist/i18n.min.js"))
		sb.WriteString("<script id=\"wp-i18n-js-after\">\nwp.i18n.setLocaleData( { 'text direction\\u0004ltr': [ 'ltr' ] } );\n</script>\n")
		sb.WriteString(gweScript("react", "/wp-includes/js/dist/vendor/react.min.js?ver=18.3.1.1"))
		sb.WriteString(gweScript("react-dom", "/wp-includes/js/dist/vendor/react-dom.min.js?ver=18.3.1.1"))
		sb.WriteString(gweScript("elementor-styleguide-app-initiator", assets+"js/styleguide-app-initiator.min.js"+ver))
	}
	sb.WriteString(s.gweV4FooterScripts(c)) // Golementor v4: atomic element handlers (gowebeditor_v4.go)
	sb.WriteString(gweScript("elementor-webpack-runtime", assets+"js/webpack.runtime.min.js"+ver))
	sb.WriteString(gweScript("elementor-frontend-modules", assets+"js/frontend-modules.min.js"+ver))
	sb.WriteString("<script id=\"jquery-ui-core-js-before\">\njQuery.uiBackCompat = true;\n</script>\n")
	sb.WriteString(gweScript("jquery-ui-core", "/wp-includes/js/jquery/ui/core.min.js?ver=1.14.2"))
	sb.WriteString("<script id=\"elementor-frontend-js-before\">\nvar elementorFrontendConfig = " + s.gweFrontendConfig(c, results[0].post, preview) + ";\n</script>\n")
	sb.WriteString(gweScript("elementor-frontend", assets+"js/frontend.min.js"+ver))
	scripts := map[string]bool{}
	if preview {
		for _, h := range []string{"swiper", "jquery-numerator"} {
			scripts[h] = true
		}
	}
	for _, r := range results {
		if r.res != nil {
			for _, h := range r.res.Scripts {
				scripts[h] = true
			}
		}
	}
	s.gweProFrontScripts(c, results, preview, scripts) // Golementor Pro: popups' widgets, dialog
	for _, h := range []string{"swiper", "jquery-numerator", "imagesloaded", "elementor-gallery", "share-link", "elementor-dialog"} {
		if scripts[h] {
			src := gweScriptFiles[h]
			if !strings.HasPrefix(src, "/") {
				src = assets + src + ver
			}
			sb.WriteString(gweScript(h, src))
		}
	}
	if preview {
		sb.WriteString(gweScript("elementor-inline-editor", assets+"lib/inline-editor/js/inline-editor.min.js"+ver))
	}
	sb.WriteString(s.gweProFooter(c, results, preview, scripts)) // Golementor Pro frontend scripts and popups
	return template.HTML(sb.String())
}

// gweFrontendConfig is elementorFrontendConfig for this page.
func (s *Server) gweFrontendConfig(c *gin.Context, p *store.Post, edit bool) string {
	base := gweData().frontCfg
	cfg := make(map[string]any, len(base))
	for k, v := range base {
		cfg[k] = v
	}
	cfg["environmentMode"] = map[string]any{"edit": edit, "wpPreview": false, "isScriptDebug": false}
	cfg["nonces"] = map[string]string{"floatingButtonsClickTracking": s.nonce(c, "elementor-floating-buttons"), "atomicFormsSendForm": s.nonce(c, gweV4FormNonce)} // gap-atomic: atomic form nonce
	featured := any(false)
	if p.FeaturedMediaID > 0 {
		if a, err := s.store.GetAttachment(c, p.FeaturedMediaID); err == nil {
			featured = s.uploadURL(c, a.FilePath)
		}
	}
	title := p.Title + " – " + s.settings.String("blogname")
	cfg["post"] = map[string]any{"id": p.ID, "title": strings.ReplaceAll(url.QueryEscape(title), "+", "%20"), "excerpt": p.Excerpt, "featuredImage": featured}
	_, kit := s.gweKitSettings(c)
	k := map[string]any{"active_breakpoints": []string{"viewport_mobile", "viewport_tablet"}, "global_image_lightbox": "yes",
		"lightbox_enable_counter": "yes", "lightbox_enable_fullscreen": "yes", "lightbox_enable_zoom": "yes", "lightbox_enable_share": "yes",
		"lightbox_title_src": "title", "lightbox_description_src": "description"}
	for key := range k {
		if v, ok := kit[key]; ok {
			k[key] = v
		}
	}
	cfg["kit"] = k
	if edit {
		if u := currentUser(c); u != nil {
			cfg["user"] = map[string]any{"roles": u.Roles}
		}
	} else {
		delete(cfg, "user")
		delete(cfg, "elements")
	}
	return s.gweFill(c, gweJSON(cfg))
}

// gweBodyClasses are the body classes of a Golementor page.
func (s *Server) gweBodyClasses(c *gin.Context, q *siteQuery) []string {
	if q == nil || q.Post == nil || !s.golementorOn() {
		return nil
	}
	preview := gwePreviewing(c)
	if !gweBuilt(q.Post) && !preview {
		return nil
	}
	kit, err := s.gweKit(c)
	if err != nil {
		return nil
	}
	out := []string{"elementor-default", "elementor-kit-" + itoa64(kit.ID), "elementor-page", "elementor-page-" + itoa64(q.Post.ID)}
	switch gweTemplate(q.Post) {
	case "elementor_canvas":
		out = append(out, "elementor-template-canvas")
	case "elementor_header_footer":
		out = append(out, "elementor-template-full-width")
	}
	if preview {
		out = append(out, "e-preview--show-hidden-elements")
	}
	return out
}

func gweTemplate(p *store.Post) string {
	t, _ := p.Meta["_wp_page_template"].(string)
	if t == "" && (p.Type == gweLibrary || p.Type == gwePartType) { // templates and theme parts preview on a blank canvas
		return "elementor_canvas"
	}
	return t
}

// gweRenderTemplate renders the Golementor page templates; false means the theme template applies.
func (s *Server) gweRenderTemplate(c *gin.Context, v *siteView, status int) bool {
	if v == nil || v.Q == nil || v.Q.Post == nil || (v.Template != "page" && v.Template != "single") || !s.golementorOn() {
		return false
	}
	p := v.Q.Post
	if !gweBuilt(p) && !gwePreviewing(c) {
		return false
	}
	tpl := gweTemplate(p)
	if tpl != "elementor_canvas" && tpl != "elementor_header_footer" {
		return false
	}
	content := template.HTML("")
	if v.Post != nil {
		content = v.Post.Content
	}
	var buf bytes.Buffer
	if tpl == "elementor_canvas" {
		fmt.Fprintf(&buf, "<!DOCTYPE html>\n<html lang=\"en-US\">\n<head>\n<meta charset=\"UTF-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0, viewport-fit=cover\" />\n<title>%s</title>\n%s</head>\n<body class=\"%s\">\n",
			v.DocumentTitle, v.Head, html.EscapeString(v.BodyClass))
		buf.WriteString(string(v.BodyOpen)) // gap-widgets: wp_body_open
		buf.WriteString(string(content))
		buf.WriteString("\n")
		buf.WriteString(string(v.AdminBar))
		buf.WriteString(string(v.Footer))
		buf.WriteString("\n</body>\n</html>\n")
	} else {
		set := s.tmpl.getTheme(true, v.Theme.Slug)
		if v.GWE != nil { // Golementor theme parts: their header and footer around the page
			set = s.tmpl.gweShell()
		}
		if set.Lookup("header") == nil || set.Lookup("footer") == nil {
			return false
		}
		if err := set.ExecuteTemplate(&buf, "header", v); err != nil {
			log.Printf("gowebeditor: full width header: %v", err)
			return false
		}
		buf.WriteString("\n")
		buf.WriteString(string(content))
		buf.WriteString("\n")
		if err := set.ExecuteTemplate(&buf, "footer", v); err != nil {
			log.Printf("gowebeditor: full width footer: %v", err)
			return false
		}
	}
	c.Data(status, "text/html; charset=UTF-8", buf.Bytes())
	return true
}

// gweEditorButton adds "Edit with Golementor" to Gotenberg and the classic editor.
func (s *Server) gweEditorButton(c *gin.Context, p *store.Post, block bool) {
	if !s.golementorOn() || !gweEditable(p) || !currentUser(c).Can("edit_post", p) {
		return
	}
	url := strconvQuote(gweEditURL(p.ID))
	a := s.pageAssets(c)
	if block {
		a.AddInline("wp-edit-post", `( function() {
	var url = `+url+`;
	function go() {
		var ed = wp.data.select( 'core/editor' );
		if ( ed.isEditedPostNew() || ed.isEditedPostDirty() ) {
			var title = ed.getEditedPostAttribute( 'title' );
			if ( ! title ) { wp.data.dispatch( 'core/editor' ).editPost( { title: 'Golementor #`+itoa64(p.ID)+`' } ); }
			wp.data.dispatch( 'core/editor' ).savePost().then( function() { location.href = url; } );
		} else {
			location.href = url;
		}
	}
	function mount() {
		var bar = document.querySelector( '.editor-header__toolbar, .edit-post-header-toolbar' );
		if ( ! bar ) { return false; }
		if ( document.getElementById( 'gowebeditor-switch' ) ) { return true; }
		var b = document.createElement( 'button' );
		b.id = 'gowebeditor-switch';
		b.type = 'button';
		b.className = 'components-button is-primary';
		b.style.marginLeft = '12px';
		b.textContent = 'Edit with Golementor';
		b.addEventListener( 'click', go );
		bar.appendChild( b );
		return true;
	}
	wp.domReady( function() {
		var tries = 0;
		var t = setInterval( function() { if ( mount() || ++tries > 100 ) { clearInterval( t ); } }, 200 );
		wp.data.subscribe( mount );
	} );
} )();`)
		return
	}
	a.AddInline("common", `jQuery( function( $ ) {
	var $t = $( '#titlediv' );
	if ( ! $t.length ) { return; }
	var $b = $( '<div id="gowebeditor-switch-mode" style="margin:12px 0"><a class="button button-primary button-hero" href='+`+url+`+'>Edit with Golementor</a></div>' );
	$t.after( $b );
	$b.find( 'a' ).on( 'click', function( e ) {
		if ( $( '#original_post_status' ).val() === 'auto-draft' ) {
			e.preventDefault();
			if ( ! $( '#title' ).val() ) { $( '#title' ).val( 'Golementor #`+itoa64(p.ID)+`' ); }
			$( '#post' ).append( '<input type="hidden" name="gowebeditor_redirect" value="1">' );
			$( '#save-post' ).trigger( 'click' );
		}
	} );
} );`)
}

// gweAfterClassicSave sends the classic editor's "save then open Golementor" back to the builder.
func (s *Server) gweClassicRedirect(c *gin.Context, id int64) (string, bool) {
	if !s.golementorOn() || c.PostForm("gowebeditor_redirect") == "" {
		return "", false
	}
	return gweEditURL(id), true
}

// gweRowAction is the posts list "Edit with Golementor" link (empty when not applicable).
func (s *Server) gweRowAction(c *gin.Context, p *store.Post) string {
	if !s.golementorOn() || !gweEditable(p) || p.Status == "trash" || !currentUser(c).Can("edit_post", p) {
		return ""
	}
	return gweEditURL(p.ID)
}
