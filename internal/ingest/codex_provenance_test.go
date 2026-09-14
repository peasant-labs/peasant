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
	"github.com/peasant-labs/peasant/internal/transcript"
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

// codexRetainedFixture is one inherited evidence block that must stay
// recoverable from the managed generation catalog without being emitted as a
// main or earlier conversational entry.
type codexRetainedFixture struct {
	Ref     string `yaml:"ref"`
	Content string `yaml:"content"`
	// SegmentInclusion names the captured context segment the retained ref must
	// be attached to, so the inherited evidence keeps its segment provenance.
	SegmentInclusion string `yaml:"segmentInclusion"`
}

// codexReferenceFixture is one declared native history dependency of the
// captured current source.
type codexReferenceFixture struct {
	Pointer          string `yaml:"pointer"`
	PhysicalSourceID string `yaml:"physicalSourceID"`
	Mode             string `yaml:"mode"`
	HistoryKind      string `yaml:"historyKind"`
	ThroughCompleted bool   `yaml:"throughCompleted"`
	Inclusion        string `yaml:"inclusion"`
	CoordinateKind   string `yaml:"coordinateKind"`
	Start            *int64 `yaml:"start"`
	EndExclusive     *int64 `yaml:"endExclusive"`
	LogicalSessionID string `yaml:"logicalSessionID"`
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
	Retained              []codexRetainedFixture     `yaml:"retained"`
	MetadataCWD           string                     `yaml:"metadataCWD"`
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
	Name             string                  `yaml:"name"`
	StableThreadID   string                  `yaml:"stableThreadID"`
	Pointer          string                  `yaml:"pointer"`
	PhysicalSourceID string                  `yaml:"physicalSourceID"`
	GenerationID     string                  `yaml:"generationID"`
	Completeness     string                  `yaml:"completeness"`
	HistoryMode      string                  `yaml:"historyMode"`
	CopyBoundary     *int64                  `yaml:"copyBoundary"`
	OwnershipProven  bool                    `yaml:"ownershipProven"`
	References       []codexReferenceFixture `yaml:"references"`
	Sources          map[string]string       `yaml:"sources"`
	Prior            *codexPriorFixture      `yaml:"prior"`
	Thread           *codexThreadFixture     `yaml:"thread"`
	EntryRefs        []string                `yaml:"entryRefs"`
	SubmissionRefs   []string                `yaml:"submissionRefs"`
	ExpectRefusal    bool                    `yaml:"expectRefusal"`
	PrivateSentinel  string                  `yaml:"privateSentinel"`
	MutationBarrier  bool                    `yaml:"mutationBarrier"`
	MutatedSources   map[string]string       `yaml:"mutatedSources"`
	Expected         codexExpectFixture      `yaml:"expected"`
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

func codexReferencesForCase(t *testing.T, fixtures []codexReferenceFixture) []ingest.CodexReference {
	t.Helper()
	references := make([]ingest.CodexReference, 0, len(fixtures))
	for _, fixture := range fixtures {
		inclusion, err := indexformat.NewSegmentInclusion(fixture.Inclusion)
		if err != nil {
			t.Fatal(err)
		}
		coordinateKind, err := indexformat.NewCoordinateKind(fixture.CoordinateKind)
		if err != nil {
			t.Fatal(err)
		}
		reference := ingest.CodexReference{
			Pointer:          fixture.Pointer,
			PhysicalSourceID: fixture.PhysicalSourceID,
			Mode:             ingest.CodexHistoryMode(fixture.Mode),
			HistoryKind:      fixture.HistoryKind,
			ThroughCompleted: fixture.ThroughCompleted,
			Inclusion:        inclusion,
			Coordinates: indexformat.SegmentCoordinates{
				Kind:         coordinateKind,
				Start:        fixture.Start,
				EndExclusive: fixture.EndExclusive,
			},
		}
		if fixture.LogicalSessionID != "" {
			id := schema.SessionID(fixture.LogicalSessionID)
			reference.LogicalSessionID = &id
		}
		references = append(references, reference)
	}
	return references
}

func codexAuthorityForCase(t *testing.T, c codexProvenanceCase) ingest.CodexSourceAuthority {
	t.Helper()
	kind, err := ingest.NewCodexSourceAuthorityKind(string(ingest.CodexAuthorityDetachedFile))
	if err != nil {
		t.Fatal(err)
	}
	return ingest.CodexSourceAuthority{
		StableThreadID:          c.StableThreadID,
		Kind:                    kind,
		CurrentPointer:          c.Pointer,
		PhysicalSourceID:        c.PhysicalSourceID,
		HistoryMode:             codexHistoryMode(t, c.HistoryMode),
		CopyBoundary:            c.CopyBoundary,
		OriginalOwnershipProven: c.OwnershipProven,
		References:              codexReferencesForCase(t, c.References),
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

func runCodexProvenanceCandidate(t *testing.T, c codexProvenanceCase) (ingest.CodexCandidate, error) {
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
		return ingest.CodexCandidate{}, err
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
	return candidate, nil
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
	codexAssertRetained(t, candidate, c.Expected.Retained)
}

// codexAssertRetained proves that inherited evidence stays recoverable from the
// managed generation catalog while never entering a main or earlier
// conversational partition. A retained ref must carry full content, must be
// attached to a captured context segment, and must not appear in either the
// main or the earlier entry streams. The retained set is derived from the
// generation itself, never from a bare count.
func codexAssertRetained(t *testing.T, candidate ingest.CodexCandidate, want []codexRetainedFixture) {
	t.Helper()
	generation := candidate.V2.Generation
	emitted := make(map[schema.SourceEntryRef]struct{}, len(generation.Main.Entries))
	for _, entry := range generation.Main.Entries {
		emitted[entry.SourceEntryRef] = struct{}{}
	}
	for i := range generation.Earlier {
		for _, entry := range generation.Earlier[i].Content.Entries {
			emitted[entry.SourceEntryRef] = struct{}{}
		}
	}
	segmentInclusions := make(map[schema.SourceEntryRef]map[indexformat.SegmentInclusion]struct{})
	for _, segment := range generation.Segments {
		for _, ref := range segment.CapturedRefs {
			if segmentInclusions[ref] == nil {
				segmentInclusions[ref] = make(map[indexformat.SegmentInclusion]struct{})
			}
			segmentInclusions[ref][segment.Inclusion] = struct{}{}
		}
	}
	retained := make(map[schema.SourceEntryRef]struct{})
	for _, record := range generation.Content {
		if _, ok := emitted[record.Ref]; !ok {
			retained[record.Ref] = struct{}{}
		}
	}
	if len(retained) != len(want) {
		t.Fatalf("retained refs = %v, want %d named retained entries", codexReferenceStrings(mapKeysOfSet(retained)), len(want))
	}
	for _, expected := range want {
		ref := schema.SourceEntryRef(expected.Ref)
		if _, ok := retained[ref]; !ok {
			t.Errorf("ref %q is not retained inherited evidence; inherited content would be lost", expected.Ref)
			continue
		}
		if _, ok := emitted[ref]; ok {
			t.Errorf("ref %q is emitted as a main or earlier entry; inherited content must not reach a conversational partition", expected.Ref)
		}
		inclusions := segmentInclusions[ref]
		if len(inclusions) == 0 {
			t.Errorf("retained ref %q is not attached to any captured context segment; the evidence is not recoverable", expected.Ref)
		} else if expected.SegmentInclusion != "" {
			if _, ok := inclusions[indexformat.SegmentInclusion(expected.SegmentInclusion)]; !ok {
				t.Errorf("retained ref %q is not captured by a %q segment; its segment provenance was lost", expected.Ref, expected.SegmentInclusion)
			}
		}
		if got := string(candidate.Content[ref]); got != expected.Content {
			t.Errorf("retained content for %q = %q, want %q", expected.Ref, got, expected.Content)
		}
	}
}

func mapKeysOfSet(set map[schema.SourceEntryRef]struct{}) []schema.SourceEntryRef {
	out := make([]schema.SourceEntryRef, 0, len(set))
	for ref := range set {
		out = append(out, ref)
	}
	slices.Sort(out)
	return out
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
			if testCase.MutationBarrier {
				codexAssertMetadataMutationBarrier(t, testCase)
				return
			}
			candidate, err := runCodexProvenanceCandidate(t, testCase)
			if testCase.ExpectRefusal {
				codexAssertCandidateRefusal(t, testCase, err)
				return
			}
			if err != nil {
				t.Fatalf("BuildCodexCandidate: %v", err)
			}
			codexAssertCase(t, testCase, candidate)
		})
	}
}

