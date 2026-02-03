#!/bin/bash
set -euo pipefail

# Seed test repository with HelmRelease manifests
# Usage: ./seed-repo.sh
# Requires: GITEA_URL, GITEA_TOKEN, GITEA_OWNER, GITEA_REPO env vars

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load env if exists
if [ -f "${SCRIPT_DIR}/.env" ]; then
    source "${SCRIPT_DIR}/.env"
fi

# Check required vars
: "${GITEA_URL:?GITEA_URL is required}"
: "${GITEA_TOKEN:?GITEA_TOKEN is required}"
: "${GITEA_OWNER:?GITEA_OWNER is required}"
: "${GITEA_REPO:?GITEA_REPO is required}"

GITEA_PASSWORD="${GITEA_PASSWORD:-fluxup123}"
API_URL="${GITEA_URL}/api/v1"
AUTH_HEADER="Authorization: token ${GITEA_TOKEN}"

echo "==> Creating repository ${GITEA_OWNER}/${GITEA_REPO}..."

# Create repo (ignore error if exists)
curl -s -X POST \
    -H "${AUTH_HEADER}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${GITEA_REPO}\",\"auto_init\":true,\"default_branch\":\"main\"}" \
    "${API_URL}/user/repos" >/dev/null || true

# Wait for repo to be ready
sleep 2

echo "==> Cloning and seeding repository..."

# Use git clone/push (more reliable than Gitea API for nested directories)
TEMP_DIR=$(mktemp -d)
trap "rm -rf ${TEMP_DIR}" EXIT

cd "${TEMP_DIR}"

# Clone the repo (extract host:port from GITEA_URL)
GITEA_HOST="${GITEA_URL#http://}"
GITEA_HOST="${GITEA_HOST#https://}"
git clone "http://${GITEA_OWNER}:${GITEA_PASSWORD}@${GITEA_HOST}/${GITEA_OWNER}/${GITEA_REPO}.git" repo 2>/dev/null
cd repo

# Configure git identity for CI environments
git config user.email "fluxup-test@localhost"
git config user.name "FluxUp Test"

# Create directory structure
mkdir -p flux/repositories flux/apps/gitea flux/apps/redis

# Copy manifests
cp "${SCRIPT_DIR}/manifests/helmrepositories.yaml" flux/repositories/
cp "${SCRIPT_DIR}/manifests/gitea-helmrelease.yaml" flux/apps/gitea/helmrelease.yaml
cp "${SCRIPT_DIR}/manifests/redis-helmrelease.yaml" flux/apps/redis/helmrelease.yaml

# Commit and push
git add .
git commit -m "Add test HelmRelease manifests" >/dev/null
git push origin main >/dev/null 2>&1

echo "  Uploaded: flux/repositories/helmrepositories.yaml"
echo "  Uploaded: flux/apps/gitea/helmrelease.yaml"
echo "  Uploaded: flux/apps/redis/helmrelease.yaml"

echo ""
echo "==> Repository seeded successfully!"
echo "    ${GITEA_URL}/${GITEA_OWNER}/${GITEA_REPO}"
echo ""
echo "Next step: Run ${SCRIPT_DIR}/run-renovate.sh to generate Renovate output"
