package userconsole

// /api/usage is the Machines tab's footprint card. What matters about it is
// what every other endpoint here cares about — it answers for the session's
// own handle and nobody else's — plus one thing peculiar to it: the numbers
// have to come from the FLEET, because an owner's sandboxes are charged by the
// machines running them and a gateway that asked its local manager would report
// a busy account as using nothing.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func getUsage(t *testing.T, tc *testConsole, handle string) usageView {
	t.Helper()
	rec := tc.do(t, "GET", "/api/usage", handle, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/usage = %d, body %s", rec.Code, rec.Body.String())
	}
	var got usageView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestUsageReportsSharingRatherThanCopies. Two sandboxes forked from the same
// template report 13 GB between them and cost a little over 1 GB; the card
// needs both figures and the difference, and the difference is the only thing
// on the page a browser could not have worked out for itself.
func TestUsageReportsSharingRatherThanCopies(t *testing.T) {
	tc := newTestConsole(t)
	tc.handler.SetSandboxes(&fakeBoxes{
		policy: host.OwnerPolicy{DiskPoolMB: 102400, MemoryPoolMB: 8192, MemoryBurstMB: 16384},
		boxes: []*host.Sandbox{
			{Name: "a", Owner: "alice", State: vmm.StateRunning, VCPUs: 4, MemMB: 8192,
				DiskMB: 6800, BaseDiskMB: 6144, DiskTotalMB: 25600},
			{Name: "b", Owner: "alice", State: vmm.StatePaused, VCPUs: 2, MemMB: 4096,
				DiskMB: 6400, BaseDiskMB: 6144, DiskTotalMB: 25600},
		},
	})
	got := getUsage(t, tc, "alice")

	if got.RawDiskMB != 13200 {
		t.Fatalf("RawDiskMB = %d, want 13200", got.RawDiskMB)
	}
	if got.UsedDiskMB != 656+256 {
		t.Fatalf("UsedDiskMB = %d, want 912", got.UsedDiskMB)
	}
	if got.SharedDiskMB != got.RawDiskMB-got.UsedDiskMB {
		t.Fatalf("SharedDiskMB = %d, want raw-used = %d", got.SharedDiskMB, got.RawDiskMB-got.UsedDiskMB)
	}
	// The pool budgets are the other half a browser cannot derive: they are the
	// host's configuration and appear in no machine record.
	if got.DiskPoolMB != 102400 || got.MemoryPoolMB != 8192 || got.MemoryBurstMB != 16384 {
		t.Fatalf("pool budgets missing: %+v", got.OwnerCapacity)
	}
	// Only the running one holds RAM and vCPU; both hold disk.
	if got.TotalSandboxes != 2 || got.RunningSandboxes != 1 {
		t.Fatalf("counts wrong: %+v", got.OwnerCapacity)
	}
	if got.AllocatedMemMB != 8192 || got.AllocatedVCPUs != 4 {
		t.Fatalf("a paused sandbox must charge no RAM or CPU: %+v", got.OwnerCapacity)
	}
}

// TestFootprintCardSurvivesTheMinifier. The page ships minified and pre-gzipped
// at package init, and the minifier tokenises the JS rather than passing it
// through — so a card that renders in hack/preview-console.py (which serves the
// unminified source) can still be missing from the binary.
func TestFootprintCardSurvivesTheMinifier(t *testing.T) {
	tc := newTestConsole(t)
	rec := tc.do(t, "GET", "/", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{
		`id="footprint"`, `id="fp-disk"`, `id="fp-mem-sub"`, `id="fp-cpu-meter"`,
		"paintFootprint", "/api/usage",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the built page is missing %q", want)
		}
	}
}

