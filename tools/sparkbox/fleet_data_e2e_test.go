package sparkbox_test

// The DATA plane against a sandbox on another machine.
//
// fleet_e2e_test.go proves the control plane: a name is placed on node-b, and
// every lifecycle verb reaches the machine that holds it. This file proves the
// other half — that a user can actually USE a sandbox that is not here. Each of
// the paths that reaches into a guest gets a test:
//
//	ssh <name>@gateway            fleet_e2e_test.go (TestFleetPlacesOnANamedNode)
//	https://<name>.<domain>       TestFleetProxyReachesARemoteGuest (+ the 101 upgrade)
//	https://<name>-xterm.<domain> TestFleetBrowserTerminalReachesARemoteGuest
//	tag-selected secrets          TestFleetSecretsReachARemoteGuest
//	scheduled jobs                TestFleetScheduledJobRunsInARemoteGuest
//
// Every one of them is the SAME code that serves a local sandbox. That is the
// property under test, and the reason these tests are worth their length: the
// gateway's record for a remote sandbox carries the synthetic address
// <sandbox>.<node>.sandbox.invalid, which resolves nowhere on purpose, so a
// path that still hands box.HostIP to net.Dial fails HERE and passes
// everywhere else. There is no other way to catch that class of bug — on one
// machine the wrong code and the right code are indistinguishable.
//
// Both machines run the mock driver and every mock guest listens on
// 127.0.0.1, so a "service inside the guest" is a loopback listener in this
// process. That is not a cheat: the node resolves the address from its own
// record and dials it itself, exactly as it would a 172.30.x.y tap address, and
// nothing above the node ever sees it.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/api"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envsync"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/proxy"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/xterm"
)

// ---------------------------------------------------------------------------
// The edge, on top of the two-machine rig
// ---------------------------------------------------------------------------

// dataStack is fleetStack plus the three HTTP surfaces that reach into a guest:
// the proxy edge, the browser-terminal handler, and the session signer that
// authenticates a visitor to both. All three are wired to the FLEET and to
// flt.DialContext, which is exactly what cmd/sparkbox does.
type dataStack struct {
	*fleetStack
	node   *nodeSide
	unplug func()
	signer *edgeauth.Signer
	px     *proxy.Server
	// edge is a real listener rather than a ResponseRecorder because the 101
	// upgrade path only exists on a connection something can hijack.
	edge *httptest.Server
	xt   *xterm.Handler
}

func newDataStack(t *testing.T) *dataStack {
	t.Helper()
	fs, node := newFleetStack(t)
	unplug := fs.join(t, node)

	signer := edgeauth.NewSigner([]byte("fleet-data-e2e-session-key"))

	px := proxy.New(fs.flt, fs.routes, proxyDomain, fs.log)
	px.SetDialer(fs.flt.DialContext)
	// No login handler: this rig never redirects a browser anywhere, it only
	// needs the edge to be able to RECOGNISE a signed-in visitor — which is
	// what decides whether a failure page may name the machine a sandbox lives
	// on. See proxy.machineFailed.
	px.SetAuth("login", nil, signer, fs.users)

	xt := xterm.New(xterm.Config{
		Sandboxes: fs.flt, Accounts: fs.users, Sessions: signer,
		UpstreamKey: fs.upstreamKey, Dial: fs.flt.DialContext,
		Domain: proxyDomain, Subdomain: "xterm",
		// The gateway owns the one live-session registry a pause closes, and a
		// browser terminal that kept its own would be silently stranded when
		// the reaper paused its sandbox. Wired here exactly as cmd/sparkbox
		// wires it, because a rig that left it nil could not tell a terminal
		// that was hung up properly from one whose SSH connection merely died
		// with the VM — the two produce different close codes and only one of
		// them explains itself to the person watching.
		Track: func(sandbox string, s xterm.SessionConn, _ bool) func() {
			return fs.gw.TrackTerminal(sandbox, s)
		},
		Log: fs.log,
	})

	edge := httptest.NewServer(px)
	t.Cleanup(edge.Close)

	return &dataStack{fleetStack: fs, node: node, unplug: unplug,
		signer: signer, px: px, edge: edge, xt: xt}
}

