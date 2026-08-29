package envsync

// Installing the host's cached agent CLIs into a guest, from the outside.
//
// This is the repo nudge's sibling (repos.go) and shares its whole argument for
// living here: what it needs is the guest exec channel — a dialer that reaches
// a sandbox on any machine in the fleet, and an upstream key to open it with —
// and that channel is wired once, in cmd/sparkbox, for this package. A second
// copy would be a second thing to route and to test against the mock driver.
//
// What it sends is a nudge too: no artifacts, no versions, no manifest. The
// guest's own /usr/local/sbin/sparkbox-update-tools pulls what it needs from
// the metadata service on its tap, verifying every artifact against the digest
// the host published, so nothing about which tools exist or what they hash to
// has to be kept in step on this side.
//
// The one place it differs from every other exec in this package is that it
// BLOCKS until the install is done, and that is not a stylistic choice: see
// RefreshTools.

import (
	"context"
	"errors"
	"fmt"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// ErrNoToolRefresh is host's, re-exported so a reader of this file can see what
// the guest's exits below become — the same courtesy repos.go does for
// ErrNoRepoSupport, and for the same reason: a sandbox created before the tool
// payload existed must be NAMED, not silently reported as refreshed.
var ErrNoToolRefresh = host.ErrNoToolRefresh

// toolRefreshTimeout bounds one refresh when the caller's context carries no
// deadline. Deliberately not pushTimeout: that is 3 minutes sized for one exec
// rewriting one file, while this downloads and installs roughly 150MB of agent
// CLIs. The caller that matters (host.Manager.refreshToolsForPack) applies its
// own five-minute budget and WithTimeout keeps the earlier deadline, so this
// value only governs a caller that passed a bare context.
const toolRefreshTimeout = 10 * time.Minute

// toolUpdaterPath is the in-guest installer, written into every template by
// deploy/install-guest-identity.sh. A package var only so the tests can point
// it at something the mock driver's unprivileged /bin/sh can actually run; it
// is never derived from a request, so it is interpolated into the script below
// without quoting.
var toolUpdaterPath = "/usr/local/sbin/sparkbox-update-tools"

// The guest exits that mean "this sandbox cannot update its tools", as opposed
// to "the update failed".
//
// 3 is the script below saying the installer is not in this rootfs — a sandbox
// created before the payload shipped. 2 and 127 are the same fact arriving by
// two other routes: the in-guest `sparkbox` dispatcher is a POSIX-sh case
// statement whose unknown-verb branch exits 2 (install-guest-identity.sh, the
// `usage: sparkbox <pin|unpin|…>` line), so a template that predates the
// `update-tools` verb answers 2 rather than 3; and 127 is a shell that could
// not find the command at all. All three are one sentence to the reader, and
// none of them is worth failing a capture over.
const (
	toolExitUnsupported = 3
	toolExitBadVerb     = 2
	toolExitNotFound    = 127
)

// toolRefreshScript runs the guest's installer and waits for it.
//
// No systemd unit and no setsid, which is the whole difference from
// repoSyncScript: that one returns the moment the guest has accepted the job
// because a monorepo clone outlives the control-plane call that triggered it.
// The caller here is about to STOP the guest, so a detached install would be
// killed partway through writing an executable — and the truncated result is
// what every fork of the resulting template would copy.
func toolRefreshScript(updater string) string {
	// The trailing sync is what makes the installed bytes survive the capture,
	// and it is the reason this does not `exec` the updater.
	//
	// The pause that follows freezes dirty page cache into the MEMORY snapshot,
	// while Driver.Snapshot reflinks the BLOCK DEVICE. An install that is still
	// only in the guest's page cache is therefore present when the sandbox
	// resumes and ABSENT from the template every fork is made from — the file is
	// renamed into place but its data blocks never landed. Writeback would get
	// there on its own in seconds; the pause does not wait seconds.
	//
	// The in-guest `sparkbox snapshot` verb runs the same sync before it commits,
	// for the same reason. This is the host-initiated half of that rule.
	return "set -eu\n" +
		"[ -x " + updater + " ] || exit " + fmt.Sprint(toolExitUnsupported) + "\n" +
		updater + "\n" +
		"sync\n"
}

// RefreshTools installs the agent CLIs box's host has cached, and returns when
// the guest has finished installing them.
//
// Implements host.ToolRefresher. It has exactly one caller on each side of the
// fleet split — host.Manager.Snapshot for a local sandbox, Fleet.Snapshot for
// one on another machine — and both call it immediately before pausing a
// sandbox to capture it as a template.
//
// Three deliberate divergences from SyncRepos, all forced by that caller:
//
//   - It is synchronous, as above.
//   - A nil or not-running box is a NAMED error rather than a silent no-op.
//     Both callers have just checked; reaching here with a paused box means a
//     check went missing, and this is not the code that may wake one — the wake
//     belongs to the pre-pack strip, whose resumeOrRecreate avoids the
//     EnsureRunning env push that would race it.
//   - It ignores the quiesce flag. That flag exists to stop a change-time
//     SECRET push racing the pre-pack strip; this writes /usr/local/bin and is
//     itself a step of the very pack sequence that set the flag, so honouring
//     it would mean the pack skipping its own step.
func (s *Syncer) RefreshTools(ctx context.Context, box *host.Sandbox) error {
	if box == nil {
		return errors.New("refresh agent tools: no sandbox")
	}
	if box.State != vmm.StateRunning || box.SSHAddr == "" {
		return fmt.Errorf("sandbox %s is not reachable for an agent-tool refresh (state %s)", box.Name, box.State)
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, toolRefreshTimeout)
		defer cancel()
	}
	client, err := sshgw.DialUpstreamVia(ctx, s.dial, box.SSHAddr, box.SSHUser, s.upstreamKey)
	if err != nil {
		return fmt.Errorf("dial %s: %w", box.Name, err)
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	out, err := runScript(sess, s.shell, toolRefreshScript(toolUpdaterPath))
	if err == nil {
		s.log.Debug("refreshed agent tools", "sandbox", box.Name)
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("refresh agent tools on %s: %w", box.Name, ctx.Err())
	}
	var exitErr *xssh.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitStatus() {
		case toolExitUnsupported, toolExitBadVerb, toolExitNotFound:
			return fmt.Errorf("%w: %s", ErrNoToolRefresh, box.Name)
		}
	}
	return fmt.Errorf("refresh agent tools on %s: %w (%s)", box.Name, err, out)
}

var _ host.ToolRefresher = (*Syncer)(nil)
