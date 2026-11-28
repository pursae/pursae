package server

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Labels of the checks that describe the running server process itself: a command-line process
// (`pursae site health`) has no plugin runtime and no live page cache, so it reports them differently.
const (
	healthLabelCacheOn     = "Page cache is enabled"
	healthLabelSandboxUp   = "The plugin sandbox is running"
	healthLabelSandboxDown = "The plugin sandbox could not start"
)

// healthTests runs every check of the Site Health screen. The dashboard widget counts the same
// list (siteHealthSummary), so a new check shows up in both places.
func (s *Server) healthTests(ctx context.Context) []healthTest {
	var tests []healthTest
	add := func(t healthTest) { tests = append(tests, t) }

	if v := s.coreUpdateAvailable(); v != "" {
		add(healthTest{Label: "A new version of Pursae (" + v + ") is available", Status: "recommended", Badge: "Security",
			Description: "You are running " + Version + ". Updates contain fixes and security patches.",
			Action:      `<a href="/wp-admin/update-core">Update Pursae</a>`})
	} else {
		add(healthTest{Label: "Your site is running the current version of Pursae (" + Version + ")", Status: "good", Badge: "Performance",
			Description: "Pursae is a single compiled binary; updating means replacing it and restarting."})
	}
	add(healthTest{Label: "Your site is running on " + runtime.Version(), Status: "good", Badge: "Performance",
		Description: "The Go runtime is compiled into the binary. There is no interpreter to keep up to date."})
	var pgVersion string
	_ = s.store.Pool.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion)
	pgMajor, _ := strconv.Atoi(strings.SplitN(pgVersion, ".", 2)[0])
	if pgMajor >= 15 {
		add(healthTest{Label: "PostgreSQL " + pgVersion + " is supported", Status: "good", Badge: "Performance", Description: "The database server is a recent, supported release."})
	} else {
		add(healthTest{Label: "PostgreSQL " + pgVersion + " is old", Status: "recommended", Badge: "Performance", Description: "PostgreSQL 15 or newer is recommended."})
	}
	if strings.HasPrefix(s.links(ctx).Home, "https://") {
		add(healthTest{Label: "Your website is using an active HTTPS connection", Status: "good", Badge: "Security", Description: "An HTTPS connection helps keep your site and its visitors safe."})
	} else {
		add(healthTest{Label: "Your website does not use HTTPS", Status: "recommended", Badge: "Security",
			Description: "HTTPS is already supported by most hosts and makes your site more secure. Put Pursae behind a TLS-terminating proxy and set PURSAE_SITE_URL to an https:// address."})
	}
	if f, err := os.CreateTemp(s.cfg.UploadsDir, ".health-*"); err == nil {
		f.Close()
		os.Remove(f.Name())
		add(healthTest{Label: "The uploads directory is writable", Status: "good", Badge: "Performance", Description: "Media files can be uploaded."})
	} else {
		add(healthTest{Label: "The uploads directory is not writable", Status: "critical", Badge: "Performance", Description: "Uploads will fail: " + err.Error()})
	}
	if s.settings.Bool("wpgo_cache_enabled") {
		st := s.currentCache().Stats()
		add(healthTest{Label: healthLabelCacheOn, Status: "good", Badge: "Performance",
			Description: fmt.Sprintf("Pursae Cache serves pages from memory (%d cached, %.0f%% hit ratio since start).", st.Entries, st.HitRatio*100)})
	} else {
		add(healthTest{Label: "Page cache is not enabled", Status: "recommended", Badge: "Performance",
			Description: "Page cache can improve your site performance by caching pages in memory.", Action: `<a href="/wp-admin/admin?page=wpgo-cache-settings">Turn on Pursae Cache</a>`})
	}
	if s.plugins != nil && s.plugins.initErr == nil {
		add(healthTest{Label: healthLabelSandboxUp, Status: "good", Badge: "Security", Description: "Plugins run as WebAssembly with only the permissions you granted."})
	} else {
		add(healthTest{Label: healthLabelSandboxDown, Status: "critical", Badge: "Security", Description: "Wasm plugins are disabled."})
	}
	if s.settings.String("timezone_string") != "" {
		add(healthTest{Label: "Your site uses a named time zone", Status: "good", Badge: "Performance", Description: "Dates follow daylight saving changes automatically."})
	} else {
		add(healthTest{Label: "Your site uses a fixed UTC offset", Status: "recommended", Badge: "Performance",
			Description: "Choose a city-based time zone so dates follow daylight saving time.", Action: `<a href="/wp-admin/options-general">General Settings</a>`})
	}
	if !s.settings.Bool("blog_public") {
		add(healthTest{Label: "Search engines are discouraged from indexing this site", Status: "recommended", Badge: "Privacy",
			Description: "Your site is hidden from search engines.", Action: `<a href="/wp-admin/options-reading">Reading Settings</a>`})
	}
	var inactivePlugins int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM plugins WHERE status = 'inactive'`).Scan(&inactivePlugins)
	if inactivePlugins > 0 {
		add(healthTest{Label: "You should remove inactive plugins", Status: "recommended", Badge: "Security",
			Description: fmt.Sprintf("Your site has %d inactive plugin(s). Inactive plugins are not run, but removing what you don't use keeps things tidy.", inactivePlugins),
			Action:      `<a href="/wp-admin/plugins?plugin_status=inactive">Manage inactive plugins</a>`})
	}
	if s.settings.Int("wp_page_for_privacy_policy") == 0 {
		add(healthTest{Label: "No privacy policy page is set", Status: "recommended", Badge: "Privacy", Action: `<a href="/wp-admin/options-privacy">Privacy Settings</a>`})
	}

	if s.cfg.SMTPHost == "" && !s.mailSMTPOn() {
		add(healthTest{Label: "Email delivery is not configured", Status: "critical", Badge: "Performance",
			Description: "Without SMTP, every message (password resets, order and privacy confirmations) is only saved as an .eml file and never reaches the recipient. Set PURSAE_SMTP_HOST or activate Pursae SMTP.",
			Action:      `<a href="/wp-admin/plugins">Manage plugins</a>`})
	}
	tests = append(tests, s.privateEndpointHealthTests(ctx)...) // private_endpoint_notice.go
	var failedJobs int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status = 'failed'`).Scan(&failedJobs)
	if failedJobs > 0 {
		add(healthTest{Label: "Background jobs have failed", Status: "recommended", Badge: "Performance",
			Description: fmt.Sprintf("%d scheduled job(s) failed after their last attempts. See Scheduled Events on the Info tab.", failedJobs)})
	}
	if used, ok := diskUsedPercent(s.cfg.UploadsDir); ok && used >= 90 {
		st := "recommended"
		if used >= 97 {
			st = "critical"
		}
		add(healthTest{Label: "The uploads disk is almost full", Status: st, Badge: "Performance",
			Description: fmt.Sprintf("The disk holding the uploads directory is %.0f%% full. Uploads and backups will fail when it runs out of space.", used)})
	}
	if s.bmgrOn() {
		if last, ok := s.settings.Get(bmgrOptLast).(map[string]any); ok {
			if msg, _ := last["error"].(string); msg != "" {
				add(healthTest{Label: "The last scheduled backup failed", Status: "recommended", Badge: "Security", Description: msg})
			}
		}
	}
	return tests
}

// builtinPluginSummary lists the built-in plugins and whether each is on, for the Info tab.
func (s *Server) builtinPluginSummary() string {
	var parts []string
	for _, r := range s.nativePluginRows() {
		state := "inactive"
		if r.Active {
			state = "active"
		}
		parts = append(parts, strings.TrimSpace(r.Name+" "+r.Version)+" ("+state+")")
	}
	return strings.Join(parts, ", ")
}

// environmentType is wp_get_environment_type(): local on a loopback or .localhost site address.
func (s *Server) environmentType() string {
	if isLocalHost(hostOf(s.settings.String("home"))) {
		return "local"
	}
	return "production"
}
