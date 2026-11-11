package onboarding_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/commit.yaml
var commitData []byte

type commitConflictWant struct {
	Field          string   `yaml:"field"`
	Stop           string   `yaml:"stop"`
	Label          string   `yaml:"label"`
	Mine           string   `yaml:"mine"`
	Theirs         string   `yaml:"theirs"`
	MineContains   []string `yaml:"mineContains"`
	TheirsContains []string `yaml:"theirsContains"`
}

type commitParsedWant struct {
	RedactionLevel      string `yaml:"redactionLevel"`
	License             string `yaml:"license"`
	Visibility          string `yaml:"visibility"`
	SharePreference     string `yaml:"sharePreference"`
	Email               string `yaml:"email"`
	AutoPublishIntent   *bool  `yaml:"autoPublishIntent"`
	AutoPublishIntended *bool  `yaml:"autoPublishIntended"`
	AutoIngest          *bool  `yaml:"autoIngest"`
	OutputBasePath      string `yaml:"outputBasePath"`
}

type commitRetentionWant struct {
	Status       string `yaml:"status"`
	Days         int    `yaml:"days"`
	NoteStarts   string `yaml:"noteStarts"`
	NoteContains string `yaml:"noteContains"`
}

type commitWant struct {
	Saved              bool                 `yaml:"saved"`
	Failure            string               `yaml:"failure"`
	ErrorContains      string               `yaml:"errorContains"`
	Conflicts          []commitConflictWant `yaml:"conflicts"`
	FileUnchanged      bool                 `yaml:"fileUnchanged"`
	FileAbsent         bool                 `yaml:"fileAbsent"`
	FileEquals         string               `yaml:"fileEquals"`
	LockReleased       bool                 `yaml:"lockReleased"`
	Parsed             commitParsedWant     `yaml:"parsed"`
	Effects            []string             `yaml:"effects"`
	Retention          commitRetentionWant  `yaml:"retention"`
	EffectSawSavedFile []string             `yaml:"effectSawSavedFile"`
	EffectSawHeldLock  []string             `yaml:"effectSawHeldLock"`
	ValidatorCalls     *int                 `yaml:"validatorCalls"`
	CLIEquivalent      bool                 `yaml:"cliEquivalent"`
	KeyOrder           []string             `yaml:"keyOrder"`
	KeepsLines         []string             `yaml:"keepsLines"`
	LacksLines         []string             `yaml:"lacksLines"`
	KeepsText          []string             `yaml:"keepsText"`
	LacksText          []string             `yaml:"lacksText"`
	DirEntries         []string             `yaml:"dirEntries"`
	VillageRequests    *int                 `yaml:"villageRequests"`
}

type commitResolution struct {
	Choice string `yaml:"choice"`
	Seen   string `yaml:"seen"`
}

type commitInterleave struct {
	Writes []string `yaml:"writes"`
}

type commitScenario struct {
	Note             string                      `yaml:"note"`
	Opened           string                      `yaml:"opened"`
	OpenedAbsent     bool                        `yaml:"openedAbsent"`
	OnDisk           string                      `yaml:"onDisk"`
	Interleave       commitInterleave            `yaml:"interleave"`
	Answers          map[string]string           `yaml:"answers"`
	Resolutions      map[string]commitResolution `yaml:"resolutions"`
	Scope            string                      `yaml:"scope"`
	ClaudeSessions   bool                        `yaml:"claudeSessions"`
	VillageConnected bool                        `yaml:"villageConnected"`
	// RetentionSeed is the period the session showed on the retention stop; it is
	// not in the file.
	RetentionSeed   int        `yaml:"retentionSeed"`
	RetentionError  string     `yaml:"retentionError"`
	GitEmail        string     `yaml:"gitEmail"`
	VillageServer   bool       `yaml:"villageServer"`
	SelectionCorpus string     `yaml:"selectionCorpus"`
	Want            commitWant `yaml:"want"`
}

type commitCase struct {
	Name      string           `yaml:"name"`
	Scenarios []commitScenario `yaml:"scenarios"`
}

type commitDocument struct {
	RequiredNames []string     `yaml:"requiredNames"`
	Cases         []commitCase `yaml:"cases"`
}

