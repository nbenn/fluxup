package integration

import (
	"context"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/nbenn/fluxup/internal/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace         = "test-namespace"
	fakeDriverName        = "fake.csi.driver"
	fakeSnapshotClassName = "fake-snapshot-class"
)

// setupFakeK8sClient creates a fake Kubernetes client with necessary schemes
func setupFakeK8sClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	err := clientgoscheme.AddToScheme(scheme)
	require.NoError(t, err)

	err = snapshotv1.AddToScheme(scheme)
	require.NoError(t, err)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&snapshotv1.VolumeSnapshot{}, &corev1.PersistentVolumeClaim{}).
		Build()
}

// createFakeVolumeSnapshotClass creates a VolumeSnapshotClass for the fake driver
func createFakeVolumeSnapshotClass() *snapshotv1.VolumeSnapshotClass {
	deletionPolicy := snapshotv1.VolumeSnapshotContentDelete
	return &snapshotv1.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: fakeSnapshotClassName,
			Labels: map[string]string{
				"fluxup.io/snapshot-class": "true",
			},
		},
		Driver:         fakeDriverName,
		DeletionPolicy: deletionPolicy,
	}
}

// createFakePVC creates a PVC backed by the fake CSI driver
func createFakePVC(name string) *corev1.PersistentVolumeClaim {
	storageClassName := "fake-storage-class"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			StorageClassName: &storageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			},
		},
	}
}

// TestSnapshotCSI_CreateSnapshot verifies snapshot creation with fake CSI driver
func TestSnapshotCSI_CreateSnapshot(t *testing.T) {
	pvc := createFakePVC("test-pvc")
	snapshotClass := createFakeVolumeSnapshotClass()

	k8sClient := setupFakeK8sClient(t, pvc, snapshotClass)

	mgr := snapshot.NewManager(k8sClient)

	ctx := context.Background()
	snapshotName := "test-snapshot"

	// Create snapshot
	req := snapshot.SnapshotRequest{
		PVCName:                 pvc.Name,
		PVCNamespace:            testNamespace,
		SnapshotName:            snapshotName,
		VolumeSnapshotClassName: fakeSnapshotClassName,
	}
	vs, err := mgr.CreateSnapshot(ctx, req)
	require.NoError(t, err, "Should successfully create snapshot")
	assert.NotNil(t, vs)
	assert.Equal(t, snapshotName, vs.Name)
	assert.Equal(t, testNamespace, vs.Namespace)

	// Verify snapshot spec
	assert.Equal(t, fakeSnapshotClassName, *vs.Spec.VolumeSnapshotClassName)
	assert.Equal(t, pvc.Name, *vs.Spec.Source.PersistentVolumeClaimName)

	// Verify snapshot exists in client
	retrievedSnapshot := &snapshotv1.VolumeSnapshot{}
	err = k8sClient.Get(ctx, client.ObjectKey{Name: snapshotName, Namespace: testNamespace}, retrievedSnapshot)
	require.NoError(t, err, "Snapshot should exist in cluster")
	assert.Equal(t, snapshotName, retrievedSnapshot.Name)
}

// TestSnapshotCSI_SnapshotReadiness simulates snapshot becoming ready
func TestSnapshotCSI_SnapshotReadiness(t *testing.T) {
	pvc := createFakePVC("test-pvc")
	snapshotClass := createFakeVolumeSnapshotClass()

	k8sClient := setupFakeK8sClient(t, pvc, snapshotClass)
	mgr := snapshot.NewManager(k8sClient)

	ctx := context.Background()
	snapshotName := "test-snapshot-ready"

	// Create snapshot
	req := snapshot.SnapshotRequest{
		PVCName:                 pvc.Name,
		PVCNamespace:            testNamespace,
		SnapshotName:            snapshotName,
		VolumeSnapshotClassName: fakeSnapshotClassName,
	}
	vs, err := mgr.CreateSnapshot(ctx, req)
	require.NoError(t, err)

	// Initially, snapshot is not ready
	assert.Nil(t, vs.Status)

	// Simulate CSI driver making snapshot ready
	vs.Status = &snapshotv1.VolumeSnapshotStatus{
		BoundVolumeSnapshotContentName: ptr("snapcontent-" + snapshotName),
		ReadyToUse:                     ptr(true),
		CreationTime:                   &metav1.Time{Time: time.Now()},
		RestoreSize:                    resource.NewQuantity(1*1024*1024*1024, resource.BinarySI), // 1Gi
	}

	err = k8sClient.Status().Update(ctx, vs)
	require.NoError(t, err, "Should update snapshot status")

	// Verify snapshot is ready
	retrievedSnapshot := &snapshotv1.VolumeSnapshot{}
	err = k8sClient.Get(ctx, client.ObjectKey{Name: snapshotName, Namespace: testNamespace}, retrievedSnapshot)
	require.NoError(t, err)

	assert.NotNil(t, retrievedSnapshot.Status)
	assert.NotNil(t, retrievedSnapshot.Status.ReadyToUse)
	assert.True(t, *retrievedSnapshot.Status.ReadyToUse, "Snapshot should be ready to use")
	assert.NotNil(t, retrievedSnapshot.Status.RestoreSize)
}

