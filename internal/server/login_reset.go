package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/mail"
	"wpgo/internal/store"
)

// lostPassword is wp-login.php?action=lostpassword (retrieve_password()).
func (s *Server) lostPassword(c *gin.Context, v loginView) {
	v.Title, v.Action = "Lost Password", "lostpassword"
	if c.Request.Method == http.MethodPost {
		login := strings.TrimSpace(c.PostForm("user_login"))
		if login == "" {
			v.Error = "<strong>Error:</strong> Please enter a username or email address."
		} else {
			if msg := s.loginCaptchaFailed(c); msg != "" {
				v.Error = msg
				s.renderAdminTemplate(c, http.StatusOK, "admin/login-lost", v)
				return
			}
			// The same answer, after the same time, whether or not the account exists, so the form can't be
			// used to find accounts: the lookup, the throttle, the key and the mail run beside the request
			// (lostPasswordRequest), and the answer waits for resetResponseTime whatever they do.
			start := time.Now()
			s.goResetRequest(c.Copy(), login)
			wait := time.NewTimer(resetResponseTime - time.Since(start))
			select {
			case <-wait.C:
			case <-c.Request.Context().Done():
			}
			wait.Stop()
			c.Redirect(http.StatusFound, s.loginPath()+"?checkemail=confirm")
			return
		}
	}
	s.renderAdminTemplate(c, http.StatusOK, "admin/login-lost", v)
}

// resetResponseTime is how long every lost-password request takes before it is answered. The work of an
// existing account (throttle, activity log, key, mail: a few milliseconds of database writes) normally
// ends well within it, so existing and unknown accounts answer after the same time.
const resetResponseTime = 250 * time.Millisecond

// goResetRequest runs lostPasswordRequest on its own goroutine, owned by the Server's background
// machinery when there is one (Close waits for it). c is a copy of the request's context (gin.Context.Copy),
// so it stays usable after the request was answered.
func (s *Server) goResetRequest(c *gin.Context, login string) {
	s.goResetRequestVia(c, login, "")
}

// goResetRequestVia is goResetRequest whose mailed link opens at via (the My Account lost-password
// endpoint) instead of wp-login.php; "" keeps wp-login.
func (s *Server) goResetRequestVia(c *gin.Context, login, via string) {
	run := func(context.Context) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("lost password request panicked", "panic", r)
			}
		}()
		s.lostPasswordRequest(c, login, via)
	}
	if s.rt.bgCtx != nil {
		s.goBackground(run)
		return
	}
	safeGo("lost-password", func() { run(context.Background()) })
}

// lostPasswordRequest is the work behind a lost-password form: when login names an account and the
// request is within the reset throttle, log it and mail the account a reset link.
func (s *Server) lostPasswordRequest(c *gin.Context, login, via string) {
	u, err := s.store.UserByLoginOrEmail(c, login)
	if err != nil || !s.resetRequestAllowed(c, u) {
		return
	}
	s.asAudit(c, asEvent{Event: "password_reset_requested", Severity: "notice", User: u, Username: u.Login, Message: "A password reset link was requested."})
	s.sendResetLinkVia(c, u, via)
}

// resetLink is get_password_reset_key() + the wp-login.php?action=rp URL: a fresh key (hashed in user
// meta, valid for a day) and the link that carries it.
func (s *Server) resetLink(c *gin.Context, u *store.User) string {
	return s.resetLinkVia(c, u, "")
}

// resetLinkVia is resetLink opening at via (an absolute URL built from the configured home, never the
// request Host); "" is wp-login.php?action=rp.
func (s *Server) resetLinkVia(c *gin.Context, u *store.User, via string) string {
	key := auth.RandomToken(20)
	sum := sha256.Sum256([]byte(key))
	_ = s.setUserMeta(c, u, "password_reset", map[string]any{"hash": hex.EncodeToString(sum[:]), "expires": time.Now().Add(24 * time.Hour).Unix()})
	if via != "" {
		sep := "?"
		if strings.Contains(via, "?") {
			sep = "&"
		}
		return via + sep + "key=" + key + "&login=" + url.QueryEscape(u.Login)
	}
	return s.configuredLinks(c).HomeURL(s.loginPath() + "?action=rp&key=" + key + "&login=" + url.QueryEscape(u.Login))
}

// sendResetLink is retrieve_password(): the "[Site] Password Reset" message.
func (s *Server) sendResetLink(c *gin.Context, u *store.User) {
	s.sendResetLinkVia(c, u, "")
}

