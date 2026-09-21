package ingest_test

// Fixture-backed validation for the read-only Codex current-history capture and
// replay. Every case drives the real production capture over sanitized native
// byte shapes and asserts the captured native node graph, ordered segments,
// ownership, correlations and completeness.

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
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/codex_history_projection.yaml
var codexHistoryProjectionYAML []byte

//go:embed testdata/codex_history_projection.manifest.yaml
var codexHistoryProjectionManifestYAML []byte

type codexHistoryReferenceFixture struct {
	Pointer                 string `yaml:"pointer"`
	PhysicalSourceID        string `yaml:"physicalSourceID"`
	Mode                    string `yaml:"mode"`
	Inclusion               string `yaml:"inclusion"`
	CoordinateKind          string `yaml:"coordinateKind"`
	HistoryKind             string `yaml:"historyKind"`
	ThroughCompleted        bool   `yaml:"throughCompleted"`
	Start                   *int64 `yaml:"start"`
	EndExclusive            *int64 `yaml:"endExclusive"`
	DecodedByteStart        *int64 `yaml:"decodedByteStart"`
	DecodedByteEndExclusive *int64 `yaml:"decodedByteEndExclusive"`
	CopyBoundary            *int64 `yaml:"copyBoundary"`
	OriginalOwnershipProven bool   `yaml:"originalOwnershipProven"`
	LogicalSessionID        string `yaml:"logicalSessionID"`
}

type codexHistoryAuthorityFixture struct {
	StableThreadID          string                         `yaml:"stableThreadID"`
	Kind                    string                         `yaml:"kind"`
	CurrentPointer          string                         `yaml:"currentPointer"`
	PhysicalSourceID        string                         `yaml:"physicalSourceID"`
	HistoryModeRaw          string                         `yaml:"historyModeRaw"`
	CopyBoundary            *int64                         `yaml:"copyBoundary"`
	OriginalOwnershipProven bool                           `yaml:"originalOwnershipProven"`
	References              []codexHistoryReferenceFixture `yaml:"references"`
}

type codexHistoryNodeExpected struct {
	Ref          string `yaml:"ref"`
	NativeKey    string `yaml:"nativeKey"`
	Segment      *int   `yaml:"segment"`
	Ordinal      *int64 `yaml:"ordinal"`
	Line         *int64 `yaml:"line"`
	EnvelopeType string `yaml:"envelopeType"`
	NativeType   string `yaml:"nativeType"`
	NativeRole   string `yaml:"nativeRole"`
	ItemID       string `yaml:"itemID"`
	Ownership    string `yaml:"ownership"`
	HasPayload   *bool  `yaml:"hasPayload"`
	HasMetadata  *bool  `yaml:"hasMetadata"`
	Payload      string `yaml:"payload"`
	Metadata     string `yaml:"metadata"`
}

type codexHistorySegmentExpected struct {
	Ordinal                 int      `yaml:"ordinal"`
	Inclusion               string   `yaml:"inclusion"`
	CoordinateKind          string   `yaml:"coordinateKind"`
	Start                   *int64   `yaml:"start"`
	EndExclusive            *int64   `yaml:"endExclusive"`
	DecodedByteStart        *int64   `yaml:"decodedByteStart"`
	DecodedByteEndExclusive *int64   `yaml:"decodedByteEndExclusive"`
	Refs                    []string `yaml:"refs"`
}

type codexHistoryCorrelationExpected struct {
	Kind            string   `yaml:"kind"`
	ItemID          string   `yaml:"itemID"`
	EventOrdinal    *int64   `yaml:"eventOrdinal"`
	ResponseOrdinal *int64   `yaml:"responseOrdinal"`
	Refs            []string `yaml:"refs"`
}