func TestDraftCommit(t *testing.T) {
	t.Parallel()
	var document commitDocument
	decodeFixture(t, "commit.yaml", commitData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			if len(row.Scenarios) == 0 {
				t.Fatalf("row %s has no scenarios", row.Name)
			}
			for i, scenario := range row.Scenarios {
				t.Run(scenarioName(i, scenario), func(t *testing.T) {
					if scenario.SelectionCorpus != "" {
						runSelectionCorpus(t, scenario)
						return
					}
					runCommitScenario(t, scenario, nil)
				})
			}
		})
	}
}

func scenarioName(index int, scenario commitScenario) string {
	return strings.ReplaceAll(strings.TrimSpace(scenario.Note), " ", "_") + "#" + string(rune('a'+index))
}

// validationProbe wraps a real field so a test can act inside the commit's
// validation pass, which runs between the read and the swap. It changes nothing
// else about the field.
type validationProbe struct {
	onboarding.Field
	probe func()
}

func (p validationProbe) Validate(w onboarding.WriteContext) error {
	p.probe()
	return p.Field.Validate(w)
}

func probedModel(t *testing.T, probe func()) onboarding.Model {
	t.Helper()
	base := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull})
	stops := append([]onboarding.Stop(nil), base.Stops...)
	for i := range stops {
		fields := append([]onboarding.Field(nil), stops[i].Fields...)
		for j, field := range fields {
			if field != nil && field.Key() == onboarding.FieldPrivacy {
				fields[j] = validationProbe{Field: field, probe: probe}
			}
		}
		stops[i].Fields = fields
	}
	model, err := onboarding.ModelFromStops(base.Scope, stops, nil)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

type effectLog struct {
	events      []string
	atHook      map[string][]byte
	lockHeld    map[string]bool
	pathForLock string
}

func newEffectLog(path string) *effectLog {
	return &effectLog{atHook: map[string][]byte{}, lockHeld: map[string]bool{}, pathForLock: path}
}

func (l *effectLog) record(name string) {
	l.events = append(l.events, name)
	l.atHook[name], _ = os.ReadFile(l.pathForLock)
	probe, err := config.OpenConfigFile(l.pathForLock, time.Now().Add(40*time.Millisecond))
	if err == nil {
		_ = probe.Close()
	}
	l.lockHeld[name] = err != nil
}

