package proxy

// Telling "your app is not listening" from "the machine your sandbox is on is
// not answering".
//
// On a single box those were the same thing, because the only thing between the
// edge and the guest was the guest. In a fleet the dial crosses another
// machine's link first, and every failure along the way used to render as one
// page: 502, "nothing is listening on port N". That page sends an owner to
// debug a program which is running perfectly well on a computer that is asleep.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

// unreachable is the error internal/fleet raises for a machine that is not
// answering, restated here by its taxonomy rather than imported: the whole
// point of the Code living in ctlops is that an edge can classify a node outage
// without importing the router.
func unreachable(sandbox, node string) error {
	return &ctlops.Error{
		Kind: ctlops.KindCapacity, Op: "restore", Code: ctlops.CodeNodeUnreachable,
		Msg:      "sandbox \"" + sandbox + "\" lives on node \"" + node + "\", which is offline",
		Details:  map[string]any{"node": node},
		Verbatim: true, Status: http.StatusServiceUnavailable,
	}
}

// failingManager answers every resume with the same error, which is how the
// EnsureRunning branch is reached.
type failingManager struct{ err error }

func (m failingManager) EnsureRunning(context.Context, string) (*host.Sandbox, error) {
	return nil, m.err
}

// refusingDialer answers every dial with err, which is how the ErrorHandler
// branch is reached: the resume succeeds and the upstream connection does not.
type refusingDialer struct{ err error }

func (d refusingDialer) dial(context.Context, string, string) (net.Conn, error) {
	return nil, d.err
}

func edgeFor(t *testing.T, mgr Resumer, dial Dialer) *Server {
	t.Helper()
	store, err := routes.Open(filepath.Join(t.TempDir(), "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Upsert(routes.Route{
		Subdomain: "demo", Sandbox: "demo", Owner: "alice", Port: 8000,
		Visibility: routes.VisibilityPublic,
	}); err != nil {
		t.Fatal(err)
	}
	s := New(mgr, store, "hivemind.tools", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if dial != nil {
		s.SetDialer(dial)
	}
	return s
}

// ask issues a request, optionally as a visitor the edge has authenticated.
func ask(s *Server, signedIn bool) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "http://demo.hivemind.tools/", nil)
	req.Host = "demo.hivemind.tools"
	if signedIn {
		// The route is public, so authorize resolves no identity of its own;
		// stamping one directly is what an authenticated request looks like by
		// the time an error page is rendered, and keeps this test about the
		// rendering rather than about the session format.
		req = req.WithContext(context.WithValue(req.Context(), identityKey,
			edgeauth.Identity{Handle: "alice"}))
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func TestUpstreamFailuresAreToldApart(t *testing.T) {
	// A guest that refused the connection inside the sandbox. The node dialed
	// it and got nowhere, which is the app's problem and the original page.
	guestRefused := &xssh.OpenChannelError{
		Reason: xssh.ConnectionFailed, Message: "nothing accepted a connection on that port in the sandbox",
	}
	// A machine that answered and refused: it has no such sandbox, or the box
	// is not running there. Between the resume above and this dial, a reaper on
	// the far machine can pause it.
	nodeRefused := &xssh.OpenChannelError{Reason: xssh.Prohibited, Message: "sandbox not running"}

	cases := []struct {
		name       string
		mgr        Resumer
		dial       Dialer
		wantCode   int
		wantPhrase string
	}{
		{
			name:       "the machine is offline, seen at resume",
			mgr:        failingManager{err: unreachable("demo", "node-b")},
			wantCode:   http.StatusServiceUnavailable,
			wantPhrase: "machine hosting this sandbox is offline",
		},
		{
			name:       "the machine is offline, seen at dial",
			mgr:        placedManager{"demo": "node-b"},
			dial:       refusingDialer{err: unreachable("demo", "node-b")}.dial,
			wantCode:   http.StatusServiceUnavailable,
			wantPhrase: "machine hosting this sandbox is offline",
		},
		{
			name:       "the machine refused the stream",
			mgr:        placedManager{"demo": "node-b"},
			dial:       refusingDialer{err: nodeRefused}.dial,
			wantCode:   http.StatusServiceUnavailable,
			wantPhrase: "isn't running on its machine",
		},
		{
			name:       "the guest refused the port",
			mgr:        placedManager{"demo": "node-b"},
			dial:       refusingDialer{err: guestRefused}.dial,
			wantCode:   http.StatusBadGateway,
			wantPhrase: "Nothing is listening on port 8000",
		},
		{
			name:       "a plain dial failure on this machine",
			mgr:        placedManager{"demo": "node-b"},
			dial:       refusingDialer{err: errors.New("connect: connection refused")}.dial,
			wantCode:   http.StatusBadGateway,
			wantPhrase: "Nothing is listening on port 8000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := edgeFor(t, tc.mgr, tc.dial)
			code, body := ask(s, false)
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", code, tc.wantCode, body)
			}
			if !strings.Contains(body, tc.wantPhrase) {
				t.Fatalf("page = %q, want it to say %q", body, tc.wantPhrase)
			}
			// Whatever went wrong, the page never spells out where the edge
			// tried to go. That is the log's job — on a public route this is
			// served to strangers.
			for _, bad := range []string{"sandbox.invalid", "127.0.0.1", "connect: connection refused"} {
				if strings.Contains(body, bad) {
					t.Errorf("page leaked %q: %s", bad, body)
				}
			}
		})
	}
}

