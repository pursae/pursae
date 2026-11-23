package server

import (
	"errors"
	"html"
	"html/template"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

type loginView struct {
	Title, Action, SiteName, HomeURL string
	Error, Message                   template.HTML
	Login, RedirectTo                string
	Remember                         bool
	ResetKey                         string
	// CanRegister is get_option( 'users_can_register' ): the "Register" link and the registration form.
	CanRegister bool
	Email       string // user_email on the registration form
	Captcha     template.HTML
	// Passkey: Pursae Security offers "Log in with a passkey" (and the browser's passkey autofill).
	Passkey bool
}

// login handles wp-login.php: the form, the POST and ?action=logout.
func (s *Server) login(c *gin.Context) {
	if s.adminsecLoginGate(c) { // AdminSecGo: /wp-login is hidden behind the custom login URL
		return
	}
	action := c.Query("action")
	switch action {
	case "logout":
		s.logout(c)
		return
	case "postpass":
		s.postPass(c)
		return
	}
	v := loginView{
		Title:       "Log In",
		Action:      "login",
		SiteName:    s.settings.String("blogname"),
		HomeURL:     s.links(c).HomeURL("/"),
		RedirectTo:  c.Query("redirect_to"),
		CanRegister: s.settings.Bool("users_can_register"),
		Captcha:     s.loginCaptchaWidget(c),
	}
	v.Passkey = s.asPasskeysUsable(c)
	switch action {
	case "lostpassword", "retrievepassword":
		if e := c.Query("error"); e == "invalidkey" || e == "expiredkey" {
			v.Error = "Your password reset link appears to be invalid. Please request a new link below."
		}
		s.lostPassword(c, v)
		return
	case "rp", "resetpass":
		s.resetPassword(c, v)
		return
	case "register":
		s.register(c, v)
		return
	case "confirmaction":
		s.confirmAction(c, v)
		return
	case "adminsec_2fa": // AdminSecGo: the second-factor step of a login
		if s.adminsecOn() {
			s.adminsecChallengePost(c, v)
			return
		}
	case "passkey_options": // Pursae Security: the options of a passkey sign-in (JSON)
		if s.adminsecOn() {
			s.adminsecPasskeyOptions(c)
			return
		}
	case "passkey_login": // Pursae Security: a passkey sign-in
		if s.adminsecOn() {
			s.adminsecPasskeyLogin(c, v)
			return
		}
	}
	if c.Query("checkemail") == "confirm" {
		v.Message = "Check your email for the confirmation link, then visit the login page."
	}
	if c.Query("checkemail") == "registered" {
		v.Message = "Registration complete. Please check your email, then visit the login page."
	}
	if c.Query("registration") == "disabled" {
		v.Error = "<strong>Error:</strong> User registration is currently not allowed."
	}
	if c.Query("password") == "changed" {
		v.Message = "Your password has been reset. You can now log in."
	}
	if c.Query("loggedout") == "true" {
		v.Message = "You are now logged out."
	}
	if c.Request.Method != http.MethodPost {
		if u := currentUser(c); u != nil && c.Query("reauth") == "" {
			c.Redirect(http.StatusFound, safeRedirect(v.RedirectTo, "/wp-admin/"))
			return
		}
		s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
		return
	}

	login := strings.TrimSpace(c.PostForm("log"))
	pass := c.PostForm("pwd")
	v.Login = login
	v.RedirectTo = c.PostForm("redirect_to")
	v.Remember = c.PostForm("rememberme") == "forever"
	ip := clientIP(c)

	fail := func(msg string) {
		v.Error = template.HTML(msg)
		s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
	}
	if msg := s.loginCaptchaFailed(c); msg != "" {
		fail(string(msg))
		return
	}
	secOn := s.adminsecOn()
	if secOn { // AdminSecGo: block list and lockouts replace the core throttle
		if msg := s.adminsecPreLogin(c, login); msg != "" {
			fail(string(msg))
			return
		}
	}
	switch {
	case login == "" && pass == "":
		fail("<strong>Error:</strong> The username field is empty.<br /><strong>Error:</strong> The password field is empty.")
		return
	case login == "":
		fail("<strong>Error:</strong> The username field is empty.")
		return
	case pass == "":
		fail("<strong>Error:</strong> The password field is empty.")
		return
	}
	// Core throttle: the attempt is recorded (as a failure) before the password is checked, so parallel
	// guesses cannot all slip past the count; see auth.BeginAttempt.
	var attemptID int64
	attemptKey := login
	if secOn { // Pursae Security: the same count-first rule against its own limits
		id, msg := s.adminsecBeginAttempt(c, login)
		if msg != "" {
			fail(string(msg))
			return
		}
		attemptID = id
	} else {
		id, key, throttled := s.coreBeginAttempt(c, login)
		if throttled {
			fail("<strong>Error:</strong> Too many failed login attempts. Please try again in 15 minutes.")
			return
		}
		attemptID, attemptKey = id, key
	}

	u, err := s.store.UserByLoginOrEmail(c, login)
	if errors.Is(err, store.ErrNotFound) {
		auth.CheckDummyPassword(c.Request.Context(), pass) // same argon2 work as a wrong password: no timing difference
		if secOn {
			s.adminsecLoginFailed(c, login, nil, "Unknown username or e-mail address.", attemptID)
		} else {
			s.auth.AttemptFailed(c, attemptID)
		}
		if msg, ok := s.adminsecGenericError(); ok {
			fail(string(msg))
		} else if strings.Contains(login, "@") {
			fail("<strong>Error:</strong> Unknown email address. Check again or try your username.")
		} else {
			fail("<strong>Error:</strong> The username <strong>" + html.EscapeString(login) + "</strong> is not registered on this site. If you are unsure of your username, try your email address instead.")
		}
		return
	}
	if err != nil {
		fail("<strong>Error:</strong> Could not log in right now. Please try again.")
		return
	}
	ok := s.checkUserPassword(c.Request.Context(), u, pass)
	if !ok {
		if secOn {
			s.adminsecLoginFailed(c, login, u, "Wrong password.", attemptID)
		} else {
			s.auth.AttemptFailed(c, attemptID)
		}
		if msg, ok := s.adminsecGenericError(); ok {
			fail(string(msg))
			return
		}
		label := "the username <strong>" + html.EscapeString(login) + "</strong>"
		if strings.Contains(login, "@") {
			label = "the email address <strong>" + html.EscapeString(login) + "</strong>"
		}
		fail("<strong>Error:</strong> The password you entered for " + label + " is incorrect. <a href=\"" + s.loginPath() + "?action=lostpassword\">Lost your password?</a>")
		return
	}
	if secOn { // AdminSecGo: password policy, second factor, alerts and the activity log
		s.auth.DropAttempt(c, attemptID) // not a failure; the finished sign-in records its success
		s.adminsecPasswordOK(c, v, u, pass)
		return
	}
	s.auth.AttemptSucceeded(c, attemptID, ip, attemptKey)

	sess, err := s.auth.Create(c, u.ID, v.Remember, ip, c.Request.UserAgent())
	if err != nil {
		fail("<strong>Error:</strong> Could not start a session. Please try again.")
		return
	}
	s.setLoginCookie(c, sess.Token, v.Remember, int(auth.SessionRemember.Seconds()))
	s.pluginUserAction(c, "wp_login", u)

	target := safeRedirect(v.RedirectTo, "/wp-admin/")
	// Users without dashboard capabilities land on their profile, like WordPress.
	if (target == "/wp-admin/" || target == "/wp-admin") && !u.Can("edit_posts", nil) {
		target = "/wp-admin/profile"
	}
	c.Redirect(http.StatusFound, target)
}

func (s *Server) setLoginCookie(c *gin.Context, token string, persistent bool, maxAge int) {
	// An https site always marks the session Secure, also when the TLS proxy in front of it is not among
	// the trusted proxies (then the request itself looks like plain http and the cookie would travel in clear).
	secure := s.requestHTTPS(c.Request) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.settings.String("home"))), "https://")
	cookie := &http.Cookie{
		Name:     auth.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
	if persistent {
		cookie.MaxAge = maxAge
	}
	http.SetCookie(c.Writer, cookie)
}

