// Package autopublishtest loads the auto-publish rules fixture,
// internal/autopublish/testdata/auto-publish-rules.yaml, for the tests of every
// package that drives its cases: the matcher, the settings routes, the hook
// push, and `peasant village auto`. One loader validates the whole file, so a
// case one package drives cannot drift from the shape another expects.
package autopublishtest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// Path is the fixture's path from the module root.
const Path = "internal/autopublish/testdata/auto-publish-rules.yaml"

// Driver names the test that runs a case. The set is closed: every case is
// run by exactly one test, and each test asserts it ran every case it owns.
type Driver string

const (
	// DriverMatcher runs the matcher on a repository (internal/autopublish).
	DriverMatcher Driver = "matcher"
	// DriverValidate reads a rule that must be refused (internal/autopublish).
	DriverValidate Driver = "validate"
	// DriverInstall saves a rule and installs it through the mounted settings
	// routes (internal/api).
	DriverInstall Driver = "install"
	// DriverPush runs `peasant village push --repository` in a repository a
	// rule may cover, against a Village double (cmd/peasant).
	DriverPush Driver = "push"
	// DriverHook runs the managed hook a rule installed through git push, with
	// an upload that fails (cmd/peasant).
	DriverHook Driver = "hook"
	// DriverVillageAuto runs `peasant village auto` (cmd/peasant).
	DriverVillageAuto Driver = "village-auto"
)

// AllDrivers is the closed driver set.
var AllDrivers = []Driver{DriverMatcher, DriverValidate, DriverInstall, DriverPush, DriverHook, DriverVillageAuto}

// Collective is one collective the Village double knows, by alias.
type Collective struct {
	ID         schema.VillageUUID                `yaml:"id"`
	Name       string                            `yaml:"name"`
	Acceptance schema.VillageGroupAcceptanceMode `yaml:"acceptance"`
}

// Fixture is the whole file.
type Fixture struct {
	RequiredNames []string              `yaml:"requiredNames"`
	Collectives   map[string]Collective `yaml:"collectives"`
	Cases         []Case                `yaml:"cases"`
}

// Case is one named case. A driver reads only the fields it documents.
type Case struct {
	Name   string `yaml:"name"`
	Driver Driver `yaml:"driver"`
	// Rules is the content of hooks.yaml. Collectives are named by alias and
	// resolved to their identifiers when the fixture loads. "{world}" in a
	// match is the directory that holds the world's repositories, and
	// "{remote}" the recorded repository's remote in bare form.
	Rules []autopublish.Rule `yaml:"rules"`
	// RulesFile, when set, is written as hooks.yaml verbatim instead of Rules
	// (push).
	RulesFile string `yaml:"rulesFile"`
	// Repository is what the matcher reads (matcher).
	Repository struct {
		Root   string `yaml:"root"`
		Remote string `yaml:"remote"`
	} `yaml:"repository"`
	// Install names the repository the install call names: recorded or
	// unrecorded (install).
	Install string `yaml:"install"`
	// ForeignHook is the content of a pre-push hook Peasant did not write,
	// placed in the recorded repository first (install, village-auto).
	ForeignHook string `yaml:"foreignHook"`
	// Config is the push configuration (push).
	Config struct {
		Visibility schema.Visibility `yaml:"visibility"`
		License    schema.License    `yaml:"license"`
	} `yaml:"config"`
	// Flags are extra push flags (push).
	Flags []string `yaml:"flags"`
	// Again, when set, pushes a second time with these flags after the
	// first push, which runs without Flags (push).
	Again []string `yaml:"again"`
	// FailShare names collectives the Village double refuses to share with
	// (push).
	FailShare []string `yaml:"failShare"`
	// UploadExits are the exit statuses of the failing uploads the hook runs
	// (hook).
	UploadExits []int `yaml:"uploadExits"`
	// Published says a session was published before the command runs, and
	// Shared the collectives its transcript is shared with, with their status
	// (village-auto).
	Published bool                                 `yaml:"published"`
	Shared    map[string]schema.VillageShareStatus `yaml:"shared"`
	// Unrecorded runs the command in a repository with no recorded session
	// (village-auto).
	Unrecorded bool   `yaml:"unrecorded"`
	Expect     Expect `yaml:"expect"`
}

