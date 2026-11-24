package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// The theme library (docs/theme-package.md, "Theme library"): complete theme packages the site can
// list, preview and install from Appearance › Add Themes › Library. The library is not part of the
// binary; it comes from
//
//   - Pursae Manager (the default): GET /api/v1/themes lists the packages with each version's
//     theme.json ("manifest"), so the cards, categories and search need no download. A package is
//     downloaded (its SHA-256 and size checked, hubDownload) when it is installed, previewed or its
//     files are needed, and is extracted, validated like an upload, into
//     <data>/theme-library-cache/<slug>/<version>/; screenshots are fetched once into the same
//     directory and served by the site, so pro and private ones work and the browser never talks
//     to the Manager;
//   - a directory of package directories when PURSAE_THEME_LIBRARY_DIR is set (development, offline
//     sites and the tests; the repository keeps the library in library/themes).
//
// Install hands the package to installThemePackage, so a library theme is validated, extracted to
// <data>/themes/<slug> and updated exactly like an uploaded package. A file in the installed
// directory records where the package came from (themePackageSource): Updates and the statistics
// (hub_telemetry.go) read it.

// themeCategories are the library categories, in the order of the filter links.
var themeCategories = []string{"blog", "magazine", "portfolio", "photography", "business", "agency", "restaurant", "food",
	"travel", "fashion", "technology", "education", "health", "fitness", "nonprofit", "music", "wedding", "real-estate",
	"personal", "minimal", "home", "shop", "legal", "construction", "finance", "architecture", "gaming", "entertainment",
	"podcast", "beauty", "kids", "pets", "automotive", "hotel", "startup", "science", "religion", "environment", "books",
	"sports"}

var themeCategoryLabels = map[string]string{"blog": "Blog", "magazine": "Magazine", "portfolio": "Portfolio",
	"photography": "Photography", "business": "Business", "agency": "Agency", "restaurant": "Restaurant", "food": "Food",
	"travel": "Travel", "fashion": "Fashion", "technology": "Technology", "education": "Education", "health": "Health",
	"fitness": "Fitness", "nonprofit": "Nonprofit", "music": "Music", "wedding": "Wedding", "real-estate": "Real Estate",
	"personal": "Personal", "minimal": "Minimal", "home": "Home & Interior", "shop": "Shop", "legal": "Legal",
	"construction": "Construction", "finance": "Finance", "architecture": "Architecture", "gaming": "Gaming",
	"entertainment": "Film & TV", "podcast": "Podcast", "beauty": "Beauty", "kids": "Kids", "pets": "Pets",
	"automotive": "Automotive", "hotel": "Hotel", "startup": "Startup", "science": "Science", "religion": "Faith",
	"environment": "Environment", "books": "Books", "sports": "Sports & Outdoors"}

// themeCategoryLabel is the display name of a category ("" for an unknown one).
func themeCategoryLabel(slug string) string { return themeCategoryLabels[slug] }

// Where an installed package came from: the file .pursae-source in its directory.
const (
	themeSourceFile    = ".pursae-source"
	themeSourceHub     = "hub"     // the library, from Pursae Manager
	themeSourceLibrary = "library" // the library, from PURSAE_THEME_LIBRARY_DIR
	themeSourceUpload  = "upload"  // Add Themes › Upload
)

