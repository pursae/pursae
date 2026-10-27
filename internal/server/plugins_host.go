package server

import (
	"context"
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
	"time"

	"github.com/gin-gonic/gin"

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

func withPluginUser(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, pluginUserKey{}, u)
}

func pluginUserFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(pluginUserKey{}).(*store.User)
	return u
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
	if err := h.s.store.InsertPost(ctx, p); err != nil {
		return nil, err
	}
	if p.Type == "post" {
		if cat := int64(h.s.settings.Int("default_category")); cat > 0 {
			_ = h.s.store.SetPostTerms(ctx, p.ID, "category", []int64{cat})
		}
	}
	h.s.purgeAll(ctx)
	return h.GetPost(ctx, "", p.ID)
}

func (h *pluginHost) UpdatePost(ctx context.Context, _ string, id int64, in plugins.PostInput) (*plugins.PostData, error) {
	p, err := h.s.store.GetPost(ctx, id)
	if err != nil || p == nil || !pluginPostTypes[p.Type] {
		return nil, errors.New("post not found")
	}
	if err := applyPostInput(p, in); err != nil {
		return nil, err
	}
	var editor int64
	if u := pluginUserFrom(ctx); u != nil {
		editor = u.ID
	}
	if err := h.s.store.UpdatePost(ctx, p, editor); err != nil {
		return nil, err
	}
	h.s.purgeAll(ctx)
	return h.GetPost(ctx, "", p.ID)
}

// Current user --------------------------------------------------------------------

func (h *pluginHost) CurrentUser(ctx context.Context, _ string) (*plugins.UserInfo, error) {
	return userInfo(pluginUserFrom(ctx)), nil
}

// Scheduled events ----------------------------------------------------------------

// Plugin events are rows of the jobs table named after the hook; the payload carries the owning
// plugin and the arguments. runDueJobs fires them as actions (scheduler.go).
func (h *pluginHost) ScheduleEvent(ctx context.Context, plugin string, ev plugins.ScheduledEvent) error {
	if err := h.UnscheduleEvent(ctx, plugin, ev.Hook); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"plugin": plugin, "args": ev.Args})
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
	_, err := h.s.store.Pool.Exec(ctx, `INSERT INTO jobs(name, payload, run_at, recurrence) VALUES ($1, $2, $3, $4)`, ev.Hook, payload, runAt, rec)
	return err
}

func (h *pluginHost) UnscheduleEvent(ctx context.Context, plugin, hook string) error {
	_, err := h.s.store.Pool.Exec(ctx, `DELETE FROM jobs WHERE name = $1 AND status = 'queued' AND payload->>'plugin' = $2`, hook, plugin)
	return err
}

// Transients ----------------------------------------------------------------------

func transientKey(plugin, key string) string { return "plugin:" + plugin + ":" + key }

func (h *pluginHost) TransientGet(ctx context.Context, plugin, key string) (json.RawMessage, bool, error) {
	var raw []byte
	err := h.s.store.Pool.QueryRow(ctx, `SELECT value FROM transients WHERE key = $1 AND (expires_at IS NULL OR expires_at > now())`, transientKey(plugin, key)).Scan(&raw)
	if err != nil {
		return nil, false, nil
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
		}
	}
	for _, h := range restHeaderSubset {
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
			var decoded any
			if c.ContentType() == "application/json" && json.Unmarshal(body, &decoded) == nil {
				req.Body = decoded
			} else {
				req.Body = string(body)
			}
		}
	}
	ctx, cancel := context.WithTimeout(withPluginUser(c.Request.Context(), restCurrent(c)), 10*time.Second)
	defer cancel()
	resp, err := p.ServeRest(ctx, *r, req)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_plugin_error", "The plugin could not handle the request.")
		return true
	}
	for k, v := range resp.Headers {
		if k = http.CanonicalHeaderKey(k); k == "Set-Cookie" || k == "Content-Type" || k == "Content-Length" {
			continue
		}
		c.Header(k, v)
	}
	c.Data(resp.Status, "application/json; charset=UTF-8", resp.Body)
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
	page := s.pluginSettingsPage(slug)
	if page == nil {
		return false
	}
	d := pluginSettingsData{Slug: slug, Page: page, Nonce: s.nonce(c, "plugin-settings_"+slug)}
	for _, f := range page.Fields {
		name := f.OptionName(slug)
		v := settingsFieldView{SettingsField: f, Name: name}
		v.Value, v.Checked = settingValue(s.settings.Get(name), f)
		d.Fields = append(d.Fields, v)
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
		raw, present := c.GetPostForm(name)
		switch f.Type {
		case "checkbox":
			values[name] = present && raw != "" && raw != "0"
		case "number":
			if !present {
				continue
			}
			n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				continue
			}
			if f.Min != nil && n < *f.Min {
				n = *f.Min
			}
			if f.Max != nil && n > *f.Max {
				n = *f.Max
			}
			values[name] = n
		case "select":
			if !present {
				continue
			}
			for _, o := range f.Options {
				if o.Value == raw {
					values[name] = raw
				}
			}
		default:
			if !present {
				continue
			}
			if len(raw) > 64<<10 {
				raw = raw[:64<<10]
			}
			values[name] = strings.ReplaceAll(raw, "\r\n", "\n")
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
func (s *Server) pluginStyleTags(where string) template.HTML {
	if s.plugins == nil || s.plugins.rt == nil {
		return ""
	}
	var sb strings.Builder
	for _, a := range s.plugins.rt.EnqueuedAssets(where, "style") {
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
		fmt.Fprintf(&sb, "<script src=\"%s\" id=\"%s-js\"></script>\n", html.EscapeString(pluginAssetURL(a.Plugin, a.Asset.Src, a.Version)), html.EscapeString(id))
	}
	return template.HTML(sb.String())
}
