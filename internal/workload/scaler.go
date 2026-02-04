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

package workload

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nbenn/fluxup/internal/logging"
)

// Workload kind constants
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
)

// Scaler handles workload scaling operations
type Scaler struct {
	client client.Client
}

// NewScaler creates a new workload scaler
func NewScaler(c client.Client) *Scaler {
	return &Scaler{client: c}
}

// ScaleInfo holds information about a scaled workload
type ScaleInfo struct {
	Kind             string
	Name             string
	Namespace        string
	OriginalReplicas int32
}

// ScaleDown scales a workload to 0 replicas.
// Returns the original replica count for later restoration.
func (s *Scaler) ScaleDown(ctx context.Context, kind, name, namespace string) (*ScaleInfo, error) {
	logger := logging.FromContext(ctx)
	logger.Debug("scaling down workload", "kind", kind, "name", name, "namespace", namespace)

	switch kind {
	case KindDeployment:
		return s.scaleDeployment(ctx, name, namespace, 0)
	case KindStatefulSet:
		return s.scaleStatefulSet(ctx, name, namespace, 0)
	default:
		return nil, fmt.Errorf("unsupported workload kind: %s", kind)
	}
}

// ScaleUp scales a workload to the specified replica count
func (s *Scaler) ScaleUp(ctx context.Context, kind, name, namespace string, replicas int32) error {
	logger := logging.FromContext(ctx)
	logger.Debug("scaling up workload", "kind", kind, "name", name, "namespace", namespace, "replicas", replicas)

	switch kind {
	case KindDeployment:
		_, err := s.scaleDeployment(ctx, name, namespace, replicas)
		return err
	case KindStatefulSet:
		_, err := s.scaleStatefulSet(ctx, name, namespace, replicas)
		return err
	default:
		return fmt.Errorf("unsupported workload kind: %s", kind)
	}
}

func (s *Scaler) scaleDeployment(ctx context.Context, name, namespace string, replicas int32) (*ScaleInfo, error) {
	var deploy appsv1.Deployment
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := s.client.Get(ctx, key, &deploy); err != nil {
		return nil, fmt.Errorf("getting deployment: %w", err)
	}

	originalReplicas := int32(1)
	if deploy.Spec.Replicas != nil {
		originalReplicas = *deploy.Spec.Replicas
	}

	// Don't scale if already at target
	if deploy.Spec.Replicas != nil && *deploy.Spec.Replicas == replicas {
		return &ScaleInfo{
			Kind:             "Deployment",
			Name:             name,
			Namespace:        namespace,
			OriginalReplicas: originalReplicas,
		}, nil
	}

	patch := client.MergeFrom(deploy.DeepCopy())
	deploy.Spec.Replicas = &replicas

	if err := s.client.Patch(ctx, &deploy, patch); err != nil {
		return nil, fmt.Errorf("scaling deployment: %w", err)
	}

	logger := logging.FromContext(ctx)
	logger.Info("scaled deployment", "name", name, "from", originalReplicas, "to", replicas)

	return &ScaleInfo{
		Kind:             "Deployment",
		Name:             name,
		Namespace:        namespace,
		OriginalReplicas: originalReplicas,
	}, nil
}

func (s *Scaler) scaleStatefulSet(ctx context.Context, name, namespace string, replicas int32) (*ScaleInfo, error) {
	var sts appsv1.StatefulSet
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := s.client.Get(ctx, key, &sts); err != nil {
		return nil, fmt.Errorf("getting statefulset: %w", err)
	}

	originalReplicas := int32(1)
	if sts.Spec.Replicas != nil {
		originalReplicas = *sts.Spec.Replicas
	}

	// Don't scale if already at target
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == replicas {
		return &ScaleInfo{
			Kind:             "StatefulSet",
			Name:             name,
			Namespace:        namespace,
			OriginalReplicas: originalReplicas,
		}, nil
	}

	patch := client.MergeFrom(sts.DeepCopy())
	sts.Spec.Replicas = &replicas

	if err := s.client.Patch(ctx, &sts, patch); err != nil {
		return nil, fmt.Errorf("scaling statefulset: %w", err)
	}

	logger := logging.FromContext(ctx)
	logger.Info("scaled statefulset", "name", name, "from", originalReplicas, "to", replicas)

	return &ScaleInfo{
		Kind:             "StatefulSet",
		Name:             name,
		Namespace:        namespace,
		OriginalReplicas: originalReplicas,
	}, nil
}

// WaitForScaleDown waits for all pods of the workload to terminate
func (s *Scaler) WaitForScaleDown(ctx context.Context, kind, name, namespace string, timeout time.Duration) error {
	logger := logging.FromContext(ctx)
	logger.Debug("waiting for workload scale down", "kind", kind, "name", name, "timeout", timeout)

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		ready, err := s.getReadyReplicas(ctx, kind, name, namespace)
		if err != nil {
			return err
		}

		if ready == 0 {
			// Also verify no pods are still running/terminating
			pods, err := s.getWorkloadPods(ctx, kind, name, namespace)
			if err != nil {
				return err
			}

			if len(pods) == 0 {
				logger.Debug("workload scaled down, no pods remaining")
				return nil
			}

			// Pods still exist but may be terminating
			logger.Debug("waiting for pods to terminate", "remaining", len(pods))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			// Continue polling
		}
	}

	return fmt.Errorf("timeout waiting for workload %s/%s to scale down", namespace, name)
}

