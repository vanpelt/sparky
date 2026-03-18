# Sparky — Project Conventions

## Python

- Use **uv** for all Python work: dependency management, virtual environments, and running scripts.
- Each Python tool should have a `pyproject.toml` with dependencies. Use `uv sync` to install and `uv run` to execute.

## Node.js

- Use **pnpm** for any Node.js tooling or packages.

## General

- Keep tools self-contained under `tools/<name>/`.
- Prefer simple, single-file scripts over complex project structures.
