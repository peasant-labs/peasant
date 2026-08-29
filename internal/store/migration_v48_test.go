package store_test

import (
	"context"
	_ "embed"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/annotation-run-state/combined-lookup.yaml
var annotationRunCombinedLookupYAML []byte

func TestMigrationV48AnnotationRunStateTable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 48 {
		t.Errorf("user_version: expected >= 48, got %d", uv)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='annotation_run_state'`); got != 1 {
		t.Fatalf("annotation_run_state table count = %d, want 1", got)
	}
}

func TestStore_AnnotationRunStateRoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := ingest.SessionID("99999999-4444-4444-8444-999999999999")
	seedSession(t, s, string(sid))
	hash := strings.Repeat("a", 64)
	annotatedAt := time.UnixMilli(1700000000123)

	if err := s.SaveAnnotationRunState(ctx, ingest.AnnotationRunState{SessionID: sid, SessionEntriesHash: hash, ComputeVersion: 12, ClassifierVersion: 3, AnnotatedAt: annotatedAt}); err != nil {
		t.Fatalf("SaveAnnotationRunState: %v", err)
	}
	state, err := s.GetAnnotationRunState(ctx, sid)
	if err != nil {
		t.Fatalf("GetAnnotationRunState: %v", err)
	}
	if state == nil {
		t.Fatal("GetAnnotationRunState returned nil")
	}
	if state.SessionID != sid || state.SessionEntriesHash != hash || state.ComputeVersion != 12 || state.ClassifierVersion != 3 || !state.AnnotatedAt.Equal(annotatedAt) {
		t.Fatalf("state round trip mismatch: %+v", state)
	}
}

func TestStore_AnnotationRunStateCascadesWithSessionDelete(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := ingest.SessionID("99999999-5555-4555-8555-999999999999")
	seedSession(t, s, string(sid))
	if err := s.SaveAnnotationRunState(ctx, ingest.AnnotationRunState{SessionID: sid, SessionEntriesHash: strings.Repeat("b", 64), ComputeVersion: 1, ClassifierVersion: 1, AnnotatedAt: time.UnixMilli(1700000000000)}); err != nil {
		t.Fatalf("SaveAnnotationRunState: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_metrics WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
		t.Fatalf("delete session_metrics: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_run_state WHERE session_id = ?`, string(sid)); got != 0 {
		t.Fatalf("annotation_run_state rows after session delete = %d, want 0", got)
	}
}

func TestMigrationV48AnnotationRunStateRejectsInvalidHash(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sid := "99999999-6666-4666-8666-999999999999"
	seedSession(t, s, sid)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	for _, invalid := range []string{strings.Repeat("c", 63), strings.Repeat("A", 64)} {
		err := sqlitex.ExecuteTransient(conn, `INSERT INTO annotation_run_state (session_id, session_entries_hash, compute_version, classifier_version, annotated_at) VALUES (?, ?, 1, 1, 1700000000000)`, &sqlitex.ExecOptions{Args: []any{sid, invalid}})
		if err == nil {
			t.Fatalf("invalid annotation_run_state hash %q was accepted", invalid)
		}
	}
}

func TestStore_GetCurrentSessionEntriesHash(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := ingest.SessionID("99999999-7777-4777-8777-999999999999")
	seedSession(t, s, string(sid))
	hash := strings.Repeat("d", 64)

	conn := takeConn(t, s.PoolForTest())
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_entries_hash = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{hash, string(sid)}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("update session_entries_hash: %v", err)
	}
	s.PoolForTest().Put(conn)

	got, ok, err := s.GetCurrentSessionEntriesHash(ctx, sid)
	if err != nil {
		t.Fatalf("GetCurrentSessionEntriesHash: %v", err)
	}
	if !ok || got != hash {
		t.Fatalf("GetCurrentSessionEntriesHash = %q, %v; want %q, true", got, ok, hash)
	}
}

type annotationRunCombinedLookupFixture struct {
	Cases []annotationRunCombinedLookupCase `yaml:"cases"`
}

type annotationRunCombinedLookupCase struct {
	Name                   string `yaml:"name"`
	SessionID              string `yaml:"session_id"`
	CurrentHash            string `yaml:"current_hash"`
	HasMetricRow           bool   `yaml:"has_metric_row"`
	HasComputeVersion      bool   `yaml:"has_compute_version"`
	MetricComputeVersion   int    `yaml:"metric_compute_version"`
	WantHasComputeVersion  bool   `yaml:"want_has_compute_version"`
	StateHash              string `yaml:"state_hash"`
	StateComputeVersion    int    `yaml:"state_compute_version"`
	StateClassifierVersion int    `yaml:"state_classifier_version"`
	HasState               bool   `yaml:"has_state"`
}

