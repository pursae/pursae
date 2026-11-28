package i18n

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"wpgo/internal/netguard"
)

// Language is one of the translations WordPress offers (api.wordpress.org/translations/core).
type Language struct {
	Locale  string `json:"locale"`
	English string `json:"english"`
	Native  string `json:"native"`
	Version string `json:"version"`
	Package string `json:"package"`
}

//go:embed languages.json
var languagesJSON []byte

var languages = func() []Language {
	var l []Language
	_ = json.Unmarshal(languagesJSON, &l)
	sort.Slice(l, func(i, j int) bool { return l[i].Native < l[j].Native })
	return l
}()

// Languages lists every language with an official WordPress translation, by native name.
func Languages() []Language { return append([]Language(nil), languages...) }

// LanguageByLocale finds a language of the list.
func LanguageByLocale(locale string) (Language, bool) {
	for _, l := range languages {
		if l.Locale == locale {
			return l, true
		}
	}
	return Language{}, false
}

// rtlLocales are the right-to-left languages WordPress ships (their packs set "text direction").
var rtlLocales = map[string]bool{"ar": true, "ary": true, "azb": true, "ckb": true, "fa_AF": true, "fa_IR": true, "haz": true,
	"he_IL": true, "ps": true, "sd_PK": true, "skr": true, "ug_CN": true, "ur": true, "dv": true}

// IsRTL reports whether locale is written right to left.
func IsRTL(locale string) bool { return rtlLocales[locale] }

// Installer downloads language packs from the WordPress.org translation API, like
// wp_download_language_pack() and the plugin translation updates do.
type Installer struct {
	API    string // default https://api.wordpress.org/translations
	Client *http.Client
}

const maxPack = 40 << 20

func (in *Installer) client() *http.Client {
	if in.Client != nil {
		return in.Client
	}
	api, _ := url.Parse(in.api())
	allowLoop := api != nil && func() bool {
		ip := net.ParseIP(api.Hostname())
		return ip != nil && ip.IsLoopback()
	}()
	d := &net.Dialer{Timeout: 30 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if allowLoop && ip != nil && ip.IsLoopback() {
			return nil
		}
		if !translationPublicIP(ip) {
			return errors.New("translation download points at a private network")
		}
		return nil
	}}
	return &http.Client{
		Timeout:   90 * time.Second,
		Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return in.urlAllowed(req.URL.String())
		},
	}
}

// urlAllowed is the host allowlist for translation API and package URLs: WordPress.org, or the
// configured API host (tests point that at a local server). Anything else is refused.
func (in *Installer) urlAllowed(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("refusing translation URL %q", raw)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if (host == "downloads.wordpress.org" || host == "api.wordpress.org") && u.Scheme == "https" {
		return nil
	}
	api, err := url.Parse(in.api())
	if err == nil && strings.EqualFold(api.Hostname(), u.Hostname()) && api.Scheme == u.Scheme {
		return nil
	}
	return fmt.Errorf("refusing translation URL host %s", host)
}

// translationPublicIP shares the server's outbound list (CGNAT, NAT64, metadata, reserved ranges).
func translationPublicIP(ip net.IP) bool { return netguard.PublicIP(ip) }

func (in *Installer) api() string {
	if in.API != "" {
		return strings.TrimRight(in.API, "/")
	}
	return "https://api.wordpress.org/translations"
}

type apiTranslations struct {
	Translations []struct {
		Language string `json:"language"`
		Version  string `json:"version"`
		Package  string `json:"package"`
	} `json:"translations"`
}

func (in *Installer) get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	if err := in.urlAllowed(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := in.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", rawURL, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: response too large", rawURL)
	}
	return b, nil
}

// packageFor asks the API for the newest package of locale for a "core" or "plugins?slug=..." query.
func (in *Installer) packageFor(ctx context.Context, query, locale string) (string, error) {
	b, err := in.get(ctx, in.api()+"/"+query, 4<<20)
	if err != nil {
		return "", err
	}
	var r apiTranslations
	if err := json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	for _, t := range r.Translations {
		if t.Language == locale && t.Package != "" {
			return t.Package, nil
		}
	}
	return "", nil
}

var packFile = regexp.MustCompile(`^[A-Za-z0-9_.@-]+\.(mo|json)$`)

// extract writes the .mo and .json files of a pack zip into dir (.po and .l10n.php are not
// needed: WordPress Go reads the binary catalogs).
func extract(zipData []byte, dir string) (int, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	n := 0
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if f.FileInfo().IsDir() || !packFile.MatchString(name) || f.UncompressedSize64 > maxPack {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return n, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxPack))
		rc.Close()
		if err != nil {
			return n, err
		}
		// A temporary file of its own: two installs of one locale (two saves of Settings › General,
		// the background plugin packs) must not write into each other's file before the rename.
		tf, err := os.CreateTemp(dir, "."+name+".*.tmp")
		if err != nil {
			return n, err
		}
		tmp := tf.Name()
		_, err = tf.Write(b)
		if cerr := tf.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(tmp, 0o644)
		}
		if err == nil {
			err = renameReplace(tmp, filepath.Join(dir, name))
		}
		if err != nil {
			_ = os.Remove(tmp)
			return n, err
		}
		n++
	}
	return n, nil
}

