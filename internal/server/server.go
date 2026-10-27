// Package server wires HTTP routes to the admin screens, the login and install
// flows and the public site rendered by the active theme.
package server

import (
	"context"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"wpgo/internal/auth"
	"wpgo/internal/config"
	"wpgo/internal/links"
	"wpgo/internal/settings"
	"wpgo/internal/store"
	"wpgo/internal/wp"
	"wpgo/web"
)

// Version is shown in the admin footer and the generator meta tag.
const Version = "0.1.0"

type Server struct {
	cfg      config.Config
	store    *store.Store
	settings *settings.Store
	auth     *auth.Manager
	web      fs.FS
	tmpl     *templates
	engine   *gin.Engine
	rest     *restAPI

	// onPostSaved runs after any post or comment change (cache purge, plugin hooks).
	onPostSaved func(ctx context.Context, p *store.Post)
	onPurgeAll  func(ctx context.Context)

	cache   atomic.Pointer[pageCache] // wpgo-cache, rebuilt when its options change
	plugins *pluginSystem
	warm    warmState
}

func New(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) (*Server, error) {
	st := settings.New(pool)
	if err := st.Load(ctx); err != nil {
		return nil, err
	}
	secret := cfg.Secret
	if secret == "" {
		secret = st.String("auth_secret")
		if secret == "" {
			secret = auth.RandomToken(48)
			if err := st.Set(ctx, map[string]any{"auth_secret": secret}); err != nil {
				return nil, err
			}
		}
	}
	s := &Server{
		cfg:      cfg,
		store:    store.New(pool),
		settings: st,
		auth:     auth.NewManager(pool, secret),
		web:      overlayFS{base: web.FS(cfg.WebDir), dir: themeOverrideDir(cfg.UploadsDir)},
	}
	if s.golementorOn() {
		s.gweLoadCustomThemes(ctx) // Golementor custom themes stored in this site's database
	} else {
		s.gweUnloadCustomThemes() // the theme registry is shared by every server in the process
	}
	tmpl, err := newTemplates(s)
	if err != nil {
		return nil, err
	}
	s.tmpl = tmpl
	registerCoreBlocks(s)
	s.engine = s.routes()
	s.onPostSaved = s.purgePost
	s.onPurgeAll = func(context.Context) {
		if c := s.currentCache(); c != nil {
			c.PurgeAll()
		}
	}
	s.configureCache()
	if s.golementorOn() {
		s.gweMigrateActiveSavedTheme(ctx) // Golementor: a saved theme active from before Hello Golementor
	}
	s.startPlugins(ctx)
	return s, nil
}

// Handler serves the app. Admin and login URLs have no ".php" suffix; the WordPress
// spellings (/wp-admin/edit.php, /wp-login.php, ...) keep working: GET requests are
// redirected permanently, other methods are rewritten in place so forms and ajax
// calls aimed at the old paths still succeed.
func (s *Server) Handler() http.Handler {
	app := s.appHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pc := s.cache.Load(); pc != nil {
			pc.h.ServeHTTP(w, r)
			return
		}
		app.ServeHTTP(w, r)
	})
}

// appHandler is the app without the page cache.
func (s *Server) appHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if clean, ok := stripPHP(r.URL.Path); ok {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				target := clean
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusMovedPermanently)
				return
			}
			r.URL.Path, r.URL.RawPath = clean, ""
		}
		s.engine.ServeHTTP(w, r)
	})
}

var legacyPHP = regexp.MustCompile(`^/(wp-admin/[a-z0-9-]+|wp-login|wp-comments-post)\.php$`)

// stripPHP maps a WordPress-style .php URL to its clean path.
func stripPHP(p string) (string, bool) {
	m := legacyPHP.FindStringSubmatch(p)
	if m == nil {
		return "", false
	}
	if m[1] == "wp-admin/index" {
		return "/wp-admin/", true
	}
	return "/" + m[1], true
}

