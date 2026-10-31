package push

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// ShareExistingDryRunReason is the reason on a step a dry run would have run.
const ShareExistingDryRunReason = "dry run: would share"

// shareExistingMissingReason replaces the receipt wording of a 404: a transcript
// named here may have no receipt on this computer, or may be someone else's.
const shareExistingMissingReason = "Peasant online answered that this account has no such transcript: it was deleted, or it is not yours; nothing was shared"

// UndecidedCollectives splits collectives into those to ask and a skipped step
// for each one that already has a share, live or decided. Only a collective
// that never had a share of the transcript is asked: a share that was approved,
// is pending, was rejected, or was taken back stays as its people left it.
func UndecidedCollectives(ctx context.Context, sharer CollectiveSharer, transcript schema.TranscriptID, collectives []schema.VillageUUID) (ask []schema.VillageUUID, skipped []schema.SyncPushStepResult, err error) {
	for _, id := range collectives {
		collective := id
		// The latest event of the share history says what the collective, or
		// the developer, last did with this transcript there.
		latest, err := sharer.LatestShareStatus(ctx, transcript, id)
		if err != nil {
			return nil, nil, err
		}
		if observed, ok := sharer.(*observedSharer); ok && latest != "" {
			if err := observed.observePair(ctx, transcript, id); err != nil {
				return nil, nil, err
			}
		}
		reason := ""
		switch latest {
		case "":
			ask = append(ask, id)
			continue
		case schema.VillageShareStatusApproved, schema.VillageShareStatusPending:
			reason = "the collective already holds this transcript or its share waits for approval"
		case schema.VillageShareStatusRejected:
			reason = "the collective's owner rejected this transcript, and a rule does not submit it again; share it again on peasant online if you want to"
		default:
			reason = "this transcript was taken back from the collective, and a rule does not share it again; share it again on peasant online if you want to"
		}
		skipped = append(skipped, schema.SyncPushStepResult{Step: schema.SyncPushStepAddCollective, CollectiveID: &collective, Outcome: schema.SyncPushStepSkipped, Reason: reason})
	}
	return ask, skipped, nil
}

// ShareExisting offers one transcript to the collectives and returns one step
// per collective, in order. With dryRun it sends nothing and reports the step
// it would run. Every asked collective goes in one request, so Village accepts
// or refuses the set as a whole, and the outcome of each is read back from its
// share history. The error is a failed read before anything was sent.
func ShareExisting(ctx context.Context, sharer CollectiveSharer, transcript schema.TranscriptID, collectives []schema.VillageUUID, dryRun bool) ([]schema.SyncPushStepResult, error) {
	ask, steps, err := UndecidedCollectives(ctx, sharer, transcript, collectives)
	if err != nil {
		return nil, err
	}
	switch {
	case len(ask) == 0:
	case dryRun:
		for _, id := range ask {
			target := id
			steps = append(steps, schema.SyncPushStepResult{Step: schema.SyncPushStepAddCollective, CollectiveID: &target, Outcome: schema.SyncPushStepNotAttempted, Reason: ShareExistingDryRunReason})
		}
	default:
		steps = append(steps, sendShare(ctx, sharer, transcript, ask)...)
	}
	return stepsInOrder(steps, collectives), nil
}

// sendShare makes the one share request for every asked collective.
func sendShare(ctx context.Context, sharer CollectiveSharer, transcript schema.TranscriptID, ask []schema.VillageUUID) []schema.SyncPushStepResult {
	var steps []schema.SyncPushStepResult
	missing := false
	if batch, ok := sharer.(collectiveActionBatch); ok {
		steps, missing = batch.shareCollectives(ctx, transcript, ask)
	} else {
		var requestErr error
		transport, ok := sharer.(collectiveBatchTransport)
		if ok {
			requestErr = transport.ShareTranscriptCollectives(ctx, transcript, ask)
			var refusal *village.StatusError
			missing = errors.As(requestErr, &refusal) && refusal.StatusCode == http.StatusNotFound
		}
		for _, id := range ask {
			target := id
			step := schema.SyncPushStepResult{Step: schema.SyncPushStepAddCollective, CollectiveID: &target}
			if !ok {
				step.Outcome = schema.SyncPushStepFailed
				step.Reason = "share transport cannot send all requested targets in one request; nothing was shared"
			} else {
				step = readShareResult(ctx, sharer, transcript, step, requestErr)
			}
			steps = append(steps, step)
		}
	}
	if missing {
		for i := range steps {
			steps[i].Reason = strings.ReplaceAll(steps[i].Reason, missingTranscriptReason, shareExistingMissingReason)
		}
	}
	return steps
}

// stepsInOrder lists the steps in the order the collectives were given.
func stepsInOrder(steps []schema.SyncPushStepResult, collectives []schema.VillageUUID) []schema.SyncPushStepResult {
	ordered := make([]schema.SyncPushStepResult, 0, len(steps))
	for _, id := range collectives {
		for _, step := range steps {
			if step.CollectiveID != nil && *step.CollectiveID == id {
				ordered = append(ordered, step)
			}
		}
	}
	return ordered
}

// ObserveSharer wraps a client so share events reach the collective
// observation store, as Pipeline.CollectiveSharer does for a pipeline. It
// checks that the saved account is still creds, because every later step acts
// for that account; on a refusal nothing was sent.
func ObserveSharer(ctx context.Context, service *collectiveobservations.Service, now func() time.Time, creds *auth.Credentials, client CollectiveSharer) (CollectiveSharer, error) {
	if now == nil {
		now = time.Now
	}
	observations := &actionObservations{service: service, now: now}
	if err := observations.capture(ctx, creds); err != nil {
		return nil, fmt.Errorf("prepare the share: %w", err)
	}
	return observeSharer(observations, client), nil
}
