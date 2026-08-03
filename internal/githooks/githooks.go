// Package githooks manages the opt-in, per-repository Git hooks that upload a
// repository's recorded sessions to the village.
//
// The scope is deliberately narrow and is meant to stay that way. Peasant
// manages exactly two conventional client-side events — post-commit (a commit
// has completed) and pre-push (a push is starting, which is NOT a confirmation
// that the push reached the remote) — and the only thing a managed hook does is
// run
//
//	peasant village push --non-interactive --repository <resolved-repository>
//
// synchronously for the repository Git is acting on. Explicit config, config
// directory, data directory, and state directory overrides supplied during
// installation are bound into that same displayed and executed command. There is no hook
// framework, no plugin registry, no daemon, no queue, and no background worker
// here.
//
// Three rules are absolute:
//
//  1. Peasant only ever rewrites or removes a hook file it wrote itself. A slot
//     holding anything else is never edited, wrapped, renamed, chmod'd, or
//     deleted; installation refuses that event. It offers a by-hand section only
//     after positively classifying a repository-local file as POSIX shell.
//  2. Peasant never reads, writes, or changes core.hooksPath at any scope. The
//     effective path is obtained by asking Git itself (git rev-parse
//     --git-path), which already accounts for whatever hooks directory Git is
//     configured to run.
//  3. A generated hook always exits successfully. A failed upload prints an
//     actionable warning and lets the commit or push carry on.
package githooks

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// Event is a supported client-side Git hook event. The set is closed: these are
// the only two events Peasant knows how to manage, and adding a third is a
// deliberate contract change, not a configuration knob.
type Event string

const (
	// EventPostCommit fires after Git has finished creating a commit.
	EventPostCommit Event = "post-commit"
	// EventPrePush fires before Git starts a push. Git has not contacted the
	// remote yet, so this event can never report that a push succeeded.
	EventPrePush Event = "pre-push"
)

// AllEvents is the closed set of managed events, in the order reports use.
var AllEvents = [...]Event{EventPostCommit, EventPrePush}

// String renders the event as the conventional Git hook filename.
func (e Event) String() string { return string(e) }

// Validate reports whether e is one of the managed events.
func (e Event) Validate() error {
	for _, known := range AllEvents {
		if e == known {
			return nil
		}
	}
	return fmt.Errorf(
		"githooks: unsupported hook event %q\n"+
			"What went wrong: %q is not an event Peasant manages.\n"+
			"Why: Peasant deliberately manages only the two client-side events that map to a village upload.\n"+
			"Where: githooks.Event.Validate.\n"+
			"When: while validating a hook lifecycle request, before any file was inspected or written.\n"+
			"Impact: no hook was planned, installed, or removed.\n"+
			"Fix: pass one of %s.",
		string(e), string(e), EventList(AllEvents[:]),
	)
}

// Timing describes, in the words shown to a user, when Git runs the event. It
// is the single source of the timing wording used by generated hooks, refusal
// guidance, and the CLI, so the honesty of the pre-push description cannot
// drift between surfaces.
func (e Event) Timing() string {
	switch e {
	case EventPostCommit:
		return "after the commit has been created"
	case EventPrePush:
		return "before the push starts, while Git has not contacted the remote yet"
	default:
		return "at an unsupported point"
	}
}

// Impact describes what a failed upload does NOT do to the Git operation in
// flight. It is the "what this means for you" line of a hook's warning.
func (e Event) Impact() string {
	switch e {
	case EventPostCommit:
		return "the commit is already recorded and is not changed"
	case EventPrePush:
		return "the push itself is unaffected and carries on"
	default:
		return "the Git operation is unaffected"
	}
}

// ParseEvent converts raw user input (a CLI flag value) into a managed Event.
func ParseEvent(raw string) (Event, error) {
	event := Event(strings.TrimSpace(raw))
	if err := event.Validate(); err != nil {
		return "", err
	}
	return event, nil
}

// EventList renders events as a comma-separated list for error messages.
func EventList(events []Event) string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.String())
	}
	return strings.Join(names, ", ")
}

