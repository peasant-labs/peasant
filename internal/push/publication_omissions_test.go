package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/publication_omissions.yaml
var publicationOmissionsYAML []byte

//go:embed testdata/publication_omissions.manifest.yaml
var publicationOmissionsManifestYAML []byte

type publicationOmissionsCase struct {
	Name                string `yaml:"name"`
	Status              string `yaml:"status"`
	FailureCode         string `yaml:"failureCode"`
	FailureMessage      string `yaml:"failureMessage,omitempty"`
	CaptureFormat       string `yaml:"captureFormat"`
	WriterCertifies     bool   `yaml:"writerCertifies"`
	Publishable         bool   `yaml:"publishable"`
	Readiness           string `yaml:"readiness"`
	FullContentReadable bool   `yaml:"fullContentReadable"`
}

type publicationOmissionsFixture struct {
	Cases []publicationOmissionsCase `yaml:"cases"`
}

func decodePublicationOmissionsFixture(raw []byte) (publicationOmissionsFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var fixture publicationOmissionsFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, errors.New("publication omissions fixture must contain exactly one YAML document")
	}
	return fixture, nil
}

func loadPublicationOmissionsFixture(t *testing.T) publicationOmissionsFixture {
	t.Helper()
	fixture, err := decodePublicationOmissionsFixture(publicationOmissionsYAML)
	if err != nil {
		t.Fatalf("decode publication omissions fixture: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(publicationOmissionsManifestYAML, "publication omissions")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		if _, err := ingest.NewContentCaptureStatus(fixtureCase.Status); err != nil {
			t.Fatalf("publication omissions case %q: %v", fixtureCase.Name, err)
		}
		if _, err := ingest.NewContentCaptureFailureCode(fixtureCase.FailureCode); err != nil {
			t.Fatalf("publication omissions case %q: %v", fixtureCase.Name, err)
		}
		if _, err := ingest.NewContentCaptureFormat(fixtureCase.CaptureFormat); err != nil {
			t.Fatalf("publication omissions case %q: %v", fixtureCase.Name, err)
		}
		if _, err := publicationOmissionsReadiness(fixtureCase.Readiness); err != nil {
			t.Fatalf("publication omissions case %q: %v", fixtureCase.Name, err)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "publication omissions"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func publicationOmissionsReadiness(name string) (ingest.PublicationReadiness, error) {
	switch name {
	case "ready":
		return ingest.PublicationReady, nil
	case "needs_ingest":
		return ingest.PublicationNeedsIngest, nil
	}
	return "", errors.New("publication omissions fixture names readiness " + name + "; use ready or needs_ingest")
}

func TestPublicationOmissionsFixtureGuards(t *testing.T) {
	loadPublicationOmissionsFixture(t)
	manifest, err := testutil.DecodeSemanticManifest(publicationOmissionsManifestYAML, "publication omissions")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(publicationOmissionsYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		fixture, err := decodePublicationOmissionsFixture(mutated)
		if err != nil {
			t.Fatalf("decode renamed fixture: %v", err)
		}
		names := make([]string, len(fixture.Cases))
		for index, fixtureCase := range fixture.Cases {
			names[index] = fixtureCase.Name
		}
		if err := testutil.ValidateSemanticNames(manifest, names, "publication omissions"); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}

// TestPublicationOmissions drives ONE table through the whole publication
// decision: the store's typed predicate, the readiness the readiness SQL
// computes, the complete-content reader export and publication both use, and the
// push preflight. They must agree case by case, which is what stops the SQL and
// the Go rule from drifting apart.
func TestPublicationOmissions(t *testing.T) {
	fixture := loadPublicationOmissionsFixture(t)
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "peasant.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			metaValue := ingest.NewUnifiedMetadata()
			meta := &metaValue
			meta.SessionID = testutil.TestSessionUUID
			meta.HostSlug = testutil.TestHostSlug
			meta.ModelHarness = defaults.HarnessClaudeCode
			meta.Model = testutil.TestModel
			meta.CWD = "/home/example/dev/widgets"
			meta.Source = ingest.SourceInfo{FilePath: "/nonexistent/source.jsonl", Format: ingest.SourceFormatJSONL}
			meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "synthetic-project"}
			ingested := int64(1700000120000)
			meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested}
			meta.Stats = ingest.StatsInfo{TurnCount: 1, DurationMs: 60000}

			recorded := "the session said this"
			omitted := ""
			entries := []schema.SessionEntry{
				{SessionID: meta.SessionID, EntryIndex: 1, Harness: meta.ModelHarness, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &recorded},
				// The placeholder that stands in an omitted record's place: it is
				// an entry like any other, so a publishable capture carries it.
				{SessionID: meta.SessionID, EntryIndex: 2, Harness: meta.ModelHarness, Role: schema.RoleTool, EntryType: schema.EntryTypeToolResult, ContentPreview: &omitted, RawByteLength: intPtrPublicationOmissions(300 << 20)},
			}
			revision := seedPublicationOmissionsSession(t, db, meta, entries)

			// The case's capture state, offered to the FULL-content writer.
			write := ingest.SessionContentCaptureWrite{
				Status:          ingest.ContentCaptureStatus(fixtureCase.Status),
				FailureCode:     ingest.ContentCaptureFailureCode(fixtureCase.FailureCode),
				FailureMessage:  fixtureCase.FailureMessage,
				CaptureFormat:   ingest.ContentCaptureFormat(fixtureCase.CaptureFormat),
				SourceAuthority: ingest.ContentSourceNewIngest,
				CapturedAtMs:    meta.Timestamp.Start,
			}
			// A certified complete capture first, so every case starts from the
			// same stored entries, counts and full-capture proof and differs only
			// in the capture state under test.
			indexPublicationOmissions(t, db, meta, entries, revision, ingest.SessionContentCaptureWrite{
				Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
				CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: meta.Timestamp.Start,
			})
			certified := indexPublicationOmissions(t, db, meta, entries, revision, write) == nil
			if certified != fixtureCase.WriterCertifies {
				t.Fatalf("the full-content writer certified=%v, want %v", certified, fixtureCase.WriterCertifies)
			}
			if !certified {
				// The state the writer refuses can still exist on disk: an older
				// build, or the preview path, left it there. Put the row in that
				// state directly, so the READ side is judged on the same state.
				forcePublicationOmissionsCaptureState(t, path, meta.SessionID, fixtureCase)
			}

			capture, found, err := db.GetSessionContentCapture(ctx, meta.SessionID)
			if err != nil || !found {
				t.Fatalf("read back the capture: found=%v err=%v", found, err)
			}
			if got := store.PublishableWithOmissions(capture); got != fixtureCase.Publishable {
				t.Errorf("store.PublishableWithOmissions=%v, want %v for status %q code %q format %q", got, fixtureCase.Publishable, capture.Status, capture.FailureCode, capture.CaptureFormat)
			}

			wantReadiness, err := publicationOmissionsReadiness(fixtureCase.Readiness)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := db.LoadPublicationInput(ctx, meta.SessionID)
			if err != nil {
				t.Fatalf("load publication input: %v", err)
			}
			if bundle.Readiness != wantReadiness {
				t.Errorf("readiness SQL says %q, want %q — the SQL and the Go predicate disagree", bundle.Readiness, wantReadiness)
			}
			preflightErr := push.ValidatePublicationInput(bundle)
			if (preflightErr == nil) != fixtureCase.Publishable {
				t.Errorf("push preflight err=%v, want publishable=%v", preflightErr, fixtureCase.Publishable)
			}
			// A refusal has to say which one incompleteness is still allowed,
			// otherwise a user whose session was omitted-records cannot tell
			// this refusal from the one their session is exempt from.
			if preflightErr != nil && !strings.Contains(preflightErr.Error(), "oversized source records that ingest omitted") {
				t.Errorf("the refusal does not name the one allowed incompleteness: %v", preflightErr)
			}

			snapshot, readErr := db.ReadSessionContent(ctx, meta.SessionID.String())
			if fixtureCase.FullContentReadable {
				if readErr != nil {
					t.Fatalf("the complete-content reader refused a publishable capture: %v", readErr)
				}
				if snapshot == nil || len(snapshot.Entries) != len(entries) {
					t.Fatalf("the reader returned %v entries, want %d including the omitted-record placeholder", snapshot, len(entries))
				}
				return
			}
			if readErr == nil {
				t.Fatalf("the complete-content reader served a capture that may not be published")
			}
			if !errors.Is(readErr, store.ErrContentCaptureIncomplete) {
				t.Fatalf("the refusal is not the incomplete-capture refusal: %v", readErr)
			}
		})
	}
}

