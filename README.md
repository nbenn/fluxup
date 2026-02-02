# FluxUp - Upgrade Controller

A custom Kubernetes controller with web UI for managing application lifecycle in GitOps-based clusters. The controller leverages **Renovate** for version detection, commits updates to **Git** (source of truth), creates **CSI snapshots** before upgrades, and provides a UI for monitoring and rollback.

---

## Why FluxUp?

FluxUp fills a gap in the GitOps ecosystem by combining capabilities that currently require cobbling together multiple disconnected tools.

### Comparison with Existing Tools

| Capability | Renovate | Velero | Weave GitOps | FluxUp |
|:-----------|:--------:|:------:|:------------:|:------:|
| Detect available updates | ✅ | ❌ | ❌ | ✅ |
| Pre-upgrade snapshots | ❌ | ⚠️ Manual | ❌ | ✅ Auto |
| One-click upgrade from UI | ❌ | ❌ | ❌ | ✅ |
| Flux suspend/resume orchestration | ❌ | ❌ | ❌ | ✅ |
| Direct Git commit (no PR) | ❌ | ❌ | ❌ | ✅ |
| Snapshot-based rollback | ❌ | ⚠️ DR-focused | ❌ | ✅ |
| Unified workflow | ❌ | ❌ | ❌ | ✅ |

**Legend:** ✅ = Full support | ⚠️ = Partial/manual | ❌ = Not supported

### What Makes FluxUp Different

- **Renovate** creates PRs — FluxUp detects updates without PRs and commits directly
- **Velero** is disaster recovery focused — FluxUp ties snapshots to the upgrade workflow
- **Weave GitOps** provides visibility — FluxUp adds actionable upgrade management
- **Flux Image Automation** only handles container images — FluxUp handles Helm charts too

---

## Prerequisites

### Required Cluster Components

| Component | Purpose | Required? |
|-----------|---------|-----------|
| **Flux CD** | GitOps reconciliation - watches Git, applies changes | **Required** |
| **CSI Snapshot Controller** | VolumeSnapshot support for pre-upgrade snapshots | **Required** |
| **CSI Driver with Snapshot Support** | Storage backend (e.g., Ceph RBD, EBS, etc.) | **Required** |
| **Git Server** | Source of truth for manifests (Gitea, GitHub, GitLab) | **Required** |
| **OIDC Provider** | Web UI authentication (optional but recommended) | Optional |

### Supported Git Backends

Initially, we'll support **Gitea** via its API. The architecture allows adding more backends:

| Backend | Status | Notes |
|---------|--------|-------|
| **Gitea** | Phase 1 | Initial implementation via Gitea API |
| **GitHub** | Future | Via GitHub API or `gh` CLI |
| **GitLab** | Future | Via GitLab API |
| **Generic Git** | Future | Via git CLI (clone/commit/push) |

### Supported Storage Backends (for Snapshots)

Any CSI driver that supports VolumeSnapshots:
- Ceph RBD (via Rook)
- AWS EBS
- GCP Persistent Disk
- Azure Disk
- Longhorn
- OpenEBS

---

## State Management

### No External Database Required

All state is stored in Kubernetes objects (CRDs). This follows the Kubernetes-native pattern:

| State | Storage | Notes |
|-------|---------|-------|
| Available updates | `ManagedApp.status.availableUpdate` | Updated by Renovate parser |
| Upgrade history | `UpgradeRequest` objects | One per upgrade, retained for audit |
| Rollback history | `RollbackRequest` objects | One per rollback |
| Snapshots | `VolumeSnapshot` objects | Native K8s, managed by CSI |
| Configuration | `ManagedApp.spec` | Per-app settings |
| Global settings | `ConfigMap` or dedicated CRD | Controller-wide defaults |

### Benefits of CRD-based State

1. **No external dependencies** - No PostgreSQL, Redis, etc. to manage
2. **GitOps-friendly** - ManagedApp specs can be stored in Git
3. **Native tooling** - `kubectl get managedapps`, standard RBAC
4. **Built-in persistence** - etcd handles storage, HA, backup
5. **Event-driven** - Controller watches for changes, no polling DB

### Limitations

- **No complex queries** - Can't do SQL-like queries across objects
- **etcd size limits** - Don't store large blobs in status (we don't)
- **No transactions** - Use optimistic concurrency (resourceVersion)

---

## Goals

1. **Detect available updates** - Leverage Renovate's mature version detection for Helm charts and container images
2. **GitOps-native updates** - Commit version changes to Git (source of truth), let Flux reconcile
3. **Pre-upgrade snapshots** - Create CSI VolumeSnapshots before Flux applies updates
4. **Controlled upgrades** - Intercept Flux reconciliation to ensure snapshots complete first
5. **Notifications** - Alert on available updates and upgrade completion
6. **Rollback capability** - Quick rollback using snapshots if upgrade fails
7. **Web UI** - Dashboard to view pending updates, trigger upgrades, and manage rollbacks

## Non-Goals (Initial Version)

- Automatic upgrades without approval (Renovate PR workflow)
- Blue/green or canary deployment strategies
- Multi-cluster support
- Backup to external storage (Velero handles this separately)

---

## Architecture