func (s *Server) logout(c *gin.Context) {
	if currentUser(c) != nil && !s.checkNonce(c, "log-out") {
		s.adminHTML(c, http.StatusForbidden, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Pursae &rsaquo; Error</title>`+
			`<link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head><body id="error-page">`+
			`<div class="wp-die-message"><p>You are attempting to log out of `+html.EscapeString(s.settings.String("blogname"))+`</p>`+
			`<p>Do you really want to <a href="`+s.loginPath()+`?action=logout&amp;_wpnonce=`+s.nonce(c, "log-out")+`">log out</a>?</p></div></body></html>`)
		return
	}
	if sess := currentSession(c); sess != nil {
		s.adminsecLogout(c)
		_ = s.auth.Destroy(c, sess.Token)
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	// wp-login.php?action=logout honours a local redirect_to (Pursae Forum's Log out link goes back to the forum).
	c.Redirect(http.StatusFound, safeRedirect(c.Query("redirect_to"), s.loginPath()+"?loggedout=true"))
}

// coreBeginAttempt is auth.BeginAttempt keyed by the account (its lowercased login) when the typed
// name or e-mail address resolves to one, so the username and the e-mail share one per-login budget.
// A storage error refuses the attempt instead of letting it through unthrottled.
func (s *Server) coreBeginAttempt(c *gin.Context, typed string) (id int64, key string, throttled bool) {
	u, _ := s.store.UserByLoginOrEmail(c, typed)
	key = s.asNormLogin(c, typed, u)
	id, throttled, err := s.auth.BeginAttempt(c, clientIP(c), key)
	if err != nil {
		log.Printf("wpgo: login throttle: %v; attempt refused", err)
		s.auth.DropAttempt(c, id)
		return 0, key, true
	}
	return id, key, throttled
}
