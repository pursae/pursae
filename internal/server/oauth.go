package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// OAuth 2.1 for the MCP endpoint, as the MCP authorization spec asks: protected-resource and
// authorization-server metadata, dynamic client registration (public clients), authorization code with
// PKCE S256, short-lived access tokens and rotating refresh tokens. A token carries the scopes the user
// ticked on the consent screen and is narrowed exactly like an Application Password
// (app_password_scopes.go); Application Passwords keep working next to it.

const (
	oauthAccessTTL  = time.Hour
	oauthRefreshTTL = 30 * 24 * time.Hour
	oauthCodeTTL    = 5 * time.Minute
	oauthMaxClients = 500
	// oauthMaxGrantsPerUser connected applications per user; a new grant beyond it ends the oldest.
	oauthMaxGrantsPerUser = 20
	// oauthRegisterLimit registrations per address per oauthRegisterWindow: a real client registers
	// once per connection, so a flood cannot fill the oauthMaxClients cap from one address.
	oauthRegisterLimit  = 10
	oauthRegisterWindow = time.Hour
	// oauthRegisterGlobalLimit caps registrations from all addresses together per window, below
	// oauthMaxClients, so rotating addresses (an IPv6 /48, many IPv4 hosts) cannot fill the cap alone.
	oauthRegisterGlobalLimit = 100
	// oauthRegisterNetLimit caps registrations from one network (IPv4 /24, IPv6 /48) per window, so a
	// few hosts of one provider cannot use up the global window and lock out every other client.
	oauthRegisterNetLimit = 25
	oauthMCPPath          = "/wp-json/pursae/v1/mcp"
	restOAuthKey          = "wpgo.restOAuthGrant" // oauth_tokens.id that authenticated the request
)

// oauthReuseGrace is how long after a rotation the replaced refresh token is refused without ending
// the grant (a variable for tests).
var oauthReuseGrace = 30 * time.Second

func (s *Server) registerOAuth(r *gin.Engine) {
	for _, m := range []string{http.MethodGet, http.MethodOptions} {
		r.Handle(m, "/.well-known/oauth-protected-resource", s.oauthResourceMeta)
		r.Handle(m, "/.well-known/oauth-protected-resource/*path", s.oauthResourceMeta)
		r.Handle(m, "/.well-known/oauth-authorization-server", s.oauthServerMeta)
		r.Handle(m, "/.well-known/oauth-authorization-server/*path", s.oauthServerMeta)
	}
	for _, m := range []string{http.MethodPost, http.MethodOptions} {
		r.Handle(m, "/oauth/register", s.oauthRegister)
		r.Handle(m, "/oauth/token", s.oauthToken)
		r.Handle(m, "/oauth/revoke", s.oauthRevoke)
	}
}

func oauthCORS(c *gin.Context) bool {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
	c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if c.Request.Method == http.MethodOptions {
		c.Status(http.StatusNoContent)
		return true
	}
	return false
}

func (s *Server) oauthIssuer(c *gin.Context) string {
	return strings.TrimRight(s.links(c).HomeURL("/"), "/")
}
func (s *Server) oauthResource(c *gin.Context) string {
	return s.links(c).HomeURL(oauthMCPPath)
}

// oauthChallenge is the WWW-Authenticate value that points a client at the resource metadata.
func (s *Server) oauthChallenge(c *gin.Context, errCode string) string {
	v := `Bearer resource_metadata="` + s.oauthIssuer(c) + `/.well-known/oauth-protected-resource` + oauthMCPPath + `"`
	if errCode != "" {
		v += `, error="` + errCode + `"`
	}
	return v
}

func scopeSlugs() []string {
	out := make([]string, 0, len(appScopes))
	for _, sc := range appScopes {
		out = append(out, sc.Slug)
	}
	return out
}

func (s *Server) oauthResourceMeta(c *gin.Context) {
	if oauthCORS(c) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"resource": s.oauthResource(c), "authorization_servers": []string{s.oauthIssuer(c)},
		"scopes_supported": scopeSlugs(), "bearer_methods_supported": []string{"header"}, "resource_name": "Pursae MCP"})
}

