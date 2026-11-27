// Package server wires HTTP routes to the admin screens, the login and install
// flows and the public site rendered by the active theme.
package server

import (
	"context"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"wpgo/internal/auth"
	"wpgo/internal/config"
	"wpgo/internal/i18n"
	"wpgo/internal/links"
	"wpgo/internal/metrics"
	"wpgo/internal/settings"
	"wpgo/internal/store"
	"wpgo/internal/wp"
	"wpgo/web"
)

// Version is shown in the admin footer and the generator meta tag, sent to Pursae Manager and compared
// with its core releases. It is a variable so release builds set it:
// go build -ldflags "-X wpgo/internal/server.Version=0.2.0" ./cmd/wpgo
var Version = "0.6.1"

type Server struct {
	cfg      config.Config
	metrics  *metrics.Metrics // nil unless PURSAE_METRICS=1 (UseMetrics); then the request middleware and /metrics are installed
	store    *store.Store
	settings *settings.Store
	auth     *auth.Manager
	web      fs.FS
	// schemaGuard caches the schema check of this binary against the database (schema_guard.go).
	schemaGuard schemaGuardState
	// themeLibrary is the directory source of the theme library (PURSAE_THEME_LIBRARY_DIR): one theme
	// package directory per slug. Nil: the library comes from Pursae Manager (theme_library.go).
	themeLibrary fs.FS
	library      libraryState
	// brandStock caches whether the logo is the stock Pursae mark (brand_mascot.go).
	brandStock  brandStockState
	tmpl        *templates
	engine      *gin.Engine
	trustedNets []*net.IPNet // PURSAE_TRUSTED_PROXIES as parsed for gin; requestIP uses the same list
	rest        *restAPI
	// staticRoots are the URL prefixes of the embedded asset directories (rtlStylesheets).
	staticRoots []staticRoot

	// onPostSaved runs after any post or comment change (cache purge, plugin hooks).
	onPostSaved func(ctx context.Context, p *store.Post)
	onPurgeAll  func(ctx context.Context)

	cache        atomic.Pointer[pageCache] // wpgo-cache, rebuilt when its options change
	cacheBuildMu sync.Mutex                // orders retirement and installation of caches
	longJobs     sync.WaitGroup            // long jobs running beside the scheduler loop (scheduler.go)
	longBusy     atomic.Bool               // a long job is running in this process
	plugins      *pluginSystem
	bmgr         bmgrRuntime // Backup Manager state (backup.go)
	warm         warmState

	// sandbox is set on a theme-preview sandbox (theme_preview.go): a second Server over its own
	// database schema, uploads and themes directory, served under /wp-admin/theme-preview/<slug>/.
	sandbox *sandboxInfo
	// previews are the site's theme-preview sandboxes (nil on a sandbox).
	previews *previewManager

	// lang holds the installed language packs (i18n.go).
	lang *i18n.Manager

	// mgr is the Pursae Manager client's state: the license answer and the queued events.
	mgr managerState

	// rt is the bounded background work: async writes, coalesced CDN purges (background.go).
	rt runtimeState

	// mediaIdx holds the uploads paths of replaced and re-cropped attachments: their file version,
	// focal point and moved paths (media_replace.go).
	mediaIdx mediaIndex

	wbWizMutationMu sync.Mutex // serializes updates to site imports and their removal records

	// geo is the local IP-to-country database (geoip.go).
	geo geoipState
}

// New builds the server of the site.
func New(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) (*Server, error) {
	return newServer(ctx, cfg, pool, nil)
}

