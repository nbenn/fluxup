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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestWaitForSnapshotReady tests the WaitForSnapshotReady method
func TestWaitForSnapshotReady(t *testing.T) {
	tests := []struct {
		name          string
		snapshot      *snapshotv1.VolumeSnapshot
		updateAfter   time.Duration // Simulate snapshot becoming ready after this duration
		timeout       time.Duration
		expectError   bool
		errorContains string
	}{
		{
			name: "snapshot already ready",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-snapshot",
					Namespace: "default",
				},
				Status: &snapshotv1.VolumeSnapshotStatus{
					ReadyToUse: ptr(true),
				},
			},
			timeout:     10 * time.Second,
			expectError: false,
		},
		{
			name: "snapshot becomes ready quickly",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-snapshot",
					Namespace: "default",
				},
				Status: &snapshotv1.VolumeSnapshotStatus{
					ReadyToUse: ptr(false),
				},
			},
			updateAfter: 2 * time.Second,
			timeout:     10 * time.Second,
			expectError: false,
		},
		{
			name: "snapshot times out",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-snapshot",
					Namespace: "default",
				},
				Status: &snapshotv1.VolumeSnapshotStatus{
					ReadyToUse: ptr(false),
				},
			},
			updateAfter:   10 * time.Second, // Will become ready but after timeout
			timeout:       2 * time.Second,
			expectError:   true,
			errorContains: "timeout",
		},
		{
			name: "snapshot has error",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-snapshot",
					Namespace: "default",
				},
				Status: &snapshotv1.VolumeSnapshotStatus{
					ReadyToUse: ptr(false),
					Error: &snapshotv1.VolumeSnapshotError{
						Message: ptr("CSI driver failed to create snapshot"),
					},
				},
			},
			timeout:       10 * time.Second,
			expectError:   true,
			errorContains: "snapshot error: CSI driver failed",
		},
		{
			name: "snapshot not found",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-existent",
					Namespace: "default",
				},
			},
			timeout:       5 * time.Second,
			expectError:   true,
			errorContains: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Create scheme and fake client
			scheme := runtime.NewScheme()
			_ = snapshotv1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)

			var objs []client.Object
			if tt.name != "snapshot not found" {
				objs = append(objs, tt.snapshot)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(&snapshotv1.VolumeSnapshot{}).
				Build()

			// If we need to simulate snapshot becoming ready
			if tt.updateAfter > 0 && tt.updateAfter < tt.timeout {
				go func() {
					time.Sleep(tt.updateAfter)
					var snapshot snapshotv1.VolumeSnapshot
					key := types.NamespacedName{Name: tt.snapshot.Name, Namespace: tt.snapshot.Namespace}
					if err := fakeClient.Get(ctx, key, &snapshot); err == nil {
						snapshot.Status = &snapshotv1.VolumeSnapshotStatus{
							ReadyToUse: ptr(true),
						}
						_ = fakeClient.Status().Update(ctx, &snapshot)
					}
				}()
			}

			manager := &Manager{client: fakeClient}

			err := manager.WaitForSnapshotReady(ctx, tt.snapshot.Name, tt.snapshot.Namespace, tt.timeout)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain %q but got: %v", tt.errorContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// TestWaitForPVCBound tests the WaitForPVCBound method
func TestWaitForPVCBound(t *testing.T) {
	tests := []struct {
		name          string
		pvc           *corev1.PersistentVolumeClaim
		updateAfter   time.Duration
		timeout       time.Duration
		expectError   bool
		errorContains string
	}{
		{
			name: "PVC already bound",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase: corev1.ClaimBound,
				},
			},
			timeout:     10 * time.Second,
			expectError: false,
		},
		{
			name: "PVC becomes bound",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase: corev1.ClaimPending,
				},
			},
			updateAfter: 2 * time.Second,
			timeout:     10 * time.Second,
			expectError: false,
		},
		{
			name: "PVC binding timeout",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase: corev1.ClaimPending,
				},
			},
			updateAfter:   10 * time.Second,
			timeout:       2 * time.Second,
			expectError:   true,
			errorContains: "timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.pvc).
				WithStatusSubresource(&corev1.PersistentVolumeClaim{}).
				Build()

			// Simulate PVC becoming bound
			if tt.updateAfter > 0 && tt.updateAfter < tt.timeout {
				go func() {
					time.Sleep(tt.updateAfter)
					var pvc corev1.PersistentVolumeClaim
					key := types.NamespacedName{Name: tt.pvc.Name, Namespace: tt.pvc.Namespace}
					if err := fakeClient.Get(ctx, key, &pvc); err == nil {
						pvc.Status.Phase = corev1.ClaimBound
						_ = fakeClient.Status().Update(ctx, &pvc)
					}
				}()
			}

			manager := &Manager{client: fakeClient}

			err := manager.WaitForPVCBound(ctx, tt.pvc.Name, tt.pvc.Namespace, tt.timeout)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain %q but got: %v", tt.errorContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// TestWaitForPVCDeleted tests the WaitForPVCDeleted method
