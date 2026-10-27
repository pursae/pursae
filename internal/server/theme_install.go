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

// themeFileExt lists the file types a package may contain: themes carry no executable code.
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

// themesDir is the directory the packages live in (WPGO_THEMES_DIR, default <data>/themes).
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
	for _, t := range packageThemes() {
		unregisterTheme(t.Slug)
	}
	dir := s.themesDir()
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
			continue
		}
		registerThemePackage(t)
	}
}

// installThemePackage validates a package, extracts it to <themes dir>/<slug>/ and registers the theme.
// An installed package with the same slug is replaced only by a higher version.
func (s *Server) installThemePackage(ctx context.Context, data []byte) (*Theme, error) {
	files, m, err := unpackThemeZip(data)
	if err != nil {
		return nil, err
	}
	if t := themeBySlug(m.Slug); t != nil {
		if t.Package == nil {
			return nil, fmt.Errorf("the slug %q belongs to a bundled theme", m.Slug)
		}
		if !semverLess(t.Version, m.Version) {
			return nil, fmt.Errorf("%s %s is already installed; the package (version %s) is not newer", t.Name, t.Version, m.Version)
		}
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
	for _, f := range files {
		target := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			cleanup()
			return nil, err
		}
		if err := os.WriteFile(target, f.Data, 0o644); err != nil {
			cleanup()
			return nil, err
		}
	}
	// The templates must parse before anything is replaced.
	if _, err := parsePackageTemplates(os.DirFS(tmp), "templates", s.tmpl.funcs()); err != nil {
		cleanup()
		return nil, fmt.Errorf("templates: %v", err)
	}
	if _, err := loadThemePackage(tmp, m.Slug); err != nil {
		cleanup()
		return nil, err
	}
	themeSaveMu.Lock()
	defer themeSaveMu.Unlock()
	final := filepath.Join(dir, m.Slug)
	old := ""
	if _, err := os.Stat(final); err == nil {
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
	registerThemePackage(t)
	s.afterThemeChange(ctx, t.Slug)
	return t, nil
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
	t := themeBySlug(slug)
	if !ok || t == nil || t.Package == nil || file == "" || strings.HasSuffix(file, "/") || !fs.ValidPath(file) {
		return
	}
	sub, err := fs.Sub(s.web, "themes/"+slug+"/assets")
	if err != nil {
		return
	}
	if st, err := fs.Stat(sub, file); err != nil || st.IsDir() {
		return
	}
	c.Header("Cache-Control", "no-cache") // files may be edited in the Theme File Editor: revalidate
	http.StripPrefix(t.URI(), http.FileServer(http.FS(sub))).ServeHTTP(c.Writer, c.Request)
	c.Abort()
}

// Screens -------------------------------------------------------------------------------

// themeInstall is theme-install.php: the bundled and uploaded themes, and the Upload Theme form.
func (s *Server) themeInstall(c *gin.Context) {
	if !currentUser(c).Can("install_themes", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to install themes on this site.")
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
	type card struct {
		Name, Author, Screenshot, Slug string
		Installed, Active, Package     bool
		Description                    string
		ActivateURL, PreviewURL        string
		Custom                         bool
		Version                        string
	}
	browse := c.DefaultQuery("browse", "all")
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	var cards []card
	for _, t := range s.installedThemes() {
		cd := card{Name: t.Name, Author: t.Author, Screenshot: t.ScreenshotSrc(), Slug: t.Slug, Installed: true, Active: t.Slug == active.Slug,
			Description: t.Description, Package: t.Package != nil, Version: t.Version,
			ActivateURL: "/wp-admin/themes?action=activate&stylesheet=" + t.Slug + "&_wpnonce=" + s.nonce(c, "switch-theme_"+t.Slug), PreviewURL: "/?theme=" + t.Slug, Custom: t.Custom}
		if search != "" && !strings.Contains(strings.ToLower(cd.Name+" "+cd.Author+" "+cd.Description), search) {
			continue
		}
		switch browse {
		case "bundled":
			if cd.Package || cd.Custom {
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
		if cd.Active {
			cards = append([]card{cd}, cards...)
		} else {
			cards = append(cards, cd)
		}
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].Active && !cards[j].Active })
	var notices []notice
	if msg := c.Query("error"); msg != "" {
		notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(msg)), Dismissible: true})
	}
	bodyClass := "theme-install-php"
	if browse == "upload" {
		bodyClass += " show-upload-view"
	}
	data := map[string]any{"Cards": cards, "Browse": browse, "Search": c.Query("search"), "Count": len(cards), "Golementor": s.golementorOn(),
		"UploadNonce": s.nonce(c, "theme-upload"), "MaxSize": fmt.Sprintf("%d MB", themeZipMaxBytes>>20)}
	s.adminPage(c, "themes", "Add Themes", bodyClass, "admin/theme-install", data, notices...)
}
