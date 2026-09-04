# Per-sandbox nested virtualization: plumbing, risk and gates

Status: design, unimplemented. Written 2026-09-04 alongside the
[feasibility spike](cloud-hypervisor-feasibility.md) and the
[port design](cloud-hypervisor-port-design.md).

The spike establishes that nested virtualization needs Cloud Hypervisor and that
it puts guest root on KVM's shadow MMU — the file that produced three 2026
guest-to-host escapes. This document is the other half of that sentence: how one
boolean travels from the `new@` door to the VMM's argv without becoming a
cluster-wide property, what admission must charge for it, and what measured
result would make us stop.

## 1. Per-sandbox nested: admission and plumbing

§7's second policy bullet says nested is "carried like any other resource: a
`vmm.Config` field, a helper protocol field (version 2), an admission check in
`host.Manager`, and a user-facing knob". That list is right and it is not the
whole cost. Nested is the first per-sandbox attribute that has to travel from
the `new@` door all the way to the VMM's argv, so §2's "Everything above the
driver is unchanged by construction" does not hold for it: eight packages carry
one bool. This section is what each of them does with it.

### 1.1 Where the flag lives at each layer

| Layer | The change | Why here |
| --- | --- | --- |
| `new@` door | `--nested`, parsed by a new `splitNestedFlag` in `internal/sshgw/tags.go` beside `splitNodeFlag`/`splitEnvFlag`/`splitRefFlag`, wired into `parseCreateArgs` — **including at the `ctl@` doors that do not accept it**. | The door reads every bare word as a tag, so an unrecognised `--nested` becomes the tag `--nested`, fails `tagRe` inside `NormalizeTags`, and vanishes: the user gets a non-nested sandbox and nothing anywhere mentions it. That is the exact failure `parseCreateArgs`'s own doc comment exists to prevent, and the reason it says a flag meaning something at one door and nothing at another has to say so. |
| REST | `nested` on `createRequest` (`internal/restapi/sandboxes.go`), copied into `ctlops.CreateArgs` beside `VCPUs`/`MemMB`, plus a schema line in `internal/restapi/openapi.json` — which is embedded and parsed at package init, so a missing line is a startup panic, not a docs gap. | The REST body is the door with its ambiguity removed; it has named fields and needs no grammar. |
| `ctlops` | `CreateArgs.Nested bool`; `SandboxInfo.Nested bool \`json:"nested,omitempty"\`` projected in `info()` (`ops.go:792`). | §7 asks that it show in `ctl info`. `omitempty` keeps every existing payload byte-identical. |
| Control record | `host.Sandbox.Nested bool \`json:"nested,omitempty"\``. | Both places that build a `vmm.Config` build it from the record — `Manager.Create` and `resumeOrRecreate` — so a cold boot carries the flag for free. |
| Fleet request | `fleet.Request.Nested`; `nodev1.CreateRequest.nested = 7`; `nodev1.Sandbox.nested = 24` beside `turbo = 19` and `base_disk_mb = 23`. | The record crosses the link. Without the `Sandbox` field a gateway's cached copy of a remote nested sandbox reads as non-nested in `ctl info`, in the owner rollup and in both consoles — the same failure `base_disk_mb`'s comment describes for reflink accounting. |
| `vmm.Config` | `Nested bool`, default false. | Cold boot only: `Driver.Resume(ctx, name)` takes a name and nothing else, and under Cloud Hypervisor that is right — the resume command line is `--restore source_url=…` with none of the VM-config flags, and the whole `VmConfig` comes back from the snapshot's `config.json`. See the re-gate in §10.2. |
| Helper | `vmhelper.Request.Nested`; `ProtocolVersion = 2`; `--nested` on `LaunchCommand`; `validate()` (`server_linux.go:261`) refuses `Nested` together with `Resume`. | The helper builds the argv, so it is the last place the flag can be refused and the only place it becomes real. It refuses rather than drops it, because under Cloud Hypervisor "drop the flag" is silently a different VM and, with `--cpus nested` defaulting to **on**, "ignore the field" is silently the *wrong* different VM. |

**Why a create flag, and not the three alternatives.** The two per-sandbox knobs
that already exist argue for it and against them.

- `Pinned` is a policy bit that flips any time with no boot: `ctl pin` →
  `Ops.SetPinned` → `Manager.SetPinned`, which sets a field and saves. Nested
  cannot be that, because it is decided at `--cpus nested=` on the command line
  at exec.
- `Turbo` *is* a boot property and is set after create — through `SetTurbo`,
  which pauses, `DropSnapshots`, and cold-boots — because it is borrowed for
  exactly one run. Nested is not borrowed; it is a property of the sandbox for
  its whole life and an admission input like `VCPUs`/`MemMB`, which are create
  arguments.
- **Not a tag.** `ctl tags <box> a b c` replaces the whole set
  (`sshgw/control.go:861`), tags are user-writable, `NormalizeTags` does not
  validate the charset, and every other reader of `sandbox_tags` either adds
  (secrets, repos) or replaces (templates). A tag that granted VMX would make a
  decoration command a privilege escalation — and because the tag set can change
  on a running box while `nested` cannot, the tag would lie until the next cold
  boot.
- **Not an environment column.** An environment's name *is* its tag, and the
  launch door hands an attachment's tags or `Env` to `ctlops.Create` verbatim
  (`internal/launch/create.go:196-206`). Nested in an environment row means a
  visitor who clicks a repository launch link gets a VMX-capable VM nobody asked
  for. Environments become the right home if nested ever turns into a plan
  entitlement; not while it is a security grant.
- **No `ctl nested <box> on|off` in M1.** Flipping it needs `SetTurbo`'s exact
  pause → `DropSnapshots` → cold-boot dance (§10.2), and there is no reason to
  ship the flip before M2 has measured the thing being flipped.

**The persisted shape is a compatibility surface, and a downgrade loses the
bit.** `Manager.load` is `json.Unmarshal` into `map[string]*Sandbox` and
`Manager.save` re-marshals the struct (`manager.go:3500-3540`), so unknown
fields are dropped, not preserved: an older node binary that reads the state
file and saves once erases `nested` permanently. Under Cloud Hypervisor that is
worse than a lost bit. `nested` lives in the snapshot's `config.json` and the
restore path takes the whole `VmConfig` from there, so the record now says "not
nested" while the VM that resumes still is — a nested sandbox nobody accounts
for and no admission check will see again. The rollback rule follows, and it
belongs in §9 M2's "roll back is the env var" line: **a downgrade drops
snapshots for every sandbox whose record says nested, before the old binary
starts.** That is nearly free on CKS, where `deployment.yaml` is `replicas: 1`
with `strategy: Recreate` and every sandbox on the pinned Node is cold-booted by
the roll anyway.

**The fleet signature is the one genuinely awkward edit.** `Fleet.Create` and
`CreateOn` are positional — `(ctx, [node,] name, owner, image, vcpus, memMB)` —
and that signature is `ctlops.Sandboxes`', which `*host.Manager` satisfies
structurally. `CreateOn`'s own comment says why `--node` became a second method
rather than a seventh parameter: a single-box gateway "must not be made to grow
a parameter it can only answer one way". A seventh positional argument re-runs
that objection, and a second method makes no sense here because nested is a
property of the VM rather than a placement preference. Replace the positional
form with one `host.CreateSpec{Name, Owner, Image, VCPUs, MemMB, Nested}`
threaded through `ctlops.Sandboxes`, `fleet.ControlPlane.Create`, `localNode` /
`remoteNode` / `GRPCControl.BeginCreate` and `nodev1.CreateRequest`. It is the
same refactor `cmd/sparkbox/driver_linux.go`'s 16-parameter positional
constructor already needs, and doing both once is cheaper than doing either
twice.

