---
title: FluxUp
description: Upgrade controller for GitOps-based Kubernetes clusters
---

# FluxUp

FluxUp is a Kubernetes controller for managing application lifecycle in GitOps-based clusters. It integrates with Flux CD to detect available updates, create pre-upgrade snapshots, and provide a unified view of your applications.

## Features

- **GitOps Native** - Works with Flux CD. Updates are committed to Git and reconciled automatically.
- **Update Detection** - Integrates with Renovate to detect available updates for Helm charts and container images.
- **Safe Upgrades** - Creates CSI VolumeSnapshots before upgrades, enabling quick rollbacks.
- **Unified View** - Single pane of glass for all managed applications with health status and available updates.

## How It Works

1. **Define** your applications as `ManagedApp` resources
2. **Detect** available updates automatically via Renovate integration
3. **Upgrade** with pre-upgrade snapshots for safety
4. **Rollback** quickly if needed using the stored snapshots

## Supported Workloads

FluxUp supports managing:

- **HelmRelease** - Flux Helm releases with chart version tracking
- **Deployment** - Raw Kubernetes Deployments with image tag tracking
- **StatefulSet** - StatefulSets with image tag tracking

## Quick Links

- [Getting Started](guides/getting-started.md)
- [Configuration Guide](guides/configuration.md)
- [ManagedApp Reference](reference/managedapp.md)
