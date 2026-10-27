package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"wpgo/internal/wp"
)

// Host functions added to ABI version 1 after the first release: shortcodes,
// REST routes, posts, the current user, scheduled events, settings pages,
// transients and enqueued assets. Each one validates and size-limits its
// request here, so Host implementations only see well-formed input.

// Limits for the requests below.
const (
	maxPostTitle    = 1000
	maxPostContent  = 1 << 20
	maxPostExcerpt  = 10000
	maxPostSlug     = 200
	maxTransientKey = 172 // WordPress' limit
	maxTransientVal = 1 << 20
	maxTransientTTL = int64(365 * 24 * 3600)
	minCronInterval = int64(60)
	maxCronAhead    = int64(365 * 24 * 3600)
	maxEventArgs    = 64 << 10
)

var (
	postStatuses = map[string]bool{"draft": true, "pending": true, "publish": true, "private": true, "future": true, "trash": true}
	postTypeRe   = regexp.MustCompile(`^[a-z0-9_-]{1,20}$`)
	nsRe         = regexp.MustCompile(`^[a-z0-9-]+(/v[0-9]+)?$`)
)

func hostRegisterShortcode(_ context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Tag      string `json:"tag"`
		Callback string `json:"callback"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if !wp.ValidShortcodeTag(r.Tag) {
		return nil, invalid("shortcode tag %q must be 1-64 lowercase letters, digits, _ or -", r.Tag)
	}
	if r.Callback == "" || len(r.Callback) > maxCallbackName {
		return nil, invalid("callback name must be 1-%d characters", maxCallbackName)
	}
	if !p.manifest.DeclaresShortcode(r.Tag) {
		p.logs.add("warning", "runtime", fmt.Sprintf("refused shortcode %q: not declared in plugin.json", r.Tag))
		return nil, &CallError{Code: CodePermissionDenied, Message: fmt.Sprintf("shortcode %q is not declared in plugin.json \"shortcodes\"", r.Tag)}
	}
	return nil, p.registerShortcode(r.Tag, r.Callback)
}

func hostRegisterRestRoute(_ context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Namespace string   `json:"namespace"`
		Route     string   `json:"route"`
		Methods   []string `json:"methods"`
		Callback  string   `json:"callback"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	r.Namespace = strings.Trim(r.Namespace, "/")
	if !nsRe.MatchString(r.Namespace) || (r.Namespace != p.Slug() && !strings.HasPrefix(r.Namespace, p.Slug()+"/")) {
		return nil, invalid("namespace must be %q or %q, got %q", p.Slug(), p.Slug()+"/v1", r.Namespace)
	}
	if r.Route == "" || len(r.Route) > maxRestRoute || !strings.HasPrefix(r.Route, "/") || strings.HasSuffix(r.Route, "/") {
		return nil, invalid("route must start with / (and not end with one), at most %d characters", maxRestRoute)
	}
	re, err := regexp.Compile("^/" + regexp.QuoteMeta(r.Namespace) + r.Route + "$")
	if err != nil {
		return nil, invalid("route %q is not a valid pattern: %v", r.Route, err)
	}
	if len(r.Methods) == 0 {
		r.Methods = []string{"GET"}
	}
	methods := make([]string, 0, len(r.Methods))
	for _, m := range r.Methods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if !restMethods[m] {
			return nil, invalid("method %q is not allowed (GET, POST, PUT, PATCH, DELETE)", m)
		}
		methods = append(methods, m)
	}
	if r.Callback == "" || len(r.Callback) > maxCallbackName {
		return nil, invalid("callback name must be 1-%d characters", maxCallbackName)
	}
	return nil, p.registerRestRoute(RestRoute{Namespace: r.Namespace, Route: r.Route, Methods: methods, Callback: r.Callback, re: re})
}

