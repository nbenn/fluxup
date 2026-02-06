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
	"sort"
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

// setupEquivalenceTestClient creates a test client with all necessary schemes
// for testing both HelmRelease and Kustomization discovery.
func setupEquivalenceTestClient(t *testing.T, objects ...client.Object) client.Client {
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

// TestDiscovery_EquivalentPVCResults verifies that both HelmRelease and Kustomization
// discovery methods return equivalent PVC information when discovering the same resources.
//
// This test is critical because users may deploy the same application via either:
// 1. A HelmRelease that renders a StatefulSet with PVCs, or
// 2. A Kustomization that manages raw StatefulSet manifests
//
// Both discovery methods should find the same PVCs.
func TestDiscovery_EquivalentPVCResults(t *testing.T) {
	ctx := context.Background()

	// Create the "deployed" resources that both discovery methods will find
	// This simulates: StatefulSet "redis" with a PVC "data-redis-0"

	// Pod owned by StatefulSet (common to both)
	redisPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "redis"},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "data-redis-0",
						},
					},
				},
			},
		},
	}

	// PVC attached to the pod (common to both)
	redisPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-redis-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	// === HelmRelease discovery setup ===
	// HelmRelease stores the rendered manifest in a secret
	helmManifest := `---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: redis
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis
  template:
    spec:
      containers:
      - name: redis
        image: redis:7.2.0
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 1Gi`

	helmSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.redis.v1",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"release": encodeHelmRelease(helmManifest),
		},
	}

	// === Kustomization discovery setup ===
	// Kustomization stores workload references in its inventory
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{ID: "default_redis_apps_StatefulSet"},
				},
			},
		},
	}

	// Create test client with all resources
	c := setupEquivalenceTestClient(t, helmSecret, kustomization, redisPod, redisPVC)
	d := New(c)

	// === Test HelmRelease discovery ===
	helmApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-helm",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			HelmReleaseRef: &fluxupv1alpha1.ObjectReference{
				Name:      "redis",
				Namespace: "default",
			},
		},
	}

	helmPVCs, err := d.discoverPVCsFromHelmRelease(ctx, helmApp)
	if err != nil {
		t.Fatalf("HelmRelease PVC discovery failed: %v", err)
	}

	// === Test Kustomization discovery ===
	kustomizationApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-kustomization",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	ksPVCs, err := d.discoverPVCsFromKustomization(ctx, kustomizationApp)
	if err != nil {
		t.Fatalf("Kustomization PVC discovery failed: %v", err)
	}

	// === Verify equivalence ===
	if len(helmPVCs) != len(ksPVCs) {
		t.Errorf("PVC count mismatch: HelmRelease found %d, Kustomization found %d",
			len(helmPVCs), len(ksPVCs))
	}

	// Sort for comparison
	sortPVCsByName(helmPVCs)
	sortPVCsByName(ksPVCs)

	for i := range helmPVCs {
		if i >= len(ksPVCs) {
			break
		}
		if helmPVCs[i].Name != ksPVCs[i].Name {
			t.Errorf("PVC name mismatch at index %d: HelmRelease=%s, Kustomization=%s",
				i, helmPVCs[i].Name, ksPVCs[i].Name)
		}
		if helmPVCs[i].Namespace != ksPVCs[i].Namespace {
			t.Errorf("PVC namespace mismatch at index %d: HelmRelease=%s, Kustomization=%s",
				i, helmPVCs[i].Namespace, ksPVCs[i].Namespace)
		}
	}
}

// TestDiscovery_EquivalentWorkloadResults verifies that both discovery methods
// return equivalent workload information for the same deployed resources.
func TestDiscovery_EquivalentWorkloadResults(t *testing.T) {
	ctx := context.Background()

	// Create pods with PVCs for workload discovery
	deployPod := &corev1.Pod{
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
					Name: "cache",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "web-cache",
						},
					},
				},
			},
		},
	}

	stsPod := &corev1.Pod{
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

	webCachePVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-cache",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	dbDataPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-db-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	// HelmRelease manifest
	helmManifest := `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  replicas: 2
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
  namespace: default
spec:
  replicas: 1`

	helmSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.myapp.v1",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"release": encodeHelmRelease(helmManifest),
		},
	}

	// Kustomization inventory
	kustomization := &kustomizev1.Kustomization{
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

	c := setupEquivalenceTestClient(t, helmSecret, kustomization, deployPod, stsPod, webCachePVC, dbDataPVC)
	d := New(c)

	// HelmRelease workload discovery
	helmApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-helm",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			HelmReleaseRef: &fluxupv1alpha1.ObjectReference{
				Name:      "myapp",
				Namespace: "default",
			},
		},
	}

	helmWorkloads, err := d.DiscoverWorkloads(ctx, helmApp)
	if err != nil {
		t.Fatalf("HelmRelease workload discovery failed: %v", err)
	}

	// Kustomization workload discovery
	ksApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-kustomization",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "apps",
				Namespace: "flux-system",
			},
		},
	}

	ksWorkloads, err := d.DiscoverWorkloads(ctx, ksApp)
	if err != nil {
		t.Fatalf("Kustomization workload discovery failed: %v", err)
	}

	// Verify equivalence
	if len(helmWorkloads) != len(ksWorkloads) {
		t.Errorf("Workload count mismatch: HelmRelease found %d, Kustomization found %d",
			len(helmWorkloads), len(ksWorkloads))
	}

	// Sort for comparison
	sortWorkloadsByName(helmWorkloads)
	sortWorkloadsByName(ksWorkloads)

	for i := range helmWorkloads {
		if i >= len(ksWorkloads) {
			break
		}
		if helmWorkloads[i].Name != ksWorkloads[i].Name {
			t.Errorf("Workload name mismatch at index %d: HelmRelease=%s, Kustomization=%s",
				i, helmWorkloads[i].Name, ksWorkloads[i].Name)
		}
		if helmWorkloads[i].Kind != ksWorkloads[i].Kind {
			t.Errorf("Workload kind mismatch at index %d: HelmRelease=%s, Kustomization=%s",
				i, helmWorkloads[i].Kind, ksWorkloads[i].Kind)
		}
	}
}

