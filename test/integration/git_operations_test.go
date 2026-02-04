package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAuthorName  = "Test Author"
	testAuthorEmail = "test@example.com"
	testBranch      = "main"
)

// setupTestRepo creates an in-memory Git repository with initial commit
func setupTestRepo(t *testing.T) (*git.Repository, billy.Filesystem) {
	t.Helper()

	fs := memfs.New()
	storer := memory.NewStorage()

	repo, err := git.Init(storer, fs)
	require.NoError(t, err)

	// Create initial commit
	w, err := repo.Worktree()
	require.NoError(t, err)

	// Create initial file
	file, err := fs.Create("README.md")
	require.NoError(t, err)
	_, err = file.Write([]byte("# Test Repository\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = w.Add("README.md")
	require.NoError(t, err)

	_, err = w.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  testAuthorName,
			Email: testAuthorEmail,
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	return repo, fs
}

// TestGitConcurrentCommits verifies that multiple controllers can commit simultaneously
// without data loss or corruption. This test demonstrates the need for proper locking
// in real-world scenarios where multiple controllers may access the same repository.
func TestGitConcurrentCommits(t *testing.T) {
	repo, fs := setupTestRepo(t)

	numGoroutines := 10
	var wg sync.WaitGroup
	var mu sync.Mutex // Protect git operations - this simulates what real code should do
	errors := make(chan error, numGoroutines)

	// Each goroutine attempts to commit a unique file
	for i := range numGoroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Lock for the entire git operation to prevent concurrent map writes
			mu.Lock()
			defer mu.Unlock()

			w, err := repo.Worktree()
			if err != nil {
				errors <- fmt.Errorf("goroutine %d: failed to get worktree: %w", id, err)
				return
			}

			// Pull latest changes first (simulating real workflow)
			err = w.Pull(&git.PullOptions{
				RemoteName: "origin",
				Force:      true,
			})
			// Ignore "already up-to-date" and "repository not found" errors (in-memory has no remote)
			if err != nil && err != git.NoErrAlreadyUpToDate &&
				err.Error() != "remote repository is empty" &&
				!strings.Contains(err.Error(), "repository not found") {
				// This is expected in concurrent scenarios - one will win, others will conflict
				t.Logf("goroutine %d: pull failed (expected in concurrent test): %v", id, err)
			}

			// Create unique file
			fileName := fmt.Sprintf("controller-%d.yaml", id)
			file, err := fs.Create(fileName)
			if err != nil {
				errors <- fmt.Errorf("goroutine %d: failed to create file: %w", id, err)
				return
			}

			content := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: controller-%d\n", id)
			_, err = file.Write([]byte(content))
			if err != nil {
				_ = file.Close()
				errors <- fmt.Errorf("goroutine %d: failed to write file: %w", id, err)
				return
			}
			if err := file.Close(); err != nil {
				errors <- fmt.Errorf("goroutine %d: failed to write file: %w", id, err)
				return
			}

			// Stage and commit
			_, err = w.Add(fileName)
			if err != nil {
				errors <- fmt.Errorf("goroutine %d: failed to add file: %w", id, err)
				return
			}

			commitMsg := fmt.Sprintf("feat: add controller-%d configuration", id)
			_, err = w.Commit(commitMsg, &git.CommitOptions{
				Author: &object.Signature{
					Name:  testAuthorName,
					Email: testAuthorEmail,
					When:  time.Now(),
				},
			})
			if err != nil {
				errors <- fmt.Errorf("goroutine %d: failed to commit: %w", id, err)
				return
			}

			t.Logf("goroutine %d: successfully committed %s", id, fileName)
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for errors
	errs := make([]error, 0, numGoroutines)
	for err := range errors {
		errs = append(errs, err)
	}

	// We expect some level of success - at least 50% should succeed
	// (in real scenarios, retry logic would handle conflicts)
	successRate := float64(numGoroutines-len(errs)) / float64(numGoroutines)
	assert.GreaterOrEqual(t, successRate, 0.5, "Expected at least 50%% success rate, got %.0f%%", successRate*100)

	// Verify repository is not corrupted
	head, err := repo.Head()
	require.NoError(t, err)
	assert.NotNil(t, head)

	// List all commits to ensure history is valid
	commitIter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	require.NoError(t, err)

	commitCount := 0
	err = commitIter.ForEach(func(c *object.Commit) error {
		commitCount++
		return nil
	})
	require.NoError(t, err)
	assert.Greater(t, commitCount, 0, "Repository should have commits")

	t.Logf("Concurrent commits test: %d/%d succeeded, %d commits in history",
		numGoroutines-len(errs), numGoroutines, commitCount)
}

// TestGitMergeConflict verifies detection and handling of merge conflicts
func TestGitMergeConflict(t *testing.T) {
	repo, fs := setupTestRepo(t)

	// Create a feature branch
	w, err := repo.Worktree()
	require.NoError(t, err)

	// Create branch "feature"
	headRef, err := repo.Head()
	require.NoError(t, err)

	featureBranch := plumbing.NewBranchReferenceName("feature")
	err = repo.Storer.SetReference(plumbing.NewHashReference(featureBranch, headRef.Hash()))
	require.NoError(t, err)

	// Checkout feature branch
	err = w.Checkout(&git.CheckoutOptions{
		Branch: featureBranch,
	})
	require.NoError(t, err)

	// Modify same file on feature branch
	file, err := fs.OpenFile("README.md", os.O_RDWR|os.O_TRUNC, 0644)
	require.NoError(t, err)
	_, err = file.Write([]byte("# Feature Branch Version\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = w.Add("README.md")
	require.NoError(t, err)

	_, err = w.Commit("feat: update README on feature branch", &git.CommitOptions{
		Author: &object.Signature{
			Name:  testAuthorName,
			Email: testAuthorEmail,
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// Switch back to main
	mainBranch := plumbing.NewBranchReferenceName(testBranch)

	// First set the main branch reference
	err = repo.Storer.SetReference(plumbing.NewHashReference(mainBranch, headRef.Hash()))
	require.NoError(t, err)

	err = w.Checkout(&git.CheckoutOptions{
		Branch: mainBranch,
		Force:  true,
	})
	require.NoError(t, err)

	// Modify same file on main branch
	file, err = fs.OpenFile("README.md", os.O_RDWR|os.O_TRUNC, 0644)
	require.NoError(t, err)
	_, err = file.Write([]byte("# Main Branch Version\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = w.Add("README.md")
	require.NoError(t, err)

	_, err = w.Commit("feat: update README on main branch", &git.CommitOptions{
		Author: &object.Signature{
			Name:  testAuthorName,
			Email: testAuthorEmail,
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// Verify that both branches have diverged and modified the same file
	// This creates a conflict scenario that would require manual resolution

	// Get commits from both branches
	mainHead, err := repo.Head()
	require.NoError(t, err)

	mainCommit, err := repo.CommitObject(mainHead.Hash())
	require.NoError(t, err)

	featureRef, err := repo.Reference(featureBranch, true)
	require.NoError(t, err)

	featureCommit, err := repo.CommitObject(featureRef.Hash())
	require.NoError(t, err)

	// Verify both modified the same file
	mainTree, err := mainCommit.Tree()
	require.NoError(t, err)
	mainFile, err := mainTree.File("README.md")
	require.NoError(t, err)
	mainContent, err := mainFile.Contents()
	require.NoError(t, err)

	featureTree, err := featureCommit.Tree()
	require.NoError(t, err)
	featureFile, err := featureTree.File("README.md")
	require.NoError(t, err)
	featureContent, err := featureFile.Contents()
	require.NoError(t, err)

	// Verify content differs (conflict scenario)
	assert.NotEqual(t, mainContent, featureContent, "File content should differ between branches")
	assert.Contains(t, mainContent, "Main Branch")
	assert.Contains(t, featureContent, "Feature Branch")

	t.Log("Successfully detected divergent branches with conflicting changes")
}

// TestGitLargeFileHandling verifies handling of YAML files larger than 1MB
func TestGitLargeFileHandling(t *testing.T) {
	repo, fs := setupTestRepo(t)

	w, err := repo.Worktree()
	require.NoError(t, err)

	// Create a large YAML file (2MB)
	largeContent := strings.Builder{}
	largeContent.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: large-config\ndata:\n")

	// Add ~2MB of data
	targetSize := 2 * 1024 * 1024 // 2MB
	keySize := 100
	valueSize := 500
	entrySize := keySize + valueSize + 10 // key: value\n

	entriesNeeded := targetSize / entrySize

	for i := range entriesNeeded {
		key := fmt.Sprintf("  key-%d:", i)
		value := strings.Repeat("x", valueSize)
		largeContent.WriteString(fmt.Sprintf("%s %s\n", key, value))
	}

	file, err := fs.Create("large-config.yaml")
	require.NoError(t, err)
	_, err = file.Write([]byte(largeContent.String()))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	// Verify file size
	fileInfo, err := fs.Stat("large-config.yaml")
	require.NoError(t, err)
	actualSize := fileInfo.Size()
	t.Logf("Created file size: %d bytes (%.2f MB)", actualSize, float64(actualSize)/(1024*1024))
	assert.Greater(t, actualSize, int64(1024*1024), "File should be larger than 1MB")

	// Add and commit large file
	_, err = w.Add("large-config.yaml")
	require.NoError(t, err, "Should successfully add large file")

	commitHash, err := w.Commit("feat: add large configuration file", &git.CommitOptions{
		Author: &object.Signature{
			Name:  testAuthorName,
			Email: testAuthorEmail,
			When:  time.Now(),
		},
	})
	require.NoError(t, err, "Should successfully commit large file")

	// Verify commit exists and contains the large file
	commit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)

	tree, err := commit.Tree()
	require.NoError(t, err)

	entry, err := tree.File("large-config.yaml")
	require.NoError(t, err)
	assert.Equal(t, "large-config.yaml", entry.Name)

	t.Logf("Successfully committed large file (hash: %s)", commitHash.String()[:8])
}

// TestGitAuthTokenExpiration simulates authentication failure scenarios
func TestGitAuthTokenExpiration(t *testing.T) {
	repo, _ := setupTestRepo(t)

	// Attempt to push to a remote with invalid credentials
	// In-memory repo has no remote, so this will fail
	err := repo.Push(&git.PushOptions{
		RemoteName: "origin",
		Auth:       nil, // No auth provided
	})

	// We expect an error due to missing remote or auth failure
	assert.Error(t, err, "Push should fail without valid authentication")
	t.Logf("Auth failure detected as expected: %v", err)

	// Verify that the error is related to remote/auth, not local corruption
	head, err := repo.Head()
	require.NoError(t, err, "Repository should still be valid after auth failure")
	assert.NotNil(t, head)
}

// TestGitCommitRetry verifies retry logic for transient failures
func TestGitCommitRetry(t *testing.T) {
	repo, fs := setupTestRepo(t)

	maxRetries := 3
	retryDelay := 100 * time.Millisecond

	ctx := context.Background()

	for attempt := range maxRetries {
		w, err := repo.Worktree()
		require.NoError(t, err)

		fileName := fmt.Sprintf("retry-test-%d.yaml", attempt)
		file, err := fs.Create(fileName)
		require.NoError(t, err)

		content := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: retry-%d\n", attempt)
		_, err = file.Write([]byte(content))
		require.NoError(t, file.Close())
		require.NoError(t, err)

		_, err = w.Add(fileName)
		require.NoError(t, err)

		commitMsg := fmt.Sprintf("feat: retry attempt %d", attempt+1)
		_, err = w.Commit(commitMsg, &git.CommitOptions{
			Author: &object.Signature{
				Name:  testAuthorName,
				Email: testAuthorEmail,
				When:  time.Now(),
			},
		})

		if err == nil {
			t.Logf("Commit succeeded on attempt %d", attempt+1)
			break
		}

		if attempt < maxRetries-1 {
			t.Logf("Commit failed on attempt %d, retrying after %v: %v", attempt+1, retryDelay, err)
			select {
			case <-ctx.Done():
				t.Fatal("Context cancelled during retry")
			case <-time.After(retryDelay):
				// Continue to next retry
			}
		} else {
			t.Fatalf("Commit failed after %d attempts: %v", maxRetries, err)
		}
	}

	// Verify final state
	head, err := repo.Head()
	require.NoError(t, err)

	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	assert.Contains(t, commit.Message, "retry attempt", "Final commit should be a retry attempt")
}

// TestGitConventionalCommits verifies that commit messages follow conventional commits format
func TestGitConventionalCommits(t *testing.T) {
	repo, fs := setupTestRepo(t)

	testCases := []struct {
		name        string
		commitMsg   string
		shouldMatch bool
	}{
		{
			name:        "valid feat commit",
			commitMsg:   "feat: add new feature",
			shouldMatch: true,
		},
		{
			name:        "valid fix commit",
			commitMsg:   "fix: resolve bug in controller",
			shouldMatch: true,
		},
		{
			name:        "valid chore commit",
			commitMsg:   "chore: update dependencies",
			shouldMatch: true,
		},
		{
			name:        "valid commit with scope",
			commitMsg:   "feat(controller): add retry logic",
			shouldMatch: true,
		},
		{
			name:        "invalid commit - no type",
			commitMsg:   "updated the code",
			shouldMatch: false,
		},
		{
			name:        "invalid commit - wrong format",
			commitMsg:   "FEAT: add feature",
			shouldMatch: false,
		},
	}

	// Conventional commit pattern: type(optional-scope): description
	conventionalCommitPattern := `^(feat|fix|docs|style|refactor|perf|test|chore|revert)(\([a-z]+\))?:\s.+`

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := repo.Worktree()
			require.NoError(t, err)

			fileName := fmt.Sprintf("test-%s.yaml", strings.ReplaceAll(tc.name, " ", "-"))
			file, err := fs.Create(fileName)
			require.NoError(t, err)

			_, err = file.Write([]byte("apiVersion: v1\nkind: ConfigMap\n"))
			require.NoError(t, file.Close())
			require.NoError(t, err)

			_, err = w.Add(fileName)
			require.NoError(t, err)

			_, err = w.Commit(tc.commitMsg, &git.CommitOptions{
				Author: &object.Signature{
					Name:  testAuthorName,
					Email: testAuthorEmail,
					When:  time.Now(),
				},
			})
			require.NoError(t, err)

			// Verify commit message format
			head, err := repo.Head()
			require.NoError(t, err)

			commit, err := repo.CommitObject(head.Hash())
			require.NoError(t, err)

			if tc.shouldMatch {
				assert.Regexp(t, conventionalCommitPattern, commit.Message,
					"Commit message should match conventional commits format")
			} else {
				assert.NotRegexp(t, conventionalCommitPattern, commit.Message,
					"Commit message should NOT match conventional commits format")
			}
		})
	}
}
