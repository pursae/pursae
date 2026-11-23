package server

import (
	"bytes"
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/mail"
	"wpgo/internal/store"
)

// The user's side of AdminSecGo: Users › Security (admin?page=adminsecgo-account) where anyone
// sets up e-mailed codes (MFA), the authenticator app (2FA), backup codes and remembered devices;
// the sessions block on the profile; the factor status and reset on user-edit and the Users list.

type asDeviceRow struct {
	ID                        int64
	Device, IP                string
	Created, LastUsed, Expire time.Time
}

type asAccountData struct {
	Page        string
	Nonce       string
	User        *store.User
	F           *asFactors
	MailReady   bool
	MailWhy     string
	MailFixURL  string
	Enroll      asEnrollment
	Step        string // "", mfa_verify, totp_setup, codes
	Token       string
	SetupEmail  string
	ResendWait  int
	QR          template.HTML
	ManualKey   string
	NewCodes    []string
	Devices     []asDeviceRow
	RememberOn  bool
	BackupLeft  int
	RequiredMsg string

	PasskeysOn     bool // the setting
	PasskeysUsable bool // and the site has a domain name passkeys can use
	PasskeyTwoStep bool
	Passkeys       []asPasskey
	MaxPasskeys    int
}

func (s *Server) asDevices(ctx context.Context, userID int64) []asDeviceRow {
	rows, err := s.store.Pool.Query(ctx, `SELECT id, user_agent, ip, created_at, last_used_at, expires_at FROM adminsec_devices
		WHERE user_id = $1 AND expires_at > now() ORDER BY last_used_at DESC`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []asDeviceRow
	for rows.Next() {
		var d asDeviceRow
		var ua string
		if rows.Scan(&d.ID, &ua, &d.IP, &d.Created, &d.LastUsed, &d.Expire) == nil {
			d.Device = asDeviceName(ua)
			out = append(out, d)
		}
	}
	return out
}

// asDeviceName turns a user agent into "Browser on OS".
func asDeviceName(ua string) string {
	browser := "Unknown browser"
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	case strings.Contains(ua, "curl/"):
		browser = "curl"
	case strings.HasPrefix(ua, "Go-http-client"):
		browser = "Go HTTP client"
	}
	osName := ""
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		osName = "iOS"
	case strings.Contains(ua, "Android"):
		osName = "Android"
	case strings.Contains(ua, "Windows"):
		osName = "Windows"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		osName = "macOS"
	case strings.Contains(ua, "CrOS"):
		osName = "ChromeOS"
	case strings.Contains(ua, "Linux"):
		osName = "Linux"
	}
	if osName == "" {
		return browser
	}
	return browser + " on " + osName
}

