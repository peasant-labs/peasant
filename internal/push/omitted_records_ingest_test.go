package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/omitted_records_ingest.yaml
var omittedRecordsIngestYAML []byte

//go:embed testdata/omitted_records_ingest.manifest.yaml
var omittedRecordsIngestManifestYAML []byte

// omittedRecordsIngestCase is one real harvest of one JSONL session, described
// by the per-record limit it runs under and by every observable outcome that
// limit decides: what the store recorded, what the publication preflight says,
// what the published request declares, and what a reader is served.
type omittedRecordsIngestCase struct {
	Name                      string `yaml:"name"`
	LargeRecordShape          string `yaml:"largeRecordShape"`
	RecordLimitBytes          int    `yaml:"recordLimitBytes"`
	LargeRecordBytes          int    `yaml:"largeRecordBytes"`
	CaptureStatus             string `yaml:"captureStatus"`
	CaptureFailureCode        string `yaml:"captureFailureCode"`
	CaptureFormat             string `yaml:"captureFormat"`
	Readiness                 string `yaml:"readiness"`
	PreflightAccepts          bool   `yaml:"preflightAccepts"`
	PublishedPartial          bool   `yaml:"publishedPartial"`
	OmissionWarning           bool   `yaml:"omissionWarning"`
	PlaceholderInServedDetail bool   `yaml:"placeholderInServedDetail"`
	LargeRecordInServedDetail bool   `yaml:"largeRecordInServedDetail"`
	// TrailingRecordInServedDetail is the record AFTER the large one. It is
	// under the limit in every case, so it must survive the omission: a
	// filter that stopped at the stand-in line, or a projection that dropped
	// the turns after a placeholder, would otherwise leave every case green.
	TrailingRecordInServedDetail bool `yaml:"trailingRecordInServedDetail"`
}

type omittedRecordsIngestFixture struct {
	Cases []omittedRecordsIngestCase `yaml:"cases"`
}

func decodeOmittedRecordsIngestFixture(raw []byte) (omittedRecordsIngestFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var fixture omittedRecordsIngestFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, errors.New("omitted-records ingest fixture must contain exactly one YAML document")
	}
	return fixture, nil
}

