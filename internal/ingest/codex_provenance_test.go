package ingest_test

// Fixture-backed validation for the Codex native provenance classifier and the
// candidate it emits. Every case drives the real production parser over
// sanitized native bytes through the actual capture, classification, graph
// mapping and shared projection, then asserts the durable entry field matrix,
// the graph, and the separate counts.

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/codex_provenance.yaml
var codexProvenanceYAML []byte

//go:embed testdata/codex_provenance.manifest.yaml
var codexProvenanceManifestYAML []byte

type codexProvenanceFixture struct {
	Origin        string `yaml:"origin"`
	Actor         string `yaml:"actor"`
	Delivery      string `yaml:"delivery"`
	Ownership     string `yaml:"ownership"`
	Evidence      string `yaml:"evidence"`
	InputModality string `yaml:"inputModality"`
}

type codexRelationshipFixture struct {
	Kind          string `yaml:"kind"`
	TargetState   string `yaml:"targetState"`
	TargetLocalID string `yaml:"targetLocalID"`
	Evidence      string `yaml:"evidence"`
	AnchorKind    string `yaml:"anchorKind"`
}

type codexPriorFixture struct {
	ParentUUID   string                     `yaml:"parentUUID"`
	Relationship []codexRelationshipFixture `yaml:"relationships"`
}

type codexEntryFixture struct {
	Ref           string                 `yaml:"ref"`
	Role          string                 `yaml:"role"`
	EntryType     string                 `yaml:"entryType"`
	Depth         int                    `yaml:"depth"`
	ParentIndex   *int                   `yaml:"parentIndex"`
	Content       string                 `yaml:"content"`
	ToolInput     string                 `yaml:"toolInput"`
	ToolOutput    string                 `yaml:"toolOutput"`
	ToolCallID    string                 `yaml:"toolCallId"`
	SubmissionRef string                 `yaml:"submissionRef"`
	Provenance    codexProvenanceFixture `yaml:"provenance"`
}

type codexEarlierFixture struct {
	State   string              `yaml:"state"`
	Entries []codexEntryFixture `yaml:"entries"`
}

type codexExpectFixture struct {
	Purpose               string                     `yaml:"purpose"`
	RootSessionID         string                     `yaml:"rootSessionID"`
	ParentUUID            string                     `yaml:"parentUUID"`
	Relationships         []codexRelationshipFixture `yaml:"relationships"`
	TurnCount             int                        `yaml:"turnCount"`
	InputSubmissionCount  *int64                     `yaml:"inputSubmissionCount"`
	InputSubmissionAbsent bool                       `yaml:"inputSubmissionAbsent"`
	TitleRefs             []string                   `yaml:"titleRefs"`
	Main                  []codexEntryFixture        `yaml:"main"`
	Earlier               []codexEarlierFixture      `yaml:"earlier"`
}

type codexThreadFixture struct {
	HasSessionMeta              bool   `yaml:"hasSessionMeta"`
	ThreadSource                string `yaml:"threadSource"`
	SourceInternalGuardian      bool   `yaml:"sourceInternalGuardian"`
	SourceSubagentOtherGuardian bool   `yaml:"sourceSubagentOtherGuardian"`
	TopParentID                 string `yaml:"topParentID"`
	NestedParentID              string `yaml:"nestedParentID"`
	ForkSourceID                string `yaml:"forkSourceID"`
	RootSessionID               string `yaml:"rootSessionID"`
}

type codexProvenanceCase struct {
	Name             string              `yaml:"name"`
	StableThreadID   string              `yaml:"stableThreadID"`
	Pointer          string              `yaml:"pointer"`
	PhysicalSourceID string              `yaml:"physicalSourceID"`
	GenerationID     string              `yaml:"generationID"`
	Completeness     string              `yaml:"completeness"`
	HistoryMode      string              `yaml:"historyMode"`
	Sources          map[string]string   `yaml:"sources"`
	Prior            *codexPriorFixture  `yaml:"prior"`
	Thread           *codexThreadFixture `yaml:"thread"`
	EntryRefs        []string            `yaml:"entryRefs"`
	SubmissionRefs   []string            `yaml:"submissionRefs"`
	Expected         codexExpectFixture  `yaml:"expected"`
}

