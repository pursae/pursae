package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wpgo/internal/backupcrypt"
	"wpgo/internal/config"
	"wpgo/internal/gocdn/s3fake"
	"wpgo/internal/i18n"
)

// bmgrWaitJob polls a background job until it is no longer running.
func bmgrWaitJob(t *testing.T, s *Server, id string) *bmgrJob {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if j := s.bmgrJobByID(id); j != nil && j.State != "running" {
			return j
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s still running", id)
	return nil
}

// Remote storage end to end against an in-process S3 (signatures checked): settings saved from the
// screen with the secrets sealed and never shown again, the connection test, automatic encrypted
// upload with retention, download-and-restore, a wrong passphrase, REST and CLI, and no secret in
// any output.
func TestBmgrRemoteStorage(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	const (
		access = "AKIAPURSAETEST"
		secret = "s3cr3t-Key/With+Chars-0123456789"
		pass   = "correct horse battery staple"
	)
	fake := s3fake.New(access, secret, "eu-central-1", "site-backups")
	defer fake.Close()
	fake.MinPartSize = 1024
	old := bmgrRemotePartSize
	bmgrRemotePartSize = 4 << 10 // the archives below go up as multipart uploads
	defer func() { bmgrRemotePartSize = old }()
	base := ts.URL + "/wp-admin/admin?page=backup-manager"

	// Nothing configured: the tab renders, the Backups tab offers no upload, backups stay local.
	code, body, _ := get(t, c, base+"&tab=remote")
	if code != http.StatusOK || !strings.Contains(body, `name="endpoint"`) || strings.Contains(body, "Test connection") {
		t.Fatalf("remote tab unconfigured: %d", code)
	}
	nonce := formNonce(t, body)
	save := func(form url.Values) string {
		t.Helper()
		form.Set("_wpnonce", nonce)
		form.Set("do", "remote-save")
		code, _, loc := post(t, c, base, form)
		if code != http.StatusFound {
			t.Fatalf("save: %d", code)
		}
		return loc
	}
	form := func() url.Values {
		return url.Values{"endpoint": {fake.URL}, "region": {"eu-central-1"}, "bucket": {"site-backups"}, "prefix": {"/site-a/"},
			"path_style": {"1"}, "access_key": {access}, "secret_key": {secret}, "passphrase": {pass}, "passphrase2": {pass},
			"auto": {"1"}, "keep": {"2"}}
	}

	// SSRF: without PURSAE_BACKUP_S3_ALLOW_PRIVATE the loopback fake is refused before anything is stored.
	if loc := save(form()); !strings.Contains(loc, "bmgr-error") || !strings.Contains(loc, "PURSAE_BACKUP_S3_ALLOW_PRIVATE") {
		t.Fatalf("a loopback endpoint was accepted: %s", loc)
	}
	if srv.bmgrRemote().configured() {
		t.Fatal("refused settings were stored")
	}
	t.Setenv("PURSAE_BACKUP_S3_ALLOW_PRIVATE", "1")
	bad := form()
	bad.Set("passphrase2", "something else entirely")
	if loc := save(bad); !strings.Contains(loc, "bmgr-error") {
		t.Errorf("mismatched passphrases accepted: %s", loc)
	}
	short := form()
	short.Set("passphrase", "short")
	short.Set("passphrase2", "short")
	if loc := save(short); !strings.Contains(loc, "bmgr-error") {
		t.Errorf("a short passphrase accepted: %s", loc)
	}
	if loc := save(form()); !strings.Contains(loc, "bmgr-msg=remote-saved") {
		t.Fatalf("save: %s", loc)
	}

	// Stored sealed; never in the page.
	raw, _ := json.Marshal(srv.settings.Get(bmgrOptRemote))
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte(pass)) || !bytes.Contains(raw, []byte(bmgrRemoteSealPfx)) {
		t.Fatalf("secrets stored in plain text: %s", raw)
	}
	var dbRaw string
	_ = srv.store.Pool.QueryRow(ctx, `SELECT value::text FROM settings WHERE key = $1`, bmgrOptRemote).Scan(&dbRaw)
	if dbRaw == "" || strings.Contains(dbRaw, secret) || strings.Contains(dbRaw, pass) {
		t.Fatalf("database row: %q", dbRaw)
	}
	st := srv.bmgrRemote()
	if st.SecretKey != secret || st.Passphrase != pass || st.Prefix != "site-a/" || !st.Auto || st.Keep != 2 {
		t.Fatalf("effective settings: %+v", st)
	}
	_, body, _ = get(t, c, base+"&tab=remote")
	if strings.Contains(body, secret) || strings.Contains(body, pass) || !strings.Contains(body, "Test connection") {
		t.Fatal("the remote tab shows a secret or no test button")
	}
	// Saving again with empty secret fields keeps them.
	keepSecrets := form()
	keepSecrets.Del("secret_key")
	keepSecrets.Del("passphrase")
	keepSecrets.Del("passphrase2")
	if loc := save(keepSecrets); !strings.Contains(loc, "remote-saved") {
		t.Fatalf("save without secrets: %s", loc)
	}
	if st := srv.bmgrRemote(); st.SecretKey != secret || st.Passphrase != pass {
		t.Fatal("an empty secret field dropped the saved value")
	}

	// Connection test.
	code, _, loc := post(t, c, base, url.Values{"_wpnonce": {nonce}, "do": {"remote-test"}})
	if code != http.StatusFound || !strings.Contains(loc, "bmgr-msg=remote-ok") {
		t.Fatalf("connection test: %s", loc)
	}
	for _, k := range fake.Keys("site-backups") {
		if strings.Contains(k, bmgrTestKeyPrefix) {
			t.Errorf("the test object %s was left behind", k)
		}
	}

	// Three backups (two manual, one scheduled) upload automatically; only the newest two stay.
	var made []string
	for i := 0; i < 3; i++ {
		kind := ""
		if i == 1 {
			kind = bmgrScheduleKind
		}
		j, err := srv.bmgrStart("backup")
		if err != nil {
			t.Fatal(err)
		}
		name, err := srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"content", "settings"}, UserMode: "all", Kind: kind})
		srv.bmgrFinish(j, err)
		jj := srv.bmgrJobByID(j.ID)
		if err != nil || jj.RemoteError != "" || jj.Remote != "site-a/"+name+".enc" {
			t.Fatalf("backup %d: %v remote=%q err=%q\n%s", i, err, jj.Remote, jj.RemoteError, strings.Join(jj.Log, "\n"))
		}
		made = append(made, name)
		time.Sleep(1100 * time.Millisecond) // distinct LastModified
	}
	// A safety snapshot stays local.
	j, _ := srv.bmgrStart("backup")
	safety, err := srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"settings"}, UserMode: "all", Kind: "safety"})
	srv.bmgrFinish(j, err)
	if err != nil {
		t.Fatal(err)
	}
	keys := strings.Join(fake.Keys("site-backups"), ",")
	if strings.Contains(keys, made[0]) || !strings.Contains(keys, made[1]) || !strings.Contains(keys, made[2]) || strings.Contains(keys, safety) {
		t.Fatalf("bucket after retention: %s", keys)
	}
	obj := fake.Object("site-backups", "site-a/"+made[2]+".enc")
	local, _ := os.ReadFile(filepath.Join(srv.bmgrDir(), made[2]))
	if obj == nil || !backupcrypt.IsEncrypted(obj.Body) || bytes.Contains(obj.Body, []byte("manifest.json")) || obj.Parts == 0 {
		t.Fatalf("remote object not encrypted or not multipart")
	}
	var dec bytes.Buffer
	if _, err := backupcrypt.DecryptStream(&dec, bytes.NewReader(obj.Body), []byte(pass)); err != nil || !bytes.Equal(dec.Bytes(), local) {
		t.Fatalf("remote object does not decrypt to the archive: %v", err)
	}
	if fake.OpenUploads() != 0 || len(fake.Errors) != 0 {
		t.Fatalf("open uploads %d, signature errors %v", fake.OpenUploads(), fake.Errors)
	}

	// The tab lists them; the Backups tab offers Upload.
	_, body, _ = get(t, c, base+"&tab=remote")
	if !strings.Contains(body, made[2]+".enc") || !strings.Contains(body, "Download and restore") {
		t.Fatal("remote list missing")
	}
	if _, body, _ = get(t, c, base+"&tab=backups"); !strings.Contains(body, `value="remote-upload"`) {
		t.Error("the Backups tab offers no upload")
	}

	// Download and restore: the local copy is gone, the remote one comes back identical.
	if err := os.Remove(filepath.Join(srv.bmgrDir(), made[2])); err != nil {
		t.Fatal(err)
	}
	// A wrong passphrase fails and leaves nothing behind.
	code, _, loc = post(t, c, base, url.Values{"_wpnonce": {nonce}, "do": {"remote-object"}, "fetch": {made[2] + ".enc"}, "passphrase": {"not the passphrase at all"}})
	jobID := func(loc string) string {
		u, _ := url.Parse(loc)
		return u.Query().Get("job")
	}
	if code != http.StatusFound || jobID(loc) == "" {
		t.Fatalf("fetch: %d %s", code, loc)
	}
	if jj := bmgrWaitJob(t, srv, jobID(loc)); jj.State != "error" || !strings.Contains(jj.Error, "passphrase") {
		t.Fatalf("wrong passphrase: %+v", jj)
	}
	if _, err := os.Stat(filepath.Join(srv.bmgrDir(), made[2])); !os.IsNotExist(err) {
		t.Fatal("a wrong passphrase left a file")
	}
	_, _, loc = post(t, c, base, url.Values{"_wpnonce": {nonce}, "do": {"remote-object"}, "fetch": {made[2] + ".enc"}})
	jj := bmgrWaitJob(t, srv, jobID(loc))
	if jj.State != "done" || jj.File != made[2] {
		t.Fatalf("fetch: %+v", jj)
	}
	if back, _ := os.ReadFile(filepath.Join(srv.bmgrDir(), made[2])); !bytes.Equal(back, local) {
		t.Fatal("the downloaded archive differs")
	}
	if code, body, _ := get(t, c, base+"&tab=restore&file="+url.QueryEscape(made[2])); code != http.StatusOK || !strings.Contains(body, `name="confirm"`) {
		t.Fatalf("restore screen after the download: %d", code)
	}

	// Manual upload of a stored archive (the oldest, pruned earlier) as a job; retention removes it again.
	_, _, loc = post(t, c, base, url.Values{"_wpnonce": {nonce}, "do": {"remote-upload"}, "file": {made[0]}})
	if jj := bmgrWaitJob(t, srv, jobID(loc)); jj.State != "done" || jj.Remote == "" {
		t.Fatalf("manual upload: %+v", jj)
	}

	// Delete from the bucket.
	_, _, loc = post(t, c, base, url.Values{"_wpnonce": {nonce}, "do": {"remote-object"}, "delete": {made[1] + ".enc"}})
	if !strings.Contains(loc, "remote-deleted") || fake.Object("site-backups", "site-a/"+made[1]+".enc") != nil {
		t.Fatalf("delete: %s", loc)
	}

	// REST: settings without secrets, the list, and an upload job.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/wp-json/pursae/v1/backups/remote", nil)
	req.Header.Set("X-WP-Nonce", restNonce(t, srv, c, ts.URL))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var rest map[string]any
	restBody := new(bytes.Buffer)
	_, _ = restBody.ReadFrom(resp.Body)
	resp.Body.Close()
	_ = json.Unmarshal(restBody.Bytes(), &rest)
	if resp.StatusCode != http.StatusOK || rest["secret_key_set"] != true || rest["passphrase_set"] != true || rest["bucket"] != "site-backups" {
		t.Fatalf("REST: %d %s", resp.StatusCode, restBody)
	}
	if strings.Contains(restBody.String(), secret) || strings.Contains(restBody.String(), pass) {
		t.Fatal("REST shows a secret")
	}
	if list, _ := rest["backups"].([]any); len(list) == 0 {
		t.Errorf("REST lists no backups: %s", restBody)
	}

	// CLI: status hides secrets, list, push, pull, decrypt.
	cfg := config.Config{DatabaseURL: config.Env("TEST_DB"), UploadsDir: srv.cfg.UploadsDir, WebDir: srv.cfg.WebDir, HubURL: "off", Secret: srv.cfg.Secret}
	code, out, errs := runOpsCLI(t, cfg, "backup", "remote", "status", "--format=json")
	if code != 0 || strings.Contains(out+errs, secret) || strings.Contains(out+errs, pass) || !strings.Contains(out, `"secret_key":true`) {
		t.Fatalf("remote status: %d %q %q", code, out, errs)
	}
	if code, out, errs := runOpsCLI(t, cfg, "backup", "remote", "list", "--field=name"); code != 0 || !strings.Contains(out, made[2]+".enc") {
		t.Fatalf("remote list: %d %q %q", code, out, errs)
	}
	if code, _, errs := runOpsCLI(t, cfg, "backup", "remote", "test"); code != 0 {
		t.Fatalf("remote test: %d %q", code, errs)
	}
	if code, _, errs := runOpsCLI(t, cfg, "backup", "remote", "push", made[1]); code != 0 {
		t.Fatalf("remote push: %d %q", code, errs)
	}
	os.Remove(filepath.Join(srv.bmgrDir(), made[1]))
	if code, out, errs := runOpsCLI(t, cfg, "backup", "remote", "pull", made[1]+".enc"); code != 0 || !strings.Contains(out+errs, made[1]) {
		t.Fatalf("remote pull: %d %q %q", code, out, errs)
	}
	if _, err := bmgrReadManifest(filepath.Join(srv.bmgrDir(), made[1])); err != nil {
		t.Fatalf("pulled archive: %v", err)
	}
	encFile := filepath.Join(t.TempDir(), made[2]+".enc")
	_ = os.WriteFile(encFile, obj.Body, 0o600)
	passFile := filepath.Join(t.TempDir(), "pass.txt")
	_ = os.WriteFile(passFile, []byte(pass+"\n"), 0o600)
	if code, _, errs := runOpsCLI(t, cfg, "backup", "decrypt", encFile, "--passphrase-file="+passFile); code != 0 {
		t.Fatalf("decrypt: %d %q", code, errs)
	}
	if b, _ := os.ReadFile(strings.TrimSuffix(encFile, ".enc")); !bytes.Equal(b, local) {
		t.Fatal("decrypt output differs")
	}
	wrong := filepath.Join(t.TempDir(), "wrong.txt")
	_ = os.WriteFile(wrong, []byte("wrong passphrase here"), 0o600)
	if code, _, errs := runOpsCLI(t, cfg, "backup", "decrypt", encFile, "--out="+encFile+".x", "--passphrase-file="+wrong); code == 0 || !strings.Contains(errs, "passphrase") {
		t.Fatalf("decrypt with a wrong passphrase: %d %q", code, errs)
	}
	if _, err := os.Stat(encFile + ".x"); !os.IsNotExist(err) {
		t.Fatal("a failed decrypt left a file")
	}
	keyFile := filepath.Join(t.TempDir(), "key.txt")
	_ = os.WriteFile(keyFile, []byte(secret), 0o600)
	if code, _, errs := runOpsCLI(t, cfg, "backup", "remote", "set", "--keep=5", "--secret-key-file="+keyFile); code != 0 {
		t.Fatalf("remote set: %d %q", code, errs)
	}
	if err := srv.settings.Load(ctx); err != nil { // the CLI wrote through its own server
		t.Fatal(err)
	}
	if st := srv.bmgrRemote(); st.Keep != 5 || st.SecretKey != secret || st.Passphrase != pass || st.Bucket != "site-backups" {
		t.Fatalf("after remote set: keep=%d bucket=%q", st.Keep, st.Bucket)
	}

	// A failing upload (wrong secret key) keeps the local backup, reports the failure without the
	// key, and a scheduled run records it as its error.
	t.Setenv("PURSAE_BACKUP_S3_SECRET_KEY", "wrong-secret-key-value-123")
	if v := bmgrRemoteLocked()["secret_key"]; v != "PURSAE_BACKUP_S3_SECRET_KEY" {
		t.Fatalf("locked: %q", v)
	}
	j, _ = srv.bmgrStart("backup")
	name, err := srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"settings"}, UserMode: "all"})
	srv.bmgrFinish(j, err)
	jj = srv.bmgrJobByID(j.ID)
	if err != nil || srv.bmgrPath(name) == "" || jj.RemoteError == "" || jj.Warnings == 0 || jj.State != "done" {
		t.Fatalf("failed upload: err=%v remote_error=%q state=%s", err, jj.RemoteError, jj.State)
	}
	all := jj.RemoteError + strings.Join(jj.Log, "\n")
	if strings.Contains(all, "wrong-secret-key-value-123") || strings.Contains(all, secret) || strings.Contains(all, pass) {
		t.Fatalf("a secret in the job output: %s", all)
	}
	if err := srv.settings.Set(ctx, map[string]any{bmgrOptSchedule: "daily", bmgrOptCats: "settings", bmgrOptNotify: "never",
		bmgrOptLast: map[string]any{"time": time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	_ = srv.bmgrScheduledRun(ctx)
	last, _ := srv.settings.Get(bmgrOptLast).(map[string]any)
	if e := strAny(last["error"]); !strings.Contains(e, "upload to remote storage failed") || strAny(last["file"]) == "" {
		t.Fatalf("scheduled run with a failing upload: %v", last)
	}
	t.Setenv("PURSAE_BACKUP_S3_SECRET_KEY", "")

	// Clearing the bucket switches remote storage off; backups stay local again.
	clear := form()
	clear.Set("bucket", "")
	clear.Set("clear", "1")
	clear.Del("secret_key")
	clear.Del("passphrase")
	clear.Del("passphrase2")
	if loc := save(clear); !strings.Contains(loc, "remote-saved") {
		t.Fatalf("switch off: %s", loc)
	}
	if st := srv.bmgrRemote(); st.configured() || st.SecretKey != "" || st.Passphrase != "" {
		t.Fatalf("still configured: %+v", st.configured())
	}
	before := len(fake.Keys("site-backups"))
	j, _ = srv.bmgrStart("backup")
	_, err = srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"settings"}, UserMode: "all"})
	srv.bmgrFinish(j, err)
	if err != nil || len(fake.Keys("site-backups")) != before || srv.bmgrJobByID(j.ID).Remote != "" {
		t.Fatal("a backup was uploaded with remote storage off")
	}
}

