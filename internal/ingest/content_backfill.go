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

// RetainedContentCapturer is implemented by an indexer whose retained capture
// can tell that rows were filtered or omitted before it parsed them. It is the
// producer of ContentCaptureResult: the adapter, not the store, declares
// completeness. Indexers without it are captured through the strict parser,
// which refuses anything it cannot certify, so their result is complete.
type RetainedContentCapturer interface {
	CaptureRetainedContent(context.Context, DiscoveredSession) (ContentCaptureResult, error)
}

// UnrepresentedRecordError means a well-formed source record of a kind this
// build does not represent. It is distinct from a malformed record: the
// strict parser refuses to certify the transcript, but the tolerant
// projection can still store the represented entries as an incomplete
// capture so previews are not empty.
type UnrepresentedRecordError struct {
	Harness Harness
	Kind    string
}

func (e *UnrepresentedRecordError) Error() string {
	return fmt.Sprintf("unrepresented %s record %q; this build does not represent it, so the transcript cannot be certified complete", e.Harness, e.Kind)
}

// RetainedContentIncompleteError means the retained input is insufficient for
// a verified complete capture. No row was written: the stored capture stays
// incomplete and the session stays eligible for a native refresh.
type RetainedContentIncompleteError struct {
	SessionID SessionID
	Harness   Harness
}

func (e *RetainedContentIncompleteError) Error() string {
	return fmt.Sprintf("content recovery %s: the retained %s input omits rows the adapter filtered or could not read, so it cannot certify a complete capture; nothing was written and the stored capture stays incomplete; regenerate harvest from the complete native source", e.SessionID, e.Harness)
}

// ContentShapeMismatchError means the retained content parsed to a different
// canonical shape than the stored projection. Content-only recovery never
// replaces projection rows; a forced index run replaces them through the
// ordinary captured-input parse and conditional write.
type ContentShapeMismatchError struct {
	SessionID SessionID
}

func (e *ContentShapeMismatchError) Error() string {
	return fmt.Sprintf("content recovery %s: retained content does not match the stored projection shape; content-only recovery changed nothing; the ordinary index run replaces the entries with annotation remapping once the session is selected, and harvest index --force selects it explicitly", e.SessionID)
}

func (e *ContentShapeMismatchError) Unwrap() error { return ContentBackfillShapeMismatch }

// contentRecovery is one completed retained-content repair. Recovering content
// repairs the stored capture only; it does not complete the session, so the
// pipeline still evaluates the same session for independent adapter and
// indexer work afterwards.
type contentRecovery struct {
	session     DiscoveredSession
	entries     int
	recoveredAt int64
}