type codexProvenanceFixtureDoc struct {
	Cases []codexProvenanceCase `yaml:"cases"`
}

func loadCodexProvenanceFixture(t *testing.T) codexProvenanceFixtureDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(codexProvenanceYAML))
	decoder.KnownFields(true)
	var fixture codexProvenanceFixtureDoc
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode codex provenance fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("codex provenance fixture must contain exactly one YAML document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(codexProvenanceManifestYAML, "codex provenance")
	if err != nil {
		t.Fatalf("load codex provenance manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("codex provenance fixture has an empty case name")
		}
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "codex provenance"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// codexProvenanceSource is a deterministic read-only source double. The
// production capture path is the code under test; this only supplies the
// sanitized native bytes and the selected authority.
type codexProvenanceSource struct {
	authority ingest.CodexSourceAuthority
	sources   map[string][]byte
}

func (s *codexProvenanceSource) ResolveCodexAuthority(context.Context, ingest.DiscoveredSession) (ingest.CodexSourceAuthority, error) {
	return s.authority, nil
}

func (s *codexProvenanceSource) ReadCodexSource(_ context.Context, pointer string) ([]byte, error) {
	data, ok := s.sources[pointer]
	if !ok {
		return nil, fmt.Errorf("fixture has no bytes for pointer %q", pointer)
	}
	return data, nil
}

// codexScriptedAllocator returns the fixture's exact opaque refs in allocation
// order so the expected field matrix binds each durable identity. A caller that
// needs more refs than the fixture names proves a projection emitted more
// blocks than the case declared.
type codexScriptedAllocator struct {
	entries     []string
	submissions []string
	entryIndex  int
	subIndex    int
}

func (a *codexScriptedAllocator) NewEntryRef() (schema.SourceEntryRef, error) {
	if a.entryIndex >= len(a.entries) {
		return "", fmt.Errorf("entry ref script exhausted after %d allocations", a.entryIndex)
	}
	ref := schema.SourceEntryRef(a.entries[a.entryIndex])
	a.entryIndex++
	return ref, nil
}

func (a *codexScriptedAllocator) NewSubmissionRef() (schema.SubmissionRef, error) {
	if a.subIndex >= len(a.submissions) {
		return "", fmt.Errorf("submission ref script exhausted after %d allocations", a.subIndex)
	}
	ref := schema.SubmissionRef(a.submissions[a.subIndex])
	a.subIndex++
	return ref, nil
}

func codexHistoryMode(t *testing.T, raw string) []byte {
	t.Helper()
	switch raw {
	case "":
		return nil
	case "null":
		return []byte("null")
	default:
		return []byte(fmt.Sprintf("%q", raw))
	}
}

func codexAuthorityForCase(t *testing.T, c codexProvenanceCase) ingest.CodexSourceAuthority {
	t.Helper()
	kind, err := ingest.NewCodexSourceAuthorityKind(string(ingest.CodexAuthorityDetachedFile))
	if err != nil {
		t.Fatal(err)
	}
	return ingest.CodexSourceAuthority{
		StableThreadID:   c.StableThreadID,
		Kind:             kind,
		CurrentPointer:   c.Pointer,
		PhysicalSourceID: c.PhysicalSourceID,
		HistoryMode:      codexHistoryMode(t, c.HistoryMode),
	}
}

func codexRelationship(t *testing.T, fixture codexRelationshipFixture) schema.SessionRelationship {
	t.Helper()
	kind, err := schema.NewSessionRelationshipKind(fixture.Kind)
	if err != nil {
		t.Fatal(err)
	}
	state, err := schema.NewRelationshipTargetState(fixture.TargetState)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := schema.NewEvidenceKind(fixture.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	relation := schema.SessionRelationship{Kind: kind, TargetState: state, Evidence: evidence}
	if fixture.TargetLocalID != "" {
		id := schema.SessionID(fixture.TargetLocalID)
		relation.TargetLocalID = &id
	}
	if fixture.AnchorKind != "" {
		anchorKind, err := schema.NewPublicSourceAnchorKind(fixture.AnchorKind)
		if err != nil {
			t.Fatal(err)
		}
		relation.Anchor = &schema.PublicSourceAnchor{Kind: anchorKind}
	}
	return relation
}

func codexPriorForCase(t *testing.T, prior *codexPriorFixture) *schema.UnifiedMetadata {
	t.Helper()
	if prior == nil {
		return nil
	}
	metadata := schema.NewUnifiedMetadata()
	if prior.ParentUUID != "" {
		id := schema.SessionID(prior.ParentUUID)
		metadata.ParentUUID = &id
	}
	for _, relation := range prior.Relationship {
		metadata.Relationships = append(metadata.Relationships, codexRelationship(t, relation))
	}
	return &metadata
}

func codexReferenceStrings(refs []schema.SourceEntryRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, string(ref))
	}
	return out
}

func runCodexProvenanceCandidate(t *testing.T, c codexProvenanceCase) ingest.CodexCandidate {
	t.Helper()
	source := &codexProvenanceSource{authority: codexAuthorityForCase(t, c), sources: map[string][]byte{}}
	for pointer, data := range c.Sources {
		source.sources[pointer] = []byte(data)
	}
	session := ingest.DiscoveredSession{
		SessionID:  schema.SessionID(c.StableThreadID),
		Harness:    ingest.HarnessCodex,
		SourcePath: ingest.ResolvedPath(c.Pointer),
	}
	history, err := ingest.CaptureCodexHistory(t.Context(), source, session, nil)
	if err != nil {
		t.Fatalf("CaptureCodexHistory: %v", err)
	}
	base := schema.NewUnifiedMetadata()
	base.SessionID = schema.SessionID(c.StableThreadID)
	base.ModelHarness = ingest.HarnessCodex
	completeness := indexformat.GenerationCompleteness(c.Completeness)
	candidate, err := ingest.BuildCodexCandidate(ingest.CodexCandidateInput{
		History:      history,
		Base:         base,
		Prior:        codexPriorForCase(t, c.Prior),
		PriorState:   ingest.NewProjectionPriorState(),
		Allocator:    &codexScriptedAllocator{entries: c.EntryRefs, submissions: c.SubmissionRefs},
		GenerationID: c.GenerationID,
	})
	if err != nil {
		t.Fatalf("BuildCodexCandidate: %v", err)
	}
	if completeness != "" && candidate.V2.Generation.Completeness != completeness {
		t.Fatalf("completeness = %q, want %q", candidate.V2.Generation.Completeness, completeness)
	}
	if candidate.Proof.StableThreadID != c.StableThreadID {
		t.Errorf("proof.stableThreadID = %q, want %q", candidate.Proof.StableThreadID, c.StableThreadID)
	}
	if candidate.Proof.Pointer != c.Pointer {
		t.Errorf("proof.pointer = %q, want %q", candidate.Proof.Pointer, c.Pointer)
	}
	if candidate.Proof.PhysicalSourceID != c.PhysicalSourceID {
		t.Errorf("proof.physicalSourceID = %q, want %q", candidate.Proof.PhysicalSourceID, c.PhysicalSourceID)
	}
	if candidate.Proof.GenerationID != c.GenerationID {
		t.Errorf("proof.generationID = %q, want %q", candidate.Proof.GenerationID, c.GenerationID)
	}
	return candidate
}

func codexAssertProvenance(t *testing.T, label string, want codexProvenanceFixture, got *schema.ContentProvenance) {
	t.Helper()
	if got == nil {
		if want != (codexProvenanceFixture{}) {
			t.Errorf("%s provenance = nil, want %+v", label, want)
		}
		return
	}
	if want.Origin != "" && string(got.Origin) != want.Origin {
		t.Errorf("%s origin = %q, want %q", label, got.Origin, want.Origin)
	}
	if want.Actor != "" && string(got.Actor) != want.Actor {
		t.Errorf("%s actor = %q, want %q", label, got.Actor, want.Actor)
	}
	if want.Delivery != "" && string(got.Delivery) != want.Delivery {
		t.Errorf("%s delivery = %q, want %q", label, got.Delivery, want.Delivery)
	}
	if want.Ownership != "" && string(got.Ownership) != want.Ownership {
		t.Errorf("%s ownership = %q, want %q", label, got.Ownership, want.Ownership)
	}
	if want.Evidence != "" && string(got.Evidence) != want.Evidence {
		t.Errorf("%s evidence = %q, want %q", label, got.Evidence, want.Evidence)
	}
	if want.InputModality != "" && string(got.InputModality) != want.InputModality {
		t.Errorf("%s inputModality = %q, want %q", label, got.InputModality, want.InputModality)
	}
}

func codexAssertEntries(t *testing.T, label string, want []codexEntryFixture, got []schema.SessionEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s entries = %d, want %d (%+v)", label, len(got), len(want), codexReferenceStrings(entryRefsOf(got)))
	}
	for i, w := range want {
		g := got[i]
		where := fmt.Sprintf("%s[%d]", label, i)
		if w.Ref != "" && string(g.SourceEntryRef) != w.Ref {
			t.Errorf("%s ref = %q, want %q", where, g.SourceEntryRef, w.Ref)
		}
		if w.Role != "" && string(g.Role) != w.Role {
			t.Errorf("%s role = %q, want %q", where, g.Role, w.Role)
		}
		if w.EntryType != "" && string(g.EntryType) != w.EntryType {
			t.Errorf("%s entryType = %q, want %q", where, g.EntryType, w.EntryType)
		}
		if g.Depth != w.Depth {
			t.Errorf("%s depth = %d, want %d", where, g.Depth, w.Depth)
		}
		switch {
		case w.ParentIndex == nil && g.ParentIndex != nil:
			t.Errorf("%s parentIndex = %d, want nil", where, *g.ParentIndex)
		case w.ParentIndex != nil && g.ParentIndex == nil:
			t.Errorf("%s parentIndex = nil, want %d", where, *w.ParentIndex)
		case w.ParentIndex != nil && *w.ParentIndex != *g.ParentIndex:
			t.Errorf("%s parentIndex = %d, want %d", where, *g.ParentIndex, *w.ParentIndex)
		}
		if w.ToolCallID != "" && (g.ToolCallID == nil || *g.ToolCallID != w.ToolCallID) {
			t.Errorf("%s toolCallId = %v, want %q", where, g.ToolCallID, w.ToolCallID)
		}
		codexAssertProvenance(t, where, w.Provenance, g.Provenance)
		if w.SubmissionRef != "" {
			if g.Provenance == nil || string(g.Provenance.SubmissionRef) != w.SubmissionRef {
				t.Errorf("%s submissionRef = %v, want %q", where, g.Provenance, w.SubmissionRef)
			}
		}
		switch g.EntryType {
		case schema.EntryTypeToolUse:
			if w.ToolInput != "" && (g.ToolInput == nil || *g.ToolInput != w.ToolInput) {
				t.Errorf("%s toolInput = %v, want %q", where, g.ToolInput, w.ToolInput)
			}
		case schema.EntryTypeToolResult:
			if w.ToolOutput != "" && (g.ToolOutput == nil || *g.ToolOutput != w.ToolOutput) {
				t.Errorf("%s toolOutput = %v, want %q", where, g.ToolOutput, w.ToolOutput)
			}
		default:
			if w.Content != "" && (g.ContentPreview == nil || *g.ContentPreview != w.Content) {
				t.Errorf("%s content = %v, want %q", where, g.ContentPreview, w.Content)
			}
		}
	}
}

