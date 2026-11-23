// Package config reads runtime configuration from the environment.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultHubURL is the Pursae Manager every Pursae install talks to.
const DefaultHubURL = "https://hub.pursae.com"

type Config struct {
	Addr        string // listen address, e.g. ":8080"
	DatabaseURL string
	SiteURL     string // public base URL without trailing slash, used on first install
	UploadsDir  string
	Secret      string // optional; generated and stored in settings when empty
	// SetupToken (PURSAE_SETUP_TOKEN) guards /wp-admin/install until the site is installed. Empty: the
	// server generates one on an uninstalled site and prints the install URL (`wpgo setup-token` issues a new one).
	SetupToken string
	WebDir     string // when set, web assets and templates are read from this directory and reloaded per request
	// HubURL is Pursae Manager (marketplace, licenses, release checks, statistics): always DefaultHubURL.
	// Only a development build (-tags devhub) can point it elsewhere, with PURSAE_DEV_HUB_URL
	// (config_devhub.go); "off" there disables it.
	HubURL string
	// HubOff (PURSAE_HUB=off) switches Pursae Manager off in any build: the site sends nothing and gets
	// no updates, marketplace or licenses; every admin page then says how to turn it back on.
	HubOff bool
	// ThemeLibraryDir (PURSAE_THEME_LIBRARY_DIR) serves the theme library from a directory of theme package
	// directories (development, offline sites, tests) instead of Pursae Manager.
	ThemeLibraryDir string
	// TrustedProxies lists the proxies (IPs or CIDRs, comma separated) whose X-Forwarded-For / X-Real-IP is believed
	// when deriving the visitor address. Empty: loopback and private ranges; "none": no proxy is trusted.
	TrustedProxies string
	// ExposeUsers (PURSAE_EXPOSE_USERS=1) lets visitors list accounts (REST users, /?author=N, the users sitemap). Off by
	// default: those routes disclose account names.
	ExposeUsers bool
	// AllowUnsignedPlugins (PURSAE_ALLOW_UNSIGNED_PLUGINS=1) is the server-side half of the plugin_allow_unsigned
	// setting: unsigned plugin packages are accepted only when both are on. A database or CLI write alone (an
	// administrator account, a SQL injection) cannot turn plugin signature checks off.
	AllowUnsignedPlugins bool
	// LangDir holds the downloaded language packs (WP_LANG_DIR); default: "languages" next to the uploads.
	LangDir         string
	TranslationsAPI string // the WordPress.org translation API; default https://api.wordpress.org/translations

	// GeoIPDB (PURSAE_GEOIP_DB) is the IP-to-country database file (MMDB); default geoip/country.mmdb next
	// to the uploads. GeoIPUpdateOff (PURSAE_GEOIP_UPDATE=off) stops the monthly download from DB-IP.
	GeoIPDB        string
	GeoIPUpdateOff bool

	// Metrics (docs/developer/operations/metrics.md). MetricsOn (PURSAE_METRICS=1) enables the Prometheus
	// endpoint; MetricsAddr (PURSAE_METRICS_ADDR) serves it on its own listener, MetricsToken
	// (PURSAE_METRICS_TOKEN) is a bearer token it requires; without MetricsAddr the endpoint is /metrics on
	// the site listener and needs the token. SlowQuery (PURSAE_SLOW_QUERY, "250ms" or whole
	// milliseconds) logs and counts database queries at least that slow; 0 is off.
	MetricsOn    bool
	MetricsAddr  string
	MetricsToken string
	SlowQuery    time.Duration

	// Outgoing mail. Without SMTPHost, messages are written as .eml files to MailDir (development).
	SMTPHost, SMTPUser, SMTPPass, MailFrom string
	SMTPPort                               string
	MailDir                                string
}

func Load() Config {
	c := Config{
		Addr:                 env("PURSAE_ADDR", ":8080"),
		DatabaseURL:          env("DATABASE_URL", "postgres://wpgo:wpgo@localhost:5432/wpgo?sslmode=disable"),
		SiteURL:              strings.TrimRight(env("PURSAE_SITE_URL", "http://localhost:8080"), "/"),
		UploadsDir:           env("PURSAE_UPLOADS_DIR", "./data/uploads"),
		Secret:               Env("SECRET"),
		SetupToken:           strings.TrimSpace(Env("SETUP_TOKEN")),
		WebDir:               Env("WEB_DIR"),
		HubURL:               hubURL(),
		HubOff:               hubOff(Env("HUB")),
		ThemeLibraryDir:      Env("THEME_LIBRARY_DIR"),
		TrustedProxies:       Env("TRUSTED_PROXIES"),
		ExposeUsers:          Env("EXPOSE_USERS") == "1",
		AllowUnsignedPlugins: Env("ALLOW_UNSIGNED_PLUGINS") == "1",
		LangDir:              Env("LANG_DIR"),
		TranslationsAPI:      Env("TRANSLATIONS_API"),
		SMTPHost:             Env("SMTP_HOST"),
		SMTPPort:             env("PURSAE_SMTP_PORT", "587"),
		SMTPUser:             Env("SMTP_USER"),
		SMTPPass:             Env("SMTP_PASS"),
		MailFrom:             Env("MAIL_FROM"),
		MailDir:              Env("MAIL_DIR"),
		GeoIPDB:              Env("GEOIP_DB"),
		GeoIPUpdateOff:       hubOff(Env("GEOIP_UPDATE")),
		MetricsOn:            Env("METRICS") == "1",
		MetricsAddr:          strings.TrimSpace(Env("METRICS_ADDR")),
		MetricsToken:         strings.TrimSpace(Env("METRICS_TOKEN")),
		SlowQuery:            parseSlowQuery(Env("SLOW_QUERY")),
	}
	if c.HubOff {
		c.HubURL = "off" // hubBase() treats it as no Manager
	}
	return c
}

// env is the variable key (a PURSAE_ name through Env, so its WPGO_ name still works) or def when it
// is unset or empty.
func env(key, def string) string {
	v := os.Getenv(key)
	if strings.HasPrefix(key, EnvPrefix) {
		v = Env(key)
	}
	if v != "" {
		return v
	}
	return def
}

// hubOff reports whether PURSAE_HUB asks to switch Pursae Manager off.
func hubOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "none", "0", "false", "no":
		return true
	}
	return false
}

// parseSlowQuery reads PURSAE_SLOW_QUERY: a Go duration ("250ms", "1s") or a plain number of
// milliseconds. Empty, invalid or not positive: 0 (off).
func parseSlowQuery(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Millisecond
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return 0
}
