package kickstart_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/stored_choice_reset.yaml
var storedChoiceResetYAML []byte

func TestStoredChoiceReset(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name     string `yaml:"name"`
			Key      string `yaml:"key"`
			Baseline string `yaml:"baseline"`
			Working  string `yaml:"working"`
			Want     string `yaml:"want"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(storedChoiceResetYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("duplicate case %s", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			parse := func(raw string) config.Config {
				var cfg config.Config
				if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
					t.Fatal(err)
				}
				return cfg
			}
			baseline, working := parse(row.Baseline), parse(row.Working)
			found := false
			kickstart.BuildRegistry(kickstart.Options{}).FieldWrites(func() config.Config { return baseline }, func() config.Config { return working }, func(field settings.Field, cfg *config.Config) {
				if field.Key() != row.Key {
					return
				}
				found = true
				var got string
				switch row.Key {
				case kickstart.FieldPrivacy:
					got = string(cfg.Redaction.Level)
				case kickstart.FieldVisibility:
					got = string(cfg.Push.Visibility)
				default:
					t.Fatalf("unknown field %s", row.Key)
				}
				if got != row.Want {
					t.Errorf("restored %s = %q, want %q", row.Key, got, row.Want)
				}
			})
			if !found {
				t.Fatalf("registry has no field %s", row.Key)
			}
		})
	}
	if err := testutil.RequireFixtureNames("stored_choice_reset.yaml", "case", fixture.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"empty-privacy-restored", "retained-privacy-restored", "empty-visibility-restored", "retained-visibility-restored"} {
		if !seen[name] {
			t.Fatalf("missing required case %s", name)
		}
	}
}