// backfillIncompleteContent traverses by key, not by offset or a repeated first
// page: a broken first snapshot cannot starve later recoverable sessions.
//
// Recovery obeys the same eligibility boundary as ordinary index maintenance:
// the explicit harness, session and age filters decide scope BEFORE any read
// or write, and a session whose stored producer or index format is newer than
// this build is refused before recovery, never downgraded. Out-of-scope rows
// receive no read, no write and no diagnostic.
func (p *Pipeline) backfillIncompleteContent(ctx context.Context) (map[SessionID]contentRecovery, error) {
	recovered := make(map[SessionID]contentRecovery)
	store, ok := p.metricsStore.(ContentBackfillTargetStore)
	if !ok || p.config.DryRun {
		return recovered, nil
	}
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		p.reportDiagnostic(DiagnosticEntry{
			ErrorType: "content_recovery_unavailable", Location: "retained-content recovery",
			Message:     "the configured store cannot read stored producer state, so retained-content recovery was not attempted; stored entries and producer evidence were preserved",
			Remediation: "Use a store that reports index state (SessionIndexStateReader) and retry harvest.",
		})
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
			scope := reindexTarget{session: DiscoveredSession{SessionID: id, Harness: target.Harness}, startMs: target.StartMs}
			if !p.includesIndexTarget(scope) {
				continue
			}
			// The same stored-metadata compatibility check the index selection
			// applies, reported the same way: EVERY failure of it goes to the
			// refusal funnel, not just the newer-schema one. A check that fails
			// per row (an unreadable project identifier, say) refuses this
			// session here and again at selection, and the two entries collapse
			// to one only because both are built by the funnel. The session is
			// refused before any retained read, store write or index-log entry.
			if err := p.checkStoredMetadataVersion(ctx, id); err != nil {
				p.reportMetadataRefusal(string(id), err)
				continue
			}
			state, err := reader.ReadIndexState(ctx, id)
			if err == nil && state == nil {
				err = fmt.Errorf("content recovery %s: the store lists the session as a recovery target but reports no index state for it", id)
			}
			if err == nil {
				err = p.checkIndexProducer(state)
			}
			if err != nil {
				if cancelErr := pipelineCancellation(ctx, err); cancelErr != nil {
					return recovered, cancelErr
				}
				p.reportDiagnostic(DiagnosticEntry{
					ErrorType: "content_recovery_refused", Location: fmt.Sprintf("session %s retained-content recovery", id),
					Message:     err.Error() + "; recovery was refused before any retained read or store write, so the stored entries and producer evidence were preserved",
					Remediation: "Use a Peasant build whose indexer is at least the stored producer revision and supports the stored index format, then retry harvest.",
				})
				continue
			}
			// A refusal this build already recorded against the producer it would
			// use again is settled: recovery would read the file, parse it
			// strictly, be refused the same way, and warn the user about a
			// condition they cannot act on until Peasant is upgraded. The index
			// path still evaluates the session, so changed bytes are still seen.
			if permanentRefusalIsSettled(state, p.versionTargets()[state.Harness]) {
				continue
			}
			recovery, err := p.backfillContentSession(ctx, store, id, state)
			if err != nil {
				if cancelErr := pipelineCancellation(ctx, err); cancelErr != nil {
					return recovered, cancelErr
				}
				var mismatch *ContentShapeMismatchError
				if p.config.Force && errors.As(err, &mismatch) {
					// The forced run replaces this projection through the ordinary
					// captured-input parse in the same invocation; a content-only
					// refusal is not something the user must act on here.
					slog.Debug("content backfill deferred to forced index replacement", "session_id", id)
					continue
				}
				slog.Warn("content backfill failed; existing canonical state unchanged", "session_id", id, "error", err)
				p.reportDiagnostic(DiagnosticEntry{
					ErrorType: "content_recovery_unavailable", Location: fmt.Sprintf("session %s retained-content recovery", id),
					Message:     err.Error() + "; the prior content and producer evidence were preserved; ordinary supported indexing remains eligible",
					Remediation: "Restore the retained transcript or native source and retry harvest; inspect the recovery error before forcing replacement.",
				})
				continue
			}
			recovered[id] = recovery
		}
	}
}

