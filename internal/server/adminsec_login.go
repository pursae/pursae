package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/auth"
	"wpgo/internal/mail"
	"wpgo/internal/store"
)

// The login side of AdminSecGo: blocked and locked-out IPs and usernames before the password is
// checked, the second-factor challenge after it, remembered devices, and what happens at the end
// of a login (forced password change, enforcement, alerts, the activity log).

const (
	asCodeTTL         = 10 * time.Minute
	asChallengeTTL    = 15 * time.Minute
	asResendCooldown  = 60 * time.Second
	asMaxCodeSends    = 5
	asMaxCodeAttempts = 5
)

// --- factors -----------------------------------------------------------------------------

type asBackup struct {
	H string `json:"h"`
	U int64  `json:"u"` // used at (unix), 0 while unused
}

type asFactors struct {
	UserID          int64
	EmailEnabled    bool
	Email           string
	EmailVerifiedAt *time.Time
	TOTPEnabled     bool
	TOTPSecret      string // sealed
	TOTPPending     string // sealed
	TOTPLastStep    int64
	TOTPEnabledAt   *time.Time
	Backup          []asBackup
	BackupAt        *time.Time
	Passkeys        int // usable passkeys, counted only while passkeys are switched on
}

func (f *asFactors) any() bool {
	return f != nil && (f.EmailEnabled || f.TOTPEnabled || f.Passkeys > 0)
}

func (f *asFactors) backupLeft() int {
	n := 0
	for _, b := range f.Backup {
		if b.U == 0 {
			n++
		}
	}
	return n
}

func (s *Server) asFactors(ctx context.Context, userID int64) *asFactors {
	f := &asFactors{UserID: userID}
	var raw []byte
	err := s.store.Pool.QueryRow(ctx, `SELECT email_enabled, email_address, email_verified_at, totp_enabled, totp_secret, totp_pending, totp_last_step,
		totp_enabled_at, backup_codes, backup_generated_at FROM adminsec_factors WHERE user_id = $1`, userID).
		Scan(&f.EmailEnabled, &f.Email, &f.EmailVerifiedAt, &f.TOTPEnabled, &f.TOTPSecret, &f.TOTPPending, &f.TOTPLastStep, &f.TOTPEnabledAt, &raw, &f.BackupAt)
	if err == nil {
		_ = json.Unmarshal(raw, &f.Backup)
	}
	if s.asPasskeysOn() {
		f.Passkeys = s.asPasskeyCount(ctx, userID)
	}
	return f
}

func (s *Server) asSaveFactors(ctx context.Context, f *asFactors) error {
	raw, _ := json.Marshal(f.Backup)
	if f.Backup == nil {
		raw = []byte("[]")
	}
	_, err := s.store.Pool.Exec(ctx, `INSERT INTO adminsec_factors (user_id, email_enabled, email_address, email_verified_at, totp_enabled, totp_secret,
		totp_pending, totp_last_step, totp_enabled_at, backup_codes, backup_generated_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now())
		ON CONFLICT (user_id) DO UPDATE SET email_enabled = EXCLUDED.email_enabled, email_address = EXCLUDED.email_address,
		email_verified_at = EXCLUDED.email_verified_at, totp_enabled = EXCLUDED.totp_enabled, totp_secret = EXCLUDED.totp_secret,
		totp_pending = EXCLUDED.totp_pending, totp_last_step = EXCLUDED.totp_last_step, totp_enabled_at = EXCLUDED.totp_enabled_at,
		backup_codes = EXCLUDED.backup_codes, backup_generated_at = EXCLUDED.backup_generated_at, updated_at = now()`,
		f.UserID, f.EmailEnabled, f.Email, f.EmailVerifiedAt, f.TOTPEnabled, f.TOTPSecret, f.TOTPPending, f.TOTPLastStep, f.TOTPEnabledAt, raw, f.BackupAt)
	return err
}

// asNewBackupCodes replaces the user's backup codes with ten new ones and returns them (shown once).
func (s *Server) asNewBackupCodes(ctx context.Context, f *asFactors) ([]string, error) {
	var codes []string
	f.Backup = nil
	for i := 0; i < 10; i++ {
		code := newBackupCode()
		codes = append(codes, code)
		f.Backup = append(f.Backup, asBackup{H: s.asHash("backup", strings.ReplaceAll(code, "-", ""))})
	}
	now := time.Now()
	f.BackupAt = &now
	return codes, s.asSaveFactors(ctx, f)
}

// asUseBackupCode spends a backup code; the update is conditional so a code works only once.
func (s *Server) asUseBackupCode(ctx context.Context, f *asFactors, code string) bool {
	code = strings.ToLower(normalizeCode(code))
	if len(code) != 10 {
		return false
	}
	h := s.asHash("backup", code)
	for i, b := range f.Backup {
		if b.U == 0 && hmac.Equal([]byte(b.H), []byte(h)) {
			old, _ := json.Marshal(f.Backup)
			f.Backup[i].U = time.Now().Unix()
			raw, _ := json.Marshal(f.Backup)
			tag, err := s.store.Pool.Exec(ctx, `UPDATE adminsec_factors SET backup_codes = $2 WHERE user_id = $1 AND backup_codes = $3::jsonb`, f.UserID, raw, old)
			return err == nil && tag.RowsAffected() == 1
		}
	}
	return false
}

// asCheckTOTP verifies an authenticator code and records its step (replay protection), atomically.
func (s *Server) asCheckTOTP(ctx context.Context, f *asFactors, code string) bool {
	secret := s.asReveal(f.TOTPSecret)
	if !f.TOTPEnabled || secret == "" {
		return false
	}
	step, ok := totpMatch(secret, code, time.Now(), f.TOTPLastStep)
	if !ok {
		return false
	}
	tag, err := s.store.Pool.Exec(ctx, `UPDATE adminsec_factors SET totp_last_step = $2 WHERE user_id = $1 AND totp_last_step < $2`, f.UserID, step)
	if err != nil || tag.RowsAffected() != 1 {
		return false
	}
	f.TOTPLastStep = step
	return true
}

