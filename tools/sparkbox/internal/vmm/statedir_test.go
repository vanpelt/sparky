package vmm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimStateDirFirstStartClaims(t *testing.T) {
	dir := t.TempDir()
	if err := ClaimStateDir(dir, "firecracker", false); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, driverMarkerName))
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != "firecracker" {
		t.Fatalf("marker = %q, want firecracker", got)
	}
	// Restarting the same driver is the common case and must stay silent.
	if err := ClaimStateDir(dir, "firecracker", false); err != nil {
		t.Fatalf("reclaim by the same driver: %v", err)
	}
}

func TestClaimStateDirRefusesAnotherDriver(t *testing.T) {
	dir := t.TempDir()
	if err := ClaimStateDir(dir, "firecracker", false); err != nil {
		t.Fatalf("setup: %v", err)
	}
	err := ClaimStateDir(dir, "qemu", false)
	if err == nil {
		t.Fatal("a second driver claimed a directory the first one owns; " +
			"every sandbox in it would have cold-booted from its base image")
	}
	// The message has to name both drivers and the way out, because the person
	// reading it is mid-deploy and the alternative to understanding it is
	// passing the override flag to make the error go away.
	for _, want := range []string{"firecracker", "qemu", "--allow-vmm-change", "base image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// The refusal must not have rewritten the marker: a failed start that
	// quietly took ownership would let the NEXT start succeed and do the damage.
	b, _ := os.ReadFile(filepath.Join(dir, driverMarkerName))
	if got := strings.TrimSpace(string(b)); got != "firecracker" {
		t.Fatalf("marker = %q after a refused claim, want firecracker", got)
	}
}

func TestClaimStateDirAllowChange(t *testing.T) {
	dir := t.TempDir()
	if err := ClaimStateDir(dir, "firecracker", false); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := ClaimStateDir(dir, "qemu", true); err != nil {
		t.Fatalf("allowChange claim: %v", err)
	}
	// And it sticks, so the operator does not have to pass the flag forever.
	if err := ClaimStateDir(dir, "qemu", false); err != nil {
		t.Fatalf("after allowChange, plain start: %v", err)
	}
	if err := ClaimStateDir(dir, "firecracker", false); err == nil {
		t.Fatal("going back was not refused")
	}
}

func TestClaimStateDirTornMarkerIsClaimable(t *testing.T) {
	// A marker that exists but is empty must not brick the node: bricking is a
	// worse failure than the one this guard prevents, and the directory it
	// describes is whatever the running driver put there anyway.
	for _, content := range []string{"", "  \n", "\n"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, driverMarkerName), []byte(content), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := ClaimStateDir(dir, "qemu", false); err != nil {
			t.Fatalf("marker %q: %v", content, err)
		}
	}
}

func TestClaimStateDirCreatesMissingDir(t *testing.T) {
	// The state dir is created by the driver, and on a fresh host the claim can
	// run first. It must not fail on a path that does not exist yet.
	dir := filepath.Join(t.TempDir(), "hot", "vms")
	if err := ClaimStateDir(dir, "qemu", false); err != nil {
		t.Fatalf("claim into a missing dir: %v", err)
	}
}

func TestClaimStateDirRejectsEmptyArguments(t *testing.T) {
	if err := ClaimStateDir("", "qemu", false); err == nil {
		t.Error("empty state dir was accepted")
	}
	if err := ClaimStateDir(t.TempDir(), "", false); err == nil {
		t.Error("empty driver name was accepted; it would claim the dir for nobody")
	}
}
