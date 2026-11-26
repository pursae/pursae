package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"wpgo/internal/auth"
	"wpgo/internal/settings"
	"wpgo/internal/store"
)

// Maintenance-Go is a native plugin, a clone of WebFactory's Maintenance with the parts of its Pro
// version that fit a single site. While maintenance mode is on, visitors get a designed "back soon"
// page with 503 Service Unavailable and Retry-After (or 200 in Coming Soon mode); logged-in users,
// whitelisted IPs, search engines (optional), holders of the secret access link or the site
// password see the site as usual. wp-admin and wp-login are never blocked.
//
// There are two switches, as in WordPress: the plugin (native_maintenance, Plugins screen; off on a
// fresh install, and while it is off nothing of it runs or shows) and maintenance mode itself
// (maintSettings.Enabled: the settings screen and the admin bar).
//
// The gate runs in Server.Handler before the page cache, so a closed site never serves a cached page
// and the maintenance page never enters the cache. Every settings change, the on/off switches and
// the countdown's auto-disable purge the cache.

const (
	maintSlug    = "maintenance"
	maintSetting = "native_maintenance"   // the plugin is active
	maintOptions = "maintenance_settings" // maintSettings as one JSON object (the original's mtnc-options)

	maintPreviewParam = "maintenance-preview" // shows the page even while the mode is off, as in the original
	maintAccessCookie = "wpgo_mtnc_access"    // the secret access link was followed
	maintUnlockCookie = "wpgo_mtnc_unlocked"  // the site password was entered
)

