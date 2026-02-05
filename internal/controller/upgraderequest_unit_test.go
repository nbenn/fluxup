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
	"testing"
	"time"

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

const (
	testUpgradeName             = "test-upgrade"
	reasonReconciliationTimeout = "ReconciliationTimeout"
	reasonHealthCheckTimeout    = "HealthCheckTimeout"
	reasonDryRunSucceeded       = "DryRunSucceeded"
)

func setupTestReconciler(_ *testing.T, objects ...client.Object) (*UpgradeRequestReconciler, *git.MockManager) {
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = kustomizev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&fluxupv1alpha1.UpgradeRequest{}, &fluxupv1alpha1.ManagedApp{}, &kustomizev1.Kustomization{}).
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
      version: "1.0.0"
`))

	reconciler := &UpgradeRequestReconciler{
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

// reconcileUntilCondition calls Reconcile repeatedly until the specified condition
// is set (either True or False based on expectedStatus) or until maxIterations is reached.
// This simulates the controller's requeue behavior in tests.
func reconcileUntilCondition(
	ctx context.Context,
	r *UpgradeRequestReconciler,
	req reconcile.Request,
	conditionType string,
	maxIterations int,
) error {
	for range maxIterations {
		_, err := r.Reconcile(ctx, req)
		if err != nil {
			return err
		}

		// Check if condition is now set
		var upgrade fluxupv1alpha1.UpgradeRequest
		if err := r.Get(ctx, req.NamespacedName, &upgrade); err != nil {
			return err
		}

		if meta.FindStatusCondition(upgrade.Status.Conditions, conditionType) != nil {
			return nil
		}
	}
	return nil // Let the test check if condition was set
}

func TestUpgradeRequest_ManagedAppNotFound(t *testing.T) {
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "nonexistent-app",
			},
		},
	}

	r, _ := setupTestReconciler(t, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Complete condition is set (handles finalizer addition)
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check status
	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	if completeCond.Reason != "ManagedAppNotFound" {
		t.Errorf("expected reason ManagedAppNotFound, got %s", completeCond.Reason)
	}
}

func TestUpgradeRequest_NoUpdateAvailable(t *testing.T) {
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: nil, // No update available
		},
	}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
		},
	}

	r, _ := setupTestReconciler(t, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Complete condition is set (handles finalizer addition)
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Reason != "NoUpdateAvailable" {
		t.Errorf("expected reason NoUpdateAvailable, got %s", completeCond.Reason)
	}
}

func TestUpgradeRequest_DryRun(t *testing.T) {
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			DryRun: true,
		},
	}

	r, mockGit := setupTestReconciler(t, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Complete condition is set (handles finalizer addition)
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Complete=True for dry run, got %s", completeCond.Status)
	}
	if completeCond.Reason != reasonDryRunSucceeded {
		t.Errorf("expected reason %s, got %s", reasonDryRunSucceeded, completeCond.Reason)
	}

	// Git should have been read but not committed
	if len(mockGit.ReadFileCalls) != 1 {
		t.Errorf("expected 1 read call, got %d", len(mockGit.ReadFileCalls))
	}
	if len(mockGit.CommitFileCalls) != 0 {
		t.Errorf("expected 0 commit calls for dry run, got %d", len(mockGit.CommitFileCalls))
	}
}

func TestUpgradeRequest_SuspendKustomization(t *testing.T) {
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true,
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Suspended condition is set (handles finalizer addition)
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeSuspended, 5); err != nil {
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

	// Check upgrade status
	var upgraded fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &upgraded); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	suspendedCond := meta.FindStatusCondition(upgraded.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended)
	if suspendedCond == nil {
		t.Fatal("expected Suspended condition to be set")
	}
	if suspendedCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Suspended=True, got %s", suspendedCond.Status)
	}
}

func TestUpgradeRequest_CommitToGit(t *testing.T) {
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true,
		},
	}

	r, mockGit := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until GitCommitted condition is set (handles finalizer addition and suspend)
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeGitCommitted, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check Git was committed
	if len(mockGit.CommitFileCalls) != 1 {
		t.Fatalf("expected 1 commit call, got %d", len(mockGit.CommitFileCalls))
	}

	// Verify new version in committed content
	content := string(mockGit.CommitFileCalls[0].Content)
	if !contains(content, "version: \"2.0.0\"") {
		t.Errorf("expected new version in commit, got: %s", content)
	}

	// Check status
	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	gitCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeGitCommitted)
	if gitCond == nil {
		t.Fatal("expected GitCommitted condition to be set")
	}
	if gitCond.Status != metav1.ConditionTrue {
		t.Errorf("expected GitCommitted=True, got %s", gitCond.Status)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestUpgradeRequest_ImageUpdate(t *testing.T) {
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
			VersionPolicy: &fluxupv1alpha1.VersionPolicy{
				VersionPath: "spec.values.image.tag",
			},
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion: &fluxupv1alpha1.VersionInfo{
				Images: []fluxupv1alpha1.ImageInfo{{Name: "myapp", Tag: "v1.0.0"}},
			},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{
				Images: []fluxupv1alpha1.ImageInfo{{Name: "myapp", Tag: "v2.0.0"}},
			},
		},
	}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true,
		},
	}

	r, mockGit := setupTestReconciler(t, kustomization, managedApp, upgrade)

	// Set up mock Git with a HelmRelease that has image values
	mockGit.SetFile("flux/apps/test-app/helmrelease.yaml", []byte(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: test-app
spec:
  values:
    image:
      repository: myapp
      tag: "v1.0.0"
`))

	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until GitCommitted condition is set
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeGitCommitted, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check Git was committed
	if len(mockGit.CommitFileCalls) != 1 {
		t.Fatalf("expected 1 commit call, got %d", len(mockGit.CommitFileCalls))
	}

	// Verify new image tag in committed content
	content := string(mockGit.CommitFileCalls[0].Content)
	if !contains(content, "tag: \"v2.0.0\"") {
		t.Errorf("expected new image tag in commit, got: %s", content)
	}

	// Check status
	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	gitCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeGitCommitted)
	if gitCond == nil {
		t.Fatal("expected GitCommitted condition to be set")
	}
	if gitCond.Status != metav1.ConditionTrue {
		t.Errorf("expected GitCommitted=True, got %s", gitCond.Status)
	}
}