func hostGetPost(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		ID int64 `json:"id"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if r.ID <= 0 {
		return nil, invalid("id must be a positive post id")
	}
	post, err := p.host.GetPost(ctx, p.Slug(), r.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"found": post != nil, "post": post}, nil
}

func hostQueryPosts(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var q PostQuery
	if err := decodeRequest(req, &q); err != nil {
		return nil, err
	}
	if q.Type != "" && !postTypeRe.MatchString(q.Type) {
		return nil, invalid("type %q is not a post type name", q.Type)
	}
	if q.Status != "" && q.Status != "any" && !postStatuses[q.Status] {
		return nil, invalid("status %q is unknown", q.Status)
	}
	switch q.OrderBy {
	case "", "date", "title", "modified", "id":
	default:
		return nil, invalid("orderby must be date, title, modified or id")
	}
	q.Order = strings.ToUpper(q.Order)
	if q.Order != "" && q.Order != "ASC" && q.Order != "DESC" {
		return nil, invalid("order must be ASC or DESC")
	}
	if q.PerPage < 0 || q.PerPage > MaxQueryPerPage {
		return nil, invalid("per_page must be 1-%d", MaxQueryPerPage)
	}
	if q.PerPage == 0 {
		q.PerPage = 10
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	if len(q.Search) > 200 {
		return nil, invalid("search is longer than 200 characters")
	}
	list, err := p.host.QueryPosts(ctx, p.Slug(), q)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = &PostList{}
	}
	if list.Posts == nil {
		list.Posts = []PostData{}
	}
	return list, nil
}

func checkPostInput(in *PostInput, insert bool) error {
	if in.Title != nil && len(*in.Title) > maxPostTitle {
		return invalid("title is longer than %d characters", maxPostTitle)
	}
	if in.Content != nil && len(*in.Content) > maxPostContent {
		return invalid("content is larger than %d bytes", maxPostContent)
	}
	if in.Excerpt != nil && len(*in.Excerpt) > maxPostExcerpt {
		return invalid("excerpt is longer than %d characters", maxPostExcerpt)
	}
	if in.Slug != nil && len(*in.Slug) > maxPostSlug {
		return invalid("slug is longer than %d characters", maxPostSlug)
	}
	if in.Status != nil && !postStatuses[*in.Status] {
		return invalid("status %q is unknown (draft, pending, publish, private, future, trash)", *in.Status)
	}
	if in.Type != nil && !postTypeRe.MatchString(*in.Type) {
		return invalid("type %q is not a post type name", *in.Type)
	}
	if insert && (in.Title == nil || strings.TrimSpace(*in.Title) == "") && (in.Content == nil || strings.TrimSpace(*in.Content) == "") {
		return invalid("a new post needs a title or content")
	}
	if raw, _ := json.Marshal(in.Meta); len(raw) > maxEventArgs {
		return invalid("meta is larger than %d bytes", maxEventArgs)
	}
	return nil
}

func hostInsertPost(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var in PostInput
	if err := decodeRequest(req, &in); err != nil {
		return nil, err
	}
	if err := checkPostInput(&in, true); err != nil {
		return nil, err
	}
	post, err := p.host.InsertPost(ctx, p.Slug(), in)
	if err != nil {
		return nil, err
	}
	return post, nil
}

func hostUpdatePost(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		ID int64 `json:"id"`
		PostInput
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if r.ID <= 0 {
		return nil, invalid("id must be a positive post id")
	}
	if err := checkPostInput(&r.PostInput, false); err != nil {
		return nil, err
	}
	post, err := p.host.UpdatePost(ctx, p.Slug(), r.ID, r.PostInput)
	if err != nil {
		return nil, err
	}
	return post, nil
}

func hostGetCurrentUser(ctx context.Context, p *Plugin, _ []byte) (any, error) {
	u, err := p.host.CurrentUser(ctx, p.Slug())
	if err != nil {
		return nil, err
	}
	return map[string]any{"found": u != nil, "user": u}, nil
}

func checkCronHook(p *Plugin, hook string) error {
	if HookPermission(hook) != PermCron {
		return invalid("hook %q must start with cron_", hook)
	}
	if !p.manifest.DeclaresHook(hook) {
		return &CallError{Code: CodePermissionDenied, Message: fmt.Sprintf("hook %q is not declared in plugin.json \"hooks\"", hook)}
	}
	return nil
}

func hostScheduleEvent(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var ev ScheduledEvent
	if err := decodeRequest(req, &ev); err != nil {
		return nil, err
	}
	if err := checkCronHook(p, ev.Hook); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	switch {
	case ev.Interval < 0 || (ev.Interval > 0 && ev.Interval < minCronInterval):
		return nil, invalid("interval must be 0 (one-off) or at least %d seconds", minCronInterval)
	case ev.Interval > maxCronAhead:
		return nil, invalid("interval is longer than a year")
	case ev.At < 0 || ev.At > now+maxCronAhead:
		return nil, invalid("at must be a unix time within the next year")
	}
	if raw, _ := json.Marshal(ev.Args); len(raw) > maxEventArgs {
		return nil, invalid("args are larger than %d bytes", maxEventArgs)
	}
	return nil, p.host.ScheduleEvent(ctx, p.Slug(), ev)
}

func hostUnscheduleEvent(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Hook string `json:"hook"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if err := checkCronHook(p, r.Hook); err != nil {
		return nil, err
	}
	return nil, p.host.UnscheduleEvent(ctx, p.Slug(), r.Hook)
}

