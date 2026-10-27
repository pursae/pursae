package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Theme packages (docs/theme-package.md): a theme installed at run time is a directory
// <data>/themes/<slug>/ with theme.json, templates/ and assets/, and no Go code. theme.json holds
// everything a bundled theme registers from Go (functions.php): enqueued styles and scripts, widget
// areas, menu locations, editor styles, the palette, theme supports and declarative Customizer
// sections. This file parses and validates the manifest and turns it into a *Theme whose hooks
// print the assets, apply the Customizer settings and feed the templates; theme_install.go loads,
// installs and deletes packages.

// themeManifest is theme.json.
type themeManifest struct {
	Slug         string   `json:"slug"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Author       string   `json:"author"`
	AuthorURL    string   `json:"author_url"`
	ThemeURL     string   `json:"theme_url"`
	Tags         []string `json:"tags"`
	RequiresCore string   `json:"requires_core"`
	Screenshot   string   `json:"screenshot"`

	WidgetAreas   []themeManifestArea     `json:"widget_areas"`
	MenuLocations []themeManifestLocation `json:"menu_locations"`

	Styles       []themeAsset `json:"styles"`
	Scripts      []themeAsset `json:"scripts"`
	EditorStyles []string     `json:"editor_styles"`
	PreloadFonts []string     `json:"preload_fonts"`

	Palette  []themePaletteColor `json:"palette"`
	Supports map[string]any      `json:"supports"`

	Customizer struct {
		Sections []themeCzSection `json:"sections"`
	} `json:"customizer"`
}

type themeManifestArea struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type themeManifestLocation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// themeAsset is one wp_enqueue_style() / wp_enqueue_script() call.
type themeAsset struct {
	Handle   string   `json:"handle"`
	Src      string   `json:"src"`
	Deps     []string `json:"deps"`
	Media    string   `json:"media"`
	InFooter bool     `json:"in_footer"`
}

type themePaletteColor struct {
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Color string `json:"color"`
}

// themeCzSection is a declarative Customizer section with its settings (each setting is also its
// control, like a customize_register() that adds one control per setting).
type themeCzSection struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Priority    int               `json:"priority"`
	Settings    []themeCzSettings `json:"settings"`
}

type themeCzSettings struct {
	ID          string         `json:"id"`
	Label       string         `json:"label"`
	Description string         `json:"description"`
	Type        string         `json:"type"`
	Default     any            `json:"default"`
	Transport   string         `json:"transport"`
	CSSVar      string         `json:"css_var"`
	BodyClass   string         `json:"body_class"`
	Choices     themeCzChoices `json:"choices"`
	Min         *int           `json:"min"`
	Max         *int           `json:"max"`
}

// themeCzChoices are a select setting's choices: an ordered array of {"value","label"} objects, or
// an object {"value": "Label"} (sorted by value, since JSON objects carry no order).
type themeCzChoices [][2]string

func (ch *themeCzChoices) UnmarshalJSON(b []byte) error {
	var list []struct {
		Value string `json:"value"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(b, &list); err == nil {
		out := make(themeCzChoices, 0, len(list))
		for _, c := range list {
			out = append(out, [2]string{c.Value, c.Label})
		}
		*ch = out
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return errors.New("choices must be an array of {value, label} objects or a {value: label} object")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(themeCzChoices, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, m[k]})
	}
	*ch = out
	return nil
}