func (s *Server) routes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), accessLog(), requestOrigin(), securityHeaders(), s.loadUser(), s.requireInstall())
	r.HandleMethodNotAllowed = false

	s.static(r, "/wp-admin/assets", "admin/assets")
	for _, th := range allThemes() {
		if th.Custom {
			continue
		}
		if th.Slug == helloThemeSlug {
			s.static(r, th.URI(), "themes/"+th.Slug+"/assets", s.golementorOnly())
		} else {
			s.static(r, th.URI(), "themes/"+th.Slug+"/assets")
		}
	}
	s.static(r, "/wp-includes", "wp-includes")
	s.static(r, "/wp-admin/js", "wp-admin/js")
	s.static(r, "/wp-admin/css", "wp-admin/css")
	s.static(r, "/wp-admin/images", "wp-admin/images")
	s.static(r, strings.TrimSuffix(gweAssetsPath, "/"), "gowebeditor", s.golementorOnly()) // Golementor's editor and frontend assets
	r.GET("/wp-content/gowebeditor/theme-screenshot/:slug", s.golementorOnly(), s.gweThemeScreenshot)
	uploads := http.StripPrefix("/wp-content/uploads", http.FileServer(http.Dir(s.cfg.UploadsDir)))
	serveUpload := func(c *gin.Context) {
		// Uploaded files are data, never code: HTML or SVG uploads run in a sandbox.
		c.Header("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; media-src 'self'")
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
		uploads.ServeHTTP(c.Writer, c.Request)
	}
	r.GET("/wp-content/uploads/*filepath", serveUpload)
	r.HEAD("/wp-content/uploads/*filepath", serveUpload)

	r.GET("/wp-admin/install", s.installForm)
	r.POST("/wp-admin/install", s.installSubmit)
	r.GET("/wp-login", s.login)
	r.POST("/wp-login", s.login)
	r.POST("/wp-comments-post", s.commentPost)

	admin := r.Group("/wp-admin", s.requireLogin())
	admin.GET("/", s.dashboard)
	admin.GET("/index", s.dashboard)
	admin.POST("/index", s.quickDraft)
	admin.GET("/edit", s.postsList)
	admin.POST("/edit", s.postsBulk)
	admin.GET("/post-new", s.postNew)
	admin.GET("/post", s.postEdit)
	admin.POST("/post", s.postSave)
	admin.GET("/edit-tags", s.termsList)
	admin.POST("/edit-tags", s.termsAction)
	admin.GET("/term", s.termEdit)
	admin.POST("/term", s.termSave)
	admin.GET("/options-general", s.optionsScreen("general"))
	admin.GET("/options-writing", s.optionsScreen("writing"))
	admin.GET("/options-reading", s.optionsScreen("reading"))
	admin.GET("/options-discussion", s.optionsScreen("discussion"))
	admin.GET("/options-media", s.optionsScreen("media"))
	admin.GET("/options-permalink", s.optionsScreen("permalink"))
	admin.POST("/options", s.optionsSave)
	admin.POST("/options-permalink", s.optionsSave)
	admin.GET("/profile", s.profile)
	admin.POST("/profile", s.profileSave)
	admin.GET("/upload", s.mediaLibrary)
	admin.GET("/media-new", s.mediaNew)
	admin.POST("/media-new", s.mediaNew)
	admin.GET("/async-upload", s.asyncUpload)
	admin.POST("/async-upload", s.asyncUpload)
	admin.GET("/admin-ajax", s.adminAjax)
	admin.POST("/admin-ajax", s.adminAjax)
	admin.GET("/themes", s.themesScreen)
	admin.GET("/widgets", s.widgetsScreen)
	admin.GET("/nav-menus", s.navMenus)
	admin.POST("/nav-menus", s.navMenus)
	admin.GET("/edit-comments", s.commentsScreen)
	admin.POST("/edit-comments", s.commentsScreen) // Screen Options form
	admin.GET("/revision", s.revisionScreen)
	admin.GET("/users", s.usersScreen)
	admin.GET("/update-core", s.updateCore)
	admin.GET("/about", s.about)
	admin.GET("/tools", s.tools)
	admin.GET("/options-privacy", s.optionsPrivacy)
	admin.POST("/options-privacy", s.optionsPrivacy)
	admin.GET("/theme-install", s.themeInstall)
	admin.GET("/site-health", s.siteHealth)
	admin.GET("/export", s.exportScreen)
	admin.GET("/theme-editor", s.themeEditor)
	admin.GET("/plugin-editor", s.pluginEditor)
	admin.GET("/export-personal-data", s.privacyTool("export_personal_data"))
	admin.POST("/export-personal-data", s.privacyTool("export_personal_data"))
	admin.GET("/erase-personal-data", s.privacyTool("remove_personal_data"))
	admin.POST("/erase-personal-data", s.privacyTool("remove_personal_data"))
	admin.GET("/import", s.importScreen)
	admin.POST("/import", s.importScreen)
	admin.GET("/user-new", s.userNew)
	admin.POST("/user-new", s.userNew)
	admin.GET("/user-edit", s.userEdit)
	admin.POST("/user-edit", s.userEdit)
	admin.GET("/comment", s.commentScreen)
	admin.POST("/comment", s.commentScreen)
	admin.GET("/plugins", s.pluginsScreen)
	admin.POST("/plugins", s.pluginsScreen)
	admin.GET("/plugin-install", s.pluginInstall)
	admin.POST("/plugin-install", s.pluginInstall)
	admin.GET("/admin", s.pluginAdmin)
	admin.POST("/admin", s.pluginAdmin)
	admin.GET("/gowebeditor-theme-builder", s.golementorOnly(), s.gweThemeBuilder) // Golementor Theme Builder
	admin.POST("/gowebeditor-theme-builder", s.golementorOnly(), s.gweThemeBuilder)
	s.registerGoWebEditorProAdmin(admin) // Golementor Pro screens (gowebeditor_pro_admin.go)
	admin.GET("/customize", s.customizeScreen)
	admin.POST("/customize", s.customizeSave)
	admin.GET("/:screen", s.notYet)

	s.rest = s.restRoutes()
	s.registerGoWebEditorREST()
	restHandler := func(c *gin.Context) { s.rest.serve(c, c.Param("route")) }
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodHead} {
		r.Handle(m, "/wp-json/*route", restHandler)
		r.Handle(m, "/wp-json", restHandler)
	}

	r.NoRoute(s.gwePreview, s.site) // Golementor's preview iframe, then the site
	return r
}

