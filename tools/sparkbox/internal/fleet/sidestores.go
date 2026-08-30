package fleet

// The half of a sandbox's lifecycle that never leaves the gateway.
//
// Four stores are keyed by a sandbox's NAME and live on the gateway no matter
// which machine holds the rootfs:
//
//   - routes    — <subdomain> -> <sandbox>:<port>, with an owner column that is
//     what gates a private route's auth (internal/proxy);
//   - schedules — cron rows that run a command IN a named sandbox;
//   - tags      — the labels that select which secrets an owner's sandbox gets
//     and which egress allowlist it runs under;
//   - the front door — per-sandbox DNS at the edge.
//
// On a single box host.Manager does all of this itself, because it is holding
// all of it: Manager.Create mints the default route, Manager.Rename moves all
// four (routes fatally, with rollback), Manager.Destroy deletes all four. A node
// is given none of them — cmd/sparkbox's node mode leaves every one nil on
// purpose, since each is backed by a store or a DNS zone the gateway owns — so
// the manager's own nil guards skip them there and the work simply does not
// happen unless this file does it.
//
// That is the split W17 names: "the gateway does its half before dispatching and
// rolls it back if the node's half fails". Without it a remote rename leaves an
// owner's tag rows stranded under the dead name — so the sandbox silently loses
// every secret those tags selected — and a remote destroy leaves a route row,
// carrying its old owner's handle, pointing at a name the ledger has just handed
// back to somebody else. That last one is not an untidiness: it is one user's
// subdomain resolving into another user's sandbox, and a schedule firing one
// user's command inside it.
//
// Everything here runs ONLY for a sandbox on another machine. A local placement
// goes to the local manager, which does its own half exactly as it always has,
// and doing it twice would mean this file racing the manager over the same rows.

