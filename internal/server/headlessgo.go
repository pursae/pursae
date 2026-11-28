package server

import (
	"context"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

// HeadlessGo is a native headless CMS plugin: a Content-Type Builder (collection types, single
// types, components and dynamic zones), a Content Manager with draft & publish and i18n through
// Golylang, a delivery REST API under /wp-json/headlessgo/v1 with filters, sorting, pagination,
// populate and field selection, API tokens, a public-role permission matrix, CORS, webhooks,
// preview URLs, a GraphQL endpoint (queries, mutations, introspection), content history, scheduled
// publishing, bulk actions, media upload, bidirectional relations, edit locks, a review flow and
// GraphQL subscriptions (server-sent events).
//
// Headless mode is opt-in (the front mode defaults to "off"): once an administrator picks it, the
// site's front end does not render and visitors get a JSON 404, a plain 404 or a redirect to the
// front-end app, as configured. wp-admin, wp-login, the REST
// API, uploads and static assets keep working. The gate and the delivery API run in
// Server.Handler before the page cache (headlessServe), so neither is ever cached.
//
// Like every native plugin it is off on a fresh install (native_headlessgo) and while it is off its
// routes answer 404, its menu is gone and the front end renders as usual. Schemas, entries,
// tokens and webhooks stay in the database when it is deactivated.

const (
	hgSlug      = "headlessgo"
	hgSetting   = "native_headlessgo"   // the plugin is active
	hgOptions   = "headlessgo_settings" // hgSettings as one JSON object
	hgVersion   = "1.0.0"
	hgAssetVer  = "1.1.0" // admin script and stylesheet (cache busting)
	hgNamespace = "headlessgo/v1"
	hgAPIPrefix = "/wp-json/" + hgNamespace
	hgAdminPath = "/wp-admin/headlessgo"
)

// hgActions are the permissions of a content type, as in the users & permissions matrix.
var hgActions = []string{"find", "findOne", "create", "update", "delete"}

// hgSettings are the plugin's options.
type hgSettings struct {
	FrontMode        string              `json:"front_mode"` // json, plain, redirect, off
	RedirectURL      string              `json:"redirect_url"`
	RedirectKeepPath bool                `json:"redirect_keep_path"`
	AllowEditors     bool                `json:"allow_editors"` // logged-in users who can edit posts still see the front end
	CORSOrigins      []string            `json:"cors_origins"`  // allowed origins; "*" allows any
	Public           map[string][]string `json:"public"`        // type uid -> actions the public (no token) may use
	PreviewSecret    string              `json:"preview_secret"`
	PreviewTTL       int                 `json:"preview_ttl"`    // minutes a preview token stays valid
	DefaultLocale    string              `json:"default_locale"` // the locale while Golylang is off
	DefaultPageSize  int                 `json:"default_page_size"`
	MaxPageSize      int                 `json:"max_page_size"`
	GraphQL          bool                `json:"graphql"`
	SchemaWebhooks   bool                `json:"schema_webhooks"`  // send schema.update events
	WebhookAttempts  int                 `json:"webhook_attempts"` // attempts per delivery, the first included
	RateLimit        int                 `json:"rate_limit"`       // requests per minute per token or address; 0 = unlimited
	CacheMaxAge      int                 `json:"cache_max_age"`    // seconds of s-maxage on public reads; 0 = revalidate always
	NoPublicIntro    bool                `json:"no_public_introspection"`
	// Front-end users (headlessgo_users.go): WordPress accounts sign in at /auth/local and get a
	// short-lived token; the Authenticated matrix says what they may do on top of the public role.
	UserAuth      bool                `json:"user_auth"`
	UserRegister  bool                `json:"user_register"`  // POST /auth/local/register creates accounts
	UserTokenTTL  int                 `json:"user_token_ttl"` // minutes a user token stays valid
	Authenticated map[string][]string `json:"authenticated"`  // type uid -> actions any signed-in user may use
	OwnerOnly     map[string][]string `json:"owner_only"`     // type uid -> update/delete only on entries the user created
}

func hgDefaults() hgSettings {
	// FrontMode starts "off": switching the plugin on must not take the public site down. An
	// administrator who wants a headless-only front end picks json/plain/redirect explicitly,
	// and sites that saved a mode keep it, since the stored front_mode overrides this default.
	return hgSettings{FrontMode: "off", AllowEditors: true, Public: map[string][]string{}, PreviewTTL: 60, DefaultLocale: "en",
		DefaultPageSize: 25, MaxPageSize: 100, GraphQL: true, WebhookAttempts: 5}
}

// hgOn reports whether HeadlessGo is active: one cached option read.
func (s *Server) hgOn() bool { return s.settings.Bool(hgSetting) }

// hgOnly is the middleware of the plugin's admin routes: 404 while it is off.
func (s *Server) hgOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.hgOn() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

func (s *Server) hgLoad() hgSettings {
	st := hgDefaults()
	if v := s.settings.Get(hgOptions); v != nil {
		if raw, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(raw, &st)
		}
	}
	return hgSanitize(st)
}

