package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/capture_assessment.yaml
var captureAssessmentCorpus []byte

// Omission placeholder dimensions shared with the store last-good guard: the
// source line that overflowed, its byte size, and the limit it exceeded.
const (
	omissionPlaceholderLine  = 7
	omissionPlaceholderSize  = 9000
	omissionPlaceholderLimit = 8000
)

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

func loadCaptureAssessment(data []byte) (captureAssessmentDocument, error) {
	var doc captureAssessmentDocument
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		return captureAssessmentDocument{}, fmt.Errorf("decode capture assessment fixture first document: %w", err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		return captureAssessmentDocument{}, fmt.Errorf("capture assessment fixture must contain exactly one YAML document: %v", trailing)
	}
	names := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			return captureAssessmentDocument{}, fmt.Errorf("capture assessment fixture holds a duplicate or empty case name %q; each assessment shape needs exactly one pin", c.Name)
		}
		names[c.Name] = true
	}
	for _, required := range requiredCaptureAssessmentCaseNames {
		if !names[required] {
			return captureAssessmentDocument{}, fmt.Errorf("capture assessment fixture is missing required case %q; restore the pin instead of shrinking coverage", required)
		}
	}
	for name := range names {
		if !slices.Contains(requiredCaptureAssessmentCaseNames, name) {
			return captureAssessmentDocument{}, fmt.Errorf("capture assessment fixture holds undeclared case %q; declare it in the required manifest or remove it", name)
		}
	}
	return doc, nil
}

func loadCaptureAssessmentFixtures(t *testing.T) captureAssessmentDocument {
	t.Helper()
	doc, err := loadCaptureAssessment(captureAssessmentCorpus)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// requiredCaptureAssessmentCaseNames is the deletion guard. Each name is a V1
// assessment shape the pins below prove; removing or renaming one must fail
// the loader, not silently shrink coverage. It is the single manifest source
// of truth: the YAML required_names header documents the same set for
// readers, and the loader pins below prove refusal of missing, undeclared,
// and same-count-renamed corpora.
var requiredCaptureAssessmentCaseNames = []string{
	"known-only",
	"unknown-only",
	"omitted-only",
	"mixed-accounted",
	"mixed-unaccounted",
	"legacy-only",
	"declared-format-mismatch",
	"nil-result",
	"typed-nil-result",
	"zero-assessment",
	"unsupported-enum",
}

func marshalCaptureAssessment(t *testing.T, doc captureAssessmentDocument) []byte {
	t.Helper()
	encoded, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal mutated fixture: %v", err)
	}
	return encoded
}

func TestLoadCaptureAssessmentRejectsDeletedCase(t *testing.T) {
	t.Parallel()
	doc, err := loadCaptureAssessment(captureAssessmentCorpus)
	if err != nil {
		t.Fatalf("load capture assessment fixture: %v", err)
	}
	trimmed := doc
	trimmed.Cases = append([]captureAssessmentCase(nil), doc.Cases[1:]...)
	if _, err := loadCaptureAssessment(marshalCaptureAssessment(t, trimmed)); err == nil {
		t.Fatal("loader accepted a fixture with a required case removed")
	} else if !strings.Contains(err.Error(), "missing required case") {
		t.Fatalf("loader refused for the wrong reason: %v", err)
	}
}

func TestLoadCaptureAssessmentRejectsUndeclaredCase(t *testing.T) {
	t.Parallel()
	doc, err := loadCaptureAssessment(captureAssessmentCorpus)
	if err != nil {
		t.Fatalf("load capture assessment fixture: %v", err)
	}
	extended := doc
	extended.Cases = append(append([]captureAssessmentCase(nil), doc.Cases...),
		captureAssessmentCase{Name: "undeclared-probe", Harness: ingest.HarnessClaudeCode, Policy: "fresh", WantCoverage: "full"})
	if _, err := loadCaptureAssessment(marshalCaptureAssessment(t, extended)); err == nil {
		t.Fatal("loader accepted a fixture with an undeclared case")
	} else if !strings.Contains(err.Error(), "undeclared case") {
		t.Fatalf("loader refused for the wrong reason: %v", err)
	}
}

