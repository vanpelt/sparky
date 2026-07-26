package hostsetup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This file answers one question before anything is provisioned: does this host
// ALREADY hold a live sparkbox somewhere other than where this run is about to
// put one?
//
// It exists because of the DGX (F4). That box predates `sparkbox setup`: its
// state, images and kernel sit flat under /srv/sparkbox, while setup's layout is
// /srv/sparkbox/data/{state,images} on an XFS reflink volume. Every Satisfied
// probe was an existence check, so a `setup --dry-run` there reported
// "users.conf ✓ already satisfied (2 user(s)) … systemd-units ✓ already
// satisfied" and planned a 300 GB volume plus a fresh artifact download. Running
// it would have produced a host with TWO data roots — a new empty one that the
// rewritten unit points at, and the live one holding the sqlite DB, the fleet
// keys and the certmagic cache — with no error anywhere and no way to tell from
// the output that anything had happened.
//
// So: find the live state directory, and if it is not the one this config
// names, either adopt it (--adopt-legacy) or refuse. Never both roots.

// stateMarkers are the files and directories that mean "a sparkbox gateway has
// actually run here", in the order they are worth reporting. sparkbox.db is the
// conclusive one — users, routes, secrets, node roster and placement all live in
// it — but a host stopped before its first login still has keys and a cert cache
// worth not stranding.
var stateMarkers = []string{
	"sparkbox.db",
	"sandboxes.json",
	"fc-vms",
	"certmagic",
	"autocert",
	"gateway_host_key.pem",
	"oidc_signing_key.pem",
}

// stateDirMarkers lists which markers a directory actually has. An empty result
// means "nothing here that would be lost", which is the only safe reading of a
// directory setup is about to provision beside.
func stateDirMarkers(dir string) []string {
	if dir == "" {
		return nil
	}
	var found []string
	for _, m := range stateMarkers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			found = append(found, m)
		}
	}
	return found
}

// hostLayout is a populated sparkbox state directory that is NOT the one this
// config names.
type hostLayout struct {
	stateDir string   // where the live state actually is
	imageDir string   // the rootfs templates that go with it ("" if none found)
	markers  []string // what made it look live
	source   string   // how we found it, for the message
	// imagesOnly marks the half of this problem that has nothing to do with the
	// state directory: this run already names the live state dir, but the rootfs
	// templates that belong with it are somewhere else. See detectLayout.
	imagesOnly bool
}

// detectLayout looks for a live state directory somewhere other than
// cfg.StateDir, and reports whether cfg.StateDir is itself already populated.
//
// Two candidates, in descending order of authority:
//
//  1. the --state-dir of the sparkbox.service actually installed on this host.
//     That is the only source that says what the RUNNING gateway uses, as
//     opposed to what some layout convention suggests it might.
//  2. <root>/state, the pre-setup flat layout. A host whose unit was hand-written
//     (or removed) still has its state there.
//
// The images are compared too, not only the state. `--state-dir` and
// `--image-dir` are separate flags and every message here names them as a pair,
// so half-typing the pair is an ordinary slip: `setup --state-dir
// /srv/sparkbox/state` on the DGX (no --image-dir) matched the state candidate,
// found nothing to report, and planned a 300 GB volume plus a full artifact
// re-download — because ImageDir was still <root>/data/images, which stepDataVolume
// duly considered applicable. The result is the half-migration this file exists
// to prevent: state flat on the root filesystem, images on a new volume, and the
// live <root>/images orphaned.
func detectLayout(e *Env) (found *hostLayout, ownPopulated bool, err error) {
	cfg := e.Cfg
	ownPopulated = len(stateDirMarkers(cfg.StateDir)) > 0

	unitState, unitImages := installedUnitDirs(e)
	candidates := []hostLayout{
		{stateDir: unitState, imageDir: unitImages, source: "the installed " + serviceUnit},
		{
			stateDir: filepath.Join(cfg.Root, "state"),
			imageDir: filepath.Join(cfg.Root, "images"),
			source:   "the pre-setup flat layout under " + cfg.Root,
		},
	}
	for _, c := range candidates {
		if c.stateDir == "" {
			continue
		}
		// An images directory only counts if it holds something. An empty one is
		// nothing to strand, and treating it as a layout would make setup refuse
		// hosts it has no reason to refuse.
		if c.imageDir != "" && len(dirPayload(c.imageDir)) == 0 {
			c.imageDir = ""
		}
		if sameDir(c.stateDir, cfg.StateDir) {
			// This run already names the live state. The templates can still be
			// elsewhere — that is the half-typed pair above.
			if c.imageDir == "" || sameDir(c.imageDir, cfg.ImageDir) {
				continue
			}
			c.imagesOnly = true
			c.markers = stateDirMarkers(c.stateDir)
			return &c, ownPopulated, nil
		}
		markers := stateDirMarkers(c.stateDir)
		if len(markers) == 0 {
			continue
		}
		c.markers = markers
		return &c, ownPopulated, nil
	}
	return nil, ownPopulated, nil
}

