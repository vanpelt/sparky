package denials

import (
	"errors"
	"testing"
	"time"
)

func TestCaptureAggregatesAndRetryDoesNotReset(t *testing.T) {
	r := New()
	now := time.Unix(100, 0)
	r.now = func() time.Time { return now }
	r.Start("sbtap3", "box-id")
	r.Record("sbtap3", "Registry.NPMJS.org.", "AAAA")
	now = now.Add(time.Second)
	r.Record("sbtap3", "registry.npmjs.org", "A")
	r.Start("sbtap3", "box-id")

	got, err := r.Snapshot("sbtap3", "box-id")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Domains) != 1 || got.Domains[0].Name != "registry.npmjs.org" || got.Domains[0].Queries != 2 {
		t.Fatalf("capture = %+v", got)
	}
	if len(got.Domains[0].QTypes) != 2 || got.Domains[0].QTypes[0] != "A" || got.Domains[0].QTypes[1] != "AAAA" {
		t.Fatalf("qtypes = %v", got.Domains[0].QTypes)
	}
}

func TestNewSandboxIDReplacesCapture(t *testing.T) {
	r := New()
	r.Start("sbtap3", "old")
	r.Record("sbtap3", "old.example", "A")
	r.Start("sbtap3", "new")
	if _, err := r.Snapshot("sbtap3", "old"); !errors.Is(err, ErrCaptureMismatch) {
		t.Fatalf("old snapshot error = %v", err)
	}
	got, err := r.Snapshot("sbtap3", "new")
	if err != nil || len(got.Domains) != 0 {
		t.Fatalf("new capture = %+v, %v", got, err)
	}
}

func TestCaptureBoundsDistinctDomains(t *testing.T) {
	r := New()
	r.Start("sbtap1", "box")
	for i := 0; i < maxDomainsPerCapture+3; i++ {
		r.Record("sbtap1", string(rune(0x1000+i))+".example", "A")
	}
	got, err := r.Snapshot("sbtap1", "box")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Domains) != maxDomainsPerCapture || got.OverflowQueries != 3 {
		t.Fatalf("domains=%d overflow=%d", len(got.Domains), got.OverflowQueries)
	}
}

func TestAbandonedCaptureExpires(t *testing.T) {
	r := New()
	now := time.Unix(100, 0)
	r.now = func() time.Time { return now }
	r.Start("sbtap2", "box")
	now = now.Add(captureTTL + time.Second)
	if _, err := r.Snapshot("sbtap2", "box"); !errors.Is(err, ErrCaptureMismatch) {
		t.Fatalf("snapshot error = %v", err)
	}
}

func TestCaptureCountIsBounded(t *testing.T) {
	r := New()
	now := time.Unix(100, 0)
	r.now = func() time.Time { return now }
	r.Start("oldest", "box")
	for i := 1; i < maxCaptures; i++ {
		now = now.Add(time.Second)
		r.Start(string(rune(0x1000+i)), "box")
	}
	now = now.Add(time.Second)
	r.Start("newest", "box")
	if len(r.captures) != maxCaptures {
		t.Fatalf("captures=%d", len(r.captures))
	}
	if _, err := r.Snapshot("oldest", "box"); !errors.Is(err, ErrCaptureMismatch) {
		t.Fatalf("oldest snapshot error = %v", err)
	}
}
