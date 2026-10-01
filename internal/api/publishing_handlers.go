package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// selectionScopeReader says which named sessions the saved selection admits
// into the local lists. The store-backed provider implements it.
type selectionScopeReader interface {
	SelectionScopeByID(ctx context.Context, ids []string) (map[string]bool, error)
}

var _ selectionScopeReader = (*StoreDataProvider)(nil)

// publishingHandler serves the reads the publish popup needs: the publication
// state of sessions, and the Village collectives the user can publish to.
type publishingHandler struct {
	store     *store.Store
	selection selectionScopeReader
	// configHome overrides the XDG config root the stored Village credential is
	// read from. Empty keeps the process environment.
	configHome string
}

// Error codes of the publishing reads.
const (
	publicationsIDsRequiredCode  = "publications_session_ids_required"
	publicationsIncludeCode      = "publications_include_unknown"
	villageSignedOutCode         = "village_signed_out"
	villageUnreachableCode       = "village_unreachable"
	villageTranscriptMissingCode = "village_transcript_missing"
	publishingUnavailableCode    = "publishing_unavailable"
	publishingContractBreachCode = "publishing_contract_breach"
)

// publicationsIncludeAudience is the one include value the publication read
// accepts.
const publicationsIncludeAudience = "audience"

// collectiveRepositoryReads bounds how many collectives' linked repositories
// the collectives read asks Village for at once.
const collectiveRepositoryReads = 4

// signedIn returns the stored Village credential when this computer holds a
// valid one for a known Village, and nil otherwise.
func (h *publishingHandler) signedIn() *auth.Credentials {
	creds, err := auth.LoadCredentialsFrom(h.configHome)
	if err != nil || creds == nil || !creds.IsValid() || creds.VillageURL == "" {
		return nil
	}
	return creds
}

// --------------------------------------------------------------------------
// GET /api/v1/publications?sessionIds=a,b&include=audience
// --------------------------------------------------------------------------

