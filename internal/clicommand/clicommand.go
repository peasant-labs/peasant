// Package clicommand renders the peasant command lines Peasant prints for a
// person to paste or bakes into something that runs later: shell quoting, the
// explicitly-bound path overrides, and the by-hand upload command.
//
// It is neutral vocabulary. Git hooks, publishing, the API, and the CLI all
// consume it, so it must depend on none of them; a dependency from here back
// into any of those packages would turn every future use of this package into
// a likely import cycle.
package clicommand

import "strings"

// Binding is the non-secret path context a printed or executed peasant command
// carries, so it runs against the same configuration and store as the
// operation that produced it. Empty fields are omitted from the rendered
// command, leaving Peasant's normal resolution in force; the peasant binary
// itself is always resolved from PATH.
type Binding struct {
	ConfigPath string
	ConfigDir  string
	DataDir    string
	StateDir   string
}

// Token is one element of a peasant invocation, plus whether it is a value a
// shell would have to see quoted.
//
// The shell and argument-list renderings of the same invocation differ only in
// that: a shell command line quotes its path values, and an argument list quotes
// nothing, because each element is already one argument. Building both from one
// token list is what keeps them from drifting - a shell-quoted path handed to an
// argv-form invocation is passed through literally, and "--repository '/path'"
// then fails on a directory whose name contains apostrophes.
type Token struct {
	Value string
	// Quoted marks a token a shell must see quoted: a filesystem path or any
	// other value Peasant did not choose itself. Flags and subcommands are fixed
	// ASCII and are rendered bare.
	Quoted bool
}

// Bare is a token Peasant chose itself, such as a flag or subcommand.
func Bare(value string) Token { return Token{Value: value} }

// Quoted is a token a shell must see quoted, such as a path.
func Quoted(value string) Token { return Token{Value: value, Quoted: true} }

// BoundTokens is `peasant` plus the explicitly-bound, non-secret path
// overrides. Every executable and recovery command starts here so a user never
// gets sent from a bound context to the default config or store by accident.
func BoundTokens(binding Binding) []Token {
	tokens := []Token{Bare("peasant")}
	for _, bound := range []struct{ flag, value string }{
		{"--config", binding.ConfigPath},
		{"--config-dir", binding.ConfigDir},
		{"--data-dir", binding.DataDir},
		{"--state-dir", binding.StateDir},
	} {
		if bound.value != "" {
			tokens = append(tokens, Bare(bound.flag), Quoted(bound.value))
		}
	}
	return tokens
}

// Prefix renders `peasant` with every explicitly-bound, non-secret path
// override. Callers append their subcommand and local flags, keeping recovery
// commands on the same config and store as the operation that produced them.
func Prefix(binding Binding) string {
	return RenderShell(BoundTokens(binding))
}

// ManualPush renders the upload for root as a PERSON runs it: the bound paths,
// then `village push`, without any of the flags that exist only because a hook
// runs the command. When root is empty, the command is unscoped; otherwise it
// retains that boundary.
//
// It is derived from the same bound paths rather than written out, because a
// hardcoded "peasant village push --repository <root>" pushes from a DIFFERENT
// store than the operation that printed it whenever a config or data directory
// was bound.
func ManualPush(root string, binding Binding) string {
	tokens := append(BoundTokens(binding), Bare("village"), Bare("push"))
	if root != "" {
		tokens = append(tokens, Bare("--repository"), Quoted(root))
	}
	return RenderShell(tokens)
}

// RenderShell joins tokens into a POSIX shell command line, quoting the values.
func RenderShell(tokens []Token) string {
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t.Quoted {
			parts = append(parts, ShellQuote(t.Value))
			continue
		}
		parts = append(parts, t.Value)
	}
	return strings.Join(parts, " ")
}

// RenderArgv returns tokens as raw arguments, with no quoting of any kind: each
// element is already exactly one argument.
func RenderArgv(tokens []Token) []string {
	args := make([]string, 0, len(tokens))
	for _, t := range tokens {
		args = append(args, t.Value)
	}
	return args
}

// ShellQuote wraps s in single quotes so a POSIX shell reads it literally.
//
// Every command Peasant prints for a user to paste has to be quoted the same
// way. A remedy rendered without it - "--repository /tmp/my repo" - is advice
// that cannot be followed from the state that printed it, which is worse than
// no remedy at all.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellUnquote reverses ShellQuote. A value that is not in exactly that form is
// returned unchanged: text Peasant did not write is not Peasant's to
// reinterpret, and a wrong guess would be reported to the user as fact.
func ShellUnquote(s string) string {
	if len(s) < 2 || !strings.HasPrefix(s, "'") || !strings.HasSuffix(s, "'") {
		return s
	}
	return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
}
