package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"wpgo/internal/cli"
	"wpgo/internal/config"
)

func TestOpsCLIRegistered(t *testing.T) {
	r, closeEnv := NewCLI(config.Config{}, "pursae", nil)
	defer closeEnv()
	for _, p := range []string{
		"cron event list", "cron event run", "cron event retry", "cron event delete", "cron status",
		"db migrate", "db size", "db tables", "db check", "db query",
		"core version", "core check-update",
		"backup list", "backup create", "backup restore", "backup delete",
		"backup remote status", "backup remote set", "backup remote test", "backup remote list", "backup remote push",
		"backup remote pull", "backup remote delete", "backup decrypt",
		"language core list", "language core install", "language core activate", "language core uninstall",
		"mail test", "site health", "site info",
	} {
		cmd := r.Lookup(p)
		if cmd == nil || cmd.Run == nil {
			t.Errorf("%s: not registered", p)
			continue
		}
		if cmd.Short == "" {
			t.Errorf("%s: no Short", p)
		}
	}
	var out bytes.Buffer
	if code := r.Run(context.Background(), []string{"help", "cron", "event", "run"}, cli.IO{Out: &out, Err: &out}); code != 0 || !strings.Contains(out.String(), "cron event run") {
		t.Errorf("help: %d %q", code, out.String())
	}
}

func TestOpsCoreVersionNeedsNoDatabase(t *testing.T) {
	var out, errb bytes.Buffer
	code := CLI(context.Background(), config.Config{}, "pursae", []string{"core", "version"}, strings.NewReader(""), &out, &errb)
	if code != 0 || strings.TrimSpace(out.String()) != Version {
		t.Fatalf("core version: %d %q %q", code, out.String(), errb.String())
	}
}

func TestOpsDBQueryWithoutStatementIsUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	code := CLI(context.Background(), config.Config{}, "pursae", []string{"db", "query", "COMMIT"}, strings.NewReader(""), &out, &errb)
	if code != cli.ExitUsage {
		t.Fatalf("db query COMMIT: %d %q", code, errb.String())
	}
}

func TestParseCronEventRef(t *testing.T) {
	if r := parseCronEventRef("42"); r.ID != 42 || r.Hook != "" {
		t.Errorf("42: %+v", r)
	}
	if r := parseCronEventRef(" wp_session_gc "); r.ID != 0 || r.Hook != "wp_session_gc" {
		t.Errorf("hook: %+v", r)
	}
	if r := parseCronEventRef("-3"); r.ID != 0 || r.Hook != "-3" {
		t.Errorf("-3: %+v", r)
	}
	if !coreEventName("wp_scheduled_delete") || !coreEventName(bmgrScheduleJob) || coreEventName("my_plugin_event") {
		t.Error("coreEventName")
	}
}

func TestCronRunFinished(t *testing.T) {
	mark := time.Now()
	cases := []struct {
		status   string
		at       time.Time
		fin, ok  bool
		scenario string
	}{
		{"queued", mark, false, false, "not picked up"},
		{"running", mark, false, false, "running"},
		{"done", mark, true, true, "one-off done"},
		{"failed", mark, true, false, "one-off failed"},
		{"queued", mark.Add(time.Hour), true, true, "recurring rescheduled"},
	}
	for _, tc := range cases {
		fin, ok := cronRunFinished(tc.status, tc.at, mark)
		if fin != tc.fin || ok != tc.ok {
			t.Errorf("%s: got %v %v", tc.scenario, fin, ok)
		}
	}
}

func TestMigrationStatusRows(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := migrationStatusRows([]string{"0001_init", "0002_seed"}, map[string]time.Time{"0001_init": at})
	if len(rows) != 2 || rows[0]["status"] != "applied" || rows[0]["applied_at"] != at || rows[1]["status"] != "pending" || rows[1]["applied_at"] != nil {
		t.Fatalf("%v", rows)
	}
}

func TestFormatDBSize(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		unit string
		want string
	}{
		{2048, "b", "2048"},
		{2048, "kb", "2.00"},
		{3 << 20, "mb", "3.00"},
		{1536, "", bmgrHuman(1536)},
	} {
		if got := formatDBSize(tc.n, tc.unit); got != tc.want {
			t.Errorf("formatDBSize(%d, %q) = %q, want %q", tc.n, tc.unit, got, tc.want)
		}
	}
}

