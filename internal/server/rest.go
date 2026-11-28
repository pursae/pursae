package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// The REST API mirrors WordPress' wp/v2 namespace closely enough for the block editor
// (Gotenberg) and headless clients written against WordPress to work unchanged.

type restError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func restErr(c *gin.Context, status int, code, msg string, extra ...map[string]any) {
	if restInternalError(code, msg) {
		// The database's own words (engine, table and column names, the failing value) stay in the log.
		slog.Error("rest: request failed", "code", code, "status", status, "method", c.Request.Method, "path", c.Request.URL.Path, "err", msg)
		if restOutOfRange(msg) {
			// A number too large for its column is the client's mistake, not the server's.
			status, code, msg = http.StatusBadRequest, "rest_invalid_param", "A number in the request is out of range."
		} else {
			msg = restInternalMessage
		}
	}
	data := map[string]any{"status": status}
	for _, e := range extra {
		for k, v := range e {
			data[k] = v
		}
	}
	c.AbortWithStatusJSON(status, restError{Code: code, Message: msg, Data: data})
}

// restInternalMessage replaces the message of an error that carried database detail (restInternalError).
const restInternalMessage = "The request could not be completed because of a database error."

// restInternalError reports whether an error message must not reach the client: rest_query_error
// carries a store error, and any message with a PostgreSQL SQLSTATE is the server's text
// (pgconn.PgError.Error()), whatever the code the handler chose.
func restInternalError(code, msg string) bool {
	if code == "rest_query_error" || strings.Contains(msg, "(SQLSTATE ") {
		return true
	}
	// pgx's own client-side errors carry no SQLSTATE but still name column types and values.
	for _, m := range restDriverMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// restDriverMarkers are the beginnings of pgx / pgconn error texts (encoding, scanning, connection).
var restDriverMarkers = []string{"failed to encode args", "unable to encode", "can't scan into", "cannot scan ",
	"pgx:", "pgconn", "(OID ", "failed to connect to", "conn closed", "conn busy", "no rows in result set"}

// restOutOfRange reports whether a database error is a number out of range for its column: SQLSTATE
// 22003 or pgx refusing to encode a value its column type cannot hold.
func restOutOfRange(msg string) bool {
	return strings.Contains(msg, "(SQLSTATE 22003)") || strings.Contains(msg, "is greater than maximum value for") ||
		strings.Contains(msg, "is less than minimum value for")
}

// restRoute describes one registered route for routing, OPTIONS and the index.
type restRoute struct {
	Namespace string
	Pattern   string // e.g. /wp/v2/posts/(?P<id>[\d]+)
	re        *regexp.Regexp
	Methods   map[string]restHandler
	Args      map[string]any
}

type restHandler func(c *gin.Context, req *restRequest)

// restRequest carries the parsed request: URL params, query/body params and the JSON body.
type restRequest struct {
	Params map[string]string
	Body   map[string]any
	Method string
}

func (r *restRequest) ID() int64 {
	id, _ := strconv.ParseInt(r.Params["id"], 10, 64)
	return id
}

// Get reads a parameter from the JSON body, then the form body, then the query string.
func (r *restRequest) get(c *gin.Context, key string) (any, bool) {
	if v, ok := r.Body[key]; ok {
		return v, true
	}
	if v, ok := c.GetPostForm(key); ok {
		return v, true
	}
	if v, ok := c.GetQuery(key); ok {
		return v, true
	}
	return nil, false
}

func (r *restRequest) Str(c *gin.Context, key string) (string, bool) {
	v, ok := r.get(c, key)
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case map[string]any:
		// {"raw": "..."} form used by the editor for title/content/excerpt.
		if raw, ok := t["raw"].(string); ok {
			return raw, true
		}
		if raw, ok := t["rendered"].(string); ok {
			return raw, true
		}
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	case nil:
		return "", true
	}
	return "", false
}

func (r *restRequest) Int(c *gin.Context, key string) (int64, bool) {
	v, ok := r.get(c, key)
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (r *restRequest) Bool(c *gin.Context, key string) (bool, bool) {
	v, ok := r.get(c, key)
	if !ok {
		return false, false
	}
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		return t == "true" || t == "1", true
	case float64:
		return t != 0, true
	}
	return false, false
}

