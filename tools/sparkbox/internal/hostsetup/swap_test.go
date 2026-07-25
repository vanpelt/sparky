package hostsetup

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// The exact /proc/swaps a stock Ubuntu 24.04 host shows: one 16 GiB swap.img,
// reported in KiB and one page short of a round 16 GiB because mkswap keeps the
// first page for its header. This is the file stepSwap used to be blind to.
const ubuntuSwaps = `Filename				Type		Size		Used		Priority
/swap.img                               file		16777212	1024		-2
`

func TestReadSwapAreas(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantPaths []string
		wantTotal uint64
	}{
		{
			name:      "ubuntu swap.img",
			body:      ubuntuSwaps,
			wantPaths: []string{"/swap.img"},
			wantTotal: 16777212 * 1024,
		},
		{
			name:      "no swap at all (header only)",
			body:      "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n",
			wantPaths: nil,
			wantTotal: 0,
		},
		{
			name:      "partition plus file",
			body:      ubuntuSwaps + "/dev/nvme0n1p3                          partition	4194304	0	-3\n",
			wantPaths: []string{"/swap.img", "/dev/nvme0n1p3"},
			wantTotal: 16777212*1024 + 4194304*1024,
		},
		{
			// seq_file appends " (deleted)" for an unlinked swapfile, which adds
			// a sixth field — a blind fields[2] read would take the TYPE column
			// as the size and silently report nothing.
			name:      "unlinked swapfile",
			body:      "Filename\tType\tSize\tUsed\tPriority\n/swapfile (deleted)\tfile\t1048576\t0\t-2\n",
			wantPaths: []string{"/swapfile (deleted)"},
			wantTotal: 1048576 * 1024,
		},
		{
			// And a path with a space is octal-escaped, so it stays one field.
			name:      "escaped space in the path",
			body:      "Filename\tType\tSize\tUsed\tPriority\n/mnt/big\\040disk/swap\tfile\t2097152\t0\t-2\n",
			wantPaths: []string{`/mnt/big\040disk/swap`},
			wantTotal: 2097152 * 1024,
		},
		{
			name:      "garbage rows are skipped, not fatal",
			body:      "Filename\tType\tSize\tUsed\tPriority\nnonsense\n/swap.img\tfile\tNaN\t0\t-2\n" + "/s\tfile\t1024\t0\t-2\n",
			wantPaths: []string{"/s"},
			wantTotal: 1024 * 1024,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeProbe{files: map[string]string{procSwaps: tc.body}}
			areas, err := readSwapAreas(p)
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, a := range areas {
				paths = append(paths, a.path)
			}
			if !slices.Equal(paths, tc.wantPaths) {
				t.Errorf("paths = %q, want %q", paths, tc.wantPaths)
			}
			if got := totalSwapBytes(areas); got != tc.wantTotal {
				t.Errorf("total = %d, want %d", got, tc.wantTotal)
			}
		})
	}
}

