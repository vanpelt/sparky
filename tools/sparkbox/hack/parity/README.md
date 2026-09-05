# The VMM parity harness

Three ways to run one suite. The suite itself is
[`internal/vmm/vmmtest`](../../internal/vmm/vmmtest); these are the ways to get
it onto a machine with `/dev/kvm`.

| | what it runs on | arch | when |
| --- | --- | --- | --- |
| `go test ./internal/vmm/mock/` | anything | any | every `go test ./...`, no gate |
| `run-on-mac.sh` | the Apple container machine `hack/dev` already uses | arm64 | local iteration |
| `run-on-cks.sh` | a throwaway Pod on the CKS node | x86_64 | before a release, and for the other arch |

Two drivers, one suite. `--pkg` is what selects one:

```sh
hack/parity/run-on-cks.sh --pkg ./internal/vmm/qemu --run TestQEMUParity
hack/parity/run-on-mac.sh --pkg ./internal/vmm/qemu --run TestQEMUParity \
  --base 127.0.0.1:5001/sparkbox-qemu:dev
```

Both scripts build a `go test -c` binary and ship it to the host; neither needs
a Go toolchain, a source tree, or a checkout on the far side.

Read [docs/vmm-parity-harness.md](../../docs/vmm-parity-harness.md) first — it
says what the suite asserts, why the gate is an environment variable rather than
a build tag, and what a run does *not* cover.

```sh
hack/parity/run-on-mac.sh                                   # everything
hack/parity/run-on-mac.sh --run 'TestFirecrackerParity/Balloon'
hack/parity/run-on-mac.sh --keep                            # leave the container up
hack/parity/run-on-cks.sh --namespace parity                # x86_64, throwaway Pod
```

Each `run-on-mac.sh` pushes a `sparkbox-parity:dev` image whose only new layer is
the ~14 MB test binary — everything else is shared with `sparkbox-cks:dev`, so
the image is cheap to keep and is left in place between runs. The registry does
accumulate one untagged manifest per push; `hack/dev/registry.sh gc
--delete-untagged` reclaims them.

## The toolchain image

`run-on-cks.sh` runs inside `ghcr.io/<owner>/sparkbox-parity`, built by
[`sparkbox-parity-image.yml`](../../../../.github/workflows/sparkbox-parity-image.yml)
from [`Dockerfile`](Dockerfile) — QEMU plus the filesystem tools the driver
shells out to, and nothing else. One native runner per architecture, so the
x86_64 image is not built under emulation on an arm64 laptop.

It used to be `ubuntu:24.04` plus an in-Pod `apt-get`, which made every run
depend on the cluster reaching an Ubuntu mirror. The test binary is still copied
in per run, so iterating on the suite costs a `go test -c` rather than a CI
round trip.

The script defaults the tag to your current branch, which is what CI pushes for
this checkout. Wait for the build before running, or pass `--image` to pin one.
**A new GHCR package is private by default** — the CKS node pulls anonymously
(the deployment has no `imagePullSecrets`), so the package has to be public or
the Pod sits in `ImagePullBackOff`.

## What it touches on the node

`run-on-cks.sh` mounts the node's `/var/lib/sparkbox` **read-only** and writes
nothing to it. Its scratch is a loopback XFS inside the Pod's emptyDir, because
the driver refuses to fall back to a full 25 GiB copy when reflink is
unavailable. It prints `kubectl -n sparkbox-poc get pods` at the end; check it.
