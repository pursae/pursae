// Package pkgtool implements the plugin packaging commands shared by cmd/wpgo-pkg and
// `wpgo plugin`: keygen, pack, verify, new (scaffold) and build (compile + pack).
package pkgtool

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

// Usage lists the commands.
const Usage = "keygen|pack|verify|new|build"

// Run executes one command (args[0]) with its flags.
func Run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: " + Usage)
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:])
	case "pack":
		return pack(args[1:])
	case "verify":
		return verify(args[1:])
	case "new":
		return scaffold(args[1:])
	case "build":
		return build(args[1:])
	}
	return fmt.Errorf("unknown command %q (want %s)", args[0], Usage)
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
		if _, err := os.Stat(pubPath); os.IsNotExist(err) {
			text, err := os.ReadFile(keyPath)
			if err != nil {
				return err
			}
			priv, err := plugins.ParsePrivateKey(string(text))
			if err != nil {
				return fmt.Errorf("keygen: %s: %w", keyPath, err)
			}
			f, err := os.OpenFile(pubPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			_, err = f.WriteString(plugins.EncodeKey(priv.Public().(ed25519.PublicKey)) + "\n")
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		fmt.Printf("keeping existing key %s\n", keyPath)
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(pubPath); err == nil {
		return fmt.Errorf("keygen: %s already exists", pubPath)
	} else if !os.IsNotExist(err) {
		return err
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
	if err := writeNewKeyFile(keyPath, header+plugins.EncodeKey(priv)+"\n", 0o600); err != nil {
		return err
	}
	if err := writeNewKeyFile(pubPath, header+plugins.EncodeKey(pub)+"\n", 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s\n", pubPath, keyPath)
	return nil
}

func writeNewKeyFile(path, content string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
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
		path := filepath.Join(*dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
					continue
				}
			}
			return fmt.Errorf("%s: %w", path, err)
		}
		extras[name] = data
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
