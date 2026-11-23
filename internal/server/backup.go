package server

import (
	"context"
	"html/template"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/config"
	"wpgo/internal/store"
)

// Pursae Backup Manager is a native plugin that backs a whole site up into one .zip file and puts it
// back on another (or the same) site: the database, the media library, themes, plugin packages,
// page-builder data, users and every other built-in plugin's tables, each one selectable. It needs
// no SQL dump and no command line: rows are exported as JSON lines straight from PostgreSQL and
// restored through jsonb_populate_recordset, so the archive does not depend on pg_dump or psql.
//
// The archive (format 1) holds
//
//	manifest.json              what is inside, where it came from (site URL, schema version)
//	database/<table>.jsonl     one JSON object per row (to_jsonb of the row)
//	files/uploads/…            the media library
//	files/themes/…             edited and installed theme files (the overlay directory)
//	files/plugins/…            installed plugin packages
//
// Restoring replaces the chosen tables inside one transaction (nothing changes when it fails),
// rewrites the old site address to the new one, keeps this site's own secret and address, and
// resets the id sequences. A safety snapshot of the current site is taken first unless switched off.
// Backups are stored outside the web root (PURSAE_BACKUPS_DIR, default <data>/backups) and are only
// ever served to administrators.

const (
	bmgrSlug    = "backup-manager"
	bmgrSetting = "native_backup_manager" // the plugin is active
	bmgrVersion = "1.0.0"
	bmgrNonce   = "backup-manager"
	bmgrFormat  = 1 // archive format version
)

// bmgrCategory is one selectable part of a site: a set of tables (by name or prefix) and, optionally,
// a directory of files.
type bmgrCategory struct {
	Key, Label, Desc string
	Tables           []string // exact table names
	Prefixes         []string // table name prefixes
	Files            string   // files group the category carries: uploads, themes or plugins
}

// bmgrCategories are the selectable parts, in screen order. Tables no category names fall into "other".
var bmgrCategories = []bmgrCategory{
	{Key: "content", Label: "Posts, pages & taxonomy", Desc: "Posts, pages, custom content, revisions, categories, tags, comments, menus and widgets.",
		Tables: []string{"posts", "post_revisions", "post_terms", "terms", "taxonomies", "content_types", "comments", "menus", "menu_items", "menu_locations", "widget_areas", "widgets"}},
	{Key: "media", Label: "Media library", Desc: "The media records and every uploaded file (images, videos, documents).",
		Tables: []string{"media", "media_variants"}, Files: "uploads"},
	{Key: "users", Label: "Users & roles", Desc: "Accounts, roles, capabilities and application passwords. You can pick which users go.",
		Tables: []string{"users", "roles", "user_roles", "user_capabilities", "app_passwords"}},
	{Key: "settings", Label: "Site settings", Desc: "Every option: site title, permalinks, reading and writing settings, and which built-in plugins are on.",
		Tables: []string{"settings"}},
	{Key: "themes", Label: "Themes", Desc: "Installed themes, their customizer settings and edited theme files.",
		Tables: []string{"themes"}, Files: "themes"},
	{Key: "plugins", Label: "Plugins", Desc: "Installed plugin packages (signed Wasm), their state and data.",
		Tables: []string{"plugins", "plugin_data"}, Files: "plugins"},
	{Key: "builder", Label: "Page builder", Desc: "Pursae Web Builder data: documents, drafts, revisions, library items and form submissions.",
		Prefixes: []string{"web_builder_"}},
	{Key: "commerce", Label: "Pursae Commerce", Desc: "Products, orders, customers' wishlists, coupons, shipping, tax, subscriptions and bookings.",
		Prefixes: []string{"goo_"}},
	{Key: "headless", Label: "Pursae Headless", Desc: "Content types, entries, tokens and webhooks of the headless API.",
		Prefixes: []string{"hg_"}},
	{Key: "multilang", Label: "Pursae Multi Language", Desc: "Languages, translations and string translations.",
		Prefixes: []string{"pll_"}},
	{Key: "seo", Label: "SEO & redirects", Desc: "Pursae SEO data and the redirect table.",
		Tables: []string{"redirects"}, Prefixes: []string{"seogo_"}},
	{Key: "forms", Label: "Contact forms", Desc: "Forms and the CAPTCHA data of Pursae Contact Form.",
		Prefixes: []string{"gontact_"}},
	{Key: "cdn", Label: "Pursae CDN", Desc: "The CDN offload index.", Prefixes: []string{"gocdn_"}},
	{Key: "consent", Label: "Pursae Cookie", Desc: "The consent log.", Prefixes: []string{"gooike_"}},
	{Key: "mail", Label: "Pursae SMTP", Desc: "Email log, debug events and rate counters.", Prefixes: []string{"wpmailsmtp_"}},
	{Key: "security", Label: "Pursae Security", Desc: "Two-factor devices, lockouts, known IPs, the audit and login logs.",
		Tables: []string{"login_attempts", "audit_log"}, Prefixes: []string{"adminsec_"}},
	{Key: "other", Label: "Other plugin data", Desc: "Tables of any other plugin, for example Maintenance subscribers."},
}

// bmgrNever are tables that are never exported: they only make sense on the site that owns them.
var bmgrNever = map[string]bool{"schema_migrations": true, "schema_meta": true, "sessions": true, "transients": true, "jobs": true}

