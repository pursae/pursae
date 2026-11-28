// Package settings is the typed counterpart of WordPress options: every value is
// stored as JSONB, autoloaded rows are cached in memory and the cache is
// refreshed on write.
//
// Several processes may share one database. Reads never leave memory; to keep
// the caches in step, a statement trigger on the settings table (migration
// 0047) sends NOTIFY wpgo_settings when its transaction commits, and Watch
// (started by `wpgo serve`) listens on a dedicated connection and reloads the
// cache on each notification, after every (re)connect and every
// WatchFullReload as a safety net. A change made by another instance therefore
// shows up here within moments, at worst after WatchFullReload. Watch only
// refreshes values: side effects a process runs when it saves a setting itself
// (starting a plugin, reconfiguring the page cache) still happen only there.
package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NotifyChannel is the channel the settings trigger notifies (migration 0047).
const NotifyChannel = "wpgo_settings"

// WatchFullReload is how often Watch reloads the cache even without a
// notification, in case one was lost.
var WatchFullReload = time.Minute

// Defaults mirrors populate_options() of a fresh WordPress install. Keys keep
// the WordPress option names so REST and import code can map 1:1.
var Defaults = map[string]any{
	"blogname":                   "My Pursae Site",
	"blogdescription":            "",
	"siteurl":                    "",
	"home":                       "",
	"admin_email":                "",
	"users_can_register":         false,
	"default_role":               "subscriber",
	"WPLANG":                     "",
	"timezone_string":            "",
	"gmt_offset":                 0.0,
	"date_format":                "F j, Y",
	"time_format":                "g:i a",
	"start_of_week":              1,
	"use_smilies":                true,
	"default_category":           1,
	"default_post_format":        "0",
	"classic-editor-replace":     "block",
	"classic-editor-allow-users": "disallow",
	"native_classic_editor":      true,
	"native_golementor":          false,       // Pursae Web Builder is a plugin that starts switched off
	"web_builder_post_types":     "page,post", // Settings › Web Builder: the post types it edits
	"native_golylang":            false,       // the multilingual plugin (Polylang Pro clone) starts switched off
	"native_gooike":              false,       // Gooike, the cookie consent plugin, starts switched off
	"native_goocommerce":         false,       // Goocommerce, the e-commerce plugin, starts switched off
	"native_headlessgo":          false,       // HeadlessGo, the headless CMS plugin, starts switched off
	"plugin_trusted_keys":        "",
	"plugin_allow_unsigned":      false,
	"csp_mode":                   "report",         // script CSP: "report" (nonce policy report-only), "enforce" or "off" (csp_nonce.go)
	"csp_admin_scripts":          true,             // scripts in stored unfiltered_html markup get the nonce (csp_trusted.go)
	"csp_extra_sources":          map[string]any{}, // directive → external origins added to the front-end policy (csp_extra.go)
	"pursae_license_key":         "",               // Settings › License: the key sent to Pursae Manager
	"wpgo_hub_trust_keys":        true,             // packages signed by the Hub install without extra setup
	"wpgo_cache_enabled":         true,
	"wpgo_cache_ttl_public":      604800,
	"wpgo_cache_ttl_front":       604800,
	"wpgo_cache_ttl_feed":        604800,
	"wpgo_cache_ttl_404":         3600,
	"wpgo_cache_mobile":          false,
	"wpgo_cache_logged_in":       false,
	"wpgo_cache_exclude_uri":     "",
	"wpgo_cache_drop_qs":         "",
	"wpgo_cache_server":          "auto",
	"wpgo_cache_disk":            true, // warm pages survive restarts and updates
	// "wpgo_cache_enabled" is the plugin switch (Plugins screen); "wpgo_cache_serve" turns page caching
	// on or off while the plugin stays active, so its screens remain reachable.
	"wpgo_cache_serve":              true,
	"wpgo_cache_query_strings":      "", // extra query parameters that are part of the cache key
	"wpgo_cache_exclude_cookies":    "",
	"wpgo_cache_exclude_ua":         "",
	"wpgo_cache_browser_ttl":        0,   // seconds, Cache-Control for static assets; 0 leaves them alone
	"wpgo_cache_redirect_ttl":       0,   // seconds, 301 responses; 0 does not cache them
	"wpgo_cache_max_mb":             256, // memory limit of the in-process cache
	"wpgo_cache_purge_url":          "",  // front proxy/CDN endpoint that receives purge requests
	"wpgo_cache_proxy_ttl":          300, // seconds a front proxy may keep a page while no purge URL is set
	"wpgo_cache_preload_purge":      false,
	"wpgo_cache_preload_daily":      false,
	"wpgo_cache_warm_result":        "",
	"wpgo_opt_lazy":                 true,
	"wpgo_opt_lazy_skip":            1,
	"wpgo_opt_defer":                true, // an empty list defers only scripts nothing depends on
	"wpgo_opt_defer_list":           "",
	"wpgo_opt_minify_html":          true,
	"wpgo_opt_defer_all":            false, // defer every body script except the exclude list
	"wpgo_opt_defer_exclude":        "",
	"wpgo_opt_delay_list":           "", // scripts held back until the first interaction
	"wpgo_opt_delay_timeout":        0,
	"wpgo_opt_minify_css":           false,
	"wpgo_opt_preconnect":           "",
	"wpgo_opt_dns_prefetch":         "",
	"wpgo_opt_preload_fonts":        true,
	"posts_per_page":                10,
	"posts_per_rss":                 10,
	"rss_use_excerpt":               false,
	"show_on_front":                 "posts",
	"page_on_front":                 0,
	"page_for_posts":                0,
	"blog_public":                   true,
	"default_pingback_flag":         true,
	"default_ping_status":           "open",
	"default_comment_status":        "open",
	"require_name_email":            true,
	"comment_registration":          false,
	"close_comments_for_old_posts":  false,
	"close_comments_days_old":       14,
	"show_comments_cookies_opt_in":  true,
	"thread_comments":               true,
	"thread_comments_depth":         5,
	"page_comments":                 false,
	"comments_per_page":             50,
	"default_comments_page":         "newest",
	"comment_order":                 "asc",
	"comments_notify":               true,
	"moderation_notify":             true,
	"comment_moderation":            false,
	"comment_previously_approved":   true,
	"comment_max_links":             2,
	"moderation_keys":               "",
	"disallowed_keys":               "",
	"show_avatars":                  true,
	"avatar_rating":                 "G",
	"avatar_default":                "mystery",
	"thumbnail_size_w":              150,
	"thumbnail_size_h":              150,
	"thumbnail_crop":                true,
	"medium_size_w":                 300,
	"medium_size_h":                 300,
	"medium_large_size_w":           768,
	"medium_large_size_h":           0,
	"large_size_w":                  1024,
	"large_size_h":                  1024,
	"uploads_use_yearmonth_folders": true,
	"image_output_format":           "",   // Settings › Media: "", "webp" or "webp-all" (Server.imageEditorOptions)
	"pursae_strip_exif":             true, // Settings › Media: drop EXIF/XMP (GPS, camera) from uploaded JPEGs (media.Options.KeepEXIF)
	"permalink_structure":           "/%year%/%monthnum%/%day%/%postname%/",
	"category_base":                 "",
	"tag_base":                      "",
	"template":                      "twentytwentyfive",
	"stylesheet":                    "twentytwentyfive",
	"sticky_posts":                  []any{},
	"wp_page_for_privacy_policy":    0,
	"site_icon":                     0,
}

