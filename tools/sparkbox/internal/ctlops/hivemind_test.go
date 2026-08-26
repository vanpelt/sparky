package ctlops

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// TestSessionsBindsTheQueryToTheNamedSandbox is the property the whole design
// rests on: the caller names a box, and what reaches HiveMind is that box's own
// identity. Nothing else about the request selects a device.
func TestSessionsBindsTheQueryToTheNamedSandbox(t *testing.T) {
	r := newRig(t)
	started := time.Now().Add(-time.Hour)
	r.hivemind.snapshot = host.HiveMindSessionSnapshot{
		ObservedAt: time.Now(),
		Sessions: []host.HiveMindSession{{
			ID: "s1", Title: "Wire up presence", State: "ended",
			AgentType: "claude", StartedAt: started, LastActivityAt: started,
		}},
		TotalCount: 1,
	}

	got, err := r.ops.Sessions(context.Background(), alice(), "alicebox", 0)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if got.TotalCount != 1 || len(got.Sessions) != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
	want := r.boxes.boxes["alicebox"].ID
	if len(r.hivemind.asked) != 1 || r.hivemind.asked[0] != want {
		t.Errorf("asked about %v, want exactly [%s]", r.hivemind.asked, want)
	}
	// pageSize 0 is passed through untouched: the clamp belongs to the client,
	// which is the thing that knows the API's cap.
	if r.hivemind.pageSize != 0 {
		t.Errorf("pageSize = %d, want the caller's 0 forwarded", r.hivemind.pageSize)
	}
}

// TestSessionsReadsAPausedSandbox: a paused VM's history is exactly what
// someone is most likely asking about, and the query never touches the guest —
// so nothing here may resume it, or wanting to look would cost a boot.
func TestSessionsReadsAPausedSandbox(t *testing.T) {
	r := newRig(t)
	if err := r.boxes.Pause(context.Background(), "alicebox"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	r.calls.reset()

	if _, err := r.ops.Sessions(context.Background(), alice(), "alicebox", 10); err != nil {
		t.Fatalf("Sessions on a paused box: %v", err)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Errorf("reading sessions reached mutating store calls: %v", got)
	}
	if r.hivemind.pageSize != 10 {
		t.Errorf("pageSize = %d, want 10", r.hivemind.pageSize)
	}
}

// TestSessionsWithoutHiveMind refuses rather than returning an empty list: "we
// don't ask" and "nothing has run here" are answers a user must not have to
// tell apart.
func TestSessionsWithoutHiveMind(t *testing.T) {
	r := newRig(t)
	r.ops.hivemind = nil

	_, err := r.ops.Sessions(context.Background(), alice(), "alicebox", 0)
	if !IsKind(err, KindDisabled) {
		t.Fatalf("err = %v, want KindDisabled", err)
	}
}

// TestSessionsClassifiesAnAPIFailureAsUpstream so ctl says the SaaS is
// unreachable rather than blaming this host, and REST answers 502.
func TestSessionsClassifiesAnAPIFailureAsUpstream(t *testing.T) {
	r := newRig(t)
	r.hivemind.err = errors.New("HTTP 503: unavailable")

	_, err := r.ops.Sessions(context.Background(), alice(), "alicebox", 0)
	if !IsKind(err, KindUpstream) {
		t.Fatalf("err = %v, want KindUpstream", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.HTTPStatus() != 502 {
		t.Errorf("status = %d, want 502", e.HTTPStatus())
	}
}

// TestSessionsRefusesASandboxWithNoID: a record from before workload identity
// has no claim to bind a token to, so there is no question to ask. Saying so
// beats forwarding it and rendering HiveMind's 403.
func TestSessionsRefusesASandboxWithNoID(t *testing.T) {
	r := newRig(t)
	r.boxes.boxes["alicebox"].ID = ""

	_, err := r.ops.Sessions(context.Background(), alice(), "alicebox", 0)
	if !IsKind(err, KindInvalid) {
		t.Fatalf("err = %v, want KindInvalid", err)
	}
	if len(r.hivemind.asked) != 0 {
		t.Errorf("an unidentified sandbox still reached HiveMind: %v", r.hivemind.asked)
	}
}
