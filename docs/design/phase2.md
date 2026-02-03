# Phase 2 Implementation Plan: Upgrade Workflow

This document outlines the detailed implementation plan for Phase 2 of FluxUp.

## Phase 2 Goals

From the architecture document:
- UpgradeRequest CRD and controller
- Git manager interface + Gitea implementation
- VolumeSnapshot creation before upgrade
- Flux Kustomization suspend/resume integration
- Health check after upgrade (wait for HelmRelease Ready)

---

## Upgrade Flow Overview

The upgrade workflow orchestrates a safe application upgrade. Progress is tracked via **conditions** (following Kubernetes API conventions - phase fields are discouraged).

```
User creates UpgradeRequest
         │
         ▼
┌────────────────────────┐
│ 1. Validate request    │◀── Check ManagedApp exists, has update
│    + validate suspend  │    Validate suspend target is root
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 2. Suspend Flux        │◀── Prevent premature reconciliation
│    Kustomization       │    Condition: Suspended=True
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 3. Scale down          │◀── Condition: WorkloadStopped=True
│    workload to 0       │    Ensures clean shutdown before snapshot
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 4. Create snapshots    │◀── Condition: SnapshotReady=False (InProgress)
│    for configured PVCs │    Now capturing application-consistent state
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 5. Wait for snapshots  │◀── Condition: SnapshotReady=True
│    ReadyToUse = true   │
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ 6. Verify still        │◀── Re-verify suspend before point of no return
│    suspended           │    Catch external un-suspend attempts
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
│    (HelmRelease Ready) │
└──────────┬─────────────┘
           │
     ┌─────┴─────┐
     │           │
  Success     Failure
     │           │
     ▼           ▼
┌─────────┐ ┌──────────┐
│Complete │ │ Complete │
│ =True   │ │ =False   │
└─────────┘ └──────────┘
```

The current step is **derived from conditions**: if `SnapshotReady=True` and `GitCommitted=False`, we're in the "committing" step.

**Note:** Step 3 (scale down) only runs if `workloadRef` is configured. This ensures application-consistent snapshots by cleanly shutting down the workload before capturing PVC state.

---

## Step 1: UpgradeRequest CRD

### 1.1 Type Definitions

```go
// api/v1alpha1/upgraderequest_types.go

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

// SnapshotStatus records snapshot creation details
type SnapshotStatus struct {
    // PVC snapshots created
    PVCSnapshots []PVCSnapshotInfo `json:"pvcSnapshots,omitempty"`

    // When snapshots were created
    CreatedAt *metav1.Time `json:"createdAt,omitempty"`

    // When snapshots became ready
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
    PreviousVersion *VersionInfo `json:"previousVersion,omitempty"`

    // New version after upgrade
    NewVersion *VersionInfo `json:"newVersion,omitempty"`

    // Git commit SHA
    GitCommit string `json:"gitCommit,omitempty"`

    // When upgrade started
    StartedAt *metav1.Time `json:"startedAt,omitempty"`

    // When upgrade completed
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// HealthCheckStatus records health check results
type HealthCheckStatus struct {
    // Status: Passed, Failed, Pending
    Status string `json:"status,omitempty"`

    // When health check was performed
    CheckedAt *metav1.Time `json:"checkedAt,omitempty"`

    // Failure message if failed
    Message string `json:"message,omitempty"`
}
```

### 1.2 Condition Types

```go
const (
    // ConditionTypeSuspended indicates the Flux Kustomization is suspended
    ConditionTypeSuspended = "Suspended"

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
```

