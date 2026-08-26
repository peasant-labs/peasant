// Package projectlabel derives a project's canonical display label from its
// recorded git remote (projects.canonical_remote), so users see a
// recognizable "host:owner/repo"-style name instead of an arbitrary
// filesystem path segment.
// Filesystem path segments are used only when no recognizable remote is
// configured.
package projectlabel

import "github.com/peasant-labs/schema"

// FromRemote formats remote as a "host:owner/repo" display label by
// delegating to the shared cross-repo rule, schema.RemoteLabel, so peasant
// and village render an identical label from the same remote. It accepts the
// normalized bare form peasant stores in canonical_remote
// ("host/owner/repo", produced by internal/ingest.NormalizeRemoteURL) and,
// defensively, an HTTPS or SSH remote URL that was not pre-normalized
// ("https://host/owner/repo[.git]", "git@host:owner/repo[.git]",
// "ssh://git@host/owner/repo[.git]"). Any userinfo embedded in the remote
// (user:pass@ or user:TOKEN@ — e.g. a personal access token baked into a
// CI/CD or git-credential-store remote) is stripped before the host is
// extracted, so a credential is never rendered into a display label.
//
// The FULL host is always used: there is no short-prefix table and no host
// allowlist, so a self-hosted or enterprise forge is rendered exactly like a
// well-known one (e.g. "github.com:owner/repo", not "github:owner/repo").
// A port carried by the remote is dropped from the rendered label.
//
// ok is false when remote is empty or does not contain a recognizable
// host + path pair; callers must fall back to another display value in
// that case.
func FromRemote(remote string) (label string, ok bool) {
	return schema.RemoteLabel(remote)
}

// Label returns FromRemote(canonicalRemote)'s label when it succeeds, else
// fallback — the caller's already-computed non-remote display value
// (typically the canonical working directory path, or failing that the
// opaque project hash).
func Label(canonicalRemote, fallback string) string {
	if label, ok := FromRemote(canonicalRemote); ok {
		return label
	}
	return fallback
}