// sendResetLinkVia is sendResetLink with the link opening at via (see resetLinkVia).
func (s *Server) sendResetLinkVia(c *gin.Context, u *store.User, via string) {
	link := s.resetLinkVia(c, u, via)
	// A store customer gets the store's own "Reset password" e-mail (Settings > Emails) when it is
	// enabled; every other account, and every site without the store, gets the core message.
	if s.gooOn() && containsStr(u.Roles, "customer") {
		if sent, err := s.gooSendEmail(c, "customer_reset_password", nil, map[string]any{"user_id": u.ID, "login": u.Login, "to": u.Email, "reset_url": link}); err == nil && sent {
			return
		}
	}
	site := s.settings.String("blogname")
	body := fmt.Sprintf("Someone has requested a password reset for the following account:\n\nSite Name: %s\n\nUsername: %s\n\nIf this was a mistake, ignore this email and nothing will happen.\n\nTo reset your password, visit the following address:\n\n%s\n\n",
		site, u.Login, link)
	if currentUser(c) == nil {
		body += fmt.Sprintf("This password reset request originated from the IP address %s.\n", clientIP(c))
	}
	s.sendMail(c, mail.Message{Kind: "password_reset", To: []string{u.Email}, Subject: "[" + site + "] Password Reset", Body: body})
}

// wpRegisterPerHour is how many accounts wp-login.php?action=register creates per client network an hour.
const wpRegisterPerHour = 5

// register is wp-login.php?action=register (register_new_user()).
func (s *Server) register(c *gin.Context, v loginView) {
	if !v.CanRegister {
		c.Redirect(http.StatusFound, s.loginPath()+"?registration=disabled")
		return
	}
	v.Title, v.Action = "Registration Form", "register"
	v.Message = `<span class="message register">Register For This Site</span>`
	if c.Request.Method == http.MethodPost {
		login := strings.TrimSpace(c.PostForm("user_login"))
		email := strings.TrimSpace(c.PostForm("user_email"))
		v.Login, v.Email, v.RedirectTo = login, email, c.PostForm("redirect_to")
		var errs []string
		if msg := s.loginCaptchaFailed(c); msg != "" {
			errs = append(errs, string(msg))
		}
		lt, et, _ := s.store.LoginOrEmailTaken(c, login, email)
		switch {
		case login == "":
			errs = append(errs, "<strong>Error:</strong> Please enter a username.")
		case !validUsername.MatchString(login):
			errs = append(errs, "<strong>Error:</strong> This username is invalid because it uses illegal characters. Please enter a valid username.")
		case lt:
			errs = append(errs, "<strong>Error:</strong> This username is already registered. Please choose another one.")
		}
		switch {
		case email == "":
			errs = append(errs, "<strong>Error:</strong> Please type your email address.")
		case !validEmail(email):
			errs = append(errs, "<strong>Error:</strong> The email address isn&#8217;t correct.")
		case et:
			errs = append(errs, "<strong>Error:</strong> This email address is already registered. <a href=\""+s.loginPath()+"\">Log in</a> with this address or choose another one.")
		}
		// A per-network cap on new accounts (like the Forum and Headless sign-ups): only a sign-up that would
		// create an account counts, so typos in the form never use it up.
		if len(errs) == 0 && !forumAllow("wp-register:"+gooikeTruncateIP(clientIP(c)), wpRegisterPerHour, time.Hour) {
			errs = append(errs, "<strong>Error:</strong> Too many registrations from your network. Please try again later.")
		}
		if len(errs) == 0 {
			// The password is random; the user sets their own from the link in the notification.
			hash, err := auth.HashPassword(auth.RandomToken(24))
			var id int64
			if err == nil {
				id, err = s.store.CreateUser(c, store.NewUser{Login: login, Email: email, PasswordHash: hash, Role: s.settings.String("default_role")})
			}
			if err != nil {
				errs = append(errs, "<strong>Error:</strong> Could not register you&hellip; please contact the site administrator!")
			} else {
				if nu, e := s.store.UserByID(c, id); e == nil {
					s.pluginUserAction(c, "user_register", nu)
					s.notifyNewUser(c, nu, true)
				}
				target := s.loginPath() + "?checkemail=registered"
				if rt := safeRedirect(v.RedirectTo, ""); rt != "" {
					target += "&redirect_to=" + url.QueryEscape(rt)
				}
				c.Redirect(http.StatusFound, target)
				return
			}
		}
		v.Error = template.HTML(strings.Join(errs, "<br />\n"))
	}
	s.renderAdminTemplate(c, http.StatusOK, "admin/login-register", v)
}

