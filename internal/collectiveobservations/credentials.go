// Package collectiveobservations coordinates account-scoped local evidence.
package collectiveobservations

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// IdentityReader verifies the stable caller of the selected Village credential.
// It is a transport dependency, not an authorization rule implemented locally.
type IdentityReader interface {
	VerifyUser(context.Context) (schema.VillageUser, error)
}

var _ IdentityReader = (*village.VillageClient)(nil)

// Dependencies selects the credential profile and remote identity transport.
type Dependencies struct {
	Credentials              CredentialBackend
	ConfigHome               string
	NewIdentityReader        func(auth.Credentials) IdentityReader
	Store                    *store.Store
	Now                      func() time.Time
	FetchTimeout             time.Duration
	SourceBudgetBytes        int64
	LocallyHeldTranscriptIDs func(context.Context, schema.CollectiveObservationScope) ([]schema.TranscriptID, error)
}

// Service shares credential selection between offline readers and online
// observation operations. A snapshot is never itself remote authorization.
type Service struct {
	credentials        CredentialBackend
	configHome         string
	newIdentityReader  func(auth.Credentials) IdentityReader
	store              *store.Store
	now                func() time.Time
	operationTimeout   time.Duration
	sourceSlots        chan struct{}
	sourceBudgetBytes  int64
	localTranscriptIDs func(context.Context, schema.CollectiveObservationScope) ([]schema.TranscriptID, error)
}

// New uses the real Village transport unless a dependency is explicitly supplied.
func New(deps Dependencies) *Service {
	credentials := deps.Credentials
	if credentials == nil {
		credentials = diskCredentialBackend{}
	}
	factory := deps.NewIdentityReader
	if factory == nil {
		factory = func(creds auth.Credentials) IdentityReader {
			return village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)
		}
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	timeout := deps.FetchTimeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	budget := deps.SourceBudgetBytes
	if budget <= 0 {
		budget = 4 * village.ObservationResponseLimit
	}
	return &Service{credentials: credentials, configHome: deps.ConfigHome, newIdentityReader: factory, store: deps.Store, now: now, operationTimeout: timeout, sourceSlots: make(chan struct{}, 4), sourceBudgetBytes: budget, localTranscriptIDs: deps.LocallyHeldTranscriptIDs}
}

func (s *Service) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(village.WithObservationReadBudget(ctx, s.sourceBudgetBytes), s.operationTimeout)
}

// SelectedCredentials selects the saved private scope OFFLINE. It never logs
// in, verifies remotely, or falls back to the most recently used account.
func (s *Service) SelectedCredentials(ctx context.Context) (auth.CredentialSnapshot, error) {
	snapshot, err := s.credentials.Capture(ctx, s.configHome)
	if err != nil {
		return auth.CredentialSnapshot{}, err
	}
	creds := snapshot.Credentials()
	id, err := uuid.Parse(creds.UserID)
	if err != nil || id.String() != creds.UserID {
		return auth.CredentialSnapshot{}, fmt.Errorf("select private collective scope: saved user identity is not a canonical peasant online UUID; no private observations selected; sign in again (unfiltered local inventory remains available): %w", auth.ErrSignedOut)
	}
	if _, err := auth.NormalizeVillageURL(creds.VillageURL); err != nil {
		return auth.CredentialSnapshot{}, err
	}
	return snapshot, nil
}

// AuthenticatedCredentials verifies auth/me using a captured credential outside
// any local write lock. Every later domain commit must still call WithCurrent
// around its actual transaction; this preliminary check cannot replace it.
func (s *Service) AuthenticatedCredentials(ctx context.Context) (auth.CredentialSnapshot, error) {
	snapshot, _, err := s.authenticatedReader(ctx)
	return snapshot, err
}

func (s *Service) authenticatedReader(ctx context.Context) (auth.CredentialSnapshot, IdentityReader, error) {
	snapshot, err := s.SelectedCredentials(ctx)
	if err != nil {
		return auth.CredentialSnapshot{}, nil, err
	}
	return s.authenticatedSnapshot(ctx, snapshot)
}

func (s *Service) authenticatedSnapshot(ctx context.Context, snapshot auth.CredentialSnapshot) (auth.CredentialSnapshot, IdentityReader, error) {
	// Original-action variants must refuse rotation BEFORE constructing a
	// client, rather than silently selecting the new account for old effects.
	if err := snapshot.WithCurrent(ctx, func() error { return nil }); err != nil {
		return auth.CredentialSnapshot{}, nil, err
	}
	creds := snapshot.Credentials()
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return auth.CredentialSnapshot{}, nil, err
	}
	creds.VillageURL = base
	reader := s.newIdentityReader(creds)
	if reader == nil {
		return auth.CredentialSnapshot{}, nil, fmt.Errorf("verify collective observation scope: identity transport is missing; no observations were fetched; configure the peasant online client and retry")
	}
	user, err := sourceRead(ctx, s.sourceSlots, func() (schema.VillageUser, error) { return reader.VerifyUser(ctx) })
	if err != nil {
		return auth.CredentialSnapshot{}, nil, fmt.Errorf("verify collective observation scope before fetching: %w; no new metadata was installed; verify the selected login and retry explicitly", err)
	}
	if user.ID.String() != creds.UserID {
		return auth.CredentialSnapshot{}, nil, fmt.Errorf("verify collective observation scope: auth/me disagrees with the saved stable user UUID; no private metadata was selected or installed; sign in again to select the correct account: %w", auth.ErrSignedOut)
	}
	if err := snapshot.WithCurrent(ctx, func() error { return nil }); err != nil {
		return auth.CredentialSnapshot{}, nil, err
	}
	return snapshot, reader, nil
}
