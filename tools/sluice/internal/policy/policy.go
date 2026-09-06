// Package policy holds sluice's live, swappable egress policy: a base allowlist
// that applies to every tap, plus per-tap allowlists keyed by tap name
// (sbtap<idx>). sparkbox pushes the per-tap lists over the control socket as a
// VM's tags change; the base is the operator's static --allowlist file.
//
// The policy drives two things:
//
//   - DNS resolution, decided per client (AllowedFor). A query's source address
//     identifies the guest's tap. A tap that has a per-tap policy resolves only
//     the base list plus its own list; a name it has no grant for gets an
//     NXDOMAIN. A tap with no per-tap policy resolves anything in default-allow
//     mode (an untagged sandbox is unfiltered) and only the base list otherwise
//     (the classic all-taps behaviour). Access, not resolution, is the real
//     gate — the eBPF allow-set still drops egress independently — but resolving
//     per-tap gives a policied VM a clean, honest DNS answer.
//
//   - Per-tap grants: given the DNS proxy's live IP→domain table, the set of
//     resolved addresses a given tap may reach. The base's matches (plus pinned
//     infrastructure) become the fleet-wide wildcard allow-set; each tap's own
//     matches become that tap's allow-set.
//
// All methods are safe for concurrent use: the DNS path reads AllowedFor on
// every query while the socket occasionally replaces the per-tap lists.
package policy

import (
	"fmt"
	"net/netip"
	"strconv"
	"sync"

	"github.com/vanpelt/sparky/tools/sluice/internal/allowlist"
	"github.com/vanpelt/sparky/tools/sluice/internal/ipmap"
)

// Policy is the live egress policy. The zero value is not usable; call New.
type Policy struct {
	mu           sync.RWMutex
	base         *allowlist.List
	perTap       map[string]*allowlist.List // tap name (sbtap<idx>) -> its allowlist
	defaultAllow bool                       // a tap with no per-tap policy resolves anything
	guestPrefix  netip.Prefix               // divided into sequential /30 guest slots
	tapPrefix    string                     // must match the meter's --tap-prefix
}

// New returns a Policy with the given base list and no per-tap overrides. base
// may be nil, which is treated as an empty list (allows nothing on its own).
func New(base *allowlist.List) *Policy {
	if base == nil {
		base, _ = allowlist.New(nil)
	}
	return &Policy{
		base: base, perTap: map[string]*allowlist.List{},
		guestPrefix: netip.MustParsePrefix("172.30.0.0/16"),
		tapPrefix:   defaultTapPrefix,
	}
}

// defaultTapPrefix must stay equal to the --tap-prefix default in
// cmd/sluice/run.go and to sparkbox's own tap naming. Nothing enforces that
// across the two modules; TestTapPrefixMatchesTheFlagDefault checks the half
// that is checkable from here.
const defaultTapPrefix = "sbtap"

// SetTapPrefix configures the interface-name prefix this policy derives tap
// names with. It must match the prefix the meter attaches to.
//
// It exists because those two used to be able to disagree. The meter attached
// to whatever --tap-prefix said while this file hardcoded "sbtap", so running
// with any other value produced a policy whose per-tap rules were looked up
// under names no interface had — every DNS decision silently falling through
// to the base list, and every denial attributed to "".
func (p *Policy) SetTapPrefix(prefix string) {
	if prefix == "" {
		prefix = defaultTapPrefix
	}
	p.mu.Lock()
	p.tapPrefix = prefix
	p.mu.Unlock()
}

// SetGuestSubnet configures the IPv4 prefix sparkbox divides into sequential
// /30 slots. It must match sparkbox's --guest-subnet value.
func (p *Policy) SetGuestSubnet(raw string) error {
	prefix, err := netip.ParsePrefix(raw)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() > 30 {
		return fmt.Errorf("invalid IPv4 guest subnet %q", raw)
	}
	p.mu.Lock()
	p.guestPrefix = prefix.Masked()
	p.mu.Unlock()
	return nil
}

// SetDefaultAllow controls what a tap with no per-tap policy may resolve. When
// true (sparkbox's "untagged is unlimited" mode) such a tap resolves anything;
// when false (the default, classic mode) it is held to the base list only.
func (p *Policy) SetDefaultAllow(v bool) {
	p.mu.Lock()
	p.defaultAllow = v
	p.mu.Unlock()
}

// IsEnforced reports whether a tap has a per-tap policy — i.e. it is a policied
// sandbox whose egress the eBPF layer should filter. A tap with no policy is
// left unrestricted in default-allow mode.
func (p *Policy) IsEnforced(tap string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.perTap[tap]
	return ok
}

// SetBase replaces the base allowlist (e.g. on a config reload).
func (p *Policy) SetBase(base *allowlist.List) {
	if base == nil {
		base, _ = allowlist.New(nil)
	}
	p.mu.Lock()
	p.base = base
	p.mu.Unlock()
}

