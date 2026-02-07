# MVP Issues and Resolution

This document tracks the key issues addressed before MVP and the current testing strategy.

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

## Testing Strategy

### Test Infrastructure

The project has 4 test suites:

1. **Unit tests** (`internal/*_test.go`) - Fast, pure Go tests with mocks
2. **Git integration tests** (`internal/git/*_integration_test.go`) - In-memory git operations using go-git
3. **K8s integration tests** (`test/k8s/`) - Real Kind cluster with envtest
4. **E2E tests** (`test/e2e/`) - Full stack (Kind + Flux + Gitea)

### Current Coverage

**Overall: 61.5%** (+2.7% from recent improvements)

*Note: Coverage includes unit tests + git integration tests. K8s integration tests verify controller deployment but don't collect coverage. E2E tests would provide additional coverage but require Git configuration fixes.*

| Package | Coverage | Status |
|---------|----------|--------|
| **internal/snapshot** | **93.6%** | ✅ **Excellent** (was 53.9%, +39.7%) |
| internal/health | 91.7% | ✅ Excellent |
| internal/renovate | 89.9% | ✅ Excellent |
| internal/flux | 83.8% | ✅ Good |
| internal/discovery | 83.5% | ✅ Good |
| internal/yaml | 79.3% | ✅ Good |
| internal/controller | 56.1% | ⚠️ Needs work |
| internal/workload | 42.8% | ❌ Critical gap |
| internal/git | 38.6% | ❌ Critical gap |
| internal/logging | 15.0% | ❌ Critical gap |

### Well-Tested Areas

**UpgradeRequest Controller** (21 unit tests):
- ✅ Happy path: dry-run → suspend → commit → reconcile → health check → complete
- ✅ Error scenarios: ManagedAppNotFound, NoUpdateAvailable, GitCommitFailed
- ✅ Auto-rollback logic: timeout triggers rollback, health check failures
- ✅ Image updates with version path validation
- ✅ Finalizer lifecycle and cleanup
- ✅ Flux external resume detection

**RollbackRequest Controller** (12 unit tests):
- ✅ Happy path: dry-run → suspend → volume restore → git revert → reconcile → complete
- ✅ Error scenarios: UpgradeRequestNotFound, NoSnapshotsAvailable, GitRevertFailed
- ✅ Skip snapshot when `skipSnapshot=true`
- ✅ Timeout handling at multiple phases
- ✅ Finalizer lifecycle

**E2E Failure Scenarios** (16 test cases):
- ✅ Concurrent upgrade requests
- ✅ Phase timeouts
- ✅ Resource deletion during operations
- ✅ Finalizer cleanup
- ✅ External interference (un-suspend)
- ✅ Pre-commit failures
- ✅ Rollback scenarios

**Integration Tests**:
- ✅ Git operations (concurrent commits, merge conflicts, large files, auth failures)
- ✅ CSI snapshots (creation, restoration, deletion, multiple snapshots)

### Critical Gaps

#### 1. Asynchronous Wait Operations

**Workload Scaling** (all at 0%):
- `WaitForScaleDown` - Async waiting for pods to terminate
- `WaitForScaleUp` - Async waiting for pods to become ready
- `getReadyReplicas` - Status checking
- `getWorkloadPods` - Pod discovery
- `IsScaledDown` - State verification

**Snapshot Management** (all at 0%):
- `WaitForSnapshotReady` - Async snapshot completion
- `WaitForPVCBound` - PVC binding verification
- `DeletePVC` - PVC cleanup
- `WaitForPVCDeleted` - Async deletion confirmation
- `IsPVCDeleted` / `IsPVCBound` - Status checks

**Impact**: Cannot verify timeout handling, error conditions, or proper async operations.

**Fix**: Add K8s integration tests that exercise these wait operations with real pods and PVCs.

#### 2. Workload Discovery

**Untested functions**:
- `DiscoverWorkloads` (0%) - Main workload discovery from Kustomization inventory
- `DiscoverPVCs` (20%) - PVC discovery from workload specs

**Impact**: Cannot verify discovery works for all workload types or edge cases.

**Fix**: Add K8s tests with StatefulSets and Deployments with various PVC configurations.

#### 3. Controller Setup Functions

All `SetupWithManager` functions are at 0% coverage. These are controller-runtime boilerplate that are implicitly tested by E2E tests running the controllers.

**Priority**: Low - these are framework registration code.

#### 4. E2E Test Configuration

**Status**: E2E tests exist but currently fail due to Git configuration.

**Problem**: 
- UpgradeRequest/RollbackRequest controllers require Git credentials (GIT_BACKEND, GIT_REPO_URL, GIT_TOKEN)
- Controllers log: "UpgradeRequest and RollbackRequest controllers disabled (Git not configured)"
- E2E tests create UpgradeRequest CRs which fail because controllers are disabled