// maintSettings are the plugin's options. JSON names follow the original's option keys where it has one.
type maintSettings struct {
	Enabled bool   `json:"status"`
	Mode    string `json:"mode"` // "maintenance" (503 + Retry-After) or "coming_soon" (200)

	// Themes and Design
	Theme           string `json:"theme_global"`
	Heading         string `json:"header_text"`
	Content         string `json:"content"` // HTML, written by an administrator (unfiltered_html)
	FooterText      string `json:"footer_text"`
	LogoURL         string `json:"logo"`
	LogoWidth       int    `json:"logo_width"`
	BgType          string `json:"background_type"` // image, color, gradient
	BgImageURL      string `json:"background_image"`
	BgSize          string `json:"background_size_opt"`
	BgPosition      string `json:"background_position"`
	BgColor         string `json:"background_color"`
	BgColor2        string `json:"background_color_2"` // second gradient stop
	BgBlur          int    `json:"background_blur"`    // px
	Overlay         int    `json:"overlay_opacity"`    // 0–90 %
	ContentPosition string `json:"content_position"`   // middle, left, right, top, bottom
	ContentWidth    int    `json:"content_width"`
	ContentBox      bool   `json:"content_overlay"` // text sits on a translucent card
	FontFamily      string `json:"body_font"`
	FontSize        int    `json:"body_font_size"`
	FontColor       string `json:"body_font_color"`
	HeadingColor    string `json:"heading_color"`
	LinkColor       string `json:"body_link_color"`
	AccentColor     string `json:"controls_color"` // buttons, countdown, progress bar
	CustomCSS       string `json:"custom_css"`

	// Pursae Web Builder: the post id of a published Theme Builder template used as the page (0 = built-in page).
	WBTemplate int64 `json:"web_builder_page"`

	// Schedule: with the mode on, the page appears at StartDate (site time zone) instead of at once.
	StartDate string `json:"start_date"` // "2006-01-02T15:04"; empty = from now on

	// Modules
	Countdown       bool              `json:"countdown"`
	CountdownEnd    string            `json:"expiry_date"` // "2006-01-02T15:04" in the site's time zone
	CountdownLabel  string            `json:"countdown_label"`
	AutoDisable     bool              `json:"auto_disable"` // switch maintenance mode off when the countdown ends
	Progress        bool              `json:"progress"`
	ProgressPercent int               `json:"progress_percent"`
	ProgressLabel   string            `json:"progress_label"`
	Subscribe       bool              `json:"subscribe"`
	SubscribeTitle  string            `json:"subscribe_title"`
	SubscribeButton string            `json:"subscribe_button"`
	SubscribeThanks string            `json:"subscribe_thanks"`
	SubscribeNotify bool              `json:"subscribe_notify"` // e-mail the site admin about each subscriber
	DoubleOptIn     bool              `json:"double_optin"`     // subscribers confirm their address by a link before they count
	NotifyOnOpen    bool              `json:"notify_on_open"`   // e-mail the subscribers when maintenance mode ends
	NotifySubject   string            `json:"notify_subject"`
	NotifyBody      string            `json:"notify_body"`
	GDPR            bool              `json:"gdpr"`
	GDPRText        string            `json:"gdpr_text"`
	GDPRPolicyLink  bool              `json:"gdpr_policy_link"`
	Social          map[string]string `json:"social"`
	SocialNewTab    bool              `json:"social_target"`
	CustomHTML      string            `json:"custom_html"`

	// SEO
	Title           string `json:"title"` // %sitetitle%, %sitetagline%
	MetaDescription string `json:"description"`
	FaviconURL      string `json:"favicon"`
	SocialImageURL  string `json:"social_preview"`
	NoIndex         bool   `json:"noindex"`
	ExcludeSE       bool   `json:"excludese"` // show the normal site to search engine crawlers
	RetryAfter      int    `json:"retry_after"`
	Analytics       string `json:"analytics"` // Google Analytics measurement ID (G-…)

	// Access
	ShowLoggedIn    bool     `json:"show_logged_in"`
	BypassRoles     []string `json:"roles"` // roles of logged-in users who see the site
	IPWhitelist     string   `json:"ip_whitelist"`
	URLRules        string   `json:"per_url_settings"` // "", whitelist, blacklist
	URLList         string   `json:"per_url_enable_disable"`
	SecretKey       string   `json:"direct_access_link"` // home/?<key>
	SecretHours     int      `json:"direct_access_hours"`
	SitePassword    string   `json:"site_password"`
	PasswordButton  bool     `json:"password_button"`
	PasswordMessage string   `json:"login_message"`
	PasswordButtonT string   `json:"login_button_text"`
	PasswordWrong   string   `json:"login_wrong_password_text"`
	LoginButton     bool     `json:"login_button"`
	LoginTooltip    string   `json:"wplogin_button_tooltip"`

	// Advanced
	AdminBarMenu bool `json:"toolbar_menu"`
	BlockREST    bool `json:"block_rest"` // the original's "Enable WordPress Rest API" (Pro) inverted
	BlockFeeds   bool `json:"block_feeds"`
}

// maintSocialNetworks are the social icons the page can show, in display order (dashicons glyphs).
var maintSocialNetworks = []struct{ Key, Label, Icon string }{
	{"facebook", "Facebook", "dashicons-facebook-alt"},
	{"twitter", "X (Twitter)", "dashicons-twitter"},
	{"instagram", "Instagram", "dashicons-instagram"},
	{"linkedin", "LinkedIn", "dashicons-linkedin"},
	{"youtube", "YouTube", "dashicons-youtube"},
	{"pinterest", "Pinterest", "dashicons-pinterest"},
	{"whatsapp", "WhatsApp", "dashicons-whatsapp"},
	{"reddit", "Reddit", "dashicons-reddit"},
	{"twitch", "Twitch", "dashicons-twitch"},
	{"spotify", "Spotify", "dashicons-spotify"},
	{"email", "E-mail", "dashicons-email-alt"},
}

// maintRoles are the roles the Access tab offers, in WordPress order.
var maintRoles = []string{"administrator", "editor", "author", "contributor", "subscriber"}

