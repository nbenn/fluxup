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
	"log/slog"
	"time"

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

// DefaultFluxNamespace is the default namespace for Flux resources.
const DefaultFluxNamespace = "flux-system"

// OperationFinalizer is used to prevent deletion of in-progress operations.
const OperationFinalizer = "fluxup.dev/operation-protection"

// Phase timeout defaults (conservative values)
const (
	TimeoutSuspend       = 2 * time.Minute
	TimeoutScaleDown     = 5 * time.Minute
	TimeoutSnapshot      = 30 * time.Minute
	TimeoutGitCommit     = 2 * time.Minute
	TimeoutReconcile     = 10 * time.Minute
	TimeoutHealthCheck   = 5 * time.Minute
	TimeoutVolumeRestore = 30 * time.Minute
	TimeoutGitRevert     = 2 * time.Minute
)

// UpgradeRequestReconciler reconciles an UpgradeRequest
type UpgradeRequestReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	GitManager      git.Manager
	SnapshotManager *snapshot.Manager
	FluxHelper      *flux.Helper
	WorkloadScaler  *workload.Scaler
	YAMLEditor      *yamlpkg.Editor
}

// setPhaseStartIfNeeded sets PhaseStartedAt if not already set for this reconcile cycle.
// Call this at the beginning of each phase handler when entering a new phase.
func (r *UpgradeRequestReconciler) setPhaseStartIfNeeded(upgrade *fluxupv1alpha1.UpgradeRequest) {
	if upgrade.Status.PhaseStartedAt == nil {
		now := metav1.Now()
		upgrade.Status.PhaseStartedAt = &now
	}
}

// resetPhaseStart clears PhaseStartedAt to mark the transition to a new phase.
// Call this when a phase completes successfully before moving to the next phase.
func (r *UpgradeRequestReconciler) resetPhaseStart(upgrade *fluxupv1alpha1.UpgradeRequest) {
	upgrade.Status.PhaseStartedAt = nil
}

// isPhaseTimedOut checks if the current phase has exceeded its timeout.
func (r *UpgradeRequestReconciler) isPhaseTimedOut(upgrade *fluxupv1alpha1.UpgradeRequest, timeout time.Duration) bool {
	if upgrade.Status.PhaseStartedAt == nil {
		return false
	}
	return time.Since(upgrade.Status.PhaseStartedAt.Time) > timeout
}

// +kubebuilder:rbac:groups=fluxup.dev,resources=upgraderequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fluxup.dev,resources=upgraderequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fluxup.dev,resources=upgraderequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

func (r *UpgradeRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// 1. Fetch UpgradeRequest
	var upgrade fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, req.NamespacedName, &upgrade); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("reconciling UpgradeRequest",
		"managedApp", upgrade.Spec.ManagedAppRef.Name)

	// 2. Handle deletion - remove finalizer if present
	if !upgrade.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&upgrade, OperationFinalizer) {
			controllerutil.RemoveFinalizer(&upgrade, OperationFinalizer)
			if err := r.Update(ctx, &upgrade); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 3. Add finalizer if not present (protects in-progress operations from deletion)
	if !controllerutil.ContainsFinalizer(&upgrade, OperationFinalizer) {
		controllerutil.AddFinalizer(&upgrade, OperationFinalizer)
		if err := r.Update(ctx, &upgrade); err != nil {
			return ctrl.Result{}, err
		}
		// Requeue to continue with fresh object
		return ctrl.Result{Requeue: true}, nil
	}

	// 4. Check if already complete (terminal state) - finalizer already removed by completion handler
	if meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete) {
		return ctrl.Result{}, nil
	}
	// Also check for failed state (Complete=False with failure reason)
	if cond := meta.FindStatusCondition(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete); cond != nil && cond.Status == metav1.ConditionFalse {
		return ctrl.Result{}, nil
	}

	// 3. Handle dry run mode
	if upgrade.Spec.DryRun {
		return r.handleDryRun(ctx, &upgrade)
	}

	// 4. Determine current step from conditions and handle accordingly
	if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
		return r.handleSuspend(ctx, &upgrade)
	}

	// Check if workload needs to be stopped (before snapshot for consistency)
	// WorkloadStopped=True means either stopped or skipped (no workloadRef)
	if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeWorkloadStopped) {
		result, err, shouldContinue := r.handleScaleDown(ctx, &upgrade)
		if !shouldContinue {
			return result, err
		}
		// If shouldContinue is true, no workloadRef configured - already marked as skipped
	}

	if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSnapshotReady) {
		return r.handleSnapshotting(ctx, &upgrade)
	}
	if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeGitCommitted) {
		return r.handleCommitting(ctx, &upgrade)
	}
	if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeReconciled) {
		return r.handleReconciling(ctx, &upgrade)
	}
	if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeHealthy) {
		return r.handleHealthChecking(ctx, &upgrade)
	}

	// All conditions met - mark complete
	return r.handleCompleted(ctx, &upgrade)
}

