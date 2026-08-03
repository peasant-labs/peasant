package ingest_test

// Phase B integration tests: salt + opaque project hashes
//
// These tests exercise the full pipeline end-to-end (MemFS + StubGitResolver +
// real Pipeline) and verify that:
//   1. Salted hashing — Project.Hash = HMAC-SHA256(salt, normalized_remote)
//   2. SSH/HTTPS dedup — same repo via different remote formats → same ProjectHash
//   3. Readable filesystem dirs — output dirs use human-readable HostSlug
//   4. Different salts — two installations produce different ProjectHash for same repo
//   5. Untracked project — no git remote → slug format __peasant-untracked__--{hash8}--{basename}
//   6. Push gate — salted project hash is always sent while raw identity fields remain opt-in

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// --- Phase B test helpers ---

// makePhaseBSalt constructs a deterministic 32-byte test salt from a seed string.
// The seed is repeated/truncated to fill 32 bytes. Use distinct seeds for distinct salts.
func makePhaseBSalt(seed string) salt.Salt {
	var s salt.Salt
	for i := range s {
		s[i] = seed[i%len(seed)]
	}
	return s
}

// makePhaseBSession constructs a DiscoveredSession with a deterministic modTime.
func makePhaseBSession(t *testing.T, sessionIDStr, sourcePath string) ingest.DiscoveredSession {
	t.Helper()
	sid, err := ingest.NewSessionID(sessionIDStr)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionIDStr, err)
	}
	return ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(sourcePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
		ModTime:       time.Now().Add(-2 * time.Hour),
	}
}

// makePhaseBMeta builds a UnifiedMetadata with project identifiers derived from
// the given salt and git remote (or fallback path when remote is empty).
// The HostSlug and Project.Hash fields are populated using the real
// DeriveProjectIdentifiers function — identical to what the real adapters do.
func makePhaseBMeta(t *testing.T, sessionIDStr string, s salt.Salt, remote, fallbackPath string) *ingest.UnifiedMetadata {
	t.Helper()
	sid, err := ingest.NewSessionID(sessionIDStr)
	if err != nil {
		t.Fatalf("makePhaseBMeta NewSessionID(%q): %v", sessionIDStr, err)
	}

	projectHash, hostSlug, err := ingest.DeriveProjectIdentifiers(s, remote, fallbackPath)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers(remote=%q, path=%q): %v", remote, fallbackPath, err)
	}

	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = ingest.HarnessClaudeCode
	ingested := time.Now().UnixMilli()
	meta.Timestamp = ingest.TimestampInfo{
		Start:    1708300800000,
		End:      1708300860000,
		Ingested: &ingested,
	}
	meta.HostSlug = hostSlug
	meta.Project = ingest.ProjectInfo{
		Hash:     projectHash,
		FilePath: fallbackPath,
		Name:     "testrepo",
	}
	if remote != "" {
		meta.Git = ingest.GitContext{
			Remote:   &remote,
			Worktree: &fallbackPath,
		}
	}
	return &meta
}

// runPhaseBPipeline assembles and runs a pipeline with the given adapter factory and salt.
// Returns the PipelineResult and the MemFS for further inspection.
func runPhaseBPipeline(
	t *testing.T,
	mfs *testutil.MemFS,
	git ingest.GitResolver,
	sessions []ingest.DiscoveredSession,
	metadata map[ingest.SessionID]*ingest.UnifiedMetadata,
	s salt.Salt,
	outputDir string,
) *ingest.PipelineResult {
	t.Helper()

	factory := func(fs ingest.FileSystem, gr ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
		return &testutil.StubAdapter{
			ProviderValue: ingest.HarnessClaudeCode,
			Sessions:      sessions,
			Metadata:      metadata,
		}
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: factory,
	}

	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {
				Paths:   []ingest.ResolvedPath{"/sources"},
				Enabled: true,
			},
		},
		OutputDir:          ingest.ResolvedPath(outputDir),
		StalenessThreshold: 5 * time.Minute,
	}

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithSalt(s))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}
	return result
}

