# FluxUp

[![ci](https://github.com/nbenn/fluxup/actions/workflows/ci.yml/badge.svg)](https://github.com/nbenn/fluxup/actions/workflows/ci.yml)
[![docs](https://github.com/nbenn/fluxup/actions/workflows/docs.yml/badge.svg)](https://nbenn.github.io/fluxup/)
[![codecov](https://codecov.io/gh/nbenn/fluxup/graph/badge.svg?token=HQVI1MAAQT)](https://codecov.io/gh/nbenn/fluxup)

A Kubernetes controller for managing application upgrades in GitOps-based clusters. FluxUp leverages **Renovate** for version detection, commits updates to **Git** (source of truth), creates **CSI snapshots** before upgrades, and provides a UI for monitoring and rollback.

## Features

- **Update Detection** - Uses Renovate to detect available updates for Helm charts and container images
- **GitOps-native** - Commits version changes to Git, letting Flux handle reconciliation
- **Pre-upgrade Snapshots** - Creates CSI VolumeSnapshots before applying updates
- **Controlled Upgrades** - Suspends Flux during snapshot creation to ensure data safety
- **Rollback Support** - Quick rollback using snapshots if upgrades fail
- **Web UI** - Dashboard for viewing pending updates, triggering upgrades, and managing rollbacks

## Quick Start

### Prerequisites

- Kubernetes cluster (v1.26+)
- [Flux CD](https://fluxcd.io/) installed and configured
- A Git repository with your application manifests

### Installation

```bash
kubectl apply -f https://github.com/nbenn/fluxup/releases/latest/download/install.yaml
```

### Create a ManagedApp

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
  namespace: default
spec:
  gitPath: "flux/apps/my-app/helmrelease.yaml"
  kustomizationRef:
    name: apps
    namespace: flux-system
  workloadRef:
    kind: HelmRelease
    name: my-app
    namespace: my-app
```

### Verify

```bash
kubectl get managedapps
```

## Documentation

Full documentation is available at **[nbenn.github.io/fluxup](https://nbenn.github.io/fluxup/)**.

- [Getting Started](https://nbenn.github.io/fluxup/guides/getting-started/)
- [Configuration Guide](https://nbenn.github.io/fluxup/guides/configuration/)
- [ManagedApp CRD Reference](https://nbenn.github.io/fluxup/reference/managedapp/)
- [Renovate Integration](https://nbenn.github.io/fluxup/guides/renovate/)

## Design Documentation

For detailed architecture, CRD specifications, and implementation plans, see:

- [Architecture & Design](docs/design/architecture.md)
- [Phase 1 Implementation](docs/design/phase1.md)

## License

Apache 2.0 - see [LICENSE](LICENSE) for details.