// maintDefaults are a fresh install's settings: the original's defaults (texts, login button, every
// logged-in user passes), with 503 on, as WordPress Go's choice for search engines.
func maintDefaults() maintSettings {
	st := maintSettings{
		Mode:            "maintenance",
		Heading:         "Site is undergoing maintenance",
		Content:         "<p>Maintenance mode is on</p>\n<p>Site will be available soon. Thank you for your patience!</p>",
		FooterText:      "&copy; %sitetitle% %year%",
		LogoWidth:       220,
		BgSize:          "cover",
		BgPosition:      "center center",
		ContentPosition: "middle",
		ContentWidth:    760,
		FontSize:        16,
		CountdownLabel:  "We'll be back in",
		ProgressPercent: 75,
		ProgressLabel:   "Work in progress",
		SubscribeTitle:  "Get notified when we're back",
		SubscribeButton: "Notify me",
		SubscribeThanks: "Thank you! We'll let you know.",
		NotifySubject:   "%sitetitle% is open again",
		NotifyBody:      "Good news: %sitetitle% is back online.\n\nVisit us: %siteurl%",
		GDPRText:        "I agree that my e-mail address is stored to notify me when the site opens.",
		Social:          map[string]string{},
		SocialNewTab:    true,
		Title:           "%sitetitle% is under maintenance",
		MetaDescription: "We are doing some work on our site. Please come back later. We'll be up and running in no time.",
		NoIndex:         true,
		RetryAfter:      1,
		ShowLoggedIn:    true,
		BypassRoles:     append([]string(nil), maintRoles...),
		SecretHours:     24,
		PasswordMessage: "Please enter the password to access the site",
		PasswordButtonT: "Access the Site",
		PasswordWrong:   "Wrong password",
		LoginButton:     true,
		LoginTooltip:    "Access WordPress admin",
		AdminBarMenu:    true,
		BlockREST:       true,
		BlockFeeds:      true,
	}
	maintThemeByKey("default").apply(&st)
	return st
}

// maintOn reports whether the Maintenance-Go plugin is active (not whether the site is closed).
func (s *Server) maintOn() bool {
	return s.settings.Bool(maintSetting)
}

// maintLoad returns the saved settings over the defaults.
//
// The decoded value is memoized per settings revision: while no setting changes, the gate that runs
// for every request does not marshal and unmarshal about sixty fields again. Social and BypassRoles
// are copies, so callers may change them.
func (s *Server) maintLoad() maintSettings {
	st := maintMemo.Get(s.settings, s.maintDecode)
	st.Social = maps.Clone(st.Social)
	st.BypassRoles = slices.Clone(st.BypassRoles)
	return st
}

// maintMemo holds the last decoded settings with the settings revision they were decoded at.
var maintMemo settings.Memo[maintSettings]

// maintDecode reads the stored option over the defaults.
func (s *Server) maintDecode() maintSettings {
	v := s.settings.Get(maintOptions)
	st := maintDefaults()
	if v != nil {
		if raw, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(raw, &st)
		}
	}
	if st.Social == nil {
		st.Social = map[string]string{}
	}
	if st.Mode != "coming_soon" {
		st.Mode = "maintenance"
	}
	if maintThemeByKey(st.Theme) == nil {
		st.Theme = "default"
	}
	return st
}

// maintSave stores the settings and drops every cached page: what visitors see changes.
func (s *Server) maintSave(ctx context.Context, st maintSettings) error {
	if err := s.settings.Set(ctx, map[string]any{maintOptions: st}); err != nil {
		return err
	}
	s.maintRegisterStrings(ctx, st)
	s.purgeAll(ctx)
	return nil
}

// maintSetMode switches maintenance mode on or off.
func (s *Server) maintSetMode(ctx context.Context, on bool) error {
	st := s.maintLoad()
	was := st.Enabled
	st.Enabled = on
	if err := s.maintSave(ctx, st); err != nil {
		return err
	}
	if was && !on {
		s.maintNotifyOpen(ctx, st)
	}
	return nil
}

