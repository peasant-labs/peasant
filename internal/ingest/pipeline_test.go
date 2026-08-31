package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// --- Test helpers ---

const (
	testOutputDir = "/output"
	testSourceDir = "/sources"
	testSessionID = testutil.TestSessionUUID
	// A second valid UUID for multi-session tests.
	testSessionID2 = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// makeStubAdapter returns an AdapterFactory backed by a StubAdapter.
// The salt parameter is accepted but ignored; stub adapters return pre-built metadata.
func makeStubAdapter(sessions []ingest.DiscoveredSession, metadata map[ingest.SessionID]*ingest.UnifiedMetadata) ingest.AdapterFactory {
	return func(fs ingest.FileSystem, git ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
		return &testutil.StubAdapter{
			ProviderValue: ingest.HarnessClaudeCode,
			Sessions:      sessions,
			Metadata:      metadata,
		}
	}
}

// makeStubAdapterWithErrors returns an AdapterFactory whose ExtractMetadata fails for
// the given session IDs and succeeds (using stubMeta) for all others.
// The salt parameter is accepted but ignored; stub adapters return pre-built metadata.
func makeStubAdapterWithErrors(sessions []ingest.DiscoveredSession, metadata map[ingest.SessionID]*ingest.UnifiedMetadata, failIDs map[ingest.SessionID]error) ingest.AdapterFactory {
	return func(fs ingest.FileSystem, git ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
		return &stubbedErrorAdapter{
			ProviderValue: ingest.HarnessClaudeCode,
			Sessions:      sessions,
			Metadata:      metadata,
			FailIDs:       failIDs,
		}
	}
}

// stubbedErrorAdapter is a SourceAdapter that returns per-session errors.
type stubbedErrorAdapter struct {
	ProviderValue ingest.Harness
	Sessions      []ingest.DiscoveredSession
	Metadata      map[ingest.SessionID]*ingest.UnifiedMetadata
	FailIDs       map[ingest.SessionID]error
}

func (a *stubbedErrorAdapter) Harness() ingest.Harness { return a.ProviderValue }

func (a *stubbedErrorAdapter) Discover(_ context.Context, _ ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	return a.Sessions, nil
}

func (a *stubbedErrorAdapter) ExtractMetadata(_ context.Context, s ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	if err, ok := a.FailIDs[s.SessionID]; ok {
		return nil, err
	}
	m, ok := a.Metadata[s.SessionID]
	if !ok {
		return nil, fmt.Errorf("no metadata for session %s", s.SessionID)
	}
	return m, nil
}

// makeDiscoveredSession creates a DiscoveredSession for testing.
func makeDiscoveredSession(t *testing.T, sessionIDStr string, sourcePathStr string, modTime time.Time) ingest.DiscoveredSession {
	t.Helper()
	sid, err := ingest.NewSessionID(sessionIDStr)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionIDStr, err)
	}
	return ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(sourcePathStr),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
		ModTime:       modTime,
	}
}

// makeMinimalMeta builds a UnifiedMetadata suitable for testing.
// It uses TestGitRemote so DeriveHostSlug works without errors.
func makeMinimalMeta(t *testing.T, sessionIDStr string) *ingest.UnifiedMetadata {
	t.Helper()
	sid, err := ingest.NewSessionID(sessionIDStr)
	if err != nil {
		t.Fatalf("makeMinimalMeta NewSessionID(%q): %v", sessionIDStr, err)
	}
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = ingest.HarnessClaudeCode
	ingested := time.Now().UnixMilli()
	meta.Timestamp = ingest.TimestampInfo{
		Start:    1708300800000, // 2024-02-19T00:00:00Z
		End:      1708300860000,
		Ingested: &ingested,
	}
	remote := testutil.TestGitRemote
	worktree := "/home/test/testrepo"
	meta.Git = ingest.GitContext{
		Remote:   &remote,
		Worktree: &worktree,
	}
	meta.Project = ingest.ProjectInfo{
		FilePath: "/home/test/testrepo",
		Name:     "testrepo",
	}
	meta.HostSlug = ingest.HostSlug(testutil.TestHostSlug)
	return &meta
}

// setupSourceFile writes a minimal JSONL source file to the MemFS.
func setupSourceFile(t *testing.T, mfs *testutil.MemFS, path string) {
	t.Helper()
	content := []byte(`{"sessionId":"test","type":"user","message":{"role":"user","content":"hi"},"timestamp":"2024-02-19T00:00:00Z"}` + "\n")
	if err := mfs.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("setupSourceFile WriteFile(%q): %v", path, err)
	}
}

// makePipelineConfig builds a PipelineConfig for testing.
func makePipelineConfig(outputDir string, opts ...func(*ingest.PipelineConfig)) ingest.PipelineConfig {
	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {
				Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(testSourceDir)},
				Enabled: true,
			},
		},
		OutputDir:          ingest.ResolvedPath(outputDir),
		StalenessThreshold: 5 * time.Minute,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// expectedOutputBase returns the base output path: {outputDir}/{hostSlug}/{sessionID}
func expectedOutputBase(outputDir, sessionID string) string {
	return fmt.Sprintf("%s/%s/%s", outputDir, testutil.TestHostSlug, sessionID)
}

// markingRedactor is a test-double TextRedactor that marks redacted content
// with a detectable prefix, enabling tests to verify both metadata and transcript
// redaction in a single pipeline run.
// MetadataCalled tracks RedactMetadata invocations; JSONCalled tracks RedactJSON invocations.
type markingRedactor struct {
	MetadataCalled int
	JSONCalled     int
}

var _ ingest.TextRedactor = (*markingRedactor)(nil)

func (r *markingRedactor) RedactMetadata(meta *ingest.UnifiedMetadata) *ingest.UnifiedMetadata {
	r.MetadataCalled++
	if meta == nil {
		return nil
	}
	redacted := *meta
	redacted.Project.Name = "REDACTED_" + meta.Project.Name
	redacted.Project.FilePath = "REDACTED_PATH"
	return &redacted
}

func (r *markingRedactor) RedactJSON(value any) any {
	r.JSONCalled++
	return markJSONValue(value)
}

func (r *markingRedactor) Level() string {
	return "standard"
}

func (r *markingRedactor) RuleSetVersion() string {
	return "1.0.0"
}

// markJSONValue recursively marks all string values with a "REDACTED_" prefix.
// Non-string scalars (numbers, booleans, nil) pass through unchanged.
func markJSONValue(value any) any {
	switch v := value.(type) {
	case string:
		return "REDACTED_" + v
	case []any:
		result := make([]any, len(v))
		for i, elem := range v {
			result[i] = markJSONValue(elem)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(v))
		for k, val := range v {
			result[k] = markJSONValue(val)
		}
		return result
	default:
		return value
	}
}

// --- Tests ---

func TestDiffStatus_String(t *testing.T) {
	tests := []struct {
		status ingest.DiffStatus
		want   string
	}{
		{ingest.DiffNew, "new"},
		{ingest.DiffUpdated, "updated"},
		{ingest.DiffUnchanged, "unchanged"},
		{ingest.DiffActive, "active"},
		{ingest.DiffStatus(99), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.status.String()
			if got != tt.want {
				t.Errorf("DiffStatus(%d).String() = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestPipeline_EndToEnd(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session.SessionID: meta,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Summary: 1 new session
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	if result.Sessions[0].Error != nil {
		t.Errorf("Sessions[0].Error = %v, want nil", result.Sessions[0].Error)
	}

	// Metadata should be written to correct path.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}
	var written ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}
	if written.SessionID != session.SessionID {
		t.Errorf("metadata.SessionID = %q, want %q", written.SessionID, session.SessionID)
	}

	// Transcript should be copied to output dir.
	transcriptFilename := fmt.Sprintf("%s--transcript.jsonl", testSessionID)
	transcriptPath := fmt.Sprintf("%s/%s", base, transcriptFilename)
	if _, err := mfs.Stat(transcriptPath); err != nil {
		t.Errorf("transcript not found at %q: %v", transcriptPath, err)
	}
}

func TestPipeline_PreparesCompleteSessionFilterCohortBeforeMatching(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	sessionIDs := []string{testSessionID, testSessionID2}
	sessions := make([]ingest.DiscoveredSession, 0, len(sessionIDs))
	metadata := make(map[ingest.SessionID]*ingest.UnifiedMetadata, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, sessionID)
		setupSourceFile(t, mfs, sourcePath)
		session := makeDiscoveredSession(t, sessionID, sourcePath, time.Now().Add(-time.Hour))
		sessions = append(sessions, session)
		metadata[session.SessionID] = makeMinimalMeta(t, sessionID)
	}

	prepareCalls := 0
	filterCalls := 0
	prepared := make(map[ingest.SessionID]bool)
	cfg := makePipelineConfig(testOutputDir, func(cfg *ingest.PipelineConfig) {
		cfg.PrepareSessionFilter = func(_ context.Context, cohort []ingest.DiscoveredSession) error {
			prepareCalls++
			if filterCalls != 0 {
				t.Fatalf("preparation ran after %d filter call(s)", filterCalls)
			}
			if len(cohort) != len(sessions) {
				t.Fatalf("preparation cohort has %d sessions, want complete cohort of %d", len(cohort), len(sessions))
			}
			for _, session := range cohort {
				prepared[session.SessionID] = true
			}
			return nil
		}
		cfg.SessionFilter = func(session ingest.DiscoveredSession) bool {
			filterCalls++
			if prepareCalls != 1 || len(prepared) != len(sessions) {
				t.Fatalf("filter observed preparation calls=%d prepared=%d, want 1 and %d", prepareCalls, len(prepared), len(sessions))
			}
			return prepared[session.SessionID]
		}
	})
	pipeline, err := ingest.NewPipeline(mfs, git, map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(sessions, metadata),
	}, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if _, err := pipeline.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prepareCalls != 1 {
		t.Errorf("preparation calls = %d, want 1", prepareCalls)
	}
	if filterCalls != len(sessions) {
		t.Errorf("filter calls = %d, want %d", filterCalls, len(sessions))
	}
}

func TestPipeline_StopsWhenSessionFilterPreparationFails(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)
	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-time.Hour))
	prepareErr := errors.New("cohort preparation failed")
	filterCalled := false
	cfg := makePipelineConfig(testOutputDir, func(cfg *ingest.PipelineConfig) {
		cfg.PrepareSessionFilter = func(context.Context, []ingest.DiscoveredSession) error { return prepareErr }
		cfg.SessionFilter = func(ingest.DiscoveredSession) bool {
			filterCalled = true
			return true
		}
	})
	pipeline, err := ingest.NewPipeline(mfs, git, map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{
			session.SessionID: makeMinimalMeta(t, testSessionID),
		}),
	}, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	_, err = pipeline.Run(context.Background())
	if !errors.Is(err, prepareErr) || !strings.Contains(err.Error(), "prepare session filter after discovery") {
		t.Fatalf("Run error = %v, want actionable preparation failure wrapping %v", err, prepareErr)
	}
	if filterCalled {
		t.Error("SessionFilter ran after preparation failed")
	}
}

func TestPipeline_Incremental(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	// ModTime older than staleness threshold.
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, mfs, sourcePath)
	mfs.ModTimes[sourcePath] = modTime

	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session.SessionID: meta,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir)

	// First run: should ingest as new.
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result1, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("First Run: %v", err)
	}
	if result1.Summary.New != 1 {
		t.Errorf("First run Summary.New = %d, want 1", result1.Summary.New)
	}

	// Second run: source hasn't changed. Should be unchanged.
	// The metadata written in run 1 has Ingested > modTime.
	pipeline2, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result2, err := pipeline2.Run(context.Background())
	if err != nil {
		t.Fatalf("Second Run: %v", err)
	}
	if result2.Summary.Unchanged != 1 {
		t.Errorf("Second run Summary.Unchanged = %d, want 1", result2.Summary.Unchanged)
	}
	if result2.Summary.New != 0 {
		t.Errorf("Second run Summary.New = %d, want 0", result2.Summary.New)
	}
}

func TestPipeline_DryRun(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session.SessionID: meta,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.DryRun = true
	})
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// DryRun: session results must be populated but no files written.
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	if result.Sessions[0].Status != ingest.DiffNew {
		t.Errorf("Sessions[0].Status = %v, want DiffNew", result.Sessions[0].Status)
	}

	// No output files should have been written.
	metaPath := fmt.Sprintf("%s/%s/%s/%s--metadata.json",
		testOutputDir, testutil.TestHostSlug, testSessionID, testSessionID)
	if _, err := mfs.Stat(metaPath); err == nil {
		t.Errorf("metadata file should not exist in dry run mode, but found at %q", metaPath)
	}
}

func TestPipeline_Force(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, mfs, sourcePath)
	mfs.ModTimes[sourcePath] = modTime

	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session.SessionID: meta,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir)

	// First run.
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result1, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("First Run: %v", err)
	}
	if result1.Summary.New != 1 {
		t.Errorf("First run Summary.New = %d, want 1", result1.Summary.New)
	}

	// Second run with Force=true: should re-ingest as new.
	cfgForce := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Force = true
	})
	pipeline2, err := ingest.NewPipeline(mfs, git, adapters, cfgForce)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result2, err := pipeline2.Run(context.Background())
	if err != nil {
		t.Fatalf("Second Run (force): %v", err)
	}
	if result2.Summary.New != 1 {
		t.Errorf("Force run Summary.New = %d, want 1", result2.Summary.New)
	}
}

func TestPipeline_ActiveSessionSkipped(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	// ModTime is very recent (within staleness threshold).
	recentModTime := time.Now().Add(-30 * time.Second)
	session := makeDiscoveredSession(t, testSessionID, sourcePath, recentModTime)
	mfs.ModTimes[sourcePath] = recentModTime

	meta := makeMinimalMeta(t, testSessionID)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session.SessionID: meta,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	// staleness threshold is 5 minutes; 30 seconds < 5 minutes, so it's active.

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Session should be classified as DiffActive.
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	if result.Sessions[0].Status != ingest.DiffActive {
		t.Errorf("Sessions[0].Status = %v, want DiffActive", result.Sessions[0].Status)
	}

	// Active sessions should not be ingested.
	if result.Summary.Active != 1 {
		t.Errorf("Summary.Active = %d, want 1", result.Summary.Active)
	}
	if result.Summary.New != 0 {
		t.Errorf("Summary.New = %d, want 0", result.Summary.New)
	}

	// No output files written.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	if _, err := mfs.Stat(metaPath); err == nil {
		t.Errorf("metadata should not be written for active session")
	}
}

func TestPipeline_IncludeActive(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	// ModTime is very recent.
	recentModTime := time.Now().Add(-30 * time.Second)
	session := makeDiscoveredSession(t, testSessionID, sourcePath, recentModTime)
	mfs.ModTimes[sourcePath] = recentModTime

	meta := makeMinimalMeta(t, testSessionID)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session.SessionID: meta,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.IncludeActive = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Session is still classified as DiffActive.
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	if result.Sessions[0].Status != ingest.DiffActive {
		t.Errorf("Sessions[0].Status = %v, want DiffActive", result.Sessions[0].Status)
	}
	if result.Sessions[0].Error != nil {
		t.Errorf("Sessions[0].Error = %v, want nil", result.Sessions[0].Error)
	}

	// With IncludeActive, the session IS ingested.
	if result.Summary.Active != 1 {
		t.Errorf("Summary.Active = %d, want 1", result.Summary.Active)
	}

	// Output files should be written.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	if _, err := mfs.Stat(metaPath); err != nil {
		t.Errorf("metadata should be written when IncludeActive=true: %v", err)
	}
}

func TestPipeline_ErrorResilience(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sid1, err := ingest.NewSessionID(testSessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	sid2, err := ingest.NewSessionID(testSessionID2)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	source2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, source1)
	setupSourceFile(t, mfs, source2)

	modTime := time.Now().Add(-1 * time.Hour)
	session1 := makeDiscoveredSession(t, testSessionID, source1, modTime)
	session2 := makeDiscoveredSession(t, testSessionID2, source2, modTime)

	meta2 := makeMinimalMeta(t, testSessionID2)

	// session1 fails extraction; session2 succeeds.
	extractErr := errors.New("extraction failed for session1")
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapterWithErrors(
			[]ingest.DiscoveredSession{session1, session2},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				sid2: meta2,
			},
			map[ingest.SessionID]error{
				sid1: extractErr,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Pipeline must not halt: both sessions must appear in results.
	if len(result.Sessions) != 2 {
		t.Fatalf("Sessions len = %d, want 2", len(result.Sessions))
	}

	// Find results by session ID.
	var errResult, okResult *ingest.SessionResult
	for i := range result.Sessions {
		sr := &result.Sessions[i]
		if sr.SessionID == sid1 {
			errResult = sr
		} else if sr.SessionID == sid2 {
			okResult = sr
		}
	}

	if errResult == nil {
		t.Fatal("session1 result not found")
	}
	if okResult == nil {
		t.Fatal("session2 result not found")
	}

	// session1 should have an error.
	if errResult.Error == nil {
		t.Errorf("session1 Error = nil, want non-nil")
	}

	// session2 should succeed.
	if okResult.Error != nil {
		t.Errorf("session2 Error = %v, want nil", okResult.Error)
	}

	// Summary counts.
	if result.Summary.Errors != 1 {
		t.Errorf("Summary.Errors = %d, want 1", result.Summary.Errors)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
}

func TestPipeline_OrphanCleanup(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Pre-create orphan .tmp-* directories.
	orphan1 := fmt.Sprintf("%s/.tmp-orphan-abc123", testOutputDir)
	orphan2 := fmt.Sprintf("%s/.tmp-another-def456", testOutputDir)
	if err := mfs.MkdirAll(orphan1, 0700); err != nil {
		t.Fatalf("MkdirAll orphan1: %v", err)
	}
	if err := mfs.WriteFile(orphan1+"/partial.json", []byte("{}"), 0600); err != nil {
		t.Fatalf("WriteFile orphan1/partial.json: %v", err)
	}
	if err := mfs.MkdirAll(orphan2, 0700); err != nil {
		t.Fatalf("MkdirAll orphan2: %v", err)
	}

	// No sessions to process (empty adapter).
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	_, err = pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Orphan dirs should be removed.
	if mfs.Dirs[orphan1] {
		t.Errorf("orphan dir %q still exists after pipeline run", orphan1)
	}
	if mfs.Dirs[orphan2] {
		t.Errorf("orphan dir %q still exists after pipeline run", orphan2)
	}
}

func TestPipeline_MultipleProviders(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	// Only Claude provider is enabled; Gemini is registered but disabled.
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
		ingest.HarnessGeminiCLI: makeStubAdapter(
			[]ingest.DiscoveredSession{session}, // would double-count if called
			nil,
		),
	}

	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(testSourceDir)}},
			ingest.HarnessGeminiCLI:  {Enabled: false}, // explicitly disabled
		},
		OutputDir:          ingest.ResolvedPath(testOutputDir),
		StalenessThreshold: 5 * time.Minute,
	}

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Only 1 session from Claude (Gemini is disabled).
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1 (Gemini disabled)", result.Summary.New)
	}
	if len(result.Sessions) != 1 {
		t.Errorf("Sessions len = %d, want 1", len(result.Sessions))
	}
}

