package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The theme library from Pursae Manager (theme_library.go): the fake Manager (hub_test.go) lists theme
// packages zipped from the fixture theme (testdata/theme-library/wpgo-fixture) under other slugs.

// fakeThemes is the theme catalog of the fake Manager.
type fakeThemes struct {
	list    []*fakeTheme
	perPage int  // listings per page (default 100)
	badSHA  bool // announce a wrong sha256
	shots   int  // screenshot requests
	pages   []string
}

type fakeTheme struct {
	slug, latest string
	access       string // public | pro
	preinstall   bool
	position     int               // place in the library order (0: none, not sent)
	zips         map[string][]byte // by version
	manifests    map[string][]byte
}

// fixtureThemePackage zips the fixture theme as slug/name/version with the given category.
func fixtureThemePackage(t *testing.T, slug, name, version, category string) (zip, manifest []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), slug)
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(fixtureLibrary, "wpgo-fixture"))); err != nil {
		t.Fatal(err)
	}
	editFile(t, filepath.Join(dir, "theme.json"), map[string]string{
		`"slug": "wpgo-fixture"`: `"slug": "` + slug + `"`, `"name": "Fixture Journal"`: `"name": "` + name + `"`,
		`"version": "1.1.0"`: `"version": "` + version + `"`, `"category": "blog"`: `"category": "` + category + `"`,
	})
	manifest, err := os.ReadFile(filepath.Join(dir, "theme.json"))
	if err != nil {
		t.Fatal(err)
	}
	return zipDir(t, dir), manifest
}

// addTheme publishes a version of a theme (a new listing or a newer version of one).
func (m *fakeManager) addTheme(t *testing.T, slug, name, version, category, access string, preinstall bool) {
	t.Helper()
	zip, manifest := fixtureThemePackage(t, slug, name, version, category)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, th := range m.themeLib.list {
		if th.slug == slug {
			th.zips[version], th.manifests[version], th.latest = zip, manifest, version
			th.access, th.preinstall = access, preinstall
			return
		}
	}
	m.themeLib.list = append(m.themeLib.list, &fakeTheme{slug: slug, latest: version, access: access, preinstall: preinstall,
		zips: map[string][]byte{version: zip}, manifests: map[string][]byte{version: manifest}})
}

func (m *fakeManager) setPreinstall(slug string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, th := range m.themeLib.list {
		if th.slug == slug {
			th.preinstall = on
		}
	}
}

// setPositions sets the library order (slug → place; the others get none).
func (m *fakeManager) setPositions(pos map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, th := range m.themeLib.list {
		th.position = pos[th.slug]
	}
}

func (m *fakeManager) screenshotRequests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.themeLib.shots
}

