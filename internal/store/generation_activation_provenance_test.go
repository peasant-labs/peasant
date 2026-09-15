package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/generation_activation_provenance.yaml
var generationActivationProvenanceYAML []byte

//go:embed testdata/generation_activation_provenance.manifest.yaml
var generationActivationProvenanceManifestYAML []byte

// generationProvenanceCase seeds one capture state across a managed-generation
// activation. provenance is the kind recorded before the activation;
// corrupt_capture_hash breaks the captured metadata row's integrity digest so
// the case carries a capture whose snapshot cannot certify readiness.
type generationProvenanceCase struct {
	Name               string `yaml:"name"`
	Provenance         string `yaml:"provenance"`
	CorruptCaptureHash bool   `yaml:"corrupt_capture_hash"`
}

type generationProvenanceFixture struct {
	Session struct {
		ID          string `yaml:"id"`
		HostSlug    string `yaml:"host_slug"`
		ProjectHash string `yaml:"project_hash"`
		CWD         string `yaml:"cwd"`
		ContentHash string `yaml:"content_hash"`
	} `yaml:"session"`
	Generation struct {
		ID string `yaml:"id"`
	} `yaml:"generation"`
	Cases []generationProvenanceCase `yaml:"cases"`
}

func loadGenerationProvenanceFixture(t *testing.T) generationProvenanceFixture {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(string(generationActivationProvenanceYAML)))
	decoder.KnownFields(true)
	var fixture generationProvenanceFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_activation_provenance.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("generation_activation_provenance.yaml must contain exactly one document: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(generationActivationProvenanceManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_activation_provenance manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	seen := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.Name) == "" || seen[c.Name] {
			t.Fatalf("generation activation provenance case name %q is empty or repeated", c.Name)
		}
		if _, err := ingest.NewCWDProvenanceKind(c.Provenance); err != nil {
			t.Fatalf("case %q names an unknown provenance kind %q: %v", c.Name, c.Provenance, err)
		}
		seen[c.Name] = true
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "generation activation provenance"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// generationProvenanceCaptureRevision is the capture revision every seeding
// case records, so the activation's index write can bind to the same revision
// the capture row and the session counter already carry.
const generationProvenanceCaptureRevision = int64(7)

// runProvenanceSQL executes one statement with parameters on the store's pool.
func runProvenanceSQL(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("exec provenance SQL: %v", err)
	}
}

// readProvenanceScalar reads one text column the activation must preserve.
func readProvenanceScalar(t *testing.T, s *Store, query string) string {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	var value string
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			value = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read provenance scalar: %v", err)
	}
	return value
}

// seedProvenanceSession aligns the seeded session row with the captured
// metadata the case records: the snapshot compares the session's project and
// working directory, so both carry the fixture's values.
func seedProvenanceSession(t *testing.T, s *Store, fixture generationProvenanceFixture, sid schema.SessionID) {
	t.Helper()
	seedGenerationSession(t, s, string(sid))
	runProvenanceSQL(t, s, `INSERT OR IGNORE INTO projects(project_hash, canonical_cwd, canonical_remote) VALUES(?, ?, ?)`,
		fixture.Session.ProjectHash, fixture.Session.CWD, "")
	runProvenanceSQL(t, s, `UPDATE sessions SET project_hash = ?, session_cwd = ? WHERE session_id = ?`,
		fixture.Session.ProjectHash, fixture.Session.CWD, string(sid))
}

