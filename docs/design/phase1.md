# Phase 1 Implementation Plan: Core Infrastructure

This document outlines the detailed implementation plan for Phase 1 of FluxUp.

## Phase 1 Goals

From the README:
- Project scaffolding with kubebuilder
- ManagedApp CRD and basic controller
- Renovate CronJob setup with `dryRun=lookup`
- Renovate output parser (JSON → UpdateInfo)
- Update ManagedApp status with available updates
- Structured JSON logging

---

## Step 1: Project Scaffolding

### 1.1 Initialize Go Module & Kubebuilder Project

```bash
# Initialize the Go module
go mod init github.com/example/fluxup

# Initialize kubebuilder project
kubebuilder init --domain fluxup.dev --repo github.com/example/fluxup

# Create the API group
kubebuilder create api --group fluxup --version v1alpha1 --kind ManagedApp
```

### 1.2 Project Structure After Scaffolding

```
fluxup/
├── api/
│   └── v1alpha1/
│       ├── managedapp_types.go      # CRD type definitions
│       ├── groupversion_info.go     # API group metadata
│       └── zz_generated.deepcopy.go # Generated
├── cmd/
│   └── main.go                      # Entrypoint
├── config/
│   ├── crd/                         # Generated CRD manifests
│   ├── manager/                     # Controller manager deployment
│   ├── rbac/                        # RBAC manifests
│   └── samples/                     # Example CRs
├── internal/
│   └── controller/
│       └── managedapp_controller.go # Reconciliation logic
├── Dockerfile
├── Makefile
├── go.mod
└── go.sum
```

### 1.3 Dependencies to Add

```go
// go.mod additions (beyond kubebuilder defaults)
require (
    // Flux APIs for HelmRelease/Kustomization types
    github.com/fluxcd/helm-controller/api v1.1.0
    github.com/fluxcd/kustomize-controller/api v1.4.0
    github.com/fluxcd/source-controller/api v1.4.1

    // Snapshot API
    github.com/kubernetes-csi/external-snapshotter/client/v8 v8.0.0

    // Structured logging
    go.uber.org/zap v1.27.0
)
```

### 1.4 Decisions & Questions

**Q1: Repository naming - `fluxup` or `flux-upgrade-controller`?**

The README uses `upgrade-controller` but the API group is `fluxup.dev`. I'll use `fluxup` throughout for consistency.

**Q2: Single binary or separate components?**

The README suggests a single binary that includes:
- Controller manager
- Web UI server
- (Renovate runs separately as a CronJob)

I'll follow this pattern - one binary, multiple goroutines.

---

## Step 2: ManagedApp CRD

### 2.1 Supported Deployment Patterns

FluxUp supports two main deployment patterns:

| Pattern | Example | Version Source |
|---------|---------|----------------|
| **HelmRelease** | `spec.chart.spec.version: "12.4.0"` | Helm chart version |
| **Raw Deployment** | `image: linuxserver/bazarr:latest` | Container image tag |

Both are managed via Flux Kustomizations pointing to a Git path. Renovate detects updates in either pattern.

### 2.2 Type Definitions

The `ManagedApp` CRD is the core resource:

```go
// api/v1alpha1/managedapp_types.go

type ManagedAppSpec struct {
    // Path to the manifest in Git (for matching Renovate output and editing)
    // Examples: "flux/apps/gitea/helmrelease.yaml", "flux/apps/bazarr/deployment.yaml"
    GitPath string `json:"gitPath"`

    // Reference to the Flux Kustomization that reconciles this app (for suspend/resume)
    KustomizationRef ObjectReference `json:"kustomizationRef"`

    // Optional: explicit workload reference for health checks and scaling
    // If omitted, controller infers from the manifest content
    WorkloadRef *WorkloadReference `json:"workloadRef,omitempty"`

    // Volume snapshot configuration
    VolumeSnapshots *VolumeSnapshotConfig `json:"volumeSnapshots,omitempty"`

    // Version policy for updates
    VersionPolicy *VersionPolicy `json:"versionPolicy,omitempty"`

    // Health check configuration
    HealthCheck *HealthCheckConfig `json:"healthCheck,omitempty"`
}

type ManagedAppStatus struct {
    // Current versions
    CurrentVersion *VersionInfo `json:"currentVersion,omitempty"`

    // Available update (if any)
    AvailableUpdate *VersionInfo `json:"availableUpdate,omitempty"`

    // Last upgrade information
    LastUpgrade *UpgradeInfo `json:"lastUpgrade,omitempty"`

    // Standard conditions
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### 2.3 Supporting Types

```go
type ObjectReference struct {
    Name      string `json:"name"`
    Namespace string `json:"namespace,omitempty"`
}