// TestPublicationOmissionsCarryPartialDiagnostics proves the published metadata
// says the capture is partial and names the omitted record, so the receiving UI
// can show the note. The contract already carries diagnostics.partial, so no
// schema change is involved: this pins the mapping that was already there.
func TestPublicationOmissionsCarryPartialDiagnostics(t *testing.T) {
	t.Parallel()
	metaValue := ingest.NewUnifiedMetadata()
	meta := &metaValue
	meta.SessionID = testutil.TestSessionUUID
	meta.HostSlug = testutil.TestHostSlug
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = testutil.TestModel
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "synthetic-project"}
	meta.Source = ingest.SourceInfo{FilePath: "/nonexistent/source.jsonl", Format: ingest.SourceFormatJSONL}
	meta.ContentHash = schema.ComputeTranscriptHash([]byte("captured transcript"))
	meta.MetadataHash = schema.ComputeMetadataHash(meta)
	partial := true
	meta.Diagnostics.Partial = &partial
	meta.Diagnostics.Warnings = []ingest.DiagnosticEntry{{
		ErrorType:   "record_too_large",
		Location:    "line 5",
		Message:     "the record at line 5 is larger than the per-record limit and was omitted",
		Remediation: "Reduce the record at the source and re-ingest, or accept the stored placeholder.",
	}}

	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("map the publication metadata: %v", err)
	}
	var request schema.PublishRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatalf("decode the published metadata: %v", err)
	}
	if err := schema.ValidatePublishRequest(payload); err != nil {
		t.Fatalf("the published metadata does not validate against the contract: %v", err)
	}
	if request.Diagnostics.Partial == nil || !*request.Diagnostics.Partial {
		t.Fatalf("the published metadata does not declare the capture partial: %v", request.Diagnostics.Partial)
	}
	if len(request.Diagnostics.Warnings) != 1 {
		t.Fatalf("the published metadata carries %d warnings, want the omission warning", len(request.Diagnostics.Warnings))
	}
	warning := request.Diagnostics.Warnings[0]
	if warning.ErrorType != "record_too_large" || warning.Location != "line 5" {
		t.Fatalf("the published warning is %+v, want the record_too_large omission at line 5", warning)
	}
	if !strings.Contains(warning.Message, "omitted") {
		t.Errorf("the published warning does not say the record was omitted: %q", warning.Message)
	}
}

