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
