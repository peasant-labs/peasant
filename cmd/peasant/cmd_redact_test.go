package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// Test constants
// ---------------------------------------------------------------------------

const (
	redactTestSessionA   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	redactTestSessionB   = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" // DB-only, no files on disk
	redactTestProject    = "1111111111111111111111111111111111111111111111111111111111111111"
	redactTestHostSlug   = "github.com-test-redact"
	redactTestTranscript = `{"role":"user","content":"my secret key is sk-abc123xyz"}` + "\n" +
		`{"role":"assistant","content":"I see your API key"}` + "\n"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// executeRedactCmd runs the redact command under a test root with
// --data-dir=dir AND --config-dir=dir (via the shared executeWithDataDir
// helper), capturing combined stdout+stderr as the first return value.
func executeRedactCmd(t *testing.T, dir string, args []string) (stdout string, stderr string, err error) {
	t.Helper()
	out, err := executeWithDataDir(t, BuildRedactCommand(), dir, args)
	return out, "", err
}

// seedRedactTestSession inserts sessions and writes transcript/metadata files on disk
// under the data directory derived from dir (matching the command's --data-dir=dir).
func seedRedactTestSession(t *testing.T, dir string) {
	t.Helper()
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("seed: create data directory: %v", err)
	}
	storetest.CopyGoldenTo(t, dbPath)
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seed: open store: %v", err)
	}
	defer db.Close()

	startA := int64(1700000000000)
	ingestedA := startA + 120000

	entries := []ingest.StoreEntry{
		{
			Metadata: &schema.UnifiedMetadata{
				SchemaVersion: ingest.CurrentSchemaVersion,
				SessionID:     schema.SessionID(redactTestSessionA),
				ModelHarness:  defaults.HarnessClaudeCode,
				Model:         schema.ModelID("claude-opus-4-6"),
				HostSlug:      schema.HostSlug(redactTestHostSlug),
				Project: schema.ProjectContext{
					Hash:     schema.ProjectHash(redactTestProject),
					Name:     "redact-test",
					FilePath: "/test/redact",
				},
				Timestamp: schema.TimestampInfo{Start: startA, End: startA + 60000, Ingested: &ingestedA},
				Source:    schema.SourceInfo{FilePath: "/test/a.jsonl", Format: schema.SourceFormatJSONL},
			},
		},
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		t.Fatalf("seed: insert sessions: %v", err)
	}

	// Write transcript and metadata files to the peasant-sync directory.
	syncDir := filepath.Join(dataDir, "peasant-sync")
	sessionDir := filepath.Join(syncDir, redactTestHostSlug, redactTestSessionA)
	if err := os.MkdirAll(sessionDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("seed: create session dir: %v", err)
	}

	transcriptPath := filepath.Join(sessionDir, fmt.Sprintf("%s%s%s", redactTestSessionA, defaults.TranscriptPrefix, schema.SourceFormatJSONL))
	if err := os.WriteFile(transcriptPath, []byte(redactTestTranscript), defaults.PrivateFilePerm); err != nil {
		t.Fatalf("seed: write transcript: %v", err)
	}

	// Write metadata with content hash.
	meta := schema.NewUnifiedMetadata()
	meta.SessionID = schema.SessionID(redactTestSessionA)
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = schema.ModelID("claude-opus-4-6")
	meta.HostSlug = schema.HostSlug(redactTestHostSlug)
	meta.Source = schema.SourceInfo{FilePath: "/test/a.jsonl", Format: schema.SourceFormatJSONL}
	meta.Timestamp = schema.TimestampInfo{Start: startA, End: startA + 60000, Ingested: &ingestedA}
	meta.Project = schema.ProjectContext{Hash: schema.ProjectHash(redactTestProject), Name: "redact-test"}
	meta.ContentHash = schema.ComputeTranscriptHash([]byte(redactTestTranscript))
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)

	metaBytes, err := json.MarshalIndent(&meta, "", "  ")
	if err != nil {
		t.Fatalf("seed: marshal metadata: %v", err)
	}

	metadataPath := filepath.Join(sessionDir, redactTestSessionA+defaults.MetadataSuffix)
	if err := os.WriteFile(metadataPath, metaBytes, defaults.PrivateFilePerm); err != nil {
		t.Fatalf("seed: write metadata: %v", err)
	}
}

