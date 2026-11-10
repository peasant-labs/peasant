package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/transcript_sparse_session.yaml
var transcriptSparseSessionYAML []byte

//go:embed testdata/transcript_segments.yaml
var transcriptSegmentsYAML []byte

//go:embed testdata/transcript_segments.manifest.yaml
var transcriptSegmentsManifestYAML []byte

//go:embed testdata/transcript_windows.yaml
var transcriptWindowsYAML []byte

//go:embed testdata/transcript_windows.manifest.yaml
var transcriptWindowsManifestYAML []byte

const transcriptTestSessionID = "77777777-7777-4777-8777-777777777777"

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type sparseTurnFixture struct {
	Index     int    `yaml:"index"`
	Role      string `yaml:"role"`
	ToolCalls int    `yaml:"toolCalls"`
}

type sparseSessionFixture struct {
	SessionID       string              `yaml:"session_id"`
	Revision        string              `yaml:"revision"`
	TotalTurns      int                 `yaml:"total_turns"`
	ToolCallCount   int                 `yaml:"tool_call_count"`
	GroupIdentities []int               `yaml:"group_identities"`
	Turns           []sparseTurnFixture `yaml:"turns"`
	Segments        []map[string]any    `yaml:"segments"`
	Window          map[string]any      `yaml:"window"`
}

type transcriptWindowExpectation struct {
	AnchorTurn int  `yaml:"anchorTurn"`
	FirstTurn  int  `yaml:"firstTurn"`
	LastTurn   int  `yaml:"lastTurn"`
	HasOlder   bool `yaml:"hasOlder"`
	HasNewer   bool `yaml:"hasNewer"`
}

type transcriptWindowCase struct {
	Name     string                      `yaml:"name"`
	Anchor   int                         `yaml:"anchor"`
	Span     int                         `yaml:"span"`
	Expected transcriptWindowExpectation `yaml:"expected"`
}

type transcriptWindowFixture struct {
	Cases []transcriptWindowCase `yaml:"cases"`
}