type codexHistoryExpected struct {
	Mode             string   `yaml:"mode"`
	Completeness     string   `yaml:"completeness"`
	MainRefs         []string `yaml:"mainRefs"`
	InheritedRefs    []string `yaml:"inheritedRefs"`
	EarlierRefs      []string `yaml:"earlierRefs"`
	RevertedRefs     []string `yaml:"revertedRefs"`
	CheckpointRefs   []string `yaml:"checkpointRefs"`
	CorrelationKinds []string `yaml:"correlationKinds"`
	Diagnostics      []string `yaml:"diagnostics"`
	// Nodes, Segments and Correlations assert the exact captured native
	// evidence handed downstream: identity, coordinates, ownership and
	// correlation endpoints in replay order. A nil section is not asserted;
	// an explicit empty list asserts emptiness.
	Nodes        []codexHistoryNodeExpected        `yaml:"nodes"`
	Segments     []codexHistorySegmentExpected     `yaml:"segments"`
	Correlations []codexHistoryCorrelationExpected `yaml:"correlations"`
	// FingerprintStableWithPrevious proves a parent append past the captured
	// cutoff does not change the child fingerprint.
	FingerprintStableWithPrevious bool `yaml:"fingerprintStableWithPrevious"`
}

type codexHistoryPhase struct {
	Authority codexHistoryAuthorityFixture `yaml:"authority"`
	Sources   map[string]string            `yaml:"sources"`
	Expected  codexHistoryExpected         `yaml:"expected"`
}

type codexHistoryCase struct {
	Name     string              `yaml:"name"`
	Unstable bool                `yaml:"unstable"`
	Phases   []codexHistoryPhase `yaml:"phases"`
}

type codexHistoryProjectionFixture struct {
	Cases []codexHistoryCase `yaml:"cases"`
}

func decodeCodexHistoryProjectionFixture(data []byte) (codexHistoryProjectionFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture codexHistoryProjectionFixture
	if err := decoder.Decode(&fixture); err != nil {
		return codexHistoryProjectionFixture{}, fmt.Errorf("decode codex history projection fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return codexHistoryProjectionFixture{}, fmt.Errorf("codex history projection fixture must contain exactly one YAML document: %v", err)
	}
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.Name) == "" {
			return codexHistoryProjectionFixture{}, fmt.Errorf("codex history projection fixture has an empty case name")
		}
		if c.Unstable {
			continue
		}
		if len(c.Phases) == 0 {
			return codexHistoryProjectionFixture{}, fmt.Errorf("codex history projection case %q has no phases", c.Name)
		}
	}
	return fixture, nil
}

