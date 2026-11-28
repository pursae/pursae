package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

// "Continue with Google" for Pursae Forum: OAuth 2.0 authorization code flow with PKCE (S256) and
// OpenID Connect, standard library only.
//
//	GET /forum-auth/google[?redirect=<local path>][&link=1]  → Google's consent screen
//	GET /forum-auth/google/callback?code=&state=             → sign in, sign up or link, then back
//
// The flow state (state, PKCE verifier, nonce, return path, link user, expiry) travels in a short-lived
// HttpOnly cookie signed with the site secret; nothing is stored server side. The id_token comes
// straight from Google's token endpoint over TLS, so its claims are checked (iss, aud, exp, nonce)
// without verifying its signature (OIDC Core 3.1.3.7); the userinfo answer must name the same subject
// and a verified email.

// Google's endpoints; package variables so tests can point them at a fake server.
var (
	forumGoogleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	forumGoogleTokenURL    = "https://oauth2.googleapis.com/token"
	forumGoogleUserinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	forumGoogleHTTP        = &http.Client{Timeout: 10 * time.Second}
)

// forumGoogleIssuers are the iss values Google's id_tokens carry.
var forumGoogleIssuers = map[string]bool{"https://accounts.google.com": true, "accounts.google.com": true}

// forumOAuthTTL is how long the visitor has on Google's screen.
const forumOAuthTTL = 10 * time.Minute

// forumOAuthState is the signed cookie of one sign-in: S state, V PKCE verifier, N nonce, R return
// path, L the user linking their account (0 = sign in), E expiry (unix).
type forumOAuthState struct {
	S, V, N, R string
	L, E       int64
}

// forumGoogleOnly answers 404 unless Google sign-in is switched on and configured.
func (s *Server) forumGoogleOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.forumGoogleReady() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// forumGoogleRedirectURI is the redirect URI to register in the Google Cloud console: the site root
// (no language prefix) + /forum-auth/google/callback.
func (s *Server) forumGoogleRedirectURI(ctx context.Context) string {
	return strings.TrimSuffix(s.links(ctx).HomeURL("/"), "/") + forumAuthPrefix + "/google/callback"
}

// forumOAuthSig signs a cookie payload.
func (s *Server) forumOAuthSig(payload string) string {
	mac := hmac.New(sha256.New, []byte("pursae-forum-oauth|"+s.siteSecret()))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// forumOAuthEncode is the cookie value: base64url(json) "." hex(HMAC-SHA256).
func (s *Server) forumOAuthEncode(st forumOAuthState) string {
	b, _ := json.Marshal(st)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + s.forumOAuthSig(payload)
}

// forumOAuthDecode checks the signature (constant time) and the expiry of a cookie value.
func (s *Server) forumOAuthDecode(raw string, now time.Time) (*forumOAuthState, bool) {
	if s.siteSecret() == "" || raw == "" || len(raw) > 4096 {
		return nil, false
	}
	payload, sig, ok := strings.Cut(raw, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.forumOAuthSig(payload))) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, false
	}
	var st forumOAuthState
	if json.Unmarshal(b, &st) != nil || st.S == "" || st.V == "" || st.N == "" || now.Unix() > st.E {
		return nil, false
	}
	return &st, true
}

// forumOAuthSetCookie writes (or with value "" deletes) the flow cookie, scoped to /forum-auth/.
func (s *Server) forumOAuthSetCookie(c *gin.Context, value string) {
	ck := &http.Cookie{Name: forumOAuthCookie, Value: value, Path: forumAuthPrefix + "/", HttpOnly: true,
		Secure: s.requestHTTPS(c.Request), SameSite: http.SameSiteLaxMode, MaxAge: int(forumOAuthTTL.Seconds())}
	if value == "" {
		ck.MaxAge = -1
	}
	http.SetCookie(c.Writer, ck)
}

