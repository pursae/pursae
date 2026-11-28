package media

import (
	"bytes"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStripJPEGMetadataDropsGPSKeepsOrientation(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 4)), nil); err != nil {
		t.Fatal(err)
	}
	src := buf.Bytes()
	// An EXIF segment with orientation 6 plus a fake GPS marker string, and an XMP segment.
	exif := orientationAPP1(6)
	gps := []byte("Exif\x00\x00GPSLatitude=41.0")
	gpsSeg := append([]byte{0xFF, 0xE1, 0, byte(len(gps) + 2)}, gps...)
	xmp := []byte("http://ns.adobe.com/xap/1.0/\x00<x:gps/>")
	xmpSeg := append([]byte{0xFF, 0xE1, 0, byte(len(xmp) + 2)}, xmp...)
	data := append([]byte{0xFF, 0xD8}, exif...)
	data = append(data, gpsSeg...)
	data = append(data, xmpSeg...)
	data = append(data, src[2:]...)

	clean, changed := StripJPEGMetadata(data)
	if !changed {
		t.Fatal("metadata not stripped")
	}
	if bytes.Contains(clean, []byte("GPSLatitude")) || bytes.Contains(clean, []byte("x:gps")) {
		t.Fatal("GPS/XMP data kept")
	}
	if o := JPEGOrientation(bytes.NewReader(clean)); o != 6 {
		t.Fatalf("orientation = %d, want 6", o)
	}
	if _, err := jpeg.Decode(bytes.NewReader(clean)); err != nil {
		t.Fatalf("clean JPEG does not decode: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.jpg"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProcessImage(dir, "p.jpg", nil); err != nil {
		t.Fatal(err)
	}
	stored, _ := os.ReadFile(filepath.Join(dir, "p.jpg"))
	if bytes.Contains(stored, []byte("GPSLatitude")) {
		t.Fatal("stored original keeps GPS data")
	}
	t.Setenv("PURSAE_KEEP_EXIF", "1")
	os.WriteFile(filepath.Join(dir, "k.jpg"), data, 0o644)
	ProcessImage(dir, "k.jpg", nil)
	if kept, _ := os.ReadFile(filepath.Join(dir, "k.jpg")); !bytes.Contains(kept, []byte("GPSLatitude")) {
		t.Fatal("PURSAE_KEEP_EXIF=1 should keep the metadata")
	}
}

// The file path streams: a JPEG padded with a large comment segment run and a large scan is
// stripped through stripJPEGFile, the image data is copied byte for byte and the mode is kept.
func TestStripJPEGFileStreamsLargeFile(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	src := buf.Bytes()
	gps := []byte("Exif\x00\x00GPSLatitude=41.0")
	data := append([]byte{0xFF, 0xD8}, orientationAPP1(3)...)
	data = append(data, append([]byte{0xFF, 0xE1, 0, byte(len(gps) + 2)}, gps...)...)
	// 200 full-size COM segments (~13 MB of header) the stripper must pass through.
	com := make([]byte, 0xFFFF+2)
	com[0], com[1], com[2], com[3] = 0xFF, 0xFE, 0xFF, 0xFF
	for i := 0; i < 200; i++ {
		data = append(data, com...)
	}
	data = append(data, src[2:]...)

	dir := t.TempDir()
	full := filepath.Join(dir, "big.jpg")
	if err := os.WriteFile(full, data, 0o640); err != nil {
		t.Fatal(err)
	}
	stripJPEGFile(full, false)
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("GPSLatitude")) {
		t.Fatal("GPS data kept")
	}
	if want := len(data) - (len(gps) + 4); len(got) != want {
		t.Fatalf("size %d, want %d", len(got), want)
	}
	if !bytes.HasSuffix(got, src[2:]) {
		t.Fatal("image data changed")
	}
	if o := JPEGOrientation(bytes.NewReader(got)); o != 3 {
		t.Fatalf("orientation = %d, want 3", o)
	}
	if _, err := jpeg.Decode(bytes.NewReader(got)); err != nil {
		t.Fatalf("stripped file does not decode: %v", err)
	}
	// Windows has no Unix permission bits to keep.
	if st, _ := os.Stat(full); runtime.GOOS != "windows" && st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v, want 0640", st.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*exif*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}

	// Options.KeepEXIF (Settings › Media off) keeps the file as uploaded.
	os.WriteFile(full, data, 0o640)
	if _, err := ProcessImageWith(dir, "big.jpg", nil, Options{KeepEXIF: true}); err != nil {
		t.Fatal(err)
	}
	if kept, _ := os.ReadFile(full); !bytes.Equal(kept, data) {
		t.Fatal("KeepEXIF changed the file")
	}

	// A malformed header is left as it is.
	bad := append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0, 1}, gps...)
	os.WriteFile(full, bad, 0o640)
	stripJPEGFile(full, false)
	if kept, _ := os.ReadFile(full); !bytes.Equal(kept, bad) {
		t.Fatal("malformed JPEG rewritten")
	}
}
