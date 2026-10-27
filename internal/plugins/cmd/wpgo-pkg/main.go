// Command wpgo-pkg creates signing keys and builds, signs and verifies wpgo
// plugin packages. It is used by tools/build-plugins.sh.
//
//	wpgo-pkg keygen -out PREFIX [-comment TEXT]
//	    writes PREFIX.pub and PREFIX.key (mode 0600); existing files are kept
//	wpgo-pkg pack -dir PLUGIN_DIR -wasm plugin.wasm -key PREFIX.key -out pkg.zip
//	    packs PLUGIN_DIR/plugin.json (+ readme.md, icon.svg and assets/** if present);
//	    the assets' digests are written into the packed plugin.json
//	wpgo-pkg verify -pub PREFIX.pub pkg.zip
//	    verifies the signature and prints the manifest summary
package main

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"

	"wpgo/internal/plugins"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "pack":
		err = pack(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wpgo-pkg:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: wpgo-pkg keygen|pack|verify [flags]  (see go doc wpgo/internal/plugins/cmd/wpgo-pkg)")
	os.Exit(2)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "output path prefix (PREFIX.pub, PREFIX.key)")
	comment := fs.String("comment", "", "comment line written at the top of both files")
	fs.Parse(args)
	if *out == "" {
		return errors.New("keygen: -out is required")
	}
	pubPath, keyPath := *out+".pub", *out+".key"
	if _, err := os.Stat(keyPath); err == nil {
		fmt.Printf("keeping existing key %s\n", keyPath)
		return nil
	}
	pub, priv, err := plugins.GenerateKey()
	if err != nil {
		return err
	}
	header := ""
	if *comment != "" {
		for _, line := range strings.Split(*comment, "\n") {
			header += "# " + line + "\n"
		}
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, []byte(header+plugins.EncodeKey(priv)+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, []byte(header+plugins.EncodeKey(pub)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s\n", pubPath, keyPath)
	return nil
}

func pack(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ExitOnError)
	dir := fs.String("dir", "", "plugin source directory containing plugin.json")
	wasmPath := fs.String("wasm", "", "compiled plugin.wasm")
	keyPath := fs.String("key", "", "private key file (omit to build an unsigned package)")
	out := fs.String("out", "", "output zip")
	fs.Parse(args)
	if *dir == "" || *wasmPath == "" || *out == "" {
		return errors.New("pack: -dir, -wasm and -out are required")
	}
	manifest, err := os.ReadFile(filepath.Join(*dir, plugins.ManifestFile))
	if err != nil {
		return err
	}
	wasm, err := os.ReadFile(*wasmPath)
	if err != nil {
		return err
	}
	extras := map[string][]byte{}
	for _, name := range []string{plugins.ReadmeFile, plugins.IconFile} {
		if data, err := os.ReadFile(filepath.Join(*dir, name)); err == nil {
			extras[name] = data
		}
	}
	assetsDir := filepath.Join(*dir, plugins.AssetsDir)
	if st, err := os.Stat(assetsDir); err == nil && st.IsDir() {
		err := filepath.WalkDir(assetsDir, func(p string, d iofs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(assetsDir, p)
			rel = filepath.ToSlash(rel)
			if err := plugins.CheckAssetPath(rel); err != nil {
				return fmt.Errorf("asset %s: %w", rel, err)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			extras[plugins.AssetsDir+"/"+rel] = data
			return nil
		})
		if err != nil {
			return err
		}
	}
	var priv ed25519.PrivateKey
	if *keyPath != "" {
		text, err := os.ReadFile(*keyPath)
		if err != nil {
			return err
		}
		if priv, err = plugins.ParsePrivateKey(string(text)); err != nil {
			return fmt.Errorf("%s: %w", *keyPath, err)
		}
	}
	var buf bytes.Buffer
	if err := plugins.WritePackage(&buf, manifest, wasm, extras, priv); err != nil {
		return err
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		return err
	}
	assets := 0
	for name := range extras {
		if strings.HasPrefix(name, plugins.AssetsDir+"/") {
			assets++
		}
	}
	fmt.Printf("wrote %s (%d bytes, signed=%v, assets=%d)\n", *out, buf.Len(), priv != nil, assets)
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	pubPath := fs.String("pub", "", "trusted public key file")
	fs.Parse(args)
	if *pubPath == "" || fs.NArg() != 1 {
		return errors.New("verify: usage: verify -pub KEY.pub PACKAGE.zip")
	}
	text, err := os.ReadFile(*pubPath)
	if err != nil {
		return err
	}
	pub, err := plugins.ParsePublicKey(string(text))
	if err != nil {
		return err
	}
	pkg, err := plugins.LoadPackageFile(fs.Arg(0), []ed25519.PublicKey{pub}, true)
	if err != nil {
		return err
	}
	m := pkg.Manifest
	fmt.Printf("%s %s (%s): signature OK, digest %x\n  permissions: %v\n  hooks: %v\n  shortcodes: %v\n  assets: %d\n",
		m.Slug, m.Version, m.Name, pkg.Digest, m.Permissions, m.Hooks, m.Shortcodes, len(pkg.Assets))
	return nil
}