// placeOn builds a sandbox on the named machine through the door a user would
// use, and returns once the ledger and that machine's own state file agree.
//
// It goes through `ssh new@gateway` rather than calling the fleet directly
// because the door is where a placement is actually decided, and a harness that
// skipped it would be testing a code path no user reaches.
func (ds *dataStack) placeOn(t *testing.T, node, name string) {
	t.Helper()
	// --node is passed for BOTH placements, including the gateway's own name.
	// The two passes then differ in exactly one argument, which is the whole
	// claim this file makes: a remote sandbox is served by the same code as a
	// local one.
	_, banner, _ := ds.session(t, ds.userKey, sshgw.NewSandboxUser+"+"+name, "--node "+node)
	if !strings.Contains(banner, `created sandbox "`+name+`"`) {
		t.Fatalf("creating %q on %s: %q", name, node, banner)
	}
	row, ok, err := ds.index.Get(name)
	if err != nil || !ok {
		t.Fatalf("no ledger row for %q: ok=%v err=%v", name, ok, err)
	}
	if row.Node != node {
		t.Fatalf("%q landed on %q, want %q", name, row.Node, node)
	}
}

// serveGuest starts an HTTP listener standing in for a service inside name's
// guest and points name.<domain> at it, publicly.
func (ds *dataStack) serveGuest(t *testing.T, name string, h http.Handler) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	ds.route(t, name, name, ln.Addr().(*net.TCPAddr).Port, routes.VisibilityPublic)
}

func (ds *dataStack) route(t *testing.T, sub, sandbox string, port int, visibility string) {
	t.Helper()
	if err := ds.routes.Upsert(routes.Route{
		Subdomain: sub, Sandbox: sandbox, Owner: "tester",
		Port: port, Visibility: visibility,
	}); err != nil {
		t.Fatal(err)
	}
	// Visibility is set on INSERT and thereafter only through SetVisibility, so
	// an Upsert over the default row a create already minted keeps that row's
	// visibility. Saying it twice is how a test gets the row it asked for
	// whether or not one was there.
	if err := ds.routes.SetVisibility(sub, visibility); err != nil {
		t.Fatal(err)
	}
}