func entryRefsOf(entries []schema.SessionEntry) []schema.SourceEntryRef {
	out := make([]schema.SourceEntryRef, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.SourceEntryRef)
	}
	return out
}

func codexAssertCase(t *testing.T, c codexProvenanceCase, candidate ingest.CodexCandidate) {
	t.Helper()
	generation := candidate.V2.Generation
	metadata := generation.Metadata
	if c.Expected.Purpose != "" && string(metadata.Purpose) != c.Expected.Purpose {
		t.Errorf("purpose = %q, want %q", metadata.Purpose, c.Expected.Purpose)
	}
	if c.Expected.RootSessionID != "" {
		if metadata.RootSessionID == nil || string(*metadata.RootSessionID) != c.Expected.RootSessionID {
			t.Errorf("rootSessionID = %v, want %q", metadata.RootSessionID, c.Expected.RootSessionID)
		}
	}
	if c.Expected.ParentUUID != "" {
		if metadata.ParentUUID == nil || string(*metadata.ParentUUID) != c.Expected.ParentUUID {
			t.Errorf("parentUUID = %v, want %q", metadata.ParentUUID, c.Expected.ParentUUID)
		}
	}
	if c.Expected.Relationships != nil {
		want := make([]schema.SessionRelationship, 0, len(c.Expected.Relationships))
		for _, relation := range c.Expected.Relationships {
			want = append(want, codexRelationship(t, relation))
		}
		if len(want) == 0 {
			want = nil
		}
		got := metadata.Relationships
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("relationships = %+v, want %+v", metadata.Relationships, c.Expected.Relationships)
		}
	}
	if generation.Metadata.Stats.TurnCount != c.Expected.TurnCount {
		t.Errorf("turnCount = %d, want %d", generation.Metadata.Stats.TurnCount, c.Expected.TurnCount)
	}
	switch {
	case c.Expected.InputSubmissionAbsent:
		if metadata.Stats.InputSubmissionCount != nil {
			t.Errorf("inputSubmissionCount = %d, want absent", *metadata.Stats.InputSubmissionCount)
		}
	case c.Expected.InputSubmissionCount != nil:
		if metadata.Stats.InputSubmissionCount == nil {
			t.Errorf("inputSubmissionCount = absent, want %d", *c.Expected.InputSubmissionCount)
		} else if *metadata.Stats.InputSubmissionCount != *c.Expected.InputSubmissionCount {
			t.Errorf("inputSubmissionCount = %d, want %d", *metadata.Stats.InputSubmissionCount, *c.Expected.InputSubmissionCount)
		}
	}
	if c.Expected.TitleRefs != nil && !slices.Equal(codexReferenceStrings(generation.TitleRefs), c.Expected.TitleRefs) {
		t.Errorf("titleRefs = %v, want %v", codexReferenceStrings(generation.TitleRefs), c.Expected.TitleRefs)
	}
	codexAssertEntries(t, "main", c.Expected.Main, generation.Main.Entries)
	if len(generation.Earlier) != len(c.Expected.Earlier) {
		t.Fatalf("earlier sections = %d, want %d", len(generation.Earlier), len(c.Expected.Earlier))
	}
	for i, want := range c.Expected.Earlier {
		if string(generation.Earlier[i].State) != want.State {
			t.Errorf("earlier[%d].state = %q, want %q", i, generation.Earlier[i].State, want.State)
		}
		codexAssertEntries(t, fmt.Sprintf("earlier[%d]", i), want.Entries, generation.Earlier[i].Content.Entries)
	}
}

