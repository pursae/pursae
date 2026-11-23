// Package backupcrypt encrypts backup archives with a passphrase, as a stream: a file of any size
// is encrypted or decrypted with a fixed amount of memory (one 64 KiB chunk), so a backup can be
// encrypted on its way to remote storage and decrypted on its way back without ever being held in
// memory or written to disk in plain text twice.
//
// Format (version 1):
//
//	header   "PSBK" | version (1) | scrypt log2(N) (1) | salt (16) | nonce prefix (7)   = 29 bytes
//	chunks   AES-256-GCM sealed chunks of 64 KiB plain text (the last one shorter, possibly empty)
//
// The key is scrypt(passphrase, salt, N, r=8, p=1). Chunk i is sealed with the nonce
// prefix | uint32 big-endian i | last flag (1 for the final chunk, else 0) and the header as
// additional data, so chunks cannot be reordered, dropped, repeated or cut off at a chunk boundary
// (the STREAM construction of age and Tink), and a changed header (another salt, cost or prefix)
// fails the first chunk. A wrong passphrase also fails the first chunk: ErrAuth.
package backupcrypt

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/scrypt"
)

const (
	magic      = "PSBK"
	version    = 1
	saltSize   = 16
	prefixSize = 7
	// HeaderSize is the length of the header in front of the chunks.
	HeaderSize = len(magic) + 2 + saltSize + prefixSize
	// ChunkSize is the plain text in every chunk but the last.
	ChunkSize = 64 << 10
	tagSize   = 16
	// DefaultLogN is the scrypt cost of new files: N = 2^16 (64 MiB of memory, a fraction of a second).
	DefaultLogN = 16
	// maxLogN bounds the cost a file may ask for, so a crafted header cannot make the reader
	// allocate gigabytes: scrypt needs 128·r·N bytes, 256 MiB at 2^18 (1 GiB at 2^20).
	maxLogN = 18
	minLogN = 10
	// Suffix is the file-name suffix of an encrypted archive.
	Suffix = ".enc"
)

// ErrAuth is returned when a chunk does not open: the passphrase is wrong, or the file was changed,
// damaged or cut short.
var ErrAuth = errors.New("the passphrase does not open this backup (wrong passphrase, or the file is damaged)")

// ErrFormat is returned for a file that is not an encrypted Pursae backup.
var ErrFormat = errors.New("not an encrypted Pursae backup")

// IsEncrypted reports whether b starts like an encrypted backup.
func IsEncrypted(b []byte) bool { return bytes.HasPrefix(b, []byte(magic)) }

// Overhead is how many bytes encryption adds to n bytes of plain text.
func Overhead(n int64) int64 {
	chunks := n/ChunkSize + 1 // the final chunk exists even when n is a multiple of ChunkSize (or 0)
	if n > 0 && n%ChunkSize == 0 {
		chunks = n / ChunkSize
	}
	return int64(HeaderSize) + chunks*tagSize
}

