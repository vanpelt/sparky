//go:build linux

package firecracker

import "testing"

// TestBlockIOEngineDefaultsToAsync pins the fix for guest boots that took
// fifteen to twenty minutes on the Mac dev box.
//
// Firecracker's default block engine is Sync: one request at a time on the VMM
// thread. That is fine for bulk throughput and ruinous for a boot, which is
// thousands of SMALL reads — shared libraries for ldconfig, unit files for
// systemd, layer metadata for containerd. This device costs per REQUEST, not
// per byte. Reading the same 8 MiB out of one guest measured:
//
//	bs=4k   2048 requests   5845 ms
//	bs=64k   128 requests    177 ms
//	bs=1M      8 requests    270 ms
//
// 33x the time for identical bytes, purely because there were more requests.
// End to end that was a 2.4x faster boot (90.4s -> 37.5s) on an otherwise
// identical VM. Sync serialises those requests; io_uring keeps them in flight.
func TestBlockIOEngineDefaultsToAsync(t *testing.T) {
	var d Driver
	if got := *d.blockIOEngine(); got != "Async" {
		t.Errorf("an unconfigured driver must use io_uring, got %q", got)
	}
}

// TestBlockIOEngineHonoursSync is the escape hatch, and the reason this is a
// flag rather than a constant. Async is a developer preview upstream: its
// io_uring workers spawn in the root cgroup, so they cannot be attributed to
// one VM or capped there, and they consume PIDs. A node packing dozens of
// sandboxes must be able to go back to the old behaviour.
func TestBlockIOEngineHonoursSync(t *testing.T) {
	d := Driver{opts: Options{BlockIOEngine: "Sync"}}
	if got := *d.blockIOEngine(); got != "Sync" {
		t.Errorf("an operator asking for Sync must get it, got %q", got)
	}
}
