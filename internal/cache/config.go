// Package cache implements wpgo-cache, the built-in full-page cache of WordPress Go.
//
// It works like the LiteSpeed Cache WordPress plugin but is server-agnostic: rendered
// public pages are cached inside the Go process (optionally persisted to disk), purged
// precisely by tags the application attaches to each response, and, depending on
// [Config.ServerHint], annotated with headers that let a front proxy (LiteSpeed,
// OpenLiteSpeed, nginx, a CDN) cache them as well.
package cache

import (
	"slices"
	"strings"
	"time"
)

// ServerHint selects which caching headers are emitted for a front proxy.
type ServerHint string

const (
	// HintAuto emits LiteSpeed headers when the request is detected to have come
	// through LiteSpeed/OpenLiteSpeed (X-LSCACHE request header or LSCACHE env var),
	// and nothing otherwise. It never makes an unknown proxy cache pages.
	HintAuto ServerHint = "auto"
	// HintLiteSpeed always emits X-LiteSpeed-Cache-Control and X-LiteSpeed-Tag.
	HintLiteSpeed ServerHint = "litespeed"
	// HintNginx emits X-Accel-Expires for nginx proxy_cache / fastcgi_cache.
	HintNginx ServerHint = "nginx"
	// HintGeneric emits a shared-cache Cache-Control (s-maxage) plus Surrogate-Key
	// and Cache-Tag headers understood by Varnish, Fastly and Cloudflare.
	HintGeneric ServerHint = "generic"
	// HintNone emits no proxy headers; only the in-process cache is used.
	HintNone ServerHint = "none"
)

// Default option values, matching LiteSpeed Cache's defaults where one exists.
const (
	DefaultPublicTTL    = 604800 * time.Second
	DefaultFrontPageTTL = 604800 * time.Second
	DefaultNotFoundTTL  = 3600 * time.Second
	searchTTL           = 600 * time.Second // upper bound for search result pages (?s=)
	// maxSearchEntries bounds the cached search result pages (memory and disk): every ?s= value is its own
	// entry, so random queries must not push posts and pages out of the LRU. Past it new searches are
	// served uncached until older ones expire or a purge drops them.
	maxSearchEntries    = 1000
	DefaultLoginCookie  = "wpgo_logged_in"
	DefaultMaxEntries   = 10000
	DefaultMaxBytes     = 256 << 20
	DefaultMaxEntrySize = 4 << 20
)

// DefaultExcludePaths are never cached: admin, login, REST, comment posting, XML-RPC
// and cron endpoints. Entries are path prefixes.
var DefaultExcludePaths = []string{
	"/wp-admin", "/wp-login", "/wp-json", "/wp-comments-post", "/xmlrpc", "/wp-cron",
}

// DefaultBypassCookies make a request bypass the cache when present (matched as cookie
// name prefixes, because WordPress appends a site hash to most of them).
// comment_author is the cookie this site sets (name, and the comment_author_email /
// comment_author_url prefixes). comment_author_ also covers WordPress's hashed names
// (comment_author_<hash>). A prefix match is used, so the bare name must be listed:
// "comment_author" does not start with "comment_author_".
var DefaultBypassCookies = []string{"wp-postpass", "comment_author", "comment_author_email", "comment_author_", "wordpress_logged_in_"}

// DefaultDropQueryStrings are tracking parameters removed from the cache key, so
// /post?utm_source=x is served from the same entry as /post. A trailing "*" matches a
// prefix.
var DefaultDropQueryStrings = []string{
	"utm_*", "fbclid", "gclid", "dclid", "msclkid", "mc_cid", "mc_eid", "_ga", "_gl", "ref", "yclid", "igshid",
}

// DefaultCacheQueryStrings are query parameters that are part of the cache key: the
// search query and pagination. Any other parameter makes the request bypass the cache.
var DefaultCacheQueryStrings = []string{"s", "paged", "page", "cpage"}

