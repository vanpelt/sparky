package fleet

// Choosing the machine a new sandbox is built on.
//
// This file is a seam more than an algorithm. M2 places a sandbox on exactly
// two kinds of machine — this one, or the one the caller named with --node —
// and a scheduler that picks for itself is a later milestone. The reason the
// interface exists now, ahead of any implementation worth the name, is that
// Fleet.Create used to hardcode f.local: every future placement rule would have
// been an edit to the create path, and the create path is the one with the
// reserve/build/undo sequence and the fleet-wide name allocation in it. Landing
// the seam first means the scheduler replaces a Placer and touches none of that.
//
// The division of labour is deliberate: a Placer decides WHICH machine, and
// the machine itself decides WHETHER. A gateway-side filter can only ever read
// a cached report, so anything it refuses on a live number it may refuse
// wrongly — which is why the checks here are limited to facts that do not go
// stale between heartbeats, and why the local machine is never filtered at all.

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Request is what is being placed. It is the create call's arguments plus the
// two things a scheduler needs that a create does not carry: which machine the
// caller asked for, and what architecture the image requires.
//
// A zero VCPUs/MemMB/DiskMB means "whatever that machine's default is", which
// is why nothing here may be compared against a budget without checking for
// zero first: the gateway does not know another machine's defaults and must not
// pretend a request for none is a request for nothing.
type Request struct {
	Owner      string
	Image      string
	Arch       string // "" means the image runs anywhere
	PreferNode string // the caller's --node; "" leaves the choice open
	// Runner is the VMM the sandbox's environment requires; "" is no
	// requirement, which is what almost every create carries.
	//
	// It is the first constraint in this struct that a caller sets on purpose.
	// Arch describes the image and PreferNode is a machine name, but a runner
	// is somebody saying "this work needs that VMM" — so unlike the others it
	// must not be quietly satisfied by the wrong answer, and pick() refuses to
	// take its single-box shortcut while one is set.
	Runner vmm.Runner
	VCPUs  int64
	MemMB  int64
	DiskMB int64
}

// Candidate is one machine as a placement decision sees it.
//
// Local and Online are here rather than derived because a Placer cannot derive
// them: it is handed a slice of machines and knows neither which of them is the
// gateway it is running inside nor how the fleet defines "answering". Without
// Local a placer could not express "otherwise, build it here" — the rule M2's
// whole placement policy consists of — without being told the gateway's own
// node name by some other route.
type Candidate struct {
	Name     string
	Local    bool
	Online   bool
	Facts    Facts
	Capacity host.NodeCapacity
}

// Placer chooses the machine. It returns a node name rather than a Node so an
// implementation cannot reach past the decision and start operating one, and so
// a placer can be written and tested against nothing but data.
//
// An error is the answer when nothing can take the request. Returning a typed
// *ctlops.Error is strongly preferred — the sentence reaches a user verbatim —
// but anything is accepted, because ctlops.AsError classifies whatever arrives.
type Placer interface {
	Place(req Request, nodes []Candidate) (node string, err error)
}

// SetPlacer installs the placement policy. Nil restores the default.
//
// It is a setter rather than an Options field because the scheduler that will
// want it does not exist yet and the wiring that would carry it does not either;
// a field would be a promise about a shape nobody has built.
func (f *Fleet) SetPlacer(p Placer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.placer = p
}

// candidates is every machine this fleet could place on, this one first and the
// rest name-sorted, offline ones included.
//
// Offline machines are included on purpose. A placer that never saw them could
// only answer "no node named %q" for a machine that is merely rebooting, and
// telling a user their laptop does not exist because it is asleep is the same
// class of unreadable failure as an io.EOF reaching a terminal.
func (f *Fleet) candidates() []Candidate {
	out := []Candidate{f.candidate(f.local, true)}
	for _, n := range f.linked() {
		out = append(out, f.candidate(n, false))
	}
	return out
}