// readMetadataFromFS reads and unmarshals the {sessionDir}/{sessionID}--metadata.json file.
func readMetadataFromFS(t *testing.T, mfs *testutil.MemFS, outputDir, hostSlug, sessionID string) ingest.UnifiedMetadata {
	t.Helper()
	metaPath := fmt.Sprintf("%s/%s/%s/%s--metadata.json", outputDir, hostSlug, sessionID, sessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", metaPath, err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("Unmarshal metadata at %q: %v", metaPath, err)
	}
	return meta
}

// --- Tests ---

// TestPipelinePhaseB_SaltedHashing verifies that metadata written to disk contains a
// ProjectHash equal to HMAC-SHA256(salt, normalized_remote), not a raw SHA-256.
func TestPipelinePhaseB_SaltedHashing(t *testing.T) {
	const outputDir = "/out/salted"
	s := makePhaseBSalt("test-salt-seed-A")
	remote := testutil.TestGitRemote // "git@github.com:testuser/testrepo.git"
	fallbackPath := testutil.TestDefaultWorktreeDir

	// Compute expected hash using the same function the adapter uses.
	wantHash, wantHostSlug, err := ingest.DeriveProjectIdentifiers(s, remote, fallbackPath)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers: %v", err)
	}

	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	sourcePath := fmt.Sprintf("/sources/%s.jsonl", testutil.TestSessionUUID)
	setupSourceFile(t, mfs, sourcePath)

	session := makePhaseBSession(t, testutil.TestSessionUUID, sourcePath)
	meta := makePhaseBMeta(t, testutil.TestSessionUUID, s, remote, fallbackPath)

	result := runPhaseBPipeline(t, mfs, git, []ingest.DiscoveredSession{session},
		map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}, s, outputDir)

	if result.Summary.New != 1 {
		t.Fatalf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Fatalf("Summary.Errors = %d, want 0 (sessions: %+v)", result.Summary.Errors, result.Sessions)
	}

	written := readMetadataFromFS(t, mfs, outputDir, string(wantHostSlug), testutil.TestSessionUUID)

	if written.Project.Hash != wantHash {
		t.Errorf("Project.Hash = %q, want HMAC-SHA256 hash %q", written.Project.Hash, wantHash)
	}

	// Sanity: the hash must be a 64-char hex string (HMAC-SHA256 → 32 bytes → 64 hex chars).
	if len(string(written.Project.Hash)) != 64 {
		t.Errorf("Project.Hash length = %d, want 64 (full hex SHA-256)", len(string(written.Project.Hash)))
	}
}

