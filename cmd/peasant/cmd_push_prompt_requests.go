package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
// The lookup runs before the upload's --timeout clock starts, so this bound is
// the whole of what the convenience can cost the commit: it is not shared with,
// and cannot draw on, the budget the upload runs under. A village that accepts
// the connection and never answers costs one second and nothing else.
const promptRequestLookupTimeout = time.Second

// reportWaitingPromptRequests asks the village which prompt requests are waiting
// for the repository being pushed and prints one line per match.
//
// It is a convenience, and every way it can fail is silent by design: a missing
// git remote, a transport error, a non-2xx status, and a malformed response all
// print nothing and leave the push to proceed exactly as it would have without
// the lookup. Nothing here may change what the push publishes, and nothing here
// may fail it.
//
// ctx is the command's context. It must NOT be the run's budget-wrapped one: the
// caller places this call ahead of that clock so that nothing here can spend the
// upload's --timeout.
//
// The repository being pushed is named by the caller, which resolved it once for
// both this lookup and the upload's scope: the remote here, the root there.
//
// client is the caller's, built once and shared with the upload, so this lookup
// and the upload that follows it open one set of pooled connections.
func reportWaitingPromptRequests(ctx context.Context, cmd *cobra.Command, client *village.VillageClient, remote string) {
	out := cmd.OutOrStdout()

	pushedFullName := village.GitHubRepositoryFullName(remote)
	if pushedFullName == "" {
		return
	}

	lookupCtx, cancel := context.WithTimeout(ctx, promptRequestLookupTimeout)
	defer cancel()

	response, _, err := client.GetPromptRequests(lookupCtx)
	if err != nil || response == nil {
		return
	}

	printWaitingPromptRequests(out, response.Requests, pushedFullName)
}

// pushedRepositoryGit returns the canonical root of the repository being pushed
// and the raw git remote at it. An empty remote (a repository with no origin) is
// reported as an error so the caller prints nothing rather than matching an
// empty name.
func pushedRepositoryGit(ctx context.Context, repository string, scoped bool) (root string, remote string, err error) {
	path := "."
	if scoped {
		if strings.TrimSpace(repository) == "" {
			// The flag's own rejection is reported later, with the reason. An
			// empty value names no repository, and resolving the working
			// directory in its place would answer a question the caller did not
			// ask.
			return "", "", fmt.Errorf("--repository names no path")
		}
		path = repository
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	resolver := &ingest.ExecGitResolver{}
	root, err = resolver.ResolveRepositoryRoot(ctx, absolute)
	if err != nil {
		return "", "", err
	}
	remote, _, err = resolver.WalkUpRemoteURL(ctx, root)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(remote) == "" {
		return "", "", fmt.Errorf("the repository being pushed has no git remote")
	}
	return root, remote, nil
}

// printWaitingPromptRequests prints one line per request that names the
// repository being pushed, as the repository its pull request was opened against
// or as the repository its head came from, and is still waiting on its author.
// It returns how many lines it printed. village.WaitingPromptRequestsFor decides
// which requests those are.
//
// The line names the base, because that is where the pull request lives, even
// when the match came through the head.
func printWaitingPromptRequests(w io.Writer, requests []schema.VillagePromptRequest, pushedFullName string) int {
	waiting := village.WaitingPromptRequestsFor(requests, pushedFullName)
	for _, request := range waiting {
		fmt.Fprintf(w, "waiting: %s#%d — run 'peasant village push' to attach the prompts behind it\n",
			request.Remote, request.Number)
	}
	return len(waiting)
}