func (f *Fleet) candidate(n Node, local bool) Candidate {
	online := n.Online()
	capacity := n.Capacity()
	capacity.Online = online
	facts := n.Facts()
	if facts.Arch == "" && local {
		facts.Arch = f.localArch
	}
	facts.Images = withLiveTemplates(facts.Images, n.Templates())
	return Candidate{Name: n.Name(), Local: local, Online: online, Facts: facts, Capacity: capacity}
}

// withLiveTemplates folds the templates a machine holds right now into the image
// list Fits reads.
//
// The two lists have different freshness. Facts.Images is a directory listing
// the node took once, while composing its hello (cmd/sparkbox/node.go:764), and
// a gateway only ever sees it again on a reconnect. Templates() is the live
// inventory, kept current by the snapshot rows the link streams. So a snapshot
// captured on a remote machine after its link came up is in the inventory and
// absent from Images — and Fits below would answer `node %q does not have the
// %q image` for a create the tag-template path now routes to that very machine
// precisely because it is the one holding the template.
//
// Strictly additive: Fits only ever refuses on an image it cannot find, so a
// longer list can turn a refusal into a pass and never the reverse.
//
// That is also why an empty Images is left empty rather than replaced by the
// template names. Fits skips the check entirely when a node reported no list,
// because unknown must not read as refused; seeding it with two snapshot names
// would turn that silence into a list, and a plain `ubuntu` create onto a
// machine whose image directory failed to read would start being refused.
//
// The clone is not defensive habit. facts is a copy of the node's Hello struct,
// but the Images header still points at the node's own backing array
// (nodelink/server.go:855 hands the stored Hello back by value), so appending
// into it could scribble on what the next candidate reads.
func withLiveTemplates(images []string, templates []*host.Snapshot) []string {
	if len(images) == 0 {
		return images
	}
	var extra []string
	for _, s := range templates {
		if s == nil || s.Image == "" || hasImage(images, s.Image) || hasImage(extra, s.Image) {
			continue
		}
		extra = append(extra, s.Image)
	}
	if len(extra) == 0 {
		return images
	}
	return append(slices.Clone(images), extra...)
}

// Fits reports whether this machine could take the request, and says why not
// when it could not. Nil means "nothing here rules it out", which is weaker
// than "yes" on purpose: the machine's own admission is the authority and runs
// again when the create arrives.
//
// Every check is skipped when the input it needs is missing, because unknown
// must never read as refused. A node that reported no image list, a request
// with no architecture, a machine whose first capacity report has not landed —
// each of those is a thing the gateway does not know, and refusing on it would
// make a machine unusable for a reason nobody could see.
func (c Candidate) Fits(req Request) error {
	const op = "create"
	if err := c.runnerFits(req); err != nil {
		return err
	}
	if req.Arch != "" && c.Facts.Arch != "" && req.Arch != c.Facts.Arch {
		return placementRefused(op, c.Name, ctlops.KindConflict, http.StatusConflict, fmt.Sprintf(
			"node %q is %s and this sandbox needs %s", c.Name, c.Facts.Arch, req.Arch),
			"Pick a machine with the right architecture, or leave --node off.")
	}
	if req.Image != "" && len(c.Facts.Images) > 0 && !hasImage(c.Facts.Images, req.Image) {
		return placementRefused(op, c.Name, ctlops.KindConflict, http.StatusConflict, fmt.Sprintf(
			"node %q does not have the %q image", c.Name, req.Image),
			"That machine can only build images it already holds; leave --node off to build here.")
	}
	// The RAM check charges what admission charges. Manager.effectiveMemMB
	// returns the per-VM working-set reserve when live overcommit is on and the
	// reserve is smaller than the ceiling, and both the node's admission and its
	// Capacity() report charge that figure — so comparing a raw MemMB against
	// BudgetMemMB would over-pack an overcommitting machine and under-pack one
	// asking for less than its reserve.
	//
	// A zero budget means no report has arrived yet, and a zero MemMB means the
	// caller wants that machine's default, which only that machine knows. Both
	// are unknowns, and both pass.
	if c.Capacity.BudgetMemMB > 0 && req.MemMB > 0 {
		need := effectiveMemMB(req.MemMB, c.Capacity.ReserveMemMB)
		if free := c.Capacity.BudgetMemMB - c.Capacity.EffectiveMemMB; need > free {
			return placementRefused(op, c.Name, ctlops.KindCapacity, http.StatusServiceUnavailable, fmt.Sprintf(
				"node %q is at capacity (%d/%d MB allocated, this needs %d)",
				c.Name, c.Capacity.EffectiveMemMB, c.Capacity.BudgetMemMB, need),
				"Pause something on that machine, or leave --node off to build here.")
		}
	}
	return nil
}