type WorkloadReference struct {
    Kind      string `json:"kind"`      // Deployment, StatefulSet, or HelmRelease
    Name      string `json:"name"`
    Namespace string `json:"namespace,omitempty"`
}

type VolumeSnapshotConfig struct {
    Enabled                 bool     `json:"enabled"`
    VolumeSnapshotClassName string   `json:"volumeSnapshotClassName"`
    PVCs                    []PVCRef `json:"pvcs"`
    RetentionPolicy         *RetentionPolicy `json:"retentionPolicy,omitempty"`
}

type PVCRef struct {
    Name string `json:"name"`
}

type RetentionPolicy struct {
    KeepLast int            `json:"keepLast,omitempty"`
    MaxAge   metav1.Duration `json:"maxAge,omitempty"`
}

type VersionPolicy struct {
    Chart  *ChartVersionPolicy  `json:"chart,omitempty"`
    Images []ImageVersionPolicy `json:"images,omitempty"`
}

type ChartVersionPolicy struct {
    // AutoUpdate: none, patch, minor, major
    AutoUpdate string `json:"autoUpdate,omitempty"`
    Constraint string `json:"constraint,omitempty"`
}

type ImageVersionPolicy struct {
    Name       string `json:"name"`
    AutoUpdate string `json:"autoUpdate,omitempty"`
    Constraint string `json:"constraint,omitempty"`
}

type HealthCheckConfig struct {
    WaitForReady bool            `json:"waitForReady,omitempty"`
    Timeout      metav1.Duration `json:"timeout,omitempty"`
}

type VersionInfo struct {
    Chart      string       `json:"chart,omitempty"`
    AppVersion string       `json:"appVersion,omitempty"`
    Images     []ImageInfo  `json:"images,omitempty"`
    DetectedAt *metav1.Time `json:"detectedAt,omitempty"`
}

type ImageInfo struct {
    Name string `json:"name"`
    Tag  string `json:"tag"`
}

type UpgradeInfo struct {
    FromVersion  string       `json:"fromVersion,omitempty"`
    ToVersion    string       `json:"toVersion,omitempty"`
    StartedAt    *metav1.Time `json:"startedAt,omitempty"`
    CompletedAt  *metav1.Time `json:"completedAt,omitempty"`
    Status       string       `json:"status,omitempty"`
    SnapshotName string       `json:"snapshotName,omitempty"`
}
```

### 2.3 Condition Types

```go
const (
    // ConditionTypeReady indicates the app is healthy
    ConditionTypeReady = "Ready"

    // ConditionTypeUpdateAvailable indicates an update was detected
    ConditionTypeUpdateAvailable = "UpdateAvailable"

    // ConditionTypeUpgrading indicates an upgrade is in progress
    ConditionTypeUpgrading = "Upgrading"
)
```

### 2.4 Validation

We should add kubebuilder markers for validation:

```go
// +kubebuilder:validation:Required
GitPath string `json:"gitPath"`

// +kubebuilder:validation:Required
KustomizationRef ObjectReference `json:"kustomizationRef"`

// +kubebuilder:validation:Enum=none;patch;minor;major
AutoUpdate string `json:"autoUpdate,omitempty"`

// +kubebuilder:validation:Enum=Deployment;StatefulSet;HelmRelease
Kind string `json:"kind"`
```

---

## Step 3: ManagedApp Controller (Basic)

### 3.1 Controller Responsibilities in Phase 1

The Phase 1 controller is minimal:

1. **Watch ManagedApps** - React to create/update/delete
2. **Fetch workload status** - From HelmRelease, Deployment, or StatefulSet (based on workloadRef)
3. **Set conditions** - Ready, UpdateAvailable based on status
4. **Reconcile periodically** - Re-check workload state

It does NOT yet:
- Create snapshots
- Trigger upgrades
- Interact with Git

### 3.2 Reconciliation Logic

```go
// internal/controller/managedapp_controller.go

