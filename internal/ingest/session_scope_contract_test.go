package ingest_test

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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
	Name      string   `yaml:"name"`
	IDs       []string `yaml:"ids"`
	Related   string   `yaml:"related"`
	WantIDs   []string `yaml:"wantIDs"`
	WantError string   `yaml:"wantError"`
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

// Each boundary that refuses an unusable scope must state what only it knows:
// the constructor returns no scope, and the run is refused before the run lock
// and before any stage. The rule text is shared; these are the clauses that
// must stay boundary-specific.
const (
	constructorWhenClause   = "when: while building the scope, before any run exists"
	constructorImpactClause = "impact: no scope is returned"
	runWhenClause           = "when: before the run lock and before any stage"
	runImpactClause         = "impact: no source was read and no session or store row was touched"
)

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
			ids := sessionScopeIDs(testCase.IDs)
			scope, err := ingest.NewSessionScope(ids, related)
			if testCase.WantError != "" {
				if err == nil {
					t.Fatalf("NewSessionScope(%v) returned a scope, want an error containing %q", testCase.IDs, testCase.WantError)
				}
				if !strings.Contains(err.Error(), testCase.WantError) {
					t.Fatalf("NewSessionScope error does not say %q:\n%v", testCase.WantError, err)
				}
				// The two boundary-specific clauses are what the constructor
				// knows: no pipeline runs, so what the caller loses is the scope.
				if strings.Contains(err.Error(), "names no sessions") || strings.Contains(err.Error(), "carries no related policy") {
					if !strings.Contains(err.Error(), constructorWhenClause) || !strings.Contains(err.Error(), constructorImpactClause) {
						t.Fatalf("the constructor does not state its own when and impact:\n%v", err)
					}
				}
				if strings.Contains(err.Error(), runWhenClause) {
					t.Fatalf("the constructor borrowed the run's when clause:\n%v", err)
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
			// The scope owns its input: changing the caller's slice after
			// construction must not change what the scope names.
			ids[0] = ingest.SessionID("mutated-after-construction")
			if !slices.Equal(scope.IDs(), want) {
				t.Fatalf("mutating the caller's ids changed the scope: IDs() = %v, want %v", scope.IDs(), want)
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
// entry point. The refusal must return no result and start no stage, so no
// source is read and no store row is touched; the progress recorder is what
// makes a check moved below the first stage go red.
func TestPipelineRunRefusesUnusableScope(t *testing.T) {
	t.Parallel()
	fixture := loadSessionScopeContractFixture(t)
	for _, testCase := range fixture.RunRefusals {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			progress := ingest.NewProgressState()
			cfg := ingest.PipelineConfig{Progress: progress}
			store := &scopeRefusalStore{}
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
			lockPath := filepath.Join(t.TempDir(), "harvest.lock")
			pipeline, err := newTestPipeline(nil, nil, ingest.DefaultAdapterRegistry, cfg,
				ingest.WithStore(store), ingest.WithRunLock(lockPath))
			if err != nil {
				t.Fatalf("build pipeline: %v", err)
			}
			result, err := pipeline.Run(context.Background())
			if result != nil {
				t.Fatalf("Run returned a partial result (%v); a stage ran before the refusal", result)
			}
			if err == nil {
				t.Fatal("Run returned no error, want a refusal")
			}
			if !strings.Contains(err.Error(), testCase.WantError) {
				t.Fatalf("Run error does not say %q:\n%v", testCase.WantError, err)
			}
			// A run refusal says what it prevented and when: before the run lock
			// and before any stage.
			if !strings.Contains(err.Error(), runWhenClause) || !strings.Contains(err.Error(), runImpactClause) {
				t.Fatalf("the run refusal does not state the ordering guarantee:\n%v", err)
			}
			if strings.Contains(err.Error(), constructorWhenClause) {
				t.Fatalf("the run refusal borrowed the constructor's when clause:\n%v", err)
			}
			if len(store.calls) > 0 {
				t.Fatalf("the refusal touched the store: %v", store.calls)
			}
			if _, statErr := os.Stat(lockPath); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("the refusal took the run lock at %s (stat error %v)", lockPath, statErr)
			}
			for stage, state := range progress.Snapshot() {
				if state.Started || state.Ended {
					t.Fatalf("stage %s ran before the refusal", stage)
				}
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
		})
	}
}

// scopeClosedSets is the one list of the closed sets the session_scope*.go files
// declare as uint8 types. The census below requires every such type to appear
// here, so a new set cannot ship without an assertion that its zero value is
// refused. The valid members are listed by hand beside each set's constants and
// must be updated with them; the census checks that every listed member
// validates and that no two share a value, but it does not read the const
// blocks.
var scopeClosedSets = []struct {
	goName string
	zero   any
	valid  []string
	parse  func(string) (any, error)
}{
	{
		goName: "RelatedPolicy",
		zero:   ingest.RelatedPolicy(0),
		valid:  []string{"include", "exclude"},
		parse: func(name string) (any, error) {
			switch name {
			case "include":
				return ingest.RelatedInclude, nil
			case "exclude":
				return ingest.RelatedExclude, nil
			}
			return nil, fmt.Errorf("related policy %q is not a declared value", name)
		},
	},
	{
		goName: "LocateOutcome",
		zero:   ingest.LocateOutcome(0),
		valid:  []string{"stored_path", "direct_lookup", "stored_unreadable"},
		parse: func(name string) (any, error) {
			switch name {
			case "stored_path":
				return ingest.LocateStoredPath, nil
			case "direct_lookup":
				return ingest.LocateDirectLookup, nil
			case "stored_unreadable":
				return ingest.LocateStoredUnreadable, nil
			}
			return nil, fmt.Errorf("locate outcome %q is not a declared value", name)
		},
	},
	{
		goName: "UnreadableReason",
		zero:   ingest.UnreadableReason(0),
		valid:  []string{"source_gone", "harness_disabled", "source_failed"},
		parse: func(name string) (any, error) {
			switch name {
			case "source_gone":
				return ingest.UnreadableSourceGone, nil
			case "harness_disabled":
				return ingest.UnreadableHarnessDisabled, nil
			case "source_failed":
				return ingest.UnreadableSourceFailed, nil
			}
			return nil, fmt.Errorf("unreadable reason %q is not a declared value", name)
		},
	},
	{
		goName: "CohortRole",
		zero:   ingest.CohortRole(0),
		valid:  []string{"selected", "child", "ancestor", "context"},
		parse: func(name string) (any, error) {
			switch name {
			case "selected":
				return ingest.CohortSelected, nil
			case "child":
				return ingest.CohortChild, nil
			case "ancestor":
				return ingest.CohortAncestor, nil
			case "context":
				return ingest.CohortContext, nil
			}
			return nil, fmt.Errorf("cohort role %q is not a declared value", name)
		},
	},
	{
		goName: "ProbeKind",
		zero:   ingest.ProbeKind(0),
		valid:  []string{"path", "row", "catalog_window"},
		parse: func(name string) (any, error) {
			switch name {
			case "path":
				return ingest.ProbePath, nil
			case "row":
				return ingest.ProbeRow, nil
			case "catalog_window":
				return ingest.ProbeCatalogWindow, nil
			}
			return nil, fmt.Errorf("probe kind %q is not a declared value", name)
		},
	},
	{
		goName: "ClaudeBuildMode",
		zero:   ingest.ClaudeBuildMode(0),
		valid:  []string{"full_walk", "scoped"},
		parse: func(name string) (any, error) {
			switch name {
			case "full_walk":
				return ingest.ClaudeBuildFullWalk, nil
			case "scoped":
				return ingest.ClaudeBuildScoped, nil
			}
			return nil, fmt.Errorf("Claude build mode %q is not a declared value", name)
		},
	},
	{
		goName: "SearchMethod",
		zero:   ingest.SearchMethod(0),
		valid:  []string{"stored_path", "name_pattern", "sqlite_row", "unsupported", "harness_disabled", "failed"},
		parse: func(name string) (any, error) {
			method, err := sessionScopeSearchMethod(name)
			return method, err
		},
	},
	{
		goName: "UnsupportedReason",
		zero:   ingest.UnsupportedReason(0),
		valid:  []string{"file_header", "file_body"},
		parse: func(name string) (any, error) {
			reason, err := sessionScopeUnsupportedReason(name)
			return reason, err
		},
	},
	{
		goName: "MissReason",
		zero:   ingest.MissReason(0),
		valid:  []string{"not_found", "harness_filtered", "since_filtered"},
		parse: func(name string) (any, error) {
			reason, err := sessionScopeMissReason(name)
			return reason, err
		},
	},
	{
		goName: "StoreAttachment",
		zero:   ingest.StoreAttachment(0),
		valid:  []string{"attached", "not_attached"},
		parse: func(name string) (any, error) {
			attachment, err := sessionScopeStoreAttachment(name)
			return attachment, err
		},
	},
}

// TestClosedSetsDeclareAnInvalidZeroAndEveryMember pins the closed-set rule:
// every declared set refuses its zero value, and every member it names is
// valid and distinct. The census makes a new set in session_scope*.go fail
// until it is added to the table above, which is what keeps this list from
// going stale.
func TestClosedSetsDeclareAnInvalidZeroAndEveryMember(t *testing.T) {
	t.Parallel()
	declared := scopeUint8TypeNames(t)
	table := make(map[string]struct{}, len(scopeClosedSets))
	for _, set := range scopeClosedSets {
		if _, duplicate := table[set.goName]; duplicate {
			t.Fatalf("%s appears twice in the closed-set table", set.goName)
		}
		table[set.goName] = struct{}{}
		if _, ok := declared[set.goName]; !ok {
			t.Fatalf("the closed-set table names %s, which no session_scope*.go file declares as a uint8 type", set.goName)
		}

		zero := reflect.ValueOf(set.zero)
		if zero.Uint() != 0 {
			t.Fatalf("%s: the table's zero value is not the zero value", set.goName)
		}
		zeroValid, ok := zero.Interface().(validatable)
		if !ok {
			t.Fatalf("%s has no IsValid method; every declared closed set must refuse its zero value", set.goName)
		}
		if zeroValid.IsValid() {
			t.Fatalf("%s: the zero value is valid; an unset field would claim a real choice", set.goName)
		}

		seen := make(map[uint64]string, len(set.valid))
		for _, name := range set.valid {
			member, err := set.parse(name)
			if err != nil {
				t.Fatalf("%s: %v", set.goName, err)
			}
			value := reflect.ValueOf(member)
			if value.Type() != zero.Type() {
				t.Fatalf("%s: %q parsed to %s, want %s", set.goName, name, value.Type(), zero.Type())
			}
			memberValue, ok := value.Interface().(validatable)
			if !ok {
				t.Fatalf("%s has no IsValid method; every declared closed set must refuse its zero value", set.goName)
			}
			if !memberValue.IsValid() {
				t.Fatalf("%s: member %q is not valid", set.goName, name)
			}
			number := value.Uint()
			if previous, duplicate := seen[number]; duplicate {
				t.Fatalf("%s: %q and %q share the numeric value %d", set.goName, previous, name, number)
			}
			seen[number] = name
		}
	}
	for name := range declared {
		if _, ok := table[name]; !ok {
			t.Fatalf("session_scope*.go declares the closed set %s with no entry in the closed-set table", name)
		}
	}
}

type validatable interface{ IsValid() bool }

// scopeUint8TypeNames returns every type a non-test session_scope*.go file
// declares with an underlying uint8. That is the census's definition of a
// closed set in these files.
func scopeUint8TypeNames(t *testing.T) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	names := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "session_scope") ||
			!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			generic, ok := decl.(*ast.GenDecl)
			if !ok || generic.Tok != token.TYPE {
				continue
			}
			for _, spec := range generic.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if underlying, ok := typeSpec.Type.(*ast.Ident); ok && underlying.Name == "uint8" {
					names[typeSpec.Name.Name] = struct{}{}
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("the census found no closed sets; the pattern no longer matches the declaring files")
	}
	return names
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

