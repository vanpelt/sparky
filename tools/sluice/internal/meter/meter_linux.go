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
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"github.com/vanpelt/sparky/tools/sluice/internal/report"
)

//go:embed sluice_bpfel.o
var bpfObject []byte

// flowKey mirrors `struct flow_key` — (tap ifindex, remote address). The remote
// is a 16-byte IPv6 value (IPv4 held v4-mapped), matching netip.Addr.As16().
// Ifindex 0 is the all-VMs wildcard in the allow-set. The field order and sizes
// match the C struct exactly (u32 then 16 bytes, 4-byte aligned, no padding).
type flowKey struct {
	Ifindex uint32
	Addr    [16]byte
}

// flowStats mirrors `struct flow_stats`.
type flowStats struct {
	TxBytes uint64
	RxBytes uint64
	TxPkts  uint64
	RxPkts  uint64
}

// Meter owns the loaded eBPF objects and every tap attachment.
type Meter struct {
	coll     *ebpf.Collection
	flows    *ebpf.Map
	allowed  *ebpf.Map
	config   *ebpf.Map
	enforced *ebpf.Map
	fromG    *ebpf.Program
	toG      *ebpf.Program

	mu    sync.RWMutex
	links map[string]attachment // ifname -> its attachments
	ready map[string]bool       // full reconcile completed after attachment
}

// attachment records the links for one interface plus the ifindex they were
// attached to, so a tap deleted and recreated under the same name (sparkbox
// reuses sbtap<idx> names) is detected as a new device and re-attached. The
// index doubles as the per-VM discriminator the allow-set and flow counters key
// on (see Ifaces / FlowsByIface).
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
	m := &Meter{coll: coll, links: map[string]attachment{}, ready: map[string]bool{}}
	for name, dst := range map[string]**ebpf.Map{"flows": &m.flows, "allowed": &m.allowed, "config": &m.config, "enforced": &m.enforced} {
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
	m.mu.Lock()
	defer m.mu.Unlock()
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return false, fmt.Errorf("lookup %s: %w", ifname, err)
	}
	if cur, ok := m.links[ifname]; ok {
		if cur.index == iface.Index {
			return false, nil // already attached to this exact device
		}
		m.detachLocked(ifname) // same name, new ifindex → drop stale links first
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
	m.ready[ifname] = false
	return true, nil
}

// Detach removes the attachments for ifname (e.g. when a tap disappears) and
// drops that tap's per-ifindex entries from the flows and allow-set maps, so a
// churned-through VM doesn't leak map space. The ifindex-0 wildcard is never
// touched.
func (m *Meter) Detach(ifname string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.detachLocked(ifname)
}

func (m *Meter) detachLocked(ifname string) {
	att, ok := m.links[ifname]
	for _, l := range att.links {
		l.Close()
	}
	if ok && att.index > 0 {
		m.forgetIface(uint32(att.index))
	}
	delete(m.links, ifname)
	delete(m.ready, ifname)
}

// forgetIface deletes every flows and allowed entry carrying ifindex. Keys are
// collected first, then deleted, since deleting mid-iteration is unsafe. The
// ifindex-0 wildcard is never evicted.
func (m *Meter) forgetIface(ifindex uint32) {
	if ifindex == 0 {
		return
	}
	del := func(mp *ebpf.Map, val any) {
		if mp == nil {
			return
		}
		var key flowKey
		var stale []flowKey
		it := mp.Iterate()
		for it.Next(&key, val) {
			if key.Ifindex == ifindex {
				stale = append(stale, key)
			}
		}
		for _, k := range stale {
			_ = mp.Delete(k)
		}
	}
	var stats flowStats
	var one uint8
	del(m.flows, &stats)
	del(m.allowed, &one)
	if m.enforced != nil {
		if err := m.enforced.Delete(ifindex); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			// best-effort cleanup; a stale entry is harmless (ifindexes are not reused while attached)
			_ = err
		}
	}
}

// Ifaces returns a snapshot of the attached taps as ifindex→ifname, letting a
// caller attribute a per-ifindex flow to a named tap (and thus to a VM).
func (m *Meter) Ifaces() map[uint32]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[uint32]string, len(m.links))
	for name, att := range m.links {
		out[uint32(att.index)] = name
	}
	return out
}

// Attached reports whether ifname currently has the program attached.
func (m *Meter) Attached(ifname string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.links[ifname]
	return ok
}

// SetReady records whether an attached interface has completed a full
// allow-set and enforcement reconcile. It is separate from Attached so a VMM
// launcher cannot race the interval between TCX attachment and map sync.
func (m *Meter) SetReady(ifname string, ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.links[ifname]; ok {
		m.ready[ifname] = ready
	}
}

// Ready reports whether the interface is attached and fully reconciled.
func (m *Meter) Ready(ifname string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready[ifname]
}