// IDs reads an id list given as a JSON array, comma list or repeated key[] params.
func (r *restRequest) IDs(c *gin.Context, key string) ([]int64, bool) {
	var out []int64
	if v, ok := r.Body[key]; ok {
		if list, ok := v.([]any); ok {
			for _, x := range list {
				if f, ok := x.(float64); ok {
					out = append(out, int64(f))
				}
			}
			return out, true
		}
	}
	vals := c.QueryArray(key + "[]")
	if len(vals) == 0 {
		if v, ok := c.GetQuery(key); ok {
			vals = strings.Split(v, ",")
		}
	}
	if len(vals) == 0 {
		vals = c.PostFormArray(key + "[]")
	}
	for _, v := range vals {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out, len(vals) > 0
}

type restAPI struct {
	s      *Server
	routes []*restRoute
}

func (api *restAPI) register(namespace, pattern string, methods map[string]restHandler, args map[string]any) {
	expr := "^" + pattern + "$"
	api.routes = append(api.routes, &restRoute{Namespace: namespace, Pattern: pattern, re: regexp.MustCompile(expr), Methods: methods, Args: args})
}

// restUser authenticates REST requests: cookie + X-WP-Nonce (as WordPress requires for
// cookie auth) or HTTP Basic with an Application Password. route is the requested REST route and
// method the effective method (after X-HTTP-Method-Override / _method).
func (s *Server) restAuthUser(c *gin.Context, route, method string) (*store.User, error) {
	if h := c.GetHeader("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") { // OAuth access token (oauth.go)
		if !s.oauthBearer(c, strings.TrimSpace(h[7:])) {
			return nil, errors.New("invalid_token")
		}
		uid, _ := c.Get(restOAuthUserKey)
		u, err := s.store.UserByID(c, uid.(int64))
		if err != nil || u == nil || !appPasswordsAvailable(u) { // no API tokens for front-end members (admin_access.go)
			return nil, errors.New("invalid_token")
		}
		if s.adminsecBlocked(c, u) != "" { // a forced password change / expired 2FA blocks tokens too
			return nil, errors.New("adminsec_account_blocked")
		}
		return scopedUser(u, restScopes(c)), nil
	}
	if user, pass, ok := c.Request.BasicAuth(); ok {
		// Goocommerce REST API keys (goo_rest_auth.go) sign in to the store's own namespaces only, like
		// WC_REST_Authentication: elsewhere "ck_…" is just an unknown user name. The key's read / write
		// permission is checked against the effective method, so ?_method=DELETE needs a write key.
		if strings.HasPrefix(user, "ck_") && s.gooOn() && gooKeyRoute(route) {
			u, aerr := s.gooKeyAuth(c, user, pass, method)
			if aerr != nil {
				return nil, aerr // woocommerce_rest_authentication_error with its reason
			}
			return u, nil
		}
		if s.adminsecRESTLocked(c, user) { // AdminSecGo: block list and lockouts apply to the API too
			return nil, errors.New("adminsec_locked")
		}
		u, err := s.store.UserByLoginOrEmail(c, user)
		if err == nil && appPassKnown(u.ID, pass) {
			// A password that matched recently is not a guess: no throttle row, so parallel requests of a
			// working client cannot count against the limits. checkAppPassword still confirms it is stored.
			if s.checkAppPassword(c, u, pass) {
				if s.adminsecBlocked(c, u) != "" {
					return nil, errors.New("adminsec_account_blocked")
				}
				return scopedUser(u, restScopes(c)), nil
			}
		}
		// The login throttle applies to Basic auth too, counted before any hashing (app_password_auth.go).
		attemptID, refused := s.restBeginAttempt(c, user)
		if refused {
			return nil, errors.New("adminsec_locked")
		}
		if err != nil {
			auth.CheckDummyPassword(c.Request.Context(), pass) // no timing difference for unknown users
			s.restLoginFailed(c, user, nil, "Unknown user (REST API).", attemptID)
			return nil, errors.New("incorrect_password")
		}
		// Application Passwords skip the second factor: they are the way REST clients sign in.
		if s.checkAppPassword(c, u, pass) {
			s.auth.DropAttempt(c, attemptID) // not a failure
			if s.adminsecBlocked(c, u) != "" {
				return nil, errors.New("adminsec_account_blocked")
			}
			return scopedUser(u, restScopes(c)), nil // scopes only narrow the user's capabilities
		}
		s.restLoginFailed(c, user, u, "Wrong application password (REST API).", attemptID)
		return nil, errors.New("incorrect_password")
	}
	u := currentUser(c)
	if u == nil {
		return nil, nil
	}
	nonce := c.GetHeader("X-WP-Nonce")
	if nonce == "" {
		nonce = c.Query("_wpnonce")
	}
	if nonce == "" && strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		nonce = c.PostForm("_wpnonce") // $_REQUEST['_wpnonce'] of rest_cookie_check_errors (form posts)
	}
	if nonce == "" {
		// Cookie without nonce: treated as logged out, like WordPress (CSRF protection).
		return nil, nil
	}
	if !s.checkNonceValue(c, nonce, "wp_rest") {
		return nil, errors.New("rest_cookie_invalid_nonce")
	}
	// AdminSecGo: a session that must first change its password or set up the factor its role
	// requires gets no API either (only the HTML screens that fix it), or it could mint an
	// Application Password and keep API access without ever meeting the requirement.
	if s.adminsecBlocked(c, u) != "" {
		return nil, errors.New("adminsec_account_blocked")
	}
	return u, nil
}

