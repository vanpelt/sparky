//go:build linux

// Package guestargs holds the parts of a guest kernel command line that every
// VMM driver must produce IDENTICALLY.
//
// The guest parses these tokens — sparkbox-netcfg reads the ip= block,
// sparkbox-repos reads the sandbox markers — so a driver that drifts by one
// token boots a guest that comes up and then quietly misbehaves, which is the
// worst failure shape available here. Before this package the QEMU driver
// carried byte-identical copies under a comment saying they must never
// diverge; a comment is not a mechanism, and this is.
//
// What is NOT here is everything drivers legitimately disagree about — root=,
// console=, pci=off and the rest. Those stay in each driver's own argv builder.
package guestargs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
)

// MachineID derives this sandbox's /etc/machine-id from its name.
//
// It exists because of forks and old templates. Current base images and
// captures carry an empty machine-id, but older templates can be byte-for-byte
// copies of somebody's populated rootfs, and PID 1 reads that file before any
// unit runs. systemd reads systemd.machine_id= off the kernel command line when
// the file is uninitialised; the host writes that argument per boot and no guest
// can forge it, so every clean fork differs from its parent from PID 1 onward.
//
// Derived rather than random so it is STABLE across the sandbox's own boots: a
// machine id that changed every time would give journald a new machine
// directory on every resume. It changes on a rename, which is the same
// tradeoff the hostname already makes.
//
// The guest-side pre-capture clear and sparkbox-identity-reset stay regardless:
// they cover dbus, SSH host keys, old templates, and images with no systemd to
// read this at all.
func MachineID(name string) string {
	sum := sha256.Sum256([]byte("sparkbox-machine-id\x00" + name))
	return hex.EncodeToString(sum[:16])
}

// ValidateDNS accepts only the empty string (feature off), the "gateway"
// sentinel, or a bare IP literal. Anything else — a hostname, or a value with
// whitespace that would inject extra kernel args — is rejected, so a typo in
// --guest-dns fails loudly instead of producing a malformed cmdline or an
// unusable /etc/resolv.conf inside the guest.
func ValidateDNS(guestDNS string) error {
	switch guestDNS {
	case "", "gateway":
		return nil
	}
	if _, err := netip.ParseAddr(guestDNS); err != nil {
		return fmt.Errorf("guest-dns %q: must be \"gateway\" or an IP address", guestDNS)
	}
	return nil
}

// DNSArg builds the sparkbox_dns kernel-arg fragment (with a leading space)
// for the guest netcfg hook. The sentinel "gateway" expands to this VM's gateway
// address, where the sluice allowlist resolver listens; an IP literal is used
// verbatim. An empty setting yields no arg, leaving the guest on public DNS.
func DNSArg(guestDNS, gatewayIP string) (string, error) {
	if err := ValidateDNS(guestDNS); err != nil {
		return "", err
	}
	switch guestDNS {
	case "":
		return "", nil
	case "gateway":
		return " sparkbox_dns=" + gatewayIP, nil
	default:
		return " sparkbox_dns=" + guestDNS, nil
	}
}
