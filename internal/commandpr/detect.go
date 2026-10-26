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
//
// The contract keeps the source string open so a later detector can be added
// without rewriting rows a consumer already cached; this is the only value the
// current detector emits.
const SourceGHPRCreate = "gh_pr_create"

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
//
// The returned rows are the contract's schema.SessionPullRequest, so the local
// read serves exactly what the wire contract validates rather than a private
// mirror of it.
func Detect(toolCalls []schema.ToolCallDetail) []schema.SessionPullRequest {
	var detected []schema.SessionPullRequest
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
		detected = append(detected, schema.SessionPullRequest{
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

// maxWrapperDepth bounds how many nested shell wrappers a command may unwrap
// before detection gives up. Four levels cover `sudo bash -c '…'` and the
// other shapes a recorded command takes; a deeper command is not a shell
// script a person writes, and the bound keeps a pathological input finite.
const maxWrapperDepth = 4

// invokesGHPRCreate reports whether the command runs the `gh pr create`
// subcommand, directly or through one of the shell wrappers a person may put
// in front of it.
//
// The command is parsed as shell rather than searched as text, so an
// invocation inside a heredoc body or a quoted argument is not a call. A
// leading `cd … &&`, an environment assignment (`GH_TOKEN=x gh pr create`),
// and a pipeline all keep working because the walk visits every simple
// command in the syntax tree.
func invokesGHPRCreate(command string) bool {
	return commandInvokesGHPRCreate(command, maxWrapperDepth)
}

// commandInvokesGHPRCreate walks one parsed shell program for a simple command
// that runs `gh pr create`. depth is the recursion budget: each shell wrapper
// whose script is re-parsed spends one level, so a script that nests wrappers
// terminates.
func commandInvokesGHPRCreate(command string, depth int) bool {
	if depth < 0 {
		return false
	}
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
		if wordsInvoke(commandWords(call), depth) {
			found = true
			return false
		}
		return true
	})
	return found
}

// commandWord is one word of a simple command, kept in both literal forms the
// detector needs.
type commandWord struct {
	// value is syntax.Word.Lit: the word's text when the whole word is one
	// unquoted literal part. A quoted, expanded, or composite word has no
	// value, and no command, wrapper, flag, or option name matches an empty
	// value, so such a word fails closed.
	value string
	// text is the word's text when the whole word is a plain literal, including
	// one wrapped in quotes, and literal reports whether it is. Only a shell's
	// -c script reads text: the script a person records is normally quoted, and
	// Word.Lit does not unwrap quotes.
	text    string
	literal bool
}

// commandWords returns one simple command's words.
func commandWords(call *syntax.CallExpr) []commandWord {
	words := make([]commandWord, len(call.Args))
	for index, arg := range call.Args {
		words[index] = commandWord{value: arg.Lit()}
		words[index].text, words[index].literal = literalWordText(arg)
	}
	return words
}

// literalWordText returns the text a word expands to when no part of it can
// change at run time. An expansion, a substitution, or an ANSI-C quoted word is
// not literal, and false tells the caller to reject it.
func literalWordText(word *syntax.Word) (string, bool) {
	var builder strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			builder.WriteString(part.Value)
		case *syntax.SglQuoted:
			if part.Dollar {
				return "", false
			}
			builder.WriteString(part.Value)
		case *syntax.DblQuoted:
			for _, inner := range part.Parts {
				literal, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				builder.WriteString(literal.Value)
			}
		default:
			return "", false
		}
	}
	return builder.String(), true
}

// ghPRCreateWords are the words that open a direct invocation, after the
// command name.
var ghPRCreateWords = [...]string{"pr", "create"}

// ghCommandNames are the spellings of the GitHub CLI. The Windows executable
// name is accepted because a recorded command may come from either platform.
var ghCommandNames = [...]string{"gh", "gh.exe"}

// shellWrappers are the shells whose `-c` script is itself a shell program
// that detection re-parses.
var shellWrappers = [...]string{"bash", "sh", "zsh", "dash", "ksh"}

