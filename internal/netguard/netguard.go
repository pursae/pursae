// Package netguard holds the one list of address ranges that outbound connections made on behalf
// of users, plugins or administrators must not reach (the host check of wp_http_validate_url()).
// internal/server, internal/i18n and internal/mail share it so the lists cannot drift apart.
package netguard

import (
	"errors"
	"net"
	"syscall"
)

// nonPublicNets are the special-purpose ranges (RFC 6890) beyond what net.IP's predicates cover,
// plus cloud metadata services that sit outside the link-local range.
var nonPublicNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8",          // "this network"
		"100.64.0.0/10",      // carrier-grade NAT
		"192.0.0.0/24",       // IETF protocol assignments
		"192.0.2.0/24",       // TEST-NET-1
		"192.88.99.0/24",     // 6to4 relay anycast (deprecated)
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // TEST-NET-2
		"203.0.113.0/24",     // TEST-NET-3
		"240.0.0.0/4",        // reserved, includes broadcast
		"100.100.100.200/32", // Alibaba Cloud metadata
		"168.63.129.16/32",   // Azure WireServer
		"::/96",              // IPv4-compatible (deprecated), includes :: and ::1
		"::ffff:0:0:0/96",    // IPv4-translated (SIIT), e.g. ::ffff:0:7f00:1
		"64:ff9b:1::/48",     // local-use NAT64
		"100::/64",           // discard-only
		"2001::/32",          // Teredo (embeds an obfuscated IPv4 address)
		"2001:db8::/32",      // documentation
		"fec0::/10",          // site-local (deprecated)
		"fd00:ec2::254/128",  // AWS metadata, IPv6 (also unique-local)
	} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// EmbeddedIPv4 returns the IPv4 address an IPv6 address reaches: IPv4-mapped (::ffff:0:0/96),
// NAT64 (64:ff9b::/96) or 6to4 (2002::/16). Otherwise nil.
func EmbeddedIPv4(ip net.IP) net.IP {
	ip16 := ip.To16()
	if ip16 == nil || len(ip) == net.IPv4len {
		return nil
	}
	switch {
	case isZero(ip16[0:10]) && ip16[10] == 0xff && ip16[11] == 0xff: // ::ffff:0:0/96
		return net.IP(append([]byte(nil), ip16[12:16]...))
	case ip16[0] == 0x00 && ip16[1] == 0x64 && ip16[2] == 0xff && ip16[3] == 0x9b && isZero(ip16[4:12]): // 64:ff9b::/96
		return net.IP(append([]byte(nil), ip16[12:16]...))
	case ip16[0] == 0x20 && ip16[1] == 0x02: // 2002::/16
		return net.IP(append([]byte(nil), ip16[2:6]...))
	}
	return nil
}

// PublicIP reports whether ip is a public unicast address: loopback, private, link-local,
// multicast, unspecified, special-purpose and metadata addresses are refused. Embedded IPv4
// addresses are judged as the IPv4 address they reach.
func PublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if inner := EmbeddedIPv4(ip); inner != nil {
		return PublicIP(inner)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, n := range nonPublicNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// PrivateNetwork reports loopback, RFC1918 / unique-local and carrier-grade NAT addresses: the
// ones a self-hosted setup may legitimately reach once an operator opts in. Link-local, metadata
// and reserved addresses are never included.
func PrivateNetwork(ip net.IP) bool {
	if inner := EmbeddedIPv4(ip); inner != nil {
		ip = inner
	}
	if ip == nil {
		return false
	}
	for _, m := range []string{"100.100.100.200", "168.63.129.16", "fd00:ec2::254"} {
		if net.ParseIP(m).Equal(ip) {
			return false
		}
	}
	ip4 := ip.To4()
	cgnat := ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 0x40
	return ip.IsLoopback() || ip.IsPrivate() || cgnat
}

// ErrNotPublic is returned by DialControl for a refused address.
var ErrNotPublic = errors.New("the address points at this server or a private network")

// DialControl is a net.Dialer Control that checks the resolved address of every connection, so
// DNS rebinding cannot slip past a check made on the host name. allowPrivate, read at dial time,
// additionally admits PrivateNetwork addresses.
func DialControl(allowPrivate func() bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if PublicIP(ip) || (allowPrivate != nil && allowPrivate() && PrivateNetwork(ip)) {
			return nil
		}
		return ErrNotPublic
	}
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
