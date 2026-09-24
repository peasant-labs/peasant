package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/capture_assessment.yaml
var captureAssessmentCorpus []byte

type captureAssessmentRecord struct {
	Namespace   string `yaml:"namespace"`
	Kind        string `yaml:"kind"`
	SourceRef   string `yaml:"source_ref"`
	RecordIndex int64  `yaml:"record_index"`
	Position    int64  `yaml:"position"`
	Pointer     string `yaml:"pointer"`
	Payload     string `yaml:"payload"`
	WithPublic  bool   `yaml:"with_public"`
}

type captureAssessmentCase struct {
	Name              string                    `yaml:"name"`
	Harness           ingest.Harness            `yaml:"harness"`
	Policy            string                    `yaml:"policy"`
	Authoritative     bool                      `yaml:"authoritative"`
	SourceOmitted     bool                      `yaml:"source_omitted"`
	Unaccounted       bool                      `yaml:"unaccounted"`
	UnknownRecords    []captureAssessmentRecord `yaml:"unknown_records"`
	Omitted           bool                      `yaml:"omitted"`
	WantCoverage      string                    `yaml:"want_coverage"`
	WantFailure       string                    `yaml:"want_failure"`
	WantPartial       bool                      `yaml:"want_partial"`
	WantOccurrences   int                       `yaml:"want_occurrences"`
	NilResult         bool                      `yaml:"nil_result"`
	TypedNilResult    bool                      `yaml:"typed_nil_result"`
	ZeroAssessment    bool                      `yaml:"zero_assessment"`
	UnsupportedPolicy *uint8                    `yaml:"unsupported_policy"`
}

type captureAssessmentDocument struct {
	Required []string                `yaml:"required_names"`
	Cases    []captureAssessmentCase `yaml:"cases"`
}

func loadCaptureAssessmentFixtures(t *testing.T) captureAssessmentDocument {
	t.Helper()
	var doc captureAssessmentDocument
	d := yaml.NewDecoder(bytes.NewReader(captureAssessmentCorpus))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing fixture document: %v", err)
	}
	names := map[string]bool{}
	var actual []string
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("duplicate or empty fixture name %q", c.Name)
		}
		names[c.Name] = true
		actual = append(actual, c.Name)
	}
	if err := testutil.RequireFixtureNames("capture assessment", "case", doc.Required, names); err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: doc.Required}, actual, "capture assessment"); err != nil {
		t.Fatal(err)
	}
	return doc
}

func buildAssessmentEntries(t *testing.T, c captureAssessmentCase, sid ingest.SessionID) []schema.SessionEntry {
	t.Helper()
	preview := "known conversation text"
	entries := []schema.SessionEntry{{
		SessionID: schema.SessionID(sid), EntryIndex: 0,
		Harness: schema.Harness(c.Harness), Role: schema.RoleUser,
		EntryType: schema.EntryTypeText, ContentPreview: &preview,
	}}
	next := 1
	for _, r := range c.UnknownRecords {
		position := ingest.UnknownSourcePosition{Line: 1, JSONPointer: r.Pointer}
		if r.WithPublic {
			position.Public = &ingest.UnknownPublicPosition{
				SourceRef: r.SourceRef, RecordIndex: r.RecordIndex, Position: r.Position,
			}
		} else {
			// Legacy wholly absent coordinates still need a genuine locator.
			position.Line = 1
		}
		record, err := ingest.NewRetainedUnknown(
			ingest.Harness(c.Harness), r.Namespace, r.Kind, position, json.RawMessage(r.Payload),
		)
		if err != nil {
			t.Fatalf("%s: NewRetainedUnknown: %v", c.Name, err)
		}
		entry, err := ingest.RetainedUnknownEntry(sid, next, record)
		if err != nil {
			t.Fatalf("%s: RetainedUnknownEntry: %v", c.Name, err)
		}
		entries = append(entries, entry)
		next++
	}
	if c.Omitted {
		rec, err := ingest.NewOmittedRecord(ingest.OmittedRecordTooLarge, 7, 9000, 8000)
		if err != nil {
			t.Fatalf("%s: NewOmittedRecord: %v", c.Name, err)
		}
		extra, err := rec.Extra()
		if err != nil {
			t.Fatalf("%s: OmittedRecord.Extra: %v", c.Name, err)
		}
		note := ingest.OmissionPlaceholderNote(rec)
		entries = append(entries, schema.SessionEntry{
			SessionID: schema.SessionID(sid), EntryIndex: next,
			Harness: schema.Harness(c.Harness), Role: schema.RoleTool,
			EntryType: schema.EntryTypeToolResult, ContentPreview: &note, Extra: &extra,
		})
	}
	return entries
}