func (p *Pipeline) backfillContentSession(ctx context.Context, store ContentBackfillTargetStore, id SessionID, state *SessionIndexState) (contentRecovery, error) {
	host, parent, err := store.LookupSessionLocation(ctx, id)
	if err != nil {
		return contentRecovery{}, err
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
			return contentRecovery{}, readErr
		}
		if retained == nil {
			return contentRecovery{}, fmt.Errorf("content backfill %s: retained metadata or transcript unreadable; restore the retained artifact or regenerate harvest; no provider substitution attempted", id)
		}
		session = retained.session
		if session.Harness == HarnessOpenCode && session.TranscriptOrigin == TranscriptOriginFile {
			// A legacy JSON session header is not a transcript snapshot. Refresh
			// from its recorded provider directory only when that source still
			// exists, and label the different authority explicitly.
			path, err := NewResolvedPath(retained.originalSourcePath)
			if err != nil {
				return contentRecovery{}, fmt.Errorf("content backfill %s: retained OpenCode header lacks its directory corpus; restore the original provider directory before retrying: %w", id, err)
			}
			if _, err := p.fs.Stat(path.String()); err != nil {
				return contentRecovery{}, fmt.Errorf("content backfill %s: retained OpenCode header lacks its directory corpus and provider source is inaccessible; restore the original directory before retrying: %w", id, err)
			}
			session.SourcePath = path
			authority = ContentSourceProviderSource
		}
	} else {
		if !os.IsNotExist(statErr) {
			return contentRecovery{}, fmt.Errorf("content backfill %s: retained metadata access failed: %w; restore access before retrying", id, statErr)
		}
		path, format, provider, lookupErr := store.LookupSourceInfo(ctx, id)
		if lookupErr != nil {
			return contentRecovery{}, lookupErr
		}
		harness, err := captureHarness(provider)
		if err != nil {
			return contentRecovery{}, err
		}
		if path == "" {
			return contentRecovery{}, fmt.Errorf("content backfill %s: neither retained artifact nor original source is available; restore harvest data before retrying", id)
		}
		resolved, err := NewResolvedPath(path)
		if err != nil {
			return contentRecovery{}, err
		}
		session = DiscoveredSession{SessionID: id, Harness: harness, SourcePath: resolved, SourceFormat: format}
		authority = ContentSourceProviderSource
	}
	indexer, ok := p.indexers[session.Harness].(AuthoritativeTranscriptIndexer)
	if !ok {
		return contentRecovery{}, fmt.Errorf("content backfill %s: harness lacks authoritative parser; upgrade Peasant before retrying", id)
	}
	capture, err := p.captureRetainedContent(ctx, indexer, session, authority)
	if err != nil {
		return contentRecovery{}, err
	}
	if !capture.Complete {
		return contentRecovery{}, &RetainedContentIncompleteError{SessionID: id, Harness: session.Harness}
	}
	now := time.Now().UnixMilli()
	write := SessionEntryWrite{
		SessionID: id, Result: indexformat.V1{Entries: capture.Entries}, IndexVersion: strictIndexFormat,
		Mode: SessionEntryWriteContentBackfill, RequireFullContent: capture.Complete,
		// Content repair keeps the stored producer stamp and conditions the
		// write on the state read before parsing: a concurrent change refuses
		// this result instead of overwriting newer evidence.
		IndexerVersion: state.IndexerVersion,
		ExpectedState:  state,
		ContentCapture: SessionContentCaptureWrite{Status: ContentCaptureComplete, SourceAuthority: capture.Authority, TranscriptOrigin: session.TranscriptOrigin, CaptureFormat: ContentCaptureFormatFull, CapturedAtMs: now},
	}
	if state.IndexedAt != nil {
		write.IndexedAtMs = *state.IndexedAt
	}
	// Recovery never invents publication proof. The capture is bound to the
	// current metadata capture only when the index already is; otherwise the
	// ordinary index write binds it once its own pending-work check runs.
	if state.PublicationBound {
		write.CaptureRevision = state.PublicationCaptureRevision
	}
	// The input proof names the captured artifact and a positive producing
	// revision; a session without either keeps recovering its content but
	// cannot claim input identity until an ordinary index run establishes it.
	if state.ArtifactHash != nil && state.IndexerVersion >= 1 {
		write.IndexedInputHash = &capture.InputHash
	}
	results := store.IndexSessionEntryBatch(ctx, []SessionEntryWrite{write})
	if len(results) != 1 {
		return contentRecovery{}, fmt.Errorf("content backfill %s: store returned no atomic result; retry after checking database", id)
	}
	if errors.Is(results[0].Err, ContentBackfillShapeMismatch) {
		return contentRecovery{}, &ContentShapeMismatchError{SessionID: id}
	}
	if results[0].Err != nil {
		return contentRecovery{}, results[0].Err
	}
	if !results[0].Written {
		return contentRecovery{}, fmt.Errorf("content backfill %s: store did not confirm the atomic write; inspect database before retrying", id)
	}
	// The run-level recovery entry (contentRecoveryLogEntries) is the one
	// index-log row for this repair; it is persisted once by the finalize stage.
	return contentRecovery{session: session, entries: len(capture.Entries), recoveredAt: now}, nil
}

// captureRetainedContent produces the ContentCaptureResult for one retained
// session. An indexer that knows about filtered or omitted rows reports its
// own completeness; every other strict parser either certifies the retained
// bytes or refuses them. InputHash is the index input digest over the bytes
// actually parsed, so an ordinary index run later recognizes the same input.
func (p *Pipeline) captureRetainedContent(ctx context.Context, indexer AuthoritativeTranscriptIndexer, session DiscoveredSession, authority ContentSourceAuthority) (ContentCaptureResult, error) {
	if capturer, ok := indexer.(RetainedContentCapturer); ok {
		capture, err := capturer.CaptureRetainedContent(ctx, session)
		if err != nil {
			return ContentCaptureResult{}, err
		}
		capture.Authority = authority
		return capture, nil
	}
	if err := ctx.Err(); err != nil {
		return ContentCaptureResult{}, err
	}
	data, err := p.fs.ReadFile(session.SourcePath.String())
	if err != nil {
		return ContentCaptureResult{}, captureFailure(session, 0, err)
	}
	capture, err := indexer.IndexTranscriptBytesForCapture(ctx, session, data)
	if err != nil {
		return ContentCaptureResult{}, err
	}
	return ContentCaptureResult{Entries: capture.Entries, Authority: authority, Complete: true, InputHash: indexInputDigest(session, data, nil)}, nil
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
