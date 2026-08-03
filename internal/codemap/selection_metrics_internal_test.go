package codemap

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/selection_metrics.yaml
var selectionMetricsYAML []byte

type selectionMetricsFixture struct {
	ExpectedCaseCount int                    `yaml:"expectedCaseCount"`
	RequiredNames     []string               `yaml:"requiredNames"`
	Cases             []selectionMetricsCase `yaml:"cases"`
}

type selectionMetricsCase struct {
	ID               string                    `yaml:"id"`
	Name             string                    `yaml:"name"`
	Project          selectionMetricsProject   `yaml:"project"`
	Selection        config.SelectionConfig    `yaml:"selection"`
	Sessions         []selectionMetricsSession `yaml:"sessions"`
	BoundSessionIDs  []string                  `yaml:"boundSessionIDs"`
	PromoteSessionID string                    `yaml:"promoteSessionID"`
	Expected         selectionMetricsExpected  `yaml:"expected"`
}

type selectionMetricsProject struct {
	Hash string `yaml:"hash"`
	Name string `yaml:"name"`
	CWD  string `yaml:"cwd"`
}

type selectionMetricsSession struct {
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	Harness    string `yaml:"harness"`
	RetryLoops int    `yaml:"retryLoops"`
}

type selectionMetricsExpected struct {
	VisibleMetricCount  int     `yaml:"visibleMetricCount"`
	InitialSignalCount  int     `yaml:"initialSignalCount"`
	PromotedSignalCount int     `yaml:"promotedSignalCount"`
	SignalKind          string  `yaml:"signalKind"`
	InitialPerChange    float64 `yaml:"initialPerChange"`
	InitialPerProject   float64 `yaml:"initialPerProject"`
	HiddenRetryLoops    int     `yaml:"hiddenRetryLoops"`
}

func decodeSelectionMetricsFixture(source []byte) (selectionMetricsFixture, error) {
	var fixture selectionMetricsFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return selectionMetricsFixture{}, fmt.Errorf("decode selection metrics fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return selectionMetricsFixture{}, fmt.Errorf("selection metrics fixture must contain exactly one YAML document: %v", err)
	}
	if err := validateSelectionMetricsFixture(fixture); err != nil {
		return selectionMetricsFixture{}, err
	}
	return fixture, nil
}

func validateSelectionMetricsFixture(fixture selectionMetricsFixture) error {
	if fixture.ExpectedCaseCount <= 0 || len(fixture.Cases) != fixture.ExpectedCaseCount || len(fixture.RequiredNames) != fixture.ExpectedCaseCount {
		return fmt.Errorf("selection metrics fixture case inventory mismatch: expectedCaseCount=%d requiredNames=%d cases=%d", fixture.ExpectedCaseCount, len(fixture.RequiredNames), len(fixture.Cases))
	}
	requiredNames := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if strings.TrimSpace(name) == "" {
			return errors.New("selection metrics fixture has an empty required name")
		}
		if _, duplicate := requiredNames[name]; duplicate {
			return fmt.Errorf("selection metrics fixture repeats required name %q", name)
		}
		requiredNames[name] = struct{}{}
	}

	caseIDs := make(map[string]struct{}, len(fixture.Cases))
	caseNames := make(map[string]struct{}, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if strings.TrimSpace(fixtureCase.ID) == "" || strings.TrimSpace(fixtureCase.Name) == "" {
			return errors.New("selection metrics fixture has a case with an empty ID or name")
		}
		if _, duplicate := caseIDs[fixtureCase.ID]; duplicate {
			return fmt.Errorf("selection metrics fixture repeats case ID %q", fixtureCase.ID)
		}
		if _, duplicate := caseNames[fixtureCase.Name]; duplicate {
			return fmt.Errorf("selection metrics fixture repeats case name %q", fixtureCase.Name)
		}
		caseIDs[fixtureCase.ID] = struct{}{}
		caseNames[fixtureCase.Name] = struct{}{}
		if _, required := requiredNames[fixtureCase.Name]; !required {
			return fmt.Errorf("selection metrics fixture case %q is not in requiredNames", fixtureCase.Name)
		}
		if err := validateSelectionMetricsCase(fixtureCase); err != nil {
			return fmt.Errorf("selection metrics fixture case %q: %w", fixtureCase.Name, err)
		}
	}
	for name := range requiredNames {
		if _, present := caseNames[name]; !present {
			return fmt.Errorf("selection metrics fixture is missing required name %q", name)
		}
	}
	return nil
}

