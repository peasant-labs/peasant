package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/managed_reference_pipeline_routes.yaml
var managedReferenceRoutesYAML []byte

//go:embed testdata/managed_reference_pipeline_routes.manifest.yaml
var managedReferenceRoutesManifestYAML []byte

//go:embed testdata/claude_erroring_branches.manifest.yaml
var claudeErroringBranchesForRoutesYAML []byte

// referenceRouteProject is the project name a route's rename writes.
const referenceRouteProject = "reference-route-renamed-project"

// referenceRouteStore observes the real store's reference step under a
// refresh run. It records every plan and install, and offers seams: a plan
// or install error, a change between the plan and the install, and a forced
// cheap-proof mismatch.
type referenceRouteStore struct {
	*halfValidatingStore
	planErr       error
	installErr    error
	beforeInstall func(ingest.SessionID)
	forceMismatch ingest.UnchangedProofMismatch

	mu       sync.Mutex
	plans    map[ingest.SessionID]error
	installs map[ingest.SessionID]error
}

var _ ingest.ManagedGenerationReferencer = (*referenceRouteStore)(nil)

func newReferenceRouteStore(db *store.Store) *referenceRouteStore {
	return &referenceRouteStore{halfValidatingStore: newHalfValidatingStore(db), plans: map[ingest.SessionID]error{}, installs: map[ingest.SessionID]error{}}
}

func (s *referenceRouteStore) ProveCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.UnchangedProof, error) {
	if s.forceMismatch != ingest.UnchangedMismatchNone {
		return ingest.UnchangedProof{Mismatch: s.forceMismatch}, nil
	}
	return s.halfValidatingStore.ProveCapturedGenerationUnchanged(ctx, request)
}

func (s *referenceRouteStore) PlanManagedGenerationReference(ctx context.Context, request ingest.ManagedGenerationReferenceRequest) (ingest.ManagedGenerationReferencePlan, error) {
	var plan ingest.ManagedGenerationReferencePlan
	err := s.planErr
	if err == nil {
		plan, err = s.Store.PlanManagedGenerationReference(ctx, request)
	}
	s.mu.Lock()
	s.plans[request.Proof.SessionID] = err
	s.mu.Unlock()
	return plan, err
}

func (s *referenceRouteStore) InstallManagedGenerationReference(ctx context.Context, plan ingest.ManagedGenerationReferencePlan) (ingest.ManagedGenerationReferenceOutcome, error) {
	if s.beforeInstall != nil {
		s.beforeInstall(plan.SessionID())
	}
	var outcome ingest.ManagedGenerationReferenceOutcome
	err := s.installErr
	if err == nil {
		outcome, err = s.Store.InstallManagedGenerationReference(ctx, plan)
	}
	s.mu.Lock()
	s.installs[plan.SessionID()] = err
	s.mu.Unlock()
	return outcome, err
}

// counts reports the plans and installs that succeeded and that were refused
// or failed.
func (s *referenceRouteStore) counts() (planned, planFailed, installed, installFailed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, err := range s.plans {
		if err == nil {
			planned++
		} else {
			planFailed++
		}
	}
	for _, err := range s.installs {
		if err == nil {
			installed++
		} else {
			installFailed++
		}
	}
	return
}

// referenceHiddenStore is the real store with the reference capability
// hidden: its Plan method shadows the promoted one with another signature,
// so the store does not satisfy ManagedGenerationReferencer.
type referenceHiddenStore struct {
	*halfValidatingStore
}

func (referenceHiddenStore) PlanManagedGenerationReference() {}

// refusalOf returns the reason a reference step refused with.
func refusalOf(err error) ingest.ReferenceRefusalReason {
	var refusal ingest.ReferenceRefusal
	if errors.As(err, &refusal) {
		return refusal.RefusalReason()
	}
	return ""
}

// renamedRoute seeds the Claude corpus, renames its project in every
// managed metadata sidecar and snapshots the store.
func renamedRoute(t *testing.T) *realRouteSeed {
	t.Helper()
	seed := seedRealRoute(t, ingest.HarnessClaudeCode, func(t *testing.T, source string) { writeManagedRefreshPipelineCorpus(t, source, 1, 8) })
	if renamed := rewriteManagedMetadata(t, seed.output, func(_ string, metadata *schema.UnifiedMetadata) {
		metadata.Project.Name = referenceRouteProject
	}); renamed == 0 {
		t.Fatal("no managed metadata sidecar was renamed")
	}
	seed.snapshot(t)
	return seed
}

