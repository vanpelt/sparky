//go:build !linux

package vmhelper

import (
	"context"
	"errors"
	"log"
)

type ServerOptions struct {
	SocketPath, FirecrackerBin, KernelPath, VMStateDir, ChrootBase string
	Subnet, Subnet6                                                string
	JailerUIDBase, ControllerUID, ControllerGID                    int
	RestrictInternalEgress                                         bool
	SluiceSocket                                                   string
	Logger                                                         *log.Logger
}

func RunServer(context.Context, ServerOptions) error {
	return errors.New("the privileged VM helper requires Linux")
}
