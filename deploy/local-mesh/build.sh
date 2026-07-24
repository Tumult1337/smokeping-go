#!/usr/bin/env bash
# Compile the static gosmokeping binary the harness image wraps.
#
# Run this whenever you change Go or UI code, then `docker compose ... up -d
# --build`. It builds on the host because container npm is broken in this
# environment (see Dockerfile). The binary embeds the UI, so `make ui` must
# have populated internal/ui/dist first.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

if [ ! -f "$repo/internal/ui/dist/index.html" ]; then
  echo "internal/ui/dist is empty — run 'make ui' first" >&2
  exit 1
fi

echo "building static binary → deploy/local-mesh/gosmokeping"
cd "$repo"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o "$here/gosmokeping" ./cmd/gosmokeping
echo "done"
