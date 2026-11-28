package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/config"
	"wpgo/internal/mail"
	"wpgo/internal/plugins"
	"wpgo/internal/settings"
	"wpgo/internal/store"
	"wpgo/internal/textfold"
	"wpgo/internal/webbuilder"
)

// Plugins are signed zip packages holding a WebAssembly module and a plugin.json manifest. They run
// sandboxed (internal/plugins) and reach the site only through the Host below and the hooks the
// core fires: init, the_content, admin_notices, wpgo_post_stats and the cron_* events they schedule.
// plugins_host.go holds the rest of the Host and the core side of shortcodes, REST routes, settings
// pages and package assets.

type pluginSystem struct {
	hooks   *plugins.Registry
	rt      *plugins.Runtime
	dir     string // installed packages: <dir>/<slug>.zip
	market  []string
	initErr error

	// The trusted signing keys: the site's own (PURSAE_PLUGIN_KEYS, plugin_trusted_keys, the dev key)
	// and the Manager's, replaced on every index refresh (hubTrustKeys). Read them with trusted().
	keyMu    sync.RWMutex
	siteKeys []ed25519.PublicKey
	envKeys  []ed25519.PublicKey // PURSAE_PLUGIN_KEYS and the dev key: siteKeys without the option's keys
	hubKeys  []ed25519.PublicKey

	// Active plugins this process could not load (loadActivePlugin): not retried on every sync tick.
	failMu sync.Mutex
	fails  map[string]pluginLoadFail
}

// trusted is a snapshot of every trusted signing key.
func (ps *pluginSystem) trusted() []ed25519.PublicKey {
	ps.keyMu.RLock()
	defer ps.keyMu.RUnlock()
	return append(append([]ed25519.PublicKey{}, ps.siteKeys...), ps.hubKeys...)
}

// setHubKeys replaces the keys that come from Pursae Manager.
func (ps *pluginSystem) setHubKeys(keys []ed25519.PublicKey) {
	ps.keyMu.Lock()
	ps.hubKeys = keys
	ps.keyMu.Unlock()
}

// isHubKey reports whether key is one of the Manager's current keys.
func (ps *pluginSystem) isHubKey(key ed25519.PublicKey) bool {
	ps.keyMu.RLock()
	defer ps.keyMu.RUnlock()
	for _, k := range ps.hubKeys {
		if bytes.Equal(k, key) {
			return true
		}
	}
	return false
}

// pluginDirs places plugin data next to the uploads directory (/data/uploads -> /data/plugins).
func (s *Server) pluginDirs() (pkgDir, cacheDir string) {
	base := filepath.Dir(filepath.Clean(s.cfg.UploadsDir))
	return filepath.Join(base, "plugins"), filepath.Join(base, "plugin-cache")
}

// startPlugins creates the runtime and loads every active plugin.
func (s *Server) startPlugins(ctx context.Context) {
	ps := &pluginSystem{hooks: plugins.NewRegistry()}
	s.plugins = ps
	defer s.wbUpgrade(ctx) // the one-time switch from the old builder (webbuilder_upgrade.go)
	pkgDir, cacheDir := s.pluginDirs()
	ps.dir = pkgDir
	_ = os.MkdirAll(pkgDir, 0o755)
	// Compiled plugins stay in memory: wazero would load native code from an on-disk cache without
	// verifying it, so whoever can write that directory could run code outside the sandbox. The
	// directory of earlier versions is removed.
	_ = os.RemoveAll(cacheDir)
	for _, k := range strings.FieldsFunc(config.Env("PLUGIN_KEYS"), func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if pub, err := plugins.ParsePublicKey(k); err == nil {
			ps.envKeys = append(ps.envKeys, pub)
		}
	}
	// Marketplace demo: packages shipped next to the binary (or built by tools/build-plugins.sh).
	for _, dir := range []string{config.Env("MARKET_DIR"), "examples/plugins/dist", "/usr/share/wpgo/plugins"} {
		if dir == "" {
			continue
		}
		if m, _ := filepath.Glob(filepath.Join(dir, "*.zip")); len(m) > 0 {
			ps.market = m
			if pub, err := os.ReadFile(filepath.Join(dir, "dev-key.pub")); err == nil && devTrustDevKey() { //nolint:gosec // G703: the market directory is operator configuration
				if k, err := plugins.ParsePublicKey(lastLine(string(pub))); err == nil {
					ps.envKeys = append(ps.envKeys, k)
					slog.Warn("PURSAE_TRUST_DEV_KEY=1: plugins signed with the public demo key are trusted; development only, set it to 0 in production", "key", filepath.Join(dir, "dev-key.pub"))
				}
			}
			break
		}
	}
	s.applySiteKeys() // the environment's keys plus the Trusted Keys screen's
	rt, err := plugins.NewRuntime(ctx, ps.hooks, plugins.Config{CoreVersion: Version, MaxHTTPBody: pluginHTTPMaxBody, Logger: slog.Default(),
		// Shorter than servePluginRest's 10s request deadline, so a handler that runs out its
		// own budget is counted as a plugin failure instead of looking like a client cancel.
		RestTimeout: pluginRestBudget,
		OnDisable: func(slug, reason string) {
			slog.Warn("plugin disabled by the runtime", "plugin", slug, "reason", reason)
			s.recordPluginDisabled(context.Background(), slug, reason)
		}})
	if err != nil {
		ps.initErr = err
		slog.Error("plugin runtime", "err", err)
		return
	}
	ps.rt = rt
	s.hubTrustCached(ctx) // the Manager's keys from the cached index, before Manager-signed plugins load
	rows, err := s.store.Pool.Query(ctx, `SELECT slug, version, permissions FROM plugins WHERE status = 'active' ORDER BY slug`)
	if err != nil {
		return
	}
	type active struct {
		slug, version string
		perms         []byte
	}
	var list []active
	for rows.Next() {
		var a active
		if rows.Scan(&a.slug, &a.version, &a.perms) == nil {
			list = append(list, a)
		}
	}
	rows.Close()
	for _, a := range list {
		_ = s.loadActivePlugin(ctx, a.slug, a.version, a.perms)
	}
	ps.hooks.DoAction(ctx, "init")
	s.goBackground(s.pluginSyncLoop) // stops on Server.Close (background.go)
}

// pluginSyncInterval is how often a process compares the plugins table with what it has loaded, so
// that activating, deactivating, updating or deleting a plugin on one instance reaches the others
// (several replicas share one database and a package directory).
var pluginSyncInterval = 10 * time.Second

func (s *Server) pluginSyncLoop(ctx context.Context) {
	t := time.NewTicker(pluginSyncInterval)
	defer t.Stop()
	pending := map[string]string{} // slug -> wanted state seen on the previous tick
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncPluginsOnce(ctx, pending)
		}
	}
}