// Config holds the cache options. Use [DefaultConfig] and adjust fields; the zero
// value is a disabled cache.
type Config struct {
	// Enabled turns page caching on. When false the middleware is a pass-through
	// (internal X-WPGo-Cache-* headers are still stripped).
	Enabled bool

	// PublicTTL is how long ordinary public pages (posts, pages, archives) stay
	// cached. LiteSpeed: "Default Public Cache TTL". Default 604800s (7 days).
	PublicTTL time.Duration
	// FrontPageTTL is the TTL of the front page ("/"). LiteSpeed: "Default Front Page
	// TTL". Default 604800s.
	FrontPageTTL time.Duration
	// FeedTTL is the TTL of RSS/Atom feeds; 0 means feeds are not cached. LiteSpeed:
	// "Default Feed TTL". Default 0.
	FeedTTL time.Duration
	// NotFoundTTL is the TTL of 404 responses; 0 disables caching them. LiteSpeed:
	// "Default 404 Page TTL". Default 3600s.
	NotFoundTTL time.Duration
	// RedirectTTL is the TTL of 301 responses; 0 (default) disables caching them.
	RedirectTTL time.Duration

	// CacheLoggedIn caches pages for logged-in users in a private, per-session bucket
	// (keyed by a hash of the login cookie value; never shared and never handed to a
	// front proxy). LiteSpeed: "Cache Logged-in Users". Default false: logged-in
	// users bypass the cache.
	CacheLoggedIn bool
	// CacheMobileSeparately keeps separate mobile and desktop copies of each page,
	// chosen by User-Agent. LiteSpeed: "Cache Mobile". Default false.
	CacheMobileSeparately bool
	// MobileUserAgents are case-insensitive substrings that classify a User-Agent as
	// mobile when CacheMobileSeparately is on. Empty uses a built-in list.
	MobileUserAgents []string

	// CacheQueryStrings lists the query parameters that become part of the cache key
	// (e.g. "s", "paged"). A request carrying any parameter that is neither listed
	// here nor in DropQueryStrings bypasses the cache.
	CacheQueryStrings []string
	// DropQueryStrings lists parameters stripped from the cache key (tracking
	// parameters such as utm_*, fbclid, gclid). A trailing "*" matches a prefix.
	// LiteSpeed: "Drop Query String".
	DropQueryStrings []string

	// ExcludePaths are URL paths never cached. An entry without "*" is a prefix; an
	// entry with "*" is a wildcard matched against the whole path ("*" matches any
	// run of characters, including "/"). LiteSpeed: "Do Not Cache URIs".
	ExcludePaths []string
	// ExcludeCookies are cookie names (prefixes, or wildcards with "*") whose presence
	// makes a request bypass the cache. LiteSpeed: "Do Not Cache Cookies".
	ExcludeCookies []string
	// ExcludeUserAgents are case-insensitive User-Agent substrings that bypass the
	// cache. LiteSpeed: "Do Not Cache User Agents".
	ExcludeUserAgents []string
	// BypassCookies are session-like cookies (name prefixes) whose holders are served
	// uncached pages because the markup differs for them (password-protected posts,
	// pending comments). The login cookie ([Options.LoginCookie]) is handled on its
	// own: it bypasses the cache unless CacheLoggedIn is set. Default
	// [DefaultBypassCookies].
	BypassCookies []string

	// MaxEntries bounds the number of entries held in memory (LRU eviction).
	MaxEntries int
	// MaxBytes bounds the memory used by cached bodies and headers (LRU eviction).
	MaxBytes int64
	// MaxEntrySize is the largest body that is captured; bigger responses stream
	// through uncached.
	MaxEntrySize int64
	// DiskDir, when non-empty, persists entries to this directory so the cache
	// survives restarts. Files are written atomically; bodies are loaded lazily on
	// first request after a restart, while their tags are indexed at startup so tag
	// purges still reach them.
	DiskDir string

	// BrowserCacheTTL, when positive, adds "Cache-Control: public, max-age=N" to
	// static asset responses (images, CSS, JS, fonts) that have no Cache-Control of
	// their own. Headers only; assets are not stored. LiteSpeed: "Browser Cache TTL".
	BrowserCacheTTL time.Duration

	// ServerHint selects the front-proxy headers: auto, litespeed, nginx, generic or
	// none. Default auto.
	ServerHint ServerHint
	// ProxyMaxAge, when positive, caps the lifetime advertised to a front proxy
	// (s-maxage, X-Accel-Expires, X-LiteSpeed-Cache-Control). Use it while purges are not
	// forwarded to the proxy, so a changed page goes stale for minutes, not days.
	ProxyMaxAge time.Duration

	// TagPrefix is prepended to every tag sent to a front proxy (X-LiteSpeed-Tag,
	// Surrogate-Key, Cache-Tag, X-LiteSpeed-Purge) so several sites can share one
	// proxy. Internal tags are not prefixed. Default "wpgo_".
	TagPrefix string

	// Optimize configures the optional HTML optimizations applied once, when a page
	// is stored.
	Optimize OptimizeConfig
}

// OptimizeConfig toggles the conservative HTML optimizations of [Optimize]. Every
// optimization is off unless its flag is set.
type OptimizeConfig struct {
	// LazyImages adds loading="lazy" and decoding="async" to <img> tags (loading="lazy"
	// to <iframe> tags) that have no loading attribute, except the first LazySkipFirst
	// images; the first image gets fetchpriority="high".
	LazyImages bool
	// LazySkipFirst is the number of leading images left eager (the likely LCP
	// image). DefaultConfig sets 1.
	LazySkipFirst int
	// DeferScripts adds "defer" to classic external scripts in <body> whose handle is
	// in DeferAllowlist and that have no async/defer and are not modules.
	DeferScripts bool
	// DeferAllowlist holds script handles. A script matches when its id is the handle
	// or "<handle>-js" (WordPress's convention), or its file name without ".min.js" /
	// ".js" equals the handle; an entry with "/" or "." also matches as a URL fragment.
	DeferAllowlist []string
	// DeferAll defers every classic external <script> in <body> except those matching
	// DeferExclude (same matching as DeferAllowlist), instead of only the allowlisted
	// ones. Scripts with a dependent "<handle>-js-after" inline script stay untouched.
	DeferAll bool
	// DeferExclude lists handles or URL fragments DeferAll leaves alone (jquery, a
	// script that inline code relies on, ...).
	DeferExclude []string
	// DelayScripts lists patterns of scripts (analytics, pixels, chat widgets) held back
	// until the visitor's first interaction (mouse, touch, scroll, key) or DelayTimeout
	// seconds. An external script matches when its handle, id or URL contains the
	// pattern; an inline script matches when its text contains it.
	DelayScripts []string
	// DelayTimeout is the number of seconds after which delayed scripts load without an
	// interaction; 0 waits for an interaction only.
	DelayTimeout int
	// MinifyInlineCSS strips comments and whitespace from <style> elements.
	MinifyInlineCSS bool
	// Preconnect and DNSPrefetch list origins injected as <link rel="preconnect"> and
	// <link rel="dns-prefetch"> right after <head>.
	Preconnect  []string
	DNSPrefetch []string
	// CollapseWhitespace collapses runs of whitespace between tags into one space,
	// outside <pre>, <textarea>, <script>, <style> and comments, and drops HTML
	// comments except conditional and <!--noptimize--> ones.
	CollapseWhitespace bool
	// Preload lists critical assets injected as <link rel="preload"> right after <head>.
	Preload []PreloadAsset
}

