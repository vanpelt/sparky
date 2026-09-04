# Guest federation: the list of relying parties

Status: **implemented** (2026-09-03). Every fleet federates with HiveMind by
default; anything more is a file an operator hands the deploy.

## What it is

A sandbox proves who it is with an OIDC assertion this fleet signs — see
[identity-federation-design.md](identity-federation-design.md) for the issuer,
the claims and the tap-position authentication. Every relying party that
accepts that shape of proof needs the same three things from the guest:

1. an assertion minted for **its** audience,
2. kept fresh at a path **its** client reads,
3. and a few environment variables telling the client where the assertion is
   and which rule to present it against.

None of that is specific to the party. So none of it is hard-coded: the fleet
carries a **list**, `internal/federation.Config`, and the guest walks it. HiveMind
is one entry, OpenAI's workload identity is another, and the next one is a
change to a file rather than a release.

## The file

`--federation-config FILE` on both the gateway and the node. JSON, one object
per party:

```json
{
  "federators": [
    { "name": "hivemind", "audience": "https://hivemind.wandb.tools" },
    {
      "name": "openai",
      "audience": "https://api.openai.com/v1",
      "token_file": "/var/run/secrets/openai.com/identity-token",
      "token_file_env": "OPENAI_IDENTITY_TOKEN_FILE",
      "context_env": "OPENAI_WORKLOAD_IDENTITY_CONTEXT",
      "env": {
        "OPENAI_FEDERATION_RULE_ID": "idpm_...",
        "OPENAI_IDENTITY_PROVIDER_ID": "idp_...",
        "OPENAI_SERVICE_ACCOUNT_ID": "svc_..."
      }
    }
  ]
}
```

| Field | Meaning |
|---|---|
| `name` | The operator's handle: lowercase, unique. Names the default token directory, the guest's log lines and its profile snippet. |
| `audience` | The `aud` the guest asks the metadata service to mint for. Must match what the party configured, exactly; it is opaque to both ends. |
| `token_file` | Where the guest keeps the assertion. Default `/var/run/secrets/<name>/token`. Mode 0600 in a 0700 directory owned by the login user. |
| `token_file_env` | Optional. A variable exported with `token_file` as its value, for a client that does not already know the path. |
| `env` | Optional. Variables exported as-is: the identifiers a client needs to present the assertion. |
| `context_env` | Optional. A shell variable exported with a JSON attribution context, `{"instance_id":"<sandbox>","labels":{"owner":...,"box":...}}`. |

The file **replaces** the built-in list rather than adding to it. Omit
`hivemind` and sandboxes get no HiveMind token; `deploy.sh` says so out loud.
With no file at all, the fleet federates with HiveMind alone, at
`--hivemind-audience`, exactly as it did before the list existed.

`sparkbox federation check FILE` runs the same loader `serve` does and prints
the list as guests will see it. `deploy.sh --federation-config` runs it before
applying anything, because the binary is the validator and a list it refuses is
a gateway that will not start.

### What the loader refuses, and why

Unknown fields (a misspelt `token_file_env` is a federation that half-works,
discovered in a sandbox). Duplicate names, shared token files, a relative or
unclean path. Any value that carries whitespace, a quote, a backslash, `$` or a
backtick: these land bare in `/etc/environment`, which pam_env reads literally
and a shell sourcing the file reads with quote removal and expansion, and the
two agree only on a value that needs neither. URLs, paths and every identifier
a relying party issues fit.

Everything in the file is an identifier, never a secret. A provider or rule id
grants nothing without an assertion this fleet's OIDC key signed, which is why
the file is a ConfigMap and not a Secret.

## What the host does with it

- **The issuer's allowlist.** Every audience in the list joins
  `--oidc-audiences` by construction (`federation.WithAudiences`), rather than
  by the operator remembering to repeat it. Those are two statements of one
  fact, and the failure when they disagree is remote from its cause: everything
  configures cleanly and guests take a 400 from a mint nobody is watching. It
  never widens an *empty* allowlist, because empty means "any" and appending
  would silently narrow it.
- **`GET /federation`** on the metadata service serves the list to the calling
  sandbox. The encoding is deliberately not JSON: one fact per line,
  `<name> TAB <key> TAB <value>`, in mint order, `env` repeated once per
  variable as `KEY=VALUE`. The guest reads it with `awk` and nothing else — no
  JSON parser is a dependency the token unit may take on this early in boot,
  and a list of objects is exactly the shape `sed` cannot walk. The loader's
  character rules are what make the format unambiguous.
