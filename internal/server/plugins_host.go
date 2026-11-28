package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/plugins"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// The second half of the plugin Host (plugins.go has options, HTTP, storage and notices): posts,
// the current user, scheduled events and transients, plus the core side of the registrations
// plugins make at init: shortcodes in the_content, REST routes under /wp-json/<slug>/…, a settings
// page under Settings, and package assets served at /wp-content/plugins/<slug>/assets/… and
// enqueued on the front end or in the admin.

// pluginUserKey carries the request's user in the context plugins see (set by loadUser), so that
// host functions called from hooks, shortcodes and REST handlers can answer get_current_user.
type pluginUserKey struct{}

// pluginScopeKey is "request" for an HTTP request (including an anonymous one) and "elevated"
// for a trusted background run (scheduled events). A missing scope is not a visitor request:
// cron-like callers may draft posts, but publishing still needs an elevated context or a user
// who can publish. posts:write on its own does not elevate an anonymous request.
type pluginScopeKey struct{}

const (
	pluginScopeRequest  = "request"
	pluginScopeElevated = "elevated"
)

func withPluginUser(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, pluginUserKey{}, u)
}

// withPluginRequest marks ctx as a visitor request and sets the user plugins act as (nil when
// the visitor is anonymous). Host writes then require that user's capabilities.
func withPluginRequest(ctx context.Context, u *store.User) context.Context {
	ctx = context.WithValue(ctx, pluginScopeKey{}, pluginScopeRequest)
	return withPluginUser(ctx, u)
}

// withPluginElevated marks ctx as a trusted background run. Publishing without a current user
// is allowed only here, not merely because the plugin holds posts:write.
func withPluginElevated(ctx context.Context) context.Context {
	return context.WithValue(ctx, pluginScopeKey{}, pluginScopeElevated)
}

func pluginUserFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(pluginUserKey{}).(*store.User)
	return u
}

func pluginScope(ctx context.Context) string {
	s, _ := ctx.Value(pluginScopeKey{}).(string)
	return s
}

func pluginPublicStatus(status string) bool {
	return status == "publish" || status == "future" || status == "private"
}

func userInfo(u *store.User) *plugins.UserInfo {
	if u == nil {
		return nil
	}
	info := &plugins.UserInfo{ID: u.ID, Login: u.Login, DisplayName: u.DisplayName, Roles: append([]string{}, u.Roles...), Caps: []string{}}
	for c, ok := range u.Caps {
		if ok {
			info.Caps = append(info.Caps, c)
		}
	}
	sort.Strings(info.Caps)
	return info
}

// Posts --------------------------------------------------------------------------

// pluginPostTypes are the post types plugins may read and write.
var pluginPostTypes = map[string]bool{"post": true, "page": true}

func (h *pluginHost) postData(ctx context.Context, p *store.Post) *plugins.PostData {
	s := h.s
	out := &plugins.PostData{
		ID: p.ID, Title: p.Title, Content: p.Content, Excerpt: p.Excerpt, Status: p.Status, Type: p.Type, Slug: p.Slug,
		Date: p.Date().In(s.loc()).Format(time.RFC3339), Modified: p.ModifiedAt.In(s.loc()).Format(time.RFC3339),
		Author: p.AuthorID, Terms: map[string][]plugins.TermData{}, Meta: p.Meta,
		ACF: s.acfgoPluginPostACF(ctx, p), // ACF-Go field values
	}
	if p.Status == "publish" {
		out.Link = s.links(ctx).Permalink(p)
	}
	for _, tax := range []string{"category", "post_tag"} {
		list := []plugins.TermData{}
		if terms, err := s.store.TermsForPosts(ctx, []int64{p.ID}, tax); err == nil {
			for _, t := range terms[p.ID] {
				list = append(list, plugins.TermData{ID: t.ID, Name: t.Name, Slug: t.Slug, Taxonomy: t.Taxonomy})
			}
		}
		out.Terms[tax] = list
	}
	return out
}