func hgSanitize(st hgSettings) hgSettings {
	switch st.FrontMode {
	case "json", "plain", "redirect", "off":
	default:
		// An unset or unknown mode keeps the site's own front end, like hgDefaults.
		st.FrontMode = "off"
	}
	if st.FrontMode == "redirect" && !hgHTTPURL(st.RedirectURL) {
		st.FrontMode = "json"
	}
	st.Public = hgCleanMatrix(st.Public, hgActions)
	st.Authenticated = hgCleanMatrix(st.Authenticated, hgActions)
	st.OwnerOnly = hgCleanMatrix(st.OwnerOnly, []string{"update", "delete"})
	if st.UserTokenTTL <= 0 || st.UserTokenTTL > 7*24*60 {
		st.UserTokenTTL = 60
	}
	var origins []string
	for _, o := range st.CORSOrigins {
		if o = hgNormOrigin(o); o != "" && !containsStr(origins, o) {
			origins = append(origins, o)
		}
	}
	st.CORSOrigins = origins
	if st.PreviewTTL <= 0 || st.PreviewTTL > 7*24*60 {
		st.PreviewTTL = 60
	}
	st.DefaultLocale = strings.TrimSpace(st.DefaultLocale)
	if st.DefaultLocale == "" {
		st.DefaultLocale = "en"
	}
	if st.MaxPageSize <= 0 || st.MaxPageSize > 1000 {
		st.MaxPageSize = 100
	}
	if st.WebhookAttempts <= 0 || st.WebhookAttempts > 10 {
		st.WebhookAttempts = 5
	}
	if st.RateLimit < 0 || st.RateLimit > 1000000 {
		st.RateLimit = 0
	}
	if st.CacheMaxAge < 0 || st.CacheMaxAge > 31536000 {
		st.CacheMaxAge = 0
	}
	if st.DefaultPageSize <= 0 || st.DefaultPageSize > st.MaxPageSize {
		st.DefaultPageSize = min(25, st.MaxPageSize)
	}
	return st
}

// hgCleanMatrix keeps the known actions of a permission matrix, once each, and drops empty rows.
func hgCleanMatrix(m map[string][]string, known []string) map[string][]string {
	out := map[string][]string{}
	for uid, acts := range m {
		var ok []string
		for _, a := range acts {
			if containsStr(known, a) && !containsStr(ok, a) {
				ok = append(ok, a)
			}
		}
		if len(ok) > 0 {
			out[uid] = ok
		}
	}
	return out
}

func hgHTTPURL(v string) bool {
	u, err := url.Parse(strings.TrimSpace(v))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// hgNormOrigin reduces an allow-list line to scheme://host[:port], or "*".
func hgNormOrigin(o string) string {
	o = strings.TrimSpace(o)
	if o == "*" {
		return o
	}
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func (s *Server) hgSaveSettings(ctx context.Context, st hgSettings) error {
	err := s.settings.Set(ctx, map[string]any{hgOptions: hgSanitize(st)})
	s.hgPurge() // locales, public permissions and the cache lifetime change what reads answer
	return err
}

// hgDefaultLocale is the locale of new entries: Golylang's default language while it is active,
// else the plugin's own setting.
func (s *Server) hgDefaultLocale(ctx context.Context) string {
	if s.golylangOn() {
		if l := s.pllDefault(ctx); l != nil {
			return l.Slug
		}
	}
	return s.hgLoad().DefaultLocale
}

// hgLocale is one content locale.
type hgLocale struct {
	Code, Name string
	Default    bool
}

// hgLocales are the locales entries can have: Golylang's languages, or the single default one.
func (s *Server) hgLocales(ctx context.Context) []hgLocale {
	if s.golylangOn() {
		var out []hgLocale
		for _, l := range s.pllLanguages(ctx) {
			out = append(out, hgLocale{Code: l.Slug, Name: l.Name, Default: l.IsDefault})
		}
		if len(out) > 0 {
			return out
		}
	}
	d := s.hgLoad().DefaultLocale
	return []hgLocale{{Code: d, Name: d, Default: true}}
}

func (s *Server) hgValidLocale(ctx context.Context, code string) bool {
	for _, l := range s.hgLocales(ctx) {
		if l.Code == code {
			return true
		}
	}
	return false
}

// Plugins screen ------------------------------------------------------------------------------------

func (s *Server) hgPluginRow() pluginRow {
	r := pluginRow{Slug: hgSlug, Name: "Pursae Headless", Version: builtinPluginVersion, Native: true, Active: s.hgOn(),
		Description: "Turns the site into a headless CMS: a Content-Type Builder with collection types, single types, components and dynamic zones, a Content Manager with draft &amp; publish and translations, a REST API with filters, sorting, pagination and populate, API tokens, public permissions, CORS, webhooks, preview URLs and GraphQL. While it is active the front end does not render.",
		Author:      "Pursae"}
	if r.Active {
		r.SettingsURL = hgAdminPath + "?page=settings"
	}
	return r
}

func (s *Server) hgPluginAction(c *gin.Context, action string) {
	on := action == "activate"
	vals := map[string]any{hgSetting: on}
	if on {
		st := s.hgLoad()
		if st.PreviewSecret == "" {
			st.PreviewSecret = auth.RandomToken(32)
			vals[hgOptions] = hgSanitize(st)
		}
	}
	if err := s.settings.Set(c, vals); err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape(err.Error()))
		return
	}
	if on {
		s.hgEnsureTick(c)
	} else {
		s.hgDropTick(c)
	}
	s.hgRegFlush()
	s.purgeAll(c) // the front end appears or disappears
	c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
}