- **A node serves the same list.** It signs nothing — the mint relays to the
  gateway, whose allowlist is what can refuse an audience — but a sandbox must
  not be able to tell which machine it landed on from the parties it was
  offered, so both halves read the same file.

Both halves read the file **once, at startup**. On CKS the deploy stamps the
ConfigMap's hash onto both pod templates so a changed list rolls the pods; a
`kubectl edit` of the ConfigMap alone restarts nothing.

## What the guest does with it

`sparkbox-token` (`deploy/install-guest-identity.sh`, `IDENTITY_REV=28`) runs
at boot and every 45 minutes:

1. `GET /federation` → `/run/sparkbox/federation`. On failure it keeps the last
   list it saw; with none, it falls back to one assertion at the host's default
   audience, at HiveMind's path — so a host that predates `/federation` never
   leaves a new template tokenless.
2. For each party, in order: `GET /token?aud=<audience>` → `token_file`, via a
   temp file and a rename, so a client re-reading on its own schedule never
   catches a half-written token. Every party is attempted whatever happened to
   the one before it; the unit's exit status reports whether *any* failed,
   which is what makes systemd retry, and it is decided last.
3. `GET /identity` → `/run/sparkbox/identity.json`, the decoded claims.
4. `sparkbox-federation-env` renders the managed block in `/etc/environment`
   — `token_file_env` and `env` for every party — and one
   `/etc/profile.d/sparkbox-<name>.sh` per party with a `context_env`.

Step 4 runs **unconditionally**, including on an empty list. That call is what
removes a party's variables from a guest whose fleet dropped it, and from a
fork template that crossed fleets: a `codex` that finds a stale
`OPENAI_IDENTITY_TOKEN_FILE` federates and fails instead of offering the login
it would otherwise have offered.

### Why two files in the guest

`/etc/environment` carries the credentials because it is the only file that
reaches both an interactive shell (pam_env) and the non-interactive
`ssh box '<cmd>'` execs agents actually run, which read no profile at all.

The attribution context is a JSON object, so it contains double quotes, and
`/etc/environment` cannot carry one safely: written bare it survives pam_env but
a shell sourcing the file performs quote removal on it — measured, not feared,
`{"instance_id":"box"}` comes back as `{instance_id:box}` — and written quoted it
depends on pam_env stripping the pair back off, which is not a promise that
file format makes. A profile.d snippet is unambiguously shell. The cost is that
attribution reaches login shells and not `ssh box '<cmd>'`, which is the right
way round for a field every party so far documents as optional.

## Adding a party

1. Get from the party what it needs: the audience it verifies, the path or
   variable its client reads the assertion from, and any identifiers the
   client presents alongside it. The claims it can write policy against are in
   [identity-federation-design.md](identity-federation-design.md).
2. Add an entry to the file. `sparkbox federation check FILE`.
3. **Egress.** A governed sandbox reaches only what its rule-set allows, and the
   party's hosts are not in the list. Add them to
   `deploy/sluice-allowlist.txt` (the floor under every governed sandbox; the
   seed is only written when absent, so an existing fleet needs the lines added
   by hand plus `systemctl reload-or-restart sluice`) and to
   `internal/netrules.TrustedDomains` (the console's prefill and a new
   environment's default rule-set). This is the one step that is still a
   release rather than a deploy, and deliberately: egress is policy.
4. `deploy/kubernetes/deploy.sh --image ... --federation-config FILE`. Running
   sandboxes pick the change up on their next refresh; new ones get it at boot.

## Verifying

From inside a sandbox:

```
cat /run/sparkbox/federation               # the list, as served
ls -l /var/run/secrets/*/                  # 0700 dirs, 0600 tokens, owned by you
grep -A9 'sparkbox federation' /etc/environment
systemctl status sparkbox-token            # a failed mint fails the unit
```

`sparkbox whoami` prints the claims a party is matching a rule against.

## Tests

- `internal/federation` — loading, every refusal, the guest encoding pinned
  byte-for-byte and round-tripped, the allowlist rules.
- `internal/metadata/federation_test.go` — the endpoint serves exactly the
  encoding, an empty list is an empty body, a non-sandbox caller is refused.
- `deploy/federation_test.go` — runs the real `sparkbox-federation-env` against
  a tree with a list the real encoder rendered: exports, removals, a party
  nobody has heard of, and that attribution survives being sourced by a shell.
  Also pins the token script's shape and that the example file loads.
- `cmd/sparkbox/wiring_test.go` — a node's metadata service carries the list.