// getEdge issues a request to the edge for <name>.<domain>. token, when
// non-empty, is presented as the bearer an authenticated visitor would carry.
func (ds *dataStack) getEdge(t *testing.T, name, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ds.edge.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Host, not the URL: the edge routes on the name a browser asked for, and
	// the connection goes to whatever ephemeral port httptest picked.
	req.Host = name + "." + proxyDomain
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ds.edge.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", req.Host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (ds *dataStack) token(t *testing.T, handle string) string {
	t.Helper()
	tok, _, err := ds.signer.Mint(edgeauth.Identity{Handle: handle}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// terminalFor fronts the browser-terminal handler with a listener that presents
// the host that sandbox's page would be served from. The handler routes on the
// "<name>-xterm" label, and a test client cannot set a Host header through a
// WebSocket dial — the rewrite is the same one internal/xterm's own harness
// makes, for the same reason.
func (ds *dataStack) terminalFor(t *testing.T, name string) *httptest.Server {
	t.Helper()
	host := name + "-xterm." + proxyDomain
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = host
		ds.xt.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startScheduler runs a scheduler for the rest of the test and JOINS it on the
// way out.
//
// The join is the point. A scheduled job runs on its own goroutine, resumes a
// sandbox and execs into it; letting the test end while one is still going
// means a job writing files into a machine's state directory at the same moment
// t.TempDir is removing that directory — a cleanup failure attributed to
// whichever test happened to be unlucky, with nothing in it to point at the
// scheduler. Scheduler.Run waits for its own jobs once its context is
// cancelled, so cancelling and then waiting for Run is enough.
//
// It ticks every 10ms so the test does not wait out a real cron minute; the
// machinery under the tick is the same either way.
func startScheduler(t *testing.T, ds *dataStack) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		schedule.NewScheduler(ds.schedules, ds.gw, ds.log).Run(ctx, 10*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// guestFile reads a file from inside a mock guest. The mock driver's "VM" is a
// directory, which is what makes "the secret actually arrived in the guest"
// checkable at all.
func guestFile(t *testing.T, stateDir, sandbox, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "mock-vms", sandbox, name))
	if err != nil {
		t.Fatalf("reading %s from %s's guest: %v", name, sandbox, err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// https://<name>.<domain>
// ---------------------------------------------------------------------------

// TestFleetProxyReachesARemoteGuest is the HTTP edge against a sandbox on the
// other machine, in both the ordinary and the upgraded shape.
//
// The Host assertion is the one that would survive a careless refactor least:
// the guest must see the name the visitor typed, never the synthetic
// <sandbox>.<node>.sandbox.invalid the edge dialed. A sandbox is not supposed to
// be able to tell which machine it is on.
func TestFleetProxyReachesARemoteGuest(t *testing.T) {
	ds := newDataStack(t)

	for _, node := range []string{"gw", "node-b"} {
		t.Run(node, func(t *testing.T) {
			name := "web-" + strings.ReplaceAll(node, "-", "")
			ds.placeOn(t, node, name)
			ds.serveGuest(t, name, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "served-by-the-guest host=%s", r.Host)
			}))

			code, body := ds.getEdge(t, name, "")
			if code != http.StatusOK {
				t.Fatalf("GET %s.%s = %d: %s", name, proxyDomain, code, body)
			}
			if !strings.Contains(body, "served-by-the-guest") {
				t.Fatalf("body = %q, want the guest's own answer", body)
			}
			if want := "host=" + name + "." + proxyDomain; !strings.Contains(body, want) {
				t.Fatalf("the guest saw %q, want %q — the edge must not leak the address it dialed", body, want)
			}
		})
	}
}

// TestFleetProxyUpgradesAWebsocketToARemoteGuest drives a real 101 handshake
// and then raw bytes both ways.
//
// The upgrade is a separate test because it is a separate mechanism: the edge
// hijacks the client connection and hands it to the transport's, so for a
// remote sandbox the tunnel that carries it is an SSH channel with a 64 KiB
// window and its own half-close. A hand-rolled handshake rather than a
// websocket library, because what is under test is that the edge stops touching
// the bytes — which is protocol-agnostic.
func TestFleetProxyUpgradesAWebsocketToARemoteGuest(t *testing.T) {
	ds := newDataStack(t)
	ds.placeOn(t, "node-b", "wsfar")
	ds.serveGuest(t, "wsfar", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack in the guest: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		io.Copy(conn, conn) //nolint:errcheck
	}))

	c := dialEdge(t, strings.TrimPrefix(ds.edge.URL, "http://"))
	fmt.Fprint(c, "GET /socket HTTP/1.1\r\nHost: wsfar."+proxyDomain+"\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")

	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("upgrade through the tunnel = %q, want 101", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	// Two round trips, because one proves the handshake and two prove the
	// tunnel stayed open across the SSH channel's own flow control.
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
}

// TestFleetEdgeSaysTheMachineIsOfflineWithoutNamingIt is the error page W22
// adds, and the rule attached to it.
//
// An offline machine used to render as "nothing is listening on port N" — the
// page that sends an owner off to debug a program which is running perfectly
// well on a computer that is asleep. It is a 503 now. And the node's NAME is
// fleet topology: an owner is told it, and a stranger who merely typed a public
// URL is not, because otherwise every outage becomes a free map of the
// deployment.
func TestFleetEdgeSaysTheMachineIsOfflineWithoutNamingIt(t *testing.T) {
	ds := newDataStack(t)
	ds.placeOn(t, "node-b", "gone")
	ds.serveGuest(t, "gone", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "never reached")
	}))
	// A second subdomain onto the same sandbox, private this time. The edge
	// only resolves an identity for a route that is gated — a public URL is
	// served openly and carries no visitor — so "the owner, signed in" is a
	// question that can only be asked of a private route.
	ds.route(t, "goneown", "gone", 9, routes.VisibilityPrivate)

	// Unplug the machine and wait for the fleet to stop counting it as online:
	// a link that has only just dropped is still inside its grace, and the
	// answer during grace is a dial that fails, not a refusal.
	ds.unplugNode(t)

	cases := []struct {
		name      string
		host      string
		token     string
		wantNamed bool
	}{
		{name: "a stranger who typed the URL", host: "gone", token: "", wantNamed: false},
		{name: "the owner, signed in", host: "goneown", token: ds.token(t, "tester"), wantNamed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := ds.getEdge(t, tc.host, tc.token)
			if code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", code, body)
			}
			if strings.Contains(body, "Nothing is listening") {
				t.Errorf("an offline machine rendered as a dead port: %q", body)
			}
			if named := strings.Contains(body, "node-b"); named != tc.wantNamed {
				t.Errorf("page names the machine = %v, want %v: %q", named, tc.wantNamed, body)
			}
			// Whoever is reading, the address the edge tried is never on the
			// page. It is in the log and nowhere else.
			if strings.Contains(body, "sandbox.invalid") || strings.Contains(body, "127.0.0.1") {
				t.Errorf("the page spells out an address: %q", body)
			}
		})
	}
}

