package api

import (
	"context"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// DataProvider is the interface for fetching session and analytics data.
// MockProvider implements this with static data; StoreDataProvider reads
// from the SQLite store.
type DataProvider interface {
	Sessions(ctx context.Context) ([]ingest.Session, error)
	SessionSummaries(ctx context.Context) ([]SessionSummary, error)
	SessionByID(ctx context.Context, id string) (*ingest.Session, error)
	DashboardMetrics(ctx context.Context) (*DashboardPayload, error)
	TrendsData(ctx context.Context) (*TrendsPayload, error)
	QualitySessions(ctx context.Context, f QualityFilter) ([]QualitySession, error)
	AnnotationsForSession(ctx context.Context, sessionID string) ([]schema.AnnotationSummary, error)
	ProjectFamiliarity(ctx context.Context, projectHash schema.ProjectHash) (*schema.FamiliarityPayload, error)
	ChildSessionsForParent(ctx context.Context, parentID string) ([]schema.ChildSessionRef, error)

	// Map / Review surfaces. commit, path, file, and branch are
	// the raw query-param values ("" where optional and absent).
	ProjectSummaries(ctx context.Context) (*codemap.ProjectSummariesResult, error)
	ResolveProject(ctx context.Context, project string) (*schema.ProjectResolutionPayload, error)
	MapGraph(ctx context.Context, projectHash schema.ProjectHash, commit string) (*schema.MapGraphPayload, error)
	MapNodeDetail(ctx context.Context, projectHash schema.ProjectHash, path string) (*schema.MapNodeDetailPayload, error)
	ProjectTasks(ctx context.Context, projectHash schema.ProjectHash, file string) (*schema.ProjectTasksPayload, error)
	ReviewChanges(ctx context.Context, projectHash schema.ProjectHash) (*schema.ReviewListPayload, error)
	ChangeDetail(ctx context.Context, projectHash schema.ProjectHash, branch string) (*schema.ChangeDetailPayload, error)
	ChangeDiff(ctx context.Context, projectHash schema.ProjectHash, branch, file string) (*schema.ChangeDiffPayload, error)

	// Search is global full-text search over recorded message entries (Cmd-K).
	// query is the raw user input; limit is the requested cap (0 = default).
	Search(ctx context.Context, query string, limit int) (*schema.SearchPayload, error)
}

// MockProviderFactory creates a mock DataProvider. Set by the mock package
// via init() to break the import cycle (api → mock → api).
var MockProviderFactory func() DataProvider
