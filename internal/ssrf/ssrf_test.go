package ssrf

import (
	"net/netip"
	"testing"
)

func TestBlockedIPTransitionMechanisms(t *testing.T) {
	blocked := []string{
		"2002:a9fe:a9fe::",   // 6to4 embedding 169.254.169.254
		"2002:7f00:1::",      // 6to4 embedding 127.0.0.1
		"2001::a9fe:a9fe",    // Teredo
		"64:ff9b::a9fe:a9fe", // NAT64 well-known embedding 169.254.169.254
	}
	for _, s := range blocked {
		if !BlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("BlockedIP(%s) = false, want true", s)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"2606:4700:4700::1111",
	}
	for _, s := range allowed {
		if BlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("BlockedIP(%s) = true, want false", s)
		}
	}
}
