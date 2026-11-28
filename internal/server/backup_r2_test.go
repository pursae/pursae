package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wpgo/internal/config"
)

// The archive's sealed values are re-encrypted with the derivations the plugins use themselves.
func TestBmgrResealMatchesPlugins(t *testing.T) {
	a := &Server{cfg: config.Config{Secret: "secret-of-site-a"}}
	b := &Server{cfg: config.Config{Secret: "secret-of-site-b"}}
	sealed := map[string]string{
		"smtp": a.msSeal("smtp-pass"),
		"cdn":  a.gocdnSeal("cdn-secret"),
		"mt":   a.pllSealMTKey("mt-key"),
		"totp": a.asSeal("JBSWY3DPEHPK3PXP"),
	}
	raw, _ := json.Marshal(map[string]any{"key": "x", "value": sealed, "plain": "enc:v1:not-sealed"})
	r := &bmgrResealer{from: "secret-of-site-a", to: "secret-of-site-b"}
	var got struct {
		Value map[string]string `json:"value"`
		Plain string            `json:"plain"`
	}
	if err := json.Unmarshal([]byte(r.line(string(raw))), &got); err != nil {
		t.Fatal(err)
	}
	if r.n != 4 {
		t.Errorf("re-encrypted %d values, want 4", r.n)
	}
	if v := b.msReveal(got.Value["smtp"]); v != "smtp-pass" {
		t.Errorf("smtp: %q", v)
	}
	if v := b.gocdnReveal(got.Value["cdn"]); v != "cdn-secret" {
		t.Errorf("cdn: %q", v)
	}
	if v := b.pllRevealMTKey(got.Value["mt"]); v != "mt-key" {
		t.Errorf("mt: %q", v)
	}
	if v := b.asReveal(got.Value["totp"]); v != "JBSWY3DPEHPK3PXP" {
		t.Errorf("totp: %q", v)
	}
	if got.Plain != "enc:v1:not-sealed" {
		t.Errorf("a value that does not open was changed: %q", got.Plain)
	}
	// Another secret than the archive's: left as it is.
	other := &bmgrResealer{from: "wrong", to: "secret-of-site-b"}
	if line := other.line(string(raw)); line != string(raw) || other.n != 0 {
		t.Errorf("a value sealed under another secret was rewritten")
	}
}

// bmgrRewriteArchive copies an archive with edit applied to each file's bytes.
func bmgrRewriteArchive(t *testing.T, dir, from, to string, edit func(name string, data []byte) []byte) {
	t.Helper()
	src, err := zip.OpenReader(filepath.Join(dir, from))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range src.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		out := edit(f.Name, data)
		if f.Name == "manifest.json" { // an edited archive no longer matches its checksums
			var m map[string]any
			if json.Unmarshal(out, &m) == nil {
				delete(m, "checksums")
				out, _ = json.Marshal(m)
			}
		}
		_, _ = w.Write(out)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, to), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An archive of a site with logins and e-mails that differ only by case (made before migration 0048,
// which then left the columns non-unique) restores on a site that has the unique indexes; an archive
// without such accounts keeps them unique.
func TestBmgrRestoreCaseDuplicateUsers(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	pool := srv.store.Pool
	index := func(name string) bool {
		var ok bool
		_ = pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&ok)
		return ok
	}
	if !index("users_lower_login_key") || !index("users_lower_email_key") {
		t.Fatal("the test database lacks the 0048 unique indexes")
	}
	var archive string
	bmgrRun(t, srv, "backup", func(j *bmgrJob) (err error) {
		archive, err = srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"users"}, UserMode: "all"})
		return err
	})
	// A plain restore keeps the indexes unique.
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error {
		return srv.bmgrRunRestore(ctx, j, bmgrRestoreReq{File: archive, Categories: []string{"users"}, Me: 1})
	})
	if !index("users_lower_login_key") || !index("users_lower_email_key") || index("users_lower_login") {
		t.Errorf("after a plain restore: login_key %v email_key %v plain login %v", index("users_lower_login_key"), index("users_lower_email_key"), index("users_lower_login"))
	}

	dup := "case-dups.zip"
	bmgrRewriteArchive(t, srv.bmgrDir(), archive, dup, func(name string, data []byte) []byte {
		if name != "database/users.jsonl" {
			return data
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &row); err != nil {
			t.Fatal(err)
		}
		row["id"], row["login"], row["email"], row["nicename"] = 9001, strings.ToUpper(row["login"].(string)), strings.ToUpper(row["email"].(string)), "admin-upper"
		b, _ := json.Marshal(row)
		return append(data, append(b, '\n')...)
	})
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error {
		return srv.bmgrRunRestore(ctx, j, bmgrRestoreReq{File: dup, Categories: []string{"users"}, Me: 1})
	})
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(login) = 'admin'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("restored accounts named admin: %d %v", n, err)
	}
	if index("users_lower_login_key") || index("users_lower_email_key") || !index("users_lower_login") || !index("users_lower_email") {
		t.Errorf("after restoring case duplicates: login_key %v email_key %v plain login %v plain email %v",
			index("users_lower_login_key"), index("users_lower_email_key"), index("users_lower_login"), index("users_lower_email"))
	}
	// Restoring the clean archive again makes them unique again.
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error {
		return srv.bmgrRunRestore(ctx, j, bmgrRestoreReq{File: archive, Categories: []string{"users"}, Me: 1})
	})
	if !index("users_lower_login_key") || !index("users_lower_email_key") {
		t.Errorf("the unique indexes did not come back after a clean restore")
	}
}