func TestPipeline_NoSessionsDiscovered(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.New != 0 {
		t.Errorf("Summary.New = %d, want 0", result.Summary.New)
	}
	if len(result.Sessions) != 0 {
		t.Errorf("Sessions len = %d, want 0", len(result.Sessions))
	}
}

func TestPipeline_OutputFileNaming(t *testing.T) {
	// Verify that the pipeline writes metadata and transcript with correct naming conventions.
	// NOTE: MemFS does not track permission bits, so actual file permission verification
	// requires OSFileSystem. This test focuses on naming only.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		t.Fatalf("Unexpected errors: %v", result.Sessions)
	}

	// Verify metadata file has correct name format.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	if _, err := mfs.Stat(metaPath); err != nil {
		t.Errorf("Expected metadata at %q: %v", metaPath, err)
	}

	// Verify transcript has correct name format: {sessionId}--transcript.{ext}
	transcriptName := fmt.Sprintf("%s--transcript.jsonl", testSessionID)
	transcriptPath := fmt.Sprintf("%s/%s", base, transcriptName)
	if _, err := mfs.Stat(transcriptPath); err != nil {
		t.Errorf("Expected transcript at %q: %v", transcriptPath, err)
	}
}

func TestPipeline_IncrementalUpdated(t *testing.T) {
	// Verify that a session is re-ingested when source is newer than last ingest.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	// First ingest: modtime 2 hours ago.
	oldModTime := time.Now().Add(-2 * time.Hour)
	mfs.ModTimes[sourcePath] = oldModTime
	session := makeDiscoveredSession(t, testSessionID, sourcePath, oldModTime)
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result1, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("First Run: %v", err)
	}
	if result1.Summary.New != 1 {
		t.Errorf("First run: Summary.New = %d, want 1", result1.Summary.New)
	}

	// Update the source modtime to AFTER the pipeline just wrote metadata.
	// We do this by backdating the metadata's Ingested field to make it appear
	// the ingest happened before the new modtime.
	// Read the written metadata, update Ingested to simulate stale ingest, rewrite.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile written metadata: %v", err)
	}
	var written ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("Unmarshal written metadata: %v", err)
	}
	// Set Ingested to 3 hours ago so any subsequent modtime will look newer.
	ingested := time.Now().Add(-3 * time.Hour).UnixMilli()
	written.Timestamp.Ingested = &ingested
	updatedData, err := json.Marshal(written)
	if err != nil {
		t.Fatalf("Marshal updated metadata: %v", err)
	}
	if err := mfs.WriteFile(metaPath, updatedData, 0600); err != nil {
		t.Fatalf("WriteFile updated metadata: %v", err)
	}

	// Update source modtime to 1 hour ago — newer than the backdated ingest.
	newModTime := time.Now().Add(-1 * time.Hour)
	mfs.ModTimes[sourcePath] = newModTime
	session.ModTime = newModTime

	adapters2 := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	pipeline2, err := ingest.NewPipeline(mfs, git, adapters2, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result2, err := pipeline2.Run(context.Background())
	if err != nil {
		t.Fatalf("Second Run: %v", err)
	}
	if result2.Summary.Updated != 1 {
		t.Errorf("Second run: Summary.Updated = %d, want 1", result2.Summary.Updated)
	}
}

func TestPipeline_Result_Duration(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", result.Duration)
	}
}

func TestPipeline_MetadataContents(t *testing.T) {
	// Verify that the written metadata JSON contains the expected fields.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)
	meta.Version = "2.1.47"

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		for _, sr := range result.Sessions {
			if sr.Error != nil {
				t.Logf("Session %s error: %v", sr.SessionID, sr.Error)
			}
		}
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}

	var written ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if written.SchemaVersion != ingest.CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", written.SchemaVersion, ingest.CurrentSchemaVersion)
	}
	if written.SessionID != session.SessionID {
		t.Errorf("SessionID = %q, want %q", written.SessionID, session.SessionID)
	}
	if written.Version != "2.1.47" {
		t.Errorf("Version = %q, want %q", written.Version, "2.1.47")
	}
	if written.Timestamp.Ingested == nil || *written.Timestamp.Ingested == 0 {
		t.Error("Timestamp.Ingested = 0, want non-zero")
	}
}

func TestPipeline_SessionResultStatus(t *testing.T) {
	// Verify that SessionResult.Status reflects the DiffStatus.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)
	modTime := time.Now().Add(-2 * time.Hour)
	mfs.ModTimes[sourcePath] = modTime

	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	sr := result.Sessions[0]
	if sr.Status != ingest.DiffNew {
		t.Errorf("SessionResult.Status = %v, want DiffNew", sr.Status)
	}
	if sr.Harness != ingest.HarnessClaudeCode {
		t.Errorf("SessionResult.Harness = %v, want %v", sr.Harness, ingest.HarnessClaudeCode)
	}
}

// failAfterNCopyFS wraps MemFS and makes CopyFile fail after the first N successes.
// This is used to simulate a mid-walk failure inside renameDir.
type failAfterNCopyFS struct {
	*testutil.MemFS
	allowedCopies int
	copyCount     int
}

func (f *failAfterNCopyFS) CopyFile(src, dst string, perm os.FileMode) error {
	if f.copyCount >= f.allowedCopies {
		return fmt.Errorf("injected CopyFile failure after %d copies (src=%s)", f.allowedCopies, src)
	}
	f.copyCount++
	return f.MemFS.CopyFile(src, dst, perm)
}

func TestPipeline_RenameDirCleansDstOnFailure(t *testing.T) {
	// Arrange: a MemFS that fails the first CopyFile call inside renameDir.
	// processSession writes only transcript to tmpDir (metadata.json is written
	// by drainLoop after DB INSERT). renameDir uses CopyFile to move the tmpDir
	// contents to sessionDir. Failing on the first renameDir CopyFile must trigger
	// cleanup of the partial sessionDir destination.
	innerFS := testutil.NewMemFS()
	mfs := &failAfterNCopyFS{
		MemFS:         innerFS,
		allowedCopies: 0, // first CopyFile (inside renameDir, transcript) fails immediately
	}
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, innerFS, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The session must report an error (renameDir failed).
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	if result.Sessions[0].Error == nil {
		t.Errorf("Sessions[0].Error = nil, want non-nil (renameDir should have failed)")
	}
	if result.Summary.Errors != 1 {
		t.Errorf("Summary.Errors = %d, want 1", result.Summary.Errors)
	}

	// The destination sessionDir must NOT exist — renameDir cleanup must have removed it.
	base := expectedOutputBase(testOutputDir, testSessionID)
	if innerFS.Dirs[base] {
		t.Errorf("sessionDir %q still exists after renameDir failure; partial dst was not cleaned up", base)
	}
	// Also verify no files leaked into sessionDir.
	entries, _ := innerFS.ReadDir(testOutputDir + "/" + testutil.TestHostSlug)
	for _, e := range entries {
		if e.Name() == testSessionID {
			t.Errorf("sessionDir entry %q found under host slug dir; partial dst was not cleaned up", e.Name())
		}
	}
}

func TestPipeline_HostSlugFallback(t *testing.T) {
	// When remote is empty, DeriveHostSlug uses the worktree path as fallback.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	// Override meta to have no remote but a worktree path.
	meta.Git.Remote = nil
	worktree := "/home/test/testrepo"
	meta.Git.Worktree = &worktree
	meta.Project.FilePath = "/home/test/testrepo"

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		for _, sr := range result.Sessions {
			if sr.Error != nil {
				t.Logf("Session %s error: %v", sr.SessionID, sr.Error)
			}
		}
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// The session was processed; verify metadata exists under some host slug dir.
	entries, err := mfs.ReadDir(testOutputDir)
	if err != nil {
		t.Fatalf("ReadDir outputDir: %v", err)
	}
	found := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Skip .tmp- dirs.
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			continue
		}
		candidateMeta := fmt.Sprintf("%s/%s/%s/%s--metadata.json",
			testOutputDir, entry.Name(), testSessionID, testSessionID)
		if _, err := mfs.Stat(candidateMeta); err == nil {
			found = true
			break
		}
	}
	if !found {
		t.Error("metadata file not found under any host slug directory")
	}
}

func TestPipeline_DiscoverPartialFailure(t *testing.T) {
	// When one provider's Discover fails and another succeeds,
	// the pipeline should process the successful provider's sessions.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	// Claude provider succeeds.
	claudeFactory := makeStubAdapter(
		[]ingest.DiscoveredSession{session},
		map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
	)

	// OpenCode provider fails discovery.
	openCodeFactory := func(fs ingest.FileSystem, git ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
		return &testutil.StubAdapter{
			ProviderValue: ingest.HarnessOpenCode,
			DiscoverErr:   errors.New("opencode storage not found"),
		}
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: claudeFactory,
		ingest.HarnessOpenCode:   openCodeFactory,
	}

	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(testSourceDir)}},
			ingest.HarnessOpenCode:   {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath("/nonexistent")}},
		},
		OutputDir:          ingest.ResolvedPath(testOutputDir),
		StalenessThreshold: 5 * time.Minute,
	}

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run should not fail when one provider succeeds: %v", err)
	}

	// The Claude session should still be processed.
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1 (from Claude provider)", result.Summary.New)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	if result.Sessions[0].SessionID != session.SessionID {
		t.Errorf("Sessions[0].SessionID = %q, want %q", result.Sessions[0].SessionID, session.SessionID)
	}
}

func TestPipeline_DiscoverAllProvidersFail(t *testing.T) {
	// When ALL providers fail, the pipeline should return an error.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	claudeFactory := func(fs ingest.FileSystem, git ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
		return &testutil.StubAdapter{
			ProviderValue: ingest.HarnessClaudeCode,
			DiscoverErr:   errors.New("claude storage not found"),
		}
	}
	openCodeFactory := func(fs ingest.FileSystem, git ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
		return &testutil.StubAdapter{
			ProviderValue: ingest.HarnessOpenCode,
			DiscoverErr:   errors.New("opencode storage not found"),
		}
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: claudeFactory,
		ingest.HarnessOpenCode:   openCodeFactory,
	}

	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath("/nonexistent1")}},
			ingest.HarnessOpenCode:   {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath("/nonexistent2")}},
		},
		OutputDir:          ingest.ResolvedPath(testOutputDir),
		StalenessThreshold: 5 * time.Minute,
	}

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	_, err = pipeline.Run(context.Background())
	if err == nil {
		t.Fatal("Run should fail when all providers fail")
	}
	if !strings.Contains(err.Error(), "all providers failed") {
		t.Errorf("error = %q, want containing %q", err.Error(), "all providers failed")
	}
}

func TestPipeline_SubagentNesting(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Set up parent and child source files.
	parentSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	childSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testutil.TestSubagentID)
	setupSourceFile(t, mfs, parentSource)
	setupSourceFile(t, mfs, childSource)

	modTime := time.Now().Add(-1 * time.Hour)
	parentSession := makeDiscoveredSession(t, testSessionID, parentSource, modTime)

	// Child session with ParentUUID set.
	childSID, err := ingest.NewSessionID(testutil.TestSubagentID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", testutil.TestSubagentID, err)
	}
	parentSID := parentSession.SessionID
	childSession := ingest.DiscoveredSession{
		SessionID:     childSID,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(childSource),
		SourceFormat:  ingest.SourceFormatJSONL,
		ParentUUID:    &parentSID,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
		ModTime:       modTime,
	}

	parentMeta := makeMinimalMeta(t, testSessionID)
	childMeta := makeMinimalMeta(t, testutil.TestSubagentID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{parentSession, childSession},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				parentSession.SessionID: parentMeta,
				childSession.SessionID:  childMeta,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		for _, sr := range result.Sessions {
			if sr.Error != nil {
				t.Logf("Session %s error: %v", sr.SessionID, sr.Error)
			}
		}
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Parent should be at flat path: {hostSlug}/{parentID}/{parentID}--metadata.json
	parentBase := expectedOutputBase(testOutputDir, testSessionID)
	parentMetaPath := fmt.Sprintf("%s/%s--metadata.json", parentBase, testSessionID)
	if _, err := mfs.Stat(parentMetaPath); err != nil {
		t.Errorf("parent metadata not found at %q: %v", parentMetaPath, err)
	}

	// Child should be nested: {hostSlug}/{parentID}/subagents/{childID}/{childID}--metadata.json
	childMetaPath := fmt.Sprintf("%s/%s/%s/subagents/%s/%s--metadata.json",
		testOutputDir, testutil.TestHostSlug, testSessionID, testutil.TestSubagentID, testutil.TestSubagentID)
	if _, err := mfs.Stat(childMetaPath); err != nil {
		t.Errorf("child metadata not found at nested path %q: %v", childMetaPath, err)
	}
}

func TestPipeline_Force_IncludeActive_ActiveSession(t *testing.T) {
	// When Force=true AND IncludeActive=true AND the session is active,
	// the session should be processed (not skipped).
	// classifySession returns DiffActive for active+force, but the filter
	// lets it through because IncludeActive=true.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	// ModTime within staleness threshold (active session).
	recentModTime := time.Now().Add(-30 * time.Second)
	session := makeDiscoveredSession(t, testSessionID, sourcePath, recentModTime)
	mfs.ModTimes[sourcePath] = recentModTime

	meta := makeMinimalMeta(t, testSessionID)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Force = true
		c.IncludeActive = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	sr := result.Sessions[0]

	// Status is DiffActive (Force+active → DiffActive in classifySession).
	if sr.Status != ingest.DiffActive {
		t.Errorf("Sessions[0].Status = %v, want DiffActive", sr.Status)
	}

	// Session should be processed (no error, output path set).
	if sr.Error != nil {
		t.Errorf("Sessions[0].Error = %v, want nil", sr.Error)
	}
	if sr.OutputPath == "" {
		t.Error("Sessions[0].OutputPath is empty, want non-empty (session should be processed)")
	}

	// Verify metadata was written.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	if _, err := mfs.Stat(metaPath); err != nil {
		t.Errorf("metadata not found at %q: %v", metaPath, err)
	}
}

func TestPipeline_SchemaVersionUpgrade_DiffUpdated(t *testing.T) {
	// When existing metadata has SchemaVersion < CurrentSchemaVersion,
	// the session should be classified as DiffUpdated on re-run.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, mfs, sourcePath)
	mfs.ModTimes[sourcePath] = modTime

	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)

	// First run: ingest as new.
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result1, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("First Run: %v", err)
	}
	if result1.Summary.New != 1 {
		t.Errorf("First run Summary.New = %d, want 1", result1.Summary.New)
	}

	// Modify the written metadata to have SchemaVersion: 0 (stale).
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}
	var written ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}
	written.SchemaVersion = 0
	updatedData, err := json.Marshal(written)
	if err != nil {
		t.Fatalf("Marshal metadata: %v", err)
	}
	if err := mfs.WriteFile(metaPath, updatedData, 0600); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}

	// Second run: should detect schema version mismatch → DiffUpdated.
	pipeline2, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result2, err := pipeline2.Run(context.Background())
	if err != nil {
		t.Fatalf("Second Run: %v", err)
	}
	if result2.Summary.Updated != 1 {
		t.Errorf("Second run Summary.Updated = %d, want 1", result2.Summary.Updated)
	}
	if result2.Summary.New != 0 {
		t.Errorf("Second run Summary.New = %d, want 0", result2.Summary.New)
	}
}

func TestPipeline_DebugFilesCopied(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	// Create debug files.
	debugFile1 := fmt.Sprintf("%s/debug/tool_output_1.json", testSourceDir)
	debugFile2 := fmt.Sprintf("%s/debug/tool_output_2.json", testSourceDir)
	if err := mfs.WriteFile(debugFile1, []byte(`{"output":"test1"}`), 0644); err != nil {
		t.Fatalf("WriteFile debug1: %v", err)
	}
	if err := mfs.WriteFile(debugFile2, []byte(`{"output":"test2"}`), 0644); err != nil {
		t.Fatalf("WriteFile debug2: %v", err)
	}

	modTime := time.Now().Add(-1 * time.Hour)
	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	session.DebugPaths = []ingest.ResolvedPath{
		ingest.ResolvedPath(debugFile1),
		ingest.ResolvedPath(debugFile2),
	}

	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		for _, sr := range result.Sessions {
			if sr.Error != nil {
				t.Logf("Session %s error: %v", sr.SessionID, sr.Error)
			}
		}
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Verify debug files are at {sessionDir}/debug/{filename}.
	base := expectedOutputBase(testOutputDir, testSessionID)
	debugPath1 := fmt.Sprintf("%s/debug/tool_output_1.json", base)
	debugPath2 := fmt.Sprintf("%s/debug/tool_output_2.json", base)

	if _, err := mfs.Stat(debugPath1); err != nil {
		t.Errorf("debug file not found at %q: %v", debugPath1, err)
	}
	if _, err := mfs.Stat(debugPath2); err != nil {
		t.Errorf("debug file not found at %q: %v", debugPath2, err)
	}

	// Verify contents are preserved.
	data, err := mfs.ReadFile(debugPath1)
	if err != nil {
		t.Fatalf("ReadFile debug1: %v", err)
	}
	if string(data) != `{"output":"test1"}` {
		t.Errorf("debug file 1 content = %q, want %q", string(data), `{"output":"test1"}`)
	}
}