func (r *ManagedAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx)

    // 1. Fetch the ManagedApp
    var app fluxupv1alpha1.ManagedApp
    if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // 2. Fetch the workload and determine health
    ready, err := r.checkWorkloadHealth(ctx, &app)
    if err != nil {
        // Set condition: workload not found, requeue with backoff
        meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
            Type:    fluxupv1alpha1.ConditionTypeReady,
            Status:  metav1.ConditionFalse,
            Reason:  "WorkloadNotFound",
            Message: err.Error(),
        })
        r.Status().Update(ctx, &app)
        return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
    }

    // 3. Update Ready condition
    status := metav1.ConditionFalse
    if ready {
        status = metav1.ConditionTrue
    }
    meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
        Type:   fluxupv1alpha1.ConditionTypeReady,
        Status: status,
        Reason: "WorkloadStatus",
    })

    // 4. Persist status
    if err := r.Status().Update(ctx, &app); err != nil {
        return ctrl.Result{}, err
    }

    // 5. Requeue after interval to re-sync
    return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// checkWorkloadHealth determines if the workload is healthy
func (r *ManagedAppReconciler) checkWorkloadHealth(ctx context.Context, app *fluxupv1alpha1.ManagedApp) (bool, error) {
    if app.Spec.WorkloadRef == nil {
        // No explicit workload ref - just check the Kustomization is ready
        return r.checkKustomizationHealth(ctx, app)
    }

    ref := app.Spec.WorkloadRef
    ns := ref.Namespace
    if ns == "" {
        ns = app.Namespace
    }
    key := types.NamespacedName{Name: ref.Name, Namespace: ns}

    switch ref.Kind {
    case "HelmRelease":
        var hr helmv2.HelmRelease
        if err := r.Get(ctx, key, &hr); err != nil {
            return false, err
        }
        return isHelmReleaseReady(&hr), nil

    case "Deployment":
        var deploy appsv1.Deployment
        if err := r.Get(ctx, key, &deploy); err != nil {
            return false, err
        }
        return deploy.Status.ReadyReplicas == *deploy.Spec.Replicas, nil

    case "StatefulSet":
        var sts appsv1.StatefulSet
        if err := r.Get(ctx, key, &sts); err != nil {
            return false, err
        }
        return sts.Status.ReadyReplicas == *sts.Spec.Replicas, nil

    default:
        return false, fmt.Errorf("unsupported workload kind: %s", ref.Kind)
    }
}

func (r *ManagedAppReconciler) checkKustomizationHealth(ctx context.Context, app *fluxupv1alpha1.ManagedApp) (bool, error) {
    ref := app.Spec.KustomizationRef
    ns := ref.Namespace
    if ns == "" {
        ns = "flux-system"
    }
    key := types.NamespacedName{Name: ref.Name, Namespace: ns}

    var ks kustomizev1.Kustomization
    if err := r.Get(ctx, key, &ks); err != nil {
        return false, err
    }

    for _, cond := range ks.Status.Conditions {
        if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
            return true, nil
        }
    }
    return false, nil
}

func isHelmReleaseReady(hr *helmv2.HelmRelease) bool {
    for _, cond := range hr.Status.Conditions {
        if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
            return true
        }
    }
    return false
}
```

### 3.3 Watches

The controller watches ManagedApps and relevant workload types:

```go
func (r *ManagedAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&fluxupv1alpha1.ManagedApp{}).
        // Watch workload types and map back to ManagedApps
        Watches(&helmv2.HelmRelease{}, handler.EnqueueRequestsFromMapFunc(r.findManagedAppsForWorkload)).
        Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.findManagedAppsForWorkload)).
        Watches(&appsv1.StatefulSet{}, handler.EnqueueRequestsFromMapFunc(r.findManagedAppsForWorkload)).
        Complete(r)
}

