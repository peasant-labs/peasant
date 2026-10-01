package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/claude_workflow_subagents.yaml
var claudeWorkflowSubagentsYAML []byte

// claudeWorkflowRoot is the Claude source path every fixture tree is written
// under.
const claudeWorkflowRoot = "/claude"

type claudeWorkflowFixtures struct {
	// RequiredNames is a deletion-protection manifest: every listed case name
	// must be present in Cases. It does not bound how many cases exist.
	RequiredNames []string             `yaml:"required_names"`
	Cases         []claudeWorkflowCase `yaml:"cases"`
}

type claudeWorkflowCase struct {
	Name     string              `yaml:"name"`
	Pipeline bool                `yaml:"pipeline"`
	Runs     []claudeWorkflowRun `yaml:"runs"`
}

type claudeWorkflowRun struct {
	Files    []claudeWorkflowFile     `yaml:"files"`
	Force    bool                     `yaml:"force"`
	Sessions []claudeWorkflowExpected `yaml:"sessions"`
	Recorded []string                 `yaml:"recorded"`
}

type claudeWorkflowFile struct {
	Path  string   `yaml:"path"`
	Lines []string `yaml:"lines"`
}

type claudeWorkflowExpected struct {
	ID             string   `yaml:"id"`
	Parent         string   `yaml:"parent"`
	Source         string   `yaml:"source"`
	Subagents      []string `yaml:"subagents"`
	SavedSubagents []string `yaml:"saved_subagents"`
}

