#!/usr/bin/env bash
set -euo pipefail

# The CLI and both remote plugins are pinned: regeneration is deterministic on
# a clean machine and does not depend on a globally installed protoc toolchain.
SPARKBOX_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$SPARKBOX_DIR"

go run github.com/bufbuild/buf/cmd/buf@v1.72.0 lint
go run github.com/bufbuild/buf/cmd/buf@v1.72.0 generate