// codexAssertCandidateRefusal proves a candidate refusal is safe: it names its
// fixed operation, reason, effect and recovery, and never echoes a private
// native locator or a raw validator value.
func codexAssertCandidateRefusal(t *testing.T, c codexProvenanceCase, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("candidate for %q was accepted, want a safe refusal", c.Name)
	}
	if c.PrivateSentinel != "" && strings.Contains(err.Error(), c.PrivateSentinel) {
		t.Errorf("candidate refusal leaked the private locator %q: %v", c.PrivateSentinel, err)
	}
	if !strings.Contains(err.Error(), "no candidate was emitted") {
		t.Errorf("candidate refusal does not name its fixed effect category: %v", err)
	}
}

// codexMutatingFileSystem serves the verified G1 bytes for the captured path
// until the configured read, then serves G2. Every other path delegates to the
// real filesystem.
type codexMutatingFileSystem struct {
	ingest.FileSystem
	path     string
	g1       []byte
	g2       []byte
	reads    int
	mutateAt int
}

func (m *codexMutatingFileSystem) ReadFile(path string) ([]byte, error) {
	if filepath.Clean(path) != filepath.Clean(m.path) {
		return m.FileSystem.ReadFile(path)
	}
	m.reads++
	if m.mutateAt > 0 && m.reads >= m.mutateAt {
		return append([]byte(nil), m.g2...), nil
	}
	return append([]byte(nil), m.g1...), nil
}

