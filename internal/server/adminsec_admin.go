package server

import (
	"context"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// The AdminSecGo screens (manage_options): Dashboard with the security score, Settings in tabs,
// Lockouts and the Activity Log.

var asTabs = [][2]string{
	{"twofactor", "Two-Factor"}, {"login", "Login"}, {"lockouts", "Lockouts & IPs"}, {"passwords", "Passwords"},
	{"sessions", "Sessions"}, {"hardening", "Hardening"}, {"firewall", "Firewall"}, {"alerts", "Alerts & Log"},
}

type asCheck struct {
	Label, Detail, FixURL, FixLabel string
	OK                              bool
	Points                          int
}

type asAdminData struct {
	Page       string
	Tab        string
	Tabs       [][2]string
	Nonce      string
	S          asSettings
	Roles      []store.Role
	MailReady  bool
	MailWhy    string
	MailFixURL string
	HomeURL    string
	LoginURL   string
	Escape     bool
	Strengths  []string
	// Dashboard
	Score             int
	Checks            []asCheck
	Users, With2FA    int
	WithMFA, Failed24 int
	WithPasskey       int
	ActiveLockouts    int
	Recent            []asLogRow
	// Lockouts
	Lockouts []asLockoutRow
	History  []asLockoutRow
	MyIP     string
	// Log
	Log        []asLogRow
	Filter     asLogFilter
	Events     [][2]string
	Pagination pagination
	ExportURL  string
	// Integrity
	Integrity *asIntegrityReport
	// Firewall tab: the core default and whether PURSAE_FIREWALL=off overrides the settings
	FirewallDefault int
	FirewallEnvOff  bool
	// Hardening tab: the REST routes visitors always keep
	RESTVisitorRoutes []string
}

type asLockoutRow struct {
	ID                   int64
	Kind, Value, Reason  string
	Until, Created       time.Time
	Long, Active, Manual bool
}

// adminsecAdmin dispatches admin?page=adminsecgo*.
func (s *Server) adminsecAdmin(c *gin.Context, page string) bool {
	if !strings.HasPrefix(page, asSlug) {
		return false
	}
	if page == asSlug+"-account" {
		s.adminsecAccount(c)
		return true
	}
	u := currentUser(c)
	if !s.adminsecOn() || !u.Can("manage_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return true
	}
	ready, why := s.asMailReady()
	d := &asAdminData{Page: strings.TrimPrefix(strings.TrimPrefix(page, asSlug), "-"), Nonce: s.nonce(c, "adminsec-admin"), S: s.asLoad(),
		MailReady: ready, MailWhy: why, MailFixURL: s.asMailFixURL(), HomeURL: strings.TrimRight(s.links(c).HomeURL("/"), "/"),
		LoginURL: s.loginPath(), Escape: asEscapeHatch(), Strengths: asStrengthLabels, MyIP: clientIP(c),
		FirewallDefault: fwDefaultRateLimit, FirewallEnvOff: fwEnvOff(), RESTVisitorRoutes: asRESTVisitorRoutes}
	if d.Page == "" {
		d.Page = "dashboard"
	}
	var notices []notice
	switch d.Page {
	case "settings":
		d.Tab, d.Tabs, d.Roles = c.DefaultQuery("tab", "twofactor"), asTabs, s.asRoleNames(c)
		if c.Request.Method == http.MethodPost {
			if !s.checkNonce(c, "adminsec-admin") {
				s.denyNonce(c)
				return true
			}
			st, errs := s.asParseSettings(c, d.S, c.PostForm("tab"))
			if len(errs) > 0 {
				for _, e := range errs {
					notices = append(notices, errNotice(e)...)
				}
				d.S = st
				break
			}
			if err := s.asSave(c, st); err != nil {
				notices = append(notices, errNotice(err.Error())...)
				break
			}
			s.asAudit(c, asEvent{Event: "adminsec_settings", Severity: "notice", Object: c.PostForm("tab"), Message: "Pursae Security settings saved (" + c.PostForm("tab") + " tab)."})
			c.Redirect(http.StatusFound, "/wp-admin/admin?page="+asSlug+"-settings&tab="+url.QueryEscape(c.PostForm("tab"))+"&settings-updated=true")
			return true
		}
		if d.Tab == "login" && d.S.HideLogin && d.Escape {
			notices = append(notices, notice{Type: "warning", Message: "<strong>" + asEscapeEnv + "</strong> is set: the custom login URL is not applied and /wp-login works as usual."})
		}
	case "integrity":
		done, ns := s.asIntegrityPage(c, d)
		if done {
			return true
		}
		notices = append(notices, ns...)
	case "lockouts":
		if c.Request.Method == http.MethodPost {
			if !s.checkNonce(c, "adminsec-admin") {
				s.denyNonce(c)
				return true
			}
			s.asLockoutAction(c)
			c.Redirect(http.StatusFound, "/wp-admin/admin?page="+asSlug+"-lockouts&released=1")
			return true
		}
		if c.Query("released") == "1" {
			notices = append(notices, okNotice("Done.")...)
		}
		d.Lockouts, d.History = s.asLockoutRows(c, true), s.asLockoutRows(c, false)
	case "log":
		d.Filter = asLogFilter{Event: c.Query("event"), User: c.Query("user"), IP: c.Query("ip"), Severity: c.Query("severity"), From: c.Query("from"), To: c.Query("to")}
		if c.Query("export") == "csv" {
			if !s.checkNonce(c, "adminsec-export") {
				s.denyNonce(c)
				return true
			}
			s.asLogCSV(c, d.Filter)
			return true
		}
		page, _ := strconv.Atoi(c.Query("paged"))
		if page < 1 {
			page = 1
		}
		const per = 50
		rows, total, err := s.asLogQuery(c, d.Filter, per, (page-1)*per)
		if err != nil {
			notices = append(notices, errNotice(err.Error())...)
		}
		d.Log, d.Events = rows, asEventLabels
		q := c.Request.URL.Query()
		q.Del("paged")
		d.Pagination = newPagination(total, per, page, url.URL{Path: "/wp-admin/admin", RawQuery: q.Encode()})
		q.Set("export", "csv")
		q.Set("_wpnonce", s.nonce(c, "adminsec-export"))
		d.ExportURL = "/wp-admin/admin?" + q.Encode()
	default:
		d.Page = "dashboard"
		s.asDashboard(c, d)
	}
	title := map[string]string{"dashboard": "Dashboard", "settings": "Settings", "lockouts": "Lockouts", "log": "Activity Log", "integrity": "Integrity"}[d.Page]
	screen := "admin?page=" + asSlug
	if d.Page != "dashboard" {
		screen += "-" + d.Page
	}
	s.adminPage(c, screen, title+" ‹ Pursae Security", "toplevel_page_adminsecgo adminsecgo-admin", "admin/adminsecgo", d, notices...)
	return true
}

func (s *Server) asDashboard(c *gin.Context, d *asAdminData) {
	ctx := c.Request.Context()
	st := d.S
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&d.Users)
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE totp_enabled AND email_enabled), count(*) FILTER (WHERE email_enabled) FROM adminsec_factors`).Scan(&d.With2FA, &d.WithMFA)
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(DISTINCT user_id) FROM adminsec_passkeys WHERE NOT clone_warning`).Scan(&d.WithPasskey)
	pkTwoStep := st.Passkeys && st.PasskeyTwoStep // a passkey counts as two-step login
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE NOT success AND NOT pending AND attempted_at > now() - interval '24 hours'`).Scan(&d.Failed24)
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM adminsec_lockouts WHERE until > now() AND NOT released`).Scan(&d.ActiveLockouts)
	d.Recent, _, _ = s.asLogQuery(ctx, asLogFilter{}, 8, 0)

	var admins, adminsWith2FA int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(DISTINCT u.id), count(DISTINCT u.id) FILTER (WHERE (f.totp_enabled AND f.email_enabled)
			OR ($1 AND EXISTS (SELECT 1 FROM adminsec_passkeys p WHERE p.user_id = u.id AND NOT p.clone_warning)))
		FROM users u JOIN user_roles ur ON ur.user_id = u.id JOIN roles r ON r.id = ur.role_id
		LEFT JOIN adminsec_factors f ON f.user_id = u.id WHERE r.slug = 'administrator'`, pkTwoStep).Scan(&admins, &adminsWith2FA)
	me := s.asFactors(ctx, currentUser(c).ID)
	var adminLogin bool
	_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(login) IN ('admin', 'administrator'))`).Scan(&adminLogin)
	adminLen, adminStrength := st.policyFor([]string{"administrator"})
	set := func(tab string) string { return "/wp-admin/admin?page=" + asSlug + "-settings&tab=" + tab }
	https := strings.HasPrefix(s.settings.String("home"), "https://")
	d.Checks = []asCheck{
		{Label: "E-mail delivery is configured", Detail: "Login codes and alerts need working e-mail.", OK: d.MailReady, Points: 10, FixURL: d.MailFixURL, FixLabel: "Set up Pursae SMTP"},
		{Label: "Your account uses two-factor authentication", Detail: "An authenticator app with e-mailed codes as the fallback.", OK: (me.TOTPEnabled && me.EmailEnabled) || (pkTwoStep && me.Passkeys > 0), Points: 10, FixURL: "/wp-admin/admin?page=" + asSlug + "-account", FixLabel: "Set up 2FA"},
		{Label: "Administrators are required to use 2FA", Detail: "Two-Factor › Requirement per role.", OK: st.RequireRoles["administrator"] == "2fa" || (st.Passkeys && st.RequireRoles["administrator"] == "passkey"), Points: 10, FixURL: set("twofactor"), FixLabel: "Require 2FA"},
		{Label: "Every administrator has 2FA", Detail: strconv.Itoa(adminsWith2FA) + " of " + strconv.Itoa(admins) + " administrators.", OK: admins > 0 && adminsWith2FA == admins, Points: 8, FixURL: "/wp-admin/users?role=administrator", FixLabel: "See administrators"},
		{Label: "Login attempts are limited", Detail: "Lockouts by IP address and by username.", OK: st.IPMaxFailures > 0 && st.UserMaxFailures > 0, Points: 8, FixURL: set("lockouts"), FixLabel: "Configure lockouts"},
		{Label: "The login page has a custom URL", Detail: "Bots looking for /wp-login and /wp-admin get a 404.", OK: st.hideActive(), Points: 6, FixURL: set("login"), FixLabel: "Set a login URL"},
		{Label: "Login errors don't reveal usernames", Detail: "One generic message for a wrong username or password.", OK: st.GenericErrors, Points: 4, FixURL: set("login"), FixLabel: "Turn on"},
		{Label: "User enumeration is blocked", Detail: "/?author=N and the REST users list for visitors.", OK: st.BlockEnumeration, Points: 4, FixURL: set("login"), FixLabel: "Turn on"},
		{Label: "No account is called “admin”", Detail: "The first username attackers try.", OK: !adminLogin, Points: 4, FixURL: "/wp-admin/users?s=admin", FixLabel: "Review users"},
		{Label: "Strong passwords for administrators", Detail: "At least 12 characters and “Strong”.", OK: adminLen >= 12 && adminStrength >= 3 && !st.PwAllowWeak, Points: 6, FixURL: set("passwords"), FixLabel: "Set the policy"},
		{Label: "Common passwords are refused", Detail: "The 10,000 most used passwords.", OK: st.PwBlockCommon && !st.PwAllowWeak, Points: 4, FixURL: set("passwords"), FixLabel: "Turn on"},
		{Label: "The file editors are disabled", Detail: "Theme and Plugin File Editor (DISALLOW_FILE_EDIT).", OK: st.DisableFileEdit, Points: 6, FixURL: set("hardening"), FixLabel: "Disable"},
		{Label: "Security headers are sent", Detail: "X-Frame-Options, Referrer-Policy, Permissions-Policy, HSTS on HTTPS.", OK: st.Headers, Points: 4, FixURL: set("hardening"), FixLabel: "Turn on"},
		{Label: "The site uses HTTPS", Detail: "Settings › General › Site Address.", OK: https, Points: 6, FixURL: "/wp-admin/options-general", FixLabel: "Check the address"},
		{Label: "The version is hidden from visitors", Detail: "No generator tag or ?ver= with the release.", OK: st.HideVersion, Points: 2, FixURL: set("hardening"), FixLabel: "Hide it"},
		{Label: "Idle sessions expire", Detail: "Log out sessions nobody used for a while.", OK: st.IdleMinutes > 0, Points: 4, FixURL: set("sessions"), FixLabel: "Set a timeout"},
		{Label: "Administrators get security alerts", Detail: "New-IP admin logins and lockouts.", OK: st.AlertNewIP && st.AlertLockout && d.MailReady, Points: 4, FixURL: set("alerts"), FixLabel: "Turn on"},
	}
	total, got := 0, 0
	for _, ch := range d.Checks {
		total += ch.Points
		if ch.OK {
			got += ch.Points
		}
	}
	if total > 0 {
		d.Score = got * 100 / total
	}
}

