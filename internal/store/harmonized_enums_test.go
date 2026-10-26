package store_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/harmonized_enums.yaml
var harmonizedEnumsYAML []byte

type harmonizedDigestFixtures struct {
	Name   string   `yaml:"name"`
	Accept []string `yaml:"accept"`
	Reject []string `yaml:"reject"`
}

type harmonizedStringSetFixtures struct {
	Name      string   `yaml:"name"`
	Accept    []string `yaml:"accept"`
	Reject    []string `yaml:"reject"`
	ClosedSet []string `yaml:"closed_set"`
}

type harmonizedPhaseFixtures struct {
	Name   string `yaml:"name"`
	Accept []int  `yaml:"accept"`
	Reject []int  `yaml:"reject"`
}

type harmonizedEnumsCorpus struct {
	Required                []string                    `yaml:"required_names"`
	Digests                 harmonizedDigestFixtures    `yaml:"digests"`
	StatsSources            harmonizedStringSetFixtures `yaml:"stats_sources"`
	MigrationPhases         harmonizedPhaseFixtures     `yaml:"migration_phases"`
	RetirementPreconditions harmonizedStringSetFixtures `yaml:"retirement_preconditions"`
	MigrateOutcomes         harmonizedStringSetFixtures `yaml:"migrate_outcomes"`
}

func loadHarmonizedEnumsFixtures(t *testing.T) harmonizedEnumsCorpus {
	t.Helper()
	var corpus harmonizedEnumsCorpus
	if err := testutil.DecodeFixtureYAML(harmonizedEnumsYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	sections := map[string]bool{}
	actual := []string{}
	for _, name := range []string{
		corpus.Digests.Name,
		corpus.StatsSources.Name,
		corpus.MigrationPhases.Name,
		corpus.RetirementPreconditions.Name,
		corpus.MigrateOutcomes.Name,
	} {
		if name == "" || sections[name] {
			t.Fatal("duplicate or blank harmonized enums fixture section")
		}
		sections[name] = true
		actual = append(actual, name)
	}
	if err := testutil.RequireFixtureNames("harmonized enums", "section", corpus.Required, sections); err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: corpus.Required}, actual, "harmonized enums"); err != nil {
		t.Fatal(err)
	}
	return corpus
}

// TestHarmonizedDigestValidators pins the 64-hex trust-boundary shape both
// digest constructors enforce: every accepted fixture value builds through
// NewBodyDigest and NewContentDigest and round-trips through IsValid and
// String, and every rejected value fails both constructors and IsValid.
func TestHarmonizedDigestValidators(t *testing.T) {
	t.Parallel()
	digests := loadHarmonizedEnumsFixtures(t).Digests
	for _, raw := range digests.Accept {
		body, err := store.NewBodyDigest(raw)
		if err != nil {
			t.Errorf("NewBodyDigest(%q): expected accept, got %v", raw, err)
			continue
		}
		if !body.IsValid() || body.String() != raw {
			t.Errorf("NewBodyDigest(%q): IsValid or String drifted: valid=%v string=%q", raw, body.IsValid(), body.String())
		}
		content, err := store.NewContentDigest(raw)
		if err != nil {
			t.Errorf("NewContentDigest(%q): expected accept, got %v", raw, err)
			continue
		}
		if !content.IsValid() || content.String() != raw {
			t.Errorf("NewContentDigest(%q): IsValid or String drifted: valid=%v string=%q", raw, content.IsValid(), content.String())
		}
	}
	for _, raw := range digests.Reject {
		if _, err := store.NewBodyDigest(raw); err == nil {
			t.Errorf("NewBodyDigest(%q): expected reject, got accept", raw)
		}
		if _, err := store.NewContentDigest(raw); err == nil {
			t.Errorf("NewContentDigest(%q): expected reject, got accept", raw)
		}
		if store.BodyDigest(raw).IsValid() {
			t.Errorf("BodyDigest(%q).IsValid: expected false", raw)
		}
		if store.ContentDigest(raw).IsValid() {
			t.Errorf("ContentDigest(%q).IsValid: expected false", raw)
		}
	}
}

// TestHarmonizedStatsSourceSet pins the closed harness|derived row-label set:
// every accepted value builds through NewStatsSource, every rejected value
// fails it, and AllStatsSources holds exactly the fixture closed set.
func TestHarmonizedStatsSourceSet(t *testing.T) {
	t.Parallel()
	fixture := loadHarmonizedEnumsFixtures(t).StatsSources
	for _, raw := range fixture.Accept {
		source, err := store.NewStatsSource(raw)
		if err != nil {
			t.Errorf("NewStatsSource(%q): expected accept, got %v", raw, err)
			continue
		}
		if !source.IsValid() || source.String() != raw {
			t.Errorf("NewStatsSource(%q): IsValid or String drifted", raw)
		}
	}
	for _, raw := range fixture.Reject {
		if _, err := store.NewStatsSource(raw); err == nil {
			t.Errorf("NewStatsSource(%q): expected reject, got accept", raw)
		}
		if store.StatsSource(raw).IsValid() {
			t.Errorf("StatsSource(%q).IsValid: expected false", raw)
		}
	}
	assertClosedStringSet(t, "stats sources", fixture.ClosedSet, statsSourceMembers(), func(raw string) bool {
		return store.StatsSource(raw).IsValid()
	})
}

func statsSourceMembers() []string {
	members := make([]string, 0, len(store.AllStatsSources))
	for _, source := range store.AllStatsSources {
		members = append(members, string(source))
	}
	return members
}