// newServer builds the site's server, or with sb a theme-preview sandbox: no page cache, plugin
// runtime or ACF-Go start-up, and nothing written to the process-wide registries.
func newServer(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, sb *sandboxInfo) (*Server, error) {
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
	wbMoveLegacyUploads(cfg.UploadsDir)       // Pursae Web Builder: uploads folder of before the Pursae rename
	wbRemoveOldBuilderUploads(cfg.UploadsDir) // the old builder's generated files (webbuilder_cleanup.go)
	s := &Server{
		cfg:      cfg,
		store:    store.New(pool),
		settings: st,
		auth:     auth.NewManager(pool, secret),
		web:      overlayFS{base: web.FS(cfg.WebDir), dir: themeOverrideDir(cfg.UploadsDir)},
		sandbox:  sb,
	}
	if cfg.ThemeLibraryDir != "" && sb == nil {
		if st, err := os.Stat(cfg.ThemeLibraryDir); err != nil || !st.IsDir() {
			slog.Warn("theme library: PURSAE_THEME_LIBRARY_DIR is not a directory", "dir", cfg.ThemeLibraryDir)
		}
		s.themeLibrary = os.DirFS(cfg.ThemeLibraryDir)
	}
	s.initLanguages()
	if err := s.loadMediaIndex(ctx); err != nil { // replaced files' versions, focal points (media_replace.go)
		slog.Warn("media index could not be loaded", "err", err)
	}
	if sb != nil {
		return s.startSandbox(ctx)
	}
	if _, err := s.schemaStatus(ctx); err != nil { // logs a schema newer than this binary (schema_guard.go)
		slog.Warn("schema guard: the schema could not be checked", "err", err)
	}
	// After the sandbox return: a theme-preview sandbox starts no loops (shutdownSandbox Closes it when
	// it is dropped), so it must not register a child context with a long-lived parent.
	s.rt.startBackground(ctx)
	s.auth.Idle = s.adminsecIdle // AdminSecGo: idle session timeout
	s.previews = newPreviewManager(s)
	s.loadThemePackages() // theme packages installed under <data>/themes (theme_install.go)
	s.migrateRemovedThemes(ctx)
	s.sealStoredSecrets(ctx) // plain secrets left by an older version (secret_seal.go)
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
	if s.acfgoOn() {
		s.acfgoStart(ctx) // ACF-Go: content types, post types and taxonomies
	}
	s.startPlugins(ctx)
	s.bmgrRegisterSchedule() // Backup Manager: the scheduled-backup job action (backup_schedule.go)
	s.adminsecStart()
	s.gocdnStartHousekeeping()
	s.regenerateResume()    // Media: continue an interrupted "Regenerate all" run
	s.optimizeResume()      // Media › Optimization: continue an interrupted "Optimize existing images" run
	s.syncSearchStemAsync() // Search: stem in the site language (search_stem.go)
	return s, nil
}

