//go:build linux

package vmhelper

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
)

func TestServerRequestValidationPinsOperationNameAndSlot(t *testing.T) {
	s := &server{network: guestnet.MustParse("172.30.0.0/20")}
	valid := request(OpLaunch, "box-1", 7)
	if err := s.validateRequest(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Request){
		func(r *Request) { r.Version++ },
		func(r *Request) { r.Op = "exec" },
		func(r *Request) { r.Name = "../../host" },
		func(r *Request) { r.Slot = -1 },
		func(r *Request) { r.Slot = s.network.Capacity() },
	} {
		req := valid
		mutate(&req)
		if err := s.validateRequest(req); err == nil {
			t.Fatalf("invalid request accepted: %+v", req)
		}
	}
}

func TestStateOpenCannotFollowControllerSymlinkOutsideRoot(t *testing.T) {
	state := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "rootfs.ext4"), []byte("host data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(state, "fc-vms"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(state, "fc-vms", "box")); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(state, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd) //nolint:errcheck
	s := &server{stateFD: fd}
	opened, err := s.openState("fc-vms/box/rootfs.ext4", unix.O_RDWR)
	if err == nil {
		unix.Close(opened) //nolint:errcheck
		t.Fatal("openat2 followed a controller-owned parent symlink outside VM state")
	}
	got, err := os.ReadFile(filepath.Join(outside, "rootfs.ext4"))
	if err != nil || string(got) != "host data" {
		t.Fatalf("outside file changed: %q, %v", got, err)
	}
}

func TestServerRejectsBroadConfiguredRootsBeforeTouchingDevices(t *testing.T) {
	_, err := newServer(ServerOptions{
		SocketPath: "/run/helper.sock", FirecrackerBin: "/firecracker",
		KernelPath: "/kernel", VMStateDir: "/", ChrootBase: "/jails",
		JailerUIDBase: 100000, ControllerUID: 65532, ControllerGID: 65532,
	})
	if err == nil {
		t.Fatal("helper accepted filesystem root as VM state directory")
	}
}

func TestPeerUIDComesFromUnixCredentials(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "helper.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck
	uidCh := make(chan uint32, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		uid, err := peerUID(conn)
		uidCh <- uid
		errCh <- err
	}()
	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	client.Close() //nolint:errcheck
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if got, want := <-uidCh, uint32(os.Getuid()); got != want {
		t.Fatalf("peer uid = %d, want kernel-reported %d", got, want)
	}
}