func loadClaudeWorkflowFixtures(t *testing.T) claudeWorkflowFixtures {
	t.Helper()
	var fixtures claudeWorkflowFixtures
	if err := testutil.DecodeFixtureYAML(claudeWorkflowSubagentsYAML, &fixtures); err != nil {
		t.Fatalf("decode Claude workflow subagent fixtures: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		present[fixture.Name] = true
		if len(fixture.Runs) == 0 {
			t.Fatalf("Claude workflow fixture %q has no runs", fixture.Name)
		}
		for i, run := range fixture.Runs {
			// A pipeline run states what it records, even when that is
			// nothing; a discovery-only run has nothing to record or force.
			if fixture.Pipeline && run.Recorded == nil {
				t.Fatalf("Claude workflow fixture %q run %d does not say what it records", fixture.Name, i+1)
			}
			if !fixture.Pipeline && (run.Recorded != nil || run.Force) {
				t.Fatalf("Claude workflow fixture %q run %d sets pipeline fields on a discovery-only case", fixture.Name, i+1)
			}
		}
	}
	if err := testutil.RequireFixtureNames("Claude workflow subagent fixture", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// writeClaudeWorkflowFiles writes files under the Claude source path, older
// than any staleness threshold so a pipeline run treats them as finished, and
// records each file's lines by its source path.
func writeClaudeWorkflowFiles(t *testing.T, fs *testutil.MemFS, files []claudeWorkflowFile, written map[string][]string) {
	t.Helper()
	for _, file := range files {
		path := claudeWorkflowSource(file.Path)
		if err := fs.WriteFile(path, []byte(strings.Join(file.Lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write Claude fixture %q: %v", file.Path, err)
		}
		fs.ModTimes[path] = time.Now().Add(-2 * time.Hour)
		written[path] = file.Lines
	}
}

func claudeWorkflowSource(relative string) string {
	return claudeWorkflowRoot + "/" + relative
}

func claudeWorkflowSources(relatives []string) []string {
	sources := make([]string, 0, len(relatives))
	for _, relative := range relatives {
		sources = append(sources, claudeWorkflowSource(relative))
	}
	slices.Sort(sources)
	return sources
}

// TestClaudeAdapter_DiscoverWorkflowSubagents checks that discovery finds the
// agent transcripts inside a workflow run, attaches each one to its parent the
// same way as a plain subagent, keeps the run directory in its source path,
// and yields one transcript per agent id.
func TestClaudeAdapter_DiscoverWorkflowSubagents(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadClaudeWorkflowFixtures(t).Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			fs := testutil.NewMemFS()
			written := make(map[string][]string)
			adapter := ingest.NewClaudeAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
			cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{claudeWorkflowRoot}, Enabled: true}
			for i, run := range fixture.Runs {
				writeClaudeWorkflowFiles(t, fs, run.Files, written)
				sessions, err := adapter.Discover(context.Background(), cfg)
				if err != nil {
					t.Fatalf("run %d discovery: %v", i+1, err)
				}
				requireClaudeWorkflowDiscovery(t, fmt.Sprintf("run %d", i+1), sessions, run.Sessions)
			}
		})
	}
}

func requireClaudeWorkflowDiscovery(t *testing.T, label string, sessions []ingest.DiscoveredSession, expected []claudeWorkflowExpected) {
	t.Helper()
	got := make(map[string]ingest.DiscoveredSession, len(sessions))
	for _, session := range sessions {
		if _, twice := got[string(session.SessionID)]; twice {
			t.Errorf("%s: discovery yielded session %q twice", label, session.SessionID)
		}
		got[string(session.SessionID)] = session
	}
	if len(got) != len(expected) {
		t.Errorf("%s: discovery yielded %d sessions %v, want %d", label, len(got), slices.Sorted(maps.Keys(got)), len(expected))
	}
	for _, want := range expected {
		session, ok := got[want.ID]
		if !ok {
			t.Errorf("%s: discovery did not yield session %q", label, want.ID)
			continue
		}
		parent := ""
		if session.ParentUUID != nil {
			parent = string(*session.ParentUUID)
		}
		if parent != want.Parent {
			t.Errorf("%s: session %q has parent %q, want %q", label, want.ID, parent, want.Parent)
		}
		if session.SourcePath.String() != claudeWorkflowSource(want.Source) {
			t.Errorf("%s: session %q has source %q, want %q", label, want.ID, session.SourcePath, claudeWorkflowSource(want.Source))
		}
		children := make([]string, 0, len(session.SubagentPaths))
		for _, path := range session.SubagentPaths {
			children = append(children, path.String())
		}
		slices.Sort(children)
		if !slices.Equal(children, claudeWorkflowSources(want.Subagents)) {
			t.Errorf("%s: session %q links children %v, want %v", label, want.ID, children, claudeWorkflowSources(want.Subagents))
		}
	}
}

// TestPipeline_IngestsClaudeWorkflowSubagents runs the production Claude
// adapter through the pipeline into a real store, one harvest per fixture run
// and one more over the unchanged tree. After each harvest the store holds
// exactly the expected sessions, each saved from the transcript that won its
// id, and the harvest recorded exactly the expected sessions; the last
// harvest records nothing and mines no transcript again.
func TestPipeline_IngestsClaudeWorkflowSubagents(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadClaudeWorkflowFixtures(t).Cases {
		if !fixture.Pipeline {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			database := openEvidenceStore(t, filepath.Join(dir, "peasant.db"))
			outputDir := filepath.Join(dir, "peasant-sync")
			fs := testutil.NewMemFS()
			written := make(map[string][]string)

			adapters := map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: ingest.DefaultAdapterRegistry[ingest.HarnessClaudeCode],
			}
			harvest := func(label string, force bool) *ingest.PipelineResult {
				t.Helper()
				cfg := ingest.PipelineConfig{
					Sources: map[ingest.Harness]ingest.SourceConfig{
						ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{claudeWorkflowRoot}, Enabled: true},
					},
					OutputDir:          ingest.ResolvedPath(outputDir),
					StalenessThreshold: time.Minute,
					Force:              force,
				}
				// The store, metrics store and indexers are the ones a harvest wires.
				pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
					ingest.WithStore(database), ingest.WithMetricsStore(database),
					ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
				if err != nil {
					t.Fatalf("NewPipeline (%s): %v", label, err)
				}
				result, err := pipeline.Run(ctx)
				if err != nil {
					t.Fatalf("Run (%s): %v", label, err)
				}
				if result.Summary.StoreError != nil {
					t.Fatalf("%s store error: %v", label, result.Summary.StoreError)
				}
				for _, session := range result.Sessions {
					if session.Error != nil {
						t.Errorf("%s: session %s failed: %v", label, session.SessionID, session.Error)
					}
				}
				return result
			}

			var last []claudeWorkflowExpected
			for i, run := range fixture.Runs {
				label := fmt.Sprintf("run %d", i+1)
				writeClaudeWorkflowFiles(t, fs, run.Files, written)
				result := harvest(label, run.Force)
				requireClaudeWorkflowRecorded(t, label, result, run.Recorded)
				requireStoredClaudeWorkflowSessions(t, ctx, database, fs, outputDir, label, run.Sessions, written)
				last = run.Sessions
			}

			repeat := harvest("repeat run", false)
			requireClaudeWorkflowRecorded(t, "repeat run", repeat, nil)
			if repeat.Summary.Unchanged != len(last) {
				t.Errorf("repeat run: %d sessions unchanged, want %d", repeat.Summary.Unchanged, len(last))
			}
			if repeat.Summary.ReminedEvidenceRecords != 0 {
				t.Errorf("repeat run mined %d transcripts again over an unchanged tree, want none", repeat.Summary.ReminedEvidenceRecords)
			}
			requireStoredClaudeWorkflowSessions(t, ctx, database, fs, outputDir, "repeat run", last, written)
		})
	}
}