// reconcileLayout is the guard Provision runs before it touches anything: adopt
// the live layout, or refuse and say exactly how to migrate. It is deliberately
// NOT a Step — a step that has to run before the plan is even printed is not a
// step, and in --dry-run the whole point is that the plan describes the host in
// front of you rather than the one setup would prefer.
func reconcileLayout(e *Env) error {
	found, ownPopulated, err := detectLayout(e)
	if err != nil || found == nil {
		return err
	}
	if ownPopulated && !found.imagesOnly {
		// Both roots already hold state. Adoption cannot help — there is no
		// single "the live one" to adopt — and provisioning would make a third
		// mess on top of the second. Only a human can say which is current.
		return fmt.Errorf("two populated sparkbox state directories on one host:\n"+
			"    %s  (%s)   — found via %s\n"+
			"    %s  (%s)   — this run's --state-dir\n"+
			"  setup cannot tell which one the gateway should serve, and running would leave both.\n"+
			"  Stop the service (systemctl stop sparkbox), decide which directory is current, move the\n"+
			"  other aside (mv %s %s.stale), and re-run.",
			found.stateDir, strings.Join(found.markers, ", "), found.source,
			e.Cfg.StateDir, strings.Join(stateDirMarkers(e.Cfg.StateDir), ", "),
			found.stateDir, found.stateDir)
	}
	if !e.Cfg.AdoptLegacy {
		return fmt.Errorf("%s", refuseLegacyMessage(e.Cfg, found))
	}
	// Adopt: point this run at the paths that are already live. Everything
	// downstream — the unit's ExecStart, the artifact destinations, the data
	// volume's own relevance — reads Cfg, so this one assignment is the whole
	// adoption.
	e.logf("== layout ==\n")
	if !found.imagesOnly {
		e.logf("   --adopt-legacy: using the existing state at %s (%s, via %s)\n",
			found.stateDir, strings.Join(found.markers, ", "), found.source)
	}
	e.Cfg.StateDir = found.stateDir
	if found.imageDir != "" {
		e.logf("   --adopt-legacy: using the existing rootfs templates at %s\n", found.imageDir)
		e.Cfg.ImageDir = found.imageDir
	}
	e.AdoptedLegacy = true
	return nil
}

// refuseLegacyMessage is the refusal an operator sees on a host like the DGX. It
// spells out both ways forward, because "refuses to run" without a next command
// is how a tool gets worked around with a shell script that does the damage
// anyway.
func refuseLegacyMessage(cfg Config, found *hostLayout) string {
	var b strings.Builder
	if found.imagesOnly {
		// The state directory is already the right one, so none of the migration
		// choreography below applies: what is wrong is one flag.
		fmt.Fprintf(&b, "this host's rootfs templates already live at %s (%s), found via %s,\n",
			found.imageDir, strings.Join(dirPayload(found.imageDir), ", "), found.source)
		fmt.Fprintf(&b, "  but this run is configured for --image-dir %s, beside a --state-dir it agrees with.\n\n", cfg.ImageDir)
		fmt.Fprintf(&b, "  Provisioning would build a %dG volume at %s, re-download the release into it, and point\n",
			cfg.DataVolumeGB, cfg.dataDir())
		fmt.Fprintf(&b, "  the unit at the new (empty) images directory while the live templates stayed behind —\n")
		fmt.Fprintf(&b, "  a host with its state on one filesystem and its images on another, and no error anywhere.\n\n")
		b.WriteString("  Name it, which is almost certainly what you meant (the two flags go together):\n")
		fmt.Fprintf(&b, "      sparkbox setup --state-dir %s --image-dir %s   <the same flags you just passed>\n\n",
			cfg.StateDir, found.imageDir)
		b.WriteString("  Or let setup find both for you:\n")
		b.WriteString("      sparkbox setup --adopt-legacy   <the same flags you just passed>")
		return b.String()
	}
	fmt.Fprintf(&b, "this host already runs sparkbox from %s (%s), found via %s,\n",
		found.stateDir, strings.Join(found.markers, ", "), found.source)
	fmt.Fprintf(&b, "  but this run is configured for --state-dir %s.\n\n", cfg.StateDir)
	fmt.Fprintf(&b, "  Provisioning would build a SECOND data root beside the live one: a %dG volume at %s\n",
		cfg.DataVolumeGB, cfg.dataDir())
	fmt.Fprintf(&b, "  and a fresh artifact download, with the rewritten unit serving from the new (empty)\n")
	fmt.Fprintf(&b, "  directory while the sqlite DB, the fleet keys and the cert cache stayed behind in\n")
	fmt.Fprintf(&b, "  %s. Nothing would report an error. So setup refuses.\n\n", found.stateDir)
	b.WriteString("  Adopt what is there (recommended — nothing moves, nothing is re-issued):\n")
	b.WriteString("      sparkbox setup --adopt-legacy   <the same flags you just passed>\n\n")
	fmt.Fprintf(&b, "  Or migrate onto the %s layout, carrying the state across:\n", cfg.dataDir())
	b.WriteString("      systemctl stop sparkbox\n")
	fmt.Fprintf(&b, "      mv %s %s.pre-migrate\n", found.stateDir, found.stateDir)
	if found.imageDir != "" {
		fmt.Fprintf(&b, "      mv %s %s.pre-migrate\n", found.imageDir, found.imageDir)
	}
	b.WriteString("      sparkbox setup   <the same flags>          # builds and mounts " + cfg.dataDir() + "\n")
	b.WriteString("      systemctl stop sparkbox\n")
	fmt.Fprintf(&b, "      cp -a %s.pre-migrate/. %s/                 # -a keeps certmagic/ intact\n",
		found.stateDir, cfg.StateDir)
	b.WriteString("      systemctl start sparkbox\n\n")
	b.WriteString("  (cp -a rather than mv for the state: the cert cache under it is DNS-01 issued under\n")
	b.WriteString("   Let's Encrypt's 5-duplicate-certs-per-week cap, so losing it can cost you a week.)")
	return b.String()
}

