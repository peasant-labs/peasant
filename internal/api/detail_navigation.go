package api

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// storedTargetLookup resolves owner-local session identifiers to stored
// targets. ResolveStoredTargets is the production implementation; it applies
// neither origin scope nor selection scope, because selection scopes discovery
// and lists only and never gates access to an already-stored session.
type storedTargetLookup func(ctx context.Context, ids []string) ([]StoredTarget, error)

// knownRelationshipTarget reports whether a durable relationship target state
// names a specific owner-local session that navigation may link to.
func knownRelationshipTarget(state schema.RelationshipTargetState) bool {
	return state == schema.RelationshipTargetKnown || state == schema.RelationshipTargetKnownRetained
}

// resolveRelationshipNavigation maps the durable session relationships carried
// on a validated detail payload to the additive read-only navigation the local
// host renders before the first main turn.
//
// Local resolution uses the stored exact target identifier, never list
// selection: a parent that is stored but filtered out of discovery stays
// linkable. A known target that is absent from the store becomes
// known_unavailable with NO identifier, so an unresolved link can never leak a
// hidden title or route to the wrong session. An unknown or conflicting target
// state stays explicit rather than guessed. An explicit-none relationship
// carries no linkable target and contributes no navigation entry.
//
// Navigation pairs by relationship kind, so a context_from and a started_by
// relationship that name different sessions produce two distinct entries.
func resolveRelationshipNavigation(ctx context.Context, relationships []schema.SessionRelationship, lookup storedTargetLookup) ([]schema.SessionRelationshipNavigation, error) {
	linkable := make([]schema.SessionRelationship, 0, len(relationships))
	for _, relationship := range relationships {
		if knownRelationshipTarget(relationship.TargetState) && relationship.TargetLocalID != nil {
			linkable = append(linkable, relationship)
		}
	}
	found := make(map[string]struct{}, len(linkable))
	if len(linkable) > 0 {
		ids := make([]string, 0, len(linkable))
		for _, relationship := range linkable {
			ids = append(ids, string(*relationship.TargetLocalID))
		}
		targets, err := lookup(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("api.resolveRelationshipNavigation: resolve stored navigation targets for %d relationship(s): %w; relationship links are unavailable and no navigation was emitted",
				len(linkable), err)
		}
		for _, target := range targets {
			if target.Found {
				found[target.ID] = struct{}{}
			}
		}
	}

	nav := make([]schema.SessionRelationshipNavigation, 0, len(relationships))
	for _, relationship := range relationships {
		entry := schema.SessionRelationshipNavigation{Kind: relationship.Kind}
		switch {
		case knownRelationshipTarget(relationship.TargetState):
			if relationship.TargetLocalID != nil {
				if _, ok := found[string(*relationship.TargetLocalID)]; ok {
					localID := *relationship.TargetLocalID
					entry.Status = schema.RelationshipNavigationResolved
					entry.LocalID = &localID
					break
				}
			}
			entry.Status = schema.RelationshipNavigationKnownUnavailable
		case relationship.TargetState == schema.RelationshipTargetConflictingCurrentNativeEvidence:
			entry.Status = schema.RelationshipNavigationConflicting
		case relationship.TargetState == schema.RelationshipTargetUnknown:
			entry.Status = schema.RelationshipNavigationUnknown
		case relationship.TargetState == schema.RelationshipTargetExplicitNone:
			// explicit_none names no linkable target; nothing to navigate to.
			continue
		default:
			return nil, fmt.Errorf("api.resolveRelationshipNavigation: relationship %s carries target state %q outside the closed set; no navigation was emitted; repair the stored generation before reading its links",
				relationship.Kind, relationship.TargetState)
		}
		if err := entry.Validate(); err != nil {
			return nil, fmt.Errorf("api.resolveRelationshipNavigation: emit navigation for %s relationship: %w; the link was withheld rather than routed to the wrong session",
				relationship.Kind, err)
		}
		nav = append(nav, entry)
	}
	if len(nav) == 0 {
		return nil, nil
	}
	return nav, nil
}

// DetailReadPayload returns the local read projection for one stored session:
// the validated durable detail plus authorized current-target navigation. The
// durable content is untouched; navigation is read-only metadata the host uses
// to build links. Selection never widens or narrows it.
func (p *StoreDataProvider) DetailReadPayload(ctx context.Context, id string) (*schema.SessionDetailReadPayload, error) {
	detail, err := p.DetailPayload(ctx, id)
	if err != nil {
		return nil, err
	}
	return p.decorateDetailReadPayload(ctx, detail)
}

// decorateDetailReadPayload augments a durable detail payload with resolved
// navigation. It is the ONE navigation-decorating site, so every local read
// construction (WebSocket session_detail and the HTTP detail route) shares it.
func (p *StoreDataProvider) decorateDetailReadPayload(ctx context.Context, detail *schema.SessionDetailPayload) (*schema.SessionDetailReadPayload, error) {
	nav, err := resolveRelationshipNavigation(ctx, detail.Relationships, p.ResolveStoredTargets)
	if err != nil {
		return nil, err
	}
	return &schema.SessionDetailReadPayload{
		SessionDetailPayload:   *detail,
		RelationshipNavigation: nav,
	}, nil
}

// detailReadProvider is implemented by providers that can serve the flat local
// read projection with resolved navigation. Stores with managed-generation
// support implement it; other providers keep the durable-only conversion.
type detailReadProvider interface {
	DetailReadPayload(ctx context.Context, id string) (*schema.SessionDetailReadPayload, error)
}

// storedTargetResolver is implemented by a provider that can resolve stored
// session identifiers into link targets. A provider without it has no way to
// authorize a link, so its read stays durable-only.
type storedTargetResolver interface {
	ResolveStoredTargets(ctx context.Context, ids []string) ([]StoredTarget, error)
}

// SessionDetailReadForProvider loads one flat local detail read payload. A
// provider that can resolve navigation (the generation-backed store, and the
// progressive provider that fronts it) serves the durable read projection; any
// other provider keeps the preserved SessionByID conversion path, decorated
// with resolved navigation when it can name stored targets and left
// navigation-free when it cannot.
func SessionDetailReadForProvider(ctx context.Context, provider DataProvider, id string) (*schema.SessionDetailReadPayload, error) {
	if reader, ok := provider.(detailReadProvider); ok {
		return reader.DetailReadPayload(ctx, id)
	}
	session, err := provider.SessionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	detail, err := transcript.SessionToDetailValidated(session)
	if err != nil {
		return nil, err
	}
	read := &schema.SessionDetailReadPayload{SessionDetailPayload: *detail}
	resolver, ok := provider.(storedTargetResolver)
	if !ok {
		return read, nil
	}
	nav, err := resolveRelationshipNavigation(ctx, detail.Relationships, resolver.ResolveStoredTargets)
	if err != nil {
		return nil, err
	}
	read.RelationshipNavigation = nav
	return read, nil
}
