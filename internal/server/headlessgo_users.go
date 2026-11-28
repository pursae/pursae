package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

// Front-end users of the delivery API. A WordPress account signs in with
// POST /headlessgo/v1/auth/local {"identifier","password"} and gets a short-lived JWT (HS256,
// lifetime from the settings); the front end sends it as "Authorization: Bearer <jwt>". The caller
// is then a "user": it may do what the public role and the Authenticated matrix allow, and
// OwnerOnly restricts update/delete (and the inverse-relation edits they cause) to entries the user
// created (created_by). A user token never carries the account's WordPress capabilities, so an
// editor's or administrator's password gives no more here than a subscriber's. The login goes
// through the same throttle as wp-login; accounts with a second factor (Pursae Security) are
// refused because this form of sign-in cannot ask for it. Changing the account's password
// invalidates its tokens. POST /auth/local/register (when allowed) creates a account with the
// site's default role. GET /users/me answers who the token belongs to.

const (
	hgUserKind   = "user"
	hgUserIssuer = "headlessgo"
)

// hgUserKey signs front-end user tokens with the site secret (PURSAE_SECRET, else the generated
// auth_secret option): nil when there is none, which hgJWTVerify refuses.
func (s *Server) hgUserKey() []byte {
	secret := s.siteSecret()
	if secret == "" {
		return nil
	}
	return []byte(secret + "|headlessgo-user-jwt")
}

// hgPwTag ties a token to the password it was issued for.
func hgPwTag(hash string) string {
	sum := sha256.Sum256([]byte(hash))
	return hex.EncodeToString(sum[:6])
}