// --- requirement per role ----------------------------------------------------------------

// asRequired is the strongest second factor the user's roles require: "", "mfa", "2fa" or "passkey".
// A passkey requirement ranks with 2FA; when roles ask for both, both apply ("2fa+passkey").
func (st asSettings) required(u *store.User) string {
	req := ""
	twofa, passkey := false, false
	for _, r := range u.Roles {
		switch st.RequireRoles[r] {
		case "2fa":
			twofa = true
		case "passkey":
			passkey = st.Passkeys
		case "mfa":
			req = "mfa"
		}
	}
	switch {
	case twofa && passkey && !st.PasskeyTwoStep:
		return "2fa+passkey"
	case passkey:
		return "passkey"
	case twofa:
		return "2fa"
	}
	return req
}

// satisfies reports whether the factors meet a requirement. A passkey meets the MFA and 2FA
// requirements too while a passkey sign-in counts as two-step login.
func (st asSettings) satisfies(req string, f *asFactors) bool {
	pk := st.Passkeys && f.Passkeys > 0
	byPasskey := pk && st.PasskeyTwoStep
	switch req {
	case "2fa+passkey":
		return pk && f.TOTPEnabled && f.EmailEnabled
	case "passkey":
		return pk
	case "2fa":
		return (f.TOTPEnabled && f.EmailEnabled) || byPasskey
	case "mfa":
		return f.EmailEnabled || byPasskey
	}
	return true
}

// asNeedsMail reports whether a requirement can only be met with e-mail delivery.
func asNeedsMail(req string) bool { return req == "mfa" || req == "2fa" || req == "2fa+passkey" }

type asEnrollment struct {
	Required  string // "", "mfa", "2fa"
	Satisfied bool
	Deadline  time.Time
	Expired   bool // grace period over: the user may only set up the factor
}

// asEnrollState reports where the user stands with their role's requirement, starting the grace
// period the first time it is seen unmet. Requirements that need e-mail are not enforced while
// e-mail cannot be delivered (nobody could meet them).
func (s *Server) asEnrollState(c *gin.Context, u *store.User) asEnrollment {
	st := s.asLoad()
	e := asEnrollment{Required: st.required(u), Satisfied: true}
	if e.Required == "" {
		return e
	}
	f := s.asFactors(c, u.ID)
	if e.Satisfied = st.satisfies(e.Required, f); e.Satisfied {
		return e
	}
	if ok, _ := s.asMailReady(); !ok && asNeedsMail(e.Required) {
		e.Required, e.Satisfied = "", true
		return e
	}
	start, _ := u.Meta["adminsec_grace_start"].(float64)
	if start == 0 {
		start = float64(time.Now().Unix())
		_ = s.setUserMeta(c, u, "adminsec_grace_start", start)
	}
	e.Deadline = time.Unix(int64(start), 0).Add(time.Duration(st.GraceDays) * 24 * time.Hour)
	e.Expired = time.Now().After(e.Deadline)
	return e
}

const (
	asProfileGateURL = "/wp-admin/profile?adminsec=password#password"
	asAccountGateURL = "/wp-admin/admin?page=" + asSlug + "-account&required=1"
)

// adminsecBlocked reports whether the user must fix their account before using the site as
// themselves: a forced password change (asProfileGateURL) or a role requirement past its grace
// period (asAccountGateURL). It returns the screen that fixes it, or "" when nothing blocks the user.
// The HTML admin (adminsecGate), the login (asFinishLogin) and cookie-authenticated REST
// (restAuthUser) all decide with it.
func (s *Server) adminsecBlocked(c *gin.Context, u *store.User) string {
	if u == nil || !s.adminsecOn() {
		return ""
	}
	if s.asForcePassword(c, u) {
		return asProfileGateURL
	}
	if e := s.asEnrollState(c, u); !e.Satisfied && e.Expired {
		return asAccountGateURL
	}
	return ""
}

// adminsecGate runs in requireLogin: a forced password change, or a requirement past its grace
// period, keeps the user on the screens that fix it.
func (s *Server) adminsecGate(c *gin.Context, u *store.User) bool {
	if !s.adminsecOn() {
		return false
	}
	p := strings.TrimSuffix(c.Request.URL.Path, ".php")
	if p == "/wp-admin/admin-ajax" && isBackgroundPoll(c) {
		return false // the heartbeat withholds its REST nonce from blocked users (heartbeat)
	}
	switch target := s.adminsecBlocked(c, u); {
	case target == "":
		return false
	case target == asProfileGateURL && p == "/wp-admin/profile",
		target == asAccountGateURL && p == "/wp-admin/admin" && c.Query("page") == asSlug+"-account":
		return false
	default:
		c.Redirect(http.StatusFound, target)
		return true
	}
}

// --- lockouts ----------------------------------------------------------------------------
//
// What Pursae Security counts and locks (see auth/throttle.go for the core's own limits):
//
//   - ip_max_failures per client network (auth.ClientNet: an IPv4 address, an IPv6 /64): an "ip" lockout
//     of the network. A lockout an administrator made for a single address is honoured too.
//   - user_max_failures per account AND client network: a "user" lockout whose value is
//     "<login> from <network>". Failing at an account from one network never locks the account for the
//     other networks, so nobody can lock the owner (an administrator, say) out by failing at their
//     username. Lockouts made per account alone by earlier versions are no longer created or enforced.
//   - user_max_failures failures at an account from every network within fail_window put it "under
//     attack" (asAccountPressure): a network that has not signed in to it within a day, and is no
//     remembered device of the account, then waits up to auth.MaxAccountDelay before its password is
//     checked, gets at most auth.MaxFailedPerLoginNetUnderAttack failures there, and on /wp-login is asked
//     for the CAPTCHA when keys are configured (login_captcha.go). A correct password from a network
//     that has not failed still signs in.

