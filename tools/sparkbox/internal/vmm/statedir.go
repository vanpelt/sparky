package vmm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// driverMarkerName is the file inside a VM state directory naming the VMM that
// owns what is in it.
const driverMarkerName = "vmm-driver"

// ClaimStateDir records which driver owns a VM state directory, and refuses to
// hand a populated one to a different driver.
//
// # WHY THIS EXISTS, AND WHY IT IS NOT PARANOIA
//
// Drivers key a sandbox's disk by name under their own subdirectory —
// firecracker uses fc-vms/<name>, qemu uses qemu-vms/<name> — and they write
// different snapshots (a mem.snap/state.snap pair versus one state.migrate).
// The separation is deliberate and it works. What it does NOT do is make
// changing a node's --driver safe.
//
// Flip the flag on a node with live sandboxes and nothing errors. The new
// driver looks under ITS subdirectory, finds nothing for any name the ledger
// knows, and every sandbox takes the path a sandbox with no disk takes: a cold
// boot from the base image. The tenant's home directory, their git checkouts
// and their running work are all still on the disk, in the other driver's tree,
// belonging to nobody. The sandbox comes back looking freshly minted. There is
// no error anywhere, and the first person to notice is the user whose work is
// gone.
//
// Rolling BACK is worse, because it is the sanctioned move: an operator who
// tries qemu, dislikes it and returns to firecracker leaves every ledger row
// pointing at a rootfs the firecracker driver's RootfsPresent cannot see, which
// reports the sandbox unrecoverable rather than missing — and the cleanup that
// follows deletes the ledger row while the disk survives.
//
// So the state directory carries a marker naming its VMM, and a driver that
// does not match it refuses to start. A refusal at startup is loud, immediate,
// and costs an operator a flag. The alternative is silent and costs a user
// their work.
//
// The escape hatch is deliberately not "force it and hope". A node genuinely
// changing VMM has to empty the old driver's tree first — archive each sandbox
// (an archive is a plain compacted rootfs and crosses drivers cleanly, because
// a restore from one is a cold boot rather than a snapshot resume), or destroy
// them. Once nothing is left, allowChange records the new owner. Callers pass
// it from an explicit operator flag and should log loudly when it is set.
//
// An empty or absent marker is not an error: it is the ordinary first start,
// and it is also every node that upgraded into this check with a directory a
// previous release wrote. Those are claimed on sight by whichever driver is
// running, which is correct — that driver is the one that wrote what is there.
func ClaimStateDir(stateDir, driver string, allowChange bool) error {
	if stateDir == "" || driver == "" {
		return errors.New("vmm: claim state dir: both a directory and a driver name are required")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("vmm: claim state dir: %w", err)
	}
	marker := filepath.Join(stateDir, driverMarkerName)

	switch existing, err := readMarker(marker); {
	case err != nil:
		return err
	case existing == driver:
		return nil
	case existing == "":
		return writeMarker(marker, driver)
	case allowChange:
		return writeMarker(marker, driver)
	default:
		return fmt.Errorf(
			"vmm: %s was created by the %q driver and this process is the %q driver; "+
				"refusing to start, because %q keeps its sandboxes' disks and snapshots in a layout "+
				"%q does not read, so every existing sandbox would silently cold-boot from its base "+
				"image and its owner's work would be left behind unreferenced. To change this node's "+
				"VMM, first archive or destroy every sandbox on it, then start once with "+
				"--allow-vmm-change to record the new owner",
			stateDir, existing, driver, existing, driver)
	}
}

// readMarker returns the recorded driver name, or "" when there is no marker.
// A marker that exists but is empty or whitespace counts as absent: a torn
// write should not brick a node, and the recovery ("claim it") is the same one
// a fresh directory gets.
func readMarker(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("vmm: read %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// writeMarker replaces the marker atomically. A half-written marker would read
// as a driver nobody has ever heard of and refuse every subsequent start, which
// is a worse failure than the one this file exists to prevent.
func writeMarker(path, driver string) error {
	tmp := path + ".next"
	if err := os.WriteFile(tmp, []byte(driver+"\n"), 0o644); err != nil {
		return fmt.Errorf("vmm: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("vmm: write %s: %w", path, err)
	}
	return nil
}
