package push

import (
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// IdentityBasis is how a repository's canonical project identity was derived. It
// is a closed set because the two cases behave differently and users have to be
// told which one they are in: a remote-derived identity is shared by every clone
// of that remote, while a path-derived identity belongs to one directory.
type IdentityBasis string

const (
	// IdentityFromRemote means the identity came from the normalized origin
	// remote, so separate clones of that origin share it.
	IdentityFromRemote IdentityBasis = "origin-remote"
	// IdentityFromPath means the identity is the worktree path the sessions
	// were recorded in, because there was no origin remote Peasant could
	// normalize into a shared one. That covers a repository with no origin at
	// all AND one whose origin is not a network remote — a local path or a
	// file:// URL — so the wording must not claim the origin is missing.
	IdentityFromPath IdentityBasis = "worktree-path"
)

// Describe renders the basis as the sentence a user reads.
func (b IdentityBasis) Describe() string {
	switch b {
	case IdentityFromRemote:
		return "derived from the normalized origin remote, so every clone of that remote is in scope"
	case IdentityFromPath:
		return "derived from the worktree path, because this repository has no origin remote Peasant can normalize into a shared one — the identity belongs to this directory alone, and another clone would have its own"
	default:
		return "derived by an unknown rule"
	}
}

// RepositoryScope narrows a push to the sessions Peasant recorded for one Git
// repository.
//
// Scope is a set of canonical project identities, not a path comparison, because
// that is what ingestion stamps on a session. The primary identity is the one
// the repository root itself derives. A repository with no origin remote is
// identified by the directory a session was recorded in, which for work done in
// a subdirectory is NOT the worktree root - so a scope also admits the
// identities of directories that were proven to belong to this same worktree.
// Nothing else is ever admitted: a clone of another remote, a submodule, or a
// nested repository derives an identity this set does not contain.
type RepositoryScope struct {
	// Root is the worktree top level Git resolved for the requested path.
	Root string
	// Identity is the canonical project hash of Root itself.
	Identity ingest.ProjectHash
	// Basis records how Identity was derived.
	Basis IdentityBasis
	// admitted is the closed set of project hashes this scope accepts.
	admitted map[ingest.ProjectHash]bool
	// alsoRecorded are the extra directories inside Root whose own recorded
	// identities are admitted, in sorted order, for reporting.
	alsoRecorded []string
	// unadmitted is the evidence that some work recorded inside this worktree
	// carries an identity this scope does NOT accept.
	unadmitted []RecordedUnderRoot
}

// RecordedUnderRoot is one directory inside a repository, paired with the
// project identity ingestion stamped on the sessions recorded there.
//
// It is used for both halves of the picture. A directory whose identity is the
// hash of that directory belongs to this repository and is admitted. A
// directory that still exists inside this worktree but whose sessions carry
// some OTHER identity is the evidence of a stale stamp — and the only thing
// that makes re-ingesting a safe recommendation, because the directory it would
// re-derive from is still there.
type RecordedUnderRoot struct {
	// Hash is the identity ingestion stamped on those sessions.
	Hash ingest.ProjectHash
	// Directory is the recorded working directory that produced Hash.
	Directory string
}

// NewRepositoryScope builds the scope for root. alsoRecorded and unadmitted
// must already have been proven to lie inside root; NewRepositoryScope does no
// filesystem or Git work of its own.
func NewRepositoryScope(
	root string,
	identity ingest.ProjectHash,
	basis IdentityBasis,
	alsoRecorded []RecordedUnderRoot,
	unadmitted []RecordedUnderRoot,
) *RepositoryScope {
	scope := &RepositoryScope{
		Root:     root,
		Identity: identity,
		Basis:    basis,
		admitted: map[ingest.ProjectHash]bool{identity: true},
	}
	for _, recorded := range alsoRecorded {
		if recorded.Hash == "" || recorded.Hash == identity {
			continue
		}
		if !scope.admitted[recorded.Hash] {
			scope.alsoRecorded = append(scope.alsoRecorded, recorded.Directory)
		}
		scope.admitted[recorded.Hash] = true
	}
	sort.Strings(scope.alsoRecorded)
	for _, recorded := range unadmitted {
		if recorded.Hash == "" || scope.admitted[recorded.Hash] {
			continue
		}
		scope.unadmitted = append(scope.unadmitted, recorded)
	}
	sort.Slice(scope.unadmitted, func(i, j int) bool {
		return scope.unadmitted[i].Directory < scope.unadmitted[j].Directory
	})
	return scope
}

// Unadmitted returns the recorded directories inside this repository whose
// sessions carry an identity the scope does not accept.
//
// It is the ONLY evidence that entitles a scoped push to suggest re-ingesting.
// Suggesting it without evidence is not merely unhelpful: with the repository
// moved, the recorded working directory no longer exists, the identity is
// re-derived from a dead path, and the sessions become unreachable from every
// scope with no way back.
func (s *RepositoryScope) Unadmitted() []RecordedUnderRoot {
	if s == nil {
		return nil
	}
	return s.unadmitted
}

// Admits reports whether a session carrying projectHash is in scope.
func (s *RepositoryScope) Admits(projectHash string) bool {
	if s == nil {
		return true
	}
	return s.admitted[ingest.ProjectHash(projectHash)]
}

// Hashes returns the admitted identities in a stable order, for reporting and
// for gating annotations by the same set the sessions were gated by.
func (s *RepositoryScope) Hashes() []string {
	if s == nil {
		return nil
	}
	hashes := make([]string, 0, len(s.admitted))
	for hash := range s.admitted {
		hashes = append(hashes, string(hash))
	}
	sort.Strings(hashes)
	return hashes
}

// describedDirectoryCap is how many admitted directories Describe names before
// it summarizes the rest.
//
// The enumeration was uncapped. On a monorepo with 150 recorded subdirectories
// that produced a single 4,309-character line, printed by every commit and every
// push a hook fired on, and it dumped the user's directory layout into terminals
// and CI logs. A handful of examples tells a reader which scope they are looking
// at; the count tells them the size.
const describedDirectoryCap = 3

// Describe renders the resolved scope in one line, so a push that uploads
// nothing is self-diagnosing: the user can see which identity was resolved and
// how, rather than guessing why their sessions did not match.
func (s *RepositoryScope) Describe() string {
	if s == nil {
		return ""
	}
	line := fmt.Sprintf("only sessions recorded for %s — project identity %s, %s",
		s.Root, shortIdentity(s.Identity), s.Basis.Describe())
	if len(s.alsoRecorded) > 0 {
		line += fmt.Sprintf("; also includes %d directory identit%s recorded inside it (%s)",
			len(s.alsoRecorded), plural(len(s.alsoRecorded), "y", "ies"), describedDirectories(s.alsoRecorded))
	}
	return line
}

// Summary names the scope in one short clause. It is what a --quiet run prints,
// where the line is the entire output of a commit: the repository, and enough of
// the identity to tell two scopes apart. Describe is the full sentence.
func (s *RepositoryScope) Summary() string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("%s (project identity %s)", s.Root, shortIdentity(s.Identity))
}

// describedDirectories lists the admitted directories up to the cap and counts
// the remainder.
func describedDirectories(directories []string) string {
	if len(directories) <= describedDirectoryCap {
		return strings.Join(directories, ", ")
	}
	return fmt.Sprintf("%s, and %d more",
		strings.Join(directories[:describedDirectoryCap], ", "), len(directories)-describedDirectoryCap)
}

// shortIdentity abbreviates a project hash for display. The full value is a
// salted digest that means nothing to a reader; the prefix is enough to tell two
// scopes apart.
func shortIdentity(hash ingest.ProjectHash) string {
	const shown = 12
	if len(hash) <= shown {
		return string(hash)
	}
	return string(hash[:shown]) + "…"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
