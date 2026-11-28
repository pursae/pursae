package server

import (
	"archive/zip"
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
)

// Secrets sealed with the site secret. Several built-in plugins keep credentials encrypted in the
// database under a key derived from the server secret (PURSAE_SECRET, else the auth_secret option):
// the Pursae SMTP passwords and API tokens, the Pursae CDN credentials, the Multi Language
// machine-translation key, the Pursae Security TOTP secrets and the credentials sealed with
// sealSecret (captcha keys, Pixel API tokens, payment gateway keys, Forum Google secret, license key). A restore keeps this site's own
// auth_secret (bmgrKeepSettings), so on another site (or a reinstalled one) those values would no
// longer decrypt: e-mail stops, the CDN loses its credentials and every user with an authenticator
// app is locked out. When the archive carries its site's key (made with "Include the site's
// encryption key", off by default) the restore re-encrypts them from that key to this site's;
// otherwise they stay unreadable here, read as empty, and the restore reports how many to re-enter.
//
// bmgrSealKinds must follow the derivations of msKey (mailsmtp.go), gocdnKey (gocdn.go),
// pllMTCipher (golylang_mt_key.go), asKey (adminsec_totp.go) and sealSecret (secret_seal.go); TestBmgrResealMatchesPlugins
// checks that it does.
var bmgrSealKinds = []struct{ Prefix, Purpose string }{
	{msSealPrefix, "wpgo-mail-smtp|"},
	{gocdnSealPrefix, "wpgo-gocdn|"},
	{pllSealPrefix, "wpgo-golylang-mt|"},
	{asSealPrefix, "adminsecgo-seal|"},
	{secretSealPrefix, secretSealPurpose},
}

// bmgrSealedRe finds sealed values in a JSON line: the longest prefix wins (pllenc: before enc:).
var bmgrSealedRe = regexp.MustCompile(`(?:pllenc|enc|asg|sec):v1:[A-Za-z0-9+/]+`)

func bmgrSealAEAD(purpose, secret string) cipher.AEAD {
	k := sha256.Sum256([]byte(purpose + secret))
	block, _ := aes.NewCipher(k[:])
	gcm, _ := cipher.NewGCM(block)
	return gcm
}

// bmgrResealer re-encrypts sealed values from the archive's secret to this site's, and counts the
// ones that open under neither: an archive made without the site's encryption key (the default)
// restored on another server, or sealed under a PURSAE_SECRET the archive does not carry. Those are
// left as they are; the plugins read them as empty ("enter it again"), and the restore says so.
type bmgrResealer struct {
	from, to   string // from is "" when the archive has no key
	n          int    // values re-encrypted
	unreadable int    // values that open under neither secret
}

func bmgrOpenSealed(purpose, secret string, data []byte) ([]byte, bool) {
	if secret == "" {
		return nil, false
	}
	gcm := bmgrSealAEAD(purpose, secret)
	if len(data) < gcm.NonceSize() {
		return nil, false
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	return plain, err == nil
}

// line re-encrypts every sealed value of one archive row; values that do not open under the
// archive's secret are left as they are.
func (r *bmgrResealer) line(s string) string {
	if r == nil || !strings.Contains(s, ":v1:") {
		return s
	}
	return bmgrSealedRe.ReplaceAllStringFunc(s, func(v string) string {
		readable := false
		for _, k := range bmgrSealKinds {
			raw, ok := strings.CutPrefix(v, k.Prefix)
			if !ok {
				continue
			}
			data, err := base64.RawStdEncoding.DecodeString(raw)
			if err != nil {
				continue
			}
			if r.from != r.to {
				if plain, ok := bmgrOpenSealed(k.Purpose, r.from, data); ok {
					to := bmgrSealAEAD(k.Purpose, r.to)
					nonce := make([]byte, to.NonceSize())
					_, _ = rand.Read(nonce)
					r.n++
					return k.Prefix + base64.RawStdEncoding.EncodeToString(to.Seal(nonce, nonce, plain, nil))
				}
			}
			if _, ok := bmgrOpenSealed(k.Purpose, r.to, data); ok {
				readable = true // sealed for this site already (its own backup)
			}
		}
		if !readable {
			r.unreadable++
		}
		return v
	})
}

// bmgrArchiveSecret is the auth_secret option stored in an archive's settings ("" when it has none).
func bmgrArchiveSecret(f *zip.File) string {
	if f == nil {
		return ""
	}
	rc, err := f.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"auth_secret"`) {
			continue
		}
		var row struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if json.Unmarshal(line, &row) != nil || row.Key != "auth_secret" {
			continue
		}
		var v string
		if json.Unmarshal(row.Value, &v) == nil {
			return v
		}
	}
	return ""
}

// siteSecret is the secret this site seals with (PURSAE_SECRET, else the auth_secret option).
func (s *Server) siteSecret() string {
	if s.cfg.Secret != "" {
		return s.cfg.Secret
	}
	return s.settings.String("auth_secret")
}