// authorizePostWrite is the capability check for insert_post and update_post. Scheduled events
// (elevated) may write any post. Elsewhere the plugin acts as the request's user (admin pages,
// widgets and meta boxes run inside the request); outside a request (init, lifecycle hooks) there
// is no user: a draft or pending post may be created, but editing an existing post or publishing
// needs a user allowed to do it. before is the stored post on update and nil on insert.
func (h *pluginHost) authorizePostWrite(ctx context.Context, before, next *store.Post) error {
	if pluginScope(ctx) == pluginScopeElevated {
		return nil
	}
	publishing := pluginPublicStatus(next.Status) && (before == nil || !pluginPublicStatus(before.Status))
	if pluginScope(ctx) != pluginScopeRequest {
		if publishing {
			return errors.New("publishing requires an elevated context or a user who can publish")
		}
		if before != nil {
			return errors.New("editing an existing post requires a scheduled event or a user who can edit it")
		}
		return nil
	}
	u := pluginUserFrom(ctx)
	if before != nil {
		if u == nil || !u.Can("edit_post", before) {
			return errors.New("the current user is not allowed to edit this post")
		}
	}
	// A new draft or pending post is allowed for anyone: front-end submission plugins create
	// them on a visitor's request. Publishing it is checked below.
	if publishing {
		capName := "publish_posts"
		if next.Type == "page" {
			capName = "publish_pages"
		}
		if u == nil || !u.Can(capName, nil) {
			return errors.New("the current user is not allowed to publish")
		}
	}
	return nil
}

// authorizeMail is the check for send_mail. mail:send is granted at install time and contact form
// plugins send a notification on an anonymous visitor's request, so every scope may send.
func (h *pluginHost) authorizeMail(ctx context.Context) error {
	return nil
}

func (h *pluginHost) GetPost(ctx context.Context, _ string, id int64) (*plugins.PostData, error) {
	p, err := h.s.store.GetPost(ctx, id)
	if err != nil || p == nil || !pluginPostTypes[p.Type] {
		return nil, nil
	}
	return h.postData(ctx, p), nil
}

func (h *pluginHost) QueryPosts(ctx context.Context, _ string, q plugins.PostQuery) (*plugins.PostList, error) {
	typ := q.Type
	if typ == "" {
		typ = "post"
	}
	if !pluginPostTypes[typ] {
		return &plugins.PostList{Posts: []plugins.PostData{}}, nil
	}
	sq := store.PostQuery{Types: []string{typ}, Search: q.Search, AuthorID: q.Author, OrderBy: q.OrderBy, Order: q.Order,
		Page: q.Page, PerPage: q.PerPage, Loc: h.s.loc()}
	switch q.Status {
	case "":
		sq.Statuses = []string{"publish"}
	case "any":
		sq.Statuses = []string{"publish", "draft", "pending", "private", "future"}
	default:
		sq.Statuses = []string{q.Status}
	}
	if q.Term != 0 {
		sq.TermIDs = []int64{q.Term}
	}
	posts, total, err := h.s.store.QueryPosts(ctx, sq)
	if err != nil {
		return nil, err
	}
	out := &plugins.PostList{Posts: make([]plugins.PostData, 0, len(posts)), Total: total}
	if q.PerPage > 0 {
		out.Pages = (total + q.PerPage - 1) / q.PerPage
	}
	for _, p := range posts {
		out.Posts = append(out.Posts, *h.postData(ctx, p))
	}
	return out, nil
}

// applyPostInput copies the set fields of in onto p with the admin's sanitisation: plugins never
// hold unfiltered_html, so content, title and excerpt go through KSES.
func applyPostInput(p *store.Post, in plugins.PostInput) error {
	if in.Type != nil {
		if !pluginPostTypes[*in.Type] {
			return fmt.Errorf("post type %q is not available to plugins", *in.Type)
		}
		p.Type = *in.Type
	}
	if in.Title != nil {
		p.Title = wp.KsesPost(strings.TrimSpace(*in.Title))
	}
	if in.Content != nil {
		p.Content = wp.KsesPost(strings.ReplaceAll(*in.Content, "\r\n", "\n"))
	}
	if in.Excerpt != nil {
		p.Excerpt = wp.KsesPost(*in.Excerpt)
	}
	if in.Status != nil {
		if *in.Status == "trash" {
			return errors.New("use the admin to trash posts")
		}
		p.Status = *in.Status
	}
	if in.Slug != nil {
		p.Slug = wp.SanitizeTitle(*in.Slug)
	}
	if len(in.Meta) > 0 {
		if p.Meta == nil {
			p.Meta = map[string]any{}
		}
		for k := range in.Meta {
			if strings.HasPrefix(k, "_") {
				// Keys starting with an underscore are protected: the core and other plugins store trusted data
				// there (the page builder's element tree is rendered unescaped), and plugins never hold unfiltered_html.
				return fmt.Errorf("meta key %q is protected (keys starting with an underscore are not available to plugins)", k)
			}
		}
		for k, v := range in.Meta {
			if v == nil {
				delete(p.Meta, k)
			} else {
				p.Meta[k] = v
			}
		}
	}
	return nil
}