**Attempted Fix**:
- Modified test BeforeAll to read Git token from .env file
- Added kubectl patch to inject GIT_* environment variables
- Added rollout wait to ensure pod restarts with new config

**Status**: Fixes implemented in test files, need validation.

**Impact if fixed**: +5-10% coverage by enabling Phase 2 controller testing.

### Test Improvements Completed

#### Phase 1: Critical Safety Nets ✅

**Flux Validation Methods** (+33.7% to flux package):
- `IsManagedByKustomization` - Prevents hierarchical conflicts
- `ValidateSuspendTarget` - Validates suspend/resume targets
- `VerifyStillSuspended` - Prevents external resume during operations

**Snapshot Integrity** (+11.4% to snapshot package):
- Orphaned snapshot handling
- Snapshot deletion edge cases
- Partial failure cleanup
- Concurrent snapshots

**Git Commit Messages** (+4.7% to git package):
- Conventional commits format compliance
- Revert message formatting
- Special character handling

#### Phase 2: E2E Failure Scenarios ✅

Added 10 new E2E test cases:
- Concurrent operations
- Timeout scenarios
- Phase transition failures (resource deletion during ops)
- Finalizer cleanup validation
- External interference detection

#### Phase 3a: Integration Tests ✅

**Git Operations** (13 tests, +105ms CI time):
- Concurrent commits with proper locking
- Merge conflict detection
- Large file handling (>1MB YAML)
- Auth token expiration
- Commit retry with backoff
- Conventional commits validation

**CSI Snapshots** (7 tests):
- Snapshot creation and readiness
- PVC restoration from snapshot
- Snapshot deletion
- Multiple snapshots
- Snapshot class selection
- Partial failure handling

### Test Improvements Completed

**Snapshot Wait Operation Tests** ✅
- Location: `internal/snapshot/wait_test.go`
- 23 comprehensive unit tests for all async wait operations
- Covers: `WaitForSnapshotReady`, `WaitForPVCBound`, `WaitForPVCDeleted`, `IsPVCBound`, `IsPVCDeleted`, `DeletePVC`
- Tests timeout scenarios, error conditions, state transitions
- **Impact**: Snapshot coverage 53.9% → 93.6% (+39.7%)
- **Overall impact**: Project coverage 58.8% → 61.5% (+2.7%)

### Priority Gaps Remaining

**High Impact** (all at 0% coverage):

1. **Workload async operations** - 5 functions in internal/workload/scaler.go
   - `WaitForScaleDown`, `WaitForScaleUp`, `getReadyReplicas`, `getWorkloadPods`, `IsScaledDown`
   - **Why critical**: Essential for safe upgrade/rollback - ensures workloads stopped before snapshots
   - **Blocker**: Requires K8s integration tests with real Deployments/StatefulSets
   - **Effort**: 1-2 days

2. **Workload discovery** - 1 function in internal/discovery/discovery.go
   - `DiscoverWorkloads` - finds workloads from Kustomization inventory
   - **Why critical**: Core functionality that powers the entire discovery mechanism
   - **Effort**: 1 day (K8s integration tests)

3. **Phase 2 upgrade/rollback handlers** - 3 functions in controllers
   - `handleStopWorkload`, `handleVolumeRestore`, `applySnapshotRetention`
   - **Why critical**: Core upgrade/rollback functionality
   - **Blocker**: E2E tests need Git configuration validated
   - **Effort**: 1-2 hours to validate E2E fixes

**Low Priority** (infrastructure/boilerplate):
- Controller `SetupWithManager` (4 functions) - framework code, implicitly tested
- Git mock methods (9 functions) - test infrastructure
- Logging constructors (4 functions) - infrastructure
- `flux.WaitForReconciliation` - edge case

### Expected Coverage After Improvements

| Area | Current | After High Priority | Target |
|------|---------|---------------------|--------|
| Overall | 58.8% | 68-73% | 70%+ |
| internal/controller | 56.1% | 65-70% | 70%+ |
| internal/snapshot | 53.9% | 65-70% | 70%+ |
| internal/workload | 42.8% | 60-65% | 65%+ |
| internal/git | 38.6% | 45-50% | 50%+ |

### Quality Metrics

Rather than focusing solely on line coverage, the emphasis is on:

- **Error path coverage** - All error returns tested
- **Phase transition coverage** - All state combinations tested
- **Async operation coverage** - All Wait* methods tested with timeouts
- **Data integrity coverage** - Snapshot/restore validated with real data
- **Concurrent operation coverage** - Critical sections tested under load

### CI/CD Strategy

**On every commit:**
- Unit tests (~1s)

**On every PR:**
- Unit tests
- Integration tests (~30s)

**On PR merge to main:**
- Unit + Integration + E2E tests (~5min)

**Nightly/on-demand:**
- Stress tests with high iteration counts
- Optional: Chaos tests (currently deferred)