// hgMenuPages are the screens of the HeadlessGo menu.
var hgMenuPages = []struct{ Page, Title string }{
	{"", "Content Manager"}, {"builder", "Content-Type Builder"}, {"tokens", "API Tokens"}, {"webhooks", "Webhooks"}, {"graphql", "GraphQL"}, {"settings", "Settings"},
}

// hgMenu adds the HeadlessGo top-level menu after Settings while the plugin is active.
func (s *Server) hgMenu(menu []*menuTop, u *store.User, screen string) []*menuTop {
	if !s.hgOn() || u == nil || !u.Can("edit_posts", nil) {
		return menu
	}
	admin := u.Can("manage_options", nil)
	top := &menuTop{Slug: hgSlug, Title: "Pursae Headless", ID: "toplevel_page_headlessgo", Icon: "dashicons-rest-api"}
	cur := screen
	if p, ok := strings.CutPrefix(screen, hgSlug+"?page="); ok && (p == "entries" || p == "entry" || p == "history") {
		cur = hgSlug
	}
	for i, p := range hgMenuPages {
		if p.Page != "" && !admin {
			continue
		}
		slug := hgSlug
		if p.Page != "" {
			slug += "?page=" + p.Page
		}
		ms := &menuSub{Slug: slug, Title: template.HTML(p.Title), First: i == 0, Current: slug == cur}
		top.Current = top.Current || ms.Current
		top.Sub = append(top.Sub, ms)
	}
	at := len(menu)
	for i, m := range menu {
		if m.ID == "menu-settings" {
			at = i + 1
			m.Last = false
		}
	}
	top.Last = at == len(menu) || menu[at].Separator
	out := append([]*menuTop{}, menu[:at]...)
	out = append(out, top)
	return append(out, menu[at:]...)
}

// registerHeadlessGo wires the admin screens; they answer 404 while the plugin is off. The delivery
// API is served by headlessServe.
func (s *Server) registerHeadlessGo(admin *gin.RouterGroup) {
	admin.GET("/headlessgo", s.hgOnly(), s.hgAdmin)
	admin.POST("/headlessgo", s.hgOnly(), s.hgAdmin)
	admin.GET("/headlessgo/search", s.hgOnly(), s.hgAdminSearch)
	admin.POST("/headlessgo/graphql", s.hgOnly(), s.hgAdminGraphQL)
	admin.POST("/headlessgo/lock", s.hgOnly(), s.hgAdminLock)
}

// The gate --------------------------------------------------------------------------------------------

type hgEngineState struct {
	once sync.Once
	h    http.Handler
}

var hgEngines sync.Map // *Server => *hgEngineState

// hgEngine is the router of the delivery API (its own gin engine, outside the site's middleware).
func (s *Server) hgEngine() http.Handler {
	v, _ := hgEngines.LoadOrStore(s, &hgEngineState{})
	st := v.(*hgEngineState)
	st.once.Do(func() {
		r := gin.New()
		// Handlers use the gin.Context as their context: with the fallback it is the request's, so
		// a read whose client went away stops querying. Writes run to the end (a half-applied save
		// with its history and webhooks cut off would be worse than finishing it).
		r.ContextWithFallback = true
		if err := applyTrustedProxies(r, s.cfg.TrustedProxies); err != nil {
			log.Printf("headlessgo: trusted proxies: %v", err)
		}
		r.Use(gin.Recovery(), s.requestOrigin(), func(c *gin.Context) {
			// GraphQL decides per operation (hgGQLExecute: mutations do not stop).
			if m := c.Request.Method; m != http.MethodGet && m != http.MethodHead && !strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/graphql") {
				c.Request = c.Request.WithContext(context.WithoutCancel(c.Request.Context()))
			}
		})
		r.Any(hgAPIPrefix+"/*rest", s.hgAPI)
		r.Any(hgAPIPrefix, s.hgAPI)
		st.h = r
	})
	return st.h
}

