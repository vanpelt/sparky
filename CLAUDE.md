# Sparky — Project Conventions

## Python

- Use **uv** for all Python work: dependency management, virtual environments, and running scripts.
- Pin dependencies in `requirements.txt`; use `uv pip install -r requirements.txt` or `uv run` to execute.

## Node.js

- Use **pnpm** for any Node.js tooling or packages.

## General

- Keep tools self-contained under `tools/<name>/`.
- Prefer simple, single-file scripts over complex project structures.
