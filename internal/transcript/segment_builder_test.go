package transcript

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/segment_cut.yaml
var segmentCutFixtureYAML []byte

//go:embed testdata/segment_cut.manifest.yaml
var segmentCutManifestYAML []byte

//go:embed testdata/conversation_turns_check.yaml
var conversationTurnsCheckFixtureYAML []byte

//go:embed testdata/conversation_turns_check.manifest.yaml
var conversationTurnsCheckManifestYAML []byte

//go:embed testdata/segment_turns.yaml
var segmentTurnsFixtureYAML []byte

//go:embed testdata/segment_turns.manifest.yaml
var segmentTurnsManifestYAML []byte

type segmentTurnSpec struct {
	Index          int    `yaml:"index"`
	Role           string `yaml:"role"`
	ContentBytes   int    `yaml:"contentBytes"`
	ArgumentsBytes []int  `yaml:"argumentsBytes,omitempty"`
	ResultBytes    []int  `yaml:"resultBytes,omitempty"`
	TextPartBytes  int    `yaml:"textPartBytes,omitempty"`
	ImageRef       string `yaml:"imageRef,omitempty"`
	// EntryType is the top-level entry type; text when the row omits it.
	EntryType string `yaml:"entryType,omitempty"`
}

func (s segmentTurnSpec) entryType() (schema.EntryType, error) {
	switch s.EntryType {
	case "", "text":
		return schema.EntryTypeText, nil
	case "tool_use":
		return schema.EntryTypeToolUse, nil
	case "tool_result":
		return schema.EntryTypeToolResult, nil
	case "thinking":
		return schema.EntryTypeThinking, nil
	case "system":
		return schema.EntryTypeSystem, nil
	}
	return "", fmt.Errorf("segment fixture names entry type %q; use text, tool_use, tool_result, thinking or system", s.EntryType)
}

func (s segmentTurnSpec) role() (schema.Role, error) {
	switch s.Role {
	case "user":
		return schema.RoleUser, nil
	case "assistant":
		return schema.RoleAssistant, nil
	case "system":
		return schema.RoleSystem, nil
	case "tool":
		return schema.RoleTool, nil
	}
	return "", fmt.Errorf("segment fixture names role %q; use user, assistant, system or tool", s.Role)
}

func (s segmentTurnSpec) turn() (ingest.Turn, error) {
	role, err := s.role()
	if err != nil {
		return ingest.Turn{}, err
	}
	if len(s.ArgumentsBytes) != len(s.ResultBytes) {
		return ingest.Turn{}, fmt.Errorf("segment fixture turn %d states %d tool arguments and %d results", s.Index, len(s.ArgumentsBytes), len(s.ResultBytes))
	}
	entryType, err := s.entryType()
	if err != nil {
		return ingest.Turn{}, err
	}
	turn := ingest.Turn{Index: s.Index, Role: role, EntryType: entryType, Content: strings.Repeat("x", s.ContentBytes)}
	for i := range s.ArgumentsBytes {
		turn.ToolCalls = append(turn.ToolCalls, ingest.ToolCall{
			ID:        fmt.Sprintf("call-%d-%d", s.Index, i),
			Name:      "Bash",
			Arguments: strings.Repeat("x", s.ArgumentsBytes[i]),
			Result:    strings.Repeat("x", s.ResultBytes[i]),
		})
	}
	return turn, nil
}

type segmentRefExpectation struct {
	Index      int `yaml:"index"`
	FirstTurn  int `yaml:"firstTurn"`
	LastTurn   int `yaml:"lastTurn"`
	ByteLength int `yaml:"byteLength"`
	// ConversationTurns is the literal span the cut must state for the segment.
	ConversationTurns segmentSpanExpectation `yaml:"conversationTurns"`
}

type segmentSpanExpectation struct {
	Before int `yaml:"before"`
	Count  int `yaml:"count"`
}

// refMatches compares a cut ref with its fixture row, span included: a ref
// without a span never matches.
func (want segmentRefExpectation) refMatches(ref schema.TranscriptSegmentRef) bool {
	if ref.ConversationTurns == nil {
		return false
	}
	got := segmentRefExpectation{
		Index:             ref.Index,
		FirstTurn:         ref.FirstTurn,
		LastTurn:          ref.LastTurn,
		ByteLength:        ref.ByteLength,
		ConversationTurns: segmentSpanExpectation{Before: ref.ConversationTurns.Before, Count: ref.ConversationTurns.Count},
	}
	return got == want
}

