# MVP Issues and Planned Fixes

This document tracks the remaining issues to address before MVP, with root cause analysis and planned solutions.

## Issue Summary

| # | Issue | Severity | Complexity |
|---|-------|----------|------------|
| 1 | PVC-centric workload discovery | High | High |
| 2 | Stale health check data | Medium | Low |
| 3 | Dry run finalizer leak | Low | Trivial |
| 4 | Timeout calculation bug | Medium | Low |

---

## Issue 1: PVC and Workload Auto-Discovery

### Problem

The current `workloadRef` model is broken for HelmRelease and fundamentally misaligned with the actual requirement.

**Symptoms:**
- `WorkloadRef.Kind` enum allows `Deployment;StatefulSet;HelmRelease`
- `workload.Scaler` only handles Deployment and StatefulSet
- HelmRelease fails with "unsupported workload kind" at runtime

**Why HelmRelease can't be "fixed" in the scaler:**
- HelmRelease is a Flux CRD that *creates* workloads - it has no replicas itself
- A single HelmRelease may create multiple Deployments/StatefulSets
- The StatefulSet name depends on Helm chart internals (often differs from HelmRelease name)

**The real requirement:**
We need to scale down workloads to:
1. Get crash-consistent snapshots (stop writes before snapshot)
2. Delete/restore PVCs (can't delete a mounted PVC)

The constraint is **PVC-based**, not workload-based. We must stop all pods that have the target PVCs mounted.

### Planned Solution

Two-phase auto-discovery:

1. **Discover RWO PVCs** belonging to the app (auto or explicit config)
2. **Discover workloads** that mount those PVCs (via ownerReferences)

#### Phase 1: PVC Discovery

Auto-discover all ReadWriteOnce PVCs belonging to the app. The source of truth depends on how the app is deployed:

**For HelmRelease-based apps:**
```
HelmRelease name
    │
    ▼
Helm release secret (sh.helm.release.v1.<name>.v<N>)
    │
    ▼
Decode (base64 → base64 → gzip → JSON)
    │
    ▼
Parse .manifest field → extract all Deployments/StatefulSets
    │
    ▼
Find Pods via ownerReferences
    │
    ▼
Extract PVCs from pod.spec.volumes, filter to RWO
```

The Helm secret is deterministic - it's how Helm itself tracks resources. Decoding is well-documented:
```go
secret.Data["release"] → base64 decode → base64 decode → gzip decompress → JSON
```

**For Kustomize-based apps:**
```
Kustomization (from kustomizationRef)
    │
    ▼
.status.inventory.entries (lists all managed resources)
    │
    ▼
Filter to Deployments/StatefulSets
    │
    ▼
Find Pods via ownerReferences
    │
    ▼
Extract PVCs from pod.spec.volumes, filter to RWO
```

Flux Kustomizations track managed resources in `.status.inventory`:
```yaml
status:
  inventory:
    entries:
    - id: default_my-app_apps_Deployment
      v: v1
    - id: default_my-sts_apps_StatefulSet
      v: v1
```

Both approaches are deterministic - no label heuristics.

**User override:**
```yaml
volumeSnapshots:
  enabled: true
  # Auto-discover RWO PVCs by default
  # Override with explicit list:
  pvcs:
    - name: only-this-one
  # Or exclude specific PVCs:
  excludePVCs:
    - name: cache-volume  # ephemeral, don't snapshot
```

#### Phase 2: Workload Discovery (from PVCs)

Once we have the PVC list, find workloads to scale:

```
PVC names to snapshot
    │
    ▼
List all Pods in namespace
    │
    ▼
Filter: pods with volumes referencing target PVCs
    │
    ▼
Extract ownerReferences → get Deployment/StatefulSet
    │
    ▼
Scale ALL discovered workloads to 0
    │
    ▼
Wait for ALL pods to terminate
    │
    ▼
Safe to snapshot/restore PVCs
```

Kubernetes `ownerReferences` is the standard mechanism for tracking ownership:
- StatefulSet → Pod (direct)
- Deployment → ReplicaSet → Pod

### Implementation

1. **New package:** `internal/discovery/`
   - `DiscoverPVCsFromHelmRelease(ctx, releaseName, namespace) ([]PVCInfo, error)`
   - `DiscoverPVCsFromKustomization(ctx, ksName, ksNamespace) ([]PVCInfo, error)`
   - `DiscoverWorkloadsForPVCs(ctx, pvcNames, namespace) ([]WorkloadInfo, error)`

2. **Update controllers:**
   - Before snapshot: auto-discover PVCs if not explicitly configured
   - Before scaling: discover workloads from PVC list
   - Scale ALL discovered workloads, track in status

3. **Simplify `workloadRef`:**
   - Remove from scaling logic entirely (auto-discovered from PVCs)
   - Keep for health checks only (or remove altogether)
   - Remove HelmRelease from enum

### Benefits

- Works identically for HelmRelease and raw Kustomize manifests
- User doesn't need to know internal workload/PVC names
- Handles multi-workload scenarios automatically
- Correct by construction - scales exactly what needs scaling
- Sensible defaults with opt-out for edge cases

---

## Issue 2: Health Check Architecture

### Problem

Three related issues with the current health check implementation:

1. **Stale data:** ManagedAppReconciler polls every 5 minutes to update `Ready` condition. Upgrade/rollback controllers read this condition, potentially seeing stale data.

2. **No shared health function:** Health check logic exists in ManagedAppReconciler but isn't reusable by other controllers.

3. **Undefined "healthy":** We haven't clearly defined what "healthy" means for a ManagedApp.

### Planned Solution

#### 1. Define "Healthy"

A ManagedApp is healthy when **both**:
- **Flux resource is Ready:** Kustomization or HelmRelease has `Ready=True` condition (manifests applied successfully)
- **Workloads are Ready:** All Deployments/StatefulSets have `readyReplicas == replicas` (pods running, passing probes)

This catches both Flux reconciliation failures (bad manifest) and application failures (crash loops, failed probes).

#### 2. Shared Health Check Function

Create a single `CheckHealth()` function used by:
- **ManagedAppReconciler:** Periodic updates for dashboard/status (informational)
- **UpgradeRequest/RollbackRequest controllers:** Real-time checks during operations (authoritative)

```go
// internal/health/check.go
func CheckHealth(ctx context.Context, c client.Client, app *v1alpha1.ManagedApp) (bool, error) {
    // 1. Check Flux resource (Kustomization or HelmRelease)
    fluxReady, err := checkFluxResourceReady(ctx, c, app)
    if err != nil || !fluxReady {
        return false, err
    }

    // 2. Check all workloads (discovered from Kustomization inventory or Helm manifest)
    workloads, err := discovery.DiscoverWorkloads(ctx, c, app)
    if err != nil {
        return false, err
    }

    for _, w := range workloads {
        if w.ReadyReplicas < w.DesiredReplicas {
            return false, nil
        }
    }

    return true, nil
}
```

#### 3. Polling Intervals

- **ManagedAppReconciler:** Reduce from 5 minutes to 15-30 seconds for dashboard freshness
- **Upgrade/Rollback controllers:** Call `CheckHealth()` directly for real-time status (no polling dependency)

---

## Issue 3: Dry Run Finalizer Leak

### Problem

UpgradeRequest `handleDryRun()` doesn't remove the finalizer, leaving orphaned finalizers that block deletion.

**Location:** `upgraderequest_controller.go:187-191`

**Comparison:** RollbackRequest correctly removes finalizer in dry run (`rollbackrequest_controller.go:182-188`)

### Planned Solution

Add finalizer removal to `handleDryRun()`:

```go
if err := r.Status().Update(ctx, upgrade); err != nil {
    return ctrl.Result{}, err
}

// Remove finalizer for dry run
if controllerutil.ContainsFinalizer(upgrade, OperationFinalizer) {
    controllerutil.RemoveFinalizer(upgrade, OperationFinalizer)
    if err := r.Update(ctx, upgrade); err != nil {
        return ctrl.Result{}, err
    }
}

return ctrl.Result{}, nil
```

---

## Issue 4: Per-Phase Timeouts

### Problem

Two related timeout issues:

1. **Shared timeout across phases:** Rollback uses `StartedAt` for all timeout calculations, but `StartedAt` is set when suspension begins. Slow operations consume the timeout budget before reaching later phases.

2. **Hardcoded timeouts too short:** PVC restore has a 5-minute timeout, but large volume restores can legitimately take 30+ minutes.

**Current timeouts (hardcoded):**
- PVC deletion: 5 min
- PVC bind (restore): 5 min per PVC
- Workload scale down: 5 min
- Reconciliation: 10 min (but measured from `StartedAt`)
- Health check: 5 min (but measured from `StartedAt`)

**Timeline showing the bug:**
```
T=0s      StartedAt set (suspension begins)
T=30s     Workload scaled down
T=5min    PVC restore times out (even if legitimately slower)
          OR if it succeeds quickly:
T=6min    Git revert completes
T=6min+   Reconciliation check: elapsed=6min from T=0
          Only 4 min left of the "10 min" budget!
```

### Planned Solution

Each phase gets its own independent timeout, reset at phase start. Applies to **both upgrade and rollback flows**.

**Upgrade phases:**
| Phase | Default Timeout | Notes |
|-------|----------------|-------|
| Suspend | 2 min | API call |
| ScaleDown | 5 min | Pod termination |
| Snapshot | 30 min | Volume size dependent |
| GitCommit | 2 min | API call |
| Reconcile | 10 min | Flux apply |
| HealthCheck | 5 min | Pod startup |

**Rollback phases:**
| Phase | Default Timeout | Notes |
|-------|----------------|-------|
| Suspend | 2 min | API call |
| ScaleDown | 5 min | Pod termination |
| VolumeRestore | 30 min | Volume size dependent |
| GitRevert | 2 min | API call |
| Reconcile | 10 min | Flux apply |
| HealthCheck | 5 min | Pod startup |

**Implementation:**
- Record `PhaseStartedAt` timestamp when entering each phase
- Each phase's timeout is measured from its own start time
- Consider making timeouts configurable in ManagedApp spec for edge cases (post-MVP)

**For volume operations (snapshot/restore):**
- Poll PVC/VolumeSnapshot status to check if operation is still in progress
- Only timeout if no progress is being made (stuck), not if it's legitimately slow

---

## MVP Checklist

### Code Fixes

- [ ] Issue 1: Implement PVC-centric workload discovery
- [ ] Issue 2: Direct workload health checks
- [ ] Issue 3: Fix dry run finalizer (trivial)
- [ ] Issue 4: Fix timeout calculation

### Testing

- [ ] Unit tests for PVC workload discovery
- [ ] E2E test with HelmRelease-based app
- [ ] E2E test with multi-workload scenario
- [ ] Manual test on real cluster

### Nice to Have (Post-MVP)

- [ ] Extract duplicate `getSuspendTarget`/`getSuspendRefStruct` to shared helper
- [ ] Fix comment numbering in controllers
