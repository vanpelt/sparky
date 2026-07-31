# CKS reflink guest-disk persistence plan

Written 2026-07-30. This plan extends the
[CKS proof of concept](deploy-cks.md) from an intentionally ephemeral
single-Pod deployment into a recoverable Sparkbox installation while preserving
fast reflink cloning for Firecracker guest disks.

## Implementation status

The first two deliberately small milestones are complete:

- `--state-dir` now holds control state and `--vm-state-dir` independently
  selects the hot per-VM directory, with the old one-directory layout retained
  as the default.
- Firecracker template and snapshot-staging clones require
  `cp --reflink=always`; an incompatible filesystem fails instead of silently
  copying a 25 GiB image.
- The CKS Deployment is pinned to one exact Node and uses the named
  `/mnt/local/sparkbox-poc` host path, split into `control` and `hot`
  subdirectories.
- Pod replacement on that Node has been exercised without changing the gateway,
  OIDC, or upstream SSH keys and without losing the existing sandbox.
- The Kubernetes bootstrap patches `claude`, `codex`, `pi`, `hivemind`, and the
  workload-identity services into the base template before serving.
- The six long-lived gateway/OIDC/node-control identity files now come from the
  read-only `sparkbox-identity` Secret with `--require-keys`; their hashes were
  verified unchanged across Pod replacement.
- A 100 GiB `shared-vast` claim is mounted only for checkpoint objects. SQLite,
  control records, and live Firecracker disks remain on the Node-local XFS
  tier.
- An owner-scoped manual command creates a latest immutable checkpoint and can
  restore it without consuming the durable object. Per-sandbox disk operations
  are serialized, a failed restore preserves the old hot disk, and a missing
  hot disk fails closed instead of silently cloning the base image.
- The live POC checkpointed `radiant-wren` to a 997,535,996-byte VAST object in
  45 seconds and restored it in 18 seconds. A changed sentinel rolled back to
  its checkpointed value, the object remained present, and another Pod
  replacement preserved the identity, checkpoint, guest, and agent tools.
  A fresh disposable sandbox also inherited Claude, Codex, Pi, HiveMind, its
  workload token, and an active HiveMind service from the patched template.
- The CKS runtime image now contains the validated Sparkbox binary, Kubernetes
  entrypoint, agent-template refresher, identity installer, and Python runtime.
  GitHub Actions publishes linux/amd64 images, and the live Deployment is pinned
  to the resulting OCI index digest rather than a mutable tag or hostPath binary
  overlay.

The manual checkpoint deliberately keeps the guest paused through filesystem
checking, zeroing, compression, and upload. It is simpler than the asynchronous
reflink-staging design below, at the cost of about 45 seconds of observed
downtime for the current guest. Checkpoint scheduling, manifests, digests,
retention, discovery after control-state loss, and automatic Node-loss recovery
remain future milestones. This still avoids putting SQLite WAL files on
unvalidated NFS.

## Decision

Use two storage tiers unless CKS can provide a persistent block volume that we
can format as reflink-capable XFS:

```text
Base image
    |
    `-- reflink clone --> local XFS hot disk --> running Firecracker VM
                                  |
                           pause + reflink
                                  |
                                  v
                         checkpoint staging clone
                                  |
                         compress/upload async
                                  v
                       durable VAST checkpoints
