package wpgo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Shortcodes --------------------------------------------------------------------

// ShortcodeFunc renders a shortcode. attrs holds the named attributes (keys
// lower-cased) plus positional ones under "0", "1", …; content is the text
// between [tag] and [/tag], or "" for a self-closing tag. The returned HTML
// replaces the shortcode in the content.
type ShortcodeFunc func(ctx Context, attrs map[string]string, content string) string

// AddShortcode registers fn for [tag]. The tag must be listed in plugin.json
// "shortcodes" and the plugin needs the "content:filter" permission. The core
// expands shortcodes in the_content after blocks are rendered.
func AddShortcode(tag string, fn ShortcodeFunc) {
	name := addCallback("shortcode", tag, func(ctx Context, _ json.RawMessage, _ []any, raw []json.RawMessage) (json.RawMessage, error) {
		attrs := map[string]string{}
		content := ""
		if len(raw) > 0 {
			_ = json.Unmarshal(raw[0], &attrs)
		}
		if len(raw) > 1 {
			_ = json.Unmarshal(raw[1], &content)
		}
		return json.Marshal(fn(ctx, attrs, content))
	})
	queueRegistration(func() {
		if _, err := hostCall("register_shortcode", map[string]string{"tag": tag, "callback": name}); err != nil {
			Log("error", "register_shortcode "+tag+": "+err.Error())
		}
	})
}

// REST routes -------------------------------------------------------------------

// RestRequest is an incoming request to a plugin REST route.
type RestRequest struct {
	// Method is the HTTP method (GET, POST, …).
	Method string `json:"method"`
	// Route is the matched path, e.g. /my-plugin/v1/items/7.
	Route string `json:"route"`
	// Params holds the named groups of the route pattern.
	Params map[string]string `json:"params"`
	// Query holds the URL query parameters (first value each).
	Query map[string]string `json:"query"`
	// Headers is a subset of the request headers (Accept, Content-Type,
	// Accept-Language, User-Agent, Referer, Origin, X-Requested-With).
	Headers map[string]string `json:"headers"`
	// Body is the decoded JSON body, or a string for other content types.
	Body json.RawMessage `json:"body"`
	// User is the authenticated user (cookie + nonce or Application
	// Password), nil for anonymous requests.
	User *User `json:"user"`
}

// BindJSON decodes the JSON request body into v.
func (r *RestRequest) BindJSON(v any) error {
	if len(r.Body) == 0 || string(r.Body) == "null" {
		return errors.New("wpgo: request has no JSON body")
	}
	return json.Unmarshal(r.Body, v)
}

// Param returns a named path parameter.
func (r *RestRequest) Param(name string) string { return r.Params[name] }