// WaitForScaleUp waits for the workload to reach the target replica count
func (s *Scaler) WaitForScaleUp(ctx context.Context, kind, name, namespace string, targetReplicas int32, timeout time.Duration) error {
	logger := logging.FromContext(ctx)
	logger.Debug("waiting for workload scale up", "kind", kind, "name", name, "target", targetReplicas, "timeout", timeout)

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		ready, err := s.getReadyReplicas(ctx, kind, name, namespace)
		if err != nil {
			return err
		}

		if ready >= targetReplicas {
			logger.Debug("workload scaled up", "ready", ready)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			logger.Debug("waiting for replicas", "ready", ready, "target", targetReplicas)
		}
	}

	return fmt.Errorf("timeout waiting for workload %s/%s to scale up to %d replicas", namespace, name, targetReplicas)
}

func (s *Scaler) getReadyReplicas(ctx context.Context, kind, name, namespace string) (int32, error) {
	key := types.NamespacedName{Name: name, Namespace: namespace}

	switch kind {
	case KindDeployment:
		var deploy appsv1.Deployment
		if err := s.client.Get(ctx, key, &deploy); err != nil {
			return 0, fmt.Errorf("getting deployment: %w", err)
		}
		return deploy.Status.ReadyReplicas, nil
	case KindStatefulSet:
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, key, &sts); err != nil {
			return 0, fmt.Errorf("getting statefulset: %w", err)
		}
		return sts.Status.ReadyReplicas, nil
	default:
		return 0, fmt.Errorf("unsupported workload kind: %s", kind)
	}
}

func (s *Scaler) getWorkloadPods(ctx context.Context, kind, name, namespace string) ([]corev1.Pod, error) {
	key := types.NamespacedName{Name: name, Namespace: namespace}

	var selector labels.Selector

	switch kind {
	case KindDeployment:
		var deploy appsv1.Deployment
		if err := s.client.Get(ctx, key, &deploy); err != nil {
			return nil, fmt.Errorf("getting deployment: %w", err)
		}
		if deploy.Spec.Selector != nil {
			var err error
			selector, err = labels.ValidatedSelectorFromSet(deploy.Spec.Selector.MatchLabels)
			if err != nil {
				return nil, fmt.Errorf("parsing deployment selector: %w", err)
			}
		}
	case KindStatefulSet:
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, key, &sts); err != nil {
			return nil, fmt.Errorf("getting statefulset: %w", err)
		}
		if sts.Spec.Selector != nil {
			var err error
			selector, err = labels.ValidatedSelectorFromSet(sts.Spec.Selector.MatchLabels)
			if err != nil {
				return nil, fmt.Errorf("parsing statefulset selector: %w", err)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported workload kind: %s", kind)
	}

	if selector == nil {
		return nil, nil
	}

	var podList corev1.PodList
	if err := s.client.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	return podList.Items, nil
}

// GetCurrentReplicas returns the current replica count for a workload
func (s *Scaler) GetCurrentReplicas(ctx context.Context, kind, name, namespace string) (int32, error) {
	key := types.NamespacedName{Name: name, Namespace: namespace}

	switch kind {
	case KindDeployment:
		var deploy appsv1.Deployment
		if err := s.client.Get(ctx, key, &deploy); err != nil {
			return 0, fmt.Errorf("getting deployment: %w", err)
		}
		if deploy.Spec.Replicas == nil {
			return 1, nil // Default is 1
		}
		return *deploy.Spec.Replicas, nil
	case KindStatefulSet:
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, key, &sts); err != nil {
			return 0, fmt.Errorf("getting statefulset: %w", err)
		}
		if sts.Spec.Replicas == nil {
			return 1, nil // Default is 1
		}
		return *sts.Spec.Replicas, nil
	default:
		return 0, fmt.Errorf("unsupported workload kind: %s", kind)
	}
}

// IsScaledDown checks if a workload is scaled down (0 replicas and no pods running).
// This is a non-blocking check for use in reconciliation loops.
func (s *Scaler) IsScaledDown(ctx context.Context, kind, name, namespace string) (bool, error) {
	ready, err := s.getReadyReplicas(ctx, kind, name, namespace)
	if err != nil {
		return false, err
	}

	if ready > 0 {
		return false, nil
	}

	// Also verify no pods are still running/terminating
	pods, err := s.getWorkloadPods(ctx, kind, name, namespace)
	if err != nil {
		return false, err
	}

	return len(pods) == 0, nil
}