// TestPipelinePhaseB_SSHHTTPSDedup verifies that SSH and HTTPS remotes for the same
// repository produce the same ProjectHash (after URL normalization).
func TestPipelinePhaseB_SSHHTTPSDedup(t *testing.T) {
	const (
		outputDir    = "/out/dedup"
		sessionSSH   = testutil.TestSessionUUID
		sessionHTTPS = testutil.TestSessionUUID2
	)

	sshRemote := "git@github.com:testuser/testrepo.git"
	httpsRemote := "https://github.com/testuser/testrepo.git"
	fallbackPath := testutil.TestDefaultWorktreeDir

	s := makePhaseBSalt("test-salt-seed-B")

	// Both remotes normalize to "github.com/testuser/testrepo" before hashing.
	hashSSH, _, err := ingest.DeriveProjectIdentifiers(s, sshRemote, fallbackPath)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers(ssh): %v", err)
	}
	hashHTTPS, _, err := ingest.DeriveProjectIdentifiers(s, httpsRemote, fallbackPath)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers(https): %v", err)
	}

	if hashSSH != hashHTTPS {
		t.Fatalf("SSH hash %q != HTTPS hash %q — normalization is not working", hashSSH, hashHTTPS)
	}

	// Set up MemFS with two source files (one per session).
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath1 := fmt.Sprintf("/sources/%s.jsonl", sessionSSH)
	sourcePath2 := fmt.Sprintf("/sources/%s.jsonl", sessionHTTPS)
	setupSourceFile(t, mfs, sourcePath1)
	setupSourceFile(t, mfs, sourcePath2)

	metaSSH := makePhaseBMeta(t, sessionSSH, s, sshRemote, fallbackPath)
	metaHTTPS := makePhaseBMeta(t, sessionHTTPS, s, httpsRemote, fallbackPath)

	sid1, _ := ingest.NewSessionID(sessionSSH)
	sid2, _ := ingest.NewSessionID(sessionHTTPS)

	session1 := makePhaseBSession(t, sessionSSH, sourcePath1)
	session2 := makePhaseBSession(t, sessionHTTPS, sourcePath2)

	result := runPhaseBPipeline(t, mfs, git,
		[]ingest.DiscoveredSession{session1, session2},
		map[ingest.SessionID]*ingest.UnifiedMetadata{
			sid1: metaSSH,
			sid2: metaHTTPS,
		},
		s, outputDir)

	if result.Summary.New != 2 {
		t.Fatalf("Summary.New = %d, want 2", result.Summary.New)
	}

	// Both written metadata files must have the same ProjectHash.
	_, hostSlug1, _ := ingest.DeriveProjectIdentifiers(s, sshRemote, fallbackPath)
	_, hostSlug2, _ := ingest.DeriveProjectIdentifiers(s, httpsRemote, fallbackPath)

	written1 := readMetadataFromFS(t, mfs, outputDir, string(hostSlug1), sessionSSH)
	written2 := readMetadataFromFS(t, mfs, outputDir, string(hostSlug2), sessionHTTPS)

	if written1.Project.Hash != written2.Project.Hash {
		t.Errorf("SSH session ProjectHash %q != HTTPS session ProjectHash %q — same repo should share hash",
			written1.Project.Hash, written2.Project.Hash)
	}
	if written1.Project.Hash != hashSSH {
		t.Errorf("SSH session ProjectHash %q != expected %q", written1.Project.Hash, hashSSH)
	}
}

// TestPipelinePhaseB_ReadableFilesystemDirs verifies that pipeline output directories
// use the human-readable HostSlug (e.g. "github.com--testuser--testrepo"), not an
// opaque hash.
func TestPipelinePhaseB_ReadableFilesystemDirs(t *testing.T) {
	const outputDir = "/out/readable"
	s := makePhaseBSalt("test-salt-seed-C")
	remote := testutil.TestGitRemote
	fallbackPath := testutil.TestDefaultWorktreeDir

	_, wantHostSlug, err := ingest.DeriveProjectIdentifiers(s, remote, fallbackPath)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers: %v", err)
	}

	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	sourcePath := fmt.Sprintf("/sources/%s.jsonl", testutil.TestSessionUUID)
	setupSourceFile(t, mfs, sourcePath)

	session := makePhaseBSession(t, testutil.TestSessionUUID, sourcePath)
	meta := makePhaseBMeta(t, testutil.TestSessionUUID, s, remote, fallbackPath)

	result := runPhaseBPipeline(t, mfs, git, []ingest.DiscoveredSession{session},
		map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}, s, outputDir)

	if result.Summary.New != 1 {
		t.Fatalf("Summary.New = %d, want 1", result.Summary.New)
	}

	// The output directory must use human-readable slug ("github.com--testuser--testrepo"),
	// never an opaque hash (64-char hex string).
	sessionDir := fmt.Sprintf("%s/%s/%s", outputDir, wantHostSlug, testutil.TestSessionUUID)
	if _, err := mfs.Stat(sessionDir); err != nil {
		t.Errorf("expected readable session dir %q to exist: %v", sessionDir, err)
	}

	// HostSlug must NOT be a 64-char hex string (would indicate it was hashed).
	hostSlugStr := string(wantHostSlug)
	if len(hostSlugStr) == 64 && isHex(hostSlugStr) {
		t.Errorf("HostSlug %q looks like an opaque hash — want human-readable format", hostSlugStr)
	}

	// HostSlug must contain at least one "--" separator (e.g. "github.com--user--repo").
	if !strings.Contains(hostSlugStr, "--") {
		t.Errorf("HostSlug %q has no '--' separator — expected human-readable format like 'host--user--repo'", hostSlugStr)
	}
}

