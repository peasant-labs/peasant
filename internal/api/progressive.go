package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// ProgressiveProvider is a DataProvider that routes requests to either a mock
// or real provider based on granular configuration.
type ProgressiveProvider struct {
	cfg       *config.Config
	component defaults.MockComponent
	mock      DataProvider
	real      DataProvider
}

// NewProgressiveProvider creates a provider that granularly switches between
// mock and real data.
func NewProgressiveProvider(cfg *config.Config, component defaults.MockComponent, mock, real DataProvider) *ProgressiveProvider {
	return &ProgressiveProvider{
		cfg:       cfg,
		component: component,
		mock:      mock,
		real:      real,
	}
}

func (p *ProgressiveProvider) isMocked(section defaults.MockSection) bool {
	if p.cfg == nil || !p.cfg.Sources.Mock.Enabled {
		return false
	}
	if p.mock == nil {
		return false
	}

	var sections []defaults.MockSection
	switch p.component {
	case defaults.MockComponents.Web:
		sections = p.cfg.Sources.Mock.Web
	case defaults.MockComponents.TUI:
		sections = p.cfg.Sources.Mock.TUI
	case defaults.MockComponents.API:
		sections = p.cfg.Sources.Mock.API
	default:
		return false
	}

	// If no sections listed, everything is mocked for this component.
	if len(sections) == 0 {
		return true
	}
	return slices.Contains(sections, section)
}

func (p *ProgressiveProvider) getProvider(section defaults.MockSection) (DataProvider, error) {
	if p.isMocked(section) {
		if p.mock == nil {
			return nil, fmt.Errorf("mock provider not initialized for section %q", section)
		}
		return p.mock, nil
	}
	if p.real == nil {
		return nil, fmt.Errorf("real provider not initialized for section %q", section)
	}
	return p.real, nil
}

func (p *ProgressiveProvider) Sessions(ctx context.Context) ([]ingest.Session, error) {
	prov, err := p.getProvider(defaults.MockSections.Sessions)
	if err != nil {
		return nil, err
	}
	return prov.Sessions(ctx)
}

func (p *ProgressiveProvider) SessionSummaries(ctx context.Context) ([]SessionSummary, error) {
	prov, err := p.getProvider(defaults.MockSections.Sessions)
	if err != nil {
		return nil, err
	}
	return prov.SessionSummaries(ctx)
}

func (p *ProgressiveProvider) SessionSummariesByID(ctx context.Context, ids []string) ([]SessionSummary, error) {
	prov, err := p.getProvider(defaults.MockSections.Sessions)
	if err != nil {
		return nil, err
	}
	return prov.SessionSummariesByID(ctx, ids)
}

func (p *ProgressiveProvider) SessionByID(ctx context.Context, id string) (*ingest.Session, error) {
	prov, err := p.getProvider(defaults.MockSections.Sessions)
	if err != nil {
		return nil, err
	}
	return prov.SessionByID(ctx, id)
}

func (p *ProgressiveProvider) DashboardMetrics(ctx context.Context) (*DashboardPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Dashboard)
	if err != nil {
		return nil, err
	}
	return prov.DashboardMetrics(ctx)
}

func (p *ProgressiveProvider) TrendsData(ctx context.Context) (*TrendsPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Trends)
	if err != nil {
		return nil, err
	}
	return prov.TrendsData(ctx)
}

func (p *ProgressiveProvider) QualitySessions(ctx context.Context, f QualityFilter) ([]QualitySession, error) {
	prov, err := p.getProvider(defaults.MockSections.QualitySessions)
	if err != nil {
		return nil, err
	}
	return prov.QualitySessions(ctx, f)
}

func (p *ProgressiveProvider) AnnotationsForSession(ctx context.Context, sessionID string) ([]schema.AnnotationSummary, error) {
	prov, err := p.getProvider(defaults.MockSections.Annotations)
	if err != nil {
		return nil, err
	}
	return prov.AnnotationsForSession(ctx, sessionID)
}

func (p *ProgressiveProvider) ProjectFamiliarity(ctx context.Context, projectHash schema.ProjectHash) (*schema.FamiliarityPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Familiarity)
	if err != nil {
		return nil, err
	}
	return prov.ProjectFamiliarity(ctx, projectHash)
}

func (p *ProgressiveProvider) ChildSessionsForParent(ctx context.Context, parentID string) ([]schema.ChildSessionRef, error) {
	prov, err := p.getProvider(defaults.MockSections.Sessions)
	if err != nil {
		return nil, err
	}
	return prov.ChildSessionsForParent(ctx, parentID)
}

func (p *ProgressiveProvider) ProjectSummaries(ctx context.Context) (*codemap.ProjectSummariesResult, error) {
	prov, err := p.getProvider(defaults.MockSections.Map)
	if err != nil {
		return nil, err
	}
	return prov.ProjectSummaries(ctx)
}

func (p *ProgressiveProvider) ResolveProject(ctx context.Context, project string) (*schema.ProjectResolutionPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Map)
	if err != nil {
		return nil, err
	}
	return prov.ResolveProject(ctx, project)
}

func (p *ProgressiveProvider) MapGraph(ctx context.Context, projectHash schema.ProjectHash, commit string) (*schema.MapGraphPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Map)
	if err != nil {
		return nil, err
	}
	return prov.MapGraph(ctx, projectHash, commit)
}

func (p *ProgressiveProvider) MapNodeDetail(ctx context.Context, projectHash schema.ProjectHash, path string) (*schema.MapNodeDetailPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Map)
	if err != nil {
		return nil, err
	}
	return prov.MapNodeDetail(ctx, projectHash, path)
}

func (p *ProgressiveProvider) ProjectTasks(ctx context.Context, projectHash schema.ProjectHash, file string) (*schema.ProjectTasksPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Map)
	if err != nil {
		return nil, err
	}
	return prov.ProjectTasks(ctx, projectHash, file)
}

func (p *ProgressiveProvider) ReviewChanges(ctx context.Context, projectHash schema.ProjectHash) (*schema.ReviewListPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Review)
	if err != nil {
		return nil, err
	}
	return prov.ReviewChanges(ctx, projectHash)
}

func (p *ProgressiveProvider) ChangeDetail(ctx context.Context, projectHash schema.ProjectHash, branch string) (*schema.ChangeDetailPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Review)
	if err != nil {
		return nil, err
	}
	return prov.ChangeDetail(ctx, projectHash, branch)
}

func (p *ProgressiveProvider) ChangeDiff(ctx context.Context, projectHash schema.ProjectHash, branch, file string) (*schema.ChangeDiffPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Review)
	if err != nil {
		return nil, err
	}
	return prov.ChangeDiff(ctx, projectHash, branch, file)
}

func (p *ProgressiveProvider) Search(ctx context.Context, query string, limit int) (*schema.SearchPayload, error) {
	prov, err := p.getProvider(defaults.MockSections.Search)
	if err != nil {
		return nil, err
	}
	return prov.Search(ctx, query, limit)
}

// Ensure implementation of DataProvider interface.
var _ DataProvider = (*ProgressiveProvider)(nil)
