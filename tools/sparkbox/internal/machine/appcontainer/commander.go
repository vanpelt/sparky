package appcontainer

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

// Commander runs the `container` binary. It is the single process-spawning
// seam: driver.go builds argv and never touches os/exec, so a test replaces
// this with a recorder and the whole adapter runs with no CLI installed.
//
// The signature is deliberately richer than hostsetup's Runner: stdin carries
// the guest script (three of the four transport shapes need it), env carries
// values by name, and stdout/stderr stay SEPARATE — vminitd runs two
// independent relays inside the machine and their relative ordering is not
// preserved, so anything that parses stdout must not have stderr interleaved
// into it.
type Commander interface {
	Run(ctx context.Context, argv []string, env []string, stdin []byte, stdout, stderr io.Writer) (int, error)
}

// execCommander is the production Commander.
type execCommander struct{ bin string }

// NewCommander returns a Commander that runs bin (normally "container").
func NewCommander(bin string) Commander {
	if bin == "" {
		bin = "container"
	}
	return execCommander{bin: bin}
}

func (c execCommander) Run(ctx context.Context, argv []string, env []string, stdin []byte, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, c.bin, argv...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != nil {
		cmd.Stdin = newBytesReader(stdin)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Cancel + WaitDelay so a wedged child is actually killed rather than
	// leaving `container` holding a pipe forever. This is belt and braces
	// around the stdin cap in machine.MaxScriptBytes, not a replacement for it:
	// the measured >=192 KiB deadlock does not respond to SIGTERM at all.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// newBytesReader avoids importing bytes purely for one reader.
func newBytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
