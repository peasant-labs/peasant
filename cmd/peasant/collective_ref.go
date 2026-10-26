package main

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// collectiveRefURLSegments are the collective page path segments a pasted URL may
// use: the live route today, and the canonical one once the API family is renamed
// (peasant-labs/schema#155). Accepting both costs nothing here, because the CLI
// parses the URL itself, and it keeps a copied page URL working across the rename.
var collectiveRefURLSegments = []string{"groups", "collectives"}

// resolveCollectiveRefs turns the repeatable --collective values into the
// collectives they name, in the order given, with duplicates collapsed. One
// value may be a collective UUID, a collective name (case-insensitive), or a
// pasted collective page URL (…/groups/<uuid> or …/collectives/<uuid>).
//
// Resolution runs ONCE against the caller's own collective list, before any
// pull-list request. An unknown or ambiguous name therefore fails early, naming
// the candidates the caller can see, and a collective the caller does not belong
// to is reported as such instead of arriving as a puzzling empty result. The
// returned names are parallel to the identifiers.
func resolveCollectiveRefs(ctx context.Context, client *village.VillageClient, values []string) ([]schema.VillageUUID, []string, error) {
	collectives, err := client.ListCollectives(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list the collectives you belong to: %w", err)
	}

	byID := make(map[schema.VillageUUID]string, len(collectives))
	byName := make(map[string][]schema.VillageUserGroup, len(collectives))
	visible := make([]string, 0, len(collectives))
	for _, collective := range collectives {
		byID[collective.ID] = collective.Name
		key := strings.ToLower(strings.TrimSpace(collective.Name))
		byName[key] = append(byName[key], collective)
		visible = append(visible, collective.Name)
	}

	ids := make([]schema.VillageUUID, 0, len(values))
	names := make([]string, 0, len(values))
	seen := make(map[schema.VillageUUID]struct{}, len(values))
	add := func(id schema.VillageUUID, name string) {
		if _, duplicate := seen[id]; duplicate {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		names = append(names, name)
	}

	for _, value := range values {
		candidate, err := collectiveRefCandidate(value)
		if err != nil {
			return nil, nil, err
		}
		if id, ok := collectiveRefUUID(candidate); ok {
			name, member := byID[id]
			if !member {
				return nil, nil, fmt.Errorf(
					"collective %s is not one you belong to\n"+
						"  what: the collective was resolved against your own collective list and was not there\n"+
						"  why:  `--collective` pulls the transcripts a collective holds, which requires membership\n"+
						"  fix:  run `peasant village transcripts list --collective <name>` with one of: %s",
					id, collectiveRefCandidates(visible))
			}
			add(id, name)
			continue
		}

		matches := byName[strings.ToLower(candidate)]
		switch len(matches) {
		case 0:
			return nil, nil, fmt.Errorf(
				"no collective you belong to is named %q\n"+
					"  what: the name did not match any collective in your own collective list\n"+
					"  why:  `--collective` accepts a collective UUID, its name, or its web URL\n"+
					"  fix:  use one of: %s",
				candidate, collectiveRefCandidates(visible))
		case 1:
			add(matches[0].ID, matches[0].Name)
		default:
			named := make([]string, 0, len(matches))
			for _, match := range matches {
				named = append(named, fmt.Sprintf("%s (%s)", match.Name, match.ID))
			}
			return nil, nil, fmt.Errorf(
				"the collective name %q is ambiguous\n"+
					"  what: %d collectives you belong to share that name\n"+
					"  why:  a name alone cannot choose between them\n"+
					"  fix:  pass the collective UUID instead, one of: %s",
				candidate, len(matches), strings.Join(named, ", "))
		}
	}

	return ids, names, nil
}

// filterByOwner keeps only the listed rows contributed by one village account,
// compared case-insensitively because a username is case-insensitive at the
// village. An empty owner keeps every row, so callers can pass the flag value
// straight through. A row whose owner the village shows anonymously never
// matches: this CLI has no username for it and never infers one.
func filterByOwner(rows []schema.PullTranscriptInfo, owner string) []schema.PullTranscriptInfo {
	if owner == "" {
		return rows
	}
	wanted := strings.ToLower(strings.TrimSpace(owner))
	kept := make([]schema.PullTranscriptInfo, 0, len(rows))
	for i := range rows {
		if rows[i].Owner.IsProfile() && strings.ToLower(rows[i].Owner.Username) == wanted {
			kept = append(kept, rows[i])
		}
	}
	return kept
}

// collectiveRefCandidate returns the reference with surrounding space removed,
// accepting either a bare identifier or name, or a pasted collective web URL.
// A URL that is not a collective page is refused: the path must be exactly
// /groups/<uuid> or /collectives/<uuid> with a collective identifier as the
// last segment, so lookalikes such as /users/<uuid> or /groups/<slug> never
// resolve as a name. A bare identifier or name is returned as is for the
// caller to resolve against the membership list; only a pasted URL is refused
// here, mirroring pull.ParseTranscriptRef's separation of a bare identifier
// from a URL whose last segment is wrong.
func collectiveRefCandidate(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf(
			"invalid --collective value: empty\n" +
				"  what: no collective reference was given\n" +
				"  why:  `--collective` needs a collective to pull from\n" +
				"  fix:  pass a collective name, its UUID, or its web URL")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return trimmed, nil
	}
	segments := make([]string, 0, 4)
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	if len(segments) != 2 || !slices.Contains(collectiveRefURLSegments, segments[0]) {
		return "", fmt.Errorf(
			"invalid --collective value %q: the URL is not a collective page\n"+
				"  what: the URL path is neither the /groups/<uuid> nor the /collectives/<uuid> shape (got %q)\n"+
				"  why:  only a collective's own page URL names a collective\n"+
				"  fix:  copy the collective's page URL (…/groups/<uuid> or …/collectives/<uuid>), or pass its name or UUID",
			trimmed, parsed.Path)
	}
	if _, ok := collectiveRefUUID(segments[1]); !ok {
		return "", fmt.Errorf(
			"invalid --collective value %q: the URL is not a collective page: last segment %q is not a collective identifier\n"+
				"  what: the URL path has the /groups/<uuid> or /collectives/<uuid> shape but the last segment is not a collective UUID (got %q)\n"+
				"  why:  a collective page URL must end in the collective's UUID, so a slug cannot name a collective\n"+
				"  fix:  copy the collective's page URL (…/groups/<uuid> or …/collectives/<uuid>), or pass its name or UUID",
			trimmed, segments[1], segments[1])
	}
	return segments[1], nil
}

// collectiveRefUUID reports whether the candidate is a collective identifier in
// the form the village emits, returning it typed.
func collectiveRefUUID(candidate string) (schema.VillageUUID, bool) {
	id := schema.VillageUUID(strings.ToLower(candidate))
	if !village.IsCollectiveID(id) {
		return "", false
	}
	return id, true
}

// collectiveRefCandidates renders the visible collective names for an error, or
// a plain statement when the caller belongs to none.
func collectiveRefCandidates(visible []string) string {
	if len(visible) == 0 {
		return "you do not belong to any collective yet"
	}
	return strings.Join(visible, ", ")
}