// TestOnlyASignedInVisitorIsToldWhichMachine is the rule that makes the 503
// safe to serve on a public URL.
//
// A machine's name is fleet topology. Told to an owner it is the useful half of
// the sentence; told to anyone who typed the URL it turns every outage into a
// map of the deployment, one sandbox at a time.
func TestOnlyASignedInVisitorIsToldWhichMachine(t *testing.T) {
	for _, signedIn := range []bool{false, true} {
		name := "a stranger"
		if signedIn {
			name = "a signed-in visitor"
		}
		t.Run(name, func(t *testing.T) {
			s := edgeFor(t, failingManager{err: unreachable("demo", "node-b")}, nil)
			code, body := ask(s, signedIn)
			if code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", code, body)
			}
			if named := strings.Contains(body, "node-b"); named != signedIn {
				t.Fatalf("page names the machine = %v, want %v: %q", named, signedIn, body)
			}
		})
	}
}

// TestOnlyASignedInVisitorIsToldWhyAResumeFailed is the same rule on the other
// error page, and it covers more than a node outage: the router's sentences for
// an orphaned and a contested sandbox name a machine too, and every one of them
// used to be spliced into a page served to strangers with %v.
func TestOnlyASignedInVisitorIsToldWhyAResumeFailed(t *testing.T) {
	orphaned := &ctlops.Error{
		Kind: ctlops.KindConflict, Op: "restore", Code: "sandbox_orphaned",
		Msg:      "sandbox \"demo\" is not on node \"node-b\" any more",
		Details:  map[string]any{"node": "node-b"},
		Verbatim: true, Status: http.StatusConflict,
	}
	for _, signedIn := range []bool{false, true} {
		name := "a stranger"
		if signedIn {
			name = "a signed-in visitor"
		}
		t.Run(name, func(t *testing.T) {
			s := edgeFor(t, failingManager{err: orphaned}, nil)
			code, body := ask(s, signedIn)
			// Not a node outage, so it keeps the original page and status: the
			// sandbox genuinely did not come up.
			if code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: %s", code, body)
			}
			if told := strings.Contains(body, "node-b"); told != signedIn {
				t.Fatalf("page explains itself = %v, want %v: %q", told, signedIn, body)
			}
		})
	}
}

// TestAReasonThatNamesNoMachineIsStillExplained is the other side of the same
// rule, and it is what keeps a single-box deployment's page exactly as it was.
//
// The reasons this page has always carried — over your limit, host at capacity
// — are the visitor's own situation and name nothing anybody could not have
// worked out. Withholding them would make every busy host answer "it didn't
// come up" and stop, which is the failure this page exists to explain.
func TestAReasonThatNamesNoMachineIsStillExplained(t *testing.T) {
	atCapacity := &host.CapacityError{UsedMB: 15000, BudgetMB: 16000}
	s := edgeFor(t, failingManager{err: atCapacity}, nil)
	code, body := ask(s, false)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", code, body)
	}
	if !strings.Contains(body, atCapacity.Error()) {
		t.Fatalf("page = %q, want it to carry %q", body, atCapacity.Error())
	}
}
