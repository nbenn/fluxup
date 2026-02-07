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
	"strings"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
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

// DefaultFluxNamespace is the default namespace for Flux resources.
const DefaultFluxNamespace = "flux-system"

// OperationFinalizer is used to prevent deletion of in-progress operations.
const OperationFinalizer = "fluxup.dev/operation-protection"

// reasonNotReady is a shared string used as a condition reason or status label.
const reasonNotReady = "NotReady"

// Phase timeout defaults (conservative values)
const (
	TimeoutSuspend       = 2 * time.Minute
	TimeoutScaleDown     = 5 * time.Minute
	TimeoutScaleUp       = 5 * time.Minute
	TimeoutSnapshot      = 30 * time.Minute
	TimeoutGitCommit     = 2 * time.Minute
	TimeoutReconcile     = 10 * time.Minute
	TimeoutHealthCheck   = 5 * time.Minute
	TimeoutVolumeRestore = 30 * time.Minute
	TimeoutGitRevert     = 2 * time.Minute
	TimeoutResume        = 2 * time.Minute
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
	Discoverer      *discovery.Discoverer
	HealthChecker   *health.Checker
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

// preflightResult holds the results of preflight checks, passed from preflight to dry-run.
type preflightResult struct {
	// Discovered workloads (from PVC-based discovery)
	Workloads []discovery.WorkloadInfo
	// Discovered PVCs
	PVCs []discovery.PVCInfo
	// Summary messages for the condition message
	Summary []string
	// Whether any warnings were produced
	Warnings []string
}

// runUpgradePreflight runs read-only preflight checks for an upgrade operation.
// It validates infrastructure prerequisites before any mutations.
func (r *UpgradeRequestReconciler) runUpgradePreflight(ctx context.Context, app *fluxupv1alpha1.ManagedApp, snapshotsEnabled bool) (*preflightResult, error) {
	logger := logging.FromContext(ctx)
	result := &preflightResult{}

	// --- Flux preflight ---

	// Suspend target validation
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

	// Kustomization health (warn only)
	reconciled, err := r.FluxHelper.IsReconciled(ctx, ksRef.Name, ksNS)
	if err != nil {
		logger.Warn("preflight: could not check Kustomization health", "error", err)
	} else if !reconciled {
		msg := fmt.Sprintf("Kustomization %s/%s is not Ready — operation may fail at reconciliation", ksNS, ksRef.Name)
		logger.Warn("preflight: " + msg)
		result.Warnings = append(result.Warnings, msg)
	}

	// Kustomization already suspended (warn only)
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

	// --- Snapshot preflight (upgrade only, when enabled) ---
	if snapshotsEnabled {
		// PVC discovery
		pvcs, err := r.Discoverer.DiscoverPVCs(ctx, app)
		if err != nil {
			return nil, fmt.Errorf("PVC discovery failed: %w", err)
		}
		result.PVCs = pvcs

		if len(pvcs) == 0 {
			logger.Info("preflight: no RWO PVCs discovered — snapshots will be skipped")
		} else {
			pvcNames := make([]string, len(pvcs))
			for i, p := range pvcs {
				pvcNames[i] = p.Name
			}
			logger.Info("preflight: discovered PVCs to snapshot", "count", len(pvcs), "pvcs", pvcNames)
		}

		// Workload discovery
		workloads, err := r.Discoverer.DiscoverWorkloads(ctx, app)
		if err != nil {
			return nil, fmt.Errorf("workload discovery failed: %w", err)
		}
		result.Workloads = workloads

		if len(workloads) == 0 {
			logger.Info("preflight: no workloads discovered mounting PVCs")
		} else {
			wNames := make([]string, len(workloads))
			for i, w := range workloads {
				wNames[i] = w.Kind + "/" + w.Name
			}
			logger.Info("preflight: workloads to scale", "count", len(workloads), "workloads", wNames)
		}

		// VolumeSnapshotClass validation
		if app.Spec.VolumeSnapshots != nil && app.Spec.VolumeSnapshots.VolumeSnapshotClassName != "" {
			var vsc snapshotv1.VolumeSnapshotClass
			if err := r.Get(ctx, types.NamespacedName{Name: app.Spec.VolumeSnapshots.VolumeSnapshotClassName}, &vsc); err != nil {
				return nil, fmt.Errorf("VolumeSnapshotClass %q not found: %w", app.Spec.VolumeSnapshots.VolumeSnapshotClassName, err)
			}
		}

		result.Summary = append(result.Summary, fmt.Sprintf("Kustomization %s/%s %s. %d PVCs to snapshot, %d workloads to scale",
			ksNS, ksRef.Name, ksStatus, len(pvcs), len(workloads)))
	} else {
		result.Summary = append(result.Summary, fmt.Sprintf("Kustomization %s/%s %s. Snapshots disabled",
			ksNS, ksRef.Name, ksStatus))
	}

	return result, nil
}

// runGitDiffPreview reads the current file and computes the version change without committing.
// Returns the current version, target version, and version path for inclusion in status messages.
func (r *UpgradeRequestReconciler) runGitDiffPreview(ctx context.Context, app *fluxupv1alpha1.ManagedApp, targetVersion *fluxupv1alpha1.VersionInfo) (currentVersion, newVersion, versionPath string, err error) {
	logger := logging.FromContext(ctx)

	// Determine version path
	versionPath = yamlpkg.DefaultHelmReleaseVersionPath
	if app.Spec.VersionPolicy != nil && app.Spec.VersionPolicy.VersionPath != "" {
		versionPath = app.Spec.VersionPolicy.VersionPath
	}

	// Determine the new version string
	if targetVersion.Chart != "" {
		newVersion = targetVersion.Chart
	} else if len(targetVersion.Images) > 0 {
		newVersion = targetVersion.Images[0].Tag
		if app.Spec.VersionPolicy == nil || app.Spec.VersionPolicy.VersionPath == "" {
			return "", "", "", fmt.Errorf("VersionPath must be specified for image updates")
		}
	} else {
		return "", "", "", fmt.Errorf("no chart version or images specified")
	}

	// Read current file from Git
	content, err := r.GitManager.ReadFile(ctx, app.Spec.GitPath)
	if err != nil {
		return "", "", "", fmt.Errorf("cannot read Git file %s: %w", app.Spec.GitPath, err)
	}

	// Verify it's a HelmRelease if using the default path
	if versionPath == yamlpkg.DefaultHelmReleaseVersionPath && (app.Spec.VersionPolicy == nil || app.Spec.VersionPolicy.VersionPath == "") {
		if !yamlpkg.IsHelmRelease(content) {
			return "", "", "", fmt.Errorf("VersionPath must be specified for non-HelmRelease resources")
		}
	}

	// Read current version
	currentVersion, err = r.YAMLEditor.GetVersion(content, versionPath)
	if err != nil {
		return "", "", "", fmt.Errorf("cannot read version at path %s: %w", versionPath, err)
	}

	// Verify update produces valid YAML
	if _, err := r.YAMLEditor.UpdateVersion(content, versionPath, newVersion); err != nil {
		return "", "", "", fmt.Errorf("version update would produce invalid YAML: %w", err)
	}

	logger.Info("version change preview",
		"file", app.Spec.GitPath,
		"path", versionPath,
		"current", currentVersion,
		"target", newVersion)

	return currentVersion, newVersion, versionPath, nil
}

// +kubebuilder:rbac:groups=fluxup.dev,resources=upgraderequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fluxup.dev,resources=upgraderequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fluxup.dev,resources=upgraderequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

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
	// Note: We check if the Suspended condition EXISTS (not just if it's True) because
	// the condition transitions from True (suspended) to False (resumed) during the flow.
	// We only want to suspend once at the beginning; once the condition is set, skip this phase.
	if meta.FindStatusCondition(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) == nil {
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

// handleDryRun validates the upgrade and exercises the suspend/scale cycle without committing.
func (r *UpgradeRequestReconciler) handleDryRun(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("processing dry run")

	// --- Initial validation (existing) ---

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
	}

	if upgrade.Spec.TargetVersion == nil && app.Status.AvailableUpdate == nil {
		return r.setFailed(ctx, upgrade, "NoUpdateAvailable", "No update available for this app")
	}

	targetVersion := upgrade.Spec.TargetVersion
	if targetVersion == nil {
		targetVersion = app.Status.AvailableUpdate
	}

	snapshotsEnabled := !upgrade.Spec.SkipSnapshot && app.Spec.VolumeSnapshots != nil && app.Spec.VolumeSnapshots.Enabled

	// --- Git diff preview (NEW — all operations) ---

	var currentVersion, newVersion, versionPath string
	if r.GitManager != nil {
		currentVersion, newVersion, versionPath, err = r.runGitDiffPreview(ctx, app, targetVersion)
		if err != nil {
			return r.setFailed(ctx, upgrade, "PreflightFailed", err.Error())
		}
	}

	// --- Preflight checks (NEW — all operations) ---

	preflight, err := r.runUpgradePreflight(ctx, app, snapshotsEnabled)
	if err != nil {
		return r.setFailed(ctx, upgrade, "PreflightFailed", err.Error())
	}

	// --- Dry-run quiescence cycle (NEW — dry-run only) ---
	// Exercise the full suspend → scale down → scale up → resume cycle.

	return r.runDryRunQuiescence(ctx, upgrade, app, preflight, currentVersion, newVersion, versionPath)
}

// runDryRunQuiescence exercises the suspend/scale cycle for dry-run validation.
// It uses conditions to track progress through the cycle, allowing the reconcile
// loop to retry on transient failures.
func (r *UpgradeRequestReconciler) runDryRunQuiescence(
	ctx context.Context,
	upgrade *fluxupv1alpha1.UpgradeRequest,
	app *fluxupv1alpha1.ManagedApp,
	preflight *preflightResult,
	currentVersion, newVersion, versionPath string,
) (ctrl.Result, error) {
	suspendName, suspendNS := r.getSuspendTarget(app)

	// Phase 1: Suspend Kustomization
	if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
		return r.dryRunSuspend(ctx, upgrade, app, suspendName, suspendNS)
	}

	// Phase 2: Scale down workloads (if any discovered)
	if len(preflight.Workloads) > 0 && !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeWorkloadStopped) {
		return r.dryRunScaleDown(ctx, upgrade, app, preflight.Workloads)
	}

	// Phase 3: Verify still suspended, then scale up
	if len(preflight.Workloads) > 0 && !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSnapshotReady) {
		return r.dryRunScaleUp(ctx, upgrade, app, preflight.Workloads, suspendName, suspendNS)
	}

	// Phase 4: Resume Kustomization and complete
	return r.dryRunResumeAndComplete(ctx, upgrade, app, preflight, suspendName, suspendNS, currentVersion, newVersion, versionPath)
}

