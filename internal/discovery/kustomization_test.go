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

package discovery

import (
	"context"
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

func setupKustomizationTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = kustomizev1.AddToScheme(scheme)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()
}

func TestExtractWorkloadsFromInventory(t *testing.T) {
	c := setupKustomizationTestClient(t)
	d := New(c)

	entries := []kustomizev1.ResourceRef{
		{ID: "default_web_apps_Deployment"},
		{ID: "default_db_apps_StatefulSet"},
		{ID: "default_config_core_ConfigMap"},
		{ID: "default_web-svc_core_Service"},
		{ID: "other-ns_cache_apps_Deployment"},
	}

	workloads := d.extractWorkloadsFromInventory(entries, "default")

	if len(workloads) != 3 {
		t.Errorf("expected 3 workloads (2 Deployments + 1 StatefulSet), got %d", len(workloads))
	}

	// Check workload details
	found := make(map[string]WorkloadInfo)
	for _, w := range workloads {
		found[w.Namespace+"/"+w.Name] = w
	}

	if w, ok := found["default/web"]; !ok || w.Kind != kindDeployment {
		t.Errorf("expected Deployment 'default/web', got %v", found)
	}
	if w, ok := found["default/db"]; !ok || w.Kind != kindStatefulSet {
		t.Errorf("expected StatefulSet 'default/db', got %v", found)
	}
	if w, ok := found["other-ns/cache"]; !ok || w.Kind != kindDeployment {
		t.Errorf("expected Deployment 'other-ns/cache', got %v", found)
	}
}

func TestExtractWorkloadsFromInventory_InvalidFormat(t *testing.T) {
	c := setupKustomizationTestClient(t)
	d := New(c)

	entries := []kustomizev1.ResourceRef{
		{ID: "invalid_format"},           // Too few parts
		{ID: "a_b_c"},                    // Still too few
		{ID: "ns_name_group_Deployment"}, // Valid
	}

	workloads := d.extractWorkloadsFromInventory(entries, "default")

	if len(workloads) != 1 {
		t.Errorf("expected 1 valid workload, got %d", len(workloads))
	}
}

func TestDiscoverPVCsFromKustomization(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{ID: "default_my-sts_apps_StatefulSet"},
				},
			},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-sts-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "my-sts"},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "data-my-sts-0",
						},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-my-sts-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	c := setupKustomizationTestClient(t, ks, pod, pvc)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	pvcs, err := d.discoverPVCsFromKustomization(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pvcs) != 1 {
		t.Errorf("expected 1 PVC, got %d", len(pvcs))
	}
	if pvcs[0].Name != "data-my-sts-0" {
		t.Errorf("expected PVC 'data-my-sts-0', got %s", pvcs[0].Name)
	}
}

func TestDiscoverPVCsFromKustomization_NoInventory(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		// No inventory
	}

	c := setupKustomizationTestClient(t, ks)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	pvcs, err := d.discoverPVCsFromKustomization(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pvcs) != 0 {
		t.Errorf("expected 0 PVCs for empty inventory, got %d", len(pvcs))
	}
}

func TestDiscoverPVCsFromKustomization_KustomizationNotFound(t *testing.T) {
	c := setupKustomizationTestClient(t) // No Kustomization
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "nonexistent",
				Namespace: "flux-system",
			},
		},
	}

	_, err := d.discoverPVCsFromKustomization(context.Background(), app)
	if err == nil {
		t.Fatal("expected error for nonexistent Kustomization")
	}
}

func TestDiscoverPVCsFromKustomization_DefaultNamespace(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system", // Default namespace when not specified
		},
		Status: kustomizev1.KustomizationStatus{
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{},
			},
		},
	}

	c := setupKustomizationTestClient(t, ks)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name: "apps",
				// Namespace not specified - should default to flux-system
			},
		},
	}

	// Should not error - uses default flux-system namespace
	_, err := d.discoverPVCsFromKustomization(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDiscoverPVCsFromKustomization_MultipleWorkloads(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{ID: "default_web_apps_Deployment"},
					{ID: "default_db_apps_StatefulSet"},
				},
			},
		},
	}

	// Web deployment pod
	webPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc123-xyz",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "web-abc123"},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "logs",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "web-logs",
						},
					},
				},
			},
		},
	}

	// DB statefulset pod
	dbPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "db"},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "data-db-0",
						},
					},
				},
			},
		},
	}

	webPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-logs",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	dbPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-db-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	c := setupKustomizationTestClient(t, ks, webPod, dbPod, webPVC, dbPVC)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	pvcs, err := d.discoverPVCsFromKustomization(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pvcs) != 2 {
		t.Errorf("expected 2 PVCs, got %d", len(pvcs))
	}

	// Check both PVCs are found
	names := make(map[string]bool)
	for _, pvc := range pvcs {
		names[pvc.Name] = true
	}
	if !names["web-logs"] || !names["data-db-0"] {
		t.Errorf("expected PVCs 'web-logs' and 'data-db-0', got %v", pvcs)
	}
}
