package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wpgo/internal/backupcrypt"
	"wpgo/internal/gocdn"
	"wpgo/internal/gocdn/s3fake"
)

func TestBmgrRemoteCheckEndpoint(t *testing.T) {
	for _, tc := range []struct {
		url          string
		allowPrivate bool
		ok           bool
	}{
		{"https://s3.eu-central-1.amazonaws.com", false, true},
		{"https://abc123.r2.cloudflarestorage.com", false, true},
		{"https://s3.us-west-004.backblazeb2.com:443", false, true},
		{"https://8.8.8.8", false, true},
		{"http://s3.amazonaws.com", false, false}, // plain http
		{"ftp://s3.amazonaws.com", false, false},  // scheme
		{"https://user:pw@s3.amazonaws.com", false, false},
		{"https://s3.amazonaws.com/?x=1", false, false},
		{"https://s3.amazonaws.com/#frag", false, false},
		{"https://", false, false},
		{"s3.amazonaws.com", false, false},
		{"https://127.0.0.1:9000", false, false},
		{"https://localhost:9000", false, false},
		{"https://minio.localhost", false, false},
		{"https://10.0.0.5", false, false},
		{"https://[::1]:9000", false, false},
		{"https://192.168.1.10:9000", false, false},
		{"https://100.64.1.1", false, false},
		{"https://169.254.169.254", false, false},
		{"http://127.0.0.1:9000", true, true}, // a MinIO on this host, explicitly allowed
		{"http://10.0.0.5:9000", true, true},
		{"https://localhost:9000", true, true},
		{"http://169.254.169.254", true, false}, // metadata: never, not even with the override
		{"http://[fe80::1]:9000", true, false},
		{"http://100.100.100.200", true, false},
		{"http://[fd00:ec2::254]", true, false},
		{"https://0.0.0.0", true, false},
		{"https://s3.amazonaws.com:99999", false, false},
	} {
		err := bmgrRemoteCheckEndpoint(tc.url, tc.allowPrivate)
		if (err == nil) != tc.ok {
			t.Errorf("%s (allowPrivate=%v): err=%v, want ok=%v", tc.url, tc.allowPrivate, err, tc.ok)
		}
	}
}

// The storage client checks the address it connects to after DNS resolution, and follows no redirect.
func TestBmgrRemoteHTTPGuards(t *testing.T) {
	hits := 0
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer inner.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/latest/meta-data/", http.StatusFound)
	}))
	defer redirect.Close()

	if _, err := bmgrRemoteHTTP(false).Get(inner.URL); err == nil || !strings.Contains(err.Error(), "private network") {
		t.Errorf("loopback reached without the override: %v", err)
	}
	resp, err := bmgrRemoteHTTP(true).Get(redirect.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || hits != 0 {
		t.Errorf("redirect followed: status %d, %d hits on the target", resp.StatusCode, hits)
	}
	// Through the S3 client a redirect is an error, not a success.
	cl := &gocdn.Client{Endpoint: redirect.URL, Region: "us-east-1", Bucket: "b", AccessKey: "AK", SecretKey: "SK", PathStyle: true, HTTP: bmgrRemoteHTTP(true)}
	if err := cl.Put(context.Background(), "k", []byte("x"), gocdn.PutOptions{}); err == nil {
		t.Error("a redirected PUT counted as success")
	}
	if hits != 0 {
		t.Errorf("the redirect target was reached %d times", hits)
	}
}

func TestBmgrRemoteRedact(t *testing.T) {
	st := bmgrRemoteSettings{AccessKey: "AKIAEXAMPLEKEY", SecretKey: "wJalrXUtnFEMI/K7MDENG", Passphrase: "a long passphrase"}
	err := st.redact(errors.New(`dial: wJalrXUtnFEMI/K7MDENG with a long passphrase for AKIAEXAMPLEKEY`))
	for _, secret := range []string{st.AccessKey, st.SecretKey, st.Passphrase} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%q left in %q", secret, err)
		}
	}
	if st.redact(nil) != nil {
		t.Error("nil error changed")
	}
}

