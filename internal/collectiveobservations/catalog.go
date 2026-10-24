package collectiveobservations

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// SelectedScope is offline scope selection, never an authority assertion.
func (s *Service) SelectedScope(ctx context.Context) (schema.CollectiveObservationScope, error) {
	snapshot, err := s.SelectedCredentials(ctx)
	if err != nil {
		return schema.CollectiveObservationScope{}, err
	}
	creds := snapshot.Credentials()
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return schema.CollectiveObservationScope{}, err
	}
	return schema.NormalizeCollectiveObservationScope(base, schema.VillageUUID(creds.UserID))
}

func (s *Service) List(ctx context.Context) (schema.CollectiveObservationResponse, error) {
	if s.store == nil {
		return schema.CollectiveObservationResponse{}, fmt.Errorf("read saved collectives: Store is not configured; no metadata read; configure the local observation Store")
	}
	snapshot, err := s.SelectedCredentials(ctx)
	if err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	return s.store.ReadCollectiveObservations(ctx, snapshot)
}

// ReferenceError is actionable local input failure, not a remote permission
// decision. Choices contain only exact matches disclosed to this selected scope.
type ReferenceError struct {
	Message string
	Choices []schema.VillageUUID
}

func (e *ReferenceError) Error() string {
	if len(e.Choices) == 0 {
		return e.Message
	}
	return fmt.Sprintf("%s; permitted choices: %v; retry with one exact UUID", e.Message, e.Choices)
}

func referenceIdentity(base, ref string) (schema.VillageUUID, bool, error) {
	if village.IsCollectiveID(schema.VillageUUID(ref)) {
		return schema.VillageUUID(ref), true, nil
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return "", false, &ReferenceError{Message: "Collective reference is malformed; no selection made; use an exact saved name, UUID or selected-Village collective URL"}
	}
	if !parsed.IsAbs() && parsed.Host == "" {
		return "", false, nil
	}
	if parsed.User != nil || parsed.Opaque != "" {
		return "", false, &ReferenceError{Message: "Collective URL contains credentials or ambiguous framing; no credential was forwarded; use the canonical collective URL"}
	}
	selected, err := url.Parse(base)
	if err != nil {
		return "", false, err
	}
	for _, route := range []string{"/collectives/", "/groups/"} {
		prefix := strings.TrimRight(selected.Path, "/") + route
		if !strings.HasPrefix(parsed.Path, prefix) {
			continue
		}
		id := strings.TrimPrefix(parsed.Path, prefix)
		if !village.IsCollectiveID(schema.VillageUUID(id)) || parsed.RawPath != "" {
			return "", false, &ReferenceError{Message: "Collective URL does not name one canonical UUID; no selection made; copy its canonical page URL"}
		}
		origin := *parsed
		origin.Path = selected.Path
		origin.RawPath = ""
		origin.RawQuery = ""
		origin.Fragment = ""
		normalized, err := auth.NormalizeVillageURL(origin.String())
		if err != nil || normalized != base {
			return "", false, &ReferenceError{Message: "Collective URL belongs to a different peasant online installation or base path; no credentials forwarded; select that peasant online installation explicitly before retrying"}
		}
		return schema.VillageUUID(id), true, nil
	}
	return "", false, &ReferenceError{Message: "Collective URL does not belong to the selected peasant online base path; no credentials forwarded; use its exact canonical collective URL"}
}

func (s *Service) ResolveReference(ctx context.Context, ref string, online bool) (schema.VillageUUID, error) {
	original, err := s.SelectedCredentials(ctx)
	if err != nil {
		return "", err
	}
	return s.ResolveReferenceFor(ctx, original, ref, online)
}

// ResolveReferenceFor selects an exact reference in the original action scope.
// It never reselects a profile after a content action captured its credentials.
func (s *Service) ResolveReferenceFor(ctx context.Context, snapshot auth.CredentialSnapshot, ref string, online bool) (id schema.VillageUUID, err error) {
	if online {
		var cancel context.CancelFunc
		ctx, cancel = s.operationContext(ctx)
		defer cancel()
	}
	defer func() {
		if err == nil {
			err = s.credentials.CheckSnapshot(ctx, snapshot)
			if err != nil {
				id = ""
			}
		}
	}()
	if err := snapshot.WithCurrent(ctx, func() error { return nil }); err != nil {
		return "", err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", &ReferenceError{Message: "Collective reference is empty; no selection made; provide an exact name, UUID or collective URL"}
	}
	creds := snapshot.Credentials()
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return "", err
	}
	if id, matched, err := referenceIdentity(base, ref); matched || err != nil {
		if err != nil || !online {
			return id, err
		}
		_, source, err := s.AuthenticatedSourceFor(ctx, snapshot)
		if err != nil {
			return "", err
		}
		if err := s.issueExplicitTargetRead(ctx, snapshot, id); err != nil {
			return "", err
		}
		if _, err := source.ReadObservationCollective(ctx, id); err != nil {
			return "", err
		}
		if err := snapshot.WithCurrent(ctx, func() error { return nil }); err != nil {
			return "", err
		}
		return id, nil
	}
	ids := []schema.VillageUUID{}
	if online {
		_, source, err := s.AuthenticatedSourceFor(ctx, snapshot)
		if err != nil {
			return "", err
		}
		rows, err := source.SearchObservationCollectives(ctx, ref)
		if err != nil {
			return "", err
		}
		for _, row := range rows {
			if strings.EqualFold(row.Name, ref) {
				ids = append(ids, row.ID)
			}
		}
		if err := snapshot.WithCurrent(ctx, func() error { return nil }); err != nil {
			return "", err
		}
	} else {
		if s.store == nil {
			return "", fmt.Errorf("resolve saved collective: Store is not configured; no name resolved; configure the local observation Store")
		}
		ids, err = s.store.ResolveSavedCollectiveName(ctx, snapshot, ref)
		if err != nil {
			return "", err
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) == 0 {
		return "", &ReferenceError{Message: "No exact collective name matched in the selected scope; no selection made; use a canonical UUID or fetch metadata explicitly"}
	}
	if len(ids) != 1 {
		return "", &ReferenceError{Message: "Exact collective name is ambiguous in the selected scope", Choices: ids}
	}
	return ids[0], nil
}