// validEmail is is_email(): one @, a dot in the domain, no spaces.
func validEmail(email string) bool {
	at := strings.LastIndex(email, "@")
	if at < 1 || at == len(email)-1 || strings.ContainsAny(email, " \t\r\n") || strings.Count(email, "@") != 1 {
		return false
	}
	return strings.Contains(email[at+1:], ".")
}

// checkResetKey is check_password_reset_key().
func (s *Server) checkResetKey(ctx context.Context, login, key string) (*store.User, bool) {
	u, err := s.store.UserByLoginOrEmail(ctx, login)
	if err != nil || key == "" {
		return nil, false
	}
	rec, _ := u.Meta["password_reset"].(map[string]any)
	hash, _ := rec["hash"].(string)
	exp, _ := rec["expires"].(float64)
	sum := sha256.Sum256([]byte(key))
	if hash == "" || time.Now().Unix() > int64(exp) || subtle.ConstantTimeCompare([]byte(hash), []byte(hex.EncodeToString(sum[:]))) != 1 {
		return nil, false
	}
	return u, true
}

// resetPassword is wp-login.php?action=rp / resetpass.
func (s *Server) resetPassword(c *gin.Context, v loginView) {
	v.Title, v.Action = "Reset Password", "resetpass"
	login, key := defaultStr(c.PostForm("rp_login"), c.Query("login")), defaultStr(c.PostForm("rp_key"), c.Query("key"))
	u, ok := s.checkResetKey(c, login, key)
	if !ok {
		c.Redirect(http.StatusFound, s.loginPath()+"?action=lostpassword&error=invalidkey")
		return
	}
	v.Login, v.ResetKey = u.Login, key
	if c.Request.Method == http.MethodPost {
		msg, invalid := s.applyPasswordReset(c, u, key, c.PostForm("pass1"), c.PostForm("pass2"))
		switch {
		case invalid:
			c.Redirect(http.StatusFound, s.loginPath()+"?action=lostpassword&error=invalidkey")
			return
		case msg == "":
			c.Redirect(http.StatusFound, s.loginPath()+"?password=changed")
			return
		}
		v.Error = template.HTML("<strong>Error:</strong> " + msg)
	}
	s.renderAdminTemplate(c, http.StatusOK, "admin/login-reset", v)
}

// applyPasswordReset sets u's new password from a reset form (wp-login.php?action=rp and the My Account
// lost-password endpoint share it): the checks, the password policy, the key spent atomically, the sessions
// and credentials revoked and the admin told. msg is an HTML-safe error ("" on success); invalid means the
// key was already used.
func (s *Server) applyPasswordReset(c *gin.Context, u *store.User, key, p1, p2 string) (msg string, invalid bool) {
	switch {
	case p1 == "":
		return "Please enter a new password.", false
	case p1 != p2:
		return "The passwords do not match.", false
	case s.adminsecUserPasswordProblem(u, p1) != "": // AdminSecGo password policy
		return template.HTMLEscapeString(s.adminsecUserPasswordProblem(u, p1)), false
	}
	hash, err := auth.HashPassword(p1)
	if err != nil {
		return template.HTMLEscapeString(err.Error()), false
	}
	// Spend the key first and atomically: two requests with the same key can't both reset.
	sum := sha256.Sum256([]byte(key))
	if ok, err := s.store.ConsumePasswordReset(c, u.ID, hex.EncodeToString(sum[:])); err != nil || !ok {
		return "", true
	}
	if err := s.store.SetUserPassword(c, u.ID, hash); err != nil {
		return template.HTMLEscapeString(err.Error()), false
	}
	delete(u.Meta, "password_reset")
	s.revokeCredentialsAfterPasswordChange(c, u.ID)
	s.adminsecPasswordChanged(c, u)
	// The password is already changed, so the reset still succeeds; sessions left alive after
	// a reset must at least be visible in the log.
	if _, err := s.store.Pool.Exec(c, `DELETE FROM sessions WHERE user_id = $1`, u.ID); err != nil {
		slog.Error("login: session revocation after password reset failed", "user_id", u.ID, "err", err)
	}
	site := s.settings.String("blogname")
	s.sendMail(c, mail.Message{Kind: "password_changed", To: []string{s.settings.String("admin_email")}, Subject: "[" + site + "] Password Changed",
		Body: fmt.Sprintf("Password changed for user: %s\n", u.Login)})
	return "", false
}