// isHex returns true if s is a non-empty string composed entirely of hex digits.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// TestPipelinePhaseB_DifferentSalts verifies that two pipeline runs with different
// salts produce different ProjectHash values for the same repository.
func TestPipelinePhaseB_DifferentSalts(t *testing.T) {
	remote := testutil.TestGitRemote
	fallbackPath := testutil.TestDefaultWorktreeDir

	salt1 := makePhaseBSalt("installation-A-salt-seed")
	salt2 := makePhaseBSalt("installation-B-salt-seed")

	hash1, _, err := ingest.DeriveProjectIdentifiers(salt1, remote, fallbackPath)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers(salt1): %v", err)
	}
	hash2, _, err := ingest.DeriveProjectIdentifiers(salt2, remote, fallbackPath)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers(salt2): %v", err)
	}

	if hash1 == hash2 {
		t.Errorf("hash1 == hash2 (%q) — different salts must produce different ProjectHash values", hash1)
	}

	// Run two separate pipeline instances with different salts and verify
	// the written metadata reflects the correct per-salt hash.
	for _, tc := range []struct {
		name      string
		s         salt.Salt
		wantHash  ingest.ProjectHash
		outputDir string
	}{
		{"salt1", salt1, hash1, "/out/salt1"},
		{"salt2", salt2, hash2, "/out/salt2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			git := testutil.DefaultGitResolver()
			sourcePath := fmt.Sprintf("/sources/%s.jsonl", testutil.TestSessionUUID)
			setupSourceFile(t, mfs, sourcePath)

			session := makePhaseBSession(t, testutil.TestSessionUUID, sourcePath)
			meta := makePhaseBMeta(t, testutil.TestSessionUUID, tc.s, remote, fallbackPath)

			result := runPhaseBPipeline(t, mfs, git, []ingest.DiscoveredSession{session},
				map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}, tc.s, tc.outputDir)

			if result.Summary.New != 1 {
				t.Fatalf("Summary.New = %d, want 1", result.Summary.New)
			}

			_, hostSlug, _ := ingest.DeriveProjectIdentifiers(tc.s, remote, fallbackPath)
			written := readMetadataFromFS(t, mfs, tc.outputDir, string(hostSlug), testutil.TestSessionUUID)

			if written.Project.Hash != tc.wantHash {
				t.Errorf("Project.Hash = %q, want %q", written.Project.Hash, tc.wantHash)
			}
		})
	}
}

// TestPipelinePhaseB_UntrackedProject verifies that a session with no git remote
// produces a HostSlug in the format __peasant-untracked__--{hash8}--{basename}.
func TestPipelinePhaseB_UntrackedProject(t *testing.T) {
	const outputDir = "/out/untracked"
	const localPath = "/home/user/local-project"

	s := makePhaseBSalt("test-salt-seed-E")

	// No remote: DeriveProjectIdentifiers falls back to path-based slug.
	_, hostSlug, err := ingest.DeriveProjectIdentifiers(s, "", localPath)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers(no remote): %v", err)
	}

	hostSlugStr := string(hostSlug)

	// Must start with the untracked prefix.
	if !strings.HasPrefix(hostSlugStr, defaults.UntrackedPrefix) {
		t.Errorf("HostSlug %q does not start with %q", hostSlugStr, defaults.UntrackedPrefix)
	}

	// Format: __peasant-untracked__--{hash8}--{basename}
	// Split on "--" → [__peasant-untracked__, hash8, basename]
	parts := strings.SplitN(hostSlugStr, "--", 3)
	if len(parts) < 3 {
		t.Fatalf("HostSlug %q: expected 3 '--' parts, got %d", hostSlugStr, len(parts))
	}
	hash8 := parts[1]
	basename := parts[2]

	if len(hash8) != 8 {
		t.Errorf("hash8 part %q: length %d, want 8", hash8, len(hash8))
	}
	if !isHex(hash8) {
		t.Errorf("hash8 part %q is not hex", hash8)
	}
	if basename != "local-project" {
		t.Errorf("basename part %q, want %q", basename, "local-project")
	}

	// Run the pipeline to verify filesystem output uses this slug.
	mfs := testutil.NewMemFS()
	git := &testutil.StubGitResolver{
		WorktreeDir: localPath,
		RemoteErr:   fmt.Errorf("no remote origin"),
		BranchErr:   fmt.Errorf("no remote origin"),
		Email:       testutil.TestEmail,
	}
	sourcePath := fmt.Sprintf("/sources/%s.jsonl", testutil.TestSessionUUID)
	setupSourceFile(t, mfs, sourcePath)

	session := makePhaseBSession(t, testutil.TestSessionUUID, sourcePath)
	meta := makePhaseBMeta(t, testutil.TestSessionUUID, s, "", localPath)

	result := runPhaseBPipeline(t, mfs, git, []ingest.DiscoveredSession{session},
		map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}, s, outputDir)

	if result.Summary.New != 1 {
		t.Fatalf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Output directory must use the untracked slug, not a raw hash.
	sessionDir := fmt.Sprintf("%s/%s/%s", outputDir, hostSlugStr, testutil.TestSessionUUID)
	if _, err := mfs.Stat(sessionDir); err != nil {
		t.Errorf("expected untracked session dir %q to exist: %v", sessionDir, err)
	}
}

