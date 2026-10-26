package kickstart_test

import (
	"context"
	_ "embed"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// autoPublishCase is one answer to "publish automatically?"; see
// testdata/auto_publish.yaml.
type autoPublishCase struct {
	Name                  string                 `yaml:"name"`
	Connected             bool                   `yaml:"connected"`
	Collectives           []string               `yaml:"collectives"`
	SeedSharePreference   config.SharePreference `yaml:"seedSharePreference"`
	PublicationFirst      string                 `yaml:"publicationFirst"`
	Answer                string                 `yaml:"answer"`
	PublicationAfter      string                 `yaml:"publicationAfter"`
	WantAutoPublishIntent bool                   `yaml:"wantAutoPublishIntent"`
	WantSharePreference   config.SharePreference `yaml:"wantSharePreference"`
	WantReview            string                 `yaml:"wantReview"`
}

type autoPublishDocument struct {
	RequiredNames []string          `yaml:"requiredNames"`
	Cases         []autoPublishCase `yaml:"cases"`
}

//go:embed testdata/auto_publish.yaml
var autoPublishFixtureData []byte

// publicationLabels names the publication step's answers by the preference
// they save, so a fixture can choose one by what it means.
var publicationLabels = map[string]string{
	"keep-local":  "keep local, do not publish",
	"share-later": "plan to publish later",
}

func loadAutoPublishCases(t *testing.T) []autoPublishCase {
	t.Helper()
	var document autoPublishDocument
	if err := testutil.DecodeNamedFixtureYAML(autoPublishFixtureData, &document); err != nil {
		t.Fatalf("decode testdata/auto_publish.yaml: %v", err)
	}
	for _, c := range document.Cases {
		if c.Answer != "yes" && c.Answer != "not now" {
			t.Fatalf("case %q answers %q, want yes or not now", c.Name, c.Answer)
		}
		for _, choice := range []string{c.PublicationFirst, c.PublicationAfter} {
			if _, known := publicationLabels[choice]; choice != "" && !known {
				t.Fatalf("case %q chooses publication %q, want keep-local or share-later", c.Name, choice)
			}
		}
		if len(c.Collectives) > 0 && !c.Connected {
			t.Fatalf("case %q lists collectives on a Village it is not signed in to", c.Name)
		}
		if strings.TrimSpace(c.WantReview) == "" {
			t.Fatalf("case %q names no review row", c.Name)
		}
	}
	return document.Cases
}

// TestAutoPublishAnswerRecordsIntentAndInstallsNothing drives the mounted
// kickstart Program through each answer and its save. The only discovered
// session was recorded in a real git repository, the one a hook would be
// installed in, and the configured Village lists the case's collectives.
func TestAutoPublishAnswerRecordsIntentAndInstallsNothing(t *testing.T) {
	t.Parallel()
	for _, c := range loadAutoPublishCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			world := t.TempDir()
			configDir := filepath.Join(world, "config")
			repository := filepath.Join(world, "ingest-api")
			initRepository(t, repository)
			village := newRecordingVillage(t, c.Collectives)

			seed := config.BaseConfig()
			seed.Push.SharePreference = c.SeedSharePreference
			seed.Village.URL = village.URL()
			seed.Village.Connected = c.Connected
			configPath := defaults.ResolveConfigFilePathWith(configDir).String()
			if err := config.SaveAtomic(configPath, seed); err != nil {
				t.Fatalf("seed config: %v", err)
			}
			loaded, err := config.Parse(mustReadFile(t, configPath))
			if err != nil {
				t.Fatalf("parse seed config: %v", err)
			}
			draft, err := settings.NewDraft(configPath, loaded)
			if err != nil {
				t.Fatalf("open draft: %v", err)
			}

			var effects []string
			program := kickstart.NewProgram(kickstart.ProgramDeps{
				Theme: theme.New(theme.ModeDark),
				Draft: draft,
				Source: kickstart.NewScannerTreeSource([]ftue.SessionListing{{
					Harness: "claude-code", ProjectName: "ingest-api", GitRemote: "git@github.com:acme/ingest-api.git",
					Branch: "main", SessionID: "auto-publish-session", WorkingDir: repository,
				}}, kickstart.WithPathIdentityResolver(ingest.NewPhysicalPathResolver())),
				AlreadyConnected: c.Connected,
				Ingest: func(context.Context) (*ftue.IngestResult, error) {
					effects = append(effects, "ingest")
					return &ftue.IngestResult{New: 1}, nil
				},
			})
			program.SetSize(120, 40)
			program = drainProgram(program, program.Init())
			if !c.Connected {
				program = declineOAuth(t, program)
			}

			if c.PublicationFirst != "" {
				program = goToSection(t, program, "publication")
				program = chooseRadioLabel(t, program, publicationLabels[c.PublicationFirst])
			}
			program = goToSection(t, program, "auto-publish")
			question := stripRender(program.View())
			for _, want := range []string{"publish automatically?", "you choose which folders go to which collectives in settings later.", "( ) yes"} {
				if !strings.Contains(question, want) {
					t.Fatalf("the question does not show %q:\n%s", want, question)
				}
			}
			program = chooseRadioLabel(t, program, c.Answer)
			if c.PublicationAfter != "" {
				program = goToSection(t, program, "publication")
				program = chooseRadioLabel(t, program, publicationLabels[c.PublicationAfter])
			}

			program = goToReceipt(t, program)
			review := stripRender(program.View())
			if !strings.Contains(review, c.WantReview) {
				t.Fatalf("the review does not show %q:\n%s", c.WantReview, review)
			}
			installsNothing := "install no git hook and save no auto-publish rule"
			if strings.Contains(review, installsNothing) != c.WantAutoPublishIntent {
				t.Fatalf("the review shows %q = %t, want %t:\n%s", installsNothing, !c.WantAutoPublishIntent, c.WantAutoPublishIntent, review)
			}
			var command tea.Cmd
			program, command = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if program.ConfirmingNoProjects() {
				t.Fatal("the discovered session's project did not reach the saved selection")
			}
			program = drainCmds(t, program, command)
			if !program.Committed() {
				t.Fatal("kickstart did not save")
			}

			saved, err := config.Parse(mustReadFile(t, configPath))
			if err != nil {
				t.Fatalf("parse saved config: %v", err)
			}
			if saved.Push.AutoPublishIntent != c.WantAutoPublishIntent || saved.Push.SharePreference != c.WantSharePreference {
				t.Fatalf("saved autoPublishIntent/sharePreference = %t/%q, want %t/%q",
					saved.Push.AutoPublishIntent, saved.Push.SharePreference, c.WantAutoPublishIntent, c.WantSharePreference)
			}
			if saved.Push.AutoPublishIntended() != c.WantAutoPublishIntent {
				t.Fatalf("saved intent holds = %t, want %t", saved.Push.AutoPublishIntended(), c.WantAutoPublishIntent)
			}
			if len(effects) != 1 || effects[0] != "ingest" {
				t.Fatalf("steps after the save = %v, want only the local import", effects)
			}
			rules := autopublish.Path(defaults.ConfigDirPath(configDir))
			if _, err := os.Stat(rules); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("kickstart wrote the auto-publish rules file %s (stat: %v)", rules, err)
			}
			assertNoHookFile(t, repository)
			if requests := village.Requests(); len(requests) != 0 {
				t.Fatalf("kickstart called the Village: %v", requests)
			}
		})
	}
}

