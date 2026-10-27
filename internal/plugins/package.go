package plugins

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Files inside a plugin package (a zip archive). Only these names are
// accepted, all at the archive root.
const (
	WasmFile      = "plugin.wasm"
	ReadmeFile    = "readme.md"
	IconFile      = "icon.svg"
	SignatureFile = "signature"
	// AssetsDir holds static files (css, js, images, fonts) the core serves
	// at /wp-content/plugins/<slug>/assets/…; see Manifest.Assets.
	AssetsDir = "assets"
)

// Per-file size limits (uncompressed).
var fileLimits = map[string]int64{
	ManifestFile:  maxManifestSize,
	WasmFile:      64 << 20,
	ReadmeFile:    1 << 20,
	IconFile:      256 << 10,
	SignatureFile: 1 << 10,
}

// digestDomain prefixes the signed digest so a plugin signature can never be
// confused with any other Ed25519 signature made with the same key.
const digestDomain = "wpgo-plugin-v1\n"

// Package errors.
var (
	// ErrUnsigned is returned when a signature is required but the package
	// has none.
	ErrUnsigned = errors.New("plugin package is not signed")
	// ErrBadSignature is returned when the signature does not verify against
	// any trusted key (the package was tampered with or signed by an unknown
	// key).
	ErrBadSignature = errors.New("plugin package signature does not match any trusted key")
)

// Package is a loaded plugin package.
type Package struct {
	Manifest     *Manifest
	ManifestJSON []byte
	Wasm         []byte
	Readme       []byte // optional
	Icon         []byte // optional
	// Assets holds the files under assets/ by their path inside that
	// directory. Each one's SHA-256 was checked against Manifest.Assets.
	Assets map[string][]byte
	// Digest is the canonical SHA-256 digest that the signature covers.
	Digest [sha256.Size]byte
	// Signed reports whether the package carries a signature file.
	Signed bool
	// SignedBy is the trusted key that verified the signature, or nil when
	// the signature was not verified (unsigned, or no trusted keys given).
	SignedBy ed25519.PublicKey
}

// Verified reports whether the package signature was verified against a
// trusted key.
func (p *Package) Verified() bool { return p.SignedBy != nil }

// PackageDigest computes the canonical digest of a plugin:
//
//	SHA-256( "wpgo-plugin-v1\n"
//	       || uint64_be(len(plugin.json)) || plugin.json
//	       || uint64_be(len(plugin.wasm)) || plugin.wasm )
//
// The length prefixes make the concatenation unambiguous. The signature file
// holds base64(Ed25519.Sign(privateKey, digest)) (standard encoding, optional
// trailing newline).
func PackageDigest(manifestJSON, wasm []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(digestDomain))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(manifestJSON)))
	h.Write(n[:])
	h.Write(manifestJSON)
	binary.BigEndian.PutUint64(n[:], uint64(len(wasm)))
	h.Write(n[:])
	h.Write(wasm)
	var d [sha256.Size]byte
	h.Sum(d[:0])
	return d
}

// Sign returns the contents of the signature file for the given plugin.json
// and plugin.wasm bytes.
func Sign(priv ed25519.PrivateKey, manifestJSON, wasm []byte) []byte {
	d := PackageDigest(manifestJSON, wasm)
	sig := ed25519.Sign(priv, d[:])
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// Verify checks signature file contents against the trusted keys and returns
// the key that verified it.
func Verify(signature, manifestJSON, wasm []byte, trustedKeys []ed25519.PublicKey) (ed25519.PublicKey, error) {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: malformed signature file", ErrBadSignature)
	}
	d := PackageDigest(manifestJSON, wasm)
	for _, k := range trustedKeys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, d[:], sig) {
			return k, nil
		}
	}
	return nil, ErrBadSignature
}