// scopeRefusalStore records every SessionStore call it receives. A refused run
// must make none, so any recorded name is the failure: this is the observable
// "before touching the store" the refusal promises.
type scopeRefusalStore struct {
	calls []string
}

var _ ingest.SessionStore = (*scopeRefusalStore)(nil)

func (s *scopeRefusalStore) InsertSessions(context.Context, []ingest.StoreEntry) error {
	s.calls = append(s.calls, "InsertSessions")
	return nil
}

func (s *scopeRefusalStore) LookupSessionLocation(context.Context, ingest.SessionID) (string, string, error) {
	s.calls = append(s.calls, "LookupSessionLocation")
	return "", "", nil
}

func (s *scopeRefusalStore) BulkLookupSessionLocations(context.Context, []ingest.SessionID) (map[ingest.SessionID]ingest.SessionLocation, error) {
	s.calls = append(s.calls, "BulkLookupSessionLocations")
	return nil, nil
}

func (s *scopeRefusalStore) UpsertSessionCommits(context.Context, ingest.SessionID, []ingest.CommitInfo) error {
	s.calls = append(s.calls, "UpsertSessionCommits")
	return nil
}

func (s *scopeRefusalStore) CleanupOrphanProjects(context.Context) error {
	s.calls = append(s.calls, "CleanupOrphanProjects")
	return nil
}

func (s *scopeRefusalStore) Close() error {
	s.calls = append(s.calls, "Close")
	return nil
}