### Testing Methodology

**Concurrency Testing:**
- Controlled race conditions using channels/sync primitives
- Controller-runtime concurrent harness with envtest
- Inject delays to widen race windows
- Stress testing with `-race -count=1000`

**Snapshot/Restore Testing:**
- Fake CSI driver for fast unit tests with failure injection
- Real CSI driver (hostpath) for data integrity validation
- Storage class edge cases and quota exhaustion scenarios

**Git Operations Testing:**
- In-memory Git (go-git) for fast, deterministic tests
- Controlled timing to expose race windows
- Real Gitea for E2E workflows

### Assessment

**Strengths:**
- Strong foundation (58.8% coverage)
- Core utility packages >80% coverage
- Comprehensive E2E failure scenarios
- Integration tests for critical operations
- Well-tested controller reconciliation logic

**Main Gap:**
- Asynchronous operations (scaling waits, snapshot waits)
- These require K8s integration tests with real resources

**Blocker:**
- E2E tests need validation (fixes implemented, ready to test)

With E2E fixes validated + async operation tests added, the project will have **strong test coverage across all critical paths** with emphasis on real-world failure scenarios.

### Deployment Pattern Coverage Gap

**Problem**: All E2E and controller unit tests exclusively use HelmRelease-based deployments.

The project supports two fundamental Flux deployment patterns:
1. **HelmRelease** - Helm charts managed by Flux's helm-controller
2. **Kustomization** - Raw manifests (Deployments, StatefulSets, etc.) managed by Flux's kustomize-controller

**Current State**:

| Test Area | HelmRelease | Kustomization |
|-----------|-------------|---------------|
| Discovery unit tests | ✅ 10 tests | ✅ 7 tests |
| PVC/workload detection | ✅ | ✅ |
| E2E upgrade workflow | ✅ | ❌ |
| E2E rollback workflow | ✅ | ❌ |
| E2E failure scenarios | ✅ 10+ tests | ❌ |
| Controller unit tests | ✅ | ❌ |
| Version path customization | ❌ (uses default) | ❌ |

All E2E tests use:
```yaml
gitPath: "flux/apps/gitea/helmrelease.yaml"  # Always HelmRelease
```

**Why This Matters**:

For Kustomization-based deployments:
- Version changes are in different locations (e.g., image tags in Deployment specs, kustomization overlays)
- The `versionPath` field exists precisely to support this, but is never tested with non-default values
- Discovery uses Kustomization inventory instead of Helm release secrets

**Gaps to Address**:

1. **Kustomization E2E tests** - Mirror existing E2E tests but with raw Deployment/StatefulSet manifests
2. **Custom versionPath tests** - Test version updates at non-standard YAML paths (e.g., `.spec.template.spec.containers[0].image`)
3. **Mixed scenario tests** - ManagedApp with both `helmReleaseRef` and `kustomizationRef` set
4. **Comparative discovery tests** - Verify both patterns discover identical workloads/PVCs for equivalent deployments

**Effort Estimate**: 2-3 days

**Priority**: High - This is a fundamental feature gap, not just coverage improvement.

#### Implementation Plan

##### Phase 1: Test Fixtures (Day 1 morning)

Add Kustomization-based test manifests to `.devcontainer/test-infra/manifests/`:

```yaml
# redis-deployment.yaml - Raw Deployment with image tag versioning
# (Mirrors redis-helmrelease.yaml but as raw manifest for comparison testing)
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis-raw
  namespace: redis
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis-raw
  template:
    metadata:
      labels:
        app: redis-raw
    spec:
      containers:
      - name: redis
        image: redis:7.2.0  # Version to be updated via versionPath
        ports:
        - containerPort: 6379
```

```yaml
# redis-statefulset.yaml - StatefulSet with PVC for snapshot testing
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: redis-persistent
  namespace: redis
spec:
  serviceName: redis-persistent
  replicas: 1
  selector:
    matchLabels:
      app: redis-persistent
  template:
    metadata:
      labels:
        app: redis-persistent
    spec:
      containers:
      - name: redis
        image: redis:7.2.0  # Version to be updated
        ports:
        - containerPort: 6379
        volumeMounts:
        - name: data
          mountPath: /data
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 1Gi
```

Update `.devcontainer/test-infra/e2e/seed-repo.sh` to include these manifests:
- Add `flux/apps/redis-raw/deployment.yaml`
- Add `flux/apps/redis-persistent/statefulset.yaml`
- Update `flux/kustomization.yaml` to include new resources

This gives us a nice comparison: Redis deployed via HelmRelease (`redis-helmrelease.yaml`) vs Redis deployed via raw Deployment (`redis-deployment.yaml`).

##### Phase 2: E2E Test File (Day 1 afternoon)

Create `test/e2e/kustomization_workflow_test.go`:

