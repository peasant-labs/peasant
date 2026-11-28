package ingest

import (
	"context"
	"fmt"
	"log/slog"
)

// SessionClaimer assigns local ownership of the sessions one successful run
// wrote. It is an optional capability: a signed-out run, a dry run, and a
// file-only harvest attach no claimer, and a nil claimer claims nothing.
//
// Claiming is deliberately best-effort and never fails the run. The artifacts
// are already stored; a later explicit `peasant user claim` retries the
// ownership assignment, so a claim failure is reported and the run keeps its
// result.
type SessionClaimer interface {
	ClaimSessions(ctx context.Context, sessionIDs []string) error
}

// SessionClaimerFunc adapts a function to SessionClaimer.
type SessionClaimerFunc func(ctx context.Context, sessionIDs []string) error

// ClaimSessions calls f. A nil func claims nothing.
func (f SessionClaimerFunc) ClaimSessions(ctx context.Context, sessionIDs []string) error {
	if f == nil {
		return nil
	}
	return f(ctx, sessionIDs)
}

// WithSessionClaimer injects the optional automatic claim seam. A nil claimer
// keeps claiming off.
func WithSessionClaimer(claimer SessionClaimer) PipelineOption {
	return func(p *Pipeline) { p.claimer = claimer }
}

// claimRunSessions claims the sessions this run stored for the first time. It
// never returns an error: ownership is a local projection of stored artifacts,
// and a later `peasant user claim` retries what this attempt could not assign.
func (p *Pipeline) claimRunSessions(ctx context.Context, logPrefix string, sessionResults []SessionResult) {
	if p.claimer == nil {
		return
	}
	ids := claimableSessionIDs(sessionResults)
	if len(ids) == 0 {
		return
	}
	if err := p.claimer.ClaimSessions(ctx, ids); err != nil {
		slog.Warn(logPrefix+": claim local ownership", "error", err, "sessions", len(ids))
		p.reportDiagnostic(DiagnosticEntry{
			ErrorType:   "local_ownership_claim_failed",
			Location:    "local ownership claim",
			Message:     fmt.Sprintf("claiming %d session(s) for the signed-in account failed: %v; the harvested sessions were kept", len(ids), err),
			Remediation: "Run `peasant user claim` to assign local ownership; re-harvesting stored sessions does not claim them.",
		})
	}
}

// claimableSessionIDs names the sessions an automatic claim may cover: every
// successful result whose FirstStored flag says the session had no row and no
// saved pair before this run, deduplicated in first-seen order. Diff status
// decides nothing: a first-seen active session is claimed, while a forced
// re-process, the web one-session ingest, a reindex, and a rebuild from saved
// copies are not. An existing installation's older rows stay unclaimed until the
// sign-in prompt or `peasant user claim` explicitly claims them.
func claimableSessionIDs(sessionResults []SessionResult) []string {
	seen := make(map[SessionID]struct{}, len(sessionResults))
	ids := make([]string, 0, len(sessionResults))
	add := func(id SessionID) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, string(id))
	}
	for _, result := range sessionResults {
		if result.Error != nil || result.ExcludedBySelection {
			continue
		}
		if !result.FirstStored {
			continue
		}
		add(result.SessionID)
	}
	return ids
}