func TestWaitForPVCDeleted(t *testing.T) {
	tests := []struct {
		name          string
		pvc           *corev1.PersistentVolumeClaim
		deleteAfter   time.Duration
		timeout       time.Duration
		expectError   bool
		errorContains string
	}{
		{
			name: "PVC already deleted",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deleted-pvc",
					Namespace: "default",
				},
			},
			timeout:     10 * time.Second,
			expectError: false,
		},
		{
			name: "PVC gets deleted",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			},
			deleteAfter: 2 * time.Second,
			timeout:     10 * time.Second,
			expectError: false,
		},
		{
			name: "PVC deletion timeout",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "stuck-pvc",
					Namespace: "default",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			},
			deleteAfter:   10 * time.Second,
			timeout:       2 * time.Second,
			expectError:   true,
			errorContains: "timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)

			var objs []client.Object
			if tt.name != "PVC already deleted" {
				objs = append(objs, tt.pvc)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				Build()

			// Simulate PVC deletion
			if tt.deleteAfter > 0 && tt.deleteAfter < tt.timeout {
				go func() {
					time.Sleep(tt.deleteAfter)
					_ = fakeClient.Delete(ctx, tt.pvc)
				}()
			}

			manager := &Manager{client: fakeClient}

			err := manager.WaitForPVCDeleted(ctx, tt.pvc.Name, tt.pvc.Namespace, tt.timeout)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain %q but got: %v", tt.errorContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// TestIsPVCBound tests the IsPVCBound method
func TestIsPVCBound(t *testing.T) {
	tests := []struct {
		name        string
		pvc         *corev1.PersistentVolumeClaim
		expectBound bool
		expectError bool
	}{
		{
			name: "PVC is bound",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bound-pvc",
					Namespace: "default",
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase: corev1.ClaimBound,
				},
			},
			expectBound: true,
			expectError: false,
		},
		{
			name: "PVC is pending",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pending-pvc",
					Namespace: "default",
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase: corev1.ClaimPending,
				},
			},
			expectBound: false,
			expectError: false,
		},
		{
			name: "PVC not found",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-existent",
					Namespace: "default",
				},
			},
			expectBound: false,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)

			var objs []client.Object
			if tt.name != "PVC not found" {
				objs = append(objs, tt.pvc)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(&corev1.PersistentVolumeClaim{}).
				Build()

			manager := &Manager{client: fakeClient}

			bound, err := manager.IsPVCBound(ctx, tt.pvc.Name, tt.pvc.Namespace)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if bound != tt.expectBound {
					t.Errorf("expected bound=%v but got %v", tt.expectBound, bound)
				}
			}
		})
	}
}

// TestIsPVCDeleted tests the IsPVCDeleted method
func TestIsPVCDeleted(t *testing.T) {
	tests := []struct {
		name          string
		pvc           *corev1.PersistentVolumeClaim
		expectDeleted bool
		expectError   bool
	}{
		{
			name: "PVC exists",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "existing-pvc",
					Namespace: "default",
				},
			},
			expectDeleted: false,
			expectError:   false,
		},
		{
			name: "PVC deleted",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deleted-pvc",
					Namespace: "default",
				},
			},
			expectDeleted: true,
			expectError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)

			var objs []client.Object
			if tt.name != "PVC deleted" {
				objs = append(objs, tt.pvc)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				Build()

			manager := &Manager{client: fakeClient}

			deleted, err := manager.IsPVCDeleted(ctx, tt.pvc.Name, tt.pvc.Namespace)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if deleted != tt.expectDeleted {
					t.Errorf("expected deleted=%v but got %v", tt.expectDeleted, deleted)
				}
			}
		})
	}
}

// TestDeletePVC tests the DeletePVC method
func TestDeletePVC(t *testing.T) {
	tests := []struct {
		name          string
		pvc           *corev1.PersistentVolumeClaim
		expectError   bool
		errorContains string
	}{
		{
			name: "delete existing PVC",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "delete non-existent PVC",
			pvc: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-existent",
					Namespace: "default",
				},
			},
			expectError:   true,
			errorContains: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)

			var objs []client.Object
			if tt.name != "delete non-existent PVC" {
				objs = append(objs, tt.pvc)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				Build()

			manager := &Manager{client: fakeClient}

			err := manager.DeletePVC(ctx, tt.pvc.Name, tt.pvc.Namespace)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain %q but got: %v", tt.errorContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}

				// Verify PVC was deleted
				var pvc corev1.PersistentVolumeClaim
				key := types.NamespacedName{Name: tt.pvc.Name, Namespace: tt.pvc.Namespace}
				err = fakeClient.Get(ctx, key, &pvc)
				if err == nil {
					t.Errorf("expected PVC to be deleted but it still exists")
				}
			}
		})
	}
}

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