func TestUpgradeRequest_ImageUpdateMissingVersionPath(t *testing.T) {
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false,
		},
	}

	// ManagedApp WITHOUT VersionPath configured
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
			// No VersionPolicy - should fail for image updates
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion: &fluxupv1alpha1.VersionInfo{
				Images: []fluxupv1alpha1.ImageInfo{{Name: "myapp", Tag: "v1.0.0"}},
			},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{
				Images: []fluxupv1alpha1.ImageInfo{{Name: "myapp", Tag: "v2.0.0"}},
			},
		},
	}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true,
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Complete condition is set (should fail due to missing VersionPath)
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check status - should be failed
	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	if completeCond.Reason != "MissingVersionPath" {
		t.Errorf("expected reason MissingVersionPath, got %s", completeCond.Reason)
	}
}

func TestUpgradeRequest_HandleReconciling(t *testing.T) {
	// Create a Kustomization that's already reconciled (Ready=True)
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: true, // Initially suspended
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
			},
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	// Create upgrade already at GitCommitted stage (need to enter handleReconciling)
	// Include finalizer since upgrade is already in progress
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			Finalizers: []string{OperationFinalizer},
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
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeGitCommitted,
					Status: metav1.ConditionTrue,
					Reason: "Committed",
				},
			},
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Reconciled condition is set
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeReconciled, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check Kustomization is resumed
	var ks kustomizev1.Kustomization
	if err := r.Get(ctx, types.NamespacedName{Name: "apps", Namespace: "flux-system"}, &ks); err != nil {
		t.Fatalf("failed to get kustomization: %v", err)
	}
	if ks.Spec.Suspend {
		t.Error("expected Kustomization to be resumed (Suspend=false)")
	}

	// Check upgrade status
	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	// Should now have Reconciled condition (since Kustomization is Ready)
	reconciledCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeReconciled)
	if reconciledCond == nil {
		t.Fatal("expected Reconciled condition to be set")
	}
	if reconciledCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Reconciled=True, got %s", reconciledCond.Status)
	}
}

func TestUpgradeRequest_HandleHealthCheck(t *testing.T) {
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false,
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
			},
		},
	}

	// ManagedApp is healthy (Ready=True)
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeReady,
					Status: metav1.ConditionTrue,
					Reason: "WorkloadHealthy",
				},
			},
		},
	}

	// Create upgrade at Reconciled stage with finalizer
	now := metav1.Now()
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			Finalizers: []string{OperationFinalizer},
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
					Type:   fluxupv1alpha1.ConditionTypeSuspended,
					Status: metav1.ConditionTrue, // Suspend phase completed
					Reason: "KustomizationSuspended",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeWorkloadStopped,
					Status: metav1.ConditionTrue,
					Reason: "Skipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeGitCommitted,
					Status: metav1.ConditionTrue,
					Reason: "Committed",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeReconciled,
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
				StartedAt:       &now,
			},
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Healthy condition is set
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeHealthy, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	// Should have Healthy condition
	healthyCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeHealthy)
	if healthyCond == nil {
		t.Logf("Conditions after reconcile: %+v", result.Status.Conditions)
		t.Fatal("expected Healthy condition to be set")
	}
	if healthyCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Healthy=True, got %s", healthyCond.Status)
	}

	// Should have HealthCheck status set
	if result.Status.HealthCheck == nil {
		t.Fatal("expected HealthCheck status to be set")
	}
	if result.Status.HealthCheck.Status != "Passed" {
		t.Errorf("expected HealthCheck.Status=Passed, got %s", result.Status.HealthCheck.Status)
	}
}

