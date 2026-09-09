#!/usr/bin/env bash
# Builds selected independently publishable application images. The frontend
# target compiles its own assets inside Docker, so backend image builds never
# need host Node tooling or an existing frontend dist directory.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"

image_namespace="${QUOIN_IMAGE_NAMESPACE:-quoin}"
default_tag="${QUOIN_IMAGE_TAG:-v0.1.0-dev}"
components="${QUOIN_IMAGE_COMPONENTS:-frontend,quoin,plinth,lintel,stele}"
versions="${QUOIN_IMAGE_VERSIONS:-}"

component_tag() {
  local component=$1 entry
  IFS=',' read -ra entries <<< "$versions"
  for entry in "${entries[@]}"; do
    if [[ "$entry" == "$component="* ]]; then
      printf '%s\n' "${entry#*=}"
      return
    fi
  done
  printf '%s\n' "$default_tag"
}

IFS=',' read -ra selected <<< "$components"
for target in "${selected[@]}"; do
  case "$target" in frontend|quoin|plinth|lintel|stele) ;; *) echo "unknown image component: $target" >&2; exit 2;; esac
  image="$image_namespace/$target:$(component_tag "$target")"
  if [ "$target" = frontend ]; then
    docker build -f build/package/Dockerfile --target web \
      ${QUOIN_IMAGE_GOPROXY:+--build-arg "GOPROXY=$QUOIN_IMAGE_GOPROXY"} \
      -t "$image" .
  else
    docker build -f build/package/Dockerfile --target "$target" \
      --build-arg "RELEASE_VERSION=$(component_tag "$target")" \
      ${QUOIN_IMAGE_GOPROXY:+--build-arg "GOPROXY=$QUOIN_IMAGE_GOPROXY"} \
      -t "$image" .
  fi
done