// maintClosed reports whether visitors currently get the maintenance page: the plugin is active,
// the mode is on and no auto-disable countdown has run out (one that has is switched off here, on
// the first request after the end, and the cache is purged).
func (s *Server) maintClosed(ctx context.Context) (maintSettings, bool) {
	if !s.maintOn() || !s.settings.Installed() {
		return maintSettings{}, false
	}
	st := s.maintLoad()
	if !st.Enabled {
		return st, false
	}
	if start, ok := s.maintStart(st); ok && time.Now().Before(start) { // scheduled: the site is open until then
		return st, false
	}
	if end, ok := s.maintEnd(st); ok && st.AutoDisable && !time.Now().Before(end) {
		st.Enabled = false
		if err := s.maintSave(context.WithoutCancel(ctx), st); err != nil {
			log.Printf("maintenance: auto-disable: %v", err)
		} else {
			s.maintNotifyOpen(context.WithoutCancel(ctx), st)
		}
		return st, false
	}
	return st, true
}

// maintStart is the scheduled start of maintenance mode, when one is set.
func (s *Server) maintStart(st maintSettings) (time.Time, bool) {
	return s.maintParseTime(st.StartDate)
}

// maintTick is run by the scheduler: it applies the schedule without waiting for a visitor, so the
// auto-disable at the end of the countdown happens (and subscribers are told) on a quiet site too.
func (s *Server) maintTick(ctx context.Context) {
	if s.maintOn() && s.settings.Installed() {
		s.maintClosed(ctx)
	}
}

// maintEnd is the countdown's end, when the countdown is on and has one.
func (s *Server) maintEnd(st maintSettings) (time.Time, bool) {
	if !st.Countdown || st.CountdownEnd == "" {
		return time.Time{}, false
	}
	return s.maintParseTime(st.CountdownEnd)
}

func (s *Server) maintParseTime(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, s.loc()); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// maintenanceServe is the gate, called by Handler for every request before the page cache. It
// reports whether it answered the request.
func (s *Server) maintenanceServe(w http.ResponseWriter, r *http.Request) bool {
	if !s.maintOn() || !s.settings.Installed() { // the plugin is off: cached option reads, nothing else
		return false
	}
	p := r.URL.Path
	if p == "/wp-admin/admin-post" || p == "/wp-admin/admin-post.php" { // admin_post_nopriv_ actions of the page
		switch r.URL.Query().Get("action") {
		case "mtnc_subscribe":
			s.maintSubscribe(w, r)
			return true
		case "mtnc_unlock":
			s.maintUnlock(w, r)
			return true
		case "mtnc_confirm", "mtnc_unsubscribe":
			s.maintSubLink(w, r, r.URL.Query().Get("action"))
			return true
		}
		return false
	}
	if maintAlwaysOpen(r.Method, p) { // before the settings are read: static files and wp-admin cost nothing here
		return false
	}
	_, preview := r.URL.Query()[maintPreviewParam]
	st, closed := s.maintClosed(r.Context())
	if !closed && !preview {
		return false
	}
	if preview { // the settings screen's Preview (the Themes tab passes &theme=)
		if th := r.URL.Query().Get("theme"); maintThemeByKey(th) != nil && th != st.Theme {
			maintThemeByKey(th).apply(&st)
		}
		s.maintRender(w, r, st, http.StatusOK)
		return true
	}
	u := s.maintUser(r)
	bypass := s.maintBypass(w, r, st, u)
	// /xmlrpc and /wp-cron are not pages. Leaving them to the site router serves ?p= and
	// ?page_id= on those paths, which would show the post while maintenance is on.
	if closed && maintRPCOrCron(p) && !bypass {
		s.maintHeaders(w, st, "text/plain; charset=utf-8")
		s.maintStatus(w, st, http.StatusServiceUnavailable)
		return true
	}
	if bypass || !s.maintURLGated(st, p) {
		return false
	}
	switch {
	case strings.HasPrefix(p, "/wp-json") || (r.URL.Query().Has("rest_route") && strings.Trim(p, "/") == ""):
		// Role bypass is maintBypass, already applied above: administrators and the roles in
		// BypassRoles (when ShowLoggedIn is on), plus the IP list, secret link and site password.
		// Logged-in users who can edit content keep the block editor's REST calls (wp-admin stays
		// open to them); a login cookie of a subscriber does not open the API while block_rest is on.
		// Clients that authenticate per request (Application Passwords, OAuth/MCP tokens, Commerce
		// API keys) still reach the REST layer, which validates them and enforces capabilities.
		if !st.BlockREST || maintRESTCredentials(r) || (u != nil && (u.Can("edit_posts", nil) || u.Can("edit_pages", nil))) {
			return false
		}
		// Coming Soon answers pages with 200 so search engines keep what they know; the REST API gets a plain
		// 403 there instead of a 503 that would tell clients and crawlers the site is down.
		code := http.StatusServiceUnavailable
		if st.Mode == "coming_soon" {
			code = http.StatusForbidden
		}
		s.maintHeaders(w, st, "application/json; charset=utf-8")
		s.maintStatus(w, st, code)
		_, _ = fmt.Fprintf(w, `{"code":"maintenance_mode","message":"The site is undergoing maintenance.","data":{"status":%d}}`, code)
		return true
	case maintIsFeed(r) || strings.HasPrefix(path.Base(p), "wp-sitemap"):
		if !st.BlockFeeds {
			return false
		}
	}
	status := http.StatusServiceUnavailable
	if st.Mode == "coming_soon" {
		status = http.StatusOK
	}
	s.maintRender(w, r, st, status)
	return true
}

