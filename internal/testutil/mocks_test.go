package testutil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// --- MemFS tests ---

func TestMemFS_ReadFile(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(m *MemFS)
		path    string
		want    string
		wantErr bool
	}{
		{
			name: "existing file",
			setup: func(m *MemFS) {
				_ = m.WriteFile("/foo/bar.txt", []byte("hello"), 0644)
			},
			path: "/foo/bar.txt",
			want: "hello",
		},
		{
			name:    "non-existent file",
			setup:   func(m *MemFS) {},
			path:    "/does/not/exist.txt",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewMemFS()
			tt.setup(m)

			data, err := m.ReadFile(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ReadFile(%q) expected error, got nil", tt.path)
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("ReadFile(%q) error = %v, want os.ErrNotExist", tt.path, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFile(%q) unexpected error: %v", tt.path, err)
			}
			if string(data) != tt.want {
				t.Errorf("ReadFile(%q) = %q, want %q", tt.path, data, tt.want)
			}
		})
	}
}

func TestMemFS_WriteFile_CreatesParentDirs(t *testing.T) {
	m := NewMemFS()
	path := "/a/b/c/file.txt"
	content := []byte("nested content")

	if err := m.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// file should exist
	got, err := m.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after WriteFile: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}

	// parent dirs should be created
	parents := []string{"/a", "/a/b", "/a/b/c"}
	for _, p := range parents {
		if !m.Dirs[p] {
			t.Errorf("expected dir %q to be created", p)
		}
	}
}

func TestMemFS_WriteFile_Overwrite(t *testing.T) {
	m := NewMemFS()
	path := "/overwrite.txt"

	if err := m.WriteFile(path, []byte("first"), 0644); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}
	if err := m.WriteFile(path, []byte("second"), 0644); err != nil {
		t.Fatalf("second WriteFile: %v", err)
	}

	got, err := m.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("got %q, want %q", got, "second")
	}
}