// A restore keeps this site's remote storage settings (they are not taken from the archive).
func TestBmgrRemoteSettingsSurviveRestore(t *testing.T) {
	for _, k := range bmgrKeepSettings {
		if k == bmgrOptRemote {
			return
		}
	}
	t.Fatal("backup_manager_remote is not kept on restore")
}

// In Turkish the Remote storage tab shows its sentences as whole translations.
func TestBmgrRemoteTurkish(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	t.Setenv("PURSAE_BACKUP_S3_ALLOW_PRIVATE", "1")
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	if err := srv.settings.Set(ctx, map[string]any{"WPLANG": "tr_TR"}); err != nil {
		t.Fatal(err)
	}
	fake := s3fake.New("AK", "secret-key", "us-east-1", "bkt")
	defer fake.Close()
	if err := srv.bmgrRemoteSave(ctx, bmgrRemoteSettings{Endpoint: fake.URL, Bucket: "bkt", AccessKey: "AK", SecretKey: "secret-key",
		PathStyle: true, Passphrase: "passphrase-long-enough", Auto: true}, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSAE_BACKUP_S3_REGION", "us-east-1")
	j, _ := srv.bmgrStart("backup")
	_, err := srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"settings"}, UserMode: "all"})
	srv.bmgrFinish(j, err)
	if err != nil {
		t.Fatal(err)
	}
	cat := srv.lang.Catalog("tr_TR")
	tr := func(msgid string, args ...string) string {
		t.Helper()
		v, ok := cat.Lookup(msgid)
		if !ok {
			t.Fatalf("no Turkish translation of %q", msgid)
		}
		return i18n.Sprintf(v, args...)
	}
	_, body, _ := get(t, c, ts.URL+"/wp-admin/admin?page=backup-manager&tab=remote&bmgr-msg=remote-ok")
	for _, want := range []string{
		tr("Connection works: a test file was encrypted, written, listed, read back and deleted."),
		tr("Empty means <code>us-east-1</code>. Cloudflare R2: <code>auto</code>."),
		tr(`Set by <code translate="no">%s</code>.`, "PURSAE_BACKUP_S3_REGION"),
		tr("<strong>Write the passphrase down somewhere safe, away from this server.</strong> Without it the uploaded backups cannot be opened, not even by Pursae. Backups uploaded before a change of passphrase still need the old one."),
		tr("Download and restore"),
		tr("Test connection"),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("remote tab lacks %q", want)
		}
	}
	for _, fragment := range []string{"Write the passphrase down", "Empty means", "Download and restore", "Set by"} {
		if strings.Contains(body, fragment) {
			t.Errorf("remote tab still shows the English %q", fragment)
		}
	}
}

