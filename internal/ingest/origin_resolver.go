package ingest

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/sessionorigin"
)

// OriginRuleVersion is the version of the classification rule whose verdict is
// stored in sessions.session_origin. A row at a lower version has not been
// judged by this build's rule and is re-resolved on the next pass.
//
// Bump this when the rule changes: a new marker, a new harness adapter, a
// corrected ordering. Every stored row is then reclassified automatically, and
// no separate migration and no separate command is needed. That is why the
// backfill is a version watermark rather than a one-shot flag.
const OriginRuleVersion = 1

// StoredOriginRow is one already-persisted session the resolver has to judge,
// carrying the facts a verdict can be built from without reading anything else.
//
// StoredOrigin and OriginVersion describe what the row says TODAY, not what it
// should say. The resolver reads them so it can leave a row alone when nothing
// about it would change, which is what makes a second pass over a settled store
// write nothing at all.
type StoredOriginRow struct {
	SessionID SessionID
	// ParentID is the session this row is a child of, or empty when it is a
	// root. It is set at insert time and never updated afterwards, which is
	// what lets the resolver treat it as settled evidence.
	ParentID string
	// Harness names which producer recorded the transcript, and therefore which
	// miner - if any - can read origin evidence back out of it.
	Harness Harness
	// SourcePath is where the original transcript was read from. The file may
	// be gone; that is the degraded case.
	SourcePath ResolvedPath
	// StoredOrigin is the raw session_origin token on the row. It is a raw
	// string rather than an Origin on purpose: a value from storage is untrusted
	// here, and the resolver only ever compares it, never acts on it. A token
	// outside the menu therefore differs from every verdict and is overwritten.
	StoredOrigin string
	// OriginVersion is the rule version the stored verdict was written at.
	OriginVersion int
}

// ResolveReport is what one resolve pass did.
//
// Degraded counts the rows that had to be resolved without their transcript.
// Those rows keep their verdict but stay below the version line, so a later run
// revisits them. The set does NOT necessarily shrink to zero: a transcript can
// be permanently gone, and such a row is re-examined on every run forever, at a
// cost of one bulk query and no file reads. That is deliberate, and it is the
// price of never marking a store final on evidence that might still improve.
type ResolveReport struct {
	// Examined is how many stored rows the pass judged.
	Examined int
	// Written is how many of them it actually persisted a change for.
	Written int
	// Degraded is how many were resolved without their transcript and so were
	// left retryable.
	Degraded int
}

// OriginResolverStore is the persistence the resolve pass needs: the rows a
// newer rule has not judged, the update that records a verdict, and the stored
// first user message that is the last surviving content evidence when a
// transcript is gone.
type OriginResolverStore interface {
	// ListStaleOriginSessions returns every session whose origin_version is
	// below currentVersion.
	ListStaleOriginSessions(ctx context.Context, currentVersion int) ([]StoredOriginRow, error)
	// UpdateOriginState records a verdict and the version it was decided at, in
	// one statement, so the two can never disagree.
	UpdateOriginState(ctx context.Context, sessionID SessionID, origin string, version int) error
	// FirstUserMessageBulk returns the stored preview of each session's first
	// user message. Sessions with no user entry are omitted.
	FirstUserMessageBulk(ctx context.Context, sessionIDs []string) (map[string]string, error)
}

// OriginEvidenceMiner re-reads one transcript that is still on disk and reports
// the origin its harness recorded there.
//
// The second result is false when the transcript cannot be read: deleted,
// unmounted, or unreadable. That is the DEGRADED case, and a caller must not
// finalise such a row, because the same file may be readable on the next run.
type OriginEvidenceMiner interface {
	MineOriginEvidence(path ResolvedPath) (sessionorigin.Origin, bool)
}

// OriginResolver writes a verdict into every stored session at or below a rule
// version, using whatever evidence about each row still survives.
//
// It introduces no second classifier. It builds an Evidence value from the best
// surviving source and calls the same sessionorigin.Classify every other caller
// uses, so a change to the rule reaches these rows through the same edit that
// reaches new ones.
type OriginResolver struct {
	store  OriginResolverStore
	cache  ClaudeEvidenceCache
	miners map[Harness]OriginEvidenceMiner
}

// NewOriginResolver builds the resolve pass.
//
// cache is optional and is the evidence cache discovery just wrote; when it is
// present the pass answers most rows from it and never opens their transcripts
// a second time. miners is keyed by harness: a harness WITH a miner has its
// transcript treated as the best evidence, so losing that file is degraded; a
// harness WITHOUT one records no origin evidence this build can read, so the
// stored entries already are its complete evidence and its rows finalise.
func NewOriginResolver(store OriginResolverStore, cache ClaudeEvidenceCache, miners map[Harness]OriginEvidenceMiner) (*OriginResolver, error) {
	if store == nil {
		return nil, fmt.Errorf(
			"ingest: cannot build the stored-origin resolver without a store (ingest.NewOriginResolver): the pass " +
				"reads the rows a newer rule has not judged and writes their verdicts back, so it has nothing to do " +
				"and nothing to write without one; pass the local store, which implements OriginResolverStore",
		)
	}
	return &OriginResolver{store: store, cache: cache, miners: miners}, nil
}