func loadSparseSessionFixture(t *testing.T) sparseSessionFixture {
	t.Helper()
	var fixture sparseSessionFixture
	decoder := yaml.NewDecoder(bytes.NewReader(transcriptSparseSessionYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode sparse transcript fixture: %v", err)
	}
	if fixture.TotalTurns != 55 || fixture.ToolCallCount != 91 {
		t.Fatalf("sparse transcript fixture declares %d turns and %d tool calls, want the measured 55 and 91", fixture.TotalTurns, fixture.ToolCallCount)
	}
	if len(fixture.Turns) != fixture.TotalTurns {
		t.Fatalf("sparse transcript fixture states %d turn rows for %d turns", len(fixture.Turns), fixture.TotalTurns)
	}
	toolCalls := 0
	for _, turn := range fixture.Turns {
		toolCalls += turn.ToolCalls
	}
	if toolCalls != fixture.ToolCallCount {
		t.Fatalf("sparse transcript fixture turn rows total %d tool calls, want %d", toolCalls, fixture.ToolCallCount)
	}
	for _, identity := range fixture.GroupIdentities {
		found := false
		for _, turn := range fixture.Turns {
			if turn.Index == identity {
				found = true
			}
		}
		if !found {
			t.Fatalf("sparse transcript fixture lost its group identity %d", identity)
		}
	}
	return fixture
}

func loadWindowFixture(t *testing.T) transcriptWindowFixture {
	t.Helper()
	var fixture transcriptWindowFixture
	decoder := yaml.NewDecoder(bytes.NewReader(transcriptWindowsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode transcript window fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(transcriptWindowsManifestYAML, "transcript windows")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, fixtureCase := range fixture.Cases {
		names[i] = fixtureCase.Name
		if fixtureCase.Name == "" || fixtureCase.Span < 1 || fixtureCase.Span > 10 {
			t.Fatalf("transcript window fixture case %d is incomplete", i)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "transcript windows"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func sparseTurns(t *testing.T, fixture sparseSessionFixture) []ingest.Turn {
	t.Helper()
	turns := make([]ingest.Turn, len(fixture.Turns))
	for i, spec := range fixture.Turns {
		role, err := transcriptRole(spec.Role)
		if err != nil {
			t.Fatal(err)
		}
		turn := ingest.Turn{Index: spec.Index, Role: role, Content: strings.Repeat("s", 64<<10)}
		for call := 0; call < spec.ToolCalls; call++ {
			turn.ToolCalls = append(turn.ToolCalls, ingest.ToolCall{
				ID:        fmt.Sprintf("sparse-%d-%d", spec.Index, call),
				Name:      "Bash",
				Arguments: `{"command":"echo"}`,
				Result:    "ok",
			})
		}
		turns[i] = turn
	}
	return turns
}

func transcriptRole(name string) (schema.Role, error) {
	switch name {
	case "user":
		return schema.RoleUser, nil
	case "assistant":
		return schema.RoleAssistant, nil
	case "system":
		return schema.RoleSystem, nil
	}
	return "", fmt.Errorf("transcript fixture names role %q; use user, assistant or system", name)
}

// ---------------------------------------------------------------------------
// Fake segment reader
// ---------------------------------------------------------------------------

type fakeSegmentReader struct {
	mu       sync.Mutex
	entries  []schema.SessionEntry
	revision string

	listCalls int
	readCalls int
	// shortAfter, when non-negative, caps the entries one segment read returns
	// so the short-segment 409 arm is reachable without corrupting a store.
	shortAfter int
}

func newFakeSegmentReader(entries []schema.SessionEntry) *fakeSegmentReader {
	return &fakeSegmentReader{entries: entries, revision: "4213", shortAfter: -1}
}

func (f *fakeSegmentReader) Revision(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revision, nil
}

func (f *fakeSegmentReader) ListEntries(context.Context, ingest.SessionID) ([]schema.SessionEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return append([]schema.SessionEntry(nil), f.entries...), nil
}

func (f *fakeSegmentReader) ReadSessionEntries(_ context.Context, _ ingest.SessionID, opts ingest.SessionEntryReadOptions) (ingest.SessionEntryReadPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls++
	selected := make([]schema.SessionEntry, 0, len(f.entries))
	for _, entry := range f.entries {
		if entry.EntryIndex < opts.FromIndex {
			continue
		}
		selected = append(selected, entry)
	}
	if f.shortAfter >= 0 && len(selected) > f.shortAfter {
		selected = selected[:f.shortAfter]
	}
	var page ingest.SessionEntryReadPage
	page.FromIndex = opts.FromIndex
	var bytes int64
	for _, entry := range selected {
		size := transcriptFakeEntryBytes(entry)
		if len(page.Entries) > 0 && bytes+size > opts.SoftMaxBytes {
			next := entry.EntryIndex
			page.NextIndex = &next
			break
		}
		page.Entries = append(page.Entries, entry)
		bytes += size
	}
	return page, nil
}

func (f *fakeSegmentReader) counts() (lists, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.readCalls
}

func (f *fakeSegmentReader) setShortAfter(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shortAfter = n
}

func transcriptFakeEntryBytes(entry schema.SessionEntry) int64 {
	var size int64
	if entry.ContentPreview != nil {
		size += int64(len(*entry.ContentPreview))
	}
	if entry.ToolInput != nil {
		size += int64(len(*entry.ToolInput))
	}
	if entry.ToolOutput != nil {
		size += int64(len(*entry.ToolOutput))
	}
	return size
}

// transcriptEntries builds one full-content entry per turn. Tool calls are
// depth-1 tool_use children placed between this turn's identity and the next,
// so the fixture's spacing must leave room for them.
func transcriptEntries(t *testing.T, turns []testTurnSpec) []schema.SessionEntry {
	t.Helper()
	entries := make([]schema.SessionEntry, 0, len(turns)*2)
	for _, turn := range turns {
		role, err := transcriptRole(turn.Role)
		if err != nil {
			t.Fatal(err)
		}
		content := strings.Repeat("t", turn.ContentBytes)
		index := turn.Index
		timestamp := int64(1_700_000_000_000 + index)
		entries = append(entries, schema.SessionEntry{
			SessionID:      ingest.SessionID(transcriptTestSessionID),
			EntryIndex:     index,
			Harness:        schema.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           role,
			TimestampMs:    &timestamp,
			ContentPreview: &content,
			Depth:          0,
		})
		for call := 0; call < turn.ToolCalls; call++ {
			id := fmt.Sprintf("tool-%d-%d", index, call)
			name := "Bash"
			input := `{"command":"echo"}`
			output := "ok"
			child := index + 1 + call
			entries = append(entries, schema.SessionEntry{
				SessionID:    ingest.SessionID(transcriptTestSessionID),
				EntryIndex:   child,
				Harness:      schema.HarnessClaudeCode,
				EntryType:    schema.EntryTypeToolUse,
				Role:         role,
				TimestampMs:  &timestamp,
				ToolCallID:   &id,
				ToolNamesCSV: &name,
				ToolInput:    &input,
				ToolOutput:   &output,
				Depth:        1,
				ParentIndex:  &index,
			})
		}
	}
	return entries
}

type testTurnSpec struct {
	Index        int
	Role         string
	ContentBytes int
	ToolCalls    int
}

// alternatingTurns builds spacing-4 turns so two tool children fit between
// consecutive identities.
func alternatingTurns(count, contentBytes, toolCalls int) []testTurnSpec {
	turns := make([]testTurnSpec, count)
	for i := 0; i < count; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		turns[i] = testTurnSpec{Index: i * 4, Role: role, ContentBytes: contentBytes, ToolCalls: toolCalls}
	}
	return turns
}

func newTestServerForSegments(t *testing.T, reader SegmentReader) *Server {
	t.Helper()
	return NewServer(ServerConfig{Port: 0, SegmentReader: reader})
}

// ---------------------------------------------------------------------------
// Directory
// ---------------------------------------------------------------------------

func TestTranscriptDirectorySparseSession(t *testing.T) {
	t.Parallel()
	fixture := loadSparseSessionFixture(t)
	turns := sparseTurns(t, fixture)
	refs, total := transcript.CutSegments(turns, defaults.TranscriptSegmentBudgetBytes)
	if total != 55 {
		t.Fatalf("cut counted %d turns, want 55", total)
	}
	if len(refs) < 2 {
		t.Fatalf("the sparse session cut to %d segment(s), want more than one so a window can cross a boundary", len(refs))
	}
	for _, ref := range refs {
		if ref.LastTurn < ref.FirstTurn {
			t.Fatalf("segment %d declares an inverted range [%d, %d]", ref.Index, ref.FirstTurn, ref.LastTurn)
		}
	}
	// Every user turn is whole: no user identity straddles a boundary.
	for _, turn := range turns {
		if turn.Role != schema.RoleUser {
			continue
		}
		for _, ref := range refs {
			if ref.FirstTurn <= turn.Index && turn.Index <= ref.LastTurn {
				break
			}
		}
	}
	payload := transcriptDirectoryPayload(ingest.SessionID(transcriptTestSessionID), segmentDirectory{
		revision: fixture.Revision, totalTurns: total, refs: refs,
	}, 1)
	if err := payload.Validate(); err != nil {
		t.Fatalf("directory does not validate: %v", err)
	}
	if payload.TotalTurns != 55 {
		t.Fatalf("directory totalTurns = %d, want 55", payload.TotalTurns)
	}
}

func TestTranscriptDirectoryTenThousandTurns(t *testing.T) {
	t.Parallel()
	entries := transcriptEntries(t, alternatingTurns(10_000, 8, 0))
	reader := newFakeSegmentReader(entries)
	producer := newSegmentProducer(reader)
	directory, err := producer.directoryFor(context.Background(), ingest.SessionID(transcriptTestSessionID), "4213")
	if err != nil {
		t.Fatal(err)
	}
	if directory.totalTurns != 10_000 {
		t.Fatalf("totalTurns = %d, want 10000", directory.totalTurns)
	}
	payload := transcriptDirectoryPayload(ingest.SessionID(transcriptTestSessionID), directory, 1)
	if err := payload.Validate(); err != nil {
		t.Fatalf("10,000-turn directory does not validate: %v", err)
	}
}

// TestTranscriptDirectoryPagingCap drives the 10,000-ref cap and the last-page
// clamp from synthetic refs: the contract caps the read's rows, not the
// session's size, so no transcript bytes are needed to exercise it.
func TestTranscriptDirectoryPagingCap(t *testing.T) {
	t.Parallel()
	const total = 12_000
	refs := make([]schema.TranscriptSegmentRef, total)
	for i := range refs {
		refs[i] = schema.TranscriptSegmentRef{Index: i, FirstTurn: i * 2, LastTurn: i*2 + 1, ByteLength: 10}
	}
	directory := segmentDirectory{revision: "4213", totalTurns: total * 2, refs: refs}

	first := transcriptDirectoryPayload(ingest.SessionID(transcriptTestSessionID), directory, 1)
	if first.DirectoryPage == nil || *first.DirectoryPage != 1 || first.DirectoryPerPage == nil || *first.DirectoryPerPage != schema.MaxTranscriptDirectoryRefs {
		t.Fatalf("page 1 fields = %v/%v, want 1/%d", first.DirectoryPage, first.DirectoryPerPage, schema.MaxTranscriptDirectoryRefs)
	}
	if len(first.Segments) != schema.MaxTranscriptDirectoryRefs || first.TotalSegments != total {
		t.Fatalf("page 1 holds %d refs of %d total, want 10000 of %d", len(first.Segments), first.TotalSegments, total)
	}
	if first.Segments[0].Index != 0 || first.Segments[len(first.Segments)-1].Index != schema.MaxTranscriptDirectoryRefs-1 {
		t.Fatalf("page 1 refs span %d..%d, want 0..%d", first.Segments[0].Index, first.Segments[len(first.Segments)-1].Index, schema.MaxTranscriptDirectoryRefs-1)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("page 1 does not validate: %v", err)
	}

	second := transcriptDirectoryPayload(ingest.SessionID(transcriptTestSessionID), directory, 2)
	if second.DirectoryPage == nil || *second.DirectoryPage != 2 || len(second.Segments) != total-schema.MaxTranscriptDirectoryRefs {
		t.Fatalf("page 2 holds %d refs at page %v, want %d", len(second.Segments), second.DirectoryPage, total-schema.MaxTranscriptDirectoryRefs)
	}
	if second.Segments[0].Index != schema.MaxTranscriptDirectoryRefs {
		t.Fatalf("page 2 starts at segment %d, want %d", second.Segments[0].Index, schema.MaxTranscriptDirectoryRefs)
	}
	if err := second.Validate(); err != nil {
		t.Fatalf("page 2 does not validate: %v", err)
	}

	past := transcriptDirectoryPayload(ingest.SessionID(transcriptTestSessionID), directory, 99)
	if past.DirectoryPage == nil || *past.DirectoryPage != 2 {
		t.Fatalf("a page past the end = %v, want the last page 2", past.DirectoryPage)
	}
	if err := past.Validate(); err != nil {
		t.Fatalf("clamped page does not validate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Segment reads
// ---------------------------------------------------------------------------

type segmentTurnFixture struct {
	Index              int    `yaml:"index"`
	Role               string `yaml:"role"`
	ContentBytes       int    `yaml:"contentBytes"`
	ToolArgumentsBytes []int  `yaml:"toolArgumentsBytes,omitempty"`
	ToolResultBytes    []int  `yaml:"toolResultBytes,omitempty"`
}

type segmentCaseExpectation struct {
	TotalTurns        int  `yaml:"totalTurns,omitempty"`
	TotalSegments     int  `yaml:"totalSegments,omitempty"`
	Turns             int  `yaml:"turns,omitempty"`
	Truncated         bool `yaml:"truncated,omitempty"`
	TurnByteLength    int  `yaml:"turnByteLength,omitempty"`
	SegmentByteLength int  `yaml:"segmentByteLength,omitempty"`
	Status            int  `yaml:"status,omitempty"`
}

type segmentReadCase struct {
	Name       string                 `yaml:"name"`
	Turns      []segmentTurnFixture   `yaml:"turns"`
	ShortAfter *int                   `yaml:"shortAfter,omitempty"`
	Expected   segmentCaseExpectation `yaml:"expected"`
}

type segmentDirectoryCase struct {
	Name     string                 `yaml:"name"`
	Turns    []segmentTurnFixture   `yaml:"turns"`
	Expected segmentCaseExpectation `yaml:"expected"`
}

type segmentReadFixture struct {
	DirectoryCases []segmentDirectoryCase `yaml:"directory_cases"`
	SegmentCases   []segmentReadCase      `yaml:"segment_cases"`
}

func loadSegmentReadFixture(t *testing.T) segmentReadFixture {
	t.Helper()
	var fixture segmentReadFixture
	decoder := yaml.NewDecoder(bytes.NewReader(transcriptSegmentsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode transcript segment fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(transcriptSegmentsManifestYAML, "transcript segments")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.DirectoryCases)+len(fixture.SegmentCases))
	for _, fixtureCase := range fixture.DirectoryCases {
		names = append(names, fixtureCase.Name)
	}
	for _, fixtureCase := range fixture.SegmentCases {
		names = append(names, fixtureCase.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "transcript segments"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func entriesFromSegmentFixture(t *testing.T, turns []segmentTurnFixture) []schema.SessionEntry {
	t.Helper()
	entries := make([]schema.SessionEntry, 0, len(turns))
	for _, turn := range turns {
		role, err := transcriptRole(turn.Role)
		if err != nil {
			t.Fatal(err)
		}
		content := strings.Repeat("t", turn.ContentBytes)
		index := turn.Index
		timestamp := int64(1_700_000_000_000 + index)
		entries = append(entries, schema.SessionEntry{
			SessionID:      ingest.SessionID(transcriptTestSessionID),
			EntryIndex:     index,
			Harness:        schema.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           role,
			TimestampMs:    &timestamp,
			ContentPreview: &content,
			Depth:          0,
		})
		if len(turn.ToolArgumentsBytes) != len(turn.ToolResultBytes) {
			t.Fatalf("segment fixture turn %d states %d tool arguments and %d results", index, len(turn.ToolArgumentsBytes), len(turn.ToolResultBytes))
		}
		for call := range turn.ToolArgumentsBytes {
			id := fmt.Sprintf("tool-%d-%d", index, call)
			name := "Bash"
			input := strings.Repeat("a", turn.ToolArgumentsBytes[call])
			output := strings.Repeat("r", turn.ToolResultBytes[call])
			child := index + 1 + call
			entries = append(entries, schema.SessionEntry{
				SessionID:    ingest.SessionID(transcriptTestSessionID),
				EntryIndex:   child,
				Harness:      schema.HarnessClaudeCode,
				EntryType:    schema.EntryTypeToolUse,
				Role:         role,
				TimestampMs:  &timestamp,
				ToolCallID:   &id,
				ToolNamesCSV: &name,
				ToolInput:    &input,
				ToolOutput:   &output,
				Depth:        1,
				ParentIndex:  &index,
			})
		}
	}
	return entries
}

func getTranscriptSegment(t *testing.T, server *Server, index string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+transcriptTestSessionID+"/transcript/segments/"+index, nil)
	request.SetPathValue("id", transcriptTestSessionID)
	request.SetPathValue("index", index)
	recorder := httptest.NewRecorder()
	server.handleTranscriptSegment(recorder, request)
	return recorder, recorder.Body.Bytes()
}

func TestTranscriptDirectoryFixtureCases(t *testing.T) {
	t.Parallel()
	fixture := loadSegmentReadFixture(t)
	for _, fixtureCase := range fixture.DirectoryCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			entries := entriesFromSegmentFixture(t, fixtureCase.Turns)
			server := newTestServerForSegments(t, newFakeSegmentReader(entries))
			request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+transcriptTestSessionID+"/transcript/segments", nil)
			request.SetPathValue("id", transcriptTestSessionID)
			recorder := httptest.NewRecorder()
			server.handleTranscriptSegments(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("directory status = %d, body %s", recorder.Code, recorder.Body.String())
			}
			var directory schema.TranscriptSegmentDirectory
			if err := json.Unmarshal(recorder.Body.Bytes(), &directory); err != nil {
				t.Fatalf("decode directory: %v", err)
			}
			if directory.TotalTurns != fixtureCase.Expected.TotalTurns {
				t.Fatalf("totalTurns = %d, want %d", directory.TotalTurns, fixtureCase.Expected.TotalTurns)
			}
			if directory.TotalSegments != fixtureCase.Expected.TotalSegments {
				t.Fatalf("totalSegments = %d, want %d", directory.TotalSegments, fixtureCase.Expected.TotalSegments)
			}
			if directory.Segments == nil {
				t.Fatal("an empty session must answer with a non-null segments array")
			}
		})
	}
}

func TestTranscriptSegmentFixtureCases(t *testing.T) {
	t.Parallel()
	fixture := loadSegmentReadFixture(t)
	for _, fixtureCase := range fixture.SegmentCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			entries := entriesFromSegmentFixture(t, fixtureCase.Turns)
			reader := newFakeSegmentReader(entries)
			server := newTestServerForSegments(t, reader)

			if fixtureCase.ShortAfter != nil {
				// Build the directory first, then shorten the store read so the
				// cached directory still declares the original range.
				request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+transcriptTestSessionID+"/transcript/segments", nil)
				request.SetPathValue("id", transcriptTestSessionID)
				recorder := httptest.NewRecorder()
				server.handleTranscriptSegments(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("directory status = %d, body %s", recorder.Code, recorder.Body.String())
				}
				reader.setShortAfter(*fixtureCase.ShortAfter)
			}

			recorder, body := getTranscriptSegment(t, server, "0")
			if fixtureCase.Expected.Status != 0 {
				if recorder.Code != fixtureCase.Expected.Status {
					t.Fatalf("segment status = %d, want %d; body %s", recorder.Code, fixtureCase.Expected.Status, body)
				}
				text := string(body)
				for _, want := range []string{transcriptTestSessionID, "segment 0", "declared turns", "revision 4213", "peasant harvest --force --session " + transcriptTestSessionID} {
					if !strings.Contains(text, want) {
						t.Fatalf("conflict body does not name %q: %s", want, text)
					}
				}
				return
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("segment status = %d, body %s", recorder.Code, body)
			}
			var segment schema.TranscriptSegment
			if err := json.Unmarshal(body, &segment); err != nil {
				t.Fatalf("decode segment: %v", err)
			}
			if len(segment.Turns) != fixtureCase.Expected.Turns {
				t.Fatalf("segment holds %d turns, want %d", len(segment.Turns), fixtureCase.Expected.Turns)
			}
			if segment.ByteLength != fixtureCase.Expected.SegmentByteLength {
				t.Fatalf("segment byteLength = %d, want %d", segment.ByteLength, fixtureCase.Expected.SegmentByteLength)
			}
			for _, turn := range segment.Turns {
				if turn.Truncated != fixtureCase.Expected.Truncated {
					t.Fatalf("turn %d truncated = %v, want %v", turn.Index, turn.Truncated, fixtureCase.Expected.Truncated)
				}
				if turn.ByteLength != fixtureCase.Expected.TurnByteLength {
					t.Fatalf("turn %d byteLength = %d, want %d", turn.Index, turn.ByteLength, fixtureCase.Expected.TurnByteLength)
				}
			}
		})
	}
}

// Window descriptors
// ---------------------------------------------------------------------------

func TestTranscriptWindowDescriptors(t *testing.T) {
	t.Parallel()
	sparse := loadSparseSessionFixture(t)
	turns := sparseTurns(t, sparse)
	refs, _ := transcript.CutSegments(turns, defaults.TranscriptSegmentBudgetBytes)
	fixture := loadWindowFixture(t)

	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			window, err := buildTranscriptWindow(schema.SessionID(transcriptTestSessionID), sparse.Revision, fixtureCase.Anchor, fixtureCase.Span, turns, refs)
			if err != nil {
				t.Fatalf("build window: %v", err)
			}
			if window.AnchorTurn != fixtureCase.Expected.AnchorTurn ||
				window.FirstTurn != fixtureCase.Expected.FirstTurn ||
				window.LastTurn != fixtureCase.Expected.LastTurn ||
				window.HasOlder != fixtureCase.Expected.HasOlder ||
				window.HasNewer != fixtureCase.Expected.HasNewer {
				t.Fatalf("window = anchor %d range [%d, %d] older=%v newer=%v, want anchor %d range [%d, %d] older=%v newer=%v",
					window.AnchorTurn, window.FirstTurn, window.LastTurn, window.HasOlder, window.HasNewer,
					fixtureCase.Expected.AnchorTurn, fixtureCase.Expected.FirstTurn, fixtureCase.Expected.LastTurn,
					fixtureCase.Expected.HasOlder, fixtureCase.Expected.HasNewer)
			}
			if window.RequestedAnchor != fixtureCase.Anchor {
				t.Fatalf("requestedAnchor = %d, want %d", window.RequestedAnchor, fixtureCase.Anchor)
			}
			if len(window.Segments) == 0 {
				t.Fatal("window names no covering segments")
			}
			if fixtureCase.Name == "window-ten-assistants-each-side" && len(window.Segments) < 2 {
				t.Fatalf("the wide window is covered by %d segment(s); this case must cross a segment boundary", len(window.Segments))
			}
			for i, segment := range window.Segments {
				if i > 0 && segment != window.Segments[i-1]+1 {
					t.Fatalf("window segments are not contiguous: %v", window.Segments)
				}
			}
			if err := window.Validate(); err != nil {
				t.Fatalf("window does not validate: %v", err)
			}
			if err := schema.ValidateTranscriptWindowCoverage(window, refs); err != nil {
				t.Fatalf("window coverage: %v", err)
			}

			// The descriptor carries no content: no turns, content or toolCalls
			// key anywhere in its body.
			encoded, err := json.Marshal(window)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{`"turns"`, `"content"`, `"toolCalls"`} {
				if bytes.Contains(encoded, []byte(forbidden)) {
					t.Fatalf("window body carries %s: %s", forbidden, encoded)
				}
			}
		})
	}
}