// Handler serves the app. Admin and login URLs have no ".php" suffix; the WordPress
// spellings (/wp-admin/edit.php, /wp-login.php, ...) keep working: GET requests are
// redirected permanently, other methods are rewritten in place so forms and ajax
// calls aimed at the old paths still succeed.
func (s *Server) Handler() http.Handler {
	app := s.appHandler()
	// Outermost: probes and the request ID (ops_http.go); then gzip for what Pursae Cache does not serve
	// compressed itself (compress.go).
	return s.opsHandler(s.instrument(gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nw := newNonceWriter(w) // the response's CSP nonce (csp_nonce.go); every request, a websocket handshake too
		defer nw.close()
		w = nw
		if s.firewallServe(w, r) { // core firewall: rate limit and request rules (firewall.go)
			return
		}
		if s.adminsecOn() { // AdminSecGo: blocked IPs, the custom login URL, security headers (adminsec.go)
			var done bool
			if w, r, done = s.adminsecFront(w, r); done {
				return
			}
		}
		if s.maintenanceServe(w, r) { // Maintenance-Go: before the page cache (maintenance.go)
			return
		}
		if s.gocdnServe(w, r) { // GoCDN: image service, offloaded uploads (gocdn.go)
			return
		}
		if s.headlessServe(w, r) { // HeadlessGo: delivery API, CORS and headless mode (headlessgo.go)
			return
		}
		if pc := s.cache.Load(); pc != nil {
			pc.h.ServeHTTP(w, r)
			return
		}
		app.ServeHTTP(w, r)
	}))))
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
	// Multipart bodies keep at most 8 MB of files in memory per request (gin's default is 32 MB);
	// larger parts spool to the temp directory and are removed when the request ends, as parts over
	// 32 MB already did. Bounds RAM under concurrent uploads.
	r.MaxMultipartMemory = 8 << 20
	if err := applyTrustedProxies(r, s.cfg.TrustedProxies); err != nil {
		log.Printf("wpgo: PURSAE_TRUSTED_PROXIES: %v; trusting no proxy", err)
		_ = r.SetTrustedProxies(nil)
		s.trustedNets = nil
	} else {
		s.trustedNets, _ = trustedProxyNets(s.cfg.TrustedProxies) // parsed the same way, so it cannot fail here
		if strings.TrimSpace(s.cfg.TrustedProxies) == "" && strings.HasPrefix(s.cfg.SiteURL, "https://") {
			log.Printf("wpgo: PURSAE_TRUSTED_PROXIES is not set: X-Forwarded-For is believed from loopback and private networks; " +
				"set it to your proxy's address, or \"none\" (docs: operations/reverse-proxy-and-tls)")
		}
	}
	r.Use(gin.Recovery(), accessLog(), s.requestOrigin(), s.securityHeaders(), s.loadUser(), s.requireInstall(), s.acfgoMiddleware(), s.golylangRequest())
	r.HandleMethodNotAllowed = false
	r.Use(s.gocdnRewrite()) // GoCDN: media and asset URLs to the CDN (gocdn_rewrite.go)

	s.static(r, "/wp-admin/assets", "admin/assets")
	for _, th := range allThemes() {
		if th.Custom || th.Package != nil { // packages are installed at run time: packageThemeAsset serves them
			continue
		}
		s.static(r, th.URI(), "themes/"+th.Slug+"/assets")
	}
	s.static(r, "/wp-includes", "wp-includes")
	s.static(r, "/wp-admin/js", "wp-admin/js")
	s.static(r, "/wp-admin/css", "wp-admin/css")
	s.static(r, "/wp-admin/images", "wp-admin/images")
	uploads := http.StripPrefix("/wp-content/uploads", http.FileServer(noListFS{http.Dir(s.cfg.UploadsDir)}))
	serveUpload := func(c *gin.Context) {
		// Uploaded files are data, never code: HTML or SVG uploads run in a sandbox.
		c.Header("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; media-src 'self'")
		c.Request.URL.Path = legacyUploadPath(c.Request.URL.Path)
		if rel := strings.TrimPrefix(path.Clean(c.Request.URL.Path), "/wp-content/uploads/"); mediaIsPrivateRel(rel) {
			s.mediaPrivateServe(c, rel) // private files: only a signed, unexpired link (media_private.go)
			return
		}
		if s.serveMovedUpload(c) { // a path a replaced file left: redirect to its successor (media_replace.go)
			return
		}
		if rel := strings.TrimPrefix(path.Clean(c.Request.URL.Path), "/wp-content/uploads/"); strings.HasPrefix(rel, wbKitsDir+"/") {
			wbKitFilePolicy(c.Writer.Header(), rel) // a design kit's scripts may start Web Workers (webbuilder_kits.go)
		}
		c.Header("Cache-Control", s.uploadCacheControl(c))
		uploads.ServeHTTP(c.Writer, c.Request)
	}
	r.GET("/wp-content/uploads/*filepath", serveUpload)
	r.HEAD("/wp-content/uploads/*filepath", serveUpload)
	r.GET(pluginAssetsPath+"/:slug/assets/*filepath", s.pluginAsset) // plugin package assets (plugins_host.go)
	r.HEAD(pluginAssetsPath+"/:slug/assets/*filepath", s.pluginAsset)
	s.registerGontact(r) // Gontact Form front-end assets
	s.registerWebBuilder(r)
	s.registerWebBuilderRedirects(r) // old builder admin URLs -> 301 (before the /wp-admin routes: it adds a middleware)
	s.registerWebBuilderLiveFrame(r) // the element editor's live frame at the page's path (webbuilder_liveframe.go; a middleware)

	r.GET("/wp-admin/install", s.installForm)
	r.POST("/wp-admin/install", s.installSubmit)
	r.POST(cspReportPath, s.cspReport) // CSP violation reports (csp_report.go)
	r.GET("/wp-login", s.login)
	r.POST("/wp-login", s.login)
	r.POST("/wp-comments-post", s.commentPost)

	admin := r.Group("/wp-admin", s.requireLogin())
	admin.GET("/", s.dashboard)
	admin.GET("/wpgo-l10n.js", s.adminL10nScript) // translations of the admin scripts (i18n_adminjs.go)
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
	admin.GET("/options", s.optionsConfirm) // ?adminhash= / ?dismiss= of a pending admin e-mail change
	admin.POST("/options", s.optionsSave)
	admin.GET("/authorize-application", s.authorizeApplication)
	admin.POST("/authorize-application", s.authorizeApplication)
	admin.GET("/oauth-authorize", s.oauthAuthorize) // OAuth 2.1 consent screen for MCP clients (oauth.go)
	admin.POST("/oauth-authorize", s.oauthAuthorize)
	admin.POST("/options-permalink", s.optionsSave)
	admin.GET("/profile", s.profile)
	admin.POST("/profile", s.profileSave)
	admin.GET("/upload", s.mediaLibrary)
	admin.GET("/media-new", s.mediaNew)
	admin.POST("/media-new", s.mediaNew)
	admin.GET("/media-optimization", s.mediaOptimizationPage) // media_optimize.go
	admin.POST("/media-optimization", s.mediaOptimizationPage)
	admin.POST("/media-replace", s.mediaReplaceSubmit) // Replace media box of the attachment edit screen (media_replace_admin.go)
	admin.GET("/async-upload", s.asyncUpload)
	admin.POST("/async-upload", s.asyncUpload)
	admin.GET("/admin-ajax", s.adminAjax)
	admin.POST("/admin-ajax", s.adminAjax)
	admin.GET("/themes", s.themesScreen)
	admin.GET("/"+wbWizPageSlug, s.wbWizScreen)  // Web Builder › Import HTML site (webbuilder_siteimport_admin.go)
	admin.POST("/"+wbWizPageSlug, s.wbWizScreen) // upload, import, publish, remove
	admin.GET("/widgets", s.widgetsScreen)
	admin.GET("/nav-menus", s.navMenus)
	admin.POST("/nav-menus", s.navMenus)
	admin.GET("/edit-comments", s.commentsScreen)
	admin.POST("/edit-comments", s.commentsScreen) // Screen Options form
	admin.GET("/revision", s.revisionScreen)
	admin.GET("/users", s.usersScreen)
	admin.POST("/users", s.usersScreen) // bulk role change and the users search form (users.php)
	admin.GET("/update-core", s.updateCore)
	admin.POST("/update-core", s.updateCore) // Update Plugins, Update to version X (hub_updates.go, core_update.go)
	admin.GET("/about", s.about)
	admin.GET("/tools", s.tools)
	admin.POST("/tools", s.tools) // Revisions: clean up old revisions
	admin.GET("/options-privacy", s.optionsPrivacy)
	admin.POST("/options-privacy", s.optionsPrivacy)
	admin.GET("/options-license", s.optionsLicense) // Settings › License (license.go)
	admin.POST("/options-license", s.optionsLicense)
	admin.GET("/options-security", s.optionsSecurity) // Settings › Security: the script CSP (csp_admin.go)
	admin.POST("/options-security", s.optionsSecurity)
	admin.GET("/theme-install", s.themeInstall)
	admin.GET("/theme-build", s.themeBuild)
	admin.POST("/theme-install", s.themeInstall)
	if s.sandbox == nil { // theme previews with demo content (theme_preview.go)
		admin.GET("/theme-preview", s.themePreviewScreen)
		admin.POST("/theme-preview", s.themePreviewScreen)
		admin.Any("/theme-preview/*rest", s.themePreviewSite)
		r.Any(previewFrameRoute+"/*rest", s.themePreviewFrame) // the sandboxed preview: a token, no cookie (theme_preview_frame.go)
	}
	admin.GET("/site-health", s.siteHealth)
	admin.GET("/scheduled-actions", s.scheduledActionsScreen)
	admin.POST("/scheduled-actions", s.scheduledActionsScreen)
	admin.GET("/ai-access", s.aiAccessScreen) // Tools › AI Access: connect, grants, MCP activity (admin_ai_activity.go)
	admin.POST("/ai-access", s.aiAccessScreen)
	admin.GET("/export", s.exportScreen)
	admin.GET("/theme-editor", s.themeEditor)
	admin.GET("/plugin-editor", s.pluginEditor)
	admin.GET("/export-personal-data", s.privacyTool("export_personal_data"))
	admin.POST("/export-personal-data", s.privacyTool("export_personal_data"))
	admin.GET("/erase-personal-data", s.privacyTool("remove_personal_data"))
	admin.POST("/erase-personal-data", s.privacyTool("remove_personal_data"))
	admin.GET("/import", s.importScreen)
	admin.POST("/import", s.importScreen)
	admin.GET("/import-wordpress", s.wp2pScreen) // Tools › Import › From WordPress (wp2p_admin.go)
	admin.POST("/import-wordpress", s.wp2pScreen)
	admin.GET("/user-new", s.userNew)
	admin.POST("/user-new", s.userNew)
	admin.GET("/user-edit", s.userEdit)
	admin.POST("/user-edit", s.userEdit)
	admin.GET("/comment", s.commentScreen)
	admin.POST("/comment", s.commentScreen)
	admin.GET("/plugins", s.pluginsScreen)
	admin.POST("/plugins", s.pluginsScreen)
	admin.GET("/plugin-setup", s.pluginSetupPage) // plugin setup guides (plugin_setup.go)
	admin.POST("/plugin-setup", s.pluginSetupPage)
	admin.GET("/plugin-install", s.pluginInstall)
	admin.POST("/plugin-install", s.pluginInstall)
	admin.GET("/plugin-keys", s.pluginKeys)
	admin.GET("/plugin-build", s.pluginBuild)
	admin.POST("/plugin-keys", s.pluginKeys)
	admin.GET("/admin", s.pluginAdmin)
	admin.POST("/admin", s.pluginAdmin)
	s.acfgoRegisterRoutes(admin)    // ACF-Go screens (acfgo.go)
	s.registerSEOGoAdmin(admin)     // Pursae SEO screens (seogo_admin.go)
	s.registerFlipbook(r)           // Pursae Flipbook: assets (the /flipbook/N page is flipbookRoute in NoRoute)
	s.registerGooike(r, admin)      // Gooike: assets and screens (gooike.go)
	s.registerPixel(r, admin)       // Pursae Pixel: assets, screens, product feed (pixelgo.go)
	s.registerAnalytics(r, admin)   // Pursae Analytics: collector and screen (analytics.go)
	s.registerForum(r, admin)       // Pursae Forum: assets, Google sign-in, admin screen (forum.go)
	s.registerGoocommerce(r, admin) // Goocommerce: assets and screens (goocommerce.go)
	s.registerGoCDN(admin)          // GoCDN screens (gocdn.go)
	s.registerHeadlessGo(admin)     // HeadlessGo screens (headlessgo.go)
	admin.GET("/customize", s.customizeScreen)
	admin.POST("/customize", s.customizeSave)
	admin.GET("/:screen", s.notYet)

	s.rest = s.restRoutes()
	s.registerGontactREST()
	s.registerMailSMTPTrack(r) // Pursae SMTP: open/click tracking and delivery webhooks (mailsmtp_track.go)
	// Pursae Web Builder: public web-builder/v2/forms (webbuilder_forms.go)
	s.registerWebBuilderForms()
	s.registerGolylangREST() // pll/v1 (golylang_rest.go)
	s.registerSEOGoREST()    // Pursae SEO: seogo/v1, yoast/v1/get_head, rankmath/v1/getHead (seogo_editor.go)
	s.registerGooikeREST()   // Gooike: gooike/v1 consent log and geo (gooike_consent.go)
	s.registerPixelREST()    // Pursae Pixel: pixelgo/v1 server-side events (pixelgo_capi.go)
	s.registerForumREST()    // Pursae Forum: forum/v1 read-only lists (rest_forum.go)
	s.registerGooREST()      // Goocommerce: wc/store/v1 and wc/v3 (goo_rest.go)
	// Pursae Web Builder: web-builder/v2 (webbuilder_rest.go)
	s.registerWebBuilderREST()
	s.registerWebBuilderLiveFrameREST() // POST documents/{id}/live-frame (webbuilder_liveframe.go)
	s.registerPursaeHeadlessREST()
	s.registerPursaeREST()      // pursae/v1: plugins, themes, cache, SEO settings, option groups, MCP (rest_pursae*.go, mcp.go)
	s.registerForumManageREST() // pursae/v1/forum: manage forums, topics, settings (rest_pursae_forum.go)
	s.registerOAuth(r)
	restHandler := func(c *gin.Context) { s.rest.serve(c, c.Param("route")) }
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodHead} {
		r.Handle(m, "/wp-json/*route", restHandler)
		r.Handle(m, "/wp-json", restHandler)
	}

	// theme package assets, /<post>/embed/, site-imported design kits' files at the root, then the site
	r.NoRoute(s.packageThemeAsset, s.embedRoute, s.flipbookRoute, s.site)
	return r
}

