package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/config"
	"wpgo/internal/mail"
	"wpgo/internal/plugins"
)

// pluginMaxPackage bounds a plugin package, from an upload as well as from the Manager.
const pluginMaxPackage = hubMaxPackage

// splitPermissions returns the granted permissions the new manifest still declares (keep) and the
// declared ones that were never granted (missing).
func splitPermissions(granted, declared []plugins.Permission) (keep, missing []plugins.Permission) {
	keep, missing = []plugins.Permission{}, []plugins.Permission{}
	had := map[plugins.Permission]bool{}
	for _, g := range granted {
		had[g] = true
	}
	for _, want := range declared {
		if had[want] {
			keep = append(keep, want)
		} else {
			missing = append(missing, want)
		}
	}
	return keep, missing
}

// recordPluginDisabled is the runtime's OnDisable: the plugin becomes inactive, the reason is kept in
// the database (it would otherwise be lost with the process) and the administrator is told once.
func (s *Server) recordPluginDisabled(ctx context.Context, slug, reason string) {
	tag, err := s.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'inactive', disabled_reason = $2, disabled_at = now() WHERE slug = $1 AND status = 'active'`, slug, reason)
	if err != nil || tag.RowsAffected() == 0 {
		return
	}
	to := strings.TrimSpace(s.settings.String("admin_email"))
	if to == "" {
		return
	}
	site := s.settings.String("blogname")
	s.sendMail(ctx, mail.Message{To: []string{to}, Subject: "[" + site + "] The plugin " + slug + " was deactivated",
		Body: "Howdy,\n\nThe plugin \"" + slug + "\" kept failing and was deactivated automatically so the site keeps working:\n\n" + reason +
			"\n\nYou can review it under Plugins in your admin area and activate it again once the problem is fixed.\n\nRegards,\nAll at " + site + "\n"})
}

// missingPluginDependencies lists the plugins slug's manifest requires (requires_plugins) that are not
// active. Built-in plugins count by their Plugins screen slug.
func (s *Server) missingPluginDependencies(ctx context.Context, slug string) []string {
	pkg, err := plugins.LoadPackageFile(pluginPackagePath(s.plugins.dir, slug), s.plugins.trusted(), !s.allowUnsignedPlugins())
	if err != nil || len(pkg.Manifest.RequiresPlugins) == 0 {
		return nil
	}
	active := map[string]bool{}
	for _, r := range s.pluginInventory(ctx) {
		if r.Active {
			active[r.Slug], active[pluginURLSlug(r.Slug)] = true, true
		}
	}
	var out []string
	for _, r := range pkg.Manifest.RequiresPlugins {
		if !active[r] {
			out = append(out, r)
		}
	}
	return out
}

func pluginPackagePath(dir, slug string) string {
	return dir + string(os.PathSeparator) + slug + ".zip"
}

// trustedKey is one line of the plugin_trusted_keys option: a base64 Ed25519 public key and a comment.
type trustedKey struct {
	Key, Comment, Fingerprint string
	pub                       ed25519.PublicKey
}

func keyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:32]
}

// parseTrustedKeys reads the option (one "key comment" per line; commas also separate keys, as
// PURSAE_PLUGIN_KEYS does). Invalid entries are skipped.
func parseTrustedKeys(text string) []trustedKey {
	var out []trustedKey
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == ',' }) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		pub, err := plugins.ParsePublicKey(f[0])
		if err != nil {
			continue
		}
		out = append(out, trustedKey{Key: plugins.EncodeKey(pub), Comment: strings.Join(f[1:], " "), Fingerprint: keyFingerprint(pub), pub: pub})
	}
	return out
}

func formatTrustedKeys(keys []trustedKey) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k.Key)
		if k.Comment != "" {
			b.WriteString(" " + k.Comment)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// applySiteKeys makes the keys from PURSAE_PLUGIN_KEYS (and the dev key) plus the option the trusted
// set at once, without a restart.
func (s *Server) applySiteKeys() {
	ps := s.plugins
	ps.keyMu.Lock()
	defer ps.keyMu.Unlock()
	ps.siteKeys = append([]ed25519.PublicKey{}, ps.envKeys...)
	if pluginKeysLocked() {
		return // the operator owns the key list: keys an administrator added are not trusted
	}
	for _, k := range parseTrustedKeys(s.settings.String("plugin_trusted_keys")) {
		ps.siteKeys = append(ps.siteKeys, k.pub)
	}
}

// pluginKeysLocked reports the operator lock on the trusted-key list (a hosting panel sets it):
// PURSAE_PLUGIN_KEYS_LOCKED=1|true|locked, or one of those words as an entry of
// PURSAE_PLUGIN_KEYS. Locked, administrators cannot add or remove keys and keys stored in the
// plugin_trusted_keys option are ignored, so a site admin cannot widen which publishers' code
// runs on the host. Off by default: self-hosted admins manage their own keys.
func pluginKeysLocked() bool {
	lockWord := func(v string) bool {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "locked":
			return true
		}
		return false
	}
	if lockWord(config.Env("PLUGIN_KEYS_LOCKED")) {
		return true
	}
	for _, f := range strings.FieldsFunc(config.Env("PLUGIN_KEYS"), func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if lockWord(f) {
			return true
		}
	}
	return false
}

var errPluginKeysLocked = errors.New("The trusted keys are managed by the server operator (PURSAE_PLUGIN_KEYS is locked) and cannot be changed here.")

type pluginKeysData struct {
	Locked    bool
	Keys      []trustedKey
	FixedKeys int // keys from the environment: shown, not editable
	Nonce     string
}

// pluginKeys is Plugins > Trusted Keys: the Ed25519 keys whose signed packages may be installed.
func (s *Server) pluginKeys(c *gin.Context) {
	if !currentUser(c).Can("install_plugins", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to install plugins on this site.")
		return
	}
	var notices []notice
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "plugin-keys") {
			s.denyNonce(c)
			return
		}
		msg, err := s.changeTrustedKeys(c, c.PostForm("action"), c.PostForm("key"), c.PostForm("comment"), c.PostForm("fingerprint"))
		if err != nil {
			c.Redirect(http.StatusFound, "/wp-admin/plugin-keys?error="+url.QueryEscape(err.Error()))
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugin-keys?done="+url.QueryEscape(msg))
		return
	}
	if v := c.Query("done"); v != "" {
		notices = append(notices, notice{Type: "success", Message: template.HTML(template.HTMLEscapeString(v)), Dismissible: true})
	}
	if v := c.Query("error"); v != "" {
		notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(v))})
	}
	d := pluginKeysData{Locked: pluginKeysLocked(), Keys: parseTrustedKeys(s.settings.String("plugin_trusted_keys")), FixedKeys: len(s.plugins.envKeys), Nonce: s.nonce(c, "plugin-keys")}
	s.adminPage(c, "plugin-keys", "Trusted Keys", "plugins-php", "admin/plugin-keys", d, notices...)
}

// changeTrustedKeys adds or removes one key of the option and applies the result live.
func (s *Server) changeTrustedKeys(ctx context.Context, action, key, comment, fingerprint string) (string, error) {
	if pluginKeysLocked() {
		return "", errPluginKeysLocked
	}
	keys := parseTrustedKeys(s.settings.String("plugin_trusted_keys"))
	var msg string
	switch action {
	case "add":
		pub, err := plugins.ParsePublicKey(strings.TrimSpace(key))
		if err != nil {
			return "", errors.New("That is not a valid Ed25519 public key (base64 of 32 bytes).")
		}
		fp := keyFingerprint(pub)
		for _, k := range keys {
			if k.Fingerprint == fp {
				return "", errors.New("That key is already trusted.")
			}
		}
		comment = strings.Join(strings.Fields(comment), " ")
		if len(comment) > 100 {
			comment = comment[:100]
		}
		keys = append(keys, trustedKey{Key: plugins.EncodeKey(pub), Comment: comment, Fingerprint: fp, pub: pub})
		msg = "Key " + fp + " is now trusted."
	case "remove":
		kept := keys[:0:0]
		for _, k := range keys {
			if k.Fingerprint != fingerprint {
				kept = append(kept, k)
			}
		}
		if len(kept) == len(keys) {
			return "", errors.New("That key is not in the list.")
		}
		keys = kept
		msg = "The key was removed. Plugins signed with it that are already installed keep running; new packages signed with it are refused."
	default:
		return "", errors.New("Unknown action.")
	}
	if err := s.settings.Set(ctx, map[string]any{"plugin_trusted_keys": formatTrustedKeys(keys)}); err != nil {
		return "", fmt.Errorf("The keys could not be saved: %v", err)
	}
	s.applySiteKeys()
	return msg, nil
}