func TestBmgrRemoteCleanPrefix(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "backups": "backups/", "/a/b/": "a/b/", "a//b": "a/b/", " x ": "x/"} {
		if got := bmgrRemoteCleanPrefix(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// bmgrRemotePutStream: one PUT for a small stream, a multipart upload with retried parts for a
// large one, and an aborted upload (no orphaned parts) when a part keeps failing.
func TestBmgrRemotePutStream(t *testing.T) {
	fake := s3fake.New("AK", "SK-secret", "us-east-1", "bk")
	defer fake.Close()
	fake.MinPartSize = 1024
	old := bmgrRemotePartSize
	bmgrRemotePartSize = 64 << 10
	defer func() { bmgrRemotePartSize = old }()
	failParts := &partFailer{next: bmgrRemoteHTTP(true).Transport}
	cl := &gocdn.Client{Endpoint: fake.URL, Region: "us-east-1", Bucket: "bk", AccessKey: "AK", SecretKey: "SK-secret", PathStyle: true,
		HTTP: &http.Client{Transport: failParts}}
	ctx := context.Background()

	small := []byte("tiny backup")
	if n, err := bmgrRemotePutStream(ctx, cl, "p/small", bytes.NewReader(small), 0, nil); err != nil || n != int64(len(small)) {
		t.Fatalf("small: %d %v", n, err)
	}
	if o := fake.Object("bk", "p/small"); o == nil || !bytes.Equal(o.Body, small) {
		t.Fatal("small object not stored")
	}

	big := make([]byte, 5*64<<10+777)
	_, _ = rand.Read(big)
	failParts.fail.Store(1) // one part fails once and is retried
	var last int64
	n, err := bmgrRemotePutStream(ctx, cl, "p/big", bytes.NewReader(big), int64(len(big)), func(done int64) { last = done })
	if err != nil || n != int64(len(big)) || last != n {
		t.Fatalf("big: %d %v (progress %d)", n, err, last)
	}
	if o := fake.Object("bk", "p/big"); o == nil || !bytes.Equal(o.Body, big) || o.Parts != 6 {
		t.Fatal("multipart object differs")
	}
	if fake.OpenUploads() != 0 {
		t.Errorf("open uploads=%d", fake.OpenUploads())
	}

	failParts.fail.Store(1000) // every part fails: the upload is aborted
	if _, err := bmgrRemotePutStream(ctx, cl, "p/fails", bytes.NewReader(big), 0, nil); err == nil {
		t.Fatal("an upload whose parts all fail succeeded")
	}
	failParts.fail.Store(0)
	if fake.Object("bk", "p/fails") != nil || fake.OpenUploads() != 0 {
		t.Errorf("a failed upload left an object or %d open uploads", fake.OpenUploads())
	}

	// A reader error mid-stream aborts too.
	r := io.MultiReader(bytes.NewReader(big[:200<<10]), iotestErrReader{})
	if _, err := bmgrRemotePutStream(ctx, cl, "p/broken", r, 0, nil); err == nil {
		t.Fatal("a broken stream was uploaded")
	}
	if fake.Object("bk", "p/broken") != nil || fake.OpenUploads() != 0 {
		t.Error("a broken stream left an object or an open upload")
	}
}

// partFailer answers UploadPart requests with 500 while fail is above zero.
type partFailer struct {
	next http.RoundTripper
	fail atomic.Int64
}

func (p *partFailer) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.RawQuery, "partNumber=") && p.fail.Add(-1) >= 0 {
		if r.Body != nil {
			r.Body.Close()
		}
		return &http.Response{StatusCode: 500, Status: "500 Internal Server Error", Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader("<Error><Code>InternalError</Code></Error>"))}, nil
	}
	if p.fail.Load() < 0 {
		p.fail.Store(0)
	}
	return p.next.RoundTrip(r)
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("disk read error") }

// An object uploaded through the encrypting pipe decrypts to the original.
func TestBmgrRemoteEncryptedPipe(t *testing.T) {
	fake := s3fake.New("AK", "SK", "us-east-1", "bk")
	defer fake.Close()
	fake.MinPartSize = 1024
	old := bmgrRemotePartSize
	bmgrRemotePartSize = 32 << 10
	defer func() { bmgrRemotePartSize = old }()
	cl := &gocdn.Client{Endpoint: fake.URL, Region: "us-east-1", Bucket: "bk", AccessKey: "AK", SecretKey: "SK", PathStyle: true, HTTP: bmgrRemoteHTTP(true)}
	plain := bytes.Repeat([]byte("PK\x03\x04 archive data "), 30000)
	pr, pw := io.Pipe()
	go func() {
		_, err := backupcrypt.EncryptStream(pw, bytes.NewReader(plain), []byte("passphrase-1234"))
		pw.CloseWithError(err)
	}()
	if _, err := bmgrRemotePutStream(context.Background(), cl, "x.zip.enc", pr, int64(len(plain)), nil); err != nil {
		t.Fatal(err)
	}
	o := fake.Object("bk", "x.zip.enc")
	if o == nil || bytes.Contains(o.Body, []byte("archive data")) {
		t.Fatal("stored object missing or readable")
	}
	var back bytes.Buffer
	if _, err := backupcrypt.DecryptStream(&back, bytes.NewReader(o.Body), []byte("passphrase-1234")); err != nil || !bytes.Equal(back.Bytes(), plain) {
		t.Fatalf("decrypt: %v", err)
	}
}
