package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// qualityDefaultPageSize is the page the quality read serves when the request
// omits maxItems (or sends 0, the omitted sentinel).
const qualityDefaultPageSize schema.PageSize = schema.PageSize25

// QualityPageProvider serves the paginated quality read: one offset page of
// rows plus the library-wide analytics summary at the page revision. It is a
// narrow provider seam rather than part of DataProvider so the WebSocket
// channels and the paginated page can adopt it independently.
type QualityPageProvider interface {
	QualityPage(ctx context.Context, page int, perPage schema.PageSize) (*schema.QualityPage, error)
}

var _ QualityPageProvider = (*StoreDataProvider)(nil)

// qualityAnalysisCache holds the summary and the personal medians computed at
// one store revision. Both are library-wide aggregates, so a page request
// reuses them until a write advances store_state.revision.
type qualityAnalysisCache struct {
	mu        sync.Mutex
	valid     bool
	revision  string
	analytics schema.ProjectAnalytics
	medians   schema.PersonalMedians
}

// handleQualityPage answers GET /api/v1/quality. It parses the page and the
// closed-set row count, then asks the provider for the page and its summary.
// A request past the last page is clamped to the last page: no surface renders
// an empty page beyond the total.
func (s *Server) handleQualityPage(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{
		operation: "quality page",
		provider:  true,
		query:     map[string]bool{"page": false, "maxItems": false},
	})
	if !ok {
		return
	}
	provider, ok := s.cfg.Provider.(QualityPageProvider)
	if !ok {
		writeAPIError(w, http.StatusServiceUnavailable,
			"The quality page could not be served because the configured provider in internal/api.handleQualityPage does not implement the paginated quality read. No rows were returned, because the full-library quality topic is retired for this surface. Restart Peasant with its store provider configured, then retry.",
			"quality_unavailable")
		return
	}
	page, pageOK := parseGroupedPageValue(request.query, "page", 1)
	if !pageOK {
		writeAPIError(w, http.StatusBadRequest,
			"The quality page could not be served because query field \"page\" is not a positive integer in internal/api.handleQualityPage. No provider call was made and no rows were returned. Provide one page number of at least 1, or remove the field to use page 1, then retry.",
			"quality_page_invalid")
		return
	}
	perPage, sizeOK := parseQualityMaxItems(request.query)
	if !sizeOK {
		writeAPIError(w, http.StatusBadRequest,
			"The quality page could not be served because query field \"maxItems\" is outside the closed 25/50/100 page sizes in internal/api.handleQualityPage. No provider call was made and no rows were returned. Provide 25, 50, or 100, or remove the field for the default of 25, then retry.",
			"quality_page_size_invalid")
		return
	}
	payload, err := provider.QualityPage(r.Context(), page, perPage)
	if err != nil {
		writeDiscoveryError(w, "failed to read the quality page", err)
		return
	}
	if err := payload.Validate(); err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			"The quality page could not be served because the assembled payload failed the schema contract in internal/api.handleQualityPage after the rows and summary were read. No response was returned, because a malformed page would misstate the total or the analytics. Retry the request; if the failure repeats, inspect the Peasant server logs.",
			"quality_contract_invalid")
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// parseQualityMaxItems reads the optional row-count field. An omitted, empty,
// or zero value maps to the 25-row default before the schema-owned page-size
// parser runs; any other value outside 25/50/100 is refused rather than cast.
func parseQualityMaxItems(values url.Values) (schema.PageSize, bool) {
	raw, present := values["maxItems"]
	if !present || len(raw) == 0 || raw[0] == "" {
		return qualityDefaultPageSize, true
	}
	parsed, err := strconv.Atoi(raw[0])
	if err != nil {
		return 0, false
	}
	if parsed == 0 {
		return qualityDefaultPageSize, true
	}
	size, err := schema.NewPageSize(parsed)
	if err != nil {
		return 0, false
	}
	return size, true
}