// TestPipelinePhaseB_PushGate verifies that project.hash is ALWAYS present in
// the push payload (R1) — it is the per-installation salted, non-correlatable
// hash and is no longer gated by PushFieldVisibility — even when every
// raw-identity field is omitted under the default (all-false) visibility.
func TestPipelinePhaseB_PushGate(t *testing.T) {
	const outputDir = "/out/pushgate"
	s := makePhaseBSalt("test-salt-seed-F")
	remote := testutil.TestGitRemote
	fallbackPath := testutil.TestDefaultWorktreeDir

	projectHash, hostSlug, err := ingest.DeriveProjectIdentifiers(s, remote, fallbackPath)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers: %v", err)
	}
	if projectHash == "" {
		t.Fatal("DeriveProjectIdentifiers returned empty hash — test setup error")
	}

	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	sourcePath := fmt.Sprintf("/sources/%s.jsonl", testutil.TestSessionUUID)
	setupSourceFile(t, mfs, sourcePath)

	session := makePhaseBSession(t, testutil.TestSessionUUID, sourcePath)
	meta := makePhaseBMeta(t, testutil.TestSessionUUID, s, remote, fallbackPath)

	result := runPhaseBPipeline(t, mfs, git, []ingest.DiscoveredSession{session},
		map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}, s, outputDir)

	if result.Summary.New != 1 {
		t.Fatalf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Read written metadata to confirm it has a non-empty ProjectHash on disk.
	written := readMetadataFromFS(t, mfs, outputDir, string(hostSlug), testutil.TestSessionUUID)
	if written.Project.Hash == "" {
		t.Fatal("metadata on disk has empty Project.Hash — test setup error")
	}

	// Build a push payload with default field visibility (all false).
	defaultVisibility := config.DefaultPushFieldVisibility()
	payload, err := push.MapMetadata(push.MapOptions{
		Meta:   &written,
		Fields: defaultVisibility,
	})
	if err != nil {
		t.Fatalf("MapMetadata: %v", err)
	}

	// Unmarshal the publish request payload and confirm ProjectHash is absent/empty.
	var req schema.PublishRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("json.Unmarshal PublishRequest: %v", err)
	}

	// project.hash is ALWAYS sent (salted, non-correlatable) regardless of the
	// default all-false field visibility — it must match the derived hash.
	if string(req.Project.Hash) != string(projectHash) {
		t.Errorf("PublishRequest.Project.Hash = %q, want %q (always sent, salted)",
			req.Project.Hash, projectHash)
	}

	// Verify the raw JSON carries the hash too (belt-and-suspenders check).
	var rawMap map[string]any
	if err := json.Unmarshal(payload, &rawMap); err != nil {
		t.Fatalf("json.Unmarshal raw map: %v", err)
	}
	project, ok := rawMap["project"].(map[string]any)
	if !ok {
		t.Fatalf("raw JSON has no project object")
	}
	if hash, _ := project["hash"].(string); hash != string(projectHash) {
		t.Errorf("raw JSON project.hash = %v, want %q", project["hash"], projectHash)
	}
}
