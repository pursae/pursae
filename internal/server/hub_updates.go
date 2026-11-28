package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/i18n"
	"wpgo/internal/plugins"
	"wpgo/internal/store"
)

// Updates of plugins installed from Pursae Manager (plugins.source = 'hub'). The catalog
// (GET /api/v1/plugins, cached 12 hours, per license) gives each listing's newest version the site may
// see; a higher version than the installed one is an update. The Plugins screen shows WordPress' "There
// is a new version of X available. Update now" row under the plugin, Dashboard › Updates lists them
// with "Update Plugins". Updating downloads the package and verifies its SHA-256 and signature exactly
// like an install (hubInstallPlugin), replaces the package and, if the plugin was active, loads the new
// version with the permissions granted before (a new version that asks for more gets none of the new
// ones until the admin re-activates it from the consent screen).
//
// Themes: the theme library comes from the Manager (theme_library.go). An installed package theme
// whose slug the theme catalog (GET /api/v1/themes, cached like the plugins) lists with a higher
// version has an update, unless it was uploaded (themePackageSource). Appearance › Themes shows the
// "New version available. Update now" notice on its card and details, Dashboard › Updates lists it
// under Themes with "Update Themes". Updating downloads the package, checks its SHA-256 and size and
// installs it through installThemePackage, which replaces the files in place: theme mods, widgets and
// the active theme stay as they are. Nothing is updated automatically.

// pluginUpdate is a newer version of an installed plugin on the Manager.
type pluginUpdate struct {
	Version, Channel, Homepage string
	Changelog                  string // what changed, from the listing (shown under the row)
	URL                        string // Update now (Plugins screen), with its nonce
}

// Automatic plugin updates: the slugs of Manager-installed plugins that update themselves in the
// daily wpgo_hub_check (option pursae_plugin_auto_updates, comma separated). An active plugin only
// updates by itself when the new version asks for no permission it was not granted; otherwise the
// administrator reviews it from the Plugins screen.
const pluginAutoUpdatesOption = "pursae_plugin_auto_updates"

func (s *Server) pluginAutoUpdates() map[string]bool {
	out := map[string]bool{}
	for _, v := range strings.Split(s.settings.String(pluginAutoUpdatesOption), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = true
		}
	}
	return out
}

func (s *Server) setPluginAutoUpdate(ctx context.Context, slug string, on bool) error {
	m := s.pluginAutoUpdates()
	if on {
		m[slug] = true
	} else {
		delete(m, slug)
	}
	list := make([]string, 0, len(m))
	for k := range m {
		list = append(list, k)
	}
	sort.Strings(list)
	return s.settings.Set(ctx, map[string]any{pluginAutoUpdatesOption: strings.Join(list, ",")})
}

// hubAutoUpdatePlugins applies the updates of plugins with auto-updates on; it returns the slugs it
// updated. Failures are logged and leave the plugin as it was.
func (s *Server) hubAutoUpdatePlugins(ctx context.Context) []string {
	auto := s.pluginAutoUpdates()
	if len(auto) == 0 || s.sandbox != nil {
		return nil
	}
	ups := s.hubPluginUpdates(ctx)
	var done []string
	for slug, it := range ups {
		if !auto[slug] {
			continue
		}
		var status string
		var perms []byte
		if s.store.Pool.QueryRow(ctx, `SELECT status, permissions FROM plugins WHERE slug = $1`, slug).Scan(&status, &perms) != nil {
			continue
		}
		if status == "active" {
			var granted []plugins.Permission
			_ = json.Unmarshal(perms, &granted)
			declared := make([]plugins.Permission, 0, len(it.Permissions))
			for _, p := range it.Permissions {
				declared = append(declared, plugins.Permission(p))
			}
			if _, missing := splitPermissions(granted, declared); len(missing) > 0 {
				continue // asks for something new: needs the administrator
			}
		}
		if _, err := s.hubUpdatePlugin(ctx, slug); err != nil {
			slog.Warn("plugin auto-update failed", "plugin", slug, "err", err)
			continue
		}
		done = append(done, slug)
	}
	sort.Strings(done)
	return done
}

