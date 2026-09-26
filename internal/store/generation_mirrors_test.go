package store

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type mirrorsFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Parent struct {
		ID string `yaml:"id"`
	} `yaml:"parent"`
	Generation struct {
		FirstID  string `yaml:"first_id"`
		SecondID string `yaml:"second_id"`
	} `yaml:"generation"`
	Cases []struct {
		Name             string `yaml:"name"`
		FirstTitle       string `yaml:"first_title"`
		SecondTitle      string `yaml:"second_title"`
		FirstInputCount  *int64 `yaml:"first_input_count"`
		SecondInputCount *int64 `yaml:"second_input_count"`
		FirstAdapter     *int   `yaml:"first_adapter"`
		SecondAdapter    *int   `yaml:"second_adapter"`
		Relationship     string `yaml:"relationship"`
		InputCount       string `yaml:"input_count"`
	} `yaml:"cases"`
}

func loadMirrorsFixture(t *testing.T) mirrorsFixture {
	t.Helper()
	var fixture mirrorsFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(generationMirrorsYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_mirrors.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(generationMirrorsManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_mirrors manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "generation mirrors"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func buildMirrorGeneration(t *testing.T, sid schema.SessionID, genID, text string, inputCount *int64, adapter *int, startMs, endMs int64, toolCalls int, relationships []schema.SessionRelationship, titleRef schema.SourceEntryRef) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	v2, blobs := buildTestGeneration(t, sid, genID, text, "input "+genID, "output "+genID)
	v2.Generation.Metadata.Stats.InputSubmissionCount = inputCount
	v2.Generation.Metadata.AdapterVersion = adapter
	v2.Generation.Metadata.Timestamp.Start = startMs
	v2.Generation.Metadata.Timestamp.End = endMs
	v2.Generation.Metadata.Stats.ToolCallCount = toolCalls
	v2.Generation.Metadata.Relationships = relationships
	if titleRef != "" {
		v2.Generation.TitleRefs = []schema.SourceEntryRef{titleRef}
	}
	return v2, blobs
}

// TestGenerationMirrors proves activation installs title, input-count, adapter,
// timestamp, logical-parent and tool-count mirrors atomically, derives the
// snapshot parent from relationship evidence, and changes fields between
// generations without a prior metadata upsert. Every case is driven from the
// typed fixture, and an unknown case fails rather than doing nothing, so
// deleting a runner branch cannot leave the manifest guard green.
func TestGenerationMirrors(t *testing.T) {
	fixture := loadMirrorsFixture(t)
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("generation mirrors fixture carries no cases")
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s, _ := openGenerationStore(t)
			seedGenerationSession(t, s, fixture.Session.ID)

			switch tc.Name {
			case "mirrors-change-between-generations":
				if tc.FirstInputCount == nil || tc.SecondInputCount == nil || tc.FirstAdapter == nil || tc.SecondAdapter == nil {
					t.Fatalf("case %q requires typed first/second counts and adapters", tc.Name)
				}
				g1, g1Blobs := buildMirrorGeneration(t, id, fixture.Generation.FirstID, tc.FirstTitle, tc.FirstInputCount, tc.FirstAdapter, 1000, 2000, 2, nil, schema.SourceEntryRef(generationRefs[0]))
				if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: g1, Blobs: g1Blobs, IndexerVersion: 1, IndexedAtMs: 100}); err != nil {
					t.Fatalf("activate G1: %v", err)
				}
				assertMirrorRow(t, s, id, 1000, 2000, nil, tc.FirstInputCount, tc.FirstAdapter, 3, 2, tc.FirstTitle)
				assertSnapshotMirrors(t, s, id, fixture.Generation.FirstID, 1000, 2000, nil, tc.FirstInputCount, 3, 2)

				g2, g2Blobs := buildMirrorGeneration(t, id, fixture.Generation.SecondID, tc.SecondTitle, tc.SecondInputCount, tc.SecondAdapter, 3000, 4000, 5, nil, schema.SourceEntryRef(generationRefs[0]))
				// Turn count stays 3 (three entries) so the test isolates the
				// mirrors that previously stayed stale.
				if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: g2, Blobs: g2Blobs, IndexerVersion: 2, IndexedAtMs: 200}); err != nil {
					t.Fatalf("activate G2: %v", err)
				}
				assertMirrorRow(t, s, id, 3000, 4000, nil, tc.SecondInputCount, tc.SecondAdapter, 3, 5, tc.SecondTitle)
				assertSnapshotMirrors(t, s, id, fixture.Generation.SecondID, 3000, 4000, nil, tc.SecondInputCount, 3, 5)

			case "missing-parent-preserved":
				// A complete generation has measured its submissions, so the
				// complete missing-parent control carries a measured count.
				zero := int64(0)
				missing, err := schema.NewSessionID(fixture.Parent.ID)
				if err != nil {
					t.Fatal(err)
				}
				relationships := []schema.SessionRelationship{{
					Kind:          schema.SessionRelationshipStartedBy,
					TargetState:   schema.RelationshipTargetKnown,
					TargetLocalID: &missing,
					Evidence:      schema.EvidenceNativeTyped,
				}}
				g1, g1Blobs := buildMirrorGeneration(t, id, fixture.Generation.FirstID, "child text", &zero, nil, 1000, 2000, 0, relationships, schema.SourceEntryRef(generationRefs[0]))
				if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: g1, Blobs: g1Blobs}); err != nil {
					t.Fatalf("activate child with missing parent: %v", err)
				}
				// The availability cache stays NULL so the admitted child
				// survives, while the snapshot still names the durable logical
				// parent.
				conn, err := s.pool.Take(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				parentNull := false
				_ = sqlitex.ExecuteTransient(conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
					Args: []any{string(id)},
					ResultFunc: func(stmt *sqlite.Stmt) error {
						parentNull = stmt.ColumnType(0) == sqlite.TypeNull
						return nil
					},
				})
				s.pool.Put(conn)
				if !parentNull {
					t.Fatal("sessions.parent_id names a missing target; it must stay NULL")
				}
				err = s.WithSessionSnapshot(context.Background(), id, func(snapshot indexformat.ReadSnapshot) error {
					if snapshot.Session.ParentSessionID == nil || *snapshot.Session.ParentSessionID != missing {
						return stringsErrorf("snapshot parent = %v, want missing target %s", snapshot.Session.ParentSessionID, missing)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}

			case "absent-input-count-preserved":
				// An unknown count is a first-discovery incomplete candidate:
				// completeness incomplete_new, no count scalar, no success
				// stamps. The count must survive as absent, not measured zero.
				g1, g1Blobs := buildMirrorGeneration(t, id, fixture.Generation.FirstID, "text absent", nil, nil, 1000, 2000, 0, nil, schema.SourceEntryRef(generationRefs[0]))
				g1 = markMirrorIncomplete(t, g1, g1Blobs)
				if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: g1, Blobs: g1Blobs, IndexerVersion: 77, IndexedAtMs: 777}); err != nil {
					t.Fatalf("activate absent count: %v", err)
				}
				stamps := readIndexStateForTest(t, s, id)
				if stamps.IndexerVersion != 0 || stamps.IndexedAt != nil {
					t.Fatalf("incomplete absent-count generation carried success stamps: %+v", stamps)
				}
				assertInputCountMirror(t, s, id, nil)

			case "zero-input-count-measured":
				zero := int64(0)
				g1, g1Blobs := buildMirrorGeneration(t, id, fixture.Generation.FirstID, "text zero", &zero, nil, 1000, 2000, 0, nil, schema.SourceEntryRef(generationRefs[0]))
				if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: g1, Blobs: g1Blobs}); err != nil {
					t.Fatalf("activate zero count: %v", err)
				}
				assertInputCountMirror(t, s, id, &zero)

			default:
				t.Fatalf("unknown generation mirror scenario %q; add it to the fixture, the required-names manifest and this runner", tc.Name)
			}
		})
	}
}

