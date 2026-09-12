package ingest

import (
	"bytes"
	_ "embed"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/oversized_record_sizes.yaml
var oversizedRecordSizeFixtureData []byte

const oversizedRecordSizeFixturePath = "internal/ingest/testdata/oversized_record_sizes.yaml"

// oversizedFilterTestLimit is the injected per-record limit these cases use.
// It is small so a case can build a record over it cheaply, and it is passed
// through the filter's parameter, so no global changes and parallel tests are
// unaffected.
const oversizedFilterTestLimit = 4096

type oversizedRecordSizeFixtures struct {
	RequiredNames []string                     `yaml:"requiredNames"`
	Cases         []oversizedRecordSizeFixture `yaml:"cases"`
}

type oversizedRecordSizeFixture struct {
	Name      string `yaml:"name"`
	SizeDelta int    `yaml:"sizeDelta"`
	Omitted   bool   `yaml:"omitted"`
}

func loadOversizedRecordSizeFixtures(t *testing.T) oversizedRecordSizeFixtures {
	t.Helper()
	var fixtures oversizedRecordSizeFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(oversizedRecordSizeFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", oversizedRecordSizeFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", oversizedRecordSizeFixturePath, err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("missing or duplicate boundary fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required boundary fixture %q missing from %s", name, oversizedRecordSizeFixturePath)
		}
	}
	return fixtures
}