// pluginAutoUpdateToggle is Plugins › "Enable/Disable auto-updates" for one plugin.
func (s *Server) pluginAutoUpdateToggle(c *gin.Context, on bool) {
	slug := c.Query("plugin")
	if !s.checkNonce(c, "auto-update-plugin_"+slug) {
		s.denyNonce(c)
		return
	}
	if !currentUser(c).Can("update_plugins", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to update plugins for this site.")
		return
	}
	if !plugins.ValidSlug(slug) {
		c.Redirect(http.StatusFound, "/wp-admin/plugins")
		return
	}
	if err := s.setPluginAutoUpdate(c.Request.Context(), slug, on); err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape("The setting could not be saved."))
		return
	}
	c.Redirect(http.StatusFound, "/wp-admin/plugins")
}

// hubPluginUpdates maps the slug of every Manager-installed plugin with a newer published version to
// its listing. Without Manager plugins it never touches the network.
func (s *Server) hubPluginUpdates(ctx context.Context) map[string]hubItem {
	if s.hubBase() == "" {
		return nil
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT slug, version FROM plugins WHERE source = 'hub'`)
	if err != nil {
		return nil
	}
	installed := map[string]string{}
	for rows.Next() {
		var slug, v string
		if rows.Scan(&slug, &v) == nil {
			installed[slug] = v
		}
	}
	rows.Close()
	if len(installed) == 0 {
		return nil
	}
	items, err := s.hubCatalog(ctx, "plugins")
	if err != nil {
		return nil
	}
	out := map[string]hubItem{}
	for _, it := range items {
		if v, ok := installed[it.Slug]; ok && versionLess(v, it.Version) {
			out[it.Slug] = it
		}
	}
	return out
}

// pluginsWithUpdates marks the rows that have a Manager update (Plugins screen, Dashboard › Updates).
func (s *Server) pluginsWithUpdates(c *gin.Context, rows []pluginRow) []pluginRow {
	ups := s.hubPluginUpdates(c.Request.Context())
	auto := s.pluginAutoUpdates()
	for i := range rows {
		if rows[i].Source == pluginSourceHub && !rows[i].Native {
			rows[i].AutoUpdate = auto[rows[i].Slug]
			act := "enable-auto-update"
			if rows[i].AutoUpdate {
				act = "disable-auto-update"
			}
			rows[i].AutoUpdateURL = "/wp-admin/plugins?action=" + act + "&plugin=" + url.QueryEscape(rows[i].Slug) + "&_wpnonce=" + s.nonce(c, "auto-update-plugin_"+rows[i].Slug)
		}
		if it, ok := ups[rows[i].Slug]; ok && !rows[i].Native {
			rows[i].Update = &pluginUpdate{Version: it.Version, Channel: it.Channel, Homepage: it.Homepage, Changelog: it.Changelog,
				URL: "/wp-admin/plugins?action=upgrade-plugin&plugin=" + url.QueryEscape(rows[i].Slug) + "&_wpnonce=" + s.nonce(c, "upgrade-plugin_"+rows[i].Slug)}
		}
	}
	return rows
}

// hubUpdatePlugin updates one Manager-installed plugin to the newest version the Manager offers.
// The package must come from the key that signed the installed version (or another Manager key) and
// must not be older; the previous package stays as <slug>.zip.prev, and if the new version cannot be
// loaded the previous one is put back.
func (s *Server) hubUpdatePlugin(ctx context.Context, slug string) (*plugins.Manifest, error) {
	if !plugins.ValidSlug(slug) {
		return nil, errors.New("Invalid plugin.")
	}
	var installed, status, source, digest, signer string
	var perms []byte
	if err := s.store.Pool.QueryRow(ctx, `SELECT version, status, source, permissions, signature, signer FROM plugins WHERE slug = $1`, slug).Scan(&installed, &status, &source, &perms, &digest, &signer); err != nil {
		return nil, errors.New("That plugin is not installed.")
	}
	if status == "active" {
		if u, ok := ctx.Value(extActorCtx{}).(*store.User); ok && !u.Can("activate_plugins", nil) {
			// Same rule as installAndReload: a new package for an active plugin runs at once. Callers
			// with no acting user (CLI, background updates) carry no actor and stay allowed.
			return nil, errPluginReloadDenied
		}
	}
	if source != pluginSourceHub {
		return nil, errors.New("Only plugins installed from Pursae Manager can be updated here; upload the new version instead.")
	}
	if _, err := s.hubIndexCached(ctx); err != nil { // the Manager's signing keys
		return nil, err
	}
	it, err := s.hubItemFor(ctx, "plugins", slug)
	if err != nil {
		return nil, err
	}
	if !versionLess(installed, it.Version) {
		return nil, fmt.Errorf("%s %s is already the newest version.", slug, installed)
	}
	data, err := s.hubDownload(ctx, it)
	if err != nil {
		return nil, err
	}
	zipPath := filepath.Join(s.plugins.dir, slug+".zip")
	prev, prevErr := os.ReadFile(zipPath)
	if prevErr == nil {
		if err := writeFileAtomic(zipPath+".prev", prev, 0o644); err != nil {
			return nil, err
		}
	}
	active := status == "active"
	if active && s.plugins.rt != nil {
		_ = s.plugins.rt.Unload(ctx, slug)
	}
	m, err := s.installPackageFrom(ctx, data, pluginSourceHub, slug)
	if err != nil {
		if active { // the old package is still in place: bring it back
			_ = s.loadPlugin(ctx, slug, perms)
		}
		return nil, err
	}
	if active {
		if err := s.reloadPlugin(ctx, slug, m, perms); err != nil {
			if prevErr == nil && writeFileAtomic(zipPath, prev, 0o644) == nil { // roll back to the previous package
				_, _ = s.store.Pool.Exec(ctx, `UPDATE plugins SET version = $2, signature = $3, signer = $4, permissions = $5 WHERE slug = $1`, slug, installed, digest, signer, perms)
				if lerr := s.loadPlugin(ctx, slug, perms); lerr == nil {
					s.plugins.hooks.DoActionOwner(ctx, slug, "init")
					return nil, fmt.Errorf("%s %s could not be activated (%v); version %s was restored", m.Name, m.Version, err, installed)
				}
			}
			_, _ = s.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'inactive' WHERE slug = $1`, slug)
			return m, fmt.Errorf("%s was updated to %s but could not be activated again: %v", m.Name, m.Version, err)
		}
	}
	s.purgeAll(ctx)
	return m, nil
}