// adminsecAccount serves admin?page=adminsecgo-account for the current user.
func (s *Server) adminsecAccount(c *gin.Context) {
	if !s.adminsecOn() {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	u := currentUser(c)
	d := s.asAccountView(c, u)
	var notices []notice
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "adminsec-account") {
			s.denyNonce(c)
			return
		}
		if c.PostForm("adminsec_do") == "passkey_begin" { // JSON for the page's script
			s.asPasskeyBegin(c, u)
			return
		}
		ns, redirect := s.asAccountAction(c, u, d)
		if redirect != "" {
			c.Redirect(http.StatusFound, redirect)
			return
		}
		notices = ns
		f := s.asFactors(c, u.ID)
		d.F, d.BackupLeft, d.Enroll = f, f.backupLeft(), s.asEnrollState(c, u)
		d.Devices = s.asDevices(c, u.ID)
		d.Passkeys = s.asPasskeys(c, u.ID)
	} else {
		switch c.Query("done") {
		case "mfa-off":
			notices = append(notices, notice{Type: "success", Message: "E-mailed login codes are switched off.", Dismissible: true})
		case "totp-off":
			notices = append(notices, notice{Type: "success", Message: "The authenticator app is removed from your account.", Dismissible: true})
		case "devices":
			notices = append(notices, notice{Type: "success", Message: "Remembered devices forgotten: the next login on them asks for a code again.", Dismissible: true})
		}
		for _, p := range d.Passkeys {
			if p.CloneWarning {
				notices = append(notices, notice{Type: "error", Message: template.HTML("<strong>A passkey may have been copied.</strong> “" + template.HTMLEscapeString(p.Name) +
					"” sent a signature counter that went backwards, so it is no longer accepted. Remove it, and add it again if you still have the device.")})
			}
		}
		if d.F.TOTPPending != "" && c.Query("step") == "totp" {
			s.asTOTPStep(c, u, d, s.asReveal(d.F.TOTPPending))
		}
	}
	if d.Enroll.Required != "" && !d.Enroll.Satisfied {
		what := "two-step login with e-mailed codes"
		switch d.Enroll.Required {
		case "2fa":
			what = "an authenticator app (with e-mailed codes as its fallback)"
		case "passkey":
			what = "a passkey"
		case "2fa+passkey":
			what = "an authenticator app (with e-mailed codes as its fallback) and a passkey"
		}
		if d.PasskeyTwoStep && (d.Enroll.Required == "mfa" || d.Enroll.Required == "2fa") {
			what += " or a passkey"
		}
		msg := "Your role requires " + what + ". "
		if d.Enroll.Expired {
			msg += "The grace period is over: set it up now to continue using the dashboard."
		} else {
			msg += "Please set it up before " + d.Enroll.Deadline.In(s.loc()).Format("January 2, 2006") + "."
		}
		notices = append([]notice{{Type: map[bool]string{true: "error", false: "warning"}[d.Enroll.Expired], Message: template.HTML(template.HTMLEscapeString(msg))}}, notices...)
	}
	s.adminPage(c, "admin?page="+asSlug+"-account", "Security", "users_page_adminsecgo-account adminsecgo-admin", "admin/adminsecgo", d, notices...)
}

func (s *Server) asAccountView(c *gin.Context, u *store.User) *asAccountData {
	f := s.asFactors(c, u.ID)
	ready, why := s.asMailReady()
	st := s.asLoad()
	return &asAccountData{Page: "account", Nonce: s.nonce(c, "adminsec-account"), User: u, F: f, MailReady: ready, MailWhy: why,
		MailFixURL: s.asMailFixURL(), Enroll: s.asEnrollState(c, u), Devices: s.asDevices(c, u.ID),
		RememberOn: st.RememberDays > 0, BackupLeft: f.backupLeft(), SetupEmail: u.Email,
		PasskeysOn: st.Passkeys, PasskeysUsable: s.asPasskeysUsable(c), PasskeyTwoStep: st.Passkeys && st.PasskeyTwoStep,
		Passkeys: s.asPasskeys(c, u.ID), MaxPasskeys: asMaxPasskeys}
}

func (s *Server) asTOTPStep(c *gin.Context, u *store.User, d *asAccountData, secret string) {
	site := s.settings.String("blogname")
	if site == "" {
		site = "Pursae"
	}
	d.Step, d.ManualKey = "totp_setup", groupKey(secret)
	if q, err := qrEncode([]byte(otpauthURI(site, u.Login, secret))); err == nil {
		d.QR = template.HTML(q.SVG(220))
	}
}

func errNotice(msg string) []notice {
	return []notice{{Type: "error", Message: template.HTML("<strong>Error:</strong> " + template.HTMLEscapeString(msg))}}
}

func okNotice(msg string) []notice {
	return []notice{{Type: "success", Message: template.HTML(template.HTMLEscapeString(msg)), Dismissible: true}}
}

