package upgrade

import (
	_ "embed"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

//go:embed testdata/refresh_undeclared.yaml
var refreshUndeclaredCases []byte

//go:embed testdata/refresh_undeclared.manifest.yaml
var refreshUndeclaredManifest []byte

type refreshUndeclaredCase struct {
	Name          string `yaml:"name"`
	Harness       string `yaml:"harness"`
	Undeclare     string `yaml:"undeclare"`
	Unregister    bool   `yaml:"unregister"`
	AdapterOffset int    `yaml:"adapterOffset"`
	IndexerOffset int    `yaml:"indexerOffset"`
	MetadataOld   bool   `yaml:"metadataOld"`
	Want          struct {
		Decided        bool   `yaml:"decided"`
		Lane           string `yaml:"lane"`
		Reason         string `yaml:"reason"`
		FullStep       string `yaml:"fullStep"`
		Skipped        string `yaml:"skipped"`
		UnknownHarness bool   `yaml:"unknownHarness"`
	} `yaml:"want"`
}

// undeclaredTable returns this build's declaration table with the named
// producer kind of one harness stripped of its change scope. The producer
// stays registered, which is what separates it from an unknown harness.
// unregister instead drops every producer of the harness, so nothing in the
// table can produce its targets.
func undeclaredTable(t *testing.T, harness ingest.Harness, raw string, unregister bool) []ProducerDeclaration {
	t.Helper()
	declarations, err := ProducerDeclarations()
	if err != nil {
		t.Fatal(err)
	}
	if unregister {
		return slices.DeleteFunc(declarations, func(d ProducerDeclaration) bool { return d.Kind.PerHarness() && d.Name == string(harness) })
	}
	if raw == "" {
		return declarations
	}
	kind, err := NewProducerKind(raw)
	if err != nil {
		t.Fatal(err)
	}
	stripped := 0
	for i := range declarations {
		if declarations[i].Kind == kind && declarations[i].Name == string(harness) {
			declarations[i].ChangeScope = ""
			stripped++
		}
	}
	if stripped != 1 {
		t.Fatalf("stripped %d declarations of %s/%s, want 1", stripped, kind, harness)
	}
	return declarations
}

// TestPlanRefreshUndeclaredProducer keeps an undeclared producer and an
// unknown harness apart: the first plans the full step and names the
// producer, the second stays undecided and names no producer.
func TestPlanRefreshUndeclaredProducer(t *testing.T) {
	var fixture struct {
		Cases []refreshUndeclaredCase `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(refreshUndeclaredCases, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := testutil.DecodeFixtureYAML(refreshUndeclaredManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	target := ingest.GenerationRepairTargets[ingest.HarnessClaudeCode]
	for _, c := range fixture.Cases {
		present[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "current.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
			if err != nil {
				t.Fatal(err)
			}
			metadata := int(ingest.CurrentSchemaVersion)
			if c.MetadataOld {
				metadata = 1
			}
			err = sqlitex.ExecuteTransient(conn, `INSERT INTO sessions(session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format,schema_version,adapter_version,index_version,indexed_input_hash) VALUES ('session', ?, 'model', 'host', 'project', 1, 2, 3, 'source', 'jsonl', ?, ?, ?, ?)`,
				&sqlitex.ExecOptions{Args: []any{c.Harness, metadata, target.AdapterVersion + c.AdapterOffset, target.IndexerVersion + c.IndexerOffset, "0000000000000000000000000000000000000000000000000000000000000000"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanRefreshFor(t.Context(), path, undeclaredTable(t, ingest.HarnessClaudeCode, c.Undeclare, c.Unregister))
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Sessions) != 1 || plan.Estimate.Sessions != 1 {
				t.Fatalf("sessions = %+v", plan.Sessions)
			}
			session := plan.Sessions[0]
			unknown := slices.Contains(plan.Unknown, RefreshUnknown{"session", "harness"})
			if unknown != c.Want.UnknownHarness || session.Decided != c.Want.Decided {
				t.Fatalf("decided %v unknown harness %v, want %v and %v: %+v", session.Decided, unknown, c.Want.Decided, c.Want.UnknownHarness, plan)
			}
			if !c.Want.Decided {
				// An unknown harness is in no lane and forces no step.
				if session.FullStep != nil || plan.Estimate.Undecided != 1 || len(plan.Estimate.ByLane) != 0 || len(plan.Estimate.FullStep) != 0 {
					t.Fatalf("unknown harness was planned: %+v", plan)
				}
				return
			}
			lane, err := ingest.NewRefreshLane(c.Want.Lane)
			if err != nil {
				t.Fatal(err)
			}
			reason, err := ingest.NewRefreshReason(c.Want.Reason)
			if err != nil {
				t.Fatal(err)
			}
			if session.PlannedLane() != lane || session.Decision.Reason != reason || plan.Estimate.ByLane[lane] != 1 || plan.Estimate.Undecided != 0 {
				t.Fatalf("planned %s/%s, want %s/%s: %+v", session.PlannedLane(), session.Decision.Reason, lane, reason, plan.Estimate)
			}
			if c.Want.FullStep == "" {
				if session.FullStep != nil || len(plan.Estimate.FullStep) != 0 {
					t.Fatalf("named an undeclared producer: %+v", session.FullStep)
				}
				return
			}
			skipped, err := ingest.NewRefreshLane(c.Want.Skipped)
			if err != nil {
				t.Fatal(err)
			}
			if session.FullStep == nil || session.FullStep.String() != c.Want.FullStep || session.FullStep.Skipped != skipped || session.Decision.Lane != skipped {
				t.Fatalf("full step = %+v, want %s skipping %s", session.FullStep, c.Want.FullStep, skipped)
			}
			if plan.Estimate.FullStep[*session.FullStep] != 1 {
				t.Fatalf("full-step census = %+v", plan.Estimate.FullStep)
			}
		})
	}
	if err := testutil.RequireFixtureNames("refresh_undeclared.yaml", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
}

// TestPlanRefreshDeclaresEveryRegisteredStoredLane proves this build's own
// table names no undeclared producer: every registered adapter and indexer
// declares the scope its stored lane needs.
func TestPlanRefreshDeclaresEveryRegisteredStoredLane(t *testing.T) {
	declarations, err := ProducerDeclarations()
	if err != nil {
		t.Fatal(err)
	}
	for _, lane := range ingest.AllRefreshLanes {
		need, stored := storedLaneScopeFor(lane)
		if !stored {
			continue
		}
		for harness := range ingest.HarvesterVersionRegistry {
			found := false
			for _, d := range declarations {
				if d.Kind == need.kind && d.Name == string(harness) {
					found = d.ChangeScope == need.scope
				}
			}
			if !found {
				t.Errorf("%s/%s does not declare %s for the %s lane", need.kind, harness, need.scope, lane)
			}
		}
	}
}
