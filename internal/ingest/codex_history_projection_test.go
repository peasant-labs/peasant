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
	Start                   *int64 `yaml:"start"`
	EndExclusive            *int64 `yaml:"endExclusive"`
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
		return []byte(fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":\"m%d\"}}\n", s.calls)), nil
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
			Inclusion:               inclusion,
			CopyBoundary:            ref.CopyBoundary,
			OriginalOwnershipProven: ref.OriginalOwnershipProven,
			Coordinates: indexformat.SegmentCoordinates{
				Kind:         coordinateKind,
				Start:        ref.Start,
				EndExclusive: ref.EndExclusive,
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
	indexer := ingest.NewCodexIndexer(boundedOnlyCodexFS{OSFileSystem: &ingest.OSFileSystem{}})
	result, err := indexer.IndexTranscriptResult(t.Context(), session)
	if err != nil {
		t.Fatalf("IndexTranscriptResult: %v", err)
	}
	entries := result.(indexformat.V1).Entries
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 through the OS-backed capture source", len(entries))
	}
}
