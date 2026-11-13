package gocdn_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"wpgo/internal/gocdn"
)

type rejectingUploadTransport struct{ calls int }

func (t *rejectingUploadTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, errors.New("unexpected request")
}

func TestUploadRejectsObjectAboveS3LimitBeforeRequest(t *testing.T) {
	transport := &rejectingUploadTransport{}
	c := &gocdn.Client{Endpoint: "https://s3.example.test", Bucket: "bucket", AccessKey: "key", SecretKey: "secret", PathStyle: true, HTTP: &http.Client{Transport: transport}}
	multipart, err := c.Upload(context.Background(), "too-large", bytes.NewReader(nil), gocdn.MaxObjectSize+1, gocdn.UploadOptions{Threshold: 1})
	if err == nil || multipart || transport.calls != 0 {
		t.Fatalf("multipart=%v calls=%d err=%v", multipart, transport.calls, err)
	}
}

func TestUploadCapsPartAllocationAtObjectSize(t *testing.T) {
	f := newFake(t)
	c := pathClient(f)
	data := []byte("x")
	multi, err := c.Upload(context.Background(), "small", bytes.NewReader(data), int64(len(data)), gocdn.UploadOptions{Threshold: 1, PartSize: math.MaxInt64})
	if err != nil || !multi {
		t.Fatalf("multipart upload: multipart=%t err=%v", multi, err)
	}
	if got := f.Object("media", "small"); got == nil || !bytes.Equal(got.Body, data) {
		t.Fatalf("uploaded object = %+v", got)
	}
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/1000)
	}
	return b
}

func TestUploadMultipart(t *testing.T) {
	f := newFake(t)
	c := pathClient(f)
	ctx := context.Background()
	data := pattern(5<<20 + 1234)
	opts := gocdn.UploadOptions{PutOptions: gocdn.PutOptions{ContentType: "video/mp4", CacheControl: "public, max-age=60", ACL: "public-read",
		SSE: "AES256", StorageClass: "STANDARD_IA"}, Threshold: 1 << 20, PartSize: 1}
	multi, err := c.Upload(ctx, "big/video.mp4", bytes.NewReader(data), int64(len(data)), opts)
	if err != nil || !multi {
		t.Fatalf("multipart upload: %v (multipart %v)", err, multi)
	}
	o := f.Object("media", "big/video.mp4")
	if o == nil || !bytes.Equal(o.Body, data) {
		t.Fatal("assembled object differs")
	}
	// PartSize below 5 MB is raised to S3's minimum: 5 MB + 1234 bytes = 2 parts.
	if o.Parts != 2 || o.ContentType != "video/mp4" || o.SSE != "AES256" || o.StorageClass != "STANDARD_IA" || o.ACL != "public-read" {
		t.Fatalf("object %d parts, %q %q %q %q", o.Parts, o.ContentType, o.SSE, o.StorageClass, o.ACL)
	}
	var create, parts, complete int
	for _, r := range f.Requests() {
		switch {
		case r.Method == "POST" && strings.HasPrefix(r.Query, "uploads"):
			create++
			if r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
				t.Error("SSE header not on CreateMultipartUpload")
			}
		case r.Method == "PUT" && strings.Contains(r.Query, "partNumber"):
			parts++
			if r.Header.Get("X-Amz-Server-Side-Encryption") != "" {
				t.Error("SSE header must not be sent with UploadPart")
			}
		case r.Method == "POST" && strings.Contains(r.Query, "uploadId"):
			complete++
		}
	}
	if create != 1 || parts != 2 || complete != 1 {
		t.Errorf("requests: create %d, parts %d, complete %d", create, parts, complete)
	}
	if len(f.Errors) != 0 || f.OpenUploads() != 0 {
		t.Fatalf("signature errors %v, open uploads %d", f.Errors, f.OpenUploads())
	}
	// HEAD shows SSE; GET returns the whole thing.
	rc, _, err := c.Get(ctx, "big/video.mp4")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("GET of the multipart object differs")
	}

	// Below the threshold: one PUT with the same headers.
	small := pattern(1000)
	if multi, err := c.Upload(ctx, "small.bin", bytes.NewReader(small), 1000, opts); err != nil || multi {
		t.Fatalf("small upload: %v %v", multi, err)
	}
	if o := f.Object("media", "small.bin"); o == nil || o.Parts != 0 || o.SSE != "AES256" || o.StorageClass != "STANDARD_IA" {
		t.Fatalf("small object %+v", o)
	}
	// Empty file.
	if _, err := c.Upload(ctx, "empty.txt", bytes.NewReader(nil), 0, opts); err != nil {
		t.Fatal(err)
	}
}

