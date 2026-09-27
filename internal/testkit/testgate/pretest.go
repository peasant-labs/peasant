package testgate

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The five pre-test steps of `make check` are inside the budget, so the gate
// must be able to price them as wall/user/system rather than as one aggregate.
// The commands live in testdata/pretest_steps.yaml so the list is a fixture, not
// an inline table, and each row names the Makefile target it mirrors plus a
// literal fragment of that target's recipe. A content anchor, not a line range:
// a line reference drifts silently the moment the Makefile above it changes.

//go:embed testdata/pretest_steps.yaml
var preTestFixtureYAML []byte

// PreTestCommand is one `make check` pre-test step and the command it runs.
// Target names the Makefile target whose recipe runs the step; Fragment is the
// distinguishing literal that must appear in that target's recipe.
type PreTestCommand struct {
	Step     PreTestStep `yaml:"step"`
	Program  string      `yaml:"program"`
	Args     []string    `yaml:"args"`
	Target   string      `yaml:"target"`
	Fragment string      `yaml:"fragment"`
}

// PreTestFixture is the embedded pre-test command list.
type PreTestFixture struct {
	RequiredNames []string         `yaml:"required_names"`
	Steps         []PreTestCommand `yaml:"steps"`
}

// LoadPreTestFixture decodes exactly one strict YAML document and validates the
// required-name manifest.
func LoadPreTestFixture(data []byte) (PreTestFixture, error) {
	var file PreTestFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return PreTestFixture{}, fmt.Errorf("decode pre-test fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return PreTestFixture{}, fmt.Errorf("pre-test fixture must contain exactly one document, got %v", err)
	}
	if err := validatePreTestFixture(file); err != nil {
		return PreTestFixture{}, err
	}
	return file, nil
}

