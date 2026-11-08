package ingest_test

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/session_scope_contract.yaml
var sessionScopeContractYAML []byte

//go:embed testdata/session_scope_contract.manifest.yaml
var sessionScopeContractManifestYAML []byte

type sessionScopeConstructionCase struct {
	Name       string   `yaml:"name"`
	IDs        []string `yaml:"ids"`
	Related    string   `yaml:"related"`
	WantIDs    []string `yaml:"wantIDs"`
	WantCopied bool     `yaml:"wantCopied"`
	WantError  string   `yaml:"wantError"`
}

type sessionScopeRunRefusalCase struct {
	Name      string   `yaml:"name"`
	Scope     string   `yaml:"scope"`
	IDs       []string `yaml:"ids"`
	Allowed   []string `yaml:"allowed"`
	WantError string   `yaml:"wantError"`
}

type sessionScopeReportMember struct {
	ID      string `yaml:"id"`
	Role    string `yaml:"role"`
	Outcome string `yaml:"outcome"`
}

type sessionScopeReportCase struct {
	Name          string                     `yaml:"name"`
	Members       []sessionScopeReportMember `yaml:"members"`
	WantProcessed []string                   `yaml:"wantProcessed"`
	WantUpkeep    []string                   `yaml:"wantUpkeep"`
}

type sessionScopeContractFixture struct {
	Construction []sessionScopeConstructionCase `yaml:"construction"`
	RunRefusals  []sessionScopeRunRefusalCase   `yaml:"runRefusals"`
	Report       []sessionScopeReportCase       `yaml:"report"`
}

func loadSessionScopeContractFixture(t *testing.T) sessionScopeContractFixture {
	t.Helper()
	var fixture sessionScopeContractFixture
	if err := yaml.Unmarshal(sessionScopeContractYAML, &fixture); err != nil {
		t.Fatalf("decode session scope contract fixture: %v", err)
	}
	names := make([]string, 0, len(fixture.Construction)+len(fixture.RunRefusals)+len(fixture.Report))
	for _, testCase := range fixture.Construction {
		names = append(names, testCase.Name)
	}
	for _, testCase := range fixture.RunRefusals {
		names = append(names, testCase.Name)
	}
	for _, testCase := range fixture.Report {
		names = append(names, testCase.Name)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(sessionScopeContractManifestYAML, "session scope contract")
	if err != nil {
		t.Fatalf("decode session scope contract manifest: %v", err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "session scope contract"); err != nil {
		t.Fatalf("validate session scope contract manifest: %v", err)
	}
	return fixture
}

// TestNewSessionScopeContract pins the boundary constructor: sorted ids, no
// duplicates, its own copy, and a refusal for each unusable input. Dropping any
// one of them changes what a scoped run reads.
func TestNewSessionScopeContract(t *testing.T) {
	t.Parallel()
	fixture := loadSessionScopeContractFixture(t)
	for _, testCase := range fixture.Construction {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			related, err := sessionScopeRelatedPolicy(testCase.Related)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := ingest.NewSessionScope(sessionScopeIDs(testCase.IDs), related)
			if testCase.WantError != "" {
				if err == nil {
					t.Fatalf("NewSessionScope(%v) returned a scope, want an error containing %q", testCase.IDs, testCase.WantError)
				}
				if !strings.Contains(err.Error(), testCase.WantError) {
					t.Fatalf("NewSessionScope error does not say %q:\n%v", testCase.WantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewSessionScope(%v, %v): %v", testCase.IDs, related, err)
			}
			want := sessionScopeIDs(testCase.WantIDs)
			if !slices.Equal(scope.IDs(), want) {
				t.Fatalf("IDs() = %v, want %v", scope.IDs(), want)
			}
			if scope.Related() != related {
				t.Fatalf("Related() = %d, want %d", scope.Related(), related)
			}
			if !testCase.WantCopied {
				return
			}
			returned := scope.IDs()
			returned[0] = ingest.SessionID("mutated-outside-the-scope")
			if !slices.Equal(scope.IDs(), want) {
				t.Fatalf("mutating the returned ids changed the scope: IDs() = %v, want %v", scope.IDs(), want)
			}
		})
	}
}

// TestPipelineRunRefusesUnusableScope pins both refusals at the production
// entry point: Run returns before any stage, so no source is read and no store
// row is touched. A scope that cannot narrow the run, and a run narrowed twice,
// are both refused.
func TestPipelineRunRefusesUnusableScope(t *testing.T) {
	t.Parallel()
	fixture := loadSessionScopeContractFixture(t)
	for _, testCase := range fixture.RunRefusals {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			var cfg ingest.PipelineConfig
			switch testCase.Scope {
			case "zero":
				cfg.Scope = &ingest.SessionScope{}
			case "built":
				scope, err := ingest.NewSessionScope(sessionScopeIDs(testCase.IDs), ingest.RelatedInclude)
				if err != nil {
					t.Fatalf("build scope: %v", err)
				}
				cfg.Scope = &scope
			default:
				t.Fatalf("fixture scope %q is not a declared value", testCase.Scope)
			}
			if len(testCase.Allowed) > 0 {
				cfg.AllowedSessionIDs = make(map[ingest.SessionID]bool, len(testCase.Allowed))
				for _, id := range testCase.Allowed {
					cfg.AllowedSessionIDs[ingest.SessionID(id)] = true
				}
			}
			pipeline, err := newTestPipeline(nil, nil, ingest.DefaultAdapterRegistry, cfg)
			if err != nil {
				t.Fatalf("build pipeline: %v", err)
			}
			result, err := pipeline.Run(context.Background())
			if err == nil {
				t.Fatalf("Run returned a result (%v), want a refusal", result)
			}
			if !strings.Contains(err.Error(), testCase.WantError) {
				t.Fatalf("Run error does not say %q:\n%v", testCase.WantError, err)
			}
		})
	}
}

