package nodelink

// What a node does when its gateway asks it to operate a sandbox.
//
// Every handler here is a thin forward to this machine's own host.Manager, and
// the thinness is the design. The node performs NO ownership check and NO name
// policy: both are the gateway's, always, and a second copy of either here
// would be a second answer that could disagree. What the node does re-run is
// everything that is its own hardware's business — its RAM admission, its disk
// quota, whether that name exists on this disk at all — so a refusal comes from
// the machine that would have had to find the resources, as the concrete
// *host.CapacityError or *host.DiskQuotaError the gateway's renderers already
// switch on.
//
// Errors leave as they are raised. Conn.reply already projects whatever a
// handler returns through ctlops.ToWire (conn.go), so a handler that
// pre-projected would wrap the error twice and the far side would rebuild a
// *ctlops.Error wrapping a *ctlops.Error.

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// bookkeepingQueue bounds the two fire-and-forget events waiting to be applied.
//
// It is the same shape as the emitter's queue and for the same reason: the work
// is lossy on purpose. A dropped touch costs one sandbox a slightly stale
// last_active — which the reaper reads, and which the next session teardown
// rewrites anyway — and the alternative to dropping is holding the goroutine
// that offered it, which here is the link's reader.
const bookkeepingQueue = 256

// registerOps installs the lifecycle verbs on a node's control channel.
//
// The two events (touch and record_key) do not run inline. Conn.dispatch
// delivers an event on the READ goroutine, and Manager.Touch takes the
// manager's lock and rewrites sandboxes.json — so applying it there would put a
// disk write, plus a wait on whatever lifecycle operation currently holds that
// lock, in front of every frame queued behind it: replies, heartbeat answers
// and the gateway's liveness ping included. A node whose only fault is being
// busy would then fail to answer two pings and be hung up on. One goroutine
// draining a bounded queue keeps the ordering (the queue is FIFO, so a touch
// and the record_key that follows it apply in the order the gateway sent them)
// and costs the reader nothing.
//
// ctx is the LINK's context, not the process's: bookkeeping queued against a
// link that has died is stale, and the gateway resends nothing.
func registerOps(ctx context.Context, conn *Conn, mgr Manager, log *slog.Logger) {
	handle(conn, TypeCreate, func(ctx context.Context, req CreateReq) (SandboxResp, error) {
		b, err := mgr.Create(ctx, req.Name, req.Owner, req.Image, req.VCPUs, req.MemMB)
		if err != nil {
			return SandboxResp{}, err
		}
		return SandboxResp{Sandbox: sandboxRow(b)}, nil
	})

	handle(conn, TypeEnsureRunning, func(ctx context.Context, req NameReq) (SandboxResp, error) {
		b, err := mgr.EnsureReady(ctx, req.Name)
		if err != nil {
			return SandboxResp{}, err
		}
		return SandboxResp{Sandbox: sandboxRow(b)}, nil
	})

	handle(conn, TypePause, func(ctx context.Context, req NameReq) (EmptyResp, error) {
		return EmptyResp{}, mgr.Pause(ctx, req.Name)
	})

	handle(conn, TypeArchive, func(ctx context.Context, req NameReq) (EmptyResp, error) {
		return EmptyResp{}, mgr.Archive(ctx, req.Name)
	})

	handle(conn, TypeResize, func(ctx context.Context, req ResizeReq) (EmptyResp, error) {
		return EmptyResp{}, mgr.Resize(ctx, req.Name, req.SizeMB)
	})

	handle(conn, TypeReboot, func(ctx context.Context, req NameReq) (EmptyResp, error) {
		return EmptyResp{}, mgr.Reboot(ctx, req.Name)
	})

	handle(conn, TypeTurbo, func(ctx context.Context, req TurboReq) (EmptyResp, error) {
		return EmptyResp{}, mgr.SetTurbo(ctx, req.Name, req.On)
	})

	// Rename is the one verb that does not forward wholesale, and the split is
	// worth stating because it is invisible from either half.
	//
	// Manager.Rename fuses two jobs: the node-local one (move the VM directory,
	// drop the snapshots that named the old box) and the gateway-owned one (the
	// route, schedule, tag and front-door rows that follow a sandbox's name).
	// It treats routes.RenameSandbox as fatal-with-rollback because a route row
	// carries the Owner column that gates private-route auth, so a route left
	// under the old name is an authorization record pointing at a sandbox that
	// no longer exists.
	//
	// On a node every one of those side stores is nil and the manager's own nil
	// guards skip them, which is correct: they live on the gateway, and the
	// gateway does its half — including moving the placement row and rolling it
	// back if this call fails — before it ever sends this frame.
	handle(conn, TypeRename, func(ctx context.Context, req RenameReq) (EmptyResp, error) {
		return EmptyResp{}, mgr.Rename(ctx, req.Name, req.NewName, req.Owner)
	})

	handle(conn, TypeDestroy, func(ctx context.Context, req NameReq) (EmptyResp, error) {
		return EmptyResp{}, mgr.Destroy(ctx, req.Name)
	})

	// SetPinned is the one manager call that takes no context, so the deadline
	// the gateway sent is honoured by hand: an operation whose budget has
	// already run out must not still be applied, because the caller has stopped
	// waiting and would never learn that it was.
	handle(conn, TypeSetPinned, func(ctx context.Context, req PinReq) (EmptyResp, error) {
		if err := ctx.Err(); err != nil {
			return EmptyResp{}, err
		}
		return EmptyResp{}, mgr.SetPinned(req.Name, req.Pinned)
	})

	// ResyncEnv reports nothing at all — a machine that could not push an
	// environment logs it and the next resync fixes it — so the reply is the
	// bare acknowledgement that the node heard the request.
	handle(conn, TypeResyncEnv, func(ctx context.Context, req NameReq) (EmptyResp, error) {
		mgr.ResyncEnv(ctx, req.Name)
		return EmptyResp{}, nil
	})

	// Vitals is the only read here, and the only verb a viewer rather than an
	// operator drives: an open browser terminal asks once a second for as long
	// as the tab is in front of somebody. It stays a request-reply like the
	// rest — a reading nobody is waiting for is worth nothing, so there is
	// nothing for a node to push — and it deliberately resolves through the
	// manager's own readers, which answer "no reading" for a sandbox that is
	// paused or not on this machine rather than raising. Watching must not
	// wake anything: nothing on this path touches last_active.
	handle(conn, TypeVitals, func(ctx context.Context, req NameReq) (VitalsResp, error) {
		v, err := mgr.Vitals(ctx, req.Name)
		if err != nil {
			return VitalsResp{}, err
		}
		resp := VitalsResp{
			CPUSeconds:     v.CPUSeconds,
			MemUsedMB:      v.MemUsedMB,
			NetRxBytes:     v.NetRxBytes,
			NetTxBytes:     v.NetTxBytes,
			ListeningPorts: v.ListeningPorts,
			PortServices:   v.PortServices,
			PortsChecked:   v.PortsChecked,
		}
		if v.HiveMind != nil {
			resp.HiveMind = &HiveMindLive{
				SessionTitle: v.HiveMind.SessionTitle,
				SessionURL:   v.HiveMind.SessionURL,
			}
			if p := v.HiveMind.Presence; p != nil {
				resp.HiveMind.Presence = p.State
				resp.HiveMind.ProtectUntil = p.ProtectUntil
				resp.HiveMind.ObservedAt = p.ObservedAt
			}
		}
		return resp, nil
	})

	handle(conn, TypeSnapshotCreate, func(ctx context.Context, req SnapshotReq) (SnapshotResp, error) {
		s, err := mgr.Snapshot(ctx, req.Sandbox, req.Snapshot, req.Owner)
		if err != nil {
			return SnapshotResp{}, err
		}
		resp := SnapshotResp{Snapshot: snapshotRow(s)}
		if b, ok := mgr.Get(req.Sandbox); ok {
			resp.Source = sandboxRow(b)
		}
		return resp, nil
	})

	handle(conn, TypeSnapshotDelete, func(ctx context.Context, req DeleteSnapshotReq) (EmptyResp, error) {
		return EmptyResp{}, mgr.DeleteSnapshot(ctx, req.Snapshot, req.Owner)
	})

	handle(conn, TypeSnapshotFork, func(ctx context.Context, req ForkReq) (SandboxResp, error) {
		b, err := mgr.Fork(ctx, req.Snapshot, req.Name, req.Owner, req.VCPUs, req.MemMB)
		if err != nil {
			return SandboxResp{}, err
		}
		return SandboxResp{Sandbox: sandboxRow(b)}, nil
	})

	writes := make(chan func(), bookkeepingQueue)
	go drainBookkeeping(ctx, writes)

	// Touch and record_key are events and never requests. They are the two
	// highest-frequency writes in the system — one per SSH session teardown and
	// one per browser keystroke batch — and a reply would put a network round
	// trip inside both.
	conn.OnEvent(TypeTouch, func(raw json.RawMessage) {
		var req NameReq
		if err := json.Unmarshal(raw, &req); err != nil {
			log.Warn("nodelink: malformed touch", "err", err)
			return
		}
		queueWrite(conn, writes, log, TypeTouch, func() { mgr.MarkActive(req.Name) })
	})

	conn.OnEvent(TypeRecordKey, func(raw json.RawMessage) {
		var req KeyReq
		if err := json.Unmarshal(raw, &req); err != nil {
			log.Warn("nodelink: malformed key record", "err", err)
			return
		}
		queueWrite(conn, writes, log, TypeRecordKey, func() { mgr.RecordKey(req.Name, req.KeyFP) })
	})
}

