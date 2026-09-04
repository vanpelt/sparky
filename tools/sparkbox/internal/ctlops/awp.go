package ctlops

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

const (
	awpTag       = "awp"
	awpTagPrefix = "awp-"
	awpMaxMemMB  = 32 * 1024
)

type awpNameLock struct {
	mu   sync.Mutex
	refs int
}

var awpRequiredVars = []string{
	"CONTROL_PLANE_URL",
	"RUN_ID",
	"SANDBOX_ID",
	"SPARKBOX_OIDC_AUDIENCE",
	"SPARKBOX_OIDC_ISSUER",
	"SPARKBOX_SANDBOX_ID",
	"TENANT_ID",
}

// CreateAWPSandbox provisions the Firecracker VM that is the AWP sandbox
// boundary. The fixed `awp` tag selects the operator-bound AWP runtime image;
// a private launch tag carries only non-secret configuration into the guest.
//
// The caller is operator-gated before any request validation so an ordinary
// account cannot use differences between validation errors to probe this
// operator surface.
func (o *Ops) CreateAWPSandbox(ctx context.Context, c Caller, a AWPCreateArgs) (AWPSandboxInfo, error) {
	const op = "awp.create"
	if err := o.operatorOnly(op, c, "only an operator may provision AWP sandboxes"); err != nil {
		return AWPSandboxInfo{}, err
	}
	if err := o.requireAWPStores(op); err != nil {
		return AWPSandboxInfo{}, err
	}
	if o.identity == nil {
		return AWPSandboxInfo{}, Disabled(op, "workload identity is not enabled on this host")
	}
	if err := validateAWPCreate(op, a); err != nil {
		return AWPSandboxInfo{}, err
	}
	if !o.identity.AudienceAllowed(a.OIDCAudience) {
		return AWPSandboxInfo{}, Invalid(op, "audience_not_allowed",
			"OIDC audience %q is not allowed by this host", a.OIDCAudience)
	}
	if err := o.requireAWPTemplate(op, c.Handle); err != nil {
		return AWPSandboxInfo{}, err
	}
	unlock := o.lockAWPName(a.SandboxID)
	defer unlock()
	if err := o.nameIsFree(op, a.SandboxID); err != nil {
		return AWPSandboxInfo{}, err
	}

	launchTag := awpLaunchTag(c.Handle, a.SandboxID, a.RunID)
	vars := map[string]string{
		"CONTROL_PLANE_URL":      a.ControlPlaneURL,
		"RUN_ID":                 a.RunID,
		"SANDBOX_ID":             a.SandboxID,
		"SPARKBOX_OIDC_AUDIENCE": a.OIDCAudience,
		"SPARKBOX_OIDC_ISSUER":   o.identity.URL(),
		"TENANT_ID":              a.TenantID,
	}
	// Refuse every env grammar problem before the first row is written. This
	// also rejects values /etc/environment cannot represent, including '#'.
	for _, name := range awpRequiredVars {
		if name == "SPARKBOX_SANDBOX_ID" {
			continue
		}
		if err := secrets.ValidateVar(name, vars[name]); err != nil {
			return AWPSandboxInfo{}, Invalid(op, "invalid_launch_environment", "%v", err)
		}
	}
	created, err := o.Create(ctx, c, CreateArgs{
		Name: a.SandboxID, Tags: []string{awpTag, launchTag}, Node: a.Node,
		VCPUs: a.VCPUs, MemMB: a.MemMB,
	})
	if err != nil {
		return AWPSandboxInfo{}, awpError(op, err)
	}
	rollback := func(cause error) (AWPSandboxInfo, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PauseTimeout)
		defer cancel()
		if err := o.boxes.Destroy(cleanupCtx, a.SandboxID); err != nil {
			o.log.Warn("AWP sandbox rollback destroy failed", "name", a.SandboxID, "err", err)
			// Keep its launch rows if the VM survived. Removing them would turn a
			// recoverable half-created guest into an unidentifiable one.
			return AWPSandboxInfo{}, awpError(op, cause)
		}
		o.clearAWPVars(c.Handle, launchTag)
		return AWPSandboxInfo{}, awpError(op, cause)
	}

	box, ok := o.boxes.Get(a.SandboxID)
	if !ok || box.ID == "" {
		return rollback(fmt.Errorf("created sandbox has no immutable provider id"))
	}
	vars["SPARKBOX_SANDBOX_ID"] = box.ID
	if err := secrets.ValidateVar("SPARKBOX_SANDBOX_ID", box.ID); err != nil {
		return rollback(err)
	}
	// Write only after Create wins the globally unique sandbox name. That keeps
	// a losing concurrent request from deleting or replacing the winner's
	// launch rows. The guest launcher waits for these required fields, and the
	// synchronous AwaitEnv below is the point at which they become committed.
	for _, name := range awpRequiredVars {
		if err := o.envVars.PutVar(c.Handle, launchTag, name, vars[name]); err != nil {
			return rollback(err)
		}
	}
	if err := o.boxes.SetPinned(a.SandboxID, true); err != nil {
		return rollback(err)
	}
	// Create's push is asynchronous and may have raced the provider-id row.
	// AwaitEnv is the barrier that makes a successful response mean the complete
	// launch contract has reached the guest.
	if err := o.boxes.AwaitEnv(ctx, a.SandboxID); err != nil {
		return rollback(err)
	}

	created.Pinned = true
	o.log.Info("AWP sandbox created", "user", c.Handle, "name", a.SandboxID,
		"run_id", a.RunID, "tenant_id", a.TenantID, "provider_id", box.ID)
	return awpInfo(created, vars, box.ID), nil
}

