---
title: Renovate Integration
description: Set up Renovate for automatic update detection
---

# Renovate Integration

FluxUp integrates with [Renovate](https://docs.renovatebot.com/) to detect available updates for your applications. Instead of reimplementing version detection, FluxUp leverages Renovate's mature ecosystem that supports 90+ package managers.

## How It Works

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│   Renovate      │     │   ConfigMap     │     │    FluxUp       │
│   CronJob       │────>│  fluxup-updates │────>│   Controller    │
│                 │     │                 │     │                 │
│ dryRun=lookup   │     │ Available       │     │ Updates         │
│ No PRs created  │     │ updates JSON    │     │ ManagedApp      │
└─────────────────┘     └─────────────────┘     │ status          │
                                                └─────────────────┘
```

1. A **CronJob** runs Renovate periodically (default: every 6 hours)
2. Renovate runs in `dryRun=lookup` mode - it detects updates but **does not create PRs**
3. The results are extracted and stored in a **ConfigMap** (`fluxup-updates`)
4. FluxUp's controller **watches** this ConfigMap and updates ManagedApp statuses
5. Users see available updates in the ManagedApp status and can trigger upgrades

## Setup

### 1. Create the Namespace

```bash
kubectl create namespace fluxup-system
```

### 2. Configure Git Credentials

Create a secret with your Git token:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: renovate-credentials
  namespace: fluxup-system
type: Opaque
stringData:
  token: "your-git-token-here"
```

The token needs read access to your repository. For write access (if you want Renovate to create PRs in other contexts), see the [Renovate documentation](https://docs.renovatebot.com/getting-started/running/#authentication).

### 3. Configure Repository Settings

Edit the ConfigMap to point to your repository:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: renovate-env
  namespace: fluxup-system
data:
  # For Gitea
  GITEA_URL: "https://gitea.example.com"
  GITEA_REPO: "user/manifests"

  # For GitHub (uncomment and adjust)
  # GITHUB_COM_TOKEN: "your-token"  # Or use renovate-credentials secret
  # RENOVATE_REPOSITORIES: "owner/repo"

  # For GitLab (uncomment and adjust)
  # GITLAB_URL: "https://gitlab.example.com"
  # GITLAB_REPO: "group/project"
```

### 4. Deploy the CronJob

```bash
kubectl apply -f https://github.com/nbenn/fluxup/releases/latest/download/renovate.yaml
```

Or apply the manifests from `config/renovate/` directly.

### 5. Verify Setup

Trigger a manual run:

```bash
kubectl create job --from=cronjob/renovate-lookup manual-run -n fluxup-system
```

Check the logs:

```bash
kubectl logs -n fluxup-system job/manual-run -f
```

Verify the ConfigMap was created:

```bash
kubectl get configmap fluxup-updates -n fluxup-system -o yaml
```

## Supported Platforms

| Platform | Environment Variables | Notes |
|----------|----------------------|-------|
| **Gitea** | `GITEA_URL`, `GITEA_REPO` | Default configuration |
| **GitHub** | `GITHUB_COM_TOKEN` | Works with github.com |
| **GitHub Enterprise** | `GITHUB_URL`, `GITHUB_TOKEN` | Self-hosted GitHub |
| **GitLab** | `GITLAB_URL`, `GITLAB_TOKEN` | GitLab.com or self-hosted |

### Platform-Specific Configuration

#### GitHub

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: renovate-env
  namespace: fluxup-system
data:
  RENOVATE_PLATFORM: "github"
  RENOVATE_REPOSITORIES: "owner/repo"
```

Update the CronJob command to remove the `--platform` and `--endpoint` flags, or set them appropriately.

#### GitLab

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: renovate-env
  namespace: fluxup-system
data:
  RENOVATE_PLATFORM: "gitlab"
  RENOVATE_ENDPOINT: "https://gitlab.example.com/api/v4"
  RENOVATE_REPOSITORIES: "group/project"
```

## Renovate Configuration

FluxUp includes a default `renovate.json` configuration. You can customize it by creating a ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: renovate-config
  namespace: fluxup-system
data:
  config.json: |
    {
      "$schema": "https://docs.renovatebot.com/renovate-schema.json",
      "extends": ["config:recommended"],
      "onboarding": false,
      "requireConfig": "ignored",
      "dryRun": "lookup",
      "flux": {
        "fileMatch": ["\\.ya?ml$"]
      },
      "helm-values": {
        "fileMatch": ["\\.ya?ml$"]
      },
      "kubernetes": {
        "fileMatch": ["\\.ya?ml$"]
      },
      "packageRules": [
        {
          "description": "Group Flux toolkit updates",
          "matchPackagePatterns": ["^ghcr.io/fluxcd/"],
          "groupName": "Flux toolkit"
        }
      ]
    }
```

Then mount it in the CronJob:

```yaml
volumes:
  - name: config
    configMap:
      name: renovate-config
volumeMounts:
  - name: config
    mountPath: /usr/src/app/config.json
    subPath: config.json
```

### Key Configuration Options

| Option | Description |
|--------|-------------|
| `dryRun: "lookup"` | **Required** - Detect updates without creating PRs |
| `flux.fileMatch` | Patterns for HelmRelease files |
| `helm-values.fileMatch` | Patterns for image tags in Helm values |
| `kubernetes.fileMatch` | Patterns for raw Kubernetes manifests |
| `packageRules` | Custom rules for grouping, pinning, ignoring |

### Detecting Different Update Types

Renovate uses different "managers" to detect updates:

| Manager | What it Detects | Example |
|---------|-----------------|---------|
| `flux` | HelmRelease chart versions | `spec.chart.spec.version: "12.4.0"` |
| `helm-values` | Image tags in Helm values | `values.image.tag: "1.21.0"` |
| `kubernetes` | Images in Deployments/StatefulSets | `image: nginx:1.25.0` |
| `docker` | Container image references | `image: ghcr.io/org/app:v1.0.0` |

## Matching Updates to ManagedApps

Updates are matched to ManagedApps using the `gitPath` field. The path must exactly match what Renovate reports:

```yaml
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: gitea
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"  # Must match Renovate's packageFile
  # ...
```

Renovate reports updates with their file path (e.g., `flux/apps/gitea/helmrelease.yaml`). FluxUp matches this against the `gitPath` field of all ManagedApps.

### Path Matching Tips

- Use the exact path as it appears in your Git repository
- Paths are relative to the repository root
- Check the Renovate logs to see exact paths being reported

## Schedule Configuration

The default schedule runs every 6 hours. Adjust the CronJob schedule as needed:

```yaml
spec:
  schedule: "0 */6 * * *"  # Every 6 hours (default)
  # schedule: "0 * * * *"   # Every hour
  # schedule: "0 0 * * *"   # Daily at midnight
  # schedule: "0 0 * * 0"   # Weekly on Sunday
```

## Troubleshooting

### No updates detected

1. **Check the CronJob ran successfully:**
   ```bash
   kubectl get jobs -n fluxup-system
   kubectl logs -n fluxup-system job/<job-name>
   ```

2. **Check the ConfigMap was created:**
   ```bash
   kubectl get configmap fluxup-updates -n fluxup-system -o yaml
   ```

3. **Verify your `gitPath` matches Renovate's output:**
   - Look for `packageFile` in the Renovate logs
   - Ensure it exactly matches your ManagedApp's `gitPath`

4. **Check Renovate can access your repository:**
   ```bash
   # Test the token manually
   kubectl exec -it deploy/... -- curl -H "Authorization: token $TOKEN" https://gitea.example.com/api/v1/repos/user/repo
   ```

### Updates detected but ManagedApp status not updated

1. **Check the FluxUp controller logs:**
   ```bash
   kubectl logs -n fluxup-system deploy/fluxup-controller-manager
   ```

2. **Verify the updates ConfigMap format:**
   ```bash
   kubectl get configmap fluxup-updates -n fluxup-system -o jsonpath='{.data.updates\.json}' | jq .
   ```

3. **Check the ManagedApp exists and has correct gitPath:**
   ```bash
   kubectl get managedapp <name> -o yaml
   ```

### Renovate authentication errors

1. **Verify the secret exists:**
   ```bash
   kubectl get secret renovate-credentials -n fluxup-system
   ```

2. **Check the token has correct permissions:**
   - For Gitea: `read:repository` scope
   - For GitHub: `repo` scope (or `public_repo` for public repos)
   - For GitLab: `read_repository` scope

3. **Test the token:**
   ```bash
   # Gitea
   curl -H "Authorization: token YOUR_TOKEN" https://gitea.example.com/api/v1/user

   # GitHub
   curl -H "Authorization: token YOUR_TOKEN" https://api.github.com/user
   ```

### Debug mode

Enable verbose logging for troubleshooting:

```yaml
env:
  - name: LOG_LEVEL
    value: debug
```

This will output detailed information about what Renovate is scanning and detecting.

## Advanced Configuration

### Private Registries

For private Helm repositories or container registries, add authentication:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: renovate-config
  namespace: fluxup-system
data:
  config.json: |
    {
      "hostRules": [
        {
          "matchHost": "registry.example.com",
          "username": "user",
          "password": "{{ secrets.REGISTRY_PASSWORD }}"
        },
        {
          "matchHost": "charts.example.com",
          "username": "helm",
          "password": "{{ secrets.HELM_PASSWORD }}"
        }
      ]
    }
```

### Limiting Scan Scope

To speed up scans, limit which files Renovate examines:

```json
{
  "includePaths": ["flux/apps/**"],
  "ignorePaths": ["**/test/**", "**/archive/**"]
}
```

### Version Constraints

Control which updates Renovate reports:

```json
{
  "packageRules": [
    {
      "matchPackagePatterns": ["^nginx"],
      "allowedVersions": "< 2.0.0"
    },
    {
      "matchDatasources": ["docker"],
      "matchPackagePatterns": ["^postgres"],
      "allowedVersions": "/^15\\./"
    }
  ]
}
```

## Resources

- [Renovate Documentation](https://docs.renovatebot.com/)
- [Renovate Flux Manager](https://docs.renovatebot.com/modules/manager/flux/)
- [Renovate Self-Hosted Configuration](https://docs.renovatebot.com/self-hosted-configuration/)
- [Renovate Package Rules](https://docs.renovatebot.com/configuration-options/#packagerules)
