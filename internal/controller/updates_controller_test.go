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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/renovate"
)

func TestUpdatesConfigMapReconciler_IgnoresOtherConfigMaps(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = fluxupv1alpha1.AddToScheme(scheme)

	// Create a different ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-other-configmap",
			Namespace: "default",
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cm).
		Build()

	r := &UpdatesConfigMapReconciler{
		Client:        client,
		Scheme:        scheme,
		StatusUpdater: renovate.NewStatusUpdater(client, UpdatesConfigMapNamespace, UpdatesConfigMapName),
	}

	ctx := context.Background()

	// Reconcile a different ConfigMap - should do nothing
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "some-other-configmap",
			Namespace: "default",
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter > 0 {
		t.Error("expected no requeue for non-matching ConfigMap")
	}
}

func TestUpdatesConfigMapReconciler_IgnoresWrongNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = fluxupv1alpha1.AddToScheme(scheme)

	// Create ConfigMap with right name but wrong namespace
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      UpdatesConfigMapName,
			Namespace: "wrong-namespace",
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cm).
		Build()

	r := &UpdatesConfigMapReconciler{
		Client:        client,
		Scheme:        scheme,
		StatusUpdater: renovate.NewStatusUpdater(client, UpdatesConfigMapNamespace, UpdatesConfigMapName),
	}

	ctx := context.Background()

	// Reconcile - should ignore due to wrong namespace
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      UpdatesConfigMapName,
			Namespace: "wrong-namespace",
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter > 0 {
		t.Error("expected no requeue for wrong namespace")
	}
}

func TestUpdatesConfigMapReconciler_ProcessesCorrectConfigMap(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = fluxupv1alpha1.AddToScheme(scheme)

	// Create ManagedApp that should receive the update
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

	// Create the updates ConfigMap with correct name/namespace and Renovate log entry format
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      UpdatesConfigMapName,
			Namespace: UpdatesConfigMapNamespace,
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

	r := &UpdatesConfigMapReconciler{
		Client:        client,
		Scheme:        scheme,
		StatusUpdater: renovate.NewStatusUpdater(client, UpdatesConfigMapNamespace, UpdatesConfigMapName),
	}

	ctx := context.Background()

	// Reconcile the correct ConfigMap
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      UpdatesConfigMapName,
			Namespace: UpdatesConfigMapNamespace,
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter > 0 {
		t.Error("expected no requeue after successful processing")
	}

	// Verify the ManagedApp was updated
	var updated fluxupv1alpha1.ManagedApp
	if err := client.Get(ctx, types.NamespacedName{Name: "gitea", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get ManagedApp: %v", err)
	}

	if updated.Status.AvailableUpdate == nil {
		t.Fatal("expected AvailableUpdate to be set")
	}
	if updated.Status.AvailableUpdate.Chart != "10.1.0" {
		t.Errorf("AvailableUpdate.Chart = %q, want %q", updated.Status.AvailableUpdate.Chart, "10.1.0")
	}
}

func TestUpdatesConfigMapReconciler_HandlesEmptyUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = fluxupv1alpha1.AddToScheme(scheme)

	// Create the updates ConfigMap with Renovate log entry format but no updates
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      UpdatesConfigMapName,
			Namespace: UpdatesConfigMapNamespace,
		},
		Data: map[string]string{
			"updates.json": `{"name":"renovate","level":20,"msg":"packageFiles with updates","config":{"flux":[]}}`,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cm).
		Build()

	r := &UpdatesConfigMapReconciler{
		Client:        client,
		Scheme:        scheme,
		StatusUpdater: renovate.NewStatusUpdater(client, UpdatesConfigMapNamespace, UpdatesConfigMapName),
	}

	ctx := context.Background()

	// Reconcile - should succeed with no updates
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      UpdatesConfigMapName,
			Namespace: UpdatesConfigMapNamespace,
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter > 0 {
		t.Error("expected no requeue")
	}
}
