package sshgw

// The operator's half of the fleet: `ctl@ node ls | approve <name> | rm <name>`.
//
// The other half is nodedoor.go, where a machine enrols itself and is told to
// wait. These two files are the whole trust ceremony — the node offers a key
// and a name, an operator reads the fingerprint off this listing, compares it
// out of band, and says yes. Nothing here decides who may do that: like every
// other command on this channel, the policy is ctlops', and what stays here is
// the argument grammar and the sentences.

import (
	"fmt"
	"log/slog"
	"strings"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// controlNode manages the machines this gateway runs sandboxes on.
func (g *Gateway) controlNode(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	// Asked up front rather than per-subcommand, so an unknown subcommand on a
	// single-box host still says the honest thing — the same shape, and for the
	// same reason, as controlSchedule.
	if !g.ops.Capabilities().Fleet {
		fmt.Fprint(s.Stderr(), "sparkbox: this host is not a fleet gateway.\r\n")
		s.Exit(1) //nolint:errcheck
		return
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	// `ls` is what the usage line documents and `list` is what every other
	// collection on this channel is called; accepting both costs one case and
	// saves an operator an exit 2 for guessing the wrong one.
	case "list", "ls":
		nodes, err := g.ops.ListNodes(s.Context(), c)
		if err != nil {
			failCtl(s, log, "node ls", err)
			return
		}
		if len(nodes) == 0 {
			fmt.Fprintf(s, "no machines in this fleet — a node joins with:\r\n"+
				"  sparkbox serve --gateway %s:2222 --node-name <name>\r\n", g.domainHint())
			s.Exit(0) //nolint:errcheck
			return
		}
		mixed := mixedEgress(nodes)
		for _, n := range nodes {
			fmt.Fprint(s, nodeLine(n, mixed))
		}
		s.Exit(0) //nolint:errcheck
	case "approve":
		if len(args) < 2 {
			// The usage line names the fingerprint and says where to read it,
			// because the obvious guess is the name in the line above it and
			// the whole point of this command is that the name is not enough.
			fmt.Fprintf(s.Stderr(),
				"usage: ssh %s@<gateway> node approve <SHA256:...>\r\n"+
					"Approve a machine by the fingerprint of its key, which it prints at startup — "+
					"compare that against `node ls` first.\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		n, err := g.ops.ApproveNode(s.Context(), c, args[1])
		if err != nil {
			failCtl(s, log, "node approve", err)
			return
		}
		fmt.Fprintf(s, "approved %s (%s) — it can carry sandboxes now\r\n", n.Name, n.FP)
		s.Exit(0) //nolint:errcheck
	case "rm":
		if len(args) < 2 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> node rm <name>\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		if err := g.ops.RemoveNode(s.Context(), c, args[1]); err != nil {
			failCtl(s, log, "node rm", err)
			return
		}
		// Removal revokes the approval; it does not blacklist the key. Saying so
		// is what stops an operator from hunting for a ban list that does not
		// exist when the machine reappears as pending.
		fmt.Fprintf(s, "removed node %q — it may enrol again and wait for approval\r\n", args[1])
		s.Exit(0) //nolint:errcheck
	default:
		fmt.Fprintf(s.Stderr(), "unknown node command %q\r\n%s", sub, controlUsage)
		s.Exit(2) //nolint:errcheck
	}
}

// nodeLine renders one machine, columns padded and then right-trimmed: the
// local machine has no fingerprint to print, and a line of trailing spaces in a
// terminal is invisible until somebody copies it.
//
// flagUnmetered marks the machines running no egress gateway. It is decided by
// the CALLER from the whole listing rather than per row — see mixedEgress.
func nodeLine(n ctlops.NodeInfo, flagUnmetered bool) string {
	presence := "offline"
	if n.Online {
		presence = "online"
	}
	arch := n.Arch
	if arch == "" {
		arch = "-"
	}
	// Shared with the `node rm` refusal, which quotes the same count back at the
	// operator who just read this column.
	boxes := ctlops.CountSandboxes(n.Sandboxes)
	name := n.Name
	if n.Local {
		// The gateway is in its own listing because a fleet's capacity includes
		// the machine the fleet runs on, and an operator reading a two-line list
		// has to be able to tell which one they are logged into.
		name += " (this gateway)"
	}
	egress := ""
	if flagUnmetered && !n.Egress {
		egress = " no-egress-control"
	}
	line := fmt.Sprintf("%-28s %-9s %-8s %-8s %-13s %s",
		name, n.Status, presence, arch, boxes, n.FP)
	return strings.TrimRight(line, " ") + egress + "\r\n"
}

// mixedEgress reports whether this fleet meters some machines and not others.
//
// It is the condition worth a word in a listing, and the only one. A fleet
// where nothing meters is a deployment choice — `doctor` says so once, in the
// place an operator goes to be told what is not configured — and repeating it
// on every row would be noise on the common case. A fleet where SOME machines
// meter is different: it is the state that produces a bandwidth panel of
// zeroes for one owner's VM and real numbers for their colleague's, with
// nothing anywhere else saying why.
func mixedEgress(nodes []ctlops.NodeInfo) bool {
	var metered, bare int
	for _, n := range nodes {
		// Only machines that are actually attached have reported. A pending or
		// offline row has no facts, so counting it as unmetered would flag
		// every fleet with a machine that has not linked yet.
		if !n.Online {
			continue
		}
		if n.Egress {
			metered++
		} else {
			bare++
		}
	}
	return metered > 0 && bare > 0
}