// syncPluginsOnce loads, unloads or reloads plugins whose database state differs from the loaded
// state. A difference must be seen on two consecutive ticks before it is acted on, so a local
// activate (load first, UPDATE a moment later) is never undone by the poll.
func (s *Server) syncPluginsOnce(ctx context.Context, pending map[string]string) {
	rt := s.plugins.rt
	if rt == nil {
		return
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT slug, status, version, permissions FROM plugins`)
	if err != nil {
		return
	}
	type row struct {
		status, version string
		perms           []byte
	}
	db := map[string]row{}
	for rows.Next() {
		var slug string
		var r row
		if err := rows.Scan(&slug, &r.status, &r.version, &r.perms); err != nil {
			rows.Close()
			return
		}
		db[slug] = r
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	want := map[string]string{} // slug -> action
	for slug, r := range db {
		p, loaded := rt.Plugin(slug)
		switch {
		case r.status == "active" && !loaded:
			if s.plugins.loadFailedRecently(slug, r.version, s.pluginTrustTag()) {
				continue // failed before with the same package and keys: retried later, not every tick
			}
			want[slug] = "load"
		case r.status == "active" && loaded && p.Manifest().Version != r.version:
			want[slug] = "reload"
		case r.status == "active" && loaded && !samePermissions(p, r.perms):
			// Permissions granted or revoked on another replica (or by SQL): the runtime only
			// applies them at load, so reload to converge instead of waiting for a restart.
			want[slug] = "reload"
		case r.status != "active" && loaded:
			want[slug] = "unload"
		}
	}
	for _, p := range rt.Plugins() { // deleted elsewhere
		if _, ok := db[p.Slug()]; !ok {
			want[p.Slug()] = "unload"
		}
	}
	for slug, act := range want {
		if pending[slug] != act {
			pending[slug] = act // first sighting: act only if it is still there next tick
			continue
		}
		delete(pending, slug)
		switch act {
		case "unload":
			_ = rt.Unload(ctx, slug)
		case "load", "reload":
			if act == "reload" {
				_ = rt.Unload(ctx, slug)
			}
			if err := s.loadActivePlugin(ctx, slug, db[slug].version, db[slug].perms); err != nil {
				continue
			}
			s.plugins.hooks.DoActionOwner(ctx, slug, "init")
		}
		s.purgeAll(ctx)
	}
	for slug := range pending {
		if _, ok := want[slug]; !ok {
			delete(pending, slug)
		}
	}
	ps := s.plugins
	ps.failMu.Lock()
	for slug := range ps.fails { // deactivated or deleted: a later activation starts afresh
		if db[slug].status != "active" {
			delete(ps.fails, slug)
		}
	}
	ps.failMu.Unlock()
}

// samePermissions reports whether a loaded plugin runs with exactly the permissions the stored
// grant gives it (declared ∩ granted, as Runtime.Load computes them).
func samePermissions(p *plugins.Plugin, rawPerms []byte) bool {
	var granted []plugins.Permission
	if len(rawPerms) > 0 && json.Unmarshal(rawPerms, &granted) != nil {
		return true // unreadable grant: never reload-loop on it
	}
	want := p.Manifest().PermissionSet().Intersect(plugins.NewPermissionSet(granted...))
	have := p.Permissions()
	if len(have) != len(want.List()) {
		return false
	}
	for _, perm := range have {
		if !want.Has(perm) {
			return false
		}
	}
	return true
}

// pluginRestBudget is the runtime budget of one plugin REST/admin-page callback.
const pluginRestBudget = 8 * time.Second

func lastLine(v string) string {
	ls := lines(v)
	if len(ls) == 0 {
		return ""
	}
	return ls[len(ls)-1]
}

func (s *Server) loadPlugin(ctx context.Context, slug string, rawPerms []byte) error {
	ps := s.plugins
	if ps.rt == nil {
		return ps.initErr
	}
	pkg, err := plugins.LoadPackageFile(filepath.Join(ps.dir, slug+".zip"), ps.trusted(), !s.allowUnsignedPlugins())
	if err != nil {
		return err
	}
	var granted []plugins.Permission
	_ = json.Unmarshal(rawPerms, &granted)
	_, err = ps.rt.Load(ctx, pkg, granted, &pluginHost{s: s})
	return err
}

// Hook helpers used by the core ---------------------------------------------------

func (s *Server) applyFilters(ctx context.Context, name string, value any, args ...any) any {
	if s.plugins == nil || !s.plugins.hooks.HasFilter(name) {
		return value
	}
	if str, ok := value.(string); ok && strings.Contains(str, cspPH) {
		return maskNoncePlaceholder(str, func(masked string) string {
			out, _ := s.plugins.hooks.ApplyFilters(ctx, name, masked, args...).(string)
			return out
		})
	}
	return s.plugins.hooks.ApplyFilters(ctx, name, value, args...)
}

// maskNoncePlaceholder runs filter (a Wasm plugin's) over markup holding trusted nonced scripts
// without showing it the process's nonce placeholder: a plugin that learnt it could put it on its own
// scripts, or store it where content would carry it, and every response would then turn it into a
// valid nonce (csp_nonce.go). Each placeholder is replaced by its own one-off random token of the
// same length and put back afterwards only where exactly one copy of that token is left; a token the
// plugin copied loses its nonce everywhere.
func maskNoncePlaceholder(markup string, filter func(string) string) string {
	parts := strings.Split(markup, cspPH)
	toks := make([]string, len(parts)-1)
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(p)
		if i < len(toks) {
			toks[i] = newCSPNonce()
			b.WriteString(toks[i])
		}
	}
	masked := b.String()
	out := filter(masked)
	out = strings.ReplaceAll(out, cspPH, "") // cannot be there: the plugin never saw it
	for _, t := range toks {
		// Restored only on its own script, unchanged: a filter that moved the token to another
		// script, or changed the trusted script's code, gets no nonce.
		el := maskedScript(masked, t)
		if strings.Count(out, t) == 1 && el != "" && strings.Count(out, el) == 1 {
			out = strings.Replace(out, t, cspPH, 1)
		} else {
			out = strings.ReplaceAll(out, t, "")
		}
	}
	return out
}

// maskedScript is the <script …>…</script> element of markup holding tok in its start tag, or "".
// The element may also be escaped (&lt;script …&gt;…&lt;/script&gt;, inside a srcdoc attribute).
func maskedScript(markup, tok string) string {
	i := strings.Index(markup, tok)
	if i < 0 {
		return ""
	}
	lower := strings.ToLower(markup)
	for _, f := range [][3]string{{"<script", "</script", ">"}, {"&lt;script", "&lt;/script", "&gt;"}} {
		start := strings.LastIndex(lower[:i], f[0])
		if start < 0 || strings.Contains(lower[start:i], f[2]) {
			continue
		}
		end := strings.Index(lower[i:], f[1])
		if end < 0 {
			continue
		}
		end += i
		if gt := strings.Index(lower[end:], f[2]); gt >= 0 {
			return markup[start : end+gt+len(f[2])]
		}
	}
	return ""
}

type noticeKey struct{}

type noticeSink struct {
	mu   sync.Mutex
	list []plugins.Notice
}

// pluginNotices fires admin_notices and returns what plugins queued, escaped for the admin screen,
// after the notices of the built-in plugins that show on every screen.
func (s *Server) pluginNotices(c *gin.Context) []notice {
	var out []notice
	if n, ok := s.wbUpgradeNotice(c); ok { // Pursae Web Builder: the old builder was replaced (webbuilder_upgrade.go)
		out = append(out, n)
	}
	if n, ok := s.wbThemeNotice(c); ok { // Pursae Web Builder: Hello Pursae switch / offer (webbuilder_settings.go)
		out = append(out, n)
	}
	if s.plugins == nil || !s.plugins.hooks.HasAction("admin_notices") {
		return out
	}
	sink := &noticeSink{}
	ctx, cancel := context.WithTimeout(context.WithValue(c.Request.Context(), noticeKey{}, sink), 2*time.Second)
	defer cancel()
	s.plugins.hooks.DoAction(ctx, "admin_notices")
	for _, n := range sink.list {
		typ := n.Type
		if typ != "success" && typ != "warning" && typ != "error" {
			typ = "info"
		}
		out = append(out, notice{Type: typ, Message: template.HTML(template.HTMLEscapeString(n.Message)), Dismissible: true})
	}
	return out
}

// pluginHost implements plugins.Host on top of the site's options, plugin_data and a guarded HTTP client.
type pluginHost struct{ s *Server }

// Plugin options. A plugin addresses its options by the names it chose (<slug>_* with dashes as
// underscores, and plugin_<slug>_* for its settings page), but they are stored under
// wasm:<slug>:<name> (pluginOptionKey). The options table is one key space shared with the core and
// the built-in plugins, so without the namespace a plugin slugged after a built-in option's first
// word ("wp-mail" → wp_mail_smtp, "gocdn" → gocdn_settings) could read or overwrite it. Reading is
// default-deny: besides its own options a plugin sees only the public core options below.

// pluginOptionNS prefixes the stored name of every plugin option; ":" never appears in a core or
// built-in option name.
const pluginOptionNS = "wasm:"

// pluginOptionKey is the settings key a plugin option is stored under.
func pluginOptionKey(plugin, name string) string { return pluginOptionNS + plugin + ":" + name }

// pluginOwnOption reports whether name is in the plugin's own option namespace.
func pluginOwnOption(plugin, name string) bool {
	return strings.HasPrefix(name, strings.ReplaceAll(plugin, "-", "_")+"_") || strings.HasPrefix(name, plugins.SettingsOptionPrefix(plugin))
}

// pluginPublicOptions are the core options every plugin with options:read may read: what a theme
// or a visitor can learn from the site anyway (WordPress' public settings), nothing about users,
// mail, keys, licenses or the built-in plugins' configuration.
var pluginPublicOptions = map[string]bool{
	"blogname": true, "blogdescription": true, "siteurl": true, "home": true, "WPLANG": true,
	"timezone_string": true, "gmt_offset": true, "date_format": true, "time_format": true, "start_of_week": true,
	"use_smilies": true, "default_category": true, "default_post_format": true, "posts_per_page": true,
	"posts_per_rss": true, "rss_use_excerpt": true, "show_on_front": true, "page_on_front": true,
	"page_for_posts": true, "blog_public": true, "default_ping_status": true, "default_comment_status": true,
	"require_name_email": true, "comment_registration": true, "close_comments_for_old_posts": true,
	"close_comments_days_old": true, "show_comments_cookies_opt_in": true, "thread_comments": true,
	"thread_comments_depth": true, "page_comments": true, "comments_per_page": true, "default_comments_page": true,
	"comment_order": true, "show_avatars": true, "avatar_rating": true, "avatar_default": true,
	"thumbnail_size_w": true, "thumbnail_size_h": true, "thumbnail_crop": true, "medium_size_w": true,
	"medium_size_h": true, "medium_large_size_w": true, "medium_large_size_h": true, "large_size_w": true,
	"large_size_h": true, "uploads_use_yearmonth_folders": true, "permalink_structure": true,
	"category_base": true, "tag_base": true, "template": true, "stylesheet": true, "sticky_posts": true,
	"wp_page_for_privacy_policy": true, "site_icon": true, "users_can_register": true, "blog_charset": true,
	"html_type": true,
}

func (h *pluginHost) GetOption(_ context.Context, plugin string, name string) (any, bool, error) {
	key := name
	switch {
	case pluginOwnOption(plugin, name):
		key = pluginOptionKey(plugin, name)
	case !pluginPublicOptions[name]:
		return nil, false, nil
	}
	v := h.s.settings.Get(key)
	return v, v != nil, nil
}

// UpdateOption only writes options in the plugin's own namespace (<slug>_* with dashes as underscores,
// and the plugin_<slug>_* options of its settings page), stored under wasm:<slug>:<name>.
func (h *pluginHost) UpdateOption(ctx context.Context, plugin, name string, value any) error {
	if !pluginOptionAllowed(plugin, name) {
		return fmt.Errorf("plugins may only write their own options (%q or %q) and never a core option", strings.ReplaceAll(plugin, "-", "_")+"_*", plugins.SettingsOptionPrefix(plugin)+"*")
	}
	return h.s.settings.Set(ctx, map[string]any{pluginOptionKey(plugin, name): value})
}

// pluginOptionAllowed reports whether plugin may write the option name. Writes are stored in the
// plugin's namespace (pluginOptionKey), so they can no longer reach a core option; names that look
// like a core option (every default, the core's own prefixes) are still refused so that a plugin
// never holds a private copy of, say, "default_role" that a reader could mistake for the real one.
func pluginOptionAllowed(plugin, name string) bool {
	own := plugins.SettingsOptionPrefix(plugin) // plugin_<slug>_: the options of its settings page
	prefix := strings.ReplaceAll(plugin, "-", "_") + "_"
	if !strings.HasPrefix(name, prefix) && !strings.HasPrefix(name, own) {
		return false
	}
	if _, core := settings.Defaults[name]; core {
		return false
	}
	if strings.HasPrefix(name, own) {
		return true
	}
	for _, p := range []string{"auth_", "wpgo_", "native_", "plugin_", "pursae_"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// pluginHTTPMaxBody caps a plugin's http_get response body (documented as 1 MiB).
const pluginHTTPMaxBody = 1 << 20

var pluginHTTP = &http.Client{
	Timeout: 8 * time.Second,
	Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second,
		Control: publicOnly(nil, "destination not allowed for plugins")}).DialContext},
}

func (h *pluginHost) HTTPGet(ctx context.Context, plugin, url string) (*plugins.HTTPResponse, error) {
	return h.HTTPRequest(ctx, plugin, plugins.HTTPRequest{Method: http.MethodGet, URL: url})
}

func (h *pluginHost) HTTPRequest(ctx context.Context, plugin string, r plugins.HTTPRequest) (*plugins.HTTPResponse, error) {
	if !strings.HasPrefix(r.URL, "https://") && !strings.HasPrefix(r.URL, "http://") {
		return nil, errors.New("only http(s) URLs are allowed")
	}
	var body io.Reader
	if r.Body != "" {
		body = strings.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "Pursae/"+Version+" plugin/"+plugin)
	resp, err := pluginHTTP.Do(req)
	if err != nil {
		return nil, pluginHTTPError(err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, pluginHTTPMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(respBody) > pluginHTTPMaxBody {
		return nil, fmt.Errorf("the response body is larger than %d bytes", pluginHTTPMaxBody)
	}
	hdr := map[string]string{}
	for k := range resp.Header {
		hdr[k] = resp.Header.Get(k)
	}
	return &plugins.HTTPResponse{Status: resp.StatusCode, Header: hdr, Body: string(respBody)}, nil
}

// pluginNetAddr matches a resolved address so a blocked private or link-local IP is not returned
// to the plugin (the dial error otherwise includes it: "dial tcp 169.254.169.254:80: …").
var pluginNetAddr = regexp.MustCompile(`(?:\d{1,3}\.){3}\d{1,3}|(?:[0-9a-fA-F]{0,4}:){2,}[0-9a-fA-F]{0,4}`)

// pluginHTTPError is err with any resolved IP removed. The URL the plugin asked for stays; the
// address it resolved to does not.
func pluginHTTPError(err error) error {
	if err == nil {
		return nil
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		inner := "destination not allowed for plugins"
		if oe.Err != nil && !pluginNetAddr.MatchString(oe.Err.Error()) {
			inner = oe.Err.Error()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s %q: %s", ue.Op, ue.URL, inner)
		}
		return errors.New(inner)
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return fmt.Errorf("%s %q: %s", ue.Op, ue.URL, pluginHTTPError(ue.Err).Error())
	}
	msg := pluginNetAddr.ReplaceAllString(err.Error(), "[redacted]")
	return errors.New(msg)
}

// pluginMailLimit caps the messages one plugin may queue per hour.
const pluginMailLimit = 60

var pluginMailCount = struct {
	sync.Mutex
	hour map[string]int
	at   time.Time
}{hour: map[string]int{}}

func (h *pluginHost) SendMail(ctx context.Context, plugin string, m plugins.MailMessage) error {
	if err := h.authorizeMail(ctx); err != nil {
		return err
	}
	pluginMailCount.Lock()
	if time.Since(pluginMailCount.at) > time.Hour {
		pluginMailCount.hour, pluginMailCount.at = map[string]int{}, time.Now()
	}
	pluginMailCount.hour[plugin]++
	over := pluginMailCount.hour[plugin] > pluginMailLimit
	pluginMailCount.Unlock()
	if over {
		return fmt.Errorf("mail limit of %d per hour reached", pluginMailLimit)
	}
	h.s.sendMail(ctx, mail.Message{To: m.To, Subject: m.Subject, Body: m.Body, HTML: m.HTML, ReplyTo: m.ReplyTo})
	return nil
}

func (h *pluginHost) StorageGet(ctx context.Context, plugin, key string) (string, bool, error) {
	var v string
	err := h.s.store.Pool.QueryRow(ctx, `SELECT d.value #>> '{}' FROM plugin_data d JOIN plugins p ON p.id = d.plugin_id WHERE p.slug = $1 AND d.key = $2`, plugin, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil { // a database error is not "not found": the guest must not overwrite the value
		return "", false, err
	}
	return v, true, nil
}