// maintRESTCredentials reports whether a REST request carries credentials a REST layer validates: Basic
// (Application Passwords, Commerce ck_/cs_ keys) or Bearer (OAuth/MCP, Headless API tokens) authorization, or
// Commerce consumer_key query parameters on a management API route (wc/v3, the only routes that validate them;
// on the public Store API they are ignored). A bad credential is refused there with 401, so
// letting it through exposes nothing; an unknown scheme would be treated as anonymous, so it is not let through.
func maintRESTCredentials(r *http.Request) bool {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return true
	}
	// Only a decodable user:password pair: the REST layer reads a malformed Basic header as no
	// credentials at all and would answer the request anonymously.
	if _, _, ok := r.BasicAuth(); ok {
		return true
	}
	q := r.URL.Query()
	if q.Get("consumer_key") == "" {
		return false
	}
	route := strings.TrimPrefix(r.URL.Path, "/wp-json")
	if q.Has("rest_route") {
		route = q.Get("rest_route")
	}
	return gooQueryKeyRoute(route)
}

// maintAlwaysOpen are the paths the gate never closes: wp-admin, the login screen, uploads and
// the static assets the maintenance page and wp-login use, robots.txt and the favicon.
// Each entry is a whole path segment (or its .php form): "/wp-login-x?p=5" or "/xmlrpcs?page_id=2"
// would otherwise pass the gate and reach the site router, which serves ?p= and ?s= on any unknown
// path. The login screen and the CSP report endpoint have no sub-routes and no GET page of their
// own, so only their exact paths (and POST for the report) skip the gate.
func maintAlwaysOpen(method, p string) bool {
	if p == cspReportPath { // browsers' CSP reports, the maintenance page's own included
		return method == http.MethodPost
	}
	for _, pre := range []string{"/wp-content/", "/wp-includes/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	if p == "/wp-admin" || p == "/wp-admin.php" || strings.HasPrefix(p, "/wp-admin/") {
		return true
	}
	return p == "/wp-login" || p == "/wp-login.php" || p == "/robots.txt" || p == "/favicon.ico"
}

