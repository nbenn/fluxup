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

// UpgradeRequest condition type constants
const (
	// ConditionTypeSuspended indicates the Flux Kustomization is suspended
	ConditionTypeSuspended = "Suspended"

	// ConditionTypeWorkloadScaled indicates workload scaling state
	// Reason: ScaledDown (replicas=0) or ScaledUp (replicas restored)
	ConditionTypeWorkloadScaled = "WorkloadScaled"

	// ConditionTypeSnapshotReady indicates snapshots are ready
	ConditionTypeSnapshotReady = "SnapshotReady"

	// ConditionTypeGitCommitted indicates version was committed to Git
	ConditionTypeGitCommitted = "GitCommitted"

	// ConditionTypeReconciled indicates Flux has reconciled the change
	ConditionTypeReconciled = "Reconciled"

	// ConditionTypeHealthy indicates post-upgrade health check passed
	ConditionTypeHealthy = "Healthy"

	// ConditionTypeComplete indicates the upgrade is complete (check Reason for success/failure)
	ConditionTypeComplete = "Complete"
)

// UpgradeRequestSpec defines the desired state of UpgradeRequest
type UpgradeRequestSpec struct {
	// Reference to the ManagedApp to upgrade
	// +kubebuilder:validation:Required
	ManagedAppRef ObjectReference `json:"managedAppRef"`

	// Target version (optional - uses latest available if empty)
	// +optional
	TargetVersion *VersionInfo `json:"targetVersion,omitempty"`

	// Skip snapshot creation (not recommended)
	// +kubebuilder:default=false
	SkipSnapshot bool `json:"skipSnapshot,omitempty"`

	// Dry run - validate only, don't apply
	// +kubebuilder:default=false
	DryRun bool `json:"dryRun,omitempty"`
}

// UpgradeRequestStatus defines the observed state of UpgradeRequest
// Progress is tracked via conditions (no phase field - following K8s API conventions)
type UpgradeRequestStatus struct {
	// Scaling information (original replica count before scale-down)
	// +optional
	Scaling *ScalingStatus `json:"scaling,omitempty"`

	// Snapshot information
	// +optional
	Snapshot *SnapshotStatus `json:"snapshot,omitempty"`

	// Upgrade details
	// +optional
	Upgrade *UpgradeStatus `json:"upgrade,omitempty"`

	// Health check results
	// +optional
	HealthCheck *HealthCheckStatus `json:"healthCheck,omitempty"`

	// Conditions represent the current state of the upgrade.
	// The controller determines the current step by checking which conditions are set:
	// - No conditions set: validation/suspend step
	// - SnapshotReady=True, GitCommitted=False: committing step
	// - GitCommitted=True, Healthy=False: health checking step
	// - Complete=True: upgrade finished (check reason for success/failure)
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ScalingStatus records workload scaling details
type ScalingStatus struct {
	// Kind of the scaled workload (Deployment, StatefulSet)
	// +optional
	WorkloadKind string `json:"workloadKind,omitempty"`

	// Name of the scaled workload
	// +optional
	WorkloadName string `json:"workloadName,omitempty"`

	// Original replica count before scale-down
	// +optional
	OriginalReplicas int32 `json:"originalReplicas,omitempty"`

	// When scale-down completed
	// +optional
	ScaledDownAt *metav1.Time `json:"scaledDownAt,omitempty"`
}

// SnapshotStatus records snapshot creation details
type SnapshotStatus struct {
	// PVC snapshots created
	// +optional
	PVCSnapshots []PVCSnapshotInfo `json:"pvcSnapshots,omitempty"`

	// When snapshots were created
	// +optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`

	// When snapshots became ready
	// +optional
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`
}

// PVCSnapshotInfo records a single PVC snapshot
type PVCSnapshotInfo struct {
	PVCName      string `json:"pvcName"`
	SnapshotName string `json:"snapshotName"`
}

// UpgradeStatus records the upgrade operation details
type UpgradeStatus struct {
	// Previous version before upgrade
	// +optional
	PreviousVersion *VersionInfo `json:"previousVersion,omitempty"`

	// New version after upgrade
	// +optional
	NewVersion *VersionInfo `json:"newVersion,omitempty"`

	// Git commit SHA
	// +optional
	GitCommit string `json:"gitCommit,omitempty"`

	// When upgrade started
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// When upgrade completed
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// HealthCheckStatus records health check results
type HealthCheckStatus struct {
	// Status: Passed, Failed, Pending
	// +optional
	Status string `json:"status,omitempty"`

	// When health check was performed
	// +optional
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`

	// Failure message if failed
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Complete",type="string",JSONPath=".status.conditions[?(@.type=='Complete')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Complete')].reason"
// +kubebuilder:printcolumn:name="ManagedApp",type="string",JSONPath=".spec.managedAppRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// UpgradeRequest is the Schema for the upgraderequests API
type UpgradeRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UpgradeRequestSpec   `json:"spec,omitempty"`
	Status UpgradeRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UpgradeRequestList contains a list of UpgradeRequest
type UpgradeRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UpgradeRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&UpgradeRequest{}, &UpgradeRequestList{})
}