// asAccountAction handles the Security screen's forms.
func (s *Server) asAccountAction(c *gin.Context, u *store.User, d *asAccountData) ([]notice, string) {
	f := d.F
	back := "/wp-admin/admin?page=" + asSlug + "-account"
	needPassword := func() bool {
		return s.checkUserPassword(c.Request.Context(), u, c.PostForm("adminsec_password"))
	}
	required := d.Enroll.Required
	if required == "" {
		required = s.asLoad().required(u)
	}
	switch c.PostForm("adminsec_do") {
	case "mfa_send", "mfa_resend":
		if !d.MailReady {
			return errNotice(d.MailWhy), ""
		}
		var ch *asChallenge
		addr := strings.TrimSpace(c.PostForm("email"))
		if c.PostForm("adminsec_do") == "mfa_resend" {
			var err error
			if ch, err = s.asChallengeByToken(c, c.PostForm("adminsec_token"), "setup_email"); err != nil || ch.UserID != u.ID {
				return errNotice("The confirmation expired. Please start again."), ""
			}
			addr = ch.Email
			d.Token = c.PostForm("adminsec_token")
		} else {
			if !validEmail(addr) {
				return errNotice("Please enter a valid e-mail address."), ""
			}
			// Re-pointing an existing factor is as strong as turning it off: it needs the password.
			if (f.EmailEnabled || f.TOTPEnabled) && !needPassword() {
				return errNotice("Your current password is incorrect."), ""
			}
			ch = &asChallenge{UserID: u.ID, Purpose: "setup_email", Email: addr, IP: clientIP(c)}
			token, err := s.asNewChallenge(c, ch)
			if err != nil {
				return errNotice("Could not start the confirmation."), ""
			}
			d.Token = token
		}
		d.Step, d.SetupEmail = "mfa_verify", addr
		err := s.asSendCode(c, ch, u, addr)
		d.ResendWait = int(ch.resendWait().Seconds() + 0.99)
		switch err {
		case nil:
			return okNotice("We sent a 6-digit code to " + addr + ". Enter it below to confirm the address."), ""
		case errAsCooldown:
			return errNotice("Please wait a minute before asking for another code."), ""
		case errAsTooMany:
			return errNotice("Too many codes were sent. Please start again."), ""
		}
		return errNotice("The code could not be sent."), ""
	case "mfa_verify":
		ch, err := s.asChallengeByToken(c, c.PostForm("adminsec_token"), "setup_email")
		if err != nil || ch.UserID != u.ID {
			return errNotice("The confirmation expired. Please start again."), ""
		}
		if !s.asReserveAttempt(c, ch) {
			return errNotice("Too many wrong codes. Please start again."), ""
		}
		if !s.asCheckCode(c, ch, c.PostForm("code")) {
			if ch.Attempts >= asMaxCodeAttempts {
				s.asDropChallenge(c, ch)
				return errNotice("Too many wrong codes. Please start again."), ""
			}
			d.Step, d.Token, d.SetupEmail, d.ResendWait = "mfa_verify", c.PostForm("adminsec_token"), ch.Email, int(ch.resendWait().Seconds()+0.99)
			return errNotice("The code is incorrect or has expired."), ""
		}
		if !s.asClaimChallenge(c, ch) {
			return errNotice("The confirmation expired. Please start again."), ""
		}
		now := time.Now()
		prev := f.Email
		changed := f.EmailEnabled && prev != "" && !strings.EqualFold(prev, ch.Email)
		f.EmailEnabled, f.Email, f.EmailVerifiedAt = true, ch.Email, &now
		if err := s.asSaveFactors(c, f); err != nil {
			return errNotice(err.Error()), ""
		}
		if changed { // the old address learns of the change; remembered devices must pass the new factor
			_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_devices WHERE user_id = $1`, u.ID)
			site := s.settings.String("blogname")
			s.sendMail(c, mail.Message{To: []string{prev}, Subject: "[" + site + "] Login codes now go to another address",
				Body: "Hi " + u.Login + ",\n\nThe e-mail address that receives your login codes on " + site + " was changed from this address to " +
					asMaskEmail(ch.Email) + ".\n\nIf you did not do this, contact the site administrator right away.\n"})
		}
		s.asAudit(c, asEvent{Event: "mfa_enabled", Severity: "notice", Object: u.Login, Message: "E-mailed login codes enabled (" + ch.Email + ")."})
		if len(f.Backup) == 0 {
			codes, _ := s.asNewBackupCodes(c, f)
			d.Step, d.NewCodes = "codes", codes
			s.asAudit(c, asEvent{Event: "backup_codes_generated", Object: u.Login})
		}
		return okNotice("E-mailed login codes are on. From now on, logging in asks for a code sent to " + ch.Email + "."), ""
	case "mfa_disable":
		if f.TOTPEnabled {
			return errNotice("Remove the authenticator app first: e-mailed codes are its fallback."), ""
		}
		if without := *f; required != "" {
			without.EmailEnabled = false
			if !s.asLoad().satisfies(required, &without) {
				return errNotice("Your role requires two-step login, so it cannot be switched off."), ""
			}
		}
		if !needPassword() {
			return errNotice("Your current password is incorrect."), ""
		}
		f.EmailEnabled, f.EmailVerifiedAt = false, nil
		if f.Passkeys == 0 { // backup codes stay while passkeys still use them
			f.Backup, f.BackupAt = nil, nil
		}
		_ = s.asSaveFactors(c, f)
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_devices WHERE user_id = $1`, u.ID)
		s.asAudit(c, asEvent{Event: "mfa_disabled", Severity: "warning", Object: u.Login, Message: "E-mailed login codes disabled."})
		return nil, back + "&done=mfa-off"
	case "totp_start":
		if !f.EmailEnabled {
			return errNotice("Set up e-mailed codes first: they are the fallback when you can't use the app."), ""
		}
		secret := newTOTPSecret()
		f.TOTPPending = s.asSeal(secret)
		if err := s.asSaveFactors(c, f); err != nil {
			return errNotice(err.Error()), ""
		}
		s.asTOTPStep(c, u, d, secret)
		return nil, ""
	case "totp_verify":
		secret := s.asReveal(f.TOTPPending)
		if secret == "" {
			return errNotice("The setup expired. Please start again."), ""
		}
		step, ok := totpMatch(secret, c.PostForm("code"), time.Now(), 0)
		if !ok {
			s.asTOTPStep(c, u, d, secret)
			return errNotice("The code is incorrect. Check that the time on your phone is right, then enter the current code."), ""
		}
		now := time.Now()
		f.TOTPEnabled, f.TOTPSecret, f.TOTPPending, f.TOTPLastStep, f.TOTPEnabledAt = true, f.TOTPPending, "", step, &now
		if err := s.asSaveFactors(c, f); err != nil {
			return errNotice(err.Error()), ""
		}
		s.asAudit(c, asEvent{Event: "totp_enabled", Severity: "notice", Object: u.Login, Message: "Authenticator app enabled."})
		if len(f.Backup) == 0 || f.backupLeft() == 0 {
			codes, _ := s.asNewBackupCodes(c, f)
			d.Step, d.NewCodes = "codes", codes
			s.asAudit(c, asEvent{Event: "backup_codes_generated", Object: u.Login})
		}
		return okNotice("Two-factor authentication is on: logging in now asks for the code from your authenticator app."), ""
	case "totp_cancel":
		f.TOTPPending = ""
		_ = s.asSaveFactors(c, f)
		return nil, back
	case "totp_disable":
		if without := *f; required != "" {
			without.TOTPEnabled = false
			if !s.asLoad().satisfies(required, &without) {
				return errNotice("Your role requires an authenticator app, so it cannot be removed."), ""
			}
		}
		if !needPassword() {
			return errNotice("Your current password is incorrect."), ""
		}
		f.TOTPEnabled, f.TOTPSecret, f.TOTPPending, f.TOTPLastStep, f.TOTPEnabledAt = false, "", "", 0, nil
		_ = s.asSaveFactors(c, f)
		s.asAudit(c, asEvent{Event: "totp_disabled", Severity: "warning", Object: u.Login, Message: "Authenticator app removed."})
		return nil, back + "&done=totp-off"
	case "backup_regen":
		if !f.any() {
			return errNotice("Backup codes come with two-step login: set it up first."), ""
		}
		if !needPassword() {
			return errNotice("Your current password is incorrect."), ""
		}
		codes, err := s.asNewBackupCodes(c, f)
		if err != nil {
			return errNotice(err.Error()), ""
		}
		d.Step, d.NewCodes = "codes", codes
		s.asAudit(c, asEvent{Event: "backup_codes_generated", Severity: "notice", Object: u.Login, Message: "New backup codes generated; the old ones no longer work."})
		return okNotice("New backup codes generated. The old ones no longer work."), ""
	case "passkey_finish":
		return s.asPasskeyFinish(c, u, d), ""
	case "passkey_rename":
		return s.asPasskeyRename(c, u), ""
	case "passkey_delete":
		return s.asPasskeyDelete(c, u, required), ""
	case "devices_forget":
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_devices WHERE user_id = $1`, u.ID)
		s.asAudit(c, asEvent{Event: "devices_forgotten", Object: u.Login})
		return nil, back + "&done=devices"
	case "device_forget":
		id, _ := strconv.ParseInt(c.PostForm("device"), 10, 64)
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_devices WHERE user_id = $1 AND id = $2`, u.ID, id)
		s.asAudit(c, asEvent{Event: "devices_forgotten", Object: u.Login, Message: "One remembered device forgotten."})
		return nil, back + "&done=devices"
	}
	return nil, back
}

