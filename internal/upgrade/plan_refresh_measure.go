//go:build skipbudget

package upgrade

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// PlanRefreshWithAdapters plans against a declaration table read from the
// supplied adapter registry instead of the shipped one. It exists only in the
// measurement build, which registers an adapter that withholds its change
// scope and passes the same registry to the pipeline it times.
func PlanRefreshWithAdapters(ctx context.Context, path string, adapters map[ingest.Harness]ingest.AdapterFactory) (RefreshPlan, error) {
	registries := currentProducerRegistries()
	registries.adapters = adapters
	declarations, err := declareProducers(registries)
	if err != nil {
		return RefreshPlan{}, fmt.Errorf("plan refresh producers: %w", err)
	}
	return planRefresh(ctx, path, declarations)
}
