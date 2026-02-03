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
	"github.com/nbenn/fluxup/internal/flux"
	"github.com/nbenn/fluxup/internal/git"
	"github.com/nbenn/fluxup/internal/snapshot"
	yamlpkg "github.com/nbenn/fluxup/internal/yaml"
)

func setupTestReconciler(t *testing.T, objects ...client.Object) (*UpgradeRequestReconciler, *git.MockManager) {
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = kustomizev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&fluxupv1alpha1.UpgradeRequest{}, &kustomizev1.Kustomization{}).
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
	}

	return reconciler, mockGit
}

func TestUpgradeRequest_ManagedAppNotFound(t *testing.T) {
	upgrade := &fluxupv1alpha1.UpgradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-upgrade",
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

	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	})
	if err != nil {
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
			Name:      "test-upgrade",
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

	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	})
	if err != nil {
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
			Name:      "test-upgrade",
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

	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	})
	if err != nil {
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
	if completeCond.Reason != "DryRunSucceeded" {
		t.Errorf("expected reason DryRunSucceeded, got %s", completeCond.Reason)
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
			Name:      "test-upgrade",
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

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after suspend")
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
			Name:      "test-upgrade",
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

	// First reconcile - suspend
	_, _ = r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	})

	// Second reconcile - commit
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		},
	})
	if err != nil {
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
