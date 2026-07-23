package sshgw

// The `node@` door: how a machine joins the fleet.
//
// It is the second door an unregistered key may open, and for the same reason
// the signup door exists — turning that key into an identity is the door's
// whole job. The difference is what enrolling buys: a signup produces an
// account that works, while a node enrols into a roster row that does nothing
// at all until an operator approves it. Reaching this door is not capacity.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
)

// NodeRoster is the fleet's node registry as this door needs it. Nil leaves the
// node door shut, which is what a single-box deployment is.
//
// List and Remove are in here rather than in a second, optional interface the
// door type-asserts for. Reclaiming the enrolment budget is the only thing
// standing between a flood of throwaway keys and a permanent lockout, and an
// optional interface makes "this roster cannot be pruned" a silent runtime
// answer: the protection compiles, the tests that inject their own roster pass,
// and the wiring in cmd/sparkbox quietly does nothing. Asking for the methods
// here turns that into a build failure.
type NodeRoster interface {
	Lookup(key xssh.PublicKey) (nodes.Node, bool)
	Enroll(name string, key xssh.PublicKey) (nodes.Node, error)
	Seen(name, arch, release string, at time.Time) error
	List() ([]nodes.Node, error)
	Remove(name string) error
}

// The roster cmd/sparkbox actually wires into this door is *nodes.Store. This
// line is what makes a drift between the two a compile error here rather than a
// feature that turns itself off on a real host.
var _ NodeRoster = (*nodes.Store)(nil)

// NodeJoiner is the fleet's half of the door: everything past authentication.
// It is an interface rather than a concrete fleet so this package keeps not
// importing one, and so a test can stand the door up without a fleet at all.
type NodeJoiner interface {
	// ServeLink owns the link for its lifetime and returns when it ends.
	// opts.Node is the AUTHENTICATED roster name; the name in the hello is
	// advisory. A nil error means the link merely ended.
	ServeLink(ctx context.Context, opts nodelink.ServerOptions) error
}

// nodeDoorOpen reports whether this gateway has a fleet to join a node to.
// Both halves are required: a roster with nobody to hand the link to would
// enrol machines it could never serve, which reads to an operator as a node
// that connects and then does nothing.
func (g *Gateway) nodeDoorOpen() bool { return g.nodes != nil && g.joiner != nil }

// isNodeDoor reports whether a connection is aimed at the node door. It mirrors
// isSignupDoor with one difference: a front-door address is never the node
// door, because `node` mints no front door and a node dials the gateway's SSH
// listener by name. It runs during the public-key check, before a session
// exists, which is why it takes the username and destination directly.
func (g *Gateway) isNodeDoor(user string, local net.Addr) bool {
	if _, _, inRange := g.resolveDoor(local); inRange {
		return false
	}
	return user == NodeUser
}