func (s *Server) asLockoutRows(ctx context.Context, active bool) []asLockoutRow {
	q := `SELECT id, kind, value, reason, until, created_at, long_lock, released FROM adminsec_lockouts WHERE until > now() AND NOT released ORDER BY until DESC LIMIT 200`
	if !active {
		q = `SELECT id, kind, value, reason, until, created_at, long_lock, released FROM adminsec_lockouts WHERE until <= now() OR released ORDER BY created_at DESC LIMIT 100`
	}
	rows, err := s.store.Pool.Query(ctx, q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []asLockoutRow
	for rows.Next() {
		var r asLockoutRow
		if rows.Scan(&r.ID, &r.Kind, &r.Value, &r.Reason, &r.Until, &r.Created, &r.Long, &r.Manual) == nil {
			r.Active = active
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) asLockoutAction(c *gin.Context) {
	switch c.PostForm("adminsec_do") {
	case "release":
		id, _ := strconv.ParseInt(c.PostForm("lockout"), 10, 64)
		var kind, value string
		if s.store.Pool.QueryRow(c, `UPDATE adminsec_lockouts SET released = true WHERE id = $1 RETURNING kind, value`, id).Scan(&kind, &value) == nil {
			s.asClearFailures(c, kind, value)
			s.asAudit(c, asEvent{Event: "lockout_released", Severity: "notice", Object: kind + " " + value, Message: "Lockout released by an administrator."})
		}
	case "release_all":
		_, _ = s.store.Pool.Exec(c, `UPDATE adminsec_lockouts SET released = true WHERE until > now()`)
		_, _ = s.store.Pool.Exec(c, `DELETE FROM login_attempts WHERE NOT success`)
		s.asAudit(c, asEvent{Event: "lockout_released", Severity: "notice", Message: "Every lockout released by an administrator."})
	case "lock_ip":
		ip := strings.TrimSpace(c.PostForm("ip"))
		hours, _ := strconv.Atoi(c.PostForm("hours"))
		if hours <= 0 {
			hours = 24
		}
		if ip != "" && ip != clientIP(c) {
			_, _ = s.store.Pool.Exec(c, `INSERT INTO adminsec_lockouts (kind, value, reason, until) VALUES ('ip', $1, 'Locked by an administrator', now() + make_interval(hours => $2))`, ip, hours)
			s.asAudit(c, asEvent{Event: "lockout", Severity: "warning", Object: "IP address " + ip, Message: "Locked out by an administrator for " + strconv.Itoa(hours) + " hours."})
		}
	}
}

func (s *Server) asClearFailures(ctx context.Context, kind, value string) {
	if kind == "ip" {
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM login_attempts WHERE ip = $1 AND NOT success`, value)
	} else {
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM login_attempts WHERE lower(login) = $1 AND NOT success`, value)
	}
}

func asFormInt(c *gin.Context, key string, def, lo, hi int) int {
	v, err := strconv.Atoi(strings.TrimSpace(c.PostForm(key)))
	if err != nil {
		return def
	}
	return min(max(v, lo), hi)
}

func validIPList(list string) (bad []string) {
	for _, l := range lines(list) {
		l, _, _ = strings.Cut(l, "#")
		for _, e := range strings.FieldsFunc(l, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if !asValidIPEntry(e) {
				bad = append(bad, e)
			}
		}
	}
	return bad
}

// asParseSettings reads one tab of the settings form over st.
func (s *Server) asParseSettings(c *gin.Context, st asSettings, tab string) (asSettings, []string) {
	var errs []string
	on := func(k string) bool { return c.PostForm(k) == "1" }
	switch tab {
	case "twofactor":
		st.RequireRoles = map[string]string{}
		for _, r := range s.asRoleNames(c) {
			if v := c.PostForm("require_" + r.Slug); v == "mfa" || v == "2fa" || v == "passkey" {
				st.RequireRoles[r.Slug] = v
			}
		}
		st.GraceDays = asFormInt(c, "grace_days", st.GraceDays, 0, 90)
		st.RememberDays = asFormInt(c, "remember_days", st.RememberDays, 0, 365)
		st.Passkeys, st.PasskeyTwoStep = on("passkeys"), on("passkey_two_step")
		needMail, needPasskey := false, false
		for _, v := range st.RequireRoles {
			needMail = needMail || v == "mfa" || v == "2fa"
			needPasskey = needPasskey || v == "passkey"
		}
		if ok, why := s.asMailReady(); !ok && needMail {
			errs = append(errs, "Two-step login can only be required once e-mail can be delivered. "+why)
		}
		if needPasskey && !st.Passkeys {
			errs = append(errs, "A passkey can only be required while passkeys are allowed.")
		}
	case "login":
		slug := strings.ToLower(strings.Trim(strings.TrimSpace(c.PostForm("login_slug")), "/"))
		st.HideLogin = on("hide_login")
		st.LoginSlug = slug
		st.HideRedirect = strings.TrimSpace(c.PostForm("hide_redirect"))
		st.GenericErrors, st.BlockEnumeration, st.CaptchaForms = on("generic_errors"), on("block_enumeration"), on("captcha_forms")
		if st.HideLogin || slug != "" {
			switch {
			case !asSlugRe.MatchString(slug):
				errs = append(errs, "The login URL must be 3 to 64 lowercase letters, digits, dashes or underscores.")
			case asReservedSlug(slug):
				errs = append(errs, "“"+slug+"” is a reserved path. Choose another login URL.")
			default:
				if p, err := s.store.PostBySlug(c, "page", slug); err == nil && p != nil {
					errs = append(errs, "A page or post already uses “"+slug+"”. Choose another login URL.")
				}
			}
		}
		if r := st.HideRedirect; r != "" && !(strings.HasPrefix(r, "/") && !strings.HasPrefix(r, "//")) && !strings.HasPrefix(r, "https://") && !strings.HasPrefix(r, "http://") {
			errs = append(errs, "The redirect must be a path on this site (like /404) or a full URL.")
		}
	case "lockouts":
		st.IPMaxFailures = asFormInt(c, "ip_max_failures", st.IPMaxFailures, 0, 100)
		st.UserMaxFailures = asFormInt(c, "user_max_failures", st.UserMaxFailures, 0, 100)
		st.FailWindow = asFormInt(c, "fail_window", st.FailWindow, 1, 1440)
		st.LockoutMinutes = asFormInt(c, "lockout_minutes", st.LockoutMinutes, 1, 10080)
		st.LongLockoutAfter = asFormInt(c, "long_lockout_after", st.LongLockoutAfter, 0, 100)
		st.LongLockoutHours = asFormInt(c, "long_lockout_hours", st.LongLockoutHours, 1, 8760)
		st.AllowList, st.BlockList = strings.TrimSpace(c.PostForm("allow_list")), strings.TrimSpace(c.PostForm("block_list"))
		st.BlockListSite = on("block_list_site")
		for _, b := range append(validIPList(st.AllowList), validIPList(st.BlockList)...) {
			errs = append(errs, "“"+b+"” is not an IP address or CIDR range.")
		}
		if ip := clientIP(c); maintIPAllowed(st.BlockList, ip) && !maintIPAllowed(st.AllowList, ip) {
			errs = append(errs, "The block list contains your own IP address ("+ip+"); you would lock yourself out.")
		}
	case "passwords":
		st.PwMinLength, st.PwMinStrength = map[string]int{}, map[string]int{}
		for _, r := range s.asRoleNames(c) {
			st.PwMinLength[r.Slug] = asFormInt(c, "pw_len_"+r.Slug, 0, 0, 128)
			st.PwMinStrength[r.Slug] = asFormInt(c, "pw_strength_"+r.Slug, 0, 0, 4)
		}
		st.PwBlockCommon, st.PwCheckLogin, st.PwAllowWeak = on("pw_block_common"), on("pw_check_login"), on("pw_allow_weak")
		st.PwBreached = on("pw_breached")
	case "sessions":
		st.IdleMinutes = asFormInt(c, "idle_minutes", st.IdleMinutes, 0, 10080)
		if st.IdleMinutes > 0 && st.IdleMinutes < 5 {
			errs = append(errs, "The idle timeout must be at least 5 minutes (or 0 for none).")
		}
	case "hardening":
		st.Headers = on("headers")
		st.HSTSMaxAge = asFormInt(c, "hsts_max_age", st.HSTSMaxAge, 0, 63072000)
		st.HSTSSubdomains, st.HSTSPreload = on("hsts_subdomains"), on("hsts_preload")
		if fo := c.PostForm("frame_options"); fo == "SAMEORIGIN" || fo == "DENY" {
			st.FrameOptions = fo
		}
		st.ReferrerPolicy = strings.TrimSpace(c.PostForm("referrer_policy"))
		st.PermissionsPolicy = strings.TrimSpace(strings.ReplaceAll(c.PostForm("permissions_policy"), "\n", " "))
		st.DisableFileEdit, st.HideVersion = on("disable_file_edit"), on("hide_version")
		st.RESTVisitors, st.RESTAllow = on("rest_restrict"), strings.TrimSpace(c.PostForm("rest_allow"))
	case "firewall":
		st.Firewall, st.AlertIntegrity = on("firewall"), on("alert_integrity")
		st.RateLimit = asFormInt(c, "rate_limit", st.RateLimit, 0, 100000)
		if st.RateLimit > 0 && st.RateLimit < 60 {
			errs = append(errs, "The rate limit must be at least 60 requests a minute (or 0 for none).")
		}
	case "alerts":
		st.AlertNewIP, st.AlertLockout, st.LogFailedSkip = on("alert_new_ip"), on("alert_lockout"), on("log_failed_skip")
		st.AlertEmail = strings.TrimSpace(c.PostForm("alert_email"))
		st.LogRetention = asFormInt(c, "log_retention", st.LogRetention, 0, 3650)
		if st.AlertEmail != "" && !validEmail(st.AlertEmail) {
			errs = append(errs, "The alert address is not a valid e-mail address.")
		}
	default:
		errs = append(errs, "Unknown settings tab.")
	}
	return st, errs
}

// asValidIPEntry accepts an IP address or a CIDR range.
func asValidIPEntry(e string) bool {
	if strings.Contains(e, "/") {
		_, _, err := net.ParseCIDR(e)
		return err == nil
	}
	return net.ParseIP(e) != nil
}

// adminsecNotices are the admin-wide notices: the role requirement's grace period and a forced
// password change.
func (s *Server) adminsecNotices(c *gin.Context) []notice {
	if !s.adminsecOn() {
		return nil
	}
	u := currentUser(c)
	if u == nil {
		return nil
	}
	var out []notice
	if c.Request.URL.Path == "/wp-admin/profile" {
		out = append(out, s.adminsecProfileNotices(c, u)...)
	}
	if c.Query("page") == asSlug+"-account" {
		return out
	}
	if e := s.asEnrollState(c, u); !e.Satisfied && !e.Expired {
		what := "two-step login with e-mailed codes"
		if e.Required == "2fa" {
			what = "two-factor authentication with an authenticator app"
		}
		days := int(time.Until(e.Deadline).Hours()/24 + 0.99)
		out = append(out, notice{Type: "warning", Message: template.HTML("<strong>Pursae Security:</strong> your role requires " + what + ". Please <a href=\"/wp-admin/admin?page=" + asSlug +
			"-account\">set it up</a> within " + strconv.Itoa(days) + " day" + map[bool]string{true: "", false: "s"}[days == 1] + "; after that the dashboard is only available once it is set up.")})
	}
	return out
}
