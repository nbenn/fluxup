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
| `scaling` | ScalingStatus | Workload scaling details (auto-discovered workloads mounting target PVCs) |
| `snapshot` | SnapshotStatus | Pre-upgrade snapshot details |
| `upgrade` | UpgradeStatus | Upgrade operation details |
| `healthCheck` | HealthCheckStatus | Health check results |
| `phaseStartedAt` | Time | When the current phase began (used for per-phase timeout calculations) |
| `conditions` | []Condition | Progress and state conditions |

### ScalingStatus

| Field | Type | Description |
|-------|------|-------------|
| `workloads` | []WorkloadScalingInfo | List of workloads that were scaled down |
| `scaledDownAt` | Time | When scale-down completed for all workloads |

### WorkloadScalingInfo

| Field | Type | Description |
|-------|------|-------------|
| `kind` | string | Kind of the workload (Deployment, StatefulSet) |
| `name` | string | Name of the workload |
| `namespace` | string | Namespace of the workload |

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
| `WorkloadStopped` | All workloads mounting target PVCs have been stopped |
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

Validate an upgrade and exercise the suspend/scale cycle without making Git or snapshot changes:

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

The dry run performs preflight checks (validates suspend target, discovers PVCs/workloads, verifies VolumeSnapshotClass), previews the Git diff, then exercises the full quiescence cycle (suspend, scale down, scale up, resume). See the [Upgrades Guide](../guides/upgrades.md#dry-run) for details.

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

## Upgrade Workflow

The upgrade proceeds through these steps:

1. **Validate** - Check ManagedApp exists and an update is available
2. **Preflight** - Validate suspend target, discover PVCs/workloads, verify VolumeSnapshotClass
3. **Git diff preview** - Log the version change that will be applied
4. **Suspend** - Suspend the Flux Kustomization to prevent reconciliation (2 min timeout)
5. **Scale down** - Scale down auto-discovered workloads mounting target PVCs (5 min timeout)
6. **Snapshot** - Create CSI VolumeSnapshots of target PVCs (30 min timeout)
7. **Git commit** - Commit the version change to Git (2 min timeout) -- **point of no return**
8. **Resume & Reconcile** - Resume the Kustomization and wait for Flux to reconcile (10 min timeout)
9. **Health Check** - Wait for workloads to become healthy (5 min timeout, configurable)

Each phase has an independent timeout tracked via `phaseStartedAt`.

## Failure Handling

How FluxUp handles failures depends on when the failure occurs:

- **Before Git commit:** Flux Kustomization is automatically resumed, no changes made to Git. You can fix the issue and retry.
- **After Git commit:** The new version is already in Git. A rollback is required (manual or auto-triggered if `autoRollback: true` on ManagedApp).

## Next Steps

- [Triggering Upgrades](../guides/upgrades.md) - Detailed upgrade guide with troubleshooting
- [Rollback Guide](../guides/rollback.md) - How to rollback failed upgrades
- [RollbackRequest Reference](rollbackrequest.md) - Full rollback CRD specification
