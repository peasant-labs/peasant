package main

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

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_coverage.yaml
var indexCoverageIntegrationFixtureData []byte

const indexCoverageIntegrationFixturePath = "cmd/peasant/testdata/index_coverage.yaml"

type indexCoverageIntegrationDocument struct {
	RequiredCases []string                           `yaml:"required_cases"`
	Cases         []indexCoverageIntegrationCaseSpec `yaml:"cases"`
}

type indexCoverageIntegrationCaseSpec struct {
	Name                  string                            `yaml:"name"`
	OutputDir             string                            `yaml:"outputDir"`
	Sessions              []indexCoverageIntegrationSession `yaml:"sessions"`
	WantFailedAttempts    int                               `yaml:"wantFailedAttempts"`
	WantEmpty             int                               `yaml:"wantEmpty"`
	WantRetained          int                               `yaml:"wantRetained"`
	WantEmptyPositions    []int                             `yaml:"wantEmptyPositions"`
	WantRetainedPositions []int                             `yaml:"wantRetainedPositions"`
}

type indexCoverageIntegrationSession struct {
	Position   int  `yaml:"position"`
	HasEntries bool `yaml:"hasEntries"`
}

func loadIndexCoverageIntegrationFixture(data []byte) (indexCoverageIntegrationDocument, error) {
	var document indexCoverageIntegrationDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("%s: decode typed fields: %w", indexCoverageIntegrationFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("found another YAML document")
		}
		return document, fmt.Errorf("%s: exactly one YAML document is allowed: %w", indexCoverageIntegrationFixturePath, err)
	}
	if len(document.Cases) == 0 {
		return document, fmt.Errorf("%s: the fixture holds no cases", indexCoverageIntegrationFixturePath)
	}
	if len(document.RequiredCases) == 0 {
		return document, fmt.Errorf("%s: no required cases are named; a fixture with no manifest protects nothing", indexCoverageIntegrationFixturePath)
	}
	required := map[string]bool{}
	for _, name := range document.RequiredCases {
		if strings.TrimSpace(name) == "" || required[name] {
			return document, fmt.Errorf("%s: required case name %q is blank or repeated", indexCoverageIntegrationFixturePath, name)
		}
		required[name] = true
	}
	seen := map[string]bool{}
	for _, spec := range document.Cases {
		if strings.TrimSpace(spec.Name) == "" || seen[spec.Name] {
			return document, fmt.Errorf("%s: case name %q is missing or duplicated", indexCoverageIntegrationFixturePath, spec.Name)
		}
		seen[spec.Name] = true
		if strings.TrimSpace(spec.OutputDir) == "" {
			return document, fmt.Errorf("%s: case %q has no output directory", indexCoverageIntegrationFixturePath, spec.Name)
		}
		if len(spec.Sessions) == 0 {
			return document, fmt.Errorf("%s: case %q seeds no sessions", indexCoverageIntegrationFixturePath, spec.Name)
		}
		positions := map[int]bool{}
		for _, session := range spec.Sessions {
			if session.Position < 0 || positions[session.Position] {
				return document, fmt.Errorf("%s: case %q names session position %d blank or twice", indexCoverageIntegrationFixturePath, spec.Name, session.Position)
			}
			positions[session.Position] = true
		}
		if spec.WantFailedAttempts != spec.WantEmpty+spec.WantRetained {
			return document, fmt.Errorf("%s: case %q states wantFailedAttempts=%d, which is not wantEmpty=%d + wantRetained=%d", indexCoverageIntegrationFixturePath, spec.Name, spec.WantFailedAttempts, spec.WantEmpty, spec.WantRetained)
		}
		if len(spec.WantEmptyPositions) != spec.WantEmpty || len(spec.WantRetainedPositions) != spec.WantRetained {
			return document, fmt.Errorf("%s: case %q names %d empty and %d retained position(s), want %d and %d", indexCoverageIntegrationFixturePath, spec.Name, len(spec.WantEmptyPositions), len(spec.WantRetainedPositions), spec.WantEmpty, spec.WantRetained)
		}
		member := map[int]bool{}
		for _, position := range append(append([]int{}, spec.WantEmptyPositions...), spec.WantRetainedPositions...) {
			if !positions[position] || member[position] {
				return document, fmt.Errorf("%s: case %q names membership position %d twice or outside the seeded sessions", indexCoverageIntegrationFixturePath, spec.Name, position)
			}
			member[position] = true
		}
		for _, session := range spec.Sessions {
			if !member[session.Position] {
				return document, fmt.Errorf("%s: case %q leaves seeded session %d in neither membership list", indexCoverageIntegrationFixturePath, spec.Name, session.Position)
			}
		}
	}
	missing, extra := "", ""
	for name := range required {
		if !seen[name] {
			missing = name
			break
		}
	}
	for name := range seen {
		if !required[name] {
			extra = name
			break
		}
	}
	if missing != "" {
		return document, fmt.Errorf("%s: required case %q is missing", indexCoverageIntegrationFixturePath, missing)
	}
	if extra != "" {
		return document, fmt.Errorf("%s: case %q is not in the required manifest", indexCoverageIntegrationFixturePath, extra)
	}
	return document, nil
}

