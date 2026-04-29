#!/usr/bin/env bash
# hack/setup-test-env.sh
#
# Sets up a local or CI kind cluster for e2e testing.
# Creates the kind cluster, optionally installs Istio, builds and loads
# the operator and authz images, and deploys them via Helm.
# Idempotent: safe to re-run; existing cluster/install steps are skipped.
#
# Usage:
#   ./hack/setup-test-env.sh [--skip-istio] [--reinstall-istio]
#
# Environment variables (all optional):
#   KIND_CLUSTER_NAME   default: llm-gateway-test
#   KIND_VERSION        default: from versions.env
#   ISTIO_VERSION       default: from versions.env
#   NAMESPACE           default: llm-gateway
#   SKIP_ISTIO          set to "true" to create cluster only (no Istio)
#   REINSTALL_ISTIO     set to "true" to force istio reinstall
#   KIND_CONFIG         path to kind config yaml
#   ISTIOCTL_INSTALL_DIR  where to install binaries (default: ~/.local/bin)
#   CONTAINER_RUNTIME   "docker" or "podman" (auto-detected if unset)
#   OPERATOR_IMG        operator image name (default: apikey-operator:latest)
#   AUTHZ_IMG           authz image name (default: apikey-authz:latest)
#   RELEASE_NAME        helm release name (default: llm-gateway-apikey)
#   CHART_DIR           helm chart path (default: chart/llm-gateway-apikey)

set -euo pipefail

# ─── Configuration ────────────────────────────────────────────────────────────
# Versions are loaded from versions.env (single source of truth).
# Environment variables take precedence over values in the file.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Save any caller-supplied overrides before sourcing the file.
_KIND_VERSION="${KIND_VERSION:-}"
_ISTIO_VERSION="${ISTIO_VERSION:-}"

# Source pinned versions.
# shellcheck source=../versions.env
if [[ -f "${REPO_ROOT}/versions.env" ]]; then
  # shellcheck disable=SC1091
  source "${REPO_ROOT}/versions.env"
else
  echo "WARNING: versions.env not found at ${REPO_ROOT}/versions.env" >&2
fi

# Restore caller overrides (env vars beat the file).
KIND_VERSION="${_KIND_VERSION:-${KIND_VERSION}}"
ISTIO_VERSION="${_ISTIO_VERSION:-${ISTIO_VERSION}}"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-llm-gateway-test}"
NAMESPACE="${NAMESPACE:-llm-gateway}"
KIND_CONFIG="${KIND_CONFIG:-${SCRIPT_DIR}/kind-config.yaml}"
SKIP_ISTIO="${SKIP_ISTIO:-false}"
REINSTALL_ISTIO="${REINSTALL_ISTIO:-false}"
ISTIOCTL_INSTALL_DIR="${ISTIOCTL_INSTALL_DIR:-${HOME}/.local/bin}"

# Image and Helm configuration.
OPERATOR_IMG="${OPERATOR_IMG:-apikey-operator:latest}"
AUTHZ_IMG="${AUTHZ_IMG:-apikey-authz:latest}"
RELEASE_NAME="${RELEASE_NAME:-llm-gateway-apikey}"
CHART_DIR="${CHART_DIR:-${REPO_ROOT}/chart/llm-gateway-apikey}"

# Detect OS/arch for binary downloads
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "${ARCH}" in
  x86_64)        ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)             echo "ERROR: Unsupported architecture: ${ARCH}"; exit 1 ;;
esac

# Detect container runtime: docker or podman.
# If CONTAINER_RUNTIME is set, use that. Otherwise auto-detect.
if [[ -z "${CONTAINER_RUNTIME:-}" ]]; then
  if command -v docker &>/dev/null; then
    CONTAINER_RUNTIME="docker"
  elif command -v podman &>/dev/null; then
    CONTAINER_RUNTIME="podman"
  else
    CONTAINER_RUNTIME=""  # will fail in check_prerequisites
  fi
