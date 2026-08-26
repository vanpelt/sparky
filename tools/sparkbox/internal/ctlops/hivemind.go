package ctlops

// `ctl sessions <name>` — what HiveMind has recorded from one sandbox.
//
// The query is live rather than served from the presence monitor's cache, and
// the difference is worth stating because the monitor already holds a snapshot.
// The monitor refreshes the catalog every ten minutes, which is the right
// cadence for something whose only consumer is a reaper lease and quite the
// wrong one for a person who just ran an agent and is asking whether it synced.
// Worse, the cache lives on the machine holding the VM, so serving from it
// would make this answerable only for local sandboxes — while the question
// costs one HTTP round trip from wherever the control plane happens to run.
//
// Authorization is the same as everywhere else in this package on the way in
// (owned() decides whose sandbox this is), and then narrower than anywhere else
// on the way out: the credential is minted for this sandbox alone, so even a
// bug here could only ever return the named VM's own sessions.

import (
	"context"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// HiveMind is the session-catalog slice of *hivemindpresence.Client. nil — the
// state of any host started without --hivemind-api — makes `sessions`
// KindDisabled rather than an empty list, because "this host does not ask" and
// "nothing has run here" are answers a user must not have to tell apart.
type HiveMind interface {
	Sessions(ctx context.Context, box *host.Sandbox, pageSize int) (host.HiveMindSessionSnapshot, error)
}

// SessionsTimeout bounds the mint → exchange → query chain. Three sequential
// HTTPS requests to a SaaS, one of which may be a cold exchange.
const SessionsTimeout = 30 * time.Second

// Sessions lists the HiveMind sessions attributed to one of the caller's
// sandboxes. pageSize <= 0 takes the client's default.
//
// It deliberately does not require the sandbox to be running: a paused VM's
// sessions are exactly what someone is most likely to be asking about, and the
// query never touches the guest.
func (o *Ops) Sessions(
	ctx context.Context,
	c Caller,
	name string,
	pageSize int,
) (host.HiveMindSessionSnapshot, error) {
	const op = "sessions"
	if o.hivemind == nil {
		return host.HiveMindSessionSnapshot{}, Disabled(op,
			"HiveMind session history isn't enabled on this host.")
	}
	box, err := o.owned(op, name, c)
	if err != nil {
		return host.HiveMindSessionSnapshot{}, err
	}
	if box.ID == "" {
		// Pre-identity records. There is no claim to bind a token to, so there
		// is no question to ask — and saying so beats a 403 from HiveMind.
		return host.HiveMindSessionSnapshot{}, Invalid(op, "no_sandbox_id",
			"%s predates workload identity, so HiveMind has nothing bound to it", name)
	}
	ctx, cancel := withBudget(ctx, SessionsTimeout)
	defer cancel()

	snapshot, err := o.hivemind.Sessions(ctx, box, pageSize)
	if err != nil {
		return host.HiveMindSessionSnapshot{}, &Error{
			Kind: KindUpstream, Op: op, Code: "hivemind_unreachable",
			Msg: err.Error(), Err: err,
		}
	}
	return snapshot, nil
}