// seedDBOnlySession inserts a session into the store without writing any files
// to disk. This produces a "not_ingested" status when redacting.
func seedDBOnlySession(t *testing.T, dir, sessionID string) {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seedDBOnlySession: open store: %v", err)
	}
	defer db.Close()

	startB := int64(1700001000000)
	ingestedB := startB + 120000

	entries := []ingest.StoreEntry{
		{
			Metadata: &schema.UnifiedMetadata{
				SchemaVersion: ingest.CurrentSchemaVersion,
				SessionID:     schema.SessionID(sessionID),
				ModelHarness:  defaults.HarnessClaudeCode,
				Model:         schema.ModelID("claude-opus-4-6"),
				HostSlug:      schema.HostSlug(redactTestHostSlug),
				Project: schema.ProjectContext{
					Hash:     schema.ProjectHash(redactTestProject),
					Name:     "redact-test-dbonly",
					FilePath: "/test/dbonly",
				},
				Timestamp: schema.TimestampInfo{Start: startB, End: startB + 60000, Ingested: &ingestedB},
				Source:    schema.SourceInfo{FilePath: "/test/b.jsonl", Format: schema.SourceFormatJSONL},
			},
		},
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		t.Fatalf("seedDBOnlySession: insert session: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRedactCommand_RequiresFilter(t *testing.T) {
	t.Parallel()
	_, _, err := executeRedactCmd(t, t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error when no filter provided")
	}
	if !strings.Contains(err.Error(), "at least one filter") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRedactCommand_InvalidLevel(t *testing.T) {
	t.Parallel()
	_, _, err := executeRedactCmd(t, t.TempDir(), []string{"--all", "--level", "ultra"})
	if err == nil {
		t.Fatal("expected error for invalid level")
	}
	if !strings.Contains(err.Error(), "invalid redaction level") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRedactCommand_Help(t *testing.T) {
	t.Parallel()
	stdout, _, err := executeRedactCmd(t, t.TempDir(), []string{"--help"})
	if err != nil {
		t.Fatalf("redact --help: unexpected error: %v", err)
	}
	if !strings.Contains(stdout, "redact") {
		t.Error("help output should mention 'redact'")
	}
	if !strings.Contains(stdout, "--session") {
		t.Error("help output should mention --session flag")
	}
	if !strings.Contains(stdout, "--dry-run") {
		t.Error("help output should mention --dry-run flag")
	}
	if !strings.Contains(stdout, "--level") {
		t.Error("help output should mention --level flag")
	}
}

// NOTE: the redact command resolves its peasant-sync directory via
// resolveOutputSyncDir(cmd), which now honors --data-dir. The shared
// executeWithDataDir helper injects --data-dir=dir, so both the DB and the
// sync dir resolve consistently to <dir>/peasant for the same dir passed to
// the seed helpers and executeRedactCmd — no XDG_DATA_HOME mutation needed,
// so these file-writing tests run with t.Parallel().
func TestRedactCommand_DryRun(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedRedactTestSession(t, dataHome)

	stdout, _, err := executeRedactCmd(t, dataHome, []string{"--session", redactTestSessionA, "--dry-run"})
	if err != nil {
		t.Fatalf("redact --session --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "would be redacted") {
		t.Errorf("expected 'would be redacted' in output; got: %s", stdout)
	}

	// Verify transcript was NOT modified (dry run).
	syncDir := filepath.Join(dataHome, "peasant", "peasant-sync")
	transcriptPath := filepath.Join(syncDir, redactTestHostSlug, redactTestSessionA,
		fmt.Sprintf("%s%s%s", redactTestSessionA, defaults.TranscriptPrefix, schema.SourceFormatJSONL))
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("read transcript after dry run: %v", err)
	}
	if string(data) != redactTestTranscript {
		t.Error("transcript should not be modified during dry run")
	}
}

func TestRedactCommand_DryRun_JSON(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedRedactTestSession(t, dataHome)

	stdout, _, err := executeRedactCmd(t, dataHome, []string{"--session", redactTestSessionA, "--dry-run", "--json"})
	if err != nil {
		t.Fatalf("redact --dry-run --json: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("JSON parse error: %v\noutput: %s", err, stdout)
	}
	if result["dry_run"] != true {
		t.Errorf("expected dry_run=true; got: %v", result)
	}
	redacted, ok := result["redacted"].(float64)
	if !ok || redacted != 1 {
		t.Errorf("expected redacted=1; got: %v", result)
	}
}

func TestRedactCommand_Redact_UpdatesFiles(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedRedactTestSession(t, dataHome)

	stdout, _, err := executeRedactCmd(t, dataHome, []string{"--session", redactTestSessionA, "--level", string(redact.Standard)})
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if !strings.Contains(stdout, "redacted 1 session(s)") {
		t.Errorf("expected 'redacted 1 session(s)'; got: %s", stdout)
	}

	// Verify metadata was updated with RedactionInfo.
	syncDir := filepath.Join(dataHome, "peasant", "peasant-sync")
	metadataPath := filepath.Join(syncDir, redactTestHostSlug, redactTestSessionA,
		redactTestSessionA+defaults.MetadataSuffix)
	metaBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta schema.UnifiedMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if !meta.Redaction.Applied {
		t.Error("expected Redaction.Applied = true")
	}
	if meta.Redaction.Level != redact.Standard.String() {
		t.Errorf("expected Redaction.Level = %q, got %q", redact.Standard.String(), meta.Redaction.Level)
	}
	if meta.Redaction.RedactedAtMs == nil {
		t.Error("expected Redaction.RedactedAtMs to be set")
	}
	if meta.Redaction.ContentHashAtRedact == "" {
		t.Error("expected Redaction.ContentHashAtRedact to be set")
	}

	// Verify the content hash was updated.
	transcriptPath := filepath.Join(syncDir, redactTestHostSlug, redactTestSessionA,
		fmt.Sprintf("%s%s%s", redactTestSessionA, defaults.TranscriptPrefix, schema.SourceFormatJSONL))
	transcriptData, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	actualHash := schema.ComputeTranscriptHash(transcriptData)
	if meta.ContentHash != actualHash {
		t.Errorf("ContentHash mismatch: metadata=%s, actual=%s", meta.ContentHash, actualHash)
	}
	if meta.Redaction.ContentHashAtRedact != actualHash {
		t.Errorf("ContentHashAtRedact mismatch: metadata=%s, actual=%s", meta.Redaction.ContentHashAtRedact, actualHash)
	}
}

func TestRedactCommand_SkipsAlreadyCurrent(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedRedactTestSession(t, dataHome)

	// First: redact the session.
	_, _, err := executeRedactCmd(t, dataHome, []string{"--session", redactTestSessionA, "--level", string(redact.Standard)})
	if err != nil {
		t.Fatalf("first redact: %v", err)
	}

	// Second: run again — should skip.
	stdout, _, err := executeRedactCmd(t, dataHome, []string{"--session", redactTestSessionA, "--level", string(redact.Standard)})
	if err != nil {
		t.Fatalf("second redact: %v", err)
	}
	if !strings.Contains(stdout, "skipped 1") {
		t.Errorf("expected 'skipped 1' in output; got: %s", stdout)
	}
}

func TestRedactCommand_All(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedRedactTestSession(t, dataHome)

	stdout, _, err := executeRedactCmd(t, dataHome, []string{"--all", "--dry-run"})
	if err != nil {
		t.Fatalf("redact --all --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "1 would be redacted") {
		t.Errorf("expected '1 would be redacted'; got: %s", stdout)
	}
}

func TestRedactCommand_InvalidSessionID(t *testing.T) {
	// Errors at NewSessionID before the env-only sync dir is resolved, so this
	// is parallel-safe via --data-dir injection.
	t.Parallel()
	dir := t.TempDir()
	seedRedactTestSession(t, dir)

	_, _, err := executeRedactCmd(t, dir, []string{"--session", "not-a-valid-uuid"})
	if err == nil {
		t.Fatal("expected error for invalid session ID")
	}
	if !strings.Contains(err.Error(), "invalid session ID") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRedactCommand_JSON_StatusValues verifies that --json output contains
// correct RedactStatusEnum values in individual session results. Exercises
// three status outcomes in a single --all --json invocation:
//   - "redacted" — session has files and was successfully redacted
//   - "not_ingested" — session exists in DB but has no local files
//
// Then re-runs to verify:
//   - "skipped" — session already redacted at the current level
//
// TestRedactCommand_Redact_PopulatesRuleSetVersion verifies AC9:
// after redaction, RedactionInfo.RuleSetVersion is populated with the current
// rule set version string and is never empty.
func TestRedactCommand_Redact_PopulatesRuleSetVersion(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedRedactTestSession(t, dataHome)

	_, _, err := executeRedactCmd(t, dataHome, []string{"--session", redactTestSessionA, "--level", string(redact.Standard)})
	if err != nil {
		t.Fatalf("redact: %v", err)
	}

	syncDir := filepath.Join(dataHome, "peasant", "peasant-sync")
	metadataPath := filepath.Join(syncDir, redactTestHostSlug, redactTestSessionA,
		redactTestSessionA+defaults.MetadataSuffix)
	metaBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta schema.UnifiedMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if meta.Redaction.RuleSetVersion == "" {
		t.Error("expected Redaction.RuleSetVersion to be non-empty after redaction")
	}
	if meta.Redaction.RuleSetVersion != redact.Version() {
		t.Errorf("expected Redaction.RuleSetVersion=%q, got %q", redact.Version(), meta.Redaction.RuleSetVersion)
	}
}

// TestRedactCommand_BackwardCompat_MissingRuleSetVersion verifies that existing
// metadata without rule_set_version parses correctly (backward compatibility).
func TestRedactCommand_BackwardCompat_MissingRuleSetVersion(t *testing.T) {
	t.Parallel()
	// A metadata JSON blob that predates the rule_set_version field.
	legacyJSON := `{
		"schemaVersion": 8,
		"sessionId": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"modelHarness": "claude",
		"model": "claude-opus-4-6",
		"version": "",
		"timestamp": {"start": 0, "end": 0},
		"source": {"format": "jsonl"},
		"git": {},
		"project": {"hash": "","name": ""},
		"hostSlug": "",
		"stats": {"turnCount":0,"toolCallCount":0,"subagentCount":0,"durationMs":0,"tokensIn":0,"tokensOut":0},
		"subagents": [],
		"diagnostics": {"warnings": []},
		"contentHash": "",
		"metadataHash": "",
		"redaction": {
			"applied": true,
			"level": "standard",
			"redacted_at_ms": 1700000000000,
			"content_hash_at_redact": "abc123"
		}
	}`
	var meta schema.UnifiedMetadata
	if err := json.Unmarshal([]byte(legacyJSON), &meta); err != nil {
		t.Fatalf("parse legacy metadata: %v", err)
	}
	if meta.Redaction.RuleSetVersion != "" {
		t.Errorf("expected empty RuleSetVersion for legacy metadata, got %q", meta.Redaction.RuleSetVersion)
	}
	if !meta.Redaction.Applied {
		t.Error("expected Redaction.Applied = true for legacy metadata")
	}
	if meta.Redaction.Level != string(redact.Standard) {
		t.Errorf("expected Redaction.Level = %q, got %q", redact.Standard, meta.Redaction.Level)
	}
}

func TestRedactCommand_JSON_StatusValues(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	// Seed session A with transcript files on disk.
	seedRedactTestSession(t, dataHome)
	// Seed session B in DB only — no files written to disk.
	seedDBOnlySession(t, dataHome, redactTestSessionB)

	// The first run produces "redacted" and "not_ingested" outcomes.
	stdout, _, err := executeRedactCmd(t, dataHome, []string{"--all", "--json", "--level", string(redact.Standard)})
	if err != nil {
		t.Fatalf("round 1: redact --all --json: %v", err)
	}

	var round1 struct {
		Redacted    int                   `json:"redacted"`
		Skipped     int                   `json:"skipped"`
		NotIngested int                   `json:"not_ingested"`
		Errors      int                   `json:"errors"`
		Sessions    []redactSessionResult `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(stdout), &round1); err != nil {
		t.Fatalf("round 1: JSON parse: %v\noutput: %s", err, stdout)
	}

	// Verify summary counts.
	if round1.Redacted != 1 {
		t.Errorf("round 1: expected redacted=1, got %d", round1.Redacted)
	}
	if round1.NotIngested != 1 {
		t.Errorf("round 1: expected not_ingested=1, got %d", round1.NotIngested)
	}
	if round1.Errors != 0 {
		t.Errorf("round 1: expected errors=0, got %d", round1.Errors)
	}

	// Build a lookup from session_id → status for per-session assertions.
	statusByID := make(map[string]RedactStatusEnum, len(round1.Sessions))
	for _, s := range round1.Sessions {
		statusByID[s.SessionID] = s.Status
	}

	if got := statusByID[redactTestSessionA]; got != RedactStatus.Redacted {
		t.Errorf("round 1: session A: expected status=%q, got %q", RedactStatus.Redacted, got)
	}
	if got := statusByID[redactTestSessionB]; got != RedactStatus.NotIngested {
		t.Errorf("round 1: session B: expected status=%q, got %q", RedactStatus.NotIngested, got)
	}

	// Re-running produces "skipped" for the already-redacted session.
	stdout2, _, err := executeRedactCmd(t, dataHome, []string{"--session", redactTestSessionA, "--json", "--level", string(redact.Standard)})
	if err != nil {
		t.Fatalf("round 2: redact --json: %v", err)
	}

	var round2 struct {
		Skipped  int                   `json:"skipped"`
		Sessions []redactSessionResult `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(stdout2), &round2); err != nil {
		t.Fatalf("round 2: JSON parse: %v\noutput: %s", err, stdout2)
	}

	if round2.Skipped != 1 {
		t.Errorf("round 2: expected skipped=1, got %d", round2.Skipped)
	}
	if len(round2.Sessions) != 1 {
		t.Fatalf("round 2: expected 1 session result, got %d", len(round2.Sessions))
	}
	if got := round2.Sessions[0].Status; got != RedactStatus.Skipped {
		t.Errorf("round 2: session A: expected status=%q, got %q", RedactStatus.Skipped, got)
	}
}
