package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// ContentCaptureResult is one retained-content capture, as the adapter that
// parsed it reports it. The adapter declares its own completeness: Complete is
// false whenever the adapter knows that rows were filtered or omitted, so a
// partial recovery can never be stored as a verified complete capture.
type ContentCaptureResult struct {
	Entries   []schema.SessionEntry
	Authority ContentSourceAuthority
	// Complete reports that the entries are the whole session, as parsed.
	Complete bool
	// InputHash is the index input digest over the retained bytes actually
	// parsed, so a later run can tell whether the same input was consumed.
	InputHash string
}

// backfillIncompleteContent traverses by key, not by offset or a repeated first
// page: a broken first snapshot cannot starve later recoverable sessions.
func (p *Pipeline) backfillIncompleteContent(ctx context.Context) (map[SessionID]bool, error) {
	recovered := make(map[SessionID]bool)
	store, ok := p.metricsStore.(ContentBackfillTargetStore)
	if !ok || p.config.DryRun {
		return recovered, nil
	}
	var after SessionID
	for {
		if err := ctx.Err(); err != nil {
			return recovered, err
		}
		targets, err := store.ListContentCaptureIncompleteSessionsAfter(ctx, after, 100)
		if cancelErr := pipelineCancellation(ctx, err); cancelErr != nil {
			return recovered, cancelErr
		}
		if err != nil {
			return recovered, err
		}
		if len(targets) == 0 {
			return recovered, nil
		}
		for _, target := range targets {
			if err := ctx.Err(); err != nil {
				return recovered, err
			}
			id := target.SessionID
			after = id
			if err := p.backfillContentSession(ctx, store, id); err != nil {
				if cancelErr := pipelineCancellation(ctx, err); cancelErr != nil {
					return recovered, cancelErr
				}
				slog.Warn("content backfill failed; existing canonical state unchanged", "session_id", id, "error", err)
				p.reportDiagnostic(DiagnosticEntry{
					ErrorType: "content_recovery_unavailable", Location: fmt.Sprintf("session %s retained-content recovery", id),
					Message:     err.Error() + "; the prior content and producer evidence were preserved; ordinary supported indexing remains eligible",
					Remediation: "Restore the retained transcript or native source and retry harvest; inspect the recovery error before forcing replacement.",
				})
				continue
			}
			recovered[id] = true
		}
	}
}

