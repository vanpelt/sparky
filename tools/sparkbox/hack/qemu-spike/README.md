# QEMU spike scripts

Throwaway probes behind [docs/qemu-spike.md](../../docs/qemu-spike.md). They
drive QEMU by hand on the arm64 Mac dev box to answer "can it boot our guest,
snapshot it, and report balloon stats" before any driver existed. Kept so the
numbers in that document are checkable.

These are **not** a test harness. That is `internal/vmm/vmmtest`, driven by
`hack/parity/`.

Run order, all from inside the dev-box container machine
(`container machine run -i --root --name sparkbox -- bash -s`):

1. `docker build -t sparkbox-qemu:smoke .` — `sparkbox-cks:dev` + `qemu-system-arm`.
2. `smoke.sh` — cold boot one guest, SSH in.
3. `probe.sh` — boot, balloon stats, `migrate` to a file, restore with `-incoming`.
4. `ab.sh` — boot-to-SSH timing, with and without the balloon device.

Each expects `--privileged`, `/dev/kvm`, `/dev/net/tun`, the devpod `images/`
and `assets/` mounted read-only, and **scratch on the reflink XFS** — on
overlayfs the 25 GiB template copy dominates every measurement.
