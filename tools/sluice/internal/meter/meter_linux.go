//go:build linux

// Package meter loads the sluice TC/eBPF program, attaches it to guest taps,
// reads per-remote-IP byte counters out of it, and mirrors the DNS-derived
// allow-set into the kernel so the same program can enforce egress.
package meter

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"github.com/vanpelt/sparky/tools/sluice/internal/report"
)

//go:embed sluice_bpfel.o
var bpfObject []byte

// flowKey mirrors `struct flow_key` — a remote address as a 16-byte IPv6 value
// (IPv4 held v4-mapped), matching netip.Addr.As16().
type flowKey [16]byte

// flowStats mirrors `struct flow_stats`.
type flowStats struct {
	TxBytes uint64
	RxBytes uint64
	TxPkts  uint64
	RxPkts  uint64
}

// Meter owns the loaded eBPF objects and every tap attachment.
type Meter struct {
	coll    *ebpf.Collection
	flows   *ebpf.Map
	allowed *ebpf.Map
	config  *ebpf.Map
	fromG   *ebpf.Program
	toG     *ebpf.Program

	links map[string]attachment // ifname -> its attachments
}

// attachment records the links for one interface plus the ifindex they were
// attached to, so a tap deleted and recreated under the same name (sparkbox
// reuses sbtap<idx> names) is detected as a new device and re-attached.
type attachment struct {
	index int
	links []link.Link
}

// Load brings up the eBPF maps and programs. It does not attach to any
// interface yet; call Attach for each tap.
func Load() (*Meter, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(bpfObject))
	if err != nil {
		return nil, fmt.Errorf("parse bpf object: %w", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("load bpf collection: %w", err)
	}
	m := &Meter{coll: coll, links: map[string]attachment{}}
	for name, dst := range map[string]**ebpf.Map{"flows": &m.flows, "allowed": &m.allowed, "config": &m.config} {
		mp, ok := coll.Maps[name]
		if !ok {
			coll.Close()
			return nil, fmt.Errorf("bpf object missing map %q", name)
		}
		*dst = mp
	}
	for name, dst := range map[string]**ebpf.Program{"from_guest": &m.fromG, "to_guest": &m.toG} {
		pr, ok := coll.Programs[name]
		if !ok {
			coll.Close()
			return nil, fmt.Errorf("bpf object missing program %q", name)
		}
		*dst = pr
	}
	return m, nil
}

// Attach wires both directions of the program onto ifname's clsact hooks and
// reports whether it performed a (re)attach. It is idempotent while the
// interface keeps the same ifindex; if the name now resolves to a different
// index (the tap was recreated), the stale links are dropped and it re-attaches
// to the new device. from_guest lands on ingress (packets the guest sent),
// to_guest on egress (packets bound for the guest).
func (m *Meter) Attach(ifname string) (bool, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return false, fmt.Errorf("lookup %s: %w", ifname, err)
	}
	if cur, ok := m.links[ifname]; ok {
		if cur.index == iface.Index {
			return false, nil // already attached to this exact device
		}
		m.Detach(ifname) // same name, new ifindex → drop stale links first
	}
	ingress, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Index, Program: m.fromG, Attach: ebpf.AttachTCXIngress,
	})
	if err != nil {
		return false, fmt.Errorf("attach ingress on %s: %w", ifname, err)
	}
	egress, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Index, Program: m.toG, Attach: ebpf.AttachTCXEgress,
	})
	if err != nil {
		ingress.Close()
		return false, fmt.Errorf("attach egress on %s: %w", ifname, err)
	}
	m.links[ifname] = attachment{index: iface.Index, links: []link.Link{ingress, egress}}
	return true, nil
}

// Detach removes the attachments for ifname (e.g. when a tap disappears).
func (m *Meter) Detach(ifname string) {
	for _, l := range m.links[ifname].links {
		l.Close()
	}
	delete(m.links, ifname)
}

// Attached reports whether ifname currently has the program attached.
func (m *Meter) Attached(ifname string) bool {
	_, ok := m.links[ifname]
	return ok
}

// AttachedNames returns the interfaces the meter is currently attached to, so
// the reconcile loop can detach ones whose tap has gone away.
func (m *Meter) AttachedNames() []string {
	out := make([]string, 0, len(m.links))
	for name := range m.links {
		out = append(out, name)
	}
	return out
}

// Flows reads the full per-remote-IP counter table.
func (m *Meter) Flows() (map[netip.Addr]report.Flow, error) {
	out := map[netip.Addr]report.Flow{}
	var key flowKey
	var val flowStats
	it := m.flows.Iterate()
	for it.Next(&key, &val) {
		addr := netip.AddrFrom16(key).Unmap()
		out[addr] = report.Flow{
			TxBytes: val.TxBytes, RxBytes: val.RxBytes,
			TxPkts: val.TxPkts, RxPkts: val.RxPkts,
		}
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterate flows: %w", err)
	}
	return out, nil
}

// SetEnforce flips the program between observe-only and drop-disallowed.
func (m *Meter) SetEnforce(on bool) error {
	var v uint32
	if on {
		v = 1
	}
	return m.config.Put(uint32(0), v)
}

// SyncAllowed makes the kernel allow-set exactly `want`: it adds newly-allowed
// addresses and removes ones that are no longer permitted. Called on a tick
// from the DNS proxy's live table.
func (m *Meter) SyncAllowed(want []netip.Addr) error {
	desired := make(map[flowKey]struct{}, len(want))
	for _, a := range want {
		desired[flowKey(a.As16())] = struct{}{}
	}
	// Remove stale keys.
	var key flowKey
	var val uint8
	var stale []flowKey
	it := m.allowed.Iterate()
	for it.Next(&key, &val) {
		if _, ok := desired[key]; !ok {
			stale = append(stale, key)
		}
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("iterate allowed: %w", err)
	}
	var errs []error
	for _, k := range stale {
		if err := m.allowed.Delete(k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			errs = append(errs, err)
		}
	}
	// Add desired keys (idempotent).
	for k := range desired {
		if err := m.allowed.Put(k, uint8(1)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close detaches everything and releases the eBPF objects.
func (m *Meter) Close() error {
	for name := range m.links {
		m.Detach(name)
	}
	if m.coll != nil {
		m.coll.Close()
	}
	return nil
}