// codexAssertMetadataMutationBarrier proves the candidate's metadata comes from
// the verified capture, never a reopened source: the source is replaced after
// the verified incarnation and the candidate must still carry the captured
// working directory, with exactly the capture's own read count and no second
// full read.
func codexAssertMetadataMutationBarrier(t *testing.T, c codexProvenanceCase) {
	t.Helper()
	if len(c.Sources) != 1 || len(c.MutatedSources) != 1 {
		t.Fatalf("mutation barrier case %q needs exactly one source and one mutated source", c.Name)
	}
	var g1, g2 []byte
	for _, data := range c.Sources {
		g1 = []byte(data)
	}
	for _, data := range c.MutatedSources {
		g2 = []byte(data)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2024-01-02T00-00-00-"+c.StableThreadID+".jsonl")
	if err := os.WriteFile(path, g1, 0o600); err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:  schema.SessionID(c.StableThreadID),
		Harness:    ingest.HarnessCodex,
		SourcePath: ingest.ResolvedPath(path),
	}
	probe := &codexMutatingFileSystem{FileSystem: &ingest.OSFileSystem{}, path: path, g1: g1, g2: g2}
	if _, err := ingest.CaptureCodexHistoryWithRetry(t.Context(), ingest.NewCodexFileSource(probe), session, nil); err != nil {
		t.Fatalf("probe capture: %v", err)
	}
	budget := probe.reads
	if budget == 0 {
		t.Fatalf("probe capture read the authoritative source zero times")
	}
	mutating := &codexMutatingFileSystem{FileSystem: &ingest.OSFileSystem{}, path: path, g1: g1, g2: g2, mutateAt: budget + 1}
	indexer := ingest.NewCodexIndexer(mutating)
	candidate, err := indexer.BuildCodexCandidateForSession(t.Context(), session, nil, ingest.NewProjectionPriorState(), &codexScriptedAllocator{entries: c.EntryRefs, submissions: c.SubmissionRefs}, c.GenerationID)
	if err != nil {
		t.Fatalf("BuildCodexCandidateForSession: %v", err)
	}
	if mutating.reads != budget {
		t.Fatalf("post-capture reads = %d, want the capture's %d; a second full read occurred after verification", mutating.reads, budget)
	}
	if candidate.V2.Generation.Metadata.CWD != c.Expected.MetadataCWD {
		t.Errorf("metadata cwd = %q, want the captured %q", candidate.V2.Generation.Metadata.CWD, c.Expected.MetadataCWD)
	}
	// The whole candidate, entries included, must be the captured incarnation.
	codexAssertCase(t, c, candidate)
}