var (
	themeSlugRe    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	themeHandleRe  = regexp.MustCompile(`^[a-z0-9]+([_-][a-z0-9]+)*$`)
	themeSemverRe  = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)
	themeCSSVarRe  = regexp.MustCompile(`^--[A-Za-z0-9_-]+$`)
	themeClassRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*(\{value\}[A-Za-z0-9_-]*)?$`)
	themeClassBad  = regexp.MustCompile(`[^a-z0-9_-]+`)
	themeCSSTextRe = regexp.MustCompile(`[{}<>;\\\r\n]`)
)

// themeCzTypes are the Customizer setting types a package may declare.
var themeCzTypes = map[string]bool{"color": true, "text": true, "textarea": true, "number": true, "checkbox": true, "select": true, "image": true}

const themeNumberMax = 1000000

// manifestError collects every problem of a theme.json so the author sees them all at once.
type manifestError struct{ problems []string }

func (e *manifestError) Error() string { return "theme.json: " + strings.Join(e.problems, "; ") }

// parseThemeManifest decodes and validates theme.json.
func parseThemeManifest(b []byte) (*themeManifest, error) {
	var m themeManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("theme.json: %w", err)
	}
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if !themeSlugRe.MatchString(m.Slug) || len(m.Slug) < 2 || len(m.Slug) > 64 {
		bad("slug %q must match ^[a-z0-9]+(-[a-z0-9]+)*$ and be 2-64 characters", m.Slug)
	}
	if strings.TrimSpace(m.Name) == "" {
		bad("name is required")
	}
	if !themeSemverRe.MatchString(m.Version) {
		bad("version %q is not a semantic version (MAJOR.MINOR.PATCH)", m.Version)
	}
	if m.RequiresCore != "" {
		if !themeSemverRe.MatchString(m.RequiresCore) {
			bad("requires_core %q is not a semantic version", m.RequiresCore)
		} else if semverLess(Version, m.RequiresCore) {
			bad("requires WordPress Go %s or newer (this is %s)", m.RequiresCore, Version)
		}
	}
	for _, u := range []struct{ field, v string }{{"author_url", m.AuthorURL}, {"theme_url", m.ThemeURL}} {
		if u.v != "" && !isHTTPURL(u.v) {
			bad("%s %q must be an http(s) URL", u.field, u.v)
		}
	}
	if m.Screenshot == "" {
		m.Screenshot = "assets/screenshot.png"
	}
	if !themeAssetPathOK(m.Screenshot) {
		bad("screenshot %q must be a path under assets/", m.Screenshot)
	}

	ids := map[string]bool{}
	for i, a := range m.WidgetAreas {
		if !themeHandleRe.MatchString(a.ID) {
			bad("widget_areas[%d].id %q must match ^[a-z0-9]+([_-][a-z0-9]+)*$", i, a.ID)
		} else if ids["area:"+a.ID] {
			bad("widget_areas[%d].id %q is listed twice", i, a.ID)
		}
		ids["area:"+a.ID] = true
		if strings.TrimSpace(a.Name) == "" {
			bad("widget_areas[%d].name is required", i)
		}
	}
	for i, l := range m.MenuLocations {
		if !themeHandleRe.MatchString(l.ID) {
			bad("menu_locations[%d].id %q must match ^[a-z0-9]+([_-][a-z0-9]+)*$", i, l.ID)
		} else if ids["menu:"+l.ID] {
			bad("menu_locations[%d].id %q is listed twice", i, l.ID)
		}
		ids["menu:"+l.ID] = true
		if strings.TrimSpace(l.Name) == "" {
			bad("menu_locations[%d].name is required", i)
		}
	}

	checkAssets := func(field string, list []themeAsset) {
		handles := map[string]bool{}
		for i, a := range list {
			if !themeHandleRe.MatchString(a.Handle) {
				bad("%s[%d].handle %q must match ^[a-z0-9]+([_-][a-z0-9]+)*$", field, i, a.Handle)
			} else if handles[a.Handle] {
				bad("%s[%d].handle %q is listed twice", field, i, a.Handle)
			}
			handles[a.Handle] = true
			if !isHTTPURL(a.Src) && !themeAssetPathOK(a.Src) {
				bad("%s[%d].src %q must be a path under assets/ or an http(s) URL", field, i, a.Src)
			}
			for _, d := range a.Deps {
				if !themeHandleRe.MatchString(d) {
					bad("%s[%d].deps: %q is not a handle", field, i, d)
				}
			}
			if a.Media != "" && strings.ContainsAny(a.Media, "<>\"'") {
				bad("%s[%d].media %q is not a media query", field, i, a.Media)
			}
		}
		if _, err := orderThemeAssets(list); err != nil {
			bad("%s: %v", field, err)
		}
	}
	checkAssets("styles", m.Styles)
	checkAssets("scripts", m.Scripts)
	for i, f := range m.EditorStyles {
		if !themeAssetPathOK(f) || !strings.HasSuffix(f, ".css") {
			bad("editor_styles[%d] %q must be a .css file under assets/", i, f)
		}
	}
	for i, f := range m.PreloadFonts {
		if !themeAssetPathOK(f) || !strings.HasSuffix(f, ".woff2") {
			bad("preload_fonts[%d] %q must be a .woff2 file under assets/", i, f)
		}
	}
	for i, p := range m.Palette {
		if strings.TrimSpace(p.Name) == "" || !themeHandleRe.MatchString(p.Slug) {
			bad("palette[%d] needs a name and a slug", i)
		}
		if sanitizeHex(p.Color) == "" {
			bad("palette[%d].color %q is not a hex color", i, p.Color)
		}
	}

	settings := map[string]bool{}
	sections := map[string]bool{}
	for i, sec := range m.Customizer.Sections {
		if !themeHandleRe.MatchString(sec.ID) {
			bad("customizer.sections[%d].id %q must match ^[a-z0-9]+([_-][a-z0-9]+)*$", i, sec.ID)
		} else if sections[sec.ID] {
			bad("customizer.sections[%d].id %q is listed twice", i, sec.ID)
		}
		sections[sec.ID] = true
		if strings.TrimSpace(sec.Title) == "" {
			bad("customizer.sections[%d].title is required", i)
		}
		for j, st := range sec.Settings {
			at := fmt.Sprintf("customizer.sections[%d].settings[%d]", i, j)
			if !themeHandleRe.MatchString(st.ID) {
				bad("%s.id %q must match ^[a-z0-9]+([_-][a-z0-9]+)*$", at, st.ID)
			} else if settings[st.ID] || st.ID == "custom_logo" || st.ID == "custom_css" {
				bad("%s.id %q is used twice (or reserved)", at, st.ID)
			}
			settings[st.ID] = true
			if strings.TrimSpace(st.Label) == "" {
				bad("%s.label is required", at)
			}
			if !themeCzTypes[st.Type] {
				bad("%s.type %q is not one of color, text, textarea, number, checkbox, select, image", at, st.Type)
				continue
			}
			if st.Transport != "" && st.Transport != "refresh" && st.Transport != "postMessage" {
				bad("%s.transport %q must be refresh or postMessage", at, st.Transport)
			}
			if st.Transport == "postMessage" && st.Type == "image" {
				bad("%s: image settings cannot use transport postMessage", at)
			}
			if st.CSSVar != "" && !themeCSSVarRe.MatchString(st.CSSVar) {
				bad("%s.css_var %q must look like --name", at, st.CSSVar)
			}
			if st.BodyClass != "" && !themeClassRe.MatchString(st.BodyClass) {
				bad("%s.body_class %q must be a class name, optionally with {value}", at, st.BodyClass)
			}
			if st.Type == "select" && len(st.Choices) == 0 {
				bad("%s: a select setting needs choices", at)
			}
			if st.Type != "select" && len(st.Choices) > 0 {
				bad("%s: only select settings have choices", at)
			}
			for k, c := range st.Choices {
				if c[0] == "" {
					bad("%s.choices[%d] has an empty value", at, k)
				}
			}
			if st.Min != nil && st.Max != nil && *st.Min > *st.Max {
				bad("%s: min is greater than max", at)
			}
			if _, err := themeCzDefault(st); err != nil {
				bad("%s.default: %v", at, err)
			}
		}
	}
	if len(problems) > 0 {
		return nil, &manifestError{problems}
	}
	return &m, nil
}

// themeCzDefault checks a setting's default against its type and returns it as the Customizer's string form.
func themeCzDefault(st themeCzSettings) (string, error) {
	switch st.Type {
	case "checkbox":
		switch v := st.Default.(type) {
		case nil:
			return "0", nil
		case bool:
			if v {
				return "1", nil
			}
			return "0", nil
		}
		return "", errors.New("must be true or false")
	case "number":
		switch v := st.Default.(type) {
		case nil:
			return strconv.Itoa(themeCzMin(st)), nil
		case float64:
			if v != float64(int(v)) {
				return "", errors.New("must be an integer")
			}
			n := int(v)
			if n < themeCzMin(st) || n > themeCzMax(st) {
				return "", errors.New("is outside min and max")
			}
			return strconv.Itoa(n), nil
		}
		return "", errors.New("must be an integer")
	case "color":
		switch v := st.Default.(type) {
		case nil:
			return "", nil
		case string:
			if v == "" {
				return "", nil
			}
			if c := sanitizeHex(v); c != "" {
				return c, nil
			}
		}
		return "", errors.New("must be a hex color")
	case "select":
		s, ok := st.Default.(string)
		if st.Default == nil {
			if len(st.Choices) > 0 {
				return st.Choices[0][0], nil
			}
			return "", nil
		}
		if !ok {
			return "", errors.New("must be one of the choices")
		}
		for _, c := range st.Choices {
			if c[0] == s {
				return s, nil
			}
		}
		return "", errors.New("must be one of the choices")
	case "image":
		switch v := st.Default.(type) {
		case nil:
			return "", nil
		case float64:
			return strconv.Itoa(int(v)), nil
		}
		return "", errors.New("must be an attachment id or omitted")
	default: // text, textarea
		switch v := st.Default.(type) {
		case nil:
			return "", nil
		case string:
			return v, nil
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64), nil
		}
		return "", errors.New("must be a string")
	}
}

func themeCzMin(st themeCzSettings) int {
	if st.Min != nil {
		return *st.Min
	}
	return 0
}

func themeCzMax(st themeCzSettings) int {
	if st.Max != nil {
		return *st.Max
	}
	return themeNumberMax
}

// themeAssetPathOK reports whether a theme-relative path points under assets/ without escaping it.
func themeAssetPathOK(p string) bool {
	return strings.HasPrefix(p, "assets/") && len(p) > len("assets/") && fs.ValidPath(p) && !strings.HasSuffix(p, "/")
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// semverLess reports whether version a is lower than b (pre-releases sort before the release).
func semverLess(a, b string) bool {
	ma, mb := themeSemverRe.FindStringSubmatch(a), themeSemverRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return a < b
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x < y
		}
	}
	if (ma[4] == "") != (mb[4] == "") {
		return ma[4] != ""
	}
	return ma[4] < mb[4]
}

// orderThemeAssets sorts styles or scripts so every asset follows the theme handles it depends on
// (wp_enqueue_* order). Core handles (wp-block-library, jquery, …) are printed by core before the
// theme's assets, so they need no ordering here. Declared order is kept where dependencies allow.
func orderThemeAssets(list []themeAsset) ([]themeAsset, error) {
	own := map[string]int{}
	for i, a := range list {
		own[a.Handle] = i
	}
	var out []themeAsset
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(i int, trail []string) error
	visit = func(i int, trail []string) error {
		h := list[i].Handle
		switch state[h] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("dependency cycle %s -> %s", strings.Join(trail, " -> "), h)
		}
		state[h] = 1
		for _, d := range list[i].Deps {
			if j, ok := own[d]; ok {
				if err := visit(j, append(trail, h)); err != nil {
					return err
				}
			}
		}
		state[h] = 2
		out = append(out, list[i])
		return nil
	}
	for i := range list {
		if err := visit(i, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Building the *Theme -------------------------------------------------------------------

// themePackage is what a package theme carries besides the generic Theme fields.
type themePackage struct {
	Manifest *themeManifest
	Dir      string // <data>/themes/<slug>
	styles   []themeAsset
	scripts  []themeAsset
	settings []themeCzSettings // every Customizer setting, in section order
	spec     *czThemeSpec
}

// newPackageTheme builds the Theme for a validated manifest living in dir.
func newPackageTheme(m *themeManifest, dir string) *Theme {
	pkg := &themePackage{Manifest: m, Dir: dir}
	pkg.styles, _ = orderThemeAssets(m.Styles)
	pkg.scripts, _ = orderThemeAssets(m.Scripts)
	t := &Theme{
		Slug: m.Slug, Name: m.Name, Version: m.Version, Description: m.Description,
		Author: m.Author, AuthorURI: m.AuthorURL, ThemeURI: m.ThemeURL, Tags: m.Tags,
		Screenshot: strings.TrimPrefix(m.Screenshot, "assets/"),
		Supports:   map[string]any{},
		Package:    pkg,
	}
	for _, a := range m.WidgetAreas {
		t.WidgetAreas = append(t.WidgetAreas, WidgetArea{ID: a.ID, Name: a.Name, Description: a.Description,
			BeforeWidget: `<section id="%1$s" class="widget %2$s">`, AfterWidget: `</section>`,
			BeforeTitle: `<h2 class="widget-title">`, AfterTitle: `</h2>`})
	}
	for _, l := range m.MenuLocations {
		t.MenuLocations = append(t.MenuLocations, MenuLocation{Slug: l.ID, Name: l.Name})
	}
	for _, f := range m.EditorStyles {
		t.EditorStyles = append(t.EditorStyles, strings.TrimPrefix(f, "assets/"))
	}
	for _, f := range m.PreloadFonts {
		t.PreloadFonts = append(t.PreloadFonts, strings.TrimPrefix(f, "assets/"))
	}
	for _, p := range m.Palette {
		t.Palette = append(t.Palette, map[string]string{"name": p.Name, "slug": p.Slug, "color": sanitizeHex(p.Color)})
	}
	for k, v := range m.Supports {
		t.Supports[k] = v
	}
	if len(t.Palette) > 0 {
		t.Supports["editor-color-palette"] = t.Palette
	}
	if len(t.EditorStyles) > 0 {
		t.Supports["editor-styles"] = true
	}
	if _, ok := t.Supports["custom-logo"]; !ok {
		t.Supports["custom-logo"] = true
	}

	// The Customizer: one section per declared section, one control per setting.
	spec := &czThemeSpec{PostMessage: []string{"blogname", "blogdescription"}}
	sections := append([]themeCzSection{}, m.Customizer.Sections...)
	sort.SliceStable(sections, func(i, j int) bool { return sections[i].Priority < sections[j].Priority })
	for _, sec := range sections {
		spec.Sections = append(spec.Sections, czSection{ID: sec.ID, Title: sec.Title, Description: sec.Description})
		for _, st := range sec.Settings {
			pkg.settings = append(pkg.settings, st)
			def, _ := themeCzDefault(st)
			ctl := czControl{ID: st.ID, Label: st.Label, Description: st.Description, Type: st.Type, Default: def, Section: sec.ID,
				Choices: [][2]string(st.Choices), Min: themeCzMin(st), Max: themeCzMax(st)}
			spec.Controls = append(spec.Controls, ctl)
			if st.Transport == "postMessage" {
				spec.PostMessage = append(spec.PostMessage, "mod:"+st.ID)
			}
		}
	}
	pkg.spec = spec

	t.Head = pkg.head
	t.Footer = pkg.footer
	t.Prepare = pkg.prepare
	t.BodyClasses = pkg.bodyClasses
	return t
}

// assetURL is the URL of a style or script: absolute URLs as they are, theme paths under the theme URI
// (assets/ is the theme's web root) with the version as cache buster.
func (pkg *themePackage) assetURL(t *Theme, src string) string {
	if isHTTPURL(src) {
		return src
	}
	return t.URI() + "/" + strings.TrimPrefix(src, "assets/") + "?ver=" + url.QueryEscape(t.Version)
}

func (pkg *themePackage) head(s *Server, c *gin.Context, v *siteView) string {
	var sb strings.Builder
	for _, st := range pkg.styles {
		media := st.Media
		if media == "" {
			media = "all"
		}
		fmt.Fprintf(&sb, "<link rel='stylesheet' id='%s-css' href='%s' media='%s' />\n", st.Handle, html.EscapeString(pkg.assetURL(v.Theme, st.Src)), html.EscapeString(media))
	}
	sb.WriteString(inlineStyle(v.Theme.Slug+"-vars", pkg.cssVars(v)))
	for _, sc := range pkg.scripts {
		if !sc.InFooter {
			fmt.Fprintf(&sb, "<script src=\"%s\" id=\"%s-js\"></script>\n", html.EscapeString(pkg.assetURL(v.Theme, sc.Src)), sc.Handle)
		}
	}
	return sb.String()
}

func (pkg *themePackage) footer(s *Server, c *gin.Context, v *siteView) string {
	var sb strings.Builder
	for _, sc := range pkg.scripts {
		if sc.InFooter {
			fmt.Fprintf(&sb, "<script src=\"%s\" id=\"%s-js\"></script>\n", html.EscapeString(pkg.assetURL(v.Theme, sc.Src)), sc.Handle)
		}
	}
	if v.Customize != nil {
		// Live preview of postMessage settings: the generic script applies css_var and body_class changes.
		cfg := map[string]any{}
		for _, st := range pkg.settings {
			if st.Transport != "postMessage" {
				continue
			}
			def, _ := themeCzDefault(st)
			cfg["mod:"+st.ID] = map[string]any{"type": st.Type, "css_var": st.CSSVar, "body_class": st.BodyClass, "default": def}
		}
		b, _ := json.Marshal(map[string]any{"settings": cfg})
		sb.WriteString("<script id=\"wpgo-theme-preview-js-extra\">var wpgoThemePreview = " + strings.ReplaceAll(string(b), "<", "\\u003c") + ";</script>\n")
		sb.WriteString("<script src=\"/wp-includes/js/wpgo-theme-preview.js?ver=" + Version + "\" id=\"wpgo-theme-preview-js\" defer></script>\n")
	}
	return sb.String()
}

// themeSettings is the view data of a package theme in v.Data["Settings"]: every Customizer setting
// with the theme mod applied and typed (bool, int or string; image settings hold the image URL).
func (pkg *themePackage) prepare(s *Server, c *gin.Context, v *siteView) {
	settings := map[string]any{}
	for _, st := range pkg.settings {
		settings[st.ID] = pkg.value(v.ctx, s, v.ThemeMods, st)
	}
	v.Data["Settings"] = settings
	nav := map[string]template.HTML{}
	for _, loc := range v.Theme.MenuLocations {
		if items := v.Menus[loc.Slug]; len(items) > 0 {
			nav[loc.Slug] = renderMenu(items, "menu-"+loc.Slug+"-container", loc.Slug+"-menu", "", "")
		}
	}
	v.Data["Nav"] = nav
}

// value is get_theme_mod() with the setting's default and type applied.
func (pkg *themePackage) value(ctx context.Context, s *Server, mods map[string]any, st themeCzSettings) any {
	def, _ := themeCzDefault(st)
	switch st.Type {
	case "checkbox":
		return modBool(mods, st.ID, def == "1")
	case "number":
		n, _ := strconv.Atoi(def)
		return min(max(modInt(mods, st.ID, n), themeCzMin(st)), themeCzMax(st))
	case "color":
		if c := sanitizeHex(modString(mods, st.ID, "")); c != "" {
			return c
		}
		return def
	case "select":
		v := modString(mods, st.ID, def)
		for _, ch := range st.Choices {
			if ch[0] == v {
				return v
			}
		}
		return def
	case "image":
		id := int64(modInt(mods, st.ID, 0))
		if id <= 0 {
			id, _ = strconv.ParseInt(def, 10, 64)
		}
		if id > 0 && s != nil {
			if a, err := s.store.GetAttachment(ctx, id); err == nil {
				return s.uploadURL(ctx, a.FilePath)
			}
		}
		return ""
	}
	return modString(mods, st.ID, def)
}

// cssText is a setting value as it may appear inside a declaration block.
func cssText(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int:
		return strconv.Itoa(x)
	}
	return themeCSSTextRe.ReplaceAllString(fmt.Sprint(v), "")
}

// cssVars prints the css_var settings as custom properties on :root.
func (pkg *themePackage) cssVars(v *siteView) string {
	settings, _ := v.Data["Settings"].(map[string]any)
	var sb strings.Builder
	for _, st := range pkg.settings {
		if st.CSSVar == "" {
			continue
		}
		val := cssText(settings[st.ID])
		if val == "" {
			continue
		}
		sb.WriteString(st.CSSVar + ":" + val + ";")
	}
	if sb.Len() == 0 {
		return ""
	}
	return ":root{" + sb.String() + "}"
}

// bodyClasses applies the body_class settings: a plain class when the value is truthy, a
// "{value}" pattern filled with the value.
func (pkg *themePackage) bodyClasses(s *Server, v *siteView) []string {
	settings, _ := v.Data["Settings"].(map[string]any)
	var out []string
	if v.Template == "single" || v.Template == "page" {
		out = append(out, "singular")
	} else {
		out = append(out, "hfeed")
	}
	for _, st := range pkg.settings {
		if st.BodyClass == "" {
			continue
		}
		val := settings[st.ID]
		if strings.Contains(st.BodyClass, "{value}") {
			if text := themeClassBad.ReplaceAllString(strings.ToLower(cssText(val)), "-"); text != "" {
				out = append(out, strings.ReplaceAll(st.BodyClass, "{value}", text))
			}
			continue
		}
		if themeTruthy(val) {
			out = append(out, st.BodyClass)
		}
	}
	return out
}

func themeTruthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int:
		return x != 0
	case string:
		return x != "" && x != "0" && x != "false"
	}
	return v != nil
}

// Templates --------------------------------------------------------------------------------

// themeTemplateCandidates is the template hierarchy of a package theme for the main query.
func themeTemplateCandidates(q *siteQuery) []string {
	switch q.Kind {
	case "front-page":
		return []string{"front-page", "page", "singular", "index"}
	case "home":
		return []string{"home", "index"}
	case "single":
		if q.Post != nil && q.Post.Format != "" && q.Post.Format != "standard" {
			return []string{"single-" + q.Post.Format, "single", "singular", "index"}
		}
		return []string{"single", "singular", "index"}
	case "page":
		return []string{"page", "singular", "index"}
	case "category":
		if q.Term != nil {
			return []string{"category-" + q.Term.Slug, "category", "archive", "index"}
		}
		return []string{"category", "archive", "index"}
	case "tag":
		if q.Term != nil {
			return []string{"tag-" + q.Term.Slug, "tag", "archive", "index"}
		}
		return []string{"tag", "archive", "index"}
	case "author":
		return []string{"author", "archive", "index"}
	case "date":
		return []string{"date", "archive", "index"}
	case "search":
		return []string{"search", "index"}
	case "404":
		return []string{"404", "index"}
	}
	return []string{"index"}
}

// parsePackageTemplates parses the templates of a package theme: every templates/**/*.html file is a
// template named by its path without the extension ("index", "header", "content-page", …).
func parsePackageTemplates(fsys fs.FS, dir string, funcs template.FuncMap) (*template.Template, error) {
	root := template.New("").Funcs(funcs)
	n := 0
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(p, dir+"/"), ".html")
		if _, err := root.New(name).Parse(string(b)); err != nil {
			return err
		}
		n++
		return nil
	})
	if err != nil {
		return nil, err
	}
	if root.Lookup("index") == nil {
		return nil, errors.New("templates/index.html is missing")
	}
	return root, nil
}

// renderSiteTemplate renders the main query with the theme's template set: "theme/<kind>" for the
// bundled themes, the first template of the hierarchy a package theme provides.
func (s *Server) renderSiteTemplate(c *gin.Context, v *siteView, status int) {
	set := s.tmpl.getTheme(true, v.Theme.Slug)
	name := "theme/" + v.Template
	if v.Theme.Package != nil {
		name = "index"
		for _, cand := range themeTemplateCandidates(v.Q) {
			if set.Lookup(cand) != nil {
				name = cand
				break
			}
		}
	}
	s.renderTemplate(c, status, set, name, v)
}

// themeTemplateFuncs are template functions any theme may use; package themes rely on them.
func themeTemplateFuncs() template.FuncMap {
	nav := func(v *siteView, loc string) template.HTML {
		if m, ok := v.Data["Nav"].(map[string]template.HTML); ok {
			if h, ok := m[loc]; ok {
				return h
			}
		}
		if items := v.Menus[loc]; len(items) > 0 {
			return renderMenu(items, "menu-"+loc+"-container", loc+"-menu", "", "")
		}
		return ""
	}
	return template.FuncMap{
		"nav_menu":     nav,
		"has_nav_menu": func(v *siteView, loc string) bool { return nav(v, loc) != "" },
		"dynamic_sidebar": func(v *siteView, id string) template.HTML {
			return v.Areas[id]
		},
		"is_active_sidebar": func(v *siteView, id string) bool { return v.Areas[id] != "" },
		"theme_mod": func(v *siteView, id string) any {
			if m, ok := v.Data["Settings"].(map[string]any); ok {
				if val, ok := m[id]; ok {
					return val
				}
			}
			return v.ThemeMods[id]
		},
	}
}

// Registry --------------------------------------------------------------------------------

// unregisterTheme removes a theme (and its Customizer spec) from the registry.
func unregisterTheme(slug string) {
	themesMu.Lock()
	delete(themes, slug)
	themesMu.Unlock()
	delete(czSpecs, slug)
}

// packageThemes lists the registered package themes.
func packageThemes() []*Theme {
	var out []*Theme
	for _, t := range allThemes() {
		if t.Package != nil {
			out = append(out, t)
		}
	}
	return out
}

// loadThemePackage reads and validates <dir>/theme.json, whose slug must be slug (the name of the
// theme's directory), and checks that the files it references exist; it returns the Theme without
// registering it.
func loadThemePackage(dir, slug string) (*Theme, error) {
	b, err := os.ReadFile(filepath.Join(dir, "theme.json"))
	if err != nil {
		return nil, err
	}
	m, err := parseThemeManifest(b)
	if err != nil {
		return nil, err
	}
	if m.Slug != slug {
		return nil, fmt.Errorf("theme.json: slug %q does not match the directory name %q", m.Slug, slug)
	}
	if t := themeBySlug(m.Slug); t != nil && t.Package == nil {
		return nil, fmt.Errorf("slug %q belongs to a bundled theme", m.Slug)
	}
	var missing []string
	need := []string{"templates/index.html"}
	for _, a := range append(append([]themeAsset{}, m.Styles...), m.Scripts...) {
		if !isHTTPURL(a.Src) {
			need = append(need, a.Src)
		}
	}
	need = append(need, m.EditorStyles...)
	need = append(need, m.PreloadFonts...)
	for _, f := range need {
		if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path.Clean(f)))); err != nil || st.IsDir() {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("files referenced by theme.json are missing: %s", strings.Join(missing, ", "))
	}
	return newPackageTheme(m, dir), nil
}

// registerThemePackage puts a loaded package theme in the registry.
func registerThemePackage(t *Theme) {
	registerTheme(t)
	czSpecs[t.Slug] = t.Package.spec
}
