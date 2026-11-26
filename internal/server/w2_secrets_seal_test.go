package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"wpgo/internal/config"
)

// sealSecret / revealSecret: sealed values open, plain (legacy) values pass through, a sealed value is
// not sealed twice, and a backup restore re-encrypts them for a site with another secret.
func TestW2SecretsSealHelper(t *testing.T) {
	a := &Server{cfg: config.Config{Secret: "secret-of-site-a"}}
	b := &Server{cfg: config.Config{Secret: "secret-of-site-b"}}
	sealed := a.sealSecret("sk_live_123")
	if !strings.HasPrefix(sealed, secretSealPrefix) || strings.Contains(sealed, "sk_live_123") {
		t.Fatalf("not sealed: %q", sealed)
	}
	if a.revealSecret(sealed) != "sk_live_123" {
		t.Errorf("does not open: %q", a.revealSecret(sealed))
	}
	if a.sealSecret(sealed) != sealed || a.sealSecret("") != "" {
		t.Error("a sealed or empty value was sealed again")
	}
	if a.revealSecret("legacy-plain") != "legacy-plain" {
		t.Error("a plain legacy value does not pass through")
	}
	if b.revealSecret(sealed) != "" {
		t.Error("opened under another secret")
	}
	// A gateway settings row as an archive carries it.
	raw, _ := json.Marshal(map[string]any{"key": "goocommerce_goo_stripe_settings", "value": map[string]any{"secret_key": sealed, "enabled": "yes"}})
	r := &bmgrResealer{from: "secret-of-site-a", to: "secret-of-site-b"}
	var row struct {
		Value map[string]string `json:"value"`
	}
	if err := json.Unmarshal([]byte(r.line(string(raw))), &row); err != nil {
		t.Fatal(err)
	}
	if r.n != 1 || b.revealSecret(row.Value["secret_key"]) != "sk_live_123" {
		t.Errorf("re-encrypted %d values; opens on site b as %q", r.n, b.revealSecret(row.Value["secret_key"]))
	}
}

// w2sRaw is the stored (database) form of an option, or of one key of a JSON option.
func w2sRaw(srv *Server, key string, path ...string) string {
	v := srv.settings.Get(key)
	for _, p := range path {
		m, _ := v.(map[string]any)
		v = m[p]
	}
	s, _ := v.(string)
	return s
}

func w2sCheckSealed(t *testing.T, what, stored, clear string) {
	t.Helper()
	if !strings.HasPrefix(stored, secretSealPrefix) || strings.Contains(stored, clear) {
		t.Errorf("%s is stored in clear: %q", what, stored)
	}
}

// w2sCapture is an outbound transport that records requests and answers 200 {}.
type w2sCapture struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func (w *w2sCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	w.mu.Lock()
	w.reqs, w.body = append(w.reqs, r), append(w.body, string(b))
	w.mu.Unlock()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
}

