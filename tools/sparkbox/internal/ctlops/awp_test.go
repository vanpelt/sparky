package ctlops

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

type fakeWorkloadIdentity struct {
	issuer  string
	allowed string
}

func (f fakeWorkloadIdentity) URL() string { return f.issuer }
func (f fakeWorkloadIdentity) AudienceAllowed(audience string) bool {
	return audience == f.allowed
}

func enableAWP(r *rig) *fakeEnvVars {
	vars := &fakeEnvVars{c: r.calls, rows: map[string]secrets.Var{}}
	r.envVars, r.ops.envVars = vars, vars
	r.ops.identity = fakeWorkloadIdentity{
		issuer: "https://oidc.example.test", allowed: "https://awp.example.test",
	}
	return vars
}

func awpArgs() AWPCreateArgs {
	return AWPCreateArgs{
		SandboxID: "sbx-123", RunID: "run-456", TenantID: "tenant-789",
		ControlPlaneURL: "https://awp.example.test", OIDCAudience: "https://awp.example.test",
		VCPUs: 2, MemMB: 2048,
	}
}

func TestAWPLifecycleUsesBoundTemplateAndWorkloadIdentity(t *testing.T) {
	r := newRig(t)
	vars := enableAWP(r)
	r.tmpl.snaps["opsy/awp-runtime"] = &host.Snapshot{
		Name: "awp-runtime", Owner: "opsy", Image: "awp-runtime.img", CreatedAt: time.Unix(0, 0).UTC(),
	}
	r.bindings.bind("opsy", awpTag, "awp-runtime")
	r.calls.reset()

	opsy := Caller{Handle: "opsy"}
	got, err := r.ops.CreateAWPSandbox(context.Background(), opsy, awpArgs())
	if err != nil {
		t.Fatalf("CreateAWPSandbox: %v", err)
	}
	if got.SandboxID != "sbx-123" || got.RunID != "run-456" || got.TenantID != "tenant-789" {
		t.Fatalf("identity = %+v", got)
	}
	if got.WorkloadIdentity.Issuer != "https://oidc.example.test" ||
		got.WorkloadIdentity.Audience != "https://awp.example.test" ||
		got.WorkloadIdentity.SandboxID != "provider-sbx-123" {
		t.Fatalf("workload identity = %+v", got.WorkloadIdentity)
	}
	if !got.Sandbox.Pinned || !slices.Contains(got.Sandbox.Tags, awpTag) {
		t.Fatalf("sandbox = %+v, want pinned with awp tag", got.Sandbox)
	}
	if image := r.boxes.boxes["sbx-123"].Image; image != "awp-runtime.img" {
		t.Fatalf("image = %q, want tag-bound AWP runtime image", image)
	}

	launchTag := awpLaunchTag("opsy", "sbx-123", "run-456")
	if _, ok := vars.rows[varKey("opsy", launchTag, "BOOTSTRAP_TOKEN")]; ok {
		t.Fatal("bootstrap bearer was persisted in the launch environment")
	}
	if row := vars.rows[varKey("opsy", launchTag, "SPARKBOX_SANDBOX_ID")]; row.Value != "provider-sbx-123" {
		t.Fatalf("provider id row = %+v", row)
	}
	if !r.calls.has("SetPinned sbx-123 true") || !r.calls.has("AwaitEnv sbx-123") {
		t.Fatalf("create did not pin and synchronously deliver env: %v", r.calls.all())
	}

	read, err := r.ops.GetAWPSandbox(context.Background(), opsy, "sbx-123")
	if err != nil {
		t.Fatalf("GetAWPSandbox: %v", err)
	}
	if read.RunID != got.RunID || read.WorkloadIdentity != got.WorkloadIdentity {
		t.Fatalf("read = %+v, create = %+v", read, got)
	}

	if err := r.ops.DeleteAWPSandbox(context.Background(), opsy, "sbx-123"); err != nil {
		t.Fatalf("DeleteAWPSandbox: %v", err)
	}
	if _, ok := r.boxes.boxes["sbx-123"]; ok {
		t.Fatal("AWP sandbox survived delete")
	}
	if rows, err := vars.VarsForTag("opsy", launchTag); err != nil || len(rows) != 0 {
		t.Fatalf("launch metadata after delete = %v, %v", rows, err)
	}
}