// requireClaudeWorkflowRecorded checks the harvest recorded exactly the
// expected sessions as new or updated.
func requireClaudeWorkflowRecorded(t *testing.T, label string, result *ingest.PipelineResult, expected []string) {
	t.Helper()
	recorded := make([]string, 0, len(result.Sessions))
	for _, session := range result.Sessions {
		if session.Status == ingest.DiffNew || session.Status == ingest.DiffUpdated {
			recorded = append(recorded, string(session.SessionID))
		}
	}
	slices.Sort(recorded)
	want := slices.Sorted(slices.Values(expected))
	if !slices.Equal(recorded, want) {
		t.Errorf("%s recorded %v as new or updated, want %v", label, recorded, want)
	}
	if got := result.Summary.New + result.Summary.Updated; got != len(want) {
		t.Errorf("%s counts %d new and %d updated sessions, want %d in all", label, result.Summary.New, result.Summary.Updated, len(want))
	}
}

// requireStoredClaudeWorkflowSessions checks the store holds exactly the
// expected sessions, each with its parent and its source path, that each
// saved transcript is the transcript at that source path, and that every
// child's saved pair sits under its parent's subagents directory with the
// parent's saved metadata listing the expected children.
func requireStoredClaudeWorkflowSessions(t *testing.T, ctx context.Context, database *store.Store, fs *testutil.MemFS, outputDir, label string, expected []claudeWorkflowExpected, written map[string][]string) {
	t.Helper()
	rows, err := database.ListSessionsFiltered(ctx, store.SessionListFilter{})
	if err != nil {
		t.Fatalf("%s: list stored sessions: %v", label, err)
	}
	stored := make(map[string]store.SessionRow, len(rows))
	for _, row := range rows {
		if _, twice := stored[row.SessionID]; twice {
			t.Errorf("%s: session %q is stored twice", label, row.SessionID)
		}
		stored[row.SessionID] = row
	}
	if len(rows) != len(expected) {
		t.Errorf("%s: the store holds %d session rows %v, want %d", label, len(rows), slices.Sorted(maps.Keys(stored)), len(expected))
	}

	children := make(map[string][]string)
	for _, want := range expected {
		if want.Parent != "" {
			children[want.Parent] = append(children[want.Parent], want.ID)
		}
	}
	for _, want := range expected {
		row, ok := stored[want.ID]
		if !ok {
			t.Errorf("%s: session %q is not stored", label, want.ID)
			continue
		}
		parent := ""
		if row.ParentID != nil {
			parent = *row.ParentID
		}
		if parent != want.Parent {
			t.Errorf("%s: stored session %q has parent %q, want %q", label, want.ID, parent, want.Parent)
		}
		source, _, _, err := database.LookupSourceInfo(ctx, ingest.SessionID(want.ID))
		if err != nil {
			t.Fatalf("%s: look up the source of %q: %v", label, want.ID, err)
		}
		if source != claudeWorkflowSource(want.Source) {
			t.Errorf("%s: stored session %q has source %q, want %q", label, want.ID, source, claudeWorkflowSource(want.Source))
		}

		host, _, err := database.LookupSessionLocation(ctx, ingest.SessionID(want.ID))
		if err != nil {
			t.Fatalf("%s: look up where %q is saved: %v", label, want.ID, err)
		}
		saved := filepath.Join(outputDir, host, want.ID)
		if want.Parent != "" {
			saved = filepath.Join(outputDir, host, want.Parent, defaults.DirSubagents.String(), want.ID)
		}
		transcript, err := fs.ReadFile(filepath.Join(saved, want.ID+defaults.TranscriptPrefix+ingest.SourceFormatJSONL.String()))
		if err != nil {
			t.Errorf("%s: session %q has no saved transcript under %q: %v", label, want.ID, saved, err)
			continue
		}
		if got, want := len(strings.Split(strings.TrimRight(string(transcript), "\n"), "\n")), len(written[source]); got != want {
			t.Errorf("%s: session %q saved %d records, want the %d of its source %q", label, row.SessionID, got, want, source)
		}

		listed := want.SavedSubagents
		if listed == nil {
			listed = children[want.ID]
		}
		if want.Parent == "" && len(listed) > 0 {
			refs := savedSubagentRefs(t, fs, filepath.Join(saved, want.ID+defaults.MetadataSuffix))
			if wantRefs := slices.Sorted(slices.Values(listed)); !slices.Equal(refs, wantRefs) {
				t.Errorf("%s: parent %q saved metadata lists children %v, want %v", label, want.ID, refs, wantRefs)
			}
		}
	}
}

func savedSubagentRefs(t *testing.T, fs *testutil.MemFS, path string) []string {
	t.Helper()
	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("read parent metadata %q: %v", path, err)
	}
	var metadata ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("decode parent metadata %q: %v", path, err)
	}
	refs := make([]string, 0, len(metadata.Subagents))
	for _, ref := range metadata.Subagents {
		refs = append(refs, string(ref.SessionID))
	}
	slices.Sort(refs)
	if metadata.Stats.SubagentCount != len(refs) {
		t.Errorf("parent metadata %q counts %d subagents but lists %d", path, metadata.Stats.SubagentCount, len(refs))
	}
	return refs
}
