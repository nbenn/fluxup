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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestScaleDown_Deployment(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)

	replicas := int32(3)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deploy).
		Build()

	scaler := NewScaler(client)
	ctx := context.Background()

	info, err := scaler.ScaleDown(ctx, KindDeployment, "test-deploy", "default")
	if err != nil {
		t.Fatalf("ScaleDown failed: %v", err)
	}

	if info.OriginalReplicas != 3 {
		t.Errorf("expected original replicas 3, got %d", info.OriginalReplicas)
	}

	if info.Kind != KindDeployment {
		t.Errorf("expected kind %s, got %s", KindDeployment, info.Kind)
	}

	// Verify the deployment was scaled
	currentReplicas, err := scaler.GetCurrentReplicas(ctx, KindDeployment, "test-deploy", "default")
	if err != nil {
		t.Fatalf("GetCurrentReplicas failed: %v", err)
	}

	if currentReplicas != 0 {
		t.Errorf("expected current replicas 0, got %d", currentReplicas)
	}
}

func TestScaleDown_StatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)

	replicas := int32(2)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sts",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sts).
		Build()

	scaler := NewScaler(client)
	ctx := context.Background()

	info, err := scaler.ScaleDown(ctx, KindStatefulSet, "test-sts", "default")
	if err != nil {
		t.Fatalf("ScaleDown failed: %v", err)
	}

	if info.OriginalReplicas != 2 {
		t.Errorf("expected original replicas 2, got %d", info.OriginalReplicas)
	}

	if info.Kind != KindStatefulSet {
		t.Errorf("expected kind %s, got %s", KindStatefulSet, info.Kind)
	}
}

func TestScaleUp_Deployment(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)

	replicas := int32(0)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deploy).
		Build()

	scaler := NewScaler(client)
	ctx := context.Background()

	err := scaler.ScaleUp(ctx, KindDeployment, "test-deploy", "default", 3)
	if err != nil {
		t.Fatalf("ScaleUp failed: %v", err)
	}

	currentReplicas, err := scaler.GetCurrentReplicas(ctx, KindDeployment, "test-deploy", "default")
	if err != nil {
		t.Fatalf("GetCurrentReplicas failed: %v", err)
	}

	if currentReplicas != 3 {
		t.Errorf("expected current replicas 3, got %d", currentReplicas)
	}
}

func TestScaleDown_UnsupportedKind(t *testing.T) {
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	scaler := NewScaler(client)
	ctx := context.Background()

	_, err := scaler.ScaleDown(ctx, "DaemonSet", "test", "default")
	if err == nil {
		t.Error("expected error for unsupported kind, got nil")
	}
}

func TestScaleDown_AlreadyAtZero(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)

	replicas := int32(0)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deploy).
		Build()

	scaler := NewScaler(client)
	ctx := context.Background()

	info, err := scaler.ScaleDown(ctx, KindDeployment, "test-deploy", "default")
	if err != nil {
		t.Fatalf("ScaleDown failed: %v", err)
	}

	// Should still return correct info even if already at 0
	if info.OriginalReplicas != 0 {
		t.Errorf("expected original replicas 0, got %d", info.OriginalReplicas)
	}
}