// unplugNode drops node-b's link and waits until the fleet has stopped counting
// it as online, so a test that follows is asking about a machine the router
// knows is gone rather than one it is still inside the grace period for.
func (ds *dataStack) unplugNode(t *testing.T) {
	t.Helper()
	ds.unplug()
	waitFor(t, "the fleet to see node-b go offline", func() bool { return !ds.flt.Online("node-b") })
}

// ---------------------------------------------------------------------------
// https://<name>-xterm.<domain>
// ---------------------------------------------------------------------------

// TestFleetBrowserTerminalReachesARemoteGuest opens the browser terminal
// against a sandbox on the other machine and types into it.
//
// internal/xterm's own bridge tests replace the PTY seam wholesale, so they
// prove the framing and prove nothing about reaching anything. This is the one
// that runs the real dialPTY: a session opened with the gateway's upstream key
// over a stream the node re-resolved from its own record.
func TestFleetBrowserTerminalReachesARemoteGuest(t *testing.T) {
	ds := newDataStack(t)

	for _, node := range []string{"gw", "node-b"} {
		t.Run(node, func(t *testing.T) {
			name := "term-" + strings.ReplaceAll(node, "-", "")
			ds.placeOn(t, node, name)
			srv := ds.terminalFor(t, name)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			hdr := http.Header{}
			hdr.Set("Origin", "http://"+name+"-xterm."+proxyDomain)
			hdr.Set("Cookie", edgeauth.CookieName+"="+ds.token(t, "tester"))
			conn, _, err := websocket.Dial(ctx,
				"ws://"+strings.TrimPrefix(srv.URL, "http://")+"/ws",
				&websocket.DialOptions{HTTPHeader: hdr, Subprotocols: []string{"sparkbox.terminal.v1"}})
			if err != nil {
				t.Fatalf("dial the terminal socket: %v", err)
			}
			defer conn.CloseNow() //nolint:errcheck

			// Status frames until "ready" — which for a running sandbox is the
			// first one, and which cannot arrive at all unless the PTY opened.
			for {
				typ, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatalf("waiting for the terminal to be ready: %v", err)
				}
				if typ != websocket.MessageBinary {
					var m struct {
						State string `json:"state"`
					}
					if err := json.Unmarshal(data, &m); err != nil {
						t.Fatalf("status frame is not JSON: %q", data)
					}
					if m.State == "ready" {
						break
					}
					continue
				}
				// Output before ready would mean the handler wrote guest bytes
				// without ever announcing the terminal; nothing does that.
				t.Fatalf("binary frame before ready: %q", data)
			}

			// A real shell on the other side of a real tunnel: type a command
			// and read its output back. The marker is unique so nothing in the
			// shell's own prompt can be mistaken for it.
			marker := "shell-on-" + node
			if err := conn.Write(ctx, websocket.MessageBinary, []byte("echo "+marker+"\n")); err != nil {
				t.Fatalf("type into the terminal: %v", err)
			}
			var seen strings.Builder
			for !strings.Contains(seen.String(), marker+"\r\n") {
				typ, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatalf("reading the guest's output: %v (so far %q)", err, seen.String())
				}
				if typ == websocket.MessageBinary {
					seen.Write(data)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------

// TestFleetSecretsReachARemoteGuest is the one W22 requirement no amount of
// dialer wiring satisfies on its own.
//
// A node has NO secrets store — cmd/sparkbox opens none in node mode and never
// installs the push hook, deliberately, because an owner's decrypted secrets
// must not sit on every machine that happens to run one of their sandboxes. So
// the manager's own propagation channel is a no-op over there, and everything
// that used to relay a resync into it was relaying into nothing. The gateway
// pushes instead, dialing the guest through the fleet.
//
// Three moments are asserted, because they are three different mechanisms:
// create (the gateway made it run), a tag change (the owner changed what it is
// entitled to), and a resume (the gateway made it run again, later).
func TestFleetSecretsReachARemoteGuest(t *testing.T) {
	ds := newDataStack(t)
	ctx := context.Background()

	if err := ds.secrets.PutSecret("tester", "API_TOKEN", "sekret-value", []string{"web"}); err != nil {
		t.Fatal(err)
	}

	for _, node := range []string{"gw", "node-b"} {
		t.Run(node, func(t *testing.T) {
			name := "sec-" + strings.ReplaceAll(node, "-", "")
			stateDir := ds.dir
			if node == "node-b" {
				stateDir = ds.node.dir
			}
			ds.placeOn(t, node, name)

			// Created untagged: the block exists and is empty. This is the
			// assertion that says the delivery channel is live rather than that
			// nothing was tried — an empty file would pass a "no secret leaked"
			// check without ever having reached the guest.
			waitFor(t, name+" to receive its (empty) managed block", func() bool {
				return strings.Contains(readGuestEnv(t, stateDir, name), envsync.BlockBegin)
			})
			if got := readGuestEnv(t, stateDir, name); strings.Contains(got, "API_TOKEN") {
				t.Fatalf("an untagged sandbox was given a tagged secret: %q", got)
			}

			// Tagging it is what selects the secret, and `ctl tags` is how an
			// owner does it. On the remote path this used to relay a resync
			// into a hook that does not exist over there.
			if out, errs, code := ds.ctl(t, "tags "+name+" web"); code != 0 {
				t.Fatalf("ctl tags: exit %d (%s%s)", code, out, errs)
			}
			waitFor(t, "the tagged secret to reach "+name, func() bool {
				return strings.Contains(readGuestEnv(t, stateDir, name), `API_TOKEN="sekret-value"`)
			})

			// And a resume re-pushes, so a box that was paused while a secret
			// changed is corrected on its next start rather than carrying a
			// stale environment forever.
			if err := ds.flt.Pause(ctx, name); err != nil {
				t.Fatalf("pause: %v", err)
			}
			if err := os.Remove(filepath.Join(stateDir, "mock-vms", name, "environment")); err != nil {
				t.Fatalf("clearing the guest's env file: %v", err)
			}
			if _, err := ds.flt.EnsureReady(ctx, name); err != nil {
				t.Fatalf("resume: %v", err)
			}
			waitFor(t, "a resume to re-push "+name+"'s environment", func() bool {
				raw, err := os.ReadFile(filepath.Join(stateDir, "mock-vms", name, "environment"))
				return err == nil && strings.Contains(string(raw), `API_TOKEN="sekret-value"`)
			})
		})
	}
}

// readGuestEnv reads the managed env file, treating "not there yet" as empty so
// a caller can poll for it.
func readGuestEnv(t *testing.T, stateDir, sandbox string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "mock-vms", sandbox, "environment"))
	if err != nil {
		return ""
	}
	return string(raw)
}

// TestFleetSecretsFollowTheOwnerTheLedgerRecords is the security half of the
// push, and it is worth its own test because getting it wrong is silent.
//
// The record a node hands back carries the owner the NODE claims — EnsureRunning
// does not even overwrite it, since resume is not an ownership-changing
// operation. Selecting secrets on that string would let any machine in the
// fleet name a handle and be sent that user's decrypted values. The ledger's
// owner column is the only authorization input everywhere else in the router,
// and the push uses it too.
func TestFleetSecretsFollowTheOwnerTheLedgerRecords(t *testing.T) {
	ds := newDataStack(t)

	// A secret belonging to somebody else, selected by a tag the sandbox has.
	if err := ds.secrets.PutSecret("stranger", "STRANGERS_KEY", "not-yours", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if err := ds.secrets.PutSecret("tester", "MY_KEY", "mine", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	ds.placeOn(t, "node-b", "owned")
	if out, errs, code := ds.ctl(t, "tags owned web"); code != 0 {
		t.Fatalf("ctl tags: exit %d (%s%s)", code, out, errs)
	}
	waitFor(t, "the owner's secret to reach the guest", func() bool {
		return strings.Contains(readGuestEnv(t, ds.node.dir, "owned"), `MY_KEY="mine"`)
	})
	if got := readGuestEnv(t, ds.node.dir, "owned"); strings.Contains(got, "STRANGERS_KEY") {
		t.Fatalf("a sandbox received another account's secret: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Scheduled jobs
// ---------------------------------------------------------------------------

// TestFleetScheduledJobRunsInARemoteGuest fires a cron job inside a sandbox on
// the other machine and checks that its exit code came back.
//
// Scheduled work is the path with no user attached: nobody is watching, so a
// job that silently stopped running when a sandbox was placed elsewhere would
// be noticed weeks later. The runner is the SSH gateway's own exec path, which
// is why it is fixed by the same dialer as an interactive session.
func TestFleetScheduledJobRunsInARemoteGuest(t *testing.T) {
	ds := newDataStack(t)
	ds.placeOn(t, "node-b", "cronbox")

	entry, err := ds.schedules.Add(schedule.Entry{
		Sandbox: "cronbox", Owner: "tester", Spec: "@every 1s",
		Command: "printf ran-in-the-guest > ran.txt; exit 7",
	})
	if err != nil {
		t.Fatal(err)
	}
	startScheduler(t, ds)

	waitFor(t, "the scheduled job to run and be recorded", func() bool {
		e, err := ds.schedules.Get(entry.ID)
		return err == nil && !e.LastRun.IsZero()
	})
	e, err := ds.schedules.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e.LastExit != 7 {
		t.Errorf("recorded exit %d, want 7 — the job's own status must survive the tunnel (err=%q)", e.LastExit, e.LastError)
	}
	// It ran INSIDE the guest, on the other machine, and not anywhere here.
	if got := guestFile(t, ds.node.dir, "cronbox", "ran.txt"); got != "ran-in-the-guest" {
		t.Errorf("the guest's file says %q", got)
	}
}

// TestFleetScheduledJobErrorNamesNoAddress is the leak this path used to have.
//
// RunInSandbox's failure is stored in the job's last_error column and rendered
// by `ctl schedule list` and by both consoles, so an address that reaches it is
// persisted rather than glimpsed. For a sandbox on a machine that is not
// answering the stored sentence must be the router's — which names the machine,
// for the owner, and no address at all.
func TestFleetScheduledJobErrorNamesNoAddress(t *testing.T) {
	ds := newDataStack(t)
	ds.placeOn(t, "node-b", "cronoff")
	entry, err := ds.schedules.Add(schedule.Entry{
		Sandbox: "cronoff", Owner: "tester", Spec: "@every 1s", Command: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	ds.unplugNode(t)

	startScheduler(t, ds)
	waitFor(t, "the scheduled job to fail and be recorded", func() bool {
		e, err := ds.schedules.Get(entry.ID)
		return err == nil && e.LastError != ""
	})
	e, err := ds.schedules.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.LastError, "node-b") {
		t.Errorf("last_error = %q, want the machine named for its owner", e.LastError)
	}
	for _, bad := range []string{"sandbox.invalid", "127.0.0.1", "dial tcp"} {
		if strings.Contains(e.LastError, bad) {
			t.Errorf("last_error = %q, which spells out %q", e.LastError, bad)
		}
	}
}

// ---------------------------------------------------------------------------
// ssh <name>@gateway, when the machine is not there
// ---------------------------------------------------------------------------

// TestFleetSshSaysTheMachineIsOfflineAndNothingElse pins what a person sees at
// a terminal when their sandbox's machine is gone.
//
// Two things are being asserted at once. The sentence is the router's, printed
// as it stands rather than wrapped in "dial vm failed: vm ssh not reachable:
// …", because the error already says everything there is to say. And nothing
// the dialer composed reaches the terminal at all — the address it tried is not
// a fact a user can act on, and for a remote sandbox it is a name that resolves
// nowhere by design.
func TestFleetSshSaysTheMachineIsOfflineAndNothingElse(t *testing.T) {
	ds := newDataStack(t)
	ds.placeOn(t, "node-b", "quiet")
	ds.unplugNode(t)

	out, errs, code := ds.session(t, ds.userKey, "quiet", "echo hi")
	if code != 1 {
		t.Fatalf("exit %d, want 1 (stdout %q, stderr %q)", code, out, errs)
	}
	want := "sparkbox: sandbox \"quiet\" lives on node \"node-b\", which is offline\r\n"
	if errs != want {
		t.Errorf("stderr = %q, want exactly %q", errs, want)
	}
}

// ---------------------------------------------------------------------------
// Nothing hands out an address
// ---------------------------------------------------------------------------

// TestFleetNoSurfaceHandsOutAGuestAddress sweeps the read surfaces for the
// three address fields.
//
// The point is not that any single one of them is dangerous on its own — it is
// that every machine in a fleet mints its guests the same 172.30.<idx>.2, so an
// address is never a fleet-wide name, and the synthetic form the gateway
// substitutes additionally says which machine holds whose work. They are
// dropped at the projection layer (host.Sandbox.Public, ctlops.info) rather
// than at each transport, so this test is really asking whether every surface
// still goes through one.
func TestFleetNoSurfaceHandsOutAGuestAddress(t *testing.T) {
	ds := newDataStack(t)
	ds.placeOn(t, "node-b", "shy")

	// The gateway's own record does carry the synthetic address — it has to,
	// since it is what the fleet dialer resolves — so the test is meaningful
	// only if that is true first.
	box, ok := ds.flt.Get("shy")
	if !ok {
		t.Fatal("the fleet lost the sandbox")
	}
	if !strings.HasSuffix(box.HostIP, ".sandbox.invalid") {
		t.Fatalf("HostIP = %q, want the synthetic fleet name", box.HostIP)
	}
	if box.State != vmm.StateRunning {
		t.Fatalf("state = %q, want running", box.State)
	}

	// And the projection drops all three.
	pub := box.Public()
	if pub.HostIP != "" || pub.SSHAddr != "" || pub.GuestV6 != "" {
		t.Errorf("Public() kept an address: %+v", pub)
	}
	if box.HostIP == "" {
		t.Error("Public() mutated the record it was given")
	}

	// ctl@ is the surface a person reads.
	out, errs, code := ds.ctl(t, "list")
	if code != 0 {
		t.Fatalf("ctl list: exit %d (%s%s)", code, out, errs)
	}
	if !strings.Contains(out, "shy") {
		t.Fatalf("ctl list does not show the sandbox:\n%s", out)
	}
	for _, bad := range []string{"sandbox.invalid", "127.0.0.1"} {
		if strings.Contains(out, bad) {
			t.Errorf("ctl list printed %q:\n%s", bad, out)
		}
	}

	// And the legacy loopback API, which is the surface that was still
	// serializing a *host.Sandbox whole.
	srv := httptest.NewServer(api.New(ds.flt, ds.routes, "ubuntu", ds.log).Handler())
	t.Cleanup(srv.Close)
	for _, path := range []string{"/v1/sandboxes", "/v1/sandboxes/shy"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for _, bad := range []string{"ssh_addr", "host_ip", "guest_v6", "sandbox.invalid"} {
			if strings.Contains(string(body), bad) {
				t.Errorf("GET %s emitted %q: %s", path, bad, body)
			}
		}
		if !strings.Contains(string(body), `"shy"`) {
			t.Errorf("GET %s did not name the sandbox at all: %s", path, body)
		}
	}
}
