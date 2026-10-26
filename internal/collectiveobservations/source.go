package collectiveobservations

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// Source reads complete validated Village inputs. Discovery and source errors
// remain explicit; callers must not substitute failed inputs with empty sets.
// This interface is checked against the actual production transport, not a
// parallel private wire model. History validation remains schema-owned.
type Source interface {
	IdentityReader
	ReadObservationMemberships(context.Context) ([]schema.VillageUserCollective, error)
	ReadObservationCollective(context.Context, schema.VillageUUID) (schema.VillageCollectiveDetailRecord, error)
	ReadOwnSubmissions(context.Context, schema.VillageUUID) ([]schema.VillageCollectiveSubmission, error)
	ReadOwnShareEvents(context.Context, schema.TranscriptID, schema.VillageUUID) ([]schema.VillageShareEvent, error)
	ReadObservationOrganizations(context.Context) ([]schema.VillageMetadataUserOrganization, error)
	ReadOwnContributionIdentities(context.Context) ([]schema.VillageUUID, error)
	ReadOwnContributionDiscovery(context.Context) ([]schema.VillageContributedCollective, error)
	SearchObservationCollectives(context.Context, string) ([]schema.VillageCollectiveSearchResult, error)
	ReadOrganizationCollectives(context.Context, string) ([]schema.VillageOrganizationCollective, error)
	ReadTranscriptCollectives(context.Context, schema.TranscriptID) ([]schema.VillageTranscriptCollective, error)
}

var _ Source = (*village.VillageClient)(nil)

// AuthenticatedSource returns the SAME transport that verified auth/me and its
// guarded credential snapshot. All later persistence still needs WithCurrent
// through the actual Store commit; this function holds no network/write lock.
func (s *Service) AuthenticatedSource(ctx context.Context) (auth.CredentialSnapshot, Source, error) {
	snapshot, err := s.SelectedCredentials(ctx)
	if err != nil {
		return auth.CredentialSnapshot{}, nil, err
	}
	return s.AuthenticatedSourceFor(ctx, snapshot)
}

// AuthenticatedSourceFor verifies the ORIGINAL action's captured profile and
// generation. It never reselects an account from current configuration.
func (s *Service) AuthenticatedSourceFor(ctx context.Context, original auth.CredentialSnapshot) (auth.CredentialSnapshot, Source, error) {
	snapshot, reader, err := s.authenticatedSnapshot(ctx, original)
	if err != nil {
		return auth.CredentialSnapshot{}, nil, err
	}
	source, ok := reader.(Source)
	if !ok {
		return auth.CredentialSnapshot{}, nil, fmt.Errorf("fetch collective observation sources: authenticated transport lacks complete source reads; no metadata was installed; configure the production peasant online source adapter before retrying")
	}
	return snapshot, &boundedSource{Source: source, slots: s.sourceSlots}, nil
}

// ObserveOwnPair is the targeted owner-history path, not a full history fetch.
// It authenticates, allocates source ordering before network work, reads the
// entire pair outside locks, and reconciles under the actual guarded Store
// transaction. It cannot be used for foreign transcript associations.
func (s *Service) ObserveOwnPair(ctx context.Context, collective schema.VillageUUID, transcript schema.TranscriptID) (store.ContributionAttemptInstall, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	if s.store == nil {
		return store.ContributionAttemptInstall{}, fmt.Errorf("observe own contribution pair: local Store is not configured; no evidence changed; configure the observation service with the selected local Store")
	}
	snapshot, source, err := s.AuthenticatedSource(ctx)
	if err != nil {
		return store.ContributionAttemptInstall{}, err
	}
	return s.observeOwnPair(ctx, snapshot, source, collective, transcript)
}

// ObserveOwnPairFor reconciles a complete owner source bound to an earlier
// action snapshot. Receipt confirmation remains separate from this lookup.
func (s *Service) ObserveOwnPairFor(ctx context.Context, original auth.CredentialSnapshot, collective schema.VillageUUID, transcript schema.TranscriptID) (store.ContributionAttemptInstall, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	if s.store == nil {
		return store.ContributionAttemptInstall{}, fmt.Errorf("observe original action history: local Store is missing; no evidence installed; configure the observation Store")
	}
	snapshot, source, err := s.AuthenticatedSourceFor(ctx, original)
	if err != nil {
		return store.ContributionAttemptInstall{}, err
	}
	return s.observeOwnPair(ctx, snapshot, source, collective, transcript)
}

func (s *Service) observeOwnPair(ctx context.Context, snapshot auth.CredentialSnapshot, source Source, collective schema.VillageUUID, transcript schema.TranscriptID) (store.ContributionAttemptInstall, error) {
	invocation, err := s.store.BeginCollectiveRead(ctx, snapshot, store.CollectiveReadPlan{Sources: []store.CollectiveReadSource{store.ContributionAttemptReadSource(collective, transcript)}}, s.now().UTC())
	if err != nil {
		return store.ContributionAttemptInstall{}, err
	}
	events, err := source.ReadOwnShareEvents(ctx, transcript, collective)
	if err != nil {
		return store.ContributionAttemptInstall{}, err
	}
	return s.store.InstallContributionAttemptSources(ctx, invocation, []store.ContributionAttemptSource{{CollectiveID: collective, TranscriptID: transcript, Events: events, ObservedAt: s.now().UTC()}})
}
