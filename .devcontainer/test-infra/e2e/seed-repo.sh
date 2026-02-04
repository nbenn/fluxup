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

# Create temp directory for cleanup
TEMP_DIR=$(mktemp -d)
PF_PID=""

cleanup() {
    if [ -n "${PF_PID}" ]; then
        kill "${PF_PID}" 2>/dev/null || true
    fi
    rm -rf "${TEMP_DIR}"
}
trap cleanup EXIT

echo "==> Creating repository ${GITEA_OWNER}/${GITEA_REPO}..."

# Set up port-forward to access the Gitea API and git
kubectl -n "${GITEA_NAMESPACE}" port-forward svc/gitea 3000:3000 &
PF_PID=$!
sleep 3

# Create repository via API (gitea admin doesn't have repo create command)
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -u "${GITEA_OWNER}:${GITEA_PASSWORD}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${GITEA_REPO}\",\"private\":false,\"auto_init\":false}" \
    "http://localhost:3000/api/v1/user/repos")

if [ "${HTTP_CODE}" = "201" ]; then
    echo "Repository created successfully"
elif [ "${HTTP_CODE}" = "409" ]; then
    echo "Repository already exists"
else
    echo "Warning: Unexpected response code ${HTTP_CODE}, continuing anyway..."
fi

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

# Also create top-level structure for alternative paths
mkdir -p "${TEMP_DIR}/apps/gitea"
mkdir -p "${TEMP_DIR}/apps/redis"
mkdir -p "${TEMP_DIR}/repositories"

cp "${MANIFESTS_DIR}/helmrepositories.yaml" "${TEMP_DIR}/repositories/"
cp "${MANIFESTS_DIR}/gitea-helmrelease.yaml" "${TEMP_DIR}/apps/gitea/helmrelease.yaml"
cp "${MANIFESTS_DIR}/redis-helmrelease.yaml" "${TEMP_DIR}/apps/redis/helmrelease.yaml"

cat > "${TEMP_DIR}/kustomization.yaml" << 'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - repositories/helmrepositories.yaml
  - apps/gitea/helmrelease.yaml
  - apps/redis/helmrelease.yaml
EOF

# Create README
echo '# FluxUp E2E Test Repo' > "${TEMP_DIR}/README.md"

# Initialize git repo and push (from host, using port-forward)
echo "==> Initializing and pushing repository..."
cd "${TEMP_DIR}"
git init -b main
git config user.email 'fluxup@test.local'
git config user.name 'FluxUp E2E'
git add .
git commit -m 'Add test HelmRelease manifests'
git remote add origin "http://${GITEA_OWNER}:${GITEA_PASSWORD}@localhost:3000/${GITEA_OWNER}/${GITEA_REPO}.git"
git push -u origin main --force

echo ""
echo "==> Repository seeded successfully!"
echo "    Repository: ${GITEA_OWNER}/${GITEA_REPO}"