// TestDiscovery_MultiplePVCsEquivalence tests that both discovery methods
// correctly handle multiple PVCs attached to different workloads.
func TestDiscovery_MultiplePVCsEquivalence(t *testing.T) {
	ctx := context.Background()

	// First StatefulSet pod with PVC
	redisPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "redis"},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "data-redis-0",
						},
					},
				},
			},
		},
	}

	// Second StatefulSet pod with PVC
	postgresPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "postgres-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "postgres"},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "data-postgres-0",
						},
					},
				},
			},
		},
	}

	redisPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-redis-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	postgresPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-postgres-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	// HelmRelease manifest with both StatefulSets
	helmManifest := `---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: redis
  namespace: default
spec:
  replicas: 1
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: postgres
  namespace: default
spec:
  replicas: 1`

	helmSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.db-stack.v1",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"release": encodeHelmRelease(helmManifest),
		},
	}

	// Kustomization with both StatefulSets in inventory
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-stack",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{ID: "default_redis_apps_StatefulSet"},
					{ID: "default_postgres_apps_StatefulSet"},
				},
			},
		},
	}

	c := setupEquivalenceTestClient(t, helmSecret, kustomization, redisPod, postgresPod, redisPVC, postgresPVC)
	d := New(c)

	// HelmRelease PVC discovery
	helmApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-stack-helm",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			HelmReleaseRef: &fluxupv1alpha1.ObjectReference{
				Name:      "db-stack",
				Namespace: "default",
			},
		},
	}

	helmPVCs, err := d.discoverPVCsFromHelmRelease(ctx, helmApp)
	if err != nil {
		t.Fatalf("HelmRelease PVC discovery failed: %v", err)
	}

	// Kustomization PVC discovery
	ksApp := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-stack-kustomization",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name:      "db-stack",
				Namespace: "flux-system",
			},
		},
	}

	ksPVCs, err := d.discoverPVCsFromKustomization(ctx, ksApp)
	if err != nil {
		t.Fatalf("Kustomization PVC discovery failed: %v", err)
	}

	// Both should find 2 PVCs
	if len(helmPVCs) != 2 {
		t.Errorf("HelmRelease should find 2 PVCs, got %d", len(helmPVCs))
	}
	if len(ksPVCs) != 2 {
		t.Errorf("Kustomization should find 2 PVCs, got %d", len(ksPVCs))
	}

	// Sort and compare
	sortPVCsByName(helmPVCs)
	sortPVCsByName(ksPVCs)

	helmNames := make([]string, len(helmPVCs))
	for i, pvc := range helmPVCs {
		helmNames[i] = pvc.Name
	}

	ksNames := make([]string, len(ksPVCs))
	for i, pvc := range ksPVCs {
		ksNames[i] = pvc.Name
	}

	if len(helmNames) != len(ksNames) {
		t.Errorf("PVC name count mismatch: Helm=%v, Ks=%v", helmNames, ksNames)
	}

	for i := range helmNames {
		if i < len(ksNames) && helmNames[i] != ksNames[i] {
			t.Errorf("PVC name mismatch at %d: Helm=%s, Ks=%s", i, helmNames[i], ksNames[i])
		}
	}
}

// Helper functions for sorting

func sortPVCsByName(pvcs []PVCInfo) {
	sort.Slice(pvcs, func(i, j int) bool {
		return pvcs[i].Name < pvcs[j].Name
	})
}

func sortWorkloadsByName(workloads []WorkloadInfo) {
	sort.Slice(workloads, func(i, j int) bool {
		return workloads[i].Name < workloads[j].Name
	})
}
