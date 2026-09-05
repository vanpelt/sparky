# The VMM parity harness

Three ways to run one suite. The suite itself is
[`internal/vmm/vmmtest`](../../internal/vmm/vmmtest); these are the ways to get
it onto a machine with `/dev/kvm`.

| | what it runs on | arch | when |
| --- | --- | --- | --- |
| `go test ./internal/vmm/mock/` | anything | any | every `go test ./...`, no gate |
| `run-on-mac.sh` | the Apple container machine `hack/dev` already uses | arm64 | local iteration |
| `run-on-cks.sh` | a throwaway Pod on the CKS node | x86_64 | before a release, and for the other arch |

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

`run-on-cks.sh` mounts the node's `/var/lib/sparkbox` **read-only** and writes
nothing to it. Its scratch is a loopback XFS inside the Pod's emptyDir, because
the driver refuses to fall back to a full 25 GiB copy when reflink is
unavailable. It prints `kubectl -n sparkbox-poc get pods` at the end; check it.
