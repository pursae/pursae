package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"wpgo/internal/plugins"
)

// Root of trust for what Pursae Manager signs.
//
// The Manager lists its signing keys in /api/v1/index ("keys"). A key that arrives in the same
// answer as the release it verifies proves nothing: whoever controls that answer (a compromised
// Manager, its domain, a proxy in front of an http Manager) could list a key of their own and sign
// a binary with it. So the binary carries root public keys of its own, and a Manager key verifies a
// core release only when a root key endorsed it:
//
//	{"id": "hub-2026", "public": "<base64 Ed25519>", "endorsement": "<base64 Ed25519 signature>"}
//
// where the endorsement is the root key's signature over hubKeyEndorsement(id, public):
//
//	"pursae-hub-key-v1\n" + id + "\n" + base64(32-byte public key) (no trailing newline)
//
// The root keys are rootKeysBuiltin (the Pursae release root, pinned in every build) plus whatever
// the build adds with
//
//	go build -ldflags "-X wpgo/internal/server.rootKeysLinked=<base64>[,<base64>…]"
//
// (the Docker image: --build-arg ROOT_KEYS=…). Linked keys only ADD roots, which is how a release
// root is rotated: builds carry the old and the new root until every Manager key is endorsed by the
// new one, then the old root leaves rootKeysBuiltin.
//
// A production binary cannot relax this. Only a development build (-tags devhub, see
// hub_trust_devhub.go) reads PURSAE_TRUST_HUB_KEYS=1, which trusts every key the Manager lists,
// endorsed or not, so a site can update from a local Pursae Manager whose throwaway key no root
// endorsed (the development compose file builds with devhub and sets it).
//
// Plugin packages: only endorsed Manager keys are added to the trusted plugin keys (every listed
// key with the devhub opt-in, or in a build without any root key), and only with
// wpgo_hub_trust_keys on.

// rootKeysBuiltin are the release root keys compiled into every build: the Pursae release root key
// (its private half never leaves the release machine).
var rootKeysBuiltin = []string{
	"LxoPRqTiJfpXB0fc+peWabyDCWO/NzATjpk7pSe1IdY=",
}

// rootKeysLinked is set at link time (-ldflags -X), comma-separated base64 keys.
var rootKeysLinked = ""

// hubKeyEndorsementPrefix is the domain separator of an endorsement signature.
const hubKeyEndorsementPrefix = "pursae-hub-key-v1\n"

// hubKeyEndorsement is the message a root key signs to endorse a Manager key.
func hubKeyEndorsement(id string, pub ed25519.PublicKey) []byte {
	return []byte(hubKeyEndorsementPrefix + id + "\n" + base64.StdEncoding.EncodeToString(pub))
}

// rootKeys are the pinned release root keys of this build.
func rootKeys() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, k := range append(append([]string{}, rootKeysBuiltin...), strings.Split(rootKeysLinked, ",")...) {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if pub, err := plugins.ParsePublicKey(k); err == nil {
			out = append(out, pub)
		}
	}
	return out
}

// trustHubKeysForce is a test seam for the devhub opt-in.
var trustHubKeysForce bool

// trustHubKeysEnv is the development opt-in to the keys the Manager lists: PURSAE_TRUST_HUB_KEYS=1 in
// a devhub build (hub_trust_devhub.go). Always false in a production build.
func trustHubKeysEnv() bool { return trustHubKeysForce || devTrustHubKeys() }

// endorsed reports whether a root key endorsed k.
func (k hubKey) endorsed(roots []ed25519.PublicKey) bool {
	pub, err := plugins.ParsePublicKey(k.Public)
	if err != nil {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k.Endorsement))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	msg := hubKeyEndorsement(k.ID, pub)
	for _, r := range roots {
		if ed25519.Verify(r, msg, sig) {
			return true
		}
	}
	return false
}

// errCoreNoRoot is why a build without a root key does not update itself.
var errCoreNoRoot = errors.New("this build has no pinned Pursae release key, so a release announced by Pursae Manager cannot be verified; replace the executable (or pull the new container image) yourself, or use a development build (-tags devhub) with PURSAE_TRUST_HUB_KEYS=1")

// coreTrustCheck reports why this build cannot verify core releases, if it cannot.
func coreTrustCheck() error {
	if len(rootKeys()) == 0 && !trustHubKeysEnv() {
		return errCoreNoRoot
	}
	return nil
}

