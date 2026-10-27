package plugins

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// moduleRoot is the wpgo module root relative to this package.
const moduleRoot = "../.."

var (
	buildMu    sync.Mutex
	buildCache = map[string][]byte{}
	buildDir   string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wpgo-plugins-test-")
	if err != nil {
		panic(err)
	}
	buildDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// buildPlugin compiles the plugin in pkgDir (relative to the module root) for
// wasip1/wasm. It skips the test when the toolchain cannot target wasip1.
func buildPlugin(t *testing.T, pkgDir string) []byte {
	t.Helper()
	return buildPluginMode(t, pkgDir, "c-shared")
}

// buildPluginMode is buildPlugin with an explicit -buildmode.
func buildPluginMode(t *testing.T, pkgDir, mode string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Wasm plugin build in -short mode")
	}
	buildMu.Lock()
	defer buildMu.Unlock()
	cacheKey := pkgDir + "@" + mode
	if wasm, ok := buildCache[cacheKey]; ok {
		return wasm
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("skipping: go toolchain not found in PATH (%v)", err)
	}
	root, err := filepath.Abs(moduleRoot)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(buildDir, strings.ReplaceAll(pkgDir, "/", "_")+"-"+mode+".wasm")
	cmd := exec.Command(goBin, "build", "-buildmode="+mode, "-o", out, "./"+pkgDir)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		msg := string(output)
		if strings.Contains(msg, "unsupported GOOS/GOARCH") || strings.Contains(msg, "cannot find GOROOT") {
			t.Skipf("skipping: toolchain cannot build wasip1/wasm: %s", msg)
		}
		t.Fatalf("building %s for wasip1/wasm failed: %v\n%s", pkgDir, err, msg)
	}
	wasm, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	buildCache[cacheKey] = wasm
	return wasm
}

// buildPackage builds pkgDir and wraps it in a signed package loaded through
// LoadPackage, mirroring what the core does with an uploaded zip.
func buildPackage(t *testing.T, pkgDir string) *Package {
	t.Helper()
	wasm := buildPlugin(t, pkgDir)
	manifest, err := os.ReadFile(filepath.Join(moduleRoot, pkgDir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	extras := map[string][]byte{}
	assetsDir := filepath.Join(moduleRoot, pkgDir, AssetsDir)
	_ = filepath.WalkDir(assetsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(assetsDir, p)
		data, _ := os.ReadFile(p)
		extras[AssetsDir+"/"+filepath.ToSlash(rel)] = data
		return nil
	})
	var buf bytes.Buffer
	if err := WritePackage(&buf, manifest, wasm, extras, priv); err != nil {
		t.Fatal(err)
	}
	pkg, err := LoadPackage(bytes.NewReader(buf.Bytes()), int64(buf.Len()), []ed25519.PublicKey{pub}, true)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	return pkg
}

// newTestRuntime returns a runtime with short timeouts, closed at cleanup.
func newTestRuntime(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 500 * time.Millisecond
	}
	if cfg.CacheDir == "" {
		// Share compiled modules between tests; compiling a Go plugin
		// takes about a second.
		cfg.CacheDir = filepath.Join(buildDir, "wazero-cache")
	}
	rt, err := NewRuntime(context.Background(), NewRegistry(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	return rt
}

// loadPlugin builds and loads a plugin with all its declared permissions
// unless perms is given.
func loadPlugin(t *testing.T, rt *Runtime, pkgDir string, host Host, perms ...Permission) *Plugin {
	t.Helper()
	pkg := buildPackage(t, pkgDir)
	if perms == nil {
		perms = pkg.Manifest.Permissions
	}
	p, err := rt.Load(context.Background(), pkg, perms, host)
	if err != nil {
		t.Fatalf("Load %s: %v", pkgDir, err)
	}
	return p
}

func dumpLogs(t *testing.T, p *Plugin) {
	t.Helper()
	for _, e := range p.Logs() {
		t.Logf("[%s/%s] %s", e.Source, e.Level, e.Message)
	}
}