// Per-plugin storage quota: one value is already capped (abi.go), but without these a plugin
// could fill the database with unlimited keys. Vars so tests can lower them.
var (
	pluginStorageMaxKeys  = 10000
	pluginStorageMaxBytes = int64(64 << 20)
)

func (h *pluginHost) StorageSet(ctx context.Context, plugin, key, value string) error {
	raw, _ := json.Marshal(value)
	var keys, total, old int64
	var exists bool
	if err := h.s.store.Pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(octet_length(d.value::text)), 0),
		COALESCE(bool_or(d.key = $2), false), COALESCE(sum(octet_length(d.value::text)) FILTER (WHERE d.key = $2), 0)
		FROM plugin_data d JOIN plugins p ON p.id = d.plugin_id WHERE p.slug = $1`, plugin, key).Scan(&keys, &total, &exists, &old); err != nil {
		return err
	}
	if !exists && keys >= int64(pluginStorageMaxKeys) {
		return fmt.Errorf("storage quota exceeded: at most %d keys per plugin (delete unused keys first)", pluginStorageMaxKeys)
	}
	if total-old+int64(len(raw)) > pluginStorageMaxBytes {
		return fmt.Errorf("storage quota exceeded: at most %d MiB of data per plugin (delete unused keys first)", pluginStorageMaxBytes>>20)
	}
	_, err := h.s.store.Pool.Exec(ctx, `INSERT INTO plugin_data (plugin_id, key, value) SELECT id, $2, $3::jsonb FROM plugins WHERE slug = $1
		ON CONFLICT (plugin_id, key) DO UPDATE SET value = EXCLUDED.value`, plugin, key, raw)
	return err
}

func (h *pluginHost) StorageDelete(ctx context.Context, plugin, key string) error {
	_, err := h.s.store.Pool.Exec(ctx, `DELETE FROM plugin_data d USING plugins p WHERE p.id = d.plugin_id AND p.slug = $1 AND d.key = $2`, plugin, key)
	return err
}

func (h *pluginHost) StorageList(ctx context.Context, plugin, prefix, after string, limit int) ([]string, error) {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	rows, err := h.s.store.Pool.Query(ctx, `SELECT d.key FROM plugin_data d JOIN plugins p ON p.id = d.plugin_id
		WHERE p.slug = $1 AND d.key LIKE $2 || '%' AND d.key > $3 ORDER BY d.key COLLATE "C" LIMIT $4`, plugin, esc, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (h *pluginHost) AdminNotice(ctx context.Context, plugin string, n plugins.Notice) error {
	sink, _ := ctx.Value(noticeKey{}).(*noticeSink)
	if sink == nil {
		return nil
	}
	n.Plugin = plugin
	sink.mu.Lock()
	sink.list = append(sink.list, n)
	sink.mu.Unlock()
	return nil
}

// Plugins screen -------------------------------------------------------------------

// builtinPluginVersion is the version every Pursae built-in plugin shows on the Plugins screen.
const builtinPluginVersion = "0.1.0"

type pluginRow struct {
	Slug, Name, Version, Description, Author, AuthorURI, PluginURI string
	Like                                                           string // "Like WP Mail SMTP": the WordPress plugin it replaces
	Native, Active, Disabled                                       bool
	DisabledWhy                                                    string
	LoadFailed                                                     bool     // active, but its package could not be loaded (DisabledWhy says why)
	MissingPerms                                                   []string // asked by an update, never granted (reloadPlugin)
	Permissions                                                    []permRow
	ActivateURL, DeactivateURL, DeleteURL, SettingsURL             string
	SetupURL                                                       string // the setup guide (plugin_setup.go), while active
	Logs                                                           []plugins.LogEntry
	Source                                                         string        // native | hub | upload
	Update                                                         *pluginUpdate // a newer version on Pursae Manager (hub_updates.go)
	AutoUpdate                                                     bool          // updates itself (Manager-installed plugins only)
	AutoUpdateURL                                                  string
}

type permRow struct {
	Name, Description string
	Danger            bool // admin:scripts, content:html: the consent screen leaves them unticked
}

type pluginsData struct {
	Rows             []pluginRow
	Active, Inactive int
	Status           string
	Search           string // "Search installed plugins"
	Consent          *pluginRow
	ConsentNonce     string
	RuntimeErr       string
	BulkNonce        string
	CanDelete        bool
}

// installedPlugins are the rows of the Plugins screen: every plugin with its action links.
func (s *Server) installedPlugins(c *gin.Context) []pluginRow {
	n := func(action, slug string) string {
		return "/wp-admin/plugins?action=" + action + "&plugin=" + pluginURLSlug(slug) + "&_wpnonce=" + s.nonce(c, action+"-plugin_"+slug)
	}
	rows := s.pluginInventory(c.Request.Context())
	for i := range rows {
		rows[i].ActivateURL, rows[i].DeactivateURL = n("activate", rows[i].Slug), n("deactivate", rows[i].Slug)
		if !rows[i].Native {
			rows[i].DeleteURL = n("delete", rows[i].Slug)
		}
		if rows[i].Active {
			if g, ok := s.setupGuideForRow(c.Request.Context(), rows[i]); ok {
				rows[i].SetupURL = g.entryURL()
			}
		}
	}
	return rows
}

// nativePluginRows are the plugins built into the core.
func (s *Server) nativePluginRows() []pluginRow {
	rows := []pluginRow{
		{Slug: "classic-editor", Name: "Classic Editor", Version: "1.6.7", Native: true, Active: s.settings.Bool("native_classic_editor"),
			Description: "Enables the previous \"classic\" editor and the old-style Edit Post screen with TinyMCE, Meta Boxes, etc. Choose the default editor and whether users may switch under Settings &rsaquo; Writing.",
			Author:      "Pursae", SettingsURL: "/wp-admin/options-writing"},
		{Slug: "wpgo-cache", Name: "Pursae Cache", Version: builtinPluginVersion, Like: "LiteSpeed Cache", Native: true, Active: s.settings.Bool("wpgo_cache_enabled"),
			Description: "Full-page cache with tag-based purging, a crawler and page optimizations. Works behind nginx, OpenLiteSpeed, Apache or on its own.",
			Author:      "Pursae", SettingsURL: "/wp-admin/admin?page=wpgo-cache"},
		{Slug: webbuilder.PluginSlug, Name: "Pursae Web Builder", Version: builtinPluginVersion, Like: "Elementor Builder", Native: true, Active: s.wbOn(),
			Description: "Build pages visually from ready-made sections, then publish them as fast, static HTML. Design the site&#8217;s header, footer and templates for posts, pages, archives, search results and 404 pages under Appearance &rsaquo; Theme Builder, shown by the bundled Hello Pursae theme. Choose which post types it edits under Settings &rsaquo; Web Builder.",
			Author:      "Pursae"},
		{Slug: msSlug, Name: "Pursae SMTP", Version: builtinPluginVersion, Like: "WP Mail SMTP", Native: true, Active: s.mailSMTPOn(),
			Description: "Make email delivery easy for Pursae. Connect with SMTP, SendLayer, Brevo, Mailgun, SendGrid, Amazon SES, Gmail, Outlook and more, with an email log, reports, failure alerts, backup connection and smart routing. A native clone of WP Mail SMTP.",
			Author:      "Pursae"},
		{Slug: gontactSlug, Name: "Pursae Contact Form", Version: builtinPluginVersion, Like: "Contact Form 7", Native: true, Active: s.gontactOn(),
			Description: "Just another contact form plugin. Simple but flexible: manage multiple contact forms, customize the form and the mail contents with simple markup, Ajax-powered submitting, quiz, reCAPTCHA and Turnstile spam protection, file uploads.",
			Author:      "Pursae", SettingsURL: "/wp-admin/admin?page=wpcf7"},
		{Slug: acfgoSlug, Name: "Pursae Custom Fields", Version: builtinPluginVersion, Like: "Advanced Custom Fields (ACF)", Native: true, Active: s.acfgoOn(), SettingsURL: map[bool]string{true: "/wp-admin/acf-field-groups"}[s.acfgoOn()],
			Description: "Customize WordPress with powerful, professional and intuitive fields: field groups, repeaters, flexible content, options pages, custom post types and blocks. A native clone of Advanced Custom Fields PRO.",
			Author:      "Pursae"},
		golylangPluginRow(s.golylangOn()), // golylang_admin.go
	}
	nativeSettingsLinks(rows)
	rows = append(rows, s.maintPluginRow())     // Maintenance-Go (maintenance_admin.go)
	rows = append(rows, s.bmgrPluginRow())      // Backup Manager (backup.go)
	rows = append(rows, s.seogoPluginRow())     // Pursae SEO (seogo.go)
	rows = append(rows, s.flipbookPluginRow())  // Pursae Flipbook (flipbook.go)
	rows = append(rows, s.gooikePluginRow())    // Gooike (gooike.go)
	rows = append(rows, s.pixelPluginRow())     // Pursae Pixel (pixelgo.go)
	rows = append(rows, s.analyticsPluginRow()) // Pursae Analytics (analytics.go)
	rows = append(rows, s.forumPluginRow())     // Pursae Forum (forum.go)
	rows = append(rows, s.gooPluginRow())       // Goocommerce (goocommerce.go)
	rows = append(rows, s.adminsecPluginRow())  // AdminSecGo (adminsec.go)
	rows = append(rows, s.gocdnPluginRow())     // GoCDN (gocdn.go)
	rows = append(rows, s.hgPluginRow())        // HeadlessGo (headlessgo.go)
	for i := range rows {
		rows[i].Source = pluginSourceNative
	}
	return rows
}

// nativeSettingsLinks sets the Settings links of the core plugins that WordPress shows only while
// a plugin is active. Rows are found by slug, so the order of nativePluginRows does not matter.
func nativeSettingsLinks(rows []pluginRow) {
	for i := range rows {
		switch rows[i].Slug {
		case "wpgo-cache":
			if !rows[i].Active {
				rows[i].SettingsURL = ""
			}
		case webbuilder.PluginSlug:
			if rows[i].Active {
				rows[i].SettingsURL = wbSettingsURL // webbuilder_settings.go
			}
		case msSlug:
			if rows[i].Active {
				rows[i].SettingsURL = "/wp-admin/admin?page=" + msSlug
			}
		case gontactSlug:
			if !rows[i].Active {
				rows[i].SettingsURL = ""
			}
		}
	}
}

// pluginInventory is every plugin, native and installed packages, without action links (the Plugins
// screen adds them; the Manager ping sends the inventory).
func (s *Server) pluginInventory(ctx context.Context) []pluginRow {
	rows := s.nativePluginRows()
	dbRows, err := s.store.Pool.Query(ctx, `SELECT slug, status, version, source, disabled_reason, missing_permissions FROM plugins ORDER BY slug`)
	if err != nil {
		return rows
	}
	type inst struct {
		slug, status, version, source, why string
		missing                            []byte
	}
	var list []inst
	for dbRows.Next() {
		var r inst
		if dbRows.Scan(&r.slug, &r.status, &r.version, &r.source, &r.why, &r.missing) == nil {
			list = append(list, r)
		}
	}
	dbRows.Close()
	for _, r := range list {
		row := pluginRow{Slug: r.slug, Name: r.slug, Version: r.version, Active: r.status == "active", Source: r.source}
		var missing []plugins.Permission
		_ = json.Unmarshal(r.missing, &missing)
		for _, p := range missing {
			row.MissingPerms = append(row.MissingPerms, string(p))
		}
		if !row.Active && r.why != "" {
			row.Disabled, row.DisabledWhy = true, r.why
		}
		if row.Active && r.why != "" && s.plugins.rt != nil {
			if _, loaded := s.plugins.rt.Plugin(r.slug); !loaded {
				// Active, but this process could not load it (loadActivePlugin): it is not running.
				row.Disabled, row.DisabledWhy, row.LoadFailed = true, r.why, true
			}
		}
		if pkg, err := plugins.LoadPackageFile(filepath.Join(s.plugins.dir, r.slug+".zip"), s.plugins.trusted(), false); err == nil {
			m := pkg.Manifest
			row.Name, row.Version, row.Description, row.Author, row.AuthorURI, row.PluginURI = m.Name, m.Version, template.HTMLEscapeString(m.Description), m.Author, m.AuthorURI, m.PluginURI
			for _, p := range m.Permissions {
				row.Permissions = append(row.Permissions, permRow{string(p), p.Description(), p.Dangerous()})
			}
		}
		if s.plugins.rt != nil {
			if lp, ok := s.plugins.rt.Plugin(r.slug); ok {
				row.Disabled, row.DisabledWhy = lp.Disabled()
				row.Logs = lp.Logs()
				if len(row.Logs) > 5 {
					row.Logs = row.Logs[len(row.Logs)-5:]
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) pluginsScreen(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("activate_plugins", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to manage plugins for this site.")
		return
	}
	if c.Query("action") == "upgrade-plugin" && c.Request.Method == http.MethodGet { // hub_updates.go
		s.pluginUpgrade(c)
		return
	}
	if a := c.Query("action"); c.Request.Method == http.MethodGet && (a == "enable-auto-update" || a == "disable-auto-update") { // hub_updates.go
		s.pluginAutoUpdateToggle(c, a == "enable-auto-update")
		return
	}
	if a := c.Query("action"); a != "" && c.Request.Method == http.MethodGet && a != "activate" {
		s.pluginAction(c, a, pluginSlugFromURL(c.Query("plugin")))
		return
	}
	if c.Request.Method == http.MethodPost {
		bulk := c.PostForm("action")
		if bulk == "" || bulk == "-1" {
			bulk = c.PostForm("action2")
		}
		if strings.HasSuffix(bulk, "-selected") {
			s.pluginsBulk(c, bulk)
			return
		}
		s.pluginAction(c, c.PostForm("action"), pluginSlugFromURL(c.PostForm("plugin")))
		return
	}
	d := pluginsData{Status: c.DefaultQuery("plugin_status", "all"), BulkNonce: s.nonce(c, "bulk-plugins"), CanDelete: u.Can("delete_plugins", nil)}
	if s.plugins.initErr != nil {
		d.RuntimeErr = s.plugins.initErr.Error()
	}
	all := s.pluginsWithUpdates(c, s.installedPlugins(c))
	d.Search = strings.TrimSpace(c.Query("s"))
	q := d.Search
	for _, r := range all {
		if r.Active {
			d.Active++
		} else {
			d.Inactive++
		}
		if q != "" && !textfold.Contains(r.Name+" "+r.Description+" "+r.Author, q) { // "ışık" finds "IŞIK"
			continue
		}
		if d.Status == "all" || (d.Status == "active") == r.Active {
			d.Rows = append(d.Rows, r)
		}
	}
	// Activating a Wasm plugin first shows the permissions it asks for.
	if c.Query("action") == "activate" {
		slug := pluginSlugFromURL(c.Query("plugin"))
		if !s.checkNonce(c, "activate-plugin_"+slug) {
			s.denyNonce(c)
			return
		}
		for i := range all {
			if all[i].Slug == slug && all[i].Native {
				s.pluginAction(c, "activate", slug)
				return
			}
			if all[i].Slug == slug {
				d.Consent = &all[i]
				d.ConsentNonce = s.nonce(c, "activate-plugin_"+slug)
			}
		}
	}
	var notices []notice
	switch {
	case c.Query("activate") == "true":
		notices = append(notices, notice{Type: "success", Message: "Plugin activated.", Dismissible: true})
	case c.Query("deactivate") == "true":
		notices = append(notices, notice{Type: "success", Message: "Plugin deactivated.", Dismissible: true})
	case c.Query("activate-multi") != "":
		notices = append(notices, notice{Type: "success", Message: "Selected plugins activated.", Dismissible: true})
	case c.Query("deactivate-multi") != "":
		notices = append(notices, notice{Type: "success", Message: "Selected plugins deactivated.", Dismissible: true})
	case c.Query("deleted-multi") != "":
		notices = append(notices, notice{Type: "success", Message: "The selected plugins have been deleted.", Dismissible: true})
	case c.Query("deleted") == "true":
		notices = append(notices, notice{Type: "success", Message: "The selected plugin has been deleted.", Dismissible: true})
	case c.Query("updated") != "":
		notices = append(notices, notice{Type: "success", Message: template.HTML("Plugin updated: <strong>" + template.HTMLEscapeString(c.Query("updated")) + "</strong>."), Dismissible: true})
	case c.Query("installed") != "":
		notices = append(notices, notice{Type: "success", Message: template.HTML("Plugin <strong>" + template.HTMLEscapeString(c.Query("installed")) + "</strong> installed. Activate it to review and grant its permissions."), Dismissible: true})
	case c.Query("error") != "":
		notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(c.Query("error")))})
	}
	if slug := c.Query("replace"); slug != "" && plugins.ValidSlug(slug) && u.Can("install_plugins", nil) {
		if _, err := os.Stat(s.pluginPendingPath(slug)); err == nil {
			link := "/wp-admin/plugin-install?action=replace-plugin&plugin=" + url.QueryEscape(slug) + "&_wpnonce=" + s.nonce(c, "replace-plugin_"+slug)
			notices = append(notices, notice{Type: "warning", Message: template.HTML("Replace the installed <strong>" + template.HTMLEscapeString(slug) +
				"</strong> with the uploaded package anyway? Only do this if you trust where the package comes from. <a class=\"button\" href=\"" + template.HTMLEscapeString(link) + "\">Replace current with uploaded</a>")})
		}
	}
	if sk := c.Query("skipped"); sk != "" {
		notices = append(notices, notice{Type: "warning", Message: template.HTML("Skipped: <strong>" + template.HTMLEscapeString(sk) +
			"</strong>. Plugins that ask for permissions are activated one by one, from their own Activate link, so you can review what they may do; built-in and active plugins cannot be deleted."), Dismissible: true})
	}
	s.adminPage(c, "plugins", "Plugins", "plugins-php", "admin/plugins", d, notices...)
}

func (s *Server) pluginAction(c *gin.Context, action, slug string) {
	defer mcpPushAll("notifications/tools/list_changed") // MCP tools of built-in plugins appear and disappear
	if !s.checkNonce(c, action+"-plugin_"+slug) {
		s.denyNonce(c)
		return
	}
	fail := func(err error) {
		c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape(err.Error()))
	}
	if action == "activate" || action == "deactivate" {
		defer s.asAuditPlugin(c, action, slug)
	}
	if action == "activate" { // the first activation asks: setup guide or settings? (plugin_setup.go)
		s.pluginSetupOnActivate(c, slug)
	}
	if action == "activate" || action == "deactivate" || action == "delete" { // statistics (hub_telemetry.go)
		ver := s.pluginVersion(c.Request.Context(), slug)
		defer func() {
			if loc := c.Writer.Header().Get("Location"); c.Writer.Status() == http.StatusFound && !strings.Contains(loc, "error=") {
				s.hubEvent("plugin_"+action, "plugin", slug, ver)
			}
		}()
	}
	switch slug {
	case asSlug: // AdminSecGo
		if err := s.adminsecPluginAction(c, action); err != nil {
			fail(err)
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	case "classic-editor":
		_ = s.settings.Set(c, map[string]any{"native_classic_editor": action == "activate"})
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	case "wpgo-cache":
		_ = s.settings.Set(c, map[string]any{"wpgo_cache_enabled": action == "activate"})
		s.configureCache()
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	case bmgrSlug: // Backup Manager
		if err := s.bmgrPluginAction(c, action); err != nil {
			fail(err)
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	case maintSlug: // Maintenance-Go
		if err := s.maintPluginAction(c, action); err != nil {
			fail(err)
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	case msSlug:
		var err error
		if action == "activate" {
			err = s.mailSMTPActivate(c)
		} else {
			err = s.mailSMTPDeactivate(c)
		}
		if err != nil {
			fail(err)
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	case acfgoSlug:
		toggle := s.acfgoDeactivate
		if action == "activate" {
			toggle = s.acfgoActivate
		}
		if err := toggle(c); err != nil {
			fail(err)
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	case golylangSlug:
		if err := s.golylangSwitch(c, action == "activate"); err != nil {
			fail(err)
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	case seogoSlug:
		s.seogoPluginAction(c, action)
		return
	case flipbookSlug: // Pursae Flipbook (flipbook.go)
		s.flipbookPluginAction(c, action)
		return
	case gooikeSlug: // Gooike (gooike.go)
		s.gooikePluginAction(c, action)
		return
	case pixelSlug: // Pursae Pixel (pixelgo.go)
		s.pixelPluginAction(c, action)
		return
	case analyticsSlug: // Pursae Analytics (analytics.go)
		s.analyticsPluginAction(c, action)
		return
	case forumSlug: // Pursae Forum (forum.go)
		s.forumPluginAction(c, action)
		return
	case gooSlug: // Goocommerce (goocommerce.go)
		s.gooPluginAction(c, action)
		return
	case gocdnSlug: // GoCDN (gocdn.go)
		s.gocdnPluginAction(c, action)
		return
	case hgSlug: // HeadlessGo (headlessgo.go)
		s.hgPluginAction(c, action)
		return
	case webbuilder.PluginSlug: // Pursae Web Builder: a plain switch (webbuilder_settings.go)
		if err := s.wbSetActive(c, action == "activate"); err != nil {
			fail(err)
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	case gontactSlug:
		var err error
		if action == "activate" {
			err = s.gontactActivate(c, currentUser(c).ID)
		} else {
			err = s.gontactDeactivate(c)
		}
		if err != nil {
			fail(err)
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
		return
	}
	// Installed plugins are addressed by slug: it must look like one and name a plugin that is installed,
	// otherwise the slug would be joined into a file path ("../x") and load a package that was never installed.
	if !plugins.ValidSlug(slug) {
		fail(errors.New("Invalid plugin."))
		return
	}
	var installed bool
	if err := s.store.Pool.QueryRow(c.Request.Context(), `SELECT EXISTS(SELECT 1 FROM plugins WHERE slug = $1)`, slug).Scan(&installed); err != nil || !installed {
		fail(errors.New("That plugin is not installed."))
		return
	}
	ctx := c.Request.Context()
	switch action {
	case "activate":
		// Grant exactly the permissions the admin ticked on the consent screen.
		var granted []plugins.Permission
		for _, p := range c.PostFormArray("permissions[]") {
			if pp := plugins.Permission(p); pp.Valid() {
				granted = append(granted, pp)
			}
		}
		raw, _ := json.Marshal(granted)
		if missing := s.missingPluginDependencies(ctx, slug); len(missing) > 0 {
			fail(fmt.Errorf("The plugin could not be activated: it requires %s to be active first.", strings.Join(missing, ", ")))
			return
		}
		if s.plugins.rt != nil {
			_ = s.plugins.rt.Unload(ctx, slug) // not loaded yet is fine
		}
		if err := s.loadPlugin(ctx, slug, raw); err != nil {
			fail(err)
			return
		}
		if _, err := s.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'active', permissions = $2, disabled_reason = '', disabled_at = NULL, missing_permissions = '[]' WHERE slug = $1`, slug, raw); err != nil {
			if s.plugins.rt != nil { // the database is the truth: do not keep it running in memory only
				_ = s.plugins.rt.Unload(ctx, slug)
			}
			fail(fmt.Errorf("The plugin could not be activated: %v", err))
			return
		}
		s.firePluginLifecycle(ctx, slug, "activate")
		s.plugins.hooks.DoActionOwner(ctx, slug, "init")
		s.purgeAll(ctx)
	case "deactivate":
		s.firePluginLifecycle(ctx, slug, "deactivate") // while it is still loaded
		if _, err := s.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'inactive' WHERE slug = $1`, slug); err != nil {
			fail(fmt.Errorf("The plugin could not be deactivated: %v", err))
			return
		}
		if s.plugins.rt != nil {
			_ = s.plugins.rt.Unload(ctx, slug)
		}
		s.purgeAll(ctx)
	case "delete":
		if !currentUser(c).Can("delete_plugins", nil) { // activate_plugins alone does not allow deleting
			fail(errors.New("Sorry, you are not allowed to delete plugins on this site."))
			return
		}
		// Like WordPress, an active plugin is deactivated before it can be deleted (the screen hides the
		// link, but the request can still be sent).
		var status string
		if err := s.store.Pool.QueryRow(ctx, `SELECT status FROM plugins WHERE slug = $1`, slug).Scan(&status); err != nil || status == "active" {
			fail(errors.New("Deactivate the plugin before deleting it."))
			return
		}
		s.uninstallPluginHook(ctx, slug)
		if s.plugins.rt != nil {
			_ = s.plugins.rt.Unload(ctx, slug)
		}
		// Database first: a failed DELETE keeps the package, so the row never points at a missing file.
		if _, err := s.store.Pool.Exec(ctx, `DELETE FROM plugins WHERE slug = $1`, slug); err != nil {
			fail(fmt.Errorf("The plugin could not be deleted: %v", err))
			return
		}
		s.purgePluginData(ctx, slug)
		_ = os.Remove(filepath.Join(s.plugins.dir, filepath.Base(slug)+".zip"))
		_ = os.Remove(filepath.Join(s.plugins.dir, filepath.Base(slug)+".zip.prev"))
		_ = os.Remove(s.pluginPendingPath(slug))
		c.Redirect(http.StatusFound, "/wp-admin/plugins?deleted=true")
		return
	default:
		c.Redirect(http.StatusFound, "/wp-admin/plugins")
		return
	}
	c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
}

// pluginLifecycleBudget bounds one activate/deactivate/uninstall callback.
const pluginLifecycleBudget = 5 * time.Second

// firePluginLifecycle runs the plugin's own callbacks for hook ("activate", "deactivate",
// "uninstall") and nobody else's, with a short budget. A callback that fails or hangs never blocks
// the state change.
func (s *Server) firePluginLifecycle(ctx context.Context, slug, hook string) {
	if s.plugins == nil || s.plugins.rt == nil {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, pluginLifecycleBudget)
	defer cancel()
	s.plugins.hooks.DoActionOwner(lctx, slug, hook)
}

// uninstallPluginHook gives a plugin about to be deleted its "uninstall" action: an inactive plugin
// is loaded just for that call (with the permissions it was last granted) and unloaded again.
func (s *Server) uninstallPluginHook(ctx context.Context, slug string) {
	if s.plugins == nil || s.plugins.rt == nil {
		return
	}
	if _, loaded := s.plugins.rt.Plugin(slug); !loaded {
		var perms []byte
		if err := s.store.Pool.QueryRow(ctx, `SELECT permissions FROM plugins WHERE slug = $1`, slug).Scan(&perms); err != nil {
			return
		}
		// A plugin that was never activated has no granted permissions, so its start-up code (hooks
		// it registers on load) meets permission_denied refusals that are expected here: keep them
		// out of the server log at error level. Its uninstall callback below logs normally.
		s.plugins.rt.SetLogsQuiet(slug, true)
		err := s.loadPlugin(ctx, slug, perms)
		s.plugins.rt.SetLogsQuiet(slug, false)
		if err != nil {
			return
		}
	}
	s.firePluginLifecycle(ctx, slug, "uninstall")
}

// purgePluginData removes what a deleted Wasm plugin left outside plugin_data (which cascades with
// its plugins row): its scheduled events, its options (wasm:<slug>:*) and its transients
// (plugin:<slug>:*). A later package with the same slug, possibly from another publisher, must not
// inherit them, and the events would otherwise stay in the job table for good.
func (s *Server) purgePluginData(ctx context.Context, slug string) {
	if !plugins.ValidSlug(slug) { // slugs are [a-z0-9-]: no LIKE metacharacters
		return
	}
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM jobs WHERE payload->>'plugin' = $1`, slug)
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM transients WHERE key LIKE $1`, transientKey(slug, "%"))
	if tag, err := s.store.Pool.Exec(ctx, `DELETE FROM settings WHERE key LIKE $1`, pluginOptionKey(slug, "%")); err == nil && tag.RowsAffected() > 0 {
		_ = s.settings.Load(ctx)
	}
}

// installPackage verifies an uploaded package and stores it as an inactive plugin.
func (s *Server) installPackage(ctx context.Context, data []byte) (*plugins.Manifest, error) {
	return s.installPackageFrom(ctx, data, pluginSourceUpload, "")
}

// pluginReplaceError refuses to replace an installed plugin with an older version or with a package
// signed by another key than the installed one. An upload can be confirmed by the administrator
// (WordPress' "Replace current with uploaded"); the Manager's updates are always refused.
type pluginReplaceError struct{ Slug, Reason string }

func (e *pluginReplaceError) Error() string { return e.Reason }

// builtinPluginSlug reports whether slug belongs to a built-in plugin (or the name its Plugins screen
// links use): a package may never take it, or its options and screens would mix with the built-in's.
func (s *Server) builtinPluginSlug(slug string) bool {
	for _, r := range s.nativePluginRows() {
		if r.Slug == slug || pluginURLSlug(r.Slug) == slug {
			return true
		}
	}
	return false
}

// pluginSigner is the stored form of the key that verified a package ("" when unsigned).
// allowUnsignedPlugins reports whether unsigned plugin packages are accepted: the plugin_allow_unsigned
// setting and the PURSAE_ALLOW_UNSIGNED_PLUGINS environment opt-in must both be on. The setting alone is a
// database row any administrator (or the CLI) can write, too weak a switch for turning signature checks off.
func (s *Server) allowUnsignedPlugins() bool {
	if !s.settings.Bool("plugin_allow_unsigned") {
		return false
	}
	if !s.cfg.AllowUnsignedPlugins {
		unsignedIgnoredOnce.Do(func() {
			slog.Warn("plugin_allow_unsigned is on but ignored: set PURSAE_ALLOW_UNSIGNED_PLUGINS=1 in the server environment to accept unsigned plugin packages (development only)")
		})
		return false
	}
	return true
}

var unsignedIgnoredOnce sync.Once

func pluginSigner(pkg *plugins.Package) string {
	if pkg.SignedBy == nil {
		return ""
	}
	return hex.EncodeToString(pkg.SignedBy)
}

// pluginReplaceReason says why pkg should not silently replace the installed version (installed,
// signed by installedSigner), or "". A package signed by another key is a different publisher, except
// when both keys are Pursae Manager's (the Manager re-signs every package and rotates its key).
func (s *Server) pluginReplaceReason(installed, installedSigner string, pkg *plugins.Package) string {
	m := pkg.Manifest
	if versionLess(m.Version, installed) {
		return fmt.Sprintf("the package is %s %s, older than the installed version %s", m.Name, m.Version, installed)
	}
	signer := pluginSigner(pkg)
	if signer == installedSigner {
		return ""
	}
	if installedSigner == "" { // a local unsigned build: a publisher's package of the same slug is another plugin
		return fmt.Sprintf("the installed %s is unsigned (a local build), the package is signed by a publisher", m.Name)
	}
	if old, err := hex.DecodeString(installedSigner); err == nil && pkg.SignedBy != nil &&
		s.plugins.isHubKey(ed25519.PublicKey(old)) && s.plugins.isHubKey(pkg.SignedBy) {
		return ""
	}
	if signer == "" {
		return fmt.Sprintf("the installed %s is signed, the package is not", m.Name)
	}
	return fmt.Sprintf("the package is signed by another key than the installed %s (a different publisher)", m.Name)
}

// installPackageFrom verifies a package and stores it: a new plugin is inactive, an installed one
// keeps its status (the caller reloads it). source is hub or upload; with wantSlug set the package
// must be that plugin. An older version or another signer than the installed one is refused with a
// *pluginReplaceError.
func (s *Server) installPackageFrom(ctx context.Context, data []byte, source, wantSlug string) (*plugins.Manifest, error) {
	return s.installPackageChecked(ctx, data, source, wantSlug, false)
}

// unverifiedWarned holds the digests of unsigned packages already warned about (installPackageChecked).
var unverifiedWarned sync.Map

// installPackageChecked is installPackageFrom; confirmed skips the downgrade and signer checks (the
// administrator confirmed the replacement of an uploaded package).
func (s *Server) installPackageChecked(ctx context.Context, data []byte, source, wantSlug string, confirmed bool) (*plugins.Manifest, error) {
	pkg, err := plugins.LoadPackage(bytes.NewReader(data), int64(len(data)), s.plugins.trusted(), !s.allowUnsignedPlugins())
	if err != nil {
		if errors.Is(err, plugins.ErrUnsigned) || errors.Is(err, plugins.ErrBadSignature) {
			return nil, fmt.Errorf("the package signature could not be verified (%v); only plugins signed with a trusted key can be installed; a site owner can trust a publisher's key under Plugins > Trusted Keys", err)
		}
		return nil, err
	}
	if err := pkg.Manifest.CheckCompatible(Version); err != nil {
		return nil, err
	}
	slug := pkg.Manifest.Slug
	// Once per package: one install can check the same bytes more than once (a refused replacement
	// confirmed with "Replace current with uploaded", a retry from the CLI or the Manager).
	if !pkg.Verified() {
		if _, seen := unverifiedWarned.LoadOrStore(fmt.Sprintf("%x", pkg.Digest), true); !seen {
			slog.Warn("installing a plugin package whose signature was not verified (plugin_allow_unsigned and PURSAE_ALLOW_UNSIGNED_PLUGINS are on; development only)", "plugin", slug)
		}
	}
	if wantSlug != "" && slug != wantSlug {
		return nil, fmt.Errorf("the package is the plugin %q, not %q", slug, wantSlug)
	}
	if s.builtinPluginSlug(slug) {
		return nil, errors.New("that slug belongs to a built-in plugin")
	}
	var before, beforeSigner string
	_ = s.store.Pool.QueryRow(ctx, `SELECT version, signer FROM plugins WHERE slug = $1`, slug).Scan(&before, &beforeSigner)
	if before != "" && !confirmed {
		if reason := s.pluginReplaceReason(before, beforeSigner, pkg); reason != "" {
			return nil, &pluginReplaceError{Slug: slug, Reason: reason}
		}
	}
	zipPath := filepath.Join(s.plugins.dir, slug+".zip")
	prev, prevErr := os.ReadFile(zipPath) //nolint:gosec // G703: slug passed manifest validation
	if err := writeFileAtomic(zipPath, data, 0o644); err != nil {
		return nil, err
	}
	// A Hub plugin stays a Hub plugin when the same Hub-signed package is uploaded over it: otherwise the
	// upload would silently stop its updates. A package from another publisher (confirmed) is an upload.
	hubSigned := pkg.SignedBy != nil && s.plugins.isHubKey(pkg.SignedBy)
	_, err = s.store.Pool.Exec(ctx, `INSERT INTO plugins (slug, version, signature, source, signer) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (slug) DO UPDATE SET version = EXCLUDED.version, signature = EXCLUDED.signature,
			source = CASE WHEN plugins.source = 'hub' AND $6 THEN plugins.source ELSE EXCLUDED.source END,
			signer = EXCLUDED.signer, updated_at = now()`,
		slug, pkg.Manifest.Version, fmt.Sprintf("%x", pkg.Digest), source, pluginSigner(pkg), hubSigned)
	if err != nil {
		// Keep the disk in step with the database: the row still names the previous package.
		if prevErr == nil {
			_ = writeFileAtomic(zipPath, prev, 0o644)
		} else if before == "" {
			_ = os.Remove(zipPath) //nolint:gosec // G703: slug passed manifest validation
		}
		return nil, err
	}
	if before == "" {
		s.hubEvent("plugin_install", "plugin", slug, pkg.Manifest.Version)
	} else {
		s.hubEvent("plugin_update", "plugin", slug, pkg.Manifest.Version)
	}
	return pkg.Manifest, nil
}

// pluginPendingPath is where an upload waiting for "Replace current with uploaded" is kept.
func (s *Server) pluginPendingPath(slug string) string {
	return filepath.Join(s.plugins.dir, ".pending-"+filepath.Base(slug)+".zip")
}

// reloadPlugin loads a replaced package of an active plugin with the permissions granted before that
// the new version still declares (new ones need the consent screen).
func (s *Server) reloadPlugin(ctx context.Context, slug string, m *plugins.Manifest, perms []byte) error {
	var granted []plugins.Permission
	_ = json.Unmarshal(perms, &granted)
	keep, missing := splitPermissions(granted, m.Permissions)
	raw, _ := json.Marshal(keep)
	missingRaw, _ := json.Marshal(missing)
	if s.plugins.rt != nil {
		_ = s.plugins.rt.Unload(ctx, slug)
	}
	if err := s.loadPlugin(ctx, slug, raw); err != nil {
		return err
	}
	_, _ = s.store.Pool.Exec(ctx, `UPDATE plugins SET permissions = $2, missing_permissions = $3 WHERE slug = $1`, slug, raw, missingRaw)
	s.plugins.hooks.DoActionOwner(ctx, slug, "init")
	return nil
}

// extActorCtx carries the user installing a plugin or theme package from a web request (withExtActor);
// the CLI sets none and may replace active plugins and installed themes.
type extActorCtx struct{}

func withExtActor(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, extActorCtx{}, u)
}

// errThemeUpdateDenied refuses to overwrite an installed theme for a user without update_themes.
var errThemeUpdateDenied = errors.New("Sorry, you are not allowed to update themes on this site.")

// errPluginReloadDenied refuses a new package for an active plugin to a user without activate_plugins.
var errPluginReloadDenied = errors.New("Sorry, you are not allowed to update an active plugin: that needs the activate_plugins capability.")

// pluginReplaceUpload is "Replace current with uploaded": it installs the upload kept by the refused
// install and, when the plugin is active, loads the new package.
func (s *Server) pluginReplaceUpload(ctx context.Context, slug string) (*plugins.Manifest, error) {
	if !plugins.ValidSlug(slug) {
		return nil, errors.New("Invalid plugin.")
	}
	path := s.pluginPendingPath(slug)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("The uploaded package is no longer available; upload it again.")
	}
	m, _, err := s.installAndReload(ctx, data, pluginSourceUpload, slug, true)
	if m != nil {
		_ = os.Remove(path)
	}
	return m, err
}

// installAndReload installs a package (installPackageChecked) and, when it replaced an active
// plugin, loads the new version with the permissions it still declares, so the running code always
// matches the installed version. The previous package stays as <slug>.zip.prev; when the new
// version cannot be loaded the previous one is put back. updated reports that the slug was already
// installed. A non-nil manifest with an error means the package was installed but is not running.
func (s *Server) installAndReload(ctx context.Context, data []byte, source, wantSlug string, confirmed bool) (m *plugins.Manifest, updated bool, err error) {
	slug := wantSlug
	if slug == "" {
		pkg, err := plugins.LoadPackage(bytes.NewReader(data), int64(len(data)), nil, false)
		if err != nil { // let the checked install report it
			m, err := s.installPackageChecked(ctx, data, source, wantSlug, confirmed)
			return m, false, err
		}
		slug = pkg.Manifest.Slug
	}
	var installed, status, oldSource, digest, signer string
	var perms []byte
	if plugins.ValidSlug(slug) {
		_ = s.store.Pool.QueryRow(ctx, `SELECT version, status, source, permissions, signature, signer FROM plugins WHERE slug = $1`, slug).
			Scan(&installed, &status, &oldSource, &perms, &digest, &signer)
	}
	updated = installed != ""
	if status == "active" {
		if u, ok := ctx.Value(extActorCtx{}).(*store.User); ok && !u.Can("activate_plugins", nil) {
			// Replacing an active plugin runs the new code at once: that is activating it.
			return nil, updated, errPluginReloadDenied
		}
	}
	zipPath := filepath.Join(s.plugins.dir, filepath.Base(slug)+".zip")
	var prev []byte
	prevErr := errors.New("not installed")
	if updated {
		if prev, prevErr = os.ReadFile(zipPath); prevErr == nil {
			if err := writeFileAtomic(zipPath+".prev", prev, 0o644); err != nil {
				return nil, updated, err
			}
		}
	}
	m, err = s.installPackageChecked(ctx, data, source, wantSlug, confirmed)
	if err != nil {
		return nil, updated, err
	}
	if m.Slug != slug { // the package named another plugin than its manifest said on the first read
		return m, updated, nil
	}
	if status == "active" {
		if err := s.reloadPlugin(ctx, slug, m, perms); err != nil {
			if prevErr == nil && writeFileAtomic(zipPath, prev, 0o644) == nil { // roll back to the previous package
				_, _ = s.store.Pool.Exec(ctx, `UPDATE plugins SET version = $2, signature = $3, signer = $4, permissions = $5, source = $6 WHERE slug = $1`,
					slug, installed, digest, signer, perms, oldSource)
				if lerr := s.loadPlugin(ctx, slug, perms); lerr == nil {
					s.plugins.hooks.DoActionOwner(ctx, slug, "init")
					s.purgeAll(ctx)
					return nil, updated, fmt.Errorf("%s %s could not be activated (%v); version %s was restored", m.Name, m.Version, err, installed)
				}
			}
			_, _ = s.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'inactive' WHERE slug = $1`, slug)
			s.purgeAll(ctx)
			return m, updated, fmt.Errorf("%s was replaced by %s but could not be activated again: %v", m.Name, m.Version, err)
		}
	}
	if updated {
		s.purgeAll(ctx)
	}
	return m, updated, nil
}