```go
var _ = Describe("Kustomization-based Upgrade Workflow", Ordered, func() {
    // Similar structure to upgrade_workflow_test.go but with:

    Context("with raw Deployment manifest", func() {
        It("should upgrade image tag via custom versionPath", func() {
            // ManagedApp with:
            //   gitPath: "flux/apps/redis-raw/deployment.yaml"
            //   versionPolicy:
            //     versionPath: ".spec.template.spec.containers[0].image"
            //
            // UpgradeRequest targeting "redis:7.4.0"
            // Verify: Git commit updates image tag, Flux reconciles, Deployment runs new image
        })
    })

    Context("with StatefulSet and PVC", func() {
        It("should discover workloads from Kustomization inventory", func() {
            // ManagedApp with:
            //   gitPath: "flux/apps/redis-persistent/statefulset.yaml"
            //   kustomizationRef: (points to Kustomization managing the StatefulSet)
            //   versionPolicy:
            //     versionPath: ".spec.template.spec.containers[0].image"
            //
            // Verify: Controller discovers StatefulSet via Kustomization inventory
            // Verify: PVC discovery works (for snapshot scenarios)
        })

        It("should upgrade StatefulSet with snapshot", func() {
            // Full upgrade flow with skipSnapshot: false
            // Verify: Snapshot created, workload scaled down, upgrade applied, workload scaled up
        })
    })
})
```

##### Phase 3: Controller Unit Tests (Day 2 morning)

Add Kustomization scenarios to `internal/controller/upgraderequest_unit_test.go`:

```go
func TestUpgradeRequest_KustomizationBasedDeployment(t *testing.T) {
    // Test upgrade with:
    // - gitPath pointing to a Deployment YAML (not HelmRelease)
    // - Custom versionPath: ".spec.template.spec.containers[0].image"
    // - No helmReleaseRef (discovery via Kustomization inventory)
}

func TestUpgradeRequest_CustomVersionPath(t *testing.T) {
    // Test various versionPath configurations:
    // - Image tag: ".spec.template.spec.containers[0].image"
    // - Annotation: ".metadata.annotations['app.kubernetes.io/version']"
    // - Nested: ".spec.template.spec.initContainers[0].image"
}
```

Add to `internal/controller/rollbackrequest_unit_test.go`:

```go
func TestRollbackRequest_KustomizationBasedDeployment(t *testing.T) {
    // Test rollback for Kustomization-managed workloads
}
```

##### Phase 4: Discovery Comparison Tests (Day 2 afternoon)

Create `internal/discovery/equivalence_test.go`:

```go
func TestDiscovery_EquivalentResults(t *testing.T) {
    // Given: A HelmRelease that deploys a Deployment with a PVC
    // And: An equivalent raw Deployment manifest managed by Kustomization
    //
    // When: DiscoverWorkloads is called for each
    // Then: Both return the same workload info
    //
    // When: DiscoverPVCs is called for each
    // Then: Both return the same PVC info
}
```

##### Phase 5: Failure Scenario Coverage (Day 3)

Add Kustomization variants to key failure scenarios in `test/e2e/failure_scenarios_test.go`:

```go
Context("Kustomization-based failure scenarios", func() {
    It("should handle timeout for Kustomization deployment", func() {
        // Same as existing timeout test but with Deployment manifest
    })

    It("should resume Kustomization when upgrade fails before Git commit", func() {
        // Same as existing pre-commit failure test
    })

    It("should handle rollback for Kustomization deployment", func() {
        // Rollback scenario with raw manifests
    })
})
```

##### Test Matrix Summary

| Scenario | HelmRelease | Kustomization | Notes |
|----------|-------------|---------------|-------|
| Basic upgrade (dry-run) | ✅ existing | ✅ Phase 2 (E2E) | `kustomization_workflow_test.go` |
| Basic upgrade (real) | ✅ existing | ✅ Phase 2 (E2E) | |
| Custom versionPath | ✅ Phase 3 | ✅ Phase 3 | `upgraderequest_unit_test.go` |
| Workload discovery | ✅ existing | ✅ Phase 2 | Via inventory |
| PVC discovery | ✅ existing | ✅ Phase 2 | Via inventory |
| Snapshot + upgrade | ✅ existing | ✅ Phase 2 (E2E) | With StatefulSet |
| Rollback | ✅ existing | 🔲 Phase 5 | Needs E2E tests |
| Timeout handling | ✅ existing | 🔲 Phase 5 | Needs E2E tests |
| Pre-commit failure | ✅ existing | 🔲 Phase 5 | Needs E2E tests |
| Discovery equivalence | ✅ Phase 4 | ✅ Phase 4 | `equivalence_test.go` |

##### Implementation Progress

