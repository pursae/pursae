package gocdn

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type initiateResponseTransport string

func (body initiateResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func TestCreateMultipartUploadValidatesResponseRoot(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`<InitiateMultipartUploadResult xmlns="urn:s3"><UploadId>valid</UploadId></InitiateMultipartUploadResult>`, "valid"},
		{`<WrongResult><UploadId>invalid</UploadId></WrongResult>`, ""},
		{`<InitiateMultipartUploadResult></InitiateMultipartUploadResult>`, ""},
	} {
		c := &Client{Endpoint: "https://s3.example.test", Bucket: "bucket", AccessKey: "key", SecretKey: "secret", PathStyle: true,
			HTTP: &http.Client{Transport: initiateResponseTransport(tc.body)}}
		got, err := c.CreateMultipartUpload(context.Background(), "object", PutOptions{})
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("body=%q: id=%q err=%v, want %q", tc.body, got, err, tc.want)
		}
	}
}

type uploadPartResponseTransport struct{ calls int }

func (transport *uploadPartResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls++
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": {`"etag"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestUploadPartValidatesPartNumberBeforeRequest(t *testing.T) {
	for _, tc := range []struct {
		n       int
		wantErr bool
	}{
		{0, true}, {1, false}, {10000, false}, {10001, true},
	} {
		transport := &uploadPartResponseTransport{}
		c := &Client{Endpoint: "https://s3.example.test", Bucket: "bucket", AccessKey: "key", SecretKey: "secret", PathStyle: true,
			HTTP: &http.Client{Transport: transport}}
		_, err := c.UploadPart(context.Background(), "object", "upload-id", tc.n, []byte("part"))
		wantCalls := 1
		if tc.wantErr {
			wantCalls = 0
		}
		if (err != nil) != tc.wantErr || transport.calls != wantCalls {
			t.Errorf("part %d: calls=%d err=%v, wantErr=%v", tc.n, transport.calls, err, tc.wantErr)
		}
	}
}

type failedResponseReader struct{}

func (failedResponseReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestErrorInBodyPreservesReadFailure(t *testing.T) {
	for _, prefix := range []string{"", "<Error><Code>", "<CopyObjectResult>"} {
		t.Run(prefix, func(t *testing.T) {
			body := io.NopCloser(io.MultiReader(strings.NewReader(prefix), failedResponseReader{}))
			defer body.Close()
			err := errorInBody(&http.Response{StatusCode: http.StatusOK, Body: body})
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("got %v, want unexpected EOF", err)
			}
		})
	}
}

func TestErrorInBodyParsesXMLRoot(t *testing.T) {
	for _, tc := range []struct {
		body string
		code string
		fail bool
	}{
		{`<Error xmlns="urn:s3"><Code>InternalError</Code></Error>`, "InternalError", true},
		{`<s:Error xmlns:s="urn:s3"><s:Code>SlowDown</s:Code></s:Error>`, "SlowDown", true},
		{`<Error request="123"><Code>AccessDenied</Code></Error>`, "AccessDenied", true},
		{`<CopyObjectResult>`, "", true},
		{"", "", true},
		{`<CopyObjectResult><ETag>ok</ETag></CopyObjectResult>`, "", false},
		{`<CompleteMultipartUploadResult xmlns="urn:s3"><ETag>ok</ETag></CompleteMultipartUploadResult>`, "", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			body := io.NopCloser(strings.NewReader(tc.body))
			defer body.Close()
			err := errorInBody(&http.Response{StatusCode: http.StatusOK, Body: body})
			if (err != nil) != tc.fail {
				t.Fatalf("error = %v, want failure %v", err, tc.fail)
			}
			if tc.code != "" {
				var s3err *Error
				if !errors.As(err, &s3err) || s3err.Code != tc.code {
					t.Fatalf("got %v, want S3 code %s", err, tc.code)
				}
			}
		})
	}
}