// headlessServe is called by Handler for every request before the page cache. It serves the
// delivery API (with CORS) and, in headless mode, answers front-end requests itself. It reports
// whether it answered the request.
func (s *Server) headlessServe(w http.ResponseWriter, r *http.Request) bool {
	if !s.hgOn() || !s.settings.Installed() {
		return false
	}
	p := r.URL.Path
	if rr := r.URL.Query().Get("rest_route"); strings.Trim(p, "/") == "" && strings.HasPrefix(rr, "/"+hgNamespace) {
		r = r.Clone(r.Context())
		q := r.URL.Query()
		q.Del("rest_route")
		r.URL.Path, r.URL.RawPath, r.URL.RawQuery = "/wp-json"+rr, "", q.Encode()
		p = r.URL.Path
	}
	if p == hgAPIPrefix || strings.HasPrefix(p, hgAPIPrefix+"/") {
		st := s.hgLoad()
		if s.hgCORS(w, r, st) {
			return true
		}
		s.hgEngine().ServeHTTP(w, r)
		return true
	}
	st := s.hgLoad()
	if st.FrontMode == "off" || hgAlwaysOpen(r) {
		return false
	}
	if st.AllowEditors {
		if u := s.hgCookieUser(r); u != nil && u.Can("edit_posts", nil) {
			return false
		}
	}
	h := w.Header()
	h.Set("Cache-Control", "no-cache, must-revalidate, max-age=0, no-store, private")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("X-Content-Type-Options", "nosniff")
	switch st.FrontMode {
	case "redirect":
		target := strings.TrimRight(st.RedirectURL, "/")
		if st.RedirectKeepPath {
			target += r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
		} else {
			target = st.RedirectURL
		}
		http.Redirect(w, r, target, http.StatusFound)
	case "plain":
		h.Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not Found"))
	default:
		h.Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"data":null,"error":{"status":404,"name":"NotFoundError","message":"Not Found","details":{}}}`))
	}
	return true
}

// hgAlwaysOpen are the paths headless mode never closes (the OAuth endpoints too: MCP clients sign in there).
func hgAlwaysOpen(r *http.Request) bool {
	p := r.URL.Path
	// Whole segments only: a raw prefix let /wp-loginx?p=1 or /xmlrpcs?s= reach the site router (#15).
	for _, pre := range []string{"/wp-content/", "/wp-includes/", "/oauth/", "/.well-known/oauth-"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	for _, seg := range []string{"/wp-admin", "/wp-json"} {
		if pathUnder(p, seg) {
			return true
		}
	}
	if p == "/wp-admin.php" || p == "/wp-login" || p == "/wp-login.php" || maintRPCOrCron(p) {
		return true
	}
	if r.URL.Query().Has("rest_route") && strings.Trim(p, "/") == "" {
		return true
	}
	return p == "/robots.txt" || p == "/favicon.ico"
}

// hgCookieUser resolves the login cookie (the gate runs before the router's loadUser middleware).
func (s *Server) hgCookieUser(r *http.Request) *store.User {
	ck, err := r.Cookie(auth.CookieName)
	if err != nil || ck.Value == "" {
		return nil
	}
	sess, err := s.auth.Lookup(r.Context(), ck.Value)
	if err != nil {
		return nil
	}
	u, err := s.store.UserByID(r.Context(), sess.UserID)
	if err != nil {
		return nil
	}
	return u
}

// hgCORS adds the CORS headers of an allowed origin and answers preflight requests. It reports
// whether the request was answered (a preflight).
func (s *Server) hgCORS(w http.ResponseWriter, r *http.Request, st hgSettings) bool {
	origin := r.Header.Get("Origin")
	h := w.Header()
	h.Add("Vary", "Origin")
	allowed := origin != "" && hgOriginAllowed(st.CORSOrigins, origin)
	if allowed {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Type")
	}
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		if allowed {
			h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-Preview-Token")
			h.Set("Access-Control-Max-Age", "86400")
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func hgOriginAllowed(list []string, origin string) bool {
	o := hgNormOrigin(origin)
	if o == "" {
		return false
	}
	for _, a := range list {
		if a == "*" || a == o {
			return true
		}
	}
	return false
}
