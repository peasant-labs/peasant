package e2e

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/infra-reaper.yaml
var infraReaperFixtureYAML []byte

type infraReaperFixtureDocument struct {
	RequiredNames []string                 `yaml:"requiredNames"`
	Cases         []infraReaperFixtureCase `yaml:"cases"`
}

type infraReaperFixtureCase struct {
	Name                   string `yaml:"name"`
	Kind                   string `yaml:"kind"`
	RawName                string `yaml:"rawName"`
	PID                    int    `yaml:"pid"`
	Age                    string `yaml:"age"`
	Status                 string `yaml:"status"`
	Alive                  bool   `yaml:"alive"`
	Selected               bool   `yaml:"selected"`
	SurroundWithWhitespace bool   `yaml:"surroundWithWhitespace"`
}

func loadInfraReaperFixtures() (infraReaperFixtureDocument, error) {
	var document infraReaperFixtureDocument
	if err := testutil.DecodeNamedFixtureYAML(infraReaperFixtureYAML, &document); err != nil {
		return document, fmt.Errorf("decode strict infra reaper fixture: %w", err)
	}
	for _, fixture := range document.Cases {
		if strings.TrimSpace(fixture.Status) == "" {
			return document, fmt.Errorf("infra reaper fixture %q has blank status", fixture.Name)
		}
		if fixture.RawName == "" && (fixture.Kind == "" || fixture.PID <= 0 || fixture.Age == "") {
			return document, fmt.Errorf("infra reaper fixture %q must provide rawName or generated-name fields", fixture.Name)
		}
		if fixture.RawName != "" && (fixture.Kind != "" || fixture.PID != 0 || fixture.Age != "") {
			return document, fmt.Errorf("infra reaper fixture %q mixes rawName and generated-name fields", fixture.Name)
		}
		if fixture.Age != "" {
			if _, err := time.ParseDuration(fixture.Age); err != nil {
				return document, fmt.Errorf("infra reaper fixture %q has invalid age: %w", fixture.Name, err)
			}
		}
	}
	return document, nil
}

func TestInfraReaperTargetsOnlyAbandonedPeasantE2EContainers(t *testing.T) {
	document, err := loadInfraReaperFixtures()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10_000_000, 0)
	var lines []string
	var wantNames []string
	alivePIDs := make(map[int]bool)
	for _, fixture := range document.Cases {
		name := fixture.RawName
		if name == "" {
			age, err := time.ParseDuration(fixture.Age)
			if err != nil {
				t.Fatalf("parse fixture %q age: %v", fixture.Name, err)
			}
			name = uniqueNameAt(fixture.Kind, fixture.PID, now.Add(-age))
			alivePIDs[fixture.PID] = fixture.Alive
		}
		selectedName := name
		if fixture.SurroundWithWhitespace {
			name = " " + name + " "
		}
		lines = append(lines, name+"\t"+fixture.Status)
		if fixture.Selected {
			wantNames = append(wantNames, selectedName)
		}
	}

	names := reapableE2EInfraNames(strings.Join(lines, "\n"), now, staleE2ETTL, func(pid int) bool {
		return alivePIDs[pid]
	})
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("reapable infra names = %v, want %v", names, wantNames)
	}

	wantArgs := append([]string{"rm", "-fv"}, wantNames...)
	args := podmanReapE2EInfraArgs(names)
	if strings.Join(args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("reap args = %v, want %v", args, wantArgs)
	}
	if args := podmanReapE2EInfraArgs(nil); args != nil {
		t.Fatalf("empty reap args = %v, want nil", args)
	}
}
