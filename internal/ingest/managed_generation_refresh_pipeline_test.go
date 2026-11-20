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

//go:embed testdata/validation_pipeline_seam.yaml
var validationPipelineSeam []byte

//go:embed testdata/validation_pipeline_seam.manifest.yaml
var validationPipelineSeamManifest []byte

type outputPreservingClaudeAdapter struct {
	*ingest.ClaudeAdapter
}

func (*outputPreservingClaudeAdapter) DeclaresOutputPreserving() bool { return true }

var _ ingest.OutputPreservingDeclaration = (*outputPreservingClaudeAdapter)(nil)

type proofCountingRefreshStore struct {
	*store.Store
	proofs            atomic.Int64
	settles           atomic.Int64
	proof             ingest.UnchangedProof
	proofError        bool
	settleRefused     bool
	forgeSettleCounts bool
	validationCalls   atomic.Int64
	forceUnproven     bool
	contextUnproven   bool
	validationResult  *ingest.ManagedGenerationValidationResult
	validationRequest ingest.ManagedGenerationValidationRequest
	validationError   bool
	validationMissing bool
}

func (s *proofCountingRefreshStore) ProveCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.UnchangedProof, error) {
	s.proofs.Add(1)
	if s.proofError {
		return ingest.UnchangedProof{}, fmt.Errorf("synthetic proof read failure")
	}
	proof, err := s.Store.ProveCapturedGenerationUnchanged(ctx, request)
	if s.forceUnproven && err == nil {
		proof = ingest.UnchangedProof{Mismatch: ingest.UnchangedNotProvenSeal}
	}
	if s.contextUnproven && err == nil {
		proof = ingest.UnchangedProof{Mismatch: ingest.UnchangedNotProvenMetadata}
	}
	s.proof = proof
	return proof, err
}

func (s *proofCountingRefreshStore) ValidateManagedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationValidationRequest) (ingest.ManagedGenerationValidationResult, error) {
	if s.validationMissing {
		return ingest.ManagedGenerationValidationResult{Outcome: ingest.ValidationUnsupported}, nil
	}
	s.validationCalls.Add(1)
	s.validationRequest = request
	if s.validationError {
		return ingest.ManagedGenerationValidationResult{}, fmt.Errorf("synthetic validation error")
	}
	if s.validationResult != nil {
		return *s.validationResult, nil
	}
	return s.Store.ValidateManagedGenerationUnchanged(ctx, request)
}