// pluginInstalledRedirect is where an install from the upload form or the marketplace lands: a new
// plugin waits for activation, a replaced one reports the version now running.
func pluginInstalledRedirect(m *plugins.Manifest, updated bool) string {
	if updated {
		return "/wp-admin/plugins?updated=" + url.QueryEscape(m.Name+" "+m.Version)
	}
	return "/wp-admin/plugins?installed=" + url.QueryEscape(m.Name)
}

// writeFileAtomic writes a file through a temporary file in the same directory and a rename, so a
// reader (the plugin runtime) never sees half a package.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

type marketItem struct {
	Slug, Name, Version, Description, Author string
	Permissions                              []permRow
	Installed                                bool
	InstallURL                               string
	Icon                                     string
	IconColor                                string // wpgo-plugin-icon-<color> background for the generated icon
	// Marketplace (Hub) listings.
	Hub       bool
	IconURL   string
	Homepage  string
	Downloads int
	Updated   string
	Access    string // public | pro | private (Manager listings)
	Channel   string // stable | beta
	// Compatibility with the running core (Manager listings): "Requires Pursae X", "Tested up to Y".
	RequiresCore, TestedCore string
	Incompatible             bool // the listing needs a newer core than this one: Install is disabled
	Tags                     []string
	// Native (built-in) plugins.
	Native, Active           bool
	SettingsURL, ActivateURL string
	NativeKind               string // "Built in" note shown in the card footer
}

