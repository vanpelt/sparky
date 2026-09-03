# AWP backend proof

Sparkbox can be an AWP sandbox backend without running Firecracker inside an
AWP sandbox. Sparkbox's Firecracker VM **is** the sandbox boundary:

```text
AWP control plane
    |
    | POST /v1/awp/sandboxes
    v
Sparkbox gateway
    |
    | Firecracker create (the `awp` template)
    v
Sparkbox microVM
    |
    `-- agent-runtime on :8000
```

The AWP runtime may run directly as a process in the guest or as a privileged
Docker container in the guest. The latter preserves its current DinD image
contract. Either way, there is only one virtualization boundary. Docker inside
the microVM is process/container isolation, not nested KVM.

## Provisional action surface

The proof adds three REST operations:

- `POST /v1/awp/sandboxes`
- `GET /v1/awp/sandboxes/{sandbox_id}`
- `DELETE /v1/awp/sandboxes/{sandbox_id}`

They are deliberately operator-only. Today the caller presents an ordinary
Sparkbox edge session; this is the temporary action AMP can call during the
proof. Later, an AWP workload-OIDC authenticator can replace that HTTP-edge
credential without changing the request or VM lifecycle contract.

Create takes:

```json
{
  "sandbox_id": "sbx-0195e6d2",
  "run_id": "run-0195e6d2",
  "tenant_id": "tenant-acme",
  "control_plane_url": "https://awp.example.com",
  "oidc_audience": "https://awp.example.com",
  "vcpus": 4,
  "mem_mb": 12288
}
```

Send an `Idempotency-Key` on POST and DELETE. The response contains the normal
Sparkbox lifecycle record plus this identity descriptor:

```json
{
  "workload_identity": {
    "issuer": "https://oidc.catnip.sh",
    "audience": "https://awp.example.com",
    "sandbox_id": "291289eb-2273-4028-8cbe-fa2e7a708643"
  }
}
```

That final `sandbox_id` is Sparkbox's immutable provider UUID and is also the
claim in the token minted to the guest. AWP should bind it during exchange. The
logical AWP sandbox id, run id, tenant id, VM name, or operator account are
useful selectors and audit fields, but are not the workload trust anchor.

Create does the following as one provider transition:

1. verifies the caller is an operator;
2. verifies the requested OIDC audience is allowed by this gateway;
3. requires an AWP runtime snapshot to be bound to the `awp` tag;
4. creates the Firecracker VM with `awp` and a private per-launch tag;
5. stores and synchronously delivers only non-secret launch metadata;
6. records the immutable Sparkbox VM id in that metadata; and
7. pins the VM so the ordinary interactive idle reaper cannot suspend a run.

The private tag makes GET recover the provider record after a gateway restart
and lets DELETE remove the launch rows. An AWP operation refuses an ordinary
Sparkbox VM even when its name is known. If any post-create step fails, the VM
and launch rows are rolled back.

## Runtime template

The endpoint fails closed with `awp_template_unbound` until the operator binds
a purpose-built image. It never silently falls back to the generic Sparkbox
image.

A minimal first template is:

1. start a universal builder VM;
2. install Docker and the AWP `agent-runtime` image;
3. install a systemd unit that waits for the required launch variables, then
   runs that image on host networking with port 8000;
4. run the image with `WARM_BOOTSTRAP=true` for the lifecycle/health proof;
5. capture and bind it:

   ```sh
   ssh ctl@catnip.sh snapshot create awp-builder awp-runtime
   ssh ctl@catnip.sh snapshot bind awp-runtime --tag awp
   ```

The current runtime image is designed to start its own Docker daemon and then
drop to uid 65532. Inside a Sparkbox VM it can keep that contract: run the image
privileged with host networking, and Firecracker remains the security boundary.
The gateway already routes a sandbox's default hostname to guest port 8000,
which is the runtime's listener.

Do not source `/etc/environment` from a shell unit. Sparkbox writes pam_env
syntax, not shell syntax, and values may contain shell metacharacters. A guest
launcher should parse only the named fields it needs or, preferably, consume a
future structured metadata document.

## OIDC cutover

There are two independent trust relationships:

1. **AWP control plane to Sparkbox API.** The provisional action uses an
   operator session. The production integration should authenticate the AWP
   service with OIDC and map the trusted service identity to this same
   operator-only operation.
2. **Sparkbox VM to AWP control plane.** The guest obtains a fresh token from
   Sparkbox's tap-bound metadata service using `oidc_audience`, then posts it to
   an AWP exchange endpoint. AWP verifies issuer, signature, audience, expiry,
   one-time `jti`, and the expected immutable Sparkbox `sandbox_id`; it returns
   the short-lived credential currently represented by `BOOTSTRAP_TOKEN`.

The create request and stored environment intentionally contain no
`BOOTSTRAP_TOKEN`. During the warm-runtime proof AWP may claim the service over
its existing `/bootstrap` endpoint. The production cold path should add the
workload-identity exchange and keep the resulting bearer in memory (or a 0600
runtime file), never in Sparkbox's environment-variable store.

The gateway must include the AWP audience in `--oidc-audiences`. A create with
an audience outside that allowlist fails before a VM or launch row is written.

## AWP adapter shape

The existing AWP control plane already isolates providers behind
`runs.SandboxProvisioner`. A future Sparkbox implementation can follow the CWS
provider's shape:

- `Provision` calls this POST and persists the returned Sparkbox provider UUID;
- `Deprovision` calls this DELETE (404 is an already-gone success);
- reconciliation calls GET and maps `running` to ready;
- `LookupClaimUID` returns the same no-claim result as CWS; and
- backend selection persists `sparkbox` on the run before provisioning.

The one intentional interface change is bootstrap authentication: the
Sparkbox provider does not send the `token` argument as an environment
variable. It arranges the workload-identity exchange described above instead.
