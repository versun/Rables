// Package ssrf guards server-side outbound requests against internal-network
// targets: only http(s) URLs whose host resolves to at least one address and
// none blocked pass. DNS failures count as unsafe.
package ssrf

import (
	"context"
	"net"
	"net/netip"
	"net/url"
)

// LookupFunc resolves a host to IP addresses (tests stub it); nil selects
// LookupIP. IP literals are expected to short-circuit without DNS.
type LookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// blockedIPPrefixes mirrors ImportRss::BLOCKED_IP_RANGES (private, loopback,
// link-local and reserved ranges).
var blockedIPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	// Teredo (RFC 4380) and 6to4 embed a client IPv4 in the address bits and
	// are deprecated (RFC 7526); a DNS AAAA answer like 2002:a9fe:a9fe::
	// would smuggle 169.254.169.254 past the IPv4 checks. No legitimate
	// server-side outbound use, so both are blocked wholesale.
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use prefix (RFC 8215)
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

// nat64WellKnown is the NAT64 well-known prefix (RFC 6052): the low 32 bits
// embed the destination IPv4 address, which BlockedIP unwraps and re-checks.
var nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")

// SafeRemoteURL reports whether rawurl is an http(s) URL whose host resolves
// to at least one address with none blocked. DNS failures are unsafe.
func SafeRemoteURL(ctx context.Context, rawurl string, lookup LookupFunc) bool {
	u, err := url.Parse(rawurl)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if lookup == nil {
		lookup = LookupIP
	}
	addrs, err := lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		if BlockedIP(addr) {
			return false
		}
	}
	return true
}

// BlockedIP reports whether addr falls into any blocked range (IPv4-mapped
// IPv6 addresses are unmapped first, matching Resolv's plain-IPv4 strings).
// Zoned addresses are refused outright: Prefix.Contains never matches them
// (the zone is not part of the address bits), and a zone has no legitimate
// use in the URL of a remote feed. Addresses under the NAT64 well-known
// prefix embed the destination IPv4 in their low 32 bits, so on an
// IPv6-only + NAT64 network a DNS AAAA answer can smuggle a blocked IPv4
// (64:ff9b::a9fe:a9fe carries 169.254.169.254); the embedded address is
// unwrapped and checked against the same list.
func BlockedIP(addr netip.Addr) bool {
	if addr.Zone() != "" {
		return true
	}
	addr = addr.Unmap()
	if nat64WellKnown.Contains(addr) {
		a16 := addr.As16()
		addr = netip.AddrFrom4([4]byte{a16[12], a16[13], a16[14], a16[15]})
	}
	for _, prefix := range blockedIPPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// LookupIP resolves host via the system resolver; IP literals skip DNS.
func LookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		if addr, ok := netip.AddrFromSlice(ip); ok {
			addrs = append(addrs, addr)
		}
	}
	return addrs, nil
}