// handleNodeLink serves one `ssh node@gateway sparkbox-link/1` session: check
// the command, recover the SSH connection, read the hello, apply roster policy,
// and hand what is left to the fleet.
func (g *Gateway) handleNodeLink(s gssh.Session, log *slog.Logger) {
	if cmd := s.Command(); len(cmd) != 1 || cmd[0] != nodelink.LinkCommand {
		// A human who typed `ssh node@gateway` gets told what this door is
		// rather than a silent hang, which is the only reason the command is
		// checked against a literal instead of being ignored.
		fmt.Fprintf(s.Stderr(),
			"sparkbox: this is the fleet link door — a sparkbox node connects here by running %q.\r\n"+
				"Start one with:  sparkbox serve --gateway %s:2222 --node-name <name>\r\n",
			nodelink.LinkCommand, g.domainHint())
		s.Exit(2) //nolint:errcheck
		return
	}

	// The gateway opens data channels back toward the node on this connection.
	// gliderlabs stores it on the session context and offers no accessor; there
	// is no other route to it, and without it the link could carry control
	// frames but never a stream.
	conn, _ := s.Context().Value(gssh.ContextKeyConn).(xssh.Conn)
	if conn == nil {
		log.Error("node link has no ssh connection to open data channels on")
		_ = nodelink.SendBye(s, nodelink.CodeProtocolError, "this session has no ssh connection")
		s.Exit(1) //nolint:errcheck
		return
	}

	greeting, err := nodelink.ReadHello(s.Context(), s, nodelink.HelloTimeout)
	if err != nil {
		// Rate-limited: an unapproved peer decides how often this happens, and a
		// peer that opens sessions in a loop must not get to choose how fast this
		// gateway writes to its disk. Same for the refusal below.
		doorNoise.warn(log, "node link did not introduce itself", "err", err)
		_ = nodelink.SendBye(s, nodelink.CodeProtocolError, err.Error())
		s.Exit(2) //nolint:errcheck
		return
	}

	row, refusal := g.admitNode(s, greeting.Hello, log)
	if refusal != nil {
		doorNoise.info(log, "node link refused", "asked_for", greeting.Hello.Node, "code", refusal.Code)
		_ = nodelink.Refuse(s, greeting, refusal)
		s.Exit(1) //nolint:errcheck
		return
	}
	// Admitted, so the connection stops being on the clock: an approved node's
	// link is the one thing on this listener that is meant to outlive every
	// timeout. (Approval is checked before the watchdog is armed, so this is
	// belt and braces rather than the common path.)
	disarmProbation(s.Context())

	log = log.With("node", row.Name)
	// Arch and release are node-authored and display-only; nothing authorizes
	// on them, so a failure to record them is not a reason to refuse the link.
	if err := g.nodes.Seen(row.Name, greeting.Hello.Arch, greeting.Hello.Release, time.Now()); err != nil {
		log.Warn("could not record a node's connection", "err", err)
	}

	// The welcome is built here rather than by the fleet because the upstream
	// key and the domain are this gateway's identity: shipping them into the
	// fleet only to be handed back would be a second copy of it.
	err = g.joiner.ServeLink(s.Context(), nodelink.ServerOptions{
		Node:     row.Name,
		Greeting: greeting,
		Session:  s,
		Stderr:   s.Stderr(),
		Conn:     conn,
		Welcome: nodelink.Welcome{
			GatewayUpstreamPub: PublicKeyLine(g.upstreamKey),
			Domain:             g.domain,
		},
		Log: log,
	})
	if err != nil {
		log.Info("node link ended in error", "err", err)
		s.Exit(1) //nolint:errcheck
		return
	}
	s.Exit(0) //nolint:errcheck
}

// admitNode is the roster policy: it returns the row this link runs as, or the
// refusal to send back. Enrolling an unknown key happens here and is itself a
// refusal — the node is told it exists and is waiting, which is the only thing
// first contact ever earns.
func (g *Gateway) admitNode(s gssh.Session, hello nodelink.Hello, log *slog.Logger) (nodes.Node, *ctlops.Error) {
	if hello.Protocol != nodelink.Protocol {
		return nodes.Node{}, nodelink.Refusal(nodelink.CodeProtocolUnsupported,
			"this gateway speaks node protocol %d, not %d.", nodelink.Protocol, hello.Protocol)
	}
	key := sessionKey(s)
	if key == nil {
		return nodes.Node{}, nodelink.Refusal(nodelink.CodeProtocolUnsupported,
			"this session has no public key.")
	}

	row, known := sessionNode(s)
	if !known {
		// The key resolved to a user account, so the public-key check took the
		// user branch and never looked at the roster. Ask it now: a key that is
		// both is still a node here.
		row, known = g.nodes.Lookup(key)
	}
	if !known {
		return g.enrolNode(key, hello, log)
	}

	switch row.Status {
	case nodes.StatusApproved:
	case nodes.StatusDisabled:
		return nodes.Node{}, nodelink.Refusal(nodelink.CodeNodeDisabled,
			"node %q is disabled on this gateway.", row.Name)
	default:
		// A machine that is still waiting knocks again every minute or so, and
		// that knock is the only evidence this gateway has that it is still out
		// there. Recording it is what lets the door tell a machine that is
		// waiting from one that was abandoned — see recycleEnrolment — and it is
		// what puts a live "last seen" next to the row an operator is deciding
		// about.
		g.noteContact(row, hello, log)
		return nodes.Node{}, g.pendingRefusal(row)
	}
	if hello.Node != row.Name {
		// The row is authoritative: an operator approved a name they read off
		// the roster, and letting a node rename itself afterwards would move
		// that approval to a name nobody looked at.
		return nodes.Node{}, nodelink.Refusal(nodelink.CodeNodeNameMismatch,
			"this key is registered as node %q, not %q — the roster name is the one that counts.",
			row.Name, hello.Node)
	}
	return row, nil
}

