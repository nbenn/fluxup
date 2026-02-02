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
	var ks kustomizev1.Kustomization
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := h.client.Get(ctx, key, &ks); err != nil {
		return fmt.Errorf("getting kustomization: %w", err)
	}

	if ks.Spec.Suspend {
		return nil // Already suspended
	}

	patch := client.MergeFrom(ks.DeepCopy())
	ks.Spec.Suspend = true

	if err := h.client.Patch(ctx, &ks, patch); err != nil {
		return fmt.Errorf("suspending kustomization: %w", err)
	}

	return nil
}

// ResumeKustomization resumes a Flux Kustomization
func (h *Helper) ResumeKustomization(ctx context.Context, name, namespace string) error {
	var ks kustomizev1.Kustomization
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := h.client.Get(ctx, key, &ks); err != nil {
		return fmt.Errorf("getting kustomization: %w", err)
	}

	if !ks.Spec.Suspend {
		return nil // Already resumed
	}

	patch := client.MergeFrom(ks.DeepCopy())
	ks.Spec.Suspend = false

	if err := h.client.Patch(ctx, &ks, patch); err != nil {
		return fmt.Errorf("resuming kustomization: %w", err)
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
			// Continue polling
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
