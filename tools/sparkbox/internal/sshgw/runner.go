package sshgw

import (
	"context"
	"fmt"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// RunInSandbox resumes the sandbox if needed and runs cmd inside it over SSH,
// returning the command's exit code and combined output. It's the headless exec
// path the platform scheduler (internal/schedule) uses to fire background jobs
// with no client attached — the same resume + dial machinery as an interactive
// session, minus the PTY plumbing. It satisfies schedule.Runner.
//
// A transport failure (can't resume, can't dial) returns a non-nil error; a
// command that ran but exited non-zero returns its exit code with err == nil,
// so the scheduler can tell "job didn't run" from "job ran and failed".
func (g *Gateway) RunInSandbox(ctx context.Context, name, cmd string) (int, string, error) {
	box, err := host.Prepare(ctx, g.mgr, name)
	if err != nil {
		return 0, "", fmt.Errorf("resume %s: %w", name, err)
	}

	client, err := g.dialUpstream(ctx, box.SSHAddr, box.SSHUser)
	if err != nil {
		// The full cause here and not at the caller: the scheduler stores
		// err.Error() in the job's last_error column and logs the same string,
		// so this is the last place that still has the address the dial named.
		// See dialFailure.
		g.log.Warn("scheduled job could not reach its sandbox", "sandbox", name, "err", err)
		return 0, "", dialFailure(name, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return 0, "", err
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(cmd)
	if err == nil {
		return 0, string(out), nil
	}
	if exitErr, ok := err.(*xssh.ExitError); ok {
		return exitErr.ExitStatus(), string(out), nil
	}
	return 0, string(out), err
}
