package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
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
	Name        string                   `yaml:"name"`
	Pipeline    bool                     `yaml:"pipeline"`
	Files       []claudeWorkflowFile     `yaml:"files"`
	Sessions    []claudeWorkflowExpected `yaml:"sessions"`
	Diagnostics []string                 `yaml:"diagnostics"`
}

type claudeWorkflowFile struct {
	Path  string   `yaml:"path"`
	Lines []string `yaml:"lines"`
}

type claudeWorkflowExpected struct {
	ID        string   `yaml:"id"`
	Parent    string   `yaml:"parent"`
	Source    string   `yaml:"source"`
	Subagents []string `yaml:"subagents"`
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
	}
	if err := testutil.RequireFixtureNames("Claude workflow subagent fixture", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// writeClaudeWorkflowTree writes a fixture tree under the Claude source path,
// older than any staleness threshold so a pipeline run treats it as finished.
func writeClaudeWorkflowTree(t *testing.T, fs *testutil.MemFS, files []claudeWorkflowFile) {
	t.Helper()
	for _, file := range files {
		path := claudeWorkflowSource(file.Path)
		if err := fs.WriteFile(path, []byte(strings.Join(file.Lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write Claude fixture %q: %v", file.Path, err)
		}
		fs.ModTimes[path] = time.Now().Add(-2 * time.Hour)
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
// and reports a workflow transcript it leaves out.
func TestClaudeAdapter_DiscoverWorkflowSubagents(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadClaudeWorkflowFixtures(t).Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			fs := testutil.NewMemFS()
			writeClaudeWorkflowTree(t, fs, fixture.Files)

			adapter := ingest.NewClaudeAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
			cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{claudeWorkflowRoot}, Enabled: true}
			// Two discoveries on one adapter: the diagnostics describe the most
			// recent call, so the second must report the same set, not twice it.
			for _, call := range []string{"first", "second"} {
				sessions, err := adapter.Discover(context.Background(), cfg)
				if err != nil {
					t.Fatalf("%s discovery: %v", call, err)
				}
				requireClaudeWorkflowDiscovery(t, call, sessions, fixture.Sessions)
				requireClaudeWorkflowDiagnostics(t, call, adapter.DiscoveryDiagnostics(), fixture.Diagnostics)
			}
		})
	}
}

func requireClaudeWorkflowDiscovery(t *testing.T, call string, sessions []ingest.DiscoveredSession, expected []claudeWorkflowExpected) {
	t.Helper()
	got := make(map[string]ingest.DiscoveredSession, len(sessions))
	for _, session := range sessions {
		if _, twice := got[string(session.SessionID)]; twice {
			t.Errorf("%s discovery yielded session %q twice", call, session.SessionID)
		}
		got[string(session.SessionID)] = session
	}
	if len(got) != len(expected) {
		t.Errorf("%s discovery yielded %d sessions %v, want %d", call, len(got), slices.Sorted(maps.Keys(got)), len(expected))
	}
	for _, want := range expected {
		session, ok := got[want.ID]
		if !ok {
			t.Errorf("%s discovery did not yield session %q", call, want.ID)
			continue
		}
		parent := ""
		if session.ParentUUID != nil {
			parent = string(*session.ParentUUID)
		}
		if parent != want.Parent {
			t.Errorf("%s discovery: session %q has parent %q, want %q", call, want.ID, parent, want.Parent)
		}
		if session.SourcePath.String() != claudeWorkflowSource(want.Source) {
			t.Errorf("%s discovery: session %q has source %q, want %q", call, want.ID, session.SourcePath, claudeWorkflowSource(want.Source))
		}
		children := make([]string, 0, len(session.SubagentPaths))
		for _, path := range session.SubagentPaths {
			children = append(children, path.String())
		}
		slices.Sort(children)
		if !slices.Equal(children, claudeWorkflowSources(want.Subagents)) {
			t.Errorf("%s discovery: session %q links children %v, want %v", call, want.ID, children, claudeWorkflowSources(want.Subagents))
		}
	}
}

func requireClaudeWorkflowDiagnostics(t *testing.T, call string, diagnostics []ingest.DiscoveryDiagnostic, expected []string) {
	t.Helper()
	locations := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		if diagnostic.Provider != ingest.HarnessClaudeCode || diagnostic.Code != ingest.ClaudeDiagnosticDuplicateAgentTranscript {
			t.Errorf("%s discovery reported %s diagnostic %q for %q, want %s %q", call, diagnostic.Provider, diagnostic.Code,
				diagnostic.Location, ingest.HarnessClaudeCode, ingest.ClaudeDiagnosticDuplicateAgentTranscript)
		}
		if diagnostic.Summary == "" || diagnostic.Detail == "" {
			t.Errorf("%s discovery reported %q without a summary and a detail", call, diagnostic.Location)
		}
		locations = append(locations, diagnostic.Location)
	}
	slices.Sort(locations)
	if want := claudeWorkflowSources(expected); !slices.Equal(locations, want) {
		t.Errorf("%s discovery reported left-out transcripts %v, want %v", call, locations, want)
	}
}