func (s *Server) oauthServerMeta(c *gin.Context) {
	if oauthCORS(c) {
		return
	}
	iss := s.oauthIssuer(c)
	c.JSON(http.StatusOK, gin.H{"issuer": iss,
		"authorization_endpoint": iss + "/wp-admin/oauth-authorize", "token_endpoint": iss + "/oauth/token",
		"registration_endpoint": iss + "/oauth/register", "revocation_endpoint": iss + "/oauth/revoke", "scopes_supported": scopeSlugs(),
		"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}})
}

func oauthErr(c *gin.Context, status int, code, desc string) {
	c.Header("Cache-Control", "no-store")
	c.JSON(status, gin.H{"error": code, "error_description": desc})
}

func oauthRandom(prefix string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func oauthHash(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }

// oauthRegister is RFC 7591 dynamic client registration for public clients (no secret; PKCE protects
// the code).
func (s *Server) oauthRegister(c *gin.Context) {
	if oauthCORS(c) {
		return
	}
	if !oauthRegisterAllowNet(oauthRegisterKey(clientIP(c)), oauthRegisterNet(clientIP(c)), time.Now()) {
		oauthErr(c, http.StatusTooManyRequests, "temporarily_unavailable", "Too many client registrations from this address. Try again later.")
		return
	}
	var in struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	// Anonymous endpoint: a client's metadata is a few hundred bytes, so do not read more than 64 KB
	// (the JSON decoder would otherwise buffer a body of any size).
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(&in); err != nil || len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthErr(c, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required (1 to 10 entries).")
		return
	}
	for _, u := range in.RedirectURIs {
		pu, err := url.Parse(u)
		if err != nil || validAuthorizeRedirect(u) != nil || pu.Fragment != "" || pu.Scheme == "" {
			oauthErr(c, http.StatusBadRequest, "invalid_redirect_uri", "Redirect URIs must be https, http on localhost, or an app scheme, without a fragment.")
			return
		}
	}
	name := strings.TrimSpace(in.ClientName)
	if name == "" {
		name = "MCP client"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	ctx := c.Request.Context()
	// Keep the table small before counting (the hourly job does the same, oauthGC).
	_ = s.oauthGC(ctx)
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM oauth_clients`).Scan(&n)
	if n >= oauthMaxClients {
		oauthErr(c, http.StatusTooManyRequests, "temporarily_unavailable", "Too many registered clients.")
		return
	}
	id := newUUID()
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO oauth_clients (client_id, name, redirect_uris) VALUES ($1, $2, $3)`, id, name, in.RedirectURIs); err != nil {
		oauthErr(c, http.StatusInternalServerError, "server_error", "Could not register the client.")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{"client_id": id, "client_name": name, "redirect_uris": in.RedirectURIs,
		"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
}

// oauthRegisterLimiter counts registrations per address in fixed windows, plus one window for all
// addresses together.
var oauthRegisterLimiter = struct {
	sync.Mutex
	hits   map[string]oauthRegisterHits
	global oauthRegisterHits
}{hits: map[string]oauthRegisterHits{}}

// oauthRegisterNet is the network of a client address for oauthRegisterNetLimit: IPv4 /24, IPv6 /48.
func oauthRegisterNet(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return "net:" + v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return "net:" + ip.Mask(net.CIDRMask(48, 128)).String() + "/48"
}

// oauthRegisterKey is the limiter key for a client address: IPv4 as is, IPv6 by its /64 prefix
// (one subscriber usually holds a whole /64, so per-address counting would never trigger).
func oauthRegisterKey(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() != nil {
		return addr
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

type oauthRegisterHits struct {
	start time.Time
	n     int
}

func oauthRegisterAllow(key string, now time.Time) bool { return oauthRegisterAllowNet(key, "", now) }

// oauthRegisterAllowNet counts one registration against the address key, its network (when known)
// and the global window; a refusal uses up none of them.
func oauthRegisterAllowNet(key, network string, now time.Time) bool {
	oauthRegisterLimiter.Lock()
	defer oauthRegisterLimiter.Unlock()
	if len(oauthRegisterLimiter.hits) > 10000 { // bound memory: drop expired windows
		for k, h := range oauthRegisterLimiter.hits {
			if now.Sub(h.start) > oauthRegisterWindow {
				delete(oauthRegisterLimiter.hits, k)
			}
		}
	}
	h := oauthRegisterLimiter.hits[key]
	if now.Sub(h.start) > oauthRegisterWindow {
		h = oauthRegisterHits{start: now}
	}
	if h.n >= oauthRegisterLimit {
		return false // a refused request does not use up the global budget
	}
	var nh oauthRegisterHits
	if network != "" {
		if nh = oauthRegisterLimiter.hits[network]; now.Sub(nh.start) > oauthRegisterWindow {
			nh = oauthRegisterHits{start: now}
		}
		if nh.n >= oauthRegisterNetLimit {
			return false
		}
	}
	g := oauthRegisterLimiter.global
	if now.Sub(g.start) > oauthRegisterWindow {
		g = oauthRegisterHits{start: now}
	}
	if g.n >= oauthRegisterGlobalLimit {
		return false
	}
	h.n++
	g.n++
	oauthRegisterLimiter.hits[key] = h
	if network != "" {
		nh.n++
		oauthRegisterLimiter.hits[network] = nh
	}
	oauthRegisterLimiter.global = g
	return true
}

type oauthClient struct {
	ID, Name string
	Redirect []string
}

func (s *Server) oauthClientByID(c *gin.Context, id string) *oauthClient {
	if !isUUID(id) {
		return nil
	}
	cl := &oauthClient{ID: id}
	if s.store.Pool.QueryRow(c, `SELECT name, redirect_uris FROM oauth_clients WHERE client_id = $1`, id).Scan(&cl.Name, &cl.Redirect) != nil {
		return nil
	}
	return cl
}

// oauthAuthorize is the consent screen (/wp-admin/oauth-authorize): the user is signed in already
// (the admin group redirects to the login), checks who asks for what, ticks the scopes and approves.
func (s *Server) oauthAuthorize(c *gin.Context) {
	u := currentUser(c)
	_ = c.Request.ParseForm()
	f := c.Request.Form
	fail := func(msg string) {
		s.adminHTML(c, http.StatusBadRequest, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Invalid request</title>`+
			`<link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head><body id="error-page"><div class="wp-die-message"><p>`+
			template.HTMLEscapeString(msg)+`</p></div></body></html>`)
	}
	cl := s.oauthClientByID(c, f.Get("client_id"))
	redirect := f.Get("redirect_uri")
	if cl == nil || !containsStr(cl.Redirect, redirect) { // never redirect to an unregistered URI
		fail("Unknown application or redirect URI.")
		return
	}
	cspAllowRedirect(c, redirect) // the consent form's POST answers 302 to the client (form-action)
	state := f.Get("state")
	back := func(q url.Values) {
		if state != "" {
			q.Set("state", state)
		}
		sep := "?"
		if strings.Contains(redirect, "?") {
			sep = "&"
		}
		c.Redirect(http.StatusFound, redirect+sep+q.Encode())
	}
	// Errors found before the user decided are shown here, never sent to redirect_uri: clients register
	// themselves anonymously (oauthRegister), so a registered redirect_uri proves nothing about where it
	// leads, and redirecting a malformed request to it would make this site an open redirector (RFC 9700
	// §4.11; OAuth 2.1 sends errors to the client only once client and redirect are trusted). Only the
	// user's own answer on the consent screen (approve, deny) goes back to the client.
	if f.Get("response_type") != "code" {
		fail("Unsupported response_type: only \"code\" is supported.")
		return
	}
	challenge := f.Get("code_challenge")
	if f.Get("code_challenge_method") != "S256" || len(challenge) < 43 || len(challenge) > 128 {
		fail("Invalid request: PKCE with code_challenge_method=S256 is required.")
		return
	}
	resource, ok := s.oauthCanonicalResource(c, f.Get("resource"))
	if !ok {
		fail("Invalid resource: this server only issues tokens for its MCP endpoint.")
		return
	}
	asked := normalizeAppScopes(strings.Fields(strings.ReplaceAll(f.Get("scope"), ",", " ")))
	// Without a scope request the consent screen starts from a narrow default; full access needs its own
	// explicit tick (a client that sends no scope must not get everything by default).
	chosen := asked
	if len(chosen) == 0 {
		chosen = oauthDefaultScopes
	}
	if c.Request.Method == http.MethodPost && c.PostForm("action") == "oauth_authorize" {
		if !s.checkNonce(c, "oauth_authorize") {
			s.denyNonce(c)
			return
		}
		if _, deny := c.GetPostForm("deny"); deny {
			back(url.Values{"error": {"access_denied"}})
			return
		}
		chosen = normalizeAppScopes(c.PostFormArray("scopes[]"))
		if c.PostForm("full_access") == "1" {
			chosen = []string{} // every permission of the user, as the ticked box says
		} else if len(chosen) == 0 {
			chosen = oauthDefaultScopes
		}
		code := oauthRandom("")
		if _, err := s.store.Pool.Exec(c, `INSERT INTO oauth_codes (code_hash, client_id, user_id, redirect_uri, scopes, challenge, expires_at, resource)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, oauthHash(code), cl.ID, u.ID, redirect, chosen, challenge, time.Now().Add(oauthCodeTTL), resource); err != nil {
			back(url.Values{"error": {"server_error"}})
			return
		}
		_, _ = s.store.Pool.Exec(c, `DELETE FROM oauth_codes WHERE expires_at < now()`)
		back(url.Values{"code": {code}})
		return
	}
	d := map[string]any{"Client": cl.Name, "Redirect": redirect, "Nonce": s.nonce(c, "oauth_authorize"), "AppScopes": scopeChoices(chosen),
		"Fields": map[string]string{"client_id": cl.ID, "redirect_uri": redirect, "response_type": "code", "state": state,
			"code_challenge": challenge, "code_challenge_method": "S256", "scope": f.Get("scope"), "resource": f.Get("resource")}}
	s.adminPage(c, "oauth-authorize", "Authorize Application", "authorize-application-php", "admin/oauth-authorize", d)
}

// oauthDefaultScopes is what an authorization gets when the client asked for no scope and the user
// ticked nothing.
var oauthDefaultScopes = []string{"content", "media"}

// oauthToken is the token endpoint: authorization_code (with the PKCE verifier) and refresh_token.
func (s *Server) oauthToken(c *gin.Context) {
	if oauthCORS(c) {
		return
	}
	ctx := c.Request.Context()
	_ = c.Request.ParseForm()
	grant := c.PostForm("grant_type")
	clientID := c.PostForm("client_id")
	if s.oauthClientByID(c, clientID) == nil {
		oauthErr(c, http.StatusUnauthorized, "invalid_client", "Unknown client_id.")
		return
	}
	var userID int64
	var scopes []string
	var resource string
	access, refresh := oauthRandom("pat_"), oauthRandom("prt_")
	now := time.Now()
	switch grant {
	case "authorization_code":
		var redirect, challenge string
		var exp time.Time
		codeHash := oauthHash(c.PostForm("code"))
		// The code is single use: it is deleted by the lookup whatever happens next.
		err := s.store.Pool.QueryRow(ctx, `DELETE FROM oauth_codes WHERE code_hash = $1 AND client_id = $2
			RETURNING user_id, redirect_uri, scopes, challenge, expires_at, resource`, codeHash, clientID).Scan(&userID, &redirect, &scopes, &challenge, &exp, &resource)
		if errors.Is(err, pgx.ErrNoRows) {
			// A code presented again was intercepted or replayed: revoke the grant its first redemption
			// issued (OAuth 2.1 §4.1.3).
			_, _ = s.store.Pool.Exec(ctx, `DELETE FROM oauth_tokens WHERE code_hash = $1 AND client_id = $2`, codeHash, clientID)
		}
		if err != nil || time.Now().After(exp) || redirect != c.PostForm("redirect_uri") {
			oauthErr(c, http.StatusBadRequest, "invalid_grant", "The authorization code is invalid, expired or used.")
			return
		}
		sum := sha256.Sum256([]byte(c.PostForm("code_verifier")))
		want := base64.RawURLEncoding.EncodeToString(sum[:])
		if v := c.PostForm("code_verifier"); len(v) < 43 || len(v) > 128 || subtle.ConstantTimeCompare([]byte(want), []byte(challenge)) != 1 {
			oauthErr(c, http.StatusBadRequest, "invalid_grant", "PKCE verification failed.")
			return
		}
		if asked := c.PostForm("resource"); asked != "" {
			canon, ok := s.oauthCanonicalResource(c, asked)
			if !ok || (resource != "" && canon != resource) {
				oauthErr(c, http.StatusBadRequest, "invalid_target", "The resource does not match this authorization.")
				return
			}
		}
		if _, err := s.store.Pool.Exec(ctx, `INSERT INTO oauth_tokens (client_id, user_id, access_hash, refresh_hash, scopes, access_expires_at, refresh_expires_at, code_hash, resource)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, clientID, userID, oauthHash(access), oauthHash(refresh), scopes,
			now.Add(oauthAccessTTL), now.Add(oauthRefreshTTL), codeHash, resource); err != nil {
			oauthErr(c, http.StatusInternalServerError, "server_error", "Could not issue a token.")
			return
		}
		// One user keeps at most oauthMaxGrantsPerUser connected applications: the oldest go, and with
		// them (after an hour) their clients, so one account cannot fill the oauthMaxClients cap.
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM oauth_tokens WHERE user_id = $1 AND id NOT IN
			(SELECT id FROM oauth_tokens WHERE user_id = $1 ORDER BY id DESC LIMIT $2)`, userID, oauthMaxGrantsPerUser)
	case "refresh_token":
		old := oauthHash(c.PostForm("refresh_token"))
		// Rotate in one statement, so two requests with the same refresh token cannot both get a pair.
		err := s.store.Pool.QueryRow(ctx, `UPDATE oauth_tokens SET access_hash = $3, refresh_hash = $4,
			prev_refresh_hash = array_to_string((string_to_array(
				CASE WHEN prev_refresh_hash IS NULL OR prev_refresh_hash = '' THEN refresh_hash
				ELSE refresh_hash || ',' || prev_refresh_hash END, ','))[1:20], ','),
			access_expires_at = $5, refresh_expires_at = $6
			WHERE refresh_hash = $1 AND client_id = $2 AND refresh_expires_at > now() RETURNING user_id, scopes, resource`,
			old, clientID, oauthHash(access), oauthHash(refresh), now.Add(oauthAccessTTL), now.Add(oauthRefreshTTL)).Scan(&userID, &scopes, &resource)
		if err != nil {
			// A refresh token that a rotation already replaced is being used again: it was stolen, or the
			// thief already rotated it. End the whole grant family (OAuth 2.1 §4.3.1) — every descendant
			// of the same authorization, not only the row whose immediate previous hash still matches.
			// Within oauthReuseGrace of the latest rotation the immediately previous token is only
			// refused: that is a client refreshing twice at once or retrying a lost answer. An older
			// token in the chain always burns the family. (A rotation sets access_expires_at to its
			// time plus oauthAccessTTL.)
			cutoff := now.Add(oauthAccessTTL - oauthReuseGrace)
			_, _ = s.store.Pool.Exec(ctx, `DELETE FROM oauth_tokens t WHERE t.client_id = $2 AND (
				t.id IN (
					SELECT id FROM oauth_tokens
					WHERE client_id = $2
					  AND $1 = ANY (string_to_array(coalesce(prev_refresh_hash, ''), ','))
					  AND NOT (split_part(coalesce(prev_refresh_hash, ''), ',', 1) = $1 AND access_expires_at > $3)
				)
				OR (
					coalesce(t.code_hash, '') <> ''
					AND t.code_hash IN (
						SELECT code_hash FROM oauth_tokens
						WHERE client_id = $2
						  AND coalesce(code_hash, '') <> ''
						  AND $1 = ANY (string_to_array(coalesce(prev_refresh_hash, ''), ','))
						  AND NOT (split_part(coalesce(prev_refresh_hash, ''), ',', 1) = $1 AND access_expires_at > $3)
					)
				)
			)`, old, clientID, cutoff)
			oauthErr(c, http.StatusBadRequest, "invalid_grant", "The refresh token is invalid or expired.")
			return
		}
	default:
		oauthErr(c, http.StatusBadRequest, "unsupported_grant_type", "Use authorization_code or refresh_token.")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusOK, gin.H{"access_token": access, "token_type": "Bearer", "expires_in": int(oauthAccessTTL.Seconds()),
		"refresh_token": refresh, "scope": strings.Join(scopes, " ")})
}

// oauthCanonicalResource is the MCP resource a grant is bound to (RFC 8707). An empty indicator, the
// MCP URL and the issuer all mean this site's MCP endpoint; any other value is refused.
func (s *Server) oauthCanonicalResource(c *gin.Context, raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	canon := s.oauthResource(c)
	iss := s.oauthIssuer(c)
	if raw == "" || raw == canon || raw == iss || raw == iss+"/" {
		return canon, true
	}
	return "", false
}

// oauthBearer authenticates "Authorization: Bearer <access token>". The scopes of the grant narrow the
// user like those of an Application Password.
func (s *Server) oauthBearer(c *gin.Context, token string) bool {
	var id, userID int64
	var scopes []string
	var resource string
	var exp time.Time
	if s.store.Pool.QueryRow(c, `SELECT id, user_id, scopes, access_expires_at, resource FROM oauth_tokens WHERE access_hash = $1`, oauthHash(token)).Scan(&id, &userID, &scopes, &exp, &resource) != nil || time.Now().After(exp) {
		return false
	}
	if resource != "" && resource != s.oauthResource(c) {
		return false // the grant was issued for a different protected resource
	}
	known := normalizeAppScopes(scopes)
	if len(scopes) > 0 && len(known) == 0 {
		return false // only scopes this version no longer knows: never widen them to full access
	}
	c.Set(restOAuthKey, id)
	c.Set(restOAuthUserKey, userID)
	if len(known) > 0 {
		c.Set(restAppScopesKey, known)
	}
	s.asyncExec(`UPDATE oauth_tokens SET last_used_at = now(), last_ip = $2 WHERE id = $1`, id, clientIP(c)) // bounded queue (background.go)
	return true
}

const restOAuthUserKey = "wpgo.restOAuthUser"

// oauthGrantRow is one connected application in the profile.
type oauthGrantRow struct {
	ID                    int64
	Client, Created, Used string
	ScopeList             string
}

func (s *Server) oauthGrantRows(c *gin.Context, userID int64) []oauthGrantRow {
	rows, err := s.store.Pool.Query(c, `SELECT t.id, cl.name, t.created_at, t.last_used_at, t.scopes FROM oauth_tokens t
		JOIN oauth_clients cl ON cl.client_id = t.client_id WHERE t.user_id = $1 ORDER BY t.id`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	loc := s.loc()
	var out []oauthGrantRow
	for rows.Next() {
		var g oauthGrantRow
		var created time.Time
		var used *time.Time
		var scopes []string
		if rows.Scan(&g.ID, &g.Client, &created, &used, &scopes) != nil {
			continue
		}
		g.Created, g.Used, g.ScopeList = created.In(loc).Format("January 2, 2006"), "—", scopeLabels(scopes)
		if used != nil {
			g.Used = used.In(loc).Format("January 2, 2006")
		}
		out = append(out, g)
	}
	return out
}

// oauthRevoke is RFC 7009: the client gives up an access or refresh token and the whole grant ends.
// Like the RFC asks, the answer is 200 whether or not the token was known.
func (s *Server) oauthRevoke(c *gin.Context) {
	if oauthCORS(c) {
		return
	}
	_ = c.Request.ParseForm()
	if t := c.PostForm("token"); t != "" && isUUID(c.PostForm("client_id")) {
		// The current access or refresh token, or a refresh token a rotation already replaced, ends the
		// whole grant family (RFC 7009). A client that lost the new pair can still revoke with the old one.
		_, _ = s.store.Pool.Exec(c.Request.Context(), `WITH hit AS (
			SELECT id, code_hash FROM oauth_tokens
			WHERE client_id = $1 AND (
				access_hash = $2 OR refresh_hash = $2
				OR $2 = ANY (string_to_array(coalesce(prev_refresh_hash, ''), ','))
			)
		)
		DELETE FROM oauth_tokens t WHERE t.client_id = $1 AND (
			t.id IN (SELECT id FROM hit)
			OR (coalesce(t.code_hash, '') <> '' AND t.code_hash IN (SELECT code_hash FROM hit WHERE coalesce(code_hash, '') <> ''))
		)`, c.PostForm("client_id"), oauthHash(t))
	}
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
}

// oauthGC keeps the OAuth tables and the MCP activity log small. It drops grants whose refresh token
// has expired (they can never be used again), expired authorization codes, clients without a grant
// after an hour (a pending code, which lives 5 minutes, keeps a client in the middle of its consent
// flow) and activity rows past their retention. It runs with the hourly session clean-up and before a
// client registers, so a site nobody registers on any more is cleaned too.
func (s *Server) oauthGC(ctx context.Context) error {
	var first error
	for _, q := range []string{
		`DELETE FROM oauth_tokens WHERE refresh_expires_at < now()`,
		`DELETE FROM oauth_codes WHERE expires_at < now()`,
		`DELETE FROM oauth_clients WHERE created_at < now() - interval '1 hour'
			AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.client_id = oauth_clients.client_id)
			AND NOT EXISTS (SELECT 1 FROM oauth_codes k WHERE k.client_id = oauth_clients.client_id AND k.expires_at > now())`,
		`DELETE FROM mcp_activity WHERE created_at < now() - interval '90 days'`,
	} {
		if _, err := s.store.Pool.Exec(ctx, q); err != nil && first == nil {
			first = err
		}
	}
	return first
}