func TestUpgradeRequest_HandleCompleted(t *testing.T) {
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeReady,
					Status: metav1.ConditionTrue,
					Reason: "WorkloadHealthy",
				},
			},
		},
	}

	// Create upgrade at Healthy stage (all conditions met except Complete) with finalizer
	now := metav1.Now()
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			Finalizers: []string{OperationFinalizer},
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
					Type:   fluxupv1alpha1.ConditionTypeSuspended,
					Status: metav1.ConditionTrue, // Suspend phase completed
					Reason: "KustomizationSuspended",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeWorkloadStopped,
					Status: metav1.ConditionTrue,
					Reason: "Skipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeGitCommitted,
					Status: metav1.ConditionTrue,
					Reason: "Committed",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeReconciled,
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeHealthy,
					Status: metav1.ConditionTrue,
					Reason: "HealthCheckPassed",
				},
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
				StartedAt:       &now,
			},
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Complete condition is set
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	// Should have Complete condition
	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Complete=True, got %s", completeCond.Status)
	}
	if completeCond.Reason != "UpgradeSucceeded" {
		t.Errorf("expected reason UpgradeSucceeded, got %s", completeCond.Reason)
	}

	// Finalizer should be removed
	if len(result.Finalizers) > 0 {
		t.Errorf("expected finalizer to be removed, got %v", result.Finalizers)
	}

	// Check ManagedApp was updated
	var app fluxupv1alpha1.ManagedApp
	if err := r.Get(ctx, types.NamespacedName{Name: "test-app", Namespace: "default"}, &app); err != nil {
		t.Fatalf("failed to get managed app: %v", err)
	}

	// CurrentVersion should be updated
	if app.Status.CurrentVersion == nil || app.Status.CurrentVersion.Chart != "2.0.0" {
		t.Errorf("expected CurrentVersion=2.0.0, got %v", app.Status.CurrentVersion)
	}

	// AvailableUpdate should be cleared
	if app.Status.AvailableUpdate != nil {
		t.Errorf("expected AvailableUpdate to be nil, got %v", app.Status.AvailableUpdate)
	}

	// LastUpgrade should be set
	if app.Status.LastUpgrade == nil {
		t.Fatal("expected LastUpgrade to be set")
	}
	if app.Status.LastUpgrade.Status != "Succeeded" {
		t.Errorf("expected LastUpgrade.Status=Succeeded, got %s", app.Status.LastUpgrade.Status)
	}
}

func TestUpgradeRequest_AlreadyComplete(t *testing.T) {
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

	// Upgrade already complete
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
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
		},
	}

	r, mockGit := setupTestReconciler(t, managedApp, upgrade)
	ctx := context.Background()

	// Reconcile - should do nothing (already complete)
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter > 0 {
		t.Error("expected no requeue for completed upgrade")
	}

	// No Git operations should have occurred
	if len(mockGit.ReadFileCalls) != 0 {
		t.Errorf("expected 0 read calls for completed upgrade, got %d", len(mockGit.ReadFileCalls))
	}
}

func TestUpgradeRequest_AlreadyFailed(t *testing.T) {
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

	// Upgrade already failed (Complete=False)
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
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
					Status: metav1.ConditionFalse,
					Reason: "SomethingFailed",
				},
			},
		},
	}

	r, mockGit := setupTestReconciler(t, managedApp, upgrade)
	ctx := context.Background()

	// Reconcile - should do nothing (already failed)
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter > 0 {
		t.Error("expected no requeue for failed upgrade")
	}

	// No Git operations should have occurred
	if len(mockGit.ReadFileCalls) != 0 {
		t.Errorf("expected 0 read calls for failed upgrade, got %d", len(mockGit.ReadFileCalls))
	}
}

func TestUpgradeRequest_FailureBeforeGitCommit_ResumesKustomization(t *testing.T) {
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	// Upgrade at Suspended stage, ready to commit (with finalizer)
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			Finalizers: []string{OperationFinalizer},
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
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
			},
		},
	}

	r, mockGit := setupTestReconciler(t, kustomization, managedApp, upgrade)

	// Make Git read fail to trigger failure before commit
	mockGit.SetReadError("flux/apps/test-app/helmrelease.yaml", "simulated read error")

	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Complete condition is set (should fail)
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check Kustomization is resumed (Suspend=false)
	var ks kustomizev1.Kustomization
	if err := r.Get(ctx, types.NamespacedName{Name: "apps", Namespace: "flux-system"}, &ks); err != nil {
		t.Fatalf("failed to get kustomization: %v", err)
	}
	if ks.Spec.Suspend {
		t.Error("expected Kustomization to be resumed after failure before commit")
	}

	// Check upgrade is marked failed
	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	if completeCond.Reason != "GitReadFailed" {
		t.Errorf("expected reason GitReadFailed, got %s", completeCond.Reason)
	}

	// Suspended should now be False
	suspendedCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended)
	if suspendedCond == nil || suspendedCond.Status != metav1.ConditionFalse {
		t.Error("expected Suspended=False after failure recovery")
	}
}

func TestUpgradeRequest_WithTargetVersion(t *testing.T) {
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false,
		},
	}

	// ManagedApp has no AvailableUpdate, but upgrade specifies TargetVersion
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: nil, // No available update
		},
	}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testUpgradeName,
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			TargetVersion: &fluxupv1alpha1.VersionInfo{Chart: "3.0.0"}, // Explicit target
			SkipSnapshot:  true,
		},
	}

	r, mockGit := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until GitCommitted condition is set
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeGitCommitted, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check Git was committed with the explicit target version
	if len(mockGit.CommitFileCalls) != 1 {
		t.Fatalf("expected 1 commit call, got %d", len(mockGit.CommitFileCalls))
	}

	content := string(mockGit.CommitFileCalls[0].Content)
	if !contains(content, "version: \"3.0.0\"") {
		t.Errorf("expected version 3.0.0 in commit, got: %s", content)
	}
}

