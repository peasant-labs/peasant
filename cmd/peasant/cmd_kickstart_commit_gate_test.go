package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const expectedKickstartCommitGateCaseCount = 12

const (
	commitGateParentSessionID = "11111111-1111-4111-8111-111111111111"
	commitGateChildSessionID  = "22222222-2222-4222-8222-222222222222"
)

type commitGateRunState string

const (
	commitGateFirstRun commitGateRunState = "first"
	commitGateLaterRun commitGateRunState = "later"
)

type commitGateSelection string

const (
	commitGateSelectionAll            commitGateSelection = "all"
	commitGateSelectionEmpty          commitGateSelection = "empty"
	commitGateSelectionExplicitParent commitGateSelection = "explicit-parent"
)

type commitGateKey string

const (
	commitGateKeySelectAll commitGateKey = "a"
	commitGateKeyDown      commitGateKey = "j"
	commitGateKeySpace     commitGateKey = "space"
	commitGateKeyLeft      commitGateKey = "left"
	commitGateKeyEnter     commitGateKey = "enter"
	commitGateKeyBack      commitGateKey = "esc"
	commitGateKeyCancel    commitGateKey = "q"
	commitGateKeyInterrupt commitGateKey = "ctrl+c"
)

type kickstartCommitGateDocument struct {
	DeclaredCaseCount int                       `yaml:"declaredCaseCount"`
	MessageLines      []string                  `yaml:"messageLines"`
	Paths             []kickstartGatePath       `yaml:"paths"`
	Listings          []kickstartGateListing    `yaml:"listings"`
	Cases             []kickstartCommitGateCase `yaml:"cases"`
}

type kickstartGatePath struct {
	Key string `yaml:"key"`
}

type kickstartGateListing struct {
	Harness     string   `yaml:"harness"`
	ProjectName string   `yaml:"projectName"`
	GitRemote   string   `yaml:"gitRemote"`
	PathKey     string   `yaml:"pathKey"`
	Branch      string   `yaml:"branch"`
	SessionID   string   `yaml:"sessionId"`
	Title       string   `yaml:"title"`
	SubagentIDs []string `yaml:"subagentIds"`
}

type kickstartCommitGateCase struct {
	Name             string              `yaml:"name"`
	RunState         commitGateRunState  `yaml:"runState"`
	InitialSelection commitGateSelection `yaml:"initialSelection"`
	EditKeys         []commitGateKey     `yaml:"editKeys"`
	AnswerKeys       []commitGateKey     `yaml:"answerKeys"`
	ExpectPrompt     bool                `yaml:"expectPrompt"`
	ExpectCommit     bool                `yaml:"expectCommit"`
	ExpectExit       bool                `yaml:"expectExit"`
	ExpectSelection  commitGateSelection `yaml:"expectSelection"`
}

//go:embed testdata/kickstart_commit_gate.yaml
var kickstartCommitGateData []byte