// pluginUpgrade is the Plugins screen's "Update now" (update.php?action=upgrade-plugin).
func (s *Server) pluginUpgrade(c *gin.Context) {
	slug := c.Query("plugin")
	if !s.checkNonce(c, "upgrade-plugin_"+slug) {
		s.denyNonce(c)
		return
	}
	if !currentUser(c).Can("update_plugins", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to update plugins for this site.")
		return
	}
	m, err := s.hubUpdatePlugin(withExtActor(c.Request.Context(), currentUser(c)), slug)
	if err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape(s.updateFailed(c, err.Error())))
		return
	}
	c.Redirect(http.StatusFound, "/wp-admin/plugins?updated="+url.QueryEscape(m.Name+" "+m.Version))
}

// updatePlugins is Dashboard › Updates › Update Plugins (update-core.php?action=do-plugin-upgrade).
func (s *Server) updatePlugins(c *gin.Context) {
	if !s.checkNonce(c, "upgrade-core") {
		s.denyNonce(c)
		return
	}
	if !currentUser(c).Can("update_plugins", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to update plugins for this site.")
		return
	}
	slugs := c.PostFormArray("checked[]")
	sort.Strings(slugs)
	var done, errs []string
	for _, slug := range slugs {
		m, err := s.hubUpdatePlugin(withExtActor(c.Request.Context(), currentUser(c)), slug)
		if err != nil {
			errs = append(errs, slug+": "+err.Error())
			continue
		}
		done = append(done, m.Name+" "+m.Version)
	}
	q := url.Values{}
	if len(done) > 0 {
		q.Set("plugins-updated", strings.Join(done, ", "))
	}
	if len(errs) > 0 {
		q.Set("error", strings.Join(errs, "; "))
	}
	c.Redirect(http.StatusFound, "/wp-admin/update-core?"+q.Encode())
}

// themeUpdate is a newer version of an installed theme on the Manager.
type themeUpdate struct {
	Version, Channel string
	URL              string // Update now (Appearance › Themes), with its nonce
}