type Store struct {
	pool          *pgxpool.Pool
	mu            sync.RWMutex
	cache         map[string]any
	cacheRevision uint64
	// writeMu orders Set's commit and cache update, so the cache ends with the
	// value committed last when two Sets of one key race.
	writeMu sync.Mutex
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Load fills the cache with every autoloaded row.
func (s *Store) Load(ctx context.Context) error {
	for {
		s.mu.RLock()
		revision := s.cacheRevision
		s.mu.RUnlock()

		cache, err := s.readCache(ctx)
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.cacheRevision == revision {
			s.cache = cache
			s.cacheRevision++
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		// A Set or another Load published during the read. Retry with a fresh snapshot.
	}
}

func (s *Store) readCache(ctx context.Context) (map[string]any, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM settings WHERE autoload`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]any{}
	for rows.Next() {
		var k string
		var raw []byte
		if err := rows.Scan(&k, &raw); err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("setting %s: %w", k, err)
		}
		m[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// Installed reports whether the install wizard has run.
func (s *Store) Installed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.cache["siteurl"]
	return ok
}

func (s *Store) Get(key string) any {
	s.mu.RLock()
	v, ok := s.cache[key]
	if ok {
		v = cloneValue(v)
	}
	s.mu.RUnlock()
	if ok {
		return v
	}
	return cloneValue(Defaults[key])
}

// Revision is a counter that changes whenever the cache does (every Set and
// every Load, including the reloads Watch runs on NOTIFY). Two equal reads
// mean Get returned the same values in between, so a value derived from
// settings and read after a Revision call can be memoized under it (see Memo).
func (s *Store) Revision() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cacheRevision
}

// Lookup returns the member sub of the JSON object stored under key (over
// Defaults, like Get) without cloning the rest of the object: v is what
// Get(key).(map[string]any)[sub] would be (nil when there is no such member).
// isObject is false when the value is not a JSON object (unset, null or
// another type); v is then nil.
func (s *Store) Lookup(key, sub string) (v any, isObject bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, found := s.cache[key]
	if !found {
		raw = Defaults[key]
	}
	m, isObject := raw.(map[string]any)
	if !isObject {
		return nil, false
	}
	return cloneValue(m[sub]), true
}

// Memo caches one value derived from a Store's settings, rebuilt when the
// Store's Revision changes. The zero value is ready; a Memo is safe for
// concurrent use and its hit path takes no lock beyond Revision's RLock.
// The cached value is shared by every caller: build's result must not be
// mutated afterwards, so callers that may modify it must copy it.
type Memo[T any] struct {
	p atomic.Pointer[memoEntry[T]]
}

type memoEntry[T any] struct {
	store *Store // kept: its address cannot be reused while the entry lives
	rev   uint64
	v     T
}

// Get returns the value built for store's current revision, calling build on
// a miss. The revision is read before build runs, so a value built from newer
// settings is at worst rebuilt once more, never served for a later revision.
func (m *Memo[T]) Get(store *Store, build func() T) T {
	rev := store.Revision()
	if e := m.p.Load(); e != nil && e.store == store && e.rev == rev {
		return e.v
	}
	v := build()
	m.p.Store(&memoEntry[T]{store: store, rev: rev, v: v})
	return v
}

// Peek returns the cached value whatever its revision, for builders that can
// reuse part of the previous result (ok is false when there is none or it
// belongs to another Store).
func (m *Memo[T]) Peek(store *Store) (v T, ok bool) {
	if e := m.p.Load(); e != nil && e.store == store {
		return e.v, true
	}
	return v, false
}

// Cached JSON objects and arrays must not escape: callers may modify values while
// preparing a subsequent Set, and the cache must change only after a commit.
func cloneValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		if v == nil {
			return map[string]any(nil)
		}
		out := make(map[string]any, len(v))
		for k, value := range v {
			out[k] = cloneValue(value)
		}
		return out
	case []any:
		if v == nil {
			return []any(nil)
		}
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = cloneValue(value)
		}
		return out
	default:
		return v
	}
}

func (s *Store) String(key string) string {
	switch v := s.Get(key).(type) {
	case string:
		return v
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func (s *Store) Int(key string) int {
	switch v := s.Get(key).(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	case bool:
		if v {
			return 1
		}
	}
	return 0
}

func (s *Store) Float(key string) float64 {
	switch v := s.Get(key).(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}

func (s *Store) Bool(key string) bool {
	switch v := s.Get(key).(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case int:
		return v != 0
	case string:
		return v != "" && v != "0"
	}
	return false
}

// Set writes the given values in one transaction and refreshes the cache.
func (s *Store) Set(ctx context.Context, values map[string]any) error {
	// Upsert in key order: two concurrent saves of the same screen lock the rows
	// in the same order and cannot deadlock.
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	loaded := make(map[string]any, len(values))
	for _, k := range keys {
		v := values[k]
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		var autoload bool
		if err := tx.QueryRow(ctx, `INSERT INTO settings(key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
			RETURNING autoload`, k, raw).Scan(&autoload); err != nil {
			return err
		}
		if autoload {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("setting %s: %w", k, err)
			}
			loaded[k] = value
		}
	}
	// Commit and refresh the cache under writeMu: a Set blocked on our row locks
	// commits after us and so also updates the cache after us.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]any{}
	}
	for _, k := range keys {
		if value, ok := loaded[k]; ok {
			s.cache[k] = value
		} else {
			delete(s.cache, k)
		}
	}
	s.cacheRevision++
	s.mu.Unlock()
	return nil
}

// Watch keeps the cache in step with changes other processes commit (see the
// package comment). It blocks until ctx ends. The LISTEN connection is opened
// outside the pool, so closing the pool never waits for it.
func (s *Store) Watch(ctx context.Context) {
	stop := make(chan struct{})
	defer close(stop)
	go func() { // safety net for lost notifications and a broken listener
		t := time.NewTicker(WatchFullReload)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.Load(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("settings: periodic reload", "err", err)
				}
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		err := s.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("settings: change listener stopped, reconnecting", "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// listen runs one LISTEN session until it fails or ctx ends.
func (s *Store) listen(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return err
	}
	// Changes made while nobody listened are only visible through a reload.
	if err := s.Load(ctx); err != nil {
		return err
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		if err := s.Load(ctx); err != nil {
			return err
		}
	}
}