// asLockedUntil returns the end of an active lockout of kind/value.
func (s *Server) asLockedUntil(ctx context.Context, kind, value string) (time.Time, bool) {
	var until time.Time
	err := s.store.Pool.QueryRow(ctx, `SELECT until FROM adminsec_lockouts WHERE kind = $1 AND value = $2 AND until > now() AND NOT released
		ORDER BY until DESC LIMIT 1`, kind, value).Scan(&until)
	return until, err == nil
}

// asNetLockedUntil returns the end of an active lockout of the client's network, or of its own address
// (an administrator's "Lock IP").
func (s *Server) asNetLockedUntil(ctx context.Context, ip string) (time.Time, bool) {
	var until time.Time
	err := s.store.Pool.QueryRow(ctx, `SELECT until FROM adminsec_lockouts WHERE kind = 'ip' AND value IN ($1, $2) AND until > now() AND NOT released
		ORDER BY until DESC LIMIT 1`, auth.ClientNet(ip), strings.TrimSpace(ip)).Scan(&until)
	return until, err == nil
}

func asWait(until time.Time) string {
	d := time.Until(until)
	switch {
	case d > 90*time.Minute:
		return strconv.Itoa(int(d.Hours()+0.5)) + " hours"
	case d > 90*time.Second:
		return strconv.Itoa(int(d.Minutes()+0.5)) + " minutes"
	}
	return "a minute"
}

func asLockedMsg(until time.Time) template.HTML {
	return template.HTML("<strong>Error:</strong> Too many failed login attempts. Please try again in " + asWait(until) + ".")
}

// asNormLogin is the username a login attempt is counted against: the account's login when the
// name or e-mail belongs to one, else what was typed, lowercased.
func (s *Server) asNormLogin(ctx context.Context, login string, u *store.User) string {
	if u != nil {
		return strings.ToLower(u.Login)
	}
	return strings.ToLower(strings.TrimSpace(login))
}

// asBucket is what a lockout counts failures in: adminsec_lockouts kind/value, and the login_attempts
// rows that belong to it.
type asBucket struct {
	kind, value string
	login, net  string // login_attempts filter: lower(login) (when set) and ip, the client network
}

func asNetBucket(net string) asBucket { return asBucket{kind: "ip", value: net, net: net} }

// asUserBucket is one account seen from one client network (no bucket without a name).
func asUserBucket(name, net string) asBucket {
	if name == "" || net == "" {
		return asBucket{kind: "user"}
	}
	return asBucket{kind: "user", value: name + " from " + net, login: name, net: net}
}

func (b asBucket) where() (string, []any) {
	if b.login != "" {
		return "ip = $1 AND lower(login) = $2", []any{b.net, b.login}
	}
	return "ip = $1", []any{b.net}
}

// adminsecPreLogin checks a login attempt before the password: the block list, then the lockouts of the
// client network and of the account from this network (the allow list skips lockouts). It returns the
// error to show, or "".
func (s *Server) adminsecPreLogin(c *gin.Context, login string) template.HTML {
	st := s.asLoad()
	ip := clientIP(c)
	if st.BlockList != "" && maintIPAllowed(st.BlockList, ip) && !maintIPAllowed(st.AllowList, ip) {
		s.asAudit(c, asEvent{Event: "blocked_ip", Severity: "warning", Username: login, Object: ip, Message: "Login attempt from a blocked IP address."})
		return "<strong>Error:</strong> Access from your IP address has been blocked."
	}
	if maintIPAllowed(st.AllowList, ip) {
		return ""
	}
	if until, ok := s.asNetLockedUntil(c, ip); ok {
		return asLockedMsg(until)
	}
	if login != "" {
		u, _ := s.store.UserByLoginOrEmail(c, login)
		if b := asUserBucket(s.asNormLogin(c, login, u), auth.ClientNet(ip)); b.value != "" {
			if until, ok := s.asLockedUntil(c, b.kind, b.value); ok {
				return asLockedMsg(until)
			}
		}
	}
	return ""
}

// adminsecBeginAttempt counts a password attempt before the password is checked, as the core throttle does
// (auth.BeginLogin): parallel guesses cannot all pass adminsecPreLogin before any is recorded. The attempt is
// stored as pending first; when the recent failures plus the attempts in flight (this one included) go past
// the network or the account-from-this-network limit, this request alone is refused without checking its
// password, so at most max passwords are checked per window. In-flight attempts never start a lockout: only
// adminsecLoginFailed does, once a password was really checked and was wrong (parallel requests with a correct
// password must not lock anyone out). At an account under attack (asAccountPressure) a network it does not
// know gets a smaller budget and waits before its password is checked. It returns the stored attempt (pass it
// to adminsecLoginFailed, or drop it on a correct password) or the message to show.
func (s *Server) adminsecBeginAttempt(c *gin.Context, login string) (int64, template.HTML) {
	st := s.asLoad()
	ip := clientIP(c)
	net := auth.ClientNet(ip)
	allowed := maintIPAllowed(st.AllowList, ip)
	u, _ := s.store.UserByLoginOrEmail(c, login)
	name := s.asNormLogin(c, login, u)
	user := asUserBucket(name, net)
	if !allowed {
		if until, ok := s.asNetLockedUntil(c, ip); ok {
			return 0, asLockedMsg(until)
		}
		if user.value != "" {
			if until, ok := s.asLockedUntil(c, user.kind, user.value); ok {
				return 0, asLockedMsg(until)
			}
		}
	}
	id, err := s.auth.ProvisionalAttempt(c, ip, name)
	if err != nil {
		return 0, "" // adminsecLoginFailed records the failure the old way
	}
	if allowed {
		return id, ""
	}
	if s.asAttemptsOver(c, st, asNetBucket(net), st.IPMaxFailures) || s.asAttemptsOver(c, st, user, st.UserMaxFailures) {
		s.auth.DropAttempt(c, id) // refused unchecked: not a guess, and no lockout
		return 0, "<strong>Error:</strong> Too many login attempts. Please try again in a minute."
	}
	if p, hot := s.asAccountPressure(c, st, name, u, id); hot {
		if p.NetFailures >= min(auth.MaxFailedPerLoginNetUnderAttack, st.UserMaxFailures) {
			s.auth.DropAttempt(c, id)
			return 0, template.HTML(fmt.Sprintf("<strong>Error:</strong> Too many failed login attempts. Please try again in %d minutes.", max1(st.FailWindow)))
		}
		loginSlowdown(c.Request.Context(), auth.AccountDelay(p.Failures, st.UserMaxFailures))
	}
	return id, ""
}

