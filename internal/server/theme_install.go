package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// Installing theme packages (docs/theme-package.md, "Packaging"): a zip of theme.json, templates/
// and assets/ is validated, extracted to <data>/themes/<slug>/ and registered; packages found there
// at start-up are registered the same way. Appearance › Themes › Delete removes the directory again.

const (
	themeZipMaxBytes   = 32 << 20 // per package
	themeFileMaxBytes  = 4 << 20  // per file
	themeTotalMaxBytes = 64 << 20 // extracted size of a package
	themeZipMaxFiles   = 2000
)

// themeFileExt lists the file types a package may contain: no server-side code, but .js (and the
// templates' inline scripts) run in the browser on every page, so a package is trusted code.
var themeFileExt = map[string]bool{
	".json": true, ".html": true, ".css": true, ".js": true, ".map": true, ".svg": true, ".png": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".webp": true, ".avif": true, ".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".md": true, ".txt": true,
}

// themeZipFile is one extracted file of a package.
type themeZipFile struct {
	Path string // theme-relative, slash separated
	Data []byte
}

// unpackThemeZip reads a theme package: it checks the limits and the file types, strips a single
// top-level folder and returns the files with the parsed manifest.
func unpackThemeZip(data []byte) ([]themeZipFile, *themeManifest, error) {
	if len(data) > themeZipMaxBytes {
		return nil, nil, fmt.Errorf("the package is larger than %d MiB", themeZipMaxBytes>>20)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, errors.New("the file is not a zip archive")
	}
	type entry struct {
		name string
		f    *zip.File
	}
	var entries []entry
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if strings.HasSuffix(name, "/") || f.FileInfo().IsDir() {
			continue
		}
		if strings.HasPrefix(name, "__MACOSX/") || path.Base(name) == ".DS_Store" || path.Base(name) == "Thumbs.db" {
			continue
		}
		if strings.HasPrefix(name, "/") || !fs.ValidPath(name) {
			return nil, nil, fmt.Errorf("%q is not a valid path inside a package", f.Name)
		}
		entries = append(entries, entry{name, f})
	}
	if len(entries) == 0 {
		return nil, nil, errors.New("the package is empty")
	}
	if len(entries) > themeZipMaxFiles {
		return nil, nil, fmt.Errorf("the package has more than %d files", themeZipMaxFiles)
	}
	// A single top-level folder (starter/theme.json, starter/templates/…) is stripped.
	if hasRootManifest := func() bool {
		for _, e := range entries {
			if e.name == "theme.json" {
				return true
			}
		}
		return false
	}(); !hasRootManifest {
		prefix := ""
		for i, e := range entries {
			top, _, ok := strings.Cut(e.name, "/")
			if !ok {
				prefix = ""
				break
			}
			if i == 0 {
				prefix = top + "/"
			} else if top+"/" != prefix {
				prefix = ""
				break
			}
		}
		if prefix != "" {
			for i := range entries {
				entries[i].name = strings.TrimPrefix(entries[i].name, prefix)
			}
		}
	}
	var files []themeZipFile
	var manifest []byte
	var total uint64
	seen := map[string]bool{}
	for _, e := range entries {
		if !themeFileExt[strings.ToLower(path.Ext(e.name))] {
			return nil, nil, fmt.Errorf("%q: files of this type are not allowed in a theme", e.name)
		}
		if e.f.UncompressedSize64 > themeFileMaxBytes {
			return nil, nil, fmt.Errorf("%q is larger than %d MiB", e.name, themeFileMaxBytes>>20)
		}
		if seen[e.name] {
			return nil, nil, fmt.Errorf("%q appears twice in the package", e.name)
		}
		seen[e.name] = true
		rc, err := e.f.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("%q: %v", e.name, err)
		}
		b, err := io.ReadAll(io.LimitReader(rc, themeFileMaxBytes+1))
		rc.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("%q: %v", e.name, err)
		}
		if len(b) > themeFileMaxBytes {
			return nil, nil, fmt.Errorf("%q is larger than %d MiB", e.name, themeFileMaxBytes>>20)
		}
		if total += uint64(len(b)); total > themeTotalMaxBytes {
			return nil, nil, fmt.Errorf("the package extracts to more than %d MiB", themeTotalMaxBytes>>20)
		}
		if e.name == "theme.json" {
			manifest = b
		}
		files = append(files, themeZipFile{Path: e.name, Data: b})
	}
	if manifest == nil {
		return nil, nil, errors.New("theme.json is missing from the package root")
	}
	m, err := parseThemeManifest(manifest)
	if err != nil {
		return nil, nil, err
	}
	if !seen["templates/index.html"] {
		return nil, nil, errors.New("templates/index.html is missing")
	}
	return files, m, nil
}