func deriveKey(pass []byte, salt []byte, logN int) (cipher.AEAD, error) {
	if len(pass) == 0 {
		return nil, errors.New("no passphrase")
	}
	key, err := scrypt.Key(pass, salt, 1<<logN, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(prefix []byte, n uint32, last bool) []byte {
	b := make([]byte, 12)
	copy(b, prefix)
	binary.BigEndian.PutUint32(b[prefixSize:], n)
	if last {
		b[11] = 1
	}
	return b
}

// Writer encrypts what is written to it; Close writes the final chunk (it does not close the
// underlying writer).
type Writer struct {
	w      io.Writer
	aead   cipher.AEAD
	header []byte
	prefix []byte
	buf    []byte
	out    []byte
	n      uint32
	err    error
	closed bool
}

// NewWriter writes the header to w and returns a Writer that encrypts with pass.
func NewWriter(w io.Writer, pass []byte) (*Writer, error) { return NewWriterCost(w, pass, DefaultLogN) }

// NewWriterCost is NewWriter with another scrypt cost (tests use a cheap one).
func NewWriterCost(w io.Writer, pass []byte, logN int) (*Writer, error) {
	if logN < minLogN || logN > maxLogN {
		return nil, fmt.Errorf("scrypt cost %d out of range", logN)
	}
	header := make([]byte, HeaderSize)
	copy(header, magic)
	header[4], header[5] = version, byte(logN)
	if _, err := rand.Read(header[6:]); err != nil {
		return nil, err
	}
	aead, err := deriveKey(pass, header[6:6+saltSize], logN)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(header); err != nil {
		return nil, err
	}
	return &Writer{w: w, aead: aead, header: header, prefix: header[6+saltSize:], buf: make([]byte, 0, ChunkSize),
		out: make([]byte, 0, ChunkSize+tagSize)}, nil
}

// Write encrypts p. A full chunk is held back until more data (or Close) shows whether it is the last.
func (e *Writer) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if e.closed {
		return 0, errors.New("backupcrypt: write after close")
	}
	total := len(p)
	for len(p) > 0 {
		if len(e.buf) == ChunkSize {
			if err := e.seal(false); err != nil {
				return total - len(p), err
			}
		}
		n := copy(e.buf[len(e.buf):ChunkSize], p)
		e.buf = e.buf[:len(e.buf)+n]
		p = p[n:]
	}
	return total, nil
}

func (e *Writer) seal(last bool) error {
	if e.n == ^uint32(0) {
		e.err = errors.New("backupcrypt: file too large")
		return e.err
	}
	e.out = e.aead.Seal(e.out[:0], nonce(e.prefix, e.n, last), e.buf, e.header)
	e.n++
	e.buf = e.buf[:0]
	if _, err := e.w.Write(e.out); err != nil {
		e.err = err
		return err
	}
	return nil
}

// Close writes the final chunk.
func (e *Writer) Close() error {
	if e.closed {
		return e.err
	}
	e.closed = true
	if e.err != nil {
		return e.err
	}
	return e.seal(true)
}

// Reader decrypts an encrypted backup. Every byte it returns has been authenticated; a file cut
// short or changed anywhere returns ErrAuth, never io.EOF, before its end.
type Reader struct {
	r      *bufio.Reader
	aead   cipher.AEAD
	header []byte
	prefix []byte
	in     []byte
	plain  []byte
	n      uint32
	done   bool
	err    error
}

// NewReader reads the header from r and returns a Reader that decrypts with pass.
func NewReader(r io.Reader, pass []byte) (*Reader, error) {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrFormat
		}
		return nil, err
	}
	if string(header[:4]) != magic {
		return nil, ErrFormat
	}
	if header[4] != version {
		return nil, fmt.Errorf("encrypted backup format %d is not supported by this version", header[4])
	}
	logN := int(header[5])
	if logN < minLogN || logN > maxLogN {
		return nil, ErrFormat
	}
	aead, err := deriveKey(pass, header[6:6+saltSize], logN)
	if err != nil {
		return nil, err
	}
	return &Reader{r: bufio.NewReaderSize(r, ChunkSize+tagSize+1), aead: aead, header: header, prefix: header[6+saltSize:],
		in: make([]byte, ChunkSize+tagSize)}, nil
}

func (d *Reader) Read(p []byte) (int, error) {
	for len(d.plain) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			return 0, io.EOF
		}
		d.err = d.next()
	}
	n := copy(p, d.plain)
	d.plain = d.plain[n:]
	return n, nil
}

// next reads and opens one chunk. A chunk shorter than a full one is the last; a full one is the
// last when nothing follows it.
func (d *Reader) next() error {
	n, err := io.ReadFull(d.r, d.in)
	last := false
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		last = true
	case err != nil:
		return err
	default:
		if _, perr := d.r.Peek(1); errors.Is(perr, io.EOF) {
			last = true
		} else if perr != nil {
			return perr
		}
	}
	if n < tagSize {
		return ErrAuth
	}
	plain, oerr := d.aead.Open(d.in[:0], nonce(d.prefix, d.n, last), d.in[:n], d.header)
	if oerr != nil {
		return ErrAuth
	}
	d.n++
	d.plain = plain
	d.done = last
	return nil
}

// EncryptStream copies src to dst encrypted with pass.
func EncryptStream(dst io.Writer, src io.Reader, pass []byte) (int64, error) {
	w, err := NewWriter(dst, pass)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(w, src)
	if err != nil {
		return n, err
	}
	return n, w.Close()
}

// DecryptStream copies the decryption of src to dst.
func DecryptStream(dst io.Writer, src io.Reader, pass []byte) (int64, error) {
	r, err := NewReader(src, pass)
	if err != nil {
		return 0, err
	}
	return io.Copy(dst, r)
}
