package sshgw

import "testing"

func TestSSHHintUsesPublicHostWithoutChangingBaseDomain(t *testing.T) {
	g := &Gateway{
		domain:  "cluster.coreweave.app",
		sshHost: "ssh.cluster.coreweave.app",
	}
	if got := g.sshHint(); got != "ssh.cluster.coreweave.app" {
		t.Fatalf("sshHint() = %q", got)
	}
	if got := g.domainHint(); got != "cluster.coreweave.app" {
		t.Fatalf("domainHint() = %q", got)
	}
}

func TestSSHHintFallsBackToDomain(t *testing.T) {
	g := &Gateway{domain: "hivemind.tools"}
	if got := g.sshHint(); got != "hivemind.tools" {
		t.Fatalf("sshHint() = %q; want the base domain", got)
	}
}