// checkAppPassword verifies an Application Password (spaces are ignored, like WordPress). The most
// recently used password is tried first and a recent match is remembered (app_password_auth.go), so a
// working client normally costs one argon2 check per few minutes instead of one per stored password.
func (s *Server) checkAppPassword(c *gin.Context, u *store.User, pass string) bool {
	pass = strings.ReplaceAll(pass, " ", "")
	if !appPasswordsAvailable(u) {
		// A front-end member's passwords (made before this check, or kept from a higher role) do not sign in.
		return false
	}
	m, ok := s.matchAppPassword(c, u.ID, pass)
	if !ok {
		return false
	}
	full, scopes, refuse := appPasswordAccess(m.scopes)
	if refuse {
		// Scopes that are no longer known must not turn into content and media, or into full access.
		log.Printf("wpgo: application password %d of user %d has only unknown scopes %v; refused", m.id, u.ID, m.scopes)
		return false
	}
	c.Set(restAppPasswordKey, m.id) // the password in use (/application-passwords/introspect)
	if !full {
		c.Set(restAppScopesKey, scopes) // app_password_scopes.go
	}
	s.asyncExec(`UPDATE app_passwords SET last_used_at = now(), last_ip = $2 WHERE id = $1`, m.id, clientIP(c)) // bounded queue (background.go)
	return true
}

const (
	restUserKey        = "wpgo.restUser"
	restAppPasswordKey = "wpgo.restAppPassword" // app_passwords.id that authenticated the request
)

func restCurrent(c *gin.Context) *store.User {
	if v, ok := c.Get(restUserKey); ok && v != nil {
		return v.(*store.User)
	}
	return nil
}