// TestPipeline_IngestsClaudeWorkflowSubagents runs the production Claude
// adapter through the pipeline into a real store, twice. The first run stores
// the parent and every child under it, with each workflow child's source path
// naming its run; the second run over the same unchanged tree stores nothing
// new, so the store still holds exactly one row per session.
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
			writeClaudeWorkflowTree(t, fs, fixture.Files)

			cfg := ingest.PipelineConfig{
				Sources: map[ingest.Harness]ingest.SourceConfig{
					ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{claudeWorkflowRoot}, Enabled: true},
				},
				OutputDir:          ingest.ResolvedPath(outputDir),
				StalenessThreshold: time.Minute,
			}
			adapters := map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: ingest.DefaultAdapterRegistry[ingest.HarnessClaudeCode],
			}
			run := func(label string) *ingest.PipelineResult {
				t.Helper()
				// The store, metrics store and indexers are the ones a harvest wires.
				pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
					ingest.WithStore(database), ingest.WithMetricsStore(database),
					ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
				if err != nil {
					t.Fatalf("NewPipeline (%s run): %v", label, err)
				}
				result, err := pipeline.Run(ctx)
				if err != nil {
					t.Fatalf("Run (%s run): %v", label, err)
				}
				if result.Summary.StoreError != nil {
					t.Fatalf("%s run store error: %v", label, result.Summary.StoreError)
				}
				for _, session := range result.Sessions {
					if session.Error != nil {
						t.Errorf("%s run: session %s failed: %v", label, session.SessionID, session.Error)
					}
				}
				requireClaudeWorkflowDiagnostics(t, label+" run", result.DiscoveryDiagnostics, fixture.Diagnostics)
				return result
			}

			first := run("first")
			if first.Summary.New != len(fixture.Sessions) {
				t.Errorf("first run recorded %d new sessions, want %d", first.Summary.New, len(fixture.Sessions))
			}
			requireStoredClaudeWorkflowSessions(t, ctx, database, fs, outputDir, "first run", fixture.Sessions)

			second := run("second")
			if second.Summary.New != 0 || second.Summary.Updated != 0 || second.Summary.Unchanged != len(fixture.Sessions) {
				t.Errorf("second run over the unchanged tree recorded %d new, %d updated and %d unchanged sessions, want 0, 0 and %d",
					second.Summary.New, second.Summary.Updated, second.Summary.Unchanged, len(fixture.Sessions))
			}
			requireStoredClaudeWorkflowSessions(t, ctx, database, fs, outputDir, "second run", fixture.Sessions)
		})
	}
}

// requireStoredClaudeWorkflowSessions checks the store holds exactly the
// expected sessions, each with its parent and its source path, and that every
// child's saved pair sits under its parent's subagents directory with the
// parent's metadata listing it.
func requireStoredClaudeWorkflowSessions(t *testing.T, ctx context.Context, database *store.Store, fs *testutil.MemFS, outputDir, label string, expected []claudeWorkflowExpected) {
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
		if want.Parent != "" {
			children[want.Parent] = append(children[want.Parent], want.ID)
		}
	}

	for parent, ids := range children {
		host, _, err := database.LookupSessionLocation(ctx, ingest.SessionID(parent))
		if err != nil {
			t.Fatalf("%s: look up where %q is saved: %v", label, parent, err)
		}
		for _, id := range ids {
			nested := filepath.Join(outputDir, host, parent, defaults.DirSubagents.String(), id, id+defaults.MetadataSuffix)
			if _, err := fs.Stat(nested); err != nil {
				t.Errorf("%s: child %q is not saved under its parent at %q: %v", label, id, nested, err)
			}
		}
		refs := storedSubagentRefs(t, fs, filepath.Join(outputDir, host, parent, parent+defaults.MetadataSuffix))
		slices.Sort(ids)
		if !slices.Equal(refs, ids) {
			t.Errorf("%s: parent %q metadata lists children %v, want %v", label, parent, refs, ids)
		}
	}
}

func storedSubagentRefs(t *testing.T, fs *testutil.MemFS, path string) []string {
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
