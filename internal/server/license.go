package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Settings › License (options-license): the site owner enters the license key bought from Pursae;
// Pursae Manager checks it (GET /api/v1/license, pursae-manager/API.md "Licenses, channels, telemetry
// and core builds"). The key goes with every request to the Manager (hubNewRequest), which then also
// lists the pro and private packages and the beta versions and releases the license unlocks. The
// answer is cached for 12 hours (and re-checked when the key is saved, on "Check now" and by the daily
// wpgo_hub_check, license_watch.go, which warns before expiry). licensePro / licenseBeta are what
// Pro- or beta-gated site features ask; today they drive the plan badge on the License screen.

const (
	licenseOption   = "pursae_license_key"
	licenseCacheKey = "pursae_license"
	licenseTTL      = 12 * time.Hour
	licenseRetry    = 15 * time.Minute // an unreachable Manager is asked again after this
)

// licenseStatus is the Manager's answer for the site's key, plus when and for which key it was fetched.
type licenseStatus struct {
	Valid    bool     `json:"valid"`
	Plan     string   `json:"plan,omitempty"` // free | pro
	Pro      bool     `json:"pro"`
	Beta     bool     `json:"beta"`
	Expires  string   `json:"expires,omitempty"` // RFC 3339, empty for no expiry
	Domains  []string `json:"domains,omitempty"`
	Packages []string `json:"packages,omitempty"` // private listings granted, "kind/slug"
	Checked  string   `json:"checked,omitempty"`
	// Error is the Manager's code (missing, unknown, revoked, expired, domain_mismatch,
	// activation_limit) or the site's own: off (no Manager configured), unreachable.
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`

	KeyHash string    `json:"key_hash,omitempty"`
	Fetched time.Time `json:"fetched"`
}

func (l *licenseStatus) fresh(hash string) bool {
	if l == nil || l.KeyHash != hash {
		return false
	}
	ttl := licenseTTL
	if l.Error == "unreachable" {
		ttl = licenseRetry
	}
	return time.Since(l.Fetched) < ttl
}

// licenseKey is the key entered under Settings › License (stored sealed, see sealSecret).
func (s *Server) licenseKey() string {
	return strings.TrimSpace(s.revealSecret(s.settings.String(licenseOption)))
}

// licenseHash is a short, stable digest of a key (cache keys; the key itself is never a cache key).
func licenseHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// license is the license state of the site: cached for 12 hours, otherwise asked from the Manager.
func (s *Server) license(ctx context.Context) *licenseStatus {
	key := s.licenseKey()
	if key == "" {
		return &licenseStatus{Error: "missing", Message: "No license key has been entered."}
	}
	if l := s.licenseCached(ctx); l != nil {
		return l
	}
	return s.licenseFetch(ctx)
}

// licenseCached is the cached answer for the current key, without going to the network.
func (s *Server) licenseCached(ctx context.Context) *licenseStatus {
	key := s.licenseKey()
	if key == "" {
		return nil
	}
	h := licenseHash(key)
	if l := s.mgr.license.Load(); l.fresh(h) {
		return l
	}
	var l licenseStatus
	if s.hubCacheGet(ctx, licenseCacheKey, &l) && l.fresh(h) {
		s.mgr.license.Store(&l)
		return &l
	}
	return nil
}

// licenseFetch asks the Manager (GET /api/v1/license) and caches the answer.
func (s *Server) licenseFetch(ctx context.Context) *licenseStatus {
	key := s.licenseKey()
	if key == "" {
		return s.license(ctx)
	}
	l := &licenseStatus{}
	err := s.hubGet(ctx, "/api/v1/license", l)
	switch {
	case errors.Is(err, errHubOff):
		return &licenseStatus{Error: "off", Message: "Pursae Manager is switched off in this build, so the license cannot be checked."}
	case err != nil:
		l = &licenseStatus{Error: "unreachable", Message: "Pursae Manager could not be reached: " + err.Error()}
	case !l.Valid && l.Error == "":
		l.Error = "unknown"
	}
	l.KeyHash, l.Fetched = licenseHash(key), time.Now()
	s.mgr.license.Store(l)
	if l.Error != "unreachable" {
		s.hubCacheSet(ctx, licenseCacheKey, l, licenseTTL)
	}
	return l
}

// licenseForget drops the cached license answer and the Manager answers that depend on it.
func (s *Server) licenseForget(ctx context.Context) {
	s.mgr.license.Store(nil)
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM transients WHERE key = $1`, licenseCacheKey)
	s.hubDropCaches(ctx)
}

