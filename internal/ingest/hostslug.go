package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
)

// scpPattern matches SCP-like SSH remotes: git@{host}:{path}(.git)?
// SCP format does not support port numbers. For SSH with ports, use ssh:// URL format.
var scpPattern = regexp.MustCompile(`^git@([^:]+):(.+?)(?:\.git)?$`)

// DeriveHostSlug parses a git remote URL into a filesystem-safe host slug.
//
// Primary: Parse SSH or HTTPS remote URLs into {host}--{user}--{repo} format.
//
//	git@github.com:acme-dev/peasant.git       -> github.com--acme-dev--peasant
//	https://gitlab.com/acme-dev/project.git -> gitlab.com--acme-dev--project
//
// Fallback: If remote is empty, use fallbackPath to create
//
//	__peasant-untracked__--{hash8}--{basename}
//
// where hash8 is the first 8 hex chars of HMAC-SHA256(salt, fallbackPath) and
// basename is filepath.Base(fallbackPath) sanitized to slug-safe characters.
//
// Returns error if:
//   - remote is empty AND fallbackPath is empty
//   - remote is empty AND fallbackPath is not absolute
//   - remote format is unrecognized
//   - derived slug fails HostSlug validation
func DeriveHostSlug(s salt.Salt, remote string, fallbackPath string) (HostSlug, error) {
	if remote == "" {
		return deriveHostSlugFromPath(s, fallbackPath)
	}

	// file:// URLs are local paths — treat as untracked.
	if strings.HasPrefix(remote, "file://") {
		u, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf("DeriveHostSlug: failed to parse remote %q: %w", remote, err)
		}
		return deriveHostSlugFromPath(s, u.Path)
	}

	// Use NormalizeRemoteURL for all network remote formats (SCP, SSH, HTTPS, HTTP, git://).
	// This ensures host is lowercased and .git suffix is stripped, matching the canonical
	// form used for ProjectHash derivation — eliminating duplicate parsing logic.
	normalized, err := NormalizeRemoteURL(remote)
	if err != nil {
		return "", fmt.Errorf("DeriveHostSlug: %w", err)
	}

	// Split canonical form "host/path/segments" on "/" and join with "--".
	segments := filterEmpty(strings.Split(normalized, "/"))
	slugStr := strings.Join(segments, "--")

	return NewHostSlug(slugStr)
}

// slugUnsafeChar matches any character NOT in [a-zA-Z0-9._-].
var slugUnsafeChar = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// deriveHostSlugFromPath creates a fallback slug from an absolute filesystem path.
//
// Format: __peasant-untracked__--{hash8}--{basename}
//
// where:
//   - hash8 is the first 8 hex characters of HMAC-SHA256(salt, fallbackPath)
//   - basename is filepath.Base(fallbackPath), sanitized to slug-safe characters
//
// The fixed-width hash prefix makes parsing unambiguous: paths with "--" in
// their basename no longer collide when split on "--".
func deriveHostSlugFromPath(s salt.Salt, fallbackPath string) (HostSlug, error) {
	if fallbackPath == "" {
		return "", fmt.Errorf("DeriveHostSlug: both remote and fallbackPath are empty")
	}
	if !filepath.IsAbs(fallbackPath) {
		return "", fmt.Errorf("DeriveHostSlug: fallbackPath %q must be absolute", fallbackPath)
	}

	// hash8 = first 8 chars of HMAC-SHA256(salt, fallbackPath) hex encoding.
	// salt.Hash returns a 64-char hex string; take the first 8.
	hashResult, err := s.Hash(fallbackPath)
	if err != nil {
		return "", fmt.Errorf(
			"deriveHostSlugFromPath: failed to hash fallbackPath %q: %w — "+
				"this is unexpected; salt.Hash should not fail for non-empty input",
			fallbackPath, err,
		)
	}
	hash8 := string(hashResult)[:8]

	// basename = filepath.Base(fallbackPath), sanitized to slug-safe characters.
	basename := filepath.Base(fallbackPath)
	basename = slugUnsafeChar.ReplaceAllString(basename, "-")
	basename = strings.Trim(basename, "-")
	if basename == "" {
		basename = "unknown"
	}

	slugStr := defaults.UntrackedPrefix + "--" + hash8 + "--" + basename
	return NewHostSlug(slugStr)
}

