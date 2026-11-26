package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// localhostURL is srv's URL with the host name "localhost": a name that resolves to the loopback
// address, so only the dial-time check can catch it.
func localhostURL(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return "http://localhost:" + u.Port()
}

// The CDN storage endpoint is checked when saved: loopback and private addresses need
// PURSAE_ALLOW_PRIVATE_ENDPOINTS, link-local and metadata addresses never pass.
func TestW2EndpointsCDNSaveTime(t *testing.T) {
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	for _, tc := range []struct {
		endpoint      string
		ok, okPrivate bool
	}{
		{"https://s3.eu-central-1.amazonaws.com", true, true},
		{"http://127.0.0.1:9000", false, true},
		{"http://localhost:9000", false, true},
		{"http://10.0.0.5:9000", false, true},
		{"http://169.254.169.254", false, false},
		{"http://[64:ff9b::a9fe:a9fe]", false, false}, // NAT64 form of the metadata address
		{"http://168.63.129.16", false, false},
		{"http://user:pw@s3.example.com", false, false},
		{"ftp://s3.example.com", false, false},
	} {
		if err := gocdnCheckEndpoint(tc.endpoint); (err == nil) != tc.ok {
			t.Errorf("%s without the switch: err = %v, want ok=%v", tc.endpoint, err, tc.ok)
		}
	}
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	for _, tc := range []struct {
		endpoint string
		ok       bool
	}{
		{"http://127.0.0.1:9000", true},
		{"http://10.0.0.5:9000", true},
		{"http://169.254.169.254", false},
		{"http://168.63.129.16", false},
	} {
		if err := gocdnCheckEndpoint(tc.endpoint); (err == nil) != tc.ok {
			t.Errorf("%s with the switch: err = %v, want ok=%v", tc.endpoint, err, tc.ok)
		}
	}
}

// gocdnSave (the settings screen and the options API) refuses a new private endpoint but keeps
// saving other settings while an old stored endpoint is left unchanged.
func TestW2EndpointsCDNSaveRefusesPrivate(t *testing.T) {
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	_, _, srv := newTestServer(t, false)
	ctx := context.Background()
	st := srv.gocdnLoad()
	st.Provider, st.Bucket, st.AccessKey, st.SecretKey = "minio", "media", "AK", "SK"
	st.Endpoint = "http://127.0.0.1:9000"
	if err := srv.gocdnSave(ctx, st); err == nil || !strings.Contains(err.Error(), "private network") {
		t.Fatalf("private endpoint saved: %v", err)
	}
	st.Endpoint = "http://169.254.169.254"
	if err := srv.gocdnSave(ctx, st); err == nil {
		t.Fatal("metadata endpoint saved")
	}
	st.Endpoint = "https://s3.example.com"
	if err := srv.gocdnSave(ctx, st); err != nil {
		t.Fatalf("public endpoint: %v", err)
	}
	if got := srv.gocdnLoad().Endpoint; got != "https://s3.example.com" {
		t.Fatalf("stored endpoint = %q", got)
	}
	// A legacy private endpoint already stored does not block saving another setting.
	if err := srv.settings.Set(ctx, map[string]any{gocdnOption: map[string]any{"provider": "minio", "endpoint": "http://127.0.0.1:9000",
		"bucket": "media"}}); err != nil {
		t.Fatal(err)
	}
	st = srv.gocdnLoad()
	st.Img = !st.Img
	if err := srv.gocdnSave(ctx, st); err != nil {
		t.Fatalf("saving with an unchanged stored endpoint: %v", err)
	}
}

// The storage client checks the address it connects to: a host name resolving to the loopback
// address is refused unless the switch is set, or PURSAE_CDN_SETTINGS (the operator) names it.
func TestW2EndpointsCDNDialTime(t *testing.T) {
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	t.Setenv("PURSAE_CDN_SETTINGS", "")
	_, _, srv := newTestServer(t, false)
	var hits atomic.Int32
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "0")
	}))
	defer s3.Close()
	ep := localhostURL(t, s3)
	st := gocdnSettings{Provider: "minio", Endpoint: ep, Bucket: "media", AccessKey: "AK", SecretKey: "SK", PathStyle: true}
	head := func() error {
		cl, err := srv.gocdnClient(st)
		if err != nil {
			t.Fatal(err)
		}
		_, err = cl.Head(context.Background(), "probe.txt")
		return err
	}
	if err := head(); err == nil || !strings.Contains(err.Error(), "private network") || hits.Load() != 0 {
		t.Fatalf("loopback storage reached without the switch: err %v, hits %d", err, hits.Load())
	}
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	if err := head(); err != nil && strings.Contains(err.Error(), "private network") {
		t.Fatalf("switch on, still refused: %v", err)
	}
	if hits.Load() == 0 {
		t.Fatal("switch on, the storage was not reached")
	}
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	t.Setenv("PURSAE_CDN_SETTINGS", `{"endpoint":"`+ep+`"}`)
	hits.Store(0)
	if err := head(); (err != nil && strings.Contains(err.Error(), "private network")) || hits.Load() == 0 {
		t.Fatalf("endpoint from PURSAE_CDN_SETTINGS refused: err %v, hits %d", err, hits.Load())
	}
}

// The remote backup keeps its own switch and also accepts the shared one.
func TestW2EndpointsBackupSwitches(t *testing.T) {
	t.Setenv("PURSAE_BACKUP_S3_ALLOW_PRIVATE", "")
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	if bmgrRemoteAllowPrivate() {
		t.Fatal("private allowed with no switch")
	}
	t.Setenv("PURSAE_BACKUP_S3_ALLOW_PRIVATE", "1")
	if !bmgrRemoteAllowPrivate() {
		t.Fatal("PURSAE_BACKUP_S3_ALLOW_PRIVATE no longer works")
	}
	t.Setenv("PURSAE_BACKUP_S3_ALLOW_PRIVATE", "")
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	if !bmgrRemoteAllowPrivate() {
		t.Fatal("the shared switch does not reach the backup storage")
	}
	if err := bmgrRemoteCheckEndpoint("http://169.254.169.254", true); err == nil {
		t.Fatal("metadata endpoint allowed for the backup storage")
	}
}