func (r *ManagedAppReconciler) findManagedAppsForWorkload(ctx context.Context, obj client.Object) []reconcile.Request {
    // Find all ManagedApps that reference this workload
    var apps fluxupv1alpha1.ManagedAppList
    if err := r.List(ctx, &apps); err != nil {
        return nil
    }

    var requests []reconcile.Request
    for _, app := range apps.Items {
        if app.Spec.WorkloadRef == nil {
            continue
        }
        ref := app.Spec.WorkloadRef
        ns := ref.Namespace
        if ns == "" {
            ns = app.Namespace
        }
        // Match by kind, name, and namespace
        if ref.Kind == obj.GetObjectKind().GroupVersionKind().Kind &&
            ref.Name == obj.GetName() &&
            ns == obj.GetNamespace() {
            requests = append(requests, reconcile.Request{
                NamespacedName: types.NamespacedName{
                    Name:      app.Name,
                    Namespace: app.Namespace,
                },
            })
        }
    }
    return requests
}
```

---

## Step 4: Renovate Integration

### 4.1 Architecture Decision: How to Run Renovate

**Decision: Kubernetes CronJob**

Renovate runs as a separate CronJob. Benefits:
- Own lifecycle, resources, logs
- Runs on schedule independent of controller
- Easier to debug Renovate issues

### 4.2 Logs vs Actionable Data

Two distinct outputs from Renovate:

| Output | Destination | Purpose |
|--------|-------------|---------|
| **Logs** | stdout → cluster observability (Loki, etc.) | Debugging, audit |
| **Actionable data** | ConfigMap | Minimal update info for controller/UI |

The actionable data is small - just the updates detected:
```json
{"app": "my-app", "file": "apps/my-app/helmrelease.yaml", "current": "12.4.0", "available": "12.5.0"}
```

Even with hundreds of apps, this is a few KB - well within ConfigMap's 1 MiB limit.

### 4.3 Renovate CronJob Manifest

The Job runs Renovate, then post-processes the output to extract only the update summary into a ConfigMap:

```yaml
# config/renovate/cronjob.yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: fluxup-renovate
  namespace: fluxup-system
spec:
  schedule: "0 * * * *"  # Hourly
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      backoffLimit: 2
      template:
        spec:
          restartPolicy: OnFailure
          serviceAccountName: fluxup-renovate
          containers:
            - name: renovate
              image: renovate/renovate:39
              env:
                - name: LOG_FORMAT
                  value: "json"
                - name: LOG_LEVEL
                  value: "debug"
                - name: RENOVATE_CONFIG_FILE
                  value: "/config/config.json"
                - name: RENOVATE_TOKEN
                  valueFrom:
                    secretKeyRef:
                      name: fluxup-git-credentials
                      key: token
              volumeMounts:
                - name: config
                  mountPath: /config
              command:
                - /bin/sh
                - -c
                - |
                  # Run Renovate - logs go to stdout (picked up by cluster logging)
                  # Extract only the actionable update info and write to ConfigMap
                  renovate 2>&1 | tee /dev/stderr | \
                    grep '"packageFiles with updates"' | \
                    head -1 > /tmp/updates.json

                  # Update ConfigMap with results
                  kubectl create configmap fluxup-updates \
                    --from-file=updates.json=/tmp/updates.json \
                    --dry-run=client -o yaml | kubectl apply -f -
          volumes:
            - name: config
              configMap:
                name: fluxup-renovate-config
```

The controller watches the `fluxup-updates` ConfigMap for changes.

### 4.3 Renovate Configuration

```json
// config/renovate/config.json
{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "platform": "gitea",
  "endpoint": "${GITEA_URL}/api/v1",
  "repositories": ["${GIT_ORG}/${GIT_REPO}"],
  "dryRun": "lookup",
  "onboarding": false,
  "requireConfig": "ignored",
  "allowedPostUpgradeCommands": [],
  "flux": {
    "fileMatch": ["\\.ya?ml$"]
  },
  "helm-values": {
    "fileMatch": ["\\.ya?ml$"]
  }
}
```

### 4.4 Output Format Analysis

Renovate with `dryRun=lookup` and `LOG_FORMAT=json` outputs JSON lines. The key message to parse contains `"packageFiles with updates"`:

```json
{
  "name": "renovate",
  "level": 20,
  "msg": "packageFiles with updates",
  "config": {
    "flux": [
      {
        "packageFile": "apps/my-app/helmrelease.yaml",
        "deps": [
          {
            "depName": "my-app",
            "currentValue": "12.4.0",
            "datasource": "helm",
            "registryUrl": "https://charts.example.com/",
            "updates": [
              {
                "newValue": "12.5.0",
                "updateType": "minor"
              }
            ]
          }
        ]
      }
    ]
  }
}
```

### 4.5 Decisions & Questions

**Q3: How does the controller know when Renovate has run?**

The controller watches the `fluxup-updates` ConfigMap. When the CronJob updates it, the controller reconciles.

**Q4: How do we map Renovate output to ManagedApps?**

Renovate reports updates by file path (e.g., `apps/my-app/helmrelease.yaml`). We need to:
1. Parse the file path to extract the HelmRelease name/namespace
2. Match against ManagedApps by their `helmReleaseRef`

This requires either:
- A naming convention (file path encodes namespace/name)
- Or reading the YAML to extract metadata

For Phase 1, I'll assume a convention: `{namespace}/{name}/helmrelease.yaml` or similar. We can make this configurable later.

---

## Step 5: Renovate Output Parser

### 5.1 Parser Types

```go
// internal/renovate/types.go

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
```

### 5.2 Parser Implementation

The parser reads from the ConfigMap populated by the Renovate CronJob:

```go
// internal/renovate/parser.go