// asAccountPressure reports whether the account name is under attack as seen from this request: at least
// user_max_failures failures at it from every network within fail_window (leaving out the attempt exclude),
// from a network that has not signed in to it recently and without a remembered-device cookie of the account.
func (s *Server) asAccountPressure(c *gin.Context, st asSettings, name string, u *store.User, exclude int64) (auth.Pressure, bool) {
	if st.UserMaxFailures <= 0 || name == "" {
		return auth.Pressure{}, false
	}
	p, err := s.auth.LoginPressure(c, clientIP(c), name, time.Duration(max1(st.FailWindow))*time.Minute, exclude)
	if err != nil || p.KnownNet || p.Failures < st.UserMaxFailures {
		return p, false
	}
	if u != nil && s.asRememberedDevice(c, u) {
		return p, false
	}
	return p, true
}

// asFailWindowStart is where the failures counted for a bucket begin: the failure window, or the bucket's
// last lockout when it is more recent (failures before it were already counted by it).
func (s *Server) asFailWindowStart(c *gin.Context, st asSettings, b asBucket) time.Time {
	since := time.Now().Add(-time.Duration(max1(st.FailWindow)) * time.Minute)
	var last time.Time
	if err := s.store.Pool.QueryRow(c, `SELECT max(created_at) FROM adminsec_lockouts WHERE kind = $1 AND value = $2`, b.kind, b.value).Scan(&last); err == nil && last.After(since) {
		since = last
	}
	return since
}

// asAttemptsOver reports whether the recent failures in b plus the attempts still in flight go past max
// (the caller's own pending attempt is one of them).
func (s *Server) asAttemptsOver(c *gin.Context, st asSettings, b asBucket, max int) bool {
	if max <= 0 || b.value == "" {
		return false
	}
	where, args := b.where()
	var n int
	if err := s.store.Pool.QueryRow(c, `SELECT count(*) FROM login_attempts WHERE `+where+` AND NOT success AND attempted_at > $`+strconv.Itoa(len(args)+1),
		append(args, s.asFailWindowStart(c, st, b))...).Scan(&n); err != nil {
		return false
	}
	if n > max {
		return true
	}
	// A lockout created by a parallel request moves the window start past the failures that caused it,
	// so a request that passed the lockout check before it existed would count almost nothing here. Checking
	// the lockout after the count closes that gap: a lockout created before the window start was read is
	// seen now, and one created after it leaves those failures in the count.
	_, locked := s.asLockedUntil(c, b.kind, b.value)
	return locked
}

// adminsecLoginFailed records a failed attempt (wrong user, password or code) and locks the client
// network, or the account from this network, out once it has too many recent failures. attemptID is the
// attempt adminsecBeginAttempt stored as pending (0: record it now); it becomes a completed failure here.
func (s *Server) adminsecLoginFailed(c *gin.Context, login string, u *store.User, reason string, attemptID int64) {
	st := s.asLoad()
	ip := clientIP(c)
	name := s.asNormLogin(c, login, u)
	if attemptID == 0 {
		s.auth.RecordAttempt(c, ip, name, false)
	} else {
		s.auth.AttemptFailed(c, attemptID)
	}
	if !st.LogFailedSkip {
		s.asAudit(c, asEvent{Event: "login_failed", Severity: "notice", User: u, Username: login, Object: name, Message: reason})
	}
	if maintIPAllowed(st.AllowList, ip) {
		return
	}
	net := auth.ClientNet(ip)
	s.asMaybeLock(c, st, asNetBucket(net), st.IPMaxFailures)
	s.asMaybeLock(c, st, asUserBucket(name, net), st.UserMaxFailures)
}

func (s *Server) asMaybeLock(c *gin.Context, st asSettings, b asBucket, max int) {
	if max <= 0 || b.value == "" {
		return
	}
	asMu.Lock()
	defer asMu.Unlock()
	if _, ok := s.asLockedUntil(c, b.kind, b.value); ok {
		return
	}
	// Completed failures only: attempts still being checked (pending) may turn out to be correct passwords.
	where, args := b.where()
	var n int
	if err := s.store.Pool.QueryRow(c, `SELECT count(*) FROM login_attempts WHERE `+where+` AND NOT success AND NOT pending AND attempted_at > $`+strconv.Itoa(len(args)+1),
		append(args, s.asFailWindowStart(c, st, b))...).Scan(&n); err != nil || n < max {
		return
	}
	var recent int
	_ = s.store.Pool.QueryRow(c, `SELECT count(*) FROM adminsec_lockouts WHERE kind = $1 AND value = $2 AND created_at > now() - interval '24 hours'`, b.kind, b.value).Scan(&recent)
	dur := time.Duration(max1(st.LockoutMinutes)) * time.Minute
	long := st.LongLockoutAfter > 0 && recent+1 >= st.LongLockoutAfter
	if long {
		dur = time.Duration(max1(st.LongLockoutHours)) * time.Hour
	}
	until := time.Now().Add(dur)
	reason := fmt.Sprintf("%d failed login attempts", n)
	if _, err := s.store.Pool.Exec(c, `INSERT INTO adminsec_lockouts (kind, value, reason, failures, long_lock, until) VALUES ($1,$2,$3,$4,$5,$6)`,
		b.kind, b.value, reason, n, long, until); err != nil {
		return
	}
	label := "IP address " + b.value
	if b.kind == "user" {
		label = "username " + b.value
	}
	s.asAudit(c, asEvent{Event: "lockout", Severity: "warning", Username: b.login, Object: label,
		Message: fmt.Sprintf("Locked out until %s after %s.", until.UTC().Format("2006-01-02 15:04 UTC"), reason)})
	if b.kind == "user" {
		s.asUserNetLockoutAlert(c, b, until, n)
	} else {
		s.asLockoutAlert(c, b.kind, b.value, until, n)
	}
}

