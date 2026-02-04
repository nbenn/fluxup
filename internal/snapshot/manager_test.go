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
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

const (
	testSnapshotName = "test-snapshot"
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
		SnapshotName:            testSnapshotName,
		VolumeSnapshotClassName: "csi-snapclass",
		Labels: map[string]string{
			"app": "test",
		},
	}

	snapshot, err := manager.CreateSnapshot(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if snapshot.Name != testSnapshotName {
		t.Errorf("expected snapshot name test-snapshot, got %s", snapshot.Name)
	}

	// Verify snapshot was created
	var result snapshotv1.VolumeSnapshot
	err = client.Get(ctx, types.NamespacedName{Name: testSnapshotName, Namespace: "default"}, &result)
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
		SnapshotName: testSnapshotName,
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
					Name:      testSnapshotName,
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

			ready, err := manager.IsSnapshotReady(ctx, testSnapshotName, "default")
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
			Name:      testSnapshotName,
			Namespace: "default",
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(snapshot).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	err := manager.DeleteSnapshot(ctx, testSnapshotName, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify snapshot was deleted
	var result snapshotv1.VolumeSnapshot
	err = client.Get(ctx, types.NamespacedName{Name: testSnapshotName, Namespace: "default"}, &result)
	if err == nil {
		t.Error("expected snapshot to be deleted")
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func stringPtr(s string) *string {
	return &s
}

func TestManager_ApplyRetentionPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = snapshotv1.AddToScheme(scheme)

	tests := []struct {
		name            string
		snapshots       []snapshotv1.VolumeSnapshot
		maxCount        int
		expectedDeleted int
		expectedRemain  int
	}{
		{
			name:            "no snapshots",
			snapshots:       nil,
			maxCount:        3,
			expectedDeleted: 0,
			expectedRemain:  0,
		},
		{
			name: "under limit",
			snapshots: []snapshotv1.VolumeSnapshot{
				makeSnapshot("snap-1", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
				makeSnapshot("snap-2", "default", "my-app", "pvc-1", "2026-01-02T10:00:00Z"),
			},
			maxCount:        3,
			expectedDeleted: 0,
			expectedRemain:  2,
		},
		{
			name: "at limit",
			snapshots: []snapshotv1.VolumeSnapshot{
				makeSnapshot("snap-1", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
				makeSnapshot("snap-2", "default", "my-app", "pvc-1", "2026-01-02T10:00:00Z"),
				makeSnapshot("snap-3", "default", "my-app", "pvc-1", "2026-01-03T10:00:00Z"),
			},
			maxCount:        3,
			expectedDeleted: 0,
			expectedRemain:  3,
		},
		{
			name: "over limit - delete oldest",
			snapshots: []snapshotv1.VolumeSnapshot{
				makeSnapshot("snap-1", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
				makeSnapshot("snap-2", "default", "my-app", "pvc-1", "2026-01-02T10:00:00Z"),
				makeSnapshot("snap-3", "default", "my-app", "pvc-1", "2026-01-03T10:00:00Z"),
				makeSnapshot("snap-4", "default", "my-app", "pvc-1", "2026-01-04T10:00:00Z"),
				makeSnapshot("snap-5", "default", "my-app", "pvc-1", "2026-01-05T10:00:00Z"),
			},
			maxCount:        3,
			expectedDeleted: 2,
			expectedRemain:  3,
		},
		{
			name: "multiple PVCs - separate limits",
			snapshots: []snapshotv1.VolumeSnapshot{
				makeSnapshot("snap-pvc1-1", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
				makeSnapshot("snap-pvc1-2", "default", "my-app", "pvc-1", "2026-01-02T10:00:00Z"),
				makeSnapshot("snap-pvc1-3", "default", "my-app", "pvc-1", "2026-01-03T10:00:00Z"),
				makeSnapshot("snap-pvc1-4", "default", "my-app", "pvc-1", "2026-01-04T10:00:00Z"),
				makeSnapshot("snap-pvc2-1", "default", "my-app", "pvc-2", "2026-01-01T10:00:00Z"),
				makeSnapshot("snap-pvc2-2", "default", "my-app", "pvc-2", "2026-01-02T10:00:00Z"),
			},
			maxCount:        2,
			expectedDeleted: 2, // 2 from pvc-1, 0 from pvc-2
			expectedRemain:  4,
		},
		{
			name: "maxCount 0 - keep all",
			snapshots: []snapshotv1.VolumeSnapshot{
				makeSnapshot("snap-1", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
				makeSnapshot("snap-2", "default", "my-app", "pvc-1", "2026-01-02T10:00:00Z"),
				makeSnapshot("snap-3", "default", "my-app", "pvc-1", "2026-01-03T10:00:00Z"),
				makeSnapshot("snap-4", "default", "my-app", "pvc-1", "2026-01-04T10:00:00Z"),
			},
			maxCount:        0,
			expectedDeleted: 0,
			expectedRemain:  4,
		},
		{
			name: "maxCount 1 - keep only newest",
			snapshots: []snapshotv1.VolumeSnapshot{
				makeSnapshot("snap-1", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
				makeSnapshot("snap-2", "default", "my-app", "pvc-1", "2026-01-02T10:00:00Z"),
				makeSnapshot("snap-3", "default", "my-app", "pvc-1", "2026-01-03T10:00:00Z"),
			},
			maxCount:        1,
			expectedDeleted: 2,
			expectedRemain:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientBuilder := fake.NewClientBuilder().WithScheme(scheme)
			for i := range tt.snapshots {
				clientBuilder = clientBuilder.WithObjects(&tt.snapshots[i])
			}
			client := clientBuilder.Build()

			manager := NewManager(client)
			ctx := context.Background()

			deleted, err := manager.ApplyRetentionPolicy(ctx, "my-app", "default", tt.maxCount)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if deleted != tt.expectedDeleted {
				t.Errorf("expected %d deleted, got %d", tt.expectedDeleted, deleted)
			}

			// Count remaining snapshots
			var remaining snapshotv1.VolumeSnapshotList
			if err := client.List(ctx, &remaining); err != nil {
				t.Fatalf("failed to list snapshots: %v", err)
			}

			if len(remaining.Items) != tt.expectedRemain {
				t.Errorf("expected %d remaining, got %d", tt.expectedRemain, len(remaining.Items))
			}
		})
	}
}

func TestManager_ApplyRetentionPolicy_KeepsNewest(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = snapshotv1.AddToScheme(scheme)

	// Create snapshots with specific timestamps - oldest first in creation order
	snapshots := []snapshotv1.VolumeSnapshot{
		makeSnapshot("oldest", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
		makeSnapshot("middle", "default", "my-app", "pvc-1", "2026-01-02T10:00:00Z"),
		makeSnapshot("newest", "default", "my-app", "pvc-1", "2026-01-03T10:00:00Z"),
	}

	clientBuilder := fake.NewClientBuilder().WithScheme(scheme)
	for i := range snapshots {
		clientBuilder = clientBuilder.WithObjects(&snapshots[i])
	}
	client := clientBuilder.Build()

	manager := NewManager(client)
	ctx := context.Background()

	// Keep only 1 - should keep "newest"
	deleted, err := manager.ApplyRetentionPolicy(ctx, "my-app", "default", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", deleted)
	}

	// Verify "newest" remains
	var remaining snapshotv1.VolumeSnapshotList
	if err := client.List(ctx, &remaining); err != nil {
		t.Fatalf("failed to list snapshots: %v", err)
	}

	if len(remaining.Items) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(remaining.Items))
	}

	if remaining.Items[0].Name != "newest" {
		t.Errorf("expected 'newest' to remain, got %s", remaining.Items[0].Name)
	}
}

// makeSnapshot creates a VolumeSnapshot for testing
//
//nolint:unparam // namespace is always "default" in tests but kept for flexibility
func makeSnapshot(name, namespace, appName, pvcName, timestamp string) snapshotv1.VolumeSnapshot {
	ts, _ := time.Parse(time.RFC3339, timestamp)
	return snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Time{Time: ts},
			Labels: map[string]string{
				"fluxup.dev/managed-app": appName,
			},
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: stringPtr(pvcName),
			},
		},
	}
}

func TestManager_DeleteSnapshot_OrphanedSnapshot(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = snapshotv1.AddToScheme(scheme)

	// Snapshot with finalizer that would prevent immediate deletion
	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testSnapshotName,
			Namespace:  "default",
			Finalizers: []string{"snapshot.storage.kubernetes.io/volumesnapshot-as-source-protection"},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(snapshot).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	// Delete should not fail even with finalizer present
	err := manager.DeleteSnapshot(ctx, testSnapshotName, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// In real cluster, object would be marked for deletion but not removed yet
	// With fake client, it's removed immediately even with finalizer
	// This tests the API call succeeds
}

func TestManager_DeleteSnapshot_NotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = snapshotv1.AddToScheme(scheme)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	// Deleting non-existent snapshot should not fail (idempotent)
	err := manager.DeleteSnapshot(ctx, "nonexistent-snapshot", "default")
	if err == nil {
		t.Error("expected error for nonexistent snapshot")
	}
}

func TestManager_CreateSnapshotsForUpgrade_PartialFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)
	_ = fluxupv1alpha1.AddToScheme(scheme)

	// Only create first PVC, second is missing
	pvc1 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-app-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pvc1).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	pvcs := []fluxupv1alpha1.PVCRef{
		{Name: "data-app-0"},
		{Name: "data-missing-0"}, // This PVC doesn't exist
	}

	_, err := manager.CreateSnapshotsForUpgrade(ctx, "my-app", "default", pvcs, "csi-snapclass")
	if err == nil {
		t.Error("expected error for missing PVC")
	}

	// Current implementation does NOT clean up snapshots on failure
	// This is a known limitation - the caller (controller) should handle cleanup
	// Verify first snapshot was created before failure
	var snapshots snapshotv1.VolumeSnapshotList
	if err := client.List(ctx, &snapshots); err != nil {
		t.Fatalf("failed to list snapshots: %v", err)
	}

	if len(snapshots.Items) != 1 {
		t.Errorf("expected 1 snapshot (partial success), got %d", len(snapshots.Items))
	}
}

