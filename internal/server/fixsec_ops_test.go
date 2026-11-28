package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gomail "wpgo/internal/mail"
)

// #37: with ACF-Go active (its middleware parses every admin POST form) the Backup Manager upload
// still receives the archive instead of reporting "No file was received.".
func TestBmgrUploadWithACFGoActive(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	acfgoActivate(t, c, ts.URL)
	ctx := context.Background()
	if err := srv.bmgrPluginAction(ctx, "activate"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSAE_BACKUPS_DIR", t.TempDir())
	var archive string
	bmgrRun(t, srv, "backup", func(j *bmgrJob) (err error) {
		archive, err = srv.bmgrRunBackup(ctx, j, bmgrSelection{Categories: []string{"content"}, UserMode: "all"})
		return err
	})
	data, err := os.ReadFile(filepath.Join(srv.bmgrDir(), archive))
	if err != nil {
		t.Fatal(err)
	}
	_, body, _ := get(t, c, ts.URL+"/wp-admin/admin?page=backup-manager&tab=backups")
	nonce := formNonceOrLink(t, body)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("archive", "site.zip")
	_, _ = fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/wp-admin/admin?page=backup-manager&action=upload&_wpnonce="+nonce, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	noRedirect := *c
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	up, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	up.Body.Close()
	if loc := up.Header.Get("Location"); up.StatusCode != http.StatusFound || !strings.Contains(loc, "tab=restore") {
		t.Fatalf("upload: %d %s", up.StatusCode, loc)
	}
}

// #36: a small archive that unpacks far beyond the limit is refused before anything is written.
func TestBmgrUnpackBudget(t *testing.T) {
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("files/uploads/big.bin")
	_, _ = w.Write(make([]byte, 4<<20)) // 4 MiB of zeros, a few KB packed
	zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(zb.Bytes()), int64(zb.Len()))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSAE_BACKUP_MAX_UNPACKED", "1M")
	if got := bmgrMaxUnpacked(); got != 1<<20 {
		t.Fatalf("PURSAE_BACKUP_MAX_UNPACKED=1M parsed as %d", got)
	}
	b := bmgrNewUnpackBudget(zr)
	if err := b.precheck(zr); !errors.Is(err, errBmgrTooLarge) {
		t.Fatalf("precheck: %v", err)
	}
	// The written bytes are counted too (a header that understates the size).
	dst := filepath.Join(t.TempDir(), "big.bin")
	b = &bmgrUnpackBudget{limit: 1 << 20}
	if err := bmgrExtractN(zr.File[0], dst, &b.used, b.limit); !errors.Is(err, errBmgrTooLarge) {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatal("the partial file was left behind")
	}
	t.Setenv("PURSAE_BACKUP_MAX_UNPACKED", "")
	if b := bmgrNewUnpackBudget(zr); b.precheck(zr) != nil || b.limit != bmgrDefaultMaxUnpacked {
		t.Fatalf("default limit %d refused a 4 MiB archive", b.limit)
	}
}

// #32: the admin SMTP mailer refuses private addresses unless PURSAE_ALLOW_PRIVATE_ENDPOINTS is
// set, and the Email Test line is a fixed category.
func TestMailSMTPPrivateGuard(t *testing.T) {
	saved := msTestBase
	msTestBase = ""
	defer func() { msTestBase = saved }()
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "")
	for _, a := range []string{"127.0.0.1:25", "10.0.0.5:25", "169.254.169.254:80", "[::ffff:127.0.0.1]:25", "168.63.129.16:25"} {
		if msSMTPDialControl("tcp", a, nil) == nil {
			t.Errorf("%s was allowed", a)
		}
	}
	c := &gomail.SMTP{Host: "127.0.0.1", Port: "25", Control: msSMTPDialControl}
	_, err := c.Send(context.Background(), "a@example.test", []string{"b@example.test"}, []byte("x"))
	if msg := msSMTPErrorMessage(err); msg != smtpPrivateHostMessage {
		t.Fatalf("refusal message %q (%v)", msg, err)
	}
	// Any other failure keeps its own category: the private-network line is only for the guard.
	if msg := msSMTPErrorMessage(&gomail.SendError{Stage: gomail.StageAuth, Err: errors.New("535 banner")}); msg != "SMTP error: authentication failed." {
		t.Fatalf("auth failure message %q", msg)
	}
	t.Setenv("PURSAE_ALLOW_PRIVATE_ENDPOINTS", "1")
	if err := msSMTPDialControl("tcp", "127.0.0.1:25", nil); err != nil {
		t.Fatalf("opt-out refused loopback: %v", err)
	}
	if msSMTPDialControl("tcp", "169.254.169.254:25", nil) == nil {
		t.Fatal("opt-out admitted the metadata address")
	}
}