// dryRunSuspend handles Phase 1 of the dry-run quiescence cycle: suspending the Kustomization.
func (r *UpgradeRequestReconciler) dryRunSuspend(
	ctx context.Context,
	upgrade *fluxupv1alpha1.UpgradeRequest,
	app *fluxupv1alpha1.ManagedApp,
	suspendName, suspendNS string,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	r.setPhaseStartIfNeeded(upgrade)

	if err := r.FluxHelper.SuspendKustomization(ctx, suspendName, suspendNS); err != nil {
		if r.isPhaseTimedOut(upgrade, TimeoutSuspend) {
			return r.setDryRunFailed(ctx, upgrade, app, fmt.Sprintf("Suspend timed out: %v", err))
		}
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	suspended, err := r.FluxHelper.IsSuspended(ctx, suspendName, suspendNS)
	if err != nil || !suspended {
		if r.isPhaseTimedOut(upgrade, TimeoutSuspend) {
			return r.setDryRunFailed(ctx, upgrade, app, "Suspend verification timed out")
		}
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	r.resetPhaseStart(upgrade)
	logger.Info("dry run: Kustomization suspended")

	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSuspended,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunSuspended",
		Message:            fmt.Sprintf("Dry run: suspended %s/%s", suspendNS, suspendName),
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// dryRunScaleDown handles Phase 2: scaling down workloads with RWO PVCs.
func (r *UpgradeRequestReconciler) dryRunScaleDown(
	ctx context.Context,
	upgrade *fluxupv1alpha1.UpgradeRequest,
	app *fluxupv1alpha1.ManagedApp,
	workloads []discovery.WorkloadInfo,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	r.setPhaseStartIfNeeded(upgrade)

	for _, w := range workloads {
		ns := workloadNS(w, app.Namespace)
		if _, err := r.WorkloadScaler.ScaleDown(ctx, w.Kind, w.Name, ns); err != nil {
			return r.setDryRunFailed(ctx, upgrade, app, fmt.Sprintf("Scale down failed for %s/%s: %v", w.Kind, w.Name, err))
		}
	}

	allDown, err := r.allWorkloadsScaledDown(ctx, workloads, app.Namespace)
	if err != nil {
		return r.setDryRunFailed(ctx, upgrade, app, fmt.Sprintf("Scale down check failed: %v", err))
	}
	if !allDown {
		if r.isPhaseTimedOut(upgrade, TimeoutScaleDown) {
			return r.setDryRunFailed(ctx, upgrade, app, "Scale down timed out")
		}
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	r.resetPhaseStart(upgrade)
	logger.Info("dry run: workloads scaled down", "count", len(workloads))

	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunScaledDown",
		Message:            fmt.Sprintf("Dry run: scaled down %d workloads", len(workloads)),
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// dryRunScaleUp handles Phase 3: verifying suspension, scaling up, and re-verifying.
func (r *UpgradeRequestReconciler) dryRunScaleUp(
	ctx context.Context,
	upgrade *fluxupv1alpha1.UpgradeRequest,
	app *fluxupv1alpha1.ManagedApp,
	workloads []discovery.WorkloadInfo,
	suspendName, suspendNS string,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	if err := r.FluxHelper.VerifyStillSuspended(ctx, suspendName, suspendNS); err != nil {
		return r.setDryRunFailed(ctx, upgrade, app, fmt.Sprintf("External interference detected before scale-up: %v", err))
	}

	r.setPhaseStartIfNeeded(upgrade)

	for _, w := range workloads {
		ns := workloadNS(w, app.Namespace)
		if err := r.WorkloadScaler.ScaleUp(ctx, w.Kind, w.Name, ns, 1); err != nil {
			return r.setDryRunFailed(ctx, upgrade, app, fmt.Sprintf("Scale up failed for %s/%s: %v", w.Kind, w.Name, err))
		}
	}

	allDown, err := r.allWorkloadsScaledDown(ctx, workloads, app.Namespace)
	if err != nil {
		return r.setDryRunFailed(ctx, upgrade, app, fmt.Sprintf("Scale up check failed: %v", err))
	}
	if allDown {
		// Not all scaled up yet
		if r.isPhaseTimedOut(upgrade, TimeoutScaleUp) {
			return r.setDryRunFailed(ctx, upgrade, app, "Scale up timed out")
		}
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	r.resetPhaseStart(upgrade)
	logger.Info("dry run: workloads scaled back up", "count", len(workloads))

	if err := r.FluxHelper.VerifyStillSuspended(ctx, suspendName, suspendNS); err != nil {
		return r.setDryRunFailed(ctx, upgrade, app, fmt.Sprintf("External interference detected after scale-up: %v", err))
	}

	// Reuse SnapshotReady to signal scale-up complete in dry-run
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSnapshotReady,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunScaledUp",
		Message:            fmt.Sprintf("Dry run: scaled up %d workloads", len(workloads)),
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// dryRunResumeAndComplete handles Phase 4: resuming the Kustomization and building the summary.
func (r *UpgradeRequestReconciler) dryRunResumeAndComplete(
	ctx context.Context,
	upgrade *fluxupv1alpha1.UpgradeRequest,
	app *fluxupv1alpha1.ManagedApp,
	preflight *preflightResult,
	suspendName, suspendNS string,
	currentVersion, newVersion, versionPath string,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	r.setPhaseStartIfNeeded(upgrade)

	if err := r.FluxHelper.ResumeKustomization(ctx, suspendName, suspendNS); err != nil {
		if r.isPhaseTimedOut(upgrade, TimeoutResume) {
			return r.setDryRunFailed(ctx, upgrade, app, fmt.Sprintf("Resume timed out: %v", err))
		}
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	suspended, err := r.FluxHelper.IsSuspended(ctx, suspendName, suspendNS)
	if err != nil || suspended {
		if r.isPhaseTimedOut(upgrade, TimeoutResume) {
			return r.setDryRunFailed(ctx, upgrade, app, "Resume verification timed out")
		}
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	r.resetPhaseStart(upgrade)
	logger.Info("dry run: Kustomization resumed")

	message := buildDryRunSummary("upgrade", preflight, currentVersion, newVersion, versionPath, app.Spec.GitPath)

	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeComplete,
		Status:             metav1.ConditionTrue,
		Reason:             "DryRunSucceeded",
		Message:            message,
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	// Remove finalizer
	if controllerutil.ContainsFinalizer(upgrade, OperationFinalizer) {
		controllerutil.RemoveFinalizer(upgrade, OperationFinalizer)
		if err := r.Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// allWorkloadsScaledDown checks if all workloads in the list are scaled to zero.
func (r *UpgradeRequestReconciler) allWorkloadsScaledDown(ctx context.Context, workloads []discovery.WorkloadInfo, defaultNS string) (bool, error) {
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

// workloadNS returns the workload's namespace, falling back to defaultNS if empty.
func workloadNS(w discovery.WorkloadInfo, defaultNS string) string {
	if w.Namespace != "" {
		return w.Namespace
	}
	return defaultNS
}

// buildDryRunSummary creates the condition message for a successful dry-run.
func buildDryRunSummary(operation string, preflight *preflightResult, currentVersion, newVersion, versionPath, gitPath string) string {
	var msgParts []string
	if versionPath != "" {
		msgParts = append(msgParts, fmt.Sprintf("Would %s from %s to %s (%s in %s)", operation, currentVersion, newVersion, versionPath, gitPath))
	}
	if len(preflight.Summary) > 0 {
		msgParts = append(msgParts, "Preflight: "+strings.Join(preflight.Summary, "; "))
	}

	var verified []string
	verified = append(verified, "suspend/resume: verified")
	if len(preflight.Workloads) > 0 {
		wNames := make([]string, len(preflight.Workloads))
		for i, w := range preflight.Workloads {
			wNames[i] = w.Kind + "/" + w.Name
		}
		verified = append(verified, fmt.Sprintf("scale down/up: verified (%s)", strings.Join(wNames, ", ")))
	}
	msgParts = append(msgParts, strings.Join(verified, ". ")+".")

	return "Dry run passed. " + strings.Join(msgParts, " ")
}

// setDryRunFailed marks the dry-run as failed. Before failing, it attempts to
// resume the Kustomization if it was suspended during the dry-run.
func (r *UpgradeRequestReconciler) setDryRunFailed(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest, app *fluxupv1alpha1.ManagedApp, message string) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Attempt to resume Kustomization if we suspended it
	if meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
		suspendName, suspendNS := r.getSuspendTarget(app)
		if resumeErr := r.FluxHelper.ResumeKustomization(ctx, suspendName, suspendNS); resumeErr != nil {
			logger.Error("dry run: failed to resume Kustomization during cleanup", "error", resumeErr)
			message = fmt.Sprintf("%s (WARNING: Kustomization may still be suspended — manual resume required)", message)
		} else {
			logger.Info("dry run: resumed Kustomization during cleanup")
		}
	}

	return r.setFailed(ctx, upgrade, "DryRunFailed", message)
}

// handleSuspend validates the request, runs preflight checks, and suspends Flux
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

	// --- Git diff preview (runs for all operations) ---
	if r.GitManager != nil {
		if _, _, _, err := r.runGitDiffPreview(ctx, app, targetVersion); err != nil {
			return r.setFailed(ctx, upgrade, "PreflightFailed", err.Error())
		}
	}

	// --- Preflight checks (runs for all operations) ---
	snapshotsEnabled := !upgrade.Spec.SkipSnapshot && app.Spec.VolumeSnapshots != nil && app.Spec.VolumeSnapshots.Enabled
	if _, err := r.runUpgradePreflight(ctx, app, snapshotsEnabled); err != nil {
		return r.setFailed(ctx, upgrade, "PreflightFailed", err.Error())
	}

	logger.Info("starting upgrade",
		"app", app.Name,
		"currentVersion", app.Status.CurrentVersion,
		"targetVersion", targetVersion)

	// Determine which Kustomization to suspend (suspendRef or kustomizationRef)
	suspendName, suspendNS := r.getSuspendTarget(app)

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
func (r *UpgradeRequestReconciler) handleScaleDown(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error, bool) {
	logger := logging.FromContext(ctx)

	// Set phase start time for timeout tracking
	r.setPhaseStartIfNeeded(upgrade)

	app, err := r.getManagedApp(ctx, upgrade)
	if err != nil {
		result, err := r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
		return result, err, false
	}

	// Skip scaling if snapshots are disabled (no need to scale down for consistency)
	if upgrade.Spec.SkipSnapshot || app.Spec.VolumeSnapshots == nil || !app.Spec.VolumeSnapshots.Enabled {
		logger.Debug("snapshots disabled, skipping workload scale-down")
		meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
			Status:             metav1.ConditionTrue,
			Reason:             "SnapshotsDisabled",
			Message:            "Workload scale-down skipped (snapshots disabled)",
			ObservedGeneration: upgrade.Generation,
		})
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err, false
		}
		return ctrl.Result{Requeue: true}, nil, true
	}

	// Auto-discover workloads from the ManagedApp
	workloads, err := r.Discoverer.DiscoverWorkloads(ctx, app)
	if err != nil {
		result, err := r.setFailed(ctx, upgrade, "WorkloadDiscoveryFailed", err.Error())
		return result, err, false
	}

	if len(workloads) == 0 {
		logger.Debug("no workloads discovered, skipping scale-down")
		meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
			Status:             metav1.ConditionTrue,
			Reason:             "NoWorkloadsFound",
			Message:            "No workloads discovered that mount RWO PVCs",
			ObservedGeneration: upgrade.Generation,
		})
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err, false
		}
		return ctrl.Result{Requeue: true}, nil, true
	}

	logger.Info("discovered workloads for scale-down", "count", len(workloads))

	// Scale down each workload
	scaledWorkloads := make([]fluxupv1alpha1.WorkloadScalingInfo, 0, len(workloads))
	for _, w := range workloads {
		ns := w.Namespace
		if ns == "" {
			ns = app.Namespace
		}

		// Scale to 0 (we ignore the returned ScaleInfo since Flux handles replica restoration)
		if _, err := r.WorkloadScaler.ScaleDown(ctx, w.Kind, w.Name, ns); err != nil {
			result, err := r.setFailed(ctx, upgrade, "ScaleDownFailed",
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
	if upgrade.Status.Scaling == nil {
		upgrade.Status.Scaling = &fluxupv1alpha1.ScalingStatus{}
	}
	upgrade.Status.Scaling.Workloads = scaledWorkloads

	// Check if all workloads are scaled down (non-blocking)
	allScaledDown := true
	for _, w := range scaledWorkloads {
		scaledDown, err := r.WorkloadScaler.IsScaledDown(ctx, w.Kind, w.Name, w.Namespace)
		if err != nil {
			result, err := r.setFailed(ctx, upgrade, "ScaleDownCheckFailed", err.Error())
			return result, err, false
		}
		if !scaledDown {
			allScaledDown = false
			break
		}
	}

	if !allScaledDown {
		// Check per-phase timeout
		if r.isPhaseTimedOut(upgrade, TimeoutScaleDown) {
			result, err := r.setFailed(ctx, upgrade, "ScaleDownTimeout", "Workloads did not scale down within timeout")
			return result, err, false
		}
		logger.Debug("waiting for workloads to scale down")
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err, false
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, false
	}

	// All workloads scaled down - reset phase timer for next phase
	r.resetPhaseStart(upgrade)

	now := metav1.Now()
	upgrade.Status.Scaling.ScaledDownAt = &now
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeWorkloadStopped,
		Status:             metav1.ConditionTrue,
		Reason:             "WorkloadsStopped",
		Message:            fmt.Sprintf("Scaled down %d workloads", len(scaledWorkloads)),
		ObservedGeneration: upgrade.Generation,
	})

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err, false
	}

	logger.Info("all workloads scaled down", "count", len(scaledWorkloads))
	return ctrl.Result{Requeue: true}, nil, false
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
		// Discover PVCs to snapshot (uses explicit list or auto-discovers)
		discoveredPVCs, err := r.Discoverer.DiscoverPVCs(ctx, app)
		if err != nil {
			return r.setFailed(ctx, upgrade, "PVCDiscoveryFailed", err.Error())
		}

		if len(discoveredPVCs) == 0 {
			logger.Info("no RWO PVCs discovered, skipping snapshots")
			meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
				Type:               fluxupv1alpha1.ConditionTypeSnapshotReady,
				Status:             metav1.ConditionTrue,
				Reason:             "NoPVCsFound",
				Message:            "No RWO PVCs discovered to snapshot",
				ObservedGeneration: upgrade.Generation,
			})
			if err := r.Status().Update(ctx, upgrade); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		// Convert discovered PVCs to PVCRef for snapshot manager
		pvcRefs := make([]fluxupv1alpha1.PVCRef, len(discoveredPVCs))
		for i, pvc := range discoveredPVCs {
			pvcRefs[i] = fluxupv1alpha1.PVCRef{
				Name:      pvc.Name,
				Namespace: pvc.Namespace,
			}
		}

		snapshots, err := r.SnapshotManager.CreateSnapshotsForUpgrade(
			ctx,
			app.Name,
			app.Namespace,
			pvcRefs,
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

	// If using the default HelmRelease version path, verify the file is actually a HelmRelease.
	// The default path (spec.chart.spec.version) only makes sense for HelmRelease manifests.
	// For other resource types (Deployment, StatefulSet, etc.), versionPath must be explicit.
	if versionPath == yamlpkg.DefaultHelmReleaseVersionPath && (app.Spec.VersionPolicy == nil || app.Spec.VersionPolicy.VersionPath == "") {
		if !yamlpkg.IsHelmRelease(content) {
			return r.setFailed(ctx, upgrade, "MissingVersionPath",
				"VersionPath must be specified in ManagedApp.spec.versionPolicy for non-HelmRelease resources")
		}
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
		// Update status before requeue to persist Suspended=False
		if err := r.Status().Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
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

	// Perform direct health check (real-time, authoritative)
	// This checks both Flux resource AND workload readiness
	healthResult, err := r.HealthChecker.CheckHealth(ctx, app)
	if err != nil {
		return r.setFailed(ctx, upgrade, "HealthCheckError", err.Error())
	}

	if !healthResult.Healthy {
		// Check per-phase timeout (use configured timeout if available)
		timeout := TimeoutHealthCheck
		if app.Spec.HealthCheck != nil && app.Spec.HealthCheck.Timeout != "" {
			if parsed, parseErr := time.ParseDuration(app.Spec.HealthCheck.Timeout); parseErr == nil {
				timeout = parsed
			}
		}

		if r.isPhaseTimedOut(upgrade, timeout) {
			return r.setFailed(ctx, upgrade, "HealthCheckTimeout",
				fmt.Sprintf("App did not become healthy within timeout: %s", healthResult.Message))
		}

		logger.Debug("app not yet healthy, waiting", "message", healthResult.Message)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Health check passed
	logger.Info("health check passed, upgrade complete")

	now := metav1.Now()
	upgrade.Status.HealthCheck = &fluxupv1alpha1.HealthCheckStatus{
		Status:    "Passed",
		CheckedAt: &now,
		Message:   healthResult.Message,
	}
	if upgrade.Status.Upgrade != nil {
		upgrade.Status.Upgrade.CompletedAt = &now
	}

	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeHealthy,
		Status:             metav1.ConditionTrue,
		Reason:             "HealthCheckPassed",
		Message:            healthResult.Message,
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

	// Ensure Kustomization suspension is marked as resolved
	meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeSuspended,
		Status:             metav1.ConditionFalse,
		Reason:             "KustomizationResumed",
		Message:            "Kustomization resumed after upgrade completion",
		ObservedGeneration: upgrade.Generation,
	})

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
