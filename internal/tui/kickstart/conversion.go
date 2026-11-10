package kickstart

import (
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

// LegacyAllConversion is the in-memory replacement for a legacy mode-all
// policy, owned by the neutral conversion.
type LegacyAllConversion = onboarding.LegacyAllConversion

// ConvertLegacyAll builds the first exact selected-mode policy from complete
// stored and scanner cohorts. It performs no file write.
func ConvertLegacyAll(
	scan []ftue.SessionListing,
	stored []store.IngestedSessionRow,
	resolver ingest.PathIdentityResolver,
	autoIngestNewBranches bool,
) (LegacyAllConversion, error) {
	return onboarding.ConvertLegacyAll(scan, stored, resolver, autoIngestNewBranches)
}

// ConvertLegacySelected replaces selected-mode project rules that do not carry
// physical clone paths with exact choices derived from the complete stored
// session cohort. The transform is in-memory only.
func ConvertLegacySelected(
	current config.SelectionConfig,
	stored []store.IngestedSessionRow,
	resolver ingest.PathIdentityResolver,
) (config.SelectionConfig, error) {
	return onboarding.ConvertLegacySelected(current, stored, resolver)
}