func observationFromDetail(row schema.VillageCollectiveDetailRecord, reasons []schema.CollectiveObservationReason, s *Service) schema.CollectiveObservation {
	acceptance, access := row.AcceptanceMode, row.DataAccess
	return schema.CollectiveObservation{ID: row.ID, Name: row.Name, Description: row.Description, AcceptanceMode: &acceptance, DataAccess: &access, Reasons: reasons, ObservedAt: s.now().UTC(), State: schema.CollectiveObservationStateObserved}
}

// Track verifies online visibility BEFORE beginning any local preference write.
// This is neither joining a collective nor consenting to publication.
func (s *Service) Track(ctx context.Context, id schema.VillageUUID) (schema.CollectiveObservationResponse, error) {
	original, err := s.SelectedCredentials(ctx)
	if err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	return s.TrackFor(ctx, original, id)
}

// TrackFor verifies visibility and commits only under the original action's
// captured generation. No credential lock is held across its network reads.
func (s *Service) TrackFor(ctx context.Context, original auth.CredentialSnapshot, id schema.VillageUUID) (schema.CollectiveObservationResponse, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	if s.store == nil {
		return schema.CollectiveObservationResponse{}, fmt.Errorf("track collective: local Store is not configured; no preference written; configure the observation Store")
	}
	snapshot, source, err := s.AuthenticatedSourceFor(ctx, original)
	if err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	invocation, err := s.store.BeginCollectiveRead(ctx, snapshot, store.CollectiveReadPlan{Sources: []store.CollectiveReadSource{{Source: schema.CollectiveObservationSourceExplicit, ID: id.String()}}}, s.now().UTC())
	if err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	row, err := source.ReadObservationCollective(ctx, id)
	if err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	observation := observationFromDetail(row, []schema.CollectiveObservationReason{schema.CollectiveObservationReasonExplicit}, s)
	if err := s.store.TrackCollectiveObservation(ctx, invocation, observation); err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	return s.store.ReadCollectiveObservations(ctx, snapshot)
}

func (s *Service) Untrack(ctx context.Context, id schema.VillageUUID) (schema.CollectiveObservationResponse, error) {
	snapshot, err := s.SelectedCredentials(ctx)
	if err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	return s.UntrackFor(ctx, snapshot, id)
}

// UntrackFor removes a preference offline under the original action generation.
func (s *Service) UntrackFor(ctx context.Context, snapshot auth.CredentialSnapshot, id schema.VillageUUID) (schema.CollectiveObservationResponse, error) {
	if s.store == nil {
		return schema.CollectiveObservationResponse{}, fmt.Errorf("untrack collective: local Store is not configured; no preference changed; configure the observation Store")
	}
	invocation, err := s.store.BeginCollectiveObservation(ctx, snapshot)
	if err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	if err := s.store.UntrackCollectiveObservation(ctx, invocation, id, s.now().UTC()); err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	return s.store.ReadCollectiveObservations(ctx, snapshot)
}

// VerifyTargets fetches only named action sources, not full business history.
// The returned policies are observations, not a second permission engine: the
// final Village mutation must still authorize every requested target atomically.
func (s *Service) VerifyTargets(ctx context.Context, transcript schema.TranscriptID, ids []schema.VillageUUID) ([]schema.VillageCollectiveDetailRecord, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	original, err := s.SelectedCredentials(ctx)
	if err != nil {
		return nil, err
	}
	return s.VerifyTargetsFor(ctx, original, transcript, ids)
}

// VerifyTargetsFor obtains only named target projections with the ORIGINAL
// action snapshot; observations never replace final Village authorization.
func (s *Service) VerifyTargetsFor(ctx context.Context, original auth.CredentialSnapshot, transcript schema.TranscriptID, ids []schema.VillageUUID) ([]schema.VillageCollectiveDetailRecord, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	if _, err := schema.NewTranscriptID(transcript.String()); err != nil {
		return nil, err
	}
	if len(ids) > schema.MaxCollectiveFetchItems {
		return nil, &ReferenceError{Message: "Too many explicit targets; no action performed; split the explicit request within the metadata bound"}
	}
	snapshot, source, err := s.AuthenticatedSourceFor(ctx, original)
	if err != nil {
		return nil, err
	}
	rows := make([]schema.VillageCollectiveDetailRecord, 0, len(ids))
	seen := map[schema.VillageUUID]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if err := s.issueExplicitTargetRead(ctx, snapshot, id); err != nil {
			return nil, err
		}
		row, err := source.ReadObservationCollective(ctx, id)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	if err := snapshot.WithCurrent(ctx, func() error { return nil }); err != nil {
		return nil, err
	}
	return rows, nil
}

// Read-only preflights can supersede an older overlapping target observation;
// selecting a name alone never implies a new tracking preference.
func (s *Service) issueExplicitTargetRead(ctx context.Context, snapshot auth.CredentialSnapshot, id schema.VillageUUID) error {
	if s.store == nil {
		return nil
	}
	_, err := s.store.BeginCollectiveRead(ctx, snapshot, store.CollectiveReadPlan{Sources: []store.CollectiveReadSource{{Source: schema.CollectiveObservationSourceExplicit, ID: id.String()}}}, s.now().UTC())
	if err != nil {
		return err
	}
	return nil
}
