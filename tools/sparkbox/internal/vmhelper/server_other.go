//go:build !linux

package vmhelper

import (
	"context"
	"errors"
	"log"
)

// ServerOptions mirrors the Linux definition field for field. It exists so the
// package compiles (and its protocol tests run) on a developer's Mac; keeping
// the two in step is not cosmetic, because TestServerOwnsEveryExecutableAndPath
// checks THIS one — the boundary rule it enforces would go unchecked on every
// non-Linux run if a field were added only to server_linux.go.
type ServerOptions struct {
	SocketPath                                  string
	Backend                                     Backend
	QemuBin, MachineType                        string
	FirecrackerBin, KernelPath, VMStateDir      string
	ChrootBase                                  string
	Subnet, Subnet6                             string
	JailerUIDBase, ControllerUID, ControllerGID int
	RestrictInternalEgress                      bool
	SluiceSocket                                string
	Logger                                      *log.Logger
}

func RunServer(context.Context, ServerOptions) error {
	return errors.New("the privileged VM helper requires Linux")
}
