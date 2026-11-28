package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// keepEXIF is the server-wide opt-out of StripJPEGMetadata (PURSAE_KEEP_EXIF=1): a photo archive
// that wants camera and location data served with its originals. A single site turns it off in
// Settings › Media instead (Options.KeepEXIF).
func keepEXIF() bool { return os.Getenv("PURSAE_KEEP_EXIF") == "1" }

// errNotJPEG stops stripJPEG on a header that is not a well-formed JPEG: the file is left as is.
var errNotJPEG = errors.New("media: not a well-formed JPEG header")

// StripJPEGMetadata removes the EXIF (APP1 "Exif") and XMP (APP1 Adobe namespace) segments of a
// JPEG: an uploaded photo's GPS position, camera serial and the like would otherwise be served to
// anyone who opens the original. The orientation, which the browser still needs, is kept in a
// minimal EXIF segment. It returns the cleaned bytes and whether anything changed; data that is
// not a well-formed JPEG header is left as it is.
func StripJPEGMetadata(data []byte) ([]byte, bool) {
	var out bytes.Buffer
	changed, err := stripJPEG(bytes.NewReader(data), &out, JPEGOrientation(bytes.NewReader(data)))
	if err != nil || !changed {
		return data, false
	}
	return out.Bytes(), true
}

// stripJPEG copies the JPEG in r to w without its EXIF/XMP segments, writing a minimal orientation
// segment in place of the first one when orientation > 1. Only the header (the segments before
// the start of scan, each at most 64 KiB) is parsed; the image data is streamed, so memory does
// not grow with the file. errNotJPEG means the header is malformed and w holds garbage.
func stripJPEG(r io.Reader, w io.Writer, orientation int) (changed bool, err error) {
	br := bufio.NewReader(r)
	var soi [2]byte
	if _, err := io.ReadFull(br, soi[:]); err != nil || soi != [2]byte{0xFF, 0xD8} {
		return false, errNotJPEG
	}
	if _, err := w.Write(soi[:]); err != nil {
		return false, err
	}
	wroteOrientation := false
	seg := make([]byte, 2+0xFFFF) // marker + the largest segment
	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			return changed, nil // a header without image data: nothing left to copy
		}
		if err != nil {
			return false, err
		}
		if b != 0xFF {
			return false, errNotJPEG
		}
		m, err := br.ReadByte()
		for err == nil && m == 0xFF { // fill bytes
			m, err = br.ReadByte()
		}
		if err != nil {
			return false, errNotJPEG
		}
		if m == 0xDA || m == 0xD9 { // start of scan: the rest is image data
			if _, err := w.Write([]byte{0xFF, m}); err != nil {
				return false, err
			}
			_, err := io.Copy(w, br)
			return changed, err
		}
		if m == 0x01 || m >= 0xD0 && m <= 0xD7 {
			if _, err := w.Write([]byte{0xFF, m}); err != nil {
				return false, err
			}
			continue
		}
		if _, err := io.ReadFull(br, seg[:4][2:4]); err != nil {
			return false, errNotJPEG
		}
		seg[0], seg[1] = 0xFF, m
		length := int(binary.BigEndian.Uint16(seg[2:4]))
		if length < 2 {
			return false, errNotJPEG
		}
		if _, err := io.ReadFull(br, seg[4:2+length]); err != nil {
			return false, errNotJPEG
		}
		payload := seg[4 : 2+length]
		if m == 0xE1 && (bytes.HasPrefix(payload, []byte("Exif\x00\x00")) || bytes.HasPrefix(payload, []byte("http://ns.adobe.com/xap/1.0/"))) {
			changed = true
			if orientation > 1 && !wroteOrientation {
				if _, err := w.Write(orientationAPP1(orientation)); err != nil {
					return false, err
				}
				wroteOrientation = true
			}
			continue
		}
		if _, err := w.Write(seg[:2+length]); err != nil {
			return false, err
		}
	}
}

// orientationAPP1 is an APP1 segment whose EXIF holds only the Orientation tag.
func orientationAPP1(o int) []byte {
	tiff := []byte{'M', 'M', 0, 0x2A, 0, 0, 0, 8, // big endian, IFD0 at 8
		0, 1, // one entry
		0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, byte(o), 0, 0, // Orientation, SHORT, 1, value
		0, 0, 0, 0} // no next IFD
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	return append(seg, payload...)
}

// stripJPEGFile rewrites a stored JPEG without its EXIF/XMP metadata (see StripJPEGMetadata),
// unless keep (Options.KeepEXIF) or PURSAE_KEEP_EXIF=1 says to keep it. The cleaned copy is
// streamed to a temporary file next to the original and renamed over it, keeping the file mode; a
// file without metadata or with a malformed header is left untouched.
func stripJPEGFile(full string, keep bool) {
	if keep || keepEXIF() {
		return
	}
	f, err := os.Open(full)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	orientation := JPEGOrientation(bufio.NewReader(f))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".exif-*")
	if err != nil {
		return
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	bw := bufio.NewWriterSize(tmp, 64<<10)
	changed, err := stripJPEG(f, bw, orientation)
	if err != nil || !changed || bw.Flush() != nil || tmp.Chmod(st.Mode().Perm()) != nil || tmp.Close() != nil {
		return
	}
	f.Close() // before the rename, for Windows
	if os.Rename(tmp.Name(), full) != nil {
		os.Remove(tmp.Name())
	}
	done = true
}
