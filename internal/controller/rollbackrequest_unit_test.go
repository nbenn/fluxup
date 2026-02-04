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
	"slices"
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/discovery"
	"github.com/nbenn/fluxup/internal/flux"
	"github.com/nbenn/fluxup/internal/git"
	"github.com/nbenn/fluxup/internal/health"
	"github.com/nbenn/fluxup/internal/snapshot"
	yamlpkg "github.com/nbenn/fluxup/internal/yaml"
)

func setupRollbackTestReconciler(_ *testing.T, objects ...client.Object) (*RollbackRequestReconciler, *git.MockManager) {
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = kustomizev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(
			&fluxupv1alpha1.RollbackRequest{},
			&fluxupv1alpha1.UpgradeRequest{},
			&fluxupv1alpha1.ManagedApp{},
			&kustomizev1.Kustomization{},
		).
		Build()

	mockGit := git.NewMockManager()
	mockGit.SetFile("flux/apps/test-app/helmrelease.yaml", []byte(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: test-app
spec:
  chart:
    spec:
      chart: test-chart
      version: "2.0.0"
`))

	reconciler := &RollbackRequestReconciler{
		Client:          fakeClient,
		Scheme:          scheme,
		GitManager:      mockGit,
		SnapshotManager: snapshot.NewManager(fakeClient),
		FluxHelper:      flux.NewHelper(fakeClient),
		YAMLEditor:      yamlpkg.NewEditor(),
		Discoverer:      discovery.New(fakeClient),
		HealthChecker:   health.NewChecker(fakeClient),
	}

	return reconciler, mockGit
}

// reconcileRollbackUntilCondition calls Reconcile repeatedly until the specified condition
// is set or until max iterations (5) is reached.
func reconcileRollbackUntilCondition(
	ctx context.Context,
	r *RollbackRequestReconciler,
	req reconcile.Request,
	conditionType string,
) error {
	for range 5 {
		_, err := r.Reconcile(ctx, req)
		if err != nil {
			return err
		}

		var rollback fluxupv1alpha1.RollbackRequest
		if err := r.Get(ctx, req.NamespacedName, &rollback); err != nil {
			return err
		}

		if meta.FindStatusCondition(rollback.Status.Conditions, conditionType) != nil {
			return nil
		}
	}
	return nil
}

func TestRollbackRequest_UpgradeRequestNotFound(t *testing.T) {
	rollback := &fluxupv1alpha1.RollbackRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rollback",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.RollbackRequestSpec{
			UpgradeRequestRef: fluxupv1alpha1.ObjectReference{
				Name: "nonexistent-upgrade",
			},
		},
	}

	r, _ := setupRollbackTestReconciler(t, rollback)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      rollback.Name,
			Namespace: rollback.Namespace,
		},
	}

	// Reconcile until Complete condition is set
	if err := reconcileRollbackUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.RollbackRequest
	if err := r.Get(ctx, types.NamespacedName{Name: rollback.Name, Namespace: rollback.Namespace}, &result); err != nil {
		t.Fatalf("failed to get rollback request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	if completeCond.Reason != "UpgradeRequestNotFound" {
		t.Errorf("expected reason UpgradeRequestNotFound, got %s", completeCond.Reason)
	}
}

func TestRollbackRequest_UpgradeInProgress(t *testing.T) {
	// UpgradeRequest without Complete condition (still in progress)
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-upgrade",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeSuspended,
					Status: metav1.ConditionTrue,
					Reason: "KustomizationSuspended",
				},
			},
		},
	}

	rollback := &fluxupv1alpha1.RollbackRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rollback",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.RollbackRequestSpec{
			UpgradeRequestRef: fluxupv1alpha1.ObjectReference{
				Name: "test-upgrade",
			},
		},
	}

	r, _ := setupRollbackTestReconciler(t, upgrade, rollback)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      rollback.Name,
			Namespace: rollback.Namespace,
		},
	}

	// Reconcile until Complete condition is set
	if err := reconcileRollbackUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.RollbackRequest
	if err := r.Get(ctx, types.NamespacedName{Name: rollback.Name, Namespace: rollback.Namespace}, &result); err != nil {
		t.Fatalf("failed to get rollback request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	if completeCond.Reason != "UpgradeInProgress" {
		t.Errorf("expected reason UpgradeInProgress, got %s", completeCond.Reason)
	}
}

func TestRollbackRequest_NoSnapshotsAvailable(t *testing.T) {
	managedApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/test-app/helmrelease.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	// UpgradeRequest that completed but had no snapshots
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-upgrade",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: false,
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeComplete,
					Status: metav1.ConditionTrue,
					Reason: "UpgradeSucceeded",
				},
			},
			// No Snapshot field - no snapshots
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			},
		},
	}

	rollback := &fluxupv1alpha1.RollbackRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rollback",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.RollbackRequestSpec{
			UpgradeRequestRef: fluxupv1alpha1.ObjectReference{
				Name: "test-upgrade",
			},
		},
	}

	r, _ := setupRollbackTestReconciler(t, managedApp, upgrade, rollback)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      rollback.Name,
			Namespace: rollback.Namespace,
		},
	}

	// Reconcile until Complete condition is set
	if err := reconcileRollbackUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.RollbackRequest
	if err := r.Get(ctx, types.NamespacedName{Name: rollback.Name, Namespace: rollback.Namespace}, &result); err != nil {
		t.Fatalf("failed to get rollback request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	if completeCond.Reason != "NoSnapshotsAvailable" {
		t.Errorf("expected reason NoSnapshotsAvailable, got %s", completeCond.Reason)
	}
}

func TestRollbackRequest_DryRun(t *testing.T) {
	managedApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/test-app/helmrelease.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	now := metav1.Now()
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-upgrade",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeComplete,
					Status: metav1.ConditionTrue,
					Reason: "UpgradeSucceeded",
				},
			},
			Snapshot: &fluxupv1alpha1.SnapshotStatus{
				CreatedAt: &now,
				PVCSnapshots: []fluxupv1alpha1.PVCSnapshotInfo{
					{PVCName: "data-0", SnapshotName: "snap-1"},
				},
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			},
		},
	}

	rollback := &fluxupv1alpha1.RollbackRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rollback",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.RollbackRequestSpec{
			UpgradeRequestRef: fluxupv1alpha1.ObjectReference{
				Name: "test-upgrade",
			},
			DryRun: true,
		},
	}

	r, mockGit := setupRollbackTestReconciler(t, managedApp, upgrade, rollback)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      rollback.Name,
			Namespace: rollback.Namespace,
		},
	}

	// Reconcile until Complete condition is set
	if err := reconcileRollbackUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.RollbackRequest
	if err := r.Get(ctx, types.NamespacedName{Name: rollback.Name, Namespace: rollback.Namespace}, &result); err != nil {
		t.Fatalf("failed to get rollback request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Complete=True for dry run, got %s", completeCond.Status)
	}
	if completeCond.Reason != "DryRunSucceeded" {
		t.Errorf("expected reason DryRunSucceeded, got %s", completeCond.Reason)
	}

	// Git should NOT have been committed for dry run
	if len(mockGit.CommitFileCalls) != 0 {
		t.Errorf("expected 0 commit calls for dry run, got %d", len(mockGit.CommitFileCalls))
	}
}

func TestRollbackRequest_SuspendKustomization(t *testing.T) {
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false,
		},
	}

	managedApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/test-app/helmrelease.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	now := metav1.Now()
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-upgrade",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true, // Skip snapshot for simpler test
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeComplete,
					Status: metav1.ConditionFalse, // Failed upgrade
					Reason: "HealthCheckTimeout",
				},
			},
			Snapshot: &fluxupv1alpha1.SnapshotStatus{
				CreatedAt: &now,
				PVCSnapshots: []fluxupv1alpha1.PVCSnapshotInfo{
					{PVCName: "data-0", SnapshotName: "snap-1"},
				},
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			},
		},
	}

	rollback := &fluxupv1alpha1.RollbackRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rollback",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.RollbackRequestSpec{
			UpgradeRequestRef: fluxupv1alpha1.ObjectReference{
				Name: "test-upgrade",
			},
		},
	}

	r, _ := setupRollbackTestReconciler(t, kustomization, managedApp, upgrade, rollback)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      rollback.Name,
			Namespace: rollback.Namespace,
		},
	}

	// Reconcile until Suspended condition is set
	if err := reconcileRollbackUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeSuspended); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check Kustomization is suspended
	var ks kustomizev1.Kustomization
	if err := r.Get(ctx, types.NamespacedName{Name: "apps", Namespace: "flux-system"}, &ks); err != nil {
		t.Fatalf("failed to get kustomization: %v", err)
	}
	if !ks.Spec.Suspend {
		t.Error("expected Kustomization to be suspended")
	}

	// Check rollback status
	var result fluxupv1alpha1.RollbackRequest
	if err := r.Get(ctx, types.NamespacedName{Name: rollback.Name, Namespace: rollback.Namespace}, &result); err != nil {
		t.Fatalf("failed to get rollback request: %v", err)
	}

	suspendedCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended)
	if suspendedCond == nil {
		t.Fatal("expected Suspended condition to be set")
	}
	if suspendedCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Suspended=True, got %s", suspendedCond.Status)
	}

	// Check RestoredFrom is populated
	if result.Status.RestoredFrom == nil {
		t.Fatal("expected RestoredFrom to be set")
	}
	if result.Status.RestoredFrom.UpgradeRequestName != "test-upgrade" {
		t.Errorf("expected UpgradeRequestName=test-upgrade, got %s", result.Status.RestoredFrom.UpgradeRequestName)
	}
}

func TestRollbackRequest_GitRevert(t *testing.T) {
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: true, // Already suspended
		},
	}

	managedApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/test-app/helmrelease.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	now := metav1.Now()
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-upgrade",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true,
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeComplete,
					Status: metav1.ConditionFalse,
					Reason: "HealthCheckTimeout",
				},
			},
			Snapshot: &fluxupv1alpha1.SnapshotStatus{
				CreatedAt:    &now,
				PVCSnapshots: []fluxupv1alpha1.PVCSnapshotInfo{}, // Empty - skipSnapshot
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			},
		},
	}

	// Rollback already at VolumesRestored stage (with finalizer)
	rollback := &fluxupv1alpha1.RollbackRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-rollback",
			Namespace:  "default",
			Finalizers: []string{OperationFinalizer},
		},
		Spec: fluxupv1alpha1.RollbackRequestSpec{
			UpgradeRequestRef: fluxupv1alpha1.ObjectReference{
				Name: "test-upgrade",
			},
		},
		Status: fluxupv1alpha1.RollbackRequestStatus{
			StartedAt: &now,
			RestoredFrom: &fluxupv1alpha1.RestoredFromStatus{
				UpgradeRequestName: "test-upgrade",
				TargetVersion:      &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				RolledBackVersion:  &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			},
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeSuspended,
					Status: metav1.ConditionTrue,
					Reason: "KustomizationSuspended",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeWorkloadStopped,
					Status: metav1.ConditionTrue,
					Reason: "Skipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeVolumesRestored,
					Status: metav1.ConditionTrue,
					Reason: "VolumesSkipped",
				},
			},
		},
	}

	r, mockGit := setupRollbackTestReconciler(t, kustomization, managedApp, upgrade, rollback)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      rollback.Name,
			Namespace: rollback.Namespace,
		},
	}

	// Reconcile until GitReverted condition is set
	if err := reconcileRollbackUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeGitReverted); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check Git was committed
	if len(mockGit.CommitFileCalls) != 1 {
		t.Fatalf("expected 1 commit call, got %d", len(mockGit.CommitFileCalls))
	}

	// Verify reverted version in committed content
	content := string(mockGit.CommitFileCalls[0].Content)
	if !containsStr(content, "version: \"1.0.0\"") {
		t.Errorf("expected version 1.0.0 in commit (reverted), got: %s", content)
	}

	// Check status
	var result fluxupv1alpha1.RollbackRequest
	if err := r.Get(ctx, types.NamespacedName{Name: rollback.Name, Namespace: rollback.Namespace}, &result); err != nil {
		t.Fatalf("failed to get rollback request: %v", err)
	}

	gitCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeGitReverted)
	if gitCond == nil {
		t.Fatal("expected GitReverted condition to be set")
	}
	if gitCond.Status != metav1.ConditionTrue {
		t.Errorf("expected GitReverted=True, got %s", gitCond.Status)
	}

	// Check GitRevert status
	if result.Status.GitRevert == nil {
		t.Fatal("expected GitRevert status to be set")
	}
	if result.Status.GitRevert.CommitSHA == "" {
		t.Error("expected CommitSHA to be set")
	}
}

// containsStr checks if s contains substr
func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestRollbackRequest_FinalizerAddedOnStart(t *testing.T) {
	managedApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/test-app/helmrelease.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	now := metav1.Now()
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-upgrade",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeComplete,
					Status: metav1.ConditionTrue,
					Reason: "UpgradeSucceeded",
				},
			},
			Snapshot: &fluxupv1alpha1.SnapshotStatus{
				CreatedAt: &now,
				PVCSnapshots: []fluxupv1alpha1.PVCSnapshotInfo{
					{PVCName: "data-0", SnapshotName: "snap-1"},
				},
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			},
		},
	}

	// Rollback without finalizer
	rollback := &fluxupv1alpha1.RollbackRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rollback",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.RollbackRequestSpec{
			UpgradeRequestRef: fluxupv1alpha1.ObjectReference{
				Name: "test-upgrade",
			},
		},
	}

	r, _ := setupRollbackTestReconciler(t, managedApp, upgrade, rollback)
	ctx := context.Background()

	// First reconcile should add finalizer
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      rollback.Name,
			Namespace: rollback.Namespace,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check finalizer was added
	var result fluxupv1alpha1.RollbackRequest
	if err := r.Get(ctx, types.NamespacedName{Name: rollback.Name, Namespace: rollback.Namespace}, &result); err != nil {
		t.Fatalf("failed to get rollback request: %v", err)
	}

	if !slices.Contains(result.Finalizers, OperationFinalizer) {
		t.Errorf("expected finalizer %s to be added, got %v", OperationFinalizer, result.Finalizers)
	}
}