func hostRegisterSettingsPage(_ context.Context, p *Plugin, req []byte) (any, error) {
	var page SettingsPage
	if err := decodeRequest(req, &page); err != nil {
		return nil, err
	}
	page.PageTitle, page.MenuTitle = strings.TrimSpace(page.PageTitle), strings.TrimSpace(page.MenuTitle)
	if page.MenuTitle == "" {
		page.MenuTitle = p.manifest.Name
	}
	if page.PageTitle == "" {
		page.PageTitle = page.MenuTitle
	}
	if len(page.PageTitle) > maxLabel || len(page.MenuTitle) > maxLabel {
		return nil, invalid("titles must be at most %d characters", maxLabel)
	}
	if len(page.Description) > maxDescription {
		return nil, invalid("description is longer than %d characters", maxDescription)
	}
	if len(page.Fields) == 0 || len(page.Fields) > maxSettingsField {
		return nil, invalid("a settings page needs 1-%d fields", maxSettingsField)
	}
	seen := map[string]bool{}
	for i := range page.Fields {
		f := &page.Fields[i]
		if !fieldIDRe.MatchString(f.ID) {
			return nil, invalid("field id %q must be 1-64 lowercase letters, digits or _", f.ID)
		}
		if seen[f.ID] {
			return nil, invalid("field %q is listed twice", f.ID)
		}
		seen[f.ID] = true
		if !fieldTypes[f.Type] {
			return nil, invalid("field %q: type must be text, textarea, number, checkbox or select", f.ID)
		}
		if strings.TrimSpace(f.Label) == "" {
			f.Label = f.ID
		}
		if len(f.Label) > maxLabel || len(f.Placeholder) > maxLabel || len(f.Description) > maxDescription {
			return nil, invalid("field %q: label and placeholder are limited to %d characters, the description to %d", f.ID, maxLabel, maxDescription)
		}
		if f.Type == "select" {
			if len(f.Options) == 0 || len(f.Options) > 100 {
				return nil, invalid("field %q: a select needs 1-100 options", f.ID)
			}
			for _, o := range f.Options {
				if len(o.Value) > maxLabel || len(o.Label) > maxLabel {
					return nil, invalid("field %q: option values and labels are limited to %d characters", f.ID, maxLabel)
				}
			}
		} else if len(f.Options) > 0 {
			return nil, invalid("field %q: only select fields take options", f.ID)
		}
		if raw, _ := json.Marshal(f.Default); len(raw) > 4096 {
			return nil, invalid("field %q: default is too large", f.ID)
		}
	}
	return map[string]any{"option_prefix": SettingsOptionPrefix(p.Slug())}, p.setSettingsPage(&page)
}