// themePackageSource is the recorded source of an installed package theme ("" when it was
// installed before the source was recorded, or for a bundled theme).
func themePackageSource(t *Theme) string {
	if t == nil || t.Package == nil || t.Package.Dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(t.Package.Dir, themeSourceFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

var (
	errLibraryOff     = errors.New("no Pursae Manager is configured")
	libraryRetryAfter = 2 * time.Minute // a Manager that did not answer is not asked again before
)

// libraryTheme is one theme of the library.
type libraryTheme struct {
	Manifest *themeManifest
	Item     *hubItem // the Manager's listing (nil for the directory source)
	Dir      string   // directory source: the theme's directory inside fsys
	fsys     fs.FS
}

// libraryState is the Manager source's memory: the parsed manifests and the last failure.
type libraryState struct {
	mu        sync.Mutex
	manifests map[string]*themeManifest // by slug@version@sha256
	failAt    time.Time
	failErr   error
	cacheMu   sync.Mutex // extracting into the cache directory
	installMu sync.Mutex // preinstall runs one at a time
}

// libraryThemes lists the valid library themes in library order (libraryLess; empty when there is
// no library).
func (s *Server) libraryThemes(ctx context.Context) []libraryTheme {
	out, _ := s.libraryThemesErr(ctx)
	return out
}

// libraryThemesErr is libraryThemes with the reason the list is empty: errLibraryOff (no source
// configured) or the Manager's error.
func (s *Server) libraryThemesErr(ctx context.Context) ([]libraryTheme, error) {
	var out []libraryTheme
	switch {
	case s.themeLibrary != nil:
		out = s.dirLibraryThemes()
	case s.hubBase() == "" || s.sandbox != nil:
		return nil, errLibraryOff
	default:
		var err error
		if out, err = s.managerLibraryThemes(ctx); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return libraryLess(out[i], out[j]) })
	return out, nil
}

// position is the theme's place in the Manager's library order (0: none).
func (lt libraryTheme) position() int {
	if lt.Item == nil || lt.Item.Position < 0 {
		return 0
	}
	return lt.Item.Position
}

// libraryLess is the library order: the themes the Manager placed (Themes › Library order) first, by
// place, then the others by name. A Manager that sends no positions, and the directory source,
// give plain name order. Add Themes › Library keeps this order within a category and in search
// results, as everything else that lists the library does.
func libraryLess(a, b libraryTheme) bool {
	pa, pb := a.position(), b.position()
	switch {
	case pa > 0 && pb > 0 && pa != pb:
		return pa < pb
	case (pa > 0) != (pb > 0):
		return pa > 0
	}
	if na, nb := strings.ToLower(a.Manifest.Name), strings.ToLower(b.Manifest.Name); na != nb {
		return na < nb
	}
	return a.Manifest.Slug < b.Manifest.Slug
}

// dirLibraryThemes reads the directory source. A directory whose theme.json does not validate,
// whose name differs from its slug or whose slug is a bundled theme is logged and left out.
func (s *Server) dirLibraryThemes() []libraryTheme {
	entries, err := fs.ReadDir(s.themeLibrary, ".")
	if err != nil {
		return nil
	}
	var out []libraryTheme
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := fs.ReadFile(s.themeLibrary, e.Name()+"/theme.json")
		if err != nil {
			continue
		}
		m, err := parseThemeManifest(b)
		if err != nil {
			log.Printf("theme library %s skipped: %v", e.Name(), err)
			continue
		}
		if m.Slug != e.Name() {
			log.Printf("theme library %s skipped: slug %q does not match the directory name", e.Name(), m.Slug)
			continue
		}
		if t := themeBySlug(m.Slug); t != nil && t.Package == nil {
			log.Printf("theme library %s skipped: the slug belongs to a bundled theme", e.Name())
			continue
		}
		out = append(out, libraryTheme{Manifest: m, Dir: e.Name(), fsys: s.themeLibrary})
	}
	return out
}

