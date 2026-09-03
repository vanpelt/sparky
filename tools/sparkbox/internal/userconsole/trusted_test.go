package userconsole

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/netrules"
)

// The console's TRUSTED_DOMAINS is a hand-copied literal of
// netrules.TrustedDomains, because a browser cannot import Go. This is the only
// thing keeping the two equal.
//
// It is worth a test rather than a comment because the two copies are used at
// the two ENDS of the same gesture. The server builds a new environment's
// default egress rule-set from the Go list; this page's "add the trusted
// domains" button prefills a hand-written rule-set from the JS one. A name in
// one and not the other is a rule-set that allows what the prefill promised and
// a build that cannot resolve it, or the reverse — and either is discovered as
// a failed `npm install` twenty minutes into somebody's build, not here.
//
// Read off the EMBEDDED page rather than the file, so it is the bytes that ship
// that are compared.
func TestTheConsolesTrustedDomainsMatchTheServers(t *testing.T) {
	literal := regexp.MustCompile("(?s)var TRUSTED_DOMAINS = `(.*?)`").FindSubmatch(indexTemplate)
	if literal == nil {
		t.Fatal("the console page no longer declares a TRUSTED_DOMAINS literal; " +
			"if the prefill list moved, move this test with it")
	}
	page := strings.Fields(string(literal[1]))
	if len(page) == 0 {
		t.Fatal("the console's TRUSTED_DOMAINS list is empty")
	}
	want := slices.Clone(netrules.TrustedDomains)
	got := slices.Clone(page)
	slices.Sort(want)
	slices.Sort(got)
	if slices.Equal(want, got) {
		return
	}
	// Reported as the two differences rather than as two long lists: the whole
	// point of the failure is which name moved.
	for _, d := range got {
		if !slices.Contains(want, d) {
			t.Errorf("the console offers %q, which netrules.TrustedDomains does not allow", d)
		}
	}
	for _, d := range want {
		if !slices.Contains(got, d) {
			t.Errorf("netrules.TrustedDomains allows %q, which the console does not offer", d)
		}
	}
	if len(got) != len(want) {
		t.Errorf("console lists %d domains, server lists %d", len(got), len(want))
	}
}
