package server

import (
	"testing"
	"time"
)

// Hosts of one network share a registration budget below the global one, so they cannot lock out
// clients from elsewhere.
func TestT2NMiscOAuthRegisterPerNetwork(t *testing.T) {
	oauthRegisterLimiter.Lock()
	saved, savedG := oauthRegisterLimiter.hits, oauthRegisterLimiter.global
	oauthRegisterLimiter.hits, oauthRegisterLimiter.global = map[string]oauthRegisterHits{}, oauthRegisterHits{}
	oauthRegisterLimiter.Unlock()
	defer func() {
		oauthRegisterLimiter.Lock()
		oauthRegisterLimiter.hits, oauthRegisterLimiter.global = saved, savedG
		oauthRegisterLimiter.Unlock()
	}()
	now := time.Now()
	allow := func(addr string) bool {
		return oauthRegisterAllowNet(oauthRegisterKey(addr), oauthRegisterNet(addr), now)
	}

	allowed := 0
	for i := 0; i < 250; i++ {
		if allow("198.51.100." + itoa64(int64(i))) {
			allowed++
		}
	}
	if allowed != oauthRegisterNetLimit {
		t.Errorf("one /24 registered %d clients, want %d", allowed, oauthRegisterNetLimit)
	}
	v6 := 0
	for i := 0; i < 200; i++ {
		if allow("2001:db8:aa:" + itoa64(int64(i)) + "::1") {
			v6++
		}
	}
	if v6 != oauthRegisterNetLimit {
		t.Errorf("one IPv6 /48 registered %d clients, want %d", v6, oauthRegisterNetLimit)
	}
	if !allow("203.0.113.5") {
		t.Error("a client from another network was locked out")
	}
}
