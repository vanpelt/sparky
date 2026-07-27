package routedguest

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

type dialCall struct {
	network string
	address string
}

type recordingContextDialer struct {
	mu    sync.Mutex
	calls []dialCall
	dial  func(context.Context, string, string) (net.Conn, error)
}

func (d *recordingContextDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.calls = append(d.calls, dialCall{network: network, address: address})
	d.mu.Unlock()
	return d.dial(ctx, network, address)
}

func (d *recordingContextDialer) Calls() []dialCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dialCall(nil), d.calls...)
}

func resolver(boxes map[string]*host.Sandbox) Resolver {
	return func(name string) (*host.Sandbox, bool) {
		box, ok := boxes[name]
		if !ok {
			return nil, false
		}
		cloned := *box
		return &cloned, true
	}
}

func runningBox(hostIP, sshAddress string) *host.Sandbox {
	return &host.Sandbox{
		Name: "demo", State: vmm.StateRunning, HostIP: hostIP, SSHAddr: sshAddress,
	}
}

func successfulSocket(t *testing.T) (net.Conn, error) {
	t.Helper()
	client, peer := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		peer.Close()
	})
	return client, nil
}

func TestRoutedDialerConfinesHostIPBeforeDialing(t *testing.T) {
	prefix := netip.MustParsePrefix("10.44.0.0/20")
	tests := []struct {
		name    string
		boxes   map[string]*host.Sandbox
		sandbox string
		want    error
	}{
		{
			name: "missing sandbox", boxes: map[string]*host.Sandbox{},
			sandbox: "missing", want: nodelink.ErrUnknownSandbox,
		},
		{
			name: "stopped with stale endpoint",
			boxes: map[string]*host.Sandbox{"demo": {
				Name: "demo", State: vmm.StatePaused,
				HostIP: "203.0.113.9", SSHAddr: "203.0.113.9:22",
			}},
			sandbox: "demo", want: nodelink.ErrNotRunning,
		},
		{
			name:    "empty HostIP",
			boxes:   map[string]*host.Sandbox{"demo": runningBox("", "10.44.0.2:22")},
			sandbox: "demo", want: ErrMalformedHostIP,
		},
		{
			name:    "non-canonical HostIP",
			boxes:   map[string]*host.Sandbox{"demo": runningBox("10.44.0.2 ", "10.44.0.2:22")},
			sandbox: "demo", want: ErrMalformedHostIP,
		},
		{
			name:    "IPv4-mapped IPv6",
			boxes:   map[string]*host.Sandbox{"demo": runningBox("::ffff:10.44.0.2", "[::ffff:10.44.0.2]:22")},
			sandbox: "demo", want: ErrMalformedHostIP,
		},
		{
			name:    "ordinary IPv6",
			boxes:   map[string]*host.Sandbox{"demo": runningBox("fd00::2", "[fd00::2]:22")},
			sandbox: "demo", want: ErrMalformedHostIP,
		},
		{
			name:    "outside approved prefix",
			boxes:   map[string]*host.Sandbox{"demo": runningBox("10.44.16.2", "10.44.16.2:22")},
			sandbox: "demo", want: ErrOutOfPrefix,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socket := &recordingContextDialer{
				dial: func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("socket dial occurred after validation refusal")
					return nil, nil
				},
			}
			var observations []Observation
			dialer, err := New(prefix, resolver(test.boxes), Options{
				Dialer: socket,
				Observer: func(observation Observation) {
					observations = append(observations, observation)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			connection, err := dialer.DialGuest(context.Background(), test.sandbox, nodelink.StreamSSH, 0)
			if connection != nil {
				connection.Close()
				t.Fatal("validation refusal returned a connection")
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("DialGuest error = %v, want %v", err, test.want)
			}
			if calls := socket.Calls(); len(calls) != 0 {
				t.Fatalf("socket calls = %+v, want none", calls)
			}
			if len(observations) != 1 || observations[0].Outcome != OutcomeRejected {
				t.Fatalf("observations = %+v, want one rejection", observations)
			}
		})
	}
}

func TestRoutedDialerUsesValidatedHostWithSSHAndTCPPorts(t *testing.T) {
	boxes := map[string]*host.Sandbox{
		"demo": runningBox("10.44.0.2", "203.0.113.99:2222"),
	}
	socket := &recordingContextDialer{
		dial: func(context.Context, string, string) (net.Conn, error) {
			return successfulSocket(t)
		},
	}
	var observations []Observation
	dialer, err := New(netip.MustParsePrefix("10.44.0.0/20"), resolver(boxes), Options{
		Dialer: socket,
		Observer: func(observation Observation) {
			observations = append(observations, observation)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ssh, err := dialer.DialGuest(context.Background(), "demo", nodelink.StreamSSH, 65535)
	if err != nil {
		t.Fatal(err)
	}
	ssh.Close()
	tcp, err := dialer.DialGuest(context.Background(), "demo", nodelink.StreamTCP, 8080)
	if err != nil {
		t.Fatal(err)
	}
	tcp.Close()

	want := []dialCall{
		{network: "tcp", address: "10.44.0.2:2222"},
		{network: "tcp", address: "10.44.0.2:8080"},
	}
	got := socket.Calls()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("socket calls = %+v, want %+v", got, want)
	}
	if len(observations) != 2 ||
		observations[0] != (Observation{Kind: KindSSH, Outcome: OutcomeConnected}) ||
		observations[1] != (Observation{Kind: KindTCP, Outcome: OutcomeConnected}) {
		t.Fatalf("observations = %+v", observations)
	}
}

func TestRoutedDialerRejectsPortsAndKindsBeforeDial(t *testing.T) {
	prefix := netip.MustParsePrefix("10.44.0.0/20")
	tests := []struct {
		name       string
		sshAddress string
		kind       string
		port       int
		want       error
		wantKind   Kind
	}{
		{name: "missing SSH port", sshAddress: "10.44.0.2", kind: nodelink.StreamSSH, want: ErrInvalidPort, wantKind: KindSSH},
		{name: "missing SSH host", sshAddress: ":22", kind: nodelink.StreamSSH, want: ErrInvalidPort, wantKind: KindSSH},
		{name: "named SSH port", sshAddress: "10.44.0.2:ssh", kind: nodelink.StreamSSH, want: ErrInvalidPort, wantKind: KindSSH},
		{name: "zero SSH port", sshAddress: "10.44.0.2:0", kind: nodelink.StreamSSH, want: ErrInvalidPort, wantKind: KindSSH},
		{name: "non-canonical SSH port", sshAddress: "10.44.0.2:022", kind: nodelink.StreamSSH, want: ErrInvalidPort, wantKind: KindSSH},
		{name: "zero TCP port", sshAddress: "10.44.0.2:22", kind: nodelink.StreamTCP, port: 0, want: ErrInvalidPort, wantKind: KindTCP},
		{name: "large TCP port", sshAddress: "10.44.0.2:22", kind: nodelink.StreamTCP, port: 65536, want: ErrInvalidPort, wantKind: KindTCP},
		{name: "unknown kind", sshAddress: "10.44.0.2:22", kind: "udp", port: 53, want: ErrUnsupportedKind, wantKind: KindOther},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socket := &recordingContextDialer{
				dial: func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("invalid target reached socket dial")
					return nil, nil
				},
			}
			var observation Observation
			dialer, err := New(prefix, resolver(map[string]*host.Sandbox{
				"demo": runningBox("10.44.0.2", test.sshAddress),
			}), Options{Dialer: socket, Observer: func(value Observation) { observation = value }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := dialer.DialGuest(context.Background(), "demo", test.kind, test.port); !errors.Is(err, test.want) {
				t.Fatalf("DialGuest error = %v, want %v", err, test.want)
			}
			if len(socket.Calls()) != 0 {
				t.Fatal("invalid target reached socket dial")
			}
			if observation != (Observation{Kind: test.wantKind, Outcome: OutcomeRejected}) {
				t.Fatalf("observation = %+v", observation)
			}
		})
	}
}

func TestRoutedDialerReportsRouteLossAndPreservesCause(t *testing.T) {
	socket := &recordingContextDialer{
		dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, syscall.EHOSTUNREACH
		},
	}
	var observation Observation
	dialer, err := New(netip.MustParsePrefix("10.44.0.0/20"), resolver(map[string]*host.Sandbox{
		"demo": runningBox("10.44.0.2", "10.44.0.2:22"),
	}), Options{Dialer: socket, Observer: func(value Observation) { observation = value }})
	if err != nil {
		t.Fatal(err)
	}

	connection, err := dialer.DialGuest(context.Background(), "demo", nodelink.StreamTCP, 8080)
	if connection != nil {
		connection.Close()
		t.Fatal("route failure returned a connection")
	}
	if !errors.Is(err, ErrRouteUnavailable) || !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Fatalf("route error = %v, want route sentinel and EHOSTUNREACH", err)
	}
	var routeError *RouteError
	if !errors.As(err, &routeError) || routeError.Kind != KindTCP {
		t.Fatalf("route error type = %#v", err)
	}
	if observation != (Observation{Kind: KindTCP, Outcome: OutcomeRouteFailure}) {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestRoutedDialerHonorsCancellation(t *testing.T) {
	entered := make(chan struct{})
	socket := &recordingContextDialer{
		dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	var observation Observation
	dialer, err := New(netip.MustParsePrefix("10.44.0.0/20"), resolver(map[string]*host.Sandbox{
		"demo": runningBox("10.44.0.2", "10.44.0.2:22"),
	}), Options{Dialer: socket, Observer: func(value Observation) { observation = value }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := dialer.DialGuest(ctx, "demo", nodelink.StreamTCP, 8080)
		result <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrRouteUnavailable) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DialGuest did not honor cancellation")
	}
	if observation != (Observation{Kind: KindTCP, Outcome: OutcomeCanceled}) {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestNewRejectsNonCanonicalOrNonIPv4Prefixes(t *testing.T) {
	validResolver := resolver(map[string]*host.Sandbox{})
	tests := []netip.Prefix{
		{},
		netip.MustParsePrefix("fd00::/64"),
		netip.MustParsePrefix("10.44.0.1/20"),
		netip.MustParsePrefix("10.44.0.0/31"),
	}
	for _, prefix := range tests {
		if _, err := New(prefix, validResolver, Options{}); !errors.Is(err, ErrInvalidPrefix) {
			t.Errorf("New(%s) error = %v, want ErrInvalidPrefix", prefix, err)
		}
	}
	if _, err := New(netip.MustParsePrefix("10.44.0.0/20"), nil, Options{}); err == nil {
		t.Fatal("New accepted nil resolver")
	}
}
