package hostsetup

import (
	"context"
	"os/exec"
)

// execRunner is the production Runner: it shells out and returns combined
// output. Callers that tolerate a non-zero exit (e.g. `systemctl is-active`)
// read the output regardless of err.
type execRunner struct{}

// NewExecRunner returns a Runner backed by os/exec.
func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