func runCommitScenario(t *testing.T, s commitScenario, golden *config.SelectionConfig) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "peasant")
	path := filepath.Join(dir, "config.yaml")

	var villageRequests atomic.Int64
	villageURL := ""
	if s.VillageServer {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			villageRequests.Add(1)
		}))
		t.Cleanup(server.Close)
		villageURL = server.URL
	}
	fill := func(text string) string { return strings.ReplaceAll(text, "{{villageURL}}", villageURL) }

	var loaded *config.Config
	if s.OpenedAbsent {
		var git *testutil.StubGitResolver
		if s.GitEmail != "" {
			git = &testutil.StubGitResolver{Email: s.GitEmail}
			loaded = config.LoadDefaults(ctx, git)
		} else {
			loaded = config.LoadDefaults(ctx, nil)
		}
	} else {
		writeFixtureFile(t, path, fill(s.Opened))
		parsed, err := config.Parse([]byte(fill(s.Opened)))
		if err != nil {
			t.Fatalf("the opened document does not parse: %v", err)
		}
		loaded = parsed
	}
	draft, err := onboarding.OpenDraft(path, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if s.RetentionSeed > 0 {
		days := s.RetentionSeed
		if err := draft.SeedTransient(func(cfg *config.Config) { cfg.ClaudeRetentionDays = days }); err != nil {
			t.Fatal(err)
		}
	}
	if s.OnDisk != "" {
		writeFixtureFile(t, path, fill(s.OnDisk))
	}
	before, _ := os.ReadFile(path)

	writes := 0
	validatorCalls := 0
	model := probedModel(t, func() {
		validatorCalls++
		if writes < len(s.Interleave.Writes) {
			writeFixtureFile(t, path, fill(s.Interleave.Writes[writes]))
			writes++
		}
	})
	if s.Scope != "" {
		scope, err := onboarding.NewModelScope(s.Scope)
		if err != nil {
			t.Fatal(err)
		}
		model = onboarding.BuildModel(onboarding.Options{Scope: scope})
	}

	answers := onboarding.Answers{}
	for key, text := range s.Answers {
		field, err := onboarding.NewFieldKey(key)
		if err != nil {
			t.Fatal(err)
		}
		answers[field] = schema.LocalSettingValue(text)
	}
	if golden != nil {
		value, err := onboarding.SelectionMappingValue(golden.Mode, golden.Harnesses)
		if err != nil {
			t.Fatal(err)
		}
		answers[onboarding.FieldSelection] = value
		answers[onboarding.FieldAutoIngest] = onboarding.BoolValue(golden.AutoIngestNewBranches)
	}
	resolutions := onboarding.Resolutions{}
	for key, resolution := range s.Resolutions {
		field, err := onboarding.NewFieldKey(key)
		if err != nil {
			t.Fatal(err)
		}
		choice, err := schema.NewLocalKickstartResolution(resolution.Choice)
		if err != nil {
			t.Fatal(err)
		}
		resolutions[field] = onboarding.Resolution{Choice: choice, Seen: schema.LocalSettingValue(resolution.Seen)}
	}

	log := newEffectLog(path)
	hooks := onboarding.CommitHooks{
		Publish: func(*config.Config) { log.record("publish") },
		Retention: func(days int) error {
			log.record("retention")
			if s.RetentionError != "" {
				return errors.New(s.RetentionError)
			}
			if days <= 0 {
				t.Errorf("retention writer called with %d days", days)
			}
			return nil
		},
	}
	input := onboarding.CommitInput{
		Model:       model,
		Answers:     answers,
		Resolutions: resolutions,
		Context: onboarding.WriteContext{
			ScanState:        onboarding.ScanDone,
			ClaudeSessions:   s.ClaudeSessions,
			VillageConnected: s.VillageConnected,
		},
		LockDeadline: time.Now().Add(5 * time.Second),
		Hooks:        hooks,
	}
	result, commitErr := draft.Commit(ctx, input)
	// The caller starts the ingest only after Commit returns: the lock must be free.
	ingestLockHeld := lockIsHeld(path)
	log.events = append(log.events, "ingest")

	want := s.Want
	assertCommitOutcome(t, s, want, result, commitErr)
	if ingestLockHeld {
		t.Errorf("the configuration lock was still held when Commit returned")
	}
	after, readErr := os.ReadFile(path)
	if want.FileUnchanged && (!bytes.Equal(before, after) || readErr != nil) {
		t.Errorf("the file changed:\nbefore: %q\n after: %q", before, after)
	}
	if want.FileAbsent && !errors.Is(readErr, os.ErrNotExist) {
		t.Errorf("the configuration file should not exist, read it: %q, %v", after, readErr)
	}
	if want.FileEquals != "" && string(after) != want.FileEquals {
		t.Errorf("file = %q, want %q", after, want.FileEquals)
	}
	assertEffects(t, want, log, after)
	if want.ValidatorCalls != nil && validatorCalls != *want.ValidatorCalls {
		t.Errorf("validation passes = %d, want %d", validatorCalls, *want.ValidatorCalls)
	}
	if want.Saved {
		assertSavedFile(t, s, want, after, golden)
	}
	if want.DirEntries != nil {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if !slices.Equal(names, want.DirEntries) {
			t.Errorf("files written beside config.yaml = %v, want exactly %v", names, want.DirEntries)
		}
	}
	if want.VillageRequests != nil && int(villageRequests.Load()) != *want.VillageRequests {
		t.Errorf("village requests = %d, want %d", villageRequests.Load(), *want.VillageRequests)
	}
	if want.CLIEquivalent && want.Saved {
		assertCLIEquivalent(t, model, loaded, input, after)
	}
	// Whatever the exit, the lock is free for the next writer.
	next, err := config.OpenConfigFile(path, time.Now().Add(2*time.Second))
	if err != nil {
		t.Fatalf("the lock is still held after Commit: %v", err)
	}
	_ = next.Close()
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lockIsHeld(path string) bool {
	probe, err := config.OpenConfigFile(path, time.Now().Add(40*time.Millisecond))
	if err == nil {
		_ = probe.Close()
	}
	return err != nil
}

func assertCommitOutcome(t *testing.T, s commitScenario, want commitWant, result onboarding.CommitResult, commitErr error) {
	t.Helper()
	if want.Failure != "" {
		var failed *onboarding.CommitError
		if !errors.As(commitErr, &failed) {
			t.Fatalf("Commit error = %v, want a *CommitError of kind %s", commitErr, want.Failure)
		}
		if failed.Kind.String() != want.Failure {
			t.Errorf("failure kind = %s, want %s: %v", failed.Kind, want.Failure, commitErr)
		}
		if want.ErrorContains != "" && !strings.Contains(commitErr.Error(), want.ErrorContains) {
			t.Errorf("error = %v, want it to contain %q", commitErr, want.ErrorContains)
		}
		if result.Saved || len(result.Conflicts) > 0 {
			t.Errorf("a failed commit returned a result: %+v", result)
		}
		return
	}
	if commitErr != nil {
		t.Fatalf("Commit: %v", commitErr)
	}
	if result.Saved != want.Saved {
		t.Fatalf("saved = %t, want %t (conflicts %v)", result.Saved, want.Saved, result.Conflicts)
	}
	if want.Conflicts != nil {
		assertConflicts(t, want.Conflicts, result.Conflicts)
	}
	if !result.Saved && len(result.Conflicts) == 0 {
		t.Errorf("a refused commit carries no row; a refusal always names at least one")
	}
	if want.Retention.Status != "" {
		got := result.Retention
		if got.Status.String() != want.Retention.Status || got.Days != want.Retention.Days {
			t.Errorf("retention = %s/%d, want %s/%d", got.Status, got.Days, want.Retention.Status, want.Retention.Days)
		}
		if !strings.HasPrefix(got.Note, want.Retention.NoteStarts) || !strings.Contains(got.Note, want.Retention.NoteContains) {
			t.Errorf("retention note = %q, want prefix %q containing %q", got.Note, want.Retention.NoteStarts, want.Retention.NoteContains)
		}
		if got.Status != onboarding.RetentionFailed && got.Note != "" {
			t.Errorf("a retention note is set on %s: %q", got.Status, got.Note)
		}
	}
}

func assertConflicts(t *testing.T, want []commitConflictWant, got []schema.LocalKickstartConflict) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("conflict rows = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, row := range want {
		g := got[i]
		if err := g.Validate(); err != nil {
			t.Errorf("row %d is not a valid contract row: %v", i, err)
		}
		if g.Field != row.Field || g.Stop != row.Stop || g.Label != row.Label {
			t.Errorf("row %d = %s/%s/%q, want %s/%s/%q", i, g.Field, g.Stop, g.Label, row.Field, row.Stop, row.Label)
		}
		if row.Mine != "" && string(g.Mine) != row.Mine {
			t.Errorf("row %d mine = %s, want %s", i, g.Mine, row.Mine)
		}
		if row.Theirs != "" && string(g.Theirs) != row.Theirs {
			t.Errorf("row %d theirs = %s, want %s", i, g.Theirs, row.Theirs)
		}
		for _, part := range row.MineContains {
			if !strings.Contains(string(g.Mine), part) {
				t.Errorf("row %d mine = %s, want it to contain %q", i, g.Mine, part)
			}
		}
		for _, part := range row.TheirsContains {
			if !strings.Contains(string(g.Theirs), part) {
				t.Errorf("row %d theirs = %s, want it to contain %q", i, g.Theirs, part)
			}
		}
	}
}

