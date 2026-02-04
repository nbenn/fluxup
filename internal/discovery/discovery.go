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

// Package discovery provides auto-discovery of PVCs and workloads for ManagedApps.
// It supports discovering resources from both HelmRelease and Kustomization sources.
package discovery

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

// PVCInfo contains information about a discovered PVC.
type PVCInfo struct {
	Name      string
	Namespace string
	// AccessModes contains the access modes of the PVC (e.g., ReadWriteOnce).
	AccessModes []corev1.PersistentVolumeAccessMode
}

// WorkloadInfo contains information about a discovered workload.
type WorkloadInfo struct {
	Kind      string // "Deployment" or "StatefulSet"
	Name      string
	Namespace string
}

// Discoverer provides methods to discover PVCs and workloads for a ManagedApp.
type Discoverer struct {
	client client.Client
}

// New creates a new Discoverer instance.
func New(c client.Client) *Discoverer {
	return &Discoverer{client: c}
}

// DiscoverPVCs discovers all RWO PVCs that should be snapshotted for the given ManagedApp.
// It uses the following priority:
// 1. Explicit PVC list from app.Spec.VolumeSnapshots.PVCs (if set)
// 2. Auto-discovery from HelmRelease (if helmReleaseRef is set)
// 3. Auto-discovery from Kustomization inventory
//
// Returns only RWO PVCs, excluding any listed in app.Spec.VolumeSnapshots.ExcludePVCs.
func (d *Discoverer) DiscoverPVCs(ctx context.Context, app *fluxupv1alpha1.ManagedApp) ([]PVCInfo, error) {
	// If explicit PVCs are configured, use those
	if app.Spec.VolumeSnapshots != nil && len(app.Spec.VolumeSnapshots.PVCs) > 0 {
		return d.getExplicitPVCs(ctx, app)
	}

	// Auto-discover PVCs
	var pvcs []PVCInfo
	var err error

	if app.Spec.HelmReleaseRef != nil {
		pvcs, err = d.discoverPVCsFromHelmRelease(ctx, app)
	} else {
		pvcs, err = d.discoverPVCsFromKustomization(ctx, app)
	}

	if err != nil {
		return nil, err
	}

	// Filter to RWO only and apply exclusions
	return d.filterPVCs(pvcs, app), nil
}

// DiscoverWorkloadsForPVCs discovers all workloads that mount the given PVCs.
// This is used to determine which workloads need to be scaled down before
// taking a snapshot or restoring from one.
func (d *Discoverer) DiscoverWorkloadsForPVCs(ctx context.Context, pvcNames []string, namespace string) ([]WorkloadInfo, error) {
	return d.discoverWorkloadsFromPVCs(ctx, pvcNames, namespace)
}

// DiscoverWorkloads discovers all workloads belonging to the ManagedApp.
// This combines PVC discovery with workload discovery.
func (d *Discoverer) DiscoverWorkloads(ctx context.Context, app *fluxupv1alpha1.ManagedApp) ([]WorkloadInfo, error) {
	// First discover PVCs
	pvcs, err := d.DiscoverPVCs(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("discovering PVCs: %w", err)
	}

	if len(pvcs) == 0 {
		// No PVCs means no workloads to scale
		return nil, nil
	}

	// Extract PVC names
	pvcNames := make([]string, len(pvcs))
	for i, pvc := range pvcs {
		pvcNames[i] = pvc.Name
	}

	// Discover workloads that mount these PVCs
	return d.DiscoverWorkloadsForPVCs(ctx, pvcNames, app.Namespace)
}

// getExplicitPVCs returns the explicitly configured PVCs from the ManagedApp spec.
func (d *Discoverer) getExplicitPVCs(ctx context.Context, app *fluxupv1alpha1.ManagedApp) ([]PVCInfo, error) {
	result := make([]PVCInfo, 0, len(app.Spec.VolumeSnapshots.PVCs))
	for _, pvcRef := range app.Spec.VolumeSnapshots.PVCs {
		ns := pvcRef.Namespace
		if ns == "" {
			ns = app.Namespace
		}

		// Verify the PVC exists and get its access modes
		var pvc corev1.PersistentVolumeClaim
		if err := d.client.Get(ctx, client.ObjectKey{Name: pvcRef.Name, Namespace: ns}, &pvc); err != nil {
			return nil, fmt.Errorf("getting PVC %s/%s: %w", ns, pvcRef.Name, err)
		}

		result = append(result, PVCInfo{
			Name:        pvc.Name,
			Namespace:   pvc.Namespace,
			AccessModes: pvc.Spec.AccessModes,
		})
	}
	return result, nil
}

// filterPVCs filters the discovered PVCs to only include RWO PVCs
// and excludes any listed in the app's exclusion list.
func (d *Discoverer) filterPVCs(pvcs []PVCInfo, app *fluxupv1alpha1.ManagedApp) []PVCInfo {
	// Build exclusion set
	excluded := make(map[string]bool)
	if app.Spec.VolumeSnapshots != nil {
		for _, excl := range app.Spec.VolumeSnapshots.ExcludePVCs {
			ns := excl.Namespace
			if ns == "" {
				ns = app.Namespace
			}
			excluded[ns+"/"+excl.Name] = true
		}
	}

	result := make([]PVCInfo, 0, len(pvcs))
	for _, pvc := range pvcs {
		// Check exclusion
		key := pvc.Namespace + "/" + pvc.Name
		if excluded[key] {
			continue
		}

		// Check for RWO access mode
		if !slices.Contains(pvc.AccessModes, corev1.ReadWriteOnce) {
			continue
		}

		result = append(result, pvc)
	}

	return result
}
