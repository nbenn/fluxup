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
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ks).
		Build()

	helper := NewHelper(k8sClient)
	ctx := context.Background()

	// Suspend
	err := helper.SuspendKustomization(ctx, "test-ks", "flux-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify suspended
	var result kustomizev1.Kustomization
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-ks", Namespace: "flux-system"}, &result)
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

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ks).
		Build()

	helper := NewHelper(k8sClient)
	ctx := context.Background()

	// Resume
	err := helper.ResumeKustomization(ctx, "test-ks", "flux-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify resumed
	var result kustomizev1.Kustomization
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-ks", Namespace: "flux-system"}, &result)
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

			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(ks).
				Build()

			helper := NewHelper(k8sClient)
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

			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(ks).
				Build()

			helper := NewHelper(k8sClient)
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

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	helper := NewHelper(k8sClient)
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

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ks).
		Build()

	helper := NewHelper(k8sClient)
	ctx := context.Background()

	// Should succeed without error (idempotent)
	err := helper.SuspendKustomization(ctx, "test-ks", "flux-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHelper_IsManagedByKustomization(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	tests := []struct {
		name             string
		kustomization    *kustomizev1.Kustomization
		expectManaged    bool
		expectParentName string
		expectParentNS   string
	}{
		{
			name: "not managed - no owner refs or labels",
			kustomization: &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "root-ks",
					Namespace: "flux-system",
				},
			},
			expectManaged: false,
		},
		{
			name: "managed via owner reference",
			kustomization: &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-ks",
					Namespace: "flux-system",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: kustomizev1.GroupVersion.String(),
							Kind:       "Kustomization",
							Name:       "parent-ks",
						},
					},
				},
			},
			expectManaged:    true,
			expectParentName: "parent-ks",
			expectParentNS:   "flux-system",
		},
		{
			name: "managed via Flux labels - same namespace",
			kustomization: &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-ks",
					Namespace: "apps",
					Labels: map[string]string{
						"kustomize.toolkit.fluxcd.io/name": "parent-ks",
					},
				},
			},
			expectManaged:    true,
			expectParentName: "parent-ks",
			expectParentNS:   "apps",
		},
		{
			name: "managed via Flux labels - cross namespace",
			kustomization: &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-ks",
					Namespace: "apps",
					Labels: map[string]string{
						"kustomize.toolkit.fluxcd.io/name":      "parent-ks",
						"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
					},
				},
			},
			expectManaged:    true,
			expectParentName: "parent-ks",
			expectParentNS:   "flux-system",
		},
		{
			name: "owner ref takes precedence over labels",
			kustomization: &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-ks",
					Namespace: "flux-system",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: kustomizev1.GroupVersion.String(),
							Kind:       "Kustomization",
							Name:       "owner-ks",
						},
					},
					Labels: map[string]string{
						"kustomize.toolkit.fluxcd.io/name": "label-ks",
					},
				},
			},
			expectManaged:    true,
			expectParentName: "owner-ks",
			expectParentNS:   "flux-system",
		},
		{
			name: "non-kustomization owner ref ignored",
			kustomization: &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-ks",
					Namespace: "flux-system",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "some-cm",
						},
					},
				},
			},
			expectManaged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.kustomization).
				Build()

			helper := NewHelper(k8sClient)
			ctx := context.Background()

			info, err := helper.IsManagedByKustomization(ctx, tt.kustomization.Name, tt.kustomization.Namespace)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if info.IsManaged != tt.expectManaged {
				t.Errorf("IsManaged = %v, want %v", info.IsManaged, tt.expectManaged)
			}

			if tt.expectManaged {
				if info.ParentName != tt.expectParentName {
					t.Errorf("ParentName = %v, want %v", info.ParentName, tt.expectParentName)
				}
				if info.ParentNamespace != tt.expectParentNS {
					t.Errorf("ParentNamespace = %v, want %v", info.ParentNamespace, tt.expectParentNS)
				}
			}
		})
	}
}

func TestHelper_IsManagedByKustomization_NotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	helper := NewHelper(k8sClient)
	ctx := context.Background()

	_, err := helper.IsManagedByKustomization(ctx, "nonexistent", "flux-system")
	if err == nil {
		t.Error("expected error for nonexistent Kustomization")
	}
}

