package upgrade

import (
	_ "embed"
	"maps"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/refresh_undeclared.yaml
var refreshUndeclaredCases []byte

//go:embed testdata/refresh_undeclared.manifest.yaml
var refreshUndeclaredManifest []byte

type refreshUndeclaredFixture struct {
	StoredLaneScopes []struct {
		Lane  string `yaml:"lane"`
		Kind  string `yaml:"kind"`
		Scope string `yaml:"scope"`
	} `yaml:"storedLaneScopes"`
	Cases []struct {
		Name          string `yaml:"name"`
		Undeclare     bool   `yaml:"undeclare"`
		Unregister    bool   `yaml:"unregister"`
		AdapterOffset int    `yaml:"adapterOffset"`
		StampAbsent   bool   `yaml:"stampAbsent"`
		IndexerOffset int    `yaml:"indexerOffset"`
		MetadataOld   bool   `yaml:"metadataOld"`
		Want          struct {
			Decided        bool   `yaml:"decided"`
			Lane           string `yaml:"lane"`
			Reason         string `yaml:"reason"`
			FullStep       string `yaml:"fullStep"`
			UnknownHarness bool   `yaml:"unknownHarness"`
		} `yaml:"want"`
	} `yaml:"cases"`
}

func loadRefreshUndeclared(t *testing.T) refreshUndeclaredFixture {
	t.Helper()
	var fixture refreshUndeclaredFixture
	if err := testutil.DecodeFixtureYAML(refreshUndeclaredCases, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// withholdingClaudeAdapter is the claude-code adapter registered as one that
// withholds its change scope: an undeclared producer.
type withholdingClaudeAdapter struct{ *ingest.ClaudeAdapter }

func (withholdingClaudeAdapter) WithholdsChangeScope() bool { return true }

var _ ingest.ChangeScopeWithholder = withholdingClaudeAdapter{}

// refreshTable builds the declaration table the way the shipped one is
// built, from registries; undeclare swaps in the withholding adapter, and
// unregister drops every claude-code producer from the result.
func refreshTable(t *testing.T, undeclare, unregister bool) []ProducerDeclaration {
	t.Helper()
	registries := currentProducerRegistries()
	if undeclare {
		registries.adapters = maps.Clone(registries.adapters)
		registries.adapters[ingest.HarnessClaudeCode] = func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return withholdingClaudeAdapter{ingest.NewClaudeAdapter(fs, git, s)}
		}
	}
	declarations, err := declareProducers(registries)
	if err != nil {
		t.Fatal(err)
	}
	if unregister {
		declarations = slices.DeleteFunc(declarations, func(d ProducerDeclaration) bool {
			return d.Kind.PerHarness() && d.Name == string(ingest.HarnessClaudeCode)
		})
	}
	return declarations
}

// TestPlanRefreshUndeclaredProducer keeps an undeclared producer and an
// unknown harness apart: the first takes the full step and names the
// producer, the second stays undecided and names no producer.
func TestPlanRefreshUndeclaredProducer(t *testing.T) {
	fixture := loadRefreshUndeclared(t)
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(refreshUndeclaredManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		present[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			seed := refreshSeed{ID: "session", Harness: ingest.HarnessClaudeCode, AdapterOffset: offset(c.AdapterOffset), IndexerOffset: c.IndexerOffset, MetadataOld: c.MetadataOld, Proof: true}
			if c.StampAbsent {
				seed.AdapterOffset = nil
			}
			path := currentRefreshStore(t, seed)
			plan, err := planRefresh(t.Context(), path, refreshTable(t, c.Undeclare, c.Unregister))
			if err != nil {
				t.Fatal(err)
			}
			if plan.Estimate.Sessions != 1 {
				t.Fatalf("sessions = %d", plan.Estimate.Sessions)
			}
			unknown := slices.Contains(plan.Unknown, RefreshUnknown{"session", "harness"})
			if unknown != c.Want.UnknownHarness || (plan.Estimate.Undecided == 0) != c.Want.Decided {
				t.Fatalf("unknown harness %v, want %v: %+v", unknown, c.Want.UnknownHarness, plan)
			}
			if !c.Want.Decided {
				// An unknown harness is in no lane and forces no step.
				planned := plan.Estimate.Undecided != 1 ||
					len(plan.Estimate.ByLane) != 0 ||
					len(plan.Estimate.FullStep) != 0
				if planned {
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
			mismatch := plan.Estimate.ByLane[lane] != 1 ||
				plan.Estimate.ByReason[reason] != 1 ||
				plan.Estimate.Undecided != 0
			if mismatch {
				t.Fatalf("want %s/%s: %+v", lane, reason, plan.Estimate)
			}
			if c.Want.FullStep == "" {
				if len(plan.Estimate.FullStep) != 0 {
					t.Fatalf("named an undeclared producer: %+v", plan.Estimate.FullStep)
				}
				return
			}
			found := false
			for producer, count := range plan.Estimate.FullStep {
				if producer.String() == c.Want.FullStep && count == 1 {
					found = true
				}
			}
			if !found || len(plan.Estimate.FullStep) != 1 {
				t.Fatalf("full step census %+v, want %s", plan.Estimate.FullStep, c.Want.FullStep)
			}
		})
	}
	if err := testutil.RequireFixtureNames("refresh_undeclared.yaml", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
}

// TestShippedProducersDeclareTheirStoredLaneScopes proves this build names no
// undeclared producer: every registered producer of a stored lane's kind
// declares the scope the fixture pins for that lane, exactly once.
func TestShippedProducersDeclareTheirStoredLaneScopes(t *testing.T) {
	fixture := loadRefreshUndeclared(t)
	declarations, err := ProducerDeclarations()
	if err != nil {
		t.Fatal(err)
	}
	lanes := make([]ingest.RefreshLane, 0, len(fixture.StoredLaneScopes))
	for _, pin := range fixture.StoredLaneScopes {
		lane, err := ingest.NewRefreshLane(pin.Lane)
		if err != nil {
			t.Fatal(err)
		}
		lanes = append(lanes, lane)
		kind, err := NewProducerKind(pin.Kind)
		if err != nil {
			t.Fatal(err)
		}
		scope, err := NewChangeScope(pin.Scope)
		if err != nil {
			t.Fatal(err)
		}
		for harness, factory := range ingest.DefaultAdapterRegistry {
			if !ingest.AdapterDeclaresChangeScope(factory(nil, nil, salt.Salt{})) {
				t.Errorf("shipped adapter %s withholds its change scope", harness)
			}
			declared := 0
			for _, d := range declarations {
				if d.Kind == kind && d.Name == string(harness) && d.ChangeScope == scope {
					declared++
				}
			}
			if declared != 1 {
				t.Errorf("%s/%s declares %s for the %s lane %d times, want once", kind, harness, scope, lane, declared)
			}
		}
	}
	for _, lane := range []ingest.RefreshLane{ingest.RefreshLaneStoredMetadata, ingest.RefreshLaneStoredIndex} {
		if !slices.Contains(lanes, lane) {
			t.Errorf("storedLaneScopes pins no scope for the %s lane", lane)
		}
	}
}
