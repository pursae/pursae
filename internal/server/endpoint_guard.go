package server

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"wpgo/internal/config"
)

// Endpoints an administrator sets and the server then connects to (remote backup storage, Pursae
// CDN object storage, the front proxy purge URL) are checked twice: when saved (IP literals and
// localhost names) and on every connection, after DNS resolution, so a host name that resolves
// (or later re-resolves) to an inside address is refused too. Loopback, RFC1918, unique-local and
// carrier-grade NAT addresses need an environment switch (PURSAE_ALLOW_PRIVATE_ENDPOINTS=1, or
// PURSAE_BACKUP_S3_ALLOW_PRIVATE=1 for the backup storage only): an administrator account alone
// cannot point the server at internal services. Link-local, cloud metadata and reserved addresses
// are refused always.

// privateEndpointsEnv is the switch (without the PURSAE_ prefix) that lets admin-set endpoints
// reach the private network.
const privateEndpointsEnv = "ALLOW_PRIVATE_ENDPOINTS"

// privateEndpointsAllowed reports PURSAE_ALLOW_PRIVATE_ENDPOINTS.
func privateEndpointsAllowed() bool { return envTrue(config.Env(privateEndpointsEnv)) }

// endpointMetadata are cloud metadata services outside the link-local range.
// 169.254.169.254 and the other link-local metadata addresses are refused by
// IsLinkLocalUnicast before this list is consulted.
var endpointMetadata = []net.IP{
	net.ParseIP("100.100.100.200"), // Alibaba Cloud
	net.ParseIP("fd00:ec2::254"),   // AWS metadata, IPv6
	net.ParseIP("168.63.129.16"),   // Azure WireServer (not link-local, not an RFC1918 address)
}

// endpointAddrOK decides whether an admin-set endpoint may be reached at ip. env names the switch
// shown in the refusal. NAT64 (64:ff9b::/96) and 6to4 (2002::/16) are judged as the IPv4 address
// they embed, so a mapped metadata or loopback address cannot skip the checks below. With
// allowPrivate, only loopback, RFC1918 / unique-local and carrier-grade NAT are added; reserved,
// documentation and metadata addresses stay refused.
func endpointAddrOK(ip net.IP, allowPrivate bool, env string) error {
	if ip == nil {
		return errors.New("the endpoint points at a link-local or reserved address")
	}
	if inner := bmgrEmbeddedIPv4(ip); inner != nil {
		return endpointAddrOK(inner, allowPrivate, env)
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return errors.New("the endpoint points at a link-local or reserved address")
	}
	for _, m := range endpointMetadata {
		if m.Equal(ip) {
			return errors.New("the endpoint points at a cloud metadata service")
		}
	}
	if publicIP(ip) {
		return nil
	}
	if allowPrivate && (ip.IsLoopback() || ip.IsPrivate() || bmgrCGNAT(ip)) {
		return nil
	}
	if !allowPrivate {
		return errors.New("the endpoint points at this server or the private network (set " + config.EnvName(env) + "=1 for a server on your own network)")
	}
	return errors.New("the endpoint points at a link-local or reserved address")
}

// endpointHostOK is the save-time check of an endpoint URL's host: an IP literal must pass
// endpointAddrOK, and localhost names need the switch. Other names are checked when dialled.
func endpointHostOK(u *url.URL, allowPrivate bool, env string) error {
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil {
		return endpointAddrOK(ip, allowPrivate, env)
	}
	if !allowPrivate && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return errors.New("the endpoint points at this server or the private network (set " + config.EnvName(env) + "=1 for a server on your own network)")
	}
	return nil
}

// endpointDialControl is a net.Dialer Control applying endpointAddrOK to the resolved address of
// every connection; allow is read at dial time (nil means never).
func endpointDialControl(allow func() bool, env string) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		return endpointAddrOK(net.ParseIP(host), allow != nil && allow(), env)
	}
}

// endpointClient is the HTTP client for an admin-set endpoint: every connection (redirects
// included) passes endpointDialControl, and no proxy hides the real destination.
func endpointClient(timeout time.Duration, allow func() bool, env string) *http.Client {
	d := &net.Dialer{Timeout: 30 * time.Second, Control: endpointDialControl(allow, env)}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
				return errors.New("too many redirects or an unsupported redirect scheme")
			}
			return nil
		},
	}
}