// static serves an embedded directory with long-lived cache headers.
func (s *Server) static(r *gin.Engine, prefix, dir string, guards ...gin.HandlerFunc) {
	sub, err := fs.Sub(s.web, dir)
	if err != nil {
		panic(err)
	}
	h := http.StripPrefix(prefix, http.FileServer(http.FS(sub)))
	handler := func(c *gin.Context) {
		if strings.HasSuffix(c.Param("filepath"), "/") || strings.HasSuffix(c.Param("filepath"), ".php") {
			c.Status(http.StatusNotFound)
			return
		}
		if o, ok := s.overlay(); ok && o.overridden(dir+c.Param("filepath")) {
			c.Header("Cache-Control", "no-cache") // edited in the Theme File Editor: revalidate (Last-Modified)
		} else if s.cfg.WebDir == "" {
			c.Header("Cache-Control", "public, max-age=604800")
		}
		h.ServeHTTP(c.Writer, c.Request)
	}
	r.GET(prefix+"/*filepath", append(guards[:len(guards):len(guards)], handler)...)
	r.HEAD(prefix+"/*filepath", append(guards[:len(guards):len(guards)], handler)...)
}

func accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/wp-admin/assets/") || strings.HasPrefix(p, "/wp-includes/") || strings.HasPrefix(p, "/wp-content/") {
			return
		}
		log.Printf("%d %s %s %s", c.Writer.Status(), c.Request.Method, c.Request.URL.RequestURI(), time.Since(start).Round(time.Microsecond))
	}
}