func codexThreadFromFixture(f codexThreadFixture) ingest.CodexThreadEvidence {
	return ingest.CodexThreadEvidence{
		HasSessionMeta:              f.HasSessionMeta,
		ThreadSource:                f.ThreadSource,
		SourceInternalGuardian:      f.SourceInternalGuardian,
		SourceSubagentOtherGuardian: f.SourceSubagentOtherGuardian,
		TopParentID:                 f.TopParentID,
		NestedParentID:              f.NestedParentID,
		ForkSourceID:                f.ForkSourceID,
		RootSessionID:               f.RootSessionID,
	}
}

// codexAssertGraph runs the exported production graph mapper over the fixture
// thread evidence and asserts the canonical logical graph.
func codexAssertGraph(t *testing.T, c codexProvenanceCase) {
	t.Helper()
	if c.Thread == nil {
		t.Fatalf("graph case %q has no thread evidence", c.Name)
	}
	mapping, err := ingest.MapCodexGraph(codexThreadFromFixture(*c.Thread), codexPriorForCase(t, c.Prior))
	if err != nil {
		t.Fatalf("MapCodexGraph: %v", err)
	}
	if c.Expected.Purpose != "" && string(mapping.Purpose) != c.Expected.Purpose {
		t.Errorf("purpose = %q, want %q", mapping.Purpose, c.Expected.Purpose)
	}
	if c.Expected.RootSessionID != "" {
		if mapping.RootSessionID == nil || string(*mapping.RootSessionID) != c.Expected.RootSessionID {
			t.Errorf("rootSessionID = %v, want %q", mapping.RootSessionID, c.Expected.RootSessionID)
		}
	}
	if c.Expected.ParentUUID != "" {
		if mapping.ParentUUID == nil || string(*mapping.ParentUUID) != c.Expected.ParentUUID {
			t.Errorf("parentUUID = %v, want %q", mapping.ParentUUID, c.Expected.ParentUUID)
		}
	}
	if c.Expected.Relationships != nil {
		want := make([]schema.SessionRelationship, 0, len(c.Expected.Relationships))
		for _, relation := range c.Expected.Relationships {
			want = append(want, codexRelationship(t, relation))
		}
		got := mapping.Relationships
		if len(want) == 0 {
			want = nil
		}
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("relationships = %+v, want %+v", mapping.Relationships, c.Expected.Relationships)
		}
	}
}

