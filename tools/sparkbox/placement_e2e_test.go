package sparkbox_test

// The same assertions, twice: once against a sandbox on this machine, once
// against one on the other machine. Nothing else differs.
//
// This is W23, and it is the milestone's definition of done rather than extra
// coverage. Every path that reaches into a guest obtains an address the same
// way — it reads box.SSHAddr or box.HostIP off a record the router handed it —
// and for a sandbox on another machine that address is the synthetic
// <sandbox>.<node>.sandbox.invalid, which resolves nowhere on purpose. So a
// path that still hands one of those fields to net.Dial is CORRECT on one
// machine and broken on two, and the only construction that can tell the
// difference is running the identical assertion under both placements.
//
// The rule this file is written to, and the reason it looks the way it does:
// no body below may differ between the two passes except the placement
// argument. There is no `if remote`, no skip, no assertion asserted more
// weakly for one pass than the other. Where the two passes genuinely have to
// name different objects — "the manager of the machine it landed on" — the
// resolution is a helper taking the placement, not a branch inside an
// assertion. A branch inside an assertion is the test lying about the property
// it claims to hold.
//
// Both passes run on the SAME rig: two machines, both joined, whichever one the
// sandbox went to. A local pass on a fleet-less harness would differ from the
// remote pass in the harness as well as the placement, and the whole claim
// would evaporate.
//
// The bodies here were moved rather than copied. e2e_test.go, proxy_test.go and
// proxy_stream_test.go each kept what has no sandbox in it — signup, invites,
// the 404 for a host nothing forwards, reserved-subdomain dispatch — and gave
// up what does. Copies drift; one body run twice cannot.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/templates"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// ---------------------------------------------------------------------------
// The parameter
// ---------------------------------------------------------------------------

// placements are the two machines the rig stands up: the gateway's own, and one
// reached only over a link. They are named rather than numbered because the
// name is literally the argument — `ssh new@gw -- --node <here>` — so a reader
// can see that the two passes differ in one word.
var placements = []string{"gw", "node-b"}

// eachPlacement runs body once per machine, on a fresh two-machine rig each
// time.
//
// A fresh rig per pass rather than one shared between them: the passes must not
// be able to depend on each other's leftovers, and a failure in the second must
// mean the second placement is broken rather than that the first one dirtied
// something. It costs a second per pass and buys an unambiguous failure.
func eachPlacement(t *testing.T, body func(t *testing.T, ds *dataStack, node string)) {
	t.Helper()
	for _, node := range placements {
		t.Run(node, func(t *testing.T) {
			body(t, newDataStack(t), node)
		})
	}
}

// mgrOn is the manager of the machine a sandbox was placed on.
//
// This is placement resolution, not a branch: an assertion body calls it with
// the pass's own argument and never learns which side it got. It exists because
// some things a test must observe — an idle reaper's decision, a machine's own
// state file — belong to one machine and cannot be asked of the other. Reaching
// for the gateway's manager unconditionally is exactly the bug this file is
// built to find, so the helper makes the honest form the short one.
func (ds *dataStack) mgrOn(node string) *host.Manager {
	if node == ds.node.name {
		return ds.node.mgr
	}
	return ds.mgr
}

// waitForState polls the ROUTER's view, which is the one every surface reads.
//
// Polling rather than reading once, in both passes identically: a lifecycle
// verb against another machine is answered by that machine and the router's
// picture is refreshed by the event that follows, not by the reply. On this
// machine the same read succeeds on the first poll. Asserting through the
// router rather than through whichever manager holds the box is deliberate —
// it is what a user, a console and the edge all see, so a placement that
// converges on the machine but never in the router is a failure this must
// catch.
func waitForState(t *testing.T, ds *dataStack, name string, want vmm.State) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%s to read as %s", name, want), func() bool {
		box, ok := ds.flt.Get(name)
		return ok && box.State == want
	})
}

// ---------------------------------------------------------------------------
// e2e_test.go's assertions
// ---------------------------------------------------------------------------

