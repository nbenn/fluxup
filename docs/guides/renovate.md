---
title: Renovate Integration
description: Set up Renovate for automatic update detection
---

# Renovate Integration

FluxUp integrates with [Renovate](https://docs.renovatebot.com/) to detect available updates for your applications.

## How It Works

1. A CronJob runs Renovate periodically against your Git repository
2. Renovate detects available updates for Helm charts and container images
3. The results are stored in a ConfigMap (`fluxup-updates`)
4. FluxUp's controller watches this ConfigMap and updates ManagedApp statuses

## Setup

### 1. Create Git Credentials

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

### 2. Configure Renovate

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: renovate-env
  namespace: fluxup-system
data:
  GITEA_URL: "https://gitea.example.com"
  GITEA_REPO: "user/manifests"
```

### 3. Deploy the CronJob

```bash
kubectl apply -f https://github.com/nbenn/fluxup/releases/latest/download/renovate.yaml
```

## Supported Platforms

| Platform | Status |
|----------|--------|
| Gitea | ✅ Supported |
| GitHub | ✅ Supported |
| GitLab | ✅ Supported |

## Matching Updates to ManagedApps

Updates are matched to ManagedApps using the `gitPath` field:

```yaml
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
```

## Troubleshooting

### No updates detected

1. Check the CronJob ran successfully:
   ```bash
   kubectl get jobs -n fluxup-system
   ```

2. Check the ConfigMap was created:
   ```bash
   kubectl get configmap fluxup-updates -n fluxup-system -o yaml
   ```

3. Verify your `gitPath` matches Renovate's output paths