// handleDryRun validates the upgrade without making any changes
func (r *UpgradeRequestReconciler) handleDryRun(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("processing dry run")

	// Fetch the ManagedApp
	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
	}

	// Check for available update
	if upgrade.Spec.TargetVersion == nil && app.Status.AvailableUpdate == nil {
		return r.setFailed(ctx, upgrade, "NoUpdateAvailable", "No update available for this app")
	}

	// Determine target version
	targetVersion := upgrade.Spec.TargetVersion
	if targetVersion == nil {
		targetVersion = app.Status.AvailableUpdate
	}

	// Validate Git path is readable
	if r.GitManager != nil {
		_, err := r.GitManager.ReadFile(ctx, app.Spec.GitPath)
		if err != nil {
			return r.setFailed(ctx, upgrade, "GitReadFailed", fmt.Sprintf("Cannot read Git file: %v", err))
		}
	}

	// Mark as complete (dry run successful)
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeComplete,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunSucceeded",
		Message:            fmt.Sprintf("Dry run validation passed. Would upgrade to %s", targetVersion.Chart),
		ObservedGeneration: upgrade.Generation,
	})

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
}

// handleSuspend validates the request and suspends Flux
func (r *UpgradeRequestReconciler) handleSuspend(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Fetch the ManagedApp
	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
	}

	// Check for available update
	if upgrade.Spec.TargetVersion == nil && app.Status.AvailableUpdate == nil {
		return r.setFailed(ctx, upgrade, "NoUpdateAvailable", "No update available for this app")
	}

	// Determine target version
	targetVersion := upgrade.Spec.TargetVersion
	if targetVersion == nil {
		targetVersion = app.Status.AvailableUpdate
	}

	logger.Info("starting upgrade",
		"app", app.Name,
		"currentVersion", app.Status.CurrentVersion,
		"targetVersion", targetVersion)

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
		return r.setFailed(ctx, upgrade, "InvalidSuspendTarget", err.Error())
	}

	// Suspend Flux Kustomization
	if err := r.FluxHelper.SuspendKustomization(ctx, suspendName, suspendNS); err != nil {
		return r.setFailed(ctx, upgrade, "SuspendFailed", err.Error())
	}

	// Set Suspended condition
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             "KustomizationSuspended",
		Message:            fmt.Sprintf("Suspended %s/%s", suspendNS, suspendName),
		ObservedGeneration: upgrade.Generation,
	})

	// If snapshots disabled, also mark SnapshotReady as true (skipped)
	if upgrade.Spec.SkipSnapshot || app.Spec.VolumeSnapshots == nil || !app.Spec.VolumeSnapshots.Enabled {
		meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeSnapshotReady,
			Status:             metav1.ConditionTrue,
			Reason:             "SnapshotsSkipped",
			Message:            "Snapshots disabled or not configured",
			ObservedGeneration: upgrade.Generation,
		})
	}

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// getSuspendTarget returns the name and namespace of the Kustomization to suspend.
// Uses suspendRef if set, otherwise falls back to kustomizationRef.
func (r *UpgradeRequestReconciler) getSuspendTarget(app *fluxupv1alpha1.ManagedApp) (name, namespace string) {
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
func (r *UpgradeRequestReconciler) getSuspendRefStruct(app *fluxupv1alpha1.ManagedApp) *struct{ Name, Namespace string } {
	if app.Spec.SuspendRef == nil {
		return nil
	}
	ns := app.Spec.SuspendRef.Namespace
	if ns == "" {
		ns = DefaultFluxNamespace
	}
	return &struct{ Name, Namespace string }{Name: app.Spec.SuspendRef.Name, Namespace: ns}
}

// handleScaleDown scales workloads to 0 replicas before snapshotting.
// Returns (result, error, shouldContinue). If shouldContinue is true, no workloads
// need scaling and the caller should continue to the next step.
// TODO: Implement auto-discovery of workloads from PVCs (Issue 1)
func (r *UpgradeRequestReconciler) handleScaleDown(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error, bool) {
	logger := logging.FromContext(ctx)

	_, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		result, err := r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
		return result, err, false
	}

	// TODO: Auto-discover workloads from PVCs
	// For now, skip scaling - will be implemented in Issue 1
	logger.Debug("workload auto-discovery not yet implemented, skipping scale down")
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
		Status:             metav1.ConditionTrue,
		Reason:             "Skipped",
		Message:            "Workload auto-discovery not yet implemented",
		ObservedGeneration: upgrade.Generation,
	})
	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err, false
	}
	return ctrl.Result{Requeue: true}, nil, true

}

