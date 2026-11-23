package server

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"wpgo/internal/backupcrypt"
	"wpgo/internal/config"
)

// pgTool finds pg_dump / pg_restore: in PURSAE_TEST_PG_BIN when set (CI points it at wrappers that
// run the tools of the database server's own image, so the versions match), else on PATH. With
// PURSAE_TEST_PG_BIN set a missing tool fails the test; otherwise the test is skipped.
func pgTool(t *testing.T, name string) string {
	t.Helper()
	if dir := config.Env("TEST_PG_BIN"); dir != "" {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return p
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found (set PURSAE_TEST_PG_BIN)", name)
	}
	return p
}

var pgMajorRe = regexp.MustCompile(`(\d+)(?:\.\d+)?`)

func pgMajor(s string) int {
	m := pgMajorRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// The documented manual database backup (docs/developer/operations/backups.md): `pg_dump -Fc`
// and `pg_restore --clean --if-exists` bring a Pursae database back exactly, through stdin and
// stdout as in the Docker variant, also after a round trip through backup encryption (a dump
// stored off the server the same way as Backup Manager archives).
func TestBackupPgDumpRoundTrip(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	dsn := config.Env("TEST_DB")
	dump, restore := pgTool(t, "pg_dump"), pgTool(t, "pg_restore")

	var serverVersion string
	if err := srv.store.Pool.QueryRow(ctx, `SHOW server_version`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(dump, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("pg_dump --version: %v %s", err, out)
	}
	if v := pgMajor(strings.TrimPrefix(string(out), "pg_dump (PostgreSQL) ")); v < pgMajor(serverVersion) {
		if config.Env("TEST_PG_BIN") != "" {
			t.Fatalf("pg_dump %d is older than the server (%s)", v, serverVersion)
		}
		t.Skipf("pg_dump %d cannot dump PostgreSQL %s", v, serverVersion)
	}

	// Content to bring back.
	if err := srv.settings.Set(ctx, map[string]any{"blogname": "Before the dump"}); err != nil {
		t.Fatal(err)
	}
	var postID int64
	if err := srv.store.Pool.QueryRow(ctx, `INSERT INTO posts(title, content, status, type, slug, author_id, published_at)
		SELECT 'Dumped post', '<p>body</p>', 'publish', 'post', 'dumped-post', id, now() FROM users ORDER BY id LIMIT 1 RETURNING id`).Scan(&postID); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := srv.store.Pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	usersBefore := count(`SELECT count(*) FROM users`)
	tablesBefore := count(`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`)

	var dumped, stderr bytes.Buffer
	cmd := exec.Command(dump, "-Fc", dsn)
	cmd.Stdout, cmd.Stderr = &dumped, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pg_dump: %v %s", err, stderr.String())
	}
	if !bytes.HasPrefix(dumped.Bytes(), []byte("PGDMP")) {
		t.Fatalf("not a custom-format dump: %q", dumped.Bytes()[:min(16, dumped.Len())])
	}

	// Encrypted and decrypted like an archive on its way to and from the bucket.
	var enc, dec bytes.Buffer
	if _, err := backupcrypt.EncryptStream(&enc, bytes.NewReader(dumped.Bytes()), []byte("dump passphrase 123")); err != nil {
		t.Fatal(err)
	}
	if _, err := backupcrypt.DecryptStream(&dec, &enc, []byte("dump passphrase 123")); err != nil || !bytes.Equal(dec.Bytes(), dumped.Bytes()) {
		t.Fatalf("encryption round trip of the dump: %v", err)
	}

	// Damage the database, then restore the dump over it.
	if _, err := srv.store.Pool.Exec(ctx, `DELETE FROM posts WHERE id = $1`, postID); err != nil {
		t.Fatal(err)
	}
	if err := srv.settings.Set(ctx, map[string]any{"blogname": "After the dump"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.Pool.Exec(ctx, `CREATE TABLE pgdump_test_extra(id int)`); err != nil {
		t.Fatal(err)
	}
	srv.store.Pool.Reset() // pg_restore --clean drops objects the pool's prepared statements refer to
	stderr.Reset()
	cmd = exec.Command(restore, "--clean", "--if-exists", "--no-owner", "-d", dsn)
	cmd.Stdin, cmd.Stderr = bytes.NewReader(dec.Bytes()), &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pg_restore: %v %s", err, stderr.String())
	}

	if n := count(`SELECT count(*) FROM posts WHERE id = $1 AND title = 'Dumped post'`, postID); n != 1 {
		t.Error("the deleted post is not back")
	}
	var blogname string
	if err := srv.store.Pool.QueryRow(ctx, `SELECT value #>> '{}' FROM settings WHERE key = 'blogname'`).Scan(&blogname); err != nil || blogname != "Before the dump" {
		t.Errorf("blogname after restore: %q %v", blogname, err)
	}
	if n := count(`SELECT count(*) FROM users`); n != usersBefore {
		t.Errorf("users: %d, want %d", n, usersBefore)
	}
	// Tables created after the dump survive (pg_restore --clean only drops what the dump holds).
	if n := count(`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`); n != tablesBefore+1 {
		t.Errorf("tables: %d, want %d", n, tablesBefore+1)
	}
	// The restored database is one this version of Pursae runs on: nothing to migrate, and the
	// site serves the restored content.
	if n := count(`SELECT count(*) FROM schema_migrations`); n == 0 {
		t.Error("schema_migrations is empty after the restore")
	}
	if err := srv.settings.Load(ctx); err != nil {
		t.Fatal(err)
	}
	code, body, loc := get(t, c, ts.URL+"/?p="+strconv.FormatInt(postID, 10))
	if code/100 == 3 && loc != "" { // to the pretty permalink
		if !strings.HasPrefix(loc, "http") {
			loc = ts.URL + loc
		}
		code, body, _ = get(t, c, loc)
	}
	if code != 200 || !strings.Contains(body, "Dumped post") {
		t.Errorf("restored post not served: %d", code)
	}
}
