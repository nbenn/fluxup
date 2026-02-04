# MVP Issues and Resolution

This document tracked the remaining issues addressed before MVP. All issues have been resolved.

## Issue Summary

| # | Issue | Severity | Status |
|---|-------|----------|--------|
| 1 | PVC-centric workload discovery | High | Resolved |
| 2 | Stale health check data | Medium | Resolved |
| 3 | Dry run finalizer leak | Low | Resolved |
| 4 | Timeout calculation bug | Medium | Resolved |

---

## Issue 1: PVC and Workload Auto-Discovery

### Problem

The original `workloadRef` model was broken for HelmRelease and fundamentally misaligned with the actual requirement.

**Symptoms:**
- `WorkloadRef.Kind` enum allowed `Deployment;StatefulSet;HelmRelease`
- `workload.Scaler` only handled Deployment and StatefulSet
- HelmRelease failed with "unsupported workload kind" at runtime

**Why HelmRelease couldn't be "fixed" in the scaler:**
- HelmRelease is a Flux CRD that *creates* workloads - it has no replicas itself
- A single HelmRelease may create multiple Deployments/StatefulSets
- The StatefulSet name depends on Helm chart internals (often differs from HelmRelease name)

**The real requirement:**
We need to scale down workloads to:
1. Get crash-consistent snapshots (stop writes before snapshot)
2. Delete/restore PVCs (can't delete a mounted PVC)

The constraint is **PVC-based**, not workload-based. We must stop all pods that have the target PVCs mounted.

### Resolution

Implemented two-phase auto-discovery in `internal/discovery/`:

1. **Discover RWO PVCs** belonging to the app (auto or explicit config)
2. **Discover workloads** that mount those PVCs (via ownerReferences)

**For HelmRelease-based apps** (`internal/discovery/helm.go`):
- Finds Helm release secret (`sh.helm.release.v1.<name>.v<N>`)
- Decodes with correct sequence: base64 → base64 → gzip → JSON
- Parses manifest for Deployments/StatefulSets
- Finds PVCs via pod ownership chains

**For Kustomize-based apps** (`internal/discovery/kustomization.go`):
- Uses Kustomization's `.status.inventory.entries`
- Filters to Deployments/StatefulSets
- Finds PVCs via pod ownership chains

**API change:**
- Removed `workloadRef` field from ManagedAppSpec
- Added `helmReleaseRef` for HelmRelease discovery
- Workloads are now auto-discovered based on PVC mounts

---

## Issue 2: Health Check Architecture

### Problem

Three related issues with the original health check implementation:

1. **Stale data:** ManagedAppReconciler polled every 5 minutes to update `Ready` condition. Upgrade/rollback controllers read this condition, potentially seeing stale data.

2. **No shared health function:** Health check logic existed in ManagedAppReconciler but wasn't reusable by other controllers.

3. **Undefined "healthy":** We hadn't clearly defined what "healthy" means for a ManagedApp.

### Resolution

**Defined "Healthy"** (`internal/health/check.go`):

A ManagedApp is healthy when **both**:
- **Flux resource is Ready:** Kustomization or HelmRelease has `Ready=True` condition
- **Workloads are Ready:** All Deployments/StatefulSets have `readyReplicas >= desiredReplicas` (or `desiredReplicas=0`)

**Shared Health Check Function:**

Created `health.Checker` with `CheckHealth()` function used by:
- **ManagedAppReconciler:** Periodic updates for dashboard/status
- **UpgradeRequest/RollbackRequest controllers:** Real-time checks during operations

Both upgrade and rollback controllers now use direct health checks rather than reading potentially stale conditions.

---

## Issue 3: Dry Run Finalizer Leak

### Problem

UpgradeRequest `handleDryRun()` didn't remove the finalizer, leaving orphaned finalizers that blocked deletion.

RollbackRequest correctly removed finalizer in dry run.

### Resolution

Added finalizer removal to `handleDryRun()` in `upgraderequest_controller.go`:

```go
// Remove finalizer for dry run
if controllerutil.ContainsFinalizer(upgrade, OperationFinalizer) {
    controllerutil.RemoveFinalizer(upgrade, OperationFinalizer)
    if err := r.Update(ctx, upgrade); err != nil {
        return ctrl.Result{}, err
    }
}
```

Both UpgradeRequest and RollbackRequest now consistently remove finalizers in dry run mode.

---

## Issue 4: Per-Phase Timeouts

### Problem

Two related timeout issues:

1. **Shared timeout across phases:** Both controllers used a single `StartedAt` timestamp for all timeout calculations, but `StartedAt` was set when suspension began. Slow operations consumed the timeout budget before reaching later phases.

2. **Hardcoded timeouts too short:** PVC restore had a 5-minute timeout, but large volume restores can legitimately take 30+ minutes.

### Resolution

Implemented per-phase timeout tracking:

**Added `PhaseStartedAt` field** to both UpgradeRequestStatus and RollbackRequestStatus.

**Phase timeout management** in both controllers:
- `setPhaseStartIfNeeded()` - Records when entering a phase
- `resetPhaseStart()` - Clears timestamp when phase completes
- `isPhaseTimedOut()` - Checks if current phase exceeded its timeout

**Timeout constants** (with appropriate values for each phase):

| Phase | Timeout | Notes |
|-------|---------|-------|
| Suspend | 2 min | API call |
| ScaleDown | 5 min | Pod termination |
| Snapshot/VolumeRestore | 30 min | Volume size dependent |
| GitCommit/GitRevert | 2 min | API call |
| Reconcile | 10 min | Flux apply |
| HealthCheck | 5 min | Pod startup (configurable via ManagedApp) |

Each phase now has independent timeout tracking, preventing slow early phases from consuming the budget for later phases.

---

## MVP Checklist

### Code Fixes

- [x] Issue 1: Implement PVC-centric workload discovery
- [x] Issue 2: Direct workload health checks
- [x] Issue 3: Fix dry run finalizer
- [x] Issue 4: Fix per-phase timeout calculation

### Testing

- [x] Unit tests for PVC discovery (`internal/discovery/*_test.go`)
- [x] Unit tests for health checks (`internal/health/check_test.go`)
- [x] Unit tests cover multi-workload scenarios (multiple Deployments/StatefulSets)
- [x] E2E test with HelmRelease-based app (basic controller and CRD tests)
- [x] Full E2E upgrade workflow test (`test/e2e/upgrade_workflow_test.go`)
  - Requires: `make test-e2e-full` (installs Flux + Gitea in Kind)
  - Tests: ManagedApp creation, dry-run upgrade, real upgrade with Git commit
- [ ] Manual test on real cluster

### Documentation

- [x] Update user documentation (removed `workloadRef`, added `helmReleaseRef`)
- [x] Update CRD reference documentation
- [x] Update sample YAML files