func TestNewPipeline_EmptyAdapters(t *testing.T) {
	_, err := ingest.NewPipeline(&testutil.MemFS{}, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{}, ingest.PipelineConfig{})
	if err == nil {
		t.Fatal("NewPipeline with empty adapters: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "adapters map must not be empty") {
		t.Errorf("error = %q, want containing %q", err.Error(), "adapters map must not be empty")
	}
}

// --- SessionStore integration tests ---

func TestPipeline_WithStore_InsertsAfterWrite(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	store := &testutil.StubSessionStore{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Pipeline should succeed with 1 new session.
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}
	if result.Summary.StoreError != nil {
		t.Errorf("Summary.StoreError = %v, want nil (happy path)", result.Summary.StoreError)
	}

	// Store should have received exactly 1 entry.
	if len(store.InsertedEntries) != 1 {
		t.Fatalf("InsertedEntries len = %d, want 1", len(store.InsertedEntries))
	}

	// Verify the inserted entry matches the processed session.
	inserted := store.InsertedEntries[0]
	if inserted.Metadata == nil {
		t.Fatal("InsertedEntries[0].Metadata is nil")
	}
	if inserted.Metadata.SessionID != session.SessionID {
		t.Errorf("InsertedEntries[0].Metadata.SessionID = %q, want %q", inserted.Metadata.SessionID, session.SessionID)
	}
	if inserted.Session.SessionID != session.SessionID {
		t.Errorf("InsertedEntries[0].Session.SessionID = %q, want %q", inserted.Session.SessionID, session.SessionID)
	}

	// Verify that the disk write also succeeded.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	if _, err := mfs.Stat(metaPath); err != nil {
		t.Errorf("metadata file not found at %q: %v", metaPath, err)
	}
}

func TestPipeline_WithStore_InsertError_NonFatal(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// Store that always fails on InsertSessions.
	store := &testutil.StubSessionStore{InsertErr: errors.New("db locked")}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	// Pipeline must succeed despite store failure.
	if err != nil {
		t.Fatalf("Run: %v (store failure should not propagate)", err)
	}

	// Disk writes must still succeed.
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0 (store error is non-fatal)", result.Summary.Errors)
	}

	// Store error must be surfaced in summary (not silently swallowed).
	if result.Summary.StoreError == nil {
		t.Errorf("Summary.StoreError = nil, want non-nil (store returned error)")
	}

	// Verify filesystem output exists.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	if _, err := mfs.Stat(metaPath); err != nil {
		t.Errorf("metadata file not found at %q: %v", metaPath, err)
	}
}

func TestPipeline_WithoutStore_SkipsDB(t *testing.T) {
	// This is essentially the existing behavior: no store, no DB interaction, no panic.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// No WithStore option — backward compatible path.
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Verify filesystem output exists (same as before store support).
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	if _, err := mfs.Stat(metaPath); err != nil {
		t.Errorf("metadata file not found at %q: %v", metaPath, err)
	}
}

func TestPipeline_WithStore_DryRun_SkipsDB(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	store := &testutil.StubSessionStore{}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.DryRun = true
	})
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// DryRun produces session results but writes nothing.
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	if result.Sessions[0].Status != ingest.DiffNew {
		t.Errorf("Sessions[0].Status = %v, want DiffNew", result.Sessions[0].Status)
	}

	// Store must NOT have received any entries in dry-run mode.
	if len(store.InsertedEntries) != 0 {
		t.Errorf("InsertedEntries len = %d, want 0 (dry-run should skip DB)", len(store.InsertedEntries))
	}
}

func TestPipeline_WithStore_MultipleSessionsInserted(t *testing.T) {
	// Verify that multiple successfully processed sessions are all inserted.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	source2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, source1)
	setupSourceFile(t, mfs, source2)

	modTime := time.Now().Add(-1 * time.Hour)
	session1 := makeDiscoveredSession(t, testSessionID, source1, modTime)
	session2 := makeDiscoveredSession(t, testSessionID2, source2, modTime)

	meta1 := makeMinimalMeta(t, testSessionID)
	meta2 := makeMinimalMeta(t, testSessionID2)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session1, session2},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session1.SessionID: meta1,
				session2.SessionID: meta2,
			},
		),
	}

	store := &testutil.StubSessionStore{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.New != 2 {
		t.Errorf("Summary.New = %d, want 2", result.Summary.New)
	}

	// Store should have received exactly 2 entries.
	if len(store.InsertedEntries) != 2 {
		t.Fatalf("InsertedEntries len = %d, want 2", len(store.InsertedEntries))
	}

	// Verify both session IDs appear in inserted entries.
	insertedIDs := make(map[ingest.SessionID]bool)
	for _, e := range store.InsertedEntries {
		insertedIDs[e.Metadata.SessionID] = true
	}
	if !insertedIDs[session1.SessionID] {
		t.Errorf("session1 (%s) not found in InsertedEntries", session1.SessionID)
	}
	if !insertedIDs[session2.SessionID] {
		t.Errorf("session2 (%s) not found in InsertedEntries", session2.SessionID)
	}
}

func TestPipeline_WithStore_ErrorSession_NotInserted(t *testing.T) {
	// When one session fails extraction, only the successful session is inserted into the store.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sid1, err := ingest.NewSessionID(testSessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	sid2, err := ingest.NewSessionID(testSessionID2)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	source2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, source1)
	setupSourceFile(t, mfs, source2)

	modTime := time.Now().Add(-1 * time.Hour)
	session1 := makeDiscoveredSession(t, testSessionID, source1, modTime)
	session2 := makeDiscoveredSession(t, testSessionID2, source2, modTime)

	meta2 := makeMinimalMeta(t, testSessionID2)

	// session1 fails extraction; session2 succeeds.
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapterWithErrors(
			[]ingest.DiscoveredSession{session1, session2},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				sid2: meta2,
			},
			map[ingest.SessionID]error{
				sid1: errors.New("extraction failed"),
			},
		),
	}

	store := &testutil.StubSessionStore{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 1 error, 1 success.
	if result.Summary.Errors != 1 {
		t.Errorf("Summary.Errors = %d, want 1", result.Summary.Errors)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Store should only have the successful session.
	if len(store.InsertedEntries) != 1 {
		t.Fatalf("InsertedEntries len = %d, want 1", len(store.InsertedEntries))
	}
	if store.InsertedEntries[0].Metadata.SessionID != sid2 {
		t.Errorf("InsertedEntries[0].Metadata.SessionID = %q, want %q", store.InsertedEntries[0].Metadata.SessionID, sid2)
	}
}

// --- v2 analytics stage tests ---

func TestPipeline_WithRedactor_RedactsMetadataOnDisk(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	redactor := &testutil.StubRedactor{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithRedactor(redactor))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Redactor should have been called once.
	if redactor.Called != 1 {
		t.Errorf("redactor.Called = %d, want 1", redactor.Called)
	}

	// Verify metadata on disk is redacted.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", metaPath, err)
	}

	var diskMeta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &diskMeta); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}

	// StubRedactor prefixes project name with "<REDACTED:...>".
	if !strings.Contains(diskMeta.Project.Name, "<REDACTED:") {
		t.Errorf("on-disk Project.Name = %q, want redacted (containing '<REDACTED:')", diskMeta.Project.Name)
	}
	if diskMeta.Project.FilePath != "<REDACTED>" {
		t.Errorf("on-disk Project.FilePath = %q, want '<REDACTED>'", diskMeta.Project.FilePath)
	}
}

// --- Pipeline v6 metadata field integration tests ---

// TestPipeline_WithRedactor_SetsRedactionInfo verifies that when a redactor is wired,
// the metadata on disk has RedactionInfo.Applied=true and a non-empty ContentHash.
func TestPipeline_WithRedactor_SetsRedactionInfo(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	redactor := &testutil.StubRedactor{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithRedactor(redactor))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", metaPath, err)
	}

	var diskMeta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &diskMeta); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}

	if !diskMeta.Redaction.Applied {
		t.Error("Redaction.Applied should be true when redactor is wired")
	}
	if diskMeta.Redaction.Level != "standard" {
		t.Errorf("Redaction.Level = %q, want %q", diskMeta.Redaction.Level, "standard")
	}
	if diskMeta.Redaction.RedactedAtMs == nil {
		t.Error("Redaction.RedactedAtMs should not be nil when redacted")
	}
	if diskMeta.ContentHash == "" {
		t.Error("ContentHash should not be empty")
	}
	if diskMeta.MetadataHash == "" {
		t.Error("MetadataHash should not be empty")
	}
	if diskMeta.Redaction.ContentHashAtRedact != diskMeta.ContentHash {
		t.Errorf("ContentHashAtRedact = %q, want ContentHash %q (should match for freshly redacted)",
			diskMeta.Redaction.ContentHashAtRedact, diskMeta.ContentHash)
	}
}

// TestPipeline_NoRedactor_SetsRedactionInfoRaw verifies that without a redactor,
// RedactionInfo.Applied=false and ContentHash/MetadataHash are still populated.
func TestPipeline_NoRedactor_SetsRedactionInfoRaw(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg) // no WithRedactor
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", metaPath, err)
	}

	var diskMeta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &diskMeta); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}

	if diskMeta.Redaction.Applied {
		t.Error("Redaction.Applied should be false when no redactor")
	}
	if diskMeta.Redaction.Level != "" {
		t.Errorf("Redaction.Level should be empty without redactor, got %q", diskMeta.Redaction.Level)
	}
	if diskMeta.ContentHash == "" {
		t.Error("ContentHash should not be empty even without redactor")
	}
	if diskMeta.MetadataHash == "" {
		t.Error("MetadataHash should not be empty even without redactor")
	}
}

// TestPipeline_ContentHash_Deterministic verifies that the same transcript bytes
// produce the same ContentHash across runs.
func TestPipeline_ContentHash_Deterministic(t *testing.T) {
	transcriptContent := `{"type":"user","content":"hello"}` + "\n"

	var hashes [2]string
	for i := range 2 {
		mfs := testutil.NewMemFS()
		git := testutil.DefaultGitResolver()
		sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
		if err := mfs.WriteFile(sourcePath, []byte(transcriptContent), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
		meta := makeMinimalMeta(t, testSessionID)
		adapters := map[ingest.Harness]ingest.AdapterFactory{
			ingest.HarnessClaudeCode: makeStubAdapter(
				[]ingest.DiscoveredSession{session},
				map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
			),
		}

		cfg := makePipelineConfig(testOutputDir)
		pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
		if err != nil {
			t.Fatalf("NewPipeline: %v", err)
		}

		if _, err := pipeline.Run(context.Background()); err != nil {
			t.Fatalf("Run[%d]: %v", i, err)
		}

		base := expectedOutputBase(testOutputDir, testSessionID)
		metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
		data, err := mfs.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}

		var diskMeta ingest.UnifiedMetadata
		if err := json.Unmarshal(data, &diskMeta); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		hashes[i] = diskMeta.ContentHash
	}

	if hashes[0] != hashes[1] {
		t.Errorf("ContentHash not deterministic: %q vs %q", hashes[0], hashes[1])
	}
	if len(hashes[0]) != 64 {
		t.Errorf("ContentHash should be 64-char hex, got len=%d", len(hashes[0]))
	}
}

// TestPipeline_RedactsTranscript_MultiLineJSONL verifies that every valid JSONL line
// in a multi-line transcript is redacted when a redactor is wired into the pipeline.
func TestPipeline_RedactsTranscript_MultiLineJSONL(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)

	// Write a 3-line JSONL transcript with known string values.
	multiLineJSONL := "" +
		`{"type":"user","content":"secret1"}` + "\n" +
		`{"type":"assistant","content":"secret2"}` + "\n" +
		`{"type":"user","content":"secret3"}` + "\n"
	if err := mfs.WriteFile(sourcePath, []byte(multiLineJSONL), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	redactor := &markingRedactor{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithRedactor(redactor))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// RedactJSON must have been called once per JSONL line (3 lines).
	if redactor.JSONCalled != 3 {
		t.Errorf("markingRedactor.JSONCalled = %d, want 3", redactor.JSONCalled)
	}

	// Verify on-disk transcript has redacted strings.
	base := expectedOutputBase(testOutputDir, testSessionID)
	transcriptPath := fmt.Sprintf("%s/%s--transcript.%s", base, testSessionID, string(ingest.SourceFormatJSONL))
	data, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", transcriptPath, err)
	}
	// Every string value should carry the REDACTED_ prefix.
	for _, want := range []string{"REDACTED_secret1", "REDACTED_secret2", "REDACTED_secret3"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("on-disk transcript missing %q; got:\n%s", want, data)
		}
	}
	// Original secrets must not appear unredacted.
	for _, original := range []string{`"secret1"`, `"secret2"`, `"secret3"`} {
		if strings.Contains(string(data), original) {
			t.Errorf("on-disk transcript still contains unredacted %q", original)
		}
	}
}

// TestPipeline_RedactsTranscript_UnparseableJSONLLinePassThrough verifies that an
// unparseable JSONL line passes through unchanged while the valid line is redacted.
func TestPipeline_RedactsTranscript_UnparseableJSONLLinePassThrough(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)

	validLine := `{"type":"user","content":"hello"}` + "\n"
	invalidLine := `NOT VALID JSON {{{{` + "\n"
	if err := mfs.WriteFile(sourcePath, []byte(validLine+invalidLine), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	redactor := &markingRedactor{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithRedactor(redactor))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// RedactJSON called only for the parseable line.
	if redactor.JSONCalled != 1 {
		t.Errorf("markingRedactor.JSONCalled = %d, want 1", redactor.JSONCalled)
	}

	base := expectedOutputBase(testOutputDir, testSessionID)
	transcriptPath := fmt.Sprintf("%s/%s--transcript.%s", base, testSessionID, string(ingest.SourceFormatJSONL))
	data, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", transcriptPath, err)
	}
	output := string(data)

	// Valid line must be redacted.
	if !strings.Contains(output, "REDACTED_hello") {
		t.Errorf("on-disk transcript: valid line not redacted; got:\n%s", output)
	}
	// Unparseable line must pass through verbatim.
	if !strings.Contains(output, "NOT VALID JSON {{{{") {
		t.Errorf("on-disk transcript: unparseable line not preserved; got:\n%s", output)
	}
}

// TestPipeline_RedactsTranscript_UnparseableJSONFilePassThrough verifies that a
// .json source that is not valid JSON passes through unchanged when a redactor is wired.
func TestPipeline_RedactsTranscript_UnparseableJSONFilePassThrough(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.json", testSourceDir, testSessionID)
	const badJSON = `NOT VALID JSON {{{{`
	if err := mfs.WriteFile(sourcePath, []byte(badJSON), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Use SourceFormatJSON so the pipeline routes to redactJSONDocBytes.
	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	session.SourceFormat = ingest.SourceFormatJSON
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	redactor := &markingRedactor{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithRedactor(redactor))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// RedactJSON must NOT have been called: parse failed before redaction.
	if redactor.JSONCalled != 0 {
		t.Errorf("markingRedactor.JSONCalled = %d, want 0 (parse failed, no redaction)", redactor.JSONCalled)
	}

	base := expectedOutputBase(testOutputDir, testSessionID)
	transcriptPath := fmt.Sprintf("%s/%s--transcript.%s", base, testSessionID, string(ingest.SourceFormatJSON))
	data, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", transcriptPath, err)
	}
	// File must be unchanged.
	if string(data) != badJSON {
		t.Errorf("on-disk transcript = %q, want %q (should pass through unchanged)", string(data), badJSON)
	}
}

// TestPipeline_WithRedactor_RedactsBothMetadataAndTranscript verifies that a single
// pipeline run with a redactor redacts BOTH metadata fields AND transcript content.
func TestPipeline_WithRedactor_RedactsBothMetadataAndTranscript(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	const transcriptContent = `{"type":"user","content":"private_data"}` + "\n"
	if err := mfs.WriteFile(sourcePath, []byte(transcriptContent), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	redactor := &markingRedactor{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithRedactor(redactor))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Both redaction methods must have been called.
	if redactor.MetadataCalled != 1 {
		t.Errorf("markingRedactor.MetadataCalled = %d, want 1", redactor.MetadataCalled)
	}
	if redactor.JSONCalled != 1 {
		t.Errorf("markingRedactor.JSONCalled = %d, want 1 (one JSONL line)", redactor.JSONCalled)
	}

	base := expectedOutputBase(testOutputDir, testSessionID)

	// Assert metadata is redacted on disk.
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	metaData, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", metaPath, err)
	}
	var diskMeta ingest.UnifiedMetadata
	if err := json.Unmarshal(metaData, &diskMeta); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}
	if !strings.HasPrefix(diskMeta.Project.Name, "REDACTED_") {
		t.Errorf("on-disk Project.Name = %q, want REDACTED_ prefix", diskMeta.Project.Name)
	}
	if diskMeta.Project.FilePath != "REDACTED_PATH" {
		t.Errorf("on-disk Project.FilePath = %q, want %q", diskMeta.Project.FilePath, "REDACTED_PATH")
	}

	// Assert transcript is redacted on disk.
	transcriptPath := fmt.Sprintf("%s/%s--transcript.%s", base, testSessionID, string(ingest.SourceFormatJSONL))
	transcriptData, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", transcriptPath, err)
	}
	if !strings.Contains(string(transcriptData), "REDACTED_private_data") {
		t.Errorf("on-disk transcript missing REDACTED_private_data; got:\n%s", transcriptData)
	}
	// Check that the bare JSON string "private_data" does not appear (only REDACTED_private_data should).
	if strings.Contains(string(transcriptData), `"private_data"`) {
		t.Errorf("on-disk transcript still contains unredacted private_data")
	}
}

