package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/plugins"
)

// Pursae Manager (formerly the Hub) is the api.wordpress.org of Pursae: it lists the plugin and theme
// marketplace, announces core releases (with signed binaries the site can install itself, core_update.go),
// checks license keys (license.go) and collects the sites' statistics. The contract is
// pursae-manager/API.md. Every request carries the site's home URL and, when one is entered, its
// license key (hubNewRequest); the answers may depend on the license, so the cached ones are keyed by it.
// Once a day the site pings the Manager with its version and inventory and sends the events it queued
// (hub_telemetry.go); statistics are always on.

const (
	hubCacheTTL   = 12 * time.Hour
	hubMaxPackage = 64 << 20
)

// hubUserAgent is the User-Agent of every request to Pursae Manager.
func hubUserAgent() string { return "Pursae/" + Version }

type hubIndex struct {
	Hub    string         `json:"hub"`
	API    int            `json:"api"`
	Core   *hubCore       `json:"core"`
	Keys   []hubKey       `json:"keys"`
	Counts map[string]int `json:"counts"`
	Brand  *hubBrand      `json:"brand,omitempty"` // brand.go
}

type hubCore struct {
	Latest      string         `json:"latest"`
	Channel     string         `json:"channel"` // stable | beta (beta only reaches beta licenses)
	Released    string         `json:"released"`
	DownloadURL string         `json:"download_url"`
	NotesURL    string         `json:"notes_url"`
	Notes       string         `json:"notes"`
	Artifacts   []coreArtifact `json:"artifacts"` // signed binaries built by the Manager (core_update.go)
}

type hubKey struct {
	ID     string `json:"id"`
	Public string `json:"public"`
	// Endorsement is a pinned root key's signature over the key (hub_trust.go); without it the
	// key cannot verify a core release on a build with a root key.
	Endorsement string `json:"endorsement,omitempty"`
}