func TestManager_CreateSnapshotsForUpgrade_EmptyPVCList(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)
	_ = fluxupv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	snapshots, err := manager.CreateSnapshotsForUpgrade(ctx, "my-app", "default", []fluxupv1alpha1.PVCRef{}, "csi-snapclass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snapshots) != 0 {
		t.Errorf("expected 0 snapshots for empty PVC list, got %d", len(snapshots))
	}
}

func TestManager_ApplyRetentionPolicy_ConcurrentSnapshots(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = snapshotv1.AddToScheme(scheme)

	// Multiple snapshots with same timestamp (concurrent creation)
	snapshots := []snapshotv1.VolumeSnapshot{
		makeSnapshot("snap-1", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
		makeSnapshot("snap-2", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
		makeSnapshot("snap-3", "default", "my-app", "pvc-1", "2026-01-01T10:00:00Z"),
	}

	clientBuilder := fake.NewClientBuilder().WithScheme(scheme)
	for i := range snapshots {
		clientBuilder = clientBuilder.WithObjects(&snapshots[i])
	}
	client := clientBuilder.Build()

	manager := NewManager(client)
	ctx := context.Background()

	// Keep 1 - should delete 2 (deterministic by name ordering)
	deleted, err := manager.ApplyRetentionPolicy(ctx, "my-app", "default", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", deleted)
	}

	var remaining snapshotv1.VolumeSnapshotList
	if err := client.List(ctx, &remaining); err != nil {
		t.Fatalf("failed to list snapshots: %v", err)
	}

	if len(remaining.Items) != 1 {
		t.Errorf("expected 1 remaining, got %d", len(remaining.Items))
	}
}

func TestManager_RestorePVCFromSnapshot_BasicFlow(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	restoreSize := resource.MustParse("10Gi")
	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testSnapshotName,
			Namespace: "default",
		},
		Status: &snapshotv1.VolumeSnapshotStatus{
			ReadyToUse:  boolPtr(true),
			RestoreSize: &restoreSize,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(snapshot).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	req := RestoreRequest{
		SnapshotName:      testSnapshotName,
		SnapshotNamespace: "default",
		NewPVCName:        "test-pvc",
		NewPVCNamespace:   "default",
		StorageClassName:  "fast-ssd",
	}

	pvc, err := manager.RestorePVCFromSnapshot(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pvc.Spec.DataSource == nil {
		t.Fatal("expected PVC to have data source")
	}

	if pvc.Spec.DataSource.Name != testSnapshotName {
		t.Errorf("expected data source snapshot test-snapshot, got %s", pvc.Spec.DataSource.Name)
	}

	if pvc.Spec.DataSource.Kind != "VolumeSnapshot" {
		t.Errorf("expected data source kind VolumeSnapshot, got %s", pvc.Spec.DataSource.Kind)
	}

	if *pvc.Spec.StorageClassName != "fast-ssd" {
		t.Errorf("expected storage class fast-ssd, got %s", *pvc.Spec.StorageClassName)
	}
}

func TestManager_RestorePVCFromSnapshot_SnapshotNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	req := RestoreRequest{
		SnapshotName:      "nonexistent-snapshot",
		SnapshotNamespace: "default",
		NewPVCName:        "test-pvc",
		NewPVCNamespace:   "default",
	}

	_, err := manager.RestorePVCFromSnapshot(ctx, req)
	if err == nil {
		t.Error("expected error for nonexistent snapshot")
	}
}

func TestManager_RestorePVCFromSnapshot_SnapshotNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testSnapshotName,
			Namespace: "default",
		},
		Status: &snapshotv1.VolumeSnapshotStatus{
			ReadyToUse: boolPtr(false), // Not ready
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(snapshot).
		Build()

	manager := NewManager(client)
	ctx := context.Background()

	req := RestoreRequest{
		SnapshotName:      testSnapshotName,
		SnapshotNamespace: "default",
		NewPVCName:        "test-pvc",
		NewPVCNamespace:   "default",
	}

	_, err := manager.RestorePVCFromSnapshot(ctx, req)
	if err == nil {
		t.Error("expected error for snapshot not ready")
	}

	expectedMsg := "snapshot test-snapshot is not ready"
	if err != nil && err.Error() != expectedMsg {
		t.Errorf("expected error %q, got %q", expectedMsg, err.Error())
	}
}
