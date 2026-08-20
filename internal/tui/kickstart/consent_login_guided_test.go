package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/schema"
)

const (
	expectedConsentRows = 3
	expectedLoginRows   = 3
)

type loginOutcome string

const (
	loginSuccess loginOutcome = "success"
	loginFailure loginOutcome = "failure"
	loginDecline loginOutcome = "decline"
)

func (o loginOutcome) valid() bool {
	return o == loginSuccess || o == loginFailure || o == loginDecline
}

type consentFixture struct {
	Name                  string               `yaml:"name"`
	SelectionMode         config.SelectionMode `yaml:"selectionMode"`
	AutoIngestNewBranches bool                 `yaml:"autoIngestNewBranches"`
	License               config.License       `yaml:"license"`
	Visibility            config.Visibility    `yaml:"visibility"`
	Connected             bool                 `yaml:"connected"`
	ClaudeSessionsPresent bool                 `yaml:"claudeSessionsPresent"`
	RetentionDays         int                  `yaml:"retentionDays"`
	WantContains          []string             `yaml:"wantContains"`
	WantMissing           []string             `yaml:"wantMissing"`
}

type loginFixture struct {
	Name            string         `yaml:"name"`
	Outcome         loginOutcome   `yaml:"outcome"`
	Username        string         `yaml:"username"`
	Error           string         `yaml:"error"`
	BufferedLicense config.License `yaml:"bufferedLicense"`
	WantContains    []string       `yaml:"wantContains"`
}

type consentLoginDocument struct {
	ExpectedConsentCount int              `yaml:"expectedConsentCount"`
	Consent              []consentFixture `yaml:"consent"`
	ExpectedLoginCount   int              `yaml:"expectedLoginCount"`
	Login                []loginFixture   `yaml:"login"`
}

//go:embed testdata/guided/consent_login.yaml
var consentLoginData []byte

func loadConsentLoginDocument(t *testing.T) consentLoginDocument {
	t.Helper()
	var document consentLoginDocument
	decodeSingleKnownFieldsDocument(t, "testdata/guided/consent_login.yaml", consentLoginData, &document)
	if document.ExpectedConsentCount != expectedConsentRows || len(document.Consent) != expectedConsentRows {
		t.Fatalf("consent rows: declared=%d actual=%d required=%d",
			document.ExpectedConsentCount, len(document.Consent), expectedConsentRows)
	}
	if document.ExpectedLoginCount != expectedLoginRows || len(document.Login) != expectedLoginRows {
		t.Fatalf("login rows: declared=%d actual=%d required=%d",
			document.ExpectedLoginCount, len(document.Login), expectedLoginRows)
	}
	consentNames := map[string]bool{}
	for _, row := range document.Consent {
		if strings.TrimSpace(row.Name) == "" || consentNames[row.Name] || !row.SelectionMode.IsValid() || len(row.WantContains) == 0 {
			t.Fatalf("consent row is incomplete or duplicated: %#v", row)
		}
		consentNames[row.Name] = true
		visibilityKnown := false
		for _, visibility := range schema.AllVisibilities {
			visibilityKnown = visibilityKnown || visibility == row.Visibility
		}
		if !visibilityKnown {
			t.Fatalf("consent row %q has unsupported visibility %q", row.Name, row.Visibility)
		}
		if row.ClaudeSessionsPresent != (row.RetentionDays > 0) {
			t.Fatalf("consent row %q must pair visible Claude retention with a positive day count", row.Name)
		}
	}
	loginNames := map[string]bool{}
	for _, row := range document.Login {
		if strings.TrimSpace(row.Name) == "" || loginNames[row.Name] || !row.Outcome.valid() || len(row.WantContains) == 0 {
			t.Fatalf("login row is incomplete or duplicated: %#v", row)
		}
		loginNames[row.Name] = true
		if row.Outcome == loginSuccess && strings.TrimSpace(row.Username) == "" {
			t.Fatalf("successful login row %q has no username", row.Name)
		}
		if row.Outcome == loginFailure && strings.TrimSpace(row.Error) == "" {
			t.Fatalf("failed login row %q has no error", row.Name)
		}
	}
	return document
}