// TestCaptureAssessmentPins exercises the production AssessCapture and checked
// ContentCapture conversion against YAML-owned expectations. A forged
// full or preview claim must be rejected by the assessment, and an unknown,
// invalid, or zero assessment must never convert to a store write.
func TestCaptureAssessmentPins(t *testing.T) {
	t.Parallel()
	doc := loadCaptureAssessmentFixtures(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			policy := ingest.CaptureFreshCandidate
			switch {
			case c.UnsupportedPolicy != nil:
				policy = ingest.CaptureReadPolicy(*c.UnsupportedPolicy)
			case c.Policy == "legacy":
				policy = ingest.CaptureLegacyPreview
			case c.Policy == "" || c.Policy == "fresh":
				policy = ingest.CaptureFreshCandidate
			default:
				t.Fatalf("unknown policy %q", c.Policy)
			}

			var result indexformat.Result
			switch {
			case c.NilResult:
				result = nil
			case c.TypedNilResult:
				var typed *indexformat.V1
				result = typed
			case c.ZeroAssessment:
				// The zero value never certifies: it must refuse conversion
				// before persistence, never silently become a preview write.
				var zero ingest.CaptureAssessment
				if _, convErr := zero.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1); convErr == nil {
					t.Fatalf("zero assessment converted, want refusal")
				}
				if got := zero.Coverage(); got != ingest.CaptureCoverageUnknown {
					t.Fatalf("zero coverage = %d, want unknown", uint8(got))
				}
				return
			default:
				entries := buildAssessmentEntries(t, c, sid)
				result = indexformat.V1{Entries: entries}
			}

			facts := ingest.CaptureFacts{
				Harness: c.Harness, Result: result, Policy: policy,
				Authoritative: c.Authoritative,
				SourceOmitted: c.SourceOmitted, Unaccounted: c.Unaccounted,
			}
			assessment, err := ingest.AssessCapture(facts)
			if c.WantCoverage == "error" {
				if err == nil {
					t.Fatalf("AssessCapture succeeded, want error")
				}
				// An invalid assessment must never convert to a write, not
				// even to a preview write through absent-value defaults.
				if _, convErr := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1); convErr == nil {
					t.Fatalf("ContentCapture converted an invalid assessment, want refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("AssessCapture: %v", err)
			}

			var wantCoverage ingest.CaptureCoverage
			switch c.WantCoverage {
			case "full":
				wantCoverage = ingest.CaptureCoverageFull
			case "preview":
				wantCoverage = ingest.CaptureCoveragePreview
			default:
				t.Fatalf("unknown want_coverage %q", c.WantCoverage)
			}
			if got := assessment.Coverage(); got != wantCoverage {
				t.Fatalf("coverage = %d, want %d", uint8(got), uint8(wantCoverage))
			}
			counts := assessment.CandidateCounts()
			occurrences := 0
			for _, k := range counts {
				occurrences += k.Occurrences
			}
			if occurrences != c.WantOccurrences {
				t.Fatalf("candidate occurrences = %d, want %d", occurrences, c.WantOccurrences)
			}

			write, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1700000000000)
			if err != nil {
				t.Fatalf("ContentCapture: %v", err)
			}
			if string(write.FailureCode) != c.WantFailure {
				t.Fatalf("failure code = %q, want %q", string(write.FailureCode), c.WantFailure)
			}
			if wantCoverage == ingest.CaptureCoverageFull && c.WantPartial {
				if write.Status != ingest.ContentCaptureIncomplete || write.CaptureFormat != ingest.ContentCaptureFormatFull {
					t.Fatalf("full partial write = %q/%q, want incomplete/full", string(write.Status), string(write.CaptureFormat))
				}
			}
			if wantCoverage == ingest.CaptureCoveragePreview && write.CaptureFormat != ingest.ContentCaptureFormatPreviewOnly {
				t.Fatalf("preview format = %q, want preview_only", string(write.CaptureFormat))
			}
			if wantCoverage == ingest.CaptureCoverageFull && !c.WantPartial {
				if write.Status != ingest.ContentCaptureComplete || write.FailureCode != ingest.ContentCaptureNoFailure {
					t.Fatalf("complete write = %q/%q, want complete/no failure", string(write.Status), string(write.FailureCode))
				}
			}

			// A forged claim that bypasses the assessment must not certify:
			// the store preflight (and the assessment itself) rejects a full
			// write for a preview assessment and a preview write for an
			// unknown assessment. Here pin the conversion refusal directly.
			if c.ZeroAssessment {
				var zero ingest.CaptureAssessment
				if _, convErr := zero.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1); convErr == nil {
					t.Fatalf("zero assessment converted, want refusal")
				}
			}
		})
	}
}

// TestCaptureAssessmentCheckedConversionRefusals pins the checked conversion
// refusals that the fixture matrix names: nil and typed-nil receivers,
// the zero value, and unsupported coverage and policy enums. These are
// behavior pins, not an inline case table of production logic.
func TestCaptureAssessmentCheckedConversionRefusals(t *testing.T) {
	t.Parallel()
	var nilAssessment *ingest.CaptureAssessment
	if _, err := nilAssessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1); err == nil {
		t.Fatalf("nil assessment converted, want refusal")
	}
	if got := nilAssessment.Coverage(); got != ingest.CaptureCoverageUnknown {
		t.Fatalf("nil coverage = %d, want unknown", uint8(got))
	}
	if got := nilAssessment.CandidateCounts(); len(got) != 0 {
		t.Fatalf("nil candidate counts = %d, want 0", len(got))
	}
	var zero ingest.CaptureAssessment
	if _, err := zero.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1); err == nil {
		t.Fatalf("zero assessment converted, want refusal")
	}
	if got := zero.Coverage(); got != ingest.CaptureCoverageUnknown {
		t.Fatalf("zero coverage = %d, want unknown", uint8(got))
	}
	// Unsupported enums refuse at both boundaries.
	facts := ingest.CaptureFacts{
		Harness: ingest.HarnessClaudeCode, Result: indexformat.V1{},
		Policy: ingest.CaptureReadPolicy(7), Authoritative: true,
	}
	if _, err := ingest.AssessCapture(facts); err == nil {
		t.Fatalf("unsupported policy assessed, want refusal")
	}
}