// TestSnapshotCSI_RestoreFromSnapshot verifies PVC restoration from snapshot
func TestSnapshotCSI_RestoreFromSnapshot(t *testing.T) {
	// Create original PVC and snapshot
	originalPVC := createFakePVC("original-pvc")
	snapshotClass := createFakeVolumeSnapshotClass()

	// Create snapshot that's ready
	snapshotName := "ready-snapshot"
	vs := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotName,
			Namespace: testNamespace,
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: ptr(fakeSnapshotClassName),
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: ptr(originalPVC.Name),
			},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: ptr("snapcontent-" + snapshotName),
			ReadyToUse:                     ptr(true),
			CreationTime:                   &metav1.Time{Time: time.Now()},
			RestoreSize:                    resource.NewQuantity(1*1024*1024*1024, resource.BinarySI),
		},
	}

	k8sClient := setupFakeK8sClient(t, originalPVC, snapshotClass, vs)
	mgr := snapshot.NewManager(k8sClient)

	ctx := context.Background()

	// Restore PVC from snapshot
	restoredPVCName := "restored-pvc"
	req := snapshot.RestoreRequest{
		SnapshotName:      snapshotName,
		SnapshotNamespace: testNamespace,
		NewPVCName:        restoredPVCName,
		NewPVCNamespace:   testNamespace,
		StorageClassName:  *originalPVC.Spec.StorageClassName,
	}

	restoredPVC, err := mgr.RestorePVCFromSnapshot(ctx, req)
	require.NoError(t, err, "Should successfully restore PVC from snapshot")
	assert.NotNil(t, restoredPVC)
	assert.Equal(t, restoredPVCName, restoredPVC.Name)

	// Verify restored PVC spec
	assert.NotNil(t, restoredPVC.Spec.DataSource)
	assert.Equal(t, "VolumeSnapshot", restoredPVC.Spec.DataSource.Kind)
	assert.Equal(t, snapshotName, restoredPVC.Spec.DataSource.Name)

	// Verify PVC was created
	retrievedPVC := &corev1.PersistentVolumeClaim{}
	err = k8sClient.Get(ctx, client.ObjectKey{Name: restoredPVCName, Namespace: testNamespace}, retrievedPVC)
	require.NoError(t, err, "Restored PVC should exist")
}

// TestSnapshotCSI_DeleteSnapshot verifies snapshot deletion
func TestSnapshotCSI_DeleteSnapshot(t *testing.T) {
	snapshotName := "delete-test-snapshot"
	vs := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotName,
			Namespace: testNamespace,
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: ptr(fakeSnapshotClassName),
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: ptr("test-pvc"),
			},
		},
	}

	k8sClient := setupFakeK8sClient(t, vs)
	mgr := snapshot.NewManager(k8sClient)

	ctx := context.Background()

	// Verify snapshot exists
	err := k8sClient.Get(ctx, client.ObjectKey{Name: snapshotName, Namespace: testNamespace}, &snapshotv1.VolumeSnapshot{})
	require.NoError(t, err, "Snapshot should exist before deletion")

	// Delete snapshot
	err = mgr.DeleteSnapshot(ctx, snapshotName, testNamespace)
	require.NoError(t, err, "Should successfully delete snapshot")

	// Verify snapshot is deleted
	err = k8sClient.Get(ctx, client.ObjectKey{Name: snapshotName, Namespace: testNamespace}, &snapshotv1.VolumeSnapshot{})
	assert.Error(t, err, "Snapshot should not exist after deletion")
	assert.True(t, client.IgnoreNotFound(err) == nil, "Error should be NotFound")
}