// enrolNode records a machine's first contact and refuses it. The refusal is
// the point: enrolling is how an operator learns a name and a fingerprint to
// approve, and it must never be the thing that grants capacity.
func (g *Gateway) enrolNode(key xssh.PublicKey, hello nodelink.Hello, log *slog.Logger) (nodes.Node, *ctlops.Error) {
	if !g.nodeEnrol {
		return nodes.Node{}, nodelink.Refusal(nodelink.CodeNodeEnrolFull,
			"this gateway is not accepting new nodes.")
	}
	row, err := g.nodes.Enroll(hello.Node, key)
	if errors.Is(err, nodes.ErrTooManyPending) && g.recycleEnrolment(time.Now(), log) {
		row, err = g.nodes.Enroll(hello.Node, key)
	}
	switch {
	case err == nil:
	case errors.Is(err, nodes.ErrBadName):
		return nodes.Node{}, nodelink.Refusal(nodelink.CodeBadNodeName,
			"%q is not a usable node name: up to 63 characters of a-z, 0-9 and dashes, starting with a letter or digit.",
			hello.Node)
	case errors.Is(err, nodes.ErrNameTaken):
		// The holder is named rather than swapped out: a pending row grants
		// nothing, but it is also the fingerprint an operator will compare, and
		// letting the newest claimant take a name would let a stranger put
		// their key in front of the one the operator is waiting for.
		return nodes.Node{}, nodelink.Refusal(nodelink.CodeNodeNameTaken,
			"node %q is registered to a different key. If that key is not yours, an operator runs:  ssh %s@%s node rm %s",
			hello.Node, ControlUser, g.domainHint(), hello.Node)
	case errors.Is(err, nodes.ErrTooManyPending):
		// Every waiting row belongs to a machine that is still knocking, so
		// there is nothing here to reclaim. Saying so — rather than "full" —
		// is what stops an operator from hunting for a limit to raise when
		// what they are looking at is a queue somebody is holding open.
		return nodes.Node{}, nodelink.Refusal(nodelink.CodeNodeEnrolFull,
			"too many machines are waiting for approval on this gateway, and all of them are still knocking. "+
				"A waiting row is only reclaimed after its machine has been quiet for %s; "+
				"an operator can make room now with:  ssh %s@%s node ls",
			nodeEnrolStale, ControlUser, g.domainHint())
	default:
		log.Error("could not enrol a node", "asked_for", hello.Node, "err", err)
		return nodes.Node{}, ctlops.Fail(nodelink.OpLink, err)
	}
	// Unlike the refusals above, this line is not something a stranger can spin:
	// enrolment is capped by the roster ceiling, and past that ceiling it costs a
	// row that has gone quiet for nodeEnrolStale. It is also the line an operator
	// greps for when the machine they just brought up has not appeared, so it is
	// deliberately not rate-limited.
	log.Info("node enrolled and awaiting approval", "node", row.Name, "fp", row.FP,
		"approve_with", fmt.Sprintf("ssh %s@%s node approve %s", ControlUser, g.domainHint(), row.Name))
	g.noteContact(row, hello, log)
	return nodes.Node{}, g.pendingRefusal(row)
}

// noteContact records that a machine that is still waiting for approval knocked,
// along with what it says it is. Arch and release are node-authored and
// display-only — nothing authorizes on them — and a row that has gone away
// between the lookup and here is not a reason to fail a refusal that is already
// decided, so every error is a log line and nothing more.
func (g *Gateway) noteContact(row nodes.Node, hello nodelink.Hello, log *slog.Logger) {
	if err := g.nodes.Seen(row.Name, hello.Arch, hello.Release, time.Now()); err != nil {
		doorNoise.warn(log, "could not record a waiting node's knock", "node", row.Name, "err", err)
	}
}

// nodeEnrolStale is how long a machine that is waiting for approval must have
// gone without knocking before this door will reclaim its row.
//
// A node retries forever with an exponential backoff that tops out at
// nodelink.DefaultBackoffMax (a minute, plus jitter), so a machine that is
// actually trying to join is heard from many times over inside this window and
// a machine that has fallen silent for it is, as far as this gateway can tell,
// gone. The generous margin is the point: the cost of reclaiming too eagerly is
// far higher than the cost of holding a dead row a few minutes longer.
const nodeEnrolStale = 10 * time.Minute

