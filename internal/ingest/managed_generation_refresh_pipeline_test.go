package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

const managedRefreshPipelineForcedProject = "managed refresh pipeline project"

//go:embed testdata/managed_refresh_pipeline_lanes.yaml
var managedRefreshPipelineLanes []byte

//go:embed testdata/managed_refresh_pipeline_lanes.manifest.yaml
var managedRefreshPipelineLanesManifest []byte

//go:embed testdata/unchanged_pipeline_routes.yaml
var unchangedPipelineRoutes []byte

//go:embed testdata/unchanged_pipeline_routes.manifest.yaml
var unchangedPipelineRoutesManifest []byte

//go:embed testdata/output_preserving_pipeline.yaml
var outputPreservingPipeline []byte

//go:embed testdata/output_preserving_pipeline.manifest.yaml
var outputPreservingPipelineManifest []byte

type outputPreservingClaudeAdapter struct {
	*ingest.ClaudeAdapter
}

func (*outputPreservingClaudeAdapter) DeclaresOutputPreserving() bool { return true }

var _ ingest.OutputPreservingDeclaration = (*outputPreservingClaudeAdapter)(nil)

type countedManagedRefreshStore struct {
	*store.Store
	calls       atomic.Int64
	forgeCounts bool
	refuse      bool
}

type proofCountingRefreshStore struct {
	*countedManagedRefreshStore
	proofs            atomic.Int64
	settles           atomic.Int64
	proof             ingest.UnchangedProof
	proofError        bool
	settleRefused     bool
	forgeSettleCounts bool
}

func (s *proofCountingRefreshStore) ProveCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.UnchangedProof, error) {
	s.proofs.Add(1)
	if s.proofError {
		return ingest.UnchangedProof{}, fmt.Errorf("synthetic proof read failure")
	}
	proof, err := s.Store.ProveCapturedGenerationUnchanged(ctx, request)
	s.proof = proof
	return proof, err
}

func (s *proofCountingRefreshStore) SettleCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.ManagedGenerationRefreshOutcome, error) {
	s.settles.Add(1)
	if s.settleRefused {
		return ingest.ManagedGenerationRefreshOutcome{}, &ingest.StaleIndexWorkError{SessionID: request.SessionID}
	}
	if s.forgeSettleCounts {
		input, conversation := int64(903), int64(902)
		request.Metadata.Stats.TurnCount = 901
		request.Metadata.Stats.InputSubmissionCount = &input
		request.Metadata.Stats.ConversationTurnCount = &conversation
	}
	return s.Store.SettleCapturedGenerationUnchanged(ctx, request)
}

type nativeCountingRefreshFS struct {
	*ingest.OSFileSystem
	source string
	reads  atomic.Int64
}

func (f *nativeCountingRefreshFS) count(path string) {
	if strings.HasPrefix(path, f.source+string(os.PathSeparator)) {
		f.reads.Add(1)
	}
}

func (f *nativeCountingRefreshFS) ReadFile(path string) ([]byte, error) {
	f.count(path)
	return f.OSFileSystem.ReadFile(path)
}

func (f *nativeCountingRefreshFS) Open(path string) (io.ReadCloser, error) {
	f.count(path)
	return f.OSFileSystem.Open(path)
}

func (f *nativeCountingRefreshFS) ReadFileHeader(path string, limit int) ([]byte, error) {
	f.count(path)
	return f.OSFileSystem.ReadFileHeader(path, limit)
}

