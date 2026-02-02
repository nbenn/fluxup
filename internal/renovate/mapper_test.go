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

package renovate

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

func TestMapUpdatesToManagedApps(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = fluxupv1alpha1.AddToScheme(scheme)

	// Create test ManagedApps
	apps := []fluxupv1alpha1.ManagedApp{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gitea",
				Namespace: "default",
			},
			Spec: fluxupv1alpha1.ManagedAppSpec{
				GitPath: "flux/apps/gitea/helmrelease.yaml",
				KustomizationRef: fluxupv1alpha1.ObjectReference{
					Name: "apps",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bazarr",
				Namespace: "default",
			},
			Spec: fluxupv1alpha1.ManagedAppSpec{
				GitPath: "flux/apps/bazarr/deployment.yaml",
				KustomizationRef: fluxupv1alpha1.ObjectReference{
					Name: "apps",
				},
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&apps[0], &apps[1]).
		Build()

	mapper := NewMapper(client)

	tests := []struct {
		name         string
		updates      []UpdateInfo
		expectedApps int
		checkFunc    func(t *testing.T, result map[types.NamespacedName]*fluxupv1alpha1.VersionInfo)
	}{
		{
			name:         "no updates",
			updates:      []UpdateInfo{},
			expectedApps: 0,
		},
		{
			name: "helm update matches gitea",
			updates: []UpdateInfo{
				{
					PackageFile:    "flux/apps/gitea/helmrelease.yaml",
					DependencyName: "gitea",
					CurrentVersion: "10.0.0",
					NewVersion:     "10.1.0",
					Datasource:     "helm",
				},
			},
			expectedApps: 1,
			checkFunc: func(t *testing.T, result map[types.NamespacedName]*fluxupv1alpha1.VersionInfo) {
				key := types.NamespacedName{Name: "gitea", Namespace: "default"}
				if result[key] == nil {
					t.Fatal("expected gitea to have update")
				}
				if result[key].Chart != "10.1.0" {
					t.Errorf("Chart = %q, want %q", result[key].Chart, "10.1.0")
				}
			},
		},
		{
			name: "docker update matches bazarr",
			updates: []UpdateInfo{
				{
					PackageFile:    "flux/apps/bazarr/deployment.yaml",
					DependencyName: "linuxserver/bazarr",
					CurrentVersion: "1.2.0",
					NewVersion:     "1.3.0",
					Datasource:     "docker",
				},
			},
			expectedApps: 1,
			checkFunc: func(t *testing.T, result map[types.NamespacedName]*fluxupv1alpha1.VersionInfo) {
				key := types.NamespacedName{Name: "bazarr", Namespace: "default"}
				if result[key] == nil {
					t.Fatal("expected bazarr to have update")
				}
				if len(result[key].Images) != 1 {
					t.Fatalf("expected 1 image, got %d", len(result[key].Images))
				}
				if result[key].Images[0].Name != "linuxserver/bazarr" {
					t.Errorf("Image name = %q, want %q", result[key].Images[0].Name, "linuxserver/bazarr")
				}
				if result[key].Images[0].Tag != "1.3.0" {
					t.Errorf("Image tag = %q, want %q", result[key].Images[0].Tag, "1.3.0")
				}
			},
		},
		{
			name: "update for unknown path is ignored",
			updates: []UpdateInfo{
				{
					PackageFile:    "flux/apps/unknown/helmrelease.yaml",
					DependencyName: "unknown",
					CurrentVersion: "1.0.0",
					NewVersion:     "2.0.0",
					Datasource:     "helm",
				},
			},
			expectedApps: 0,
		},
		{
			name: "multiple docker updates for same app",
			updates: []UpdateInfo{
				{
					PackageFile:    "flux/apps/bazarr/deployment.yaml",
					DependencyName: "linuxserver/bazarr",
					CurrentVersion: "1.2.0",
					NewVersion:     "1.3.0",
					Datasource:     "docker",
				},
				{
					PackageFile:    "flux/apps/bazarr/deployment.yaml",
					DependencyName: "redis",
					CurrentVersion: "7.0",
					NewVersion:     "7.2",
					Datasource:     "docker",
				},
			},
			expectedApps: 1,
			checkFunc: func(t *testing.T, result map[types.NamespacedName]*fluxupv1alpha1.VersionInfo) {
				key := types.NamespacedName{Name: "bazarr", Namespace: "default"}
				if result[key] == nil {
					t.Fatal("expected bazarr to have update")
				}
				if len(result[key].Images) != 2 {
					t.Errorf("expected 2 images, got %d", len(result[key].Images))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := mapper.MapUpdatesToManagedApps(context.Background(), tt.updates)
			if err != nil {
				t.Fatalf("MapUpdatesToManagedApps() error = %v", err)
			}
			if len(result) != tt.expectedApps {
				t.Errorf("got %d apps, want %d", len(result), tt.expectedApps)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, result)
			}
		})
	}
}
