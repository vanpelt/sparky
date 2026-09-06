//go:build linux

package vmhelper

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
)

func TestWaitForSluiceRequiresReadyAcknowledgement(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sluice.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	gotPath := make(chan string, 1)
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})}
	go httpServer.Serve(listener) //nolint:errcheck
	defer httpServer.Close()      //nolint:errcheck

	s := &server{opts: ServerOptions{SluiceSocket: socket, Logger: log.New(io.Discard, "", 0)}}
	if err := s.waitForSluice(context.Background(), "sbtap7"); err != nil {
		t.Fatal(err)
	}
	if got, want := <-gotPath, "/ready/sbtap7"; got != want {
		t.Fatalf("ready path = %q, want %q", got, want)
	}
}

func TestServerRequestValidationPinsOperationNameAndSlot(t *testing.T) {
	s := &server{network: guestnet.MustParse("172.30.0.0/20")}
	valid := request(OpLaunch, "box-1", 7)
	if err := s.validateRequest(valid, BackendFirecracker); err != nil {
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
		if err := s.validateRequest(req, BackendFirecracker); err == nil {
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

func TestChrootBaseIsSearchableButNotListable(t *testing.T) {
	base := filepath.Join(t.TempDir(), "jailer")
	if err := ensureChrootBase(base); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o711); got != want {
		t.Fatalf("chroot base mode = %#o, want %#o", got, want)
	}
}

func TestTapSourceRulesAreDerivedFromSlot(t *testing.T) {
	_, prefix6, err := net.ParseCIDR("2001:db8:1234::/64")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{network: guestnet.MustParse("172.30.0.0/20"), prefix6: prefix6.IP}
	rules := s.tapSourceRules(7)
	if got, want := len(rules), 2; got != want {
		t.Fatalf("source rules = %d, want %d", got, want)
	}
	if got, want := rules[0].binary+" "+rules[0].chain+" "+strings.Join(rules[0].args, " "),
		"iptables SPARKBOX_GUEST_OUT -i sbtap7 ! -s 172.30.0.30/32 -j DROP"; got != want {
		t.Fatalf("IPv4 source rule = %q, want %q", got, want)
	}
	if got, want := rules[1].binary+" "+rules[1].chain+" "+strings.Join(rules[1].args, " "),
		"ip6tables SPARKBOX_GUEST_OUT -i sbtap7 ! -s 2001:db8:1234::11/128 -j DROP"; got != want {
		t.Fatalf("IPv6 source rule = %q, want %q", got, want)
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