// managerLibraryThemes reads the Manager's theme catalog (cached 12 hours per license, hubCatalog).
// A listing whose manifest does not validate or does not match it is logged and left out.
func (s *Server) managerLibraryThemes(ctx context.Context) ([]libraryTheme, error) {
	st := &s.library
	st.mu.Lock()
	if st.failErr != nil && time.Since(st.failAt) < libraryRetryAfter {
		err := st.failErr
		st.mu.Unlock()
		return nil, err
	}
	st.mu.Unlock()
	items, err := s.hubCatalog(ctx, "themes")
	if err != nil {
		st.mu.Lock()
		st.failAt, st.failErr = time.Now(), err
		st.mu.Unlock()
		return nil, err
	}
	var out []libraryTheme
	for _, it := range items {
		m := s.libraryManifest(it)
		if m == nil {
			continue
		}
		if t := themeBySlug(m.Slug); t != nil && t.Package == nil {
			continue // the slug belongs to a bundled theme
		}
		out = append(out, libraryTheme{Manifest: m, Item: &it})
	}
	return out, nil
}

// libraryManifest parses (once per version) the theme.json the Manager lists with a theme.
func (s *Server) libraryManifest(it hubItem) *themeManifest {
	if !validSlug(it.Slug) || len(it.Manifest) == 0 {
		return nil
	}
	key := it.Slug + "@" + it.Version + "@" + it.SHA256
	st := &s.library
	st.mu.Lock()
	defer st.mu.Unlock()
	if m, ok := st.manifests[key]; ok {
		return m
	}
	m, err := parseThemeManifest(it.Manifest)
	switch {
	case err != nil:
		log.Printf("theme library %s skipped: %v", it.Slug, err)
		m = nil
	case m.Slug != it.Slug || m.Version != it.Version:
		log.Printf("theme library %s skipped: theme.json (%s %s) does not match the listing (%s)", it.Slug, m.Slug, m.Version, it.Version)
		m = nil
	}
	if st.manifests == nil {
		st.manifests = map[string]*themeManifest{}
	}
	st.manifests[key] = m
	return m
}

// libraryForget drops what the Manager source remembers (Updates › Check again).
func (s *Server) libraryForget() {
	s.library.mu.Lock()
	s.library.failErr, s.library.manifests = nil, nil
	s.library.mu.Unlock()
}

// libraryTheme returns the library entry of a slug.
func (s *Server) libraryTheme(ctx context.Context, slug string) *libraryTheme {
	for _, lt := range s.libraryThemes(ctx) {
		if lt.Manifest.Slug == slug {
			return &lt
		}
	}
	return nil
}

