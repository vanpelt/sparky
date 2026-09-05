// Package vmhelper implements the narrow privileged boundary used by the CKS
// Firecracker node. The unprivileged controller can ask for operations derived
// from a validated VM name and slot, but it cannot supply commands, device
// numbers, credentials, or arbitrary filesystem paths.
package vmhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

const (
	// ProtocolVersion 2 adds the machine fields a QEMU launch needs. Version 1
	// is still accepted, because the controller and the helper are separate
	// CONTAINERS of one Pod and restart independently: a rolling update briefly
	// has one on each image, and a hard version match would turn that window
	// into "every launch fails" rather than "the old client keeps working".
	// A version-1 request is a Firecracker launch by definition — it predates
	// there being another backend to ask for.
	ProtocolVersion = 2
	// MinProtocolVersion is the oldest request the server still answers.
	MinProtocolVersion = 1

	OpPing          = "ping"
	OpLaunch        = "launch"
	OpSnapshot      = "snapshot-outputs"
	OpCPUTime       = "cpu-time"
	maxMessageBytes = 4096

	// MaxCmdlineBytes bounds the guest kernel command line. The whole message
	// is read through a 4 KiB limit, so this cannot be raised without raising
	// that too; the real line is ~300 bytes.
	MaxCmdlineBytes = 2048
	// MaxVCPUs and MaxMemMB bound the machine a caller can ask for. They are
	// sanity limits, not policy — admission control lives in the controller —
	// but the helper is the privileged process and must not build an argv from
	// numbers it has not looked at.
	MaxVCPUs = 256
	MaxMemMB = 1 << 20 // 1 TiB
)

// Backend names the VMM this helper launches. It is a SERVER-SIDE startup
// flag and never appears in a Request, which is the point: the securityContext
// comment in deploy/kubernetes/deployment.yaml calls this socket "the entire
// long-lived Linux privilege boundary" and says its protocol contains no
// commands. A field letting the unprivileged controller pick which binary the
// root process executes would make that sentence false.
//
// One helper launches one VMM for its whole life, which also matches how a node
// works: vmm.ClaimStateDir already refuses to let a node's VM state directory
// change hands between drivers, so a helper that could serve both would be
// serving a situation that cannot arise.
type Backend string

const (
	BackendFirecracker Backend = "firecracker"
	BackendQEMU        Backend = "qemu"
)

// ParseBackend maps the operator-facing flag value onto a Backend. An empty
// value is Firecracker, so every existing deployment keeps its behaviour
// without changing a flag.
func ParseBackend(v string) (Backend, error) {
	switch v {
	case "", string(BackendFirecracker):
		return BackendFirecracker, nil
	case string(BackendQEMU):
		return BackendQEMU, nil
	default:
		return "", fmt.Errorf("unknown vmm backend %q (want %q or %q)", v, BackendFirecracker, BackendQEMU)
	}
}

// Request is deliberately path-free and command-free. The helper derives every
// path from its immutable startup configuration after validating Name and Slot,
// and it chooses the VMM from ITS OWN startup flag rather than from anything
// here — a controller that could name the binary to run would not be a boundary.
//
// # WHY THE MACHINE FIELDS EXIST, AND WHY THEY ARE NOT A COMMAND
//
// Firecracker takes an empty argv and receives its machine configuration over
// its own REST socket afterwards, so a launch needed nothing but a name and a
// slot. QEMU takes the entire machine on the command line and there is no
// after. The helper therefore has to know the vCPU count, the memory size and
// the guest kernel command line to build an argv at all.
//
// Those three are data, not instructions: the helper validates each, places
// each at a fixed position in an argv IT constructs, and passes Cmdline as a
// single element of an exec vector — never through a shell — so a value
// containing spaces stays one token and cannot become another QEMU option.
// Everything that selects behaviour (which binary, which machine type, which
// kernel image, every path) still comes from the helper's own startup flags.
type Request struct {
	Version int    `json:"version"`
	Op      string `json:"op"`
	Name    string `json:"name,omitempty"`
	Slot    int    `json:"slot,omitempty"`
	Resume  bool   `json:"resume,omitempty"`
	VCPUs   int64  `json:"vcpus,omitempty"`
	MemMB   int64  `json:"mem_mb,omitempty"`
	Cmdline string `json:"cmdline,omitempty"`
}

// cmdlineRE is the guest kernel command line the helper will accept: printable
// ASCII, no control characters, no quotes or shell metacharacters that would
// make a reader wonder whether this ever reaches a shell (it does not).
//
// The line is composed by the controller from values it derives — the guest's
// addresses, its DNS, its machine-id, the sparkbox_fresh marker — so this is a
// shape check rather than a policy one. A NUL would be rejected by execve
// itself, far less legibly; a newline would be the one character that could
// confuse a log reader into seeing two events.
var cmdlineRE = regexp.MustCompile(`^[A-Za-z0-9 ._:,=/@%+-]*$`)

