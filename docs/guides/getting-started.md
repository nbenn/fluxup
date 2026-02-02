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
apiVersion: fluxup.fluxup.dev/v1alpha1
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

  # Optional: explicit workload for health checks
  workloadRef:
    kind: HelmRelease
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

### 3. Configure Renovate (Optional)

To enable automatic update detection, deploy the Renovate CronJob:

```bash
kubectl apply -f https://github.com/nbenn/fluxup/releases/latest/download/renovate.yaml
```

Then configure the `renovate-env` ConfigMap with your Git repository details.

## Next Steps

- [Configuration Guide](configuration.md) - Learn about all configuration options
- [ManagedApp Reference](../reference/managedapp.md) - Full CRD specification
- [Renovate Integration](renovate.md) - Set up automatic update detection
