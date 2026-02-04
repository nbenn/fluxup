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

package git

import (
	"fmt"
	"strings"
	"testing"
)

func TestFormatCommitMessage(t *testing.T) {
	tests := []struct {
		name        string
		appName     string
		fromVersion string
		toVersion   string
		snapshots   []string
		wantPrefix  string
		wantContain []string
	}{
		{
			name:        "basic upgrade",
			appName:     "gitea",
			fromVersion: "10.1.4",
			toVersion:   "11.0.0",
			snapshots:   nil,
			wantPrefix:  "chore(deps): update gitea to 11.0.0",
			wantContain: []string{
				"Upgrade triggered via FluxUp",
				"Managed-By: fluxup",
				"Previous-Version: 10.1.4",
			},
		},
		{
			name:        "upgrade with snapshots",
			appName:     "myapp",
			fromVersion: "1.0.0",
			toVersion:   "2.0.0",
			snapshots:   []string{"myapp-data-pre-upgrade-123", "myapp-db-pre-upgrade-123"},
			wantPrefix:  "chore(deps): update myapp to 2.0.0",
			wantContain: []string{
				"Pre-upgrade snapshots:",
				"- myapp-data-pre-upgrade-123",
				"- myapp-db-pre-upgrade-123",
			},
		},
		{
			name:        "no previous version",
			appName:     "newapp",
			fromVersion: "",
			toVersion:   "1.0.0",
			snapshots:   nil,
			wantPrefix:  "chore(deps): update newapp to 1.0.0",
			wantContain: []string{
				"Managed-By: fluxup",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatCommitMessage(tt.appName, tt.fromVersion, tt.toVersion, tt.snapshots)

			if !strings.HasPrefix(result, tt.wantPrefix) {
				t.Errorf("expected message to start with %q, got:\n%s", tt.wantPrefix, result)
			}

			for _, want := range tt.wantContain {
				if !strings.Contains(result, want) {
					t.Errorf("expected message to contain %q, got:\n%s", want, result)
				}
			}

			// Verify no previous version line if fromVersion is empty
			if tt.fromVersion == "" && strings.Contains(result, "Previous-Version:") {
				t.Errorf("expected no Previous-Version for empty fromVersion, got:\n%s", result)
			}
		})
	}
}

func TestFormatRevertCommitMessage(t *testing.T) {
	tests := []struct {
		name               string
		appName            string
		fromVersion        string
		toVersion          string
		upgradeRequestName string
		wantPrefix         string
		wantContain        []string
	}{
		{
			name:               "basic rollback",
			appName:            "gitea",
			fromVersion:        "11.0.0",
			toVersion:          "10.1.4",
			upgradeRequestName: "gitea-upgrade-123",
			wantPrefix:         "chore(rollback): revert gitea to 10.1.4",
			wantContain: []string{
				"Rollback triggered via FluxUp",
				"Rolling back from: 11.0.0",
				"Original upgrade: gitea-upgrade-123",
				"Managed-By: fluxup",
			},
		},
		{
			name:               "rollback with complex version",
			appName:            "myapp",
			fromVersion:        "v2.0.0-beta.1",
			toVersion:          "v1.9.0",
			upgradeRequestName: "myapp-upgrade-failed",
			wantPrefix:         "chore(rollback): revert myapp to v1.9.0",
			wantContain: []string{
				"Rollback triggered via FluxUp",
				"Rolling back from: v2.0.0-beta.1",
				"Original upgrade: myapp-upgrade-failed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatRevertCommitMessage(tt.appName, tt.fromVersion, tt.toVersion, tt.upgradeRequestName)

			if !strings.HasPrefix(result, tt.wantPrefix) {
				t.Errorf("expected message to start with %q, got:\n%s", tt.wantPrefix, result)
			}

			for _, want := range tt.wantContain {
				if !strings.Contains(result, want) {
					t.Errorf("expected message to contain %q, got:\n%s", want, result)
				}
			}
		})
	}
}

