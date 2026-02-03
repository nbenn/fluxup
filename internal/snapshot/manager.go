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
	"fmt"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/logging"
)

// Manager handles VolumeSnapshot lifecycle
type Manager struct {
	client client.Client
}

// NewManager creates a new snapshot manager
func NewManager(c client.Client) *Manager {
	return &Manager{client: c}
}

// SnapshotRequest defines what to snapshot
type SnapshotRequest struct {
	PVCName                 string
	PVCNamespace            string
	SnapshotName            string
	VolumeSnapshotClassName string
	Labels                  map[string]string
}

// SnapshotInfo holds snapshot creation results
type SnapshotInfo struct {
	PVCName      string
	SnapshotName string
	Namespace    string
}

// CreateSnapshot creates a VolumeSnapshot for a PVC
func (m *Manager) CreateSnapshot(ctx context.Context, req SnapshotRequest) (*snapshotv1.VolumeSnapshot, error) {
	logger := logging.FromContext(ctx)
	logger.Debug("creating volume snapshot", "pvc", req.PVCName, "namespace", req.PVCNamespace, "snapshotName", req.SnapshotName)

	// Verify PVC exists
	var pvc corev1.PersistentVolumeClaim
	pvcKey := types.NamespacedName{Name: req.PVCName, Namespace: req.PVCNamespace}
	if err := m.client.Get(ctx, pvcKey, &pvc); err != nil {
		return nil, fmt.Errorf("PVC not found %s/%s: %w", req.PVCNamespace, req.PVCName, err)
	}

	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.SnapshotName,
			Namespace: req.PVCNamespace,
			Labels:    req.Labels,
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &req.PVCName,
			},
		},
	}

	if req.VolumeSnapshotClassName != "" {
		snapshot.Spec.VolumeSnapshotClassName = &req.VolumeSnapshotClassName
	}

	if err := m.client.Create(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("creating snapshot: %w", err)
	}

	logger.Info("created volume snapshot", "snapshot", req.SnapshotName, "pvc", req.PVCName)
	return snapshot, nil
}

// WaitForSnapshotReady waits for a snapshot to be ready using simple polling
func (m *Manager) WaitForSnapshotReady(ctx context.Context, name, namespace string, timeout time.Duration) error {
	logger := logging.FromContext(ctx)
	logger.Debug("waiting for snapshot to be ready", "snapshot", name, "namespace", namespace, "timeout", timeout)

	deadline := time.Now().Add(timeout)
	key := types.NamespacedName{Name: name, Namespace: namespace}

	for time.Now().Before(deadline) {
		var snapshot snapshotv1.VolumeSnapshot
		if err := m.client.Get(ctx, key, &snapshot); err != nil {
			return fmt.Errorf("getting snapshot: %w", err)
		}

		if snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse {
			logger.Debug("snapshot is ready", "snapshot", name)
			return nil
		}

		// Check for errors
		if snapshot.Status != nil && snapshot.Status.Error != nil && snapshot.Status.Error.Message != nil {
			return fmt.Errorf("snapshot error: %s", *snapshot.Status.Error.Message)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			logger.Debug("snapshot not ready yet, polling", "snapshot", name)
		}
	}

	return fmt.Errorf("timeout waiting for snapshot %s/%s to be ready", namespace, name)
}

// IsSnapshotReady checks if a snapshot is ready without waiting
func (m *Manager) IsSnapshotReady(ctx context.Context, name, namespace string) (bool, error) {
	key := types.NamespacedName{Name: name, Namespace: namespace}
	var snapshot snapshotv1.VolumeSnapshot
	if err := m.client.Get(ctx, key, &snapshot); err != nil {
		return false, fmt.Errorf("getting snapshot: %w", err)
	}

	if snapshot.Status != nil && snapshot.Status.ReadyToUse != nil {
		return *snapshot.Status.ReadyToUse, nil
	}

	return false, nil
}