// hubItem is one plugin or theme listing.
type hubItem struct {
	Slug          string   `json:"slug"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Description   string   `json:"description"`
	Author        string   `json:"author"`
	AuthorURL     string   `json:"author_url"`
	Homepage      string   `json:"homepage"`
	IconURL       string   `json:"icon_url"`
	ScreenshotURL string   `json:"screenshot_url"`
	RequiresCore  string   `json:"requires_core"`
	TestedCore    string   `json:"tested_core"`
	Permissions   []string `json:"permissions"`
	Tags          []string `json:"tags"`
	Changelog     string   `json:"changelog"` // what the listed version changes (plain text)
	Downloads     int      `json:"downloads"`
	Updated       string   `json:"updated"`
	Size          int64    `json:"size"`
	SHA256        string   `json:"sha256"`
	SignedBy      string   `json:"signed_by"`
	DownloadURL   string   `json:"download_url"`
	Access        string   `json:"access"`  // public | pro | private
	Channel       string   `json:"channel"` // stable | beta, of the listed version
	// Themes: the listed version's theme.json (library metadata without a download, theme_library.go)
	// and whether a new site installs the theme right after its installer.
	Manifest   json.RawMessage `json:"manifest,omitempty"`
	Preinstall bool            `json:"preinstall,omitempty"`
	// Position is the theme's place in the Manager's library order (1 is first; 0 when it has
	// none, or from a Manager that does not send it).
	Position int `json:"position,omitempty"`
}

type hubList struct {
	Total int       `json:"total"`
	Pages int       `json:"pages"`
	Items []hubItem `json:"items"`
}

// hubBase is Pursae Manager's URL: config.DefaultHubURL (https://hub.pursae.com) in every
// production build; no option or environment variable changes it. A development build
// (-tags devhub) may set PURSAE_DEV_HUB_URL, and tests set cfg.HubURL; "off" (or empty) disables it.
func (s *Server) hubBase() string {
	u := strings.TrimSpace(s.cfg.HubURL)
	switch strings.ToLower(u) {
	case "", "off", "none", "0", "false":
		return ""
	}
	return strings.TrimRight(u, "/")
}

// errHubOff: PURSAE_HUB=off, a devhub build with PURSAE_DEV_HUB_URL=off or a test has no Manager; the message keeps
// its translated wording.
var errHubOff = errors.New("Pursae Manager is switched off in this build")

func (s *Server) hubClient() *http.Client {
	return hubHTTPClient(20 * time.Second)
}

// hubNewRequest builds every request to Pursae Manager (JSON documents, package and core downloads,
// the logo, the ping and the events): the User-Agent, X-Pursae-Site (the site's home URL) and, when
// a key is entered under Settings › License, X-Pursae-License. The site and license headers go only
// to the Manager itself, never to a third-party URL (an absolute logo_url).
func (s *Server) hubNewRequest(ctx context.Context, method, u string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if err := hubURLAllowed(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", hubUserAgent()+"; "+s.settings.String("home"))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if base := s.hubBase(); base != "" && strings.HasPrefix(u, base+"/") {
		if site := hubSiteURL(s.settings.String("home")); site != "" {
			req.Header.Set("X-Pursae-Site", site)
		}
		if key := s.licenseKey(); key != "" {
			req.Header.Set("X-Pursae-License", key)
		}
	}
	return req, nil
}

// hubSiteIsLocal reports whether the site's address is a local one (localhost, *.localhost, *.local,
// *.test, a loopback, private or link-local IP) talking to a Manager on the internet: such a site
// sends that Manager no ping and no events, so development and test installs never reach
// hub.pursae.com's site list or statistics. A local Manager (a development hub, the tests' fake
// one) still gets them.
func (s *Server) hubSiteIsLocal() bool { return hubLocalSite(s.hubBase(), s.settings.String("home")) }

// hubLocalSite is hubSiteIsLocal for a Manager address and a site home URL.
func hubLocalSite(hubBase, home string) bool {
	if hu, err := url.Parse(hubBase); err == nil && hu.Hostname() != "" && isLocalHost(strings.ToLower(hu.Hostname())) {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(home))
	if err != nil || u.Hostname() == "" {
		return false
	}
	return isLocalHost(strings.ToLower(u.Hostname()))
}

// hubSiteURL normalises the home URL like the Manager does for pings: scheme://host[:port], the host
// lower-cased, default ports, path, query and user info dropped.
func hubSiteURL(home string) string {
	u, err := url.Parse(strings.TrimSpace(home))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host
}

// hubCacheKey is the transient key of a cached Manager answer. The answers depend on the license
// (pro and private listings, beta versions and releases), so a site with a key caches them under a
// key of its own; a new key never sees the answers given to the old one.
func (s *Server) hubCacheKey(name string) string {
	if key := s.licenseKey(); key != "" {
		return name + "_" + licenseHash(key)
	}
	return name
}

// hubGet fetches a JSON document from the Hub.
func (s *Server) hubGet(ctx context.Context, path string, out any) error {
	base := s.hubBase()
	if base == "" {
		return errHubOff
	}
	req, err := s.hubNewRequest(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.hubClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub: %s: HTTP %d", path, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// hubCacheGet reads a cached Hub document from the transients table.
func (s *Server) hubCacheGet(ctx context.Context, key string, out any) bool {
	var raw []byte
	err := s.store.Pool.QueryRow(ctx, `SELECT value FROM transients WHERE key = $1 AND (expires_at IS NULL OR expires_at > now())`, key).Scan(&raw)
	return err == nil && json.Unmarshal(raw, out) == nil
}

func (s *Server) hubCacheSet(ctx context.Context, key string, v any, ttl time.Duration) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = s.store.Pool.Exec(ctx, `INSERT INTO transients(key, value, expires_at) VALUES ($1, $2, now() + $3::interval)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at, updated_at = now()`,
		key, raw, fmt.Sprintf("%d seconds", int(ttl.Seconds())))
}

// hubIndexCached is the Hub index (latest core release, signing keys), cached for 12 hours.
func (s *Server) hubIndexCached(ctx context.Context) (*hubIndex, error) {
	var idx hubIndex
	if s.hubCacheGet(ctx, s.hubCacheKey("wpgo_hub_index"), &idx) && idx.API > 0 {
		s.hubTrustKeys(idx.Keys)
		s.rememberCoreLatest(ctx, &idx)
		return &idx, nil
	}
	return s.hubFetchIndex(ctx)
}

// hubTrustCached trusts the keys of an already cached index without going to the network (start-up).
func (s *Server) hubTrustCached(ctx context.Context) {
	var idx hubIndex
	if s.hubCacheGet(ctx, s.hubCacheKey("wpgo_hub_index"), &idx) {
		s.hubTrustKeys(idx.Keys)
		s.rememberCoreLatest(ctx, &idx)
		return
	}
	// The index expired (restart more than 12 hours after the last fetch, or the Manager is
	// unreachable): keep trusting the last fetched keys so Manager-signed plugins still load.
	// They pass the same endorsement check; the next fetch replaces the set.
	var keys []hubKey
	if s.hubCacheGet(ctx, hubLastKeysKey, &keys) {
		s.hubTrustKeys(keys)
	}
}

// hubLastKeysKey is the non-expiring copy of the last fetched index's signing keys (outside the
// wpgo_hub_ prefix, so hubDropCaches keeps it: the keys do not depend on the license).
const hubLastKeysKey = "wpgo_hubkeys_last"

func (s *Server) hubFetchIndex(ctx context.Context) (*hubIndex, error) {
	var idx hubIndex
	if err := s.hubGet(ctx, "/api/v1/index", &idx); err != nil {
		return nil, err
	}
	s.hubCacheSet(ctx, s.hubCacheKey("wpgo_hub_index"), idx, hubCacheTTL)
	if raw, err := json.Marshal(idx.Keys); err == nil {
		_, _ = s.store.Pool.Exec(ctx, `INSERT INTO transients(key, value, expires_at) VALUES ($1, $2, NULL)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, expires_at = NULL, updated_at = now()`, hubLastKeysKey, raw)
	}
	s.hubTrustKeys(idx.Keys)
	s.rememberCoreLatest(ctx, &idx) // the admin pages' "Pursae X is available" (core_update_nag.go)
	s.hubSyncBrand(ctx, idx.Brand)
	return &idx, nil
}

// hubTrustKeys makes the Manager's current signing keys the trusted plugin keys that come from
// the Manager (wpgo_hub_trust_keys; with a pinned root key only endorsed ones, hub_trust.go). The
// set is replaced, not added to, so a key the Manager stops listing stops being trusted.
func (s *Server) hubTrustKeys(keys []hubKey) {
	if s.plugins == nil {
		return
	}
	var trusted []ed25519.PublicKey
	if s.settings.Bool("wpgo_hub_trust_keys") {
		trusted = pluginHubKeys(keys)
	}
	s.plugins.setHubKeys(trusted)
}

// hubCatalogMaxPages bounds the pages of one catalog read (100 listings each).
const hubCatalogMaxPages = 50

// hubCatalog lists the Hub's plugins or themes (kind "plugins" | "themes"), every page of them,
// cached for 12 hours under the license's cache key.
func (s *Server) hubCatalog(ctx context.Context, kind string) ([]hubItem, error) {
	var list hubList
	key := s.hubCacheKey("wpgo_hub_" + kind)
	if s.hubCacheGet(ctx, key, &list) {
		return list.Items, nil
	}
	list = hubList{}
	for page := 1; page <= hubCatalogMaxPages; page++ {
		var p hubList
		if err := s.hubGet(ctx, "/api/v1/"+kind+"?per_page=100&sort=downloads&page="+strconv.Itoa(page), &p); err != nil {
			return nil, err
		}
		list.Items = append(list.Items, p.Items...)
		list.Total, list.Pages = p.Total, p.Pages
		if len(p.Items) == 0 || page >= p.Pages || len(list.Items) >= p.Total {
			break
		}
	}
	s.hubCacheSet(ctx, key, list, hubCacheTTL)
	return list.Items, nil
}

// hubItemFor is one listing with its download details.
func (s *Server) hubItemFor(ctx context.Context, kind, slug string) (*hubItem, error) {
	if !validSlug(slug) {
		return nil, errors.New("invalid slug")
	}
	var it hubItem
	if err := s.hubGet(ctx, "/api/v1/"+kind+"/"+slug, &it); err != nil {
		return nil, err
	}
	return &it, nil
}

func validSlug(slug string) bool {
	if len(slug) < 2 || len(slug) > 64 {
		return false
	}
	for i, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(slug)-1:
		default:
			return false
		}
	}
	return true
}

// hubDownload fetches a package from the Hub and checks its SHA-256 against the listing.
func (s *Server) hubDownload(ctx context.Context, it *hubItem) ([]byte, error) {
	base := s.hubBase()
	if base == "" {
		return nil, errHubOff
	}
	u := it.DownloadURL
	if u == "" || !strings.HasPrefix(u, base+"/") {
		// Only the Hub itself may serve packages: no third-party download URLs.
		return nil, fmt.Errorf("hub: %s has no download on the Hub", it.Slug)
	}
	req, err := s.hubNewRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hubClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, errors.New("hub: this package needs a license that includes it (Settings › License)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub: download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, hubMaxPackage+1))
	if err != nil {
		return nil, err
	}
	if len(data) > hubMaxPackage {
		return nil, errors.New("hub: the package is larger than 64 MiB")
	}
	if it.Size > 0 && int64(len(data)) != it.Size {
		return nil, errors.New("hub: the downloaded package does not have the published size")
	}
	sum := sha256.Sum256(data)
	if it.SHA256 != "" && !strings.EqualFold(hex.EncodeToString(sum[:]), it.SHA256) {
		return nil, errors.New("hub: the downloaded package does not match the published checksum")
	}
	return data, nil
}

// hubInstallPlugin installs a plugin from the marketplace: download, checksum, signature, install.
// An installed active plugin is reloaded with the new package (installAndReload); updated reports
// that the plugin was installed before.
func (s *Server) hubInstallPlugin(ctx context.Context, slug string) (*plugins.Manifest, bool, error) {
	if _, err := s.hubIndexCached(ctx); err != nil { // the Hub's signing keys
		return nil, false, err
	}
	it, err := s.hubItemFor(ctx, "plugins", slug)
	if err != nil {
		return nil, false, err
	}
	data, err := s.hubDownload(ctx, it)
	if err != nil {
		return nil, false, err
	}
	return s.installAndReload(ctx, data, pluginSourceHub, slug, false)
}

// hubPing is the daily wpgo_hub_check event (WordPress does the same against api.wordpress.org): it
// sends the Manager the site's version and inventory (hubPingBody), flushes the queued events and
// refreshes the cached index.
// hubReportedVersionOption is the core version the last successful ping reported; a start with
// another version (after a core update) pings again right away (ensureEvents).
const hubReportedVersionOption = "wpgo_hub_reported_version"

func (s *Server) hubPing(ctx context.Context) error {
	base := s.hubBase()
	if base == "" || s.sandbox != nil {
		return nil
	}
	// Before the installer has run there is no site address: the Manager would refuse the ping
	// (bad_url) and the retries would run out before anyone installs. installSubmit queues a
	// check as soon as the address is known.
	if hubSiteURL(s.settings.String("home")) == "" {
		return nil
	}
	// A site on localhost, a .test/.local name or a private address is a development or test
	// install: it still reads the Manager's index (updates, the catalog) but reports nothing, so it
	// never shows up among the Manager's sites or in its statistics.
	if !s.hubSiteIsLocal() {
		body, _ := json.Marshal(s.hubPingBody(ctx))
		req, err := s.hubNewRequest(ctx, http.MethodPost, base+"/api/v1/sites/ping", bytes.NewReader(body))
		if err != nil {
			return err
		}
		resp, err := s.hubClient().Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("hub: ping: HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusOK && s.settings.String(hubReportedVersionOption) != Version {
			_ = s.settings.Set(ctx, map[string]any{hubReportedVersionOption: Version})
		}
		s.hubFlushEvents(ctx)
	}
	s.licenseWatch(ctx) // expiring / revoked license: notice + one e-mail (license_watch.go)
	_, err := s.hubFetchIndex(ctx)
	if err == nil {
		s.hubAutoUpdatePlugins(ctx) // plugins with auto-updates on (hub_updates.go)
		s.coreAutoUpdate(ctx)       // core with automatic updates on (core_update_auto.go); restarts last
	}
	return err
}

// queueHubCheck runs the Manager check now (a fresh install): a queued one-off check is moved to
// now, otherwise one is queued, so the new site shows on the Manager within the scheduler tick
// instead of at the next daily check.
func (s *Server) queueHubCheck(ctx context.Context) {
	if s.hubBase() == "" {
		return
	}
	tag, err := s.store.Pool.Exec(ctx, `UPDATE jobs SET run_at = now(), attempts = 0, last_error = ''
		WHERE name = 'wpgo_hub_check' AND recurrence = '' AND status = 'queued'`)
	if err == nil && tag.RowsAffected() > 0 {
		return
	}
	if err := s.queueJob(ctx, "wpgo_hub_check", nil, time.Time{}); err != nil {
		slog.Error("queue hub check", "err", err)
	}
}

// hubDropCaches forgets every cached Manager answer (index, catalogs, under any license key).
func (s *Server) hubDropCaches(ctx context.Context) {
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM transients WHERE key LIKE 'wpgo\_hub\_%'`)
	s.libraryForget()
	s.rememberCoreLatest(ctx, nil) // a release the previous license could see is not announced any more
}