// TestFilterOversizedJSONLRecordsBoundary pins the size boundary, the
// diagnostic, the retention of every other record, and the stand-in that keeps
// the omitted record's position.
func TestFilterOversizedJSONLRecordsBoundary(t *testing.T) {
	fixtures := loadOversizedRecordSizeFixtures(t)
	laterRecord := []byte(`{"type":"session.titled","data":{"title":"retained"}}`)

	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			recordSize := oversizedFilterTestLimit + fixture.SizeDelta
			input := append(bytes.Repeat([]byte{'x'}, recordSize), '\n')
			input = append(input, laterRecord...)
			input = append(input, '\n')
			sourcePath := fixture.Name + ".jsonl"

			filtered, diagnostics, err := filterOversizedJSONLRecords(
				t.Context(), input, sourcePath, oversizedFilterTestLimit,
			)
			if err != nil {
				t.Fatalf("filter refused a %d-byte record: %v; no record size may fail a session", recordSize, err)
			}

			if !fixture.Omitted {
				if len(diagnostics) != 0 {
					t.Fatalf("diagnostics = %+v, want none for a %d-byte record within the %d-byte limit", diagnostics, recordSize, oversizedFilterTestLimit)
				}
				if !bytes.Equal(filtered, input) {
					t.Fatalf("a record within the limit was rewritten; the filter must return the source bytes unchanged")
				}
				return
			}

			if len(diagnostics) != 1 || diagnostics[0].ErrorType != OversizedRecordDiagnosticType {
				t.Fatalf("diagnostics = %+v, want exactly one %q warning", diagnostics, OversizedRecordDiagnosticType)
			}
			diagnostic := diagnostics[0]
			if !strings.Contains(diagnostic.Location, sourcePath) || !strings.Contains(diagnostic.Location, "line 1") {
				t.Errorf("diagnostic location %q names neither the source nor the line", diagnostic.Location)
			}
			for _, want := range []string{
				"omitted one JSONL record",
				"every other record in the session was kept",
				"placeholder entry marks the position",
			} {
				if !strings.Contains(diagnostic.Message, want) {
					t.Errorf("diagnostic message %q does not say %q", diagnostic.Message, want)
				}
			}
			for _, want := range []string{"partial capture", "next harvest", "publishable"} {
				if !strings.Contains(diagnostic.Remediation, want) {
					t.Errorf("diagnostic remediation %q does not say %q", diagnostic.Remediation, want)
				}
			}
			// This warning is copied verbatim into the publication request, so
			// a sentence that sends the user away from sharing is shown to a
			// reader of the very session it says cannot be shared.
			for _, forbidden := range []string{
				"not available yet",
				"Publishing refuses",
				"before sharing this session",
			} {
				if strings.Contains(diagnostic.Remediation, forbidden) {
					t.Errorf("diagnostic remediation %q still says %q, which is not true of a session that may be published as it stands", diagnostic.Remediation, forbidden)
				}
			}

			if !bytes.Contains(filtered, laterRecord) {
				t.Fatal("the record after the omitted one was dropped")
			}
			if bytes.Contains(filtered, bytes.Repeat([]byte{'x'}, oversizedFilterTestLimit)) {
				t.Fatal("bytes of the omitted record survived into the filtered artifact")
			}
			if len(filtered) > len(laterRecord)+512 {
				t.Fatalf("filtered artifact is %d bytes; the omitted record must not be buffered into it", len(filtered))
			}

			// The stand-in must sit at the omitted record's own line, so the
			// placeholder entry lands at that position and not at the end.
			firstLine, _, _ := bytes.Cut(filtered, []byte{'\n'})
			at, isStandIn := parseOmittedRecordSentinel(firstLine)
			if !isStandIn {
				t.Fatalf("first filtered line %q is not the omission stand-in", firstLine)
			}
			if at.Record.Reason != OmittedRecordTooLarge || at.Record.Line != 1 {
				t.Errorf("stand-in records %+v, want reason %q at line 1", at.Record, OmittedRecordTooLarge)
			}
			if at.Record.Bytes != int64(recordSize) {
				t.Errorf("stand-in records %d bytes, want the record's true size %d", at.Record.Bytes, recordSize)
			}
			if at.Record.LimitBytes != oversizedFilterTestLimit {
				t.Errorf("stand-in records limit %d, want the injected limit %d", at.Record.LimitBytes, oversizedFilterTestLimit)
			}

			// The filtered artifact must still index, and the placeholder must
			// be the first entry, where the omitted record used to be.
			indexer := NewStrikeIndexer(nil)
			session := DiscoveredSession{Harness: HarnessStrike, SessionID: "01948bd4-1e5a-7c3d-9f21-6b7c8d9e0a1b"}
			result, err := indexer.IndexTranscriptBytesResult(t.Context(), session, filtered)
			if err != nil {
				t.Fatalf("filtered artifact refused by the indexer: %v", err)
			}
			entries := result.(indexformat.V1).Entries
			if len(entries) == 0 {
				t.Fatal("the filtered artifact produced no entries; the omission placeholder is missing")
			}
			assertOmissionPlaceholder(t, entries[0], at.Record)
		})
	}
}

// assertOmissionPlaceholder checks the shape every harness's placeholder must
// have: the typed record in Extra, the reader-facing note in ContentPreview,
// the omitted record's byte length, and no content of its own.
func assertOmissionPlaceholder(t *testing.T, entry schema.SessionEntry, want OmittedRecord) {
	t.Helper()
	if entry.Role != RoleTool {
		t.Errorf("placeholder role = %q, want %q", entry.Role, RoleTool)
	}
	if entry.EntryType != EntryTypeToolResult {
		t.Errorf("placeholder entry type = %q, want %q", entry.EntryType, EntryTypeToolResult)
	}
	if entry.Depth != 0 {
		t.Errorf("placeholder depth = %d, want 0", entry.Depth)
	}
	if entry.IsError {
		t.Error("placeholder is marked an error; an omission is not a failed tool call")
	}
	if entry.ToolOutput != nil {
		t.Errorf("placeholder carries tool output %q; no byte of the omitted record may be stored", *entry.ToolOutput)
	}
	if entry.RawByteLength == nil || int64(*entry.RawByteLength) != want.Bytes {
		t.Errorf("placeholder raw byte length = %v, want the omitted record's %d bytes", entry.RawByteLength, want.Bytes)
	}
	if entry.Extra == nil {
		t.Fatal("placeholder has no extra field; the typed omission record is the machine-readable half of this feature")
	}
	got, err := ParseOmittedRecord(*entry.Extra)
	if err != nil {
		t.Fatalf("placeholder extra %q is not a typed omission record: %v", *entry.Extra, err)
	}
	if got != want {
		t.Errorf("placeholder omission record = %+v, want %+v", got, want)
	}
	if entry.ContentPreview == nil {
		t.Fatal("placeholder has no content preview; a reader would see an empty turn with no explanation")
	}
	note := *entry.ContentPreview
	if len(note) >= 500 {
		t.Errorf("placeholder note is %d characters, over the wire's content preview bound", len(note))
	}
	for _, wantText := range []string{"only showing preview of tool output", "full output is over the", "limit"} {
		if !strings.Contains(note, wantText) {
			t.Errorf("placeholder note %q does not say %q", note, wantText)
		}
	}
	// The note speaks about ONE tool output. The line and the size of the
	// omitted record live in the typed record in the entry's extra field and in
	// the session's metadata diagnostic; repeating them here crowds out the one
	// thing a reader of the conversation needs at that position.
	for _, forbidden := range []string{"tool output omitted", "the rest of the session was kept"} {
		if strings.Contains(note, forbidden) {
			t.Errorf("placeholder note %q still says %q", note, forbidden)
		}
	}
}

