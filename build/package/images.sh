#!/usr/bin/env bash
# Builds the four component images from the single canonical Dockerfile.
# Images are local dev projections (v0.1.0-dev); release qualification owns
# the digest-pinned publishing path and is out of scope for this ticket.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"

if [ ! -f internal/gen/web/dist/index.html ] || [ -z "$(ls -A internal/gen/web/dist/assets 2>/dev/null)" ]; then
  echo "frontend projection missing; building web first" >&2
  pnpm --dir web install --frozen-lockfile
  pnpm --dir web build
fi

for target in quoin plinth lintel stele; do
  if docker image inspect "quoin/$target:v0.1.0-dev" >/dev/null 2>&1 && [ "${QUOIN_FORCE_IMAGE_BUILD:-0}" != "1" ]; then
    echo "image quoin/$target:v0.1.0-dev already present"
    continue
  fi
  docker build -f build/package/Dockerfile --target "$target" -t "quoin/$target:v0.1.0-dev" .
done
