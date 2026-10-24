package collectiveobservations

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/schema"
)

// History is an offline saved-scope read; no Village transport is constructed.
func (s *Service) History(ctx context.Context, q schema.LocalContributionHistoryQuery) (schema.LocalContributionHistoryResponse, error) {
	snapshot, err := s.SelectedCredentials(ctx)
	if err != nil {
		return schema.LocalContributionHistoryResponse{}, err
	}
	return s.HistoryFor(ctx, snapshot, q)
}

// HistoryFor reads saved evidence offline under the original action generation.
func (s *Service) HistoryFor(ctx context.Context, snapshot auth.CredentialSnapshot, q schema.LocalContributionHistoryQuery) (schema.LocalContributionHistoryResponse, error) {
	if s.store == nil {
		return schema.LocalContributionHistoryResponse{}, fmt.Errorf("read contribution history: local Store is not configured; no private evidence was read; configure the observation service Store")
	}
	return s.store.ReadContributionHistory(ctx, snapshot, q)
}

// RecordReceipt binds the actual action effect to its originally captured
// credential generation. A remote effect and a failed local commit remain
// separate results at the caller; this function never claims remote rollback.
func (s *Service) RecordReceipt(ctx context.Context, snapshot auth.CredentialSnapshot, receipt schema.LocalConfirmedContributionReceipt) error {
	if s.store == nil {
		return fmt.Errorf("record confirmed contribution: local Store is not configured; remote effects are not undone; configure the Store and fetch history to reconcile")
	}
	return s.store.RecordContributionReceipt(ctx, snapshot, receipt)
}

func (s *Service) RecordDiagnostic(ctx context.Context, snapshot auth.CredentialSnapshot, diagnostic schema.LocalOperationDiagnostic) error {
	if s.store == nil {
		return fmt.Errorf("record operation diagnostic: local Store is not configured; the operation outcome remains unchanged; configure the Store before retrying")
	}
	return s.store.RecordOperationDiagnostic(ctx, snapshot, diagnostic)
}
