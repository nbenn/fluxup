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

package flux

import (
	"context"
	"fmt"
	"time"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nbenn/fluxup/internal/logging"
)

// Helper provides Flux-related operations
type Helper struct {
	client client.Client
}

// NewHelper creates a new Flux helper
func NewHelper(c client.Client) *Helper {
	return &Helper{client: c}
}

// SuspendKustomization suspends a Flux Kustomization
func (h *Helper) SuspendKustomization(ctx context.Context, name, namespace string) error {
	logger := logging.FromContext(ctx)
	logger.Debug("suspending Kustomization", "name", name, "namespace", namespace)

	var ks kustomizev1.Kustomization
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := h.client.Get(ctx, key, &ks); err != nil {
		return fmt.Errorf("getting kustomization: %w", err)
	}

	if ks.Spec.Suspend {
		logger.Debug("Kustomization already suspended", "name", name)
		return nil // Already suspended
	}

	// Use Update instead of Patch to ensure the change is applied
	ks.Spec.Suspend = true

	if err := h.client.Update(ctx, &ks); err != nil {
		return fmt.Errorf("suspending kustomization: %w", err)
	}

	logger.Info("suspended Kustomization", "name", name, "namespace", namespace)
	return nil
}

// ResumeKustomization resumes a Flux Kustomization
func (h *Helper) ResumeKustomization(ctx context.Context, name, namespace string) error {
	logger := logging.FromContext(ctx)
	logger.Info("resuming Kustomization", "name", name, "namespace", namespace)

	var ks kustomizev1.Kustomization
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := h.client.Get(ctx, key, &ks); err != nil {
		logger.Error("failed to get Kustomization for resume", "error", err, "name", name, "namespace", namespace)
		return fmt.Errorf("getting kustomization: %w", err)
	}

	logger.Info("current Kustomization suspend state", "name", name, "suspend", ks.Spec.Suspend)

	if !ks.Spec.Suspend {
		logger.Info("Kustomization already resumed", "name", name)
		return nil // Already resumed
	}

	// Use Update instead of Patch to ensure the change is applied
	ks.Spec.Suspend = false

	if err := h.client.Update(ctx, &ks); err != nil {
		logger.Error("failed to update Kustomization to resume", "error", err, "name", name, "namespace", namespace)
		return fmt.Errorf("resuming kustomization: %w", err)
	}

	logger.Info("successfully resumed Kustomization", "name", name, "namespace", namespace)

	// Verify the patch worked by reading back
	var ksVerify kustomizev1.Kustomization
	if err := h.client.Get(ctx, key, &ksVerify); err != nil {
		logger.Error("failed to verify Kustomization resume", "error", err)
	} else {
		logger.Info("verified Kustomization state after resume", "suspend", ksVerify.Spec.Suspend)
	}

	return nil
}

// IsSuspended checks if a Kustomization is currently suspended
func (h *Helper) IsSuspended(ctx context.Context, name, namespace string) (bool, error) {
	var ks kustomizev1.Kustomization
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := h.client.Get(ctx, key, &ks); err != nil {
		return false, fmt.Errorf("getting kustomization: %w", err)
	}

	return ks.Spec.Suspend, nil
}