func TestPipeline_WithIndexers_IndexesTranscripts(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)
	sid := session.SessionID

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
		),
	}

	// StubIndexer returns 3 entries for the session.
	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {
				{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
				{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText},
				{SessionID: sid, EntryIndex: 2, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeToolUse},
			},
		},
	}

	metricsStore := testutil.NewStubMetricsStore()

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1", result.Summary.Indexed)
	}

	// Verify entries were stored.
	entries, ok := metricsStore.IndexedEntries[sid]
	if !ok {
		t.Fatal("session not found in IndexedEntries")
	}
	if len(entries) != 3 {
		t.Errorf("IndexedEntries count = %d, want 3", len(entries))
	}
}

func TestPipeline_WithAnalyzer_ComputesMetrics(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)
	sid := session.SessionID

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
		),
	}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}
	metricsStore := testutil.NewStubMetricsStore()
	analyzer := &testutil.StubAnalyzer{ComputeCount: 1}
	store := &testutil.StubSessionStore{}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithStore(store),
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
		ingest.WithAnalyzer(analyzer),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1", result.Summary.Indexed)
	}
	if result.Summary.Computed != 1 {
		t.Errorf("Summary.Computed = %d, want 1", result.Summary.Computed)
	}

	// Verify analyzer received the session ID.
	if len(analyzer.ComputedSessionIDs) != 1 {
		t.Fatalf("ComputedSessionIDs len = %d, want 1", len(analyzer.ComputedSessionIDs))
	}
	if analyzer.ComputedSessionIDs[0] != sid {
		t.Errorf("ComputedSessionIDs[0] = %q, want %q", analyzer.ComputedSessionIDs[0], sid)
	}

	// Verify insights were computed for the affected day.
	if len(analyzer.InsightDays) == 0 {
		t.Error("InsightDays is empty, expected at least 1 day")
	}
}

func TestPipeline_WithClassifier_AnnotatesNewSessions(t *testing.T) {
	t.Parallel()
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)
	sid := session.SessionID

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
		),
	}

	// Indexer produces one entry so the session lands in successfullyIndexed.
	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}
	metricsStore := testutil.NewStubMetricsStore()
	classifier := &testutil.StubSessionClassifier{}
	store := &testutil.StubSessionStore{}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithStore(store),
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
		ingest.WithClassifier(classifier),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Classifier must have been called exactly once, for the ingested session.
	// pipeline.Run has returned; no concurrent access remains.
	if len(classifier.Annotated) != 1 {
		t.Fatalf("classifier.Annotated len = %d, want 1", len(classifier.Annotated))
	}
	if classifier.Annotated[0] != sid {
		t.Errorf("classifier.Annotated[0] = %q, want %q", classifier.Annotated[0], sid)
	}
}

type recordingBufferedClassifier struct {
	mu        sync.Mutex
	annotated []ingest.SessionID
	prepared  []ingest.SessionID
	flushes   [][]ingest.SessionID
}

var _ ingest.BufferedSessionClassifier = (*recordingBufferedClassifier)(nil)

func (c *recordingBufferedClassifier) Annotate(_ context.Context, sessionID ingest.SessionID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.annotated = append(c.annotated, sessionID)
	return nil
}

func (c *recordingBufferedClassifier) PrepareAnnotations(_ context.Context, sessionID ingest.SessionID, _ *ingest.IndexProfiler) (ingest.SessionAnnotationBatch, error) {
	c.mu.Lock()
	c.prepared = append(c.prepared, sessionID)
	c.mu.Unlock()
	return ingest.SessionAnnotationBatch{
		SessionID: sessionID,
		Writes: []ingest.SessionAnnotationWrite{{
			TypeID:     "test.annotation",
			Value:      "prepared",
			TargetKind: ingest.AnnotationProfileTargetSession,
		}},
	}, nil
}

func (c *recordingBufferedClassifier) FlushAnnotationBatches(_ context.Context, batches []ingest.SessionAnnotationBatch, _ *ingest.IndexProfiler) []ingest.SessionAnnotationBatchResult {
	ids := make([]ingest.SessionID, len(batches))
	results := make([]ingest.SessionAnnotationBatchResult, len(batches))
	for i, batch := range batches {
		ids[i] = batch.SessionID
		results[i] = ingest.SessionAnnotationBatchResult{
			SessionID: batch.SessionID,
			Results: []ingest.ClassifierAnnotationWriteResult{{
				Dedup:        ingest.DedupCreate,
				AnnotationID: "buffered-annotation",
			}},
		}
	}
	c.mu.Lock()
	c.flushes = append(c.flushes, ids)
	c.mu.Unlock()
	return results
}

func TestPipeline_WithBufferedClassifier_FlushesPreparedSessions(t *testing.T) {
	t.Parallel()
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePathA := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	sourcePathB := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, sourcePathA)
	setupSourceFile(t, mfs, sourcePathB)

	sessionA := makeDiscoveredSession(t, testSessionID, sourcePathA, time.Now().Add(-1*time.Hour))
	sessionB := makeDiscoveredSession(t, testSessionID2, sourcePathB, time.Now().Add(-1*time.Hour))
	sidA := sessionA.SessionID
	sidB := sessionB.SessionID
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{sessionA, sessionB},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				sidA: makeMinimalMeta(t, testSessionID),
				sidB: makeMinimalMeta(t, testSessionID2),
			},
		),
	}
	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sidA: {{SessionID: sidA, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
			sidB: {{SessionID: sidB, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}
	classifier := &recordingBufferedClassifier{}

	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Parallelism = 2
	})
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithStore(&testutil.StubSessionStore{}),
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(testutil.NewStubMetricsStore()),
		ingest.WithClassifier(classifier),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if _, err := pipeline.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	classifier.mu.Lock()
	defer classifier.mu.Unlock()
	if len(classifier.annotated) != 0 {
		t.Fatalf("Annotate calls = %d, want 0 buffered path calls", len(classifier.annotated))
	}
	if len(classifier.prepared) != 2 {
		t.Fatalf("PrepareAnnotations calls = %d, want 2", len(classifier.prepared))
	}
	if len(classifier.flushes) != 1 || len(classifier.flushes[0]) != 2 {
		t.Fatalf("flushes = %+v, want one flush containing both sessions", classifier.flushes)
	}
}

func TestPipeline_IndexError_NonFatal(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)
	sid := session.SessionID

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
		),
	}

	// Indexer that always fails.
	indexer := &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile, Err: errors.New("parse failed")}
	metricsStore := testutil.NewStubMetricsStore()

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	// Pipeline must succeed despite indexer failure.
	if err != nil {
		t.Fatalf("Run: %v (indexer failure should not propagate)", err)
	}

	// Session should still be counted as processed.
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0 (index error is non-fatal)", result.Summary.Errors)
	}
	// But indexed count should be 0.
	if result.Summary.Indexed != 0 {
		t.Errorf("Summary.Indexed = %d, want 0 (indexer failed)", result.Summary.Indexed)
	}

	// Disk write should still succeed.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	if _, err := mfs.Stat(metaPath); err != nil {
		t.Errorf("metadata file not found at %q: %v", metaPath, err)
	}
}

func TestPipeline_ComputeError_NonFatal(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)
	sid := session.SessionID

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
		),
	}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}
	metricsStore := testutil.NewStubMetricsStore()
	// Analyzer that always fails.
	analyzer := &testutil.StubAnalyzer{ComputeErr: errors.New("compute failed")}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
		ingest.WithAnalyzer(analyzer),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	// Pipeline must succeed despite compute failure.
	if err != nil {
		t.Fatalf("Run: %v (compute failure should not propagate)", err)
	}

	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	// Indexing should still succeed.
	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1", result.Summary.Indexed)
	}
	// Computed should be 0 (error).
	if result.Summary.Computed != 0 {
		t.Errorf("Summary.Computed = %d, want 0 (compute failed)", result.Summary.Computed)
	}
}

func TestPipeline_NilOptionalDeps_SkipsNewStages(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// No optional deps — backward-compatible mode.
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	// No indexing or compute should occur.
	if result.Summary.Indexed != 0 {
		t.Errorf("Summary.Indexed = %d, want 0 (no indexers configured)", result.Summary.Indexed)
	}
	if result.Summary.Computed != 0 {
		t.Errorf("Summary.Computed = %d, want 0 (no analyzer configured)", result.Summary.Computed)
	}

	// Disk write should still work.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	if _, err := mfs.Stat(metaPath); err != nil {
		t.Errorf("metadata file not found at %q: %v", metaPath, err)
	}
}

// TestPipeline_WithAnalyzer_NoWithStore verifies that ComputeInsights is called
// with the correct days even when WithStore is NOT configured (p.store == nil).
// This is the regression test for Fix A (day source) and Fix B (gate condition).
func TestPipeline_WithAnalyzer_NoWithStore(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)
	// meta.Timestamp.Start is set to 1708300800000 (2024-02-19T00:00:00Z) by makeMinimalMeta.
	sid := session.SessionID

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
		),
	}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}
	metricsStore := testutil.NewStubMetricsStore()
	analyzer := &testutil.StubAnalyzer{ComputeCount: 1}

	// Deliberately NOT using WithStore — p.store will be nil.
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
		ingest.WithAnalyzer(analyzer),
		// No WithStore
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Pipeline should succeed.
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}
	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1", result.Summary.Indexed)
	}
	if result.Summary.Computed != 1 {
		t.Errorf("Summary.Computed = %d, want 1", result.Summary.Computed)
	}

	// Fix C: ComputeMetrics must receive only the successfully-indexed session.
	if len(analyzer.ComputedSessionIDs) != 1 {
		t.Fatalf("ComputedSessionIDs len = %d, want 1", len(analyzer.ComputedSessionIDs))
	}
	if analyzer.ComputedSessionIDs[0] != sid {
		t.Errorf("ComputedSessionIDs[0] = %q, want %q", analyzer.ComputedSessionIDs[0], sid)
	}

	// Fix A: ComputeInsights must be called with the correct day derived from
	// session metadata (not storeEntries, which is empty when p.store == nil).
	if len(analyzer.InsightDays) == 0 {
		t.Fatal("InsightDays is empty; ComputeInsights was not called (Fix A regression)")
	}
	// meta.Timestamp.Start = 1708300800000 ms → 2024-02-19 UTC.
	wantDay := "2024-02-19"
	found := false
	for _, d := range analyzer.InsightDays {
		if d == wantDay {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("InsightDays = %v, want to contain %q", analyzer.InsightDays, wantDay)
	}
}