// TestCodexInheritedPrefixConversionKeepsChildMain drives the same
// fixture-produced candidate through the real transcript conversion and title
// selection: the inherited copied prefix never reaches a rendered turn or a
// detail payload, and the surviving own input is the only title seed.
func TestCodexInheritedPrefixConversionKeepsChildMain(t *testing.T) {
	fixture := loadCodexProvenanceFixture(t)
	var testCase codexProvenanceCase
	for _, candidateCase := range fixture.Cases {
		if candidateCase.Name == "inherited-copied-prefix-retained" {
			testCase = candidateCase
			break
		}
	}
	if testCase.Name == "" {
		t.Fatal("fixture case inherited-copied-prefix-retained is missing")
	}
	candidate, err := runCodexProvenanceCandidate(t, testCase)
	if err != nil {
		t.Fatalf("BuildCodexCandidate: %v", err)
	}
	generation := candidate.V2.Generation
	turns := transcript.EntriesToTurns(generation.Main.Entries)
	if len(turns) != testCase.Expected.TurnCount {
		t.Fatalf("converted turns = %d, want %d", len(turns), testCase.Expected.TurnCount)
	}
	for _, turn := range turns {
		for _, retained := range testCase.Expected.Retained {
			if strings.Contains(turn.Content, retained.Content) {
				t.Errorf("rendered turn %d leaked inherited content %q", turn.Index, retained.Content)
			}
		}
	}
	if len(generation.TitleRefs) != 1 || string(generation.TitleRefs[0]) != "e_u1" {
		t.Errorf("title refs = %v, want the surviving own input e_u1", codexReferenceStrings(generation.TitleRefs))
	}
	detail := transcript.SessionToDetail(&ingest.Session{
		ID:      ingest.SessionID(generation.Metadata.SessionID),
		Harness: ingest.Harness(generation.Metadata.ModelHarness),
		Turns:   turns,
	})
	if detail == nil {
		t.Fatal("converted detail payload is nil")
	}
	if len(detail.Turns) != testCase.Expected.TurnCount {
		t.Errorf("detail turns = %d, want %d", len(detail.Turns), testCase.Expected.TurnCount)
	}
	for _, turn := range detail.Turns {
		for _, retained := range testCase.Expected.Retained {
			if strings.Contains(turn.Content, retained.Content) {
				t.Errorf("detail turn %d leaked inherited content %q", turn.Index, retained.Content)
			}
		}
	}
}

// codexFindProvenanceCase returns one named fixture case and fails when the
// manifest-guarded corpus does not carry it.
func codexFindProvenanceCase(t *testing.T, name string) codexProvenanceCase {
	t.Helper()
	for _, testCase := range loadCodexProvenanceFixture(t).Cases {
		if testCase.Name == name {
			return testCase
		}
	}
	t.Fatalf("fixture case %s is missing", name)
	return codexProvenanceCase{}
}

// TestCodexExactPastedWrapperConversionKeepsLiteralBodies drives the exact
// pasted wrapper three ways corpus through the real transcript conversion and
// detail construction. Literal wrapper-looking text is never reinterpreted:
// every main body survives with its role and stable ref, the injected
// instructions stay a system turn, the unknown-vector entry keeps its user
// turn, and only the eligible user submission remains a title seed.
func TestCodexExactPastedWrapperConversionKeepsLiteralBodies(t *testing.T) {
	testCase := codexFindProvenanceCase(t, "exact-pasted-wrapper-three-ways")
	candidate, err := runCodexProvenanceCandidate(t, testCase)
	if err != nil {
		t.Fatalf("BuildCodexCandidate: %v", err)
	}
	generation := candidate.V2.Generation
	turns := transcript.EntriesToTurns(generation.Main.Entries)
	if len(turns) != testCase.Expected.TurnCount {
		t.Fatalf("converted turns = %d, want %d", len(turns), testCase.Expected.TurnCount)
	}
	for i, want := range testCase.Expected.Main {
		if string(turns[i].Role) != want.Role {
			t.Errorf("turn %d role = %q, want %q", i, turns[i].Role, want.Role)
		}
		if turns[i].Content != want.Content {
			t.Errorf("turn %d content = %q, want the literal %q", i, turns[i].Content, want.Content)
		}
	}
	detail := transcript.SessionToDetail(&ingest.Session{
		ID:      ingest.SessionID(generation.Metadata.SessionID),
		Harness: ingest.Harness(generation.Metadata.ModelHarness),
		Turns:   turns,
	})
	if detail == nil {
		t.Fatal("converted detail payload is nil")
	}
	if len(detail.Turns) != testCase.Expected.TurnCount {
		t.Fatalf("detail turns = %d, want %d", len(detail.Turns), testCase.Expected.TurnCount)
	}
	for i, want := range testCase.Expected.Main {
		if string(detail.Turns[i].Role) != want.Role {
			t.Errorf("detail turn %d role = %q, want %q", i, detail.Turns[i].Role, want.Role)
		}
		if detail.Turns[i].Content != want.Content {
			t.Errorf("detail turn %d content = %q, want the literal %q", i, detail.Turns[i].Content, want.Content)
		}
	}
	if len(generation.TitleRefs) != 1 || string(generation.TitleRefs[0]) != "e_wrap1" {
		t.Errorf("title refs = %v, want the eligible user submission e_wrap1", codexReferenceStrings(generation.TitleRefs))
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