// QualityPage assembles one quality page: the revision, the cached summary at
// that revision, the total, and one bounded page of rows. The page is clamped
// to the last page when the request asks past the end.
func (p *StoreDataProvider) QualityPage(ctx context.Context, page int, perPage schema.PageSize) (*schema.QualityPage, error) {
	revision, err := p.store.Revision(ctx)
	if err != nil {
		return nil, fmt.Errorf("store adapter: quality page revision: %w", err)
	}
	summary, _, err := p.qualityAnalysisAt(ctx, revision)
	if err != nil {
		return nil, err
	}
	total, err := p.store.CountQualitySessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("store adapter: quality page total: %w", err)
	}
	if page < 1 {
		page = 1
	}
	if last := lastQualityPage(total, perPage); page > last {
		page = last
	}
	read, err := p.store.QualityPageRows(ctx, page, int(perPage))
	if err != nil {
		return nil, fmt.Errorf("store adapter: quality page rows: %w", err)
	}
	return buildQualityPage(read.Rows, page, perPage, total, revision, summary), nil
}

// qualityAnalysisAt returns the library summary and the personal medians for
// one revision, recomputing both when the revision has advanced. The page and
// the transcript read share this cache: a reader that no longer receives the
// full list cannot derive either value.
func (p *StoreDataProvider) qualityAnalysisAt(ctx context.Context, revision string) (schema.ProjectAnalytics, schema.PersonalMedians, error) {
	cache := &p.qualityAnalysis
	cache.mu.Lock()
	if cache.valid && cache.revision == revision {
		analytics, medians := cache.analytics, cache.medians
		cache.mu.Unlock()
		return analytics, medians, nil
	}
	cache.mu.Unlock()

	analytics, medians, err := p.store.AnalyticsAggregates(ctx, store.AnalyticsFilter{})
	if err != nil {
		return schema.ProjectAnalytics{}, schema.PersonalMedians{}, fmt.Errorf("store adapter: quality analysis: %w", err)
	}

	cache.mu.Lock()
	cache.valid = true
	cache.revision = revision
	cache.analytics = analytics
	cache.medians = medians
	cache.mu.Unlock()
	return analytics, medians, nil
}

// PersonalMedians returns the personal medians at the current revision. The
// transcript read consumes it; the cache is shared with the quality page so one
// revision computes one baseline.
func (p *StoreDataProvider) PersonalMedians(ctx context.Context) (schema.PersonalMedians, error) {
	revision, err := p.store.Revision(ctx)
	if err != nil {
		return schema.PersonalMedians{}, fmt.Errorf("store adapter: personal medians revision: %w", err)
	}
	_, medians, err := p.qualityAnalysisAt(ctx, revision)
	return medians, err
}

// buildQualityPage turns one bounded row read into the wire page. The rows are
// the requested page followed by the next page; each array is capped at
// perPage by construction. The page rows carry no annotation join: the page is
// one bounded read, and a consumer that needs annotations reads them from the
// annotation routes rather than expanding every row on the page.
func buildQualityPage(rows []store.SessionRow, page int, perPage schema.PageSize, total int, revision string, summary schema.ProjectAnalytics) *schema.QualityPage {
	size := int(perPage)
	items := make([]schema.QualitySession, 0, min(size, len(rows)))
	next := make([]schema.QualitySession, 0, min(size, len(rows)))
	for i := range rows {
		row := sessionRowToQuality(&rows[i])
		switch {
		case i < size:
			items = append(items, row)
		case i < 2*size:
			next = append(next, row)
		}
	}
	return &schema.QualityPage{
		OffsetPage: schema.OffsetPage[schema.QualitySession]{
			Items:     items,
			NextItems: next,
			Page:      page,
			PerPage:   perPage,
			Total:     total,
			Revision:  revision,
		},
		Summary: summary,
	}
}

// lastQualityPage returns the one-based last page for a total, or 1 for an
// empty library so an empty page still renders as page 1 of 1.
func lastQualityPage(total int, perPage schema.PageSize) int {
	if total <= 0 {
		return 1
	}
	size := int(perPage)
	return (total + size - 1) / size
}
