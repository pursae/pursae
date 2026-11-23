package server

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"wpgo/internal/config"
	"wpgo/internal/store"
)

// AdminSecGo is WordPress Go's native security plugin: e-mailed one-time codes (MFA), an
// authenticator app (2FA, RFC 6238) with MFA as its fallback, passkeys (WebAuthn), backup codes, remembered devices,
// per-role enforcement, a custom login URL, lockouts, password policy, session management,
// hardening toggles, admin alerts and an activity log. Like the other native plugins it is off
// on a fresh install, and while it is off nothing of it runs: every hook below starts with
// adminsecOn(). Deactivating keeps users' factors (they are simply not asked for) and brings back
// /wp-login. docs/adminsecgo-features.md is the feature inventory.
//
// Files: adminsec.go (settings, plugin switch, front gate, hide login, headers, menu),
// adminsec_login.go (login hooks, challenges, lockouts, devices), adminsec_account.go (the user's
// Security screen, profile and Users list), adminsec_admin.go (the AdminSecGo screens),
// adminsec_audit.go (activity log, alerts, purge), adminsec_password.go (policy, strength),
// adminsec_totp.go and adminsec_qr.go (TOTP and the QR code), adminsec_passkey.go (passkeys).

const (
	asSlug    = "adminsecgo"
	asSetting = "native_adminsecgo"
	asOption  = "adminsecgo_settings"

	asDeviceCookie = "wpgo_adminsec_device"
	asEscapeEnv    = "PURSAE_ADMINSEC_DISABLE_HIDE_LOGIN"
)

// asSettings is the plugin's one JSON option.
type asSettings struct {
	// Two-factor
	RequireRoles map[string]string `json:"require_roles"` // role -> "" | "mfa" | "2fa"
	GraceDays    int               `json:"grace_days"`
	RememberDays int               `json:"remember_days"` // 0: no "remember this device"

	// Passkeys (adminsec_passkey.go)
	Passkeys       bool `json:"passkeys"`         // users may register passkeys and sign in with them
	PasskeyTwoStep bool `json:"passkey_two_step"` // a passkey sign-in counts as two-step login (no code asked)

	// Login
	HideLogin        bool   `json:"hide_login"`
	LoginSlug        string `json:"login_slug"`
	HideRedirect     string `json:"hide_redirect"` // empty: 404
	GenericErrors    bool   `json:"generic_errors"`
	BlockEnumeration bool   `json:"block_enumeration"`
	CaptchaForms     bool   `json:"captcha_forms"` // CAPTCHA on login, register and lost password (Contact Form keys)

	// Lockouts
	IPMaxFailures    int    `json:"ip_max_failures"`
	UserMaxFailures  int    `json:"user_max_failures"`
	FailWindow       int    `json:"fail_window"`     // minutes
	LockoutMinutes   int    `json:"lockout_minutes"` // first lockouts
	LongLockoutAfter int    `json:"long_lockout_after"`
	LongLockoutHours int    `json:"long_lockout_hours"`
	AllowList        string `json:"allow_list"`
	BlockList        string `json:"block_list"`
	BlockListSite    bool   `json:"block_list_site"` // blocked IPs lose the whole site, not just the login

	// Passwords
	PwMinLength   map[string]int `json:"pw_min_length"`   // per role
	PwMinStrength map[string]int `json:"pw_min_strength"` // per role, 0-4
	PwBlockCommon bool           `json:"pw_block_common"`
	PwCheckLogin  bool           `json:"pw_check_login"` // weak current password: change it at login
	PwAllowWeak   bool           `json:"pw_allow_weak"`  // test sites: no policy, no forced change for a weak password
	PwBreached    bool           `json:"pw_breached"`    // opt-in: refuse passwords found in data breaches (HIBP k-anonymity)

	// Sessions
	IdleMinutes int `json:"idle_minutes"`

	// Hardening
	Headers           bool   `json:"headers"`
	HSTSMaxAge        int    `json:"hsts_max_age"`
	HSTSSubdomains    bool   `json:"hsts_subdomains"`
	HSTSPreload       bool   `json:"hsts_preload"`
	FrameOptions      string `json:"frame_options"`
	ReferrerPolicy    string `json:"referrer_policy"`
	PermissionsPolicy string `json:"permissions_policy"`
	DisableFileEdit   bool   `json:"disable_file_edit"`
	HideVersion       bool   `json:"hide_version"`
	RESTVisitors      bool   `json:"rest_restrict"` // opt-in: REST only for logged-in users
	RESTAllow         string `json:"rest_allow"`    // extra route prefixes visitors keep

	// Firewall (firewall.go; the core runs it with the defaults while the plugin is off)
	RateLimit      int  `json:"rate_limit"`      // requests per minute and IP, 0: off
	Firewall       bool `json:"firewall"`        // the WAF rules
	AlertIntegrity bool `json:"alert_integrity"` // e-mail when the daily integrity scan finds something new

	// Alerts and log
	AlertNewIP    bool   `json:"alert_new_ip"`
	AlertLockout  bool   `json:"alert_lockout"`
	AlertEmail    string `json:"alert_email"` // empty: the site's admin e-mail
	LogRetention  int    `json:"log_retention"`
	LogFailedSkip bool   `json:"log_failed_skip"` // don't log failed logins (busy sites)
}

