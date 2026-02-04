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
	"fmt"
	"strings"
	"unicode"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/logging"
)

// DefaultFluxNamespace is the default namespace for Flux resources.
const DefaultFluxNamespace = "flux-system"

// discoverPVCsFromKustomization discovers PVCs from a Kustomization by:
// 1. Reading the Kustomization's inventory (status.inventory.entries)
// 2. Filtering to Deployments and StatefulSets
// 3. Finding Pods via ownerReferences
// 4. Extracting PVCs from pod volumes
func (d *Discoverer) discoverPVCsFromKustomization(ctx context.Context, app *fluxupv1alpha1.ManagedApp) ([]PVCInfo, error) {
	logger := logging.FromContext(ctx)

	ksRef := app.Spec.KustomizationRef
	ksNamespace := ksRef.Namespace
	if ksNamespace == "" {
		ksNamespace = DefaultFluxNamespace
	}

	logger.Debug("discovering PVCs from Kustomization",
		"kustomization", ksRef.Name,
		"namespace", ksNamespace)

	// Get the Kustomization
	var ks kustomizev1.Kustomization
	if err := d.client.Get(ctx, client.ObjectKey{Name: ksRef.Name, Namespace: ksNamespace}, &ks); err != nil {
		return nil, fmt.Errorf("getting Kustomization %s/%s: %w", ksNamespace, ksRef.Name, err)
	}

	// Check if inventory is available
	if ks.Status.Inventory == nil || len(ks.Status.Inventory.Entries) == 0 {
		logger.Debug("Kustomization has no inventory, returning empty PVC list")
		return nil, nil
	}

	// Extract workloads from inventory
	workloads := d.extractWorkloadsFromInventory(ks.Status.Inventory.Entries, app.Namespace)
	logger.Debug("found workloads in Kustomization inventory", "count", len(workloads))

	// Find PVCs used by these workloads
	return d.findPVCsForWorkloads(ctx, workloads, app.Namespace)
}

// extractWorkloadsFromInventory extracts Deployment and StatefulSet resources from the inventory.
// Inventory entries have the format: <namespace>_<name>_<group>_<kind>
// Example: "default_my-app_apps_Deployment"
func (d *Discoverer) extractWorkloadsFromInventory(entries []kustomizev1.ResourceRef, defaultNamespace string) []WorkloadInfo {
	workloads := make([]WorkloadInfo, 0, len(entries))

	for _, entry := range entries {
		// Parse the entry ID
		// Format: <namespace>_<name>_<group>_<kind>
		parts := strings.Split(entry.ID, "_")
		if len(parts) < 4 {
			continue
		}

		namespace := parts[0]
		name := parts[1]
		// group := parts[2]  // e.g., "apps"
		kind := parts[3]

		if kind != kindDeployment && kind != kindStatefulSet {
			continue
		}

		if namespace == "" {
			namespace = defaultNamespace
		}

		workloads = append(workloads, WorkloadInfo{
			Kind:      kind,
			Name:      name,
			Namespace: namespace,
		})
	}

	return workloads
}

// discoverWorkloadsFromPVCs discovers all workloads that mount the given PVCs.
// This is used to find workloads that need to be scaled down before snapshot/restore.
func (d *Discoverer) discoverWorkloadsFromPVCs(ctx context.Context, pvcNames []string, namespace string) ([]WorkloadInfo, error) {
	logger := logging.FromContext(ctx)

	// Build a set of PVC names for fast lookup
	pvcSet := make(map[string]bool)
	for _, name := range pvcNames {
		pvcSet[name] = true
	}

	// List all pods in the namespace
	var pods corev1.PodList
	if err := d.client.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	// Find pods that mount any of the target PVCs
	workloadSet := make(map[string]WorkloadInfo)
	for _, pod := range pods.Items {
		// Check if this pod mounts any of our PVCs
		mountsTargetPVC := false
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && pvcSet[vol.PersistentVolumeClaim.ClaimName] {
				mountsTargetPVC = true
				break
			}
		}

		if !mountsTargetPVC {
			continue
		}

		// Find the owning workload
		workload := d.getOwningWorkload(ctx, &pod)
		if workload != nil {
			key := fmt.Sprintf("%s/%s/%s", workload.Kind, workload.Namespace, workload.Name)
			if _, exists := workloadSet[key]; !exists {
				workloadSet[key] = *workload
			}
		}
	}

	// Convert map to slice
	result := make([]WorkloadInfo, 0, len(workloadSet))
	for _, w := range workloadSet {
		result = append(result, w)
	}

	logger.Debug("discovered workloads for PVCs",
		"pvcCount", len(pvcNames),
		"workloadCount", len(result))

	return result, nil
}

// getOwningWorkload finds the Deployment or StatefulSet that owns the given Pod.
func (d *Discoverer) getOwningWorkload(_ context.Context, pod *corev1.Pod) *WorkloadInfo {
	for _, owner := range pod.OwnerReferences {
		switch owner.Kind {
		case kindStatefulSet:
			// StatefulSets directly own their Pods
			return &WorkloadInfo{
				Kind:      kindStatefulSet,
				Name:      owner.Name,
				Namespace: pod.Namespace,
			}
		case "ReplicaSet":
			// Deployments own ReplicaSets which own Pods
			// Find the Deployment that owns this ReplicaSet
			deployment := findDeploymentForReplicaSet(owner.Name)
			if deployment != "" {
				return &WorkloadInfo{
					Kind:      kindDeployment,
					Name:      deployment,
					Namespace: pod.Namespace,
				}
			}
		}
	}
	return nil
}

// findDeploymentForReplicaSet finds the Deployment that owns a ReplicaSet.
// ReplicaSets created by Deployments are named <deployment-name>-<hash>.
func findDeploymentForReplicaSet(rsName string) string {
	// Try to find the deployment by checking the ReplicaSet's ownerReferences
	// For simplicity, we use the naming convention: <deployment>-<hash>
	// where hash is a 10-character alphanumeric string

	// Find the last dash that separates deployment name from hash
	lastDash := strings.LastIndex(rsName, "-")
	if lastDash == -1 {
		return ""
	}

	// The hash is typically 8-10 characters
	hash := rsName[lastDash+1:]
	if len(hash) < 5 || len(hash) > 12 {
		// Doesn't look like a Deployment-generated ReplicaSet name
		return ""
	}

	// Check if all characters in hash are lowercase alphanumeric
	for _, c := range hash {
		if !unicode.IsLower(c) && !unicode.IsDigit(c) {
			return ""
		}
	}

	return rsName[:lastDash]
}
