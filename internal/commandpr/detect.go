// Package commandpr detects pull requests created by a recorded shell
// command. It reads stored tool calls; it never contacts GitHub and never
// decides who may read a transcript.
//
// Detection is display-only. A detected pull request is named in the local
// transcript beside the session that produced it; it widens no access and
// posts nothing.
package commandpr

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/peasant-labs/schema"
	"mvdan.cc/sh/v3/syntax"
)

// SourceGHPRCreate names the one command that produces a detected pull
// request. Consumers render it as evidence for where the row came from.
const SourceGHPRCreate = "gh_pr_create"

// PullRequest is one pull request detected from a stored tool call.
type PullRequest struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Source string `json:"source"`
}

// pullRequestURL matches a GitHub pull request URL. Owner and repository
// names may contain letters, digits, dots, underscores, and dashes; the
// number is the path segment after /pull/. Anything after the number (a
// fragment, a sub-page such as /files) is not part of the identity.
var pullRequestURL = regexp.MustCompile(`https://github\.com/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/pull/([0-9]+)`)

// Detect returns the pull requests a session's stored tool calls created.
//
// A call counts only when two independent pieces of evidence are present
// together: the recorded shell command actually invokes `gh pr create`, and
// the recorded result carries a GitHub pull request URL. The command is parsed
// as shell, so a heredoc body or a quoted string that merely mentions the
// command is not an invocation. An errored call is skipped.
//
// Rows are deduplicated by owner/repository/number and keep the order in which
// they were first seen, so the same pull request mentioned twice renders once.
func Detect(toolCalls []schema.ToolCallDetail) []PullRequest {
	var detected []PullRequest
	seen := make(map[string]struct{}, len(toolCalls))
	for _, call := range toolCalls {
		if call.IsError {
			continue
		}
		command := commandFromArguments(call.Arguments)
		if command == "" || !invokesGHPRCreate(command) {
			continue
		}
		reference, ok := lastPullRequestReference(call.Result)
		if !ok {
			continue
		}
		key := reference.Owner + "/" + reference.Repo + "/" + strconv.Itoa(reference.Number)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		detected = append(detected, PullRequest{
			Owner:  reference.Owner,
			Repo:   reference.Repo,
			Number: reference.Number,
			URL:    reference.URL,
			Source: SourceGHPRCreate,
		})
	}
	return detected
}

// commandFromArguments extracts the shell command from a stored tool call's
// arguments. Harnesses record arguments either as a JSON object with a
// "command" key (some use "cmd") or, for a harness that stores the invocation
// directly, as the command text itself. Empty input carries no command.
func commandFromArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}
	if trimmed[0] == '{' {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &fields); err == nil {
			for _, key := range []string{"command", "cmd"} {
				raw, ok := fields[key]
				if !ok {
					continue
				}
				var value string
				if err := json.Unmarshal(raw, &value); err == nil && strings.TrimSpace(value) != "" {
					return value
				}
			}
			// A JSON object without a command field describes a tool that did
			// not run a shell command.
			return ""
		}
	}
	return arguments
}

// invokesGHPRCreate reports whether the command contains a real call whose
// first three words are exactly `gh`, `pr`, and `create`.
//
// The command is parsed as shell rather than searched as text, so an
// invocation inside a heredoc body or a quoted argument is not a call. A
// leading `cd … &&`, an environment assignment (`GH_TOKEN=x gh pr create`),
// and a pipeline all keep working because the walk visits every simple
// command in the syntax tree.
func invokesGHPRCreate(command string) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		if callInvokesGHPRCreate(call) {
			found = true
			return false
		}
		return true
	})
	return found
}

// ghPRCreateWords are the exact literal words that open an invocation.
var ghPRCreateWords = [...]string{"gh", "pr", "create"}

// callInvokesGHPRCreate reports whether one simple command opens with the
// gh pull-request create subcommand. Only literal words count: an expansion,
// a quote, or a substitution in the first three words is a different word, and
// Word.Lit reports it as empty, so the command is not accepted.
func callInvokesGHPRCreate(call *syntax.CallExpr) bool {
	if len(call.Args) < len(ghPRCreateWords) {
		return false
	}
	for index, want := range ghPRCreateWords {
		if call.Args[index].Lit() != want {
			return false
		}
	}
	return true
}

// pullRequestReference is the identity parsed out of one GitHub URL.
type pullRequestReference struct {
	Owner  string
	Repo   string
	Number int
	URL    string
}

// lastPullRequestReference returns the last GitHub pull request URL in the
// result text. A result often carries several URLs (the PR page and its
// related links); the last one is the created pull request's own URL in the
// shapes `gh pr create` prints.
func lastPullRequestReference(result string) (pullRequestReference, bool) {
	matches := pullRequestURL.FindAllStringSubmatch(result, -1)
	for index := len(matches) - 1; index >= 0; index-- {
		number, err := strconv.Atoi(matches[index][3])
		if err != nil || number <= 0 {
			continue
		}
		return pullRequestReference{
			Owner:  matches[index][1],
			Repo:   matches[index][2],
			Number: number,
			URL:    matches[index][0],
		}, true
	}
	return pullRequestReference{}, false
}