### 1.3 Kubebuilder Markers

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Complete",type="string",JSONPath=".status.conditions[?(@.type=='Complete')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Complete')].reason"
// +kubebuilder:printcolumn:name="ManagedApp",type="string",JSONPath=".spec.managedAppRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
```

### 1.4 Create the CRD

```bash
kubebuilder create api --group fluxup --version v1alpha1 --kind UpgradeRequest
```

---

## Step 2: Git Manager Interface

### 2.1 Interface Design

The Git manager abstracts Git operations. Initial implementation targets Gitea, with room for GitHub/GitLab/generic later.

```go
// internal/git/interface.go

package git

import (
    "context"
)

// FileChange represents a file modification
type FileChange struct {
    Path    string // File path in repository
    Content []byte // New file content
}

// CommitInfo contains commit metadata
type CommitInfo struct {
    SHA     string
    Message string
    Author  string
    URL     string
}

// Manager abstracts Git operations
type Manager interface {
    // ReadFile reads a file from the repository
    ReadFile(ctx context.Context, path string) ([]byte, error)

    // CommitFile commits a single file change
    CommitFile(ctx context.Context, change FileChange, message string) (*CommitInfo, error)

    // CommitFiles commits multiple file changes atomically
    CommitFiles(ctx context.Context, changes []FileChange, message string) (*CommitInfo, error)

    // GetLatestCommit returns the latest commit on the branch
    GetLatestCommit(ctx context.Context) (*CommitInfo, error)
}
```

### 2.2 Configuration

```go
// internal/git/config.go

// Config holds Git backend configuration
type Config struct {
    // Backend type: gitea, github, gitlab, generic
    Backend string

    // Repository URL (https://gitea.example.com/org/repo)
    RepoURL string

    // Branch to operate on
    Branch string

    // Authentication
    Token string

    // For generic backend: local clone path
    ClonePath string
}

// NewManager creates a Git manager based on configuration
func NewManager(cfg Config) (Manager, error) {
    switch cfg.Backend {
    case "gitea":
        return NewGiteaManager(cfg)
    // case "github":
    //     return NewGitHubManager(cfg)
    // case "generic":
    //     return NewGenericManager(cfg)
    default:
        return nil, fmt.Errorf("unsupported git backend: %s", cfg.Backend)
    }
}
```

### 2.3 Gitea Implementation

```go
// internal/git/gitea.go

package git

import (
    "context"
    "encoding/base64"
    "fmt"
    "net/url"

    "code.gitea.io/sdk/gitea"
)

type GiteaManager struct {
    client *gitea.Client
    owner  string
    repo   string
    branch string
}

func NewGiteaManager(cfg Config) (*GiteaManager, error) {
    // Parse repo URL to extract owner and repo
    parsed, err := url.Parse(cfg.RepoURL)
    if err != nil {
        return nil, fmt.Errorf("parsing repo URL: %w", err)
    }

    // URL format: https://gitea.example.com/owner/repo
    parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
    if len(parts) < 2 {
        return nil, fmt.Errorf("invalid repo URL format: %s", cfg.RepoURL)
    }
    owner, repo := parts[0], parts[1]

    // Create Gitea client
    baseURL := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
    client, err := gitea.NewClient(baseURL, gitea.SetToken(cfg.Token))
    if err != nil {
        return nil, fmt.Errorf("creating gitea client: %w", err)
    }

    branch := cfg.Branch
    if branch == "" {
        branch = "main"
    }

    return &GiteaManager{
        client: client,
        owner:  owner,
        repo:   repo,
        branch: branch,
    }, nil
}

func (g *GiteaManager) ReadFile(ctx context.Context, path string) ([]byte, error) {
    content, resp, err := g.client.GetContents(g.owner, g.repo, g.branch, path)
    if err != nil {
        if resp != nil && resp.StatusCode == 404 {
            return nil, fmt.Errorf("file not found: %s", path)
        }
        return nil, fmt.Errorf("reading file: %w", err)
    }

    // Content is base64 encoded
    decoded, err := base64.StdEncoding.DecodeString(*content.Content)
    if err != nil {
        return nil, fmt.Errorf("decoding content: %w", err)
    }

    return decoded, nil
}

func (g *GiteaManager) CommitFile(ctx context.Context, change FileChange, message string) (*CommitInfo, error) {
    // Get current file SHA for update (required by Gitea API)
    existing, _, _ := g.client.GetContents(g.owner, g.repo, g.branch, change.Path)

    opts := gitea.UpdateFileOptions{
        FileOptions: gitea.FileOptions{
            Message: message,
            BranchName: g.branch,
        },
        Content: base64.StdEncoding.EncodeToString(change.Content),
    }

    if existing != nil && existing.SHA != nil {
        opts.SHA = *existing.SHA
    }

    resp, _, err := g.client.UpdateFile(g.owner, g.repo, change.Path, opts)
    if err != nil {
        return nil, fmt.Errorf("committing file: %w", err)
    }

    return &CommitInfo{
        SHA:     resp.Commit.SHA,
        Message: message,
        URL:     resp.Commit.HTMLURL,
    }, nil
}

func (g *GiteaManager) CommitFiles(ctx context.Context, changes []FileChange, message string) (*CommitInfo, error) {
    // Gitea doesn't have atomic multi-file commit via API
    // For now, commit files sequentially
    // TODO: Consider using git CLI for atomic commits
    var lastCommit *CommitInfo
    for _, change := range changes {
        commit, err := g.CommitFile(ctx, change, message)
        if err != nil {
            return nil, err
        }
        lastCommit = commit
    }
    return lastCommit, nil
}

func (g *GiteaManager) GetLatestCommit(ctx context.Context) (*CommitInfo, error) {
    commits, _, err := g.client.ListRepoCommits(g.owner, g.repo, gitea.ListCommitOptions{
        SHA: g.branch,
        ListOptions: gitea.ListOptions{
            PageSize: 1,
        },
    })
    if err != nil {
        return nil, fmt.Errorf("getting latest commit: %w", err)
    }

    if len(commits) == 0 {
        return nil, fmt.Errorf("no commits found")
    }

    return &CommitInfo{
        SHA:     commits[0].SHA,
        Message: commits[0].RepoCommit.Message,
        Author:  commits[0].Author.UserName,
        URL:     commits[0].HTMLURL,
    }, nil
}
```

### 2.4 Commit Message Format

```go
// internal/git/commit.go

// FormatCommitMessage creates a standardized commit message
func FormatCommitMessage(appName, fromVersion, toVersion string, snapshots []string) string {
    var sb strings.Builder

    sb.WriteString(fmt.Sprintf("chore(deps): update %s to %s\n\n", appName, toVersion))
    sb.WriteString("Upgrade triggered via FluxUp\n")

    if len(snapshots) > 0 {
        sb.WriteString("\nPre-upgrade snapshots:\n")
        for _, snap := range snapshots {
            sb.WriteString(fmt.Sprintf("- %s\n", snap))
        }
    }

    sb.WriteString(fmt.Sprintf("\nManaged-By: fluxup\n"))
    sb.WriteString(fmt.Sprintf("Previous-Version: %s\n", fromVersion))

    return sb.String()
}
```

---

## Step 3: YAML Editor

### 3.1 Approach: kyaml

We use `sigs.k8s.io/kustomize/kyaml` - the same library Flux's image-automation-controller uses. It preserves YAML structure and comments.

```go
// internal/yaml/editor.go

package yaml

import (
    "fmt"

    "sigs.k8s.io/kustomize/kyaml/yaml"
)

// Editor modifies YAML content while preserving structure
type Editor struct{}

func NewEditor() *Editor {
    return &Editor{}
}

// UpdateHelmReleaseVersion updates the chart version in a HelmRelease
func (e *Editor) UpdateHelmReleaseVersion(content []byte, newVersion string) ([]byte, error) {
    obj, err := yaml.Parse(string(content))
    if err != nil {
        return nil, fmt.Errorf("parsing YAML: %w", err)
    }

    // Navigate to spec.chart.spec.version
    path := []string{"spec", "chart", "spec", "version"}
    if err := obj.PipeE(
        yaml.LookupCreate(yaml.ScalarNode, path...),
        yaml.FieldSetter{StringValue: newVersion},
    ); err != nil {
        return nil, fmt.Errorf("setting version: %w", err)
    }

    result, err := obj.String()
    if err != nil {
        return nil, fmt.Errorf("serializing YAML: %w", err)
    }

    return []byte(result), nil
}

// UpdateImageTag updates an image tag in values or a Deployment
func (e *Editor) UpdateImageTag(content []byte, imagePath []string, newTag string) ([]byte, error) {
    obj, err := yaml.Parse(string(content))
    if err != nil {
        return nil, fmt.Errorf("parsing YAML: %w", err)
    }

    if err := obj.PipeE(
        yaml.LookupCreate(yaml.ScalarNode, imagePath...),
        yaml.FieldSetter{StringValue: newTag},
    ); err != nil {
        return nil, fmt.Errorf("setting image tag: %w", err)
    }

    result, err := obj.String()
    if err != nil {
        return nil, fmt.Errorf("serializing YAML: %w", err)
    }

    return []byte(result), nil
}
```

### 3.2 Version Update Logic

```go
// internal/yaml/version.go

// DetermineUpdatePath figures out what to update based on datasource
func DetermineUpdatePath(datasource string) ([]string, error) {
    switch datasource {
    case "helm":
        // HelmRelease chart version
        return []string{"spec", "chart", "spec", "version"}, nil
    case "docker":
        // This varies by manifest type - caller must specify
        return nil, fmt.Errorf("docker image path must be specified explicitly")
    default:
        return nil, fmt.Errorf("unknown datasource: %s", datasource)
    }
}
```

---

## Step 4: Snapshot Manager

### 4.1 Interface

```go
// internal/snapshot/manager.go

package snapshot

import (
    "context"
    "fmt"
    "time"

    snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

// Manager handles VolumeSnapshot lifecycle
type Manager struct {
    client client.Client
}

func NewManager(c client.Client) *Manager {
    return &Manager{client: c}
}

// SnapshotRequest defines what to snapshot
type SnapshotRequest struct {
    PVCName                 string
    PVCNamespace            string
    SnapshotName            string
    VolumeSnapshotClassName string
    Labels                  map[string]string
}

// CreateSnapshot creates a VolumeSnapshot for a PVC
func (m *Manager) CreateSnapshot(ctx context.Context, req SnapshotRequest) (*snapshotv1.VolumeSnapshot, error) {
    // Verify PVC exists
    var pvc corev1.PersistentVolumeClaim
    pvcKey := types.NamespacedName{Name: req.PVCName, Namespace: req.PVCNamespace}
    if err := m.client.Get(ctx, pvcKey, &pvc); err != nil {
        return nil, fmt.Errorf("PVC not found: %w", err)
    }

    snapshot := &snapshotv1.VolumeSnapshot{
        ObjectMeta: metav1.ObjectMeta{
            Name:      req.SnapshotName,
            Namespace: req.PVCNamespace,
            Labels:    req.Labels,
        },
        Spec: snapshotv1.VolumeSnapshotSpec{
            Source: snapshotv1.VolumeSnapshotSource{
                PersistentVolumeClaimName: &req.PVCName,
            },
        },
    }

    if req.VolumeSnapshotClassName != "" {
        snapshot.Spec.VolumeSnapshotClassName = &req.VolumeSnapshotClassName
    }

    if err := m.client.Create(ctx, snapshot); err != nil {
        return nil, fmt.Errorf("creating snapshot: %w", err)
    }

    return snapshot, nil
}

// WaitForSnapshotReady waits for a snapshot to be ready
func (m *Manager) WaitForSnapshotReady(ctx context.Context, name, namespace string, timeout time.Duration) error {
    deadline := time.Now().Add(timeout)
    key := types.NamespacedName{Name: name, Namespace: namespace}

    for time.Now().Before(deadline) {
        var snapshot snapshotv1.VolumeSnapshot
        if err := m.client.Get(ctx, key, &snapshot); err != nil {
            return fmt.Errorf("getting snapshot: %w", err)
        }

        if snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse {
            return nil
        }

        // Check for errors
        if snapshot.Status != nil && snapshot.Status.Error != nil && snapshot.Status.Error.Message != nil {
            return fmt.Errorf("snapshot error: %s", *snapshot.Status.Error.Message)
        }

        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(5 * time.Second):
            // Continue polling
        }
    }

    return fmt.Errorf("timeout waiting for snapshot %s/%s to be ready", namespace, name)
}

// CreateSnapshotsForUpgrade creates pre-upgrade snapshots for all configured PVCs
func (m *Manager) CreateSnapshotsForUpgrade(ctx context.Context, appName, appNamespace string, pvcs []PVCRef, snapshotClass string) ([]SnapshotInfo, error) {
    timestamp := time.Now().Format("20060102-150405")
    var snapshots []SnapshotInfo

    for _, pvc := range pvcs {
        ns := pvc.Namespace
        if ns == "" {
            ns = appNamespace
        }

        snapshotName := fmt.Sprintf("%s-%s-pre-upgrade-%s", appName, pvc.Name, timestamp)

        req := SnapshotRequest{
            PVCName:                 pvc.Name,
            PVCNamespace:            ns,
            SnapshotName:            snapshotName,
            VolumeSnapshotClassName: snapshotClass,
            Labels: map[string]string{
                "fluxup.dev/managed-app": appName,
                "fluxup.dev/purpose":     "pre-upgrade",
            },
        }

        snapshot, err := m.CreateSnapshot(ctx, req)
        if err != nil {
            return nil, fmt.Errorf("creating snapshot for PVC %s: %w", pvc.Name, err)
        }

        snapshots = append(snapshots, SnapshotInfo{
            PVCName:      pvc.Name,
            SnapshotName: snapshot.Name,
            Namespace:    snapshot.Namespace,
        })
    }

    return snapshots, nil
}

// SnapshotInfo holds snapshot creation results
type SnapshotInfo struct {
    PVCName      string
    SnapshotName string
    Namespace    string
}
```

### 4.2 Retention Policy

> **Implementation Note:** The design below shows both `maxCount` and `maxAge`. During implementation, we simplified to **`maxCount` only** (generational approach). See "Design Changes During Implementation" in the Implementation Status section.

```go
// internal/snapshot/retention.go (ORIGINAL DESIGN - simplified in implementation)

// ApplyRetentionPolicy cleans up old snapshots based on policy
func (m *Manager) ApplyRetentionPolicy(ctx context.Context, appName, namespace string, policy *RetentionPolicy) error {
    // List snapshots for this app
    var snapshots snapshotv1.VolumeSnapshotList
    if err := m.client.List(ctx, &snapshots,
        client.InNamespace(namespace),
        client.MatchingLabels{"fluxup.dev/managed-app": appName},
    ); err != nil {
        return err
    }

    // Sort by creation time (newest first)
    sort.Slice(snapshots.Items, func(i, j int) bool {
        return snapshots.Items[i].CreationTimestamp.After(snapshots.Items[j].CreationTimestamp.Time)
    })

    // Apply maxCount
    if policy.MaxCount != nil && len(snapshots.Items) > *policy.MaxCount {
        for _, snap := range snapshots.Items[*policy.MaxCount:] {
            if err := m.client.Delete(ctx, &snap); err != nil {
                return fmt.Errorf("deleting snapshot %s: %w", snap.Name, err)
            }
        }
    }

    // Apply maxAge
    if policy.MaxAge != "" {
        maxAge, err := time.ParseDuration(policy.MaxAge)
        if err != nil {
            return fmt.Errorf("parsing maxAge: %w", err)
        }
        cutoff := time.Now().Add(-maxAge)

        for _, snap := range snapshots.Items {
            if snap.CreationTimestamp.Time.Before(cutoff) {
                if err := m.client.Delete(ctx, &snap); err != nil {
                    return fmt.Errorf("deleting snapshot %s: %w", snap.Name, err)
                }
            }
        }
    }

    return nil
}
```

---

## Step 5: Flux Suspend/Resume

### 5.1 Helpers

```go
// internal/flux/suspend.go

package flux

import (
    "context"
    "fmt"
    "time"

    kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

// Helper provides Flux-related operations
type Helper struct {
    client client.Client
}

func NewHelper(c client.Client) *Helper {
    return &Helper{client: c}
}

// SuspendKustomization suspends a Flux Kustomization
func (h *Helper) SuspendKustomization(ctx context.Context, name, namespace string) error {
    var ks kustomizev1.Kustomization
    key := types.NamespacedName{Name: name, Namespace: namespace}

    if err := h.client.Get(ctx, key, &ks); err != nil {
        return fmt.Errorf("getting kustomization: %w", err)
    }

    if ks.Spec.Suspend {
        return nil // Already suspended
    }

    patch := client.MergeFrom(ks.DeepCopy())
    ks.Spec.Suspend = true

    if err := h.client.Patch(ctx, &ks, patch); err != nil {
        return fmt.Errorf("suspending kustomization: %w", err)
    }

    return nil
}

// ResumeKustomization resumes a Flux Kustomization
func (h *Helper) ResumeKustomization(ctx context.Context, name, namespace string) error {
    var ks kustomizev1.Kustomization
    key := types.NamespacedName{Name: name, Namespace: namespace}

    if err := h.client.Get(ctx, key, &ks); err != nil {
        return fmt.Errorf("getting kustomization: %w", err)
    }

    if !ks.Spec.Suspend {
        return nil // Already resumed
    }

    patch := client.MergeFrom(ks.DeepCopy())
    ks.Spec.Suspend = false

    if err := h.client.Patch(ctx, &ks, patch); err != nil {
        return fmt.Errorf("resuming kustomization: %w", err)
    }

    return nil
}

// WaitForReconciliation waits for Kustomization to reconcile
func (h *Helper) WaitForReconciliation(ctx context.Context, name, namespace string, timeout time.Duration) error {
    deadline := time.Now().Add(timeout)
    key := types.NamespacedName{Name: name, Namespace: namespace}

    // Get initial state
    var initial kustomizev1.Kustomization
    if err := h.client.Get(ctx, key, &initial); err != nil {
        return err
    }
    initialGeneration := initial.Status.ObservedGeneration

    for time.Now().Before(deadline) {
        var ks kustomizev1.Kustomization
        if err := h.client.Get(ctx, key, &ks); err != nil {
            return err
        }

        // Check if reconciliation happened (generation changed)
        if ks.Status.ObservedGeneration > initialGeneration {
            // Check Ready condition
            for _, cond := range ks.Status.Conditions {
                if cond.Type == "Ready" {
                    if cond.Status == "True" {
                        return nil
                    }
                    if cond.Status == "False" {
                        return fmt.Errorf("kustomization reconciliation failed: %s", cond.Message)
                    }
                }
            }
        }

        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(5 * time.Second):
            // Continue polling
        }
    }

    return fmt.Errorf("timeout waiting for kustomization reconciliation")
}
```

---

## Step 6: UpgradeRequest Controller

### 6.1 Controller Structure

```go
// internal/controller/upgraderequest_controller.go

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
    yamlpkg "github.com/nbenn/fluxup/internal/yaml"
)