func loadOmittedRecordsIngestFixture(t *testing.T) omittedRecordsIngestFixture {
	t.Helper()
	fixture, err := decodeOmittedRecordsIngestFixture(omittedRecordsIngestYAML)
	if err != nil {
		t.Fatalf("decode omitted-records ingest fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(omittedRecordsIngestManifestYAML, "omitted-records ingest")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		// Every enum the fixture names is parsed at the boundary, so a typo in
		// the fixture is a fixture error rather than a silent mismatch later.
		if _, err := ingest.NewContentCaptureStatus(fixtureCase.CaptureStatus); err != nil {
			t.Fatalf("omitted-records ingest case %q: %v", fixtureCase.Name, err)
		}
		if _, err := ingest.NewContentCaptureFailureCode(fixtureCase.CaptureFailureCode); err != nil {
			t.Fatalf("omitted-records ingest case %q: %v", fixtureCase.Name, err)
		}
		if _, err := ingest.NewContentCaptureFormat(fixtureCase.CaptureFormat); err != nil {
			t.Fatalf("omitted-records ingest case %q: %v", fixtureCase.Name, err)
		}
		if _, err := publicationOmissionsReadiness(fixtureCase.Readiness); err != nil {
			t.Fatalf("omitted-records ingest case %q: %v", fixtureCase.Name, err)
		}
		if _, err := newLargeRecordShape(fixtureCase.LargeRecordShape); err != nil {
			t.Fatalf("omitted-records ingest case %q: %v", fixtureCase.Name, err)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "omitted-records ingest"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestOmittedRecordsIngestFixtureGuards(t *testing.T) {
	t.Parallel()
	loadOmittedRecordsIngestFixture(t)
	manifest, err := testutil.DecodeRequiredNamesManifest(omittedRecordsIngestManifestYAML, "omitted-records ingest")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(omittedRecordsIngestYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		fixture, err := decodeOmittedRecordsIngestFixture(mutated)
		if err != nil {
			t.Fatalf("decode renamed fixture: %v", err)
		}
		names := make([]string, len(fixture.Cases))
		for index, fixtureCase := range fixture.Cases {
			names[index] = fixtureCase.Name
		}
		if err := testutil.ValidateRequiredNames(manifest, names, "omitted-records ingest"); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}

// omittedRecordsSessionID is the session both cases harvest. It is a synthetic
// UUID; no real recorded session is read anywhere in this file.
const omittedRecordsSessionID = "3f2c9d18-4a6b-4c1d-8e2f-5a7b9c0d1e2f"

// omittedRecordsSentinel marks the one large record. A served detail either
// carries it (the record was kept) or carries the placeholder note instead
// (the record was omitted); it can never carry both.
const omittedRecordsSentinel = "LARGE_RECORD_SENTINEL"

// omittedRecordsTrailingText is the closing record, which is under the limit in
// every case and must survive the omission.
const omittedRecordsTrailingText = "the tool finished"

// TestOmittedRecordsIngestPublishesEndToEnd drives the REAL ingest pipeline,
// the REAL store writer, the REAL readiness query, the REAL publication
// preflight and mapper, and the REAL served-detail projection over one JSONL
// session holding one large record.
//
// It is the end-to-end proof that an omitted-records session is publishable:
// the store-side rule was written against a capture state that ingest itself
// never produced, because the omitted-records session took the permanent-
// refusal path and its capture was written by the preview writer.
func TestOmittedRecordsIngestPublishesEndToEnd(t *testing.T) {
	fixture := loadOmittedRecordsIngestFixture(t)
	for _, fixtureCase := range fixture.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root := t.TempDir()

			sourceDir := filepath.Join(root, "source", "-workspace")
			if err := os.MkdirAll(sourceDir, 0o700); err != nil {
				t.Fatal(err)
			}
			shape, err := newLargeRecordShape(fixtureCase.LargeRecordShape)
			if err != nil {
				t.Fatal(err)
			}
			transcript := omittedRecordsTranscript(shape, fixtureCase.LargeRecordBytes)
			sourcePath := filepath.Join(sourceDir, omittedRecordsSessionID+".jsonl")
			if err := os.WriteFile(sourcePath, []byte(transcript), 0o600); err != nil {
				t.Fatal(err)
			}

			db, err := store.Open(filepath.Join(root, "peasant.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			fs := &ingest.OSFileSystem{}
			cfg := ingest.PipelineConfig{
				Sources: map[ingest.Harness]ingest.SourceConfig{
					ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join(root, "source"))}},
				},
				OutputDir:   ingest.ResolvedPath(filepath.Join(root, "output")),
				Parallelism: 1,
			}
			pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
				ingest.WithStore(db),
				ingest.WithMetricsStore(db),
				// The one injected value the two cases differ in. Production
				// passes defaults.MaxJSONLRecordBytes; a test may not build a
				// record of that size, so the limit is a parameter of the run
				// rather than a global anything could mutate.
				ingest.WithMaxJSONLRecordBytes(fixtureCase.RecordLimitBytes),
				ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{MaxRecordBytes: fixtureCase.RecordLimitBytes})),
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatalf("the harvest failed for a session holding a large record; it must ingest the session either way: %v", err)
			}
			if result.Summary.New != 1 {
				t.Fatalf("the harvest recorded %+v, want exactly one new session", result.Summary)
			}

			sid, err := ingest.NewSessionID(omittedRecordsSessionID)
			if err != nil {
				t.Fatal(err)
			}

			capture, found, err := db.GetSessionContentCapture(ctx, sid)
			if err != nil || !found {
				t.Fatalf("read back the stored capture: found=%v err=%v", found, err)
			}
			if string(capture.Status) != fixtureCase.CaptureStatus {
				t.Errorf("stored capture status = %q, want %q", capture.Status, fixtureCase.CaptureStatus)
			}
			if string(capture.FailureCode) != fixtureCase.CaptureFailureCode {
				t.Errorf("stored capture failure code = %q, want %q", capture.FailureCode, fixtureCase.CaptureFailureCode)
			}
			if string(capture.CaptureFormat) != fixtureCase.CaptureFormat {
				t.Errorf("stored capture format = %q, want %q; only a full capture may be read whole, exported and published", capture.CaptureFormat, fixtureCase.CaptureFormat)
			}
			if !store.PublishableWithOmissions(capture) {
				t.Errorf("the store rule refuses the capture real ingest wrote: status %q code %q format %q", capture.Status, capture.FailureCode, capture.CaptureFormat)
			}

			wantReadiness, err := publicationOmissionsReadiness(fixtureCase.Readiness)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := push.LoadPublicationInput(ctx, db, omittedRecordsSessionID)
			if err != nil {
				t.Fatalf("load the publication input the push command reads: %v", err)
			}
			if bundle.Readiness != wantReadiness {
				t.Errorf("readiness = %q, want %q", bundle.Readiness, wantReadiness)
			}
			preflightErr := push.ValidatePublicationInput(bundle)
			if (preflightErr == nil) != fixtureCase.PreflightAccepts {
				t.Fatalf("the push preflight said %v, want accepted=%v", preflightErr, fixtureCase.PreflightAccepts)
			}

			// The published request, built from the bundle metadata exactly as
			// the push pipeline builds it.
			meta := bundle.Metadata
			payload, err := push.MapMetadata(mapOpts(&meta, nil, nil))
			if err != nil {
				t.Fatalf("map the publication metadata: %v", err)
			}
			var request struct {
				Diagnostics struct {
					Partial  *bool `json:"partial"`
					Warnings []struct {
						ErrorType   string `json:"errorType"`
						Message     string `json:"message"`
						Remediation string `json:"remediation"`
					} `json:"warnings"`
				} `json:"diagnostics"`
			}
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Fatalf("decode the published metadata: %v", err)
			}
			gotPartial := request.Diagnostics.Partial != nil && *request.Diagnostics.Partial
			if gotPartial != fixtureCase.PublishedPartial {
				t.Errorf("the published request declares partial=%v, want %v", gotPartial, fixtureCase.PublishedPartial)
			}
			gotWarning := false
			for _, warning := range request.Diagnostics.Warnings {
				if warning.ErrorType != ingest.OversizedRecordDiagnosticType {
					continue
				}
				gotWarning = true
				// The remediation travels verbatim, so a reader of the
				// PUBLISHED session reads it. It may not send them away from
				// the sharing they have just done.
				for _, forbidden := range []string{"Publishing refuses", "before sharing this session"} {
					if strings.Contains(warning.Remediation, forbidden) {
						t.Errorf("the published omission warning tells a reader %q, on a session this same run published: %q", forbidden, warning.Remediation)
					}
				}
				if !strings.Contains(warning.Remediation, "publishable") {
					t.Errorf("the published omission warning does not tell the reader the session is publishable as it stands: %q", warning.Remediation)
				}
			}
			if gotWarning != fixtureCase.OmissionWarning {
				t.Errorf("the published request carries the omission warning=%v, want %v (warnings %+v)", gotWarning, fixtureCase.OmissionWarning, request.Diagnostics.Warnings)
			}

			// The complete-content reader export and publication both use. It
			// is the merge point between the writer that stores a placeholder
			// row and the reader that requires every entry with a content
			// preview to have one, so it is asserted on a REAL capture rather
			// than assumed.
			snapshot, err := db.ReadSessionContent(ctx, omittedRecordsSessionID)
			if err != nil {
				t.Fatalf("the complete-content reader refused the capture real ingest wrote: %v", err)
			}
			if snapshot == nil || len(snapshot.Entries) == 0 {
				t.Fatalf("the complete-content reader returned no entries for a publishable capture")
			}
			storedPlaceholders := 0
			for _, entry := range snapshot.Entries {
				if _, omitted := ingest.OmittedRecordOf(entry); omitted {
					storedPlaceholders++
				}
			}
			wantPlaceholders := 0
			if fixtureCase.PlaceholderInServedDetail {
				wantPlaceholders = 1
			}
			if storedPlaceholders != wantPlaceholders {
				t.Errorf("the complete-content reader returned %d omission placeholders, want %d", storedPlaceholders, wantPlaceholders)
			}
			storedTrailing := false
			for _, entry := range snapshot.Entries {
				for _, field := range []*string{entry.ContentPreview, entry.ToolOutput} {
					if field != nil && strings.Contains(*field, omittedRecordsTrailingText) {
						storedTrailing = true
					}
				}
			}
			if storedTrailing != fixtureCase.TrailingRecordInServedDetail {
				t.Errorf("the complete-content reader export and publication use returned the record after the large one=%v, want %v", storedTrailing, fixtureCase.TrailingRecordInServedDetail)
			}

			// What a reader is served. The placeholder note reaches every
			// transcript UI on the existing wire fields, so the served detail
			// is where it has to be visible.
			provider := api.NewStoreDataProvider(db, sessionvisibility.All())
			session, err := provider.SessionByID(ctx, omittedRecordsSessionID)
			if err != nil {
				t.Fatalf("the served session detail refused a session holding a large record: %v", err)
			}
			served, err := json.Marshal(api.SessionToDetail(session))
			if err != nil {
				t.Fatal(err)
			}
			gotPlaceholder := strings.Contains(string(served), "tool output omitted")
			if gotPlaceholder != fixtureCase.PlaceholderInServedDetail {
				t.Errorf("the served detail carries the omission placeholder note=%v, want %v", gotPlaceholder, fixtureCase.PlaceholderInServedDetail)
			}
			gotRecord := strings.Contains(string(served), omittedRecordsSentinel)
			if gotRecord != fixtureCase.LargeRecordInServedDetail {
				t.Errorf("the served detail carries the large record=%v, want %v", gotRecord, fixtureCase.LargeRecordInServedDetail)
			}
			gotTrailing := strings.Contains(string(served), omittedRecordsTrailingText)
			if gotTrailing != fixtureCase.TrailingRecordInServedDetail {
				t.Errorf("the served detail carries the record after the large one=%v, want %v; omitting one record must cost one record", gotTrailing, fixtureCase.TrailingRecordInServedDetail)
			}
		})
	}
}

