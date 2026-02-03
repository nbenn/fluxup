---
title: RollbackRequest CRD Reference
description: Complete reference for the RollbackRequest Custom Resource Definition
---

# RollbackRequest CRD Reference

The `RollbackRequest` custom resource triggers a rollback of an application to its previous version by restoring PVCs from snapshots and reverting the version in Git.

## API Version

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
```

## Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `upgradeRequestRef` | ObjectReference | Yes | Reference to the UpgradeRequest to rollback |
| `autoTriggered` | boolean | No | Whether this was auto-triggered by a failed upgrade (default: false) |
| `dryRun` | boolean | No | Validate only, don't apply (default: false) |

### ObjectReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Name of the UpgradeRequest |
| `namespace` | string | No | Namespace (defaults to RollbackRequest's namespace) |

## Status

| Field | Type | Description |
|-------|------|-------------|
| `restoredFrom` | RestoredFromStatus | Information about what was restored |
| `volumeRestore` | VolumeRestoreStatus | PVC restoration details |
| `gitRevert` | GitRevertStatus | Git revert details |
| `startedAt` | Time | When rollback started |
| `completedAt` | Time | When rollback completed |
| `conditions` | []Condition | Progress and state conditions |

### RestoredFromStatus

| Field | Type | Description |
|-------|------|-------------|
| `upgradeRequestName` | string | The UpgradeRequest that was rolled back |
| `targetVersion` | VersionInfo | The version being rolled back to |
| `rolledBackVersion` | VersionInfo | The version being rolled back from |

### VolumeRestoreStatus

| Field | Type | Description |
|-------|------|-------------|
| `restoredPVCs` | []RestoredPVCInfo | PVCs that were restored |
| `startedAt` | Time | When restore started |
| `completedAt` | Time | When restore completed |

### RestoredPVCInfo

| Field | Type | Description |
|-------|------|-------------|
| `pvcName` | string | Name of the PVC |
| `snapshotName` | string | Name of the snapshot it was restored from |
| `restorationState` | string | State: `Pending`, `Restored`, or `Failed` |

### GitRevertStatus

| Field | Type | Description |
|-------|------|-------------|
| `commitSHA` | string | Commit SHA of the revert |
| `committedAt` | Time | When the revert was committed |

## Conditions

| Type | Description |
|------|-------------|
| `Suspended` | Flux Kustomization has been suspended |
| `WorkloadStopped` | Workload has been stopped (reason: `WorkloadStopped` or `Skipped` if no workloadRef) |
| `VolumesRestored` | PVCs have been restored from snapshots |
| `GitReverted` | Version has been reverted in Git |
| `Reconciled` | Flux has reconciled the changes |
| `Healthy` | Post-rollback health check passed |
| `Complete` | Rollback finished (check `reason` for success/failure) |

## Examples

### Basic Rollback

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: my-app-rollback
  namespace: default
spec:
  upgradeRequestRef:
    name: my-app-upgrade-20240115
```

### Dry Run

Validate a rollback without applying changes:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: test-rollback
  namespace: default
spec:
  upgradeRequestRef:
    name: my-app-upgrade-20240115
  dryRun: true
```

### Auto-Triggered Rollback

When a ManagedApp has `autoRollback: true` and an upgrade fails after the Git commit, FluxUp automatically creates a RollbackRequest like this:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: my-app-auto-rollback-20240115-143000
  namespace: default
  ownerReferences:
    - apiVersion: fluxup.dev/v1alpha1
      kind: UpgradeRequest
      name: my-app-upgrade-20240115
      uid: <upgrade-uid>
spec:
  upgradeRequestRef:
    name: my-app-upgrade-20240115
  autoTriggered: true
```

## Kubectl Commands

### List Rollbacks

```bash
kubectl get rollbackrequests
```

### Watch Progress

```bash
kubectl get rollbackrequest my-app-rollback -w
```

### Get Details

```bash
kubectl describe rollbackrequest my-app-rollback
```

### Check Conditions

```bash
kubectl get rollbackrequest my-app-rollback -o jsonpath='{.status.conditions}'
```

## Rollback Workflow

The rollback proceeds through these steps:

1. **Validate** - Check UpgradeRequest exists and is terminal (completed or failed)
2. **Suspend** - Suspend the Flux Kustomization to prevent reconciliation
3. **Stop Workload** - Scale workload to 0 replicas (if workloadRef configured)
4. **Restore Volumes** - Delete current PVCs and recreate from snapshots
5. **Revert Git** - Commit the previous version back to Git
6. **Resume Flux** - Resume the Kustomization to trigger reconciliation
7. **Health Check** - Wait for workload to become healthy

## Preconditions

A RollbackRequest requires:

1. **Terminal UpgradeRequest** - The referenced UpgradeRequest must be completed or failed (not in-progress)
2. **Snapshots Available** - The UpgradeRequest must have snapshot information (unless it had `skipSnapshot: true`)
3. **Previous Version Recorded** - The UpgradeRequest must have recorded the previous version

## Failure Handling

If a rollback fails:

- The `Complete` condition is set to `False` with a reason and message
- The Flux Kustomization remains in its current state (may be suspended)
- Manual intervention may be required depending on where the failure occurred

**Point of No Return:** PVC deletion is the critical step. If rollback fails after PVCs are deleted but before restoration completes, manual intervention is required.
