# Phase 3 Implementation Plan: Rollback Workflow

This document outlines the detailed implementation plan for Phase 3 of FluxUp.

## Phase 3 Goals

From the architecture document:
- RollbackRequest CRD and controller
- PVC restore from VolumeSnapshot
- Git revert functionality
- Workload scale down/up orchestration

---

## Rollback Flow Overview

The rollback workflow restores an application to a previous state. Like upgrades, progress is tracked via **conditions**.

```
User creates RollbackRequest (or auto-triggered on upgrade failure)
         │
         ▼
┌────────────────────────┐
│ 1. Validate request    │◀── Check UpgradeRequest/snapshot exists
│                        │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 2. Suspend Flux        │◀── Condition: Suspended=True
│    Kustomization       │    Prevent reconciliation during restore
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 3. Scale down          │◀── Condition: WorkloadScaled=True (reason=ScaledDown)
│    workload to 0       │    Required for PVC restore
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 4. Delete current PVCs │◀── Condition: VolumesRestored=False (InProgress)
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 5. Create new PVCs     │◀── From VolumeSnapshot dataSource
│    from snapshots      │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 6. Wait for PVCs       │◀── Condition: VolumesRestored=True
│    to be bound         │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 7. Revert version      │◀── Condition: GitReverted=True
│    in Git              │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 8. Resume Flux         │◀── Condition: Suspended=False
│    Kustomization       │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 9. Wait for workload   │◀── Condition: WorkloadScaled=True (reason=ScaledUp)
│    to scale up         │    Flux reconciles, restores replicas
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 10. Health check       │◀── Condition: Healthy=True
└──────────┬─────────────┘
           │
     ┌─────┴─────┐
     │           │
  Success     Failure
     │           │
     ▼           ▼
┌─────────┐ ┌──────────┐
│Complete │ │Complete  │
│ =True   │ │ =False   │
└─────────┘ └──────────┘
```

---

## Flux Suspend Strategy

### The Problem: App-of-Apps Pattern

In app-of-apps patterns, a parent Kustomization manages child Kustomizations:

```
root-kustomization (flux-system)     ← root, not managed by another Kustomization
  └── app-kustomization (apps)       ← managed by root
        └── HelmRelease
              └── StatefulSet
```

If we suspend `app-kustomization`, the `root-kustomization` might "heal" it back to `suspend: false` on its next reconciliation. This would cause the child app to resume mid-upgrade/rollback, potentially corrupting data.

### Solution: Two-Level Targeting

ManagedApp supports separate references for the upgrade target and suspend target:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
spec:
  # The Kustomization containing the HelmRelease (upgrade target)
  # This is where Git changes are made
  kustomizationRef:
    name: my-app
    namespace: apps

  # The root Kustomization to suspend (optional)
  # Must be a root - not managed by another Kustomization
  # Defaults to kustomizationRef if not specified
  suspendRef:
    name: root-apps
    namespace: flux-system

  workloadRef:
    kind: StatefulSet
    name: my-app
  # ... rest of spec
```

### Runtime Validation

Before suspending, the controller validates that the suspend target is a root:

1. **Check owner references** - If Kustomization has an `ownerReference` pointing to another Kustomization, it's managed
2. **Check Flux labels** - Look for `kustomize.toolkit.fluxcd.io/name` label indicating management
3. **Fail if managed without explicit suspendRef** - Clear error: "Kustomization 'X' is managed by 'Y'. Set spec.suspendRef to a root Kustomization."

### Suspend Verification

The owner reference and label checks are best-effort - they can't detect all cases (e.g., external automation, race conditions before Flux adds labels). To catch unexpected un-suspend:

1. **After suspending** - Wait briefly (~5s) and verify Kustomization is still suspended
2. **Before critical steps** - Re-verify suspend before:
   - PVC deletion (rollback)
   - Git commit (upgrade and rollback)
3. **On unexpected resume** - Fail immediately with: "Kustomization was un-suspended externally. Ensure no parent Kustomization or external automation manages suspend state."

This catches problems early, before destructive operations, rather than discovering them at health check time.

### Behavior Matrix

| Setup | `suspendRef` | What Gets Suspended | What Gets Scaled Down |
|-------|-------------|---------------------|----------------------|
| Single Kustomization (root) | not set | `kustomizationRef` | Target workload only |
| App-of-apps | set to root | `suspendRef` (root) | Target workload only |
| App-of-apps | not set | **ERROR** at validation | - |

### Trade-offs

Suspending a parent Kustomization that manages multiple apps means sibling apps won't reconcile during the upgrade/rollback window:

- **Acceptable** because the window is short (typically minutes)
- **Safer** than risking the child being un-suspended mid-operation
- **Explicit** - user opts into this by configuring `suspendRef`

Only the target workload is scaled down; sibling apps continue running (just won't receive updates during the window).

---

## Upgrade Flow Changes (Phase 2 Update)

Phase 3 requires a change to the **upgrade flow** to ensure application-consistent snapshots. Currently, snapshots are taken while the app is running, which risks capturing inconsistent state (e.g., database mid-transaction).

**Updated upgrade flow:**

```
User creates UpgradeRequest
         │
         ▼
┌────────────────────────┐
│ 1. Validate request    │◀── Check ManagedApp exists, has update
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 2. Suspend Flux        │◀── Condition: Suspended=True
│    Kustomization       │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 3. Scale down          │◀── NEW: Condition: WorkloadScaled=True (reason=ScaledDown)
│    workload to 0       │    Ensures clean shutdown before snapshot
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 4. Wait for pods gone  │◀── NEW: PVC finalizer protection ensures unmounted
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 5. Create snapshots    │◀── Condition: SnapshotReady=False (InProgress)
│    for configured PVCs │    Now capturing clean, consistent state
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 6. Wait for snapshots  │◀── Condition: SnapshotReady=True
│    ReadyToUse = true   │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 7. Commit version      │◀── Condition: GitCommitted=True
│    change to Git       │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 8. Resume Flux         │◀── Condition: Suspended=False
│    Kustomization       │    Flux reconciles, scales workload back up
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 9. Wait for Flux       │◀── Condition: Reconciled=True
│    reconciliation      │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 10. Health check       │◀── Condition: Healthy=True
└──────────┬─────────────┘
           │
     ┌─────┴─────┐
     │           │
  Success     Failure
     │           │
     ▼           ▼
