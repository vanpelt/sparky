package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sluice/internal/ipmap"
	"github.com/vanpelt/sparky/tools/sluice/internal/policy"
	"github.com/vanpelt/sparky/tools/sluice/internal/report"
)

type fakeMeter struct {
	byIf   map[uint32]map[netip.Addr]report.Flow
	ifaces map[uint32]string
	ready  map[string]bool
}

func (f *fakeMeter) FlowsByIface() (map[uint32]map[netip.Addr]report.Flow, error) {
	return f.byIf, nil
}
func (f *fakeMeter) Ifaces() map[uint32]string { return f.ifaces }
func (f *fakeMeter) Ready(tap string) bool     { return f.ready[tap] }

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestReportJoinsPerTapDomainsAndLabelsByVM(t *testing.T) {
	im := ipmap.New()
	im.Record("github.com", []netip.Addr{addr("140.82.112.3")}, time.Hour)
	im.Record("youtube.com", []netip.Addr{addr("142.250.72.14")}, time.Hour)

	fm := &fakeMeter{
		ifaces: map[uint32]string{7: "sbtap3", 9: "sbtap4"},
		byIf: map[uint32]map[netip.Addr]report.Flow{
			7: {addr("140.82.112.3"): {TxBytes: 100, RxBytes: 900}},  // sbtap3 -> github
			9: {addr("142.250.72.14"): {TxBytes: 10, RxBytes: 5000}}, // sbtap4 -> youtube
		},
	}
	s := New(fm, im, policy.New(nil), nil, nil)

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/report.json", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var rep Report
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Taps) != 2 {
		t.Fatalf("want 2 taps, got %d: %+v", len(rep.Taps), rep.Taps)
	}
	// Busiest tap (sbtap4, 5010 bytes) leads.
	if rep.Taps[0].Tap != "sbtap4" {
		t.Errorf("taps not sorted by bytes desc: %+v", rep.Taps)
	}
	byTap := map[string]TapUsage{}
	for _, tu := range rep.Taps {
		byTap[tu.Tap] = tu
	}
	gh := byTap["sbtap3"]
	if gh.TxBytes != 100 || gh.RxBytes != 900 {
		t.Errorf("sbtap3 totals = %d/%d, want 100/900", gh.TxBytes, gh.RxBytes)
	}
	if len(gh.Domains) != 1 || gh.Domains[0].Domain != "github.com" {
		t.Errorf("sbtap3 domains = %+v, want github.com", gh.Domains)
	}
	if !gh.Domains[0].Resolved {
		t.Error("github.com should be marked resolved")
	}
}

func TestPutPolicyReplacesTapsAndPokes(t *testing.T) {
	pol := policy.New(nil)
	poked := make(chan struct{}, 1)
	s := New(nil, ipmap.New(), pol, func() { poked <- struct{}{} }, nil)

	body := `{"taps":{"sbtap3":["github.com","*.githubusercontent.com"]}}`
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("PUT", "/policy", strings.NewReader(body)))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	pats := pol.TapPatterns("sbtap3")
	if len(pats) != 2 {
		t.Errorf("TapPatterns = %v, want 2 entries", pats)
	}
	select {
	case <-poked:
	default:
		t.Error("a policy push should poke a reconcile")
	}
}

func TestReadyTapWaitsForReconciledAttachment(t *testing.T) {
	fm := &fakeMeter{ready: map[string]bool{}}
	poked := make(chan struct{}, 1)
	s := New(fm, ipmap.New(), policy.New(nil), func() {
		fm.ready["sbtap3"] = true
		poked <- struct{}{}
	}, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/ready/sbtap3", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	select {
	case <-poked:
	default:
		t.Error("ready request did not trigger reconciliation")
	}
}

func TestPutPolicyRejectsBadPattern(t *testing.T) {
	s := New(nil, ipmap.New(), policy.New(nil), nil, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("PUT", "/policy",
		strings.NewReader(`{"taps":{"sbtap3":["*"]}}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad pattern should 400, got %d", rr.Code)
	}
}
