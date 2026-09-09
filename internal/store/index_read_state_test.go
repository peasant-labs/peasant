package store_test

import (
	"bytes"
	_ "embed"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_read_state.yaml
var indexReadStateYAML []byte

// indexReadCapture names how the case seeds the stored content capture.
type indexReadCapture string

const (
	indexReadCaptureNone    indexReadCapture = "none"
	indexReadCapturePreview indexReadCapture = "preview"
	indexReadCaptureFull    indexReadCapture = "full"
)

type indexReadStateCase struct {
	Name                string                      `yaml:"name"`
	Capture             indexReadCapture            `yaml:"capture"`
	PublicationRevision int64                       `yaml:"publicationRevision"`
	SessionRevision     int64                       `yaml:"sessionRevision"`
	IndexedRevision     int64                       `yaml:"indexedRevision"`
	Provenance          ingest.CWDProvenanceKind    `yaml:"provenance"`
	StatusOverride      ingest.ContentCaptureStatus `yaml:"statusOverride"`
	WantBound           bool                        `yaml:"wantBound"`
	WantStatus          ingest.ContentCaptureStatus `yaml:"wantStatus"`
	WantFormat          ingest.ContentCaptureFormat `yaml:"wantFormat"`
}

type indexReadStateDocument struct {
	MetadataHash  string               `yaml:"metadataHash"`
	ContentHash   string               `yaml:"contentHash"`
	RequiredNames []string             `yaml:"requiredNames"`
	Cases         []indexReadStateCase `yaml:"cases"`
}

func loadIndexReadStateFixtures(t *testing.T) indexReadStateDocument {
	t.Helper()
	var document indexReadStateDocument
	decoder := yaml.NewDecoder(bytes.NewReader(indexReadStateYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("index read state fixtures require one document: %v", err)
	}
	names := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid index read state fixture %q", row.Name)
		}
		switch row.Capture {
		case indexReadCaptureNone, indexReadCapturePreview, indexReadCaptureFull:
		default:
			t.Fatalf("unknown capture seed %q in case %q", row.Capture, row.Name)
		}
		if _, err := ingest.NewContentCaptureStatus(string(row.WantStatus)); err != nil {
			t.Fatalf("case %q expects an unknown capture status: %v", row.Name, err)
		}
		if (row.Capture == indexReadCaptureNone) != (row.WantFormat == "") {
			t.Fatalf("case %q must state the stored capture format exactly when it seeds a capture row", row.Name)
		}
		if row.WantFormat != "" {
			if _, err := ingest.NewContentCaptureFormat(string(row.WantFormat)); err != nil {
				t.Fatalf("case %q expects an unknown capture format: %v", row.Name, err)
			}
		}
		if _, err := ingest.NewCWDProvenanceKind(string(row.Provenance)); err != nil {
			t.Fatalf("case %q names an unknown provenance kind %q: %v", row.Name, row.Provenance, err)
		}
		if row.StatusOverride != "" {
			if _, err := ingest.NewContentCaptureStatus(string(row.StatusOverride)); err != nil {
				t.Fatalf("case %q overrides the capture status with an unknown value: %v", row.Name, err)
			}
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index read state", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document
}

// seedPublicationBinding writes the captured publication metadata and the two
// session revision columns the binding predicate compares against it. A zero
// revision leaves the session without any captured metadata row.
func seedPublicationBinding(t *testing.T, db *store.Store, sid schema.SessionID, row indexReadStateCase, document indexReadStateDocument) {
	t.Helper()
	if row.PublicationRevision > 0 {
		inputSQL(t, db, sid,
			`INSERT INTO session_publication_metadata
(capture_revision,schema_version,metadata_json,metadata_hash,content_hash,captured_at,session_id)
VALUES (?,?,?,?,?,?,?)`,
			row.PublicationRevision, int64(ingest.CurrentSchemaVersion), `{"seed":true}`,
			document.MetadataHash, document.ContentHash, int64(1700000000000))
	}
	inputSQL(t, db, sid,
		`UPDATE sessions SET publication_capture_revision=?, indexed_publication_capture_revision=?,
cwd_provenance_kind=? WHERE session_id=?`,
		row.SessionRevision, row.IndexedRevision, string(row.Provenance))
}

// TestIndexReadStateReportsPublicationBindingAndContentStatus proves that one
// captured snapshot reports whether the stored index write is bound to the
// current publication metadata, and what content the session actually holds.
func TestIndexReadStateReportsPublicationBindingAndContentStatus(t *testing.T) {
	document := loadIndexReadStateFixtures(t)
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			sid := schema.SessionID(testutil.TestSessionUUID)
			seedSession(t, db, string(sid))
			entries := contentEntries(ingest.SessionID(sid), contentCaseNamed(t, loadContentFixtures(t), "long_unicode"))
			switch row.Capture {
			case indexReadCapturePreview:
				if err := db.IndexSessionEntries(t.Context(), ingest.SessionID(sid), entries); err != nil {
					t.Fatal(err)
				}
			case indexReadCaptureFull:
				writeFull(t, db, ingest.SessionID(sid), entries, ingest.SessionEntryWriteReplaceAll)
			}
			if row.StatusOverride != "" {
				// The capture writers cannot certify a failed capture, so the
				// stored status is set directly. The read side must report
				// whatever the row holds, not only the statuses it can write.
				inputSQL(t, db, sid, "UPDATE session_content_captures SET status = ? WHERE session_id = ?", string(row.StatusOverride))
			}
			seedPublicationBinding(t, db, sid, row, document)

			state := readInputState(t, db, sid)
			if state == nil {
				t.Fatal("seeded session has no readable index state")
			}
			if state.PublicationBound != row.WantBound {
				t.Fatalf("publication bound=%v, want %v", state.PublicationBound, row.WantBound)
			}
			if state.ContentStatus != row.WantStatus {
				t.Fatalf("content status=%q, want %q", state.ContentStatus, row.WantStatus)
			}
			stored, found, err := db.GetSessionContentCapture(t.Context(), ingest.SessionID(sid))
			if err != nil {
				t.Fatalf("stored capture unreadable: %v", err)
			}
			if found != (row.Capture != indexReadCaptureNone) {
				t.Fatalf("stored capture row present=%v, but the case seeds capture %q", found, row.Capture)
			}
			// No caller in this test supplies a capture format, so this pins
			// the WRITER's own default for each kind of write. Swapping those
			// defaults would file a bounded preview under a format that claims
			// whole-session content, and every read that trusts the format
			// would then be reading a promise the row cannot keep.
			if found && stored.CaptureFormat != row.WantFormat {
				t.Fatalf("stored capture format=%q, want %q", stored.CaptureFormat, row.WantFormat)
			}
		})
	}
}
