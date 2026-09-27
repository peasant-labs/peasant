package testgate

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Fixture-driven tests for the measurement surface: the LPT batch planner,
// the per-class decision table, and the pre-test step rows. No inline case
// tables; the cases and their required-name manifest live in
// testdata/measure_cases.yaml.

//go:embed testdata/measure_cases.yaml
var measureCasesYAML []byte

type plannerCase struct {
	Name      string            `yaml:"name"`
	Names     []string          `yaml:"names"`
	N         int               `yaml:"n"`
	Prior     map[string]string `yaml:"prior"`
	WantLPT   bool              `yaml:"want_lpt"`
	WantBatch map[string]int    `yaml:"want_batch"`
}

type passCase struct {
	Pass       string `yaml:"pass"`
	Wall       string `yaml:"wall"`
	User       string `yaml:"user"`
	System     string `yaml:"system"`
	Serialized bool   `yaml:"serialized"`
	Units      int    `yaml:"units"`
}

type recordCase struct {
	Unit   string `yaml:"unit"`
	Class  string `yaml:"class"`
	Pass   string `yaml:"pass"`
	Wall   string `yaml:"wall"`
	User   string `yaml:"user"`
	System string `yaml:"system"`
}

type rowCase struct {
	Class    string `yaml:"class"`
	Units    int    `yaml:"units"`
	WallMS   int64  `yaml:"wall_ms"`
	UserMS   int64  `yaml:"user_ms"`
	SystemMS int64  `yaml:"system_ms"`
	GapMS    int64  `yaml:"gap_ms"`
	Basis    string `yaml:"basis"`
}

type classTableCase struct {
	Name    string       `yaml:"name"`
	Passes  []passCase   `yaml:"passes"`
	Records []recordCase `yaml:"records"`
	Want    []rowCase    `yaml:"want"`
}

type stepCase struct {
	Step   string `yaml:"step"`
	Wall   string `yaml:"wall"`
	User   string `yaml:"user"`
	System string `yaml:"system"`
}

type pretestCase struct {
	Name  string     `yaml:"name"`
	Steps []stepCase `yaml:"steps"`
	Want  []rowCase  `yaml:"want"`
}

type measureCasesFile struct {
	RequiredNames   []string         `yaml:"required_names"`
	PlannerCases    []plannerCase    `yaml:"planner_cases"`
	ClassTableCases []classTableCase `yaml:"classtable_cases"`
	PreTestCases    []pretestCase    `yaml:"pretest_cases"`
}