func TestTranscriptWindowHasNoUserTurn(t *testing.T) {
	t.Parallel()
	turns := []ingest.Turn{
		{Index: 4, Role: schema.RoleAssistant, Content: "a"},
		{Index: 7, Role: schema.RoleSystem, Content: "s"},
	}
	refs, _ := transcript.CutSegments(turns, defaults.TranscriptSegmentBudgetBytes)
	_, err := buildTranscriptWindow(schema.SessionID(transcriptTestSessionID), "4213", 4, 10, turns, refs)
	var missing *noUserTurnError
	if !errors.As(err, &missing) {
		t.Fatalf("window error = %v, want noUserTurnError", err)
	}
}

// ---------------------------------------------------------------------------
// Producer: cache, single-flight, cold build, jump
// ---------------------------------------------------------------------------

func TestTranscriptProducerSingleFlight(t *testing.T) {
	t.Parallel()
	entries := transcriptEntries(t, alternatingTurns(64, 4096, 0))
	reader := newFakeSegmentReader(entries)
	producer := newSegmentProducer(reader)

	const readers = 8
	var wg sync.WaitGroup
	wg.Add(readers)
	results := make([]segmentDirectory, readers)
	for i := 0; i < readers; i++ {
		go func(i int) {
			defer wg.Done()
			directory, err := producer.directoryFor(context.Background(), ingest.SessionID(transcriptTestSessionID), "4213")
			if err != nil {
				t.Errorf("concurrent directory read: %v", err)
				return
			}
			results[i] = directory
		}(i)
	}
	wg.Wait()
	lists, _ := reader.counts()
	if lists != 1 {
		t.Fatalf("concurrent reads caused %d ListEntries passes, want 1", lists)
	}
	if got := producer.buildCount(); got != 1 {
		t.Fatalf("producer completed %d builds, want 1", got)
	}
	for i := 1; i < readers; i++ {
		if len(results[i].refs) != len(results[0].refs) {
			t.Fatalf("concurrent readers saw different directories: %d and %d refs", len(results[0].refs), len(results[i].refs))
		}
	}
}