// forgeActiveSealOf forges the stored seal of a session's active generation,
// with the immutability trigger lifted for the one update.
func forgeActiveSealOf(t *testing.T, db *store.Store, sid ingest.SessionID) {
	t.Helper()
	conn, err := db.Pool().Take(context.Background())
	if err != nil {
		t.Error(err)
		return
	}
	defer db.Pool().Put(conn)
	var trigger string
	if err := sqlitex.Execute(conn, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='session_generations_immutable'`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		trigger = st.ColumnText(0)
		return nil
	}}); err != nil || trigger == "" {
		t.Errorf("read the immutability trigger: %q, %v", trigger, err)
		return
	}
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`DROP TRIGGER session_generations_immutable`, nil},
		{`UPDATE session_generations SET candidate_digest = ? WHERE session_id = ? AND generation_id = (SELECT active_generation_id FROM sessions WHERE session_id = ?)`, []any{strings.Repeat("f", 64), string(sid), string(sid)}},
		{trigger, nil},
	} {
		if err := sqlitex.ExecuteTransient(conn, step.query, &sqlitex.ExecOptions{Args: step.args}); err != nil {
			t.Errorf("forge the active seal: %s: %v", step.query, err)
			return
		}
	}
}

// assertNoReference requires the run to have planned no reference.
func assertNoReference(t *testing.T, profiler *ingest.IndexProfiler) {
	t.Helper()
	if total, _ := routeStage(profiler, ingest.StageRefreshReference); total != 0 {
		t.Fatalf("refresh_reference total %d, want no reference attempt", total)
	}
}

func managedReferenceRoutes() map[string]func(*testing.T) {
	// parsesOnce runs a renamed route with the store and run given, then
	// requires one parse, no install, and the plain parse's detail bytes.
	parsesOnce := func(t *testing.T, seed *realRouteSeed, db managedRefreshPipelineStore, run realRouteRun, prepare func(*store.Store)) *ingest.PipelineResult {
		t.Helper()
		result := runRealRoute(t, seed, db, seed.empty, run)
		if result.Summary.Errors != 0 || routeParses(result) != 1 {
			t.Fatalf("summary %+v, parses %d; want one parse", result.Summary, routeParses(result))
		}
		assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), prepare)
		return result
	}
	return map[string]func(*testing.T){
		"context-change-references": func(t *testing.T) {
			seed := renamedRoute(t)
			sid := routeSessions(t, seed.db)[0]
			before := managedRefreshPipelineActiveGeneration(t, seed.db, sid)
			observed := newReferenceRouteStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, observed, seed.empty, realRouteRun{profiler: profiler})
			if outcome, asked := observed.outcome(sid); !asked || outcome.Outcome != ingest.ValidationNotProven || !outcome.Mismatch.IsContext() {
				t.Fatalf("validation %+v (asked %t), want not proven on a context term", outcome, asked)
			}
			if planned, _, installed, _ := observed.counts(); planned != 1 || installed != 1 {
				t.Fatalf("plans %d installs %d, want one each", planned, installed)
			}
			oneSkip := len(result.IndexLog) == 1 && result.IndexLog[0].Outcome == ingest.IndexOutcomeSkipped
			referenceReason := oneSkip && result.IndexLog[0].Reason != nil && *result.IndexLog[0].Reason == ingest.IndexManagedReferenceReasonForTest
			if parses := routeParses(result); parses != 0 || !referenceReason {
				t.Fatalf("parses %d, index log %+v; want no parse and one skipped row with the reference reason", parses, result.IndexLog)
			}
			if total, done := routeStage(profiler, ingest.StageRefreshReference); total != 1 || done != 1 {
				t.Fatalf("refresh_reference total %d done %d, want 1 and 1", total, done)
			}
			if after := managedRefreshPipelineActiveGeneration(t, seed.db, sid); after == before {
				t.Fatal("the reference did not move the active generation")
			}
			assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), nil)
		},
		"no-candidate-parses": func(t *testing.T) {
			seed := seedRealRouteAt(t, ingest.HarnessCodex, "sessions", func(t *testing.T, source string) {
				if out, err := exec.Command("cp", "-a", codexRouteFixture+"/.", source).CombinedOutput(); err != nil {
					t.Fatalf("copy the Codex fixture: %v: %s", err, out)
				}
			})
			rewriteManagedMetadata(t, seed.output, func(_ string, metadata *schema.UnifiedMetadata) { metadata.Project.Name = referenceRouteProject })
			seed.snapshot(t)
			sessions := len(routeSessions(t, seed.db))
			observed := newReferenceRouteStore(seed.db)
			observed.forceContext = true
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, observed, seed.empty, realRouteRun{profiler: profiler})
			if total, _ := routeStage(profiler, ingest.StageRefreshValidateUnsupported); total != sessions {
				t.Fatalf("refresh_validate_unsupported total %d, want %d", total, sessions)
			}
			assertNoReference(t, profiler)
			if result.Summary.Errors != 0 || routeParses(result) != sessions {
				t.Fatalf("summary %+v, parses %d; want %d parses", result.Summary, routeParses(result), sessions)
			}
			assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), nil)
		},
		"source-moved-parses": func(t *testing.T) {
			seed := renamedRoute(t)
			observed := newReferenceRouteStore(seed.db)
			observed.beforeInstall = func(sid ingest.SessionID) { forgeActiveSealOf(t, seed.db, sid) }
			parsesOnce(t, seed, observed, realRouteRun{}, nil)
			for _, err := range observed.installs {
				if refusalOf(err) != ingest.ReferenceRefusalSourceMoved {
					t.Fatalf("install returned %v, want a source-moved refusal", err)
				}
			}
			if _, _, installed, failed := observed.counts(); installed != 0 || failed != 1 {
				t.Fatalf("installs %d refused %d, want one refused", installed, failed)
			}
		},
		"owner-seal-mismatch-parses": func(t *testing.T) {
			seed := renamedRoute(t)
			sid := routeSessions(t, seed.db)[0]
			forgeActiveSealOf(t, seed.db, sid)
			observed := newReferenceRouteStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			parsesOnce(t, seed, observed, realRouteRun{profiler: profiler}, func(db *store.Store) { forgeActiveSealOf(t, db, sid) })
			if refusalOf(observed.plans[sid]) != ingest.ReferenceRefusalOwnerSeal {
				t.Fatalf("plan returned %v, want an owner-seal-mismatch refusal", observed.plans[sid])
			}
			if total, done := routeStage(profiler, ingest.StageRefreshReference); total != 1 || done != 0 {
				t.Fatalf("refresh_reference total %d done %d, want 1 and 0", total, done)
			}
			if total, _ := routeStage(profiler, ingest.StageRefreshReferenceRefused(ingest.ReferenceRefusalOwnerSeal)); total != 1 {
				t.Fatalf("the owner-seal-mismatch refusal stage counted %d, want 1", total)
			}
		},
		"parse-input-context-parses": func(t *testing.T) {
			seed := renamedRoute(t)
			sid := routeSessions(t, seed.db)[0]
			observed := newReferenceRouteStore(seed.db)
			parsesOnce(t, seed, observed, realRouteRun{options: []ingest.PipelineOption{
				ingest.WithParseInputContextFieldsForTest(func(ingest.Harness) []string { return []string{"project.name"} }),
			}}, nil)
			if refusalOf(observed.plans[sid]) != ingest.ReferenceRefusalParseInputContext {
				t.Fatalf("plan returned %v, want a parse-input-context-changed refusal", observed.plans[sid])
			}
		},
		"plan-error-parses": func(t *testing.T) {
			seed := renamedRoute(t)
			observed := newReferenceRouteStore(seed.db)
			observed.planErr = errors.New("synthetic read failure: disk I/O error")
			profiler := &ingest.IndexProfiler{}
			parsesOnce(t, seed, observed, realRouteRun{profiler: profiler}, nil)
			if total, _ := routeStage(profiler, ingest.StageRefreshReferenceError); total != 1 {
				t.Fatalf("refresh_reference_error total %d, want 1", total)
			}
		},
		"install-error-parses": func(t *testing.T) {
			seed := renamedRoute(t)
			sid := routeSessions(t, seed.db)[0]
			before := managedRefreshPipelineActiveGeneration(t, seed.db, sid)
			observed := newReferenceRouteStore(seed.db)
			observed.installErr = errors.New("synthetic write failure: disk I/O error")
			activeAtInstall := ""
			observed.beforeInstall = func(got ingest.SessionID) { activeAtInstall = managedRefreshPipelineActiveGeneration(t, seed.db, got) }
			parsesOnce(t, seed, observed, realRouteRun{}, nil)
			if activeAtInstall != before {
				t.Fatalf("the active generation at the failed install was %s, want the old %s", activeAtInstall, before)
			}
		},
		"force-skips-the-step": func(t *testing.T) {
			seed := renamedRoute(t)
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, newReferenceRouteStore(seed.db), seed.source, realRouteRun{force: true, profiler: profiler})
			assertNoReference(t, profiler)
			if result.Summary.Errors != 0 || routeParses(result) == 0 {
				t.Fatalf("summary %+v, parses %d; want the forced run to parse", result.Summary, routeParses(result))
			}
			assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), nil)
		},
		"reindex-skips-the-step": func(t *testing.T) {
			seed := renamedRoute(t)
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, newReferenceRouteStore(seed.db), seed.source, realRouteRun{reindex: true, profiler: profiler})
			assertNoReference(t, profiler)
			if result.Summary.Errors != 0 || routeParses(result) != 0 {
				t.Fatalf("summary %+v, parses %d; want the reindex run to finish without errors and without parsing an already-current session", result.Summary, routeParses(result))
			}
			assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), nil)
		},
		"disable-refresh-skips-skips-the-step": func(t *testing.T) {
			seed := renamedRoute(t)
			profiler := &ingest.IndexProfiler{}
			parsesOnce(t, seed, newReferenceRouteStore(seed.db), realRouteRun{disableSkips: true, profiler: profiler}, nil)
			assertNoReference(t, profiler)
		},
		"non-context-mismatch-skips-the-step": func(t *testing.T) {
			seed := renamedRoute(t)
			observed := newReferenceRouteStore(seed.db)
			observed.forceMismatch = ingest.UnchangedNotProvenIndexer
			profiler := &ingest.IndexProfiler{}
			parsesOnce(t, seed, observed, realRouteRun{profiler: profiler}, nil)
			if len(observed.outcomes) != 0 {
				t.Fatalf("the validator answered %v after a non-context mismatch", observed.outcomes)
			}
			assertNoReference(t, profiler)
		},
		"store-without-referencer": func(t *testing.T) {
			seed := renamedRoute(t)
			sid := routeSessions(t, seed.db)[0]
			hidden := referenceHiddenStore{halfValidatingStore: newHalfValidatingStore(seed.db)}
			profiler := &ingest.IndexProfiler{}
			parsesOnce(t, seed, hidden, realRouteRun{profiler: profiler}, nil)
			if outcome, asked := hidden.outcome(sid); !asked || outcome.Outcome != ingest.ValidationNotProven {
				t.Fatalf("validation %+v (asked %t), want not proven", outcome, asked)
			}
			assertNoReference(t, profiler)
		},
		"validated-settle-wins-over-reference": func(t *testing.T) {
			seed := seedRealRoute(t, ingest.HarnessClaudeCode, func(t *testing.T, source string) { writeManagedRefreshPipelineCorpus(t, source, 1, 8) })
			forgeStoredMetadataHash(t, seed.db)
			seed.snapshot(t)
			observed := newReferenceRouteStore(seed.db)
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, observed, seed.empty, realRouteRun{profiler: profiler})
			if len(observed.settled) != 1 || routeParses(result) != 0 {
				t.Fatalf("settles %v, parses %d; want one settle and no parse", observed.settled, routeParses(result))
			}
			assertNoReference(t, profiler)
			assertMatchesPlainParse(t, seed, detailOfEvery(t, seed), nil)
		},
		"claude-without-candidate-parses": func(t *testing.T) {
			document := loadOracleDocument(t)
			const parent, fork = "-workspace/22222222-2222-4222-8222-222222222222.jsonl", ingest.SessionID("99d59925-36bc-424c-a789-8be54d9702ba")
			seed := seedRealRoute(t, ingest.HarnessClaudeCode, func(t *testing.T, source string) {
				for _, file := range document.Corpus {
					if file.Path == parent || strings.Contains(file.Path, string(fork)) {
						writeOracleFile(t, source, document, file.Path)
					}
				}
			})
			rewriteManagedMetadata(t, seed.output, func(_ string, metadata *schema.UnifiedMetadata) { metadata.Project.Name = referenceRouteProject }, string(fork))
			seed.snapshot(t)
			before := managedRefreshPipelineActiveGeneration(t, seed.db, fork)
			beforeDetail := sessionDetailBytes(t, seed, fork)
			observed := newReferenceRouteStore(seed.db)
			halves := newHalfRecorder()
			prove := &oracleProveSeam{halfValidatingStore: observed.halfValidatingStore, before: func() { removeOracleFiles(t, seed.source, []string{parent}) }}
			profiler := &ingest.IndexProfiler{}
			result := runRealRoute(t, seed, prove, seed.empty, realRouteRun{profiler: profiler, halves: halves})
			if halves.refusal(fork) == nil {
				t.Fatal("the fork's metadata half did not refuse")
			}
			if total, _ := routeStage(profiler, ingest.StageRefreshValidateUnsupported); total != 1 {
				t.Fatalf("refresh_validate_unsupported total %d, want 1", total)
			}
			assertNoReference(t, profiler)
			if parses := routeParses(result); parses != 1 {
				t.Fatalf("parses %d, want the fork to parse once", parses)
			}
			refused := false
			for _, entry := range result.IndexLog {
				if entry.SessionID == fork && entry.Outcome != ingest.IndexOutcomeSkipped && entry.Outcome != ingest.IndexOutcomeIndexed {
					refused = true
				}
			}
			for _, session := range result.Sessions {
				if session.SessionID == fork && session.Error != nil {
					refused = true
				}
			}
			if !refused {
				t.Fatalf("the fork's parse recorded no refusal: index log %+v", result.IndexLog)
			}
			if after := managedRefreshPipelineActiveGeneration(t, seed.db, fork); after != before {
				t.Fatalf("the refused parse moved the active generation %s -> %s", before, after)
			}
			if afterDetail := sessionDetailBytes(t, seed, fork); !bytes.Equal(afterDetail, beforeDetail) {
				t.Fatalf("the refused parse changed session_detail bytes:\nbefore %s\nafter  %s", beforeDetail, afterDetail)
			}
		},
	}
}

func TestManagedReferencePipelineRoutes(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name  string `yaml:"name"`
			Given string `yaml:"given"`
			Then  string `yaml:"then"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
		NotApplicable []struct {
			Name   string `yaml:"name"`
			Reason string `yaml:"reason"`
		} `yaml:"notApplicable"`
	}
	if err := yaml.Unmarshal(managedReferenceRoutesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(managedReferenceRoutesManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	var erroring struct {
		ErroringBranches []any `yaml:"erroringBranches"`
	}
	if err := yaml.Unmarshal(claudeErroringBranchesForRoutesYAML, &erroring); err != nil {
		t.Fatal(err)
	}
	if len(erroring.ErroringBranches) != 0 {
		t.Fatal("claude_erroring_branches.manifest.yaml now lists an erroring branch; add a route that parses it without a reference and drop the not-applicable row")
	}
	routes := managedReferenceRoutes()
	var names []string
	for _, c := range fixture.Cases {
		if c.Given == "" || c.Then == "" || slices.Contains(names, c.Name) {
			t.Fatalf("route %q is duplicated or misses its given or then", c.Name)
		}
		names = append(names, c.Name)
	}
	if got, want := slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(manifest.RequiredNames)); !slices.Equal(got, want) {
		t.Fatalf("routes %v differ from the required names %v", got, want)
	}
	if got := slices.Sorted(maps.Keys(routes)); !slices.Equal(got, slices.Sorted(slices.Values(names))) {
		t.Fatalf("route functions %v differ from the fixture %v", got, names)
	}
	for _, na := range manifest.NotApplicable {
		if len(strings.Fields(na.Reason)) < 3 {
			t.Fatalf("not-applicable route %q needs a reason", na.Name)
		}
	}
	for _, name := range names {
		t.Run(name, routes[name])
	}
}