- **Phase 1 ✅**: Test fixtures created (`redis-deployment.yaml`, `redis-statefulset.yaml`, updated `seed-repo.sh`)
- **Phase 2 ✅**: E2E test file created (`test/e2e/kustomization_workflow_test.go`)
- **Phase 3 ✅**: Controller unit tests with custom versionPath (`internal/controller/upgraderequest_unit_test.go`)
- **Phase 4 ✅**: Discovery equivalence tests (`internal/discovery/equivalence_test.go`)
- **Phase 5 🔲**: Failure scenario E2E tests - requires devcontainer environment

**Additional fixes made during implementation:**
- Fixed YAML editor `parseVersionPath` to handle bracket notation (e.g., `annotations['app.kubernetes.io/version']`)

---

## Future Testing Enhancements

**Auto-rollback on health check failure**: Test scenario removed due to complexity. Requires:
- HelmRelease that can be upgraded but deploys failing pods (e.g., invalid image)
- Auto-rollback configuration on ManagedApp
- Validation of automatic rollback trigger and execution

This scenario should be implemented once more sophisticated test fixtures are available.

---

## Enhanced Dry-Run and Preflight Checks

### Problem

The current dry-run implementation is shallow. For upgrades, it checks that the ManagedApp exists, an update is available, and the Git file is readable. For rollbacks, it checks that the UpgradeRequest exists, is terminal, has snapshots, and has a previous version recorded. Neither goes deeper into the operation pipeline.

This means operations — dry-run *or real* — can't catch issues early like:
- The version change would produce invalid YAML
- The Flux Kustomization doesn't exist or isn't healthy
- The VolumeSnapshotClass doesn't exist
- No workloads mount the PVCs (nothing to scale down)
- Rollback snapshots have been garbage-collected since the upgrade

Users also get no visibility into *what* the operation would actually do — there's no preview of the Git change. And the dry-run doesn't exercise the most critical (and least trusted) part of the pipeline: the suspend → scale-down → scale-up → resume cycle.

### Goals

1. **Preflight checks for all operations** — validate infrastructure prerequisites before any mutations, whether dry-run or real
2. **Git diff preview** — show what the version change would look like before committing
3. **Active verification in dry-run** — exercise the full suspend and scale-down/up cycle, confirming the quiescence path works end-to-end

### Design Principles

- **Preflight runs always** — read-only cluster validation happens before branching into dry-run vs real. A real upgrade that would fail because the VolumeSnapshotClass doesn't exist should fail fast with a clear `PreflightFailed` reason, not halfway through the pipeline with a cryptic CSI error.
- **Dry-run exercises mutations** — the suspend → scale-down → scale-up → resume cycle is real state mutation. This is intentional. Discovering a suspend/resume or scaling problem during a dry-run means you fix it and retry. Discovering it mid-snapshot-restore means you're in an incident. Dry-runs should therefore be performed during maintenance windows.
- **No new API fields** — diff, preflight, and verification results go to controller log output and the condition message summary.
- **Reuse existing helpers** — all required functionality already exists in the codebase.

---

### Enhancement 1: Infrastructure Preflight Checks (All Operations)

Preflight checks run for **every** operation — dry-run and real. They are read-only cluster queries that validate the prerequisites for each pipeline phase. They execute after initial validation (ManagedApp exists, update available, etc.) but before any mutations.

#### Flux Preflight (both upgrade and rollback)

**Suspend target validation** — via `FluxHelper.ValidateSuspendTarget()`:

Checks two things:
1. The Kustomization referenced by `kustomizationRef` (or `suspendRef`) actually exists on the cluster
2. If no explicit `suspendRef` is set, the `kustomizationRef` Kustomization is a "root" — not managed by a parent Kustomization via ownerReferences or Flux labels

The second check matters because suspending a child Kustomization is unsafe — the parent can un-suspend it during the operation. Today this check only runs at the start of `handleSuspend()`; moving it into preflight catches misconfigured `suspendRef` / app-of-apps patterns before any state is touched.

Failure mode: fail with reason `PreflightFailed` (or `InvalidSuspendTarget`).

**Kustomization health** — via `FluxHelper.IsReconciled()`:

Check whether the Kustomization has `Ready=True`. Upgrading or rolling back an app whose Kustomization is already in a failed state is risky — the reconciliation phase will likely time out regardless of the version change.

Failure mode: warn in log. This is a warning rather than a hard failure because there are legitimate reasons to proceed (e.g., the upgrade itself fixes the issue). The log message should make it clear: `"WARNING: Kustomization flux-system/gitea-ks is not Ready — operation may fail at reconciliation"`.

**Kustomization currently suspended** — via `FluxHelper.IsSuspended()`:

If the Kustomization is already suspended (e.g., someone manually suspended it, or a previous failed operation left it suspended), the real operation's `SuspendKustomization()` call will succeed silently (it's a no-op when already suspended). But resuming after the operation may have unintended consequences if the suspension was intentional.

Failure mode: warn in log. `"WARNING: Kustomization flux-system/gitea-ks is already suspended — operation will resume it upon completion"`.

