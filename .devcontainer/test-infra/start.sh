#!/bin/bash
set -euo pipefail

# Start Gitea for integration testing
# Usage: ./start.sh
#
# If GITEA_URL is already set and reachable, uses existing instance (e.g., CI service).
# Otherwise, starts a local container.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GITEA_CONTAINER="fluxup-gitea-test"
GITEA_ADMIN_USER="${GITEA_ADMIN_USER:-fluxup}"
GITEA_ADMIN_PASSWORD="${GITEA_ADMIN_PASSWORD:-fluxup123}"
GITEA_ADMIN_EMAIL="fluxup@test.local"

# Check if Gitea is already available (e.g., CI service)
if [ -n "${GITEA_URL:-}" ]; then
    if curl -s "${GITEA_URL}/api/v1/version" >/dev/null 2>&1; then
        echo "==> Using existing Gitea instance at ${GITEA_URL}"

        # If we have a token, we're good
        if [ -n "${GITEA_TOKEN:-}" ]; then
            echo "==> GITEA_TOKEN already set, skipping setup"
            # Write .env for other scripts
            cat > "${SCRIPT_DIR}/.env" << EOF
GITEA_URL=${GITEA_URL}
GITEA_TOKEN=${GITEA_TOKEN}
GITEA_OWNER=${GITEA_OWNER:-${GITEA_ADMIN_USER}}
GITEA_REPO=${GITEA_REPO:-flux-test-repo}
GITEA_PASSWORD=${GITEA_ADMIN_PASSWORD}
EOF
            exit 0
        fi

        # Need to create user/token on existing instance
        echo "==> Setting up user and token on existing Gitea..."
        GITEA_PORT="${GITEA_URL##*:}"
        GITEA_PORT="${GITEA_PORT%%/*}"
        STARTED_CONTAINER=false
    else
        echo "==> GITEA_URL set but not reachable, starting local container..."
        unset GITEA_URL
    fi
fi

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

# Start local container if no existing Gitea
if [ -z "${GITEA_URL:-}" ]; then
    if [ -z "${GITEA_PORT:-}" ]; then
        GITEA_PORT=$(find_free_port 3000)
    fi

    echo "==> Starting Gitea test instance on port ${GITEA_PORT}..."

    # Check if already running
    if docker ps --format '{{.Names}}' | grep -q "^${GITEA_CONTAINER}$"; then
        echo "Gitea container is already running"
        GITEA_URL="http://localhost:${GITEA_PORT}"
        STARTED_CONTAINER=true
    else
        # Remove stopped container if exists
        docker rm -f "${GITEA_CONTAINER}" 2>/dev/null || true

        # Start Gitea
        docker run -d \
            --name "${GITEA_CONTAINER}" \
            -p "${GITEA_PORT}:3000" \
            -e GITEA__security__INSTALL_LOCK=true \
            -e GITEA__server__ROOT_URL="http://localhost:${GITEA_PORT}" \
            -e GITEA__server__HTTP_PORT=3000 \
            -e USER_UID=1000 \
            -e USER_GID=1000 \
            gitea/gitea:latest

        GITEA_URL="http://localhost:${GITEA_PORT}"
        STARTED_CONTAINER=true

        echo "==> Waiting for Gitea to be ready..."
        for i in {1..60}; do
            if curl -s "${GITEA_URL}/api/v1/version" >/dev/null 2>&1; then
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
    fi
fi

# Create admin user (for local container)
if [ "${STARTED_CONTAINER:-false}" = "true" ]; then
    echo "==> Creating admin user..."
    docker exec -u git "${GITEA_CONTAINER}" gitea admin user create \
        --username "${GITEA_ADMIN_USER}" \
        --password "${GITEA_ADMIN_PASSWORD}" \
        --email "${GITEA_ADMIN_EMAIL}" \
        --admin \
        --must-change-password=false 2>/dev/null || echo "Admin user may already exist"
fi

echo "==> Generating API token..."
# Create token via API (need to use basic auth first)
TOKEN_RESPONSE=$(curl -s -X POST \
    -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASSWORD}" \
    -H "Content-Type: application/json" \
    -d '{"name":"fluxup-test-token","scopes":["write:repository","write:user"]}' \
    "${GITEA_URL}/api/v1/users/${GITEA_ADMIN_USER}/tokens")

TOKEN=$(echo "${TOKEN_RESPONSE}" | grep -o '"sha1":"[^"]*"' | cut -d'"' -f4)

if [ -z "${TOKEN}" ]; then
    echo "WARNING: Could not create token. Response: ${TOKEN_RESPONSE}"
    echo "You may need to create a token manually or the token already exists."
else
    echo "==> API Token created successfully"
fi

# Save config for tests
cat > "${SCRIPT_DIR}/.env" << EOF
GITEA_URL=${GITEA_URL}
GITEA_TOKEN=${TOKEN:-}
GITEA_OWNER=${GITEA_ADMIN_USER}
GITEA_REPO=flux-test-repo
GITEA_PASSWORD=${GITEA_ADMIN_PASSWORD}
EOF
echo "Test config saved to ${SCRIPT_DIR}/.env"

echo ""
echo "=========================================="
echo "Gitea test instance is running!"
echo "=========================================="
echo "URL:      ${GITEA_URL}"
echo "Username: ${GITEA_ADMIN_USER}"
echo "Password: ${GITEA_ADMIN_PASSWORD}"
echo ""
echo "Next steps:"
echo "  1. Run: source ${SCRIPT_DIR}/.env"
echo "  2. Run: ${SCRIPT_DIR}/seed-repo.sh"
echo "  3. Run: ${SCRIPT_DIR}/run-renovate.sh"
echo ""
