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
)

func TestExtractUpdates(t *testing.T) {
	tests := []struct {
		name     string
		config   *RenovateConfig
		expected int
	}{
		{
			name:     "nil config",
			config:   nil,
			expected: 0,
		},
		{
			name:     "empty config",
			config:   &RenovateConfig{},
			expected: 0,
		},
		{
			name: "flux updates only",
			config: &RenovateConfig{
				Flux: []PackageFile{
					{
						PackageFile: "flux/apps/gitea/helmrelease.yaml",
						Deps: []Dependency{
							{
								DepName:      "gitea",
								CurrentValue: "10.0.0",
								Datasource:   DatasourceHelm,
								Updates: []Update{
									{NewValue: "10.1.0", UpdateType: "minor"},
								},
							},
						},
					},
				},
			},
			expected: 1,
		},
		{
			name: "multiple updates across managers",
			config: &RenovateConfig{
				Flux: []PackageFile{
					{
						PackageFile: "flux/apps/gitea/helmrelease.yaml",
						Deps: []Dependency{
							{
								DepName:      "gitea",
								CurrentValue: "10.0.0",
								Datasource:   DatasourceHelm,
								Updates: []Update{
									{NewValue: "10.1.0", UpdateType: "minor"},
								},
							},
						},
					},
				},
				HelmValues: []PackageFile{
					{
						PackageFile: "flux/apps/gitea/helmrelease.yaml",
						Deps: []Dependency{
							{
								DepName:      "postgres",
								CurrentValue: "15.0",
								Datasource:   DatasourceDocker,
								Updates: []Update{
									{NewValue: "15.1", UpdateType: "patch"},
								},
							},
						},
					},
				},
				Regex: []PackageFile{
					{
						PackageFile: "flux/apps/bazarr/deployment.yaml",
						Deps: []Dependency{
							{
								DepName:      "linuxserver/bazarr",
								CurrentValue: "1.2.0",
								Datasource:   DatasourceDocker,
								Updates: []Update{
									{NewValue: "1.3.0", UpdateType: "minor"},
								},
							},
						},
					},
				},
			},
			expected: 3,
		},
		{
			name: "deps without updates are ignored",
			config: &RenovateConfig{
				Flux: []PackageFile{
					{
						PackageFile: "flux/apps/gitea/helmrelease.yaml",
						Deps: []Dependency{
							{
								DepName:      "gitea",
								CurrentValue: "10.0.0",
								Datasource:   DatasourceHelm,
								Updates:      []Update{}, // No updates
							},
							{
								DepName:      "redis",
								CurrentValue: "7.0.0",
								Datasource:   DatasourceHelm,
								Updates: []Update{
									{NewValue: "7.1.0", UpdateType: "minor"},
								},
							},
						},
					},
				},
			},
			expected: 1, // Only redis has updates
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Parser{}
			var updates []UpdateInfo
			if tt.config != nil {
				updates = p.extractUpdates(context.Background(), tt.config)
			}
			if len(updates) != tt.expected {
				t.Errorf("extractUpdates() got %d updates, want %d", len(updates), tt.expected)
			}
		})
	}
}

func TestUpdateInfoFields(t *testing.T) {
	config := &RenovateConfig{
		Flux: []PackageFile{
			{
				PackageFile: "flux/apps/gitea/helmrelease.yaml",
				Deps: []Dependency{
					{
						DepName:      "gitea",
						CurrentValue: "10.0.0",
						Datasource:   DatasourceHelm,
						Updates: []Update{
							{NewValue: "10.1.0", UpdateType: "minor"},
						},
					},
				},
			},
		},
	}

	p := &Parser{}
	updates := p.extractUpdates(context.Background(), config)

	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}

	u := updates[0]
	if u.PackageFile != "flux/apps/gitea/helmrelease.yaml" {
		t.Errorf("PackageFile = %q, want %q", u.PackageFile, "flux/apps/gitea/helmrelease.yaml")
	}
	if u.DependencyName != "gitea" {
		t.Errorf("DependencyName = %q, want %q", u.DependencyName, "gitea")
	}
	if u.CurrentVersion != "10.0.0" {
		t.Errorf("CurrentVersion = %q, want %q", u.CurrentVersion, "10.0.0")
	}
	if u.NewVersion != "10.1.0" {
		t.Errorf("NewVersion = %q, want %q", u.NewVersion, "10.1.0")
	}
	if u.UpdateType != "minor" {
		t.Errorf("UpdateType = %q, want %q", u.UpdateType, "minor")
	}
	if u.Datasource != DatasourceHelm {
		t.Errorf("Datasource = %q, want %q", u.Datasource, DatasourceHelm)
	}
}