// TestTranscriptProducerColdBuild32MiB measures one cold directory build on the
// synthetic 32 MiB fixture (1,312 turns, no real transcript text) against the
// slice's recorded budget.
func TestTranscriptProducerColdBuild32MiB(t *testing.T) {
	t.Parallel()
	const turns = 1312
	const contentBytes = 24 << 10 // ~32 MiB across the run
	entries := transcriptEntries(t, alternatingTurns(turns, contentBytes, 0))
	reader := newFakeSegmentReader(entries)
	producer := newSegmentProducer(reader)

	started := time.Now()
	directory, err := producer.directoryFor(context.Background(), ingest.SessionID(transcriptTestSessionID), "4213")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if directory.totalTurns != turns {
		t.Fatalf("totalTurns = %d, want %d", directory.totalTurns, turns)
	}
	if len(directory.refs) < 24 || len(directory.refs) > 40 {
		t.Fatalf("32 MiB directory cut to %d segments, want the recorded 24..40 band", len(directory.refs))
	}
	for _, ref := range directory.refs {
		if ref.ByteLength > defaults.TranscriptSegmentBudgetBytes {
			t.Fatalf("segment %d is %d bytes, over the %d budget", ref.Index, ref.ByteLength, defaults.TranscriptSegmentBudgetBytes)
		}
	}
	t.Logf("cold segment directory build: %s (budget %s)", elapsed, defaults.TranscriptSegmentColdBuildBudget)
	if elapsed > defaults.TranscriptSegmentColdBuildBudget {
		t.Fatalf("cold build took %s, over the %s budget", elapsed, defaults.TranscriptSegmentColdBuildBudget)
	}
}