func TestUploadRejectsNegativeSize(t *testing.T) {
	f := newFake(t)
	c := pathClient(f)
	if multipart, err := c.Upload(context.Background(), "negative", bytes.NewReader(nil), -1, gocdn.UploadOptions{}); err == nil || multipart {
		t.Fatalf("multipart=%v error=%v", multipart, err)
	}
	if len(f.Requests()) != 0 {
		t.Fatalf("negative upload made %d requests", len(f.Requests()))
	}
}

func TestUploadMultipartRetriesAndAborts(t *testing.T) {
	f := newFake(t)
	c := pathClient(f)
	ctx := context.Background()
	data := pattern(11 << 20)
	opts := gocdn.UploadOptions{Threshold: 1 << 20, PartSize: 5 << 20}
	// The first part's request fails once (500) and is retried.
	failer := &failAfter{r: bytes.NewReader(data), f: func() { f.FailNext = 1 }}
	if _, err := c.Upload(ctx, "x.bin", failer, int64(len(data)), opts); err != nil {
		t.Fatalf("a retried part failed the upload: %v", err)
	}
	if o := f.Object("media", "x.bin"); o == nil || !bytes.Equal(o.Body, data) || o.Parts != 3 {
		t.Fatalf("object after retry: %v", o != nil)
	}
	// A part that keeps failing aborts the upload: nothing stored, no open upload left.
	f.FailNext = 0
	failer = &failAfter{r: bytes.NewReader(data), f: func() { f.FailNext = 3 }}
	if _, err := c.Upload(ctx, "y.bin", failer, int64(len(data)), opts); err == nil {
		t.Fatal("an upload whose part always fails succeeded")
	}
	if f.Object("media", "y.bin") != nil || f.OpenUploads() != 0 {
		t.Fatalf("after abort: object %v, open uploads %d", f.Object("media", "y.bin") != nil, f.OpenUploads())
	}
	// Invalid SSE / storage class values are refused by the server and reported.
	if err := c.Put(ctx, "z", []byte("x"), gocdn.PutOptions{StorageClass: "FAST"}); err == nil || !strings.Contains(err.Error(), "InvalidStorageClass") {
		t.Fatalf("invalid storage class: %v", err)
	}
}

// failAfter runs f once, on the first read (after CreateMultipartUpload was sent).
type failAfter struct {
	r    *bytes.Reader
	f    func()
	done bool
}

func (a *failAfter) ReadAt(p []byte, off int64) (int, error) {
	if !a.done {
		a.done = true
		a.f()
	}
	return a.r.ReadAt(p, off)
}

func TestCopyObject(t *testing.T) {
	f := newFake(t)
	f.AddBucket("other")
	c := pathClient(f)
	ctx := context.Background()
	if err := c.Put(ctx, "a/b c.jpg", []byte("img"), gocdn.PutOptions{ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	dst := pathClient(f)
	dst.Bucket = "other"
	if err := dst.Copy(ctx, "media", "a/b c.jpg", "new/b c.jpg", gocdn.PutOptions{ContentType: "image/jpeg", CacheControl: "immutable", SSE: "AES256"}); err != nil {
		t.Fatal(err)
	}
	o := f.Object("other", "new/b c.jpg")
	if o == nil || string(o.Body) != "img" || o.CacheControl != "immutable" || o.SSE != "AES256" {
		t.Fatalf("copied object %+v", o)
	}
	if err := dst.Copy(ctx, "media", "missing", "x", gocdn.PutOptions{}); !gocdn.IsNotFound(err) {
		t.Fatalf("copy of a missing key: %v", err)
	}
	if len(f.Errors) != 0 {
		t.Fatal(f.Errors)
	}
}