┌─────────┐ ┌──────────┐
│Complete │ │Complete  │
│ =True   │ │ =False   │
└─────────┘ └──────────┘
```

**Key changes from Phase 2:**
- Steps 3-4 are new: scale down and wait for pods to terminate
- Snapshots (step 5) now capture application-consistent state
- Flux resume (step 8) handles scaling back up via reconciliation

**Trade-off:** Brief additional downtime during upgrade while snapshotting. This is acceptable because:
- There's already downtime during the upgrade rollout
- Consistent snapshots are critical for reliable rollback
- Database corruption on rollback would be far worse than brief downtime

---

## Rollback Triggers

### User-Triggered Rollback

User creates a RollbackRequest CR manually (via kubectl or future UI):

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: my-app-rollback-20240115
  namespace: default
spec:
  upgradeRequestRef:
    name: my-app-upgrade-20240115
```

### Auto-Triggered Rollback (on Upgrade Failure)

When an UpgradeRequest fails **after Git commit** (health check timeout, reconciliation failure), the UpgradeRequest controller automatically creates a RollbackRequest if `autoRollback` is enabled on the ManagedApp.

```yaml
# ManagedApp with auto-rollback enabled
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
spec:
  # ... other fields ...
  autoRollback: true  # Default: false
```

The auto-created RollbackRequest references the failed UpgradeRequest:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: my-app-auto-rollback-20240115-143000
  namespace: default
  ownerReferences:
    - apiVersion: fluxup.dev/v1alpha1
      kind: UpgradeRequest
      name: my-app-upgrade-20240115
      uid: <upgrade-uid>
spec:
  upgradeRequestRef:
    name: my-app-upgrade-20240115
  autoTriggered: true
```

**No automatic retry**: If rollback fails, the RollbackRequest is marked failed. User must investigate and manually retry or intervene.

---

## Step 1: RollbackRequest CRD

### 1.1 Type Definitions

```go
// api/v1alpha1/rollbackrequest_types.go