// Secrets sealed on the archive's site open on this one after a restore: a site restored elsewhere keeps
// its mail password and its users' authenticator apps.
func TestBmgrRestoreResealsSecrets(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	if srv.cfg.Secret != "" {
		t.Skip("PURSAE_SECRET is set for the tests")
	}
	if err := srv.settings.Set(ctx, map[string]any{"r2_sealed_probe": srv.msSeal("smtp-pass")}); err != nil {
		t.Fatal(err)
	}
	var archive string
	bmgrRun(t, srv, "backup", func(j *bmgrJob) (err error) {
		archive, err = srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"settings"}, UserMode: "all", IncludeKey: true})
		return err
	})
	if m, err := bmgrReadManifest(filepath.Join(srv.bmgrDir(), archive)); err != nil || !m.SiteKey {
		t.Fatalf("an archive made with the key is not marked: %+v %v", m, err)
	}
	// This site gets a new secret (another server, a reinstall): the archive's value no longer opens.
	if err := srv.settings.Set(ctx, map[string]any{"auth_secret": "a-brand-new-secret-for-this-site"}); err != nil {
		t.Fatal(err)
	}
	if v := srv.msReveal(srv.settings.String("r2_sealed_probe")); v != "" {
		t.Fatalf("sealed value opened under another secret: %q", v)
	}
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error {
		return srv.bmgrRunRestore(ctx, j, bmgrRestoreReq{File: archive, Categories: []string{"settings"}, Me: 1})
	})
	if got := srv.settings.String("auth_secret"); got != "a-brand-new-secret-for-this-site" {
		t.Errorf("auth_secret after the restore: %q", got)
	}
	if v := srv.msReveal(srv.settings.String("r2_sealed_probe")); v != "smtp-pass" {
		t.Errorf("sealed value after the restore: %q", v)
	}
}

// By default an archive does not carry the site's encryption key: restored on a site with another key the
// sealed secrets read as empty (never an error), and the restore reports how many must be entered again.
func TestBmgrArchiveWithoutKey(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	if srv.cfg.Secret != "" {
		t.Skip("PURSAE_SECRET is set for the tests")
	}
	if err := srv.settings.Set(ctx, map[string]any{"r2_sealed_probe": srv.msSeal("smtp-pass"), "r2_sec_probe": srv.sealSecret("stripe-key")}); err != nil {
		t.Fatal(err)
	}
	var archive string
	bmgrRun(t, srv, "backup", func(j *bmgrJob) (err error) {
		archive, err = srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"settings"}, UserMode: "all"})
		return err
	})
	m, err := bmgrReadManifest(filepath.Join(srv.bmgrDir(), archive))
	if err != nil || m.SiteKey {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	zr, err := zip.OpenReader(filepath.Join(srv.bmgrDir(), archive))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "database/settings.jsonl" {
			if got := bmgrArchiveSecret(f); got != "" {
				t.Errorf("the archive carries auth_secret")
			}
		}
	}
	zr.Close()

	// The same site restores its own archive: nothing is unreadable.
	var job *bmgrJob
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error {
		job = j
		return srv.bmgrRunRestore(ctx, j, bmgrRestoreReq{File: archive, Categories: []string{"settings"}, Me: 1})
	})
	if job.Unreadable != 0 || srv.msReveal(srv.settings.String("r2_sealed_probe")) != "smtp-pass" {
		t.Errorf("same-site restore: %d unreadable, %q", job.Unreadable, srv.msReveal(srv.settings.String("r2_sealed_probe")))
	}

	// Another key (another server): the values stay sealed, read as empty, and the restore says so.
	if err := srv.settings.Set(ctx, map[string]any{"auth_secret": "a-brand-new-secret-for-this-site"}); err != nil {
		t.Fatal(err)
	}
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error {
		job = j
		return srv.bmgrRunRestore(ctx, j, bmgrRestoreReq{File: archive, Categories: []string{"settings"}, Me: 1})
	})
	if job.Unreadable < 2 || job.Warnings == 0 || !strings.Contains(strings.Join(job.Log, "\n"), "cannot be decrypted on this site") {
		t.Errorf("unreadable secrets not reported: %d, %d warnings\n%s", job.Unreadable, job.Warnings, strings.Join(job.Log, "\n"))
	}
	if v := srv.msReveal(srv.settings.String("r2_sealed_probe")); v != "" {
		t.Errorf("msReveal = %q", v)
	}
	if v := srv.revealSecret(srv.settings.String("r2_sec_probe")); v != "" {
		t.Errorf("revealSecret = %q", v)
	}
	// The status the screen polls carries the count.
	code, body, _ := get(t, c, ts.URL+"/wp-admin/admin?page=backup-manager&action=status&job="+job.ID)
	if code == http.StatusOK && !strings.Contains(body, `"unreadable":`) {
		t.Errorf("status lacks the unreadable count: %s", body)
	}
}

// The Create tab offers the key as an opt-in, in Turkish on a Turkish site.
func TestBmgrIncludeKeyOptionTurkish(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	if err := srv.settings.Set(ctx, map[string]any{"WPLANG": "tr_TR"}); err != nil {
		t.Fatal(err)
	}
	var id string
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error { id = j.ID; return nil })
	_, body, _ := get(t, c, ts.URL+"/wp-admin/admin?page=backup-manager&tab=create&job="+id)
	if !strings.Contains(body, `name="include_key" value="1">`) || strings.Contains(body, `name="include_key" value="1" checked`) {
		t.Error("the include-key box is missing or ticked by default")
	}
	for _, want := range []string{"Sitenin şifreleme anahtarını ekle (yeni bir sunucuya taşımak için)", "Yedekte bulunmayan eklenti veya tema dosyaları", "Bu sitede çözülemeyen kayıtlı parolalar"} {
		if !strings.Contains(body, want) {
			t.Errorf("create tab lacks %q", want)
		}
	}
}