// themesDir is the directory the packages live in (PURSAE_THEMES_DIR, default <data>/themes).
func (s *Server) themesDir() string {
	if o, ok := s.overlay(); ok && o.dir != "" {
		return o.dir
	}
	return themeOverrideDir(s.cfg.UploadsDir)
}

// loadThemePackages registers every <themes dir>/<slug>/theme.json at start-up. The registry is
// shared by every Server in the process, so the packages registered before are dropped first. A
// broken package is logged and skipped.
func (s *Server) loadThemePackages() {
	if s.sandbox == nil { // a theme-preview sandbox keeps its packages to itself (theme_preview.go)
		for _, t := range packageThemes() {
			unregisterTheme(t.Slug)
		}
	}
	dir := s.themesDir()
	resetThemeLoadFailures(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(sub, "theme.json")); err != nil {
			continue // a bundled theme's edited files
		}
		t, err := loadThemePackage(sub, e.Name())
		if err != nil {
			log.Printf("theme package %s skipped: %v", e.Name(), err)
			recordThemeLoadFailure(dir, e.Name(), err) // shown as an admin notice (theme_safety.go)
			continue
		}
		s.registerPackage(t)
	}
}

// installThemePackage validates a package, extracts it to <themes dir>/<slug>/ and registers the theme.
// An installed package with the same slug is replaced only by a higher version.
func (s *Server) installThemePackage(ctx context.Context, data []byte) (*Theme, error) {
	return s.installThemePackageFrom(ctx, data, themeSourceUpload)
}

