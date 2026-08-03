package ingest

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// NormalizeRemoteURL parses a git remote URL into canonical form: "{lowercase_host}/{path}".
//
// Handles the following URL formats:
//   - SCP (git@host:path):              git@github.com:user/repo.git  → github.com/user/repo
//   - SSH URL (ssh://):                 ssh://git@host:22/path        → host/path
//   - HTTPS:                            https://HOST/path.git         → host/path
//   - HTTP:                             http://host/path              → host/path
//   - git://:                           git://host/path               → host/path
//
// Normalizations applied:
//   - Host is lowercased
//   - .git suffix stripped from path
//   - Port numbers stripped
//   - Auth/userinfo (user:pass@) stripped
//   - Trailing slashes trimmed
//   - Full path preserved (GitLab subgroups: gitlab.com/group/subgroup/repo)
//
// Returns an error for:
//   - file:// URLs (local paths, not normalizable as remotes)
//   - Empty input
//   - Unrecognized URL formats
//
// NormalizeRemoteURL is consumed by DeriveProjectIdentifiers so that SSH and
// HTTPS clones of the same repo produce the same ProjectHash.
func NormalizeRemoteURL(remote string) (string, error) {
	if remote == "" {
		return "", fmt.Errorf(
			"NormalizeRemoteURL (internal/ingest/remote.go): empty remote URL — " +
				"what: remote URL is empty; " +
				"why: a git remote is required to derive a canonical identifier; " +
				"fix: ensure the repository has at least one git remote (e.g. 'git remote add origin <url>')",
		)
	}

	// SCP format: git@host:path(.git)? — must be checked before generic URL parsing.
	if matches := scpPattern.FindStringSubmatch(remote); matches != nil {
		host := strings.ToLower(matches[1])
		path := strings.TrimSuffix(matches[2], defaults.ExtGit.String())
		path = strings.Trim(path, "/")
		if path == "" {
			return "", fmt.Errorf(
				"NormalizeRemoteURL (internal/ingest/remote.go): SCP remote %q has no path — "+
					"what: the SCP remote URL contains no repository path; "+
					"why: a valid SCP URL requires 'git@host:user/repo' form; "+
					"fix: use a complete remote URL such as 'git@github.com:user/repo.git'",
				remote,
			)
		}
		return host + "/" + path, nil
	}

	// file:// URLs are local paths and cannot be normalized to a canonical remote form.
	if strings.HasPrefix(remote, "file://") {
		return "", fmt.Errorf(
			"NormalizeRemoteURL (internal/ingest/remote.go): remote %q uses file:// scheme — "+
				"what: file:// URLs are local filesystem paths, not network remotes; "+
				"why: canonical project identification requires a network remote URL; "+
				"fix: use an SSH or HTTPS remote (e.g. 'git remote set-url origin https://github.com/user/repo')",
			remote,
		)
	}

	// URL formats: https://, http://, ssh://, git://
	if strings.HasPrefix(remote, "https://") ||
		strings.HasPrefix(remote, "http://") ||
		strings.HasPrefix(remote, "ssh://") ||
		strings.HasPrefix(remote, "git://") {

		u, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf(
				"NormalizeRemoteURL (internal/ingest/remote.go): failed to parse remote %q — "+
					"what: URL parsing failed; "+
					"why: the URL is malformed; "+
					"fix: verify the remote URL is a valid %s, https://, ssh://, or git:// URL; "+
					"parse error: %w",
				remote, u.Scheme, err,
			)
		}

		host := strings.ToLower(u.Hostname()) // strips port, lowercases
		path := strings.TrimPrefix(u.Path, "/")
		path = strings.TrimSuffix(path, defaults.ExtGit.String())
		path = strings.Trim(path, "/")

		if host == "" {
			return "", fmt.Errorf(
				"NormalizeRemoteURL (internal/ingest/remote.go): remote %q has no host — "+
					"what: the URL contains no hostname; "+
					"why: a valid remote URL requires a host (e.g. github.com); "+
					"fix: use a complete remote URL such as 'https://github.com/user/repo.git'",
				remote,
			)
		}
		if path == "" {
			return "", fmt.Errorf(
				"NormalizeRemoteURL (internal/ingest/remote.go): remote %q has no path — "+
					"what: the URL contains no repository path; "+
					"why: a valid remote URL requires a path (e.g. /user/repo); "+
					"fix: use a complete remote URL such as 'https://github.com/user/repo.git'",
				remote,
			)
		}

		return host + "/" + path, nil
	}

	return "", fmt.Errorf(
		"NormalizeRemoteURL (internal/ingest/remote.go): unrecognized remote format %q — "+
			"what: the remote URL scheme is not supported; "+
			"why: only SCP (git@host:path), ssh://, https://, http://, and git:// formats are supported; "+
			"fix: configure a supported remote URL (e.g. 'git remote set-url origin https://github.com/user/repo')",
		remote,
	)
}

// RepoNameFromRemote extracts the repository name from a raw git remote URL.
// It normalizes the URL and returns the last path component (the repo name).
//
//	git@github.com:acme/my-project.git → my-project
//	https://gitlab.com/group/repo.git  → repo
//
// Returns empty string if the remote cannot be parsed.
func RepoNameFromRemote(remote string) string {
	normalized, err := NormalizeRemoteURL(remote)
	if err != nil || normalized == "" {
		return ""
	}
	// normalized is "host/user/repo" — last component is the repo name.
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
		return normalized[idx+1:]
	}
	return ""
}

// NormalizeRemoteForMatch normalizes a git remote URL for SELECTION-MATCHING
// comparison: unlike NormalizeRemoteURL, which is a strict
// parser used to derive a project's canonical identity (and errors on
// anything it doesn't recognize), this is a best-effort comparison helper
// that must never error — an unparseable or already-canonical remote should
// simply fail to match rather than blow up the caller.
//
// The bug this fixes: a persisted kickstart selection stores git remotes in
// whatever form the user typed them in kickstart (typically the SSH/SCP form
// a `git remote -v` shows, e.g. "git@github.com:owner/repo.git"), while the
// projects table stores the ALREADY-NORMALIZED bare form
// ("github.com/owner/repo", no scheme) produced by NormalizeRemoteURL at
// ingest time. Comparing the two raw strings never matches. Both the config
// side and the stored side must be normalized to the SAME form before
// comparison; this function is that one shared normalization, used at both
// ends of the comparison (see SelectionMatcher).
//
// Handles, in order:
//   - Any form NormalizeRemoteURL accepts (SCP, https://, http://, ssh://,
//     git://) → its canonical "host/owner/repo" output.
//   - The already-canonical bare "host/owner/repo" form the projects table
//     stores (no scheme, no "git@") → lightly cleaned up (lowercase host
//     segment, trailing ".git"/slash stripped) but otherwise passed through.
//   - Anything else (empty, unparseable) → "" so it can never spuriously
//     equal another normalized value.
func NormalizeRemoteForMatch(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if normalized, err := NormalizeRemoteURL(remote); err == nil {
		return normalized
	}
	// Not a recognized scheme/SCP form — treat as the already-bare stored
	// form and best-effort clean it up rather than rejecting it outright.
	trimmed := strings.Trim(remote, "/")
	trimmed = strings.TrimSuffix(trimmed, defaults.ExtGit.String())
	if trimmed == "" {
		return ""
	}
	if idx := strings.Index(trimmed, "/"); idx > 0 {
		return strings.ToLower(trimmed[:idx]) + trimmed[idx:]
	}
	return strings.ToLower(trimmed)
}
