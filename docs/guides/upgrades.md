---
title: Triggering Upgrades
description: How to trigger and monitor application upgrades with FluxUp
---

# Triggering Upgrades

This guide explains how to trigger application upgrades, monitor their progress, and handle failures.

## Overview

FluxUp upgrades follow a safe workflow:

1. Suspend Flux reconciliation
2. Create pre-upgrade volume snapshots (if configured)
3. Commit version change to Git
4. Resume Flux reconciliation
5. Wait for health checks to pass

## Creating an Upgrade

### Basic Upgrade

To upgrade to the latest available version:

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

Apply with:

```bash
kubectl apply -f upgrade.yaml
```

### Upgrade to Specific Chart Version

To upgrade a HelmRelease to a specific chart version:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: gitea-upgrade
  namespace: default
spec:
  managedAppRef:
    name: gitea
  targetVersion:
    chart: "11.0.0"
```

### Upgrade to Specific Image Version

To upgrade an app to a specific image tag (requires `versionPath` in ManagedApp):

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

### Skip Snapshots

To skip pre-upgrade snapshots (not recommended for production):

```yaml
spec:
  managedAppRef:
    name: my-app
  skipSnapshot: true
```

### Dry Run

To validate an upgrade without applying changes:

```yaml
spec:
  managedAppRef:
    name: my-app
  dryRun: true
```

## Monitoring Progress

### Check Status

```bash
kubectl get upgraderequests
```

Output:

```
NAME              COMPLETE   REASON              MANAGEDAPP   AGE
my-app-upgrade    True       UpgradeSucceeded    my-app       5m
```

### Detailed Status

```bash
kubectl describe upgraderequest my-app-upgrade
```

### Watch Progress

```bash
kubectl get upgraderequest my-app-upgrade -w
```

## Understanding Status

### Conditions

FluxUp tracks progress through conditions:

| Condition | Meaning |
|-----------|---------|
| `Suspended` | Flux Kustomization has been paused |
| `SnapshotReady` | Pre-upgrade snapshots are ready |
| `GitCommitted` | Version change committed to Git |
| `Reconciled` | Flux has applied the changes |
| `Healthy` | Application passed health checks |
| `Complete` | Upgrade finished |

### Success

A successful upgrade shows:

```yaml
status:
  conditions:
    - type: Complete
      status: "True"
      reason: UpgradeSucceeded
      message: "Upgraded to 2.0.0"
```

### Failure

A failed upgrade shows:

```yaml
status:
  conditions:
    - type: Complete
      status: "False"
      reason: HealthCheckTimeout
      message: "Workload did not become healthy within timeout"
```

## Failure Handling

How FluxUp handles failures depends on when the failure occurs:

### Before Git Commit

If the upgrade fails during validation, suspend, or snapshot steps:

- Flux Kustomization is automatically resumed
- No changes are made to Git
- You can fix the issue and retry

### After Git Commit

If the upgrade fails during reconciliation or health checks:

- The new version is already in Git
- Flux Kustomization remains in current state
- A rollback is required (see [Rollback Guide](rollback.md))

The status message will indicate "rollback required":

```yaml
status:
  conditions:
    - type: Complete
      status: "False"
      reason: HealthCheckTimeout
      message: "Workload did not become healthy within timeout - rollback required"
```

## Troubleshooting

### Upgrade Not Starting

**Check if Git is configured:**

```bash
kubectl logs -n fluxup-system deploy/fluxup-controller-manager | grep -i git
```

If you see "UpgradeRequest controller disabled", configure Git settings. See [Configuration Guide](configuration.md#git-configuration).

**Check ManagedApp exists:**

```bash
kubectl get managedapp my-app
```

**Check for available update:**

```bash
kubectl get managedapp my-app -o jsonpath='{.status.availableUpdate}'
```

### Snapshot Creation Failed

**Verify VolumeSnapshotClass exists:**

```bash
kubectl get volumesnapshotclass
```

**Check PVCs exist and are bound:**

```bash
kubectl get pvc -n <namespace>
```

**Check CSI driver logs:**

```bash
kubectl logs -n kube-system -l app=csi-controller
```

### Git Commit Failed

**Check credentials:**

```bash
kubectl get secret fluxup-git-credentials -n fluxup-system
```

**Verify token has write access** to the repository.

**Check controller logs:**

```bash
kubectl logs -n fluxup-system deploy/fluxup-controller-manager | grep -i "git"
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
kubectl get helmrelease -n <namespace>
kubectl describe helmrelease <name> -n <namespace>
```

**Check pod status:**

```bash
kubectl get pods -n <namespace>
kubectl describe pod <name> -n <namespace>
```

## Next Steps

- [Configuration Guide](configuration.md) - Configure Git backend and snapshots
- [UpgradeRequest Reference](../reference/upgraderequest.md) - Full CRD specification