func TestMemFS_WalkDir(t *testing.T) {
	m := NewMemFS()
	files := map[string]string{
		"/root/z.txt":     "z",
		"/root/a.txt":     "a",
		"/root/sub/b.txt": "b",
	}
	for path, content := range files {
		if err := m.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("WriteFile %q: %v", path, err)
		}
	}

	var visited []string
	err := m.WalkDir("/root", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("/root", path)
		visited = append(visited, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	// paths must be sorted; root itself appears as "."
	want := []string{".", "a.txt", "sub", filepath.Join("sub", "b.txt"), "z.txt"}
	if len(visited) != len(want) {
		t.Fatalf("WalkDir visited %v, want %v", visited, want)
	}
	for i, w := range want {
		if visited[i] != w {
			t.Errorf("visited[%d] = %q, want %q", i, visited[i], w)
		}
	}
}

func TestMemFS_Rename(t *testing.T) {
	m := NewMemFS()
	oldPath := "/old.txt"
	newPath := "/new.txt"
	content := []byte("rename me")

	if err := m.WriteFile(oldPath, content, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := m.Rename(oldPath, newPath); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// old key should be gone
	if _, ok := m.Files[oldPath]; ok {
		t.Errorf("old path %q still exists after Rename", oldPath)
	}

	// new key should exist with original content
	got, err := m.ReadFile(newPath)
	if err != nil {
		t.Fatalf("ReadFile new path: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("new path content = %q, want %q", got, content)
	}
}

// TestMemFS_Rename_DirectoryWithContents demonstrates the known MemFS limitation (I4):
// renaming a directory moves only the directory node itself, NOT its children.
// The pipeline works around this via renameDir() which does a recursive walk+copy+remove.
func TestMemFS_Rename_DirectoryWithContents(t *testing.T) {
	m := NewMemFS()

	// Create a directory with files and subdirectories.
	if err := m.WriteFile("/src/a.txt", []byte("aaa"), 0644); err != nil {
		t.Fatalf("WriteFile /src/a.txt: %v", err)
	}
	if err := m.WriteFile("/src/sub/b.txt", []byte("bbb"), 0644); err != nil {
		t.Fatalf("WriteFile /src/sub/b.txt: %v", err)
	}

	// Rename the directory.
	if err := m.Rename("/src", "/dst"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// The directory node itself should have moved.
	if m.Dirs["/src"] {
		t.Errorf("/src dir node still exists after Rename")
	}
	if !m.Dirs["/dst"] {
		t.Errorf("/dst dir node not created by Rename")
	}

	// Children are NOT moved — they remain under the old path.
	// This is the known limitation documented on MemFS.Rename.
	if _, err := m.ReadFile("/dst/a.txt"); err == nil {
		t.Errorf("/dst/a.txt should NOT exist (children are not moved by MemFS.Rename)")
	}
	if _, err := m.ReadFile("/src/a.txt"); err != nil {
		t.Errorf("/src/a.txt should still exist under old prefix: %v", err)
	}
	if _, err := m.ReadFile("/src/sub/b.txt"); err != nil {
		t.Errorf("/src/sub/b.txt should still exist under old prefix: %v", err)
	}
}

func TestMemFS_Rename_NonExistent(t *testing.T) {
	m := NewMemFS()
	err := m.Rename("/ghost.txt", "/new.txt")
	if err == nil {
		t.Errorf("Rename on non-existent path expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Rename error = %v, want os.ErrNotExist", err)
	}
}

func TestMemFS_Remove(t *testing.T) {
	m := NewMemFS()
	path := "/to-remove.txt"

	if err := m.WriteFile(path, []byte("bye"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := m.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := m.ReadFile(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still readable after Remove")
	}
}

func TestMemFS_RemoveAll(t *testing.T) {
	m := NewMemFS()
	paths := []string{"/tree/a.txt", "/tree/b/c.txt", "/tree/b/d.txt"}
	for _, p := range paths {
		if err := m.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("WriteFile %q: %v", p, err)
		}
	}

	if err := m.RemoveAll("/tree"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	for _, p := range paths {
		if _, err := m.ReadFile(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("file %q still readable after RemoveAll", p)
		}
	}
	if m.Dirs["/tree"] {
		t.Errorf("dir /tree still exists after RemoveAll")
	}
}

func TestMemFS_CopyFile(t *testing.T) {
	m := NewMemFS()
	src := "/src.txt"
	dst := "/dst.txt"
	content := []byte("copy content")

	if err := m.WriteFile(src, content, 0644); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}
	if err := m.CopyFile(src, dst, 0600); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}

	got, err := m.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("dst = %q, want %q", got, content)
	}

	// mutate dst; src must be unaffected (deep copy)
	m.Files[dst][0] = 'X'
	srcData, _ := m.ReadFile(src)
	if srcData[0] == 'X' {
		t.Errorf("CopyFile shares backing array with src")
	}
}

func TestMemFS_ReadDir(t *testing.T) {
	m := NewMemFS()
	files := []string{"/dir/z.txt", "/dir/a.txt", "/dir/m.txt"}
	for _, p := range files {
		if err := m.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("WriteFile %q: %v", p, err)
		}
	}

	entries, err := m.ReadDir("/dir")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("ReadDir returned %d entries, want 3", len(entries))
	}

	want := []string{"a.txt", "m.txt", "z.txt"}
	for i, e := range entries {
		if e.Name() != want[i] {
			t.Errorf("entries[%d].Name() = %q, want %q", i, e.Name(), want[i])
		}
	}
}

func TestMemFS_Stat_File(t *testing.T) {
	m := NewMemFS()
	content := []byte("stat test")
	path := "/stat.txt"

	if err := m.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info, err := m.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.IsDir() {
		t.Errorf("Stat.IsDir = true for file")
	}
	if info.Size() != int64(len(content)) {
		t.Errorf("Stat.Size = %d, want %d", info.Size(), len(content))
	}
}

func TestMemFS_Stat_NonExistent(t *testing.T) {
	m := NewMemFS()
	_, err := m.Stat("/ghost.txt")
	if err == nil {
		t.Errorf("Stat on non-existent path expected error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat error = %v, want os.ErrNotExist", err)
	}
}

// --- StubGitResolver tests ---

func TestDefaultGitResolver(t *testing.T) {
	g := DefaultGitResolver()
	if g.Remote != TestGitRemote {
		t.Errorf("Remote = %q, want %q", g.Remote, TestGitRemote)
	}
	if g.Email != TestEmail {
		t.Errorf("Email = %q, want %q", g.Email, TestEmail)
	}
	if g.BranchName == "" {
		t.Errorf("BranchName is empty")
	}
	if g.WorktreeDir == "" {
		t.Errorf("WorktreeDir is empty")
	}
}

func TestNoGitResolver(t *testing.T) {
	g := NoGitResolver()
	if g.RemoteErr == nil {
		t.Errorf("NoGitResolver.RemoteErr expected non-nil")
	}
	if g.BranchErr == nil {
		t.Errorf("NoGitResolver.BranchErr expected non-nil")
	}
	if g.WorktreeErr == nil {
		t.Errorf("NoGitResolver.WorktreeErr expected non-nil")
	}
	if g.EmailErr == nil {
		t.Errorf("NoGitResolver.EmailErr expected non-nil")
	}
}

func TestNoRemoteGitResolver(t *testing.T) {
	g := NoRemoteGitResolver()
	if g.RemoteErr == nil {
		t.Errorf("NoRemoteGitResolver.RemoteErr expected non-nil")
	}
	if g.BranchErr != nil {
		t.Errorf("NoRemoteGitResolver.BranchErr = %v, want nil", g.BranchErr)
	}
	if g.WorktreeErr != nil {
		t.Errorf("NoRemoteGitResolver.WorktreeErr = %v, want nil", g.WorktreeErr)
	}
	if g.EmailErr != nil {
		t.Errorf("NoRemoteGitResolver.EmailErr = %v, want nil", g.EmailErr)
	}
	if g.BranchName == "" {
		t.Errorf("NoRemoteGitResolver.BranchName is empty")
	}
	if g.Email == "" {
		t.Errorf("NoRemoteGitResolver.Email is empty")
	}
}

// --- StubAdapter tests ---

func TestStubAdapter_Provider(t *testing.T) {
	a := &StubAdapter{ProviderValue: ingest.HarnessClaudeCode}
	if got := a.Harness(); got != ingest.HarnessClaudeCode {
		t.Errorf("Provider() = %q, want %q", got, ingest.HarnessClaudeCode)
	}
}

func TestStubAdapter_Discover(t *testing.T) {
	sid, err := ingest.NewSessionID(TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	sessions := []ingest.DiscoveredSession{
		{SessionID: sid, Harness: ingest.HarnessClaudeCode},
	}
	a := &StubAdapter{
		ProviderValue: ingest.HarnessClaudeCode,
		Sessions:      sessions,
	}

	got, err := a.Discover(context.Background(), ingest.SourceConfig{})
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(got))
	}
	if got[0].SessionID != sid {
		t.Errorf("Discover()[0].SessionID = %q, want %q", got[0].SessionID, sid)
	}
}