// TestStepSwapCountsExistingSwap is F4b: the probe must look at the swap the
// KERNEL has on, not at whether our own path happens to exist. Ubuntu's 16 GiB
// /swap.img was invisible to the old check, so a --swap-gb 16 host ended up with
// 32 GiB in two files and two fstab lines.
func TestStepSwapCountsExistingSwap(t *testing.T) {
	s := stepSwap()
	cases := []struct {
		name      string
		swapGB    int
		procSwaps string // "" means /proc/swaps is unreadable
		ownFile   bool   // our SwapPath already exists on disk
		wantSat   bool
		wantNote  string
		wantPlan  string
	}{
		{
			name: "disabled", swapGB: 0, procSwaps: ubuntuSwaps,
			wantSat: true, wantNote: "disabled",
		},
		{
			name: "distro swap already covers the request", swapGB: 16, procSwaps: ubuntuSwaps,
			wantSat: true, wantNote: "/swap.img",
		},
		{
			// One page short of 16 GiB is what a 16 GiB swapfile actually
			// reports; an exact >= would fail on the very file it is looking for.
			name: "exactly one mkswap header short", swapGB: 16, procSwaps: ubuntuSwaps,
			wantSat: true, wantNote: "no second swapfile needed",
		},
		{
			name: "no swap on the host", swapGB: 16,
			procSwaps: "Filename\tType\tSize\tUsed\tPriority\n",
			wantSat:   false, wantPlan: "create 16G",
		},
		{
			name:      "partial swap is topped up, not doubled",
			swapGB:    16,
			procSwaps: "Filename\tType\tSize\tUsed\tPriority\n/swap.img\tfile\t4194304\t0\t-2\n",
			wantSat:   false, wantPlan: "add 12G",
		},
		{
			name:      "our own file is live and short — reported, never resized",
			swapGB:    16,
			procSwaps: "Filename\tType\tSize\tUsed\tPriority\nSWAPPATH\tfile\t4194304\t0\t-2\n",
			wantSat:   true, wantNote: "will not resize a live swapfile",
		},
		{
			// No /proc/swaps (not Linux, or a sandbox that hides it): fall back
			// to the old check, but say that is what happened.
			name: "unreadable /proc/swaps with our file present", swapGB: 16, ownFile: true,
			wantSat: true, wantNote: "unreadable",
		},
		{
			name: "unreadable /proc/swaps with nothing on disk", swapGB: 16,
			wantSat: false, wantPlan: "create 16G",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := testEnv(t, false)
			e.Cfg.SwapGB = tc.swapGB
			if tc.procSwaps != "" {
				e.Probe = fakeProbe{files: map[string]string{
					procSwaps: strings.ReplaceAll(tc.procSwaps, "SWAPPATH", e.SwapPath),
				}}
			}
			if tc.ownFile {
				mustWrite(t, e.SwapPath, "x")
			}
			sat, note, err := s.Satisfied(e)
			if err != nil {
				t.Fatal(err)
			}
			if sat != tc.wantSat {
				t.Fatalf("satisfied = %v (note %q), want %v", sat, note, tc.wantSat)
			}
			if tc.wantNote != "" && !strings.Contains(note, tc.wantNote) {
				t.Errorf("note = %q, want it to mention %q", note, tc.wantNote)
			}
			if tc.wantPlan != "" {
				if plan := s.Plan(e); !strings.Contains(plan, tc.wantPlan) {
					t.Errorf("plan = %q, want it to mention %q", plan, tc.wantPlan)
				}
			}
		})
	}
}

// TestStepSwapApplyTopsUp proves the size Plan promises is the size dd writes,
// and that an active swapfile is never written over.
func TestStepSwapApplyTopsUp(t *testing.T) {
	t.Run("tops up the shortfall", func(t *testing.T) {
		e, _ := testEnv(t, false)
		e.Cfg.SwapGB = 16
		e.Probe = fakeProbe{files: map[string]string{
			procSwaps: "Filename\tType\tSize\tUsed\tPriority\n/swap.img\tfile\t4194304\t0\t-2\n",
		}}
		// dd must create the file, since Apply chmods it straight after.
		mustWrite(t, e.SwapPath, "")
		fr := runnerWith(map[string]string{
			"dd if=/dev/zero of=" + e.SwapPath + " bs=1M count=12288 status=none": "",
			"mkswap " + e.SwapPath: "",
			"swapon " + e.SwapPath: "",
		})
		e.Run = fr
		if err := stepSwap().Apply(e); err != nil {
			t.Fatalf("apply: %v (calls %v)", err, fr.calls)
		}
		if !slices.Contains(fr.calls, "dd if=/dev/zero of="+e.SwapPath+" bs=1M count=12288 status=none") {
			t.Errorf("expected a 12 GiB top-up, got calls %v", fr.calls)
		}
		fstab, err := os.ReadFile(e.FstabPath)
		if err != nil || !strings.Contains(string(fstab), e.SwapPath+" none swap sw 0 0") {
			t.Errorf("fstab = %q err=%v", fstab, err)
		}
	})

	t.Run("refuses to overwrite a live swapfile", func(t *testing.T) {
		e, _ := testEnv(t, false)
		e.Cfg.SwapGB = 16
		e.Probe = fakeProbe{files: map[string]string{
			procSwaps: "Filename\tType\tSize\tUsed\tPriority\n" + e.SwapPath + "\tfile\t4194304\t0\t-2\n",
		}}
		err := stepSwap().Apply(e)
		if err == nil {
			t.Fatal("dd'ing over a swapfile the kernel is paging to must be refused")
		}
		for _, want := range []string{"swapoff", e.SwapPath} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q: %v", want, err)
			}
		}
	})
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{16 << 30, "16G"},
		{16777212 * 1024, "16.0G"}, // one page short of 16 GiB, and it should say so
		{512 << 20, "512M"},
		{0, "0B"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
