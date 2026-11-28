package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Headless webhook secrets and custom header values are stored sealed, read back in clear, and a
// plain row from an older version still works and is sealed at start-up.
func TestHeadlessWebhookSecretsSealed(t *testing.T) {
	_, _, srv := hgSite(t)
	ctx := context.Background()
	w := &hgWebhook{Name: "Deploy", URL: "https://hooks.example/build", Events: []string{"entry.create"}, Secret: "hook-s3cret",
		Headers: map[string]string{"Authorization": "Bearer api-token-123"}, Enabled: true}
	if err := srv.hgSaveWebhook(ctx, w); err != nil {
		t.Fatal(err)
	}
	var secret, headers string
	_ = srv.store.Pool.QueryRow(ctx, `SELECT secret, headers::text FROM hg_webhooks WHERE id = $1`, w.ID).Scan(&secret, &headers)
	if !strings.HasPrefix(secret, secretSealPrefix) || strings.Contains(headers, "api-token-123") || !strings.Contains(headers, secretSealPrefix) {
		t.Fatalf("stored in clear: secret %q headers %s", secret, headers)
	}
	got := srv.hgWebhookByID(ctx, w.ID)
	if got == nil || got.Secret != "hook-s3cret" || got.Headers["Authorization"] != "Bearer api-token-123" {
		t.Fatalf("read back: %+v", got)
	}

	// A legacy plain row reads as it is, then start-up seals it.
	var legacy int64
	if err := srv.store.Pool.QueryRow(ctx, `INSERT INTO hg_webhooks(name, url, events, headers, secret) VALUES ('Old', 'https://hooks.example/old', '{entry.create}', '{"Authorization":"Bearer old-token"}', 'old-secret') RETURNING id`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if o := srv.hgWebhookByID(ctx, legacy); o.Secret != "old-secret" || o.Headers["Authorization"] != "Bearer old-token" {
		t.Fatalf("legacy row: %+v", o)
	}
	srv.sealStoredSecrets(ctx)
	_ = srv.store.Pool.QueryRow(ctx, `SELECT secret, headers::text FROM hg_webhooks WHERE id = $1`, legacy).Scan(&secret, &headers)
	if strings.Contains(secret, "old-secret") || strings.Contains(headers, "old-token") {
		t.Fatalf("legacy row not sealed: %q %s", secret, headers)
	}
	if o := srv.hgWebhookByID(ctx, legacy); o.Secret != "old-secret" || o.Headers["Authorization"] != "Bearer old-token" {
		t.Fatalf("legacy row after sealing: %+v", o)
	}
}

// Commerce webhook secrets: sealed when saved (REST), legacy plain rows sealed at start-up, and the
// delivery is still signed with the clear secret.
func TestGooWebhookSecretsSealed(t *testing.T) {
	gooWebhookAllowPrivate.Store(true)
	defer gooWebhookAllowPrivate.Store(false)
	base, c, srv := gooSite(t)
	ctx := context.Background()
	var sig string
	var body []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		sig = r.Header.Get("X-WC-Webhook-Signature")
	}))
	defer hook.Close()

	v3 := gooAdminAPI(t, base, c)
	r := v3("POST", "/webhooks", map[string]any{"topic": "order.created", "delivery_url": hook.URL, "secret": "rest-s3cret"})
	if r.Code != 201 {
		t.Fatalf("create: %d %s", r.Code, r.Raw)
	}
	if strings.Contains(string(r.Raw), "rest-s3cret") {
		t.Error("the REST response shows the secret")
	}
	id := int64(gooInt(gooJ(r.Body, "id")))
	var stored string
	_ = srv.store.Pool.QueryRow(ctx, `SELECT secret FROM goo_webhooks WHERE id = $1`, id).Scan(&stored)
	if !strings.HasPrefix(stored, secretSealPrefix) {
		t.Fatalf("stored in clear: %q", stored)
	}
	// An update through REST keeps it sealed (and does not seal it twice).
	if r := v3("PUT", "/webhooks/"+itoa64(id), map[string]any{"name": "Renamed"}); r.Code != 200 {
		t.Fatalf("update: %d %s", r.Code, r.Raw)
	}
	if w, err := srv.gooWebhookByID(ctx, id); err != nil || w.Secret != "rest-s3cret" {
		t.Fatalf("read back: %+v %v", w, err)
	}
	if _, err := srv.gooWebhookPing(ctx, id); err != nil {
		t.Fatal(err)
	}
	if sig == "" || sig != gooWebhookSignature(body, "rest-s3cret") {
		t.Errorf("delivery signature %q does not match the clear secret", sig)
	}

	// Legacy plain row: works, then sealed at start-up, then still signs the same way.
	var legacy int64
	_ = srv.store.Pool.QueryRow(ctx, `INSERT INTO goo_webhooks (name, status, topic, delivery_url, secret) VALUES ('Old', 'active', 'order.created', $1, 'plain-old') RETURNING id`, hook.URL).Scan(&legacy)
	srv.sealStoredSecrets(ctx)
	_ = srv.store.Pool.QueryRow(ctx, `SELECT secret FROM goo_webhooks WHERE id = $1`, legacy).Scan(&stored)
	if !strings.HasPrefix(stored, secretSealPrefix) {
		t.Fatalf("legacy row not sealed: %q", stored)
	}
	sig = ""
	if _, err := srv.gooWebhookPing(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if sig != gooWebhookSignature(body, "plain-old") {
		t.Error("the sealed legacy secret no longer signs deliveries")
	}
}