// RestResponse is what a route handler returns.
type RestResponse struct {
	Status  int               `json:"status"`
	Body    json.RawMessage   `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// JSON builds a response with a JSON body.
func JSON(status int, v any) *RestResponse {
	body, err := json.Marshal(v)
	if err != nil {
		return RestError(http.StatusInternalServerError, "rest_encode_error", err.Error())
	}
	return &RestResponse{Status: status, Body: body}
}

// RestError builds a WordPress-style error response {"code","message","data":{"status"}}.
func RestError(status int, code, message string) *RestResponse {
	return JSON(status, map[string]any{"code": code, "message": message, "data": map[string]any{"status": status}})
}

// RestHandler serves a plugin REST route.
type RestHandler func(ctx Context, req *RestRequest) *RestResponse

// RegisterRestRoute registers handler at /wp-json/<namespace><route>. The
// namespace must be the plugin slug or "<slug>/v1" (any version number), so
// plugins cannot shadow core routes; route is a path pattern starting with
// "/" that may contain named groups, e.g. `/items/(?P<id>\d+)`. methods are
// the HTTP methods the route accepts (GET when empty). Requires the
// "rest:routes" permission.
//
// The core authenticates the request like any other REST request; the
// handler decides what anonymous users may do by checking req.User.
func RegisterRestRoute(namespace, route string, methods []string, handler RestHandler) {
	name := addCallback("rest", strings.Trim(namespace, "/")+route, func(ctx Context, _ json.RawMessage, _ []any, raw []json.RawMessage) (json.RawMessage, error) {
		var req RestRequest
		if len(raw) > 0 {
			if err := json.Unmarshal(raw[0], &req); err != nil {
				return nil, fmt.Errorf("decode REST request: %w", err)
			}
		}
		resp := handler(ctx, &req)
		if resp == nil {
			resp = &RestResponse{Status: http.StatusNoContent}
		}
		return json.Marshal(resp)
	})
	if len(methods) == 0 {
		methods = []string{http.MethodGet}
	}
	queueRegistration(func() {
		_, err := hostCall("register_rest_route", map[string]any{"namespace": namespace, "route": route, "methods": methods, "callback": name})
		if err != nil {
			Log("error", "register_rest_route "+route+": "+err.Error())
		}
	})
}

// Posts -------------------------------------------------------------------------

// Post is a post as returned by the host.
type Post struct {
	ID       int64             `json:"id"`
	Title    string            `json:"title"`
	Content  string            `json:"content"`
	Excerpt  string            `json:"excerpt"`
	Status   string            `json:"status"`
	Type     string            `json:"type"`
	Slug     string            `json:"slug"`
	Date     string            `json:"date"`     // RFC 3339
	Modified string            `json:"modified"` // RFC 3339
	Author   int64             `json:"author"`
	Link     string            `json:"link,omitempty"`
	Terms    map[string][]Term `json:"terms"` // by taxonomy: category, post_tag
	Meta     map[string]any    `json:"meta,omitempty"`
}

// Term is a term attached to a post.
type Term struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Taxonomy string `json:"taxonomy"`
}

// PostQuery selects posts for QueryPosts. Zero values mean: type post,
// status publish, ordered by date descending, 10 per page, page 1.
type PostQuery struct {
	Type    string `json:"type,omitempty"`
	Status  string `json:"status,omitempty"` // publish, draft, pending, private, future, trash or any
	Search  string `json:"search,omitempty"`
	Author  int64  `json:"author,omitempty"`
	Term    int64  `json:"term,omitempty"`
	OrderBy string `json:"orderby,omitempty"` // date, title, modified, id
	Order   string `json:"order,omitempty"`   // ASC or DESC
	PerPage int    `json:"per_page,omitempty"`
	Page    int    `json:"page,omitempty"`
}

// PostList is the result of QueryPosts.
type PostList struct {
	Posts []Post `json:"posts"`
	Total int    `json:"total"`
	Pages int    `json:"pages"`
}

// PostInput carries the fields InsertPost and UpdatePost set. Nil fields are
// left alone (update) or take defaults (insert: type post, status draft).
type PostInput struct {
	Title   *string        `json:"title,omitempty"`
	Content *string        `json:"content,omitempty"`
	Excerpt *string        `json:"excerpt,omitempty"`
	Status  *string        `json:"status,omitempty"`
	Type    *string        `json:"type,omitempty"`
	Slug    *string        `json:"slug,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"` // merged into the post meta; nil values delete keys
}

// String returns a pointer for PostInput fields.
func String(s string) *string { return &s }

// GetPost returns a post by id; found is false when it does not exist.
// Requires the "posts:read" permission.
func GetPost(id int64) (post *Post, found bool, err error) {
	res, err := hostCall("get_post", map[string]int64{"id": id})
	if err != nil {
		return nil, false, err
	}
	var out struct {
		Found bool  `json:"found"`
		Post  *Post `json:"post"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, false, fmt.Errorf("wpgo: decode get_post result: %w", err)
	}
	return out.Post, out.Found, nil
}

// QueryPosts lists posts matching q. Requires the "posts:read" permission.
func QueryPosts(q PostQuery) (*PostList, error) {
	res, err := hostCall("query_posts", q)
	if err != nil {
		return nil, err
	}
	var out PostList
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("wpgo: decode query_posts result: %w", err)
	}
	return &out, nil
}

// InsertPost creates a post and returns it. Content is sanitised like a post
// written by a user without unfiltered_html. Requires "posts:write".
func InsertPost(in PostInput) (*Post, error) {
	res, err := hostCall("insert_post", in)
	if err != nil {
		return nil, err
	}
	var out Post
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("wpgo: decode insert_post result: %w", err)
	}
	return &out, nil
}

