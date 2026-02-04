---
title: Configuration Guide
description: Learn how to configure FluxUp
---

# Configuration Guide

This guide covers all the configuration options available for FluxUp.

## ManagedApp Configuration

### Basic Configuration

The minimal configuration requires only `gitPath` and `kustomizationRef`:

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
```

### HelmRelease Reference

For HelmRelease-based applications, specify the HelmRelease for automatic PVC and workload discovery:

```yaml
spec:
  helmReleaseRef:
    name: my-app
    namespace: my-app  # optional, defaults to ManagedApp's namespace
```

When `helmReleaseRef` is set, FluxUp automatically:
- Discovers RWO PVCs from the Helm release manifest
- Discovers workloads (Deployments/StatefulSets) that mount those PVCs
- Scales down workloads before snapshotting for data consistency
- Checks workload health after upgrades

If `helmReleaseRef` is not set, FluxUp uses the Kustomization's inventory for discovery.

### Version Policy

Control how updates are handled:

```yaml
spec:
  versionPolicy:
    autoUpdate: none  # none, patch, minor, or major
    versionPath: "spec.chart.spec.version"  # YAML path to version field
```

| Field | Description |
|-------|-------------|
| `autoUpdate` | Auto-update policy: `none`, `patch`, `minor`, or `major` |
| `versionPath` | YAML path to the version field (see below) |

**Auto-update values:**

| Value | Description |
|-------|-------------|
| `none` | Manual upgrades only (default) |
| `patch` | Auto-apply patch updates (1.0.0 → 1.0.1) |
| `minor` | Auto-apply minor updates (1.0.0 → 1.1.0) |
| `major` | Auto-apply all updates (1.0.0 → 2.0.0) |

**Version Path:**

The `versionPath` field specifies where in the YAML manifest the version is located. This works for both Helm chart versions and Docker image tags.

For **HelmRelease chart versions** (default):
```yaml
versionPolicy:
  versionPath: "spec.chart.spec.version"  # This is the default
```

For **image tags in HelmRelease values**:
```yaml
versionPolicy:
  versionPath: "spec.values.image.tag"
```

For **Deployment image tags** (requires explicit path):
```yaml
versionPolicy:
  versionPath: "spec.template.spec.containers.0.image"
```

> **Note:** For image updates, `versionPath` is required. There is no sensible default for image tag locations.

### App-of-Apps Pattern (suspendRef)

In Flux, it's common to use an "app-of-apps" pattern where a root Kustomization manages child Kustomizations:

```
root-kustomization (flux-system)     ← root, not managed by another Kustomization
  └── app-kustomization (apps)       ← managed by root
        └── HelmRelease
              └── StatefulSet
```

**The Problem:** When FluxUp suspends the child `app-kustomization` during an upgrade, the parent `root-kustomization` may reconcile and set `suspend: false` back on the child - potentially corrupting data mid-upgrade.

**The Solution:** Use `suspendRef` to tell FluxUp to suspend the root Kustomization instead:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
spec:
  gitPath: "flux/apps/my-app/helmrelease.yaml"

  # Where Git changes are made (the child Kustomization)
  kustomizationRef:
    name: my-app
    namespace: apps

  # What to suspend (the root Kustomization)
  suspendRef:
    name: root-apps
    namespace: flux-system

  helmReleaseRef:
    name: my-app
    namespace: my-app
```

**How FluxUp detects this:**
- If `kustomizationRef` points to a Kustomization that is managed by another Kustomization (detected via owner references or Flux labels), FluxUp will fail with a clear error message
- The error tells you to set `suspendRef` to a root Kustomization

**Trade-offs:**
- Suspending a parent Kustomization means sibling apps won't reconcile during the upgrade window (typically minutes)
- This is safer than risking the child being un-suspended mid-operation
- Only the target workload is scaled down; sibling apps continue running

### Auto-Rollback

Enable automatic rollback when upgrades fail after the Git commit (point of no return):

```yaml
spec:
  autoRollback: true  # default: false
```

When enabled, if an upgrade fails during reconciliation or health checks, FluxUp automatically creates a `RollbackRequest` to restore the previous version and data.

### Health Check Configuration

Customize health check behavior:

```yaml
spec:
  healthCheck:
    timeout: "10m"  # default: 5m
```

### Volume Snapshots

Configure pre-upgrade snapshots:

```yaml
spec:
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: "csi-snapclass"
    # PVCs are auto-discovered, but can be explicit:
    pvcs:
      - name: data
        namespace: my-app
    # Exclude specific PVCs from auto-discovery:
    excludePVCs:
      - name: cache-volume  # ephemeral, don't snapshot
    retentionPolicy:
      maxCount: 3  # Keep 3 snapshots per PVC (default)
```

**PVC Discovery:**
- When `helmReleaseRef` is set, PVCs are discovered from the Helm release manifest
- When not set, PVCs are discovered from the Kustomization's inventory
- Only ReadWriteOnce (RWO) PVCs are considered (shared volumes don't need scale-down)
- Use `pvcs` to override auto-discovery with an explicit list
- Use `excludePVCs` to exclude specific PVCs from auto-discovery

The retention policy uses a generational approach: after each successful upgrade, snapshots beyond `maxCount` are pruned (oldest first). Set `maxCount: 0` to disable pruning and keep all snapshots.

## Git Configuration

To enable upgrades, FluxUp needs access to your Git repository. Configure via environment variables on the controller deployment.

### Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `GIT_BACKEND` | Yes | Git provider: `gitea`, `github`, or `gitlab` |
| `GIT_REPO_URL` | Yes | Repository URL (e.g., `https://gitea.example.com/org/repo`) |
| `GIT_BRANCH` | No | Branch to commit to (default: `main`) |
| `GIT_TOKEN` | Yes | Authentication token with write access |

### Creating the Credentials Secret

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

### Configuring the Controller

Edit the controller deployment to add environment variables:

```yaml
env:
  - name: GIT_BACKEND
    value: "gitea"
  - name: GIT_REPO_URL
    value: "https://gitea.example.com/org/flux-config"
  - name: GIT_BRANCH
    value: "main"
  - name: GIT_TOKEN
    valueFrom:
      secretKeyRef:
        name: fluxup-git-credentials
        key: TOKEN
```

Or use `kubectl set env`:

```bash
kubectl set env -n fluxup-system deployment/fluxup-controller-manager \
  GIT_BACKEND=gitea \
  GIT_REPO_URL=https://gitea.example.com/org/flux-config \
  GIT_BRANCH=main

# The secret key must be uppercase (TOKEN) so --prefix=GIT_ creates GIT_TOKEN
kubectl set env -n fluxup-system deployment/fluxup-controller-manager \
  --from=secret/fluxup-git-credentials --prefix=GIT_
```

### Git Token Permissions

The token needs write access to the repository:

| Provider | Required Scopes |
|----------|-----------------|
| Gitea | `repo` (or repository write access) |
| GitHub | `repo` or `contents:write` |
| GitLab | `api` or `write_repository` |

## Controller Configuration

The FluxUp controller can be configured via command-line flags.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `0` | Metrics endpoint address |
| `--health-probe-bind-address` | `:8081` | Health probe endpoint |
| `--leader-elect` | `false` | Enable leader election |
| `--git-backend` | | Git provider (can also use `GIT_BACKEND` env var) |
| `--git-repo-url` | | Repository URL (can also use `GIT_REPO_URL` env var) |
| `--git-branch` | `main` | Git branch (can also use `GIT_BRANCH` env var) |
