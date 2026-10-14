package store

import (
	"context"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/stored_capture_authority.yaml
var storedCaptureAuthorityYAML []byte

//go:embed testdata/stored_capture_authority.manifest.yaml
var storedCaptureAuthorityManifestYAML []byte

// storedCaptureAuthorityCase seeds one stored content-capture certificate and
// states the authority the bridge must report back. present false means no row
// is seeded at all.
type storedCaptureAuthorityCase struct {
	Name                       string `yaml:"name"`
	Present                    bool   `yaml:"present"`
	Status                     string `yaml:"status"`
	SourceAuthority            string `yaml:"sourceAuthority"`
	TranscriptOrigin           string `yaml:"transcriptOrigin"`
	CaptureFormat              string `yaml:"captureFormat"`
	FailureCode                string `yaml:"failureCode"`
	PublicationCaptureRevision int64  `yaml:"publicationCaptureRevision"`
	WantAuthority              bool   `yaml:"wantAuthority"`
}

type storedCaptureAuthorityFixture struct {
	Session struct {
		ID string `yaml:"id"`
	} `yaml:"session"`
	Cases []storedCaptureAuthorityCase `yaml:"cases"`
}

// transcriptOriginFor maps a fixture name to the closed-set origin it seeds and
// the bridge must preserve.
func transcriptOriginFor(t *testing.T, name string) ingest.TranscriptOrigin {
	t.Helper()
	switch name {
	case "file":
		return ingest.TranscriptOriginFile
	case "opencode-legacy-sqlite":
		return ingest.TranscriptOriginOpenCodeLegacySQLite
	case "opencode-current-sqlite":
		return ingest.TranscriptOriginOpenCodeCurrentSQLite
	default:
		t.Fatalf("unknown transcript origin %q; use file, opencode-legacy-sqlite or opencode-current-sqlite", name)
		return ingest.TranscriptOriginFile
	}
}

func loadStoredCaptureAuthorityFixture(t *testing.T) storedCaptureAuthorityFixture {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(string(storedCaptureAuthorityYAML)))
	decoder.KnownFields(true)
	var fixture storedCaptureAuthorityFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode stored_capture_authority.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("stored_capture_authority.yaml must contain exactly one document: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(storedCaptureAuthorityManifestYAML)
	if err != nil {
		t.Fatalf("decode stored capture authority manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	seen := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.Name) == "" || seen[c.Name] {
			t.Fatalf("stored capture authority case name %q is empty or repeated", c.Name)
		}
		seen[c.Name] = true
		if c.Present {
			if _, err := ingest.NewContentCaptureStatus(c.Status); err != nil {
				t.Fatalf("case %q names an unknown status %q: %v", c.Name, c.Status, err)
			}
			if _, err := ingest.NewContentSourceAuthority(c.SourceAuthority); err != nil {
				t.Fatalf("case %q names an unknown source authority %q: %v", c.Name, c.SourceAuthority, err)
			}
			transcriptOriginFor(t, c.TranscriptOrigin)
			if _, err := ingest.NewContentCaptureFormat(c.CaptureFormat); err != nil {
				t.Fatalf("case %q names an unknown capture format %q: %v", c.Name, c.CaptureFormat, err)
			}
			if _, err := ingest.NewContentCaptureFailureCode(c.FailureCode); err != nil {
				t.Fatalf("case %q names an unknown failure code %q: %v", c.Name, c.FailureCode, err)
			}
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "stored capture authority"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// seedStoredCaptureAuthority writes the certificate row the case names. The
// complete state carries its required integrity digest and no failure, exactly
// as the table's CHECK requires.
func seedStoredCaptureAuthority(t *testing.T, s *Store, sid schema.SessionID, tc storedCaptureAuthorityCase) {
	t.Helper()
	var failureCode any
	if tc.FailureCode != "" {
		failureCode = tc.FailureCode
	}
	runProvenanceSQL(t, s, `INSERT INTO session_content_captures
(session_id,status,source_authority,transcript_origin,capture_format,entry_count,content_row_count,full_capture_sha256,captured_at_ms,failure_code,failure_message,publication_capture_revision)
VALUES(?,?,?,?,?,?,?,?,?,?,NULL,?)`,
		string(sid), tc.Status, tc.SourceAuthority, int64(transcriptOriginFor(t, tc.TranscriptOrigin)), tc.CaptureFormat, 1, 0,
		strings.Repeat("a", 64), int64(1700000000000), failureCode, tc.PublicationCaptureRevision)
}

// TestReadStoredCaptureAuthority pins the OpenCode V1 authority bridge to the
// content writer's own publishable-capture predicate: the stored certificate
// holds last-good full authority exactly when the writer's guard would refuse a
// preview replacement over it. A publication revision is never proof, and the
// certificate's own status, source authority, transcript origin, capture
// format, and failure code are preserved so an accounted incomplete/full
// certificate is never flattened into a complete one.
func TestReadStoredCaptureAuthority(t *testing.T) {
	fixture := loadStoredCaptureAuthorityFixture(t)
	sid := schema.SessionID(fixture.Session.ID)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			s, _ := openGenerationStore(t)
			seedGenerationSession(t, s, string(sid))
			if tc.Present {
				seedStoredCaptureAuthority(t, s, sid, tc)
			}
			authority, err := s.ReadStoredCaptureAuthority(context.Background(), sid)
			if err != nil {
				t.Fatalf("ReadStoredCaptureAuthority: %v", err)
			}
			if got := authority != nil; got != tc.WantAuthority {
				t.Fatalf("authority present = %v, want %v (authority=%+v)", got, tc.WantAuthority, authority)
			}
			if tc.WantAuthority {
				assertStoredAuthorityPreserved(t, tc, authority)
			}
			assertStoredCaptureAuthorityMatchesWriterPredicate(t, s, sid, tc.Present)
		})
	}
}

// assertStoredAuthorityPreserved proves the bridge reports the certificate's
// own state verbatim, so a complete certificate and an accounted
// incomplete/full certificate are distinguishable.
func assertStoredAuthorityPreserved(t *testing.T, tc storedCaptureAuthorityCase, authority *ingest.StoredCaptureAuthority) {
	t.Helper()
	if authority.Status != ingest.ContentCaptureStatus(tc.Status) {
		t.Fatalf("authority status = %q, want %q", authority.Status, tc.Status)
	}
	if authority.SourceAuthority != ingest.ContentSourceAuthority(tc.SourceAuthority) {
		t.Fatalf("authority source = %q, want %q", authority.SourceAuthority, tc.SourceAuthority)
	}
	if authority.TranscriptOrigin != transcriptOriginFor(t, tc.TranscriptOrigin) {
		t.Fatalf("authority origin = %s, want %s", authority.TranscriptOrigin, tc.TranscriptOrigin)
	}
	if authority.CaptureFormat != ingest.ContentCaptureFormat(tc.CaptureFormat) {
		t.Fatalf("authority format = %q, want %q", authority.CaptureFormat, tc.CaptureFormat)
	}
	if authority.FailureCode != ingest.ContentCaptureFailureCode(tc.FailureCode) {
		t.Fatalf("authority failure code = %q, want %q", authority.FailureCode, tc.FailureCode)
	}
}

// assertStoredCaptureAuthorityMatchesWriterPredicate proves the bridge reads the
// writer's one predicate rather than a second copy: the method reports authority
// exactly when PublishableWithOmissions protects the stored certificate.
func assertStoredCaptureAuthorityMatchesWriterPredicate(t *testing.T, s *Store, sid schema.SessionID, present bool) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	capture, found, readErr := readCapture(conn, sid)
	s.pool.Put(conn)
	if readErr != nil {
		t.Fatalf("readCapture: %v", readErr)
	}
	if found != present {
		t.Fatalf("stored capture found = %v, want %v", found, present)
	}
	authority, err := s.ReadStoredCaptureAuthority(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	want := found && PublishableWithOmissions(capture)
	if (authority != nil) != want {
		t.Fatalf("authority present = %v, but the writer predicate says %v", authority != nil, want)
	}
}
