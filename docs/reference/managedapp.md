---
title: ManagedApp CRD Reference
description: Complete reference for the ManagedApp Custom Resource Definition
---

# ManagedApp CRD Reference

The `ManagedApp` custom resource defines an application managed by FluxUp.

## API Version

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
```

## Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `gitPath` | string | ✅ | Path to the manifest in Git |
| `kustomizationRef` | ObjectReference | ✅ | Reference to the Flux Kustomization |
| `workloadRef` | WorkloadReference | | Optional workload for health checks |
| `versionPolicy` | VersionPolicy | | Update policy configuration |
| `healthCheck` | HealthCheckConfig | | Health check configuration |
| `volumeSnapshots` | VolumeSnapshotConfig | | Pre-upgrade snapshot configuration |

### ObjectReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | ✅ | Name of the object |
| `namespace` | string | | Namespace (defaults vary by context) |

### WorkloadReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `kind` | string | ✅ | One of: `Deployment`, `StatefulSet`, `HelmRelease` |
| `name` | string | ✅ | Name of the workload |
| `namespace` | string | | Namespace (defaults to ManagedApp's namespace) |

### VersionPolicy

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `autoUpdate` | string | `none` | One of: `none`, `patch`, `minor`, `major` |
| `versionPath` | string | | YAML path to version field (defaults to `spec.chart.spec.version` for HelmRelease) |

### HealthCheckConfig

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `timeout` | string | `5m` | Health check timeout duration |

### VolumeSnapshotConfig

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `enabled` | boolean | ✅ | Enable pre-upgrade snapshots |
| `volumeSnapshotClassName` | string | | CSI snapshot class name |
| `pvcs` | []PVCRef | | PVCs to snapshot |
| `retentionPolicy` | RetentionPolicy | | Snapshot retention settings |

## Status

| Field | Type | Description |
|-------|------|-------------|
| `currentVersion` | VersionInfo | Current deployed version |
| `availableUpdate` | VersionInfo | Available update (if any) |
| `lastUpgrade` | UpgradeInfo | Last upgrade details |
| `conditions` | []Condition | Standard Kubernetes conditions |

## Conditions

| Type | Description |
|------|-------------|
| `Ready` | Whether the workload is healthy |
| `UpdateAvailable` | Whether an update is available |

## Examples

### HelmRelease-based App

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: gitea
  namespace: default
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: apps
    namespace: flux-system
  workloadRef:
    kind: HelmRelease
    name: gitea
    namespace: gitea
  versionPolicy:
    autoUpdate: none
  healthCheck:
    timeout: "10m"
```

### Deployment-based App

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: bazarr
  namespace: default
spec:
  gitPath: "flux/apps/bazarr/deployment.yaml"
  kustomizationRef:
    name: apps
    namespace: flux-system
  workloadRef:
    kind: Deployment
    name: bazarr
    namespace: media
  versionPolicy:
    autoUpdate: patch
```

### With Volume Snapshots

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: gitea
  namespace: default
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: apps
    namespace: flux-system
  workloadRef:
    kind: HelmRelease
    name: gitea
    namespace: gitea
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: csi-snapclass
    pvcs:
      - name: data-gitea-0
      - name: data-gitea-postgresql-0
    retentionPolicy:
      maxCount: 3
      maxAge: "168h"
  healthCheck:
    timeout: "10m"
```

### Minimal Configuration

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: simple-app
  namespace: default
spec:
  gitPath: "flux/apps/simple-app/kustomization.yaml"
  kustomizationRef:
    name: apps
    namespace: flux-system
```