// ReplaceTaps swaps the entire per-tap policy set atomically. A tap absent from
// taps reverts to base-only. Callers build the map from a PUT /policy body.
func (p *Policy) ReplaceTaps(taps map[string]*allowlist.List) {
	next := make(map[string]*allowlist.List, len(taps))
	for name, l := range taps {
		if l != nil {
			next[name] = l
		}
	}
	p.mu.Lock()
	p.perTap = next
	p.mu.Unlock()
}

// Allowed implements the union view: a name is resolvable if the base or any
// per-tap list permits it. It is the fallback the DNS proxy uses for a client it
// cannot attribute to a tap; AllowedFor is the per-client path. The returned
// pattern is the base match when there is one, else the first per-tap match.
func (p *Policy) Allowed(name string) (bool, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if ok, pat := p.base.Allowed(name); ok {
		return true, pat
	}
	for _, l := range p.perTap {
		if ok, pat := l.Allowed(name); ok {
			return true, pat
		}
	}
	return false, ""
}

// AllowedFor decides resolution for a specific guest, identified by the source
// address of its DNS query. The base list is a floor for everyone. Beyond that:
// a tap with its own policy resolves only what that policy permits (a clean
// NXDOMAIN otherwise); a tap with no policy resolves anything in default-allow
// mode and nothing extra otherwise. A client that maps to no tap (the host, or a
// non-guest address) is treated as having no policy.
func (p *Policy) AllowedFor(client netip.Addr, name string) (bool, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if ok, pat := p.base.Allowed(name); ok {
		return true, pat
	}
	if tap := tapForGuest(p.tapPrefix, p.guestPrefix, client); tap != "" {
		if l := p.perTap[tap]; l != nil {
			if ok, pat := l.Allowed(name); ok {
				return true, pat
			}
			return false, "" // policied tap: not on its list → deny
		}
	}
	if p.defaultAllow {
		return true, "" // untagged sandbox: unrestricted
	}
	return false, ""
}

// TapForClient maps a DNS client's source address to the tap that owns its /30
// slot. It is also used by the denial recorder, so the DNS decision and its
// attribution cannot disagree.
func (p *Policy) TapForClient(a netip.Addr) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return tapForGuest(p.tapPrefix, p.guestPrefix, a)
}

func tapForGuest(tapPrefix string, prefix netip.Prefix, a netip.Addr) string {
	if !a.Is4() {
		if a.Is4In6() {
			a = a.Unmap()
		} else {
			return ""
		}
	}
	if !a.Is4() {
		return ""
	}
	if !prefix.Contains(a) {
		return ""
	}
	o := a.As4()
	b := prefix.Addr().As4()
	addr := uint32(o[0])<<24 | uint32(o[1])<<16 | uint32(o[2])<<8 | uint32(o[3])
	base := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	offset := addr - base
	if offset%4 != 2 {
		return ""
	}
	return tapPrefix + strconv.FormatUint(uint64(offset/4), 10)
}

// TapPatterns returns the canonical patterns configured for a tap (nil if the
// tap has no per-tap policy), for display/debugging.
func (p *Policy) TapPatterns(tap string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if l := p.perTap[tap]; l != nil {
		return l.Patterns()
	}
	return nil
}

// BaseGrants returns the addresses every tap may reach: pinned infrastructure
// (gateways, static --allow-ip entries — Expire zero) plus any resolved address
// whose domain the base list permits. These become the ifindex-0 wildcard set,
// preserving the pre-per-tap behaviour where the static allowlist applied fleet
// wide.
func (p *Policy) BaseGrants(snap []ipmap.Entry) []netip.Addr {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	var out []netip.Addr
	for _, e := range snap {
		if e.Expire.IsZero() { // pinned infra — always reachable
			out = append(out, e.Addr)
			continue
		}
		if ok, _ := base.Allowed(e.Domain); ok {
			out = append(out, e.Addr)
		}
	}
	return out
}

// TapGrants returns the additional addresses a specific tap may reach: resolved
// addresses whose domain that tap's own list permits. Infra is already covered
// by BaseGrants (the wildcard), so it is not repeated here. A tap with no
// per-tap policy grants nothing extra.
func (p *Policy) TapGrants(tap string, snap []ipmap.Entry) []netip.Addr {
	p.mu.RLock()
	l := p.perTap[tap]
	p.mu.RUnlock()
	if l == nil {
		return nil
	}
	var out []netip.Addr
	for _, e := range snap {
		if e.Expire.IsZero() {
			continue // infra handled by the wildcard
		}
		if ok, _ := l.Allowed(e.Domain); ok {
			out = append(out, e.Addr)
		}
	}
	return out
}