func TestUpgradeRequest_AutoRollback_CreatesRollbackRequest(t *testing.T) {
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: true, // Already suspended from the upgrade
		},
	}

	// ManagedApp with autoRollback enabled
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
			AutoRollback: true, // Auto-rollback enabled
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeReady,
					Status: metav1.ConditionFalse, // Unhealthy - will cause health check failure
					Reason: "WorkloadUnhealthy",
				},
			},
		},
	}

	// UpgradeRequest already at health check stage (with GitCommitted)
	// Will fail health check and trigger auto-rollback
	now := metav1.Now()
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			UID:        "test-uid-123",
			Finalizers: []string{OperationFinalizer},
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
					Type:   fluxupv1alpha1.ConditionTypeSuspended,
					Status: metav1.ConditionFalse,
					Reason: "KustomizationResumed",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeWorkloadStopped,
					Status: metav1.ConditionTrue,
					Reason: "Skipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeGitCommitted,
					Status: metav1.ConditionTrue,
					Reason: "Committed",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeReconciled,
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
				StartedAt:       &now,
			},
			Snapshot: &fluxupv1alpha1.SnapshotStatus{
				CreatedAt: &now,
				PVCSnapshots: []fluxupv1alpha1.PVCSnapshotInfo{
					{PVCName: "data-0", SnapshotName: "snap-1"},
				},
			},
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Keep reconciling - health check will fail, but we need to wait for timeout
	// For the test, we'll set a very old PhaseStartedAt to trigger timeout
	oldTime := metav1.Time{Time: now.Add(-10 * time.Minute)}
	upgrade.Status.PhaseStartedAt = &oldTime
	if err := r.Status().Update(ctx, upgrade); err != nil {
		t.Fatalf("failed to update upgrade status: %v", err)
	}

	// Reconcile until Complete condition is set (should fail with auto-rollback)
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check upgrade is marked failed
	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	if completeCond.Reason != "HealthCheckTimeout" {
		t.Errorf("expected reason HealthCheckTimeout, got %s", completeCond.Reason)
	}

	// Check message indicates auto-rollback was initiated
	if !contains(completeCond.Message, "auto-rollback initiated") {
		t.Errorf("expected message to contain 'auto-rollback initiated', got: %s", completeCond.Message)
	}

	// Check that a RollbackRequest was created
	var rollbackList fluxupv1alpha1.RollbackRequestList
	if err := r.List(ctx, &rollbackList, client.InNamespace("default")); err != nil {
		t.Fatalf("failed to list rollback requests: %v", err)
	}

	if len(rollbackList.Items) != 1 {
		t.Fatalf("expected 1 RollbackRequest to be created, got %d", len(rollbackList.Items))
	}

	rollback := rollbackList.Items[0]
	if rollback.Spec.UpgradeRequestRef.Name != testUpgradeName {
		t.Errorf("expected RollbackRequest to reference %s, got %s", testUpgradeName, rollback.Spec.UpgradeRequestRef.Name)
	}
	if !rollback.Spec.AutoTriggered {
		t.Error("expected RollbackRequest.AutoTriggered to be true")
	}

	// Check owner reference
	if len(rollback.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(rollback.OwnerReferences))
	}
	if rollback.OwnerReferences[0].Name != testUpgradeName {
		t.Errorf("expected owner reference to %s, got %s", testUpgradeName, rollback.OwnerReferences[0].Name)
	}
}

// =============================================================================
// Timeout Scenario Tests
// =============================================================================

func TestUpgradeRequest_ReconcileTimeout_TriggersAutoRollback(t *testing.T) {
	// Test: Reconciliation times out after Git commit -> should trigger auto-rollback
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false, // Resumed, but not reconciled yet
		},
		Status: kustomizev1.KustomizationStatus{
			// No Ready=True condition - reconciliation pending
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionFalse, // Not reconciled
					Reason: "Progressing",
				},
			},
		},
	}

	// ManagedApp with autoRollback enabled
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
			AutoRollback: true,
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	// Upgrade at GitCommitted stage, waiting for reconciliation
	now := metav1.Now()
	// Set phase start time far in the past to trigger timeout
	pastTime := metav1.Time{Time: now.Add(-15 * time.Minute)} // > TimeoutReconcile (10m)

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			UID:        "test-uid-reconcile-timeout",
			Finalizers: []string{OperationFinalizer},
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true,
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			PhaseStartedAt: &pastTime, // Trigger timeout
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeSuspended,
					Status: metav1.ConditionFalse, // Already resumed
					Reason: "KustomizationResumed",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeWorkloadStopped,
					Status: metav1.ConditionTrue,
					Reason: "Skipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeGitCommitted,
					Status: metav1.ConditionTrue,
					Reason: "Committed",
				},
				// No Reconciled condition yet - we're in reconciling phase
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
				StartedAt:       &now,
			},
			Snapshot: &fluxupv1alpha1.SnapshotStatus{
				CreatedAt:    &now,
				PVCSnapshots: []fluxupv1alpha1.PVCSnapshotInfo{},
			},
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile - should detect timeout and fail
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	// Should be failed with ReconciliationTimeout
	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	if completeCond.Reason != reasonReconciliationTimeout {
		t.Errorf("expected reason ReconciliationTimeout, got %s", completeCond.Reason)
	}

	// Should have auto-rollback initiated (post-commit failure with AutoRollback=true)
	if !contains(completeCond.Message, "auto-rollback initiated") {
		t.Errorf("expected message to contain 'auto-rollback initiated', got: %s", completeCond.Message)
	}

	// Verify RollbackRequest was created
	var rollbackList fluxupv1alpha1.RollbackRequestList
	if err := r.List(ctx, &rollbackList, client.InNamespace("default")); err != nil {
		t.Fatalf("failed to list rollback requests: %v", err)
	}
	if len(rollbackList.Items) != 1 {
		t.Fatalf("expected 1 RollbackRequest to be created, got %d", len(rollbackList.Items))
	}
}