func TestLoadCaptureAssessmentRejectsSameCountRenamedCase(t *testing.T) {
	t.Parallel()
	doc, err := loadCaptureAssessment(captureAssessmentCorpus)
	if err != nil {
		t.Fatalf("load capture assessment fixture: %v", err)
	}
	renamed := doc
	renamed.Cases = append([]captureAssessmentCase(nil), doc.Cases...)
	renamed.Cases[0].Name = "renamed-probe"
	if _, err := loadCaptureAssessment(marshalCaptureAssessment(t, renamed)); err == nil {
		t.Fatal("loader accepted a fixture with a required case renamed at the same count")
	} else if !strings.Contains(err.Error(), "missing required case") {
		t.Fatalf("loader refused for the wrong reason: %v", err)
	}
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
		rec, err := ingest.NewOmittedRecord(ingest.OmittedRecordTooLarge, omissionPlaceholderLine, omissionPlaceholderSize, omissionPlaceholderLimit)
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

// assessmentPolicyForCase resolves the fixture policy name to the production
// read policy, refusing unknown names so a typo cannot silently certify.
func assessmentPolicyForCase(t *testing.T, c captureAssessmentCase) ingest.CaptureReadPolicy {
	t.Helper()
	if c.UnsupportedPolicy != nil {
		return ingest.CaptureReadPolicy(*c.UnsupportedPolicy)
	}
	switch c.Policy {
	case "legacy":
		return ingest.CaptureLegacyPreview
	case "", "fresh":
		return ingest.CaptureFreshCandidate
	default:
		t.Fatalf("unknown policy %q", c.Policy)
		return ingest.CaptureFreshCandidate
	}
}

// TestCaptureAssessmentPins exercises the production AssessCapture and checked
// ContentCapture conversion against YAML-owned expectations. An unknown,
// invalid, or zero assessment must never convert to a store write.
// Forged-claim disagreement is pinned by TestCaptureAssessmentForgedTupleDisagrees.
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
			policy := assessmentPolicyForCase(t, c)

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
		})
	}
}

// TestCaptureAssessmentForgedTupleDisagrees proves at the ingest level that a
// hand-built complete/full claim cannot pass as the assessment's own
// conversion. For every fixture case that yields a genuine write, the
// canonical forgery (complete/full with no failure code) must disagree with
// the assessed status/format/code triple — except the one case that genuinely
// certifies complete full, which must agree as the control that keeps the
// comparison from being vacuous. The store pins the same comparison at the
// boundary in TestCaptureAssessmentLastGoodGuard.
func TestCaptureAssessmentForgedTupleDisagrees(t *testing.T) {
	t.Parallel()
	doc := loadCaptureAssessmentFixtures(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.WantCoverage == "error" {
				t.Skip("refusal cases yield no genuine write to compare against")
			}
			sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			entries := buildAssessmentEntries(t, c, sid)
			assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
				Harness: c.Harness, Result: indexformat.V1{Entries: entries},
				Policy:        assessmentPolicyForCase(t, c),
				Authoritative: c.Authoritative,
				SourceOmitted: c.SourceOmitted, Unaccounted: c.Unaccounted,
			})
			if err != nil {
				t.Fatalf("AssessCapture: %v", err)
			}
			genuine, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1700000000000)
			if err != nil {
				t.Fatalf("ContentCapture: %v", err)
			}
			forged := ingest.SessionContentCaptureWrite{
				Status: ingest.ContentCaptureComplete, CaptureFormat: ingest.ContentCaptureFormatFull,
			}
			control := c.WantCoverage == "full" && !c.WantPartial && c.WantFailure == ""
			disagrees := genuine.Status != forged.Status || genuine.CaptureFormat != forged.CaptureFormat || genuine.FailureCode != forged.FailureCode
			if control && disagrees {
				t.Fatalf("control case %q: genuine complete/full write disagrees with itself: %+v", c.Name, genuine)
			}
			if !control && !disagrees {
				t.Fatalf("case %q: forged complete/full claim agrees with assessed %+v/%+v/%+v", c.Name, genuine.Status, genuine.CaptureFormat, genuine.FailureCode)
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
	// An interface-boxed typed-nil assessment still arrives as a nil receiver
	// and must refuse without any reflection defense.
	var boxedNil *ingest.CaptureAssessment
	var boxed any = boxedNil
	if boxed == nil {
		t.Fatal("test setup: interface holding a typed-nil pointer must be non-nil")
	}
	if got := boxed.(*ingest.CaptureAssessment).Coverage(); got != ingest.CaptureCoverageUnknown {
		t.Fatalf("boxed typed-nil coverage = %d, want unknown", uint8(got))
	}
	if _, err := boxed.(*ingest.CaptureAssessment).ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1); err == nil {
		t.Fatal("boxed typed-nil assessment converted, want refusal")
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
