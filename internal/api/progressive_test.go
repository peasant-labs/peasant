package api

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

var progressiveProjectHash = schema.ProjectHash("1111111111111111111111111111111111111111111111111111111111111111")
var progressiveMockProjectHash = schema.ProjectHash("2222222222222222222222222222222222222222222222222222222222222222")
var progressiveRealProjectHash = schema.ProjectHash("3333333333333333333333333333333333333333333333333333333333333333")

type mockDataProvider struct {
	sessions          []ingest.Session
	dashboard         *DashboardPayload
	trends            *TrendsPayload
	summaries         []SessionSummary
	quality           []QualitySession
	mapGraph          *MapGraphPayload
	reviewList        *ReviewListPayload
	projectSummaries  *codemap.ProjectSummariesResult
	projectResolution *schema.ProjectResolutionPayload
	returnsErr        bool
}

func (m *mockDataProvider) Sessions(ctx context.Context) ([]ingest.Session, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return m.sessions, nil
}

func (m *mockDataProvider) SessionSummaries(ctx context.Context) ([]SessionSummary, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return m.summaries, nil
}

func (m *mockDataProvider) SessionByID(ctx context.Context, id string) (*ingest.Session, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	for i := range m.sessions {
		if string(m.sessions[i].ID) == id {
			return &m.sessions[i], nil
		}
	}
	return nil, nil
}

func (m *mockDataProvider) DashboardMetrics(ctx context.Context) (*DashboardPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return m.dashboard, nil
}

func (m *mockDataProvider) TrendsData(ctx context.Context) (*TrendsPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return m.trends, nil
}

func (m *mockDataProvider) QualitySessions(ctx context.Context, f QualityFilter) ([]QualitySession, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return m.quality, nil
}

func (m *mockDataProvider) AnnotationsForSession(_ context.Context, _ string) ([]schema.AnnotationSummary, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return nil, nil
}

func (m *mockDataProvider) ProjectFamiliarity(_ context.Context, projectHash schema.ProjectHash) (*schema.FamiliarityPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return &schema.FamiliarityPayload{ProjectHash: projectHash}, nil
}

func (m *mockDataProvider) ChildSessionsForParent(_ context.Context, _ string) ([]schema.ChildSessionRef, error) {
	return nil, nil
}

func (m *mockDataProvider) ProjectSummaries(_ context.Context) (*codemap.ProjectSummariesResult, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	if m.projectSummaries != nil {
		return m.projectSummaries, nil
	}
	return &codemap.ProjectSummariesResult{Projects: []schema.ProjectSummary{}}, nil
}

func (m *mockDataProvider) ResolveProject(_ context.Context, project string) (*schema.ProjectResolutionPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	if m.projectResolution != nil {
		return m.projectResolution, nil
	}
	return &schema.ProjectResolutionPayload{Project: project, ProjectHash: progressiveProjectHash}, nil
}

func (m *mockDataProvider) MapGraph(_ context.Context, projectHash schema.ProjectHash, _ string) (*MapGraphPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	if m.mapGraph != nil {
		return m.mapGraph, nil
	}
	return schema.NewMapGraphPayload(projectHash), nil
}

func (m *mockDataProvider) MapNodeDetail(_ context.Context, _ schema.ProjectHash, path string) (*MapNodeDetailPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return schema.NewMapNodeDetailPayload(path), nil
}

func (m *mockDataProvider) ProjectTasks(_ context.Context, projectHash schema.ProjectHash, file string) (*ProjectTasksPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	p := schema.NewProjectTasksPayload(projectHash)
	p.FileFilter = file
	return p, nil
}

func (m *mockDataProvider) ReviewChanges(_ context.Context, projectHash schema.ProjectHash) (*ReviewListPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	if m.reviewList != nil {
		return m.reviewList, nil
	}
	return schema.NewReviewListPayload(projectHash), nil
}

func (m *mockDataProvider) ChangeDetail(_ context.Context, _ schema.ProjectHash, branch string) (*ChangeDetailPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return schema.NewChangeDetailPayload(branch), nil
}

func (m *mockDataProvider) ChangeDiff(_ context.Context, _ schema.ProjectHash, branch, file string) (*schema.ChangeDiffPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return schema.NewChangeDiffPayload(branch, file), nil
}

func (m *mockDataProvider) Search(_ context.Context, query string, _ int) (*schema.SearchPayload, error) {
	if m.returnsErr {
		return nil, context.DeadlineExceeded
	}
	return schema.NewSearchPayload(query), nil
}