func assertEffects(t *testing.T, want commitWant, log *effectLog, final []byte) {
	t.Helper()
	if want.Effects != nil {
		var hooks []string
		for _, event := range log.events {
			if event != "ingest" {
				hooks = append(hooks, event)
			}
		}
		if !slices.Equal(hooks, want.Effects) {
			t.Errorf("effects = %v, want %v", hooks, want.Effects)
		}
		// The ingest starts last, after every hook and after the lock is free.
		if log.events[len(log.events)-1] != "ingest" {
			t.Errorf("effects = %v, want ingest last", log.events)
		}
	}
	for _, name := range want.EffectSawSavedFile {
		if !bytes.Equal(log.atHook[name], final) || len(final) == 0 {
			t.Errorf("%s ran before the configuration was saved:\nsaw:   %q\nfinal: %q", name, log.atHook[name], final)
		}
	}
	for _, name := range want.EffectSawHeldLock {
		if !log.lockHeld[name] {
			t.Errorf("%s ran after the configuration lock was released", name)
		}
	}
}

func assertSavedFile(t *testing.T, s commitScenario, want commitWant, written []byte, golden *config.SelectionConfig) {
	t.Helper()
	parsed, err := config.Parse(written)
	if err != nil {
		t.Fatalf("the saved file does not parse: %v\n%s", err, written)
	}
	p := want.Parsed
	if p.RedactionLevel != "" && parsed.Redaction.Level.String() != p.RedactionLevel {
		t.Errorf("redaction level = %s, want %s", parsed.Redaction.Level, p.RedactionLevel)
	}
	if p.License != "" && string(parsed.Push.License) != p.License {
		t.Errorf("license = %q, want %q", parsed.Push.License, p.License)
	}
	if p.Visibility != "" && string(parsed.Push.Visibility) != p.Visibility {
		t.Errorf("visibility = %q, want %q", parsed.Push.Visibility, p.Visibility)
	}
	if p.SharePreference != "" && string(parsed.Push.SharePreference) != p.SharePreference {
		t.Errorf("share preference = %q, want %q", parsed.Push.SharePreference, p.SharePreference)
	}
	if p.Email != "" && parsed.User.Email != p.Email {
		t.Errorf("user email = %q, want %q", parsed.User.Email, p.Email)
	}
	if p.AutoPublishIntent != nil && parsed.Push.AutoPublishIntent != *p.AutoPublishIntent {
		t.Errorf("autoPublishIntent = %t, want %t", parsed.Push.AutoPublishIntent, *p.AutoPublishIntent)
	}
	if p.AutoPublishIntended != nil && parsed.Push.AutoPublishIntended() != *p.AutoPublishIntended {
		t.Errorf("auto-publish intended = %t, want %t", parsed.Push.AutoPublishIntended(), *p.AutoPublishIntended)
	}
	if p.OutputBasePath != "" && parsed.Output.BasePath != p.OutputBasePath {
		t.Errorf("output.basePath = %q, want %q", parsed.Output.BasePath, p.OutputBasePath)
	}
	if p.AutoIngest != nil && parsed.Selection.AutoIngestNewBranches != *p.AutoIngest {
		t.Errorf("autoIngestNewBranches = %t, want %t", parsed.Selection.AutoIngestNewBranches, *p.AutoIngest)
	}
	if golden != nil {
		if got, want := normalizedSelection(t, parsed.Selection), normalizedSelection(t, *golden); got != want {
			t.Errorf("saved selection differs from its golden\n got: %s\nwant: %s", got, want)
		}
	}
	text := string(written)
	if want.KeyOrder != nil {
		var doc yaml.Node
		if err := yaml.Unmarshal(written, &doc); err != nil {
			t.Fatal(err)
		}
		var keys []string
		mapping := doc.Content[0]
		for i := 0; i+1 < len(mapping.Content); i += 2 {
			keys = append(keys, mapping.Content[i].Value)
		}
		if !slices.Equal(keys, want.KeyOrder) {
			t.Errorf("top-level key order = %v, want %v", keys, want.KeyOrder)
		}
	}
	lines := strings.Split(text, "\n")
	for _, line := range want.KeepsLines {
		if !slices.Contains(lines, line) {
			t.Errorf("the saved file lost the line %q:\n%s", line, text)
		}
	}
	for _, line := range want.LacksLines {
		if slices.Contains(lines, line) {
			t.Errorf("the saved file still holds the line %q:\n%s", line, text)
		}
	}
	for _, part := range want.KeepsText {
		if !strings.Contains(text, part) {
			t.Errorf("the saved file lost the text %q:\n%s", part, text)
		}
	}
	for _, part := range want.LacksText {
		if strings.Contains(text, part) {
			t.Errorf("the saved file holds the text %q:\n%s", part, text)
		}
	}
}