// serve dispatches /wp-json/* and ?rest_route=.
func (api *restAPI) serve(c *gin.Context, route string) {
	s := api.s
	c.Header("Access-Control-Expose-Headers", "X-WP-Total, X-WP-TotalPages, Link")
	c.Header("Access-Control-Allow-Headers", "Authorization, X-WP-Nonce, Content-Disposition, Content-MD5, Content-Type")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Link", `<`+s.links(c).HomeURL("/wp-json/")+`>; rel="https://api.w.org/"`)

	// Method override is POST-only. A GET ?_method=DELETE (or a GET carrying
	// X-HTTP-Method-Override) must stay a GET: otherwise a link or an <img> can
	// turn a read into a write. On POST, ?_method= still wins over the header,
	// matching the previous dispatcher order.
	method := c.Request.Method
	if method == http.MethodPost {
		if m := c.GetHeader("X-HTTP-Method-Override"); m != "" {
			method = strings.ToUpper(m)
		}
		if m := c.Query("_method"); m != "" {
			method = strings.ToUpper(m)
		}
	}

	u, err := s.restAuthUser(c, route, method)
	if err != nil {
		var keyErr *gooAPIErr
		if errors.As(err, &keyErr) {
			gooFail(c, keyErr)
		} else if err.Error() == "rest_cookie_invalid_nonce" {
			restErr(c, http.StatusForbidden, "rest_cookie_invalid_nonce", "Cookie check failed")
		} else if err.Error() == "invalid_token" {
			c.Header("WWW-Authenticate", s.oauthChallenge(c, "invalid_token"))
			restErr(c, http.StatusUnauthorized, "invalid_token", "The access token is invalid or expired.")
		} else if err.Error() == "adminsec_account_blocked" {
			restErr(c, http.StatusForbidden, "adminsec_account_action_required", "Your account needs attention before it can use the REST API: change your password or set up the sign-in method your role requires.")
		} else if err.Error() == "adminsec_locked" {
			restErr(c, http.StatusTooManyRequests, "too_many_retries", "Too many failed login attempts, or access from this IP address is blocked.")
		} else {
			restErr(c, http.StatusUnauthorized, "incorrect_password", "The provided password is an invalid application password.")
		}
		return
	}
	if u != nil {
		c.Set(restUserKey, u)
		c.Header("X-WP-Nonce", s.nonce(c, "wp_rest"))
	}

	if s.adminsecRESTBlocked(c, route, method, u) { // AdminSecGo: user enumeration, REST only for logged-in users
		return
	}
	if route == "" || route == "/" {
		api.index(c)
		return
	}
	route = "/" + strings.Trim(route, "/")
	if !restScopeAllows(c, route) { // Application Password scopes (app_password_scopes.go)
		restErr(c, http.StatusForbidden, "rest_scope_forbidden", "This application password is not allowed to use this route.",
			map[string]any{"required_scope": appScopeForRoute(route), "scopes": restScopes(c)})
		return
	}
	builderOn := s.wbOn()
	for _, r := range append(api.routes[:len(api.routes):len(api.routes)], api.acfgoTypeRoutes()...) {
		if (!builderOn && r.Namespace == webbuilder.RESTNamespace) || s.gontactRESTHidden(r.Namespace) || (r.Namespace == pllRESTNamespace && !s.golylangOn()) || s.gooikeRESTHidden(r.Namespace) || s.pixelRESTHidden(r.Namespace) || s.forumRESTHidden(r.Namespace) {
			continue
		}
		m := r.re.FindStringSubmatch(route)
		if m == nil {
			continue
		}
		req := &restRequest{Params: map[string]string{}, Method: method}
		for i, name := range r.re.SubexpNames() {
			if name != "" {
				req.Params[name] = m[i]
			}
		}
		if method == http.MethodOptions {
			api.options(c, r)
			return
		}
		h, ok := r.Methods[method]
		if !ok && method == http.MethodPatch {
			h, ok = r.Methods[http.MethodPut]
		}
		if !ok && method == http.MethodPut {
			h, ok = r.Methods[http.MethodPost]
		}
		if !ok && method == http.MethodHead {
			h, ok = r.Methods[http.MethodGet]
		}
		if !ok {
			restErr(c, http.StatusNotFound, "rest_no_route", "No route was found matching the URL and request method.")
			return
		}
		if ct := c.ContentType(); ct == "application/json" && !restDecodeJSON(c, u, req) {
			return
		}
		if req.Body == nil {
			req.Body = map[string]any{}
		}
		s.golylangRESTParams(c, r, req) // Golylang: lang and translations sent in the body
		h(c, req)
		return
	}
	if (method == http.MethodGet || method == http.MethodHead) && api.namespaceIndex(c, route) {
		return
	}
	if s.servePluginRest(c, route, method) { // plugin routes (plugins_host.go), never shadowing core ones
		return
	}
	restErr(c, http.StatusNotFound, "rest_no_route", "No route was found matching the URL and request method.")
}