### 1.2 Node capability reporting and placement

Advertise nested the way `Sluice` is advertised — a stated fact about the
machine — not as a `Capabilities` token, which the frame documents as
"independently negotiated transport features". Two carriers, because `Facts` has
two producers:

- **`nodelink.Hello.Nested bool \`json:"nested,omitempty"\``**, filled by the
  hello builder in `cmd/sparkbox/node.go:809-827` beside `Sluice`, and by
  `fleet.localNode.Facts()` (`local.go:78-93`) — which a single-box gateway is
  served by, and which would otherwise refuse its own nested creates.
- **`nodev1.Capacity.nested_virtualization = 24`**, because `factsFromProto`
  (`grpc_remote.go:855-877`) rebuilds `Facts` from `Capacity` on the gRPC path,
  and because `Capacity` rides the heartbeat: a Node rebooted onto a patched
  kernel re-advertises without a gateway restart, where a hello-only field would
  need a reconnect.

`Hello.Sluice`'s own comment is the precedent and its reasoning transfers
unchanged: an older node omits the field and reads as false, which is the honest
answer for a build that had no way to honour it.

Where the node gets the answer is a driver capability, asserted once in
`host.NewManager` beside `vmm.Ballooner` and `vmm.Rebooter`:

```go
// NestedReporter is an optional Driver capability: whether this driver can boot
// a guest with hardware virtualization exposed to it, and why not when it
// cannot. Asserted once at startup like every other capability here, so a
// driver that does not implement it — mock, and firecracker, which has no
// supported switch for VMX/SVM — answers "no" structurally rather than by
// configuration.
type NestedReporter interface {
	NestedAvailable() (ok bool, reason string)
}
```

`Manager.NestedEnabled() bool` mirrors `ArchivingEnabled()` and `Snapshotter()`
and is what the hello and the `Capacity` report read.

Placement is then one row in `Candidate.Fits`, in the shape the arch and image
rows already have:

```go
if req.Nested && !c.Facts.Nested {
	return placementRefused(op, c.Name, ctlops.KindConflict, http.StatusConflict, fmt.Sprintf(
		"node %q cannot run nested virtualization", c.Name),
		"Leave --nested off, or pick a machine that reports it (`ctl node ls`).")
}
```

**This row deliberately breaks `Fits`' stated rule** that every check is skipped
when the input it needs is missing, "because unknown must never read as
refused". That rule is right for arch and images: a node that reported no image
list can still boot the sandbox correctly. It is wrong here in both directions.
A node build old enough not to report the field cannot honour a nested launch at
all — and, since Cloud Hypervisor's `nested` defaults to on, a node build old
enough not to *understand* the field would boot the sandbox nested with nobody
having asked. Absence must read as refused, and the comment saying so belongs
next to the rule it breaks.

Three refusals, all inside the existing taxonomy:

| Case | Error |
| --- | --- |
| The caller named a node that does not report nested | `placementRefused(…, ctlops.KindConflict, 409, "node %q cannot run nested virtualization", …)` — `Code: "node_cannot_place"`, the same code the arch and image rows use, because it is the same kind of refusal. |
| No node named, the fleet has machines, none reports nested | `&ctlops.Error{Kind: ctlops.KindCapacity, Op: "create", Code: "no_nested_node", Msg: "no machine in this fleet can run nested virtualization right now", Hint: "Try again later, or create without --nested."}` — `RemoteOnlyPlacer`'s `no_vm_node_online` shape: HTTP 503, exit 1, retryable. |
| This deployment has no nested-capable driver at all (Firecracker everywhere, or nested compiled out) | `KindDisabled`, HTTP 501: `ctlops.Disabled("create", "nested virtualization is not enabled on this host.")`, or the same struct written out when a client needs a code more specific than `Disabled`'s derived `create_disabled`. |

The distinction is what an edge does with it: 503 says try later, 501 says stop
asking and hide the checkbox.

**Placement is not the gate.** `defaultPlacer` returns the local candidate
without ever calling `Fits` on it — deliberately, per `place.go`'s "a Placer
decides WHICH machine, and the machine itself decides WHETHER". So on a
single-box deployment a nested request reaches `host.Manager` ungated, and the
manager must refuse it itself: in `Create`, before `admit`, a
`&host.DisabledError{Code: "nested_unavailable", Msg: …}` when
`!m.NestedEnabled()`. `DisabledError` already classifies as `KindDisabled`
through `ctlops.AsError`, so the sentence reaches `ctl@` and REST with no new
taxonomy and no new Kind — which matters, because a new `Kind` also means
editing the `Error.kind` enum in the embedded `openapi.json`.

**The gate has to be re-evaluated on resume, and the answer is a cold boot, not
a refusal.** `--restore` accepts only `source_url`, `prefault`,
`memory_restore_mode`, `resume`, `zone_updates` and `net_fds`; everything else,
`--cpus nested=` included, comes back from `config.json`. So nested is baked
into the snapshot. `ensureReady` re-checks `b.Nested` against
`m.NestedEnabled()` before `resumeOrRecreate` and, on a mismatch, drops the
snapshot and cold-boots — §8.1's fallback, in the shape `Reboot` already has.
The same rule holds in the other direction and is the sharper one: **any change
to `b.Nested` must drop the memory snapshot**, for precisely the reason
`endTurbo` (`manager.go:2040-2060`) drops it today — "a snapshot of a doubled
machine resumed under a record that says otherwise gives a VM twice the size the
control plane is accounting for, invisibly." Substitute "nested" for "doubled"
and the sentence needs no other edit.

### 1.3 The node-side preflight

`hack/probe-nested-virt.sh` is the operator's instrument and M0's; the helper
asks the same questions from the process that will actually exec the VMM, once,
in `vmhelper.RunServer` before it listens.

| Check | Source | On failure |
| --- | --- | --- |
| `vmx`/`svm` **and** `ept`/`npt` | `/proc/cpuinfo` | not available — an L2 on shadow paging is a demo, not a product |
| `/dev/kvm` present | already hard-failed by `deploy/kubernetes/vmm-helper-entrypoint.sh` | — |
| `/sys/module/kvm_{intel,amd}/parameters/nested` | read the value and accept `1\|Y\|y`: Intel's parameter is a `bool` and prints `Y`, AMD's is an `int` and prints `1` | not available |
| `uname -r` against the three shadow-MMU CVE floors of §3 | the kernel.org records | not available |
| `runtime.GOARCH == "amd64"` | build | not available — nested is x86-only in Cloud Hypervisor |

The module-parameter row is the one a preflight written from §3's prose gets
wrong: comparing the value to `1` false-negatives every Intel Node. The script
already handles both spellings and the Go check must match it.

**Advertise false *and* fail closed** — they answer different failures.
Advertising false keeps a nested create off this Node in the first place, and it
must not be a hard startup failure: the overwhelmingly common Node is one that
is perfectly good at everything except nested, and refusing to serve any sandbox
at all would be a far larger outage than the feature is worth. Failing closed at
the launch is what makes a stale advert harmless — the helper re-reads its
verdict when a launch arrives with `Nested: true` and refuses the request rather
than quietly dropping the flag.

