package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/turn_count_invariants.yaml
var turnCountInvariantsYAML []byte

type turnCountInvariantCase struct {
	Name                        string `yaml:"name"`
	Path                        string `yaml:"path"`
	StoredTurnCount             int    `yaml:"storedTurnCount"`
	StoredConversationTurnCount int    `yaml:"storedConversationTurnCount"`
	WantTurnCount               int    `yaml:"wantTurnCount"`
	WantConversationTurnCount   int    `yaml:"wantConversationTurnCount"`
	// SessionID, NativeRows and Rollout state the native records of a row that
	// does not read the shared Claude capture: OpenCode message rows, or the
	// lines of a Codex rollout.
	SessionID  string                    `yaml:"sessionId"`
	NativeRows []assistantEntryNativeRow `yaml:"nativeRows"`
	Rollout    []string                  `yaml:"rollout"`
	// WantToolCallCount is the session's tool call count after the metrics
	// recompute. It is required on the managed OpenCode and Codex rows.
	WantToolCallCount *int `yaml:"wantToolCallCount"`
}

type turnCountInvariantFixture struct {
	RequiredNames []string                 `yaml:"requiredNames"`
	Capture       []string                 `yaml:"capture"`
	Cases         []turnCountInvariantCase `yaml:"cases"`
}

func loadTurnCountInvariantFixture(t *testing.T) turnCountInvariantFixture {
	t.Helper()
	var fixture turnCountInvariantFixture
	if err := testutil.DecodeFixtureYAML(turnCountInvariantsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	required := make(map[string]bool, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		required[name] = true
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if seen[c.Name] {
			t.Fatalf("turn count invariant fixture repeats case %q", c.Name)
		}
		seen[c.Name] = true
		if c.WantTurnCount == c.WantConversationTurnCount {
			t.Fatalf("case %q expects equal counts; the two counts must differ so a swapped column fails", c.Name)
		}
		if (c.Path == "managed-opencode" || c.Path == "managed-codex") && c.WantToolCallCount == nil {
			t.Fatalf("case %q states no wantToolCallCount; a managed OpenCode or Codex row must pin it", c.Name)
		}
	}
	for name := range required {
		if !seen[name] {
			t.Fatalf("turn count invariant fixture is missing required case %q", name)
		}
	}
	return fixture
}

func openTurnCountInvariantStore(t *testing.T) *store.Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	return storetest.OpenWith(t, store.WithPoolSize(2), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
}

// assertCaptureHoldsUncountedRows is the precondition of the invariant rows:
// the stored entries include a system row, a top-level tool row and an image,
// so the two counts are measured over rows the allowed list must skip.
func assertCaptureHoldsUncountedRows(t *testing.T, entries []schema.SessionEntry) {
	t.Helper()
	var system, tool, image bool
	for _, entry := range entries {
		switch {
		case entry.Role == schema.RoleSystem:
			system = true
		case entry.EntryType == schema.EntryTypeImage:
			image = true
		case entry.Depth == 0 && (entry.EntryType == schema.EntryTypeToolUse || entry.EntryType == schema.EntryTypeToolResult):
			tool = true
		}
	}
	if !system || !tool || !image {
		t.Fatalf("the capture must hold a system row, a top-level tool row and an image; got system=%t tool=%t image=%t", system, tool, image)
	}
}

// startTranscriptServer serves the real transcript reads over the store and
// returns the server's base URL.
func startTranscriptServer(t *testing.T, db *store.Store) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(api.ServerConfig{Port: 0, Store: db})
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && err != context.Canceled {
			t.Errorf("transcript server shutdown: %v", err)
		}
	})
	return "http://" + srv.Addr().String()
}

// servedTranscriptDirectory reads the segment directory through the real
// server over the store.
func servedTranscriptDirectory(t *testing.T, db *store.Store, sessionID string) (int, schema.TranscriptSegmentDirectory, string) {
	t.Helper()
	return readTranscriptDirectory(t, startTranscriptServer(t, db), sessionID)
}

func readTranscriptDirectory(t *testing.T, base, sessionID string) (int, schema.TranscriptSegmentDirectory, string) {
	t.Helper()
	response, err := http.Get(base + "/api/v1/sessions/" + sessionID + "/transcript/segments")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var directory schema.TranscriptSegmentDirectory
	if response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &directory); err != nil {
			t.Fatalf("decode directory: %v", err)
		}
	}
	return response.StatusCode, directory, string(body)
}