// securityHeaders sets the default headers from the spec's security section.
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/wp-admin") || strings.HasPrefix(p, "/wp-login") {
			// wp-admin's Backbone/underscore templates (media library, customizer, …) compile with new Function(),
			// and the block editor previews uploads through blob: URLs.
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https:; media-src 'self' blob: https:; "+
				"style-src 'self' 'unsafe-inline'; font-src 'self' data:; script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
				"worker-src 'self' blob:; connect-src 'self'; frame-src 'self' https: blob:; object-src 'none'; base-uri 'self'; frame-ancestors 'self'; form-action 'self'")
		} else {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; media-src 'self' https:; "+
				"style-src 'self' 'unsafe-inline'; font-src 'self' data:; script-src 'self' 'unsafe-inline'; "+
				"frame-src 'self' https:; object-src 'none'; base-uri 'self'; frame-ancestors 'self'; form-action 'self'")
		}
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		c.Writer = &cspWriter{ResponseWriter: c.Writer, c: c} // gap-widgets: per-response CSP allow-list (gwe_embed_csp.go)
		c.Next()
	}
}

const (
	ctxUser    = "wpgo.user"
	ctxSession = "wpgo.session"
)

// loadUser resolves the login cookie to a user for every request (admin bar, edit links, comments).
func (s *Server) loadUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(auth.CookieName)
		if err == nil && token != "" {
			if sess, err := s.auth.Lookup(c, token); err == nil {
				if u, err := s.store.UserByID(c, sess.UserID); err == nil {
					c.Set(ctxUser, u)
					c.Set(ctxSession, sess)
				}
			}
		}
		c.Next()
	}
}

func (s *Server) requireInstall() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if s.settings.Installed() || p == "/wp-admin/install" ||
			strings.HasPrefix(p, "/wp-admin/assets/") || strings.HasPrefix(p, "/wp-includes/") ||
			strings.HasPrefix(p, "/wp-admin/js/") || strings.HasPrefix(p, "/wp-admin/css/") || strings.HasPrefix(p, "/wp-admin/images/") {
			c.Next()
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/install")
		c.Abort()
	}
}

// requireLogin is auth_redirect(): anonymous visitors go to the login form and come back afterwards.
func (s *Server) requireLogin() gin.HandlerFunc {
	return func(c *gin.Context) {
		u := currentUser(c)
		if u == nil && s.golementorOn() && gweProNoPriv(c) { // Golementor Pro: visitors' form submissions (wp_ajax_nopriv_)
			c.Next()
			return
		}
		if u == nil {
			c.Redirect(http.StatusFound, "/wp-login?redirect_to="+urlEscape(c.Request.URL.RequestURI())+"&reauth=1")
			c.Abort()
			return
		}
		// Subscribers may only reach their profile, like WordPress' read-only dashboard access.
		if !u.Can("read", nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
			c.Abort()
			return
		}
		c.Next()
	}
}

func currentUser(c *gin.Context) *store.User {
	if v, ok := c.Get(ctxUser); ok {
		return v.(*store.User)
	}
	return nil
}

func currentSession(c *gin.Context) *auth.Session {
	if v, ok := c.Get(ctxSession); ok {
		return v.(*auth.Session)
	}
	return nil
}

// nonce is wp_create_nonce() for the current visitor.
func (s *Server) nonce(c *gin.Context, action string) string {
	var uid int64
	var token string
	if u := currentUser(c); u != nil {
		uid = u.ID
	}
	if sess := currentSession(c); sess != nil {
		token = sess.Token
	}
	return s.auth.Nonce(action, uid, token)
}

// checkNonce is check_admin_referer(): verifies _wpnonce from the form or query string.
func (s *Server) checkNonce(c *gin.Context, action string) bool {
	n := c.PostForm("_wpnonce")
	if n == "" {
		n = c.Query("_wpnonce")
	}
	var uid int64
	var token string
	if u := currentUser(c); u != nil {
		uid = u.ID
	}
	if sess := currentSession(c); sess != nil {
		token = sess.Token
	}
	return s.auth.VerifyNonce(n, action, uid, token) > 0
}