func TestAWPMethodsGateOperatorBeforeValidationOrLookup(t *testing.T) {
	r := newRig(t)
	enableAWP(r)

	if _, err := r.ops.CreateAWPSandbox(context.Background(), alice(), AWPCreateArgs{}); !IsKind(err, KindDenied) {
		t.Fatalf("non-operator create = %v, want denied before validation", err)
	}
	if _, err := r.ops.GetAWPSandbox(context.Background(), alice(), "alicebox"); !IsKind(err, KindDenied) {
		t.Fatalf("non-operator get = %v, want denied before lookup", err)
	}
	if err := r.ops.DeleteAWPSandbox(context.Background(), alice(), "alicebox"); !IsKind(err, KindDenied) {
		t.Fatalf("non-operator delete = %v, want denied before lookup", err)
	}
}

func TestAWPSurfaceDoesNotOperateOnOrdinarySandbox(t *testing.T) {
	r := newRig(t)
	enableAWP(r)
	r.boxes.boxes["ordinary"] = &host.Sandbox{
		ID: "ordinary-id", Name: "ordinary", Owner: "opsy", State: vmm.StateRunning,
	}
	opsy := Caller{Handle: "opsy"}

	if _, err := r.ops.GetAWPSandbox(context.Background(), opsy, "ordinary"); !IsKind(err, KindNotFound) {
		t.Fatalf("get ordinary = %v, want masked not found", err)
	}
	if err := r.ops.DeleteAWPSandbox(context.Background(), opsy, "ordinary"); !IsKind(err, KindNotFound) {
		t.Fatalf("delete ordinary = %v, want masked not found", err)
	}
	if _, ok := r.boxes.boxes["ordinary"]; !ok {
		t.Fatal("AWP delete destroyed an ordinary sandbox")
	}
}

func TestAWPCreateRefusesAudienceBeforeAnyWrite(t *testing.T) {
	r := newRig(t)
	enableAWP(r)
	a := awpArgs()
	a.OIDCAudience = "https://other.example.test"
	r.calls.reset()

	_, err := r.ops.CreateAWPSandbox(context.Background(), Caller{Handle: "opsy"}, a)
	if !IsKind(err, KindInvalid) || err.(*Error).Code != "audience_not_allowed" {
		t.Fatalf("err = %v, want audience_not_allowed", err)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("refused audience reached writes: %v", got)
	}
}

func TestAWPCreateEnforcesMemoryLimitBeforeAnyWrite(t *testing.T) {
	r := newRig(t)
	enableAWP(r)
	a := awpArgs()
	a.MemMB = awpMaxMemMB
	if err := validateAWPCreate("awp.create", a); err != nil {
		t.Fatalf("32 GiB boundary rejected: %v", err)
	}

	a.MemMB++
	r.calls.reset()
	_, err := r.ops.CreateAWPSandbox(context.Background(), Caller{Handle: "opsy"}, a)
	if !IsKind(err, KindInvalid) || err.(*Error).Code != "invalid_mem_mb" {
		t.Fatalf("err = %v, want invalid_mem_mb", err)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("oversized AWP create reached writes: %v", got)
	}
}

func TestAWPCreateRefusesGenericImageWhenTemplateIsUnbound(t *testing.T) {
	r := newRig(t)
	enableAWP(r)
	r.calls.reset()

	_, err := r.ops.CreateAWPSandbox(context.Background(), Caller{Handle: "opsy"}, awpArgs())
	if !IsKind(err, KindConflict) || err.(*Error).Code != "awp_template_unbound" {
		t.Fatalf("err = %v, want awp_template_unbound", err)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("unbound AWP create reached writes: %v", got)
	}
	if _, ok := r.boxes.boxes["sbx-123"]; ok {
		t.Fatal("unbound AWP create fell back to the generic base image")
	}
}

func TestAWPCreateRollsBackVMAndMetadataWhenDeliveryFails(t *testing.T) {
	r := newRig(t)
	vars := enableAWP(r)
	r.tmpl.snaps["opsy/awp-runtime"] = &host.Snapshot{
		Name: "awp-runtime", Owner: "opsy", Image: "awp-runtime.img", CreatedAt: time.Unix(0, 0).UTC(),
	}
	r.bindings.bind("opsy", awpTag, "awp-runtime")
	r.boxes.awaitEnvErr = errors.New("guest did not accept launch environment")

	_, err := r.ops.CreateAWPSandbox(context.Background(), Caller{Handle: "opsy"}, awpArgs())
	if !IsKind(err, KindInternal) {
		t.Fatalf("err = %v, want internal delivery failure", err)
	}
	if _, ok := r.boxes.boxes["sbx-123"]; ok {
		t.Fatal("VM survived failed launch-environment delivery")
	}
	tag := awpLaunchTag("opsy", "sbx-123", "run-456")
	if rows, err := vars.VarsForTag("opsy", tag); err != nil || len(rows) != 0 {
		t.Fatalf("launch metadata survived rollback: %v, %v", rows, err)
	}
}
