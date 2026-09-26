package codemap

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/peasant-labs/schema"
)

// directLookupScore is the relevance score of an id/hash direct-lookup hit.
// Rank is authoritative and a direct lookup returns at most one hit, so the
// value only needs to be a positive, deterministic relevance marker.
const directLookupScore = 1.0

// directLookupKind names the well-formed identifier shape a trimmed search
// query carries, if any.
type directLookupKind int

const (
	// directLookupNone means the query is not a complete, well-formed id or
	// hash: the caller falls through to the FTS5 content search unchanged.
	directLookupNone directLookupKind = iota
	// directLookupSession means the query is a session id (UUID or
	// ses_-prefixed) that resolves through the by-id read.
	directLookupSession
	// directLookupProject means the query is a 64-character hex project hash
	// that resolves through the project read.
	directLookupProject
)

// sessionIDUUIDPattern matches the canonical lowercase-UUID session shape,
// mirroring the schema module's session-id grammar. It is re-stated here
// (rather than re-derived from schema.NewSessionID) so the detector admits
// exactly the issue's stated session shapes — UUID and ses_-prefixed — and
// leaves every other well-formed session shape (agent-, msg_-, Strike) on
// the existing FTS path.
var sessionIDUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// classifyDirectLookup trims the query and reports whether it is a complete,
// well-formed session id or project hash. A project-hash match returns its
// validated hash; a session match returns the trimmed id. Near-misses — a
// 63-character hex string, a bare "ses_" prefix, an uppercase UUID — fail
// their constructors and report directLookupNone, as does any other content
// query.
func classifyDirectLookup(query string) (directLookupKind, string, schema.ProjectHash) {
	trimmed := strings.TrimSpace(query)
	if hash, err := schema.NewProjectHash(trimmed); err == nil {
		return directLookupProject, trimmed, hash
	}
	if _, err := schema.NewSessionID(trimmed); err != nil {
		return directLookupNone, "", ""
	}
	if sessionIDUUIDPattern.MatchString(trimmed) || strings.HasPrefix(trimmed, "ses_") {
		return directLookupSession, trimmed, ""
	}
	return directLookupNone, "", ""
}

// directLookup resolves one well-formed session id or project hash without
// touching the FTS index, reusing the store's by-id and project reads. A
// ranked-window page past the first reports exhaustion (empty, not an error)
// because a direct lookup yields at most one hit — this keeps ranked-window
// walkers, such as the grouped search view, terminating.
func (s *Service) directLookup(ctx context.Context, payload *schema.SearchPayload, kind directLookupKind, id string, hash schema.ProjectHash, offset int) (*schema.SearchPayload, error) {
	if offset > 0 {
		return payload, nil
	}
	if kind == directLookupProject {
		return s.directProjectLookup(ctx, payload, hash)
	}
	return s.directSessionLookup(ctx, payload, id)
}

// directSessionLookup returns the session named by a well-formed id as the
// sole result. It reuses the store's by-id read with no discovery scope: a
// held identifier resolves exactly like a deep link. An id that names no
// stored session returns an empty result set, not an error.
func (s *Service) directSessionLookup(ctx context.Context, payload *schema.SearchPayload, id string) (*schema.SearchPayload, error) {
	row, err := s.store.SessionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return payload, nil
	}
	hash, err := schema.NewProjectHash(row.ProjectHash)
	if err != nil {
		return nil, fmt.Errorf("codemap: direct session %q has invalid stored project hash %q: %w; run `peasant ingest verify` and repair the store before retrying", id, row.ProjectHash, err)
	}
	result, err := s.directSessionResult(ctx, row.SessionID, row.ProjectName, hash)
	if err != nil {
		return nil, err
	}
	payload.Results = append(payload.Results, result)
	return payload, nil
}

// directProjectLookup returns the newest session of the named project as the
// sole result identifying that project. An unknown hash, or a project with no
// sessions, returns an empty result set, not an error.
func (s *Service) directProjectLookup(ctx context.Context, payload *schema.SearchPayload, hash schema.ProjectHash) (*schema.SearchPayload, error) {
	cwd, _, found, err := s.queryProjectCwd(ctx, hash)
	if err != nil {
		return nil, err
	}
	if !found {
		return payload, nil
	}
	sessions, err := s.querySessions(ctx, hash)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return payload, nil
	}
	// querySessions orders newest first, so the first row is the session a
	// project hit identifies.
	newest := sessions[0]
	project := cwd
	if project == "" {
		project = hash.String()
	}
	result, err := s.directSessionResult(ctx, newest.id, project, hash)
	if err != nil {
		return nil, err
	}
	payload.Results = append(payload.Results, result)
	return payload, nil
}

// directSessionResult shapes one direct-lookup hit anchored at the session's
// first indexed entry, so it carries the same deep-link coordinates as a
// content match (sessionId + entryIndex + projectHash) with an empty snippet.
// The empty snippet is the marker the command palette (#351) renders as a
// distinct id/hash row showing the session or project identity instead of a
// content excerpt. A session with no indexed entries still resolves at entry
// 0, because the lookup names the session, not its transcript.
func (s *Service) directSessionResult(ctx context.Context, sessionID, project string, hash schema.ProjectHash) (schema.SearchResult, error) {
	result := schema.SearchResult{
		SessionID:   sessionID,
		Project:     project,
		ProjectHash: hash,
		EntryIndex:  0,
		Role:        string(schema.RoleUser),
		Score:       directLookupScore,
	}
	entries, err := s.listEntries(ctx, sessionID)
	if err != nil {
		return schema.SearchResult{}, err
	}
	if len(entries) == 0 {
		return result, nil
	}
	// ListEntries orders by entry_index, so the first entry is the session's
	// opening turn.
	first := entries[0]
	result.EntryIndex = first.EntryIndex
	result.Role = string(first.Role)
	return result, nil
}
