#!/usr/bin/env bash
# Validates Helm rendering invariants for both authentication charts.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
APIKEY_CHART_DIR="${REPO_ROOT}/chart/llm-gateway-apikey"
AZUREAUTH_CHART_DIR="${REPO_ROOT}/chart/llm-gateway-azureauth"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

APIKEY_VALUES=(
  --set operator.image.repository=example.invalid/apikey-operator
  --set authz.image.repository=example.invalid/apikey-authz
  --set istio.enabled=true
  --set istio.gatewayNamespace=istio-system
  --set istio.gatewaySelector.istio=ingressgateway
)
AZUREAUTH_VALUES=(
  --set authz.image.repository=example.invalid/azure-authz
  --set azure.tenantId=00000000-0000-0000-0000-000000000000
  --set 'azure.jwt.audiences[0]=example-audience'
)

render_apikey() {
  helm template llm-gateway-apikey "${APIKEY_CHART_DIR}" \
    --namespace llm-gateway \
    "${APIKEY_VALUES[@]}" \
    "$@"
}

render_azureauth() {
  helm template llm-gateway-azureauth "${AZUREAUTH_CHART_DIR}" \
    --namespace llm-gateway \
    "${AZUREAUTH_VALUES[@]}" \
    "$@"
}

fail() {
  echo "$1" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local pattern="$2"
  local description="$3"

  if ! grep -Eq "${pattern}" "${file}"; then
    fail "${description}: expected pattern '${pattern}'"
  fi
}

assert_not_contains() {
  local file="$1"
  local pattern="$2"
  local description="$3"

  if grep -Eq "${pattern}" "${file}"; then
    fail "${description}: unexpected pattern '${pattern}'"
  fi
}

helm lint "${APIKEY_CHART_DIR}" "${APIKEY_VALUES[@]}"
helm lint "${AZUREAUTH_CHART_DIR}" "${AZUREAUTH_VALUES[@]}"

# The removed extAuthzPerRoute value must never change API-key manifests or
# restore the unsafe global per-virtual-host namespace override.
APIKEY_DEFAULT_RENDER="${TMP_DIR}/apikey-default.yaml"
APIKEY_LEGACY_RENDER="${TMP_DIR}/apikey-legacy-value.yaml"
render_apikey >"${APIKEY_DEFAULT_RENDER}"
render_apikey --set istio.extAuthzPerRoute.enabled=true >"${APIKEY_LEGACY_RENDER}"

if ! cmp -s "${APIKEY_DEFAULT_RENDER}" "${APIKEY_LEGACY_RENDER}"; then
  echo "legacy extAuthzPerRoute value unexpectedly changes rendered manifests" >&2
  diff -u "${APIKEY_DEFAULT_RENDER}" "${APIKEY_LEGACY_RENDER}" || true
  exit 1
fi

assert_not_contains \
  "${APIKEY_DEFAULT_RENDER}" \
  'ExtAuthzPerRoute|context_extensions|gateway-namespace' \
  "API-key default render contains an unsafe per-virtual-host namespace override"

# Autoscaling is opt-in. Fixed replica counts remain in both Deployments and no
# HPA is rendered by default.
AZUREAUTH_DEFAULT_RENDER="${TMP_DIR}/azureauth-default.yaml"
APIKEY_DEFAULT_DEPLOYMENT="${TMP_DIR}/apikey-default-deployment.yaml"
AZUREAUTH_DEFAULT_DEPLOYMENT="${TMP_DIR}/azureauth-default-deployment.yaml"
render_azureauth >"${AZUREAUTH_DEFAULT_RENDER}"
render_apikey --show-only templates/authz/deployment.yaml >"${APIKEY_DEFAULT_DEPLOYMENT}"
render_azureauth --show-only templates/authz/deployment.yaml >"${AZUREAUTH_DEFAULT_DEPLOYMENT}"

assert_not_contains "${APIKEY_DEFAULT_RENDER}" '^kind: HorizontalPodAutoscaler$' \
  "API-key chart renders an HPA by default"
assert_not_contains "${AZUREAUTH_DEFAULT_RENDER}" '^kind: HorizontalPodAutoscaler$' \
  "Azure auth chart renders an HPA by default"
assert_contains "${APIKEY_DEFAULT_DEPLOYMENT}" '^  replicas: 3$' \
  "API-key authz Deployment lost its fixed replica count"
