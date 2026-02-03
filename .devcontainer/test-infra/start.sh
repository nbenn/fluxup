#!/bin/bash
set -euo pipefail

# Start Gitea for integration testing
# Usage: ./start.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GITEA_CONTAINER="fluxup-gitea-test"
GITEA_ADMIN_USER="fluxup"
GITEA_ADMIN_PASSWORD="fluxup123"
GITEA_ADMIN_EMAIL="fluxup@test.local"

# Find an available port if not specified
find_free_port() {
    local port="${1:-3000}"
    while nc -z localhost "$port" 2>/dev/null || lsof -i :"$port" >/dev/null 2>&1; do
        port=$((port + 1))
        if [ "$port" -gt 4000 ]; then
            echo "ERROR: Could not find free port between 3000-4000" >&2
            exit 1
        fi
    done
    echo "$port"
}

if [ -z "${GITEA_PORT:-}" ]; then
    GITEA_PORT=$(find_free_port 3000)
fi

echo "==> Starting Gitea test instance on port ${GITEA_PORT}..."

# Check if already running
if docker ps --format '{{.Names}}' | grep -q "^${GITEA_CONTAINER}$"; then
    echo "Gitea is already running at http://localhost:${GITEA_PORT}"
    exit 0
fi

# Remove stopped container if exists
docker rm -f "${GITEA_CONTAINER}" 2>/dev/null || true

# Start Gitea (run as git user to avoid root warnings)
docker run -d \
    --name "${GITEA_CONTAINER}" \
    -p "${GITEA_PORT}:3000" \
    -e GITEA__security__INSTALL_LOCK=true \
    -e GITEA__server__ROOT_URL="http://localhost:${GITEA_PORT}" \
    -e GITEA__server__HTTP_PORT=3000 \
    -e USER_UID=1000 \
    -e USER_GID=1000 \
    gitea/gitea:latest

echo "==> Waiting for Gitea to be ready..."
for i in {1..60}; do
    if curl -s "http://localhost:${GITEA_PORT}/api/v1/version" >/dev/null 2>&1; then
        echo "Gitea is ready!"
        break
    fi
    if [ $i -eq 60 ]; then
        echo "ERROR: Gitea failed to start within 60 seconds"
        docker logs "${GITEA_CONTAINER}"
        exit 1
    fi
    sleep 1
done

echo "==> Creating admin user..."
docker exec -u git "${GITEA_CONTAINER}" gitea admin user create \
    --username "${GITEA_ADMIN_USER}" \
    --password "${GITEA_ADMIN_PASSWORD}" \
    --email "${GITEA_ADMIN_EMAIL}" \
    --admin \
    --must-change-password=false 2>/dev/null || echo "Admin user may already exist"

echo "==> Generating API token..."
# Create token via API (need to use basic auth first)
TOKEN_RESPONSE=$(curl -s -X POST \
    -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASSWORD}" \
    -H "Content-Type: application/json" \
    -d '{"name":"fluxup-test-token","scopes":["write:repository","write:user"]}' \
    "http://localhost:${GITEA_PORT}/api/v1/users/${GITEA_ADMIN_USER}/tokens")

TOKEN=$(echo "${TOKEN_RESPONSE}" | grep -o '"sha1":"[^"]*"' | cut -d'"' -f4)

if [ -z "${TOKEN}" ]; then
    echo "WARNING: Could not create token. Response: ${TOKEN_RESPONSE}"
    echo "You may need to create a token manually or the token already exists."
else
    echo "==> API Token created successfully"

    # Save config for tests
    cat > "${SCRIPT_DIR}/.env" << EOF
GITEA_URL=http://localhost:${GITEA_PORT}
GITEA_TOKEN=${TOKEN}
GITEA_OWNER=${GITEA_ADMIN_USER}
GITEA_REPO=flux-test-repo
EOF
    echo "Test config saved to ${SCRIPT_DIR}/.env"
fi

echo ""
echo "=========================================="
echo "Gitea test instance is running!"
echo "=========================================="
echo "URL:      http://localhost:${GITEA_PORT}"
echo "Username: ${GITEA_ADMIN_USER}"
echo "Password: ${GITEA_ADMIN_PASSWORD}"
echo ""
echo "Next steps:"
echo "  1. Run: source ${SCRIPT_DIR}/.env"
echo "  2. Run: ${SCRIPT_DIR}/seed-repo.sh"
echo "  3. Run: ${SCRIPT_DIR}/run-renovate.sh"
echo ""
