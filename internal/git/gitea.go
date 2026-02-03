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
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"code.gitea.io/sdk/gitea"

	"github.com/nbenn/fluxup/internal/logging"
)

// GiteaManager implements Manager for Gitea
type GiteaManager struct {
	client *gitea.Client
	owner  string
	repo   string
	branch string
}

// NewGiteaManager creates a new Gitea Git manager
func NewGiteaManager(cfg Config) (*GiteaManager, error) {
	// Parse repo URL to extract owner and repo
	parsed, err := url.Parse(cfg.RepoURL)
	if err != nil {
		return nil, fmt.Errorf("parsing repo URL: %w", err)
	}

	// URL format: https://gitea.example.com/owner/repo
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid repo URL format: %s (expected https://host/owner/repo)", cfg.RepoURL)
	}
	owner, repo := parts[0], parts[1]

	// Remove .git suffix if present
	repo = strings.TrimSuffix(repo, ".git")

	// Create Gitea client
	baseURL := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
	client, err := gitea.NewClient(baseURL, gitea.SetToken(cfg.Token))
	if err != nil {
		return nil, fmt.Errorf("creating gitea client: %w", err)
	}

	branch := cfg.Branch
	if branch == "" {
		branch = "main"
	}

	return &GiteaManager{
		client: client,
		owner:  owner,
		repo:   repo,
		branch: branch,
	}, nil
}

// ReadFile reads a file from the repository
func (g *GiteaManager) ReadFile(ctx context.Context, path string) ([]byte, error) {
	logger := logging.FromContext(ctx)
	logger.Debug("reading file from git", "owner", g.owner, "repo", g.repo, "branch", g.branch, "path", path)

	content, resp, err := g.client.GetContents(g.owner, g.repo, g.branch, path)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			logger.Debug("file not found in git", "path", path)
			return nil, fmt.Errorf("file not found: %s", path)
		}
		return nil, fmt.Errorf("reading file: %w", err)
	}

	if content == nil || content.Content == nil {
		return nil, fmt.Errorf("empty content for file: %s", path)
	}

	// Content is base64 encoded
	decoded, err := base64.StdEncoding.DecodeString(*content.Content)
	if err != nil {
		return nil, fmt.Errorf("decoding content: %w", err)
	}

	logger.Debug("successfully read file from git", "path", path, "size", len(decoded))
	return decoded, nil
}

// CommitFile commits a single file change
func (g *GiteaManager) CommitFile(ctx context.Context, change FileChange, message string) (*CommitInfo, error) {
	logger := logging.FromContext(ctx)
	logger.Debug("committing file to git", "path", change.Path, "branch", g.branch)

	// Get current file SHA for update (required by Gitea API for updates)
	existing, _, _ := g.client.GetContents(g.owner, g.repo, g.branch, change.Path)

	content := base64.StdEncoding.EncodeToString(change.Content)

	// Use CreateFile for new files, UpdateFile for existing ones
	if existing == nil || existing.SHA == "" {
		logger.Debug("creating new file in git", "path", change.Path)
		opts := gitea.CreateFileOptions{
			FileOptions: gitea.FileOptions{
				Message:    message,
				BranchName: g.branch,
			},
			Content: content,
		}
		resp, _, err := g.client.CreateFile(g.owner, g.repo, change.Path, opts)
		if err != nil {
			return nil, fmt.Errorf("creating file: %w", err)
		}
		logger.Info("created file in git", "path", change.Path, "commit", resp.Commit.SHA)
		return &CommitInfo{
			SHA:     resp.Commit.SHA,
			Message: message,
			URL:     resp.Commit.HTMLURL,
		}, nil
	}

	// Update existing file
	logger.Debug("updating existing file in git", "path", change.Path, "existingSHA", existing.SHA)
	opts := gitea.UpdateFileOptions{
		FileOptions: gitea.FileOptions{
			Message:    message,
			BranchName: g.branch,
		},
		Content: content,
		SHA:     existing.SHA,
	}
	resp, _, err := g.client.UpdateFile(g.owner, g.repo, change.Path, opts)
	if err != nil {
		return nil, fmt.Errorf("updating file: %w", err)
	}

	logger.Info("updated file in git", "path", change.Path, "commit", resp.Commit.SHA)
	return &CommitInfo{
		SHA:     resp.Commit.SHA,
		Message: message,
		URL:     resp.Commit.HTMLURL,
	}, nil
}

// CommitFiles commits multiple file changes
// Note: Gitea API doesn't support atomic multi-file commits,
// so files are committed sequentially
func (g *GiteaManager) CommitFiles(ctx context.Context, changes []FileChange, message string) (*CommitInfo, error) {
	logger := logging.FromContext(ctx)
	logger.Debug("committing multiple files to git", "count", len(changes))

	var lastCommit *CommitInfo
	for _, change := range changes {
		commit, err := g.CommitFile(ctx, change, message)
		if err != nil {
			return nil, fmt.Errorf("committing file %s: %w", change.Path, err)
		}
		lastCommit = commit
	}

	logger.Info("committed multiple files to git", "count", len(changes), "lastCommit", lastCommit.SHA)
	return lastCommit, nil
}

// GetLatestCommit returns the latest commit on the branch
func (g *GiteaManager) GetLatestCommit(ctx context.Context) (*CommitInfo, error) {
	logger := logging.FromContext(ctx)
	logger.Debug("getting latest commit", "branch", g.branch)

	commits, _, err := g.client.ListRepoCommits(g.owner, g.repo, gitea.ListCommitOptions{
		SHA: g.branch,
		ListOptions: gitea.ListOptions{
			PageSize: 1,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting latest commit: %w", err)
	}

	if len(commits) == 0 {
		return nil, fmt.Errorf("no commits found on branch %s", g.branch)
	}

	author := ""
	if commits[0].Author != nil {
		author = commits[0].Author.UserName
	}

	logger.Debug("found latest commit", "sha", commits[0].SHA, "author", author)
	return &CommitInfo{
		SHA:     commits[0].SHA,
		Message: commits[0].RepoCommit.Message,
		Author:  author,
		URL:     commits[0].HTMLURL,
	}, nil
}