// largeRecordShape names the source record a case makes large. It decides
// whether the omission placeholder can carry a tool call id at all: a Claude
// tool_result record opens with its tool_use_id, so the placeholder pairs with
// the tool call it answers; an assistant message carries no correlation id, so
// the placeholder has none and has to reach the reader on its own.
type largeRecordShape string

const (
	// largeRecordShapeClaudeToolResult makes the tool result large.
	largeRecordShapeClaudeToolResult largeRecordShape = "claude_tool_result"
	// largeRecordShapeClaudeAssistantText makes an assistant message large.
	largeRecordShapeClaudeAssistantText largeRecordShape = "claude_assistant_text"
)

// newLargeRecordShape parses a fixture value at the boundary, so a typo names
// itself instead of silently selecting the default transcript.
func newLargeRecordShape(raw string) (largeRecordShape, error) {
	switch largeRecordShape(raw) {
	case largeRecordShapeClaudeToolResult:
		return largeRecordShapeClaudeToolResult, nil
	case largeRecordShapeClaudeAssistantText:
		return largeRecordShapeClaudeAssistantText, nil
	}
	return "", fmt.Errorf(
		"omitted-records ingest fixture names large record shape %q, which no transcript builder in internal/push/omitted_records_ingest_test.go knows; the case cannot be built, so no outcome it declares is checked; use %q or %q",
		raw, largeRecordShapeClaudeToolResult, largeRecordShapeClaudeAssistantText,
	)
}

