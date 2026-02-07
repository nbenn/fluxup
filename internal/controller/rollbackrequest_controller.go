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
	"github.com/nbenn/fluxup/internal/discovery"
	"github.com/nbenn/fluxup/internal/flux"
	"github.com/nbenn/fluxup/internal/git"
	"github.com/nbenn/fluxup/internal/health"
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
	Discoverer      *discovery.Discoverer
	HealthChecker   *health.Checker
}

// setPhaseStartIfNeeded sets PhaseStartedAt if not already set for this reconcile cycle.
// Call this at the beginning of each phase handler when entering a new phase.
func (r *RollbackRequestReconciler) setPhaseStartIfNeeded(rollback *fluxupv1alpha1.RollbackRequest) {
	if rollback.Status.PhaseStartedAt == nil {
		now := metav1.Now()
		rollback.Status.PhaseStartedAt = &now
	}
}

// resetPhaseStart clears PhaseStartedAt to mark the transition to a new phase.
// Call this when a phase completes successfully before moving to the next phase.
func (r *RollbackRequestReconciler) resetPhaseStart(rollback *fluxupv1alpha1.RollbackRequest) {
	rollback.Status.PhaseStartedAt = nil
}

// isPhaseTimedOut checks if the current phase has exceeded its timeout.
func (r *RollbackRequestReconciler) isPhaseTimedOut(rollback *fluxupv1alpha1.RollbackRequest, timeout time.Duration) bool {
	if rollback.Status.PhaseStartedAt == nil {
		return false
	}
	return time.Since(rollback.Status.PhaseStartedAt.Time) > timeout
}

// runRollbackPreflight runs read-only preflight checks for a rollback operation.
func (r *RollbackRequestReconciler) runRollbackPreflight(ctx context.Context, app *fluxupv1alpha1.ManagedApp, upgrade *fluxupv1alpha1.UpgradeRequest) (*preflightResult, error) {
	logger := logging.FromContext(ctx)
	result := &preflightResult{}

	// --- Flux preflight ---

	ksRef := app.Spec.KustomizationRef
	ksNS := ksRef.Namespace
	if ksNS == "" {
		ksNS = DefaultFluxNamespace
	}

	if err := r.FluxHelper.ValidateSuspendTarget(ctx,
		&struct{ Name, Namespace string }{Name: ksRef.Name, Namespace: ksNS},
		r.getSuspendRefStruct(app)); err != nil {
		return nil, fmt.Errorf("invalid suspend target: %w", err)
	}

	reconciled, err := r.FluxHelper.IsReconciled(ctx, ksRef.Name, ksNS)
	if err != nil {
		logger.Warn("preflight: could not check Kustomization health", "error", err)
	} else if !reconciled {
		msg := fmt.Sprintf("Kustomization %s/%s is not Ready — operation may fail at reconciliation", ksNS, ksRef.Name)
		logger.Warn("preflight: " + msg)
		result.Warnings = append(result.Warnings, msg)
	}

	suspendName, suspendNS := r.getSuspendTarget(app)
	suspended, err := r.FluxHelper.IsSuspended(ctx, suspendName, suspendNS)
	if err != nil {
		logger.Warn("preflight: could not check Kustomization suspension state", "error", err)
	} else if suspended {
		msg := fmt.Sprintf("Kustomization %s/%s is already suspended — operation will resume it upon completion", suspendNS, suspendName)
		logger.Warn("preflight: " + msg)
		result.Warnings = append(result.Warnings, msg)
	}

	ksStatus := "Ready"
	if !reconciled {
		ksStatus = reasonNotReady
	}

	// --- Snapshot restore preflight (when snapshots present) ---
	hasSnapshots := upgrade.Status.Snapshot != nil && len(upgrade.Status.Snapshot.PVCSnapshots) > 0 && !upgrade.Spec.SkipSnapshot

	if hasSnapshots {
		// Verify each snapshot still exists and is ready
		for _, snap := range upgrade.Status.Snapshot.PVCSnapshots {
			ready, err := r.SnapshotManager.IsSnapshotReady(ctx, snap.SnapshotName, app.Namespace)
			if err != nil {
				return nil, fmt.Errorf("snapshot %s no longer exists or cannot be checked: %w", snap.SnapshotName, err)
			}
			if !ready {
				return nil, fmt.Errorf("snapshot %s is not ready", snap.SnapshotName)
			}
		}

		// Discover workloads for PVCs
		pvcNames := make([]string, len(upgrade.Status.Snapshot.PVCSnapshots))
		for i, snap := range upgrade.Status.Snapshot.PVCSnapshots {
			pvcNames[i] = snap.PVCName
		}

		workloads, err := r.Discoverer.DiscoverWorkloadsForPVCs(ctx, pvcNames, app.Namespace)
		if err != nil {
			return nil, fmt.Errorf("workload discovery for PVCs failed: %w", err)
		}
		result.Workloads = workloads

		if len(workloads) == 0 {
			logger.Info("preflight: no workloads discovered mounting the PVCs")
		} else {
			wNames := make([]string, len(workloads))
			for i, w := range workloads {
				wNames[i] = w.Kind + "/" + w.Name
			}
			logger.Info("preflight: workloads to scale down before PVC restore", "count", len(workloads), "workloads", wNames)
		}

		result.Summary = append(result.Summary, fmt.Sprintf("Kustomization %s/%s %s. %d snapshots to restore, %d workloads to scale",
			ksNS, ksRef.Name, ksStatus, len(upgrade.Status.Snapshot.PVCSnapshots), len(workloads)))
	} else {
		result.Summary = append(result.Summary, fmt.Sprintf("Kustomization %s/%s %s. No snapshots to restore",
			ksNS, ksRef.Name, ksStatus))
	}

	return result, nil
}

