package hostsetup

import (
	"fmt"
	"strconv"
	"strings"
)

// procSwaps is the kernel's list of active swap areas. It is read through the
// Probe (not os.ReadFile) because it is the one path in the swap step that does
// NOT hang off Cfg.Root, so it is also the one a test cannot redirect into a
// tempdir.
const procSwaps = "/proc/swaps"

// swapSlack is how far short of the requested size the measured total may fall
// and still count.
//
// mkswap reserves the first page of a swap area for its header, so a nominal
// 16 GiB swapfile reports 16777212 KiB — 16 GiB minus one page. An exact
// `total >= want` comparison therefore fails on precisely the file this check
// exists to recognise. One page is 4 KiB on x86-64 and can be 64 KiB on arm64
// (the DGX), so the slack is a round 1 MiB rather than a page constant we would
// have to be right about per-architecture.
const swapSlack = 1 << 20

// swapArea is one row of /proc/swaps.
type swapArea struct {
	path  string
	kind  string // "file" or "partition"
	bytes uint64
}

// readSwapAreas parses /proc/swaps into the active swap areas.
//
// Two escaping details the kernel's seq_file imposes on the first column mean a
// blind 5-field split is wrong: whitespace in a path is octal-escaped (\040)
// and an unlinked swapfile gains a literal " (deleted)" suffix, which adds a
// sixth field. So the numeric columns are taken from the END of the row
// (…, Type, Size, Used, Priority) and whatever precedes them is the name.
//
// Size and Used are in KiB — 1024-byte units, not bytes and not pages.
func readSwapAreas(p Probe) ([]swapArea, error) {
	b, err := p.ReadFile(procSwaps)
	if err != nil {
		return nil, err
	}
	var out []swapArea
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] == "Filename" {
			// The header, a blank line, or something we do not understand.
			// Skipping by content rather than by index: a kernel that ever drops
			// the header would otherwise cost us the first real area.
			continue
		}
		kib, err := strconv.ParseUint(fields[len(fields)-3], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, swapArea{
			path:  strings.Join(fields[:len(fields)-4], " "),
			kind:  fields[len(fields)-4],
			bytes: kib * 1024,
		})
	}
	return out, nil
}

func totalSwapBytes(areas []swapArea) uint64 {
	var n uint64
	for _, a := range areas {
		n += a.bytes
	}
	return n
}

// describeSwap renders the areas for a Satisfied note: "/swap.img 16G".
func describeSwap(areas []swapArea) string {
	parts := make([]string, 0, len(areas))
	for _, a := range areas {
		parts = append(parts, fmt.Sprintf("%s %s", a.path, humanBytes(a.bytes)))
	}
	return strings.Join(parts, ", ")
}

// swapActive reports whether path is one of the active areas — the guard that
// keeps Apply from dd'ing over a swapfile the kernel is currently paging to.
func swapActive(areas []swapArea, path string) (swapArea, bool) {
	for _, a := range areas {
		// Compare the undecorated path too: an unlinked-and-recreated swapfile
		// is listed as "/swapfile (deleted)" and is still very much in use.
		if a.path == path || strings.TrimSuffix(a.path, " (deleted)") == path {
			return a, true
		}
	}
	return swapArea{}, false
}

// humanBytes formats a size the way an operator would say it (16G, 512M).
func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		g := float64(n) / (1 << 30)
		if g == float64(uint64(g)) {
			return fmt.Sprintf("%dG", uint64(g))
		}
		return fmt.Sprintf("%.1fG", g)
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