// REST JSON bodies are decoded into map[string]any before the handler runs, so before the handler's
// own permission check, and that decode costs many times the body for small values: 31 MB of "0,"
// once took 1 GB. How much is read therefore depends on who is asking. Visitors and front-end members
// (frontEndOnly: subscribers, customers, forum members, whom anyone can become on an open site) send
// comments, cart changes, consent and form posts and get restJSONSmallMax. Back-office users and API
// credentials get restJSONMax: the largest bodies a core route takes are below it (Web Builder publish
// 4 MB, design-kit files 24 MB, live canvas and AI 8 MB, base64 packages, batch/v1). Whoever sends it,
// a body holds at most restJSONMaxValues values, which bounds the decode to some 100 MB.
const (
	restJSONSmallMax  = 1 << 20
	restJSONMax       = 32 << 20
	restJSONMaxValues = 2 << 20
)

// restJSONLimit is the largest JSON body serve reads for u (nil: anonymous).
func restJSONLimit(u *store.User) int64 {
	if frontEndOnly(u) {
		return restJSONSmallMax
	}
	return restJSONMax
}

// restDecodeJSON reads the application/json body of the request into req.Body. A body over the
// caller's limit is refused with 413 before it is read (Content-Length) or as soon as the limit is
// passed (a chunked body), and before anything is decoded; so is a body with too many values. It
// reports false when it answered the request.
// restJSONHasNUL reports whether a JSON text holds a NUL character: a raw byte, or a \u0000 escape
// (an escaped backslash before "u0000" is no escape).
func restJSONHasNUL(body []byte) bool {
	if bytes.IndexByte(body, 0) >= 0 {
		return true
	}
	for i := 0; ; {
		j := bytes.Index(body[i:], []byte(`\u0000`))
		if j < 0 {
			return false
		}
		j += i
		n := 0 // backslashes in a row before this one: odd means it is itself escaped
		for k := j - 1; k >= 0 && body[k] == '\\'; k-- {
			n++
		}
		if n%2 == 0 {
			return true
		}
		i = j + 1
	}
}

func restDecodeJSON(c *gin.Context, u *store.User, req *restRequest) bool {
	limit := restJSONLimit(u)
	tooLarge := func() bool {
		restErr(c, http.StatusRequestEntityTooLarge, "rest_request_too_large", "The request body is too large.", map[string]any{"limit": limit})
		return false
	}
	if c.Request.ContentLength > limit {
		return tooLarge()
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if int64(len(body)) > limit {
		return tooLarge()
	}
	if err != nil {
		var maxErr *http.MaxBytesError // the server-wide body limit (ops_http.go)
		if errors.As(err, &maxErr) {
			return tooLarge()
		}
		restErr(c, http.StatusBadRequest, "rest_invalid_json", "Invalid JSON body passed.")
		return false
	}
	if len(body) == 0 {
		return true
	}
	// PostgreSQL stores no NUL in text (22021) and json.Unmarshal turns invalid UTF-8 into U+FFFD
	// silently: refuse both here instead of failing later with a 500.
	if !utf8.Valid(body) {
		restErr(c, http.StatusBadRequest, "rest_invalid_json", "The request body is not valid UTF-8.")
		return false
	}
	if restJSONHasNUL(body) {
		restErr(c, http.StatusBadRequest, "rest_invalid_json", "The request body must not contain NUL characters.")
		return false
	}
	if !jsonValuesWithin(body, restJSONMaxValues) {
		restErr(c, http.StatusRequestEntityTooLarge, "rest_request_too_large", "The request body holds too many values.", map[string]any{"limit_values": restJSONMaxValues})
		return false
	}
	if err := json.Unmarshal(body, &req.Body); err != nil {
		restErr(c, http.StatusBadRequest, "rest_invalid_json", "Invalid JSON body passed.")
		return false
	}
	return true
}

// jsonValuesWithin reports whether the JSON text b holds at most about max values, without
// allocating: it counts the arrays, objects and commas outside strings, which is the number of
// values decoding allocates give or take one per non-empty container. Malformed text is left to the
// decoder.
func jsonValuesWithin(b []byte, max int) bool {
	n, inString, escaped := 0, false, false
	for _, ch := range b {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
		case ch == '"':
			inString = true
		case ch == ',' || ch == '[' || ch == '{':
			if n++; n > max {
				return false
			}
		}
	}
	return true
}

