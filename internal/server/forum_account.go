package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"html"
	"html/template"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Forum membership: the login, register, verify and account views (GET) and their forms (POST
// forum_action=login|register|resend|account|password|google_unlink). Google sign-in is in
// forum_google.go, the confirmation mail and the rate limits in forum_mail.go.
//
// Logged-out forms carry the forumAuthNonce (the pages are no-store, so a logged-out nonce is safe
// there); the account forms carry the member's forumNonce. A form that fails is rendered again with
// its values and an error (422; 403 for an expired form, 429 when rate limited).

// forumErrorMessages are the ?forum_msg codes that report a refusal.
var forumErrorMessages = map[string]bool{"google_failed": true, "google_cancelled": true, "google_link_required": true,
	"google_blocked": true, "registration_closed": true, "login_blocked": true}

// forumMinPassword is the shortest password accepted while Pursae Security does not set the policy.
const forumMinPassword = 8

// forumRouteAccount serves the login, register, verify and account views; false answers 404.
func (s *Server) forumRouteAccount(c *gin.Context, rt forumRoute) bool {
	ctx := c.Request.Context()
	switch rt.View {
	case forumViewLogin:
		if u := currentUser(c); u != nil {
			c.Redirect(http.StatusFound, s.forumTarget(c, c.Query("redirect")))
			return true
		}
		p := s.forumAuthPage(c, forumViewLogin, c.Query("redirect"))
		s.forumRender(c, p, "login", "", 1)
		return true
	case forumViewRegister:
		if !s.forumLoad().Registration {
			return false
		}
		if currentUser(c) != nil {
			c.Redirect(http.StatusFound, s.forumTarget(c, c.Query("redirect")))
			return true
		}
		p := s.forumAuthPage(c, forumViewRegister, c.Query("redirect"))
		s.forumRender(c, p, "register", "", 1)
		return true
	case forumViewVerify:
		s.forumVerifyGet(c)
		return true
	case forumViewAccount:
		if currentUser(c) == nil {
			c.Redirect(http.StatusFound, s.forumURL(ctx, "login")+"?redirect="+url.QueryEscape(forumLocalPath(s.forumURL(ctx, "account"))))
			return true
		}
		s.forumRender(c, s.forumAccountPage(c), "account", "", 1)
		return true
	}
	return false
}

// forumAccountPost handles login, register, resend, account, password and google_unlink.
func (s *Server) forumAccountPost(c *gin.Context, rt forumRoute, action string) {
	c.Header("Cache-Control", "no-store")
	switch action {
	case "login":
		s.forumLoginPost(c)
	case "register":
		s.forumRegisterPost(c)
	case "resend":
		s.forumResendPost(c)
	case "account", "password", "google_unlink", "avatar_upload", "avatar_remove":
		s.forumAccountFormPost(c, action)
	case "change_email":
		s.forumChangeEmailPost(c) // v0.2.5 (forum_account_more.go)
	default:
		c.String(http.StatusBadRequest, "Unknown forum action.")
	}
}

// forumTarget is where a form sends the visitor afterwards: a local path from ?redirect / the form,
// else the forum index.
func (s *Server) forumTarget(c *gin.Context, raw string) string {
	index := forumLocalPath(s.forumURL(c.Request.Context()))
	if t := forumSafeLocal(raw); t != "" {
		return t
	}
	return index
}

// forumSafeLocal is raw when it is a local path (safeRedirect's rule), else "".
func forumSafeLocal(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2000 {
		return ""
	}
	out := safeRedirect(raw, "")
	// safeRedirect cleans the path, which drops the trailing slash every forum URL has.
	if p, _, _ := strings.Cut(raw, "?"); out != "" && strings.HasSuffix(p, "/") {
		if op, q, ok := strings.Cut(out, "?"); !strings.HasSuffix(op, "/") {
			out = op + "/"
			if ok {
				out += "?" + q
			}
		}
	}
	return out
}

// forumAuthPage is the dot of the login / register / verify views: the title and crumb, the redirect
// target kept in a hidden field, and the Google button's URL (which returns to the same target).
func (s *Server) forumAuthPage(c *gin.Context, view, redirect string) *forumPage {
	p := s.forumNewPage(c, view, "")
	switch view {
	case forumViewLogin:
		p.Title = p.T("Log in")
	case forumViewRegister:
		p.Title = p.T("Create an account")
	case forumViewVerify:
		p.Title = p.T("Confirm your email")
	}
	p.Crumbs = append(p.Crumbs, forumCrumb{Label: p.Title})
	if forumErrorMessages[c.Query("forum_msg")] && p.Notice != "" { // a refusal reads as an error, not a success
		p.Error, p.Notice = p.Notice, ""
	}
	redirect = forumSafeLocal(redirect)
	p.Form["redirect"] = redirect
	if redirect != "" {
		p.URLs.Login = s.forumURL(c.Request.Context(), "login") + "?redirect=" + url.QueryEscape(redirect)
		p.URLs.Register = s.forumURL(c.Request.Context(), "register") + "?redirect=" + url.QueryEscape(redirect)
	}
	if p.GoogleOn {
		back := redirect
		if back == "" {
			back = forumLocalPath(p.URLs.Index)
		}
		p.Extra["google"] = forumAuthPrefix + "/google?redirect=" + url.QueryEscape(back)
	}
	// v0.2.5: the rules checkbox, the community's totals ("2,140 members, 86 new topics this week")
	// and, while waiting for the email, a few popular topics to read.
	p.Extra["require_rules"] = s.forumLoad().RequireRules
	if st, err := s.forumStatsOf(c.Request.Context()); err == nil {
		p.Stats = st
	}
	if view == forumViewVerify {
		if list, _, err := s.forumTopics(c.Request.Context(), forumTopicQuery{Page: 1, PerPage: 3, Sort: "popular"}); err == nil {
			p.Topics = list
		}
	}
	// The form posts to the view's own URL (without the query); resend forms post to the verify view.
	p.Extra["action"] = s.forumURL(c.Request.Context(), view)
	p.Extra["verify"] = s.forumURL(c.Request.Context(), "verify")
	return p
}

// forumAccountFail renders a view again with an error and a status.
func (s *Server) forumAccountFail(c *gin.Context, p *forumPage, tmpl string, status int, msg string) {
	p.Error = p.T(msg)
	p.Extra["status"] = status
	s.forumRender(c, p, tmpl, "", 1)
}

// forumPlain turns a core HTML notice ("<strong>Error:</strong> …") into the plain text forum notices use.
func forumPlain(h string) string {
	t := strings.TrimSpace(html.UnescapeString(wp.StripTags(h)))
	t = strings.TrimSpace(strings.TrimPrefix(t, "Error:"))
	return t
}

// Login ----------------------------------------------------------------------------------------------

// forumLoginPost is the forum's login form, step for step the Commerce front login (gooLoginPost):
// the nonce, the CAPTCHA, the Pursae Security pre-checks or the core throttle counted before the
// password check, a timing-safe check for unknown users, and Pursae Security's hand-off (password
// policy, second factor) when it is on. One generic error: no account enumeration.
func (s *Server) forumLoginPost(c *gin.Context) {
	ctx := c.Request.Context()
	login, pass := strings.TrimSpace(c.PostForm("log")), c.PostForm("pwd")
	remember := c.PostForm("rememberme") != ""
	redirect := c.PostForm("redirect")
	target := s.forumTarget(c, redirect)
	var pressure string // the "account under attack" CAPTCHA widget, once asked
	fail := func(status int, msg string) {
		p := s.forumAuthPage(c, forumViewLogin, redirect)
		if pressure != "" && p.Captcha == "" {
			p.Captcha = template.HTML(pressure)
		}
		p.Form["log"] = login
		if remember {
			p.Form["rememberme"] = "1"
		}
		s.forumAccountFail(c, p, "login", status, msg)
	}
	if !s.checkNonceValue(c, c.PostForm("_wpnonce"), forumAuthNonce) {
		fail(http.StatusForbidden, "Your session has expired. Please reload the page and try again.")
		return
	}
	if login == "" || pass == "" {
		fail(http.StatusUnprocessableEntity, "Please enter your username or email and your password.")
		return
	}
	if len(login) > 200 || len(pass) > 4096 {
		fail(http.StatusUnprocessableEntity, "Wrong username or password.")
		return
	}
	if msg := s.loginCaptchaFailed(c); msg != "" {
		fail(http.StatusUnprocessableEntity, forumPlain(string(msg)))
		return
	}
	ip := clientIP(c)
	secOn := s.adminsecOn()
	if secOn {
		if msg := s.adminsecPreLogin(c, login); msg != "" {
			fail(http.StatusForbidden, forumPlain(string(msg)))
			return
		}
	}
	// An account under attack asks for the CAPTCHA here as on wp-login; a missing answer is not counted.
	if widget, msg := s.loginPressureCaptcha(c, login); widget != "" {
		pressure = string(widget)
		if msg != "" {
			fail(http.StatusUnprocessableEntity, forumPlain(string(msg)))
			return
		}
	}
	var attemptID int64
	attemptKey := login
	if secOn {
		id, msg := s.adminsecBeginAttempt(c, login)
		if msg != "" {
			fail(http.StatusTooManyRequests, forumPlain(string(msg)))
			return
		}
		attemptID = id
	} else {
		id, key, throttled := s.coreBeginAttempt(c, login)
		if throttled {
			fail(http.StatusTooManyRequests, "Too many failed login attempts. Please try again in 15 minutes.")
			return
		}
		attemptID, attemptKey = id, key
	}
	u, err := s.store.UserByLoginOrEmail(ctx, login)
	ok := false
	if err == nil {
		ok = s.checkUserPassword(ctx, u, pass)
	} else {
		auth.CheckDummyPassword(ctx, pass) // no timing difference for unknown users
	}
	if !ok {
		if secOn {
			var uu *store.User
			if err == nil {
				uu = u
			}
			s.adminsecLoginFailed(c, login, uu, "Wrong username or password.", attemptID)
		} else {
			s.auth.AttemptFailed(c, attemptID)
		}
		fail(http.StatusUnprocessableEntity, "Wrong username or password.") // one answer, known user or not
		return
	}
	if secOn { // Pursae Security: password policy, second factor, alerts; it answers the request
		s.auth.DropAttempt(c, attemptID)
		s.adminsecPasswordOK(c, loginView{Title: "Log In", Action: "login", SiteName: s.settings.String("blogname"), HomeURL: s.links(ctx).HomeURL("/"),
			RedirectTo: target, Login: login, Remember: remember}, u, pass)
		return
	}
	s.auth.AttemptSucceeded(c, attemptID, ip, attemptKey)
	if err := s.loginUserCookie(c, u.ID, remember); err != nil {
		fail(http.StatusInternalServerError, "Could not start a session. Please try again.")
		return
	}
	s.pluginUserAction(ctx, "wp_login", u)
	c.Redirect(http.StatusFound, target)
}

// Registration ---------------------------------------------------------------------------------------

// forumRegisterPost creates a member: gates (nonce, Registration, rate limit), spam traps and
// CAPTCHA, field validation, then the account with the forum's default role. With RequireVerify the
// member gets a confirmation link and is not logged in; otherwise they are logged in at once.
func (s *Server) forumRegisterPost(c *gin.Context) {
	ctx := c.Request.Context()
	st := s.forumLoad()
	if !st.Registration {
		s.forumNotFound(c)
		return
	}
	username := strings.TrimSpace(c.PostForm("user_login"))
	email := strings.TrimSpace(c.PostForm("user_email"))
	pass := c.PostForm("user_pass")
	redirect := c.PostForm("redirect")
	page := func() *forumPage {
		p := s.forumAuthPage(c, forumViewRegister, redirect)
		p.Form["user_login"], p.Form["user_email"] = username, email
		return p
	}
	if !s.checkNonceValue(c, c.PostForm("_wpnonce"), forumAuthNonce) {
		s.forumAccountFail(c, page(), "register", http.StatusForbidden, "Your session has expired. Please reload the page and try again.")
		return
	}
	network := gooikeTruncateIP(clientIP(c))
	if !forumAllow("register-try:"+network, 20, 10*time.Minute) {
		s.forumAccountFail(c, page(), "register", http.StatusTooManyRequests, "Too many sign-ups from your network. Please try again later.")
		return
	}
	if c.PostForm(commentTimeField) == "" || !s.commentTrapsPassed(c, 0) {
		s.forumAccountFail(c, page(), "register", http.StatusUnprocessableEntity, "Your sign-up could not be accepted. Please wait a moment and try again.")
		return
	}
	if msg := s.loginCaptchaFailed(c); msg != "" {
		s.forumAccountFail(c, page(), "register", http.StatusUnprocessableEntity, forumPlain(string(msg)))
		return
	}
	p := page()
	errs := map[string]string{}
	rules, digest := c.PostForm("rules") != "", c.PostForm("digest") != ""
	if rules {
		p.Form["rules"] = "1"
	}
	if digest {
		p.Form["digest"] = "1"
	}
	if st.RequireRules && !rules { // v0.2.5
		errs["rules"] = p.T("Please accept the community rules.")
	}
	if n := utf8.RuneCountInString(username); n < 3 || n > 60 || !validUsername.MatchString(username) {
		errs["user_login"] = p.T("Use 3 to 60 characters: letters, numbers, spaces and . _ - @")
	}
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email || len(email) > 100 {
		errs["user_email"] = p.T("Please enter a valid email address.")
	}
	if msg := s.forumPasswordProblem([]string{st.DefaultRole}, username, email, pass); msg != "" {
		errs["user_pass"] = p.T(msg)
	}
	if len(errs) == 0 {
		loginTaken, _, err := s.store.LoginOrEmailTaken(ctx, username, username)
		_, emailTaken, err2 := s.store.LoginOrEmailTaken(ctx, email, email)
		if err != nil || err2 != nil {
			s.forumAccountFail(c, p, "register", http.StatusInternalServerError, "Something went wrong. Please try again.")
			return
		}
		if loginTaken {
			errs["user_login"] = p.T("This username is taken. Please choose another one.")
		}
		if emailTaken {
			errs["user_email"] = p.T("An account with this email already exists. Log in instead.")
		}
	}
	if len(errs) > 0 {
		for k, v := range errs {
			p.Form["err_"+k] = v
		}
		s.forumAccountFail(c, p, "register", http.StatusUnprocessableEntity, "Please fix the fields marked below.")
		return
	}
	if !forumAllow("register:"+network, 5, time.Hour) { // new accounts per network
		s.forumAccountFail(c, p, "register", http.StatusTooManyRequests, "Too many sign-ups from your network. Please try again later.")
		return
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		s.forumAccountFail(c, p, "register", http.StatusInternalServerError, "Something went wrong. Please try again.")
		return
	}
	id, err := s.store.CreateUser(ctx, store.NewUser{Login: username, Email: email, PasswordHash: hash, DisplayName: username, Role: st.DefaultRole})
	if err != nil {
		msg := "Something went wrong. Please try again."
		if errors.Is(err, store.ErrLoginTaken) || errors.Is(err, store.ErrEmailTaken) {
			msg = "This username or email is already registered."
		}
		s.forumAccountFail(c, p, "register", http.StatusUnprocessableEntity, msg)
		return
	}
	u, err := s.store.UserByID(ctx, id)
	if err != nil {
		s.forumAccountFail(c, p, "register", http.StatusInternalServerError, "Something went wrong. Please try again.")
		return
	}
	s.pluginUserAction(ctx, "user_register", u)
	s.forumSaveSignupPrefs(ctx, u.ID, rules, digest) // v0.2.5: rules accepted, weekly digest
	if st.RequireVerify {
		if err := s.forumSendVerify(ctx, u); err != nil {
			s.forumAccountFail(c, p, "register", http.StatusInternalServerError, "Something went wrong. Please try again.")
			return
		}
		done := s.forumAuthPage(c, forumViewRegister, redirect)
		done.Notice = done.T(forumMessages["verify_sent"])
		done.Extra["sent"] = true
		done.Form["user_email"] = email
		done.Extra["change_token"] = s.forumChangeEmailToken(id, email, time.Now().Add(forumVerifyTTL)) // "change the address"
		s.forumRender(c, done, "register", "", 1)
		return
	}
	if err := s.loginUserCookie(c, id, false); err != nil {
		c.Redirect(http.StatusFound, s.forumURL(ctx, "login"))
		return
	}
	s.pluginUserAction(ctx, "wp_login", u)
	c.Redirect(http.StatusFound, s.forumTarget(c, redirect))
}

// forumPasswordProblem is why pw cannot be used: Pursae Security's policy when it is on, else the
// forum's minimum length.
func (s *Server) forumPasswordProblem(roles []string, login, email, pw string) string {
	if pw == "" {
		return "Please choose a password."
	}
	if len(pw) > 4096 {
		return "This password is too long."
	}
	if s.adminsecOn() {
		return s.adminsecPasswordProblem(roles, login, email, pw)
	}
	if utf8.RuneCountInString(pw) < forumMinPassword {
		return "Use at least 8 characters."
	}
	return ""
}

// Email confirmation ---------------------------------------------------------------------------------

// forumVerifyGet checks a confirmation link (?u=&k=). A valid one clears the unverified flag, logs
// the member in (not accounts with dashboard rights or a second factor: they log in themselves, and
// not when another account is logged in in this browser) and goes to the index; anything else shows
// the verify view with a form to get a new link.
//
// Mail link scanners open links before the member does, so HEAD never acts and a used key stays
// recognisable until it expires: a second open of the same link only says the email is confirmed
// (and sends the visitor to log in) instead of failing or logging in again.
func (s *Server) forumVerifyGet(c *gin.Context) {
	ctx := c.Request.Context()
	c.Header("Referrer-Policy", "no-referrer") // the key is in the URL
	p := s.forumAuthPage(c, forumViewVerify, "")
	uid, _ := strconv.ParseInt(c.Query("u"), 10, 64)
	key := c.Query("k")
	cu := currentUser(c)
	if cu != nil {
		p.Form["user_email"] = cu.Email
		if forumMetaBool(cu.Meta, forumUMetaUnverified) {
			p.Extra["change_email"] = true // v0.2.5: "change the address" (forum_action=change_email)
		}
	}
	if uid <= 0 && key == "" {
		if cu != nil && !forumMetaBool(cu.Meta, forumUMetaUnverified) {
			c.Redirect(http.StatusFound, s.forumURL(ctx, "account")) // nothing to confirm
			return
		}
		s.forumRender(c, p, "verify", "", 1)
		return
	}
	if c.Request.Method == http.MethodHead {
		s.forumRender(c, p, "verify", "", 1)
		return
	}
	index := s.forumURL(ctx) + "?forum_msg=verified"
	login := s.forumURL(ctx, "login") + "?forum_msg=verified"
	var u *store.User
	if uid > 0 {
		u, _ = s.store.UserByID(ctx, uid)
	}
	if u == nil || !forumCheckVerify(u, key, time.Now()) {
		if cu != nil && cu.ID == uid && !forumMetaBool(cu.Meta, forumUMetaUnverified) {
			c.Redirect(http.StatusFound, index) // already confirmed (an old link, or a newer one used)
			return
		}
		p.Extra["failed"] = true
		s.forumAccountFail(c, p, "verify", http.StatusUnprocessableEntity, "This confirmation link is invalid or has expired. Request a new one below.")
		return
	}
	vm, _ := u.Meta[forumUMetaVerify].(map[string]any)
	if forumMetaBool(vm, "used") { // a second open: confirmed already, never a second login
		if cu != nil && cu.ID == u.ID {
			c.Redirect(http.StatusFound, index)
		} else {
			c.Redirect(http.StatusFound, login)
		}
		return
	}
	vm["used"] = true
	if err := s.forumSetUserMeta(ctx, u.ID, map[string]any{forumUMetaVerify: vm, forumUMetaEmailVerified: strings.ToLower(u.Email)}, forumUMetaUnverified); err != nil {
		s.forumAccountFail(c, p, "verify", http.StatusInternalServerError, "Something went wrong. Please try again.")
		return
	}
	s.forumPurge(ctx)
	if cu != nil { // the member, or another account logged in here: confirm, never switch sessions
		c.Redirect(http.StatusFound, index)
		return
	}
	if u.Can("manage_options", nil) || u.Can("edit_posts", nil) || (s.adminsecOn() && s.asFactors(ctx, u.ID).any()) || s.forumSecRefused(c) {
		c.Redirect(http.StatusFound, login)
		return
	}
	if err := s.forumStartSession(c, u, false, "an email confirmation link"); err != nil {
		c.Redirect(http.StatusFound, login)
		return
	}
	s.pluginUserAction(ctx, "wp_login", u)
	c.Redirect(http.StatusFound, index)
}

// forumResendPost sends a new confirmation link: to the logged-in member, or to the typed address.
// The answer is always the same ("check your inbox"); a link is only sent when the account exists and
// is unverified, at most 3 times an hour per address and per network.
func (s *Server) forumResendPost(c *gin.Context) {
	ctx := c.Request.Context()
	cu := currentUser(c)
	back := s.forumURL(ctx, "verify")
	if cu != nil {
		back = s.forumURL(ctx, "account")
	}
	if !s.checkNonceValue(c, c.PostForm("_wpnonce"), forumAuthNonce) {
		p := s.forumAuthPage(c, forumViewVerify, "")
		s.forumAccountFail(c, p, "verify", http.StatusForbidden, "Your session has expired. Please reload the page and try again.")
		return
	}
	email := strings.TrimSpace(c.PostForm("user_email"))
	if cu != nil {
		email = cu.Email
	}
	if !forumAllow("resend-ip:"+gooikeTruncateIP(clientIP(c)), 3, time.Hour) {
		p := s.forumAuthPage(c, forumViewVerify, "")
		p.Form["user_email"] = email
		s.forumAccountFail(c, p, "verify", http.StatusTooManyRequests, "Too many requests. Please try again later.")
		return
	}
	if a, err := mail.ParseAddress(email); err == nil && a.Address == email && forumAllow("resend-mail:"+strings.ToLower(email), 3, time.Hour) {
		if u, err := s.store.UserByLoginOrEmail(ctx, email); err == nil && strings.EqualFold(u.Email, email) &&
			forumMetaBool(u.Meta, forumUMetaUnverified) {
			_ = s.forumSendVerify(ctx, u)
		}
	}
	c.Redirect(http.StatusFound, back+"?forum_msg=verify_sent")
}

// Account --------------------------------------------------------------------------------------------

// forumAccountPage is the account view's dot: the profile values, the Google link state and whether
// the password form asks for the current password.
func (s *Server) forumAccountPage(c *gin.Context) *forumPage {
	u := currentUser(c)
	p := s.forumNewPage(c, forumViewAccount, "")
	p.Title = p.T("Your account")
	p.Crumbs = append(p.Crumbs, forumCrumb{Label: p.Title})
	if forumErrorMessages[c.Query("forum_msg")] && p.Notice != "" {
		p.Error, p.Notice = p.Notice, ""
	}
	p.Extra["action"] = s.forumURL(c.Request.Context(), "account")
	if u == nil {
		return p
	}
	p.Form["display_name"] = u.DisplayName
	p.Form["bio"] = u.Profile["description"]
	p.Form["email"] = u.Email
	p.Form["login"] = u.Login
	if forumMetaBool(u.Meta, forumUMetaDigest) {
		p.Form["digest"] = "1"
	}
	if sub := forumMetaStr(u.Meta, forumUMetaGoogleSub); sub != "" {
		p.Extra["google_linked"] = true
		p.Form["google_email"] = forumMetaStr(u.Meta, forumUMetaGoogleEmail)
	}
	p.Extra["password_set"] = !forumNoPassword(u)
	p.Extra["need_current"] = forumNeedCurrent(u)
	p.Extra["profile_url"] = s.forumProfileURL(c.Request.Context(), u.Nicename)
	if p.User != nil {
		_ = s.forumGamifyFill(c.Request.Context(), p.User) // the level and progress of the photo card
	}
	return p
}

// forumAvatarErrors are the avatar errors whose text is shown to the member (translated).
var forumAvatarErrors = []error{errForumAvatarMissing, errForumAvatarType, errForumAvatarBytes, errForumAvatarDims,
	errForumAvatarSmall, errForumAvatarBroken}

// forumAvatarMessage is the member-facing English text of an avatar upload error and its status:
// 422 for a photo the member can fix, 500 otherwise.
func forumAvatarMessage(err error) (string, int) {
	for _, e := range forumAvatarErrors {
		if errors.Is(err, e) {
			return e.Error(), http.StatusUnprocessableEntity
		}
	}
	return "Your photo could not be saved. Please try again.", http.StatusInternalServerError
}

// Photo changes per member are bounded: each one decodes, resizes and stores an image in the Media
// Library and purges the whole page cache, and any registered member (unconfirmed or blocked too)
// may make them.
const (
	forumAvatarMax      = 10
	forumAvatarWindow   = time.Hour
	forumAvatarTooOften = "You are changing your photo too often. Please try again later."
)

var forumAvatarHits = struct {
	sync.Mutex
	m map[int64][]time.Time
}{m: map[int64][]time.Time{}}

// forumAvatarAllow counts a photo change of the member and reports whether it may go ahead (at most
// forumAvatarMax per forumAvatarWindow, in this process).
func forumAvatarAllow(userID int64, now time.Time) bool {
	h := &forumAvatarHits
	h.Lock()
	defer h.Unlock()
	if len(h.m) > 10000 { // forget members whose window has passed
		for id, ts := range h.m {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= forumAvatarWindow {
				delete(h.m, id)
			}
		}
	}
	kept := h.m[userID][:0]
	for _, t := range h.m[userID] {
		if now.Sub(t) < forumAvatarWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= forumAvatarMax {
		h.m[userID] = kept
		return false
	}
	h.m[userID] = append(kept, now)
	return true
}

// forumNeedCurrent reports whether changing the password asks for the current one: always, except
// for a member who joined with Google and never chose a password (they do not know the random one).
func forumNeedCurrent(u *store.User) bool {
	return !forumNoPassword(u)
}

// forumEmailProven reports whether the member proved they own the account's current address.
func forumEmailProven(u *store.User) bool {
	v := forumMetaStr(u.Meta, forumUMetaEmailVerified)
	return v != "" && strings.EqualFold(v, u.Email)
}

// forumSecRefused reports whether Pursae Security refuses a login from this request's IP (block
// list, IP lockout) on the routes that log in without a password: Google sign-in and the
// confirmation link. It logs the refusal as the password forms do.
func (s *Server) forumSecRefused(c *gin.Context) bool {
	return s.adminsecOn() && s.adminsecPreLogin(c, "") != ""
}

// forumStartSession logs u in on a route without a password and, while Pursae Security is on,
// records it like every other login (audit event, known IPs and the new-IP alert).
func (s *Server) forumStartSession(c *gin.Context, u *store.User, remember bool, method string) error {
	if err := s.loginUserCookie(c, u.ID, remember); err != nil {
		return err
	}
	if s.adminsecOn() {
		s.auth.RecordAttempt(c, clientIP(c), strings.ToLower(u.Login), true)
		s.asAudit(c, asEvent{Event: "login_success", User: u, Object: u.Login, Message: "Logged in with " + method + "."})
		s.asNoteLoginIP(c, u)
	}
	return nil
}

// forumPasswordMark fingerprints a password hash for forumUMetaNoPassword (never the hash itself).
func forumPasswordMark(hash string) string {
	sum := sha256.Sum256([]byte("forum-no-password:" + hash))
	return hex.EncodeToString(sum[:16])
}

// forumNoPassword reports whether the account still has the random password Google sign-in gave it.
// The mark names that exact hash, so a password set anywhere else (wp-admin, a reset link, the core
// profile) makes the account ask for its current password again.
func forumNoPassword(u *store.User) bool {
	m := forumMetaStr(u.Meta, forumUMetaNoPassword)
	return m != "" && u.PasswordHash != "" && subtle.ConstantTimeCompare([]byte(m), []byte(forumPasswordMark(u.PasswordHash))) == 1
}

// forumAccountFormPost saves the profile (account), the password (password), the profile photo
// (avatar_upload, avatar_remove) or disconnects Google (google_unlink; only when the member has a
// password to log in with).
func (s *Server) forumAccountFormPost(c *gin.Context, action string) {
	ctx := c.Request.Context()
	u := currentUser(c)
	if u == nil {
		c.Redirect(http.StatusFound, s.forumURL(ctx, "login")+"?redirect="+url.QueryEscape(forumLocalPath(s.forumURL(ctx, "account"))))
		return
	}
	account := s.forumURL(ctx, "account")
	if !s.checkNonceValue(c, c.PostForm("_wpnonce"), forumNonce) {
		s.forumAccountFail(c, s.forumAccountPage(c), "account", http.StatusForbidden, "Your session has expired. Please reload the page and try again.")
		return
	}
	switch action {
	case "account":
		name := strings.TrimSpace(wp.StripTags(c.PostForm("display_name")))
		bio := strings.TrimSpace(wp.StripTags(c.PostForm("bio")))
		fail := func(msg string) {
			p := s.forumAccountPage(c)
			p.Form["display_name"], p.Form["bio"] = name, bio
			s.forumAccountFail(c, p, "account", http.StatusUnprocessableEntity, msg)
		}
		if name == "" || utf8.RuneCountInString(name) > 60 {
			fail("Please enter a display name of up to 60 characters.")
			return
		}
		if utf8.RuneCountInString(bio) > 1000 {
			fail("Keep your bio under 1000 characters.")
			return
		}
		fresh, err := s.store.UserByID(ctx, u.ID)
		if err != nil {
			fail("Something went wrong. Please try again.")
			return
		}
		if fresh.Profile == nil {
			fresh.Profile = map[string]string{}
		}
		fresh.DisplayName = name
		fresh.Profile["description"] = bio
		if err := s.store.UpdateUserProfile(ctx, fresh); err != nil {
			fail("Something went wrong. Please try again.")
			return
		}
		if c.PostForm("digest_shown") != "" { // v0.2.5: only a form that shows the checkbox changes it
			_ = s.forumSetUserMeta(ctx, u.ID, map[string]any{forumUMetaDigest: c.PostForm("digest") != ""})
		}
		s.forumPurge(ctx) // names show on cached forum pages
		c.Redirect(http.StatusFound, account+"?forum_msg=saved")
	case "password":
		cur, pw := c.PostForm("current_pass"), c.PostForm("new_pass")
		fail := func(status int, msg string) {
			s.forumAccountFail(c, s.forumAccountPage(c), "account", status, msg)
		}
		if forumNeedCurrent(u) {
			// The same per-address budget as the login form: the current password is a guessable secret.
			id, key, throttled := s.coreBeginAttempt(c, u.Login)
			if throttled {
				fail(http.StatusTooManyRequests, "Too many failed attempts. Please try again in 15 minutes.")
				return
			}
			if cur == "" || !s.checkUserPassword(ctx, u, cur) {
				s.auth.AttemptFailed(c, id)
				fail(http.StatusUnprocessableEntity, "Your current password is not correct.")
				return
			}
			s.auth.AttemptSucceeded(c, id, clientIP(c), key)
		}
		if msg := s.forumPasswordProblem(u.Roles, u.Login, u.Email, pw); msg != "" {
			fail(http.StatusUnprocessableEntity, msg)
			return
		}
		hash, err := auth.HashPassword(pw)
		if err == nil {
			err = s.store.SetUserPassword(ctx, u.ID, hash)
		}
		if err == nil {
			err = s.forumSetUserMeta(ctx, u.ID, nil, forumUMetaNoPassword)
		}
		if err != nil {
			fail(http.StatusInternalServerError, "Something went wrong. Please try again.")
			return
		}
		s.adminsecPasswordChanged(c, u)
		s.endSessionsAfterPasswordChange(ctx, c, u.ID)
		c.Redirect(http.StatusFound, account+"?forum_msg=password_changed")
	case "avatar_upload":
		// Blocked and unconfirmed members may change their photo too: it is their account, not a post.
		fh, err := c.FormFile("avatar")
		if err == nil && !forumAvatarAllow(u.ID, time.Now()) {
			s.forumAccountFail(c, s.forumAccountPage(c), "account", http.StatusTooManyRequests, forumAvatarTooOften)
			return
		}
		if err != nil {
			s.forumAccountFail(c, s.forumAccountPage(c), "account", http.StatusUnprocessableEntity, errForumAvatarMissing.Error())
			return
		}
		if _, err := s.forumSaveAvatarUpload(ctx, u, fh); err != nil {
			msg, status := forumAvatarMessage(err)
			s.forumAccountFail(c, s.forumAccountPage(c), "account", status, msg)
			return
		}
		c.Redirect(http.StatusFound, account+"?forum_msg=avatar_saved")
	case "avatar_remove":
		if !forumAvatarAllow(u.ID, time.Now()) {
			s.forumAccountFail(c, s.forumAccountPage(c), "account", http.StatusTooManyRequests, forumAvatarTooOften)
			return
		}
		if err := s.forumRemoveAvatar(ctx, u); err != nil {
			s.forumAccountFail(c, s.forumAccountPage(c), "account", http.StatusInternalServerError, "Something went wrong. Please try again.")
			return
		}
		c.Redirect(http.StatusFound, account+"?forum_msg=avatar_removed")
	case "google_unlink":
		if forumMetaStr(u.Meta, forumUMetaGoogleSub) == "" {
			c.Redirect(http.StatusFound, account)
			return
		}
		if forumNoPassword(u) {
			s.forumAccountFail(c, s.forumAccountPage(c), "account", http.StatusUnprocessableEntity,
				"Choose a password first, so you can still log in without Google.")
			return
		}
		if err := s.forumSetUserMeta(ctx, u.ID, nil, forumUMetaGoogleSub, forumUMetaGoogleEmail); err != nil {
			s.forumAccountFail(c, s.forumAccountPage(c), "account", http.StatusInternalServerError, "Something went wrong. Please try again.")
			return
		}
		c.Redirect(http.StatusFound, account+"?forum_msg=google_unlinked")
	}
}

// registerForumAuth wires /forum-auth/google and its callback (404 while the forum is off or Google
// sign-in is not configured).
func (s *Server) registerForumAuth(r *gin.Engine) {
	g := r.Group(forumAuthPrefix, s.forumOnly(), s.forumGoogleOnly())
	g.GET("/google", s.forumGoogleStart)
	g.GET("/google/callback", s.forumGoogleCallback)
}
