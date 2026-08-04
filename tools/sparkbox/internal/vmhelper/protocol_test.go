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

func TestRequestHasNoCallerControlledPathOrCommand(t *testing.T) {
	typeOf := reflect.TypeOf(Request{})
	var fields []string
	for i := 0; i < typeOf.NumField(); i++ {
		fields = append(fields, typeOf.Field(i).Name)
	}
	want := []string{"Version", "Op", "Name", "Slot", "Resume"}
	if !slices.Equal(fields, want) {
		t.Fatalf("request fields = %v, want only %v", fields, want)
	}
}

func TestLaunchCommandIsOnlyAnUnprivilegedSocketClient(t *testing.T) {
	cmd := LaunchCommand("/usr/local/bin/sparkbox-vmm-helper", "/run/helper.sock", "box", 7, true)
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
	if err := RunLaunchClient(context.Background(), socket, "box", 2, false); err != nil {
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
	go func() { done <- RunLaunchClient(ctx, socket, "box", 2, false) }()
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