// asUserNetLockoutAlert is asLockoutAlert for a lockout of one account from one network: it says that only
// that network is locked out, so the mail does not read as if the account itself were.
func (s *Server) asUserNetLockoutAlert(c *gin.Context, b asBucket, until time.Time, failures int) {
	st := s.asLoad()
	if !st.AlertLockout {
		return
	}
	if _, err := s.store.UserByLoginOrEmail(c, b.login); err != nil { // a made-up username is not worth a mail
		return
	}
	if !asLockoutAlertSlot(time.Now()) {
		return
	}
	site := s.settings.String("blogname")
	body := fmt.Sprintf("The network %s was locked out of the username %s on %s after %d failed login attempts. "+
		"Sign-ins to that account from other networks are not affected.\n\nLocked until: %s\nLast attempt from: %s\n\nLockouts: %s\n",
		b.net, strconv.Quote(b.login), site, failures, until.UTC().Format(time.RFC1123), clientIP(c), s.links(c).HomeURL("/wp-admin/admin?page=adminsecgo-lockouts"))
	s.sendMail(c, mail.Message{To: []string{s.asAlertTo(st)}, Subject: "[" + site + "] Login lockout", Body: body})
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

// adminsecLoginError is the message for a wrong username or password: WordPress' detailed one,
// or one generic sentence that does not tell which part was wrong.
func (s *Server) adminsecGenericError() (template.HTML, bool) {
	if s.adminsecOn() && !s.asLoad().GenericErrors {
		return "", false // AdminSecGo active with generic errors switched off by the administrator
	}
	return template.HTML(`<strong>Error:</strong> The username or password you entered is incorrect. <a href="` + s.loginPath() + `?action=lostpassword">Lost your password?</a>`), true
}

// --- remembered devices ------------------------------------------------------------------

func (s *Server) asDeviceSig(id, userID int64, token string) string {
	return s.asHash("device", strconv.FormatInt(id, 10)+"|"+strconv.FormatInt(userID, 10)+"|"+token)[:32]
}

// asRememberedDevice reports whether the request carries a valid remembered-device cookie of u.
func (s *Server) asRememberedDevice(c *gin.Context, u *store.User) bool {
	if s.asLoad().RememberDays <= 0 {
		return false
	}
	ck, err := c.Cookie(asDeviceCookie)
	if err != nil || ck == "" {
		return false
	}
	for _, part := range strings.Split(ck, "~") { // one cookie may remember several accounts
		f := strings.Split(part, ".")
		if len(f) != 3 {
			continue
		}
		id, _ := strconv.ParseInt(f[0], 10, 64)
		if !hmac.Equal([]byte(s.asDeviceSig(id, u.ID, f[1])), []byte(f[2])) {
			continue
		}
		sum := sha256.Sum256([]byte(f[1]))
		tag, err := s.store.Pool.Exec(c, `UPDATE adminsec_devices SET last_used_at = now() WHERE id = $1 AND user_id = $2 AND token_hash = $3 AND expires_at > now()`,
			id, u.ID, sum[:])
		if err == nil && tag.RowsAffected() == 1 {
			return true
		}
	}
	return false
}

func (s *Server) asRememberDevice(c *gin.Context, u *store.User) {
	days := s.asLoad().RememberDays
	if days <= 0 {
		return
	}
	token := auth.RandomToken(24)
	sum := sha256.Sum256([]byte(token))
	ua := c.Request.UserAgent()
	if len(ua) > 400 {
		ua = ua[:400]
	}
	var id int64
	if err := s.store.Pool.QueryRow(c, `INSERT INTO adminsec_devices (user_id, token_hash, user_agent, ip, expires_at) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		u.ID, sum[:], ua, clientIP(c), time.Now().Add(time.Duration(days)*24*time.Hour)).Scan(&id); err != nil {
		return
	}
	val := fmt.Sprintf("%d.%s.%s", id, token, s.asDeviceSig(id, u.ID, token))
	if old, err := c.Cookie(asDeviceCookie); err == nil && old != "" {
		parts := strings.Split(old, "~")
		if len(parts) > 4 {
			parts = parts[len(parts)-4:]
		}
		val = strings.Join(append(parts, val), "~")
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: asDeviceCookie, Value: val, Path: "/", MaxAge: days * 86400, HttpOnly: true,
		Secure: s.requestHTTPS(c.Request), SameSite: http.SameSiteLaxMode})
	s.asAudit(c, asEvent{Event: "device_remembered", User: u, Message: fmt.Sprintf("Device remembered for %d days.", days)})
}

// --- challenges --------------------------------------------------------------------------

type asChallenge struct {
	ID, UserID  int64
	Purpose     string
	Remember    bool
	RedirectTo  string
	IP, Email   string
	CodeHash    string
	CodeExpires *time.Time
	CodeSentAt  *time.Time
	Sends       int
	Attempts    int
	ExpiresAt   time.Time
}

func asTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte("adminsec|" + token))
	return sum[:]
}

func (s *Server) asNewChallenge(ctx context.Context, ch *asChallenge) (string, error) {
	token := auth.RandomToken(32)
	ch.ExpiresAt = time.Now().Add(asChallengeTTL)
	err := s.store.Pool.QueryRow(ctx, `INSERT INTO adminsec_challenges (token_hash, user_id, purpose, remember, redirect_to, ip, email, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, asTokenHash(token), ch.UserID, ch.Purpose, ch.Remember, ch.RedirectTo, ch.IP, ch.Email, ch.ExpiresAt).Scan(&ch.ID)
	return token, err
}

func (s *Server) asChallengeByToken(ctx context.Context, token, purpose string) (*asChallenge, error) {
	if token == "" {
		return nil, pgx.ErrNoRows
	}
	ch := &asChallenge{}
	err := s.store.Pool.QueryRow(ctx, `SELECT id, user_id, purpose, remember, redirect_to, ip, email, code_hash, code_expires, code_sent_at, sends, attempts, expires_at
		FROM adminsec_challenges WHERE token_hash = $1 AND purpose = $2 AND expires_at > now()`, asTokenHash(token), purpose).
		Scan(&ch.ID, &ch.UserID, &ch.Purpose, &ch.Remember, &ch.RedirectTo, &ch.IP, &ch.Email, &ch.CodeHash, &ch.CodeExpires, &ch.CodeSentAt, &ch.Sends, &ch.Attempts, &ch.ExpiresAt)
	return ch, err
}

func (s *Server) asDropChallenge(ctx context.Context, ch *asChallenge) {
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM adminsec_challenges WHERE id = $1`, ch.ID)
}

// asResendWait is how long until another code may be sent (0: now).
func (ch *asChallenge) resendWait() time.Duration {
	if ch.CodeSentAt == nil {
		return 0
	}
	if d := asResendCooldown - time.Since(*ch.CodeSentAt); d > 0 {
		return d
	}
	return 0
}

var errAsCooldown = errors.New("cooldown")
var errAsTooMany = errors.New("too many codes")

// asSendCode e-mails a new 6-digit code for the challenge (10 minutes, single use) to addr.
func (s *Server) asSendCode(c *gin.Context, ch *asChallenge, u *store.User, addr string) error {
	if ch.resendWait() > 0 {
		return errAsCooldown
	}
	if ch.Sends >= asMaxCodeSends {
		return errAsTooMany
	}
	code := randomDigits(6)
	now := time.Now()
	exp := now.Add(asCodeTTL)
	if _, err := s.store.Pool.Exec(c, `UPDATE adminsec_challenges SET code_hash = $2, code_expires = $3, code_sent_at = $4, sends = sends + 1 WHERE id = $1`,
		ch.ID, s.asHash("code", strconv.FormatInt(ch.ID, 10)+"|"+code), exp, now); err != nil {
		return err
	}
	ch.CodeExpires, ch.CodeSentAt = &exp, &now
	ch.Sends++
	site := s.settings.String("blogname")
	what := "log in to"
	subject := "[" + site + "] Your login code"
	if ch.Purpose == "setup_email" {
		what, subject = "confirm this e-mail address for two-step login on", "["+site+"] Confirm your e-mail for login codes"
	}
	body := fmt.Sprintf("Howdy %s,\n\nYour code to %s %s is:\n\n    %s\n\nThe code expires in 10 minutes and can be used once.\n\nRequested from IP address %s.\nIf this wasn't you, change your password: someone may know it.\n",
		u.DisplayName, what, site, code, clientIP(c))
	s.sendMail(c, mail.Message{To: []string{addr}, Subject: subject, Body: body})
	return nil
}

// asReserveAttempt counts a code check against the challenge before the code is verified, so
// parallel requests cannot each test a code against the same remaining budget. It reports false
// (and drops the challenge) once the limit is used up.
func (s *Server) asReserveAttempt(ctx context.Context, ch *asChallenge) bool {
	var n int
	if err := s.store.Pool.QueryRow(ctx, `UPDATE adminsec_challenges SET attempts = attempts + 1
		WHERE id = $1 AND attempts < $2 RETURNING attempts`, ch.ID, asMaxCodeAttempts).Scan(&n); err != nil {
		s.asDropChallenge(ctx, ch)
		ch.Attempts = asMaxCodeAttempts
		return false
	}
	ch.Attempts = n
	return true
}

// asClaimChallenge deletes the challenge and reports whether this request was the one that did,
// so one challenge yields one session.
func (s *Server) asClaimChallenge(ctx context.Context, ch *asChallenge) bool {
	tag, err := s.store.Pool.Exec(ctx, `DELETE FROM adminsec_challenges WHERE id = $1`, ch.ID)
	return err == nil && tag.RowsAffected() == 1
}

// asCheckCode verifies an e-mailed code; a correct code is spent. The caller reserves the attempt
// first (asReserveAttempt).
func (s *Server) asCheckCode(ctx context.Context, ch *asChallenge, code string) bool {
	code = normalizeCode(code)
	if ch.CodeHash == "" || ch.CodeExpires == nil || time.Now().After(*ch.CodeExpires) || len(code) != 6 {
		return false
	}
	if !hmac.Equal([]byte(ch.CodeHash), []byte(s.asHash("code", strconv.FormatInt(ch.ID, 10)+"|"+code))) {
		return false
	}
	tag, err := s.store.Pool.Exec(ctx, `UPDATE adminsec_challenges SET code_hash = '' WHERE id = $1 AND code_hash = $2`, ch.ID, ch.CodeHash)
	return err == nil && tag.RowsAffected() == 1
}

func asMaskEmail(e string) string {
	at := strings.LastIndex(e, "@")
	if at < 1 {
		return e
	}
	name := e[:at]
	keep := 1
	if len(name) > 4 {
		keep = 2
	}
	return name[:keep] + strings.Repeat("•", max(len(name)-keep, 3)) + e[at:]
}

// --- the login flow ------------------------------------------------------------------------

type asLoginView struct {
	loginView
	Token        string
	Provider     string // totp, email, backup, passkey
	HasTOTP      bool
	HasEmail     bool
	HasPasskey   bool
	PasskeyOpts  string // JSON request options while Provider is passkey
	PasskeyToken string
	BackupLeft   int
	MaskedEmail  string
	RememberDays int
	ResendWait   int
	FormAction   string
}

// adminsecPasswordOK takes over after a correct password while the plugin is on: the policy check,
// then either the second-factor challenge or the end of the login.
func (s *Server) adminsecPasswordOK(c *gin.Context, v loginView, u *store.User, pass string) {
	st := s.asLoad()
	f := s.asFactors(c, u.ID)
	if st.PwCheckLogin && !f.any() && !s.asForcePassword(c, u) {
		if msg := s.adminsecUserPasswordProblem(u, pass); msg != "" {
			_ = s.setUserMeta(c, u, "adminsec_force_password", true)
			s.asAudit(c, asEvent{Event: "password_change_forced", Severity: "notice", User: u, Object: u.Login, Message: "The password no longer meets the policy: " + msg})
		}
	}
	if !f.any() || s.asRememberedDevice(c, u) {
		method := "password"
		if f.any() {
			method = "password + remembered device"
		}
		s.asFinishLogin(c, u, v.Remember, v.RedirectTo, method)
		return
	}
	s.asStartChallenge(c, v, u, f, "login")
}

// asStartChallenge opens the second step of a login: purpose "login" after a password, "login_pk"
// after a passkey sign-in that does not count as two-step login (the passkey can't be the second step).
func (s *Server) asStartChallenge(c *gin.Context, v loginView, u *store.User, f *asFactors, purpose string) {
	ch := &asChallenge{UserID: u.ID, Purpose: purpose, Remember: v.Remember, RedirectTo: v.RedirectTo, IP: clientIP(c)}
	token, err := s.asNewChallenge(c, ch)
	if err != nil {
		v.Error = "<strong>Error:</strong> Could not start the verification. Please try again."
		s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
		return
	}
	provider := asDefaultProvider(f, ch)
	if provider == "email" {
		_ = s.asSendCode(c, ch, u, f.Email)
	}
	s.asRenderChallenge(c, v, token, ch, u, f, provider, "", "")
}

// asPasskeyOnly reports whether the second step after a password must be the passkey: the user's role
// requires one and the user has one. Backup codes stay as the way back in; the app and e-mailed codes
// are not offered, so the requirement means passkey logins, not only a registered passkey.
func (s *Server) asPasskeyOnly(u *store.User, f *asFactors, ch *asChallenge) bool {
	return asPasskeyStep(f, ch) && strings.Contains(s.asLoad().required(u), "passkey")
}

// asPasskeyStep reports whether the challenge may be answered with a passkey.
func asPasskeyStep(f *asFactors, ch *asChallenge) bool {
	return f.Passkeys > 0 && ch.Purpose == "login"
}

// asDefaultProvider is the method a challenge opens with: a passkey (no code to type or mail), then
// the authenticator app, then an e-mailed code.
func asDefaultProvider(f *asFactors, ch *asChallenge) string {
	switch {
	case asPasskeyStep(f, ch):
		return "passkey"
	case f.TOTPEnabled:
		return "totp"
	case f.EmailEnabled:
		return "email"
	}
	return "backup"
}

func (s *Server) asRenderChallenge(c *gin.Context, v loginView, token string, ch *asChallenge, u *store.User, f *asFactors, provider string, errMsg, msg template.HTML) {
	only := s.asPasskeyOnly(u, f, ch)
	lv := asLoginView{loginView: v, Token: token, Provider: provider, HasTOTP: f.TOTPEnabled && !only, HasEmail: f.EmailEnabled && !only, HasPasskey: asPasskeyStep(f, ch),
		BackupLeft: f.backupLeft(), MaskedEmail: asMaskEmail(f.Email), RememberDays: s.asLoad().RememberDays, FormAction: s.loginPath() + "?action=adminsec_2fa"}
	lv.Title, lv.Action = "Verify it's you", "adminsec-2fa"
	lv.Error, lv.Message = errMsg, msg
	lv.ResendWait = int(ch.resendWait().Seconds() + 0.99)
	if provider == "passkey" {
		lv.PasskeyOpts, lv.PasskeyToken = s.asPasskeySecondOptions(c, ch, u)
	}
	c.Header("Cache-Control", "no-store")
	s.renderAdminTemplate(c, http.StatusOK, "admin/login-adminsec", lv)
}

// adminsecChallengePost is the challenge form's POST (loginPath?action=adminsec_2fa).
func (s *Server) adminsecChallengePost(c *gin.Context, v loginView) {
	if c.Request.Method != http.MethodPost {
		c.Redirect(http.StatusFound, s.loginPath())
		return
	}
	token := c.PostForm("adminsec_token")
	ch, err := s.asChallengeByToken(c, token, "login")
	if err != nil {
		ch, err = s.asChallengeByToken(c, token, "login_pk")
	}
	expired := func() {
		v.Error = "<strong>Error:</strong> Your verification expired or was used. Please log in again."
		s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
	}
	if err != nil {
		expired()
		return
	}
	u, err := s.store.UserByID(c, ch.UserID)
	if err != nil {
		expired()
		return
	}
	if msg := s.adminsecPreLogin(c, u.Login); msg != "" {
		s.asDropChallenge(c, ch)
		v.Error = msg
		s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
		return
	}
	f := s.asFactors(c, u.ID)
	first := map[bool]string{true: "passkey", false: "password"}[ch.Purpose == "login_pk"]
	if !f.any() || (ch.Purpose == "login_pk" && !f.EmailEnabled && !f.TOTPEnabled) { // factors were reset meanwhile
		s.asDropChallenge(c, ch)
		s.asFinishLogin(c, u, ch.Remember, ch.RedirectTo, first)
		return
	}
	provider := c.PostForm("provider")
	switch provider {
	case "totp", "email", "backup", "passkey":
	default:
		provider = asDefaultProvider(f, ch)
	}
	if (provider == "totp" && !f.TOTPEnabled) || (provider == "passkey" && !asPasskeyStep(f, ch)) || (provider == "email" && !f.EmailEnabled) {
		provider = asDefaultProvider(f, ch)
	}
	if (provider == "totp" || provider == "email") && s.asPasskeyOnly(u, f, ch) {
		provider = "passkey"
	}
	v.Login = u.Login
	switch c.PostForm("adminsec_do") {
	case "switch": // "Email me a code" / "Use your authenticator app" / "Use a backup code"
		var msg, errMsg template.HTML
		if provider == "email" {
			switch err := s.asSendCode(c, ch, u, f.Email); err {
			case nil:
				msg = template.HTML("We e-mailed a code to <strong>" + template.HTMLEscapeString(asMaskEmail(f.Email)) + "</strong>.")
			case errAsCooldown:
				msg = "A code was sent a moment ago. Check your inbox, or wait before asking for another one."
			default:
				errMsg = "<strong>Error:</strong> No more codes can be sent for this login. Log in again to start over."
			}
		}
		s.asRenderChallenge(c, v, token, ch, u, f, provider, errMsg, msg)
		return
	case "resend":
		if s.asPasskeyOnly(u, f, ch) { // no codes are mailed while a passkey is required
			s.asRenderChallenge(c, v, token, ch, u, f, provider, "", "")
			return
		}
		var msg, errMsg template.HTML
		switch err := s.asSendCode(c, ch, u, f.Email); err {
		case nil:
			msg = "A new code is on its way."
		case errAsCooldown:
			errMsg = template.HTML("<strong>Error:</strong> Please wait " + strconv.Itoa(int(ch.resendWait().Seconds()+0.99)) + " seconds before asking for another code.")
		default:
			errMsg = "<strong>Error:</strong> No more codes can be sent for this login. Log in again to start over."
		}
		s.asRenderChallenge(c, v, token, ch, u, f, "email", errMsg, msg)
		return
	}
	code := c.PostForm("authcode")
	// Count the attempt before checking the code: parallel requests share one budget.
	if !s.asReserveAttempt(c, ch) {
		v.Error = "<strong>Error:</strong> Too many wrong codes. Please log in again."
		s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
		return
	}
	ok := false
	method := ""
	switch provider {
	case "totp":
		ok, method = s.asCheckTOTP(c, f, code), first+" + authenticator app"
	case "email":
		ok, method = s.asCheckCode(c, ch, code), first+" + e-mailed code"
	case "passkey":
		ok, method = s.asCheckPasskeySecond(c, ch, u), "password + passkey"
	case "backup":
		ok, method = s.asUseBackupCode(c, f, code), first+" + backup code"
		if ok {
			s.asAudit(c, asEvent{Event: "backup_code_used", Severity: "notice", User: u, Message: fmt.Sprintf("Backup code used to log in; %d left.", f.backupLeft())})
		}
	}
	if !ok {
		reason := "Wrong or expired " + map[string]string{"totp": "authenticator", "email": "e-mailed", "backup": "backup"}[provider] + " code."
		if provider == "passkey" {
			reason = "Passkey not verified."
		}
		s.adminsecLoginFailed(c, u.Login, u, reason, 0)
		if ch.Attempts >= asMaxCodeAttempts {
			s.asDropChallenge(c, ch)
			v.Error = "<strong>Error:</strong> Too many wrong codes. Please log in again."
			s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
			return
		}
		errMsg := template.HTML("<strong>Error:</strong> The code is incorrect or has expired. Please try again.")
		if provider == "passkey" {
			errMsg = "<strong>Error:</strong> The passkey could not be verified. Please try again, or choose another way below."
		}
		s.asRenderChallenge(c, v, token, ch, u, f, provider, errMsg, "")
		return
	}
	if !s.asClaimChallenge(c, ch) { // another request finished this login already
		v.Error = "<strong>Error:</strong> This login was already completed or has expired. Please log in again."
		s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
		return
	}
	if c.PostForm("adminsec_remember") == "1" {
		s.asRememberDevice(c, u)
	}
	s.asFinishLogin(c, u, ch.Remember, ch.RedirectTo, method)
}

// asFinishLogin is the end of every login while the plugin is on: the session, the log and alert,
// and where the user goes (a forced password change and an expired requirement come first).
func (s *Server) asFinishLogin(c *gin.Context, u *store.User, remember bool, redirectTo, method string) {
	ip := clientIP(c)
	s.auth.RecordAttempt(c, ip, strings.ToLower(u.Login), true)
	sess, err := s.auth.Create(c, u.ID, remember, ip, c.Request.UserAgent())
	if err != nil {
		v := loginView{Title: "Log In", Action: "login", SiteName: s.settings.String("blogname"), HomeURL: s.links(c).HomeURL("/"),
			Error: "<strong>Error:</strong> Could not start a session. Please try again."}
		s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
		return
	}
	s.setLoginCookie(c, sess.Token, remember, int(auth.SessionRemember.Seconds()))
	s.asAudit(c, asEvent{Event: "login_success", User: u, Object: u.Login, Message: "Logged in with " + method + "."})
	s.asNoteLoginIP(c, u)
	target := safeRedirect(redirectTo, "/wp-admin/")
	if (target == "/wp-admin/" || target == "/wp-admin") && !u.Can("edit_posts", nil) {
		target = "/wp-admin/profile"
	}
	if blocked := s.adminsecBlocked(c, u); blocked != "" {
		target = blocked
	}
	c.Redirect(http.StatusFound, target)
}

// adminsecLogout logs the logout.
func (s *Server) adminsecLogout(c *gin.Context) {
	if u := currentUser(c); u != nil && s.adminsecOn() {
		s.asAudit(c, asEvent{Event: "logout", User: u, Object: u.Login})
	}
}

// adminsecRESTLocked reports whether an API sign-in is refused by a block or lockout. REST Basic
// authentication with an Application Password (which deliberately skips the second factor, as in
// WordPress' Two Factor plugin) obeys the block list and the lockout of the client network only: no
// account lockout stops a client that holds a valid Application Password, and its wrong passwords are
// limited per network in a bucket of their own (app_password_auth.go). A password sign-in through the
// API (the Headless API's /auth/local) obeys every lockout, as the login form does.
func (s *Server) adminsecRESTLocked(c *gin.Context, user string) bool {
	if !s.adminsecOn() {
		return false
	}
	if appPasswordAttempt(c, user) {
		return s.adminsecPreLogin(c, "") != ""
	}
	return s.adminsecPreLogin(c, user) != ""
}