The verdict reaches the controller on the handshake that already exists:
`firecracker.New` pings the helper at driver construction and treats a failure
as fatal (`fc.go:221`). `vmhelper.Response` gains `Nested bool` and
`NestedReason string`; the driver stores them and answers `NestedAvailable()`
from them. No new op, no new round trip, and the reason string is what `doctor`
prints. Caching at startup is sound rather than lazy: both module parameters are
mode `0444` and AMD's is `__ro_after_init`, so neither can change without a
module reload or a reboot, and a reboot restarts the Pod.

Two exclusions worth stating. The preflight must **not** fold in Landlock ABI
v3, `/dev/userfaultfd` or `CONFIG_SCHED_CORE`: those decide how the VMM is
confined and how a resume is performed, not whether a guest may see VMX, and one
combined verdict would make a Node that cannot do `ondemand` restore also unable
to run nested. And the script is not in the CKS image —
`deploy/kubernetes/Containerfile` copies nothing from `hack/` — so the deployed
answer is always the helper's. Keep the CVE floors in one table that both read,
or they will drift.

### 1.4 Admission and accounting

Four decisions.

**A nested sandbox is charged its full ceiling, not `MemReserveMB`.**
`effectiveMemFor` (`manager.go:1183`) returns the working-set floor whenever
`0 < reserveMB < MemMB`, and the entire density model rests on the reaper
ballooning an idle guest down to that floor. It cannot: virtio-balloon inflates
by allocating pages *in the L1* (`fill_balloon()` → `balloon_page_alloc()` per
page, giving up with "Out of puff!"), so it reclaims only what the L1 kernel
considers free, and an L2's resident RAM is the inner VMM's anonymous memory —
neither MemFree nor MemAvailable. Charging `MemReserveMB` for RAM that can never
be handed back is an unfunded liability against both the owner pool and
`memAdmitPct`. One clause:

```go
if b.Nested {
	return b.MemMB // the balloon cannot reclaim an L2's pages — see §8.2
}
```

`effectiveMemFor` takes `reserveMB` as a parameter precisely so a gateway
folding another machine's records charges them the way that machine does, so
this single edit is correct at once in node admission, in `RollUpOwner`, and in
`fleet.Candidate.Fits`' RAM check. On a 12 GiB default sandbox with
`MemReserveMB` at 1 GiB, a nested box costs 12× a normal one — the honest price,
and the main thing that stops nested from quietly eating the density target.

**A memory floor, stated as policy rather than derived as physics.** An L1 that
will host an L2 needs the inner guest's RAM plus its own working set inside one
ceiling. The 12288 MB default is already generous for that; what needs refusing
is the 1–2 GiB sandbox where the inner VM cannot fit and the user meets it as an
OOM twenty minutes later. Refuse `--nested` below a stated floor in `Ops.Create`
with `Invalid("create", "nested_needs_memory", …)` — exit 2, HTTP 400 — rather
than in the manager, because it is a malformed request and not a capacity
answer. Pick the number from M2's measurements; until then it belongs in this
document, not in code.

**Out of the balloon paths — but not via `Pinned`.** `Pinned` short-circuits the
whole reaper loop (`manager.go:3304`), so reusing it would also disable idle
pause and make every nested sandbox permanently resident. Two narrower guards:

- `reclaimMemory`'s candidate loop (`manager.go:3210`) already skips
  `b.Pinned || b.Ballooned`; add `|| b.Nested`. A box that cannot give memory
  back must not be picked to relieve pressure — otherwise the controller spends
  a `SetBalloonTarget` round trip, marks `Ballooned = true` on the API accept
  (`manager.go:3463`), and then skips it forever while believing it reclaimed.
- `reapOnce`'s balloon-down arm (`manager.go:3319`) gets the same guard. Its
  pause arm is left alone: an L1 running a live inner VM burns host CPU
  continuously, so `refreshVitals` keeps `LastActive` fresh and the pause arm
  will rarely fire — and when it does, the user has genuinely walked away.

**Pause is allowed, but until §8.1 is answered a nested pause drops the
snapshot.** `Manager.pause` gains a `DropSnapshots` for a nested sandbox beside
the `endTurbo` call it already makes for the identical reason, so the next start
is a cold boot. That loses the L2 and keeps the record honest. The alternative —
resuming a snapshot whose nested vCPU state we have never seen survive our
pause → kill → restore sequence — loses the L2 *and* leaves a guest that
believes it is in VMX operation while KVM does not. Deleting this line is M2's
acceptance criterion, not M1's.

What nested does **not** change: pooled disk (`pooledDiskMB`, `BaseDiskMB`) — an
L2's disk is a file inside the L1's rootfs and is already counted; the per-owner
running cap; egress metering; the metadata identity check.

### 1.5 The threat-model delta, and what containment is worth

With `nested=off` and a guest kernel carrying no `CONFIG_KVM`, a sandbox's guest
root reaches KVM only through the ordinary vCPU exit paths every sandbox already
uses. With `nested=on` and `CONFIG_KVM` in the guest, guest root additionally
reaches L0's **shadow MMU** — the legacy `arch/x86/kvm/mmu/mmu.c` path KVM must
use to shadow the nested EPT/NPT an L1 builds for an L2, instead of the TDP MMU
that handles ordinary guests — plus `vmx/nested.c` / `svm/nested.c`,
`KVM_GET/SET_NESTED_STATE`, and the emulated `IA32_VMX_*` MSRs. That one file
produced three public 2026 guest-to-host escapes (Januscape CVE-2026-53359;
Zapscape CVE-2026-64561, with a full public escape chain; and the `role.invalid`
follow-on CVE-2026-80726 at CVSS 9.3, PR:N), none of which needs any host
credential. It is also the only boundary this port moves that nothing Sparkbox
owns can contain: the chroot, the slot uid, seccomp, Landlock and the tap
perimeter all sit *above* KVM, and an L0 escape lands as root on a shared
bare-metal CoreWeave Node beside every other tenant's sandbox.

Containment, in the order it is worth paying for:

1. **The node gate (§10.2–§10.3). Non-negotiable and nearly free.** It is the
   only control that keeps a nested sandbox off an unpatched kernel, and it is
   one bool through two carriers.
2. **A per-owner allowlist. Worth it, and cheap.** Nested is a security grant,
   not a resource, and the accounts store already carries an operator bit
   (`Caller.Operator`, `Whoami.Operator`). One column plus one refusal in
   `Ops.Create` — `Denied("create", "nested_not_permitted", …)`, which is
   already the taxonomy for "authenticated, not permitted" — is a day's work and
   it bounds the population that can reach the shadow MMU to people an operator
   named, rather than everyone holding an invite.
3. **A dedicated node pool. Correct, and expensive — cost it honestly.** It is
   not one `nodeSelector`. `deployment.yaml` is `replicas: 1`, `strategy:
   Recreate`, with a `nodeSelector` pinned to `kubernetes.io/hostname` and a
   Node-local `hostPath` hot tier; `deploy.sh` refuses a pool with more than one
   eligible Node unless `--node` is passed; and `internal/deviceplugin`
   advertises one `sparkbox.dev/kvm` per Node for the same reason. A nested pool
   is therefore a second full `sparkbox-node` deployment — its own Node, hot
   tier, node identity and fleet enrolment — plus a `Placer` that routes nested
   requests to it. The routing machinery is already the `fleet.Request.Nested` +
   `Fits` pair above, so the cost is operational rather than architectural.
   Defer it to M3 and decide it on M2's numbers and on how many owners item 2
   admits.
4. **Not worth it: a separate VMM binary or a separate helper for nested
   sandboxes.** The helper builds the command line per launch, so one binary
   serving both is the design; a second doubles the pin, the preflight and the
   advisory watch for no boundary KVM does not already cross.

