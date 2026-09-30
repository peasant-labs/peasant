package autopublish_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/autopublish/autopublishtest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestRulesDecideRepositories runs the matcher cases of
// testdata/auto-publish-rules.yaml: which rules publish the repository, which
// cover it paused, and the collectives it is shared with.
func TestRulesDecideRepositories(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.For(t, autopublishtest.DriverMatcher) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			for _, rule := range c.Rules {
				if err := rule.Validate(); err != nil {
					t.Fatalf("a matcher case holds valid rules: %v", err)
				}
			}
			root := c.Repository.Root
			if rest, ok := strings.CutPrefix(root, "~/"); ok {
				root = filepath.Join(home, rest)
			}
			decision := autopublish.Decide(c.Rules, autopublish.Repository{Root: root, Remote: c.Repository.Remote, Origin: c.Repository.Origin})
			if !sameList(decision.Rules, c.Expect.Applying) {
				t.Errorf("applying rules = %v, want %v", decision.Rules, c.Expect.Applying)
			}
			if !sameList(decision.Paused, c.Expect.Paused) {
				t.Errorf("paused rules = %v, want %v", decision.Paused, c.Expect.Paused)
			}
			if want := fixture.IDs(c.Expect.Collectives); !sameList(decision.Collectives, want) {
				t.Errorf("collectives = %v, want %v", decision.Collectives, want)
			}
			if decision.Covered() != (len(c.Expect.Applying)+len(c.Expect.Paused) > 0) {
				t.Errorf("Covered() = %v; a repository is covered exactly when a rule applies or is paused", decision.Covered())
			}
		})
	}
}

// TestInvalidRulesAreRefused runs the validate cases: a rule is refused alone
// and in a rules file, and a rules file is refused when it is read, so no push
// publishes under a rule that cannot be read.
func TestInvalidRulesAreRefused(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverValidate) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if (len(c.Rules) == 1) == (c.RulesFile != "") || c.Expect.Invalid == "" {
				t.Fatal("a validate case holds one rule or a rules file, and names why it is refused")
			}
			raw := []byte(c.RulesFile)
			if len(c.Rules) == 1 {
				if err := c.Rules[0].Validate(); err == nil || !strings.Contains(err.Error(), c.Expect.Invalid) {
					t.Fatalf("Validate() = %v, want an error saying %q", err, c.Expect.Invalid)
				}
				var err error
				raw, err = yaml.Marshal(map[string]any{"version": 1, "autoPublish": c.Rules})
				if err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), autopublish.FileName)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := autopublish.Load(path); err == nil || !strings.Contains(err.Error(), c.Expect.Invalid) {
				t.Fatalf("Load() = %v, want an error saying %q", err, c.Expect.Invalid)
			}
		})
	}
}

// TestUpdateKeepsTheFileReadable pins the rules file: a missing file holds no
// rule, a saved rule reads back as saved in a file private to its owner, and a
// change that would leave an invalid rule writes nothing.
func TestUpdateKeepsTheFileReadable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "peasant", autopublish.FileName)
	if rules, err := autopublish.Load(path); err != nil || len(rules) != 0 {
		t.Fatalf("Load(missing) = %v, %v; want no rule", rules, err)
	}
	rule := autopublish.Rule{ID: "acme", Kind: schema.AutoPublishRuleRemote, Match: "github.com/acme/*", Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{"11111111-1111-4111-8111-111111111111"}}
	if err := autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) { return append(rules, rule), nil }); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := autopublish.Load(path)
	if err != nil || len(rules) != 1 || rules[0].ID != rule.ID || rules[0].Match != rule.Match || !slices.Equal(rules[0].Collectives, rule.Collectives) {
		t.Fatalf("Load(saved) = %+v, %v; want the saved rule", rules, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the rules file mode = %v, %v; it is private to its owner", info.Mode().Perm(), err)
	}
	broken := rule
	broken.Kind = "branch"
	if err := autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) { return append(rules, broken), nil }); err == nil {
		t.Fatal("an update that adds an invalid rule was saved")
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(saved) {
		t.Fatal("a refused update changed the rules file")
	}
}

// TestFolderMatchNamesExactlyItsFolder pins the glob a rule for one folder
// uses: every glob character of the path is escaped, so the rule covers that
// folder and no sibling.
func TestFolderMatchNamesExactlyItsFolder(t *testing.T) {
	t.Parallel()
	root := "/home/dev/work/[old] a*b?"
	rule := autopublish.Rule{ID: "one", Kind: schema.AutoPublishRuleFolder, Match: autopublish.FolderMatch(root), Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{}}
	if err := rule.Validate(); err != nil {
		t.Fatalf("the rule for %q is invalid: %v", root, err)
	}
	if !rule.Covers(autopublish.Repository{Root: root}) {
		t.Errorf("%q does not cover %q", rule.Match, root)
	}
	if rule.Covers(autopublish.Repository{Root: "/home/dev/work/o a-bc"}) {
		t.Errorf("%q covers a sibling folder", rule.Match)
	}
}

// driverCall finds a driver test's call for its fixture cases.
var driverCall = regexp.MustCompile(`\.For\(t, autopublishtest\.(Driver\w+)\)`)

// TestEveryDriverHasOneTest guards the fixture against a deleted, renamed, or
// duplicated driver test: its cases would then run nowhere, or twice, while
// the required-name manifest stayed green.
func TestEveryDriverHasOneTest(t *testing.T) {
	t.Parallel()
	root := testutil.ModuleRoot(t)
	calls := map[string]int{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, match := range driverCall.FindAllStringSubmatch(string(raw), -1) {
				calls[match[1]]++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	names := map[autopublishtest.Driver]string{
		autopublishtest.DriverMatcher: "DriverMatcher", autopublishtest.DriverValidate: "DriverValidate",
		autopublishtest.DriverInstall: "DriverInstall", autopublishtest.DriverRuleBody: "DriverRuleBody",
		autopublishtest.DriverPush: "DriverPush", autopublishtest.DriverHook: "DriverHook",
		autopublishtest.DriverVillageAuto: "DriverVillageAuto",
	}
	for _, driver := range autopublishtest.AllDrivers {
		name, ok := names[driver]
		if !ok {
			t.Fatalf("driver %q has no constant name here; add it", driver)
		}
		if calls[name] != 1 {
			t.Errorf("autopublishtest.%s is run by %d tests; exactly one test runs each driver's cases", name, calls[name])
		}
	}
}

func sameList[T comparable](got, want []T) bool {
	return len(got) == len(want) && (len(got) == 0 || slices.Equal(got, want))
}