// UpgradeRequestReconciler reconciles an UpgradeRequest
type UpgradeRequestReconciler struct {
    client.Client
    Scheme          *runtime.Scheme
    GitManager      git.Manager
    SnapshotManager *snapshot.Manager
    FluxHelper      *flux.Helper
    YAMLEditor      *yamlpkg.Editor
}

// +kubebuilder:rbac:groups=fluxup.dev,resources=upgraderequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fluxup.dev,resources=upgraderequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fluxup.dev,resources=upgraderequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

func (r *UpgradeRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // 1. Fetch UpgradeRequest
    var upgrade fluxupv1alpha1.UpgradeRequest
    if err := r.Get(ctx, req.NamespacedName, &upgrade); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    logger.Info("Reconciling UpgradeRequest",
        "managedApp", upgrade.Spec.ManagedAppRef.Name)

    // 2. Check if already complete (terminal state)
    if meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete) {
        return ctrl.Result{}, nil
    }
    // Also check for failed state (Complete=False with failure reason)
    if cond := meta.FindStatusCondition(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete); cond != nil && cond.Status == metav1.ConditionFalse {
        return ctrl.Result{}, nil
    }

    // 3. Determine current step from conditions and handle accordingly
    if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
        return r.handleSuspend(ctx, &upgrade)
    }
    if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSnapshotReady) {
        return r.handleSnapshotting(ctx, &upgrade)
    }
    if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeGitCommitted) {
        return r.handleCommitting(ctx, &upgrade)
    }
    if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeReconciled) {
        return r.handleReconciling(ctx, &upgrade)
    }
    if !meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeHealthy) {
        return r.handleHealthChecking(ctx, &upgrade)
    }

    // All conditions met - mark complete
    return r.handleCompleted(ctx, &upgrade)
}
```

### 6.2 Step Handlers

```go
// handleSuspend validates the request and suspends Flux
func (r *UpgradeRequestReconciler) handleSuspend(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // Fetch the ManagedApp
    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
    }

    // Check for available update
    if upgrade.Spec.TargetVersion == nil && app.Status.AvailableUpdate == nil {
        return r.setFailed(ctx, upgrade, "NoUpdateAvailable", "No update available for this app")
    }

    // Determine target version
    targetVersion := upgrade.Spec.TargetVersion
    if targetVersion == nil {
        targetVersion = app.Status.AvailableUpdate
    }

    logger.Info("Starting upgrade",
        "app", app.Name,
        "currentVersion", app.Status.CurrentVersion,
        "targetVersion", targetVersion)

    // Suspend Flux Kustomization
    ksRef := app.Spec.KustomizationRef
    ksNS := ksRef.Namespace
    if ksNS == "" {
        ksNS = "flux-system"
    }

    if err := r.FluxHelper.SuspendKustomization(ctx, ksRef.Name, ksNS); err != nil {
        return r.setFailed(ctx, upgrade, "SuspendFailed", err.Error())
    }

    // Set Suspended condition
    meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeSuspended,
        Status:             metav1.ConditionTrue,
        Reason:             "KustomizationSuspended",
        Message:            fmt.Sprintf("Suspended %s/%s", ksNS, ksRef.Name),
        ObservedGeneration: upgrade.Generation,
    })

    // If snapshots disabled, also mark SnapshotReady as true (skipped)
    if upgrade.Spec.SkipSnapshot || app.Spec.VolumeSnapshots == nil || !app.Spec.VolumeSnapshots.Enabled {
        meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
            Type:               fluxupv1alpha1.ConditionTypeSnapshotReady,
            Status:             metav1.ConditionTrue,
            Reason:             "SnapshotsSkipped",
            Message:            "Snapshots disabled or not configured",
            ObservedGeneration: upgrade.Generation,
        })
    }

    if err := r.Status().Update(ctx, upgrade); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{Requeue: true}, nil
}

