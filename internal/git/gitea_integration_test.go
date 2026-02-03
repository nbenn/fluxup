//go:build integration

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
	"os"
	"testing"
	"time"
)

// Integration tests for Gitea manager.
// These require a running Gitea instance with:
// - GITEA_URL: Base URL (e.g., http://localhost:3000)
// - GITEA_TOKEN: API token with repo access
// - GITEA_OWNER: Repository owner
// - GITEA_REPO: Repository name (will be created if missing)

func getTestConfig(t *testing.T) Config {
	url := os.Getenv("GITEA_URL")
	token := os.Getenv("GITEA_TOKEN")
	owner := os.Getenv("GITEA_OWNER")
	repo := os.Getenv("GITEA_REPO")

	if url == "" || token == "" || owner == "" || repo == "" {
		t.Skip("GITEA_URL, GITEA_TOKEN, GITEA_OWNER, and GITEA_REPO must be set")
	}

	return Config{
		RepoURL: fmt.Sprintf("%s/%s/%s", url, owner, repo),
		Token:   token,
		Branch:  "main",
	}
}

func TestGiteaManager_Integration_ReadWriteCommit(t *testing.T) {
	cfg := getTestConfig(t)
	ctx := context.Background()

	manager, err := NewGiteaManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create Gitea manager: %v", err)
	}

	// Generate unique test path
	testPath := fmt.Sprintf("test/integration_%d.yaml", time.Now().UnixNano())
	testContent := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  version: "1.0.0"
`)

	// Commit a new file
	t.Run("commit new file", func(t *testing.T) {
		commit, err := manager.CommitFile(ctx, FileChange{
			Path:    testPath,
			Content: testContent,
		}, "test: add integration test file")

		if err != nil {
			t.Fatalf("Failed to commit file: %v", err)
		}

		if commit.SHA == "" {
			t.Error("Expected commit SHA to be set")
		}
		t.Logf("Created commit: %s", commit.SHA)
	})

	// Read the file back
	t.Run("read committed file", func(t *testing.T) {
		content, err := manager.ReadFile(ctx, testPath)
		if err != nil {
			t.Fatalf("Failed to read file: %v", err)
		}

		if string(content) != string(testContent) {
			t.Errorf("Content mismatch:\nExpected: %s\nGot: %s", testContent, content)
		}
	})

	// Update the file
	updatedContent := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  version: "2.0.0"
`)

	t.Run("update existing file", func(t *testing.T) {
		commit, err := manager.CommitFile(ctx, FileChange{
			Path:    testPath,
			Content: updatedContent,
		}, "test: update integration test file")

		if err != nil {
			t.Fatalf("Failed to update file: %v", err)
		}

		if commit.SHA == "" {
			t.Error("Expected commit SHA to be set")
		}
		t.Logf("Updated commit: %s", commit.SHA)
	})

	// Read updated content
	t.Run("read updated file", func(t *testing.T) {
		content, err := manager.ReadFile(ctx, testPath)
		if err != nil {
			t.Fatalf("Failed to read file: %v", err)
		}

		if string(content) != string(updatedContent) {
			t.Errorf("Content mismatch:\nExpected: %s\nGot: %s", updatedContent, content)
		}
	})

	// Get latest commit
	t.Run("get latest commit", func(t *testing.T) {
		commit, err := manager.GetLatestCommit(ctx)
		if err != nil {
			t.Fatalf("Failed to get latest commit: %v", err)
		}

		if commit.SHA == "" {
			t.Error("Expected commit SHA to be set")
		}
		t.Logf("Latest commit: %s - %s", commit.SHA, commit.Message)
	})
}

func TestGiteaManager_Integration_ReadNonexistent(t *testing.T) {
	cfg := getTestConfig(t)
	ctx := context.Background()

	manager, err := NewGiteaManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create Gitea manager: %v", err)
	}

	_, err = manager.ReadFile(ctx, "nonexistent/path/file.yaml")
	if err == nil {
		t.Error("Expected error reading nonexistent file")
	}
}

func TestGiteaManager_Integration_CommitMultipleFiles(t *testing.T) {
	cfg := getTestConfig(t)
	ctx := context.Background()

	manager, err := NewGiteaManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create Gitea manager: %v", err)
	}

	timestamp := time.Now().UnixNano()
	changes := []FileChange{
		{
			Path:    fmt.Sprintf("test/multi1_%d.yaml", timestamp),
			Content: []byte("file: one\n"),
		},
		{
			Path:    fmt.Sprintf("test/multi2_%d.yaml", timestamp),
			Content: []byte("file: two\n"),
		},
	}

	commit, err := manager.CommitFiles(ctx, changes, "test: commit multiple files")
	if err != nil {
		t.Fatalf("Failed to commit files: %v", err)
	}

	if commit.SHA == "" {
		t.Error("Expected commit SHA to be set")
	}

	// Verify both files exist
	for _, change := range changes {
		content, err := manager.ReadFile(ctx, change.Path)
		if err != nil {
			t.Errorf("Failed to read %s: %v", change.Path, err)
			continue
		}
		if string(content) != string(change.Content) {
			t.Errorf("Content mismatch for %s", change.Path)
		}
	}
}
