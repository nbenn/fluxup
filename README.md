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
  helmReleaseRef:
    name: my-app
    namespace: my-app
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: csi-snapclass
    # PVCs are auto-discovered, or can be explicit:
    pvcs:
      - name: data-my-app-0
```

### Trigger an Upgrade

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: my-app-upgrade
spec:
  managedAppRef:
    name: my-app
```

### Verify

```bash
kubectl get managedapps
kubectl get upgraderequests
```

## Documentation

Full documentation is available at **[nbenn.github.io/fluxup](https://nbenn.github.io/fluxup/)**.

- [Getting Started](https://nbenn.github.io/fluxup/guides/getting-started/)
- [Configuration Guide](https://nbenn.github.io/fluxup/guides/configuration/)
- [Triggering Upgrades](https://nbenn.github.io/fluxup/guides/upgrades/)
- [ManagedApp Reference](https://nbenn.github.io/fluxup/reference/managedapp/)
- [UpgradeRequest Reference](https://nbenn.github.io/fluxup/reference/upgraderequest/)
- [Renovate Integration](https://nbenn.github.io/fluxup/guides/renovate/)

## Development

### Prerequisites

The project includes a **devcontainer** configuration that provides all necessary tools:
- Go 1.25
- Docker-in-Docker
- Kind (Kubernetes in Docker)
- Kubebuilder
- kubectl

To use it, open the project in VS Code with the Dev Containers extension, or use GitHub Codespaces.

### Running Tests

```bash
# Unit tests (fast, no external dependencies)
make test-unit   # or just: make test

# Lint
make lint

# Git integration tests (starts Gitea container automatically)
make test-git

# Kubernetes tests (creates Kind cluster, deploys controller)
make test-k8s

# Full E2E tests (Kind + Flux + Gitea)
make test-e2e
```

### Test Infrastructure

The project uses containerized test infrastructure with a consistent `up`/`down` pattern:

| Test | Purpose | Infrastructure |
|------|---------|----------------|
| `test-unit` | Unit tests | None |
| `test-git` | Git integration tests | Gitea container |
| `test-k8s` | Kubernetes controller tests | Kind cluster |
| `test-e2e` | Full end-to-end tests | Kind + Flux + Gitea |

All test commands automatically start and stop their infrastructure.

### Regenerating Renovate Fixtures

To update the Renovate test fixtures with real output:

```bash
make test-fixtures
```

This starts Gitea, seeds a test repository, runs Renovate in dry-run mode, and captures the JSON output to `test/fixtures/renovate/`.

## Design Documentation

For architecture and implementation details, see:

- [Architecture & Design](docs/design/architecture.md)
- [Phase 1 Design](docs/design/phase1.md)
- [Phase 2 Design](docs/design/phase2.md)
- [Phase 3 Design](docs/design/phase3.md)

## License

Apache 2.0 - see [LICENSE](LICENSE) for details.
