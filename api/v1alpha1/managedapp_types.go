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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition type constants
const (
	ConditionTypeReady           = "Ready"
	ConditionTypeUpdateAvailable = "UpdateAvailable"
)

// ManagedAppSpec defines the desired state of ManagedApp
type ManagedAppSpec struct {
	// Path to the manifest in Git (for matching Renovate output and editing)
	// Examples: "flux/apps/gitea/helmrelease.yaml", "flux/apps/bazarr/deployment.yaml"
	// +kubebuilder:validation:Required
	GitPath string `json:"gitPath"`

	// Reference to the Flux Kustomization that reconciles this app (for suspend/resume)
	// +kubebuilder:validation:Required
	KustomizationRef ObjectReference `json:"kustomizationRef"`

	// Optional: explicit workload reference for health checks
	// If omitted, controller checks only the Kustomization status
	// +optional
	WorkloadRef *WorkloadReference `json:"workloadRef,omitempty"`

	// Volume snapshot configuration (Phase 2)
	// +optional
	VolumeSnapshots *VolumeSnapshotConfig `json:"volumeSnapshots,omitempty"`

	// Version policy for updates
	// +optional
	VersionPolicy *VersionPolicy `json:"versionPolicy,omitempty"`

	// Health check configuration
	// +optional
	HealthCheck *HealthCheckConfig `json:"healthCheck,omitempty"`
}

// ObjectReference is a reference to a Kubernetes object
type ObjectReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// WorkloadReference references a workload for health checks
type WorkloadReference struct {
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;HelmRelease
	Kind string `json:"kind"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// VolumeSnapshotConfig configures volume snapshots before upgrades (Phase 2)
type VolumeSnapshotConfig struct {
	Enabled                 bool             `json:"enabled"`
	VolumeSnapshotClassName string           `json:"volumeSnapshotClassName,omitempty"`
	PVCs                    []PVCRef         `json:"pvcs,omitempty"`
	RetentionPolicy         *RetentionPolicy `json:"retentionPolicy,omitempty"`
}

// PVCRef references a PVC for snapshotting
type PVCRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// RetentionPolicy defines how long to keep snapshots
type RetentionPolicy struct {
	MaxCount *int   `json:"maxCount,omitempty"`
	MaxAge   string `json:"maxAge,omitempty"`
}

// VersionPolicy defines which updates to allow
type VersionPolicy struct {
	// +kubebuilder:validation:Enum=none;patch;minor;major
	// +kubebuilder:default=none
	AutoUpdate string `json:"autoUpdate,omitempty"`
}

// HealthCheckConfig configures health checking
type HealthCheckConfig struct {
	// Timeout for health checks
	// +kubebuilder:default="5m"
	Timeout string `json:"timeout,omitempty"`
}

// ManagedAppStatus defines the observed state of ManagedApp.
type ManagedAppStatus struct {
	// Current version information
	// +optional
	CurrentVersion *VersionInfo `json:"currentVersion,omitempty"`

	// Available update information
	// +optional
	AvailableUpdate *VersionInfo `json:"availableUpdate,omitempty"`

	// Last upgrade information
	// +optional
	LastUpgrade *UpgradeInfo `json:"lastUpgrade,omitempty"`

	// conditions represent the current state of the ManagedApp resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VersionInfo contains version details
type VersionInfo struct {
	// Chart version (for HelmRelease)
	Chart string `json:"chart,omitempty"`

	// Image versions (for raw deployments or helm values)
	Images []ImageInfo `json:"images,omitempty"`
}

// ImageInfo contains image version details
type ImageInfo struct {
	Name string `json:"name"`
	Tag  string `json:"tag"`
}

// UpgradeInfo records the last upgrade
type UpgradeInfo struct {
	FromVersion *VersionInfo `json:"fromVersion,omitempty"`
	ToVersion   *VersionInfo `json:"toVersion,omitempty"`
	Timestamp   *metav1.Time `json:"timestamp,omitempty"`
	Status      string       `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Update",type="string",JSONPath=".status.conditions[?(@.type=='UpdateAvailable')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ManagedApp is the Schema for the managedapps API
type ManagedApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManagedAppSpec   `json:"spec,omitempty"`
	Status ManagedAppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ManagedAppList contains a list of ManagedApp
type ManagedAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagedApp `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ManagedApp{}, &ManagedAppList{})
}