func TestHelper_ValidateSuspendTarget(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	tests := []struct {
		name             string
		kustomizations   []kustomizev1.Kustomization
		kustomizationRef *struct{ Name, Namespace string }
		suspendRef       *struct{ Name, Namespace string }
		expectError      bool
		errorContains    string
	}{
		{
			name: "root kustomization - no suspendRef needed",
			kustomizations: []kustomizev1.Kustomization{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "root-ks",
						Namespace: "flux-system",
					},
				},
			},
			kustomizationRef: &struct{ Name, Namespace string }{"root-ks", "flux-system"},
			suspendRef:       nil,
			expectError:      false,
		},
		{
			name: "managed kustomization - no suspendRef specified",
			kustomizations: []kustomizev1.Kustomization{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "child-ks",
						Namespace: "flux-system",
						Labels: map[string]string{
							"kustomize.toolkit.fluxcd.io/name":      "parent-ks",
							"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
						},
					},
				},
			},
			kustomizationRef: &struct{ Name, Namespace string }{"child-ks", "flux-system"},
			suspendRef:       nil,
			expectError:      true,
			errorContains:    "is managed by",
		},
		{
			name: "managed kustomization - explicit suspendRef provided and exists",
			kustomizations: []kustomizev1.Kustomization{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "child-ks",
						Namespace: "apps",
						Labels: map[string]string{
							"kustomize.toolkit.fluxcd.io/name":      "parent-ks",
							"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "parent-ks",
						Namespace: "flux-system",
					},
				},
			},
			kustomizationRef: &struct{ Name, Namespace string }{"child-ks", "apps"},
			suspendRef:       &struct{ Name, Namespace string }{"parent-ks", "flux-system"},
			expectError:      false,
		},
		{
			name: "suspendRef specified but does not exist",
			kustomizations: []kustomizev1.Kustomization{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "child-ks",
						Namespace: "flux-system",
					},
				},
			},
			kustomizationRef: &struct{ Name, Namespace string }{"child-ks", "flux-system"},
			suspendRef:       &struct{ Name, Namespace string }{"nonexistent", "flux-system"},
			expectError:      true,
			errorContains:    "not found",
		},
		{
			name:             "kustomizationRef does not exist",
			kustomizations:   []kustomizev1.Kustomization{},
			kustomizationRef: &struct{ Name, Namespace string }{"nonexistent", "flux-system"},
			suspendRef:       nil,
			expectError:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]client.Object, len(tt.kustomizations))
			for i := range tt.kustomizations {
				objs[i] = &tt.kustomizations[i]
			}

			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				Build()

			helper := NewHelper(k8sClient)
			ctx := context.Background()

			err := helper.ValidateSuspendTarget(ctx, tt.kustomizationRef, tt.suspendRef)

			if tt.expectError {
				if err == nil {
					t.Error("expected error but got none")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("error = %v, want to contain %q", err, tt.errorContains)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestHelper_VerifyStillSuspended(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	tests := []struct {
		name          string
		kustomization *kustomizev1.Kustomization
		expectError   bool
		errorContains string
	}{
		{
			name: "still suspended",
			kustomization: &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ks",
					Namespace: "flux-system",
				},
				Spec: kustomizev1.KustomizationSpec{
					Suspend: true,
				},
			},
			expectError: false,
		},
		{
			name: "was un-suspended externally",
			kustomization: &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ks",
					Namespace: "flux-system",
				},
				Spec: kustomizev1.KustomizationSpec{
					Suspend: false,
				},
			},
			expectError:   true,
			errorContains: "was un-suspended externally",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.kustomization).
				Build()

			helper := NewHelper(k8sClient)
			ctx := context.Background()

			err := helper.VerifyStillSuspended(ctx, tt.kustomization.Name, tt.kustomization.Namespace)

			if tt.expectError {
				if err == nil {
					t.Error("expected error but got none")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("error = %v, want to contain %q", err, tt.errorContains)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestHelper_VerifyStillSuspended_NotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	helper := NewHelper(k8sClient)
	ctx := context.Background()

	err := helper.VerifyStillSuspended(ctx, "nonexistent", "flux-system")
	if err == nil {
		t.Error("expected error for nonexistent Kustomization")
	}
}

// Helper function for string contains check
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
