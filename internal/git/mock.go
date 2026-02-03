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
	"sync"
)

// MockManager is a mock implementation of Manager for testing
type MockManager struct {
	mu sync.RWMutex

	// Files stores the current state of files in the mock repo
	Files map[string][]byte

	// Commits stores the commit history
	Commits []*CommitInfo

	// Error injection
	ReadFileErr        error
	ReadFileErrors     map[string]error // Per-file read errors
	CommitFileErr      error
	CommitFilesErr     error
	GetLatestCommitErr error

	// Call tracking
	ReadFileCalls   []string
	CommitFileCalls []FileChange
}

// NewMockManager creates a new mock Git manager
func NewMockManager() *MockManager {
	return &MockManager{
		Files:          make(map[string][]byte),
		Commits:        make([]*CommitInfo, 0),
		ReadFileErrors: make(map[string]error),
	}
}

// ReadFile reads a file from the mock repository
func (m *MockManager) ReadFile(ctx context.Context, path string) ([]byte, error) {
	m.mu.Lock()
	m.ReadFileCalls = append(m.ReadFileCalls, path)
	m.mu.Unlock()

	if m.ReadFileErr != nil {
		return nil, m.ReadFileErr
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Check for per-file error
	if err, ok := m.ReadFileErrors[path]; ok {
		return nil, err
	}

	content, ok := m.Files[path]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	return content, nil
}

// CommitFile commits a single file change
func (m *MockManager) CommitFile(ctx context.Context, change FileChange, message string) (*CommitInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.CommitFileCalls = append(m.CommitFileCalls, change)

	if m.CommitFileErr != nil {
		return nil, m.CommitFileErr
	}

	// Update file content
	m.Files[change.Path] = change.Content

	// Create commit
	commit := &CommitInfo{
		SHA:     fmt.Sprintf("mock-sha-%d", len(m.Commits)+1),
		Message: message,
		Author:  "mock-author",
		URL:     fmt.Sprintf("https://mock.git/commit/mock-sha-%d", len(m.Commits)+1),
	}
	m.Commits = append(m.Commits, commit)

	return commit, nil
}

// CommitFiles commits multiple file changes
func (m *MockManager) CommitFiles(ctx context.Context, changes []FileChange, message string) (*CommitInfo, error) {
	if m.CommitFilesErr != nil {
		return nil, m.CommitFilesErr
	}

	var lastCommit *CommitInfo
	for _, change := range changes {
		commit, err := m.CommitFile(ctx, change, message)
		if err != nil {
			return nil, err
		}
		lastCommit = commit
	}
	return lastCommit, nil
}

// GetLatestCommit returns the latest commit
func (m *MockManager) GetLatestCommit(ctx context.Context) (*CommitInfo, error) {
	if m.GetLatestCommitErr != nil {
		return nil, m.GetLatestCommitErr
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.Commits) == 0 {
		return &CommitInfo{
			SHA:     "initial-mock-sha",
			Message: "Initial commit",
			Author:  "mock-author",
		}, nil
	}

	return m.Commits[len(m.Commits)-1], nil
}

// SetFile sets a file in the mock repository
func (m *MockManager) SetFile(path string, content []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Files[path] = content
}

// SetReadError sets an error to be returned when reading a specific file
func (m *MockManager) SetReadError(path string, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReadFileErrors[path] = fmt.Errorf("%s", errMsg)
}

// Reset clears all state
func (m *MockManager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Files = make(map[string][]byte)
	m.Commits = make([]*CommitInfo, 0)
	m.ReadFileCalls = nil
	m.CommitFileCalls = nil
	m.ReadFileErr = nil
	m.ReadFileErrors = make(map[string]error)
	m.CommitFileErr = nil
	m.CommitFilesErr = nil
	m.GetLatestCommitErr = nil
}
