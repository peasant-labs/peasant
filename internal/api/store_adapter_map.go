package api

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// newCodemapService wires the production codemap.Service: the store, the
// exec-git repository factory (canonical_cwd → repo), and the tree-sitter
// graph builder.
func newCodemapService(s *store.Store, visibility sessionvisibility.Policy) *codemap.Service {
	return codemap.NewService(
		s,
		func(repoPath string) gitops.Repository { return gitops.NewExecGitRepository(repoPath) },
		codegraph.NewGraphBuilder(),
		visibility,
	)
}

// ProjectSummaries lists the home-picker rows: one per store project, plus
// selection-state metadata from the map contract extension.
func (p *StoreDataProvider) ProjectSummaries(ctx context.Context) (*codemap.ProjectSummariesResult, error) {
	result, err := p.codemap.ProjectSummaries(ctx)
	if err != nil {
		return nil, fmt.Errorf("store adapter: project summaries: %w", err)
	}
	return result, nil
}

// ResolveProject resolves one explicit deep link without enumerating projects.
func (p *StoreDataProvider) ResolveProject(ctx context.Context, project string) (*schema.ProjectResolutionPayload, error) {
	payload, err := p.codemap.ResolveProject(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("store adapter: resolve project: %w", err)
	}
	return payload, nil
}

// MapGraph builds the full map graph for a project.
func (p *StoreDataProvider) MapGraph(ctx context.Context, projectHash schema.ProjectHash, commit string) (*schema.MapGraphPayload, error) {
	payload, err := p.codemap.MapGraph(ctx, projectHash, commit)
	if err != nil {
		return nil, fmt.Errorf("store adapter: map graph: %w", err)
	}
	return payload, nil
}

// MapNodeDetail builds the Map node rail panel for one node path.
func (p *StoreDataProvider) MapNodeDetail(ctx context.Context, projectHash schema.ProjectHash, path string) (*schema.MapNodeDetailPayload, error) {
	payload, err := p.codemap.MapNodeDetail(ctx, projectHash, path)
	if err != nil {
		return nil, fmt.Errorf("store adapter: map node detail: %w", err)
	}
	return payload, nil
}

// ProjectTasks lists the project's tasks, optionally filtered to one file or
// directory path ("" = all).
func (p *StoreDataProvider) ProjectTasks(ctx context.Context, projectHash schema.ProjectHash, file string) (*schema.ProjectTasksPayload, error) {
	payload, err := p.codemap.ProjectTasks(ctx, projectHash, file)
	if err != nil {
		return nil, fmt.Errorf("store adapter: project tasks: %w", err)
	}
	return payload, nil
}

// ReviewChanges lists the project's changes (open branches, then merged).
func (p *StoreDataProvider) ReviewChanges(ctx context.Context, projectHash schema.ProjectHash) (*schema.ReviewListPayload, error) {
	payload, err := p.codemap.ReviewChanges(ctx, projectHash)
	if err != nil {
		return nil, fmt.Errorf("store adapter: review changes: %w", err)
	}
	return payload, nil
}

// ChangeDetail builds the Review detail payload for one branch.
func (p *StoreDataProvider) ChangeDetail(ctx context.Context, projectHash schema.ProjectHash, branch string) (*schema.ChangeDetailPayload, error) {
	payload, err := p.codemap.ChangeDetail(ctx, projectHash, branch)
	if err != nil {
		return nil, fmt.Errorf("store adapter: change detail: %w", err)
	}
	return payload, nil
}

// ChangeDiff builds the rendered per-file diff for one changed file of a branch.
func (p *StoreDataProvider) ChangeDiff(ctx context.Context, projectHash schema.ProjectHash, branch, file string) (*schema.ChangeDiffPayload, error) {
	payload, err := p.codemap.ChangeDiff(ctx, projectHash, branch, file)
	if err != nil {
		return nil, fmt.Errorf("store adapter: change diff: %w", err)
	}
	return payload, nil
}

// Search runs a global full-text search over recorded message entries.
func (p *StoreDataProvider) Search(ctx context.Context, query string, limit int) (*schema.SearchPayload, error) {
	payload, err := p.codemap.Search(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("store adapter: search: %w", err)
	}
	return payload, nil
}