// omittedRecordsTranscript builds a Claude Code JSONL transcript whose third
// record is large, in the shape the case asks for: a user turn, an assistant
// turn, the large record, and a closing assistant turn that proves the records
// AFTER the large one survive.
func omittedRecordsTranscript(shape largeRecordShape, largeRecordBytes int) string {
	record := func(format string, args ...any) string { return fmt.Sprintf(format, args...) + "\n" }
	filler := strings.Repeat("x", largeRecordBytes)
	first := record(`{"sessionId":%q,"type":"user","uuid":"entry-first","timestamp":"2023-11-14T22:13:20Z","cwd":"/workspace","message":{"role":"user","content":"run the tool"}}`, omittedRecordsSessionID)
	last := record(`{"sessionId":%q,"type":"assistant","uuid":"entry-fourth","timestamp":"2023-11-14T22:13:23Z","message":{"role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"%s"}]}}`, omittedRecordsSessionID, omittedRecordsTrailingText)
	if shape == largeRecordShapeClaudeAssistantText {
		// No tool_use, no tool_use_id anywhere: the large record opens with
		// none of the correlation id keys, so its placeholder has no id.
		return first +
			record(`{"sessionId":%q,"type":"assistant","uuid":"entry-second","timestamp":"2023-11-14T22:13:21Z","message":{"role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"reading the file"}]}}`, omittedRecordsSessionID) +
			record(`{"sessionId":%q,"type":"assistant","uuid":"entry-third","timestamp":"2023-11-14T22:13:22Z","message":{"role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"%s%s"}]}}`, omittedRecordsSessionID, omittedRecordsSentinel, filler) +
			last
	}
	return first +
		record(`{"sessionId":%q,"type":"assistant","uuid":"entry-second","timestamp":"2023-11-14T22:13:21Z","message":{"role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"call-large","name":"Bash","input":{"command":"cat big"}}]}}`, omittedRecordsSessionID) +
		record(`{"sessionId":%q,"type":"user","uuid":"entry-third","timestamp":"2023-11-14T22:13:22Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-large","content":"%s%s"}]}}`, omittedRecordsSessionID, omittedRecordsSentinel, filler) +
		last
}