// wordsInvoke reports whether one simple command's words run `gh pr create`,
// unwrapping the closed set of shell wrappers detection understands: `sudo`,
// `command`, `env`, `nohup`, and a shell with a `-c` script.
//
// A wrapper's own options are skipped only where their arity is known, so a
// word this code does not recognize becomes the wrapped command and is
// rejected. That is what keeps `echo "gh pr create"` and
// `bash -c 'echo gh pr create'` false: the words after the wrapper are not the
// gh command, and no text search ever runs.
func wordsInvoke(words []commandWord, depth int) bool {
	if depth < 0 || len(words) == 0 {
		return false
	}
	for _, name := range ghCommandNames {
		if words[0].value != name {
			continue
		}
		if len(words) < 1+len(ghPRCreateWords) {
			return false
		}
		for index, want := range ghPRCreateWords {
			if words[1+index].value != want {
				return false
			}
		}
		return true
	}
	switch words[0].value {
	case "sudo":
		return wordsInvoke(skipWrapperOptions(words[1:], sudoFlags, sudoValueFlags), depth)
	case "command":
		return wordsInvoke(skipWrapperOptions(words[1:], commandFlags, nil), depth)
	case "env":
		rest := skipWrapperOptions(words[1:], envFlags, envValueFlags)
		for len(rest) > 0 && isEnvironmentAssignment(rest[0]) {
			rest = rest[1:]
		}
		return wordsInvoke(rest, depth)
	case "nohup":
		return wordsInvoke(skipWrapperOptions(words[1:], nohupFlags, nil), depth)
	default:
		for _, name := range shellWrappers {
			if words[0].value != name {
				continue
			}
			script, ok := shellCommandScript(words[1:])
			if !ok {
				return false
			}
			return commandInvokesGHPRCreate(script, depth-1)
		}
	}
	return false
}

// Wrapper options that take no value, and the options that consume the next
// word. Each set is deliberately closed: an option this code does not
// recognize is treated as the wrapped command, so a wrapper form that was not
// considered fails to detect rather than guessing.
var (
	sudoFlags      = stringSet("-E", "-H", "-n", "-S", "-b", "--")
	sudoValueFlags = stringSet("-u", "-g", "-h", "-p", "-C", "-T", "-r", "-t")
	commandFlags   = stringSet("-p", "-v", "-V", "--")
	envFlags       = stringSet("-i", "-0", "--")
	envValueFlags  = stringSet("-u", "-C", "-S")
	nohupFlags     = stringSet("--help", "--version", "--")
)

// stringSet builds a lookup set from a closed list of words.
func stringSet(words ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(words))
	for _, word := range words {
		set[word] = struct{}{}
	}
	return set
}

// skipWrapperOptions drops a wrapper's leading options and returns the words
// that remain: the wrapped command and its arguments. valueFlags names the
// options that consume the following word. A bare `--` ends the options. A
// word with no literal value is returned unchanged so the caller's command
// check rejects it.
func skipWrapperOptions(args []commandWord, flags, valueFlags map[string]struct{}) []commandWord {
	for len(args) > 0 {
		word := args[0].value
		switch {
		case word == "":
			return args
		case word == "--":
			return args[1:]
		case hasWord(valueFlags, word):
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		case hasWord(flags, word):
			args = args[1:]
		default:
			return args
		}
	}
	return nil
}

// hasWord reports whether a closed set contains word.
func hasWord(set map[string]struct{}, word string) bool {
	_, ok := set[word]
	return ok
}

// isEnvironmentAssignment reports whether an env argument is the `NAME=VALUE`
// form env accepts before the command it runs.
func isEnvironmentAssignment(word commandWord) bool {
	name, _, ok := strings.Cut(word.value, "=")
	if !ok || name == "" {
		return false
	}
	for index, char := range name {
		switch {
		case char == '_':
		case char >= 'A' && char <= 'Z':
		case char >= 'a' && char <= 'z':
		case index > 0 && char >= '0' && char <= '9':
		default:
			return false
		}
	}
	return true
}

// shellValueFlags are the short shell options that consume the following word,
// so `bash -o pipefail -c '…'` still finds its script.
var shellValueFlags = stringSet("-o", "-O")

// shellCommandScript returns the script a shell runs for its `-c` option. The
// option may be written alone (`-c`) or in a combined flag word that contains
// `c` (`-lc`, `-xec`). A shell followed by a script file instead of `-c` runs
// no inline command and reports no script, and so does a script that is not a
// literal word.
func shellCommandScript(args []commandWord) (string, bool) {
	for index := 0; index < len(args); index++ {
		word := args[index].value
		switch {
		case word == "":
			return "", false
		case word == "--":
			return "", false
		case strings.HasPrefix(word, "--"):
			continue
		case len(word) > 1 && word[0] == '-':
			if hasWord(shellValueFlags, word) {
				index++
				continue
			}
			if !strings.Contains(word[1:], "c") {
				continue
			}
			if index+1 >= len(args) || !args[index+1].literal {
				return "", false
			}
			return args[index+1].text, true
		default:
			// A non-flag word before -c is a script file the shell reads; the
			// words after it are that script's arguments, not a shell command.
			return "", false
		}
	}
	return "", false
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
