package push

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// PipelineOption injects action dependencies before a pipeline is run.
type PipelineOption func(*Pipeline)

// WithObservations connects the same scoped service used by local metadata
// readers to a consented action. Dry runs never capture credentials or contact it.
func WithObservations(service *collectiveobservations.Service, now func() time.Time) PipelineOption {
	return func(p *Pipeline) {
		if now == nil {
			now = time.Now
		}
		p.observations = &actionObservations{service: service, now: now}
	}
}

type actionObservations struct {
	service  *collectiveobservations.Service
	now      func() time.Time
	snapshot auth.CredentialSnapshot
	ready    bool
}

func (a *actionObservations) capture(ctx context.Context, expected *auth.Credentials) error {
	a.ready = false
	if a.service == nil {
		return fmt.Errorf("prepare publication observations: scoped service is missing; nothing was sent; configure the local Store-backed observation service and retry")
	}
	snapshot, err := a.service.AuthenticatedCredentials(ctx)
	if err != nil {
		return err
	}
	saved := snapshot.Credentials()
	changed := expected == nil
	if expected != nil {
		changed = saved.UserID != expected.UserID || saved.APIKey != expected.APIKey || saved.VillageURL != expected.VillageURL
	}
	if changed {
		return fmt.Errorf("prepare publication observations: selected credentials changed since this action was prepared; nothing was sent; retry explicitly with the selected account: %w", auth.ErrCredentialsChanged)
	}
	a.snapshot, a.ready = snapshot, true
	return nil
}

func (a *actionObservations) current(ctx context.Context, commit func() error) error {
	if a == nil {
		return commit()
	}
	if !a.ready {
		return fmt.Errorf("publication account was not captured; no new action or evidence was committed; retry the publication from its mounted entry point")
	}
	return a.snapshot.WithCurrent(ctx, commit)
}

func actionOperationID() schema.VillageUUID {
	// uuid.NewString produces a canonical UUID, not a remote attempt ordinal.
	return schema.VillageUUID(uuid.NewString())
}

func (a *actionObservations) published(ctx context.Context, receipt schema.AuthoritativePublishResponse, operationID schema.VillageUUID) error {
	if a == nil {
		return nil
	}
	source := string(receipt.RequestOperationFingerprint)
	err := a.service.RecordReceipt(ctx, a.snapshot, schema.LocalConfirmedContributionReceipt{
		ReceiptID: operationID.String(), Operation: schema.CollectiveOperationPublish,
		TranscriptID: receipt.TranscriptID, ConfirmedAt: a.now().UTC(),
		Outcome: schema.LocalConfirmedContributionPublished, SourceReceiptID: &source,
	})
	if err != nil {
		return fmt.Errorf("Peasant online confirmed the content publication, but recording its local historical receipt failed; the remote content is not rolled back and no collective approval is implied; run 'peasant village collectives fetch' with the original account to reconcile before explicitly retrying: %w", err)
	}
	return nil
}

func (a *actionObservations) failed(ctx context.Context, operation schema.CollectiveOperation, transcript *schema.TranscriptID, collective *schema.VillageUUID, operationID schema.VillageUUID, primary error) error {
	if a == nil {
		return primary
	}
	outcome := schema.LocalOperationDiagnosticUncertain
	var status *village.StatusError
	if errors.As(primary, &status) && status.StatusCode >= http.StatusBadRequest && status.StatusCode < http.StatusInternalServerError {
		outcome = schema.LocalOperationDiagnosticFailed
	}
	// Do not persist raw transport errors: response bodies may contain secrets
	// or withheld resource detail. The typed outcome and corrective text suffice.
	err := a.service.RecordDiagnostic(ctx, a.snapshot, schema.LocalOperationDiagnostic{
		OperationID: operationID, Operation: operation, TranscriptID: transcript, CollectiveID: collective,
		AttemptedAt: a.now().UTC(), Outcome: outcome,
		Issue: schema.OperationIssue{Code: schema.OperationIssueRemote, Operation: operation,
			Message: "The requested peasant online action did not return a confirmed successful result; a missing response does not prove that peasant online made no change.",
			Retry:   "Inspect the transcript on peasant online and run 'peasant village collectives fetch' with the original account; retry explicitly only after checking the actual outcome."},
	})
	if err != nil {
		return &actionDiagnosticPersistenceError{primary: primary, persistence: err}
	}
	return primary
}