func TestStubAdapter_DiscoverError(t *testing.T) {
	wantErr := errors.New("discover failed")
	a := &StubAdapter{
		ProviderValue: ingest.HarnessClaudeCode,
		DiscoverErr:   wantErr,
	}

	_, err := a.Discover(context.Background(), ingest.SourceConfig{})
	if err == nil {
		t.Fatal("Discover() expected error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Discover() error = %v, want %v", err, wantErr)
	}
}

func TestStubAdapter_ExtractMetadata(t *testing.T) {
	sid, err := ingest.NewSessionID(TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = sid

	a := &StubAdapter{
		ProviderValue: ingest.HarnessClaudeCode,
		Metadata: map[ingest.SessionID]*ingest.UnifiedMetadata{
			sid: &meta,
		},
	}

	session := ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessClaudeCode}
	got, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata() unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("ExtractMetadata() returned nil metadata")
	}
	if got.SessionID != sid {
		t.Errorf("ExtractMetadata().SessionID = %q, want %q", got.SessionID, sid)
	}
}

func TestStubAdapter_ExtractMetadataError(t *testing.T) {
	sid, err := ingest.NewSessionID(TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	wantErr := errors.New("extract failed")
	a := &StubAdapter{
		ProviderValue: ingest.HarnessClaudeCode,
		ExtractErr:    wantErr,
	}

	session := ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessClaudeCode}
	_, err = a.ExtractMetadata(context.Background(), session)
	if err == nil {
		t.Fatal("ExtractMetadata() expected error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("ExtractMetadata() error = %v, want %v", err, wantErr)
	}
}

// --- StubGitRepository tests ---

func TestDefaultGitRepository(t *testing.T) {
	g := DefaultGitRepository()
	ctx := context.Background()

	def, err := g.DefaultBranch(ctx)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if def != TestDefaultBranch {
		t.Errorf("DefaultBranch = %q, want %q", def, TestDefaultBranch)
	}

	branches, err := g.Branches(ctx)
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if len(branches) != 1 || branches[0] != TestFeatureBranch {
		t.Errorf("Branches = %v, want [%s]", branches, TestFeatureBranch)
	}

	state, err := g.BranchState(ctx, TestFeatureBranch)
	if err != nil {
		t.Fatalf("BranchState: %v", err)
	}
	if state.MergeBase != TestBaseCommitHash {
		t.Errorf("MergeBase = %q, want %q", state.MergeBase, TestBaseCommitHash)
	}
	if state.AheadCount != 2 || state.BehindCount != 1 {
		t.Errorf("ahead/behind = %d/%d, want 2/1", state.AheadCount, state.BehindCount)
	}
	var foundRename bool
	for _, fc := range state.ChangedFiles {
		if fc.Status == gitops.FileStatusRenamed {
			foundRename = true
			if fc.OldPath == nil || *fc.OldPath != TestRenameOldPath {
				t.Errorf("rename OldPath = %v, want %q", fc.OldPath, TestRenameOldPath)
			}
		}
	}
	if !foundRename {
		t.Errorf("ChangedFiles missing a rename entry: %+v", state.ChangedFiles)
	}

	merged, err := g.MergedBranches(ctx, 10)
	if err != nil {
		t.Fatalf("MergedBranches: %v", err)
	}
	if len(merged) != 1 || merged[0].MergeCommit != TestMergeCommitHash {
		t.Errorf("MergedBranches = %+v, want one entry with merge commit %s", merged, TestMergeCommitHash)
	}

	hashes, err := g.CommitsInRange(ctx, TestBaseCommitHash, TestFeatureBranch)
	if err != nil {
		t.Fatalf("CommitsInRange: %v", err)
	}
	if len(hashes) != 1 || hashes[0] != TestHeadCommitHash {
		t.Errorf("CommitsInRange = %v, want [%s]", hashes, TestHeadCommitHash)
	}

	stats, err := g.DiffStats(ctx, TestBaseCommitHash, TestFeatureBranch)
	if err != nil {
		t.Fatalf("DiffStats: %v", err)
	}
	// Seed coherence: one row per ChangedFiles entry, totals = column sums.
	if len(stats.PerFile) != len(state.ChangedFiles) {
		t.Errorf("DiffStats PerFile has %d rows, want %d (one per ChangedFiles entry)",
			len(stats.PerFile), len(state.ChangedFiles))
	}
	var sumAdded, sumRemoved int
	statPaths := map[string]bool{}
	for _, fs := range stats.PerFile {
		sumAdded += fs.Added
		sumRemoved += fs.Removed
		statPaths[fs.Path] = true
	}
	if stats.LinesAdded != sumAdded || stats.LinesRemoved != sumRemoved {
		t.Errorf("DiffStats totals = +%d/-%d, want column sums +%d/-%d",
			stats.LinesAdded, stats.LinesRemoved, sumAdded, sumRemoved)
	}
	for _, fc := range state.ChangedFiles {
		if !statPaths[fc.Path] {
			t.Errorf("DiffStats PerFile missing row for changed file %q", fc.Path)
		}
	}
}

func TestStubGitRepository_LookupSemantics(t *testing.T) {
	g := DefaultGitRepository()
	ctx := context.Background()

	// Unseeded branch state errors (mirrors real git unknown-ref behavior).
	if _, err := g.BranchState(ctx, "no/such/branch"); err == nil {
		t.Error("BranchState: expected error for unseeded branch")
	}
	// Unseeded file content errors.
	if _, err := g.FileAtCommit(ctx, TestHeadCommitHash, "nope.txt"); err == nil {
		t.Error("FileAtCommit: expected error for unseeded commit:path")
	}
	// Batch reads return the seeded subset; unseeded paths are absent, not errors.
	batch, err := g.FilesAtCommit(ctx, TestBaseCommitHash, []string{"internal/ingest/pipeline.go", "nope.txt"})
	if err != nil {
		t.Fatalf("FilesAtCommit: %v", err)
	}
	if len(batch) != 1 || string(batch["internal/ingest/pipeline.go"]) != "package ingest\n" {
		t.Errorf("FilesAtCommit = %v, want only the seeded pipeline.go", batch)
	}
	// Unseeded list-style queries return empty, no error.
	files, err := g.ListFiles(ctx, "unseeded-ref")
	if err != nil || len(files) != 0 {
		t.Errorf("ListFiles(unseeded) = %v, %v; want empty, nil", files, err)
	}
	commits, err := g.Commits(ctx, "unseeded-ref", 5)
	if err != nil || len(commits) != 0 {
		t.Errorf("Commits(unseeded) = %v, %v; want empty, nil", commits, err)
	}
	hashes, err := g.CommitsInRange(ctx, "a", "b")
	if err != nil || len(hashes) != 0 {
		t.Errorf("CommitsInRange(unseeded) = %v, %v; want empty, nil", hashes, err)
	}

	// Limits cap seeded results.
	limited, err := g.Commits(ctx, TestFeatureBranch, 1)
	if err != nil {
		t.Fatalf("Commits: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("Commits(limit 1) returned %d, want 1", len(limited))
	}

	// Unseeded diff stats error (returning nil, nil would hide misuse behind
	// nil-pointer panics).
	g.DiffStatsResult = nil
	if _, err := g.DiffStats(ctx, TestBaseCommitHash, TestFeatureBranch); err == nil {
		t.Error("DiffStats: expected error when unseeded")
	}
}

func TestNoGitRepository(t *testing.T) {
	g := NoGitRepository()
	ctx := context.Background()

	if _, err := g.DefaultBranch(ctx); err == nil {
		t.Error("DefaultBranch: expected error")
	}
	if _, err := g.Branches(ctx); err == nil {
		t.Error("Branches: expected error")
	}
	if _, err := g.BranchState(ctx, TestFeatureBranch); err == nil {
		t.Error("BranchState: expected error")
	}
	if _, err := g.MergedBranches(ctx, 1); err == nil {
		t.Error("MergedBranches: expected error")
	}
	if _, err := g.FileAtCommit(ctx, TestBaseCommitHash, "x"); err == nil {
		t.Error("FileAtCommit: expected error")
	}
	if _, err := g.FilesAtCommit(ctx, TestBaseCommitHash, []string{"x"}); err == nil {
		t.Error("FilesAtCommit: expected error")
	}
	if _, err := g.ListFiles(ctx, TestDefaultBranch); err == nil {
		t.Error("ListFiles: expected error")
	}
	if _, err := g.Commits(ctx, TestDefaultBranch, 1); err == nil {
		t.Error("Commits: expected error")
	}
	if _, err := g.CommitsInRange(ctx, "a", "b"); err == nil {
		t.Error("CommitsInRange: expected error")
	}
	if _, err := g.DiffStats(ctx, "a", "b"); err == nil {
		t.Error("DiffStats: expected error")
	}
}