// runRollbackGitDiffPreview reads the current file and computes the version revert without committing.
func (r *RollbackRequestReconciler) runRollbackGitDiffPreview(ctx context.Context, app *fluxupv1alpha1.ManagedApp, upgrade *fluxupv1alpha1.UpgradeRequest) (currentVersion, revertVersion, versionPath string, err error) {
	logger := logging.FromContext(ctx)

	targetVersion := upgrade.Status.Upgrade.PreviousVersion
	if targetVersion == nil {
		return "", "", "", fmt.Errorf("no previous version recorded")
	}

	versionPath = yamlpkg.DefaultHelmReleaseVersionPath
	if app.Spec.VersionPolicy != nil && app.Spec.VersionPolicy.VersionPath != "" {
		versionPath = app.Spec.VersionPolicy.VersionPath
	}

	if targetVersion.Chart != "" {
		revertVersion = targetVersion.Chart
	} else if len(targetVersion.Images) > 0 {
		revertVersion = targetVersion.Images[0].Tag
	} else {
		return "", "", "", fmt.Errorf("no chart version or images in previous version")
	}

	content, err := r.GitManager.ReadFile(ctx, app.Spec.GitPath)
	if err != nil {
		return "", "", "", fmt.Errorf("cannot read Git file %s: %w", app.Spec.GitPath, err)
	}

	currentVersion, err = r.YAMLEditor.GetVersion(content, versionPath)
	if err != nil {
		return "", "", "", fmt.Errorf("cannot read version at path %s: %w", versionPath, err)
	}

	if _, err := r.YAMLEditor.UpdateVersion(content, versionPath, revertVersion); err != nil {
		return "", "", "", fmt.Errorf("version revert would produce invalid YAML: %w", err)
	}

	logger.Info("version change preview",
		"file", app.Spec.GitPath,
		"path", versionPath,
		"current", currentVersion,
		"target", revertVersion)

	return currentVersion, revertVersion, versionPath, nil
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
	// Note: We check if the Suspended condition EXISTS (not just if it's True) because
	// the condition transitions from True (suspended) to False (resumed) during the flow.
	// We only want to suspend once at the beginning; once the condition is set, skip this phase.
	if meta.FindStatusCondition(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) == nil {
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

// handleDryRun validates the rollback and exercises the suspend/scale cycle without restoring.
func (r *RollbackRequestReconciler) handleDryRun(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("processing dry run")

	// --- Initial validation (existing) ---

	upgrade, err := r.getUpgradeRequest(ctx, rollback)
	if err != nil {
		return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
	}

	if !r.isUpgradeTerminal(upgrade) {
		return r.setFailed(ctx, rollback, "UpgradeInProgress", "Cannot rollback an in-progress upgrade")
	}

	if upgrade.Status.Snapshot == nil || len(upgrade.Status.Snapshot.PVCSnapshots) == 0 {
		return r.setFailed(ctx, rollback, "NoSnapshotsAvailable", "UpgradeRequest has no snapshots to restore from")
	}

	if upgrade.Status.Upgrade == nil || upgrade.Status.Upgrade.PreviousVersion == nil {
		return r.setFailed(ctx, rollback, "NoPreviousVersion", "UpgradeRequest has no previous version recorded")
	}

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
	}

	// --- Git diff preview (NEW — all operations) ---

	var currentVersion, revertVersion, versionPath string
	if r.GitManager != nil {
		currentVersion, revertVersion, versionPath, err = r.runRollbackGitDiffPreview(ctx, app, upgrade)
		if err != nil {
			return r.setFailed(ctx, rollback, "PreflightFailed", err.Error())
		}
	}

	// --- Preflight checks (NEW — all operations) ---

	preflight, err := r.runRollbackPreflight(ctx, app, upgrade)
	if err != nil {
		return r.setFailed(ctx, rollback, "PreflightFailed", err.Error())
	}

	// --- Dry-run quiescence cycle (NEW — dry-run only) ---

	return r.runRollbackDryRunQuiescence(ctx, rollback, app, preflight, currentVersion, revertVersion, versionPath)
}

// runRollbackDryRunQuiescence exercises the suspend/scale cycle for rollback dry-run.
func (r *RollbackRequestReconciler) runRollbackDryRunQuiescence(
	ctx context.Context,
	rollback *fluxupv1alpha1.RollbackRequest,
	app *fluxupv1alpha1.ManagedApp,
	preflight *preflightResult,
	currentVersion, revertVersion, versionPath string,
) (ctrl.Result, error) {
	suspendName, suspendNS := r.getSuspendTarget(app)

	// Phase 1: Suspend Kustomization
	if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
		return r.rollbackDryRunSuspend(ctx, rollback, app, suspendName, suspendNS)
	}

	// Phase 2: Scale down workloads (if any discovered)
	if len(preflight.Workloads) > 0 && !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeWorkloadStopped) {
		return r.rollbackDryRunScaleDown(ctx, rollback, app, preflight.Workloads)
	}

	// Phase 3: Verify still suspended, then scale up
	if len(preflight.Workloads) > 0 && !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeVolumesRestored) {
		return r.rollbackDryRunScaleUp(ctx, rollback, app, preflight.Workloads, suspendName, suspendNS)
	}

	// Phase 4: Resume Kustomization and complete
	return r.rollbackDryRunResumeAndComplete(ctx, rollback, app, preflight, suspendName, suspendNS, currentVersion, revertVersion, versionPath)
}

