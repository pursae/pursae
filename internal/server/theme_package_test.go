package server

import (
	"archive/zip"
	"bytes"
	"html"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const starterDir = "../../examples/themes/starter"

// starterManifest is the example package's theme.json with a few fields replaced.
func starterManifest(t *testing.T, edits map[string]string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(starterDir, "theme.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for from, to := range edits {
		if !strings.Contains(s, from) {
			t.Fatalf("theme.json does not contain %q", from)
		}
		s = strings.Replace(s, from, to, 1)
	}
	return s
}

func TestThemeManifestValidation(t *testing.T) {
	if _, err := parseThemeManifest([]byte(starterManifest(t, nil))); err != nil {
		t.Fatalf("the example manifest is rejected: %v", err)
	}
	cases := map[string]struct {
		edits map[string]string
		want  string
	}{
		"bad slug":           {map[string]string{`"slug": "starter"`: `"slug": "Starter Theme"`}, "slug"},
		"short slug":         {map[string]string{`"slug": "starter"`: `"slug": "s"`}, "slug"},
		"no name":            {map[string]string{`"name": "Starter"`: `"name": " "`}, "name is required"},
		"bad version":        {map[string]string{`"version": "1.0.0"`: `"version": "1.0"`}, "semantic version"},
		"requires core":      {map[string]string{`"requires_core": "0.1.0"`: `"requires_core": "9.0.0"`}, "requires WordPress Go 9.0.0"},
		"src outside assets": {map[string]string{`"src": "assets/style.css"`: `"src": "../style.css"`}, "styles[0].src"},
		"bad handle":         {map[string]string{`"handle": "starter-style"`: `"handle": "Starter Style"`}, "styles[0].handle"},
		"dependency cycle":   {map[string]string{`"handle": "starter-style", "src": "assets/style.css", "deps": ["wp-block-library"]`: `"handle": "starter-style", "src": "assets/style.css", "deps": ["starter-style"]`}, "cycle"},
		"editor style type":  {map[string]string{`"editor_styles": ["assets/editor-style.css"]`: `"editor_styles": ["assets/editor-style.js"]`}, "editor_styles[0]"},
		"palette color":      {map[string]string{`"color": "#2271b1"}`: `"color": "blue"}`}, "palette[0].color"},
		"setting type":       {map[string]string{`"type": "color", "default": "#2271b1"`: `"type": "rainbow", "default": "#2271b1"`}, "not one of"},
		"color default":      {map[string]string{`"default": "#2271b1"`: `"default": "red"`}, "hex color"},
		"checkbox default":   {map[string]string{`"type": "checkbox", "default": true`: `"type": "checkbox", "default": "yes"`}, "true or false"},
		"select default":     {map[string]string{`"type": "select", "default": "normal"`: `"type": "select", "default": "huge"`}, "one of the choices"},
		"css var":            {map[string]string{`"css_var": "--starter-accent"`: `"css_var": "accent"`}, "css_var"},
		"body class":         {map[string]string{`"body_class": "has-sidebar"`: `"body_class": "has sidebar"`}, "body_class"},
		"transport":          {map[string]string{`"transport": "refresh"`: `"transport": "ajax"`}, "transport"},
		"duplicate setting":  {map[string]string{`"id": "footer_text"`: `"id": "accent"`}, "used twice"},
		"duplicate area":     {map[string]string{`{"id": "primary", "name": "Primary Menu"}`: `{"id": "primary", "name": "Primary Menu"}, {"id": "primary", "name": "Again"}`}, "listed twice"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseThemeManifest([]byte(starterManifest(t, tc.edits)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	if _, err := parseThemeManifest([]byte(`{"slug": "x"`)); err == nil {
		t.Fatal("broken JSON accepted")
	}

	// Choices as an object are accepted, sorted by value; min/max default to 0 and 1000000.
	m, err := parseThemeManifest([]byte(starterManifest(t, map[string]string{
		`"choices": [{"value": "narrow", "label": "Narrow (640px)"}, {"value": "normal", "label": "Normal (720px)"}, {"value": "wide", "label": "Wide (860px)"}]`: `"choices": {"wide": "Wide", "narrow": "Narrow", "normal": "Normal"}`,
		`{"id": "footer_text", "label": "Footer text", "description": "Replaces the copyright line.", "type": "text", "default": "", "transport": "refresh"}`:     `{"id": "columns", "label": "Columns", "type": "number", "default": 2}`,
	})))
	if err != nil {
		t.Fatal(err)
	}
	sel := m.Customizer.Sections[0].Settings[2]
	if got := [][2]string(sel.Choices); len(got) != 3 || got[0][0] != "narrow" || got[2][0] != "wide" {
		t.Fatalf("choices object: %v", got)
	}
	th := newPackageTheme(m, "/tmp/starter")
	if ctl := th.Package.spec.control("columns"); ctl == nil || ctl.Min != 0 || ctl.Max != themeNumberMax || ctl.Default != "2" {
		t.Fatalf("number control: %+v", ctl)
	}
	if ctl := th.Package.spec.control("show_sidebar"); ctl == nil || ctl.Default != "1" || ctl.Section != "starter_options" {
		t.Fatalf("checkbox control: %+v", ctl)
	}
	if pm := strings.Join(th.Package.spec.PostMessage, ","); !strings.Contains(pm, "mod:accent") || strings.Contains(pm, "mod:columns") {
		t.Fatalf("postMessage settings: %s", pm)
	}
	if th.Screenshot != "screenshot.png" || th.EditorStyles[0] != "editor-style.css" || th.MenuLocations[1].Slug != "footer" || th.WidgetAreas[0].ID != "sidebar-1" {
		t.Fatalf("theme fields: %+v", th)
	}
	if th.Supports["editor-color-palette"] == nil || th.Supports["align-wide"] != true {
		t.Fatalf("supports: %v", th.Supports)
	}

	// Assets are printed in dependency order, declared order otherwise.
	ordered, err := orderThemeAssets([]themeAsset{{Handle: "c", Deps: []string{"b"}}, {Handle: "a", Deps: []string{"jquery"}}, {Handle: "b", Deps: []string{"a"}}})
	if err != nil {
		t.Fatal(err)
	}
	var handles []string
	for _, a := range ordered {
		handles = append(handles, a.Handle)
	}
	if got := strings.Join(handles, ","); got != "a,b,c" {
		t.Fatalf("asset order %s", got)
	}
	if _, err := orderThemeAssets([]themeAsset{{Handle: "a", Deps: []string{"b"}}, {Handle: "b", Deps: []string{"a"}}}); err == nil {
		t.Fatal("cycle not detected")
	}
	for _, c := range [][3]any{{"1.0.0", "1.0.1", true}, {"1.2.0", "1.10.0", true}, {"2.0.0", "1.9.9", false}, {"1.0.0-beta", "1.0.0", true}, {"1.0.0", "1.0.0", false}} {
		if got := semverLess(c[0].(string), c[1].(string)); got != c[2].(bool) {
			t.Fatalf("semverLess(%s, %s) = %v", c[0], c[1], got)
		}
	}

	// The example templates parse with the theme functions and resolve the hierarchy.
	set, err := parsePackageTemplates(os.DirFS(starterDir), "templates", (&templates{s: &Server{}}).funcs())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index", "single", "page", "archive", "search", "404", "header", "footer", "content-page"} {
		if set.Lookup(name) == nil {
			t.Fatalf("template %s missing", name)
		}
	}
	if got := themeTemplateCandidates(&siteQuery{Kind: "category"}); got[0] != "category" || got[len(got)-1] != "index" {
		t.Fatalf("hierarchy %v", got)
	}
}

func TestThemeZipUnpack(t *testing.T) {
	// A single top-level folder is stripped; forbidden files and oversized packages are rejected.
	z := zipFiles(t, map[string][]byte{"starter/theme.json": []byte(starterManifest(t, nil)), "starter/templates/index.html": []byte("hi"), "starter/assets/style.css": []byte("")})
	files, m, err := unpackThemeZip(z)
	if err != nil || m.Slug != "starter" || files[0].Path != "theme.json" {
		t.Fatalf("stripped folder: %v %v", err, files)
	}
	if _, _, err := unpackThemeZip(zipFiles(t, map[string][]byte{"theme.json": []byte(starterManifest(t, nil)), "templates/index.html": []byte("hi"), "assets/run.php": []byte("<?php")})); err == nil || !strings.Contains(err.Error(), "run.php") {
		t.Fatalf("php file: %v", err)
	}
	if _, _, err := unpackThemeZip(zipFiles(t, map[string][]byte{"theme.json": []byte(starterManifest(t, nil)), "assets/style.css": []byte("")})); err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("missing index: %v", err)
	}
	if _, _, err := unpackThemeZip(zipFiles(t, map[string][]byte{"templates/index.html": []byte("hi")})); err == nil || !strings.Contains(err.Error(), "theme.json") {
		t.Fatalf("missing manifest: %v", err)
	}
	if _, _, err := unpackThemeZip(zipFiles(t, map[string][]byte{"theme.json": []byte(starterManifest(t, nil)), "templates/index.html": []byte("hi"), "assets/big.css": bytes.Repeat([]byte("a"), themeFileMaxBytes+1)})); err == nil || !strings.Contains(err.Error(), "big.css") {
		t.Fatalf("oversized file: %v", err)
	}
	if _, _, err := unpackThemeZip([]byte("not a zip")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func zipFiles(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipDir packages a theme directory the way tools/build-themes.sh does (files at the zip root).
func zipDir(t *testing.T, dir string) []byte {
	t.Helper()
	files := map[string][]byte{}
	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(filepath.Join(dir, p))
		files[p] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return zipFiles(t, files)
}

func postZip(t *testing.T, c *http.Client, u, nonce, field, name string, data []byte) (int, string, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("_wpnonce", nonce)
	fw, _ := mw.CreateFormFile(field, name)
	_, _ = fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, u, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header.Get("Location")
}

func TestThemePackageInstallActivateDelete(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WPGO_THEMES_DIR", dir)
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Packages"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK {
		t.Fatalf("install: %d %s", code, body)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	must := func(what string, code, want int, body string) {
		t.Helper()
		if code != want {
			t.Fatalf("%s: %d (want %d) %s", what, code, want, body)
		}
	}

	// Upload Theme: the form carries a nonce; the package lands in <themes dir>/starter.
	code, body, _ := get(t, c, base+"/wp-admin/theme-install?browse=upload")
	must("theme-install", code, http.StatusOK, body)
	if !strings.Contains(body, `name="themezip"`) || strings.Contains(body, "Coming Soon") {
		t.Fatal("upload form missing or demo listings still shown")
	}
	nonce := formNonce(t, body)
	pkg := zipDir(t, starterDir)
	code, body, loc := postZip(t, c, base+"/wp-admin/theme-install", nonce, "themezip", "starter.zip", pkg)
	must("upload", code, http.StatusFound, body)
	if loc != "/wp-admin/themes?installed=starter" {
		t.Fatalf("upload redirect %q", loc)
	}
	if _, err := os.Stat(filepath.Join(dir, "starter", "templates", "index.html")); err != nil {
		t.Fatalf("package not extracted: %v", err)
	}
	if th := themeBySlug("starter"); th == nil || th.Package == nil || th.Version != "1.0.0" {
		t.Fatalf("starter not registered: %+v", th)
	}
	t.Cleanup(func() { unregisterTheme("starter") })

	// Garbage and a package that is not newer are refused with a notice.
	if _, _, loc := postZip(t, c, base+"/wp-admin/theme-install", nonce, "themezip", "x.zip", []byte("nope")); !strings.Contains(loc, "error=") {
		t.Fatalf("garbage upload redirect %q", loc)
	}
	if _, _, loc := postZip(t, c, base+"/wp-admin/theme-install", nonce, "themezip", "starter.zip", pkg); !strings.Contains(loc, "not+newer") {
		t.Fatalf("same version redirect %q", loc)
	}
	if code, _, _ := postZip(t, c, base+"/wp-admin/theme-install", "bad", "themezip", "starter.zip", pkg); code != http.StatusForbidden {
		t.Fatalf("bad nonce: %d", code)
	}

	// Themes screen: listed with its screenshot, a Delete action and the install notice.
	code, body, _ = get(t, c, base+"/wp-admin/themes?installed=starter")
	must("themes", code, http.StatusOK, body)
	for _, want := range []string{`"id":"starter"`, `/wp-content/themes/starter/screenshot.png`, `action=delete`, `Starter 1.0.0 has been installed.`} {
		if !strings.Contains(body, want) {
			t.Fatalf("themes screen lacks %q", want)
		}
	}
	if code, body, _ := get(t, c, base+"/wp-content/themes/starter/screenshot.png"); code != http.StatusOK || !strings.HasPrefix(body, "\x89PNG") {
		t.Fatalf("screenshot: %d", code)
	}
	code, body, _ = get(t, c, base+"/wp-admin/theme-install?browse=uploaded")
	must("uploaded tab", code, http.StatusOK, body)
	if !strings.Contains(body, `data-slug="starter"`) || strings.Contains(body, `data-slug="twentysixteen"`) {
		t.Fatal("uploaded tab lists the wrong themes")
	}

	// REST lists it as a classic theme with the declared supports.
	code, body, _ = get(t, c, base+"/wp-json/wp/v2/themes")
	must("rest themes", code, http.StatusOK, body)
	for _, want := range []string{`"stylesheet":"starter"`, `"is_block_theme":false`, `"align-wide":true`, `"textdomain":"starter"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("rest themes lacks %q", want)
		}
	}

	// Activate it: the front page renders with its stylesheet, the css var and the body classes.
	activate := func(slug string) {
		t.Helper()
		_, body, _ := get(t, c, base+"/wp-admin/themes")
		m := regexp.MustCompile(`href="(/wp-admin/themes\?action=activate&amp;stylesheet=` + slug + `&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no activate link for %s", slug)
		}
		code, body, _ := get(t, c, base+html.UnescapeString(m[1]))
		must("activate "+slug, code, http.StatusFound, body)
	}
	activate("starter")
	if srv.activeTheme().Slug != "starter" {
		t.Fatalf("active theme %s", srv.activeTheme().Slug)
	}
	front := func() string {
		t.Helper()
		code, body, _ := get(t, c, base+"/")
		must("front page", code, http.StatusOK, body)
		return body
	}
	body = front()
	for _, want := range []string{`id='starter-style-css' href='/wp-content/themes/starter/style.css?ver=1.0.0'`, `--starter-accent:#2271b1;`, `has-sidebar`, `starter-width-normal`,
		`wp-theme-starter`, `class="site-title"`, `src="/wp-content/themes/starter/js/navigation.js?ver=1.0.0" id="starter-navigation-js"`, `Proudly powered by`} {
		if !strings.Contains(body, want) {
			t.Fatalf("front page lacks %q", want)
		}
	}
	if code, body, _ := get(t, c, base+"/wp-content/themes/starter/style.css"); code != http.StatusOK || !strings.Contains(body, "Theme Name: Starter") {
		t.Fatalf("stylesheet: %d", code)
	}
	if code, _, _ := get(t, c, base+"/wp-content/themes/starter/../../../theme.json"); code == http.StatusOK {
		t.Fatal("path traversal served")
	}
	if code, _, _ := get(t, c, base+"/wp-content/themes/starter/js/"); code != http.StatusNotFound {
		t.Fatalf("directory listing: %d", code)
	}
	for _, u := range []string{"/?s=hello", "/wpgo-no-such-page/", "/?p=1"} {
		if code, body, _ := get(t, c, base+u); code >= 500 || strings.Contains(body, "Template error") {
			t.Fatalf("%s: %d", u, code)
		}
	}

	// The Customizer shows the declared section; publishing values changes the css var and the body class.
	code, body, _ = get(t, c, base+"/wp-admin/customize")
	must("customize", code, http.StatusOK, body)
	if !strings.Contains(body, `id="accordion-section-starter_options"`) || !strings.Contains(body, `data-setting="mod:accent"`) {
		t.Fatal("customizer section missing")
	}
	czNonce := regexp.MustCompile(`nonce = "([^"]+)"`).FindStringSubmatch(body)
	if czNonce == nil {
		t.Fatal("no customize nonce")
	}
	code, body, _ = post(t, c, base+"/wp-admin/customize", url.Values{"_wpnonce": {czNonce[1]}, "action": {"publish"},
		"customized": {`{"mod:accent":"#ff0000","mod:show_sidebar":"0","mod:content_width":"wide","mod:footer_text":"Made with Go"}`}})
	must("publish", code, http.StatusOK, body)
	body = front()
	for _, want := range []string{`--starter-accent:#ff0000;`, `starter-width-wide`, `Made with Go`} {
		if !strings.Contains(body, want) {
			t.Fatalf("front page after publish lacks %q", want)
		}
	}
	if strings.Contains(body, "has-sidebar") {
		t.Fatal("sidebar class still set")
	}
	// The preview script and its config are printed in a Customizer preview only.
	if strings.Contains(body, "wpgo-theme-preview.js") {
		t.Fatal("preview script printed outside the Customizer")
	}
	uuid := regexp.MustCompile(`uuid = "([^"]+)"`).FindStringSubmatch(func() string { _, b, _ := get(t, c, base+"/wp-admin/customize"); return b }())
	if uuid == nil {
		t.Fatal("no changeset uuid")
	}
	code, body, _ = get(t, c, base+"/?customize_changeset_uuid="+uuid[1])
	must("preview", code, http.StatusOK, body)
	if !strings.Contains(body, "/wp-includes/js/wpgo-theme-preview.js") || !strings.Contains(body, `"mod:accent":{"body_class":"","css_var":"--starter-accent","default":"#2271b1","type":"color"}`) {
		t.Fatal("preview script or config missing in the Customizer preview")
	}
	if code, _, _ := get(t, c, base+"/wp-includes/js/wpgo-theme-preview.js"); code != http.StatusOK {
		t.Fatalf("preview script: %d", code)
	}

	// The Theme File Editor edits the package's own files (no Restore original).
	code, body, _ = get(t, c, base+"/wp-admin/theme-editor?theme=starter&file=style.css")
	must("theme editor", code, http.StatusOK, body)
	if !strings.Contains(body, "Theme Name: Starter") || strings.Contains(body, "action=restore") {
		t.Fatal("theme editor shows the wrong file or offers Restore")
	}

	// Updating: a higher version replaces the directory and the registration.
	newer := map[string][]byte{}
	_ = fs.WalkDir(os.DirFS(starterDir), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			newer[p], _ = os.ReadFile(filepath.Join(starterDir, p))
		}
		return nil
	})
	newer["theme.json"] = []byte(starterManifest(t, map[string]string{`"version": "1.0.0"`: `"version": "1.1.0"`}))
	newer["templates/index.html"] = append([]byte(`<span class="starter-1-1"></span>`+"\n"), newer["templates/index.html"]...) // html/template drops comments
	if code, _, loc := postZip(t, c, base+"/wp-admin/theme-install", nonce, "themezip", "starter.zip", zipFiles(t, newer)); code != http.StatusFound || loc != "/wp-admin/themes?installed=starter" {
		t.Fatalf("update: %d %q", code, loc)
	}
	if th := themeBySlug("starter"); th == nil || th.Version != "1.1.0" {
		t.Fatalf("update not registered: %+v", th)
	}
	if body := front(); !strings.Contains(body, `class="starter-1-1"`) || !strings.Contains(body, "style.css?ver=1.1.0") {
		t.Fatal("updated templates not rendered")
	}

	// A start-up scan finds the package and skips a broken one.
	if err := os.MkdirAll(filepath.Join(dir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "broken", "theme.json"), []byte(`{"slug": "broken", "name": "Broken"}`), 0o644)
	srv.loadThemePackages()
	if th := themeBySlug("starter"); th == nil || th.Version != "1.1.0" {
		t.Fatal("start-up scan lost the package")
	}
	if themeBySlug("broken") != nil {
		t.Fatal("broken package registered")
	}

	// Delete: refused while active, done once another theme is active.
	deleteURL := func() string {
		t.Helper()
		_, body, _ := get(t, c, base+"/wp-admin/themes")
		// The action sits in the _wpThemeSettings JSON, where & is \u0026.
		m := regexp.MustCompile(`action=delete(?:&|\\u0026)stylesheet=starter(?:&|\\u0026)_wpnonce=([a-f0-9]+)`).FindStringSubmatch(body)
		if m == nil {
			return ""
		}
		return "/wp-admin/themes?action=delete&stylesheet=starter&_wpnonce=" + m[1]
	}
	if deleteURL() != "" {
		t.Fatal("the active theme offers Delete")
	}
	if code, _, _ := get(t, c, base+"/wp-admin/themes?action=delete&stylesheet=starter&_wpnonce="+"deadbeef"); code == http.StatusFound {
		t.Fatal("deleting with a bad nonce succeeded")
	}
	activate("twentysixteen")
	del := deleteURL()
	if del == "" {
		t.Fatal("no Delete action for the inactive package")
	}
	code, body, loc = get(t, c, base+del)
	must("delete", code, http.StatusFound, body)
	if loc != "/wp-admin/themes?deleted=true" {
		t.Fatalf("delete redirect %q", loc)
	}
	if _, err := os.Stat(filepath.Join(dir, "starter")); !os.IsNotExist(err) {
		t.Fatalf("directory still there: %v", err)
	}
	if themeBySlug("starter") != nil {
		t.Fatal("still registered")
	}
	if code, _, _ := get(t, c, base+"/wp-content/themes/starter/style.css"); code != http.StatusNotFound {
		t.Fatalf("deleted stylesheet still served: %d", code)
	}
	if _, body, _ := get(t, c, base+"/wp-json/wp/v2/themes"); strings.Contains(body, `"stylesheet":"starter"`) {
		t.Fatal("REST still lists the deleted theme")
	}
	if code, _, _ := get(t, c, base+"/"); code != http.StatusOK {
		t.Fatalf("front page after delete: %d", code)
	}
}
