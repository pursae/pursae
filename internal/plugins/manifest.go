package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"wpgo/internal/wp"
)

// ManifestFile is the name of the manifest inside a plugin package.
const ManifestFile = "plugin.json"

// Manifest is the parsed plugin.json of a plugin package.
//
//	{
//	  "slug": "hello-gopher",
//	  "name": "Hello Gopher",
//	  "version": "1.0.0",
//	  "description": "…",
//	  "author": "…",
//	  "author_uri": "https://…",
//	  "plugin_uri": "https://…",
//	  "requires": "0.1.0",
//	  "license": "GPL-2.0-or-later",
//	  "permissions": ["options:read", "content:filter"],
//	  "hooks": ["the_content"],
//	  "shortcodes": ["gopher"],
//	  "assets": {"style.css": "<sha256 hex>"}
//	}
type Manifest struct {
	// Slug uniquely identifies the plugin: lowercase letters, digits and
	// single dashes, 2–64 characters. It also owns the plugin's hooks.
	Slug string `json:"slug"`
	// Name is the human readable plugin name.
	Name string `json:"name"`
	// Version is the plugin version, semantic versioning (1.2.3[-pre]).
	Version string `json:"version"`
	// Description is a one paragraph summary.
	Description string `json:"description,omitempty"`
	// Author is the author's name.
	Author string `json:"author,omitempty"`
	// AuthorURI is the author's homepage (http or https).
	AuthorURI string `json:"author_uri,omitempty"`
	// PluginURI is the plugin's homepage (http or https).
	PluginURI string `json:"plugin_uri,omitempty"`
	// Requires is the minimum wpgo version the plugin needs (e.g. "0.1.0").
	Requires string `json:"requires,omitempty"`
	// License is an SPDX license identifier.
	License string `json:"license,omitempty"`
	// Permissions lists the capabilities the plugin asks for.
	Permissions []Permission `json:"permissions"`
	// Hooks lists every hook the plugin may subscribe to. Registrations for
	// hooks missing from this list are rejected at runtime.
	Hooks []string `json:"hooks"`
	// Shortcodes lists every shortcode tag the plugin may register
	// (register_shortcode); it needs the content:filter permission.
	Shortcodes []string `json:"shortcodes,omitempty"`
	// Assets maps every file under assets/ in the package to its SHA-256
	// (hex). wpgo-pkg fills it in when packing; since plugin.json is covered
	// by the package signature, the assets are too. A package may not carry
	// asset files that are missing here.
	Assets map[string]string `json:"assets,omitempty"`
}

// maxManifestSize bounds plugin.json.
const maxManifestSize = 64 << 10