// runnerFits is the VMM half of Fits, split out because it is the one check
// that also has to run on the LOCAL candidate.
//
// Every other check in Fits is deliberately skipped for this machine — its own
// manager is the authority on whether it can take a sandbox, and a second
// opinion here could only disagree with it. The runner is different in kind:
// the manager is asked to build a sandbox, not to judge whether the environment
// that asked for it wanted this VMM. It has never been told what was required
// and cannot refuse on it, so if the gateway does not check here, a default
// create on a single-box deployment builds on whatever VMM happens to be
// installed and the requirement silently does nothing.
//
// It keeps Fits' unknown-is-not-refused rule: a machine that has not said what
// it runs is not turned away. That is a real state — a node linked by an older
// build, or one whose first capacity report has not landed — and refusing on it
// would take a working machine out of the fleet for a reason nobody can see.
func (c Candidate) runnerFits(req Request) error {
	const op = "create"
	if req.Runner == "" || c.Facts.Driver == "" {
		return nil
	}
	if req.Runner.SatisfiedBy(vmm.Runner(c.Facts.Driver)) {
		return nil
	}
	return placementRefused(op, c.Name, ctlops.KindConflict, http.StatusConflict, fmt.Sprintf(
		"node %q runs %s and this sandbox's environment requires %s",
		c.Name, c.Facts.Driver, req.Runner),
		"Pick a machine running that VMM, or change the environment's runner with `ctl env set <name> --runner`.")
}

// effectiveMemMB mirrors host.Manager.effectiveMemMB. It is duplicated rather
// than exported from there because the input is another machine's reserve,
// arriving in its capacity report, and the manager's method reads this
// machine's configuration.
func effectiveMemMB(memMB, reserveMB int64) int64 {
	if reserveMB > 0 && reserveMB < memMB {
		return reserveMB
	}
	return memMB
}

func hasImage(images []string, want string) bool {
	for _, i := range images {
		if i == want {
			return true
		}
	}
	return false
}

// defaultPlacer is M2's whole placement policy: honour an explicit --node, and
// otherwise build here.
//
// "Otherwise build here" is not a placeholder for a scheduler — it is the
// behaviour a single-box deployment has always had, and it has to survive
// unchanged through every later milestone, because a gateway that starts
// spreading sandboxes across machines the day a second one joins would move a
// user's work without being asked.
type defaultPlacer struct{}

func (defaultPlacer) Place(req Request, nodes []Candidate) (string, error) {
	const op = "create"
	if req.PreferNode == "" {
		for _, c := range nodes {
			if c.Local {
				// "Otherwise build here" still, but not when here is the wrong
				// VMM: that is the one thing this machine's own manager cannot
				// catch on the way past.
				if err := c.runnerFits(req); err != nil {
					return "", err
				}
				return c.Name, nil
			}
		}
		// Unreachable through Fleet, which always offers itself. A placer given
		// no machine at all still has to answer something a user can read.
		return "", ctlops.Disabled(op, "this gateway has no machine to build a sandbox on.")
	}
	for _, c := range nodes {
		if c.Name != req.PreferNode {
			continue
		}
		if c.Local {
			// This machine, named explicitly. Its manager is the authority on
			// whether it can take the sandbox and raises the refusals the
			// gateway already renders in full — a second opinion here could only
			// disagree with it. Except about the VMM, which it was never told.
			if err := c.runnerFits(req); err != nil {
				return "", err
			}
			return c.Name, nil
		}
		if !c.Online {
			return "", nodeOffline(op, c.Name)
		}
		if err := c.Fits(req); err != nil {
			return "", err
		}
		return c.Name, nil
	}
	// A name no machine in this fleet answers to. This is the same masked-free
	// answer `ctl node` gives: node names are not a tenant's secret — an
	// operator publishes them so people can place work — so there is nothing to
	// mask, and a user who mistypes one needs to be told.
	return "", ctlops.NotFound(op, "node", req.PreferNode)
}

