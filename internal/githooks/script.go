package githooks

import (
	"bytes"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

const (
	// scriptShebang is the first line of every generated hook. Generated hooks
	// are POSIX shell so they run wherever Git runs.
	//
	// It names /bin/sh directly, and that is a requirement rather than a
	// convention: the interpreter has to exist in environments that provide NO
	// /usr/bin AT ALL. A hermetic build sandbox is the proven case - /bin/sh is
	// there, /usr/bin is not even a directory - so '#!/usr/bin/env sh' makes
	// every generated hook unexecutable there, with fork/exec reporting only
	// ENOENT. The reverse has never been observed: every environment that runs
	// Git hooks has /bin/sh, which is also what Git's own hook samples use.
	// Do not restore the env form on POSIX-purity grounds; POSIX specifies no
	// location for either path, so purity cannot decide this and availability
	// can.
	//
	// The script it introduces needs the shell and Git, and no other external
	// utility: everything else it runs is a shell builtin. That is what makes
	// the interpreter the only path that has to resolve.
	//
	// This line is also load-bearing for ownership: IsManaged matches it
	// exactly, so changing it makes every previously generated hook foreign.
	// That is safe only while nothing has shipped.
	scriptShebang = "#!/bin/sh"

	// ScriptMarkerBegin and ScriptMarkerEnd frame a Peasant-generated hook file.
	// Ownership is decided by these two lines and nothing else: a file counts as
	// Peasant's only when it starts with the shebang followed by ScriptMarkerBegin
	// and ends with ScriptMarkerEnd. Anything else is foreign and untouchable.
	//
	// The pair is deliberately symmetric, and says GENERATED FROM rather than
	// naming a manager: these two lines are the envelope of a file that is
	// entirely machine-owned, and a reader who finds one of them should be able
	// to tell that without reading the other.
	ScriptMarkerBegin = "# BEGIN GENERATED FROM peasant village hooks"
	ScriptMarkerEnd   = "# END GENERATED FROM peasant village hooks"

	// ScriptMode is the permission mode a generated hook is written with. Git
	// only runs a hook that is executable.
	ScriptMode fs.FileMode = 0o755

	// DefaultUploadBudget bounds the WHOLE upload a generated hook runs.
	//
	// The village client's own timeout is per request, and one upload issues
	// several requests in sequence, so a village that accepts a connection and
	// then never answers — the realistic VPN or captive-portal failure, unlike a
	// refused connection, which fails at once — stalls git for minutes, once per
	// commit. A three-commit rebase fires the hook three times.
	//
	// The budget is deliberately short, because the hook is best effort: giving
	// up prints the non-blocking warning, git carries on, and the next commit
	// tries again. An upload that genuinely needs longer — a large first push —
	// is a one-time manual run, which the generated hook's header says.
	DefaultUploadBudget = 5 * time.Second
)

// ManualMarkerPrefix opens the by-hand snippet. It is deliberately different
// from ScriptMarkerBegin: a user who pastes the snippet into their own hook must
// never end up with a file Peasant mistakes for its own and later rewrites or
// deletes. It stays recognizable so Peasant can still tell the user that an
// upload section is running from a file it will not touch.
//
// It shares the BEGIN/END symmetry of the generated fence but NOT its
// "GENERATED FROM" wording, because the two say opposite things about
// provenance: this section is the one Peasant did not write and will never
// manage. Neither marker is a substring of the other, so ContainsManualSection
// and ContainsGeneratedSection stay independent tests over the same bytes.
const ManualMarkerPrefix = "# BEGIN peasant village upload"

const (
	// headerCommandPrefix opens the generated hook's header line naming the
	// exact upload command that hook runs. Both scriptTemplate and
	// EmbeddedCommand use this one string, so what Peasant writes is what it
	// reads back; TestScript_EmbeddedFactsRoundTrip fails if the template and
	// the reader ever drift apart.
	headerCommandPrefix = "#   command    : "

	// embeddedRepositoryAssignment opens the shell assignment that carries the
	// repository root a generated hook was written for. Reading it back is how
	// status notices that the repository has been moved or renamed since the
	// hook was installed.
	embeddedRepositoryAssignment = "peasant_hook_repository="
)

// posixShellCommands are the interpreter names whose shells run the by-hand
// section as written. The section is plain POSIX: a parameter expansion, a
// subshell, command -v, printf, and a status restore.
//
// The set is a whitelist rather than a blacklist because the cost of the two
// mistakes is not symmetric. Wrongly withholding the section leaves a user to
// paste it themselves; wrongly offering it to a python, ruby, perl, or node
// hook makes that hook a syntax error, which blocks every push.
var posixShellCommands = map[string]bool{
	"sh": true, "bash": true, "dash": true, "ash": true, "busybox": true,
	"ksh": true, "ksh93": true, "mksh": true, "pdksh": true, "zsh": true,
}

// binarySniffLimit is how much of a hook file is examined for the NUL byte that
// proves it is not a text script. A shebang and the first lines of any script
// are well inside it.
const binarySniffLimit = 512

// ClassifyHookLanguage reports what Git will run content AS, and names the
// interpreter when the file names one.
//
// A file with no shebang is POSIX shell: Git's own exec falls back to running
// such a file with the shell, which is why a shebang-less hook works at all.
// A file with a NUL byte near its start is a compiled program, and nothing can
// be appended to it.
func ClassifyHookLanguage(content []byte) (HookLanguage, string) {
	head := content
	if len(head) > binarySniffLimit {
		head = head[:binarySniffLimit]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return HookLanguageBinary, ""
	}
	shebang, _, _ := strings.Cut(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n")
	if !strings.HasPrefix(shebang, "#!") {
		return HookLanguagePOSIXShell, ""
	}
	fields := strings.Fields(strings.TrimPrefix(shebang, "#!"))
	if len(fields) == 0 {
		return HookLanguagePOSIXShell, ""
	}
	name := filepath.Base(fields[0])
	// `#!/usr/bin/env python3` names env, not the interpreter. The real one is
	// the first argument that is not an option.
	if name == "env" {
		for _, argument := range fields[1:] {
			if strings.HasPrefix(argument, "-") || strings.Contains(argument, "=") {
				continue
			}
			name = filepath.Base(argument)
			break
		}
		if name == "env" {
			return HookLanguageOther, "env"
		}
	}
	if posixShellCommands[name] {
		return HookLanguagePOSIXShell, name
	}
	return HookLanguageOther, name
}

// ContainsManualSection reports whether content carries the by-hand upload
// section. It never grants Peasant permission to change the file.
func ContainsManualSection(content []byte) bool {
	return strings.Contains(string(content), ManualMarkerPrefix)
}

// ContainsGeneratedSection reports whether content carries the opening marker of
// a Peasant-generated hook.
//
// It answers a different question from IsManaged. IsManaged asks "may Peasant
// rewrite or delete this file", and demands intact framing. This asks "does a
// Peasant upload still run from this file", which stays true the moment someone
// appends a line to a generated hook: the framing breaks, ownership becomes
// foreign, and the upload command is still sitting there. Reporting "nothing of
// Peasant's is here" in that state is false, and the upload keeps running on
// every commit.
func ContainsGeneratedSection(content []byte) bool {
	return strings.Contains(string(content), ScriptMarkerBegin)
}

// IsManaged reports whether content is an intact Peasant-generated hook.
//
// The check is deliberately strict and framing-based: the first line must be the
// shebang, the second must be ScriptMarkerBegin, and the last non-blank line must
// be ScriptMarkerEnd. A file that merely mentions a marker somewhere in the
// middle - for example a user hook that pasted the manual snippet - is NOT
// Peasant's, and Peasant will never rewrite or delete it.
func IsManaged(content []byte) bool {
	lines := scriptLines(content)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 3 {
		return false
	}
	return lines[0] == scriptShebang &&
		lines[1] == ScriptMarkerBegin &&
		lines[len(lines)-1] == ScriptMarkerEnd
}

// token is one element of an upload invocation, plus whether it is a value that
// a shell would have to see quoted.
//
// The two renderings of the same invocation differ only in that: a shell command
// line quotes its path values, and an argument list quotes nothing, because each
// element is already one argument. Building both from one token list is what
// keeps them from drifting - a shell-quoted path handed to an argv-form
// invocation is passed through literally, and "--repository '/path'" then fails
// on a directory whose name contains apostrophes.
type token struct {
	value string
	// quoted marks a token a shell must see quoted: a filesystem path or any
	// other value Peasant did not choose itself. Flags and subcommands are
	// fixed ASCII and are rendered bare.
	quoted bool
}

// boundTokens is `peasant` plus the explicitly-bound, non-secret path
// overrides. Every executable and recovery command starts here so a user never
// gets sent from a bound hook to the default config or store by accident.
func boundTokens(binding Binding) []token {
	tokens := []token{{value: "peasant"}}
	for _, bound := range []struct{ flag, value string }{
		{"--config", binding.ConfigPath},
		{"--config-dir", binding.ConfigDir},
		{"--data-dir", binding.DataDir},
		{"--state-dir", binding.StateDir},
	} {
		if bound.value != "" {
			tokens = append(tokens, token{value: bound.flag}, token{value: bound.value, quoted: true})
		}
	}
	return tokens
}

// CommandPrefix renders `peasant` with every explicitly-bound, non-secret path
// override. Callers append their subcommand and local flags, keeping recovery
// commands on the same config and store as the operation that produced them.
func CommandPrefix(binding Binding) string {
	return renderShell(boundTokens(binding))
}

// uploadTokens is the upload invocation: the program, the explicitly-bound
// non-secret path overrides, the push subcommand, and - when root is non-empty -
// the repository the push is confined to.
//
// The peasant binary is resolved from PATH; only explicitly-bound paths are
// pinned, so a hook installed without overrides keeps Peasant's normal
// resolution.
//
// --quiet is always included: a hook fires on every commit or push, and the
// default summary would print several lines into an otherwise ordinary git
// command. --quiet still prints errors and one final result line, so a failure
// is never hidden.
//
// --timeout is always included for the same reason: git must not be held up by a
// village that stopped answering. See DefaultUploadBudget.
func uploadTokens(root string, binding Binding) []token {
	tokens := boundTokens(binding)
	tokens = append(tokens,
		token{value: "village"}, token{value: "push"},
		token{value: "--non-interactive"}, token{value: "--quiet"},
		token{value: "--timeout"}, token{value: binding.UploadBudget().String()})
	if root != "" {
		tokens = append(tokens, token{value: "--repository"}, token{value: root, quoted: true})
	}
	return tokens
}

// renderShell joins tokens into a POSIX shell command line, quoting the values.
func renderShell(tokens []token) string {
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t.quoted {
			parts = append(parts, ShellQuote(t.value))
			continue
		}
		parts = append(parts, t.value)
	}
	return strings.Join(parts, " ")
}

// renderArgv returns tokens as raw arguments, with no quoting of any kind: each
// element is already exactly one argument.
func renderArgv(tokens []token) []string {
	args := make([]string, 0, len(tokens))
	for _, t := range tokens {
		args = append(args, t.value)
	}
	return args
}

// CommandLine renders the exact upload command a hook runs, as a POSIX shell
// command line. This is the single builder every displayed and executed shell
// rendering of the command comes from.
func CommandLine(binding Binding) string {
	return renderShell(uploadTokens("", binding))
}

// RepositoryArgv renders the upload for root as an ARGUMENT LIST: the program
// followed by one element per argument, with no shell quoting anywhere.
//
// It exists for the hooks Peasant must never hand shell syntax to. The advice
// for a '#!/usr/bin/env python3' hook is to start the upload the way python
// starts a program, and python passes each list element through untouched - so a
// shell-quoted "--repository '/path'" arrives with the apostrophes still on it
// and resolves nothing.
func RepositoryArgv(root string, binding Binding) []string {
	return renderArgv(uploadTokens(root, binding))
}

// ArgumentList renders an argument list in the bracketed, double-quoted form
// python, ruby, and node all accept literally, so the reader of a refusal can
// transcribe it into their own hook without inventing quoting rules.
//
// It is deliberately NOT a shell command line: the whole point is that no shell
// is involved, and a path containing a space has to stay one element.
func ArgumentList(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// ManualCommand renders the upload for root as a PERSON runs it: the same bound
// paths, without the three flags that exist only because a hook runs the
// command - the non-interactive answer to the visibility confirmation, the quiet
// output, and the time budget whose whole purpose is to keep git moving. When
// root is empty, the command is unscoped; otherwise it retains that boundary.
//
// It is derived from the same bound paths rather than written out, because the
// hardcoded "peasant village push --repository <root>" a hook used to print as
// its run-it-by-hand line pushes from a DIFFERENT store than the hook itself
// whenever the install bound a config or data directory.
func ManualCommand(root string, binding Binding) string {
	tokens := append(boundTokens(binding), token{value: "village"}, token{value: "push"})
	if root != "" {
		tokens = append(tokens, token{value: "--repository"}, token{value: root, quoted: true})
	}
	return renderShell(tokens)
}

// StatusCommand renders the command that reports the hook state of root.
func StatusCommand(root string) string {
	return StatusCommandWithBinding(root, Binding{})
}

// StatusCommandWithBinding is StatusCommand in the same bound path context as
// the hook being inspected.
func StatusCommandWithBinding(root string, binding Binding) string {
	tokens := append(boundTokens(binding),
		token{value: "village"}, token{value: "hooks"}, token{value: "status"},
		token{value: "--dir"}, token{value: root, quoted: true})
	return renderShell(tokens)
}

// UninstallCommand renders the command that removes event's hook from root.
func UninstallCommand(event Event, root string) string {
	return UninstallCommandWithBinding(event, root, Binding{})
}

// UninstallCommandWithBinding is UninstallCommand in the same bound path
// context as the hook. The paths do not affect removal, but retaining them keeps
// every command printed by a generated hook internally consistent.
func UninstallCommandWithBinding(event Event, root string, binding Binding) string {
	tokens := append(boundTokens(binding),
		token{value: "village"}, token{value: "hooks"}, token{value: "uninstall"},
		token{value: "--event"}, token{value: event.String()},
		token{value: "--dir"}, token{value: root, quoted: true})
	return renderShell(tokens)
}

// InstallCommand renders the command that writes event's hook for root. Status
// hands it back when an installed hook no longer matches the repository it was
// written for, so the fix is a command the user can paste.
//
// --dir is never optional here. A reinstall command printed without it acts on
// whatever repository the user happens to be standing in, which for a message
// about ONE repository's hook is either an error or - worse - a silent install
// into an unrelated repository.
func InstallCommand(event Event, root string) string {
	return InstallCommandWithBinding(event, root, Binding{})
}

// InstallCommandWithBinding is InstallCommand with the config/data/state paths
// the resulting generated hook must retain.
func InstallCommandWithBinding(event Event, root string, binding Binding) string {
	tokens := append(boundTokens(binding),
		token{value: "village"}, token{value: "hooks"}, token{value: "install"},
		token{value: "--event"}, token{value: event.String()},
		token{value: "--dir"}, token{value: root, quoted: true})
	return renderShell(tokens)
}

// InstallCommandWithBudget renders the complete reinstall command after a hook
// exhausts its cap. It preserves both --dir and the hook's bound path overrides,
// so reinstalling cannot silently switch the hook to another config or store.
func InstallCommandWithBudget(event Event, root string, budget time.Duration, binding Binding) string {
	tokens := append(boundTokens(binding),
		token{value: "village"}, token{value: "hooks"}, token{value: "install"},
		token{value: "--event"}, token{value: event.String()},
		token{value: "--dir"}, token{value: root, quoted: true},
		token{value: "--timeout"}, token{value: budget.String()})
	return renderShell(tokens)
}

// EmbeddedRepository returns the repository root a generated hook was written
// for, or "" when content carries no such assignment. Peasant bakes the root
// into the hook, so a repository that has since been moved or renamed can be
// detected by comparing this against the root Git resolves today.
func EmbeddedRepository(content []byte) string {
	for _, line := range scriptLines(content) {
		if value, ok := strings.CutPrefix(line, embeddedRepositoryAssignment); ok {
			return shellUnquote(value)
		}
	}
	return ""
}

// EmbeddedCommand returns the upload command a generated hook actually runs, as
// that hook's own header records it, or "" when content carries no such header.
// It is what runs today, which is not necessarily what a fresh install would
// write: the bound paths were fixed when the hook was installed.
func EmbeddedCommand(content []byte) string {
	for _, line := range scriptLines(content) {
		if value, ok := strings.CutPrefix(line, headerCommandPrefix); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// scriptLines splits hook content into lines, tolerating CRLF so a hook written
// on one platform is still readable on another.
func scriptLines(content []byte) []string {
	return strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
}

// RepositoryCommand renders the exact upload command for root, including every
// explicitly bound path override. Generated hooks EXECUTE this, and status
// displays it, so what a hook runs is never wider in scope than what it shows.
func RepositoryCommand(root string, binding Binding) string {
	return renderShell(uploadTokens(root, binding))
}

// ManualRecovery renders the command a user runs after a hook's upload failed:
// enter the repository, then run the by-hand form.
//
// It is deliberately NOT the command the hook itself ran. A hook's upload
// carries --quiet and a time budget, both of which exist only because a hook
// fires on every commit; telling a user to "retry by hand, drop --quiet and add
// --verbose for detail" and then printing the identical capped, quiet command
// reproduces the same failure with the same cap and no extra detail. The
// by-hand builder already drops exactly those flags, so it is used here.
func ManualRecovery(root string, binding Binding) string {
	return "cd " + shellQuote(root) + " && " + ManualCommand(root, binding)
}

// Script renders the exact bytes Peasant writes for event in the repository
// rooted at root, whose effective hook file is path.
func Script(event Event, root, path string, binding Binding) (string, error) {
	if err := validateScriptInputs(event, root, path, binding); err != nil {
		return "", err
	}
	return strings.NewReplacer(
		"{{EVENT}}", event.String(),
		"{{EVENT_Q}}", shellQuote(event.String()),
		"{{TIMING}}", event.Timing(),
		"{{TIMING_Q}}", shellQuote(event.Timing()),
		"{{IMPACT_Q}}", shellQuote(event.Impact()),
		"{{ROOT}}", root,
		"{{ROOT_Q}}", shellQuote(root),
		"{{PATH}}", path,
		"{{PATH_Q}}", shellQuote(path),
		"{{SCOPED_COMMAND}}", RepositoryCommand(root, binding),
		"{{MANUAL_COMMAND}}", ManualCommand(root, binding),
		"{{RECOVERY_Q}}", shellQuote(ManualRecovery(root, binding)),
		"{{COMMAND_PREFIX_Q}}", shellQuote(CommandPrefix(binding)),
		"{{BUDGET}}", binding.UploadBudget().String(),
		"{{STATUS}}", StatusCommandWithBinding(root, binding),
		"{{UNINSTALL}}", UninstallCommandWithBinding(event, root, binding),
		"{{UNINSTALL_Q}}", shellQuote(UninstallCommandWithBinding(event, root, binding)),
		"{{NOTHING_ATTEMPTED}}", defaults.ExitNothingAttempted.String(),
		"{{STDIN}}", stdinSection(event),
	).Replace(scriptTemplate), nil
}

// ManualSnippet renders the exact section a user can paste into a hook Peasant
// refuses to touch. It carries no ownership markers, so pasting it never makes
// Peasant claim that file.
//
// The section is exit-status neutral by construction, and that is what makes its
// placement rule safe to follow literally. It captures the status the
// surrounding hook's own commands left, runs the upload in a subshell that
// always succeeds, and then restores that status WITHOUT exiting. So it neither
// masks a failure of those commands when it is appended at the end of a file,
// nor makes a following exit line unreachable when it is placed above one.
func ManualSnippet(event Event, root, path string, binding Binding) (string, error) {
	if err := validateScriptInputs(event, root, path, binding); err != nil {
		return "", err
	}
	return strings.NewReplacer(
		"{{EVENT}}", event.String(),
		"{{IMPACT}}", event.Impact(),
		"{{ROOT}}", root,
		"{{ROOT_Q}}", shellQuote(root),
		"{{PATH}}", path,
		"{{SCOPED_COMMAND}}", RepositoryCommand(root, binding),
		"{{RECOVERY_Q}}", shellQuote(ManualRecovery(root, binding)),
		"{{STDIN_NOTE}}", stdinNote(event),
	).Replace(manualTemplate), nil
}

// stdinSection returns the stdin handling a generated hook needs. Git streams
// the refs being pushed into a pre-push hook; draining them keeps Git from
// being left writing into a pipe nobody reads. A post-commit hook gets no
// stdin, and reading it there could block on the user's terminal.
//
// The drain is a read loop rather than 'cat >/dev/null' for the same reason the
// runtime quoting below is a builtin: cat is a separate program that has to be
// found on PATH, and a drain that fails to run leaves Git writing into a pipe
// nobody reads - the exact condition this section exists to prevent, reintroduced
// by the environment. read is a builtin, so it cannot go missing. It returns
// non-zero at end of input, which is what ends the loop; a final line with no
// terminator is discarded like every other, because nothing here is parsed.
func stdinSection(event Event) string {
	if event != EventPrePush {
		return ""
	}
	return "\n# git streams the refs being pushed on this hook's stdin. Drain them so git\n" +
		"# is never left writing into a pipe nobody reads.\n" +
		"while read -r peasant_hook_pushed_ref; do :; done\n"
}

// stdinNote returns the pre-push ordering warning for the manual snippet. The
// snippet must not drain stdin itself, because the hook it joins may need it.
func stdinNote(event Event) string {
	if event != EventPrePush {
		return ""
	}
	return "# Your pre-push hook receives the refs being pushed on stdin. If your own\n" +
		"# commands read them, add this section after that read.\n"
}

// validateScriptInputs rejects anything that cannot be embedded in a shell
// script safely and legibly. A newline in a path would break both the framing
// comments and the quoting, so Peasant refuses instead of writing a hook it
// cannot reason about.
func validateScriptInputs(event Event, root, path string, binding Binding) error {
	if err := event.Validate(); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{"repository root", root},
		{"hook path", path},
		{"--config", binding.ConfigPath},
		{"--config-dir", binding.ConfigDir},
		{"--data-dir", binding.DataDir},
		{"--state-dir", binding.StateDir},
	} {
		if strings.ContainsAny(field.value, "\n\r\x00") {
			return fmt.Errorf(
				"githooks: %s contains a line break or NUL and cannot be written into a hook\n"+
					"What went wrong: the value %q for %s has a character that cannot be embedded in a shell script.\n"+
					"Why: a generated hook quotes these values inline and documents them in comments; a line break would corrupt both.\n"+
					"Where: githooks.validateScriptInputs, generating the %s hook.\n"+
					"When: while rendering the hook script, before any file was created or changed.\n"+
					"Impact: nothing was installed; the existing hook state is untouched.\n"+
					"Fix: move the repository, or pass paths without line breaks, then retry.",
				field.name, field.value, field.name, event,
			)
		}
	}
	if binding.Timeout < 0 {
		return fmt.Errorf(
			"githooks: the upload time budget cannot be negative\n"+
				"What went wrong: a budget of %s was requested for the %s hook.\n"+
				"Why: the budget caps the whole upload, and a negative cap has no meaning.\n"+
				"Where: githooks.validateScriptInputs.\n"+
				"When: while rendering the hook script, before any file was created or changed.\n"+
				"Impact: nothing was installed; the existing hook state is untouched.\n"+
				"Fix: pass a positive duration such as --timeout 30s, or omit it to use %s.",
			binding.Timeout, event, DefaultUploadBudget,
		)
	}
	if strings.TrimSpace(root) == "" || strings.TrimSpace(path) == "" {
		return fmt.Errorf(
			"githooks: cannot render the %s hook without a repository root and hook path\n"+
				"What went wrong: root=%q path=%q.\n"+
				"Why: both values are documented in the hook and used in its messages.\n"+
				"Where: githooks.validateScriptInputs.\n"+
				"When: while rendering the hook script, before any file was created or changed.\n"+
				"Impact: nothing was installed; the existing hook state is untouched.\n"+
				"Fix: resolve the repository first, then render the hook.",
			event, root, path,
		)
	}
	return nil
}

// ShellQuote wraps s in single quotes so a POSIX shell reads it literally.
//
// It is exported because every command Peasant prints for a user to paste has
// to be quoted the same way. A remedy rendered without it - "--repository
// /tmp/my repo" - is advice that cannot be followed from the state that printed
// it, which is worse than no remedy at all.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellQuote is the package-internal spelling of ShellQuote.
func shellQuote(s string) string { return ShellQuote(s) }

// shellUnquote reverses shellQuote. A value that is not in exactly that form is
// returned unchanged: a hook Peasant did not write is not Peasant's to
// reinterpret, and a wrong guess would be reported to the user as fact.
func shellUnquote(s string) string {
	if len(s) < 2 || !strings.HasPrefix(s, "'") || !strings.HasSuffix(s, "'") {
		return s
	}
	return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
}

// scriptTemplate is the whole generated hook. It stays ASCII so it behaves
// identically under every locale Git might run it in.
const scriptTemplate = `#!/bin/sh
# BEGIN GENERATED FROM peasant village hooks
#
# Peasant generated this file. It uploads this repository's recorded sessions to
# your village, and does nothing else.
#
#   event      : {{EVENT}}
#   timing     : git runs it {{TIMING}}
#   repository : {{ROOT}}
#   hook file  : {{PATH}}
#   command    : {{SCOPED_COMMAND}}
#
# The scope is a project identity, not a path. When this repository has an origin
# remote peasant can normalize, the identity is derived from it and every clone
# of that remote resolves to the same scope. Otherwise - no origin remote, or an
# origin that is not a network remote, such as a local path or a file:// URL -
# the identity is the worktree path the sessions were recorded in, which belongs
# to this directory alone. Run the inspect command below to see which of the two
# this repository actually uses.
#
# The command runs non-interactively, so it answers on your behalf every
# confirmation it would otherwise ask you for, including the one about who can
# see a published session. It does not decide that here: the upload resolves who
# can see a session when it runs, and prints what it actually applied. That
# output is the only statement of it that cannot be out of date - this file was
# written once, when the hook was installed, and your configuration can change
# afterwards without it.
#
# The upload is synchronous: it needs network access and a valid village login,
# and it adds its own latency to the git command. It never blocks git, because
# this file always exits 0 and a failed upload only prints a warning.
#
# The whole upload is capped at {{BUDGET}} so a village that stops answering cannot
# hold git up. Giving up is a warning, not a failure, and the next commit or push
# tries again. If an upload genuinely needs longer - a large first push - run it
# once by hand without the time budget (the same bound paths, without the flags
# that exist only for a hook):
#
#   cd {{ROOT_Q}} && {{MANUAL_COMMAND}}
#
#   inspect   : {{STATUS}}
#   uninstall : {{UNINSTALL}}
#
# Do not edit this file by hand. 'peasant village hooks' rewrites or removes it
# only while its first two lines and its last line are still intact.

set -u
{{STDIN}}
peasant_hook_event={{EVENT_Q}}
peasant_hook_repository={{ROOT_Q}}
peasant_hook_file={{PATH_Q}}
peasant_hook_timing={{TIMING_Q}}
peasant_hook_impact={{IMPACT_Q}}
peasant_hook_command_prefix={{COMMAND_PREFIX_Q}}
peasant_hook_uninstall={{UNINSTALL_Q}}
peasant_hook_recovery={{RECOVERY_Q}}

# Quote a path discovered at runtime as one POSIX shell argument. The repository
# known at install time was quoted by the Go builder; this is only for the new
# root Git reports after that original path has moved.
#
# It closes over every embedded quote with parameter expansion and a case test -
# both shell builtins - rather than piping through sed. sed is a separate program
# that has to be found on PATH, and when it is not, the substitution yields
# NOTHING: the commands below then read --dir '' and cannot work, which is
# exactly the state a user reaches after renaming their repository and the one
# moment these commands exist for. A builtin cannot go missing.
peasant_hook_quote() {
	peasant_hook_quote_rest=$1
	peasant_hook_quote_done=''
	while :; do
		case $peasant_hook_quote_rest in
		*\'*)
			# End the open quote, emit an escaped quote, reopen: '\''
			peasant_hook_quote_done="$peasant_hook_quote_done${peasant_hook_quote_rest%%\'*}'\\''"
			peasant_hook_quote_rest=${peasant_hook_quote_rest#*\'}
			;;
		*)
			printf "'%s'" "$peasant_hook_quote_done$peasant_hook_quote_rest"
			return 0
			;;
		esac
	done
}

# $3 is what the failure means for the village. It is a parameter because a
# non-zero exit does NOT imply nothing was published: an upload can succeed and
# the run still fail afterwards, and the budget message peasant prints above says
# so itself. Asserting 'nothing reached the village' there contradicted peasant's
# own output three lines earlier.
peasant_hook_warn() {
	printf 'peasant: village upload did not complete\n' >&2
	printf '  what  : %s\n' "$1" >&2
	printf '  why   : %s\n' "$2" >&2
	printf '  where : repository %s, hook %s\n' "$peasant_hook_repository" "$peasant_hook_file" >&2
	printf '  when  : %s (%s)\n' "$peasant_hook_timing" "$peasant_hook_event" >&2
	printf '  means : %s, and %s\n' "$3" "$peasant_hook_impact" >&2
	printf '  fix   : %s\n' "$4" >&2
	printf '  stop  : %s\n' "$peasant_hook_uninstall" >&2
}

# Every command this hook prints is rendered against the repository pinned above,
# because that is the only path it knew at install time. If that directory is
# gone the repository was moved or renamed, and all three - where, fix, and the
# uninstall escape hatch - name a directory that does not exist, so following any
# of them fails. Ask git where the worktree is NOW and re-point them.
#
# A linked worktree is not this case: it runs its main worktree's hooks
# directory, and that main worktree is still there, so this test is false and
# nothing below changes.
if [ ! -d "$peasant_hook_repository" ]; then
	peasant_hook_root_now=$(git rev-parse --show-toplevel 2>/dev/null) || peasant_hook_root_now=''
	if [ -n "$peasant_hook_root_now" ]; then
		peasant_hook_root_now_q=$(peasant_hook_quote "$peasant_hook_root_now")
		peasant_hook_uninstall="$peasant_hook_command_prefix village hooks uninstall --event $peasant_hook_event --dir $peasant_hook_root_now_q"
		peasant_hook_warn \
			"the repository this hook was installed for is not at $peasant_hook_repository any more" \
			"it was moved or renamed; this hook pins its upload to that old path, and git resolves this worktree as $peasant_hook_root_now" \
			'nothing reached the village: the upload was not attempted, because the repository it names is not there' \
			"refresh this hook against where the repository is now: $peasant_hook_command_prefix village hooks install --event $peasant_hook_event --dir $peasant_hook_root_now_q"
	else
		peasant_hook_warn \
			"the repository this hook was installed for is not at $peasant_hook_repository any more" \
			'it was moved or renamed, and git could not name the worktree this hook is running in either' \
			'nothing reached the village: the upload was not attempted, because the repository it names is not there' \
			"cd into the repository and refresh this hook there: $peasant_hook_command_prefix village hooks install --event $peasant_hook_event --dir ."
	fi
	exit 0
fi

if ! command -v peasant >/dev/null 2>&1; then
	peasant_hook_warn \
		'the peasant command was not found' \
		'git ran this hook with a PATH that does not contain the peasant binary' \
		'nothing reached the village, because the upload never started' \
		"install peasant or put it on the PATH git uses, then retry with: $peasant_hook_recovery"
	exit 0
fi

{{SCOPED_COMMAND}} </dev/null
peasant_hook_status=$?
if [ "$peasant_hook_status" -ne 0 ]; then
	# peasant exits {{NOTHING_ATTEMPTED}} when it failed before making a single village
	# request - an expired login is the common one. Claiming that "whatever
	# finished is on the village and is recorded as published" there describes an
	# upload that never started.
	if [ "$peasant_hook_status" -eq {{NOTHING_ATTEMPTED}} ]; then
		peasant_hook_meaning='nothing reached the village: the upload never started, so nothing was published and nothing was recorded as published'
	else
		peasant_hook_meaning='whatever the upload finished before it stopped is on the village and is recorded as published; the peasant output above says what did and did not get through'
	fi
	peasant_hook_warn \
		"the village upload command exited $peasant_hook_status" \
		'the upload failed, most often an expired village login, no network, or a rejected payload' \
		"$peasant_hook_meaning" \
		"retry by hand with this exact bound command - it runs without this hook's quiet output and without its time budget, so it prints the full result and is not cut off (add --verbose for per-session detail, and run peasant village login first if the login expired): $peasant_hook_recovery"
fi

exit 0
# END GENERATED FROM peasant village hooks
`

// manualTemplate is the by-hand section offered when Peasant refuses to touch a
// hook it does not own. It stays ASCII for the same reason as scriptTemplate.
//
// It deliberately ends by RESTORING the previous status in a subshell rather
// than by exiting. An `exit` here would silently make every line below the
// section unreachable, which would turn a pre-push policy gate's own `exit 1`
// into a no-op for anyone who followed the placement instruction literally.
const manualTemplate = `# BEGIN peasant village upload ({{EVENT}}) -- added by hand, not managed by peasant
# Add this to {{PATH}}, as the last thing that runs: put it at the end of the
# file, or directly above a final 'exit' line if the file has one, because
# nothing below an 'exit' ever runs.
# Wherever you put it, it does not change what your hook reports to git: it
# always succeeds itself, and then restores the status your own commands left.
peasant_hook_previous_status=$?
{{STDIN_NOTE}}(
	if command -v peasant >/dev/null 2>&1; then
		{{SCOPED_COMMAND}} </dev/null \
			|| printf 'peasant: village upload failed for %s ({{EVENT}}); {{IMPACT}}; retry by hand with: %s\n' {{ROOT_Q}} {{RECOVERY_Q}} >&2
	else
		printf 'peasant: the peasant command is not on PATH, village upload skipped for %s ({{EVENT}})\n' {{ROOT_Q}} >&2
	fi
) || true
# Restore the status your commands left, in a subshell: a bare 'exit' here would
# make every line below this section unreachable.
(exit "$peasant_hook_previous_status")
# END peasant village upload ({{EVENT}})
`
