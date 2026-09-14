package transcript

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/snapshot_detail_hydration.yaml
var snapshotDetailHydrationFixtureYAML []byte

//go:embed testdata/snapshot_detail_hydration.manifest.yaml
var snapshotDetailHydrationManifestYAML []byte

type snapshotHydrationEntry struct {
	Index         int    `yaml:"index"`
	Role          string `yaml:"role"`
	Depth         int    `yaml:"depth"`
	ParentIndex   *int   `yaml:"parentIndex,omitempty"`
	EntryType     string `yaml:"entryType,omitempty"`
	Ref           string `yaml:"ref,omitempty"`
	Submission    string `yaml:"submission,omitempty"`
	Origin        string `yaml:"origin,omitempty"`
	Actor         string `yaml:"actor,omitempty"`
	Delivery      string `yaml:"delivery,omitempty"`
	Ownership     string `yaml:"ownership,omitempty"`
	Evidence      string `yaml:"evidence,omitempty"`
	Modality      string `yaml:"modality,omitempty"`
	Content       string `yaml:"content,omitempty"`
	ContentSeed   string `yaml:"contentSeed,omitempty"`
	ContentRepeat int    `yaml:"contentRepeat,omitempty"`
	ToolCallID    string `yaml:"toolCallId,omitempty"`
	ToolName      string `yaml:"toolName,omitempty"`
}

type snapshotHydrationEarlier struct {
	State   string                   `yaml:"state"`
	Entries []snapshotHydrationEntry `yaml:"entries"`
}

type snapshotHydrationFoldedTool struct {
	Parent     int    `yaml:"parent"`
	Call       string `yaml:"call"`
	Result     string `yaml:"result"`
	Arguments  string `yaml:"arguments,omitempty"`
	ResultText string `yaml:"resultText,omitempty"`
}

type snapshotHydrationCase struct {
	Name                         string                       `yaml:"name"`
	Harness                      string                       `yaml:"harness,omitempty"`
	Purpose                      string                       `yaml:"purpose,omitempty"`
	InputSubmissionCount         *int64                       `yaml:"inputSubmissionCount,omitempty"`
	TurnCount                    int                          `yaml:"turnCount,omitempty"`
	Legacy                       bool                         `yaml:"legacy,omitempty"`
	Earlier                      []snapshotHydrationEarlier   `yaml:"earlier,omitempty"`
	Main                         []snapshotHydrationEntry     `yaml:"main,omitempty"`
	CorruptRef                   string                       `yaml:"corruptRef,omitempty"`
	DropContentRef               string                       `yaml:"dropContentRef,omitempty"`
	ExpectError                  bool                         `yaml:"expectError,omitempty"`
	ExpectLegacy                 bool                         `yaml:"expectLegacy,omitempty"`
	ExpectedMainIndices          []int                        `yaml:"expectedMainIndices,omitempty"`
	ExpectedFoldedTool           *snapshotHydrationFoldedTool `yaml:"expectedFoldedTool,omitempty"`
	ExpectedEarlierCounts        []int                        `yaml:"expectedEarlierCounts,omitempty"`
	ExpectedInputSubmissionCount *int64                       `yaml:"expectedInputSubmissionCount,omitempty"`
	ExpectedTurnCount            *int                         `yaml:"expectedTurnCount,omitempty"`
	ExpectedLongResultBytes      int                          `yaml:"expectedLongResultBytes,omitempty"`
	ExpectedTurnContents         map[int]string               `yaml:"expectedTurnContents,omitempty"`
	ExpectedTurnEntryTypes       map[int]string               `yaml:"expectedTurnEntryTypes,omitempty"`
	ExpectedTurnSubmissions      map[int]string               `yaml:"expectedTurnSubmissions,omitempty"`
	ExpectedEarlierContents      []string                     `yaml:"expectedEarlierContents,omitempty"`
}

type snapshotHydrationFixture struct {
	Cases []snapshotHydrationCase `yaml:"cases"`
}

