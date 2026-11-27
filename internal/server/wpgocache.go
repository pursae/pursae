package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/cache"
	"wpgo/internal/media"
	"wpgo/internal/settings"
	"wpgo/internal/store"
)

// wpgo-cache is a native plugin: a LiteSpeed Cache style full-page cache that works behind any server
// (nginx, OpenLiteSpeed, Apache or none). Pages live in the Go process, are tagged while rendering and
// purged by tag when content changes; optional headers let a front proxy cache them too.

type pageCache struct {
	c *cache.Cache
	h http.Handler // c.Middleware(inner)
}

type warmState struct {
	mu      sync.Mutex
	running bool
	last    cache.WarmupProgress
	at      time.Time
	timer   *time.Timer
}

// cacheOptionKeys are the plugin's options, saved from its settings screens.
var cacheOptionKeys = map[string][]string{
	"wpgo-cache-settings": {"wpgo_cache_serve", "wpgo_cache_ttl_public", "wpgo_cache_ttl_front", "wpgo_cache_ttl_feed",
		"wpgo_cache_ttl_404", "wpgo_cache_mobile", "wpgo_cache_logged_in", "wpgo_cache_exclude_uri", "wpgo_cache_drop_qs",
		"wpgo_cache_server", "wpgo_cache_disk", "wpgo_cache_query_strings", "wpgo_cache_exclude_cookies", "wpgo_cache_exclude_ua",
		"wpgo_cache_browser_ttl", "wpgo_cache_redirect_ttl", "wpgo_cache_max_mb", "wpgo_cache_purge_url", "wpgo_cache_proxy_ttl",
		"wpgo_cache_preload_purge", "wpgo_cache_preload_daily"},
	"wpgo-cache-optimize": {"wpgo_opt_lazy", "wpgo_opt_lazy_skip", "wpgo_opt_defer", "wpgo_opt_defer_list", "wpgo_opt_minify_html",
		"wpgo_opt_preload_fonts", "wpgo_opt_defer_all", "wpgo_opt_defer_exclude", "wpgo_opt_delay_list", "wpgo_opt_delay_timeout",
		"wpgo_opt_minify_css", "wpgo_opt_preconnect", "wpgo_opt_dns_prefetch"},
}