func (h *pluginHost) InsertPost(ctx context.Context, _ string, in plugins.PostInput) (*plugins.PostData, error) {
	p := &store.Post{Type: "post", Status: "draft", CommentStatus: h.s.settings.String("default_comment_status"), PingStatus: h.s.settings.String("default_ping_status")}
	if u := pluginUserFrom(ctx); u != nil {
		p.AuthorID = u.ID
	}
	if err := applyPostInput(p, in); err != nil {
		return nil, err
	}
	if err := h.authorizePostWrite(ctx, nil, p); err != nil {
		return nil, err
	}
	if err := h.s.store.InsertPost(ctx, p); err != nil {
		return nil, err
	}
	if p.Type == "post" {
		if cat := int64(h.s.settings.Int("default_category")); cat > 0 {
			_ = h.s.store.SetPostTerms(ctx, p.ID, "category", []int64{cat})
		}
	}
	h.s.afterPostSave(ctx, p) // the same save_post chain as every other save path
	h.s.purgeAll(ctx)
	return h.GetPost(ctx, "", p.ID)
}

func (h *pluginHost) UpdatePost(ctx context.Context, _ string, id int64, in plugins.PostInput) (*plugins.PostData, error) {
	p, err := h.s.store.GetPost(ctx, id)
	if err != nil || p == nil || !pluginPostTypes[p.Type] {
		return nil, errors.New("post not found")
	}
	before := *p
	if err := applyPostInput(p, in); err != nil {
		return nil, err
	}
	if err := h.authorizePostWrite(ctx, &before, p); err != nil {
		return nil, err
	}
	var editor int64
	if u := pluginUserFrom(ctx); u != nil {
		editor = u.ID
	}
	if err := h.s.store.UpdatePost(ctx, p, editor); err != nil {
		return nil, err
	}
	h.s.afterPostSave(ctx, p) // language, SEO redirects, menus, Web Builder drift, save_post
	h.s.purgeAll(ctx)
	return h.GetPost(ctx, "", p.ID)
}

// pluginSavePost fires save_post_<type> and save_post (in WordPress' order) after a post or page
// was saved (afterPostSave, and the plugins' own InsertPost/UpdatePost): the post id and the post as
// GetPost returns it. Other post types are not visible to plugins and fire nothing.
func (s *Server) pluginSavePost(ctx context.Context, p *store.Post) {
	if s.plugins == nil || p == nil || !pluginPostTypes[p.Type] {
		return
	}
	if prev := p.PrevStatus; prev != "" { // set by the store on insert / update (plugins_hooks.go)
		p.PrevStatus = ""
		s.pluginPostTransition(ctx, p, p.Status, prev)
	}
	hooks := s.plugins.hooks
	if !hooks.HasAction("save_post") && !hooks.HasAction("save_post_"+p.Type) {
		return
	}
	data := (&pluginHost{s: s}).postData(ctx, p)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	hooks.DoAction(ctx, "save_post_"+p.Type, p.ID, data)
	hooks.DoAction(ctx, "save_post", p.ID, data)
}

// pluginUserAction fires the user lifecycle actions user_register and wp_login for Wasm plugins
// (users:read): the user id and the user as CurrentUser returns it.
func (s *Server) pluginUserAction(ctx context.Context, hook string, u *store.User) {
	if s.plugins == nil || u == nil || !s.plugins.hooks.HasAction(hook) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.plugins.hooks.DoAction(ctx, hook, u.ID, userInfo(u))
}

// Current user --------------------------------------------------------------------

func (h *pluginHost) CurrentUser(ctx context.Context, _ string) (*plugins.UserInfo, error) {
	return userInfo(pluginUserFrom(ctx)), nil
}

// Scheduled events ----------------------------------------------------------------