func validateSelectionMetricsCase(fixtureCase selectionMetricsCase) error {
	if _, err := schema.NewProjectHash(fixtureCase.Project.Hash); err != nil {
		return fmt.Errorf("invalid project hash: %w", err)
	}
	if strings.TrimSpace(fixtureCase.Project.Name) == "" || strings.TrimSpace(fixtureCase.Project.CWD) == "" {
		return errors.New("project name and cwd must be nonempty")
	}
	if fixtureCase.Selection.Mode != config.SelectionModeSelected {
		return fmt.Errorf("selection mode = %q, want selected", fixtureCase.Selection.Mode)
	}
	if _, err := sessionvisibility.New(fixtureCase.Selection); err != nil {
		return fmt.Errorf("invalid selection: %w", err)
	}
	if len(fixtureCase.Sessions) < unusualMinProjectSessions || len(fixtureCase.BoundSessionIDs) < unusualMinChangeSessions {
		return errors.New("session inventory cannot exercise unusual-signal thresholds")
	}

	sessionsByID := make(map[string]selectionMetricsSession, len(fixtureCase.Sessions))
	sessionNames := make(map[string]struct{}, len(fixtureCase.Sessions))
	for _, session := range fixtureCase.Sessions {
		if strings.TrimSpace(session.ID) == "" || strings.TrimSpace(session.Name) == "" {
			return errors.New("session has an empty ID or name")
		}
		if _, err := schema.NewSessionID(session.ID); err != nil {
			return fmt.Errorf("invalid session ID %q: %w", session.ID, err)
		}
		if strings.TrimSpace(session.Harness) == "" || session.RetryLoops < 0 {
			return fmt.Errorf("session %q has an invalid harness or retry-loop count", session.ID)
		}
		if _, duplicate := sessionsByID[session.ID]; duplicate {
			return fmt.Errorf("duplicate session ID %q", session.ID)
		}
		if _, duplicate := sessionNames[session.Name]; duplicate {
			return fmt.Errorf("duplicate session name %q", session.Name)
		}
		sessionsByID[session.ID] = session
		sessionNames[session.Name] = struct{}{}
	}

	selectedIDs := make(map[string]struct{})
	for harness, selected := range fixtureCase.Selection.Harnesses {
		for _, sessionID := range selected.Sessions {
			session, exists := sessionsByID[sessionID]
			if !exists {
				return fmt.Errorf("selected session %q is absent from sessions", sessionID)
			}
			if session.Harness != harness {
				return fmt.Errorf("selected session %q harness = %q, want %q", sessionID, session.Harness, harness)
			}
			if _, duplicate := selectedIDs[sessionID]; duplicate {
				return fmt.Errorf("selected session %q is repeated", sessionID)
			}
			selectedIDs[sessionID] = struct{}{}
		}
	}
	if len(selectedIDs) != fixtureCase.Expected.VisibleMetricCount {
		return fmt.Errorf("selected session count = %d, want visibleMetricCount %d", len(selectedIDs), fixtureCase.Expected.VisibleMetricCount)
	}
	boundIDs := make(map[string]struct{}, len(fixtureCase.BoundSessionIDs))
	for _, sessionID := range fixtureCase.BoundSessionIDs {
		if strings.TrimSpace(sessionID) == "" {
			return errors.New("bound session ID must be nonempty")
		}
		if _, duplicate := boundIDs[sessionID]; duplicate {
			return fmt.Errorf("bound session ID %q is repeated", sessionID)
		}
		boundIDs[sessionID] = struct{}{}
		if _, selected := selectedIDs[sessionID]; !selected {
			return fmt.Errorf("bound session %q is not selected", sessionID)
		}
	}
	promoted, exists := sessionsByID[fixtureCase.PromoteSessionID]
	if !exists {
		return fmt.Errorf("promoted session %q is absent from sessions", fixtureCase.PromoteSessionID)
	}
	if _, alreadySelected := selectedIDs[fixtureCase.PromoteSessionID]; alreadySelected {
		return fmt.Errorf("promoted session %q is already selected", fixtureCase.PromoteSessionID)
	}
	if promoted.RetryLoops != fixtureCase.Expected.HiddenRetryLoops {
		return fmt.Errorf("promoted session retry-loop count = %d, want hidden expected count %d", promoted.RetryLoops, fixtureCase.Expected.HiddenRetryLoops)
	}
	var maximumVisibleRetryLoops int
	visibleBaselineSet := false
	for sessionID := range selectedIDs {
		if !visibleBaselineSet || sessionsByID[sessionID].RetryLoops > maximumVisibleRetryLoops {
			maximumVisibleRetryLoops = sessionsByID[sessionID].RetryLoops
			visibleBaselineSet = true
		}
	}
	if promoted.RetryLoops <= maximumVisibleRetryLoops {
		return fmt.Errorf("promoted session retry-loop count = %d, must exceed visible baseline maximum %d", promoted.RetryLoops, maximumVisibleRetryLoops)
	}
	if strings.TrimSpace(fixtureCase.Expected.SignalKind) == "" {
		return errors.New("expected signal kind must be nonempty")
	}
	if fixtureCase.Expected.InitialSignalCount == fixtureCase.Expected.PromotedSignalCount {
		return errors.New("promotion must change the observable unusual-signal outcome")
	}
	return nil
}

