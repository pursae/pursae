package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
)

func t2nMultipart(t *testing.T, size int, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	fw, err := w.CreateFormFile("file", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(bytes.Repeat([]byte("x"), size))
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

// A request without a signed-in user or credentials has a small body limit, except on the routes
// that bound uploads themselves; signed-in users keep the server-wide limit.
func TestT2NMiscAnonymousBodyLimit(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	old := anonMaxBody
	anonMaxBody = 1 << 20
	t.Cleanup(func() { anonMaxBody = old })

	send := func(client *http.Client, path string, hdr map[string]string) int {
		body, ct := t2nMultipart(t, 2<<20, map[string]string{"comment": "x"})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, body)
		req.Header.Set("Content-Type", ct)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{"/", "/some-page/", "/wp-json/wp/v2/posts", "/wp-admin/admin-ajax.php"} {
		if code := send(anon, p, nil); code != http.StatusRequestEntityTooLarge {
			t.Errorf("anonymous 2 MB POST to %s: %d, want 413", p, code)
		}
	}
	// A bogus session cookie is still anonymous.
	if code := send(anon, "/wp-admin/admin-ajax.php", map[string]string{"Cookie": auth.CookieName + "=bogus"}); code != http.StatusRequestEntityTooLarge {
		t.Errorf("anonymous POST with a bogus cookie: %d", code)
	}
	for _, p := range []string{"/wp-json/contact-form-7/v1/contact-forms/1/feedback", "/?goo-ajax=addon_upload", "/xmlrpc.php"} {
		if code := send(anon, p, nil); code == http.StatusRequestEntityTooLarge {
			t.Errorf("self-bounded route %s refused by the anonymous limit", p)
		}
	}
	if code := send(anon, "/wp-json/wp/v2/media", map[string]string{"Authorization": "Basic YWRtaW46eA=="}); code == http.StatusRequestEntityTooLarge {
		t.Error("a request with credentials was refused by the anonymous limit")
	}
	if code := send(c, "/wp-admin/admin-ajax.php", nil); code == http.StatusRequestEntityTooLarge {
		t.Error("a signed-in user's POST was refused by the anonymous limit")
	}
}

// The heartbeat check runs before the session is known: it never parses a multipart body.
func TestT2NMiscHeartbeatCheckSkipsMultipart(t *testing.T) {
	body, ct := t2nMultipart(t, 1<<10, map[string]string{"action": "heartbeat"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/wp-admin/admin-ajax.php", body)
	c.Request.Header.Set("Content-Type", ct)
	isBackgroundPoll(c)
	if c.Request.MultipartForm != nil {
		t.Error("isBackgroundPoll parsed a multipart body")
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/wp-admin/admin-ajax.php", strings.NewReader("action=heartbeat&interval=60"))
	c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if !isBackgroundPoll(c) {
		t.Error("heartbeat.js's URL-encoded body is no longer recognised")
	}
}
