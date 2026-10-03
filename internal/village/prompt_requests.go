package village

import (
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// githubRemoteHost is the only host a prompt request can belong to.
//
// A waiting request names a GitHub pull request, so a remote on any other host
// can never be the repository it was raised against. Requiring the host keeps a
// mirror clone — a GitLab remote whose path happens to end in the same two
// segments — from being told a GitHub request is waiting for it.
const githubRemoteHost = "github.com"

// WaitingPromptRequestsFor returns the requests that name the repository
// fullName, as the repository the pull request was opened against or as the
// repository its head came from, and are still waiting on their author. The
// order of requests is kept.
//
// A request for any other repository is left out: the caller is publishing
// this repository, and a request raised against a different one is not theirs
// to act on here. A request that is not waiting is left out too. The village's
// own query already filters to waiting, so that check is defensive — a later
// server that serves every state must not turn a result into a claim that
// something is waiting when it has been attached, or is a preview nobody
// confirmed. An empty fullName names no repository and matches nothing.
func WaitingPromptRequestsFor(requests []schema.VillagePromptRequest, fullName string) []schema.VillagePromptRequest {
	var waiting []schema.VillagePromptRequest
	for _, request := range requests {
		if request.State != schema.VillagePullRequestAttachmentWaiting {
			continue
		}
		if !namesRepository(request, fullName) {
			continue
		}
		waiting = append(waiting, request)
	}
	return waiting
}

// namesRepository reports whether the repository is one the request names: the
// repository the pull request was opened against, or the repository its head
// came from.
//
// The two differ when the pull request came from a fork whose head repository
// GitHub reports, which is the ordinary contribution workflow: the author cloned
// their fork, so the remote on their machine is the fork while the request names
// the base it was raised against. They are equal for a same-repository pull
// request and for a fork whose head repository is unknown, and matching only the
// base leaves a fork's author unread. A request from a village older than the
// head remote carries an empty one, and the base decides alone, exactly as
// before.
func namesRepository(request schema.VillagePromptRequest, fullName string) bool {
	return sameRepositoryFullName(request.Remote, fullName) ||
		sameRepositoryFullName(request.HeadRemote, fullName)
}

// sameRepositoryFullName reports whether two "owner/name" repository names are
// the same repository. GitHub resolves owner and repository case-insensitively,
// and the village's own lookups compare them that way, so a remote that differs
// from the request only in case is the same repository.
func sameRepositoryFullName(left, right string) bool {
	return left != "" && right != "" && strings.EqualFold(left, right)
}

// GitHubRepositoryFullName reduces a git remote URL to the "owner/name" form a
// prompt request names, or returns "" when the remote cannot be one.
//
// The village serves a request's remote as GitHub's repository full_name —
// "owner/name", with no host and no scheme — because that is what the App reads
// out of the webhook. The remote on this machine is whatever the user cloned
// from, in any of the forms git accepts, so it is normalized first (the same
// normalization every other remote comparison in Peasant uses) and then reduced
// to the same shape.
//
// The host must be github.com and a GitHub repository is exactly
// "github.com/owner/name": three segments. Two repositories whose paths end in
// the same two segments are different repositories, so anything longer is not
// reduced — a GitLab subgroup is not a GitHub owner. A remote that names no host
// at all is refused for the same reason: there is nothing to prove it is GitHub.
// The cost is a false negative for a GitHub Enterprise host, for a remote
// configured as a bare "owner/name" with no host, and for an SSH-config alias
// such as "git@github-work:owner/repo" — the commonest local setup of the three,
// and the one most likely to surprise. All stay silent rather than name a
// request that may not exist.
func GitHubRepositoryFullName(remote string) string {
	// A trailing slash survives the shared normalizer's .git stripping, and
	// "owner/repo.git/" would otherwise reduce to a repository named "repo.git".
	remote = strings.TrimRight(strings.TrimSpace(remote), "/")
	normalized := ingest.NormalizeRemoteForMatch(remote)
	if normalized == "" {
		return ""
	}
	segments := strings.Split(normalized, "/")
	if len(segments) != 3 || !strings.EqualFold(segments[0], githubRemoteHost) {
		return ""
	}
	return segments[1] + "/" + segments[2]
}
