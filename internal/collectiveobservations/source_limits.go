package collectiveobservations

import (
	"context"
	"errors"
	"net"

	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

type observationSourceContractError struct{ error }

func (e *observationSourceContractError) Unwrap() error { return e.error }

func sourceRead[T any](ctx context.Context, slots chan struct{}, read func() (T, error)) (T, error) {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		var empty T
		return empty, ctx.Err()
	}
	defer func() { <-slots }()
	value, err := read()
	if err == nil {
		return value, nil
	}
	var status *village.StatusError
	var network net.Error
	transportFailure := errors.As(err, &status) || errors.As(err, &network)
	cancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	if transportFailure || cancelled {
		return value, err
	}
	return value, &observationSourceContractError{error: err}
}

type boundedSource struct {
	Source
	slots chan struct{}
}

func (s *boundedSource) ReadOwnContributionDiscovery(ctx context.Context) ([]schema.VillageContributedCollective, error) {
	return sourceRead(ctx, s.slots, func() ([]schema.VillageContributedCollective, error) {
		return s.Source.ReadOwnContributionDiscovery(ctx)
	})
}

var _ Source = (*boundedSource)(nil)

func (s *boundedSource) VerifyUser(ctx context.Context) (schema.VillageUser, error) {
	return sourceRead(ctx, s.slots, func() (schema.VillageUser, error) { return s.Source.VerifyUser(ctx) })
}
func (s *boundedSource) ReadObservationMemberships(ctx context.Context) ([]schema.VillageUserGroup, error) {
	return sourceRead(ctx, s.slots, func() ([]schema.VillageUserGroup, error) { return s.Source.ReadObservationMemberships(ctx) })
}
func (s *boundedSource) ReadObservationCollective(ctx context.Context, id schema.VillageUUID) (schema.VillageGroupDetailRecord, error) {
	return sourceRead(ctx, s.slots, func() (schema.VillageGroupDetailRecord, error) { return s.Source.ReadObservationCollective(ctx, id) })
}
func (s *boundedSource) ReadOwnSubmissions(ctx context.Context, id schema.VillageUUID) ([]schema.VillageCollectiveSubmission, error) {
	return sourceRead(ctx, s.slots, func() ([]schema.VillageCollectiveSubmission, error) { return s.Source.ReadOwnSubmissions(ctx, id) })
}
func (s *boundedSource) ReadOwnShareEvents(ctx context.Context, id schema.TranscriptID, collective schema.VillageUUID) ([]schema.VillageShareEvent, error) {
	return sourceRead(ctx, s.slots, func() ([]schema.VillageShareEvent, error) { return s.Source.ReadOwnShareEvents(ctx, id, collective) })
}
func (s *boundedSource) ReadObservationOrganizations(ctx context.Context) ([]schema.VillageMetadataUserOrganization, error) {
	return sourceRead(ctx, s.slots, func() ([]schema.VillageMetadataUserOrganization, error) {
		return s.Source.ReadObservationOrganizations(ctx)
	})
}
func (s *boundedSource) ReadTranscriptCollectives(ctx context.Context, id schema.TranscriptID) ([]schema.VillageTranscriptCollective, error) {
	return sourceRead(ctx, s.slots, func() ([]schema.VillageTranscriptCollective, error) {
		return s.Source.ReadTranscriptCollectives(ctx, id)
	})
}
func (s *boundedSource) ReadOwnContributionIdentities(ctx context.Context) ([]schema.VillageUUID, error) {
	return sourceRead(ctx, s.slots, func() ([]schema.VillageUUID, error) { return s.Source.ReadOwnContributionIdentities(ctx) })
}
func (s *boundedSource) SearchObservationCollectives(ctx context.Context, name string) ([]schema.VillageCollectiveSearchResult, error) {
	return sourceRead(ctx, s.slots, func() ([]schema.VillageCollectiveSearchResult, error) {
		return s.Source.SearchObservationCollectives(ctx, name)
	})
}
func (s *boundedSource) ReadOrganizationCollectives(ctx context.Context, login string) ([]schema.VillageOrganizationCollective, error) {
	return sourceRead(ctx, s.slots, func() ([]schema.VillageOrganizationCollective, error) {
		return s.Source.ReadOrganizationCollectives(ctx, login)
	})
}