// maintRPCOrCron reports paths that must not reach the site resolver while maintenance is on.
// The resolver serves ?p= and ?page_id= before it looks at the path, so /xmlrpc.php?p=1 would
// otherwise return the post.
func maintRPCOrCron(p string) bool {
	for _, seg := range []string{"/wp-cron", "/xmlrpc"} {
		if p == seg || p == seg+".php" || strings.HasPrefix(p, seg+"/") || strings.HasPrefix(p, seg+".php/") {
			return true
		}
	}
	return false
}

func maintIsFeed(r *http.Request) bool {
	p := strings.Trim(r.URL.Path, "/")
	return r.URL.Query().Has("feed") || p == "feed" || strings.HasSuffix(p, "/feed") || strings.Contains(p, "/feed/") || strings.HasPrefix(p, "feed/")
}

// maintUser resolves the login cookie (the gate runs before the router's loadUser middleware).
func (s *Server) maintUser(r *http.Request) *store.User {
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

// maintBypass reports whether the visitor sees the normal site, checked in the original's order:
// search engines, logged-in users (by role), the IP whitelist, the secret access link and the site
// password. Administrators always pass.
func (s *Server) maintBypass(w http.ResponseWriter, r *http.Request, st maintSettings, u *store.User) bool {
	if st.ExcludeSE && maintIsCrawler(r.UserAgent()) {
		return true
	}
	if u != nil {
		if u.Can("manage_options", nil) {
			return true
		}
		if st.ShowLoggedIn {
			for _, role := range u.Roles {
				for _, allowed := range st.BypassRoles {
					if role == allowed {
						return true
					}
				}
			}
		}
	}
	if maintIPAllowed(st.IPWhitelist, s.requestIP(r)) {
		return true
	}
	if st.SecretKey != "" {
		want := s.maintToken("link", st.SecretKey)
		if ck, err := r.Cookie(maintAccessCookie); err == nil && hmac.Equal([]byte(ck.Value), []byte(want)) {
			return true
		}
		if _, ok := r.URL.Query()[st.SecretKey]; ok { // home/?<secret>
			hours := st.SecretHours
			if hours <= 0 {
				hours = 24
			}
			http.SetCookie(w, &http.Cookie{Name: maintAccessCookie, Value: want, Path: "/", MaxAge: hours * 3600,
				HttpOnly: true, Secure: s.httpsRequest(r), SameSite: http.SameSiteLaxMode})
			return true
		}
	}
	if st.SitePassword != "" {
		if ck, err := r.Cookie(maintUnlockCookie); err == nil && hmac.Equal([]byte(ck.Value), []byte(s.maintToken("password", st.SitePassword))) {
			return true
		}
	}
	return false
}

// maintToken is a bypass cookie's value: an HMAC bound to the site secret and to the link key or
// password, so changing either locks everyone holding an old cookie out.
func (s *Server) maintToken(kind, value string) string {
	m := hmac.New(sha256.New, []byte(s.siteSecret()+"|maintenance|"+kind))
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))
}

// maintCrawlers are the original's search engine user agents (check_referrer()).
var maintCrawlers = []string{"abachobot", "acoon", "acoirobot", "adidxbot", "altavista", "scooter", "aspseek", "atomz", "bingbot",
	"bingpreview", "croccrawler", "dumbot", "estyle", "fast-webcrawler", "geonabot", "gigabot", "googlebot", "idbot", "lycos",
	"msnbot", "msrbot", "rambler", "scrubby", "yahoo", "duckduckbot", "yandexbot", "baiduspider", "applebot"}

func maintIsCrawler(ua string) bool {
	ua = strings.ToLower(ua)
	for _, c := range maintCrawlers {
		if strings.Contains(ua, c) {
			return true
		}
	}
	return false
}

