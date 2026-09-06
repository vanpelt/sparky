//go:build linux

// Package hostnet owns the host side of a guest's networking: the tap device,
// the addresses on it, and the proxy-NDP entry that makes a routed IPv6 /64
// reach a guest whose address lives on the tap rather than the uplink.
//
// None of it involves a VMM. Both drivers create the same tap the same way and
// tear it down the same way, and before this package each carried its own copy
// of the sequence — including the per-tap rp_filter sysctl, which is the
// anti-source-spoofing control internal/metadata's identify-by-source-address
// leans on. A hardening applied to one copy did not reach the other's guests,
// and nothing failed to say so.
//
// The one thing drivers legitimately disagree about is the tap NAME PREFIX,
// because each driver's startup sweep deletes stale devices carrying its own
// prefix and a shared prefix would mean constructing one driver eats the
// other's live taps. That is Plumbing.TapPrefix.
package hostnet

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
)

// Plumbing is one driver's host-network configuration. Every method is
// keyed on a slot index, which is the driver's own allocation unit.
type Plumbing struct {
	// Net is the IPv4 allocator handing each slot a /30.
	Net guestnet.Network
	// TapPrefix names this driver's tap devices. See the package comment for
	// why it is not shared.
	TapPrefix string
	// Prefix6 is the routed IPv6 /64, or nil when the host has no IPv6.
	Prefix6 net.IP
	// Uplink6 is the interface that answers NDP for guest addresses. Empty
	// disables proxy NDP.
	Uplink6 string
}

// TapName is the host device for one slot.
func (p Plumbing) TapName(idx int) string { return p.TapPrefix + strconv.Itoa(idx) }

// Slot is the IPv4 pair for one index. It panics rather than returning an
// error: slots come from the driver's allocator and are stored in its VM
// records, so an out-of-range one is a violated invariant, not bad input.
func (p Plumbing) Slot(idx int) guestnet.Slot {
	slot, err := p.Net.Slot(idx)
	if err != nil {
		panic(err)
	}
	return slot
}

func (p Plumbing) HostIP(idx int) string  { return p.Slot(idx).Host.String() }
func (p Plumbing) GuestIP(idx int) string { return p.Slot(idx).Guest.String() }

// The host takes the even address of each slot's /127 and the guest the odd
// one, leaving ::1 free for the edge. Callers outside this package must not
// re-derive that convention; a fifth copy getting it wrong hands a guest the
// host's own AAAA target.
func (p Plumbing) HostIP6(idx int) string  { return p.Addr6((idx + 1) * 2) }
func (p Plumbing) GuestIP6(idx int) string { return p.Addr6((idx+1)*2 + 1) }

// Addr6 places off in the low 32 bits of Prefix6's host portion.
//
// It ORs rather than assigns. Assigning is correct only for a prefix of /96 or
// shorter, where those four bytes are entirely host bits — and callers accept
// prefixes down to /112. With, say, 2001:db8::abcd:0/112, assignment zeroes
// bytes 12 and 13 and slot 0's guest address comes out as 2001:db8::3 instead
// of 2001:db8::abcd:3: an address outside the prefix, which nothing routes.
// The bound that makes OR safe is the caller's: it refuses a Subnet6 whose host
// bits cannot cover (capacity*2 + 2) addresses, so off never overflows into the
// prefix.
func (p Plumbing) Addr6(off int) string {
	ip := make(net.IP, net.IPv6len)
	copy(ip, p.Prefix6)
	ip[12] |= byte(off >> 24)
	ip[13] |= byte(off >> 16)
	ip[14] |= byte(off >> 8)
	ip[15] |= byte(off)
	return ip.String()
}

