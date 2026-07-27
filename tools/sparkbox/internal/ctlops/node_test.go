package ctlops

// What removing a machine from the roster has to do to the machine.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
)

// liveRoster is a roster wired to a running fleet, which is what makes it able
// to answer the one question a plain table cannot: is that machine connected
// right now, and can you hang up on it. The fakes elsewhere in this suite are
// deliberately not — a roster with no fleet behind it is a real shape, and the
// test below pins that it still works.
type liveRoster struct {
	list    []NodeInfo
	linked  map[string]bool
	evicted []string
	reason  string
}

func (r *liveRoster) ListNodes() ([]NodeInfo, error) {
	return append([]NodeInfo(nil), r.list...), nil
}

func (r *liveRoster) ApproveNode(fp, by string) (NodeInfo, error) {
	for i := range r.list {
		if r.list[i].FP != "" && r.list[i].FP == fp {
			r.list[i].Status = "approved"
			r.list[i].ApprovedBy = by
			return r.list[i], nil
		}
	}
	return NodeInfo{}, errors.New("no such node")
}

func (r *liveRoster) RemoveNode(name string) error {
	for i := range r.list {
		if r.list[i].Name == name {
			r.list = append(r.list[:i:i], r.list[i+1:]...)
			return nil
		}
	}
	return errors.New("no such node")
}

func (r *liveRoster) EvictNode(name, reason string) bool {
	r.evicted = append(r.evicted, name)
	r.reason = reason
	if !r.linked[name] {
		return false
	}
	delete(r.linked, name)
	return true
}

var _ NodeEvicter = (*liveRoster)(nil)

// configuredRoster is the production-shaped side of the approval migration:
// it still satisfies the legacy NodeRoster for listing/removal, but exposing
// ApproveFPWithConfig means ctlops must never call its old approval method.
type configuredRoster struct {
	list            []NodeInfo
	configuredCalls int
	legacyCalls     int
	config          nodes.ApprovalConfig
	approveErr      error
}

func (r *configuredRoster) ListNodes() ([]NodeInfo, error) {
	return append([]NodeInfo(nil), r.list...), nil
}

func (r *configuredRoster) ApproveNode(fp, by string) (NodeInfo, error) {
	r.legacyCalls++
	return NodeInfo{}, errors.New("legacy approval must not be called")
}

func (r *configuredRoster) ApproveFPWithConfig(fp, by string, cfg nodes.ApprovalConfig) error {
	r.configuredCalls++
	r.config = cfg
	if r.approveErr != nil {
		return r.approveErr
	}
	for i := range r.list {
		if r.list[i].FP == fp {
			r.list[i].Status = "approved"
			r.list[i].ApprovedBy = by
			r.list[i].GuestSubnet = cfg.GuestSubnet
			r.list[i].GRPCAddr = cfg.GRPCAddr
			return nil
		}
	}
	return nodes.ErrNoSuchNode
}

func (r *configuredRoster) RemoveNode(name string) error {
	for i := range r.list {
		if r.list[i].Name == name {
			r.list = append(r.list[:i:i], r.list[i+1:]...)
			return nil
		}
	}
	return nodes.ErrNoSuchNode
}

var _ NodeConfiguredApprover = (*configuredRoster)(nil)