// servedSegmentTurns reads every segment the directory names and returns the
// turns they serve, in order. A directory folded from other entries than its
// segments fails here: the segment read refuses a range it cannot reproduce.
func servedSegmentTurns(t *testing.T, base, sessionID string, directory schema.TranscriptSegmentDirectory) []schema.TranscriptTurn {
	t.Helper()
	var turns []schema.TranscriptTurn
	for _, ref := range directory.Segments {
		response, err := http.Get(fmt.Sprintf("%s/api/v1/sessions/%s/transcript/segments/%d", base, sessionID, ref.Index))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("segment %d status = %d, body %s", ref.Index, response.StatusCode, body)
		}
		var segment schema.TranscriptSegment
		if err := json.Unmarshal(body, &segment); err != nil {
			t.Fatalf("decode segment %d: %v", ref.Index, err)
		}
		turns = append(turns, segment.Turns...)
	}
	return turns
}

// TestSessionTurnCountsAgreeWithTheSegmentDirectory proves the two equalities
// for sessions with stored entries, managed and unmanaged, and that an
// entry-less session serves its stored counts.
func TestSessionTurnCountsAgreeWithTheSegmentDirectory(t *testing.T) {
	t.Parallel()
	fixture := loadTurnCountInvariantFixture(t)
	capture := []byte(strings.Join(fixture.Capture, "\n") + "\n")
	for _, c := range fixture.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := openTurnCountInvariantStore(t)
			sessionID := testutil.TestSessionUUID
			sid, err := ingest.NewSessionID(sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if c.SessionID != "" {
				sessionID = c.SessionID
				if sid, err = ingest.NewSessionID(sessionID); err != nil {
					t.Fatal(err)
				}
			}
			storetest.SeedSession(t, db, sessionID)
			mfs := testutil.NewMemFS()
			sourcePath := "/native/" + sessionID + ".jsonl"
			if err := mfs.WriteFile(sourcePath, capture, 0644); err != nil {
				t.Fatal(err)
			}
			discovered := ingest.DiscoveredSession{
				SessionID: sid, Harness: ingest.HarnessClaudeCode,
				SourcePath: ingest.ResolvedPath(sourcePath), SourceFormat: ingest.SourceFormatJSONL,
			}
			indexer := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true), ingest.WithClaudeFullContent(true))

			switch c.Path {
			case "unmanaged":
				entries, err := indexer.IndexTranscript(ctx, discovered)
				if err != nil {
					t.Fatalf("IndexTranscript: %v", err)
				}
				assertCaptureHoldsUncountedRows(t, entries)
				if err := testutil.WriteFullEntries(ctx, db, sid, entries); err != nil {
					t.Fatalf("write entries: %v", err)
				}
			case "managed":
				metadata := makeReindexMeta(t, sessionID, sourcePath)
				metadata.ContentHash = schema.ComputeTranscriptHash(capture)
				candidate, err := indexer.BuildGeneration(ctx, ingest.GenerationBuildInput{
					Session: discovered, GenerationID: "gen-turn-count-invariant",
					RecordedMetadata: metadata, CapturedTranscript: capture,
				})
				if err != nil {
					t.Fatalf("BuildGeneration: %v", err)
				}
				assertCaptureHoldsUncountedRows(t, candidate.Result.Generation.Main.Entries)
				assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
					Harness: ingest.HarnessClaudeCode, Result: candidate.Result,
					Policy: ingest.CaptureFreshCandidate, Authoritative: true,
				})
				if err != nil {
					t.Fatalf("AssessCapture: %v", err)
				}
				contentCapture, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1)
				if err != nil {
					t.Fatalf("content capture: %v", err)
				}
				outcome, err := db.ActivateGenerationCandidate(ctx, ingest.GenerationActivation{
					Generation: candidate.Result, Blobs: candidate.Blobs,
					IndexerVersion: 1, IndexedAtMs: 1, ContentCapture: contentCapture,
				})
				if err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("activate the managed generation: outcome=%+v err=%v", outcome, err)
				}
			case "managed-opencode":
				setSeededSessionHarness(t, db, sessionID, ingest.HarnessOpenCode)
				candidate := buildOpenCodeTurnCountCandidate(t, c, sid)
				activateTurnCountCandidate(t, db, ingest.HarnessOpenCode, ingest.TranscriptOriginOpenCodeCurrentSQLite, candidate)
				recomputeTurnCountMetrics(t, db, sid)
			case "managed-codex":
				setSeededSessionHarness(t, db, sessionID, ingest.HarnessCodex)
				candidate := buildCodexTurnCountCandidate(t, c, sid)
				activateTurnCountCandidate(t, db, ingest.HarnessCodex, ingest.TranscriptOriginFile, candidate)
				recomputeTurnCountMetrics(t, db, sid)
			case "entry-less-unmanaged", "entry-less-managed":
				turnCount, conversationTurnCount := c.StoredTurnCount, c.StoredConversationTurnCount
				if _, err := db.UpsertCapturedStats(ctx, store.CapturedStats{
					SessionID: schema.SessionID(sid), TurnCount: &turnCount, ConversationTurnCount: &conversationTurnCount,
					Source: store.StatsSourceHarness, UpdatedAtMs: 1,
				}); err != nil {
					t.Fatal(err)
				}
				if c.Path == "entry-less-managed" {
					markSessionManagedWithoutEntries(t, db, sessionID)
				}
				assertEntryLessSessionServesStoredCounts(t, db, sessionID, c)
				return
			default:
				t.Fatalf("case %q has unknown path %q", c.Name, c.Path)
			}

			detail, err := export.ExportSession(ctx, db, mfs, sessionID)
			if err != nil {
				t.Fatalf("read the session detail: %v", err)
			}
			base := startTranscriptServer(t, db)
			status, directory, body := readTranscriptDirectory(t, base, sessionID)
			if status != http.StatusOK {
				t.Fatalf("directory status = %d, body %s", status, body)
			}
			segmentTurns := servedSegmentTurns(t, base, sessionID, directory)
			if len(segmentTurns) != c.WantTurnCount {
				t.Errorf("the segments serve %d turns, want %d", len(segmentTurns), c.WantTurnCount)
			}
			for i := range segmentTurns {
				if i < len(detail.Turns) && segmentTurns[i].Index != detail.Turns[i].Index {
					t.Errorf("segment turn %d has identity %d, the detail's has %d", i, segmentTurns[i].Index, detail.Turns[i].Index)
				}
			}
			stats, err := db.ReadCapturedStats(ctx, schema.SessionID(sid))
			if err != nil {
				t.Fatal(err)
			}
			if detail.TurnCount != c.WantTurnCount {
				t.Errorf("detail turnCount = %d, want %d", detail.TurnCount, c.WantTurnCount)
			}
			if directory.TotalTurns != c.WantTurnCount {
				t.Errorf("directory totalTurns = %d, want %d", directory.TotalTurns, c.WantTurnCount)
			}
			if len(detail.Turns) != c.WantTurnCount {
				t.Errorf("detail serves %d turns, want %d", len(detail.Turns), c.WantTurnCount)
			}
			if detail.ConversationTurnCount == nil || *detail.ConversationTurnCount != int64(c.WantConversationTurnCount) {
				t.Errorf("detail conversationTurnCount = %v, want %d", detail.ConversationTurnCount, c.WantConversationTurnCount)
			}
			if directory.TotalConversationTurns == nil || *directory.TotalConversationTurns != c.WantConversationTurnCount {
				t.Errorf("directory totalConversationTurns = %v, want %d", directory.TotalConversationTurns, c.WantConversationTurnCount)
			}
			if stats.ConversationTurnCount == nil || *stats.ConversationTurnCount != c.WantConversationTurnCount {
				t.Errorf("stored conversation_turn_count = %v, want %d", stats.ConversationTurnCount, c.WantConversationTurnCount)
			}
			if stats.TurnCount == nil || *stats.TurnCount != c.WantTurnCount {
				t.Errorf("stored turn_count = %v, want %d", stats.TurnCount, c.WantTurnCount)
			}
			if c.WantToolCallCount != nil {
				if detail.ToolCallCount != *c.WantToolCallCount {
					t.Errorf("detail toolCallCount = %d, want %d", detail.ToolCallCount, *c.WantToolCallCount)
				}
				if stats.ToolCallCount == nil || *stats.ToolCallCount != *c.WantToolCallCount {
					t.Errorf("stored tool_call_count = %v, want %d", stats.ToolCallCount, *c.WantToolCallCount)
				}
			}
		})
	}
}