```

The current CKS Node exposes roughly 7 TiB of XFS-backed ephemeral storage and
passes a reflink probe. It is an excellent hot tier, but it is not durable:
CoreWeave documents local storage as lost when a Node reboots or is replaced.
The only persistent StorageClass exposed by the POC cluster is `shared-vast`,
which is a distributed NFSv3 filesystem rather than a tenant-controlled local
filesystem. It should hold immutable checkpoint objects and control data, not
live Firecracker disk images that depend on Linux `FICLONE`.

References:

- [CoreWeave local storage](https://docs.coreweave.com/products/storage/local-storage)
- [CoreWeave distributed file storage](https://docs.coreweave.com/products/storage/distributed-file-storage/about)
- [Creating distributed file storage volumes](https://docs.coreweave.com/products/storage/distributed-file-storage/create-volumes)

This design makes the failure boundary explicit:

- Pod replacement on the same live Node can reuse the local hot disk.
- Graceful shutdown checkpoints all committed writes.
- Unexpected Node loss recovers the most recent durable checkpoint and can lose
  writes made since it.
- Zero-RPO recovery from unexpected Node loss requires durable block storage or
  a substantially different guest storage architecture.

## POC objectives

Use these as the initial service targets:

- A planned Pod restart loses no guest-disk writes.
- Unexpected Node loss loses at most five minutes of guest-disk writes.
- A normally sized guest can be recovered in under five minutes.
- Creating a sandbox does not copy the complete 25 GiB base image.
- A reflink clone normally completes in approximately one second or less.
- SSH, OIDC, Node CA, and gateway identities remain stable across every Pod
  replacement.

The initial implementation remains a single Sparkbox replica on one CPU-only
bare-metal Node. Multi-Node placement and replicated control-plane state are
out of scope, but the on-disk model must not prevent them.

## M0: resolve the persistent-block storage fork

Timebox a question to the CKS storage team:

> Can CKS staging in US-EAST-06A expose a persistent RWO block volume whose
> filesystem the tenant controls, formatted as XFS with `reflink=1`, and
> reattach that volume after Node replacement?

If such a volume is available, validate all of the following before selecting
it:

1. Create and attach the volume through a documented StorageClass.
2. Format or provision it as XFS with reflink support.
3. Verify `cp --reflink=always` and `FICLONE`.
4. Move the workload to another Node and reattach the volume.
5. Confirm the durability, backup, and failure guarantees.

If all checks pass, use a StatefulSet with the RWO volume for hot guest disks.
Keep versioned checkpoints anyway for rollback and disaster recovery. If the
volume is unavailable, proceed with the local-hot-plus-checkpoint design below.

## M1: separate identity, control state, and guest storage

Sparkbox currently places keys, SQLite databases, sandbox metadata,
certificates, and Firecracker disks beneath one state directory. Introduce
separate configuration and mounts:

| Purpose | Proposed path | Storage |
| --- | --- | --- |
| Private identity keys | `/run/sparkbox/keys` | read-only Kubernetes Secret |
| Control metadata | `/var/lib/sparkbox/control` | durable single-writer storage |
| Live guest disks | `/mnt/local/sparkbox/hot` | Node-local XFS |
| Checkpoint staging | `/mnt/local/sparkbox/checkpoints` | Node-local XFS |
| Durable checkpoints | `/mnt/sparkbox-durable/checkpoints` | `shared-vast` PVC |
| Base images | existing image directory | downloadable local cache |

Required changes:

- Mount `sparkbox-identity` as a Kubernetes Secret and start with
  `--key-dir=/run/sparkbox/keys --require-keys`.
- Add explicit control-state and VM-state directory flags. Do not infer one
  from the other.
- Treat the local presence of a guest disk as cache state, not proof that the
  sandbox is durable.
- Record the latest committed checkpoint generation separately from the local
  disk path.
- Keep the base image downloadable and reproducible; it does not require
  durable storage.

Before putting the existing SQLite databases on VAST, test locking, journal
behavior, crash recovery, and latency through the CKS NFS mount. The POC may
use a single-writer non-WAL journal mode if it passes. Otherwise, move control
records to PostgreSQL rather than relying on unsafe SQLite behavior over NFS.

## M2: establish the local XFS hot tier

Replace the current `emptyDir` guest-state mount with either a statically
provisioned local PersistentVolume or a deliberately named `hostPath` beneath
`/mnt/local`. Prefer a local PersistentVolume because it makes Node affinity
and retention intent visible to the scheduler.

The volume must:

- Be pinned to the selected CKS Node.
- Use a Sparkbox-specific path that no other workload shares.
- Use a `Retain` policy.
- Be mounted by only one Sparkbox Pod.
- Have capacity monitoring and an explicit cleanup procedure.

Change the Firecracker clone operation from `cp --reflink=auto` to
`cp --reflink=always`. Starting Sparkbox on an incompatible filesystem should
fail immediately instead of silently copying an entire disk.

At startup, reconcile durable metadata with the hot directory and assign every
sandbox one of these states:

- `hot`: the local disk exists and is usable.
- `restorable`: no local disk exists, but a committed checkpoint does.
- `restoring`: checkpoint recovery is in progress.
- `checkpointing`: a new durable generation is being produced.
- `unrecoverable`: neither a local disk nor a committed checkpoint exists.

A local PersistentVolume or `hostPath` can survive ordinary Pod replacement on
the same Node. It still must be treated as disposable because a CKS Node reboot
or replacement can erase it.

## M3: implement versioned checkpoints

Add a checkpoint operation distinct from the existing archive operation:

1. Pause the Firecracker VM.
2. Reflink-clone `rootfs.ext4` into a checkpoint staging directory.
3. Resume the VM immediately.
4. Run filesystem checking and zeroing against the staging clone.
5. Compress and upload the clone without blocking the live VM.
6. Verify the uploaded digest and size.
7. Atomically publish a manifest that identifies the new committed generation.
8. Retain the previous two or three committed generations.
9. Delete local staging files after the remote commit succeeds.

The pause covers only the local reflink operation. Expensive filesystem
checking, zeroing, compression, and network transfer occur against the frozen
clone after the VM has resumed.

Each immutable checkpoint manifest should contain:

- Sandbox ID, name, and owner.
- Checkpoint generation and creation time.
- Logical disk size and compressed object size.
- Base-image version.
- Compression and checkpoint format versions.
- Content digest.
- Whether the snapshot is crash-consistent or guest-assisted.

Upload a generation under a temporary name and publish its manifest or pointer
only after verification. Recovery must ignore temporary, partial, corrupt, and
unreferenced objects.

Restoring a checkpoint must use copy semantics. It must not delete the durable
generation, unlike the current archive restore flow. Archive can remain a
separate operation meaning "evict this guest from the hot tier."

## M4: automate checkpointing and recovery

Add the following lifecycle behavior:

- Checkpoint a running, writable guest every five minutes.
- Checkpoint after graceful shutdown and before archive.
- Attempt a final checkpoint during Pod `preStop` and graceful Node drain.
- Do not depend on `preStop` for correctness after unexpected Node loss.
- Restore the latest committed generation when its hot disk is absent.
- Support lazy restore on first connection if eager recovery makes gateway
  startup too slow.
- Garbage-collect an old durable generation only after a newer generation is
  completely committed and the retention floor is satisfied.

The first implementation promises crash-consistent disk checkpoints, equivalent
to recovery after sudden power loss. Application-consistent checkpoints require
guest cooperation such as `sync` or `fsfreeze` and can be added later.

Memory snapshots are not part of the durable recovery contract. A recovered
guest cold-boots from its checkpointed root filesystem.

## M5: expose control and observability

Add operator commands or equivalent API operations:

```text
checkpoint <sandbox>
checkpoint list <sandbox>
checkpoint restore <sandbox> <generation>
```

Expose at least:

- Last successful checkpoint time and generation.
- Checkpoint lag in seconds.
- Pause, compression, upload, and restore durations.
- Logical and compressed byte counts.
- Consecutive checkpoint failures.
- Hot-tier capacity and staged-checkpoint capacity.
- Current sandbox storage state.

Checkpoint lag beyond the RPO target and repeated checkpoint failures must be
visible without reading Pod logs.

## M6: validation and fault injection

The POC is complete only when it passes all of these:

1. **Reflink enforcement:** `cp --reflink=always` succeeds, sandbox creation
   does not transfer 25 GiB, and clone latency meets the target.
2. **Pod replacement:** write a sentinel inside a guest, replace the Pod on the
   same Node, and verify the sentinel without restoring from VAST.
3. **Graceful recovery:** force a final checkpoint, delete the hot disk, restore
   it, and verify all committed writes.
4. **Unexpected loss:** remove the hot tier without a final checkpoint and
   measure the actual RPO and RTO.
5. **Interrupted upload:** kill Sparkbox during upload and verify that recovery
   selects the previous committed generation.
6. **Corruption:** alter a remote object and verify that digest validation fails
   closed.
7. **Identity persistence:** replace the Pod repeatedly and verify that SSH,
   OIDC, Node CA, and gateway identities do not change.
8. **Control-state recovery:** restart during metadata updates and prove the
   sandbox/checkpoint relationship remains consistent.
9. **Capacity:** fill the hot tier to its warning and rejection thresholds and
   verify controlled admission and garbage collection.

Record clone latency, pause duration, checkpoint throughput, compression ratio,
restore throughput, and storage overhead for one and two simultaneous guests.

## M7: operationalize

Before calling the deployment durable:

- Document recovery after Pod loss, Node loss, namespace loss, and corrupt
  checkpoint data.
- Define checkpoint retention and deletion behavior.
- Add identity-key backup and rotation procedures.
- Make namespace deletion warn that durable PVC and Secret handling are
  separate decisions.
- Pin the runtime image by immutable digest.
- Document how to rebind or rebuild the local PersistentVolume after Node
  replacement.
- Decide whether VAST snapshots provide a useful second layer of protection for
  control data and checkpoint manifests.

## Recommended implementation order

1. Ask the CKS persistent-block question in parallel with the remaining work.
2. Persist the identity Secret and split the filesystem paths.
3. Establish the Node-local XFS hot volume and enforce reflinks.
4. Build manual checkpoint and restore with atomic, versioned generations.
5. Add reconciliation and automatic recovery.
6. Add scheduled checkpoints, metrics, and retention.
7. Run fault injection and record measured RPO/RTO.

The working default is a five-minute checkpoint interval, three retained
generations, VAST as the durable checkpoint target, and local XFS treated
strictly as a reconstructible hot cache. A future persistent XFS block volume
can replace that hot tier without invalidating the checkpoint format or
recovery machinery.
