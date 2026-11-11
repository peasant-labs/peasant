package api

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
)

//go:embed testdata/kickstart_commit.yaml
var kickstartCommitYAML []byte

// settingsSectionsManifest is the settings page's section manifest, where the
// finish menu's automatic-publishing link must land.
const settingsSectionsManifest = "../../web/src/app/testdata/settings-sections.yaml"

// TestKickstartCommit runs the commit fixture against the mounted server: the
// save, its refusal with rows, the import that follows, and what each tab hears.
func TestKickstartCommit(t *testing.T) {
	runKickstartScript(t, kickstartCommitYAML)
}

// kickstartIngestCheck is what a case requires of the imports the server ran.
type kickstartIngestCheck struct {
	// Runs waits until this many imports have started.
	Runs int `yaml:"runs"`
	// Finished waits until this many imports have ended.
	Finished int `yaml:"finished"`
	// SelectionMode and Sessions are the live saved selection the last started
	// import saw when it began.
	SelectionMode string   `yaml:"selectionMode"`
	Sessions      []string `yaml:"sessions"`
	// Present and Absent name sessions the local store must and must not hold.
	Present []string `yaml:"present"`
	Absent  []string `yaml:"absent"`
}

// kickstartRetentionCheck is what a case requires of the Claude retention writes.
type kickstartRetentionCheck struct {
	// Calls are the periods written, in order.
	Calls []int `yaml:"calls"`
	// SawSavedFile requires the configuration to be saved when each was written.
	SawSavedFile bool `yaml:"sawSavedFile"`
}

// kickstartEditConfig replaces one piece of text in the configuration file.
type kickstartEditConfig struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

type kickstartIngestRun struct {
	selection config.SelectionConfig
}

type kickstartRetentionCall struct {
	days int
	file string
}

// kickstartCommitWorld is what the commit fixtures add to a world: recordings
// for the import to read, a hold on the import, and a record of what it saw.
type kickstartCommitWorld struct {
	mu         sync.Mutex
	dir        string
	recordings string
	output     string
	homes      testXDGHomes
	real       bool
	gate       chan struct{}
	started    []kickstartIngestRun
	finished   int
	retention  []kickstartRetentionCall
	wake       chan struct{}
}

func (c *kickstartCommitWorld) prepare(t *testing.T, dir string, homes testXDGHomes, spec kickstartSpec) {
	t.Helper()
	c.dir, c.homes = dir, homes
	c.recordings = filepath.Join(dir, "recordings")
	c.output = filepath.Join(dir, "managed")
	c.wake = make(chan struct{})
	if spec.HoldIngest {
		c.gate = make(chan struct{}, 16)
		// A held import must not outlive the case.
		t.Cleanup(func() {
			for range 16 {
				select {
				case c.gate <- struct{}{}:
				default:
				}
			}
		})
	}
	if len(spec.Recordings) == 0 {
		return
	}
	c.real = true
	project := filepath.Join(c.recordings, "-workspace")
	cwd := filepath.Join(dir, "cwd")
	for _, path := range []string{project, cwd} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range spec.Recordings {
		writeClaudeSession(t, filepath.Join(project, id+".jsonl"), id, cwd, "main")
	}
}

// fill replaces the case's placeholders in a document: {dir}, {recordings}, and
// {sources}, a sources block that turns every harness off except Claude Code,
// read from the recordings directory.
func (c *kickstartCommitWorld) fill(text string) string {
	sources := "sources:\n  " + ingest.HarnessClaudeCode.String() + ":\n    enabled: true\n    paths: [" + c.recordings + "]\n"
	var others []string
	for harness := range ingest.DefaultAdapterRegistry {
		if harness != ingest.HarnessClaudeCode {
			others = append(others, harness.String())
		}
	}
	slices.Sort(others)
	for _, harness := range others {
		sources += "  " + harness + ":\n    enabled: false\n    paths: []\n"
	}
	return strings.NewReplacer("{sources}\n", sources, "{sources}", sources, "{recordings}", c.recordings, "{dir}", c.dir).Replace(text)
}

