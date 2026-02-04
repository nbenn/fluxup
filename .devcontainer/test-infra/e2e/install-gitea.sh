#!/bin/bash
set -euo pipefail

# Install Gitea in Kind cluster for E2E tests
# Usage: ./install-gitea.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GITEA_NAMESPACE="gitea"
GITEA_ADMIN_USER="fluxup"
GITEA_ADMIN_PASSWORD="fluxup123"

echo "==> Creating Gitea namespace..."
kubectl create namespace "${GITEA_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "==> Deploying Gitea..."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: gitea-data
  namespace: ${GITEA_NAMESPACE}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gitea
  namespace: ${GITEA_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: gitea
  template:
    metadata:
      labels:
        app: gitea
    spec:
      containers:
      - name: gitea
        image: gitea/gitea:latest
        ports:
        - containerPort: 3000
          name: http
        - containerPort: 22
          name: ssh
        env:
        - name: GITEA__security__INSTALL_LOCK
          value: "true"
        - name: GITEA__server__ROOT_URL
          value: "http://gitea.${GITEA_NAMESPACE}.svc.cluster.local:3000"
        - name: GITEA__server__HTTP_PORT
          value: "3000"
        - name: GITEA__server__SSH_DOMAIN
          value: "gitea.${GITEA_NAMESPACE}.svc.cluster.local"
        - name: USER_UID
          value: "1000"
        - name: USER_GID
          value: "1000"
        volumeMounts:
        - name: data
          mountPath: /data
        readinessProbe:
          httpGet:
            path: /api/v1/version
            port: 3000
          initialDelaySeconds: 5
          periodSeconds: 5
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: gitea-data
---
apiVersion: v1
kind: Service
metadata:
  name: gitea
  namespace: ${GITEA_NAMESPACE}
spec:
  selector:
    app: gitea
  ports:
  - name: http
    port: 3000
    targetPort: 3000
  - name: ssh
    port: 22
    targetPort: 22
EOF

echo "==> Waiting for Gitea to be ready..."
kubectl -n "${GITEA_NAMESPACE}" wait --for=condition=available --timeout=180s deployment/gitea

# Wait for pod to be fully ready
echo "==> Waiting for Gitea pod to be ready..."
kubectl -n "${GITEA_NAMESPACE}" wait --for=condition=ready --timeout=60s pod -l app=gitea

# Create admin user
echo "==> Creating Gitea admin user..."
GITEA_POD=$(kubectl -n "${GITEA_NAMESPACE}" get pod -l app=gitea -o jsonpath='{.items[0].metadata.name}')

# Wait a bit for gitea to fully initialize
sleep 5

kubectl -n "${GITEA_NAMESPACE}" exec "${GITEA_POD}" -- \
    gitea admin user create \
    --username "${GITEA_ADMIN_USER}" \
    --password "${GITEA_ADMIN_PASSWORD}" \
    --email "fluxup@test.local" \
    --admin \
    --must-change-password=false 2>/dev/null || echo "Admin user may already exist"

# Generate API token
echo "==> Generating API token..."
GITEA_URL="http://gitea.${GITEA_NAMESPACE}.svc.cluster.local:3000"

# Create a job to generate the token from inside the cluster
TOKEN=$(kubectl -n "${GITEA_NAMESPACE}" exec "${GITEA_POD}" -- \
    gitea admin user generate-access-token \
    --username "${GITEA_ADMIN_USER}" \
    --scopes "write:repository,write:user" \
    --token-name "fluxup-e2e-token" 2>/dev/null | grep -oP 'Access token was successfully created: \K.*' || echo "")

if [ -z "${TOKEN}" ]; then
    echo "WARNING: Could not generate token via CLI, trying API..."
    # Try via API using port-forward
    kubectl -n "${GITEA_NAMESPACE}" port-forward svc/gitea 3000:3000 &
    PF_PID=$!
    sleep 3
    
    TOKEN_RESPONSE=$(curl -s -X POST \
        -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASSWORD}" \
        -H "Content-Type: application/json" \
        -d '{"name":"fluxup-e2e-token","scopes":["write:repository","write:user"]}' \
        "http://localhost:3000/api/v1/users/${GITEA_ADMIN_USER}/tokens" 2>/dev/null || echo "")
    
    kill ${PF_PID} 2>/dev/null || true
    
    TOKEN=$(echo "${TOKEN_RESPONSE}" | grep -o '"sha1":"[^"]*"' | cut -d'"' -f4 || echo "")
fi

if [ -z "${TOKEN}" ]; then
    echo "ERROR: Could not generate API token"
    exit 1
fi

echo "==> Creating Gitea credentials secret..."
kubectl -n fluxup-system create namespace fluxup-system --dry-run=client -o yaml | kubectl apply -f -

kubectl -n fluxup-system create secret generic fluxup-git-credentials \
    --from-literal=TOKEN="${TOKEN}" \
    --dry-run=client -o yaml | kubectl apply -f -

# Save config
cat > "${SCRIPT_DIR}/.env" << EOF
GITEA_URL=${GITEA_URL}
GITEA_TOKEN=${TOKEN}
GITEA_OWNER=${GITEA_ADMIN_USER}
GITEA_REPO=flux-test-repo
GITEA_PASSWORD=${GITEA_ADMIN_PASSWORD}
GITEA_NAMESPACE=${GITEA_NAMESPACE}
EOF

echo ""
echo "==> Gitea is ready!"
echo "    URL (in-cluster): ${GITEA_URL}"
echo "    Username: ${GITEA_ADMIN_USER}"
echo "    Config saved to: ${SCRIPT_DIR}/.env"
