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

// Package health provides shared health checking functionality for ManagedApps.
// It checks both the Flux resource (Kustomization/HelmRelease) readiness
// and the workload (Deployment/StatefulSet) readiness.
package health

import (
	"context"
	"fmt"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/discovery"
	"github.com/nbenn/fluxup/internal/logging"
)

// DefaultFluxNamespace is the default namespace for Flux resources.
const DefaultFluxNamespace = "flux-system"

// Checker provides health checking functionality for ManagedApps.
type Checker struct {
	client     client.Client
	discoverer *discovery.Discoverer
}

// NewChecker creates a new health checker.
func NewChecker(c client.Client) *Checker {
	return &Checker{
		client:     c,
		discoverer: discovery.New(c),
	}
}

// Result contains the health check result details.
type Result struct {
	// Healthy indicates whether the app is healthy overall.
	Healthy bool

	// FluxReady indicates whether the Flux resource is ready.
	FluxReady bool

	// WorkloadsReady indicates whether all workloads are ready.
	WorkloadsReady bool

	// Message provides additional context about the health status.
	Message string

	// WorkloadStatuses contains per-workload status for debugging.
	WorkloadStatuses []WorkloadStatus
}

// WorkloadStatus contains health status for a single workload.
type WorkloadStatus struct {
	Kind            string
	Name            string
	Namespace       string
	DesiredReplicas int32
	ReadyReplicas   int32
	Ready           bool
}

// CheckHealth performs a comprehensive health check for a ManagedApp.
// It checks both the Flux resource readiness and all workload readiness.
// This is the authoritative health check used by both ManagedAppReconciler (periodic)
// and Upgrade/Rollback controllers (real-time).
func (c *Checker) CheckHealth(ctx context.Context, app *fluxupv1alpha1.ManagedApp) (*Result, error) {
	logger := logging.FromContext(ctx)
	result := &Result{}

	// 1. Check Flux resource readiness (Kustomization)
	fluxReady, fluxMsg, err := c.checkFluxResourceReady(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("checking Flux resource: %w", err)
	}
	result.FluxReady = fluxReady

	if !fluxReady {
		result.Healthy = false
		result.Message = fluxMsg
		return result, nil
	}

	// 2. Discover workloads belonging to this app
	workloads, err := c.discoverer.DiscoverWorkloads(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("discovering workloads: %w", err)
	}

	// If no workloads discovered, app is healthy (flux is ready)
	if len(workloads) == 0 {
		logger.Debug("no workloads discovered, app is healthy based on Flux status")
		result.Healthy = true
		result.WorkloadsReady = true
		result.Message = "Flux resource is ready, no workloads to check"
		return result, nil
	}

	// 3. Check each workload's readiness
	allWorkloadsReady := true
	result.WorkloadStatuses = make([]WorkloadStatus, 0, len(workloads))

	for _, w := range workloads {
		ns := w.Namespace
		if ns == "" {
			ns = app.Namespace
		}

		status, err := c.checkWorkloadReady(ctx, w.Kind, w.Name, ns)
		if err != nil {
			return nil, fmt.Errorf("checking workload %s/%s: %w", ns, w.Name, err)
		}

		result.WorkloadStatuses = append(result.WorkloadStatuses, *status)

		if !status.Ready {
			allWorkloadsReady = false
		}
	}

	result.WorkloadsReady = allWorkloadsReady

	// 4. Determine overall health
	result.Healthy = result.FluxReady && result.WorkloadsReady

	if result.Healthy {
		result.Message = fmt.Sprintf("All checks passed: Flux ready, %d/%d workloads ready",
			len(result.WorkloadStatuses), len(result.WorkloadStatuses))
	} else if !result.WorkloadsReady {
		var notReady int
		for _, ws := range result.WorkloadStatuses {
			if !ws.Ready {
				notReady++
			}
		}
		result.Message = fmt.Sprintf("Flux ready, but %d/%d workloads not ready",
			notReady, len(result.WorkloadStatuses))
	}

	return result, nil
}

// checkFluxResourceReady checks if the Kustomization is ready.
func (c *Checker) checkFluxResourceReady(ctx context.Context, app *fluxupv1alpha1.ManagedApp) (bool, string, error) {
	ref := app.Spec.KustomizationRef
	ns := ref.Namespace
	if ns == "" {
		ns = DefaultFluxNamespace
	}

	var ks kustomizev1.Kustomization
	if err := c.client.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, &ks); err != nil {
		return false, fmt.Sprintf("Kustomization %s/%s not found", ns, ref.Name), err
	}

	// Check for Ready condition
	for _, cond := range ks.Status.Conditions {
		if cond.Type == "Ready" {
			if cond.Status == metav1.ConditionTrue {
				return true, "Kustomization is ready", nil
			}
			return false, fmt.Sprintf("Kustomization not ready: %s", cond.Message), nil
		}
	}

	return false, "Kustomization has no Ready condition", nil
}

// checkWorkloadReady checks if a single Deployment or StatefulSet is ready.
func (c *Checker) checkWorkloadReady(ctx context.Context, kind, name, namespace string) (*WorkloadStatus, error) {
	status := &WorkloadStatus{
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
	}

	key := types.NamespacedName{Name: name, Namespace: namespace}

	switch kind {
	case "Deployment":
		var deploy appsv1.Deployment
		if err := c.client.Get(ctx, key, &deploy); err != nil {
			return nil, fmt.Errorf("getting deployment: %w", err)
		}

		status.DesiredReplicas = 1
		if deploy.Spec.Replicas != nil {
			status.DesiredReplicas = *deploy.Spec.Replicas
		}
		status.ReadyReplicas = deploy.Status.ReadyReplicas

		// Deployment is ready when:
		// - ReadyReplicas >= DesiredReplicas
		// - OR DesiredReplicas is 0 (scaled down intentionally)
		status.Ready = status.DesiredReplicas == 0 ||
			status.ReadyReplicas >= status.DesiredReplicas

	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := c.client.Get(ctx, key, &sts); err != nil {
			return nil, fmt.Errorf("getting statefulset: %w", err)
		}

		status.DesiredReplicas = 1
		if sts.Spec.Replicas != nil {
			status.DesiredReplicas = *sts.Spec.Replicas
		}
		status.ReadyReplicas = sts.Status.ReadyReplicas

		// StatefulSet is ready when:
		// - ReadyReplicas >= DesiredReplicas
		// - OR DesiredReplicas is 0 (scaled down intentionally)
		status.Ready = status.DesiredReplicas == 0 ||
			status.ReadyReplicas >= status.DesiredReplicas

	default:
		return nil, fmt.Errorf("unsupported workload kind: %s", kind)
	}

	return status, nil
}

// IsHealthy is a convenience function that returns just the health status.
func (c *Checker) IsHealthy(ctx context.Context, app *fluxupv1alpha1.ManagedApp) (bool, error) {
	result, err := c.CheckHealth(ctx, app)
	if err != nil {
		return false, err
	}
	return result.Healthy, nil
}
