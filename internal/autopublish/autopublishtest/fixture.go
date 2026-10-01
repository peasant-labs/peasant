// Package autopublishtest loads the auto-publish rules fixture,
// internal/autopublish/testdata/auto-publish-rules.yaml, for the tests of every
// package that drives its cases: the matcher, the settings routes, the hook
// push, and `peasant village auto`. One loader validates the whole file, so a
// case one package drives cannot drift from the shape another expects.
package autopublishtest

import (
	"os"
	"path/filepath"
	"reflect"
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
// run by exactly one test, each test asserts it ran every case it owns, and a
// source guard asserts each driver has exactly one test.
type Driver string

const (
	// DriverMatcher runs the matcher on a repository (internal/autopublish).
	DriverMatcher Driver = "matcher"
	// DriverValidate reads a rule, or a rules file, that must be refused
	// (internal/autopublish).
	DriverValidate Driver = "validate"
	// DriverInstall saves the rules and installs the first one through the
	// mounted settings routes (internal/api).
	DriverInstall Driver = "install"
	// DriverRuleBody sends a body the settings routes must refuse
	// (internal/api).
	DriverRuleBody Driver = "rule-body"
	// DriverPush runs `peasant village push` against a Village double
	// (cmd/peasant).
	DriverPush Driver = "push"
	// DriverHook installs a rule's hook with `peasant village auto` and runs it
	// through git push with an upload that fails (cmd/peasant).
	DriverHook Driver = "hook"
	// DriverVillageAuto runs `peasant village auto` (cmd/peasant).
	DriverVillageAuto Driver = "village-auto"
)

// AllDrivers is the closed driver set.
var AllDrivers = []Driver{DriverMatcher, DriverValidate, DriverInstall, DriverRuleBody, DriverPush, DriverHook, DriverVillageAuto}

// fieldsOf are the case and expectation fields each driver reads, by YAML
// name. A case that sets another field would assert nothing with it.
var fieldsOf = map[Driver][]string{
	DriverMatcher:     {"rules", "repository", "applying", "paused", "collectives"},
	DriverValidate:    {"rules", "rulesFile", "invalid"},
	DriverInstall:     {"rules", "install", "foreignHook", "remoteRecorded", "status", "code", "hooks", "remedyContains", "snippetContains", "label", "installed"},
	DriverRuleBody:    {"method", "body", "rulesFile", "unavailable", "status", "code"},
	DriverPush:        {"rules", "rulesFile", "config", "flags", "unscoped", "noRemote", "sessions", "before", "villagePublic", "failShare", "stallShare", "failTranscriptRead", "failShareRead", "selected", "historical", "deleteRule", "reads", "outputOmits", "villageDecides", "privateBeforeAgain", "again", "errorContains", "outputContains", "publishes", "license", "audience", "others", "ownerUpdates", "attemptContains"},
	DriverHook:        {"uploadExits"},
	DriverVillageAuto: {"failTranscriptRead", "rules", "foreignHook", "handAdded", "forkUpstream", "hooksPath", "publications", "unrecorded", "signedOut", "errorContains", "outputContains", "output", "rule", "ruleIds", "installed", "binding"},
}

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

// Publication is one session published, in order, and the collectives its
// transcript is shared with afterwards, by alias.
type Publication struct {
	Shared map[string]schema.VillageShareStatus `yaml:"shared"`
}

// Case is one named case. A driver reads only the fields fieldsOf names.
type Case struct {
	Name   string `yaml:"name"`
	Driver Driver `yaml:"driver"`
	// Rules is the content of hooks.yaml. Collectives are named by alias and
	// resolved to their identifiers when the fixture loads. "{world}" in a
	// match is the directory that holds the world's repositories, and
	// "{remote}" the recorded repository's remote in bare form.
	Rules []autopublish.Rule `yaml:"rules"`
	// RulesFile, when set, is hooks.yaml verbatim instead of Rules.
	RulesFile string `yaml:"rulesFile"`
	// Repository is what the matcher reads; "~/" in its root is the home
	// directory.
	Repository struct {
		Root   string `yaml:"root"`
		Remote string `yaml:"remote"`
		Origin string `yaml:"origin"`
	} `yaml:"repository"`
	// Install names the repository the install call names: recorded or
	// unrecorded. RemoteRecorded gives the recorded repository an origin.
	Install        string `yaml:"install"`
	RemoteRecorded bool   `yaml:"remoteRecorded"`
	// ForeignHook is the content of a pre-push hook Peasant did not write,
	// placed in the target repository first.
	ForeignHook string `yaml:"foreignHook"`
	// Method and Body are a request the settings routes must refuse; Body is
	// sent verbatim to the rule route, or to the install route for POST.
	Unavailable bool   `yaml:"unavailable"`
	Method      string `yaml:"method"`
	Body        string `yaml:"body"`
	// Config is the push configuration.
	Config struct {
		Visibility schema.Visibility `yaml:"visibility"`
		License    schema.License    `yaml:"license"`
	} `yaml:"config"`
	// Flags are extra flags of the push; Unscoped drops --repository.
	Flags    []string `yaml:"flags"`
	Unscoped bool     `yaml:"unscoped"`
	// NoRemote gives the world's repositories no remote. Sessions records one
	// more session each: "clone" in another clone of the same remote, "gone"
	// in a clone removed after the session was recorded, and "linked" in a
	// linked worktree inside the repository.
	NoRemote bool     `yaml:"noRemote"`
	Sessions []string `yaml:"sessions"`
	// Before publishes the sessions once, with no rule, before the case's
	// rules are written; VillagePublic then makes the transcript public on
	// Village, as its owner could.
	Before        bool `yaml:"before"`
	VillagePublic bool `yaml:"villagePublic"`
	// FailShare names collectives the Village double refuses to share with;
	// StallShare makes every share answer only after the push's budget.
	FailShare          []string `yaml:"failShare"`
	StallShare         bool     `yaml:"stallShare"`
	FailTranscriptRead bool     `yaml:"failTranscriptRead"`
	FailShareRead      bool     `yaml:"failShareRead"`
	DeleteRule         string   `yaml:"deleteRule"`
	Historical         int      `yaml:"historical"`
	Selected           bool     `yaml:"selected"`
	// VillageDecides sets shares on Village after the push, as a collective
	// owner or the developer would, and PrivateBeforeAgain makes the
	// transcript private there; Again then pushes once more with these flags.
	VillageDecides     map[string]schema.VillageShareStatus `yaml:"villageDecides"`
	PrivateBeforeAgain bool                                 `yaml:"privateBeforeAgain"`
	Again              []string                             `yaml:"again"`
	// UploadExits are the exit statuses of the failing uploads the hook runs.
	UploadExits []int `yaml:"uploadExits"`
	// Publications are published in order before the command runs.
	Publications []Publication `yaml:"publications"`
	// HandAdded makes the foreign hook carry the by-hand upload section;
	// ForkUpstream makes the checked-out branch track a fork; HooksPath sets
	// core.hooksPath in the repository.
	HandAdded    bool   `yaml:"handAdded"`
	ForkUpstream bool   `yaml:"forkUpstream"`
	HooksPath    string `yaml:"hooksPath"`
	// Unrecorded runs the command in a repository with no recorded session;
	// SignedOut runs it with no stored credential.
	Unrecorded bool   `yaml:"unrecorded"`
	SignedOut  bool   `yaml:"signedOut"`
	Expect     Expect `yaml:"expect"`
}

// Expect is what a case asserts. A driver asserts only the fields fieldsOf
// names.
type Expect struct {
	// Applying are the identifiers of the rules that publish the repository,
	// Paused the ones that cover it and name no event, and Collectives the
	// aliases of the collectives it is shared with, in order.
	Applying    []string `yaml:"applying"`
	Paused      []string `yaml:"paused"`
	Collectives []string `yaml:"collectives"`
	// Invalid is part of the reason the rule or file is refused.
	Invalid string `yaml:"invalid"`
	// Status and Code are the route's answer; Hooks each event's status;
	// RemedyContains and SnippetContains parts of a hook's remedy; Label the
	// repository's remote label.
	Status          int                                                      `yaml:"status"`
	Code            string                                                   `yaml:"code"`
	Hooks           map[schema.AutoPublishEvent]schema.AutoPublishHookStatus `yaml:"hooks"`
	RemedyContains  []string                                                 `yaml:"remedyContains"`
	SnippetContains []string                                                 `yaml:"snippetContains"`
	Label           string                                                   `yaml:"label"`
	// Installed names the repositories that hold a Peasant-managed pre-push
	// hook afterwards.
	Installed []string `yaml:"installed"`
	// ErrorContains are parts of the command's error, none meaning it
	// succeeds; OutputContains parts of its standard error.
	ErrorContains  []string `yaml:"errorContains"`
	OutputContains []string `yaml:"outputContains"`
	OutputOmits    []string `yaml:"outputOmits"`
	Reads          *int     `yaml:"reads"`
	// Publishes counts the uploads Village received; License is the license
	// every upload after Before carried; Audience the collectives that can
	// read the recorded session's transcript afterwards (approved or pending),
	// and Others those of each extra session's, by alias; OwnerUpdates the owner visibility updates;
	// AttemptContains part of the session's latest failed attempt.
	Publishes       *int                                            `yaml:"publishes"`
	License         schema.License                                  `yaml:"license"`
	Audience        map[string]schema.VillageShareStatus            `yaml:"audience"`
	Others          map[string]map[string]schema.VillageShareStatus `yaml:"others"`
	OwnerUpdates    int                                             `yaml:"ownerUpdates"`
	AttemptContains string                                          `yaml:"attemptContains"`
	// Output is the exact standard output.
	Output string `yaml:"output"`
	// Rule is the one rule hooks.yaml holds afterwards; "{remote}" in its
	// match is the world's remote in bare form.
	Rule *struct {
		Kind        schema.AutoPublishRuleKind `yaml:"kind"`
		Match       string                     `yaml:"match"`
		Events      []schema.AutoPublishEvent  `yaml:"events"`
		Collectives []string                   `yaml:"collectives"`
	} `yaml:"rule"`
	// RuleIDs are the identifiers hooks.yaml holds afterwards, in order.
	RuleIDs []string `yaml:"ruleIds"`
	// Binding says the installed hook binds the command's config, data, and
	// state directories.
	Binding bool `yaml:"binding"`
}

// Load reads and validates the fixture from the module root.
func Load(t *testing.T) Fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testutil.ModuleRoot(t), Path))
	if err != nil {
		t.Fatalf("%s: %v", Path, err)
	}
	var fixture Fixture
	if err := testutil.DecodeNamedFixtureYAML(raw, &fixture); err != nil {
		t.Fatalf("%s: %v", Path, err)
	}
	for i := range fixture.Cases {
		c := &fixture.Cases[i]
		allowed, ok := fieldsOf[c.Driver]
		if !ok {
			t.Fatalf("%s: case %q names driver %q; use one of %v", Path, c.Name, c.Driver, AllDrivers)
		}
		for _, field := range setFields(*c) {
			if !slices.Contains(allowed, field) {
				t.Fatalf("%s: case %q sets %q, which the %s driver does not read; move it or drop it", Path, c.Name, field, c.Driver)
			}
		}
		for j := range c.Rules {
			for k, alias := range c.Rules[j].Collectives {
				c.Rules[j].Collectives[k] = fixture.collective(t, c.Name, string(alias)).ID
			}
		}
		if (len(c.VillageDecides) > 0 || c.PrivateBeforeAgain || c.DeleteRule != "") && c.Again == nil {
			t.Fatalf("%s: case %q changes Village between pushes but names no second push (again)", Path, c.Name)
		}
		if c.VillagePublic && !c.Before {
			t.Fatalf("%s: case %q makes a transcript public but publishes none before (before)", Path, c.Name)
		}
		for _, session := range c.Sessions {
			if !slices.Contains([]string{"clone", "gone", "linked", "unrelated", "gone-subfolder"}, session) {
				t.Fatalf("%s: case %q names session %q; use clone, gone, or linked", Path, c.Name, session)
			}
		}
		aliases := append(append(mapKeys(c.VillageDecides), mapKeys(c.Expect.Audience)...), c.FailShare...)
		for _, audience := range c.Expect.Others {
			aliases = append(aliases, mapKeys(audience)...)
		}
		aliases = append(aliases, c.Expect.Collectives...)
		for _, publication := range c.Publications {
			aliases = append(aliases, mapKeys(publication.Shared)...)
		}
		if c.Expect.Rule != nil {
			aliases = append(aliases, c.Expect.Rule.Collectives...)
		}
		for _, alias := range aliases {
			fixture.collective(t, c.Name, alias)
		}
	}
	return fixture
}

func (f Fixture) collective(t testing.TB, name, alias string) Collective {
	t.Helper()
	collective, ok := f.Collectives[alias]
	if !ok {
		t.Fatalf("%s: case %q names collective %q, which the fixture does not declare", Path, name, alias)
	}
	return collective
}

// setFields lists the YAML names of the case and expectation fields a case
// sets, besides its name and driver.
func setFields(c Case) []string {
	var set []string
	collect := func(v reflect.Value) {
		for i := 0; i < v.NumField(); i++ {
			name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("yaml"), ",")
			if name == "name" || name == "driver" || name == "expect" || v.Field(i).IsZero() {
				continue
			}
			set = append(set, name)
		}
	}
	collect(reflect.ValueOf(c))
	collect(reflect.ValueOf(c.Expect))
	return set
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

// Alias returns the alias of the collective id, or "" for an unknown one.
func (f Fixture) Alias(id schema.VillageUUID) string {
	for alias, collective := range f.Collectives {
		if collective.ID == id {
			return alias
		}
	}
	return ""
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