func TestFormatCommitMessage_ConventionalCommits(t *testing.T) {
	msg := FormatCommitMessage("test-app", "1.0.0", "2.0.0", nil)

	// Verify conventional commits format: type(scope): subject
	lines := strings.Split(msg, "\n")
	firstLine := lines[0]

	if !strings.HasPrefix(firstLine, "chore(deps): ") {
		t.Errorf("expected conventional commit format 'chore(deps): ...', got %q", firstLine)
	}

	// Verify first line is concise (< 72 chars is conventional)
	if len(firstLine) > 72 {
		t.Errorf("first line should be <= 72 chars, got %d: %q", len(firstLine), firstLine)
	}

	// Verify blank line after subject
	if len(lines) > 1 && lines[1] != "" {
		t.Errorf("expected blank line after subject, got %q", lines[1])
	}
}

func TestFormatRevertCommitMessage_ConventionalCommits(t *testing.T) {
	msg := FormatRevertCommitMessage("test-app", "2.0.0", "1.0.0", "test-upgrade")

	lines := strings.Split(msg, "\n")
	firstLine := lines[0]

	if !strings.HasPrefix(firstLine, "chore(rollback): ") {
		t.Errorf("expected conventional commit format 'chore(rollback): ...', got %q", firstLine)
	}

	// Verify first line is concise
	if len(firstLine) > 72 {
		t.Errorf("first line should be <= 72 chars, got %d: %q", len(firstLine), firstLine)
	}

	// Verify blank line after subject
	if len(lines) > 1 && lines[1] != "" {
		t.Errorf("expected blank line after subject, got %q", lines[1])
	}
}

func TestFormatCommitMessage_SpecialCharacters(t *testing.T) {
	tests := []struct {
		name      string
		appName   string
		version   string
		wantValid bool
	}{
		{
			name:      "app name with hyphens",
			appName:   "my-app-name",
			version:   "1.0.0",
			wantValid: true,
		},
		{
			name:      "version with build metadata",
			appName:   "app",
			version:   "1.0.0+build.123",
			wantValid: true,
		},
		{
			name:      "version with pre-release",
			appName:   "app",
			version:   "1.0.0-alpha.1",
			wantValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := FormatCommitMessage(tt.appName, "0.0.0", tt.version, nil)

			if tt.wantValid {
				if !strings.Contains(msg, tt.appName) {
					t.Errorf("expected message to contain app name %q", tt.appName)
				}
				if !strings.Contains(msg, tt.version) {
					t.Errorf("expected message to contain version %q", tt.version)
				}
			}
		})
	}
}

func TestFormatCommitMessage_EmptySnapshots(t *testing.T) {
	msg := FormatCommitMessage("app", "1.0.0", "2.0.0", []string{})

	// Empty snapshots list should not include snapshot section
	if strings.Contains(msg, "Pre-upgrade snapshots:") {
		t.Error("expected no snapshot section for empty snapshots list")
	}
}

func TestFormatCommitMessage_MultipleSnapshots(t *testing.T) {
	snapshots := []string{
		"app-data-1-snapshot",
		"app-data-2-snapshot",
		"app-data-3-snapshot",
	}

	msg := FormatCommitMessage("app", "1.0.0", "2.0.0", snapshots)

	// Verify all snapshots are listed
	for _, snap := range snapshots {
		expected := fmt.Sprintf("- %s", snap)
		if !strings.Contains(msg, expected) {
			t.Errorf("expected message to contain %q", expected)
		}
	}

	// Verify they're in the same order
	idx1 := strings.Index(msg, snapshots[0])
	idx2 := strings.Index(msg, snapshots[1])
	idx3 := strings.Index(msg, snapshots[2])

	if idx1 == -1 || idx2 == -1 || idx3 == -1 {
		t.Error("snapshots not found in message")
	}

	if idx1 >= idx2 || idx2 >= idx3 {
		t.Error("snapshots are not in the correct order")
	}
}