// UpdatePost changes the set fields of post id and returns the updated post.
// Requires the "posts:write" permission.
func UpdatePost(id int64, in PostInput) (*Post, error) {
	req := struct {
		ID int64 `json:"id"`
		PostInput
	}{id, in}
	res, err := hostCall("update_post", req)
	if err != nil {
		return nil, err
	}
	var out Post
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("wpgo: decode update_post result: %w", err)
	}
	return &out, nil
}

// Users -------------------------------------------------------------------------

// User is the current user as seen by plugins.
type User struct {
	ID          int64    `json:"id"`
	Login       string   `json:"login"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
	Caps        []string `json:"caps"`
}

// Can reports whether the user has a capability.
func (u *User) Can(capability string) bool {
	if u == nil {
		return false
	}
	for _, c := range u.Caps {
		if c == capability {
			return true
		}
	}
	return false
}

// CurrentUser returns the user of the current request, or nil for an
// anonymous visitor (and in cron jobs). Requires the "users:read" permission.
func CurrentUser() (*User, error) {
	res, err := hostCall("get_current_user", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Found bool  `json:"found"`
		User  *User `json:"user"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("wpgo: decode get_current_user result: %w", err)
	}
	if !out.Found {
		return nil, nil
	}
	return out.User, nil
}

// Scheduled events --------------------------------------------------------------

// ScheduleEvent runs the action hook every interval (at least a minute),
// starting one interval from now. hook must start with "cron_", be listed in
// plugin.json "hooks" and have an action registered with AddAction; args is
// passed to it as its single argument. Scheduling a hook that is already
// scheduled replaces the schedule. Requires the "cron" permission.
func ScheduleEvent(hook string, interval time.Duration, args map[string]any) error {
	_, err := hostCall("schedule_event", map[string]any{"hook": hook, "interval": int64(interval / time.Second), "args": args})
	return err
}

// ScheduleSingleEvent runs the action hook once at the given time (now when
// zero). Requires the "cron" permission.
func ScheduleSingleEvent(hook string, at time.Time, args map[string]any) error {
	var unix int64
	if !at.IsZero() {
		unix = at.Unix()
	}
	_, err := hostCall("schedule_event", map[string]any{"hook": hook, "interval": 0, "at": unix, "args": args})
	return err
}

// UnscheduleEvent removes every pending run of hook. Requires "cron".
func UnscheduleEvent(hook string) error {
	_, err := hostCall("unschedule_event", map[string]string{"hook": hook})
	return err
}

// Settings pages ----------------------------------------------------------------

// SettingsPage describes a page the core renders under Settings in the admin.
// Values are saved as site options named plugin_<slug>_<field id> (dashes in
// the slug become underscores) and read back with Setting* or GetOption.
type SettingsPage struct {
	PageTitle   string          `json:"page_title"`
	MenuTitle   string          `json:"menu_title"`
	Description string          `json:"description,omitempty"`
	Fields      []SettingsField `json:"fields"`
}

// SettingsField is one input on a SettingsPage.
type SettingsField struct {
	ID          string           `json:"id"` // lowercase letters, digits and _
	Label       string           `json:"label"`
	Type        string           `json:"type"` // text, textarea, number, checkbox, select
	Description string           `json:"description,omitempty"`
	Placeholder string           `json:"placeholder,omitempty"`
	Default     any              `json:"default,omitempty"`
	Options     []SettingsOption `json:"options,omitempty"` // select
	Min         *float64         `json:"min,omitempty"`     // number
	Max         *float64         `json:"max,omitempty"`     // number
	Rows        int              `json:"rows,omitempty"`    // textarea
}

// SettingsOption is a choice of a select field.
type SettingsOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

var (
	settingsMu     sync.Mutex
	settingsPrefix string
)

// RegisterSettingsPage registers the plugin's settings page. Requires the
// "admin:settings" permission; reading the values needs "options:read".
func RegisterSettingsPage(page SettingsPage) {
	queueRegistration(func() {
		res, err := hostCall("register_settings_page", page)
		if err != nil {
			Log("error", "register_settings_page: "+err.Error())
			return
		}
		var out struct {
			Prefix string `json:"option_prefix"`
		}
		if json.Unmarshal(res, &out) == nil && out.Prefix != "" {
			settingsMu.Lock()
			settingsPrefix = out.Prefix
			settingsMu.Unlock()
		}
	})
}