// TestEndToEnd is the original single-machine end-to-end test, run against both
// machines: a command through the gateway lands in a guest, its side effects
// survive the session, a pause really pauses, and the next connection brings the
// box back with its disk intact.
func TestEndToEnd(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ctx := context.Background()
		ds.placeOn(t, node, "demo")

		// State persists across sessions: two separate connections, and the
		// second reads what the first wrote.
		if out, errs, code := ds.session(t, ds.userKey, "demo", "echo hello-from-sandbox > marker.txt"); code != 0 {
			t.Fatalf("writing the marker exited %d: %s%s", code, out, errs)
		}
		out, errs, code := ds.session(t, ds.userKey, "demo", "cat marker.txt")
		if code != 0 {
			t.Fatalf("reading the marker exited %d: %s%s", code, out, errs)
		}
		if strings.TrimSpace(out) != "hello-from-sandbox" {
			t.Fatalf("marker roundtrip failed, got %q (stderr %q)", out, errs)
		}
		// And it really ran over there. The ledger saying where a sandbox lives
		// is one claim; the file being in that machine's own copy of the guest
		// is the other, and a create that quietly fell back to the machine
		// serving the connection would satisfy only the first.
		if got := readGuestFile(t, ds.dirOn(node), "demo", "marker.txt"); strings.TrimSpace(got) != "hello-from-sandbox" {
			t.Fatalf("the marker is not in %s's own copy of the guest: %q", node, got)
		}

		// Suspend, then resume-on-connect must bring it back with the disk
		// intact.
		if err := ds.flt.Pause(ctx, "demo"); err != nil {
			t.Fatal(err)
		}
		waitForState(t, ds, "demo", vmm.StatePaused)

		out, errs, code = ds.session(t, ds.userKey, "demo", "cat marker.txt")
		if code != 0 {
			t.Fatalf("post-resume read exited %d: %s%s", code, out, errs)
		}
		if strings.TrimSpace(out) != "hello-from-sandbox" {
			t.Fatalf("post-resume marker read failed, got %q", out)
		}
		waitForState(t, ds, "demo", vmm.StateRunning)
	})
}

// TestOwnershipIsMaskedOnEitherMachine is the masking rule, asked of a sandbox
// on each machine.
//
// A sandbox somebody else owns must not merely be refused — it must be
// indistinguishable from one that does not exist, byte for byte and exit code
// for exit code. Where it lives cannot enter into that: a fleet in which
// "somebody else's box, over there" reads differently from "no such box" is a
// fleet whose topology anyone can map by guessing names.
func TestOwnershipIsMaskedOnEitherMachine(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		// The stranger's own sandbox, built by the stranger, on this pass's
		// machine.
		_, banner, _ := ds.session(t, ds.strangerKey, sshgw.NewSandboxUser+"+theirs", "--node "+node)
		if !strings.Contains(banner, `created sandbox "theirs"`) {
			t.Fatalf("the stranger could not build a sandbox on %s: %q", node, banner)
		}

		real0, realErr, realCode := ds.session(t, ds.userKey, "theirs", "true")
		if realCode == 0 {
			t.Fatalf("another account's sandbox answered: %q", real0)
		}
		if !strings.Contains(realErr, "no sandbox named") {
			t.Fatalf("ownership failure should look like not-found, got %q", realErr)
		}

		// And the answer for a name nobody ever created. The two must be the
		// same bytes once the name the caller typed is substituted out — a
		// refusal has to quote what was asked for, and that is the ONLY thing
		// it may differ by. Anything else is an existence oracle: probe a
		// guessed name, and the shape of the answer tells you whether it hit.
		ghost0, ghostErr, ghostCode := ds.session(t, ds.userKey, "never-existed", "true")
		norm := func(s, name string) string { return strings.ReplaceAll(s, name, "<asked-for>") }
		if norm(ghostErr, "never-existed") != norm(realErr, "theirs") ||
			ghostCode != realCode || norm(ghost0, "never-existed") != norm(real0, "theirs") {
			t.Errorf("a real sandbox answers %q/%q/%d and an invented one %q/%q/%d; they must be identical",
				real0, realErr, realCode, ghost0, ghostErr, ghostCode)
		}
	})
}

// TestIdleReaperOnEitherMachine lets a machine pause its own idle sandbox and
// asks the ROUTER about it afterwards.
//
// The reaper is a machine's own housekeeping and always will be — a gateway
// cannot sample another machine's CPU. What has to work across a fleet is the
// other half: a decision one machine took alone must reach the gateway, or a
// user's box is paused and every surface still says it is running.
func TestIdleReaperOnEitherMachine(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ds.placeOn(t, node, "sleepy")
		// balloonAfter=0 disables the balloon stage; this exercises pause.
		go ds.mgrOn(node).RunReaper(ctx, 0, 50*time.Millisecond, 25*time.Millisecond)

		waitForState(t, ds, "sleepy", vmm.StatePaused)
	})
}

// ---------------------------------------------------------------------------
// proxy_test.go's assertions
// ---------------------------------------------------------------------------

