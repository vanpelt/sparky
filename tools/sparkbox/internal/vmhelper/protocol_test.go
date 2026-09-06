package vmhelper

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

// TestRequestHasNoCallerControlledPathOrCommand is the boundary, written down.
// The helper is root; the controller is not. Adding a field here is adding
// something an unprivileged process gets to say to a privileged one, so the
// list is exact and changing it is deliberate.
//
// The rule the list encodes: a Request may carry DATA the helper validates and
// places at a fixed position in an argv it builds; it may never carry anything
// that SELECTS BEHAVIOUR — a path, an executable, a device, a credential, a
// flag, or a fragment of a command.
//
// VCPUs, MemMB and Cmdline were added for QEMU and are on the data side of that
// line. Firecracker takes an empty argv and receives its machine configuration
// over its own REST socket afterwards; QEMU takes the whole machine on the
// command line and there is no afterwards, so the helper cannot build an argv
// without them. Each is range- or shape-checked by ValidateMachine, and Cmdline
// is passed as a single element of an exec vector — never through a shell — so
// a value with spaces in it stays one token. What is executed, which machine
// model, which kernel image and every filesystem path still come only from the
// helper's own startup flags.
func TestRequestHasNoCallerControlledPathOrCommand(t *testing.T) {
	typeOf := reflect.TypeOf(Request{})
	var fields []string
	for i := 0; i < typeOf.NumField(); i++ {
		fields = append(fields, typeOf.Field(i).Name)
	}
	want := []string{"Version", "Op", "Name", "Slot", "Resume", "VCPUs", "MemMB", "Cmdline"}
	if !slices.Equal(fields, want) {
		t.Fatalf("request fields = %v, want only %v.\n"+
			"Adding a field here widens what an unprivileged process can say to a root one. "+
			"Read this test's doc comment before changing the list.", fields, want)
	}
}

// TestServerOwnsEveryExecutableAndPath is the other half of the same rule, and
// it is mechanical rather than a matter of review: every name by which the
// helper knows a binary or a location must live on ServerOptions, which is
// built from its own startup flags, and none of them may appear on Request.
func TestServerOwnsEveryExecutableAndPath(t *testing.T) {
	fieldNames := func(v any) map[string]bool {
		typeOf := reflect.TypeOf(v)
		out := map[string]bool{}
		for i := 0; i < typeOf.NumField(); i++ {
			out[typeOf.Field(i).Name] = true
		}
		return out
	}
	server := fieldNames(ServerOptions{})
	request := fieldNames(Request{})

	// Everything that names something on the filesystem, selects a binary, or
	// picks the machine model. If one of these ever migrates to Request, the
	// controller has gained the ability to point root at a target of its choice.
	for _, owned := range []string{
		"SocketPath", "Backend", "QemuBin", "MachineType",
		"FirecrackerBin", "KernelPath", "VMStateDir", "ChrootBase",
		"JailerUIDBase", "ControllerUID", "ControllerGID", "SluiceSocket",
	} {
		if !server[owned] {
			t.Errorf("ServerOptions no longer has %q; if it moved, check it did not move to Request", owned)
		}
		if request[owned] {
			t.Errorf("Request has %q: the caller can now choose it, and the caller is unprivileged", owned)
		}
	}
}

func TestLaunchCommandIsOnlyAnUnprivilegedSocketClient(t *testing.T) {
	cmd := LaunchCommand("/usr/local/bin/sparkbox-vmm-helper", Launch{
		Socket: "/run/helper.sock", Name: "box", Slot: 7, Resume: true,
	})
	want := []string{
		"/usr/local/bin/sparkbox-vmm-helper", "launch",
		"--socket", "/run/helper.sock", "--name", "box", "--slot", "7", "--resume",
	}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("args = %v, want %v", cmd.Args, want)
	}
	if cmd.SysProcAttr != nil {
		t.Fatalf("helper client unexpectedly changes process attributes: %+v", cmd.SysProcAttr)
	}
}

// A Firecracker launch must produce the argv it has always produced, byte for
// byte: the controller and the helper are separate containers with independent
// restarts, so a rolling update runs a new client against an old helper, and an
// unconditional --vcpus would be an unknown flag to it.
func TestLaunchCommandOmitsTheMachineWhenThereIsNone(t *testing.T) {
	cmd := LaunchCommand("/h", Launch{Socket: "/s", Name: "box", Slot: 0})
	for _, flag := range []string{"--vcpus", "--mem-mb", "--cmdline", "--resume"} {
		if slices.Contains(cmd.Args, flag) {
			t.Errorf("a Firecracker launch carries %s", flag)
		}
	}
}

// The guest command line contains spaces by construction. It has to survive as
// ONE argv element from here to the helper's JSON to QEMU's -append; a value
// that split would become several QEMU options chosen by the caller.
func TestLaunchCommandKeepsTheGuestCmdlineOneArgument(t *testing.T) {
	const cmdline = "console=ttyS0 root=/dev/vda rw sparkbox_fresh=1"
	cmd := LaunchCommand("/h", Launch{
		Socket: "/s", Name: "box", Slot: 3, VCPUs: 2, MemMB: 1024, Cmdline: cmdline,
	})
	i := slices.Index(cmd.Args, "--cmdline")
	if i < 0 || i+1 >= len(cmd.Args) {
		t.Fatalf("no --cmdline in %v", cmd.Args)
	}
	if cmd.Args[i+1] != cmdline {
		t.Errorf("cmdline argument = %q, want %q", cmd.Args[i+1], cmdline)
	}
	// argv[0], "launch", and six flag/value pairs. Counted so a cmdline that
	// split into words fails here rather than at exec time on a real node.
	if got := len(cmd.Args); got != 14 {
		t.Errorf("argv has %d elements, want 14: %v", got, cmd.Args)
	}
}

func TestLaunchClientPreservesBufferedFinalResponse(t *testing.T) {
	socket := shortSocket(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		var req Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			serverErr <- err
			return
		}
		// A short-lived VMM can put both JSON documents in one socket read. The
		// client must retain the decoder's buffer between the acknowledgement and
		// final status.
		_, err = conn.Write([]byte("{\"ok\":true}\n{\"ok\":true}\n"))
		serverErr <- err
	}()
	if err := RunLaunchClient(context.Background(), Launch{Socket: socket, Name: "box", Slot: 2, Resume: false}); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLaunchClientWaitsForCleanupAcknowledgement(t *testing.T) {
	socket := shortSocket(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck
	cleaned := make(chan struct{})
	acknowledged := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		var req Request
		if json.NewDecoder(conn).Decode(&req) != nil {
			return
		}
		json.NewEncoder(conn).Encode(Response{OK: true}) //nolint:errcheck
		close(acknowledged)
		var b [1]byte
		conn.Read(b[:]) //nolint:errcheck // EOF is the stop request.
		close(cleaned)
		json.NewEncoder(conn).Encode(Response{OK: true}) //nolint:errcheck
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunLaunchClient(ctx, Launch{Socket: socket, Name: "box", Slot: 2, Resume: false}) }()
	select {
	case <-acknowledged:
	case <-time.After(time.Second):
		t.Fatal("launch client did not receive acknowledgement")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("launch client returned before helper cleanup acknowledgement")
	}
}

func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vmhelper-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "helper.sock")
}
