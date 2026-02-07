---
title: Architecture & Design
description: Complete design documentation for FluxUp
---

# FluxUp Architecture & Design

This document contains the detailed architecture, CRD specifications, controller components, and implementation details for FluxUp.

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

## Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                                                                              │
│  ┌────────────────┐     ┌──────────────────────────────────────────────────┐ │
│  │   Renovate     │     │              Upgrade Controller                  │ │
│  │   (CronJob)    │     │                                                  │ │
│  │                │     │  ┌──────────────────┐   ┌─────────────────────┐  │ │
│  │  - dryRun=     │     │  │  Upgrade Manager │   │   Web UI Server     │  │ │
│  │    lookup      │────>│  │                  │   │                     │  │ │
│  │  - JSON logs   │     │  │  - Parse updates │   │  - List pending     │  │ │
│  │  - Scheduled   │     │  │  - Create snap   │   │  - Trigger upgrade  │  │ │
│  │                │     │  │  - Commit to Git │   │  - View status      │  │ │
│  └────────────────┘     │  │  - Monitor Flux  │   │  - Rollback         │  │ │
│                         │  └────────┬─────────┘   └──────────┬──────────┘  │ │
│                         │           │                        │             │ │
│                         │           v                        v             │ │
│                         │  ┌─────────────────────────────────────────────┐ │ │
│                         │  │            Reconciliation Loop              │ │ │
│                         │  │   Watches: ManagedApp, HelmRelease,         │ │ │
│                         │  │            VolumeSnapshot, Git commits      │ │ │
│                         │  └─────────────────────────────────────────────┘ │ │
│                         └──────────────────────────────────────────────────┘ │
│                                              │                               │
└──────────────────────────────────────────────┼───────────────────────────────┘
         │                                     │
         │                                     v
         │                     ┌───────────────────────────────────┐
         │                     │         Kubernetes API            │
         │                     │                                   │
         │                     │  ┌─────────────┐ ┌─────────────┐  │
         │                     │  │ HelmRelease │ │ VolSnapshot │  │
         │                     │  └─────────────┘ └─────────────┘  │
         │                     └───────────────────────────────────┘
         │                                     │
         v                                     v
┌──────────────────┐               ┌──────────────────┐
│   Git Server     │<──────────────│      Flux        │
│   (Gitea, etc.)  │    watches    │                  │
│                  │──────────────>│  - Reconciles    │
│  - HelmReleases  │               │  - Applies       │
│  - Deployments   │               │                  │
└──────────────────┘               └──────────────────┘
```

## Renovate Integration

FluxUp uses [Renovate](https://docs.renovatebot.com/) for version detection. Renovate has a dedicated [Flux manager](https://docs.renovatebot.com/modules/manager/flux/) that understands HelmRelease ↔ HelmRepository relationships, handles private registries, and supports 90+ package managers.

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
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: my-app
  namespace: my-app
spec:
  # Reference to the HelmRelease to manage
  helmReleaseRef:
    name: my-app
    namespace: my-app

  # Reference to the Kustomization that owns the HelmRelease
  # Used to suspend/resume during upgrades
  kustomizationRef:
    name: my-app
    namespace: flux-system

  # PVCs to snapshot before upgrade
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: csi-snapclass
    # Explicit list of PVCs to snapshot (required)
    pvcs:
      - name: data-my-app-0
      - name: data-my-app-postgresql-0
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
apiVersion: fluxup.dev/v1alpha1
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
apiVersion: fluxup.dev/v1alpha1
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

### 2. YAML Editing Strategy

Editing YAML programmatically while preserving comments, formatting, and structure is non-trivial. We use **kyaml** (Kustomize's YAML library).

[kyaml](https://pkg.go.dev/sigs.k8s.io/kustomize/kyaml/yaml) is what Flux's [image-automation-controller](https://github.com/fluxcd/image-automation-controller) uses. It's designed specifically for Kubernetes resources and preserves YAML structure and comments.

**Pros:**
- Battle-tested by Kustomize and Flux
- Preserves comments and formatting
- Understands Kubernetes resource structure
- Go-native (no external process)

**Cons:**
- Has known bugs with non-ASCII characters, anchors/aliases
- Some edge cases with complex YAML

### 3. Git Manager

Handles committing version changes to Git (the source of truth).

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
1. Validate upgrade request
2. Suspend the Flux Kustomization (prevent premature reconciliation)
3. Create VolumeSnapshots for all PVCs
4. Wait for snapshots to be ready (ReadyToUse=true)
5. Commit version change to Git via GitManager
6. Resume the Flux Kustomization
7. Wait for HelmRelease reconciliation (Flux picks up Git change)
8. Run health checks
9. Update status and send notifications

### 5. Snapshot Manager

Handles VolumeSnapshot lifecycle:
- Create pre-upgrade snapshots
- Apply retention policy (keepLast, maxAge)
- Restore PVCs from snapshots for rollback

---

## Web UI

Inspired by the [Flux Operator Web UI](https://fluxoperator.dev/web-ui/).

### Technology Stack

- **Backend**: Embedded Go HTTP server (same binary as controller)
- **Frontend**: Lightweight - either server-rendered HTML with htmx or simple React/Vue SPA
- **Authentication**: OIDC (configurable provider)

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

PUT  /api/settings                # Update global settings

# WebSocket for real-time updates
WS   /api/ws                      # Real-time status updates

# Prometheus metrics
GET  /metrics                     # Prometheus metrics endpoint
```

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
│ Create VolumeSnapshots    │◀── status: Snapshotting
│ for each configured PVC   │
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
│           │ │             │
│ Cleanup   │ │ Await       │
│           │ │ rollback    │
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
└───────────────────────────┘
```

---

## Project Structure

```
fluxup/
├── api/
│   └── v1alpha1/
│       ├── managedapp_types.go
│       ├── upgraderequest_types.go
│       ├── rollbackrequest_types.go
│       ├── groupversion_info.go
│       └── zz_generated.deepcopy.go
├── cmd/
│   └── main.go
├── internal/
│   ├── controller/
│   │   ├── managedapp_controller.go
│   │   ├── upgraderequest_controller.go
│   │   └── rollbackrequest_controller.go
│   ├── renovate/
│   │   ├── parser.go
│   │   └── mapper.go
│   ├── git/
│   │   ├── interface.go
│   │   ├── gitea.go
│   │   ├── github.go
│   │   └── generic.go
│   ├── snapshot/
│   │   ├── manager.go
│   │   └── discovery.go
│   ├── upgrade/
│   │   ├── manager.go
│   │   └── rollback.go
│   ├── flux/
│   │   ├── suspend.go
│   │   └── watch.go
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
├── config/
│   ├── crd/
│   ├── rbac/
│   ├── manager/
│   ├── renovate/
│   └── samples/
├── web/
│   └── (frontend source)
├── Dockerfile
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
  name: fluxup-controller