assert_contains "${AZUREAUTH_DEFAULT_DEPLOYMENT}" '^  replicas: 2$' \
  "Azure authz Deployment lost its fixed replica count"

# Enabling autoscaling renders a correctly targeted HPA and leaves replica
# ownership to the HPA instead of the Helm-managed Deployment.
for chart in apikey azureauth; do
  if [[ "${chart}" == "apikey" ]]; then
    render=render_apikey
    target=apikey-authz
    min_replicas=3
  else
    render=render_azureauth
    target=azure-authz
    min_replicas=2
  fi

  cpu_hpa="${TMP_DIR}/${chart}-cpu-hpa.yaml"
  memory_hpa="${TMP_DIR}/${chart}-memory-hpa.yaml"
  combined_hpa="${TMP_DIR}/${chart}-combined-hpa.yaml"
  autoscaled_deployment="${TMP_DIR}/${chart}-autoscaled-deployment.yaml"

  "${render}" \
    --set authz.autoscaling.enabled=true \
    --show-only templates/authz/hpa.yaml >"${cpu_hpa}"
  "${render}" \
    --set authz.autoscaling.enabled=true \
    --set-string authz.autoscaling.targetCPUUtilizationPercentage= \
    --set authz.autoscaling.targetMemoryUtilizationPercentage=70 \
    --show-only templates/authz/hpa.yaml >"${memory_hpa}"
  "${render}" \
    --set authz.autoscaling.enabled=true \
    --set authz.autoscaling.targetMemoryUtilizationPercentage=70 \
    --show-only templates/authz/hpa.yaml >"${combined_hpa}"
  "${render}" \
    --set authz.autoscaling.enabled=true \
    --show-only templates/authz/deployment.yaml >"${autoscaled_deployment}"

  assert_contains "${cpu_hpa}" '^apiVersion: autoscaling/v2$' \
    "${chart} HPA uses the wrong API version"
  assert_contains "${cpu_hpa}" '^kind: HorizontalPodAutoscaler$' \
    "${chart} HPA has the wrong kind"
  assert_contains "${cpu_hpa}" "^  name: ${target}$" \
    "${chart} HPA has the wrong name"
  assert_contains "${cpu_hpa}" '^  namespace: llm-gateway$' \
    "${chart} HPA has the wrong namespace"
  assert_contains "${cpu_hpa}" "^    name: ${target}$" \
    "${chart} HPA targets the wrong Deployment"
  assert_contains "${cpu_hpa}" "^  minReplicas: ${min_replicas}$" \
    "${chart} HPA has the wrong minimum"
  assert_contains "${cpu_hpa}" '^  maxReplicas: 10$' \
    "${chart} HPA has the wrong maximum"
  assert_contains "${cpu_hpa}" '^        name: cpu$' \
    "${chart} CPU-only HPA omits the CPU metric"
  assert_contains "${cpu_hpa}" '^          averageUtilization: 80$' \
    "${chart} CPU-only HPA has the wrong utilization target"
  assert_not_contains "${cpu_hpa}" '^        name: memory$' \
    "${chart} CPU-only HPA includes a memory metric"

  assert_contains "${memory_hpa}" '^        name: memory$' \
    "${chart} memory-only HPA omits the memory metric"
  assert_contains "${memory_hpa}" '^          averageUtilization: 70$' \
    "${chart} memory-only HPA has the wrong utilization target"
  assert_not_contains "${memory_hpa}" '^        name: cpu$' \
    "${chart} memory-only HPA includes a CPU metric"

  assert_contains "${combined_hpa}" '^        name: cpu$' \
    "${chart} combined HPA omits the CPU metric"
  assert_contains "${combined_hpa}" '^        name: memory$' \
    "${chart} combined HPA omits the memory metric"
  assert_not_contains "${autoscaled_deployment}" '^  replicas:' \
    "${chart} autoscaled Deployment still owns replicas"

  if "${render}" \
    --set authz.autoscaling.enabled=true \
    --set-string authz.autoscaling.targetCPUUtilizationPercentage= \
    --set-string authz.autoscaling.targetMemoryUtilizationPercentage= \
    --show-only templates/authz/hpa.yaml >/dev/null 2>&1; then
    fail "${chart} HPA renders without a CPU or memory metric"
  fi
done
