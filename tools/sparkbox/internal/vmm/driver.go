// Package vmm defines the driver interface sparkbox uses to manage sandbox
// VMs. Two implementations exist: mock (in-process fake VMs, no KVM needed,
// used for dev and tests) and firecracker (real microVMs via
// firecracker-go-sdk, requires /dev/kvm).
package vmm

import "context"

type State string

const (
	StateRunning State = "running"
	StatePaused  State = "paused"
)

// Config describes the sandbox VM to create.
type Config struct {
	// Name is the unique sandbox name; drivers use it to key all resources.
	Name string
	// Image is the rootfs template name (e.g. "ubuntu"). The firecracker
	// driver resolves it to <imageDir>/<Image>.ext4; the mock driver ignores it.
	Image string
	VCPUs int64
	MemMB int64
	// GatewayPublicKey is the SSH public key (authorized_keys line) that the
	// gateway will authenticate with when dialing into the VM.
	GatewayPublicKey string
}

// Instance is a running or paused sandbox VM.
type Instance struct {
	Name    string
	State   State
	SSHAddr string // host:port the gateway dials to reach the VM's sshd
	SSHUser string // user the gateway logs in as
}

// Driver manages sandbox VM lifecycles on a single host.
//
// Pause must persist enough state that Resume brings the sandbox back with
// its filesystem intact (the firecracker driver snapshots memory too; the
// mock driver only preserves the workdir).
type Driver interface {
	Create(ctx context.Context, cfg Config) (*Instance, error)
	Pause(ctx context.Context, name string) error
	Resume(ctx context.Context, name string) (*Instance, error)
	Destroy(ctx context.Context, name string) error
	// Close releases driver resources (running fake VMs, VMM processes).
	Close() error
}