// GenerateKey creates a new Ed25519 signing key pair.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodeKey encodes a public or private key as base64 text (the format used by
// key files). Parsers ignore blank lines and lines starting with '#', so key
// files may carry comments.
func EncodeKey(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

// ParsePublicKey decodes a base64 Ed25519 public key.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(keyText(text))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key: want base64 of 32 bytes")
	}
	return ed25519.PublicKey(b), nil
}

// ParsePrivateKey decodes a base64 Ed25519 private key (64 bytes, as produced
// by EncodeKey).
func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(keyText(text))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key: want base64 of 64 bytes")
	}
	return ed25519.PrivateKey(b), nil
}

// keyText strips comment and blank lines from a key file.
func keyText(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			b.WriteString(line)
		}
	}
	return b.String()
}

// LoadPackage reads and validates a plugin package (zip archive).
//
// When a signature file is present and trustedKeys is non-empty it must verify
// against one of them, otherwise ErrBadSignature is returned. When
// requireSignature is true the package must be signed by a trusted key.
func LoadPackage(r io.ReaderAt, size int64, trustedKeys []ed25519.PublicKey, requireSignature bool) (*Package, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("plugin package is not a valid zip archive: %w", err)
	}
	files := map[string][]byte{}
	assets := map[string][]byte{}
	var assetTotal int64
	for _, f := range zr.File {
		name := f.Name
		if f.FileInfo().IsDir() {
			continue
		}
		if rel, ok := strings.CutPrefix(name, AssetsDir+"/"); ok {
			if err := CheckAssetPath(rel); err != nil {
				return nil, fmt.Errorf("plugin package asset %q: %w", name, err)
			}
			if _, dup := assets[rel]; dup {
				return nil, fmt.Errorf("plugin package contains %q twice", name)
			}
			if f.UncompressedSize64 > MaxAssetFile {
				return nil, fmt.Errorf("%s is %d bytes, limit is %d", name, f.UncompressedSize64, MaxAssetFile)
			}
			data, err := readZipFile(f, MaxAssetFile)
			if err != nil {
				return nil, err
			}
			if assetTotal += int64(len(data)); assetTotal > MaxAssetTotal {
				return nil, fmt.Errorf("plugin package assets exceed %d bytes in total", MaxAssetTotal)
			}
			if len(assets) >= MaxAssetFiles {
				return nil, fmt.Errorf("plugin package has more than %d assets", MaxAssetFiles)
			}
			assets[rel] = data
			continue
		}
		limit, known := fileLimits[name]
		if !known {
			return nil, fmt.Errorf("plugin package contains unexpected file %q (allowed: %s, %s/…)", name, allowedFiles(), AssetsDir)
		}
		if _, dup := files[name]; dup {
			return nil, fmt.Errorf("plugin package contains %q twice", name)
		}
		if f.UncompressedSize64 > uint64(limit) {
			return nil, fmt.Errorf("%s is %d bytes, limit is %d", name, f.UncompressedSize64, limit)
		}
		data, err := readZipFile(f, limit)
		if err != nil {
			return nil, err
		}
		files[name] = data
	}

	pkg := &Package{
		ManifestJSON: files[ManifestFile],
		Wasm:         files[WasmFile],
		Readme:       files[ReadmeFile],
		Icon:         files[IconFile],
	}
	if pkg.ManifestJSON == nil {
		return nil, errors.New("plugin package has no plugin.json")
	}
	if len(pkg.Wasm) == 0 {
		return nil, errors.New("plugin package has no plugin.wasm")
	}
	if !bytes.HasPrefix(pkg.Wasm, []byte("\x00asm")) {
		return nil, errors.New("plugin.wasm is not a WebAssembly binary")
	}
	pkg.Digest = PackageDigest(pkg.ManifestJSON, pkg.Wasm)

	sig, signed := files[SignatureFile]
	pkg.Signed = signed
	switch {
	case signed && len(trustedKeys) > 0:
		key, err := Verify(sig, pkg.ManifestJSON, pkg.Wasm, trustedKeys)
		if err != nil {
			return nil, err
		}
		pkg.SignedBy = key
	case requireSignature && !signed:
		return nil, ErrUnsigned
	case requireSignature:
		return nil, fmt.Errorf("%w: no trusted keys configured", ErrBadSignature)
	}

	m, err := ParseManifest(pkg.ManifestJSON)
	if err != nil {
		return nil, err
	}
	pkg.Manifest = m
	// Assets are covered by the signature through their digests in plugin.json.
	for rel, data := range assets {
		want, listed := m.Assets[rel]
		if !listed {
			return nil, fmt.Errorf("plugin package asset %q is not listed in plugin.json \"assets\"", rel)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != want {
			return nil, fmt.Errorf("plugin package asset %q does not match its digest in plugin.json", rel)
		}
	}
	for rel := range m.Assets {
		if _, ok := assets[rel]; !ok {
			return nil, fmt.Errorf("plugin.json lists asset %q, which is missing from the package", rel)
		}
	}
	pkg.Assets = assets
	return pkg, nil
}