// SettingName returns the option name of a settings field, once
// RegisterSettingsPage has run; before that it returns "".
func SettingName(field string) string {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if settingsPrefix == "" {
		return ""
	}
	return settingsPrefix + field
}

// Setting returns the saved value of a settings field as a string ("" and
// false when it was never saved). Requires the "options:read" permission.
func Setting(field string) (string, bool, error) {
	name := SettingName(field)
	if name == "" {
		return "", false, errors.New("wpgo: RegisterSettingsPage has not run yet")
	}
	return GetOption(name)
}

// SettingString returns a text, textarea or select field, or def.
func SettingString(field, def string) string {
	v, ok, err := Setting(field)
	if err != nil || !ok {
		return def
	}
	return v
}

// SettingBool returns a checkbox field, or def.
func SettingBool(field string, def bool) bool {
	name := SettingName(field)
	if name == "" {
		return def
	}
	return OptionBool(name, def)
}

// SettingFloat returns a number field, or def.
func SettingFloat(field string, def float64) float64 {
	v, ok, err := Setting(field)
	if err != nil || !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// Transients --------------------------------------------------------------------

// TransientStore is the plugin-scoped transient cache (values expire).
type TransientStore struct{}

// Transients caches JSON values with a time-to-live, like set_transient().
// Requires the "storage" permission.
var Transients TransientStore

// Get decodes the transient key into v and reports whether it existed.
func (TransientStore) Get(key string, v any) (bool, error) {
	res, err := hostCall("transient_get", map[string]string{"key": key})
	if err != nil {
		return false, err
	}
	var out struct {
		Found bool            `json:"found"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return false, fmt.Errorf("wpgo: decode transient_get result: %w", err)
	}
	if !out.Found {
		return false, nil
	}
	return true, json.Unmarshal(out.Value, v)
}

// Set stores value (JSON-encoded) under key for ttl; 0 means no expiry.
func (TransientStore) Set(key string, value any, ttl time.Duration) error {
	_, err := hostCall("transient_set", map[string]any{"key": key, "value": value, "ttl": int64(ttl / time.Second)})
	return err
}

// Delete removes a transient.
func (TransientStore) Delete(key string) error {
	_, err := hostCall("transient_delete", map[string]string{"key": key})
	return err
}

// Assets ------------------------------------------------------------------------

// Where an enqueued asset is loaded.
const (
	Front = "front"
	Admin = "admin"
	Both  = "both"
)

// Style describes a stylesheet from the package's assets/ directory.
type Style struct {
	Handle string   // unique name, e.g. "my-plugin"
	Src    string   // path inside assets/, e.g. "style.css"
	Deps   []string // handles of this plugin's styles loaded first
	Where  string   // Front (default), Admin or Both
	Media  string   // media attribute, default "all"
}

// Script describes a script from the package's assets/ directory.
type Script struct {
	Handle   string
	Src      string
	Deps     []string
	Where    string
	InFooter bool
}

// EnqueueStyle adds a stylesheet to the front end and/or admin pages. It is
// served at /wp-content/plugins/<slug>/assets/<src>. Call it from init or
// OnInit. Requires the "content:filter" permission.
func EnqueueStyle(s Style) {
	queueRegistration(func() {
		if _, err := hostCall("enqueue_style", map[string]any{"handle": s.Handle, "src": s.Src, "deps": s.Deps, "where": s.Where, "media": s.Media}); err != nil {
			Log("error", "enqueue_style "+s.Handle+": "+err.Error())
		}
	})
}

// EnqueueScript adds a script to the front end and/or admin pages. Requires
// the "content:filter" permission.
func EnqueueScript(s Script) {
	queueRegistration(func() {
		if _, err := hostCall("enqueue_script", map[string]any{"handle": s.Handle, "src": s.Src, "deps": s.Deps, "where": s.Where, "in_footer": s.InFooter}); err != nil {
			Log("error", "enqueue_script "+s.Handle+": "+err.Error())
		}
	})
}
