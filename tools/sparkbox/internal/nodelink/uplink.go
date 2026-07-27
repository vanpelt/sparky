package nodelink

// The node's outbound REQUEST channel: how a machine asks its gateway for
// something and waits for the answer.
//
// It is the deliberate opposite of the Emitter next door, and the two together
// are the whole of what a node initiates. An event is fire-and-forget because
// it is emitted from inside the manager's lock and nothing may block there. A
// request is the other shape entirely: it is made on an HTTP handler's
// goroutine, serving a guest that is sitting on an open socket waiting, and the
// answer IS the point — so it blocks, and it fails loudly when it cannot.
//
// What the two share is that they hold no link of their own. RunClient binds
// each to the live connection and unbinds it when that connection dies, so a
// node between reconnects is a node whose requests fail immediately with
// ErrNoLink rather than one that hangs until a deadline it inherited from
// somebody else's timeout.

import (
	"context"
	"errors"
	"sync"
)

// ErrNoLink is what an Uplink returns when this node has no gateway right now.
// It is a distinct error, and distinct from the link's own ErrLinkClosed,
// because it is the only failure here that is not a fault: a node reconnects
// on a jittered backoff forever, so "not linked" is a state every long-running
// node passes through routinely and callers are expected to degrade over
// rather than log a stack about.
var ErrNoLink = errors.New("nodelink: this node has no link to its gateway right now")

// Uplink is a handle on whatever link is currently up. Its zero value is a
// usable, permanently unlinked uplink — which is what a node running without a
// gateway configured has, and what makes the callers' nil checks unnecessary.
type Uplink struct {
	mu   sync.Mutex
	conn *Conn
}

// NewUplink returns an unlinked uplink. It is safe to hand to a server that
// starts before the first connection is made — which every node's does, since
// the metadata service must be listening before a guest boots.
func NewUplink() *Uplink { return &Uplink{} }

// Request sends one request to the gateway and waits for its reply.
//
// The caller's context is the whole budget: it rides the wire as a deadline so
// the gateway gives up fractionally first (see LinkMargin) and this side gets a
// typed refusal rather than a timeout it has to guess the meaning of.
//
// The connection is read out under the lock and used outside it. That is not a
// race worth closing: a link that dies between the read and the send fails the
// send, which is the same answer by a different route, and holding the lock for
// the duration of a round trip would mean one slow request blocking every
// other — including the detach that would have freed it.
func (u *Uplink) Request(ctx context.Context, typ string, body, out any) error {
	if u == nil {
		return ErrNoLink
	}
	u.mu.Lock()
	conn := u.conn
	u.mu.Unlock()
	if conn == nil {
		return ErrNoLink
	}
	return conn.Request(ctx, typ, body, out)
}

// Linked reports whether a gateway is reachable right now. It is advisory by
// nature — the link can die between this call and the next Request — and exists
// so a health surface can say "waiting for the gateway" instead of nothing.
func (u *Uplink) Linked() bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conn != nil
}

// attach binds the uplink to a live link and returns the function that unbinds
// it. The unbind only ever clears the connection it installed, so a link
// tearing itself down cannot detach the one that has already replaced it —
// the same rule, and the same failure it prevents, as the Emitter's.
func (u *Uplink) attach(conn *Conn) func() {
	u.mu.Lock()
	u.conn = conn
	u.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			if u.conn == conn {
				u.conn = nil
			}
			u.mu.Unlock()
		})
	}
}