// Plugin events are rows of the jobs table named after the hook; the payload carries the owning
// plugin and the arguments. runDueJobs fires them as actions (scheduler.go).
//
// Plugins schedule from OnInit, which runs on every instantiation (start, activation, re-instantiation
// after a trap), so scheduling an identical recurring event again keeps the queued row and its run_at:
// otherwise every restart would push the next run back by a full interval. Only a changed interval or
// arguments (or a one-off event) replaces the schedule.
func (h *pluginHost) ScheduleEvent(ctx context.Context, plugin string, ev plugins.ScheduledEvent) error {
	payload, _ := json.Marshal(map[string]any{"plugin": plugin, "args": ev.Args})
	args, _ := json.Marshal(ev.Args)
	runAt := time.Now()
	if ev.At > 0 {
		runAt = time.Unix(ev.At, 0)
	} else if ev.Interval > 0 {
		runAt = runAt.Add(time.Duration(ev.Interval) * time.Second)
	}
	rec := ""
	if ev.Interval > 0 {
		rec = "every:" + strconv.FormatInt(ev.Interval, 10)
	}
	tx, err := h.s.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Two instances (or two instantiations) scheduling at once must not both insert.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('plugin-event:' || $1 || ':' || $2))`, plugin, ev.Hook); err != nil {
		return err
	}
	if ev.At == 0 && rec != "" {
		var total, same int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE recurrence = $3 AND payload->'args' = $4::jsonb AND status <> 'failed')
			FROM jobs WHERE name = $1 AND payload->>'plugin' = $2 AND COALESCE(recurrence, '') <> ''`, ev.Hook, plugin, rec, string(args)).Scan(&total, &same); err != nil {
			return err
		}
		if total == 1 && same == 1 {
			return tx.Commit(ctx) // already scheduled exactly like this: keep its next run
		}
	}
	// Replace, a running row too (finishJob then finds no row to re-queue, so the old schedule cannot come
	// back next to the new one). A recurring event replaces the hook's recurring rows only; a one-off event
	// replaces only the one-off rows with the same args, so "one reminder per order" can be queued.
	if rec != "" {
		_, err = tx.Exec(ctx, `DELETE FROM jobs WHERE name = $1 AND payload->>'plugin' = $2 AND COALESCE(recurrence, '') <> ''`, ev.Hook, plugin)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM jobs WHERE name = $1 AND payload->>'plugin' = $2 AND COALESCE(recurrence, '') = ''
			AND COALESCE(NULLIF(payload->'args', 'null'::jsonb), '{}'::jsonb) = $3::jsonb`, ev.Hook, plugin, normalizedEventArgs(ev.Args))
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO jobs(name, payload, run_at, recurrence) VALUES ($1, $2, $3, $4)`, ev.Hook, payload, runAt, rec); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UnscheduleEvent removes the hook's rows in any status: a run in progress finishes, but finishJob finds no
// row to put back in the queue.
func (h *pluginHost) UnscheduleEvent(ctx context.Context, plugin, hook string) error {
	_, err := h.s.store.Pool.Exec(ctx, `DELETE FROM jobs WHERE name = $1 AND payload->>'plugin' = $2`, hook, plugin)
	return err
}

// normalizedEventArgs is the JSON used to compare event args: nil and empty are the same.
func normalizedEventArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	raw, _ := json.Marshal(args)
	return string(raw)
}

// UnscheduleEventArgs removes the runs of hook scheduled with exactly args.
func (h *pluginHost) UnscheduleEventArgs(ctx context.Context, plugin, hook string, args map[string]any) error {
	_, err := h.s.store.Pool.Exec(ctx, `DELETE FROM jobs WHERE name = $1 AND payload->>'plugin' = $2
		AND COALESCE(NULLIF(payload->'args', 'null'::jsonb), '{}'::jsonb) = $3::jsonb`, hook, plugin, normalizedEventArgs(args))
	return err
}

// Transients ----------------------------------------------------------------------

func transientKey(plugin, key string) string { return "plugin:" + plugin + ":" + key }

func (h *pluginHost) TransientGet(ctx context.Context, plugin, key string) (json.RawMessage, bool, error) {
	var raw []byte
	err := h.s.store.Pool.QueryRow(ctx, `SELECT value FROM transients WHERE key = $1 AND (expires_at IS NULL OR expires_at > now())`, transientKey(plugin, key)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil { // a database error is not "not found"
		return nil, false, err
	}
	return json.RawMessage(raw), true, nil
}

func (h *pluginHost) TransientSet(ctx context.Context, plugin, key string, value json.RawMessage, ttl time.Duration) error {
	var expires *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl)
		expires = &t
	}
	_, err := h.s.store.Pool.Exec(ctx, `INSERT INTO transients (key, value, expires_at) VALUES ($1, $2::jsonb, $3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at, updated_at = now()`, transientKey(plugin, key), []byte(value), expires)
	return err
}