// handleSnapshotting creates pre-upgrade snapshots
func (r *UpgradeRequestReconciler) handleSnapshotting(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
    }

    // Create snapshots if not already created
    if upgrade.Status.Snapshot == nil || len(upgrade.Status.Snapshot.PVCSnapshots) == 0 {
        snapshots, err := r.SnapshotManager.CreateSnapshotsForUpgrade(
            ctx,
            app.Name,
            app.Namespace,
            app.Spec.VolumeSnapshots.PVCs,
            app.Spec.VolumeSnapshots.VolumeSnapshotClassName,
        )
        if err != nil {
            return r.setFailed(ctx, upgrade, "SnapshotCreationFailed", err.Error())
        }

        now := metav1.Now()
        upgrade.Status.Snapshot = &fluxupv1alpha1.SnapshotStatus{
            CreatedAt: &now,
        }
        for _, snap := range snapshots {
            upgrade.Status.Snapshot.PVCSnapshots = append(upgrade.Status.Snapshot.PVCSnapshots,
                fluxupv1alpha1.PVCSnapshotInfo{
                    PVCName:      snap.PVCName,
                    SnapshotName: snap.SnapshotName,
                })
        }

        if err := r.Status().Update(ctx, upgrade); err != nil {
            return ctrl.Result{}, err
        }

        logger.Info("Created pre-upgrade snapshots", "count", len(snapshots))
    }

    // Wait for all snapshots to be ready
    for _, snap := range upgrade.Status.Snapshot.PVCSnapshots {
        if err := r.SnapshotManager.WaitForSnapshotReady(ctx, snap.SnapshotName, app.Namespace, 5*time.Minute); err != nil {
            // Not ready yet - requeue
            if ctx.Err() == nil {
                return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
            }
            return r.setFailed(ctx, upgrade, "SnapshotNotReady", err.Error())
        }
    }

    // All snapshots ready
    now := metav1.Now()
    upgrade.Status.Snapshot.ReadyAt = &now
    meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeSnapshotReady,
        Status:             metav1.ConditionTrue,
        Reason:             "SnapshotsReady",
        Message:            fmt.Sprintf("%d snapshots ready", len(upgrade.Status.Snapshot.PVCSnapshots)),
        ObservedGeneration: upgrade.Generation,
    })

    if err := r.Status().Update(ctx, upgrade); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{Requeue: true}, nil
}