// recycleEnrolment frees one waiting slot by dropping a row whose machine has
// stopped knocking, and reports whether it managed to.
//
// Two failures pull in opposite directions here and both are real. Anyone who
// can reach the listener can offer a key, and nodes.MaxPending is a ceiling on
// rows rather than on people, so a stranger with 32 throwaway keys can fill the
// roster and — if nothing is ever reclaimed — lock every genuine machine out of
// enrolment for good. But a row is not just a slot: it is also the name that
// `node approve <name>` is keyed on, and it is the ErrNameTaken that keeps that
// name attached to one key. Reclaim a row and the name goes back on the market;
// reclaim the wrong row and a stranger can put their own key behind the name an
// operator is about to say yes to, which turns an approval into a machine
// nobody meant to admit.
//
// What resolves it is that the operator is waiting on a specific machine, and
// that machine is knocking. So the door reclaims by silence, not by age: the
// candidate is the pending row that has gone longest without contact, and only
// if that silence has run past nodeEnrolStale. A machine that is trying to join
// refreshes its own row every time it retries (see noteContact) and therefore
// can never be the row that is dropped, which is exactly the guarantee approval
// needs. A fire-and-forget flood, by contrast, goes quiet the moment it is over
// and ages out on its own without an operator having to prune anything.
//
// The residue is a stranger who keeps all 32 rows knocking: enrolment then
// stays closed for as long as they keep it up, and the refusal above says so.
// That is a denial of service somebody has to keep paying for, which is a
// strictly better failure than handing them a name.
//
// Only pending rows are ever candidates. An approved or disabled row is a
// decision somebody made and is never touched here.
func (g *Gateway) recycleEnrolment(now time.Time, log *slog.Logger) bool {
	rows, err := g.nodes.List()
	if err != nil {
		doorNoise.warn(log, "could not read the roster to recycle an enrolment slot", "err", err)
		return false
	}
	var quietest *nodes.Node
	var quietestAt time.Time
	for i, row := range rows {
		if row.Status != nodes.StatusPending {
			continue
		}
		if at := lastContact(row); quietest == nil || at.Before(quietestAt) {
			quietest, quietestAt = &rows[i], at
		}
	}
	if quietest == nil {
		return false
	}
	if quiet := now.Sub(quietestAt); quiet < nodeEnrolStale {
		doorNoise.warn(log, "the enrolment queue is full and every machine in it is still knocking",
			"waiting", nodes.MaxPending, "quietest", quietest.Name, "quiet_for", quiet.Round(time.Second))
		return false
	}
	if err := g.nodes.Remove(quietest.Name); err != nil {
		doorNoise.warn(log, "could not recycle an enrolment slot", "node", quietest.Name, "err", err)
		return false
	}
	log.Info("dropped an unapproved node that stopped knocking, to make room for another",
		"node", quietest.Name, "fp", quietest.FP, "last_contact", quietestAt)
	return true
}

// lastContact is when a roster row's machine was last heard from.
//
// LastSeen is stamped on every connection a machine makes, including the refused
// ones it makes while it waits, so for a row this door wrote it is always set.
// FirstSeen is the fallback for a row that reached the roster some other way and
// has not been connected to since.
//
// Both stamps are written from this gateway's own clock and neither is anything
// a peer can influence, which is what makes silence usable as evidence at all:
// a machine cannot make its row look fresh except by connecting, and cannot make
// another machine's row look abandoned at any price.
func lastContact(n nodes.Node) time.Time {
	if n.LastSeen != nil {
		return *n.LastSeen
	}
	return n.FirstSeen
}

// nodeAdmissionBudget is everything a machine this gateway has not approved may
// hold a connection for: the handshake it has already finished, the session it
// has yet to open, the hello it owes us and the refusal it is about to be sent.
// Generous, because it is a backstop rather than a protocol deadline — the
// hello has its own, tighter one — and a machine on a bad link should still get
// far enough to be told what is wrong with it.
const nodeAdmissionBudget = 30 * time.Second

// probate puts a connection on the clock. Until it is disarmed, the connection
// is closed outright when the budget runs out.
//
// It exists because nothing else here bounds an unapproved peer. The gliderlabs
// server sets no session timeouts on purpose (a node's link must outlive them),
// a refused session's Exit does not close the connection it arrived on, and a
// peer that authenticates and then opens nothing at all never reaches a handler
// that could hang up on it. The raw net.Conn is the only handle that covers all
// three, which is why Server stashes it.
func (g *Gateway) probate(ctx gssh.Context) {
	conn, _ := ctx.Value(rawConnKey).(net.Conn)
	if conn == nil {
		return // no handle, nothing to promise; the peer is still refused
	}
	disarmed := make(chan struct{})
	var once sync.Once
	ctx.SetValue(probationKey, func() { once.Do(func() { close(disarmed) }) })
	budget := g.admissionBudget
	go func() {
		t := time.NewTimer(budget)
		defer t.Stop()
		select {
		case <-disarmed:
		case <-ctx.Done(): // the connection ended on its own
		case <-t.C:
			// Rate-limited for the same reason as the door's other lines: this one
			// is written once per connection a stranger opens and abandons, which
			// is the cheapest thing there is to open and abandon.
			doorNoise.warn(g.log, "hanging up on a machine that never got past the node door",
				"remote", conn.RemoteAddr(), "after", budget)
			conn.Close() //nolint:errcheck
		}
	}()
}