type segmentCutCase struct {
	Name        string                `yaml:"name"`
	BudgetBytes int                   `yaml:"budgetBytes"`
	Turns       []segmentTurnSpec     `yaml:"turns"`
	Expected    segmentCutExpectation `yaml:"expected"`
}

type segmentCutExpectation struct {
	Segments     []segmentRefExpectation `yaml:"segments"`
	CrossingTurn int                     `yaml:"crossingTurn,omitempty"`
	// UncountedSegments is the least number of segments that must hold a system
	// or tool row, so a position case cannot pass on a cut with none.
	UncountedSegments int `yaml:"uncountedSegments,omitempty"`
}

type segmentCutFixture struct {
	Cases []segmentCutCase `yaml:"cases"`
}

type segmentTurnExpectation struct {
	Truncated  bool `yaml:"truncated"`
	ByteLength int  `yaml:"byteLength"`
}

type segmentTurnsCase struct {
	Name          string                   `yaml:"name"`
	PerFieldBytes int                      `yaml:"perFieldBytes"`
	DocumentBytes int                      `yaml:"documentBytes,omitempty"`
	Turns         []segmentTurnSpec        `yaml:"turns"`
	Expected      []segmentTurnExpectation `yaml:"expected"`
}

type segmentTurnsFixture struct {
	Cases []segmentTurnsCase `yaml:"cases"`
}

func decodeYAML[T any](raw []byte, label string) (T, error) {
	var value T
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return value, fmt.Errorf("%s must contain exactly one YAML document: %v", label, err)
	}
	return value, nil
}

