#!/bin/bash
set -euo pipefail

# Install Flux in Kind cluster for E2E tests
# Usage: ./install-flux.sh

echo "==> Installing Flux..."

# Check if flux CLI is available
if ! command -v flux &> /dev/null; then
    echo "Installing Flux CLI..."
    curl -s https://fluxcd.io/install.sh | bash
fi

# Install Flux components without GitHub integration
# We use --components to avoid requiring cloud credentials
flux install \
    --components=source-controller,kustomize-controller,helm-controller \
    --namespace=flux-system \
    --export | kubectl apply -f -

echo "==> Waiting for Flux to be ready..."
kubectl -n flux-system wait --for=condition=available --timeout=120s deployment/source-controller
kubectl -n flux-system wait --for=condition=available --timeout=120s deployment/kustomize-controller
kubectl -n flux-system wait --for=condition=available --timeout=120s deployment/helm-controller

echo "==> Flux is ready!"
flux version