// TestPipeline_WithAnalyzer_IndexStoreError_NotPassedToCompute verifies that a
// session whose metricsStore.IndexSessionEntries call fails is NOT included in
// the session IDs passed to ComputeMetrics (Fix C).
func TestPipeline_WithAnalyzer_IndexStoreError_NotPassedToCompute(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sid1, err := ingest.NewSessionID(testSessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	sid2, err := ingest.NewSessionID(testSessionID2)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	source2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, source1)
	setupSourceFile(t, mfs, source2)

	modTime := time.Now().Add(-1 * time.Hour)
	session1 := makeDiscoveredSession(t, testSessionID, source1, modTime)
	session2 := makeDiscoveredSession(t, testSessionID2, source2, modTime)

	meta1 := makeMinimalMeta(t, testSessionID)
	meta2 := makeMinimalMeta(t, testSessionID2)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session1, session2},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				sid1: meta1,
				sid2: meta2,
			},
		),
	}

	// Both sessions get entries from the indexer.
	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid1: {{SessionID: sid1, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
			sid2: {{SessionID: sid2, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	// metricsStore that fails IndexSessionEntries for sid1.
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.IndexErr = errors.New("storage full")

	analyzer := &testutil.StubAnalyzer{}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
		ingest.WithAnalyzer(analyzer),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Both disk writes should succeed.
	if result.Summary.New != 2 {
		t.Errorf("Summary.New = %d, want 2", result.Summary.New)
	}
	// But indexing fails for both (IndexErr applies globally in StubMetricsStore).
	if result.Summary.Indexed != 0 {
		t.Errorf("Summary.Indexed = %d, want 0 (metricsStore.IndexSessionEntries fails)", result.Summary.Indexed)
	}

	// ComputeMetrics should receive an empty list (no successfully indexed sessions).
	if len(analyzer.ComputedSessionIDs) != 0 {
		t.Errorf("ComputedSessionIDs = %v, want empty (no sessions indexed successfully)", analyzer.ComputedSessionIDs)
	}
}

// --- AUDIT stage tests ---

func TestPipeline_WithLogger_WritesAuditLog(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	logger := &testutil.StubIngestLogger{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithLogger(logger))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Logger should have been called once.
	if len(logger.Entries) != 1 {
		t.Fatalf("logger.Entries len = %d, want 1", len(logger.Entries))
	}

	entry := logger.Entries[0]
	if entry.SessionsNew != 1 {
		t.Errorf("log entry SessionsNew = %d, want 1", entry.SessionsNew)
	}
	if entry.StartedAt == 0 {
		t.Error("log entry StartedAt is 0, want non-zero")
	}
	if entry.FinishedAt == nil || *entry.FinishedAt == 0 {
		t.Error("log entry FinishedAt is nil or 0, want non-zero")
	}
	if entry.FinishedAt != nil && *entry.FinishedAt < entry.StartedAt {
		t.Errorf("log entry FinishedAt (%d) < StartedAt (%d)", *entry.FinishedAt, entry.StartedAt)
	}
}

func TestPipeline_LoggerError_NonFatal(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// Logger that always fails.
	logger := &testutil.StubIngestLogger{Err: errors.New("log write failed")}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithLogger(logger))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	// Pipeline must succeed despite logger failure.
	if err != nil {
		t.Fatalf("Run: %v (logger failure should not propagate)", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
}

// --- Transcript redaction integration tests (T3) ---

// wrappingRedactor is a test-local TextRedactor that wraps every string value
// with "[REDACTED]" so tests can assert that redaction was applied to transcripts.
// RedactMetadata delegates to testutil.StubRedactor for metadata assertions.
type wrappingRedactor struct {
	stub *testutil.StubRedactor
}

func (m *wrappingRedactor) RedactMetadata(meta *ingest.UnifiedMetadata) *ingest.UnifiedMetadata {
	return m.stub.RedactMetadata(meta)
}

func (m *wrappingRedactor) RedactJSON(value any) any {
	switch v := value.(type) {
	case string:
		return "[REDACTED]" + v + "[REDACTED]"
	case []any:
		result := make([]any, len(v))
		for i, elem := range v {
			result[i] = m.RedactJSON(elem)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, val := range v {
			result[key] = m.RedactJSON(val)
		}
		return result
	default:
		return value
	}
}

func (m *wrappingRedactor) Level() string {
	return "standard"
}

func (m *wrappingRedactor) RuleSetVersion() string {
	return "1.0.0"
}

// TestPipeline_WithRedactor_RedactsJSONLTranscriptOnDisk verifies that when a
// TextRedactor is wired, the JSONL transcript written to disk has its string
// values redacted (T3, SourceFormatJSONL path).
func TestPipeline_WithRedactor_RedactsJSONLTranscriptOnDisk(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	// Write a JSONL transcript containing a known sentinel string.
	const sentinel = "hello-world-sentinel"
	jsonlContent := fmt.Sprintf(`{"type":"user","content":%q}`+"\n", sentinel)
	if err := mfs.WriteFile(sourcePath, []byte(jsonlContent), 0644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	redactor := &wrappingRedactor{stub: &testutil.StubRedactor{}}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithRedactor(redactor))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Read the written transcript from MemFS.
	base := expectedOutputBase(testOutputDir, testSessionID)
	transcriptPath := fmt.Sprintf("%s/%s--transcript.jsonl", base, testSessionID)
	diskData, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("ReadFile transcript: %v", err)
	}

	// The sentinel string must appear wrapped with the redaction marker,
	// and must NOT appear unwrapped (raw) in the transcript.
	diskStr := string(diskData)
	if !strings.Contains(diskStr, "[REDACTED]"+sentinel+"[REDACTED]") {
		t.Errorf("transcript on disk does not contain redacted sentinel: got %q", diskStr)
	}
	// Bare sentinel should not appear in the transcript — redaction must have replaced it.
	if strings.Contains(diskStr, `"`+sentinel+`"`) {
		t.Errorf("transcript on disk still contains unredacted sentinel: got %q", diskStr)
	}
}

// TestPipeline_WithRedactor_RedactsJSONTranscriptOnDisk verifies that when a
// TextRedactor is wired, the JSON transcript written to disk has its string
// values redacted (T3, SourceFormatJSON path).
func TestPipeline_WithRedactor_RedactsJSONTranscriptOnDisk(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sid, err := ingest.NewSessionID(testSessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	sourcePath := fmt.Sprintf("%s/%s.json", testSourceDir, testSessionID)
	// Write a JSON transcript containing a known sentinel string.
	const sentinel = "json-sentinel-value"
	jsonContent := fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, sentinel)
	if err := mfs.WriteFile(sourcePath, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(sourcePath),
		SourceFormat:  ingest.SourceFormatJSON,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
		ModTime:       time.Now().Add(-1 * time.Hour),
	}
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	redactor := &wrappingRedactor{stub: &testutil.StubRedactor{}}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithRedactor(redactor))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Read the written transcript from MemFS.
	base := expectedOutputBase(testOutputDir, testSessionID)
	transcriptPath := fmt.Sprintf("%s/%s--transcript.json", base, testSessionID)
	diskData, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("ReadFile transcript: %v", err)
	}

	diskStr := string(diskData)
	if !strings.Contains(diskStr, "[REDACTED]"+sentinel+"[REDACTED]") {
		t.Errorf("transcript on disk does not contain redacted sentinel: got %q", diskStr)
	}
	if strings.Contains(diskStr, `"`+sentinel+`"`) {
		t.Errorf("transcript on disk still contains unredacted sentinel: got %q", diskStr)
	}
}

// TestPipeline_WithNilRedactor_TranscriptCopiedVerbatim verifies that when no
// redactor is wired, the transcript is written verbatim (backward compatibility).
func TestPipeline_WithNilRedactor_TranscriptCopiedVerbatim(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	const originalContent = `{"type":"user","content":"unchanged"}` + "\n"
	if err := mfs.WriteFile(sourcePath, []byte(originalContent), 0644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// No WithRedactor option: redactor is nil.
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Transcript on disk must equal the original bytes exactly.
	base := expectedOutputBase(testOutputDir, testSessionID)
	transcriptPath := fmt.Sprintf("%s/%s--transcript.jsonl", base, testSessionID)
	diskData, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("ReadFile transcript: %v", err)
	}
	if string(diskData) != originalContent {
		t.Errorf("transcript on disk = %q, want %q", string(diskData), originalContent)
	}
}

// TestPipeline_WithRedactor_CustomPattern verifies that user-defined patterns wired via
// NewRedactor(level, userPatterns) + WithRedactor(r) redact matching content in JSONL
// transcripts written to disk. This is the end-to-end integration test for redaction output.
func TestPipeline_WithRedactor_CustomPattern(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	// Write a JSONL transcript containing the word "SECRET" which will be matched
	// by the user-defined pattern below.
	const secretWord = "SECRET"
	const replacement = "<REDACTED>"
	jsonlContent := fmt.Sprintf(`{"type":"user","content":"my %s is here"}`+"\n", secretWord)
	if err := mfs.WriteFile(sourcePath, []byte(jsonlContent), 0644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// Build a real DefaultRedactor with a user pattern that matches "SECRET".
	userPatterns := []redact.UserPattern{
		{
			ID:          "test-pat",
			Category:    redact.CategorySecrets,
			Pattern:     secretWord,
			Replacement: replacement,
		},
	}
	r, err := redact.NewRedactor(redact.Minimal, userPatterns, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithRedactor(r))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Read the written JSONL transcript from MemFS.
	base := expectedOutputBase(testOutputDir, testSessionID)
	transcriptPath := fmt.Sprintf("%s/%s--transcript.jsonl", base, testSessionID)
	diskData, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("ReadFile transcript: %v", err)
	}

	diskStr := string(diskData)
	// The transcript is JSONL: values are JSON-encoded, so angle brackets may appear
	// as Unicode escapes (\u003c, \u003e) due to Go's json.Encoder HTML-escaping.
	// Check that "REDACTED" (the meaningful part of the replacement) appears in the output,
	// and that the original secret word does NOT appear.
	if !strings.Contains(diskStr, "REDACTED") {
		t.Errorf("transcript on disk does not contain REDACTED (replacement): got %q", diskStr)
	}
	// The original secret word must NOT appear in the transcript.
	if strings.Contains(diskStr, secretWord) {
		t.Errorf("transcript on disk still contains secret word %q: got %q", secretWord, diskStr)
	}
}

// TestPipeline_InsertSessionsDoesNotRecomputeSummary verifies that InsertSessions
// (via StubSessionStore) and ComputeInsights (via StubAnalyzer) are decoupled stages.
// InsertSessions alone does not trigger daily_summary recomputation.
func TestPipeline_InsertSessionsDoesNotRecomputeSummary(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	sessionStore := &testutil.StubSessionStore{}
	analyzer := &testutil.StubAnalyzer{ComputeCount: 1}

	cfg := makePipelineConfig(testOutputDir)

	// Run with both WithStore and WithAnalyzer to observe both stages.
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithStore(sessionStore),
		ingest.WithAnalyzer(analyzer),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify InsertSessions was called (store received entries).
	if len(sessionStore.InsertedEntries) == 0 {
		t.Fatal("InsertedEntries is empty — InsertSessions was not called")
	}

	// Verify ComputeInsights was called independently (analyzer received days).
	// The key assertion: InsightDays is populated by the COMPUTE stage, not INSERT.
	// If InsertSessions were coupled to daily_summary, we would see duplicate days
	// or the analyzer wouldn't need to be called at all.
	if len(analyzer.InsightDays) == 0 {
		t.Error("InsightDays is empty — ComputeInsights was not called (decoupled stage)")
	}

	// Verify the pipeline completed successfully.
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Now run WITHOUT WithAnalyzer — InsertSessions alone must not recompute summaries.
	mfs2 := testutil.NewMemFS()
	sourcePath2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs2, sourcePath2)
	sessionStore2 := &testutil.StubSessionStore{}

	pipeline2, err := ingest.NewPipeline(mfs2, git, adapters, cfg,
		ingest.WithStore(sessionStore2),
		// Deliberately NO WithAnalyzer
	)
	if err != nil {
		t.Fatalf("NewPipeline (no analyzer): %v", err)
	}

	result2, err := pipeline2.Run(context.Background())
	if err != nil {
		t.Fatalf("Run (no analyzer): %v", err)
	}

	// InsertSessions happened.
	if len(sessionStore2.InsertedEntries) == 0 {
		t.Fatal("InsertedEntries is empty (no analyzer run)")
	}
	// Pipeline succeeded without analyzer — no daily_summary computed.
	if result2.Summary.Computed != 0 {
		t.Errorf("Summary.Computed = %d, want 0 (no analyzer)", result2.Summary.Computed)
	}
}

// --- Reindex mode tests ---

// setupPeasantSyncSession writes metadata and transcript files to the MemFS at the
// expected peasant-sync output layout: {outputDir}/{hostSlug}/{sessionID}/{sessionID}--metadata.json
// and {outputDir}/{hostSlug}/{sessionID}/{sessionID}--transcript.{ext}.
// Returns the metadata path and transcript path.
func setupPeasantSyncSession(t *testing.T, mfs *testutil.MemFS, outputDir, hostSlug, sessionIDStr string, meta *ingest.UnifiedMetadata) (string, string) {
	t.Helper()

	sessionDir := fmt.Sprintf("%s/%s/%s", outputDir, hostSlug, sessionIDStr)
	metaFilename := fmt.Sprintf("%s--metadata.json", sessionIDStr)
	metaPath := fmt.Sprintf("%s/%s", sessionDir, metaFilename)

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := mfs.WriteFile(metaPath, metaJSON, 0644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	ext := string(meta.Source.Format)
	if ext == "" {
		ext = string(ingest.SourceFormatJSONL)
	}
	transcriptFilename := fmt.Sprintf("%s--transcript.%s", sessionIDStr, ext)
	transcriptPath := fmt.Sprintf("%s/%s", sessionDir, transcriptFilename)
	content := []byte(`{"type":"user","content":"hello"}` + "\n")
	if err := mfs.WriteFile(transcriptPath, content, 0644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	return metaPath, transcriptPath
}

// makeReindexMeta creates a UnifiedMetadata with Source.FilePath set for reindex testing.
func makeReindexMeta(t *testing.T, sessionIDStr, originalSourcePath string) *ingest.UnifiedMetadata {
	t.Helper()
	meta := makeMinimalMeta(t, sessionIDStr)
	meta.Source = ingest.SourceInfo{
		FilePath: originalSourcePath,
		Format:   ingest.SourceFormatJSONL,
	}
	return meta
}

// TestPipeline_Reindex_DiscoversSessions verifies that --reindex scans the
// peasant-sync output directory and discovers sessions from metadata files.
func TestPipeline_Reindex_DiscoversSessions(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	originalSourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	meta := makeReindexMeta(t, testSessionID, originalSourcePath)

	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta)

	// Mark session as stale in the metrics store.
	metricsStore := testutil.NewStubMetricsStore()
	sid, _ := ingest.NewSessionID(testSessionID)
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {
				{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
			},
		},
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Reindex should discover and process the session.
	if result.Summary.Updated != 1 {
		t.Errorf("Summary.Updated = %d, want 1", result.Summary.Updated)
	}
	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1", result.Summary.Indexed)
	}
}

// TestPipeline_Reindex_ReExtractsWhenSourceExists verifies that when the original
// source file exists, reindex re-extracts metadata from it (EXTRACT+WRITE path).
func TestPipeline_Reindex_ReExtractsWhenSourceExists(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	originalSourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	// Write the original source file so reindex can re-extract.
	setupSourceFile(t, mfs, originalSourcePath)

	meta := makeReindexMeta(t, testSessionID, originalSourcePath)
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta)

	sid, _ := ingest.NewSessionID(testSessionID)
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			nil,
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
		),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Should re-extract: session results have an OutputPath (from processSession).
	if result.Summary.Updated != 1 {
		t.Errorf("Summary.Updated = %d, want 1", result.Summary.Updated)
	}
	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1", result.Summary.Indexed)
	}

	// Verify the session was written to disk (processSession succeeded).
	found := false
	for _, sr := range result.Sessions {
		if sr.SessionID == sid && sr.OutputPath != "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected session to have OutputPath (re-extracted from source)")
	}
}

// TestPipeline_Reindex_FallbackWhenSourceMissing verifies that when the original
// source file is missing, reindex falls back to INDEX+COMPUTE from the existing
// peasant-sync transcript and records a fallback IndexLogEntry.
func TestPipeline_Reindex_FallbackWhenSourceMissing(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// The original source path does NOT exist in MemFS.
	originalSourcePath := "/nonexistent/source.jsonl"
	meta := makeReindexMeta(t, testSessionID, originalSourcePath)
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta)

	sid, _ := ingest.NewSessionID(testSessionID)
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	indexLogger := &testutil.StubIndexLogger{}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
		ingest.WithIndexLogger(indexLogger),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Should fall back to INDEX+COMPUTE, session still processed.
	if result.Summary.Updated != 1 {
		t.Errorf("Summary.Updated = %d, want 1", result.Summary.Updated)
	}
	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1", result.Summary.Indexed)
	}

	// Verify IndexLog contains a fallback entry.
	foundFallback := false
	for _, entry := range result.IndexLog {
		if entry.SessionID == sid && entry.Outcome == ingest.IndexOutcomeFallback {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Error("expected IndexLog to contain a fallback entry for missing source")
	}

	// Also verify the index logger received the fallback entry.
	if len(indexLogger.Entries) == 0 {
		t.Fatal("indexLogger.Entries is empty")
	}
	loggedFallback := false
	for _, entry := range indexLogger.Entries {
		if entry.Outcome == ingest.IndexOutcomeFallback {
			loggedFallback = true
			break
		}
	}
	if !loggedFallback {
		t.Error("expected indexLogger to have logged a fallback entry")
	}
}

// TestPipeline_Reindex_ForceTargetsAll verifies that --reindex --force targets
// ALL sessions in peasant-sync, not just those with stale index_version.
func TestPipeline_Reindex_ForceTargetsAll(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Set up two sessions in peasant-sync.
	meta1 := makeReindexMeta(t, testSessionID, "/nonexistent/1.jsonl")
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta1)

	meta2 := makeReindexMeta(t, testSessionID2, "/nonexistent/2.jsonl")
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID2, meta2)

	sid1, _ := ingest.NewSessionID(testSessionID)
	sid2, _ := ingest.NewSessionID(testSessionID2)

	// Only mark sid1 as stale. sid2 is NOT stale.
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid1}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid1: {{SessionID: sid1, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
			sid2: {{SessionID: sid2, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}

	// Without --force: only stale sessions targeted.
	cfgNoForce := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
	})
	pipelineNoForce, err := ingest.NewPipeline(mfs, git, adapters, cfgNoForce,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline (no force): %v", err)
	}

	resultNoForce, err := pipelineNoForce.Run(context.Background())
	if err != nil {
		t.Fatalf("Run (no force): %v", err)
	}
	if resultNoForce.Summary.Updated != 1 {
		t.Errorf("no-force: Summary.Updated = %d, want 1 (only stale session)", resultNoForce.Summary.Updated)
	}

	// With --force: ALL sessions targeted.
	cfgForce := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
		c.Force = true
	})
	pipelineForce, err := ingest.NewPipeline(mfs, git, adapters, cfgForce,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline (force): %v", err)
	}

	resultForce, err := pipelineForce.Run(context.Background())
	if err != nil {
		t.Fatalf("Run (force): %v", err)
	}
	if resultForce.Summary.Updated != 2 {
		t.Errorf("force: Summary.Updated = %d, want 2 (all sessions)", resultForce.Summary.Updated)
	}
}

// TestPipeline_Reindex_IndexLogPopulated verifies that PipelineResult.IndexLog is
// populated with correct outcomes during reindex mode.
func TestPipeline_Reindex_IndexLogPopulated(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Session 1: source missing → fallback + reindexed entries.
	meta1 := makeReindexMeta(t, testSessionID, "/nonexistent/source.jsonl")
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta1)

	sid1, _ := ingest.NewSessionID(testSessionID)

	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid1}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid1: {
				{SessionID: sid1, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
				{SessionID: sid1, EntryIndex: 1, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText},
			},
		},
	}

	indexLogger := &testutil.StubIndexLogger{}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
		ingest.WithIndexLogger(indexLogger),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// IndexLog should have 2 entries: one fallback (source missing) and one reindexed (indexing succeeded).
	if len(result.IndexLog) != 2 {
		t.Fatalf("IndexLog length = %d, want 2", len(result.IndexLog))
	}

	// First entry: fallback.
	if result.IndexLog[0].Outcome != ingest.IndexOutcomeFallback {
		t.Errorf("IndexLog[0].Outcome = %q, want %q", result.IndexLog[0].Outcome, ingest.IndexOutcomeFallback)
	}

	// Second entry: reindexed.
	if result.IndexLog[1].Outcome != ingest.IndexOutcomeReindexed {
		t.Errorf("IndexLog[1].Outcome = %q, want %q", result.IndexLog[1].Outcome, ingest.IndexOutcomeReindexed)
	}
	if result.IndexLog[1].EntriesCount != 2 {
		t.Errorf("IndexLog[1].EntriesCount = %d, want 2", result.IndexLog[1].EntriesCount)
	}

	// Verify IndexLogger received same entries.
	if len(indexLogger.Entries) != 2 {
		t.Errorf("indexLogger.Entries length = %d, want 2", len(indexLogger.Entries))
	}
}

// TestPipeline_Reindex_DryRun verifies that --reindex --dry-run shows what would
// be processed without actually doing anything.
func TestPipeline_Reindex_DryRun(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	meta := makeReindexMeta(t, testSessionID, "/nonexistent/source.jsonl")
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta)

	sid, _ := ingest.NewSessionID(testSessionID)
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
		c.DryRun = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.Updated != 1 {
		t.Errorf("DryRun Summary.Updated = %d, want 1", result.Summary.Updated)
	}
	// No indexing should have happened.
	if result.Summary.Indexed != 0 {
		t.Errorf("DryRun Summary.Indexed = %d, want 0", result.Summary.Indexed)
	}
	// No IndexLog entries should be present (dry-run skips processing).
	if len(result.IndexLog) != 0 {
		t.Errorf("DryRun IndexLog length = %d, want 0", len(result.IndexLog))
	}
}

// TestPipeline_Reindex_UpdatesIndexState verifies that after successful reindexing,
// the pipeline calls UpdateIndexState with CurrentIndexVersion.
func TestPipeline_Reindex_UpdatesIndexState(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	meta := makeReindexMeta(t, testSessionID, "/nonexistent/source.jsonl")
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta)

	sid, _ := ingest.NewSessionID(testSessionID)
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	_, err = pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify UpdateIndexState was called.
	version, ok := metricsStore.IndexStates[sid]
	if !ok {
		t.Fatal("UpdateIndexState not called for session")
	}
	if version != ingest.CurrentIndexVersion {
		t.Errorf("IndexStates[%s] = %d, want %d", sid, version, ingest.CurrentIndexVersion)
	}
}

// setupPeasantSyncSubagentSession writes metadata and transcript for a subagent
// session in the nested layout: {outputDir}/{hostSlug}/{parentID}/subagents/{subagentID}/
func setupPeasantSyncSubagentSession(t *testing.T, mfs *testutil.MemFS, outputDir, hostSlug, parentIDStr, subagentIDStr string, meta *ingest.UnifiedMetadata) (string, string) {
	t.Helper()

	sessionDir := fmt.Sprintf("%s/%s/%s/%s/%s",
		outputDir, hostSlug, parentIDStr, defaults.DirSubagents.String(), subagentIDStr)
	metaFilename := fmt.Sprintf("%s--metadata.json", subagentIDStr)
	metaPath := fmt.Sprintf("%s/%s", sessionDir, metaFilename)

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal subagent metadata: %v", err)
	}
	if err := mfs.WriteFile(metaPath, metaJSON, 0644); err != nil {
		t.Fatalf("write subagent metadata: %v", err)
	}

	ext := string(meta.Source.Format)
	if ext == "" {
		ext = string(ingest.SourceFormatJSONL)
	}
	transcriptFilename := fmt.Sprintf("%s--transcript.%s", subagentIDStr, ext)
	transcriptPath := fmt.Sprintf("%s/%s", sessionDir, transcriptFilename)
	content := []byte(`{"type":"user","content":"subagent hello"}` + "\n")
	if err := mfs.WriteFile(transcriptPath, content, 0644); err != nil {
		t.Fatalf("write subagent transcript: %v", err)
	}

	return metaPath, transcriptPath
}