func loadKickstartCommitGateDocument(t *testing.T) kickstartCommitGateDocument {
	t.Helper()
	var document kickstartCommitGateDocument
	decoder := yaml.NewDecoder(bytes.NewReader(kickstartCommitGateData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode testdata/kickstart_commit_gate.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("kickstart_commit_gate.yaml must hold exactly one document: %v", err)
	}
	if document.DeclaredCaseCount != expectedKickstartCommitGateCaseCount || len(document.Cases) != expectedKickstartCommitGateCaseCount {
		t.Fatalf(
			"declaredCaseCount=%d and actual cases=%d; both must equal required count %d",
			document.DeclaredCaseCount,
			len(document.Cases),
			expectedKickstartCommitGateCaseCount,
		)
	}
	validateKickstartCommitGateDocument(t, document)
	return document
}

func validateKickstartCommitGateDocument(t *testing.T, document kickstartCommitGateDocument) {
	t.Helper()
	if len(document.MessageLines) != 6 {
		t.Fatalf("messageLines has %d rows, want the six accepted sentences", len(document.MessageLines))
	}
	pathKeys := make(map[string]struct{}, len(document.Paths))
	for _, path := range document.Paths {
		if path.Key == "" || filepath.IsAbs(path.Key) || filepath.Clean(path.Key) != path.Key {
			t.Fatalf("fixture has unsafe path key %q", path.Key)
		}
		if _, duplicate := pathKeys[path.Key]; duplicate {
			t.Fatalf("fixture repeats path key %q", path.Key)
		}
		pathKeys[path.Key] = struct{}{}
	}
	if len(document.Listings) < 2 {
		t.Fatal("fixture must include an available parent and nested descendant")
	}
	listingIDs := make(map[string]struct{}, len(document.Listings))
	for _, listing := range document.Listings {
		if listing.Harness == "" || listing.SessionID == "" {
			t.Fatalf("fixture listing lacks harness or session ID: %#v", listing)
		}
		if _, ok := pathKeys[listing.PathKey]; !ok {
			t.Fatalf("fixture listing %q references unknown path key %q", listing.SessionID, listing.PathKey)
		}
		if _, err := ingest.NewSessionID(listing.SessionID); err != nil {
			t.Fatalf("fixture listing has invalid session ID %q: %v", listing.SessionID, err)
		}
		if _, duplicate := listingIDs[listing.SessionID]; duplicate {
			t.Fatalf("fixture repeats session ID %q", listing.SessionID)
		}
		listingIDs[listing.SessionID] = struct{}{}
	}

	caseNames := make(map[string]struct{}, len(document.Cases))
	pairs := make(map[string]int, len(document.Cases))
	runCounts := make(map[commitGateRunState]int)
	outcomeCounts := make(map[string]int)
	for _, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" {
			t.Fatal("fixture case has an empty name")
		}
		if _, duplicate := caseNames[testCase.Name]; duplicate {
			t.Fatalf("fixture repeats case name %q", testCase.Name)
		}
		caseNames[testCase.Name] = struct{}{}
		if !testCase.RunState.valid() || !testCase.InitialSelection.valid() || !testCase.ExpectSelection.valid() {
			t.Fatalf("fixture case %q has an unknown run state or selection", testCase.Name)
		}
		for _, key := range append(append([]commitGateKey(nil), testCase.EditKeys...), testCase.AnswerKeys...) {
			if !key.valid() {
				t.Fatalf("fixture case %q has unknown key %q", testCase.Name, key)
			}
		}
		if testCase.ExpectCommit && testCase.ExpectExit {
			t.Fatalf("fixture case %q cannot both commit and exit without saving", testCase.Name)
		}
		outcome := commitGateOutcome(testCase)
		if outcome == "unknown" {
			t.Fatalf("fixture case %q has no supported confirmation outcome", testCase.Name)
		}
		pairs[string(testCase.RunState)+"/"+outcome]++
		runCounts[testCase.RunState]++
		outcomeCounts[outcome]++
	}
	if len(pairs) != expectedKickstartCommitGateCaseCount {
		t.Fatalf("fixture has %d distinct run/outcome pairs, want %d", len(pairs), expectedKickstartCommitGateCaseCount)
	}
	for pair, count := range pairs {
		if count != 1 {
			t.Fatalf("fixture repeats run/outcome pair %q %d times", pair, count)
		}
	}
	if runCounts[commitGateFirstRun] != 6 || runCounts[commitGateLaterRun] != 6 {
		t.Fatalf("fixture run counts = first:%d later:%d, want 6 each", runCounts[commitGateFirstRun], runCounts[commitGateLaterRun])
	}
	if outcomeCounts["suppressed"] != 2 || outcomeCounts["yes"] != 2 || outcomeCounts["no"] != 2 ||
		outcomeCounts["back"] != 2 || outcomeCounts["cancel"] != 2 || outcomeCounts["interrupt"] != 2 {
		t.Fatalf("fixture must cover every confirmation outcome once per run state; got %v", outcomeCounts)
	}
}

func (s commitGateRunState) valid() bool {
	return s == commitGateFirstRun || s == commitGateLaterRun
}

func (s commitGateSelection) valid() bool {
	switch s {
	case commitGateSelectionAll, commitGateSelectionEmpty, commitGateSelectionExplicitParent:
		return true
	default:
		return false
	}
}

func (k commitGateKey) valid() bool {
	switch k {
	case commitGateKeySelectAll, commitGateKeyDown, commitGateKeySpace, commitGateKeyLeft,
		commitGateKeyEnter, commitGateKeyBack, commitGateKeyCancel, commitGateKeyInterrupt:
		return true
	default:
		return false
	}
}

func commitGateOutcome(testCase kickstartCommitGateCase) string {
	if !testCase.ExpectPrompt {
		return "suppressed"
	}
	if len(testCase.AnswerKeys) == 2 && testCase.AnswerKeys[0] == commitGateKeyLeft && testCase.AnswerKeys[1] == commitGateKeyEnter {
		return "yes"
	}
	if len(testCase.AnswerKeys) != 1 {
		return "unknown"
	}
	switch testCase.AnswerKeys[0] {
	case commitGateKeyEnter:
		return "no"
	case commitGateKeyBack:
		return "back"
	case commitGateKeyCancel:
		return "cancel"
	case commitGateKeyInterrupt:
		return "interrupt"
	default:
		return "unknown"
	}
}