func (c *kickstartCommitWorld) serverConfig() *config.Config {
	cfg := config.BaseConfig()
	cfg.Output.BasePath = c.output
	return cfg
}

func (c *kickstartCommitWorld) changed() {
	close(c.wake)
	c.wake = make(chan struct{})
}

// tuneSync replaces how the server runs an import. A world with recordings runs
// the real pipeline over them; any other world finishes the import at once, so
// no case reads the developer's own sessions. Both record what the import saw
// and wait on the hold.
func (c *kickstartCommitWorld) tuneSync(sh *syncHandler) {
	real := sh.runIngestPipeline
	sh.ingestRun = func(progress *ingest.ProgressState) {
		cfg := sh.currentConfig()
		c.mu.Lock()
		c.started = append(c.started, kickstartIngestRun{selection: cfg.Selection})
		c.changed()
		c.mu.Unlock()
		if c.gate != nil {
			<-c.gate
		}
		if c.real {
			c.guardSources(cfg)
			real(progress)
		} else {
			sh.mu.Lock()
			sh.ingestStatus = "done"
			sh.ingestResult = &ingest.PipelineResult{}
			sh.mu.Unlock()
		}
		c.mu.Lock()
		c.finished++
		c.changed()
		c.mu.Unlock()
	}
}

// guardSources fails a case whose import would read outside the case directory.
func (c *kickstartCommitWorld) guardSources(cfg *config.Config) {
	for harness := range ingest.DefaultAdapterRegistry {
		provider, ok := cfg.Sources.Provider(harness)
		if !ok || !provider.Enabled {
			continue
		}
		for _, path := range provider.Paths {
			if !strings.HasPrefix(path, c.dir) {
				panic("kickstart fixture: the import would read " + path + ", outside the case directory " + c.dir + "; name {sources} in the case's configuration")
			}
		}
	}
}

// writeRetention records each Claude retention write and the configuration at
// that moment, and writes nothing.
func (c *kickstartCommitWorld) writeRetention(configPath string) func(int) error {
	return func(days int) error {
		file, _ := os.ReadFile(configPath)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.retention = append(c.retention, kickstartRetentionCall{days: days, file: string(file)})
		return nil
	}
}

func (w *kickstartWorld) releaseIngest() {
	if w.saving.gate == nil {
		w.t.Fatalf("case %q: releaseIngest needs holdIngest in the world", w.name)
	}
	w.saving.gate <- struct{}{}
}

// waitFor waits until the commit world's counters satisfy ready.
func (w *kickstartWorld) waitFor(index int, what string, ready func() bool) {
	w.t.Helper()
	ctx := testwait.Context(w.t)
	for {
		w.saving.mu.Lock()
		ok := ready()
		wake := w.saving.wake
		w.saving.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-wake:
		case <-ctx.Done():
			w.fail(index, "waited for %s and it did not happen", what)
		}
	}
}

func (w *kickstartWorld) ingestCheck(index int, check kickstartIngestCheck) {
	w.t.Helper()
	if check.Runs > 0 {
		w.waitFor(index, "an import to start", func() bool { return len(w.saving.started) >= check.Runs })
	}
	if check.Finished > 0 {
		w.waitFor(index, "an import to end", func() bool { return w.saving.finished >= check.Finished })
	}
	w.saving.mu.Lock()
	started := append([]kickstartIngestRun(nil), w.saving.started...)
	w.saving.mu.Unlock()
	if check.Runs > 0 && len(started) != check.Runs {
		w.fail(index, "%d imports started, want %d", len(started), check.Runs)
	}
	if check.SelectionMode != "" || check.Sessions != nil {
		if len(started) == 0 {
			w.fail(index, "no import started, so none saw a selection")
		}
		saw := started[len(started)-1].selection
		if check.SelectionMode != "" && string(saw.Mode) != check.SelectionMode {
			w.fail(index, "the last import began with selection mode %q, want %q", saw.Mode, check.SelectionMode)
		}
		if check.Sessions != nil {
			got := append([]string(nil), saw.Harnesses[ingest.HarnessClaudeCode.String()].Sessions...)
			want := append([]string(nil), check.Sessions...)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				w.fail(index, "the last import began with sessions %v, want %v", got, want)
			}
		}
	}
	if len(check.Present) > 0 || len(check.Absent) > 0 {
		w.storeCheck(index, check)
	}
}

