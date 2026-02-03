#!/bin/bash
set -euo pipefail

# Run Renovate in dryRun=lookup mode and capture output
# Usage: ./run-renovate.sh
# Requires: GITEA_URL, GITEA_TOKEN, GITEA_OWNER, GITEA_REPO env vars

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
FIXTURES_DIR="${PROJECT_ROOT}/test/fixtures/renovate"

# Load env if exists
if [ -f "${SCRIPT_DIR}/.env" ]; then
    source "${SCRIPT_DIR}/.env"
fi

# Check required vars
: "${GITEA_URL:?GITEA_URL is required}"
: "${GITEA_TOKEN:?GITEA_TOKEN is required}"
: "${GITEA_OWNER:?GITEA_OWNER is required}"
: "${GITEA_REPO:?GITEA_REPO is required}"

echo "==> Running Renovate against ${GITEA_OWNER}/${GITEA_REPO}..."

# Create temp dir for Renovate config
TEMP_DIR=$(mktemp -d)
trap "rm -rf ${TEMP_DIR}" EXIT

# Create Renovate config
cat > "${TEMP_DIR}/config.json" << EOF
{
  "\$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "platform": "gitea",
  "endpoint": "${GITEA_URL}/api/v1",
  "token": "${GITEA_TOKEN}",
  "repositories": ["${GITEA_OWNER}/${GITEA_REPO}"],
  "onboarding": false,
  "requireConfig": "ignored",
  "dryRun": "lookup",
  "flux": {
    "fileMatch": ["\\\\.ya?ml$"]
  },
  "helm-values": {
    "fileMatch": ["\\\\.ya?ml$"]
  }
}
EOF

# Run Renovate and capture output
echo "==> Running Renovate (this may take a minute)..."
RENOVATE_OUTPUT="${TEMP_DIR}/renovate-output.log"

docker run --rm \
    --network host \
    -v "${TEMP_DIR}/config.json:/config/config.json:ro" \
    -e LOG_LEVEL=debug \
    -e LOG_FORMAT=json \
    -e RENOVATE_CONFIG_FILE=/config/config.json \
    renovate/renovate:latest 2>&1 | tee "${RENOVATE_OUTPUT}"

echo ""
echo "==> Extracting package updates..."

# Extract the "packageFiles with updates" line
UPDATES_LINE=$(grep '"packageFiles with updates"' "${RENOVATE_OUTPUT}" | head -1 || echo "")

if [ -z "${UPDATES_LINE}" ]; then
    echo "WARNING: No 'packageFiles with updates' found in Renovate output"
    echo "This might mean:"
    echo "  - No updates are available (versions are current)"
    echo "  - Renovate couldn't parse the manifests"
    echo "  - Network issues reaching registries"
    echo ""
    echo "Full output saved to: ${RENOVATE_OUTPUT}"

    # Save empty fixture
    echo '{"name":"renovate","level":20,"msg":"packageFiles with updates","config":{}}' > "${FIXTURES_DIR}/no_updates.json"
    echo "Created: ${FIXTURES_DIR}/no_updates.json"
else
    echo "==> Saving fixtures..."
    mkdir -p "${FIXTURES_DIR}"

    # Save the full updates line
    echo "${UPDATES_LINE}" > "${FIXTURES_DIR}/mixed_updates.json"
    echo "Created: ${FIXTURES_DIR}/mixed_updates.json"

    # Also save a pretty-printed version for readability
    echo "${UPDATES_LINE}" | python3 -m json.tool > "${FIXTURES_DIR}/mixed_updates_pretty.json" 2>/dev/null || true

    echo ""
    echo "==> Fixtures saved to ${FIXTURES_DIR}/"
    ls -la "${FIXTURES_DIR}/"
fi

echo ""
echo "==> Done!"
echo ""
echo "To use these fixtures in tests, the parser expects the JSON structure:"
echo '  {"name":"renovate","msg":"packageFiles with updates","config":{"flux":[...],"helm-values":[...]}}'
