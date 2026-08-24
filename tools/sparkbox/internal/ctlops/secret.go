package ctlops

// The owner-scoped secret verbs.
//
// These existed only in the user console until now, which made the shortest
// path to a working agent in a sandbox a browser trip: mint a session token
// over ssh, sign in, find the secrets panel, paste, remember to tag. Every step
// of that is a place to stop, and the thing being pasted — `claude
// setup-token`'s output, `gh auth token`'s — was already on the command line
// the user was standing at. So the verbs move down here, where the ssh channel
// and the REST API can both reach them, and the console keeps calling the same
// store it always did.
//
// Values do not appear in this file's audit lines, its errors, or its results.
// The store has no read-a-value method at all, so there is nothing to leak back
// out; what this layer has to get right is not logging what came in.

import (
	"context"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

// SecretResult is what a write reports back: the metadata of the row, plus the
// sandboxes whose environment was refreshed to match.
//
// Resynced is the part worth printing. Tag selection is the one piece of this
// feature users get wrong, and a write that answers "0 sandboxes" is the
// earliest possible moment to notice that the secret you just saved reaches
// nothing you own.
type SecretResult struct {
	Name     string   `json:"name"`
	Tags     []string `json:"tags"`
	Resynced []string `json:"resynced"`
}

// ListSecrets returns the caller's secrets as metadata — never values.
func (o *Ops) ListSecrets(c Caller) ([]secrets.SecretMeta, error) {
	const op = "secret.list"
	if o.secrets == nil {
		return nil, Disabled(op, "secrets are not enabled on this host")
	}
	metas, err := o.secrets.ListSecrets(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	if metas == nil {
		metas = []secrets.SecretMeta{}
	}
	return metas, nil
}

// PutSecret creates or replaces one of the caller's secrets and re-pushes the
// environment of every sandbox the change reaches.
//
// The re-push is what makes this feel like setting a variable rather than
// filing a request. Without it a secret saved while a box is running does not
// reach that box until its next resume, which for a pinned sandbox is never —
// and the user is left looking at an agent that still says it is logged out.
//
// An empty tag list is not rejected: the store stamps secrets.DefaultTag on it,
// and new sandboxes are stamped with the same tag, so the common case works
// without the user learning what a tag is. Naming tags explicitly is how you
// opt out of that, not a thing you must do first.
func (o *Ops) PutSecret(ctx context.Context, c Caller, name, value string, tags []string) (SecretResult, error) {
	const op = "secret.set"
	if o.secrets == nil {
		return SecretResult{}, Disabled(op, "secrets are not enabled on this host")
	}
	if value == "" {
		return SecretResult{}, Invalid(op, "empty_value",
			"refusing to store an empty value for %s — delete it instead if that is what you meant", name)
	}
	want, err := NormalizeTags(tags)
	if err != nil {
		return SecretResult{}, Invalid(op, "bad_tag", "%v", err)
	}
	// The store's own validation covers the env-name grammar, the reserved
	// names, and the characters /etc/environment cannot carry. Those are
	// user-facing sentences already, so they pass through verbatim rather than
	// being re-worded here into something less specific.
	if err := o.secrets.PutSecret(c.Handle, name, value, want); err != nil {
		return SecretResult{}, verbatim(Invalid(op, "bad_secret", "%v", err))
	}
	// Deliberately after the write: this is the effective tag set, which is
	// what the caller needs to see when the store filled in the default.
	res := SecretResult{Name: name, Tags: want, Resynced: o.resyncSecret(ctx, c.Handle, name)}
	if len(res.Tags) == 0 {
		res.Tags = []string{secrets.DefaultTag}
	}
	o.log.Info("secret set", "user", c.Handle, "env", name, "tags", res.Tags, "resynced", len(res.Resynced))
	return res, nil
}

// DeleteSecret removes one of the caller's secrets and re-pushes the sandboxes
// that were receiving it.
//
// The fan-out is computed BEFORE the delete, for the obvious reason: afterwards
// the row is gone and nothing can say which boxes used to select it, so the
// variable would linger in every running guest's environment until its next
// resume. Deleting a credential has to actually remove it from the places it
// reached, or it is not a deletion.
func (o *Ops) DeleteSecret(ctx context.Context, c Caller, name string) ([]string, error) {
	const op = "secret.rm"
	if o.secrets == nil {
		return nil, Disabled(op, "secrets are not enabled on this host")
	}
	affected, err := o.secrets.SandboxesForSecret(c.Handle, name)
	if err != nil {
		return nil, Fail(op, err)
	}
	if err := o.secrets.DeleteSecret(c.Handle, name); err != nil {
		return nil, verbatim(Invalid(op, "no_such_secret", "%v", err))
	}
	o.resyncBoxes(ctx, affected)
	o.log.Info("secret removed", "user", c.Handle, "env", name, "resynced", len(affected))
	return affected, nil
}

// resyncSecret re-pushes every sandbox that selects envName, returning the
// names it touched.
func (o *Ops) resyncSecret(ctx context.Context, owner, envName string) []string {
	affected, err := o.secrets.SandboxesForSecret(owner, envName)
	if err != nil {
		// Best-effort by design. The value is stored; failing the whole command
		// because the fan-out query stumbled would tell the user their secret
		// did not save, which is false and would have them enter it twice.
		o.log.Warn("could not resolve sandboxes for secret", "user", owner, "env", envName, "err", err)
		return nil
	}
	o.resyncBoxes(ctx, affected)
	return affected
}

// resyncBoxes pushes the current environment to each named sandbox. ResyncEnv
// is itself asynchronous and never wakes a paused box, so this neither blocks
// the caller nor resurrects a sandbox somebody deliberately paused.
func (o *Ops) resyncBoxes(ctx context.Context, names []string) {
	for _, n := range names {
		o.boxes.ResyncEnv(ctx, n)
	}
}