func TestProgressiveProvider_MockEnabled_RoutesToMock(t *testing.T) {
	t.Parallel()

	mockProv := &mockDataProvider{
		sessions:  []ingest.Session{{ID: "mock-session-1"}},
		dashboard: &DashboardPayload{TotalSessions: 1},
	}
	realProv := &mockDataProvider{
		sessions:  []ingest.Session{{ID: "real-session-1"}},
		dashboard: &DashboardPayload{TotalSessions: 10},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil, // nil means all sections mocked
			},
		},
	}

	prov := NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	ctx := context.Background()
	sessions, err := prov.Sessions(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sessions) != 1 || string(sessions[0].ID) != "mock-session-1" {
		t.Errorf("expected mock session, got %v", sessions)
	}

	metrics, err := prov.DashboardMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.TotalSessions != 1 {
		t.Errorf("expected mock dashboard (1), got %d", metrics.TotalSessions)
	}
}

func TestProgressiveProvider_MockDisabled_RoutesToReal(t *testing.T) {
	t.Parallel()

	mockProv := &mockDataProvider{
		sessions:  []ingest.Session{{ID: "mock-session-1"}},
		dashboard: &DashboardPayload{TotalSessions: 1},
	}
	realProv := &mockDataProvider{
		sessions:  []ingest.Session{{ID: "real-session-1"}},
		dashboard: &DashboardPayload{TotalSessions: 10},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: false,
			},
		},
	}

	prov := NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	ctx := context.Background()
	sessions, err := prov.Sessions(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sessions) != 1 || string(sessions[0].ID) != "real-session-1" {
		t.Errorf("expected real session, got %v", sessions)
	}

	metrics, err := prov.DashboardMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.TotalSessions != 10 {
		t.Errorf("expected real dashboard (10), got %d", metrics.TotalSessions)
	}
}

func TestProgressiveProvider_NilRealProvider_ReturnsError(t *testing.T) {
	t.Parallel()

	mockProv := &mockDataProvider{
		sessions:  []ingest.Session{{ID: "mock-session-1"}},
		dashboard: &DashboardPayload{TotalSessions: 1},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: false,
			},
		},
	}

	prov := NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, nil)

	ctx := context.Background()
	_, err := prov.Sessions(ctx)
	if err == nil {
		t.Error("expected error when real provider is nil and mock disabled")
	}
}

func TestProgressiveProvider_NilMockProvider_FallsBackToReal(t *testing.T) {
	t.Parallel()

	realProv := &mockDataProvider{
		sessions:  []ingest.Session{{ID: "real-session-1"}},
		dashboard: &DashboardPayload{TotalSessions: 10},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     []defaults.MockSection{defaults.MockSections.Dashboard},
			},
		},
	}

	prov := NewProgressiveProvider(cfg, defaults.MockComponents.Web, nil, realProv)

	ctx := context.Background()
	metrics, err := prov.DashboardMetrics(ctx)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// When mock provider is nil, isMocked returns false, falls back to real
	if metrics.TotalSessions != 10 {
		t.Errorf("expected real dashboard (10), got %d", metrics.TotalSessions)
	}
}

func TestProgressiveProvider_ComponentSpecific(t *testing.T) {
	t.Parallel()

	tuiMockProv := &mockDataProvider{sessions: []ingest.Session{{ID: "tui-mock"}}}
	webMockProv := &mockDataProvider{sessions: []ingest.Session{{ID: "web-mock"}}}
	realProv := &mockDataProvider{sessions: []ingest.Session{{ID: "real"}}}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				TUI:     []defaults.MockSection{defaults.MockSections.Sessions},
				Web:     nil, // empty means all mocked for Web
			},
		},
	}

	// TUI component - Sessions is mocked, but Dashboard is not
	tuiProv := NewProgressiveProvider(cfg, defaults.MockComponents.TUI, tuiMockProv, realProv)
	sessions, _ := tuiProv.Sessions(context.Background())
	if len(sessions) != 1 || string(sessions[0].ID) != "tui-mock" {
		t.Errorf("TUI: expected tui-mock, got %v", sessions)
	}

	// Web component - nil sections means all mocked
	webProv := NewProgressiveProvider(cfg, defaults.MockComponents.Web, webMockProv, realProv)
	sessions, _ = webProv.Sessions(context.Background())
	if len(sessions) != 1 || string(sessions[0].ID) != "web-mock" {
		t.Errorf("Web: expected web-mock, got %v", sessions)
	}
}