// TestPipeline_Reindex_IncludesSubagents verifies that scanPeasantSyncSessions
// discovers both top-level and nested subagent sessions via the --reindex --force
// --dry-run path (which calls scanPeasantSyncSessions without needing a MetricsStore
// stale-session filter).
func TestPipeline_Reindex_IncludesSubagents(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Set up parent session in flat layout.
	parentSourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	parentMeta := makeReindexMeta(t, testSessionID, parentSourcePath)
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, parentMeta)

	// Set up subagent session in nested layout under parent.
	subagentSourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testutil.TestSubagentID)
	subagentMeta := makeReindexMeta(t, testutil.TestSubagentID, subagentSourcePath)
	setupPeasantSyncSubagentSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, testutil.TestSubagentID, subagentMeta)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
		c.Force = true // target ALL sessions (no MetricsStore needed)
		c.DryRun = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Expect both parent and subagent to be discovered.
	if result.Summary.Updated != 2 {
		t.Errorf("Summary.Updated = %d, want 2 (parent + subagent)", result.Summary.Updated)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("Sessions len = %d, want 2", len(result.Sessions))
	}

	// Verify both session IDs are present.
	parentSID, _ := ingest.NewSessionID(testSessionID)
	subSID, _ := ingest.NewSessionID(testutil.TestSubagentID)
	foundParent, foundSub := false, false
	for _, sr := range result.Sessions {
		switch sr.SessionID {
		case parentSID:
			foundParent = true
		case subSID:
			foundSub = true
		}
	}
	if !foundParent {
		t.Error("parent session not found in results")
	}
	if !foundSub {
		t.Error("subagent session not found in results")
	}
}

// TestPipeline_AutoDetect_ReconstructsSubagent verifies that reconstructFromMetadata
// can find a subagent session stored in the nested layout when the auto-detect stale
// session mechanism in Run() looks it up.
func TestPipeline_AutoDetect_ReconstructsSubagent(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Set up parent session dir (needed so the parent dir exists for walk),
	// but the parent itself is not stale — only the subagent is.
	parentSourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	parentMeta := makeReindexMeta(t, testSessionID, parentSourcePath)
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, parentMeta)

	// Set up subagent in nested layout.
	subagentSourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testutil.TestSubagentID)
	subagentMeta := makeReindexMeta(t, testutil.TestSubagentID, subagentSourcePath)
	setupPeasantSyncSubagentSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, testutil.TestSubagentID, subagentMeta)

	subSID, _ := ingest.NewSessionID(testutil.TestSubagentID)

	// MetricsStore reports only the subagent as stale.
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{subSID}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			subSID: {
				{SessionID: subSID, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
			},
		},
	}

	// Provide an adapter that discovers NO sessions from source (empty Discover).
	// The subagent should be picked up by auto-detect via reconstructFromMetadata.
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir)

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The subagent should have been indexed via auto-detect.
	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1 (subagent via auto-detect)", result.Summary.Indexed)
	}

	// Verify the indexed session is our subagent.
	indexed, ok := metricsStore.IndexedEntries[subSID]
	if !ok {
		t.Fatal("subagent session was not indexed in MetricsStore")
	}
	if len(indexed) != 1 {
		t.Errorf("indexed entries for subagent = %d, want 1", len(indexed))
	}
}

// TestPipeline_AutoDetect_StaleVersionTriggersReindex verifies that the auto-detect
// mechanism in Run() triggers re-indexing for a session whose index_version is
// older than CurrentIndexVersion. This proves the version bump is wired correctly:
// ListStaleIndexSessions is called with CurrentIndexVersion, the session is indexed,
// and UpdateIndexState is called with the new version.
func TestPipeline_AutoDetect_StaleVersionTriggersReindex(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Arrange: write a session to peasant-sync output as if it was previously
	// indexed at an older version than CurrentIndexVersion.
	originalSourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	meta := makeReindexMeta(t, testSessionID, originalSourcePath)
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta)

	sid, _ := ingest.NewSessionID(testSessionID)

	// metricsStore returns this session when asked for index_version < currentVersion.
	// This simulates the DB returning sid because its stored version < CurrentIndexVersion.
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	// Normal pipeline run (no --reindex flag): discovers no new sessions from source,
	// but auto-detect picks up the stale session.
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir)

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Pipeline must call ListStaleIndexSessions with exactly CurrentIndexVersion=4.
	if metricsStore.ListStaleCalledWithVersion != ingest.CurrentIndexVersion {
		t.Errorf("ListStaleIndexSessions called with version=%d, want CurrentIndexVersion=%d",
			metricsStore.ListStaleCalledWithVersion, ingest.CurrentIndexVersion)
	}

	// The stale session must have been re-indexed.
	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1 (stale v3 session re-indexed to v%d)",
			result.Summary.Indexed, ingest.CurrentIndexVersion)
	}

	// UpdateIndexState must have been called with CurrentIndexVersion (4).
	version, ok := metricsStore.IndexStates[sid]
	if !ok {
		t.Fatal("UpdateIndexState not called for stale session after re-indexing")
	}
	if version != ingest.CurrentIndexVersion {
		t.Errorf("IndexStates[%s] = %d, want CurrentIndexVersion=%d",
			sid, version, ingest.CurrentIndexVersion)
	}
}

// TestPipeline_AutoDetect_ReconstructFromSourceInfo verifies that a session
// that is stale (in StaleIndexSessions) but has NO peasant-sync metadata on disk
// can still be re-indexed via the reconstructFromSourceInfo fallback path, which
// reads source_path, source_format, and provider from the DB and constructs the
// peasant-sync output transcript path using the host_slug from LookupSessionLocation.
func TestPipeline_AutoDetect_ReconstructFromSourceInfo(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sid, err := ingest.NewSessionID(testSessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	// The session has NO peasant-sync metadata on disk — only a transcript file at
	// the output path that reconstructFromSourceInfo will construct.
	// Output path: {outputDir}/{hostSlug}/{sid}/{sid}--transcript.jsonl
	outputTranscriptPath := fmt.Sprintf("%s/%s/%s/%s--transcript.jsonl",
		testOutputDir, testutil.TestHostSlug, testSessionID, testSessionID)
	content := []byte(`{"type":"user","content":"hello"}` + "\n")
	if err := mfs.WriteFile(outputTranscriptPath, content, 0644); err != nil {
		t.Fatalf("write output transcript: %v", err)
	}

	// MetricsStore reports this session as stale and provides source info via DB.
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	metricsStore.SourceInfoByID = map[ingest.SessionID]struct {
		SourcePath   string
		SourceFormat ingest.SourceFormat
		Harness      string
	}{
		sid: {
			SourcePath:   sourcePath,
			SourceFormat: ingest.SourceFormatJSONL,
			Harness:      string(ingest.HarnessClaudeCode),
		},
	}
	metricsStore.LookupSessionLocationFunc = func(_ context.Context, _ ingest.SessionID) (string, string, error) {
		return testutil.TestHostSlug, "", nil
	}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {
				{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
			},
		},
		CalledWith: make(map[ingest.SessionID]ingest.DiscoveredSession),
	}

	// No sessions discovered from source — the session must be picked up by
	// reconstructFromSourceInfo, not the normal discovery path.
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir)

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The session should have been indexed via the source-info fallback.
	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1 (via reconstructFromSourceInfo)", result.Summary.Indexed)
	}

	// Verify the indexed session is our session.
	indexed, ok := metricsStore.IndexedEntries[sid]
	if !ok {
		t.Fatal("session was not indexed in MetricsStore (reconstructFromSourceInfo fallback failed)")
	}
	if len(indexed) != 1 {
		t.Errorf("indexed entries for session = %d, want 1", len(indexed))
	}

	// Verify the indexer received the correct output transcript path (flat layout).
	called, ok := indexer.CalledWith[sid]
	if !ok {
		t.Fatal("indexer was not called for session")
	}
	wantPath := fmt.Sprintf("%s/%s/%s/%s--transcript.jsonl",
		testOutputDir, testutil.TestHostSlug, testSessionID, testSessionID)
	if got := string(called.SourcePath); got != wantPath {
		t.Errorf("indexer received SourcePath = %q, want flat output path %q", got, wantPath)
	}
}

// TestPipeline_AutoDetect_ReconstructFromSourceInfo_Subagent verifies that a
// subagent session that is stale but has NO peasant-sync metadata on disk is
// re-indexed via reconstructFromSourceInfo using the subagent directory layout:
// {outputDir}/{hostSlug}/{parentID}/subagents/{sid}/{sid}--transcript.{ext}
func TestPipeline_AutoDetect_ReconstructFromSourceInfo_Subagent(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Use TestSubagentID as the subagent session ID and TestSessionUUID as the parent.
	sid, err := ingest.NewSessionID(testutil.TestSubagentID)
	if err != nil {
		t.Fatalf("NewSessionID (subagent): %v", err)
	}
	parentSid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID (parent): %v", err)
	}

	// The subagent session has NO peasant-sync metadata on disk — only a transcript
	// file at the subagent layout path:
	// {outputDir}/{hostSlug}/{parentID}/subagents/{sid}/{sid}--transcript.jsonl
	outputTranscriptPath := fmt.Sprintf("%s/%s/%s/%s/%s/%s--transcript.jsonl",
		testOutputDir, testutil.TestHostSlug, string(parentSid),
		defaults.DirSubagents.String(), string(sid), string(sid))
	content := []byte(`{"type":"user","content":"subagent hello"}` + "\n")
	if err := mfs.WriteFile(outputTranscriptPath, content, 0644); err != nil {
		t.Fatalf("write subagent output transcript: %v", err)
	}

	// MetricsStore reports the subagent as stale and provides source info + location.
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, string(sid))
	metricsStore.SourceInfoByID = map[ingest.SessionID]struct {
		SourcePath   string
		SourceFormat ingest.SourceFormat
		Harness      string
	}{
		sid: {
			SourcePath:   sourcePath,
			SourceFormat: ingest.SourceFormatJSONL,
			Harness:      string(ingest.HarnessClaudeCode),
		},
	}
	// LookupSessionLocation returns a non-empty parentID, indicating subagent layout.
	metricsStore.LookupSessionLocationFunc = func(_ context.Context, sessionID ingest.SessionID) (string, string, error) {
		return testutil.TestHostSlug, string(parentSid), nil
	}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {
				{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
			},
		},
		CalledWith: make(map[ingest.SessionID]ingest.DiscoveredSession),
	}

	// No sessions discovered from source adapters — the session must be picked up
	// by reconstructFromSourceInfo via the subagent path, not normal discovery.
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir)

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The subagent session should have been indexed via the source-info fallback.
	if result.Summary.Indexed != 1 {
		t.Errorf("Summary.Indexed = %d, want 1 (subagent via reconstructFromSourceInfo)", result.Summary.Indexed)
	}

	// Verify the indexed session is our subagent session.
	indexed, ok := metricsStore.IndexedEntries[sid]
	if !ok {
		t.Fatal("subagent session was not indexed in MetricsStore (reconstructFromSourceInfo subagent fallback failed)")
	}
	if len(indexed) != 1 {
		t.Errorf("indexed entries for subagent session = %d, want 1", len(indexed))
	}

	// Verify the indexer received the subagent layout output path (not the flat path).
	called, ok := indexer.CalledWith[sid]
	if !ok {
		t.Fatal("indexer was not called for subagent session")
	}
	wantPath := fmt.Sprintf("%s/%s/%s/%s/%s/%s--transcript.jsonl",
		testOutputDir, testutil.TestHostSlug, string(parentSid),
		defaults.DirSubagents.String(), string(sid), string(sid))
	if got := string(called.SourcePath); got != wantPath {
		t.Errorf("indexer received SourcePath = %q, want subagent output path %q", got, wantPath)
	}
}

// --- Commit Detection Tests ---

// testCommitFixture returns a CommitInfo with the given hash that matches
// testutil.TestEmail so CommitDetector's email filter passes.
func testCommitFixture(hash, message string) ingest.CommitInfo {
	return ingest.CommitInfo{
		Hash:        hash,
		Message:     message,
		AuthorName:  "Test User",
		AuthorEmail: testutil.TestEmail, // matches DefaultGitResolver email
		CommitTime:  1708300830000,      // within session window
		AuthorTime:  1708300830000,
	}
}

// readMetadataFromMemFS parses the written metadata JSON for the given sessionID.
func readMetadataFromMemFS(t *testing.T, mfs interface{ ReadFile(string) ([]byte, error) }, sessionID string) ingest.UnifiedMetadata {
	t.Helper()
	base := expectedOutputBase(testOutputDir, sessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, sessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata at %q: %v", metaPath, err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}
	return meta
}

func TestPipeline_CommitDetection_WrittenToMetadata(t *testing.T) {
	// Verify that commits returned by GitDiffAnalyzer are written to the
	// session's {sessionId}--metadata.json file under Git.Commits.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver() // email = testutil.TestEmail

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	wantCommit := testCommitFixture("abc123def456", "feat: add session tracking")
	gitAnalyzer := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{wantCommit},
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithGitDiffAnalyzer(gitAnalyzer))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	written := readMetadataFromMemFS(t, mfs, testSessionID)

	if len(written.Git.Commits) != 1 {
		t.Fatalf("Git.Commits len = %d, want 1", len(written.Git.Commits))
	}
	got := written.Git.Commits[0]
	if got.Hash != wantCommit.Hash {
		t.Errorf("Commits[0].Hash = %q, want %q", got.Hash, wantCommit.Hash)
	}
	if got.Message != wantCommit.Message {
		t.Errorf("Commits[0].Message = %q, want %q", got.Message, wantCommit.Message)
	}
	if got.AuthorEmail != wantCommit.AuthorEmail {
		t.Errorf("Commits[0].AuthorEmail = %q, want %q", got.AuthorEmail, wantCommit.AuthorEmail)
	}
}

func TestPipeline_CommitDetection_WrittenToDatabase(t *testing.T) {
	// Verify that commits are persisted to the store via UpsertSessionCommits
	// when both WithGitDiffAnalyzer and WithStore are configured.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	wantCommit := testCommitFixture("deadbeef1234", "fix: correct timing logic")
	gitAnalyzer := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{wantCommit},
	}
	store := &testutil.StubSessionStore{}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithGitDiffAnalyzer(gitAnalyzer),
		ingest.WithStore(store),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	sid := session.SessionID
	commits, ok := store.UpsertedCommits[sid]
	if !ok {
		t.Fatalf("UpsertedCommits[%s] not set; expected commit to be persisted", sid)
	}
	if len(commits) != 1 {
		t.Fatalf("UpsertedCommits[%s] len = %d, want 1", sid, len(commits))
	}
	if commits[0].Hash != wantCommit.Hash {
		t.Errorf("UpsertedCommits[%s][0].Hash = %q, want %q", sid, commits[0].Hash, wantCommit.Hash)
	}
}

func TestPipeline_CommitDetection_WithNoMatchingCommits(t *testing.T) {
	// When the git analyzer returns commits whose author email does not match
	// the session user email, the metadata should have no commits and
	// UpsertSessionCommits should not be called.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver() // email = testutil.TestEmail = "test@example.com"

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// Commit with a different author email — should be filtered out.
	otherCommit := ingest.CommitInfo{
		Hash:        "cafebabe0001",
		Message:     "chore: by someone else",
		AuthorEmail: "other@example.com",
		CommitTime:  1708300830000,
		AuthorTime:  1708300830000,
	}
	gitAnalyzer := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{otherCommit},
	}
	store := &testutil.StubSessionStore{}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithGitDiffAnalyzer(gitAnalyzer),
		ingest.WithStore(store),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	written := readMetadataFromMemFS(t, mfs, testSessionID)
	if len(written.Git.Commits) != 0 {
		t.Errorf("Git.Commits = %v, want empty (non-matching author filtered out)", written.Git.Commits)
	}

	// UpsertSessionCommits IS called (unconditional) but with an empty slice,
	// so the DB row count should be zero for this session.
	if commits, ok := store.UpsertedCommits[session.SessionID]; ok {
		if len(commits) != 0 {
			t.Errorf("UpsertedCommits[%s] = %v, want empty slice (non-matching author filtered out)", session.SessionID, commits)
		}
	}
}

func TestPipeline_CommitDetection_WithGitTimeout(t *testing.T) {
	// When git log times out, the session still ingests and a diagnostic
	// warning is recorded in metadata.Diagnostics.Warnings.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// Simulate a timeout error from the git analyzer.
	gitAnalyzer := &testutil.StubGitDiffAnalyzer{
		GetCommitsWithMetaErr: fmt.Errorf("git log timed out after 5s"),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithGitDiffAnalyzer(gitAnalyzer),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (git timeout must not propagate as pipeline error)", err)
	}

	// Session must still be ingested despite the git timeout.
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1 (session must ingest despite git timeout)", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Diagnostic warning must be recorded in the metadata.
	written := readMetadataFromMemFS(t, mfs, testSessionID)
	if len(written.Diagnostics.Warnings) == 0 {
		t.Fatal("Diagnostics.Warnings is empty; expected git_timeout warning")
	}
	found := false
	for _, w := range written.Diagnostics.Warnings {
		if w.ErrorType == "git_timeout" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no git_timeout warning found in Diagnostics.Warnings: %+v", written.Diagnostics.Warnings)
	}
}