// handleCommitting commits the version change to Git
func (r *UpgradeRequestReconciler) handleCommitting(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
    }

    // Get target version
    targetVersion := upgrade.Spec.TargetVersion
    if targetVersion == nil {
        targetVersion = app.Status.AvailableUpdate
    }

    // Read current file from Git
    content, err := r.GitManager.ReadFile(ctx, app.Spec.GitPath)
    if err != nil {
        return r.setFailed(ctx, upgrade, "GitReadFailed", err.Error())
    }

    // Update version in YAML
    var newContent []byte
    if targetVersion.Chart != "" {
        newContent, err = r.YAMLEditor.UpdateHelmReleaseVersion(content, targetVersion.Chart)
    } else if len(targetVersion.Images) > 0 {
        // TODO: Handle image updates - need path from ManagedApp spec
        return r.setFailed(ctx, upgrade, "NotImplemented", "Image updates not yet implemented")
    }
    if err != nil {
        return r.setFailed(ctx, upgrade, "YAMLUpdateFailed", err.Error())
    }

    // Build commit message
    var snapshotNames []string
    if upgrade.Status.Snapshot != nil {
        for _, snap := range upgrade.Status.Snapshot.PVCSnapshots {
            snapshotNames = append(snapshotNames, snap.SnapshotName)
        }
    }

    currentVersion := ""
    if app.Status.CurrentVersion != nil {
        currentVersion = app.Status.CurrentVersion.Chart
    }

    message := git.FormatCommitMessage(app.Name, currentVersion, targetVersion.Chart, snapshotNames)

    // Commit to Git
    commitInfo, err := r.GitManager.CommitFile(ctx, git.FileChange{
        Path:    app.Spec.GitPath,
        Content: newContent,
    }, message)
    if err != nil {
        return r.setFailed(ctx, upgrade, "GitCommitFailed", err.Error())
    }

    logger.Info("Committed version change to Git",
        "commit", commitInfo.SHA,
        "newVersion", targetVersion.Chart)

    // Update status
    now := metav1.Now()
    upgrade.Status.Upgrade = &fluxupv1alpha1.UpgradeStatus{
        PreviousVersion: app.Status.CurrentVersion,
        NewVersion:      targetVersion,
        GitCommit:       commitInfo.SHA,
        StartedAt:       &now,
    }

    meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeGitCommitted,
        Status:             metav1.ConditionTrue,
        Reason:             "Committed",
        Message:            fmt.Sprintf("Commit %s", commitInfo.SHA),
        ObservedGeneration: upgrade.Generation,
    })

    if err := r.Status().Update(ctx, upgrade); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{Requeue: true}, nil
}

// handleReconciling resumes Flux and waits for reconciliation
func (r *UpgradeRequestReconciler) handleReconciling(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
    }

    // Resume Flux Kustomization (if still suspended)
    ksRef := app.Spec.KustomizationRef
    ksNS := ksRef.Namespace
    if ksNS == "" {
        ksNS = "flux-system"
    }

    if err := r.FluxHelper.ResumeKustomization(ctx, ksRef.Name, ksNS); err != nil {
        return r.setFailed(ctx, upgrade, "ResumeFailed", err.Error())
    }

    // Update Suspended condition to False
    meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeSuspended,
        Status:             metav1.ConditionFalse,
        Reason:             "KustomizationResumed",
        Message:            fmt.Sprintf("Resumed %s/%s", ksNS, ksRef.Name),
        ObservedGeneration: upgrade.Generation,
    })

    logger.Info("Resumed Flux Kustomization, waiting for reconciliation")

    // Wait for Flux to reconcile (with timeout)
    if err := r.FluxHelper.WaitForReconciliation(ctx, ksRef.Name, ksNS, 10*time.Minute); err != nil {
        // Check if it's a timeout vs actual failure
        if ctx.Err() == nil {
            if updateErr := r.Status().Update(ctx, upgrade); updateErr != nil {
                return ctrl.Result{}, updateErr
            }
            return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
        }
        return r.setFailed(ctx, upgrade, "ReconciliationFailed", err.Error())
    }

    // Reconciliation complete
    meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeReconciled,
        Status:             metav1.ConditionTrue,
        Reason:             "ReconciliationSucceeded",
        Message:            "Flux reconciled the changes",
        ObservedGeneration: upgrade.Generation,
    })

    if err := r.Status().Update(ctx, upgrade); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{Requeue: true}, nil
}

