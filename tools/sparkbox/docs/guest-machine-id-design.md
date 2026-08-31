# The machine id a guest boots with, and the journal nobody can read

Why `journalctl` returns "No entries" on a freshly created sandbox, why a fork
carries its ancestors' system logs, and what to change so neither is true.

## Status

Diagnosed on the live CKS cluster on 2026-08-31, from three sandboxes. Nothing
here is built. The fix is two edits in two places that already do adjacent work,
and it is small enough that the argument below is longer than the change.

The bug is **first-boot only** and self-corrects on the second cold boot, which
is exactly what made it invisible until somebody installed a unit that runs at
boot and went looking for why it had not.

---

# Part 1 — the symptom

On a sandbox created minutes ago:

```
$ sudo journalctl -u dev-stack
No journal files were found.
-- No entries --
$ sudo journalctl --disk-usage
Archived and active journals take up 0B in the file system.
```

Both are false. journald is running, has spent 24 seconds of CPU, and is writing
8 MB files that are being appended to *right now*:

```
$ sudo ls -la /var/log/journal/f17029a30a434bc090f2820f082b9f1c/
-rw-r----- 1 root systemd-journal 8388608 Aug 31 20:49 system.journal
-rw-r----- 1 root systemd-journal 8388608 Aug 31 20:49 user-1000.journal
$ cat /etc/machine-id
e122e755d067a7a647cab4c1094ae40f
```

journald is writing under one machine id and `journalctl` is reading under
another. Nothing is lost; it is merely unaddressable, and every tool that
reports "no journal" is reporting the absence of a *directory*, not of logs.

# Part 2 — the mechanism

`f17029a30a434bc090f2820f082b9f1c` is the machine id **baked into the base
rootfs image**. It appears on three sandboxes with three different lineages,
including one that has never been forked from any of the others.

`hack/build-rootfs.sh` contains no handling of `/etc/machine-id` at all. Whatever
the Docker image and the ext4 conversion leave in that file ships to every
sandbox on the host.

So on a sandbox's first boot:

1. PID 1 reads `/etc/machine-id`, finds the inherited value, and uses it. The
   `systemd.machine_id=` on the command line (`fc.go:kernelArgs`, from
   `machineIDFor(name)`) is not consulted, because the file is already
   initialised — that argument only decides an *uninitialised* id.
2. journald starts and opens `/var/log/journal/<inherited-id>/`.
3. The correct per-sandbox id is written to `/etc/machine-id`.
4. For the remainder of that boot, journald writes to one directory and every
   reader looks in the other.

The timestamps are unambiguous. On the sandbox above, journald started at
`19:36:29`, its journal directory was written at `19:36:30.060`, and
`/etc/machine-id` was written at `19:36:30.084` — **24 milliseconds later**.

`machineIDFor("hmdev")` is `e122e755d067a7a647cab4c1094ae40f`, which is what
both the command line and the file say. The per-sandbox id mechanism works
perfectly. It is only ever *late*.

## Why it self-corrects

On the second cold boot, `/etc/machine-id` already holds the right value, PID 1
uses it, journald opens the right directory, and `journalctl` works. Both other
sandboxes examined were past their first boot and both were fine.

That is the whole reason this survived: it is invisible unless you look during
the one boot where it is true, and the natural time to look is the first boot.

## `sparkbox-identity-reset` is not the fix and cannot be

`deploy/install-guest-identity.sh` already writes `/etc/machine-id` from the
command line, and its own comment states the problem exactly:

> PID 1 has already read /etc/machine-id by now, so the cmdline is what actually
> gives this boot its own id; this keeps the file honest for every boot after.

"Honest for every boot after" is precisely the behaviour observed. The unit is
`WantedBy=multi-user.target`; journald starts long before `basic.target`. There
is no ordering that fixes this, because the read it is racing is PID 1's, and
PID 1 runs before every unit.

# Part 3 — what it costs

**A sandbox is journal-blind on its first boot.** That is the boot where a
newly-installed unit is most likely to fail and where somebody is most likely to
be watching. A `setup-dev`-style skill that installs a boot unit and then asks
"did it work?" gets `-- No entries --` and no way to tell a broken unit from a
missing journal.

**Journal directories accumulate down a fork chain.** `sanitizeTemplate`
(`fc.go:1974`) removes `etc/machine-id` — but `fc.go:1291` only runs it when
host rootfs mounts are permitted, and the live cluster runs with
`--disable-host-rootfs-mounts`. So on CKS a template keeps the captured
sandbox's machine id *and* its `/var/log/journal/<id>/`. One sandbox two forks
deep held three directories and 51 MB, two thirds of it inherited. journald's
default cap is 10% of the filesystem, so on the 25 GB per-VM allowance this has
room to become real money.

