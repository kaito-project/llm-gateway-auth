#!/usr/bin/env bash
# deploy/setup-aks.sh — Create an AKS cluster with Istio service mesh,
# deploy the ext_authz plugin, BookInfo, and protect access.
#
# Usage:
#   bash deploy/setup-aks.sh
#
# Override any configuration by editing deploy/aks.env or by exporting
# environment variables before running this script.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Source operator-editable configuration.
# shellcheck source=deploy/aks.env
source "${SCRIPT_DIR}/aks.env"

# envsubst only reads exported environment variables.
export AZURE_AD_TENANT_ID
export EXTAUTHZ_NAMESPACE

# Compute the git short SHA for image tagging.
GIT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo "latest")"

# ── Helpers ───────────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

log()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
err()  { echo -e "${RED}[ERROR]${NC} $*" >&2; }

stage() {
    echo ""
    echo -e "${CYAN}══════════════════════════════════════════════════════════════${NC}"
    echo -e "${CYAN}  $*${NC}"
    echo -e "${CYAN}══════════════════════════════════════════════════════════════${NC}"
    echo ""
}

# ── Step 0: Prerequisites ────────────────────────────────────────────────────

check_prerequisites() {
    stage "Checking prerequisites"

    local missing=()
    for cmd in az kubectl; do
        if ! command -v "$cmd" &>/dev/null; then
            missing+=("$cmd")
        fi
    done

    if [[ ${#missing[@]} -gt 0 ]]; then
        err "Missing required tools: ${missing[*]}"
        err "Install them before running this script."
        exit 1
    fi

    # Verify az login.
    if ! az account show &>/dev/null; then
        err "Not logged into Azure. Run 'az login' first."
        exit 1
    fi

    log "Azure CLI logged in as: $(az account show --query user.name -o tsv)"
    log "Subscription: $(az account show --query name -o tsv)"
    log "Azure AD Tenant: ${AZURE_AD_TENANT_ID}"
}

# ── Step 1: Resource Group ───────────────────────────────────────────────────

create_resource_group() {
    stage "Creating resource group: ${RESOURCE_GROUP} in ${LOCATION}"

    az group create \
        --name "${RESOURCE_GROUP}" \
        --location "${LOCATION}" \
        --output none

    log "Resource group ready."
}

# ── Step 2: Azure Container Registry ────────────────────────────────────────

create_acr() {
    stage "Creating Azure Container Registry: ${ACR_NAME}"

    if az acr show --name "${ACR_NAME}" &>/dev/null; then
        log "ACR '${ACR_NAME}' already exists, skipping creation."
    else
        az acr create \
            --resource-group "${RESOURCE_GROUP}" \
            --name "${ACR_NAME}" \
            --sku Standard \
            --location "${LOCATION}" \
            --admin-enabled false \
            --output none
    fi

    ACR_LOGIN_SERVER="$(az acr show --name "${ACR_NAME}" --query loginServer -o tsv)"
    log "ACR login server: ${ACR_LOGIN_SERVER}"
}

# ── Step 3: AKS Cluster ─────────────────────────────────────────────────────

create_aks_cluster() {
    stage "Creating AKS cluster: ${CLUSTER_NAME}"

    if az aks show --resource-group "${RESOURCE_GROUP}" --name "${CLUSTER_NAME}" &>/dev/null; then
        log "AKS cluster '${CLUSTER_NAME}' already exists, skipping creation."
    else
        log "This takes ~10 minutes..."
        az aks create \
            --resource-group "${RESOURCE_GROUP}" \
            --name "${CLUSTER_NAME}" \
            --location "${LOCATION}" \
            --node-count "${NODE_COUNT}" \
            --node-vm-size "${NODE_VM_SIZE}" \
            --enable-asm \
            --network-plugin azure \
            --network-policy cilium \
            --network-dataplane "${NETWORK_DATAPLANE}" \
            --enable-managed-identity \
            --attach-acr "${ACR_NAME}" \
            --generate-ssh-keys \
            --output none
    fi

    log "AKS cluster ready."
}

# ── Step 4: Get Credentials ─────────────────────────────────────────────────

get_credentials() {
    stage "Getting AKS credentials"

    az aks get-credentials \
        --resource-group "${RESOURCE_GROUP}" \
        --name "${CLUSTER_NAME}" \
        --overwrite-existing

    log "kubectl context set. Nodes:"
    kubectl get nodes -o wide
}

# ── Step 5: Enable External Ingress Gateway ──────────────────────────────────

enable_ingress_gateway() {
    stage "Enabling external ingress gateway"

    if kubectl get svc aks-istio-ingressgateway-external -n aks-istio-ingress &>/dev/null; then
        log "External ingress gateway already enabled, skipping."
    else
        az aks mesh enable-ingress-gateway \
            --resource-group "${RESOURCE_GROUP}" \
            --name "${CLUSTER_NAME}" \
            --ingress-gateway-type external
    fi

    log "Waiting for ingress gateway pods..."
    kubectl wait --for=condition=ready pod \
        -l app=aks-istio-ingressgateway-external \
        -n aks-istio-ingress \
        --timeout=300s

    log "External ingress gateway is ready."
    kubectl get svc -n aks-istio-ingress aks-istio-ingressgateway-external
}

# ── Step 6: Build and Push Image ────────────────────────────────────────────

build_and_push_image() {
    stage "Building and pushing ext_authz image via ACR Build"

    ACR_LOGIN_SERVER="$(az acr show --name "${ACR_NAME}" --query loginServer -o tsv)"
    IMG="${ACR_LOGIN_SERVER}/llm-gateway-auth/extauthz:${GIT_SHA}"

    az acr build \
        --registry "${ACR_NAME}" \
        --image "llm-gateway-auth/extauthz:${GIT_SHA}" \
        --file "${REPO_ROOT}/Dockerfile.azure-authz" \
        "${REPO_ROOT}"

    log "Image pushed: ${IMG}"
    export IMG
}

# ── Step 7: Configure MeshConfig ────────────────────────────────────────────

configure_meshconfig() {
    stage "Configuring Istio MeshConfig with ext_authz extensionProvider"

    # AKS Istio add-on blocks edits to the default ConfigMap (istio-asm-1-28).
    # Instead, we create a shared ConfigMap that istiod merges with the default.
    # See: https://learn.microsoft.com/azure/aks/istio-meshconfig
    local shared_cm="istio-shared-configmap-${ASM_REVISION}"
    local cm_namespace="aks-istio-system"

    log "Creating shared ConfigMap: ${shared_cm} in ${cm_namespace}"

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${shared_cm}
  namespace: ${cm_namespace}
data:
  mesh: |-
    extensionProviders:
    - name: llm-gateway-ext-authz
      envoyExtAuthzGrpc:
        service: extauthz.llm-gateway.svc.cluster.local
        port: "9001"
EOF

    log "Shared ConfigMap created. Waiting for istiod to pick up the change..."
    # istiod watches for ConfigMap changes and automatically reloads.
    # Give it a moment to reconcile.
    sleep 5

    log "MeshConfig updated with llm-gateway-ext-authz extensionProvider."
}

# ── Step 8: Deploy ext_authz Server ─────────────────────────────────────────

deploy_extauthz() {
    stage "Deploying ext_authz server to namespace: ${EXTAUTHZ_NAMESPACE}"

    # Create namespace with Istio sidecar injection label.
    kubectl create namespace "${EXTAUTHZ_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    kubectl label namespace "${EXTAUTHZ_NAMESPACE}" \
        "istio.io/rev=${ASM_REVISION}" --overwrite

    # Apply RBAC manifests.
    kubectl apply -f "${REPO_ROOT}/config/rbac/service_account.yaml"
    kubectl apply -f "${REPO_ROOT}/config/rbac/clusterrole.yaml"
    kubectl apply -f "${REPO_ROOT}/config/rbac/clusterrolebinding.yaml"

    # Apply Deployment + Service with image substitution.
    envsubst '${IMG}' < "${REPO_ROOT}/config/deployment/extauthz.yaml" | kubectl apply -f -

    # Apply PeerAuthentication for the ext_authz namespace.
    kubectl apply -f "${REPO_ROOT}/config/istio/peer-authentication.yaml"

    log "Waiting for ext_authz pods..."
    kubectl rollout status deployment/extauthz \
        -n "${EXTAUTHZ_NAMESPACE}" --timeout=180s

    log "ext_authz server is running."
    kubectl get pods -n "${EXTAUTHZ_NAMESPACE}"
}

# ── Step 9: Deploy BookInfo ──────────────────────────────────────────────────

deploy_bookinfo() {
    stage "Deploying BookInfo to namespace: ${BOOKINFO_NAMESPACE}"

    # Create namespace with Istio sidecar injection label.
    kubectl create namespace "${BOOKINFO_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    kubectl label namespace "${BOOKINFO_NAMESPACE}" \
        "istio.io/rev=${ASM_REVISION}" --overwrite

    # Deploy BookInfo from the official Istio samples.
    local bookinfo_url="https://raw.githubusercontent.com/istio/istio/release-${ISTIO_VERSION%.*}/samples/bookinfo/platform/kube/bookinfo.yaml"
    log "Applying BookInfo from: ${bookinfo_url}"
    kubectl apply -n "${BOOKINFO_NAMESPACE}" -f "${bookinfo_url}"

    log "Waiting for BookInfo pods..."
    kubectl rollout status deployment/productpage-v1 \
        -n "${BOOKINFO_NAMESPACE}" --timeout=300s
    kubectl rollout status deployment/details-v1 \
        -n "${BOOKINFO_NAMESPACE}" --timeout=120s
    kubectl rollout status deployment/ratings-v1 \
        -n "${BOOKINFO_NAMESPACE}" --timeout=120s
    kubectl rollout status deployment/reviews-v1 \
        -n "${BOOKINFO_NAMESPACE}" --timeout=120s

    log "BookInfo is running."
    kubectl get pods -n "${BOOKINFO_NAMESPACE}"
}

# ── Step 10: Configure BookInfo Gateway ──────────────────────────────────────

configure_bookinfo_gateway() {
    stage "Configuring BookInfo Gateway and VirtualService"

    kubectl apply -f "${REPO_ROOT}/config/istio/bookinfo-gateway.yaml"

    log "Gateway and VirtualService applied."
}

# ── Step 11: Apply Istio Policies ────────────────────────────────────────────

apply_istio_policies() {
    stage "Applying Istio authorization policies"

    # Apply RequestAuthentication for Azure AD JWT validation on the gateway.
    log "Applying RequestAuthentication for Azure AD tenant: ${AZURE_AD_TENANT_ID}"
    envsubst '${AZURE_AD_TENANT_ID}' \
        < "${REPO_ROOT}/config/istio/request-authentication.yaml" \
        | kubectl apply -f -

    # Apply CUSTOM AuthorizationPolicy on the gateway.
    # kubectl apply -f "${REPO_ROOT}/config/istio/authorization-policy.yaml"

    # Apply EnvoyFilter to reorder jwt_authn before ext_authz on the gateway.
    # Istio places ext_authz before jwt_authn by default, which means claims
    # aren't available when ext_authz fires. This fixes the ordering.
    envsubst '${EXTAUTHZ_NAMESPACE} ${AZURE_AD_TENANT_ID}' \
        < "${REPO_ROOT}/config/istio/envoy-filter-reorder.yaml" \
        | kubectl apply -f -

    log "Istio policies applied."
}

# ── Step 12: Print Summary ───────────────────────────────────────────────────

print_summary() {
    stage "Deployment Complete"

    local ingress_ip
    ingress_ip="$(kubectl get svc aks-istio-ingressgateway-external \
        -n aks-istio-ingress \
        -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo '<pending>')"

    echo ""
    echo "═══════════════════════════════════════════════════════════"
    echo "  AKS Cluster:     ${CLUSTER_NAME}"
    echo "  Resource Group:  ${RESOURCE_GROUP}"
    echo "  Location:        ${LOCATION}"
    echo "  Azure AD Tenant: ${AZURE_AD_TENANT_ID}"
    echo "  ext_authz Image: ${IMG}"
    echo ""
    echo "  BookInfo URL:    http://${ingress_ip}/productpage"
    echo ""
    echo "  Test with a JWT:"
    echo "    TOKEN=\$(az account get-access-token --query accessToken -o tsv)"
    echo "    curl -H \"Authorization: Bearer \$TOKEN\" \\"
    echo "      http://${ingress_ip}/productpage"
    echo ""
    echo "  Teardown: bash deploy/teardown-aks.sh"
    echo "═══════════════════════════════════════════════════════════"
    echo ""
}

# ── Main ─────────────────────────────────────────────────────────────────────

main() {
    check_prerequisites
    create_resource_group
    create_acr
    create_aks_cluster
    get_credentials
    enable_ingress_gateway
    build_and_push_image
#    configure_meshconfig
    deploy_extauthz
    deploy_bookinfo
    configure_bookinfo_gateway
    apply_istio_policies
    print_summary
}

main "$@"
