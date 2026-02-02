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
	"fmt"
	"strings"

	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// DefaultHelmReleaseVersionPath is the standard path for chart version in HelmRelease
const DefaultHelmReleaseVersionPath = "spec.chart.spec.version"

// Editor modifies YAML content while preserving structure and comments
type Editor struct{}

// NewEditor creates a new YAML editor
func NewEditor() *Editor {
	return &Editor{}
}

// UpdateVersion updates the version at the given path in YAML content
func (e *Editor) UpdateVersion(content []byte, versionPath, newVersion string) ([]byte, error) {
	obj, err := yaml.Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	// Convert dot-separated path to slice
	pathParts := parseVersionPath(versionPath)

	// Navigate to the field and update it
	if err := obj.PipeE(
		yaml.LookupCreate(yaml.ScalarNode, pathParts...),
		yaml.FieldSetter{StringValue: newVersion},
	); err != nil {
		return nil, fmt.Errorf("setting version at path %s: %w", versionPath, err)
	}

	result, err := obj.String()
	if err != nil {
		return nil, fmt.Errorf("serializing YAML: %w", err)
	}

	return []byte(result), nil
}

// UpdateHelmReleaseVersion updates the chart version in a HelmRelease
// Uses the default path: spec.chart.spec.version
func (e *Editor) UpdateHelmReleaseVersion(content []byte, newVersion string) ([]byte, error) {
	return e.UpdateVersion(content, DefaultHelmReleaseVersionPath, newVersion)
}

// GetVersion reads the version at the given path in YAML content
func (e *Editor) GetVersion(content []byte, versionPath string) (string, error) {
	obj, err := yaml.Parse(string(content))
	if err != nil {
		return "", fmt.Errorf("parsing YAML: %w", err)
	}

	pathParts := parseVersionPath(versionPath)

	// Navigate to the field
	node, err := obj.Pipe(yaml.Lookup(pathParts...))
	if err != nil {
		return "", fmt.Errorf("looking up path %s: %w", versionPath, err)
	}

	if node == nil {
		return "", fmt.Errorf("version not found at path: %s", versionPath)
	}

	value := yaml.GetValue(node)
	if value == "" {
		return "", fmt.Errorf("empty version at path: %s", versionPath)
	}

	return value, nil
}

// parseVersionPath converts a dot-separated path (possibly with leading dot) to a slice
// Examples:
//   - ".spec.chart.spec.version" -> ["spec", "chart", "spec", "version"]
//   - "spec.chart.spec.version" -> ["spec", "chart", "spec", "version"]
func parseVersionPath(path string) []string {
	// Remove leading dot if present
	path = strings.TrimPrefix(path, ".")
	return strings.Split(path, ".")
}

// DetermineVersionPath returns the appropriate version path for a given datasource
// For HelmRelease, returns the standard chart version path
// For other types, returns empty string (must be specified explicitly)
func DetermineVersionPath(datasource string) string {
	switch datasource {
	case "helm":
		return DefaultHelmReleaseVersionPath
	default:
		return ""
	}
}
