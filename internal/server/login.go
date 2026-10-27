package server

import (
	"errors"
	"html"
	"html/template"
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
}

// login handles wp-login.php: the form, the POST and ?action=logout.
func (s *Server) login(c *gin.Context) {
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
	}
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
	if throttled, _ := s.auth.Throttled(c, ip); throttled {
		fail("<strong>Error:</strong> Too many failed login attempts. Please try again in 15 minutes.")
		return
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

	u, err := s.store.UserByLoginOrEmail(c, login)
	if errors.Is(err, store.ErrNotFound) {
		s.auth.RecordAttempt(c, ip, login, false)
		if strings.Contains(login, "@") {
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
	ok, _ := auth.CheckPassword(pass, u.PasswordHash)
	if !ok {
		s.auth.RecordAttempt(c, ip, login, false)
		label := "the username <strong>" + html.EscapeString(login) + "</strong>"
		if strings.Contains(login, "@") {
			label = "the email address <strong>" + html.EscapeString(login) + "</strong>"
		}
		fail("<strong>Error:</strong> The password you entered for " + label + " is incorrect. <a href=\"/wp-login?action=lostpassword\">Lost your password?</a>")
		return
	}
	s.auth.RecordAttempt(c, ip, login, true)

	sess, err := s.auth.Create(c, u.ID, v.Remember, ip, c.Request.UserAgent())
	if err != nil {
		fail("<strong>Error:</strong> Could not start a session. Please try again.")
		return
	}
	s.setLoginCookie(c, sess.Token, v.Remember, int(auth.SessionRemember.Seconds()))

	target := safeRedirect(v.RedirectTo, "/wp-admin/")
	// Users without dashboard capabilities land on their profile, like WordPress.
	if (target == "/wp-admin/" || target == "/wp-admin") && !u.Can("edit_posts", nil) {
		target = "/wp-admin/profile"
	}
	c.Redirect(http.StatusFound, target)
}

func (s *Server) setLoginCookie(c *gin.Context, token string, persistent bool, maxAge int) {
	secure := c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https"
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
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusForbidden, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>WordPress &rsaquo; Error</title>`+
			`<link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head><body id="error-page">`+
			`<div class="wp-die-message"><p>You are attempting to log out of `+html.EscapeString(s.settings.String("blogname"))+`</p>`+
			`<p>Do you really want to <a href="/wp-login?action=logout&amp;_wpnonce=`+s.nonce(c, "log-out")+`">log out</a>?</p></div></body></html>`)
		return
	}
	if sess := currentSession(c); sess != nil {
		_ = s.auth.Destroy(c, sess.Token)
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	c.Redirect(http.StatusFound, "/wp-login?loggedout=true")
}