// bmgrKeepSettings are the options this site keeps when a backup is restored over it: its own secret
// (so cookies and nonces stay valid), its address (rewritten from the archive's) and this plugin's state.
var bmgrKeepSettings = []string{"auth_secret", "siteurl", "home", bmgrSetting, bmgrOptSchedule, bmgrOptKeep, bmgrOptCats, bmgrOptLast,
	bmgrOptRemote}

// bmgrNoExportSettings are options never written into an archive: the remote storage settings hold
// the bucket credentials and the encryption passphrase (sealed with the site secret, which the
// archive also carries), and a restore keeps this site's own values anyway (bmgrKeepSettings).
var bmgrNoExportSettings = []string{bmgrOptRemote}

// bmgrCategoryOf returns the category key a table belongs to.
func bmgrCategoryOf(table string) string {
	for _, c := range bmgrCategories {
		for _, t := range c.Tables {
			if t == table {
				return c.Key
			}
		}
		for _, p := range c.Prefixes {
			if strings.HasPrefix(table, p) {
				return c.Key
			}
		}
	}
	return "other"
}

func bmgrCategoryByKey(key string) *bmgrCategory {
	for i := range bmgrCategories {
		if bmgrCategories[i].Key == key {
			return &bmgrCategories[i]
		}
	}
	return nil
}

// bmgrJob is one backup or restore run, polled by the screen.
type bmgrJob struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`  // backup | restore | upload | fetch
	State    string    `json:"state"` // running | done | error
	Phase    string    `json:"phase"`
	Percent  int       `json:"percent"`
	Error    string    `json:"error,omitempty"`
	File     string    `json:"file,omitempty"` // the archive a backup produced
	Log      []string  `json:"log"`
	Warnings int       `json:"warnings,omitempty"` // files skipped etc.: the job is done, but not perfectly
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	// Remote is the bucket key a backup was uploaded to; RemoteError why the upload failed (the
	// local archive is kept either way).
	Remote      string `json:"remote,omitempty"`
	RemoteError string `json:"remote_error,omitempty"`
}

// bmgrRuntime is the plugin's in-memory state: one job runs at a time.
type bmgrRuntime struct {
	mu   sync.Mutex
	jobs map[string]*bmgrJob
	busy bool
	// cancels stops a running backup (restores are not cancelable once they touch the database).
	cancels map[string]context.CancelFunc
}

// bmgrOn reports whether the plugin is active.
func (s *Server) bmgrOn() bool { return s.settings.Bool(bmgrSetting) }

// bmgrDir is where archives live: PURSAE_BACKUPS_DIR, default <data>/backups next to the uploads. It is
// never inside the uploads directory, which is served publicly.
func (s *Server) bmgrDir() string {
	if d := config.Env("BACKUPS_DIR"); d != "" {
		return d
	}
	return filepath.Join(filepath.Dir(filepath.Clean(s.cfg.UploadsDir)), "backups")
}

// bmgrFileRoots maps a files group to its directory on disk.
func (s *Server) bmgrFileRoots() map[string]string {
	pkg, _ := s.pluginDirs()
	return map[string]string{
		"uploads": filepath.Clean(s.cfg.UploadsDir),
		"themes":  themeOverrideDir(s.cfg.UploadsDir),
		"plugins": pkg,
	}
}

// bmgrPluginRow is the plugin's row on the Plugins screen.
func (s *Server) bmgrPluginRow() pluginRow {
	r := pluginRow{Slug: bmgrSlug, Name: "Pursae Backup Manager", Version: builtinPluginVersion, Native: true, Active: s.bmgrOn(),
		Description: "Back up and move a whole site in one click: database, media, themes, plugins, page builder data and users, each one selectable. No SQL dump needed: restore the .zip on another site and the address is rewritten for you.",
		Author:      "Pursae"}
	if r.Active {
		r.SettingsURL = "/wp-admin/admin?page=" + bmgrSlug
	}
	return r
}

// bmgrPluginAction activates or deactivates the plugin; the scheduled backup follows it.
func (s *Server) bmgrPluginAction(ctx context.Context, action string) error {
	if err := s.settings.Set(ctx, map[string]any{bmgrSetting: action == "activate"}); err != nil {
		return err
	}
	return s.bmgrSyncSchedule(ctx, false)
}

// bmgrMenu adds the top-level Backup Manager menu above Settings while the plugin is active.
func (s *Server) bmgrMenu(menu []*menuTop, u *store.User, screen string) []*menuTop {
	if !s.bmgrOn() || !u.Can("manage_options", nil) {
		return menu
	}
	slug := "admin?page=" + bmgrSlug
	top := &menuTop{Slug: slug, Title: template.HTML("Backup Manager"), ID: "toplevel_page_backup-manager", Icon: "dashicons-database-export", Current: screen == slug}
	for i, m := range menu {
		if m.ID == "menu-settings" {
			top.First, m.First = m.First, false
			return append(menu[:i], append([]*menuTop{top}, menu[i:]...)...)
		}
	}
	return append(menu, top)
}

// bmgrAllowed reports whether the request may use the plugin: an administrator, plugin active.
func (s *Server) bmgrAllowed(c *gin.Context) bool {
	u := currentUser(c)
	return u != nil && s.bmgrOn() && u.Can("manage_options", nil)
}
