package kickstart_test

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	expectedProgressRows   = 6
	expectedCompletionRows = 3
)

type progressStageFixture struct {
	Stage   ingest.Stage `yaml:"stage"`
	Started bool         `yaml:"started"`
	Done    int          `yaml:"done"`
	Total   int          `yaml:"total"`
	Ended   bool         `yaml:"ended"`
	HasErr  bool         `yaml:"hasErr"`
}

type progressObservationFixture struct {
	AdvanceSeconds int                    `yaml:"advanceSeconds"`
	Stages         []progressStageFixture `yaml:"stages"`
}

type progressFixture struct {
	Name         string                       `yaml:"name"`
	Observations []progressObservationFixture `yaml:"observations"`
	WantContains []string                     `yaml:"wantContains"`
	WantMissing  []string                     `yaml:"wantMissing"`
}

type completionFixture struct {
	Name                    string   `yaml:"name"`
	AttemptErrors           []string `yaml:"attemptErrors"`
	RetentionError          string   `yaml:"retentionError"`
	MutateConfigBeforeRetry bool     `yaml:"mutateConfigBeforeRetry"`
	UseRealProgress         bool     `yaml:"useRealProgress"`
	WantFailureContains     []string `yaml:"wantFailureContains"`
	WantRetryContains       []string `yaml:"wantRetryContains"`
	WantRetryMissing        []string `yaml:"wantRetryMissing"`
	WantContains            []string `yaml:"wantContains"`
	WantMissing             []string `yaml:"wantMissing"`
	WantIngestCalls         int      `yaml:"wantIngestCalls"`
	WantRetentionCalls      int      `yaml:"wantRetentionCalls"`
	WantProgressResets      int      `yaml:"wantProgressResets"`
}

type progressCompletionDocument struct {
	ExpectedProgressCount   int                 `yaml:"expectedProgressCount"`
	Progress                []progressFixture   `yaml:"progress"`
	ExpectedCompletionCount int                 `yaml:"expectedCompletionCount"`
	Completion              []completionFixture `yaml:"completion"`
}

//go:embed testdata/guided/progress_completion.yaml
var progressCompletionData []byte

func loadProgressCompletionDocument(t *testing.T) progressCompletionDocument {
	t.Helper()
	var document progressCompletionDocument
	decodeSingleKnownFieldsDocument(t, "testdata/guided/progress_completion.yaml", progressCompletionData, &document)
	if document.ExpectedProgressCount != expectedProgressRows || len(document.Progress) != expectedProgressRows {
		t.Fatalf("progress rows: declared=%d actual=%d required=%d",
			document.ExpectedProgressCount, len(document.Progress), expectedProgressRows)
	}
	if document.ExpectedCompletionCount != expectedCompletionRows || len(document.Completion) != expectedCompletionRows {
		t.Fatalf("completion rows: declared=%d actual=%d required=%d",
			document.ExpectedCompletionCount, len(document.Completion), expectedCompletionRows)
	}
	validStages := make(map[ingest.Stage]bool, len(ingest.StageOrder))
	for _, stage := range ingest.StageOrder {
		validStages[stage] = true
	}
	progressNames := map[string]bool{}
	for _, row := range document.Progress {
		if strings.TrimSpace(row.Name) == "" || progressNames[row.Name] || len(row.Observations) < 2 || len(row.WantContains) == 0 {
			t.Fatalf("progress row is incomplete or duplicated: %#v", row)
		}
		progressNames[row.Name] = true
		for observationIndex, observation := range row.Observations {
			if observation.AdvanceSeconds < 0 || len(observation.Stages) == 0 {
				t.Fatalf("progress row %q observation %d is empty or moves time backwards", row.Name, observationIndex)
			}
			seenStages := map[ingest.Stage]bool{}
			for _, stage := range observation.Stages {
				if !validStages[stage.Stage] || seenStages[stage.Stage] || !stage.Started || stage.Done < 0 || stage.Total < 0 {
					t.Fatalf("progress row %q has invalid or duplicate stage observation: %#v", row.Name, stage)
				}
				seenStages[stage.Stage] = true
			}
		}
	}
	completionNames := map[string]bool{}
	for _, row := range document.Completion {
		if strings.TrimSpace(row.Name) == "" || completionNames[row.Name] || len(row.AttemptErrors) == 0 || len(row.WantContains) == 0 ||
			row.WantIngestCalls != len(row.AttemptErrors) || row.WantRetentionCalls != 1 || row.WantProgressResets != len(row.AttemptErrors) {
			t.Fatalf("completion row is incomplete, duplicated, or internally inconsistent: %#v", row)
		}
		completionNames[row.Name] = true
		if len(row.AttemptErrors) > 1 && (len(row.WantFailureContains) == 0 || len(row.WantRetryContains) == 0 ||
			len(row.WantRetryMissing) == 0 || !row.MutateConfigBeforeRetry || !row.UseRealProgress) {
			t.Fatalf("retry completion row %q does not prove failure truth, volatile reset, and no recommit", row.Name)
		}
	}
	return document
}

type fixtureClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixtureClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixtureClock) Advance(seconds int) {
	c.mu.Lock()
	c.now = c.now.Add(time.Duration(seconds) * time.Second)
	c.mu.Unlock()
}

type fixtureProgressSource struct {
	mu       sync.Mutex
	snapshot map[ingest.Stage]ingest.StageProgress
	resets   int
}

type realProgressSource struct {
	*ingest.ProgressState
	mu     sync.Mutex
	resets int
}

func newRealProgressSource() *realProgressSource {
	return &realProgressSource{ProgressState: ingest.NewProgressState()}
}

func (s *realProgressSource) Reset() {
	s.ProgressState.Reset()
	s.mu.Lock()
	s.resets++
	s.mu.Unlock()
}

func (s *realProgressSource) Resets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resets
}

type resetCountingProgress interface {
	kickstart.ProgressSource
	Resets() int
}

func (s *fixtureProgressSource) Snapshot() map[ingest.Stage]ingest.StageProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyOfSnapshot := make(map[ingest.Stage]ingest.StageProgress, len(s.snapshot))
	for stage, progress := range s.snapshot {
		copyOfSnapshot[stage] = progress
	}
	return copyOfSnapshot
}

func (s *fixtureProgressSource) Reset() {
	s.mu.Lock()
	s.resets++
	s.snapshot = map[ingest.Stage]ingest.StageProgress{}
	s.mu.Unlock()
}

func (s *fixtureProgressSource) Set(stages []progressStageFixture) {
	s.mu.Lock()
	s.snapshot = make(map[ingest.Stage]ingest.StageProgress, len(stages))
	for _, stage := range stages {
		s.snapshot[stage.Stage] = ingest.StageProgress{
			Started: stage.Started,
			Done:    stage.Done,
			Total:   stage.Total,
			Ended:   stage.Ended,
			HasErr:  stage.HasErr,
		}
	}
	s.mu.Unlock()
}

func (s *fixtureProgressSource) Resets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resets
}

func newProgressProgram(
	t *testing.T,
	progress kickstart.ProgressSource,
	clock *fixtureClock,
	ingestRun kickstart.IngestFunc,
	retention kickstart.RetentionWriter,
	tickCapture *func(time.Time) tea.Msg,
) (kickstart.Program, string, tea.Cmd) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	loaded := config.BaseConfig()
	if err := config.SaveAtomic(path, loaded); err != nil {
		t.Fatalf("seed progress config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open progress draft: %v", err)
	}
	draft.Working().Push.License = config.LicenseCCBY
	if err := kickstart.SeedRetentionInitial(draft, 90); err != nil {
		t.Fatalf("seed progress retention: %v", err)
	}
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:                 theme.New(theme.ModeDark),
		Draft:                 draft,
		Source:                scannerfix.NewFixtureTreeSource("standard"),
		ClaudeSessionsPresent: true,
		AlreadyConnected:      true,
		Retention:             retention,
		Ingest:                ingestRun,
		Progress:              progress,
		Clock:                 clock,
		Tick: func(_ time.Duration, callback func(time.Time) tea.Msg) tea.Cmd {
			*tickCapture = callback
			return func() tea.Msg { return callback(clock.Now()) }
		},
	})
	program.SetSize(180, 50)
	program = drainProgram(program, program.Init())
	program, command := advanceToCommit(program)
	if !program.Committed() || program.Phase() != kickstart.PhaseIngest {
		t.Fatalf("progress Program committed/phase=%t/%s, want true/ingest", program.Committed(), program.Phase())
	}
	return program, path, command
}

