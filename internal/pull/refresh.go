package pull

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// refreshPageLimit is the page size used when enumerating the requester's own
// pushed transcripts from the village's pullable listing during a refresh.
const refreshPageLimit = 100

// RefreshOwnAnnotations refreshes the FOREIGN annotations on the requester's OWN
// pushed transcripts. It enumerates the requester's own transcripts from the
// village pullable listing (OwnerUserID == creds.UserID; optionally narrowed to a
// single local session via opts.SessionID), fetches each transcript's
// annotations, EXCLUDES the requester's own authored rows (AuthorUserID ==
// creds.UserID — the own-author exclusion), and upserts the remaining foreign
// rows via the store's single-transaction UpsertPulledAnnotations.
//
// This path touches NO files (annotations only), so a failure simply leaves the
// pulled_annotations table unchanged. NEGOTIATE runs EXACTLY ONCE before the
// FETCH stages, mirroring PullTranscript.
func (p *Pipeline) RefreshOwnAnnotations(ctx context.Context, opts RefreshOptions) (*RefreshResult, error) {
	// --- AUTH-CHECK ---
	if !p.creds.IsLoggedIn() {
		return &RefreshResult{Status: PullStatusNotLoggedIn}, fmt.Errorf(
			"peasant village annotations sync: not logged in\n" +
				"  what: no valid village credentials are present\n" +
				"  why:  refreshing annotations contacts the village and requires authentication\n" +
				"  fix:  run `peasant village login`, then re-run the sync")
	}
	host := p.villageHost()

	// --- NEGOTIATE (exactly once) ---
	if err := p.reader.NegotiatePull(ctx); err != nil {
		return &RefreshResult{Status: classifyStatus(err), VillageHost: host}, err
	}

	// --- FETCH-META: enumerate own pushed transcripts (paginated) ---
	own, err := p.listOwnTranscripts(ctx, opts.SessionID)
	if err != nil {
		return &RefreshResult{Status: classifyStatus(err), VillageHost: host}, err
	}

	result := &RefreshResult{Status: PullStatusUpToDate, VillageHost: host, TranscriptsScanned: len(own)}

	now := p.clock.NowUnixMilli()
	var rows []store.PulledAnnotationRow

	// --- FETCH-ANNOTATIONS (to memory) + own-author exclusion ---
	for i := range own {
		info := &own[i]
		annotations, err := p.reader.GetPullTranscriptAnnotations(ctx, info.TranscriptID)
		if err != nil {
			return &RefreshResult{Status: classifyStatus(err), VillageHost: host, TranscriptsScanned: len(own)}, err
		}
		if err := validatePulledAnnotations(annotations); err != nil {
			return &RefreshResult{Status: PullStatusError, VillageHost: host, TranscriptsScanned: len(own)}, err
		}
		for j := range annotations {
			a := &annotations[j]
			if a.AuthorUserID == p.creds.UserID {
				result.Excluded++ // own-authored: never re-enter the local store
				continue
			}
			rows = append(rows, p.refreshAnnotationRow(host, now, info, a))
		}
	}

	// --- DB-TX (single transaction) ---
	created, updated, skipped, err := p.store.UpsertPulledAnnotations(ctx, rows)
	if err != nil {
		return &RefreshResult{Status: PullStatusError, VillageHost: host, TranscriptsScanned: len(own), Excluded: result.Excluded}, fmt.Errorf(
			"peasant village annotations sync: persist foreign annotations @ %s: %w", host, err)
	}
	result.Created, result.Updated, result.Skipped = created, updated, skipped
	if created > 0 || updated > 0 {
		result.Status = PullStatusPulled
	}
	return result, nil
}

// listOwnTranscripts enumerates the requester's own pushed transcripts from the
// village pullable listing (OwnerUserID == creds.UserID), paging until exhausted.
// When sessionID is non-empty, only the transcript whose LocalID matches it is
// returned (the --session narrowing).
func (p *Pipeline) listOwnTranscripts(ctx context.Context, sessionID string) ([]schema.PullTranscriptInfo, error) {
	var own []schema.PullTranscriptInfo
	for page := 1; ; page++ {
		resp, err := p.reader.ListPullableTranscripts(ctx, page, refreshPageLimit)
		if err != nil {
			return nil, err
		}
		if resp == nil || len(resp.Transcripts) == 0 {
			break
		}
		for i := range resp.Transcripts {
			info := resp.Transcripts[i]
			if info.OwnerUserID != p.creds.UserID {
				continue // only OWN pushed transcripts carry foreign annotations we refresh
			}
			if sessionID != "" && info.LocalID != sessionID {
				continue
			}
			own = append(own, info)
		}
		// Stop once we have seen every advertised transcript (offset pagination).
		if resp.Total > 0 && page*refreshPageLimit >= resp.Total {
			break
		}
		if len(resp.Transcripts) < refreshPageLimit {
			break
		}
	}
	return own, nil
}

// refreshAnnotationRow projects a foreign PullAnnotation into a V34
// pulled_annotations row for the refresh path. TranscriptID may target an OWN
// pushed transcript that has NO pulled_transcripts row (refresh is independent of
// the transcript-pull path — the V34 schema intentionally has no FK).
func (p *Pipeline) refreshAnnotationRow(host string, now int64, info *schema.PullTranscriptInfo, a *schema.PullAnnotation) store.PulledAnnotationRow {
	payload, _ := json.Marshal(a)
	return store.PulledAnnotationRow{
		VillageHost:    host,
		ContentHash:    annotationContentHash(a),
		TranscriptID:   info.TranscriptID,
		LocalSessionID: info.LocalID,
		AuthorUserID:   a.AuthorUserID,
		AuthorUsername: a.AuthorUsername,
		Payload:        string(payload),
		PulledAt:       now,
	}
}
