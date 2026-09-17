package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// promptRequestLookupTimeout bounds the waiting-request lookup.
//
// The lookup exists to tell an author that a reviewer wants the prompts behind a
// pull request. It is worth a moment of a push, and not more: a village that
// accepts a connection and never answers must not hold up a commit.
//
// The lookup runs inside the run's budget — the same --timeout a hook passes for
// the whole upload — and this bound is applied on top, so the time it can take
// from that budget is capped by whichever is smaller. It is kept well under
// githooks.DefaultUploadBudget because the lookup is synchronous and comes
// first: a bound anywhere near the budget would let one stalled read leave the
// upload too little room, and the failure would look like the push's own
// deadline. TestPromptRequestLookupBoundStaysUnderTheHookBudget holds the two
// apart.
const promptRequestLookupTimeout = time.Second

// promptRequestLookupBudgetFloor is the least budget the lookup will run under.
//
// The hint is a convenience ahead of an upload that has its own work to finish
// in the same budget, so it is only worth anything when the budget has room to
// spare. Below this floor the lookup is skipped entirely rather than risked
// against the upload's time, which makes "too small a budget" a run that behaves
// exactly as it did before this lookup existed. It is expressed in the lookup's
// own units so lowering one moves the other.
const promptRequestLookupBudgetFloor = githooks.LookupBudgetShare * promptRequestLookupTimeout

// githubRemoteHost is the only host a prompt request can belong to.
//
// A waiting request names a GitHub pull request, so a remote on any other host
// can never be the repository it was raised against. Requiring the host keeps a
// mirror clone — a GitLab remote whose path happens to end in the same two
// segments — from being told a GitHub request is waiting for it.
const githubRemoteHost = "github.com"

// reportWaitingPromptRequests asks the village which prompt requests are waiting
// for the repository being pushed and prints one line per match.
//
// It is a convenience, and every way it can fail is silent by design: a missing
// git remote, a transport error, a non-2xx status, and a malformed response all
// print nothing and leave the push to proceed exactly as it would have without
// the lookup. Nothing here may change what the push publishes, and nothing here
// may fail it.
//
// ctx is the run's budget context, so the lookup can never outlast the run; its
// own bound is applied on top of it, and it does not run at all when the budget
// left is below promptRequestLookupBudgetFloor.
//
// The repository being pushed is named by --repository when the caller supplied
// one (a hook does), and by the working directory otherwise, which is the
// manual push's shape.
func reportWaitingPromptRequests(ctx context.Context, cmd *cobra.Command, creds *auth.Credentials, repository string, scoped bool) {
	out := cmd.OutOrStdout()

	remote, err := pushedRepositoryRemote(ctx, repository, scoped)
	if err != nil {
		return
	}
	pushedFullName := githubRepositoryFullName(remote)
	if pushedFullName == "" {
		return
	}

	if deadline, bounded := ctx.Deadline(); bounded && time.Until(deadline) < promptRequestLookupBudgetFloor {
		// The upload needs this budget more than the author needs a hint this
		// run: below the floor the lookup does not happen, so it cannot be the
		// reason a short run ran out of time.
		return
	}

	lookupCtx, cancel := context.WithTimeout(ctx, promptRequestLookupTimeout)
	defer cancel()

	client := village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)
	response, _, err := client.GetPromptRequests(lookupCtx)
	if err != nil || response == nil {
		return
	}

	printWaitingPromptRequests(out, response.Requests, pushedFullName)
}

// pushedRepositoryRemote returns the raw git remote of the repository being
// pushed. An empty remote (a repository with no origin) is reported as an error
// so the caller prints nothing rather than matching an empty name.
func pushedRepositoryRemote(ctx context.Context, repository string, scoped bool) (string, error) {
	path := "."
	if scoped {
		if strings.TrimSpace(repository) == "" {
			// The flag's own rejection is reported later, with the reason. An
			// empty value names no repository, and resolving the working
			// directory in its place would answer a question the caller did not
			// ask.
			return "", fmt.Errorf("--repository names no path")
		}
		path = repository
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolver := &ingest.ExecGitResolver{}
	root, err := resolver.ResolveRepositoryRoot(ctx, absolute)
	if err != nil {
		return "", err
	}
	remote, _, err := resolver.WalkUpRemoteURL(ctx, root)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(remote) == "" {
		return "", fmt.Errorf("the repository being pushed has no git remote")
	}
	return remote, nil
}

// printWaitingPromptRequests prints one line per request that names the
// repository being pushed and is still waiting on its author, and returns how
// many lines it printed.
//
// A request for any other repository prints nothing: the caller is pushing this
// repository, and a request raised against a different one is not theirs to act
// on here. A request that is not waiting prints nothing either: the endpoint
// serves the states a request passes through, and telling an author that
// something is waiting when it has already been attached, or is a preview they
// have not confirmed, would be wrong in exactly the way the line exists to avoid.
func printWaitingPromptRequests(w io.Writer, requests []schema.VillagePromptRequest, pushedFullName string) int {
	printed := 0
	for _, request := range requests {
		if request.State != schema.VillagePullRequestAttachmentWaiting {
			continue
		}
		if !sameRepositoryFullName(request.Remote, pushedFullName) {
			continue
		}
		fmt.Fprintf(w, "waiting: %s#%d — run 'peasant village push' to attach the prompts behind it\n",
			request.Remote, request.Number)
		printed++
	}
	return printed
}

// sameRepositoryFullName reports whether two "owner/name" repository names are
// the same repository. GitHub resolves owner and repository case-insensitively,
// and the village's own lookups compare them that way, so a remote that differs
// from the request only in case is the same repository.
func sameRepositoryFullName(left, right string) bool {
	return left != "" && right != "" && strings.EqualFold(left, right)
}

// githubRepositoryFullName reduces a git remote URL to the "owner/name" form a
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
// The cost is a false negative for a GitHub Enterprise host and for a remote
// configured as a bare "owner/name" with no host; both stay silent rather than
// name a request that may not exist.
func githubRepositoryFullName(remote string) string {
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