// validatePreTestFixture enforces that the fixture covers exactly the frozen
// PreTestSteps, in execution order, with a program and no empty step name.
func validatePreTestFixture(file PreTestFixture) error {
	if len(file.RequiredNames) == 0 {
		return errors.New("pre-test fixture declares no required_names manifest")
	}
	if len(file.Steps) != len(PreTestSteps) {
		return fmt.Errorf("pre-test fixture has %d steps, want the %d frozen steps", len(file.Steps), len(PreTestSteps))
	}
	seen := map[PreTestStep]bool{}
	for i, cmd := range file.Steps {
		if cmd.Step != PreTestSteps[i] {
			return fmt.Errorf("pre-test fixture step %d is %q, want %q in execution order", i, cmd.Step, PreTestSteps[i])
		}
		if seen[cmd.Step] {
			return fmt.Errorf("pre-test fixture repeats step %q", cmd.Step)
		}
		seen[cmd.Step] = true
		if cmd.Program == "" {
			return fmt.Errorf("pre-test fixture step %q has no program", cmd.Step)
		}
		if cmd.Target == "" {
			return fmt.Errorf("pre-test fixture step %q names no Makefile target", cmd.Step)
		}
		if cmd.Fragment == "" {
			return fmt.Errorf("pre-test fixture step %q has no recipe fragment", cmd.Step)
		}
	}
	for _, want := range file.RequiredNames {
		found := false
		for _, cmd := range file.Steps {
			if string(cmd.Step) == want {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("required pre-test step %q is missing from the fixture", want)
		}
	}
	return nil
}

// PreTestCommands returns the embedded pre-test command list, validated against
// the frozen step set.
func PreTestCommands() ([]PreTestCommand, error) {
	file, err := LoadPreTestFixture(preTestFixtureYAML)
	if err != nil {
		return nil, err
	}
	return file.Steps, nil
}

// checkTarget is the Makefile target whose execution runs the pre-test steps:
// its direct prerequisites first, then its own recipe.
const checkTarget = "check"

// makefileTarget is one parsed Makefile target: its direct prerequisites and the
// physical lines of its recipe, with the leading tab stripped.
type makefileTarget struct {
	Prereqs []string
	Recipe  []string
}

// makefileTargetRE matches a target definition line (`name: prerequisites`).
// Variable assignments (`NAME := ...`) also match the shape, so a candidate
// whose value field begins with `=` is rejected in parseMakefileTargets.
var makefileTargetRE = regexp.MustCompile(`^([A-Za-z0-9_./-]+)[ \t]*:[ \t]*(.*)$`)

// parseMakefileTargets reads the target/prerequisite/recipe structure needed to
// resolve the pre-test anchors. It is a small reader for this repository's own
// Makefile, not a general Make parser.
func parseMakefileTargets(data []byte) map[string]makefileTarget {
	targets := map[string]makefileTarget{}
	current := ""
	for _, raw := range strings.Split(string(data), "\n") {
		if current != "" && strings.HasPrefix(raw, "\t") {
			t := targets[current]
			t.Recipe = append(t.Recipe, strings.TrimPrefix(raw, "\t"))
			targets[current] = t
			continue
		}
		current = ""
		m := makefileTargetRE.FindStringSubmatch(raw)
		if m == nil || strings.HasPrefix(m[2], "=") {
			continue
		}
		targets[m[1]] = makefileTarget{Prereqs: makefilePrereqs(m[2])}
		current = m[1]
	}
	return targets
}

// makefilePrereqs splits a target line's prerequisite field, dropping an inline
// recipe after `;` and the order-only `|` separator.
func makefilePrereqs(field string) []string {
	if i := strings.IndexByte(field, ';'); i >= 0 {
		field = field[:i]
	}
	var out []string
	for _, p := range strings.Fields(field) {
		if p == "|" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func recipeContains(recipe []string, fragment string) bool {
	for _, line := range recipe {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

// validatePreTestSources checks the fixture against the Makefile it mirrors.
// Each step's Fragment must appear in its declared Target's recipe, and the
// steps must appear, fragment by fragment, in the same order the `check` target
// executes them: its direct prerequisites in order, then its own recipe. A
// dropped, renamed, or reordered recipe line therefore fails loudly instead of
// the fixture measuring a step the Makefile no longer runs.
func validatePreTestSources(file PreTestFixture, makefile []byte) error {
	targets := parseMakefileTargets(makefile)
	check, ok := targets[checkTarget]
	if !ok {
		return fmt.Errorf("Makefile has no %q target", checkTarget)
	}

	type execLine struct {
		target string
		line   string
	}
	var exec []execLine
	for _, pre := range check.Prereqs {
		t, ok := targets[pre]
		if !ok {
			return fmt.Errorf("%s prerequisite %q is not a Makefile target", checkTarget, pre)
		}
		for _, line := range t.Recipe {
			exec = append(exec, execLine{target: pre, line: line})
		}
	}
	for _, line := range check.Recipe {
		exec = append(exec, execLine{target: checkTarget, line: line})
	}

	pos := -1
	for _, cmd := range file.Steps {
		t, ok := targets[cmd.Target]
		if !ok {
			return fmt.Errorf("step %q names Makefile target %q, which does not exist", cmd.Step, cmd.Target)
		}
		if !recipeContains(t.Recipe, cmd.Fragment) {
			return fmt.Errorf("step %q: fragment %q does not appear in Makefile target %q", cmd.Step, cmd.Fragment, cmd.Target)
		}
		found := -1
		for i := pos + 1; i < len(exec); i++ {
			if exec[i].target == cmd.Target && strings.Contains(exec[i].line, cmd.Fragment) {
				found = i
				break
			}
		}
		if found < 0 {
			return fmt.Errorf("step %q (target %q) is out of order or missing from the %s target's execution", cmd.Step, cmd.Target, checkTarget)
		}
		pos = found
	}
	return nil
}

// StepMeasurement is one measured pre-test step. Every step runs alone, so wall,
// user, and system are exact. Failed reports a non-zero exit so a reader can
// tell a measured success from a measured failure; LogPath holds the captured
// combined output.
type StepMeasurement struct {
	Step     PreTestStep
	Wall     time.Duration
	User     time.Duration
	System   time.Duration
	ExitCode int
	Failed   bool
	LogPath  string
}

// PreTestDocument is the machine-readable output of `profile -pretest`. The
// gate's `run` reads it from the same output directory, so one `make check` can
// carry per-step pre-test rows alongside the test-pass rows.
type PreTestDocument struct {
	SchemaVersion int               `json:"schema_version"`
	Calibration   Calibration       `json:"calibration"`
	Steps         []StepMeasurement `json:"steps"`
	Rows          []ClassRow        `json:"rows"`
}

// WritePreTestDocument writes the pre-test measurement as indented JSON.
func WritePreTestDocument(path string, doc PreTestDocument) error {
	if doc.Steps == nil {
		doc.Steps = []StepMeasurement{}
	}
	if doc.Rows == nil {
		doc.Rows = []ClassRow{}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ReadPreTestDocument reads a pre-test measurement. found is false when the file
// is absent.
func ReadPreTestDocument(path string) (PreTestDocument, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PreTestDocument{}, false, nil
		}
		return PreTestDocument{}, false, fmt.Errorf("read pre-test doc %s: %w", path, err)
	}
	var doc PreTestDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return PreTestDocument{}, false, fmt.Errorf("decode pre-test doc %s: %w", path, err)
	}
	return doc, true, nil
}

// StepRecords converts measured pre-test steps into report records so the
// gate's report can carry them beside the test-pass records.
func StepRecords(steps []StepMeasurement) []ReportRecord {
	out := make([]ReportRecord, 0, len(steps))
	for _, s := range steps {
		out = append(out, ReportRecord{
			Unit:     string(s.Step),
			Class:    string(PreTestClass(s.Step)),
			Pass:     "pre",
			WallMS:   s.Wall.Milliseconds(),
			UserMS:   s.User.Milliseconds(),
			SystemMS: s.System.Milliseconds(),
		})
	}
	return out
}

// RunPreTestSteps executes each command serially from root, captures combined
// output to logDir/<step>.log, and wraps it with the child CPU accumulator. It
// runs every step even after a failure so the table is complete, and returns one
// measurement per command. A non-nil error is a harness failure (a step could
// not be started), not a step failure.
func RunPreTestSteps(ctx context.Context, root, logDir string, cmds []PreTestCommand, env []string) ([]StepMeasurement, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create pre-test log dir: %w", err)
	}
	out := make([]StepMeasurement, 0, len(cmds))
	for _, c := range cmds {
		logPath := filepath.Join(logDir, string(c.Step)+".log")
		logFile, err := os.Create(logPath)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", logPath, err)
		}
		cmd := exec.CommandContext(ctx, c.Program, c.Args...)
		cmd.Dir = root
		cmd.Env = env
		cmd.Stdout = logFile
		cmd.Stderr = logFile

		beforeUser, beforeSys, _ := readChildUsage()
		start := time.Now()
		runErr := cmd.Run()
		wall := time.Since(start)
		afterUser, afterSys, _ := readChildUsage()
		if err := logFile.Close(); err != nil {
			return nil, fmt.Errorf("close %s: %w", logPath, err)
		}

		m := StepMeasurement{
			Step:    c.Step,
			Wall:    wall,
			User:    afterUser - beforeUser,
			System:  afterSys - beforeSys,
			LogPath: logPath,
		}
		if runErr != nil {
			m.Failed = true
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				m.ExitCode = exitErr.ExitCode()
			} else {
				m.ExitCode = -1
			}
		}
		out = append(out, m)
	}
	return out, nil
}
