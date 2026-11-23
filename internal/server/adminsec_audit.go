package server

import (
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/mail"
	"wpgo/internal/store"
)

// The activity log: who did what, from where. Rows go to adminsec_log while the plugin is on;
// the daily job (adminsecgo_daily) drops rows older than the retention period.

type asEvent struct {
	Event    string
	Severity string // info (default), notice, warning, critical
	User     *store.User
	UserID   int64
	Username string
	IP       string
	Object   string
	Message  string
}

// asEventLabels names the events in the log screen's filter.
var asEventLabels = [][2]string{
	{"login_success", "Login"},
	{"login_failed", "Failed login"},
	{"lockout", "Lockout"},
	{"lockout_released", "Lockout released"},
	{"blocked_ip", "Blocked IP"},
	{"logout", "Logout"},
	{"mfa_enabled", "E-mail codes enabled"},
	{"mfa_disabled", "E-mail codes disabled"},
	{"totp_enabled", "Authenticator app enabled"},
	{"totp_disabled", "Authenticator app disabled"},
	{"backup_codes_generated", "Backup codes generated"},
	{"backup_code_used", "Backup code used"},
	{"passkey_added", "Passkey added"},
	{"passkey_removed", "Passkey removed"},
	{"passkey_clone_warning", "Passkey refused: possible copy"},
	{"factors_reset", "Factors reset by an administrator"},
	{"device_remembered", "Device remembered"},
	{"devices_forgotten", "Remembered devices forgotten"},
	{"sessions_logged_out", "Sessions logged out"},
	{"sessions_logout_failed", "Session logout failed"},
	{"password_changed", "Password changed"},
	{"password_change_forced", "Password change required"},
	{"role_changed", "Role changed"},
	{"user_created", "User created"},
	{"user_deleted", "User deleted"},
	{"plugin_activated", "Plugin activated"},
	{"plugin_deactivated", "Plugin deactivated"},
	{"theme_switched", "Theme switched"},
	{"settings_updated", "Settings changed"},
	{"csp_sources_changed", "Allowed external sources changed"},
	{"adminsec_settings", "Pursae Security settings changed"},
	{"firewall_blocked", "Request blocked by the firewall"},
	{"rate_limited", "Rate limit reached"},
	{"integrity_scan", "Integrity scan"},
	{"integrity_findings", "Integrity scan findings"},
}

func asEventLabel(ev string) string {
	for _, l := range asEventLabels {
		if l[0] == ev {
			return l[1]
		}
	}
	return ev
}