// AttachedNames returns the interfaces the meter is currently attached to, so
// the reconcile loop can detach ones whose tap has gone away.
func (m *Meter) AttachedNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.links))
	for name := range m.links {
		out = append(out, name)
	}
	return out
}

// Flows reads the counter table aggregated across every tap: bytes to the same
// remote IP from different VMs are summed under one key. This is the fleet-wide
// view the periodic stdout report uses; FlowsByIface keeps them separate.
func (m *Meter) Flows() (map[netip.Addr]report.Flow, error) {
	out := map[netip.Addr]report.Flow{}
	var key flowKey
	var val flowStats
	it := m.flows.Iterate()
	for it.Next(&key, &val) {
		addr := netip.AddrFrom16(key.Addr).Unmap()
		f := out[addr]
		f.TxBytes += val.TxBytes
		f.RxBytes += val.RxBytes
		f.TxPkts += val.TxPkts
		f.RxPkts += val.RxPkts
		out[addr] = f
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterate flows: %w", err)
	}
	return out, nil
}

// FlowsByIface reads the counter table broken out per tap ifindex, so a caller
// can attribute per-domain bandwidth to a specific VM. Join the ifindex to a tap
// name with Ifaces(). A single remote IP under two ifindexes yields two entries.
func (m *Meter) FlowsByIface() (map[uint32]map[netip.Addr]report.Flow, error) {
	out := map[uint32]map[netip.Addr]report.Flow{}
	var key flowKey
	var val flowStats
	it := m.flows.Iterate()
	for it.Next(&key, &val) {
		addr := netip.AddrFrom16(key.Addr).Unmap()
		per := out[key.Ifindex]
		if per == nil {
			per = map[netip.Addr]report.Flow{}
			out[key.Ifindex] = per
		}
		per[addr] = report.Flow{
			TxBytes: val.TxBytes, RxBytes: val.RxBytes,
			TxPkts: val.TxPkts, RxPkts: val.RxPkts,
		}
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterate flows: %w", err)
	}
	return out, nil
}

// SetEnforce flips the master switch between observe-only and enforcing. It
// gates enforcement globally; which taps are actually filtered is decided
// per-tap by SetEnforceFor, so this alone drops nothing unless a tap is also in
// the enforced set.
func (m *Meter) SetEnforce(on bool) error {
	var v uint32
	if on {
		v = 1
	}
	return m.config.Put(uint32(0), v)
}

// SetEnforceFor adds or removes a tap (ifindex) from the enforced set. Only taps
// in the set are subject to allow-set drops (and only while the master switch is
// on); a tap left out is metered but unrestricted — an untagged sandbox's
// unlimited egress. Removing an absent tap is a no-op.
func (m *Meter) SetEnforceFor(ifindex uint32, on bool) error {
	if ifindex == 0 {
		return nil // ifindex 0 is the allow-set wildcard, never a real tap
	}
	if on {
		return m.enforced.Put(ifindex, uint8(1))
	}
	if err := m.enforced.Delete(ifindex); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	return nil
}

// SyncAllowed makes the fleet-wide (ifindex 0) allow-set exactly `want`. Those
// wildcard entries permit the address for every tap, so this preserves the
// old behaviour where any resolved name is reachable by all VMs. Per-tap policy
// goes through SyncAllowedFor and is reconciled independently.
func (m *Meter) SyncAllowed(want []netip.Addr) error {
	return m.syncAllowedKeyed(0, want)
}

// SyncAllowedFor makes the allow-set for a single tap (ifindex) exactly `want`.
// It only touches keys carrying that ifindex, leaving the ifindex-0 wildcard and
// other taps' entries alone, so callers can mix a global infra allow-set with
// per-VM policy.
func (m *Meter) SyncAllowedFor(ifindex uint32, want []netip.Addr) error {
	return m.syncAllowedKeyed(ifindex, want)
}

// syncAllowedKeyed reconciles exactly the allow-set entries carrying ifindex:
// it adds the desired (ifindex, addr) keys and removes stale ones, never
// disturbing entries under a different ifindex.
func (m *Meter) syncAllowedKeyed(ifindex uint32, want []netip.Addr) error {
	desired := make(map[flowKey]struct{}, len(want))
	for _, a := range want {
		desired[flowKey{Ifindex: ifindex, Addr: a.As16()}] = struct{}{}
	}
	// Remove stale keys — but only within this ifindex's slice of the map.
	var key flowKey
	var val uint8
	var stale []flowKey
	it := m.allowed.Iterate()
	for it.Next(&key, &val) {
		if key.Ifindex != ifindex {
			continue
		}
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
	for _, name := range m.AttachedNames() {
		m.Detach(name)
	}
	if m.coll != nil {
		m.coll.Close()
	}
	return nil
}