// assertCLIEquivalent runs the terminal's save path on a draft opened over the
// same configuration with the same answers, and requires that both files parse
// to the same configuration. The terminal's save marshals the working
// configuration whole; the web save must reach the same configuration.
func assertCLIEquivalent(t *testing.T, model onboarding.Model, loaded *config.Config, input onboarding.CommitInput, written []byte) {
	t.Helper()
	cliPath := filepath.Join(t.TempDir(), "config.yaml")
	draft, err := settings.NewDraft(cliPath, loaded)
	if err != nil {
		t.Fatal(err)
	}
	w := input.Context
	w.Config = draft.Working()
	kept, err := model.DropHidden(draft.Working(), input.Answers, w)
	if err != nil {
		t.Fatal(err)
	}
	w.Answers = kept
	if err := model.ApplyAnswers(draft.Working(), kept, w); err != nil {
		t.Fatal(err)
	}
	if err := draft.Commit(); err != nil {
		t.Fatal(err)
	}
	cliBytes, err := os.ReadFile(cliPath)
	if err != nil {
		t.Fatal(err)
	}
	web, err := config.Parse(written)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := config.Parse(cliBytes)
	if err != nil {
		t.Fatal(err)
	}
	webYAML, _ := yaml.Marshal(web)
	cliYAML, _ := yaml.Marshal(cli)
	if !bytes.Equal(webYAML, cliYAML) {
		t.Errorf("the web first save and the terminal first save differ\n web: %s\n cli: %s", webYAML, cliYAML)
	}
}