// asAudit writes one log row. ctx may be the request's gin.Context: the acting user, IP and user
// agent then come from it unless the event names them.
func (s *Server) asAudit(ctx context.Context, e asEvent) {
	if !s.adminsecOn() {
		return
	}
	var ua string
	if gc, ok := ctx.(*gin.Context); ok && gc.Request != nil {
		if e.IP == "" {
			e.IP = clientIP(gc)
		}
		ua = gc.Request.UserAgent()
		if e.User == nil && e.UserID == 0 && e.Username == "" {
			e.User = currentUser(gc)
		}
	}
	if e.User != nil {
		e.UserID, e.Username = e.User.ID, e.User.Login
	}
	if e.Severity == "" {
		e.Severity = "info"
	}
	var uid any
	if e.UserID != 0 {
		uid = e.UserID
	}
	if len(ua) > 400 {
		ua = ua[:400]
	}
	if _, err := s.store.Pool.Exec(context.WithoutCancel(ctx), `INSERT INTO adminsec_log (event, severity, user_id, username, ip, user_agent, object, message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, e.Event, e.Severity, uid, e.Username, e.IP, ua, e.Object, e.Message); err != nil {
		slog.Error("adminsecgo: log", "err", err)
	}
}

type asLogRow struct {
	ID                                       int64
	When                                     time.Time
	Event, Label, Severity                   string
	UserID                                   int64
	Username, IP, UserAgent, Object, Message string
}

type asLogFilter struct {
	Event, User, IP, Severity, From, To string
	Page                                int
}

func (f asLogFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.Event != "" {
		add("event = ?", f.Event)
	}
	if f.Severity != "" {
		add("severity = ?", f.Severity)
	}
	if f.User != "" {
		add("lower(username) LIKE ?", "%"+strings.ToLower(f.User)+"%")
	}
	if f.IP != "" {
		add("ip LIKE ?", f.IP+"%")
	}
	if t, err := time.Parse("2006-01-02", f.From); err == nil {
		add("created_at >= ?", t)
	}
	if t, err := time.Parse("2006-01-02", f.To); err == nil {
		add("created_at < ?", t.Add(24*time.Hour))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (s *Server) asLogQuery(ctx context.Context, f asLogFilter, limit, offset int) ([]asLogRow, int, error) {
	where, args := f.where()
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM adminsec_log`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := s.store.Pool.Query(ctx, `SELECT id, created_at, event, severity, coalesce(user_id, 0), username, ip, user_agent, object, message
		FROM adminsec_log`+where+fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []asLogRow
	for rows.Next() {
		var r asLogRow
		if err := rows.Scan(&r.ID, &r.When, &r.Event, &r.Severity, &r.UserID, &r.Username, &r.IP, &r.UserAgent, &r.Object, &r.Message); err != nil {
			return nil, 0, err
		}
		r.Label = asEventLabel(r.Event)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// asLogCSV streams the filtered log as a CSV download.
func (s *Server) asLogCSV(c *gin.Context, f asLogFilter) {
	rows, _, err := s.asLogQuery(c, f, 50000, 0)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="adminsecgo-activity-`+time.Now().Format("2006-01-02")+`.csv"`)
	c.Header("Cache-Control", "no-store")
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"Date (UTC)", "Event", "Severity", "User", "IP", "Object", "Message", "User agent"})
	for _, r := range rows {
		_ = w.Write(asLogCSVRecord(r))
	}
	w.Flush()
}

// asLogCSVRecord renders one log row as CSV cells. Every cell a visitor can
// influence (the typed login, IP, object, message, user agent) is formula-neutralized.
func asLogCSVRecord(r asLogRow) []string {
	return []string{r.When.UTC().Format(time.RFC3339), csvSafe(r.Event), csvSafe(r.Severity), csvSafe(r.Username), csvSafe(r.IP), csvSafe(r.Object), csvSafe(r.Message), csvSafe(r.UserAgent)}
}

// asAuditPlugin logs a plugin switch from the Plugins screen (AdminSecGo logs its own).
func (s *Server) asAuditPlugin(c *gin.Context, action, slug string) {
	if slug == asSlug || c.Writer.Status() >= 400 || strings.Contains(c.Writer.Header().Get("Location"), "error=") {
		return
	}
	name := slug
	for _, r := range s.installedPlugins(c) {
		if r.Slug == slug {
			name = r.Name
		}
	}
	s.asAudit(c, asEvent{Event: "plugin_" + action + "d", Severity: "notice", Object: name, Message: "Plugin " + name + " " + action + "d."})
}

// adminsecRoleChanged logs a role change (Users list, user-edit, REST).
func (s *Server) adminsecRoleChanged(ctx context.Context, id int64, login, before, after string) {
	if before == after || !s.adminsecOn() {
		return
	}
	if before == "" {
		before = "none"
	}
	s.asAudit(ctx, asEvent{Event: "role_changed", Severity: "notice", Object: login, Message: "Role of " + login + " changed from " + before + " to " + after + "."})
}

// --- alerts ------------------------------------------------------------------------------

func (s *Server) asAlertTo(st asSettings) string {
	if st.AlertEmail != "" {
		return st.AlertEmail
	}
	return s.settings.String("admin_email")
}

// asNoteLoginIP remembers the IP of a login and mails the alert for an administrator's first
// login from a new address (not for the very first login: there is nothing to compare with).
func (s *Server) asNoteLoginIP(c *gin.Context, u *store.User) {
	st := s.asLoad()
	ip := clientIP(c)
	var known, total int
	_ = s.store.Pool.QueryRow(c, `SELECT count(*) FILTER (WHERE ip = $2), count(*) FROM adminsec_known_ips WHERE user_id = $1`, u.ID, ip).Scan(&known, &total)
	_, _ = s.store.Pool.Exec(c, `INSERT INTO adminsec_known_ips (user_id, ip) VALUES ($1, $2)
		ON CONFLICT (user_id, ip) DO UPDATE SET last_seen = now()`, u.ID, ip)
	if known > 0 || total == 0 || !st.AlertNewIP || !u.Can("manage_options", nil) {
		return
	}
	site := s.settings.String("blogname")
	body := fmt.Sprintf("The administrator account %q just logged in to %s from an IP address it has not used before.\n\nIP address: %s\nBrowser: %s\nTime: %s\n\nIf this was not expected, change the account's password, log out its other sessions (Users › Profile) and review Pursae Security › Activity Log:\n%s\n",
		u.Login, site, ip, c.Request.UserAgent(), time.Now().UTC().Format(time.RFC1123), s.links(c).HomeURL("/wp-admin/admin?page=adminsecgo-log"))
	s.sendMail(c, mail.Message{To: []string{s.asAlertTo(st)}, Subject: "[" + site + "] Administrator login from a new IP address", Body: body})
}

func (s *Server) asLockoutAlert(c *gin.Context, kind, value string, until time.Time, failures int) {
	st := s.asLoad()
	if !st.AlertLockout {
		return
	}
	if kind == "user" { // a made-up username is not worth a mail: an attacker could fill the inbox with them
		if _, err := s.store.UserByLoginOrEmail(c, value); err != nil {
			return
		}
	}
	if !asLockoutAlertSlot(time.Now()) { // at most asAlertsPerHour mails an hour; the lockouts stay in the log
		return
	}
	site := s.settings.String("blogname")
	what := "The IP address " + value
	if kind == "user" {
		what = "The username " + strconv.Quote(value)
	}
	body := fmt.Sprintf("%s was locked out of %s after %d failed login attempts.\n\nLocked until: %s\nLast attempt from: %s\n\nLockouts: %s\n",
		what, site, failures, until.UTC().Format(time.RFC1123), clientIP(c), s.links(c).HomeURL("/wp-admin/admin?page=adminsecgo-lockouts"))
	s.sendMail(c, mail.Message{To: []string{s.asAlertTo(st)}, Subject: "[" + site + "] Login lockout", Body: body})
}

// --- daily housekeeping ----------------------------------------------------------------------

func (s *Server) adminsecDaily(ctx context.Context) error {
	if _, err := s.store.Pool.Exec(ctx, `DELETE FROM adminsec_challenges WHERE expires_at < now() - interval '1 day'`); err != nil {
		return err
	}
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM adminsec_devices WHERE expires_at < now()`)
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM adminsec_passkey_ceremonies WHERE expires_at < now()`)
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM adminsec_lockouts WHERE until < now() - interval '30 days'`)
	if !s.adminsecOn() {
		return nil
	}
	s.asIntegrityDaily(ctx) // the file integrity scan (adminsec_integrity.go)
	if days := s.asLoad().LogRetention; days > 0 {
		_, err := s.store.Pool.Exec(ctx, `DELETE FROM adminsec_log WHERE created_at < now() - make_interval(days => $1)`, days)
		return err
	}
	return nil
}

// asAlertsPerHour caps the lockout alert e-mails: a distributed attack opens many lockouts at once.
const asAlertsPerHour = 5

var (
	asAlertMu    sync.Mutex
	asAlertTimes []time.Time
)

// asLockoutAlertSlot takes one of the hourly alert slots; false when they are used up.
func asLockoutAlertSlot(now time.Time) bool {
	asAlertMu.Lock()
	defer asAlertMu.Unlock()
	keep := asAlertTimes[:0]
	for _, t := range asAlertTimes {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	asAlertTimes = keep
	if len(asAlertTimes) >= asAlertsPerHour {
		return false
	}
	asAlertTimes = append(asAlertTimes, now)
	return true
}
