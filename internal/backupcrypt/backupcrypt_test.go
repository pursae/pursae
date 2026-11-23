package backupcrypt

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

const testCost = minLogN // cheap scrypt for tests

func encrypt(t *testing.T, plain, pass []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriterCost(&out, pass, testCost)
	if err != nil {
		t.Fatal(err)
	}
	// Odd-sized writes cross chunk boundaries.
	for p := plain; len(p) > 0; {
		n := min(len(p), 7777)
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func decrypt(cipherText, pass []byte) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(cipherText), pass)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestRoundTrip(t *testing.T) {
	pass := []byte("correct horse battery staple")
	for _, size := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3 * ChunkSize, 3*ChunkSize + 12345} {
		plain := make([]byte, size)
		_, _ = rand.Read(plain)
		ct := encrypt(t, plain, pass)
		if int64(len(ct)) != int64(size)+Overhead(int64(size)) {
			t.Errorf("size %d: cipher text %d bytes, Overhead says %d", size, len(ct), int64(size)+Overhead(int64(size)))
		}
		if !IsEncrypted(ct) {
			t.Errorf("size %d: no magic", size)
		}
		if size > 64 && bytes.Contains(ct, plain[:64]) {
			t.Errorf("size %d: plain text visible in the output", size)
		}
		got, err := decrypt(ct, pass)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size %d: round trip differs", size)
		}
		// One byte at a time through the reader as well.
		r, _ := NewReader(iotest.OneByteReader(bytes.NewReader(ct)), pass)
		if got, err := io.ReadAll(iotest.OneByteReader(r)); err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("size %d: byte-wise read: %v", size, err)
		}
	}
}

func TestStreamHelpers(t *testing.T) {
	plain := bytes.Repeat([]byte("pursae backup "), 20000)
	var ct, back bytes.Buffer
	// EncryptStream uses the default cost.
	if _, err := EncryptStream(&ct, bytes.NewReader(plain), []byte("pw")); err != nil {
		t.Fatal(err)
	}
	if ct.Bytes()[5] != DefaultLogN {
		t.Errorf("cost byte %d", ct.Bytes()[5])
	}
	if _, err := DecryptStream(&back, &ct, []byte("pw")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.Bytes(), plain) {
		t.Fatal("stream round trip differs")
	}
}

func TestWrongPassphraseAndTampering(t *testing.T) {
	pass := []byte("s3cret passphrase")
	plain := make([]byte, 2*ChunkSize+100)
	_, _ = rand.Read(plain)
	ct := encrypt(t, plain, pass)

	if _, err := decrypt(ct, []byte("wrong passphrase")); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if _, err := NewReader(bytes.NewReader(ct), nil); err == nil {
		t.Error("an empty passphrase was accepted")
	}
	flip := func(i int) []byte {
		c := bytes.Clone(ct)
		c[i] ^= 1
		return c
	}
	for name, c := range map[string][]byte{
		"salt":         flip(10),
		"nonce prefix": flip(HeaderSize - 1),
		"first chunk":  flip(HeaderSize + 5),
		"last chunk":   flip(len(ct) - 3),
	} {
		if _, err := decrypt(c, pass); !errors.Is(err, ErrAuth) {
			t.Errorf("%s changed: %v", name, err)
		}
	}
	full := ChunkSize + tagSize
	for name, c := range map[string][]byte{
		"cut at a chunk boundary": ct[:HeaderSize+full],
		"cut inside a chunk":      ct[:HeaderSize+full+50],
		"header only":             ct[:HeaderSize],
		"last chunk dropped":      ct[:HeaderSize+2*full],
		"chunks swapped":          append(append(append(bytes.Clone(ct[:HeaderSize]), ct[HeaderSize+full:HeaderSize+2*full]...), ct[HeaderSize:HeaderSize+full]...), ct[HeaderSize+2*full:]...),
		"extra bytes":             append(bytes.Clone(ct), 0),
	} {
		if _, err := decrypt(c, pass); !errors.Is(err, ErrAuth) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Data read before the damage is authenticated; the reader never reports a clean EOF early.
	r, _ := NewReader(bytes.NewReader(ct[:HeaderSize+full+50]), pass)
	got, err := io.ReadAll(r)
	if !errors.Is(err, ErrAuth) || !bytes.Equal(got, plain[:ChunkSize]) {
		t.Errorf("truncated read: %d bytes, %v", len(got), err)
	}
}

func TestNotEncrypted(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("PK\x03\x04 a zip file, not encrypted....................")} {
		if _, err := NewReader(bytes.NewReader(b), []byte("x")); !errors.Is(err, ErrFormat) {
			t.Errorf("%q: %v", b, err)
		}
	}
	// A header asking for an absurd scrypt cost is refused before any work.
	var out bytes.Buffer
	w, _ := NewWriterCost(&out, []byte("x"), testCost)
	_ = w.Close()
	c := out.Bytes()
	c[5] = 40
	if _, err := NewReader(bytes.NewReader(c), []byte("x")); !errors.Is(err, ErrFormat) {
		t.Errorf("cost 40: %v", err)
	}
	c[5], c[4] = testCost, 9
	if _, err := NewReader(bytes.NewReader(c), []byte("x")); err == nil {
		t.Error("an unknown version was accepted")
	}
}

// Two encryptions of the same data with the same passphrase share nothing (fresh salt and nonce).
func TestFreshSaltAndNonce(t *testing.T) {
	a := encrypt(t, []byte("same"), []byte("pw"))
	b := encrypt(t, []byte("same"), []byte("pw"))
	if bytes.Equal(a[6:HeaderSize], b[6:HeaderSize]) || bytes.Equal(a[HeaderSize:], b[HeaderSize:]) {
		t.Error("salt or nonce reused")
	}
}

// A header asking for an scrypt cost above maxLogN is refused before any key is derived, so a file
// planted in the bucket cannot make a restore allocate a gigabyte or more.
func TestCostBound(t *testing.T) {
	var out bytes.Buffer
	w, err := NewWriterCost(&out, []byte("x"), testCost)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	b := out.Bytes()
	for _, logN := range []byte{19, 20, 31} {
		b[5] = logN
		if _, err := NewReader(bytes.NewReader(b), []byte("x")); !errors.Is(err, ErrFormat) {
			t.Errorf("logN %d: err=%v, want ErrFormat", logN, err)
		}
		if _, err := NewWriterCost(io.Discard, []byte("x"), int(logN)); err == nil {
			t.Errorf("NewWriterCost accepted logN %d", logN)
		}
	}
	if DefaultLogN > maxLogN {
		t.Fatal("the default cost is above the bound")
	}
}