// RollbackRequestSpec defines the desired state of RollbackRequest
type RollbackRequestSpec struct {
    // Reference to the UpgradeRequest to rollback
    // The controller uses this to find the pre-upgrade snapshot and version
    // +kubebuilder:validation:Required
    UpgradeRequestRef ObjectReference `json:"upgradeRequestRef"`

    // Whether this rollback was auto-triggered by a failed upgrade
    // +kubebuilder:default=false
    AutoTriggered bool `json:"autoTriggered,omitempty"`

    // Dry run - validate only, don't apply
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

    // Conditions represent the current state of the rollback
    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RestoredFromStatus records what the rollback restored from
type RestoredFromStatus struct {
    // The UpgradeRequest that was rolled back
    UpgradeRequestName string `json:"upgradeRequestName,omitempty"`

    // The version being rolled back to
    TargetVersion *VersionInfo `json:"targetVersion,omitempty"`

    // The version being rolled back from
    RolledBackVersion *VersionInfo `json:"rolledBackVersion,omitempty"`
}

// VolumeRestoreStatus records PVC restoration details
type VolumeRestoreStatus struct {
    // PVCs that were restored
    RestoredPVCs []RestoredPVCInfo `json:"restoredPVCs,omitempty"`

    // When restore started
    StartedAt *metav1.Time `json:"startedAt,omitempty"`

    // When restore completed
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// RestoredPVCInfo records a single PVC restoration
type RestoredPVCInfo struct {
    PVCName          string `json:"pvcName"`
    SnapshotName     string `json:"snapshotName"`
    RestorationState string `json:"restorationState,omitempty"` // Pending, Restored, Failed
}

// GitRevertStatus records Git revert details
type GitRevertStatus struct {
    // Commit SHA of the revert
    CommitSHA string `json:"commitSHA,omitempty"`

    // When the revert was committed
    CommittedAt *metav1.Time `json:"committedAt,omitempty"`
}
```

### 1.2 Condition Types

```go
const (
    // ConditionTypeSuspended indicates the Flux Kustomization is suspended
    // (reused from UpgradeRequest)
    // ConditionTypeSuspended = "Suspended"

    // ConditionTypeWorkloadScaled indicates workload scaling state
    // Reason: ScaledDown (replicas=0) or ScaledUp (replicas restored)
    ConditionTypeWorkloadScaled = "WorkloadScaled"

    // ConditionTypeVolumesRestored indicates PVCs have been restored from snapshots
    ConditionTypeVolumesRestored = "VolumesRestored"

    // ConditionTypeGitReverted indicates the version has been reverted in Git
    ConditionTypeGitReverted = "GitReverted"

    // ConditionTypeHealthy indicates post-rollback health check passed
    // (reused from UpgradeRequest)
    // ConditionTypeHealthy = "Healthy"

    // ConditionTypeComplete indicates the rollback is complete
    // (reused from UpgradeRequest)
    // ConditionTypeComplete = "Complete"
)
```

### 1.3 Kubebuilder Markers

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Complete",type="string",JSONPath=".status.conditions[?(@.type=='Complete')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Complete')].reason"
// +kubebuilder:printcolumn:name="UpgradeRequest",type="string",JSONPath=".spec.upgradeRequestRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
```

---

## Step 2: ManagedApp Updates

Add `autoRollback` field to ManagedApp spec:

```go
// In api/v1alpha1/managedapp_types.go

type ManagedAppSpec struct {
    // ... existing fields ...

    // AutoRollback enables automatic rollback when upgrades fail after Git commit
    // +kubebuilder:default=false
    AutoRollback bool `json:"autoRollback,omitempty"`
}
```

---

## Step 3: Snapshot Manager Additions

### 3.1 PVC Restoration

```go
// internal/snapshot/restore.go

// RestoreRequest defines a PVC restoration from snapshot
type RestoreRequest struct {
    SnapshotName     string
    SnapshotNamespace string
    NewPVCName       string
    NewPVCNamespace  string
    StorageClassName string // Optional, uses snapshot's SC if empty
    Labels           map[string]string
}

// RestorePVCFromSnapshot creates a new PVC from a VolumeSnapshot
func (m *Manager) RestorePVCFromSnapshot(ctx context.Context, req RestoreRequest) (*corev1.PersistentVolumeClaim, error) {
    // Verify snapshot exists and is ready
    var snapshot snapshotv1.VolumeSnapshot
    snapshotKey := types.NamespacedName{Name: req.SnapshotName, Namespace: req.SnapshotNamespace}
    if err := m.client.Get(ctx, snapshotKey, &snapshot); err != nil {
        return nil, fmt.Errorf("snapshot not found: %w", err)
    }

    if snapshot.Status == nil || snapshot.Status.ReadyToUse == nil || !*snapshot.Status.ReadyToUse {
        return nil, fmt.Errorf("snapshot %s is not ready", req.SnapshotName)
    }

    // Get restore size from snapshot
    var restoreSize resource.Quantity
    if snapshot.Status.RestoreSize != nil {
        restoreSize = *snapshot.Status.RestoreSize
    } else {
        return nil, fmt.Errorf("snapshot %s has no restore size", req.SnapshotName)
    }

    // Create PVC with snapshot as data source
    apiGroup := "snapshot.storage.k8s.io"
    pvc := &corev1.PersistentVolumeClaim{
        ObjectMeta: metav1.ObjectMeta{
            Name:      req.NewPVCName,
            Namespace: req.NewPVCNamespace,
            Labels:    req.Labels,
        },
        Spec: corev1.PersistentVolumeClaimSpec{
            AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
            Resources: corev1.VolumeResourceRequirements{
                Requests: corev1.ResourceList{
                    corev1.ResourceStorage: restoreSize,
                },
            },
            DataSource: &corev1.TypedLocalObjectReference{
                APIGroup: &apiGroup,
                Kind:     "VolumeSnapshot",
                Name:     req.SnapshotName,
            },
        },
    }

    if req.StorageClassName != "" {
        pvc.Spec.StorageClassName = &req.StorageClassName
    }

    if err := m.client.Create(ctx, pvc); err != nil {
        return nil, fmt.Errorf("creating PVC from snapshot: %w", err)
    }

    return pvc, nil
}

// WaitForPVCBound waits for a PVC to be bound
func (m *Manager) WaitForPVCBound(ctx context.Context, name, namespace string, timeout time.Duration) error {
    deadline := time.Now().Add(timeout)
    key := types.NamespacedName{Name: name, Namespace: namespace}

    for time.Now().Before(deadline) {
        var pvc corev1.PersistentVolumeClaim
        if err := m.client.Get(ctx, key, &pvc); err != nil {
            return fmt.Errorf("getting PVC: %w", err)
        }

        if pvc.Status.Phase == corev1.ClaimBound {
            return nil
        }

        // Check for errors in events (TODO: watch events)

        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(5 * time.Second):
            // Continue polling
        }
    }

    return fmt.Errorf("timeout waiting for PVC %s/%s to be bound", namespace, name)
}

// DeletePVC deletes a PVC
func (m *Manager) DeletePVC(ctx context.Context, name, namespace string) error {
    pvc := &corev1.PersistentVolumeClaim{
        ObjectMeta: metav1.ObjectMeta{
            Name:      name,
            Namespace: namespace,
        },
    }
    return m.client.Delete(ctx, pvc)
}
```

---

## Step 4: Workload Scaling

### 4.1 Scale Helpers

```go
// internal/workload/scale.go

package workload

import (
    "context"
    "fmt"

    appsv1 "k8s.io/api/apps/v1"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

// Scaler handles workload scaling operations
type Scaler struct {
    client client.Client
}

func NewScaler(c client.Client) *Scaler {
    return &Scaler{client: c}
}

// ScaleInfo holds information about a scaled workload
type ScaleInfo struct {
    Kind             string
    Name             string
    Namespace        string
    OriginalReplicas int32
}

// ScaleDown scales a workload to 0 replicas
// Returns the original replica count for later restoration
func (s *Scaler) ScaleDown(ctx context.Context, kind, name, namespace string) (*ScaleInfo, error) {
    switch kind {
    case "Deployment":
        return s.scaleDeployment(ctx, name, namespace, 0)
    case "StatefulSet":
        return s.scaleStatefulSet(ctx, name, namespace, 0)
    default:
        return nil, fmt.Errorf("unsupported workload kind: %s", kind)
    }
}

// ScaleUp scales a workload to the specified replica count
func (s *Scaler) ScaleUp(ctx context.Context, kind, name, namespace string, replicas int32) error {
    switch kind {
    case "Deployment":
        _, err := s.scaleDeployment(ctx, name, namespace, replicas)
        return err
    case "StatefulSet":
        _, err := s.scaleStatefulSet(ctx, name, namespace, replicas)
        return err
    default:
        return fmt.Errorf("unsupported workload kind: %s", kind)
    }
}

func (s *Scaler) scaleDeployment(ctx context.Context, name, namespace string, replicas int32) (*ScaleInfo, error) {
    var deploy appsv1.Deployment
    key := types.NamespacedName{Name: name, Namespace: namespace}

    if err := s.client.Get(ctx, key, &deploy); err != nil {
        return nil, fmt.Errorf("getting deployment: %w", err)
    }

    originalReplicas := int32(1)
    if deploy.Spec.Replicas != nil {
        originalReplicas = *deploy.Spec.Replicas
    }

    // Don't scale if already at target
    if deploy.Spec.Replicas != nil && *deploy.Spec.Replicas == replicas {
        return &ScaleInfo{
            Kind:             "Deployment",
            Name:             name,
            Namespace:        namespace,
            OriginalReplicas: originalReplicas,
        }, nil
    }

    patch := client.MergeFrom(deploy.DeepCopy())
    deploy.Spec.Replicas = &replicas

    if err := s.client.Patch(ctx, &deploy, patch); err != nil {
        return nil, fmt.Errorf("scaling deployment: %w", err)
    }

    return &ScaleInfo{
        Kind:             "Deployment",
        Name:             name,
        Namespace:        namespace,
        OriginalReplicas: originalReplicas,
    }, nil
}

func (s *Scaler) scaleStatefulSet(ctx context.Context, name, namespace string, replicas int32) (*ScaleInfo, error) {
    var sts appsv1.StatefulSet
    key := types.NamespacedName{Name: name, Namespace: namespace}

    if err := s.client.Get(ctx, key, &sts); err != nil {
        return nil, fmt.Errorf("getting statefulset: %w", err)
    }

    originalReplicas := int32(1)
    if sts.Spec.Replicas != nil {
        originalReplicas = *sts.Spec.Replicas
    }

    // Don't scale if already at target
    if sts.Spec.Replicas != nil && *sts.Spec.Replicas == replicas {
        return &ScaleInfo{
            Kind:             "StatefulSet",
            Name:             name,
            Namespace:        namespace,
            OriginalReplicas: originalReplicas,
        }, nil
    }

    patch := client.MergeFrom(sts.DeepCopy())
    sts.Spec.Replicas = &replicas

    if err := s.client.Patch(ctx, &sts, patch); err != nil {
        return nil, fmt.Errorf("scaling statefulset: %w", err)
    }

    return &ScaleInfo{
        Kind:             "StatefulSet",
        Name:             name,
        Namespace:        namespace,
        OriginalReplicas: originalReplicas,
    }, nil
}

// WaitForScaleDown waits for all pods to terminate
func (s *Scaler) WaitForScaleDown(ctx context.Context, kind, name, namespace string, timeout time.Duration) error {
    // TODO: Implement pod watching
    // For now, use simple polling
    deadline := time.Now().Add(timeout)

    for time.Now().Before(deadline) {
        ready, err := s.getReadyReplicas(ctx, kind, name, namespace)
        if err != nil {
            return err
        }

        if ready == 0 {
            return nil
        }

        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(5 * time.Second):
            // Continue polling
        }
    }

    return fmt.Errorf("timeout waiting for workload %s/%s to scale down", namespace, name)
}

func (s *Scaler) getReadyReplicas(ctx context.Context, kind, name, namespace string) (int32, error) {
    key := types.NamespacedName{Name: name, Namespace: namespace}

    switch kind {
    case "Deployment":
        var deploy appsv1.Deployment
        if err := s.client.Get(ctx, key, &deploy); err != nil {
            return 0, err
        }
        return deploy.Status.ReadyReplicas, nil
    case "StatefulSet":
        var sts appsv1.StatefulSet
        if err := s.client.Get(ctx, key, &sts); err != nil {
            return 0, err
        }
        return sts.Status.ReadyReplicas, nil
    default:
        return 0, fmt.Errorf("unsupported workload kind: %s", kind)
    }
}
```

---

## Step 5: Git Revert

### 5.1 Revert Commit Message

```go
// internal/git/commit.go (additions)

// FormatRevertCommitMessage creates a commit message for rollback
func FormatRevertCommitMessage(appName, fromVersion, toVersion, upgradeRequestName string) string {
    var sb strings.Builder

    sb.WriteString(fmt.Sprintf("chore(rollback): revert %s to %s\n\n", appName, toVersion))
    sb.WriteString(fmt.Sprintf("Rollback triggered via FluxUp\n"))
    sb.WriteString(fmt.Sprintf("Rolling back from: %s\n", fromVersion))
    sb.WriteString(fmt.Sprintf("Original upgrade: %s\n", upgradeRequestName))
    sb.WriteString(fmt.Sprintf("\nManaged-By: fluxup\n"))

    return sb.String()
}
```

---

## Step 6: RollbackRequest Controller

### 6.1 Controller Structure

```go
// internal/controller/rollbackrequest_controller.go

package controller

import (
    "context"
    "fmt"
    "time"

    "k8s.io/apimachinery/pkg/api/meta"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/types"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/log"

    fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
    "github.com/nbenn/fluxup/internal/flux"
    "github.com/nbenn/fluxup/internal/git"
    "github.com/nbenn/fluxup/internal/snapshot"
    "github.com/nbenn/fluxup/internal/workload"
    yamlpkg "github.com/nbenn/fluxup/internal/yaml"
)

// RollbackRequestReconciler reconciles a RollbackRequest
type RollbackRequestReconciler struct {
    client.Client
    Scheme          *runtime.Scheme
    GitManager      git.Manager
    SnapshotManager *snapshot.Manager
    FluxHelper      *flux.Helper
    WorkloadScaler  *workload.Scaler
    YAMLEditor      *yamlpkg.Editor
}

// +kubebuilder:rbac:groups=fluxup.dev,resources=rollbackrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fluxup.dev,resources=rollbackrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fluxup.dev,resources=rollbackrequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete

func (r *RollbackRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // 1. Fetch RollbackRequest
    var rollback fluxupv1alpha1.RollbackRequest
    if err := r.Get(ctx, req.NamespacedName, &rollback); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    logger.Info("Reconciling RollbackRequest",
        "upgradeRequest", rollback.Spec.UpgradeRequestRef.Name)

    // 2. Check if already complete (terminal state)
    if meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete) {
        return ctrl.Result{}, nil
    }
    if cond := meta.FindStatusCondition(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete); cond != nil && cond.Status == metav1.ConditionFalse {
        return ctrl.Result{}, nil
    }

    // 3. Determine current step from conditions and handle accordingly
    if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
        return r.handleSuspend(ctx, &rollback)
    }

    // Check if scaled down (WorkloadScaled=True with reason=ScaledDown)
    scaledCond := meta.FindStatusCondition(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeWorkloadScaled)
    if scaledCond == nil || scaledCond.Reason != "ScaledDown" {
        return r.handleScaleDown(ctx, &rollback)
    }

    if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeVolumesRestored) {
        return r.handleVolumeRestore(ctx, &rollback)
    }

    if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeGitReverted) {
        return r.handleGitRevert(ctx, &rollback)
    }

    // Check if Flux resumed (Suspended=False)
    if meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
        return r.handleResume(ctx, &rollback)
    }

    // Check if scaled up (WorkloadScaled=True with reason=ScaledUp)
    if scaledCond == nil || scaledCond.Reason != "ScaledUp" {
        return r.handleWaitForScaleUp(ctx, &rollback)
    }

    if !meta.IsStatusConditionTrue(rollback.Status.Conditions, fluxupv1alpha1.ConditionTypeHealthy) {
        return r.handleHealthCheck(ctx, &rollback)
    }

    // All conditions met - mark complete
    return r.handleCompleted(ctx, &rollback)
}
```

### 6.2 Step Handlers (Partial - key handlers shown)

```go
// handleSuspend validates the request and suspends Flux
func (r *RollbackRequestReconciler) handleSuspend(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // Set start time
    if rollback.Status.StartedAt == nil {
        now := metav1.Now()
        rollback.Status.StartedAt = &now
    }

    // Fetch the UpgradeRequest
    upgrade, err := r.getUpgradeRequest(ctx, rollback)
    if err != nil {
        return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
    }

    // Fetch the ManagedApp
    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
    }

    // Verify upgrade has snapshot info
    if upgrade.Status.Snapshot == nil || len(upgrade.Status.Snapshot.PVCSnapshots) == 0 {
        return r.setFailed(ctx, rollback, "NoSnapshotsAvailable", "UpgradeRequest has no snapshots to restore from")
    }

    // Record what we're rolling back
    rollback.Status.RestoredFrom = &fluxupv1alpha1.RestoredFromStatus{
        UpgradeRequestName: upgrade.Name,
        TargetVersion:      upgrade.Status.Upgrade.PreviousVersion,
        RolledBackVersion:  upgrade.Status.Upgrade.NewVersion,
    }

    logger.Info("Starting rollback",
        "app", app.Name,
        "fromVersion", upgrade.Status.Upgrade.NewVersion,
        "toVersion", upgrade.Status.Upgrade.PreviousVersion)

    // Suspend Flux Kustomization
    ksRef := app.Spec.KustomizationRef
    ksNS := ksRef.Namespace
    if ksNS == "" {
        ksNS = "flux-system"
    }

    if err := r.FluxHelper.SuspendKustomization(ctx, ksRef.Name, ksNS); err != nil {
        return r.setFailed(ctx, rollback, "SuspendFailed", err.Error())
    }

    meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeSuspended,
        Status:             metav1.ConditionTrue,
        Reason:             "KustomizationSuspended",
        Message:            fmt.Sprintf("Suspended %s/%s", ksNS, ksRef.Name),
        ObservedGeneration: rollback.Generation,
    })

    if err := r.Status().Update(ctx, rollback); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{Requeue: true}, nil
}

// handleScaleDown scales the workload to 0 replicas
func (r *RollbackRequestReconciler) handleScaleDown(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    upgrade, err := r.getUpgradeRequest(ctx, rollback)
    if err != nil {
        return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
    }

    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
    }

    // Scale down the workload
    workloadRef := app.Spec.WorkloadRef
    scaleInfo, err := r.WorkloadScaler.ScaleDown(ctx, workloadRef.Kind, workloadRef.Name, app.Namespace)
    if err != nil {
        return r.setFailed(ctx, rollback, "ScaleDownFailed", err.Error())
    }

    logger.Info("Scaling down workload",
        "kind", workloadRef.Kind,
        "name", workloadRef.Name,
        "originalReplicas", scaleInfo.OriginalReplicas)

    // Wait for scale down to complete
    if err := r.WorkloadScaler.WaitForScaleDown(ctx, workloadRef.Kind, workloadRef.Name, app.Namespace, 5*time.Minute); err != nil {
        // Check if it's just not ready yet vs actual error
        if ctx.Err() == nil {
            return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
        }
        return r.setFailed(ctx, rollback, "ScaleDownTimeout", err.Error())
    }

    meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeWorkloadScaled,
        Status:             metav1.ConditionTrue,
        Reason:             "ScaledDown",
        Message:            fmt.Sprintf("Scaled %s/%s to 0 (was %d)", workloadRef.Kind, workloadRef.Name, scaleInfo.OriginalReplicas),
        ObservedGeneration: rollback.Generation,
    })

    // Store original replicas in annotation for later restoration
    // (Flux will restore this when it reconciles, but we track it for visibility)

    if err := r.Status().Update(ctx, rollback); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{Requeue: true}, nil
}

// handleVolumeRestore deletes current PVCs and creates new ones from snapshots
func (r *RollbackRequestReconciler) handleVolumeRestore(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    upgrade, err := r.getUpgradeRequest(ctx, rollback)
    if err != nil {
        return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
    }

    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
    }

    // Initialize volume restore status
    if rollback.Status.VolumeRestore == nil {
        now := metav1.Now()
        rollback.Status.VolumeRestore = &fluxupv1alpha1.VolumeRestoreStatus{
            StartedAt: &now,
        }
    }

    // Process each PVC from the upgrade's snapshot list
    for _, snapInfo := range upgrade.Status.Snapshot.PVCSnapshots {
        // Check if already processed
        alreadyRestored := false
        for _, restored := range rollback.Status.VolumeRestore.RestoredPVCs {
            if restored.PVCName == snapInfo.PVCName {
                alreadyRestored = true
                break
            }
        }
        if alreadyRestored {
            continue
        }

        logger.Info("Restoring PVC from snapshot",
            "pvc", snapInfo.PVCName,
            "snapshot", snapInfo.SnapshotName)

        // Delete the current PVC
        if err := r.SnapshotManager.DeletePVC(ctx, snapInfo.PVCName, app.Namespace); err != nil {
            // Ignore not found - might already be deleted
            if !apierrors.IsNotFound(err) {
                return r.setFailed(ctx, rollback, "PVCDeleteFailed", err.Error())
            }
        }

        // Wait a moment for PVC to be fully deleted
        // TODO: Use finalizer watching instead
        time.Sleep(2 * time.Second)

        // Create new PVC from snapshot
        _, err := r.SnapshotManager.RestorePVCFromSnapshot(ctx, snapshot.RestoreRequest{
            SnapshotName:      snapInfo.SnapshotName,
            SnapshotNamespace: app.Namespace,
            NewPVCName:        snapInfo.PVCName,
            NewPVCNamespace:   app.Namespace,
            Labels: map[string]string{
                "fluxup.dev/restored-from": snapInfo.SnapshotName,
            },
        })
        if err != nil {
            return r.setFailed(ctx, rollback, "PVCRestoreFailed", err.Error())
        }

        // Record the restoration
        rollback.Status.VolumeRestore.RestoredPVCs = append(rollback.Status.VolumeRestore.RestoredPVCs,
            fluxupv1alpha1.RestoredPVCInfo{
                PVCName:          snapInfo.PVCName,
                SnapshotName:     snapInfo.SnapshotName,
                RestorationState: "Pending",
            })

        if err := r.Status().Update(ctx, rollback); err != nil {
            return ctrl.Result{}, err
        }
    }

    // Wait for all PVCs to be bound
    for i, restored := range rollback.Status.VolumeRestore.RestoredPVCs {
        if restored.RestorationState == "Restored" {
            continue
        }

        if err := r.SnapshotManager.WaitForPVCBound(ctx, restored.PVCName, app.Namespace, 5*time.Minute); err != nil {
            if ctx.Err() == nil {
                return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
            }
            return r.setFailed(ctx, rollback, "PVCBindTimeout", err.Error())
        }

        rollback.Status.VolumeRestore.RestoredPVCs[i].RestorationState = "Restored"
    }

    // All PVCs restored
    now := metav1.Now()
    rollback.Status.VolumeRestore.CompletedAt = &now

    meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeVolumesRestored,
        Status:             metav1.ConditionTrue,
        Reason:             "AllPVCsRestored",
        Message:            fmt.Sprintf("Restored %d PVCs from snapshots", len(rollback.Status.VolumeRestore.RestoredPVCs)),
        ObservedGeneration: rollback.Generation,
    })

    if err := r.Status().Update(ctx, rollback); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{Requeue: true}, nil
}

// handleGitRevert reverts the version in Git
func (r *RollbackRequestReconciler) handleGitRevert(ctx context.Context, rollback *fluxupv1alpha1.RollbackRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    upgrade, err := r.getUpgradeRequest(ctx, rollback)
    if err != nil {
        return r.setFailed(ctx, rollback, "UpgradeRequestNotFound", err.Error())
    }

    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, rollback, "ManagedAppNotFound", err.Error())
    }

    // Get the version to revert to
    targetVersion := upgrade.Status.Upgrade.PreviousVersion
    if targetVersion == nil || targetVersion.Chart == "" {
        return r.setFailed(ctx, rollback, "NoPreviousVersion", "UpgradeRequest has no previous version recorded")
    }

    logger.Info("Reverting Git to previous version",
        "targetVersion", targetVersion.Chart)

    // Read current file
    content, err := r.GitManager.ReadFile(ctx, app.Spec.GitPath)
    if err != nil {
        return r.setFailed(ctx, rollback, "GitReadFailed", err.Error())
    }

    // Update to previous version
    newContent, err := r.YAMLEditor.UpdateHelmReleaseVersion(content, targetVersion.Chart)
    if err != nil {
        return r.setFailed(ctx, rollback, "YAMLUpdateFailed", err.Error())
    }

    // Commit
    message := git.FormatRevertCommitMessage(
        app.Name,
        upgrade.Status.Upgrade.NewVersion.Chart,
        targetVersion.Chart,
        upgrade.Name,
    )

    commitInfo, err := r.GitManager.CommitFile(ctx, git.FileChange{
        Path:    app.Spec.GitPath,
        Content: newContent,
    }, message)
    if err != nil {
        return r.setFailed(ctx, rollback, "GitCommitFailed", err.Error())
    }

    logger.Info("Committed version revert to Git",
        "commit", commitInfo.SHA,
        "version", targetVersion.Chart)

    now := metav1.Now()
    rollback.Status.GitRevert = &fluxupv1alpha1.GitRevertStatus{
        CommitSHA:   commitInfo.SHA,
        CommittedAt: &now,
    }

    meta.SetStatusCondition(&rollback.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeGitReverted,
        Status:             metav1.ConditionTrue,
        Reason:             "VersionReverted",
        Message:            fmt.Sprintf("Commit %s", commitInfo.SHA),
        ObservedGeneration: rollback.Generation,
    })

    if err := r.Status().Update(ctx, rollback); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{Requeue: true}, nil
}
```

---

## Step 7: Auto-Rollback in UpgradeRequest Controller

Update the UpgradeRequest controller's `setFailed` method to create a RollbackRequest when appropriate:

```go
// In internal/controller/upgraderequest_controller.go

func (r *UpgradeRequestReconciler) setFailed(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest, reason, message string) (ctrl.Result, error) {
    logger := log.FromContext(ctx)
    logger.Error(nil, "Upgrade failed", "reason", reason, "message", message)

    gitCommitted := meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeGitCommitted)

    if gitCommitted {
        // After Git commit: check if auto-rollback is enabled
        app, err := r.getManagedApp(ctx, upgrade)
        if err == nil && app.Spec.AutoRollback {
            // Create RollbackRequest
            if err := r.createAutoRollback(ctx, upgrade, app); err != nil {
                logger.Error(err, "Failed to create auto-rollback")
            } else {
                message = fmt.Sprintf("%s - auto-rollback initiated", message)
            }
        } else {
            message = fmt.Sprintf("%s - rollback required", message)
        }

        meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
            Type:               fluxupv1alpha1.ConditionTypeComplete,
            Status:             metav1.ConditionFalse,
            Reason:             reason,
            Message:            message,
            ObservedGeneration: upgrade.Generation,
        })
        logger.Info("Upgrade failed after Git commit", "autoRollback", app != nil && app.Spec.AutoRollback)
    } else {
        // Before Git commit: safe to resume and abort
        // ... existing code ...
    }

    // ... rest of existing code ...
}

func (r *UpgradeRequestReconciler) createAutoRollback(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest, app *fluxupv1alpha1.ManagedApp) error {
    rollbackName := fmt.Sprintf("%s-auto-rollback-%s", app.Name, time.Now().Format("20060102-150405"))

    rollback := &fluxupv1alpha1.RollbackRequest{
        ObjectMeta: metav1.ObjectMeta{
            Name:      rollbackName,
            Namespace: upgrade.Namespace,
            OwnerReferences: []metav1.OwnerReference{
                {
                    APIVersion: upgrade.APIVersion,
                    Kind:       upgrade.Kind,
                    Name:       upgrade.Name,
                    UID:        upgrade.UID,
                },
            },
        },
        Spec: fluxupv1alpha1.RollbackRequestSpec{
            UpgradeRequestRef: fluxupv1alpha1.ObjectReference{
                Name: upgrade.Name,
            },
            AutoTriggered: true,
        },
    }

    return r.Create(ctx, rollback)
}
```

---

## Implementation Order

1. **RollbackRequest CRD** - Scaffold and define types
2. **ManagedApp updates** - Add `autoRollback` field
3. **Workload scaler** - Scale down/up helpers
4. **Snapshot manager additions** - PVC restore from snapshot
5. **Git revert helper** - Commit message format
6. **RollbackRequest controller** - Main orchestration
7. **Auto-rollback integration** - Update UpgradeRequest controller
8. **Tests** - Unit and integration tests

---

## Testing Strategy

### Unit Tests

```
internal/workload/scale_test.go        # Scaling tests (mock)
internal/snapshot/restore_test.go      # PVC restoration tests (mock)
internal/controller/rollbackrequest_controller_test.go
```

### Integration Tests

```go
var _ = Describe("RollbackRequest Controller", func() {
    Context("When processing a RollbackRequest", func() {
        It("Should complete the rollback workflow", func() {
            // 1. Create ManagedApp, UpgradeRequest with snapshots
            // 2. Create RollbackRequest
            // 3. Verify rollback completes (Complete=True)
        })

        It("Should auto-rollback on upgrade failure when enabled", func() {
            // 1. Create ManagedApp with autoRollback=true
            // 2. Create UpgradeRequest that will fail after Git commit
            // 3. Verify RollbackRequest is auto-created
        })
    })
})
```

---

## Sample Resources

### RollbackRequest (User-Triggered)

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: my-app-rollback-20240115
  namespace: default
spec:
  upgradeRequestRef:
    name: my-app-upgrade-20240115
```

### ManagedApp with Auto-Rollback

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
  namespace: default
spec:
  gitPath: "flux/apps/my-app/helmrelease.yaml"
  kustomizationRef:
    name: apps
    namespace: flux-system
  workloadRef:
    kind: StatefulSet
    name: my-app
  autoRollback: true  # Enable auto-rollback on upgrade failure
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: csi-snapclass
    pvcs:
      - name: data-my-app-0
    retentionPolicy:
      maxCount: 3
```

---

## RBAC Updates

The RollbackRequest controller needs permissions for workload scaling and PVC management:

```go
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
```

---

## Resolved Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Rollback triggers | Both user and auto | Modest complexity increase; auto-rollback provides significant value |
| Auto-retry | No | Failed rollback needs human investigation |
| Git revert strategy | Direct version update | Reuses existing YAML editor + Gitea API; no new dependencies |
| Partial rollback | Full only | Data and version must stay in sync; partial rollback risks inconsistency |
| Snapshot selection | UpgradeRequest ref only | Rollback restores exactly the snapshot created for that specific upgrade |
| Rollback of in-progress upgrades | Not allowed | Reject RollbackRequest if UpgradeRequest is not terminal; avoids race conditions |
| PVC unmount verification | Wait for PVC deletion | Use `pvc-protection` finalizer; Kubernetes blocks deletion until unmounted |
| Stuck pod handling | Fail with guidance | No auto force-delete; user must investigate and manually intervene |
| Snapshot timing (upgrade flow) | After scale-down | Ensures application-consistent snapshots; brief downtime is acceptable |
| Operation order | PVC restore before Git | PVC deletion is point of no return; if Git fails after PVC restore, easier to fix manually |
| Flux suspend target | Separate `suspendRef` field | Support app-of-apps; suspend root to prevent parent from un-suspending child |
| Suspend target validation | Runtime check for root + periodic verification | Best-effort detection at start; re-verify before critical steps (PVC delete, Git commit) |
| Workload scale-up | Let Flux handle it | GitOps principle: desired state in Git, Flux enforces it |

---

## Failure Handling

### Rollback Preconditions

The RollbackRequest controller validates before proceeding:

1. **UpgradeRequest must be terminal** - Only allow rollback of completed (`Complete=True`) or failed (`Complete=False`) UpgradeRequests. Reject if upgrade is still in progress to avoid race conditions.

2. **Snapshots must exist** - The referenced UpgradeRequest must have snapshot information in its status.

3. **Previous version must be recorded** - The UpgradeRequest must have `status.upgrade.previousVersion` set.

### PVC Deletion Safety

PVC deletion uses Kubernetes' built-in protection:

1. **Scale down workload** - Set replicas to 0
2. **Delete PVC** - Issues delete request
3. **Wait for PVC to disappear** - The `kubernetes.io/pvc-protection` finalizer blocks deletion until no pods are mounting the PVC
4. **Timeout with clear error** - If PVC doesn't disappear within timeout (e.g., 5 minutes), fail with actionable message

This approach lets Kubernetes tell us when the PVC is truly unmounted rather than us trying to track pod state.

### Pod Termination Failures

If pods get stuck in `Terminating` state (dead node, stuck finalizers):

- **No automatic force-delete** - Force-deleting pods is risky; if the node is alive the container might still be running
- **Fail with guidance** - Error message guides user: "Pod stuck in Terminating state. Check node health, then consider `kubectl delete pod <name> --grace-period=0 --force` and retry rollback."
- **User can retry** - After manual intervention, user creates a new RollbackRequest

### Failure at Each Step

| Step | Failure | State After Failure | Recovery |
|------|---------|---------------------|----------|
| 1. Validate | UpgradeRequest not found | No changes | Fix reference, retry |
| 2. Suspend | Kustomization not found | No changes | Fix ManagedApp config, retry |
| 3. Scale down | Timeout waiting for pods | Workload scaled to 0, pods terminating | Manual pod cleanup, retry |
| 4. Delete PVCs | PVC stuck (finalizer) | Workload down, PVCs in Terminating | Wait for unmount or force-delete pods, retry |
| 5. Create PVCs | Snapshot not found/ready | Workload down, old PVCs deleted | Manual PVC recreation needed |
| 6. Wait for bind | PVC won't bind | Workload down, new PVCs pending | Check storage class, CSI driver |
| 7. Git revert | API error | Workload down, PVCs restored, wrong version in Git | Retry or manual Git fix |
| 8. Resume Flux | Kustomization error | PVCs restored, Git reverted, Flux suspended | Manual Flux resume |
| 9. Scale up | Flux reconciliation fails | Everything restored, workload not starting | Debug app startup |
| 10. Health check | Timeout | App running but unhealthy | Debug app, consider re-rollback |

### Point of No Return

For rollback, **PVC deletion (step 4)** is the point of no return:

- **Before step 4**: Workload is down but data is intact. Safe to abort and resume Flux.
- **After step 4**: Old PVCs are gone. Must complete PVC restoration before resuming.

The controller tracks this via the `VolumesRestored` condition. If rollback fails after PVC deletion but before restoration completes, the error message clearly indicates manual intervention is needed.

---

## Future Work

Items identified during Phase 3 design that are deferred to future phases:

| Item | Current Approach | Future Options |
|------|------------------|----------------|
| **Git Revert Command** | Direct version update via YAML editor | Use `git revert <sha>` for automatic multi-file rollback |
| **Multi-file Rollback** | Single-file manifests only | Track all changed files or use git revert for atomic multi-file rollback |
| **Direct Snapshot Reference** | RollbackRequest references UpgradeRequest | Add `snapshotRef` for rollback to arbitrary snapshots (disaster recovery) |
| **Rollback Dry Run** | Validation only | Show what would be restored without applying changes |

---

## Next Steps

All design questions are resolved. Implementation can proceed in this order:

1. **Update UpgradeRequest controller** - Add scale-down before snapshot for consistent snapshots
2. **RollbackRequest CRD** - Scaffold and define types
3. **ManagedApp updates** - Add `autoRollback` and `suspendRef` fields
4. **Workload scaler** - Scale down/up helpers (shared with upgrade flow)
5. **Snapshot manager additions** - PVC delete, restore from snapshot, wait for bound
6. **Git revert helper** - Commit message format
7. **RollbackRequest controller** - Main orchestration
8. **Auto-rollback integration** - Update UpgradeRequest controller to create RollbackRequest on failure
9. **Tests** - Unit and integration tests

See [Architecture](architecture.md#implementation-phases) for the full roadmap.

---

## Phase 2 Prerequisites

Phase 3 requires changes to the existing Phase 2 implementation before rollback can work correctly. These are listed as step 1 in the implementation order above, but are detailed here for clarity:

### Required Changes to UpgradeRequest Controller

| Change | Reason |
|--------|--------|
| Scale down workload before snapshot | Application-consistent snapshots (clean shutdown, no in-flight transactions) |
| Wait for PVC to be unmounted | Use `pvc-protection` finalizer to verify pods are gone |
| Add `suspendRef` to ManagedApp | Support app-of-apps patterns where parent manages child Kustomizations |
| Validate suspend target is root | Prevent parent from un-suspending child mid-operation |
| Re-verify suspend before Git commit | Catch external un-suspend before point of no return |

### Why These Are Prerequisites

Rollback restores from snapshots created during upgrade. If the upgrade flow doesn't create consistent snapshots, rollback will restore inconsistent data. Therefore:

1. **Snapshot consistency** must be fixed in the upgrade flow first
2. **Suspend handling** must be robust in upgrade before we add rollback (same logic)
3. **Workload scaler** is shared between upgrade and rollback

The Phase 2 design document should be updated to reflect these changes once implemented.