// rollbackDryRunSuspend handles Phase 1 of the rollback dry-run: suspending the Kustomization.
func (r *RollbackRequestReconciler) rollbackDryRunSuspend(
	ctx context.Context,
	rollback *fluxupv1alpha1.RollbackRequest,
	app *fluxupv1alpha1.ManagedApp,
	suspendName, suspendNS string,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	r.setPhaseStartIfNeeded(rollback)

	if err := r.FluxHelper.SuspendKustomization(ctx, suspendName, suspendNS); err != nil {
		if r.isPhaseTimedOut(rollback, TimeoutSuspend) {
			return r.setRollbackDryRunFailed(ctx, rollback, app, fmt.Sprintf("Suspend timed out: %v", err))
		}
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	suspended, err := r.FluxHelper.IsSuspended(ctx, suspendName, suspendNS)
	if err != nil || !suspended {
		if r.isPhaseTimedOut(rollback, TimeoutSuspend) {
			return r.setRollbackDryRunFailed(ctx, rollback, app, "Suspend verification timed out")
		}
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	r.resetPhaseStart(rollback)
	logger.Info("dry run: Kustomization suspended")

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunSuspended",
		Message:            fmt.Sprintf("Dry run: suspended %s/%s", suspendNS, suspendName),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// rollbackDryRunScaleDown handles Phase 2: scaling down workloads with RWO PVCs.
func (r *RollbackRequestReconciler) rollbackDryRunScaleDown(
	ctx context.Context,
	rollback *fluxupv1alpha1.RollbackRequest,
	app *fluxupv1alpha1.ManagedApp,
	workloads []discovery.WorkloadInfo,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	r.setPhaseStartIfNeeded(rollback)

	for _, w := range workloads {
		ns := workloadNS(w, app.Namespace)
		if _, err := r.WorkloadScaler.ScaleDown(ctx, w.Kind, w.Name, ns); err != nil {
			return r.setRollbackDryRunFailed(ctx, rollback, app, fmt.Sprintf("Scale down failed for %s/%s: %v", w.Kind, w.Name, err))
		}
	}

	allDown, err := r.allWorkloadsScaledDown(ctx, workloads, app.Namespace)
	if err != nil {
		return r.setRollbackDryRunFailed(ctx, rollback, app, fmt.Sprintf("Scale down check failed: %v", err))
	}
	if !allDown {
		if r.isPhaseTimedOut(rollback, TimeoutScaleDown) {
			return r.setRollbackDryRunFailed(ctx, rollback, app, "Scale down timed out")
		}
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	r.resetPhaseStart(rollback)
	logger.Info("dry run: workloads scaled down", "count", len(workloads))

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunScaledDown",
		Message:            fmt.Sprintf("Dry run: scaled down %d workloads", len(workloads)),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// rollbackDryRunScaleUp handles Phase 3: verifying suspension, scaling up, and re-verifying.
func (r *RollbackRequestReconciler) rollbackDryRunScaleUp(
	ctx context.Context,
	rollback *fluxupv1alpha1.RollbackRequest,
	app *fluxupv1alpha1.ManagedApp,
	workloads []discovery.WorkloadInfo,
	suspendName, suspendNS string,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	if err := r.FluxHelper.VerifyStillSuspended(ctx, suspendName, suspendNS); err != nil {
		return r.setRollbackDryRunFailed(ctx, rollback, app, fmt.Sprintf("External interference detected before scale-up: %v", err))
	}

	r.setPhaseStartIfNeeded(rollback)

	for _, w := range workloads {
		ns := workloadNS(w, app.Namespace)
		if err := r.WorkloadScaler.ScaleUp(ctx, w.Kind, w.Name, ns, 1); err != nil {
			return r.setRollbackDryRunFailed(ctx, rollback, app, fmt.Sprintf("Scale up failed for %s/%s: %v", w.Kind, w.Name, err))
		}
	}

	allDown, err := r.allWorkloadsScaledDown(ctx, workloads, app.Namespace)
	if err != nil {
		return r.setRollbackDryRunFailed(ctx, rollback, app, fmt.Sprintf("Scale up check failed: %v", err))
	}
	if allDown {
		// Not all scaled up yet
		if r.isPhaseTimedOut(rollback, TimeoutScaleUp) {
			return r.setRollbackDryRunFailed(ctx, rollback, app, "Scale up timed out")
		}
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	r.resetPhaseStart(rollback)
	logger.Info("dry run: workloads scaled back up", "count", len(workloads))

	if err := r.FluxHelper.VerifyStillSuspended(ctx, suspendName, suspendNS); err != nil {
		return r.setRollbackDryRunFailed(ctx, rollback, app, fmt.Sprintf("External interference detected after scale-up: %v", err))
	}

	// Reuse VolumesRestored to signal scale-up complete in dry-run
	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeVolumesRestored,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunScaledUp",
		Message:            fmt.Sprintf("Dry run: scaled up %d workloads", len(workloads)),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// rollbackDryRunResumeAndComplete handles Phase 4: resuming the Kustomization and building the summary.
func (r *RollbackRequestReconciler) rollbackDryRunResumeAndComplete(
	ctx context.Context,
	rollback *fluxupv1alpha1.RollbackRequest,
	app *fluxupv1alpha1.ManagedApp,
	preflight *preflightResult,
	suspendName, suspendNS string,
	currentVersion, revertVersion, versionPath string,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	r.setPhaseStartIfNeeded(rollback)

	if err := r.FluxHelper.ResumeKustomization(ctx, suspendName, suspendNS); err != nil {
		if r.isPhaseTimedOut(rollback, TimeoutResume) {
			return r.setRollbackDryRunFailed(ctx, rollback, app, fmt.Sprintf("Resume timed out: %v", err))
		}
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	suspended, err := r.FluxHelper.IsSuspended(ctx, suspendName, suspendNS)
	if err != nil || suspended {
		if r.isPhaseTimedOut(rollback, TimeoutResume) {
			return r.setRollbackDryRunFailed(ctx, rollback, app, "Resume verification timed out")
		}
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	r.resetPhaseStart(rollback)
	logger.Info("dry run: Kustomization resumed")

	message := buildDryRunSummary("rollback", preflight, currentVersion, revertVersion, versionPath, app.Spec.GitPath)

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeComplete,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunSucceeded",
		Message:            message,
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err
	}

	if controllerutil.ContainsFinalizer(rollback, OperationFinalizer) {
		controllerutil.RemoveFinalizer(rollback, OperationFinalizer)
		if err := r.Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// allWorkloadsScaledDown checks if all workloads in the list are scaled to zero.
func (r *RollbackRequestReconciler) allWorkloadsScaledDown(ctx context.Context, workloads []discovery.WorkloadInfo, defaultNS string) (bool, error) {
	for _, w := range workloads {
		ns := workloadNS(w, defaultNS)
		down, err := r.WorkloadScaler.IsScaledDown(ctx, w.Kind, w.Name, ns)
		if err != nil {
			return false, err
		}
		if !down {
			return false, nil
		}
	}
	return true, nil
}

// setRollbackDryRunFailed marks the rollback dry-run as failed, attempting to resume the Kustomization first.
func (r *RollbackRequestReconciler) setRollbackDryRunFailed(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest, app *fluxupv1alpha1.ManagedApp, message string) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	if meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
		suspendName, suspendNS := r.getSuspendTarget(app)
		if resumeErr := r.FluxHelper.ResumeKustomization(ctx, suspendName, suspendNS); resumeErr != nil {
			logger.Error("dry run: failed to resume Kustomization during cleanup", "error", resumeErr)
			message = fmt.Sprintf("%s (WARNING: Kustomization may still be suspended — manual resume required)", message)
		} else {
			logger.Info("dry run: resumed Kustomization during cleanup")
		}
	}

	return r.setFailed(ctx, rollback, "DryRunFailed", message)
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

	// --- Git diff preview (runs for all operations) ---
	if r.GitManager != nil {
		if _, _, _, err := r.runRollbackGitDiffPreview(ctx, app, upgrade); err != nil {
			return r.setFailed(ctx, rollback, "PreflightFailed", err.Error())
		}
	}

	// --- Preflight checks (runs for all operations) ---
	if _, err := r.runRollbackPreflight(ctx, app, upgrade); err != nil {
		return r.setFailed(ctx, rollback, "PreflightFailed", err.Error())
	}

	logger.Info("starting rollback",
		"app", app.Name,
		"fromVersion", r.getVersionString(upgrade.Status.Upgrade.NewVersion),
		"toVersion", r.getVersionString(upgrade.Status.Upgrade.PreviousVersion))

	// Determine which Kustomization to suspend (suspendRef or kustomizationRef)
	suspendName, suspendNS := r.getSuspendTarget(app)

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

// handleStopWorkload scales workloads to 0 replicas before volume restore.
// Returns (result, error, shouldContinue). If shouldContinue is true, no workloads
// need scaling and the caller should continue to the next step.
func (r *RollbackRequestReconciler) handleStopWorkload(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error, bool) {
	logger := logging.FromContext(ctx)

	// Set phase start time for timeout tracking
	r.setPhaseStartIfNeeded(rollback)

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

	// Skip scaling if there are no snapshots to restore (no need to stop workloads)
	if upgrade.Spec.SkipSnapshot || upgrade.Status.Snapshot == nil || len(upgrade.Status.Snapshot.PVCSnapshots) == 0 {
		logger.Debug("no snapshots to restore, skipping workload stop")
		meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
			Status:             metav1.ConditionTrue,
			Reason:             "NoVolumesToRestore",
			Message:            "Workload stop skipped (no volumes to restore)",
			ObservedGeneration: rollback.Generation,
		})
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err, false
		}
		return ctrl.Result{Requeue: true}, nil, true
	}

	// Get PVC names from the upgrade's snapshot list
	pvcNames := make([]string, len(upgrade.Status.Snapshot.PVCSnapshots))
	for i, snap := range upgrade.Status.Snapshot.PVCSnapshots {
		pvcNames[i] = snap.PVCName
	}

	// Discover workloads that mount these PVCs
	workloads, err := r.Discoverer.DiscoverWorkloadsForPVCs(ctx, pvcNames, app.Namespace)
	if err != nil {
		result, err := r.setFailed(ctx, rollback, "WorkloadDiscoveryFailed", err.Error())
		return result, err, false
	}

	if len(workloads) == 0 {
		logger.Debug("no workloads discovered mounting the PVCs, skipping stop")
		meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
			Status:             metav1.ConditionTrue,
			Reason:             "NoWorkloadsFound",
			Message:            "No workloads found mounting the PVCs",
			ObservedGeneration: rollback.Generation,
		})
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err, false
		}
		return ctrl.Result{Requeue: true}, nil, true
	}

	logger.Info("discovered workloads for stop", "count", len(workloads))

	// Scale down each workload
	scaledWorkloads := make([]fluxupv1alpha1.WorkloadScalingInfo, 0, len(workloads))
	for _, w := range workloads {
		ns := w.Namespace
		if ns == "" {
			ns = app.Namespace
		}

		// Scale to 0 (we ignore the returned ScaleInfo since Flux handles replica restoration)
		if _, err := r.WorkloadScaler.ScaleDown(ctx, w.Kind, w.Name, ns); err != nil {
			result, err := r.setFailed(ctx, rollback, "ScaleDownFailed",
				fmt.Sprintf("Failed to scale down %s/%s: %v", ns, w.Name, err))
			return result, err, false
		}

		scaledWorkloads = append(scaledWorkloads, fluxupv1alpha1.WorkloadScalingInfo{
			Kind:      w.Kind,
			Name:      w.Name,
			Namespace: ns,
		})
	}

	// Initialize scaling status if needed
	if rollback.Status.Scaling == nil {
		rollback.Status.Scaling = &fluxupv1alpha1.ScalingStatus{}
	}
	rollback.Status.Scaling.Workloads = scaledWorkloads

	// Check if all workloads are scaled down (non-blocking)
	allScaledDown := true
	for _, w := range scaledWorkloads {
		scaledDown, err := r.WorkloadScaler.IsScaledDown(ctx, w.Kind, w.Name, w.Namespace)
		if err != nil {
			result, err := r.setFailed(ctx, rollback, "ScaleDownCheckFailed", err.Error())
			return result, err, false
		}
		if !scaledDown {
			allScaledDown = false
			break
		}
	}

	if !allScaledDown {
		// Check per-phase timeout
		if r.isPhaseTimedOut(rollback, TimeoutScaleDown) {
			result, err := r.setFailed(ctx, rollback, "ScaleDownTimeout", "Workloads did not scale down within timeout")
			return result, err, false
		}
		logger.Debug("waiting for workloads to scale down")
		if err := r.Status().Update(ctx, rollback); err != nil {
			return ctrl.Result{}, err, false
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, false
	}

	// All workloads scaled down - reset phase timer for next phase
	r.resetPhaseStart(rollback)

	now := metav1.Now()
	rollback.Status.Scaling.ScaledDownAt = &now
	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
		Status:             metav1.ConditionTrue,
		Reason:             "WorkloadsStopped",
		Message:            fmt.Sprintf("Scaled down %d workloads", len(scaledWorkloads)),
		ObservedGeneration: rollback.Generation,
	})

	if err := r.Status().Update(ctx, rollback); err != nil {
		return ctrl.Result{}, err, false
	}

	logger.Info("all workloads scaled down", "count", len(scaledWorkloads))
	return ctrl.Result{Requeue: true}, nil, false
}

// handleVolumeRestore deletes current PVCs and creates new ones from snapshots
func (r *RollbackRequestReconciler) handleVolumeRestore(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Set phase start time for timeout tracking
	r.setPhaseStartIfNeeded(rollback)

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

		// Wait for PVC to be fully deleted (non-blocking check with phase timeout)
		deleted, err := r.SnapshotManager.IsPVCDeleted(ctx, snapInfo.PVCName, app.Namespace)
		if err != nil {
			return r.setFailed(ctx, rollback, "PVCDeleteCheckFailed", err.Error())
		}
		if !deleted {
			if r.isPhaseTimedOut(rollback, TimeoutVolumeRestore) {
				return r.setFailed(ctx, rollback, "PVCDeleteTimeout", "PVC deletion timed out")
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		// Create new PVC from snapshot
		_, err = r.SnapshotManager.RestorePVCFromSnapshot(ctx, snapshot.RestoreRequest{
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

		// Check if PVC is bound (non-blocking check with phase timeout)
		bound, err := r.SnapshotManager.IsPVCBound(ctx, restored.PVCName, app.Namespace)
		if err != nil {
			return r.setFailed(ctx, rollback, "PVCBoundCheckFailed", err.Error())
		}
		if !bound {
			if r.isPhaseTimedOut(rollback, TimeoutVolumeRestore) {
				return r.setFailed(ctx, rollback, "PVCBindTimeout", "PVC binding timed out")
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		rollback.Status.VolumeRestore.RestoredPVCs[i].RestorationState = "Restored"
	}

	// All PVCs restored - reset phase timer for next phase
	r.resetPhaseStart(rollback)

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

	// Set phase start time for timeout tracking
	r.setPhaseStartIfNeeded(rollback)

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
		// Check per-phase timeout
		if r.isPhaseTimedOut(rollback, TimeoutReconcile) {
			return r.setFailed(ctx, rollback, "ReconciliationTimeout", "Kustomization did not reconcile within timeout")
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Reconciliation complete - reset phase timer for next phase
	r.resetPhaseStart(rollback)

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

	// Set phase start time for timeout tracking
	r.setPhaseStartIfNeeded(rollback)

	upgrade, err := r.getUpgradeRequest(ctx, rollback)
	if err != nil {
		return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
	}

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
	}

	// Perform direct health check (real-time, authoritative)
	// This checks both Flux resource AND workload readiness
	healthResult, err := r.HealthChecker.CheckHealth(ctx, app)
	if err != nil {
		return r.setFailed(ctx, rollback, "HealthCheckError", err.Error())
	}

	if !healthResult.Healthy {
		// Check per-phase timeout (use configured timeout if available)
		timeout := TimeoutHealthCheck
		if app.Spec.HealthCheck != nil && app.Spec.HealthCheck.Timeout != "" {
			if parsed, parseErr := time.ParseDuration(app.Spec.HealthCheck.Timeout); parseErr == nil {
				timeout = parsed
			}
		}

		if r.isPhaseTimedOut(rollback, timeout) {
			return r.setFailed(ctx, rollback, "HealthCheckTimeout",
				fmt.Sprintf("App did not become healthy within timeout: %s", healthResult.Message))
		}

		logger.Debug("app not yet healthy, waiting", "message", healthResult.Message)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Health check passed
	logger.Info("health check passed, rollback complete")

	meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeHealthy,
		Status:             metav1.ConditionTrue,
		Reason:             "HealthCheckPassed",
		Message:            healthResult.Message,
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