// TestRemovingANodeClosesItsLiveLink is the difference between revoking an
// approval and merely writing down that you did.
//
// The roster row is read at one moment only — when a machine connects. Nothing
// re-reads it afterwards, and the link is built with no idle timeout so that a
// node may stay connected for weeks. A removal that stopped at the store would
// therefore leave the removed machine holding its control channel, reporting
// capacity and serving streams, indefinitely.
func TestRemovingANodeClosesItsLiveLink(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	roster := &liveRoster{
		list: []NodeInfo{
			{Name: "here", Status: "approved", Online: true, Local: true},
			{Name: "node-b", Status: "approved", Online: true, FP: "SHA256:bbbb"},
		},
		linked: map[string]bool{"node-b": true},
	}
	r.ops.nodes = roster

	if err := r.ops.RemoveNode(ctx, Caller{Handle: "opsy"}, "node-b"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	if !slices.Equal(roster.evicted, []string{"node-b"}) {
		t.Fatalf("evictions = %v, want the link for node-b closed", roster.evicted)
	}
	if roster.reason == "" {
		t.Error("the node was hung up on with no reason to log")
	}
	if roster.linked["node-b"] {
		t.Error("node-b was removed from the roster and is still linked")
	}
}

// TestRemovingANodeWithoutALiveFleetStillWorks is the other half of making the
// eviction optional: a roster that is only a table — every fake in this suite,
// and any wiring that has not been joined to a running fleet — has no link to
// close, and a removal through it must be exactly what it always was.
func TestRemovingANodeWithoutALiveFleetStillWorks(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.withNodes()

	// "newcomer" is the pending row: it holds nothing, so the sandbox guard
	// does not fire and the removal reaches the store.
	if err := r.ops.RemoveNode(ctx, Caller{Handle: "opsy"}, "newcomer"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	list, err := r.ops.ListNodes(ctx, Caller{Handle: "opsy"})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	for _, n := range list {
		if n.Name == "newcomer" {
			t.Fatal("newcomer survived its own removal")
		}
	}
}

func TestConfiguredApprovalPersistsCanonicalTrustedTopology(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	roster := &configuredRoster{list: []NodeInfo{
		{Name: "newcomer", Status: "pending", FP: fpNewcomer},
	}}
	r.ops.nodes = roster
	r.ops.gatewayGuestSubnet = "10.200.8.9/20"

	got, err := r.ops.ApproveNode(ctx, Caller{Handle: "opsy"}, fpNewcomer, NodeApprovalConfig{
		GuestSubnet: " 10.201.7.9/20 ",
		GRPCAddr:    " 100.64.0.12:09443 ",
	})
	if err != nil {
		t.Fatalf("ApproveNode: %v", err)
	}
	want := nodes.ApprovalConfig{
		GuestSubnet:        "10.201.0.0/20",
		GatewayGuestSubnet: "10.200.0.0/20",
		GRPCAddr:           "100.64.0.12:9443",
	}
	if roster.config != want {
		t.Errorf("configured write = %+v, want %+v", roster.config, want)
	}
	if roster.configuredCalls != 1 || roster.legacyCalls != 0 {
		t.Errorf("calls: configured=%d legacy=%d, want 1/0",
			roster.configuredCalls, roster.legacyCalls)
	}
	if got.Status != "approved" || got.GuestSubnet != want.GuestSubnet || got.GRPCAddr != want.GRPCAddr {
		t.Errorf("approved row = %+v", got)
	}
}

func TestConfiguredApprovalRequiresExplicitSubnet(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	roster := &configuredRoster{list: []NodeInfo{
		{Name: "newcomer", Status: "pending", FP: fpNewcomer},
	}}
	r.ops.nodes = roster
	r.ops.gatewayGuestSubnet = "10.200.0.0/20"

	_, err := r.ops.ApproveNode(ctx, Caller{Handle: "opsy"}, fpNewcomer)
	if !IsKind(err, KindInvalid) || !errors.Is(err, nodes.ErrGuestSubnetRequired) {
		t.Fatalf("ApproveNode without subnet = %v, want missing-subnet KindInvalid", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Code != "missing_guest_subnet" || e.Hint != nodeApproveUsage || e.ExitCode() != 2 {
		t.Errorf("error = %+v, want stable missing-subnet usage error", e)
	}
	if roster.configuredCalls != 0 || roster.legacyCalls != 0 {
		t.Errorf("missing config reached roster: configured=%d legacy=%d",
			roster.configuredCalls, roster.legacyCalls)
	}
}

func TestConfiguredApprovalValidationAndTaxonomy(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name          string
		gatewaySubnet string
		request       NodeApprovalConfig
		rosterErr     error
		kind          Kind
		code          string
	}{
		{
			name: "bad node subnet", gatewaySubnet: "10.200.0.0/20",
			request: NodeApprovalConfig{GuestSubnet: "not-a-cidr"},
			kind:    KindInvalid, code: "bad_guest_subnet",
		},
		{
			name: "bad grpc address", gatewaySubnet: "10.200.0.0/20",
			request: NodeApprovalConfig{GuestSubnet: "10.201.0.0/20", GRPCAddr: "100.64.0.12"},
			kind:    KindInvalid, code: "bad_grpc_addr",
		},
		{
			name:    "missing gateway subnet",
			request: NodeApprovalConfig{GuestSubnet: "10.201.0.0/20"},
			kind:    KindDisabled, code: "gateway_guest_subnet_missing",
		},
		{
			name: "invalid gateway subnet", gatewaySubnet: "broken",
			request: NodeApprovalConfig{GuestSubnet: "10.201.0.0/20"},
			kind:    KindDisabled, code: "gateway_guest_subnet_invalid",
		},
		{
			name: "roster overlap", gatewaySubnet: "10.200.0.0/20",
			request:   NodeApprovalConfig{GuestSubnet: "10.201.0.0/20"},
			rosterErr: fmt.Errorf("%w: 10.201.0.0/20 overlaps node-a", nodes.ErrGuestSubnetOverlap),
			kind:      KindConflict, code: "guest_subnet_overlap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			roster := &configuredRoster{
				list:       []NodeInfo{{Name: "newcomer", Status: "pending", FP: fpNewcomer}},
				approveErr: tc.rosterErr,
			}
			r.ops.nodes = roster
			r.ops.gatewayGuestSubnet = tc.gatewaySubnet

			_, err := r.ops.ApproveNode(ctx, Caller{Handle: "opsy"}, fpNewcomer, tc.request)
			if !IsKind(err, tc.kind) {
				t.Fatalf("err = %v, want %s", err, tc.kind)
			}
			var e *Error
			errors.As(err, &e)
			if e.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Code, tc.code)
			}
			if tc.rosterErr != nil {
				if !errors.Is(err, nodes.ErrGuestSubnetOverlap) {
					t.Error("overlap sentinel was lost")
				}
				if e.Details["guest_subnet"] != "10.201.0.0/20" {
					t.Errorf("overlap details = %v", e.Details)
				}
				if roster.configuredCalls != 1 || roster.legacyCalls != 0 {
					t.Errorf("overlap calls: configured=%d legacy=%d",
						roster.configuredCalls, roster.legacyCalls)
				}
			} else if roster.configuredCalls != 0 || roster.legacyCalls != 0 {
				t.Errorf("validation error reached roster: configured=%d legacy=%d",
					roster.configuredCalls, roster.legacyCalls)
			}
		})
	}
}

