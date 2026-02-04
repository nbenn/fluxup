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

	// SuspendRef specifies which Kustomization to suspend during upgrades/rollbacks.
	// This is required for app-of-apps patterns where a parent Kustomization manages
	// child Kustomizations. If not set, defaults to KustomizationRef.
	// The referenced Kustomization must be a "root" - not managed by another Kustomization.
	// +optional
	SuspendRef *ObjectReference `json:"suspendRef,omitempty"`

	// HelmReleaseRef references a HelmRelease for PVC and workload discovery.
	// If set, the controller will discover RWO PVCs and workloads from the Helm release.
	// If not set, the controller will attempt to discover from the Kustomization's inventory.
	// +optional
	HelmReleaseRef *ObjectReference `json:"helmReleaseRef,omitempty"`

	// Volume snapshot configuration
	// +optional
	VolumeSnapshots *VolumeSnapshotConfig `json:"volumeSnapshots,omitempty"`

	// Version policy for updates
	// +optional
	VersionPolicy *VersionPolicy `json:"versionPolicy,omitempty"`

	// Health check configuration
	// +optional
	HealthCheck *HealthCheckConfig `json:"healthCheck,omitempty"`

	// AutoRollback enables automatic rollback when upgrades fail after Git commit.
	// When enabled, a RollbackRequest is automatically created if the upgrade fails
	// after the point of no return (Git commit).
	// +kubebuilder:default=false
	AutoRollback bool `json:"autoRollback,omitempty"`
}

// ObjectReference is a reference to a Kubernetes object
type ObjectReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// VolumeSnapshotConfig configures volume snapshots
type VolumeSnapshotConfig struct {
	// Enabled enables volume snapshots before upgrades
	Enabled bool `json:"enabled"`

	// VolumeSnapshotClassName is the name of the VolumeSnapshotClass to use
	// +optional
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`

	// PVCs is an explicit list of PVCs to snapshot. If not specified,
	// the controller will auto-discover RWO PVCs from the app's workloads.
	// +optional
	PVCs []PVCRef `json:"pvcs,omitempty"`

	// ExcludePVCs is a list of PVC names to exclude from auto-discovery.
	// Useful for excluding ephemeral volumes like caches.
	// +optional
	ExcludePVCs []PVCRef `json:"excludePVCs,omitempty"`

	// RetentionPolicy defines how many snapshots to keep per PVC
	// +optional
	RetentionPolicy *RetentionPolicy `json:"retentionPolicy,omitempty"`
}

// PVCRef references a PVC for snapshotting
type PVCRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// RetentionPolicy defines how many snapshots to keep
type RetentionPolicy struct {
	// MaxCount is the maximum number of snapshots to retain per PVC.
	// After a successful upgrade, older snapshots beyond this count are pruned.
	// Defaults to 3 if not specified. Set to 0 to disable retention (keep all).
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxCount *int `json:"maxCount,omitempty"`
}

// VersionPolicy defines which updates to allow
type VersionPolicy struct {
	// +kubebuilder:validation:Enum=none;patch;minor;major
	// +kubebuilder:default=none
	AutoUpdate string `json:"autoUpdate,omitempty"`

	// VersionPath is the YAML path to the version field in the manifest.
	// For HelmRelease, defaults to ".spec.chart.spec.version".
	// For other resources, must be specified explicitly.
	// +optional
	VersionPath string `json:"versionPath,omitempty"`
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