// --- reading the installed unit ---------------------------------------------

// installedUnitDirs reads the --state-dir and --image-dir out of the
// sparkbox.service actually installed on this host.
//
// This is the authoritative answer to "where does the running gateway keep its
// data", and it is the signal a layout convention cannot supply: an operator who
// passed --state-dir once has a host whose real layout matches no default at
// all. Both are empty when the unit is absent or does not name them, which is
// the fresh-host case.
func installedUnitDirs(e *Env) (stateDir, imageDir string) {
	b, err := os.ReadFile(filepath.Join(e.SystemdDir, serviceUnit))
	if err != nil {
		return "", ""
	}
	exec := execStartLine(string(b))
	return execStartFlag(exec, "--state-dir"), execStartFlag(exec, "--image-dir")
}

// execStartLine extracts the unit's ExecStart= as ONE line, joining systemd's
// backslash continuations. The template spreads the command over eight lines, so
// a naive per-line scan finds "--state-dir" on a line of its own and the value
// on the same line only by luck.
func execStartLine(unit string) string {
	var b strings.Builder
	joining := false
	for _, line := range strings.Split(unit, "\n") {
		t := strings.TrimSpace(line)
		if !joining {
			if !strings.HasPrefix(t, "ExecStart=") {
				continue
			}
			t = strings.TrimPrefix(t, "ExecStart=")
			t = strings.TrimPrefix(t, "-") // systemd's "ignore failure" prefix
		}
		joining = strings.HasSuffix(t, `\`)
		b.WriteString(strings.TrimSuffix(t, `\`))
		b.WriteString(" ")
		if !joining {
			break // one ExecStart is all this unit has
		}
	}
	return strings.TrimSpace(b.String())
}

// execStartFlag pulls one flag's value out of a joined ExecStart. Both spellings
// (`--flag value` and `--flag=value`) because either is legal and the unit could
// have been hand-edited into the other one.
func execStartFlag(exec, flag string) string {
	fields := strings.Fields(exec)
	for i, f := range fields {
		if name, val, hasEq := strings.Cut(f, "="); hasEq && name == flag {
			return val
		}
		if f != flag {
			continue
		}
		if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
			return fields[i+1]
		}
	}
	return ""
}

// --- path helpers -----------------------------------------------------------

// sameDir compares two directory paths for identity, tolerating trailing
// slashes and unclean forms. It deliberately does not resolve symlinks: these
// paths come from a unit file and a flag, and a Stat-based comparison would
// answer "different" for two names of one directory only when one of them does
// not exist yet — which is exactly when this is called.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// underDir reports whether child is parent or lives inside it.
func underDir(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// dirPayload lists what would be HIDDEN by mounting a filesystem over dir:
// files, and directories that are not empty. An empty directory left behind by
// an interrupted run is not payload, and treating it as such would make setup
// un-re-runnable after its own failure.
func dirPayload(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range entries {
		if ent.IsDir() {
			sub, err := os.ReadDir(filepath.Join(dir, ent.Name()))
			if err != nil || len(sub) == 0 {
				continue
			}
			out = append(out, ent.Name()+"/")
			continue
		}
		out = append(out, ent.Name())
	}
	return out
}