// RemoteOnlyPlacer is the placement policy for a gateway that deliberately
// holds no VMs. It never returns the local candidate: an ordinary create goes
// to the first online remote machine that fits, while an explicit --node is
// still honoured when it names an eligible remote machine.
//
// Candidates arrive local-first and then name-sorted, so the choice is stable.
// This is intentionally a small deployment policy rather than a load-aware
// scheduler; the selected node remains the authority on live admission.
type RemoteOnlyPlacer struct{}

func (RemoteOnlyPlacer) Place(req Request, nodes []Candidate) (string, error) {
	const op = "create"
	if req.PreferNode != "" {
		for _, c := range nodes {
			if c.Name != req.PreferNode {
				continue
			}
			if c.Local {
				return "", ctlops.Disabled(op,
					"this gateway is control-plane-only and cannot hold sandboxes; choose a VM node")
			}
			if !c.Online {
				return "", nodeOffline(op, c.Name)
			}
			if err := c.Fits(req); err != nil {
				return "", err
			}
			return c.Name, nil
		}
		return "", ctlops.NotFound(op, "node", req.PreferNode)
	}

	var (
		sawRemote bool
		sawRunner bool
		firstFit  error
	)
	for _, c := range nodes {
		if c.Local {
			continue
		}
		sawRemote = true
		if !c.Online {
			continue
		}
		if c.runnerFits(req) == nil {
			sawRunner = true
		}
		if err := c.Fits(req); err != nil {
			if firstFit == nil {
				firstFit = err
			}
			continue
		}
		return c.Name, nil
	}
	// A required VMM that no online machine runs is answered once, about the
	// fleet, rather than by handing back whichever machine happened to be first
	// in the list. With four nodes and one requirement, firstFit's sentence
	// names an arbitrary machine and reads as a fact about that machine — the
	// user retries with --node on another one and gets the same shape of
	// refusal about a different name. The useful sentence is that nothing here
	// runs it.
	if req.Runner != "" && !sawRunner && sawRemote {
		return "", &ctlops.Error{
			Kind: ctlops.KindCapacity, Op: op, Code: "no_node_runs_runner",
			Msg: fmt.Sprintf("no online VM node runs %s, which this sandbox's environment requires", req.Runner),
			Hint: "Bring up a node with --driver " + req.Runner.String() +
				", or change the environment's runner with `ctl env set <name> --runner`.",
			Details:  map[string]any{"runner": req.Runner.String()},
			Verbatim: true,
			Exit:     1,
			Status:   http.StatusServiceUnavailable,
		}
	}
	if firstFit != nil {
		return "", firstFit
	}
	if sawRemote {
		return "", &ctlops.Error{
			Kind: ctlops.KindCapacity, Op: op, Code: "no_vm_node_online",
			Msg: "no VM node is online; wait for a node to reconnect and try again",
		}
	}
	return "", ctlops.Disabled(op,
		"this gateway has no VM nodes; enrol and approve one before creating a sandbox")
}

// placementRefused is the sentence a user reads when the machine they named
// cannot take the sandbox. Verbatim, because every word of it was chosen to say
// what to do next.
func placementRefused(op, node string, kind ctlops.Kind, status int, msg, hint string) *ctlops.Error {
	return &ctlops.Error{
		Kind:     kind,
		Op:       op,
		Code:     "node_cannot_place",
		Msg:      msg,
		Hint:     hint,
		Details:  map[string]any{"node": node},
		Verbatim: true,
		Exit:     1,
		Status:   status,
	}
}
