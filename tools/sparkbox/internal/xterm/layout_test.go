package xterm

// Three details of the terminal page that are invisible in Go and easy to undo
// in CSS, all of them things a person reported seeing rather than things a type
// checker could have caught.
//
// The assertions are written against the MINIFIED page, because that is what
// ships: the page is minified and pre-gzipped at package init, so a rule that
// reads correctly in index.html and is mangled on the way out would pass a
// source-level check and still be broken in the browser.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func terminalPage(t *testing.T) string {
	t.Helper()
	hz := newHarness(t, newBox("demo", "alice", vmm.StateRunning))
	rec := hz.get(t, "demo-xterm."+testDomain, "/", "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("page = %d", rec.Code)
	}
	return rec.Body.String()
}

// TestTerminalKeepsItsLastRowOffTheBottomEdge.
//
// The gap has to be an INSET rather than bottom padding, and the distinction is
// the whole fix: FitAddon sizes the grid from #term's own computed height, so
// padding is space xterm still believes it may draw into. It matters because a
// fractional cell height (13px at lineHeight 1.2 = 15.6px) makes N rows render
// slightly taller than N x the floor()ed cell FitAddon divided by, and that
// accumulated remainder was landing under the bottom edge of the window.
func TestTerminalKeepsItsLastRowOffTheBottomEdge(t *testing.T) {
	page := terminalPage(t)
	if !strings.Contains(page, "#term{position:absolute;inset:0 0 10px 0;padding:8px 4px 0 10px}") {
		t.Errorf("#term no longer reserves a bottom inset; the last row can be clipped again.\nGot: %s",
			ruleFor(page, "#term"))
	}
}

// TestTerminalHasNoScrollbarGutter. xterm's viewport is a scrollback buffer,
// not a document: the wheel, trackpad and Shift-PageUp all still work without
// the gutter, and both properties are needed because Firefox reads
// scrollbar-width while WebKit and Blink read the pseudo-element.
func TestTerminalHasNoScrollbarGutter(t *testing.T) {
	page := terminalPage(t)
	for _, want := range []string{
		"scrollbar-width:none",
		"scrollbar{width:0;height:0}",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	for _, gone := range []string{"scrollbar-width:thin", "scrollbar{width:10px}"} {
		if strings.Contains(page, gone) {
			t.Errorf("the scrollbar gutter is back: %q", gone)
		}
	}
}

// TestHiveMindLinkNeedsASessionOrALiveLease is the guard itself, asserted as
// the condition rather than through a browser.
//
// A HiveMind daemon reports a presence state whenever it is installed and
// running, so "idle" with nothing in the session catalog is the resting state
// of every VM where nobody has started an agent. Keying the link on presence
// alone put a link reading "HiveMind session" in the header of such a VM that
// named no session and, having no URL, did nothing when clicked.
func TestHiveMindLinkNeedsASessionOrALiveLease(t *testing.T) {
	page := terminalPage(t)
	if !strings.Contains(page, "!v.hivemind_session_url&&!v.hivemind_active)") {
		t.Error("the session link no longer requires a session or a live lease")
	}
	if strings.Contains(page, "!title&&!v.hivemind_presence)") {
		t.Error("the link is keyed on presence again, so an idle daemon renders a dead link")
	}
}

// TestIdleDaemonWithNoSessionRendersNoLink joins the two halves of the bug.
//
// The host side was already right and already tested: an expired lease reports
// presence "idle" and omits hivemind_active, and a reading with no session
// invents no title. This is the exact payload a real VM was serving — presence
// and nothing else — and the assertion is that the page's guard, given it,
// hides the link instead of drawing one that opens nothing.
func TestIdleDaemonWithNoSessionRendersNoLink(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	fv := &fakeVitals{hivemind: map[string]*host.HiveMindLive{"demo": {
		Presence: &host.HiveMindPresence{State: "idle", ProtectUntil: &expired},
	}}}
	hz := withVitals(t, fv, runningBox("demo", "alice"))
	m := decode(t, hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"))

	if m["hivemind_presence"] != "idle" {
		t.Fatalf("hivemind_presence = %v, want the idle daemon's own word", m["hivemind_presence"])
	}
	title, _ := m["hivemind_session_title"].(string)
	url, _ := m["hivemind_session_url"].(string)
	active, _ := m["hivemind_active"].(bool)
	if title != "" || url != "" || active {
		t.Fatalf("this is no longer the payload the bug was reported against: %+v", m)
	}
	// The page's guard, spelled the same way it is spelled in index.html.
	if !(title == "" && url == "" && !active) {
		t.Error("the guard would draw a link for an idle daemon with no session")
	}
}

// ruleFor pulls one minified CSS rule out of the page so a failing assertion
// can say what the rule became rather than only that it changed.
func ruleFor(page, selector string) string {
	i := strings.Index(page, selector+"{")
	if i < 0 {
		return "(" + selector + " not found)"
	}
	j := strings.Index(page[i:], "}")
	if j < 0 {
		return page[i:min(i+120, len(page))]
	}
	return page[i : i+j+1]
}
