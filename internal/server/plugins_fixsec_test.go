package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/url"
	"strings"
	"testing"

	"wpgo/internal/plugins"
)

// A permission revoked (or granted) on another replica is applied by the sync loop, not only
// after a restart.
func TestPluginSyncAppliesPermissionChanges(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	installDemo(t, srv)
	ctx := context.Background()
	pending := map[string]string{}
	if p, _ := srv.plugins.rt.Plugin("shortcode-demo"); !p.HasPermission(plugins.PermStorage) {
		t.Fatal("demo loaded without storage")
	}
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE plugins SET permissions = '["options:read"]' WHERE slug = 'shortcode-demo'`)
	srv.syncPluginsOnce(ctx, pending)
	srv.syncPluginsOnce(ctx, pending)
	p, ok := srv.plugins.rt.Plugin("shortcode-demo")
	if !ok {
		t.Fatal("unloaded instead of reloaded")
	}
	if got := p.Permissions(); len(got) != 1 || got[0] != "options:read" {
		t.Fatalf("permissions after revoke elsewhere: %v", got)
	}
	// Steady state: no reload loop.
	srv.syncPluginsOnce(ctx, pending)
	srv.syncPluginsOnce(ctx, pending)
	if again, _ := srv.plugins.rt.Plugin("shortcode-demo"); again != p {
		t.Fatal("reloaded although nothing changed")
	}
}

func TestPluginStorageQuota(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	installDemo(t, srv)
	ctx := context.Background()
	h := &pluginHost{s: srv}
	oldKeys, oldBytes := pluginStorageMaxKeys, pluginStorageMaxBytes
	t.Cleanup(func() { pluginStorageMaxKeys, pluginStorageMaxBytes = oldKeys, oldBytes })
	pluginStorageMaxKeys, pluginStorageMaxBytes = 3, 1000
	for _, k := range []string{"a", "b", "c"} {
		if err := h.StorageSet(ctx, "shortcode-demo", k, "v"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.StorageSet(ctx, "shortcode-demo", "d", "v"); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("4th key: %v", err)
	}
	if err := h.StorageSet(ctx, "shortcode-demo", "a", "updated"); err != nil {
		t.Fatalf("overwriting an existing key at the key limit: %v", err)
	}
	if err := h.StorageSet(ctx, "shortcode-demo", "b", strings.Repeat("x", 1000)); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("over the size limit: %v", err)
	}
}

func TestPluginKeysOperatorLock(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := srv.changeTrustedKeys(ctx, "add", plugins.EncodeKey(pub), "pre-lock", ""); err != nil {
		t.Fatal(err)
	}
	_, page, _ := get(t, c, ts.URL+"/wp-admin/plugin-keys")
	nonce := ""
	if i := strings.Index(page, `name="_wpnonce" value="`); i >= 0 {
		nonce = page[i+len(`name="_wpnonce" value="`):]
		nonce = nonce[:strings.Index(nonce, `"`)]
	}
	before := len(srv.plugins.trusted())
	t.Setenv("PURSAE_PLUGIN_KEYS", "locked")
	srv.applySiteKeys()
	if got := len(srv.plugins.trusted()); got != before-1 {
		t.Errorf("an admin-added key is still trusted under the lock: %d -> %d", before, got)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := srv.changeTrustedKeys(ctx, "add", plugins.EncodeKey(other), "", ""); err == nil {
		t.Fatal("key added while locked")
	}
	_, body, _ := get(t, c, ts.URL+"/wp-admin/plugin-keys")
	if strings.Contains(body, "Trust a new key") || !strings.Contains(body, "managed by the server operator") {
		t.Error("locked screen still offers to add keys")
	}
	code, _, loc := post(t, c, ts.URL+"/wp-admin/plugin-keys", url.Values{"_wpnonce": {nonce}, "action": {"add"}, "key": {plugins.EncodeKey(other)}})
	if code != 302 || !strings.Contains(loc, "error=") {
		t.Errorf("POST while locked: %d %s", code, loc)
	}
	t.Setenv("PURSAE_PLUGIN_KEYS", "")
	t.Setenv("PURSAE_PLUGIN_KEYS_LOCKED", "true")
	if !pluginKeysLocked() {
		t.Error("PURSAE_PLUGIN_KEYS_LOCKED=true not honoured")
	}
	t.Setenv("PURSAE_PLUGIN_KEYS_LOCKED", "")
	if pluginKeysLocked() {
		t.Error("locked by default")
	}
}