// TestTranscriptJumpLoadsOnlyCoveringSegments drives the design case: a jump
// into the middle of the 32 MiB fixture resolves a descriptor and reads only
// the covering segments.
func TestTranscriptJumpLoadsOnlyCoveringSegments(t *testing.T) {
	t.Parallel()
	const turns = 1312
	const contentBytes = 24 << 10
	entries := transcriptEntries(t, alternatingTurns(turns, contentBytes, 0))
	reader := newFakeSegmentReader(entries)
	server := newTestServerForSegments(t, reader)

	directory, err := server.segments.directoryFor(context.Background(), ingest.SessionID(transcriptTestSessionID), "4213")
	if err != nil {
		t.Fatal(err)
	}
	if len(directory.refs) < 6 {
		t.Fatalf("fixture cut to %d segments, need a middle segment", len(directory.refs))
	}
	mid := directory.refs[4]
	anchor := mid.FirstTurn
	if anchor%2 != 0 {
		anchor++ // alternatingTurns puts users on even identities
	}
	allTurns, err := server.segments.turns(context.Background(), ingest.SessionID(transcriptTestSessionID), "4213")
	if err != nil {
		t.Fatal(err)
	}
	window, err := buildTranscriptWindow(schema.SessionID(transcriptTestSessionID), "4213", anchor, 10, allTurns, directory.refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Segments) >= len(directory.refs) {
		t.Fatalf("window names %d of %d segments; a jump must not load the session", len(window.Segments), len(directory.refs))
	}

	_, before := reader.counts()
	for _, index := range window.Segments {
		if index >= len(directory.refs) {
			t.Fatalf("window names segment %d outside the %d-segment directory", index, len(directory.refs))
		}
		ref := directory.refs[index]
		page, readErr := reader.ReadSessionEntries(context.Background(), ingest.SessionID(transcriptTestSessionID), ingest.SessionEntryReadOptions{
			Mode:         ingest.SessionEntryReadAvailable,
			FromIndex:    ref.FirstTurn,
			Limit:        defaults.TranscriptSegmentEntryReadLimit,
			SoftMaxBytes: defaults.TranscriptContinuationReadBytes,
		})
		if readErr != nil {
			t.Fatal(readErr)
		}
		readTurns := transcript.EntriesToTurns(page.Entries)
		if len(readTurns) == 0 || readTurns[0].Index != ref.FirstTurn {
			t.Fatalf("segment %d read from turn %d did not start there", index, ref.FirstTurn)
		}
	}
	_, after := reader.counts()
	if after-before != len(window.Segments) {
		t.Fatalf("jump caused %d segment reads, want its %d covering segments", after-before, len(window.Segments))
	}
}

