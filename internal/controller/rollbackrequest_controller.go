/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/flux"
	"github.com/nbenn/fluxup/internal/git"
	"github.com/nbenn/fluxup/internal/logging"
	"github.com/nbenn/fluxup/internal/snapshot"
	"github.com/nbenn/fluxup/internal/workload"
	yamlpkg "github.com/nbenn/fluxup/internal/yaml"
)

// unknownVersion is returned when a version cannot be determined
const unknownVersion = "unknown"

// RollbackRequestReconciler reconciles a RollbackRequest
type RollbackRequestReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	GitManager      git.Manager
	SnapshotManager *snapshot.Manager
	FluxHelper      *flux.Helper
	WorkloadScaler  *workload.Scaler
	YAMLEditor      *yamlpkg.Editor
}

// +kubebuilder:rbac:groups=fluxup.dev,resources=rollbackrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fluxup.dev,resources=rollbackrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fluxup.dev,resources=rollbackrequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete

func (r *RollbackRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// 1. Fetch RollbackRequest
	var rollback fluxupv1alpha1.RollbackRequest
	if err := r.Get(ctx, req.NamespacedName, &rollback); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("reconciling RollbackRequest",
		"upgradeRequest", rollback.Spec.UpgradeRequestRef.Name)

	// 2. Handle deletion - remove finalizer if present
	if !rollback.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&rollback, OperationFinalizer) {
			controllerutil.RemoveFinalizer(&rollback, OperationFinalizer)
			if err := r.Update(ctx, &rollback); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 3. Add finalizer if not present (protects in-progress operations from deletion)
	if !controllerutil.ContainsFinalizer(&rollback, OperationFinalizer) {
		controllerutil.AddFinalizer(&rollback, OperationFinalizer)
		if err := r.Update(ctx, &rollback); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 4. Check if already complete (terminal state)
	if meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete) {
		return ctrl.Result{}, nil
	}
	// Also check for failed state (Complete=False with failure reason)
	if cond := meta.FindStatusCondition(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete); cond != nil && cond.Status == metav1.ConditionFalse {
		return ctrl.Result{}, nil
	}

	// 5. Handle dry run mode
	if rollback.Spec.DryRun {
		return r.handleDryRun(ctx, &rollback)
	}

	// 6. Determine current step from conditions and handle accordingly
	if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
		return r.handleSuspend(ctx, &rollback)
	}

	// Check if workload is stopped
	if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeWorkloadStopped) {
		result, err, shouldContinue := r.handleStopWorkload(ctx, &rollback)
		if !shouldContinue {
			return result, err
		}
		// If shouldContinue is true, no workloadRef configured - already marked as skipped
	}

	if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeVolumesRestored) {
		return r.handleVolumeRestore(ctx, &rollback)
	}

	if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeGitReverted) {
		return r.handleGitRevert(ctx, &rollback)
	}

	if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeReconciled) {
		return r.handleReconciling(ctx, &rollback)
	}

	if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeHealthy) {
		return r.handleHealthChecking(ctx, &rollback)
	}

	// All conditions met - mark complete
	return r.handleCompleted(ctx, &rollback)
}