// hubRefresh drops the cached catalog and index (Updates › Check again).
func (s *Server) hubRefresh(ctx context.Context) {
	s.hubDropCaches(ctx)
	if _, err := s.hubFetchIndex(ctx); err != nil && !errors.Is(err, errHubOff) {
		slog.Warn("hub: refresh", "err", err)
	}
}

// hubUpdate reports a newer core release than the running one, if the Hub announced one.
func (s *Server) hubUpdate(ctx context.Context) *hubCore {
	idx, err := s.hubIndexCached(ctx)
	if err != nil || idx.Core == nil || idx.Core.Latest == "" {
		return nil
	}
	if versionLess(Version, idx.Core.Latest) {
		return idx.Core
	}
	return nil
}

// versionLess reports whether a < b in semver order: major.minor.patch, then a pre-release sorts
// before its release (0.2.0-beta.1 < 0.2.0-rc.1 < 0.2.0) with dot-separated identifiers compared
// numerically when both are numbers; build metadata (+…) is ignored.
func versionLess(a, b string) bool {
	parts := func(v string) ([3]int, string) {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		if i := strings.IndexByte(v, '+'); i >= 0 {
			v = v[:i]
		}
		pre := ""
		if i := strings.IndexByte(v, '-'); i >= 0 {
			v, pre = v[:i], v[i+1:]
		}
		var out [3]int
		for i, p := range strings.SplitN(v, ".", 3) {
			out[i], _ = strconv.Atoi(p)
		}
		return out, pre
	}
	pa, preA := parts(a)
	pb, preB := parts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	if preA == "" || preB == "" {
		return preA != "" && preB == "" // a pre-release comes before its release
	}
	ia, ib := strings.Split(preA, "."), strings.Split(preB, ".")
	for i := 0; i < len(ia) && i < len(ib); i++ {
		if ia[i] == ib[i] {
			continue
		}
		na, errA := strconv.Atoi(ia[i])
		nb, errB := strconv.Atoi(ib[i])
		switch {
		case errA == nil && errB == nil:
			return na < nb
		case errA == nil || errB == nil:
			return errA == nil // numeric identifiers sort before alphanumeric ones
		default:
			return ia[i] < ib[i]
		}
	}
	return len(ia) < len(ib)
}