### 1.6 What this adds to M1

- `vmm.Config.Nested` + `vmm.NestedReporter`, asserted in `host.NewManager`
  beside the other capabilities; the Firecracker and mock drivers do not
  implement it, which is the honest answer.
- `host.Sandbox.Nested`, `host.CreateSpec` replacing the positional create
  across `ctlops.Sandboxes` / `fleet` / `nodev1.CreateRequest`, and
  `nodev1.Sandbox.nested = 24`.
- `nodelink.Hello.Nested` + `nodev1.Capacity.nested_virtualization = 24` +
  `factsFromProto` + `localNode.Facts()`; the `Fits` row and its three typed
  refusals.
- Helper protocol v2 with `Nested`, `--nested` on `LaunchCommand`, the startup
  preflight, and `Response.Nested`/`NestedReason` on `OpPing`.
- `--nested` in `parseCreateArgs` and on the REST create body, `openapi.json`,
  and `SandboxInfo.Nested`; a compile-time test that `parseCreateArgs` absorbs
  no unknown `--` flag into the tag list.
- Accounting: the `effectiveMemFor` clause, the two reaper guards, the
  drop-snapshot-on-nested-pause line, and the resume re-gate — each with a test,
  because none of them is observable from outside until it is wrong.

### Open questions from this section

- What is the memory floor below which `--nested` should be refused? It needs M2's measurement of an L1 hosting a realistic L2 (inner VMM RSS plus the guest's own working set) before a number can be written into `Ops.Create`.
- Does CoreWeave's CKS allow a second `sparkbox-node` deployment on a second bare-metal Node in the same (or a separate) CPU pool, with its own `hostPath` hot tier and its own `sparkbox.dev/kvm` device-plugin allocation? The dedicated-nested-pool containment in §10.5 is a second full deployment, not a node selector, and its feasibility is a CoreWeave answer.
- On the target Node (`g084f44`) and on every pool we might place nested sandboxes on: what does `/sys/module/kvm_{intel,amd}/parameters/nested` read, what is `uname -r`, and does that kernel carry all three shadow-MMU fixes (CVE-2026-53359, CVE-2026-64561, CVE-2026-80726)? The helper preflight's CVE floor table is only as good as CoreWeave's patch cadence, and the answer may differ per pool.
- Is the CPU pool Intel or AMD, and is it uniform? The module parameter renders differently (`Y` vs `1`), Zapscape's public escape chain is AMD-only while Intel additionally needs EPT PWL4 and PWL5 exposed to L1, and Cloud Hypervisor's `nested=off` was a silent no-op on AMD before v52.0 — so vendor is an input to both the preflight and the version pin.
- Does the balloon in fact fail gracefully when the reaper asks an L1 hosting a live L2 for memory it cannot give — how far does inflation get before `balloon_page_alloc()` fails, does `deflate_on_oom` return the pages before the L1's OOM killer picks the inner VMM, and how long does the L1 thrash first? §10.4 excludes nested from the balloon paths on the strength of the direction of that answer, not its magnitude.
- How much host RAM do L0's nested paging structures cost per L2, and does that charge (allocated `GFP_KERNEL_ACCOUNT`, so it lands in the `vmm-helper` memcg) fit inside the roughly 26 GiB of headroom between the container's `memory: 448Gi` cap and the ~422 GiB of guest RAM admission will hand out? If it is material, nested-enabled Nodes need a lower `SPARKBOX_MEM_ADMISSION_PCT`.
- Does a paused-and-resumed Cloud Hypervisor sandbox with a live L2 actually come back correct through our sequence (pause → snapshot to a directory → kill the VMM → restore later)? Until it does, §10.4's drop-the-snapshot-on-nested-pause line stands, and upstream has no test that combines `nested` with snapshot or restore, so a failure would be an upstream feature gap rather than a Sparkbox bug.

## 2. Risk register and kill criteria

§8 lists what hardware has to answer and §9 lists what we would build. Neither
says what result would make us stop. This section does. Every gate below is a
command or a number, and the numbers are compared against a Firecracker
baseline **recorded in the same run on the same Node**, not against a
remembered figure — the repo has no published boot/resume/RSS baseline today
(`docs/benchmarks/` covers fleet SSH only), so M1's first deliverable is that
baseline.

### 2.1 Risk register

| # | Risk | Probability | Impact | Earliest detection |
| --- | --- | --- | --- | --- |
| R1 | Nested state does not survive our pause | Medium | Nested sandboxes lose pause-to-disk | M2 |
| R2 | The snapshot mechanism does not port | **Certain** | M1 slips; helper needs a new verb | M1, day one |
| R3 | Snapshots are not portable across VMM versions | High | Fleet-wide cold boot at every upgrade | M1 |
| R4 | Resume latency regression (`copy` restore) | High | Density lever weakens | M1 |
| R5 | Boot latency / per-VM RSS regression | Low | Density model shifts | M1 |
| R6 | Advisory cadence and the wider device surface | Medium | Recurring forced upgrades | M0, then continuous |
| R7 | Node kernel patch lag on CKS | Medium | Nested never admissible | M0 |
| R8 | Guest-to-host escape from a nested sandbox | Low per sandbox, catastrophic | Whole Node, all tenants | Not detectable in advance |
| R9 | Maintenance cost of two drivers | High | Permanent tax on every VMM-touching change | M1 |
| R10 | Nested sandboxes break the density model | **Certain** | Unfunded RAM per nested sandbox | M2 |
| R11 | Silent misconfiguration | Medium | Wrong behaviour with no error | M1 (tests) |
| R12 | Nested workloads break perimeter semantics | High for bridged L2 | Confusing outage, not an escape | M2 |

The entries that need their reasoning stated:

**R1 — Nested state does not survive our pause.**
*Probability:* medium. Cloud Hypervisor's KVM backend calls `nested_state()` on
every x86_64 vCPU save (`hypervisor/src/kvm/mod.rs`), so the state is in the
snapshot by construction. What is untested is our sequence — pause → snapshot
into a jail directory → reap the VMM → hard-link out → restore later from a
different process. Upstream has no test that combines `nested` with
snapshot/restore at all: the only nested tests in `tests/integration.rs` are
`test_nested_virtualization_on`/`_off`, which assert that `/dev/kvm` does or
does not exist inside L1 and never boot an L2. M2's test is therefore the first
known execution of this path, and a failure is an upstream feature gap.
*Impact:* nested sandboxes lose pause-to-disk; everything else is unaffected.
*Earliest detection:* M2 — boot a stock Firecracker or Cloud Hypervisor guest
inside the sandbox, run a CPU-bound loop in it, `ctl pause`, `ctl resume`,
assert the loop's counter advanced and the inner guest still answers.
`kind` does not test this: its nodes are containers, so the test passes
vacuously under Firecracker today.
*Fallback:* refuse pause-with-snapshot for nested sandboxes and cold-boot them
via `DropSnapshots`, exactly as `Reboot`, `Resize` and `Rename` already do
(`internal/host/manager.go:2052,2136,2238`). That fallback is VMM-independent,
which is why R1 is not a kill criterion on its own.