// TestScopeReportProcessedAndUpkeep pins the split that decides what a scoped
// run may work on and what the store-side passes may touch. In particular a
// stored-but-unreadable selected member is upkeep but never processed, and a
// context member is in neither set.
func TestScopeReportProcessedAndUpkeep(t *testing.T) {
	t.Parallel()
	fixture := loadSessionScopeContractFixture(t)
	for _, testCase := range fixture.Report {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			members := make([]ingest.LocatedMember, 0, len(testCase.Members))
			for _, fixtureMember := range testCase.Members {
				member, err := sessionScopeLocatedMember(fixtureMember)
				if err != nil {
					t.Fatal(err)
				}
				members = append(members, member)
			}
			report := ingest.ScopeReport{Members: members}
			processed := report.Processed()
			upkeep := report.Upkeep()
			if want := sessionScopeIDs(testCase.WantProcessed); !slices.Equal(processed, want) {
				t.Fatalf("Processed() = %v, want %v", processed, want)
			}
			if want := sessionScopeIDs(testCase.WantUpkeep); !slices.Equal(upkeep, want) {
				t.Fatalf("Upkeep() = %v, want %v", upkeep, want)
			}
			for _, id := range processed {
				if !slices.Contains(upkeep, id) {
					t.Fatalf("Processed() names %s, which Upkeep() does not carry", id)
				}
			}
		})
	}
}

// TestClosedSetZeroValuesAreInvalid pins the invalid zero value of every closed
// set this slice declares. A value inserted at the top of a constant block would
// shift the rest, and a zero that quietly passed IsValid would let an unset
// field claim a real choice.
func TestClosedSetZeroValuesAreInvalid(t *testing.T) {
	t.Parallel()
	if ingest.RelatedPolicy(0).IsValid() {
		t.Error("the zero RelatedPolicy is valid; an unset policy must be refused")
	}
	if ingest.LocateOutcome(0).IsValid() {
		t.Error("the zero LocateOutcome is valid")
	}
	if ingest.UnreadableReason(0).IsValid() {
		t.Error("the zero UnreadableReason is valid")
	}
	if ingest.CohortRole(0).IsValid() {
		t.Error("the zero CohortRole is valid")
	}
	if ingest.ProbeKind(0).IsValid() {
		t.Error("the zero ProbeKind is valid")
	}
	if ingest.ClaudeBuildMode(0).IsValid() {
		t.Error("the zero ClaudeBuildMode is valid")
	}
	if ingest.SearchMethod(0).IsValid() {
		t.Error("the zero SearchMethod is valid")
	}
	if ingest.UnsupportedReason(0).IsValid() {
		t.Error("the zero UnsupportedReason is valid")
	}
	if ingest.MissReason(0).IsValid() {
		t.Error("the zero MissReason is valid")
	}
	if ingest.StoreAttachment(0).IsValid() {
		t.Error("the zero StoreAttachment is valid")
	}
}

func sessionScopeIDs(raw []string) []ingest.SessionID {
	ids := make([]ingest.SessionID, 0, len(raw))
	for _, id := range raw {
		ids = append(ids, ingest.SessionID(id))
	}
	return ids
}

func sessionScopeRelatedPolicy(raw string) (ingest.RelatedPolicy, error) {
	switch raw {
	case "include":
		return ingest.RelatedInclude, nil
	case "exclude":
		return ingest.RelatedExclude, nil
	case "":
		return ingest.RelatedPolicy(0), nil
	default:
		return ingest.RelatedPolicy(0), fmt.Errorf("fixture related policy %q is not a declared value", raw)
	}
}

func sessionScopeLocatedMember(fixtureMember sessionScopeReportMember) (ingest.LocatedMember, error) {
	id := ingest.SessionID(fixtureMember.ID)
	member := ingest.LocatedMember{ID: id}

	switch fixtureMember.Role {
	case "selected":
		member.Role = ingest.CohortSelected
	case "child":
		member.Role = ingest.CohortChild
	case "ancestor":
		member.Role = ingest.CohortAncestor
	case "context":
		member.Role = ingest.CohortContext
	default:
		return ingest.LocatedMember{}, fmt.Errorf("fixture role %q is not a declared value", fixtureMember.Role)
	}

	switch fixtureMember.Outcome {
	case "stored-path":
		member.Outcome = ingest.LocateStoredPath
	case "direct-lookup":
		member.Outcome = ingest.LocateDirectLookup
	case "stored-unreadable":
		member.Outcome = ingest.LocateStoredUnreadable
		member.Unreadable = ingest.UnreadableSourceGone
	default:
		return ingest.LocatedMember{}, fmt.Errorf("fixture outcome %q is not a declared value", fixtureMember.Outcome)
	}
	if member.Outcome != ingest.LocateStoredUnreadable {
		member.Source = &ingest.DiscoveredSession{SessionID: id}
	}
	return member, nil
}
