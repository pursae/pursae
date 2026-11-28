package netguard

import (
	"errors"
	"net"
	"testing"
)

func TestPublicIPRanges(t *testing.T) {
	for _, a := range []string{
		"127.0.0.1", "10.1.2.3", "192.168.0.1", "169.254.169.254", "100.64.0.1", "100.100.100.200",
		"168.63.129.16", "192.88.99.1", "0.0.0.0", "::1", "::ffff:127.0.0.1", "::ffff:0:7f00:1",
		"::ffff:0:a00:1", "64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "2002:7f00:1::", "100::1",
		"fd00:ec2::254", "fe80::1", "2001:db8::1", "64:ff9b:1::1",
	} {
		if PublicIP(net.ParseIP(a)) {
			t.Errorf("%s is treated as public", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "64:ff9b::808:808", "::ffff:8.8.8.8"} {
		if !PublicIP(net.ParseIP(a)) {
			t.Errorf("%s is treated as non-public", a)
		}
	}
}

func TestDialControl(t *testing.T) {
	deny := DialControl(func() bool { return false })
	allow := DialControl(func() bool { return true })
	if err := deny("tcp", "127.0.0.1:25", nil); !errors.Is(err, ErrNotPublic) {
		t.Fatalf("loopback passed: %v", err)
	}
	if err := allow("tcp", "127.0.0.1:25", nil); err != nil {
		t.Fatalf("opt-out refused loopback: %v", err)
	}
	for _, a := range []string{"169.254.169.254:80", "[fd00:ec2::254]:80", "168.63.129.16:80"} {
		if allow("tcp", a, nil) == nil {
			t.Errorf("opt-out admitted %s", a)
		}
	}
	if err := deny("tcp", "8.8.8.8:25", nil); err != nil {
		t.Fatal(err)
	}
}
