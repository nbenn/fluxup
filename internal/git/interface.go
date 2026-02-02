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
	"context"
	"fmt"
)

// FileChange represents a file modification
type FileChange struct {
	Path    string // File path in repository
	Content []byte // New file content
}

// CommitInfo contains commit metadata
type CommitInfo struct {
	SHA     string
	Message string
	Author  string
	URL     string
}

// Manager abstracts Git operations
type Manager interface {
	// ReadFile reads a file from the repository
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// CommitFile commits a single file change
	CommitFile(ctx context.Context, change FileChange, message string) (*CommitInfo, error)

	// CommitFiles commits multiple file changes (may not be atomic depending on backend)
	CommitFiles(ctx context.Context, changes []FileChange, message string) (*CommitInfo, error)

	// GetLatestCommit returns the latest commit on the branch
	GetLatestCommit(ctx context.Context) (*CommitInfo, error)
}

// Config holds Git backend configuration
type Config struct {
	// Backend type: gitea, github, gitlab
	Backend string

	// Repository URL (https://gitea.example.com/org/repo)
	RepoURL string

	// Branch to operate on
	Branch string

	// Authentication token
	Token string
}

// NewManager creates a Git manager based on configuration
func NewManager(cfg Config) (Manager, error) {
	switch cfg.Backend {
	case "gitea":
		return NewGiteaManager(cfg)
	// case "github":
	//     return NewGitHubManager(cfg)
	// case "gitlab":
	//     return NewGitLabManager(cfg)
	default:
		return nil, fmt.Errorf("unsupported git backend: %s", cfg.Backend)
	}
}