// setSeededSessionHarness makes the seeded session row name the harness whose
// generation the row activates.
func setSeededSessionHarness(t *testing.T, db *store.Store, sessionID string, harness ingest.Harness) {
	t.Helper()
	conn, err := db.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET model_harness = ? WHERE session_id = ?`,
		&sqlitex.ExecOptions{Args: []any{string(harness), sessionID}}); err != nil {
		t.Fatal(err)
	}
}

// buildOpenCodeTurnCountCandidate decodes the row's native OpenCode message
// rows into a history snapshot and builds the managed generation from it with
// the indexer's BuildGeneration, the build a harvest runs.
func buildOpenCodeTurnCountCandidate(t *testing.T, c turnCountInvariantCase, sid ingest.SessionID) ingest.GenerationCandidate {
	t.Helper()
	if len(c.NativeRows) == 0 {
		t.Fatalf("case %q has no native rows", c.Name)
	}
	scope := ingest.OpenCodeProvenanceScope{SessionID: c.SessionID, Shape: ingest.OpenCodeProvenanceCurrent, ParentNullProven: true}
	snapshot := ingest.OpenCodeHistorySnapshot{SessionID: c.SessionID}
	for _, native := range c.NativeRows {
		if !json.Valid([]byte(native.Data)) {
			t.Fatalf("case %q row %q data is not valid JSON", c.Name, native.ID)
		}
		decoded, settled, err := ingest.DecodeOpenCodeProvenanceRow(ingest.OpenCodeProvenanceRow{
			ID: native.ID, SessionID: scope.SessionID, Type: native.Type,
			TimeCreated: native.TimeCreated, TimeUpdated: native.TimeUpdated,
			Seq: native.Seq, HasSeq: true, Data: native.Data,
		}, scope)
		if err != nil {
			t.Fatalf("row %q decode: %v", native.ID, err)
		}
		snapshot.Messages = append(snapshot.Messages, ingest.OpenCodeHistoryRow{
			MessageID: native.ID, Shape: scope.Shape, Seq: native.Seq, HasSeq: true, NativeType: native.Type,
			CreatedMs: native.TimeCreated, UpdatedMs: native.TimeUpdated, Settled: settled, Message: decoded,
		})
	}
	snapshot.SourceEvidenceDigest = ingest.DigestOpenCodeSnapshot(snapshot)
	metadata := schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: sid, ModelHarness: ingest.HarnessOpenCode}
	candidate, err := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}).BuildGeneration(context.Background(), ingest.GenerationBuildInput{
		Session:          ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessOpenCode, TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite},
		GenerationID:     "gen-turn-count-opencode",
		RecordedMetadata: &metadata,
		Prior:            generationTestPriorFromOpenCode(t, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState(), HasCompleteGeneration: true}),
		OpenCode: generationTestOpenCodeSource{snapshot: func(context.Context, string) (ingest.OpenCodeHistorySnapshot, error) {
			return snapshot, nil
		}},
	})
	if err != nil {
		t.Fatalf("build the OpenCode generation: %v", err)
	}
	return candidate
}

// buildCodexTurnCountCandidate writes the row's rollout lines to a file and
// builds the managed generation from it with the indexer's BuildGeneration.
func buildCodexTurnCountCandidate(t *testing.T, c turnCountInvariantCase, sid ingest.SessionID) ingest.GenerationCandidate {
	t.Helper()
	if len(c.Rollout) == 0 {
		t.Fatalf("case %q has no rollout lines", c.Name)
	}
	for i, line := range c.Rollout {
		if !json.Valid([]byte(line)) {
			t.Fatalf("case %q rollout line %d is not valid JSON: %s", c.Name, i, line)
		}
	}
	path := filepath.Join(t.TempDir(), "rollout-2026-01-02T00-00-00-"+c.SessionID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(c.Rollout, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := ingest.NewCodexIndexer(&ingest.OSFileSystem{}).BuildGeneration(context.Background(), ingest.GenerationBuildInput{
		Session:      ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessCodex, SourcePath: ingest.ResolvedPath(path), SourceFormat: ingest.SourceFormatJSONL},
		GenerationID: "gen-turn-count-codex",
	})
	if err != nil {
		t.Fatalf("build the Codex generation: %v", err)
	}
	return candidate
}

// activateTurnCountCandidate assesses the candidate as a fresh authoritative
// capture and installs it as the session's active generation.
func activateTurnCountCandidate(t *testing.T, db *store.Store, harness ingest.Harness, origin ingest.TranscriptOrigin, candidate ingest.GenerationCandidate) {
	t.Helper()
	if len(candidate.Result.Generation.Main.Entries) == 0 {
		t.Fatal("the built generation holds no main entries")
	}
	assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
		Harness: harness, Result: candidate.Result, Policy: ingest.CaptureFreshCandidate, Authoritative: true,
	})
	if err != nil {
		t.Fatalf("AssessCapture: %v", err)
	}
	contentCapture, err := assessment.ContentCapture(ingest.ContentSourceProviderSource, origin, 1)
	if err != nil {
		t.Fatalf("content capture: %v", err)
	}
	outcome, err := db.ActivateGenerationCandidate(context.Background(), ingest.GenerationActivation{
		Generation: candidate.Result, Blobs: candidate.Blobs, PriorEvidence: candidate.PriorEvidence,
		IndexerVersion: ingest.GenerationRepairTargets[harness].IndexerVersion, IndexedAtMs: 1, ContentCapture: contentCapture,
	})
	if err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("activate the %s generation: outcome=%+v err=%v", harness, outcome, err)
	}
}

// recomputeTurnCountMetrics runs the production metrics recompute for the
// session, so a tool call count read after it is the one a harvest leaves.
func recomputeTurnCountMetrics(t *testing.T, db *store.Store, sid ingest.SessionID) {
	t.Helper()
	if _, err := metrics.NewEngine(db).RecomputeMetrics(context.Background(), []ingest.SessionID{sid}); err != nil {
		t.Fatalf("recompute metrics: %v", err)
	}
}

// markSessionManagedWithoutEntries points the session at a managed generation
// that emitted no entries, the state a store can hold after its content was
// reclaimed.
func markSessionManagedWithoutEntries(t *testing.T, db *store.Store, sessionID string) {
	t.Helper()
	conn, err := db.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	digest := strings.Repeat("a", 64)
	generationID := "generation-entry-less"
	if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_generations(session_id, generation_id, schema_version, harness, model, version, ts_start, ts_end, ts_ingested,
  source_file_path, source_format, project_hash, project_name, host_slug, content_hash, metadata_hash,
  redaction_applied, completeness, source_evidence_digest, index_format_version, candidate_digest, installed_at_ms)
VALUES(?, ?, 12, 'claude-code', 'fixture-model', '', 1, 2, 3,
  '/synthetic/entry-less.jsonl', 'jsonl', 'testprojhash0000000000000000000000000000000000000000000000000000', 'testproj', 'testslug', ?, ?,
  0, 'complete', ?, 2, ?, 1)`,
		&sqlitex.ExecOptions{Args: []any{sessionID, generationID, digest, digest, digest, digest}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET active_generation_id = ? WHERE session_id = ?`,
		&sqlitex.ExecOptions{Args: []any{generationID, sessionID}}); err != nil {
		t.Fatal(err)
	}
}

// assertEntryLessSessionServesStoredCounts reads the session row every list
// serves and the segment directory. With no stored entries there is no fold:
// the stored counts are served and the directory states no turns.
func assertEntryLessSessionServesStoredCounts(t *testing.T, db *store.Store, sessionID string, c turnCountInvariantCase) {
	t.Helper()
	row, err := db.SessionByID(context.Background(), sessionID)
	if err != nil || row == nil {
		t.Fatalf("read the session row: row=%v err=%v", row, err)
	}
	if row.TurnCount != c.WantTurnCount || row.ConversationTurnCount != c.WantConversationTurnCount {
		t.Errorf("served counts = (%d, %d), want the stored (%d, %d)", row.TurnCount, row.ConversationTurnCount, c.WantTurnCount, c.WantConversationTurnCount)
	}
	status, directory, body := servedTranscriptDirectory(t, db, sessionID)
	if status == http.StatusOK && (directory.TotalTurns != 0 || directory.TotalSegments != 0 || len(directory.Segments) != 0) {
		t.Errorf("an entry-less session's directory states %d turns in %d segments, want none: %s", directory.TotalTurns, directory.TotalSegments, body)
	}
}
