package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/peasant-labs/peasant/internal/commandpr"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// sessionCommitsResponse is the commits read envelope. The key is the domain
// noun the local transcript renders, not the store table name.
type sessionCommitsResponse struct {
	Commits []ingest.CommitInfo `json:"commits"`
}

// sessionPullRequestsResponse is the pull-request read envelope. The rows are
// detected from stored tool calls and are display-only.
type sessionPullRequestsResponse struct {
	PullRequests []commandpr.PullRequest `json:"pullRequests"`
}

// sessionExtrasSessionID validates the path identifier the two session-extra
// reads share. It writes the refusal itself so both routes answer an
// unparseable identifier identically.
func sessionExtrasSessionID(w http.ResponseWriter, r *http.Request) (ingest.SessionID, bool) {
	raw := r.PathValue("id")
	if raw == "" {
		http.Error(w, `{"error":"missing session ID"}`, http.StatusBadRequest)
		return "", false
	}
	sessionID, err := ingest.NewSessionID(raw)
	if err != nil {
		http.Error(w, `{"error":"invalid session ID"}`, http.StatusBadRequest)
		return "", false
	}
	return sessionID, true
}

// sessionExtrasStoredSession reports whether the requested session is stored,
// writing the refusal when it is not. A lookup failure is reported as a server
// error rather than folded into the honest not-found answer, so a database
// problem is never displayed as a missing session.
func (s *Server) sessionExtrasStoredSession(w http.ResponseWriter, r *http.Request, sessionID ingest.SessionID) bool {
	hostSlug, _, err := s.cfg.Store.LookupSessionLocation(r.Context(), sessionID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			fmt.Sprintf("session %s could not be looked up: %v; nothing was read; retry after resolving the reported cause", sessionID, err), "")
		return false
	}
	if hostSlug == "" {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return false
	}
	return true
}

// handleSessionCommits serves the commits recorded for one stored session.
//
// The rows come from the session_commits projection, so the answer is the same
// binding the review surfaces read. An unknown session is a 404; a known
// session with no recorded commits is an empty array.
func (s *Server) handleSessionCommits(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	sessionID, ok := sessionExtrasSessionID(w, r)
	if !ok {
		return
	}
	if s.cfg.Store == nil {
		http.Error(w, `{"error":"store not available"}`, http.StatusServiceUnavailable)
		return
	}
	if !s.sessionExtrasStoredSession(w, r, sessionID) {
		return
	}

	commits, err := s.cfg.Store.ListSessionCommits(r.Context(), sessionID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			fmt.Sprintf("commits could not be read for session %s: %v; no commits were returned; retry after resolving the reported cause", sessionID, err), "")
		return
	}
	if commits == nil {
		commits = []ingest.CommitInfo{}
	}
	data, err := json.Marshal(sessionCommitsResponse{Commits: commits})
	if err != nil {
		http.Error(w, `{"error":"failed to marshal session commits"}`, http.StatusInternalServerError)
		return
	}
	w.Write(data)
}

// handleSessionPullRequests serves the pull requests detected in one stored
// session's tool calls.
//
// Detection reads the session's stored detail and requires both a real
// `gh pr create` invocation and a GitHub pull request URL in the same call
// result. The answer is display-only evidence: it never widens who can read
// the transcript, and nothing is posted to GitHub. An unknown session is a
// 404; a session with no detected pull requests is an empty array.
func (s *Server) handleSessionPullRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	sessionID, ok := sessionExtrasSessionID(w, r)
	if !ok {
		return
	}
	if s.cfg.Store == nil {
		http.Error(w, `{"error":"store not available"}`, http.StatusServiceUnavailable)
		return
	}
	if s.cfg.Provider == nil {
		http.Error(w, `{"error":"data provider not available"}`, http.StatusServiceUnavailable)
		return
	}
	if !s.sessionExtrasStoredSession(w, r, sessionID) {
		return
	}

	detail, err := SessionDetailReadForProvider(r.Context(), s.cfg.Provider, string(sessionID))
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		writeAPIError(w, http.StatusInternalServerError,
			fmt.Sprintf("pull requests could not be detected for session %s: the stored session detail could not be read: %v; no pull requests were returned; retry after resolving the reported cause", sessionID, err), "")
		return
	}

	calls := make([]schema.ToolCallDetail, 0)
	for _, turn := range detail.Turns {
		calls = append(calls, turn.ToolCalls...)
	}
	pullRequests := commandpr.Detect(calls)
	if pullRequests == nil {
		pullRequests = []commandpr.PullRequest{}
	}
	data, err := json.Marshal(sessionPullRequestsResponse{PullRequests: pullRequests})
	if err != nil {
		http.Error(w, `{"error":"failed to marshal session pull requests"}`, http.StatusInternalServerError)
		return
	}
	w.Write(data)
}