var asRoles = []string{"administrator", "editor", "author", "contributor", "subscriber"}

func asDefaults() asSettings {
	return asSettings{
		RequireRoles: map[string]string{}, GraceDays: 7, RememberDays: 30, Passkeys: true, PasskeyTwoStep: true,
		LoginSlug: "", GenericErrors: true, BlockEnumeration: true,
		IPMaxFailures: 5, UserMaxFailures: 5, FailWindow: 15, LockoutMinutes: 20, LongLockoutAfter: 4, LongLockoutHours: 24,
		PwMinLength:   map[string]int{"administrator": 12, "editor": 10, "author": 10, "contributor": 8, "subscriber": 8},
		PwMinStrength: map[string]int{"administrator": 3, "editor": 2, "author": 2, "contributor": 1, "subscriber": 0},
		PwBlockCommon: true, PwCheckLogin: true,
		Headers: true, HSTSMaxAge: 31536000, FrameOptions: "SAMEORIGIN", ReferrerPolicy: "strict-origin-when-cross-origin",
		PermissionsPolicy: "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()",
		DisableFileEdit:   true, HideVersion: true,
		RateLimit: fwDefaultRateLimit, Firewall: true, AlertIntegrity: true,
		AlertNewIP: true, AlertLockout: true, LogRetention: 90,
	}
}

// adminsecOn reports whether the plugin is active: one cached option read.
func (s *Server) adminsecOn() bool { return s.settings.Bool(asSetting) }

type asCache struct {
	src map[string]any // the stored option the settings were parsed from (kept: its address stays unique)
	st  asSettings
}

var asCached atomic.Pointer[asCache]

// asLoad returns the settings over the defaults (parsed once per stored version of the option).
func (s *Server) asLoad() asSettings {
	raw := s.settings.Get(asOption)
	m, _ := raw.(map[string]any)
	if c := asCached.Load(); c != nil && m != nil && reflect.ValueOf(c.src).Pointer() == reflect.ValueOf(m).Pointer() {
		return c.st
	}
	st := asDefaults()
	if raw != nil {
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &st)
		}
	}
	if st.RequireRoles == nil {
		st.RequireRoles = map[string]string{}
	}
	if st.PwMinLength == nil {
		st.PwMinLength = map[string]int{}
	}
	if st.PwMinStrength == nil {
		st.PwMinStrength = map[string]int{}
	}
	if m != nil {
		asCached.Store(&asCache{src: m, st: st})
	}
	return st
}

func (s *Server) asSave(ctx context.Context, st asSettings) error {
	if err := s.settings.Set(ctx, map[string]any{asOption: st}); err != nil {
		return err
	}
	s.purgeAll(ctx) // login links, the generator tag and headers live in cached pages
	return nil
}

// --- the plugin switch ---------------------------------------------------------------------