// Ownership is what Peasant found in a hook slot. It is the only thing that
// decides whether a mutation is allowed.
type Ownership string

const (
	// OwnershipAbsent means no file exists at the effective hook path.
	OwnershipAbsent Ownership = "absent"
	// OwnershipPeasant means the file is intact Peasant-generated content:
	// Peasant may rewrite or remove it.
	OwnershipPeasant Ownership = "peasant"
	// OwnershipForeign means a file exists that Peasant did not write, or whose
	// Peasant-generated framing is no longer intact. It is never touched.
	OwnershipForeign Ownership = "foreign"
)

// AllOwnerships is the closed set of ownership states.
var AllOwnerships = [...]Ownership{OwnershipAbsent, OwnershipPeasant, OwnershipForeign}

// String renders the ownership state for CLI output.
func (o Ownership) String() string { return string(o) }

// Refusal is why Peasant will not manage a slot. It is a closed set so every
// surface can phrase the same fact for its own operation: telling someone who is
// removing a hook to "add this section by hand" is install wording leaking into
// uninstall.
type Refusal string

const (
	// RefusalNone means nothing was refused.
	RefusalNone Refusal = ""
	// RefusalSharedPath means the effective hook path is outside the resolved
	// repository, so it is shared: every worktree and every repository whose
	// configuration resolves to that path runs the same file. Repository-specific
	// consent cannot be honored there.
	RefusalSharedPath Refusal = "shared-path"
	// RefusalForeignFile means a file Peasant did not write occupies the slot.
	RefusalForeignFile Refusal = "foreign-file"
)

// String renders the refusal reason for CLI output.
func (r Refusal) String() string { return string(r) }

// HookLanguage is what Git will execute an existing hook file AS. It decides
// one thing and nothing else: whether the by-hand POSIX shell section Peasant
// offers can be pasted into that file at all.
//
// It is a closed set because the wrong answer is destructive. Pasting a shell
// section into a hook whose shebang names another interpreter — the
// `#!/usr/bin/env python3` pre-commit-framework hook is the common one — does
// not "not work": it makes the file a syntax error, so every push is blocked by
// a dead pre-push hook and every post-commit hook fails silently.
type HookLanguage string

const (
	// HookLanguageUnclassified is the zero value: no file was read, so nothing
	// was decided. It never means "safe".
	HookLanguageUnclassified HookLanguage = ""
	// HookLanguagePOSIXShell means the file runs under a POSIX-compatible
	// shell, either because its shebang names one or because it has no shebang
	// at all — Git falls back to running such a file with the shell.
	HookLanguagePOSIXShell HookLanguage = "posix-shell"
	// HookLanguageOther means the shebang names an interpreter that is not a
	// shell. Slot.Interpreter carries its name.
	HookLanguageOther HookLanguage = "other"
	// HookLanguageBinary means the file is not a text script at all, so no
	// section of any language can be added to it.
	HookLanguageBinary HookLanguage = "binary"
)

// AllHookLanguages is the closed set used by fixture validation and consumers
// that must reject an invented classification rather than treating it as a shell.
var AllHookLanguages = [...]HookLanguage{
	HookLanguageUnclassified, HookLanguagePOSIXShell, HookLanguageOther, HookLanguageBinary,
}

// String renders the language for CLI output.
func (l HookLanguage) String() string { return string(l) }

// WarningKind is the closed set of non-blocking disclosures Peasant attaches to
// a hook it successfully wrote. A warning never means the install failed; it
// means something true about the result that consent depends on.
type WarningKind string

const (
	// WarningCommittableHook means the hook was written inside the working tree
	// rather than inside the Git directory, so it can be committed and would
	// then run for everyone who adopts that hooks directory.
	WarningCommittableHook WarningKind = "committable-hook"
	// WarningSharedWithLinkedWorktrees means the repository has linked
	// worktrees, which run the main worktree's hooks directory and therefore
	// this same file. It is disclosed on the install that SUCCEEDS, not only on
	// the one that is refused: installing from a linked worktree is refused
	// with a full explanation, while installing from the main worktree quietly
	// arms every linked worktree too.
	WarningSharedWithLinkedWorktrees WarningKind = "shared-with-linked-worktrees"
	// WarningRepositoryMoved means an installed hook names a repository root
	// that is not the one Git resolves today, so it fails on every event.
	WarningRepositoryMoved WarningKind = "repository-moved"
	// WarningHookNotExecutable means an installed hook has lost its executable
	// bit. Git refuses to run such a file and prints a hint instead, so the hook
	// is present and inert: reporting it as installed and active would describe
	// uploads that are not happening.
	WarningHookNotExecutable WarningKind = "hook-not-executable"
)