func TestPipeline_CommitDetection_NotFatalOnGitFailure(t *testing.T) {
	// When git log fails (e.g. repo not found), the session ingests with
	// empty commits and a diagnostic warning — not a pipeline error.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	gitAnalyzer := &testutil.StubGitDiffAnalyzer{
		GetCommitsWithMetaErr: fmt.Errorf("not a git repository: /home/test/testrepo"),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithGitDiffAnalyzer(gitAnalyzer),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (git failure must not propagate as pipeline error)", err)
	}

	// Session is ingested even when git fails.
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Metadata is written with no commits but with a diagnostic warning.
	written := readMetadataFromMemFS(t, mfs, testSessionID)
	if len(written.Git.Commits) != 0 {
		t.Errorf("Git.Commits = %v, want empty on git failure", written.Git.Commits)
	}
	if len(written.Diagnostics.Warnings) == 0 {
		t.Fatal("Diagnostics.Warnings is empty; expected git_failure warning")
	}
	found := false
	for _, w := range written.Diagnostics.Warnings {
		if w.ErrorType == "git_failure" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no git_failure warning in Diagnostics.Warnings: %+v", written.Diagnostics.Warnings)
	}
}

func TestPipeline_CommitDetection_Idempotent_SecondRun(t *testing.T) {
	// A second pipeline run for an unchanged session skips EXTRACT+WRITE,
	// leaving the metadata (with commits) on disk from the first run intact.
	// The store receives UpsertSessionCommits only on the first run.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	modTime := time.Now().Add(-2 * time.Hour)
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	wantCommit := testCommitFixture("f00dcafe0001", "test: idempotent run")
	gitAnalyzer := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{wantCommit},
	}
	store := &testutil.StubSessionStore{}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithGitDiffAnalyzer(gitAnalyzer),
		ingest.WithStore(store),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	// First run: session is New, commits are detected and written.
	result1, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if result1.Summary.New != 1 {
		t.Fatalf("Run 1: Summary.New = %d, want 1", result1.Summary.New)
	}

	// Verify commits were written to metadata on disk.
	written1 := readMetadataFromMemFS(t, mfs, testSessionID)
	if len(written1.Git.Commits) != 1 {
		t.Fatalf("Run 1: Git.Commits len = %d, want 1", len(written1.Git.Commits))
	}
	if written1.Git.Commits[0].Hash != wantCommit.Hash {
		t.Errorf("Run 1: Commits[0].Hash = %q, want %q", written1.Git.Commits[0].Hash, wantCommit.Hash)
	}

	// Verify UpsertSessionCommits was called on first run.
	if _, ok := store.UpsertedCommits[session.SessionID]; !ok {
		t.Fatal("Run 1: UpsertedCommits not populated after first run")
	}
	store.UpsertedCommits = nil // reset to detect second-run calls

	// Second run: session is Unchanged (same source modtime + schema version),
	// so EXTRACT+WRITE is skipped. Metadata on disk must remain unchanged.
	result2, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if result2.Summary.Unchanged != 1 {
		t.Errorf("Run 2: Summary.Unchanged = %d, want 1 (session unchanged)", result2.Summary.Unchanged)
	}

	// Metadata on disk is unchanged — commits still present.
	written2 := readMetadataFromMemFS(t, mfs, testSessionID)
	if len(written2.Git.Commits) != 1 {
		t.Fatalf("Run 2: Git.Commits len = %d, want 1 (unchanged on disk)", len(written2.Git.Commits))
	}
	if written2.Git.Commits[0].Hash != wantCommit.Hash {
		t.Errorf("Run 2: Commits[0].Hash = %q, want %q", written2.Git.Commits[0].Hash, wantCommit.Hash)
	}

	// UpsertSessionCommits must NOT have been called on second run.
	if len(store.UpsertedCommits) != 0 {
		t.Errorf("Run 2: UpsertedCommits should not be called for unchanged session, got: %v", store.UpsertedCommits)
	}
}

func TestPipeline_CommitDetection_StandardRepo_NoWorktree(t *testing.T) {
	// BLOCKER-3 regression test: commit detection must work when meta.Git.Worktree
	// is nil (standard repo — the common case). Project.FilePath is used as fallback.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)
	// Explicitly clear Worktree to simulate a standard (non-linked-worktree) repo.
	meta.Git.Worktree = nil
	// Project.FilePath is already set by makeMinimalMeta to "/home/test/testrepo".

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	wantCommit := testCommitFixture("standardrepo001", "feat: standard repo commit")
	gitAnalyzer := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{wantCommit},
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithGitDiffAnalyzer(gitAnalyzer),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	written := readMetadataFromMemFS(t, mfs, testSessionID)

	if len(written.Git.Commits) != 1 {
		t.Fatalf("Git.Commits len = %d, want 1 (standard repo, no Worktree field)", len(written.Git.Commits))
	}
	if written.Git.Commits[0].Hash != wantCommit.Hash {
		t.Errorf("Commits[0].Hash = %q, want %q", written.Git.Commits[0].Hash, wantCommit.Hash)
	}
}

func TestPipeline_CommitDetection_ForceReingest_ClearsStaleDBRows(t *testing.T) {
	// IMPORTANT-1 regression test: a --force re-ingest that finds 0 commits must
	// call UpsertSessionCommits with an empty slice, deleting stale DB rows and
	// keeping JSON metadata and DB in sync.
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// First run: git analyzer returns a commit — written to DB.
	gitAnalyzer := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{testCommitFixture("stale001stale", "feat: will become stale")},
	}
	store := &testutil.StubSessionStore{}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithGitDiffAnalyzer(gitAnalyzer),
		ingest.WithStore(store),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	if _, err := pipeline.Run(context.Background()); err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	if commits, ok := store.UpsertedCommits[session.SessionID]; !ok || len(commits) != 1 {
		t.Fatalf("Run 1: expected 1 commit in store, got %v", store.UpsertedCommits[session.SessionID])
	}

	// Second run (--force): git analyzer returns no commits (email mismatch, etc.).
	// UpsertSessionCommits must be called with empty slice to clear stale rows.
	gitAnalyzer.CommitInfos = nil // StubGitDiffAnalyzer returns [] when nil
	store.UpsertedCommits = nil   // reset to detect second-run calls

	cfgForce := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Force = true
	})
	pipelineForce, err := ingest.NewPipeline(mfs, git, adapters, cfgForce,
		ingest.WithGitDiffAnalyzer(gitAnalyzer),
		ingest.WithStore(store),
	)
	if err != nil {
		t.Fatalf("NewPipeline (force): %v", err)
	}

	if _, err := pipelineForce.Run(context.Background()); err != nil {
		t.Fatalf("Run 2 (force): %v", err)
	}

	// UpsertSessionCommits must have been called with empty slice (clears stale rows).
	commits, ok := store.UpsertedCommits[session.SessionID]
	if !ok {
		t.Fatal("Run 2: UpsertSessionCommits not called on forced re-ingest with 0 commits")
	}
	if len(commits) != 0 {
		t.Errorf("Run 2: UpsertedCommits = %v, want empty slice (stale rows cleared)", commits)
	}

	// Metadata on disk must also have no commits (JSON/DB in sync).
	written := readMetadataFromMemFS(t, mfs, testSessionID)
	if len(written.Git.Commits) != 0 {
		t.Errorf("Run 2: metadata Git.Commits = %v, want empty", written.Git.Commits)
	}
}

// TestPipeline_DetectCommitsFlag_OFF verifies that without WithGitDiffAnalyzer
// (i.e., --detect-commits flag is OFF), commit detection never runs and
// metadata.Git.Commits is nil in the written output.
func TestPipeline_DetectCommitsFlag_OFF(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// No WithGitDiffAnalyzer — simulates --detect-commits flag OFF.
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Commit detection must not have run: Git.Commits should be nil
	// (no key in JSON; omitempty omits nil/empty slices).
	written := readMetadataFromMemFS(t, mfs, testSessionID)
	if written.Git.Commits != nil {
		t.Errorf("Git.Commits = %v, want nil (no git detection without WithGitDiffAnalyzer)", written.Git.Commits)
	}
}

// TestPipeline_DetectCommitsFlag_ON verifies that with WithGitDiffAnalyzer
// (i.e., --detect-commits flag is ON), detection runs but produces nil
// (not []CommitInfo{}) in the JSON when no commits match the author email.
// This exercises the omitempty tag: an empty result must not appear in the JSON.
func TestPipeline_DetectCommitsFlag_ON(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver() // email = testutil.TestEmail

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	// Analyzer injected (flag ON), but returns a commit by a different author —
	// the email filter removes it, leaving zero matching commits.
	gitAnalyzer := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{{
			Hash:        "deadbeef0000",
			Message:     "refactor: by another author",
			AuthorEmail: "other@example.com", // not testutil.TestEmail
			CommitTime:  1708300830000,
			AuthorTime:  1708300830000,
		}},
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithGitDiffAnalyzer(gitAnalyzer),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.Errors != 0 {
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}

	// Read back the JSON-serialized metadata.
	// Because GitContext.Commits has `json:"commits,omitempty"`, the field is
	// absent from JSON when empty, so unmarshaling gives a nil slice.
	written := readMetadataFromMemFS(t, mfs, testSessionID)
	if len(written.Git.Commits) != 0 {
		t.Errorf("Git.Commits = %v, want nil/empty (email filter removed non-matching commit; omitempty omits empty)", written.Git.Commits)
	}

	// Verify the raw JSON does not contain a "commits" key.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}
	if strings.Contains(string(data), `"commits"`) {
		t.Errorf("metadata JSON contains \"commits\" key, want it omitted by omitempty:\n%s", data)
	}
}

// TestPipeline_Reindex_EmitsProgressEvents verifies that runReindex() drives
// all 9 pipeline stages to completion via ProgressState.
func TestPipeline_Reindex_EmitsProgressEvents(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	meta := makeReindexMeta(t, testSessionID, "/nonexistent/source.jsonl")
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta)

	sid, _ := ingest.NewSessionID(testSessionID)
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	// ProgressState replaces the old channel; no buffer sizing or drop risk.
	progState := ingest.NewProgressState()

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
		c.Progress = progState
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	_, err = pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify every stage reached the Ended state.
	snap := progState.Snapshot()
	for _, stage := range ingest.StageOrder {
		sp, ok := snap[stage]
		if !ok {
			t.Errorf("stage %s: missing from snapshot", stage)
			continue
		}
		if !sp.Ended {
			t.Errorf("stage %s: Ended=false after pipeline completed", stage)
		}
	}
}

// TestPipeline_Reindex_SummaryVersionFields verifies that PipelineSummary
// includes IndexVersion and MetadataVersion after a reindex run.
func TestPipeline_Reindex_SummaryVersionFields(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	meta := makeReindexMeta(t, testSessionID, "/nonexistent/source.jsonl")
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta)

	sid, _ := ingest.NewSessionID(testSessionID)
	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.IndexVersion != ingest.CurrentIndexVersion {
		t.Errorf("Summary.IndexVersion = %d, want %d", result.Summary.IndexVersion, ingest.CurrentIndexVersion)
	}
	if result.Summary.MetadataVersion != int(ingest.CurrentSchemaVersion) {
		t.Errorf("Summary.MetadataVersion = %d, want %d", result.Summary.MetadataVersion, int(ingest.CurrentSchemaVersion))
	}
}

// TestPipeline_NormalIngest_SummaryVersionFields verifies that PipelineSummary
// includes IndexVersion and MetadataVersion after a normal (non-reindex) run.
func TestPipeline_NormalIngest_SummaryVersionFields(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourceFile := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourceFile)

	sid, _ := ingest.NewSessionID(testSessionID)
	metricsStore := testutil.NewStubMetricsStore()

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{{
				SessionID:    sid,
				Harness:      ingest.HarnessClaudeCode,
				SourcePath:   ingest.ResolvedPath(sourceFile),
				SourceFormat: ingest.SourceFormatJSONL,
			}},
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: makeMinimalMeta(t, testSessionID)},
		),
	}
	cfg := makePipelineConfig(testOutputDir)

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.IndexVersion != ingest.CurrentIndexVersion {
		t.Errorf("Summary.IndexVersion = %d, want %d", result.Summary.IndexVersion, ingest.CurrentIndexVersion)
	}
	if result.Summary.MetadataVersion != int(ingest.CurrentSchemaVersion) {
		t.Errorf("Summary.MetadataVersion = %d, want %d", result.Summary.MetadataVersion, int(ingest.CurrentSchemaVersion))
	}
}

// TestPipeline_Reindex_ParallelExtract verifies that when multiple sessions
// have existing source files, reindex processes them via runParallel + StagingBuffer.
func TestPipeline_Reindex_ParallelExtract(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Set up two sessions with existing source files.
	sourceFile1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	sourceFile2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, sourceFile1)
	setupSourceFile(t, mfs, sourceFile2)

	meta1 := makeReindexMeta(t, testSessionID, sourceFile1)
	meta2 := makeReindexMeta(t, testSessionID2, sourceFile2)
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID, meta1)
	setupPeasantSyncSession(t, mfs, testOutputDir, testutil.TestHostSlug, testSessionID2, meta2)

	sid1, _ := ingest.NewSessionID(testSessionID)
	sid2, _ := ingest.NewSessionID(testSessionID2)

	metricsStore := testutil.NewStubMetricsStore()
	metricsStore.StaleIndexSessions = []ingest.SessionID{sid1, sid2}

	indexer := &testutil.StubIndexer{
		Kind: ingest.TranscriptSourceFile,
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid1: {{SessionID: sid1, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
			sid2: {{SessionID: sid2, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
		},
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			nil,
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				sid1: meta1,
				sid2: meta2,
			},
		),
	}
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.Reindex = true
		c.Parallelism = 2 // explicitly use 2 workers
	})

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: indexer,
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Both sessions should be processed (extracted from source).
	if result.Summary.Updated != 2 {
		t.Errorf("Summary.Updated = %d, want 2", result.Summary.Updated)
	}
	if result.Summary.Indexed != 2 {
		t.Errorf("Summary.Indexed = %d, want 2", result.Summary.Indexed)
	}

	// Verify both have OutputPaths (processSession succeeded).
	outputPaths := 0
	for _, sr := range result.Sessions {
		if sr.OutputPath != "" {
			outputPaths++
		}
	}
	if outputPaths != 2 {
		t.Errorf("sessions with OutputPath = %d, want 2", outputPaths)
	}
}

// TestPipeline_GoroutineLifecycle_ParentChild exercises the full goroutine
// lifecycle with a parent+child session pair. This verifies Commit-before-Ack
// ordering: the parent must be committed to the staging buffer before the
// child becomes eligible for drain — if ordering is reversed the child will
// never drain and the pipeline will deadlock.
//
// The test also injects a store error to verify that errCh propagation reaches
// Summary.StoreError without blocking any goroutine.
func TestPipeline_GoroutineLifecycle_ParentChild(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Set up parent and child source files.
	parentSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	childSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testutil.TestSubagentID)
	setupSourceFile(t, mfs, parentSource)
	setupSourceFile(t, mfs, childSource)

	modTime := time.Now().Add(-1 * time.Hour)
	parentSession := makeDiscoveredSession(t, testSessionID, parentSource, modTime)
	parentSID := parentSession.SessionID

	childSID, err := ingest.NewSessionID(testutil.TestSubagentID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", testutil.TestSubagentID, err)
	}
	childSession := ingest.DiscoveredSession{
		SessionID:     childSID,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(childSource),
		SourceFormat:  ingest.SourceFormatJSONL,
		ParentUUID:    &parentSID,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
		ModTime:       modTime,
	}

	parentMeta := makeMinimalMeta(t, testSessionID)
	childMeta := makeMinimalMeta(t, testutil.TestSubagentID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{parentSession, childSession},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				parentSession.SessionID: parentMeta,
				childSession.SessionID:  childMeta,
			},
		),
	}

	// Inject a store error to exercise the errCh → StoreError propagation path.
	store := &testutil.StubSessionStore{InsertErr: errors.New("db locked")}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (store error should not propagate as Run error)", err)
	}

	// Both sessions must be present in results (no deadlock, no missing sessions).
	if len(result.Sessions) != 2 {
		t.Fatalf("Sessions len = %d, want 2", len(result.Sessions))
	}
	if result.Summary.Errors != 0 {
		for _, sr := range result.Sessions {
			if sr.Error != nil {
				t.Logf("Session %s error: %v", sr.SessionID, sr.Error)
			}
		}
		t.Errorf("Summary.Errors = %d, want 0 (store error is non-fatal)", result.Summary.Errors)
	}
	// new = 2 (parent + child).
	if result.Summary.New != 2 {
		t.Errorf("Summary.New = %d, want 2", result.Summary.New)
	}

	// Store error must surface in Summary.StoreError (not silently dropped).
	if result.Summary.StoreError == nil {
		t.Error("Summary.StoreError = nil, want non-nil (store returned error)")
	}

	// Both output paths must exist on disk (disk writes unaffected by store failure).
	parentBase := expectedOutputBase(testOutputDir, testSessionID)
	parentMetaPath := fmt.Sprintf("%s/%s--metadata.json", parentBase, testSessionID)
	if _, err := mfs.Stat(parentMetaPath); err != nil {
		t.Errorf("parent metadata not found at %q: %v", parentMetaPath, err)
	}
	childMetaPath := fmt.Sprintf("%s/%s/%s/subagents/%s/%s--metadata.json",
		testOutputDir, testutil.TestHostSlug, testSessionID, testutil.TestSubagentID, testutil.TestSubagentID)
	if _, err := mfs.Stat(childMetaPath); err != nil {
		t.Errorf("child metadata not found at nested path %q: %v", childMetaPath, err)
	}
}

