package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"wpgo/internal/config"
)

func TestCLICacheFlushClearsDiskOnProxyFailure(t *testing.T) {
	// The test proxy listens on 127.0.0.1: a private purge target needs the operator switch.
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	ts, client, srv := newTestServer(t, false)
	installAndLogin(t, ts, client)
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer proxy.Close()
	if err := srv.settings.Set(context.Background(), map[string]any{
		"wpgo_cache_server": "generic", "wpgo_cache_purge_url": proxy.URL,
	}); err != nil {
		t.Fatal(err)
	}
	cfg := srv.cfg
	cfg.DatabaseURL = config.Env("TEST_DB")
	cfg.UploadsDir = filepath.Join(t.TempDir(), "uploads")
	dir := cacheDiskDir(cfg.UploadsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"page.wpc", ".tmp-write", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errs bytes.Buffer
	code := CLI(context.Background(), cfg, "pursae", []string{"cache", "flush"}, strings.NewReader(""), &out, &errs)
	if code == 0 || !strings.Contains(errs.String(), "front proxy purge") || strings.Contains(out.String(), "Success:") {
		t.Fatalf("proxy failure: exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
	}
	for _, name := range []string{"page.wpc", ".tmp-write"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("cache file %q was not removed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestCLICacheFlushReportsProxyAndDiskFailures(t *testing.T) {
	// The test proxy listens on 127.0.0.1: a private purge target needs the operator switch.
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	ts, client, srv := newTestServer(t, false)
	installAndLogin(t, ts, client)
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer proxy.Close()
	if err := srv.settings.Set(context.Background(), map[string]any{
		"wpgo_cache_server": "generic", "wpgo_cache_purge_url": proxy.URL,
	}); err != nil {
		t.Fatal(err)
	}
	cfg := srv.cfg
	cfg.DatabaseURL = config.Env("TEST_DB")
	cfg.UploadsDir = filepath.Join(t.TempDir(), "uploads")
	dir := cacheDiskDir(cfg.UploadsDir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := CLI(context.Background(), cfg, "pursae", []string{"cache", "flush"}, strings.NewReader(""), &out, &errs)
	if code == 0 || !strings.Contains(errs.String(), "front proxy purge") || !strings.Contains(errs.String(), "disk cache") {
		t.Fatalf("both failures should be reported: exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
	}
}