func TestProgramProgressShowsHonestElapsedAndQualifiedEstimate(t *testing.T) {
	for _, row := range loadProgressCompletionDocument(t).Progress {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			clock := &fixtureClock{now: time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)}
			progress := &fixtureProgressSource{}
			var tick func(time.Time) tea.Msg
			program, _, _ := newProgressProgram(t, progress, clock, func(context.Context) (*ftue.IngestResult, error) {
				return &ftue.IngestResult{New: 1}, nil
			}, nil, &tick)
			if tick == nil {
				t.Fatal("starting local ingest did not schedule the injected progress tick")
			}
			for _, observation := range row.Observations {
				clock.Advance(observation.AdvanceSeconds)
				progress.Set(observation.Stages)
				program, _ = program.Update(tick(clock.Now()))
			}
			view := strings.ToLower(stripRender(program.View()))
			for _, want := range row.WantContains {
				if !strings.Contains(view, strings.ToLower(want)) {
					t.Errorf("progress view does not contain %q:\n%s", want, view)
				}
			}
			for _, missing := range row.WantMissing {
				if strings.Contains(view, strings.ToLower(missing)) {
					t.Errorf("progress view contains unsupported estimate %q:\n%s", missing, view)
				}
			}
		})
	}
}

func runAttemptCommandsOnce(program kickstart.Program, command tea.Cmd) (kickstart.Program, bool) {
	if command == nil {
		return program, false
	}
	message := command()
	children, isBatch := message.(tea.BatchMsg)
	if !isBatch {
		children = tea.BatchMsg{func() tea.Msg { return message }}
	}
	quitEarly := false
	for _, child := range children {
		if child == nil {
			continue
		}
		message := child()
		if message == nil {
			continue
		}
		var next tea.Cmd
		program, next = program.Update(message)
		if program.Phase() == kickstart.PhaseDone && next != nil {
			_, quitEarly = next().(tea.QuitMsg)
		}
	}
	return program, quitEarly
}

func TestProgramCompletionPersistsAndRetryRunsOnlyLocalImport(t *testing.T) {
	for _, row := range loadProgressCompletionDocument(t).Completion {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			clock := &fixtureClock{now: time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)}
			var progress resetCountingProgress = &fixtureProgressSource{}
			var realProgress *realProgressSource
			if row.UseRealProgress {
				realProgress = newRealProgressSource()
				progress = realProgress
			}
			retentionCalls := 0
			ingestCalls := 0
			ingestRun := func(context.Context) (*ftue.IngestResult, error) {
				attempt := ingestCalls
				ingestCalls++
				if row.AttemptErrors[attempt] != "" {
					return nil, errors.New(row.AttemptErrors[attempt])
				}
				return &ftue.IngestResult{New: 1}, nil
			}
			var tick func(time.Time) tea.Msg
			program, path, command := newProgressProgram(t, progress, clock, ingestRun,
				kickstart.RetentionWriterFunc(func(int) error {
					retentionCalls++
					if row.RetentionError != "" {
						return errors.New(row.RetentionError)
					}
					return nil
				}), &tick)
			program, quitEarly := runAttemptCommandsOnce(program, command)
			if quitEarly {
				t.Fatal("local ingest completion requested tea.Quit before explicit user exit")
			}

			if len(row.AttemptErrors) > 1 {
				failureView := strings.ToLower(stripRender(program.View()))
				for _, want := range row.WantFailureContains {
					if !strings.Contains(failureView, strings.ToLower(want)) {
						t.Errorf("failure completion does not contain %q:\n%s", want, failureView)
					}
				}
				if row.MutateConfigBeforeRetry {
					file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						t.Fatalf("open committed config for retry sentinel: %v", err)
					}
					if _, err := file.WriteString("# external retry sentinel\n"); err != nil {
						_ = file.Close()
						t.Fatalf("write retry sentinel: %v", err)
					}
					if err := file.Close(); err != nil {
						t.Fatalf("close retry sentinel: %v", err)
					}
				}
				if realProgress != nil {
					realProgress.Update(ingest.ProgressEvent{
						Kind: ingest.KindEnd, Stage: ingest.StageReport,
						Done: 1, Total: 2, Err: errors.New("stale prior-attempt progress"),
					})
				}
				mutated := mustReadFile(t, path)
				program, command = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				retryView := strings.ToLower(stripRender(program.View()))
				for _, want := range row.WantRetryContains {
					if !strings.Contains(retryView, strings.ToLower(want)) {
						t.Errorf("retry start does not contain %q:\n%s", want, retryView)
					}
					for _, missing := range row.WantRetryMissing {
						if strings.Contains(retryView, strings.ToLower(missing)) {
							t.Errorf("retry retained stale progress %q:\n%s", missing, retryView)
						}
					}
				}
				program, quitEarly = runAttemptCommandsOnce(program, command)
				if quitEarly {
					t.Fatal("successful retry requested tea.Quit before explicit user exit")
				}
				if after := mustReadFile(t, path); string(after) != string(mutated) {
					t.Fatal("local-import retry recommitted or rewrote configuration")
				}
			}

			view := strings.ToLower(stripRender(program.View()))
			for _, want := range row.WantContains {
				if !strings.Contains(view, strings.ToLower(want)) {
					t.Errorf("completion does not contain %q:\n%s", want, view)
				}
			}
			for _, missing := range row.WantMissing {
				if strings.Contains(view, strings.ToLower(missing)) {
					t.Errorf("completion fabricates forbidden value %q:\n%s", missing, view)
				}
			}
			if ingestCalls != row.WantIngestCalls || retentionCalls != row.WantRetentionCalls || progress.Resets() != row.WantProgressResets {
				t.Errorf("ingest/retention/progress-reset calls=%d/%d/%d, want %d/%d/%d",
					ingestCalls, retentionCalls, progress.Resets(), row.WantIngestCalls, row.WantRetentionCalls, row.WantProgressResets)
			}
			if program.Phase() != kickstart.PhaseDone {
				t.Fatalf("completion phase=%s, want done", program.Phase())
			}
			beforeExit := program.View()
			program, _ = program.Update(tea.WindowSizeMsg{Width: 181, Height: 51})
			if program.View() == "" || beforeExit == "" {
				t.Fatal("completion disappeared before explicit exit")
			}
			_, exitCommand := program.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
			if exitCommand == nil {
				t.Fatal("explicit completion exit produced no quit command")
			}
			if _, ok := exitCommand().(tea.QuitMsg); !ok {
				t.Fatal("explicit completion exit did not request tea.Quit")
			}
		})
	}
}

