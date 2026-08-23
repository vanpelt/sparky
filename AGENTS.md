# Sparky Agent Guide

## Repository shape

- Sparky is a collection of experiments and self-contained tools for the OpenClaw instance running on a DGX Spark.
- Keep tool-specific code, dependencies, tests, and documentation under `tools/<name>/` unless the change is genuinely repository-wide.
- Read the target tool's README and any nearer `AGENTS.md` before editing. A nested `AGENTS.md` extends and overrides this file for its subtree.
- The repository may contain active, unrelated work. Inspect `git status` first and preserve changes you did not make.

## Tooling

- Python: use `uv` for dependency management, environments, and execution. Python tools should declare dependencies in `pyproject.toml`; use `uv sync` and `uv run`.
- Node.js: use `pnpm` for packages and scripts.
- Go: this is not a root Go workspace. Run Go commands from the directory containing the relevant `go.mod` (currently `tools/sparkbox` or `tools/sluice`).
- Prefer existing scripts, Make targets, and package-local helpers over introducing a new repository-wide toolchain.

## Change discipline

- Keep edits scoped to the owning tool. Do not refactor sibling experiments as part of an unrelated change.
- Treat READMEs and design documents as context, not as stronger evidence than current code and tests. Note proposal/status labels and check recent history when behavior is evolving.
- Update user-facing documentation when commands, configuration, deployment steps, or public behavior change.
- Never commit secrets, host credentials, private keys, local state, build artifacts, or scratch output.

## Verification

- Use the narrowest useful checks while iterating, then run the owning tool's documented full checks when practical.
- Report platform-dependent checks that could not run locally. DGX, Linux, KVM, eBPF, network, and deployment behavior may require a suitable host even when portable unit tests pass.
- For documentation-only changes, verify paths and commands against the current tree and review the final diff; do not run expensive builds without a reason.