// goToSection steps the mounted flow, forward and then back, until the frame
// shows the section titled title.
func goToSection(t *testing.T, p kickstart.Program, title string) kickstart.Program {
	t.Helper()
	marker := "─ " + title + " ─"
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyTab}, {Code: tea.KeyTab, Mod: tea.ModShift}} {
		for step := 0; step < 12; step++ {
			if firstLine(stripRender(p.View())) != "" && strings.Contains(firstLine(stripRender(p.View())), marker) {
				return p
			}
			if key.Mod == 0 && p.OnReceipt() {
				break
			}
			var command tea.Cmd
			p, command = p.Update(key)
			p = drainProgram(p, command)
		}
	}
	t.Fatalf("the flow never showed the %q section:\n%s", title, stripRender(p.View()))
	return p
}

// goToReceipt steps the mounted flow forward to the review step.
func goToReceipt(t *testing.T, p kickstart.Program) kickstart.Program {
	t.Helper()
	for step := 0; step < 12 && !p.OnReceipt(); step++ {
		var command tea.Cmd
		p, command = p.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		p = drainProgram(p, command)
	}
	if !p.OnReceipt() {
		t.Fatalf("the flow never reached the review step:\n%s", stripRender(p.View()))
	}
	return p
}

var radioRow = regexp.MustCompile(`\((?:•| )\) (.+?)\s*│?$`)

// chooseRadioLabel selects the option labeled label in the section on screen,
// finding its position from the rendered rows.
func chooseRadioLabel(t *testing.T, p kickstart.Program, label string) kickstart.Program {
	t.Helper()
	var labels []string
	for _, line := range strings.Split(stripRender(p.View()), "\n") {
		if match := radioRow.FindStringSubmatch(strings.TrimRight(line, " │")); match != nil {
			labels = append(labels, strings.TrimSpace(match[1]))
		}
	}
	index := -1
	for i, l := range labels {
		if l == label {
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("the section offers %v, not %q", labels, label)
	}
	for range labels {
		p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	for range index {
		p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	return p
}

func firstLine(view string) string {
	line, _, _ := strings.Cut(view, "\n")
	return line
}

// initRepository creates an empty git repository at dir.
func initRepository(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

// assertNoHookFile fails when the repository's hooks directory holds anything
// but git's own samples, or when git reads hooks from somewhere else.
func assertNoHookFile(t *testing.T, repository string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", repository, "config", "--get", "core.hooksPath").Output(); err == nil {
		t.Fatalf("the repository reads hooks from %q", strings.TrimSpace(string(out)))
	}
	out, err := exec.Command("git", "-C", repository, "rev-parse", "--path-format=absolute", "--git-path", "hooks").Output()
	if err != nil {
		t.Fatalf("resolve the hooks directory: %v", err)
	}
	hooks := strings.TrimSpace(string(out))
	entries, err := os.ReadDir(hooks)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read %s: %v", hooks, err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sample") {
			t.Fatalf("kickstart left a hook file %s in %s", entry.Name(), hooks)
		}
	}
}

// recordingVillage is a Village that lists collectives and records every
// request it gets.
type recordingVillage struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []string
}

func newRecordingVillage(t *testing.T, collectives []string) *recordingVillage {
	t.Helper()
	v := &recordingVillage{}
	v.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.requests = append(v.requests, r.Method+" "+r.URL.Path)
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		names := make([]string, len(collectives))
		for i, name := range collectives {
			names[i] = `{"name":"` + name + `"}`
		}
		_, _ = w.Write([]byte(`{"collectives":[` + strings.Join(names, ",") + `]}`))
	}))
	t.Cleanup(v.server.Close)
	return v
}

func (v *recordingVillage) URL() string { return v.server.URL }

func (v *recordingVillage) Requests() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.requests...)
}
