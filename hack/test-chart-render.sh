#!/usr/bin/env bash
# Validates that the API-key chart never emits a global per-virtual-host
# namespace override, including when callers pass the removed legacy value.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHART_DIR="${REPO_ROOT}/chart/llm-gateway-apikey"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

DEFAULT_RENDER="${TMP_DIR}/default.yaml"
LEGACY_RENDER="${TMP_DIR}/legacy-value.yaml"

HELM_VALUES=(
  --set operator.image.repository=example.invalid/apikey-operator
  --set authz.image.repository=example.invalid/apikey-authz
  --set istio.enabled=true
  --set istio.gatewayNamespace=istio-system
  --set istio.gatewaySelector.istio=ingressgateway
)
HELM_ARGS=(
  llm-gateway-apikey
  "${CHART_DIR}"
  --namespace llm-gateway
  "${HELM_VALUES[@]}"
)

helm lint "${CHART_DIR}" "${HELM_VALUES[@]}"

helm template "${HELM_ARGS[@]}" >"${DEFAULT_RENDER}"
helm template "${HELM_ARGS[@]}" \
  --set istio.extAuthzPerRoute.enabled=true >"${LEGACY_RENDER}"

if ! cmp -s "${DEFAULT_RENDER}" "${LEGACY_RENDER}"; then
  echo "legacy extAuthzPerRoute value unexpectedly changes rendered manifests" >&2
  diff -u "${DEFAULT_RENDER}" "${LEGACY_RENDER}" || true
  exit 1
fi

if grep -En 'ExtAuthzPerRoute|context_extensions|gateway-namespace' "${DEFAULT_RENDER}"; then
  echo "rendered manifests contain an unsafe per-virtual-host namespace override" >&2
  exit 1
fi
