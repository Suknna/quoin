#!/usr/bin/env bash
# Builds the four component images from the single canonical Dockerfile.
# Images are local dev projections (v0.1.0-dev); release qualification owns
# the digest-pinned publishing path and is out of scope for this ticket.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"

if [ "${QUOIN_FORCE_IMAGE_BUILD:-0}" = "1" ] || [ ! -f internal/gen/web/dist/index.html ] || [ -z "$(ls -A internal/gen/web/dist/assets 2>/dev/null)" ]; then
  echo "building frontend projection" >&2
  pnpm --dir web install --frozen-lockfile
  pnpm --dir web build
fi

image_namespace="${QUOIN_IMAGE_NAMESPACE:-quoin}"
image_tag="${QUOIN_IMAGE_TAG:-v0.1.0-dev}"
for target in quoin plinth lintel stele; do
  image="$image_namespace/$target:$image_tag"
  if docker image inspect "$image" >/dev/null 2>&1 && [ "${QUOIN_FORCE_IMAGE_BUILD:-0}" != "1" ]; then
    echo "image $image already present"
    continue
  fi
  docker build -f build/package/Dockerfile --target "$target" \
    ${QUOIN_IMAGE_GOPROXY:+--build-arg "GOPROXY=$QUOIN_IMAGE_GOPROXY"} \
    -t "$image" .
done
