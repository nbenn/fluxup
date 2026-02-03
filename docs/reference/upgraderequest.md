---
title: UpgradeRequest CRD Reference
description: Complete reference for the UpgradeRequest Custom Resource Definition
---

# UpgradeRequest CRD Reference

The `UpgradeRequest` custom resource triggers an application upgrade.

## API Version

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
```

## Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `managedAppRef` | ObjectReference | Yes | Reference to the ManagedApp to upgrade |
| `targetVersion` | VersionInfo | No | Target version (uses available update if omitted) |
| `skipSnapshot` | boolean | No | Skip pre-upgrade snapshots (default: false) |
| `dryRun` | boolean | No | Validate only, don't apply (default: false) |

### ObjectReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Name of the ManagedApp |
| `namespace` | string | No | Namespace (defaults to UpgradeRequest's namespace) |

### VersionInfo

| Field | Type | Description |
|-------|------|-------------|
| `chart` | string | Helm chart version |
| `images` | []ImageInfo | Container image versions |

## Status

| Field | Type | Description |
|-------|------|-------------|
| `scaling` | ScalingStatus | Workload scaling details (if workloadRef configured) |
| `snapshot` | SnapshotStatus | Pre-upgrade snapshot details |
| `upgrade` | UpgradeStatus | Upgrade operation details |
| `healthCheck` | HealthCheckStatus | Health check results |
| `conditions` | []Condition | Progress and state conditions |

### ScalingStatus

| Field | Type | Description |
|-------|------|-------------|
| `workloadKind` | string | Kind of the scaled workload (Deployment, StatefulSet) |
| `workloadName` | string | Name of the scaled workload |
| `originalReplicas` | int32 | Original replica count before scale-down |
| `scaledDownAt` | Time | When scale-down completed |

### SnapshotStatus

| Field | Type | Description |
|-------|------|-------------|
| `pvcSnapshots` | []PVCSnapshotInfo | List of created snapshots |
| `createdAt` | Time | When snapshots were created |
| `readyAt` | Time | When snapshots became ready |

### UpgradeStatus

| Field | Type | Description |
|-------|------|-------------|
| `previousVersion` | VersionInfo | Version before upgrade |
| `newVersion` | VersionInfo | Version after upgrade |
| `gitCommit` | string | Git commit SHA |
| `startedAt` | Time | When upgrade started |
| `completedAt` | Time | When upgrade completed |

### HealthCheckStatus

| Field | Type | Description |
|-------|------|-------------|
| `status` | string | `Passed`, `Failed`, or `Pending` |
| `checkedAt` | Time | When health check was performed |
| `message` | string | Details if failed |

## Conditions

| Type | Description |
|------|-------------|
| `Suspended` | Flux Kustomization has been suspended |
| `WorkloadScaled` | Workload scaling state (reason: `ScaledDown`, `ScaledUp`, or `ScalingSkipped`) |
| `SnapshotReady` | Pre-upgrade snapshots are ready |
| `GitCommitted` | Version change committed to Git |
| `Reconciled` | Flux has reconciled the changes |
| `Healthy` | Post-upgrade health check passed |
| `Complete` | Upgrade finished (check `reason` for success/failure) |

## Examples

### Basic Upgrade

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

### Upgrade to Specific Chart Version

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: gitea-upgrade-to-11
  namespace: default
spec:
  managedAppRef:
    name: gitea
  targetVersion:
    chart: "11.0.0"
```

### Upgrade to Specific Image Version

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: bazarr-upgrade
  namespace: default
spec:
  managedAppRef:
    name: bazarr
  targetVersion:
    images:
      - name: bazarr
        tag: "v1.5.0"
```

### Dry Run

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: test-upgrade
  namespace: default
spec:
  managedAppRef:
    name: my-app
  dryRun: true
```

### Skip Snapshots

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: quick-upgrade
  namespace: default
spec:
  managedAppRef:
    name: my-app
  skipSnapshot: true
```

## Kubectl Commands

### List Upgrades

```bash
kubectl get upgraderequests
```

### Watch Progress

```bash
kubectl get upgraderequest my-app-upgrade -w
```

### Get Details

```bash
kubectl describe upgraderequest my-app-upgrade
```

### Check Conditions

```bash
kubectl get upgraderequest my-app-upgrade -o jsonpath='{.status.conditions}'
```
