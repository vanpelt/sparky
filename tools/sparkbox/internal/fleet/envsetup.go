package fleet

// Environment builds for a builder VM on another machine.
//
// This is the fourth node -> gateway capability, and it is here for the
// lifecycle trio's reason with an extra turn of the screw. The rows that decide
// everything — which environment is `building`, which sandbox it named as its
// builder, who owns it, what script it is supposed to run — are in the
// gateway's environments store, and no node holds that table or any part of it.
// A node that answered from its own view would be answering from nothing.
//
// # What a node may say
//
// A sandbox NAME, and — for a report — what the run did. That is the whole
// request. There is no environment field on either message, deliberately: the
// metadata door these two relay names nothing at all (see internal/metadata's
// envsetup.go), because a guest that could name the environment its result
// lands on could re-point somebody else's template. Relaying it must not put
// that field back, and it does not.
//
// The check that carries the weight is selfServiceBox, first, exactly as in
// selfservice.go and repos.go: the ledger must place that sandbox on the machine
// that asked. Without it any linked machine could fetch another owner's setup
// script — the shape of their private toolchain, often the names of their
// internal repositories — by guessing a sandbox name, and could then report a
// success that captures somebody else's disk into somebody else's tag.

import (
	"context"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// EnvSetup is the gateway's own environment-build door, reached from here on
// behalf of a machine that holds neither the environment rows nor the bindings a
// finished build writes.
//
// It takes the sandbox RECORD this package resolved from its placement ledger,
// never a name from the request, for the reason SelfLifecycle does: the
// implementation reads the owner off that record and matches it against the
// environment's, so a node relaying a guest's request never chooses whose
// authority the work runs under.
//
// It is *ctlops.Ops on a gateway — the same two methods internal/metadata
// reaches for this machine's own guests, so a builder gets the same answer
// whichever machine it landed on, which is the whole point of relaying rather
// than reimplementing.
type EnvSetup interface {
	SetupFor(ctx context.Context, box *host.Sandbox) (script, env string, ok bool, err error)
	SetupDone(ctx context.Context, box *host.Sandbox, r ctlops.SetupReport) error
}

// SetEnvSetup installs it. A setter rather than an Options field for the reason
// SetSelfLifecycle and SetRepos are setters: Ops is built with this fleet as its
// sandbox store, so the fleet exists first and the thing it delegates to cannot
// be handed to New.
func (f *Fleet) SetEnvSetup(e EnvSetup) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envSetup = e
}

func (f *Fleet) envSetupDoor() EnvSetup {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.envSetup
}

// SelfSetup answers a builder on another machine asking what it should run.
//
// The common answer by far is "no job": every VM in the fleet asks once at boot
// and all but a builder is told no, which is why nothing here is expensive and
// why an error is never a fabricated job.
func (f *Fleet) SelfSetup(ctx context.Context, node string, req nodelink.SelfSetupReq) (nodelink.SelfSetupResp, error) {
	box, err := f.selfServiceBox(node, req.Sandbox)
	if err != nil {
		return nodelink.SelfSetupResp{}, err
	}
	door := f.envSetupDoor()
	if door == nil {
		return nodelink.SelfSetupResp{}, ctlops.Disabled(nodelink.OpLink,
			"environment builds are not enabled on this gateway")
	}
	script, env, ok, err := door.SetupFor(ctx, box)
	if err != nil {
		return nodelink.SelfSetupResp{}, err
	}
	if !ok {
		return nodelink.SelfSetupResp{}, nil
	}
	// The environment's name and the script's size, never the script: it is an
	// owner's private build recipe and a gateway log is not where it belongs.
	f.log.Info("handed a setup job to a builder on another machine",
		"sandbox", box.Name, "owner", box.Owner, "node", node,
		"env", env, "script_bytes", len(script))
	return nodelink.SelfSetupResp{Job: true, Env: env, Script: script}, nil
}

// SelfSetupResult records what a builder on another machine reported.
//
// It returns as soon as the report is ACCEPTED, exactly as SelfPause does and
// for a sharper version of the same reason: the work this triggers pauses that
// VM and captures its disk, and the guest that sent the report has already been
// answered by the node's metadata service. Blocking here would hold a node's
// uplink request open across a full capture for a reply nobody is waiting on —
// on a kernel that is no longer running.
func (f *Fleet) SelfSetupResult(ctx context.Context, node string, req nodelink.SelfSetupResultReq) (nodelink.SelfSetupResultResp, error) {
	box, err := f.selfServiceBox(node, req.Sandbox)
	if err != nil {
		return nodelink.SelfSetupResultResp{}, err
	}
	door := f.envSetupDoor()
	if door == nil {
		return nodelink.SelfSetupResultResp{}, ctlops.Disabled(nodelink.OpLink,
			"environment builds are not enabled on this gateway")
	}
	f.log.Info("a builder on another machine reported its environment setup",
		"sandbox", box.Name, "owner", box.Owner, "node", node,
		"ok", req.OK, "exit", req.ExitCode)
	if err := door.SetupDone(ctx, box, ctlops.SetupReport{
		OK: req.OK, ExitCode: req.ExitCode, Script: req.Script, Log: req.Log,
	}); err != nil {
		return nodelink.SelfSetupResultResp{}, err
	}
	return nodelink.SelfSetupResultResp{Sandbox: box.Name}, nil
}