// WaitForReconciliation waits for Kustomization to reconcile after resuming
func (h *Helper) WaitForReconciliation(ctx context.Context, name, namespace string, timeout time.Duration) error {
	logger := logging.FromContext(ctx)
	logger.Debug("waiting for Kustomization reconciliation", "name", name, "namespace", namespace, "timeout", timeout)

	deadline := time.Now().Add(timeout)
	key := types.NamespacedName{Name: name, Namespace: namespace}

	// Get initial state to compare against
	var initial kustomizev1.Kustomization
	if err := h.client.Get(ctx, key, &initial); err != nil {
		return fmt.Errorf("getting initial kustomization state: %w", err)
	}
	initialGeneration := initial.Status.ObservedGeneration

	for time.Now().Before(deadline) {
		var ks kustomizev1.Kustomization
		if err := h.client.Get(ctx, key, &ks); err != nil {
			return fmt.Errorf("getting kustomization: %w", err)
		}

		// Check if reconciliation happened (generation increased or we observe new generation)
		if ks.Status.ObservedGeneration > initialGeneration {
			// Check Ready condition
			for _, cond := range ks.Status.Conditions {
				if cond.Type == "Ready" {
					if cond.Status == "True" {
						logger.Debug("Kustomization reconciliation succeeded", "name", name)
						return nil // Reconciliation succeeded
					}
					if cond.Status == "False" {
						return fmt.Errorf("kustomization reconciliation failed: %s", cond.Message)
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			logger.Debug("waiting for Kustomization reconciliation", "name", name, "observedGen", ks.Status.ObservedGeneration)
		}
	}

	return fmt.Errorf("timeout waiting for kustomization %s/%s reconciliation", namespace, name)
}

// IsReconciled checks if the Kustomization is in Ready state
func (h *Helper) IsReconciled(ctx context.Context, name, namespace string) (bool, error) {
	var ks kustomizev1.Kustomization
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := h.client.Get(ctx, key, &ks); err != nil {
		return false, fmt.Errorf("getting kustomization: %w", err)
	}

	for _, cond := range ks.Status.Conditions {
		if cond.Type == "Ready" {
			return cond.Status == "True", nil
		}
	}

	return false, nil
}

// ManagedByInfo contains information about the parent Kustomization if one exists
type ManagedByInfo struct {
	// IsManaged is true if this Kustomization is managed by another Kustomization
	IsManaged bool
	// ParentName is the name of the parent Kustomization (if managed)
	ParentName string
	// ParentNamespace is the namespace of the parent Kustomization (if managed)
	ParentNamespace string
}

// IsManagedByKustomization checks if a Kustomization is managed by another Kustomization.
// This is detected via owner references and Flux labels.
func (h *Helper) IsManagedByKustomization(ctx context.Context, name, namespace string) (*ManagedByInfo, error) {
	var ks kustomizev1.Kustomization
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := h.client.Get(ctx, key, &ks); err != nil {
		return nil, fmt.Errorf("getting kustomization: %w", err)
	}

	// Check owner references for Kustomization parents
	for _, ref := range ks.OwnerReferences {
		if ref.APIVersion == kustomizev1.GroupVersion.String() && ref.Kind == "Kustomization" {
			return &ManagedByInfo{
				IsManaged:       true,
				ParentName:      ref.Name,
				ParentNamespace: namespace, // Owner refs are same-namespace
			}, nil
		}
	}

	// Check Flux labels that indicate management
	// kustomize.toolkit.fluxcd.io/name and kustomize.toolkit.fluxcd.io/namespace
	if parentName, ok := ks.Labels["kustomize.toolkit.fluxcd.io/name"]; ok {
		parentNS := namespace
		if ns, ok := ks.Labels["kustomize.toolkit.fluxcd.io/namespace"]; ok {
			parentNS = ns
		}
		return &ManagedByInfo{
			IsManaged:       true,
			ParentName:      parentName,
			ParentNamespace: parentNS,
		}, nil
	}

	return &ManagedByInfo{IsManaged: false}, nil
}

// ValidateSuspendTarget validates that the suspend target is appropriate.
// If suspendRef is nil, validates that kustomizationRef is a root (not managed).
// If suspendRef is set, validates that it exists (the user explicitly chose it).
func (h *Helper) ValidateSuspendTarget(ctx context.Context, kustomizationRef, suspendRef *struct{ Name, Namespace string }) error {
	// Determine the actual suspend target
	target := kustomizationRef
	if suspendRef != nil {
		target = suspendRef
	}

	// If no explicit suspendRef, check that kustomizationRef is a root
	if suspendRef == nil {
		managedBy, err := h.IsManagedByKustomization(ctx, kustomizationRef.Name, kustomizationRef.Namespace)
		if err != nil {
			return fmt.Errorf("checking if kustomization is managed: %w", err)
		}

		if managedBy.IsManaged {
			return fmt.Errorf("kustomization '%s/%s' is managed by '%s/%s': set spec.suspendRef to a root Kustomization to prevent the parent from un-suspending the child during operations",
				kustomizationRef.Namespace, kustomizationRef.Name,
				managedBy.ParentNamespace, managedBy.ParentName)
		}
	}

	// Verify the target exists
	var ks kustomizev1.Kustomization
	key := types.NamespacedName{Name: target.Name, Namespace: target.Namespace}
	if err := h.client.Get(ctx, key, &ks); err != nil {
		return fmt.Errorf("suspend target Kustomization '%s/%s' not found: %w", target.Namespace, target.Name, err)
	}

	return nil
}

// VerifyStillSuspended checks that a Kustomization is still suspended.
// Use this before critical operations (PVC delete, Git commit) to catch external un-suspend.
func (h *Helper) VerifyStillSuspended(ctx context.Context, name, namespace string) error {
	suspended, err := h.IsSuspended(ctx, name, namespace)
	if err != nil {
		return fmt.Errorf("checking suspend state: %w", err)
	}

	if !suspended {
		return fmt.Errorf("kustomization '%s/%s' was un-suspended externally: ensure no parent Kustomization or external automation manages the suspend state", namespace, name)
	}

	return nil
}