// TestProxyRoutesToBackend is the edge's round trip, on either machine.
//
// The Host assertion is the one that matters most here and the one a careless
// refactor would break first: the guest must see the name the visitor typed,
// never the address the edge dialed. On this machine the difference is
// invisible; on the other, the address is a name that resolves nowhere, so a
// guest that saw it would be told to serve a host that cannot exist.
func TestProxyRoutesToBackend(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ds.placeOn(t, node, "webvm")
		ds.serveGuest(t, "webvm", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "hello-from-app host=%s", r.Host)
		}))

		code, body := ds.getEdge(t, "webvm", "")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", code, body)
		}
		if want := "hello-from-app"; !strings.HasPrefix(body, want) {
			t.Fatalf("unexpected body %q", body)
		}
		if want := "host=webvm." + proxyDomain; !strings.HasSuffix(body, want) {
			t.Fatalf("Host not preserved: %q", body)
		}
	})
}

// TestProxyResumesPausedSandbox: a visitor's request is enough to start a box
// back up, wherever it sleeps.
func TestProxyResumesPausedSandbox(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ctx := context.Background()
		ds.placeOn(t, node, "sleepy")
		ds.serveGuest(t, "sleepy", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "awake")
		}))

		if err := ds.flt.Pause(ctx, "sleepy"); err != nil {
			t.Fatal(err)
		}
		waitForState(t, ds, "sleepy", vmm.StatePaused)

		code, body := ds.getEdge(t, "sleepy", "")
		if code != http.StatusOK {
			t.Fatalf("expected 200 after resume, got %d (%s)", code, body)
		}
		waitForState(t, ds, "sleepy", vmm.StateRunning)
	})
}

// TestProxyDeadPortErrorPage is the 502 half of the old TestProxyErrorPages.
//
// The distinction it defends now costs something: a running sandbox whose port
// has no listener is the user's own program not being up, and must keep reading
// as a dead port even when the box is on a machine that answered the dial by
// refusing it on the user's behalf. The other failure — the machine itself
// being gone — is a 503 and is asserted separately, in fleet_data_e2e_test.go.
// Collapsing the two is what sends an owner off to debug a program that is
// running perfectly well.
func TestProxyDeadPortErrorPage(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ds.placeOn(t, node, "deadport")

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close() // free the port so nothing is listening
		ds.route(t, "deadport", "deadport", port, routes.VisibilityPublic)

		code, body, ct := ds.getEdgeHTML(t, "deadport")
		if code != http.StatusBadGateway {
			t.Fatalf("expected 502, got %d (%s)", code, body)
		}
		if !strings.Contains(ct, "text/html") ||
			!strings.Contains(body, fmt.Sprintf("Nothing is listening on port %d", port)) {
			t.Fatalf("expected HTML 502 naming port %d, got ct=%q body=%q", port, ct, body)
		}
	})
}

// TestDefaultRouteCreatedOnCreate: a sandbox gets its own subdomain the moment
// it exists, and loses it when destroyed.
//
// On the other machine the row cannot come from the manager that built the box,
// because a node holds no routes store — cmd/sparkbox leaves it nil there. The
// gateway mints and sweeps it instead. That is invisible from up here, which is
// the point: the same assertion has to hold either way.
func TestDefaultRouteCreatedOnCreate(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ctx := context.Background()
		ds.placeOn(t, node, "auto")

		r, ok, err := ds.routes.GetBySubdomain("auto")
		if err != nil || !ok {
			t.Fatalf("default route missing: ok=%v err=%v", ok, err)
		}
		if r.Sandbox != "auto" || r.Port != routes.DefaultPort {
			t.Fatalf("unexpected default route: %+v", r)
		}

		if err := ds.flt.Destroy(ctx, "auto"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := ds.routes.GetBySubdomain("auto"); ok {
			t.Fatal("route should be removed on destroy")
		}
	})
}

// ---------------------------------------------------------------------------
// proxy_stream_test.go's assertions
// ---------------------------------------------------------------------------

// serveSandboxOn is proxy_stream_test.go's serveSandbox with a machine named:
// build the sandbox there, start handler on a loopback port standing in for a
// service inside it, and point <name>.<domain> at that port.
//
// The edge itself is the rig's, already listening — these tests need a real
// listener rather than a ResponseRecorder because hijacking for 101 and flush
// timing only exist on a connection.
func (ds *dataStack) serveSandboxOn(t *testing.T, node, name string, h http.Handler) (edgeAddr string) {
	t.Helper()
	ds.placeOn(t, node, name)
	ds.serveGuest(t, name, h)
	return strings.TrimPrefix(ds.edge.URL, "http://")
}

