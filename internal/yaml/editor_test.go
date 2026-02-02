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

package yaml

import (
	"testing"
)

func TestEditor_UpdateHelmReleaseVersion(t *testing.T) {
	editor := NewEditor()

	input := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: gitea
  namespace: gitea
spec:
  interval: 30m
  chart:
    spec:
      chart: gitea
      version: "10.1.4"
      sourceRef:
        kind: HelmRepository
        name: gitea-charts
        namespace: flux-system
  values:
    replicaCount: 1
`

	expected := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: gitea
  namespace: gitea
spec:
  interval: 30m
  chart:
    spec:
      chart: gitea
      version: "11.0.0"
      sourceRef:
        kind: HelmRepository
        name: gitea-charts
        namespace: flux-system
  values:
    replicaCount: 1
`

	result, err := editor.UpdateHelmReleaseVersion([]byte(input), "11.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(result) != expected {
		t.Errorf("unexpected result:\ngot:\n%s\nwant:\n%s", string(result), expected)
	}
}

func TestEditor_UpdateVersion_CustomPath(t *testing.T) {
	editor := NewEditor()

	input := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  template:
    spec:
      containers:
        - name: app
          image: myapp:v1.0.0
          custom:
            version: "1.0.0"
`

	result, err := editor.UpdateVersion([]byte(input), "spec.template.spec.containers.0.custom.version", "2.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the version was updated
	version, err := editor.GetVersion(result, "spec.template.spec.containers.0.custom.version")
	if err != nil {
		t.Fatalf("failed to get version: %v", err)
	}

	if version != "2.0.0" {
		t.Errorf("expected version 2.0.0, got %s", version)
	}
}

func TestEditor_GetVersion(t *testing.T) {
	editor := NewEditor()

	input := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: gitea
spec:
  chart:
    spec:
      chart: gitea
      version: "10.1.4"
`

	version, err := editor.GetVersion([]byte(input), DefaultHelmReleaseVersionPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if version != "10.1.4" {
		t.Errorf("expected version 10.1.4, got %s", version)
	}
}

func TestEditor_GetVersion_NotFound(t *testing.T) {
	editor := NewEditor()

	input := `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value
`

	_, err := editor.GetVersion([]byte(input), DefaultHelmReleaseVersionPath)
	if err == nil {
		t.Fatal("expected error for missing version path")
	}
}

func TestEditor_UpdateVersion_LeadingDot(t *testing.T) {
	editor := NewEditor()

	input := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
spec:
  chart:
    spec:
      version: "1.0.0"
`

	// Test with leading dot in path
	result, err := editor.UpdateVersion([]byte(input), ".spec.chart.spec.version", "2.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	version, err := editor.GetVersion(result, "spec.chart.spec.version")
	if err != nil {
		t.Fatalf("failed to get version: %v", err)
	}

	if version != "2.0.0" {
		t.Errorf("expected version 2.0.0, got %s", version)
	}
}

func TestDetermineVersionPath(t *testing.T) {
	tests := []struct {
		datasource string
		expected   string
	}{
		{"helm", DefaultHelmReleaseVersionPath},
		{"docker", ""},
		{"unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.datasource, func(t *testing.T) {
			result := DetermineVersionPath(tt.datasource)
			if result != tt.expected {
				t.Errorf("DetermineVersionPath(%s) = %s, want %s", tt.datasource, result, tt.expected)
			}
		})
	}
}