```
┌───────────────────────────────────────────────────────────────────────┐
│                                                                       │
│  ┌─────────────┐    ┌───────────────────────────────────────────────┐│
│  │  Renovate   │    │           Upgrade Controller                  ││
│  │  (CronJob)  │    │  ┌─────────────────┐  ┌────────────────────┐  ││
│  │             │    │  │ Upgrade Manager │  │   Web UI Server    │  ││
│  │ - dryRun=   │───>│  │                 │  │                    │  ││
│  │   lookup    │    │  │ - Parse updates │  │ - List pending     │  ││
│  │ - JSON logs │    │  │ - Create snap   │  │ - Trigger upgrade  │  ││
│  │ - Scheduled │    │  │ - Commit to Git │  │ - View status      │  ││
│  └─────────────┘    │  │ - Monitor Flux  │  │ - Rollback         │  ││
│                     │  └────────┬────────┘  └──────────┬─────────┘  ││
│                     │           │                      │            ││
│                     │           v                      v            ││
│                     │  ┌────────────────────────────────────────┐   ││
│                     │  │          Reconciliation Loop           │   ││
│                     │  │  Watches: ManagedApp, HelmRelease,     │   ││
│                     │  │           VolumeSnapshot, Git commits  │   ││
│                     │  └────────────────────────────────────────┘   ││
│                     └───────────────────────────────────────────────┘│
└───────────────────────────────────────────────────────────────────────┘
        │                              │
        │                              v
        │              ┌───────────────────────────────┐
        │              │        Kubernetes API         │
        │              │  ┌───────────┐ ┌───────────┐  │
        │              │  │HelmRelease│ │VolSnapshot│  │
        │              │  └───────────┘ └───────────┘  │
        │              └───────────────────────────────┘
        │                              │
        v                              v
┌─────────────────┐          ┌─────────────────┐
│   Git Server    │<─────────│      Flux       │
│  (Gitea, etc.)  │  watches │                 │
│ - HelmReleases  │─────────>│ - Reconciles    │
│ - Deployments   │          │ - Applies       │
└─────────────────┘          └─────────────────┘
```

## Key Design Decision: Renovate Integration