func loadIndexCoverageIntegrationCases(t *testing.T) []indexCoverageIntegrationCaseSpec {
	t.Helper()
	document, err := loadIndexCoverageIntegrationFixture(indexCoverageIntegrationFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// coverageFailIndexer refuses every parse. The pipeline records a failed index
// attempt for every targeted session while the entries the store already holds
// stay exactly as seeded, which is the shape the coverage report exists to
// describe: a failed attempt that retained its entries, or one that never had
// any.
type coverageFailIndexer struct{ err error }

func (f coverageFailIndexer) SourceKind() ingest.TranscriptSourceKind {
	return ingest.TranscriptSourceFile
}

func (f coverageFailIndexer) IndexTranscript(context.Context, ingest.DiscoveredSession) ([]schema.SessionEntry, error) {
	return nil, f.err
}

func (f coverageFailIndexer) IndexTranscriptBytes(context.Context, ingest.DiscoveredSession, []byte) ([]schema.SessionEntry, error) {
	return nil, f.err
}

var _ ingest.TranscriptIndexer = coverageFailIndexer{}

// coverageIntegrationFixture is one seeded session the integration run will
// fail to index, with the stored entries that decide which side of the split it
// lands on.
type coverageIntegrationFixture struct {
	id          ingest.SessionID
	retainEntry bool
}

// coverageIntegrationSessionID returns a distinct valid session UUID for the
// fixture at position i.
func coverageIntegrationSessionID(i int) ingest.SessionID {
	return ingest.SessionID(fmt.Sprintf("22222222-2222-4222-8222-%012x", i))
}

// coverageIntegrationMeta builds the retained metadata a completed harvest
// leaves for one seeded session.
func coverageIntegrationMeta(t *testing.T, session ingest.SessionID) ingest.UnifiedMetadata {
	t.Helper()
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = session
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.Source = schema.SourceInfo{FilePath: "/synthetic/source.jsonl", Format: ingest.SourceFormatJSONL}
	ingested := int64(1700000000000)
	meta.Timestamp = schema.TimestampInfo{Start: 1708300800000, End: 1708300860000, Ingested: &ingested}
	meta.HostSlug = schema.HostSlug(testutil.TestHostSlug)
	// A fixed but well-formed identity: production refuses metadata without a
	// project hash, and the value itself is irrelevant to coverage membership.
	meta.Project = schema.ProjectContext{
		Hash:     schema.ProjectHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Name:     "testproj",
		FilePath: "/testproj",
	}
	return meta
}

// seedCoverageIntegrationSession writes the retained pair for one fixture and,
// when the fixture is retained, one stored entry row. It returns nothing: the
// store is the fixture.
func seedCoverageIntegrationSession(t *testing.T, db *store.Store, filesystem *testutil.MemFS, output string, fixture coverageIntegrationFixture) {
	t.Helper()
	meta := coverageIntegrationMeta(t, fixture.id)
	storetest.SeedManagedArtifact(t, db, filesystem, output, meta, []byte(`{"sessionId":"synthetic"}`))
	if !fixture.retainEntry {
		return
	}
	preview := "retained entry content"
	entries := []schema.SessionEntry{{
		SessionID:      schema.SessionID(fixture.id),
		Harness:        schema.HarnessClaudeCode,
		EntryIndex:     0,
		EntryType:      schema.EntryTypeText,
		Role:           schema.RoleUser,
		ContentPreview: &preview,
	}}
	if err := db.IndexSessionEntries(context.Background(), schema.SessionID(fixture.id), entries); err != nil {
		t.Fatalf("seed retained entries for %s: %v", fixture.id, err)
	}
}

// runCoverageIntegration drives the production pipeline finalize over a real
// SQLite store and returns the result. The pipeline is the reindex path the
// command uses: it scans the seeded output tree, reconciles the rows, and
// indexes every target, so the failed set and the stored membership both come
// from real components rather than a hand-built result.
func runCoverageIntegration(t *testing.T, db *store.Store, outputDir string, fixtures []coverageIntegrationFixture) *ingest.PipelineResult {
	t.Helper()
	filesystem := testutil.NewMemFS()
	for _, fixture := range fixtures {
		seedCoverageIntegrationSession(t, db, filesystem, outputDir, fixture)
	}
	pipeline, err := ingest.NewPipeline(
		filesystem,
		testutil.DefaultGitResolver(),
		map[ingest.Harness]ingest.AdapterFactory{
			ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
				return &testutil.StubAdapter{ProviderValue: ingest.HarnessClaudeCode}
			},
		},
		ingest.PipelineConfig{
			OutputDir:   ingest.ResolvedPath(outputDir),
			Reindex:     true,
			Parallelism: 1,
		},
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithIndexLogger(db),
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: coverageFailIndexer{err: errors.New("synthetic parse refusal")},
		}),
	)
	if err != nil {
		t.Fatalf("construct coverage integration pipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("run coverage integration pipeline: %v", err)
	}
	return result
}