// handlePublications reports whether each named session is published on
// Village for the signed-in account, from the receipts and attempts this
// computer recorded. It ignores the saved selection: a session the lists leave
// out is still returned, marked outsideSelection.
func (h *publishingHandler) handlePublications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	query := r.URL.Query()
	ids := splitSessionIDs(query.Get("sessionIds"))
	if len(ids) == 0 {
		writeAPIError(w, http.StatusBadRequest,
			"The publication state could not be read because the request named no session in query field \"sessionIds\" of GET "+defaults.RoutePublications.String()+". Nothing was read. Send sessionIds as a comma-separated list of local session IDs, then retry.",
			publicationsIDsRequiredCode)
		return
	}
	include := query.Get("include")
	if include != "" && include != publicationsIncludeAudience {
		writeAPIError(w, http.StatusBadRequest,
			fmt.Sprintf("The publication state could not be read because query field \"include\" is %q on GET %s, and the only value it takes is %q. Nothing was read. Omit include, or set it to %s, then retry.", include, defaults.RoutePublications, publicationsIncludeAudience, publicationsIncludeAudience),
			publicationsIncludeCode)
		return
	}
	if h.store == nil || h.selection == nil {
		writeAPIError(w, http.StatusServiceUnavailable,
			"The publication state could not be read because this server runs without its session store. Nothing was read. Start Peasant with its normal store, then retry.",
			publishingUnavailableCode)
		return
	}

	scope, err := h.selection.SelectionScopeByID(r.Context(), ids)
	if err != nil {
		writeDiscoveryError(w, "failed to read the publication state", err)
		return
	}
	creds := h.signedIn()
	receipts := map[string]store.PublicationRecord{}
	attempts := map[string]store.PublicationAttemptDiagnostic{}
	if creds != nil {
		if receipts, err = h.store.SessionPublications(r.Context(), creds.VillageURL, creds.UserID, ids); err == nil {
			attempts, err = h.store.SessionPublicationAttempts(r.Context(), creds.VillageURL, creds.UserID, ids)
		}
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError,
				"The publication state could not be read from the local store: "+err.Error()+". Nothing was returned. Retry; if the failure repeats, inspect the Peasant store.",
				"")
			return
		}
	}

	dirs, err := h.store.SessionDirectories(r.Context(), ids)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			"The publication state could not be read from the local store: "+err.Error()+". Nothing was returned. Retry; if the failure repeats, inspect the Peasant store.",
			"")
		return
	}
	// A hook push applies the saved selection, so a session the selection
	// leaves out is never published by a hook, whatever is installed.
	// Many sessions share a repository, so each directory is resolved to its
	// repository once, and each repository's hooks are read once. A hook push
	// publishes nothing for a session a paused rule covers, and nothing at all
	// while the rules cannot be read.
	rules, rulesErr := autopublish.Load(autopublish.Path(defaults.ResolveConfigDirPathWith(h.configHome)))
	remotes := slices.ContainsFunc(rules, func(rule autopublish.Rule) bool { return rule.Kind == schema.AutoPublishRuleRemote })
	lifecycle := githooks.New(githooks.NewExecGit())
	git := &ingest.ExecGitResolver{}
	byDir := map[string]bool{}
	uploads := map[string]bool{}
	autoPublishes := func(dir string) bool {
		if rulesErr != nil || dir == "" {
			return false
		}
		if known, ok := byDir[dir]; ok {
			return known
		}
		publishes := false
		if repo, err := autopublish.Resolve(r.Context(), git, dir, remotes); err == nil {
			if _, known := uploads[repo.Root]; !known {
				uploads[repo.Root] = autopublish.Publishing(r.Context(), lifecycle, repo.Root)
			}
			decision := autopublish.Decide(rules, repo)
			publishes = uploads[repo.Root] && (!decision.Covered() || len(decision.Rules) > 0)
		}
		byDir[dir] = publishes
		return publishes
	}

	var client *village.VillageClient
	publications := make([]schema.LocalPublication, 0, len(ids))
	for _, id := range ids {
		inSelection, stored := scope[id]
		if !stored {
			continue
		}
		row := schema.LocalPublication{SessionID: id, State: schema.LocalPublicationUnpublished, OutsideSelection: !inSelection}
		row.AutoPublish = inSelection && autoPublishes(dirs[id])
		if attempt, ok := attempts[id]; ok {
			row.LastAttempt = &schema.LocalPublicationAttemptFailure{AttemptedAt: time.UnixMilli(attempt.AttemptedAt).UTC(), Message: attempt.Message}
		}
		if record, ok := receipts[id]; ok {
			transcriptID := record.Receipt.TranscriptID
			publishedAt := time.UnixMilli(record.Receipt.UpdatedAt).UTC()
			row.State = schema.LocalPublicationPublished
			row.TranscriptID = &transcriptID
			row.TranscriptURL = record.Receipt.TranscriptURL
			row.PublishedAt = &publishedAt
			if include == publicationsIncludeAudience {
				if client == nil {
					client = village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)
				}
				audience, err := publicationAudience(r.Context(), client, transcriptID)
				var refusal *village.StatusError
				if errors.As(err, &refusal) && refusal.StatusCode == http.StatusNotFound {
					writeAPIError(w, http.StatusBadGateway,
						"Village no longer holds transcript "+transcriptID.String()+", which this computer's receipt names for session "+id+", for example because it was deleted on Village. Nothing was returned. Run 'peasant village push --force' choosing only this session to publish it again, or omit include=audience, then retry.",
						villageTranscriptMissingCode)
					return
				}
				if err != nil {
					writeAPIError(w, http.StatusBadGateway,
						"The publication state could not name who can read transcript "+transcriptID.String()+" because Village could not be read: "+err.Error()+". Nothing was returned. Check the connection to Village, or omit include=audience, then retry.",
						villageUnreachableCode)
					return
				}
				row.Audience = &audience
			}
		}
		publications = append(publications, row)
	}
	response := schema.LocalPublicationsResponse{Publications: publications}
	if err := response.Validate(); err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			"The publication state breaks the Local API contract, so it is not returned: "+err.Error()+". Retry; if the failure repeats, inspect the Peasant store.",
			publishingContractBreachCode)
		return
	}
	_ = json.NewEncoder(w).Encode(response)
}

// publicationAudience lists the collectives that can read the transcript, or
// will once their owner approves it: the approved and pending shares. A
// rejected or withdrawn share gives no access and is left out.
func publicationAudience(ctx context.Context, client *village.VillageClient, transcriptID schema.TranscriptID) ([]schema.LocalPublicationAudienceMember, error) {
	shares, err := client.TranscriptShares(ctx, transcriptID)
	if err != nil {
		return nil, err
	}
	audience := make([]schema.LocalPublicationAudienceMember, 0, len(shares))
	for _, share := range shares {
		if share.Status != schema.VillageShareStatusApproved && share.Status != schema.VillageShareStatusPending {
			continue
		}
		audience = append(audience, schema.LocalPublicationAudienceMember{CollectiveID: share.CollectiveID, Name: share.Name, Status: share.Status})
	}
	return audience, nil
}