// TestKeptRecordLimitLeavesTheRedactStageRoomForItsNewline pairs the two limits
// that decide whether the largest kept record survives ingestion. The filter
// keeps a record of exactly the limit and omits one byte more. The redact stage
// behind the filter reads that record as a LINE and refuses a line it cannot
// hold with its newline, so its limit must exceed the filter's by at least that
// byte; otherwise the session fails at the one size the filter promises to keep.
func TestKeptRecordLimitLeavesTheRedactStageRoomForItsNewline(t *testing.T) {
	t.Parallel()
	// The boundary of the filter's promise, at an injected limit so the case
	// costs no memory. The production limit is the same predicate.
	const probe = 4096
	if jsonlRecordTooLarge(make([]byte, probe), probe) {
		t.Fatalf("a record of exactly the %d-byte limit is omitted; a record up to the limit is read whole", probe)
	}
	if !jsonlRecordTooLarge(make([]byte, probe+1), probe) {
		t.Fatalf("a record one byte over the %d-byte limit is kept; a record over the limit is omitted", probe)
	}
	limit := productionJSONLRecordLimit(0)
	if limit != defaults.MaxJSONLRecordBytes {
		t.Fatalf("the production per-record limit is %d bytes, not the declared %d", limit, defaults.MaxJSONLRecordBytes)
	}
	if defaults.RedactScannerMaxLineBytes < limit+1 {
		t.Fatalf("the redact stage reads lines of at most %d bytes while records of up to %d bytes are kept; the largest kept record's line is %d bytes, so that record would fail the whole session",
			defaults.RedactScannerMaxLineBytes, limit, limit+1)
	}
}

// TestOmissionPlaceholderNoteIsTheReadersLine pins the EXACT sentence a reader is
// shown where a tool output was left out, at the per-record limit production
// runs with. Every viewer the placeholder reaches shows this string, and the
// publication body carries it to a reader on another machine, so the wording is
// pinned as a literal here rather than assembled from the code under test.
func TestOmissionPlaceholderNoteIsTheReadersLine(t *testing.T) {
	t.Parallel()
	limit := int64(defaults.MaxJSONLRecordBytes)
	record, err := NewOmittedRecord(OmittedRecordTooLarge, 7, limit+1, limit)
	if err != nil {
		t.Fatal(err)
	}
	const want = "only showing preview of tool output: full output is over the 256 MiB limit"
	if got := OmissionPlaceholderNote(record); got != want {
		t.Errorf("the reader's omission note = %q, want %q", got, want)
	}
}

