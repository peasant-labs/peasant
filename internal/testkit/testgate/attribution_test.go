package testgate

import (
	"os"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
)

type attributionCase struct {
	Name            string   `yaml:"name"`
	Race            bool     `yaml:"race"`
	WantNoRaceBasis string   `yaml:"wantNoRaceBasis"`
	WantRaceBasis   string   `yaml:"wantRaceBasis"`
	OverlapPackages []string `yaml:"overlapPackages"`
}

type attributionFile struct {
	RequiredNames []string          `yaml:"requiredNames"`
	Cases         []attributionCase `yaml:"cases"`
}

func loadAttributionModes(t *testing.T) attributionFile {
	t.Helper()
	data, err := os.ReadFile("testdata/attribution_modes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var file attributionFile
	if err := testutil.DecodeNamedFixtureYAML(data, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestAttributionPassesMatchModes(t *testing.T) {
	file := loadAttributionModes(t)
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			passes := AttributionPasses(tc.Race, time.Second, 2*time.Second, time.Second, time.Second, time.Second, time.Second, 1, 2)
			if tc.Race {
				if len(passes) != 2 || passes[0].Pass != ModeRace || passes[0].Serialized || passes[1].Pass != ModeNoRace || !passes[1].Serialized {
					t.Fatalf("passes = %+v, want a concurrent race pass and a serialized no-race pass", passes)
				}
			} else if len(passes) != 1 || passes[0].Pass != ModeNoRace || passes[0].Serialized {
				t.Fatalf("passes = %+v, want one concurrent no-race pass", passes)
			}
			records := []Record{{Unit: "pkg/a", Class: ClassRace, Pass: ModeNoRace, Wall: time.Second}}
			if tc.Race {
				records = []Record{
					{Unit: "pkg/race", Class: ClassRace, Pass: ModeRace, Wall: time.Second},
					{Unit: "pkg/part", Class: ClassSingleThreadedBytes, Pass: ModeNoRace, Wall: time.Second},
				}
			}
			got := map[string]string{}
			for _, row := range BuildClassTable(passes, records) {
				got[row.Class] = row.Basis
			}
			if tc.WantRaceBasis != "" && got[string(ClassRace)] != tc.WantRaceBasis {
				t.Fatalf("race basis = %q, want %q (%v)", got[string(ClassRace)], tc.WantRaceBasis, got)
			}
			noRaceClass := string(ClassRace)
			if tc.Race {
				noRaceClass = string(ClassSingleThreadedBytes)
			}
			if got[noRaceClass] != tc.WantNoRaceBasis {
				t.Fatalf("no-race class %s basis = %q, want %q (%v)", noRaceClass, got[noRaceClass], tc.WantNoRaceBasis, got)
			}
		})
	}
}