// hubIncompatible reports whether a listing that requires core version req cannot run on this build.
// A build without a release version number (a dev build) is never reported incompatible.
func hubIncompatible(req string) bool {
	req = strings.TrimSpace(req)
	if req == "" || Version == "" || Version[0] < '0' || Version[0] > '9' {
		return false
	}
	return versionLess(Version, req)
}

type pluginInstallData struct {
	Tag         string
	Tab         string
	Search      string
	Items       []marketItem
	Counts      map[string]int
	UploadNonce string
	HubEnabled  bool // this build talks to a Pursae Hub (drives the empty-marketplace message)
}

// pluginInstall is plugin-install.php: a small marketplace and the zip upload form.
func (s *Server) pluginInstall(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("install_plugins", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to install plugins on this site.")
		return
	}
	ctx := withExtActor(c.Request.Context(), u)
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "plugin-upload") {
			s.denyNonce(c)
			return
		}
		f, _, err := c.Request.FormFile("pluginzip")
		if err != nil {
			c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape("No package was uploaded."))
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, pluginMaxPackage+1))
		f.Close()
		if err != nil || len(data) > pluginMaxPackage {
			c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape(fmt.Sprintf("The package is larger than %d MiB.", pluginMaxPackage>>20)))
			return
		}
		m, updated, err := s.installAndReload(ctx, data, pluginSourceUpload, "", false)
		var replace *pluginReplaceError
		if errors.As(err, &replace) {
			// Like WordPress' "Replace current with uploaded": keep the upload and ask.
			if werr := writeFileAtomic(s.pluginPendingPath(replace.Slug), data, 0o600); werr == nil {
				c.Redirect(http.StatusFound, "/wp-admin/plugins?replace="+url.QueryEscape(replace.Slug)+"&error="+url.QueryEscape("Installation stopped: "+err.Error()+"."))
				return
			}
		}
		if err != nil {
			c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape("Installation failed: "+err.Error()))
			return
		}
		c.Redirect(http.StatusFound, pluginInstalledRedirect(m, updated))
		return
	}
	if c.Query("action") == "replace-plugin" { // Replace current with uploaded
		slug := c.Query("plugin")
		if !s.checkNonce(c, "replace-plugin_"+slug) {
			s.denyNonce(c)
			return
		}
		m, err := s.pluginReplaceUpload(ctx, slug)
		if err != nil {
			c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape("Installation failed: "+err.Error()))
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?updated="+url.QueryEscape(m.Name+" "+m.Version))
		return
	}
	if c.Query("action") == "install-plugin" {
		slug := c.Query("plugin")
		if !s.checkNonce(c, "install-plugin_"+slug) {
			s.denyNonce(c)
			return
		}
		if c.Query("source") == "hub" {
			m, updated, err := s.hubInstallPlugin(ctx, slug)
			if err != nil {
				c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape("Installation failed: "+err.Error()))
				return
			}
			c.Redirect(http.StatusFound, pluginInstalledRedirect(m, updated))
			return
		}
		for _, path := range s.plugins.market {
			pkg, err := plugins.LoadPackageFile(path, nil, false)
			if err != nil || pkg.Manifest.Slug != slug {
				continue
			}
			data, _ := os.ReadFile(path)
			m, updated, err := s.installAndReload(ctx, data, pluginSourceUpload, slug, false)
			if err != nil {
				c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape("Installation failed: "+err.Error()))
				return
			}
			c.Redirect(http.StatusFound, pluginInstalledRedirect(m, updated))
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugin-install?error=notfound")
		return
	}
	d := pluginInstallData{Tag: strings.ToLower(strings.TrimSpace(c.Query("tag"))), Tab: c.DefaultQuery("tab", "featured"), Search: strings.TrimSpace(c.Query("s")), UploadNonce: s.nonce(c, "plugin-upload")}
	installed := map[string]bool{}
	var native []marketItem
	icons := map[string][2]string{"classic-editor": {"dashicons-edit-page", "blue"}, "wpgo-cache": {"dashicons-performance", "green"}, webbuilder.PluginSlug: {"dashicons-layout", "purple"}, maintSlug: {"dashicons-hammer", "teal"}, msSlug: {"dashicons-email-alt", "blue"}, gontactSlug: {"dashicons-email", "teal"}, acfgoSlug: {"dashicons-welcome-widgets-menus", "green"}, golylangSlug: {"dashicons-translation", "teal"}, seogoSlug: {"dashicons-chart-line", "blue"}, gooikeSlug: {"dashicons-shield", "orange"}, flipbookSlug: {"dashicons-book-alt", "purple"}, pixelSlug: {"dashicons-chart-area", "purple"}, analyticsSlug: {"dashicons-chart-bar", "green"}, forumSlug: {"dashicons-format-chat", "blue"}, gooSlug: {"dashicons-cart", "purple"}, gocdnSlug: {"dashicons-cloud-upload", "blue"}, hgSlug: {"dashicons-rest-api", "blue"}}
	for _, r := range s.installedPlugins(c) {
		installed[r.Slug] = true
		if r.Native {
			ic := icons[r.Slug]
			native = append(native, marketItem{Slug: r.Slug, Name: r.Name, Version: r.Version, Description: r.Description, Author: r.Author,
				Native: true, Active: r.Active, SettingsURL: r.SettingsURL, ActivateURL: r.ActivateURL, Icon: ic[0], IconColor: ic[1], NativeKind: "Built in to Pursae"})
		}
	}
	var featured []marketItem
	var notices []notice
	// The marketplace: packages published on the WordPress Go Hub.
	if s.hubBase() != "" {
		items, err := s.hubCatalogFresh(ctx, "plugins") // live: a new Hub listing shows at once
		if err != nil {
			notices = append(notices, notice{Type: "warning", Message: template.HTML("Pursae Manager could not be reached (" + html.EscapeString(err.Error()) + "). Only locally available packages are listed."), Dismissible: true})
		}
		for _, h := range items {
			it := marketItem{Slug: h.Slug, Name: h.Name, Version: h.Version, Description: h.Description, Author: h.Author, Hub: true,
				IconURL: h.IconURL, Homepage: h.Homepage, Downloads: h.Downloads, Access: h.Access, Channel: h.Channel, Installed: installed[h.Slug], Icon: "dashicons-admin-plugins", IconColor: "teal",
				RequiresCore: h.RequiresCore, TestedCore: h.TestedCore, Incompatible: hubIncompatible(h.RequiresCore), Tags: h.Tags,
				InstallURL: "/wp-admin/plugin-install?action=install-plugin&source=hub&plugin=" + h.Slug + "&_wpnonce=" + s.nonce(c, "install-plugin_"+h.Slug)}
			if t, err := time.Parse(time.RFC3339, h.Updated); err == nil {
				it.Updated = t.In(s.loc()).Format("January 2, 2006")
			}
			for _, p := range h.Permissions {
				it.Permissions = append(it.Permissions, permRow{p, plugins.Permission(p).Description(), plugins.Permission(p).Dangerous()})
			}
			featured = append(featured, it)
		}
	}
	hubSlugs := map[string]bool{}
	for _, it := range featured {
		hubSlugs[it.Slug] = true
	}
	for _, path := range s.plugins.market {
		pkg, err := plugins.LoadPackageFile(path, nil, false)
		if err != nil {
			continue
		}
		m := pkg.Manifest
		if hubSlugs[m.Slug] {
			continue
		}
		it := marketItem{Slug: m.Slug, Name: m.Name, Version: m.Version, Description: m.Description, Author: m.Author,
			Installed: installed[m.Slug], InstallURL: "/wp-admin/plugin-install?action=install-plugin&plugin=" + m.Slug + "&_wpnonce=" + s.nonce(c, "install-plugin_"+m.Slug), Icon: "dashicons-admin-plugins", IconColor: "teal"}
		for _, p := range m.Permissions {
			it.Permissions = append(it.Permissions, permRow{string(p), p.Description(), p.Dangerous()})
		}
		featured = append(featured, it)
	}
	// Pursae Manager's listings first, in the order the Manager gives them (its operator's order);
	// then the local packages by name.
	sort.SliceStable(featured, func(i, j int) bool {
		if featured[i].Hub != featured[j].Hub {
			return featured[i].Hub
		}
		if featured[i].Hub {
			return false
		}
		return featured[i].Name < featured[j].Name
	})
	if d.Tag != "" { // filter the marketplace by tag
		kept := featured[:0:0]
		for _, it := range featured {
			for _, t := range it.Tags {
				if strings.EqualFold(t, d.Tag) {
					kept = append(kept, it)
					break
				}
			}
		}
		featured = kept
	}
	d.HubEnabled = s.hubBase() != ""
	d.Counts = map[string]int{"featured": len(featured), "native": len(native)}
	switch d.Tab {
	case "native":
		d.Items = native
	case "search":
		q := d.Search
		for _, it := range append(append([]marketItem{}, featured...), native...) {
			if q == "" || textfold.Contains(it.Name+" "+it.Description+" "+it.Author, q) { // "IŞIK" finds "ışık"
				d.Items = append(d.Items, it)
			}
		}
	case "upload":
		d.Items = featured
	default:
		d.Tab = "featured"
		d.Items = featured
	}
	body := "plugin-install-php"
	if d.Tab == "upload" {
		body += " show-upload-view"
	}
	s.adminPage(c, "plugin-install", "Add Plugins", body, "admin/plugin-install", d, notices...)
}

// pluginPostStats lets plugins add numbers to the classic editor's publish box (wpgo_post_stats).
func (s *Server) pluginPostStats(ctx context.Context, p *store.Post) map[string]any {
	out, _ := s.applyFilters(ctx, "wpgo_post_stats", map[string]any{}, map[string]any{"id": p.ID, "content": p.Content, "title": p.Title}).(map[string]any)
	return out
}

// statsLine renders plugin stats for the publish box.
func statsLine(st map[string]any) string {
	var parts []string
	keys := make([]string, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, strings.ReplaceAll(k, "_", " ")+": "+fmt.Sprint(st[k]))
	}
	return strings.Join(parts, " · ")
}