// indexPublicationOmissions runs the real full-content write and returns what it
// said, so the fixture can assert which capture states the writer certifies.
func indexPublicationOmissions(t *testing.T, db *store.Store, meta *ingest.UnifiedMetadata, entries []schema.SessionEntry, revision int64, capture ingest.SessionContentCaptureWrite) error {
	t.Helper()
	versions := testutil.HarvesterVersionsForSeed(t, meta.ModelHarness)
	results := db.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{
		SessionID: meta.SessionID, Result: indexformat.V1{Entries: entries},
		IndexVersion: versions.IndexVersion, IndexerVersion: versions.IndexerVersion,
		RequireFullContent: true, CaptureRevision: revision, IndexedAtMs: meta.Timestamp.Start,
		ContentCapture: capture,
	}})
	if len(results) != 1 {
		t.Fatalf("one write, %d results", len(results))
	}
	return results[0].Err
}

func seedPublicationOmissionsSession(t *testing.T, db *store.Store, meta *schema.UnifiedMetadata, entries []schema.SessionEntry) int64 {
	t.Helper()
	content, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	meta.ContentHash = schema.ComputeTranscriptHash(content)
	meta.MetadataHash = schema.ComputeMetadataHash(meta)
	revisions, err := db.InsertSessionsWithRevisions(context.Background(), []ingest.StoreEntry{{
		Metadata: meta, PublicationCapture: true, CWDProvenance: ingest.CWDSourceExact,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return revisions[meta.SessionID]
}

// forcePublicationOmissionsCaptureState writes a capture state the writer refuses
// to certify, exactly as an older build or the preview path could have left it.
// The stored entries, counts and full-capture proof are untouched, so the read
// side is judged on the state columns alone.
func forcePublicationOmissionsCaptureState(t *testing.T, path string, id ingest.SessionID, fixtureCase publicationOmissionsCase) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	err = sqlitex.ExecuteTransient(conn,
		`UPDATE session_content_captures SET status=?,failure_code=?,failure_message=?,capture_format=? WHERE session_id=?`,
		&sqlitex.ExecOptions{Args: []any{fixtureCase.Status, fixtureCase.FailureCode, fixtureCase.FailureMessage, fixtureCase.CaptureFormat, string(id)}})
	if err != nil {
		t.Fatalf("force the stored capture state: %v", err)
	}
}

func intPtrPublicationOmissions(v int) *int { return &v }