// --- profile block -------------------------------------------------------------------------

type asSessionRow struct {
	ID                int64
	Device, IP        string
	Created, LastSeen time.Time
	Current           bool
}

type asProfileData struct {
	Self, CanManage bool
	TargetID        int64
	F               *asFactors
	BackupLeft      int
	Enroll          asEnrollment
	RequiredLabel   string
	Passkeys        int
	Sessions        []asSessionRow
	Devices         int
	ForcePassword   bool
	AccountURL      string
	IdleMinutes     int
}

func (s *Server) asSessions(ctx context.Context, userID, currentID int64) []asSessionRow {
	rows, err := s.store.Pool.Query(ctx, `SELECT id, user_agent, ip, created_at, last_seen_at FROM sessions WHERE user_id = $1 AND expires_at > now()
		ORDER BY last_seen_at DESC LIMIT 50`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []asSessionRow
	for rows.Next() {
		var r asSessionRow
		var ua string
		if rows.Scan(&r.ID, &ua, &r.IP, &r.Created, &r.LastSeen) == nil {
			r.Device, r.Current = asDeviceName(ua), r.ID == currentID
			out = append(out, r)
		}
	}
	return out
}

// adminsecProfileHTML is the AdminSecGo block of profile.php / user-edit.php.
func (s *Server) adminsecProfileHTML(c *gin.Context, target *store.User, self bool) template.HTML {
	if !s.adminsecOn() {
		return ""
	}
	u := currentUser(c)
	var cur int64
	if sess := currentSession(c); sess != nil && self {
		cur = sess.ID
	}
	f := s.asFactors(c, target.ID)
	d := asProfileData{Self: self, CanManage: !self && u.Can("edit_users", nil), TargetID: target.ID, F: f, BackupLeft: f.backupLeft(),
		Sessions: s.asSessions(c, target.ID, cur), Devices: len(s.asDevices(c, target.ID)), ForcePassword: s.asForcePassword(c, target),
		AccountURL: "/wp-admin/admin?page=" + asSlug + "-account", IdleMinutes: s.asLoad().IdleMinutes}
	d.Enroll = s.asEnrollState(c, target)
	d.Passkeys = f.Passkeys
	d.RequiredLabel = asRequiredLabel(d.Enroll.Required)
	var buf bytes.Buffer
	if err := s.tmpl.get(false).ExecuteTemplate(&buf, "adminsecgo-profile", d); err != nil {
		return template.HTML("<!-- adminsecgo: " + template.HTMLEscapeString(err.Error()) + " -->")
	}
	return template.HTML(buf.String())
}

// adminsecProfileAction handles the block's buttons inside the profile form (already nonce-checked).
func (s *Server) adminsecProfileAction(c *gin.Context, target *store.User, back string) bool {
	if !s.adminsecOn() {
		return false
	}
	act := c.PostForm("adminsec_action")
	if act == "" {
		return false
	}
	u := currentUser(c)
	self := u.ID == target.ID
	manage := !self && u.Can("edit_users", nil)
	back = strings.Replace(back, "updated=1", "adminsec=done", 1)
	switch {
	case strings.HasPrefix(act, "revoke-session:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(act, "revoke-session:"), 10, 64)
		if sess := currentSession(c); self && sess != nil && sess.ID == id {
			break // the current session ends with "Log Out"
		}
		if self || manage {
			if _, err := s.store.Pool.Exec(c, `DELETE FROM sessions WHERE id = $1 AND user_id = $2`, id, target.ID); err != nil {
				s.asSessionLogoutFailed(c, target, err)
				back = strings.Replace(back, "adminsec=done", "adminsec=failed", 1)
				break
			}
			s.asAudit(c, asEvent{Event: "sessions_logged_out", Object: target.Login, Message: "One session of " + target.Login + " ended."})
		}
	case act == "logout-others" && self:
		if err := s.auth.DestroyOthers(c, u.ID, currentSession(c).ID); err != nil {
			s.asSessionLogoutFailed(c, target, err)
			back = strings.Replace(back, "adminsec=done", "adminsec=failed", 1)
			break
		}
		s.asAudit(c, asEvent{Event: "sessions_logged_out", Object: target.Login, Message: "Logged out of every other session."})
	case act == "logout-all" && manage:
		if _, err := s.store.Pool.Exec(c, `DELETE FROM sessions WHERE user_id = $1`, target.ID); err != nil {
			s.asSessionLogoutFailed(c, target, err)
			back = strings.Replace(back, "adminsec=done", "adminsec=failed", 1)
			break
		}
		s.asAudit(c, asEvent{Event: "sessions_logged_out", Severity: "notice", Object: target.Login, Message: "Every session of " + target.Login + " ended by an administrator."})
	case act == "reset-factors" && manage:
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_factors WHERE user_id = $1`, target.ID)
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_devices WHERE user_id = $1`, target.ID)
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_challenges WHERE user_id = $1`, target.ID)
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_passkeys WHERE user_id = $1`, target.ID)
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_passkey_ceremonies WHERE user_id = $1`, target.ID)
		_ = s.setUserMeta(c, target, "adminsec_grace_start", nil)
		s.asAudit(c, asEvent{Event: "factors_reset", Severity: "warning", Object: target.Login, Message: "Two-step login factors, passkeys, backup codes and remembered devices of " + target.Login + " removed."})
	case act == "force-password" && manage:
		_ = s.setUserMeta(c, target, "adminsec_force_password", "admin")
		s.asAudit(c, asEvent{Event: "password_change_forced", Severity: "notice", Object: target.Login, Message: "Password change required at the next login."})
	case act == "unforce-password" && manage:
		_ = s.setUserMeta(c, target, "adminsec_force_password", nil)
	case act == "forget-devices" && (self || manage):
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_devices WHERE user_id = $1`, target.ID)
		s.asAudit(c, asEvent{Event: "devices_forgotten", Object: target.Login})
	}
	c.Redirect(http.StatusFound, back)
	return true
}