// TestHarvestIndexCoverage_RealStoreThroughSummaryAndJSON is the end-to-end
// regression: a real SQLite store, the production pipeline finalize, the text
// summary and the real JSON serialization, all in one run. The previous
// failure surfaced as a bare count of failed attempts described as empty
// sessions; this pins the number a user reads to the entries the store
// actually holds.
func TestHarvestIndexCoverage_RealStoreThroughSummaryAndJSON(t *testing.T) {
	for _, spec := range loadIndexCoverageIntegrationCases(t) {
		spec := spec
		t.Run(spec.Name, func(t *testing.T) {
			t.Parallel()
			db := storetest.Open(t)
			ctx := context.Background()

			fixtures := make([]coverageIntegrationFixture, 0, len(spec.Sessions))
			positionByID := map[ingest.SessionID]int{}
			for _, session := range spec.Sessions {
				id := coverageIntegrationSessionID(session.Position)
				fixtures = append(fixtures, coverageIntegrationFixture{id: id, retainEntry: session.HasEntries})
				positionByID[id] = session.Position
			}
			result := runCoverageIntegration(t, db, spec.OutputDir, fixtures)

			coverage := result.IndexCoverage
			if coverage == nil {
				t.Fatal("the production finalize left IndexCoverage nil for a store that answers stored-entry membership; nil means unavailable, never zero")
			}
			if coverage.FailedAttempts != spec.WantFailedAttempts || coverage.Empty != spec.WantEmpty || coverage.FailedRetained != spec.WantRetained {
				t.Fatalf("IndexCoverage = %+v, want {FailedAttempts:%d Empty:%d FailedRetained:%d} from the real store's membership answer", coverage, spec.WantFailedAttempts, spec.WantEmpty, spec.WantRetained)
			}
			if coverage.FailedAttempts != coverage.Empty+coverage.FailedRetained {
				t.Errorf("FailedAttempts = %d, want Empty + FailedRetained = %d + %d", coverage.FailedAttempts, coverage.Empty, coverage.FailedRetained)
			}

			// The real store's answer is the independent expectation: every
			// seeded session is reported exactly as its entries dictate.
			failed := ingest.FailedIndexSessions(result.IndexLog)
			gotWithout, err := db.SessionsWithoutEntries(ctx, failed)
			if err != nil {
				t.Fatalf("read stored-entry membership: %v", err)
			}
			for _, session := range spec.Sessions {
				id := coverageIntegrationSessionID(session.Position)
				if got := gotWithout[id]; got != !session.HasEntries {
					t.Errorf("stored-entry membership for %s = %v, want %v", id, got, !session.HasEntries)
				}
			}

			// Membership, not just the numbers: which failed sessions the run
			// reports empty and which retained, through the real store.
			gotEmptyPositions, gotRetainedPositions := []int{}, []int{}
			for _, id := range failed {
				position, ok := positionByID[id]
				if !ok {
					t.Fatalf("failed session %s is not one of the seeded fixture sessions", id)
				}
				if gotWithout[id] {
					gotEmptyPositions = append(gotEmptyPositions, position)
				} else {
					gotRetainedPositions = append(gotRetainedPositions, position)
				}
			}
			if fmt.Sprintf("%v", gotEmptyPositions) != fmt.Sprintf("%v", spec.WantEmptyPositions) {
				t.Errorf("empty membership = %v, want %v", gotEmptyPositions, spec.WantEmptyPositions)
			}
			if fmt.Sprintf("%v", gotRetainedPositions) != fmt.Sprintf("%v", spec.WantRetainedPositions) {
				t.Errorf("retained membership = %v, want %v", gotRetainedPositions, spec.WantRetainedPositions)
			}

			var summary strings.Builder
			printSummary(&summary, result, false, false, spec.OutputDir, "", nil, 0)
			printed := summary.String()
			if spec.WantEmpty > 0 {
				if want := fmt.Sprintf("%d session(s) have no stored entries", spec.WantEmpty); !strings.Contains(printed, want) {
					t.Errorf("the summary must name the %d empty session(s) from the measured coverage. got:\n%s", spec.WantEmpty, printed)
				}
			} else if strings.Contains(printed, "have no stored entries") {
				t.Errorf("no failed session is empty, yet the summary warns about empty sessions:\n%s", printed)
			}
			if spec.WantRetained > 0 {
				if want := fmt.Sprintf("%d session(s) failed to re-index but kept their previous entries", spec.WantRetained); !strings.Contains(printed, want) {
					t.Errorf("the summary must name the %d retained session(s) from the measured coverage. got:\n%s", spec.WantRetained, printed)
				}
			} else if strings.Contains(printed, "kept their previous entries") {
				t.Errorf("no failed session retained its entries, yet the summary says some did:\n%s", printed)
			}
			if strings.Contains(printed, "were imported but NOT indexed") {
				t.Errorf("measured coverage must replace the bare attempt count, not repeat it. got:\n%s", printed)
			}
			if spec.WantFailedAttempts > 0 {
				if !strings.Contains(printed, indexFailureRemedy) {
					t.Errorf("the coverage warning must carry the remedy %q. got:\n%s", indexFailureRemedy, printed)
				}
			} else if strings.Contains(printed, indexFailureRemedy) {
				t.Errorf("a run with no failures must not carry the failure remedy:\n%s", printed)
			}

			var jsonOut strings.Builder
			if err := printJSON(&jsonOut, result); err != nil {
				t.Fatalf("printJSON: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(jsonOut.String()), &decoded); err != nil {
				t.Fatalf("printJSON output is not JSON: %v\n%s", err, jsonOut.String())
			}
			raw, ok := decoded["indexCoverage"]
			if !ok {
				t.Fatalf("measured coverage is missing from the real JSON output:\n%s", jsonOut.String())
			}
			fields, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("indexCoverage is not an object:\n%s", jsonOut.String())
			}
			for field, want := range map[string]int{
				"failedAttempts": spec.WantFailedAttempts,
				"empty":          spec.WantEmpty,
				"failedRetained": spec.WantRetained,
			} {
				got, ok := fields[field].(float64)
				if !ok || int(got) != want {
					t.Errorf("indexCoverage.%s = %v, want %d:\n%s", field, fields[field], want, jsonOut.String())
				}
			}

			// The contrast the field exists for: a run the store could not
			// answer for omits indexCoverage entirely, so a reader never
			// confuses "we checked and found none" with "we could not check".
			var unavailableOut strings.Builder
			if err := printJSON(&unavailableOut, &ingest.PipelineResult{}); err != nil {
				t.Fatalf("printJSON for an unavailable run: %v", err)
			}
			if strings.Contains(unavailableOut.String(), "indexCoverage") {
				t.Errorf("an unavailable run must omit indexCoverage so a measured zero stays distinguishable from an absent answer:\n%s", unavailableOut.String())
			}
			if jsonOut.String() == unavailableOut.String() {
				t.Error("a measured zero and an unavailable answer serialized identically; a reader cannot tell them apart")
			}
		})
	}
}