**A fork carries its ancestors' system logs, and they are not its own.** This is
the part that matters beyond tidiness. A bound tag template hands every sandbox
created from it the full system journal of the machine it was captured from:
sudo command lines, session records, unit output, request paths with their query
strings. Reading one of the inherited journals surfaced
`/v1/sessions?user_id=…` from a browser session belonging to the template's
author.

It is the same *class* as the secret-strip leak fixed in #47 and nowhere near
the same severity — a journal is not a credential — but a template is a thing
one person builds and other people boot, and it should not carry its author's
logs any more than it should carry their secrets.

# Part 4 — the fix

An empty `/etc/machine-id` is systemd's documented "uninitialised, first boot"
state, and it is exactly the state in which `systemd.machine_id=` is honoured.
Every part of this fix is a way of making sure the file is empty in anything
that will be booted as a new sandbox.

**Layer 1 — the base image.** In `hack/build-rootfs.sh`, before the ext4 is
sealed: truncate `/etc/machine-id` to zero bytes, remove
`/var/lib/dbus/machine-id`, and remove `/var/log/journal/*`. PID 1 then takes
the id from the command line on the very first boot, journald opens the right
directory, and boot one behaves like boot two. This fixes every host, including
those that never snapshot anything.

Truncate rather than delete. A zero-byte file is what systemd's own
`systemd-machine-id-setup` leaves and what image builders are expected to ship;
it is also the form that still works if `/etc` is ever mounted read-only, where
systemd bind-mounts a transient id over the existing file and has nothing to
mount over if the file is absent. (`sanitizeTemplate` currently deletes it. That
works today because `/etc` is writable, and it should be changed to truncate for
the same reason, in the same pass.)

**Layer 2 — the capture path, for hosts that cannot mount.** The clears above
belong in the guest-side pre-capture hook that already exists: `stripEnvForPack`
and `refreshToolsForPack` both run inside the guest, before the pause, on both
the local and the fleet path. Adding the same three clears there gives CKS the
same clean template a mount-capable host gets from `sanitizeTemplate`.

Doing it guest-side also lets it become the **single** implementation. Today
`sanitizeTemplate` and `sparkbox-identity-reset` each own half of "make this
rootfs stop being the machine it was captured from", they overlap, and only one
of them runs on the cluster that matters. One guest-side hook that runs on every
capture is one behaviour instead of two that have to agree.

**Not part of the fix: reordering the reset unit.** Running
`sparkbox-identity-reset` before journald is the obvious-looking move and it is
wrong twice over. It only patches boots where the file is *already* stale, so it
would leave the base-image case untouched; and making it work needs
`DefaultDependencies=no` plus hand-written `sysinit.target` ordering against a
writable `/etc`, which is a large amount of fragile boot-order surface to buy a
symptom. Clearing the file where templates are made means the stale state does
not exist, and nothing has to win a race.

# Part 5 — the workaround, which is worth shipping anyway

`journalctl --merge` (`-m`) reads across every machine id present on the disk
and works on any sandbox in any boot:

```
sudo journalctl -m -u <unit>
```

That belongs in the guest documentation and in any skill that installs a boot
unit regardless of this fix, because it is also how somebody reads the logs of
the boot *before* the one they are sitting in — which a fork's owner may well
want, and which the fix does not otherwise provide.

# Part 6 — verification

The bug is only observable on a first boot, so the test has to make one.

1. Build a rootfs with Layer 1 applied and confirm `/etc/machine-id` is zero
   bytes in the image and `/var/log/journal` is empty.
2. Create a sandbox from it. On the **first** boot, assert
   `/etc/machine-id` equals `machineIDFor(<name>)`, that `/var/log/journal`
   contains exactly that one directory, and that `journalctl -u docker` returns
   entries.
3. Capture a snapshot on a `--disable-host-rootfs-mounts` host, fork it, and
   assert the same three things on the fork's first boot — plus that the fork's
   `/var/log/journal` does **not** contain the parent's directory.

Step 3 is the one that fails today and the one that needs a real CKS run;
steps 1 and 2 are checkable on any Linux/KVM host.

# Part 7 — an unrelated bug found while testing this

Rebooting a guest from inside (`systemctl reboot`) exits the Firecracker VMM but
leaves the manager's record at `StateRunning`. `ensureReady` classifies
`StateRunning` as "warm" and never probes liveness, so it never reaches
`resumeOrRecreate`; `pause` fails on the now-absent `fc.sock`; and `Reboot`
pauses first, so it fails the same way. `load()` is a plain unmarshal and
preserves the state verbatim, so restarting the gateway does not clear it
either — which is the real reason `docs/deploy-cks.md` says to pause sandboxes
before rolling the node.

The result is a sandbox that no user-facing verb can recover, with its rootfs
sitting intact on the node. It wants its own fix — most likely a liveness check
in `ensureReady`, or a `Pause` that treats an absent socket as "already
stopped" — and its own document. It is recorded here only because this is where
it was found.