func TestProgressiveProvider_EmptySections_MocksAll(t *testing.T) {
	t.Parallel()

	mockProv := &mockDataProvider{sessions: []ingest.Session{{ID: "mock"}}}
	realProv := &mockDataProvider{sessions: []ingest.Session{{ID: "real"}}}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil, // nil/empty means all mocked
			},
		},
	}

	prov := NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	sessions, _ := prov.Sessions(context.Background())
	if len(sessions) != 1 || string(sessions[0].ID) != "mock" {
		t.Errorf("expected mock (empty sections means all mocked), got %v", sessions)
	}
}

func TestProgressiveProvider_QualitySessions_RoutesToMock(t *testing.T) {
	t.Parallel()

	mockProv := &mockDataProvider{
		quality: []QualitySession{{ID: "mock-quality-1", Project: "test"}},
	}
	realProv := &mockDataProvider{
		quality: []QualitySession{{ID: "real-quality-1", Project: "real"}},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil, // all sections mocked
			},
		},
	}

	prov := NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	ctx := context.Background()
	quality, err := prov.QualitySessions(ctx, QualityFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(quality) != 1 || quality[0].ID != "mock-quality-1" {
		t.Errorf("expected mock quality session, got %v", quality)
	}
}

func TestProgressiveProvider_DefaultConfig_MockDisabled(t *testing.T) {
	t.Parallel()

	mockProv := &mockDataProvider{sessions: []ingest.Session{{ID: "mock"}}}
	realProv := &mockDataProvider{sessions: []ingest.Session{{ID: "real"}}}

	// Load default config
	cfg := config.LoadDefaults(context.Background(), nil)

	// Web component
	webProv := NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)
	sessions, err := webProv.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Web: unexpected error: %v", err)
	}
	if len(sessions) != 1 || string(sessions[0].ID) != "real" {
		t.Errorf("Web: expected real session by default, got %v", sessions)
	}

	// TUI component
	tuiProv := NewProgressiveProvider(cfg, defaults.MockComponents.TUI, mockProv, realProv)
	sessions, err = tuiProv.Sessions(context.Background())
	if err != nil {
		t.Fatalf("TUI: unexpected error: %v", err)
	}
	if len(sessions) != 1 || string(sessions[0].ID) != "real" {
		t.Errorf("TUI: expected real session by default, got %v", sessions)
	}
}

