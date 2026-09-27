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
	"time"

	"gopkg.in/yaml.v3"
)

// The five pre-test steps of `make check` are inside the budget, so the gate
// must be able to price them as wall/user/system rather than as one aggregate.
// The commands live in testdata/pretest_steps.yaml so the list is a fixture, not
// an inline table, and each row names the Makefile line it mirrors.

//go:embed testdata/pretest_steps.yaml
var preTestFixtureYAML []byte

// PreTestCommand is one `make check` pre-test step and the command it runs.
type PreTestCommand struct {
	Step     PreTestStep `yaml:"step"`
	Program  string      `yaml:"program"`
	Args     []string    `yaml:"args"`
	Makefile string      `yaml:"makefile"`
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
		if cmd.Makefile == "" {
			return fmt.Errorf("pre-test fixture step %q names no makefile source", cmd.Step)
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
