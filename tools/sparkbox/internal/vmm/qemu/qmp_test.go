//go:build linux

package qemu

// Tests for the QMP client, driven against an in-process fake monitor. No KVM,
// no QEMU binary, no root: `go test ./internal/vmm/qemu` runs all of this.
//
// The fake speaks the wire protocol the spike measured — an unsolicited
// greeting, newline-delimited JSON, replies carrying the request's id, and
// events interleaved wherever the test wants them — because the framing rules
// are the part of this client that is easy to get subtly, permanently wrong.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const qmpTestTimeout = 5 * time.Second

// fakeQMPRequest is one command as the fake sees it. Separate from qmpRequest so
// the test decodes arguments generically instead of trusting the client's own
// marshalling shape.
type fakeQMPRequest struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments"`
	ID        uint64         `json:"id"`
}

type fakeQMPServer struct {
	t    *testing.T
	conn net.Conn
	dec  *json.Decoder
}

// send writes one frame. Errors are reported, never fatal: this runs on a
// goroutine, where t.Fatal is illegal.
func (s *fakeQMPServer) send(v any) bool {
	s.t.Helper()
	buf, err := json.Marshal(v)
	if err != nil {
		s.t.Errorf("fake server: marshal %v: %v", v, err)
		return false
	}
	if _, err := s.conn.Write(append(buf, '\n')); err != nil {
		// A closed pipe at teardown is expected, so this is informational.
		s.t.Logf("fake server: write: %v", err)
		return false
	}
	return true
}

func (s *fakeQMPServer) read() (fakeQMPRequest, bool) {
	var req fakeQMPRequest
	if err := s.dec.Decode(&req); err != nil {
		s.t.Logf("fake server: read: %v", err)
		return req, false
	}
	return req, true
}

// expect reads one command and checks its name.
func (s *fakeQMPServer) expect(cmd string) (fakeQMPRequest, bool) {
	s.t.Helper()
	req, ok := s.read()
	if !ok {
		return req, false
	}
	if req.Execute != cmd {
		s.t.Errorf("fake server: got command %q, want %q", req.Execute, cmd)
		return req, false
	}
	return req, true
}

func (s *fakeQMPServer) reply(id uint64, ret any) bool {
	if ret == nil {
		ret = map[string]any{}
	}
	return s.send(map[string]any{"return": ret, "id": id})
}

func (s *fakeQMPServer) replyErr(id uint64, class, desc string) bool {
	return s.send(map[string]any{
		"error": map[string]any{"class": class, "desc": desc},
		"id":    id,
	})
}

func (s *fakeQMPServer) event(name string, data any) bool {
	ev := map[string]any{
		"event":     name,
		"timestamp": map[string]any{"seconds": 1700000000, "microseconds": 0},
	}
	if data != nil {
		ev["data"] = data
	}
	return s.send(ev)
}

// greeting is the unsolicited object QEMU sends on connect, before anything.
func (s *fakeQMPServer) greeting() bool {
	return s.send(map[string]any{"QMP": map[string]any{
		"version": map[string]any{
			"qemu":    map[string]any{"major": 8, "minor": 2, "micro": 2},
			"package": "Debian 1:8.2.2+ds-0ubuntu1",
		},
		"capabilities": []string{"oob"},
	}})
}

// hello is the whole handshake: greeting, then qmp_capabilities.
func (s *fakeQMPServer) hello() bool {
	if !s.greeting() {
		return false
	}
	req, ok := s.expect("qmp_capabilities")
	if !ok {
		return false
	}
	return s.reply(req.ID, nil)
}

// serveCommand answers exactly one command with ret (or an error when class is
// non-empty), after emitting each of the given events first.
func startFakeQMPServer(t *testing.T, serve func(*fakeQMPServer)) (net.Conn, func()) {
	t.Helper()
	cli, srv := net.Pipe()
	s := &fakeQMPServer{t: t, conn: srv, dec: json.NewDecoder(srv)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(s)
	}()
	stop := func() {
		cli.Close()
		srv.Close()
		select {
		case <-done:
		case <-time.After(qmpTestTimeout):
			t.Error("fake QMP server goroutine did not exit")
		}
	}
	return cli, stop
}