// markMirrorIncomplete turns a mirror candidate into a valid first-discovery
// incomplete generation: completeness incomplete_new with no count scalar.
func markMirrorIncomplete(t *testing.T, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) indexformat.V2 {
	t.Helper()
	v2.Generation.Completeness = indexformat.GenerationCompletenessIncompleteNew
	v2.Generation.Metadata.Stats.InputSubmissionCount = nil
	if err := filledCandidateForValidation(t, v2, blobs).Validate(); err != nil {
		t.Fatalf("incomplete mirror candidate is not otherwise valid: %v", err)
	}
	return v2
}

func assertMirrorRow(t *testing.T, s *Store, sid schema.SessionID, wantStart, wantEnd int64, wantParent *string, wantInput *int64, wantAdapter *int, wantTurns, wantTools int, wantTitle string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var gotStart, gotEnd int64
	var gotParent *string
	var gotInput *int64
	var gotAdapter *int
	var gotTurns, gotTools int
	var gotTitle *string
	if err := sqlitex.ExecuteTransient(conn, `SELECT s.start_ms, s.end_ms, s.parent_id, s.input_submission_count, s.adapter_version, COALESCE(m.turn_count, -1), COALESCE(m.tool_calls, -1), m.title FROM sessions s LEFT JOIN session_metrics m ON m.session_id = s.session_id WHERE s.session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			gotStart = stmt.ColumnInt64(0)
			gotEnd = stmt.ColumnInt64(1)
			if stmt.ColumnType(2) != sqlite.TypeNull {
				v := stmt.ColumnText(2)
				gotParent = &v
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				v := stmt.ColumnInt64(3)
				gotInput = &v
			}
			if stmt.ColumnType(4) != sqlite.TypeNull {
				v := stmt.ColumnInt(4)
				gotAdapter = &v
			}
			gotTurns = stmt.ColumnInt(5)
			gotTools = stmt.ColumnInt(6)
			if stmt.ColumnType(7) != sqlite.TypeNull {
				v := stmt.ColumnText(7)
				gotTitle = &v
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read mirrors: %v", err)
	}
	if gotStart != wantStart || gotEnd != wantEnd {
		t.Fatalf("timestamps = (%d,%d), want (%d,%d)", gotStart, gotEnd, wantStart, wantEnd)
	}
	if !equalOptionalStringPtr(gotParent, wantParent) {
		t.Fatalf("parent = %v, want %v", gotParent, wantParent)
	}
	if !equalOptionalInt64Ptr(gotInput, wantInput) {
		t.Fatalf("input count = %v, want %v", gotInput, wantInput)
	}
	if !equalOptionalIntPtr(gotAdapter, wantAdapter) {
		t.Fatalf("adapter = %v, want %v", gotAdapter, wantAdapter)
	}
	if gotTurns != wantTurns || gotTools != wantTools {
		t.Fatalf("counts = (%d,%d), want (%d,%d)", gotTurns, gotTools, wantTurns, wantTools)
	}
	if wantTitle != "" && (gotTitle == nil || *gotTitle != wantTitle) {
		t.Fatalf("title = %v, want %q", gotTitle, wantTitle)
	}
}

func assertSnapshotMirrors(t *testing.T, s *Store, sid schema.SessionID, wantGen string, wantStart, wantEnd int64, wantParent *string, wantInput *int64, wantTurns, wantTools int) {
	t.Helper()
	err := s.WithSessionSnapshot(context.Background(), sid, func(snapshot indexformat.ReadSnapshot) error {
		if snapshot.GenerationID != wantGen {
			return stringsErrorf("generation = %q, want %q", snapshot.GenerationID, wantGen)
		}
		if snapshot.Session.StartTime.UnixMilli() != wantStart || snapshot.Session.EndTime.UnixMilli() != wantEnd {
			return stringsErrorf("snapshot timestamps = (%d,%d), want (%d,%d)", snapshot.Session.StartTime.UnixMilli(), snapshot.Session.EndTime.UnixMilli(), wantStart, wantEnd)
		}
		if !equalOptionalSessionIDPtr(snapshot.Session.ParentSessionID, wantParent) {
			return stringsErrorf("snapshot parent = %v, want %v", snapshot.Session.ParentSessionID, wantParent)
		}
		if !equalOptionalInt64Ptr(snapshot.Session.InputSubmissionCount, wantInput) {
			return stringsErrorf("snapshot input count = %v, want %v", snapshot.Session.InputSubmissionCount, wantInput)
		}
		if snapshot.Session.TurnCount != wantTurns || snapshot.Session.ToolCallCount != wantTools {
			return stringsErrorf("snapshot counts = (%d,%d), want (%d,%d)", snapshot.Session.TurnCount, snapshot.Session.ToolCallCount, wantTurns, wantTools)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertInputCountMirror(t *testing.T, s *Store, sid schema.SessionID, want *int64) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var got *int64
	_ = sqlitex.ExecuteTransient(conn, `SELECT input_submission_count FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				v := stmt.ColumnInt64(0)
				got = &v
			}
			return nil
		},
	})
	_ = sqlitex.ExecuteTransient(conn, `SELECT input_submission_count FROM session_projection_generations WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			var row *int64
			if stmt.ColumnType(0) != sqlite.TypeNull {
				v := stmt.ColumnInt64(0)
				row = &v
			}
			if !equalOptionalInt64Ptr(row, want) {
				t.Fatalf("generation input count = %v, want %v", row, want)
			}
			return nil
		},
	})
	if !equalOptionalInt64Ptr(got, want) {
		t.Fatalf("sessions input count = %v, want %v (absent versus measured zero must be preserved)", got, want)
	}
	err = s.WithSessionSnapshot(context.Background(), sid, func(snapshot indexformat.ReadSnapshot) error {
		if !equalOptionalInt64Ptr(snapshot.Session.InputSubmissionCount, want) {
			return stringsErrorf("snapshot input count = %v, want %v", snapshot.Session.InputSubmissionCount, want)
		}
		if !equalOptionalInt64Ptr(snapshot.Metadata.Stats.InputSubmissionCount, want) {
			return stringsErrorf("metadata input count = %v, want %v", snapshot.Metadata.Stats.InputSubmissionCount, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func equalOptionalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalOptionalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalOptionalIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalOptionalSessionIDPtr(a *schema.SessionID, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return string(*a) == *b
}

type stringsError string

func stringsErrorf(format string, args ...any) error {
	return stringsError(strings.TrimSpace(sprintf(format, args...)))
}

func (e stringsError) Error() string { return string(e) }

func sprintf(format string, args ...any) string {
	// Minimal formatting without importing fmt in the helper path.
	argStrings := make([]string, 0, len(args))
	for _, arg := range args {
		argStrings = append(argStrings, stringify(arg))
	}
	out := format
	for _, s := range argStrings {
		out = strings.Replace(out, "%v", s, 1)
		out = strings.Replace(out, "%q", `"`+s+`"`, 1)
		out = strings.Replace(out, "%d", s, 1)
		out = strings.Replace(out, "%s", s, 1)
	}
	return out
}

func stringify(v any) string {
	switch value := v.(type) {
	case nil:
		return "<nil>"
	case string:
		return value
	case *string:
		if value == nil {
			return "<nil>"
		}
		return *value
	case *int64:
		if value == nil {
			return "<nil>"
		}
		return int64String(*value)
	case *int:
		if value == nil {
			return "<nil>"
		}
		return intString(*value)
	case int:
		return intString(value)
	case int64:
		return int64String(value)
	default:
		return "..."
	}
}

func intString(v int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	digits := ""
	if v == 0 {
		digits = "0"
	}
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	if neg {
		return "-" + digits
	}
	return digits
}

func int64String(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	digits := ""
	if v == 0 {
		digits = "0"
	}
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	if neg {
		return "-" + digits
	}
	return digits
}