// TestUsageIsScopedToTheSessionsOwner. The handle is the only input, so there
// is nothing to authorize past having a session — but that only holds if the
// handle really is the only input.
func TestUsageIsScopedToTheSessionsOwner(t *testing.T) {
	tc := newTestConsole(t)
	tc.handler.SetSandboxes(&fakeBoxes{boxes: []*host.Sandbox{
		{Name: "hers", Owner: "alice", State: vmm.StateRunning, VCPUs: 4, MemMB: 8192,
			DiskMB: 9000, BaseDiskMB: 6144},
		{Name: "his", Owner: "bob", State: vmm.StateRunning, VCPUs: 8, MemMB: 16384,
			DiskMB: 20000, BaseDiskMB: 6144},
	}})

	alice := getUsage(t, tc, "alice")
	if alice.Owner != "alice" || alice.TotalSandboxes != 1 || alice.RawDiskMB != 9000 {
		t.Fatalf("alice saw more than her own: %+v", alice.OwnerCapacity)
	}
	// An operator is not exempt here the way they are from the per-sandbox
	// ownership check: this endpoint reports a footprint, and the only sensible
	// footprint to report is the caller's own.
	opsy := getUsage(t, tc, "opsy")
	if opsy.TotalSandboxes != 0 || opsy.RawDiskMB != 0 {
		t.Fatalf("an operator's own footprint must be their own: %+v", opsy.OwnerCapacity)
	}
}

// TestUsageComesFromTheFleetNotTheLocalManager. The console is handed the fleet
// router by SetSandboxes precisely so reads reach other machines; a rollup that
// bypassed it would report every remote sandbox as absent.
func TestUsageComesFromTheFleetNotTheLocalManager(t *testing.T) {
	tc := newTestConsole(t)
	// A sandbox the LOCAL manager knows about, which the fleet fake does not.
	tc.create(t, "local-only", "alice")
	tc.handler.SetSandboxes(&fakeBoxes{boxes: []*host.Sandbox{
		{Name: "elsewhere", Owner: "alice", State: vmm.StateRunning, Node: "nodeb",
			VCPUs: 2, MemMB: 4096, DiskMB: 7000, BaseDiskMB: 6144},
	}})
	got := getUsage(t, tc, "alice")
	if got.TotalSandboxes != 1 || got.RawDiskMB != 7000 {
		t.Fatalf("the answer did not come from the router: %+v", got.OwnerCapacity)
	}
}

// TestUsageWithNoSandboxesIsAnEmptyRollup, not an error. The page hides the
// card on total_sandboxes == 0, so this is the shape a new account gets.
func TestUsageWithNoSandboxesIsAnEmptyRollup(t *testing.T) {
	tc := newTestConsole(t)
	got := getUsage(t, tc, "alice")
	if got.TotalSandboxes != 0 || got.UsedDiskMB != 0 || got.SharedDiskMB != 0 {
		t.Fatalf("want a zero rollup, got %+v", got.OwnerCapacity)
	}
	if got.Owner != "alice" {
		t.Fatalf("Owner = %q, want alice even with nothing to report", got.Owner)
	}
}

// TestUsageNeverReportsNegativeSharing. Disk and its baseline are sampled
// separately and a template can be replaced between them, so the subtraction
// has to be floored — a card that said "-4 GB shared" would be nonsense.
func TestUsageNeverReportsNegativeSharing(t *testing.T) {
	tc := newTestConsole(t)
	tc.handler.SetSandboxes(&fakeBoxes{boxes: []*host.Sandbox{
		{Name: "skewed", Owner: "alice", State: vmm.StateRunning, VCPUs: 2, MemMB: 4096,
			DiskMB: 5000, BaseDiskMB: 9000},
	}})
	got := getUsage(t, tc, "alice")
	if got.SharedDiskMB < 0 {
		t.Fatalf("SharedDiskMB = %d, want it floored at 0", got.SharedDiskMB)
	}
	// pooledDiskMB charges nothing at all when the baseline swallows the disk,
	// which is the pooled admission rule and stays the rule here: the whole
	// measurement is suspect, so the safe reading is "shared", not "written".
	// The page suppresses its multiplier when there is no charge to divide by.
	if got.UsedDiskMB != 0 || got.SharedDiskMB != got.RawDiskMB {
		t.Fatalf("an unusable baseline must charge 0: used %d, raw %d, shared %d",
			got.UsedDiskMB, got.RawDiskMB, got.SharedDiskMB)
	}
}
