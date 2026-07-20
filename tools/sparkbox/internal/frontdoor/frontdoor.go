// Package frontdoor gives every sandbox a public, deterministic "front door"
// IPv6 address inside the host's delegated /64, enabling hostname-based SSH
// routing (`ssh myvm.hivemind.tools` instead of `ssh myvm@gateway`). SSH
// carries no hostname on the wire, so the address the client dialed is the
// only in-band signal: DNS points <name>.<domain> at the name's front door,
// the host claims the whole range with an AnyIP local route, and the gateway
// maps an accepted connection's destination address back to the sandbox.
//
// Address layout inside the /64: the firecracker driver allocates host/guest
// pairs from the low 32 bits of the interface ID (see fc.go addr6), so front
// doors live in the UPPER /65 — interface ID = first 8 bytes of
// HMAC-SHA256("sparkbox-frontdoor/v1", name) with the top bit forced on. The
// derivation is stateless (no allocation table, nothing to persist) and the
// two ranges can never collide. Reversing an address back to a name is a scan
// over candidate names; the HMAC is one-way by design.
package frontdoor

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net"
)

// hmacKey is the derivation domain separator. Changing it moves every front
// door (and thus invalidates published DNS records) — treat it as versioned.
const hmacKey = "sparkbox-frontdoor/v1"

// Mapper derives front-door addresses inside a delegated IPv6 prefix.
type Mapper struct {
	prefix net.IP // 16 bytes, host portion zero
}

// New parses a routable prefix in CIDR form (e.g. "2001:db8:1c7::/64"). The
// prefix must be /64 or wider so the full 8-byte interface ID is ours to set.
func New(subnet6 string) (*Mapper, error) {
	_, ipNet, err := net.ParseCIDR(subnet6)
	if err != nil {
		return nil, fmt.Errorf("frontdoor subnet %q: %w", subnet6, err)
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 128 {
		return nil, fmt.Errorf("frontdoor subnet %q: not IPv6", subnet6)
	}
	if ones > 64 {
		return nil, fmt.Errorf("frontdoor subnet %q: need /64 or wider", subnet6)
	}
	prefix := make(net.IP, net.IPv6len)
	copy(prefix, ipNet.IP.To16())
	return &Mapper{prefix: prefix}, nil
}

// Addr returns name's front-door address. Deterministic: the same name maps
// to the same address on every host sharing the prefix, forever.
func (m *Mapper) Addr(name string) net.IP {
	mac := hmac.New(sha256.New, []byte(hmacKey))
	mac.Write([]byte(name))
	sum := mac.Sum(nil)
	ip := make(net.IP, net.IPv6len)
	copy(ip, m.prefix[:8])
	copy(ip[8:], sum[:8])
	ip[8] |= 0x80 // upper /65: the driver's guest/host pairs own the lower half
	return ip
}

// Contains reports whether ip lies in the front-door range (the prefix's
// upper /65). Guest and host addresses from the same /64 are NOT contained.
func (m *Mapper) Contains(ip net.IP) bool {
	ip = ip.To16()
	if ip == nil || ip.To4() != nil {
		return false
	}
	return bytes.Equal(ip[:8], m.prefix[:8]) && ip[8]&0x80 != 0
}

// Range returns the CIDR the host should claim with an AnyIP local route
// (`ip -6 route add local <Range> dev lo`): the prefix's upper /65.
func (m *Mapper) Range() string {
	ip := make(net.IP, net.IPv6len)
	copy(ip, m.prefix)
	ip[8] = 0x80
	return ip.String() + "/65"
}

// Resolve maps a dialed address to the matching candidate name. The
// derivation is one-way, so this is a linear scan — fine for the tens of
// sandboxes a single host runs.
func (m *Mapper) Resolve(ip net.IP, names []string) (string, bool) {
	if !m.Contains(ip) {
		return "", false
	}
	for _, n := range names {
		if m.Addr(n).Equal(ip) {
			return n, true
		}
	}
	return "", false
}
