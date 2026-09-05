# The toolchain the VMM parity suite needs on a KVM host, and nothing else.
#
# Deliberately NOT the sparkbox node image. A parity Pod should not be
# mistakable for part of the deployment when someone reads `kubectl get pods -o
# wide` on a live node, and it has no business carrying the node's binaries or
# entrypoint.
#
# Deliberately NOT built on the machine that runs the suite, either. The
# previous approach was `ubuntu:24.04` plus an `apt-get install` inside the Pod,
# which made every run depend on the cluster having egress to archive.ubuntu.com
# and on that mirror serving the same versions twice. Building it here also
# means the x86_64 image is built on an x86_64 runner rather than under
# emulation on an arm64 laptop.
#
# The test BINARY is not baked in on purpose: it is `kubectl cp`ed (or `docker
# cp`ed) in per run, so iterating on the suite costs a `go test -c` rather than
# a CI round trip. This image changes only when the toolchain does.
FROM ubuntu:24.04

ARG TARGETARCH

# qemu-system-* is arch-specific and the metapackage pulls in every target
# emulator, which is hundreds of megabytes of machines we will never boot. Pick
# the one that matches, and fail loudly on an arch we have not thought about
# rather than silently producing an image with no VMM in it.
#
# The rest is what the driver shells out to:
#   xfsprogs   mkfs.xfs -m reflink=1 for the loopback scratch filesystem; the
#              driver REFUSES to fall back to a full 25 GiB copy, so a scratch
#              dir without reflink fails Create rather than being slow.
#   e2fsprogs  e2fsck/resize2fs, behind DiskResizer and Archivable.compact
#   zerofree   Archivable.compact, before the zstd
#   zstd       Archivable.PackRootfs / UnpackRootfs
#   iproute2   tap creation and the /sys counters NetStatser reads
#   openssh-client  nothing in the driver, but every parity case SSHes in
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) qemu_pkg=qemu-system-x86 ;; \
      arm64) qemu_pkg=qemu-system-arm ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      "$qemu_pkg" qemu-utils \
      xfsprogs e2fsprogs zerofree zstd \
      iproute2 iputils-ping openssh-client ca-certificates; \
    rm -rf /var/lib/apt/lists/*

# Proof the arch-specific half of the RUN above did what it claims, at build
# time rather than on a KVM host an hour later.
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) qemu-system-x86_64 --version ;; \
      arm64) qemu-system-aarch64 --version ;; \
    esac; \
    mkfs.xfs -V; zstd --version; zerofree -h 2>&1 | head -1 || true

# The runner drives everything through `kubectl exec` / `docker exec`, so the
# Pod only has to stay up. sleep infinity is the whole job.
CMD ["sleep", "infinity"]