func (h *pluginHost) TransientDelete(ctx context.Context, plugin, key string) error {
	_, err := h.s.store.Pool.Exec(ctx, `DELETE FROM transients WHERE key = $1`, transientKey(plugin, key))
	return err
}

// Shortcodes ----------------------------------------------------------------------

// doShortcodes expands plugin shortcodes in rendered content (do_shortcode() at the_content
// priority 11: after wpautop and block rendering, before the plugins' own the_content filters).
func (s *Server) doShortcodes(ctx context.Context, content string) string {
	if s.plugins == nil || s.plugins.rt == nil || !strings.Contains(content, "[") {
		return content
	}
	tags := s.plugins.rt.ShortcodeTags()
	if len(tags) == 0 {
		return content
	}
	return wp.DoShortcode(content, tags, func(tag string, attrs map[string]string, inner string) string {
		for k, v := range attrs { // a trusted script written into an attribute: the plugin never sees the placeholder
			if strings.Contains(v, cspPH) {
				attrs[k] = strings.ReplaceAll(v, cspPH, "")
			}
		}
		if strings.Contains(inner, cspPH) { // hide the nonce placeholder from the plugin (maskNoncePlaceholder)
			return maskNoncePlaceholder(inner, func(m string) string {
				out, _ := s.plugins.rt.DoShortcode(ctx, tag, attrs, m)
				return out
			})
		}
		out, _ := s.plugins.rt.DoShortcode(ctx, tag, attrs, inner)
		return out
	})
}

// REST routes ---------------------------------------------------------------------

// restHeaderSubset is what plugin REST handlers see of the request headers.
var restHeaderSubset = []string{"Accept", "Content-Type", "Accept-Language", "User-Agent", "Referer", "Origin", "X-Requested-With"}

// servePluginRest dispatches a /wp-json request no core route matched to a plugin route. It is
// called after the core routes, so plugins can never shadow them.
func (s *Server) servePluginRest(c *gin.Context, route, method string) bool {
	if s.plugins == nil || s.plugins.rt == nil {
		return false
	}
	p, r, params, ok := s.plugins.rt.FindRestRoute(route)
	if !ok {
		return false
	}
	if method == http.MethodOptions {
		c.Header("Allow", strings.Join(r.Methods, ", "))
		restJSON(c, http.StatusOK, map[string]any{"namespace": r.Namespace, "methods": r.Methods, "endpoints": []map[string]any{{"methods": r.Methods}}})
		return true
	}
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if !r.Allows(method) {
		restErr(c, http.StatusNotFound, "rest_no_route", "No route was found matching the URL and request method.")
		return true
	}
	req := plugins.RestRequest{Method: method, Route: route, Params: params, Query: map[string]string{}, Headers: map[string]string{}, User: userInfo(restCurrent(c))}
	for k, v := range c.Request.URL.Query() {
		if len(v) > 0 {
			req.Query[k] = v[0]
			if len(v) > 1 {
				if req.QueryAll == nil {
					req.QueryAll = map[string][]string{}
				}
				req.QueryAll[k] = v
			}
		}
	}
	for _, h := range restHeaderSubset {
		if v := c.GetHeader(h); v != "" {
			req.Headers[h] = v
		}
	}
	for _, h := range r.Headers { // headers the route declared (webhook signatures)
		if v := c.GetHeader(h); v != "" {
			req.Headers[h] = v
		}
	}
	if c.Request.Body != nil && method != http.MethodGet {
		body, _ := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20+1))
		if len(body) > 1<<20 {
			restErr(c, http.StatusRequestEntityTooLarge, "rest_upload_limited_size", "The request body is larger than 1 MiB.")
			return true
		}
		if len(body) > 0 {
			req.RawBody = base64.StdEncoding.EncodeToString(body)
			var decoded any
			if c.ContentType() == "application/json" && json.Unmarshal(body, &decoded) == nil {
				req.Body = decoded
			} else {
				req.Body = string(body)
			}
		}
	}
	// REST authentication, not the cookie alone: a cookie without a nonce is anonymous, and the
	// handler's host calls (insert, update, mail) are checked against that user.
	actor := restCurrent(c)
	ctx, cancel := context.WithTimeout(withPluginRequest(c.Request.Context(), actor), 10*time.Second)
	defer cancel()
	resp, err := p.ServeRest(ctx, *r, req)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_plugin_error", "The plugin could not handle the request.")
		return true
	}
	for k, v := range resp.Headers {
		k = http.CanonicalHeaderKey(k)
		if !pluginRestHeaderAllowed(p.Slug(), k, v) {
			if _, seen := pluginRestHeaderDropped.LoadOrStore(p.Slug()+"\x00"+k, true); !seen {
				p.HostLog("warning", fmt.Sprintf("REST response header %q dropped: not on the allowed list", k))
			}
			continue
		}
		c.Header(k, v)
	}
	if resp.ContentType != "" { // validated by ServeRest: allow-listed type, string body
		var text string
		_ = json.Unmarshal(resp.Body, &text)
		c.Header("X-Content-Type-Options", "nosniff")
		if resp.ContentType == "text/html" {
			c.Header("Content-Security-Policy", "sandbox")
		}
		c.Data(resp.Status, resp.ContentType+"; charset=UTF-8", []byte(text))
		return true
	}
	c.Data(resp.Status, "application/json; charset=UTF-8", resp.Body)
	return true
}

