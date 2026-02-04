#!/bin/bash
set -euo pipefail

# Seed test repository in Gitea (in-cluster) for E2E tests
# Usage: ./seed-repo.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="${SCRIPT_DIR}/../manifests"

# Load env
if [ -f "${SCRIPT_DIR}/.env" ]; then
    source "${SCRIPT_DIR}/.env"
fi

: "${GITEA_NAMESPACE:=gitea}"
: "${GITEA_OWNER:=fluxup}"
: "${GITEA_REPO:=flux-test-repo}"
: "${GITEA_PASSWORD:=fluxup123}"

GITEA_POD=$(kubectl -n "${GITEA_NAMESPACE}" get pod -l app=gitea -o jsonpath='{.items[0].metadata.name}')

echo "==> Creating repository ${GITEA_OWNER}/${GITEA_REPO}..."

# Create repo via gitea CLI in the pod (must run as git user)
kubectl -n "${GITEA_NAMESPACE}" exec "${GITEA_POD}" -- \
    su-exec git gitea admin repo create \
    --owner "${GITEA_OWNER}" \
    --name "${GITEA_REPO}" \
    --private false 2>/dev/null || echo "Repository may already exist"

# Create temp directory
TEMP_DIR=$(mktemp -d)
trap "rm -rf ${TEMP_DIR}" EXIT

# Create the repo structure locally
mkdir -p "${TEMP_DIR}/flux/repositories"
mkdir -p "${TEMP_DIR}/flux/apps/gitea"
mkdir -p "${TEMP_DIR}/flux/apps/redis"

# Copy manifests
cp "${MANIFESTS_DIR}/helmrepositories.yaml" "${TEMP_DIR}/flux/repositories/"
cp "${MANIFESTS_DIR}/gitea-helmrelease.yaml" "${TEMP_DIR}/flux/apps/gitea/helmrelease.yaml"
cp "${MANIFESTS_DIR}/redis-helmrelease.yaml" "${TEMP_DIR}/flux/apps/redis/helmrelease.yaml"

# Create kustomization
cat > "${TEMP_DIR}/flux/kustomization.yaml" << 'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - repositories/helmrepositories.yaml
  - apps/gitea/helmrelease.yaml
  - apps/redis/helmrelease.yaml
EOF

echo "==> Copying manifests to Gitea pod..."

# Copy files to pod
kubectl cp "${TEMP_DIR}/flux" "${GITEA_NAMESPACE}/${GITEA_POD}:/tmp/repo-content"

# Initialize and push from inside the pod
echo "==> Initializing and pushing repository..."
kubectl -n "${GITEA_NAMESPACE}" exec "${GITEA_POD}" -- bash -c "
set -e
cd /tmp
rm -rf repo-init
mkdir repo-init && cd repo-init
git init -b main
git config user.email 'fluxup@test.local'
git config user.name 'FluxUp E2E'

# Copy the content
cp -r /tmp/repo-content/* . 2>/dev/null || mkdir -p flux
cp -r /tmp/repo-content/. ./flux/ 2>/dev/null || true

# Create README
echo '# FluxUp E2E Test Repo' > README.md

# Add and commit
git add .
git commit -m 'Add test HelmRelease manifests'

# Push
git remote add origin http://${GITEA_OWNER}:${GITEA_PASSWORD}@localhost:3000/${GITEA_OWNER}/${GITEA_REPO}.git || true
git push -u origin main --force
"

echo ""
echo "==> Repository seeded successfully!"
echo "    Repository: ${GITEA_OWNER}/${GITEA_REPO}"