type Parser struct {
    client    client.Client
    namespace string
    configMap string
}

func NewParser(c client.Client, namespace, configMapName string) *Parser {
    return &Parser{
        client:    c,
        namespace: namespace,
        configMap: configMapName,
    }
}

// Parse reads the ConfigMap and extracts update info
func (p *Parser) Parse(ctx context.Context) ([]UpdateInfo, error) {
    var cm corev1.ConfigMap
    key := types.NamespacedName{Name: p.configMap, Namespace: p.namespace}
    if err := p.client.Get(ctx, key, &cm); err != nil {
        return nil, fmt.Errorf("getting updates configmap: %w", err)
    }

    data, ok := cm.Data["updates.json"]
    if !ok || data == "" {
        return nil, nil // No updates
    }

    var entry RenovateLogEntry
    if err := json.Unmarshal([]byte(data), &entry); err != nil {
        return nil, fmt.Errorf("parsing updates json: %w", err)
    }

    if entry.Config == nil {
        return nil, nil
    }

    return p.extractUpdates(entry.Config), nil
}

func (p *Parser) extractUpdates(config *RenovateConfig) []UpdateInfo {
    var updates []UpdateInfo

    // Process Flux manager updates (HelmRelease chart versions)
    for _, pf := range config.Flux {
        for _, dep := range pf.Deps {
            if len(dep.Updates) > 0 {
                updates = append(updates, UpdateInfo{
                    PackageFile:    pf.PackageFile,
                    DependencyName: dep.DepName,
                    CurrentVersion: dep.CurrentValue,
                    NewVersion:     dep.Updates[0].NewValue,
                    UpdateType:     dep.Updates[0].UpdateType,
                    Datasource:     dep.Datasource,
                })
            }
        }
    }

    // Process helm-values manager updates (image tags in values)
    for _, pf := range config.HelmValues {
        for _, dep := range pf.Deps {
            if len(dep.Updates) > 0 {
                updates = append(updates, UpdateInfo{
                    PackageFile:    pf.PackageFile,
                    DependencyName: dep.DepName,
                    CurrentVersion: dep.CurrentValue,
                    NewVersion:     dep.Updates[0].NewValue,
                    UpdateType:     dep.Updates[0].UpdateType,
                    Datasource:     dep.Datasource,
                })
            }
        }
    }

    return updates
}
```

### 5.3 Mapping Updates to ManagedApps

Matching is explicit: `ManagedApp.spec.gitPath` must exactly match `update.PackageFile`.

```go
// internal/renovate/mapper.go

type Mapper struct {
    client client.Client
}

func NewMapper(c client.Client) *Mapper {
    return &Mapper{client: c}
}