// hubThemeUpdates maps the slug of every installed package theme with a newer version on the Manager
// to its listing. Without package themes from the Manager it never touches the network.
func (s *Server) hubThemeUpdates(ctx context.Context) map[string]hubItem {
	if s.hubBase() == "" || s.sandbox != nil {
		return nil
	}
	installed := map[string]string{}
	for _, t := range s.installedThemes() {
		if t.Package != nil && !t.Custom && themePackageSource(t) != themeSourceUpload {
			installed[t.Slug] = t.Version
		}
	}
	if len(installed) == 0 {
		return nil
	}
	lib, err := s.managerLibraryThemes(ctx) // the catalog, valid listings only; a Manager that failed is not asked again at once
	if err != nil {
		return nil
	}
	out := map[string]hubItem{}
	for _, lt := range lib {
		if v, ok := installed[lt.Manifest.Slug]; ok && semverLess(v, lt.Manifest.Version) {
			out[lt.Manifest.Slug] = *lt.Item
		}
	}
	return out
}

// themeUpdateURL is the Update now link of a theme (Appearance › Themes).
func (s *Server) themeUpdateURL(c *gin.Context, slug string) string {
	return "/wp-admin/themes?action=upgrade-theme&stylesheet=" + url.QueryEscape(slug) + "&_wpnonce=" + s.nonce(c, "upgrade-theme_"+slug)
}

// hubUpdateTheme updates one installed package theme to the newest version the Manager offers.
func (s *Server) hubUpdateTheme(ctx context.Context, slug string) (*Theme, error) {
	t := s.installedTheme(slug)
	if t == nil || t.Package == nil || t.Custom {
		return nil, errors.New("That theme is not installed.")
	}
	if themePackageSource(t) == themeSourceUpload {
		return nil, errors.New("Only themes installed from Pursae Manager can be updated here; upload the new version instead.")
	}
	lt, err := s.hubThemeListing(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !semverLess(t.Version, lt.Manifest.Version) {
		return nil, fmt.Errorf("%s %s is already the newest version.", t.Name, t.Version)
	}
	return s.installLibraryTheme(ctx, lt)
}

// themeUpgrade is Appearance › Themes › Update now (update.php?action=upgrade-theme).
func (s *Server) themeUpgrade(c *gin.Context) {
	slug := c.Query("stylesheet")
	if !s.checkNonce(c, "upgrade-theme_"+slug) {
		s.denyNonce(c)
		return
	}
	if !currentUser(c).Can("update_themes", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to update themes for this site.")
		return
	}
	t, err := s.hubUpdateTheme(c, slug)
	if err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/themes?error="+url.QueryEscape(s.updateFailed(c, err.Error())))
		return
	}
	c.Redirect(http.StatusFound, "/wp-admin/themes?updated="+url.QueryEscape(t.Slug))
}

// updateThemes is Dashboard › Updates › Update Themes (update-core.php?action=do-theme-upgrade).
func (s *Server) updateThemes(c *gin.Context) {
	if !s.checkNonce(c, "upgrade-core") {
		s.denyNonce(c)
		return
	}
	if !currentUser(c).Can("update_themes", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to update themes for this site.")
		return
	}
	slugs := c.PostFormArray("checked[]")
	sort.Strings(slugs)
	var done, errs []string
	for _, slug := range slugs {
		t, err := s.hubUpdateTheme(c, slug)
		if err != nil {
			errs = append(errs, slug+": "+err.Error())
			continue
		}
		done = append(done, t.Name+" "+t.Version)
	}
	q := url.Values{}
	if len(done) > 0 {
		q.Set("themes-updated", strings.Join(done, ", "))
	}
	if len(errs) > 0 {
		q.Set("error", strings.Join(errs, "; "))
	}
	c.Redirect(http.StatusFound, "/wp-admin/update-core?"+q.Encode())
}

// updateFailed is the "Update failed: %s" notice in the user's language, the reason included: the
// redirect carries it to the screen as text, so it is translated here as a whole.
func (s *Server) updateFailed(c *gin.Context, reason string) string {
	return i18n.Sprintf(s.__(c, "Update failed: %s"), s.catalog(c).Text(reason))
}