func (p *Pipeline) backfillContentSession(ctx context.Context, store ContentBackfillTargetStore, id SessionID) error {
	host, parent, err := store.LookupSessionLocation(ctx, id)
	if err != nil {
		return err
	}
	dir := filepath.Join(p.config.OutputDir.String(), host)
	if parent != "" {
		dir = filepath.Join(dir, parent, "subagents")
	}
	// An existing metadata artifact is authoritative even when corrupt. Never
	// reinterpret its failure as permission to substitute current provider data.
	metaPath := filepath.Join(dir, id.String(), id.String()+"--metadata.json")
	var session DiscoveredSession
	authority := ContentSourcePeasantSnapshot
	if _, statErr := p.fs.Stat(metaPath); statErr == nil {
		retained, readErr := p.readSessionMetadata(dir, id, "content backfill")
		if readErr != nil {
			return readErr
		}
		if retained == nil {
			return fmt.Errorf("content backfill %s: retained metadata or transcript unreadable; restore the retained artifact or regenerate harvest; no provider substitution attempted", id)
		}
		session = retained.session
		if session.Harness == HarnessOpenCode && session.TranscriptOrigin == TranscriptOriginFile {
			// A legacy JSON session header is not a transcript snapshot. Refresh
			// from its recorded provider directory only when that source still
			// exists, and label the different authority explicitly.
			path, err := NewResolvedPath(retained.originalSourcePath)
			if err != nil {
				return fmt.Errorf("content backfill %s: retained OpenCode header lacks its directory corpus; restore the original provider directory before retrying: %w", id, err)
			}
			if _, err := p.fs.Stat(path.String()); err != nil {
				return fmt.Errorf("content backfill %s: retained OpenCode header lacks its directory corpus and provider source is inaccessible; restore the original directory before retrying: %w", id, err)
			}
			session.SourcePath = path
			authority = ContentSourceProviderSource
		}
	} else {
		if !os.IsNotExist(statErr) {
			return fmt.Errorf("content backfill %s: retained metadata access failed: %w; restore access before retrying", id, statErr)
		}
		path, format, provider, lookupErr := store.LookupSourceInfo(ctx, id)
		if lookupErr != nil {
			return lookupErr
		}
		harness, err := captureHarness(provider)
		if err != nil {
			return err
		}
		if path == "" {
			return fmt.Errorf("content backfill %s: neither retained artifact nor original source is available; restore harvest data before retrying", id)
		}
		resolved, err := NewResolvedPath(path)
		if err != nil {
			return err
		}
		session = DiscoveredSession{SessionID: id, Harness: harness, SourcePath: resolved, SourceFormat: format}
		authority = ContentSourceProviderSource
	}
	indexer, ok := p.indexers[session.Harness].(AuthoritativeTranscriptIndexer)
	if !ok {
		return fmt.Errorf("content backfill %s: harness lacks authoritative parser; upgrade Peasant before retrying", id)
	}
	capture, err := indexer.IndexTranscriptForCapture(ctx, session)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	write := SessionEntryWrite{SessionID: id, Result: indexformat.V1{Entries: capture.Entries}, IndexVersion: 1, Mode: SessionEntryWriteContentBackfill, RequireFullContent: true,
		ContentCapture: SessionContentCaptureWrite{Status: ContentCaptureComplete, SourceAuthority: authority, TranscriptOrigin: session.TranscriptOrigin, CaptureRevision: ContentCaptureRevision, CapturedAtMs: now}}
	results := store.IndexSessionEntryBatch(ctx, []SessionEntryWrite{write})
	if len(results) != 1 {
		return fmt.Errorf("content backfill %s: store returned no atomic result; retry after checking database", id)
	}
	replaced := false
	if errors.Is(results[0].Err, ContentBackfillShapeMismatch) && p.config.Force {
		write.Mode = SessionEntryWriteReplaceAll
		write.IndexerVersion, write.IndexedAtMs = p.versionTargets()[session.Harness].IndexerVersion, now
		results = store.IndexSessionEntryBatch(ctx, []SessionEntryWrite{write})
		if len(results) != 1 {
			return fmt.Errorf("content backfill %s: force store returned no result", id)
		}
		replaced = results[0].Err == nil && results[0].Written
	}
	if results[0].Err != nil {
		return results[0].Err
	}
	if !results[0].Written {
		return fmt.Errorf("content backfill %s: store did not confirm the atomic write; inspect database before retrying", id)
	}
	if replaced {
		// Projection replacement invalidates tool-derived metrics and annotation
		// classifiers. Content-only recovery does not need these recomputations.
		if p.analyzer != nil {
			if _, err := p.analyzer.ComputeMetrics(ctx, []SessionID{id}); err != nil {
				slog.Warn("content captured but metrics recomputation failed; rerun harvest index", "session_id", id, "error", err)
			}
		}
		if err := p.stageAnnotate(ctx, []SessionID{id}, nil); err != nil {
			slog.Warn("content captured but annotation recomputation failed; rerun harvest index", "session_id", id, "error", err)
		}
	}
	if p.indexLogger != nil {
		entry := p.makeIndexLogEntry(indexedMeta{session: session, outputTranscriptPath: session.SourcePath.String()}, IndexOutcomeReindexed, len(capture.Entries), now, nil, nil)
		if err := p.indexLogger.LogIndexEntry(ctx, entry); err != nil {
			slog.Warn("content captured but index audit failed", "session_id", id, "error", err)
		}
	}
	return nil
}

func captureHarness(raw string) (Harness, error) {
	// The parser registry owns the supported capture inventory. Match its typed
	// keys at this raw-string boundary rather than maintaining a second menu.
	for harness := range NewIndexerRegistry(nil, IndexerRegistryOptions{}) {
		if harness.String() == raw {
			return harness, nil
		}
	}
	return "", fmt.Errorf("content capture: unsupported harness %q; select a supported source before retrying", raw)
}
