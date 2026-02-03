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

package renovate

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

const testNewVersion = "10.1.0"

func TestStatusUpdater_UpdateManagedAppStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/test-app/helmrelease.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name: "apps",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	updater := &StatusUpdater{
		client: client,
	}

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-app", Namespace: "default"}
	version := &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"}

	// Update the status
	err := updater.updateManagedAppStatus(ctx, key, version)
	if err != nil {
		t.Fatalf("updateManagedAppStatus() error = %v", err)
	}

	// Verify the update
	var updated fluxupv1alpha1.ManagedApp
	if err := client.Get(ctx, key, &updated); err != nil {
		t.Fatalf("failed to get ManagedApp: %v", err)
	}

	if updated.Status.AvailableUpdate == nil {
		t.Fatal("expected AvailableUpdate to be set")
	}
	if updated.Status.AvailableUpdate.Chart != "2.0.0" {
		t.Errorf("AvailableUpdate.Chart = %q, want %q", updated.Status.AvailableUpdate.Chart, "2.0.0")
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, fluxupv1alpha1.ConditionTypeUpdateAvailable)
	if cond == nil {
		t.Fatal("expected UpdateAvailable condition to be set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("UpdateAvailable status = %v, want %v", cond.Status, metav1.ConditionTrue)
	}
	if cond.Reason != "UpdateDetected" {
		t.Errorf("UpdateAvailable reason = %q, want %q", cond.Reason, "UpdateDetected")
	}
}

func TestStatusUpdater_ClearUpdateStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Start with an app that has an available update
	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/test-app/helmrelease.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name: "apps",
			},
		},
		Status: fluxupv1alpha1.ManagedAppStatus{
			AvailableUpdate: &fluxupv1alpha1.VersionInfo{Chart: "2.0.0"},
			Conditions: []metav1.Condition{
				{
					Type:   fluxupv1alpha1.ConditionTypeUpdateAvailable,
					Status: metav1.ConditionTrue,
					Reason: "UpdateDetected",
				},
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	updater := &StatusUpdater{
		client: client,
	}

	ctx := context.Background()
	key := types.NamespacedName{Name: "test-app", Namespace: "default"}

	// Clear the status
	err := updater.ClearUpdateStatus(ctx, key)
	if err != nil {
		t.Fatalf("ClearUpdateStatus() error = %v", err)
	}

	// Verify the update was cleared
	var updated fluxupv1alpha1.ManagedApp
	if err := client.Get(ctx, key, &updated); err != nil {
		t.Fatalf("failed to get ManagedApp: %v", err)
	}

	if updated.Status.AvailableUpdate != nil {
		t.Error("expected AvailableUpdate to be nil")
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, fluxupv1alpha1.ConditionTypeUpdateAvailable)
	if cond == nil {
		t.Fatal("expected UpdateAvailable condition to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("UpdateAvailable status = %v, want %v", cond.Status, metav1.ConditionFalse)
	}
	if cond.Reason != "NoUpdateAvailable" {
		t.Errorf("UpdateAvailable reason = %q, want %q", cond.Reason, "NoUpdateAvailable")
	}
}

func TestStatusUpdater_ProcessRenovateOutput_NoUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Create ConfigMap with Renovate log entry structure but no updates
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "renovate-updates",
			Namespace: "fluxup-system",
		},
		Data: map[string]string{
			"updates.json": `{"name":"renovate","level":20,"msg":"packageFiles with updates","config":{"flux":[]}}`,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cm).
		Build()

	updater := NewStatusUpdater(client, "fluxup-system", "renovate-updates")
	ctx := context.Background()

	// Should return nil when no updates
	err := updater.ProcessRenovateOutput(ctx)
	if err != nil {
		t.Fatalf("ProcessRenovateOutput() error = %v", err)
	}
}

func TestStatusUpdater_ProcessRenovateOutput_WithUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Create ManagedApp
	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gitea",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			GitPath: "flux/apps/gitea/helmrelease.yaml",
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name: "apps",
			},
		},
	}

	// Create ConfigMap with Renovate log entry structure containing an update
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "renovate-updates",
			Namespace: "fluxup-system",
		},
		Data: map[string]string{
			"updates.json": `{"name":"renovate","level":20,"msg":"packageFiles with updates","config":{"flux":[{"packageFile":"flux/apps/gitea/helmrelease.yaml","deps":[{"depName":"gitea","currentValue":"10.0.0","datasource":"helm","updates":[{"newValue":"10.1.0","updateType":"minor"}]}]}]}}`,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, cm).
		WithStatusSubresource(app).
		Build()

	updater := NewStatusUpdater(client, "fluxup-system", "renovate-updates")
	ctx := context.Background()

	// Process updates
	err := updater.ProcessRenovateOutput(ctx)
	if err != nil {
		t.Fatalf("ProcessRenovateOutput() error = %v", err)
	}

	// Verify ManagedApp was updated
	var updated fluxupv1alpha1.ManagedApp
	key := types.NamespacedName{Name: "gitea", Namespace: "default"}
	if err := client.Get(ctx, key, &updated); err != nil {
		t.Fatalf("failed to get ManagedApp: %v", err)
	}

	if updated.Status.AvailableUpdate == nil {
		t.Fatal("expected AvailableUpdate to be set")
	}
	if updated.Status.AvailableUpdate.Chart != testNewVersion {
		t.Errorf("AvailableUpdate.Chart = %q, want %q", updated.Status.AvailableUpdate.Chart, testNewVersion)
	}
}