**R2 — The snapshot mechanism does not port.**
*Probability:* certain — this is settled by code, not by measurement. The
helper pre-creates `mem.snap.next`/`state.snap.next` and publishes them with
`linkat(fd, "", AT_EMPTY_PATH)` (`internal/vmhelper/server_linux.go:605`).
Cloud Hypervisor writes three files into a directory that must already exist
and opens all three `create_new(true)` — `O_CREAT|O_EXCL` — so a pre-created
inode fails with `EEXIST`, and `link(2)` returns `EPERM` for a directory, so
the directory itself cannot be published either. Whatever the VMM writes lives
only inside the jail, which `cleanupSlot`'s `os.RemoveAll(jailWorkspace)`
(`server_linux.go:703`) deletes when the VMM is reaped.
*Impact:* schedule, not feasibility. Pause needs a new helper verb and a
promotion that is three links rather than one atomic `rename`.
*Earliest detection:* M1, day one — it is visible from the source before any
code is written.
*Mitigation:* budget it explicitly in M1 as helper work, and define the
crash-mid-promotion recovery before writing it. A partially written
`snap.next/` is indistinguishable from a complete one by existence alone
(`config.json` is written first), so promotion must validate all three files.

**R3 — Snapshots are not portable across VMM versions.**
*Probability:* high. Cloud Hypervisor's stability list states "Snapshot/restore
is not supported across different versions" (`release-notes.md`, v15.0
section, never superseded), and the only newer guarantee — `docs/live_migration.md`
"Version Compatibility" — is scoped to *migration* and starts at v54.
Firecracker by contrast ships a versioned snapshot format with a compatibility
check on load. Worse, Cloud Hypervisor serialises an 8320-byte
`KvmNestedStateBuffer` into **every** x86_64 vCPU state regardless of the
`nested` setting, and its layout is a `#[repr(C)]` struct from kvm-bindings, so
a crate bump can change the on-disk state for non-nested sandboxes too.
*Impact:* every paused sandbox is unresumable after a VMM bump. Today a Pod
replacement preserves running sandboxes (`cks-reflink-persistence-plan.md`,
implementation status); under a naive port, every VMM upgrade becomes a
fleet-wide cold boot.
*Earliest detection:* M1 — snapshot under the pinned build, restore under the
next patch release, on a scratch host.
*Mitigation:* record the VMM version in the snapshot directory, refuse a
mismatched restore, and cold-boot via `DropSnapshots` on mismatch. Treat a VMM
upgrade as a snapshot-format migration with a drain window.

**R4 — Resume latency regression.**
*Probability:* high under the default. Firecracker restores by mapping
`mem.snap` `MAP_PRIVATE` (`vstate/memory.rs::snapshot_file`), so a resume pays
no eager read. Cloud Hypervisor's default `copy` mode calls `fill_saved_regions`
(`vmm/src/memory_manager.rs`), which reads the saved ranges into guest RAM
before the VM runs — and because our guest RAM is plain private anonymous
memory, the memory file is dense, not sparse (the `SEEK_DATA`/`SEEK_HOLE` path
requires `MAP_SHARED` with a file offset), so that is a full-working-set read
per resume.
*Impact:* resume-on-connect is half the density model. A slow resume shows up
directly as user-visible latency.
*Earliest detection:* M1 on any KVM host — resume the same 12 GiB sandbox 20
times under each mode and take p50/p95.
*Mitigation:* configure `memory_restore_mode=copyonwrite`, which maps the file
copy-on-write and copies nothing up front. Its precondition — the memory file
must stay on disk and unchanged for the VM's lifetime — is compatible with the
`.next`-then-promote dance but not with `PackRootfs` deleting the memory
snapshot, so the pack path has to be sequenced against it. `ondemand` is not a
comparison to run: it needs `/dev/userfaultfd` (the jail mknods only
`/dev/kvm` and `/dev/net/tun`) or `userfaultfd(2)`, which containerd's
`RuntimeDefault` profile does not allow, and it fails the restore rather than
falling back.

**R5 — Boot latency and per-VM RSS regression.**
*Probability:* low. Both VMMs boot the same kernel with the same devices, and
guest RAM is lazily faulted in both. `--memory` defaults to `thp=on`
(`MADV_HUGEPAGE`) where Firecracker defaults to no huge-page hint, which is a
real difference in the other direction: THP raises RSS and works against the
balloon, because inflation releases memory 4 KiB at a time
(`release_memory_range_4k` → `MADV_DONTNEED`) while khugepaged's
`max_ptes_none` default of 511 lets it re-collapse a 2 MiB range from one
present PTE.
*Impact:* the pooled model in `resource-model-design.md` is calibrated on
measured idle cost, so a regression moves admission, not correctness.
*Earliest detection:* M1 — `hack/measure-density.py`, after fixing its VM
selector, which matches `/proc/<pid>/comm` against the literal `firecracker`
(`hack/measure-density.py:87`) and would otherwise report zero VMs and zero Pss
on a Cloud Hypervisor node.
*Mitigation:* measure `thp=on` against `thp=off` before pinning either.

**R6 — Advisory cadence and the wider device surface.**
*Probability:* medium and recurring. Three CVEs are assigned to Cloud
Hypervisor as of 2026-09-04: CVE-2023-30612 (API fd-close, fixed 30.1/31.1),
CVE-2026-27211 (host-file exfiltration through a QCOW2 header written into a
**raw** guest disk and then autodetected, fixed 50.1) and CVE-2026-45782
(virtio-blk use-after-free in the async I/O path, fixed 51.2/52.0). Two of the
three describe our configuration: a guest-writable raw ext4 image with a guest
that can reboot itself, and asynchronous block I/O — `open_raw` selects
io_uring, then aio, then sync, and the Pod's `RuntimeDefault` profile blocks
`io_uring_*` but allows `io_setup`/`io_submit`/`io_getevents`, so we land on
aio, still the async path. The device model is also larger: virtio-rng is
always instantiated and cannot be removed, and on x86-64 the i8042, CMOS, the
0x402 and 0x80 debug ports and the ACPI shutdown/GED/PM-timer devices are
created unconditionally.
*Impact:* forced upgrades on someone else's schedule, into a snapshot format
that is not version-portable (R3).
*Mitigation:* a **minimum pinned version** enforced in `hack/check-cks-pin.sh`
alongside the checksum, not a mailing-list subscription; `--landlock` (which
the CVE-2026-27211 advisory itself names as its workaround); `image_type=raw`
so no guest-authored format structure is parsed; and a named owner for the
feed. `security-hardening.md`'s acceptance gates already require "Firecracker/KVM
advisories and kernel updates as a release gate" — this adds one more feed to
the same gate.

**R7 — Node kernel patch lag on CKS.**
*Probability:* medium, and it is the one risk we cannot mitigate technically.
The floor is not the two CVEs §3 names. It is three: CVE-2026-53359
(Januscape, fixed 6.1.177 / 6.6.144 / 6.12.95 / 6.18.38 / 7.1.3),
CVE-2026-64561 (Zapscape, fixed 5.15.218 / 6.1.183 / 6.6.148 / 6.12.101 /
6.18.42 / 7.1.6) and CVE-2026-80726, the `role.invalid` follow-on that
Zapscape's own record defers, scored CVSS 9.3 with PR:N and fixed only in
6.1.183 / 6.6.152 / 6.12.104 / 6.18.45 / 7.1.9. All three are in
`arch/x86/kvm/mmu/mmu.c`. The binding floor is therefore **6.1.183, 6.6.152,
6.12.104, 6.18.45, 7.1.9 or mainline 7.2** — a Node at exactly 6.6.148 clears
Zapscape and is still exposed.
*Impact:* nested is never admissible on that pool. Since nested is the only
reason for the port, that is the whole feature.
*Earliest detection:* M0, in minutes.
*Mitigation:* none technical. The probe's most likely answer on a real cloud
kernel is exit 2 — RHEL 5.14.x, Ubuntu 6.8.0/6.14.0, Flatcar and Debian
6.1.0-NN all fail to map to an upstream stable series — so the design needs a
written rule for "undetermined": treat it as fail-closed and resolve it with a
vendor advisory reference, not a shrug.

