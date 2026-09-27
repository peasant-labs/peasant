package testgate

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/teststream"
	"gopkg.in/yaml.v3"
)

// screenCasesYAML freezes the rule 3 scoping cases: liveness is required only
// for registered packages the plan contains.
//
//go:embed testdata/screen_cases.yaml
var screenCasesYAML []byte

type screenPlanPkg struct {
	ImportPath string   `yaml:"import_path"`
	Dir        string   `yaml:"dir"`
	Tests      []string `yaml:"tests"`
	Registered bool     `yaml:"registered"`
}

type screenEvent struct {
	Package string `yaml:"package"`
	Test    string `yaml:"test"`
	Skipped bool   `yaml:"skipped"`
}

type screenCase struct {
	Name      string            `yaml:"name"`
	Race      bool              `yaml:"race"`
	Plan      []screenPlanPkg   `yaml:"plan"`
	Registry  Registry          `yaml:"registry"`
	Events    []screenEvent     `yaml:"events"`
	WantRules map[string]string `yaml:"want_rules"`
	WantFail  bool              `yaml:"want_fail"`
}

type screenCaseFile struct {
	RequiredNames []string     `yaml:"required_names"`
	Cases         []screenCase `yaml:"cases"`
}

func loadScreenCases(t *testing.T) screenCaseFile {
	t.Helper()
	var file screenCaseFile
	decoder := yaml.NewDecoder(bytes.NewReader(screenCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		t.Fatalf("decode screen cases: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("screen cases must contain exactly one YAML document, got %v", err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("screen cases declare no required_names manifest")
	}
	present := map[string]bool{}
	for _, c := range file.Cases {
		if c.Name == "" || present[c.Name] {
			t.Fatalf("screen cases have an empty or duplicate case %q", c.Name)
		}
		present[c.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !present[want] {
			t.Fatalf("required screen case %q is missing from the fixture", want)
		}
	}
	return file
}

func TestScreen_Rule3IsScopedToThePlan(t *testing.T) {
	file := loadScreenCases(t)
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			plan := &Plan{}
			for _, p := range tc.Plan {
				plan.Packages = append(plan.Packages, PackagePlan{
					ImportPath: p.ImportPath,
					Dir:        p.Dir,
					Tests:      p.Tests,
					Registered: p.Registered,
				})
			}
			streams := map[PassMode]map[string][]teststream.Record{}
			pass := ModeNoRace
			if tc.Race {
				pass = ModeRace
			}
			streams[pass] = map[string][]teststream.Record{}
			for _, e := range tc.Events {
				streams[pass][e.Package] = append(streams[pass][e.Package], teststream.Record{
					Package: e.Package,
					Test:    e.Test,
					Skipped: e.Skipped,
				})
			}
			findings := Screen(ScreenInput{Plan: plan, Registry: tc.Registry, Race: tc.Race, Streams: streams})

			got := map[string]string{}
			for _, f := range findings {
				got[f.Rule] = f.Severity.String()
			}
			if len(got) != len(tc.WantRules) {
				t.Fatalf("finding rules = %v, want %v (findings: %s)", got, tc.WantRules, renderFindings(findings))
			}
			for rule, want := range tc.WantRules {
				if got[rule] != want {
					t.Fatalf("rule %q severity = %q, want %q (findings: %s)", rule, got[rule], want, renderFindings(findings))
				}
			}
			if Fails(findings) != tc.WantFail {
				t.Fatalf("Fails = %v, want %v (findings: %s)", Fails(findings), tc.WantFail, renderFindings(findings))
			}
		})
	}
}

func renderFindings(findings []Finding) string {
	var b bytes.Buffer
	for _, f := range findings {
		fmt.Fprintf(&b, "[%s] %s: %s; ", f.Severity, f.Rule, f.What)
	}
	return b.String()
}
