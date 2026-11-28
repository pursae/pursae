package server

import (
	"crypto/tls"
	"net"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"wpgo/internal/plugins"
	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// A comment author's URL is shown as a link, so only http and https URLs may be stored or rendered.
func TestCommentAuthorURL(t *testing.T) {
	ok := map[string]string{
		"https://example.com/a?b=1": "https://example.com/a?b=1",
		"http://example.com":        "http://example.com",
		"example.com/me":            "http://example.com/me", // like the comment form: a missing scheme becomes http://
		"  https://example.com  ":   "https://example.com",
		"":                          "",
		"example.com:8080/x":        "http://example.com:8080/x",
	}
	for in, want := range ok {
		if got := commentAuthorURL(in); got != want {
			t.Errorf("commentAuthorURL(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		"javascript:alert(1)", "JaVaScRiPt:alert(1)", "java\tscript:alert(1)", "java\nscript:alert(1)", "vbscript:x",
		"data:text/html;base64,PHNjcmlwdD4=", "//evil.example", "http://", "https://exa mple.com", "http://example.com/\x00",
		"ftp://example.com/", "mailto:a@b.example", "http://[::1", "http:///x",
	} {
		if got := commentAuthorURL(in); got != "" {
			t.Errorf("commentAuthorURL(%q) = %q, want it dropped", in, got)
		}
	}
}

// update_option must stay inside the plugin's own options. A slug that equals the first word of a core
// option must not reach that option.
func TestPluginOptionAllowed(t *testing.T) {
	allow := []struct{ slug, name string }{
		{"hello-gopher", "hello_gopher_count"}, {"hello-gopher", "plugin_hello_gopher_greeting"},
		{"word-counter", "word_counter_total"}, {"default", "default_greeting"}, {"allow", "plugin_allow_mine"},
	}
	for _, c := range allow {
		if !pluginOptionAllowed(c.slug, c.name) {
			t.Errorf("pluginOptionAllowed(%q, %q) = false, want true", c.slug, c.name)
		}
	}
	deny := []struct{ slug, name string }{
		{"default", "default_role"}, {"users", "users_can_register"}, {"plugin-allow", "plugin_allow_unsigned"},
		{"allow", "plugin_allow_unsigned"}, {"trusted", "plugin_trusted_keys"}, {"wpgo", "wpgo_hub_url"}, {"wpgo", "wpgo_hub_trust_keys"},
		{"auth", "auth_secret"}, {"wpgo", "wpgo_plugin_note"}, {"native", webbuilder.PluginOption}, {"hello-gopher", "default_role"}, {"hello-gopher", "blogname"},
		{"blogname", "blogname"}, {"admin", "admin_email"}, {"date", "date_format"}, {"time", "time_format"}, {"start", "start_of_week"},
	}
	for _, c := range deny {
		if pluginOptionAllowed(c.slug, c.name) {
			t.Errorf("pluginOptionAllowed(%q, %q) = true, want false", c.slug, c.name)
		}
	}
}

func clientIPFor(t *testing.T, spec, remote, xff string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := applyTrustedProxies(r, spec); err != nil {
		t.Fatal(err)
	}
	r.GET("/", func(c *gin.Context) { c.String(200, clientIP(c)) })
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = remote
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Body.String()
}

// Behind a proxy on a private address the visitor's address is used, so one visitor's failed logins
// do not lock out everybody; a client that reaches the server directly cannot choose its own address.
func TestClientIPTrustedProxies(t *testing.T) {
	cases := []struct{ name, spec, remote, xff, want string }{
		{"proxy on a private address", "", "10.0.0.2:5555", "203.0.113.9", "203.0.113.9"},
		{"proxy on loopback", "", "127.0.0.1:5555", "203.0.113.9", "203.0.113.9"},
		{"chain, the proxy appends the client", "", "172.18.0.2:5555", "198.51.100.77, 203.0.113.9", "203.0.113.9"},
		{"loopback proxy: a spoofed leftmost value is ignored", "", "127.0.0.1:5555", "203.0.113.200, 198.51.100.7", "198.51.100.7"},
		{"spoofed private hop before the real client", "", "127.0.0.1:5555", "10.9.9.9, 198.51.100.7, 172.18.0.3", "198.51.100.7"},
		{"none: XFF ignored from loopback", "none", "127.0.0.1:5555", "203.0.113.9", "127.0.0.1"},
		{"env list replaces the default", "192.0.2.10", "127.0.0.1:5555", "203.0.113.9", "127.0.0.1"},
		{"direct public client cannot spoof", "", "198.51.100.7:5555", "203.0.113.9", "198.51.100.7"},
		{"no header", "", "10.0.0.2:5555", "", "10.0.0.2"},
		{"none trusts nobody", "none", "10.0.0.2:5555", "203.0.113.9", "10.0.0.2"},
		{"explicit list, listed proxy", "198.51.100.0/24", "198.51.100.7:5555", "203.0.113.9", "203.0.113.9"},
		{"explicit list, unlisted private peer", "198.51.100.0/24", "10.0.0.2:5555", "203.0.113.9", "10.0.0.2"},
		{"garbage header from a trusted proxy", "", "10.0.0.2:5555", "not-an-ip", "10.0.0.2"},
		{"all hops trusted: the leftmost", "", "10.0.0.2:5555", "192.168.1.5, 10.0.0.9", "192.168.1.5"},
		{"bare IP in the list", "10.0.0.2", "10.0.0.2:5555", "203.0.113.9", "203.0.113.9"},
		{"IPv6 proxy", "", "[::1]:5555", "2001:db8::7", "2001:db8::7"},
	}
	for _, c := range cases {
		if got := clientIPFor(t, c.spec, c.remote, c.xff); got != c.want {
			t.Errorf("%s: clientIP = %q, want %q", c.name, got, c.want)
		}
		// The handler-level gates (site block list, maintenance whitelist) must resolve the same address.
		nets, err := trustedProxyNets(c.spec)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := (&Server{trustedNets: nets}).requestIP(req); got != c.want {
			t.Errorf("%s: requestIP = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRequestHTTPSTrustsForwardedProtoOnlyFromProxies(t *testing.T) {
	nets, err := trustedProxyNets("")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{trustedNets: nets}
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.RemoteAddr = "198.51.100.7:5555"
	if s.requestHTTPS(req) {
		t.Error("an untrusted client chose https")
	}
	req.RemoteAddr = "10.0.0.2:5555"
	if !s.requestHTTPS(req) {
		t.Error("a trusted proxy's https was ignored")
	}
	req.Header.Set("X-Forwarded-Proto", "HTTPS")
	if s.requestHTTPS(req) {
		t.Error("an upper-case proto was trusted")
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	none, err := trustedProxyNets("none")
	if err != nil {
		t.Fatal(err)
	}
	s.trustedNets = none
	if s.requestHTTPS(req) {
		t.Error(`"none" still trusted the header`)
	}
}

func TestWebhookURLRejectsPrivateTargets(t *testing.T) {
	for _, u := range []string{"javascript:alert(1)", "http://127.0.0.1/hook", "https://169.254.169.254/", "https://user@hooks.example/x", "ftp://hooks.example/"} {
		if err := msWebhookURL(u); err == nil {
			t.Errorf("msWebhookURL(%q) accepted", u)
		}
	}
	if err := msWebhookURL("https://hooks.slack.com/services/T/B/x"); err != nil {
		t.Errorf("public webhook: %v", err)
	}
}

func TestApplyTrustedProxiesRejectsGarbage(t *testing.T) {
	if err := applyTrustedProxies(gin.New(), "10.0.0.0/8, not-a-cidr"); err == nil {
		t.Error("an unparsable entry must be an error, not silently ignored")
	}
	if _, err := trustedProxyNets("10.0.0.0/8, not-a-cidr"); err == nil {
		t.Error("trustedProxyNets accepts what gin refuses")
	}
}

// The outbound fetchers (feeds, WXR attachments, plugin http_get) share one deny list.
func TestPublicIPPolicyIsShared(t *testing.T) {
	for _, s := range []string{"100.100.100.200", "100.64.0.1", "100.127.255.254", "0.1.2.3", "255.255.255.255", "127.0.0.1", "10.1.2.3", "172.16.0.9", "192.168.1.1", "169.254.169.254", "::1", "fe80::1", "fc00::1", "224.0.0.1"} {
		ip := net.ParseIP(s)
		if publicIP(ip) || wxrPublicIP(ip) {
			t.Errorf("%s must not be a public address (publicIP %v, wxrPublicIP %v)", s, publicIP(ip), wxrPublicIP(ip))
		}
	}
	for _, s := range []string{"93.184.216.34", "8.8.8.8", "2606:4700:4700::1111", "100.63.255.255", "100.128.0.1"} {
		if ip := net.ParseIP(s); !publicIP(ip) || !wxrPublicIP(ip) {
			t.Errorf("%s should be a public address", s)
		}
	}
}

// Meta keys starting with an underscore are protected: a plugin must not write the page builder's marker
// or any other trusted core value through insert_post and update_post.
func TestPluginPostMetaRefusesProtectedKeys(t *testing.T) {
	for _, k := range []string{webbuilder.PostMarkerMeta, webbuilder.UserPrefsMeta, "_wp_page_template", "_thumbnail_id"} {
		p := &store.Post{}
		if err := applyPostInput(p, plugins.PostInput{Meta: map[string]any{k: "x"}}); err == nil {
			t.Errorf("meta key %q was accepted", k)
		}
		if err := applyPostInput(p, plugins.PostInput{Meta: map[string]any{k: nil}}); err == nil {
			t.Errorf("deleting meta key %q was accepted", k)
		}
	}
	p := &store.Post{}
	if err := applyPostInput(p, plugins.PostInput{Meta: map[string]any{"my_plugin_note": "hello"}}); err != nil || p.Meta["my_plugin_note"] != "hello" {
		t.Errorf("an ordinary meta key stopped working: %v %v", err, p.Meta)
	}
}

func TestHTTPSRequestGatesForwardedProto(t *testing.T) {
	nets, err := trustedProxyNets(defaultTrustedProxies)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{trustedNets: nets}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	r.Header.Set("X-Forwarded-Proto", "https")
	if s.httpsRequest(r) {
		t.Error("an untrusted peer turned on Secure with X-Forwarded-Proto")
	}
	r.RemoteAddr = "127.0.0.1:1234"
	if !s.httpsRequest(r) {
		t.Error("a trusted proxy's X-Forwarded-Proto: https was ignored")
	}
	r.Header.Set("X-Forwarded-Proto", "http")
	if s.httpsRequest(r) {
		t.Error("X-Forwarded-Proto: http was treated as https")
	}
	r.Header.Del("X-Forwarded-Proto")
	r.TLS = &tls.ConnectionState{}
	if !s.httpsRequest(r) {
		t.Error("a TLS request was not treated as https")
	}
}