func TestKickstartCommitGateCandidateCohortUsesStablePhysicalIdentity(t *testing.T) {
	t.Parallel()
	document := loadKickstartCommitGateDocument(t)
	paths := materializeKickstartGatePaths(t, document.Paths)
	listings := kickstartGateListings(document.Listings, paths)
	source := kickstart.NewScannerTreeSource(
		listings,
		kickstart.WithPathIdentityResolver(ingest.NewPhysicalPathResolver()),
	)

	candidates := source.CommitGateCandidates()
	if len(candidates) != 1 {
		t.Fatalf("commit-gate candidate projects = %d, want 1 physical project", len(candidates))
	}
	candidate := candidates[0]
	physicalPath := ingest.ClonePath(paths["worktree"])
	wantParentID := selectionprojection.ParentProjectID((kickstart.ProjectIdentity{
		Harness:   ingest.Harness(defaults.HarnessClaudeCode),
		ClonePath: physicalPath,
	}).String())
	if candidate.ParentProjectID != wantParentID {
		t.Fatalf("ParentProjectID = %q, want stable ProjectIdentity %q", candidate.ParentProjectID, wantParentID)
	}
	if candidate.ClonePath != physicalPath {
		t.Fatalf("project fallback ClonePath = %q, want resolved path %q", candidate.ClonePath, physicalPath)
	}
	if len(candidate.Descendants) != 2 {
		t.Fatalf("candidate descendants = %d, want complete parent and child cohort", len(candidate.Descendants))
	}
	descendants := append([]selectionprojection.SessionCandidate(nil), candidate.Descendants...)
	sort.Slice(descendants, func(i, j int) bool { return descendants[i].SessionID < descendants[j].SessionID })
	if descendants[0].SessionID != ingest.SessionID(commitGateParentSessionID) || descendants[0].ParentSessionID != "" {
		t.Fatalf("parent descendant identity = %#v", descendants[0])
	}
	if descendants[1].SessionID != ingest.SessionID(commitGateChildSessionID) || descendants[1].ParentSessionID != ingest.SessionID(commitGateParentSessionID) {
		t.Fatalf("nested descendant identity = %#v", descendants[1])
	}
	for _, descendant := range descendants {
		if descendant.ClonePath != physicalPath {
			t.Fatalf("session %q ClonePath = %q, want per-session resolved path %q", descendant.SessionID, descendant.ClonePath, physicalPath)
		}
	}
}

