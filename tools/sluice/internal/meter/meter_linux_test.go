//go:build linux

package meter

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"testing"

	"github.com/cilium/ebpf"

	"github.com/vanpelt/sparky/tools/sluice/internal/report"
)

// These tests exercise the real eBPF data plane: they load the compiled object
// through the kernel verifier and drive the programs with BPF_PROG_TEST_RUN, so
// they need root and a BPF-capable kernel. They skip cleanly otherwise.

func loadOrSkip(t *testing.T) *Meter {
	t.Helper()
	m, err := Load()
	if err != nil {
		if os.Geteuid() != 0 || errors.Is(err, os.ErrPermission) {
			t.Skipf("skipping eBPF test (need root + BPF): %v", err)
		}
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// ethIPv4 builds a minimal Ethernet+IPv4(+padding) frame with the given dst/src.
func ethIPv4(t *testing.T, src, dst string) []byte {
	t.Helper()
	pkt := make([]byte, 54)
	// Ethernet: dst mac, src mac, ethertype 0x0800.
	binary.BigEndian.PutUint16(pkt[12:], 0x0800)
	// IPv4 header starts at 14.
	pkt[14] = 0x45 // version 4, ihl 5
	pkt[23] = 6    // protocol TCP
	copy(pkt[26:30], netip.MustParseAddr(src).AsSlice())
	copy(pkt[30:34], netip.MustParseAddr(dst).AsSlice())
	return pkt
}

func ethIPv6(t *testing.T, src, dst string) []byte {
	t.Helper()
	pkt := make([]byte, 74)
	binary.BigEndian.PutUint16(pkt[12:], 0x86DD)
	pkt[14] = 0x60 // version 6
	pkt[20] = 6    // next header TCP
	copy(pkt[22:38], netip.MustParseAddr(src).AsSlice())
	copy(pkt[38:54], netip.MustParseAddr(dst).AsSlice())
	return pkt
}

func run(t *testing.T, prog *ebpf.Program, pkt []byte) uint32 {
	t.Helper()
	ret, err := prog.Run(&ebpf.RunOptions{
		Data:    pkt,
		DataOut: make([]byte, len(pkt)+256),
	})
	if err != nil {
		t.Fatalf("prog.Run: %v", err)
	}
	return ret
}

const (
	tcActOK   = 0
	tcActShot = 2
)

func TestMeterAccountsIPv4(t *testing.T) {
	m := loadOrSkip(t)
	pkt := ethIPv4(t, "172.30.5.2", "140.82.112.3")

	if ret := run(t, m.fromG, pkt); ret != tcActOK {
		t.Fatalf("from_guest observe-mode ret = %d, want OK", ret)
	}
	run(t, m.fromG, pkt) // second packet accumulates

	flows, err := m.Flows()
	if err != nil {
		t.Fatal(err)
	}
	f, ok := flows[netip.MustParseAddr("140.82.112.3")]
	if !ok {
		t.Fatalf("no flow recorded for remote dst; flows=%v", flows)
	}
	if f.TxPkts != 2 {
		t.Errorf("tx_pkts = %d, want 2", f.TxPkts)
	}
	if f.TxBytes != 2*uint64(len(pkt)) {
		t.Errorf("tx_bytes = %d, want %d", f.TxBytes, 2*len(pkt))
	}
	if f.RxBytes != 0 {
		t.Errorf("rx_bytes = %d, want 0 (guest→out is tx only)", f.RxBytes)
	}
}

func TestMeterAccountsDownloadDirection(t *testing.T) {
	m := loadOrSkip(t)
	// to_guest keys on the source (the remote sender).
	pkt := ethIPv4(t, "93.184.216.34", "172.30.5.2")
	run(t, m.toG, pkt)

	flows, _ := m.Flows()
	f, ok := flows[netip.MustParseAddr("93.184.216.34")]
	if !ok {
		t.Fatalf("no rx flow for remote src; flows=%v", flows)
	}
	if f.RxPkts != 1 || f.RxBytes != uint64(len(pkt)) {
		t.Errorf("rx = %d bytes/%d pkts, want %d/1", f.RxBytes, f.RxPkts, len(pkt))
	}
}

func TestMeterAccountsIPv6(t *testing.T) {
	m := loadOrSkip(t)
	pkt := ethIPv6(t, "2606:4700::1111", "2001:4860:4860::8888")
	run(t, m.fromG, pkt)

	flows, _ := m.Flows()
	if _, ok := flows[netip.MustParseAddr("2001:4860:4860::8888")]; !ok {
		t.Fatalf("no IPv6 flow recorded; flows=%v", flows)
	}
}

// testRunIfindex discovers the ifindex BPF_PROG_TEST_RUN stamps on skb->ifindex
// (kernel-dependent, typically loopback = 1) by running a probe packet and
// reading it back from the per-iface counters. Enforcement is gated per tap, so
// tests must opt this ifindex into the enforced set to see drops.
func testRunIfindex(t *testing.T, m *Meter) uint32 {
	t.Helper()
	run(t, m.fromG, ethIPv4(t, "172.30.5.2", "198.51.100.7"))
	perIf, err := m.FlowsByIface()
	if err != nil {
		t.Fatal(err)
	}
	for idx := range perIf {
		if idx == 0 {
			t.Skip("test-run stamps ifindex 0; per-tap enforcement can't be exercised on this kernel")
		}
		return idx
	}
	t.Fatal("no ifindex observed from probe run")
	return 0
}

func TestEnforcementDropsDisallowed(t *testing.T) {
	m := loadOrSkip(t)
	dst := "203.0.113.50"
	pkt := ethIPv4(t, "172.30.5.2", dst)

	// Observe mode: never drops.
	if ret := run(t, m.fromG, pkt); ret != tcActOK {
		t.Fatalf("observe mode dropped: ret=%d", ret)
	}

	if err := m.SetEnforce(true); err != nil {
		t.Fatal(err)
	}
	// Master switch alone drops nothing until this tap is in the enforced set.
	if err := m.SetEnforceFor(testRunIfindex(t, m), true); err != nil {
		t.Fatal(err)
	}
	if ret := run(t, m.fromG, pkt); ret != tcActShot {
		t.Fatalf("enforce mode should drop disallowed dst, ret=%d", ret)
	}

	// Allow it, then it passes.
	if err := m.SyncAllowed([]netip.Addr{netip.MustParseAddr(dst)}); err != nil {
		t.Fatal(err)
	}
	if ret := run(t, m.fromG, pkt); ret != tcActOK {
		t.Fatalf("allowed dst should pass in enforce mode, ret=%d", ret)
	}

	// Downloads are never dropped even in enforce mode.
	down := ethIPv4(t, dst, "172.30.5.2")
	if ret := run(t, m.toG, down); ret != tcActOK {
		t.Fatalf("to_guest must never drop, ret=%d", ret)
	}
}

func TestSyncAllowedRemovesStale(t *testing.T) {
	m := loadOrSkip(t)
	a := netip.MustParseAddr("10.0.0.1")
	b := netip.MustParseAddr("10.0.0.2")

	if err := m.SyncAllowed([]netip.Addr{a, b}); err != nil {
		t.Fatal(err)
	}
	// Drop a; only b should remain.
	if err := m.SyncAllowed([]netip.Addr{b}); err != nil {
		t.Fatal(err)
	}
	m.SetEnforce(true)
	if err := m.SetEnforceFor(testRunIfindex(t, m), true); err != nil {
		t.Fatal(err)
	}
	if ret := run(t, m.fromG, ethIPv4(t, "172.30.5.2", "10.0.0.1")); ret != tcActShot {
		t.Errorf("stale allow entry not removed: ret=%d", ret)
	}
	if ret := run(t, m.fromG, ethIPv4(t, "172.30.5.2", "10.0.0.2")); ret != tcActOK {
		t.Errorf("surviving allow entry dropped: ret=%d", ret)
	}
}

// TestPerTapEnforcementGate proves the untagged-is-unlimited model: with the
// master switch on and a disallowed destination, a tap that is NOT in the
// enforced set passes the packet (an untagged sandbox keeps open egress), while
// the same tap, once enforced, drops it.
func TestPerTapEnforcementGate(t *testing.T) {
	m := loadOrSkip(t)
	if err := m.SetEnforce(true); err != nil {
		t.Fatal(err)
	}
	idx := testRunIfindex(t, m) // also confirms nonzero
	pkt := ethIPv4(t, "172.30.5.2", "203.0.113.99")

	// Not enforced: master on, but this tap is unlisted → pass.
	if ret := run(t, m.fromG, pkt); ret != tcActOK {
		t.Fatalf("unenforced tap must pass even a disallowed dst, ret=%d", ret)
	}

	// Enforce this tap: same disallowed dst now drops.
	if err := m.SetEnforceFor(idx, true); err != nil {
		t.Fatal(err)
	}
	if ret := run(t, m.fromG, pkt); ret != tcActShot {
		t.Fatalf("enforced tap must drop a disallowed dst, ret=%d", ret)
	}

	// Un-enforce again: back to open.
	if err := m.SetEnforceFor(idx, false); err != nil {
		t.Fatal(err)
	}
	if ret := run(t, m.fromG, pkt); ret != tcActOK {
		t.Fatalf("tap removed from enforced set must pass again, ret=%d", ret)
	}
}

func TestNonIPFramePasses(t *testing.T) {
	m := loadOrSkip(t)
	m.SetEnforce(true) // even in enforce mode, non-IP must pass
	arp := make([]byte, 42)
	binary.BigEndian.PutUint16(arp[12:], 0x0806) // ARP
	if ret := run(t, m.fromG, arp); ret != tcActOK {
		t.Errorf("ARP frame should pass, ret=%d", ret)
	}
}

// TestFlowsByIfaceMatchesFlows checks the per-tap readout folds back into the
// fleet-wide aggregate. Every packet in a single BPF_PROG_TEST_RUN carries the
// same synthetic ifindex (kernel-dependent — often loopback, not necessarily
// 0), so FlowsByIface must yield exactly one bucket equal to Flows().
func TestFlowsByIfaceMatchesFlows(t *testing.T) {
	m := loadOrSkip(t)
	run(t, m.fromG, ethIPv4(t, "172.30.5.2", "140.82.112.3"))
	run(t, m.toG, ethIPv4(t, "93.184.216.34", "172.30.5.2"))

	agg, err := m.Flows()
	if err != nil {
		t.Fatal(err)
	}
	perIf, err := m.FlowsByIface()
	if err != nil {
		t.Fatal(err)
	}
	if len(perIf) != 1 {
		t.Fatalf("a single test-run should produce one ifindex bucket, got %d: %v", len(perIf), perIf)
	}
	var per map[netip.Addr]report.Flow
	for _, v := range perIf {
		per = v
	}
	for addr, f := range agg {
		if per[addr] != f {
			t.Errorf("addr %v: Flows()=%+v FlowsByIface=%+v", addr, f, per[addr])
		}
	}
}

// allowedKeys reads the raw (ifindex, addr) entries from the allow-set map.
func allowedKeys(t *testing.T, m *Meter) map[flowKey]struct{} {
	t.Helper()
	out := map[flowKey]struct{}{}
	var k flowKey
	var v uint8
	it := m.allowed.Iterate()
	for it.Next(&k, &v) {
		out[k] = struct{}{}
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSyncAllowedForIsolatesByIfindex proves per-tap policy and the fleet-wide
// wildcard live independently in the allow-set: reconciling one ifindex never
// disturbs another's entries or the ifindex-0 wildcard.
func TestSyncAllowedForIsolatesByIfindex(t *testing.T) {
	m := loadOrSkip(t)
	wild := netip.MustParseAddr("10.0.0.1")
	tapA := netip.MustParseAddr("10.0.0.2")
	tapB := netip.MustParseAddr("10.0.0.3")

	if err := m.SyncAllowed([]netip.Addr{wild}); err != nil { // ifindex 0
		t.Fatal(err)
	}
	if err := m.SyncAllowedFor(7, []netip.Addr{tapA}); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncAllowedFor(9, []netip.Addr{tapB}); err != nil {
		t.Fatal(err)
	}
	want := map[flowKey]struct{}{
		{Ifindex: 0, Addr: wild.As16()}: {},
		{Ifindex: 7, Addr: tapA.As16()}: {},
		{Ifindex: 9, Addr: tapB.As16()}: {},
	}
	if got := allowedKeys(t, m); !mapsEqual(got, want) {
		t.Fatalf("allow-set = %v, want %v", got, want)
	}

	// Clearing tap 7 must leave the wildcard and tap 9 untouched.
	if err := m.SyncAllowedFor(7, nil); err != nil {
		t.Fatal(err)
	}
	delete(want, flowKey{Ifindex: 7, Addr: tapA.As16()})
	if got := allowedKeys(t, m); !mapsEqual(got, want) {
		t.Fatalf("after clearing tap 7, allow-set = %v, want %v", got, want)
	}
}

func mapsEqual(a, b map[flowKey]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// TestAttachToInterface proves the TCX attach path on a real link. It prefers a
// throwaway dummy device but falls back to loopback (attach+detach is transient
// and harmless) when iproute2 is unavailable.
func TestAttachToInterface(t *testing.T) {
	m := loadOrSkip(t)
	dev := "sldummy0"
	created := exec.Command("ip", "link", "add", dev, "type", "dummy").Run() == nil
	if created {
		t.Cleanup(func() { exec.Command("ip", "link", "del", dev).Run() })
	} else {
		dev = "lo" // always present; we detach immediately
	}

	attached, err := m.Attach(dev)
	if err != nil {
		t.Fatalf("Attach(%s): %v", dev, err)
	}
	if !attached {
		t.Fatal("first Attach should report attached=true")
	}
	if !m.Attached(dev) {
		t.Fatal("Attached() false after Attach")
	}
	// Second attach to the same device (same ifindex) is a no-op.
	if attached, err := m.Attach(dev); err != nil || attached {
		t.Fatalf("Attach idempotency: attached=%v err=%v", attached, err)
	}
	if names := m.AttachedNames(); len(names) != 1 || names[0] != dev {
		t.Fatalf("AttachedNames = %v, want [%s]", names, dev)
	}
	m.Detach(dev)
	if m.Attached(dev) {
		t.Fatal("Attached() true after Detach")
	}
}
