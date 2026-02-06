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
	"log/slog"
	"strings"

	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// DefaultHelmReleaseVersionPath is the standard path for chart version in HelmRelease
const DefaultHelmReleaseVersionPath = "spec.chart.spec.version"

// Editor modifies YAML content while preserving structure and comments
type Editor struct {
	logger *slog.Logger
}

// NewEditor creates a new YAML editor
func NewEditor() *Editor {
	return &Editor{
		logger: slog.Default(),
	}
}

// NewEditorWithLogger creates a new YAML editor with a custom logger
func NewEditorWithLogger(logger *slog.Logger) *Editor {
	return &Editor{
		logger: logger,
	}
}

// UpdateVersion updates the version at the given path in YAML content
func (e *Editor) UpdateVersion(content []byte, versionPath, newVersion string) ([]byte, error) {
	e.logger.Debug("updating version in YAML", "path", versionPath, "newVersion", newVersion)

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

	e.logger.Debug("successfully updated version in YAML", "path", versionPath, "newVersion", newVersion)
	return []byte(result), nil
}

// UpdateHelmReleaseVersion updates the chart version in a HelmRelease
// Uses the default path: spec.chart.spec.version
func (e *Editor) UpdateHelmReleaseVersion(content []byte, newVersion string) ([]byte, error) {
	return e.UpdateVersion(content, DefaultHelmReleaseVersionPath, newVersion)
}

// GetVersion reads the version at the given path in YAML content
func (e *Editor) GetVersion(content []byte, versionPath string) (string, error) {
	e.logger.Debug("reading version from YAML", "path", versionPath)

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

	e.logger.Debug("found version in YAML", "path", versionPath, "version", value)
	return value, nil
}

// parseVersionPath converts a path notation to a slice of path segments
// Supports multiple formats:
//   - Dot notation: ".spec.chart.spec.version" or "spec.chart.spec.version"
//   - Array brackets: "containers[0].image" -> ["containers", "0", "image"]
//   - Map key brackets: "annotations['app.kubernetes.io/version']" -> ["annotations", "app.kubernetes.io/version"]
//
// This allows specifying paths to keys that contain dots (like Kubernetes annotations)
func parseVersionPath(path string) []string {
	// Remove leading dot if present
	path = strings.TrimPrefix(path, ".")

	var parts []string
	var current strings.Builder
	i := 0

	for i < len(path) {
		ch := path[i]

		switch ch {
		case '.':
			// Dot separator - flush current segment
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
			i++

		case '[':
			// Start of bracket notation
			// First, flush any current segment
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}

			// Find the closing bracket
			i++ // skip '['
			if i >= len(path) {
				// Malformed path, just return what we have
				break
			}

			// Check if it's a quoted key (e.g., ['key.with.dots'])
			if path[i] == '\'' || path[i] == '"' {
				quote := path[i]
				i++ // skip opening quote
				for i < len(path) && path[i] != quote {
					current.WriteByte(path[i])
					i++
				}
				if i < len(path) {
					i++ // skip closing quote
				}
			} else {
				// Unquoted bracket content (e.g., array index [0])
				for i < len(path) && path[i] != ']' {
					current.WriteByte(path[i])
					i++
				}
			}

			// Flush the bracket content
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}

			// Skip the closing bracket
			if i < len(path) && path[i] == ']' {
				i++
			}

		default:
			current.WriteByte(ch)
			i++
		}
	}

	// Flush any remaining content
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}

// IsHelmRelease checks if the given YAML content is a HelmRelease resource.
func IsHelmRelease(content []byte) bool {
	obj, err := yaml.Parse(string(content))
	if err != nil {
		return false
	}
	node, err := obj.Pipe(yaml.Lookup("kind"))
	if err != nil || node == nil {
		return false
	}
	return yaml.GetValue(node) == "HelmRelease"
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
