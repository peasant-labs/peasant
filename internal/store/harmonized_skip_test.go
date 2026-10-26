package store

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type generationSkipCase struct {
	Name               string                `yaml:"name"`
	Operation          string                `yaml:"operation"`
	AdapterVersion     int                   `yaml:"adapterVersion"`
	Preview            string                `yaml:"preview"`
	TurnCount          int                   `yaml:"turnCount"`
	Purpose            schema.SessionPurpose `yaml:"purpose"`
	AliasKey           string                `yaml:"aliasKey"`
	AliasRef           schema.SourceEntryRef `yaml:"aliasRef"`
	SegmentRef         schema.SourceEntryRef `yaml:"segmentRef"`
	SegmentSource      string                `yaml:"segmentSource"`
	SegmentText        string                `yaml:"segmentText"`
	Texts              []string              `yaml:"texts"`
	ComparedGeneration string                `yaml:"comparedGeneration"`
	WantDisposition    string                `yaml:"wantDisposition"`
}

func (c generationSkipCase) disposition(t *testing.T) ingest.ActivationDisposition {
	t.Helper()
	switch c.WantDisposition {
	case "skipped":
		return ingest.ActivationSkipped
	case "committed":
		return ingest.ActivationCommittedNow
	default:
		t.Fatalf("generation skip %q has invalid expected disposition %q", c.Name, c.WantDisposition)
		return 0
	}
}

//go:embed testdata/generation_skip.yaml
var generationSkipYAML []byte

//go:embed testdata/generation_skip.manifest.yaml
var generationSkipManifestYAML []byte

