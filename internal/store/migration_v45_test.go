package store_test

// V45 fixture-driven round trip and rejection tests. See
// migration_v45_check_test.go (package store) for the CHECK-bijection parser
// and its baseline — this file cannot live in that package, because it needs
// testutil.RequireFixtureNames and testutil imports store for its mocks,
// which would be a real import cycle from inside package store's own test
// binary.

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v45_session_origin.yaml
var v45SessionOriginFixtureBytes []byte

type v45SessionOriginFixture struct {
	RequiredSessionRecordNames     []string            `yaml:"required_session_record_names"`
	RequiredSessionRejectionNames  []string            `yaml:"required_session_rejection_names"`
	RequiredEvidenceRecordNames    []string            `yaml:"required_evidence_record_names"`
	RequiredEvidenceRejectionNames []string            `yaml:"required_evidence_rejection_names"`
	SessionRecords                 []v45SessionRecord  `yaml:"session_records"`
	SessionRejections              []v45NamedSQL       `yaml:"session_rejections"`
	EvidenceRecords                []v45EvidenceRecord `yaml:"evidence_records"`
	EvidenceRejections             []v45NamedSQL       `yaml:"evidence_rejections"`
}

type v45SessionRecord struct {
	Name          string `yaml:"name"`
	SessionID     string `yaml:"session_id"`
	SessionOrigin string `yaml:"session_origin"`
}

type v45EvidenceRecord struct {
	Name            string                     `yaml:"name"`
	SourcePath      string                     `yaml:"source_path"`
	Scope           ingest.ClaudeEvidenceScope `yaml:"scope"`
	ModTimeUnixNano int64                      `yaml:"mod_time_unix_nano"`
	SizeBytes       int64                      `yaml:"size_bytes"`
	HasConversation bool                       `yaml:"has_conversation"`
	Origin          string                     `yaml:"origin"`
}

func (r v45EvidenceRecord) toEvidence() ingest.ClaudeTranscriptEvidence {
	return ingest.ClaudeTranscriptEvidence{
		SourcePath:            ingest.ResolvedPath(r.SourcePath),
		Scope:                 r.Scope,
		ModTimeUnixNano:       r.ModTimeUnixNano,
		SizeBytes:             r.SizeBytes,
		HasConversationRecord: r.HasConversation,
		Origin:                sessionorigin.Origin(r.Origin),
	}
}

type v45NamedSQL struct {
	Name string `yaml:"name"`
	SQL  string `yaml:"sql"`
}

