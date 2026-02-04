#!/bin/bash
set -euo pipefail

# Full E2E environment setup: Flux + Gitea in Kind cluster
# Usage: ./setup.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "============================================"
echo "Setting up E2E test environment"
echo "============================================"

# Step 1: Install Flux
echo ""
echo "[1/3] Installing Flux..."
"${SCRIPT_DIR}/install-flux.sh"

# Step 2: Install Gitea
echo ""
echo "[2/3] Installing Gitea..."
"${SCRIPT_DIR}/install-gitea.sh"

# Step 3: Seed repository
echo ""
echo "[3/3] Seeding test repository..."
"${SCRIPT_DIR}/seed-repo.sh"

echo ""
echo "============================================"
echo "E2E environment is ready!"
echo "============================================"
echo ""
echo "Gitea URL (in-cluster): http://gitea.gitea.svc.cluster.local:3000"
echo "Flux controllers: source-controller, kustomize-controller, helm-controller"
echo ""
echo "Environment config: ${SCRIPT_DIR}/.env"