// pluginRestHeaders are the response headers a plugin REST handler may set (besides its own
// X-<Slug>-* headers). Everything else (cookies, CORS, CSP, Clear-Site-Data, X-Accel-Redirect /
// X-Sendfile, proxy cache control, Content-Encoding, Refresh, …) is dropped.
var pluginRestHeaders = map[string]bool{
	"Cache-Control": true, "Expires": true, "Etag": true, "Last-Modified": true, "Vary": true, "Link": true,
	"Retry-After": true, "Content-Language": true, "Content-Disposition": true, "X-Wp-Total": true, "X-Wp-Totalpages": true,
	"Location": true,
}

// pluginRestHeaderDropped remembers which dropped headers were logged already (slug + name).
var pluginRestHeaderDropped sync.Map

// pluginRestHeaderAllowed reports whether a plugin REST handler may send header k (canonical) with
// value v. Location must be a same-site path.
func pluginRestHeaderAllowed(slug, k, v string) bool {
	if strings.HasPrefix(k, http.CanonicalHeaderKey("X-"+slug+"-")) {
		return true
	}
	if !pluginRestHeaders[k] {
		return false
	}
	if k == "Location" {
		return strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") && !strings.ContainsAny(v, "\\\r\n")
	}
	return true
}

// pluginRestRoutes lists the routes of the enabled plugins for the REST index.
func (s *Server) pluginRestRoutes() []plugins.RestRoute {
	if s.plugins == nil || s.plugins.rt == nil {
		return nil
	}
	var out []plugins.RestRoute
	for _, p := range s.plugins.rt.Plugins() {
		if d, _ := p.Disabled(); !d {
			out = append(out, p.RestRoutes()...)
		}
	}
	return out
}

// Settings pages ------------------------------------------------------------------

// pluginSettingsSlug is the option_page value of a plugin settings form.
const pluginSettingsSlug = "plugin:"

type settingsFieldView struct {
	plugins.SettingsField
	Name    string // option name = input name
	Value   string
	Checked bool
	Row     template.HTML // the form-table row (plugins_fields.go)
}

type pluginSettingsData struct {
	Slug, Nonce string
	Page        *plugins.SettingsPage
	Fields      []settingsFieldView
}

func (s *Server) pluginSettingsPage(slug string) *plugins.SettingsPage {
	if s.plugins == nil || s.plugins.rt == nil {
		return nil
	}
	for _, ps := range s.plugins.rt.SettingsPages() {
		if ps.Plugin == slug {
			return ps.Page
		}
	}
	return nil
}

// pluginMenuEntries adds the plugins' settings pages under Settings.
func (s *Server) pluginMenuEntries(menu []*menuTop, screen string) {
	if s.plugins == nil || s.plugins.rt == nil {
		return
	}
	pages := s.plugins.rt.SettingsPages()
	if len(pages) == 0 {
		return
	}
	for _, top := range menu {
		if top.ID != "menu-settings" {
			continue
		}
		for _, ps := range pages {
			slug := "options-general?page=" + ps.Plugin
			sub := &menuSub{Slug: slug, Title: template.HTML(template.HTMLEscapeString(ps.Page.MenuTitle)), Current: slug == screen}
			if sub.Current {
				top.Current = true
			}
			top.Sub = append(top.Sub, sub)
		}
	}
}