func (s *proofCountingRefreshStore) SettleCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.ManagedGenerationUnchangedSettleOutcome, error) {
	s.settles.Add(1)
	if s.settleRefused {
		return ingest.ManagedGenerationUnchangedSettleOutcome{}, &ingest.StaleIndexWorkError{SessionID: request.SessionID}
	}
	if s.contextUnproven && s.validationResult != nil && s.validationResult.Outcome == ingest.ValidationProven {
		if request.Validated == nil || request.Validated.GenerationID != request.Expected.ActiveGenerationID ||
			s.validationRequest.Candidate == nil || request.Validated.Candidate.Metadata.SessionID != s.validationRequest.Candidate.Metadata.SessionID {
			return ingest.ManagedGenerationUnchangedSettleOutcome{}, fmt.Errorf("synthetic settle: validated candidate was not forwarded")
		}
		return ingest.ManagedGenerationUnchangedSettleOutcome{GenerationID: request.Validated.GenerationID}, nil
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

func TestManagedGenerationValidationPipelineSeam(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name           string `yaml:"name"`
			Outcome        string `yaml:"outcome"`
			Mismatch       string `yaml:"mismatch"`
			Error          bool   `yaml:"error"`
			Missing        bool   `yaml:"missing"`
			ValidatorCalls int64  `yaml:"validatorCalls"`
			Builds         int64  `yaml:"builds"`
			Settles        int64  `yaml:"settles"`
			Unsupported    bool   `yaml:"unsupported"`
			Diagnostic     bool   `yaml:"diagnostic"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(validationPipelineSeam, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(validationPipelineSeamManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, tc := range fixture.Cases {
		if seen[tc.Name] {
			t.Fatalf("duplicate case %q", tc.Name)
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			source, output, empty := filepath.Join(root, "source"), filepath.Join(root, "managed"), filepath.Join(root, "empty")
			mustMkdir(t, source)
			mustMkdir(t, empty)
			id := writeManagedRefreshPipelineCorpus(t, source, 1, 8)[0]
			db := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
			t.Cleanup(func() { _ = db.Close() })
			runManagedRefreshPipeline(t, db, source, output)
			generationID := managedRefreshPipelineActiveGeneration(t, db, id)
			managedRefreshPipelineDowngrade(t, db, ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion-1)
			counted := &proofCountingRefreshStore{Store: db, contextUnproven: true, validationError: tc.Error, validationMissing: tc.Missing}
			if tc.Outcome != "" {
				outcome, err := ingest.NewValidationOutcome(tc.Outcome)
				if err != nil {
					t.Fatal(err)
				}
				mismatch, err := ingest.NewUnchangedProofMismatch(tc.Mismatch)
				if err != nil {
					t.Fatal(err)
				}
				// Deliberately allow the inconsistent pair to exercise the boundary.
				counted.validationResult = &ingest.ManagedGenerationValidationResult{Outcome: outcome, Mismatch: mismatch}
			}
			var parses, builds atomic.Int64
			var transcript []byte
			profiler := &ingest.IndexProfiler{}
			adapters := maps.Clone(ingest.DefaultAdapterRegistry)
			if tc.Diagnostic {
				adapters[ingest.HarnessClaudeCode] = func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
					return &outputPreservingClaudeAdapter{ClaudeAdapter: ingest.NewClaudeAdapter(fs, git, s)}
				}
			}
			runResult := runManagedRefreshPipelineWithAdapters(t, counted, empty, output, &parses, &builds, &transcript, &ingest.OSFileSystem{}, profiler, adapters)
			if runResult.Summary.Errors != 0 || counted.validationCalls.Load() != tc.ValidatorCalls {
				t.Fatalf("summary %+v validation calls %d", runResult.Summary, counted.validationCalls.Load())
			}
			if tc.ValidatorCalls != 0 && (counted.validationRequest.SessionID != id || counted.validationRequest.GenerationID != generationID) {
				t.Fatalf("validation request session %s generation %q, want %s %q", counted.validationRequest.SessionID, counted.validationRequest.GenerationID, id, generationID)
			}
			if builds.Load() != tc.Builds || counted.settles.Load() != tc.Settles {
				t.Fatalf("builds %d settles %d; want %d %d", builds.Load(), counted.settles.Load(), tc.Builds, tc.Settles)
			}
			if tc.Diagnostic {
				found := false
				for _, diagnostic := range runResult.Diagnostics {
					if diagnostic.ErrorType == "output_preserving_proof_failed" && strings.Contains(diagnostic.Message, tc.Mismatch) {
						found = true
					}
				}
				if !found {
					t.Fatalf("diagnostic does not carry mismatch %q: %+v", tc.Mismatch, runResult.Diagnostics)
				}
			}
			found, unsupported := false, false
			for _, stage := range profiler.StageTotals() {
				if stage.Stage == ingest.StageRefreshValidate {
					found = stage.Total == 1 && stage.Done == int(tc.Settles)
				}
				if stage.Stage == ingest.StageRefreshValidateUnsupported {
					unsupported = stage.Total == 1 && stage.Done == 0
				}
			}
			if !found || unsupported != (tc.Unsupported || tc.Missing || tc.Error) {
				t.Fatalf("validation profile attempts=%t unsupported=%t", found, unsupported)
			}
		})
	}
	for _, name := range manifest.RequiredNames {
		if !seen[name] {
			t.Errorf("missing case %q", name)
		}
	}
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
		unproven := mode == "unproven"
		counted := &proofCountingRefreshStore{Store: db, proofError: mode == "proof-error", settleRefused: mode == "settle-refused", forgeSettleCounts: mode == "", forceUnproven: unproven, validationMissing: unproven}
		counted.validationResult = &ingest.ManagedGenerationValidationResult{Outcome: ingest.ValidationNotProven, Mismatch: ingest.UnchangedNotProvenMetadata}
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
	flagRoute := func(reindex bool, configure func(*ingest.PipelineConfig)) func(*testing.T) {
		return func(t *testing.T) {
			source, output, _, _, db, _ := setup(t)
			if reindex {
				conn, err := db.Pool().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.Execute(conn, `UPDATE sessions SET index_version = 0`, nil)
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			}
			counted := &proofCountingRefreshStore{Store: db, contextUnproven: true}
			var parses, builds atomic.Int64
			var transcript []byte
			result := runManagedRefreshPipelineConfigured(t, counted, source, output, &parses, &builds, &transcript, &ingest.OSFileSystem{}, &ingest.IndexProfiler{}, ingest.DefaultAdapterRegistry, configure)
			if result.Summary.Errors != 0 || counted.proofs.Load() != 0 || counted.validationCalls.Load() != 0 || builds.Load() != 1 {
				t.Fatalf("summary %+v proofs %d validations %d builds %d", result.Summary, counted.proofs.Load(), counted.validationCalls.Load(), builds.Load())
			}
		}
	}
	routes := map[string]func(*testing.T){
		"disable-refresh-skips-never-validates": flagRoute(false, func(cfg *ingest.PipelineConfig) { cfg.DisableRefreshSkips = true }),
		"force-never-validates":                 flagRoute(false, func(cfg *ingest.PipelineConfig) { cfg.Force = true }),
		"reindex-never-validates":               flagRoute(true, func(cfg *ingest.PipelineConfig) { cfg.Reindex = true }),
		"proven": func(t *testing.T) {
			source, output, empty, id, db, _ := setup(t)
			before := managedRefreshPipelineRows(t, db, `SELECT c.turn_count || ':' || c.conversation_turn_count || ':' || c.input_submission_count || ':' || s.input_submission_count FROM sessions s JOIN session_captured_stats c USING (session_id) WHERE s.session_id = ?`, id)
			counted, _, profiler, _, parses, builds, _ := run(t, db, source, output, empty, "")
			if counted.proofs.Load() != 1 || !counted.proof.Proven || counted.validationCalls.Load() != 0 || counted.settles.Load() != 1 || parses.Load() != 0 || builds.Load() != 0 {
				t.Fatalf("proof %+v attempts %d settles %d parses %d builds %d", counted.proof, counted.proofs.Load(), counted.settles.Load(), parses.Load(), builds.Load())
			}
			assertProfile(t, profiler, 1)
			assertManagedRefreshRowsEqual(t, "parse-owned counts", before, managedRefreshPipelineRows(t, db, `SELECT c.turn_count || ':' || c.conversation_turn_count || ':' || c.input_submission_count || ':' || s.input_submission_count FROM sessions s JOIN session_captured_stats c USING (session_id) WHERE s.session_id = ?`, id))
			next := runManagedRefreshPipelineInstrumented(t, counted, empty, output, parses, builds, new([]byte), &ingest.OSFileSystem{}, &ingest.IndexProfiler{})
			if next.Summary.Errors != 0 || next.Census == nil || next.Census.Sessions() != 0 || counted.proofs.Load() != 1 || counted.settles.Load() != 1 || parses.Load() != 0 || builds.Load() != 0 {
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
			if counted.proofs.Load() != 1 || counted.proof.Proven || counted.proof.Mismatch != ingest.UnchangedNotProvenIndexer || counted.settles.Load() != 0 || builds.Load() != 1 || !bytes.Equal(*transcript, expected) || fs.reads.Load() != 0 {
				t.Fatalf("proof %+v attempts %d settles %d builds %d stored bytes match %t native reads %d", counted.proof, counted.proofs.Load(), counted.settles.Load(), builds.Load(), bytes.Equal(*transcript, expected), fs.reads.Load())
			}
			assertProfile(t, profiler, 0)
		},
		"metadata-context": func(t *testing.T) {
			source, output, empty, id, db, expected := setup(t)
			stored := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
			if managedRefreshPipelineChangeMetadata(t, output, stored, managedRefreshPipelineForcedProject) != 1 {
				t.Fatal("expected one metadata file")
			}
			previous := managedRefreshPipelineActiveGeneration(t, db, id)
			counted, fs, profiler, result, _, builds, transcript := run(t, db, source, output, empty, "")
			got := observedRefreshRoute(counted, builds, fs)
			// The store's validator is asked and does not prove it either.
			if want := (refreshRoute{Proofs: 1, Validations: 1, Builds: 1}); got != want || counted.proof.Mismatch != ingest.UnchangedNotProvenMetadata {
				t.Fatalf("route %+v proof %+v; want %+v and a metadata mismatch", got, counted.proof, want)
			}
			if !bytes.Equal(*transcript, expected) {
				t.Fatal("the parse did not read the stored transcript bytes")
			}
			assertManagedRefreshParsed(t, db, result, id, previous, managedRefreshPipelineForcedProject, true)
			assertProfile(t, profiler, 0)
		},
		"unproven-unchanged-parses": func(t *testing.T) {
			source, output, empty, id, db, expected := setup(t)
			previous := managedRefreshPipelineActiveGeneration(t, db, id)
			project := managedRefreshPipelineProjectName(t, db, id, previous)
			counted, fs, profiler, result, _, builds, transcript := run(t, db, source, output, empty, "unproven")
			got := observedRefreshRoute(counted, builds, fs)
			if want := (refreshRoute{Proofs: 1, Builds: 1}); got != want || counted.proof.Proven {
				t.Fatalf("route %+v proof %+v; want %+v and no proven proof", got, counted.proof, want)
			}
			if !bytes.Equal(*transcript, expected) {
				t.Fatal("the parse did not read the stored transcript bytes")
			}
			assertManagedRefreshParsed(t, db, result, id, previous, project, false)
			assertProfile(t, profiler, 0)
		},
		"proof-error": func(t *testing.T) {
			source, output, empty, _, db, expected := setup(t)
			counted, fs, profiler, _, _, builds, transcript := run(t, db, source, output, empty, "proof-error")
			if counted.proofs.Load() != 1 || counted.settles.Load() != 0 || builds.Load() != 1 || !bytes.Equal(*transcript, expected) || fs.reads.Load() != 0 {
				t.Fatalf("proofs %d settles %d builds %d bytes match %t native reads %d", counted.proofs.Load(), counted.settles.Load(), builds.Load(), bytes.Equal(*transcript, expected), fs.reads.Load())
			}
			assertProfile(t, profiler, 0)
		},
		"settle-refused": func(t *testing.T) {
			source, output, empty, _, db, expected := setup(t)
			counted, fs, profiler, _, _, builds, transcript := run(t, db, source, output, empty, "settle-refused")
			if counted.proofs.Load() != 1 || !counted.proof.Proven || counted.settles.Load() != 1 || builds.Load() != 1 || !bytes.Equal(*transcript, expected) || fs.reads.Load() != 0 {
				t.Fatalf("proof %+v attempts %d settles %d builds %d bytes match %t native reads %d", counted.proof, counted.proofs.Load(), counted.settles.Load(), builds.Load(), bytes.Equal(*transcript, expected), fs.reads.Load())
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

func TestManagedGenerationOutputPreservingDeclaration(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name       string `yaml:"name"`
			Declared   bool   `yaml:"declared"`
			Diagnostic bool   `yaml:"diagnostic"`
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
			counted := &proofCountingRefreshStore{Store: db}
			var parses, builds atomic.Int64
			var transcript []byte
			result := runManagedRefreshPipelineWithAdapters(t, counted, empty, output, &parses, &builds, &transcript, &ingest.OSFileSystem{}, &ingest.IndexProfiler{}, adapters)
			if result.Summary.Errors != 0 || result.Summary.StoreError != nil || counted.proof.Mismatch != ingest.UnchangedNotProvenMetadata || counted.settles.Load() != 0 || builds.Load() != 1 {
				t.Fatalf("summary %+v proof %+v settles %d builds %d", result.Summary, counted.proof, counted.settles.Load(), builds.Load())
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
			Proof    bool   `yaml:"proof"`
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
			counted := &proofCountingRefreshStore{Store: base}
			var parses atomic.Int64
			result := runManagedRefreshPipelineCounting(t, counted, empty, output, &parses)
			if result.Summary.Errors != 0 || result.Census == nil || len(result.Census.Rows) != 1 {
				t.Fatalf("run summary %+v census %+v", result.Summary, result.Census)
			}
			if got := result.Census.Rows[0].Reason.Code(); got != c.Reason {
				t.Fatalf("reason = %q, want %q", got, c.Reason)
			}
			wantProofs := map[bool]int64{true: 1, false: 0}[c.Proof]
			if counted.proofs.Load() != wantProofs || parses.Load() == 0 || counted.settles.Load() != 0 {
				t.Fatalf("proofs %d, parses %d, settles %d; want %d, positive, 0", counted.proofs.Load(), parses.Load(), counted.settles.Load(), wantProofs)
			}
		})
	}
	for _, name := range manifest.RequiredNames {
		if !names[name] {
			t.Fatalf("missing required lane %q", name)
		}
	}
}

// TestManagedGenerationContextChangeParsesThroughThePipeline is the production
// path for a changed metadata context on an adapter bump. The unchanged proof
// fails on the metadata, so every session parses its stored transcript and
// installs a generation carrying the new context; nothing copies the stored
// generation. A following transcript change takes the ordinary native parse
// again.
func TestManagedGenerationContextChangeParsesThroughThePipeline(t *testing.T) {
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
	for _, sid := range sessionIDs {
		initial[sid] = managedRefreshPipelineActiveGeneration(t, db, sid)
	}
	initialEntries := managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || entry_index || ':' || source_entry_ref || ':' || body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, sessionIDs[0], initial[sessionIDs[0]])
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
	refresher := &proofCountingRefreshStore{Store: db}
	refreshed := runManagedRefreshPipelineCounting(t, refresher, emptySource, output, &parses)
	if refresher.proofs.Load() != int64(len(sessionIDs)) || refresher.settles.Load() != 0 {
		t.Fatalf("proofs %d settles %d, want %d and 0", refresher.proofs.Load(), refresher.settles.Load(), len(sessionIDs))
	}
	if parses.Load() != int64(len(sessionIDs)) {
		t.Fatalf("the failed proofs parsed %d times, want %d", parses.Load(), len(sessionIDs))
	}
	type summaryCounts struct{ Errors, New, Unchanged, Updated, Indexed int }
	summary := refreshed.Summary
	if got, want := (summaryCounts{summary.Errors, summary.New, summary.Unchanged, summary.Updated, summary.Indexed}), (summaryCounts{Updated: len(sessionIDs), Indexed: len(sessionIDs)}); got != want {
		t.Fatalf("context-change summary = %+v, want %+v", got, want)
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
		t.Fatalf("context-change run produced %d index rows, want one per session", len(refreshed.IndexLog))
	}
	for _, entry := range refreshed.IndexLog {
		if entry.ParseDuration == nil || *entry.ParseDuration <= 0 {
			t.Fatalf("session %s was not parsed: %+v", entry.SessionID, entry)
		}
	}
	for _, sid := range sessionIDs {
		active := managedRefreshPipelineActiveGeneration(t, db, sid)
		if active == initial[sid] {
			t.Fatalf("session %s kept the source generation active; want a parsed generation", sid)
		}
		if name := managedRefreshPipelineProjectName(t, db, sid, active); name != managedRefreshPipelineForcedProject {
			t.Fatalf("parsed generation of session %s records project %q, want %q", sid, name, managedRefreshPipelineForcedProject)
		}
	}
	// The parse reads the same stored bytes, so the entries are reproduced.
	refreshedAuthority := managedRefreshPipelineActiveGeneration(t, db, sessionIDs[0])
	assertManagedRefreshRowsEqual(t, "entries", initialEntries, managedRefreshPipelineRows(t, db, `SELECT partition_id || ':' || entry_index || ':' || source_entry_ref || ':' || body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, sessionIDs[0], refreshedAuthority))

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
	if refresher.settles.Load() != 0 {
		t.Fatalf("changed-source parse attempted %d unchanged settlements, want zero", refresher.settles.Load())
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

// refreshRoute is the work one refresh run did on the index stage: proofs
// attempted, settles, validator calls, generation builds (this format's parse),
// and reads of the native source.
type refreshRoute struct {
	Proofs, Settles, Validations, Builds, NativeReads int64
}

func observedRefreshRoute(counted *proofCountingRefreshStore, builds *atomic.Int64, fs *nativeCountingRefreshFS) refreshRoute {
	return refreshRoute{
		Proofs: counted.proofs.Load(), Settles: counted.settles.Load(), Validations: counted.validationCalls.Load(),
		Builds: builds.Load(), NativeReads: fs.reads.Load(),
	}
}

// assertManagedRefreshParsed checks that the one index row of a refresh run
// recorded a parse and that the active generation carries the wanted project
// context afterwards. replaced says whether the parse must install a new
// generation: a parse that reproduces the stored projection keeps it.
func assertManagedRefreshParsed(t *testing.T, db *store.Store, result *ingest.PipelineResult, id ingest.SessionID, previous, project string, replaced bool) {
	t.Helper()
	logged := len(result.IndexLog) == 1 && result.IndexLog[0].ParseDuration != nil
	if result.Summary.Indexed != 1 || !logged || *result.IndexLog[0].ParseDuration <= 0 {
		t.Fatalf("the failed proof did not parse: summary %+v index log %+v", result.Summary, result.IndexLog)
	}
	active := managedRefreshPipelineActiveGeneration(t, db, id)
	if (active != previous) != replaced {
		t.Fatalf("active generation %q after the parse, previous %q; want replaced=%t", active, previous, replaced)
	}
	if name := managedRefreshPipelineProjectName(t, db, id, active); name != project {
		t.Fatalf("parsed generation %q records project %q, want %q", active, name, project)
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

// The validator seam tests need a candidate, not a second copy of the Claude
// metadata algorithm. The store double decides the validation outcome.
func (i countingManagedRefreshIndexer) BuildGenerationMetadata(_ context.Context, input ingest.GenerationBuildInput) (ingest.GenerationMetadataCandidate, error) {
	return ingest.GenerationMetadataCandidate{Metadata: *input.RecordedMetadata}, nil
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
	return runManagedRefreshPipelineConfigured(t, db, source, output, calls, builds, transcript, fs, profiler, adapters, nil)
}

// runManagedRefreshPipelineConfigured lets a test change the pipeline config
// after the shared defaults are set and before the pipeline is built.
func runManagedRefreshPipelineConfigured(t *testing.T, db managedRefreshPipelineStore, source, output string, calls, builds *atomic.Int64, transcript *[]byte, fs ingest.FileSystem, profiler *ingest.IndexProfiler, adapters map[ingest.Harness]ingest.AdapterFactory, configure func(*ingest.PipelineConfig)) *ingest.PipelineResult {
	t.Helper()
	cfg := makePipelineConfig(output)
	if validator, ok := db.(ingest.ManagedGenerationValidator); ok {
		cfg.Validator = validator
	}
	if counted, ok := db.(*proofCountingRefreshStore); ok && counted.validationMissing {
		cfg.Validator = nil
	}
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}, Enabled: true},
	}
	cfg.StalenessThreshold = 0
	cfg.IndexProfiler = profiler
	if configure != nil {
		configure(&cfg)
	}
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