// Keep the two failure stages typed until the result is rendered. In
// particular, a mutation's HTTP recovery must not erase this local failure.
type actionDiagnosticPersistenceError struct {
	primary     error
	persistence error
}

const diagnosticPersistenceWarning = "The local diagnostic could not be persisted; no new diagnostic was saved on this computer. Inspect peasant online and fetch history with the original account before explicitly retrying"

func (e *actionDiagnosticPersistenceError) Error() string {
	return e.primary.Error() + "; " + diagnosticPersistenceWarning
}

func (e *actionDiagnosticPersistenceError) Unwrap() []error {
	return []error{e.primary, e.persistence}
}

// CollectiveSharer returns the action adapter for both the local publish route
// and rule-driven hooks. The underlying Village mutation remains authoritative.
func (p *Pipeline) CollectiveSharer(client CollectiveSharer) CollectiveSharer {
	if p.observations == nil {
		return client
	}
	return &observedSharer{CollectiveSharer: client, observations: p.observations}
}

type observedSharer struct {
	CollectiveSharer
	observations *actionObservations
}

var _ CollectiveSharer = (*observedSharer)(nil)

type collectiveBatchTransport interface {
	ShareTranscriptCollectives(context.Context, schema.TranscriptID, []schema.VillageUUID) error
}

var _ collectiveBatchTransport = (*village.VillageClient)(nil)
var _ collectiveActionBatch = (*observedSharer)(nil)

func (s *observedSharer) shareCollectives(ctx context.Context, transcript schema.TranscriptID, ids []schema.VillageUUID) ([]schema.SyncPushStepResult, bool) {
	if len(ids) == 0 {
		return nil, false
	}
	a := s.observations
	preflight := a.current(ctx, func() error { return nil })
	if preflight == nil {
		_, preflight = a.service.VerifyTargetsFor(ctx, a.snapshot, transcript, ids)
	}
	if preflight == nil {
		preflight = a.current(ctx, func() error { return nil })
	}
	transport, ok := s.CollectiveSharer.(collectiveBatchTransport)
	if !ok && preflight == nil {
		preflight = fmt.Errorf("share transport cannot send all requested targets atomically; no share was sent or queued; configure the production peasant online batch adapter before retrying")
	}
	steps := make([]schema.SyncPushStepResult, 0, len(ids))
	if preflight != nil {
		for _, id := range ids {
			target := id
			steps = append(steps, schema.SyncPushStepResult{Step: schema.SyncPushStepAddCollective, CollectiveID: &target, Outcome: schema.SyncPushStepFailed, Reason: "verify the requested collective sources before sharing: " + preflight.Error() + "; no share was sent or queued; check current access on peasant online and retry explicitly without changing audience or license"})
		}
		return steps, false
	}
	// One request carries ALL targets to final Village authority. Local evidence
	// commits below are independent and can fail without rolling it back.
	operationIDs := make([]schema.VillageUUID, len(ids))
	for i := range ids {
		operationIDs[i] = actionOperationID()
	}
	requestErr := transport.ShareTranscriptCollectives(ctx, transcript, ids)
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	var refusal *village.StatusError
	duplicate := errors.As(requestErr, &refusal) && refusal.StatusCode == http.StatusConflict
	missingTranscript := refusal != nil && refusal.StatusCode == http.StatusNotFound
	for i, id := range ids {
		target := id
		err := requestErr
		if err != nil && !duplicate {
			err = a.failed(persistCtx, schema.CollectiveOperationShare, &transcript, &target, operationIDs[i], err)
		}
		step := readShareResult(ctx, s, transcript, schema.SyncPushStepResult{Step: schema.SyncPushStepAddCollective, CollectiveID: &target}, err)
		if requestErr == nil {
			switch step.Outcome {
			case schema.SyncPushStepSucceeded, schema.SyncPushStepPendingApproval:
				err = a.service.RecordReceipt(persistCtx, a.snapshot, schema.LocalConfirmedContributionReceipt{ReceiptID: operationIDs[i].String(), Operation: schema.CollectiveOperationShare, TranscriptID: transcript, CollectiveID: &target, ConfirmedAt: a.now().UTC(), Outcome: schema.LocalConfirmedContributionSubmitted})
				if err != nil {
					step.Outcome = schema.SyncPushStepFailed
					step.Reason = "Peasant online confirmed the share request but recording its local receipt failed; the remote effect is not rolled back; inspect peasant online and run 'peasant village collectives fetch' with the original account: " + err.Error()
				}
			default:
				// Legacy servers could answer success while silently skipping a
				// target. No live readback means no confirmed contribution receipt.
				if step.Outcome == schema.SyncPushStepSkipped {
					step.Outcome = schema.SyncPushStepFailed
					step.Reason += "; the requested share was not confirmed; check current policy and membership and retry explicitly"
				}
				err = a.failed(persistCtx, schema.CollectiveOperationShare, &transcript, &target, operationIDs[i], errors.New(step.Reason))
				step.Reason = err.Error()
			}
		}
		confirmed := step.Outcome == schema.SyncPushStepSucceeded || step.Outcome == schema.SyncPushStepPendingApproval
		if confirmed || (duplicate && step.Outcome == schema.SyncPushStepSkipped) {
			if err := s.observePair(ctx, transcript, target); err != nil {
				step.Outcome, step.Reason = schema.SyncPushStepFailed, err.Error()
			}
		}
		steps = append(steps, step)
	}
	return steps, missingTranscript
}

