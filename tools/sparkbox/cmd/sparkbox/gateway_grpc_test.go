package main

import (
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
)

func TestControlRolloutStageConfig(t *testing.T) {
	ssh := fleet.ControlTransportSSH
	grpcMode := fleet.ControlTransportGRPC
	tests := []struct {
		stage      string
		want       fleet.ControlRollout
		wantShadow bool
		wantErr    bool
	}{
		{stage: "inherit"},
		{
			stage: "shadow",
			want: fleet.ControlRollout{
				ReadOnly: ssh, Idempotent: ssh, Destructive: ssh,
			},
			wantShadow: true,
		},
		{
			stage: "read-only",
			want: fleet.ControlRollout{
				ReadOnly: grpcMode, Idempotent: ssh, Destructive: ssh,
			},
			wantShadow: true,
		},
		{
			stage: "idempotent",
			want: fleet.ControlRollout{
				ReadOnly: grpcMode, Idempotent: grpcMode, Destructive: ssh,
			},
			wantShadow: true,
		},
		{
			stage: "grpc",
			want: fleet.ControlRollout{
				ReadOnly: grpcMode, Idempotent: grpcMode, Destructive: grpcMode,
			},
			wantShadow: true,
		},
		{stage: "surprise", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.stage, func(t *testing.T) {
			got, shadow, err := controlRolloutStageConfig(tc.stage)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("rollout = %#v, want %#v", got, tc.want)
			}
			if shadow != tc.wantShadow {
				t.Fatalf("shadow = %v, want %v", shadow, tc.wantShadow)
			}
		})
	}
}
