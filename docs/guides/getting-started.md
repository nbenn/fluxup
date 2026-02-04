---
title: Getting Started
description: Get started with FluxUp
---

# Getting Started

This guide walks you through installing FluxUp and creating your first ManagedApp.

## Prerequisites

- Kubernetes cluster (v1.26+)
- [Flux CD](https://fluxcd.io/) installed and configured
- `kubectl` configured to access your cluster
- A Git repository with your application manifests

### Optional Components

| Component | Purpose |
|-----------|---------|
| CSI Snapshot Controller | Required for pre-upgrade volume snapshots |
| Renovate | Required for automatic update detection |

## Installation

### Using kubectl

```bash
kubectl apply -f https://github.com/nbenn/fluxup/releases/latest/download/install.yaml
```

## Quick Start

### 1. Create a ManagedApp

Create a `ManagedApp` resource for each application you want FluxUp to manage:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
  namespace: default
spec:
  # Path to the manifest in your Git repo
  gitPath: "flux/apps/my-app/helmrelease.yaml"

  # Reference to the Flux Kustomization
  kustomizationRef:
    name: apps
    namespace: flux-system

  # Optional: reference HelmRelease for PVC/workload discovery
  helmReleaseRef:
    name: my-app
    namespace: my-app
```

### 2. Verify the ManagedApp

```bash
kubectl get managedapps
```

You should see output like:

```
NAME     READY   UPDATE   AGE
my-app   True    False    5m
```

### 3. Configure Git Access (For Upgrades)

To enable upgrades, FluxUp needs write access to your Git repository. Create a secret with your Git token:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: fluxup-git-credentials
  namespace: fluxup-system
type: Opaque
stringData:
  TOKEN: "your-git-token-here"
```

Then configure the controller:

```bash
kubectl set env -n fluxup-system deployment/fluxup-controller-manager \
  GIT_BACKEND=gitea \
  GIT_REPO_URL=https://gitea.example.com/org/flux-config \
  GIT_BRANCH=main

kubectl set env -n fluxup-system deployment/fluxup-controller-manager \
  --from=secret/fluxup-git-credentials --prefix=GIT_
```

See [Configuration Guide](configuration.md#git-configuration) for details.

### 4. Configure Renovate (Optional)

To enable automatic update detection, deploy the Renovate CronJob:

```bash
kubectl apply -f https://github.com/nbenn/fluxup/releases/latest/download/renovate.yaml
```

Then configure the `renovate-env` ConfigMap with your Git repository details.

### 5. Trigger an Upgrade

When an update is available, create an UpgradeRequest:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: my-app-upgrade
  namespace: default
spec:
  managedAppRef:
    name: my-app
```

Monitor progress:

```bash
kubectl get upgraderequests -w
```

## Next Steps

- [Configuration Guide](configuration.md) - Configure Git backend and snapshots
- [Triggering Upgrades](upgrades.md) - Learn about the upgrade workflow
- [Rollback Guide](rollback.md) - Learn how to rollback failed upgrades
- [ManagedApp Reference](../reference/managedapp.md) - Full CRD specification
- [UpgradeRequest Reference](../reference/upgraderequest.md) - Full CRD specification
- [RollbackRequest Reference](../reference/rollbackrequest.md) - Full CRD specification
- [Renovate Integration](renovate.md) - Set up automatic update detection