// handleSnapshotting creates pre-upgrade snapshots
func (r *UpgradeRequestReconciler) handleSnapshotting(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Set phase start time for timeout tracking
	r.setPhaseStartIfNeeded(upgrade)

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
	}

	// Create snapshots if not already created
	if upgrade.Status.Snapshot == nil || len(upgrade.Status.Snapshot.PVCSnapshots) == 0 {
		snapshots, err := r.SnapshotManager.CreateSnapshotsForUpgrade(
			ctx,
			app.Name,
			app.Namespace,
			app.Spec.VolumeSnapshots.PVCs,
			app.Spec.VolumeSnapshots.VolumeSnapshotClassName,
		)
		if err != nil {
			return r.setFailed(ctx, upgrade, "SnapshotCreationFailed", err.Error())
		}

		now := metav1.Now()
		upgrade.Status.Snapshot = &fluxupv1alpha1.SnapshotStatus{
			CreatedAt: &now,
		}
		for _, snap := range snapshots {
			upgrade.Status.Snapshot.PVCSnapshots = append(upgrade.Status.Snapshot.PVCSnapshots,
				fluxupv1alpha1.PVCSnapshotInfo{
					PVCName:      snap.PVCName,
					SnapshotName: snap.SnapshotName,
				})
		}

		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("created pre-upgrade snapshots", "count", len(snapshots))
	}

	// Check if all snapshots are ready (non-blocking check)
	allReady := true
	for _, snap := range upgrade.Status.Snapshot.PVCSnapshots {
		ready, err := r.SnapshotManager.IsSnapshotReady(ctx, snap.SnapshotName, app.Namespace)
		if err != nil {
			return r.setFailed(ctx, upgrade, "SnapshotCheckFailed", err.Error())
		}
		if !ready {
			allReady = false
			break
		}
	}

	if !allReady {
		// Check per-phase timeout for snapshot readiness
		if r.isPhaseTimedOut(upgrade, TimeoutSnapshot) {
			return r.setFailed(ctx, upgrade, "SnapshotTimeout", "Snapshots did not become ready within timeout")
		}
		logger.Debug("waiting for snapshots to become ready")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// All snapshots ready - reset phase timer for next phase
	r.resetPhaseStart(upgrade)

	now := metav1.Now()
	upgrade.Status.Snapshot.ReadyAt = &now
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSnapshotReady,
		Status:             metav1.ConditionTrue,
		Reason:             "SnapshotsReady",
		Message:            fmt.Sprintf("%d snapshots ready", len(upgrade.Status.Snapshot.PVCSnapshots)),
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleCommitting commits the version change to Git
func (r *UpgradeRequestReconciler) handleCommitting(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
	}

	// Re-verify suspend before Git commit (point of no return)
	// This catches external un-suspend attempts before we commit
	suspendName, suspendNS := r.getSuspendTarget(app)
	if err := r.FluxHelper.VerifyStillSuspended(ctx, suspendName, suspendNS); err != nil {
		return r.setFailed(ctx, upgrade, "SuspendVerificationFailed", err.Error())
	}

	// Get target version
	targetVersion := upgrade.Spec.TargetVersion
	if targetVersion == nil {
		targetVersion = app.Status.AvailableUpdate
	}

	// Determine version path and new version value
	versionPath := yamlpkg.DefaultHelmReleaseVersionPath
	if app.Spec.VersionPolicy != nil && app.Spec.VersionPolicy.VersionPath != "" {
		versionPath = app.Spec.VersionPolicy.VersionPath
	}

	// Determine the new version string based on update type (chart or image)
	var newVersionStr string
	var currentVersionStr string
	if targetVersion.Chart != "" {
		newVersionStr = targetVersion.Chart
		if app.Status.CurrentVersion != nil {
			currentVersionStr = app.Status.CurrentVersion.Chart
		}
	} else if len(targetVersion.Images) > 0 {
		// For image updates, use the first image's tag
		// VersionPath must be configured to point to the image tag field
		newVersionStr = targetVersion.Images[0].Tag
		if app.Status.CurrentVersion != nil && len(app.Status.CurrentVersion.Images) > 0 {
			currentVersionStr = app.Status.CurrentVersion.Images[0].Tag
		}
		// Require explicit VersionPath for image updates (no sensible default)
		if app.Spec.VersionPolicy == nil || app.Spec.VersionPolicy.VersionPath == "" {
			return r.setFailed(ctx, upgrade, "MissingVersionPath",
				"VersionPath must be specified in ManagedApp.spec.versionPolicy for image updates")
		}
	} else {
		return r.setFailed(ctx, upgrade, "InvalidTargetVersion", "No chart version or images specified")
	}

	// Read current file from Git
	content, err := r.GitManager.ReadFile(ctx, app.Spec.GitPath)
	if err != nil {
		return r.setFailed(ctx, upgrade, "GitReadFailed", err.Error())
	}

	// Update version in YAML
	newContent, err := r.YAMLEditor.UpdateVersion(content, versionPath, newVersionStr)
	if err != nil {
		return r.setFailed(ctx, upgrade, "YAMLUpdateFailed", err.Error())
	}

	// Build commit message
	var snapshotNames []string
	if upgrade.Status.Snapshot != nil {
		for _, snap := range upgrade.Status.Snapshot.PVCSnapshots {
			snapshotNames = append(snapshotNames, snap.SnapshotName)
		}
	}

	message := git.FormatCommitMessage(app.Name, currentVersionStr, newVersionStr, snapshotNames)

	// Commit to Git
	commitInfo, err := r.GitManager.CommitFile(ctx, git.FileChange{
		Path:    app.Spec.GitPath,
		Content: newContent,
	}, message)
	if err != nil {
		return r.setFailed(ctx, upgrade, "GitCommitFailed", err.Error())
	}

	logger.Info("committed version change to Git",
		"commit", commitInfo.SHA,
		"newVersion", newVersionStr)

	// Update status
	now := metav1.Now()
	upgrade.Status.Upgrade = &fluxupv1alpha1.UpgradeStatus{
		PreviousVersion: app.Status.CurrentVersion,
		NewVersion:      targetVersion,
		GitCommit:       commitInfo.SHA,
		StartedAt:       &now,
	}

	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeGitCommitted,
		Status:             metav1.ConditionTrue,
		Reason:             "Committed",
		Message:            fmt.Sprintf("Commit %s", commitInfo.SHA),
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleReconciling resumes Flux and waits for reconciliation
func (r *UpgradeRequestReconciler) handleReconciling(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Set phase start time for timeout tracking
	r.setPhaseStartIfNeeded(upgrade)

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
	}

	// Resume the Kustomization we suspended (suspendRef or kustomizationRef)
	suspendName, suspendNS := r.getSuspendTarget(app)

	if err := r.FluxHelper.ResumeKustomization(ctx, suspendName, suspendNS); err != nil {
		return r.setFailed(ctx, upgrade, "ResumeFailed", err.Error())
	}

	// Update Suspended condition to False
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSuspended,
		Status:             metav1.ConditionFalse,
		Reason:             "KustomizationResumed",
		Message:            fmt.Sprintf("Resumed %s/%s", suspendNS, suspendName),
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("resumed Flux Kustomization, waiting for reconciliation")

	// Check if reconciliation is complete (non-blocking)
	// Check the kustomizationRef (the actual app's Kustomization), not suspendRef
	ksRef := app.Spec.KustomizationRef
	ksNS := ksRef.Namespace
	if ksNS == "" {
		ksNS = DefaultFluxNamespace
	}
	reconciled, err := r.FluxHelper.IsReconciled(ctx, ksRef.Name, ksNS)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ReconciliationCheckFailed", err.Error())
	}

	if !reconciled {
		// Check per-phase timeout
		if r.isPhaseTimedOut(upgrade, TimeoutReconcile) {
			return r.setFailed(ctx, upgrade, "ReconciliationTimeout", "Kustomization did not reconcile within timeout")
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Reconciliation complete - reset phase timer for next phase
	r.resetPhaseStart(upgrade)

	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeReconciled,
		Status:             metav1.ConditionTrue,
		Reason:             "ReconciliationSucceeded",
		Message:            "Flux reconciled the changes",
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleHealthChecking verifies the upgrade succeeded
func (r *UpgradeRequestReconciler) handleHealthChecking(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Set phase start time for timeout tracking
	r.setPhaseStartIfNeeded(upgrade)

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
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
		// Check per-phase timeout (use configured timeout if available)
		timeout := TimeoutHealthCheck
		if app.Spec.HealthCheck != nil && app.Spec.HealthCheck.Timeout != "" {
			if parsed, err := time.ParseDuration(app.Spec.HealthCheck.Timeout); err == nil {
				timeout = parsed
			}
		}

		if r.isPhaseTimedOut(upgrade, timeout) {
			return r.setFailed(ctx, upgrade, "HealthCheckTimeout", "Workload did not become healthy within timeout")
		}

		logger.Debug("workload not yet healthy, waiting")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Health check passed
	logger.Info("health check passed, upgrade complete")

	now := metav1.Now()
	upgrade.Status.HealthCheck = &fluxupv1alpha1.HealthCheckStatus{
		Status:    "Passed",
		CheckedAt: &now,
	}
	if upgrade.Status.Upgrade != nil {
		upgrade.Status.Upgrade.CompletedAt = &now
	}

	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeHealthy,
		Status:             metav1.ConditionTrue,
		Reason:             "HealthCheckPassed",
		Message:            "Workload is healthy",
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleCompleted marks the upgrade as complete and updates ManagedApp
func (r *UpgradeRequestReconciler) handleCompleted(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
	}

	versionStr := "unknown"
	if upgrade.Status.Upgrade != nil && upgrade.Status.Upgrade.NewVersion != nil {
		versionStr = upgrade.Status.Upgrade.NewVersion.Chart
	}

	logger.Info("upgrade complete", "newVersion", versionStr)

	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeComplete,
		Status:             metav1.ConditionTrue,
		Reason:             "UpgradeSucceeded",
		Message:            fmt.Sprintf("Upgraded to %s", versionStr),
		ObservedGeneration: upgrade.Generation,
	})

	// Update ManagedApp's currentVersion and clear availableUpdate
	if err := r.updateManagedAppAfterUpgrade(ctx, app, upgrade); err != nil {
		logger.Error("failed to update ManagedApp status after upgrade", "error", err)
	}

	// Apply snapshot retention policy (prune old snapshots)
	if app.Spec.VolumeSnapshots != nil && app.Spec.VolumeSnapshots.Enabled {
		r.applySnapshotRetention(ctx, app, logger)
	}

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	// Remove finalizer now that operation is complete
	if controllerutil.ContainsFinalizer(upgrade, OperationFinalizer) {
		controllerutil.RemoveFinalizer(upgrade, OperationFinalizer)
		if err := r.Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// getManagedApp fetches the referenced ManagedApp
func (r *UpgradeRequestReconciler) getManagedApp(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (*fluxupv1alpha1.ManagedApp, error) {
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

// setFailed marks the upgrade as failed via Complete=False condition
// Failure handling depends on whether Git commit has happened (point of no return):
// - Before Git commit: Resume Kustomization, user can retry
// - After Git commit: Rollback required (handled by Phase 3), do NOT resume
func (r *UpgradeRequestReconciler) setFailed(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest, reason, message string) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Error("upgrade failed", "reason", reason, "message", message)

	gitCommitted := meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeGitCommitted)

	if gitCommitted {
		// After Git commit: check if auto-rollback is enabled
		app, err := r.getManagedApp(ctx, upgrade)
		if err == nil && app.Spec.AutoRollback {
			// Create RollbackRequest
			if err := r.createAutoRollback(ctx, upgrade, app); err != nil {
				logger.Error("failed to create auto-rollback", "error", err)
				message = fmt.Sprintf("%s - rollback required (auto-rollback failed: %v)", message, err)
			} else {
				message = fmt.Sprintf("%s - auto-rollback initiated", message)
			}
			logger.Info("upgrade failed after Git commit", "autoRollback", true)
		} else {
			message = fmt.Sprintf("%s - rollback required", message)
			logger.Warn("upgrade failed after Git commit - rollback required")
		}

		meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeComplete,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: upgrade.Generation,
		})
	} else {
		// Before Git commit: safe to resume and abort
		meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeComplete,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: upgrade.Generation,
		})

		// Resume Kustomization if we suspended it
		if meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
			app, err := r.getManagedApp(ctx, upgrade)
			if err == nil {
				// Resume the same Kustomization we suspended
				suspendName, suspendNS := r.getSuspendTarget(app)
				if resumeErr := r.FluxHelper.ResumeKustomization(ctx, suspendName, suspendNS); resumeErr != nil {
					logger.Error("failed to resume Kustomization after failure", "error", resumeErr)
				} else {
					meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
						Type:               fluxupv1alpha1.ConditionTypeSuspended,
						Status:             metav1.ConditionFalse,
						Reason:             "ResumedAfterFailure",
						Message:            "Kustomization resumed after upgrade failure",
						ObservedGeneration: upgrade.Generation,
					})
				}
			}
		}
	}

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	// Remove finalizer now that operation is complete (failed)
	if controllerutil.ContainsFinalizer(upgrade, OperationFinalizer) {
		controllerutil.RemoveFinalizer(upgrade, OperationFinalizer)
		if err := r.Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// createAutoRollback creates a RollbackRequest for the failed upgrade
func (r *UpgradeRequestReconciler) createAutoRollback(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest, app *fluxupv1alpha1.ManagedApp) error {
	logger := logging.FromContext(ctx)

	rollbackName := fmt.Sprintf("%s-auto-rollback-%s", app.Name, time.Now().Format("20060102-150405"))

	rollback := &fluxupv1alpha1.RollbackRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rollbackName,
			Namespace: upgrade.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: fluxupv1alpha1.GroupVersion.String(),
					Kind:       "UpgradeRequest",
					Name:       upgrade.Name,
					UID:        upgrade.UID,
				},
			},
		},
		Spec: fluxupv1alpha1.RollbackRequestSpec{
			UpgradeRequestRef: fluxupv1alpha1.ObjectReference{
				Name: upgrade.Name,
			},
			AutoTriggered: true,
		},
	}

	if err := r.Create(ctx, rollback); err != nil {
		return fmt.Errorf("creating RollbackRequest: %w", err)
	}

	logger.Info("created auto-rollback request", "rollback", rollbackName, "upgrade", upgrade.Name)
	return nil
}