func TestCodexProvenanceFixtures(t *testing.T) {
	fixture := loadCodexProvenanceFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			if testCase.Thread != nil {
				codexAssertGraph(t, testCase)
				return
			}
			candidate := runCodexProvenanceCandidate(t, testCase)
			codexAssertCase(t, testCase, candidate)
		})
	}
}

// TestCodexCandidateSourceProofIsIndependent proves the stable thread, explicit
// root, generation identity and physical source incarnation stay separate
// facts: none is derived from another.
func TestCodexCandidateSourceProofIsIndependent(t *testing.T) {
	const (
		thread   = "11111111-1111-4111-8111-111111111111"
		root     = "22222222-2222-4222-8222-222222222222"
		physical = "phys-33333333-3333-4333-8333-333333333333"
		genID    = "gen-independent"
		pointer  = "/synthetic/current.jsonl"
	)
	source := &codexProvenanceSource{
		authority: ingest.CodexSourceAuthority{
			StableThreadID:   thread,
			Kind:             ingest.CodexAuthorityDetachedFile,
			CurrentPointer:   pointer,
			PhysicalSourceID: physical,
		},
		sources: map[string][]byte{pointer: []byte(strings.Join([]string{
			`{"type":"session_meta","payload":{"id":"` + thread + `","session_id":"` + root + `","thread_source":"user"}}`,
			`{"type":"response_item","payload":{"type":"message","role":"user","id":"u1","content":[{"type":"input_text","text":"hello"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}},"metadata":{"user_input_order":1}}`,
		}, "\n"))},
	}
	history, err := ingest.CaptureCodexHistory(t.Context(), source, ingest.DiscoveredSession{
		SessionID:  schema.SessionID(thread),
		Harness:    ingest.HarnessCodex,
		SourcePath: ingest.ResolvedPath(pointer),
	}, nil)
	if err != nil {
		t.Fatalf("CaptureCodexHistory: %v", err)
	}
	base := schema.NewUnifiedMetadata()
	base.SessionID = schema.SessionID(thread)
	base.ModelHarness = ingest.HarnessCodex
	candidate, err := ingest.BuildCodexCandidate(ingest.CodexCandidateInput{
		History:      history,
		Base:         base,
		PriorState:   ingest.NewProjectionPriorState(),
		Allocator:    &codexScriptedAllocator{entries: []string{"e_u1"}, submissions: []string{"s_u1"}},
		GenerationID: genID,
	})
	if err != nil {
		t.Fatalf("BuildCodexCandidate: %v", err)
	}
	if candidate.Proof.StableThreadID != thread {
		t.Errorf("stableThreadID = %q, want %q", candidate.Proof.StableThreadID, thread)
	}
	if candidate.Proof.GenerationID != genID {
		t.Errorf("generationID = %q, want %q", candidate.Proof.GenerationID, genID)
	}
	if candidate.Proof.PhysicalSourceID != physical {
		t.Errorf("physicalSourceID = %q, want %q", candidate.Proof.PhysicalSourceID, physical)
	}
	if candidate.Proof.Pointer != pointer {
		t.Errorf("pointer = %q, want %q", candidate.Proof.Pointer, pointer)
	}
	if candidate.V2.Generation.Metadata.SessionID != schema.SessionID(thread) {
		t.Errorf("session id = %q, want stable thread %q", candidate.V2.Generation.Metadata.SessionID, thread)
	}
	if candidate.V2.Generation.Metadata.RootSessionID == nil || string(*candidate.V2.Generation.Metadata.RootSessionID) != root {
		t.Errorf("root = %v, want explicit root %q distinct from the thread", candidate.V2.Generation.Metadata.RootSessionID, root)
	}
}