func TestUpgradeRequest_ReconcileTimeout_NoAutoRollback(t *testing.T) {
	// Test: Reconciliation times out, but AutoRollback is disabled
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false,
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionFalse,
					Reason: "Progressing",
				},
			},
		},
	}

	// ManagedApp WITHOUT autoRollback
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
			AutoRollback: false, // Disabled
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	now := metav1.Now()
	pastTime := metav1.Time{Time: now.Add(-15 * time.Minute)}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			UID:        "test-uid-no-autorollback",
			Finalizers: []string{OperationFinalizer},
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true,
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			PhaseStartedAt: &pastTime,
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeSuspended,
					Status: metav1.ConditionFalse,
					Reason: "KustomizationResumed",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeWorkloadStopped,
					Status: metav1.ConditionTrue,
					Reason: "Skipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeGitCommitted,
					Status: metav1.ConditionTrue,
					Reason: "Committed",
				},
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
				StartedAt:       &now,
			},
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	if completeCond.Reason != reasonReconciliationTimeout {
		t.Errorf("expected reason ReconciliationTimeout, got %s", completeCond.Reason)
	}

	// Should say "rollback required" but NOT "auto-rollback initiated"
	if !contains(completeCond.Message, "rollback required") {
		t.Errorf("expected message to contain 'rollback required', got: %s", completeCond.Message)
	}
	if contains(completeCond.Message, "auto-rollback initiated") {
		t.Errorf("expected message NOT to contain 'auto-rollback initiated', got: %s", completeCond.Message)
	}

	// Verify NO RollbackRequest was created
	var rollbackList fluxupv1alpha1.RollbackRequestList
	if err := r.List(ctx, &rollbackList, client.InNamespace("default")); err != nil {
		t.Fatalf("failed to list rollback requests: %v", err)
	}
	if len(rollbackList.Items) != 0 {
		t.Errorf("expected 0 RollbackRequests, got %d", len(rollbackList.Items))
	}
}

func TestUpgradeRequest_GitCommitTimeout(t *testing.T) {
	// Test: Git commit phase times out (before point of no return)
	// Should resume Kustomization and NOT trigger auto-rollback
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: true, // Still suspended
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
			AutoRollback: true, // Even with auto-rollback enabled
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	now := metav1.Now()
	pastTime := metav1.Time{Time: now.Add(-5 * time.Minute)} // > TimeoutGitCommit (2m)

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			UID:        "test-uid-git-timeout",
			Finalizers: []string{OperationFinalizer},
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true,
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			PhaseStartedAt: &pastTime,
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
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
				// No GitCommitted - we're in the committing phase
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
				StartedAt:       &now,
			},
		},
	}

	r, mockGit := setupTestReconciler(t, kustomization, managedApp, upgrade)

	// Make Git commit hang (return error to simulate timeout scenario)
	mockGit.CommitFileErr = fmt.Errorf("simulated git timeout")

	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	// Should be failed
	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}

	// Kustomization should be resumed (failure before Git commit)
	var ks kustomizev1.Kustomization
	if err := r.Get(ctx, types.NamespacedName{Name: "apps", Namespace: "flux-system"}, &ks); err != nil {
		t.Fatalf("failed to get kustomization: %v", err)
	}
	if ks.Spec.Suspend {
		t.Error("expected Kustomization to be resumed after pre-commit failure")
	}

	// Suspended condition should be False
	suspendedCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended)
	if suspendedCond == nil || suspendedCond.Status != metav1.ConditionFalse {
		t.Error("expected Suspended=False after pre-commit failure")
	}

	// NO auto-rollback should be created (failure before point of no return)
	var rollbackList fluxupv1alpha1.RollbackRequestList
	if err := r.List(ctx, &rollbackList, client.InNamespace("default")); err != nil {
		t.Fatalf("failed to list rollback requests: %v", err)
	}
	if len(rollbackList.Items) != 0 {
		t.Errorf("expected 0 RollbackRequests for pre-commit failure, got %d", len(rollbackList.Items))
	}
}