// String renders the warning kind for CLI output.
func (k WarningKind) String() string { return string(k) }

// Warning is one non-blocking disclosure. Kind classifies it; Detail is the
// actionable text a user reads.
type Warning struct {
	Kind   WarningKind
	Detail string
}

// Action is what Install would do for one event, given the slot's current
// state.
type Action string

const (
	// ActionCreate writes a new hook file with an exclusive create.
	ActionCreate Action = "create"
	// ActionReplace rewrites a slot Peasant already owns.
	ActionReplace Action = "replace"
	// ActionRefuse changes nothing and returns manual integration guidance.
	ActionRefuse Action = "refuse"
)

// AllActions is the closed set of planned actions.
var AllActions = [...]Action{ActionCreate, ActionReplace, ActionRefuse}

// String renders the planned action for CLI output.
func (a Action) String() string { return string(a) }

// Outcome is what a mutating call actually did for one event.
type Outcome string

const (
	// OutcomeCreated means a hook file was written where none existed.
	OutcomeCreated Outcome = "created"
	// OutcomeReplaced means an existing Peasant-owned hook was rewritten.
	OutcomeReplaced Outcome = "replaced"
	// OutcomeRemoved means a Peasant-owned hook file was deleted.
	OutcomeRemoved Outcome = "removed"
	// OutcomeNotPresent means uninstall found nothing of Peasant's to remove.
	OutcomeNotPresent Outcome = "not-present"
	// OutcomeRefused means Peasant declined to act and changed nothing.
	OutcomeRefused Outcome = "refused"
	// OutcomeFailed means Peasant tried to act and the filesystem or an
	// unexpected state stopped it. Nothing was changed.
	OutcomeFailed Outcome = "failed"
)

// AllOutcomes is the closed set of mutation outcomes.
var AllOutcomes = [...]Outcome{
	OutcomeCreated, OutcomeReplaced, OutcomeRemoved, OutcomeNotPresent, OutcomeRefused, OutcomeFailed,
}

// String renders the outcome for CLI output.
func (o Outcome) String() string { return string(o) }

// Repository is the resolved Git repository a lifecycle call acts on.
type Repository struct {
	// RequestedDir is the absolute form of the directory the caller named.
	RequestedDir string
	// Root is the absolute worktree top level Git reported for RequestedDir.
	Root string
	// GitDir is the absolute Git directory of Root. A hook path inside Root but
	// outside GitDir is ordinary working-tree content: it can be committed, and
	// would then run for anyone who adopts that hooks directory.
	GitDir string
}

// Binding is the non-secret Peasant context bound into a generated hook so the
// hook runs against the same configuration and data the install command used.
// Empty fields are omitted from the generated command line, leaving Peasant's
// normal resolution in force; the peasant binary itself is always resolved from
// PATH.
type Binding struct {
	ConfigPath string
	ConfigDir  string
	DataDir    string
	StateDir   string
	// Timeout is the overall budget bound into the generated hook's upload
	// command. Zero means DefaultUploadBudget.
	//
	// It is chosen at install time rather than edited afterwards on purpose: a
	// generated hook is only manageable while its framing is intact, so a user
	// who needed a longer budget and edited the file would break ownership and
	// leave a hook that still uploads but can no longer be removed by Peasant.
	Timeout time.Duration
}

// UploadBudget is the overall time budget a hook built from this binding pins
// into its upload command.
func (b Binding) UploadBudget() time.Duration {
	if b.Timeout > 0 {
		return b.Timeout
	}
	return DefaultUploadBudget
}