// static serves an embedded directory with long-lived cache headers.
func (s *Server) static(r *gin.Engine, prefix, dir string, guards ...gin.HandlerFunc) {
	sub, err := fs.Sub(s.web, dir)
	if err != nil {
		panic(err)
	}
	s.staticRoots = append(s.staticRoots, staticRoot{prefix, dir})
	h := http.StripPrefix(prefix, http.FileServer(http.FS(sub)))
	handler := func(c *gin.Context) {
		if strings.HasSuffix(c.Param("filepath"), "/") || strings.HasSuffix(c.Param("filepath"), ".php") {
			c.Status(http.StatusNotFound)
			return
		}
		if dir == "wp-includes" && s.serveBrandOverride(c, c.Param("filepath")) { // the Hub's logo (brand.go)
			return
		}
		edited := false
		if o, ok := s.overlay(); ok && o.overridden(dir+c.Param("filepath")) {
			edited = true
			c.Header("Cache-Control", "no-cache") // edited in the Theme File Editor: revalidate (Last-Modified)
		} else if dir == "wp-includes" && strings.HasPrefix(c.Param("filepath"), brandDir) {
			// The shipped logo: a Hub logo replaces it at the same URL (brand.go), so browsers may
			// keep it only as long as the Hub's own (serveBrandOverride).
			c.Header("Cache-Control", "public, max-age=3600")
		} else if s.cfg.WebDir == "" {
			c.Header("Cache-Control", "public, max-age=604800")
		}
		if !edited && s.serveGzipped(c, sub, dir, strings.TrimPrefix(c.Param("filepath"), "/")) { // static_gzip.go
			return
		}
		h.ServeHTTP(c.Writer, c.Request)
	}
	r.GET(prefix+"/*filepath", append(guards[:len(guards):len(guards)], handler)...)
	r.HEAD(prefix+"/*filepath", append(guards[:len(guards):len(guards)], handler)...)
}