// adminsecProfileNotices are the profile's messages: a required password change, block actions.
func (s *Server) adminsecProfileNotices(c *gin.Context, u *store.User) []notice {
	if !s.adminsecOn() {
		return nil
	}
	var out []notice
	if s.asForcePassword(c, u) {
		out = append(out, notice{Type: "error", Message: "<strong>Please choose a new password.</strong> Your password has to be changed before you can continue (it is too weak for the site's password policy, or an administrator asked for a new one)."})
	}
	if c.Query("adminsec") == "done" {
		out = append(out, notice{Type: "success", Message: "<strong>Done.</strong>", Dismissible: true})
	}
	if c.Query("adminsec") == "failed" {
		out = append(out, notice{Type: "error", Message: "<strong>The sessions could not be ended.</strong> Please try again.", Dismissible: true})
	}
	return out
}

// --- Users list column ---------------------------------------------------------------------

// adminsecUsersColumn fills the Two-Step column of the Users list.
func (s *Server) adminsecUsersColumn(c *gin.Context, rows []userRow, users []*store.User) bool {
	if !s.adminsecOn() || len(rows) == 0 {
		return false
	}
	st := s.asLoad()
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	type fs struct {
		mfa, totp bool
		passkeys  int
	}
	have := map[int64]fs{}
	if res, err := s.store.Pool.Query(c, `SELECT u.id, coalesce(f.email_enabled, false), coalesce(f.totp_enabled, false),
		(SELECT count(*) FROM adminsec_passkeys p WHERE p.user_id = u.id AND NOT p.clone_warning)
		FROM users u LEFT JOIN adminsec_factors f ON f.user_id = u.id WHERE u.id = ANY($1)`, ids); err == nil {
		for res.Next() {
			var id int64
			var x fs
			if res.Scan(&id, &x.mfa, &x.totp, &x.passkeys) == nil {
				if !st.Passkeys {
					x.passkeys = 0
				}
				have[id] = x
			}
		}
		res.Close()
	}
	byID := map[int64]*store.User{}
	for _, u := range users {
		byID[u.ID] = u
	}
	for i := range rows {
		x := have[rows[i].ID]
		var label string
		switch {
		case x.totp && x.mfa:
			label = `<span class="adminsec-badge is-2fa">2FA</span> app + e-mail`
		case x.mfa:
			label = `<span class="adminsec-badge is-mfa">MFA</span> e-mail`
		case x.passkeys == 0:
			label = `<span aria-hidden="true">&#8212;</span><span class="screen-reader-text">Off</span>`
		}
		if x.passkeys > 0 {
			if label != "" {
				label += "<br>"
			}
			label += `<span class="adminsec-badge is-passkey">Passkey</span> ` + strconv.Itoa(x.passkeys)
		}
		if u := byID[rows[i].ID]; u != nil {
			if req := st.required(u); req != "" && !st.satisfies(req, &asFactors{EmailEnabled: x.mfa, TOTPEnabled: x.totp, Passkeys: x.passkeys}) {
				label += `<br><span class="adminsec-required">` + asRequiredLabel(req) + ` required</span>`
			}
		}
		rows[i].Security = template.HTML(label)
	}
	return true
}

// asSessionLogoutFailed records a session revocation that did not happen: the success entry is not
// written (the sessions are still live); the failure is logged and kept in the security log instead.
func (s *Server) asSessionLogoutFailed(c *gin.Context, target *store.User, err error) {
	slog.Error("adminsec: session logout failed", "user_id", target.ID, "err", err)
	s.asAudit(c, asEvent{Event: "sessions_logout_failed", Severity: "warning", Object: target.Login, Message: "Session logout failed for " + target.Login + "."})
}

// asRequiredLabel names a requirement in the Users list and notices.
func asRequiredLabel(req string) string {
	switch req {
	case "passkey":
		return "Passkey"
	case "2fa+passkey":
		return "2FA and passkey"
	}
	return strings.ToUpper(req)
}
