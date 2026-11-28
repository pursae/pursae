package server

import (
	"context"
	"html/template"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/cache"
)

// Admin-set endpoints on the private network (endpoint_guard.go) are refused at connect time unless
// the server sets PURSAE_ALLOW_PRIVATE_ENDPOINTS=1. A site that saved a loopback or LAN SMTP host or
// purge URL before that rule would otherwise only find out from the first failure, so the admin
// screens (manage_options) and Site Health say so up front.

const (
	privSMTPNoticeFmt  = "Email cannot be sent: the SMTP server %s is on a private network. Set PURSAE_ALLOW_PRIVATE_ENDPOINTS=1 on the server to allow it."
	privPurgeNoticeFmt = "Cache purge requests cannot be sent: the purge URL %s is on a private network. Set PURSAE_ALLOW_PRIVATE_ENDPOINTS=1 on the server to allow it."
	privSMTPLabel      = "The SMTP server is on a private network"
	privPurgeLabel     = "The cache purge URL is on a private network"
	privHostTTL        = 10 * time.Minute
	privLookupTimeout  = 1500 * time.Millisecond
)

type privHostEntry struct {
	private bool
	at      time.Time
}

var (
	privHostMu    sync.Mutex
	privHostCache = map[string]privHostEntry{}
	// privLookup resolves a host name (tests replace it).
	privLookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return net.DefaultResolver.LookupIPAddr(ctx, host)
	}
)

// privateHost reports whether host is (or resolves to) a loopback, RFC1918, unique-local or CGNAT
// address that only PURSAE_ALLOW_PRIVATE_ENDPOINTS lets through. A name that does not resolve is
// not reported. Results are cached for privHostTTL per host.
func privateHost(ctx context.Context, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return privateOnlyIP(ip)
	}
	privHostMu.Lock()
	e, ok := privHostCache[host]
	privHostMu.Unlock()
	if ok && time.Since(e.at) < privHostTTL {
		return e.private
	}
	lctx, cancel := context.WithTimeout(ctx, privLookupTimeout)
	defer cancel()
	addrs, err := privLookup(lctx, host)
	private := false
	if err == nil {
		for _, a := range addrs {
			if privateOnlyIP(a.IP) {
				private = true
				break
			}
		}
	}
	privHostMu.Lock()
	privHostCache[host] = privHostEntry{private: private, at: time.Now()}
	privHostMu.Unlock()
	return private
}

// privateOnlyIP: refused without the switch, accepted with it.
func privateOnlyIP(ip net.IP) bool {
	return endpointAddrOK(ip, false, privateEndpointsEnv) != nil && endpointAddrOK(ip, true, privateEndpointsEnv) == nil
}

// privateSMTPHost is the Pursae SMTP "Other SMTP" host (primary mailer or an additional
// connection) that sits on the private network, "" when there is none or the switch is set.
func (s *Server) privateSMTPHost(ctx context.Context) string {
	if privateEndpointsAllowed() || msTestBase != "" || !s.mailSMTPOn() {
		return ""
	}
	o := s.msLoad()
	var hosts []string
	if o.Mail.Mailer == "smtp" {
		hosts = append(hosts, o.Mailers["smtp"]["host"])
	}
	for _, c := range o.Connections {
		if c.Mailer == "smtp" {
			hosts = append(hosts, c.Opts["host"])
		}
	}
	for _, h := range hosts {
		if privateHost(ctx, h) {
			return strings.TrimSpace(h)
		}
	}
	return ""
}

// privatePurgeURL is the configured front proxy purge URL when it is on the private network.
func (s *Server) privatePurgeURL(ctx context.Context) string {
	if privateEndpointsAllowed() {
		return ""
	}
	raw := strings.TrimSpace(s.settings.String("wpgo_cache_purge_url"))
	if !proxyPurgeConfigured(cache.ServerHint(s.settings.String("wpgo_cache_server")), raw) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || !privateHost(ctx, u.Hostname()) {
		return ""
	}
	return raw
}

func privEndpointText(format, v string) string { return strings.Replace(format, "%s", v, 1) }

// privateEndpointNotices are the admin notices for both endpoints (manage_options only).
func (s *Server) privateEndpointNotices(c *gin.Context) []notice {
	u := currentUser(c)
	if u == nil || !u.Can("manage_options", nil) {
		return nil
	}
	ctx := c.Request.Context()
	var out []notice
	if h := s.privateSMTPHost(ctx); h != "" {
		out = append(out, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(privEndpointText(privSMTPNoticeFmt, h)))})
	}
	if p := s.privatePurgeURL(ctx); p != "" {
		out = append(out, notice{Type: "warning", Message: template.HTML(template.HTMLEscapeString(privEndpointText(privPurgeNoticeFmt, p)))})
	}
	return out
}

// privateEndpointHealthTests are the Site Health rows for both endpoints.
func (s *Server) privateEndpointHealthTests(ctx context.Context) []healthTest {
	var out []healthTest
	if h := s.privateSMTPHost(ctx); h != "" {
		out = append(out, healthTest{Label: privSMTPLabel, Status: "critical", Badge: "Performance",
			Description: privEndpointText(privSMTPNoticeFmt, h),
			Action:      `<a href="/wp-admin/admin?page=wp-mail-smtp">Pursae SMTP settings</a>`})
	}
	if p := s.privatePurgeURL(ctx); p != "" {
		out = append(out, healthTest{Label: privPurgeLabel, Status: "recommended", Badge: "Performance",
			Description: privEndpointText(privPurgeNoticeFmt, p),
			Action:      `<a href="/wp-admin/admin?page=wpgo-cache-settings">Cache Settings</a>`})
	}
	return out
}