// TestHarmonizedMigrationPhaseRange pins the 0..5 progress axis: every
// accepted phase builds through NewMigrationPhase with a named String form,
// every rejected value fails it, and AllMigrationPhases holds 0..5 in order.
func TestHarmonizedMigrationPhaseRange(t *testing.T) {
	t.Parallel()
	fixture := loadHarmonizedEnumsFixtures(t).MigrationPhases
	for _, raw := range fixture.Accept {
		phase, err := store.NewMigrationPhase(raw)
		if err != nil {
			t.Errorf("NewMigrationPhase(%d): expected accept, got %v", raw, err)
			continue
		}
		if !phase.IsValid() || phase.String() == "unknown" {
			t.Errorf("NewMigrationPhase(%d): IsValid or String drifted: valid=%v string=%q", raw, phase.IsValid(), phase.String())
		}
	}
	for _, raw := range fixture.Reject {
		if _, err := store.NewMigrationPhase(raw); err == nil {
			t.Errorf("NewMigrationPhase(%d): expected reject, got accept", raw)
		}
		if store.MigrationPhase(raw).IsValid() {
			t.Errorf("MigrationPhase(%d).IsValid: expected false", raw)
		}
	}
	if len(store.AllMigrationPhases) != len(fixture.Accept) {
		t.Fatalf("AllMigrationPhases: expected %d members, got %d", len(fixture.Accept), len(store.AllMigrationPhases))
	}
	for i, phase := range store.AllMigrationPhases {
		if int(phase) != fixture.Accept[i] {
			t.Errorf("AllMigrationPhases[%d]: expected phase %d, got %d", i, fixture.Accept[i], int(phase))
		}
	}
}

// TestHarmonizedRetirementPreconditionSet pins the five Release N+1 guards:
// every accepted name builds through NewRetirementPrecondition, every
// rejected name fails it, and AllRetirementPreconditions holds exactly the
// fixture closed set.
func TestHarmonizedRetirementPreconditionSet(t *testing.T) {
	t.Parallel()
	fixture := loadHarmonizedEnumsFixtures(t).RetirementPreconditions
	for _, raw := range fixture.Accept {
		precondition, err := store.NewRetirementPrecondition(raw)
		if err != nil {
			t.Errorf("NewRetirementPrecondition(%q): expected accept, got %v", raw, err)
			continue
		}
		if !precondition.IsValid() || precondition.String() != raw {
			t.Errorf("NewRetirementPrecondition(%q): IsValid or String drifted", raw)
		}
	}
	for _, raw := range fixture.Reject {
		if _, err := store.NewRetirementPrecondition(raw); err == nil {
			t.Errorf("NewRetirementPrecondition(%q): expected reject, got accept", raw)
		}
		if store.RetirementPrecondition(raw).IsValid() {
			t.Errorf("RetirementPrecondition(%q).IsValid: expected false", raw)
		}
	}
	members := make([]string, 0, len(store.AllRetirementPreconditions))
	for _, precondition := range store.AllRetirementPreconditions {
		members = append(members, string(precondition))
	}
	assertClosedStringSet(t, "retirement preconditions", fixture.ClosedSet, members, func(raw string) bool {
		return store.RetirementPrecondition(raw).IsValid()
	})
}

// TestHarmonizedMigrateOutcomeSet pins the four per-session dispositions:
// every accepted name builds through NewMigrateOutcome, every rejected name
// fails it, and AllMigrateOutcomes holds exactly the fixture closed set.
func TestHarmonizedMigrateOutcomeSet(t *testing.T) {
	t.Parallel()
	fixture := loadHarmonizedEnumsFixtures(t).MigrateOutcomes
	for _, raw := range fixture.Accept {
		outcome, err := store.NewMigrateOutcome(raw)
		if err != nil {
			t.Errorf("NewMigrateOutcome(%q): expected accept, got %v", raw, err)
			continue
		}
		if !outcome.IsValid() || outcome.String() != raw {
			t.Errorf("NewMigrateOutcome(%q): IsValid or String drifted", raw)
		}
	}
	for _, raw := range fixture.Reject {
		if _, err := store.NewMigrateOutcome(raw); err == nil {
			t.Errorf("NewMigrateOutcome(%q): expected reject, got accept", raw)
		}
		if store.MigrateOutcome(raw).IsValid() {
			t.Errorf("MigrateOutcome(%q).IsValid: expected false", raw)
		}
	}
	members := make([]string, 0, len(store.AllMigrateOutcomes))
	for _, outcome := range store.AllMigrateOutcomes {
		members = append(members, string(outcome))
	}
	assertClosedStringSet(t, "migrate outcomes", fixture.ClosedSet, members, func(raw string) bool {
		return store.MigrateOutcome(raw).IsValid()
	})
}

// assertClosedStringSet pins exact closed-set membership in both directions:
// every fixture member validates, and the production set holds no member the
// fixture does not name.
func assertClosedStringSet(t *testing.T, label string, closedSet, members []string, valid func(string) bool) {
	t.Helper()
	for _, raw := range closedSet {
		if !valid(raw) {
			t.Errorf("%s: closed-set member %q fails IsValid", label, raw)
		}
	}
	declared := make(map[string]bool, len(closedSet))
	for _, raw := range closedSet {
		declared[raw] = true
	}
	for _, member := range members {
		if !declared[member] {
			t.Errorf("%s: production set carries undeclared member %q", label, member)
		}
	}
	if len(members) != len(closedSet) {
		t.Errorf("%s: expected %d members, got %d", label, len(closedSet), len(members))
	}
}