// An archive never carries the remote storage settings: they hold the bucket credentials and the
// encryption passphrase sealed with the site secret, which the same archive carries in auth_secret.
func TestBmgrRemoteSettingsNotInArchive(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	if err := srv.bmgrRemoteSave(ctx, bmgrRemoteSettings{Endpoint: "https://s3.eu-central-1.amazonaws.com", Bucket: "bkt",
		AccessKey: "AKIAARCHIVETEST", SecretKey: "archive-secret-key", Passphrase: "archive-passphrase-long"}, false); err != nil {
		t.Fatal(err)
	}
	j, _ := srv.bmgrStart("backup")
	name, err := srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"settings"}, UserMode: "all"})
	srv.bmgrFinish(j, err)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(filepath.Join(srv.bmgrDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	found := false
	for _, f := range zr.File {
		if f.Name != "database/settings.jsonl" {
			continue
		}
		found = true
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if !strings.Contains(string(b), `"auth_secret"`) {
			t.Fatal("settings.jsonl does not look like the settings table")
		}
		for _, leak := range []string{bmgrOptRemote, bmgrRemoteSealPfx, "AKIAARCHIVETEST"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("the archive carries %q", leak)
			}
		}
	}
	if !found {
		t.Fatal("no settings in the archive")
	}
}