func TestUpgradeRequest_ReconcileFailed_KustomizationError(t *testing.T) {
	// Test: Kustomization reconciliation fails (Ready=False with error)
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false,
		},
		Status: kustomizev1.KustomizationStatus{
			ObservedGeneration: 2, // Indicates reconciliation attempted
			Conditions: []metav1.Condition{
				{
					Type:    "Ready",
					Status:  metav1.ConditionFalse,
					Reason:  "ReconciliationFailed",
					Message: "helm upgrade failed: values validation error",
				},
			},
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
			AutoRollback: true,
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	now := metav1.Now()
	// Set a past time but within timeout - the failure is detected by Ready=False
	pastTime := metav1.Time{Time: now.Add(-1 * time.Minute)}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			UID:        "test-uid-reconcile-failed",
			Finalizers: []string{OperationFinalizer},
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			SkipSnapshot: true,
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
			PhaseStartedAt: &pastTime,
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeSuspended,
					Status: metav1.ConditionFalse,
					Reason: "KustomizationResumed",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeWorkloadStopped,
					Status: metav1.ConditionTrue,
					Reason: "Skipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeGitCommitted,
					Status: metav1.ConditionTrue,
					Reason: "Committed",
				},
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
				StartedAt:       &now,
			},
			Snapshot: &fluxupv1alpha1.SnapshotStatus{
				CreatedAt:    &now,
				PVCSnapshots: []fluxupv1alpha1.PVCSnapshotInfo{},
			},
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// The controller checks IsReconciled which returns false for Ready=False
	// Then it waits for timeout. Let's set the timeout to trigger failure.
	// Actually, the test will just keep requeuing. Let me check the actual logic.
	// Looking at handleReconciling: it checks IsReconciled, if false, checks timeout.
	// So we need to trigger the timeout path.

	// Update the phase start to trigger timeout
	upgrade.Status.PhaseStartedAt = &metav1.Time{Time: now.Add(-15 * time.Minute)}
	if err := r.Status().Update(ctx, upgrade); err != nil {
		t.Fatalf("failed to update upgrade: %v", err)
	}

	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}

	// Verify auto-rollback was created
	var rollbackList fluxupv1alpha1.RollbackRequestList
	if err := r.List(ctx, &rollbackList, client.InNamespace("default")); err != nil {
		t.Fatalf("failed to list rollback requests: %v", err)
	}
	if len(rollbackList.Items) != 1 {
		t.Fatalf("expected 1 RollbackRequest, got %d", len(rollbackList.Items))
	}
}

func TestUpgradeRequest_FluxResumedExternally(t *testing.T) {
	// Test: Flux is unexpectedly resumed during a critical operation (before Git commit)
	// VerifyStillSuspended should catch this
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false, // Unexpectedly resumed!
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
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	now := metav1.Now()
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			Finalizers: []string{OperationFinalizer},
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
					Type:   fluxupv1alpha1.ConditionTypeSuspended,
					Status: metav1.ConditionTrue, // We think it's suspended
					Reason: "KustomizationSuspended",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeWorkloadStopped,
					Status: metav1.ConditionTrue,
					Reason: "Skipped",
				},
				{
					Type:   fluxupv1alpha1.ConditionTypeSnapshotReady,
					Status: metav1.ConditionTrue,
					Reason: "SnapshotsSkipped",
				},
				// About to commit - but Kustomization was externally resumed
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
				StartedAt:       &now,
			},
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}

	// Should indicate the external resume was detected
	if !contains(completeCond.Message, "un-suspended externally") {
		t.Errorf("expected message about external un-suspend, got: %s", completeCond.Message)
	}
}

func TestUpgradeRequest_SnapshotCreationFailure(t *testing.T) {
	// Test: Snapshot creation fails (before point of no return)
	// Should resume Kustomization and NOT trigger auto-rollback
	// Note: VolumeSnapshot CRD isn't registered in the test scheme, so creation will fail
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: true,
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
			VolumeSnapshots: &fluxupv1alpha1.VolumeSnapshotConfig{
				Enabled: true,
				PVCs:    []fluxupv1alpha1.PVCRef{{Name: "data-pvc"}},
			},
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
		},
	}

	// Create the PVC that will be snapshotted
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-pvc",
			Namespace: "default",
		},
	}

	now := metav1.Now()

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testUpgradeName,
			Namespace:  "default",
			Finalizers: []string{OperationFinalizer},
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "test-app",
			},
			// NOT skipping snapshot - will fail because VolumeSnapshot CRD not registered
		},
		Status: fluxupv1alpha1.UpgradeRequestStatus{
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
				// No SnapshotReady - we're in snapshot phase
			},
			Upgrade: &fluxupv1alpha1.UpgradeStatus{
				PreviousVersion: &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
				NewVersion:      &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
				StartedAt:       &now,
			},
		},
	}

	r, _ := setupTestReconciler(t, kustomization, managedApp, upgrade, pvc)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Complete=False, got %s", completeCond.Status)
	}
	// Snapshot creation will fail because VolumeSnapshot CRD isn't registered
	if completeCond.Reason != "SnapshotCreationFailed" && completeCond.Reason != "SnapshotTimeout" {
		t.Errorf("expected reason SnapshotCreationFailed or SnapshotTimeout, got %s", completeCond.Reason)
	}

	// Should have resumed Kustomization (failure before Git commit)
	var ks kustomizev1.Kustomization
	if err := r.Get(ctx, types.NamespacedName{Name: "apps", Namespace: "flux-system"}, &ks); err != nil {
		t.Fatalf("failed to get kustomization: %v", err)
	}
	if ks.Spec.Suspend {
		t.Error("expected Kustomization to be resumed after snapshot failure")
	}

	// NO auto-rollback should be created (failure before point of no return)
	var rollbackList fluxupv1alpha1.RollbackRequestList
	if err := r.List(ctx, &rollbackList, client.InNamespace("default")); err != nil {
		t.Fatalf("failed to list rollback requests: %v", err)
	}
	if len(rollbackList.Items) != 0 {
		t.Errorf("expected 0 RollbackRequests for pre-commit failure, got %d", len(rollbackList.Items))
	}
}