fi

# kind uses KIND_EXPERIMENTAL_PROVIDER to select the container runtime.
# For podman, this must be exported before any kind commands.
if [[ "${CONTAINER_RUNTIME}" == "podman" ]]; then
  export KIND_EXPERIMENTAL_PROVIDER="podman"
fi

# ─── Logging helpers ──────────────────────────────────────────────────────────

_stage_start=""

log()  { echo "▶ $*"; }
ok()   { echo "✓ $*"; }
warn() { echo "⚠ $*" >&2; }
die()  { echo "✗ ERROR: $*" >&2; exit 1; }

# Stage logging with elapsed time tracking.
stage_begin() {
  _stage_start="$(date +%s)"
  echo ""
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo "▶ STAGE: $*"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
}

stage_end() {
  local elapsed=$(( $(date +%s) - _stage_start ))
  echo "✓ DONE: $* (${elapsed}s)"
  echo ""
}

# ─── Prerequisite checks ─────────────────────────────────────────────────────

install_kind() {
  if [[ "${OS}" == "darwin" ]]; then
    # On macOS, use Homebrew to avoid Gatekeeper blocking unsigned binaries.
    if ! command -v brew &>/dev/null; then
      die "Homebrew not found. Install kind via: brew install kind"
    fi
    log "Installing kind via Homebrew..."
    brew install kind
    ok "kind installed via Homebrew."
  else
    # On Linux (CI), download the binary directly.
    mkdir -p "${ISTIOCTL_INSTALL_DIR}"
    local url="https://github.com/kubernetes-sigs/kind/releases/download/v${KIND_VERSION}/kind-${OS}-${ARCH}"
    log "Downloading kind v${KIND_VERSION} from ${url}"
    curl -sSL "${url}" -o "${ISTIOCTL_INSTALL_DIR}/kind"
    chmod +x "${ISTIOCTL_INSTALL_DIR}/kind"
    export PATH="${ISTIOCTL_INSTALL_DIR}:${PATH}"
    ok "kind v${KIND_VERSION} installed to ${ISTIOCTL_INSTALL_DIR}/kind"
  fi
}

install_istioctl() {
  if [[ "${OS}" == "darwin" ]]; then
    # On macOS, use Homebrew to avoid Gatekeeper blocking unsigned binaries.
    if ! command -v brew &>/dev/null; then
      die "Homebrew not found. Install istioctl via: brew install istioctl"
    fi
    log "Installing istioctl via Homebrew..."
    brew install istioctl
    ok "istioctl installed via Homebrew."
  else
    # On Linux (CI), download directly for pinned version control.
    mkdir -p "${ISTIOCTL_INSTALL_DIR}"
    log "Downloading istioctl v${ISTIO_VERSION}"
    local tmpdir
    tmpdir="$(mktemp -d)"
    pushd "${tmpdir}" > /dev/null
    curl -sSL https://istio.io/downloadIstio | \
      ISTIO_VERSION="${ISTIO_VERSION}" TARGET_ARCH="${ARCH}" sh - 2>&1 | tail -5
    local istio_bin="${tmpdir}/istio-${ISTIO_VERSION}/bin/istioctl"
    if [[ -f "${istio_bin}" ]]; then
      cp "${istio_bin}" "${ISTIOCTL_INSTALL_DIR}/istioctl"
      chmod +x "${ISTIOCTL_INSTALL_DIR}/istioctl"
    else
      popd > /dev/null
      rm -rf "${tmpdir}"
      die "istioctl binary not found after download"
    fi
    popd > /dev/null
    rm -rf "${tmpdir}"
    export PATH="${ISTIOCTL_INSTALL_DIR}:${PATH}"
    ok "istioctl v${ISTIO_VERSION} installed to ${ISTIOCTL_INSTALL_DIR}/istioctl"
  fi
}