rules:
  # ManagedApp, UpgradeRequest, RollbackRequest (own CRDs)
  - apiGroups: ["fluxup.dev"]
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

## Implementation Phases

### Phase 1: Core Infrastructure ✅
- [x] Project scaffolding with kubebuilder
- [x] ManagedApp CRD and basic controller
- [x] Renovate CronJob setup with `dryRun=lookup`
- [x] Renovate output parser (JSON → UpdateInfo)
- [x] Update ManagedApp status with available updates
- [x] Structured JSON logging

### Phase 2: Upgrade Workflow ✅
- [x] UpgradeRequest CRD and controller
- [x] Git manager interface + Gitea implementation
- [x] VolumeSnapshot creation before upgrade
- [x] Snapshot retention policy (generational, maxCount-based)
- [x] Flux Kustomization suspend/resume integration
- [x] YAML editor (kyaml-based, preserves comments)
- [x] Chart version updates (default path for HelmRelease)
- [x] Image tag updates (custom versionPath)
- [x] Health check after upgrade (wait for workload Ready)
- [x] Dry run mode (validation only)
- [x] Failure recovery (auto-resume Kustomization)

### Phase 3: Rollback Workflow ✅
- [x] RollbackRequest CRD and controller
- [x] PVC restore from VolumeSnapshot
- [x] Git revert functionality
- [x] Workload scale down/up orchestration
- [x] Auto-rollback on upgrade failure

### MVP: Bug Fixes & Hardening ✅
Post-Phase 3 fixes required before production use. See [mvp.md](mvp.md) for details.
- [x] PVC-centric workload discovery (auto-detect workloads from PVC mounts)
- [x] Direct workload health checks (avoid stale ManagedApp conditions)
- [x] Fix dry run finalizer leak
- [x] Fix rollback timeout calculation
- [x] Enhanced dry-run with preflight checks and quiescence cycle
- [x] Git diff preview for all operations

### Phase 4: Web UI
- [ ] Embedded HTTP server
- [ ] List ManagedApps with update status
- [ ] Trigger upgrade via UI
- [ ] Trigger rollback via UI
- [ ] Upgrade/rollback progress view
- [ ] OIDC authentication

### Phase 5: Observability
- [ ] Prometheus metrics endpoint (`/metrics`)
- [ ] Key metrics: upgrades total, duration, failures, available updates gauge
- [ ] Snapshot retention/cleanup automation

### Phase 6: Advanced Features
- [ ] Additional Git backends (GitHub, GitLab, generic)
- [ ] On-demand Renovate scan (trigger update check from UI)
- [ ] Bulk upgrades (upgrade all with available updates)
- [ ] Multi-HelmRelease apps (group related releases, e.g., app + database)
- [ ] Multi-image updates (multiple versionPaths per ManagedApp)
- [ ] Dependency ordering (upgrade A before B)
- [ ] Version pinning/skipping in UI
- [ ] PVC auto-discovery from HelmRelease labels/selectors

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