// ValidateMachine checks the fields a QEMU launch adds. Split out from the
// server so it is testable without a Linux host, and exported-adjacent so the
// client can fail early rather than after a round trip.
func ValidateMachine(req Request) error {
	if req.VCPUs < 1 || req.VCPUs > MaxVCPUs {
		return fmt.Errorf("vcpus %d out of range (1..%d)", req.VCPUs, MaxVCPUs)
	}
	if req.MemMB < 1 || req.MemMB > MaxMemMB {
		return fmt.Errorf("memory %d MiB out of range (1..%d)", req.MemMB, MaxMemMB)
	}
	if req.Cmdline == "" {
		return errors.New("guest kernel command line is required")
	}
	if len(req.Cmdline) > MaxCmdlineBytes {
		return fmt.Errorf("guest kernel command line is %d bytes, limit %d", len(req.Cmdline), MaxCmdlineBytes)
	}
	if !cmdlineRE.MatchString(req.Cmdline) {
		return errors.New("guest kernel command line contains characters outside the accepted set")
	}
	return nil
}

type Response struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	CPUTimeNanos uint64 `json:"cpu_time_nanos,omitempty"`
}

func request(op, name string, slot int) Request {
	return Request{Version: ProtocolVersion, Op: op, Name: name, Slot: slot}
}

func dial(ctx context.Context, socketPath string) (*net.UnixConn, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close() //nolint:errcheck
		return nil, errors.New("helper socket did not produce a Unix connection")
	}
	return unixConn, nil
}

func writeRequest(conn net.Conn, req Request) error {
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("write helper request: %w", err)
	}
	return nil
}

func readResponse(decoder *json.Decoder) (Response, error) {
	var response Response
	if err := decoder.Decode(&response); err != nil {
		return response, fmt.Errorf("read helper response: %w", err)
	}
	if !response.OK {
		if response.Error == "" {
			response.Error = "request was refused"
		}
		return response, errors.New(response.Error)
	}
	return response, nil
}

func Call(ctx context.Context, socketPath string, req Request) (Response, error) {
	conn, err := dial(ctx, socketPath)
	if err != nil {
		return Response{}, fmt.Errorf("dial privileged helper: %w", err)
	}
	defer conn.Close() //nolint:errcheck
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline) //nolint:errcheck
	}
	if err := writeRequest(conn, req); err != nil {
		return Response{}, err
	}
	return readResponse(json.NewDecoder(io.LimitReader(conn, maxMessageBytes)))
}

func Ping(ctx context.Context, socketPath string) error {
	_, err := Call(ctx, socketPath, request(OpPing, "", 0))
	return err
}

func PrepareSnapshotOutputs(ctx context.Context, socketPath, name string, slot int) error {
	_, err := Call(ctx, socketPath, request(OpSnapshot, name, slot))
	return err
}

func CPUTimeNanos(ctx context.Context, socketPath, name string, slot int) (uint64, error) {
	response, err := Call(ctx, socketPath, request(OpCPUTime, name, slot))
	return response.CPUTimeNanos, err
}

// LaunchCommand returns the unprivileged process runner handed to the
// Firecracker SDK. That process owns no VMM privileges: it only holds an
// authenticated connection open while the helper owns the real Firecracker
// child. SIGTERM closes the write side, waits for helper cleanup, and exits.
func LaunchCommand(helperBin, socketPath, name string, slot int, resume bool) *exec.Cmd {
	args := []string{
		"launch",
		"--socket", socketPath,
		"--name", name,
		"--slot", strconv.Itoa(slot),
	}
	if resume {
		args = append(args, "--resume")
	}
	// Do not use CommandContext. The SDK signals this process before cancelling
	// the VM context; an automatic SIGKILL would bypass the cleanup handshake.
	return exec.Command(helperBin, args...)
}

// RunLaunchClient blocks for the lifetime of one VMM. Cancellation asks the
// server to terminate Firecracker and waits for the tap and jail to be removed.
func RunLaunchClient(ctx context.Context, socketPath, name string, slot int, resume bool) error {
	conn, err := dial(ctx, socketPath)
	if err != nil {
		return fmt.Errorf("dial privileged helper: %w", err)
	}
	defer conn.Close() //nolint:errcheck
	req := request(OpLaunch, name, slot)
	req.Resume = resume
	if err := writeRequest(conn, req); err != nil {
		return err
	}
	decoder := json.NewDecoder(io.LimitReader(conn, 2*maxMessageBytes))
	if _, err := readResponse(decoder); err != nil {
		return fmt.Errorf("launch refused: %w", err)
	}

	finished := make(chan error, 1)
	go func() {
		_, err := readResponse(decoder)
		finished <- err
	}()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		// Half-close is the protocol's stop request. Keep the read side alive so
		// the server can acknowledge only after the VMM, tap, and jail are gone.
		if err := conn.CloseWrite(); err != nil {
			return fmt.Errorf("request helper stop: %w", err)
		}
		select {
		case err := <-finished:
			return err
		case <-time.After(15 * time.Second):
			return errors.New("timed out waiting for privileged helper cleanup")
		}
	}
}