func (s *observedSharer) ShareTranscript(ctx context.Context, transcript schema.TranscriptID, collective schema.VillageUUID) error {
	a := s.observations
	if err := a.current(ctx, func() error { return nil }); err != nil {
		return err
	}
	if _, err := a.service.VerifyTargetsFor(ctx, a.snapshot, transcript, []schema.VillageUUID{collective}); err != nil {
		return fmt.Errorf("verify the requested collective before sharing: %w; no share was sent or queued; check current access on peasant online and retry explicitly without changing audience or license", err)
	}
	if err := a.current(ctx, func() error { return nil }); err != nil {
		return err
	}
	operationID := actionOperationID()
	err := s.CollectiveSharer.ShareTranscript(ctx, transcript, collective)
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	if err != nil {
		var status *village.StatusError
		if errors.As(err, &status) && status.StatusCode == http.StatusConflict {
			return err // Existing live evidence must be read, not a new receipt.
		}
		return a.failed(persistCtx, schema.CollectiveOperationShare, &transcript, &collective, operationID, err)
	}
	status, readErr := s.LatestShareStatus(ctx, transcript, collective)
	if readErr != nil {
		return a.failed(persistCtx, schema.CollectiveOperationShare, &transcript, &collective, operationID,
			fmt.Errorf("Peasant online answered the share request, but its actual result could not be read; no collective approval is inferred; inspect peasant online before explicitly retrying: %w", readErr))
	}
	if status != schema.VillageShareStatusApproved && status != schema.VillageShareStatusPending {
		return a.failed(persistCtx, schema.CollectiveOperationShare, &transcript, &collective, operationID,
			&village.StatusError{StatusCode: http.StatusForbidden, Message: "Peasant online did not confirm a live share for the requested collective; no share was queued; check its current contribution policy and your membership and retry explicitly"})
	}
	// A live readback and successful mutation confirm submission, not approval.
	err = a.service.RecordReceipt(persistCtx, a.snapshot, schema.LocalConfirmedContributionReceipt{
		ReceiptID: operationID.String(), Operation: schema.CollectiveOperationShare,
		TranscriptID: transcript, CollectiveID: &collective, ConfirmedAt: a.now().UTC(),
		Outcome: schema.LocalConfirmedContributionSubmitted,
	})
	if err != nil {
		return fmt.Errorf("Peasant online confirmed the share request but its local receipt could not be recorded; the remote effect is not rolled back and approval is not inferred; inspect peasant online and run 'peasant village collectives fetch' with the original account: %w", err)
	}
	return s.observePair(ctx, transcript, collective)
}