**R8 — A guest-to-host escape from a nested sandbox lands on a shared Node.**
*Probability:* low per sandbox. *Impact:* the entire Node — every other
tenant's disks, the hot tier, the helper's capabilities.
This is the risk the rest of the design exists to bound, and the honest
baseline is worse than §7 originally stated: Sparkbox sets no Firecracker CPU
template (`internal/vmm/firecracker/fc.go` `MachineCfg` carries only
`VcpuCount`, `MemSizeMib`, `Smt`), and Firecracker's normaliser does not clear
VMX/SVM, so on a `kvm_*.nested=1` Node our guests already see the bit today.
What stops an L2 is the guest kernel's missing `CONFIG_KVM` — and M1 removes
that, for every sandbox, because there is one pinned `vmlinux-<arch>` asset.
After M1 the guarantee that non-nested sandboxes cannot run an L2 rests
entirely on Cloud Hypervisor's `nested=off` CPUID masking.
*Earliest detection:* not detectable in advance. That is the point.
*Mitigation, in order of strength:* (a) a dedicated node pool — which is a
second full `sparkbox-node` deployment, not a `nodeSelector`, because
`deployment.yaml` is `replicas: 1` / `strategy: Recreate` pinned to one
hostname with a Node-local `hostPath`, `deploy.sh` refuses a pool with more
than one eligible Node, and the device plugin advertises one `sparkbox.dev/kvm`
per Node; (b) the kernel floor in R7; (c) `CONFIG_LIST_HARDENED` on the Node,
which downgrades Zapscape's public chain from escape to denial of service;
(d) removing the loop bundle, already item 1 of `security-hardening.md`'s
remaining work. Note that a VMM escape then chains into KVM regardless of the
nested flag — the jailed VMM has `/dev/kvm` — so nested changes the exposure
of the *guest*, not of the VMM.

**R9 — Maintenance cost of two drivers.**
*Probability:* high, because M3's expected landing is "both drivers ship".
The measured split of `fc.go`'s 2,153 lines is roughly 950 VMM-neutral, 1,100
VMM-coupled and 100 mixed — and the coupled half is the half without a Go SDK
under it. `firecracker-go-sdk` supplies machine lifecycle, API-socket
readiness, the ordered configure-then-boot sequence, snapshot load, `PID()`,
`Wait()`, and the `WithProcessRunner` injection point the entire vmhelper
protocol is built around (`internal/vmhelper/protocol.go:113`). None of that
has a Cloud Hypervisor equivalent.
*Impact:* every future VMM-touching change is done twice, and there is no
parity harness to catch a drift: every lifecycle e2e test in the tree runs on
the mock driver (`e2e_test.go:81`, `fleet_e2e_test.go:244,470`), and the
Firecracker package's 1,361 lines of tests never boot a guest.
*Earliest detection:* M1 — the parity harness is a milestone deliverable to
*build*, not one to inherit.
*Mitigation:* one KVM-host integration test parametrised by driver; extend the
compile-time capability assertions from the four currently checked
(`Renamer`, `Rebooter`, `CPUStatser`, `TemplateReporter`) to all ten, so a
missing capability fails the build rather than degrading the fleet.

**R10 — Nested sandboxes break the density model.**
*Probability:* certain. virtio-balloon inflates by allocating pages **in the
L1** (`fill_balloon()` → `balloon_page_alloc()`, giving up with "Out of puff!"),
so it reclaims only what the L1 kernel considers free. An L2's resident RAM is
the inner VMM's anonymous memory — neither MemFree nor MemAvailable. The reaper's
other stage fails the same way: `shouldReap` gates pause on CPU idleness, and an
L1 running an inner VM burns host CPU continuously. Separately, KVM allocates
host-side shadow/nested page tables with `GFP_KERNEL_ACCOUNT`, so they land in
the `vmm-helper` memcg, which is capped at `memory: 448Gi` while the controller
admits against 480000 MB × 90% ≈ 422 GiB — roughly 26 GiB of headroom for
every L0 structure on the Node.
*Impact:* a nested sandbox charged `SPARKBOX_MEM_RESERVE_MB` (256) against a
12 GiB ceiling is an unfunded liability in both the owner pool and
`memAdmitPct`, and neither of the two density levers applies to it.
*Earliest detection:* M2 — balloon `actual` versus target for an L1 running a
4 GiB L2; host RSS of a ballooned-down nested sandbox.
*Mitigation:* charge a nested sandbox its full ceiling at admission, and keep
it out of the reaper's balloon and pause paths. `Pinned` does the second but
also disables idle pause wholesale, so this wants its own flag. Independently,
`host.Manager` should verify reclaim instead of assuming it: `setBalloonTarget`
sets `Ballooned = true` the moment the driver call returns
(`internal/host/manager.go:3463`), only `deflate` clears it, and `reclaimMemory`
skips any box with it set — so a target the guest never met reads as reclaimed
for as long as the sandbox stays warm. `vmm.BalloonStats.ActualMiB` is already
populated and nothing compares it to the target.

**R11 — Silent misconfiguration.**
*Probability:* medium; the failure mode is what makes it worth a row. Four
known cases, all of which produce wrong behaviour with no error: (a) `nested`
lives in the snapshot's `config.json` and a restore takes the whole VmConfig
from there, so a sandbox created with nested on resumes with nested on
whatever the helper passes — the §7 Node gate must be re-evaluated on resume;
(b) `--cpus nested=on` on a Node with `kvm_*.nested=0` boots fine and the guest
simply never sees `vmx`, so a mis-scheduled nested sandbox is a silently
non-nested sandbox, not a launch failure; (c) omitting `image_type=raw` on
v53.0 makes Cloud Hypervisor autodetect the format and set
`disable_sector0_writes`, silently refusing guest writes to sector 0 — and
supplying it means a guest that writes QCOW2 magic into the ext4 boot block
(bytes 0–1023, which neither `e2fsck` nor `zerofree` rewrites) makes its own
sandbox unbootable and poisons any template captured from it; (d) on arm64 the
CI kernel config has `# CONFIG_SERIAL_AMBA_PL011 is not set` while Cloud
Hypervisor's arm64 UART is a PL011, so the guest boots, SSH works, and
`console.log` is empty — the defect hides until the first boot that fails.
*Mitigation:* assert the full argv in a driver unit test the way
`check-cks-pin.sh` asserts checksums; re-run the node gate on restore; check
the first four bytes of `rootfs.ext4` in `Snapshot` before promoting a
capture; make "non-empty `console.log` on both arches" an M1 acceptance gate.

**R12 — Nested workloads break perimeter semantics.**
*Probability:* high for any inner VM that is not NATed. The per-slot rule
`-i <tap> ! -s <guest>/32 -j DROP` (`internal/vmhelper/server_linux.go:752`)
drops any packet whose source is not the L1's address, so a bridged or
macvtap inner VM — a common libvirt default, and often what a user reaching
for nested virt wants — is silently dropped. Sluice's DNS-derived allowlist
has the matching problem: an L2 with its own resolver produces egress from
names Sluice never observed, so a *tagged* sandbox's nested workload is
narrowed to nothing.
*Impact:* a confusing outage, not an escape. The perimeter holds; its meaning
changes.
*Mitigation:* document "the inner VM must be NATed behind the L1's address" as
a supported-configuration constraint, and test both cases in the M2 canary.