// TestProxyWebsocketUpgrade drives a real 101 handshake and then raw bytes in
// both directions.
//
// Hand-rolled rather than through a websocket library on purpose: what needs
// proving is that the edge hands the connection over intact and never touches
// the bytes again, which is protocol-agnostic. On the other machine the tunnel
// underneath is an SSH channel with its own 64 KiB window and its own
// half-close, and the upgrade must survive being carried on one.
func TestProxyWebsocketUpgrade(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				http.Error(w, "not an upgrade", http.StatusBadRequest)
				return
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer conn.Close()
			fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
				"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
				"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
			io.Copy(conn, conn) //nolint:errcheck
		})
		addr := ds.serveSandboxOn(t, node, "wsvm", backend)

		c := dialEdge(t, addr)
		fmt.Fprint(c, "GET /socket HTTP/1.1\r\nHost: wsvm."+proxyDomain+"\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")

		br := bufio.NewReader(c)
		status, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read status: %v", err)
		}
		if !strings.Contains(status, "101") {
			t.Fatalf("expected 101 Switching Protocols, got %q", strings.TrimSpace(status))
		}
		for { // drain the handshake headers
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read headers: %v", err)
			}
			if strings.TrimSpace(line) == "" {
				break
			}
		}

		// Bidirectional and framing-free: two separate writes must arrive as
		// written, with the second round trip proving the tunnel stays open.
		for _, msg := range []string{"ping-one", "and-again"} {
			if _, err := io.WriteString(c, msg); err != nil {
				t.Fatalf("write %q: %v", msg, err)
			}
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(br, got); err != nil {
				t.Fatalf("read echo of %q: %v", msg, err)
			}
			if string(got) != msg {
				t.Fatalf("echo mismatch: sent %q, got %q", msg, got)
			}
		}
	})
}

// TestProxyStreamsWithoutBuffering asserts the edge forwards each write as it
// happens. The response deliberately carries a plain Content-Type and a known
// Content-Length — the case the stdlib's default flush policy does NOT stream
// eagerly — so a regression that drops FlushInterval fails here rather than
// silently stalling a user's log tail or token stream in production.
func TestProxyStreamsWithoutBuffering(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		const first, second = "first-chunk\n", "second-chunk\n"
		released := make(chan struct{}) // closed once the client has seen `first`

		backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", fmt.Sprint(len(first)+len(second)))
			io.WriteString(w, first) //nolint:errcheck
			w.(http.Flusher).Flush()
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				t.Error("client never saw the first chunk — response was buffered")
			}
			io.WriteString(w, second) //nolint:errcheck
		})
		addr := ds.serveSandboxOn(t, node, "streamvm", backend)

		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/stream", nil)
		req.Host = "streamvm." + proxyDomain
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		got := make([]byte, len(first))
		if _, err := io.ReadFull(resp.Body, got); err != nil {
			t.Fatalf("read first chunk: %v", err)
		}
		if string(got) != first {
			t.Fatalf("first chunk = %q, want %q", got, first)
		}
		close(released)

		rest, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read second chunk: %v", err)
		}
		if string(rest) != second {
			t.Fatalf("second chunk = %q, want %q", rest, second)
		}
	})
}

// TestProxyChunkedUpload covers the request direction: a body with no
// Content-Length (chunked) must reach the app whole. This is what a file upload
// or a streaming API client sends.
func TestProxyChunkedUpload(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		payload := bytes.Repeat([]byte("upload"), 200_000) // ~1.2 MB
		backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength != -1 {
				t.Errorf("upstream saw Content-Length %d, want chunked (-1)", r.ContentLength)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			fmt.Fprintf(w, "%d", len(body))
		})
		addr := ds.serveSandboxOn(t, node, "uploadvm", backend)

		// A pipe body has unknown length, so net/http sends it chunked.
		pr, pw := io.Pipe()
		go func() {
			pw.Write(payload) //nolint:errcheck
			pw.Close()
		}()
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/upload", pr)
		req.Host = "uploadvm." + proxyDomain
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, _ := io.ReadAll(resp.Body)
		if string(got) != fmt.Sprint(len(payload)) {
			t.Fatalf("upstream received %s bytes, sent %d", got, len(payload))
		}
	})
}

// TestProxyLargeBodyIntegrity round-trips several megabytes of random bytes to
// catch any truncation or corruption the flush-every-write path might
// introduce — and, on the other machine, anything the SSH channel's windowing
// might.
func TestProxyLargeBodyIntegrity(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		payload := make([]byte, 8<<20)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(payload) //nolint:errcheck
		})
		addr := ds.serveSandboxOn(t, node, "bigvm", backend)

		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/big", nil)
		req.Host = "bigvm." + proxyDomain
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("body corrupted: got %d bytes, want %d", len(got), len(payload))
		}
	})
}