// handleDryRun validates the rollback without making any changes
func (r *RollbackRequestReconciler) handleDryRun(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("processing dry run")

	// Fetch the UpgradeRequest
	upgrade, err := r.getUpgradeRequest(ctx, rollback)
	if err != nil {
		return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
	}

	// Verify upgrade is terminal
	if !r.isUpgradeTerminal(upgrade) {
		return r.setFailed(ctx, rollback, "UpgradeInProgress", "Cannot rollback an in-progress upgrade")
	}

	// Verify upgrade has snapshot info
	if upgrade.Status.Snapshot == nil || len(upgrade.Status.Snapshot.PVCSnapshots) == 0 {
		return r.setFailed(ctx, rollback, "NoSnapshotsAvailable", "UpgradeRequest has no snapshots to restore from")
	}

	// Verify previous version is recorded
	if upgrade.Status.Upgrade == nil || upgrade.Status.Upgrade.PreviousVersion == nil {
		return r.setFailed(ctx, rollback, "NoPreviousVersion", "UpgradeRequest has no previous version recorded")
	}

	// Mark as complete (dry run successful)
	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeComplete,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunSucceeded",
		Message:            fmt.Sprintf("Dry run validation passed. Would rollback to %s", r.getVersionString(upgrade.Status.Upgrade.PreviousVersion)),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	// Remove finalizer for dry run
	if controllerutil.ContainsFinalizer(rollback, OperationFinalizer) {
		controllerutil.RemoveFinalizer(rollback, OperationFinalizer)
		if err := r.Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// handleSuspend validates the request and suspends Flux
func (r *RollbackRequestReconciler) handleSuspend(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Set start time
	if rollback.Status.StartedAt == nil {
		now := metav1.Now()
		rollback.Status.StartedAt = &now
	}

	// Fetch the UpgradeRequest
	upgrade, err := r.getUpgradeRequest(ctx, rollback)
	if err != nil {
		return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
	}

	// Verify upgrade is terminal
	if !r.isUpgradeTerminal(upgrade) {
		return r.setFailed(ctx, rollback, "UpgradeInProgress", "Cannot rollback an in-progress upgrade")
	}

	// Fetch the ManagedApp
	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
	}

	// Verify upgrade has snapshot info (unless snapshots were skipped)
	if upgrade.Spec.SkipSnapshot {
		logger.Info("upgrade had skipSnapshot=true, skipping volume restore")
	} else if upgrade.Status.Snapshot == nil || len(upgrade.Status.Snapshot.PVCSnapshots) == 0 {
		return r.setFailed(ctx, rollback, "NoSnapshotsAvailable", "UpgradeRequest has no snapshots to restore from")
	}

	// Verify previous version is recorded
	if upgrade.Status.Upgrade == nil || upgrade.Status.Upgrade.PreviousVersion == nil {
		return r.setFailed(ctx, rollback, "NoPreviousVersion", "UpgradeRequest has no previous version recorded")
	}

	// Record what we're rolling back
	rollback.Status.RestoredFrom = &fluxupv1alpha1.RestoredFromStatus{
		UpgradeRequestName: upgrade.Name,
		TargetVersion:      upgrade.Status.Upgrade.PreviousVersion,
		RolledBackVersion:  upgrade.Status.Upgrade.NewVersion,
	}

	logger.Info("starting rollback",
		"app", app.Name,
		"fromVersion", r.getVersionString(upgrade.Status.Upgrade.NewVersion),
		"toVersion", r.getVersionString(upgrade.Status.Upgrade.PreviousVersion))

	// Determine which Kustomization to suspend (suspendRef or kustomizationRef)
	suspendName, suspendNS := r.getSuspendTarget(app)

	// Validate suspend target is appropriate (root if no explicit suspendRef)
	ksRef := app.Spec.KustomizationRef
	ksNS := ksRef.Namespace
	if ksNS == "" {
		ksNS = DefaultFluxNamespace
	}

	if err := r.FluxHelper.ValidateSuspendTarget(ctx,
		&struct{ Name, Namespace string }{Name: ksRef.Name, Namespace: ksNS},
		r.getSuspendRefStruct(app)); err != nil {
		return r.setFailed(ctx, rollback, "InvalidSuspendTarget", err.Error())
	}

	// Suspend Flux Kustomization
	if err := r.FluxHelper.SuspendKustomization(ctx, suspendName, suspendNS); err != nil {
		return r.setFailed(ctx, rollback, "SuspendFailed", err.Error())
	}

	// Set Suspended condition
	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             "KustomizationSuspended",
		Message:            fmt.Sprintf("Suspended %s/%s", suspendNS, suspendName),
		ObservedGeneration: rollback.Generation,
	})

	// If snapshots disabled, also mark VolumesRestored as true (skipped)
	if upgrade.Spec.SkipSnapshot || upgrade.Status.Snapshot == nil || len(upgrade.Status.Snapshot.PVCSnapshots) == 0 {
		meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeVolumesRestored,
			Status:             metav1.ConditionTrue,
			Reason:             "VolumesSkipped",
			Message:            "No snapshots to restore (upgrade had skipSnapshot=true or no snapshots)",
			ObservedGeneration: rollback.Generation,
		})
	}

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleStopWorkload scales the workload to 0 replicas
func (r *RollbackRequestReconciler) handleStopWorkload(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error, bool) {
	logger := logging.FromContext(ctx)

	upgrade, err := r.getUpgradeRequest(ctx, rollback)
	if err != nil {
		result, err := r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
		return result, err, false
	}

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		result, err := r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
		return result, err, false
	}

	// If no workloadRef configured, skip scaling (return shouldContinue=true)
	if app.Spec.WorkloadRef == nil {
		logger.Debug("no workloadRef configured, skipping workload stop")
		// Mark as stopped (skipped) so we don't check this again
		meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
			Status:             metav1.ConditionTrue,
			Reason:             "Skipped",
			Message:            "No workloadRef configured",
			ObservedGeneration: rollback.Generation,
		})
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err, false
		}
		return ctrl.Result{Requeue: true}, nil, true
	}

	workloadRef := app.Spec.WorkloadRef
	workloadNS := workloadRef.Namespace
	if workloadNS == "" {
		workloadNS = app.Namespace
	}

	// Scale down the workload
	scaleInfo, err := r.WorkloadScaler.ScaleDown(ctx, workloadRef.Kind, workloadRef.Name, workloadNS)
	if err != nil {
		result, err := r.setFailed(ctx, rollback, "StopWorkloadFailed", err.Error())
		return result, err, false
	}

	logger.Info("stopping workload",
		"kind", workloadRef.Kind,
		"name", workloadRef.Name,
		"originalReplicas", scaleInfo.OriginalReplicas)

	// Wait for scale down to complete (with timeout)
	scaleTimeout := 5 * time.Minute
	if err := r.WorkloadScaler.WaitForScaleDown(ctx, workloadRef.Kind, workloadRef.Name, workloadNS, scaleTimeout); err != nil {
		// Check if it's just not ready yet vs actual error
		if ctx.Err() == nil {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil, false
		}
		result, err := r.setFailed(ctx, rollback, "StopWorkloadTimeout", err.Error())
		return result, err, false
	}

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
		Status:             metav1.ConditionTrue,
		Reason:             "WorkloadStopped",
		Message:            fmt.Sprintf("Stopped %s/%s (was %d replicas)", workloadRef.Kind, workloadRef.Name, scaleInfo.OriginalReplicas),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err, false
	}

	return ctrl.Result{Requeue: true}, nil, false
}

