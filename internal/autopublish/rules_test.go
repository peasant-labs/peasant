package autopublish_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/autopublish/autopublishtest"
	"github.com/peasant-labs/schema"
)

// TestRulesApplyToRepositories runs the matcher cases of
// testdata/auto-publish-rules.yaml: which rules publish a push of the
// repository, and the collectives it is shared with.
func TestRulesApplyToRepositories(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
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
				home, err := os.UserHomeDir()
				if err != nil {
					t.Fatal(err)
				}
				root = filepath.Join(home, rest)
			}
			repo := autopublish.Repository{Root: root, Remote: c.Repository.Remote}
			applying := autopublish.Applying(c.Rules, repo)
			if got := autopublish.IDs(applying); !slices.Equal(got, c.Expect.Applying) && (len(got) > 0 || len(c.Expect.Applying) > 0) {
				t.Errorf("applying rules = %v, want %v", got, c.Expect.Applying)
			}
			if got, want := autopublish.Collectives(applying), fixture.IDs(c.Expect.Collectives); !slices.Equal(got, want) && (len(got) > 0 || len(want) > 0) {
				t.Errorf("collectives = %v, want %v", got, want)
			}
		})
	}
}

// TestInvalidRulesAreRefused runs the validate cases: each rule is refused
// alone, and a rules file that holds it is refused when read, so no push
// publishes under a rule that cannot be read.
func TestInvalidRulesAreRefused(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverValidate) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if len(c.Rules) != 1 || c.Expect.Invalid == "" {
				t.Fatal("a validate case holds one rule and names why it is refused")
			}
			err := c.Rules[0].Validate()
			if err == nil || !strings.Contains(err.Error(), c.Expect.Invalid) {
				t.Fatalf("Validate() = %v, want an error saying %q", err, c.Expect.Invalid)
			}
			raw, err := yaml.Marshal(struct {
				Version     int                `yaml:"version"`
				AutoPublish []autopublish.Rule `yaml:"autoPublish"`
			}{Version: 1, AutoPublish: c.Rules})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), autopublish.FileName)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := autopublish.Load(path); err == nil || !strings.Contains(err.Error(), c.Expect.Invalid) {
				t.Fatalf("Load() of a file holding the rule = %v, want an error saying %q", err, c.Expect.Invalid)
			}
		})
	}
}

// TestUpdateKeepsTheFileReadable pins the rules file: a missing file holds no
// rule, a saved rule reads back as saved, a change that would leave an invalid
// rule writes nothing, and a file of another version is refused.
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
	duplicate := rule
	if err := autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) { return append(rules, duplicate), nil }); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("an update that repeats an identifier = %v; want a refusal", err)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(saved) {
		t.Fatal("a refused update changed the rules file")
	}

	if err := os.WriteFile(path, []byte("version: 2\nautoPublish: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := autopublish.Load(path); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("Load(version 2) = %v; want a refusal naming the version", err)
	}
}