import (
	"context"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

// SandboxRows is a name-keyed side store the fleet has to keep in step with a
// sandbox on another machine. Satisfied structurally by *schedule.Store,
// *secrets.Store and *repos.Store, which are the SAME objects host.Options is given: there is one
// set of rows per deployment, and the fleet reaches them directly only for the
// sandboxes the local manager will never be told about.
type SandboxRows interface {
	DeleteBySandbox(sandbox string) error
	RenameSandbox(old, new string) error
}

// RouteRows is SandboxRows plus the two reads and writes only the routes table
// needs: the default subdomain a create mints, and the collision check a rename
// has to make. Satisfied structurally by *routes.Store.
//
// It is a separate interface rather than more methods on SandboxRows because
// routes are the only one of the four that is authorization-bearing, and the
// only one whose rows a rename must treat as fatal.
type RouteRows interface {
	SandboxRows
	Upsert(r routes.Route) error
	GetBySubdomain(subdomain string) (routes.Route, bool, error)
	ListBySandbox(sandbox string) ([]routes.Route, error)
	SetVisibility(subdomain, visibility string) error
}

// sides is the set of gateway stores a remote sandbox's name has to be carried
// through, resolved once at construction. Any of them may be nil — a unit test's
// fleet wires none, and a deployment without a Cloudflare token has no front
// door — and every use is nil-guarded, exactly as host.Manager guards its own.
type sides struct {
	routes    RouteRows
	schedules SandboxRows
	tags      SandboxRows
	repoRefs  SandboxRows
	frontDoor host.FrontDoor
}

// mint does for a sandbox built on another machine what Manager.Create does for
// one built here: gives it its default subdomain and its front-door name.
//
// It matters beyond reachability, which is M3's problem. The default route row
// is also what CLAIMS the name in the routes table, and a name that is claimed
// nowhere is a name a later custom route can be pointed at by anyone. It is
// best-effort for the same reason the manager's is: a sandbox that exists and
// boots is not failed over its DNS.
func (f *Fleet) mint(ctx context.Context, name, owner string) {
	if f.sides.routes != nil {
		if err := f.sides.routes.Upsert(routes.Route{
			Subdomain: name, Sandbox: name, Owner: owner, Port: routes.DefaultPort,
		}); err != nil {
			f.log.Warn("default route creation failed for a sandbox on another machine",
				"name", name, "owner", owner, "err", err)
		}
	}
	if f.sides.frontDoor != nil {
		f.sides.frontDoor.Ensure(ctx, name)
	}
}

// sweep does for a sandbox destroyed on another machine what Manager.Destroy
// does for one destroyed here.
//
// It runs BEFORE the placement row is released, and the order is the whole
// point: the moment Release commits, the name is free and the next create may
// take it — and would then have its own fresh rows deleted by this sweep. While
// the row is still held nobody else can be holding that name, so every row
// keyed by it is unambiguously the destroyed sandbox's.
//
// Best-effort per store, and deliberately not fatal: the sandbox is gone
// whatever happens here, and failing the user's `ctl rm` over a row they cannot
// see would leave them with nothing to do about it. A row that survives is
// logged loudly, which is what an operator can act on.
func (f *Fleet) sweep(ctx context.Context, name string) {
	if f.sides.routes != nil {
		if err := f.sides.routes.DeleteBySandbox(name); err != nil {
			f.log.Error("could not delete the routes of a sandbox destroyed on another machine",
				"name", name, "err", err,
				"next", "that subdomain still carries its old owner and would answer for whoever takes the name next; delete it by hand")
		}
	}
	if f.sides.schedules != nil {
		if err := f.sides.schedules.DeleteBySandbox(name); err != nil {
			f.log.Error("could not delete the schedules of a sandbox destroyed on another machine",
				"name", name, "err", err)
		}
	}
	if f.sides.tags != nil {
		if err := f.sides.tags.DeleteBySandbox(name); err != nil {
			f.log.Error("could not delete the tag rows of a sandbox destroyed on another machine",
				"name", name, "err", err)
		}
	}
	if f.sides.repoRefs != nil {
		if err := f.sides.repoRefs.DeleteBySandbox(name); err != nil {
			f.log.Error("could not delete the repo ref overrides of a sandbox destroyed on another machine",
				"name", name, "err", err,
				"next", "whoever takes this name next gets the branch that sandbox asked for; delete the row by hand")
		}
	}
	if f.sides.frontDoor != nil {
		f.sides.frontDoor.Remove(ctx, name)
	}
}

// subdomainFree is the collision check Manager.renameChecks makes against the
// routes table, made here because on a node m.routes is nil and that check is
// therefore skipped entirely — a remote rename onto a subdomain another user's
// custom route already occupies would otherwise be refused by nobody.
//
// The predicate is the manager's, restated rather than shared because it is
// three lines and the manager's is behind an unexported method on a type this
// package does not embed. A subdomain is free if nothing has it, if the row is
// this sandbox's own custom route, or if it is this same sandbox half-moved by a
// rename that crashed — the owner guard on that last one is what stops a stale
// row from a destroyed box, which carries somebody else's auth, from being
// silently adopted.
func (f *Fleet) subdomainFree(oldName, newName, owner string) (bool, error) {
	if f.sides.routes == nil {
		return true, nil
	}
	r, found, err := f.sides.routes.GetBySubdomain(newName)
	if err != nil {
		return false, err
	}
	if !found || r.Sandbox == oldName || (r.Sandbox == newName && r.Owner == owner) {
		return true, nil
	}
	return false, nil
}

// carry moves a remote sandbox's gateway-side rows to its new name and returns
// the undo.
//
// The ordering mirrors Manager.Rename exactly, and for the same reasons:
//
//   - routes go first and FATALLY. A route row carries the owner column that
//     gates private-route auth, so one left under the old name is an
//     authorization record pointing at a sandbox that no longer exists. Failing
//     before anything else has moved is clean — the node has not been asked to
//     do anything yet — and the store's RenameSandbox is idempotent, so the
//     repair for a crash on either side is to rename again.
//   - schedules, tags and the front door follow best-effort. Each is
//     idempotent under a re-run and none of them is authorization-bearing.
//
// The undo is what runs if the NODE then refuses. It is best-effort throughout:
// by then the rows have moved, the ledger row is about to move back, and the
// repair for a half-moved rename is the same rename again.
func (f *Fleet) carry(ctx context.Context, oldName, newName string) (undo func(), err error) {
	if f.sides.routes != nil {
		if err := f.sides.routes.RenameSandbox(oldName, newName); err != nil {
			return nil, err
		}
	}
	if f.sides.schedules != nil {
		if err := f.sides.schedules.RenameSandbox(oldName, newName); err != nil {
			f.log.Warn("schedule rename failed for a sandbox on another machine",
				"old", oldName, "new", newName, "err", err)
		}
	}
	if f.sides.tags != nil {
		if err := f.sides.tags.RenameSandbox(oldName, newName); err != nil {
			f.log.Warn("tag rename failed for a sandbox on another machine",
				"old", oldName, "new", newName, "err", err)
		}
	}
	if f.sides.repoRefs != nil {
		if err := f.sides.repoRefs.RenameSandbox(oldName, newName); err != nil {
			f.log.Warn("repo ref-override rename failed for a sandbox on another machine",
				"old", oldName, "new", newName, "err", err)
		}
	}
	if f.sides.frontDoor != nil {
		f.sides.frontDoor.Remove(ctx, oldName)
		f.sides.frontDoor.Ensure(ctx, newName)
	}
	return func() { f.carryBack(ctx, oldName, newName) }, nil
}

func (f *Fleet) carryBack(ctx context.Context, oldName, newName string) {
	if f.sides.routes != nil {
		if err := f.sides.routes.RenameSandbox(newName, oldName); err != nil {
			f.log.Warn("route rename rollback failed; rename again to repair",
				"old", oldName, "new", newName, "err", err)
		}
	}
	if f.sides.schedules != nil {
		if err := f.sides.schedules.RenameSandbox(newName, oldName); err != nil {
			f.log.Warn("schedule rename rollback failed", "old", oldName, "new", newName, "err", err)
		}
	}
	if f.sides.tags != nil {
		if err := f.sides.tags.RenameSandbox(newName, oldName); err != nil {
			f.log.Warn("tag rename rollback failed", "old", oldName, "new", newName, "err", err)
		}
	}
	if f.sides.repoRefs != nil {
		if err := f.sides.repoRefs.RenameSandbox(newName, oldName); err != nil {
			f.log.Warn("repo ref-override rename rollback failed", "old", oldName, "new", newName, "err", err)
		}
	}
	if f.sides.frontDoor != nil {
		f.sides.frontDoor.Remove(ctx, newName)
		f.sides.frontDoor.Ensure(ctx, oldName)
	}
}