func loadSelectionMetricsFixture(t *testing.T) selectionMetricsFixture {
	t.Helper()
	fixture, err := decodeSelectionMetricsFixture(selectionMetricsYAML)
	if err != nil {
		t.Fatalf("load selection metrics fixture: %v", err)
	}
	return fixture
}

func TestSelectionMetricsFixtureGuards(t *testing.T) {
	fixture := loadSelectionMetricsFixture(t)

	unknown := bytes.Replace(selectionMetricsYAML, []byte("expectedCaseCount: 1"), []byte("expectedCaseCount: 1\nunexpected: true"), 1)
	if _, err := decodeSelectionMetricsFixture(unknown); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict rejection", err)
	}
	trailing := append(append([]byte{}, selectionMetricsYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeSelectionMetricsFixture(trailing); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document mutation error = %v, want strict rejection", err)
	}
	duplicate := bytes.Replace(selectionMetricsYAML, []byte("id: 44444444-4444-4444-4444-444444444444"), []byte("id: 11111111-1111-1111-1111-111111111111"), 1)
	if _, err := decodeSelectionMetricsFixture(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate session ID") {
		t.Fatalf("duplicate mutation error = %v, want duplicate rejection", err)
	}
	renamed := bytes.Replace(selectionMetricsYAML, []byte("name: selected_hidden_extreme_baseline"), []byte("name: renamed_without_inventory_update"), 1)
	if _, err := decodeSelectionMetricsFixture(renamed); err == nil || !strings.Contains(err.Error(), "not in requiredNames") {
		t.Fatalf("count-preserving rename error = %v, want exact-inventory rejection", err)
	}
	deleted := fixture
	deleted.Cases = append([]selectionMetricsCase(nil), fixture.Cases[1:]...)
	if err := validateSelectionMetricsFixture(deleted); err == nil || !strings.Contains(err.Error(), "case inventory mismatch") {
		t.Fatalf("deletion mutation error = %v, want inventory rejection", err)
	}
	emptySignalKind := fixture
	emptySignalKind.Cases = append([]selectionMetricsCase(nil), fixture.Cases...)
	emptySignalKind.Cases[0].Expected.SignalKind = ""
	if err := validateSelectionMetricsFixture(emptySignalKind); err == nil || !strings.Contains(err.Error(), "expected signal kind must be nonempty") {
		t.Fatalf("empty signal-kind mutation error = %v, want expectation rejection", err)
	}
	nonExtreme := fixture
	nonExtreme.Cases = append([]selectionMetricsCase(nil), fixture.Cases...)
	nonExtreme.Cases[0].Sessions = append([]selectionMetricsSession(nil), fixture.Cases[0].Sessions...)
	mutatedCase := &nonExtreme.Cases[0]
	var replacementRetryLoops int
	for _, session := range mutatedCase.Sessions {
		if session.ID == mutatedCase.BoundSessionIDs[0] {
			replacementRetryLoops = session.RetryLoops
			break
		}
	}
	for index := range mutatedCase.Sessions {
		if mutatedCase.Sessions[index].ID == mutatedCase.PromoteSessionID {
			mutatedCase.Sessions[index].RetryLoops = replacementRetryLoops
			mutatedCase.Expected.HiddenRetryLoops = mutatedCase.Sessions[index].RetryLoops
			break
		}
	}
	if err := validateSelectionMetricsFixture(nonExtreme); err == nil || !strings.Contains(err.Error(), "must exceed visible baseline maximum") {
		t.Fatalf("non-extreme mutation error = %v, want relational-extremity rejection", err)
	}
}