func LoadGenerationSkipFixtures(t *testing.T) []generationSkipCase {
	t.Helper()
	var fixtures struct {
		Cases []generationSkipCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(generationSkipYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeRecoveryRequiredNames(generationSkipManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixtures.Cases {
		if c.Operation == "" {
			t.Fatalf("generation skip %q has no operation", c.Name)
		}
		switch c.Operation {
		case "completeness-differs", "incomplete-new-never-skips", "file-backed-never-skips":
			if len(c.Texts) != 3 {
				t.Fatalf("generation skip %q needs text, input, and output fixture values", c.Name)
			}
		case "pointer-moved-fallback":
			if c.ComparedGeneration == "" {
				t.Fatalf("generation skip %q lacks the moved comparison pointer", c.Name)
			}
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "generation skip"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

// skipSetup activates one complete harmonized generation and returns the
// store, the session, and the committed candidate with its bytes: every
// skip subtest refreshes from this state.
func skipSetup(t *testing.T, sid schema.SessionID, texts ...string) (*Store, indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	if len(texts) == 0 {
		texts = []string{"skip base text", "skip base input", "skip base output"}
	}
	v2, blobs := buildTestGeneration(t, sid, "gen_skip_base", texts[0], texts[1], texts[2])
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     filledCandidateForValidation(t, v2, blobs),
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    100,
	}); err != nil {
		t.Fatalf("activate skip base: %v", err)
	}
	return s, v2, blobs
}

// skipProof carries the input proof a refresh presents: the pair identity
// it establishes and the consumed input digest, both valid digests.
var (
	skipArtifactIdentity = strings.Repeat("b", 64)
	skipInputHash        = strings.Repeat("a", 64)
)

// skipRefresh activates one refresh candidate and returns its outcome: the
// caller names a fresh identifier per attempt, because production
// identifiers are fresh random values and a reused identifier tests the
// immutable identity instead of the skip comparison.
func skipRefresh(t *testing.T, s *Store, sid schema.SessionID, genID string, mutate func(*indexformat.V2, map[schema.SourceEntryRef][]byte), indexerVersion int, indexedAtMs int64) (indexformat.V2, map[schema.SourceEntryRef][]byte, ingest.ActivationOutcome, error) {
	t.Helper()
	base, blobs := buildTestGeneration(t, sid, genID, "skip base text", "skip base input", "skip base output")
	if mutate != nil {
		mutate(&base, blobs)
	}
	filled := filledCandidateForValidation(t, base, blobs)
	state, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	identity := skipArtifactIdentity
	proof := skipInputHash
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:       filled,
		Blobs:            blobs,
		IndexerVersion:   indexerVersion,
		IndexedAtMs:      indexedAtMs,
		ExpectedState:    state,
		ArtifactIdentity: &identity,
		IndexedInputHash: &proof,
	})
	return filled, blobs, outcome, err
}

// countSessionGenerations counts one session's harmonized generation rows.
func countSessionGenerations(t *testing.T, s *Store, sid schema.SessionID) int {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	return countRowsOnConn(t, conn, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, string(sid))
}

// TestHarmonizedSkip drives the refresh comparison through its
// dimensions: identical refreshes skip with zero new rows and advanced
// bookkeeping, read-invisible provenance advances without a new
// generation, and every visible change writes.
func TestHarmonizedSkip(t *testing.T) {
	sid, err := schema.NewSessionID("d4d4d4d4-d4d4-44d4-84d4-d4d4d4d4d4d4")
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range LoadGenerationSkipFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Operation {
			case "identical":
				s, _, _ := skipSetup(t, sid)
				beforeGens := countSessionGenerations(t, s, sid)
				beforeBodies := countMappingBodies(t, s, sid)
				_, _, outcome, err := skipRefresh(t, s, sid, "gen_skip_identical", nil, 1, 200)
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Disposition != c.disposition(t) {
					t.Fatalf("identical refresh disposition = %v, want skipped", outcome.Disposition)
				}
				if got := countSessionGenerations(t, s, sid); got != beforeGens {
					t.Fatalf("skip wrote %d generation rows, want %d", got, beforeGens)
				}
				if got := countMappingBodies(t, s, sid); got != beforeBodies {
					t.Fatalf("skip wrote bodies %d, want %d", got, beforeBodies)
				}
				// Every bookkeeping stamp advanced: a second harvest selects
				// nothing because the input proof matches the input.
				state, err := s.ReadIndexState(context.Background(), sid)
				if err != nil {
					t.Fatal(err)
				}
				if state.IndexedAt == nil || *state.IndexedAt != 200 {
					t.Fatalf("skip indexed_at = %v, want the refresh time 200", state.IndexedAt)
				}
				if state.IndexedInputHash == nil || *state.IndexedInputHash != skipInputHash {
					t.Fatalf("skip input proof = %v, want the consumed input", state.IndexedInputHash)
				}
			case "adapter-only":
				s, _, _ := skipSetup(t, sid)
				version := c.AdapterVersion
				_, _, outcome, err := skipRefresh(t, s, sid, "gen_skip_adapter", func(v2 *indexformat.V2, _ map[schema.SourceEntryRef][]byte) {
					v2.Generation.Metadata.AdapterVersion = &version
				}, 1, 200)
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Disposition != c.disposition(t) {
					t.Fatalf("adapter-only refresh disposition = %v, want skipped", outcome.Disposition)
				}
				// The session row advances while the immutable generation row
				// keeps its older adapter revision.
				conn, err := s.pool.Take(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer s.pool.Put(conn)
				var sessionAdapter *int
				_ = sqlitex.ExecuteTransient(conn, `SELECT adapter_version FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
					Args: []any{string(sid)},
					ResultFunc: func(stmt *sqlite.Stmt) error {
						if stmt.ColumnType(0) != sqlite.TypeNull {
							v := stmt.ColumnInt(0)
							sessionAdapter = &v
						}
						return nil
					},
				})
				if sessionAdapter == nil || *sessionAdapter != version {
					t.Fatalf("session adapter = %v, want %d", sessionAdapter, version)
				}
				var rowAdapter *int
				_ = sqlitex.ExecuteTransient(conn, `SELECT adapter_version FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
					Args: []any{string(sid), "gen_skip_base"},
					ResultFunc: func(stmt *sqlite.Stmt) error {
						if stmt.ColumnType(0) != sqlite.TypeNull {
							v := stmt.ColumnInt(0)
							rowAdapter = &v
						}
						return nil
					},
				})
				if rowAdapter != nil {
					t.Fatalf("generation row adapter = %v, want NULL (rows are immutable)", rowAdapter)
				}
			case "read-visible-metadata":
				s, _, _ := skipSetup(t, sid)
				_, _, outcome, err := skipRefresh(t, s, sid, "gen_skip_purpose", func(v2 *indexformat.V2, _ map[schema.SourceEntryRef][]byte) {
					v2.Generation.Metadata.Purpose = c.Purpose
				}, 1, 200)
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Disposition != c.disposition(t) {
					t.Fatalf("purpose change disposition = %v, want a new generation", outcome.Disposition)
				}
			case "one-entry-changed":
				s, _, _ := skipSetup(t, sid)
				_, _, outcome, err := skipRefresh(t, s, sid, "gen_skip_changed", func(v2 *indexformat.V2, _ map[schema.SourceEntryRef][]byte) {
					altered := c.Preview
					v2.Generation.Main.Entries[0].ContentPreview = &altered
				}, 1, 200)
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Disposition != c.disposition(t) {
					t.Fatalf("changed entry disposition = %v, want a new generation", outcome.Disposition)
				}
			case "alias-changed":
				s, _, _ := skipSetup(t, sid)
				_, _, outcome, err := skipRefresh(t, s, sid, "gen_skip_alias", func(v2 *indexformat.V2, _ map[schema.SourceEntryRef][]byte) {
					v2.Generation.Aliases = []indexformat.NativeAlias{{NativeKey: c.AliasKey, Ref: c.AliasRef}}
				}, 1, 200)
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Disposition != c.disposition(t) {
					t.Fatalf("alias change disposition = %v, want a new generation", outcome.Disposition)
				}
			case "segment-changed":
				s, _, _ := skipSetup(t, sid)
				_, _, outcome, err := skipRefresh(t, s, sid, "gen_skip_segment", func(v2 *indexformat.V2, blobs map[schema.SourceEntryRef][]byte) {
					retained := c.SegmentRef
					v2.Generation.Segments = []indexformat.ContextSegment{{
						Ordinal:          0,
						PhysicalSourceID: c.SegmentSource,
						Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
						Inclusion:        indexformat.SegmentInclusionInherited,
						CapturedRefs:     []schema.SourceEntryRef{retained},
					}}
					v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: retained})
					blobs[retained] = []byte(c.SegmentText)
				}, 1, 200)
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Disposition != c.disposition(t) {
					t.Fatalf("segment change disposition = %v, want a new generation", outcome.Disposition)
				}
			case "completeness-differs":
				s, _, _ := skipSetup(t, sid)
				incomplete, incompleteBlobs := buildIncompleteGeneration(t, sid, "gen_skip_incomplete", c.Texts[0], c.Texts[1], c.Texts[2])
				_ = incompleteBlobs
				_, err := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     filledCandidateForValidation(t, incomplete, incompleteBlobs),
					Blobs:          incompleteBlobs,
					ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete},
				})
				if err == nil {
					t.Fatal("incomplete_new over a complete generation succeeded; the last-good guard must refuse it")
				}
				if got := visibleGeneration(t, s, sid); got != "gen_skip_base" {
					t.Fatalf("visible = %q, want the complete last-good generation", got)
				}
			case "pointer-moved-fallback":
				s, base, baseBlobs := skipSetup(t, sid)
				prepared, err := prepareHarmonizedCandidate(sid, base.Generation, baseBlobs)
				if err != nil {
					t.Fatal(err)
				}
				stamps := skipStamps{indexerVersion: 1, indexedAtMs: 200, stats: base.Generation.Metadata.Stats, seedJSON: seedJSONForStats(base.Generation.Metadata.Stats), updatedAtMs: 200}
				// The compared generation is no longer active: the bookkeeping
				// must refuse so the caller falls back to the write path.
				if err := stampSkippedHarmonized(context.Background(), s, prepared, c.ComparedGeneration, stamps); err == nil || !strings.Contains(err.Error(), "moved between the skip comparison") {
					t.Fatalf("stale skip err = %v, want the pointer-moved refusal", err)
				}
				state, err := s.ReadIndexState(context.Background(), sid)
				if err != nil {
					t.Fatal(err)
				}
				if state.IndexedAt == nil || *state.IndexedAt != 100 {
					t.Fatalf("stale skip stamped indexed_at = %v, want the base stamps (100) unchanged", state.IndexedAt)
				}
			case "incomplete-new-never-skips":
				s, _, _ := skipSetup(t, sid)
				// An incomplete candidate over a complete active generation never
				// compares equal: completeness participates, so the refresh takes
				// the write path, where the last-good guard refuses it and the
				// complete generation stays authoritative.
				incomplete, incompleteBlobs := buildIncompleteGeneration(t, sid, "gen_skip_inc", c.Texts[0], c.Texts[1], c.Texts[2])
				_, err := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     filledCandidateForValidation(t, incomplete, incompleteBlobs),
					Blobs:          incompleteBlobs,
					ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete},
				})
				if err == nil {
					t.Fatal("incomplete_new over a complete generation succeeded; the last-good guard must refuse it")
				}
				if got := visibleGeneration(t, s, sid); got != "gen_skip_base" {
					t.Fatalf("visible = %q, want the complete last-good generation", got)
				}
			case "file-backed-never-skips":
				s, root := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				v2, blobs := buildTestGeneration(t, sid, "gen_skip_fb", c.Texts[0], c.Texts[1], c.Texts[2])
				seedFileBackedGeneration(t, s, root, sid, v2, blobs, false)
				// An identical refresh of a file-backed session converts it: the
				// skip comparison only runs over harmonized generations, and the
				// refresh carries a fresh identifier.
				refresh, refreshBlobs := buildTestGeneration(t, sid, "gen_skip_fb2", c.Texts[0], c.Texts[1], c.Texts[2])
				outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     filledCandidateForValidation(t, refresh, refreshBlobs),
					Blobs:          refreshBlobs,
					IndexerVersion: 1,
					IndexedAtMs:    200,
				})
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Disposition != c.disposition(t) {
					t.Fatalf("file-backed refresh disposition = %v, want conversion", outcome.Disposition)
				}
				if got := visibleGeneration(t, s, sid); got != "gen_skip_fb2" {
					t.Fatalf("visible = %q, want the converted harmonized generation", got)
				}
			case "stats-only-change-updates-in-place":
				s, _, _ := skipSetup(t, sid)
				beforeGens := countSessionGenerations(t, s, sid)
				refreshed, refreshedBlobs := buildTestGeneration(t, sid, "gen_skip_stats", "skip base text", "skip base input", "skip base output")
				refreshed.Generation.Metadata.Stats.TurnCount = c.TurnCount
				outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     filledCandidateForValidation(t, refreshed, refreshedBlobs),
					Blobs:          refreshedBlobs,
					IndexerVersion: 1,
					IndexedAtMs:    200,
				})
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Disposition != c.disposition(t) {
					t.Fatalf("stats-only refresh disposition = %v, want skipped", outcome.Disposition)
				}
				if got := countSessionGenerations(t, s, sid); got != beforeGens {
					t.Fatalf("stats-only refresh wrote generations %d, want %d", got, beforeGens)
				}
				stats, err := readStatsForTest(t, s, sid)
				if err != nil {
					t.Fatal(err)
				}
				if stats.TurnCount == nil || *stats.TurnCount != c.TurnCount {
					t.Fatalf("captured turn count = %v, want the refreshed %d", stats.TurnCount, c.TurnCount)
				}
			case "metadata-hash-stats-excluded":
				s, base, _ := skipSetup(t, sid)
				stored := readGenerationMetadataHash(t, s, sid, "gen_skip_base")
				if stored != statsExcludedMetadataHash(base.Generation.Metadata) {
					t.Fatal("stored metadata_hash is not the stats-excluded anchor")
				}
				// A stats-only refresh advances the bookkeeping but never
				// re-derives the anchor.
				refreshed, refreshedBlobs := buildTestGeneration(t, sid, "gen_skip_anchor", "skip base text", "skip base input", "skip base output")
				refreshed.Generation.Metadata.Stats.TurnCount = c.TurnCount
				outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     filledCandidateForValidation(t, refreshed, refreshedBlobs),
					Blobs:          refreshedBlobs,
					IndexerVersion: 1,
					IndexedAtMs:    300,
				})
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Disposition != c.disposition(t) {
					t.Fatalf("stats anchor refresh disposition = %v, want skipped", outcome.Disposition)
				}
				if got := readGenerationMetadataHash(t, s, sid, "gen_skip_base"); got != stored {
					t.Fatal("stats-only refresh re-derived the immutable capture anchor")
				}
			default:
				t.Fatalf("unknown generation skip operation %q", c.Operation)
			}
		})
	}
}

// countMappingBodies counts the distinct bodies one session's active
// generation maps: the skip writes none of its own.
func countMappingBodies(t *testing.T, s *Store, sid schema.SessionID) int {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	return countRowsOnConn(t, conn, `SELECT COUNT(DISTINCT body_digest) FROM session_generation_entries WHERE session_id = ?`, string(sid))
}

func countRowsOnConn(t *testing.T, conn *sqlite.Conn, query string, args ...any) int {
	t.Helper()
	count := 0
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

// readStatsForTest reads one session's measurements row.
func readStatsForTest(t *testing.T, s *Store, sid schema.SessionID) (CapturedStats, error) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		return CapturedStats{}, err
	}
	defer s.pool.Put(conn)
	return readCapturedStatsOnConn(conn, sid)
}

// readGenerationMetadataHash reads one generation's capture anchor.
func readGenerationMetadataHash(t *testing.T, s *Store, sid schema.SessionID, generationID string) string {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	hash := ""
	if err := sqlitex.ExecuteTransient(conn, `SELECT metadata_hash FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			hash = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return hash
}
