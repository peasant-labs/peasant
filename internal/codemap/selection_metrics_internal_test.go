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

	// Every stored session contributes a metric row: selection is ingest-only
	// metadata and never narrows this baseline.
	if fixtureCase.Expected.VisibleMetricCount != len(fixtureCase.Sessions) {
		return fmt.Errorf("visibleMetricCount = %d, want every stored session (%d) to contribute", fixtureCase.Expected.VisibleMetricCount, len(fixtureCase.Sessions))
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
		if _, exists := sessionsByID[sessionID]; !exists {
			return fmt.Errorf("bound session %q is absent from sessions", sessionID)
		}
	}
	if _, exists := sessionsByID[fixtureCase.PromoteSessionID]; !exists {
		return fmt.Errorf("promoted session %q is absent from sessions", fixtureCase.PromoteSessionID)
	}
	if strings.TrimSpace(fixtureCase.Expected.SignalKind) == "" {
		return errors.New("expected signal kind must be nonempty")
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
	t.Parallel()
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
	hiddenMetric := fixture
	hiddenMetric.Cases = append([]selectionMetricsCase(nil), fixture.Cases...)
	hiddenMetric.Cases[0].Expected.VisibleMetricCount--
	if err := validateSelectionMetricsFixture(hiddenMetric); err == nil || !strings.Contains(err.Error(), "every stored session") {
		t.Fatalf("hidden-metric mutation error = %v, want every-stored-session rejection", err)
	}
}

// TestProjectDataIncludesEveryStoredSessionMetric proves the Map/Review data
// path keeps every stored session's metrics regardless of the saved selection:
// the selection only controls future ingestion, so the unusual-signal baseline
// is computed over the whole project.
func TestProjectDataIncludesEveryStoredSessionMetric(t *testing.T) {
	t.Parallel()
	fixture := loadSelectionMetricsFixture(t)
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			database := storetest.Open(t)
			seedSelectionMetricsCase(t, database, fixtureCase)

			service := NewService(database, nil, codegraph.NewGraphBuilder())
			projectHash, _ := schema.NewProjectHash(fixtureCase.Project.Hash)
			project, err := service.loadProjectData(context.Background(), projectHash)
			if err != nil {
				t.Fatalf("loadProjectData: %v", err)
			}
			if len(project.metrics) != fixtureCase.Expected.VisibleMetricCount {
				t.Fatalf("metric count = %d, want %d (every stored session)", len(project.metrics), fixtureCase.Expected.VisibleMetricCount)
			}
			for _, session := range fixtureCase.Sessions {
				metric, present := project.metrics[session.ID]
				if !present || metric.retryLoops == nil || *metric.retryLoops != session.RetryLoops {
					t.Fatalf("session %q metric = %+v, present=%t, want retry-loop count %d", session.ID, metric, present, session.RetryLoops)
				}
			}

			bindings := boundSelectionMetricSessions(fixtureCase.BoundSessionIDs)
			signals := unusualSignals(project, bindings)
			assertSelectionMetricSignals(t, signals, fixtureCase.Expected.InitialSignalCount, fixtureCase.Expected.SignalKind, fixtureCase.Expected.InitialPerChange, fixtureCase.Expected.InitialPerProject)
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