// disarmProbation lifts the admission clock off a connection. Safe on a
// connection that was never on it.
func disarmProbation(ctx gssh.Context) {
	if disarm, ok := ctx.Value(probationKey).(func()); ok {
		disarm()
	}
}

// pendingRefusal names the exact command that unblocks the node. The node logs
// this sentence and that log line is the only place anyone will look.
//
// It ends by pointing at the fingerprint because approval is keyed on the name
// and the name is only ever a label: this sentence is printed on the machine
// that owns the key, so the fingerprint in it is the one thing an operator can
// compare out of band against what the roster shows before they say yes.
func (g *Gateway) pendingRefusal(row nodes.Node) *ctlops.Error {
	return nodelink.Refusal(nodelink.CodeNodePending,
		"node %q (%s) is waiting for approval. An operator runs:  ssh %s@%s node approve %s  "+
			"— after checking that `node ls` lists this machine's fingerprint against that name.",
		row.Name, row.FP, ControlUser, g.domainHint(), row.Name)
}

// doorNoise bounds the log lines a machine this gateway has not approved can
// make the node door write.
//
// Every one of those lines is written on an unapproved peer's command: it opens
// a session, says the wrong thing or nothing at all, and the door narrates it.
// Unbounded, that is a peer choosing how fast the gateway writes to its own
// disk, which costs the peer a TCP connection and the operator a filesystem. The
// bound is a token bucket rather than a mute switch because the first lines are
// the ones with the diagnostic value in them and the thousandth is a duplicate.
//
// Burst is nodes.MaxPending because that is the most machines that can
// legitimately be waiting at this door at once, so a whole fleet arriving
// together is still narrated in full. Refill is deliberately slow: past the
// burst, this is no longer news.
//
// It is package state rather than per-Gateway state because the resource being
// protected is the process's log, and a process serves one gateway.
var doorNoise = newNoiseLimiter(nodes.MaxPending, 2*time.Second)

// noiseLimiter is a token bucket over log lines. It counts what it drops so the
// next line that gets through can say how much was suppressed behind it —
// silence that does not admit to being silence is how a flood becomes invisible.
type noiseLimiter struct {
	burst int
	every time.Duration

	mu      sync.Mutex
	tokens  int
	last    time.Time // when the bucket was last refilled
	dropped int
}

func newNoiseLimiter(burst int, every time.Duration) *noiseLimiter {
	return &noiseLimiter{burst: burst, every: every, tokens: burst}
}

// take spends one token, reporting how many lines were dropped since the last
// one it let through.
func (n *noiseLimiter) take(now time.Time) (dropped int, ok bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.last.IsZero() {
		n.last = now
	}
	if gained := int(now.Sub(n.last) / n.every); gained > 0 {
		n.tokens = min(n.tokens+gained, n.burst)
		n.last = n.last.Add(time.Duration(gained) * n.every)
	}
	if n.tokens <= 0 {
		n.dropped++
		return 0, false
	}
	n.tokens--
	dropped, n.dropped = n.dropped, 0
	return dropped, true
}

// warn writes one line at Warn level if the bucket allows it.
func (n *noiseLimiter) warn(log *slog.Logger, msg string, args ...any) {
	dropped, ok := n.take(time.Now())
	if !ok {
		return
	}
	if dropped > 0 {
		args = append(args, "suppressed_since", dropped)
	}
	log.Warn(msg, args...)
}

// info writes one line at Info level if the bucket allows it.
func (n *noiseLimiter) info(log *slog.Logger, msg string, args ...any) {
	dropped, ok := n.take(time.Now())
	if !ok {
		return
	}
	if dropped > 0 {
		args = append(args, "suppressed_since", dropped)
	}
	log.Info(msg, args...)
}

// reset refills the bucket. Only a test calls it, so that what it measures is
// this door's behaviour rather than what the test before it happened to log.
func (n *noiseLimiter) reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tokens, n.dropped, n.last = n.burst, 0, time.Time{}
}

// sessionNode is the roster row the public-key check resolved this session's
// key to, when it took the node branch.
func sessionNode(s gssh.Session) (nodes.Node, bool) {
	n, ok := s.Context().Value(authedNodeKey).(nodes.Node)
	return n, ok
}