// ---------------------------------------------------------------------------
// HTTP surface
// ---------------------------------------------------------------------------

func TestTranscriptRoutesServeDirectoryAndRevisionConflict(t *testing.T) {
	t.Parallel()
	entries := transcriptEntries(t, alternatingTurns(4, 64, 0))
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServer(ServerConfig{Port: 0, SegmentReader: newFakeSegmentReader(entries)})
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	base := "http://" + srv.Addr().String()

	response, err := http.Get(base + "/api/v1/sessions/" + transcriptTestSessionID + "/transcript/segments")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("directory status = %d, body %s", response.StatusCode, body)
	}
	var directory schema.TranscriptSegmentDirectory
	if err := json.Unmarshal(body, &directory); err != nil {
		t.Fatalf("decode directory: %v", err)
	}
	if directory.Revision != "4213" || directory.TotalTurns != 4 {
		t.Fatalf("directory = revision %q turns %d, want 4213 and 4", directory.Revision, directory.TotalTurns)
	}

	response, err = http.Get(base + "/api/v1/sessions/" + transcriptTestSessionID + "/transcript/segments?revision=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale revision status = %d, want 409; body %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "revision 1") || !strings.Contains(string(body), "revision 4213") {
		t.Fatalf("stale revision body does not name both revisions: %s", body)
	}

	response, err = http.Get(base + "/api/v1/sessions/" + transcriptTestSessionID + "/transcript/segments/0")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("segment status = %d, body %s", response.StatusCode, body)
	}

	response, err = http.Get(base + "/api/v1/sessions/" + transcriptTestSessionID + "/transcript/segments/0?revision=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale segment revision status = %d, want 409; body %s", response.StatusCode, body)
	}

	response, err = http.Get(base + "/api/v1/sessions/" + transcriptTestSessionID + "/transcript/window?anchor=0&span=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("window status = %d, body %s", response.StatusCode, body)
	}
	var windowBody map[string]json.RawMessage
	if err := json.Unmarshal(body, &windowBody); err != nil {
		t.Fatalf("decode window: %v", err)
	}
	for _, forbidden := range []string{"turns", "content", "toolCalls"} {
		if _, ok := windowBody[forbidden]; ok {
			t.Fatalf("window body carries %q: %s", forbidden, body)
		}
	}
	if _, ok := windowBody["segments"]; !ok {
		t.Fatalf("window body names no covering segments: %s", body)
	}

	response, err = http.Get(base + "/api/v1/sessions/" + transcriptTestSessionID + "/transcript/window?anchor=0&span=1&revision=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale window revision status = %d, want 409; body %s", response.StatusCode, body)
	}

	response, err = http.Get(base + "/api/v1/sessions/" + transcriptTestSessionID + "/transcript/window?anchor=-1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad anchor status = %d, want 400; body %s", response.StatusCode, body)
	}
	response, err = http.Get(base + "/api/v1/sessions/" + transcriptTestSessionID + "/transcript/window?anchor=0&span=11")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("span 11 status = %d, want 400; body %s", response.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// Metadata-only header projection
// ---------------------------------------------------------------------------

// headerStubProvider serves one canned local read payload through the
// production provider contract.
type headerStubProvider struct {
	DataProvider
	payload *schema.SessionDetailReadPayload
}

func (p headerStubProvider) DetailReadPayload(context.Context, string) (*schema.SessionDetailReadPayload, error) {
	return p.payload, nil
}

func TestSessionDetailHeaderProjectionIsMetadataOnly(t *testing.T) {
	t.Parallel()
	read := &schema.SessionDetailReadPayload{
		SessionDetailPayload: schema.SessionDetailPayload{
			ID:        transcriptTestSessionID,
			Harness:   schema.HarnessClaudeCode,
			TurnCount: 2,
			Turns: []schema.TurnDetail{
				{Index: 4, Role: schema.RoleUser, Content: "keep this out of the header"},
				{Index: 7, Role: schema.RoleAssistant, Content: "and this too"},
			},
			EarlierHistory: []schema.EarlierHistorySection{{
				State: schema.EarlierHistoryUncertainMigrated,
				Turns: []schema.TurnDetail{{Index: 1, Role: schema.RoleUser, Content: "retained history turn"}},
			}},
		},
		Revision:               "4213",
		RelationshipNavigation: []schema.SessionRelationshipNavigation{{Kind: schema.SessionRelationshipStartedBy, Status: schema.RelationshipNavigationUnknown}},
	}
	header := SessionDetailHeaderPayloadFromRead(read)
	if err := header.Validate(); err != nil {
		t.Fatalf("header does not validate: %v", err)
	}
	if header.ID != transcriptTestSessionID || header.TurnCount != 2 {
		t.Fatalf("header lost metadata: id=%q turnCount=%d", header.ID, header.TurnCount)
	}
	if len(header.EarlierHistory) != 1 || header.EarlierHistory[0] != schema.EarlierHistoryUncertainMigrated {
		t.Fatalf("header earlier-history states = %v, want the retained section state", header.EarlierHistory)
	}
	if len(header.RelationshipNavigation) != 1 {
		t.Fatalf("header navigation = %v, want the read's authorized navigation", header.RelationshipNavigation)
	}

	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"turns"`, `"content"`, `"nativeMetadata"`} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("metadata-only header carries %s: %s", forbidden, encoded)
		}
	}

	// The durable read still carries the turns and earlier-history content it
	// had before the projection, so push and export are untouched.
	if len(read.Turns) != 2 || len(read.EarlierHistory[0].Turns) != 1 {
		t.Fatalf("durable read was mutated by the header projection: %+v", read)
	}
}

func TestSessionDetailHeaderForProvider(t *testing.T) {
	t.Parallel()
	read := &schema.SessionDetailReadPayload{SessionDetailPayload: schema.SessionDetailPayload{
		ID:        transcriptTestSessionID,
		Harness:   schema.HarnessCodex,
		TurnCount: 1,
		Turns:     []schema.TurnDetail{{Index: 1, Role: schema.RoleUser, Content: "hello"}},
	}}
	header, err := SessionDetailHeaderForProvider(context.Background(), headerStubProvider{payload: read}, transcriptTestSessionID)
	if err != nil {
		t.Fatalf("header for provider: %v", err)
	}
	if header.ID != transcriptTestSessionID || header.Harness != schema.HarnessCodex {
		t.Fatalf("header = %+v, want the read's metadata", header)
	}
}

func TestTranscriptProducerRevisionKeyedLRU(t *testing.T) {
	t.Parallel()
	entries := transcriptEntries(t, alternatingTurns(4, 64, 0))
	reader := newFakeSegmentReader(entries)
	producer := newSegmentProducer(reader)
	producer.capacity = 2

	ctx := context.Background()
	sid := ingest.SessionID(transcriptTestSessionID)
	for _, revision := range []string{"1", "2", "3"} {
		if _, err := producer.directoryFor(ctx, sid, revision); err != nil {
			t.Fatal(err)
		}
	}
	lists, _ := reader.counts()
	if lists != 3 {
		t.Fatalf("three revisions caused %d builds, want 3", lists)
	}
	if _, err := producer.directoryFor(ctx, sid, "3"); err != nil {
		t.Fatal(err)
	}
	lists, _ = reader.counts()
	if lists != 3 {
		t.Fatalf("a repeated revision rebuilt the directory: %d builds", lists)
	}
	if _, err := producer.directoryFor(ctx, sid, "1"); err != nil {
		t.Fatal(err)
	}
	lists, _ = reader.counts()
	if lists != 4 {
		t.Fatalf("the evicted oldest revision was not rebuilt: %d builds", lists)
	}
	if producer.capacity != 2 {
		t.Fatalf("capacity changed to %d", producer.capacity)
	}
}
