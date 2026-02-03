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
)

// FormatCommitMessage creates a standardized commit message following conventional commits
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

	sb.WriteString("\nManaged-By: fluxup\n")
	if fromVersion != "" {
		sb.WriteString(fmt.Sprintf("Previous-Version: %s\n", fromVersion))
	}

	return sb.String()
}

// FormatRevertCommitMessage creates a commit message for rollback operations
func FormatRevertCommitMessage(appName, fromVersion, toVersion, upgradeRequestName string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("chore(rollback): revert %s to %s\n\n", appName, toVersion))
	sb.WriteString("Rollback triggered via FluxUp\n")
	sb.WriteString(fmt.Sprintf("Rolling back from: %s\n", fromVersion))
	sb.WriteString(fmt.Sprintf("Original upgrade: %s\n", upgradeRequestName))
	sb.WriteString("\nManaged-By: fluxup\n")

	return sb.String()
}