// CreateTap brings up the device for one slot with its addresses.
func (p Plumbing) CreateTap(ctx context.Context, idx int) error {
	tap := p.TapName(idx)
	cmds := [][]string{
		{"ip", "tuntap", "add", "dev", tap, "mode", "tap"},
		{"ip", "addr", "add", p.HostIP(idx) + "/30", "dev", tap},
	}
	if p.Prefix6 != nil {
		// Host side of the point-to-point /127; the connected route this
		// creates is how inbound traffic to the guest's /128 reaches the tap.
		cmds = append(cmds, []string{"ip", "-6", "addr", "add", p.HostIP6(idx) + "/127", "dev", tap})
	}
	cmds = append(cmds, []string{"ip", "link", "set", "dev", tap, "up"})
	for _, c := range cmds {
		if out, err := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %v: %s", c, err, strings.TrimSpace(string(out)))
		}
	}
	// Strict reverse-path filtering: drop any packet from this tap whose source
	// address doesn't route back to it, so a guest can't source-spoof a
	// neighbour's address across the host's inter-tap forwarding. Set per-tap
	// because the kernel takes the max of the "all" and per-device values, so a
	// permissive host default can't undo it.
	//
	// Best-effort, like the proxy-NDP setup below: this is defence in depth,
	// not the guarantee. The metadata service identifies callers by source
	// address (see internal/metadata), and TCP already makes that unspoofable —
	// a forged SYN is answered towards the real owner of the address, so the
	// spoofer never completes the handshake. Failing sandbox creation over this
	// would trade a whole-host outage for no real security.
	exec.CommandContext(ctx, "sysctl", "-qw", "net.ipv4.conf."+tap+".rp_filter=1").Run() //nolint:errcheck

	// Answer NDP for this guest's /128 on the uplink so the provider's on-link
	// delivery of the routed /64 reaches the VM (its address lives on the tap,
	// not the uplink). Best-effort: the VM still boots if this fails, it just
	// won't have v6 return traffic. del-then-add keeps it idempotent.
	if p.Prefix6 != nil && p.Uplink6 != "" {
		exec.CommandContext(ctx, "sysctl", "-qw", "net.ipv6.conf."+p.Uplink6+".proxy_ndp=1").Run()             //nolint:errcheck
		exec.CommandContext(ctx, "ip", "-6", "neigh", "del", "proxy", p.GuestIP6(idx), "dev", p.Uplink6).Run() //nolint:errcheck
		exec.CommandContext(ctx, "ip", "-6", "neigh", "add", "proxy", p.GuestIP6(idx), "dev", p.Uplink6).Run() //nolint:errcheck
	}
	return nil
}

// DeleteTap removes one slot's device and its proxy-NDP entry. Best-effort:
// there is nothing useful a caller can do about a failure here.
func (p Plumbing) DeleteTap(idx int) {
	if p.Prefix6 != nil && p.Uplink6 != "" {
		exec.Command("ip", "-6", "neigh", "del", "proxy", p.GuestIP6(idx), "dev", p.Uplink6).Run() //nolint:errcheck
	}
	exec.Command("ip", "link", "del", p.TapName(idx)).Run() //nolint:errcheck
}

// SweepStale deletes leftover taps carrying this driver's prefix. A previous
// process leaves its devices behind and the next Create would fail with
// "Device or resource busy", so this runs at construction — when nothing of
// ours is running. It matches TapPrefix and nothing else, which is what keeps
// one driver's sweep from eating another's live taps.
func (p Plumbing) SweepStale() {
	out, err := exec.Command("ip", "-o", "link", "show").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// "3: sbtap1@if4: <BROADCAST,...>" -> field 1 is the name.
		parts := strings.SplitN(line, ": ", 3)
		if len(parts) < 2 {
			continue
		}
		name := parts[1]
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		if strings.HasPrefix(name, p.TapPrefix) {
			exec.Command("ip", "link", "del", name).Run() //nolint:errcheck
		}
	}
}

// DefaultRoute6Dev reports the interface carrying the IPv6 default route, which
// is the one that must answer NDP for guest addresses.
func DefaultRoute6Dev() string {
	out, err := exec.Command("ip", "-6", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// MAC is a stable locally-administered address for one slot. oui3 is the third
// octet and separates the drivers, so two drivers sharing a subnet on one host
// (which only the parity suite does) cannot hand out the same MAC.
func MAC(oui3 byte, idx int) string {
	return fmt.Sprintf("02:5b:%02x:00:%02x:%02x", oui3, (idx>>8)&0xff, idx&0xff)
}
