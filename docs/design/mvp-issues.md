# MVP Issues and Recommendations

This document lists issues and recommendations identified during the Phase 3 completion review, before testing on a real cluster.

## Documentation Issues

### Missing Documentation

1. **RollbackRequest Reference Doc Missing**
   - `docs/reference/rollbackrequest.md` does not exist
   - Should be created with same structure as `upgraderequest.md`
   - Should document spec fields, status fields, conditions, and examples

2. **Rollback Guide Missing**
   - The upgrades guide (`docs/guides/upgrades.md:193`) references `[Rollback Guide](rollback.md)` but the file doesn't exist
   - Should create `docs/guides/rollback.md` explaining:
     - How to create a RollbackRequest
     - When auto-rollback triggers
     - How to monitor rollback progress
     - Troubleshooting failed rollbacks

3. **Docs Site Navigation Incomplete**
   - `.github/workflows/docs.yml` sidebar is missing:
     - UpgradeRequest CRD reference
     - RollbackRequest CRD reference
     - Upgrades guide
     - Future rollback guide

### Documentation Fixes Applied

- ✅ Updated `docs/design/architecture.md` to mark Phase 3 as complete
- ✅ Added Phase 3 Design link to `README.md`

## Code Issues

### Potential Issues (Medium Priority)

1. **HelmRelease workloadRef Not Fully Supported**
   - `ManagedAppSpec.WorkloadRef.Kind` has enum `Deployment;StatefulSet;HelmRelease`
   - However, `workload.Scaler` only handles `Deployment` and `StatefulSet`
   - If `workloadRef.Kind = HelmRelease`, scale operations will fail with "unsupported workload kind"
   - **Recommendation**: Either:
     - Remove `HelmRelease` from the enum, or
     - Add special handling that skips scaling for HelmRelease (Flux manages it)
     - Document this limitation

2. **Rollback Health Check Uses ManagedApp.Status.Conditions**
   - `rollbackrequest_controller.go:692-697` checks `ManagedApp.Status.Conditions[Ready]`
   - This requires the `ManagedAppReconciler` to correctly set the Ready condition
   - Need to verify the ManagedApp controller properly updates Ready condition based on workload status

3. **UpgradeRequest Dry Run Doesn't Remove Finalizer**
   - In `handleDryRun()` (line 149-191), finalizer is not explicitly removed
   - This may leave orphaned finalizers on dry-run UpgradeRequests
   - Compare with RollbackRequest dry run which does remove finalizer (line 183-188)

4. **Timeout Calculation in Rollback Reconciling Phase**
   - `rollbackrequest_controller.go:651-658` uses `rollback.Status.StartedAt` for timeout
   - But `StartedAt` is set at the very beginning of the rollback
   - A slow volume restore could eat into the reconciliation timeout
   - Consider using a separate timestamp or measuring from Flux resume time

### Minor Issues (Low Priority)

5. **Duplicate Code: getSuspendTarget and getSuspendRefStruct**
   - Both `upgraderequest_controller.go` and `rollbackrequest_controller.go` have identical copies of:
     - `getSuspendTarget()`
     - `getSuspendRefStruct()`
   - **Recommendation**: Extract to a shared helper in the controller package

6. **Comment Number Mismatch**
   - `upgraderequest_controller.go` line 111-116 comments say "3. Handle dry run" and "4. Determine current step"
   - But steps 2-3 (deletion and finalizer) were already numbered 2 and 3
   - Minor clarity issue

## Testing Recommendations

### Before Real Cluster Testing

1. **Verify E2E Tests Pass**
   ```bash
   make test-e2e
   ```

2. **Manual Smoke Test Checklist**
   - [ ] Create ManagedApp with workloadRef pointing to StatefulSet
   - [ ] Create UpgradeRequest with targetVersion
   - [ ] Verify upgrade completes successfully
   - [ ] Create RollbackRequest referencing the UpgradeRequest
   - [ ] Verify rollback completes successfully
   - [ ] Verify PVCs were restored from snapshots
   - [ ] Verify Git has the reverted version

3. **Auto-Rollback Test**
   - [ ] Create ManagedApp with `autoRollback: true`
   - [ ] Create UpgradeRequest that will fail health check
   - [ ] Verify RollbackRequest is auto-created
   - [ ] Verify rollback completes

### Edge Cases to Test

1. **No Snapshots Scenario**
   - UpgradeRequest with `skipSnapshot: true`
   - Rollback should skip volume restore and just do Git revert

2. **No workloadRef Scenario**
   - ManagedApp without workloadRef
   - Upgrade/rollback should skip scaling steps

3. **App-of-Apps Pattern**
   - ManagedApp with explicit `suspendRef`
   - Verify parent Kustomization is suspended, not child

## Configuration Recommendations

### For Real Cluster Deployment

1. **Git Credentials**
   - Ensure `GIT_TOKEN` has write access to repository
   - Verify the repository URL matches Flux's source repository

2. **VolumeSnapshotClass**
   - Verify the `volumeSnapshotClassName` exists and is functional
   - Test snapshot creation manually first:
     ```bash
     kubectl get volumesnapshotclass
     kubectl apply -f - <<EOF
     apiVersion: snapshot.storage.k8s.io/v1
     kind: VolumeSnapshot
     metadata:
       name: test-snap
       namespace: default
     spec:
       volumeSnapshotClassName: your-class
       source:
         persistentVolumeClaimName: your-pvc
     EOF
     ```

3. **RBAC**
   - Verify controller has necessary permissions
   - Check for any namespace-scoped restrictions

## Summary

| Category | Count |
|----------|-------|
| Missing Documentation | 3 |
| Code Issues (Medium) | 4 |
| Code Issues (Minor) | 2 |

**Recommended Priority Order:**
1. Create rollback guide and reference doc (user-facing)
2. Fix HelmRelease workloadRef issue (potential runtime error)
3. Fix dry run finalizer issue (resource leak)
4. Run full E2E test suite
5. Address minor issues as time permits