// TestSnapshotCSI_MultipleSnapshots verifies handling of multiple snapshots
func TestSnapshotCSI_MultipleSnapshots(t *testing.T) {
	pvc1 := createFakePVC("pvc-1")
	pvc2 := createFakePVC("pvc-2")
	snapshotClass := createFakeVolumeSnapshotClass()

	k8sClient := setupFakeK8sClient(t, pvc1, pvc2, snapshotClass)
	mgr := snapshot.NewManager(k8sClient)

	ctx := context.Background()

	// Create multiple snapshots manually (or test individual creation)
	req1 := snapshot.SnapshotRequest{
		PVCName:                 pvc1.Name,
		PVCNamespace:            testNamespace,
		SnapshotName:            "multi-test-pvc-1",
		VolumeSnapshotClassName: fakeSnapshotClassName,
	}
	req2 := snapshot.SnapshotRequest{
		PVCName:                 pvc2.Name,
		PVCNamespace:            testNamespace,
		SnapshotName:            "multi-test-pvc-2",
		VolumeSnapshotClassName: fakeSnapshotClassName,
	}

	vs1, err := mgr.CreateSnapshot(ctx, req1)
	require.NoError(t, err, "Should create first snapshot")
	vs2, err := mgr.CreateSnapshot(ctx, req2)
	require.NoError(t, err, "Should create second snapshot")

	snapshots := []*snapshotv1.VolumeSnapshot{vs1, vs2}
	assert.Len(t, snapshots, 2, "Should create 2 snapshots")

	// Verify each snapshot
	pvcs := []*corev1.PersistentVolumeClaim{pvc1, pvc2}
	expectedNames := []string{"multi-test-pvc-1", "multi-test-pvc-2"}
	for i, vs := range snapshots {
		assert.NotNil(t, vs)
		assert.Equal(t, expectedNames[i], vs.Name)
		assert.Equal(t, pvcs[i].Name, *vs.Spec.Source.PersistentVolumeClaimName)
	}

	// Verify all snapshots exist
	snapshotList := &snapshotv1.VolumeSnapshotList{}
	err = k8sClient.List(ctx, snapshotList, client.InNamespace(testNamespace))
	require.NoError(t, err)
	assert.Len(t, snapshotList.Items, 2, "Should have 2 snapshots in namespace")
}

// TestSnapshotCSI_SnapshotClassSelection verifies correct snapshot class selection
func TestSnapshotCSI_SnapshotClassSelection(t *testing.T) {
	// Create multiple snapshot classes
	class1 := createFakeVolumeSnapshotClass()
	class1.Name = "class-1"
	class1.Driver = "driver-1"

	class2 := createFakeVolumeSnapshotClass()
	class2.Name = "class-2"
	class2.Driver = "driver-2"

	pvc := createFakePVC("test-pvc")

	k8sClient := setupFakeK8sClient(t, pvc, class1, class2)
	mgr := snapshot.NewManager(k8sClient)

	ctx := context.Background()

	// Create snapshot with specific class
	req := snapshot.SnapshotRequest{
		PVCName:                 pvc.Name,
		PVCNamespace:            testNamespace,
		SnapshotName:            "test-snapshot",
		VolumeSnapshotClassName: "class-2",
	}
	vs, err := mgr.CreateSnapshot(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, "class-2", *vs.Spec.VolumeSnapshotClassName, "Should use specified snapshot class")

	// Verify snapshot references correct class
	retrievedSnapshot := &snapshotv1.VolumeSnapshot{}
	err = k8sClient.Get(ctx, client.ObjectKey{Name: "test-snapshot", Namespace: testNamespace}, retrievedSnapshot)
	require.NoError(t, err)
	assert.Equal(t, "class-2", *retrievedSnapshot.Spec.VolumeSnapshotClassName)
}

// TestSnapshotCSI_PartialFailureHandling verifies cleanup on partial failures
func TestSnapshotCSI_PartialFailureHandling(t *testing.T) {
	pvc1 := createFakePVC("pvc-1")
	pvc2 := createFakePVC("pvc-2")
	snapshotClass := createFakeVolumeSnapshotClass()

	k8sClient := setupFakeK8sClient(t, pvc1, pvc2, snapshotClass)
	mgr := snapshot.NewManager(k8sClient)

	ctx := context.Background()

	// Create first snapshot successfully
	req1 := snapshot.SnapshotRequest{
		PVCName:                 pvc1.Name,
		PVCNamespace:            testNamespace,
		SnapshotName:            "snap-1",
		VolumeSnapshotClassName: fakeSnapshotClassName,
	}
	snap1, err := mgr.CreateSnapshot(ctx, req1)
	require.NoError(t, err)
	assert.NotNil(t, snap1)

	// Try to create second snapshot with non-existent PVC (should fail)
	req2 := snapshot.SnapshotRequest{
		PVCName:                 "non-existent-pvc",
		PVCNamespace:            testNamespace,
		SnapshotName:            "snap-2",
		VolumeSnapshotClassName: fakeSnapshotClassName,
	}
	_, err = mgr.CreateSnapshot(ctx, req2)
	assert.Error(t, err, "Should fail with non-existent PVC")

	// Verify first snapshot still exists (no rollback unless explicitly requested)
	existingSnapshot := &snapshotv1.VolumeSnapshot{}
	err = k8sClient.Get(ctx, client.ObjectKey{Name: "snap-1", Namespace: testNamespace}, existingSnapshot)
	assert.NoError(t, err, "First snapshot should still exist")
}

// ptr is a helper function to get pointer to value
func ptr[T any](v T) *T {
	return &v
}