// installThemePackageFrom is installThemePackage recording where the package came from
// (themeSourceUpload, themeSourceHub or themeSourceLibrary; theme_library.go).
func (s *Server) installThemePackageFrom(ctx context.Context, data []byte, source string) (*Theme, error) {
	files, m, err := unpackThemeZip(data)
	if err != nil {
		return nil, err
	}
	checkVersion := func(t *Theme) error {
		if t == nil {
			return nil
		}
		if t.Package == nil {
			return fmt.Errorf("the slug %q belongs to a bundled theme", m.Slug)
		}
		if !semverLess(t.Version, m.Version) {
			return fmt.Errorf("%s %s is already installed; the package (version %s) is not newer", t.Name, t.Version, m.Version)
		}
		return nil
	}
	if err := checkVersion(s.themeFor(m.Slug)); err != nil {
		return nil, err
	}
	dir := s.themesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(dir, "."+m.Slug+".install-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	if err := writeThemeFiles(tmp, files); err != nil {
		cleanup()
		return nil, err
	}
	if source != "" {
		if err := os.WriteFile(filepath.Join(tmp, themeSourceFile), []byte(source+"\n"), 0o644); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := s.checkThemeDir(tmp, m.Slug); err != nil {
		cleanup()
		return nil, err
	}
	themeSaveMu.Lock()
	defer themeSaveMu.Unlock()
	// Another install may have published a newer version while this one staged its files.
	current := s.themeFor(m.Slug)
	if err := checkVersion(current); err != nil {
		cleanup()
		return nil, err
	}
	event := "theme_install"
	if current != nil {
		event = "theme_update"
	}
	final := filepath.Join(dir, m.Slug)
	old := ""
	if _, err := os.Stat(final); err == nil {
		// Keep the files edited on this server: the new version replaces the whole folder (theme_safety.go).
		{
			cur, version := &themePackage{Dir: final}, "unknown" // also a package that failed to load
			cur.loadEdited()
			if t := themeBySlug(m.Slug); t != nil && t.Package != nil {
				version = t.Version
			}
			bak, files, err := backupEditedThemeFiles(dir, cur, m.Slug, version)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("could not back up the edited theme files: %w", err)
			}
			if bak != "" {
				rememberThemeBackup(dir, m.Slug, bak, files)
			}
		}
		old = filepath.Join(dir, strings.Replace(filepath.Base(tmp), ".install-", ".old-", 1))
		if err := os.Rename(final, old); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		if old != "" {
			_ = os.Rename(old, final)
		}
		cleanup()
		return nil, err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	t, err := loadThemePackage(final, m.Slug)
	if err != nil {
		return nil, err
	}
	clearThemeLoadFailure(dir, t.Slug)
	s.registerPackage(t)
	s.afterThemeChange(ctx, t.Slug)
	s.hubEvent(event, "theme", t.Slug, t.Version) // statistics (hub_telemetry.go)
	return t, nil
}

// writeThemeFiles writes the files of an unpacked package below dir.
func writeThemeFiles(dir string, files []themeZipFile) error {
	for _, f := range files {
		target := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, f.Data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// checkThemeDir validates an extracted package before it is put in place: the templates parse and
// theme.json, the files it references and the demo content are valid.
func (s *Server) checkThemeDir(dir, slug string) error {
	if _, err := parsePackageTemplates(os.DirFS(dir), "templates", s.tmpl.funcs()); err != nil {
		return fmt.Errorf("templates: %v", err)
	}
	_, err := loadThemePackage(dir, slug)
	return err
}

// afterThemeChange re-parses the theme templates and drops cached pages after a package was
// installed, updated or deleted.
func (s *Server) afterThemeChange(ctx context.Context, slug string) {
	if err := s.tmpl.load(); err != nil {
		log.Printf("theme templates: %v", err)
	}
	s.purgeAll(ctx)
	if s.activeTheme().Slug == slug {
		s.configureCache()
	}
}

// deleteThemePackage is delete_theme(): the directory and the registration go; the active theme
// cannot be deleted.
func (s *Server) deleteThemePackage(ctx context.Context, slug string) error {
	t := themeBySlug(slug)
	if t == nil || t.Package == nil {
		return errors.New("Only uploaded theme packages can be deleted here.")
	}
	if s.activeTheme().Slug == slug {
		return errors.New("You cannot delete a theme while it is active on the site.")
	}
	themeSaveMu.Lock()
	err := os.RemoveAll(t.Package.Dir)
	themeSaveMu.Unlock()
	if err != nil {
		return err
	}
	unregisterTheme(slug)
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM themes WHERE slug = $1`, slug)
	s.afterThemeChange(ctx, slug)
	return nil
}

// packageThemeAsset serves /wp-content/themes/<slug>/… of package themes from their assets directory
// (the bundled themes have static routes; packages come and go at run time, so they are matched here).
func (s *Server) packageThemeAsset(c *gin.Context) {
	rel, ok := strings.CutPrefix(c.Request.URL.Path, "/wp-content/themes/")
	if !ok || (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) {
		return
	}
	slug, file, ok := strings.Cut(rel, "/")
	t := s.themeFor(slug)
	if !ok || t == nil || t.Package == nil || file == "" || strings.HasSuffix(file, "/") || !fs.ValidPath(file) {
		return
	}
	sub, err := fs.Sub(s.web, "themes/"+slug+"/assets")
	if err != nil {
		return
	}
	f, err := http.FS(sub).Open(file)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return
	}
	// The theme's styles and scripts are linked with ?ver=<theme version>, and a new version is a new
	// directory: such a URL never changes, like the core's versioned assets. Files edited in the Theme
	// File Editor keep the version, and unversioned URLs (fonts and images from url(), the
	// screenshot) may change under the same URL, so those revalidate (Last-Modified).
	if v, ok := c.GetQuery("ver"); ok && v == t.Version && !t.Package.isEdited(file) {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		c.Header("Cache-Control", "no-cache")
	}
	http.ServeContent(c.Writer, c.Request, st.Name(), st.ModTime(), f)
	c.Abort()
}

// Screens -------------------------------------------------------------------------------

// themeInstallCard is one card of Add Themes.
type themeInstallCard struct {
	Name, Author, Screenshot, Slug string
	Installed, Active, Package     bool
	Description                    string
	ActivateURL, PreviewURL        string
	Version                        string
	InstallURL                     string // a deleted bundled theme or a library theme: Install puts it in place
	// Library themes.
	Library              bool
	Style, CategoryLabel string
	DemoURL              string // Install & Import Demo
	UpdateURL            string // an older version is installed
	NewVersion           string
	DemoPreviewURL       string // Live Preview with demo content (theme_preview.go)
}

// themeCategoryLink is a category filter of the Library tab.
type themeCategoryLink struct {
	Slug, Label, URL string
	Count            int
	Current          bool
}

// themeInstall is theme-install.php: the theme library, the bundled and uploaded themes, and the
// Upload Theme form.
func (s *Server) themeInstall(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("install_themes", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to install themes on this site.")
		return
	}
	switch c.Query("action") {
	case "library-screenshot":
		s.serveLibraryScreenshot(c)
		return
	case "install-theme": // a deleted bundled theme (theme_delete.go) or a library theme (theme_library.go)
		s.themeInstallAction(c)
		return
	case "install-recommended": // the Manager's preinstall themes (theme_library.go)
		s.themeInstallRecommended(c)
		return
	}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "theme-upload") {
			s.denyNonce(c)
			return
		}
		fail := func(msg string) {
			c.Redirect(http.StatusFound, "/wp-admin/theme-install?browse=upload&error="+url.QueryEscape(msg))
		}
		f, _, err := c.Request.FormFile("themezip")
		if err != nil {
			fail("No package was uploaded.")
			return
		}
		data, _ := io.ReadAll(io.LimitReader(f, themeZipMaxBytes+1))
		f.Close()
		t, err := s.installThemePackage(c, data)
		if err != nil {
			fail("Installation failed: " + err.Error())
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/themes?installed="+url.QueryEscape(t.Slug))
		return
	}
	active := s.activeTheme()
	browse := c.DefaultQuery("browse", "library")
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	category := c.Query("category")
	var cards []themeInstallCard
	var categories []themeCategoryLink
	installURL := func(slug string) string {
		return "/wp-admin/theme-install?action=install-theme&theme=" + slug + "&_wpnonce=" + s.nonce(c, "install-theme_"+slug)
	}
	activateURL := func(slug string) string {
		return "/wp-admin/themes?action=activate&stylesheet=" + slug + "&_wpnonce=" + s.nonce(c, "switch-theme_"+slug)
	}
	canDemo := u.Can("switch_themes", nil) && u.Can("import", nil)
	var libraryNotice string
	recommended := 0
	if browse == "library" {
		library, libErr := s.libraryThemesErr(c)
		switch {
		case errors.Is(libErr, errLibraryOff):
			libraryNotice = "The theme library comes from Pursae Manager, and no Pursae Manager is configured in this build."
		case libErr != nil:
			libraryNotice = "The theme library comes from Pursae Manager, and Pursae Manager is not reachable right now. Try again later."
			slog.Warn("theme library", "err", libErr)
		}
		recommended = len(s.preinstallPending(library))
		counts := map[string]int{}
		for _, lt := range library {
			counts[lt.Manifest.Category]++
		}
		if category != "" && counts[category] == 0 {
			category = ""
		}
		categories = append(categories, themeCategoryLink{Label: "All", URL: "/wp-admin/theme-install", Count: len(library), Current: category == ""})
		for _, cat := range themeCategories {
			if counts[cat] > 0 {
				categories = append(categories, themeCategoryLink{Slug: cat, Label: themeCategoryLabel(cat), Count: counts[cat],
					URL: "/wp-admin/theme-install?browse=library&category=" + cat, Current: category == cat})
			}
		}
		for _, lt := range library {
			m := lt.Manifest
			if category != "" && m.Category != category {
				continue
			}
			if search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{m.Name, m.Author, m.Description, m.Style,
				themeCategoryLabel(m.Category), strings.Join(m.Tags, " ")}, " ")), search) {
				continue
			}
			cd := themeInstallCard{Name: m.Name, Author: m.Author, Slug: m.Slug, Description: m.Description, Version: m.Version,
				Screenshot: libraryScreenshotURL(m),
				Library:    true, Style: m.Style, CategoryLabel: themeCategoryLabel(m.Category), Package: true}
			if canPreview(u, true) {
				cd.DemoPreviewURL = previewURL(m.Slug, false)
			}
			if t := s.installedTheme(m.Slug); t != nil && t.Package != nil {
				cd.Installed, cd.Active = true, t.Slug == active.Slug
				cd.ActivateURL, cd.PreviewURL = activateURL(t.Slug), "/?theme="+t.Slug
				if semverLess(t.Version, m.Version) {
					cd.UpdateURL, cd.NewVersion = installURL(m.Slug), m.Version
				}
			} else {
				cd.InstallURL = installURL(m.Slug)
				if m.Demo != "" && canDemo {
					cd.DemoURL = cd.InstallURL + "&demo=1"
				}
			}
			cards = append(cards, cd)
		}
	} else {
		list := s.installedThemes()
		for _, slug := range s.deletedThemes() {
			if t := themeBySlug(slug); t != nil && t.Package == nil {
				list = append(list, t)
			}
		}
		for _, t := range list {
			cd := themeInstallCard{Name: t.Name, Author: t.Author, Screenshot: t.ScreenshotSrc() + "?ver=" + t.Version, Slug: t.Slug,
				Installed: true, Active: t.Slug == active.Slug, Description: t.Description, Package: t.Package != nil, Version: t.Version,
				ActivateURL: activateURL(t.Slug), PreviewURL: "/?theme=" + t.Slug}
			if t.Package != nil {
				cd.Style, cd.CategoryLabel = t.Package.Manifest.Style, themeCategoryLabel(t.Package.Manifest.Category)
			}
			if s.themeDeleted(t.Slug) {
				cd.Installed, cd.ActivateURL = false, ""
				cd.InstallURL = installURL(t.Slug)
			}
			if search != "" && !strings.Contains(strings.ToLower(cd.Name+" "+cd.Author+" "+cd.Description+" "+cd.Style), search) {
				continue
			}
			switch browse {
			case "bundled":
				if cd.Package {
					continue
				}
			case "uploaded":
				if !cd.Package {
					continue
				}
			case "upload":
			default:
				browse = "all"
			}
			cards = append(cards, cd)
		}
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].Active && !cards[j].Active })
	notices := s.themeLoadNotices(c)
	if msg := c.Query("error"); msg != "" {
		notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(msg)), Dismissible: true})
	}
	if v := c.Query("recommended"); v != "" {
		notices = append(notices, notice{Type: "success", Message: template.HTML("Recommended themes installed: <strong>" + template.HTMLEscapeString(v) + "</strong>."), Dismissible: true})
	}
	if c.Query("previews") == "cleared" {
		notices = append(notices, notice{Type: "success", Message: "The theme previews have been cleared.", Dismissible: true})
	}
	bodyClass := "theme-install-php"
	if browse == "upload" {
		bodyClass += " show-upload-view"
	}
	data := map[string]any{"Cards": cards, "Browse": browse, "Search": c.Query("search"), "Count": len(cards),
		"UploadNonce": s.nonce(c, "theme-upload"), "MaxSize": fmt.Sprintf("%d MB", themeZipMaxBytes>>20),
		"Categories": categories, "Category": category, "LibraryNotice": libraryNotice, "FromManager": s.themeLibrary == nil}
	if recommended > 0 {
		data["Recommended"], data["RecommendedNonce"] = recommended, s.nonce(c, "install-recommended-themes")
	}
	if browse == "library" && canPreview(u, true) {
		data["Previews"], data["PreviewsNonce"] = s.previewCount(c), s.nonce(c, "clear-theme-previews")
	}
	s.adminPage(c, "themes", "Add Themes", bodyClass, "admin/theme-install", data, notices...)
}

// themeInstallAction is Add Themes › Install (and Update, and Install & Import Demo): a deleted
// bundled theme is put back; a library theme is installed or updated through the package installer
// and, with demo=1, activated and given its demo content.
func (s *Server) themeInstallAction(c *gin.Context) {
	slug := c.Query("theme")
	if !s.checkNonce(c, "install-theme_"+slug) {
		s.denyNonce(c)
		return
	}
	fail := func(msg string) {
		c.Redirect(http.StatusFound, "/wp-admin/theme-install?error="+url.QueryEscape(msg))
	}
	if t := themeBySlug(slug); t != nil && t.Package == nil {
		if err := s.restoreTheme(c, slug); err != nil {
			fail(err.Error())
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/themes?installed="+url.QueryEscape(slug))
		return
	}
	lt := s.libraryTheme(c, slug)
	if lt == nil {
		fail("The requested theme does not exist.")
		return
	}
	u := currentUser(c)
	demo := c.Query("demo") == "1"
	if demo && (lt.Manifest.Demo == "" || !u.Can("switch_themes", nil) || !u.Can("import", nil)) {
		fail("This theme’s demo content cannot be imported here.")
		return
	}
	installed := themeBySlug(slug)
	updated := false
	if installed == nil || semverLess(installed.Version, lt.Manifest.Version) {
		updated = installed != nil
		t, err := s.installLibraryTheme(c, lt)
		if err != nil {
			fail("Installation failed: " + err.Error())
			return
		}
		installed = t
	}
	if !demo {
		if updated {
			c.Redirect(http.StatusFound, "/wp-admin/themes?updated="+url.QueryEscape(slug))
		} else {
			c.Redirect(http.StatusFound, "/wp-admin/themes?installed="+url.QueryEscape(slug))
		}
		return
	}
	if s.activeTheme().Slug != slug {
		if err := s.switchTheme(c, slug); err != nil {
			fail(err.Error())
			return
		}
	}
	rep, err := s.importThemeDemo(c.Request.Context(), u.ID, installed)
	if err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/themes?activated=true&error="+url.QueryEscape("The demo content could not be imported: "+err.Error()))
		return
	}
	c.Redirect(http.StatusFound, "/wp-admin/themes?activated=true&"+rep.query())
}

// themeInstallRecommended is Add Themes › Library › Install recommended themes: the themes Pursae
// Manager marks for preinstall (installed on new sites after the installer), installed on demand.
func (s *Server) themeInstallRecommended(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.Redirect(http.StatusFound, "/wp-admin/theme-install")
		return
	}
	if !s.checkNonce(c, "install-recommended-themes") {
		s.denyNonce(c)
		return
	}
	done, err := s.preinstallThemes(c)
	q := url.Values{}
	if len(done) > 0 {
		q.Set("recommended", strings.Join(done, ", "))
	}
	if err != nil {
		q.Set("error", "Some themes could not be installed: "+err.Error())
	}
	c.Redirect(http.StatusFound, "/wp-admin/theme-install?"+q.Encode())
}