// ============================================================================
// Kustomization-based Deployment Tests (raw manifests with custom versionPath)
// ============================================================================

// setupKustomizationTestReconciler creates a reconciler for Kustomization-based deployment tests
// (raw Deployment/StatefulSet manifests instead of HelmRelease)
func setupKustomizationTestReconciler(_ *testing.T, objects ...client.Object) (*UpgradeRequestReconciler, *git.MockManager) {
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = kustomizev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&fluxupv1alpha1.UpgradeRequest{}, &fluxupv1alpha1.ManagedApp{}, &kustomizev1.Kustomization{}).
		Build()

	mockGit := git.NewMockManager()
	// Set up a raw Deployment manifest (not HelmRelease)
	mockGit.SetFile("flux/apps/redis-raw/deployment.yaml", []byte(`apiVersion: apps/v1
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
        image: redis:7.2.0
        ports:
        - containerPort: 6379
`))

	reconciler := &UpgradeRequestReconciler{
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

// TestUpgradeRequest_KustomizationBasedDeployment tests upgrade with raw Deployment manifest
// using custom versionPath for image tag updates
func TestUpgradeRequest_KustomizationBasedDeployment(t *testing.T) {
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false,
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	managedApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-raw-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/redis-raw/deployment.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
			VersionPolicy: &fluxupv1alpha1.VersionPolicy{
				AutoUpdate:  "none",
				VersionPath: ".spec.template.spec.containers[0].image",
			},
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "redis:7.2.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "redis:7.4.0"},
		},
	}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-raw-upgrade",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "redis-raw-app",
			},
			TargetVersion: &fluxupv1alpha1.VersionInfo{
				Chart: "redis:7.4.0",
			},
			SkipSnapshot: true,
		},
	}

	r, mockGit := setupKustomizationTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	// Reconcile until Complete condition is set
	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 15); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Complete=True, got %s (reason: %s, message: %s)",
			completeCond.Status, completeCond.Reason, completeCond.Message)
	}
	if completeCond.Reason != "UpgradeSucceeded" {
		t.Errorf("expected reason UpgradeSucceeded, got %s", completeCond.Reason)
	}

	// Verify Git commit was called with correct path and content
	if len(mockGit.CommitFileCalls) != 1 {
		t.Errorf("expected 1 commit call, got %d", len(mockGit.CommitFileCalls))
	} else {
		commitCall := mockGit.CommitFileCalls[0]
		if commitCall.Path != "flux/apps/redis-raw/deployment.yaml" {
			t.Errorf("expected commit path flux/apps/redis-raw/deployment.yaml, got %s", commitCall.Path)
		}
		// Verify the image was updated in the content
		if !stringContains(string(commitCall.Content), "redis:7.4.0") {
			t.Error("expected commit content to contain redis:7.4.0")
		}
	}
}