// licenseNow is the cached license state for code without a request context (never the network).
func (s *Server) licenseNow() *licenseStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.licenseCached(ctx)
}

// licensePro reports whether the site has a valid license with pro features.
func (s *Server) licensePro() bool {
	l := s.licenseNow()
	return l != nil && l.Valid && l.Pro
}

// licenseBeta reports whether the site has a valid license for the beta channel.
func (s *Server) licenseBeta() bool {
	l := s.licenseNow()
	return l != nil && l.Valid && l.Beta
}

// licenseErrorText explains a license error code on the License screen.
var licenseErrorText = map[string]string{
	"missing":          "No license key has been entered.",
	"unknown":          "This license key does not exist.",
	"revoked":          "This license key has been revoked.",
	"expired":          "This license key has expired.",
	"domain_mismatch":  "This license key is not valid for this site.",
	"activation_limit": "This license key is already active on as many sites as it allows.",
}

type licenseView struct {
	Key, Nonce string
	Status     *licenseStatus
	Message    string
	Plan       string
	Expires    string
	Checked    string
	Hub        bool
	Badge      string // Pro and/or Beta, from licensePro / licenseBeta
	SendStats  bool
}

// optionsLicense is Settings › License.
func (s *Server) optionsLicense(c *gin.Context) {
	if !currentUser(c).Can("manage_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to manage options for this site.")
		return
	}
	ctx := c.Request.Context()
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "pursae-license") {
			s.denyNonce(c)
			return
		}
		q := "checked=true"
		if c.PostForm("check") == "" { // Save Changes
			key := strings.TrimSpace(c.PostForm(licenseOption))
			if cur := s.licenseKey(); cur != "" && key == gfMask(cur) {
				key = cur // the form shows the saved key masked; sending it back keeps it
			}
			if len(key) > 200 || strings.ContainsAny(key, "\r\n\t") {
				c.Redirect(http.StatusFound, "/wp-admin/options-license?error=invalid")
				return
			}
			if err := s.settings.Set(ctx, map[string]any{licenseOption: s.sealSecret(key)}); err != nil {
				c.Redirect(http.StatusFound, "/wp-admin/options-license?error=save")
				return
			}
			q = "settings-updated=true"
		}
		if vals := c.Request.PostForm[statsOption]; c.PostForm("check") == "" && len(vals) > 0 { // hidden 0, then the checkbox's 1
			stats := "off"
			if vals[len(vals)-1] == "1" {
				stats = "on"
			}
			_ = s.settings.Set(ctx, map[string]any{statsOption: stats})
		}
		s.licenseForget(ctx)
		if s.licenseKey() != "" {
			s.licenseFetch(ctx)
		}
		c.Redirect(http.StatusFound, "/wp-admin/options-license?"+q)
		return
	}
	v := licenseView{Key: gfMask(s.licenseKey()), Nonce: s.nonce(c, "pursae-license"), Hub: s.hubBase() != ""}
	v.SendStats = s.statsEnabled()
	if v.Key != "" {
		v.Status = s.license(ctx)
		switch {
		case s.licensePro() && s.licenseBeta():
			v.Badge = "Pro + Beta"
		case s.licensePro():
			v.Badge = "Pro"
		case s.licenseBeta():
			v.Badge = "Beta"
		}
		v.Message = v.Status.Message
		if t, ok := licenseErrorText[v.Status.Error]; ok && v.Message == "" {
			v.Message = t
		}
		v.Plan = map[string]string{"pro": "Pro", "free": "Free"}[v.Status.Plan]
		if v.Plan == "" {
			v.Plan = v.Status.Plan
		}
		v.Expires = "Never"
		if v.Status.Expires != "" {
			v.Expires = v.Status.Expires
			if t, err := time.Parse(time.RFC3339, v.Status.Expires); err == nil {
				v.Expires = t.In(s.loc()).Format("January 2, 2006")
			}
		}
		if !v.Status.Fetched.IsZero() {
			v.Checked = v.Status.Fetched.In(s.loc()).Format("January 2, 2006 at 3:04 pm")
		}
	}
	var notices []notice
	switch {
	case c.Query("checked") == "true":
		notices = append(notices, notice{Type: "info", Message: "The license was checked with Pursae Manager.", Dismissible: true})
	case c.Query("error") == "invalid":
		notices = append(notices, notice{Type: "error", Message: template.HTML("That does not look like a license key."), Dismissible: true})
	case c.Query("error") != "":
		notices = append(notices, notice{Type: "error", Message: "The license key could not be saved.", Dismissible: true})
	}
	s.adminPage(c, "options-license", "License Settings", "options-license-php", "admin/options-license", v, notices...)
}