#### Snapshot Preflight (upgrade only, when snapshots enabled)

Only runs when `!skipSnapshot && volumeSnapshots.enabled`. Validates that snapshot infrastructure will work when the real operation reaches `handleSnapshotting()`.

**PVC discovery** — via `Discoverer.DiscoverPVCs()`:

Runs the same discovery logic the real operation uses: explicit PVC list from config, or auto-discovery from HelmRelease / Kustomization inventory, filtered to RWO only and with exclusions applied.

This catches:
- HelmRelease doesn't exist or has no release secret (Helm discovery fails)
- Kustomization has no inventory entries (Kustomization discovery fails)
- No RWO PVCs exist (nothing to snapshot — not an error, but informative)
- Explicitly configured PVCs don't exist on the cluster

Failure mode: discovery errors fail with reason `PreflightFailed`. "No PVCs found" is logged as info.

Log output: `"preflight: discovered N PVCs to snapshot: [data-redis-0, data-postgres-0]"` or `"preflight: no RWO PVCs discovered — snapshots will be skipped"`.

**Workload discovery** — via `Discoverer.DiscoverWorkloads()`:

Discovers Deployments/StatefulSets that mount the discovered PVCs. In the real operation, these workloads are scaled to zero before snapshotting to ensure crash-consistent snapshots.

This catches:
- Pods mounting PVCs aren't owned by a Deployment or StatefulSet (can't be scaled)
- Workloads are in a different namespace than expected

Failure mode: discovery errors fail with reason `PreflightFailed`. "No workloads found" is logged as info (the PVC might be unmounted, which is fine for snapshotting).

Log output: `"preflight: N workloads to scale: [StatefulSet/redis, Deployment/worker]"`.

**VolumeSnapshotClass validation** — via `client.Get()` on VolumeSnapshotClass:

If `volumeSnapshots.volumeSnapshotClassName` is configured in the ManagedApp, verify the VolumeSnapshotClass exists on the cluster. This is a new check — the real operation currently fails at `SnapshotManager.CreateSnapshot()` time when the CSI driver rejects the snapshot request with a non-existent class.

If no class is configured, skip this check (the CSI driver uses its default class).

Failure mode: fail with reason `PreflightFailed` (`VolumeSnapshotClassNotFound`).

#### Snapshot Restore Preflight (rollback only, when snapshots present)

Only runs when the referenced UpgradeRequest has snapshots in `status.snapshot.pvcSnapshots`. Validates that the restore path in `handleVolumeRestore()` will work.

**Snapshot existence and readiness** — via `SnapshotManager.IsSnapshotReady()` per snapshot:

The current rollback dry-run checks that the *list* of snapshots is non-empty in the UpgradeRequest status. But it doesn't verify the actual VolumeSnapshot objects still exist on the cluster. Between the upgrade and the rollback:
- Snapshots may have been garbage-collected by retention policy
- Snapshots may have been manually deleted
- The VolumeSnapshot CRD's `deletionPolicy` may have removed the underlying storage

For each snapshot in `upgrade.Status.Snapshot.PVCSnapshots`, call `IsSnapshotReady()` to verify the VolumeSnapshot object exists and has `readyToUse=true`.

Failure mode: fail with reason `PreflightFailed` (`SnapshotNotReady`) — `"snapshot gitea-data-pre-upgrade-20260205-120000 no longer exists or is not ready"`.

**Workload discovery for PVCs** — via `Discoverer.DiscoverWorkloadsForPVCs()`:

The rollback `handleStopWorkload()` discovers workloads that mount the PVCs being restored. This is necessary because PVCs can't be deleted while mounted. Preflight verifies this discovery works and reports what would be scaled down.

Failure mode: discovery errors fail with reason `PreflightFailed`. "No workloads found" is logged as info.

Log output: `"preflight: N workloads to scale down before PVC restore: [StatefulSet/redis]"`.

#### Preflight Summary

| Check | Upgrade | Rollback | Failure mode |
|-------|:-------:|:--------:|-------------|
| Suspend target valid | Yes | Yes | Fail |
| Kustomization healthy | Yes | Yes | Warn |
| Kustomization already suspended | Yes | Yes | Warn |
| PVC discovery | If snapshots enabled | — | Fail on error, info if empty |
| Workload discovery | If snapshots enabled | If snapshots present | Fail on error, info if empty |
| VolumeSnapshotClass exists | If class configured | — | Fail |
| Snapshots still exist/ready | — | If snapshots present | Fail |

---

### Enhancement 2: Git Diff Preview (All Operations)

#### Current behavior

UpgradeRequest dry-run calls `GitManager.ReadFile()` to verify the path is readable. That's it — the file content is discarded.

RollbackRequest dry-run doesn't touch Git at all.

The real operations only read the file at commit/revert time, after suspension and scaling are already done.

#### New behavior