check_prerequisites() {
  stage_begin "Checking prerequisites"

  # Container runtime -- docker or podman
  log "Checking container runtime..."
  if [[ -z "${CONTAINER_RUNTIME}" ]]; then
    die "No container runtime found. Install Docker or Podman."
  fi
  if ! command -v "${CONTAINER_RUNTIME}" &>/dev/null; then
    die "${CONTAINER_RUNTIME} not found in PATH."
  fi
  if ! "${CONTAINER_RUNTIME}" info &>/dev/null; then
    if [[ "${CONTAINER_RUNTIME}" == "podman" ]]; then
      die "Podman machine is not running. Start it with: podman machine start"
    else
      die "Docker daemon is not running."
    fi
  fi
  ok "Container runtime: ${CONTAINER_RUNTIME}"

  # Podman machine memory check -- kind + Istio needs at least 4GB.
  # The default Podman machine has only 2GB which causes API server timeouts.
  if [[ "${CONTAINER_RUNTIME}" == "podman" ]]; then
    log "Checking Podman machine memory..."
    local podman_mem_mb
    podman_mem_mb="$(podman machine inspect --format '{{.Resources.Memory}}' 2>/dev/null || echo 0)"
    if [[ "${podman_mem_mb}" -gt 0 ]] && [[ "${podman_mem_mb}" -lt 4096 ]]; then
      die "Podman machine has ${podman_mem_mb}MB RAM. kind + Istio requires at least 4096MB.
  Resize with:
    podman machine stop
    podman machine set --memory 4096
    podman machine start"
    elif [[ "${podman_mem_mb}" -ge 4096 ]]; then
      ok "Podman machine memory: ${podman_mem_mb}MB"
    fi
  fi

  # kubectl -- hard requirement
  log "Checking kubectl..."
  if ! command -v kubectl &>/dev/null; then
    die "kubectl not found. Install: https://kubernetes.io/docs/tasks/tools/"
  fi
  ok "kubectl found"

  # kind -- auto-install if missing
  log "Checking kind..."
  if ! command -v kind &>/dev/null; then
    warn "kind not found -- installing v${KIND_VERSION} to ${ISTIOCTL_INSTALL_DIR}"
    install_kind
  else
    local installed_version
    installed_version="$(kind version | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
    if [[ "${installed_version}" != "${KIND_VERSION}" ]]; then
      warn "kind version mismatch: have v${installed_version}, want v${KIND_VERSION}"
    fi
    ok "kind v${installed_version}"
  fi

  # istioctl -- auto-install if missing (unless SKIP_ISTIO)
  if [[ "${SKIP_ISTIO}" != "true" ]]; then
    log "Checking istioctl..."
    if ! command -v istioctl &>/dev/null; then
      warn "istioctl not found -- installing v${ISTIO_VERSION} to ${ISTIOCTL_INSTALL_DIR}"
      install_istioctl
    else
      # Use --remote=false to only check the local client version.
      # Without it, istioctl tries to reach the cluster control plane which
      # may not exist yet and will hang indefinitely.
      local istio_installed
      istio_installed="$(istioctl version --short --remote=false 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || echo 'unknown')"
      if [[ "${istio_installed}" != "${ISTIO_VERSION}" ]]; then
        warn "istioctl version mismatch: have v${istio_installed}, want v${ISTIO_VERSION}"
      fi
      ok "istioctl v${istio_installed}"
    fi
  fi

  # helm -- required to deploy the operator and authz
  log "Checking helm..."
  if ! command -v helm &>/dev/null; then
    die "helm not found. Install: https://helm.sh/docs/intro/install/"
  fi
  ok "helm found"

  stage_end "Prerequisites satisfied"
}

# ─── Kind cluster lifecycle ──────────────────────────────────────────────────