func TestProgressiveProvider_MapSection_RoutesToMock(t *testing.T) {
	t.Parallel()

	mockGraph := schema.NewMapGraphPayload(progressiveProjectHash)
	mockGraph.RepoPath = "/mock/repo"
	realGraph := schema.NewMapGraphPayload(progressiveProjectHash)
	realGraph.RepoPath = "/real/repo"

	mockReview := schema.NewReviewListPayload(progressiveProjectHash)
	mockReview.DefaultBranch = "mock-main"
	realReview := schema.NewReviewListPayload(progressiveProjectHash)
	realReview.DefaultBranch = "real-main"

	mockSummaries := &codemap.ProjectSummariesResult{Projects: []schema.ProjectSummary{{Project: "mock-project"}}}
	realSummaries := &codemap.ProjectSummariesResult{Projects: []schema.ProjectSummary{{Project: "real-project"}}}

	mockProv := &mockDataProvider{
		mapGraph:          mockGraph,
		reviewList:        mockReview,
		summaries:         []SessionSummary{{ID: "mock-session-1"}},
		projectSummaries:  mockSummaries,
		projectResolution: &schema.ProjectResolutionPayload{Project: "hidden-project", ProjectHash: progressiveMockProjectHash},
	}
	realProv := &mockDataProvider{
		mapGraph:          realGraph,
		reviewList:        realReview,
		summaries:         []SessionSummary{{ID: "real-session-1"}},
		projectSummaries:  realSummaries,
		projectResolution: &schema.ProjectResolutionPayload{Project: "hidden-project", ProjectHash: progressiveRealProjectHash},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     []defaults.MockSection{defaults.MockSections.Map},
			},
		},
	}

	prov := NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)
	ctx := context.Background()

	// All four Map methods route to mock.
	graph, err := prov.MapGraph(ctx, progressiveProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph: unexpected error: %v", err)
	}
	if graph.RepoPath != "/mock/repo" {
		t.Errorf("MapGraph RepoPath = %q, want /mock/repo", graph.RepoPath)
	}
	if _, err := prov.MapNodeDetail(ctx, progressiveProjectHash, "internal/api"); err != nil {
		t.Errorf("MapNodeDetail: unexpected error: %v", err)
	}
	if _, err := prov.ProjectTasks(ctx, progressiveProjectHash, ""); err != nil {
		t.Errorf("ProjectTasks: unexpected error: %v", err)
	}
	picker, err := prov.ProjectSummaries(ctx)
	if err != nil {
		t.Fatalf("ProjectSummaries: unexpected error: %v", err)
	}
	if len(picker.Projects) != 1 || picker.Projects[0].Project != "mock-project" {
		t.Errorf("ProjectSummaries = %+v, want the mock picker row", picker.Projects)
	}
	resolution, err := prov.ResolveProject(ctx, "hidden-project")
	if err != nil {
		t.Fatalf("ResolveProject: unexpected error: %v", err)
	}
	if resolution.ProjectHash != progressiveMockProjectHash {
		t.Errorf("ResolveProject hash = %q, want %q", resolution.ProjectHash, progressiveMockProjectHash)
	}

	// Sections are isolated: sessions stays on the real provider.
	summaries, err := prov.SessionSummaries(ctx)
	if err != nil {
		t.Fatalf("SessionSummaries: unexpected error: %v", err)
	}
	if len(summaries) != 1 || summaries[0].ID != "real-session-1" {
		t.Errorf("expected real summaries with only map mocked, got %v", summaries)
	}

	// Review is its own section: not mocked here.
	review, err := prov.ReviewChanges(ctx, progressiveProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: unexpected error: %v", err)
	}
	if review.DefaultBranch != "real-main" {
		t.Errorf("ReviewChanges DefaultBranch = %q, want real-main (map-only mock)", review.DefaultBranch)
	}
}

func TestProgressiveProvider_ReviewSection_RoutesToMock(t *testing.T) {
	t.Parallel()

	mockList := schema.NewReviewListPayload(progressiveProjectHash)
	mockList.DefaultBranch = "mock-main"
	realList := schema.NewReviewListPayload(progressiveProjectHash)
	realList.DefaultBranch = "real-main"

	mockGraph := schema.NewMapGraphPayload(progressiveProjectHash)
	mockGraph.RepoPath = "/mock/repo"
	realGraph := schema.NewMapGraphPayload(progressiveProjectHash)
	realGraph.RepoPath = "/real/repo"

	mockPicker := &codemap.ProjectSummariesResult{Projects: []schema.ProjectSummary{{Project: "mock-project"}}}
	realPicker := &codemap.ProjectSummariesResult{Projects: []schema.ProjectSummary{{Project: "real-project"}}}

	mockProv := &mockDataProvider{reviewList: mockList, mapGraph: mockGraph, projectSummaries: mockPicker}
	realProv := &mockDataProvider{reviewList: realList, mapGraph: realGraph, projectSummaries: realPicker}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     []defaults.MockSection{defaults.MockSections.Review},
			},
		},
	}

	prov := NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)
	ctx := context.Background()

	list, err := prov.ReviewChanges(ctx, progressiveProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: unexpected error: %v", err)
	}
	if list.DefaultBranch != "mock-main" {
		t.Errorf("ReviewChanges DefaultBranch = %q, want mock-main", list.DefaultBranch)
	}
	if _, err := prov.ChangeDetail(ctx, progressiveProjectHash, "feat/x"); err != nil {
		t.Errorf("ChangeDetail: unexpected error: %v", err)
	}

	// Map is its own section: not mocked here.
	graph, err := prov.MapGraph(ctx, progressiveProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph: unexpected error: %v", err)
	}
	if graph.RepoPath != "/real/repo" {
		t.Errorf("MapGraph RepoPath = %q, want /real/repo (review-only mock)", graph.RepoPath)
	}

	// ProjectSummaries rides the Map section: real under a review-only mock.
	picker, err := prov.ProjectSummaries(ctx)
	if err != nil {
		t.Fatalf("ProjectSummaries: unexpected error: %v", err)
	}
	if len(picker.Projects) != 1 || picker.Projects[0].Project != "real-project" {
		t.Errorf("ProjectSummaries = %+v, want the real picker row (review-only mock)", picker.Projects)
	}
}