func checkTransientKey(key string) error {
	if key == "" || len(key) > maxTransientKey {
		return invalid("transient key must be 1-%d characters", maxTransientKey)
	}
	return nil
}

func hostTransientGet(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Key string `json:"key"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if err := checkTransientKey(r.Key); err != nil {
		return nil, err
	}
	v, found, err := p.host.TransientGet(ctx, p.Slug(), r.Key)
	if err != nil {
		return nil, err
	}
	if !found || len(v) == 0 {
		return map[string]any{"found": false}, nil
	}
	return map[string]any{"found": true, "value": v}, nil
}

func hostTransientSet(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
		TTL   int64           `json:"ttl"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if err := checkTransientKey(r.Key); err != nil {
		return nil, err
	}
	if len(r.Value) == 0 {
		r.Value = json.RawMessage("null")
	}
	if len(r.Value) > maxTransientVal {
		return nil, invalid("transient value exceeds %d bytes", maxTransientVal)
	}
	if r.TTL < 0 || r.TTL > maxTransientTTL {
		return nil, invalid("ttl must be 0 (no expiry) to %d seconds", maxTransientTTL)
	}
	return nil, p.host.TransientSet(ctx, p.Slug(), r.Key, r.Value, time.Duration(r.TTL)*time.Second)
}

func hostTransientDelete(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Key string `json:"key"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if err := checkTransientKey(r.Key); err != nil {
		return nil, err
	}
	return nil, p.host.TransientDelete(ctx, p.Slug(), r.Key)
}

func hostEnqueueStyle(_ context.Context, p *Plugin, req []byte) (any, error) {
	return hostEnqueue(p, req, "style")
}

func hostEnqueueScript(_ context.Context, p *Plugin, req []byte) (any, error) {
	return hostEnqueue(p, req, "script")
}

func hostEnqueue(p *Plugin, req []byte, kind string) (any, error) {
	var a Asset
	if err := decodeRequest(req, &a); err != nil {
		return nil, err
	}
	a.Kind = kind
	if !handleRe.MatchString(a.Handle) {
		return nil, invalid("handle %q must be 1-64 lowercase letters, digits, _ or -", a.Handle)
	}
	src := path.Clean(strings.TrimPrefix(strings.TrimPrefix(a.Src, "/"), AssetsDir+"/"))
	if err := CheckAssetPath(src); err != nil {
		return nil, invalid("src %q: %v", a.Src, err)
	}
	if _, ok := p.pkgAssets[src]; !ok {
		return nil, invalid("src %q is not in the package's %s/ directory", a.Src, AssetsDir)
	}
	ext := strings.ToLower(path.Ext(src))
	if (kind == "style" && ext != ".css") || (kind == "script" && ext != ".js") {
		return nil, invalid("src %q has the wrong extension for a %s", a.Src, kind)
	}
	a.Src = src
	if a.Where == "" {
		a.Where = "front"
	}
	if !assetWhere[a.Where] {
		return nil, invalid(`where must be "front", "admin" or "both"`)
	}
	if len(a.Deps) > maxAssetDeps {
		return nil, invalid("at most %d deps", maxAssetDeps)
	}
	for _, d := range a.Deps {
		if !handleRe.MatchString(d) {
			return nil, invalid("dep %q is not a handle", d)
		}
	}
	if kind == "style" {
		a.InFooter = false
		if a.Media == "" {
			a.Media = "all"
		}
		if len(a.Media) > 64 {
			return nil, invalid("media is longer than 64 characters")
		}
	} else {
		a.Media = ""
	}
	return nil, p.enqueue(a)
}