// MapUpdatesToManagedApps matches Renovate updates to ManagedApp resources
func (m *Mapper) MapUpdatesToManagedApps(ctx context.Context, updates []UpdateInfo) (map[types.NamespacedName]*VersionInfo, error) {
    // Fetch all ManagedApps
    var apps fluxupv1alpha1.ManagedAppList
    if err := m.client.List(ctx, &apps); err != nil {
        return nil, err
    }

    // Build lookup map: gitPath -> ManagedApp key
    pathToApp := make(map[string]types.NamespacedName)
    for _, app := range apps.Items {
        if app.Spec.GitPath != "" {
            pathToApp[app.Spec.GitPath] = types.NamespacedName{
                Name:      app.Name,
                Namespace: app.Namespace,
            }
        }
    }

    result := make(map[types.NamespacedName]*VersionInfo)

    for _, update := range updates {
        appKey, ok := pathToApp[update.PackageFile]
        if !ok {
            continue // No ManagedApp for this file
        }

        if result[appKey] == nil {
            result[appKey] = &VersionInfo{}
        }

        if update.Datasource == "helm" {
            result[appKey].Chart = update.NewVersion
        } else if update.Datasource == "docker" {
            result[appKey].Images = append(result[appKey].Images, ImageInfo{
                Name: update.DependencyName,
                Tag:  update.NewVersion,
            })
        }
    }

    return result, nil
}
```

---

## Step 6: Update ManagedApp Status

### 6.1 Status Updater Component

```go
// internal/renovate/updater.go

type StatusUpdater struct {
    client client.Client
    parser *Parser
    mapper *Mapper
}

func NewStatusUpdater(c client.Client, outputDir string) *StatusUpdater {
    return &StatusUpdater{
        client: c,
        parser: NewParser(outputDir),
        mapper: NewMapper(c),
    }
}

// ProcessRenovateOutput parses Renovate output and updates ManagedApp statuses
func (u *StatusUpdater) ProcessRenovateOutput(ctx context.Context) error {
    log := log.FromContext(ctx)

    // 1. Parse Renovate output
    updates, err := u.parser.ParseLatestOutput(ctx)
    if err != nil {
        return fmt.Errorf("parsing renovate output: %w", err)
    }
    log.Info("Parsed Renovate output", "updateCount", len(updates))

    // 2. Map updates to ManagedApps
    appUpdates, err := u.mapper.MapUpdatesToManagedApps(ctx, updates)
    if err != nil {
        return fmt.Errorf("mapping updates: %w", err)
    }

    // 3. Update each ManagedApp's status
    for key, versionInfo := range appUpdates {
        if err := u.updateAppStatus(ctx, key, versionInfo); err != nil {
            log.Error(err, "Failed to update ManagedApp status", "app", key)
            continue
        }
        log.Info("Updated ManagedApp with available update",
            "app", key,
            "newVersion", versionInfo.Chart)
    }

    return nil
}

func (u *StatusUpdater) updateAppStatus(ctx context.Context, key types.NamespacedName, update *VersionInfo) error {
    var app fluxupv1alpha1.ManagedApp
    if err := u.client.Get(ctx, key, &app); err != nil {
        return err
    }

    // Set available update
    now := metav1.Now()
    update.DetectedAt = &now
    app.Status.AvailableUpdate = update

    // Set UpdateAvailable condition
    meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
        Type:               fluxupv1alpha1.ConditionTypeUpdateAvailable,
        Status:             metav1.ConditionTrue,
        Reason:             "NewVersionDetected",
        Message:            fmt.Sprintf("Update available: %s", update.Chart),
        ObservedGeneration: app.Generation,
    })

    return u.client.Status().Update(ctx, &app)
}
```

### 6.2 Integration with Controller

The controller watches the `fluxup-updates` ConfigMap. When it changes (after a Renovate run), the controller processes the updates:

```go
// internal/controller/updates_controller.go

type UpdatesConfigMapReconciler struct {
    client.Client
    StatusUpdater *renovate.StatusUpdater
}

func (r *UpdatesConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx)

    // Only process our specific ConfigMap
    if req.Name != "fluxup-updates" || req.Namespace != "fluxup-system" {
        return ctrl.Result{}, nil
    }

    log.Info("Processing Renovate updates")
    if err := r.StatusUpdater.ProcessRenovateOutput(ctx); err != nil {
        log.Error(err, "Failed to process Renovate output")
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}

func (r *UpdatesConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&corev1.ConfigMap{}).
        Complete(r)
}
```

---

## Step 7: Structured Logging

### 7.1 Logging Setup

Kubebuilder uses `logr` with `zap` as the backend. We should ensure structured fields are used consistently:

```go
// internal/controller/managedapp_controller.go

