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
| `gitPath` | string | Yes | Path to the manifest in Git |
| `kustomizationRef` | ObjectReference | Yes | Reference to the Flux Kustomization |
| `suspendRef` | ObjectReference | | Kustomization to suspend (defaults to `kustomizationRef`). Use for app-of-apps patterns. |
| `helmReleaseRef` | ObjectReference | | Reference to HelmRelease for PVC and workload discovery |
| `versionPolicy` | VersionPolicy | | Update policy configuration |
| `healthCheck` | HealthCheckConfig | | Health check configuration |
| `volumeSnapshots` | VolumeSnapshotConfig | | Pre-upgrade snapshot configuration |
| `autoRollback` | boolean | | Enable automatic rollback on upgrade failure (default: false) |

### ObjectReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Name of the object |
| `namespace` | string | | Namespace (defaults vary by context) |

### VersionPolicy

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `autoUpdate` | string | `none` | One of: `none`, `patch`, `minor`, `major` |
| `versionPath` | string | | YAML path to version field. Defaults to `spec.chart.spec.version` for chart updates. **Required** for image updates. |

### HealthCheckConfig

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `timeout` | string | `5m` | Health check timeout duration |

### VolumeSnapshotConfig

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `enabled` | boolean | Yes | Enable pre-upgrade snapshots |
| `volumeSnapshotClassName` | string | | CSI snapshot class name |
| `pvcs` | []PVCRef | | Explicit PVCs to snapshot (auto-discovered if omitted) |
| `excludePVCs` | []PVCRef | | PVCs to exclude from auto-discovery |
| `retentionPolicy` | RetentionPolicy | | Snapshot retention settings |

### PVCRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Name of the PVC |
| `namespace` | string | | Namespace (defaults to ManagedApp's namespace) |

### RetentionPolicy

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `maxCount` | int | 3 | Maximum snapshots to keep per PVC. Set to 0 to keep all. |

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
| `Ready` | Whether the application is healthy (Flux reconciled and workloads ready) |
| `UpdateAvailable` | Whether an update is available |

## PVC and Workload Discovery

FluxUp automatically discovers PVCs and workloads for snapshotting and scaling:

**When `helmReleaseRef` is set:**
- Discovers PVCs and workloads from the Helm release secret
- Decodes the Helm manifest to find Deployments/StatefulSets
- Finds RWO PVCs mounted by those workloads

**When `helmReleaseRef` is not set:**
- Discovers from the Kustomization's inventory (`.status.inventory.entries`)
- Finds workloads and their mounted RWO PVCs

You can override auto-discovery with explicit `volumeSnapshots.pvcs` or exclude specific PVCs with `volumeSnapshots.excludePVCs`.

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
  helmReleaseRef:
    name: gitea
    namespace: gitea
  versionPolicy:
    autoUpdate: none
  healthCheck:
    timeout: "10m"
```

### Image Updates via HelmRelease Values

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: bazarr
  namespace: default
spec:
  gitPath: "flux/apps/bazarr/helmrelease.yaml"
  kustomizationRef:
    name: apps
    namespace: flux-system
  helmReleaseRef:
    name: bazarr
    namespace: media
  versionPolicy:
    autoUpdate: patch
    versionPath: "spec.values.image.tag"  # Required for image updates
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
  helmReleaseRef:
    name: gitea
    namespace: gitea
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: csi-snapclass
    # PVCs are auto-discovered from HelmRelease, but can be explicit:
    pvcs:
      - name: data-gitea-0
      - name: data-gitea-postgresql-0
    retentionPolicy:
      maxCount: 3
  healthCheck:
    timeout: "10m"
```

### App-of-Apps Pattern (with suspendRef)

When using an app-of-apps pattern where a parent Kustomization manages child Kustomizations, use `suspendRef` to suspend the root:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
  namespace: default
spec:
  gitPath: "flux/apps/my-app/helmrelease.yaml"
  # The child Kustomization where Git changes are made
  kustomizationRef:
    name: my-app
    namespace: apps
  # The root Kustomization to suspend (prevents parent from un-suspending child)
  suspendRef:
    name: root-apps
    namespace: flux-system
  helmReleaseRef:
    name: my-app
    namespace: my-app
  autoRollback: true  # Automatically rollback if upgrade fails
```

### Kustomization-based App (no HelmRelease)

For apps deployed directly via Kustomization without Helm:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: simple-app
  namespace: default
spec:
  gitPath: "flux/apps/simple-app/deployment.yaml"
  kustomizationRef:
    name: apps
    namespace: flux-system
  # No helmReleaseRef - workloads discovered from Kustomization inventory
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: csi-snapclass
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
