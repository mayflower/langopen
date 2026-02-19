#!/usr/bin/env bash
set -euo pipefail

REGISTRY="${REGISTRY:-registry.data.mayflower.zone/data/langopen}"
TAG="${TAG:-$(git rev-parse --short HEAD 2>/dev/null || date +%Y%m%d%H%M%S)}"

build() {
  local component="$1"
  local dockerfile="$2"
  docker build -f "$dockerfile" -t "${REGISTRY}/${component}:${TAG}" .
  docker push "${REGISTRY}/${component}:${TAG}"
}

build api-server services/api-server/Dockerfile
build worker services/worker/Dockerfile
build control-plane services/control-plane/Dockerfile
build builder services/builder/Dockerfile
build operator services/operator/Dockerfile
build portal portal/Dockerfile

echo "Pushed tag ${TAG}."
echo "Update image.tag in deploy/helm/values-data-muc.yaml and ../data-cluster/langopen/values.yaml."