func TestStore_GetAnnotationRunInputsCombinedLookup(t *testing.T) {
	t.Parallel()
	var fixture annotationRunCombinedLookupFixture
	if err := yaml.Unmarshal(annotationRunCombinedLookupYAML, &fixture); err != nil {
		t.Fatalf("unmarshal annotation run combined lookup fixture: %v", err)
	}
	assertRequiredAnnotationRunCombinedLookupCases(t, fixture.Cases)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			ctx := context.Background()
			sid := ingest.SessionID(tc.SessionID)
			seedSession(t, s, string(sid))

			conn := takeConn(t, s.PoolForTest())
			if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_entries_hash = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{tc.CurrentHash, string(sid)}}); err != nil {
				s.PoolForTest().Put(conn)
				t.Fatalf("update session_entries_hash: %v", err)
			}
			if !tc.HasMetricRow {
				if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_metrics WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
					s.PoolForTest().Put(conn)
					t.Fatalf("delete session_metrics: %v", err)
				}
			}
			s.PoolForTest().Put(conn)

			if tc.HasMetricRow {
				var computeVersion *int
				if tc.HasComputeVersion {
					computeVersion = intPtr(tc.MetricComputeVersion)
				}
				if err := s.SaveMetrics(ctx, &ingest.SessionMetrics{SessionID: sid, QualityMetrics: schema.QualityMetrics{ComputeVersion: computeVersion}}); err != nil {
					t.Fatalf("SaveMetrics: %v", err)
				}
			}
			if tc.HasState {
				if err := s.SaveAnnotationRunState(ctx, ingest.AnnotationRunState{
					SessionID:          sid,
					SessionEntriesHash: tc.StateHash,
					ComputeVersion:     tc.StateComputeVersion,
					ClassifierVersion:  tc.StateClassifierVersion,
					AnnotatedAt:        time.UnixMilli(1700000000000),
				}); err != nil {
					t.Fatalf("SaveAnnotationRunState: %v", err)
				}
			}

			inputs, err := s.GetAnnotationRunInputs(ctx, sid)
			if err != nil {
				t.Fatalf("GetAnnotationRunInputs: %v", err)
			}
			if inputs == nil {
				t.Fatal("GetAnnotationRunInputs returned nil")
			}
			if inputs.SessionID != sid || !inputs.HasSessionEntriesHash || inputs.SessionEntriesHash != tc.CurrentHash {
				t.Fatalf("session hash input mismatch: %+v", inputs)
			}
			if inputs.HasComputeVersion != tc.WantHasComputeVersion {
				t.Fatalf("compute version input mismatch: %+v", inputs)
			}
			if tc.WantHasComputeVersion && inputs.ComputeVersion != tc.MetricComputeVersion {
				t.Fatalf("compute version = %d, want %d", inputs.ComputeVersion, tc.MetricComputeVersion)
			}
			if tc.HasState {
				if inputs.State == nil {
					t.Fatal("state missing from combined lookup")
				}
				if inputs.State.SessionEntriesHash != tc.StateHash || inputs.State.ComputeVersion != tc.StateComputeVersion || inputs.State.ClassifierVersion != tc.StateClassifierVersion {
					t.Fatalf("state input mismatch: %+v", inputs.State)
				}
			} else if inputs.State != nil {
				t.Fatalf("state = %+v, want nil", inputs.State)
			}
		})
	}
}

func assertRequiredAnnotationRunCombinedLookupCases(t *testing.T, cases []annotationRunCombinedLookupCase) {
	t.Helper()
	requiredNames := map[string]bool{
		"current state":        false,
		"stale hash":           false,
		"stale metric version": false,
		"missing state":        false,
		"missing metrics":      false,
		"nil compute version":  false,
	}
	for _, tc := range cases {
		if _, ok := requiredNames[tc.Name]; ok {
			requiredNames[tc.Name] = true
		}
	}
	for name, seen := range requiredNames {
		if !seen {
			t.Fatalf("required annotation run combined lookup fixture case %q is missing", name)
		}
	}
}