func TestDBQueryCheck(t *testing.T) {
	for _, q := range []string{"SELECT 1", " -- note\n select now()", "/* x */ WITH a AS (SELECT 1) SELECT * FROM a", "(SELECT 1)", "SHOW server_version", "EXPLAIN SELECT 1"} {
		if err := dbQueryCheck(q); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	for _, q := range []string{"", "   ", "-- only a comment", "COMMIT", "rollback", "begin; delete from users", "SET default_transaction_read_only = off", "/* x */ end"} {
		if err := dbQueryCheck(q); err == nil {
			t.Errorf("%q: accepted", q)
		}
	}
}

func TestUniqueColumnNames(t *testing.T) {
	got := uniqueColumnNames([]string{"count", "count", "?column?", "id", "count_2", ""})
	want := []string{"count", "count_2", "column", "id", "count_2_2", "column_2"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Fatalf("duplicate %q in %v", n, got)
		}
		seen[n] = true
	}
}

func TestSQLCLIValue(t *testing.T) {
	if v := sqlCLIValue([]byte{0xde, 0xad}); v != `\xdead` {
		t.Errorf("bytea: %v", v)
	}
	u := [16]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}
	if v := sqlCLIValue(u); v != "12345678-9abc-def0-1234-56789abcdef0" {
		t.Errorf("uuid: %v", v)
	}
	if v := sqlCLIValue(int64(5)); v != int64(5) {
		t.Errorf("int: %v", v)
	}
	if v, ok := sqlCLIValue([]any{[]byte{1}}).([]any); !ok || v[0] != `\x01` {
		t.Errorf("array: %v", v)
	}
}

func TestCoreUpdateType(t *testing.T) {
	for _, tc := range [][3]string{{"1.2.3", "2.0.0", "major"}, {"1.2.3", "1.3.0", "minor"}, {"1.2.3", "1.2.4", "patch"}, {"v1.2.3", "1.2.4-beta.1", "patch"}, {"1.2", "1.2.1", "patch"}} {
		if got := coreUpdateType(tc[0], tc[1]); got != tc[2] {
			t.Errorf("coreUpdateType(%s, %s) = %s, want %s", tc[0], tc[1], got, tc[2])
		}
	}
}

func TestBmgrParseParts(t *testing.T) {
	all, err := bmgrParseParts("all")
	if err != nil || len(all) != len(bmgrCategories) {
		t.Fatalf("all: %v %v", all, err)
	}
	got, err := bmgrParseParts("content, media,content")
	if err != nil || !slices.Equal(got, []string{"content", "media"}) {
		t.Fatalf("list: %v %v", got, err)
	}
	if _, err := bmgrParseParts("content,nope"); err == nil {
		t.Fatal("unknown part accepted")
	}
}