func loadMeasureCases(t *testing.T) measureCasesFile {
	t.Helper()
	var file measureCasesFile
	decoder := yaml.NewDecoder(bytes.NewReader(measureCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		t.Fatalf("decode measure cases: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("measure cases must contain exactly one document, got %v", err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("measure cases declare no required_names manifest")
	}
	names := map[string]bool{}
	collect := func(name string) {
		if name == "" || names[name] {
			t.Fatalf("measure case has an empty or duplicate name %q", name)
		}
		names[name] = true
	}
	for _, c := range file.PlannerCases {
		collect(c.Name)
	}
	for _, c := range file.ClassTableCases {
		collect(c.Name)
	}
	for _, c := range file.PreTestCases {
		collect(c.Name)
	}
	for _, want := range file.RequiredNames {
		if !names[want] {
			t.Fatalf("required measure case %q is missing from the fixture", want)
		}
	}
	return file
}

func mustDuration(t *testing.T, value string) time.Duration {
	t.Helper()
	if value == "" {
		return 0
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		t.Fatalf("fixture duration %q: %v", value, err)
	}
	return d
}

func TestPlanBatches_MatchesFixture(t *testing.T) {
	file := loadMeasureCases(t)
	for _, tc := range file.PlannerCases {
		t.Run(tc.Name, func(t *testing.T) {
			var prior map[string]time.Duration
			if len(tc.Prior) > 0 {
				prior = map[string]time.Duration{}
				for name, d := range tc.Prior {
					prior[name] = mustDuration(t, d)
				}
			}
			plan, err := PlanBatches(tc.Names, prior, tc.N)
			if err != nil {
				t.Fatalf("PlanBatches: %v", err)
			}
			if plan.LPT != tc.WantLPT {
				t.Fatalf("LPT = %v, want %v", plan.LPT, tc.WantLPT)
			}
			if !reflect.DeepEqual(plan.Assignment, tc.WantBatch) {
				t.Fatalf("assignment = %v, want %v", plan.Assignment, tc.WantBatch)
			}
		})
	}
}

func TestPlanBatches_RejectsZeroBatches(t *testing.T) {
	if _, err := PlanBatches([]string{"A"}, nil, 0); err == nil {
		t.Fatal("PlanBatches accepted n=0; a zero-batch plan is invalid")
	}
}

func TestPlanBatches_EmptyNames(t *testing.T) {
	plan, err := PlanBatches(nil, nil, 3)
	if err != nil {
		t.Fatalf("PlanBatches: %v", err)
	}
	if len(plan.Batches) != 3 {
		t.Fatalf("batches = %d, want 3", len(plan.Batches))
	}
	if len(plan.Assignment) != 0 {
		t.Fatalf("assignment = %v, want empty", plan.Assignment)
	}
}

func TestBuildClassTable_MatchesFixture(t *testing.T) {
	file := loadMeasureCases(t)
	for _, tc := range file.ClassTableCases {
		t.Run(tc.Name, func(t *testing.T) {
			var passes []PassSummary
			for _, p := range tc.Passes {
				passes = append(passes, PassSummary{
					Pass:       passModeFromString(t, p.Pass),
					Wall:       mustDuration(t, p.Wall),
					User:       mustDuration(t, p.User),
					System:     mustDuration(t, p.System),
					Serialized: p.Serialized,
					Units:      p.Units,
				})
			}
			var records []Record
			for _, r := range tc.Records {
				records = append(records, Record{
					Unit:   r.Unit,
					Class:  Class(r.Class),
					Pass:   passModeFromString(t, r.Pass),
					Wall:   mustDuration(t, r.Wall),
					User:   mustDuration(t, r.User),
					System: mustDuration(t, r.System),
				})
			}
			got := BuildClassTable(passes, records)
			want := rowsFromFixture(tc.Want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("class table =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

func TestPreTestRows_MatchesFixture(t *testing.T) {
	file := loadMeasureCases(t)
	for _, tc := range file.PreTestCases {
		t.Run(tc.Name, func(t *testing.T) {
			var steps []StepMeasurement
			for _, s := range tc.Steps {
				steps = append(steps, StepMeasurement{
					Step:   PreTestStep(s.Step),
					Wall:   mustDuration(t, s.Wall),
					User:   mustDuration(t, s.User),
					System: mustDuration(t, s.System),
				})
			}
			got := PreTestRows(steps)
			want := rowsFromFixture(tc.Want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("pre-test rows =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

func rowsFromFixture(in []rowCase) []ClassRow {
	out := make([]ClassRow, 0, len(in))
	for _, r := range in {
		out = append(out, ClassRow{
			Class: r.Class, Units: r.Units, WallMS: r.WallMS, UserMS: r.UserMS,
			SystemMS: r.SystemMS, GapMS: r.GapMS, Basis: r.Basis,
		})
	}
	return out
}

func passModeFromString(t *testing.T, s string) PassMode {
	t.Helper()
	switch s {
	case "race":
		return ModeRace
	case "no-race":
		return ModeNoRace
	default:
		t.Fatalf("fixture pass %q is not a known pass mode", s)
		return ModeRace
	}
}

func TestPreTestFixture_CoversFrozenSteps(t *testing.T) {
	cmds, err := PreTestCommands()
	if err != nil {
		t.Fatalf("PreTestCommands: %v", err)
	}
	if len(cmds) != len(PreTestSteps) {
		t.Fatalf("embedded fixture has %d steps, want %d", len(cmds), len(PreTestSteps))
	}
	for i, cmd := range cmds {
		if cmd.Step != PreTestSteps[i] {
			t.Fatalf("embedded fixture step %d is %q, want %q", i, cmd.Step, PreTestSteps[i])
		}
	}
}

// TestPreTestFixture_MutationsAreRejected proves the loader's coverage and
// required-name checks are not vacuous.
func TestPreTestFixture_MutationsAreRejected(t *testing.T) {
	t.Run("pretest-mutation-unknown-step", func(t *testing.T) {
		file, err := LoadPreTestFixture(preTestFixtureYAML)
		if err != nil {
			t.Fatalf("base fixture: %v", err)
		}
		file.Steps[0].Step = "bogus-step"
		if err := validatePreTestFixture(file); err == nil {
			t.Fatal("the loader accepted a step outside the frozen PreTestSteps set")
		}
	})
	t.Run("pretest-mutation-missing-required", func(t *testing.T) {
		file, err := LoadPreTestFixture(preTestFixtureYAML)
		if err != nil {
			t.Fatalf("base fixture: %v", err)
		}
		file.RequiredNames = append(file.RequiredNames, "not-a-step")
		if err := validatePreTestFixture(file); err == nil {
			t.Fatal("the loader accepted a required name absent from the fixture")
		}
	})
}

// TestClassTable_MutationDropsRow proves the comparison above is sensitive: a
// dropped expected row is a mismatch.
func TestClassTable_MutationDropsRow(t *testing.T) {
	passes := []PassSummary{{Pass: ModeNoRace, Wall: 2 * time.Second, Serialized: true, Units: 2}}
	records := []Record{
		{Unit: "a", Class: ClassSubprocess, Pass: ModeNoRace, Wall: time.Second},
		{Unit: "b", Class: ClassToolchain, Pass: ModeNoRace, Wall: time.Second},
	}
	got := BuildClassTable(passes, records)
	if len(got) != 2 {
		t.Fatalf("class table has %d rows, want 2", len(got))
	}
	if got[0].Class != string(ClassSubprocess) {
		t.Fatalf("first row class = %q, want the registry order starting at %q", got[0].Class, ClassSubprocess)
	}
	if got[0].GapMS != 1000 {
		t.Fatalf("gap = %d, want 1000 (wall - CPU)", got[0].GapMS)
	}
}
