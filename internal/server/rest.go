package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

// The REST API mirrors WordPress' wp/v2 namespace closely enough for the block editor
// (Gotenberg) and headless clients written against WordPress to work unchanged.

type restError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func restErr(c *gin.Context, status int, code, msg string, extra ...map[string]any) {
	data := map[string]any{"status": status}
	for _, e := range extra {
		for k, v := range e {
			data[k] = v
		}
	}
	c.AbortWithStatusJSON(status, restError{Code: code, Message: msg, Data: data})
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
// cookie auth) or HTTP Basic with an Application Password.
func (s *Server) restAuthUser(c *gin.Context) (*store.User, error) {
	if user, pass, ok := c.Request.BasicAuth(); ok {
		u, err := s.store.UserByLoginOrEmail(c, user)
		if err != nil {
			return nil, errors.New("incorrect_password")
		}
		if s.checkAppPassword(c, u, pass) {
			return u, nil
		}
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
	if nonce == "" {
		// Cookie without nonce: treated as logged out, like WordPress (CSRF protection).
		return nil, nil
	}
	if !s.checkNonceValue(c, nonce, "wp_rest") {
		return nil, errors.New("rest_cookie_invalid_nonce")
	}
	return u, nil
}

// checkAppPassword verifies an Application Password (spaces are ignored, like WordPress).
func (s *Server) checkAppPassword(c *gin.Context, u *store.User, pass string) bool {
	pass = strings.ReplaceAll(pass, " ", "")
	rows, err := s.store.Pool.Query(c, `SELECT id, password_hash FROM app_passwords WHERE user_id = $1`, u.ID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var hash string
		if rows.Scan(&id, &hash) != nil {
			continue
		}
		if ok, _ := auth.CheckPassword(pass, hash); ok {
			c.Set(restAppPasswordKey, id) // the password in use (/application-passwords/introspect)
			go s.store.Pool.Exec(c.Copy(), `UPDATE app_passwords SET last_used_at = now(), last_ip = $2 WHERE id = $1`, id, clientIP(c))
			return true
		}
	}
	return false
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

	method := c.Request.Method
	if m := c.GetHeader("X-HTTP-Method-Override"); m != "" && method == http.MethodPost {
		method = strings.ToUpper(m)
	}
	if m := c.Query("_method"); m != "" {
		method = strings.ToUpper(m)
	}

	u, err := s.restAuthUser(c)
	if err != nil {
		if err.Error() == "rest_cookie_invalid_nonce" {
			restErr(c, http.StatusForbidden, "rest_cookie_invalid_nonce", "Cookie check failed")
		} else {
			restErr(c, http.StatusUnauthorized, "incorrect_password", "The provided password is an invalid application password.")
		}
		return
	}
	if u != nil {
		c.Set(restUserKey, u)
		c.Header("X-WP-Nonce", s.nonce(c, "wp_rest"))
	}

	if route == "" || route == "/" {
		api.index(c)
		return
	}
	route = "/" + strings.Trim(route, "/")
	builderOn := s.golementorOn()
	for _, r := range api.routes {
		if !builderOn && gweRESTNamespace(r.Namespace) {
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
		if ct := c.ContentType(); ct == "application/json" {
			body, _ := io.ReadAll(io.LimitReader(c.Request.Body, 32<<20))
			if len(body) > 0 {
				if err := json.Unmarshal(body, &req.Body); err != nil {
					restErr(c, http.StatusBadRequest, "rest_invalid_json", "Invalid JSON body passed.")
					return
				}
			}
		}
		if req.Body == nil {
			req.Body = map[string]any{}
		}
		h(c, req)
		return
	}
	restErr(c, http.StatusNotFound, "rest_no_route", "No route was found matching the URL and request method.")
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

func (api *restAPI) index(c *gin.Context) {
	s := api.s
	lb := s.links(c)
	routes := map[string]any{}
	nsSet := map[string]bool{}
	builderOn := s.golementorOn()
	for _, r := range api.routes {
		if !builderOn && gweRESTNamespace(r.Namespace) {
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
	var namespaces []string
	for ns := range nsSet {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
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

func escapeQuery(v string) string { return url.QueryEscape(v) }