// ResolveStoredOrigins writes a verdict into every session at or below the given
// rule version and returns what it did.
//
// It is idempotent and crash-safe: a row is advanced past the version line only
// after its verdict is committed, in the same statement, so an interrupted pass
// resumes rather than restarting on half-written state. Running it again over a
// settled store examines the rows still below the line and writes nothing.
//
// The four evidence sources, in order, and what each means for the watermark:
//
//  1. The row already has a parent. That is rule step one, decided with no file
//     read at all. FULL.
//  2. The evidence cache carries a menu origin for the row's source path. The
//     discovery this pass rides on has just refreshed it. FULL.
//  3. The transcript is still on disk and its harness has a miner that reads it.
//     The structured markers are all present. FULL.
//  4. The transcript is gone. The verdict is built from the stored first user
//     message and the row's parent, so a person's slash-command session is still
//     recognisable, but the agent markers are unrecoverable. DEGRADED: the
//     verdict is written, the version line is NOT advanced, and a later run
//     retries the row the moment its transcript is readable again.
//
// Source four can therefore only ever reach a person's session or unknown, never
// agent on marker evidence. That is the fail-safe direction: a row wrongly left
// visible is a nuisance, a row wrongly hidden is a person's own work disappearing.
//
// Ordering matters and is the caller's to keep. The pass must run BEFORE this
// run writes its own sessions, so that it sees only rows an EARLIER run
// persisted. That is what makes it safe against re-parenting: a parent
// identifier is written at insert time and never updated afterwards, so a row
// this pass finalises cannot acquire a parent later.
func (r *OriginResolver) ResolveStoredOrigins(ctx context.Context, ruleVersion int) (ResolveReport, error) {
	var report ResolveReport
	if ruleVersion < 1 {
		return report, fmt.Errorf(
			"ingest: rule version %d is not a usable origin watermark (ingest.OriginResolver.ResolveStoredOrigins): "+
				"the stored origin_version column starts at 0 for a row no rule has judged, so a pass at version %d "+
				"would list nothing and finalise nothing, leaving every row unjudged while reporting success; pass "+
				"ingest.OriginRuleVersion, which is the version of the rule this build actually carries",
			ruleVersion, ruleVersion,
		)
	}

	rows, err := r.store.ListStaleOriginSessions(ctx, ruleVersion)
	if err != nil {
		return report, fmt.Errorf("ingest: list sessions below origin rule version %d: %w", ruleVersion, err)
	}
	if len(rows) == 0 {
		return report, nil
	}

	cached := r.loadCache(ctx)

	// Decide everything that needs no content first, so the stored-message
	// query only covers the rows that actually need it.
	type verdict struct {
		origin   sessionorigin.Origin
		degraded bool
		decided  bool
	}
	verdicts := make([]verdict, len(rows))
	var needContent []string
	for i, row := range rows {
		switch {
		case row.ParentID != "":
			origin, _ := sessionorigin.Classify(sessionorigin.Evidence{HasParent: true})
			verdicts[i] = verdict{origin: origin, decided: true}
		default:
			if record, ok := cached[row.SourcePath]; ok && record.Origin.Valid() {
				verdicts[i] = verdict{origin: record.Origin, decided: true}
				continue
			}
			needContent = append(needContent, string(row.SessionID))
		}
	}

	firstUserText := map[string]string{}
	if len(needContent) > 0 {
		firstUserText, err = r.store.FirstUserMessageBulk(ctx, needContent)
		if err != nil {
			return report, fmt.Errorf("ingest: load stored first user messages for %d unresolved sessions: %w", len(needContent), err)
		}
	}

	for i, row := range rows {
		if verdicts[i].decided {
			continue
		}
		miner, mineable := r.miners[row.Harness]
		if mineable {
			if origin, ok := miner.MineOriginEvidence(row.SourcePath); ok {
				verdicts[i] = verdict{origin: origin, decided: true}
				continue
			}
		}
		// Either the transcript is unreachable, or this build reads no origin
		// evidence from that harness at all. Both fall back to the stored first
		// user message; only the first is retryable, because only the first can
		// ever answer differently later.
		origin, _ := sessionorigin.Classify(sessionorigin.Evidence{FirstUserText: firstUserText[string(row.SessionID)]})
		verdicts[i] = verdict{origin: origin, degraded: mineable, decided: true}
	}

	for i, row := range rows {
		decided := verdicts[i]
		report.Examined++
		if decided.degraded {
			report.Degraded++
		}
		targetVersion := ruleVersion
		if decided.degraded {
			// Keep the row exactly where it was found so the next run lists it
			// again. Writing a different version here would either finalise a
			// degraded verdict or silently move an older watermark.
			targetVersion = row.OriginVersion
		}
		if decided.origin.String() == row.StoredOrigin && targetVersion == row.OriginVersion {
			continue
		}
		if err := r.store.UpdateOriginState(ctx, row.SessionID, decided.origin.String(), targetVersion); err != nil {
			return report, fmt.Errorf(
				"ingest: write origin %q at version %d for stored session %s (ingest.OriginResolver.ResolveStoredOrigins): %w; "+
					"the %d rows already written keep their verdicts and the rest were not touched, so re-running the pass "+
					"resumes at the first row it did not reach",
				decided.origin, targetVersion, row.SessionID, err, report.Written,
			)
		}
		report.Written++
	}
	return report, nil
}

// loadCache returns the evidence records discovery has just refreshed. A missing
// or failing cache is not an error: the pass simply falls through to the miner,
// which is always correct and only slower.
func (r *OriginResolver) loadCache(ctx context.Context) map[ResolvedPath]ClaudeTranscriptEvidence {
	if r.cache == nil {
		return nil
	}
	records, err := r.cache.LoadClaudeEvidence(ctx)
	if err != nil {
		return nil
	}
	return records
}
