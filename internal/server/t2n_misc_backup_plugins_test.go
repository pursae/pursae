package server

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"wpgo/internal/plugins"
)

func t2nSignedPlugin(t *testing.T, slug string, perms []string, priv []byte) []byte {
	t.Helper()
	pj, _ := json.Marshal(perms)
	manifest := []byte(`{"slug": "` + slug + `", "name": "` + slug + `", "version": "1.0.0", "requires": "0.1.0", "permissions": ` + string(pj) + `}`)
	var buf bytes.Buffer
	if err := plugins.WritePackage(&buf, manifest, []byte("\x00asm\x01\x00\x00\x00"), nil, priv); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A restore verifies the restored plugin packages with this site's trusted keys (which the archive
// cannot change) and keeps a dangerous permission only when this site had granted it to that package.
func TestT2NMiscRestoreVetsPlugins(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	pool := srv.store.Pool

	pubA, privA, _ := plugins.GenerateKey()
	pubB, privB, _ := plugins.GenerateKey()
	setKeys := func(keys ...[]byte) {
		var text strings.Builder
		for _, k := range keys {
			text.WriteString(plugins.EncodeKey(k) + "\n")
		}
		if err := srv.settings.Set(ctx, map[string]any{"plugin_trusted_keys": text.String()}); err != nil {
			t.Fatal(err)
		}
		srv.applySiteKeys()
	}
	setKeys(pubA, pubB)
	install := func(slug string, priv []byte) {
		if _, err := srv.installPackageChecked(ctx, t2nSignedPlugin(t, slug, []string{"posts:read", "content:html"}, priv), pluginSourceUpload, "", true); err != nil {
			t.Fatalf("install %s: %v", slug, err)
		}
	}
	grant := func(slug string, perms ...string) {
		raw, _ := json.Marshal(perms)
		if _, err := pool.Exec(ctx, `UPDATE plugins SET permissions = $2 WHERE slug = $1`, slug, raw); err != nil {
			t.Fatal(err)
		}
	}
	install("t2n-alpha", privA) // content:html granted only in the archive
	install("t2n-beta", privA)  // content:html granted here and in the archive
	install("t2n-gamma", privB) // signed by a key this site stops trusting
	grant("t2n-alpha", "posts:read", "content:html")
	grant("t2n-beta", "posts:read", "content:html")

	cats := []string{"plugins", "settings"}
	var archive string
	bmgrRun(t, srv, "backup", func(j *bmgrJob) (err error) {
		archive, err = srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: cats, UserMode: "all"})
		return err
	})

	grant("t2n-alpha", "posts:read")
	setKeys(pubA)
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error {
		return srv.bmgrRunRestore(ctx, j, bmgrRestoreReq{File: archive, Categories: cats, Me: 1})
	})

	if got := parseTrustedKeys(srv.settings.String("plugin_trusted_keys")); len(got) != 1 || got[0].Key != plugins.EncodeKey(pubA) {
		t.Errorf("the archive's trusted keys replaced this site's: %+v", got)
	}
	perms := func(slug string) (granted, missing []string, status, reason string) {
		var g, m []byte
		if err := pool.QueryRow(ctx, `SELECT permissions, missing_permissions, status, disabled_reason FROM plugins WHERE slug = $1`, slug).Scan(&g, &m, &status, &reason); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(g, &granted)
		_ = json.Unmarshal(m, &missing)
		return
	}
	if g, m, _, _ := perms("t2n-alpha"); slices.Contains(g, "content:html") || !slices.Contains(m, "content:html") || !slices.Contains(g, "posts:read") {
		t.Errorf("t2n-alpha: a dangerous grant came back from the archive: granted %v, missing %v", g, m)
	}
	if g, _, _, _ := perms("t2n-beta"); !slices.Contains(g, "content:html") {
		t.Errorf("t2n-beta: a grant this site had given was dropped: %v", g)
	}
	if _, _, status, reason := perms("t2n-gamma"); status != "inactive" || !strings.Contains(reason, "could not be verified") {
		t.Errorf("t2n-gamma: an untrusted restored package was not deactivated: %s %q", status, reason)
	}
}