// TestCodexProvenanceIndexerAdmitsV2Candidate drives the real production
// indexer exit over an OS-backed rollout file: the candidate path returns the
// concrete format-2 result, keeps V1 outputs untouched while disabled, and
// leaves the native source bytes byte-identical.
func TestCodexProvenanceIndexerAdmitsV2Candidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2024-01-02T00-00-00-11111111-1111-4111-8111-111111111111.jsonl")
	content := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","history_mode":"legacy","thread_source":"user"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","id":"u1","content":[{"type":"input_text","text":"fix parser"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}},"metadata":{"user_input_order":1}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","id":"u2","content":[{"type":"input_text","text":"injected"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["agents_md.instructions"]}}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID("11111111-1111-4111-8111-111111111111"),
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	indexer := ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexProvenanceCapture(ingest.CodexProvenanceIndexerConfig{
		Enabled:      true,
		GenerationID: func(ingest.DiscoveredSession) string { return "gen-indexer" },
		Allocator:    &codexScriptedAllocator{entries: []string{"e_u1", "e_ctx1"}, submissions: []string{"s_u1"}},
	}))
	result, err := indexer.IndexTranscriptResult(t.Context(), session)
	if err != nil {
		t.Fatalf("IndexTranscriptResult: %v", err)
	}
	v2, ok := result.(indexformat.V2)
	if !ok {
		t.Fatalf("result = %T, want indexformat.V2", result)
	}
	if v2.Generation.Metadata.Stats.TurnCount != 2 {
		t.Errorf("turnCount = %d, want 2", v2.Generation.Metadata.Stats.TurnCount)
	}
	if v2.Generation.Metadata.Stats.InputSubmissionCount == nil || *v2.Generation.Metadata.Stats.InputSubmissionCount != 1 {
		t.Errorf("inputSubmissionCount = %v, want 1", v2.Generation.Metadata.Stats.InputSubmissionCount)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("native source bytes changed across a read-only candidate build")
	}
	// A disabled provenance path keeps the retained V1 entry result.
	retained := ingest.NewCodexIndexer(&ingest.OSFileSystem{})
	retainedResult, err := retained.IndexTranscriptResult(t.Context(), session)
	if err != nil {
		t.Fatalf("retained IndexTranscriptResult: %v", err)
	}
	if _, ok := retainedResult.(indexformat.V1); !ok {
		t.Fatalf("retained result = %T, want indexformat.V1", retainedResult)
	}
}