func TestSelectedMetricsExcludeHiddenExtremeFromUnusualSignalBaseline(t *testing.T) {
	fixture := loadSelectionMetricsFixture(t)
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			database := storetest.Open(t)
			seedSelectionMetricsCase(t, database, fixtureCase)

			policy, err := sessionvisibility.New(fixtureCase.Selection)
			if err != nil {
				t.Fatalf("selection policy: %v", err)
			}
			service := NewService(database, nil, codegraph.NewGraphBuilder(), policy)
			projectHash, _ := schema.NewProjectHash(fixtureCase.Project.Hash)
			project, err := service.loadProjectData(context.Background(), projectHash)
			if err != nil {
				t.Fatalf("loadProjectData: %v", err)
			}
			if len(project.metrics) != fixtureCase.Expected.VisibleMetricCount {
				t.Fatalf("visible metric count = %d, want %d", len(project.metrics), fixtureCase.Expected.VisibleMetricCount)
			}
			if _, leaked := project.metrics[fixtureCase.PromoteSessionID]; leaked {
				t.Fatalf("hidden session %q with retry-loop count %d leaked into project metrics", fixtureCase.PromoteSessionID, fixtureCase.Expected.HiddenRetryLoops)
			}

			bindings := boundSelectionMetricSessions(fixtureCase.BoundSessionIDs)
			initialSignals := unusualSignals(project, bindings)
			assertSelectionMetricSignals(t, initialSignals, fixtureCase.Expected.InitialSignalCount, fixtureCase.Expected.SignalKind, fixtureCase.Expected.InitialPerChange, fixtureCase.Expected.InitialPerProject)

			promotedSelection := cloneSelectionWithSession(fixtureCase.Selection, fixtureCase.Sessions, fixtureCase.PromoteSessionID)
			promotedPolicy, err := sessionvisibility.New(promotedSelection)
			if err != nil {
				t.Fatalf("promoted selection policy: %v", err)
			}
			promotedService := NewService(database, nil, codegraph.NewGraphBuilder(), promotedPolicy)
			promotedProject, err := promotedService.loadProjectData(context.Background(), projectHash)
			if err != nil {
				t.Fatalf("loadProjectData after promotion: %v", err)
			}
			promotedMetric, present := promotedProject.metrics[fixtureCase.PromoteSessionID]
			if !present || promotedMetric.retryLoops == nil || *promotedMetric.retryLoops != fixtureCase.Expected.HiddenRetryLoops {
				t.Fatalf("promoted hidden metric = %+v, present=%t, want retry-loop count %d", promotedMetric, present, fixtureCase.Expected.HiddenRetryLoops)
			}
			promotedSignals := unusualSignals(promotedProject, bindings)
			assertSelectionMetricSignalCount(t, promotedSignals, fixtureCase.Expected.PromotedSignalCount)
			if len(initialSignals) == len(promotedSignals) {
				t.Fatalf("promoting hidden extreme did not change unusual-signal outcome: before=%+v after=%+v", initialSignals, promotedSignals)
			}
		})
	}
}

