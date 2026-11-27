package ingest_test

import (
	_ "embed"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/claude_metadata_half.yaml
var claudeMetadataHalfYAML []byte

//go:embed testdata/claude_metadata_half.manifest.yaml
var claudeMetadataHalfManifestYAML []byte

//go:embed testdata/claude_transcript_fixtures.manifest.yaml
var claudeTranscriptFixturesManifestYAML []byte

//go:embed testdata/claude_erroring_branches.manifest.yaml
var claudeErroringBranchesManifestYAML []byte

//go:embed testdata/claude_metadata_half_mutations.yaml
var claudeMetadataHalfMutationsYAML []byte

// The three ways one corpus row may end, comparing the metadata half with the
// full builder on the same input.
const (
	// halfOutcomeEqual: both succeed and the half's metadata equals the
	// builder's (stats aside), with the same completeness and evidence.
	halfOutcomeEqual = "equal"
	// halfOutcomeBothRefuse: both refuse before the entry parse, with the same
	// error class.
	halfOutcomeBothRefuse = "both-refuse"
	// halfOutcomeBuilderRefusesAfterHalf: the half succeeds and the builder
	// refuses in its entry parse, classification or projection.
	halfOutcomeBuilderRefusesAfterHalf = "builder-refuses-after-half"
	// halfOutcomeHalfErrors: the half refuses while the builder succeeds. Such
	// a branch must be listed in claude_erroring_branches.manifest.yaml.
	halfOutcomeHalfErrors = "half-errors"
)

// claudeMetadataHalfRow is one extra corpus row: the producer row's inputs
// with no generation expectations.
type claudeMetadataHalfRow struct {
	claudeProducerCase `yaml:",inline"`
}

type claudeMetadataHalfDocument struct {
	Rows []claudeMetadataHalfRow `yaml:"rows"`
	// Branches names, for each builder branch the differential must cover,
	// the corpus row that exercises it.
	Branches map[string]string `yaml:"branches"`
	// Outcomes names every corpus row whose outcome is not "equal", with the
	// expected outcome. For builder-refuses-after-half the class is named in
	// AfterHalfClasses.
	Outcomes         map[string]string `yaml:"outcomes"`
	AfterHalfClasses map[string]string `yaml:"afterHalfClasses"`
}

type claudeTranscriptFixturesManifest struct {
	// Directories are the repository directories (from the module root) that
	// hold Claude JSONL transcript fixtures; every Claude transcript file
	// under a testdata directory must sit under one of them.
	Directories []string `yaml:"directories"`
	// Documents are the YAML fixture files that carry Claude transcripts
	// inline and form part of the corpus.
	Documents []string `yaml:"documents"`
}

type claudeErroringBranchesManifest struct {
	// ErroringBranches lists every corpus row whose half refuses while the
	// builder succeeds, with the reason. Expected empty.
	ErroringBranches []struct {
		Name   string `yaml:"name"`
		Reason string `yaml:"reason"`
	} `yaml:"erroringBranches"`
}

// claudeHalfCorpusRow is one differential input.
type claudeHalfCorpusRow struct {
	name  string
	build func(t *testing.T) (*ingest.ClaudeIndexer, ingest.GenerationBuildInput)
}

const moduleRootFromIngest = "../.."

// claudeFixturePathMarker marks a fixture path as Claude's: every Claude
// transcript fixture directory names the harness by its short identifier.
var claudeFixturePathMarker = string(defaults.LegacyHarnessClaude)

// claudeSubagentsDirName is the directory a Claude subagent transcript sits
// in, under its owner's directory.
const claudeSubagentsDirName = "subagents"

// corpusWalkSkippedDirs are the repository directories the fixture-corpus
// walk never enters: no Go test fixture lives in them.
var corpusWalkSkippedDirs = []string{".git", "node_modules", "web", "third_party"}

func loadClaudeMetadataHalfDocument(t *testing.T) claudeMetadataHalfDocument {
	t.Helper()
	var document claudeMetadataHalfDocument
	if err := testutil.DecodeFixtureYAML(claudeMetadataHalfYAML, &document); err != nil {
		t.Fatalf("decode the metadata-half fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(claudeMetadataHalfManifestYAML, "claude metadata half")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Rows))
	for _, row := range document.Rows {
		names = append(names, row.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "claude metadata half"); err != nil {
		t.Fatal(err)
	}
	return document
}

// claudeHalfCorpus is every Claude transcript fixture in the repository: the
// producer rows, the metadata-half rows, and every JSONL file under the
// manifest's directories.
func claudeHalfCorpus(t *testing.T) []claudeHalfCorpusRow {
	t.Helper()
	var corpus []claudeHalfCorpusRow
	addRow := func(prefix string, row claudeProducerCase) {
		corpus = append(corpus, claudeHalfCorpusRow{name: prefix + row.Name, build: func(t *testing.T) (*ingest.ClaudeIndexer, ingest.GenerationBuildInput) {
			idx, input, _, _, _ := claudeProducerInput(t, row)
			return idx, input
		}})
	}
	for _, row := range loadClaudeProducerCases(t) {
		addRow("producer/", row)
	}
	for _, row := range loadClaudeMetadataHalfDocument(t).Rows {
		addRow("half/", row.claudeProducerCase)
	}
	for _, path := range claudeTranscriptFixtureFiles(t) {
		corpus = append(corpus, claudeHalfCorpusRow{name: "file/" + path, build: func(t *testing.T) (*ingest.ClaudeIndexer, ingest.GenerationBuildInput) {
			return claudeFileFixtureInput(t, path)
		}})
	}
	return corpus
}

func loadClaudeTranscriptFixturesManifest(t *testing.T) claudeTranscriptFixturesManifest {
	t.Helper()
	var manifest claudeTranscriptFixturesManifest
	if err := testutil.DecodeFixtureYAML(claudeTranscriptFixturesManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Directories) == 0 || len(manifest.Documents) == 0 {
		t.Fatal("the Claude transcript fixture manifest names no directories or no documents")
	}
	return manifest
}

// claudeTranscriptFixtureFiles returns every JSONL file under the manifest's
// directories, relative to the module root, sorted.
func claudeTranscriptFixtureFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, dir := range loadClaudeTranscriptFixturesManifest(t).Directories {
		err := filepath.WalkDir(filepath.Join(moduleRootFromIngest, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
				rel, err := filepath.Rel(moduleRootFromIngest, path)
				if err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk the Claude fixture directory %s: %v", dir, err)
		}
	}
	if len(files) == 0 {
		t.Fatal("the Claude fixture directories hold no JSONL transcript")
	}
	sort.Strings(files)
	return files
}

// claudeFileFixtureInput builds the builder input for one JSONL fixture file:
// the session is the file name, a file under subagents/ records its owner as
// the parent, and the indexer reads the real fixture tree, so a structural
// sibling proof sees the files beside it.
func claudeFileFixtureInput(t *testing.T, rel string) (*ingest.ClaudeIndexer, ingest.GenerationBuildInput) {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join(moduleRootFromIngest, rel))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	sid := strings.TrimSuffix(filepath.Base(abs), ".jsonl")
	metadata := makeReindexMeta(t, sid, abs)
	if filepath.Base(filepath.Dir(abs)) == claudeSubagentsDirName {
		parent, err := ingest.NewSessionID(filepath.Base(filepath.Dir(filepath.Dir(abs))))
		if err != nil {
			t.Fatalf("subagent owner of %s: %v", rel, err)
		}
		metadata.ParentUUID = &parent
	}
	sessionID, err := ingest.NewSessionID(sid)
	if err != nil {
		t.Fatalf("fixture session id of %s: %v", rel, err)
	}
	idx := ingest.NewClaudeIndexer(&ingest.OSFileSystem{}, ingest.WithClaudeFullDepth(true), ingest.WithClaudeFullContent(true))
	return idx, ingest.GenerationBuildInput{
		Session: ingest.DiscoveredSession{
			SessionID: sessionID, Harness: ingest.HarnessClaudeCode,
			SourceFormat: ingest.SourceFormatJSONL, SourcePath: ingest.ResolvedPath(abs),
		},
		GenerationID:       "gen-file-fixture",
		RecordedMetadata:   metadata,
		CapturedTranscript: data,
	}
}

// TestClaudeTranscriptFixtureCorpusIsComplete fails when a Claude transcript
// fixture exists that the differential corpus does not read: a JSONL file
// under a testdata directory with "claude" in its path outside the listed
// directories, or a YAML fixture declaring the Claude harness with inline
// transcripts that is not a listed document.
func TestClaudeTranscriptFixtureCorpusIsComplete(t *testing.T) {
	manifest := loadClaudeTranscriptFixturesManifest(t)
	var unlisted []string
	err := filepath.WalkDir(moduleRootFromIngest, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(moduleRootFromIngest, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if slices.Contains(corpusWalkSkippedDirs, entry.Name()) {
				return filepath.SkipDir
			}
			// A nested checkout (a git worktree inside this one) is another
			// tree's fixtures, not this module's.
			if rel != "." {
				if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.Contains(rel, "/testdata/") {
			return nil
		}
		if strings.HasSuffix(rel, ".jsonl") && strings.Contains(strings.ToLower(rel), claudeFixturePathMarker) {
			listed := false
			for _, dir := range manifest.Directories {
				if strings.HasPrefix(rel, dir+"/") {
					listed = true
				}
			}
			if !listed {
				unlisted = append(unlisted, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range manifest.Documents {
		if _, err := os.Stat(filepath.Join(moduleRootFromIngest, document)); err != nil {
			t.Errorf("listed Claude fixture document %s: %v", document, err)
		}
	}
	for _, dir := range manifest.Directories {
		if _, err := os.Stat(filepath.Join(moduleRootFromIngest, dir)); err != nil {
			t.Errorf("listed Claude fixture directory %s: %v", dir, err)
		}
	}
	if len(unlisted) != 0 {
		t.Fatalf("Claude transcript fixtures outside the differential corpus: %v; list their directory in claude_transcript_fixtures.manifest.yaml", unlisted)
	}
}

// claudeHalfObservation is what one corpus row produced.
type claudeHalfObservation struct {
	half       ingest.GenerationMetadataCandidate
	halfErr    error
	build      ingest.GenerationCandidate
	buildErr   error
	recordedJS []byte
}

// observeClaudeHalf runs the half and then the builder on one input, and
// checks the recorded metadata is unchanged by either.
func observeClaudeHalf(t *testing.T, row claudeHalfCorpusRow) claudeHalfObservation {
	t.Helper()
	idx, input := row.build(t)
	before, err := json.Marshal(input.RecordedMetadata)
	if err != nil {
		t.Fatal(err)
	}
	var observed claudeHalfObservation
	observed.half, observed.halfErr = idx.BuildGenerationMetadata(t.Context(), input)
	observed.build, observed.buildErr = idx.BuildGeneration(t.Context(), input)
	observed.recordedJS, err = json.Marshal(input.RecordedMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(observed.recordedJS) {
		t.Fatalf("the half and the builder changed the recorded metadata:\nbefore %s\nafter  %s", before, observed.recordedJS)
	}
	return observed
}

func classifyClaudeHalf(observed claudeHalfObservation) string {
	switch {
	case observed.halfErr == nil && observed.buildErr == nil:
		return halfOutcomeEqual
	case observed.halfErr != nil && observed.buildErr != nil:
		return halfOutcomeBothRefuse
	case observed.halfErr == nil:
		return halfOutcomeBuilderRefusesAfterHalf
	default:
		return halfOutcomeHalfErrors
	}
}

// metadataWithoutStats is the metadata with the stats cleared: the projection
// recounts them after the half, and the generation record excludes them.
func metadataWithoutStats(metadata schema.UnifiedMetadata) schema.UnifiedMetadata {
	metadata.Stats = schema.SessionStats{}
	return metadata
}

// claudeHalfDifference returns why the half's candidate differs from the
// builder's metadata, or "" when they agree.
func claudeHalfDifference(observed claudeHalfObservation) string {
	generation := observed.build.Result.Generation
	if !reflect.DeepEqual(metadataWithoutStats(observed.half.Metadata), metadataWithoutStats(generation.Metadata)) {
		half, _ := json.Marshal(metadataWithoutStats(observed.half.Metadata))
		build, _ := json.Marshal(metadataWithoutStats(generation.Metadata))
		return "metadata differs:\nhalf    " + string(half) + "\nbuilder " + string(build)
	}
	if !slices.EqualFunc(observed.half.Metadata.Diagnostics.Warnings, generation.Metadata.Diagnostics.Warnings, func(a, b schema.DiagnosticEntry) bool { return reflect.DeepEqual(a, b) }) {
		return "warnings differ in content or order"
	}
	if observed.half.Completeness != generation.Completeness {
		return "completeness " + string(observed.half.Completeness) + " != " + string(generation.Completeness)
	}
	if observed.half.SourceEvidenceDigest != generation.SourceEvidenceDigest {
		return "source evidence digest differs"
	}
	return ""
}

// sameRefusalClass reports whether the half's refusal is the builder's
// refusal: the same fixed sanitized shape, differing only in the operation it
// names.
func sameRefusalClass(halfErr, buildErr error) bool {
	half := strings.Replace(halfErr.Error(), "ingest.ClaudeIndexer.BuildGenerationMetadata:", "ingest.ClaudeIndexer.BuildGeneration:", 1)
	return half == buildErr.Error()
}

// TestClaudeMetadataHalfMatchesTheBuilder runs BuildGenerationMetadata and
// BuildGeneration over every Claude transcript fixture in the repository and
// asserts the three-way outcome each row declares.
func TestClaudeMetadataHalfMatchesTheBuilder(t *testing.T) {
	document := loadClaudeMetadataHalfDocument(t)
	var erroring claudeErroringBranchesManifest
	if err := testutil.DecodeFixtureYAML(claudeErroringBranchesManifestYAML, &erroring); err != nil {
		t.Fatal(err)
	}
	listedErroring := map[string]bool{}
	for _, branch := range erroring.ErroringBranches {
		if strings.TrimSpace(branch.Reason) == "" {
			t.Fatalf("erroring branch %s carries no reason", branch.Name)
		}
		listedErroring[branch.Name] = true
	}
	corpus := claudeHalfCorpus(t)
	names := map[string]bool{}
	for _, row := range corpus {
		names[row.name] = true
	}
	for branch, row := range document.Branches {
		if !names[row] {
			t.Errorf("branch %s names corpus row %s, which does not exist", branch, row)
		}
	}
	for row := range document.Outcomes {
		if !names[row] {
			t.Errorf("an outcome names corpus row %s, which does not exist", row)
		}
	}
	observedErroring := map[string]bool{}
	for _, row := range corpus {
		t.Run(row.name, func(t *testing.T) {
			observed := observeClaudeHalf(t, row)
			got := classifyClaudeHalf(observed)
			want := document.Outcomes[row.name]
			if want == "" {
				want = halfOutcomeEqual
			}
			if listedErroring[row.name] {
				want = halfOutcomeHalfErrors
			}
			if got != want {
				t.Fatalf("outcome %s, want %s (half error %v, builder error %v)", got, want, observed.halfErr, observed.buildErr)
			}
			switch got {
			case halfOutcomeEqual:
				if difference := claudeHalfDifference(observed); difference != "" {
					t.Fatal(difference)
				}
			case halfOutcomeBothRefuse:
				if !sameRefusalClass(observed.halfErr, observed.buildErr) {
					t.Fatalf("the half and the builder refuse differently:\nhalf    %v\nbuilder %v", observed.halfErr, observed.buildErr)
				}
			case halfOutcomeBuilderRefusesAfterHalf:
				if document.AfterHalfClasses[row.name] == "" {
					t.Fatalf("row %s refuses after the half but names no refusal class", row.name)
				}
			case halfOutcomeHalfErrors:
				observedErroring[row.name] = true
			}
		})
	}
	for name := range listedErroring {
		if !observedErroring[name] {
			t.Errorf("erroring branch %s is listed but no longer errors in the half", name)
		}
	}
}

// TestClaudeBuilderIgnoresTheHalfMutation sets the metadata-half mutation hook
// and shows BuildGeneration's output does not move: the hook reaches the half
// only.
func TestClaudeBuilderIgnoresTheHalfMutation(t *testing.T) {
	type built struct {
		name     string
		idx      *ingest.ClaudeIndexer
		input    ingest.GenerationBuildInput
		baseline []byte
	}
	var rows []built
	for _, row := range claudeHalfCorpus(t) {
		idx, input := row.build(t)
		rows = append(rows, built{name: row.name, idx: idx, input: input, baseline: claudeBuilderMetadataJSON(t, idx, input)})
	}
	ingest.SetClaudeMetadataHalfMutationForTest(t, func(_ ingest.GenerationBuildInput, metadata *schema.UnifiedMetadata) {
		metadata.Project.Name = "mutated by the half hook"
		metadata.Relationships = nil
		metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, schema.DiagnosticEntry{ErrorType: "half-hook"})
	})
	mutatedHalf := 0
	for _, row := range rows {
		if got := claudeBuilderMetadataJSON(t, row.idx, row.input); string(got) != string(row.baseline) {
			t.Fatalf("%s: BuildGeneration changed under the half's mutation hook:\nwithout %s\nwith    %s", row.name, row.baseline, got)
		}
		if half, err := row.idx.BuildGenerationMetadata(t.Context(), row.input); err == nil && half.Metadata.Project.Name == "mutated by the half hook" {
			mutatedHalf++
		}
	}
	if mutatedHalf == 0 {
		t.Fatal("the half's mutation hook changed no half output; the comparison above proves nothing")
	}
}

func claudeBuilderMetadataJSON(t *testing.T, idx *ingest.ClaudeIndexer, input ingest.GenerationBuildInput) []byte {
	t.Helper()
	candidate, err := idx.BuildGeneration(t.Context(), input)
	if err != nil {
		return []byte("refused: " + err.Error())
	}
	encoded, err := json.Marshal(candidate.Result.Generation.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestCodexHasNoMetadataHalf pins that only Claude builds a metadata
// candidate: the Codex indexer, with or without its graph, does not implement
// the capability, so a Codex session has no candidate and parses.
func TestCodexHasNoMetadataHalf(t *testing.T) {
	registry := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{})
	if _, ok := registry[ingest.HarnessClaudeCode].(ingest.GenerationMetadataBuilder); !ok {
		t.Fatal("the registered Claude indexer does not build a metadata candidate")
	}
	builders := 0
	for harness, indexer := range registry {
		if _, ok := indexer.(ingest.GenerationMetadataBuilder); ok {
			builders++
			if harness != ingest.HarnessClaudeCode {
				t.Errorf("the %s indexer builds a metadata candidate; only Claude does", harness)
			}
		}
	}
	if builders != 1 {
		t.Fatalf("%d registered indexers build a metadata candidate, want only Claude", builders)
	}
	for _, indexer := range []any{ingest.NewCodexIndexer(&ingest.OSFileSystem{}), ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexHistoryCapture(true))} {
		if _, ok := indexer.(ingest.GenerationMetadataBuilder); ok {
			t.Fatalf("a Codex indexer %T builds a metadata candidate", indexer)
		}
	}
}

// claudeMetadataHalfMutation is one named change to the half's output that
// the differential and the oracle must both catch.
type claudeMetadataHalfMutation struct {
	Name string `yaml:"name"`
	// OracleUnreachable, when set, says why no store the pipeline writes can
	// reach the mutated branch, so only the differential can catch it.
	OracleUnreachable string `yaml:"oracleUnreachable"`
}

func loadClaudeMetadataHalfMutations(t *testing.T) []claudeMetadataHalfMutation {
	t.Helper()
	var fixture struct {
		Mutations []claudeMetadataHalfMutation `yaml:"mutations"`
	}
	if err := testutil.DecodeFixtureYAML(claudeMetadataHalfMutationsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range fixture.Mutations {
		names = append(names, m.Name)
	}
	want := []string{"skip-target-normalization", "skip-derived-owner-prepend"}
	if !slices.Equal(names, want) {
		t.Fatalf("metadata-half mutations = %v, want exactly %v", names, want)
	}
	return fixture.Mutations
}

// claudeHalfMutationHook returns the hook that applies one named mutation to
// the half's output, undoing a step of the identity proof.
func claudeHalfMutationHook(t *testing.T, name string) func(ingest.GenerationBuildInput, *schema.UnifiedMetadata) {
	t.Helper()
	recordedHas := func(in ingest.GenerationBuildInput, relationship schema.SessionRelationship) (schema.SessionRelationship, bool) {
		for _, recorded := range in.RecordedMetadata.Relationships {
			if recorded.Kind == relationship.Kind && reflect.DeepEqual(recorded.TargetLocalID, relationship.TargetLocalID) {
				return recorded, true
			}
		}
		return schema.SessionRelationship{}, false
	}
	switch name {
	case "skip-target-normalization":
		// The proof turns every known target into known_retained with
		// retained_last_good evidence. Skipping it leaves a natively proven
		// relationship as the proof received it: target_known with
		// native_typed evidence.
		return func(_ ingest.GenerationBuildInput, metadata *schema.UnifiedMetadata) {
			for i, relationship := range metadata.Relationships {
				if relationship.TargetLocalID != nil && relationship.TargetState == schema.RelationshipTargetKnownRetained {
					metadata.Relationships[i].TargetState, metadata.Relationships[i].Evidence = schema.RelationshipTargetKnown, schema.EvidenceNativeTyped
				}
			}
		}
	case "skip-derived-owner-prepend":
		// The proof prepends a started_by owner derived from the transcript;
		// skipping it drops every started_by the recorded metadata lacks.
		return func(in ingest.GenerationBuildInput, metadata *schema.UnifiedMetadata) {
			kept := metadata.Relationships[:0:0]
			for _, relationship := range metadata.Relationships {
				if _, ok := recordedHas(in, relationship); relationship.Kind == schema.SessionRelationshipStartedBy && !ok {
					continue
				}
				kept = append(kept, relationship)
			}
			metadata.Relationships = kept
		}
	}
	t.Fatalf("unknown metadata-half mutation %q", name)
	return nil
}

// TestClaudeMetadataHalfMutationsFailTheDifferential applies each named
// mutation through the half's hook and requires the differential to see a
// difference on at least one corpus row.
func TestClaudeMetadataHalfMutationsFailTheDifferential(t *testing.T) {
	corpus := claudeHalfCorpus(t)
	for _, mutation := range loadClaudeMetadataHalfMutations(t) {
		t.Run(mutation.Name, func(t *testing.T) {
			ingest.SetClaudeMetadataHalfMutationForTest(t, claudeHalfMutationHook(t, mutation.Name))
			var caught []string
			for _, row := range corpus {
				observed := observeClaudeHalf(t, row)
				if classifyClaudeHalf(observed) == halfOutcomeEqual && claudeHalfDifference(observed) != "" {
					caught = append(caught, row.name)
				}
			}
			if len(caught) == 0 {
				t.Fatal("the mutation passed the differential on every corpus row")
			}
			t.Logf("caught on %v", caught)
		})
	}
}

// claudeAfterHalfRefusalLeavesNoGeneration imports one transcript through the
// real pipeline and store and requires that its first parse left no active
// generation. Rows that refuse after the half use it.
func claudeAfterHalfRefusalLeavesNoGeneration(t *testing.T, sid ingest.SessionID, transcript []byte) {
	t.Helper()
	root := t.TempDir()
	source, output := filepath.Join(root, "source"), filepath.Join(root, "managed")
	workspace := filepath.Join(source, "-workspace")
	mustMkdir(t, workspace)
	if err := os.WriteFile(filepath.Join(workspace, string(sid)+".jsonl"), transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	db := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
	t.Cleanup(func() { _ = db.Close() })
	runManagedRefreshPipeline(t, db, source, output)
	active := managedRefreshPipelineRows(t, db, `SELECT COALESCE(active_generation_id,'') FROM sessions WHERE session_id = ?`, sid)
	if len(active) > 0 && active[0] != "" {
		t.Fatalf("the first parse of %s installed generation %s; a builder refusal must leave none", sid, active[0])
	}
}

// TestClaudeRefusalAfterTheHalfLeavesNoGeneration runs every corpus row the
// fixture classes as refusing after the half through a first import.
func TestClaudeRefusalAfterTheHalfLeavesNoGeneration(t *testing.T) {
	document := loadClaudeMetadataHalfDocument(t)
	rows := 0
	for _, row := range claudeHalfCorpus(t) {
		if document.Outcomes[row.name] != halfOutcomeBuilderRefusesAfterHalf {
			continue
		}
		rows++
		t.Run(row.name, func(t *testing.T) {
			_, input := row.build(t)
			claudeAfterHalfRefusalLeavesNoGeneration(t, input.Session.SessionID, input.CapturedTranscript)
		})
	}
	if rows == 0 {
		t.Skip("no corpus row refuses after the half")
	}
}
