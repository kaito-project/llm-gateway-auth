#!/usr/bin/env bash
# hack/teardown-test-env.sh
#
# Deletes the kind cluster. Safe to call if cluster doesn't exist.
#
# Environment variables:
#   KIND_CLUSTER_NAME    default: llm-gateway-test
#   CONTAINER_RUNTIME    "docker" or "podman" (auto-detected if unset)

set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-llm-gateway-test}"

# Auto-detect container runtime and set KIND_EXPERIMENTAL_PROVIDER for podman.
echo "▶ Detecting container runtime..."
if [[ -z "${CONTAINER_RUNTIME:-}" ]]; then
  if command -v docker &>/dev/null; then
    CONTAINER_RUNTIME="docker"
  elif command -v podman &>/dev/null; then
    CONTAINER_RUNTIME="podman"
  fi
fi
if [[ "${CONTAINER_RUNTIME:-}" == "podman" ]]; then
  export KIND_EXPERIMENTAL_PROVIDER="podman"
fi
echo "✓ Container runtime: ${CONTAINER_RUNTIME:-unknown}"

echo "▶ Checking for kind cluster '${KIND_CLUSTER_NAME}'..."
if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  echo "▶ Deleting kind cluster '${KIND_CLUSTER_NAME}'..."
  kind delete cluster --name "${KIND_CLUSTER_NAME}"
  echo "✓ Cluster '${KIND_CLUSTER_NAME}' deleted."
else
  echo "✓ Cluster '${KIND_CLUSTER_NAME}' does not exist -- nothing to do."
fi