// handleHealthChecking verifies the upgrade succeeded
func (r *UpgradeRequestReconciler) handleHealthChecking(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
    }

    // Check workload health
    healthy, err := r.checkWorkloadHealth(ctx, app)
    if err != nil {
        return r.setFailed(ctx, upgrade, "HealthCheckError", err.Error())
    }

    if !healthy {
        // Not healthy yet - check timeout
        timeout := 5 * time.Minute
        if app.Spec.HealthCheck != nil && app.Spec.HealthCheck.Timeout != "" {
            if parsed, err := time.ParseDuration(app.Spec.HealthCheck.Timeout); err == nil {
                timeout = parsed
            }
        }

        if upgrade.Status.Upgrade != nil && upgrade.Status.Upgrade.StartedAt != nil {
            elapsed := time.Since(upgrade.Status.Upgrade.StartedAt.Time)
            if elapsed > timeout {
                return r.setFailed(ctx, upgrade, "HealthCheckTimeout", "Workload did not become healthy within timeout")
            }
        }

        logger.Info("Workload not yet healthy, waiting...")
        return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
    }

    // Health check passed
    logger.Info("Health check passed, upgrade complete")

    now := metav1.Now()
    upgrade.Status.HealthCheck = &fluxupv1alpha1.HealthCheckStatus{
        Status:    "Passed",
        CheckedAt: &now,
    }
    if upgrade.Status.Upgrade != nil {
        upgrade.Status.Upgrade.CompletedAt = &now
    }

    meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeHealthy,
        Status:             metav1.ConditionTrue,
        Reason:             "HealthCheckPassed",
        Message:            "Workload is healthy",
        ObservedGeneration: upgrade.Generation,
    })

    if err := r.Status().Update(ctx, upgrade); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{Requeue: true}, nil
}

// handleCompleted marks the upgrade as complete and updates ManagedApp
func (r *UpgradeRequestReconciler) handleCompleted(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    app, err := r.getManagedApp(ctx, upgrade)
    if err != nil {
        return r.setFailed(ctx, upgrade, "ManagedAppNotFound", err.Error())
    }

    logger.Info("Upgrade complete")

    meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeComplete,
        Status:             metav1.ConditionTrue,
        Reason:             "UpgradeSucceeded",
        Message:            fmt.Sprintf("Upgraded to %s", upgrade.Status.Upgrade.NewVersion.Chart),
        ObservedGeneration: upgrade.Generation,
    })

    // Update ManagedApp's currentVersion and clear availableUpdate
    if err := r.updateManagedAppAfterUpgrade(ctx, app, upgrade); err != nil {
        logger.Error(err, "Failed to update ManagedApp status after upgrade")
    }

    if err := r.Status().Update(ctx, upgrade); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}
```

### 6.3 Helper Methods

```go
// getManagedApp fetches the referenced ManagedApp
func (r *UpgradeRequestReconciler) getManagedApp(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest) (*fluxupv1alpha1.ManagedApp, error) {
    ref := upgrade.Spec.ManagedAppRef
    ns := ref.Namespace
    if ns == "" {
        ns = upgrade.Namespace
    }

    var app fluxupv1alpha1.ManagedApp
    key := types.NamespacedName{Name: ref.Name, Namespace: ns}
    if err := r.Get(ctx, key, &app); err != nil {
        return nil, err
    }
    return &app, nil
}

// checkWorkloadHealth checks if the ManagedApp's workload is healthy
func (r *UpgradeRequestReconciler) checkWorkloadHealth(ctx context.Context, app *fluxupv1alpha1.ManagedApp) (bool, error) {
    // Reuse logic from ManagedAppReconciler
    // This should be extracted to a shared utility
    for _, cond := range app.Status.Conditions {
        if cond.Type == fluxupv1alpha1.ConditionTypeReady {
            return cond.Status == metav1.ConditionTrue, nil
        }
    }
    return false, nil
}

// setFailed marks the upgrade as failed via Complete=False condition
// Failure handling depends on whether Git commit has happened (point of no return):
// - Before Git commit: Resume Kustomization, user can retry
// - After Git commit: Rollback required (handled by Phase 3), do NOT resume
func (r *UpgradeRequestReconciler) setFailed(ctx context.Context, upgrade *fluxupv1alpha1.UpgradeRequest, reason, message string) (ctrl.Result, error) {
    logger := log.FromContext(ctx)
    logger.Error(nil, "Upgrade failed", "reason", reason, "message", message)

    gitCommitted := meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeGitCommitted)

    if gitCommitted {
        // After Git commit: rollback required
        // Do NOT resume - leave state as-is for rollback
        // Phase 3 will add auto-rollback here
        meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
            Type:               fluxupv1alpha1.ConditionTypeComplete,
            Status:             metav1.ConditionFalse,
            Reason:             reason,
            Message:            fmt.Sprintf("%s - rollback required", message),
            ObservedGeneration: upgrade.Generation,
        })
        logger.Info("Upgrade failed after Git commit - rollback required")
    } else {
        // Before Git commit: safe to resume and abort
        meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
            Type:               fluxupv1alpha1.ConditionTypeComplete,
            Status:             metav1.ConditionFalse,
            Reason:             reason,
            Message:            message,
            ObservedGeneration: upgrade.Generation,
        })

        // Resume Kustomization if we suspended it
        if meta.IsStatusConditionTrue(upgrade.Status.Conditions, fluxupv1alpha1.ConditionTypeSuspended) {
            app, err := r.getManagedApp(ctx, upgrade)
            if err == nil {
                ksRef := app.Spec.KustomizationRef
                ksNS := ksRef.Namespace
                if ksNS == "" {
                    ksNS = "flux-system"
                }
                if resumeErr := r.FluxHelper.ResumeKustomization(ctx, ksRef.Name, ksNS); resumeErr != nil {
                    logger.Error(resumeErr, "Failed to resume Kustomization after failure")
                } else {
                    meta.SetStatusCondition(&upgrade.Status.Conditions, metav1.Condition{
                        Type:               fluxupv1alpha1.ConditionTypeSuspended,
                        Status:             metav1.ConditionFalse,
                        Reason:             "ResumedAfterFailure",
                        Message:            "Kustomization resumed after upgrade failure",
                        ObservedGeneration: upgrade.Generation,
                    })
                }
            }
        }
    }

    if err := r.Status().Update(ctx, upgrade); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}