func (s *Server) adminsecPluginRow() pluginRow {
	r := pluginRow{Slug: asSlug, Name: "Pursae Security", Version: builtinPluginVersion, Native: true, Active: s.adminsecOn(),
		Description: "Login security for Pursae: e-mailed one-time codes (MFA), authenticator app two-factor (2FA) with backup codes and remembered devices, per-role enforcement, a custom login URL, login lockouts, IP allow and block lists, password policy, session management, security headers and hardening, admin alerts and an activity log. While it is off, nothing of it runs; users' factors are kept.",
		Author:      "Pursae"}
	if r.Active {
		r.SettingsURL = "/wp-admin/admin?page=" + asSlug + "-settings"
	}
	return r
}

func (s *Server) adminsecPluginAction(c *gin.Context, action string) error {
	if action == "deactivate" && s.adminsecOn() {
		s.asAudit(c, asEvent{Event: "plugin_deactivated", Severity: "warning", Object: "Pursae Security", Message: "Pursae Security deactivated: second factors are no longer asked for and the login URL is /wp-login again."})
	}
	if err := s.settings.Set(c, map[string]any{asSetting: action == "activate"}); err != nil {
		return err
	}
	if action == "activate" {
		_, _ = s.store.Pool.Exec(c, `INSERT INTO jobs(name, run_at, recurrence)
			SELECT 'adminsecgo_daily', now() + interval '1 hour', 'daily' WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE name = 'adminsecgo_daily' AND recurrence <> '')`)
		s.asAudit(c, asEvent{Event: "plugin_activated", Severity: "notice", Object: "Pursae Security", Message: "Pursae Security activated."})
	}
	s.purgeAll(c)
	return nil
}

// adminsecStart is the startup hook server.go calls. The login URL is read per request
// (s.loginPath), so there is nothing to prime any more.
func (s *Server) adminsecStart() {}

// --- custom login URL --------------------------------------------------------------------

// asEscapeHatch is the recovery switch: PURSAE_ADMINSEC_DISABLE_HIDE_LOGIN=1 (or `wpgo adminsec
// disable-hide-login`, which turns the setting off for good) brings /wp-login back.
func asEscapeHatch() bool {
	v := strings.ToLower(strings.TrimSpace(config.Env(asEscapeEnv)))
	return v == "1" || v == "true" || v == "yes"
}

func (st asSettings) hideActive() bool {
	return st.HideLogin && st.LoginSlug != "" && !asEscapeHatch()
}

// loginPath is wp_login_url()'s path: /wp-login, or the custom slug while it hides the login.
func (s *Server) loginPath() string {
	if s.adminsecOn() {
		if st := s.asLoad(); st.hideActive() {
			return "/" + st.LoginSlug
		}
	}
	return "/wp-login"
}

var asSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,63}$`)

// asReservedSlug lists paths a login slug may not take.
func asReservedSlug(slug string) bool {
	switch slug {
	case "wp-login", "wp-admin", "wp-json", "wp-content", "wp-includes", "wp-comments-post", "feed", "comments", "author",
		"category", "tag", "page", "search", "embed", "xmlrpc", "wp-cron", "login", "admin", "dashboard", "wp-signup", "wp-activate":
		return true
	}
	return strings.HasPrefix(slug, "wp-")
}

type asCtxKey int

const (
	asLoginAllowed asCtxKey = iota // the request came in on the custom login slug
	asDenied                       // a hidden URL: answer with the 404 page
)

// asAdminOpen are wp-admin paths visitors keep while the login is hidden (assets, ajax, admin-post).
func asAdminOpen(p string) bool {
	for _, pre := range []string{"/wp-admin/assets/", "/wp-admin/js/", "/wp-admin/css/", "/wp-admin/images/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	switch strings.TrimSuffix(p, ".php") {
	case "/wp-admin/admin-ajax", "/wp-admin/admin-post", "/wp-admin/install":
		return true
	}
	return false
}

func asIsLogin(p string) bool {
	p = strings.ToLower(strings.TrimRight(p, "/"))
	return p == "/wp-login" || p == "/wp-login.php"
}

func asIsAdmin(p string) bool {
	return p == "/wp-admin" || strings.HasPrefix(p, "/wp-admin/")
}

// adminsecFront runs first for every request while the plugin is on: blocked IPs, the custom login
// URL (its slug is served as /wp-login; /wp-login and wp-admin are hidden from visitors) and the
// response wrapper that sets the security headers.
func (s *Server) adminsecFront(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, bool) {
	if !s.adminsecOn() || !s.settings.Installed() {
		return w, r, false
	}
	st := s.asLoad()
	w = &asWriter{ResponseWriter: w, s: s, st: st, r: r}
	ip := s.requestIP(r)
	if st.BlockListSite && st.BlockList != "" && maintIPAllowed(st.BlockList, ip) && !maintIPAllowed(st.AllowList, ip) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("403 Forbidden: access from your IP address has been blocked.\n"))
		return w, r, true
	}
	if !st.hideActive() {
		return w, r, false
	}
	p := r.URL.Path
	slug := "/" + st.LoginSlug
	switch {
	case p == slug || p == slug+"/":
		r = r.WithContext(context.WithValue(r.Context(), asLoginAllowed, true))
		r.URL.Path, r.URL.RawPath = "/wp-login", ""
	case asIsLogin(p):
		if r.URL.Query().Get("action") == "postpass" { // password-protected posts keep working, as with WPS Hide Login
			return w, r, false
		}
		nr := s.asDeny(w, r, st)
		return w, nr, nr == nil
	case asIsAdmin(p) && !asAdminOpen(p):
		if s.maintUser(r) != nil {
			return w, r, false
		}
		if tp := strings.TrimSuffix(p, ".php"); tp == "/wp-admin/authorize-application" || tp == "/wp-admin/oauth-authorize" { // REST app authorization starts at the login
			http.Redirect(w, r, slug+"?redirect_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return w, r, true
		}
		nr := s.asDeny(w, r, st)
		return w, nr, nr == nil
	}
	return w, r, false
}

// asDeny answers a hidden URL: a redirect when one is configured, otherwise the request goes on
// as /wp-login marked denied, and the login handler renders the theme's 404 page.
func (s *Server) asDeny(w http.ResponseWriter, r *http.Request, st asSettings) *http.Request {
	if st.HideRedirect != "" {
		http.Redirect(w, r, st.HideRedirect, http.StatusFound)
		return nil
	}
	r = r.WithContext(context.WithValue(r.Context(), asDenied, true))
	r.URL.Path, r.URL.RawPath, r.URL.RawQuery = "/wp-login", "", ""
	r.Method = http.MethodGet
	return r
}

// adminsecLoginGate is the login handler's first step: a hidden /wp-login answers 404.
func (s *Server) adminsecLoginGate(c *gin.Context) bool {
	if !s.adminsecOn() {
		return false
	}
	ctx := c.Request.Context()
	denied, _ := ctx.Value(asDenied).(bool)
	if !denied {
		st := s.asLoad()
		allowed, _ := ctx.Value(asLoginAllowed).(bool)
		if !st.hideActive() || allowed || c.Query("action") == "postpass" {
			return false
		}
	}
	c.Header("Cache-Control", "no-cache, must-revalidate, max-age=0")
	s.renderSite(c, &siteQuery{Kind: "404"})
	return true
}

// asWriter sets the configured security headers just before the response's headers go out, and
// points any Location left on /wp-login at the custom login URL.
type asWriter struct {
	http.ResponseWriter
	s     *Server
	st    asSettings
	r     *http.Request
	wrote bool
}

func (w *asWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *asWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		w.finalize()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *asWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *asWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *asWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *asWriter) finalize() {
	h, st := w.Header(), w.st
	if st.hideActive() {
		if loc := h.Get("Location"); loc != "" {
			if u, err := url.Parse(loc); err == nil && asIsLogin(u.Path) && u.Query().Get("action") != "postpass" {
				u.Path = "/" + st.LoginSlug
				h.Set("Location", u.String())
			}
		}
	}
	if !st.Headers {
		return
	}
	h.Set("X-Content-Type-Options", "nosniff")
	if st.FrameOptions != "" {
		h.Set("X-Frame-Options", st.FrameOptions)
	}
	if st.ReferrerPolicy != "" {
		h.Set("Referrer-Policy", st.ReferrerPolicy)
	}
	if st.PermissionsPolicy != "" {
		h.Set("Permissions-Policy", st.PermissionsPolicy)
	}
	if w.s.requestHTTPS(w.r) && st.HSTSMaxAge > 0 {
		v := "max-age=" + strconv.Itoa(st.HSTSMaxAge)
		if st.HSTSSubdomains {
			v += "; includeSubDomains"
		}
		if st.HSTSPreload {
			v += "; preload"
		}
		h.Set("Strict-Transport-Security", v)
	}
	// The Content-Security-Policy stays the core's (and its per-response allow-list) untouched.
}

// adminsecIdle is the idle session timeout (auth.Manager.Idle).
func (s *Server) adminsecIdle() time.Duration {
	if !s.adminsecOn() {
		return 0
	}
	return time.Duration(s.asLoad().IdleMinutes) * time.Minute
}

// --- e-mail delivery ---------------------------------------------------------------------

// asMailReady reports whether e-mail can actually be delivered: WPGO Mail SMTP with a configured
// SMTP or API mailer, or the core PURSAE_SMTP_* transport. Without it MFA cannot be set up.
func (s *Server) asMailReady() (bool, string) {
	if s.mailSMTPOn() {
		o := s.msLoad()
		if o.General.DoNotSend {
			return false, "Pursae SMTP has “Do Not Send” switched on, so no e-mail leaves the site."
		}
		if p := o.primary(); p.Mailer != "" && p.Mailer != "mail" && msConnReady(p) {
			return true, ""
		}
	}
	if s.cfg.SMTPHost != "" {
		return true, ""
	}
	return false, "E-mail delivery is not set up: configure an SMTP server or an e-mail API in Pursae SMTP (or the PURSAE_SMTP_* environment variables) so codes can reach users."
}

// msConnReady reports whether a connection's mailer has its required settings.
func msConnReady(c *msConn) bool {
	for _, d := range msMailers {
		if d.Slug != c.Mailer {
			continue
		}
		for _, f := range d.Fields {
			if f.Type == "text" || f.Type == "password" {
				return strings.TrimSpace(c.Opts[f.Key]) != ""
			}
		}
		return true
	}
	return false
}

// asMailFixURL is where the "set up e-mail" links go.
func (s *Server) asMailFixURL() string {
	if s.mailSMTPOn() {
		return "/wp-admin/admin?page=" + msSlug
	}
	return "/wp-admin/plugins?plugin_status=all&s=Mail+SMTP"
}

// --- admin chrome ------------------------------------------------------------------------

var asAdminPages = [][2]string{
	{"admin?page=adminsecgo", "Dashboard"},
	{"admin?page=adminsecgo-settings", "Settings"},
	{"admin?page=adminsecgo-lockouts", "Lockouts"},
	{"admin?page=adminsecgo-integrity", "Integrity"},
	{"admin?page=adminsecgo-log", "Activity Log"},
}

// adminsecMenu adds the top-level AdminSecGo menu (administrators), the Security entry under
// Users/Profile (everyone) and drops the file editors when they are disabled.
func (s *Server) adminsecMenu(c *gin.Context, menu []*menuTop, screen string) []*menuTop {
	if !s.adminsecOn() {
		return menu
	}
	u := currentUser(c)
	st := s.asLoad()
	for _, m := range menu {
		if m.ID == "menu-users" {
			sub := &menuSub{Slug: "admin?page=" + asSlug + "-account", Title: "Security", Current: screen == "admin?page="+asSlug+"-account"}
			m.Sub = append(m.Sub, sub)
			m.Current = m.Current || sub.Current
		}
		if st.DisableFileEdit {
			kept := m.Sub[:0]
			for _, sb := range m.Sub {
				if sb.Slug != "theme-editor" && sb.Slug != "plugin-editor" {
					kept = append(kept, sb)
				}
			}
			m.Sub = kept
		}
	}
	if u == nil || !u.Can("manage_options", nil) {
		return menu
	}
	top := &menuTop{Slug: asAdminPages[0][0], Title: "Pursae Security", ID: "toplevel_page_adminsecgo", Icon: "dashicons-shield"}
	for i, p := range asAdminPages {
		ms := &menuSub{Slug: p[0], Title: template.HTML(p[1]), First: i == 0, Current: p[0] == screen}
		top.Current = top.Current || ms.Current
		top.Sub = append(top.Sub, ms)
	}
	at := len(menu)
	for i, m := range menu {
		if m.ID == "menu-users" {
			at = i + 1
		}
	}
	out := make([]*menuTop, 0, len(menu)+1)
	out = append(out, menu[:at]...)
	out = append(out, top)
	return append(out, menu[at:]...)
}

// adminsecFileEditBlocked is DISALLOW_FILE_EDIT for the Theme and Plugin File Editors.
func (s *Server) adminsecFileEditBlocked(c *gin.Context) bool {
	if !s.adminsecOn() || !s.asLoad().DisableFileEdit {
		return false
	}
	c.String(http.StatusForbidden, "File editing is disabled on this site (Pursae Security › Settings › Hardening).")
	return true
}

// asHideVersion reports whether front-end markup leaves out the WordPress Go version.
func (s *Server) asHideVersion() bool {
	return s.adminsecOn() && s.asLoad().HideVersion
}

// publicVersion is the version for front-end markup and feeds.
func (s *Server) publicVersion() string {
	if s.asHideVersion() {
		return ""
	}
	return Version
}

// asStripVersion replaces ?ver=<version> on front-end asset URLs with a site-specific hash.
func (s *Server) asStripVersion(c *gin.Context, b []byte) []byte {
	if !s.asHideVersion() {
		return b
	}
	p := c.Request.URL.Path
	if asIsAdmin(p) || asIsLogin(p) {
		return b
	}
	str := string(b)
	if !strings.Contains(str, "ver="+Version) {
		return b
	}
	h := s.asHash("ver", Version)[:8]
	str = strings.ReplaceAll(str, "ver="+Version+"'", "ver="+h+"'")
	str = strings.ReplaceAll(str, "ver="+Version+"\"", "ver="+h+"\"")
	str = strings.ReplaceAll(str, "ver="+Version+"&", "ver="+h+"&")
	return []byte(str)
}

// --- REST and enumeration --------------------------------------------------------------------

// asRESTVisitorRoutes are the REST routes the front end calls for anonymous visitors: oEmbed, the contact form,
// Web Builder forms, Pursae Cookie's consent log and geo check, and Pursae Pixel's server events and the public Commerce Store API (cart, checkout; its own permission checks apply)
// and Pursae Headless' delivery API (served before the REST dispatcher with its own tokens, listed so the rule is explicit). Exact route
// prefixes, not whole namespaces, so admin routes those namespaces gain later stay blocked.
var asRESTVisitorRoutes = []string{"/oembed/", "/" + gontactNamespace + "/", "/web-builder/v2/forms/",
	"/" + gooikeNamespace + "/consent", "/" + gooikeNamespace + "/geo", "/" + pixelNamespace + "/event",
	"/" + gooStoreNS + "/", "/" + hgNamespace + "/", wp2pRouteMigrate + "/"} // migrate: the WP to Pursae plugin, authenticated by its token

// adminsecRESTBlocked answers REST requests visitors may not make: user listing (enumeration) and,
// when opted in, everything but the routes the front end needs. method is the effective method after
// X-HTTP-Method-Override / ?_method= (the one the dispatcher routes by), not the raw request method.
func (s *Server) adminsecRESTBlocked(c *gin.Context, route, method string, u *store.User) bool {
	if u != nil {
		return false
	}
	r := "/" + strings.Trim(route, "/")
	reading := method == http.MethodGet || method == http.MethodHead
	if reading && s.blockEnumeration() && (r == "/wp/v2/users" || strings.HasPrefix(r, "/wp/v2/users/")) {
		restErr(c, http.StatusUnauthorized, "rest_user_cannot_view", "Sorry, you are not allowed to list users.")
		return true
	}
	if !s.adminsecOn() {
		return false
	}
	st := s.asLoad()
	if !st.RESTVisitors {
		return false
	}
	allowed := append([]string(nil), asRESTVisitorRoutes...)
	for _, l := range lines(st.RESTAllow) {
		if l = strings.TrimSpace(l); l != "" {
			allowed = append(allowed, "/"+strings.Trim(l, "/"))
		}
	}
	for _, a := range allowed {
		if r == strings.TrimSuffix(a, "/") || strings.HasPrefix(r+"/", strings.TrimSuffix(a, "/")+"/") {
			return false
		}
	}
	restErr(c, http.StatusUnauthorized, "rest_login_required", "The REST API on this site is only available to logged-in users.")
	return true
}

// adminsecAuthorQuery hides ?author=N from visitors (user enumeration).
func (s *Server) adminsecAuthorQuery(c *gin.Context) bool {
	if currentUser(c) != nil || !s.blockEnumeration() {
		return false
	}
	if v := c.Query("author"); v != "" {
		if _, err := strconv.Atoi(strings.TrimSpace(v)); err == nil || strings.Contains(v, ",") {
			s.renderSite(c, &siteQuery{Kind: "404"})
			return true
		}
	}
	return false
}

// asBlockAuthorSitemap leaves the users sitemap empty while enumeration is blocked.
func (s *Server) asBlockAuthorSitemap() bool {
	return s.blockEnumeration()
}

// blockEnumeration reports whether visitors are kept from listing accounts (REST users, /?author=N, the users
// sitemap). This is the core's own default, so a site is safe without enabling any plugin: PURSAE_EXPOSE_USERS=1 turns
// it off, and when AdminSecGo is active its "block enumeration" setting (on by default) decides instead.
func (s *Server) blockEnumeration() bool {
	if s.adminsecOn() {
		return s.asLoad().BlockEnumeration
	}
	return !s.cfg.ExposeUsers
}

// --- recovery from the command line --------------------------------------------------------

// AdminSecCLI runs `wpgo adminsec <command>`: disable-hide-login, reset-user <login>, unlock-all,
// deactivate. It works on the database directly, for an administrator locked out of the site.
func AdminSecCLI(ctx context.Context, pool *pgxpool.Pool, args []string) (string, error) {
	if len(args) == 0 {
		return "usage: wpgo adminsec disable-hide-login | reset-user <login> | unlock-all | deactivate", nil
	}
	switch args[0] {
	case "disable-hide-login":
		_, err := pool.Exec(ctx, `UPDATE settings SET value = jsonb_set(value, '{hide_login}', 'false') WHERE key = $1 AND jsonb_typeof(value) = 'object'`, asOption)
		return "The custom login URL is off: log in at /wp-login (restart the server if it is running, or set " + asEscapeEnv + "=1).", err
	case "reset-user":
		if len(args) < 2 {
			return "", errFmt("reset-user needs a login")
		}
		login := args[1]
		tag, err := pool.Exec(ctx, `DELETE FROM adminsec_factors WHERE user_id = (SELECT id FROM users WHERE lower(login) = lower($1) OR lower(email) = lower($1) LIMIT 1)`, login)
		for _, table := range []string{"adminsec_devices", "adminsec_passkeys"} {
			if err == nil {
				_, err = pool.Exec(ctx, `DELETE FROM `+table+` WHERE user_id = (SELECT id FROM users WHERE lower(login) = lower($1) OR lower(email) = lower($1) LIMIT 1)`, login)
			}
		}
		return "Second factors and passkeys removed for " + args[1] + " (" + strconv.FormatInt(tag.RowsAffected(), 10) + " record).", err
	case "unlock-all":
		_, err := pool.Exec(ctx, `UPDATE adminsec_lockouts SET released = true WHERE until > now()`)
		if err == nil {
			_, err = pool.Exec(ctx, `DELETE FROM login_attempts WHERE NOT success`)
		}
		return "All lockouts released.", err
	case "deactivate":
		_, err := pool.Exec(ctx, `INSERT INTO settings(key, value) VALUES ($1, 'false') ON CONFLICT (key) DO UPDATE SET value = 'false'`, asSetting)
		return "Pursae Security is deactivated (restart the server if it is running).", err
	}
	return "", errFmt("unknown adminsec command " + args[0])
}

type errFmt string

func (e errFmt) Error() string { return string(e) }

// asRoleNames maps role slugs to names (settings screens).
func (s *Server) asRoleNames(ctx context.Context) []store.Role {
	roles, err := s.store.Roles(ctx)
	if err != nil || len(roles) == 0 {
		for _, r := range asRoles {
			roles = append(roles, store.Role{Slug: r, Name: strings.ToUpper(r[:1]) + r[1:]})
		}
	}
	return roles
}

// asMu serializes lockout creation so concurrent failures create one lockout.
var asMu sync.Mutex