// serveThemes answers /api/v1/themes (paged), /api/v1/themes/{slug}, …/download/{version} and
// …/screenshot. Pro listings exist only for the valid license key. Called with m.mu held.
func (m *fakeManager) serveThemes(w http.ResponseWriter, r *http.Request) {
	licensed := m.validKey != "" && r.Header.Get("X-Pursae-License") == m.validKey
	visible := func(th *fakeTheme) bool { return th.access != "pro" || licensed }
	item := func(th *fakeTheme) map[string]any {
		zip := th.zips[th.latest]
		sum := sha256.Sum256(zip)
		sha := hex.EncodeToString(sum[:])
		if m.themeLib.badSHA {
			sha = strings.Repeat("0", 64)
		}
		it := map[string]any{"slug": th.slug, "name": th.slug, "version": th.latest, "author": "Pursae", "sha256": sha, "size": len(zip),
			"download_url":   m.URL + "/api/v1/themes/" + th.slug + "/download/" + th.latest,
			"screenshot_url": m.URL + "/api/v1/themes/" + th.slug + "/screenshot",
			"access":         th.access, "channel": "stable", "preinstall": th.preinstall, "manifest": json.RawMessage(th.manifests[th.latest])}
		if th.position > 0 {
			it["position"] = th.position
		}
		return it
	}
	find := func(slug string) *fakeTheme {
		for _, th := range m.themeLib.list {
			if th.slug == slug && visible(th) {
				return th
			}
		}
		return nil
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1/themes"), "/")
	if rest == "" {
		per := m.themeLib.perPage
		if per == 0 {
			per = 100
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		m.themeLib.pages = append(m.themeLib.pages, strconv.Itoa(page))
		var all []any
		for _, th := range m.themeLib.list {
			if visible(th) {
				all = append(all, item(th))
			}
		}
		from, to := min((page-1)*per, len(all)), min(page*per, len(all))
		json.NewEncoder(w).Encode(map[string]any{"total": len(all), "pages": (len(all) + per - 1) / per, "page": page, "items": all[from:to]})
		return
	}
	slug, sub, _ := strings.Cut(rest, "/")
	th := find(slug)
	switch {
	case th == nil:
		w.WriteHeader(http.StatusNotFound)
	case sub == "":
		json.NewEncoder(w).Encode(item(th))
	case sub == "screenshot":
		m.themeLib.shots++
		png, _ := os.ReadFile(filepath.Join(fixtureLibrary, "wpgo-fixture", "assets", "screenshot.png"))
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	case strings.HasPrefix(sub, "download/"):
		zip, ok := th.zips[strings.TrimPrefix(sub, "download/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zip)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// managerThemeSite is managerSite with the library read from the fake Manager (not library/themes).
func managerThemeSite(t *testing.T) (base string, c *http.Client, srv *Server, hub *fakeManager) {
	t.Helper()
	t.Setenv("PURSAE_THEMES_DIR", t.TempDir())
	base, c, srv, hub = managerSite(t)
	srv.themeLibrary = nil
	t.Cleanup(srv.closePreviews)
	t.Cleanup(func() {
		for _, slug := range []string{"mgr-journal", "mgr-folio", "mgr-pro"} {
			unregisterTheme(slug)
		}
	})
	return base, c, srv, hub
}

var libraryInstallRe = regexp.MustCompile(`href="(/wp-admin/theme-install\?action=install-theme&amp;theme=([a-z-]+)&amp;_wpnonce=[^"&]+)"`)

// libraryInstallLink is the Install link of a library card.
func libraryInstallLink(t *testing.T, c *http.Client, base, slug string) string {
	t.Helper()
	_, body, _ := get(t, c, base+"/wp-admin/theme-install")
	for _, m := range libraryInstallRe.FindAllStringSubmatch(body, -1) {
		if m[2] == slug {
			return base + html.UnescapeString(m[1])
		}
	}
	t.Fatalf("no Install link for %s on Add Themes › Library", slug)
	return ""
}

func TestManagerThemeLibrary(t *testing.T) {
	base, c, srv, hub := managerThemeSite(t)
	ctx := context.Background()
	hub.addTheme(t, "mgr-journal", "Manager Journal", "1.0.0", "blog", "public", false)
	hub.addTheme(t, "mgr-folio", "Manager Folio", "1.0.0", "portfolio", "public", false)
	hub.addTheme(t, "mgr-pro", "Manager Pro", "1.0.0", "business", "pro", false)
	hub.themeLib.perPage = 1

	// The catalog, all pages of it: cards, categories and style from the manifests.
	_, body, _ := get(t, c, base+"/wp-admin/theme-install")
	for _, want := range []string{"Manager Journal", "Manager Folio", "Ready-made themes from Pursae Manager", "Quiet serif journal",
		`>Blog <span class="count">(1)</span>`, `>Portfolio <span class="count">(1)</span>`, `>All <span class="count">(2)</span>`,
		`src="/wp-admin/theme-install?action=library-screenshot&amp;theme=mgr-journal&amp;ver=1.0.0"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Library lacks %q", want)
		}
	}
	if strings.Contains(body, "Manager Pro") {
		t.Error("a pro theme is listed without a license")
	}
	if !slices.Equal(hub.themeLib.pages, []string{"1", "2"}) {
		t.Errorf("catalog pages read: %v", hub.themeLib.pages)
	}
	get(t, c, base+"/wp-admin/theme-install?browse=library&category=portfolio")
	if len(hub.themeLib.pages) != 2 {
		t.Error("the catalog is not cached")
	}

	// The screenshot is fetched once through the site, with the site's headers.
	shot := base + "/wp-admin/theme-install?action=library-screenshot&theme=mgr-journal&ver=1.0.0"
	png, _ := os.ReadFile(filepath.Join(fixtureLibrary, "wpgo-fixture", "assets", "screenshot.png"))
	for i := 0; i < 2; i++ {
		resp, err := c.Get(shot)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(b, png) {
			t.Fatalf("screenshot: %d %s %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(b))
		}
	}
	if n := hub.screenshotRequests(); n != 1 {
		t.Errorf("the Manager was asked for the screenshot %d times", n)
	}
	for _, r := range hub.requests() {
		if r.Site != hubSiteURL(base) {
			t.Errorf("%s without X-Pursae-Site", r.Path)
		}
	}

	// A package that does not match its checksum is refused.
	hub.mu.Lock()
	hub.themeLib.badSHA = true
	hub.mu.Unlock()
	srv.hubDropCaches(ctx)
	code, _, loc := get(t, c, libraryInstallLink(t, c, base, "mgr-journal"))
	if code != http.StatusFound || !strings.Contains(loc, "error=") || !strings.Contains(loc, "checksum") {
		t.Errorf("bad sha256: %d %s", code, loc)
	}
	if themeBySlug("mgr-journal") != nil {
		t.Fatal("a package with a bad checksum was installed")
	}
	hub.mu.Lock()
	hub.themeLib.badSHA = false
	hub.mu.Unlock()
	srv.hubDropCaches(ctx)

	// Install: downloaded, installed as a package from the Manager.
	code, _, loc = get(t, c, libraryInstallLink(t, c, base, "mgr-journal"))
	if code != http.StatusFound || !strings.Contains(loc, "installed=mgr-journal") {
		t.Fatalf("install: %d %s", code, loc)
	}
	th := themeBySlug("mgr-journal")
	if th == nil || th.Package == nil || th.Version != "1.0.0" || themePackageSource(th) != themeSourceHub {
		t.Fatalf("installed theme %+v", th)
	}
	var inv string
	for _, e := range srv.themeInventory() {
		if e.Slug == "mgr-journal" {
			inv = e.Source
		}
	}
	if inv != "hub" {
		t.Errorf("inventory source %q, want hub", inv)
	}

	// Live Preview of a theme that is not installed: the package is downloaded into the cache and
	// the sandbox is built from it.
	if code, body, _ := get(t, c, base+"/wp-admin/theme-preview?theme=mgr-folio"); code != http.StatusOK || !strings.Contains(body, "Manager Folio") {
		t.Fatalf("preview: %d", code)
	}
	waitReady(t, c, base, "mgr-folio")
	if code, page, _ := get(t, c, base+"/wp-admin/theme-preview/mgr-folio/"); code != http.StatusOK || !strings.Contains(page, "fixture-style") {
		t.Errorf("preview page: %d", code)
	}
	if _, err := os.Stat(filepath.Join(srv.libraryCacheDir(), "mgr-folio", "1.0.0", "theme.json")); err != nil {
		t.Errorf("the package is not in the library cache: %v", err)
	}
	if themeBySlug("mgr-folio") != nil {
		t.Error("the previewed theme was installed on the site")
	}

	// A license with pro: the pro theme appears.
	if err := srv.settings.Set(ctx, map[string]any{licenseOption: hub.validKey}); err != nil {
		t.Fatal(err)
	}
	_, body, _ = get(t, c, base+"/wp-admin/theme-install")
	if !strings.Contains(body, "Manager Pro") {
		t.Error("the pro theme is not listed for a pro license")
	}
}

func TestManagerThemeUpdate(t *testing.T) {
	base, c, srv, hub := managerThemeSite(t)
	ctx := context.Background()
	hub.addTheme(t, "mgr-journal", "Manager Journal", "1.0.0", "blog", "public", false)
	if code, _, loc := get(t, c, libraryInstallLink(t, c, base, "mgr-journal")); code != http.StatusFound {
		t.Fatalf("install: %d %s", code, loc)
	}
	_, body, _ := get(t, c, base+"/wp-admin/themes")
	a := regexp.MustCompile(`href="(/wp-admin/themes\?action=activate&amp;stylesheet=mgr-journal&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	if a == nil {
		t.Fatal("no Activate link")
	}
	get(t, c, base+html.UnescapeString(a[1]))
	if srv.activeTheme().Slug != "mgr-journal" {
		t.Fatal("not activated")
	}
	_, body, _ = get(t, c, base+"/wp-admin/update-core")
	if !strings.Contains(body, "Your themes are all up to date.") {
		t.Error("an update is offered for the newest version")
	}

	// 1.0.1 is published: Themes and Updates offer it.
	hub.addTheme(t, "mgr-journal", "Manager Journal", "1.0.1", "blog", "public", false)
	srv.hubDropCaches(ctx)
	_, body, _ = get(t, c, base+"/wp-admin/themes")
	m := regexp.MustCompile(`href="(/wp-admin/themes\?action=upgrade-theme&amp;stylesheet=mgr-journal&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, "New version available.") || !strings.Contains(body, `"hasUpdate":true`) {
		t.Fatal("no update notice on Appearance › Themes")
	}
	_, body, _ = get(t, c, base+"/wp-admin/update-core")
	if !strings.Contains(body, "You have version 1.0.0 installed. Update to 1.0.1.") || !strings.Contains(body, `value="Update Themes"`) {
		t.Fatal("Dashboard › Updates does not list the theme")
	}
	if code, _, _ := get(t, c, base+"/wp-admin/themes?action=upgrade-theme&stylesheet=mgr-journal&_wpnonce=bad"); code == http.StatusFound {
		t.Error("update without a valid nonce")
	}
	nonce := formNonce(t, body[strings.Index(body, "upgrade-themes"):])
	_, _, loc := post(t, c, base+"/wp-admin/update-core?action=do-theme-upgrade", url.Values{"_wpnonce": {nonce}, "checked[]": {"mgr-journal"}})
	if !strings.Contains(loc, "themes-updated=Manager+Journal+1.0.1") {
		t.Fatalf("Update Themes: %s", loc)
	}
	if th := themeBySlug("mgr-journal"); th == nil || th.Version != "1.0.1" || srv.activeTheme().Slug != "mgr-journal" {
		t.Errorf("after the update: %+v, active %s", th, srv.activeTheme().Slug)
	}
	_, body, _ = get(t, c, base+"/wp-admin/update-core")
	if !strings.Contains(body, "Your themes are all up to date.") {
		t.Error("the updated theme is still listed")
	}

	// Update now on Appearance › Themes.
	hub.addTheme(t, "mgr-journal", "Manager Journal", "1.0.2", "blog", "public", false)
	srv.hubDropCaches(ctx)
	_, body, _ = get(t, c, base+"/wp-admin/themes")
	if m = regexp.MustCompile(`href="(/wp-admin/themes\?action=upgrade-theme&amp;stylesheet=mgr-journal&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body); m == nil {
		t.Fatal("no Update now link")
	}
	code, _, loc := get(t, c, base+html.UnescapeString(m[1]))
	if code != http.StatusFound || !strings.Contains(loc, "updated=mgr-journal") {
		t.Fatalf("Update now: %d %s", code, loc)
	}
	if th := themeBySlug("mgr-journal"); th.Version != "1.0.2" || srv.activeTheme().Slug != "mgr-journal" {
		t.Errorf("after Update now: %s, active %s", th.Version, srv.activeTheme().Slug)
	}
	_, body, _ = get(t, c, base+loc)
	if !strings.Contains(body, "Manager Journal has been updated to version 1.0.2.") {
		t.Error("no updated notice")
	}
	srv.hubFlushEvents(ctx)
	if got := hub.eventTypes(); slices.Index(got, "theme_update:mgr-journal") < 0 || slices.Index(got, "theme_install:mgr-journal") < 0 {
		t.Errorf("events %v", got)
	}

	// An uploaded package is not updated from the Manager.
	th := themeBySlug("mgr-journal")
	if err := os.WriteFile(filepath.Join(th.Package.Dir, themeSourceFile), []byte("upload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub.addTheme(t, "mgr-journal", "Manager Journal", "1.0.3", "blog", "public", false)
	srv.hubDropCaches(ctx)
	if ups := srv.hubThemeUpdates(ctx); len(ups) != 0 {
		t.Errorf("an uploaded theme has Manager updates: %v", ups)
	}
}

func TestManagerThemePreinstall(t *testing.T) {
	t.Setenv("PURSAE_THEMES_DIR", t.TempDir())
	ts, c, srv := testServerBuilderOff(t)
	srv.themeLibrary = nil
	t.Cleanup(srv.closePreviews)
	t.Cleanup(func() {
		for _, slug := range []string{"mgr-journal", "mgr-folio", "mgr-pro"} {
			unregisterTheme(slug)
		}
	})
	hub := fakeHub(t)
	hub.addTheme(t, "mgr-journal", "Manager Journal", "1.0.0", "blog", "public", true)
	hub.addTheme(t, "mgr-folio", "Manager Folio", "1.0.0", "portfolio", "public", false)
	hub.addTheme(t, "mgr-pro", "Manager Pro", "1.0.0", "business", "pro", true)
	srv.cfg.HubURL = hub.URL
	base := ts.URL

	start := time.Now()
	installAndLogin(t, ts, c)
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("the installer took %s", d)
	}
	deadline := time.Now().Add(30 * time.Second)
	for themeBySlug("mgr-journal") == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	srv.library.installMu.Lock() // wait for the rest of the preinstall run
	srv.library.installMu.Unlock()
	th := themeBySlug("mgr-journal")
	if th == nil || themePackageSource(th) != themeSourceHub {
		t.Fatalf("the preinstall theme was not installed: %+v", th)
	}
	if themeBySlug("mgr-folio") != nil || themeBySlug("mgr-pro") != nil {
		t.Error("a theme that is not marked preinstall (or not visible to the site) was installed")
	}
	if srv.activeTheme().Slug != defaultTheme {
		t.Errorf("a preinstalled theme was activated: %s", srv.activeTheme().Slug)
	}
	srv.hubFlushEvents(context.Background())
	if got := hub.eventTypes(); slices.Index(got, "theme_install:mgr-journal") < 0 {
		t.Errorf("events %v", got)
	}

	// Add Themes › Library › Install recommended themes does the same on demand.
	hub.setPreinstall("mgr-folio", true)
	srv.hubDropCaches(context.Background())
	_, body, _ := get(t, c, base+"/wp-admin/theme-install")
	if !strings.Contains(body, "1 recommended theme is not installed yet.") || !strings.Contains(body, `value="Install recommended themes"`) {
		t.Fatal("no Install recommended themes button")
	}
	i := strings.Index(body, `class="wpgo-install-recommended"`)
	code, _, loc := post(t, c, base+"/wp-admin/theme-install?action=install-recommended", url.Values{"_wpnonce": {formNonce(t, body[i:])}})
	if code != http.StatusFound || !strings.Contains(loc, "recommended=Manager+Folio") {
		t.Fatalf("install recommended: %d %s", code, loc)
	}
	if themeBySlug("mgr-folio") == nil {
		t.Error("Manager Folio is not installed")
	}
	_, body, _ = get(t, c, base+"/wp-admin/theme-install")
	if strings.Contains(body, "Install recommended themes") {
		t.Error("the button stays with nothing left to install")
	}

	// No Manager, or one that does not answer: the Library says why it is empty.
	srv.cfg.HubURL = "off"
	_, body, _ = get(t, c, base+"/wp-admin/theme-install")
	if !strings.Contains(body, "no Pursae Manager is configured") {
		t.Error("no notice without a Manager")
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	srv.cfg.HubURL = down.URL
	srv.hubDropCaches(context.Background())
	_, body, _ = get(t, c, base+"/wp-admin/theme-install")
	if !strings.Contains(body, "Pursae Manager is not reachable") {
		t.Error("no notice for an unreachable Manager")
	}
}