// TestProxyMidStreamFailureIsNotAnErrorPage guards the ErrorHandler. Once bytes
// are on the wire the edge cannot retract them, so a failing upstream must not
// try to render a 502 over the top: the client would receive a truncated body
// with an HTML page glued onto the end, which looks like the app corrupting its
// own output. A short read is the honest outcome.
func TestProxyMidStreamFailureIsNotAnErrorPage(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		const partial = "half-a-resp"
		backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			// Promise 500 bytes, deliver a few, then vanish — a crashed app.
			fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 500\r\n\r\n")
			fmt.Fprint(conn, partial)
			conn.Close()
		})
		addr := ds.serveSandboxOn(t, node, "crashvm", backend)

		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
		req.Host = "crashvm." + proxyDomain
		req.Header.Set("Accept", "text/html") // the shape that would get an HTML page
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected the upstream's own 200 to stand, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err == nil {
			t.Errorf("expected a truncated-body error, got a clean read of %q", body)
		}
		if string(body) != partial {
			t.Fatalf("body = %q, want exactly the upstream's %q", body, partial)
		}
		if strings.Contains(string(body), "<html") || strings.Contains(string(body), "sparkbox") {
			t.Fatalf("an error page was appended to a live response: %q", body)
		}
	})
}

// ---------------------------------------------------------------------------
// The browser terminal
// ---------------------------------------------------------------------------

// Why the bridge tests are not parameterised where they live, and these exist
// instead.
//
// internal/xterm/bridge_test.go replaces the handler's PTY seam wholesale
// (h.open returns a pair of pipes), which is exactly what makes it able to
// assert the framing, the resize clamp and the close codes with no VM anywhere.
// It also means it never dials, so running it "twice" would run the identical
// code twice and prove nothing about placement — a green test asserting nothing
// is worse than no test, because it reads like coverage. Those bodies stay
// where they are, as the framing pins they were written to be.
//
// What placement can be asked of the browser terminal is the other half: the
// real dialPTY, opening a real session with the gateway's upstream key against
// a real guest, on either machine. That is what these are. The assertions are
// the bridge tests' own, re-expressed against a guest that is a shell rather
// than a pipe: output arrives, typing arrives, a window size reaches the tty, a
// shell exiting closes with 4001 carrying its status, and a pause closes with
// 4002 after the goodbye.
//
// The one bridge assertion that has no counterpart here is the resize CLAMP
// ({99999,0} → {1000,1}): the clamp happens in the handler before anything is
// sent, so a guest cannot observe the difference between clamped and refused,
// and only a fake PTY recording what it was handed can. It is asserted there
// and nowhere else, on purpose.

// terminal opens the browser terminal for name the way the page does — session
// cookie, Origin naming the terminal's own host — and drains status frames until
// the shell is ready. It returns the socket and the states it saw on the way.
func (ds *dataStack) terminal(t *testing.T, name string) (*websocket.Conn, context.Context, []string) {
	t.Helper()
	srv := ds.terminalFor(t, name)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	hdr := http.Header{}
	hdr.Set("Origin", "http://"+name+"-xterm."+proxyDomain)
	hdr.Set("Cookie", edgeauth.CookieName+"="+ds.token(t, "tester"))
	conn, _, err := websocket.Dial(ctx, "ws://"+strings.TrimPrefix(srv.URL, "http://")+"/ws",
		&websocket.DialOptions{HTTPHeader: hdr, Subprotocols: []string{"sparkbox.terminal.v1"}})
	if err != nil {
		t.Fatalf("dial the terminal socket for %s: %v", name, err)
	}
	t.Cleanup(func() { conn.CloseNow() }) //nolint:errcheck

	var states []string
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read while waiting for ready: %v", err)
		}
		if typ != websocket.MessageText {
			t.Fatalf("got a binary frame before ready: %q", data)
		}
		var m struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("status frame is not JSON: %q", data)
		}
		states = append(states, m.State)
		if m.State == "ready" {
			return conn, ctx, states
		}
	}
}

// typeLine sends one line of input, as a person pressing Enter would.
func typeLine(t *testing.T, ctx context.Context, conn *websocket.Conn, line string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte(line+"\n")); err != nil {
		t.Fatalf("typing %q: %v", line, err)
	}
}