// cacheConfig turns the plugin options into the engine configuration.
func (s *Server) cacheConfig() cache.Config {
	cfg := cache.DefaultConfig()
	st := s.settings
	cfg.Enabled = st.Bool("wpgo_cache_enabled") && st.Bool("wpgo_cache_serve")
	sec := func(k string) time.Duration { return time.Duration(st.Int(k)) * time.Second }
	cfg.PublicTTL, cfg.FrontPageTTL, cfg.FeedTTL, cfg.NotFoundTTL = sec("wpgo_cache_ttl_public"), sec("wpgo_cache_ttl_front"), sec("wpgo_cache_ttl_feed"), sec("wpgo_cache_ttl_404")
	cfg.CacheMobileSeparately = st.Bool("wpgo_cache_mobile")
	cfg.CacheLoggedIn = st.Bool("wpgo_cache_logged_in")
	cfg.ExcludePaths = append(cfg.ExcludePaths, lines(st.String("wpgo_cache_exclude_uri"))...)
	cfg.ExcludePaths, cfg.ExcludeCookies = s.gooCacheExcludes(cfg.ExcludePaths, cfg.ExcludeCookies) // Goocommerce: cart, checkout, my account
	cfg.DropQueryStrings = append(cfg.DropQueryStrings, lines(st.String("wpgo_cache_drop_qs"))...)
	cfg.CacheQueryStrings = append(cfg.CacheQueryStrings, lines(st.String("wpgo_cache_query_strings"))...)
	if s.forumOn() {
		// Pursae Forum's topic list sort and tag filter are page variants (unknown sorts are redirected
		// to the canonical URL, so arbitrary values do not each get an entry).
		cfg.CacheQueryStrings = append(cfg.CacheQueryStrings, forumCacheQueryStrings...)
	}
	cfg.ExcludeCookies = append(cfg.ExcludeCookies, lines(st.String("wpgo_cache_exclude_cookies"))...)
	cfg.ExcludeUserAgents = append(cfg.ExcludeUserAgents, lines(st.String("wpgo_cache_exclude_ua"))...)
	cfg.BrowserCacheTTL, cfg.RedirectTTL = sec("wpgo_cache_browser_ttl"), sec("wpgo_cache_redirect_ttl")
	if mb := st.Int("wpgo_cache_max_mb"); mb > 0 {
		cfg.MaxBytes = int64(mb) << 20
	}
	cfg.ServerHint = cache.ServerHint(st.String("wpgo_cache_server"))
	if proxyPurgeConfigured(cfg.ServerHint, st.String("wpgo_cache_purge_url")) {
		// purges are forwarded (wpgocache_proxy.go)
	} else if ttl := st.Int("wpgo_cache_proxy_ttl"); ttl > 0 && (cfg.ServerHint == cache.HintLiteSpeed || cfg.ServerHint == cache.HintNginx || cfg.ServerHint == cache.HintGeneric) {
		cfg.ProxyMaxAge = time.Duration(ttl) * time.Second // nothing purges the proxy: keep its copy short-lived
	}
	if st.Bool("wpgo_cache_disk") {
		cfg.DiskDir = s.cfg.UploadsDir + "/../wpgo-cache"
	}
	cfg.Optimize = cache.OptimizeConfig{
		LazyImages:         st.Bool("wpgo_opt_lazy"),
		LazySkipFirst:      st.Int("wpgo_opt_lazy_skip"),
		DeferScripts:       st.Bool("wpgo_opt_defer"),
		DeferAllowlist:     s.deferList(lines(st.String("wpgo_opt_defer_list"))),
		CollapseWhitespace: st.Bool("wpgo_opt_minify_html"),
		DeferAll:           st.Bool("wpgo_opt_defer_all"),
		DeferExclude:       lines(st.String("wpgo_opt_defer_exclude")),
		DelayScripts:       lines(st.String("wpgo_opt_delay_list")),
		DelayTimeout:       st.Int("wpgo_opt_delay_timeout"),
		MinifyInlineCSS:    st.Bool("wpgo_opt_minify_css"),
		Preconnect:         lines(st.String("wpgo_opt_preconnect")),
		DNSPrefetch:        lines(st.String("wpgo_opt_dns_prefetch")),
	}
	if st.Bool("wpgo_opt_preload_fonts") {
		// The fonts the active theme paints above the fold, so text renders without a late swap.
		if t := s.activeTheme(); t != nil {
			for _, f := range t.PreloadFonts {
				cfg.Optimize.Preload = append(cfg.Optimize.Preload, cache.PreloadAsset{
					Href: t.URI() + "/" + f, As: "font", Type: "font/woff2", CrossOrigin: true,
					IfDocContains: t.URI() + "/"}) // not on pages without the theme (builder canvas)
			}
		}
	}
	return cfg
}

// cacheSafeDefer are the core and default-theme scripts nothing inline depends on, deferred when
// "Defer JavaScript" is on and the list is left empty.
var cacheSafeDefer = []string{"comment-reply", "wp-embed", "twentytwentyfive-navigation", "skip-link-focus-fix", "keyboard-image-navigation"}

// deferList is the "Defer JavaScript" allowlist: the list from the settings, or when it is left empty
// the safe defaults plus the scripts of the active theme package (its theme.json scripts, which, like
// Twenty Twenty-Five's navigation script, nothing inline depends on). The engine's exclusions apply
// to all of them: only scripts in the body, none with an "-js-after" inline script, async, defer,
// nomodule or data-no-defer.
func (s *Server) deferList(v []string) []string {
	if len(v) > 0 {
		return v
	}
	out := append([]string{}, cacheSafeDefer...)
	if t := s.activeTheme(); t != nil && t.Package != nil {
		for _, sc := range t.Package.scripts {
			out = append(out, sc.Handle)
		}
	}
	return out
}