### 2.2 Kill criteria

Each of these is a measured result that ends the effort rather than starting
another iteration. Any one of them is sufficient.

**K1 — The non-nested pause/resume path is not reliable.**
100 consecutive pause/resume cycles on one 12 GiB sandbox under the Cloud
Hypervisor driver, with a monotonically incrementing sentinel written from
inside the guest. Any lost, duplicated or corrupted sentinel, or any resume
that does not return the guest to SSH, kills the port. Pause-to-disk is the
product; a VMM that does it less reliably than Firecracker is not a candidate
regardless of what else it offers.

**K2 — Resume is more than 2× slower than Firecracker after tuning.**
p50 resume-to-SSH for a 12 GiB warm sandbox, 20 samples, measured under
`copy`, `copyonwrite` and any other mode reachable from the jail, against the
Firecracker baseline recorded in the same run. If the best available mode is
worse than 2× p50 or 2.5× p95, kill it: resume-on-connect is the density
model, and a slower resume converts a pooled plan back into a per-VM one.

**K3 — Idle density regresses.**
`hack/measure-density.py` (selector fixed) at 10 idle 12 GiB guests. Kill if
the marginal MemAvailable cost per idle guest exceeds Firecracker's by more
than 25%, or mean per-VM Pss exceeds it by more than 64 MiB, under the better
of `thp=on` and `thp=off`. `resource-model-design.md`'s whole bargain is that
an idle guest costs its working set, not its ceiling.

**K4 — Nested is a demo, not a feature.**
A CPU-bound benchmark inside an L2, on a Node with `ept`/`npt` enabled at the
module level, against the same benchmark in the L1. Kill the *nested* feature
— and with it the reason for the port — if L2 throughput is below 50% of L1,
or if an L2 cannot be booted at all inside a Cloud Hypervisor guest. Nested
virtualization that is unusably slow is not what was asked for.

**K5 — CoreWeave cannot put us on a patched kernel.**
Kill if, by the end of M0, there is no CKS CPU pool whose Nodes are at or past
6.1.183 / 6.6.152 / 6.12.104 / 6.18.45 / 7.1.9 / 7.2 (or a vendor kernel with a
named advisory covering all three CVEs), **and** no written commitment to a
patch cadence that reaches that floor within a stated window. Nested on an
unpatched shadow MMU is not a risk to accept on a shared bare-metal Node.

**K6 — The escape rate does not slow.**
`arch/x86/kvm/mmu/mmu.c` has produced four CVEs in one year: CVE-2026-46113,
CVE-2026-53359, CVE-2026-64561 and CVE-2026-80726. Kill nested if a fifth
guest-to-host escape lands in the shadow MMU or the nested VMX/SVM code
between M0 and M3. At that rate the mitigation is not patching faster; it is
not exposing the surface.

**K7 — The helper needs a capability it does not have.**
Kill if making the Cloud Hypervisor snapshot path work requires any capability
beyond the helper's current ten — `CAP_SYS_ADMIN` for a bind mount being the
likely one. The whole reason `--chroot-jailer` exists is that the stock jailer
needs `CAP_SYS_ADMIN` (`security-hardening.md`, "Boundary and threat model").
Buying nested with that capability back is a net loss.

**K8 — Snapshots do not survive a VMM patch bump and upstream will not commit.**
Take a snapshot under the pinned build; restore it under the next patch
release. If the restore fails and upstream declines a compatibility guarantee
for the pinned line, kill the *default-VMM* end state: Cloud Hypervisor can
still run nested-requesting sandboxes that accept cold boot on upgrade, but it
cannot carry the fleet, because a fleet-wide loss of running state at every
upgrade is worse than not having nested.

**K9 — The two drivers cannot be tested as one.**
Kill the dual-driver end state if, at M3, the parity harness cannot run the
same test body against both drivers with the driver as a parameter, or if the
shared `internal/vmm/rootfs` and `internal/vmm/slots` packages have not
absorbed the disk, template, tap and slot code. Two independently maintained
2,000-line drivers with no shared test surface is a permanent tax that this
spike is not authorised to create; at that point pick one VMM.

### 2.3 Go/no-go checklist: enabling nested on the live CKS deployment

Every box is checked by running the named command and reading its output. All
must pass on the specific Node the nested pool runs on.

**Node preconditions**

- [ ] `kubectl exec -i -n sparkbox-poc deploy/sparkbox-node -c vmm-helper -- bash -s < hack/probe-nested-virt.sh; echo $?` exits `0`. The script is not in the image (`deploy/kubernetes/Containerfile` copies nothing from `hack/`) and `vmm-helper` has `readOnlyRootFilesystem: true`, so pipe it over stdin. Requires the corrected CVE tables — three CVEs, kernel.org provenance, absent series treated as unfixed.
- [ ] `uname -r` is at or past **6.1.183 / 6.6.152 / 6.12.104 / 6.18.45 / 7.1.9**, or mainline ≥ 7.2, or a vendor kernel whose advisory names all three of CVE-2026-53359, CVE-2026-64561 and CVE-2026-80726.
- [ ] `cat /sys/module/kvm_intel/parameters/nested` returns `Y`, or `cat /sys/module/kvm_amd/parameters/nested` returns `1`. Intel's is a `bool` module param and AMD's an `int`; do not compare either to a literal `1`.
- [ ] `cat /sys/module/kvm_intel/parameters/ept` (or `.../kvm_amd/parameters/npt`) returns on. With it off, every guest runs on the shadow MMU, nested or not.
- [ ] `zgrep -E 'CONFIG_(LIST_HARDENED|DEBUG_LIST|BUG_ON_DATA_CORRUPTION)=y' /proc/config.gz` (or `/boot/config-$(uname -r)`) matches at least `CONFIG_LIST_HARDENED`. This turns Zapscape's public chain into a DoS.
- [ ] `grep -o landlock /sys/kernel/security/lsm` is non-empty and `uname -r` is ≥ 6.2. Cloud Hypervisor pins Landlock ABI v3 as a `HardRequirement`, so without it `--landlock` fails `vm.create` rather than degrading.
- [ ] A written CoreWeave answer naming the kernel patch cadence for this pool, filed in this document.

**VMM pin**

- [ ] `cloud-hypervisor --version` reports **≥ v52.0** on both arches. Below that, `nested=off` is a silent no-op on AMD (the CPUID loop broke out on leaf 1 before reaching the SVM leaf) and `nested=off` is a hard parse error on arm64.
- [ ] `hack/check-cks-pin.sh` passes with a `SHA256_CLOUD_HYPERVISOR` row. `hack/stage-artifacts.sh` must emit that key first, or `check()` prints `skip … (not in manifest-<arch>.env)` and an absent pin passes CI silently. Upstream asset names are asymmetric (`cloud-hypervisor-static`, `cloud-hypervisor-static-aarch64`) and cannot be templated by the `$upstream_arch` variable.
- [ ] `go test ./internal/vmm/cloudhypervisor -run TestLaunchArgv` pins the exact argv: `--cpus nested=off,core_scheduling=vm`, `--disk path=…,image_type=raw,sparse=on`, `--console off`, `--seccomp true`, `--landlock`, `--net …,id=<pinned>,fd=…`. `--console` defaults to `tty` and `--serial` to `null`, so an omitted flag adds a device instead of failing.

**Driver and control plane**