// splitSessionIDs reads a comma-separated identifier list, dropping blanks and
// repeats and keeping the order.
func splitSessionIDs(raw string) []string {
	var ids []string
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// --------------------------------------------------------------------------
// GET /api/v1/village/collectives?sessionId=X
// --------------------------------------------------------------------------

// handleVillageCollectives lists the Village collectives the signed-in user
// belongs to, read with this computer's stored credential, and suggests the
// ones that link the named session's repository or its GitHub organization.
// The server compares remotes, so a client never normalizes one.
func (h *publishingHandler) handleVillageCollectives(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	creds := h.signedIn()
	if creds == nil {
		writeAPIError(w, http.StatusUnauthorized,
			"The Village collectives could not be listed because this computer is not signed in to Village. Nothing was read. Sign in with 'peasant village login' or from the publish popup, then retry.",
			villageSignedOutCode)
		return
	}
	client := village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)
	groups, err := client.ListCollectives(r.Context())
	if err != nil {
		writeVillageReadError(w, "The Village collectives could not be listed", err)
		return
	}

	label := ""
	if sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId")); sessionID != "" && h.store != nil {
		rows, err := h.store.SessionsByIDs(r.Context(), []string{sessionID})
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError,
				"The collectives for session "+sessionID+" could not be suggested because the local store could not be read: "+err.Error()+". Nothing was returned. Retry; if the failure repeats, inspect the Peasant store.",
				"")
			return
		}
		if len(rows) == 1 && rows[0].CanonicalRemote != nil {
			label, _ = schema.RemoteLabel(*rows[0].CanonicalRemote)
		}
	}

	repositories, err := linkedRepositoriesFor(r.Context(), client, groups, label)
	if err != nil {
		writeVillageReadError(w, "The collectives could not be suggested", err)
		return
	}
	collectives := make([]schema.LocalVillageCollective, len(groups))
	for i, group := range groups {
		collectives[i] = schema.LocalVillageCollective{Group: group, Suggestion: suggestCollective(label, group, repositories[i])}
	}
	response := schema.LocalVillageCollectivesResponse{Collectives: collectives}
	if err := response.Validate(); err != nil {
		writeAPIError(w, http.StatusBadGateway,
			"Village's collectives break the Local API contract, so they are not returned: "+err.Error()+". Retry; if the failure repeats, check the Village version.",
			villageUnreachableCode)
		return
	}
	_ = json.NewEncoder(w).Encode(response)
}

// linkedRepositoriesFor reads each collective's linked repositories, only when
// a repository could match: the session has a github.com remote, because a
// collective links GitHub repositories. A suggestion is a convenience, so a
// collective whose repositories cannot be read contributes none and the list
// is still served. Only a refused credential fails the read, so the user signs
// in again.
func linkedRepositoriesFor(ctx context.Context, client *village.VillageClient, groups []schema.VillageUserGroup, sessionLabel string) ([][]schema.VillageLinkedRepository, error) {
	repositories := make([][]schema.VillageLinkedRepository, len(groups))
	if host, _, _ := strings.Cut(sessionLabel, ":"); host != githubHost {
		return repositories, nil
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(collectiveRepositoryReads)
	for i, group := range groups {
		g.Go(func() error {
			linked, err := client.ListCollectiveRepositories(gctx, group.ID)
			var refusal *village.StatusError
			if errors.As(err, &refusal) && refusal.StatusCode == http.StatusUnauthorized {
				return err
			}
			if err == nil {
				repositories[i] = linked
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return repositories, nil
}

// githubHost is the host a linked repository and a linked GitHub organization
// belong to.
const githubHost = "github.com"

// suggestCollective says why the collective is suggested for a session whose
// git remote has the schema.RemoteLabel sessionLabel, or returns nil. A linked
// repository is the more specific reason, so it wins over the organization.
// Both compare case-insensitively, as GitHub resolves names.
func suggestCollective(sessionLabel string, group schema.VillageUserGroup, repositories []schema.VillageLinkedRepository) *schema.LocalCollectiveSuggestion {
	if sessionLabel == "" {
		return nil
	}
	for _, repository := range repositories {
		label, ok := schema.RemoteLabel(githubHost + "/" + repository.Owner + "/" + repository.Name)
		if ok && strings.EqualFold(label, sessionLabel) {
			return &schema.LocalCollectiveSuggestion{Reason: schema.LocalCollectiveSuggestionLinkedRepository, Match: label}
		}
	}
	host, path, _ := strings.Cut(sessionLabel, ":")
	owner, _, _ := strings.Cut(path, "/")
	if host == githubHost && group.LinkedGithubOrg != nil && owner != "" && strings.EqualFold(*group.LinkedGithubOrg, owner) {
		return &schema.LocalCollectiveSuggestion{Reason: schema.LocalCollectiveSuggestionLinkedGithubOrg, Match: *group.LinkedGithubOrg}
	}
	return nil
}

// writeVillageReadError answers a failed Village read: 401 when Village refused
// this computer's credential, so the user signs in again, and 502 otherwise.
func writeVillageReadError(w http.ResponseWriter, what string, err error) {
	var refusal *village.StatusError
	if errors.As(err, &refusal) && refusal.StatusCode == http.StatusUnauthorized {
		writeAPIError(w, http.StatusUnauthorized,
			what+" because Village refused this computer's credential: "+refusal.Message+". Nothing was read. Sign in to Village again, then retry.",
			villageSignedOutCode)
		return
	}
	writeAPIError(w, http.StatusBadGateway,
		what+" because Village could not be read: "+err.Error()+". Nothing was read. Check the connection to Village, then retry.",
		villageUnreachableCode)
}