// handleVolumeRestore deletes current PVCs and creates new ones from snapshots
func (r *RollbackRequestReconciler) handleVolumeRestore(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	upgrade, err := r.getUpgradeRequest(ctx, rollback)
	if err != nil {
		return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
	}

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
	}

	// Re-verify suspend before PVC deletion (point of no return)
	suspendName, suspendNS := r.getSuspendTarget(app)
	if err := r.FluxHelper.VerifyStillSuspended(ctx, suspendName, suspendNS); err != nil {
		return r.setFailed(ctx, rollback, "SuspendVerificationFailed", err.Error())
	}

	// Initialize volume restore status
	if rollback.Status.VolumeRestore == nil {
		now := metav1.Now()
		rollback.Status.VolumeRestore = &fluxupv1alpha1.VolumeRestoreStatus{
			StartedAt: &now,
		}
	}

	// Process each PVC from the upgrade's snapshot list
	for _, snapInfo := range upgrade.Status.Snapshot.PVCSnapshots {
		// Check if already processed
		alreadyRestored := false
		for _, restored := range rollback.Status.VolumeRestore.RestoredPVCs {
			if restored.PVCName == snapInfo.PVCName {
				alreadyRestored = true
				break
			}
		}
		if alreadyRestored {
			continue
		}

		logger.Info("restoring PVC from snapshot",
			"pvc", snapInfo.PVCName,
			"snapshot", snapInfo.SnapshotName)

		// Delete the current PVC
		if err := r.SnapshotManager.DeletePVC(ctx, snapInfo.PVCName, app.Namespace); err != nil {
			// Ignore not found - might already be deleted
			if !apierrors.IsNotFound(err) {
				return r.setFailed(ctx, rollback, "PVCDeleteFailed", err.Error())
			}
		}

		// Wait for PVC to be fully deleted
		deleteTimeout := 5 * time.Minute
		if err := r.SnapshotManager.WaitForPVCDeleted(ctx, snapInfo.PVCName, app.Namespace, deleteTimeout); err != nil {
			if ctx.Err() == nil {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			return r.setFailed(ctx, rollback, "PVCDeleteTimeout", err.Error())
		}

		// Create new PVC from snapshot
		_, err := r.SnapshotManager.RestorePVCFromSnapshot(ctx, snapshot.RestoreRequest{
			SnapshotName:      snapInfo.SnapshotName,
			SnapshotNamespace: app.Namespace,
			NewPVCName:        snapInfo.PVCName,
			NewPVCNamespace:   app.Namespace,
			Labels: map[string]string{
				"fluxup.dev/restored-from": snapInfo.SnapshotName,
			},
		})
		if err != nil {
			return r.setFailed(ctx, rollback, "PVCRestoreFailed", err.Error())
		}

		// Record the restoration
		rollback.Status.VolumeRestore.RestoredPVCs = append(rollback.Status.VolumeRestore.RestoredPVCs,
			fluxupv1alpha1.RestoredPVCInfo{
				PVCName:          snapInfo.PVCName,
				SnapshotName:     snapInfo.SnapshotName,
				RestorationState: "Pending",
			})

		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Wait for all PVCs to be bound
	for i, restored := range rollback.Status.VolumeRestore.RestoredPVCs {
		if restored.RestorationState == "Restored" {
			continue
		}

		bindTimeout := 5 * time.Minute
		if err := r.SnapshotManager.WaitForPVCBound(ctx, restored.PVCName, app.Namespace, bindTimeout); err != nil {
			if ctx.Err() == nil {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			return r.setFailed(ctx, rollback, "PVCBindTimeout", err.Error())
		}

		rollback.Status.VolumeRestore.RestoredPVCs[i].RestorationState = "Restored"
	}

	// All PVCs restored
	now := metav1.Now()
	rollback.Status.VolumeRestore.CompletedAt = &now

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeVolumesRestored,
		Status:             metav1.ConditionTrue,
		Reason:             "AllPVCsRestored",
		Message:            fmt.Sprintf("Restored %d PVCs from snapshots", len(rollback.Status.VolumeRestore.RestoredPVCs)),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleGitRevert reverts the version in Git
func (r *RollbackRequestReconciler) handleGitRevert(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	upgrade, err := r.getUpgradeRequest(ctx, rollback)
	if err != nil {
		return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
	}

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
	}

	// Re-verify suspend before Git commit
	suspendName, suspendNS := r.getSuspendTarget(app)
	if err := r.FluxHelper.VerifyStillSuspended(ctx, suspendName, suspendNS); err != nil {
		return r.setFailed(ctx, rollback, "SuspendVerificationFailed", err.Error())
	}

	// Get the version to revert to
	targetVersion := upgrade.Status.Upgrade.PreviousVersion
	if targetVersion == nil {
		return r.setFailed(ctx, rollback, "NoPreviousVersion", "UpgradeRequest has no previous version recorded")
	}

	// Determine version path and new version value
	versionPath := yamlpkg.DefaultHelmReleaseVersionPath
	if app.Spec.VersionPolicy != nil && app.Spec.VersionPolicy.VersionPath != "" {
		versionPath = app.Spec.VersionPolicy.VersionPath
	}

	// Determine the revert version string based on update type (chart or image)
	var revertVersionStr string
	var currentVersionStr string
	if targetVersion.Chart != "" {
		revertVersionStr = targetVersion.Chart
		if upgrade.Status.Upgrade.NewVersion != nil {
			currentVersionStr = upgrade.Status.Upgrade.NewVersion.Chart
		}
	} else if len(targetVersion.Images) > 0 {
		revertVersionStr = targetVersion.Images[0].Tag
		if upgrade.Status.Upgrade.NewVersion != nil && len(upgrade.Status.Upgrade.NewVersion.Images) > 0 {
			currentVersionStr = upgrade.Status.Upgrade.NewVersion.Images[0].Tag
		}
	} else {
		return r.setFailed(ctx, rollback, "InvalidTargetVersion", "No chart version or images in previous version")
	}

	logger.Info("reverting Git to previous version",
		"targetVersion", revertVersionStr)

	// Read current file from Git
	content, err := r.GitManager.ReadFile(ctx, app.Spec.GitPath)
	if err != nil {
		return r.setFailed(ctx, rollback, "GitReadFailed", err.Error())
	}

	// Update to previous version
	newContent, err := r.YAMLEditor.UpdateVersion(content, versionPath, revertVersionStr)
	if err != nil {
		return r.setFailed(ctx, rollback, "YAMLUpdateFailed", err.Error())
	}

	// Build commit message
	message := git.FormatRevertCommitMessage(
		app.Name,
		currentVersionStr,
		revertVersionStr,
		upgrade.Name,
	)

	// Commit to Git
	commitInfo, err := r.GitManager.CommitFile(ctx, git.FileChange{
		Path:    app.Spec.GitPath,
		Content: newContent,
	}, message)
	if err != nil {
		return r.setFailed(ctx, rollback, "GitCommitFailed", err.Error())
	}

	logger.Info("committed version revert to Git",
		"commit", commitInfo.SHA,
		"version", revertVersionStr)

	// Update status
	now := metav1.Now()
	rollback.Status.GitRevert = &fluxupv1alpha1.GitRevertStatus{
		CommitSHA:   commitInfo.SHA,
		CommittedAt: &now,
	}

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeGitReverted,
		Status:             metav1.ConditionTrue,
		Reason:             "VersionReverted",
		Message:            fmt.Sprintf("Commit %s", commitInfo.SHA),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleReconciling resumes Flux and waits for reconciliation
func (r *RollbackRequestReconciler) handleReconciling(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	upgrade, err := r.getUpgradeRequest(ctx, rollback)
	if err != nil {
		return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
	}

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
	}

	// Resume the Kustomization we suspended (suspendRef or kustomizationRef)
	suspendName, suspendNS := r.getSuspendTarget(app)

	if err := r.FluxHelper.ResumeKustomization(ctx, suspendName, suspendNS); err != nil {
		return r.setFailed(ctx, rollback, "ResumeFailed", err.Error())
	}

	// Update Suspended condition to False
	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSuspended,
		Status:             metav1.ConditionFalse,
		Reason:             "KustomizationResumed",
		Message:            fmt.Sprintf("Resumed %s/%s", suspendNS, suspendName),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("resumed Flux Kustomization, waiting for reconciliation")

	// Check if reconciliation is complete (non-blocking)
	ksRef := app.Spec.KustomizationRef
	ksNS := ksRef.Namespace
	if ksNS == "" {
		ksNS = DefaultFluxNamespace
	}
	reconciled, err := r.FluxHelper.IsReconciled(ctx, ksRef.Name, ksNS)
	if err != nil {
		return r.setFailed(ctx, rollback, "ReconciliationCheckFailed", err.Error())
	}

	if !reconciled {
		// Check timeout
		timeout := 10 * time.Minute
		if rollback.Status.StartedAt != nil {
			elapsed := time.Since(rollback.Status.StartedAt.Time)
			if elapsed > timeout {
				return r.setFailed(ctx, rollback, "ReconciliationTimeout", "Kustomization did not reconcile within timeout")
			}
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Reconciliation complete
	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeReconciled,
		Status:             metav1.ConditionTrue,
		Reason:             "ReconciliationSucceeded",
		Message:            "Flux reconciled the changes",
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleHealthChecking verifies the rollback succeeded
func (r *RollbackRequestReconciler) handleHealthChecking(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	upgrade, err := r.getUpgradeRequest(ctx, rollback)
	if err != nil {
		return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
	}

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
	}

	// Check workload health via ManagedApp Ready condition
	healthy := false
	for _, cond := range app.Status.Conditions {
		if cond.Type == fluxupv1alpha1.ConditionTypeReady {
			healthy = cond.Status == metav1.ConditionTrue
			break
		}
	}

	if !healthy {
		// Check timeout
		timeout := 5 * time.Minute
		if app.Spec.HealthCheck != nil && app.Spec.HealthCheck.Timeout != "" {
			if parsed, err := time.ParseDuration(app.Spec.HealthCheck.Timeout); err == nil {
				timeout = parsed
			}
		}

		if rollback.Status.StartedAt != nil {
			elapsed := time.Since(rollback.Status.StartedAt.Time)
			if elapsed > timeout {
				return r.setFailed(ctx, rollback, "HealthCheckTimeout", "Workload did not become healthy within timeout")
			}
		}

		logger.Debug("workload not yet healthy, waiting")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Health check passed
	logger.Info("health check passed, rollback complete")

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeHealthy,
		Status:             metav1.ConditionTrue,
		Reason:             "HealthCheckPassed",
		Message:            "Workload is healthy",
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleCompleted marks the rollback as complete
func (r *RollbackRequestReconciler) handleCompleted(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	versionStr := unknownVersion
	if rollback.Status.RestoredFrom != nil && rollback.Status.RestoredFrom.TargetVersion != nil {
		versionStr = r.getVersionString(rollback.Status.RestoredFrom.TargetVersion)
	}

	logger.Info("rollback complete", "targetVersion", versionStr)

	now := metav1.Now()
	rollback.Status.CompletedAt = &now

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeComplete,
		Status:             metav1.ConditionTrue,
		Reason:             "RollbackSucceeded",
		Message:            fmt.Sprintf("Rolled back to %s", versionStr),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	// Remove finalizer now that operation is complete
	if controllerutil.ContainsFinalizer(rollback, OperationFinalizer) {
		controllerutil.RemoveFinalizer(rollback, OperationFinalizer)
		if err := r.Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// getUpgradeRequest fetches the referenced UpgradeRequest
func (r *RollbackRequestReconciler) getUpgradeRequest(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (*fluxupv1alpha1.UpgradeRequest, error) {
	ref := rollback.Spec.UpgradeRequestRef
	ns := ref.Namespace
	if ns == "" {
		ns = rollback.Namespace
	}

	var upgrade fluxupv1alpha1.UpgradeRequest
	key := types.NamespacedName{Name: ref.Name, Namespace: ns}
	if err := r.Get(ctx, key, &upgrade); err != nil {
		return nil, err
	}
	return &upgrade, nil
}

// getManagedApp fetches the ManagedApp from an UpgradeRequest
func (r *RollbackRequestReconciler) getManagedApp(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (*fluxupv1alpha1.ManagedApp, error) {
	ref := upgrade.Spec.ManagedAppRef
	ns := ref.Namespace
	if ns == "" {
		ns = upgrade.Namespace
	}

	var app fluxupv1alpha1.ManagedApp
	key := types.NamespacedName{Name: ref.Name, Namespace: ns}
	if err := r.Get(ctx, key, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

// isUpgradeTerminal checks if the upgrade is in a terminal state
func (r *RollbackRequestReconciler) isUpgradeTerminal(upgrade *fluxupv1alpha1.UpgradeRequest) bool {
	return meta.FindStatusCondition(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete) != nil
}

// getSuspendTarget returns the name and namespace of the Kustomization to suspend.
// Uses suspendRef if set, otherwise falls back to kustomizationRef.
func (r *RollbackRequestReconciler) getSuspendTarget(app *fluxupv1alpha1.ManagedApp) (name, namespace string) {
	if app.Spec.SuspendRef != nil {
		namespace = app.Spec.SuspendRef.Namespace
		if namespace == "" {
			namespace = DefaultFluxNamespace
		}
		return app.Spec.SuspendRef.Name, namespace
	}

	namespace = app.Spec.KustomizationRef.Namespace
	if namespace == "" {
		namespace = DefaultFluxNamespace
	}
	return app.Spec.KustomizationRef.Name, namespace
}

// getSuspendRefStruct returns the suspendRef as a struct pointer for validation.
// Returns nil if no suspendRef is configured.
func (r *RollbackRequestReconciler) getSuspendRefStruct(app *fluxupv1alpha1.ManagedApp) *struct{ Name, Namespace string } {
	if app.Spec.SuspendRef == nil {
		return nil
	}
	ns := app.Spec.SuspendRef.Namespace
	if ns == "" {
		ns = DefaultFluxNamespace
	}
	return &struct{ Name, Namespace string }{Name: app.Spec.SuspendRef.Name, Namespace: ns}
}

// getVersionString returns a string representation of a VersionInfo
func (r *RollbackRequestReconciler) getVersionString(v *fluxupv1alpha1.VersionInfo) string {
	if v == nil {
		return unknownVersion
	}
	if v.Chart != "" {
		return v.Chart
	}
	if len(v.Images) > 0 {
		return v.Images[0].Tag
	}
	return unknownVersion
}

// setFailed marks the rollback as failed via Complete=False condition
func (r *RollbackRequestReconciler) setFailed(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest, reason, message string) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Error("rollback failed", "reason", reason, "message", message)

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeComplete,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	// Remove finalizer now that operation is complete (failed)
	if controllerutil.ContainsFinalizer(rollback, OperationFinalizer) {
		controllerutil.RemoveFinalizer(rollback, OperationFinalizer)
		if err := r.Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager configures the controller
func (r *RollbackRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fluxupv1alpha1.RollbackRequest{}).
		Named("rollbackrequest").
		Complete(r)
}