func TestLegacyRosterKeepsLegacyApprovalDuringMigration(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	legacy := r.withNodes()

	got, err := r.ops.ApproveNode(ctx, Caller{Handle: "opsy"}, fpNewcomer)
	if err != nil {
		t.Fatalf("ApproveNode through legacy roster: %v", err)
	}
	if got.Status != "approved" {
		t.Errorf("legacy approval = %+v", got)
	}
	if calls := r.calls.all(); !slices.Contains(calls, "ApproveNode "+fpNewcomer+" by=opsy") {
		t.Errorf("legacy approval call missing from %v", calls)
	}
	if _, ok := any(legacy).(NodeConfiguredApprover); ok {
		t.Fatal("legacy test roster unexpectedly exposes configured approval")
	}
}

func TestNodeInfoRendersTrustedNetworkAndCertificateMetadata(t *testing.T) {
	expires := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	revoked := expires.Add(-time.Hour)
	raw, err := json.Marshal(NodeInfo{
		Name: "node-a", Status: "disabled", FP: fpNodeB,
		GuestSubnet: "10.201.0.0/20", GRPCAddr: "100.64.0.12:9443",
		CertSerial: "01ab", CertExpiresAt: &expires, CertRevokedAt: &revoked,
	})
	if err != nil {
		t.Fatalf("Marshal NodeInfo: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal NodeInfo: %v", err)
	}
	for key, want := range map[string]any{
		"guest_subnet": "10.201.0.0/20",
		"grpc_addr":    "100.64.0.12:9443",
		"cert_serial":  "01ab",
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v (JSON: %s)", key, got[key], want, raw)
		}
	}
	if got["cert_expires_at"] != expires.Format(time.RFC3339) ||
		got["cert_revoked_at"] != revoked.Format(time.RFC3339) {
		t.Errorf("certificate times rendered unexpectedly: %s", raw)
	}
}
