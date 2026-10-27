package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Registrations other than hooks: shortcodes, REST routes, the settings page
// and enqueued assets. They live in the plugin (guarded by p.state) and the
// core reads them through the Runtime lookups below. Like hooks they are
// re-sent by the guest after a re-instantiation, so registering the same
// thing twice is a no-op.

// RestRoute is a REST route a plugin registered.
type RestRoute struct {
	Namespace string   `json:"namespace"` // <slug> or <slug>/v1
	Route     string   `json:"route"`     // regexp path under the namespace, e.g. /items/(?P<id>\d+)
	Methods   []string `json:"methods"`
	Callback  string   `json:"callback"`
	re        *regexp.Regexp
}

// Path returns the full route path (namespace + route).
func (r RestRoute) Path() string { return "/" + r.Namespace + r.Route }

// Allows reports whether the route accepts method.
func (r RestRoute) Allows(method string) bool {
	for _, m := range r.Methods {
		if m == method {
			return true
		}
	}
	return false
}

// Request limits for the registrations.
const (
	maxRestRoute     = 256
	maxRestBody      = 1 << 20
	maxSettingsField = 50
	maxLabel         = 200
	maxDescription   = 2000
	maxAssetDeps     = 10
)

var (
	restMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
	handleRe    = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	fieldIDRe   = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	fieldTypes  = map[string]bool{"text": true, "textarea": true, "number": true, "checkbox": true, "select": true}
	assetWhere  = map[string]bool{"front": true, "admin": true, "both": true}
)

func (p *Plugin) registerShortcode(tag, callback string) error {
	p.state.Lock()
	defer p.state.Unlock()
	if p.disabled.Load() {
		return ErrPluginDisabled
	}
	if p.shortcodes == nil {
		p.shortcodes = map[string]string{}
	}
	p.shortcodes[tag] = callback
	return nil
}

func (p *Plugin) registerRestRoute(r RestRoute) error {
	p.state.Lock()
	defer p.state.Unlock()
	if p.disabled.Load() {
		return ErrPluginDisabled
	}
	for i, old := range p.rest {
		if old.Namespace == r.Namespace && old.Route == r.Route {
			p.rest[i] = r // re-registration (or a changed callback / method list)
			return nil
		}
	}
	p.rest = append(p.rest, r)
	return nil
}

func (p *Plugin) setSettingsPage(page *SettingsPage) error {
	p.state.Lock()
	defer p.state.Unlock()
	if p.disabled.Load() {
		return ErrPluginDisabled
	}
	p.settings = page
	return nil
}

func (p *Plugin) enqueue(a Asset) error {
	p.state.Lock()
	defer p.state.Unlock()
	if p.disabled.Load() {
		return ErrPluginDisabled
	}
	key := a.Kind + ":" + a.Handle
	for i, old := range p.assets {
		if old.Kind+":"+old.Handle == key {
			p.assets[i] = a
			return nil
		}
	}
	p.assets = append(p.assets, a)
	return nil
}

// Shortcodes returns the plugin's shortcode tags.
func (p *Plugin) Shortcodes() []string {
	p.state.Lock()
	defer p.state.Unlock()
	out := make([]string, 0, len(p.shortcodes))
	for tag := range p.shortcodes {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

// RestRoutes returns the plugin's REST routes.
func (p *Plugin) RestRoutes() []RestRoute {
	p.state.Lock()
	defer p.state.Unlock()
	return append([]RestRoute(nil), p.rest...)
}

// SettingsPage returns the plugin's settings page, or nil.
func (p *Plugin) SettingsPage() *SettingsPage {
	p.state.Lock()
	defer p.state.Unlock()
	return p.settings
}

// Assets returns the plugin's enqueued styles and scripts.
func (p *Plugin) Assets() []Asset {
	p.state.Lock()
	defer p.state.Unlock()
	return append([]Asset(nil), p.assets...)
}

// PackageAsset returns a file from the package's assets/ directory.
func (p *Plugin) PackageAsset(path string) ([]byte, bool) {
	data, ok := p.pkgAssets[path]
	return data, ok
}

// Version returns the plugin version from its manifest.
func (p *Plugin) Version() string { return p.manifest.Version }

// DoShortcode runs the plugin's callback for tag.
func (p *Plugin) DoShortcode(ctx context.Context, tag string, attrs map[string]string, content string) (string, error) {
	p.state.Lock()
	cb, ok := p.shortcodes[tag]
	p.state.Unlock()
	if !ok {
		return "", fmt.Errorf("shortcode %q is not registered by %s", tag, p.Slug())
	}
	if attrs == nil {
		attrs = map[string]string{}
	}
	raw, err := p.call(ctx, &handleRequest{Plugin: p.Slug(), Callback: cb, Hook: tag, Type: "shortcode", Args: []any{attrs, content}})
	if err != nil {
		return "", err
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		p.logs.add("warning", "runtime", fmt.Sprintf("shortcode %s returned a non-string value", tag))
		return "", errors.New("shortcode callback must return a string")
	}
	return out, nil
}

// ServeRest runs the route's callback for req and returns the response. The
// response status defaults to 200 and the body to null.
func (p *Plugin) ServeRest(ctx context.Context, route RestRoute, req RestRequest) (*RestResponse, error) {
	if req.Params == nil {
		req.Params = map[string]string{}
	}
	if req.Query == nil {
		req.Query = map[string]string{}
	}
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}
	raw, err := p.call(ctx, &handleRequest{Plugin: p.Slug(), Callback: route.Callback, Hook: route.Path(), Type: "rest", Args: []any{req}})
	if err != nil {
		return nil, err
	}
	var resp RestResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("malformed REST response: %w", err)
	}
	if resp.Status == 0 {
		resp.Status = 200
	}
	if resp.Status < 100 || resp.Status > 599 {
		return nil, fmt.Errorf("REST response has an invalid status %d", resp.Status)
	}
	if len(resp.Body) > maxRestBody {
		return nil, fmt.Errorf("REST response body exceeds %d bytes", maxRestBody)
	}
	if len(resp.Body) == 0 {
		resp.Body = json.RawMessage("null")
	}
	return &resp, nil
}