func loadSegmentCutFixture(t *testing.T) segmentCutFixture {
	t.Helper()
	fixture, err := decodeYAML[segmentCutFixture](segmentCutFixtureYAML, "segment cut fixture")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(segmentCutManifestYAML, "segment cut")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	seen := make(map[string]bool, len(fixture.Cases))
	for i, fixtureCase := range fixture.Cases {
		names[i] = fixtureCase.Name
		if fixtureCase.Name == "" || fixtureCase.BudgetBytes <= 0 {
			t.Fatalf("segment cut fixture case %d is incomplete", i)
		}
		if seen[fixtureCase.Name] {
			t.Fatalf("segment cut fixture repeats case %q", fixtureCase.Name)
		}
		seen[fixtureCase.Name] = true
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "segment cut"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func loadSegmentTurnsFixture(t *testing.T) segmentTurnsFixture {
	t.Helper()
	fixture, err := decodeYAML[segmentTurnsFixture](segmentTurnsFixtureYAML, "segment turn bound fixture")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(segmentTurnsManifestYAML, "segment turn bound")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, fixtureCase := range fixture.Cases {
		names[i] = fixtureCase.Name
		if fixtureCase.Name == "" || fixtureCase.PerFieldBytes <= 0 || len(fixtureCase.Turns) == 0 {
			t.Fatalf("segment turn bound fixture case %d is incomplete", i)
		}
		if len(fixtureCase.Expected) != len(fixtureCase.Turns) {
			t.Fatalf("segment turn bound fixture case %q states %d expectations for %d turns", fixtureCase.Name, len(fixtureCase.Expected), len(fixtureCase.Turns))
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "segment turn bound"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestSegmentCutFixtureCases(t *testing.T) {
	t.Parallel()
	fixture := loadSegmentCutFixture(t)
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			turns := make([]ingest.Turn, len(fixtureCase.Turns))
			for i, spec := range fixtureCase.Turns {
				turn, err := spec.turn()
				if err != nil {
					t.Fatal(err)
				}
				turns[i] = turn
			}
			refs, totalTurns := CutSegments(turns, fixtureCase.BudgetBytes)
			if want := fixtureCase.Expected.UncountedSegments; want > 0 {
				holding := 0
				for _, ref := range refs {
					for _, turn := range turns {
						inSegment := turn.Index >= ref.FirstTurn && turn.Index <= ref.LastTurn
						uncounted := turn.Role == schema.RoleSystem || turn.Role == schema.RoleTool
						if inSegment && uncounted {
							holding++
							break
						}
					}
				}
				if holding < want {
					t.Fatalf("%d segment(s) hold a system or tool row, fixture requires at least %d", holding, want)
				}
			}
			if totalTurns != len(turns) {
				t.Fatalf("cut reports %d turns, want %d", totalTurns, len(turns))
			}
			if len(refs) != len(fixtureCase.Expected.Segments) {
				t.Fatalf("cut produced %d segments, fixture expects %d: %+v", len(refs), len(fixtureCase.Expected.Segments), refs)
			}
			for i, want := range fixtureCase.Expected.Segments {
				if !want.refMatches(refs[i]) {
					t.Fatalf("segment %d = %+v with span %+v, want %+v", i, refs[i], refs[i].ConversationTurns, want)
				}
			}
			// Every turn must belong to exactly one segment, and the segment's
			// turns must be the whole range it declares.
			for _, ref := range refs {
				inSegment := 0
				for _, turn := range turns {
					if turn.Index >= ref.FirstTurn && turn.Index <= ref.LastTurn {
						inSegment++
					}
				}
				if inSegment == 0 {
					t.Fatalf("segment %d declares turns %d-%d but holds no turns", ref.Index, ref.FirstTurn, ref.LastTurn)
				}
			}
			if fixtureCase.Expected.CrossingTurn != 0 {
				// The crossing turn is absent from the segment that crossed its
				// budget and first in the next one: no turn is split.
				for i := 0; i+1 < len(refs); i++ {
					if refs[i].LastTurn < fixtureCase.Expected.CrossingTurn && fixtureCase.Expected.CrossingTurn <= refs[i+1].LastTurn {
						if refs[i+1].FirstTurn != fixtureCase.Expected.CrossingTurn {
							t.Fatalf("crossing turn %d is not first in segment %d (starts at %d)", fixtureCase.Expected.CrossingTurn, refs[i+1].Index, refs[i+1].FirstTurn)
						}
					}
				}
			}
		})
	}
}

func TestSegmentTurnBoundFixtureCases(t *testing.T) {
	t.Parallel()
	fixture := loadSegmentTurnsFixture(t)
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			documentBytes := fixtureCase.DocumentBytes
			if documentBytes == 0 {
				documentBytes = 16 * fixtureCase.PerFieldBytes
			}
			budget, err := NewServedDocumentBudget(fixtureCase.PerFieldBytes, documentBytes)
			if err != nil {
				t.Fatal(err)
			}
			turns := make([]schema.TurnDetail, len(fixtureCase.Turns))
			for i, spec := range fixtureCase.Turns {
				role, err := spec.role()
				if err != nil {
					t.Fatal(err)
				}
				turns[i] = schema.TurnDetail{
					Index:   spec.Index,
					Role:    role,
					Content: strings.Repeat("x", spec.ContentBytes),
				}
				if spec.TextPartBytes > 0 {
					turns[i].ContentParts = append(turns[i].ContentParts, schema.TurnContentPart{Type: schema.TurnPartTypeText, Text: strings.Repeat("x", spec.TextPartBytes)})
				}
				if spec.ImageRef != "" {
					turns[i].ContentParts = append(turns[i].ContentParts, schema.TurnContentPart{Type: schema.TurnPartTypeImage, Image: &schema.ImagePart{Ref: spec.ImageRef, MediaType: schema.ImageMediaType("image/png"), ByteSize: 8}})
				}
				for call := range spec.ArgumentsBytes {
					turns[i].ToolCalls = append(turns[i].ToolCalls, schema.ToolCallDetail{
						ID:        fmt.Sprintf("call-%d-%d", spec.Index, call),
						Arguments: strings.Repeat("x", spec.ArgumentsBytes[call]),
						Result:    strings.Repeat("x", spec.ResultBytes[call]),
					})
				}
			}
			before := make([]int, len(turns))
			for i := range turns {
				before[i] = TurnByteLength(turns[i])
			}
			bounds := BoundServedTurns(turns, budget)
			if fixtureCase.DocumentBytes > 0 {
				encoded, err := json.Marshal(schema.SessionDetailPayload{Turns: turns})
				if err != nil || len(encoded) > documentBytes {
					t.Fatalf("bounded document size = %d, budget = %d, error = %v", len(encoded), documentBytes, err)
				}
			}
			for i, want := range fixtureCase.Expected {
				if bounds[i].Truncated != want.Truncated {
					t.Fatalf("turn %d truncated = %v, want %v", turns[i].Index, bounds[i].Truncated, want.Truncated)
				}
				if bounds[i].ByteLength != want.ByteLength {
					t.Fatalf("turn %d byteLength = %d, want %d", turns[i].Index, bounds[i].ByteLength, want.ByteLength)
				}
				if bounds[i].ByteLength != before[i] {
					t.Fatalf("turn %d byteLength changed after the bound: %d then %d", turns[i].Index, before[i], bounds[i].ByteLength)
				}
				if fixtureCase.Turns[i].TextPartBytes > fixtureCase.PerFieldBytes && len(turns[i].ContentParts[0].Text) >= fixtureCase.Turns[i].TextPartBytes {
					t.Fatal("oversized text part was not bounded")
				}
			}
		})
	}
}