// Each secret is stored sealed, is used in clear where it is needed, is never shown back, and a
// plain value from an older version still works and is sealed at start-up.
func TestW2SecretsStoredSealed(t *testing.T) {
	base, admin, srv := gooSite(t)
	ctx := context.Background()

	// Stripe: secret keys and webhook secrets.
	gooStripeEnable(t, srv)
	key := gooOptPrefix + gooStripeID + "_settings"
	w2sCheckSealed(t, "the Stripe secret key", w2sRaw(srv, key, "test_secret_key"), "sk_test_fake")
	w2sCheckSealed(t, "the Stripe webhook secret", w2sRaw(srv, key, "test_webhook_secret"), "whsec_fake")
	if srv.gooStripeSecret() != "sk_test_fake" || srv.gooStripeWebhookSecret() != "whsec_fake" {
		t.Errorf("Stripe reads %q / %q", srv.gooStripeSecret(), srv.gooStripeWebhookSecret())
	}
	js, _ := json.Marshal(srv.gooV3GatewayJSON(ctx, srv.gooGateway(gooStripeID), 0))
	if strings.Contains(string(js), "sk_test_fake") || strings.Contains(string(js), "whsec_fake") || strings.Contains(string(js), secretSealPrefix) {
		t.Errorf("the payment gateway REST answer carries a secret: %s", js)
	}
	// Saving the form again (secret fields left empty) keeps the keys.
	_, page, _ := get(t, admin, base+"/wp-admin/goocommerce?page=settings&tab=checkout&section=goo_stripe")
	if strings.Contains(page, secretSealPrefix) {
		t.Error("the Stripe settings page shows a sealed value")
	}

	// Contact Form captcha keys (also used by the login CAPTCHA and Web Builder forms).
	if err := srv.gontactSaveOptions(ctx, gontactOptions{RecaptchaSitekey: "rc-site", RecaptchaSecret: "rc-secret-value"}); err != nil {
		t.Fatal(err)
	}
	w2sCheckSealed(t, "the reCAPTCHA secret", w2sRaw(srv, "gontact_recaptcha_secret"), "rc-secret-value")
	if p, ok := srv.captchaProvider(); !ok || p.secret != "rc-secret-value" {
		t.Errorf("captcha provider: %+v %v", p, ok)
	}
	capt := &w2sCapture{}
	oldCaptcha := captchaClient
	captchaClient = &http.Client{Transport: capt}
	t.Cleanup(func() { captchaClient = oldCaptcha })
	p, _ := srv.captchaProvider()
	srv.verifyCaptcha(ctx, p.endpoint, p.secret, "tok", "203.0.113.1")
	if len(capt.body) != 1 || !strings.Contains(capt.body[0], "secret=rc-secret-value") {
		t.Errorf("siteverify got %v", capt.body)
	}

	// Pursae Pixel API tokens.
	st := srv.pixelLoad()
	st.Platforms["tiktok"] = pixelPlatformCfg{Enabled: true, IDs: []string{"C1ABCDEFGHIJKLMNOPQR"}, Token: "tt-token-123", ServerSide: true}
	st.Platforms["ga4"] = pixelPlatformCfg{Enabled: true, IDs: []string{"G-ABCD1234"}, Token: "ga4-api-secret", ServerSide: true}
	if err := srv.pixelSave(ctx, st); err != nil {
		t.Fatal(err)
	}
	w2sCheckSealed(t, "the TikTok token", w2sRaw(srv, pixelOptions, "platforms", "tiktok", "token"), "tt-token-123")
	pix := &w2sCapture{}
	oldPixel := pixelHTTP
	pixelHTTP = &http.Client{Transport: pix}
	t.Cleanup(func() { pixelHTTP = oldPixel })
	srv.pixelSendTest(ctx, "tiktok")
	srv.pixelSendTest(ctx, "ga4")
	if len(pix.reqs) != 2 || pix.reqs[0].Header.Get("Access-Token") != "tt-token-123" || pix.reqs[1].URL.Query().Get("api_secret") != "ga4-api-secret" {
		t.Errorf("pixel deliveries: %d requests", len(pix.reqs))
	}

	// Forum Google sign-in client secret.
	fs := srv.forumLoad()
	fs.GoogleEnabled, fs.GoogleClientID, fs.GoogleClientSecret = true, "cid.apps.googleusercontent.com", "g-client-secret"
	if err := srv.forumSave(ctx, fs); err != nil {
		t.Fatal(err)
	}
	w2sCheckSealed(t, "the Google client secret", w2sRaw(srv, forumOptions, "google_client_secret"), "g-client-secret")
	fs.GoogleClientSecret = "" // a save without the secret keeps it
	if err := srv.forumSave(ctx, fs); err != nil {
		t.Fatal(err)
	}
	goo := &w2sCapture{}
	oldGoogle := forumGoogleHTTP
	forumGoogleHTTP = &http.Client{Transport: goo}
	t.Cleanup(func() { forumGoogleHTTP = oldGoogle })
	_, _ = srv.forumGoogleExchange(ctx, &forumOAuthState{V: "verifier"}, "code")
	if len(goo.body) == 0 || !strings.Contains(goo.body[0], "client_secret=g-client-secret") {
		t.Errorf("token exchange got %v", goo.body)
	}

	// License key: the form stores it sealed, shows it masked, and the masked value sent back keeps it.
	_, body, _ := get(t, admin, base+"/wp-admin/options-license")
	if code, _, _ := post(t, admin, base+"/wp-admin/options-license", url.Values{"_wpnonce": {formNonce(t, body)}, licenseOption: {"PRS-AAAA-BBBB-CCCC-DDDD"}}); code != http.StatusFound {
		t.Fatalf("license save: %d", code)
	}
	w2sCheckSealed(t, "the license key", w2sRaw(srv, licenseOption), "PRS-AAAA-BBBB-CCCC-DDDD")
	if srv.licenseKey() != "PRS-AAAA-BBBB-CCCC-DDDD" {
		t.Errorf("license key reads %q", srv.licenseKey())
	}
	_, body, _ = get(t, admin, base+"/wp-admin/options-license")
	if strings.Contains(body, "PRS-AAAA-BBBB-CCCC-DDDD") || strings.Contains(body, secretSealPrefix) || !strings.Contains(body, gfMask("PRS-AAAA-BBBB-CCCC-DDDD")) {
		t.Error("the License screen shows the key in clear (or not masked)")
	}
	_, _, _ = post(t, admin, base+"/wp-admin/options-license", url.Values{"_wpnonce": {formNonce(t, body)}, licenseOption: {gfMask("PRS-AAAA-BBBB-CCCC-DDDD")}})
	if srv.licenseKey() != "PRS-AAAA-BBBB-CCCC-DDDD" {
		t.Errorf("the masked key sent back replaced the key: %q", srv.licenseKey())
	}

	// Plain values from an older version: still read, then sealed by sealStoredSecrets (start-up).
	pm := srv.settings.Get(pixelOptions).(map[string]any)
	pm["platforms"].(map[string]any)["tiktok"].(map[string]any)["token"] = "tt-legacy"
	fm := srv.settings.Get(forumOptions).(map[string]any)
	fm["google_client_secret"] = "g-legacy"
	if err := srv.settings.Set(ctx, map[string]any{
		key:                         map[string]any{"enabled": "yes", "testmode": "yes", "test_secret_key": "sk_test_legacy", "test_webhook_secret": "whsec_legacy"},
		"gontact_turnstile_sitekey": "ts-site", "gontact_turnstile_secret": "ts-legacy",
		licenseOption: "PRS-LEGACY", pixelOptions: pm, forumOptions: fm,
	}); err != nil {
		t.Fatal(err)
	}
	if srv.gooStripeSecret() != "sk_test_legacy" || srv.licenseKey() != "PRS-LEGACY" {
		t.Errorf("legacy values read as %q / %q", srv.gooStripeSecret(), srv.licenseKey())
	}
	if p, _ := srv.captchaProvider(); p.secret != "ts-legacy" {
		t.Errorf("legacy captcha secret reads %q", p.secret)
	}
	srv.sealStoredSecrets(ctx)
	w2sCheckSealed(t, "legacy Stripe key", w2sRaw(srv, key, "test_secret_key"), "sk_test_legacy")
	w2sCheckSealed(t, "legacy webhook secret", w2sRaw(srv, key, "test_webhook_secret"), "whsec_legacy")
	w2sCheckSealed(t, "legacy Turnstile secret", w2sRaw(srv, "gontact_turnstile_secret"), "ts-legacy")
	w2sCheckSealed(t, "legacy license key", w2sRaw(srv, licenseOption), "PRS-LEGACY")
	w2sCheckSealed(t, "legacy pixel token", w2sRaw(srv, pixelOptions, "platforms", "tiktok", "token"), "tt-legacy")
	w2sCheckSealed(t, "legacy Google secret", w2sRaw(srv, forumOptions, "google_client_secret"), "g-legacy")
	if srv.gooStripeSecret() != "sk_test_legacy" || srv.gooStripeWebhookSecret() != "whsec_legacy" || srv.licenseKey() != "PRS-LEGACY" {
		t.Errorf("after sealing: %q / %q / %q", srv.gooStripeSecret(), srv.gooStripeWebhookSecret(), srv.licenseKey())
	}
	if p, _ := srv.captchaProvider(); p.secret != "ts-legacy" {
		t.Errorf("sealed captcha secret reads %q", p.secret)
	}
	if srv.forumLoad().GoogleClientSecret != "g-legacy" {
		t.Error("sealed Google secret does not open")
	}
	if srv.gooSealGatewaySecrets("cod_settings", map[string]any{"title": "x"}).(map[string]any)["title"] != "x" {
		t.Error("a gateway without secrets was changed")
	}
}