// newTestQMPConn wires a handshaken qmpConn to a fake that runs serve afterwards.
func newTestQMPConn(t *testing.T, serve func(*fakeQMPServer)) *qmpConn {
	t.Helper()
	cli, stop := startFakeQMPServer(t, func(s *fakeQMPServer) {
		if s.hello() && serve != nil {
			serve(s)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), qmpTestTimeout)
	defer cancel()
	c, err := newQMPConn(ctx, cli)
	if err != nil {
		stop()
		t.Fatalf("newQMPConn: %v", err)
	}
	t.Cleanup(func() {
		c.Close()
		stop()
	})
	return c
}

// ---------------------------------------------------------------------------

func TestQMPCommands(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// serve runs after the handshake and drives the fake monitor.
		serve func(*fakeQMPServer)
		// run issues the client call under test.
		run func(context.Context, *qmpConn) (any, error)
		// want is compared against run's value when non-nil.
		want any
		// wantErr is a substring of the expected error; empty means success.
		wantErr string
		// wantEvents are the event names the client must have received, in order.
		wantEvents []string
	}{
		{
			name: "stop",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("stop")
				if ok {
					s.reply(req.ID, nil)
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.Stop(ctx) },
		},
		{
			name: "cont",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("cont")
				if ok {
					s.reply(req.ID, nil)
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.Cont(ctx) },
		},
		{
			name: "query-status",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("query-status")
				if ok {
					s.reply(req.ID, map[string]any{"status": "running", "running": true, "singlestep": false})
				}
			},
			run:  func(ctx context.Context, c *qmpConn) (any, error) { return c.QueryStatus(ctx) },
			want: "running",
		},
		{
			// The reason ids exist. Events land on both sides of the reply and
			// a client that reads one line and calls it a response parses the
			// first event as the answer.
			name: "events interleaved with the reply",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("query-status")
				if !ok {
					return
				}
				s.event("STOP", nil)
				s.event("BALLOON_CHANGE", map[string]any{"actual": 805306368})
				s.reply(req.ID, map[string]any{"status": "paused", "running": false})
				s.event("RESUME", nil)
			},
			run:        func(ctx context.Context, c *qmpConn) (any, error) { return c.QueryStatus(ctx) },
			want:       "paused",
			wantEvents: []string{"STOP", "BALLOON_CHANGE", "RESUME"},
		},
		{
			name: "error reply carries class and desc",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("cont")
				if ok {
					s.replyErr(req.ID, "GenericError", "Migration is not finalized")
				}
			},
			run:     func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.Cont(ctx) },
			wantErr: "GenericError: Migration is not finalized",
		},
		{
			name: "error reply arriving after an event",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("query-balloon")
				if !ok {
					return
				}
				s.event("SHUTDOWN", map[string]any{"guest": false})
				s.replyErr(req.ID, "DeviceNotActive", "No balloon device has been activated")
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) {
				return c.BalloonActualBytes(ctx)
			},
			wantErr:    "DeviceNotActive",
			wantEvents: []string{"SHUTDOWN"},
		},
		{
			// QEMU may exit before it flushes the reply to quit; a missing
			// reply is success.
			name: "quit tolerates the monitor closing instead of replying",
			serve: func(s *fakeQMPServer) {
				if _, ok := s.expect("quit"); ok {
					s.conn.Close()
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.Quit(ctx) },
		},
		{
			name: "quit with a normal reply",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("quit")
				if ok {
					s.reply(req.ID, nil)
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.Quit(ctx) },
		},
		{
			name: "migrate sends a file: uri",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("migrate")
				if !ok {
					return
				}
				if got := req.Arguments["uri"]; got != "file:/vm/state.migrate.next" {
					s.t.Errorf("migrate uri = %v, want file:/vm/state.migrate.next", got)
				}
				s.reply(req.ID, nil)
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) {
				return nil, c.MigrateToFile(ctx, "/vm/state.migrate.next")
			},
		},
		{
			// The bare {} a fresh VMM returns before any migration: an absent
			// status means "none", not a decode error.
			name: "await migration walks none -> active -> completed",
			serve: func(s *fakeQMPServer) {
				replies := []map[string]any{
					{},
					{"status": "setup"},
					{"status": "active", "ram": map[string]any{"remaining": 1024}},
					{"status": "completed"},
				}
				for _, r := range replies {
					req, ok := s.expect("query-migrate")
					if !ok {
						return
					}
					if !s.reply(req.ID, r) {
						return
					}
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.AwaitMigration(ctx) },
		},
		{
			name: "await migration reports failure with QEMU's reason",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("query-migrate")
				if ok {
					s.reply(req.ID, map[string]any{"status": "failed", "error-desc": "No space left on device"})
				}
			},
			run:     func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.AwaitMigration(ctx) },
			wantErr: "migration failed: No space left on device",
		},
		{
			name: "await migration distinguishes cancelled from failed",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("query-migrate")
				if ok {
					s.reply(req.ID, map[string]any{"status": "cancelled"})
				}
			},
			run:     func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.AwaitMigration(ctx) },
			wantErr: "cancelled",
		},
		{
			name: "migrate_cancel",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("migrate_cancel")
				if ok {
					s.reply(req.ID, nil)
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.MigrateCancel(ctx) },
		},
		{
			// The recovery poll. Unlike AwaitMigration a cancel is the success
			// case here: all it has to establish is that QEMU has stopped
			// writing, so the partial file can be unlinked and the vCPUs
			// restarted without a completion undoing both.
			name: "await migration settled treats cancelled as done",
			serve: func(s *fakeQMPServer) {
				replies := []map[string]any{
					{"status": "active", "ram": map[string]any{"remaining": 1024}},
					{"status": "cancelling"},
					{"status": "cancelled"},
				}
				for _, r := range replies {
					req, ok := s.expect("query-migrate")
					if !ok {
						return
					}
					if !s.reply(req.ID, r) {
						return
					}
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.AwaitMigrationSettled(ctx) },
		},
		{
			// cont fails until the destination leaves inmigrate; this is the
			// poll that replaces the spike's retry loop.
			name: "await runnable waits out inmigrate",
			serve: func(s *fakeQMPServer) {
				states := []string{"inmigrate", "inmigrate", "paused"}
				for _, st := range states {
					req, ok := s.expect("query-status")
					if !ok {
						return
					}
					if !s.reply(req.ID, map[string]any{"status": st, "running": false}) {
						return
					}
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) { return nil, c.AwaitRunnable(ctx) },
		},
		{
			name: "balloon sets a byte value",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("balloon")
				if !ok {
					return
				}
				if got, want := req.Arguments["value"], float64(768<<20); got != want {
					s.t.Errorf("balloon value = %v, want %v", got, want)
				}
				s.reply(req.ID, nil)
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) {
				return nil, c.SetBalloonBytes(ctx, 768<<20)
			},
		},
		{
			name: "query-balloon returns the guest's visible RAM",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("query-balloon")
				if ok {
					s.reply(req.ID, map[string]any{"actual": 1073741824})
				}
			},
			run:  func(ctx context.Context, c *qmpConn) (any, error) { return c.BalloonActualBytes(ctx) },
			want: uint64(1073741824),
		},
		{
			name: "enable balloon stats is a qom-set on the named device",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("qom-set")
				if !ok {
					return
				}
				if got := req.Arguments["path"]; got != balloonQOMPath {
					s.t.Errorf("qom-set path = %v, want %v", got, balloonQOMPath)
				}
				if got := req.Arguments["property"]; got != "guest-stats-polling-interval" {
					s.t.Errorf("qom-set property = %v", got)
				}
				if got, want := req.Arguments["value"], float64(balloonStatsIntervalSecs); got != want {
					s.t.Errorf("qom-set value = %v, want %v", got, want)
				}
				s.reply(req.ID, nil)
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) {
				return nil, c.EnableBalloonStats(ctx, balloonStatsIntervalSecs)
			},
		},
		{
			name: "guest stats",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("qom-get")
				if !ok {
					return
				}
				if got := req.Arguments["property"]; got != "guest-stats" {
					s.t.Errorf("qom-get property = %v, want guest-stats", got)
				}
				s.reply(req.ID, map[string]any{
					"stats": map[string]any{
						"stat-free-memory":      1007513600,
						"stat-available-memory": 978378752,
						"stat-total-memory":     1034022912,
					},
					"last-update": 1700000001,
				})
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) {
				free, available, err := c.GuestStats(ctx)
				return [2]uint64{free, available}, err
			},
			want: [2]uint64{1007513600, 978378752},
		},
		{
			// Before the first refresh QEMU reports last-update 0. Zeros here
			// would make the manager charge every sandbox its full ceiling.
			name: "guest stats before the first sample is an error",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("qom-get")
				if ok {
					s.reply(req.ID, map[string]any{"stats": map[string]any{}, "last-update": 0})
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) {
				free, available, err := c.GuestStats(ctx)
				return [2]uint64{free, available}, err
			},
			wantErr: "no sample yet",
		},
		{
			// An unsampled field is 0xFFFFFFFFFFFFFFFF, not a missing key.
			name: "guest stats with the unset sentinel is an error",
			serve: func(s *fakeQMPServer) {
				req, ok := s.expect("qom-get")
				if ok {
					s.reply(req.ID, map[string]any{
						"stats": map[string]any{
							"stat-free-memory":      uint64(math.MaxUint64),
							"stat-available-memory": uint64(math.MaxUint64),
						},
						"last-update": 1700000001,
					})
				}
			},
			run: func(ctx context.Context, c *qmpConn) (any, error) {
				free, _, err := c.GuestStats(ctx)
				return free, err
			},
			wantErr: "stat-free-memory unset",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestQMPConn(t, tc.serve)

			ctx, cancel := context.WithTimeout(context.Background(), qmpTestTimeout)
			defer cancel()
			got, err := tc.run(ctx, c)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("got no error, want one containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
			if tc.want != nil && err == nil && got != tc.want {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
			for _, want := range tc.wantEvents {
				select {
				case ev := <-c.Events():
					if ev.Name != want {
						t.Errorf("event = %q, want %q", ev.Name, want)
					}
				case <-time.After(qmpTestTimeout):
					t.Fatalf("timed out waiting for event %q", want)
				}
			}
		})
	}
}