func TestManagedGenerationUnchangedProofPipelineRoutes(t *testing.T) {
	setup := func(t *testing.T) (string, string, string, ingest.SessionID, *store.Store, []byte) {
		t.Helper()
		root := t.TempDir()
		source, output := filepath.Join(root, "source"), filepath.Join(root, "managed")
		mustMkdir(t, source)
		id := writeManagedRefreshPipelineCorpus(t, source, 1, 8)[0]
		db := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
		t.Cleanup(func() { _ = db.Close() })
		initial := runManagedRefreshPipeline(t, db, source, output)
		if initial.Summary.Errors != 0 || initial.Summary.Indexed != 1 {
			t.Fatalf("initial import: %+v", initial.Summary)
		}
		data, err := os.ReadFile(filepath.Join(source, "-workspace", string(id)+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		managedRefreshPipelineDowngrade(t, db, ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion-1)
		empty := filepath.Join(root, "empty")
		mustMkdir(t, empty)
		return source, output, empty, id, db, data
	}
	run := func(t *testing.T, db *store.Store, source, output, empty string, mode string) (*proofCountingRefreshStore, *nativeCountingRefreshFS, *ingest.IndexProfiler, *ingest.PipelineResult, *atomic.Int64, *atomic.Int64, *[]byte) {
		t.Helper()
		counted := &proofCountingRefreshStore{countedManagedRefreshStore: &countedManagedRefreshStore{Store: db, refuse: mode == "copy-refused"}, proofError: mode == "proof-error", settleRefused: mode == "settle-refused", forgeSettleCounts: mode == ""}
		fs := &nativeCountingRefreshFS{OSFileSystem: &ingest.OSFileSystem{}, source: source}
		profiler := &ingest.IndexProfiler{}
		parses, builds := &atomic.Int64{}, &atomic.Int64{}
		transcript := new([]byte)
		result := runManagedRefreshPipelineInstrumented(t, counted, empty, output, parses, builds, transcript, fs, profiler)
		if result.Summary.Errors != 0 || result.Census == nil || len(result.Census.Rows) != 1 || result.Census.Rows[0].Lane != ingest.RefreshLaneStoredMetadata {
			t.Fatalf("refresh run summary %+v, census %+v", result.Summary, result.Census)
		}
		return counted, fs, profiler, result, parses, builds, transcript
	}
	assertProfile := func(t *testing.T, p *ingest.IndexProfiler, proven int) {
		t.Helper()
		for _, stage := range p.StageTotals() {
			if stage.Stage == ingest.StageRefreshProof {
				if stage.Total != 1 || stage.Done != proven {
					t.Fatalf("proof profile = %+v, want one attempt and %d proven", stage, proven)
				}
				return
			}
		}
		t.Fatal("missing proof profile stage")
	}
	routes := map[string]func(*testing.T){
		"proven": func(t *testing.T) {
			source, output, empty, id, db, _ := setup(t)
			before := managedRefreshPipelineRows(t, db, `SELECT c.turn_count || ':' || c.conversation_turn_count || ':' || c.input_submission_count || ':' || s.input_submission_count FROM sessions s JOIN session_captured_stats c USING (session_id) WHERE s.session_id = ?`, id)
			counted, _, profiler, _, parses, builds, _ := run(t, db, source, output, empty, "")
			if counted.proofs.Load() != 1 || !counted.proof.Proven || counted.settles.Load() != 1 || counted.calls.Load() != 0 || parses.Load() != 0 || builds.Load() != 0 {
				t.Fatalf("proof %+v attempts %d settles %d copies %d parses %d builds %d", counted.proof, counted.proofs.Load(), counted.settles.Load(), counted.calls.Load(), parses.Load(), builds.Load())
			}
			assertProfile(t, profiler, 1)
			assertManagedRefreshRowsEqual(t, "parse-owned counts", before, managedRefreshPipelineRows(t, db, `SELECT c.turn_count || ':' || c.conversation_turn_count || ':' || c.input_submission_count || ':' || s.input_submission_count FROM sessions s JOIN session_captured_stats c USING (session_id) WHERE s.session_id = ?`, id))
			next := runManagedRefreshPipelineInstrumented(t, counted, empty, output, parses, builds, new([]byte), &ingest.OSFileSystem{}, &ingest.IndexProfiler{})
			if next.Summary.Errors != 0 || next.Census == nil || next.Census.Sessions() != 0 || counted.proofs.Load() != 1 || counted.settles.Load() != 1 || counted.calls.Load() != 0 || parses.Load() != 0 || builds.Load() != 0 {
				t.Fatalf("second run did refresh work: summary %+v census %+v", next.Summary, next.Census)
			}
			locations, err := db.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{id})
			if err != nil {
				t.Fatal(err)
			}
			cfg := makePipelineConfig(output)
			pipeline, err := newTestPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg)
			if err != nil {
				t.Fatal(err)
			}
			decision := ingest.StoredDecisionForTest(pipeline, ingest.DiscoveredSession{SessionID: id, Harness: ingest.HarnessClaudeCode}, locations[id])
			if decision.Reason != ingest.RefreshReasonNone || decision.Lane != ingest.RefreshLaneNone {
				t.Fatalf("second decision = %+v, want none", decision)
			}
		},
		"index-not-current": func(t *testing.T) {
			source, output, empty, _, db, expected := setup(t)
			conn, err := db.Pool().Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = sqlitex.Execute(conn, `UPDATE sessions SET index_version = 0`, nil)
			db.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			counted, fs, profiler, _, _, builds, transcript := run(t, db, source, output, empty, "")
			if counted.proofs.Load() != 1 || counted.proof.Proven || counted.proof.Mismatch != ingest.UnchangedNotProvenIndexer || counted.settles.Load() != 0 || counted.calls.Load() != 0 || builds.Load() != 1 || !bytes.Equal(*transcript, expected) || fs.reads.Load() != 0 {
				t.Fatalf("proof %+v attempts %d settles %d copies %d builds %d stored bytes match %t native reads %d", counted.proof, counted.proofs.Load(), counted.settles.Load(), counted.calls.Load(), builds.Load(), bytes.Equal(*transcript, expected), fs.reads.Load())
			}
			assertProfile(t, profiler, 0)
		},
		"metadata-context": func(t *testing.T) {
			source, output, empty, _, db, _ := setup(t)
			stored := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
			if managedRefreshPipelineChangeMetadata(t, output, stored, managedRefreshPipelineForcedProject) != 1 {
				t.Fatal("expected one metadata file")
			}
			counted, _, profiler, _, _, builds, _ := run(t, db, source, output, empty, "")
			if counted.proofs.Load() != 1 || counted.proof.Proven || counted.proof.Mismatch != ingest.UnchangedNotProvenMetadata || counted.settles.Load() != 0 || counted.calls.Load() != 1 || builds.Load() != 0 {
				t.Fatalf("proof %+v attempts %d settles %d copies %d builds %d", counted.proof, counted.proofs.Load(), counted.settles.Load(), counted.calls.Load(), builds.Load())
			}
			assertProfile(t, profiler, 0)
		},
		"copy-refused": func(t *testing.T) {
			source, output, empty, _, db, expected := setup(t)
			stored := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
			if managedRefreshPipelineChangeMetadata(t, output, stored, managedRefreshPipelineForcedProject) != 1 {
				t.Fatal("expected one metadata file")
			}
			counted, fs, profiler, _, _, builds, transcript := run(t, db, source, output, empty, "copy-refused")
			if counted.proofs.Load() != 1 || counted.proof.Mismatch != ingest.UnchangedNotProvenMetadata || counted.settles.Load() != 0 || counted.calls.Load() != 1 || builds.Load() != 1 || !bytes.Equal(*transcript, expected) || fs.reads.Load() != 0 {
				t.Fatalf("proof %+v attempts %d settles %d copies %d builds %d stored bytes match %t native reads %d", counted.proof, counted.proofs.Load(), counted.settles.Load(), counted.calls.Load(), builds.Load(), bytes.Equal(*transcript, expected), fs.reads.Load())
			}
			assertProfile(t, profiler, 0)
		},
		"proof-error": func(t *testing.T) {
			source, output, empty, _, db, expected := setup(t)
			counted, fs, profiler, _, _, builds, transcript := run(t, db, source, output, empty, "proof-error")
			if counted.proofs.Load() != 1 || counted.settles.Load() != 0 || counted.calls.Load() != 0 || builds.Load() != 1 || !bytes.Equal(*transcript, expected) || fs.reads.Load() != 0 {
				t.Fatalf("proofs %d settles %d copies %d builds %d bytes match %t native reads %d", counted.proofs.Load(), counted.settles.Load(), counted.calls.Load(), builds.Load(), bytes.Equal(*transcript, expected), fs.reads.Load())
			}
			assertProfile(t, profiler, 0)
		},
		"settle-refused": func(t *testing.T) {
			source, output, empty, _, db, expected := setup(t)
			counted, fs, profiler, _, _, builds, transcript := run(t, db, source, output, empty, "settle-refused")
			if counted.proofs.Load() != 1 || !counted.proof.Proven || counted.settles.Load() != 1 || counted.calls.Load() != 0 || builds.Load() != 1 || !bytes.Equal(*transcript, expected) || fs.reads.Load() != 0 {
				t.Fatalf("proof %+v attempts %d settles %d copies %d builds %d bytes match %t native reads %d", counted.proof, counted.proofs.Load(), counted.settles.Load(), counted.calls.Load(), builds.Load(), bytes.Equal(*transcript, expected), fs.reads.Load())
			}
			assertProfile(t, profiler, 1)
		},
	}
	var fixture struct {
		Cases []struct {
			Name string `yaml:"name"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(unchangedPipelineRoutes, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(unchangedPipelineRoutesManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range fixture.Cases {
		fn, ok := routes[c.Name]
		if !ok || seen[c.Name] {
			t.Fatalf("unknown or duplicate route %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, fn)
	}
	for _, name := range manifest.RequiredNames {
		if !seen[name] {
			t.Errorf("missing required route %q", name)
		}
	}
}

func (s *countedManagedRefreshStore) RefreshManagedGenerationWithoutParse(ctx context.Context, request ingest.ManagedGenerationRefreshRequest) (ingest.ManagedGenerationRefreshOutcome, error) {
	s.calls.Add(1)
	if s.refuse {
		return ingest.ManagedGenerationRefreshOutcome{}, &ingest.StaleIndexWorkError{SessionID: request.SessionID}
	}
	if s.forgeCounts {
		input, conversation := int64(903), int64(902)
		request.Metadata.Stats.TurnCount = 901
		request.Metadata.Stats.InputSubmissionCount = &input
		request.Metadata.Stats.ConversationTurnCount = &conversation
	}
	return s.Store.RefreshManagedGenerationWithoutParse(ctx, request)
}

func TestManagedGenerationOutputPreservingDeclaration(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name       string `yaml:"name"`
			Declared   bool   `yaml:"declared"`
			Diagnostic bool   `yaml:"diagnostic"`
			Builds     int64  `yaml:"builds"`
			Copies     int64  `yaml:"copies"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(outputPreservingPipeline, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(outputPreservingPipelineManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	required := make(map[string]bool, len(manifest.RequiredNames))
	for _, name := range manifest.RequiredNames {
		required[name] = false
	}
	for _, tc := range fixture.Cases {
		if seen, ok := required[tc.Name]; !ok || seen {
			t.Fatalf("unknown or repeated declaration case %q", tc.Name)
		}
		required[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			source, output, empty := filepath.Join(root, "source"), filepath.Join(root, "managed"), filepath.Join(root, "empty")
			mustMkdir(t, source)
			mustMkdir(t, empty)
			id := writeManagedRefreshPipelineCorpus(t, source, 1, 8)[0]
			db := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
			t.Cleanup(func() { _ = db.Close() })
			initial := runManagedRefreshPipeline(t, db, source, output)
			if initial.Summary.Errors != 0 || initial.Summary.Indexed != 1 {
				t.Fatalf("initial import: %+v", initial.Summary)
			}
			previous := managedRefreshPipelineActiveGeneration(t, db, id)
			stored := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
			managedRefreshPipelineDowngrade(t, db, stored)
			if managedRefreshPipelineChangeMetadata(t, output, stored, managedRefreshPipelineForcedProject) != 1 {
				t.Fatal("expected one changed metadata file")
			}
			adapters := maps.Clone(ingest.DefaultAdapterRegistry)
			if tc.Declared {
				adapters[ingest.HarnessClaudeCode] = func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
					return &outputPreservingClaudeAdapter{ClaudeAdapter: ingest.NewClaudeAdapter(fs, git, s)}
				}
			}
			counted := &proofCountingRefreshStore{countedManagedRefreshStore: &countedManagedRefreshStore{Store: db}}
			var parses, builds atomic.Int64
			var transcript []byte
			result := runManagedRefreshPipelineWithAdapters(t, counted, empty, output, &parses, &builds, &transcript, &ingest.OSFileSystem{}, &ingest.IndexProfiler{}, adapters)
			if result.Summary.Errors != 0 || result.Summary.StoreError != nil || counted.proof.Mismatch != ingest.UnchangedNotProvenMetadata || counted.settles.Load() != 0 || builds.Load() != tc.Builds || counted.calls.Load() != tc.Copies {
				t.Fatalf("summary %+v proof %+v settles %d builds %d copies %d", result.Summary, counted.proof, counted.settles.Load(), builds.Load(), counted.calls.Load())
			}
			diagnostics := 0
			for _, diagnostic := range result.Diagnostics {
				if diagnostic.ErrorType == "output_preserving_proof_failed" && diagnostic.Location == string(id) {
					diagnostics++
				}
			}
			if (tc.Diagnostic && diagnostics != 1) || (!tc.Diagnostic && diagnostics != 0) {
				t.Fatalf("declaration diagnostics = %d; all diagnostics: %+v", diagnostics, result.Diagnostics)
			}
			active := managedRefreshPipelineActiveGeneration(t, db, id)
			if active == previous || managedRefreshPipelineProjectName(t, db, id, active) != managedRefreshPipelineForcedProject {
				t.Fatalf("refreshed generation %q did not install the changed context", active)
			}
		})
	}
	for name, seen := range required {
		if !seen {
			t.Errorf("missing required declaration case %q", name)
		}
	}
}

func TestManagedGenerationRefreshPipelineLanes(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name     string `yaml:"name"`
			Mutation string `yaml:"mutation"`
			Reason   string `yaml:"reason"`
			Refresh  bool   `yaml:"refresh"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(managedRefreshPipelineLanes, &fixture); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(managedRefreshPipelineLanesManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range fixture.Cases {
		names[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			source, output := filepath.Join(root, "source"), filepath.Join(root, "managed")
			mustMkdir(t, source)
			writeManagedRefreshPipelineCorpus(t, source, 1, 8)
			base := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
			t.Cleanup(func() { _ = base.Close() })
			runManagedRefreshPipeline(t, base, source, output)
			stored := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
			switch c.Mutation {
			case "adapter-behind":
				managedRefreshPipelineDowngrade(t, base, stored)
				managedRefreshPipelineChangeMetadata(t, output, stored, managedRefreshPipelineForcedProject)
			case "adapter-absent":
				managedRefreshPipelineDowngrade(t, base, -1)
				managedRefreshPipelineChangeMetadata(t, output, -1, managedRefreshPipelineForcedProject)
			case "indexer-behind":
				conn, err := base.Pool().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.Execute(conn, `UPDATE sessions SET index_version = 0`, nil)
				base.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown lane mutation %q", c.Mutation)
			}
			empty := filepath.Join(root, "empty")
			mustMkdir(t, empty)
			counted := &countedManagedRefreshStore{Store: base}
			var parses atomic.Int64
			result := runManagedRefreshPipelineCounting(t, counted, empty, output, &parses)
			if result.Summary.Errors != 0 || result.Census == nil || len(result.Census.Rows) != 1 {
				t.Fatalf("run summary %+v census %+v", result.Summary, result.Census)
			}
			if got := result.Census.Rows[0].Reason.Code(); got != c.Reason {
				t.Fatalf("reason = %q, want %q", got, c.Reason)
			}
			if c.Refresh && (counted.calls.Load() != 1 || parses.Load() != 0) {
				t.Fatalf("refresh calls %d, parses %d; want 1 and 0", counted.calls.Load(), parses.Load())
			}
			if !c.Refresh && (counted.calls.Load() != 0 || parses.Load() == 0) {
				t.Fatalf("refresh calls %d, parses %d; want 0 and positive", counted.calls.Load(), parses.Load())
			}
		})
	}
	for _, name := range manifest.RequiredNames {
		if !names[name] {
			t.Fatalf("missing required lane %q", name)
		}
	}
}

// TestManagedGenerationMetadataRefreshRunsThroughThePipeline is the production
// path for a changed metadata context on an adapter bump. The stored metadata
// extraction is followed by a copy refresh without parsing. A following
// transcript change takes the ordinary native parse again.
func TestManagedGenerationMetadataRefreshRunsThroughThePipeline(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	output := filepath.Join(root, "managed")
	mustMkdir(t, source)
	sessionIDs := writeManagedRefreshPipelineCorpus(t, source, 2, 24)

	db := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the managed store: %v", err)
		}
	})

	first := runManagedRefreshPipeline(t, db, source, output)
	if first.Summary.New != len(sessionIDs) || first.Summary.Errors != 0 || first.Summary.Indexed != len(sessionIDs) {
		t.Fatalf("initial import summary = %+v, want %d new, indexed and no errors", first.Summary, len(sessionIDs))
	}
	initial := make(map[ingest.SessionID]string, len(sessionIDs))
	initialCounts := managedRefreshPipelineRows(t, db, `SELECT c.turn_count || ':' || c.conversation_turn_count || ':' || c.input_submission_count || ':' || s.input_submission_count FROM sessions s JOIN session_captured_stats c USING (session_id) WHERE s.session_id = ?`, sessionIDs[0])
	for _, sid := range sessionIDs {
		initial[sid] = managedRefreshPipelineActiveGeneration(t, db, sid)
	}
	initialEntries := managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || entry_index || ':' || source_entry_ref || ':' || body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, sessionIDs[0], initial[sessionIDs[0]])
	initialAliases := managedRefreshPipelineRows(t, db, `SELECT native_key || ':' || source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? ORDER BY native_key`, sessionIDs[0], initial[sessionIDs[0]])
	initialSections := managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || COALESCE(earlier_state, '') FROM session_projection_sections WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, sessionIDs[0], initial[sessionIDs[0]])
	if len(initialEntries) == 0 {
		t.Fatal("the initial managed generation stored no entries")
	}

	// A version-only bump with a forced metadata change: the stored adapter
	// revision moves below the managed target and the project name moves in the
	// stored metadata. The transcript bytes stay untouched.
	storedAdapter := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
	if storedAdapter < 1 {
		t.Fatalf("claude managed adapter target %d cannot be downgraded", storedAdapter+1)
	}
	managedRefreshPipelineDowngrade(t, db, storedAdapter)
	if changed := managedRefreshPipelineChangeMetadata(t, output, storedAdapter, managedRefreshPipelineForcedProject); changed != len(sessionIDs) {
		t.Fatalf("changed %d metadata files, want %d", changed, len(sessionIDs))
	}

	emptySource := filepath.Join(root, "empty-source")
	mustMkdir(t, emptySource)
	var parses atomic.Int64
	refresher := &countedManagedRefreshStore{Store: db, forgeCounts: true}
	refreshed := runManagedRefreshPipelineCounting(t, refresher, emptySource, output, &parses)
	if refresher.calls.Load() != int64(len(sessionIDs)) {
		t.Fatalf("copy refresh requests = %d, want %d", refresher.calls.Load(), len(sessionIDs))
	}
	if parses.Load() != 0 {
		t.Fatalf("metadata refresh invoked parser %d times, want zero", parses.Load())
	}
	assertManagedRefreshRowsEqual(t, "parse counts", initialCounts, managedRefreshPipelineRows(t, db, `SELECT c.turn_count || ':' || c.conversation_turn_count || ':' || c.input_submission_count || ':' || s.input_submission_count FROM sessions s JOIN session_captured_stats c USING (session_id) WHERE s.session_id = ?`, sessionIDs[0]))
	if refreshed.Summary.Errors != 0 || refreshed.Summary.New != 0 || refreshed.Summary.Unchanged != 0 {
		t.Fatalf("metadata-only refresh summary = %+v, want updated only and no errors", refreshed.Summary)
	}
	if refreshed.Summary.Updated != len(sessionIDs) {
		t.Fatalf("metadata-only refresh updated %d sessions, want %d", refreshed.Summary.Updated, len(sessionIDs))
	}
	if refreshed.Summary.Indexed != 0 {
		t.Fatalf("metadata refresh indexed %d sessions, want zero", refreshed.Summary.Indexed)
	}
	if refreshed.Census == nil || refreshed.Census.Sessions() != len(sessionIDs) {
		t.Fatalf("refresh census = %+v, want %d sessions", refreshed.Census, len(sessionIDs))
	}
	for _, row := range refreshed.Census.Rows {
		if row.Reason != ingest.OutcomeReasonRefreshAdapterBehind || row.Lane != ingest.RefreshLaneStoredMetadata {
			t.Fatalf("refresh census row = %+v, want adapter_behind on the stored-metadata lane", row)
		}
	}
	if len(refreshed.IndexLog) != len(sessionIDs) {
		t.Fatalf("metadata-only refresh produced %d index rows, want one per session", len(refreshed.IndexLog))
	}
	for _, entry := range refreshed.IndexLog {
		if entry.Outcome != ingest.IndexOutcomeSkipped {
			t.Fatalf("refresh index outcome for session %s = %q, want skipped", entry.SessionID, entry.Outcome)
		}
		if entry.ParseDuration != nil && *entry.ParseDuration > 0 {
			t.Fatalf("refresh parsed session %s", entry.SessionID)
		}
		if entry.WriteDuration == nil || *entry.WriteDuration <= 0 {
			t.Fatalf("refresh reported no generation write time for session %s", entry.SessionID)
		}
	}
	for _, sid := range sessionIDs {
		active := managedRefreshPipelineActiveGeneration(t, db, sid)
		if active == initial[sid] {
			t.Fatalf("session %s kept the source generation active; want a refreshed generation", sid)
		}
		if name := managedRefreshPipelineProjectName(t, db, sid, active); name != managedRefreshPipelineForcedProject {
			t.Fatalf("refreshed generation of session %s records project %q, want %q", sid, name, managedRefreshPipelineForcedProject)
		}
	}
	// The copy refresh retains entries, aliases and sections without parsing.
	refreshedEntries := managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || entry_index || ':' || source_entry_ref || ':' || body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, sessionIDs[0], managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]))
	refreshedAliases := managedRefreshPipelineRows(t, db, `SELECT native_key || ':' || source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? ORDER BY native_key`, sessionIDs[0], managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]))
	refreshedSections := managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || COALESCE(earlier_state, '') FROM session_projection_sections WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, sessionIDs[0], managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]))
	assertManagedRefreshRowsEqual(t, "entries", initialEntries, refreshedEntries)
	assertManagedRefreshRowsEqual(t, "aliases", initialAliases, refreshedAliases)
	assertManagedRefreshRowsEqual(t, "sections", initialSections, refreshedSections)
	// The copy refresh flags its superseded source for sweep.
	if pending := managedRefreshPipelineInt(t, db, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, sessionIDs[0]); pending != 1 {
		t.Fatalf("refreshed session sweep flag = %d, want 1", pending)
	}
	refreshedAuthority := managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0])
	if err := db.SweepSessionForHarvest(t.Context(), sessionIDs[0]); err != nil {
		t.Fatalf("sweep the superseded generation: %v", err)
	}
	if rows := managedRefreshPipelineInt(t, db, `SELECT count(*) FROM session_generations WHERE session_id = ? AND generation_id = ?`, sessionIDs[0], initial[sessionIDs[0]]); rows != 0 {
		t.Fatalf("superseded generation rows after sweep = %d, want 0", rows)
	}
	if active := managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]); active != refreshedAuthority {
		t.Fatalf("active generation after sweep = %q, want the refreshed authority %q", active, refreshedAuthority)
	}

	// A following transcript change re-enters the native parse path and writes
	// a new generation from the changed bytes.
	appendManagedRefreshPipelineRecord(t, filepath.Join(source, "-workspace", string(sessionIDs[0])+".jsonl"), string(sessionIDs[0]))
	parses.Store(0)
	changed := runManagedRefreshPipelineCounting(t, db, source, output, &parses)
	if parses.Load() == 0 {
		t.Fatal("changed input did not invoke the generation builder or parser")
	}
	if changed.Summary.Errors != 0 || changed.Summary.Indexed != 1 {
		t.Fatalf("changed-source run summary = %+v, want one re-indexed session and no errors", changed.Summary)
	}
	if active := managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0]); active == refreshedAuthority {
		t.Fatal("a changed transcript left the old generation active; want a new parsed generation")
	}
	if len(changed.IndexLog) == 0 {
		t.Fatal("a changed transcript produced no index row")
	}
	parsed := false
	for _, entry := range changed.IndexLog {
		if entry.SessionID != sessionIDs[0] || entry.ParseDuration == nil {
			continue
		}
		if *entry.ParseDuration > 0 {
			parsed = true
		}
	}
	if !parsed {
		t.Fatal("a changed transcript was settled without a parse")
	}
}

// staleManagedRefreshStore is a real store whose metadata-only refresh reports
// a stale captured proof. It exercises the pipeline's fallback to the ordinary
// parse path; every other store method is the real implementation.
type staleManagedRefreshStore struct {
	*store.Store
}

func (s staleManagedRefreshStore) RefreshManagedGenerationWithoutParse(context.Context, ingest.ManagedGenerationRefreshRequest) (ingest.ManagedGenerationRefreshOutcome, error) {
	return ingest.ManagedGenerationRefreshOutcome{}, &ingest.StaleIndexWorkError{SessionID: ingest.SessionID("stale")}
}

// TestManagedGenerationRefreshStaleProofFallsBackToTheParse pins the fallback:
// a refused refresh changes no authority and the ordinary parse path still
// runs, so the session settles with the refreshed capture instead of silently
// keeping a stale generation.
func TestManagedGenerationRefreshStaleProofFallsBackToTheParse(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	output := filepath.Join(root, "managed")
	mustMkdir(t, source)
	sessionIDs := writeManagedRefreshPipelineCorpus(t, source, 1, 8)

	base := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
	t.Cleanup(func() {
		if err := base.Close(); err != nil {
			t.Errorf("close the managed store: %v", err)
		}
	})
	first := runManagedRefreshPipeline(t, base, source, output)
	if first.Summary.New != 1 || first.Summary.Errors != 0 {
		t.Fatalf("initial import summary = %+v", first.Summary)
	}
	initial := managedRefreshPipelineActiveGeneration(t, base, sessionIDs[0])

	storedAdapter := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
	managedRefreshPipelineDowngrade(t, base, storedAdapter)
	if changed := managedRefreshPipelineChangeMetadata(t, output, storedAdapter, managedRefreshPipelineForcedProject); changed != 1 {
		t.Fatalf("changed %d metadata files, want 1", changed)
	}
	emptySource := filepath.Join(root, "empty-source")
	mustMkdir(t, emptySource)

	stale := staleManagedRefreshStore{Store: base}
	result := runManagedRefreshPipelineWithStore(t, stale, emptySource, output)
	if result.Summary.Errors != 0 || result.Summary.Updated != 1 {
		t.Fatalf("fallback summary = %+v, want one updated session and no errors", result.Summary)
	}
	if result.Summary.Indexed != 1 {
		t.Fatalf("fallback indexed %d sessions, want the ordinary parse path to run", result.Summary.Indexed)
	}
	active := managedRefreshPipelineActiveGeneration(t, base, sessionIDs[0])
	if active == initial {
		t.Fatal("fallback left the source generation active; the ordinary parse must install the refreshed capture")
	}
	if name := managedRefreshPipelineProjectName(t, base, sessionIDs[0], active); name != managedRefreshPipelineForcedProject {
		t.Fatalf("fallback generation project = %q, want the refreshed capture %q", name, managedRefreshPipelineForcedProject)
	}
}

func runManagedRefreshPipeline(t *testing.T, db *store.Store, source, output string) *ingest.PipelineResult {
	t.Helper()
	return runManagedRefreshPipelineWithStore(t, db, source, output)
}

func runManagedRefreshPipelineWithStore(t *testing.T, db managedRefreshPipelineStore, source, output string) *ingest.PipelineResult {
	return runManagedRefreshPipelineCounting(t, db, source, output, nil)
}

type countingManagedRefreshIndexer struct {
	*ingest.ClaudeIndexer
	calls      *atomic.Int64
	builds     *atomic.Int64
	transcript *[]byte
}

func (i countingManagedRefreshIndexer) BuildGeneration(ctx context.Context, input ingest.GenerationBuildInput) (ingest.GenerationCandidate, error) {
	if i.builds != nil {
		i.builds.Add(1)
		*i.transcript = append([]byte(nil), input.CapturedTranscript...)
	} else {
		i.calls.Add(1)
	}
	return i.ClaudeIndexer.BuildGeneration(ctx, input)
}

func (i countingManagedRefreshIndexer) IndexTranscript(ctx context.Context, session ingest.DiscoveredSession) ([]schema.SessionEntry, error) {
	i.calls.Add(1)
	return i.ClaudeIndexer.IndexTranscript(ctx, session)
}

func (i countingManagedRefreshIndexer) IndexTranscriptBytes(ctx context.Context, session ingest.DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	i.calls.Add(1)
	return i.ClaudeIndexer.IndexTranscriptBytes(ctx, session, data)
}

func (i countingManagedRefreshIndexer) IndexTranscriptResult(ctx context.Context, session ingest.DiscoveredSession) (indexformat.Result, error) {
	i.calls.Add(1)
	return i.ClaudeIndexer.IndexTranscriptResult(ctx, session)
}

func (i countingManagedRefreshIndexer) IndexTranscriptBytesResult(ctx context.Context, session ingest.DiscoveredSession, data []byte) (indexformat.Result, error) {
	i.calls.Add(1)
	return i.ClaudeIndexer.IndexTranscriptBytesResult(ctx, session, data)
}

func (i countingManagedRefreshIndexer) IndexTranscriptForCapture(ctx context.Context, session ingest.DiscoveredSession) (ingest.TranscriptCaptureResult, error) {
	i.calls.Add(1)
	return i.ClaudeIndexer.IndexTranscriptForCapture(ctx, session)
}

func (i countingManagedRefreshIndexer) IndexTranscriptBytesForCapture(ctx context.Context, session ingest.DiscoveredSession, data []byte) (ingest.TranscriptCaptureResult, error) {
	i.calls.Add(1)
	return i.ClaudeIndexer.IndexTranscriptBytesForCapture(ctx, session, data)
}

func runManagedRefreshPipelineCounting(t *testing.T, db managedRefreshPipelineStore, source, output string, calls *atomic.Int64) *ingest.PipelineResult {
	return runManagedRefreshPipelineInstrumented(t, db, source, output, calls, nil, nil, &ingest.OSFileSystem{}, nil)
}

func runManagedRefreshPipelineInstrumented(t *testing.T, db managedRefreshPipelineStore, source, output string, calls, builds *atomic.Int64, transcript *[]byte, fs ingest.FileSystem, profiler *ingest.IndexProfiler) *ingest.PipelineResult {
	return runManagedRefreshPipelineWithAdapters(t, db, source, output, calls, builds, transcript, fs, profiler, ingest.DefaultAdapterRegistry)
}

func runManagedRefreshPipelineWithAdapters(t *testing.T, db managedRefreshPipelineStore, source, output string, calls, builds *atomic.Int64, transcript *[]byte, fs ingest.FileSystem, profiler *ingest.IndexProfiler, adapters map[ingest.Harness]ingest.AdapterFactory) *ingest.PipelineResult {
	t.Helper()
	cfg := makePipelineConfig(output)
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}, Enabled: true},
	}
	cfg.StalenessThreshold = 0
	cfg.IndexProfiler = profiler
	indexers := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})
	if calls != nil {
		indexers[ingest.HarnessClaudeCode] = countingManagedRefreshIndexer{
			ClaudeIndexer: ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true)),
			calls:         calls,
			builds:        builds,
			transcript:    transcript,
		}
	}
	pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), adapters, cfg,
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(indexers),
		ingest.WithLogger(db),
		ingest.WithIndexLogger(db),
	)
	if err != nil {
		t.Fatalf("build managed refresh pipeline: %v", err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("run managed refresh pipeline: %v", err)
	}
	return result
}

type managedRefreshPipelineStore interface {
	ingest.SessionStore
	ingest.MetricsStore
	ingest.IngestLogger
	ingest.IndexLogger
	InstallationSalt() salt.Salt
}

func openManagedRefreshPipelineStore(t *testing.T, dbDir, output string) *store.Store {
	t.Helper()
	mustMkdir(t, dbDir)
	options, err := store.ManagedGenerationOptions(output)
	if err != nil {
		t.Fatalf("managed generation options: %v", err)
	}
	db, err := store.Open(filepath.Join(dbDir, "peasant.db"), options...)
	if err != nil {
		t.Fatalf("open managed generation store: %v", err)
	}
	return db
}

// writeManagedRefreshPipelineCorpus writes one deterministic synthetic Claude
// JSONL file per session. The text is synthetic and carries no user data.
func writeManagedRefreshPipelineCorpus(t *testing.T, root string, sessions, entries int) []ingest.SessionID {
	t.Helper()
	ids := make([]ingest.SessionID, 0, sessions)
	workspace := filepath.Join(root, "-workspace")
	mustMkdir(t, workspace)
	filler := strings.Repeat("synthetic transcript text for the managed refresh pipeline test. ", 6)
	for n := range sessions {
		id := ingest.SessionID(fmt.Sprintf("abc00000-0000-4000-8000-%012d", n+1))
		ids = append(ids, id)
		path := filepath.Join(workspace, string(id)+".jsonl")
		content := syntheticManagedRefreshPipelineTranscript(string(id), entries, filler)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(1_700_000_000+int64(n), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func syntheticManagedRefreshPipelineTranscript(sessionID string, entries int, filler string) string {
	var content strings.Builder
	for n := range entries {
		at := time.Unix(1_700_000_000+int64(n), 0).UTC().Format(time.RFC3339)
		if n%2 == 0 {
			fmt.Fprintf(&content, `{"type":"user","sessionId":"%s","uuid":"u%d","cwd":"/work/project","timestamp":"%s","message":{"role":"user","content":"prompt %d: %s"}}`+"\n", sessionID, n, at, n, filler)
			continue
		}
		fmt.Fprintf(&content, `{"type":"assistant","sessionId":"%s","uuid":"a%d","parentUuid":"u%d","cwd":"/work/project","timestamp":"%s","message":{"role":"assistant","model":"claude-synthetic","content":[{"type":"text","text":"reply %d: %s"}],"usage":{"input_tokens":120,"output_tokens":80}}}`+"\n", sessionID, n, n-1, at, n, filler)
	}
	return content.String()
}

func appendManagedRefreshPipelineRecord(t *testing.T, path, sessionID string) {
	t.Helper()
	at := time.Now().UTC().Format(time.RFC3339)
	record := fmt.Sprintf(`{"type":"user","sessionId":"%s","uuid":"u-appended","cwd":"/work/project","timestamp":"%s","message":{"role":"user","content":"appended transcript change"}}`+"\n", sessionID, at)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(record); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func managedRefreshPipelineDowngrade(t *testing.T, db *store.Store, stored int) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var value any = stored
	if stored < 0 {
		value = nil
	}
	if err := sqlitex.Execute(conn, `UPDATE sessions SET adapter_version = ?`, &sqlitex.ExecOptions{Args: []any{value}}); err != nil {
		t.Fatalf("downgrade session adapter rows: %v", err)
	}
}

func managedRefreshPipelineChangeMetadata(t *testing.T, output string, stored int, projectName string) int {
	t.Helper()
	var files []string
	if err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), defaults.MetadataSuffix) {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var metadata schema.UnifiedMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			t.Fatalf("decode managed metadata %s: %v", path, err)
		}
		if stored < 0 {
			metadata.AdapterVersion = nil
		} else {
			metadata.AdapterVersion = &stored
		}
		metadata.Project.Name = projectName
		metadata.MetadataHash = schema.ComputeMetadataHash(&metadata)
		encoded, err := json.Marshal(&metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return len(files)
}

func managedRefreshPipelineActiveGeneration(t *testing.T, db *store.Store, sid ingest.SessionID) string {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var active string
	if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(active_generation_id, '') FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			active = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read active generation for %s: %v", sid, err)
	}
	if active == "" {
		t.Fatalf("session %s has no active generation", sid)
	}
	return active
}

func managedRefreshPipelineProjectName(t *testing.T, db *store.Store, sid ingest.SessionID, generationID string) string {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var name string
	if err := sqlitex.ExecuteTransient(conn, `SELECT project_name FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			name = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read generation project name for %s: %v", sid, err)
	}
	return name
}

func managedRefreshPipelineRows(t *testing.T, db *store.Store, query string, args ...any) []string {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var rows []string
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("query generation rows: %v", err)
	}
	return rows
}

func managedRefreshPipelineInt(t *testing.T, db *store.Store, query string, args ...any) int64 {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var value int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			value = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("query generation count: %v", err)
	}
	return value
}

func assertManagedRefreshRowsEqual(t *testing.T, label string, before, after []string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s row count changed: %d -> %d", label, len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s row %d changed: %q -> %q", label, i, before[i], after[i])
		}
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
