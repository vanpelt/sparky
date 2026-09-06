//go:build linux

package hoststat

import "testing"

func TestProcStatCPUTicks(t *testing.T) {
	// comm ("fire cr) acker") contains a space and a ')': fields must be
	// counted from the LAST ')'. utime=150, stime=25 (fields 14/15).
	line := "1234 (fire cr) acker) S 10 10 10 0 -1 4194560 500 0 0 0 150 25 12 3 20 0 4 0 100000 0 0"
	got, err := parseCPUTicks(line)
	if err != nil {
		t.Fatal(err)
	}
	if got != 175 {
		t.Errorf("ticks = %d, want 175", got)
	}

	for _, bad := range []string{
		"no closing paren",
		"1234 (fc) S 10 10", // too few fields after comm
	} {
		if _, err := parseCPUTicks(bad); err == nil {
			t.Errorf("parseCPUTicks(%q) accepted malformed input", bad)
		}
	}
}
