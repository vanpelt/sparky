# Choosing a VMM: the argument, not the enthusiasm

Status: decision document, 2026-09-04. No code changed.

Companions: [cloud-hypervisor-feasibility.md](cloud-hypervisor-feasibility.md)
(what the port touches, plus the M0/M0b/M0c hardware measurements),
[cloud-hypervisor-port-design.md](cloud-hypervisor-port-design.md) (how),
[nested-virtualization-design.md](nested-virtualization-design.md) (risk register
and kill criteria), [cocoon-evaluation.md](cocoon-evaluation.md) (why we are not
adopting someone else's engine).

This document exists because the spike that produced those found something
awkward: **the reason we started was not the reason to finish.** We went looking
for nested virtualization, discovered it already works on the VMM we ship, and
had to ask honestly whether anything was left. This is that argument, written so
it can be defended by someone who did not run the experiments — including the
parts that argue against us.

## The decision is not "Firecracker vs Cloud Hypervisor"

On today's workload the two engines are close enough that neither wins on merit.
Boot time does not matter for beefy dev sandboxes, security is a wash (§3), and
nested virtualization turned out not to need a port at all (§4).

The decision is narrower:

> **Does the roadmap contain anything Firecracker structurally cannot do?**

Everything below is in service of that question, and the answer turns on one
input we do not control.

## The position we are defending

> We are moving to Cloud Hypervisor because GPU-capable sandboxes are on the
> roadmap and Firecracker structurally cannot get there, and because doing it
> proves our VMM abstraction carries a second backend — which is what makes
> QEMU-for-GPU a driver rather than a rewrite. We are explicitly **not** claiming
> a security, performance, or nested-virtualization win: on those the engines are
> a wash and we will say so. The two things Cloud Hypervisor gives us that we
> cannot get otherwise are a per-sandbox VMX gate and live CPU/memory resize.

Everything in that paragraph is falsifiable. The rest of this document is the
evidence for each clause, and §7 is the case for the opposite conclusion.

## 1. GPU and organisational alignment — the load-bearing argument

**Firecracker cannot do VFIO device passthrough. Not "less mature" — absent.**
There is an AWS-managed, community-built roadmap
([discussion #4845](https://github.com/firecracker-microvm/firecracker/discussions/4845)),
whose *first* milestone is virtio-pci rather than passthrough, and which states
it will not aim to snapshot GPU devices in its initial iteration.

Kata Containers supports VFIO device assignment on QEMU, Cloud Hypervisor and
Dragonball. Firecracker is not on that list. For production GPU work the mature
answer is QEMU: Cloud Hypervisor's VFIO path exists but is rougher — see
[kata#11687](https://github.com/kata-containers/kata-containers/issues/11687),
where a GPU is passed through but the guest gets no IOMMU groups.

This reframes what the port is for. It is not "move to Cloud Hypervisor". It is:

**Our `vmm.Driver` abstraction — five core methods plus ten optional capability
interfaces — has exactly one implementation, so it is unproven. A second backend
is what turns "QEMU for GPU sandboxes" from a rewrite into a driver.**

CoreWeave runs Kata already. If they run it on Cloud Hypervisor, we inherit
shared operational vocabulary, their kernel and VMM debugging experience, and
their security review. That is a real benefit and it is also the answer to "why
are you on Firecracker?" — a question worth being able to answer well.

> **Open question, and it is load-bearing: confirm which VMM CoreWeave actually
> runs under Kata, and whether GPU sandboxes are a committed roadmap item or an
> aspiration.** Neither is verifiable from public sources. Both change the
> conclusion.

## 2. Performance — the right instinct, the wrong conclusion

The intuition "Firecracker is a bit faster but it will not matter, because the
bottleneck is filesystem I/O" is correct about boot time and correct about the
bottleneck. But it argues *for* Cloud Hypervisor rather than for indifference:

| | Firecracker | Cloud Hypervisor |
|---|---|---|
| block I/O | virtio-blk, io_uring engine | multi-queue virtio-blk with io_uring **plus vhost-user-blk** (SPDK-style offload) |
| host dir sharing | none | virtio-fs |
| snapshot restore | userfaultfd (`Uffd` mem backend) | userfaultfd (`memory_restore_mode=ondemand`) |

If I/O is where our users feel pain, Cloud Hypervisor has headroom we do not
have. We would not use it on day one, and it should not be sold as a day-one
benefit.

**One trap worth naming:** the eye-catching "clones share page cache via an
mmap'd snapshot" mode is **not upstream**. It is `memory_restore_mode=CopyOnWrite`
in cocoonstack's Cloud Hypervisor fork. Stock v53.0 has `copy` and `ondemand`
only. Do not put it in a plan.

Restore latency is *not* a differentiator: both engines do userfaultfd lazy
paging, so the 3.0 GB memory file our M0c snapshot produced does not have to be
copied eagerly on either.

## 3. Security — a wash, and the real finding is about us

From the 2026 comparative study of AI code sandboxes
([arXiv 2606.08433](https://arxiv.org/abs/2606.08433)), 24-month window
(2024-05-20 → 2026-05-20):

| | Firecracker | Cloud Hypervisor |
|---|---|---|
| in-window CVEs | 2 | 2 |
| escape-class | CVE-2026-5747, virtio-pci OOB write, CVSS v4 **8.7** | CVE-2026-45782, virtio-block async-I/O UAF, CVSS v4 **8.9** |
| other | CVE-2026-1386, jailer symlink host-write, **6.0** | CVE-2026-27211, host file exfiltration, **9.1** |
| upstream patch cadence | 100% coordinated, P50/P95 = **0 days** | 100% coordinated, P50/P95 = **0 days** |
| seccomp posture | uniform mode-2 across all threads, 55 allowlisted syscalls | leader thread **mode-0 (unfiltered)**, mode-2 on 32 of 33 workers |
| in-tree continuous fuzzing | present, lower tier | cargo-fuzz harness, higher tier |
| size | ~83k LOC Rust | ~106k LOC Rust |
| host isolation wrapper | jailer (namespaces + cgroups + chroot) | seccomp + `--landlock` (node measured at ABI 7) |

Neither engine is clean: both shipped escape-class CVEs inside a four-month span,
which the paper notes contradicts the industry's prior assumption about microVM
robustness. Upstream response is identical and excellent on both sides.

**The study's dominant finding is not about either engine.** Downstream pin lag
spanned 0 days to 471+ days across the products measured, and it — not engine
design — was the main driver of operator-visible exposure. Our artifact pin lives
in three places and only moves on release. **That is the actual security work,
and it pays off identically on either VMM.** Do not let a VMM port stand in for
it.

Two Cloud Hypervisor specifics that must be designed for, not discovered:

- **It defaults to host CPU passthrough**, where Firecracker virtualises the CPU
  model (our M0 measured a Firecracker guest reporting the generic brand string
  "Intel(R) Xeon(R) Processor" against a host Xeon Platinum 8562Y+). So the
  masking CPU template that §7 of the feasibility doc treats as optional
  hardening becomes **mandatory** on Cloud Hypervisor. It is the same lever that
  gates VMX exposure, so this is one piece of work, not two.
- **CVE-2026-27211 is precisely our shape.** A guest overwrites sector 0 of its
  own *raw* disk with a crafted QCOW2 header naming a backing file; the host
  opens that file and serves its contents back as disk data. Deployments using
  only trusted read-only images were unaffected — our disks are guest-writable
  raw ext4, so we would have been squarely in scope. Fixed in 50.1, and note the
  aggravating factor: a guest-initiated reboot does not exit the Cloud Hypervisor
  process, so it was repeatably exploitable without host intervention. **Pinning
  `image_type=raw` so the format sniffer never runs is a day-one requirement.**

**Conclusion: security is neither a reason to move nor a reason to stay.** Any
version of this argument that leans on it is weaker than it sounds, and a
knowledgeable reviewer will say so.

## 4. Nested virtualization — demote it

We went looking for this and it turned out not to need a port. Measured on the
CKS node (feasibility §10–§12):

- The node already has `kvm_intel.nested=Y`, KVM advertises VMX, and **every
  sandbox already carries the VMX bit in its CPUID today.**
- A guest kernel with `CONFIG_KVM_INTEL` boots an inner microVM under the
  Firecracker we already ship. One kernel-config line.
- What breaks is **pausing** it: Firecracker returns HTTP 204, writes a
  normal-looking snapshot, and the restored sandbox's kernel hits
  `BUG at arch/x86/kvm/x86.c:511`. Silent data loss, and scale-to-zero pauses
  sandboxes automatically.
- Cloud Hypervisor v53.0 carries the inner VM through: both guests resumed
  mid-flight, tick counters continuing exactly where they stopped.
- **And `--cpus nested=off` genuinely masks VMX** — no `vmx`, no `/dev/kvm`, no
  inner VM.

The defensible half of this is **the gate, not the feature**. Today every
sandbox carries the VMX bit and we have no mechanism to remove it; a masking
template plus a real per-sandbox switch is a security capability. "Nested
virtualization is cool" is not an argument and should not be made.

## 5. The device model — what it concretely buys

Worth naming precisely, because "virtio and device plugins seem neat" is not a
justification:

| Capability | What it changes for us |
|---|---|
| **CPU + memory hotplug** | Turbo mode (2× CPU/RAM) currently requires a cold boot. It becomes live. |
| **virtio-mem** | Actually removes guest memory instead of pinning it inside a balloon — a better primitive for the idle reaper than what we drive today |
| **Hot-attach disk** | Our `DiskResizer` capability, without a restart |
| **VFIO** | The GPU path from §1 |
| **vhost-user-blk / virtio-fs** | The I/O headroom from §2 |

## 6. What cuts against the move

Stated plainly, because a decision document that only argues one way is not
useful.

**Cross-version snapshot stability.** Cloud Hypervisor documents that
snapshot/restore and live migration are **not guaranteed stable across
versions**, and live migration explicitly does not work across versions.
Firecracker has a documented snapshot-compatibility policy. Our fleet is mostly
paused — 14 of 15 sandboxes at the time of writing. If we ever want a paused
sandbox to survive a VMM upgrade, Cloud Hypervisor makes that *harder*. Today a
node roll already cold-boots everything, so this is a lost opportunity rather
than a regression — but it closes a door we might have wanted.

**Live migration is worth less than it looks.** It would fix destructive node
rolls, which is arguably worth more to this product than nested virtualization.
But it needs two nodes *and* the same Cloud Hypervisor version on both, and the
roll we actually care about is a version bump. We have one node. Bank it as a
future benefit at ≥2 nodes; do not spend it now.

**We give up the jailer** and a smaller default device surface. Our `vmhelper`
already does much of what the jailer does, but "much of" is doing work in that
sentence.

**The migration itself is disruptive.** Feasibility §9 M2: `replicas: 1` +
`Recreate` + one `sparkbox.dev/kvm` means the node Pod is terminated before the
new one starts. Every sandbox on the pinned node goes down and comes back on the
other VMM. Budget a window; rollback is a second full Recreate.

## 7. The counter-case, stated as strongly as we can

**If GPU-in-sandbox is not a committed roadmap item, stay on Firecracker.**

Tighter default hardening (uniform seccomp across every thread, a virtualised CPU
model, fewer exposed device nodes), a documented snapshot-compatibility policy, a
smaller codebase, the jailer, a driver already proven on our hardware across many
deployments — and nested virtualization available today for one kernel-config
line if we accept "do not pause a nested guest" and gate it behind a masking
template.

That is a genuine defence. It survives every argument in this document except
one: someone asking for a GPU.

## 8. Cost, and why the stakes are lower than they look

**The port is reversible and incremental.** Both drivers can coexist keyed per
sandbox, because the privileged helper builds the argv per launch. This is not a
one-way door.

**The real cost is not the driver.** Our Firecracker driver is ~2,150 lines, and
**its 1,300 lines of tests never boot a guest** — every lifecycle end-to-end runs
against the mock. The parity harness is the actual deliverable, and we need it
whether or not we ever add a second VMM. The honest framing:

> We are buying the boot-level integration suite we are missing, and getting a
> second VMM along the way.

## 9. What to do next

1. **Answer the load-bearing question.** Confirm with CoreWeave which VMM they
   run under Kata, and whether GPU sandboxes are committed or aspirational. If
   aspirational, this is a "not yet".
2. **Do the pin-lag work regardless.** It is the largest real security lever in
   §3 and it is independent of this decision.
3. **Build the parity harness regardless.** Same reasoning.
4. **If the answer to (1) is yes:** proceed with M1 as scoped in
   [cloud-hypervisor-port-design.md](cloud-hypervisor-port-design.md), reading
   cocoon's Cloud Hypervisor backend first (see
   [cocoon-evaluation.md](cocoon-evaluation.md) for the specific list), and treat
   `image_type=raw` and the masking CPU template as day-one requirements rather
   than hardening to schedule later.
5. **Independent of everything above:** do not ship a `CONFIG_KVM` guest kernel
   until the per-sandbox gate exists. It would turn the idle reaper into a way to
   panic a user's sandbox with no error anywhere.