// updateManagedAppAfterUpgrade updates the ManagedApp status after successful upgrade
func (r *UpgradeRequestReconciler) updateManagedAppAfterUpgrade(ctx context.Context, app *fluxupv1alpha1.ManagedApp, upgrade *fluxupv1alpha1.UpgradeRequest) error {
    app.Status.CurrentVersion = upgrade.Status.Upgrade.NewVersion
    app.Status.AvailableUpdate = nil

    now := metav1.Now()
    app.Status.LastUpgrade = &fluxupv1alpha1.UpgradeInfo{
        FromVersion: upgrade.Status.Upgrade.PreviousVersion,
        ToVersion:   upgrade.Status.Upgrade.NewVersion,
        Timestamp:   &now,
        Status:      "Succeeded",
    }

    // Clear UpdateAvailable condition
    meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeUpdateAvailable,
        Status:             metav1.ConditionFalse,
        Reason:             "UpgradeCompleted",
        Message:            "No pending updates",
        ObservedGeneration: app.Generation,
    })

    return r.Status().Update(ctx, app)
}

// SetupWithManager configures the controller
func (r *UpgradeRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&fluxupv1alpha1.UpgradeRequest{}).
        Named("upgraderequest").
        Complete(r)
}
```

---

## Step 7: Configuration

### 7.1 Git Credentials Secret

```yaml
# config/samples/git-credentials.yaml
apiVersion: v1
kind: Secret
metadata:
  name: fluxup-git-credentials
  namespace: fluxup-system
type: Opaque
stringData:
  token: "${GITEA_TOKEN}"
  # For generic backend:
  # username: "${GIT_USERNAME}"
  # password: "${GIT_PASSWORD}"
```

### 7.2 Controller Configuration

```go
// cmd/main.go additions

// Git configuration from environment/flags
gitConfig := git.Config{
    Backend: os.Getenv("GIT_BACKEND"),  // "gitea", "github", etc.
    RepoURL: os.Getenv("GIT_REPO_URL"),
    Branch:  os.Getenv("GIT_BRANCH"),
    Token:   os.Getenv("GIT_TOKEN"),
}

gitManager, err := git.NewManager(gitConfig)
if err != nil {
    setupLog.Error(err, "Failed to create git manager")
    os.Exit(1)
}

// Initialize controllers with dependencies
if err = (&controller.UpgradeRequestReconciler{
    Client:          mgr.GetClient(),
    Scheme:          mgr.GetScheme(),
    GitManager:      gitManager,
    SnapshotManager: snapshot.NewManager(mgr.GetClient()),
    FluxHelper:      flux.NewHelper(mgr.GetClient()),
    YAMLEditor:      yaml.NewEditor(),
}).SetupWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create controller", "controller", "UpgradeRequest")
    os.Exit(1)
}
```

### 7.3 Manager Deployment Updates

```yaml
# config/manager/manager.yaml (additions)
env:
  - name: GIT_BACKEND
    value: "gitea"
  - name: GIT_REPO_URL
    value: "https://gitea.example.com/org/repo"
  - name: GIT_BRANCH
    value: "main"
  - name: GIT_TOKEN
    valueFrom:
      secretKeyRef:
        name: fluxup-git-credentials
        key: token
```

---

## Step 8: RBAC Updates

The UpgradeRequest controller needs additional permissions:

```go
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
```

After adding markers, run:
```bash
make manifests
```

---

## Step 9: Testing Strategy

### 9.1 Unit Tests

```
internal/git/gitea_test.go         # Mock Gitea API tests
internal/yaml/editor_test.go       # YAML editing tests
internal/snapshot/manager_test.go  # Snapshot creation tests (mock)
internal/flux/suspend_test.go      # Suspend/resume tests (mock)
```

### 9.2 Integration Tests

```go
// internal/controller/upgraderequest_controller_test.go

var _ = Describe("UpgradeRequest Controller", func() {
    Context("When processing an UpgradeRequest", func() {
        It("Should complete the upgrade workflow", func() {
            // 1. Create a ManagedApp with available update
            app := &fluxupv1alpha1.ManagedApp{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      "test-app",
                    Namespace: "default",
                },
                Spec: fluxupv1alpha1.ManagedAppSpec{
                    GitPath: "apps/test-app/helmrelease.yaml",
                    KustomizationRef: fluxupv1alpha1.ObjectReference{
                        Name:      "test-ks",
                        Namespace: "flux-system",
                    },
                },
            }
            app.Status.AvailableUpdate = &fluxupv1alpha1.VersionInfo{
                Chart: "2.0.0",
            }
            Expect(k8sClient.Create(ctx, app)).To(Succeed())
            Expect(k8sClient.Status().Update(ctx, app)).To(Succeed())

            // 2. Create Kustomization (mock)
            ks := &kustomizev1.Kustomization{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      "test-ks",
                    Namespace: "flux-system",
                },
            }
            Expect(k8sClient.Create(ctx, ks)).To(Succeed())

            // 3. Create UpgradeRequest
            upgrade := &fluxupv1alpha1.UpgradeRequest{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      "test-upgrade",
                    Namespace: "default",
                },
                Spec: fluxupv1alpha1.UpgradeRequestSpec{
                    ManagedAppRef: fluxupv1alpha1.ObjectReference{
                        Name: "test-app",
                    },
                    SkipSnapshot: true, // Skip for unit tests
                },
            }
            Expect(k8sClient.Create(ctx, upgrade)).To(Succeed())

            // 4. Verify upgrade completes successfully (Complete=True)
            Eventually(func() bool {
                var updated fluxupv1alpha1.UpgradeRequest
                k8sClient.Get(ctx, client.ObjectKeyFromObject(upgrade), &updated)
                return meta.IsStatusConditionTrue(updated.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
            }, timeout, interval).Should(BeTrue())
        })

        It("Should fail gracefully if ManagedApp not found", func() {
            upgrade := &fluxupv1alpha1.UpgradeRequest{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      "orphan-upgrade",
                    Namespace: "default",
                },
                Spec: fluxupv1alpha1.UpgradeRequestSpec{
                    ManagedAppRef: fluxupv1alpha1.ObjectReference{
                        Name: "nonexistent",
                    },
                },
            }
            Expect(k8sClient.Create(ctx, upgrade)).To(Succeed())

            // Verify upgrade fails (Complete=False with failure reason)
            Eventually(func() bool {
                var updated fluxupv1alpha1.UpgradeRequest
                k8sClient.Get(ctx, client.ObjectKeyFromObject(upgrade), &updated)
                cond := meta.FindStatusCondition(updated.Status.Conditions, fluxupv1alpha1.ConditionTypeComplete)
                return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == "ManagedAppNotFound"
            }, timeout, interval).Should(BeTrue())
        })
    })
})
```

### 9.3 E2E Tests

E2E tests require:
- Kind cluster with Flux installed
- Mock Gitea server (or use a real test instance)
- CSI driver with snapshot support (can use mock-csi)

---

## Step 10: Sample Resources

> **Implementation Note:** These samples show the original design. The actual implementation simplified `retentionPolicy` to only use `maxCount` (no `maxAge`). See actual samples in `config/samples/`.

### 10.1 ManagedApp with Snapshots

```yaml
# config/samples/managedapp_with_snapshots.yaml
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
    kind: HelmRelease
    name: my-app
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: csi-snapclass
    pvcs:
      - name: data-my-app-0
      - name: data-my-app-postgresql-0
    retentionPolicy:
      maxCount: 3  # maxAge removed in implementation
  healthCheck:
    timeout: "5m"