// TestQMPHandshake covers the framing rule that is invisible until it is wrong:
// the greeting is unsolicited and must be consumed before qmp_capabilities.
func TestQMPHandshake(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		serve   func(*fakeQMPServer)
		wantErr string
		// wantErrAlt is a SECOND acceptable message, for the one case where
		// which of two correct errors appears is a race. See below.
		wantErrAlt string
	}{
		{
			name:  "greeting then a capabilities reply",
			serve: func(s *fakeQMPServer) { s.hello() },
		},
		{
			name: "an event before the capabilities reply is skipped",
			serve: func(s *fakeQMPServer) {
				if !s.greeting() {
					return
				}
				req, ok := s.expect("qmp_capabilities")
				if !ok {
					return
				}
				s.event("BALLOON_CHANGE", map[string]any{"actual": 1073741824})
				s.reply(req.ID, nil)
			},
		},
		{
			name: "a first message that is not the greeting is refused",
			serve: func(s *fakeQMPServer) {
				s.event("SHUTDOWN", nil)
			},
			wantErr: "not a greeting",
		},
		{
			name: "capabilities negotiation failing",
			serve: func(s *fakeQMPServer) {
				if !s.greeting() {
					return
				}
				req, ok := s.expect("qmp_capabilities")
				if ok {
					s.replyErr(req.ID, "CommandNotFound", "nope")
				}
			},
			wantErr: "CommandNotFound",
		},
		{
			// Two error messages are correct here, and which one appears is a
			// race this test cannot win. The fake closes the server end on its
			// own goroutine while newQMPConn runs, and net.Pipe fails
			// SetDeadline with io.ErrClosedPipe when EITHER end is closed, so
			// a Close that lands first makes handshake return at qmp.go's
			// SetDeadline -- one line before it ever reaches Decode and the
			// greeting message. Measured at ~1.2%: 7 failures in 600 runs of
			// this subtest when asserting the greeting stage alone.
			//
			// It is an artefact of the transport, not a defect in the client:
			// on the AF_UNIX socket the driver actually uses, SetDeadline is
			// local state and always succeeds, so the greeting stage is the
			// only reachable one there. Accept both rather than reshape a
			// production error to suit a test-only pipe.
			name:       "a monitor that closes before greeting",
			serve:      func(s *fakeQMPServer) { s.conn.Close() },
			wantErr:    "reading greeting",
			wantErrAlt: "handshake: io: read/write on closed pipe",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cli, stop := startFakeQMPServer(t, tc.serve)
			defer stop()

			ctx, cancel := context.WithTimeout(context.Background(), qmpTestTimeout)
			defer cancel()
			c, err := newQMPConn(ctx, cli)
			if err == nil {
				defer c.Close()
			} else {
				cli.Close()
			}

			matched := err != nil && (strings.Contains(err.Error(), tc.wantErr) ||
				(tc.wantErrAlt != "" && strings.Contains(err.Error(), tc.wantErrAlt)))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("got no error, want one containing %q", tc.wantErr)
			case tc.wantErr != "" && !matched:
				if tc.wantErrAlt != "" {
					t.Fatalf("error %q contains neither %q nor %q", err, tc.wantErr, tc.wantErrAlt)
				}
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestQMPDialUnixSocket exercises dialQMP itself, over the transport the driver
// actually uses.
func TestQMPDialUnixSocket(t *testing.T) {
	t.Parallel()

	// Unix socket paths are length-limited, so keep it short.
	sock := filepath.Join(t.TempDir(), "q.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		s := &fakeQMPServer{t: t, conn: conn, dec: json.NewDecoder(conn)}
		if !s.hello() {
			return
		}
		req, ok := s.expect("query-status")
		if ok {
			s.reply(req.ID, map[string]any{"status": "running", "running": true})
		}
		<-time.After(50 * time.Millisecond)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), qmpTestTimeout)
	defer cancel()
	c, err := dialQMP(ctx, sock)
	if err != nil {
		t.Fatalf("dialQMP: %v", err)
	}
	defer c.Close()

	state, err := c.QueryStatus(ctx)
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if state != "running" {
		t.Errorf("status = %q, want running", state)
	}

	if _, err := dialQMP(ctx, filepath.Join(t.TempDir(), "absent.sock")); err == nil {
		t.Error("dialing a missing socket succeeded")
	}
	<-served
}

// TestQMPContextDeadline proves a wedged VMM cannot hang a driver goroutine:
// the fake reads the command and never answers.
func TestQMPContextDeadline(t *testing.T) {
	t.Parallel()

	read := make(chan struct{})
	// release outlives the connection: the fake must keep the socket open and
	// silent until the assertions are done, so the client gives up on its own
	// deadline rather than on EOF. Registered after newTestQMPConn so it runs
	// before newTestQMPConn's own cleanup, which is what waits for the goroutine.
	release := make(chan struct{})
	c := newTestQMPConn(t, func(s *fakeQMPServer) {
		if _, ok := s.expect("query-status"); ok {
			close(read)
		}
		<-release
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.QueryStatus(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > qmpTestTimeout {
		t.Errorf("took %s to honour a 100ms deadline", elapsed)
	}
	select {
	case <-read:
	case <-time.After(qmpTestTimeout):
		t.Error("the command was never written")
	}
}

// TestQMPCancelledCommandDoesNotPoisonTheNextOne: a reply that arrives after
// its caller gave up has no waiter, and must be dropped rather than handed to
// whoever asks next.
func TestQMPCancelledCommandDoesNotPoisonTheNextOne(t *testing.T) {
	t.Parallel()

	c := newTestQMPConn(t, func(s *fakeQMPServer) {
		first, ok := s.expect("query-status")
		if !ok {
			return
		}
		// Answer long after the caller's 100ms deadline.
		<-time.After(300 * time.Millisecond)
		if !s.reply(first.ID, map[string]any{"status": "inmigrate", "running": false}) {
			return
		}
		second, ok := s.expect("query-status")
		if ok {
			s.reply(second.ID, map[string]any{"status": "running", "running": true})
		}
	})

	shortCtx, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if _, err := c.QueryStatus(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call error = %v, want context.DeadlineExceeded", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), qmpTestTimeout)
	defer cancel()
	state, err := c.QueryStatus(ctx)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if state != "running" {
		t.Errorf("status = %q, want running (the abandoned reply leaked)", state)
	}
}

// TestQMPConcurrentCommands: the driver calls from more than one goroutine, and
// replies are matched by id, not by arrival order.
func TestQMPConcurrentCommands(t *testing.T) {
	t.Parallel()

	const n = 8
	c := newTestQMPConn(t, func(s *fakeQMPServer) {
		reqs := make([]fakeQMPRequest, 0, n)
		for i := 0; i < n; i++ {
			req, ok := s.expect("qom-get")
			if !ok {
				return
			}
			reqs = append(reqs, req)
		}
		// Answer in reverse, with an event wedged in the middle, so a client
		// relying on arrival order gets every answer wrong.
		for i := len(reqs) - 1; i >= 0; i-- {
			if i == len(reqs)/2 {
				s.event("BALLOON_CHANGE", map[string]any{"actual": 1073741824})
			}
			if !s.reply(reqs[i].ID, reqs[i].Arguments["path"]) {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), qmpTestTimeout)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/machine/peripheral/dev%d", i)
			var got string
			if err := c.QOMGet(ctx, path, "id", &got); err != nil {
				errs[i] = err
				return
			}
			if got != path {
				errs[i] = fmt.Errorf("goroutine %d got reply for %q", i, got)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
}

// TestQMPEventFloodDoesNotStallTheReader: the event sink drops rather than
// blocks, because a stalled reader stalls the monitor for good.
func TestQMPEventFloodDoesNotStallTheReader(t *testing.T) {
	t.Parallel()

	c := newTestQMPConn(t, func(s *fakeQMPServer) {
		for i := 0; i < qmpEventBufferSize*3; i++ {
			if !s.event("BALLOON_CHANGE", map[string]any{"actual": i}) {
				return
			}
		}
		req, ok := s.expect("query-status")
		if ok {
			s.reply(req.ID, map[string]any{"status": "running", "running": true})
		}
	})

	// Nobody ever reads c.Events(); the command must still complete.
	ctx, cancel := context.WithTimeout(context.Background(), qmpTestTimeout)
	defer cancel()
	state, err := c.QueryStatus(ctx)
	if err != nil {
		t.Fatalf("QueryStatus after an event flood: %v", err)
	}
	if state != "running" {
		t.Errorf("status = %q, want running", state)
	}
}

// TestQMPClosedConnection: once the monitor goes away, every call fails with a
// wrapped errQMPClosed rather than hanging.
func TestQMPClosedConnection(t *testing.T) {
	t.Parallel()

	gone := make(chan struct{})
	c := newTestQMPConn(t, func(s *fakeQMPServer) {
		<-gone
		s.conn.Close()
	})
	close(gone)

	ctx, cancel := context.WithTimeout(context.Background(), qmpTestTimeout)
	defer cancel()

	// Poll: the close races the assertion, and either the in-flight call or a
	// later one must report it.
	deadline := time.Now().Add(qmpTestTimeout)
	var err error
	for time.Now().Before(deadline) {
		if _, err = c.QueryStatus(ctx); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !qmpIsDisconnect(err) {
		t.Fatalf("error = %v, want a disconnect the driver can recognise", err)
	}
	// Quit forgives exactly this: QEMU exiting before it answers.
	if err := c.Quit(ctx); err != nil {
		t.Errorf("Quit on a dead monitor = %v, want nil", err)
	}
}

// TestQMPErrorIsTyped: callers can reach QEMU's class, which is the only way to
// tell "no balloon device" from a genuine failure.
func TestQMPErrorIsTyped(t *testing.T) {
	t.Parallel()

	c := newTestQMPConn(t, func(s *fakeQMPServer) {
		req, ok := s.expect("query-balloon")
		if ok {
			s.replyErr(req.ID, "DeviceNotActive", "No balloon device has been activated")
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), qmpTestTimeout)
	defer cancel()
	_, err := c.BalloonActualBytes(ctx)
	var qerr *qmpError
	if !errors.As(err, &qerr) {
		t.Fatalf("error %v is not a *qmpError", err)
	}
	if qerr.Class != "DeviceNotActive" {
		t.Errorf("class = %q, want DeviceNotActive", qerr.Class)
	}
	if qerr.Desc != "No balloon device has been activated" {
		t.Errorf("desc = %q", qerr.Desc)
	}
}