// readUntil accumulates guest output until want appears, so a test never has to
// guess how the tty split it into frames or how much echo came with it.
func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, want string) string {
	t.Helper()
	var seen strings.Builder
	for !strings.Contains(seen.String(), want) {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("reading the guest's output waiting for %q: %v (so far %q)", want, err, seen.String())
		}
		if typ == websocket.MessageBinary {
			seen.Write(data)
		}
	}
	return seen.String()
}

// TestTerminalStreamsBothWays is bridge_test.go's first assertion against a real
// guest on either machine: what the guest prints reaches the browser as binary
// frames, and what the browser types reaches the guest.
func TestTerminalStreamsBothWays(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ds.placeOn(t, node, "termvm")
		conn, ctx, states := ds.terminal(t, "termvm")

		// A running sandbox is ready at once; there is nothing to start.
		if len(states) != 1 || states[0] != "ready" {
			t.Fatalf("states = %v, want [ready]", states)
		}

		// Browser -> guest -> browser, in one round trip. The marker is unique
		// so the shell's own echo of the command cannot be mistaken for output.
		typeLine(t, ctx, conn, "echo terminal-round-trip-ok")
		readUntil(t, ctx, conn, "terminal-round-trip-ok\r\n")
	})
}

// TestTerminalResumesAPausedSandbox: opening the terminal on a sleeping box
// starts it, and says so first.
func TestTerminalResumesAPausedSandbox(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ctx := context.Background()
		ds.placeOn(t, node, "termpaused")
		if err := ds.flt.Pause(ctx, "termpaused"); err != nil {
			t.Fatal(err)
		}
		waitForState(t, ds, "termpaused", vmm.StatePaused)

		conn, wsCtx, states := ds.terminal(t, "termpaused")
		if len(states) != 2 || states[0] != "starting" || states[1] != "ready" {
			t.Fatalf("states = %v, want [starting ready]", states)
		}
		// And it is a real shell on the far side, not merely a status frame.
		typeLine(t, wsCtx, conn, "echo resumed-and-usable")
		readUntil(t, wsCtx, conn, "resumed-and-usable\r\n")
		waitForState(t, ds, "termpaused", vmm.StateRunning)
	})
}

// TestTerminalForwardsTheWindowSize is the resize path end to end: the browser's
// window message must become a real window-change on the guest's tty, which the
// guest can read back with stty.
//
// bridge_test.go asserts what the handler HANDS the PTY. This asserts that what
// it hands it arrives — over a link, for the far machine, where the resize is a
// request on an SSH channel rather than an ioctl a few frames away.
func TestTerminalForwardsTheWindowSize(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ds.placeOn(t, node, "termsize")
		conn, ctx, _ := ds.terminal(t, "termsize")

		if err := conn.Write(ctx, websocket.MessageText,
			[]byte(`{"type":"resize","rows":40,"cols":120}`)); err != nil {
			t.Fatal(err)
		}
		// The shell reports what the kernel thinks its window is, which is only
		// what the resize set if the resize actually arrived.
		typeLine(t, ctx, conn, "stty size")
		readUntil(t, ctx, conn, "40 120")
	})
}

// TestTerminalCarriesALargePaste puts more through the tunnel in one go than an
// SSH channel's window holds, which is the case a naive copy loop deadlocks on.
//
// The paste is newline-terminated in chunks because a tty in canonical mode
// caps a single line, and a person pasting a file into an editor is pasting
// lines. The guest counts the bytes it received and writes the count where the
// test can read it, so a partial delivery is a wrong number rather than a hang.
func TestTerminalCarriesALargePaste(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ds.placeOn(t, node, "termpaste")
		conn, ctx, _ := ds.terminal(t, "termpaste")

		paste := strings.Repeat("abcdefghijklmnopqrstuvwxyz012345678901234567890123456789012\n", 8192) // ~488 KiB
		typeLine(t, ctx, conn, "stty -echo; wc -c > paste-size.txt")
		if err := conn.Write(ctx, websocket.MessageBinary, []byte(paste)); err != nil {
			t.Fatalf("write paste: %v", err)
		}
		// Ctrl-D ends wc's stdin, which is what makes it print.
		if err := conn.Write(ctx, websocket.MessageBinary, []byte{4}); err != nil {
			t.Fatalf("write EOF: %v", err)
		}
		typeLine(t, ctx, conn, "cat paste-size.txt")
		got := readUntil(t, ctx, conn, fmt.Sprint(len(paste)))
		if !strings.Contains(got, fmt.Sprint(len(paste))) {
			t.Fatalf("the guest counted a different number of bytes: %q", got)
		}
	})
}