// Expect is what a case asserts. A driver asserts only the fields it
// documents.
type Expect struct {
	// Applying are the identifiers of the rules that publish the repository,
	// and Collectives the aliases of the collectives it is shared with, in
	// order (matcher).
	Applying    []string `yaml:"applying"`
	Collectives []string `yaml:"collectives"`
	// Invalid is part of the reason the rule is refused (validate).
	Invalid string `yaml:"invalid"`
	// Status and Code are the install answer; Hooks each event's status;
	// RemedyContains and SnippetContains parts of a hook's remedy (install).
	Status          int                                                      `yaml:"status"`
	Code            string                                                   `yaml:"code"`
	Hooks           map[schema.AutoPublishEvent]schema.AutoPublishHookStatus `yaml:"hooks"`
	RemedyContains  []string                                                 `yaml:"remedyContains"`
	SnippetContains []string                                                 `yaml:"snippetContains"`
	// Installed names the repositories that hold a Peasant-managed pre-push
	// hook afterwards (install, village-auto).
	Installed []string `yaml:"installed"`
	// ErrorContains are parts of the command's error; none means it succeeds
	// (push, village-auto).
	ErrorContains []string `yaml:"errorContains"`
	// Publishes counts the uploads Village received; License is the license
	// the upload carried; Audience the transcript's collectives afterwards, by
	// alias; OwnerUpdates the owner visibility updates (push).
	Publishes    *int                                 `yaml:"publishes"`
	License      schema.License                       `yaml:"license"`
	Audience     map[string]schema.VillageShareStatus `yaml:"audience"`
	OwnerUpdates int                                  `yaml:"ownerUpdates"`
	// Output is the exact standard output (village-auto).
	Output string `yaml:"output"`
	// Rule is the rule hooks.yaml holds afterwards; "{remote}" in its match is
	// the world's remote in bare form (village-auto).
	Rule *struct {
		Kind        schema.AutoPublishRuleKind `yaml:"kind"`
		Match       string                     `yaml:"match"`
		Events      []schema.AutoPublishEvent  `yaml:"events"`
		Collectives []string                   `yaml:"collectives"`
	} `yaml:"rule"`
}

// Load reads and validates the fixture from the module root.
func Load(t *testing.T) Fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testutil.ModuleRoot(t), Path))
	if err != nil {
		t.Fatalf("%s: %v", Path, err)
	}
	return Parse(t, raw)
}

// Parse validates the fixture and resolves collective aliases in its rules.
func Parse(t testing.TB, raw []byte) Fixture {
	t.Helper()
	var fixture Fixture
	if err := testutil.DecodeNamedFixtureYAML(raw, &fixture); err != nil {
		t.Fatalf("%s: %v", Path, err)
	}
	for i := range fixture.Cases {
		c := &fixture.Cases[i]
		if !slices.Contains(AllDrivers, c.Driver) {
			t.Fatalf("%s: case %q names driver %q; use one of %v", Path, c.Name, c.Driver, AllDrivers)
		}
		for j := range c.Rules {
			for k, alias := range c.Rules[j].Collectives {
				collective, ok := fixture.Collectives[string(alias)]
				if !ok {
					t.Fatalf("%s: case %q names collective %q, which the fixture does not declare", Path, c.Name, alias)
				}
				c.Rules[j].Collectives[k] = collective.ID
			}
		}
		aliases := append(append(mapKeys(c.Shared), mapKeys(c.Expect.Audience)...), c.FailShare...)
		aliases = append(aliases, c.Expect.Collectives...)
		if c.Expect.Rule != nil {
			aliases = append(aliases, c.Expect.Rule.Collectives...)
		}
		for _, alias := range aliases {
			if _, ok := fixture.Collectives[alias]; !ok {
				t.Fatalf("%s: case %q names collective %q, which the fixture does not declare", Path, c.Name, alias)
			}
		}
	}
	return fixture
}

// RulesFor returns the case's rules with the placeholders of their matches
// replaced: "{world}" by the directory that holds the world's repositories,
// and "{remote}" by the world's remote in bare form.
func (c Case) RulesFor(world, remote string) []autopublish.Rule {
	replacer := strings.NewReplacer("{world}", world, "{remote}", remote)
	rules := make([]autopublish.Rule, len(c.Rules))
	for i, rule := range c.Rules {
		rule.Match = replacer.Replace(rule.Match)
		rule.Events = slices.Clone(rule.Events)
		rule.Collectives = slices.Clone(rule.Collectives)
		rules[i] = rule
	}
	return rules
}

// IDs returns the identifiers of the collectives the aliases name.
func (f Fixture) IDs(aliases []string) []schema.VillageUUID {
	ids := make([]schema.VillageUUID, len(aliases))
	for i, alias := range aliases {
		ids[i] = f.Collectives[alias].ID
	}
	return ids
}

// For returns the cases of one driver, failing the test when it owns none: a
// driver that runs no case proves nothing.
func (f Fixture) For(t testing.TB, driver Driver) []Case {
	t.Helper()
	var cases []Case
	for _, c := range f.Cases {
		if c.Driver == driver {
			cases = append(cases, c)
		}
	}
	if len(cases) == 0 {
		t.Fatalf("%s: no case has driver %q", Path, driver)
	}
	return cases
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