// notifyComment is wp_new_comment_notify_moderator() / wp_new_comment_notify_postauthor().
func (s *Server) notifyComment(c *gin.Context, cm *store.Comment) {
	p, err := s.store.GetPost(c, cm.PostID)
	if err != nil {
		return
	}
	site := s.settings.String("blogname")
	link := s.links(c).Permalink(p)
	text := fmt.Sprintf("Author: %s (IP address: %s)\nEmail: %s\nURL: %s\nComment:\n%s\n", cm.AuthorName, cm.AuthorIP, cm.AuthorEmail, cm.AuthorURL, cm.Content)
	admin := s.links(c).HomeURL("/wp-admin/edit-comments?comment_status=")
	switch cm.Status {
	case "pending":
		if !s.settings.Bool("moderation_notify") || !s.notifyMailAllowed("comment", cm.AuthorIP) {
			return
		}
		body := fmt.Sprintf("A new comment on the post \"%s\" is waiting for your approval\n%s\n\n%s\nApprove it: %s\nTrash it: %s\nSpam it: %s\n\nCurrently 1 comment is waiting for approval. Please visit the moderation panel:\n%s\n",
			p.Title, link, text, admin+"pending", admin+"pending", admin+"pending", admin+"moderated")
		s.sendMail(c, mail.Message{Kind: "comment_moderation", To: []string{s.settings.String("admin_email")}, Subject: fmt.Sprintf("[%s] Please moderate: \"%s\"", site, p.Title), Body: body})
	case "approved":
		if !s.settings.Bool("comments_notify") || cm.UserID == p.AuthorID || !s.notifyMailAllowed("comment", cm.AuthorIP) {
			return
		}
		author, err := s.store.UserByID(c, p.AuthorID)
		if err != nil || author.Email == "" {
			return
		}
		body := fmt.Sprintf("New comment on your post \"%s\"\n%s\nYou can see all comments on this post here:\n%s#comments\n\nPermalink: %s#comment-%d\n", p.Title, text, link, link, cm.ID)
		s.sendMail(c, mail.Message{Kind: "comment_published", To: []string{author.Email}, Subject: fmt.Sprintf("[%s] Comment: \"%s\"", site, p.Title), Body: body, ReplyTo: cm.AuthorEmail})
	}
}

// notifyNewUser is wp_new_user_notification(): the admin is told, and the user gets "[Site] Login Details"
// with a link to set a password.
func (s *Server) notifyNewUser(c *gin.Context, u *store.User, sendUser bool) {
	site := s.settings.String("blogname")
	s.sendMail(c, mail.Message{Kind: "new_user_admin", To: []string{s.settings.String("admin_email")}, Subject: "[" + site + "] New User Registration",
		Body: fmt.Sprintf("New user registration on your site %s:\n\nUsername: %s\n\nEmail: %s\n", site, u.Login, u.Email)})
	if sendUser {
		link := s.resetLink(c, u)
		body := fmt.Sprintf("Username: %s\n\nTo set your password, visit the following address:\n\n%s\n\n%s\n", u.Login, link, s.configuredLinks(c).HomeURL(s.loginPath()))
		s.sendMail(c, mail.Message{Kind: "new_user_user", To: []string{u.Email}, Subject: "[" + site + "] Login Details", Body: body})
	}
}

// Password reset requests are limited per client network (auth.ClientNet: an IPv6 /64 counts as one)
// and per account so the form cannot be used to flood a mailbox (every request mails the owner and
// replaces the previous key). The answer shown to the visitor stays the same whether or not a request
// was throttled or the account exists.
const (
	resetRequestMax    = 3
	resetRequestWindow = 15 * time.Minute
)

// resetRequestAllowed reports whether a reset mail may be sent for u from this request, and records it.
func (s *Server) resetRequestAllowed(c *gin.Context, u *store.User) bool {
	return s.resetRequestAllowedFor(c, clientIP(c), u.Login)
}

// resetRequestAllowedFor is resetRequestAllowed for any reset form (wp-login, My Account): the
// counts live in the database (login_attempts, auth.AllowResetRequest), so they hold across restarts
// and processes. A database error refuses: the visitor sees the same answer either way.
func (s *Server) resetRequestAllowedFor(ctx context.Context, ip, login string) bool {
	if st := s.asLoad(); s.adminsecOn() && st.BlockList != "" && maintIPAllowed(st.BlockList, ip) && !maintIPAllowed(st.AllowList, ip) {
		return false
	}
	ok, err := s.auth.AllowResetRequest(ctx, ip, login, resetRequestMax, resetRequestWindow)
	return err == nil && ok
}
