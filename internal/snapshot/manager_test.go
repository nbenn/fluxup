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

package snapshot

import (
	"context"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

func TestManager_CreateSnapshot(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pvc",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pvc).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	req := SnapshotRequest{
		PVCName:                 "test-pvc",
		PVCNamespace:            "default",
		SnapshotName:            "test-snapshot",
		VolumeSnapshotClassName: "csi-snapclass",
		Labels: map[string]string{
			"app": "test",
		},
	}

	snapshot, err := manager.CreateSnapshot(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if snapshot.Name != "test-snapshot" {
		t.Errorf("expected snapshot name test-snapshot, got %s", snapshot.Name)
	}

	// Verify snapshot was created
	var result snapshotv1.VolumeSnapshot
	err = client.Get(ctx, types.NamespacedName{Name: "test-snapshot", Namespace: "default"}, &result)
	if err != nil {
		t.Fatalf("failed to get snapshot: %v", err)
	}

	if *result.Spec.Source.PersistentVolumeClaimName != "test-pvc" {
		t.Errorf("expected PVC name test-pvc, got %s", *result.Spec.Source.PersistentVolumeClaimName)
	}

	if result.Labels["app"] != "test" {
		t.Errorf("expected label app=test, got %s", result.Labels["app"])
	}
}

func TestManager_CreateSnapshot_PVCNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	req := SnapshotRequest{
		PVCName:      "nonexistent-pvc",
		PVCNamespace: "default",
		SnapshotName: "test-snapshot",
	}

	_, err := manager.CreateSnapshot(ctx, req)
	if err == nil {
		t.Error("expected error for nonexistent PVC")
	}
}

func TestManager_IsSnapshotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = snapshotv1.AddToScheme(scheme)

	tests := []struct {
		name       string
		readyToUse *bool
		expected   bool
	}{
		{
			name:       "ready",
			readyToUse: boolPtr(true),
			expected:   true,
		},
		{
			name:       "not ready",
			readyToUse: boolPtr(false),
			expected:   false,
		},
		{
			name:       "nil status",
			readyToUse: nil,
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-snapshot",
					Namespace: "default",
				},
			}

			if tt.readyToUse != nil {
				snapshot.Status = &snapshotv1.VolumeSnapshotStatus{
					ReadyToUse: tt.readyToUse,
				}
			}

			client := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(snapshot).
				Build()

			manager := NewManager(client)
			ctx := context.Background()

			ready, err := manager.IsSnapshotReady(ctx, "test-snapshot", "default")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if ready != tt.expected {
				t.Errorf("IsSnapshotReady() = %v, want %v", ready, tt.expected)
			}
		})
	}
}

func TestManager_CreateSnapshotsForUpgrade(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)
	_ = fluxupv1alpha1.AddToScheme(scheme)

	pvc1 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-app-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	pvc2 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-db-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pvc1, pvc2).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	pvcs := []fluxupv1alpha1.PVCRef{
		{Name: "data-app-0"},
		{Name: "data-db-0"},
	}

	snapshots, err := manager.CreateSnapshotsForUpgrade(ctx, "my-app", "default", pvcs, "csi-snapclass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snapshots) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(snapshots))
	}

	// Verify snapshots have correct labels
	for _, snap := range snapshots {
		var result snapshotv1.VolumeSnapshot
		err = client.Get(ctx, types.NamespacedName{Name: snap.SnapshotName, Namespace: "default"}, &result)
		if err != nil {
			t.Fatalf("failed to get snapshot %s: %v", snap.SnapshotName, err)
		}

		if result.Labels["fluxup.dev/managed-app"] != "my-app" {
			t.Errorf("expected label fluxup.dev/managed-app=my-app")
		}

		if result.Labels["fluxup.dev/purpose"] != "pre-upgrade" {
			t.Errorf("expected label fluxup.dev/purpose=pre-upgrade")
		}
	}
}

func TestManager_DeleteSnapshot(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = snapshotv1.AddToScheme(scheme)

	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(snapshot).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	err := manager.DeleteSnapshot(ctx, "test-snapshot", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify snapshot was deleted
	var result snapshotv1.VolumeSnapshot
	err = client.Get(ctx, types.NamespacedName{Name: "test-snapshot", Namespace: "default"}, &result)
	if err == nil {
		t.Error("expected snapshot to be deleted")
	}
}

func boolPtr(b bool) *bool {
	return &b
}