func hgJWTSign(key []byte, claims map[string]any) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	msg := head + "." + base64.RawURLEncoding.EncodeToString(body)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// hgJWTVerify checks the signature and the algorithm and returns the claims; expiry is the caller's.
func hgJWTVerify(key []byte, tok string) (map[string]any, bool) {
	parts := strings.Split(tok, ".")
	if len(key) == 0 || len(parts) != 3 {
		return nil, false
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	var head struct{ Alg, Typ string }
	if json.Unmarshal(hb, &head) != nil || head.Alg != "HS256" {
		return nil, false
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, m.Sum(nil)) {
		return nil, false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if json.Unmarshal(body, &claims) != nil {
		return nil, false
	}
	return claims, true
}

// hgIssueUserToken returns a token for u and its lifetime in seconds.
func (s *Server) hgIssueUserToken(st hgSettings, u *store.User) (string, int) {
	ttl := st.UserTokenTTL * 60
	now := time.Now().Unix()
	return hgJWTSign(s.hgUserKey(), map[string]any{"iss": hgUserIssuer, "sub": strconv.FormatInt(u.ID, 10), "iat": now, "exp": now + int64(ttl), "pw": hgPwTag(u.PasswordHash)}), ttl
}

// hgUserFromToken resolves a user token (nil when invalid, expired, revoked or the feature is off).
func (s *Server) hgUserFromToken(c *gin.Context, st hgSettings, tok string) *store.User {
	if !st.UserAuth {
		return nil
	}
	claims, ok := hgJWTVerify(s.hgUserKey(), tok)
	if !ok || claims["iss"] != hgUserIssuer {
		return nil
	}
	exp, _ := claims["exp"].(float64)
	if exp == 0 || time.Now().Unix() >= int64(exp) {
		return nil
	}
	sub, _ := claims["sub"].(string)
	id, err := strconv.ParseInt(sub, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	u, err := s.store.UserByID(c, id)
	if err != nil || u == nil || claims["pw"] != hgPwTag(u.PasswordHash) {
		return nil
	}
	return u
}

func (a *hgAuth) userID() int64 {
	if a != nil && a.Kind == hgUserKind {
		return a.UserID
	}
	return 0
}

// ownerOnly reports whether the caller may use action on type uid only for entries it created.
func (a *hgAuth) ownerOnly(uid, action string) bool {
	return a != nil && a.Kind == hgUserKind && containsStr(a.OwnerOnly[uid], action)
}

// hgOwnerAllows is false when a user restricted to its own entries addresses somebody else's
// document (every locale of a document has the creator of its first version). A failed lookup
// denies: only a document that does not exist yet is let through (the caller answers 404 or
// creates it).
func (s *Server) hgOwnerAllows(c *gin.Context, a *hgAuth, sc *hgSchema, doc, action string) bool {
	if !a.ownerOnly(sc.UID, action) {
		return true
	}
	if sc.IsSingle() {
		doc = s.hgSingleDoc(c, sc.UID)
	}
	var owner int64
	err := s.store.Pool.QueryRow(c, `SELECT created_by FROM hg_entries WHERE type_uid = $1 AND document_id = $2 ORDER BY id LIMIT 1`, sc.UID, doc).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return true // no such entry: the caller answers 404 (or creates it)
	}
	if err != nil {
		return false // fail closed: a database error must not skip the owner check
	}
	return owner == a.UserID && owner > 0
}

func (s *Server) hgAuthAPI(c *gin.Context, a *hgAuth, st hgSettings, segs []string, method string) {
	if !st.UserAuth {
		hgNotFound(c)
		return
	}
	switch {
	case len(segs) == 1 && segs[0] == "local" && method == http.MethodPost:
		s.hgLogin(c, st)
	case len(segs) == 2 && segs[0] == "local" && segs[1] == "register" && method == http.MethodPost:
		s.hgRegister(c, st)
	default:
		hgErr(c, http.StatusNotFound, "NotFoundError", "Not Found", nil)
	}
}

func hgJSONBody(c *gin.Context) (map[string]any, bool) {
	if !strings.HasPrefix(c.ContentType(), "application/json") {
		hgErr(c, http.StatusBadRequest, "ValidationError", "The body must be JSON", nil)
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
	var m map[string]any
	if err != nil || json.Unmarshal(raw, &m) != nil {
		hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid JSON body", nil)
		return nil, false
	}
	return m, true
}

func hgUserJSON(u *store.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Login, "email": u.Email, "name": u.DisplayName}
}

func (s *Server) hgLogin(c *gin.Context, st hgSettings) {
	body, ok := hgJSONBody(c)
	if !ok {
		return
	}
	ident, _ := body["identifier"].(string)
	pass, _ := body["password"].(string)
	ident = strings.TrimSpace(ident)
	if ident == "" || pass == "" || len(ident) > 255 || len(pass) > 1024 {
		hgErr(c, http.StatusBadRequest, "ValidationError", "identifier and password are required", nil)
		return
	}
	if s.adminsecRESTLocked(c, ident) {
		hgErr(c, http.StatusTooManyRequests, "TooManyRequestsError", "Too many failed attempts. Try again later.", nil)
		return
	}
	// The same CAPTCHA the login forms ask for (always, or for an account under attack); a missing
	// answer is no guess and is not counted.
	token, _ := body["captchaToken"].(string)
	if p, asked, good := s.apiLoginCaptcha(c, ident, token); asked && !good {
		hgErr(c, http.StatusForbidden, "ForbiddenError", "A valid CAPTCHA token (captchaToken) is required",
			map[string]any{"code": "captcha_required", "provider": p.kind, "siteKey": p.site})
		return
	}
	attemptID, refused := s.restBeginAttempt(c, ident)
	if refused {
		hgErr(c, http.StatusTooManyRequests, "TooManyRequestsError", "Too many failed attempts. Try again later.", nil)
		return
	}
	u, err := s.store.UserByLoginOrEmail(c, ident)
	if err != nil {
		auth.CheckDummyPassword(c, pass)
		s.restLoginFailed(c, ident, nil, "Unknown user (Headless API).", attemptID)
		hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid identifier or password", nil)
		return
	}
	good := s.checkUserPassword(c, u, pass)
	if !good {
		s.restLoginFailed(c, ident, u, "Wrong password (Headless API).", attemptID)
		hgErr(c, http.StatusBadRequest, "ValidationError", "Invalid identifier or password", nil)
		return
	}
	var twoFactor bool
	_ = s.store.Pool.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM adminsec_factors WHERE user_id = $1)`, u.ID).Scan(&twoFactor)
	if twoFactor {
		s.auth.DropAttempt(c, attemptID)
		hgErr(c, http.StatusForbidden, "ForbiddenError", "This account uses a second factor and cannot sign in to the API with a password", nil)
		return
	}
	s.auth.DropAttempt(c, attemptID)
	// A forced password change or an expired second-factor requirement (Pursae Security) gets no token:
	// the account must be fixed on the HTML screens first, like cookie and token REST (restAuthUser).
	if s.adminsecBlocked(c, u) != "" {
		hgErr(c, http.StatusForbidden, "ForbiddenError", "Your account needs attention before it can use the API: change your password or set up the sign-in method your role requires", map[string]any{"code": "account_action_required"})
		return
	}
	tok, ttl := s.hgIssueUserToken(st, u)
	hgWriteJSON(c, http.StatusOK, map[string]any{"jwt": tok, "expiresIn": ttl, "user": hgUserJSON(u)})
}

func (s *Server) hgRegister(c *gin.Context, st hgSettings) {
	if !st.UserRegister {
		hgErr(c, http.StatusForbidden, "ForbiddenError", "Registration is closed", nil)
		return
	}
	body, ok := hgJSONBody(c)
	if !ok {
		return
	}
	// The same gates as the other sign-up forms (wp-login, Forum): a per-network limit, and the
	// account-forms CAPTCHA when Pursae Security asks for one.
	if !forumAllow("hg-register:"+gooikeTruncateIP(clientIP(c)), 20, 10*time.Minute) {
		hgErr(c, http.StatusTooManyRequests, "TooManyRequestsError", "Too many sign-ups from your network. Try again later.", nil)
		return
	}
	if p, ok := s.loginCaptchaCfg(); ok {
		token, _ := body["captchaToken"].(string)
		token = strings.TrimSpace(token)
		good := token != "" && len(token) <= 4096
		if good {
			score, ok := s.verifyCaptcha(c.Request.Context(), p.endpoint, p.secret, token, clientIP(c))
			good = ok && score >= 0.5
		}
		if !good {
			hgErr(c, http.StatusForbidden, "ForbiddenError", "A valid CAPTCHA token (captchaToken) is required",
				map[string]any{"code": "captcha_required", "provider": p.kind, "siteKey": p.site})
			return
		}
	}
	login, _ := body["username"].(string)
	email, _ := body["email"].(string)
	pass, _ := body["password"].(string)
	login, email = strings.TrimSpace(login), strings.TrimSpace(email)
	var errs hgValidationError
	add := func(field, msg string) {
		errs = append(errs, hgFieldError{Path: []string{field}, Message: msg, Name: "ValidationError"})
	}
	lt, et, _ := s.store.LoginOrEmailTaken(c, login, email)
	switch {
	case login == "" || !validUsername.MatchString(login) || len(login) > 60:
		add("username", "Enter a valid username (letters, numbers and _ . - @ only)")
	case lt:
		add("username", "This username is already taken")
	}
	switch {
	case !validEmail(email) || len(email) > 100:
		add("email", "Enter a valid email address")
	case et:
		add("email", "This email address is already registered")
	}
	role := s.settings.String("default_role")
	if role == "" {
		role = "subscriber"
	}
	if len(pass) < 8 || len(pass) > 200 {
		add("password", "The password must be 8 to 200 characters")
	} else if msg := s.adminsecPasswordProblem([]string{role}, login, email, pass); msg != "" { // Pursae Security password policy
		add("password", msg)
	}
	if len(errs) > 0 {
		hgErr(c, http.StatusBadRequest, "ValidationError", errs.Error(), errs)
		return
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		hgFail(c, err)
		return
	}
	id, err := s.store.CreateUser(c, store.NewUser{Login: login, Email: email, PasswordHash: hash, Role: role})
	if err != nil {
		hgFail(c, err)
		return
	}
	u, err := s.store.UserByID(c, id)
	if err != nil {
		hgFail(c, err)
		return
	}
	tok, ttl := s.hgIssueUserToken(st, u)
	hgWriteJSON(c, http.StatusCreated, map[string]any{"jwt": tok, "expiresIn": ttl, "user": hgUserJSON(u)})
}
