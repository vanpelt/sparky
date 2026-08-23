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
	"strconv"
	"time"
)

const (
	ProtocolVersion = 1
	OpPing          = "ping"
	OpLaunch        = "launch"
	OpSnapshot      = "snapshot-outputs"
	OpCPUTime       = "cpu-time"
	maxMessageBytes = 4096
)

// Request is deliberately path-free. The helper derives every path from its
// immutable startup configuration after validating Name and Slot.
type Request struct {
	Version int    `json:"version"`
	Op      string `json:"op"`
	Name    string `json:"name,omitempty"`
	Slot    int    `json:"slot,omitempty"`
	Resume  bool   `json:"resume,omitempty"`
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