// LoadPackageFile is LoadPackage for a file on disk.
func LoadPackageFile(path string, trustedKeys []ed25519.PublicKey, requireSignature bool) (*Package, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return LoadPackage(f, st.Size(), trustedKeys, requireSignature)
}

// WritePackage writes a plugin package zip to w. extras may hold readme.md,
// icon.svg and files under assets/ (keys like "assets/style.css"). When the
// package has assets, their digests are written into the manifest's "assets"
// map, so the signature (made over the final plugin.json) covers them. When
// priv is non-nil the package is signed.
func WritePackage(w io.Writer, manifestJSON, wasm []byte, extras map[string][]byte, priv ed25519.PrivateKey) error {
	m, err := ParseManifest(manifestJSON)
	if err != nil {
		return err
	}
	files := map[string][]byte{ManifestFile: manifestJSON, WasmFile: wasm}
	digests := map[string]string{}
	var assetTotal int
	for name, data := range extras {
		if rel, ok := strings.CutPrefix(name, AssetsDir+"/"); ok {
			if err := CheckAssetPath(rel); err != nil {
				return fmt.Errorf("asset %q: %w", name, err)
			}
			if len(data) > MaxAssetFile {
				return fmt.Errorf("asset %q is %d bytes, limit is %d", name, len(data), MaxAssetFile)
			}
			if assetTotal += len(data); assetTotal > MaxAssetTotal {
				return fmt.Errorf("assets exceed %d bytes in total", MaxAssetTotal)
			}
			digests[rel] = fmt.Sprintf("%x", sha256.Sum256(data))
			files[name] = data
			continue
		}
		if name != ReadmeFile && name != IconFile {
			return fmt.Errorf("unsupported extra file %q (allowed: %s, %s, %s/…)", name, ReadmeFile, IconFile, AssetsDir)
		}
		files[name] = data
	}
	if len(digests) > 0 || len(m.Assets) > 0 {
		if len(digests) > MaxAssetFiles {
			return fmt.Errorf("more than %d assets", MaxAssetFiles)
		}
		m.Assets = digests
		if len(digests) == 0 {
			m.Assets = nil
		}
		manifestJSON, err = json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		manifestJSON = append(manifestJSON, '\n')
		files[ManifestFile] = manifestJSON
	}
	if priv != nil {
		files[SignatureFile] = Sign(priv, manifestJSON, wasm)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	zw := zip.NewWriter(w)
	for _, name := range names {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			return err
		}
		if _, err := fw.Write(files[name]); err != nil {
			return err
		}
	}
	return zw.Close()
}

func readZipFile(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds the %d byte limit", f.Name, limit)
	}
	return data, nil
}

func allowedFiles() string {
	names := make([]string, 0, len(fileLimits))
	for n := range fileLimits {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
