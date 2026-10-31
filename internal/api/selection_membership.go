package api

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// storedSelectionMembership is the publication metadata projection shared by
// Home and the by-ID read. Prepare identity once over the complete stored cohort
// (including nonpushable clones), then match only requested stored IDs. Missing
// IDs stay absent. This never decides which stored rows may be browsed.
func storedSelectionMembership(ctx context.Context, rows []store.SessionRow, ids []string, selection config.SelectionConfig, resolver ingest.RepositoryIdentityResolver) (map[string]bool, error) {
	membership := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return membership, nil
	}
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	known := false
	for _, row := range rows {
		if requested[row.SessionID] {
			known = true
			break
		}
	}
	if !known {
		return membership, nil
	}
	if !selection.Mode.IsValid() {
		return nil, fmt.Errorf("api.storedSelectionMembership: the saved kickstart selection has no valid mode %q, so outsideSelection and autoPublish cannot be reported truthfully; no publication metadata was returned; run `peasant kickstart` to repair selection.mode (valid: all, selected), then retry", selection.Mode)
	}
	if err := config.ValidateSelection(selection); err != nil {
		return nil, fmt.Errorf("api.storedSelectionMembership: the saved ingestion selection is invalid: %w; outsideSelection and autoPublish cannot be reported truthfully, so no publication metadata was returned; run `peasant kickstart` to repair the saved selection, then retry", err)
	}
	if selection.Mode == config.SelectionModeAll {
		for _, row := range rows {
			if requested[row.SessionID] {
				membership[row.SessionID] = true
			}
		}
		return membership, nil
	}
	if resolver == nil {
		resolver = ingest.NewGitRepositoryIdentityResolver()
	}
	prepared, remotes, names, err := prepareWebDiscoveryIdentities(ctx, rows, selection, resolver)
	if err != nil {
		return nil, err
	}
	compiled := config.CompileSelectionMatcher(selection)
	for i, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !requested[row.SessionID] {
			continue
		}
		switch compiled.MatchBranchCandidate(ingest.DiscoveryCandidate{SessionID: ingest.SessionID(row.SessionID), Harness: defaults.Harness(row.ModelHarness), GitRemote: value(row.CanonicalRemote), ProjectName: row.ProjectName, ClonePath: prepared[i].clonePath, Branch: value(row.GitBranch), RemoteMultiplicity: remotes[i], NameMultiplicity: names[i]}) {
		case ingest.BranchMatchYes:
			membership[row.SessionID] = true
		case ingest.BranchMatchNo:
			membership[row.SessionID] = false
		case ingest.BranchMatchWithheldConflict:
			return nil, fmt.Errorf("api.storedSelectionMembership: conflicting persisted project branch rules for session %q; outsideSelection cannot be reported truthfully, so no publication metadata was returned; run `peasant kickstart` and make the project's branch selection consistent, then retry", row.SessionID)
		default:
			return nil, fmt.Errorf("api.storedSelectionMembership: unknown canonical matcher result for session %q; no publication metadata was returned because the saved ingest/push selection cannot be reported truthfully; update Peasant and retry", row.SessionID)
		}
	}
	return membership, nil
}