Both upgrade and rollback — dry-run and real — will compute and log the version diff **before** any mutations. This runs after preflight, alongside the existing initial validation.

Steps:

1. Read the current file content via `GitManager.ReadFile()`
2. Read the current version via `YAMLEditor.GetVersion()`
3. Compute the new content via `YAMLEditor.UpdateVersion()`
4. Log the version change at Info level

For upgrades, the version path and new version are determined the same way as in `handleCommitting()`. For rollbacks, they follow the same logic as `handleGitRevert()`.

This catches issues early — before any suspension or scaling:
- Version path doesn't exist in the YAML (bad `versionPath` config)
- YAML parsing failures
- Non-HelmRelease files using the default HelmRelease version path

#### Example log output

```
INFO  version change preview
      file=flux/apps/gitea/helmrelease.yaml
      path=spec.chart.spec.version
      current=1.0.0
      target=2.0.0
```

---

### Enhancement 3: Active Verification in Dry-Run

This is the key difference between dry-run and real operations. After preflight and Git diff (which both paths share), the dry-run exercises the full quiescence cycle: suspend → scale down → verify → scale up → verify → resume → verify.

This is a **real mutation** of cluster state. The app goes offline briefly. This is intentional — the quiescence path is the most critical and least trusted part of the pipeline. Discovering problems here during a dry-run (maintenance window, nothing else at stake) is far preferable to discovering them mid-snapshot-restore.

#### Dry-run quiescence cycle

1. **Suspend Kustomization** — via `FluxHelper.SuspendKustomization()`
2. **Verify suspended** — via `FluxHelper.IsSuspended()`, confirm `spec.suspend=true`
3. **Scale down workloads** — via `WorkloadScaler.ScaleDown()` using workloads discovered during preflight
4. **Verify scaled down** — via `WorkloadScaler.WaitForScaleDown()`, confirm all pods terminated
5. **Verify still suspended** — via `FluxHelper.VerifyStillSuspended()`, catch external interference before scale-up
6. **Scale up workloads** — via `WorkloadScaler.ScaleUp()`, restore original replica counts
7. **Verify scaled up** — via `WorkloadScaler.WaitForScaleUp()`, confirm pods running
8. **Verify still suspended** — via `FluxHelper.VerifyStillSuspended()`, confirm no interference before resuming
9. **Resume Kustomization** — via `FluxHelper.ResumeKustomization()`
10. **Verify resumed** — via `FluxHelper.IsSuspended()`, confirm `spec.suspend=false`

#### Error handling

The dry-run **must not** exit `DryRunSucceeded` until the Kustomization is confirmed resumed and workloads are confirmed scaled back up. The existing finalizer provides a safety net — if the controller crashes between suspend and resume, the finalizer's cleanup logic will resume the Kustomization on deletion.

If resume fails, the dry-run retries within the reconcile loop (same pattern as the real operation). The dry-run only fails with `DryRunFailed` after the 2-minute timeout for the resume phase is exhausted.

If scale-up fails after a successful scale-down, the dry-run continues retrying scale-up. It does not attempt to resume the Kustomization until workloads are confirmed up — resuming while scaled to zero could cause Flux to see the app as degraded and take corrective action.

If `VerifyStillSuspended` fails (external interference un-suspended the Kustomization), the dry-run fails with reason `DryRunFailed` and message indicating external interference was detected. At this point the workloads may already be scaling back up via Flux reconciliation, so the dry-run should not attempt to re-suspend or further mutate state — it reports the failure and exits. This mirrors the real operation's behavior when `VerifyStillSuspended` fails before Git commit or PVC deletion.

#### Timeouts

The dry-run reuses the existing per-phase timeout infrastructure (`setPhaseStartIfNeeded`, `isPhaseTimedOut`):

| Dry-run phase | Timeout | Notes |
|---------------|---------|-------|
| Suspend | 2 min | Same as real operation |
| Scale down | 5 min | Same as real operation |
| Scale up | 5 min | Same as real operation |
| Resume | 2 min | Same as real operation |

#### Passing preflight data to dry-run

The preflight phase discovers PVCs and workloads. These results are passed to the dry-run quiescence cycle rather than re-discovering. This avoids duplicate work and ensures consistency — the workloads that were validated in preflight are exactly the workloads that get scaled.

Implementation: preflight stores discovered workloads in a local variable within the reconcile handler, which is passed to the dry-run branch. No status field needed.

---

### Condition Messages

The dry-run condition message distinguishes between the phases:

```
Dry run passed. Would upgrade gitea from 1.0.0 to 2.0.0 (spec.chart.spec.version in flux/apps/gitea/helmrelease.yaml).
Preflight: Kustomization flux-system/gitea-ks Ready. 2 PVCs to snapshot, 2 workloads to scale.
Suspend/resume: verified. Scale down/up: verified (StatefulSet/redis, Deployment/worker).
```

