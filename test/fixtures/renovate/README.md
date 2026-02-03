# Renovate Test Fixtures

This directory contains captured Renovate JSON output for testing the parser.

## Fixtures

| File | Description |
|------|-------------|
| `helm_update.json` | HelmRelease chart version update |
| `image_update.json` | Container image tag update |
| `mixed_updates.json` | Multiple updates across managers |
| `no_updates.json` | Empty result (no updates available) |

## Regenerating Fixtures

To regenerate fixtures from a live Renovate run:

```bash
# From project root, in devcontainer or with Docker available

# 1. Start test Gitea instance
.devcontainer/test-infra/start.sh

# 2. Load environment
source .devcontainer/test-infra/.env

# 3. Seed test repository with manifests
.devcontainer/test-infra/seed-repo.sh

# 4. Run Renovate and capture output
.devcontainer/test-infra/run-renovate.sh

# 5. Stop Gitea when done
.devcontainer/test-infra/stop.sh
```

Or use the Makefile:

```bash
make test-infra-up
make test-fixtures
make test-infra-down
```

## JSON Structure

The parser expects Renovate's JSON log format with `dryRun=lookup`:

```json
{
  "name": "renovate",
  "level": 20,
  "msg": "packageFiles with updates",
  "config": {
    "flux": [
      {
        "packageFile": "flux/apps/gitea/helmrelease.yaml",
        "deps": [
          {
            "depName": "gitea",
            "currentValue": "10.0.0",
            "datasource": "helm",
            "updates": [
              {"newValue": "10.1.0", "updateType": "minor"}
            ]
          }
        ]
      }
    ],
    "helm-values": [...],
    "regex": [...]
  }
}
```
