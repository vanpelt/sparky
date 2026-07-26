package machine

import (
	"regexp"
	"strconv"
	"strings"
)

// ParseVersion splits a dotted version into its numeric components, tolerating
// a suffix on the last one ("1.1.0-beta", "26.5.2"). ok is false when the
// leading component is not a number at all.
func ParseVersion(s string) (nums []int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	for _, seg := range strings.Split(s, ".") {
		// Take the leading digits; "0-beta" is 0.
		i := 0
		for i < len(seg) && seg[i] >= '0' && seg[i] <= '9' {
			i++
		}
		if i == 0 {
			break
		}
		n, err := strconv.Atoi(seg[:i])
		if err != nil {
			break
		}
		nums = append(nums, n)
	}
	return nums, len(nums) > 0
}

// VersionAtLeast reports whether have >= floor, comparing components
// NUMERICALLY with missing components read as 0.
//
// The numeric part is load-bearing rather than fussy: macOS jumped from 15 to
// 26, so a lexical or prefix comparison of "26.5.2" against a floor of "15"
// returns the wrong answer, and `uname -r` (25.5.0 on that same Mac) is the
// Darwin version, a different number entirely. Read kern.osproductversion and
// compare integers.
func VersionAtLeast(have, floor string) bool {
	h, hok := ParseVersion(have)
	f, fok := ParseVersion(floor)
	if !hok || !fok {
		return false
	}
	for i := 0; i < len(h) || i < len(f); i++ {
		var a, b int
		if i < len(h) {
			a = h[i]
		}
		if i < len(f) {
			b = f[i]
		}
		if a != b {
			return a > b
		}
	}
	return true
}

// appleSiliconRe matches the CPU brand strings Apple Silicon reports through
// machdep.cpu.brand_string ("Apple M4 Max", "Apple M1").
var appleSiliconRe = regexp.MustCompile(`^Apple M(\d+)`)

// AppleGeneration extracts the M-series generation from a CPU brand string.
// ok is false for anything that is not an Apple M<N> — an Intel Mac, or a
// string we do not recognise — and the caller must treat that as a refusal
// rather than as a pass: nested virtualization needs M3 or newer.
func AppleGeneration(brand string) (int, bool) {
	m := appleSiliconRe.FindStringSubmatch(strings.TrimSpace(brand))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}