// A backup restored onto a site with another secret keeps the new sealed secrets usable, and plain
// secrets in an archive from an older version are sealed by the restore.
func TestW2SecretsRestoreOtherSecret(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	if srv.cfg.Secret != "" {
		t.Skip("PURSAE_SECRET is set for the tests")
	}
	gooStripeEnable(t, srv)
	if err := srv.settings.Set(ctx, map[string]any{licenseOption: srv.sealSecret("PRS-RESTORE"), "gontact_recaptcha_secret": "rc-plain-old"}); err != nil {
		t.Fatal(err)
	}
	var archive string
	bmgrRun(t, srv, "backup", func(j *bmgrJob) (err error) {
		archive, err = srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"settings"}, UserMode: "all"})
		return err
	})
	if err := srv.settings.Set(ctx, map[string]any{"auth_secret": "another-secret-for-this-site-w2s"}); err != nil {
		t.Fatal(err)
	}
	if srv.licenseKey() != "" {
		t.Fatal("a sealed value opened under another secret")
	}
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error {
		return srv.bmgrRunRestore(ctx, j, bmgrRestoreReq{File: archive, Categories: []string{"settings"}, Me: 1})
	})
	if srv.settings.String("auth_secret") != "another-secret-for-this-site-w2s" {
		t.Fatal("the restore replaced this site's secret")
	}
	if srv.gooStripeSecret() != "sk_test_fake" || srv.gooStripeWebhookSecret() != "whsec_fake" || srv.licenseKey() != "PRS-RESTORE" {
		t.Errorf("after the restore: %q / %q / %q", srv.gooStripeSecret(), srv.gooStripeWebhookSecret(), srv.licenseKey())
	}
	w2sCheckSealed(t, "a plain secret from the archive", w2sRaw(srv, "gontact_recaptcha_secret"), "rc-plain-old")
	if srv.revealSecret(srv.settings.String("gontact_recaptcha_secret")) != "rc-plain-old" {
		t.Error("the restored captcha secret does not open")
	}
}