// settingValue renders the saved (or default) value of a field for its input.
func settingValue(v any, f plugins.SettingsField) (text string, checked bool) {
	if v == nil {
		v = f.Default
	}
	switch t := v.(type) {
	case nil:
		return "", false
	case bool:
		if t {
			return "1", true
		}
		return "0", false
	case string:
		return t, t != "" && t != "0" && t != "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), t != 0
	case int:
		return strconv.Itoa(t), t != 0
	}
	b, _ := json.Marshal(v)
	return string(b), false
}

// pluginSettingsScreen renders Settings › <plugin>; it reports false when slug has no page.
func (s *Server) pluginSettingsScreen(c *gin.Context, slug string) bool {
	if slug == wbSettingsPage { // Settings › Web Builder, built in (webbuilder_settings.go)
		s.wbSettingsScreen(c)
		return true
	}
	page := s.pluginSettingsPage(slug)
	if page == nil {
		return false
	}
	d := pluginSettingsData{Slug: slug, Page: page, Nonce: s.nonce(c, "plugin-settings_"+slug)}
	for _, f := range page.Fields {
		name := f.OptionName(slug)
		v := settingsFieldView{SettingsField: f, Name: name}
		stored := s.settings.Get(pluginOptionKey(slug, name)) // plugins.go: stored in the plugin's namespace
		v.Value, v.Checked = settingValue(stored, f)
		v.Row = template.HTML(pluginFieldRow(name, f, stored))
		d.Fields = append(d.Fields, v)
	}
	if pluginFieldsHaveImage(page.Fields) { // the media modal for image fields (plugins_fields.go)
		s.enqueueMedia(c, 0)
		s.pageAssets(c).AddInline("media-editor", pluginMediaFieldJS)
	}
	if pluginFieldsHave(page.Fields, "color") { // swatch + Clear of color fields (plugins_fields.go)
		s.pageAssets(c).Script("jquery").AddInline("jquery", pluginColorFieldJS)
	}
	var notices []notice
	if c.Query("settings-updated") == "true" {
		notices = append(notices, notice{Type: "success", Message: "Settings saved.", Dismissible: true})
	}
	s.adminPage(c, "options-general?page="+slug, page.PageTitle, "settings_page_"+slug, "admin/plugin-settings", d, notices...)
	return true
}

// pluginSettingsSave handles the form POST (option_page = "plugin:<slug>").
func (s *Server) pluginSettingsSave(c *gin.Context, optionPage string) {
	slug := strings.TrimPrefix(optionPage, pluginSettingsSlug)
	if slug == wbSettingsPage { // Settings › Web Builder, built in (webbuilder_settings.go)
		s.wbSettingsSave(c)
		return
	}
	page := s.pluginSettingsPage(slug)
	if page == nil {
		c.String(http.StatusBadRequest, "Error: The options page was not found in the allowed options list.")
		return
	}
	if !s.checkNonce(c, "plugin-settings_"+slug) {
		s.denyNonce(c)
		return
	}
	values := map[string]any{}
	for _, f := range page.Fields {
		name := f.OptionName(slug)
		key := pluginOptionKey(slug, name) // plugins.go: the plugin's option namespace
		raw, present := c.GetPostForm(name)
		if v, ok := pluginFieldValue(f, raw, present); ok { // plugins_fields.go
			values[key] = v
		}
	}
	if err := s.settings.Set(c.Request.Context(), values); err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	s.purgeAll(c.Request.Context())
	c.Redirect(http.StatusFound, "/wp-admin/options-general?page="+url.QueryEscape(slug)+"&settings-updated=true")
}

// Package assets ------------------------------------------------------------------

const pluginAssetsPath = "/wp-content/plugins"

// pluginAssetURL is the URL of a file in a plugin package's assets/ directory.
func pluginAssetURL(slug, src, version string) string {
	return pluginAssetsPath + "/" + slug + "/assets/" + src + "?ver=" + url.QueryEscape(version)
}