// seedProvenanceCapture writes the captured publication metadata row and the
// two revision columns the binding predicate compares against it, recording
// provenance as the captured metadata's source evidence. corrupt replaces the
// row's integrity digest with a foreign one so the captured snapshot can no
// longer certify readiness.
func seedProvenanceCapture(t *testing.T, s *Store, fixture generationProvenanceFixture, sid schema.SessionID, provenance string, corrupt bool) schema.UnifiedMetadata {
	t.Helper()
	meta := schema.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion,
		SessionID:     sid,
		ModelHarness:  defaults.HarnessClaudeCode,
		HostSlug:      schema.HostSlug(fixture.Session.HostSlug),
		Project:       schema.ProjectContext{Hash: schema.ProjectHash(fixture.Session.ProjectHash)},
		CWD:           fixture.Session.CWD,
		ContentHash:   fixture.Session.ContentHash,
		Stats:         schema.SessionStats{TurnCount: 3},
	}
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal captured metadata: %v", err)
	}
	runProvenanceSQL(t, s, `INSERT INTO session_publication_metadata(capture_revision, schema_version, metadata_json, metadata_hash, content_hash, captured_at, session_id) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		generationProvenanceCaptureRevision, int64(ingest.CurrentSchemaVersion), string(encoded), meta.MetadataHash, meta.ContentHash, int64(1700000000000), string(sid))
	if corrupt {
		runProvenanceSQL(t, s, `UPDATE session_publication_metadata SET metadata_hash = ? WHERE session_id = ?`, strings.Repeat("f", 64), string(sid))
	}
	runProvenanceSQL(t, s, `UPDATE sessions SET publication_capture_revision = ?, indexed_publication_capture_revision = ?, cwd_provenance_kind = ? WHERE session_id = ?`,
		generationProvenanceCaptureRevision, generationProvenanceCaptureRevision, provenance, string(sid))
	return meta
}

// TestGenerationActivationPreservesPublicationCapture proves that pointing a
// session at a managed generation preserves the recorded publication-capture
// agreement. The pointing UPDATE names facts the row-version trigger watches,
// so the activation must re-state the provenance it read instead of letting
// the trigger clear it: a cleared kind re-ingests the session on every
// discovery run and leaves it permanently unpublishable.
func TestGenerationActivationPreservesPublicationCapture(t *testing.T) {
	fixture := loadGenerationProvenanceFixture(t)
	sid := schema.SessionID(fixture.Session.ID)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			s, _ := openGenerationStore(t)
			seedProvenanceSession(t, s, fixture, sid)
			bound := tc.Provenance != string(ingest.CWDNotRecovered)
			if bound {
				seedProvenanceCapture(t, s, fixture, sid, tc.Provenance, tc.CorruptCaptureHash)
			} else {
				runProvenanceSQL(t, s, `UPDATE sessions SET cwd_provenance_kind = ? WHERE session_id = ?`, tc.Provenance, string(sid))
			}

			before, err := s.ReadIndexState(context.Background(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if before == nil {
				t.Fatal("seeded session has no readable index state")
			}

			complete, blobs := buildTestGeneration(t, sid, fixture.Generation.ID, "repaired full text", "repaired tool input", "repaired tool output")
			// The repaired generation derives its title from the same first
			// user entry whose full text it captures, so the activation's
			// derived title mirror replaces the stale one.
			complete.Generation.TitleRefs = []schema.SourceEntryRef{schema.SourceEntryRef(generationRefs[0])}
			var captureRevision int64
			if bound {
				captureRevision = generationProvenanceCaptureRevision
			}
			if err := s.ActivateNativeGeneration(context.Background(), ingest.NativeGenerationActivation{
				Generation:      complete,
				Blobs:           blobs,
				IndexerVersion:  77,
				IndexedAtMs:     777,
				CaptureRevision: captureRevision,
				ContentCapture: ingest.SessionContentCaptureWrite{
					Status:           ingest.ContentCaptureComplete,
					SourceAuthority:  ingest.ContentSourceProviderSource,
					TranscriptOrigin: ingest.TranscriptOriginFile,
					CaptureFormat:    ingest.ContentCaptureFormatFull,
					CapturedAtMs:     777,
				},
			}); err != nil {
				t.Fatalf("activate managed generation: %v", err)
			}

			if got := readProvenanceScalar(t, s, `SELECT cwd_provenance_kind FROM sessions WHERE session_id = '`+string(sid)+`'`); got != tc.Provenance {
				t.Fatalf("activation changed the recorded provenance kind %q -> %q; pointing a session at a generation is not a new capture", tc.Provenance, got)
			}
			after, err := s.ReadIndexState(context.Background(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if after.PublicationBound != bound {
				t.Fatalf("publication bound=%v, want %v", after.PublicationBound, bound)
			}
			if after.PublicationCaptureRevision != before.PublicationCaptureRevision {
				t.Fatalf("activation moved the capture revision %d -> %d; the recorded agreement must survive unchanged",
					before.PublicationCaptureRevision, after.PublicationCaptureRevision)
			}
			locations, err := s.BulkLookupSessionLocations(context.Background(), []ingest.SessionID{sid})
			if err != nil {
				t.Fatal(err)
			}
			readiness := locations[sid].PublicationReadiness
			// Ready requires a capture whose snapshot still certifies the
			// session: the absent case has no capture at all, and the
			// corrupted case carries a foreign integrity digest, so both must
			// keep asking for ingest even though their provenance kind (where
			// one exists) survived.
			wantReady := bound && !tc.CorruptCaptureHash
			if wantReady && readiness != ingest.PublicationReady {
				t.Fatalf("bound capture readiness = %q, want ready", readiness)
			}
			if !wantReady && readiness != ingest.PublicationNeedsIngest {
				t.Fatalf("case readiness = %q, want needs_ingest", readiness)
			}
			turnCount := readProvenanceScalar(t, s, `SELECT COALESCE(turn_count,-1) FROM session_metrics WHERE session_id = '`+string(sid)+`'`)
			if turnCount != "3" {
				t.Fatalf("activation turn_count mirror = %q, want the generation's 3", turnCount)
			}
			title := readProvenanceScalar(t, s, `SELECT COALESCE(title,'') FROM session_metrics WHERE session_id = '`+string(sid)+`'`)
			if title != "repaired full text" {
				t.Fatalf("activation title = %q, want the generation's derived title", title)
			}
		})
	}
}
