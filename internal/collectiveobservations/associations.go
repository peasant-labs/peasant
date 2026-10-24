package collectiveobservations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

func operationIssue(err error, operation schema.CollectiveOperation) schema.OperationIssue {
	code := schema.OperationIssueRemote
	var status *village.StatusError
	var conflict *store.ContributionAttemptConflictError
	var persistence *observationPersistenceError
	var contract *observationSourceContractError
	switch {
	case errors.Is(err, auth.ErrSignedOut), errors.Is(err, auth.ErrCredentialsChanged):
		code = schema.OperationIssueAuth
	case errors.As(err, &status) && status.StatusCode == http.StatusUnauthorized:
		code = schema.OperationIssueAuth
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		code = schema.OperationIssueDeadline
	case errors.Is(err, store.ErrObservationSuperseded):
		code = schema.OperationIssueSuperseded
	case errors.As(err, &conflict):
		code = schema.OperationIssueContract
	case errors.As(err, &contract):
		code = schema.OperationIssueContract
	case errors.As(err, &persistence):
		code = schema.OperationIssuePersistence
	}
	message := err.Error()
	if len(message) > schema.MaxCollectiveOperationIssueBytes {
		message = message[:schema.MaxCollectiveOperationIssueBytes]
	}
	return schema.OperationIssue{Code: code, Operation: operation, Message: message, Retry: "Check the selected account and peasant online connection, then explicitly retry metadata fetch; cached observations do not grant permission."}
}

// Associations reads saved per-transcript disclosure OFFLINE. A missing legacy
// observation stays absent, never becomes an inferred selector membership.
func (s *Service) Associations(ctx context.Context, ids []schema.TranscriptID) (map[schema.TranscriptID]store.TranscriptCollectiveObservation, error) {
	if s.store == nil {
		return nil, fmt.Errorf("read associations: local Store is not configured; no evidence read; configure the observation service Store")
	}
	snapshot, err := s.SelectedCredentials(ctx)
	if err != nil {
		return nil, err
	}
	return s.store.ReadTranscriptAssociations(ctx, snapshot, ids)
}

// ObserveAssociations is OPTIONAL metadata after independently authorized
// content success. It returns a schema result AND an error for failed metadata
// so explicit full fetch can fail while a pull adapter can warn and retain its
// content success. It never authorizes content or reads foreign owner history.
func (s *Service) ObserveAssociations(ctx context.Context, id schema.TranscriptID) (schema.PullAssociationMetadataResult, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	if s.store == nil {
		return schema.PullAssociationMetadataResult{}, fmt.Errorf("observe associations: local Store is not configured; content permission is unchanged; configure the observation service Store")
	}
	selected, err := s.SelectedCredentials(ctx)
	if err != nil {
		return schema.PullAssociationMetadataResult{}, err
	}
	return s.ObserveAssociationsFor(ctx, selected, id)
}

// ObserveAssociationsFor refreshes optional metadata for the original content
// action's account. Rotation cannot turn it into another principal's evidence.
func (s *Service) ObserveAssociationsFor(ctx context.Context, original auth.CredentialSnapshot, id schema.TranscriptID) (schema.PullAssociationMetadataResult, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	if s.store == nil {
		return schema.PullAssociationMetadataResult{}, fmt.Errorf("observe original content associations: local Store is missing; no evidence installed; configure the observation Store")
	}
	snapshot, source, err := s.AuthenticatedSourceFor(ctx, original)
	if err != nil {
		return s.associationFailureResult(ctx, original, id, err)
	}
	invocation, err := s.store.BeginCollectiveObservation(ctx, snapshot)
	if err != nil {
		return s.associationFailureResult(ctx, snapshot, id, err)
	}
	return s.observeAssociations(ctx, snapshot, source, invocation, id)
}

func (s *Service) observeAssociations(ctx context.Context, snapshot auth.CredentialSnapshot, source Source, invocation store.CollectiveObservationInvocation, id schema.TranscriptID) (schema.PullAssociationMetadataResult, error) {
	if err := s.store.IssueCollectiveReads(ctx, invocation, store.CollectiveReadPlan{Transcripts: []schema.TranscriptID{id}}, s.now().UTC()); err != nil {
		return s.associationFailureResult(ctx, snapshot, id, persistenceError(err))
	}
	rows, readErr := source.ReadTranscriptCollectives(ctx, id)
	invalidated := false
	var status *village.StatusError
	if errors.As(readErr, &status) && (status.StatusCode == http.StatusForbidden || status.StatusCode == http.StatusNotFound) {
		invalidated = true
		rows = []schema.VillageTranscriptCollective{}
		readErr = nil
	}
	at := s.now().UTC()
	if readErr == nil {
		readErr = persistenceError(s.store.InstallTranscriptAssociations(ctx, invocation, id, rows, at, invalidated))
	}
	if readErr != nil {
		issue := operationIssue(readErr, schema.CollectiveOperationPull)
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.store.RecordTranscriptAssociationFailure(cleanup, invocation, id, at, issue); err != nil {
			return s.associationFailureResult(cleanup, snapshot, id, fmt.Errorf("persist optional association failure: %w; original source error: %v", err, readErr))
		}
		return s.associationFailureResult(cleanup, snapshot, id, readErr)
	}
	metadata := schema.PullAssociationMetadataResult{State: schema.PullAssociationMetadataObserved, ObservedAt: &at}
	if invalidated {
		metadata.State = schema.PullAssociationMetadataInvalidated
	}
	return metadata, nil
}

func (s *Service) associationFailureResult(ctx context.Context, snapshot auth.CredentialSnapshot, id schema.TranscriptID, cause error) (schema.PullAssociationMetadataResult, error) {
	issue := operationIssue(cause, schema.CollectiveOperationPull)
	result := schema.PullAssociationMetadataResult{State: schema.PullAssociationMetadataUnknown, Warning: &issue}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	rows, err := s.store.ReadTranscriptAssociations(cleanup, snapshot, []schema.TranscriptID{id})
	if err != nil {
		return result, fmt.Errorf("read retained association metadata after source failure: %w; source: %v", err, cause)
	}
	if prior := rows[id]; prior.Metadata != nil && prior.Metadata.ObservedAt != nil {
		result.State = schema.PullAssociationMetadataStale
		at := *prior.Metadata.ObservedAt
		result.ObservedAt = &at
	}
	if err := result.Validate(); err != nil {
		return schema.PullAssociationMetadataResult{}, err
	}
	return result, cause
}