func (r *ManagedAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx).WithValues(
        "managedApp", req.NamespacedName,
    )

    // ... reconciliation logic ...

    log.Info("Reconciled ManagedApp",
        "currentVersion", app.Status.CurrentVersion.Chart,
        "updateAvailable", app.Status.AvailableUpdate != nil,
    )
}
```

### 7.2 Key Log Events

| Event | Level | Fields |
|-------|-------|--------|
| ManagedApp reconciled | Info | managedApp, currentVersion, updateAvailable |
| HelmRelease not found | Error | managedApp, helmReleaseRef |
| Renovate output parsed | Info | updateCount, outputFile |
| Update mapped to app | Info | managedApp, newVersion, updateType |
| Failed to update status | Error | managedApp, error |

---

## Step 8: Testing Strategy

### 8.1 Unit Tests

```
internal/renovate/parser_test.go      # Test JSON parsing
internal/renovate/mapper_test.go      # Test update-to-app mapping
internal/controller/*_test.go         # Controller logic tests (envtest)
```

### 8.2 Integration Tests

Using `envtest` (kubebuilder's test framework):

```go
// internal/controller/managedapp_controller_test.go

var _ = Describe("ManagedApp Controller", func() {
    Context("When reconciling a ManagedApp", func() {
        It("Should update status from HelmRelease", func() {
            // Create a HelmRelease
            hr := &helmv2.HelmRelease{...}
            Expect(k8sClient.Create(ctx, hr)).To(Succeed())

            // Create a ManagedApp referencing it
            app := &fluxupv1alpha1.ManagedApp{...}
            Expect(k8sClient.Create(ctx, app)).To(Succeed())

            // Wait for reconciliation
            Eventually(func() string {
                var updated fluxupv1alpha1.ManagedApp
                k8sClient.Get(ctx, key, &updated)
                return updated.Status.CurrentVersion.Chart
            }).Should(Equal("12.4.0"))
        })
    })
})
```

### 8.3 Sample Renovate Output for Testing

```json
{"name":"renovate","level":20,"msg":"packageFiles with updates","config":{"flux":[{"packageFile":"apps/my-app/helmrelease.yaml","deps":[{"depName":"my-app","currentValue":"12.4.0","datasource":"helm","registryUrl":"https://charts.example.com/","updates":[{"newValue":"12.5.0","updateType":"minor"}]}]}]}}
```

---

## Implementation Order

I recommend implementing in this order:

1. **Project scaffolding** (Step 1) - Get the skeleton in place
2. **ManagedApp CRD types** (Step 2) - Define the data model
3. **Basic controller** (Step 3) - Watch ManagedApps, read HelmRelease status
4. **Renovate parser** (Step 5) - Parse JSON output independently
5. **Mapper** (Step 5.3) - Connect updates to ManagedApps
6. **Status updater** (Step 6) - Wire it all together
7. **Renovate CronJob manifests** (Step 4) - Deployment config
8. **Tests** (Step 8) - Validate everything works

---

## Resolved Decisions

| Decision | Choice |
|----------|--------|
| Renovate execution | CronJob (separate from controller) |
| Output sharing | ConfigMap (filtered, not full logs) |
| Update-to-app mapping | Explicit `gitPath` field |
| Namespace scoping | Namespace-scoped ManagedApps |
| Workload types | HelmRelease, Deployment, StatefulSet all supported |
| Missing workload handling | Set Ready=False condition, requeue with backoff |

---

## Estimated Effort

| Step | Estimated Time |
|------|----------------|
| Project scaffolding | 1-2 hours |
| ManagedApp CRD | 2-3 hours |
| Basic controller | 3-4 hours |
| Renovate parser | 2-3 hours |
| Mapper + Status updater | 2-3 hours |
| Renovate manifests | 1-2 hours |
| Tests | 3-4 hours |
| **Total** | **~15-20 hours** |

---

## Next Steps After Phase 1

Once Phase 1 is complete, we'll have:
- A working CRD and controller
- Renovate detecting updates
- ManagedApp statuses showing available updates

Phase 2 will add:
- UpgradeRequest CRD
- Git manager (Gitea API)
- Snapshot creation
- Flux suspend/resume
- The actual upgrade workflow