func seedSelectionMetricsCase(t *testing.T, database *store.Store, fixtureCase selectionMetricsCase) {
	t.Helper()
	projectHash, _ := schema.NewProjectHash(fixtureCase.Project.Hash)
	for index, session := range fixtureCase.Sessions {
		sessionID, _ := schema.NewSessionID(session.ID)
		startMs := testutil.TestSessionStartTime.UnixMilli() + int64(index)*1000
		endMs := startMs + 500
		ingestedMs := endMs + 1
		metadata := &schema.UnifiedMetadata{
			SessionID:    sessionID,
			ModelHarness: defaults.Harness(session.Harness),
			Model:        testutil.TestModel,
			HostSlug:     schema.HostSlug(testutil.TestHostSlug),
			Project: schema.ProjectContext{
				Hash:     projectHash,
				Name:     fixtureCase.Project.Name,
				FilePath: fixtureCase.Project.CWD,
			},
			Timestamp: schema.TimestampInfo{Start: startMs, End: endMs, Ingested: &ingestedMs},
			Source:    schema.SourceInfo{FilePath: "/selection-metrics.jsonl", Format: schema.SourceFormatJSONL},
		}
		if err := database.InsertSessions(context.Background(), []ingest.StoreEntry{{Metadata: metadata}}); err != nil {
			t.Fatalf("seed session %q: %v", session.ID, err)
		}
		retryLoops := session.RetryLoops
		computeVersion := 1
		metrics := &ingest.SessionMetrics{SessionID: sessionID}
		metrics.RetryLoops = &retryLoops
		metrics.ComputeVersion = &computeVersion
		if err := database.SaveMetrics(context.Background(), metrics); err != nil {
			t.Fatalf("seed metrics for %q: %v", session.ID, err)
		}
	}
}

func boundSelectionMetricSessions(sessionIDs []string) []sessionBinding {
	bindings := make([]sessionBinding, len(sessionIDs))
	for index, sessionID := range sessionIDs {
		bindings[index] = sessionBinding{sessionID: sessionID, binding: schema.ChangeBindingBound}
	}
	return bindings
}

func cloneSelectionWithSession(selection config.SelectionConfig, sessions []selectionMetricsSession, sessionID string) config.SelectionConfig {
	promoted := selection
	promoted.Harnesses = make(map[string]config.SelectionHarnessConfig, len(selection.Harnesses))
	for harness, selected := range selection.Harnesses {
		selected.Sessions = append([]string(nil), selected.Sessions...)
		selected.Projects = append([]config.ProjectSelection(nil), selected.Projects...)
		promoted.Harnesses[harness] = selected
	}
	for _, session := range sessions {
		if session.ID == sessionID {
			selected := promoted.Harnesses[session.Harness]
			selected.Sessions = append(selected.Sessions, sessionID)
			promoted.Harnesses[session.Harness] = selected
			return promoted
		}
	}
	return promoted
}

func assertSelectionMetricSignals(t *testing.T, signals []schema.UnusualSignal, count int, signalKind string, perChange, perProject float64) {
	t.Helper()
	if len(signals) != count {
		t.Fatalf("unusual signals = %+v, want count %d", signals, count)
	}
	if count == 0 {
		return
	}
	for _, signal := range signals {
		if signal.Kind != signalKind || signal.PerChange != perChange || signal.PerProject != perProject {
			t.Fatalf("unusual signals = %+v, want %d %q signals with perChange=%v perProject=%v", signals, count, signalKind, perChange, perProject)
		}
	}
}

func assertSelectionMetricSignalCount(t *testing.T, signals []schema.UnusualSignal, count int) {
	t.Helper()
	if len(signals) != count {
		t.Fatalf("unusual signals = %+v, want count %d", signals, count)
	}
}