// TestTerminalShellExitClosesWith4001 pins the close code and the exit status,
// and that the shell's last output arrives before either.
func TestTerminalShellExitClosesWith4001(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ds.placeOn(t, node, "termexit")
		conn, ctx, _ := ds.terminal(t, "termexit")

		typeLine(t, ctx, conn, "echo last-words; exit 7")

		var sawOutput bool
		exitCode := -1
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				if code := websocket.CloseStatus(err); code != 4001 {
					t.Fatalf("close status = %v, want 4001 (shell exited)", code)
				}
				break
			}
			if typ == websocket.MessageBinary {
				if strings.Contains(string(data), "last-words") {
					sawOutput = true
				}
				continue
			}
			var m struct {
				Type string `json:"type"`
				Code int    `json:"code"`
			}
			if err := json.Unmarshal(data, &m); err == nil && m.Type == "exit" {
				exitCode = m.Code
			}
		}
		if !sawOutput {
			t.Error("the shell's last output was lost")
		}
		if exitCode != 7 {
			t.Errorf("exit code = %d, want 7", exitCode)
		}
	})
}

// TestTerminalHangsUpWith4002WhenTheSandboxPauses is the live-session registry
// across a fleet.
//
// A pause has to reach every terminal attached to the box, including terminals
// held by a gateway whose manager never saw the pause because another machine
// took it. One registry, fed by the manager for a sandbox here and by the router
// for one anywhere else; two would mean half of somebody's terminals hanging
// until they noticed.
func TestTerminalHangsUpWith4002WhenTheSandboxPauses(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ds.placeOn(t, node, "termpause")
		conn, ctx, _ := ds.terminal(t, "termpause")

		// A round trip first, so the session is unambiguously established and
		// registered before anything pauses it.
		typeLine(t, ctx, conn, "echo attached")
		readUntil(t, ctx, conn, "attached\r\n")

		if err := ds.flt.Pause(context.Background(), "termpause"); err != nil {
			t.Fatal(err)
		}

		var got strings.Builder
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				if code := websocket.CloseStatus(err); code != 4002 {
					t.Fatalf("close status = %v, want 4002 (sandbox hung up); saw %q", code, got.String())
				}
				break
			}
			if typ == websocket.MessageBinary {
				got.WriteString(string(data))
			}
		}
		// The goodbye that undoes a full-screen TUI must have arrived with it.
		if !strings.Contains(got.String(), "\x1b[?1000l") {
			t.Errorf("the terminal-restore escapes never reached the browser: %q", got.String())
		}
		if !strings.Contains(got.String(), "paused") {
			t.Errorf("the goodbye never reached the browser: %q", got.String())
		}
	})
}

// The second bridge assertion with no counterpart here is
// TestClientHangUpClosesTheGuestStdin, and the reason is the same shape as the
// resize clamp's: it is not observable from inside a guest, on either machine.
//
// bridge's client-reading goroutine unwinds `defer pty.Close()` immediately
// after `defer pty.CloseWrite()`, so the guest's stdin closes and the session
// is torn down in the same breath — by design, since a tab that went away
// should not leave a shell for the reaper to find. A guest therefore never gets
// to act on the EOF and write down that it saw one; only a fake PTY, whose
// Close is a pipe close and nothing more, can distinguish "stdin was closed"
// from "everything was closed". It was tried here first, against a real shell,
// and could not be made honest: it failed identically in BOTH passes, which is
// the signature of an assertion about the harness rather than about placement.
// It stays in bridge_test.go, where the seam makes it real.

// dirOn is the state directory of the machine a sandbox was placed on. Like
// mgrOn, it resolves the pass's argument rather than branching on it.
func (ds *dataStack) dirOn(node string) string {
	if node == ds.node.name {
		return ds.node.dir
	}
	return ds.dir
}

// readGuestFile reads a file out of a mock guest's directory, treating "not
// there yet" as empty so a caller can poll for it. A mock guest's "VM" is a
// directory on the machine that built it, which is what makes "it really
// happened over there" checkable at all.
func readGuestFile(t *testing.T, stateDir, sandbox, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "mock-vms", sandbox, name))
	if err != nil {
		return ""
	}
	return string(raw)
}

// getEdgeHTML is getEdge with a browser's Accept header, for the assertions
// about which error page a person is shown.
func (ds *dataStack) getEdgeHTML(t *testing.T, name string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ds.edge.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = name + "." + proxyDomain
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := ds.edge.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", req.Host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header.Get("Content-Type")
}

// ---------------------------------------------------------------------------
// Tag templates: a binding is a placement directive
// ---------------------------------------------------------------------------

