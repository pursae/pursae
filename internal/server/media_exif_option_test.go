package server

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Settings › Media "Remove location and camera data from uploaded photos": on by default (the
// stored JPEG loses its GPS data); unticked, the upload keeps its EXIF as sent.
func TestMediaStripEXIFOption(t *testing.T) {
	t.Setenv("PURSAE_KEEP_EXIF", "")
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	var uid int64
	_ = srv.store.Pool.QueryRow(ctx, `SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&uid)

	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, image.NewRGBA(image.Rect(0, 0, 40, 30)), nil); err != nil {
		t.Fatal(err)
	}
	gps := []byte("Exif\x00\x00GPSLatitude=41.0")
	photo := append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0, byte(len(gps) + 2)}, gps...)
	photo = append(photo, enc.Bytes()[2:]...)
	stored := func(name string) []byte {
		t.Helper()
		a, err := srv.insertAttachmentFile(ctx, name, bytes.NewReader(photo), uid, 0)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(srv.cfg.UploadsDir, filepath.FromSlash(a.FilePath)))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	_, body, _ := get(t, c, ts.URL+"/wp-admin/options-media")
	if !strings.Contains(body, `id="pursae_strip_exif" value="1" aria-describedby="pursae-strip-exif-description" checked="checked"`) {
		t.Fatal("strip EXIF checkbox missing or not ticked by default")
	}
	if bytes.Contains(stored("on.jpg"), []byte("GPSLatitude")) {
		t.Fatal("default: GPS data kept in the stored photo")
	}

	form := url.Values{"option_page": {"media"}, "_wpnonce": {formNonce(t, body)}, "uploads_use_yearmonth_folders": {"1"}}
	if code, _, _ := post(t, c, ts.URL+"/wp-admin/options", form); code != http.StatusFound || srv.settings.Bool("pursae_strip_exif") {
		t.Fatalf("save unticked: %d, option %v", code, srv.settings.Bool("pursae_strip_exif"))
	}
	if !bytes.Contains(stored("off.jpg"), []byte("GPSLatitude")) {
		t.Fatal("option off: EXIF removed anyway")
	}
}