// CreateSnapshotsForUpgrade creates pre-upgrade snapshots for all configured PVCs
func (m *Manager) CreateSnapshotsForUpgrade(
	ctx context.Context,
	appName, appNamespace string,
	pvcs []fluxupv1alpha1.PVCRef,
	snapshotClass string,
) ([]SnapshotInfo, error) {
	logger := logging.FromContext(ctx)
	logger.Info("creating pre-upgrade snapshots", "app", appName, "pvcCount", len(pvcs))

	timestamp := time.Now().Format("20060102-150405")
	snapshots := make([]SnapshotInfo, 0, len(pvcs))

	for _, pvc := range pvcs {
		ns := pvc.Namespace
		if ns == "" {
			ns = appNamespace
		}

		snapshotName := fmt.Sprintf("%s-%s-pre-upgrade-%s", appName, pvc.Name, timestamp)

		req := SnapshotRequest{
			PVCName:                 pvc.Name,
			PVCNamespace:            ns,
			SnapshotName:            snapshotName,
			VolumeSnapshotClassName: snapshotClass,
			Labels: map[string]string{
				"fluxup.dev/managed-app": appName,
				"fluxup.dev/purpose":     "pre-upgrade",
			},
		}

		snapshot, err := m.CreateSnapshot(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("creating snapshot for PVC %s: %w", pvc.Name, err)
		}

		snapshots = append(snapshots, SnapshotInfo{
			PVCName:      pvc.Name,
			SnapshotName: snapshot.Name,
			Namespace:    snapshot.Namespace,
		})
	}

	logger.Info("created all pre-upgrade snapshots", "app", appName, "count", len(snapshots))
	return snapshots, nil
}

// DeleteSnapshot deletes a VolumeSnapshot
func (m *Manager) DeleteSnapshot(ctx context.Context, name, namespace string) error {
	logger := logging.FromContext(ctx)
	logger.Debug("deleting volume snapshot", "snapshot", name, "namespace", namespace)

	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	if err := m.client.Delete(ctx, snapshot); err != nil {
		return err
	}

	logger.Info("deleted volume snapshot", "snapshot", name, "namespace", namespace)
	return nil
}

// DefaultMaxSnapshotCount is the default number of snapshots to retain per PVC
const DefaultMaxSnapshotCount = 3

// ApplyRetentionPolicy prunes old snapshots for a ManagedApp based on the retention policy.
// It keeps only the newest maxCount snapshots per PVC, deleting older ones.
// If maxCount is 0, no snapshots are deleted (retention disabled).
func (m *Manager) ApplyRetentionPolicy(ctx context.Context, appName, namespace string, maxCount int) (int, error) {
	logger := logging.FromContext(ctx)

	if maxCount == 0 {
		logger.Debug("retention policy disabled, keeping all snapshots", "app", appName)
		return 0, nil
	}

	logger.Debug("applying snapshot retention policy", "app", appName, "maxCount", maxCount)

	// List all snapshots for this app
	var snapshots snapshotv1.VolumeSnapshotList
	if err := m.client.List(ctx, &snapshots,
		client.InNamespace(namespace),
		client.MatchingLabels{"fluxup.dev/managed-app": appName},
	); err != nil {
		return 0, fmt.Errorf("listing snapshots: %w", err)
	}

	if len(snapshots.Items) == 0 {
		logger.Debug("no snapshots found for app", "app", appName)
		return 0, nil
	}

	logger.Debug("found snapshots for retention check", "app", appName, "total", len(snapshots.Items))

	// Group snapshots by PVC name
	byPVC := make(map[string][]snapshotv1.VolumeSnapshot)
	for _, snap := range snapshots.Items {
		if snap.Spec.Source.PersistentVolumeClaimName == nil {
			continue
		}
		pvcName := *snap.Spec.Source.PersistentVolumeClaimName
		byPVC[pvcName] = append(byPVC[pvcName], snap)
	}

	// For each PVC, sort by creation time and delete oldest beyond maxCount
	deleted := 0
	for pvcName, pvcSnapshots := range byPVC {
		if len(pvcSnapshots) <= maxCount {
			continue
		}

		// Sort by creation time (newest first)
		sortSnapshotsByCreationTime(pvcSnapshots)

		// Delete snapshots beyond maxCount
		for _, snap := range pvcSnapshots[maxCount:] {
			if err := m.client.Delete(ctx, &snap); err != nil {
				logger.Warn("failed to delete old snapshot", "snapshot", snap.Name, "error", err)
				continue
			}
			logger.Debug("deleted old snapshot", "snapshot", snap.Name, "pvc", pvcName)
			deleted++
		}
	}

	if deleted > 0 {
		logger.Info("pruned old snapshots", "app", appName, "deleted", deleted)
	}
	return deleted, nil
}

// sortSnapshotsByCreationTime sorts snapshots by creation timestamp, newest first
func sortSnapshotsByCreationTime(snapshots []snapshotv1.VolumeSnapshot) {
	for i := 0; i < len(snapshots)-1; i++ {
		for j := i + 1; j < len(snapshots); j++ {
			if snapshots[j].CreationTimestamp.After(snapshots[i].CreationTimestamp.Time) {
				snapshots[i], snapshots[j] = snapshots[j], snapshots[i]
			}
		}
	}
}
