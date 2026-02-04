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

package health

import (
	"context"
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

func setupTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = kustomizev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()
}

func TestCheckHealth_FluxNotReady(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:    "Ready",
					Status:  metav1.ConditionFalse,
					Reason:  "ReconciliationFailed",
					Message: "manifest error",
				},
			},
		},
	}

	c := setupTestClient(t, ks)
	checker := NewChecker(c)

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

	result, err := checker.CheckHealth(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Healthy {
		t.Error("expected unhealthy when Flux is not ready")
	}
	if result.FluxReady {
		t.Error("expected FluxReady=false")
	}
}

func TestCheckHealth_FluxReady_NoWorkloads(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
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

	c := setupTestClient(t, ks)
	checker := NewChecker(c)

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

	result, err := checker.CheckHealth(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Healthy {
		t.Error("expected healthy when Flux is ready and no workloads")
	}
	if !result.FluxReady {
		t.Error("expected FluxReady=true")
	}
	if !result.WorkloadsReady {
		t.Error("expected WorkloadsReady=true (no workloads)")
	}
}

func TestCheckHealth_DeploymentReady(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
			},
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{ID: "default_web_apps_Deployment"},
				},
			},
		},
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(3)),
		},
		Status: appsv1.DeploymentStatus{
			Replicas:      3,
			ReadyReplicas: 3,
		},
	}

	// Create a pod so workload discovery works
	pod := &corev1.Pod{
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
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "web-data",
						},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-data",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	c := setupTestClient(t, ks, deploy, pod, pvc)
	checker := NewChecker(c)

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

	result, err := checker.CheckHealth(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Healthy {
		t.Errorf("expected healthy, got message: %s", result.Message)
	}
	if !result.FluxReady {
		t.Error("expected FluxReady=true")
	}
	if !result.WorkloadsReady {
		t.Error("expected WorkloadsReady=true")
	}
}

func TestCheckHealth_DeploymentNotReady(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
			},
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{ID: "default_web_apps_Deployment"},
				},
			},
		},
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(3)),
		},
		Status: appsv1.DeploymentStatus{
			Replicas:      3,
			ReadyReplicas: 1, // Only 1 of 3 ready
		},
	}

	pod := &corev1.Pod{
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
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "web-data",
						},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-data",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	c := setupTestClient(t, ks, deploy, pod, pvc)
	checker := NewChecker(c)

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

	result, err := checker.CheckHealth(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Healthy {
		t.Error("expected unhealthy when deployment has insufficient replicas")
	}
	if result.FluxReady != true {
		t.Error("expected FluxReady=true")
	}
	if result.WorkloadsReady {
		t.Error("expected WorkloadsReady=false")
	}
}

func TestCheckHealth_StatefulSetReady(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
			},
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{ID: "default_db_apps_StatefulSet"},
				},
			},
		},
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(3)),
		},
		Status: appsv1.StatefulSetStatus{
			Replicas:      3,
			ReadyReplicas: 3,
		},
	}

	pod := &corev1.Pod{
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

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-db-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	c := setupTestClient(t, ks, sts, pod, pvc)
	checker := NewChecker(c)

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

	result, err := checker.CheckHealth(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Healthy {
		t.Errorf("expected healthy, got message: %s", result.Message)
	}
}

func TestCheckHealth_ScaledToZero(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
			},
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{ID: "default_web_apps_Deployment"},
				},
			},
		},
	}

	// Deployment scaled to 0 - should be considered healthy
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(0)),
		},
		Status: appsv1.DeploymentStatus{
			Replicas:      0,
			ReadyReplicas: 0,
		},
	}

	// Pod that used to mount a PVC but is now gone
	// We still need a way to discover the workload - use inventory directly

	c := setupTestClient(t, ks, deploy)
	checker := NewChecker(c)

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

	result, err := checker.CheckHealth(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No workloads discovered (no pods), so it should be healthy
	if !result.Healthy {
		t.Errorf("expected healthy, got message: %s", result.Message)
	}
}

func TestCheckHealth_KustomizationNotFound(t *testing.T) {
	c := setupTestClient(t) // No Kustomization
	checker := NewChecker(c)

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

	_, err := checker.CheckHealth(context.Background(), app)
	if err == nil {
		t.Fatal("expected error for nonexistent Kustomization")
	}
}

func TestCheckHealth_NoReadyCondition(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		// No conditions
	}

	c := setupTestClient(t, ks)
	checker := NewChecker(c)

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

	result, err := checker.CheckHealth(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Healthy {
		t.Error("expected unhealthy when Kustomization has no Ready condition")
	}
	if result.FluxReady {
		t.Error("expected FluxReady=false when no Ready condition")
	}
}

func TestIsHealthy(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
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

	c := setupTestClient(t, ks)
	checker := NewChecker(c)

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

	healthy, err := checker.IsHealthy(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !healthy {
		t.Error("expected IsHealthy to return true")
	}
}

func TestCheckWorkloadReady_UnsupportedKind(t *testing.T) {
	c := setupTestClient(t)
	checker := NewChecker(c)

	_, err := checker.checkWorkloadReady(context.Background(), "DaemonSet", "test", "default")
	if err == nil {
		t.Fatal("expected error for unsupported workload kind")
	}
}

func TestCheckHealth_DefaultFluxNamespace(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system", // Default namespace
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

	c := setupTestClient(t, ks)
	checker := NewChecker(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			KustomizationRef: fluxupv1alpha1.ObjectReference{
				Name: "apps",
				// No namespace specified - should default to flux-system
			},
		},
	}

	result, err := checker.CheckHealth(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Healthy {
		t.Errorf("expected healthy, got message: %s", result.Message)
	}
}

func TestCheckHealth_WorkloadStatuses(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps",
			Namespace: "flux-system",
		},
		Status: kustomizev1.KustomizationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "ReconciliationSucceeded",
				},
			},
			Inventory: &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{ID: "default_web_apps_Deployment"},
					{ID: "default_db_apps_StatefulSet"},
				},
			},
		},
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(2)),
		},
		Status: appsv1.DeploymentStatus{
			Replicas:      2,
			ReadyReplicas: 2,
		},
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(1)),
		},
		Status: appsv1.StatefulSetStatus{
			Replicas:      1,
			ReadyReplicas: 0, // Not ready
		},
	}

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
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "web-data",
						},
					},
				},
			},
		},
	}

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
		ObjectMeta: metav1.ObjectMeta{Name: "web-data", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
	}

	dbPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
	}

	c := setupTestClient(t, ks, deploy, sts, webPod, dbPod, webPVC, dbPVC)
	checker := NewChecker(c)

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

	result, err := checker.CheckHealth(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Healthy {
		t.Error("expected unhealthy when one workload is not ready")
	}

	// Should have 2 workload statuses
	if len(result.WorkloadStatuses) != 2 {
		t.Errorf("expected 2 workload statuses, got %d", len(result.WorkloadStatuses))
	}

	// Find the statuses
	var deployStatus, stsStatus *WorkloadStatus
	for i := range result.WorkloadStatuses {
		switch result.WorkloadStatuses[i].Kind {
		case "Deployment":
			deployStatus = &result.WorkloadStatuses[i]
		case "StatefulSet":
			stsStatus = &result.WorkloadStatuses[i]
		}
	}

	if deployStatus == nil || !deployStatus.Ready {
		t.Error("expected Deployment to be ready")
	}
	if stsStatus == nil || stsStatus.Ready {
		t.Error("expected StatefulSet to not be ready")
	}
}