type countingTreeSource struct {
	mu    sync.Mutex
	inner kit.TreeSource
	loads int
}

func (s *countingTreeSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	s.mu.Lock()
	s.loads++
	s.mu.Unlock()
	return s.inner.Load(ctx)
}

func (s *countingTreeSource) Loads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func newConsentProgram(t *testing.T, row consentFixture) (kickstart.Program, *settings.Draft, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	loaded := config.BaseConfig()
	loaded.Selection.Mode = row.SelectionMode
	loaded.Selection.AutoIngestNewBranches = row.AutoIngestNewBranches
	if row.SelectionMode == config.SelectionModeSelected {
		loaded.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
			"claude-code": {Sessions: []string{"fixture-selected-session"}},
		}
	}
	loaded.Push.License = row.License
	loaded.Push.Visibility = row.Visibility
	if err := config.SaveAtomic(path, loaded); err != nil {
		t.Fatalf("seed consent config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open consent draft: %v", err)
	}
	if row.RetentionDays > 0 {
		if err := kickstart.SeedRetentionInitial(draft, row.RetentionDays); err != nil {
			t.Fatalf("seed consent retention: %v", err)
		}
	}
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:                 theme.New(theme.ModeDark),
		Draft:                 draft,
		Source:                scannerfix.NewFixtureTreeSource("standard"),
		AlreadyConnected:      row.Connected,
		ClaudeSessionsPresent: row.ClaudeSessionsPresent,
	})
	program.SetSize(180, 50)
	return program, draft, path
}

func runFlowInitOnce(t *testing.T, program kickstart.Program, command tea.Cmd) kickstart.Program {
	t.Helper()
	if command == nil {
		return program
	}
	message := command()
	commands, isBatch := message.(tea.BatchMsg)
	if !isBatch {
		updated, _ := program.Update(message)
		return updated
	}
	for _, child := range commands {
		if child == nil {
			continue
		}
		message := child()
		if message == nil {
			continue
		}
		program, _ = program.Update(message)
	}
	return program
}

func enterDisconnectedFlow(t *testing.T, program kickstart.Program) kickstart.Program {
	t.Helper()
	var command tea.Cmd
	program, command = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if program.Phase() != kickstart.PhaseFlow {
		t.Fatalf("declining initial connection entered phase %s, want flow", program.Phase())
	}
	return runFlowInitOnce(t, program, command)
}

func advanceToConsent(t *testing.T, program kickstart.Program) kickstart.Program {
	t.Helper()
	for step := 0; step < 24; step++ {
		if program.Phase() == kickstart.PhaseVisibility {
			var command tea.Cmd
			program, command = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			program = runFlowInitOnce(t, program, command)
			continue
		}
		if strings.Contains(stripRender(program.View()), "review your changes") {
			return program
		}
		program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	t.Fatalf("guided Program did not reach final consent; phase=%s view:\n%s", program.Phase(), stripRender(program.View()))
	return program
}

func TestFinalConsentUsesVisibleDraftValuesAndPromisesNoPublication(t *testing.T) {
	for _, row := range loadConsentLoginDocument(t).Consent {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			program, _, path := newConsentProgram(t, row)
			before := mustReadFile(t, path)
			if row.Connected {
				program = runFlowInitOnce(t, program, program.Init())
			} else {
				program = enterDisconnectedFlow(t, program)
			}
			program = advanceToConsent(t, program)
			view := stripRender(program.View())
			for _, want := range row.WantContains {
				if !strings.Contains(view, want) {
					t.Errorf("consent does not contain Draft-derived %q:\n%s", want, view)
				}
			}
			for _, missing := range row.WantMissing {
				if strings.Contains(view, missing) {
					t.Errorf("consent contains hidden effect %q:\n%s", missing, view)
				}
			}
			if program.Committed() {
				t.Fatal("rendering final consent committed before explicit confirmation")
			}
			if after := mustReadFile(t, path); !bytes.Equal(before, after) {
				t.Fatal("rendering final consent changed config bytes before confirmation")
			}
		})
	}
}