// options answers OPTIONS with the Allow header the editor's canUser() checks.
func (api *restAPI) options(c *gin.Context, r *restRoute) {
	u := restCurrent(c)
	var methods []string
	for m := range r.Methods {
		if m != http.MethodGet && u == nil {
			continue
		}
		if m != http.MethodGet && !api.canWrite(u, r.Pattern) {
			continue
		}
		methods = append(methods, m)
	}
	sort.Strings(methods)
	if len(methods) == 0 {
		methods = []string{http.MethodGet}
	}
	c.Header("Allow", strings.Join(methods, ", "))
	var endpoints []map[string]any
	for m := range r.Methods {
		endpoints = append(endpoints, map[string]any{"methods": []string{m}, "args": map[string]any{}})
	}
	c.JSON(http.StatusOK, map[string]any{"namespace": r.Namespace, "methods": methods, "endpoints": endpoints})
}

// canWrite maps a route to the capability needed to create on it (used for Allow headers).
func (api *restAPI) canWrite(u *store.User, pattern string) bool {
	switch {
	case strings.HasPrefix(pattern, "/wp/v2/media"):
		return u.Can("upload_files", nil)
	case strings.HasPrefix(pattern, "/wp/v2/pages"):
		return u.Can("edit_pages", nil)
	case pattern == "/wp/v2/blocks": // creating a pattern: wp_block's create_posts is publish_posts
		return u.Can(restCreateCap("wp_block"), nil)
	case strings.HasPrefix(pattern, "/wp/v2/posts"), strings.HasPrefix(pattern, "/wp/v2/blocks"):
		return u.Can("edit_posts", nil)
	case strings.HasPrefix(pattern, "/wp/v2/settings"), strings.HasPrefix(pattern, "/wp/v2/global-styles"):
		return u.Can("manage_options", nil) || u.Can("edit_theme_options", nil)
	case strings.HasPrefix(pattern, "/wp/v2/categories"), strings.HasPrefix(pattern, "/wp/v2/tags"):
		return u.Can("manage_categories", nil) || u.Can("edit_posts", nil)
	case strings.HasPrefix(pattern, "/wp/v2/templates"):
		return false
	case strings.HasPrefix(pattern, "/wp/v2/users"):
		return u.Can("create_users", nil) || u.Can("edit_users", nil)
	case strings.HasPrefix(pattern, "/wp/v2/comments"):
		return u.Can("moderate_comments", nil) || u.Can("edit_posts", nil)
	case strings.HasPrefix(pattern, "/wp/v2/sidebars"), strings.HasPrefix(pattern, "/wp/v2/widgets"), strings.HasPrefix(pattern, "/wp/v2/widget-types"),
		strings.HasPrefix(pattern, "/wp/v2/menus"), strings.HasPrefix(pattern, "/wp/v2/menu-items"), strings.HasPrefix(pattern, "/wp/v2/menu-locations"):
		return u.Can("edit_theme_options", nil)
	}
	return u.Can("edit_posts", nil)
}