// maintIPAllowed matches an address against the whitelist: one IP or CIDR range per line, # comments.
func maintIPAllowed(list, addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	for _, l := range lines(list) {
		l, _, _ = strings.Cut(l, "#")
		for _, entry := range strings.FieldsFunc(l, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if strings.Contains(entry, "/") {
				if _, n, err := net.ParseCIDR(entry); err == nil && n.Contains(ip) {
					return true
				}
			} else if e := net.ParseIP(entry); e != nil && e.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// maintURLGated applies the URL Based Rules: with "whitelist" the listed URLs stay open, with
// "blacklist" only the listed URLs are closed. As in the original, the path is lowercased and
// compared with a trailing slash (unless it ends in a file extension); full URLs in the list are
// reduced to their path, and a trailing * (a WordPress Go addition) matches a prefix.
func (s *Server) maintURLGated(st maintSettings, p string) bool {
	if st.URLRules != "whitelist" && st.URLRules != "blacklist" {
		return true
	}
	listed := false
	req := maintSlashit(strings.ToLower(p))
	for _, l := range lines(st.URLList) {
		if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
			if u, err := url.Parse(l); err == nil {
				l = u.Path
			}
		}
		l = strings.ToLower(l)
		if pre, ok := strings.CutSuffix(l, "*"); ok {
			if strings.HasPrefix(req, "/"+strings.TrimLeft(pre, "/")) {
				listed = true
			}
		} else if req == maintSlashit(l) {
			listed = true
		}
	}
	if st.URLRules == "whitelist" {
		return !listed
	}
	return listed
}

// maintSlashit is the original's slashit(): a leading slash, and a trailing one unless the path
// ends in a file extension.
func maintSlashit(p string) string {
	if p == "" {
		return "/"
	}
	if !(len(p) >= 4 && p[len(p)-4] == '.') && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return "/" + strings.TrimLeft(p, "/")
}

// maintRetryAfter is the Retry-After value in seconds: the countdown's remaining time, or the
// configured number of hours (the original sends 3600).
func (s *Server) maintRetryAfter(st maintSettings) int {
	if end, ok := s.maintEnd(st); ok {
		if d := int(time.Until(end).Seconds()); d > 0 {
			return d
		}
	}
	h := st.RetryAfter
	if h <= 0 {
		h = 1
	}
	return h * 3600
}

// maintStatus writes the status line; a 503 carries Retry-After.
func (s *Server) maintStatus(w http.ResponseWriter, st maintSettings, status int) {
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", itoa64(int64(s.maintRetryAfter(st))))
	}
	w.WriteHeader(status)
}

// maintHeaders are the headers of every response the gate writes itself: never cached (by the
// page cache, a proxy or the browser: nocache_headers()), noindex when asked, and the front end's
// security headers, with Google Analytics allowed when it is configured.
func (s *Server) maintHeaders(w http.ResponseWriter, st maintSettings, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-cache, must-revalidate, max-age=0, no-store, private")
	h.Set("Expires", "Wed, 11 Jan 1984 05:00:00 GMT")
	h.Set("X-LiteSpeed-Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	if st.NoIndex {
		h.Set("X-Robots-Tag", "noindex, nofollow")
	}
	mode := s.cspMode()
	script, connect := cspScriptSrc(mode, false), "'self'"
	if st.Analytics != "" {
		script += " https://www.googletagmanager.com"
		connect += " https://*.google-analytics.com https://*.analytics.google.com https://www.googletagmanager.com"
	}
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; "+
		"script-src "+script+"; connect-src "+connect+"; object-src 'none'; base-uri 'self'; frame-ancestors 'self'; form-action 'self'")
	cspReportOnly(h, mode, false)
}

// maintPluginAction is the Plugins screen's activate/deactivate. Deactivating opens the site again;
// the settings stay for the next activation, as in WordPress.
func (s *Server) maintPluginAction(ctx context.Context, action string) error {
	if err := s.settings.Set(ctx, map[string]any{maintSetting: action == "activate"}); err != nil {
		return err
	}
	if action == "activate" {
		if err := maintEnsureTable(ctx, s.store); err != nil {
			log.Printf("maintenance: subscribers table: %v", err)
		}
	}
	s.purgeAll(ctx)
	return nil
}
