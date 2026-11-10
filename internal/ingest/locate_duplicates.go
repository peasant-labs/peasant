package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// refuseDuplicates runs every built claim, together with the extra claims a
// stored Claude root or a stored Codex id can carry, through the same refusal a
// full run uses. A refused id joins the run's failures and is dropped from the
// processed members; a refused row that the store still records joins the
// report's upkeep set, because the store-side passes a full run performs for
// that row must still reach it. LOCATE processes neither copy.
func (l *scopeLocator) refuseDuplicates(ctx context.Context) error {
	if err := l.collectStoredDuplicateClaims(ctx); err != nil {
		return err
	}
	claims := make([]DiscoveredSession, 0, len(l.members)+len(l.extraClaims))
	for _, member := range l.members {
		if member.Source != nil {
			claims = append(claims, *member.Source)
		}
	}
	claims = append(claims, l.extraClaims...)
	if len(claims) == 0 {
		return nil
	}
	admitted := make(map[SessionID]struct{}, len(l.members))
	for id := range l.members {
		admitted[id] = struct{}{}
	}
	kept, refused := refuseDuplicateSessionIDs(l.p.fs, claims, locateDuplicateScope(admitted))
	l.refusals = refused
	l.removeRefused(kept, refused)
	return nil
}

// removeRefused drops every member the refusal did not keep: a member whose
// source was a refused claim, and a subagent the refusal left out with its
// parent. A dropped id whose row the store still records joins refusedUpkeep:
// the run must not process it, but the store-side passes a full run performs for
// that row must still see it. The recorded set is both halves the store
// answered: the selected ids' rows and their stored children.
func (l *scopeLocator) removeRefused(kept []DiscoveredSession, refused []SessionResult) {
	keptPaths := make(map[ResolvedPath]struct{}, len(kept))
	for _, session := range kept {
		keptPaths[session.SourcePath] = struct{}{}
	}
	refusedIDs := make(map[SessionID]struct{}, len(refused))
	for _, result := range refused {
		refusedIDs[result.SessionID] = struct{}{}
	}
	recorded := make(map[SessionID]struct{}, len(l.stored))
	for id := range l.stored {
		recorded[id] = struct{}{}
	}
	for _, rows := range l.children {
		for _, row := range rows {
			recorded[row.ID] = struct{}{}
		}
	}
	for id, member := range l.members {
		drop := false
		if _, bad := refusedIDs[id]; bad {
			drop = true
		} else if member.Source != nil {
			if _, ok := keptPaths[member.Source.SourcePath]; !ok {
				drop = true
			}
		}
		if !drop {
			continue
		}
		delete(l.members, id)
		if _, stored := recorded[id]; stored {
			l.refusedUpkeep = append(l.refusedUpkeep, id)
		}
	}
	sortSessionIDs(l.refusedUpkeep)
}

// collectStoredDuplicateClaims adds the second claim a stored id can carry
// beyond the source LOCATE already built.
//
// A stored Claude root gets the root name probe, so a second root copy under
// another project directory is refused exactly as a full walk refuses it. A
// stored Codex id gets the catalog count and, below two catalogued paths, the
// same two date folders the ancestor probe declares.
func (l *scopeLocator) collectStoredDuplicateClaims(ctx context.Context) error {
	if l.store == nil {
		return nil
	}
	for _, id := range l.scope.IDs() {
		row, ok := l.stored[id]
		if !ok {
			continue
		}
		switch row.Harness {
		case HarnessClaudeCode:
			// Only a stored root has another root copy to look for.
			if row.Parent != nil {
				continue
			}
			if err := l.collectClaudeRootCopies(ctx, id, row); err != nil {
				return err
			}
		case HarnessCodex:
			if err := l.collectCodexClaims(ctx, id, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectClaudeRootCopies probes <root>/*/<id>.jsonl for a second root copy.
// Only a path laid out as a root transcript has a root above it.
func (l *scopeLocator) collectClaudeRootCopies(ctx context.Context, id SessionID, row StoredSessionSource) error {
	cfg, enabled := l.enabledConfig(HarnessClaudeCode)
	if !enabled {
		return nil
	}
	path := string(row.Path)
	if filepath.Base(path) != string(id)+defaults.ExtJSONL.String() {
		return nil
	}
	root := ResolvedPath(filepath.Dir(filepath.Dir(path)))
	rootConfigured := false
	for _, configured := range cfg.Paths {
		if configured == root {
			rootConfigured = true
			break
		}
	}
	if !rootConfigured {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := l.p.fs.ReadDir(string(root))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pipeline locate claude root probe: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(string(root), entry.Name(), string(id)+defaults.ExtJSONL.String())
		if candidate == path {
			continue
		}
		info, err := l.p.fs.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		l.extraClaims = append(l.extraClaims, DiscoveredSession{
			SessionID:    id,
			Harness:      HarnessClaudeCode,
			SourcePath:   ResolvedPath(candidate),
			SourceFormat: SourceFormatJSONL,
			ModTime:      info.ModTime(),
		})
	}
	return nil
}

// collectCodexClaims counts the catalogued rollouts for a stored Codex id. At
// two or more it materialises every catalogued path; below two it also lists the
// id's own date folder and the previous day. Every claim carries a ModTime, so
// the refusal's newer line is deterministic.
func (l *scopeLocator) collectCodexClaims(ctx context.Context, id SessionID, row StoredSessionSource) error {
	if l.derived == nil {
		return fmt.Errorf(
			"ingest: the attached store cannot answer CountCodexRolloutsBySession; a stored Codex session whose id two rollout files claim can only be detected through the indexed rollout catalog, so this run would process a duplicated session as if it were unique; where: ingest.Pipeline.locateScopedRun in internal/ingest/locate.go; when: checking stored session %s for duplicate rollouts; impact: no session was processed and no store row was written; fix: attach a store that implements DerivedLookupReader, or run without a store for a logs-only run",
			id)
	}
	count, err := l.derived.CountCodexRolloutsBySession(ctx, id)
	if err != nil {
		return fmt.Errorf("pipeline locate codex rollouts: %w", err)
	}
	if count >= 2 {
		paths, err := l.derived.CodexRolloutPaths(ctx, id)
		if err != nil {
			return fmt.Errorf("pipeline locate codex rollouts: %w", err)
		}
		for _, path := range paths {
			l.extraClaims = append(l.extraClaims, l.codexCatalogueClaim(id, path))
		}
		return nil
	}
	paths, err := l.codexWindowPaths(ctx, id, row.Path)
	if err != nil {
		return fmt.Errorf("pipeline locate codex rollout window: %w", err)
	}
	for _, path := range paths {
		l.extraClaims = append(l.extraClaims, l.codexCatalogueClaim(id, path))
	}
	return nil
}

// codexCatalogueClaim materialises one catalogued rollout as a claim and stats
// it once for its ModTime.
func (l *scopeLocator) codexCatalogueClaim(id SessionID, path ResolvedPath) DiscoveredSession {
	claim := DiscoveredSession{
		SessionID:    id,
		Harness:      HarnessCodex,
		SourcePath:   path,
		SourceFormat: SourceFormatJSONL,
	}
	if info, err := l.p.fs.Stat(string(path)); err == nil && !info.IsDir() {
		claim.ModTime = info.ModTime()
	}
	return claim
}