// Request is one lifecycle call: which repository, which events, and the
// context to bind into anything written.
type Request struct {
	// Dir names the repository. Empty means the process working directory.
	Dir string
	// Events are the requested events. Plan, Status, and Uninstall treat an
	// empty slice as every managed event; Install requires at least one.
	Events []Event
	// Binding is bound into generated hooks. Ignored by Status and Uninstall.
	Binding Binding
}

// Slot is the observed state of one event's hook file. Every field describes
// what is on disk right now, never what Peasant intends to do.
type Slot struct {
	Event Event
	// Path is the absolute hook slot Git reports for Event, already accounting
	// for any configured hooks directory. When it is a symlink, LinkTarget is the
	// file Git executes.
	Path      string
	Ownership Ownership
	// Mode is the existing file mode, or zero when the slot is absent.
	Mode fs.FileMode
	// Size is the existing file size in bytes, or zero when absent.
	Size int64
	// HandAdded reports that a foreign file already carries the by-hand upload
	// section Peasant offers when it refuses a slot. Peasant still never edits
	// that file, but it can say so instead of reporting nothing to do.
	HandAdded bool
	// GeneratedRemnant reports that a foreign file still carries the opening
	// marker of a Peasant-generated hook: the framing is no longer intact, so
	// Peasant may not rewrite or delete it, but the upload line it was generated
	// with is still there and still runs. Appending to an existing hook is
	// ordinary tooling behavior, so this is a realistic way for a managed hook
	// to become unmanageable while remaining fully active.
	GeneratedRemnant bool
	// EmbeddedRoot is the repository root a Peasant-generated hook was written
	// for, read back out of the file. Empty unless Ownership is OwnershipPeasant.
	// It differs from the resolved root exactly when the repository has been
	// moved or renamed since the install.
	EmbeddedRoot string
	// EmbeddedCommand is the upload command a Peasant-generated hook actually
	// runs, read back out of the file's own header. Empty unless Ownership is
	// OwnershipPeasant. It reflects the paths bound at install time, which a
	// fresh install would not necessarily reproduce.
	EmbeddedCommand string
	// Language is what Git runs an existing foreign file as. It is
	// HookLanguageUnclassified when no file was read.
	Language HookLanguage
	// Interpreter is the program named on a foreign hook's shebang, when it
	// names one — "python3" for the pre-commit framework's hook. Empty when the
	// file has no shebang or is not a text script.
	Interpreter string
	// LinkTarget is the file Git really executes when Path is a symlink,
	// resolved as far as it resolves. Empty when Path is not a symlink.
	//
	// A symlinked hook is ordinary — dotfile managers, stow, a tracked team
	// hooks/ directory — and it changes two answers that nothing else does.
	// Editing Path edits the TARGET, and deleting Path deletes only the
	// pointer, so every "open this file and change it" instruction has to name
	// the target instead. And what Git runs is the target's bytes, so the
	// classification and activity checks are decided there too.
	LinkTarget string
	// Unreadable reports that the bytes Git would execute for this slot could
	// not be read: a dangling link, a link to a directory, or a file Peasant
	// may not open. Nothing about the file was classified, so no offer that
	// depends on its contents may be made.
	Unreadable bool
}

// FileToEdit names the file a user has to open to change what Git runs for this
// slot. For an ordinary hook that is the slot itself; for a symlinked slot it is
// the target, because appending to the link path appends to the target and
// deleting the link path removes only the pointer.
func (s Slot) FileToEdit() string {
	if s.LinkTarget != "" {
		return s.LinkTarget
	}
	return s.Path
}

// LinkLeavesHooksDirectory reports that Git reaches this slot's file through a
// symlink that lands outside the directory Git runs hooks from.
//
// That file is not this repository's hooks directory content: it is a tracked
// script, a dotfile-manager source, or a shared team hooks file. Pasting an
// upload section pinned to one absolute repository path into it is the same
// leak the shared-path refusal exists to prevent, so the section is withheld
// there even when the target is a perfectly ordinary shell script.
func (s Slot) LinkLeavesHooksDirectory() bool {
	if s.LinkTarget == "" {
		return false
	}
	return !pathWithin(filepath.Dir(s.Path), s.LinkTarget)
}