// storeCheck reads the local store the import wrote.
func (w *kickstartWorld) storeCheck(index int, check kickstartIngestCheck) {
	w.t.Helper()
	cfg := w.server.live.load()
	options, err := store.ManagedGenerationOptions(w.saving.output)
	if err != nil {
		w.fail(index, "open the managed generation options: %v", err)
	}
	db, err := store.Open(w.saving.homes.dbPath(), append(options, store.WithWriteConfig(cfg.Write), store.WithSkipMigrations())...)
	if err != nil {
		w.fail(index, "open the local store: %v", err)
	}
	defer db.Close()
	for _, id := range check.Present {
		if row, err := db.SessionByID(context.Background(), id); err != nil || row == nil {
			w.fail(index, "the store should hold session %s: row=%v err=%v", id, row, err)
		}
	}
	for _, id := range check.Absent {
		if row, err := db.SessionByID(context.Background(), id); err != nil || row != nil {
			w.fail(index, "the store should not hold session %s: row=%v err=%v", id, row, err)
		}
	}
}

func (w *kickstartWorld) retentionCheck(index int, check kickstartRetentionCheck) {
	w.t.Helper()
	w.saving.mu.Lock()
	calls := append([]kickstartRetentionCall(nil), w.saving.retention...)
	w.saving.mu.Unlock()
	var days []int
	for _, call := range calls {
		days = append(days, call.days)
		if check.SawSavedFile && call.file == w.configBytes {
			w.fail(index, "retention %d was written before the configuration was saved", call.days)
		}
	}
	if !slices.Equal(days, check.Calls) {
		w.fail(index, "retention writes = %v, want %v", days, check.Calls)
	}
}

func (w *kickstartWorld) settingsSectionCheck(index int, id string) {
	w.t.Helper()
	data, err := os.ReadFile(settingsSectionsManifest)
	if err != nil {
		w.fail(index, "read the settings sections manifest: %v", err)
	}
	var manifest struct {
		Sections []struct {
			ID string `yaml:"id"`
		} `yaml:"sections"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		w.fail(index, "decode the settings sections manifest: %v", err)
	}
	for _, section := range manifest.Sections {
		if section.ID == id {
			return
		}
	}
	w.fail(index, "the settings page has no section %q", id)
}

func (w *kickstartWorld) configHas(index int, parts []string) {
	w.t.Helper()
	data, err := os.ReadFile(w.configPath)
	if err != nil {
		w.fail(index, "read the configuration file: %v", err)
	}
	for _, part := range parts {
		if !strings.Contains(string(data), part) {
			w.fail(index, "the configuration file should contain %q:\n%s", part, data)
		}
	}
}

func (w *kickstartWorld) writeConfig(index int, content string) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Dir(w.configPath), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		w.fail(index, "create the configuration directory: %v", err)
	}
	if err := os.WriteFile(w.configPath, []byte(w.saving.fill(content)), 0o600); err != nil {
		w.fail(index, "write the configuration file: %v", err)
	}
}

func (w *kickstartWorld) editConfig(index int, edit kickstartEditConfig) {
	w.t.Helper()
	data, err := os.ReadFile(w.configPath)
	if err != nil {
		w.fail(index, "read the configuration file: %v", err)
	}
	if !strings.Contains(string(data), edit.From) {
		w.fail(index, "the configuration file has no %q to change:\n%s", edit.From, data)
	}
	edited := strings.Replace(string(data), edit.From, edit.To, 1)
	if err := os.WriteFile(w.configPath, []byte(edited), 0o600); err != nil {
		w.fail(index, "write the configuration file: %v", err)
	}
}
