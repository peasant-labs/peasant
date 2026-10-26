package ingest_test

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/claude_producer_stability.yaml
var claudeProducerStabilityYAML []byte

//go:embed testdata/claude_producer_stability.manifest.yaml
var claudeProducerStabilityManifestYAML []byte

type claudeStabilityCase struct {
	Name string `yaml:"name"`
	// First and Second are the two attempts' captured bytes. The second is
	// built with the first generation's own alias state as its prior, exactly
	// as a refresh does.
	First  string `yaml:"first"`
	Second string `yaml:"second"`
	// UnchangedKeys must keep their exact ref across both builds.
	UnchangedKeys []string `yaml:"unchangedKeys"`
	// AddedKeys appear only in the second build and must allocate fresh refs.
	AddedKeys []string `yaml:"addedKeys"`
	// RemovedKeys exist only in the first build.
	RemovedKeys []string `yaml:"removedKeys"`

	FirstSubmissionCount  int `yaml:"firstSubmissionCount"`
	SecondSubmissionCount int `yaml:"secondSubmissionCount"`
	// SecondDistinctSubmissionRefs requires the second build's submission refs
	// to be pairwise distinct, so a repeated-uuid group never collapses.
	SecondDistinctSubmissionRefs bool `yaml:"secondDistinctSubmissionRefs"`
}

type claudeStabilityDocument struct {
	Cases []claudeStabilityCase `yaml:"cases"`
}

func loadClaudeStabilityCases(t *testing.T) []claudeStabilityCase {
	t.Helper()
	var document claudeStabilityDocument
	if err := testutil.DecodeFixtureYAML(claudeProducerStabilityYAML, &document); err != nil {
		t.Fatalf("decode claude producer stability fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(claudeProducerStabilityManifestYAML, "claude producer stability")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "claude producer stability"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// TestClaudeProducerStabilityFixture builds the same session twice -- the
// second attempt over the first generation's persisted alias state -- and
// asserts that unchanged source keeps its identity, the appended boundary
// allocates, and a re-keying refresh or a per-attempt scheme would fail.
func TestClaudeProducerStabilityFixture(t *testing.T) {
	t.Parallel()
	for _, row := range loadClaudeStabilityCases(t) {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			runClaudeStabilityCase(t, row)
		})
	}
}

func runClaudeStabilityCase(t *testing.T, row claudeStabilityCase) {
	t.Helper()
	first := buildClaudeStabilityCandidate(t, row.Name+"-first", row.First, ingest.ProjectionPriorState{})
	firstRefs, _ := claudeAliasIndex(t, first)
	firstCount := claudeSubmissionCount(t, first)
	if firstCount != row.FirstSubmissionCount {
		t.Fatalf("first inputSubmissionCount = %d, want %d", firstCount, row.FirstSubmissionCount)
	}

	prior, err := ingest.PriorStateFromGeneration(first.Result.Generation)
	if err != nil {
		t.Fatalf("PriorStateFromGeneration: %v", err)
	}
	second := buildClaudeStabilityCandidate(t, row.Name+"-second", row.Second, prior)
	secondRefs, secondSubmissions := claudeAliasIndex(t, second)
	secondCount := claudeSubmissionCount(t, second)
	if secondCount != row.SecondSubmissionCount {
		t.Fatalf("second inputSubmissionCount = %d, want %d", secondCount, row.SecondSubmissionCount)
	}

	for _, key := range row.UnchangedKeys {
		if firstRefs[key] == "" || secondRefs[key] == "" {
			t.Fatalf("key %q is missing from one of the builds", key)
		}
		if firstRefs[key] != secondRefs[key] {
			t.Errorf("key %q ref changed across an unchanged source: %q -> %q", key, firstRefs[key], secondRefs[key])
		}
	}
	for _, key := range row.AddedKeys {
		if firstRefs[key] != "" {
			t.Errorf("key %q was already present in the first build", key)
		}
		if secondRefs[key] == "" {
			t.Fatalf("added key %q is missing from the second build", key)
		}
	}
	for _, key := range row.RemovedKeys {
		if secondRefs[key] != "" {
			t.Errorf("key %q is still present in the second build", key)
		}
	}
	if row.SecondDistinctSubmissionRefs {
		seen := make(map[schema.SubmissionRef]string, len(secondSubmissions))
		for key, ref := range secondSubmissions {
			if ref == "" {
				t.Fatalf("submission key %q has no submission ref", key)
			}
			if other, duplicate := seen[ref]; duplicate {
				t.Errorf("submission keys %q and %q share the submission ref %q", other, key, ref)
			}
			seen[ref] = key
		}
	}
}

func buildClaudeStabilityCandidate(t *testing.T, generationID, transcript string, prior ingest.ProjectionPriorState) ingest.GenerationCandidate {
	t.Helper()
	mfs := testutil.NewMemFS()
	sessionID := testutil.TestSessionUUID
	sourcePath := "/native/" + sessionID + ".jsonl"
	metadata := makeReindexMeta(t, sessionID, sourcePath)
	idx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true), ingest.WithClaudeFullContent(true))
	var priorSource ingest.GenerationPriorSource
	if prior.Entries != nil {
		priorSource = generationTestPriorSource{prior: &ingest.GenerationPrior{GenerationID: "gen-prior", Aliases: prior}}
	}
	candidate, err := idx.BuildGeneration(t.Context(), ingest.GenerationBuildInput{
		Session: ingest.DiscoveredSession{
			SessionID:    ingest.SessionID(sessionID),
			Harness:      ingest.HarnessClaudeCode,
			SourceFormat: ingest.SourceFormatJSONL,
			SourcePath:   ingest.ResolvedPath(sourcePath),
		},
		GenerationID:       generationID,
		RecordedMetadata:   metadata,
		CapturedTranscript: []byte(transcript),
		Prior:              priorSource,
	})
	if err != nil {
		t.Fatalf("BuildGeneration: %v", err)
	}
	return candidate
}

// claudeAliasIndex returns each block native key's ref and each submission
// key's submission ref from the built generation's persisted aliases.
func claudeAliasIndex(t *testing.T, candidate ingest.GenerationCandidate) (map[string]schema.SourceEntryRef, map[string]schema.SubmissionRef) {
	t.Helper()
	generation := candidate.Result.Generation
	byRef := make(map[schema.SourceEntryRef]*schema.ContentProvenance)
	for i := range generation.Main.Entries {
		entry := generation.Main.Entries[i]
		if entry.SourceEntryRef != "" && entry.Provenance != nil {
			byRef[entry.SourceEntryRef] = entry.Provenance
		}
	}
	refs := make(map[string]schema.SourceEntryRef)
	submissions := make(map[string]schema.SubmissionRef)
	for _, alias := range generation.Aliases {
		switch {
		case strings.HasPrefix(alias.NativeKey, "block:"):
			refs[strings.TrimPrefix(alias.NativeKey, "block:")] = alias.Ref
		case strings.HasPrefix(alias.NativeKey, "accept:"):
			key := strings.TrimPrefix(alias.NativeKey, "accept:")
			if provenance := byRef[alias.Ref]; provenance != nil {
				submissions[key] = provenance.SubmissionRef
			}
		}
	}
	return refs, submissions
}

func claudeSubmissionCount(t *testing.T, candidate ingest.GenerationCandidate) int {
	t.Helper()
	stats := candidate.Result.Generation.Metadata.Stats
	if stats.InputSubmissionCount == nil {
		t.Fatalf("inputSubmissionCount is unmeasured")
	}
	return int(*stats.InputSubmissionCount)
}