func TestVisibilityLoginRetainsSameSourceAndPreservesDraft(t *testing.T) {
	for _, row := range loadConsentLoginDocument(t).Login {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			loaded := config.BaseConfig()
			if err := config.SaveAtomic(path, loaded); err != nil {
				t.Fatalf("seed visibility login config: %v", err)
			}
			draft, err := settings.NewDraft(path, loaded)
			if err != nil {
				t.Fatalf("open visibility login draft: %v", err)
			}
			draft.Working().Push.License = row.BufferedLicense
			before := mustReadFile(t, path)
			source := &countingTreeSource{inner: scannerfix.NewFixtureTreeSource("standard")}
			loginCalls := 0
			program := kickstart.NewProgram(kickstart.ProgramDeps{
				Theme:  theme.New(theme.ModeDark),
				Draft:  draft,
				Source: source,
				Login: func(context.Context, func(string)) (string, error) {
					loginCalls++
					if row.Outcome == loginFailure {
						return "", errors.New(row.Error)
					}
					return row.Username, nil
				},
			})
			program.SetSize(180, 50)
			program = enterDisconnectedFlow(t, program)
			for step := 0; step < 16 && program.Phase() != kickstart.PhaseVisibility; step++ {
				program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			}
			if program.Phase() != kickstart.PhaseVisibility {
				t.Fatalf("logged-out guided flow never offered visibility login; phase=%s view:\n%s", program.Phase(), stripRender(program.View()))
			}

			if row.Outcome == loginDecline {
				var command tea.Cmd
				program, command = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				if command != nil {
					t.Fatal("declining visibility login emitted an unexpected command")
				}
			} else {
				program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
				var command tea.Cmd
				program, command = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				if command == nil {
					t.Fatal("accepting visibility login produced no Login command")
				}
				for _, message := range collectMsgs(command) {
					beforePhase := program.Phase()
					var next tea.Cmd
					program, next = program.Update(message)
					if beforePhase == kickstart.PhaseVisibility && program.Phase() == kickstart.PhaseFlow {
						if next != nil {
							t.Fatal("successful visibility login restarted the mounted settings flow")
						}
					}
				}
			}

			wantLoginCalls := 1
			if row.Outcome == loginDecline {
				wantLoginCalls = 0
			}
			if loginCalls != wantLoginCalls {
				t.Fatalf("visibility Login calls=%d, want %d", loginCalls, wantLoginCalls)
			}
			if draft.Working().Push.License != row.BufferedLicense {
				t.Fatalf("visibility login changed buffered license to %q, want %q", draft.Working().Push.License, row.BufferedLicense)
			}
			if after := mustReadFile(t, path); !bytes.Equal(before, after) {
				t.Fatal("visibility login wrote config before final consent")
			}
			view := stripRender(program.View())
			for _, want := range row.WantContains {
				if !strings.Contains(view, want) {
					t.Errorf("visibility login outcome does not contain %q:\n%s", want, view)
				}
			}
			switch row.Outcome {
			case loginSuccess:
				if !program.Connected() || program.Phase() != kickstart.PhaseFlow {
					t.Fatalf("successful visibility login connected/phase=%t/%s, want true/flow", program.Connected(), program.Phase())
				}
				if source.Loads() != 1 {
					t.Fatalf("successful visibility login source loads=%d, want one retained-flow load", source.Loads())
				}
			case loginFailure:
				if program.Connected() || program.Phase() != kickstart.PhaseVisibility {
					t.Fatalf("failed visibility login connected/phase=%t/%s, want false/visibility", program.Connected(), program.Phase())
				}
				if source.Loads() != 1 {
					t.Fatalf("failed visibility login rebuilt source %d times, want initial load only", source.Loads())
				}
			case loginDecline:
				if program.Connected() || program.Phase() != kickstart.PhaseFlow {
					t.Fatalf("declined visibility login connected/phase=%t/%s, want false/flow", program.Connected(), program.Phase())
				}
				if source.Loads() != 1 {
					t.Fatalf("declined visibility login loaded source %d times, want initial load only", source.Loads())
				}
			}
		})
	}
}