func TestBmgrParseUsers(t *testing.T) {
	if m, _, _, err := bmgrParseUsers(""); err != nil || m != "all" {
		t.Errorf("empty: %s %v", m, err)
	}
	if m, roles, _, err := bmgrParseUsers("roles:administrator, editor"); err != nil || m != "roles" || !slices.Equal(roles, []string{"administrator", "editor"}) {
		t.Errorf("roles: %s %v %v", m, roles, err)
	}
	if m, _, ids, err := bmgrParseUsers("ids:1,7"); err != nil || m != "ids" || !slices.Equal(ids, []int64{1, 7}) {
		t.Errorf("ids: %s %v %v", m, ids, err)
	}
	for _, bad := range []string{"roles:", "ids:", "ids:x", "some"} {
		if _, _, _, err := bmgrParseUsers(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestOpsNewLogLines(t *testing.T) {
	if got := opsNewLogLines(nil, []string{"a", "b"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("first: %v", got)
	}
	if got := opsNewLogLines([]string{"a", "b"}, []string{"a", "b", "c"}); !slices.Equal(got, []string{"c"}) {
		t.Errorf("more: %v", got)
	}
	if got := opsNewLogLines([]string{"a", "b"}, []string{"b", "c", "d"}); !slices.Equal(got, []string{"c", "d"}) {
		t.Errorf("window: %v", got)
	}
	if got := opsNewLogLines([]string{"a", "b"}, []string{"a", "b"}); len(got) != 0 {
		t.Errorf("none: %v", got)
	}
}

func TestLangFiles(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"admin-de_DE.mo", "admin-de_DE.po", "de_DE.mo", "admin-network-de_DE.mo", "continents-cities-de_DE.mo",
		"de_DE-0123456789abcdef0123456789abcdef.json", "plugins/woocommerce-de_DE.mo", "plugins/wp-mail-smtp-de_DE-0123456789abcdef0123456789abcdef.json",
		// other locales
		"admin-de_DE_formal.mo", "de_DE_formal.mo", "plugins/woocommerce-de_DE_formal.mo", "admin-de_CH.mo", "readme.txt",
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := langFiles(dir, "de_DE")
	slices.Sort(got)
	want := append([]string(nil), files[:8]...)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if f := langFiles(dir, "de_DE_formal"); len(f) != 3 {
		t.Fatalf("formal: %v", f)
	}
}

func TestLangStatus(t *testing.T) {
	inst := map[string]bool{"de_DE": true}
	if langStatus("de_DE", "de_DE", inst) != "active" || langStatus("de_DE", "en_US", inst) != "installed" ||
		langStatus("en_US", "de_DE", inst) != "installed" || langStatus("fr_FR", "en_US", inst) != "uninstalled" {
		t.Fatal("langStatus")
	}
}

func TestCLIHealthAdjustments(t *testing.T) {
	tests := cliHealthTests([]healthTest{
		{Label: healthLabelSandboxDown, Status: "critical"},
		{Label: healthLabelCacheOn, Status: "good", Description: "1 cached"},
		{Label: "other", Status: "good"},
	})
	if len(tests) != 2 || tests[0].Label != healthLabelCacheOn || strings.Contains(tests[0].Description, "1 cached") {
		t.Fatalf("%+v", tests)
	}
	f := cliHealthInfoFields("Server", [][2]string{{"Go version", "go1"}, {"Goroutines", "9"}, {"Memory in use", "1 MB"}}, nil)
	if len(f) != 1 || f[0][0] != "Go version" {
		t.Errorf("server: %v", f)
	}
	f = cliHealthInfoFields("Plugins", [][2]string{{"Active Wasm plugins", "None"}}, []string{"hello 1.0.0"})
	if f[0][1] != "hello 1.0.0" {
		t.Errorf("plugins: %v", f)
	}
}

func TestHealthScore(t *testing.T) {
	for _, tc := range []struct {
		g, r, c int
		val     int
		label   string
	}{
		{10, 0, 0, 100, "Good"},
		{0, 0, 0, 100, "Good"},
		{8, 2, 0, 90, "Good"},
		{8, 0, 1, 84, "Should be improved"},
		{0, 0, 3, 0, "Should be improved"},
	} {
		v, l := healthScore(tc.g, tc.r, tc.c)
		if v != tc.val || l != tc.label {
			t.Errorf("healthScore(%d,%d,%d) = %d %q, want %d %q", tc.g, tc.r, tc.c, v, l, tc.val, tc.label)
		}
	}
}

func TestHealthActionText(t *testing.T) {
	if got := healthActionText(`<a href="/wp-admin/admin?page=wp-mail-smtp-tools&amp;tab=test">Send a test email</a>`); got != "Send a test email (/wp-admin/admin?page=wp-mail-smtp-tools&tab=test)" {
		t.Errorf("%q", got)
	}
	if got := healthActionText(""); got != "" {
		t.Errorf("%q", got)
	}
}

// --- integration (PURSAE_TEST_DB) ---------------------------------------------------------

// runOpsCLI runs one command against the test server's database.
func runOpsCLI(t *testing.T, cfg config.Config, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := CLI(context.Background(), cfg, "pursae", args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestOpsCLIIntegration(t *testing.T) {
	ts, client, srv := newTestServer(t, false)
	installAndLogin(t, ts, client)
	ctx := context.Background()
	cfg := config.Config{DatabaseURL: config.Env("TEST_DB"), UploadsDir: srv.cfg.UploadsDir, WebDir: srv.cfg.WebDir, HubURL: "off"}
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	pool := srv.store.Pool

	// cron
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO jobs(name, run_at, status, attempts, last_error) VALUES ('cli_test_job', now() + interval '1 day', 'failed', 5, 'boom') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := runOpsCLI(t, cfg, "cron", "event", "list", "--status=failed", "--field=hook"); code != 0 || strings.TrimSpace(out) != "cli_test_job" {
		t.Fatalf("cron event list: %d %q %q", code, out, errs)
	}
	if code, _, errs := runOpsCLI(t, cfg, "cron", "event", "run", "cli_test_job"); code != 0 {
		t.Fatalf("cron event run: %d %q", code, errs)
	}
	var status, lastErr string
	var attempts int
	var due bool
	if err := pool.QueryRow(ctx, `SELECT status, attempts, last_error, run_at <= now() FROM jobs WHERE id = $1`, id).Scan(&status, &attempts, &lastErr, &due); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || attempts != 0 || lastErr != "" || !due {
		t.Fatalf("after run: %s %d %q due=%v", status, attempts, lastErr, due)
	}
	if code, _, _ := runOpsCLI(t, cfg, "cron", "event", "run", "no_such_hook"); code != 1 {
		t.Fatalf("run unknown hook: %d", code)
	}
	if code, _, _ := runOpsCLI(t, cfg, "cron", "event", "delete", "--status=failed"); code != 0 {
		t.Fatalf("delete without failed jobs: %d", code)
	}
	if code, _, errs := runOpsCLI(t, cfg, "cron", "event", "delete", "1", "2"); code != 1 || !strings.Contains(errs, "--yes") {
		t.Fatalf("delete without --yes: %d %q", code, errs)
	}
	if code, _, errs := runOpsCLI(t, cfg, "cron", "event", "delete", itoa64(id), "--yes"); code != 0 {
		t.Fatalf("delete: %d %q", code, errs)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, id).Scan(&n)
	if n != 0 {
		t.Fatal("job not deleted")
	}
	if code, out, errs := runOpsCLI(t, cfg, "cron", "status", "--format=json"); code != 0 || !strings.Contains(out, `"events"`) {
		t.Fatalf("cron status: %d %q %q", code, out, errs)
	}

	// db
	if code, out, errs := runOpsCLI(t, cfg, "db", "check", "--field=pending_migrations"); code != 0 || strings.TrimSpace(out) != "0" {
		t.Fatalf("db check: %d %q %q", code, out, errs)
	}
	if code, out, _ := runOpsCLI(t, cfg, "db", "tables"); code != 0 || !strings.Contains(out, "\nposts\n") {
		t.Fatalf("db tables: %d %q", code, out)
	}
	if code, out, _ := runOpsCLI(t, cfg, "db", "size", "--tables", "--format=csv", "--fields=Name,Category"); code != 0 || !strings.Contains(out, "settings,settings") {
		t.Fatalf("db size: %d %q", code, out)
	}
	if code, out, _ := runOpsCLI(t, cfg, "db", "migrate", "--status", "--pending", "--format=count"); code != 0 || strings.TrimSpace(out) != "0" {
		t.Fatalf("db migrate --status: %d %q", code, out)
	}
	if code, out, errs := runOpsCLI(t, cfg, "db", "query", "SELECT 1 AS a, 'x' AS a", "--format=json"); code != 0 || !strings.Contains(out, `"a_2":"x"`) {
		t.Fatalf("db query: %d %q %q", code, out, errs)
	}
	if code, _, errs := runOpsCLI(t, cfg, "db", "query", "DELETE FROM jobs"); code != 1 || !strings.Contains(errs, "read-only") {
		t.Fatalf("db query write: %d %q", code, errs)
	}

	// core
	if code, _, errs := runOpsCLI(t, cfg, "core", "check-update"); code != 1 || !strings.Contains(errs, "switched off") {
		t.Fatalf("core check-update with the hub off: %d %q", code, errs)
	}

	// backup
	code, out, errs := runOpsCLI(t, cfg, "backup", "create", "--parts=settings", "--porcelain")
	name := strings.TrimSpace(out)
	if code != 0 || !strings.HasSuffix(name, ".zip") {
		t.Fatalf("backup create: %d %q %q", code, out, errs)
	}
	if code, out, _ := runOpsCLI(t, cfg, "backup", "list", "--field=name"); code != 0 || strings.TrimSpace(out) != name {
		t.Fatalf("backup list: %d %q", code, out)
	}
	if code, out, errs := runOpsCLI(t, cfg, "backup", "restore", name, "--no-safety", "--yes"); code != 0 {
		t.Fatalf("backup restore: %d %q %q", code, out, errs)
	}
	if code, _, errs := runOpsCLI(t, cfg, "backup", "delete", name, "--yes"); code != 0 {
		t.Fatalf("backup delete: %d %q", code, errs)
	}
	if code, out, _ := runOpsCLI(t, cfg, "backup", "list", "--format=count"); code != 0 || strings.TrimSpace(out) != "0" {
		t.Fatalf("backup list after delete: %d %q", code, out)
	}

	// language
	if code, out, _ := runOpsCLI(t, cfg, "language", "core", "list", "--status=active", "--field=language"); code != 0 || strings.TrimSpace(out) != "en_US" {
		t.Fatalf("language core list: %d %q", code, out)
	}
	if code, _, errs := runOpsCLI(t, cfg, "language", "core", "activate", "de_DE"); code != 1 || !strings.Contains(errs, "not installed") {
		t.Fatalf("activate uninstalled: %d %q", code, errs)
	}

	// site
	if code, out, errs := runOpsCLI(t, cfg, "site", "info", "--section=database", "--format=csv"); code != 0 || !strings.Contains(out, "Server version") {
		t.Fatalf("site info: %d %q %q", code, out, errs)
	}
	code, out, errs = runOpsCLI(t, cfg, "site", "health", "--format=json")
	if code != 0 && code != 1 || strings.Contains(out, "plugin sandbox") {
		t.Fatalf("site health: %d %q %q", code, out, errs)
	}

	// mail: no SMTP in the test config, so the message lands in the mail directory.
	if code, _, errs := runOpsCLI(t, cfg, "mail", "test", "someone@example.test"); code != 0 || !strings.Contains(errs, ".eml") {
		t.Fatalf("mail test: %d %q", code, errs)
	}
}
