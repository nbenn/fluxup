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

// Datasource constants for Renovate dependencies
const (
	DatasourceHelm   = "helm"
	DatasourceDocker = "docker"
)

// RenovateLogEntry represents the JSON log line with updates from Renovate
type RenovateLogEntry struct {
	Name   string          `json:"name"`
	Level  int             `json:"level"`
	Msg    string          `json:"msg"`
	Config *RenovateConfig `json:"config,omitempty"`
}

// RenovateConfig contains the package files with updates
type RenovateConfig struct {
	Flux       []PackageFile `json:"flux,omitempty"`
	HelmValues []PackageFile `json:"helm-values,omitempty"`
	Regex      []PackageFile `json:"regex,omitempty"`
}

// PackageFile represents a file containing dependencies
type PackageFile struct {
	PackageFile string       `json:"packageFile"`
	Deps        []Dependency `json:"deps"`
}

// Dependency represents a single dependency with updates
type Dependency struct {
	DepName      string   `json:"depName"`
	CurrentValue string   `json:"currentValue"`
	Datasource   string   `json:"datasource"`
	RegistryURL  string   `json:"registryUrl,omitempty"`
	Updates      []Update `json:"updates,omitempty"`
}

// Update represents an available update
type Update struct {
	NewValue   string `json:"newValue"`
	UpdateType string `json:"updateType"` // patch, minor, major
}

// UpdateInfo is our normalized update representation
type UpdateInfo struct {
	PackageFile    string // e.g., "apps/my-app/helmrelease.yaml"
	DependencyName string // e.g., "my-app" (chart name) or "nginx" (image)
	CurrentVersion string
	NewVersion     string
	UpdateType     string // patch, minor, major
	Datasource     string // helm, docker, etc.
}