func TestMountedKickstartCommitGateFirstAndLaterRuns(t *testing.T) {
	document := loadKickstartCommitGateDocument(t)
	for _, testCase := range document.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			paths := materializeKickstartGatePaths(t, document.Paths)
			listings := kickstartGateListings(document.Listings, paths)
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			seedKickstartGateConfig(t, configPath, testCase.RunState, testCase.InitialSelection)
			before := snapshotKickstartGateFile(t, configPath)

			var ingestCalls int
			var mounted kickstart.Model
			flowIngest := func(context.Context) (*ftue.IngestResult, error) {
				ingestCalls++
				return &ftue.IngestResult{}, nil
			}
			driveFlow := func(model tea.Model) error {
				var ok bool
				mounted, ok = model.(kickstart.Model)
				if !ok {
					return fmt.Errorf("mounted model has type %T, want kickstart.Model", model)
				}
				mounted = startKickstartGateModel(t, mounted)
				for _, key := range testCase.EditKeys {
					mounted = updateKickstartGateModel(t, mounted, key)
				}
				for index := 0; index < 12 && !mounted.Program().OnReceipt(); index++ {
					mounted = updateKickstartGateModel(t, mounted, commitGateKey("tab"))
				}
				if !mounted.Program().OnReceipt() {
					return fmt.Errorf("kickstart flow did not reach review and save")
				}

				mounted = updateKickstartGateModel(t, mounted, commitGateKeyEnter)
				if got := mounted.Program().ConfirmingNoProjects(); got != testCase.ExpectPrompt {
					return fmt.Errorf("no-project confirmation visible=%t, want %t", got, testCase.ExpectPrompt)
				}
				if testCase.ExpectPrompt {
					if mounted.Program().Confirming() {
						return fmt.Errorf("no-project save used the no-save exit modal")
					}
					view := ansiPattern.ReplaceAllString(mounted.Program().View(), "")
					for _, line := range document.MessageLines {
						if !strings.Contains(view, line) {
							return fmt.Errorf("confirmation does not contain accepted sentence %q; view:\n%s", line, view)
						}
					}
				}
				for _, key := range testCase.AnswerKeys {
					mounted = updateKickstartGateModel(t, mounted, key)
				}
				if testCase.ExpectCommit {
					mounted = updateKickstartGateModel(t, mounted, commitGateKeyEnter)
				}
				return nil
			}

			switch testCase.RunState {
			case commitGateFirstRun:
				configured := config.BaseConfig()
				configured.Selection = kickstartGateSelection(testCase.InitialSelection)
				draft, err := settings.NewDraft(configPath, configured)
				if err != nil {
					t.Fatalf("open first-run draft: %v", err)
				}
				source := kickstart.NewScannerTreeSource(
					listings,
					kickstart.WithPathIdentityResolver(ingest.NewPhysicalPathResolver()),
				)
				model := kickstart.NewModel(kickstart.NewProgram(kickstart.ProgramDeps{
					Theme:            theme.New(theme.ModeDark),
					Draft:            draft,
					Source:           source,
					CommitGate:       settings.NewCommitGateEvaluator(source.CommitGateCandidates()),
					Ingest:           flowIngest,
					AlreadyConnected: true,
					Context:          t.Context(),
				}))
				if err := driveFlow(model); err != nil {
					t.Fatalf("run first mounted kickstart flow: %v", err)
				}
			case commitGateLaterRun:
				deps := defaultKickstartCommandDeps()
				deps.flowIngest = flowIngest
				deps.runFlow = driveFlow
				cmd := mountTestCmd(t, t.TempDir())
				if err := runKickstartFlow(cmd, deps, configPath, ftue.ProviderInventory{}, listings); err != nil {
					t.Fatalf("run later mounted kickstart flow: %v", err)
				}
			default:
				t.Fatalf("unknown run state %q", testCase.RunState)
			}
			program := mounted.Program()
			if program.Committed() != testCase.ExpectCommit {
				t.Fatalf("committed=%t, want %t", program.Committed(), testCase.ExpectCommit)
			}
			if program.Exited() != testCase.ExpectExit {
				t.Fatalf("exited=%t, want %t", program.Exited(), testCase.ExpectExit)
			}
			wantIngestCalls := 0
			if testCase.ExpectCommit {
				wantIngestCalls = 1
			}
			if ingestCalls != wantIngestCalls {
				t.Fatalf("ingest calls=%d, want %d", ingestCalls, wantIngestCalls)
			}
			if testCase.ExpectCommit {
				assertKickstartGateCommittedSelection(t, configPath, testCase.ExpectSelection)
				return
			}
			after := snapshotKickstartGateFile(t, configPath)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("declined or interrupted confirmation changed config bytes\n before=%#v\n after=%#v", before, after)
			}
		})
	}
}

func materializeKickstartGatePaths(t *testing.T, fixtures []kickstartGatePath) map[string]string {
	t.Helper()
	root := t.TempDir()
	paths := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		path := filepath.Join(root, fixture.Key)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("create physical path %q: %v", fixture.Key, err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("resolve physical path %q: %v", fixture.Key, err)
		}
		paths[fixture.Key], err = filepath.Abs(resolved)
		if err != nil {
			t.Fatalf("make physical path %q absolute: %v", fixture.Key, err)
		}
	}
	return paths
}

func kickstartGateListings(fixtures []kickstartGateListing, paths map[string]string) []ftue.SessionListing {
	listings := make([]ftue.SessionListing, 0, len(fixtures))
	for _, fixture := range fixtures {
		listings = append(listings, ftue.SessionListing{
			Harness:     fixture.Harness,
			ProjectName: fixture.ProjectName,
			GitRemote:   fixture.GitRemote,
			Branch:      fixture.Branch,
			Title:       fixture.Title,
			SessionID:   fixture.SessionID,
			SubagentIDs: append([]string(nil), fixture.SubagentIDs...),
			WorkingDir:  paths[fixture.PathKey],
		})
	}
	return listings
}

func seedKickstartGateConfig(
	t *testing.T,
	path string,
	runState commitGateRunState,
	selection commitGateSelection,
) {
	t.Helper()
	if runState == commitGateFirstRun {
		return
	}
	configured := config.BaseConfig()
	configured.Selection = kickstartGateSelection(selection)
	if err := config.SaveAtomic(path, configured); err != nil {
		t.Fatalf("seed later-run config: %v", err)
	}
}