// securityHeaders sets the default headers from the spec's security section.
func (s *Server) securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		p := c.Request.URL.Path
		mode := s.cspMode()
		if strings.HasPrefix(p, "/wp-admin") || strings.HasPrefix(p, "/wp-login") {
			// wp-admin's Backbone/underscore templates (media library, customizer, …) compile with new Function(),
			// and the block editor previews uploads through blob: URLs.
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https:; media-src 'self' blob: https:; "+
				"style-src 'self' 'unsafe-inline'; font-src 'self' data:; script-src "+cspScriptSrc(mode, true)+"; "+
				"worker-src 'self' blob:; connect-src 'self'; frame-src 'self' https: blob:; object-src 'none'; base-uri 'self'; frame-ancestors 'self'; form-action 'self'")
			cspReportOnly(h, mode, true)
		} else {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; media-src 'self' https:; "+
				"style-src 'self' 'unsafe-inline'; font-src 'self' data:; script-src "+cspScriptSrc(mode, false)+"; "+
				"frame-src 'self' https:; object-src 'none'; base-uri 'self'; frame-ancestors 'self'; form-action 'self'")
			cspReportOnly(h, mode, false)
			if !strings.HasPrefix(p, "/wp-json") {
				s.cspAllowExtra(c) // the site's allowed external sources (csp_extra.go); merged into HTML responses only
			}
		}
		if s.requestHTTPS(c.Request) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		c.Writer = &cspWriter{ResponseWriter: c.Writer, c: c} // per-response CSP allow-list (csp_embed.go)
		c.Writer = newNoStoreWriter(c)                        // no-store for admin pages and signed-in requests (nocache.go)
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
		var u *store.User
		token, err := c.Cookie(auth.CookieName)
		if err == nil && token != "" {
			lookup := s.auth.Lookup
			if isBackgroundPoll(c) { // polling is no activity: the idle timeout still runs
				lookup = s.auth.LookupPassive
			}
			if sess, err := lookup(c, token); err == nil {
				if got, err := s.store.UserByID(c, sess.UserID); err == nil {
					u = got
					c.Set(ctxUser, u)
					c.Set(ctxSession, sess)
				}
			}
		}
		if u == nil && !anonBoundBody(c) {
			return
		}
		// Every request is request-scoped, including anonymous ones, so a plugin cannot publish
		// or send mail as a visitor merely because it holds posts:write or mail:send.
		c.Request = c.Request.WithContext(withPluginRequest(c.Request.Context(), u))
		c.Next()
	}
}