var (
	slugRe    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+(\.\d+)?(-[0-9A-Za-z.-]+)?$`)
	hookRe    = regexp.MustCompile(`^[A-Za-z0-9_.:/-]+$`)
)

// ManifestError lists every problem found while validating a manifest.
type ManifestError struct {
	Problems []string
}

func (e *ManifestError) Error() string {
	if len(e.Problems) == 1 {
		return "plugin.json: " + e.Problems[0]
	}
	return "plugin.json has " + strconv.Itoa(len(e.Problems)) + " problems:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// ParseManifest decodes and validates plugin.json. Unknown fields are rejected
// so that typos (e.g. "permision") do not silently drop data.
func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) > maxManifestSize {
		return nil, fmt.Errorf("plugin.json: %d bytes exceeds the %d byte limit", len(data), maxManifestSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return nil, fmt.Errorf("plugin.json: invalid JSON at byte %d: %w", se.Offset, err)
		}
		return nil, fmt.Errorf("plugin.json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("plugin.json: unexpected data after the JSON object")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest and returns a *ManifestError describing every
// problem, or nil.
func (m *Manifest) Validate() error {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	switch {
	case m.Slug == "":
		add(`"slug" is required`)
	case len(m.Slug) < 2 || len(m.Slug) > 64 || !slugRe.MatchString(m.Slug):
		add(`"slug" %q must be 2-64 characters of lowercase letters, digits and single dashes (e.g. "my-plugin")`, m.Slug)
	case m.Slug == CoreOwner:
		add(`"slug" %q is reserved`, m.Slug)
	}
	if strings.TrimSpace(m.Name) == "" {
		add(`"name" is required`)
	} else if len(m.Name) > 100 {
		add(`"name" is longer than 100 characters`)
	}
	if m.Version == "" {
		add(`"version" is required (e.g. "1.0.0")`)
	} else if !versionRe.MatchString(m.Version) {
		add(`"version" %q is not a version number like "1.0.0"`, m.Version)
	}
	if m.Requires != "" && !versionRe.MatchString(m.Requires) {
		add(`"requires" %q is not a version number like "0.1.0"`, m.Requires)
	}
	if len(m.Description) > 2000 {
		add(`"description" is longer than 2000 characters`)
	}
	for _, f := range [...][2]string{{"author_uri", m.AuthorURI}, {"plugin_uri", m.PluginURI}} {
		field, v := f[0], f[1]
		if v == "" {
			continue
		}
		if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add(`%q %q must be an absolute http(s) URL`, field, v)
		}
	}

	declared := PermissionSet{}
	for _, perm := range m.Permissions {
		switch {
		case !perm.Valid():
			add(`unknown permission %q (known: %s)`, perm, joinPermissions(AllPermissions()))
		case declared[perm]:
			add(`permission %q is listed twice`, perm)
		}
		declared[perm] = true
	}

	seen := map[string]bool{}
	for _, hook := range m.Hooks {
		switch {
		case hook == "" || len(hook) > 128 || !hookRe.MatchString(hook):
			add(`hook name %q is invalid (use letters, digits and _ . : / -, max 128 characters)`, hook)
			continue
		case seen[hook]:
			add(`hook %q is listed twice`, hook)
			continue
		}
		seen[hook] = true
		if need := HookPermission(hook); need != "" && !declared[need] {
			add(`hook %q requires the %q permission; add it to "permissions"`, hook, need)
		}
	}

	seenTag := map[string]bool{}
	for _, tag := range m.Shortcodes {
		switch {
		case !wp.ValidShortcodeTag(tag):
			add(`shortcode %q is invalid (use lowercase letters, digits, _ and -, max 64 characters)`, tag)
			continue
		case seenTag[tag]:
			add(`shortcode %q is listed twice`, tag)
			continue
		}
		seenTag[tag] = true
	}
	if len(m.Shortcodes) > 0 && !declared[PermContentFilter] {
		add(`shortcodes require the %q permission; add it to "permissions"`, PermContentFilter)
	}

	var assetTotal int64
	for name, sum := range m.Assets {
		if err := CheckAssetPath(name); err != nil {
			add(`asset %q: %v`, name, err)
		}
		if !sha256HexRe.MatchString(sum) {
			add(`asset %q: digest must be the hex SHA-256 of the file`, name)
		}
		assetTotal++
	}
	if assetTotal > MaxAssetFiles {
		add(`too many assets: %d, limit is %d`, assetTotal, MaxAssetFiles)
	}

	if len(p) > 0 {
		return &ManifestError{Problems: p}
	}
	return nil
}

// Asset limits: files under assets/ in a package.
const (
	MaxAssetFile  = 2 << 20  // per file
	MaxAssetTotal = 16 << 20 // per package
	MaxAssetFiles = 256
	maxAssetPath  = 200
)

var (
	sha256HexRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	assetPathRe   = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
	assetTypes    = map[string]string{".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".woff2": "font/woff2"}
	assetExtOrder = []string{".css", ".js", ".svg", ".png", ".jpg", ".jpeg", ".webp", ".woff2"}
)

// CheckAssetPath validates a path relative to the package's assets/ directory:
// plain segments (letters, digits, dot, dash, underscore), no "." or ".."
// segments, at most 200 characters, and one of the allowed extensions.
func CheckAssetPath(name string) error {
	if name == "" || len(name) > maxAssetPath || !assetPathRe.MatchString(name) {
		return fmt.Errorf("path must be relative segments of letters, digits, '.', '-' and '_' (max %d characters)", maxAssetPath)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return errors.New("path segments may not start with a dot")
		}
	}
	if AssetContentType(name) == "" {
		return fmt.Errorf("extension must be one of %s", strings.Join(assetExtOrder, ", "))
	}
	return nil
}

// AssetContentType returns the Content-Type for an asset path, or "" when the
// extension is not allowed.
func AssetContentType(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return assetTypes[strings.ToLower(name[i:])]
}

// DeclaresShortcode reports whether tag is listed in the manifest.
func (m *Manifest) DeclaresShortcode(tag string) bool {
	for _, t := range m.Shortcodes {
		if t == tag {
			return true
		}
	}
	return false
}

// PermissionSet returns the declared permissions as a set.
func (m *Manifest) PermissionSet() PermissionSet { return NewPermissionSet(m.Permissions...) }

// DeclaresHook reports whether hook is listed in the manifest.
func (m *Manifest) DeclaresHook(hook string) bool {
	for _, h := range m.Hooks {
		if h == hook {
			return true
		}
	}
	return false
}

// CheckCompatible returns an error when the plugin requires a newer wpgo
// version than coreVersion. An empty Requires or coreVersion always passes.
func (m *Manifest) CheckCompatible(coreVersion string) error {
	if m.Requires == "" || coreVersion == "" {
		return nil
	}
	if compareVersions(coreVersion, m.Requires) < 0 {
		return fmt.Errorf("plugin %s requires wpgo %s or newer (running %s)", m.Slug, m.Requires, coreVersion)
	}
	return nil
}

// compareVersions compares the numeric major.minor.patch parts of two
// versions, ignoring pre-release suffixes. It returns -1, 0 or 1.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(s)
	}
	return out
}

func joinPermissions(ps []Permission) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = string(p)
	}
	return strings.Join(s, ", ")
}
