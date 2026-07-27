package fleet

import (
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// ShadowInventoryReport compares the independently populated SSH and gRPC
// caches without exposing sandbox names as rollout dimensions.
type ShadowInventoryReport struct {
	Available bool
	Match     bool

	SSHSandboxes  int
	GRPCSandboxes int
	MissingOnSSH  int
	MissingOnGRPC int
	SandboxDiffs  int

	SSHSnapshots  int
	GRPCSnapshots int
	SnapshotDiffs int

	CapacityDiff bool
	FactsDiff    bool
}

type ShadowInventoryObserver func(ShadowInventoryReport)

type shadowInventoryConfig struct {
	enabled  bool
	observer ShadowInventoryObserver
}

func (s *ControlSelector) ConfigureShadowInventory(enabled bool, observer ShadowInventoryObserver) {
	s.mu.Lock()
	s.shadow.enabled = enabled
	s.shadow.observer = observer
	s.mu.Unlock()
}

// CompareShadowInventory performs one side-effect-free comparison. It is safe
// to call while SSH remains authoritative and is therefore the rollout probe
// used before any operation class moves to gRPC.
func (s *ControlSelector) CompareShadowInventory() ShadowInventoryReport {
	s.mu.RLock()
	grpc, ssh := s.grpc, s.ssh
	node, metrics := s.node, s.metrics
	observer := s.shadow.observer
	s.mu.RUnlock()

	report := compareShadowInventory(grpc, ssh)
	outcome := "mismatch"
	if !report.Available {
		outcome = "unavailable"
	} else if report.Match {
		outcome = "match"
	}
	metrics.IncShadowInventory(node, outcome)
	if observer != nil {
		observer(report)
	}
	return report
}

func (s *ControlSelector) observeShadowInventory() {
	s.mu.RLock()
	enabled := s.shadow.enabled
	s.mu.RUnlock()
	if enabled {
		s.CompareShadowInventory()
	}
}

func (s *ControlSelector) setMetrics(node string, metrics *fleetmetrics.Registry) {
	s.mu.Lock()
	s.node, s.metrics = node, metrics
	s.mu.Unlock()
}

func compareShadowInventory(grpc *GRPCControl, ssh ControlPlane) ShadowInventoryReport {
	if grpc == nil || ssh == nil || !grpc.Healthy() {
		return ShadowInventoryReport{}
	}
	sshBoxes, grpcBoxes := ssh.Boxes(), grpc.Boxes()
	sshSnapshots, grpcSnapshots := ssh.Templates(), grpc.Templates()
	report := ShadowInventoryReport{
		Available: true, SSHSandboxes: len(sshBoxes), GRPCSandboxes: len(grpcBoxes),
		SSHSnapshots: len(sshSnapshots), GRPCSnapshots: len(grpcSnapshots),
	}
	sshByName := make(map[string]sandboxShadow, len(sshBoxes))
	for _, box := range sshBoxes {
		if box != nil {
			sshByName[box.Name] = sandboxShadowOf(box)
		}
	}
	grpcByName := make(map[string]sandboxShadow, len(grpcBoxes))
	for _, box := range grpcBoxes {
		if box != nil {
			grpcByName[box.Name] = sandboxShadowOf(box)
		}
	}
	for name, sshBox := range sshByName {
		grpcBox, ok := grpcByName[name]
		if !ok {
			report.MissingOnGRPC++
		} else if grpcBox != sshBox {
			report.SandboxDiffs++
		}
	}
	for name := range grpcByName {
		if _, ok := sshByName[name]; !ok {
			report.MissingOnSSH++
		}
	}

	sshSnaps := make(map[string]snapshotShadow, len(sshSnapshots))
	for _, snapshot := range sshSnapshots {
		if snapshot != nil {
			sshSnaps[snapshot.Name] = snapshotShadowOf(snapshot)
		}
	}
	grpcSnaps := make(map[string]snapshotShadow, len(grpcSnapshots))
	for _, snapshot := range grpcSnapshots {
		if snapshot != nil {
			grpcSnaps[snapshot.Name] = snapshotShadowOf(snapshot)
		}
	}
	for name, sshSnapshot := range sshSnaps {
		if grpcSnapshot, ok := grpcSnaps[name]; !ok || grpcSnapshot != sshSnapshot {
			report.SnapshotDiffs++
		}
	}
	for name := range grpcSnaps {
		if _, ok := sshSnaps[name]; !ok {
			report.SnapshotDiffs++
		}
	}
	report.CapacityDiff = capacityShadowOf(ssh.Capacity()) != capacityShadowOf(grpc.Capacity())
	report.FactsDiff = factsShadowOf(ssh.Facts()) != factsShadowOf(grpc.Facts())
	report.Match = report.MissingOnSSH == 0 && report.MissingOnGRPC == 0 &&
		report.SandboxDiffs == 0 && report.SnapshotDiffs == 0 &&
		!report.CapacityDiff && !report.FactsDiff
	return report
}

type sandboxShadow struct {
	Owner       string
	Image       string
	VCPUs       int64
	MemMB       int64
	State       vmm.State
	SSHUser     string
	CreatedAt   time.Time
	LastActive  time.Time
	Pinned      bool
	Ballooned   bool
	KeyFP       string
	NetRxBytes  uint64
	NetTxBytes  uint64
	ArchivedAt  time.Time
	DiskMB      int64
	DiskTotalMB int64
}

func sandboxShadowOf(box *host.Sandbox) sandboxShadow {
	return sandboxShadow{
		Owner: box.Owner, Image: box.Image, VCPUs: box.VCPUs, MemMB: box.MemMB,
		State: box.State, SSHUser: box.SSHUser, CreatedAt: box.CreatedAt,
		LastActive: box.LastActive, Pinned: box.Pinned, Ballooned: box.Ballooned,
		KeyFP: box.KeyFP, NetRxBytes: box.NetRxBytes, NetTxBytes: box.NetTxBytes,
		ArchivedAt: box.ArchivedAt, DiskMB: box.DiskMB, DiskTotalMB: box.DiskTotalMB,
	}
}

type snapshotShadow struct {
	Owner     string
	Image     string
	FromBox   string
	CreatedAt time.Time
}

func snapshotShadowOf(snapshot *host.Snapshot) snapshotShadow {
	return snapshotShadow{
		Owner: snapshot.Owner, Image: snapshot.Image,
		FromBox: snapshot.FromBox, CreatedAt: snapshot.CreatedAt,
	}
}

type capacityShadow struct {
	TotalVCPUs, TotalMemMB, BudgetMemMB, UsedVCPUs, UsedMemMB    int64
	EffectiveMemMB, ReserveMemMB, DiskPoolMBPerOwner, UsedDiskMB int64
	Running, Sandboxes                                           int
}

func capacityShadowOf(capacity host.NodeCapacity) capacityShadow {
	return capacityShadow{
		TotalVCPUs: capacity.TotalVCPUs, TotalMemMB: capacity.TotalMemMB,
		BudgetMemMB: capacity.BudgetMemMB, UsedVCPUs: capacity.UsedVCPUs,
		UsedMemMB: capacity.UsedMemMB, EffectiveMemMB: capacity.EffectiveMemMB,
		ReserveMemMB: capacity.ReserveMemMB, DiskPoolMBPerOwner: capacity.DiskPoolMBPerOwner,
		UsedDiskMB: capacity.UsedDiskMB, Running: capacity.Running,
		Sandboxes: capacity.Sandboxes,
	}
}

type factsShadow struct {
	Arch, OS, Release, Version, Driver string
	Archiving, Snapshots, Sluice       bool
	StartedAt                          time.Time
}

func factsShadowOf(facts Facts) factsShadow {
	return factsShadow{
		Arch: facts.Arch, OS: facts.OS, Release: facts.Release,
		Version: facts.Version, Driver: facts.Driver,
		Archiving: facts.Archiving, Snapshots: facts.Snapshots,
		Sluice: facts.Sluice, StartedAt: facts.StartedAt,
	}
}
