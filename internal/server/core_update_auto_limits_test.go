package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"wpgo/internal/db"
)

func TestSameMinor(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.3.1", "0.3.2", true},
		{"0.3.1", "0.3.10-beta.1", true},
		{"v0.3.0", "0.3.4+build", true},
		{"0.3.9", "0.4.0", false},
		{"0.3.9", "1.3.9", false},
		{"dev", "0.3.1", false},
		{"0.3", "0.3.1", true},
		{"0", "0.0.1", false},
	} {
		if got := sameMinor(c.a, c.b); got != c.want {
			t.Errorf("sameMinor(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// autoUpdateSite is a site with automatic updates on whose Manager offers 9.9.9 for this platform,
// and whose executable is a temporary file (restarts are recorded, not made).
func autoUpdateSite(t *testing.T, running string) (srv *Server, target string) {
	t.Helper()
	savedVersion := Version
	Version = running
	t.Cleanup(func() { Version = savedVersion })
	withRootKeys(t)
	withTrustHubKeys(t, true)
	_, _, srv, hub := managerSite(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	hub.keys = append(hub.keys, map[string]string{"id": "hub-2026", "public": base64.StdEncoding.EncodeToString(pub)})
	bin := []byte("\x7fELF pursae 9.9.9")
	art := signedCore("9.9.9", runtime.GOOS, runtime.GOARCH, bin, priv, "hub-2026")
	art.URL = hub.URL + "/api/v1/core/9.9.9/" + runtime.GOOS + "-" + runtime.GOARCH
	hub.coreBin = bin
	hub.core["artifacts"] = []*coreArtifact{art}
	srv.hubDropCaches(ctx)
	if _, err := srv.hubFetchIndex(ctx); err != nil {
		t.Fatal(err)
	}

	target = filepath.Join(t.TempDir(), "pursae")
	os.WriteFile(target, []byte("old binary"), 0o755)
	oldExe, oldRestart := coreExecutable, coreRestart
	t.Cleanup(func() { coreExecutable, coreRestart = oldExe, oldRestart })
	coreRestart = func(string) error { return nil }
	coreExecutable = func() (string, error) { return target, nil }
	t.Cleanup(func() {
		for i := 0; i < 100 && coreBusy.Load(); i++ {
			time.Sleep(50 * time.Millisecond)
		}
	})
	if err := srv.settings.Set(ctx, map[string]any{coreAutoUpdateOption: "1"}); err != nil {
		t.Fatal(err)
	}
	return srv, target
}

// A new minor version waits for a click; the Updates screen says why.
func TestCoreAutoUpdateStaysWithinMinor(t *testing.T) {
	srv, target := autoUpdateSite(t, "9.8.5")
	ctx := context.Background()
	if v := srv.coreAutoUpdate(ctx); v != "" {
		t.Fatalf("9.8.5 → 9.9.9 was installed automatically: %q", v)
	}
	if b, _ := os.ReadFile(target); string(b) != "old binary" {
		t.Fatal("the executable was replaced")
	}
	last := srv.coreAutoLast()
	if last == nil || last.Version != "9.9.9" || last.Status != "manual" {
		t.Fatalf("not recorded as manual: %+v", last)
	}
	if n := len(srv.bmgrList()); n != 0 {
		t.Errorf("%d backups taken for an update that does not run", n)
	}
	if coreBusy.Load() {
		t.Error("coreBusy held")
	}
}

// A failed pre-update backup skips the update and is shown on the Updates screen.
func TestCoreAutoUpdateSkippedWhenBackupFails(t *testing.T) {
	srv, target := autoUpdateSite(t, "9.9.1")
	ctx := context.Background()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	os.WriteFile(blocker, nil, 0o600)
	t.Setenv("PURSAE_BACKUPS_DIR", filepath.Join(blocker, "backups")) // cannot be created
	if v := srv.coreAutoUpdate(ctx); v != "" {
		t.Fatalf("updated without a backup: %q", v)
	}
	if b, _ := os.ReadFile(target); string(b) != "old binary" {
		t.Fatal("the executable was replaced")
	}
	last := srv.coreAutoLast()
	if last == nil || last.Status != "skipped" || last.Error == "" {
		t.Fatalf("not recorded as skipped: %+v", last)
	}
	if coreBusy.Load() {
		t.Error("coreBusy held after the skipped update")
	}
	view := srv.coreAutoUpdateView(nil)
	if l, _ := view["Last"].(*coreAutoResult); l == nil || l.Status != "skipped" {
		t.Errorf("the Updates screen does not show the skipped update: %+v", view)
	}

	// Another backup still running counts as a failure too.
	t.Setenv("PURSAE_BACKUPS_DIR", "")
	j, err := srv.bmgrStart("backup")
	if err != nil {
		t.Fatal(err)
	}
	if v := srv.coreAutoUpdate(ctx); v != "" {
		t.Fatalf("updated while another backup ran: %q", v)
	}
	srv.bmgrFinish(j, nil)
	if last := srv.coreAutoLast(); last == nil || last.Status != "skipped" || !strings.Contains(last.Error, "still running") {
		t.Fatalf("not recorded as skipped: %+v", last)
	}
}

// Only the newest preUpdateBackupKeep pre-update backups stay; other archives are not touched.
func TestPreUpdateBackupRetention(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	ctx := context.Background()
	dir := srv.bmgrDir()
	os.MkdirAll(dir, 0o700)
	now := time.Now()
	for i := range 4 {
		bmgrFakeArchive(t, dir, "pre-update-old-"+string(rune('a'+i))+".zip", bmgrPreUpdateKind, now.Add(-time.Duration(i+1)*time.Hour))
	}
	bmgrFakeArchive(t, dir, "backup-manual.zip", "manual", now.Add(-48*time.Hour))
	name, err := srv.preUpdateBackup(ctx, "9.9.9", true)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, e := range srv.bmgrList() {
		if e.M != nil && e.M.Kind == bmgrPreUpdateKind {
			kept = append(kept, e.Name)
		}
	}
	if len(kept) != preUpdateBackupKeep || !strings.Contains(strings.Join(kept, ","), name) {
		t.Fatalf("kept %v, want the newest %d including %s", kept, preUpdateBackupKeep, name)
	}
	if _, err := os.Stat(filepath.Join(dir, "backup-manual.zip")); err != nil {
		t.Error("a manual backup was pruned")
	}
	if srv.bmgrOn() {
		t.Error("the backup turned the Backup Manager plugin on")
	}
}

// /readyz reports the schema guard: a newer but compatible schema is ready (and says so), a binary
// below the recorded floor is not.
func TestReadyzSchemaGuard(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	ctx := context.Background()
	h := srv.Handler()
	probe := func() (int, string) {
		srv.schemaGuard.mu.Lock()
		srv.schemaGuard.at = time.Time{} // skip the cache
		srv.schemaGuard.mu.Unlock()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := probe(); code != 200 || strings.TrimSpace(body) != "ok" {
		t.Fatalf("/readyz: %d %s", code, body)
	}
	if floor, err := db.MinBinary(ctx, srv.store.Pool); err != nil || floor != db.MinCompatibleBinary {
		t.Fatalf("the migrated test database records floor %q (%v)", floor, err)
	}

	// A newer release applied a migration this binary does not know.
	if _, err := srv.store.Pool.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ('9999_future')`); err != nil {
		t.Fatal(err)
	}
	if code, body := probe(); code != 200 || body != "ok" && !strings.HasPrefix(body, "ok") {
		t.Fatalf("/readyz with a newer, compatible schema: %d %s", code, body)
	}

	// ...and raised the floor above this binary (running only because of the override).
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE schema_meta SET value = '999.0.0' WHERE key = 'min_binary_version'`); err != nil {
		t.Fatal(err)
	}
	if code, body := probe(); code != http.StatusServiceUnavailable || strings.Contains(body, "999.0.0") || !strings.Contains(body, "not ready") {
		t.Fatalf("/readyz below the floor: %d %s", code, body)
	}
}