// TestTagTemplatePlacesOnTheTemplatesMachine is the only end-to-end proof of
// Part 4 of the tag-templates design, and it is here rather than in
// internal/ctlops because the property only exists on two machines.
//
// A snapshot is a file in ONE machine's image directory. Binding a tag to one
// therefore turns `--tag cuda` into a placement directive: the create must land
// where the template is, with nobody having typed --node. Both halves of that
// are easy to get wrong in opposite directions — fleet.pick short-circuits to
// the local machine whenever nothing names a node, and Candidate.Fits refuses a
// remote machine whose hello-time image listing predates the snapshot — so this
// runs under eachPlacement and asserts the same thing twice: once where "the
// template's machine" is this one, and once where it is the other.
//
// The Ops is built here rather than taken from the harness because the shared
// one is deliberately assembled without the fleet-owned stores this needs, and
// because the binding store is what is under test.
func TestTagTemplatePlacesOnTheTemplatesMachine(t *testing.T) {
	eachPlacement(t, func(t *testing.T, ds *dataStack, node string) {
		ctx := context.Background()
		store, err := templates.Open(filepath.Join(ds.dir, "sparkbox.db"), ds.log)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() }) //nolint:errcheck
		ops := ctlops.New(ctlops.Config{
			Sandboxes: ds.flt, Templates: ds.flt, Accounts: ds.users,
			Tags: ds.secrets, TemplateTags: store,
			DefaultImage: "ubuntu", Domain: proxyDomain, Log: ds.log,
		})
		t.Cleanup(ops.Close)
		who := ctlops.Caller{Handle: "tester"}

		// The template is captured on the pass's machine, through the door a
		// user would use for the create and through the control plane for the
		// capture — a node has no signer with which to reach its own guests, so
		// a remote capture is the gateway's work either way.
		ds.placeOn(t, node, "tplbox")
		snap, err := ops.CreateSnapshot(ctx, who, "tplbox", "cuda-base")
		if err != nil {
			t.Fatalf("snapshotting %s on %s: %v", "tplbox", node, err)
		}
		if snap.Node != node {
			t.Fatalf("the snapshot records node %q, want %q", snap.Node, node)
		}

		// PRE-EXISTING FLEET GAP, and the reason this bounces the link. A node
		// reports its templates only in a full inventory frame (nodelink's
		// InventoryMsg), and host.Observer carries sandbox events ONLY — there
		// is no snapshot event — so a template captured after the link came up
		// is invisible to fleet.Snapshots, fleet.templateNode and therefore to
		// `snapshot ls`, `fork` and `bind`, until the node reconnects. That is
		// not something this milestone introduced and not something it fixes;
		// it is stated here because a reader would otherwise take the reconnect
		// for harness noise. The bounce runs in BOTH passes so the two bodies
		// stay identical, which is this file's whole rule.
		ds.unplug()
		ds.unplug = ds.relink(t, ds.node)

		waitFor(t, "the gateway to see the snapshot taken on "+node, func() bool {
			list, err := ops.ListSnapshots(ctx, who)
			if err != nil {
				return false
			}
			for _, s := range list {
				if s.Name == "cuda-base" {
					return true
				}
			}
			return false
		})

		if _, err := ops.BindTemplate(ctx, who, "cuda-base", "cuda"); err != nil {
			t.Fatalf("binding cuda: %v", err)
		}

		// The whole point: no --node anywhere in this call.
		box, err := ops.Create(ctx, who, ctlops.CreateArgs{Name: "forked", Tags: []string{"cuda"}})
		if err != nil {
			t.Fatalf("creating on the bound tag with no --node: %v", err)
		}
		if box.Node != node {
			t.Errorf("the tagged create reports node %q, want the template's machine %q", box.Node, node)
		}
		// The ledger is the authority the edge, the reaper and the next restart
		// all read, so it is what the claim is asserted against.
		row, ok, err := ds.index.Get("forked")
		if err != nil || !ok {
			t.Fatalf("no ledger row for the tagged create: ok=%v err=%v", ok, err)
		}
		if row.Node != node {
			t.Errorf("the tagged sandbox landed on %q, want the template's machine %q", row.Node, node)
		}
		// And it really is the template's disk, not the stock image that a
		// silent fallback would have handed over.
		built, ok := ds.flt.Get("forked")
		if !ok {
			t.Fatal("the fleet does not know the sandbox it just created")
		}
		if built.Image == "ubuntu" || built.Image == "" {
			t.Errorf("the tagged sandbox booted image %q, want the bound template's", built.Image)
		}
	})
}
