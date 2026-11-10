package onboarding

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// PrepareBaseline replaces a legacy mode-all or pathless selected policy in
// memory. It does not write the config file. A selected policy that already
// names clone paths is returned unchanged. Mode all is narrowed to the exact
// projects the current scan can show, and stored sessions the scan cannot show
// stay in the residual that the caller merges before the user saves.
func PrepareBaseline(
	loaded *config.Config,
	scan []SessionListing,
	stored []store.IngestedSessionRow,
	resolver ingest.PathIdentityResolver,
) (*config.Config, error) {
	if loaded == nil {
		return nil, fmt.Errorf(
			"prepare an in-memory setup baseline.\n" +
				"what: no loaded configuration was provided.\n" +
				"why: legacy conversion needs the saved selection mode and auto-ingest flag.\n" +
				"where: onboarding.PrepareBaseline.\n" +
				"when: before a setup draft is opened.\n" +
				"means: no baseline was built and no file was written.\n" +
				"fix: load the configuration, then call PrepareBaseline with that value.")
	}
	if loaded.Selection.Mode == config.SelectionModeSelected && hasPathlessProject(loaded.Selection) {
		selection, err := ConvertLegacySelected(loaded.Selection, stored, resolver)
		if err != nil {
			return nil, err
		}
		converted := *loaded
		converted.Selection = selection
		return &converted, nil
	}
	if loaded.Selection.Mode != config.SelectionModeAll {
		return loaded, nil
	}
	conversion, err := ConvertLegacyAll(scan, stored, resolver, loaded.Selection.AutoIngestNewBranches)
	if err != nil {
		return nil, err
	}
	merged := MergeResidual(conversion.Initial, conversion.Unmatched)
	converted := *loaded
	converted.Selection = merged
	converted.Selection.Mode = conversion.Initial.Mode
	converted.Selection.AutoIngestNewBranches = conversion.Initial.AutoIngestNewBranches
	return &converted, nil
}

func hasPathlessProject(selection config.SelectionConfig) bool {
	if selection.Mode != config.SelectionModeSelected {
		return false
	}
	for _, configured := range selection.Harnesses {
		for _, project := range configured.Projects {
			if len(project.ClonePaths) == 0 {
				return true
			}
		}
	}
	return false
}