// denyNonce renders the "link you followed has expired" screen (wp_nonce_ays()).
func (s *Server) denyNonce(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusForbidden, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Something went wrong.</title>`+
		`<link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head><body id="error-page">`+
		`<div class="wp-die-message">The link you followed has expired.</div><p><a href="javascript:history.back()">Please try again.</a></p></body></html>`)
	c.Abort()
}

// links returns a permalink builder for the current settings.
func (s *Server) links(ctx context.Context) *links.Builder {
	home := strings.TrimRight(s.settings.String("home"), "/")
	if home == "" {
		home = s.cfg.SiteURL
	}
	home = localOrigin(ctx, home)
	b := &links.Builder{
		Home:      home,
		Structure: s.settings.String("permalink_structure"),
		CatBase:   s.settings.String("category_base"),
		TagBase:   s.settings.String("tag_base"),
		Loc:       s.loc(),
	}
	b.Ancestors = func(p *store.Post) []*store.Post {
		a, _ := s.store.Ancestors(ctx, p)
		return a
	}
	b.FirstCategory = func(p *store.Post) *store.Term {
		m, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "category")
		if ts := m[p.ID]; len(ts) > 0 {
			return ts[0]
		}
		return nil
	}
	b.CategoryPath = func(t *store.Term) string {
		path := t.Slug
		cur := t
		for i := 0; cur.ParentID != 0 && i < 20; i++ {
			parent, err := s.store.GetTerm(ctx, cur.ParentID)
			if err != nil {
				break
			}
			path = parent.Slug + "/" + path
			cur = parent
		}
		return path
	}
	b.AuthorSlug = func(id int64) string {
		u, err := s.store.UserByID(ctx, id)
		if err != nil {
			return ""
		}
		return u.Nicename
	}
	return b
}

// loc is the site time zone.
func (s *Server) loc() *time.Location {
	return wp.Location(s.settings.String("timezone_string"), s.settings.Float("gmt_offset"))
}

func clientIP(c *gin.Context) string {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return c.Request.RemoteAddr
	}
	return host
}

// safeRedirect keeps redirects on this site (wp_safe_redirect()).
func safeRedirect(target, fallback string) string {
	if target == "" || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.Contains(target, "\\") {
		return fallback
	}
	return path.Clean("/"+strings.SplitN(target, "?", 2)[0]) + queryPart(target)
}

func queryPart(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[i:]
	}
	return ""
}

// purgeAll drops every cached page (settings changes affect the whole site).
func (s *Server) purgeAll(ctx context.Context) {
	if s.onPurgeAll != nil {
		s.onPurgeAll(ctx)
	}
}

type originKey struct{}

// requestOrigin remembers scheme://host of the request so links can follow it on local sites.
func requestOrigin() gin.HandlerFunc {
	return func(c *gin.Context) {
		scheme := "http"
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), originKey{}, scheme+"://"+c.Request.Host))
		c.Next()
	}
}

// localOrigin keeps a development site usable from any address: when the configured home URL is a
// local or private address (localhost, 127.x, 192.168.x, *.local …) and the request also comes in on
// one, links use the request's origin. Sites with a real domain always use the configured URL, so a
// forged Host header can't change their links.
func localOrigin(ctx context.Context, home string) string {
	if ctx == nil {
		return home
	}
	var origin string
	if gc, ok := ctx.(*gin.Context); ok && gc.Request != nil {
		origin, _ = gc.Request.Context().Value(originKey{}).(string)
	} else {
		origin, _ = ctx.Value(originKey{}).(string)
	}
	if origin == "" {
		return home
	}
	hu, err := url.Parse(home)
	if err != nil || !isLocalHost(hu.Hostname()) {
		return home
	}
	ou, err := url.Parse(origin)
	if err != nil || !isLocalHost(ou.Hostname()) || ou.Host == hu.Host {
		return home
	}
	return ou.Scheme + "://" + ou.Host + strings.TrimRight(hu.Path, "/")
}

func isLocalHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".test") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}