// indexRoutes lists the visible routes (the /wp-json index) and their namespaces.
func (api *restAPI) indexRoutes() (map[string]any, []string) {
	s := api.s
	routes := map[string]any{}
	nsSet := map[string]bool{}
	builderOn := s.wbOn()
	for _, r := range append(api.routes[:len(api.routes):len(api.routes)], api.acfgoTypeRoutes()...) {
		if (!builderOn && r.Namespace == webbuilder.RESTNamespace) || s.gontactRESTHidden(r.Namespace) || (r.Namespace == pllRESTNamespace && !s.golylangOn()) || s.forumRESTHidden(r.Namespace) || s.gooModuleRESTHidden(r.Pattern) {
			continue
		}
		var methods []string
		for m := range r.Methods {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		routes[r.Pattern] = map[string]any{"namespace": r.Namespace, "methods": methods}
		nsSet[r.Namespace] = true
	}
	for _, r := range s.pluginRestRoutes() { // plugin routes (plugins_host.go)
		routes[r.Path()] = map[string]any{"namespace": r.Namespace, "methods": r.Methods}
		nsSet[r.Namespace] = true
	}
	var namespaces []string
	for ns := range nsSet {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	return routes, namespaces
}

// namespaceIndex answers GET /wp-json/<namespace> as WP_REST_Server::get_namespace_index() does:
// the namespace and its routes, so a client can discover one API without the whole index.
func (api *restAPI) namespaceIndex(c *gin.Context, route string) bool {
	ns := strings.Trim(route, "/")
	routes, namespaces := api.indexRoutes()
	if !slices.Contains(namespaces, ns) {
		return false
	}
	own := map[string]any{"/" + ns: map[string]any{"namespace": ns, "methods": []string{http.MethodGet}}}
	for p, r := range routes {
		if r.(map[string]any)["namespace"] == ns {
			own[p] = r
		}
	}
	restJSON(c, http.StatusOK, map[string]any{"namespace": ns, "routes": own,
		"_links": map[string]any{"up": []map[string]string{{"href": api.s.links(c).HomeURL("/wp-json/")}}}})
	return true
}

func (api *restAPI) index(c *gin.Context) {
	s := api.s
	lb := s.links(c)
	routes, namespaces := api.indexRoutes()
	icon := ""
	out := map[string]any{
		"name": s.settings.String("blogname"), "description": s.settings.String("blogdescription"),
		"url": lb.Home, "home": lb.Home, "gmt_offset": s.settings.Float("gmt_offset"),
		"timezone_string": s.settings.String("timezone_string"), "page_for_posts": s.settings.Int("page_for_posts"),
		"page_on_front": s.settings.Int("page_on_front"), "show_on_front": s.settings.String("show_on_front"),
		"namespaces": namespaces, "authentication": map[string]any{"application-passwords": map[string]any{"endpoints": map[string]string{"authorization": lb.HomeURL("/wp-admin/authorize-application")}}},
		"routes": routes, "site_logo": 0, "site_icon": 0, "site_icon_url": icon,
		"_links": map[string]any{"help": []map[string]string{{"href": "https://developer.wordpress.org/rest-api/"}}},
	}
	restJSON(c, http.StatusOK, out)
}

// restJSON writes a response honouring _fields (comma list, dot paths allowed) and _envelope.
func restJSON(c *gin.Context, status int, v any) {
	if f := c.Query("_fields"); f != "" {
		v = filterFields(v, strings.Split(f, ","))
	}
	if _, ok := c.GetQuery("_envelope"); ok {
		headers := map[string]string{}
		for _, h := range []string{"X-WP-Total", "X-WP-TotalPages", "Link"} {
			if val := c.Writer.Header().Get(h); val != "" {
				headers[h] = val
			}
		}
		v = map[string]any{"body": v, "status": status, "headers": headers}
		status = http.StatusOK
	}
	c.JSON(status, v)
}

func filterFields(v any, fields []string) any {
	switch t := v.(type) {
	case []map[string]any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = filterFields(item, fields)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = filterFields(item, fields)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for _, f := range fields {
			f = strings.TrimSpace(f)
			parts := strings.SplitN(f, ".", 2)
			val, ok := t[parts[0]]
			if !ok {
				continue
			}
			if len(parts) == 2 {
				existing, _ := out[parts[0]].(map[string]any)
				sub := filterFields(val, []string{parts[1]})
				if sm, ok := sub.(map[string]any); ok {
					if existing == nil {
						existing = map[string]any{}
					}
					for k, x := range sm {
						existing[k] = x
					}
					out[parts[0]] = existing
				}
				continue
			}
			out[parts[0]] = val
		}
		return out
	}
	return v
}

// paginate sets X-WP-Total, X-WP-TotalPages and Link headers for a collection.
func paginate(c *gin.Context, total, perPage, page int) {
	pages := 1
	if perPage > 0 {
		pages = (total + perPage - 1) / perPage
	}
	if pages < 1 {
		pages = 1
	}
	c.Header("X-WP-Total", strconv.Itoa(total))
	c.Header("X-WP-TotalPages", strconv.Itoa(pages))
	var links []string
	u := *c.Request.URL
	mk := func(p int) string {
		q := u.Query()
		q.Set("page", strconv.Itoa(p))
		u.RawQuery = q.Encode()
		return "<" + u.String() + ">"
	}
	if page > 1 {
		links = append(links, mk(page-1)+`; rel="prev"`)
	}
	if page < pages {
		links = append(links, mk(page+1)+`; rel="next"`)
	}
	if len(links) > 0 {
		c.Header("Link", strings.Join(links, ", "))
	}
}

// pageArgs reads page/per_page with WordPress' bounds; per_page=-1 (editor fetch-all) is capped at 100 per page.
func pageArgs(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	per, _ := strconv.Atoi(c.DefaultQuery("per_page", "10"))
	if per <= 0 || per > 100 {
		per = 100
	}
	return page, per
}

// WordPress' orderby enums for the core collections (each controller's get_collection_params()).
var (
	restPostOrderBy    = []string{"author", "date", "id", "include", "modified", "parent", "relevance", "slug", "include_slugs", "title", "menu_order"}
	restTermOrderBy    = []string{"id", "include", "name", "slug", "include_slugs", "term_group", "description", "count"}
	restUserOrderBy    = []string{"id", "include", "name", "registered_date", "slug", "include_slugs", "email", "url"}
	restCommentOrderBy = []string{"date", "date_gmt", "id", "include", "post", "parent", "type"}
)

// listArgs is pageArgs for the wp/v2 collections: like WordPress, a page, per_page, order or
// orderby outside the schema is a 400 rest_invalid_param naming every bad parameter, instead of
// being silently replaced (a client would otherwise believe it got what it asked for).
// per_page=-1 stays accepted: older editor code asks for "everything" that way.
func listArgs(c *gin.Context, orderby []string) (int, int, bool) {
	bad := map[string]string{}
	var names []string
	fail := func(k, msg string) {
		if _, ok := bad[k]; !ok {
			names = append(names, k)
		}
		bad[k] = msg
	}
	if v, ok := c.GetQuery("page"); ok {
		if n, err := strconv.Atoi(v); err != nil {
			fail("page", "page is not of type integer.")
		} else if n < 1 {
			fail("page", "page must be greater than or equal to 1")
		}
	}
	if v, ok := c.GetQuery("per_page"); ok {
		if n, err := strconv.Atoi(v); err != nil {
			fail("per_page", "per_page is not of type integer.")
		} else if n != -1 && (n < 1 || n > 100) {
			fail("per_page", "per_page must be between 1 (inclusive) and 100 (inclusive)")
		}
	}
	if v, ok := c.GetQuery("order"); ok && v != "asc" && v != "desc" {
		fail("order", "order is not one of asc and desc.")
	}
	if v, ok := c.GetQuery("orderby"); ok && orderby != nil && !slices.Contains(orderby, v) {
		fail("orderby", "orderby is not one of "+strings.Join(orderby[:len(orderby)-1], ", ")+" and "+orderby[len(orderby)-1]+".")
	}
	if len(names) > 0 {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): "+strings.Join(names, ", "), map[string]any{"params": bad})
		return 0, 0, false
	}
	page, per := pageArgs(c)
	return page, per, true
}

func restContext(c *gin.Context) string {
	return c.DefaultQuery("context", "view")
}

func restLinks(self, collection string, extra map[string]any) map[string]any {
	l := map[string]any{
		"self":       []map[string]any{{"href": self}},
		"collection": []map[string]any{{"href": collection}},
	}
	for k, v := range extra {
		l[k] = v
	}
	return l
}
