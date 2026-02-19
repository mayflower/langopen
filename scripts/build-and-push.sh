#!/usr/bin/env bash
set -euo pipefail

REGISTRY="${REGISTRY:-ghcr.io/mayflower/langopen}"
TAG="${TAG:-$(git rev-parse --short=12 HEAD 2>/dev/null || date +%Y%m%d%H%M%S)}"

build() {
  local component="$1"
  local dockerfile="$2"
  local image="${REGISTRY}/${component}:${TAG}"
  local attempt=1
  local max_attempts=4

  while true; do
    if docker build -f "$dockerfile" -t "$image" . && docker push "$image"; then
      break
    fi
    if [[ "$attempt" -ge "$max_attempts" ]]; then
      echo "failed to build/push ${image} after ${max_attempts} attempts" >&2
      return 1
    fi
    local sleep_seconds=$((attempt * 5))
    echo "retrying ${image} in ${sleep_seconds}s (attempt $((attempt + 1))/${max_attempts})" >&2
    sleep "$sleep_seconds"
    attempt=$((attempt + 1))
  done
}

build api-server services/api-server/Dockerfile
build worker services/worker/Dockerfile
build control-plane services/control-plane/Dockerfile
build builder services/builder/Dockerfile
build operator services/operator/Dockerfile
build runtime-runner services/runtime-runner/Dockerfile
build portal portal/Dockerfile

echo "Pushed tag ${TAG}."
echo "Update image.tag in deploy/helm/values-data-muc.yaml and ../data-cluster/langopen/values.yaml."