// DeriveProjectIdentifiers returns (ProjectHash, HostSlug) for a project.
//
// If a git remote URL is provided, it is normalized via NormalizeRemoteURL and
// then hashed with HMAC-SHA256 using s to produce a stable ProjectHash that is
// identical for SSH and HTTPS clones of the same repository.
// Otherwise, the worktree/fallback path is hashed as a fallback.
//
// Returns a typed ProjectHash (validated), eliminating the need for callers
// to call NewProjectHash separately.
func DeriveProjectIdentifiers(s salt.Salt, remote, fallbackPath string) (ProjectHash, HostSlug, error) {
	var toHash string
	var hostSlug HostSlug
	var err error

	if remote != "" {
		// Normalize the remote URL so SSH and HTTPS produce the same canonical form
		// before hashing. On failure, fall back to path-based slug and hash.
		normalized, normErr := NormalizeRemoteURL(remote)
		if normErr != nil {
			// Fall back to path-based slug if remote parsing/normalization fails.
			// Hash must follow slug: if slug uses fallbackPath, hash fallbackPath too.
			// This prevents directory collisions where different bad remotes at the
			// same local path would get different hashes but the same output slug.
			slog.Warn("unrecognized git remote, falling back to local path",
				"remote", remote, "fallbackPath", fallbackPath,
				"what", "git remote URL could not be normalized to a canonical form",
				"why", "remote format not recognized (not SSH or HTTPS)",
				"user_impact", "session grouped by local path instead of repo — may appear as a separate project in the session viewer",
				"how_to_fix", "verify git remote origin is set correctly in the project directory")
			toHash = fallbackPath
			hostSlug, err = DeriveHostSlug(s, "", fallbackPath)
			if err != nil {
				return "", "", fmt.Errorf("DeriveProjectIdentifiers: %w", err)
			}
		} else {
			toHash = normalized
			hostSlug, err = DeriveHostSlug(s, remote, fallbackPath)
			if err != nil {
				// DeriveHostSlug uses the original remote for slug format (not normalized).
				// If that fails (e.g. unrecognized format not caught by NormalizeRemoteURL),
				// fall back to path.
				slog.Warn("unrecognized git remote, falling back to local path",
					"remote", remote, "fallbackPath", fallbackPath,
					"what", "git remote URL could not be normalized to a canonical form",
					"why", "remote format not recognized (not SSH or HTTPS)",
					"user_impact", "session grouped by local path instead of repo — may appear as a separate project in the session viewer",
					"how_to_fix", "verify git remote origin is set correctly in the project directory")
				toHash = fallbackPath
				hostSlug, err = DeriveHostSlug(s, "", fallbackPath)
				if err != nil {
					return "", "", fmt.Errorf("DeriveProjectIdentifiers: %w", err)
				}
			}
		}
	} else {
		toHash = fallbackPath
		hostSlug, err = DeriveHostSlug(s, "", fallbackPath)
		if err != nil {
			return "", "", fmt.Errorf("DeriveProjectIdentifiers: %w", err)
		}
	}

	projectHash, err := s.Hash(toHash)
	if err != nil {
		return "", "", fmt.Errorf("DeriveProjectIdentifiers: %w", err)
	}
	return projectHash, hostSlug, nil
}

// DeriveProjectIdentifiersWithGit is like DeriveProjectIdentifiers but, when
// remote is empty, it first walks up the parent directories of fallbackPath
// using git.WalkUpRemoteURL to find a git remote before falling back to
// path-based hashing. This ensures that sessions launched from a subdirectory
// of a git repository (e.g., ~/.claude/projects/ slug paths that decode to a
// non-root project dir) are correctly grouped with the repository's remote.
//
// If ctx is cancelled mid-walk, the function falls back to path-based hashing.
func DeriveProjectIdentifiersWithGit(ctx context.Context, s salt.Salt, git GitResolver, remote, fallbackPath string) (ProjectHash, HostSlug, error) {
	// If remote is already known, delegate directly — no walk-up needed.
	if remote != "" {
		return DeriveProjectIdentifiers(s, remote, fallbackPath)
	}

	// Attempt walk-up to find a git remote.
	if git != nil && fallbackPath != "" {
		walkedRemote, _, walkErr := git.WalkUpRemoteURL(ctx, fallbackPath)
		if walkErr == nil && walkedRemote != "" {
			// Found a remote by walking up — use it.
			return DeriveProjectIdentifiers(s, walkedRemote, fallbackPath)
		}
		// Walk returned an error (e.g. context cancelled) or no remote found.
		// Fall through to path-based fallback in DeriveProjectIdentifiers.
	}

	return DeriveProjectIdentifiers(s, "", fallbackPath)
}

// filterEmpty removes empty strings from a slice.
// Returns a new slice; does not modify the input.
func filterEmpty(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
