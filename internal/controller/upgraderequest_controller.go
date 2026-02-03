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

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/flux"
	"github.com/nbenn/fluxup/internal/git"
	"github.com/nbenn/fluxup/internal/snapshot"
	yamlpkg "github.com/nbenn/fluxup/internal/yaml"
)

// DefaultFluxNamespace is the default namespace for Flux resources.
const DefaultFluxNamespace = "flux-system"

// UpgradeRequestReconciler reconciles an UpgradeRequest
type UpgradeRequestReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	GitManager      git.Manager
	SnapshotManager *snapshot.Manager
	FluxHelper      *flux.Helper
	YAMLEditor      *yamlpkg.Editor
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
	logger := log.FromContext(ctx)

	// 1. Fetch UpgradeRequest
	var upgrade fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, req.NamespacedName, &upgrade); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling UpgradeRequest",
		"managedApp", upgrade.Spec.ManagedAppRef.Name)

	// 2. Check if already complete (terminal state)
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
	logger := log.FromContext(ctx)
	logger.Info("Processing dry run")

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

	return ctrl.Result{}, nil
}

// handleSuspend validates the request and suspends Flux
func (r *UpgradeRequestReconciler) handleSuspend(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

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

	logger.Info("Starting upgrade",
		"app", app.Name,
		"currentVersion", app.Status.CurrentVersion,
		"targetVersion", targetVersion)

	// Suspend Flux Kustomization
	ksRef := app.Spec.KustomizationRef
	ksNS := ksRef.Namespace
	if ksNS == "" {
		ksNS = DefaultFluxNamespace
	}

	if err := r.FluxHelper.SuspendKustomization(ctx, ksRef.Name, ksNS); err != nil {
		return r.setFailed(ctx, upgrade, "SuspendFailed", err.Error())
	}

	// Set Suspended condition
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             "KustomizationSuspended",
		Message:            fmt.Sprintf("Suspended %s/%s", ksNS, ksRef.Name),
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

// handleSnapshotting creates pre-upgrade snapshots
func (r *UpgradeRequestReconciler) handleSnapshotting(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

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

		logger.Info("Created pre-upgrade snapshots", "count", len(snapshots))
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
		logger.Info("Waiting for snapshots to become ready")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// All snapshots ready
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
	logger := log.FromContext(ctx)

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
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

	logger.Info("Committed version change to Git",
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
	logger := log.FromContext(ctx)

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
	}

	// Resume Flux Kustomization
	ksRef := app.Spec.KustomizationRef
	ksNS := ksRef.Namespace
	if ksNS == "" {
		ksNS = DefaultFluxNamespace
	}

	if err := r.FluxHelper.ResumeKustomization(ctx, ksRef.Name, ksNS); err != nil {
		return r.setFailed(ctx, upgrade, "ResumeFailed", err.Error())
	}

	// Update Suspended condition to False
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSuspended,
		Status:             metav1.ConditionFalse,
		Reason:             "KustomizationResumed",
		Message:            fmt.Sprintf("Resumed %s/%s", ksNS, ksRef.Name),
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Resumed Flux Kustomization, waiting for reconciliation")

	// Check if reconciliation is complete (non-blocking)
	reconciled, err := r.FluxHelper.IsReconciled(ctx, ksRef.Name, ksNS)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ReconciliationCheckFailed", err.Error())
	}

	if !reconciled {
		// Check timeout
		timeout := 10 * time.Minute
		if upgrade.Status.Upgrade != nil && upgrade.Status.Upgrade.StartedAt != nil {
			elapsed := time.Since(upgrade.Status.Upgrade.StartedAt.Time)
			if elapsed > timeout {
				return r.setFailed(ctx, upgrade, "ReconciliationTimeout", "Kustomization did not reconcile within timeout")
			}
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Reconciliation complete
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
	logger := log.FromContext(ctx)

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
		// Check timeout
		timeout := 5 * time.Minute
		if app.Spec.HealthCheck != nil && app.Spec.HealthCheck.Timeout != "" {
			if parsed, err := time.ParseDuration(app.Spec.HealthCheck.Timeout); err == nil {
				timeout = parsed
			}
		}

		if upgrade.Status.Upgrade != nil && upgrade.Status.Upgrade.StartedAt != nil {
			elapsed := time.Since(upgrade.Status.Upgrade.StartedAt.Time)
			if elapsed > timeout {
				return r.setFailed(ctx, upgrade, "HealthCheckTimeout", "Workload did not become healthy within timeout")
			}
		}

		logger.Info("Workload not yet healthy, waiting...")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Health check passed
	logger.Info("Health check passed, upgrade complete")

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
	logger := log.FromContext(ctx)

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
	}

	versionStr := "unknown"
	if upgrade.Status.Upgrade != nil && upgrade.Status.Upgrade.NewVersion != nil {
		versionStr = upgrade.Status.Upgrade.NewVersion.Chart
	}

	logger.Info("Upgrade complete", "newVersion", versionStr)

	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeComplete,
		Status:             metav1.ConditionTrue,
		Reason:             "UpgradeSucceeded",
		Message:            fmt.Sprintf("Upgraded to %s", versionStr),
		ObservedGeneration: upgrade.Generation,
	})

	// Update ManagedApp's currentVersion and clear availableUpdate
	if err := r.updateManagedAppAfterUpgrade(ctx, app, upgrade); err != nil {
		logger.Error(err, "Failed to update ManagedApp status after upgrade")
	}

	// Apply snapshot retention policy (prune old snapshots)
	if app.Spec.VolumeSnapshots != nil && app.Spec.VolumeSnapshots.Enabled {
		r.applySnapshotRetention(ctx, app, logger)
	}

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
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
	logger := log.FromContext(ctx)
	logger.Error(nil, "Upgrade failed", "reason", reason, "message", message)

	gitCommitted := meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeGitCommitted)

	if gitCommitted {
		// After Git commit: rollback required
		// Do NOT resume - leave state as-is for rollback
		// Phase 3 will add auto-rollback here
		meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeComplete,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            fmt.Sprintf("%s - rollback required", message),
			ObservedGeneration: upgrade.Generation,
		})
		logger.Info("Upgrade failed after Git commit - rollback required")
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
				ksRef := app.Spec.KustomizationRef
				ksNS := ksRef.Namespace
				if ksNS == "" {
					ksNS = DefaultFluxNamespace
				}
				if resumeErr := r.FluxHelper.ResumeKustomization(ctx, ksRef.Name, ksNS); resumeErr != nil {
					logger.Error(resumeErr, "Failed to resume Kustomization after failure")
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

	return ctrl.Result{}, nil
}

// applySnapshotRetention prunes old snapshots based on the retention policy
func (r *UpgradeRequestReconciler) applySnapshotRetention(ctx context.Context, app *fluxupv1alpha1.ManagedApp, logger logr.Logger) {
	// Determine maxCount from retention policy, defaulting to 3
	maxCount := snapshot.DefaultMaxSnapshotCount
	if app.Spec.VolumeSnapshots.RetentionPolicy != nil && app.Spec.VolumeSnapshots.RetentionPolicy.MaxCount != nil {
		maxCount = *app.Spec.VolumeSnapshots.RetentionPolicy.MaxCount
	}

	deleted, err := r.SnapshotManager.ApplyRetentionPolicy(ctx, app.Name, app.Namespace, maxCount)
	if err != nil {
		logger.Error(err, "Failed to apply snapshot retention policy")
		return
	}

	if deleted > 0 {
		logger.Info("Pruned old snapshots", "deleted", deleted, "maxCount", maxCount)
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