// isBackgroundPoll reports requests a page sends on its own while it stays open: the admin
// heartbeat (action in the query or, as heartbeat.js sends it, the POST body) and the Headless
// entry lock heartbeat.
func isBackgroundPoll(c *gin.Context) bool {
	p := strings.TrimSuffix(c.Request.URL.Path, ".php")
	switch p {
	case "/wp-admin/admin-ajax":
		action := c.Query("action")
		if action == "" && c.Request.Method == http.MethodPost {
			// Runs before the session is known: only a URL-encoded body (what heartbeat.js sends) is
			// read, never a multipart one, which would spool its files to disk.
			_ = c.Request.ParseForm()
			action = c.Request.PostForm.Get("action")
		}
		return action == "heartbeat"
	case "/wp-admin/headlessgo/lock":
		return true
	}
	return false
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
		if u == nil {
			c.Redirect(http.StatusFound, s.loginPath()+"?redirect_to="+urlEscape(c.Request.URL.RequestURI())+"&reauth=1")
			c.Abort()
			return
		}
		// Subscribers may only reach their profile, like WordPress' read-only dashboard access.
		if !u.Can("read", nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
			c.Abort()
			return
		}
		// Forum participants, subscribers and customers use the front end only (admin_access.go).
		if s.denyFrontEndAdmin(c, u) {
			return
		}
		if s.adminsecGate(c, u) { // AdminSecGo: forced password change, 2FA required by the role
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

// endSessionsAfterPasswordChange ends the sessions of the user whose password was just changed, so a stolen
// session cookie does not survive it. The session that made the change (when it is the user's own) stays.
func (s *Server) endSessionsAfterPasswordChange(ctx context.Context, c *gin.Context, userID int64) {
	var keep int64
	if c != nil {
		if sess := currentSession(c); sess != nil && sess.UserID == userID {
			keep = sess.ID
		}
	}
	_ = s.auth.DestroyOthers(ctx, userID, keep)
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
	s.adminHTML(c, http.StatusForbidden, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Something went wrong.</title>`+
		`<link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head><body id="error-page">`+
		`<div class="wp-die-message">The link you followed has expired.</div><p>`+historyBackLink("Please try again.")+`</p></body></html>`)
	c.Abort()
}

// historyBackLink is wp_die()'s back link (href="javascript:history.back()") without a javascript: URL,
// which a nonce CSP blocks: a link plus a nonced script that goes back in history. label is HTML.
func historyBackLink(label string) string {
	return `<a href="#" id="wpgo-history-back">` + label + `</a><script` + cspNonceAttr() +
		`>document.getElementById("wpgo-history-back").addEventListener("click",function(e){e.preventDefault();history.back();});</script>`
}

// configuredLinks is links() on the configured home URL only, never the request's Host (see
// localOrigin). Links that carry a key or token, and links sent by e-mail, use it: WordPress builds
// them from the home/siteurl option, and a Host header is chosen by whoever sends the request.
func (s *Server) configuredLinks(ctx context.Context) *links.Builder {
	return s.links(withConfiguredOrigin(ctx))
}

// withConfiguredOrigin hides the request origin from localOrigin for everything built from ctx.
func withConfiguredOrigin(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, originKey{}, "")
}

// configuredHome is the configured home URL without a trailing slash.
func (s *Server) configuredHome() string {
	home := strings.TrimRight(s.settings.String("home"), "/")
	if home == "" {
		home = strings.TrimRight(s.cfg.SiteURL, "/")
	}
	return home
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
		return s.ancestors(ctx, p) // request memo (site_memo.go)
	}
	b.FirstCategory = func(p *store.Post) *store.Term {
		if ts := s.postTerms(ctx, p.ID, "category"); len(ts) > 0 { // request memo (site_memo.go)
			return ts[0]
		}
		return nil
	}
	b.CategoryPath = func(t *store.Term) string {
		return s.categoryPath(ctx, t) // request memo (site_memo.go)
	}
	b.AuthorSlug = func(id int64) string {
		if u := s.displayUser(ctx, id); u != nil { // request memo (site_memo.go)
			return u.Nicename
		}
		return ""
	}
	b.TypeHierarchical = siteTypeHierarchical
	if s.settings.String("show_on_front") == "page" {
		b.FrontPageID = int64(s.settings.Int("page_on_front"))
	}
	b.TypeBase = siteTypeBase                        // public Pursae Custom Fields post types (site.go)
	b.LinkFilter = s.golylangLinkFilter(ctx, b.Home) // Golylang: URLs carry their language
	return b
}

// loc is the site time zone.
func (s *Server) loc() *time.Location {
	return wp.Location(s.settings.String("timezone_string"), s.settings.Float("gmt_offset"))
}

// clientIP is the visitor's address: the connection's peer, or the address a trusted proxy forwarded
// (see applyTrustedProxies). The login throttle, comment flood check and audit logs key on it.
func clientIP(c *gin.Context) string {
	if ip := c.ClientIP(); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return c.Request.RemoteAddr
	}
	return host
}

// defaultTrustedProxies are the addresses a reverse proxy normally has when it runs next to the server:
// loopback and private networks. A client reaching the server directly from the internet is not among
// them, so it cannot choose its own address with an X-Forwarded-For header. The private ranges stay in
// the default because in Docker a proxy on the host (or in another container) reaches the server from a
// bridge address (172.16/12 and the like), not loopback. A server whose port is reachable from other
// machines on a private network should set PURSAE_TRUSTED_PROXIES to its proxy's address; the production
// compose file publishes the port on 127.0.0.1 only.
const defaultTrustedProxies = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"

// applyTrustedProxies sets which peers may report the visitor address (PURSAE_TRUSTED_PROXIES: a comma separated
// list of IPs and CIDRs; empty means defaultTrustedProxies; "none" trusts nobody). Without this gin would
// believe X-Forwarded-For from every client, and behind a proxy every visitor would share the proxy's address.
func applyTrustedProxies(r *gin.Engine, spec string) error {
	return r.SetTrustedProxies(trustedProxyList(spec))
}

// trustedProxyList splits PURSAE_TRUSTED_PROXIES (see applyTrustedProxies) into its entries.
func trustedProxyList(spec string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = defaultTrustedProxies
	}
	var list []string
	if !strings.EqualFold(spec, "none") {
		for _, p := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			list = append(list, p)
		}
	}
	return list
}

// trustedProxyNets parses PURSAE_TRUSTED_PROXIES the way gin does: a bare IP becomes a /32 or /128 network.
func trustedProxyNets(spec string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, p := range trustedProxyList(spec) {
		if !strings.Contains(p, "/") {
			ip := net.ParseIP(p)
			if ip == nil {
				return nil, &net.ParseError{Type: "IP address", Text: p}
			}
			if ip.To4() != nil {
				p += "/32"
			} else {
				p += "/128"
			}
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, err
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// peerTrusted reports whether r's immediate peer may supply forwarded client headers
// (X-Forwarded-For, X-Forwarded-Proto). A unix-socket peer is trusted, as gin trusts it;
// any other peer must be in PURSAE_TRUSTED_PROXIES (see isTrustedProxy).
func (s *Server) peerTrusted(r *http.Request) bool {
	if r == nil {
		return false
	}
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && strings.HasPrefix(la.Network(), "unix") {
		return true
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(peer))
	if ip == nil {
		return false
	}
	return s.isTrustedProxy(ip)
}

// requestHTTPS is the scheme the application renders for, and the scheme the page cache stores
// (cache.Options.TrustForwarded is peerTrusted). Real TLS counts. X-Forwarded-Proto counts only
// when it is exactly "https" and the immediate peer is a trusted proxy; a direct client cannot
// put its response under the https cache key.
func (s *Server) requestHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return s.peerTrusted(r) && r.Header.Get("X-Forwarded-Proto") == "https"
}

// requestIP is clientIP for the http.Handler-level gates that run before gin (the Pursae Security site block
// list, the maintenance IP whitelist). It follows gin's ClientIP with the same trusted proxies: a peer that is a
// trusted proxy may report the visitor in X-Forwarded-For (then X-Real-IP), read right to left up to the first
// untrusted hop; any other peer is the visitor.
func (s *Server) requestIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !s.peerTrusted(r) {
		return peer
	}
	for _, name := range []string{"X-Forwarded-For", "X-Real-IP"} {
		items := strings.Split(strings.Join(r.Header.Values(name), ","), ",")
		if len(items) == 1 && strings.TrimSpace(items[0]) == "" {
			continue
		}
		for i := len(items) - 1; i >= 0; i-- {
			v := strings.TrimSpace(items[i])
			ip := net.ParseIP(v)
			if ip == nil {
				break
			}
			if i == 0 || !s.isTrustedProxy(ip) {
				return v
			}
		}
	}
	return peer
}

func (s *Server) isTrustedProxy(ip net.IP) bool {
	for _, n := range s.trustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// safeRedirect keeps redirects on this site (wp_safe_redirect()).
func safeRedirect(target, fallback string) string {
	// Browsers delete tab, CR and LF from a URL before reading it, so "/<TAB>/host" would become "//host":
	// any control character makes the target unsafe.
	if target == "" || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.Contains(target, "\\") || hasControlChar(target) {
		return fallback
	}
	return path.Clean("/"+strings.SplitN(target, "?", 2)[0]) + queryPart(target)
}

// hasControlChar reports whether s contains an ASCII control character (including tab, CR and LF) or DEL.
func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
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
func (s *Server) requestOrigin() gin.HandlerFunc {
	return func(c *gin.Context) {
		scheme := "http"
		if s.requestHTTPS(c.Request) {
			scheme = "https"
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), originKey{}, scheme+"://"+c.Request.Host))
		c.Next()
	}
}

// localOrigin keeps a development site usable from any address: when the configured home URL is a
// local or private address (localhost, 127.x, 192.168.x, *.local …) and the request also comes in on
// one, links use the request's origin. Sites with a real domain always use the configured URL, so a
// forged Host header can't change their links. Only rendered pages follow the request: links that
// carry a key and links sent by e-mail use configuredLinks, and sendMail maps the request origin
// back to the configured home in every message.
func localOrigin(ctx context.Context, home string) string {
	if ctx == nil {
		return home
	}
	var origin string
	if v, ok := ctx.Value(originKey{}).(string); ok {
		origin = v // set on this context itself (withConfiguredOrigin clears it)
	} else if gc, ok := ctx.(*gin.Context); ok && gc.Request != nil {
		origin, _ = gc.Request.Context().Value(originKey{}).(string)
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