// TestPipeline_ContextCancellation_NoDeadlock verifies that cancelling the
// context during a pipeline run causes all goroutines to terminate cleanly
// without deadlocking or triggering data races.
//
// The test uses a brief timeout to confirm the pipeline returns in finite time.
func TestPipeline_ContextCancellation_NoDeadlock(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Set up two sessions so there is at least some work to cancel.
	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	source2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, source1)
	setupSourceFile(t, mfs, source2)

	sid1 := makeDiscoveredSession(t, testSessionID, source1, time.Now().Add(-1*time.Hour))
	sid2 := makeDiscoveredSession(t, testSessionID2, source2, time.Now().Add(-1*time.Hour))
	meta1 := makeMinimalMeta(t, testSessionID)
	meta2 := makeMinimalMeta(t, testSessionID2)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{sid1, sid2},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				sid1.SessionID: meta1,
				sid2.SessionID: meta2,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Run the pipeline in a goroutine; cancel the context immediately
	// to exercise the cancellation path inside the pipeline goroutines.
	type runResult struct {
		result *ingest.PipelineResult
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		cancel() // cancel before Run; workers will see ctx.Err() != nil
		r, e := pipeline.Run(ctx)
		done <- runResult{r, e}
	}()

	// Pipeline must return within 5 seconds — no deadlock.
	select {
	case res := <-done:
		// Run may return nil error even when ctx is cancelled (context cancellation
		// is best-effort in the pipeline; it stops new work but doesn't abort ongoing writes).
		// The important property is that it terminates without deadlock.
		_ = res
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline.Run did not return within 5 seconds after context cancellation (deadlock?)")
	}
}

// TestPipeline_AuditLog_PreservedThroughGoroutineRefactor verifies requirement R7:
// the IngestLogger must receive a correct LogIngestRun call even when the pipeline
// uses the goroutine-based drain architecture. This proves that the audit log
// is written after the goroutines complete and that session counts are accurate.
//
// This test also exercises the combined path: store error via errCh, correct
// session counts in the log entry, and non-fatal error handling.
func TestPipeline_AuditLog_PreservedThroughGoroutineRefactor(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Two sessions: parent + standalone (no parent).
	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	source2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, source1)
	setupSourceFile(t, mfs, source2)

	s1 := makeDiscoveredSession(t, testSessionID, source1, time.Now().Add(-1*time.Hour))
	s2 := makeDiscoveredSession(t, testSessionID2, source2, time.Now().Add(-1*time.Hour))
	meta1 := makeMinimalMeta(t, testSessionID)
	meta2 := makeMinimalMeta(t, testSessionID2)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{s1, s2},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				s1.SessionID: meta1,
				s2.SessionID: meta2,
			},
		),
	}

	// Inject store error so errCh path is exercised.
	store := &testutil.StubSessionStore{InsertErr: errors.New("disk full")}
	logger := &testutil.StubIngestLogger{}
	cfg := makePipelineConfig(testOutputDir)

	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		ingest.WithStore(store),
		ingest.WithLogger(logger),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify result is correct.
	if result.Summary.New != 2 {
		t.Errorf("Summary.New = %d, want 2", result.Summary.New)
	}
	if result.Summary.Errors != 0 {
		t.Errorf("Summary.Errors = %d, want 0 (store error is non-fatal)", result.Summary.Errors)
	}
	if result.Summary.StoreError == nil {
		t.Error("Summary.StoreError = nil, want non-nil (injected store error)")
	}

	// R7: audit logger must have been called exactly once.
	if len(logger.Entries) != 1 {
		t.Fatalf("logger.Entries len = %d, want 1 (LogIngestRun must be called once per run)", len(logger.Entries))
	}

	entry := logger.Entries[0]

	// Session counts must reflect what actually processed, not what was stored.
	if entry.SessionsNew != 2 {
		t.Errorf("log entry SessionsNew = %d, want 2", entry.SessionsNew)
	}
	if entry.SessionsError != 0 {
		t.Errorf("log entry SessionsError = %d, want 0 (disk write errors, not store errors)", entry.SessionsError)
	}

	// Timestamps must be set.
	if entry.StartedAt == 0 {
		t.Error("log entry StartedAt = 0, want non-zero")
	}
	if entry.FinishedAt == nil || *entry.FinishedAt == 0 {
		t.Error("log entry FinishedAt = nil or 0, want non-zero")
	}
	if entry.FinishedAt != nil && *entry.FinishedAt < entry.StartedAt {
		t.Errorf("log entry FinishedAt (%d) < StartedAt (%d)", *entry.FinishedAt, entry.StartedAt)
	}
}

// --- AllowedSessionIDs filter tests ---

func TestPipeline_AllowedSessionIDs_NilAllowsAll(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	source2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, source1)
	setupSourceFile(t, mfs, source2)

	modTime := time.Now().Add(-1 * time.Hour)
	session1 := makeDiscoveredSession(t, testSessionID, source1, modTime)
	session2 := makeDiscoveredSession(t, testSessionID2, source2, modTime)
	meta1 := makeMinimalMeta(t, testSessionID)
	meta2 := makeMinimalMeta(t, testSessionID2)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session1, session2},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session1.SessionID: meta1,
				session2.SessionID: meta2,
			},
		),
	}

	// nil AllowedSessionIDs → all sessions processed
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	newCount := 0
	for _, sr := range result.Sessions {
		if sr.Status == ingest.DiffNew {
			newCount++
		}
	}
	if newCount != 2 {
		t.Errorf("nil AllowedSessionIDs: got %d new sessions, want 2", newCount)
	}
}

func TestPipeline_AllowedSessionIDs_Subset(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	source2 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	setupSourceFile(t, mfs, source1)
	setupSourceFile(t, mfs, source2)

	modTime := time.Now().Add(-1 * time.Hour)
	session1 := makeDiscoveredSession(t, testSessionID, source1, modTime)
	session2 := makeDiscoveredSession(t, testSessionID2, source2, modTime)
	meta1 := makeMinimalMeta(t, testSessionID)
	meta2 := makeMinimalMeta(t, testSessionID2)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session1, session2},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session1.SessionID: meta1,
				session2.SessionID: meta2,
			},
		),
	}

	// Only allow session1 — session2 should be filtered out.
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.AllowedSessionIDs = map[ingest.SessionID]bool{
			session1.SessionID: true,
		}
	})
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	newCount := 0
	unchangedCount := 0
	for _, sr := range result.Sessions {
		switch sr.Status {
		case ingest.DiffNew:
			newCount++
			if string(sr.SessionID) != testSessionID {
				t.Errorf("unexpected new session: %s", sr.SessionID)
			}
		case ingest.DiffUnchanged:
			unchangedCount++
		}
	}
	if newCount != 1 {
		t.Errorf("AllowedSessionIDs subset: got %d new sessions, want 1", newCount)
	}
	if unchangedCount != 1 {
		t.Errorf("AllowedSessionIDs subset: got %d unchanged (filtered) sessions, want 1", unchangedCount)
	}
}

func TestPipeline_AllowedSessionIDs_EmptyMap(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, source1)

	modTime := time.Now().Add(-1 * time.Hour)
	session1 := makeDiscoveredSession(t, testSessionID, source1, modTime)
	meta1 := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session1},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session1.SessionID: meta1,
			},
		),
	}

	// Empty non-nil map → NO sessions pass.
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.AllowedSessionIDs = map[ingest.SessionID]bool{}
	})
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, sr := range result.Sessions {
		if sr.Status == ingest.DiffNew {
			t.Errorf("empty AllowedSessionIDs: got new session %s, want none", sr.SessionID)
		}
	}
	if result.Summary.New != 0 {
		t.Errorf("empty AllowedSessionIDs: summary.New = %d, want 0", result.Summary.New)
	}
}

func TestPipeline_AllowedSessionIDs_NoOverlap(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	source1 := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, source1)

	modTime := time.Now().Add(-1 * time.Hour)
	session1 := makeDiscoveredSession(t, testSessionID, source1, modTime)
	meta1 := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session1},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session1.SessionID: meta1,
			},
		),
	}

	// AllowedSessionIDs contains a UUID not present in discovered sessions.
	nonExistentID, _ := ingest.NewSessionID(testSessionID2)
	cfg := makePipelineConfig(testOutputDir, func(c *ingest.PipelineConfig) {
		c.AllowedSessionIDs = map[ingest.SessionID]bool{
			nonExistentID: true,
		}
	})
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, sr := range result.Sessions {
		if sr.Status == ingest.DiffNew {
			t.Errorf("no-overlap AllowedSessionIDs: got new session %s, want none", sr.SessionID)
		}
	}
	if result.Summary.New != 0 {
		t.Errorf("no-overlap AllowedSessionIDs: summary.New = %d, want 0", result.Summary.New)
	}
}

// TestPipeline_CWD_StoredInMetadata verifies that the CWD field flows through
// the pipeline and is persisted in the written metadata JSON file.
func TestPipeline_CWD_StoredInMetadata(t *testing.T) {
	const testCWD = "/home/test/myproject"

	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)
	meta.CWD = testCWD

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{
				session.SessionID: meta,
			},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.New != 1 {
		t.Fatalf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Read the written metadata JSON and verify CWD is persisted.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}

	var written ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}

	if written.CWD != testCWD {
		t.Errorf("metadata.CWD = %q, want %q", written.CWD, testCWD)
	}
}

// --- metadata.json as write-through cache (schema v8, derived_at) ---

// TestPipeline_SchemaV8_DerivedAtPopulated verifies that after a full ingest run
// with a store, the written metadata.json contains a non-nil DerivedAt field.
// DerivedAt must be populated after DB INSERT (it marks when the file was derived
// from DB state), so it cannot be present if no store is configured.
func TestPipeline_SchemaV8_DerivedAtPopulated(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	store := &testutil.StubSessionStore{}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	before := time.Now().UnixMilli()
	result, err := pipeline.Run(context.Background())
	after := time.Now().UnixMilli()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}

	var written ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}

	if written.SchemaVersion != ingest.CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", written.SchemaVersion, ingest.CurrentSchemaVersion)
	}

	if written.DerivedAt == nil {
		t.Fatal("DerivedAt is nil; expected non-nil (DB INSERT happened, file should be derived from DB state)")
	}
	if *written.DerivedAt < before || *written.DerivedAt > after {
		t.Errorf("DerivedAt = %d, want in range [%d, %d]", *written.DerivedAt, before, after)
	}
}

// TestPipeline_SchemaV8_DerivedAtNilWithoutStore verifies that without a store,
// metadata.json is still written successfully but DerivedAt is nil (no DB INSERT,
// so no derived timestamp).
func TestPipeline_SchemaV8_DerivedAtNilWithoutStore(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)
	// No WithStore — backward compatible path.
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}

	var written ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}

	// Without a store, DerivedAt should be nil (no DB insertion happened).
	if written.DerivedAt != nil {
		t.Errorf("DerivedAt = %d, want nil (no store configured)", *written.DerivedAt)
	}
}

// TestPipeline_SchemaV8_StoreAndDerivedAtBothPresent verifies that when a store is
// configured, both the DB INSERT and the DerivedAt field are populated after a
// successful pipeline run. DerivedAt marks metadata.json as a derived artifact;
// the DB INSERT records the session in the store.
//
// Note: write-order enforcement (DB INSERT before metadata.json) is a future
// enhancement requiring pipeline restructuring. This test verifies the observable
// outcomes without asserting on ordering.
func TestPipeline_SchemaV8_StoreAndDerivedAtBothPresent(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)

	session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	var dbInsertCalled bool
	store := &testutil.StubSessionStore{
		OnInsert: func(_ []ingest.StoreEntry) {
			dbInsertCalled = true
		},
	}
	cfg := makePipelineConfig(testOutputDir)
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Errorf("Summary.New = %d, want 1", result.Summary.New)
	}

	// Verify DB INSERT was called.
	if !dbInsertCalled {
		t.Error("DB INSERT hook was never called — store not used")
	}

	// Read metadata.json and verify DerivedAt is set.
	base := expectedOutputBase(testOutputDir, testSessionID)
	metaPath := fmt.Sprintf("%s/%s--metadata.json", base, testSessionID)
	data, err := mfs.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}
	var written ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}
	if written.DerivedAt == nil {
		t.Error("DerivedAt is nil; expected non-nil when store is configured")
	}
}

// TestPipeline_SchemaV8_DiffUsesDBIngestedMs verifies that when the DB has a
// session record with a recent ingested_ms and current schema_version, the diff
// stage classifies it as DiffUnchanged without needing to read metadata.json.
func TestPipeline_SchemaV8_DiffUsesDBIngestedMs(t *testing.T) {
	t.Skip("TODO: requires pipeline write-order inversion (DB INSERT before metadata.json write) — deferred")
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	modTime := time.Now().Add(-2 * time.Hour)
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)
	mfs.ModTimes[sourcePath] = modTime

	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)

	// First run: ingest and write to DB+disk.
	store := &testutil.StubSessionStore{}
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result1, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("First Run: %v", err)
	}
	if result1.Summary.New != 1 {
		t.Errorf("First run Summary.New = %d, want 1", result1.Summary.New)
	}

	// Set up the store for second run: BulkLookupSessionLocations returns
	// IngestedMs = now (recent, after modTime) and SchemaVersion = CurrentSchemaVersion.
	ingestedMs := time.Now().UnixMilli() // ingested after modTime → unchanged
	store2 := &testutil.StubSessionStore{
		LocationsByID: map[ingest.SessionID]ingest.SessionLocation{
			session.SessionID: {
				HostSlug:      testutil.TestHostSlug,
				IngestedMs:    &ingestedMs,
				SchemaVersion: ingest.CurrentSchemaVersion,
			},
		},
	}

	pipeline2, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store2))
	if err != nil {
		t.Fatalf("NewPipeline (2nd): %v", err)
	}

	// Second run: source is older than DB ingested_ms + schema version is current.
	// Must be classified as DiffUnchanged.
	result2, err := pipeline2.Run(context.Background())
	if err != nil {
		t.Fatalf("Second Run: %v", err)
	}
	if result2.Summary.Unchanged != 1 {
		t.Errorf("Second run Summary.Unchanged = %d, want 1 (DB ingested_ms should detect unchanged)", result2.Summary.Unchanged)
	}
	if result2.Summary.New != 0 {
		t.Errorf("Second run Summary.New = %d, want 0", result2.Summary.New)
	}
}

// TestPipeline_SchemaV8_DiffFallsBackToFile verifies backward compatibility:
// when the DB has no record for a session but metadata.json exists on disk,
// the diff stage reads the file (old pre-migration behavior).
func TestPipeline_SchemaV8_DiffFallsBackToFile(t *testing.T) {
	t.Skip("TODO: requires pipeline write-order inversion (DB INSERT before metadata.json write) — deferred")
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	modTime := time.Now().Add(-2 * time.Hour)
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	setupSourceFile(t, mfs, sourcePath)
	mfs.ModTimes[sourcePath] = modTime

	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
		),
	}

	cfg := makePipelineConfig(testOutputDir)

	// First run without store: writes only metadata.json (no DB state).
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result1, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("First Run: %v", err)
	}
	if result1.Summary.New != 1 {
		t.Errorf("First run Summary.New = %d, want 1", result1.Summary.New)
	}

	// Second run: store with empty LocationsByID (no DB record for this session).
	// The diff stage must fall back to reading metadata.json from disk.
	// metadata.json was written with ingested > modTime → should be Unchanged.
	store2 := &testutil.StubSessionStore{
		LocationsByID: map[ingest.SessionID]ingest.SessionLocation{}, // empty — no DB record
	}

	pipeline2, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(store2))
	if err != nil {
		t.Fatalf("NewPipeline (2nd): %v", err)
	}
	result2, err := pipeline2.Run(context.Background())
	if err != nil {
		t.Fatalf("Second Run: %v", err)
	}
	// metadata.json exists with current schema version and ingested > modTime → Unchanged.
	if result2.Summary.Unchanged != 1 {
		t.Errorf("Second run Summary.Unchanged = %d, want 1 (file fallback should detect unchanged)", result2.Summary.Unchanged)
	}
}

// TestPipeline_SchemaV8_V7MetadataParses verifies that old v7 metadata.json
// without a DerivedAt field still parses correctly (backward compat).
// A v7 file should be classified as DiffUpdated (schema version mismatch).
func TestPipeline_SchemaV8_V7MetadataParses(t *testing.T) {
	// Construct a v7-style metadata JSON without derivedAt field.
	v7JSON := `{
		"schemaVersion": 7,
		"sessionId": "` + testSessionID + `",
		"parentUuid": null,
		"modelHarness": "claude",
		"model": "claude-opus-4-5",
		"version": "2.1.47",
		"timestamp": {"start": 1000, "end": 2000, "ingested": 9999999999999},
		"source": {"filePath": "/src/test.jsonl", "format": "jsonl"},
		"git": {},
		"project": {"hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "name": "test"},
		"hostSlug": "` + testutil.TestHostSlug + `",
		"stats": {"turnCount": 0, "toolCallCount": 0, "subagentCount": 0, "durationMs": 0, "tokensIn": 0, "tokensOut": 0},
		"subagents": [],
		"diagnostics": {"warnings": []},
		"contentHash": "",
		"metadataHash": "",
		"redaction": {"applied": false}
	}`

	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal([]byte(v7JSON), &meta); err != nil {
		t.Fatalf("Failed to parse v7 metadata JSON: %v", err)
	}

	// v7 file should parse with SchemaVersion = 7 and DerivedAt = nil.
	if meta.SchemaVersion != 7 {
		t.Errorf("SchemaVersion = %d, want 7", meta.SchemaVersion)
	}
	if meta.DerivedAt != nil {
		t.Errorf("DerivedAt = %v, want nil (v7 file has no derivedAt)", meta.DerivedAt)
	}
	// Ingested should still parse correctly.
	if meta.Timestamp.Ingested == nil {
		t.Error("Ingested is nil, want non-nil")
	}
}