create_cluster() {
  stage_begin "Creating kind cluster"

  if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
    ok "Cluster '${KIND_CLUSTER_NAME}' already exists -- skipping creation."
    stage_end "Cluster already exists"
    return 0
  fi

  log "Creating kind cluster '${KIND_CLUSTER_NAME}' (image: ${KIND_NODE_IMAGE})..."
  kind create cluster \
    --name "${KIND_CLUSTER_NAME}" \
    --config "${KIND_CONFIG}" \
    --image "${KIND_NODE_IMAGE}" \
    --wait 120s

  stage_end "Cluster '${KIND_CLUSTER_NAME}' created"
}

set_kubecontext() {
  log "Setting kubectl context to kind-${KIND_CLUSTER_NAME}..."
  kubectl config use-context "kind-${KIND_CLUSTER_NAME}"
  ok "kubectl context set to kind-${KIND_CLUSTER_NAME}."
}

# ─── Istio installation ──────────────────────────────────────────────────────
# Uses istioctl with the 'minimal' profile:
#   - istiod only (no ingress/egress gateways)
#   - No CNI plugin (critical for kind -- races with kindnet)
#   - Reduced resource requests to fit kind + free GA runner
#   - holdApplicationUntilProxyStarts for test determinism

install_istio() {
  if [[ "${SKIP_ISTIO}" == "true" ]]; then
    log "SKIP_ISTIO=true -- skipping Istio installation."
    return 0
  fi

  stage_begin "Installing Istio"

  if kubectl get namespace istio-system &>/dev/null && \
     kubectl get deployment istiod -n istio-system &>/dev/null && \
     [[ "${REINSTALL_ISTIO}" != "true" ]]; then
    ok "Istio is already installed -- skipping. (Set REINSTALL_ISTIO=true to force.)"
    stage_end "Istio already installed"
    return 0
  fi

  log "Installing Istio v${ISTIO_VERSION} (minimal profile)..."

  istioctl install \
    --context "kind-${KIND_CLUSTER_NAME}" \
    --set profile=minimal \
    --set values.pilot.resources.requests.cpu=100m \
    --set values.pilot.resources.requests.memory=256Mi \
    --set values.pilot.resources.limits.cpu=500m \
    --set values.pilot.resources.limits.memory=512Mi \
    --set components.cni.enabled=false \
    --set components.ingressGateways[0].name=istio-ingressgateway \
    --set components.ingressGateways[0].enabled=true \
    --set values.gateways.istio-ingressgateway.resources.requests.cpu=50m \
    --set values.gateways.istio-ingressgateway.resources.requests.memory=64Mi \
    --set values.gateways.istio-ingressgateway.resources.limits.cpu=200m \
    --set values.gateways.istio-ingressgateway.resources.limits.memory=256Mi \
    --set values.global.proxy.resources.requests.cpu=50m \
    --set values.global.proxy.resources.requests.memory=64Mi \
    --set values.global.proxy.resources.limits.cpu=200m \
    --set values.global.proxy.resources.limits.memory=256Mi \
    --set meshConfig.defaultConfig.holdApplicationUntilProxyStarts=true \
    --skip-confirmation

  stage_end "Istio installed"
}

wait_for_istio() {
  if [[ "${SKIP_ISTIO}" == "true" ]]; then return 0; fi

  stage_begin "Waiting for istiod and ingress gateway"

  kubectl rollout status deployment/istiod \
    -n istio-system \
    --timeout=300s

  kubectl rollout status deployment/istio-ingressgateway \
    -n istio-system \
    --timeout=300s

  stage_end "istiod and ingress gateway are ready"
}

create_app_namespace() {
  stage_begin "Creating application namespace"

  log "Creating namespace '${NAMESPACE}'..."
  kubectl create namespace "${NAMESPACE}" \
    --dry-run=client -o yaml | kubectl apply -f -

  if [[ "${SKIP_ISTIO}" != "true" ]]; then
    log "Labelling namespace '${NAMESPACE}' with Istio sidecar injection..."
    kubectl label namespace "${NAMESPACE}" \
      istio-injection=enabled \
      --overwrite
  else
    # Remove the label if it was set by a previous run with Istio enabled.
    # This prevents unwanted sidecar injection when running without Istio.
    log "Ensuring Istio sidecar injection is disabled on namespace '${NAMESPACE}'..."
    kubectl label namespace "${NAMESPACE}" \
      istio-injection- \
      --overwrite 2>/dev/null || true
  fi

  stage_end "Namespace '${NAMESPACE}' ready"
}