func loadSnapshotHydrationFixture(t *testing.T) snapshotHydrationFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(snapshotDetailHydrationFixtureYAML))
	decoder.KnownFields(true)
	var fixture snapshotHydrationFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode snapshot hydration fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("snapshot hydration fixture must contain exactly one YAML document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(snapshotDetailHydrationManifestYAML, "snapshot hydration")
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(fixture.Cases))
	caseNames := make([]string, 0, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || names[fixtureCase.Name] {
			t.Fatalf("snapshot hydration fixture case %q is missing or duplicated", fixtureCase.Name)
		}
		names[fixtureCase.Name] = true
		caseNames = append(caseNames, fixtureCase.Name)
		if !fixtureCase.ExpectError && !fixtureCase.ExpectLegacy && len(fixtureCase.ExpectedMainIndices) == 0 {
			t.Fatalf("snapshot hydration fixture case %q asserts no main turns", fixtureCase.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, caseNames, "snapshot hydration"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// memorySnapshotResolver is an in-memory ContentResolver for the hydration
// boundary under test. It proves integrity handling without touching the
// filesystem: the corrupt ref fails like a damaged artifact, every other known
// ref returns its captured bytes.
type memorySnapshotResolver struct {
	blobs      map[schema.SourceEntryRef][]byte
	corruptRef schema.SourceEntryRef
}

func (m memorySnapshotResolver) ReadFullContent(_ context.Context, _ schema.SessionID, _ string, record indexformat.ContentRecord) ([]byte, error) {
	if record.Ref == m.corruptRef {
		return nil, fmt.Errorf("memory snapshot resolver: managed content for source ref %q fails its integrity digest; the artifact is corrupt", record.Ref)
	}
	blob, ok := m.blobs[record.Ref]
	if !ok {
		return nil, fmt.Errorf("memory snapshot resolver: no captured blob for source ref %q", record.Ref)
	}
	return blob, nil
}

var _ indexformat.ContentResolver = memorySnapshotResolver{}

func hydrationEntryContent(source snapshotHydrationEntry) string {
	if source.ContentRepeat > 0 {
		return strings.Repeat(source.ContentSeed, source.ContentRepeat)
	}
	return source.Content
}

func hydrationProvenance(t *testing.T, fixtureCase snapshotHydrationCase, source snapshotHydrationEntry) *schema.ContentProvenance {
	t.Helper()
	if source.Submission == "" && source.Origin == "" {
		return nil
	}
	submission, err := schema.NewSubmissionRef(source.Submission)
	if err != nil {
		t.Fatalf("case %q entry %d has invalid submission ref: %v", fixtureCase.Name, source.Index, err)
	}
	return &schema.ContentProvenance{
		Origin:        schema.ContentOrigin(source.Origin),
		Actor:         schema.ActorOrigin(source.Actor),
		Delivery:      schema.DeliveryOrigin(source.Delivery),
		Ownership:     schema.ContentOwnership(source.Ownership),
		Evidence:      schema.EvidenceKind(source.Evidence),
		InputModality: schema.InputModality(source.Modality),
		SubmissionRef: submission,
	}
}

func hydrationSessionEntry(t *testing.T, fixtureCase snapshotHydrationCase, sessionID schema.SessionID, harness schema.Harness, source snapshotHydrationEntry) schema.SessionEntry {
	t.Helper()
	ref, err := schema.NewSourceEntryRef(source.Ref)
	if err != nil {
		t.Fatalf("case %q entry %d has invalid source ref: %v", fixtureCase.Name, source.Index, err)
	}
	entryType := schema.EntryType(source.EntryType)
	if entryType == "" {
		entryType = schema.EntryTypeText
	}
	entry := schema.SessionEntry{
		SessionID:      sessionID,
		EntryIndex:     source.Index,
		Harness:        harness,
		EntryType:      entryType,
		Role:           schema.Role(source.Role),
		Depth:          source.Depth,
		ParentIndex:    source.ParentIndex,
		SourceEntryRef: ref,
		Provenance:     hydrationProvenance(t, fixtureCase, source),
	}
	if !entry.Role.IsValid() {
		t.Fatalf("case %q entry %d has invalid role %q", fixtureCase.Name, source.Index, source.Role)
	}
	if source.ToolCallID != "" {
		toolCallID := source.ToolCallID
		entry.ToolCallID = &toolCallID
	}
	if source.ToolName != "" {
		toolName := source.ToolName
		entry.ToolNamesCSV = &toolName
	}
	return entry
}

func buildHydrationSnapshot(t *testing.T, fixtureCase snapshotHydrationCase) (indexformat.ReadSnapshot, memorySnapshotResolver) {
	t.Helper()
	sessionID := schema.SessionID(testutil.TestSessionUUID)
	harness := schema.Harness(fixtureCase.Harness)
	if fixtureCase.Legacy {
		return indexformat.ReadSnapshot{
			Session:      schema.SessionDetailPayload{ID: string(sessionID), Harness: harness},
			Metadata:     schema.UnifiedMetadata{SchemaVersion: 1, SessionID: sessionID, ModelHarness: harness},
			IndexVersion: 1,
			LegacySource: indexformat.LegacySource{Harness: harness, Path: "retained-transcript"},
		}, memorySnapshotResolver{}
	}
	var count *int64
	if fixtureCase.InputSubmissionCount != nil {
		dupe := *fixtureCase.InputSubmissionCount
		count = &dupe
	}
	purpose := schema.SessionPurpose(fixtureCase.Purpose)
	metadata := schema.UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     sessionID,
		ModelHarness:  harness,
		Stats: schema.SessionStats{
			TurnCount:            fixtureCase.TurnCount,
			InputSubmissionCount: count,
		},
		Purpose: purpose,
	}
	session := schema.SessionDetailPayload{
		ID:                   string(sessionID),
		Harness:              harness,
		TurnCount:            fixtureCase.TurnCount,
		InputSubmissionCount: count,
		Purpose:              purpose,
	}
	mainEntries := make([]schema.SessionEntry, 0, len(fixtureCase.Main))
	content := make([]indexformat.ContentRecord, 0, len(fixtureCase.Main))
	resolver := memorySnapshotResolver{blobs: map[schema.SourceEntryRef][]byte{}, corruptRef: schema.SourceEntryRef(fixtureCase.CorruptRef)}
	addEntry := func(entries *[]schema.SessionEntry, source snapshotHydrationEntry) {
		entry := hydrationSessionEntry(t, fixtureCase, sessionID, harness, source)
		*entries = append(*entries, entry)
		if source.Ref == fixtureCase.DropContentRef {
			return
		}
		ref := entry.SourceEntryRef
		blob := []byte(hydrationEntryContent(source))
		content = append(content, indexformat.ContentRecord{Ref: ref, RelativeBlob: "c_" + string(ref) + ".blob", ByteLength: int64(len(blob)), Digest: "fixture"})
		resolver.blobs[ref] = blob
	}
	for _, source := range fixtureCase.Main {
		addEntry(&mainEntries, source)
	}
	var earlier []indexformat.EarlierPartition
	for _, section := range fixtureCase.Earlier {
		state, err := schema.NewEarlierHistoryState(section.State)
		if err != nil {
			t.Fatalf("case %q has invalid earlier state: %v", fixtureCase.Name, err)
		}
		var entries []schema.SessionEntry
		for _, source := range section.Entries {
			addEntry(&entries, source)
		}
		earlier = append(earlier, indexformat.EarlierPartition{State: state, Content: indexformat.Partition{Entries: entries}})
	}
	snapshot := indexformat.ReadSnapshot{
		Session:      session,
		Metadata:     metadata,
		GenerationID: "g1-" + fixtureCase.Name,
		Completeness: indexformat.GenerationCompletenessComplete,
		IndexVersion: 2,
		Main:         indexformat.Partition{Entries: mainEntries},
		Earlier:      earlier,
		Content:      content,
	}
	return snapshot, resolver
}

func TestSnapshotToDetailValidated(t *testing.T) {
	fixture := loadSnapshotHydrationFixture(t)
	for _, fixtureCase := range fixture.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			snapshot, resolver := buildHydrationSnapshot(t, fixtureCase)
			detail, err := SnapshotToDetailValidated(context.Background(), snapshot, resolver)
			if fixtureCase.ExpectLegacy {
				if !errors.Is(err, ErrLegacySnapshot) {
					t.Fatalf("legacy snapshot must report the legacy path, got detail=%v err=%v", detail, err)
				}
				return
			}
			if fixtureCase.ExpectError {
				if err == nil {
					t.Fatalf("expected hydration failure, got detail with %d turns", len(detail.Turns))
				}
				return
			}
			if err != nil {
				t.Fatalf("SnapshotToDetailValidated: %v", err)
			}
			if len(detail.Turns) != len(fixtureCase.ExpectedMainIndices) {
				t.Fatalf("main turns = %d, want %d", len(detail.Turns), len(fixtureCase.ExpectedMainIndices))
			}
			for i, want := range fixtureCase.ExpectedMainIndices {
				if detail.Turns[i].Index != want {
					t.Fatalf("main turn %d has index %d, want %d", i, detail.Turns[i].Index, want)
				}
			}
			if len(detail.EarlierHistory) != len(fixtureCase.ExpectedEarlierCounts) {
				t.Fatalf("earlier sections = %d, want %d", len(detail.EarlierHistory), len(fixtureCase.ExpectedEarlierCounts))
			}
			for i, want := range fixtureCase.ExpectedEarlierCounts {
				if len(detail.EarlierHistory[i].Turns) != want {
					t.Fatalf("earlier[%d] turns = %d, want %d", i, len(detail.EarlierHistory[i].Turns), want)
				}
			}
			if fixtureCase.ExpectedInputSubmissionCount != nil {
				if detail.InputSubmissionCount == nil || *detail.InputSubmissionCount != *fixtureCase.ExpectedInputSubmissionCount {
					t.Fatalf("inputSubmissionCount = %v, want %d", detail.InputSubmissionCount, *fixtureCase.ExpectedInputSubmissionCount)
				}
			}
			if fixtureCase.ExpectedTurnCount != nil && detail.TurnCount != *fixtureCase.ExpectedTurnCount {
				t.Fatalf("turnCount = %d, want %d", detail.TurnCount, *fixtureCase.ExpectedTurnCount)
			}
			if want := fixtureCase.ExpectedFoldedTool; want != nil {
				turn := detail.Turns[want.Parent]
				if len(turn.ToolCalls) != 1 {
					t.Fatalf("parent turn %d tool calls = %d, want 1", want.Parent, len(turn.ToolCalls))
				}
				call := turn.ToolCalls[0]
				if string(call.CallEntryRef) != want.Call || string(call.ResultEntryRef) != want.Result {
					t.Fatalf("folded tool refs = %q/%q, want %q/%q", call.CallEntryRef, call.ResultEntryRef, want.Call, want.Result)
				}
				if want.Arguments != "" && call.Arguments != want.Arguments {
					t.Fatalf("folded tool arguments = %q, want %q", call.Arguments, want.Arguments)
				}
				if want.ResultText != "" && call.Result != want.ResultText {
					t.Fatalf("folded tool result = %q, want %q", call.Result, want.ResultText)
				}
			}
			if want := fixtureCase.ExpectedLongResultBytes; want > 0 {
				var found bool
				for _, turn := range detail.Turns {
					for _, call := range turn.ToolCalls {
						if len(call.Result) == want {
							found = true
						}
					}
				}
				if !found {
					t.Fatalf("no folded tool result carries exactly %d bytes", want)
				}
			}
			for index, want := range fixtureCase.ExpectedTurnContents {
				var found *schema.TurnDetail
				for i := range detail.Turns {
					if detail.Turns[i].Index == index {
						found = &detail.Turns[i]
					}
				}
				if found == nil {
					t.Fatalf("no main turn carries index %d", index)
				}
				if found.Content != want {
					t.Fatalf("main turn %d content = %q, want %q", index, found.Content, want)
				}
			}
			for index, want := range fixtureCase.ExpectedTurnEntryTypes {
				for i := range detail.Turns {
					if detail.Turns[i].Index == index && string(detail.Turns[i].EntryType) != want {
						t.Fatalf("main turn %d entryType = %q, want %q", index, detail.Turns[i].EntryType, want)
					}
				}
			}
			for index, want := range fixtureCase.ExpectedTurnSubmissions {
				var found *schema.TurnDetail
				for i := range detail.Turns {
					if detail.Turns[i].Index == index {
						found = &detail.Turns[i]
					}
				}
				if found == nil {
					t.Fatalf("no main turn carries index %d", index)
				}
				if found.Provenance == nil || string(found.Provenance.SubmissionRef) != want {
					t.Fatalf("main turn %d submission = %v, want %q", index, found.Provenance, want)
				}
			}
			for i, want := range fixtureCase.ExpectedEarlierContents {
				if i >= len(detail.EarlierHistory) || len(detail.EarlierHistory[i].Turns) == 0 {
					t.Fatalf("earlier[%d] carries no turn for content assertion", i)
				}
				if got := detail.EarlierHistory[i].Turns[0].Content; got != want {
					t.Fatalf("earlier[%d] turn content = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// stubSnapshotReader serves one canned snapshot through the production
// SnapshotReader contract. The callback runs synchronously, so the test proves
// the boundary's lock-scoped hydration and serialization without a database.
type stubSnapshotReader struct {
	snapshot indexformat.ReadSnapshot
	err      error
}

func (s stubSnapshotReader) WithSessionSnapshot(_ context.Context, _ schema.SessionID, fn func(indexformat.ReadSnapshot) error) error {
	if s.err != nil {
		return s.err
	}
	return fn(s.snapshot)
}

var _ indexformat.SnapshotReader = stubSnapshotReader{}

// TestBuildSnapshotDetailBytes proves the single payload-construction boundary:
// hydration, folding, validation and final serialization happen inside the
// snapshot callback, and the returned bytes decode to the returned payload.
// Callers therefore send or write owned bytes after the lock is released, with
// no store access and no network inside the lock.
func TestBuildSnapshotDetailBytes(t *testing.T) {
	fixture := loadSnapshotHydrationFixture(t)
	var section4 snapshotHydrationCase
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "section4_exact_layout" {
			section4 = fixtureCase
		}
	}
	if section4.Name == "" {
		t.Fatal("section4_exact_layout case is missing from the hydration fixture")
	}
	snapshot, resolver := buildHydrationSnapshot(t, section4)
	sessionID := snapshot.Metadata.SessionID

	owned, payload, err := BuildSnapshotDetailBytes(context.Background(), stubSnapshotReader{snapshot: snapshot}, resolver, sessionID)
	if err != nil {
		t.Fatalf("BuildSnapshotDetailBytes: %v", err)
	}
	var decoded schema.SessionDetailPayload
	if err := json.Unmarshal(owned, &decoded); err != nil {
		t.Fatalf("owned bytes do not decode to a detail payload: %v", err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-encode decoded payload: %v", err)
	}
	if !bytes.Equal(owned, encoded) {
		t.Fatal("owned bytes differ from the re-encoded payload; serialization is not stable")
	}
	if len(payload.Turns) != 5 || len(decoded.Turns) != 5 {
		t.Fatalf("turns = %d/%d, want 5/5", len(payload.Turns), len(decoded.Turns))
	}
	if payload.InputSubmissionCount == nil || *payload.InputSubmissionCount != 1 {
		t.Fatalf("inputSubmissionCount = %v, want 1", payload.InputSubmissionCount)
	}

	if _, _, err := BuildSnapshotDetailBytes(context.Background(), stubSnapshotReader{err: errors.New("snapshot unavailable")}, resolver, sessionID); err == nil {
		t.Fatal("snapshot read failure must fail the boundary")
	}
	legacy, _ := buildHydrationSnapshot(t, snapshotHydrationCase{Legacy: true, Harness: string(defaults.HarnessCodex)})
	if _, _, err := BuildSnapshotDetailBytes(context.Background(), stubSnapshotReader{snapshot: legacy}, resolver, sessionID); !errors.Is(err, ErrLegacySnapshot) {
		t.Fatalf("legacy snapshot must report the legacy path, got %v", err)
	}
}