// zip packages a theme directory of fsys the way tools/build-themes.sh does (files at the zip root).
func zipThemeDir(fsys fs.FS, dir string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasPrefix(path.Base(p), ".") {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		w, err := zw.Create(strings.TrimPrefix(p, dir+"/"))
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// libraryCacheDir holds the downloaded library packages and screenshots (next to the uploads).
func (s *Server) libraryCacheDir() string {
	return filepath.Join(filepath.Dir(filepath.Clean(s.cfg.UploadsDir)), "theme-library-cache")
}

// libraryFiles returns the theme's files as a directory of an fs.FS: the directory source's, or the
// Manager package extracted into the cache (downloaded on first use).
func (s *Server) libraryFiles(ctx context.Context, lt *libraryTheme) (fs.FS, string, error) {
	if lt.Item == nil {
		return lt.fsys, lt.Dir, nil
	}
	root := filepath.Join(s.libraryCacheDir(), lt.Manifest.Slug)
	if _, err := os.Stat(filepath.Join(root, lt.Manifest.Version, "theme.json")); err != nil {
		if _, err := s.libraryPackage(ctx, lt); err != nil {
			return nil, "", err
		}
	}
	return os.DirFS(root), lt.Manifest.Version, nil
}

// libraryPackage returns the theme's package zip: zipped from the directory source, or downloaded
// from the Manager (SHA-256 and size checked, contents validated like an upload, slug and version
// matching the listing) and extracted into the cache on the way.
func (s *Server) libraryPackage(ctx context.Context, lt *libraryTheme) ([]byte, error) {
	if lt.Item == nil {
		return zipThemeDir(lt.fsys, lt.Dir)
	}
	if lt.Item.SHA256 == "" { // themes are not signed: the checksum is what vouches for the package
		return nil, errors.New("Pursae Manager lists no checksum for this theme")
	}
	data, err := s.hubDownload(ctx, lt.Item)
	if err != nil {
		return nil, err
	}
	files, m, err := unpackThemeZip(data)
	if err != nil {
		return nil, fmt.Errorf("the package from Pursae Manager is not a valid theme: %v", err)
	}
	if m.Slug != lt.Manifest.Slug || m.Version != lt.Manifest.Version {
		return nil, fmt.Errorf("the package from Pursae Manager (%s %s) does not match its listing (%s %s)", m.Slug, m.Version, lt.Manifest.Slug, lt.Manifest.Version)
	}
	if err := s.libraryExtract(m, files); err != nil {
		return nil, err
	}
	return data, nil
}

// libraryExtract writes a validated package into <cache>/<slug>/<version>/ (a temporary directory
// renamed into place) and removes the other versions of the slug.
func (s *Server) libraryExtract(m *themeManifest, files []themeZipFile) error {
	st := &s.library
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	root := filepath.Join(s.libraryCacheDir(), m.Slug)
	final := filepath.Join(root, m.Version)
	if _, err := os.Stat(filepath.Join(final, "theme.json")); err == nil {
		return nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(root, ".extract-")
	if err != nil {
		return err
	}
	err = writeThemeFiles(tmp, files)
	if err == nil {
		err = s.checkThemeDir(tmp, m.Slug)
	}
	if err == nil {
		_ = os.RemoveAll(final)
		err = os.Rename(tmp, final)
	}
	if err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if n := e.Name(); n != m.Version && !strings.HasPrefix(n, ".") && !strings.HasPrefix(n, "screenshot-"+m.Version+".") {
			_ = os.RemoveAll(filepath.Join(root, n))
		}
	}
	return nil
}

// installLibraryTheme installs (or updates) a library theme through the package installer.
func (s *Server) installLibraryTheme(ctx context.Context, lt *libraryTheme) (*Theme, error) {
	data, err := s.libraryPackage(ctx, lt)
	if err != nil {
		return nil, err
	}
	source := themeSourceLibrary
	if lt.Item != nil {
		source = themeSourceHub
	}
	return s.installThemePackageFrom(ctx, data, source)
}

// libraryScreenshotURL is the site's URL of a library theme's screenshot.
func libraryScreenshotURL(m *themeManifest) string {
	return "/wp-admin/theme-install?action=library-screenshot&theme=" + m.Slug + "&ver=" + m.Version
}

// serveLibraryScreenshot serves a library theme's screenshot (?action=library-screenshot&theme=slug):
// the theme is not installed, so its assets have no URL yet. A Manager theme's screenshot comes from
// the cached package or is fetched once from the Manager into the cache.
func (s *Server) serveLibraryScreenshot(c *gin.Context) {
	lt := s.libraryTheme(c, c.Query("theme"))
	if lt == nil || !fs.ValidPath(lt.Manifest.Screenshot) {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "private, max-age=3600")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox") // an SVG opened on its own
	if lt.Item == nil {
		http.ServeFileFS(c.Writer, c.Request, lt.fsys, lt.Dir+"/"+lt.Manifest.Screenshot)
		return
	}
	pkg := filepath.Join(s.libraryCacheDir(), lt.Manifest.Slug, lt.Manifest.Version, filepath.FromSlash(lt.Manifest.Screenshot))
	if st, err := os.Stat(pkg); err == nil && !st.IsDir() {
		http.ServeFile(c.Writer, c.Request, pkg)
		return
	}
	file, err := s.libraryScreenshotFile(c, lt)
	if err != nil {
		slog.Warn("theme library screenshot", "theme", lt.Manifest.Slug, "err", err)
		c.Status(http.StatusNotFound)
		return
	}
	http.ServeFile(c.Writer, c.Request, file)
}

// libraryScreenshotTypes maps the image types the Manager serves to file extensions.
var libraryScreenshotTypes = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp",
	"image/gif": ".gif", "image/avif": ".avif", "image/svg+xml": ".svg"}

// libraryScreenshotFile downloads a Manager theme's screenshot into <cache>/<slug>/screenshot-<version>.<ext>
// (once) and returns the file.
func (s *Server) libraryScreenshotFile(ctx context.Context, lt *libraryTheme) (string, error) {
	root := filepath.Join(s.libraryCacheDir(), lt.Manifest.Slug)
	prefix := filepath.Join(root, "screenshot-"+lt.Manifest.Version+".")
	if m, _ := filepath.Glob(prefix + "*"); len(m) > 0 {
		return m[0], nil
	}
	base := s.hubBase()
	if base == "" {
		return "", errHubOff
	}
	u := lt.Item.ScreenshotURL
	if !strings.HasPrefix(u, base+"/") { // only the Manager itself, never a third-party URL
		u = base + "/api/v1/themes/" + lt.Manifest.Slug + "/screenshot"
	}
	req, err := s.hubNewRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.hubClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	ext, ok := libraryScreenshotTypes[strings.ToLower(strings.TrimSpace(ct))]
	if !ok {
		return "", fmt.Errorf("unexpected content type %q", ct)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, themeFileMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > themeFileMaxBytes {
		return "", errors.New("the screenshot is too large")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(root, ".screenshot-")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	final := prefix[:len(prefix)-1] + ext
	if err == nil {
		err = os.Rename(f.Name(), final)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return final, nil
}

// Preinstall ------------------------------------------------------------------------------

// preinstallThemesAsync installs the Manager's preinstall themes in the background after the
// installer has finished; errors are logged, nothing is activated.
func (s *Server) preinstallThemesAsync() {
	if s.themeLibrary != nil || s.hubBase() == "" || s.sandbox != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		done, err := s.preinstallThemes(ctx)
		if err != nil {
			slog.Warn("theme preinstall", "err", err)
		}
		if len(done) > 0 {
			slog.Info("theme preinstall", "installed", strings.Join(done, ", "))
		}
	}()
}

// preinstallThemes installs every Manager library theme marked preinstall that is not installed yet
// (without activating it) and returns their names. It does nothing without a Manager.
func (s *Server) preinstallThemes(ctx context.Context) ([]string, error) {
	if s.themeLibrary != nil {
		return nil, nil
	}
	s.library.installMu.Lock()
	defer s.library.installMu.Unlock()
	themes, err := s.libraryThemesErr(ctx)
	if errors.Is(err, errLibraryOff) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var done []string
	var errs []error
	for _, lt := range s.preinstallPending(themes) {
		if _, err := s.installLibraryTheme(ctx, &lt); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", lt.Manifest.Slug, err))
			continue
		}
		done = append(done, lt.Manifest.Name)
	}
	return done, errors.Join(errs...)
}

// preinstallPending are the Manager's preinstall themes that are not installed.
func (s *Server) preinstallPending(themes []libraryTheme) []libraryTheme {
	var out []libraryTheme
	for _, lt := range themes {
		if lt.Item != nil && lt.Item.Preinstall && themeBySlug(lt.Manifest.Slug) == nil {
			out = append(out, lt)
		}
	}
	return out
}

// hubThemeListing is a Manager theme listing (fetched fresh, for an update) as a library entry.
func (s *Server) hubThemeListing(ctx context.Context, slug string) (*libraryTheme, error) {
	it, err := s.hubItemFor(ctx, "themes", slug)
	if err != nil {
		return nil, err
	}
	if it.Slug == "" {
		it.Slug = slug
	}
	m := s.libraryManifest(*it)
	if m == nil || m.Slug != slug {
		return nil, errors.New("the theme's listing on Pursae Manager is not valid")
	}
	return &libraryTheme{Manifest: m, Item: it}, nil
}