type corpusDocument struct {
	Scenarios []corpusScenario `yaml:"scenarios"`
}

type corpusScenario struct {
	Name   string       `yaml:"name"`
	Golden corpusGolden `yaml:"golden"`
}

type corpusGolden struct {
	Mode                  string                   `yaml:"mode"`
	AutoIngestNewBranches bool                     `yaml:"autoIngestNewBranches"`
	Harnesses             map[string]corpusHarness `yaml:"harnesses"`
}

type corpusHarness struct {
	Projects []config.ProjectSelection `yaml:"projects"`
	Sessions []string                  `yaml:"sessions"`
}

func (g corpusGolden) selection() config.SelectionConfig {
	selection := config.SelectionConfig{Mode: config.SelectionMode(g.Mode), AutoIngestNewBranches: g.AutoIngestNewBranches}
	if len(g.Harnesses) > 0 {
		selection.Harnesses = map[string]config.SelectionHarnessConfig{}
		for name, harness := range g.Harnesses {
			selection.Harnesses[name] = config.SelectionHarnessConfig{Projects: harness.Projects, Sessions: harness.Sessions}
		}
	}
	return selection
}

// runSelectionCorpus commits every captured selection of the terminal wizard's
// equivalence corpus as a first save and requires the saved selection to match
// the capture and the terminal's own save.
func runSelectionCorpus(t *testing.T, s commitScenario) {
	t.Helper()
	data, err := os.ReadFile(s.SelectionCorpus)
	if err != nil {
		t.Fatalf("read the selection corpus: %v", err)
	}
	var corpus corpusDocument
	if err := yaml.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("decode the selection corpus: %v", err)
	}
	if len(corpus.Scenarios) == 0 {
		t.Fatalf("the selection corpus %s holds no scenarios", s.SelectionCorpus)
	}
	for _, scenario := range corpus.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			golden := scenario.Golden.selection()
			runCommitScenario(t, s, &golden)
		})
	}
}

func normalizedSelection(t *testing.T, selection config.SelectionConfig) string {
	t.Helper()
	for name, harness := range selection.Harnesses {
		slices.Sort(harness.Sessions)
		for i := range harness.Projects {
			slices.Sort(harness.Projects[i].Branches)
			slices.Sort(harness.Projects[i].ClonePaths)
		}
		slices.SortFunc(harness.Projects, func(a, b config.ProjectSelection) int {
			return strings.Compare(a.GitRemote+"|"+a.Name, b.GitRemote+"|"+b.Name)
		})
		selection.Harnesses[name] = harness
	}
	encoded, err := yaml.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
