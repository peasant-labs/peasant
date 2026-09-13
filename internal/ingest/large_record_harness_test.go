package ingest

import (
	_ "embed"
	"strings"
	"testing"

	"bytes"
	"io"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// largeRecordSessionID is the session id every case uses. It is a literal so
// the case needs no helper package, which keeps this file inside package
// ingest and lets it call the production filter directly instead of through a
// test-only export.
const largeRecordSessionID = "99d59925-36bc-424c-a789-8be54d9702ba"

//go:embed testdata/large_record_harnesses.yaml
var largeRecordHarnessYAML []byte

const largeRecordHarnessFixturePath = "internal/ingest/testdata/large_record_harnesses.yaml"

// retiredPerLineLimit is the 10 MiB per-line limit this build replaced. The
// "indexed whole" cases build a record over it, so restoring that limit turns
// every one of them red.
const retiredPerLineLimit = 10 << 20

// injectedOverLimit is the small per-record limit the omission cases inject
// through the indexer registry and the filter parameter. Nothing global
// changes, so these cases stay safe beside any other test.
const injectedOverLimit = 4096

type largeRecordHarnessFixture struct {
	Name            string   `yaml:"name"`
	Harness         string   `yaml:"harness"`
	LeadingRecords  []string `yaml:"leadingRecords"`
	LargeRecord     string   `yaml:"largeRecord"`
	TrailingRecords []string `yaml:"trailingRecords"`
	WantToolCallID  string   `yaml:"wantToolCallId"`
	// ProjectionFollowsPhysicalOrder is false for a harness whose indexer
	// projects a tree rather than the physical record order, where the
	// placeholder's index cannot be compared against the record order.
	ProjectionFollowsPhysicalOrder bool `yaml:"projectionFollowsPhysicalOrder"`
}

// build renders the transcript with the large record padded to exactly
// recordBytes, and returns the transcript and the sentinel the large record
// carries.
func (f largeRecordHarnessFixture) build(t *testing.T, recordBytes int) ([]byte, string) {
	t.Helper()
	marker := "large-record-marker-" + f.Name
	skeleton := strings.ReplaceAll(f.LargeRecord, "MARK", marker)
	skeleton = strings.ReplaceAll(skeleton, "SESSION_ID", largeRecordSessionID)
	base := strings.ReplaceAll(skeleton, "PAD", "")
	if len(base) > recordBytes {
		t.Fatalf("%s: the large record is already %d bytes, over the %d-byte target", f.Name, len(base), recordBytes)
	}
	large := strings.ReplaceAll(skeleton, "PAD", strings.Repeat("x", recordBytes-len(base)))
	if len(large) != recordBytes {
		t.Fatalf("%s: built a %d-byte record, want exactly %d", f.Name, len(large), recordBytes)
	}

	var builder strings.Builder
	for _, record := range f.LeadingRecords {
		builder.WriteString(strings.ReplaceAll(record, "SESSION_ID", largeRecordSessionID))
		builder.WriteByte('\n')
	}
	builder.WriteString(large)
	builder.WriteByte('\n')
	for _, record := range f.TrailingRecords {
		builder.WriteString(strings.ReplaceAll(record, "SESSION_ID", largeRecordSessionID))
		builder.WriteByte('\n')
	}
	return []byte(builder.String()), marker
}

func loadLargeRecordHarnessFixtures(t *testing.T) ([]largeRecordHarnessFixture, []string, []string) {
	t.Helper()
	var fixture struct {
		SurvivingRecordTexts []string                    `yaml:"survivingRecordTexts"`
		RequiredNames        []string                    `yaml:"requiredNames"`
		Harnesses            []largeRecordHarnessFixture `yaml:"harnesses"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(largeRecordHarnessYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode committed fixture %s: %v", largeRecordHarnessFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", largeRecordHarnessFixturePath, err)
	}
	seen := make(map[string]bool, len(fixture.Harnesses))
	for _, harness := range fixture.Harnesses {
		if harness.Name == "" || seen[harness.Name] {
			t.Fatalf("missing or duplicate harness name %q in %s", harness.Name, largeRecordHarnessFixturePath)
		}
		seen[harness.Name] = true
	}
	if len(fixture.SurvivingRecordTexts) == 0 {
		t.Fatalf("committed fixture %s names no survivingRecordTexts, so nothing would check that the records around an omitted one were kept", largeRecordHarnessFixturePath)
	}
	for index, text := range fixture.SurvivingRecordTexts {
		if strings.TrimSpace(text) == "" {
			t.Fatalf("committed fixture %s survivingRecordTexts[%d] is empty; an empty needle is found in every transcript and would check nothing", largeRecordHarnessFixturePath, index)
		}
	}
	return fixture.Harnesses, fixture.RequiredNames, fixture.SurvivingRecordTexts
}

// TestLargeRecordsAreHandledUniformlyAcrossHarnesses is the one shared
// expectation the user asked for: every JSONL harness reads a large record
// whole, and omits an over-limit one the same way, with the same diagnostic
// and the same placeholder.
func TestLargeRecordsAreHandledUniformlyAcrossHarnesses(t *testing.T) {
	harnesses, requiredNames, survivingRecordTexts := loadLargeRecordHarnessFixtures(t)
	ran := make(map[string]bool, len(requiredNames))

	for _, fixture := range harnesses {
		harness := Harness(fixture.Harness)

		t.Run(fixture.Name+"-large-record-indexed-whole", func(t *testing.T) {
			ran[fixture.Name+"-large-record-indexed-whole"] = true
			data, marker := fixture.build(t, retiredPerLineLimit+1)

			// The source filter must leave a record within the limit alone,
			// byte for byte, and report nothing about it.
			filtered, diagnostics, err := filterOversizedJSONLRecords(
				t.Context(), data, "/synthetic/transcript.jsonl", defaults.MaxJSONLRecordBytes,
			)
			if err != nil {
				t.Fatalf("the filter refused a %d-byte record: %v", retiredPerLineLimit+1, err)
			}
			if len(diagnostics) != 0 {
				t.Fatalf("a record within the limit was reported: %+v", diagnostics)
			}
			if string(filtered) != string(data) {
				t.Fatal("a record within the limit was rewritten by the filter")
			}

			entries := indexHarnessTranscript(t, harness, filtered, 0)
			if placeholders := omissionPlaceholders(t, entries); len(placeholders) != 0 {
				t.Fatalf("a record within the limit produced %d omission placeholder(s)", len(placeholders))
			}
			if !entriesCarry(entries, marker) {
				t.Fatalf("the %d-byte record was not indexed: no entry carries its content", retiredPerLineLimit+1)
			}
			assertMetadataExtractionSucceeds(t, harness, filtered)
		})

		t.Run(fixture.Name+"-over-limit-record-omitted", func(t *testing.T) {
			ran[fixture.Name+"-over-limit-record-omitted"] = true
			data, marker := fixture.build(t, injectedOverLimit+1)

			filtered, diagnostics, err := filterOversizedJSONLRecords(
				t.Context(), data, "/synthetic/transcript.jsonl", injectedOverLimit,
			)
			if err != nil {
				t.Fatalf("the filter refused an over-limit record instead of omitting it: %v", err)
			}
			if len(diagnostics) != 1 || diagnostics[0].ErrorType != OversizedRecordDiagnosticType {
				t.Fatalf("diagnostics = %+v, want exactly one %q warning", diagnostics, OversizedRecordDiagnosticType)
			}
			if strings.Contains(string(filtered), marker) {
				t.Fatal("content of the omitted record survived into the filtered artifact")
			}

			// The session still ingests: it is a partial capture, never a
			// failure and never a silent gap.
			meta := NewUnifiedMetadata()
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, diagnostics...)
			if !captureContentOmitted(&meta) {
				t.Fatal("the capture was not marked as omitting content, so the session would be stored as complete")
			}

			entries := indexHarnessTranscript(t, harness, filtered, injectedOverLimit)
			placeholders := omissionPlaceholders(t, entries)
			if len(placeholders) != 1 {
				t.Fatalf("want exactly one omission placeholder, got %d in %d entries", len(placeholders), len(entries))
			}
			index, placeholder := placeholders[0].index, placeholders[0].entry
			if fixture.ProjectionFollowsPhysicalOrder {
				if index == 0 {
					t.Error("the placeholder is the first entry; the records before the omitted one were lost")
				}
				if index == len(entries)-1 {
					t.Error("the placeholder is the last entry; the records after the omitted one were lost")
				}
			} else if len(entries) < 2 {
				t.Errorf("the placeholder is the only entry; every record around the omitted one was lost")
			}
			record, err := ParseOmittedRecord(*placeholder.Extra)
			if err != nil {
				t.Fatalf("the placeholder's extra field is not a typed omission record: %v", err)
			}
			if record.Reason != OmittedRecordTooLarge {
				t.Errorf("omission reason = %q, want %q", record.Reason, OmittedRecordTooLarge)
			}
			if record.Bytes != int64(injectedOverLimit+1) || record.LimitBytes != injectedOverLimit {
				t.Errorf("omission record = %+v, want %d bytes over a %d-byte limit", record, injectedOverLimit+1, injectedOverLimit)
			}
			if placeholder.Role != schema.RoleTool || placeholder.EntryType != schema.EntryTypeToolResult {
				t.Errorf("placeholder is role %q type %q, want role %q type %q",
					placeholder.Role, placeholder.EntryType, schema.RoleTool, schema.EntryTypeToolResult)
			}
			if placeholder.ContentPreview == nil || !strings.Contains(*placeholder.ContentPreview, "only showing preview of tool output") {
				t.Errorf("placeholder note = %v, want the reader-facing omission note", placeholder.ContentPreview)
			}
			if fixture.WantToolCallID != "" {
				if placeholder.ToolCallID == nil || *placeholder.ToolCallID != fixture.WantToolCallID {
					t.Errorf("placeholder tool call id = %v, want %q so it pairs with its tool call", placeholder.ToolCallID, fixture.WantToolCallID)
				}
			}
			if entriesCarry(entries, marker) {
				t.Fatal("content of the omitted record reached the indexed entries")
			}
			// Omitting one record costs one record. The stored diagnostic
			// tells the user that every other record was kept and indexed, so
			// the records on both sides of the omitted one must be there.
			for _, text := range survivingRecordTexts {
				if !entriesCarry(entries, text) {
					t.Errorf("the record carrying %q is missing from the %d indexed entries; omitting one record cost more than that record", text, len(entries))
				}
			}
		})
	}

	for _, name := range requiredNames {
		if !ran[name] {
			t.Errorf("required case %q did not run; a case named in requiredNames must not be deleted from %s", name, largeRecordHarnessFixturePath)
		}
	}
}

func indexHarnessTranscript(t *testing.T, harness Harness, data []byte, maxRecordBytes int) []schema.SessionEntry {
	t.Helper()
	session := DiscoveredSession{
		SessionID:  schema.SessionID(largeRecordSessionID),
		Harness:    harness,
		SourcePath: "/synthetic/transcript.jsonl",
	}
	indexer := NewIndexerRegistry(nil, IndexerRegistryOptions{MaxRecordBytes: maxRecordBytes})[harness]
	if indexer == nil {
		t.Fatalf("no indexer is registered for harness %q", harness)
	}
	entries, err := indexer.IndexTranscriptBytes(t.Context(), session, data)
	if err != nil {
		t.Fatalf("%s indexer refused the transcript: %v; no record size may fail a session", harness, err)
	}
	if versioned, ok := indexer.(VersionedTranscriptIndexer); ok {
		if result, resultErr := versioned.IndexTranscriptBytesResult(t.Context(), session, data); resultErr == nil {
			if v1, isV1 := result.(indexformat.V1); isV1 {
				return v1.Entries
			}
		}
	}
	return entries
}

// assertMetadataExtractionSucceeds mounts the harness adapter's own scan of
// the transcript bytes, which is where the replaced per-line limit used to
// fail the whole session with bufio.ErrTooLong.
func assertMetadataExtractionSucceeds(t *testing.T, harness Harness, data []byte) {
	t.Helper()
	factory, known := DefaultAdapterRegistry[harness]
	if !known {
		t.Fatalf("no adapter is registered for harness %q", harness)
	}
	adapter := factory(nil, nil, salt.Salt{})
	extractor, scans := adapter.(TranscriptMetadataExtractor)
	if !scans {
		// A harness whose adapter finds record boundaries itself never used
		// the replaced per-line limit and has nothing to prove here.
		return
	}
	original := NewUnifiedMetadata()
	original.SessionID = SessionID(largeRecordSessionID)
	original.ModelHarness = harness
	original.HostSlug = HostSlug("github.com--testuser--testrepo")
	original.Source = SourceInfo{FilePath: "/synthetic/transcript.jsonl", Format: SourceFormatJSONL}
	if _, err := extractor.ExtractMetadataFromTranscript(t.Context(), data, &original); err != nil {
		t.Fatalf("%s adapter refused a transcript holding a record over the retired 10 MiB per-line limit: %v", harness, err)
	}
}

type placeholderAt struct {
	index int
	entry schema.SessionEntry
}

func omissionPlaceholders(t *testing.T, entries []schema.SessionEntry) []placeholderAt {
	t.Helper()
	var found []placeholderAt
	for index, entry := range entries {
		if entry.Extra == nil {
			continue
		}
		if _, err := ParseOmittedRecord(*entry.Extra); err == nil {
			found = append(found, placeholderAt{index: index, entry: entry})
		}
	}
	return found
}

func entriesCarry(entries []schema.SessionEntry, marker string) bool {
	for _, entry := range entries {
		for _, field := range []*string{entry.ContentPreview, entry.ToolOutput, entry.ToolInput, entry.Extra} {
			if field != nil && strings.Contains(*field, marker) {
				return true
			}
		}
	}
	return false
}