// TestSegmentTurnRowsCarryTheBoundFacts pins the row projection: a segment row
// is the one TurnDetail projection plus truncated and the pre-bound size.
func TestSegmentTurnRowsCarryTheBoundFacts(t *testing.T) {
	t.Parallel()
	turns := []ingest.Turn{
		{Index: 4, Role: schema.RoleUser, Content: "hello"},
		{Index: 7, Role: schema.RoleAssistant, Content: strings.Repeat("x", 2*defaults.ServedTextFieldBudgetBytes)},
	}
	rows := SegmentTurnRows(turns, DefaultServedDocumentBudget())
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Index != 4 || rows[0].Truncated || rows[0].ByteLength != 7 {
		t.Fatalf("small row = %+v, want an unbounded turn with byteLength 7", rows[0])
	}
	if rows[1].Index != 7 || !rows[1].Truncated || rows[1].ByteLength != 2*defaults.ServedTextFieldBudgetBytes+2 {
		t.Fatalf("oversized row truncated=%v byteLength=%d, want bounded and %d", rows[1].Truncated, rows[1].ByteLength, 2*defaults.ServedTextFieldBudgetBytes+2)
	}
	if !strings.HasPrefix(rows[1].Content, "xxx") || len(rows[1].Content) >= 2*defaults.ServedTextFieldBudgetBytes {
		t.Fatal("oversized row content was not bounded")
	}
}

// TestTurnIdentityIsTheSourceEntryIndex pins the one coordinate the segment
// read relies on: a projected turn's Index is the depth-0 entry's EntryIndex on
// every stored path, so a segment read can resume exactly at firstTurn.
func TestTurnIdentityIsTheSourceEntryIndex(t *testing.T) {
	t.Parallel()
	entries := []schema.SessionEntry{
		{EntryIndex: 4, Depth: 0, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: strPtrForSegmentTest("prompt")},
		{EntryIndex: 9, Depth: 0, Role: schema.RoleAssistant, EntryType: schema.EntryTypeText, ContentPreview: strPtrForSegmentTest("answer")},
		{EntryIndex: 12, Depth: 1, ParentIndex: intPtrForSegmentTest(9), Role: schema.RoleAssistant, EntryType: schema.EntryTypeToolUse},
	}
	turns := EntriesToTurns(entries)
	if len(turns) != 2 {
		t.Fatalf("projected %d turns, want 2 (the depth-1 child folds into its parent)", len(turns))
	}
	for _, turn := range turns {
		if turn.Index != entries[0].EntryIndex && turn.Index != entries[1].EntryIndex {
			t.Fatalf("turn identity %d is not a depth-0 entry index", turn.Index)
		}
	}
}

func strPtrForSegmentTest(value string) *string { return &value }

func intPtrForSegmentTest(value int) *int { return &value }

// TestSegmentEntryCountsIncludeChildEntries pins the count a segment read uses
// as its limit: a turn owns its depth-0 entry and every child entry before the
// next turn's identity, so the count reaches past lastTurn's own children.
func TestSegmentEntryCountsIncludeChildEntries(t *testing.T) {
	t.Parallel()
	entries := []schema.SessionEntry{
		{EntryIndex: 4, Depth: 0},
		{EntryIndex: 5, Depth: 1},
		{EntryIndex: 6, Depth: 1},
		{EntryIndex: 7, Depth: 0},
		{EntryIndex: 8, Depth: 1},
	}
	refs := []schema.TranscriptSegmentRef{
		{Index: 0, FirstTurn: 4, LastTurn: 4},
		{Index: 1, FirstTurn: 7, LastTurn: 7},
	}
	counts := SegmentEntryCounts(entries, refs)
	if len(counts) != 2 || counts[0] != 3 || counts[1] != 2 {
		t.Fatalf("entry counts = %v, want [3 2]", counts)
	}
}

func TestFoldedTurnByteLengthMatchesTurnByteLength(t *testing.T) {
	t.Parallel()
	folded := ingest.Turn{
		Index:   4,
		Role:    schema.RoleAssistant,
		Content: "c",
		ToolCalls: []ingest.ToolCall{
			{Arguments: "aa", Result: "rrr"},
		},
	}
	rows := SegmentTurnRows([]ingest.Turn{folded}, DefaultServedDocumentBudget())
	if FoldedTurnByteLength(folded) != rows[0].ByteLength {
		t.Fatalf("folded byte length = %d, row byte length = %d", FoldedTurnByteLength(folded), rows[0].ByteLength)
	}
}

