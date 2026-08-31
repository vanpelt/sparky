package host

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestScanListeningPorts(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	openPort := listener.Addr().(*net.TCPAddr).Port

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := scanListeningPorts(ctx, "127.0.0.1", []int{closedPort, openPort})
	if err != nil {
		t.Fatalf("scanListeningPorts: %v", err)
	}
	if len(got) != 1 || got[0] != openPort {
		t.Fatalf("listening ports = %v, want [%d]", got, openPort)
	}
}

func TestScanListeningPortsHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanListeningPorts(ctx, "127.0.0.1", []int{3000}); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