// TestOmittedRecordRoundTrip pins the typed record's boundary constructors.
func TestOmittedRecordRoundTrip(t *testing.T) {
	record, err := NewOmittedRecord(OmittedRecordTooLarge, 4821, 327155712, 268435456)
	if err != nil {
		t.Fatalf("NewOmittedRecord refused a valid omission: %v", err)
	}
	extra, err := record.Extra()
	if err != nil {
		t.Fatalf("Extra: %v", err)
	}
	parsed, err := ParseOmittedRecord(extra)
	if err != nil {
		t.Fatalf("ParseOmittedRecord: %v", err)
	}
	if parsed != record {
		t.Fatalf("round trip = %+v, want %+v", parsed, record)
	}

	if _, err := ParseOmittedRecordReason("truncated"); err == nil {
		t.Error("an unknown omission reason was accepted; the reason set must stay closed")
	}
	if _, err := NewOmittedRecord(OmittedRecordTooLarge, 0, 10, 5); err == nil {
		t.Error("an omission with no line number was accepted")
	}
	if _, err := NewOmittedRecord(OmittedRecordTooLarge, 1, 5, 5); err == nil {
		t.Error("an omission was recorded for a record that fits the limit")
	}
	if _, err := ParseOmittedRecord(`{"other":1}`); err == nil {
		t.Error("an ordinary entry's extra field was read as an omission record")
	}
}

// TestFilterOversizedJSONLRecordsKeepsTheSourceEnding pins that the filter
// does not finish an unfinished last record. A transcript still being written
// has no final newline; adding one would make its last record look complete
// to every later reader, including the incomplete-tail check.
func TestFilterOversizedJSONLRecordsKeepsTheSourceEnding(t *testing.T) {
	over := append(bytes.Repeat([]byte{'x'}, oversizedFilterTestLimit+1), '\n')
	unfinished := append(over, []byte(`{"type":"session.titled","data":{"title":"unfinis`)...)

	filtered, diagnostics, err := filterOversizedJSONLRecords(t.Context(), unfinished, "unfinished.jsonl", oversizedFilterTestLimit)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one omission", diagnostics)
	}
	if len(filtered) == 0 || filtered[len(filtered)-1] == '\n' {
		t.Fatalf("the filter finished an unfinished last record: %q", filtered)
	}
	if !bytes.HasSuffix(filtered, []byte(`"unfinis`)) {
		t.Fatalf("the unfinished last record was changed: %q", filtered)
	}
}

// TestFilterOversizedJSONLRecordsKeepsAnOmissionItAlreadyCarries pins that
// filtering an artifact that already records an omission keeps that omission,
// at its position, instead of dropping the record of it.
func TestFilterOversizedJSONLRecordsKeepsAnOmissionItAlreadyCarries(t *testing.T) {
	over := append(bytes.Repeat([]byte{'x'}, oversizedFilterTestLimit+1), '\n')
	later := []byte(`{"type":"session.titled","data":{"title":"retained"}}` + "\n")
	once, firstDiagnostics, err := filterOversizedJSONLRecords(t.Context(), append(over, later...), "twice.jsonl", oversizedFilterTestLimit)
	if err != nil || len(firstDiagnostics) != 1 {
		t.Fatalf("first pass: %v / %+v", err, firstDiagnostics)
	}

	twice, secondDiagnostics, err := filterOversizedJSONLRecords(t.Context(), once, "twice.jsonl", oversizedFilterTestLimit)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(secondDiagnostics) != 1 || secondDiagnostics[0].ErrorType != OversizedRecordDiagnosticType {
		t.Fatalf("second pass diagnostics = %+v, want the omission reported again", secondDiagnostics)
	}
	if !bytes.Equal(twice, once) {
		t.Fatalf("a second pass changed the artifact:\n once = %q\ntwice = %q", once, twice)
	}
	firstLine, _, _ := bytes.Cut(twice, []byte{'\n'})
	if _, isStandIn := parseOmittedRecordSentinel(firstLine); !isStandIn {
		t.Fatalf("the second pass dropped the omission stand-in: %q", twice)
	}
}
