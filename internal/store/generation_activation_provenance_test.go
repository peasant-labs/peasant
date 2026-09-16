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
	"github.com/peasant-labs/peasant/internal/indexformat"
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
// activation_capture is the kind the activation itself certifies, or empty for
// an activation that supplies no capture; activation_foreign_project makes the
// certified capture name a project the stored session does not carry. The
// want_* fields are the stored agreement a discovery run compares afterwards.
type generationProvenanceCase struct {
	Name                     string `yaml:"name"`
	Provenance               string `yaml:"provenance"`
	CorruptCaptureHash       bool   `yaml:"corrupt_capture_hash"`
	ActivationCapture        string `yaml:"activation_capture"`
	ActivationForeignProject bool   `yaml:"activation_foreign_project"`
	WantReadiness            string `yaml:"want_readiness"`
	WantCaptureRevision      int64  `yaml:"want_capture_revision"`
	WantBound                bool   `yaml:"want_bound"`
	WantRefused              bool   `yaml:"want_refused"`
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
		if c.ActivationCapture != "" {
			if _, err := ingest.NewCWDProvenanceKind(c.ActivationCapture); err != nil {
				t.Fatalf("case %q names an unknown activation capture kind %q: %v", c.Name, c.ActivationCapture, err)
			}
		}
		if c.WantReadiness != string(ingest.PublicationReady) && c.WantReadiness != string(ingest.PublicationNeedsIngest) {
			t.Fatalf("case %q names an unknown readiness %q", c.Name, c.WantReadiness)
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

// provenanceCaptureMetadata builds the captured metadata snapshot the fixture
// records. A source_exact capture names the fixture's working directory; every
// other kind records no working directory, which is the agreement the
// capture-validation rule requires.
func provenanceCaptureMetadata(fixture generationProvenanceFixture, sid schema.SessionID, kind ingest.CWDProvenanceKind) schema.UnifiedMetadata {
	meta := schema.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion,
		SessionID:     sid,
		ModelHarness:  defaults.HarnessClaudeCode,
		HostSlug:      schema.HostSlug(fixture.Session.HostSlug),
		Project:       schema.ProjectContext{Hash: schema.ProjectHash(fixture.Session.ProjectHash)},
		ContentHash:   fixture.Session.ContentHash,
		Stats:         schema.SessionStats{TurnCount: 3},
	}
	if kind == ingest.CWDSourceExact {
		meta.CWD = fixture.Session.CWD
	}
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	return meta
}

// seedProvenanceCapture writes the captured publication metadata row and the
// two revision columns the binding predicate compares against it, recording
// provenance as the captured metadata's source evidence. corrupt replaces the
// row's integrity digest with a foreign one so the captured snapshot can no
// longer certify readiness.
func seedProvenanceCapture(t *testing.T, s *Store, fixture generationProvenanceFixture, sid schema.SessionID, provenance string, corrupt bool) schema.UnifiedMetadata {
	t.Helper()
	kind, err := ingest.NewCWDProvenanceKind(provenance)
	if err != nil {
		t.Fatalf("seed capture provenance %q: %v", provenance, err)
	}
	meta := provenanceCaptureMetadata(fixture, sid, kind)
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
// agreement and records a certified one that is missing. The pointing UPDATE
// names facts the row-version trigger watches, so the activation must re-state
// the provenance it read instead of letting the trigger clear it: a cleared
// kind re-ingests the session on every discovery run and leaves it permanently
// unpublishable. The activation also records the capture it certifies in the
// SAME transaction as the generation install, so a repaired session is
// publishable without a further run; an uncertified kind records nothing and
// leaves the stored provenance exactly as it was.
func TestGenerationActivationPreservesPublicationCapture(t *testing.T) {
	fixture := loadGenerationProvenanceFixture(t)
	sid := schema.SessionID(fixture.Session.ID)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			s, _ := openGenerationStore(t)
			seedProvenanceSession(t, s, fixture, sid)
			if tc.Provenance != string(ingest.CWDNotRecovered) {
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
			activation := ingest.NativeGenerationActivation{
				Generation:      complete,
				Blobs:           blobs,
				IndexerVersion:  77,
				IndexedAtMs:     777,
				CaptureRevision: before.PublicationCaptureRevision,
				ContentCapture: ingest.SessionContentCaptureWrite{
					Status:           ingest.ContentCaptureComplete,
					SourceAuthority:  ingest.ContentSourceProviderSource,
					TranscriptOrigin: ingest.TranscriptOriginFile,
					CaptureFormat:    ingest.ContentCaptureFormatFull,
					CapturedAtMs:     777,
				},
			}
			if tc.ActivationCapture != "" {
				kind, kindErr := ingest.NewCWDProvenanceKind(tc.ActivationCapture)
				if kindErr != nil {
					t.Fatalf("activation capture kind %q: %v", tc.ActivationCapture, kindErr)
				}
				meta := provenanceCaptureMetadata(fixture, sid, kind)
				if tc.ActivationForeignProject {
					meta.Project.Hash = schema.ProjectHash(strings.Repeat("e", 64))
					meta.MetadataHash = schema.ComputeMetadataHash(&meta)
				}
				activation.Capture = &ingest.PublicationCaptureWrite{Metadata: meta, CWDProvenance: kind}
			}

			activateErr := s.ActivateNativeGeneration(context.Background(), activation)
			if tc.WantRefused {
				if activateErr == nil {
					t.Fatal("activation with a publication capture that disagrees with the stored session was accepted; want refusal")
				}
			} else if activateErr != nil {
				t.Fatalf("activate managed generation: %v", activateErr)
			}

			// A capture the activation certified replaces the stored provenance
			// with the certified kind; every other activation must leave the
			// stored provenance exactly as it was.
			wantProvenance := tc.Provenance
			if tc.ActivationCapture != "" && !tc.WantRefused && tc.ActivationCapture != string(ingest.CWDNotRecovered) {
				wantProvenance = tc.ActivationCapture
			}
			if got := readProvenanceScalar(t, s, `SELECT cwd_provenance_kind FROM sessions WHERE session_id = '`+string(sid)+`'`); got != wantProvenance {
				t.Fatalf("activation changed the recorded provenance kind %s -> %q, want %q", tc.Provenance, got, wantProvenance)
			}
			after, err := s.ReadIndexState(context.Background(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if after == nil {
				t.Fatal("activated session has no readable index state")
			}
			if after.PublicationBound != tc.WantBound {
				t.Fatalf("publication bound=%v, want %v", after.PublicationBound, tc.WantBound)
			}
			if after.PublicationCaptureRevision != tc.WantCaptureRevision {
				t.Fatalf("capture revision = %d, want %d", after.PublicationCaptureRevision, tc.WantCaptureRevision)
			}
			locations, err := s.BulkLookupSessionLocations(context.Background(), []ingest.SessionID{sid})
			if err != nil {
				t.Fatal(err)
			}
			readiness := locations[sid].PublicationReadiness
			if readiness != ingest.PublicationReadiness(tc.WantReadiness) {
				t.Fatalf("readiness = %q, want %q", readiness, tc.WantReadiness)
			}
			if tc.WantRefused {
				// A refused activation installs nothing: the caller must not be
				// able to read a half-written generation or capture.
				var generationID string
				snapshotErr := s.WithSessionSnapshot(context.Background(), sid, func(snapshot indexformat.ReadSnapshot) error {
					generationID = snapshot.GenerationID
					return nil
				})
				if snapshotErr != nil {
					t.Fatal(snapshotErr)
				}
				if generationID != "" {
					t.Fatalf("refused activation installed generation %q, want none", generationID)
				}
				if before.PublicationCaptureRevision != after.PublicationCaptureRevision {
					t.Fatalf("refused activation moved the capture revision %d -> %d", before.PublicationCaptureRevision, after.PublicationCaptureRevision)
				}
				return
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

// TestGenerationActivationRecoveryRecordsPublicationCapture proves a crash
// between staging and the commit replays the certified capture: the durable
// intent carries it, and recovery records it with the same guarded
// transaction, so a repaired session is publishable after recovery too.
func TestGenerationActivationRecoveryRecordsPublicationCapture(t *testing.T) {
	fixture := loadGenerationProvenanceFixture(t)
	sid := schema.SessionID(fixture.Session.ID)
	s, _ := openGenerationStore(t)
	seedProvenanceSession(t, s, fixture, sid)
	runProvenanceSQL(t, s, `UPDATE sessions SET cwd_provenance_kind = ? WHERE session_id = ?`, string(ingest.CWDNotRecovered), string(sid))

	complete, blobs := buildTestGeneration(t, sid, fixture.Generation.ID, "recovered full text", "recovered tool input", "recovered tool output")
	complete.Generation.TitleRefs = []schema.SourceEntryRef{schema.SourceEntryRef(generationRefs[0])}
	kind := ingest.CWDSourceExact
	activation := ingest.NativeGenerationActivation{
		Generation:     complete,
		Blobs:          blobs,
		IndexerVersion: 77,
		IndexedAtMs:    777,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureComplete,
			SourceAuthority:  ingest.ContentSourceProviderSource,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatFull,
			CapturedAtMs:     777,
		},
		Capture: &ingest.PublicationCaptureWrite{Metadata: provenanceCaptureMetadata(fixture, sid, kind), CWDProvenance: kind},
	}

	installRecoveryFault(t, s, "after-rename-before-db")
	if err := s.ActivateNativeGeneration(context.Background(), activation); err == nil {
		t.Fatal("activation across the staging seam succeeded; the crash must interrupt it")
	}
	clearRecoveryFault(t, s, "after-rename-before-db")
	if err := s.RecoverGenerationActivation(context.Background(), sid); err != nil {
		t.Fatalf("recover interrupted activation: %v", err)
	}

	state, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("recovered session has no readable index state")
	}
	if state.PublicationCaptureRevision == 0 || !state.PublicationBound {
		t.Fatalf("recovery did not record the certified capture: revision=%d bound=%v", state.PublicationCaptureRevision, state.PublicationBound)
	}
	locations, err := s.BulkLookupSessionLocations(context.Background(), []ingest.SessionID{sid})
	if err != nil {
		t.Fatal(err)
	}
	if got := locations[sid].PublicationReadiness; got != ingest.PublicationReady {
		t.Fatalf("recovered readiness = %q, want ready", got)
	}
}
