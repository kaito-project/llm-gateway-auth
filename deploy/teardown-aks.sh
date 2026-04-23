#!/usr/bin/env bash
# deploy/teardown-aks.sh — Delete all AKS resources created by setup-aks.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=deploy/aks.env
source "${SCRIPT_DIR}/aks.env"

RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo ""
echo -e "${YELLOW}This will permanently delete:${NC}"
echo "  Resource Group:  ${RESOURCE_GROUP}"
echo "  AKS Cluster:     ${CLUSTER_NAME}"
echo "  ACR:             ${ACR_NAME}"
echo "  Location:        ${LOCATION}"
echo ""

read -r -p "Are you sure? [y/N] " confirm
if [[ ! "${confirm}" =~ ^[Yy]$ ]]; then
    echo "Aborted."
    exit 0
fi

echo ""
echo -e "${RED}Deleting resource group: ${RESOURCE_GROUP}...${NC}"
az group delete \
    --name "${RESOURCE_GROUP}" \
    --yes \
    --no-wait

echo ""
echo "Resource group deletion initiated (runs in background)."
echo "Monitor progress:"
echo "  az group show --name ${RESOURCE_GROUP} --query properties.provisioningState"
echo ""