```

### 10.2 UpgradeRequest

```yaml
# config/samples/upgraderequest_basic.yaml
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: my-app-upgrade-20240115
  namespace: default
spec:
  managedAppRef:
    name: my-app
  # targetVersion: optional, uses availableUpdate if omitted
  # skipSnapshot: false
```

---

## Implementation Order

1. **UpgradeRequest CRD** - Scaffold and define types
2. **Git manager interface** - Define the abstraction
3. **Gitea implementation** - First backend
4. **YAML editor** - Version update logic
5. **Snapshot manager** - VolumeSnapshot creation
6. **Flux helpers** - Suspend/resume logic
7. **UpgradeRequest controller** - Main orchestration
8. **Configuration** - Secrets, deployment updates
9. **Tests** - Unit and integration tests

---

## Dependencies to Add

```go
// go.mod additions

require (
    // Gitea SDK
    code.gitea.io/sdk/gitea v0.19.0

    // VolumeSnapshot API
    github.com/kubernetes-csi/external-snapshotter/client/v8 v8.0.0

    // YAML editing
    sigs.k8s.io/kustomize/kyaml v0.18.1
)
```

---

## Resolved Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Progress tracking | Conditions only (no phase field) | Follows K8s API conventions; phase fields are discouraged |
| Git backend | Gitea first | User's current setup; interface allows future backends |
| YAML editing | kyaml | Battle-tested by Flux, preserves structure |
| Snapshot wait | Polling with timeout | Simple, reliable; could add informer later |
| Error handling | Conditional based on Git commit | Before commit: resume and fail; After commit: rollback required |
| Commit format | Conventional commits | Consistent, parseable history |

---

## Failure Handling

The Git commit is the **point of no return**. Failure handling differs based on when the failure occurs:

### Before Git Commit (Steps 1-4)

Failures during validation, suspend, snapshot creation, or snapshot wait:
- Resume the Kustomization (if suspended)
- Mark UpgradeRequest as `Complete=False`
- User can investigate and retry

Git is unchanged, so resuming Flux just reconciles the existing (old) version.

### After Git Commit (Steps 5-7)

Failures during resume, reconciliation, or health check:
- Do NOT resume the Kustomization
- Mark UpgradeRequest as `Complete=False` with message "rollback required"
- Phase 3 will add automatic RollbackRequest creation

The new version is in Git. Resuming would apply the (possibly broken) new version. A proper rollback (revert Git + restore snapshots) is needed.

```
┌─────────────────────────────────────────────────────────────────┐
│                     FAILURE HANDLING                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  Steps 1-4 (before Git commit)     Steps 5-7 (after Git commit)│
│  ┌─────────────────────────┐       ┌─────────────────────────┐  │
│  │ • Resume Kustomization  │       │ • Do NOT resume         │  │
│  │ • Mark Failed           │       │ • Mark Failed           │  │
│  │ • User can retry        │       │ • Rollback required     │  │
│  └─────────────────────────┘       └─────────────────────────┘  │
│                                                                 │
│              ─────── Git Commit (Point of No Return) ───────    │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## Implementation Status

Phase 2 is **complete**. All core functionality has been implemented:

| Component | Status | Notes |
|-----------|--------|-------|
| UpgradeRequest CRD | ✅ Complete | Full spec with targetVersion, skipSnapshot, dryRun |
| UpgradeRequest controller | ✅ Complete | Condition-driven state machine |
| Git manager interface | ✅ Complete | `git.Manager` interface with Gitea implementation |
| Gitea backend | ✅ Complete | Read, commit via Gitea API |
| VolumeSnapshot creation | ✅ Complete | Pre-upgrade snapshots with readiness waiting |
| Snapshot retention | ✅ Complete | Generational approach (maxCount per PVC) |
| Flux suspend/resume | ✅ Complete | Kustomization suspend before commit, resume after |
| YAML editor | ✅ Complete | kyaml-based, preserves comments |
| Chart version updates | ✅ Complete | Default path `spec.chart.spec.version` |
| Image tag updates | ✅ Complete | Custom `versionPath` required |
| Health check | ✅ Complete | Wait for workload Ready condition |
| Dry run mode | ✅ Complete | Validation only |
| Failure recovery | ✅ Complete | Auto-resume Kustomization on failure |
| Unit tests | ✅ Complete | Controller, git, snapshot, yaml tests |
| E2E tests | ⚠️ Scaffolded | Framework in place, tests need expansion |
| GitHub/GitLab backends | ❌ Deferred | Gitea only for now |

### Design Changes During Implementation

1. **Snapshot Retention**: Changed from time-based (`maxAge`) to generational (`maxCount`). Simpler, more predictable - prunes after each successful upgrade.

2. **Image Updates**: Reused `versionPath` field for both chart versions and image tags instead of separate config. Image updates require explicit `versionPath` (no default).

3. **GIT_TOKEN Secret Format**: Secret key must be uppercase (`TOKEN`) so that `kubectl set env --prefix=GIT_` creates `GIT_TOKEN`.

---

## Future Work

Items identified during Phase 2 that are deferred to future phases:

| Item | Current Approach | Future Options |
|------|------------------|----------------|
| **Multi-Image Updates** | One versionPath per ManagedApp | Add `ImagePaths map[string]string` for multiple images |
| **GitHub/GitLab Backends** | Gitea only | Implement `github.Manager`, `gitlab.Manager`, or generic git CLI |
| **E2E Test Coverage** | Unit tests with mocks | Full E2E with Kind, real Gitea, mock CSI |
| **Multi-file Atomic Commits** | Single-file manifests | Use git CLI for atomic multi-file updates |
| **Smarter Version Path Detection** | Manual `versionPath` config | Parse Renovate output to auto-detect paths |
| **Richer Dry Run** | Validation only | Create snapshot without commit, full simulation reports |

---

## Next Steps

Phase 2 delivered:
- UpgradeRequest CRD and controller
- Git manager with Gitea backend
- VolumeSnapshot creation with generational retention
- Flux Kustomization suspend/resume
- Chart and image version updates via configurable versionPath
- Health checks and automatic failure recovery

Phase 3 will add:
- RollbackRequest CRD and controller
- PVC restore from VolumeSnapshot
- Git revert functionality
- Workload scale down/up orchestration

See [Architecture](architecture.md#implementation-phases) for the full roadmap.
