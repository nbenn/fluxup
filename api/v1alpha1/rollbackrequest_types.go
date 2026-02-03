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

// RollbackRequest condition type constants (rollback-specific)
const (
	// ConditionTypeVolumesRestored indicates PVCs have been restored from snapshots
	ConditionTypeVolumesRestored = "VolumesRestored"

	// ConditionTypeGitReverted indicates version was reverted in Git
	ConditionTypeGitReverted = "GitReverted"
)

// RollbackRequestSpec defines the desired state of RollbackRequest
type RollbackRequestSpec struct {
	// Reference to the UpgradeRequest to rollback.
	// The controller uses this to find the pre-upgrade snapshot and version.
	// +kubebuilder:validation:Required
	UpgradeRequestRef ObjectReference `json:"upgradeRequestRef"`

	// Whether this rollback was auto-triggered by a failed upgrade.
	// +kubebuilder:default=false
	AutoTriggered bool `json:"autoTriggered,omitempty"`

	// Dry run - validate only, don't apply.
	// +kubebuilder:default=false
	DryRun bool `json:"dryRun,omitempty"`
}

// RollbackRequestStatus defines the observed state of RollbackRequest
type RollbackRequestStatus struct {
	// Information about what was restored
	// +optional
	RestoredFrom *RestoredFromStatus `json:"restoredFrom,omitempty"`

	// PVC restoration details
	// +optional
	VolumeRestore *VolumeRestoreStatus `json:"volumeRestore,omitempty"`

	// Git revert details
	// +optional
	GitRevert *GitRevertStatus `json:"gitRevert,omitempty"`

	// When rollback started
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// When rollback completed
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Conditions represent the current state of the rollback.
	// The controller determines the current step by checking which conditions are set:
	// - Suspended=True: Flux suspended
	// - WorkloadStopped=True: Workload stopped
	// - VolumesRestored=True: PVCs restored from snapshots
	// - GitReverted=True: Version reverted in Git
	// - Suspended=False: Flux resumed
	// - Healthy=True: Health check passed
	// - Complete=True: Rollback finished (check reason for success/failure)
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RestoredFromStatus records what the rollback restored from
type RestoredFromStatus struct {
	// The UpgradeRequest that was rolled back
	// +optional
	UpgradeRequestName string `json:"upgradeRequestName,omitempty"`

	// The version being rolled back to (previous version)
	// +optional
	TargetVersion *VersionInfo `json:"targetVersion,omitempty"`

	// The version being rolled back from (new version that failed)
	// +optional
	RolledBackVersion *VersionInfo `json:"rolledBackVersion,omitempty"`
}

// VolumeRestoreStatus records PVC restoration details
type VolumeRestoreStatus struct {
	// PVCs that were restored
	// +optional
	RestoredPVCs []RestoredPVCInfo `json:"restoredPVCs,omitempty"`

	// When restore started
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// When restore completed
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// RestoredPVCInfo records a single PVC restoration
type RestoredPVCInfo struct {
	// Name of the PVC
	PVCName string `json:"pvcName"`

	// Name of the snapshot it was restored from
	SnapshotName string `json:"snapshotName"`

	// Restoration state: Pending, Restored, Failed
	// +optional
	RestorationState string `json:"restorationState,omitempty"`
}

// GitRevertStatus records Git revert details
type GitRevertStatus struct {
	// Commit SHA of the revert
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`

	// When the revert was committed
	// +optional
	CommittedAt *metav1.Time `json:"committedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Complete",type="string",JSONPath=".status.conditions[?(@.type=='Complete')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Complete')].reason"
// +kubebuilder:printcolumn:name="UpgradeRequest",type="string",JSONPath=".spec.upgradeRequestRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// RollbackRequest is the Schema for the rollbackrequests API.
// It triggers a rollback of an application to its previous version
// by restoring PVCs from snapshots and reverting the version in Git.
type RollbackRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RollbackRequestSpec   `json:"spec,omitempty"`
	Status RollbackRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RollbackRequestList contains a list of RollbackRequest
type RollbackRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RollbackRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RollbackRequest{}, &RollbackRequestList{})
}
