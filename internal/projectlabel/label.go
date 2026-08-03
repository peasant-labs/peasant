// Package projectlabel derives a project's canonical display label from its
// recorded git remote (projects.canonical_remote), so users see a
// recognizable "github:owner/repo"-style name instead of an arbitrary
// filesystem path segment.
// Filesystem path segments are used only when no recognizable remote is
// configured.
package projectlabel

import "strings"

// hostPrefixes maps a normalized git remote host to its short display
// prefix. A host that is not in this table keeps its full hostname as the
// prefix instead of failing closed (e.g. "git.example.com:owner/repo").
var hostPrefixes = map[string]string{
	"github.com":    "github",
	"gitlab.com":    "gitlab",
	"bitbucket.org": "bitbucket",
}

// FromRemote formats remote as a short "host:owner/repo" display label. It
// accepts the normalized bare form peasant stores in canonical_remote
// ("host/owner/repo", produced by internal/ingest.NormalizeRemoteURL) and,
// defensively, an HTTPS or SSH remote URL that was not pre-normalized
// ("https://host/owner/repo[.git]", "git@host:owner/repo[.git]"). Any
// userinfo embedded in an HTTPS remote (user:pass@ or user:TOKEN@ — e.g. a
// personal access token baked into a CI/CD or git-credential-store remote)
// is stripped before the host is extracted, so a credential is never
// rendered into a display label.
//
// ok is false when remote is empty or does not contain a recognizable
// host + path pair; callers must fall back to another display value in
// that case.
func FromRemote(remote string) (label string, ok bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", false
	}
	remote = strings.TrimSuffix(remote, ".git")
	remote = strings.TrimRight(remote, "/")

	var hostAndPath string
	switch {
	case strings.Contains(remote, "://"):
		// HTTPS/SSH/git URL scheme: keep everything after "://", then strip
		// embedded userinfo (user:pass@ or user:token@ — e.g. a PAT baked
		// into a CI/CD or git-credential-store remote URL) from the host so
		// a credential is never rendered into a display label.
		after := remote[strings.Index(remote, "://")+3:]
		if at := strings.LastIndex(after, "@"); at >= 0 {
			if slash := strings.Index(after, "/"); slash < 0 || at < slash {
				after = after[at+1:]
			}
		}
		hostAndPath = after
	case strings.Contains(remote, "@") && strings.Contains(remote, ":"):
		// SCP-like SSH form: git@host:owner/repo.
		at := strings.Index(remote, "@")
		colon := strings.Index(remote, ":")
		if colon <= at {
			return "", false
		}
		hostAndPath = remote[at+1:colon] + "/" + remote[colon+1:]
	default:
		// Bare normalized form: host/owner/repo.
		hostAndPath = remote
	}

	host, rest, found := strings.Cut(hostAndPath, "/")
	host = strings.ToLower(strings.TrimSpace(host))
	rest = strings.Trim(rest, "/")
	if !found || host == "" || rest == "" {
		return "", false
	}
	if short, known := hostPrefixes[host]; known {
		host = short
	}
	return host + ":" + rest, true
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