// coreReleaseKeys are the Manager keys that may verify a core release: every listed one with the
// devhub PURSAE_TRUST_HUB_KEYS opt-in, the endorsed ones when a root key is pinned, otherwise none.
func coreReleaseKeys(keys []hubKey) ([]hubKey, error) {
	if trustHubKeysEnv() {
		return keys, nil
	}
	if roots := rootKeys(); len(roots) > 0 {
		var out []hubKey
		for _, k := range keys {
			if k.endorsed(roots) {
				out = append(out, k)
			}
		}
		if len(out) == 0 {
			return nil, errors.New("none of the signing keys Pursae Manager lists is endorsed by this build's release key")
		}
		return out, nil
	}
	return nil, errCoreNoRoot
}

// pluginHubKeys are the Manager keys added to the trusted plugin keys.
func pluginHubKeys(keys []hubKey) []ed25519.PublicKey {
	roots := rootKeys()
	check := len(roots) > 0 && !trustHubKeysEnv()
	var out []ed25519.PublicKey
	for _, k := range keys {
		if check && !k.endorsed(roots) {
			continue
		}
		if pub, err := plugins.ParsePublicKey(k.Public); err == nil {
			out = append(out, pub)
		}
	}
	return out
}

// Transport rules for Pursae Manager -------------------------------------------------------------

// errHubInsecure refuses a plain-http Manager on a public host.
var errHubInsecure = errors.New("Pursae Manager must be reached over https (http is only allowed for localhost and *.localhost / *.test development hosts); check PURSAE_DEV_HUB_URL")

// localDevHost reports whether host is a loopback or development name, where http is acceptable and
// the dial may reach loopback. Only a devhub build (a local Pursae Manager) has such hosts
// (devLocalHubs): a production build's Manager is https://hub.pursae.com, so there a redirect or a
// listed URL naming localhost, *.test or 127.0.0.1 is refused like any other private target.
func localDevHost(host string) bool {
	if !devLocalHubs() {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".test") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hubURLAllowed checks the scheme of a URL the site sends a Manager request to.
func hubURLAllowed(u *url.URL) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if localDevHost(u.Hostname()) {
			return nil
		}
	}
	return errHubInsecure
}

// hubCredentialHeaders are the headers that identify the site to the Manager.
var hubCredentialHeaders = []string{"X-Pursae-License", "X-Pursae-Site"}

// hubTransport is shared so idle connections to the Manager are reused. It has no proxy:
// ProxyFromEnvironment would dial the proxy and publicOnly would never see the real address.
var hubTransport = &http.Transport{
	Proxy:               nil,
	DialContext:         hubDialContext,
	TLSHandshakeTimeout: 10 * time.Second,
	IdleConnTimeout:     30 * time.Second,
}

// hubHTTPClient is every client that talks to Pursae Manager, including core downloads
// (core_update.go). The dial refuses private addresses except loopback for a local development
// host, and hubCheckRedirect refuses a redirect whose name resolves to one.
func hubHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: hubTransport, CheckRedirect: hubCheckRedirect}
}

// hubDialContext applies publicOnly after DNS. Loopback is allowed only when the URL host itself
// is a local development name (localhost, 127.0.0.1, *.test): a public name that resolves to
// 127.0.0.1 or 169.254.169.254 is still refused.
func hubDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	allowLoop := localDevHost(host)
	block := publicOnly(nil, "Pursae Manager address points at a private network")
	d := &net.Dialer{Timeout: 15 * time.Second, Control: func(network, address string, c syscall.RawConn) error {
		if allowLoop {
			h, _, splitErr := net.SplitHostPort(address)
			if splitErr == nil {
				if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
					return nil
				}
			}
		}
		return block(network, address, c)
	}}
	return d.DialContext(ctx, network, addr)
}

// hubResolvedPublic rejects a redirect target that is not a local development host and that
// resolves to any non-public address. The dial control is the second check, after DNS again.
func hubResolvedPublic(host string) error {
	if localDevHost(host) {
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return errors.New("Pursae Manager redirect has no address")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return errors.New("Pursae Manager redirect points at a private network")
		}
	}
	return nil
}

// hubCheckRedirect follows at most 10 redirects, never from https to http on a public host,
// never to a private address, and drops the license and site headers when a redirect leaves the
// original host (net/http only strips Authorization and Cookie itself).
func hubCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if err := hubURLAllowed(req.URL); err != nil {
		return err
	}
	if err := hubResolvedPublic(req.URL.Hostname()); err != nil {
		return err
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		for _, h := range hubCredentialHeaders {
			req.Header.Del(h)
		}
	}
	return nil
}