// forumPKCEChallenge is base64url(sha256(verifier)) (RFC 7636 S256).
func forumPKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// forumGoogleStart sends the visitor to Google. With link=1 a logged-in member connects Google to
// their account; a member who is already logged in otherwise just goes back.
func (s *Server) forumGoogleStart(c *gin.Context) {
	ctx := c.Request.Context()
	c.Header("Cache-Control", "no-store")
	back := forumSafeLocal(c.Query("redirect"))
	if back == "" {
		back = forumLocalPath(s.forumURL(ctx))
	}
	u := currentUser(c)
	st := forumOAuthState{S: auth.RandomToken(24), V: auth.RandomToken(48), N: auth.RandomToken(16), R: back,
		E: time.Now().Add(forumOAuthTTL).Unix()}
	if c.Query("link") == "1" {
		if u == nil {
			c.Redirect(http.StatusFound, s.forumURL(ctx, "login")+"?redirect="+url.QueryEscape(forumLocalPath(s.forumURL(ctx, "account"))))
			return
		}
		st.L = u.ID
	} else if u != nil {
		c.Redirect(http.StatusFound, back)
		return
	}
	if s.siteSecret() == "" {
		c.Redirect(http.StatusFound, s.forumURL(ctx, "login")+"?forum_msg=google_failed")
		return
	}
	s.forumOAuthSetCookie(c, s.forumOAuthEncode(st))
	q := url.Values{
		"client_id":             {s.forumLoad().GoogleClientID},
		"redirect_uri":          {s.forumGoogleRedirectURI(ctx)},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {st.S},
		"nonce":                 {st.N},
		"code_challenge":        {forumPKCEChallenge(st.V)},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	sep := "?"
	if strings.Contains(forumGoogleAuthURL, "?") {
		sep = "&"
	}
	c.Redirect(http.StatusFound, forumGoogleAuthURL+sep+q.Encode())
}

// forumGoogleClaims are the id_token / userinfo fields the forum reads.
type forumGoogleClaims struct {
	Iss           string          `json:"iss"`
	Aud           json.RawMessage `json:"aud"`
	Azp           string          `json:"azp"`
	Exp           float64         `json:"exp"`
	Nonce         string          `json:"nonce"`
	Sub           string          `json:"sub"`
	Email         string          `json:"email"`
	EmailVerified any             `json:"email_verified"` // bool, or "true" in some answers
	Name          string          `json:"name"`
	GivenName     string          `json:"given_name"`
	FamilyName    string          `json:"family_name"`
}

func (cl *forumGoogleClaims) verified() bool {
	switch v := cl.EmailVerified.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// audiences is aud as a list (a string or an array).
func (cl *forumGoogleClaims) audiences() []string {
	var one string
	if json.Unmarshal(cl.Aud, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(cl.Aud, &many)
	return many
}

// forumCheckIDToken decodes the id_token payload and checks iss, aud (azp when there are several
// audiences), exp and nonce.
func forumCheckIDToken(idToken, clientID, nonce string, now time.Time) (*forumGoogleClaims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("id_token: malformed")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errors.New("id_token: payload")
	}
	var cl forumGoogleClaims
	if err := json.Unmarshal(b, &cl); err != nil {
		return nil, errors.New("id_token: claims")
	}
	if !forumGoogleIssuers[cl.Iss] {
		return nil, fmt.Errorf("id_token: iss %q", cl.Iss)
	}
	auds := cl.audiences()
	found := false
	for _, a := range auds {
		if a == clientID {
			found = true
		}
	}
	if !found || clientID == "" || (len(auds) > 1 && cl.Azp != clientID) {
		return nil, errors.New("id_token: aud")
	}
	if cl.Exp <= 0 || int64(cl.Exp) <= now.Unix() {
		return nil, errors.New("id_token: expired")
	}
	if cl.Nonce == "" || subtle.ConstantTimeCompare([]byte(cl.Nonce), []byte(nonce)) != 1 {
		return nil, errors.New("id_token: nonce")
	}
	if cl.Sub == "" {
		return nil, errors.New("id_token: sub")
	}
	return &cl, nil
}

// forumGoogleExchange trades the code for tokens and returns the checked claims, merged with the
// userinfo answer (same sub, verified email).
func (s *Server) forumGoogleExchange(ctx context.Context, st *forumOAuthState, code string) (*forumGoogleClaims, error) {
	cfg := s.forumLoad()
	form := url.Values{"code": {code}, "client_id": {cfg.GoogleClientID}, "client_secret": {cfg.GoogleClientSecret},
		"redirect_uri": {s.forumGoogleRedirectURI(ctx)}, "grant_type": {"authorization_code"}, "code_verifier": {st.V}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, forumGoogleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := forumGoogleHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint: %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" || tok.IDToken == "" {
		return nil, errors.New("token endpoint: no tokens")
	}
	cl, err := forumCheckIDToken(tok.IDToken, cfg.GoogleClientID, st.N, time.Now())
	if err != nil {
		return nil, err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, forumGoogleUserinfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err = forumGoogleHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("userinfo: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo: %d", resp.StatusCode)
	}
	var ui forumGoogleClaims
	if err := json.Unmarshal(body, &ui); err != nil {
		return nil, errors.New("userinfo: json")
	}
	if ui.Sub == "" || subtle.ConstantTimeCompare([]byte(ui.Sub), []byte(cl.Sub)) != 1 {
		return nil, errors.New("userinfo: sub mismatch")
	}
	ui.Email = strings.TrimSpace(ui.Email)
	if ui.Email == "" || !ui.verified() {
		return nil, errors.New("userinfo: email not verified")
	}
	cl.Email, cl.EmailVerified = ui.Email, true
	if ui.Name != "" {
		cl.Name = ui.Name
	}
	if ui.GivenName != "" || ui.FamilyName != "" {
		cl.GivenName, cl.FamilyName = ui.GivenName, ui.FamilyName
	}
	return cl, nil
}

// forumGoogleCallback finishes the flow: the state, the code exchange and claim checks, then the
// account (link, known subject, safe auto-link, new member) and the sign-in gate.
func (s *Server) forumGoogleCallback(c *gin.Context) {
	ctx := c.Request.Context()
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	login := s.forumURL(ctx, "login")
	account := s.forumURL(ctx, "account")
	ck, _ := c.Cookie(forumOAuthCookie)
	s.forumOAuthSetCookie(c, "") // single use
	st, ok := s.forumOAuthDecode(ck, time.Now())
	if !ok {
		c.Redirect(http.StatusFound, login+"?forum_msg=google_failed")
		return
	}
	back := login
	if st.L > 0 {
		back = account
	}
	if c.Query("error") != "" {
		c.Redirect(http.StatusFound, back+"?forum_msg=google_cancelled")
		return
	}
	failed := func(why string, err error) {
		if err != nil {
			log.Printf("forum google sign-in: %s: %v", why, err)
		}
		c.Redirect(http.StatusFound, back+"?forum_msg=google_failed")
	}
	if state := c.Query("state"); state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(st.S)) != 1 {
		failed("state mismatch", nil)
		return
	}
	code := c.Query("code")
	if code == "" || len(code) > 2048 {
		failed("no code", nil)
		return
	}
	cl, err := s.forumGoogleExchange(ctx, st, code)
	if err != nil {
		failed("exchange", err)
		return
	}

	// (a) Link mode: the member who started it, the subject not linked to anyone else.
	if st.L > 0 {
		u := currentUser(c)
		if u == nil || u.ID != st.L {
			failed("link: different session", nil)
			return
		}
		if other, err := s.forumUserByGoogleSub(ctx, cl.Sub); err == nil && other.ID != u.ID {
			failed("link: subject linked to another account", nil)
			return
		} else if err != nil && !errors.Is(err, errForumNotFound) {
			failed("link: lookup", err)
			return
		}
		set := map[string]any{forumUMetaGoogleSub: cl.Sub, forumUMetaGoogleEmail: cl.Email}
		if strings.EqualFold(cl.Email, u.Email) {
			set[forumUMetaEmailVerified] = strings.ToLower(u.Email) // Google vouches for the account's own address
		}
		if err := s.forumSetUserMeta(ctx, u.ID, set); err != nil {
			failed("link: save", err)
			return
		}
		c.Redirect(http.StatusFound, account+"?forum_msg=google_linked")
		return
	}

	// Pursae Security's login block list and IP lockout apply to Google sign-in as to the password form.
	if s.forumSecRefused(c) {
		c.Redirect(http.StatusFound, login+"?forum_msg=login_blocked")
		return
	}
	u, msg, err := s.forumGoogleAccount(ctx, cl)
	if err != nil {
		failed("account", err)
		return
	}
	if msg != "" {
		c.Redirect(http.StatusFound, login+"?forum_msg="+msg)
		return
	}
	// The sign-in gate: administrators and accounts with a second factor log in with their password.
	if u.Can("manage_options", nil) || (s.adminsecOn() && s.asFactors(ctx, u.ID).any()) {
		c.Redirect(http.StatusFound, login+"?forum_msg=google_blocked")
		return
	}
	if err := s.forumStartSession(c, u, true, "Google"); err != nil {
		failed("session", err)
		return
	}
	s.pluginUserAction(ctx, "wp_login", u)
	c.Redirect(http.StatusFound, s.forumTarget(c, st.R))
}

// forumGoogleAccount resolves the account of a Google identity: (b) the account linked to the
// subject; (c) an account with the same email, linked automatically only when it has no editing,
// moderating or admin rights (and no other Google account), else google_link_required; (d) a new
// member when registration is open, else registration_closed. msg is a forumMessages code.
// An account whose address was never proven (Commerce checkout, HeadlessGo, a changed email, an
// unconfirmed sign-up) may belong to someone else than the Google user (account pre-hijacking): it
// is linked with a fresh random password and all its sessions ended.
func (s *Server) forumGoogleAccount(ctx context.Context, cl *forumGoogleClaims) (*store.User, string, error) {
	if u, err := s.forumUserByGoogleSub(ctx, cl.Sub); err == nil {
		return u, "", nil
	} else if !errors.Is(err, errForumNotFound) {
		return nil, "", err
	}
	var id int64
	err := s.store.Pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = lower($1) ORDER BY id LIMIT 1`, cl.Email).Scan(&id)
	switch {
	case err == nil:
		u, err := s.store.UserByID(ctx, id)
		if err != nil {
			return nil, "", err
		}
		if u.Can("edit_posts", nil) || u.Can(capForumModerate, nil) || u.Can("manage_options", nil) ||
			forumMetaStr(u.Meta, forumUMetaGoogleSub) != "" {
			return nil, "google_link_required", nil
		}
		set := map[string]any{forumUMetaGoogleSub: cl.Sub, forumUMetaGoogleEmail: cl.Email, forumUMetaEmailVerified: strings.ToLower(u.Email)}
		if !forumEmailProven(u) {
			// Nobody proved they own this address, so whoever made the account may not be the person
			// Google vouches for now: drop that password, every session of it and every credential
			// minted with it (application passwords, OAuth grants and pending codes).
			hash, err := auth.HashPassword(auth.RandomToken(32))
			if err == nil {
				err = s.store.SetUserPassword(ctx, u.ID, hash)
			}
			if err == nil {
				err = s.auth.DestroyOthers(ctx, u.ID, 0)
			}
			if err == nil {
				err = s.revokeUserCredentials(ctx, u.ID, 0, 0)
			}
			if err != nil {
				return nil, "", err
			}
			set[forumUMetaNoPassword] = forumPasswordMark(hash)
		}
		// Google vouches for the address, which also confirms it.
		if err := s.forumSetUserMeta(ctx, u.ID, set, forumUMetaUnverified, forumUMetaVerify); err != nil {
			return nil, "", err
		}
		u, err = s.store.UserByID(ctx, id)
		return u, "", err
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, "", err
	}
	st := s.forumLoad()
	if !st.Registration {
		return nil, "registration_closed", nil
	}
	given, family := cl.GivenName, cl.FamilyName
	if r := []rune(given + family); len(r) > 40 { // keep logins short
		given, family = string(r[:40]), ""
	}
	login := s.gooGenerateUsername(ctx, cl.Email, given, family)
	if strings.HasPrefix(login, "customer") && !strings.HasPrefix(strings.ToLower(cl.Email), "customer") {
		login = s.gooGenerateUsername(ctx, cl.Email, "member", "") // Commerce's fallback name reads oddly here
	}
	if !validUsername.MatchString(login) || len(login) > 60 {
		return nil, "", fmt.Errorf("no usable username for %q", cl.Email)
	}
	name := strings.TrimSpace(cl.Name)
	if name == "" {
		name = login
	}
	if r := []rune(name); len(r) > 60 {
		name = string(r[:60])
	}
	hash, err := auth.HashPassword(auth.RandomToken(32))
	if err != nil {
		return nil, "", err
	}
	id, err = s.store.CreateUser(ctx, store.NewUser{Login: login, Email: cl.Email, PasswordHash: hash, DisplayName: name, Role: st.DefaultRole})
	if err != nil {
		return nil, "", err
	}
	if err := s.forumSetUserMeta(ctx, id, map[string]any{forumUMetaGoogleSub: cl.Sub, forumUMetaGoogleEmail: cl.Email,
		forumUMetaEmailVerified: strings.ToLower(cl.Email), forumUMetaNoPassword: forumPasswordMark(hash)}); err != nil {
		return nil, "", err
	}
	u, err := s.store.UserByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	s.pluginUserAction(ctx, "user_register", u)
	return u, "", nil
}