Instead of reimplementing version detection, we leverage [Renovate](https://docs.renovatebot.com/):

### Why Renovate?

1. **Mature & Battle-tested** - Supports 90+ package managers, handles edge cases
2. **Flux-native support** - Has a dedicated [Flux manager](https://docs.renovatebot.com/modules/manager/flux/) that understands HelmRelease ↔ HelmRepository relationships
3. **Registry authentication** - Already handles private registries, OCI, etc.
4. **Semver handling** - Proper version constraint parsing and comparison
5. **No wheel reinvention** - Version detection is complex; Renovate solved it

### How We Use Renovate (Without PRs)

Renovate supports a [`dryRun=lookup`](https://docs.renovatebot.com/self-hosted-configuration/#dryrun) mode that:
- Scans the repository for dependencies
- Checks registries for available updates
- Outputs results to logs (JSON format available)
- **Does NOT create PRs or branches**

We run Renovate as a CronJob with `dryRun=lookup` and `LOG_FORMAT=json`, then parse the output to populate our `ManagedApp` status with available updates.

When user approves an upgrade in our UI:
1. Controller creates VolumeSnapshots
2. Controller commits the version bump directly to Git (no PR)
3. Flux detects the change and reconciles
4. Controller monitors the rollout

---

## Custom Resource Definitions

### ManagedApp CRD

The primary CRD that defines an application to be managed by the upgrade controller.

```yaml
apiVersion: upgrades.example.com/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
  namespace: my-app
spec:
  # Reference to the HelmRelease to manage
  helmReleaseRef:
    name: my-app
    namespace: my-app

  # PVCs to snapshot before upgrade (auto-detected if empty)
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: csi-snapclass
    # Explicit PVC list (optional - auto-detect from HelmRelease if empty)
    pvcs: []
    # How long to keep pre-upgrade snapshots
    retentionPolicy:
      keepLast: 3
      maxAge: 168h  # 7 days

  # Version policy
  versionPolicy:
    # Helm chart version constraints (semver)
    chart:
      # "none" = manual only, "patch" = 1.2.x, "minor" = 1.x.x, "major" = any
      autoUpdate: patch
      # Explicit version constraint (optional)
      constraint: ">=12.0.0 <13.0.0"

    # Container image version constraints
    images:
      - name: example/app
        autoUpdate: patch
        constraint: ">=1.21.0"

  # Notification settings
  notifications:
    # Notify when new version is available
    onAvailable: true
    # Notify after upgrade completes
    onUpgrade: true
    # Notify on upgrade failure
    onFailure: true

  # Health check after upgrade
  healthCheck:
    # Wait for HelmRelease Ready condition
    waitForReady: true
    # Additional health check timeout
    timeout: 5m
    # Custom health check (optional)
    # httpGet:
    #   path: /api/healthz
    #   port: 3000

status:
  # Current versions
  currentVersion:
    chart: "12.4.0"
    appVersion: "1.21.5"
    images:
      - name: example/app
        tag: "1.21.5"

  # Available update (if any)
  availableUpdate:
    chart: "12.5.0"
    appVersion: "1.22.0"
    images:
      - name: example/app
        tag: "1.22.0"
    detectedAt: "2024-01-15T10:30:00Z"

  # Upgrade history
  lastUpgrade:
    fromVersion: "12.3.0"
    toVersion: "12.4.0"
    startedAt: "2024-01-10T14:00:00Z"
    completedAt: "2024-01-10T14:05:00Z"
    status: Succeeded
    snapshotName: "my-app-pre-upgrade-20240110-140000"

  # Current state
  conditions:
    - type: Ready
      status: "True"
      reason: Healthy
      message: "Application running, no pending updates"
    - type: UpdateAvailable
      status: "True"
      reason: NewVersionDetected
      message: "Chart 12.5.0 available (current: 12.4.0)"
```

### UpgradeRequest CRD

Represents an upgrade operation (created by user via UI or kubectl).

```yaml
apiVersion: upgrades.example.com/v1alpha1
kind: UpgradeRequest
metadata:
  name: my-app-upgrade-20240115
  namespace: my-app
spec:
  # Reference to ManagedApp
  managedAppRef:
    name: my-app

  # Target version (optional - uses latest available if empty)
  targetVersion:
    chart: "12.5.0"

  # Skip snapshot (not recommended)
  skipSnapshot: false

  # Dry run - just validate, don't apply
  dryRun: false

status:
  phase: Completed  # Pending, Snapshotting, Upgrading, HealthChecking, Completed, Failed, RolledBack

  # Snapshot created for this upgrade
  snapshot:
    name: "my-app-pre-upgrade-20240115-143000"
    pvcs:
      - name: data-my-app-0
        snapshotName: "my-app-data-snap-20240115-143000"
      - name: data-my-app-postgresql-0
        snapshotName: "my-app-postgresql-snap-20240115-143000"
    createdAt: "2024-01-15T14:30:00Z"
    readyAt: "2024-01-15T14:30:05Z"

  # Upgrade details
  upgrade:
    startedAt: "2024-01-15T14:30:10Z"
    completedAt: "2024-01-15T14:35:00Z"
    previousVersion: "12.4.0"
    newVersion: "12.5.0"
    gitCommit: "abc123def456"

  # Health check results
  healthCheck:
    status: Passed
    checkedAt: "2024-01-15T14:35:30Z"

  conditions:
    - type: Complete
      status: "True"
      reason: UpgradeSucceeded
      message: "Upgrade to 12.5.0 completed successfully"
```

### RollbackRequest CRD

Represents a rollback operation.

```yaml
apiVersion: upgrades.example.com/v1alpha1
kind: RollbackRequest
metadata:
  name: my-app-rollback-20240115
  namespace: my-app
spec:
  # Reference to the UpgradeRequest to rollback
  upgradeRequestRef:
    name: my-app-upgrade-20240115

  # Or reference a specific snapshot
  # snapshotRef:
  #   name: my-app-pre-upgrade-20240115-143000

status:
  phase: Completed  # Pending, StoppingApp, RestoringVolumes, StartingApp, Completed, Failed

  # Restored from
  restoredFrom:
    snapshotName: "my-app-pre-upgrade-20240115-143000"
    chartVersion: "12.4.0"

  startedAt: "2024-01-15T15:00:00Z"
  completedAt: "2024-01-15T15:05:00Z"
```

---

## Controller Components

### 1. Renovate Integration (Version Detection)

Instead of implementing version detection ourselves, we run Renovate in `dryRun=lookup` mode.

**Renovate Configuration (`renovate.json`):**
```json
{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "extends": ["config:recommended"],
  "platform": "gitea",
  "endpoint": "https://<GITEA_HOST>/api/v1",
  "repositories": ["<ORG>/<REPO>"],
  "dryRun": "lookup",

  "flux": {
    "fileMatch": ["flux/.+\\.ya?ml$"]
  },

  "helm-values": {
    "fileMatch": ["flux/.+\\.ya?ml$"]
  },

  "kubernetes": {
    "fileMatch": ["flux/apps/.+\\.ya?ml$"]
  },

  "packageRules": [
    {
      "matchDatasources": ["docker"],
      "matchPackagePatterns": ["^linuxserver/"],
      "pinDigests": true
    }
  ]
}
```

**What Renovate Detects:**

| Manager | Source | Example |
|---------|--------|---------|
| `flux` | HelmRelease `spec.chart.spec.version` | `my-app: 12.4.0 → 12.5.0` |
| `helm-values` | Image tags in HelmRelease values | `image.tag: 1.21.0 → 1.22.0` |
| `kubernetes` | Raw Deployment `image:` fields | `example/app:1.35.2 → 1.36.0` |
| `git-refs` | GitRepository refs | `ref.branch: main` or `ref.tag: v1.0.0` |

**Running Renovate:**
```bash
# As a CronJob or triggered by controller
LOG_FORMAT=json renovate --token=$GIT_TOKEN 2>&1 | tee /data/renovate-output.json
```

**Parsing Renovate Output:**

Renovate outputs JSON lines with update information at DEBUG level:
```json
{
  "name": "renovate",
  "level": 20,
  "msg": "packageFiles with updates",
  "config": {
    "flux": {
      "my-app/helmrelease.yaml": {
        "deps": [{
          "depName": "my-app",
          "currentVersion": "12.4.0",
          "newVersion": "12.5.0",
          "datasource": "helm",
          "registryUrl": "https://charts.example.com/"
        }]
      }
    }
  }
}
```

**Implementation:**
```go
type RenovateParser struct {
    outputPath string
}

func (p *RenovateParser) ParseUpdates() ([]UpdateInfo, error) {
    // 1. Read JSON lines from Renovate output
    // 2. Filter for "packageFiles with updates" messages
    // 3. Extract dependency updates
    // 4. Map to our UpdateInfo struct
}

type RenovateRunner struct {
    gitToken   string
    configPath string
    outputPath string
}

func (r *RenovateRunner) Run(ctx context.Context) error {
    // 1. Execute Renovate with dryRun=lookup
    // 2. Capture JSON output
    // 3. Store for parsing
}
```

**Why This Approach:**
- Renovate handles all the complexity of registry authentication, semver parsing, etc.
- The [Flux manager](https://docs.renovatebot.com/modules/manager/flux/) already understands HelmRelease ↔ HelmRepository linking
- We just parse the output and present it in our UI

### 2. YAML Editing Strategy

Editing YAML programmatically while preserving comments, formatting, and structure is non-trivial. There are three main approaches:

#### Option A: kyaml (Kustomize's YAML library) - Recommended

[kyaml](https://pkg.go.dev/sigs.k8s.io/kustomize/kyaml/yaml) is what Flux's [image-automation-controller](https://github.com/fluxcd/image-automation-controller) uses. It's designed specifically for Kubernetes resources and preserves YAML structure and comments.

```go
import "sigs.k8s.io/kustomize/kyaml/yaml"

// Update HelmRelease chart version
func UpdateChartVersion(filePath string, newVersion string) error {
    data, _ := os.ReadFile(filePath)
    node, _ := yaml.Parse(string(data))

    // Navigate to spec.chart.spec.version and update
    _, err := node.Pipe(
        yaml.Lookup("spec", "chart", "spec"),
        yaml.SetField("version", yaml.NewScalarRNode(newVersion)),
    )

    output, _ := node.String()
    return os.WriteFile(filePath, []byte(output), 0644)
}

// Update container image in Deployment/StatefulSet
func UpdateContainerImage(filePath string, containerName string, newImage string) error {
    data, _ := os.ReadFile(filePath)
    node, _ := yaml.Parse(string(data))

    // Find the container and update its image
    containers, _ := node.Pipe(yaml.Lookup("spec", "template", "spec", "containers"))
    _ = containers.VisitElements(func(container *yaml.RNode) error {
        name, _ := container.Pipe(yaml.Lookup("name"))
        if name.YNode().Value == containerName {
            return container.PipeE(yaml.SetField("image", yaml.NewScalarRNode(newImage)))
        }
        return nil
    })

    output, _ := node.String()
    return os.WriteFile(filePath, []byte(output), 0644)
}
```

**Update Types We Handle:**

| Type | YAML Path | Example |
|------|-----------|---------|
| Helm chart version | `spec.chart.spec.version` | `12.4.0 → 12.5.0` |
| Container image (Deployment) | `spec.template.spec.containers[].image` | `example/app:1.35.2 → 1.36.0` |
| Container image (HelmRelease values) | `spec.values.image.tag` | Varies by chart |
| Init container image | `spec.template.spec.initContainers[].image` | Plugin images |

**Pros:**
- Battle-tested by Kustomize and Flux
- Preserves comments and formatting
- Understands Kubernetes resource structure
- Go-native (no external process)

**Cons:**
- Has known bugs with non-ASCII characters, anchors/aliases
- Some edge cases with complex YAML

#### Option B: Regex-based replacement (Renovate's approach)

Renovate uses regex to find and replace version strings without parsing YAML at all.

```go
func UpdateVersionRegex(content string, depName string, oldVer string, newVer string) string {
    // Pattern matches: version: "12.4.0" (with optional quotes)
    pattern := regexp.MustCompile(`(version:\s*["']?)` + regexp.QuoteMeta(oldVer) + `(["']?)`)
    return pattern.ReplaceAllString(content, "${1}" + newVer + "${2}")
}
```

**Pros:**
- Zero formatting changes
- Simple and predictable
- No YAML parsing edge cases

**Cons:**
- Fragile if version appears multiple times
- No structural understanding
- Need precise patterns per file type

#### Option C: yq CLI (external tool)

[yq](https://github.com/mikefarah/yq) is a YAML processor that attempts to preserve comments.

```go
func UpdateWithYq(filePath string, path string, value string) error {
    cmd := exec.Command("yq", "-i",
        fmt.Sprintf("%s = \"%s\"", path, value),
        filePath)
    return cmd.Run()
}
```

**Pros:**
- Well-maintained, popular tool
- Handles most cases well

**Cons:**
- External dependency
- Some formatting changes possible
- Slower (process spawn)

#### Our Recommendation: kyaml

Since Flux already uses kyaml successfully for their image automation, and we're operating in the same Kubernetes/Flux ecosystem, using kyaml makes sense. We accept the known limitations (rare edge cases with anchors) in exchange for Go-native operation and battle-tested code.

---

### 3. Git Manager

Handles committing version changes to Git (the source of truth).

```go
type GitManager interface {
    CommitVersionBump(ctx context.Context, update *UpdateInfo, snapshotRefs []string) (commitSHA string, err error)
    RevertCommit(ctx context.Context, commitSHA string) error
}

// GiteaGitManager implements GitManager using Gitea API
type GiteaGitManager struct {
    client      *gitea.Client
    repoOwner   string
    repoName    string
    branch      string
    authorName  string
    authorEmail string
}

func (g *GiteaGitManager) CommitVersionBump(ctx context.Context, update *UpdateInfo, snapshotRefs []string) (string, error) {
    // 1. Get current file content via Gitea API
    // 2. Update version using kyaml
    // 3. Commit via Gitea API (creates commit directly, no clone needed)
    // 4. Return commit SHA
}

func (g *GiteaGitManager) RevertCommit(ctx context.Context, commitSHA string) error {
    // 1. Get the commit's changes
    // 2. Create inverse patch
    // 3. Commit the revert via API
}
```

**Commit Message Format:**
```
chore(deps): update my-app to 12.5.0

Upgrade triggered via upgrade-controller UI
Pre-upgrade snapshots:
- my-app-data-snap-20240115-143000
- my-app-postgresql-snap-20240115-143000

Managed-By: upgrade-controller
Previous-Version: 12.4.0
```

### 4. Upgrade Manager

Orchestrates the upgrade process:

```go
type UpgradeManager struct {
    client      client.Client
    gitManager  GitManager
    snapManager *SnapshotManager
    recorder    record.EventRecorder
    notifier    Notifier
}

func (m *UpgradeManager) ExecuteUpgrade(ctx context.Context, req *v1alpha1.UpgradeRequest) error {
    // 1. Validate upgrade request
    // 2. Suspend the Flux Kustomization (prevent premature reconciliation)
    // 3. Create VolumeSnapshots for all PVCs
    // 4. Wait for snapshots to be ready (ReadyToUse=true)
    // 5. Commit version change to Git via GitManager
    // 6. Resume the Flux Kustomization
    // 7. Wait for HelmRelease reconciliation (Flux picks up Git change)
    // 8. Run health checks
    // 9. Update status and send notifications
}

func (m *UpgradeManager) ExecuteRollback(ctx context.Context, req *v1alpha1.RollbackRequest) error {
    // 1. Suspend the Flux Kustomization
    // 2. Scale down workload (Deployment/StatefulSet)
    // 3. Delete existing PVCs
    // 4. Restore PVCs from VolumeSnapshots
    // 5. Revert Git commit (or commit previous version)
    // 6. Resume the Flux Kustomization
    // 7. Wait for reconciliation
    // 8. Verify health
}
```

### 5. Snapshot Manager

Handles VolumeSnapshot lifecycle:

```go
type SnapshotManager struct {
    client                 client.Client
    volumeSnapshotClass    string
    defaultRetentionPolicy RetentionPolicy
}

func (s *SnapshotManager) CreatePreUpgradeSnapshots(ctx context.Context, app *v1alpha1.ManagedApp) ([]SnapshotInfo, error) {
    // 1. Discover PVCs from HelmRelease (if not explicit)
    // 2. Create VolumeSnapshot for each PVC
    // 3. Wait for ReadyToUse=true
    // 4. Return snapshot references
}

func (s *SnapshotManager) CleanupOldSnapshots(ctx context.Context, app *v1alpha1.ManagedApp) error {
    // Apply retention policy (keepLast, maxAge)
}

func (s *SnapshotManager) RestoreFromSnapshot(ctx context.Context, snapshot *SnapshotInfo) (*corev1.PersistentVolumeClaim, error) {
    // Create new PVC from VolumeSnapshot dataSource
}
```

### 6. PVC Discovery

Auto-detect PVCs associated with a HelmRelease:

```go
func (d *PVCDiscoverer) DiscoverPVCs(ctx context.Context, hr *helmv2.HelmRelease) ([]corev1.PersistentVolumeClaim, error) {
    // Strategy 1: Label selector (app.kubernetes.io/instance = helmrelease-name)
    // Strategy 2: Owner references (if HelmRelease creates PVCs directly)
    // Strategy 3: Namespace scan with heuristics (StatefulSet volumeClaimTemplates)
    //
    // Filter to only include PVCs with supported snapshot StorageClass
}
```

---

## Web UI

### Technology Stack

- **Backend**: Embedded Go HTTP server (same binary as controller)
- **Frontend**: Lightweight - either:
  - Server-rendered HTML with htmx for interactivity
  - Or: Simple React/Vue SPA served as static assets
- **Authentication**: OIDC (configurable provider)

### UI Pages

#### Dashboard
- Overview of all ManagedApps
- Count of pending updates
- Recent upgrade activity
- Quick actions

#### Applications List
```
┌──────────────────────────────────────────────────────────────┐
│ Managed Applications                                [+ Add]  │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ ● My App                          [Update Available]   │  │
│  │ Namespace: my-app                                      │  │
│  │ Current: 12.4.0 → Available: 12.5.0                    │  │
│  │ Last upgrade: 5 days ago (succeeded)                   │  │
│  │                              [View] [Upgrade] [Skip]   │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ ● Another App                           [Up to Date]   │  │
│  │ Namespace: another-app                                 │  │
│  │ Current: 2025.10.3                                     │  │
│  │ Last upgrade: 2 weeks ago (succeeded)                  │  │
│  │                                                [View]  │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

#### Application Detail
- Current version info
- Available update details (changelog link if available)
- PVCs and their sizes
- Snapshot history
- Upgrade history with status
- Configuration (version policy, notifications)

#### Upgrade Confirmation
- Pre-flight checks
- PVCs to be snapshotted
- Version diff
- Confirm/Cancel buttons

#### Upgrade Progress
- Real-time status updates
- Snapshot creation progress
- HelmRelease reconciliation status
- Health check results
- Rollback button (if failed)

#### Rollback Page
- Select snapshot to restore
- Warning about data loss
- Confirm rollback

#### Settings
- Global notification settings
- Default version policies
- Snapshot retention defaults
- Git backend configuration

### API Endpoints

```
GET  /api/apps                    # List all ManagedApps
GET  /api/apps/:namespace/:name   # Get ManagedApp details
POST /api/apps                    # Create ManagedApp

GET  /api/apps/:ns/:name/updates  # Check for updates
POST /api/apps/:ns/:name/upgrade  # Trigger upgrade
POST /api/apps/:ns/:name/rollback # Trigger rollback

GET  /api/upgrades                # List UpgradeRequests
GET  /api/upgrades/:ns/:name      # Get UpgradeRequest status

GET  /api/snapshots/:ns           # List snapshots in namespace
DELETE /api/snapshots/:ns/:name   # Delete snapshot

GET  /api/notifications           # List recent notifications
PUT  /api/settings                # Update global settings

# WebSocket for real-time updates
WS   /api/ws                      # Real-time status updates
```

---

## Notifications

### Supported Channels

- **Slack/Discord** via webhook
- **Email** via SMTP
- **Gotify/ntfy** for push notifications
- **Flux Notification Controller** integration (send events that Flux can forward)

### Notification Events

1. **Update Available**
   - App name, current version, new version
   - Link to UI for upgrade

2. **Upgrade Started**
   - App name, target version
   - Triggered by (user or automation)

3. **Upgrade Completed**
   - App name, old → new version
   - Duration
   - Link to verify

4. **Upgrade Failed**
   - App name, target version
   - Error message
   - Link to rollback

5. **Rollback Completed**
   - App name, restored version
   - Snapshot used

---

## Upgrade Flow Sequence

```
User clicks "Upgrade" in UI
              │
              ▼
┌───────────────────────────┐
│ Create UpgradeRequest     │
│ (status: Pending)         │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Suspend Flux              │◀── Prevent premature reconciliation
│ Kustomization             │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Discover PVCs             │
│ (from HelmRelease)        │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Create VolumeSnapshots    │◀── status: Snapshotting
│ for each PVC              │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Wait for snapshots        │
│ ReadyToUse = true         │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Commit version bump       │◀── status: Committing
│ to Git (source of truth)  │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Resume Flux               │◀── status: Upgrading
│ Kustomization             │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Wait for Flux to detect   │
│ Git change & reconcile    │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Wait for HelmRelease      │
│ Ready condition           │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Health checks             │◀── status: HealthChecking
│ (optional custom)         │
└─────────────┬─────────────┘
              │
       ┌──────┴──────┐
       │             │
    Success       Failure
       │             │
       ▼             ▼
┌───────────┐ ┌─────────────┐
│ Completed │ │ Failed      │
│           │ │ (notify,    │
│ Notify    │ │  await      │
│ Cleanup   │ │  rollback)  │
└───────────┘ └─────────────┘
```

### Why Suspend/Resume Flux?

The suspend/resume pattern ensures:
1. Snapshots complete **before** the upgrade starts
2. Git commit happens after snapshots are ready
3. Flux doesn't race ahead while we're preparing

Without suspension, Flux might detect the Git change and start reconciling before snapshots are ready, defeating the purpose of pre-upgrade snapshots.

---

## Rollback Flow

```
User clicks "Rollback" (or auto-triggered on failure)
              │
              ▼
┌───────────────────────────┐
│ Create RollbackRequest    │
│ (status: Pending)         │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Suspend Flux              │◀── Prevent Flux from reconciling
│ Kustomization             │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Scale down workload       │◀── status: StoppingApp
│ (Deployment/StatefulSet)  │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Delete current PVCs       │◀── status: RestoringVolumes
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Create new PVCs from      │
│ VolumeSnapshot source     │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Revert Git commit         │◀── status: Reverting
│ (commit previous ver)     │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Resume Flux               │◀── status: StartingApp
│ Kustomization             │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Wait for Flux to detect   │
│ revert & reconcile        │
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│ Wait for Ready            │◀── status: Completed
│ Notify completion         │
└───────────────────────────┘
```

### Git Revert Strategy

Two options for reverting in Git:

**Option A: Revert Commit (Preferred)**
```bash
git revert <upgrade-commit-sha> --no-edit
git push
```
- Preserves history
- Clear audit trail of what happened
- Commit message: "Revert 'chore(deps): update my-app to 12.5.0'"

**Option B: Direct Version Commit**
```bash
# Edit HelmRelease to previous version
git commit -m "chore(deps): rollback my-app to 12.4.0 (from snapshot)"
git push
```
- Simpler if revert has conflicts
- Still maintains audit trail

---

## Renovate Deployment

Renovate runs as a CronJob that periodically checks for updates.

### CronJob Manifest

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: renovate-scanner
  namespace: upgrade-controller
spec:
  schedule: "0 * * * *"  # Hourly
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers:
            - name: renovate
              image: renovate/renovate:latest
              env:
                - name: LOG_FORMAT
                  value: "json"
                - name: LOG_LEVEL
                  value: "debug"
                - name: RENOVATE_TOKEN
                  valueFrom:
                    secretKeyRef:
                      name: renovate-credentials
                      key: git-token
              args:
                - --platform=gitea
                - --endpoint=https://<GITEA_HOST>/api/v1
                - --dry-run=lookup
                - <ORG>/<REPO>
              volumeMounts:
                - name: output
                  mountPath: /tmp/renovate
                - name: config
                  mountPath: /usr/src/app/config.js
                  subPath: config.js
          volumes:
            - name: output
              persistentVolumeClaim:
                claimName: renovate-output
            - name: config
              configMap:
                name: renovate-config
```

### Renovate Config (ConfigMap)

```javascript
// config.js
module.exports = {
  platform: "gitea",
  endpoint: "https://<GITEA_HOST>/api/v1",
  repositories: ["<ORG>/<REPO>"],
  dryRun: "lookup",
  onboarding: false,
  requireConfig: "ignored",
  flux: {
    fileMatch: ["flux/apps/.+\\.ya?ml$", "flux/infrastructure/.+\\.ya?ml$"]
  },
  "helm-values": {
    fileMatch: ["flux/apps/.+\\.ya?ml$"]
  },
  // Write structured output for our controller to parse
  logFileLevel: "debug"
};
```

### Output Parsing

The controller watches the PVC or reads from a shared volume where Renovate writes its JSON output. Key log entries to parse:

```json
{"level":20,"msg":"packageFiles with updates","config":{"flux":{...}}}
{"level":30,"msg":"DRY-RUN: Would create PR for ..."}
```

---

## Project Structure

```
upgrade-controller/
├── api/
│   └── v1alpha1/
│       ├── managedapp_types.go
│       ├── upgraderequest_types.go
│       ├── rollbackrequest_types.go
│       ├── groupversion_info.go
│       └── zz_generated.deepcopy.go
├── cmd/
│   └── controller/
│       └── main.go
├── internal/
│   ├── controller/
│   │   ├── managedapp_controller.go
│   │   ├── upgraderequest_controller.go
│   │   └── rollbackrequest_controller.go
│   ├── renovate/
│   │   ├── runner.go         # Execute Renovate CLI
│   │   ├── parser.go         # Parse JSON output
│   │   └── config.go         # Generate renovate.json
│   ├── git/
│   │   ├── interface.go      # GitManager interface
│   │   ├── gitea.go          # Gitea implementation
│   │   ├── github.go         # GitHub implementation (future)
│   │   └── generic.go        # Git CLI implementation (future)
│   ├── snapshot/
│   │   ├── manager.go
│   │   └── discovery.go
│   ├── upgrade/
│   │   ├── manager.go
│   │   └── rollback.go
│   ├── flux/
│   │   ├── suspend.go        # Suspend/resume Kustomizations
│   │   └── watch.go          # Watch HelmRelease status
│   ├── notify/
│   │   ├── notifier.go
│   │   ├── slack.go
│   │   ├── webhook.go
│   │   └── flux.go
│   └── webui/
│       ├── server.go
│       ├── handlers.go
│       ├── middleware.go
│       └── static/
│           └── (embedded frontend assets)
├── config/
│   ├── crd/
│   │   └── bases/
│   │       ├── upgrades.example.com_managedapps.yaml
│   │       ├── upgrades.example.com_upgraderequests.yaml
│   │       └── upgrades.example.com_rollbackrequests.yaml
│   ├── rbac/
│   │   ├── role.yaml
│   │   └── role_binding.yaml
│   ├── manager/
│   │   └── manager.yaml
│   ├── renovate/
│   │   └── renovate.json     # Base Renovate config
│   └── samples/
│       └── managedapp_example.yaml
├── web/
│   └── (frontend source if using SPA)
├── Dockerfile                 # Multi-stage: Go + Node (for Renovate)
├── Makefile
├── go.mod
└── go.sum
```

---

## RBAC Requirements

The controller needs permissions to:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: upgrade-controller
rules:
  # ManagedApp, UpgradeRequest, RollbackRequest (own CRDs)
  - apiGroups: ["upgrades.example.com"]
    resources: ["*"]
    verbs: ["*"]

  # HelmRelease (read and suspend/resume)
  - apiGroups: ["helm.toolkit.fluxcd.io"]
    resources: ["helmreleases"]
    verbs: ["get", "list", "watch", "patch", "update"]

  # Kustomization (suspend/resume)
  - apiGroups: ["kustomize.toolkit.fluxcd.io"]
    resources: ["kustomizations"]
    verbs: ["get", "list", "watch", "patch", "update"]

  # HelmRepository (read chart index)
  - apiGroups: ["source.toolkit.fluxcd.io"]
    resources: ["helmrepositories"]
    verbs: ["get", "list", "watch"]

  # VolumeSnapshots (create, delete, list)
  - apiGroups: ["snapshot.storage.k8s.io"]
    resources: ["volumesnapshots"]
    verbs: ["get", "list", "watch", "create", "delete"]

  # PVCs (list, create from snapshot)
  - apiGroups: [""]
    resources: ["persistentvolumeclaims"]
    verbs: ["get", "list", "watch", "create", "delete"]

  # Deployments/StatefulSets (scale for rollback)
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets"]
    verbs: ["get", "list", "watch", "patch", "update"]

  # Pods (list for health checks)
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]

  # Events (create for audit)
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]

  # Secrets (read notification credentials)
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
```

---

## Deployment

Deploy via Flux:

```yaml
# flux/apps/upgrade-controller/helmrelease.yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: upgrade-controller
  namespace: upgrade-controller
spec:
  interval: 30m
  chart:
    spec:
      chart: upgrade-controller
      sourceRef:
        kind: HelmRepository
        name: upgrade-controller
        namespace: flux-system
  values:
    replicaCount: 1

    image:
      repository: ghcr.io/example/upgrade-controller
      tag: latest

    ingress:
      enabled: true
      className: ""  # Use cluster default
      hosts:
        - host: upgrades.example.com
          paths:
            - path: /
              pathType: Prefix

    auth:
      oidc:
        enabled: true
        issuer: https://auth.example.com/
        clientId: upgrade-controller
        # clientSecret from Secret

    git:
      backend: gitea  # gitea, github, gitlab, generic
      url: https://git.example.com
      # token from Secret

    notifications:
      slack:
        enabled: false
        # webhookUrl from Secret

    config:
      defaultSnapshotClass: csi-snapclass
      defaultRetention:
        keepLast: 3
        maxAge: 168h
```

---

## Implementation Phases

### Phase 1: Core Infrastructure
- [ ] Project scaffolding with kubebuilder
- [ ] ManagedApp CRD and basic controller
- [ ] Renovate CronJob setup with `dryRun=lookup`
- [ ] Renovate output parser (JSON → UpdateInfo)
- [ ] Update ManagedApp status with available updates

### Phase 2: Upgrade Workflow
- [ ] UpgradeRequest CRD and controller
- [ ] Git manager interface + Gitea implementation
- [ ] VolumeSnapshot creation before upgrade
- [ ] Flux suspend/resume integration
- [ ] Basic status reporting and events

### Phase 3: Web UI (MVP)
- [ ] Embedded HTTP server
- [ ] List ManagedApps with update status
- [ ] Trigger upgrade via UI
- [ ] Upgrade progress/status page
- [ ] OIDC authentication

### Phase 4: Rollback
- [ ] RollbackRequest CRD and controller
- [ ] PVC restore from VolumeSnapshot
- [ ] Git revert functionality
- [ ] Rollback UI page

### Phase 5: Notifications & Polish
- [ ] Notification system (Slack, webhook, ntfy)
- [ ] Changelog links (fetch from GitHub releases, etc.)
- [ ] Snapshot retention/cleanup automation
- [ ] Settings page in UI
- [ ] Improved error handling and recovery

### Phase 6: Advanced Features
- [ ] Additional Git backends (GitHub, GitLab, generic)
- [ ] Bulk upgrades (upgrade all with available updates)
- [ ] Dependency ordering (upgrade A before B)
- [ ] Version pinning/skipping in UI
- [ ] Metrics and Prometheus integration
- [ ] Grafana dashboard

---

## Alternatives Considered

### Version Detection

| Approach | Pros | Cons | Decision |
|:---------|:-----|:-----|:--------:|
| Custom implementation | Full control, no dependencies | Reinventing the wheel, many edge cases | ❌ |
| Flux Image Automation | Native Flux integration | Only for images, not Helm charts | ❌ |
| Renovate with PRs | Standard workflow, review process | Requires manual PR merge, slower | ❌ |
| **Renovate dryRun=lookup** | Mature detection, no PRs, JSON output | Extra component (Node.js) | ✅ |

### Git Updates

| Approach | Pros | Cons | Decision |
|:---------|:-----|:-----|:--------:|
| Direct K8s API update | Simple, fast | Git out of sync, not GitOps | ❌ |
| Renovate create PR | Standard, reviewable | Requires merge step, slower | ❌ |
| **Direct Git commit** | True GitOps, fast, atomic | Needs Git credentials | ✅ |

### Snapshot Timing

| Approach | Pros | Cons | Decision |
|:---------|:-----|:-----|:--------:|
| Rely on scheduled backups | Already exists | May not be recent enough | ❌ |
| Snapshot after Git commit | Simpler flow | Race with Flux, might miss | ❌ |
| **Snapshot before Git commit + suspend Flux** | Guaranteed pre-upgrade state | More complex | ✅ |

### YAML Editing

| Approach | Pros | Cons | Decision |
|:---------|:-----|:-----|:--------:|
| Unmarshal/Marshal (naive) | Simple | Loses comments, changes formatting | ❌ |
| Regex replacement | No formatting changes | Fragile, no structure awareness | ⚠️ |
| yq CLI | Good preservation | External dependency, slower | ❌ |
| **kyaml (Kustomize)** | Battle-tested by Flux, Go-native | Rare edge cases | ✅ |

### State Storage

| Approach | Pros | Cons | Decision |
|:---------|:-----|:-----|:--------:|
| PostgreSQL | Rich queries, transactions | Extra dependency, operational overhead | ❌ |
| SQLite | Simple, embedded | Not HA, backup complexity | ❌ |
| Redis | Fast, pub/sub | Volatile, another component | ❌ |
| **Kubernetes CRDs** | Native, GitOps-friendly, no extra deps | Limited queries | ✅ |

---

## Open Questions

1. ~~**Git integration**~~: ✅ Resolved - Use Renovate for detection, commit directly to Git for upgrades

2. **Renovate execution model**: Run as:
   - Sidecar container (always running, periodic checks)?
   - CronJob (scheduled, separate pod)?
   - On-demand (triggered by controller when user requests update check)?

   *Recommendation*: CronJob (e.g., hourly) that writes output to a PVC or ConfigMap that the controller reads.

3. ~~**Image tag detection**~~: ✅ Resolved - Renovate handles this well:
   - **Semver tags** (`v1.2.3`, `1.2.3`): Normal version comparison
   - **`latest` tags**: Renovate can pin to digest and track actual changes
   - **Rolling tags** (`develop`, `main`): Pin to digest for reproducibility

4. **Multi-instance apps**: How to handle apps with multiple HelmReleases (e.g., app + database)?
   - Option A: Group them in a single ManagedApp with multiple HelmRelease refs
   - Option B: Separate ManagedApps with dependency ordering

5. **Flux suspension granularity**: Suspend Kustomization or HelmRelease?
   - HelmRelease: More granular, only affects one app
   - Kustomization: Might be needed if HelmRelease is created by Kustomization

   *Recommendation*: Suspend HelmRelease directly when possible

6. **Snapshot naming convention**:
   - Proposed: `{app}-{pvc-short-name}-{timestamp}`
   - Example: `my-app-data-20240115-143000`

7. **Git authentication**: How to pass tokens for different backends?
   - Mount as Secret
   - Use external secrets operator if available

8. **Concurrent upgrades**: Allow multiple apps to upgrade simultaneously, or serialize?
   - Serialize is safer (easier to debug failures)
   - Parallel is faster for bulk updates

---

## References

### Kubernetes & Controller Development
- [Kubernetes controller-runtime](https://pkg.go.dev/sigs.k8s.io/controller-runtime)
- [Kubebuilder Book](https://book.kubebuilder.io/)

### Flux
- [Flux HelmRelease API](https://fluxcd.io/flux/components/helm/helmreleases/)
- [Flux Notification Controller](https://fluxcd.io/flux/components/notification/)

### Renovate
- [Renovate Documentation](https://docs.renovatebot.com/)
- [Renovate Flux Manager](https://docs.renovatebot.com/modules/manager/flux/) - Native support for HelmRelease/HelmRepository
- [Renovate Self-Hosted Configuration](https://docs.renovatebot.com/self-hosted-configuration/) - dryRun options
- [Renovate dryRun=lookup Discussion](https://github.com/renovatebot/renovate/discussions/16007) - JSON output for updates

### Storage & Snapshots
- [CSI VolumeSnapshot](https://kubernetes.io/docs/concepts/storage/volume-snapshots/)
- [Rook Ceph Snapshots](https://rook.io/docs/rook/latest/Storage-Configuration/Ceph-CSI/ceph-csi-snapshot/)