// TestCutTurnRowsDirect pins the cut over already-projected wire turns, the
// entry point the directory build uses.
func TestCutTurnRowsDirect(t *testing.T) {
	t.Parallel()
	turns := []schema.TurnDetail{
		{Index: 4, Role: schema.RoleUser, EntryType: schema.EntryTypeText, Content: strings.Repeat("a", 1024)},
		{Index: 7, Role: schema.RoleAssistant, EntryType: schema.EntryTypeText, Content: strings.Repeat("b", 1024)},
		{Index: 11, Role: schema.RoleUser, EntryType: schema.EntryTypeText, Content: strings.Repeat("c", 1024)},
	}
	refs, total := CutTurnRows(turns, 3*1024)
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(refs) != 2 {
		t.Fatalf("segments = %d, want 2 (two 1026-byte turns fit the 3072-byte budget; the third is deferred)", len(refs))
	}
	if !(segmentRefExpectation{Index: 0, FirstTurn: 4, LastTurn: 7, ByteLength: 2052, ConversationTurns: segmentSpanExpectation{Before: 0, Count: 2}}).refMatches(refs[0]) {
		t.Fatalf("ref 0 = %+v with span %+v, want turns 4-7 with 2052 bytes and span {0, 2}", refs[0], refs[0].ConversationTurns)
	}
	if !(segmentRefExpectation{Index: 1, FirstTurn: 11, LastTurn: 11, ByteLength: 1026, ConversationTurns: segmentSpanExpectation{Before: 2, Count: 1}}).refMatches(refs[1]) {
		t.Fatalf("ref 1 = %+v with span %+v, want turn 11 alone with 1026 bytes and span {2, 1}", refs[1], refs[1].ConversationTurns)
	}
}

type conversationTurnsCheckRef struct {
	Index int `yaml:"index"`
	// ConversationTurns is absent for a ref that carries no span.
	ConversationTurns *segmentSpanExpectation `yaml:"conversationTurns,omitempty"`
}

type conversationTurnsCheckFault struct {
	Rule    string `yaml:"rule"`
	Segment int    `yaml:"segment"`
	Want    int    `yaml:"want"`
	Got     int    `yaml:"got"`
	// MessageContains are fragments the fault's message must hold.
	MessageContains []string `yaml:"messageContains"`
}

type conversationTurnsCheckCase struct {
	Name      string                      `yaml:"name"`
	Canonical int                         `yaml:"canonical"`
	Refs      []conversationTurnsCheckRef `yaml:"refs"`
	// Fault is absent for a sound cut.
	Fault *conversationTurnsCheckFault `yaml:"fault,omitempty"`
}

type conversationTurnsCheckFixture struct {
	Cases []conversationTurnsCheckCase `yaml:"cases"`
}

func TestCheckConversationTurnsFixtureCases(t *testing.T) {
	t.Parallel()
	fixture, err := decodeYAML[conversationTurnsCheckFixture](conversationTurnsCheckFixtureYAML, "conversation turns check fixture")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(conversationTurnsCheckManifestYAML, "conversation turns check")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, fixtureCase := range fixture.Cases {
		names[i] = fixtureCase.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "conversation turns check"); err != nil {
		t.Fatal(err)
	}
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			refs := make([]schema.TranscriptSegmentRef, len(fixtureCase.Refs))
			for i, spec := range fixtureCase.Refs {
				refs[i] = schema.TranscriptSegmentRef{Index: spec.Index}
				if spec.ConversationTurns != nil {
					refs[i].ConversationTurns = &schema.ConversationTurnSpan{Before: spec.ConversationTurns.Before, Count: spec.ConversationTurns.Count}
				}
			}
			fault := CheckConversationTurns(refs, fixtureCase.Canonical)
			want := fixtureCase.Fault
			if want == nil {
				if fault != nil {
					t.Fatalf("a sound cut was faulted: %v", fault)
				}
				return
			}
			if fault == nil {
				t.Fatalf("the check passed a cut that breaks the %s rule", want.Rule)
			}
			wantFault := ConversationTurnsFault{Rule: ConversationTurnsRule(want.Rule), Segment: want.Segment, Want: want.Want, Got: want.Got}
			if *fault != wantFault {
				t.Fatalf("fault = %+v, want %+v", *fault, wantFault)
			}
			if len(want.MessageContains) == 0 {
				t.Fatal("a fault row must state the message fragments it expects")
			}
			for _, fragment := range want.MessageContains {
				if !strings.Contains(fault.Error(), fragment) {
					t.Fatalf("fault message does not hold %q: %s", fragment, fault.Error())
				}
			}
		})
	}
}
