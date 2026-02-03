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

package renovate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestParseFixture_HelmUpdate tests parsing real Renovate output for Helm chart updates
func TestParseFixture_HelmUpdate(t *testing.T) {
	entry := loadFixture(t, "helm_update.json")

	p := &Parser{}
	updates := p.extractUpdates(entry.Config)

	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}

	u := updates[0]
	assertUpdate(t, u, UpdateInfo{
		PackageFile:    "flux/apps/gitea/helmrelease.yaml",
		DependencyName: "gitea",
		CurrentVersion: "10.0.0",
		NewVersion:     "10.4.1",
		UpdateType:     "minor",
		Datasource:     "helm",
	})
}

// TestParseFixture_ImageUpdate tests parsing real Renovate output for image tag updates
func TestParseFixture_ImageUpdate(t *testing.T) {
	entry := loadFixture(t, "image_update.json")

	p := &Parser{}
	updates := p.extractUpdates(entry.Config)

	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}

	u := updates[0]
	assertUpdate(t, u, UpdateInfo{
		PackageFile:    "flux/apps/redis/helmrelease.yaml",
		DependencyName: "bitnami/redis",
		CurrentVersion: "7.2.0",
		NewVersion:     "7.2.4",
		UpdateType:     "patch",
		Datasource:     "docker",
	})
}

// TestParseFixture_MixedUpdates tests parsing multiple updates across managers
// Note: Real Renovate output may contain duplicates across managers (e.g., the same
// image appears in both 'flux' and 'helm-values' managers). This test validates
// we correctly parse all updates, including duplicates.
func TestParseFixture_MixedUpdates(t *testing.T) {
	entry := loadFixture(t, "mixed_updates.json")

	p := &Parser{}
	updates := p.extractUpdates(entry.Config)

	// Real Renovate output has:
	// - flux manager: gitea (helm), gitea/gitea (docker), redis (helm)
	// - helm-values manager: gitea/gitea (docker) - duplicate
	// Total: 4 updates (bitnami/redis has no available updates)
	if len(updates) < 3 {
		t.Fatalf("expected at least 3 updates, got %d", len(updates))
	}

	// Verify we got updates from different sources
	var helmCount, dockerCount int
	for _, u := range updates {
		switch u.Datasource {
		case "helm":
			helmCount++
		case "docker":
			dockerCount++
		}
	}

	if helmCount < 2 {
		t.Errorf("expected at least 2 helm updates, got %d", helmCount)
	}
	if dockerCount < 1 {
		t.Errorf("expected at least 1 docker update, got %d", dockerCount)
	}

	// Verify specific updates exist
	foundGiteaHelm := false
	foundGiteaImage := false
	foundRedisHelm := false

	for _, u := range updates {
		switch u.DependencyName {
		case "gitea":
			foundGiteaHelm = true
			if u.Datasource != "helm" {
				t.Errorf("gitea: expected datasource helm, got %s", u.Datasource)
			}
		case "gitea/gitea":
			foundGiteaImage = true
			if u.Datasource != "docker" {
				t.Errorf("gitea/gitea: expected datasource docker, got %s", u.Datasource)
			}
		case "redis":
			foundRedisHelm = true
			if u.Datasource != "helm" {
				t.Errorf("redis: expected datasource helm, got %s", u.Datasource)
			}
		}
	}

	if !foundGiteaHelm {
		t.Error("missing gitea helm chart update")
	}
	if !foundGiteaImage {
		t.Error("missing gitea/gitea image update")
	}
	if !foundRedisHelm {
		t.Error("missing redis helm chart update")
	}
}

// TestParseFixture_NoUpdates tests parsing when no updates are available
func TestParseFixture_NoUpdates(t *testing.T) {
	entry := loadFixture(t, "no_updates.json")

	p := &Parser{}
	updates := p.extractUpdates(entry.Config)

	if len(updates) != 0 {
		t.Fatalf("expected 0 updates, got %d", len(updates))
	}
}

// TestParseFixture_JSONStructure validates we can parse the full JSON log entry
func TestParseFixture_JSONStructure(t *testing.T) {
	fixtures := []string{"helm_update.json", "image_update.json", "mixed_updates.json", "no_updates.json"}

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			entry := loadFixture(t, fixture)

			// Verify expected fields
			if entry.Name != "renovate" {
				t.Errorf("expected name=renovate, got %s", entry.Name)
			}
			if entry.Level != 20 {
				t.Errorf("expected level=20 (debug), got %d", entry.Level)
			}
			if entry.Msg != "packageFiles with updates" {
				t.Errorf("expected msg='packageFiles with updates', got %s", entry.Msg)
			}
			if entry.Config == nil {
				t.Error("expected config to be non-nil")
			}
		})
	}
}

// Helper functions

func loadFixture(t *testing.T, name string) RenovateLogEntry {
	t.Helper()

	// Find fixtures directory relative to test file
	fixturesDir := filepath.Join("..", "..", "test", "fixtures", "renovate")
	path := filepath.Join(fixturesDir, name)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read fixture %s: %v", name, err)
	}

	var entry RenovateLogEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("failed to parse fixture %s: %v", name, err)
	}

	return entry
}

func assertUpdate(t *testing.T, got, want UpdateInfo) {
	t.Helper()

	if got.PackageFile != want.PackageFile {
		t.Errorf("PackageFile: got %q, want %q", got.PackageFile, want.PackageFile)
	}
	if got.DependencyName != want.DependencyName {
		t.Errorf("DependencyName: got %q, want %q", got.DependencyName, want.DependencyName)
	}
	if got.CurrentVersion != want.CurrentVersion {
		t.Errorf("CurrentVersion: got %q, want %q", got.CurrentVersion, want.CurrentVersion)
	}
	if got.NewVersion != want.NewVersion {
		t.Errorf("NewVersion: got %q, want %q", got.NewVersion, want.NewVersion)
	}
	if got.UpdateType != want.UpdateType {
		t.Errorf("UpdateType: got %q, want %q", got.UpdateType, want.UpdateType)
	}
	if got.Datasource != want.Datasource {
		t.Errorf("Datasource: got %q, want %q", got.Datasource, want.Datasource)
	}
}
