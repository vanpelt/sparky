// Package guestnet defines Sparkbox's IPv4 guest address space.
//
// A host prefix is divided into /30 slots. Within each slot the network
// address is unused, the host owns offset 1, the guest owns offset 2, and the
// broadcast address is unused. Keeping the arithmetic here prevents the VM
// driver, metadata service, and network accounting from disagreeing about
// which sandbox owns an address.
package guestnet

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

const (
	// DefaultPrefix is the compatibility address space used by standalone
	// installations which do not configure --guest-subnet.
	DefaultPrefix = "172.30.0.0/16"

	// DefaultFleetPrefixBits gives each fleet node 1,024 /30 guest slots while
	// leaving room to allocate non-overlapping node prefixes from a larger
	// private range.
	DefaultFleetPrefixBits = 20

	SlotBits = 30
)

// Network is a normalized IPv4 prefix divided into /30 guest slots.
type Network struct {
	prefix netip.Prefix
	base   uint32
}

// Slot is one point-to-point /30 assigned to a sandbox.
type Slot struct {
	Index  int
	Prefix netip.Prefix
	Host   netip.Addr
	Guest  netip.Addr
}

// Parse parses an IPv4 CIDR. An empty value selects DefaultPrefix. Host bits
// are masked so Network.String and Prefix always return canonical CIDR form.
func Parse(raw string) (Network, error) {
	if raw == "" {
		raw = DefaultPrefix
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		return Network{}, fmt.Errorf("guest subnet %q: %w", raw, err)
	}
	return FromPrefix(prefix)
}

// FromPrefix validates and normalizes an IPv4 prefix.
func FromPrefix(prefix netip.Prefix) (Network, error) {
	if !prefix.IsValid() {
		return Network{}, fmt.Errorf("guest subnet is invalid")
	}
	if !prefix.Addr().Is4() {
		return Network{}, fmt.Errorf("guest subnet %q: must be IPv4", prefix)
	}
	if prefix.Bits() > SlotBits {
		return Network{}, fmt.Errorf("guest subnet %q: need at least one /30 slot", prefix)
	}
	prefix = prefix.Masked()
	octets := prefix.Addr().As4()
	return Network{
		prefix: prefix,
		base:   binary.BigEndian.Uint32(octets[:]),
	}, nil
}

// MustParse is Parse for package defaults and tests.
func MustParse(raw string) Network {
	network, err := Parse(raw)
	if err != nil {
		panic(err)
	}
	return network
}

// Prefix returns the normalized guest prefix.
func (n Network) Prefix() netip.Prefix { return n.prefix }

func (n Network) String() string { return n.prefix.String() }

// Capacity returns the number of /30 slots in the prefix.
func (n Network) Capacity() int {
	if !n.prefix.IsValid() {
		return 0
	}
	return 1 << (SlotBits - n.prefix.Bits())
}

// Overlaps reports whether this network shares any address with other.
func (n Network) Overlaps(other Network) bool {
	return Overlaps(n.prefix, other.prefix)
}

// Slot returns the addresses assigned to index. Slot zero is valid.
func (n Network) Slot(index int) (Slot, error) {
	if index < 0 || index >= n.Capacity() {
		return Slot{}, fmt.Errorf("guest slot %d is outside %s (capacity %d)", index, n.prefix, n.Capacity())
	}
	base := n.base + uint32(index)*4
	network := addrFromUint32(base)
	return Slot{
		Index:  index,
		Prefix: netip.PrefixFrom(network, SlotBits),
		Host:   addrFromUint32(base + 1),
		Guest:  addrFromUint32(base + 2),
	}, nil
}

// SlotForGuest returns the slot index only when addr is exactly the guest
// offset (+2) of a /30 inside the network.
func (n Network) SlotForGuest(addr netip.Addr) (int, bool) {
	return n.slotForOffset(addr, 2)
}

// SlotForHost returns the slot index only when addr is exactly the host offset
// (+1) of a /30 inside the network.
func (n Network) SlotForHost(addr netip.Addr) (int, bool) {
	return n.slotForOffset(addr, 1)
}

// SlotContaining returns the /30 index containing addr, regardless of which
// address within the slot it is. It is useful for reserving an in-prefix host
// service address so Firecracker never assigns the same /30 to a guest.
func (n Network) SlotContaining(addr netip.Addr) (int, bool) {
	if !addr.Is4() || !n.prefix.Contains(addr) {
		return 0, false
	}
	octets := addr.As4()
	offset := binary.BigEndian.Uint32(octets[:]) - n.base
	index := int(offset / 4)
	return index, index < n.Capacity()
}

func (n Network) slotForOffset(addr netip.Addr, want uint32) (int, bool) {
	if !addr.Is4() || !n.prefix.Contains(addr) {
		return 0, false
	}
	octets := addr.As4()
	offset := binary.BigEndian.Uint32(octets[:]) - n.base
	if offset%4 != want {
		return 0, false
	}
	index := int(offset / 4)
	return index, index < n.Capacity()
}

// Overlaps reports whether two normalized IP prefixes share any address.
func Overlaps(a, b netip.Prefix) bool {
	if !a.IsValid() || !b.IsValid() || a.Addr().BitLen() != b.Addr().BitLen() {
		return false
	}
	a = a.Masked()
	b = b.Masked()
	return a.Contains(b.Addr()) || b.Contains(a.Addr())
}

func addrFromUint32(value uint32) netip.Addr {
	var octets [4]byte
	binary.BigEndian.PutUint32(octets[:], value)
	return netip.AddrFrom4(octets)
}

// MACFor is the guest NIC address for a network slot.
//
// It lives here, rather than in a driver, because TWO processes have to agree
// on it and they are not the same program. On the privileged-helper path the
// helper builds the QEMU argv, so the helper picks the MAC; everywhere else the
// driver does. A restored guest whose NIC came back on a different address
// would present the guest kernel with a new interface — its netcfg hook has
// never seen that MAC, and the sandbox loses its network on resume rather than
// at boot, which is the hardest version of this bug to find.
//
// 02: is the locally-administered unicast prefix, and the low two bytes encode
// the slot, which is why Network.Capacity above 1<<16 is refused by the drivers.
func MACFor(index int) string {
	return fmt.Sprintf("02:5b:01:00:%02x:%02x", (index>>8)&0xff, index&0xff)
}
