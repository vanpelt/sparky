package host

// White-box test for the guest-memory rounding: evenMemMB is unexported and
// deliberately so — Create is the only caller, because the size it returns is
// the one written into b.MemMB and charged to admission.

import "testing"

// TestEvenMemMBKeepsGuestMemory2MiBAligned pins the rounding that decides
// whether KVM can use 2 MiB stage-2 blocks for a guest or has to spend a 4 KiB
// PTE on every page. Firecracker v1.16.1 mmaps guest RAM without aligning it,
// so an odd MiB size lands the mapping 1 MiB off the 2 MiB-aligned guest
// physical base and arm64 KVM sets force_pte for the whole memslot. Measured on
// the Mac dev box that is ~18x on first touch — 10725 MiB took 4.77s to touch
// 512MB where 10724 and 10726 took 0.27s.
func TestEvenMemMBKeepsGuestMemory2MiBAligned(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int64
		want int64
	}{
		// The dev box's own number: host_mem/3 = 32175/3, the size that was
		// costing every sandbox on that machine its block mappings.
		{"odd size is rounded down", 10725, 10724},
		{"already even is untouched", 10724, 10724},
		{"the built-in default is already even", DefaultMemMB, DefaultMemMB},
		{"odd default", 12289, 12288},
		// Nothing should be able to hand a VMM a zero or negative size, which
		// firecracker rejects outright. A guest that small is already broken, so
		// pass it through and let the VMM be the one to say so.
		{"tiny sizes pass through rather than becoming zero", 1, 1},
		{"two stays two", 2, 2},
		{"three rounds to two", 3, 2},
		{"zero passes through", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evenMemMB(tc.in); got != tc.want {
				t.Errorf("evenMemMB(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestEvenMemMBNeverGrowsAGuest: rounding must only ever take memory away, and
// never more than 1 MiB of it. Growing a guest would leave admission control
// charging less than the VM actually takes.
func TestEvenMemMBNeverGrowsAGuest(t *testing.T) {
	for in := int64(0); in < 4096; in++ {
		got := evenMemMB(in)
		if got > in {
			t.Fatalf("evenMemMB(%d) = %d grew the guest", in, got)
		}
		if in-got > 1 {
			t.Fatalf("evenMemMB(%d) = %d took %d MiB, want at most 1", in, got, in-got)
		}
		if got > 2 && got%2 != 0 {
			t.Fatalf("evenMemMB(%d) = %d is still odd", in, got)
		}
	}
}

// TestTurboKeepsAnAlignedGuestAligned is why SetTurbo needs no rounding of its
// own: it multiplies by TurboFactor, and an even size times two is still even.
// A future odd TurboFactor would have to round again.
func TestTurboKeepsAnAlignedGuestAligned(t *testing.T) {
	if TurboFactor%2 != 0 {
		for _, base := range []int64{10724, 12288, 2048} {
			if got := base * TurboFactor; got != evenMemMB(got) {
				t.Fatalf("TurboFactor %d turns an aligned %d MiB into an odd %d MiB; "+
					"SetTurbo needs to round", TurboFactor, base, got)
			}
		}
	}
}
