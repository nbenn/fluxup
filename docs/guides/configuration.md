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

### Workload Reference

For more accurate health checks, specify the workload explicitly:

```yaml
spec:
  workloadRef:
    kind: HelmRelease  # or Deployment, StatefulSet
    name: my-app
    namespace: my-app  # optional, defaults to ManagedApp's namespace
```

### Version Policy

Control how updates are handled:

```yaml
spec:
  versionPolicy:
    autoUpdate: none  # none, patch, minor, or major
```

| Value | Description |
|-------|-------------|
| `none` | Manual upgrades only (default) |
| `patch` | Auto-apply patch updates (1.0.0 → 1.0.1) |
| `minor` | Auto-apply minor updates (1.0.0 → 1.1.0) |
| `major` | Auto-apply all updates (1.0.0 → 2.0.0) |

### Health Check Configuration

Customize health check behavior:

```yaml
spec:
  healthCheck:
    timeout: "10m"  # default: 5m
```

### Volume Snapshots (Phase 2)

Configure pre-upgrade snapshots:

```yaml
spec:
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: "csi-snapclass"
    pvcs:
      - name: data
        namespace: my-app
    retentionPolicy:
      maxCount: 3  # Keep 3 snapshots per PVC (default)
```

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
  token: "your-git-token-here"
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
        key: token
```

Or use `kubectl set env`:

```bash
kubectl set env -n fluxup-system deployment/fluxup-controller-manager \
  GIT_BACKEND=gitea \
  GIT_REPO_URL=https://gitea.example.com/org/flux-config \
  GIT_BRANCH=main

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