// GetAWPSandbox returns only sandboxes launched through the AWP surface. An
// ordinary Sparkbox VM is indistinguishable from a missing one here, even to an
// operator, so this endpoint cannot become a second lifecycle API by accident.
func (o *Ops) GetAWPSandbox(ctx context.Context, c Caller, name string) (AWPSandboxInfo, error) {
	const op = "awp.get"
	if err := o.operatorOnly(op, c, "only an operator may inspect AWP sandboxes"); err != nil {
		return AWPSandboxInfo{}, err
	}
	if err := o.requireAWPStores(op); err != nil {
		return AWPSandboxInfo{}, err
	}
	unlock := o.lockAWPName(name)
	defer unlock()
	box, err := o.owned(op, name, c)
	if err != nil {
		return AWPSandboxInfo{}, err
	}
	launchTag, err := o.awpLaunchTagFor(op, box.Name)
	if err != nil {
		return AWPSandboxInfo{}, err
	}
	vars, err := o.readAWPVars(op, c.Handle, launchTag)
	if err != nil {
		return AWPSandboxInfo{}, err
	}
	return awpInfo(o.info(box), vars, box.ID), nil
}

// DeleteAWPSandbox destroys one AWP VM and removes its private launch config.
// The public `awp` tag is not configuration and is intentionally retained for
// every other AWP sandbox.
func (o *Ops) DeleteAWPSandbox(ctx context.Context, c Caller, name string) error {
	const op = "awp.rm"
	if err := o.operatorOnly(op, c, "only an operator may destroy AWP sandboxes"); err != nil {
		return err
	}
	if err := o.requireAWPStores(op); err != nil {
		return err
	}
	unlock := o.lockAWPName(name)
	defer unlock()
	box, err := o.owned(op, name, c)
	if err != nil {
		return err
	}
	launchTag, err := o.awpLaunchTagFor(op, box.Name)
	if err != nil {
		return err
	}
	if err := o.Destroy(ctx, c, box.Name); err != nil {
		return awpError(op, err)
	}
	if err := o.envVars.DeleteVarsForTag(c.Handle, launchTag); err != nil {
		// The VM is already irreversibly gone. Report success and leave an audit
		// warning rather than make a retry look as though destruction failed.
		o.log.Warn("AWP launch metadata cleanup failed", "name", box.Name, "tag", launchTag, "err", err)
	}
	o.log.Info("AWP sandbox destroyed", "user", c.Handle, "name", box.Name)
	return nil
}

func (o *Ops) requireAWPStores(op string) error {
	if o.tags == nil || o.envVars == nil {
		return Disabled(op, "AWP sandbox provisioning is not enabled on this host")
	}
	return nil
}

func (o *Ops) requireAWPTemplate(op, owner string) error {
	if o.templateTags == nil {
		return Disabled(op, "AWP template bindings are not enabled on this host")
	}
	bindings, err := o.templateTags.BindingsForTags(owner, []string{awpTag})
	if err != nil {
		return Fail(op, err)
	}
	for _, binding := range bindings {
		if binding.Tag == awpTag && binding.Snapshot != "" {
			return nil
		}
	}
	return &Error{
		Kind: KindConflict, Op: op, Code: "awp_template_unbound",
		Msg:      "the awp tag has no runtime template bound; snapshot an AWP-capable VM and bind it before provisioning",
		Verbatim: true,
	}
}

