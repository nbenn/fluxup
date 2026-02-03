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
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHelper_SuspendKustomization(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ks",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: false,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ks).
		Build()

	helper := NewHelper(client)
	ctx := context.Background()

	// Suspend
	err := helper.SuspendKustomization(ctx, "test-ks", "flux-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify suspended
	var result kustomizev1.Kustomization
	err = client.Get(ctx, types.NamespacedName{Name: "test-ks", Namespace: "flux-system"}, &result)
	if err != nil {
		t.Fatalf("failed to get kustomization: %v", err)
	}

	if !result.Spec.Suspend {
		t.Error("expected Kustomization to be suspended")
	}
}

func TestHelper_ResumeKustomization(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ks",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: true,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ks).
		Build()

	helper := NewHelper(client)
	ctx := context.Background()

	// Resume
	err := helper.ResumeKustomization(ctx, "test-ks", "flux-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify resumed
	var result kustomizev1.Kustomization
	err = client.Get(ctx, types.NamespacedName{Name: "test-ks", Namespace: "flux-system"}, &result)
	if err != nil {
		t.Fatalf("failed to get kustomization: %v", err)
	}

	if result.Spec.Suspend {
		t.Error("expected Kustomization to be resumed (not suspended)")
	}
}

func TestHelper_IsSuspended(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	tests := []struct {
		name     string
		suspend  bool
		expected bool
	}{
		{"suspended", true, true},
		{"not suspended", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ks := &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ks",
					Namespace: "flux-system",
				},
				Spec: kustomizev1.KustomizationSpec{
					Suspend: tt.suspend,
				},
			}

			client := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(ks).
				Build()

			helper := NewHelper(client)
			ctx := context.Background()

			result, err := helper.IsSuspended(ctx, "test-ks", "flux-system")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result != tt.expected {
				t.Errorf("IsSuspended() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestHelper_IsReconciled(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   bool
	}{
		{
			name: "ready true",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			expected: true,
		},
		{
			name: "ready false",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
			expected: false,
		},
		{
			name:       "no conditions",
			conditions: nil,
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ks := &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ks",
					Namespace: "flux-system",
				},
			}
			ks.Status.Conditions = tt.conditions

			client := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(ks).
				Build()

			helper := NewHelper(client)
			ctx := context.Background()

			result, err := helper.IsReconciled(ctx, "test-ks", "flux-system")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result != tt.expected {
				t.Errorf("IsReconciled() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestHelper_SuspendKustomization_NotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	helper := NewHelper(client)
	ctx := context.Background()

	err := helper.SuspendKustomization(ctx, "nonexistent", "flux-system")
	if err == nil {
		t.Error("expected error for nonexistent Kustomization")
	}
}

func TestHelper_SuspendKustomization_AlreadySuspended(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ks",
			Namespace: "flux-system",
		},
		Spec: kustomizev1.KustomizationSpec{
			Suspend: true, // Already suspended
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ks).
		Build()

	helper := NewHelper(client)
	ctx := context.Background()

	// Should succeed without error (idempotent)
	err := helper.SuspendKustomization(ctx, "test-ks", "flux-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