// Install downloads the core pack of locale into m.Dir and the packs of the given plugin slugs
// into m.Dir/plugins. A missing plugin translation is not an error: not every plugin is
// translated into every language.
func (m *Manager) Install(ctx context.Context, in *Installer, locale string, plugins []string) error {
	if m.Dir == "" {
		return errors.New("no languages directory")
	}
	if !validLocale(locale) {
		return fmt.Errorf("invalid locale %q", locale)
	}
	pkg, err := in.packageFor(ctx, "core/1.0/", locale)
	if pkg == "" {
		if l, ok := LanguageByLocale(locale); ok {
			pkg = l.Package
		}
	}
	if pkg == "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("no translation of WordPress for %s", locale)
	}
	zipData, err := in.get(ctx, pkg, maxPack)
	if err != nil {
		return err
	}
	defer m.Reset() // also after a failed extract: files may be half written, cached results are stale
	installed, err := extract(zipData, m.Dir)
	if err != nil {
		return err
	}
	if installed == 0 {
		return fmt.Errorf("translation archive for %s contains no catalog files", locale)
	}
	var pluginErrors []error
	for _, slug := range plugins {
		pkg, err := in.packageFor(ctx, "plugins/1.0/?slug="+slug, locale)
		if err != nil || pkg == "" {
			continue
		}
		if zipData, err := in.get(ctx, pkg, maxPack); err == nil {
			if _, err := extract(zipData, filepath.Join(m.Dir, "plugins")); err != nil {
				pluginErrors = append(pluginErrors, fmt.Errorf("%s translation for %s: %w", slug, locale, err))
			}
		}
	}
	return errors.Join(pluginErrors...)
}

// ScriptTranslations returns the JSON locale data WordPress stores for a script (load_script_textdomain):
// <locale>-<md5 of the script path>.json in the languages directory, or in plugins/ with the
// plugin slug as prefix. relPath is the script path below the site root ("wp-includes/js/dist/editor.min.js").
// The result is the "locale_data.messages" object, ready for wp.i18n.setLocaleData.
func (m *Manager) ScriptTranslations(locale, relPath, plugin string) []byte {
	if m.Dir == "" || locale == "" || locale == "en_US" || !validLocale(locale) {
		return nil
	}
	rel := strings.TrimPrefix(relPath, "/")
	if strings.HasSuffix(rel, ".min.js") {
		rel = strings.TrimSuffix(rel, ".min.js") + ".js"
	}
	key := locale + "|" + rel + "|" + plugin
	m.scriptMu.Lock()
	out, ok := m.scripts[key]
	m.scriptMu.Unlock()
	if ok {
		return out
	}
	out = m.readScriptTranslations(locale, rel, plugin)
	m.scriptMu.Lock()
	// Bounded: the keys come from registered script paths, but never let the map grow without limit.
	if len(m.scripts) < 4096 {
		if m.scripts == nil {
			m.scripts = map[string][]byte{}
		}
		m.scripts[key] = out
	}
	m.scriptMu.Unlock()
	return out
}

// readScriptTranslations reads and converts one script's JSON file (nil when absent or invalid).
// A file another process changes shows up after Reset or a restart, like the .mo catalogs.
func (m *Manager) readScriptTranslations(locale, rel, plugin string) []byte {
	sum := md5.Sum([]byte(rel))
	name := locale + "-" + hex.EncodeToString(sum[:]) + ".json"
	path := filepath.Join(m.Dir, name)
	if plugin != "" {
		path = filepath.Join(m.Dir, "plugins", plugin+"-"+name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return localeData(b, plugin != "")
}

// PluginScriptTranslations merges every JSON file of a plugin pack (for bundles whose
// script paths differ from upstream).
func (m *Manager) PluginScriptTranslations(locale, plugin string) []byte {
	if m.Dir == "" || !validLocale(locale) || locale == "en_US" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(m.Dir, "plugins", plugin+"-"+locale+"-*.json"))
	sort.Strings(files)
	merged := map[string]json.RawMessage{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var msgs map[string]json.RawMessage
		if json.Unmarshal(localeData(b, true), &msgs) != nil {
			continue
		}
		for k, v := range msgs {
			if _, ok := merged[k]; !ok || k == "" {
				merged[k] = v
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	out, _ := json.Marshal(merged)
	return out
}

func localeData(b []byte, brand bool) []byte {
	var f struct {
		LocaleData map[string]map[string]json.RawMessage `json:"locale_data"`
	}
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	msgs := f.LocaleData["messages"]
	if msgs == nil {
		for _, v := range f.LocaleData {
			msgs = v
			break
		}
	}
	if msgs == nil {
		return nil
	}
	if brand {
		for k, v := range msgs {
			var forms []string
			if k == "" || json.Unmarshal(v, &forms) != nil {
				continue
			}
			for i := range forms {
				forms[i] = rebrandString(forms[i])
			}
			msgs[k], _ = json.Marshal(forms)
		}
	}
	out, _ := json.Marshal(msgs)
	return out
}

// renameReplace moves tmp over dst. On Windows a rename onto a file that another install is replacing
// at the same moment fails with "Access is denied", so it retries briefly there.
func renameReplace(tmp, dst string) error {
	err := os.Rename(tmp, dst)
	for i := 0; err != nil && runtime.GOOS == "windows" && i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		err = os.Rename(tmp, dst)
	}
	return err
}
