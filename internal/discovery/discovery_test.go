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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

func setupTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()
}

func TestDiscoverPVCs_ExplicitList(t *testing.T) {
	// Create PVCs
	pvc1 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}
	pvc2 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-1",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	c := setupTestClient(t, pvc1, pvc2)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			VolumeSnapshots: &fluxupv1alpha1.VolumeSnapshotConfig{
				Enabled: true,
				PVCs: []fluxupv1alpha1.PVCRef{
					{Name: "data-0"},
					{Name: "data-1"},
				},
			},
		},
	}

	pvcs, err := d.DiscoverPVCs(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pvcs) != 2 {
		t.Errorf("expected 2 PVCs, got %d", len(pvcs))
	}

	// Verify PVC names
	names := make(map[string]bool)
	for _, pvc := range pvcs {
		names[pvc.Name] = true
	}
	if !names["data-0"] || !names["data-1"] {
		t.Errorf("expected PVCs data-0 and data-1, got %v", pvcs)
	}
}

func TestDiscoverPVCs_ExplicitList_PVCNotFound(t *testing.T) {
	c := setupTestClient(t) // No PVCs
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			VolumeSnapshots: &fluxupv1alpha1.VolumeSnapshotConfig{
				Enabled: true,
				PVCs: []fluxupv1alpha1.PVCRef{
					{Name: "nonexistent"},
				},
			},
		},
	}

	_, err := d.DiscoverPVCs(context.Background(), app)
	if err == nil {
		t.Fatal("expected error for nonexistent PVC")
	}
}

func TestFilterPVCs_RWOOnly(t *testing.T) {
	c := setupTestClient(t)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
	}

	pvcs := []PVCInfo{
		{Name: "rwo-pvc", Namespace: "default", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		{Name: "rwx-pvc", Namespace: "default", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}},
		{Name: "rox-pvc", Namespace: "default", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}},
	}

	filtered := d.filterPVCs(pvcs, app)

	if len(filtered) != 1 {
		t.Errorf("expected 1 RWO PVC, got %d", len(filtered))
	}
	if filtered[0].Name != "rwo-pvc" {
		t.Errorf("expected rwo-pvc, got %s", filtered[0].Name)
	}
}

func TestFilterPVCs_Exclusions(t *testing.T) {
	c := setupTestClient(t)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			VolumeSnapshots: &fluxupv1alpha1.VolumeSnapshotConfig{
				Enabled: true,
				ExcludePVCs: []fluxupv1alpha1.PVCRef{
					{Name: "cache-pvc"},
				},
			},
		},
	}

	pvcs := []PVCInfo{
		{Name: "data-pvc", Namespace: "default", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		{Name: "cache-pvc", Namespace: "default", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
	}

	filtered := d.filterPVCs(pvcs, app)

	if len(filtered) != 1 {
		t.Errorf("expected 1 PVC after exclusion, got %d", len(filtered))
	}
	if filtered[0].Name != "data-pvc" {
		t.Errorf("expected data-pvc, got %s", filtered[0].Name)
	}
}

func TestFilterPVCs_ExclusionWithNamespace(t *testing.T) {
	c := setupTestClient(t)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			VolumeSnapshots: &fluxupv1alpha1.VolumeSnapshotConfig{
				Enabled: true,
				ExcludePVCs: []fluxupv1alpha1.PVCRef{
					{Name: "cache-pvc", Namespace: "other"},
				},
			},
		},
	}

	pvcs := []PVCInfo{
		{Name: "cache-pvc", Namespace: "default", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		{Name: "cache-pvc", Namespace: "other", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
	}

	filtered := d.filterPVCs(pvcs, app)

	// Only exclude the one in "other" namespace
	if len(filtered) != 1 {
		t.Errorf("expected 1 PVC after exclusion, got %d", len(filtered))
	}
	if filtered[0].Namespace != "default" {
		t.Errorf("expected PVC from default namespace, got %s", filtered[0].Namespace)
	}
}

func TestDiscoverWorkloadsForPVCs(t *testing.T) {
	// Create a pod with PVC volume and owner reference to StatefulSet
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-sts-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind: "StatefulSet",
					Name: "my-sts",
				},
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

	c := setupTestClient(t, pod)
	d := New(c)

	workloads, err := d.DiscoverWorkloadsForPVCs(context.Background(), []string{"data-my-sts-0"}, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(workloads) != 1 {
		t.Errorf("expected 1 workload, got %d", len(workloads))
	}
	if workloads[0].Kind != "StatefulSet" {
		t.Errorf("expected StatefulSet, got %s", workloads[0].Kind)
	}
	if workloads[0].Name != "my-sts" {
		t.Errorf("expected my-sts, got %s", workloads[0].Name)
	}
}

func TestDiscoverWorkloadsForPVCs_Deployment(t *testing.T) {
	// Create a pod with PVC volume and owner reference to ReplicaSet
	// (Deployments own ReplicaSets which own Pods)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-deploy-abc123-xyz",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind: "ReplicaSet",
					Name: "my-deploy-abc123",
				},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "data-pvc",
						},
					},
				},
			},
		},
	}

	c := setupTestClient(t, pod)
	d := New(c)

	workloads, err := d.DiscoverWorkloadsForPVCs(context.Background(), []string{"data-pvc"}, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(workloads) != 1 {
		t.Errorf("expected 1 workload, got %d", len(workloads))
	}
	if workloads[0].Kind != "Deployment" {
		t.Errorf("expected Deployment, got %s", workloads[0].Kind)
	}
	if workloads[0].Name != "my-deploy" {
		t.Errorf("expected my-deploy, got %s", workloads[0].Name)
	}
}