- [ ] `sparkbox ctl info <box>` shows the sandbox's `nested` state, proving the field reached `vmm.Config` → helper protocol v2 → the `node.v1` inventory → `sandboxes.json` → the console.
- [ ] A test proves a paused nested sandbox is refused resume on a Node that fails the preflight, and cold-boots via `DropSnapshots` instead. `nested` is baked into the snapshot's `config.json` and a restore reads the whole VmConfig from there.
- [ ] A test proves `effectiveMemFor` charges a nested sandbox its full `MemMB`, not `SPARKBOX_MEM_RESERVE_MB`, and that the reaper's balloon and pause stages skip it.
- [ ] A test proves the driver maps Cloud Hypervisor's pre-first-sample balloon response (`last_update: 0`, empty `stats`) to `ok=false`. Otherwise `MemStats` reads free = available = 0, charges the guest its whole ceiling, and `reclaimMemory` balloons other sandboxes to fix an overage that does not exist.
- [ ] `hack/measure-density.py` reports a non-zero VM count on a Cloud Hypervisor node. It selects by `/proc/<pid>/comm == "firecracker"` today and would otherwise report zero VMs and zero Pss — a silent wrong answer on the script every M2 number depends on.
- [ ] M1's parity suite is green against both drivers on a KVM host: boot, SSH, pause, resume, fork, checkpoint, restore, rename, resize, reboot.
- [ ] `console.log` is non-empty after a Cloud Hypervisor boot on **both** x86_64 and arm64.

**Containment**

- [ ] A second `sparkbox-node` Deployment exists on its own Node, with its own hot tier and node identity, and `internal/placement` routes nested-enabled owners to it. One `nodeSelector` does not do this: `deployment.yaml` is `replicas: 1` / `strategy: Recreate` pinned to `kubernetes.io/hostname`, and the device plugin advertises one `sparkbox.dev/kvm` per Node.
- [ ] `grep -c 'sparkbox.dev/loop' deploy/kubernetes/deployment.yaml` returns `0` — the loop bundle removed, per item 1 of `security-hardening.md`'s remaining work.
- [ ] A rollback has been rehearsed on the canary and its downtime measured. `replicas: 1` + `Recreate` + one `sparkbox.dev/kvm` means the old Pod terminates before the new one starts: every sandbox on that Node goes down and comes back on the other VMM. Budget a window; rollback is a second full Recreate.
- [ ] The M2 canary recorded L2 behaviour for both a NATed and a bridged inner VM, and for a tagged sandbox whose L2 uses its own resolver.

**Operations**

- [ ] A named person owns the Cloud Hypervisor, Firecracker and kernel.org KVM advisory feeds, and the minimum pinned version is enforced in `hack/check-cks-pin.sh`, not only in a subscription.
- [ ] `security-hardening.md`'s acceptance gates re-run and pass against the Cloud Hypervisor driver: chroot launch, pause/resume, destroy, stale-jail cleanup, distinct non-root uid per VMM, default-deny east-west, no host parsing of user-derived disks.

### 2.4 If the answer is no-go

Ranked by how much of the ask each actually delivers. None of these is a
consolation prize for the same capability; they are different trades, stated as
such.

1. **Ship nothing, and say so.** Nested virtualization stays unavailable and
   `doctor` reports why. Honest, free, and the right answer if K5 or K6 fires —
   the reason for refusing is the Node kernel, not our engineering budget.
2. **Rootless / user-namespaced containers inside the sandbox** (Podman
   rootless, Buildah, Sysbox-style userns nesting). Covers Docker-in-Docker,
   which is the most-cited ask and needs no `/dev/kvm` at all — E2B runs Docker
   inside Firecracker sandboxes today and exe.dev's own docs say "exe.dev VMs
   run Docker normally". Costs a rootfs change, not a VMM. It does nothing for
   an Android emulator or an inner KVM guest.
3. **A dedicated bare-metal nested pool running the existing Firecracker
   driver with a masking-inverse CPU template.** A custom template setting
   CPUID.1:ECX[5] (Intel) or CPUID.80000001:ECX[2] (AMD) plus `CONFIG_KVM` in
   the guest kernel exposes VMX/SVM on a `nested=1` Node with the VMM we
   already ship, and §8.1's fallback — refuse pause-with-snapshot, cold-boot
   via `DropSnapshots` — covers the missing nested state. Days, not
   milestones. It is unsupported upstream (`src/vmm/src/arch/x86_64/msr.rs`:
   "Firecracker is not tested with nested virtualization"), Firecracker does
   not report a CPUID bit KVM quietly refuses, and it should be run as M0's
   control experiment rather than shipped — but it establishes the price of the
   Cloud Hypervisor port honestly, which is a supported switch plus snapshot
   fidelity, not the difference between possible and impossible.
4. **An external VM-per-request service**, brokered by Sparkbox: the sandbox
   gets an API and a network path to a VM it does not host. Keeps the escape
   surface off our Nodes entirely and needs no VMM change. It breaks the
   product's single unit — that VM is not paused, forked, checkpointed or
   billed with the sandbox — and adds a vendor.
5. **Harden the current baseline instead.** Pin `cpu_template: T2CL` (Intel) /
   `T2A` (AMD) so "guests never see VMX/SVM" becomes a property Sparkbox
   enforces rather than an assumption about CoreWeave's module parameters. This
   is the change that actually establishes the baseline §7's "nothing gets more
   than it has today" is written against, and it is available now, with no port.
   It moves in the opposite direction from the ask, and that is the point:
   choosing not to offer nested is a decision worth implementing rather than
   leaving implicit.
6. **Adopt Kata.** Rejected in §4 on its own merits, and one more reason
   belongs here: Kata's Go `kata-clh` shim never sets `nested` except on MSHV,
   so on a KVM node every `kata-clh` Pod boots with guest VMX/SVM exposed and
   no supported way to turn it off. The runtime-rs shim has
   `disable_nested_virtualization`, but it is a config-file key, not a Pod
   annotation, so the choice is made per RuntimeClass. Whatever else Kata
   costs, it cannot give us the per-sandbox gate §7 requires.

### Open questions from this section

- What is the exact kernel version on each CKS CPU pool we could schedule onto, and does CoreWeave commit to a patch cadence that reaches 6.1.183 / 6.6.152 / 6.12.104 / 6.18.45 / 7.1.9 / 7.2 within a stated window?
- Are kvm_intel.nested / kvm_amd.nested deliberately disabled on any CoreWeave pool, and can we request a pool where they are on?
- Is CONFIG_LIST_HARDENED set in the CKS node kernel, and if not can CoreWeave enable it? It downgrades Zapscape's public exploit chain from escape to denial of service.
- Does the CKS node kernel carry Landlock (CONFIG_SECURITY_LANDLOCK plus landlock in the boot-time LSM list) at ABI v3 or later? Without it --landlock fails vm.create rather than degrading.
- Will CoreWeave allocate a second bare-metal Node for a nested-only pool, given that a dedicated pool is a second full sparkbox-node Deployment with its own hot tier rather than a nodeSelector?
- Is vm.unprivileged_userfaultfd settable on CKS, and would a /dev/userfaultfd node be permitted in the device cgroup? This decides whether ondemand restore is ever measurable.
- Does upstream Cloud Hypervisor offer any snapshot-format compatibility guarantee within a pinned release line, or is every VMM patch bump a fleet-wide cold boot?
- What did the users actually ask for -- an Android emulator, an inner KVM guest, or Docker-in-Docker (which needs no /dev/kvm and works today)? The scope of the demo, and whether the port is justified at all, depends on the answer.
