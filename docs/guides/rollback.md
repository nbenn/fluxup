---
title: Rollback Guide
description: How to rollback applications with FluxUp
---

# Rollback Guide

This guide explains how to rollback applications to their previous version using FluxUp.

## Overview

FluxUp rollbacks restore an application to its pre-upgrade state by:

1. Suspending Flux reconciliation
2. Scaling down workloads that mount the target PVCs
3. Deleting current PVCs and restoring them from pre-upgrade snapshots
4. Reverting the version in Git to the previous value
5. Resuming Flux and waiting for reconciliation and health checks

## When to Rollback

Consider a rollback when:

- An upgrade fails health checks
- The new version has bugs or performance issues
- You need to quickly restore service

## Creating a Rollback

### Manual Rollback

Create a RollbackRequest referencing the UpgradeRequest you want to rollback:

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

Apply it:

```bash
kubectl apply -f rollback.yaml
```

### Finding the UpgradeRequest Name

List recent upgrade requests:

```bash
kubectl get upgraderequests
```

Output:

```
NAME                      COMPLETE   REASON              MANAGEDAPP   AGE
my-app-upgrade-20240115   False      HealthCheckTimeout  my-app       30m
my-app-upgrade-20240110   True       UpgradeSucceeded    my-app       5d
```

### Auto-Rollback

For critical applications, enable automatic rollback when upgrades fail:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
spec:
  # ... other fields ...
  autoRollback: true
```

When enabled, if an upgrade fails **after the Git commit** (the point of no return), FluxUp automatically creates a RollbackRequest.

## Monitoring Progress

### Watch Status

```bash
kubectl get rollbackrequest my-app-rollback -w
```

Output as rollback progresses:

```
NAME              COMPLETE   REASON              UPGRADEREQUEST             AGE
my-app-rollback   <none>     <none>              my-app-upgrade-20240115    5s
my-app-rollback   True       RollbackSucceeded   my-app-upgrade-20240115    2m
```

### Check Conditions

```bash
kubectl describe rollbackrequest my-app-rollback
```

Key conditions to watch:

| Condition | Meaning |
|-----------|---------|
| `Suspended` | Flux Kustomization is suspended |
| `WorkloadStopped` | Workload scaled to 0 |
| `VolumesRestored` | PVCs restored from snapshots |
| `GitReverted` | Version reverted in Git |
| `Reconciled` | Flux has reconciled |
| `Healthy` | Application is healthy |
| `Complete` | Rollback finished |

### View Detailed Status

```bash
kubectl get rollbackrequest my-app-rollback -o yaml
```

Check the `status` section for:

- `restoredFrom` - What version was rolled back to
- `volumeRestore.restoredPVCs` - Which PVCs were restored
- `gitRevert.commitSHA` - The revert commit

## Rollback Without Snapshots

If the original upgrade was created with `skipSnapshot: true`, the rollback will:

- Skip the volume restore step
- Only revert the version in Git

This means data changes made by the new version will **not** be reverted.

## Dry Run

A dry run validates your rollback configuration and exercises the full suspend/scale cycle without making any Git or snapshot changes. This is useful for verifying that the rollback infrastructure works before you need it in an incident.

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: test-rollback
spec:
  upgradeRequestRef:
    name: my-app-upgrade-20240115
  dryRun: true
```

### What the Dry Run Does

**Preflight checks** (read-only cluster validation):

- Validates the UpgradeRequest exists and is terminal
- Verifies snapshots still exist and are ready (not garbage-collected)
- Validates the Flux suspend target exists and is a root Kustomization
- Checks Kustomization health (warns if not Ready)
- Discovers workloads that mount the target PVCs

**Git diff preview** (logged, no commit):

- Reads the current file from Git
- Computes what the version revert would look like
- Logs the current and target versions

**Quiescence cycle** (real cluster mutations):

- Suspends the Flux Kustomization
- Scales down discovered workloads to 0
- Verifies all pods terminated
- Scales workloads back up to original replica counts
- Resumes the Flux Kustomization

> **Important:** The dry run briefly takes the application offline during the quiescence cycle. Run dry runs during maintenance windows.

### Interpreting Results

A successful dry run sets the `Complete` condition with reason `DryRunSucceeded` and a summary message:

```
Dry run passed. Would rollback gitea from 2.0.0 to 1.0.0.
Preflight: Kustomization flux-system/gitea-ks Ready. 2 snapshots verified.
Suspend/resume: verified. Scale down/up: verified (StatefulSet/redis).
```

A failed dry run sets reason `DryRunFailed` with details about what went wrong.

## Troubleshooting

### Rollback Not Starting

**Check if UpgradeRequest is terminal:**

```bash
kubectl get upgraderequest my-app-upgrade-20240115 -o jsonpath='{.status.conditions[?(@.type=="Complete")]}'
```

Rollback requires the UpgradeRequest to be completed or failed (not in-progress).

**Check for snapshots:**

```bash
kubectl get upgraderequest my-app-upgrade-20240115 -o jsonpath='{.status.snapshot}'
```

If empty, the upgrade had `skipSnapshot: true` or no snapshots were configured.

### Volume Restore Failed

**Check PVC status:**

```bash
kubectl get pvc -n <namespace>
```

**Check for stuck PVCs:**

If a PVC is stuck in `Terminating`, pods may still be mounting it:

```bash
kubectl get pods -n <namespace>
```

**Check snapshot exists:**

```bash
kubectl get volumesnapshot -n <namespace>
```

### Git Revert Failed

**Check controller logs:**

```bash
kubectl logs -n fluxup-system deploy/fluxup-controller-manager | grep -i "git"
```

**Verify Git credentials:**

```bash
kubectl get secret fluxup-git-credentials -n fluxup-system
```

### Health Check Timeout

**Increase timeout** in ManagedApp:

```yaml
spec:
  healthCheck:
    timeout: "15m"
```

**Check workload status:**

```bash
kubectl get pods -n <namespace>
kubectl describe pod <name> -n <namespace>
```

### Rollback Failed After PVC Deletion

This is the most critical failure scenario. If rollback fails after PVCs are deleted but before restoration completes:

1. **Do not delete the RollbackRequest** - It contains the snapshot names needed for recovery
2. **Check the snapshot still exists:**
   ```bash
   kubectl get volumesnapshot -n <namespace>
   ```
3. **Manually restore the PVC:**
   ```yaml
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata:
     name: data-my-app-0
     namespace: my-app
   spec:
     accessModes: [ReadWriteOnce]
     resources:
       requests:
         storage: 10Gi
     dataSource:
       apiGroup: snapshot.storage.k8s.io
       kind: VolumeSnapshot
       name: my-app-data-my-app-0-pre-upgrade-20240115-143000
   ```

## Best Practices

1. **Run dry runs first** - Verify the suspend/scale cycle works before you need it in an incident
2. **Enable autoRollback for critical apps** - Reduces recovery time when upgrades fail
3. **Don't skip snapshots in production** - Snapshots enable data recovery
4. **Test rollbacks in staging** - Verify your rollback process works before you need it
5. **Monitor rollback duration** - Long rollbacks may indicate storage or reconciliation issues
6. **Keep enough snapshot retention** - Ensure `retentionPolicy.maxCount` allows rollback to recent versions

## Next Steps

- [UpgradeRequest Reference](../reference/upgraderequest.md) - Understand upgrade status
- [RollbackRequest Reference](../reference/rollbackrequest.md) - Full CRD specification
- [Configuration Guide](configuration.md) - Configure auto-rollback and snapshots
