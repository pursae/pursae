package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
)

// Credentials of the built-in plugins kept in options are stored sealed (AES-256-GCM under a key
// derived from siteSecret(), like the SMTP and CDN credentials): the Contact Form reCAPTCHA and
// Turnstile secret keys, the Pursae Pixel server-side API tokens, the payment gateways' password
// fields (Stripe secret keys and webhook signing secrets), the Forum Google client secret, the
// license key, and the Commerce and Headless webhook secrets and custom header values (rows of
// goo_webhooks and hg_webhooks). Reading accepts a plain value (a site from before sealing, a value set by hand);
// sealStoredSecrets seals those once at start-up and after a restore. A backup restore onto a site
// with another secret re-encrypts them (bmgrSealKinds).

const (
	secretSealPrefix  = "sec:v1:"
	secretSealPurpose = "wpgo-secret|"
)

// secretLooksSealed reports whether v has the shape of a sealed value (prefix, base64, room for a
// nonce and a tag), so a re-saved sealed value is kept rather than sealed twice.
func secretLooksSealed(v string) bool {
	raw, ok := strings.CutPrefix(v, secretSealPrefix)
	if !ok {
		return false
	}
	data, err := base64.RawStdEncoding.DecodeString(raw)
	return err == nil && len(data) >= 12+16
}

// sealSecret encrypts a secret for storage; empty and already sealed values pass through.
func (s *Server) sealSecret(v string) string {
	if v == "" || secretLooksSealed(v) {
		return v
	}
	gcm := bmgrSealAEAD(secretSealPurpose, s.siteSecret())
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return secretSealPrefix + base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(v), nil))
}

// revealSecret returns a stored secret in clear: a plain (legacy) value as it is, a sealed one
// decrypted, "" when it does not decrypt (the server secret changed).
func (s *Server) revealSecret(v string) string {
	if !secretLooksSealed(v) {
		return v
	}
	data, _ := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(v, secretSealPrefix))
	gcm := bmgrSealAEAD(secretSealPurpose, s.siteSecret())
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		slog.Warn("a stored secret cannot be decrypted (was the server secret changed?)")
		return ""
	}
	return string(plain)
}

// sealStoredSecrets seals the plain secrets still stored in options (sites upgraded from a version
// that kept them in clear, an archive from such a site, a value set by hand).
func (s *Server) sealStoredSecrets(ctx context.Context) {
	if s.siteSecret() == "" {
		return
	}
	vals := map[string]any{}
	sealStr := func(v any) (any, bool) {
		if str, ok := v.(string); ok && str != "" && !secretLooksSealed(str) {
			return s.sealSecret(str), true
		}
		return v, false
	}
	for _, k := range []string{"gontact_recaptcha_secret", "gontact_turnstile_secret", licenseOption} {
		if v, ok := sealStr(s.settings.Get(k)); ok {
			vals[k] = v
		}
	}
	if m, ok := s.settings.Get(forumOptions).(map[string]any); ok {
		if v, changed := sealStr(m["google_client_secret"]); changed {
			m["google_client_secret"] = v
			vals[forumOptions] = m
		}
	}
	if m, ok := s.settings.Get(pixelOptions).(map[string]any); ok {
		platforms, _ := m["platforms"].(map[string]any)
		for _, p := range platforms {
			if pm, ok := p.(map[string]any); ok {
				if v, changed := sealStr(pm["token"]); changed {
					pm["token"] = v
					vals[pixelOptions] = m
				}
			}
		}
	}
	for id, g := range gooGatewayRegistry {
		key := gooOptPrefix + id + "_settings"
		m, ok := s.settings.Get(key).(map[string]any)
		if !ok {
			continue
		}
		for _, f := range g.FormFields() {
			if f.Type != "password" {
				continue
			}
			if v, changed := sealStr(m[f.ID]); changed {
				m[f.ID] = v
				vals[key] = m
			}
		}
	}
	s.sealWebhookSecrets(ctx)
	if len(vals) == 0 {
		return
	}
	if err := s.settings.Set(ctx, vals); err != nil {
		slog.Warn("sealing stored secrets", "err", err)
	}
}

// sealWebhookSecrets seals the plain webhook secrets left in goo_webhooks (Pursae Commerce) and
// hg_webhooks (Pursae Headless, also the custom header values such as an Authorization token).
// A row changed meanwhile is left for the next start (the UPDATE matches the value it read).
func (s *Server) sealWebhookSecrets(ctx context.Context) {
	type row struct {
		id     int64
		secret string
		hdr    []byte
	}
	read := func(q string, withHeaders bool) []row {
		rows, err := s.store.Pool.Query(ctx, q)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			dst := []any{&r.id, &r.secret}
			if withHeaders {
				dst = append(dst, &r.hdr)
			}
			if rows.Scan(dst...) == nil {
				out = append(out, r)
			}
		}
		return out
	}
	for _, r := range read(`SELECT id, secret FROM goo_webhooks WHERE secret <> '' AND secret NOT LIKE 'sec:v1:%'`, false) {
		if _, err := s.store.Pool.Exec(ctx, `UPDATE goo_webhooks SET secret = $2 WHERE id = $1 AND secret = $3`, r.id, s.sealSecret(r.secret), r.secret); err != nil {
			slog.Warn("sealing a Commerce webhook secret", "err", err)
		}
	}
	for _, r := range read(`SELECT id, secret, headers FROM hg_webhooks`, true) {
		hdr := map[string]string{}
		_ = json.Unmarshal(r.hdr, &hdr)
		plain := r.secret != "" && !secretLooksSealed(r.secret)
		for _, v := range hdr {
			plain = plain || v != "" && !secretLooksSealed(v)
		}
		if !plain {
			continue
		}
		if _, err := s.store.Pool.Exec(ctx, `UPDATE hg_webhooks SET secret = $2, headers = $3 WHERE id = $1 AND secret = $4 AND headers = $5::jsonb`,
			r.id, s.sealSecret(r.secret), s.hgSealHeaders(hdr), r.secret, r.hdr); err != nil {
			slog.Warn("sealing a Headless webhook secret", "err", err)
		}
	}
}