func TestProgramRetryIgnoresPriorAttemptTimerChains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	loaded := config.BaseConfig()
	if err := config.SaveAtomic(path, loaded); err != nil {
		t.Fatalf("seed retry generation config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open retry generation draft: %v", err)
	}
	clock := &fixtureClock{now: time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)}
	var callbacks []func(time.Time) tea.Msg
	ingestCalls := 0
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:            theme.New(theme.ModeDark),
		Draft:            draft,
		Source:           scannerfix.NewFixtureTreeSource("standard"),
		AlreadyConnected: true,
		Clock:            clock,
		Progress:         ingest.NewProgressState(),
		Tick: func(_ time.Duration, callback func(time.Time) tea.Msg) tea.Cmd {
			callbacks = append(callbacks, callback)
			return func() tea.Msg { return callback(clock.Now()) }
		},
		Ingest: func(context.Context) (*ftue.IngestResult, error) {
			ingestCalls++
			if ingestCalls == 1 {
				return nil, errors.New("first attempt failed")
			}
			return &ftue.IngestResult{New: 1}, nil
		},
	})
	program.SetSize(120, 30)
	program = drainProgram(program, program.Init())
	program, firstBatch := advanceToCommit(program)
	firstChildren := unwrapBatch(firstBatch)
	if len(firstChildren) != 3 || len(callbacks) != 1 {
		t.Fatalf("first attempt children/callbacks=%d/%d, want 3/1", len(firstChildren), len(callbacks))
	}
	oldProgressMessage := callbacks[0](clock.Now())
	oldSpinnerMessage := firstChildren[2]()
	program, _ = program.Update(firstChildren[0]())
	if program.Phase() != kickstart.PhaseDone || program.IngestErr() == nil {
		t.Fatalf("first attempt phase/error=%s/%v, want failed completion", program.Phase(), program.IngestErr())
	}

	program, retryBatch := program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	retryChildren := unwrapBatch(retryBatch)
	if program.Phase() != kickstart.PhaseIngest || len(retryChildren) != 3 || len(callbacks) != 2 {
		t.Fatalf("retry phase/children/callbacks=%s/%d/%d, want ingest/3/2",
			program.Phase(), len(retryChildren), len(callbacks))
	}

	var staleCommand tea.Cmd
	program, staleCommand = program.Update(oldProgressMessage)
	if staleCommand != nil || len(callbacks) != 2 {
		t.Fatalf("stale progress tick rescheduled a chain: command=%v callbacks=%d", staleCommand != nil, len(callbacks))
	}
	program, staleCommand = program.Update(oldSpinnerMessage)
	if staleCommand != nil {
		t.Fatal("stale spinner tick rescheduled an animation chain")
	}

	currentProgressMessage := callbacks[1](clock.Now())
	program, currentCommand := program.Update(currentProgressMessage)
	if currentCommand == nil || len(callbacks) != 3 {
		t.Fatalf("current progress tick command/callbacks=%v/%d, want one continuing chain", currentCommand != nil, len(callbacks))
	}
	_, currentSpinnerCommand := program.Update(retryChildren[2]())
	if currentSpinnerCommand == nil {
		t.Fatal("current retry spinner tick did not continue its animation chain")
	}
}