// AcceptsShellSection reports whether the by-hand POSIX shell section could be
// pasted into what Git actually runs for this slot without destroying it, and
// without landing in a file this repository does not own.
//
// It fails CLOSED on anything Peasant did not positively classify as a shell:
// offering a shell section for a file Git runs with another interpreter is the
// difference between advice that does nothing and advice that blocks every push.
// An unclassified slot is therefore refused, not accepted — a symlink whose
// target could not be read tells Peasant nothing about the interpreter behind
// it, and "nothing is known" is the one answer that must never be treated as
// "a shell".
func (s Slot) AcceptsShellSection() bool {
	if s.Unreadable || s.LinkLeavesHooksDirectory() {
		return false
	}
	return s.Language == HookLanguagePOSIXShell
}

// Managed reports whether the slot contains an intact Peasant-generated hook.
// Its executable mode independently decides whether Git currently runs it.
func (s Slot) Managed() bool { return s.Ownership == OwnershipPeasant }

// CarriesUploadSection reports that a foreign file still contains either the
// by-hand section or the remains of a generated hook. Presence is kept separate
// from activation: a non-executable file carries the section without Git running
// it, and install/uninstall must neither offer a duplicate nor call it absent.
func (s Slot) CarriesUploadSection() bool {
	return s.Ownership == OwnershipForeign && (s.HandAdded || s.GeneratedRemnant)
}

// UploadsFromForeignFile reports that a file Peasant may not touch both carries
// a village upload section and is executable by Git right now.
//
// A symlinked slot is included, because Git runs the target's bytes: a complete
// generated hook moved into a tracked hooks/ directory and linked back uploads
// on every commit, and reporting that as an inert slot tells a user who wants
// the uploads to stop that none are running.
func (s Slot) UploadsFromForeignFile() bool {
	return s.CarriesUploadSection() && executableByGit(s.Mode)
}

// Plan is the read-only answer to "what would install do for this event".
type Plan struct {
	Slot
	// Action is what Install would do.
	Action Action
	// Refusal classifies why Action is ActionRefuse. RefusalNone otherwise.
	Refusal Refusal
	// Script is the exact file content Install would write. Empty when Action
	// is ActionRefuse.
	Script string
	// Manual is the exact snippet the user can add by hand. Non-empty only when
	// a foreign file occupies a slot Peasant could otherwise manage: a shared
	// path is never answered with a snippet, because the file it would go in is
	// run by every worktree and repository that resolves to it.
	Manual string
	// Reason is the actionable explanation of Action, phrased for the operation
	// that produced this plan.
	Reason string
	// Warnings are non-blocking disclosures about the slot as it stands.
	Warnings []Warning
}

// Result is what a mutating call did for one event.
type Result struct {
	// Slot is the state observed immediately before the mutation was attempted.
	Slot
	Outcome Outcome
	// Refusal classifies why Outcome is OutcomeRefused. RefusalNone otherwise.
	Refusal Refusal
	// Manual is the exact snippet the user can add by hand. Non-empty only when
	// an install was refused because a foreign file occupies a manageable slot.
	Manual string
	// Reason is the actionable explanation of Outcome.
	Reason string
	// Warnings are non-blocking disclosures about what was written. They never
	// mean the operation failed.
	Warnings []Warning
	// Err carries the underlying failure when Outcome is OutcomeFailed.
	Err error
}

// PlanReport is the result of a read-only Plan or Status call.
type PlanReport struct {
	Repository Repository
	Plans      []Plan
}

// ChangeReport is the result of an Install or Uninstall call. Events are
// independent: a refusal on one event never suppresses the real outcome of
// another.
type ChangeReport struct {
	Repository Repository
	Results    []Result
}

// Blocked reports whether any event was refused or failed, meaning the
// requested state was not reached everywhere. Callers use it for exit codes;
// the per-event Outcome says whether Peasant would not act or could not.
func (r ChangeReport) Blocked() bool {
	for _, result := range r.Results {
		if result.Outcome == OutcomeRefused || result.Outcome == OutcomeFailed {
			return true
		}
	}
	return false
}