func (s *observedSharer) observePair(ctx context.Context, transcript schema.TranscriptID, collective schema.VillageUUID) error {
	_, err := s.observations.service.ObserveOwnPairFor(ctx, s.observations.snapshot, collective, transcript)
	if err != nil {
		return fmt.Errorf("Peasant online's action result is retained, but reading or recording its original-account contribution history failed; no confirmed receipt is erased and no remote rollback or approval is inferred; inspect peasant online and run 'peasant village collectives fetch' with the original account before retrying: %w", err)
	}
	return nil
}

func (s *observedSharer) LatestShareStatus(ctx context.Context, transcript schema.TranscriptID, collective schema.VillageUUID) (schema.VillageShareStatus, error) {
	if err := s.observations.current(ctx, func() error { return nil }); err != nil {
		return "", err
	}
	status, err := s.CollectiveSharer.LatestShareStatus(ctx, transcript, collective)
	if err != nil {
		return "", err
	}
	if err := s.observations.current(ctx, func() error { return nil }); err != nil {
		return "", err
	}
	return status, nil
}

func (s *observedSharer) UnshareTranscript(ctx context.Context, transcript schema.TranscriptID, collective schema.VillageUUID) error {
	if err := s.observations.current(ctx, func() error { return nil }); err != nil {
		return err
	}
	operationID := actionOperationID()
	err := s.CollectiveSharer.UnshareTranscript(ctx, transcript, collective)
	if err == nil {
		// Withdrawal is not a new submitted receipt. A later source refusal is
		// NOT a refusal of the already confirmed withdrawal, even for HTTP 404.
		if observationErr := s.observePair(ctx, transcript, collective); observationErr != nil {
			persistCtx, cancel := persistenceContext(ctx)
			defer cancel()
			code := schema.OperationIssueRemote
			if errors.Is(observationErr, context.DeadlineExceeded) || errors.Is(observationErr, context.Canceled) {
				code = schema.OperationIssueDeadline
			}
			diagnosticErr := s.observations.service.RecordDiagnostic(persistCtx, s.observations.snapshot, schema.LocalOperationDiagnostic{
				OperationID: actionOperationID(), Operation: schema.CollectiveOperationFetch,
				TranscriptID: &transcript, CollectiveID: &collective, AttemptedAt: s.observations.now().UTC(), Outcome: schema.LocalOperationDiagnosticFailed,
				Issue: schema.OperationIssue{Code: code, Operation: schema.CollectiveOperationFetch,
					Message: "The withdrawal was confirmed, but its contribution history could not be fetched or recorded; saved history remains dated.",
					Retry:   "Inspect peasant online and run 'peasant village collectives fetch' with the original account to reconcile history; do not republish merely because this metadata read failed."},
			})
			if diagnosticErr != nil {
				observationErr = errors.Join(observationErr, fmt.Errorf("the local history-failure diagnostic could not be recorded; no new diagnostic was saved; inspect peasant online and fetch with the original account: %w", diagnosticErr))
			}
			return &confirmedWithdrawalObservationError{cause: observationErr}
		}
		return nil
	}
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	return s.observations.failed(persistCtx, schema.CollectiveOperationShare, &transcript, &collective, operationID, err)
}

// confirmedWithdrawalObservationError preserves both the confirmed mutation
// stage and the underlying metadata cause. Callers must classify this stage
// before extracting any nested HTTP status from the independent source read.
type confirmedWithdrawalObservationError struct{ cause error }

func (e *confirmedWithdrawalObservationError) Error() string {
	return "the withdrawal was confirmed by peasant online, but its contribution history is incomplete; no remote rollback or transcript deletion is inferred; inspect peasant online and run 'peasant village collectives fetch' with the original account: " + e.cause.Error()
}

func (e *confirmedWithdrawalObservationError) Unwrap() error { return e.cause }
