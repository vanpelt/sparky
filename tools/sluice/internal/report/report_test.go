package report

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
)

type fakeResolver map[string]string // ip string -> domain

func (f fakeResolver) Label(a netip.Addr) (string, bool) {
	d, ok := f[a.Unmap().String()]
	return d, ok
}

func TestAggregate(t *testing.T) {
	flows := map[netip.Addr]Flow{
		netip.MustParseAddr("140.82.112.3"): {TxBytes: 100, RxBytes: 900, TxPkts: 2, RxPkts: 8},
		netip.MustParseAddr("140.82.112.4"): {TxBytes: 50, RxBytes: 50, TxPkts: 1, RxPkts: 1},
		netip.MustParseAddr("8.8.8.8"):      {TxBytes: 10, RxBytes: 20},
		netip.MustParseAddr("203.0.113.7"):  {TxBytes: 5, RxBytes: 5}, // unresolved
	}
	res := fakeResolver{
		"140.82.112.3": "github.com",
		"140.82.112.4": "github.com",
		"8.8.8.8":      "dns.google",
	}

	usage := Aggregate(flows, res)

	// github.com is busiest and folds two IPs together.
	if usage[0].Domain != "github.com" {
		t.Fatalf("top domain = %q, want github.com", usage[0].Domain)
	}
	gh := usage[0]
	if gh.Addresses != 2 {
		t.Errorf("github addresses = %d, want 2", gh.Addresses)
	}
	if gh.TxBytes != 150 || gh.RxBytes != 950 {
		t.Errorf("github bytes tx=%d rx=%d, want 150/950", gh.TxBytes, gh.RxBytes)
	}
	if gh.Total() != 1100 {
		t.Errorf("github total = %d, want 1100", gh.Total())
	}

	// The raw-IP flow must survive as its own unresolved bucket.
	var foundRaw bool
	for _, u := range usage {
		if u.Domain == "203.0.113.7" {
			foundRaw = true
			if u.Resolved {
				t.Error("raw IP bucket marked resolved")
			}
		}
	}
	if !foundRaw {
		t.Error("unresolved raw-IP flow was dropped")
	}
}

// A guest's IPv6 router solicitations go to ff02::2 forever and are not egress
// to anywhere. They were showing up as a destination row on the console.
func TestAggregateDropsUnnamedLinkLocal(t *testing.T) {
	flows := map[netip.Addr]Flow{
		netip.MustParseAddr("160.79.104.10"): {TxBytes: 1000},
		netip.MustParseAddr("ff02::2"):       {TxBytes: 630},
		netip.MustParseAddr("fe80::1"):       {TxBytes: 12},
		netip.MustParseAddr("169.254.1.2"):   {TxBytes: 12},
	}
	usage := Aggregate(flows, fakeResolver{})
	for _, u := range usage {
		switch u.Domain {
		case "ff02::2", "fe80::1", "169.254.1.2":
			t.Errorf("link-scope address %q reported as egress", u.Domain)
		}
	}
	if len(usage) != 1 || usage[0].Domain != "160.79.104.10" {
		t.Fatalf("usage = %+v, want just the routable unresolved address", usage)
	}
}

// ...but the gateway is link-local on some fleets and is pinned by name. A name
// somebody set on purpose outranks the heuristic.
func TestAggregateKeepsNamedLinkLocal(t *testing.T) {
	flows := map[netip.Addr]Flow{
		netip.MustParseAddr("fe80::1"): {TxBytes: 42},
	}
	usage := Aggregate(flows, fakeResolver{"fe80::1": "gateway"})
	if len(usage) != 1 || usage[0].Domain != "gateway" || !usage[0].Resolved {
		t.Fatalf("usage = %+v, want the named gateway row", usage)
	}
}

func TestAggregateSortedDescending(t *testing.T) {
	flows := map[netip.Addr]Flow{
		netip.MustParseAddr("1.1.1.1"): {RxBytes: 10},
		netip.MustParseAddr("2.2.2.2"): {RxBytes: 1000},
		netip.MustParseAddr("3.3.3.3"): {RxBytes: 100},
	}
	usage := Aggregate(flows, nil) // nil resolver ⇒ all unresolved
	var prev uint64 = ^uint64(0)
	for _, u := range usage {
		if u.Total() > prev {
			t.Fatalf("not sorted descending: %d after %d", u.Total(), prev)
		}
		prev = u.Total()
	}
}

func TestWriteTable(t *testing.T) {
	flows := map[netip.Addr]Flow{
		netip.MustParseAddr("140.82.112.3"): {TxBytes: 2048, RxBytes: 1048576},
	}
	res := fakeResolver{"140.82.112.3": "github.com"}
	var buf bytes.Buffer
	WriteTable(&buf, Aggregate(flows, res))
	out := buf.String()
	if !strings.Contains(out, "github.com") {
		t.Errorf("table missing domain:\n%s", out)
	}
	if !strings.Contains(out, "1.0MiB") {
		t.Errorf("table missing humanized download:\n%s", out)
	}
	if !strings.Contains(out, "TOTAL") {
		t.Errorf("table missing total row:\n%s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{0: "0B", 512: "512B", 1024: "1.0KiB", 1048576: "1.0MiB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