// PreloadAsset is one <link rel="preload"> to inject.
type PreloadAsset struct {
	Href        string // asset URL
	As          string // "font", "style", "script", "image", ...
	Type        string // optional MIME type, e.g. "font/woff2"
	CrossOrigin bool   // adds crossorigin (required for fonts)
	// IfDocContains, when set, injects the preload only into documents that contain this text
	// (e.g. a theme's URI: its fonts are preloaded only on pages that load the theme, not on a
	// Pursae Web Builder canvas page, which has no theme stylesheet).
	IfDocContains string
}

func (o OptimizeConfig) enabled() bool {
	return o.LazyImages || o.DeferScripts || o.DeferAll || o.CollapseWhitespace || o.MinifyInlineCSS || len(o.Preload) > 0 ||
		len(o.DelayScripts) > 0 || len(o.Preconnect) > 0 || len(o.DNSPrefetch) > 0
}

// DefaultConfig returns an enabled configuration with LiteSpeed-like defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:           true,
		PublicTTL:         DefaultPublicTTL,
		FrontPageTTL:      DefaultFrontPageTTL,
		NotFoundTTL:       DefaultNotFoundTTL,
		CacheQueryStrings: append([]string(nil), DefaultCacheQueryStrings...),
		DropQueryStrings:  append([]string(nil), DefaultDropQueryStrings...),
		ExcludePaths:      append([]string(nil), DefaultExcludePaths...),
		BypassCookies:     append([]string(nil), DefaultBypassCookies...),
		MaxEntries:        DefaultMaxEntries,
		MaxBytes:          DefaultMaxBytes,
		MaxEntrySize:      DefaultMaxEntrySize,
		ServerHint:        HintAuto,
		TagPrefix:         "wpgo_",
		Optimize:          OptimizeConfig{LazySkipFirst: 1},
	}
}

// normalized fills unset limits with defaults.
func (c Config) normalized() Config {
	c = c.clone()
	if c.MaxEntries <= 0 {
		c.MaxEntries = DefaultMaxEntries
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.MaxEntrySize <= 0 {
		c.MaxEntrySize = DefaultMaxEntrySize
	}
	if c.ServerHint == "" {
		c.ServerHint = HintAuto
	}
	c.ServerHint = ServerHint(strings.ToLower(string(c.ServerHint)))
	return c
}

func (c Config) clone() Config {
	c.MobileUserAgents = slices.Clone(c.MobileUserAgents)
	c.CacheQueryStrings = slices.Clone(c.CacheQueryStrings)
	c.DropQueryStrings = slices.Clone(c.DropQueryStrings)
	c.ExcludePaths = slices.Clone(c.ExcludePaths)
	c.ExcludeCookies = slices.Clone(c.ExcludeCookies)
	c.ExcludeUserAgents = slices.Clone(c.ExcludeUserAgents)
	c.BypassCookies = slices.Clone(c.BypassCookies)
	c.Optimize.DeferAllowlist = slices.Clone(c.Optimize.DeferAllowlist)
	c.Optimize.DeferExclude = slices.Clone(c.Optimize.DeferExclude)
	c.Optimize.DelayScripts = slices.Clone(c.Optimize.DelayScripts)
	c.Optimize.Preconnect = slices.Clone(c.Optimize.Preconnect)
	c.Optimize.DNSPrefetch = slices.Clone(c.Optimize.DNSPrefetch)
	c.Optimize.Preload = slices.Clone(c.Optimize.Preload)
	return c
}

// matchWildcard reports whether s matches pattern, where "*" matches any run of
// characters (including none).
func matchWildcard(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return len(s) >= len(last) && strings.HasSuffix(s, last)
}

// matchPrefixOrWildcard treats patterns without "*" as prefixes.
func matchPrefixOrWildcard(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	if strings.Contains(pattern, "*") {
		return matchWildcard(pattern, s)
	}
	return strings.HasPrefix(s, pattern)
}
