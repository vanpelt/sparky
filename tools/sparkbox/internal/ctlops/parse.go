package ctlops

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxTagsPerSandbox bounds one write's tag list. Generous for real use and
	// small enough that a runaway argument list can't write unbounded rows.
	MaxTagsPerSandbox = 32
	// MaxDiskMB is the largest disk a resize will hand out (1 TB). Not policy so
	// much as a fat-finger guard: the image is sparse, so an accidental "25000G"
	// would succeed instantly and only bite later, as a guest slowly filling a
	// disk far larger than the host.
	MaxDiskMB = 1 << 20
	// SessionTokenMaxTTL caps how long a minted edge session stays valid. Long
	// enough that re-minting isn't a chore, short enough that a leaked token
	// self-heals within a week.
	SessionTokenMaxTTL = 7 * 24 * time.Hour
	// DefaultSessionTokenTTL is what a caller who names no TTL gets.
	DefaultSessionTokenTTL = 12 * time.Hour
)

// ParseSize reads a human disk size into MiB: "25G"/"25GB"/"25g" and
// "512M"/"512MB", or a bare number, which is taken as GB because that is the
// unit anyone naming a sandbox disk is thinking in — "resize box 25" meaning
// 25 MB would be a surprising way to lose an afternoon.
//
// It lives here rather than in sshgw so the REST `size` field and `ctl resize`
// cannot drift, error text included.
func ParseSize(arg string) (int64, error) {
	t := strings.TrimSpace(strings.ToUpper(arg))
	t = strings.TrimSuffix(t, "B") // GB -> G, MB -> M
	mult := int64(1024)            // bare number: GB
	switch {
	case strings.HasSuffix(t, "G"):
		t, mult = strings.TrimSuffix(t, "G"), 1024
	case strings.HasSuffix(t, "M"):
		t, mult = strings.TrimSuffix(t, "M"), 1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q — use e.g. 25G or 512M", arg)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive, got %q", arg)
	}
	mb := n * mult
	if mb > MaxDiskMB {
		return 0, fmt.Errorf("size %q exceeds the %d GB per-sandbox limit", arg, MaxDiskMB/1024)
	}
	return mb, nil
}

// NormalizeTags lowercases, splits on commas, trims, dedupes, sorts and enforces
// MaxTagsPerSandbox. Every write path calls it, so the cap cannot be bypassed by
// a transport that does not parse flags. Idempotent, so sshgw calling it first
// costs nothing.
//
// It does NOT parse flags — `--tag` handling is CLI syntax and stays in sshgw —
// and it does not validate the tag charset either: the secrets store already
// filters tags it cannot store, and rejecting here would turn a silently-ignored
// tag into a newly-failing `ctl tags` invocation.
func NormalizeTags(in []string) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		for _, t := range strings.Split(v, ",") {
			t = strings.ToLower(strings.TrimSpace(t))
			if t == "" {
				continue
			}
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	if len(out) > MaxTagsPerSandbox {
		return nil, fmt.Errorf("too many tags (%d); limit is %d", len(out), MaxTagsPerSandbox)
	}
	if len(out) == 0 {
		return nil, nil
	}
	sort.Strings(out)
	return out, nil
}