// Runtime lookups used by the core -------------------------------------------------

// enabledPlugins returns the loaded plugins that are not disabled.
func (rt *Runtime) enabledPlugins() []*Plugin {
	var out []*Plugin
	for _, p := range rt.Plugins() {
		if !p.disabled.Load() {
			out = append(out, p)
		}
	}
	return out
}

// ShortcodeTags returns every shortcode tag registered by an enabled plugin.
func (rt *Runtime) ShortcodeTags() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range rt.enabledPlugins() {
		for _, tag := range p.Shortcodes() {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	sort.Strings(out)
	return out
}

// DoShortcode expands one shortcode through the plugin that registered its
// tag. It returns ok=false when no enabled plugin owns the tag; on a callback
// failure it returns "" and ok=true (the shortcode is dropped, like a
// WordPress callback that returned nothing).
func (rt *Runtime) DoShortcode(ctx context.Context, tag string, attrs map[string]string, content string) (string, bool) {
	for _, p := range rt.enabledPlugins() {
		p.state.Lock()
		_, owns := p.shortcodes[tag]
		p.state.Unlock()
		if !owns {
			continue
		}
		out, err := p.DoShortcode(ctx, tag, attrs, content)
		if err != nil {
			return "", true
		}
		return out, true
	}
	return "", false
}

// FindRestRoute matches a request path (e.g. /my-plugin/v1/items/7) against
// the enabled plugins' routes and returns the owning plugin, the route and the
// named path parameters.
func (rt *Runtime) FindRestRoute(path string) (*Plugin, *RestRoute, map[string]string, bool) {
	path = "/" + strings.Trim(path, "/")
	for _, p := range rt.enabledPlugins() {
		for _, r := range p.RestRoutes() {
			m := r.re.FindStringSubmatch(path)
			if m == nil {
				continue
			}
			params := map[string]string{}
			for i, name := range r.re.SubexpNames() {
				if name != "" && i < len(m) {
					params[name] = m[i]
				}
			}
			route := r
			return p, &route, params, true
		}
	}
	return nil, nil, nil, false
}

// RestNamespaces returns the namespaces of all registered plugin routes.
func (rt *Runtime) RestNamespaces() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range rt.enabledPlugins() {
		for _, r := range p.RestRoutes() {
			if !seen[r.Namespace] {
				seen[r.Namespace] = true
				out = append(out, r.Namespace)
			}
		}
	}
	sort.Strings(out)
	return out
}

// PluginSettings pairs a settings page with its plugin.
type PluginSettings struct {
	Plugin string
	Page   *SettingsPage
}

// SettingsPages returns the settings pages of the enabled plugins, by slug.
func (rt *Runtime) SettingsPages() []PluginSettings {
	var out []PluginSettings
	for _, p := range rt.enabledPlugins() {
		if page := p.SettingsPage(); page != nil {
			out = append(out, PluginSettings{Plugin: p.Slug(), Page: page})
		}
	}
	return out
}

// PluginAsset pairs an enqueued asset with its plugin.
type PluginAsset struct {
	Plugin  string
	Version string
	Asset   Asset
}

// EnqueuedAssets returns the assets enabled plugins enqueued for where ("front"
// or "admin"), styles and scripts of the given kind, in dependency order
// within each plugin.
func (rt *Runtime) EnqueuedAssets(where, kind string) []PluginAsset {
	var out []PluginAsset
	for _, p := range rt.enabledPlugins() {
		assets := p.Assets()
		var mine []Asset
		for _, a := range assets {
			if a.Kind == kind && (a.Where == where || a.Where == "both") {
				mine = append(mine, a)
			}
		}
		for _, a := range sortByDeps(mine) {
			out = append(out, PluginAsset{Plugin: p.Slug(), Version: p.Version(), Asset: a})
		}
	}
	return out
}

// sortByDeps orders assets so that dependencies (handles of the same plugin
// and kind) come first; unknown dependencies are ignored.
func sortByDeps(list []Asset) []Asset {
	byHandle := map[string]Asset{}
	for _, a := range list {
		byHandle[a.Handle] = a
	}
	var out []Asset
	done := map[string]bool{}
	var visit func(a Asset, depth int)
	visit = func(a Asset, depth int) {
		if done[a.Handle] || depth > maxAssetDeps {
			return
		}
		done[a.Handle] = true
		for _, d := range a.Deps {
			if dep, ok := byHandle[d]; ok {
				visit(dep, depth+1)
			}
		}
		out = append(out, a)
	}
	for _, a := range list {
		visit(a, 0)
	}
	return out
}