// applySnapshotRetention prunes old snapshots based on the retention policy
func (r *UpgradeRequestReconciler) applySnapshotRetention(ctx context.Context, app *fluxupv1alpha1.ManagedApp, logger *slog.Logger) {
	// Determine maxCount from retention policy, defaulting to 3
	maxCount := snapshot.DefaultMaxSnapshotCount
	if app.Spec.VolumeSnapshots.RetentionPolicy != nil && app.Spec.VolumeSnapshots.RetentionPolicy.MaxCount != nil {
		maxCount = *app.Spec.VolumeSnapshots.RetentionPolicy.MaxCount
	}

	deleted, err := r.SnapshotManager.ApplyRetentionPolicy(ctx, app.Name, app.Namespace, maxCount)
	if err != nil {
		logger.Error("failed to apply snapshot retention policy", "error", err)
		return
	}

	if deleted > 0 {
		logger.Info("pruned old snapshots", "deleted", deleted, "maxCount", maxCount)
	}
}

// updateManagedAppAfterUpgrade updates the ManagedApp status after successful upgrade
func (r *UpgradeRequestReconciler) updateManagedAppAfterUpgrade(ctx context.Context, app *fluxupv1alpha1.ManagedApp, upgrade *fluxupv1alpha1.UpgradeRequest) error {
	if upgrade.Status.Upgrade == nil {
		return nil
	}

	app.Status.CurrentVersion = upgrade.Status.Upgrade.NewVersion
	app.Status.AvailableUpdate = nil

	now := metav1.Now()
	app.Status.LastUpgrade = &fluxupv1alpha1.UpgradeInfo{
		FromVersion: upgrade.Status.Upgrade.PreviousVersion,
		ToVersion:   upgrade.Status.Upgrade.NewVersion,
		Timestamp:   &now,
		Status:      "Succeeded",
	}

	// Clear UpdateAvailable condition
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeUpdateAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             "UpgradeCompleted",
		Message:            "No pending updates",
		ObservedGeneration: app.Generation,
	})

	return r.Status().Update(ctx, app)
}

// SetupWithManager configures the controller
func (r *UpgradeRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fluxupv1alpha1.UpgradeRequest{}).
		Named("upgraderequest").
		Complete(r)
}