For real operations, preflight results are logged but not included in the condition message (the condition reflects the current phase, not preflight).

---

### Implementation

#### Files to modify

| File | Change |
|------|--------|
| `internal/controller/upgraderequest_controller.go` | Extract preflight into shared path, extend `handleDryRun()` with Git diff + quiescence cycle |
| `internal/controller/rollbackrequest_controller.go` | Extract preflight into shared path, extend `handleDryRun()` with Git diff + quiescence cycle |
| `internal/controller/upgraderequest_unit_test.go` | Add preflight tests (shared path) + dry-run quiescence tests |
| `internal/controller/rollbackrequest_unit_test.go` | Add preflight tests (shared path) + dry-run quiescence tests |

#### RBAC

New marker needed for VolumeSnapshotClass read access:

```go
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list
```

#### Existing helpers reused

All required functionality already exists — no new packages or interfaces:

- `YAMLEditor.UpdateVersion()` / `GetVersion()` — `internal/yaml/editor.go`
- `GitManager.ReadFile()` — `internal/git/interface.go`
- `FluxHelper.ValidateSuspendTarget()` / `IsReconciled()` / `IsSuspended()` / `SuspendKustomization()` / `ResumeKustomization()` — `internal/flux/suspend.go`
- `Discoverer.DiscoverPVCs()` / `DiscoverWorkloads()` / `DiscoverWorkloadsForPVCs()` — `internal/discovery/discovery.go`
- `SnapshotManager.IsSnapshotReady()` — `internal/snapshot/manager.go`
- `WorkloadScaler.ScaleDown()` / `ScaleUp()` / `WaitForScaleDown()` / `WaitForScaleUp()` — `internal/workload/scaler.go`

#### Upgrade flow (revised)

```
 1. Fetch ManagedApp                          (existing)
 2. Check update available                    (existing)
 3. Determine target version                  (existing)
 4. Git diff preview                          (NEW — all operations)
    a. Read file via GitManager.ReadFile()
    b. Read current version via YAMLEditor.GetVersion()
    c. Compute new content via YAMLEditor.UpdateVersion()
    d. Log version change
 5. Preflight checks                          (NEW — all operations)
    a. ValidateSuspendTarget()
    b. IsReconciled() — warn if not Ready
    c. IsSuspended() — warn if already suspended
    d. If snapshots enabled:
       - DiscoverPVCs()
       - DiscoverWorkloads()
       - Verify VolumeSnapshotClass exists (if configured)
 6. Branch:
    DRY-RUN:
      a. Suspend Kustomization
      b. Verify suspended
      c. Scale down discovered workloads
      d. Verify scaled down (WaitForScaleDown)
      e. Verify still suspended (VerifyStillSuspended)
      f. Scale up workloads (restore replicas)
      g. Verify scaled up (WaitForScaleUp)
      h. Verify still suspended (VerifyStillSuspended)
      i. Resume Kustomization
      j. Verify resumed
      k. Set DryRunSucceeded with summary
      l. Remove finalizer
    REAL:
      → handleSuspend
      → handleScaleDown
      → handleSnapshotting
      → handleCommitting         ← point of no return
      → handleReconciling
      → handleHealthChecking
      → handleCompleted
```

#### Rollback flow (revised)

```
 1. Fetch UpgradeRequest                      (existing)
 2. Verify upgrade terminal                   (existing)
 3. Verify snapshots exist                    (existing)
 4. Verify previous version recorded          (existing)
 5. Fetch ManagedApp (via UpgradeRequest)     (NEW)
 6. Git diff preview                          (NEW — all operations)
    a. Read file via GitManager.ReadFile()
    b. Read current version via YAMLEditor.GetVersion()
    c. Compute revert content via YAMLEditor.UpdateVersion()
    d. Log version change
 7. Preflight checks                          (NEW — all operations)
    a. ValidateSuspendTarget()
    b. IsReconciled() — warn if not Ready
    c. IsSuspended() — warn if already suspended
    d. If snapshots present:
       - Verify each snapshot still exists and ready
       - DiscoverWorkloadsForPVCs()
 8. Branch:
    DRY-RUN:
      a. Suspend Kustomization
      b. Verify suspended
      c. Scale down discovered workloads
      d. Verify scaled down (WaitForScaleDown)
      e. Verify still suspended (VerifyStillSuspended)
      f. Scale up workloads (restore replicas)
      g. Verify scaled up (WaitForScaleUp)
      h. Verify still suspended (VerifyStillSuspended)
      i. Resume Kustomization
      j. Verify resumed
      k. Set DryRunSucceeded with summary
      l. Remove finalizer
    REAL:
      → handleSuspend
      → handleStopWorkload
      → handleVolumeRestore      ← point of no return
      → handleGitRevert
      → handleReconciling
      → handleHealthChecking
      → handleCompleted
```
