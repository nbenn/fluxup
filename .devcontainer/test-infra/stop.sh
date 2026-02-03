#!/bin/bash
set -euo pipefail

# Stop Gitea test instance
# Usage: ./stop.sh

GITEA_CONTAINER="fluxup-gitea-test"

echo "==> Stopping Gitea test instance..."

if docker ps --format '{{.Names}}' | grep -q "^${GITEA_CONTAINER}$"; then
    docker stop "${GITEA_CONTAINER}"
    docker rm "${GITEA_CONTAINER}"
    echo "Gitea stopped and removed."
else
    echo "Gitea container not running."
fi

# Clean up env file
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
rm -f "${SCRIPT_DIR}/.env"