func TestDiscoverWorkloadsForPVCs_NoPods(t *testing.T) {
	c := setupTestClient(t) // No pods
	d := New(c)

	workloads, err := d.DiscoverWorkloadsForPVCs(context.Background(), []string{"data-pvc"}, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(workloads) != 0 {
		t.Errorf("expected 0 workloads, got %d", len(workloads))
	}
}

func TestDiscoverWorkloadsForPVCs_MultipleWorkloads(t *testing.T) {
	// Create pods from different workloads mounting the same PVC
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sts-0",
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
							ClaimName: "shared-pvc",
						},
					},
				},
			},
		},
	}

	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deploy-abc123-xyz",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-deploy-abc123"},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "shared-pvc",
						},
					},
				},
			},
		},
	}

	c := setupTestClient(t, pod1, pod2)
	d := New(c)

	workloads, err := d.DiscoverWorkloadsForPVCs(context.Background(), []string{"shared-pvc"}, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(workloads) != 2 {
		t.Errorf("expected 2 workloads, got %d", len(workloads))
	}

	// Check both workload types are present
	kinds := make(map[string]bool)
	for _, w := range workloads {
		kinds[w.Kind] = true
	}
	if !kinds["StatefulSet"] || !kinds["Deployment"] {
		t.Errorf("expected both StatefulSet and Deployment, got %v", workloads)
	}
}

func TestDiscoverWorkloadsForPVCs_DuplicateWorkload(t *testing.T) {
	// Multiple pods from the same StatefulSet should result in one workload
	pod1 := &corev1.Pod{
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
							ClaimName: "data-0",
						},
					},
				},
			},
		},
	}

	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-sts-1",
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
							ClaimName: "data-1",
						},
					},
				},
			},
		},
	}

	c := setupTestClient(t, pod1, pod2)
	d := New(c)

	workloads, err := d.DiscoverWorkloadsForPVCs(context.Background(), []string{"data-0", "data-1"}, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should deduplicate to single StatefulSet
	if len(workloads) != 1 {
		t.Errorf("expected 1 workload (deduplicated), got %d", len(workloads))
	}
	if workloads[0].Name != "my-sts" {
		t.Errorf("expected my-sts, got %s", workloads[0].Name)
	}
}

func TestFindDeploymentForReplicaSet(t *testing.T) {
	tests := []struct {
		rsName     string
		wantDeploy string
	}{
		{"my-deploy-abc123def", "my-deploy"},
		{"frontend-7d5b89f4cb", "frontend"},
		{"app-with-dashes-abc123", "app-with-dashes"},
		{"nodash", ""},           // No hash suffix
		{"short-ab", ""},         // Hash too short
		{"abc-UPPERCASE123", ""}, // Uppercase in hash
		{"abc-123!@#", ""},       // Special chars in hash
	}

	for _, tt := range tests {
		t.Run(tt.rsName, func(t *testing.T) {
			got := findDeploymentForReplicaSet(tt.rsName)
			if got != tt.wantDeploy {
				t.Errorf("findDeploymentForReplicaSet(%q) = %q, want %q", tt.rsName, got, tt.wantDeploy)
			}
		})
	}
}