# ─── Smoke tests ──────────────────────────────────────────────────────────────

# ─── Build, load images, and deploy via Helm ─────────────────────────────────

build_and_load_images() {
  stage_begin "Building and loading images into kind"

  log "Building operator image: ${OPERATOR_IMG}..."
  ${CONTAINER_RUNTIME} build \
    -f "${REPO_ROOT}/Dockerfile.apikey-operator" \
    -t "${OPERATOR_IMG}" \
    "${REPO_ROOT}"
  ok "Operator image built: ${OPERATOR_IMG}"

  log "Building authz image: ${AUTHZ_IMG}..."
  ${CONTAINER_RUNTIME} build \
    -f "${REPO_ROOT}/Dockerfile.apikey-authz" \
    -t "${AUTHZ_IMG}" \
    "${REPO_ROOT}"
  ok "Authz image built: ${AUTHZ_IMG}"

  log "Loading images into kind cluster '${KIND_CLUSTER_NAME}'..."
  if [[ "${CONTAINER_RUNTIME}" == "podman" ]]; then
    podman save "${OPERATOR_IMG}" -o /tmp/kind-operator.tar
    kind load image-archive /tmp/kind-operator.tar --name "${KIND_CLUSTER_NAME}"
    rm -f /tmp/kind-operator.tar

    podman save "${AUTHZ_IMG}" -o /tmp/kind-authz.tar
    kind load image-archive /tmp/kind-authz.tar --name "${KIND_CLUSTER_NAME}"
    rm -f /tmp/kind-authz.tar
  else
    kind load docker-image "${OPERATOR_IMG}" --name "${KIND_CLUSTER_NAME}"
    kind load docker-image "${AUTHZ_IMG}" --name "${KIND_CLUSTER_NAME}"
  fi
  ok "Images loaded into kind."

  # Podman prefixes images with "localhost/" and kind preserves that name
  # inside the node. Resolve the actual image references so Helm uses the
  # name the kubelet will find.
  OPERATOR_IMG_REPO="${OPERATOR_IMG%%:*}"
  OPERATOR_IMG_TAG="${OPERATOR_IMG#*:}"
  AUTHZ_IMG_REPO="${AUTHZ_IMG%%:*}"
  AUTHZ_IMG_TAG="${AUTHZ_IMG#*:}"
  if [[ "${CONTAINER_RUNTIME}" == "podman" ]]; then
    # podman stores images as localhost/<name>; kind keeps that prefix.
    [[ "${OPERATOR_IMG_REPO}" != localhost/* ]] && OPERATOR_IMG_REPO="localhost/${OPERATOR_IMG_REPO}"
    [[ "${AUTHZ_IMG_REPO}" != localhost/* ]] && AUTHZ_IMG_REPO="localhost/${AUTHZ_IMG_REPO}"
  fi
  log "Resolved operator image: ${OPERATOR_IMG_REPO}:${OPERATOR_IMG_TAG}"
  log "Resolved authz image:    ${AUTHZ_IMG_REPO}:${AUTHZ_IMG_TAG}"

  stage_end "Images built and loaded"
}

helm_install() {
  stage_begin "Deploying via Helm"

  # Install the CRD separately so Helm does not try to adopt a pre-existing one
  # (envtest or a previous kubectl apply may have already created it).
  log "Applying CRD..."
  kubectl apply -f "${REPO_ROOT}/config/crd/bases/"
  ok "CRD applied."

  # If the release already exists, upgrade it; otherwise install.
  if helm status "${RELEASE_NAME}" -n "${NAMESPACE}" &>/dev/null; then
    log "Helm release '${RELEASE_NAME}' exists -- upgrading..."
    helm upgrade "${RELEASE_NAME}" "${CHART_DIR}" \
      --namespace "${NAMESPACE}" \
      --set operator.image.repository="${OPERATOR_IMG_REPO}" \
      --set operator.image.tag="${OPERATOR_IMG_TAG}" \
      --set authz.image.repository="${AUTHZ_IMG_REPO}" \
      --set authz.image.tag="${AUTHZ_IMG_TAG}" \
      --set global.imagePullPolicy=Never \
      --set istio.enabled=true \
      --set istio.gatewayNamespace=istio-system \
      --set istio.gatewaySelector.istio=ingressgateway \
      --set istio.meshConfigConfigMap.patch=true \
      --set operator.webhook.enabled=false \
      --set crds.install=false \
      --set operator.replicaCount=1 \
      --set authz.replicaCount=1 \
      --wait --timeout 120s
    ok "Helm release upgraded."
  else
    log "Installing Helm release '${RELEASE_NAME}'..."
    helm install "${RELEASE_NAME}" "${CHART_DIR}" \
      --namespace "${NAMESPACE}" --create-namespace \
      --set operator.image.repository="${OPERATOR_IMG_REPO}" \
      --set operator.image.tag="${OPERATOR_IMG_TAG}" \
      --set authz.image.repository="${AUTHZ_IMG_REPO}" \
      --set authz.image.tag="${AUTHZ_IMG_TAG}" \
      --set global.imagePullPolicy=Never \
      --set istio.enabled=true \
      --set istio.gatewayNamespace=istio-system \
      --set istio.gatewaySelector.istio=ingressgateway \
      --set istio.meshConfigConfigMap.patch=true \
      --set operator.webhook.enabled=false \
      --set crds.install=false \
      --set operator.replicaCount=1 \
      --set authz.replicaCount=1 \
      --wait --timeout 120s
    ok "Helm release installed."
  fi

  stage_end "Helm deployment complete"
}

wait_for_deployments() {
  stage_begin "Waiting for operator and authz deployments"

  log "Waiting for operator deployment..."
  kubectl rollout status deployment/apikey-operator \
    -n "${NAMESPACE}" --timeout=120s
  ok "Operator deployment is ready."

  log "Waiting for authz deployment..."
  kubectl rollout status deployment/apikey-authz \
    -n "${NAMESPACE}" --timeout=120s
  ok "Authz deployment is ready."

  stage_end "All deployments ready"
}

# ─── Smoke tests ──────────────────────────────────────────────────────────────

run_smoke_tests() {
  stage_begin "Running smoke tests"

  # Check nodes are Ready
  log "Checking node status..."
  local not_ready
  not_ready="$(kubectl get nodes --no-headers | grep -cv " Ready" || true)"
  if [[ "${not_ready}" -gt 0 ]]; then
    die "${not_ready} node(s) not in Ready state. Run: kubectl get nodes"
  fi
  ok "All nodes are Ready."

  if [[ "${SKIP_ISTIO}" != "true" ]]; then
    # Check istiod pod is Running
    log "Checking istiod pod status..."
    local istiod_status
    istiod_status="$(kubectl get pods -n istio-system \
      -l app=istiod --no-headers \
      | awk '{print $3}' | head -1)"
    if [[ "${istiod_status}" != "Running" ]]; then
      die "istiod pod is not Running (status: ${istiod_status}). Run: kubectl get pods -n istio-system"
    fi
    ok "istiod is Running."

    # Check ingress gateway pod is Running
    log "Checking istio-ingressgateway pod status..."
    local gw_status
    gw_status="$(kubectl get pods -n istio-system \
      -l app=istio-ingressgateway --no-headers \
      | awk '{print $3}' | head -1)"
    if [[ "${gw_status}" != "Running" ]]; then
      die "istio-ingressgateway pod is not Running (status: ${gw_status}). Run: kubectl get pods -n istio-system"
    fi
    ok "istio-ingressgateway is Running."

    # Verify istio-injection label
    log "Checking namespace labels..."
    local label_val
    label_val="$(kubectl get namespace "${NAMESPACE}" \
      -o jsonpath='{.metadata.labels.istio-injection}' 2>/dev/null || echo '')"
    if [[ "${label_val}" != "enabled" ]]; then
      die "Namespace '${NAMESPACE}' missing istio-injection=enabled label."
    fi
    ok "Namespace '${NAMESPACE}' has istio-injection=enabled."
  fi

  # Check operator pod is Running
  log "Checking operator pod status..."
  local operator_status
  operator_status="$(kubectl get pods -n "${NAMESPACE}" \
    -l app.kubernetes.io/component=apikey-operator --no-headers \
    | awk '{print $3}' | head -1)"
  if [[ "${operator_status}" != "Running" ]]; then
    die "Operator pod is not Running (status: ${operator_status}). Run: kubectl get pods -n ${NAMESPACE}"
  fi
  ok "Operator pod is Running."

  # Check authz pod is Running
  log "Checking authz pod status..."
  local authz_status
  authz_status="$(kubectl get pods -n "${NAMESPACE}" \
    -l app.kubernetes.io/component=apikey-authz --no-headers \
    | awk '{print $3}' | head -1)"
  if [[ "${authz_status}" != "Running" ]]; then
    die "Authz pod is not Running (status: ${authz_status}). Run: kubectl get pods -n ${NAMESPACE}"
  fi
  ok "Authz pod is Running."

  # Verify the APIKey CRD is installed
  log "Checking APIKey CRD..."
  if ! kubectl get crd apikeys.kaito.sh &>/dev/null; then
    die "CRD apikeys.kaito.sh is not installed."
  fi
  ok "CRD apikeys.kaito.sh is installed."

  stage_end "All smoke tests passed"
}

# ─── Main ─────────────────────────────────────────────────────────────────────

main() {
  local total_start
  total_start="$(date +%s)"

  echo ""
  echo "╔══════════════════════════════════════════════════╗"
  echo "║   llm-gateway-auth test environment setup       ║"
  echo "╚══════════════════════════════════════════════════╝"
  echo ""
  log "  Cluster   : ${KIND_CLUSTER_NAME}"
  log "  Runtime   : ${CONTAINER_RUNTIME}"
  log "  kind      : v${KIND_VERSION}"
  log "  Istio     : v${ISTIO_VERSION}"
  log "  Node image: ${KIND_NODE_IMAGE}"
  log "  OS/arch   : ${OS}/${ARCH}"
  log "  Operator  : ${OPERATOR_IMG}"
  log "  Authz     : ${AUTHZ_IMG}"
  log "  Helm chart: ${CHART_DIR}"

  check_prerequisites
  create_cluster
  set_kubecontext
  install_istio
  wait_for_istio
  create_app_namespace
  build_and_load_images
  helm_install
  wait_for_deployments
  run_smoke_tests

  local total_elapsed=$(( $(date +%s) - total_start ))
  echo ""
  echo "╔══════════════════════════════════════════════════╗"
  echo "║   Environment ready (${total_elapsed}s total)                  ║"
  echo "╚══════════════════════════════════════════════════╝"
  echo ""
  echo "  kubectl config use-context kind-${KIND_CLUSTER_NAME}"
  echo "  kubectl get pods -n ${NAMESPACE}"
  echo "  make e2e-test"
  echo ""
}

# Parse flags
for arg in "$@"; do
  case "${arg}" in
    --skip-istio)      SKIP_ISTIO=true ;;
    --reinstall-istio) REINSTALL_ISTIO=true ;;
    --help|-h)
      sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) warn "Unknown argument: ${arg}" ;;
  esac
done

main