// handle registers one typed verb.
//
// It exists so the unmarshal — and the sentence a body this node cannot read is
// answered with — is written once rather than fifteen times. A missing body is
// not an error: every request struct here is additive-by-omitempty, so an older
// gateway sending fewer fields gets this build's zero values rather than a
// refusal.
func handle[Req, Resp any](conn *Conn, typ string, fn func(context.Context, Req) (Resp, error)) {
	conn.Handle(typ, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req Req
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, ctlops.Invalid(typ, "bad_request",
					"that %s request could not be read: %v", typ, err)
			}
		}
		return fn(ctx, req)
	})
}

// queueWrite hands one bookkeeping write to the drain goroutine, or drops it.
// Never blocks: the caller is the link's reader.
func queueWrite(conn *Conn, writes chan<- func(), log *slog.Logger, typ string, fn func()) {
	select {
	case writes <- fn:
	default:
		// Worth a line, because a queue this deep filling up means the manager
		// is holding its lock for a long time — which is a fact about this
		// machine an operator wants, not about the gateway that asked.
		log.Warn("nodelink: bookkeeping queue full; dropping an event", "type", typ)
		conn.recordDrop("bookkeeping")
	}
}

func drainBookkeeping(ctx context.Context, writes <-chan func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case fn := <-writes:
			fn()
		}
	}
}
