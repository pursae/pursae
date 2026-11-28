package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func setPrivEndpoints(t *testing.T, srv *Server, smtpHost, purgeURL string) {
	t.Helper()
	ctx := context.Background()
	if err := srv.settings.Set(ctx, map[string]any{
		msSetting: true,
		msOption: map[string]any{"mail": map[string]any{"mailer": "smtp"},
			"mailers": map[string]any{"smtp": map[string]any{"host": smtpHost, "port": "25"}}},
		"wpgo_cache_server":    "generic",
		"wpgo_cache_purge_url": purgeURL,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateEndpointNoticeAndSiteHealth(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	smtpMsg := "Email cannot be sent: the SMTP server 127.0.0.1 is on a private network. Set PURSAE_ALLOW_PRIVATE_ENDPOINTS=1 on the server to allow it."
	purgeMsg := "Cache purge requests cannot be sent: the purge URL http://10.0.0.5:6081 is on a private network."

	health := func() map[string]string {
		m := map[string]string{}
		for _, x := range srv.healthTests(ctx) {
			m[x.Label] = x.Status
		}
		return m
	}

	// Private host, no switch: notice on the dashboard and Site Health items.
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	setPrivEndpoints(t, srv, "127.0.0.1", "http://10.0.0.5:6081")
	_, body, _ := get(t, c, ts.URL+"/wp-admin/")
	if !strings.Contains(body, smtpMsg) || !strings.Contains(body, purgeMsg) {
		t.Fatalf("private endpoint notices missing from the dashboard")
	}
	h := health()
	if h[privSMTPLabel] != "critical" || h[privPurgeLabel] != "recommended" {
		t.Fatalf("site health items: %v / %v", h[privSMTPLabel], h[privPurgeLabel])
	}

	// With the switch: nothing.
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	_, body, _ = get(t, c, ts.URL+"/wp-admin/")
	if strings.Contains(body, "is on a private network") {
		t.Error("notice shown although PURSAE_ALLOW_PRIVATE_ENDPOINTS=1")
	}
	if h := health(); h[privSMTPLabel] != "" || h[privPurgeLabel] != "" {
		t.Error("site health item shown although PURSAE_ALLOW_PRIVATE_ENDPOINTS=1")
	}

	// Public hosts: nothing.
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	setPrivEndpoints(t, srv, "93.184.215.14", "http://93.184.215.14:6081")
	_, body, _ = get(t, c, ts.URL+"/wp-admin/")
	if strings.Contains(body, "is on a private network") {
		t.Error("notice shown for a public host")
	}
	if h := health(); h[privSMTPLabel] != "" || h[privPurgeLabel] != "" {
		t.Error("site health item shown for a public host")
	}
}

func TestPrivateHostResolvesAndCaches(t *testing.T) {
	old := privLookup
	t.Cleanup(func() { privLookup = old })
	calls := 0
	privLookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
		calls++
		switch host {
		case "mail.lan.test":
			return []net.IPAddr{{IP: net.ParseIP("192.168.1.10")}}, nil
		case "mail.public.test":
			return []net.IPAddr{{IP: net.ParseIP("93.184.215.14")}}, nil
		}
		return nil, errors.New("no such host")
	}
	ctx := context.Background()
	if !privateHost(ctx, "mail.lan.test") || !privateHost(ctx, "mail.lan.test") {
		t.Error("LAN name not reported")
	}
	if calls != 1 {
		t.Errorf("lookup not cached: %d calls", calls)
	}
	if privateHost(ctx, "mail.public.test") || privateHost(ctx, "nowhere.invalid.test") {
		t.Error("public or unresolvable name reported")
	}
	if privateHost(ctx, "169.254.169.254") {
		t.Error("link-local is refused always, not a private-network notice")
	}
}