func decodeV45Fixture(source []byte) (v45SessionOriginFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	var fixture v45SessionOriginFixture
	if err := decoder.Decode(&fixture); err != nil {
		return v45SessionOriginFixture{}, fmt.Errorf("decode v45 session-origin fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return v45SessionOriginFixture{}, fmt.Errorf("v45 session-origin fixture must contain exactly one YAML document: %v", err)
	}

	sessionRecordNames := make(map[string]bool, len(fixture.SessionRecords))
	for _, r := range fixture.SessionRecords {
		sessionRecordNames[r.Name] = true
	}
	sessionRejectionNames := make(map[string]bool, len(fixture.SessionRejections))
	for _, r := range fixture.SessionRejections {
		sessionRejectionNames[r.Name] = true
	}
	evidenceRecordNames := make(map[string]bool, len(fixture.EvidenceRecords))
	for _, r := range fixture.EvidenceRecords {
		evidenceRecordNames[r.Name] = true
	}
	evidenceRejectionNames := make(map[string]bool, len(fixture.EvidenceRejections))
	for _, r := range fixture.EvidenceRejections {
		evidenceRejectionNames[r.Name] = true
	}

	if err := testutil.RequireFixtureNames("v45 session-origin fixture", "session record", fixture.RequiredSessionRecordNames, sessionRecordNames); err != nil {
		return v45SessionOriginFixture{}, err
	}
	if err := testutil.RequireFixtureNames("v45 session-origin fixture", "session rejection", fixture.RequiredSessionRejectionNames, sessionRejectionNames); err != nil {
		return v45SessionOriginFixture{}, err
	}
	if err := testutil.RequireFixtureNames("v45 session-origin fixture", "evidence record", fixture.RequiredEvidenceRecordNames, evidenceRecordNames); err != nil {
		return v45SessionOriginFixture{}, err
	}
	if err := testutil.RequireFixtureNames("v45 session-origin fixture", "evidence rejection", fixture.RequiredEvidenceRejectionNames, evidenceRejectionNames); err != nil {
		return v45SessionOriginFixture{}, err
	}
	return fixture, nil
}

func loadV45Fixture(t *testing.T) v45SessionOriginFixture {
	t.Helper()
	fixture, err := decodeV45Fixture(v45SessionOriginFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestMigrationV45SessionOriginRoundTripAndRejections inserts one row per
// sessions.session_origin menu value directly against a real, fully migrated
// Store, reads it back, and confirms the stored value survives
// sessionorigin.Parse — then confirms the CHECK refuses a value outside the
// menu, and separately refuses the empty string (which IS legal on the
// evidence cache table but is NOT a fourth sessions origin).
func TestMigrationV45SessionOriginRoundTripAndRejections(t *testing.T) {
	t.Parallel()
	fixture := loadV45Fixture(t)
	s := openTestStore(t)
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// The real Store enforces the sessions FKs to host_slugs/projects, unlike
	// the bare in-memory connection migration_v45_check_test.go uses for the
	// CHECK-bijection tests. Seed the one host and project row every fixture
	// record references.
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO host_slugs (opaque_id, host_slug) VALUES ('h', 'h')`, nil); err != nil {
		t.Fatalf("seed host_slugs: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO projects (project_hash) VALUES ('p')`, nil); err != nil {
		t.Fatalf("seed projects: %v", err)
	}

	const insertSession = `INSERT INTO sessions
		(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, session_origin)
		VALUES (?, 'claude-code', 'm', 'h', 'p', 1, 2, 3, '/x.jsonl', 'jsonl', ?)`

	for _, record := range fixture.SessionRecords {
		if err := sqlitex.ExecuteTransient(conn, insertSession, &sqlitex.ExecOptions{
			Args: []any{record.SessionID, record.SessionOrigin},
		}); err != nil {
			t.Fatalf("case %q: insert session: %v", record.Name, err)
		}

		var got string
		if err := sqlitex.ExecuteTransient(conn, `SELECT session_origin FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{record.SessionID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				got = stmt.ColumnText(0)
				return nil
			},
		}); err != nil {
			t.Fatalf("case %q: select session: %v", record.Name, err)
		}
		if got != record.SessionOrigin {
			t.Errorf("case %q: session_origin round-tripped to %q, want %q", record.Name, got, record.SessionOrigin)
		}
		if _, err := sessionorigin.Parse(got); err != nil {
			t.Errorf("case %q: stored sessions.session_origin %q does not survive sessionorigin.Parse: %v", record.Name, got, err)
		}
	}

	for _, rejection := range fixture.SessionRejections {
		if err := sqlitex.ExecuteTransient(conn, rejection.SQL, nil); err == nil {
			t.Errorf("case %q: expected the sessions.session_origin CHECK to refuse this insert, it succeeded", rejection.Name)
		}
	}
}

// TestMigrationV45EvidenceOriginRoundTripAndRejections saves and loads every
// evidence fixture record through the REAL ClaudeEvidenceCache (store.Store),
// not an in-memory fake, so this is the SQL half of the round trip — the
// adapter half is proven separately in
// internal/ingest/claude_origin_test.go, whose own comment says its in-memory
// fake cache does not prove the stored form. Both scopes (root and subagent)
// are exercised, and the empty "predates the field" marker is proven to
// survive the round trip distinctly from a real menu value.
func TestMigrationV45EvidenceOriginRoundTripAndRejections(t *testing.T) {
	t.Parallel()
	fixture := loadV45Fixture(t)
	ctx := context.Background()
	s := openTestStore(t)

	upserts := make([]ingest.ClaudeTranscriptEvidence, 0, len(fixture.EvidenceRecords))
	for _, record := range fixture.EvidenceRecords {
		upserts = append(upserts, record.toEvidence())
	}
	if err := s.SaveClaudeEvidence(ctx, upserts, nil); err != nil {
		t.Fatalf("SaveClaudeEvidence: %v", err)
	}
	records, err := s.LoadClaudeEvidence(ctx)
	if err != nil {
		t.Fatalf("LoadClaudeEvidence: %v", err)
	}

	scopesSeen := make(map[ingest.ClaudeEvidenceScope]bool, 2)
	for i, want := range upserts {
		name := fixture.EvidenceRecords[i].Name
		got, ok := records[want.SourcePath]
		if !ok {
			t.Fatalf("case %q: transcript %q missing from the cache after a real save", name, want.SourcePath)
		}
		if got.Origin != want.Origin {
			t.Errorf("case %q: cached origin = %q, want %q", name, got.Origin, want.Origin)
		}
		scopesSeen[got.Scope] = true
	}
	for _, scope := range []ingest.ClaudeEvidenceScope{ingest.ClaudeEvidenceScopeRoot, ingest.ClaudeEvidenceScopeSubagent} {
		if !scopesSeen[scope] {
			t.Fatalf("no fixture record exercised scope %q; the round trip is unproven for that scope", scope)
		}
	}

	// The empty marker round-trips as a real value, distinct from every menu
	// value, and it fails Validate — it is the absence of an origin, not one.
	for _, record := range fixture.EvidenceRecords {
		if record.Origin != "" {
			continue
		}
		got, ok := records[ingest.ResolvedPath(record.SourcePath)]
		if !ok {
			t.Fatalf("case %q: transcript %q missing from the cache", record.Name, record.SourcePath)
		}
		if got.Origin != "" {
			t.Fatalf("case %q: expected the empty marker to survive the round trip, got %q", record.Name, got.Origin)
		}
		if err := got.Origin.Validate(); err == nil {
			t.Errorf("case %q: a record carrying the empty marker validated as a real origin, want a refusal", record.Name)
		}
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	for _, rejection := range fixture.EvidenceRejections {
		if err := sqlitex.ExecuteTransient(conn, rejection.SQL, nil); err == nil {
			t.Errorf("case %q: expected the claude_transcript_evidence.origin CHECK to refuse this insert, it succeeded", rejection.Name)
		}
	}
}

// TestV45FixtureGuardsRequiredRowDeletion mutation-proves the required-name
// manifests for all four rows this fixture carries: deleting any one of them
// must fail the load with a message naming the missing row.
func TestV45FixtureGuardsRequiredRowDeletion(t *testing.T) {
	t.Parallel()

	if _, err := decodeV45Fixture(v45SessionOriginFixtureBytes); err != nil {
		t.Fatalf("baseline fixture failed to decode before mutation: %v", err)
	}

	cases := []struct {
		name    string
		marker  []byte
		wantErr string
	}{
		{
			name:    "deleted required session record",
			marker:  []byte("  - name: an unknown-declared session round-trips\n    session_id: 33333333-3333-4333-8333-333333333333\n    session_origin: unknown\n"),
			wantErr: `missing required session record "an unknown-declared session round-trips"`,
		},
		{
			name: "deleted required session rejection",
			marker: []byte("  - name: the empty string is not a legal sessions.session_origin\n    sql: >-\n" +
				"      INSERT INTO sessions\n" +
				"      (session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, session_origin)\n" +
				"      VALUES ('bad-2', 'claude-code', 'm', 'h', 'p', 1, 2, 3, '/x.jsonl', 'jsonl', '')\n"),
			wantErr: `missing required session rejection "the empty string is not a legal sessions.session_origin"`,
		},
		{
			name:    "deleted required evidence record",
			marker:  []byte("  - name: a record mined before the origin field existed keeps the empty marker\n    source_path: /home/example/.claude/projects/-workspace/66666666-6666-4666-8666-666666666666.jsonl\n    scope: root\n    mod_time_unix_nano: 1750000000000000013\n    size_bytes: 300\n    has_conversation: true\n    origin: \"\"\n"),
			wantErr: `missing required evidence record "a record mined before the origin field existed keeps the empty marker"`,
		},
		{
			name: "deleted required evidence rejection",
			marker: []byte("  - name: an evidence origin outside the closed menu is refused\n    sql: >-\n" +
				"      INSERT INTO claude_transcript_evidence\n" +
				"      (source_path, scope, mod_time_unix_nano, size_bytes, has_conversation, spawns_json, title, branch, cwd, origin)\n" +
				"      VALUES ('/tmp/v45-bad.jsonl', 'root', 1, 1, 1, '[]', '', '', '', 'teammate')\n"),
			wantErr: `missing required evidence rejection "an evidence origin outside the closed menu is refused"`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mutated := bytes.Replace(v45SessionOriginFixtureBytes, tc.marker, nil, 1)
			if bytes.Equal(mutated, v45SessionOriginFixtureBytes) {
				t.Fatalf("mutation marker did not match the fixture; the test is out of sync with the fixture file")
			}
			_, err := decodeV45Fixture(mutated)
			if err == nil {
				t.Fatal("fixture decoder accepted a corpus missing a required row")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