func kickstartGateSelection(selection commitGateSelection) config.SelectionConfig {
	switch selection {
	case commitGateSelectionAll:
		return config.SelectionConfig{Mode: config.SelectionModeAll, AutoIngestNewBranches: true}
	case commitGateSelectionExplicitParent:
		return config.SelectionConfig{
			Mode: config.SelectionModeSelected,
			Harnesses: map[string]config.SelectionHarnessConfig{
				string(defaults.HarnessClaudeCode): {Sessions: []string{commitGateParentSessionID}},
			},
		}
	case commitGateSelectionEmpty:
		return config.SelectionConfig{Mode: config.SelectionModeSelected}
	default:
		return config.SelectionConfig{}
	}
}

func startKickstartGateModel(t *testing.T, model kickstart.Model) kickstart.Model {
	t.Helper()
	model = settleKickstartGateModel(t, model, model.Init())
	if model.Program().Phase() == kickstart.PhaseOAuth {
		model = updateKickstartGateModel(t, model, commitGateKeyEnter)
	}
	if model.Program().Phase() != kickstart.PhaseFlow {
		t.Fatalf("mounted kickstart phase=%s, want flow", model.Program().Phase())
	}
	return model
}

func updateKickstartGateModel(t *testing.T, model kickstart.Model, key commitGateKey) kickstart.Model {
	t.Helper()
	next, cmd := model.Update(kickstartGateKeyMessage(t, key))
	updated, ok := next.(kickstart.Model)
	if !ok {
		t.Fatalf("updated model has type %T, want kickstart.Model", next)
	}
	return settleKickstartGateModel(t, updated, cmd)
}

func settleKickstartGateModel(t *testing.T, model kickstart.Model, first tea.Cmd) kickstart.Model {
	t.Helper()
	queue := []tea.Cmd{first}
	for iterations := 0; len(queue) > 0 && iterations < 256; iterations++ {
		cmd := queue[0]
		queue = queue[1:]
		if cmd == nil {
			continue
		}
		message := cmd()
		if batch, ok := message.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if message == nil {
			continue
		}
		next, follow := model.Update(message)
		updated, ok := next.(kickstart.Model)
		if !ok {
			t.Fatalf("async update model has type %T, want kickstart.Model", next)
		}
		model = updated
		if follow != nil {
			queue = append(queue, follow)
		}
		if model.Program().Phase() == kickstart.PhaseDone {
			return model
		}
	}
	if len(queue) > 0 {
		t.Fatal("mounted kickstart command queue did not settle")
	}
	return model
}

func kickstartGateKeyMessage(t *testing.T, key commitGateKey) tea.KeyPressMsg {
	t.Helper()
	switch key {
	case commitGateKeySelectAll:
		return tea.KeyPressMsg{Code: 'a', Text: "a"}
	case commitGateKeyDown:
		return tea.KeyPressMsg{Code: 'j', Text: "j"}
	case commitGateKeySpace:
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case commitGateKeyLeft:
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case commitGateKeyEnter:
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case commitGateKeyBack:
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case commitGateKeyCancel:
		return tea.KeyPressMsg{Code: 'q', Text: "q"}
	case commitGateKeyInterrupt:
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	default:
		t.Fatalf("no key message for fixture token %q", key)
		return tea.KeyPressMsg{}
	}
}

type kickstartGateFileSnapshot struct {
	Exists bool
	Bytes  []byte
}

func snapshotKickstartGateFile(t *testing.T, path string) kickstartGateFileSnapshot {
	t.Helper()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return kickstartGateFileSnapshot{}
	}
	if err != nil {
		t.Fatalf("snapshot config %q: %v", path, err)
	}
	return kickstartGateFileSnapshot{Exists: true, Bytes: content}
}

func assertKickstartGateCommittedSelection(
	t *testing.T,
	path string,
	want commitGateSelection,
) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed config: %v", err)
	}
	configured, err := config.Parse(content)
	if err != nil {
		t.Fatalf("parse committed config: %v", err)
	}
	got := normalizeKickstartGateSelection(configured.Selection)
	expected := normalizeKickstartGateSelection(kickstartGateSelection(want))
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("committed selection mismatch\n got: %#v\nwant: %#v", got, expected)
	}
}

func normalizeKickstartGateSelection(selection config.SelectionConfig) config.SelectionConfig {
	selection.DeprecatedProviders = nil
	if len(selection.Harnesses) == 0 {
		selection.Harnesses = nil
		return selection
	}
	for harness, configured := range selection.Harnesses {
		if len(configured.Sessions) == 0 {
			configured.Sessions = nil
		}
		if len(configured.Projects) == 0 {
			configured.Projects = nil
		}
		selection.Harnesses[harness] = configured
	}
	return selection
}
