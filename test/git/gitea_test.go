//go:build git

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

package git_test

import (
	"context"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nbenn/fluxup/internal/git"
)

var _ = Describe("Gitea Manager", Ordered, func() {
	var (
		manager git.Manager
		ctx     context.Context
	)

	BeforeAll(func() {
		url := os.Getenv("GITEA_URL")
		token := os.Getenv("GITEA_TOKEN")
		owner := os.Getenv("GITEA_OWNER")
		repo := os.Getenv("GITEA_REPO")

		if url == "" || token == "" || owner == "" || repo == "" {
			Skip("GITEA_URL, GITEA_TOKEN, GITEA_OWNER, and GITEA_REPO must be set")
		}

		cfg := git.Config{
			RepoURL: fmt.Sprintf("%s/%s/%s", url, owner, repo),
			Token:   token,
			Branch:  "main",
		}

		var err error
		manager, err = git.NewGiteaManager(cfg)
		Expect(err).NotTo(HaveOccurred())

		ctx = context.Background()
	})

	Describe("File Operations", func() {
		var testPath string
		testContent := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  version: "1.0.0"
`)

		BeforeEach(func() {
			testPath = fmt.Sprintf("test/integration_%d.yaml", time.Now().UnixNano())
		})

		It("should commit a new file", func() {
			commit, err := manager.CommitFile(ctx, git.FileChange{
				Path:    testPath,
				Content: testContent,
			}, "test: add integration test file")

			Expect(err).NotTo(HaveOccurred())
			Expect(commit.SHA).NotTo(BeEmpty())
		})

		It("should read a committed file", func() {
			// First commit
			_, err := manager.CommitFile(ctx, git.FileChange{
				Path:    testPath,
				Content: testContent,
			}, "test: add file for read test")
			Expect(err).NotTo(HaveOccurred())

			// Then read
			content, err := manager.ReadFile(ctx, testPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal(string(testContent)))
		})

		It("should update an existing file", func() {
			// Create initial file
			_, err := manager.CommitFile(ctx, git.FileChange{
				Path:    testPath,
				Content: testContent,
			}, "test: create file")
			Expect(err).NotTo(HaveOccurred())

			// Update it
			updatedContent := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  version: "2.0.0"
`)
			commit, err := manager.CommitFile(ctx, git.FileChange{
				Path:    testPath,
				Content: updatedContent,
			}, "test: update file")
			Expect(err).NotTo(HaveOccurred())
			Expect(commit.SHA).NotTo(BeEmpty())

			// Verify update
			content, err := manager.ReadFile(ctx, testPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal(string(updatedContent)))
		})

		It("should fail to read a nonexistent file", func() {
			_, err := manager.ReadFile(ctx, "nonexistent/path/file.yaml")
			Expect(err).To(HaveOccurred())
		})

		It("should commit multiple files atomically", func() {
			timestamp := time.Now().UnixNano()
			changes := []git.FileChange{
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
			Expect(err).NotTo(HaveOccurred())
			Expect(commit.SHA).NotTo(BeEmpty())

			// Verify both files
			for _, change := range changes {
				content, err := manager.ReadFile(ctx, change.Path)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(content)).To(Equal(string(change.Content)))
			}
		})
	})

	Describe("Commit Operations", func() {
		It("should get the latest commit", func() {
			commit, err := manager.GetLatestCommit(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(commit.SHA).NotTo(BeEmpty())
		})
	})
})
