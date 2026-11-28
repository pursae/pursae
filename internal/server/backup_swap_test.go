package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFileT(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileT(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// A full restore makes the plugins and themes folders exactly the archive's: a file planted after the
// backup is moved to quarantine (not deleted), while an extra media file stays where it is.
func TestBmgrRestoreQuarantinesPlantedCode(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	ctx := context.Background()
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	roots := srv.bmgrFileRoots()
	writeFileT(t, filepath.Join(roots["plugins"], "good", "main.wasm"), "original plugin")
	writeFileT(t, filepath.Join(roots["themes"], "basho", "style.css"), "original theme")
	writeFileT(t, filepath.Join(roots["uploads"], "2026", "a.txt"), "photo")

	var archive string
	bmgrRun(t, srv, "backup", func(j *bmgrJob) (err error) {
		archive, err = srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"plugins", "themes", "media"}, UserMode: "all"})
		return err
	})

	// After the backup: a planted plugin file and theme file, a changed file, a new upload.
	writeFileT(t, filepath.Join(roots["plugins"], "good", "main.wasm"), "tampered")
	writeFileT(t, filepath.Join(roots["plugins"], "evil", "shell.wasm"), "planted")
	writeFileT(t, filepath.Join(roots["themes"], "basho", "backdoor.html"), "planted")
	writeFileT(t, filepath.Join(roots["uploads"], "2026", "new.txt"), "uploaded later")

	var job *bmgrJob
	bmgrRun(t, srv, "restore", func(j *bmgrJob) error {
		job = j
		return srv.bmgrRunRestore(ctx, j, bmgrRestoreReq{File: archive, Categories: []string{"plugins", "themes", "media"}, Me: 1})
	})

	if got := readFileT(filepath.Join(roots["plugins"], "good", "main.wasm")); got != "original plugin" {
		t.Errorf("plugin file = %q", got)
	}
	for _, p := range []string{filepath.Join(roots["plugins"], "evil", "shell.wasm"), filepath.Join(roots["themes"], "basho", "backdoor.html")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived the restore", p)
		}
	}
	if got := readFileT(filepath.Join(roots["uploads"], "2026", "new.txt")); got != "uploaded later" {
		t.Errorf("an extra media file was touched: %q", got)
	}
	if got := readFileT(filepath.Join(roots["uploads"], "2026", "a.txt")); got != "photo" {
		t.Errorf("media file = %q", got)
	}
	// Quarantine: exactly the two planted files, outside the web root; the tampered copy is superseded.
	var found []string
	q := srv.bmgrQuarantineDir()
	_ = filepath.WalkDir(q, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(q, p)
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	joined := strings.Join(found, " ")
	if len(found) != 2 || !strings.Contains(joined, "/plugins/evil/shell.wasm") || !strings.Contains(joined, "/themes/basho/backdoor.html") {
		t.Errorf("quarantine holds %v", found)
	}
	if rel, _ := filepath.Rel(roots["uploads"], q); !strings.HasPrefix(rel, "..") {
		t.Errorf("the quarantine %s is inside the uploads folder", q)
	}
	log := strings.Join(srv.bmgrJobByID(job.ID).Log, "\n")
	if !strings.Contains(log, "2 plugin or theme files that are not in the archive were moved to") {
		t.Errorf("the restore log does not report the quarantine:\n%s", log)
	}
	// No staging or old folders are left beside the roots.
	ents, _ := os.ReadDir(filepath.Dir(roots["plugins"]))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".wpgo-restore-") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

// A swap that fails half-way is undone: the replaced folder and the already merged files come back.
func TestBmgrSwapRollsBack(t *testing.T) {
	base := t.TempDir()
	srv := &Server{}
	plugins, uploads := filepath.Join(base, "plugins"), filepath.Join(base, "uploads")
	writeFileT(t, filepath.Join(plugins, "p", "old.wasm"), "old plugin")
	writeFileT(t, filepath.Join(uploads, "a.txt"), "old a")
	writeFileT(t, filepath.Join(uploads, "blocker"), "a file where the archive has a folder")

	roots, staged := map[string]string{}, map[string]string{}
	for g, r := range map[string]string{"plugins": plugins, "uploads": uploads} {
		res, stage, err := bmgrStageDir(r, g)
		if err != nil {
			t.Fatal(err)
		}
		roots[g], staged[g] = res, stage
	}
	writeFileT(t, filepath.Join(staged["plugins"], "p", "new.wasm"), "new plugin")
	writeFileT(t, filepath.Join(staged["uploads"], "a.txt"), "new a")
	writeFileT(t, filepath.Join(staged["uploads"], "b.txt"), "new b")
	writeFileT(t, filepath.Join(staged["uploads"], "blocker", "c.txt"), "cannot be placed")

	j := &bmgrJob{}
	if _, err := srv.bmgrSwapIn(context.Background(), j, []string{"plugins", "uploads"}, staged, roots); err == nil {
		t.Fatal("the swap succeeded through a file used as a folder")
	}
	if got := readFileT(filepath.Join(plugins, "p", "old.wasm")); got != "old plugin" {
		t.Errorf("plugins not rolled back: %q", got)
	}
	if _, err := os.Stat(filepath.Join(plugins, "p", "new.wasm")); err == nil {
		t.Error("the restored plugin stayed after the rollback")
	}
	if got := readFileT(filepath.Join(uploads, "a.txt")); got != "old a" {
		t.Errorf("a.txt not rolled back: %q", got)
	}
	if _, err := os.Stat(filepath.Join(uploads, "b.txt")); err == nil {
		t.Error("b.txt stayed after the rollback")
	}

	// A successful swap, then the database commit fails: rollback restores the old files too.
	for g := range staged {
		os.RemoveAll(staged[g])
	}
	os.Remove(filepath.Join(uploads, "blocker"))
	for g, r := range map[string]string{"plugins": plugins, "uploads": uploads} {
		_, stage, err := bmgrStageDir(r, g)
		if err != nil {
			t.Fatal(err)
		}
		staged[g] = stage
	}
	writeFileT(t, filepath.Join(staged["plugins"], "p", "new.wasm"), "new plugin")
	writeFileT(t, filepath.Join(staged["uploads"], "a.txt"), "new a")
	w, err := srv.bmgrSwapIn(context.Background(), j, []string{"plugins", "uploads"}, staged, roots)
	if err != nil {
		t.Fatal(err)
	}
	if readFileT(filepath.Join(uploads, "a.txt")) != "new a" || readFileT(filepath.Join(plugins, "p", "new.wasm")) != "new plugin" {
		t.Fatal("the swap did not put the files in place")
	}
	if err := w.rollback(); err != nil {
		t.Fatal(err)
	}
	if readFileT(filepath.Join(uploads, "a.txt")) != "old a" || readFileT(filepath.Join(plugins, "p", "old.wasm")) != "old plugin" {
		t.Error("rollback after a successful swap did not restore the old files")
	}
	// finish after success quarantines what the archive lacked.
	_, stage, _ := bmgrStageDir(plugins, "plugins")
	writeFileT(t, filepath.Join(stage, "p", "new.wasm"), "new plugin")
	w, err = srv.bmgrSwapIn(context.Background(), j, []string{"plugins"}, map[string]string{"plugins": stage}, roots)
	if err != nil {
		t.Fatal(err)
	}
	moved, where, err := w.finish(filepath.Join(base, "q"), time.Unix(0, 0))
	if err != nil || moved != 1 || readFileT(filepath.Join(where, "plugins", "p", "old.wasm")) != "old plugin" {
		t.Errorf("finish: %d %s %v", moved, where, err)
	}
}