func validateAWPCreate(op string, a AWPCreateArgs) error {
	if a.MemMB < 0 || a.MemMB > awpMaxMemMB {
		return Invalid(op, "invalid_mem_mb",
			"mem_mb must be between 0 and %d MiB (32 GiB); zero uses the Sparkbox default", awpMaxMemMB)
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"sandbox_id", a.SandboxID},
		{"run_id", a.RunID},
		{"tenant_id", a.TenantID},
	} {
		field, value := item.field, item.value
		if strings.TrimSpace(value) == "" {
			return Invalid(op, "missing_"+field, "%s is required", field)
		}
		if len(value) > 256 {
			return Invalid(op, "invalid_"+field, "%s exceeds 256 bytes", field)
		}
	}
	for _, item := range []struct {
		field string
		raw   string
	}{
		{"control_plane_url", a.ControlPlaneURL},
		{"oidc_audience", a.OIDCAudience},
	} {
		field, raw := item.field, item.raw
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return Invalid(op, "invalid_"+field,
				"%s must be an absolute HTTPS URL without credentials, query, or fragment", field)
		}
	}
	return nil
}

func awpLaunchTag(owner, sandboxID, runID string) string {
	sum := sha256.Sum256([]byte(owner + "\x00" + sandboxID + "\x00" + runID))
	return fmt.Sprintf("%s%x", awpTagPrefix, sum[:10])
}

func (o *Ops) awpLaunchTagFor(op, sandbox string) (string, error) {
	tags, err := o.tags.TagsFor(sandbox)
	if err != nil {
		return "", Fail(op, err)
	}
	if !slices.Contains(tags, awpTag) {
		return "", NotFound(op, "sandbox", sandbox)
	}
	var found string
	for _, tag := range tags {
		if !strings.HasPrefix(tag, awpTagPrefix) {
			continue
		}
		if found != "" {
			return "", Fail(op, fmt.Errorf("AWP sandbox %q has multiple launch tags", sandbox))
		}
		found = tag
	}
	if found == "" {
		return "", Fail(op, fmt.Errorf("AWP sandbox %q has no launch metadata tag", sandbox))
	}
	return found, nil
}

func (o *Ops) readAWPVars(op, owner, tag string) (map[string]string, error) {
	rows, err := o.envVars.VarsForTag(owner, tag)
	if err != nil {
		return nil, Fail(op, err)
	}
	vars := make(map[string]string, len(rows))
	for _, row := range rows {
		vars[row.Name] = row.Value
	}
	for _, name := range awpRequiredVars {
		if vars[name] == "" {
			return nil, Fail(op, fmt.Errorf("AWP launch metadata %s is missing", name))
		}
	}
	return vars, nil
}

func awpInfo(box SandboxInfo, vars map[string]string, providerID string) AWPSandboxInfo {
	return AWPSandboxInfo{
		Sandbox:         box,
		SandboxID:       vars["SANDBOX_ID"],
		RunID:           vars["RUN_ID"],
		TenantID:        vars["TENANT_ID"],
		ControlPlaneURL: vars["CONTROL_PLANE_URL"],
		RuntimePort:     routes.DefaultPort,
		WorkloadIdentity: AWPWorkloadIdentity{
			Issuer: vars["SPARKBOX_OIDC_ISSUER"], Audience: vars["SPARKBOX_OIDC_AUDIENCE"],
			SandboxID: providerID,
		},
	}
}

func (o *Ops) clearAWPVars(owner, tag string) {
	if err := o.envVars.DeleteVarsForTag(owner, tag); err != nil {
		o.log.Warn("AWP launch metadata rollback failed", "tag", tag, "err", err)
	}
}

func awpError(op string, err error) *Error {
	e := AsError(op, err)
	cp := *e
	cp.Op = op
	return &cp
}

func (o *Ops) lockAWPName(name string) func() {
	o.awpLocksMu.Lock()
	l := o.awpLocks[name]
	if l == nil {
		l = &awpNameLock{}
		o.awpLocks[name] = l
	}
	l.refs++
	o.awpLocksMu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		o.awpLocksMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(o.awpLocks, name)
		}
		o.awpLocksMu.Unlock()
	}
}