func lines(v string) []string {
	var out []string
	for _, l := range strings.Split(v, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// configureCache (re)builds the page cache from the current options. Entries are dropped, like
// LiteSpeed Cache purging everything when its settings change.
func (s *Server) configureCache() {
	if s.sandbox != nil { // a theme-preview sandbox is served without the page cache
		return
	}
	s.cacheBuildMu.Lock()
	defer s.cacheBuildMu.Unlock()
	if old := s.cache.Load(); old != nil {
		if old.h == nil {
			// CLI commands restore this purge-only cache after reusing an admin path.
			old.c.PurgeAll()
		} else {
			old.c.Retire()
		}
	}
	c := cache.New(s.cacheConfig(), cache.Options{LoginCookie: auth.CookieName, Logger: slog.Default(),
		OnPurge: s.onCachePurge, TrustForwarded: s.peerTrusted, CSPMode: s.cspCacheState}) // the other instances purge too (wpgocache_sync.go)
	s.dropCacheOfOtherCore(c)
	s.cache.Store(&pageCache{c: c, h: c.Middleware(s.appHandler())})
}

// Keep the shared setting for older cores that consult it after a rollback.
const cacheCoreVersionOption = "wpgo_cache_core_version"

const cacheCoreVersionFile = ".core-version"

// dropCacheOfOtherCore purges the pages persisted on disk when they were rendered by another core
// version: after a core update (or a rollback) they carry the old generator, asset versions and
// markup, and would otherwise be served until they expire.
func (s *Server) dropCacheOfOtherCore(c *cache.Cache) {
	recordedVersion := s.settings.String(cacheCoreVersionOption)
	needsPurge := recordedVersion != Version
	var marker string
	if dir := c.Config().DiskDir; dir != "" {
		marker = filepath.Join(dir, cacheCoreVersionFile)
		version, err := os.ReadFile(marker)
		needsPurge = err != nil || string(version) != Version
	}
	if needsPurge {
		var n int
		if recordedVersion == Version {
			// Another instance already announced the upgrade. Only this directory needs a purge.
			n = c.Apply(cache.Purge{All: true})
		} else {
			n = c.PurgeAll()
		}
		if n > 0 {
			slog.Info("page cache emptied after a core version change", "pages", n, "version", Version)
		}
		if marker != "" {
			if err := writeFileAtomic(marker, []byte(Version), 0o600); err != nil {
				slog.Warn("page cache: recording the disk cache version", "err", err)
			}
		}
	}
	if recordedVersion == Version {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.settings.Set(ctx, map[string]any{cacheCoreVersionOption: Version}); err != nil {
		slog.Warn("page cache: recording the core version", "err", err)
	}
}

func (s *Server) currentCache() *cache.Cache {
	if pc := s.cache.Load(); pc != nil {
		return pc.c
	}
	return nil
}

// Tagging ----------------------------------------------------------------------

// cacheTags labels a public response for tag purges. Only pages that show a widget area (recent posts,
// comments, archives, categories), a list block or a shortcode carry the blunt "widgets" tag, which
// every public post change purges; the rest carry what they list (site_memo.go): the terms and
// authors of their posts, the objects of their menus, "archive:<type>" for Twenty Twenty-Five's page
// navigation and More posts or for previous/next links, and their ancestors.
// cachePostTerms is every term of the post: from the request memo when the page loaded them already
// (prefetchPosts on the single view), else with one query.
func (s *Server) cachePostTerms(c *gin.Context, postID int64) []*store.Term {
	if m := viewMemoFrom(c.Request.Context()); m != nil {
		m.mu.Lock()
		ts, ok := m.terms[postID]
		m.mu.Unlock()
		if ok {
			return ts
		}
	}
	terms, err := s.store.TermsForPosts(c, []int64{postID}, "")
	if err != nil {
		return nil
	}
	return terms[postID]
}

func (s *Server) cacheTags(c *gin.Context, q *siteQuery) {
	widgets, deps := s.pageCacheDeps(c.Request.Context())
	var tags []string
	if widgets {
		tags = append(tags, "widgets")
	}
	tags = append(tags, deps...)
	switch q.Kind {
	case "home", "front-page":
		tags = append(tags, "home", "archive:post")
	case "single", "page", "attachment":
		if q.Post != nil {
			tags = append(tags, "post:"+itoa64(q.Post.ID), "author:"+itoa64(q.Post.AuthorID))
			for _, t := range s.cachePostTerms(c, q.Post.ID) {
				tags = append(tags, "term:"+itoa64(t.ID))
			}
			if q.Kind == "page" && q.Post.ID == int64(s.settings.Int("page_on_front")) {
				tags = append(tags, "home")
			}
		}
	case "category", "tag", "taxonomy":
		if q.Term != nil && q.Term.ID != 0 {
			tags = append(tags, "term:"+itoa64(q.Term.ID))
		} else {
			tags = append(tags, "archive:post") // post formats: any post change may move it in or out
		}
	case "author":
		if q.Author != nil {
			tags = append(tags, "author:"+itoa64(q.Author.ID))
		}
	case "date":
		d := fmt.Sprintf("date:%04d", q.Year)
		if q.Month > 0 {
			d += fmt.Sprintf("-%02d", q.Month)
			if q.Day > 0 {
				d += fmt.Sprintf("-%02d", q.Day)
			}
		}
		tags = append(tags, d, "archive:post")
	case "post-type-archive":
		tags = append(tags, "archive:"+q.PostType)
	case "search":
		tags = append(tags, "search")
	case "404":
		tags = append(tags, "404")
	}
	if q.Feed != "" {
		tags = []string{"feed"}
		if q.Post != nil {
			tags = append(tags, "post:"+itoa64(q.Post.ID))
		}
	}
	if q.Kind == "front-page" || q.Kind == "home" {
		c.Header(cache.HeaderTTL, strconv.Itoa(s.settings.Int("wpgo_cache_ttl_front")))
	}
	tags = append(tags, s.gooCacheTags(c, q)...) // Goocommerce: products and the shop (goo_front.go)
	if q.Forum != "" {
		tags = append(tags, forumCacheTag)             // Pursae Forum: every forum write purges every forum view
		tags = append(tags, forumTopicCacheTags(c)...) // and a topic page also its own (a like purges only it)
	}
	tags = append(tags, s.wbCacheTags(c, q)...) // Pursae Web Builder pages (webbuilder_hooks.go)
	c.Header(cache.HeaderTags, strings.Join(tags, ","))
}

// Purging ----------------------------------------------------------------------

// purgePost drops the pages a post change can affect: the post, every listing it appears on and,
// when it is (or was) public, the sidebar widgets that list recent posts.
func (s *Server) purgePost(ctx context.Context, p *store.Post) {
	if p != nil {
		s.gocdnPurgePostPages(ctx, p) // CDNs that cache HTML (gocdn_housekeeping.go)
	}
	c := s.currentCache()
	if c == nil || p == nil {
		return
	}
	tags := []string{"post:" + itoa64(p.ID), "author:" + itoa64(p.AuthorID), "feed", "home", "404", "search", "archive:" + p.Type}
	if p.PublishedAt != nil {
		t := p.PublishedAt.In(s.loc())
		tags = append(tags, t.Format("date:2006"), t.Format("date:2006-01"), t.Format("date:2006-01-02"))
	}
	if terms, err := s.store.TermsForPosts(ctx, []int64{p.ID}, ""); err == nil {
		for _, t := range terms[p.ID] {
			tags = append(tags, "term:"+itoa64(t.ID))
		}
	}
	if p.Status != "draft" && p.Status != "auto-draft" && p.Status != "pending" {
		tags = append(tags, "widgets")
	}
	if s.hgOn() {
		tags = append(tags, hgCacheTag) // Pursae Headless answers populate WordPress posts
	}
	c.PurgeTags(tags...)
}

// purgePostAgain repeats purgePost after a trash, restore or delete whose caller purged before
// making the change (so the terms were still there to purge): a page rendered between that purge
// and the change (the post's own page, the home page or an archive listing it) was stored with the
// post as it was and would be served, trashed post included, for the whole TTL. After a delete the
// terms are gone, but every listing of the post also carries its author's tag.
func (s *Server) purgePostAgain(ctx context.Context, p *store.Post) {
	s.purgePost(ctx, p)
}

// purgeTerm drops the pages a term change can affect: its archive and the pages that list or link
// it (they carry "term:<id>"), the sidebars, and the cached 404s (a new slug's URL may have one).
// Run it after the change, never before: a page rendered in between would be stored with the old
// term. Deleting a category moves its posts that have no other category into the default one,
// whose archive changes too.
func (s *Server) purgeTerm(t *store.Term, deleted bool) {
	if t == nil {
		return
	}
	tags := []string{"term:" + itoa64(t.ID), "widgets", "404"}
	if deleted {
		if def := int64(s.settings.Int("default_category")); t.Taxonomy == "category" && def > 0 && def != t.ID {
			tags = append(tags, "term:"+itoa64(def))
		}
	}
	s.purgeTags(tags...)
}

// purgeAttachment drops the pages that showed a media item (a featured image, a logo, a gallery the
// theme builds: site_memo.go tags them "media:<id>") once it was edited or deleted.
func (s *Server) purgeAttachment(a *store.Attachment) {
	if a != nil {
		s.purgeTags("media:" + itoa64(a.ID))
	}
}

func (s *Server) purgeTags(tags ...string) {
	if c := s.currentCache(); c != nil {
		c.PurgeTags(tags...)
	}
}

// Admin screens -------------------------------------------------------------------

type cacheScreen struct {
	Tab        string
	Stats      cache.Stats
	HitPct     string
	Size       string
	Enabled    bool
	Nonce      string
	PurgeNonce string
	Warm       cache.WarmupProgress
	Warming    bool
	WarmAt     string
	Server     string
	ServerOpts []choice
	TopTags    []tagCount
	ProxyWarn  string
}

type tagCount struct {
	Tag   string
	Count int
}

// pluginAdmin serves /wp-admin/admin?page=..., the screens registered by native plugins.
func (s *Server) pluginAdmin(c *gin.Context) {
	page := c.Query("page")
	if s.acfgoAdminPage(c) { // ACF-Go options pages
		return
	}
	if s.adminsecAdmin(c, page) { // AdminSecGo: admin?page=adminsecgo… (adminsec_admin.go)
		return
	}
	if page == bmgrSlug { // Backup Manager (backup_admin.go)
		s.bmgrAdmin(c)
		return
	}
	if page == flipbookSlug { // Pursae Flipbook (flipbook_admin.go)
		s.flipbookAdmin(c)
		return
	}
	if page == maintSlug { // Maintenance-Go (maintenance_admin.go)
		s.maintAdmin(c)
		return
	}
	if strings.HasPrefix(page, msSlug) { // WPGO Mail SMTP
		s.mailSMTPAdmin(c, page)
		return
	}
	if s.gontactAdmin(c) { // Gontact Form: admin?page=wpcf7… (gontact_admin.go)
		return
	}
	if s.golylangAdmin(c, page) { // Golylang: Languages screens (mlang, mlang_strings, mlang_settings)
		return
	}
	if s.wasmAdminPage(c, page) { // Wasm plugins: register_admin_page (plugins_ui.go)
		return
	}
	// Any other page is not registered: admin.php answers that with a 403 die, never a blank screen.
	if _, ok := cacheScreenTitles[page]; !ok {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	u := currentUser(c)
	// An inactive plugin's screens are gone, as in WordPress (the menu hides them too).
	if !u.Can("manage_options", nil) || !s.settings.Bool("wpgo_cache_enabled") {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	if c.Request.Method == http.MethodPost || c.Query("action") != "" {
		s.cacheAction(c, page)
		return
	}
	d := cacheScreen{Tab: page, Enabled: s.settings.Bool("wpgo_cache_enabled"), Nonce: s.nonce(c, page+"-options"),
		PurgeNonce: s.nonce(c, "wpgo-cache-purge"), Server: s.settings.String("wpgo_cache_server")}
	d.Enabled = s.settings.Bool("wpgo_cache_serve")
	d.ProxyWarn = s.cacheProxyWarning()
	if cc := s.currentCache(); cc != nil {
		d.Stats = cc.Stats()
		d.HitPct = fmt.Sprintf("%.1f%%", d.Stats.HitRatio*100)
		d.Size = media.HumanSize(d.Stats.Bytes)
		for t, n := range d.Stats.Tags {
			if strings.HasPrefix(t, "post:") || strings.HasPrefix(t, "term:") || t == "home" {
				d.TopTags = append(d.TopTags, tagCount{t, n})
			}
		}
	}
	for _, o := range [][2]string{{"auto", "Auto-detect (LiteSpeed headers only when LiteSpeed is in front)"}, {"litespeed", "LiteSpeed / OpenLiteSpeed"},
		{"nginx", "nginx (fastcgi/proxy cache)"}, {"generic", "Generic CDN or proxy (Cache-Control, Surrogate-Key)"}, {"none", "None: cache inside Pursae only"}} {
		d.ServerOpts = append(d.ServerOpts, choice{Value: o[0], Label: o[1], Checked: o[0] == d.Server})
	}
	s.loadWarmResult()
	s.warm.mu.Lock()
	d.Warm, d.Warming = s.warm.last, s.warm.running
	if !s.warm.at.IsZero() {
		d.WarmAt = s.warm.at.In(s.loc()).Format("Jan 2, 15:04")
	}
	s.warm.mu.Unlock()
	s.adminPage(c, "admin?page="+page, cacheScreenTitles[page], "wpgo-cache-page", "admin/wpgo-cache", d)
}

// cacheScreenTitles are Pursae Cache's admin?page= screens (the Pursae Cache menu).
var cacheScreenTitles = map[string]string{"wpgo-cache": "Pursae Cache", "wpgo-cache-settings": "Cache Settings",
	"wpgo-cache-optimize": "Page Optimization", "wpgo-cache-toolbox": "Toolbox"}

func (s *Server) cacheAction(c *gin.Context, page string) {
	back := "/wp-admin/admin?page=" + page
	if keys, ok := cacheOptionKeys[page]; ok && c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, page+"-options") {
			s.denyNonce(c)
			return
		}
		values := map[string]any{}
		for _, k := range keys {
			raw, present := c.GetPostForm(k)
			switch settings.Defaults[k].(type) {
			case bool:
				values[k] = present && raw != "" && raw != "0"
			case int:
				if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n >= 0 {
					values[k] = n
				}
			default:
				if present {
					values[k] = strings.TrimSpace(raw)
				}
			}
		}
		if v, ok := values["wpgo_cache_purge_url"].(string); ok {
			if err := proxyPurgeURLProblem(v); err != nil {
				c.Redirect(http.StatusFound, back+"&wpgo-cache-err="+url.QueryEscape(err.Error()))
				return
			}
		}
		if err := s.settings.Set(c, values); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		s.configureCache()
		c.Redirect(http.StatusFound, back+"&settings-updated=true")
		return
	}
	if !s.checkNonce(c, "wpgo-cache-purge") {
		s.denyNonce(c)
		return
	}
	cc := s.currentCache()
	msg := ""
	switch c.DefaultQuery("action", c.PostForm("action")) {
	case "purge_all":
		n := 0
		if cc != nil {
			n = cc.PurgeAll()
		}
		msg = fmt.Sprintf("Purged all caches (%d pages).", n)
	case "purge_front":
		if cc != nil {
			cc.PurgeTags("home")
		}
		msg = "Purged the front page."
	case "purge_404":
		if cc != nil {
			cc.PurgeTags("404")
		}
		msg = "Purged all 404 pages."
	case "purge_url":
		u := strings.TrimSpace(c.PostForm("purge_url"))
		if pu, err := url.Parse(u); err == nil && cc != nil && u != "" {
			if pu.Host == "" {
				u = s.links(c).Home + "/" + strings.TrimLeft(pu.EscapedPath(), "/")
				if pu.RawQuery != "" {
					u += "?" + pu.RawQuery
				}
			}
			msg = fmt.Sprintf("Purged %d cached copies of %s.", cc.PurgeURL(u), u)
		}
	case "warm":
		s.startWarmup(c)
		msg = "Crawler started: the cache is being warmed from the sitemap URLs."
	}
	if r := c.Query("_wp_http_referer"); r != "" && strings.HasPrefix(r, "/") && !strings.HasPrefix(r, "//") {
		back = r
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	c.Redirect(http.StatusFound, back+sep+"wpgo-cache-msg="+url.QueryEscape(msg))
}

// warmupURLs lists every public URL of the site from the core sitemap providers: the home page,
// all public post types (products included), taxonomies and authors, in sitemap pages.
func (s *Server) warmupURLs(ctx context.Context) []string {
	const maxURLs = 50000
	seen := map[string]bool{}
	var urls []string
	add := func(u string) {
		if u != "" && !seen[u] && len(urls) < maxURLs {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	add(s.links(ctx).HomeURL("/"))
	for _, name := range s.sitemapIndexNames(ctx) {
		provider, subtype, page, ok := parseSitemapName(name)
		if !ok {
			continue
		}
		for _, e := range s.sitemapURLs(ctx, provider, subtype, page) {
			add(e.Loc)
		}
	}
	return urls
}

// startWarmup crawls every public URL in-process at a gentle rate (the Warm Up Cache button).
func (s *Server) startWarmup(c *gin.Context) {
	s.runWarmup(context.WithoutCancel(c.Request.Context()))
}

// preloadCache warms the cache without a request (after a purge, daily).
func (s *Server) preloadCache() { s.runWarmup(context.Background()) }

func (s *Server) runWarmup(ctx context.Context) {
	cc := s.currentCache()
	s.warm.mu.Lock()
	if s.warm.running || cc == nil {
		s.warm.mu.Unlock()
		return
	}
	s.warm.running = true
	s.warm.mu.Unlock()
	safeGo("wpgocache.runWarmup", func() {
		urls := s.warmupURLs(ctx)
		prog, err := cc.Warm(ctx, s.appHandler(), urls, cache.WarmupOptions{Concurrency: 2, RatePerSecond: 8})
		if err != nil {
			slog.Warn("wpgo-cache warmup", "err", err)
		}
		now := time.Now()
		s.warm.mu.Lock()
		s.warm.running, s.warm.last, s.warm.at = false, prog, now
		s.warm.mu.Unlock()
		if raw, err := json.Marshal(warmResult{At: now, Progress: prog}); err == nil { // survives a restart
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.settings.Set(sctx, map[string]any{"wpgo_cache_warm_result": string(raw)})
		}
	})
}

// warmResult is the stored outcome of the last crawler run.
type warmResult struct {
	At       time.Time            `json:"at"`
	Progress cache.WarmupProgress `json:"p"`
}

// loadWarmResult restores the last crawler result after a restart.
func (s *Server) loadWarmResult() {
	var r warmResult
	if json.Unmarshal([]byte(s.settings.String("wpgo_cache_warm_result")), &r) != nil || r.At.IsZero() {
		return
	}
	s.warm.mu.Lock()
	if s.warm.at.IsZero() {
		s.warm.last, s.warm.at = r.Progress, r.At
	}
	s.warm.mu.Unlock()
}

// schedulePreload re-warms the cache a little after the last purge ("Preload after purge"); purges that
// arrive within the delay share one crawl.
func (s *Server) schedulePreload() {
	s.warm.mu.Lock()
	defer s.warm.mu.Unlock()
	if s.warm.timer != nil {
		s.warm.timer.Stop()
	}
	s.warm.timer = time.AfterFunc(cachePreloadDelay, s.preloadCache)
}

// cachePreloadDelay is how long after a purge the automatic preload starts.
var cachePreloadDelay = 30 * time.Second

// onCachePurge runs after every purge: announce it to the other instances, forward it to a front
// proxy or CDN, and schedule the preload.
func (s *Server) onCachePurge(p cache.Purge) {
	s.announcePurge(p)
	s.forwardProxyPurge(p)
	if s.settings.Bool("wpgo_cache_preload_purge") && (p.All || len(p.Tags) > 0) {
		s.schedulePreload()
	}
}

// cacheAdminBar is the toolbar node with LiteSpeed-style purge shortcuts.
func (s *Server) cacheAdminBar(c *gin.Context, u *store.User) template.HTML {
	if !u.Can("manage_options", nil) || !s.settings.Bool("wpgo_cache_enabled") {
		return ""
	}
	n := s.nonce(c, "wpgo-cache-purge")
	ref := url.QueryEscape(c.Request.URL.RequestURI())
	item := func(id, action, label string) string {
		return `<li role="group" id="wp-admin-bar-wpgo-cache-` + id + `"><a class="ab-item" role="menuitem" href="/wp-admin/admin?page=wpgo-cache&amp;action=` + action + `&amp;_wpnonce=` + n + `&amp;_wp_http_referer=` + ref + `">` + label + `</a></li>`
	}
	return template.HTML(`<li role="group" id="wp-admin-bar-wpgo-cache" class="menupop"><a class="ab-item" role="menuitem" aria-expanded="false" href="/wp-admin/admin?page=wpgo-cache"><span class="ab-icon dashicons dashicons-performance" aria-hidden="true" style="top:2px"></span><span class="ab-label">Cache</span></a>
<div class="ab-sub-wrapper"><ul role="menu" id="wp-admin-bar-wpgo-cache-default" class="ab-submenu">` +
		item("purge-all", "purge_all", "Purge All") + item("purge-front", "purge_front", "Purge Front Page") + item("purge-404", "purge_404", "Purge All 404 Pages") +
		`<li role="group" id="wp-admin-bar-wpgo-cache-settings"><a class="ab-item" role="menuitem" href="/wp-admin/admin?page=wpgo-cache-settings">Cache Settings</a></li></ul></div></li>`)
}