// pluginAsset serves /wp-content/plugins/:slug/assets/*filepath from the loaded package.
func (s *Server) pluginAsset(c *gin.Context) {
	if s.plugins == nil || s.plugins.rt == nil {
		c.Status(http.StatusNotFound)
		return
	}
	p, ok := s.plugins.rt.Plugin(c.Param("slug"))
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	rel := strings.TrimPrefix(c.Param("filepath"), "/")
	data, ok := p.PackageAsset(rel)
	if !ok || plugins.CheckAssetPath(rel) != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "public, max-age=604800")
	c.Header("X-Content-Type-Options", "nosniff")
	if strings.HasSuffix(rel, ".svg") {
		c.Header("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'")
	}
	c.Data(http.StatusOK, plugins.AssetContentType(rel), data)
}

// pluginStyleTags returns <link> tags for the styles plugins enqueued for where (front|admin).
// In the admin (screen is the admin screen) a plugin's styles load on its own settings page only,
// unless it holds admin:scripts (which may change every admin screen anyway).
func (s *Server) pluginStyleTags(where, screen string) template.HTML {
	if s.plugins == nil || s.plugins.rt == nil {
		return ""
	}
	var sb strings.Builder
	for _, a := range s.plugins.rt.EnqueuedAssets(where, "style") {
		if where == "admin" && screen != "options-general?page="+a.Plugin {
			if p, ok := s.plugins.rt.Plugin(a.Plugin); !ok || !p.HasPermission(plugins.PermAdminScripts) {
				continue
			}
		}
		id := a.Plugin + "-" + a.Asset.Handle
		fmt.Fprintf(&sb, "<link rel='stylesheet' id='%s-css' href='%s' media='%s' />\n",
			html.EscapeString(id), html.EscapeString(pluginAssetURL(a.Plugin, a.Asset.Src, a.Version)), html.EscapeString(a.Asset.Media))
	}
	return template.HTML(sb.String())
}

// pluginScriptTags returns <script> tags for the scripts plugins enqueued for where, in the head
// or the footer.
func (s *Server) pluginScriptTags(where string, footer bool) template.HTML {
	if s.plugins == nil || s.plugins.rt == nil {
		return ""
	}
	var sb strings.Builder
	for _, a := range s.plugins.rt.EnqueuedAssets(where, "script") {
		if a.Asset.InFooter != footer {
			continue
		}
		id := a.Plugin + "-" + a.Asset.Handle
		sb.WriteString(pluginScriptTag(a, id, where, footer))
	}
	return template.HTML(sb.String())
}

// pluginScriptTag renders one enqueued script: an optional data block (wp_localize_script), then the
// tag with type="module" / defer / async. Head scripts on the front end default to defer.
func pluginScriptTag(a plugins.PluginAsset, id, where string, footer bool) string {
	var sb strings.Builder
	if len(a.Asset.Data) > 0 {
		raw, err := json.Marshal(a.Asset.Data) // escapes <, > and & so the data cannot close the tag
		if err == nil {
			fmt.Fprintf(&sb, "<script%s id=\"%s-js-extra\">window.%s = %s;</script>\n", cspNonceAttr(), html.EscapeString(id), pluginDataVar(a.Asset.Handle), raw)
		}
	}
	attrs := ""
	switch {
	case a.Asset.Module:
		attrs = " type=\"module\""
		if a.Asset.Strategy == "async" {
			attrs += " async"
		}
	case a.Asset.Strategy == "async":
		attrs = " async"
	case a.Asset.Strategy == "defer", a.Asset.Strategy == "" && where == "front" && !footer:
		attrs = " defer"
	}
	fmt.Fprintf(&sb, "<script%s src=\"%s\" id=\"%s-js\"%s></script>\n", cspNonceAttr(), html.EscapeString(pluginAssetURL(a.Plugin, a.Asset.Src, a.Version)), html.EscapeString(id), attrs)
	return sb.String()
}

// pluginDataVar turns a handle ("my-plugin_x") into the global name "myPluginXData".
func pluginDataVar(handle string) string {
	var sb strings.Builder
	up := false
	for _, r := range handle {
		if r == '-' || r == '_' {
			up = true
			continue
		}
		if up && r >= 'a' && r <= 'z' && sb.Len() > 0 {
			r -= 'a' - 'A'
		}
		up = false
		sb.WriteRune(r)
	}
	name := sb.String()
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		name = "_" + name
	}
	return name + "Data"
}