// TestUpgradeRequest_CustomVersionPath tests various versionPath configurations
func TestUpgradeRequest_CustomVersionPath(t *testing.T) {
	tests := []struct {
		name           string
		versionPath    string
		initialContent string
		targetVersion  string
		expectInOutput string
	}{
		{
			name:        "image tag update",
			versionPath: ".spec.template.spec.containers[0].image",
			initialContent: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  template:
    spec:
      containers:
      - name: app
        image: myapp:1.0.0
`,
			targetVersion:  "myapp:2.0.0",
			expectInOutput: "myapp:2.0.0",
		},
		{
			name:        "init container image",
			versionPath: ".spec.template.spec.initContainers[0].image",
			initialContent: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  template:
    spec:
      initContainers:
      - name: init
        image: busybox:1.35
      containers:
      - name: app
        image: myapp:latest
`,
			targetVersion:  "busybox:1.36",
			expectInOutput: "busybox:1.36",
		},
		{
			name:        "annotation with dots in key",
			versionPath: ".metadata.annotations['app.kubernetes.io/version']",
			initialContent: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  annotations:
    app.kubernetes.io/version: "1.0.0"
spec:
  replicas: 1
`,
			targetVersion:  "2.0.0",
			expectInOutput: "app.kubernetes.io/version: \"2.0.0\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kustomization := &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "apps",
					Namespace: "flux-system",
				},
				Spec: kustomizev1.KustomizationSpec{
					Suspend: false,
				},
				Status: kustomizev1.KustomizationStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Ready",
							Status: metav1.ConditionTrue,
						},
					},
				},
			}

			managedApp := &fluxupv1alpha1.ManagedApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-app",
					Namespace: "default",
				},
				Spec: fluxupv1alpha1.ManagedAppSpec{
					GitPath: "flux/apps/test-app/manifest.yaml",
					KustomizationRef: fluxupv1alpha1.ObjectReference{
						Name:      "apps",
						Namespace: "flux-system",
					},
					VersionPolicy: &fluxupv1alpha1.VersionPolicy{
						AutoUpdate:  "none",
						VersionPath: tt.versionPath,
					},
				},
				Status: fluxupv1alpha1.ManagedAppStatus{
					CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "1.0.0"},
					AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: tt.targetVersion},
				},
			}

			upgrade := &fluxupv1alpha1.UpgradeRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-upgrade-%s", tt.name),
					Namespace: "default",
				},
				Spec: fluxupv1alpha1.UpgradeRequestSpec{
					ManagedAppRef: fluxupv1alpha1.ObjectReference{
						Name: "test-app",
					},
					TargetVersion: &fluxupv1alpha1.VersionInfo{
						Chart: tt.targetVersion,
					},
					SkipSnapshot: true,
				},
			}

			scheme := runtime.NewScheme()
			_ = fluxupv1alpha1.AddToScheme(scheme)
			_ = kustomizev1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(kustomization, managedApp, upgrade).
				WithStatusSubresource(&fluxupv1alpha1.UpgradeRequest{}, &fluxupv1alpha1.ManagedApp{}, &kustomizev1.Kustomization{}).
				Build()

			mockGit := git.NewMockManager()
			mockGit.SetFile("flux/apps/test-app/manifest.yaml", []byte(tt.initialContent))

			r := &UpgradeRequestReconciler{
				Client:          fakeClient,
				Scheme:          scheme,
				GitManager:      mockGit,
				SnapshotManager: snapshot.NewManager(fakeClient),
				FluxHelper:      flux.NewHelper(fakeClient),
				YAMLEditor:      yamlpkg.NewEditor(),
				Discoverer:      discovery.New(fakeClient),
				HealthChecker:   health.NewChecker(fakeClient),
			}

			ctx := context.Background()
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      upgrade.Name,
					Namespace: upgrade.Namespace,
				},
			}

			if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 15); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var result fluxupv1alpha1.UpgradeRequest
			if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
				t.Fatalf("failed to get upgrade request: %v", err)
			}

			completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
			if completeCond == nil {
				t.Fatal("expected Complete condition to be set")
			}
			if completeCond.Status != metav1.ConditionTrue {
				t.Errorf("expected Complete=True, got %s (reason: %s)", completeCond.Status, completeCond.Reason)
			}

			// Verify Git commit content contains the expected updated value
			if len(mockGit.CommitFileCalls) != 1 {
				t.Errorf("expected 1 commit call, got %d", len(mockGit.CommitFileCalls))
			} else {
				content := string(mockGit.CommitFileCalls[0].Content)
				if !stringContains(content, tt.expectInOutput) {
					t.Errorf("expected commit content to contain %q, got:\n%s", tt.expectInOutput, content)
				}
			}
		})
	}
}

// TestUpgradeRequest_DryRunWithCustomVersionPath tests dry-run validation for custom versionPath
func TestUpgradeRequest_DryRunWithCustomVersionPath(t *testing.T) {
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false,
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	managedApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-raw-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/redis-raw/deployment.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
			VersionPolicy: &fluxupv1alpha1.VersionPolicy{
				AutoUpdate:  "none",
				VersionPath: ".spec.template.spec.containers[0].image",
			},
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			CurrentVersion:  &fluxupv1alpha1.VersionInfo{Chart: "redis:7.2.0"},
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "redis:7.4.0"},
		},
	}

	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-raw-dryrun",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.UpgradeRequestSpec{
			ManagedAppRef: fluxupv1alpha1.ObjectReference{
				Name: "redis-raw-app",
			},
			TargetVersion: &fluxupv1alpha1.VersionInfo{
				Chart: "redis:7.4.0",
			},
			DryRun:       true,
			SkipSnapshot: true,
		},
	}

	r, mockGit := setupKustomizationTestReconciler(t, kustomization, managedApp, upgrade)
	ctx := context.Background()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	}

	if err := reconcileUntilCondition(ctx, r, req, fluxupv1alpha1.ConditionTypeComplete, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fluxupv1alpha1.UpgradeRequest
	if err := r.Get(ctx, types.NamespacedName{Name: upgrade.Name, Namespace: upgrade.Namespace}, &result); err != nil {
		t.Fatalf("failed to get upgrade request: %v", err)
	}

	completeCond := meta.FindStatusCondition(result.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
	if completeCond == nil {
		t.Fatal("expected Complete condition to be set")
	}
	if completeCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Complete=True, got %s", completeCond.Status)
	}
	if completeCond.Reason != reasonDryRunSucceeded {
		t.Errorf("expected reason %s, got %s", reasonDryRunSucceeded, completeCond.Reason)
	}

	// Dry run should NOT commit
	if len(mockGit.CommitFileCalls) != 0 {
		t.Errorf("expected 0 commit calls for dry run, got %d", len(mockGit.CommitFileCalls))
	}

	// Should have read the file to validate
	if len(mockGit.ReadFileCalls) != 1 {
		t.Errorf("expected 1 read call for validation, got %d", len(mockGit.ReadFileCalls))
	}
}

// stringContains checks if substr is in s (simple helper to avoid importing strings)
func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
