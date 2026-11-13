package pkgtool

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"wpgo/internal/plugins"
)

func TestScaffoldBuildVerify(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	root, _ := filepath.Abs("../../..")
	dir := filepath.Join(t.TempDir(), "demo-plugin")
	if err := Run([]string{"new", "-slug", "demo-plugin", "-dir", dir, "-sdk", root}); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"new", "-slug", "demo-plugin", "-dir", dir}); err == nil {
		t.Fatal("scaffolding over an existing plugin must fail")
	}
	if err := Run([]string{"new", "-slug", "Bad Slug", "-dir", dir + "2"}); err == nil {
		t.Fatal("invalid slug must fail")
	}
	// The generated unit test runs on a normal toolchain against the fake host.
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scaffold test failed: %v\n%s", err, out)
	}
	zip := filepath.Join(t.TempDir(), "demo.zip")
	if err := Run([]string{"build", "-dir", dir, "-out", zip}); err != nil {
		t.Fatal(err)
	}
	pkg, err := plugins.LoadPackageFile(zip, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Manifest.Slug != "demo-plugin" {
		t.Fatalf("slug %q", pkg.Manifest.Slug)
	}
}

func TestScaffoldSDKPathWithSpaces(t *testing.T) {
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '\\' {
		t.Skip("fake go executable is POSIX only")
	}
	t.Setenv("PATH", bin)
	dir := filepath.Join(t.TempDir(), "plugin")
	if err := Run([]string{"new", "-slug", "example", "-dir", dir, "-sdk", "../my sdk"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(realGo, "mod", "edit", "-json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Replace []struct{ New struct{ Path string } }
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Replace) != 1 || parsed.Replace[0].New.Path != "../my sdk" {
		t.Fatalf("go.mod=%s err=%v", raw, err)
	}
}

func TestScaffoldRejectsInvalidSlugsBeforeWriting(t *testing.T) {
	root := t.TempDir()
	for _, slug := range []string{"bad--slug", "bad-", "a", "admin"} {
		dir := filepath.Join(root, slug)
		if err := Run([]string{"new", "-slug", slug, "-dir", dir}); err == nil {
			t.Errorf("accepted %q", slug)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("created %q: %v", dir, err)
		}
	}
}

func TestScaffoldPreservesExistingFiles(t *testing.T) {
	for _, name := range []string{"main.go", "main_test.go", "go.mod", "readme.md", "plugin.json"} {
		dir := t.TempDir()
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		gate, done := make(chan struct{}), make(chan error, 1)
		go func() { <-gate; done <- Run([]string{"new", "-slug", "valid", "-dir", dir}) }()
		close(gate)
		if err := <-done; err == nil {
			t.Errorf("accepted existing %s", name)
		}
		data, err := os.ReadFile(file)
		entries, readErr := os.ReadDir(dir)
		if err != nil || string(data) != "original" || readErr != nil || len(entries) != 1 {
			t.Errorf("%s changed: %q err=%v entries=%d", name, data, err, len(entries))
		}
	}
}

func TestPackRejectsInvalidOptionalFilePaths(t *testing.T) {
	for _, name := range []string{plugins.ReadmeFile, plugins.IconFile} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			manifest := `{"slug":"valid-plugin","name":"Valid Plugin","version":"0.1.0","requires":"0.1.0","license":"GPL-2.0-or-later"}`
			wasm := filepath.Join(dir, "plugin.wasm")
			if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(wasm, []byte("\x00asm\x01\x00\x00\x00"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := Run([]string{"pack", "-dir", dir, "-wasm", wasm, "-out", filepath.Join(dir, "plugin.zip")}); err == nil {
				t.Fatal("pack accepted a directory in place of an optional source file")
			}
		})
	}
}

func TestKeygenRecoversMissingPublicKey(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "signer")
	pub, priv, err := plugins.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	original := plugins.EncodeKey(priv) + "\n"
	if err := os.WriteFile(prefix+".key", []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"keygen", "-out", prefix}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(prefix + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	got, err := plugins.ParsePublicKey(string(raw))
	key, keyErr := os.ReadFile(prefix + ".key")
	if err != nil || !pub.Equal(got) || keyErr != nil || string(key) != original {
		t.Fatalf("public err=%v private err=%v", err, keyErr)
	}
}

func TestKeygenPreservesExistingPublicFile(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "signer")
	if err := os.WriteFile(prefix+".pub", []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	gate, done := make(chan struct{}), make(chan error, 1)
	go func() { <-gate; done <- Run([]string{"keygen", "-out", prefix}) }()
	close(gate)
	if err := <-done; err == nil {
		t.Fatal("expected an error for an existing public file")
	}
	pub, err := os.ReadFile(prefix + ".pub")
	if err != nil || string(pub) != "original" {
		t.Fatalf("public file=%q err=%v", pub, err)
	}
	if _, err := os.Lstat(prefix + ".key"); !os.IsNotExist(err) {
		t.Fatalf("unexpected private file: %v", err)
	}
}

func TestRunUnknown(t *testing.T) {
	if Run([]string{"nope"}) == nil || Run(nil) == nil {
		t.Fatal("expected errors")
	}
}