func loadCodexHistoryProjectionFixture(t *testing.T) codexHistoryProjectionFixture {
	t.Helper()
	fixture, err := decodeCodexHistoryProjectionFixture(codexHistoryProjectionYAML)
	if err != nil {
		t.Fatalf("load codex history projection fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(codexHistoryProjectionManifestYAML, "codex history projection")
	if err != nil {
		t.Fatalf("load codex history projection manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "codex history projection"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// fixtureCodexSource is a deterministic CodexReadOnlySource. It is a test
// double for the native/filesystem source the production code depends on, not
// a replacement for the capture itself.
type fixtureCodexSource struct {
	authority ingest.CodexSourceAuthority
	sources   map[string][]byte
	fail      map[string]bool
	calls     int
	unstable  bool
}

func (s *fixtureCodexSource) ResolveCodexAuthority(context.Context, ingest.DiscoveredSession) (ingest.CodexSourceAuthority, error) {
	return s.authority, nil
}

func (s *fixtureCodexSource) ReadCodexSource(_ context.Context, pointer string) ([]byte, error) {
	s.calls++
	if s.unstable {
		// Every read returns a different bounded prefix so the stability
		// recheck always observes a change.
		return []byte(fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[],\"id\":\"m%d\"}}\n", s.calls)), nil
	}
	if s.fail[pointer] {
		return nil, fmt.Errorf("synthetic missing native source %q", pointer)
	}
	data, ok := s.sources[pointer]
	if !ok {
		return nil, fmt.Errorf("fixture has no bytes for pointer %q", pointer)
	}
	return data, nil
}

func codexHistoryAuthorityFromFixture(t *testing.T, f codexHistoryAuthorityFixture) ingest.CodexSourceAuthority {
	t.Helper()
	kind, err := ingest.NewCodexSourceAuthorityKind(f.Kind)
	if err != nil {
		t.Fatal(err)
	}
	authority := ingest.CodexSourceAuthority{
		StableThreadID:          f.StableThreadID,
		Kind:                    kind,
		CurrentPointer:          f.CurrentPointer,
		PhysicalSourceID:        f.PhysicalSourceID,
		HistoryMode:             codexRawHistoryMode(t, f.HistoryModeRaw),
		CopyBoundary:            f.CopyBoundary,
		OriginalOwnershipProven: f.OriginalOwnershipProven,
	}
	for _, ref := range f.References {
		mode := ingest.CodexHistoryMode(ref.Mode)
		if !mode.IsValid() {
			t.Fatalf("reference mode %q is outside the closed history-mode set", ref.Mode)
		}
		inclusion, err := indexformat.NewSegmentInclusion(ref.Inclusion)
		if err != nil {
			t.Fatal(err)
		}
		coordinateKind, err := indexformat.NewCoordinateKind(ref.CoordinateKind)
		if err != nil {
			t.Fatal(err)
		}
		reference := ingest.CodexReference{
			Pointer:                 ref.Pointer,
			PhysicalSourceID:        ref.PhysicalSourceID,
			Mode:                    mode,
			HistoryKind:             ref.HistoryKind,
			ThroughCompleted:        ref.ThroughCompleted,
			Inclusion:               inclusion,
			CopyBoundary:            ref.CopyBoundary,
			OriginalOwnershipProven: ref.OriginalOwnershipProven,
			Coordinates: indexformat.SegmentCoordinates{
				Kind:                    coordinateKind,
				Start:                   ref.Start,
				EndExclusive:            ref.EndExclusive,
				DecodedByteStart:        ref.DecodedByteStart,
				DecodedByteEndExclusive: ref.DecodedByteEndExclusive,
			},
		}
		if ref.LogicalSessionID != "" {
			sid, err := ingest.NewSessionID(ref.LogicalSessionID)
			if err != nil {
				t.Fatal(err)
			}
			reference.LogicalSessionID = &sid
		}
		authority.References = append(authority.References, reference)
	}
	return authority
}

func codexRawHistoryMode(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	switch raw {
	case "":
		return nil
	case "null":
		return json.RawMessage("null")
	default:
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
}

func codexFixtureRefs(t *testing.T, refs []schema.SourceEntryRef) []string {
	t.Helper()
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, string(ref))
	}
	return out
}

func codexCorrelationKinds(correlations []ingest.CodexCapturedCorrelation) []string {
	seen := map[string]struct{}{}
	for _, correlation := range correlations {
		seen[string(correlation.Kind)] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for kind := range seen {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func codexDiagnosticTypes(diagnostics []schema.DiagnosticEntry) []string {
	seen := map[string]struct{}{}
	for _, diagnostic := range diagnostics {
		seen[diagnostic.ErrorType] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for kind := range seen {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func codexAssertExpected(t *testing.T, want codexHistoryExpected, history ingest.CodexCapturedHistory) {
	t.Helper()
	if want.Mode != "" && string(history.Mode) != want.Mode {
		t.Errorf("mode = %q, want %q", history.Mode, want.Mode)
	}
	if want.Completeness != "" && string(history.Completeness) != want.Completeness {
		t.Errorf("completeness = %q, want %q", history.Completeness, want.Completeness)
	}
	checks := []struct {
		label string
		want  []string
		got   []string
	}{
		{"mainRefs", want.MainRefs, codexFixtureRefs(t, history.MainRefs)},
		{"inheritedRefs", want.InheritedRefs, codexFixtureRefs(t, history.InheritedRefs)},
		{"earlierRefs", want.EarlierRefs, codexFixtureRefs(t, history.EarlierRefs)},
		{"revertedRefs", want.RevertedRefs, codexFixtureRefs(t, history.RevertedRefs)},
		{"checkpointRefs", want.CheckpointRefs, codexFixtureRefs(t, history.CheckpointRefs)},
		{"correlationKinds", want.CorrelationKinds, codexCorrelationKinds(history.Correlations)},
	}
	for _, check := range checks {
		if !slices.Equal(normalizeRefs(check.want), normalizeRefs(check.got)) {
			t.Errorf("%s = %v, want %v", check.label, check.got, check.want)
		}
	}
	if want.Diagnostics != nil {
		if got := codexDiagnosticTypes(history.Diagnostics); !slices.Equal(want.Diagnostics, got) {
			t.Errorf("diagnostics = %v, want %v", got, want.Diagnostics)
		}
	}
	if want.Nodes != nil {
		codexAssertNodes(t, want.Nodes, history.Nodes)
	}
	if want.Segments != nil {
		codexAssertSegments(t, want.Segments, history.Segments)
	}
	if want.Correlations != nil {
		codexAssertCorrelations(t, want.Correlations, history.Correlations)
	}
	if want.Nodes != nil || want.Segments != nil {
		codexAssertCapturedSegments(t, history)
	}
}

func codexAssertNodes(t *testing.T, want []codexHistoryNodeExpected, got []ingest.CodexCapturedNode) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("nodes = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if string(g.Ref) != w.Ref {
			t.Errorf("nodes[%d].ref = %q, want %q", i, g.Ref, w.Ref)
		}
		if w.NativeKey != "" && g.NativeKey != w.NativeKey {
			t.Errorf("nodes[%d].nativeKey = %q, want %q", i, g.NativeKey, w.NativeKey)
		}
		if w.Segment != nil && g.SegmentOrdinal != *w.Segment {
			t.Errorf("nodes[%d].segment = %d, want %d", i, g.SegmentOrdinal, *w.Segment)
		}
		if w.Ordinal != nil && g.Ordinal != *w.Ordinal {
			t.Errorf("nodes[%d].ordinal = %d, want %d", i, g.Ordinal, *w.Ordinal)
		}
		if w.Line != nil && g.LineIndex != *w.Line {
			t.Errorf("nodes[%d].line = %d, want %d", i, g.LineIndex, *w.Line)
		}
		if w.EnvelopeType != "" && g.EnvelopeType != w.EnvelopeType {
			t.Errorf("nodes[%d].envelopeType = %q, want %q", i, g.EnvelopeType, w.EnvelopeType)
		}
		if w.NativeType != "" && g.NativeType != w.NativeType {
			t.Errorf("nodes[%d].nativeType = %q, want %q", i, g.NativeType, w.NativeType)
		}
		if w.NativeRole != "" && g.NativeRole != w.NativeRole {
			t.Errorf("nodes[%d].nativeRole = %q, want %q", i, g.NativeRole, w.NativeRole)
		}
		if w.ItemID != "" && g.ItemID != w.ItemID {
			t.Errorf("nodes[%d].itemID = %q, want %q", i, g.ItemID, w.ItemID)
		}
		if w.Ownership != "" && string(g.Ownership) != w.Ownership {
			t.Errorf("nodes[%d].ownership = %q, want %q", i, g.Ownership, w.Ownership)
		}
		if w.HasPayload != nil && (len(g.Payload) > 0) != *w.HasPayload {
			t.Errorf("nodes[%d].hasPayload = %t, want %t", i, len(g.Payload) > 0, *w.HasPayload)
		}
		if w.HasMetadata != nil && (len(bytes.TrimSpace(g.Metadata)) > 0) != *w.HasMetadata {
			t.Errorf("nodes[%d].hasMetadata = %t, want %t", i, len(bytes.TrimSpace(g.Metadata)) > 0, *w.HasMetadata)
		}
		if w.Payload != "" && string(g.Payload) != w.Payload {
			t.Errorf("nodes[%d].payload = %s, want %s", i, g.Payload, w.Payload)
		}
		if w.Metadata != "" && string(g.Metadata) != w.Metadata {
			t.Errorf("nodes[%d].metadata = %s, want %s", i, g.Metadata, w.Metadata)
		}
	}
}

func codexAssertSegments(t *testing.T, want []codexHistorySegmentExpected, got []indexformat.ContextSegment) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("segments = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Ordinal != w.Ordinal {
			t.Errorf("segments[%d].ordinal = %d, want %d", i, g.Ordinal, w.Ordinal)
		}
		if w.Inclusion != "" && string(g.Inclusion) != w.Inclusion {
			t.Errorf("segments[%d].inclusion = %q, want %q", i, g.Inclusion, w.Inclusion)
		}
		if w.CoordinateKind != "" && string(g.Coordinates.Kind) != w.CoordinateKind {
			t.Errorf("segments[%d].coordinateKind = %q, want %q", i, g.Coordinates.Kind, w.CoordinateKind)
		}
		if w.Start != nil && (g.Coordinates.Start == nil || *g.Coordinates.Start != *w.Start) {
			t.Errorf("segments[%d].start = %v, want %d", i, g.Coordinates.Start, *w.Start)
		}
		if w.EndExclusive != nil && (g.Coordinates.EndExclusive == nil || *g.Coordinates.EndExclusive != *w.EndExclusive) {
			t.Errorf("segments[%d].endExclusive = %v, want %d", i, g.Coordinates.EndExclusive, *w.EndExclusive)
		}
		if w.DecodedByteStart != nil && (g.Coordinates.DecodedByteStart == nil || *g.Coordinates.DecodedByteStart != *w.DecodedByteStart) {
			t.Errorf("segments[%d].decodedByteStart = %v, want %d", i, g.Coordinates.DecodedByteStart, *w.DecodedByteStart)
		}
		if w.DecodedByteEndExclusive != nil && (g.Coordinates.DecodedByteEndExclusive == nil || *g.Coordinates.DecodedByteEndExclusive != *w.DecodedByteEndExclusive) {
			t.Errorf("segments[%d].decodedByteEndExclusive = %v, want %d", i, g.Coordinates.DecodedByteEndExclusive, *w.DecodedByteEndExclusive)
		}
		if w.Refs != nil && !slices.Equal(normalizeRefs(codexFixtureRefs(t, g.CapturedRefs)), normalizeRefs(w.Refs)) {
			t.Errorf("segments[%d].refs = %v, want %v", i, codexFixtureRefs(t, g.CapturedRefs), w.Refs)
		}
	}
}

func codexAssertCorrelations(t *testing.T, want []codexHistoryCorrelationExpected, got []ingest.CodexCapturedCorrelation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("correlations = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if string(g.Kind) != w.Kind {
			t.Errorf("correlations[%d].kind = %q, want %q", i, g.Kind, w.Kind)
		}
		if w.ItemID != "" && g.ItemID != w.ItemID {
			t.Errorf("correlations[%d].itemID = %q, want %q", i, g.ItemID, w.ItemID)
		}
		if w.EventOrdinal != nil && (g.EventOrdinal == nil || *g.EventOrdinal != *w.EventOrdinal) {
			t.Errorf("correlations[%d].eventOrdinal = %v, want %d", i, g.EventOrdinal, *w.EventOrdinal)
		}
		if w.ResponseOrdinal != nil && (g.ResponseOrdinal == nil || *g.ResponseOrdinal != *w.ResponseOrdinal) {
			t.Errorf("correlations[%d].responseOrdinal = %v, want %d", i, g.ResponseOrdinal, *w.ResponseOrdinal)
		}
		if w.Refs != nil && !slices.Equal(normalizeRefs(codexFixtureRefs(t, g.Refs)), normalizeRefs(w.Refs)) {
			t.Errorf("correlations[%d].refs = %v, want %v", i, codexFixtureRefs(t, g.Refs), w.Refs)
		}
	}
}

// codexAssertCapturedSegments proves the handoff carries every captured
// segment payload: one captured segment per projected segment, bounded bytes
// present exactly when the segment proves valid records, and per-record
// payload plus envelope metadata preserved without reopening the source.
func codexAssertCapturedSegments(t *testing.T, history ingest.CodexCapturedHistory) {
	t.Helper()
	if len(history.CapturedSegments) != len(history.Segments) {
		t.Fatalf("captured segments = %d, want %d (one per projected segment)", len(history.CapturedSegments), len(history.Segments))
	}
	for i, captured := range history.CapturedSegments {
		if captured.Ordinal != history.Segments[i].Ordinal {
			t.Errorf("captured segments[%d].ordinal = %d, want %d", i, captured.Ordinal, history.Segments[i].Ordinal)
		}
		hasValid := false
		for _, record := range captured.Records {
			if record.DecodedOrdinal != nil {
				hasValid = true
				break
			}
		}
		if hasValid && len(captured.Data) == 0 {
			t.Errorf("captured segments[%d] proves valid records but carries no bounded bytes", i)
		}
	}
	for _, node := range history.Nodes {
		if len(node.Payload) == 0 && len(node.RetainedUnknown) == 0 {
			t.Errorf("node %s carries no captured payload; the classifier would have to reopen the source", node.Ref)
		}
	}
}

func normalizeRefs(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func TestCodexHistoryProjectionFixtures(t *testing.T) {
	fixture := loadCodexHistoryProjectionFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			if testCase.Unstable {
				testCodexUnstableCapture(t, testCase)
				return
			}
			allocator := func(index int) schema.SourceEntryRef {
				return schema.SourceEntryRef(fmt.Sprintf("e_%d", index+1))
			}
			registry := ingest.NewCodexRefRegistry(allocator)
			var previousFingerprint string
			for phaseIndex, phase := range testCase.Phases {
				source := &fixtureCodexSource{
					authority: codexHistoryAuthorityFromFixture(t, phase.Authority),
					sources:   map[string][]byte{},
				}
				for pointer, data := range phase.Sources {
					source.sources[pointer] = []byte(data)
				}
				session := ingest.DiscoveredSession{
					SessionID:  schema.SessionID(phase.Authority.StableThreadID),
					Harness:    ingest.HarnessCodex,
					SourcePath: "/synthetic/current.jsonl",
				}
				history, err := ingest.CaptureCodexHistory(t.Context(), source, session, registry)
				if err != nil {
					t.Fatalf("phase %d: CaptureCodexHistory: %v", phaseIndex, err)
				}
				t.Run(fmt.Sprintf("phase-%d", phaseIndex), func(t *testing.T) {
					codexAssertExpected(t, phase.Expected, history)
					if phase.Expected.FingerprintStableWithPrevious {
						if previousFingerprint == "" {
							t.Fatal("fingerprint stability asserted without a previous phase")
						}
						if history.Fingerprint != previousFingerprint {
							t.Fatalf("fingerprint = %s, want previous %s after a parent append past the captured cutoff", history.Fingerprint, previousFingerprint)
						}
					}
				})
				previousFingerprint = history.Fingerprint
			}
		})
	}
}

func testCodexUnstableCapture(t *testing.T, testCase codexHistoryCase) {
	t.Helper()
	source := &fixtureCodexSource{
		authority: ingest.CodexSourceAuthority{
			StableThreadID:   testCase.Phases[0].Authority.StableThreadID,
			Kind:             ingest.CodexAuthorityDetachedFile,
			CurrentPointer:   "current",
			PhysicalSourceID: "phys-current",
		},
		sources:  map[string][]byte{},
		unstable: true,
	}
	session := ingest.DiscoveredSession{
		SessionID:  schema.SessionID(testCase.Phases[0].Authority.StableThreadID),
		Harness:    ingest.HarnessCodex,
		SourcePath: "/synthetic/current.jsonl",
	}
	_, err := ingest.CaptureCodexHistoryWithRetry(t.Context(), source, session, nil)
	var changed *ingest.CodexSourceChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("unstable source error = %v, want *CodexSourceChangedError", err)
	}
	if changed.Attempts != 3 {
		t.Fatalf("unstable source attempts = %d, want 3", changed.Attempts)
	}
}

// boundedOnlyCodexFS proves the production indexer reads the Codex current
// source through the bounded capture path: ReadFile is refused, so only the
// capture's header and bounded-prefix reads can succeed.
type boundedOnlyCodexFS struct {
	*ingest.OSFileSystem
}

var _ ingest.FileSystem = boundedOnlyCodexFS{}

func (boundedOnlyCodexFS) ReadFile(path string) ([]byte, error) {
	return nil, fmt.Errorf("ReadFile(%q) must not be used: the Codex current source has to be read through the bounded capture path", path)
}

// TestCodexCaptureFileSourceProductionPath drives the real OS-backed file
// source through the production indexer: authority selection, bounded read and
// fingerprint recheck all run against a real rollout file.
func TestCodexCaptureFileSourceProductionPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2024-01-02T00-00-00-11111111-1111-4111-8111-111111111111.jsonl")
	content := strings.Join([]string{
		`{"timestamp":"2024-01-02T00:00:00Z","type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","history_mode":"paginated"}}`,
		`{"timestamp":"2024-01-02T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","id":"m1","content":[{"type":"input_text","text":"hello"}]}}`,
		`{"timestamp":"2024-01-02T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"m2","content":[{"type":"output_text","text":"hi"}]}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID("11111111-1111-4111-8111-111111111111"),
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	indexer := ingest.NewCodexIndexer(boundedOnlyCodexFS{OSFileSystem: &ingest.OSFileSystem{}}, ingest.WithCodexHistoryCapture(true))
	result, err := indexer.IndexTranscriptResult(t.Context(), session)
	if err != nil {
		t.Fatalf("IndexTranscriptResult: %v", err)
	}
	entries := result.(indexformat.V1).Entries
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 through the OS-backed capture source", len(entries))
	}
}
