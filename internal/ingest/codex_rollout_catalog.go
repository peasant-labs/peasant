package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// CodexRolloutCatalogStore is the store capability the full Codex walk uses to
// keep the derived rollout catalog current.
//
// The catalog is a file catalog, not a projection of stored sessions: it holds
// one row per rollout PATH the walk enumerated, including a rollout that was
// never stored and a second copy of an id the duplicate stage refused. Keeping
// the enumerated paths (rather than the sessions that survived the write) is
// what lets a later scoped run answer the duplicate and parent questions
// without walking the sessions tree.
type CodexRolloutCatalogStore interface {
	// LoadCodexRolloutCatalog returns every catalogued rollout path with the
	// session id it carries.
	LoadCodexRolloutCatalog(ctx context.Context) ([]RolloutFile, error)
	// SaveCodexRolloutCatalog upserts one row per path in catalog and removes
	// the rows named by deletes, in one transaction. An empty catalog and an
	// empty delete list write nothing.
	SaveCodexRolloutCatalog(ctx context.Context, catalog []RolloutFile, deletes []ResolvedPath) error
}

// CodexRolloutCatalogCaching is implemented by an adapter that keeps the
// derived rollout catalog current. The adapter factory signature carries only
// the filesystem, the git resolver, and the salt, so a caller attaches the
// store capability after it builds the adapter — the same optional,
// type-asserted attach shape the evidence cache uses.
type CodexRolloutCatalogCaching interface {
	SetCodexRolloutCatalog(catalog CodexRolloutCatalogStore)
}

// AttachCodexRolloutCatalog gives adapter the rollout catalog store when the
// adapter maintains one. Adapters that keep no catalog ignore the store.
func AttachCodexRolloutCatalog(adapter SourceAdapter, catalog CodexRolloutCatalogStore) {
	if adapter == nil || catalog == nil {
		return
	}
	if caching, ok := adapter.(CodexRolloutCatalogCaching); ok {
		caching.SetCodexRolloutCatalog(catalog)
	}
}

// DiscoveryCodeCodexRolloutCatalogWrite is the DiscoveryDiagnostic code for a
// full walk whose rollout catalog could not be updated.
const DiscoveryCodeCodexRolloutCatalogWrite = "codex_rollout_catalog_write"

// SetCodexRolloutCatalog makes the full walk keep the derived rollout catalog
// current. A nil store leaves the walk writing nothing.
func (a *CodexAdapter) SetCodexRolloutCatalog(catalog CodexRolloutCatalogStore) {
	a.rolloutCatalog = catalog
}

// codexRolloutFiles names the session id each enumerated rollout path carries.
// The enumeration already refused every file whose name is not a rollout name,
// so the id comes from the same file-name parse discovery uses; a name that
// still fails id validation contributes no row, because a row keyed by a path
// whose id cannot be parsed could answer no duplicate or parent question.
func codexRolloutFiles(sources []codexRolloutSource) []RolloutFile {
	files := make([]RolloutFile, 0, len(sources))
	for _, source := range sources {
		rawID, ok := codexSessionIDFromFilename(filepath.Base(source.path.String()))
		if !ok {
			continue
		}
		id, err := NewSessionID(rawID)
		if err != nil {
			continue
		}
		files = append(files, RolloutFile{Path: source.path, SessionID: id})
	}
	return files
}

// maintainCodexRolloutCatalog brings the catalog in line with the walk that
// just finished: one row per enumerated path, and no row for a path the walk
// re-walked and found gone.
//
// The walk never prunes a row whose file it did not look for. A row is removed
// only when its path lies under a walked root, the walk did not enumerate it,
// and the file is gone; a path that still exists, or whose stat fails for any
// reason other than absence, keeps its row. That is what a partial walk needs:
// a directory this run could not read hides rollouts that are still on disk,
// and deleting their rows would reopen the duplicate-detection gap the catalog
// exists to close.
//
// A failed catalog write is reported, never swallowed: the harvest still
// completes, the next full walk re-derives the catalog, and a scoped run keeps
// answering from whatever the last full walk left.
func (a *CodexAdapter) maintainCodexRolloutCatalog(ctx context.Context, roots []ResolvedPath, enumerated []RolloutFile) {
	if a.rolloutCatalog == nil {
		return
	}
	unique := make([]RolloutFile, 0, len(enumerated))
	seen := make(map[ResolvedPath]struct{}, len(enumerated))
	for _, file := range enumerated {
		if file.Path == "" || file.SessionID == "" {
			continue
		}
		if _, duplicate := seen[file.Path]; duplicate {
			continue
		}
		seen[file.Path] = struct{}{}
		unique = append(unique, file)
	}

	existing, err := a.rolloutCatalog.LoadCodexRolloutCatalog(ctx)
	if err != nil {
		a.noteCodexRolloutCatalogFailure(err)
		return
	}

	var deletes []ResolvedPath
	for _, row := range existing {
		if _, walked := seen[row.Path]; walked {
			continue
		}
		if !pathUnderAnyRoot(row.Path, roots) {
			continue
		}
		if _, statErr := a.fs.Stat(row.Path.String()); statErr == nil || !errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		deletes = append(deletes, row.Path)
	}

	if len(unique) == 0 && len(deletes) == 0 {
		return
	}
	if err := a.rolloutCatalog.SaveCodexRolloutCatalog(ctx, unique, deletes); err != nil {
		a.noteCodexRolloutCatalogFailure(err)
	}
}

// noteCodexRolloutCatalogFailure records a failed catalog write on the run's
// diagnostics. The walk itself is unaffected: the sessions it discovered are
// built normally, and the next full walk re-derives the catalog from the tree.
func (a *CodexAdapter) noteCodexRolloutCatalogFailure(err error) {
	a.diagnostics = append(a.diagnostics, DiscoveryDiagnostic{
		Provider: HarnessCodex,
		Code:     DiscoveryCodeCodexRolloutCatalogWrite,
		Summary:  "the Codex rollout catalog could not be updated",
		Detail: fmt.Sprintf(
			"the catalog write failed: %s; the sessions discovered this run are unaffected, a scoped run keeps answering duplicate and parent questions from the last full walk, and the next full walk re-derives the catalog",
			firstLine(err.Error())),
	})
}
