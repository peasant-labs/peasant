package main

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/spf13/cobra"
)

// hooksLongDescription is shared by the group and its subcommands so the timing
// and safety promises are stated identically wherever a user meets them.
var hooksLongDescription = `Manage the opt-in Git hooks that upload a repository's recorded sessions to the village.

A managed hook runs '` + githooks.CommandLine(githooks.Binding{}) + ` --repository <resolved-repository>'
for the repository git is acting on, and nothing else. Explicit path overrides
supplied during installation are included in that same displayed and executed
command. --quiet keeps an ordinary commit or push from filling with push output;
errors and a final result line are still printed. --non-interactive answers the
public-visibility confirmation on your behalf. Hooks honor the configured
push.visibility. Peasant publishes content and, when needed, follows it with an
owner visibility update to converge the configured private or public state. If
visibility convergence or local receipt persistence fails, no terminal local
receipt is recorded, so the next repository-scoped run retries that session.
--timeout caps the WHOLE upload, so a village that accepts a connection and then
stops answering cannot hold git up: the per-request client timeout does not bound
a push, which issues several requests in sequence. Giving up is a warning, not a
failure, and the next commit or push tries again; to finish a large first upload,
run 'peasant village push --repository <repo>' once by hand without the budget.
The upload is synchronous: it needs network access and a valid village login, and
it adds its own latency to the git command.

The scope is a project identity, not a path. When the repository has an origin
remote Peasant can normalize, the identity is derived from it, so every clone of
that remote resolves to the same scope. Otherwise — no origin remote, or an origin
that is not a network remote, such as a local path or a file:// URL — the identity
is the worktree path the sessions were recorded in, which belongs to that directory
alone and is shared with no other clone. 'peasant village hooks status' and
'peasant village push --repository' both report which of the two was actually used.
A repository nested inside another one keeps its own identity and never inherits
the outer repository's remote, or its sessions. Earlier versions did let it
inherit, so work recorded inside a submodule, a clone inside a clone, or a plain
'git init' in a subdirectory was filed under the OUTER repository. Those sessions
keep that older identity, so such a directory can show up as two projects: the
historical sessions under the outer identity and new ones under its own. Nothing
is lost and no migration runs. 'peasant village push --repository <dir>' names any
sessions it finds recorded there that its scope cannot reach, and the one command
that re-derives them without touching any other project. Annotations that cannot
be attributed to a session or to that project are not published by a
repository-scoped push.

Two events are supported, and they are chosen independently:

  post-commit  git runs it after a commit has been created.
  pre-push     git runs it before a push starts. Git has not contacted the remote
               at that point, so this hook can never tell you whether the push
               itself succeeded.

A managed hook always exits successfully, so a failed upload prints an actionable
warning and lets the commit or push carry on.

Peasant automatically installs only into an absent hook slot, and only rewrites
or removes a hook file it wrote itself. An existing file, including a zero-byte
file or a symlink, occupies the slot. Install refuses it and changes nothing.

Whether it also prints a section you can add by hand depends on what git actually
runs there, which for a symlink is the file the link points at, not the link. The
section is POSIX shell, so it is offered ONLY for a slot git runs with a shell and
whose file this repository owns. Otherwise it is withheld with the reason. A
readable script gets an invocation suited to the detected interpreter: a shell
command for a shell target outside the hooks directory, or an unquoted argument
list a non-shell interpreter can start directly. A binary or unreadable target is
offered no invocation it cannot safely execute. Pasting POSIX shell into a hook
git runs with python does not fail to help: it makes that file a syntax error, so
a pre-push policy gate dies and no push succeeds again.

The path reported for each event is the one git would really run: it is obtained
by asking git. Automatic management is refused when that effective path is outside
the selected repository, because such a path is shared by every worktree and
repository configured to use it; a repository-local custom path is supported.
Peasant never sets, unsets, or changes core.hooksPath at any scope.`

// BuildVillageHooksCommand constructs the `peasant village hooks` group
// (install, status, uninstall).
func BuildVillageHooksCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Install, inspect, and remove the repository's village upload hooks",
		Long:  hooksLongDescription,
	}
	cmd.AddCommand(buildVillageHooksInstallCommand())
	cmd.AddCommand(buildVillageHooksStatusCommand())
	cmd.AddCommand(buildVillageHooksUninstallCommand())
	return cmd
}

func buildVillageHooksInstallCommand() *cobra.Command {
	var (
		dir       string
		rawEvents []string
		budget    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "install --event post-commit [--event pre-push] [--dir <repo>]",
		Short: "Install a village upload hook for the named git events",
		Long: hooksLongDescription + `

At least one --event is required: a hook is never installed from a defaulted or
implied choice. Each event is handled independently, so a refusal on one still
leaves an accurate, acted-upon result for the other.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			events, err := parseHookEvents(rawEvents)
			if err != nil {
				return err
			}
			if levelErr := checkHookRedactionLevel(cmd); levelErr != nil {
				return levelErr
			}
			binding := hookBinding(cmd)
			binding.Timeout = budget
			request := githooks.Request{Dir: dir, Events: events, Binding: binding}
			report, err := githooks.New(githooks.NewExecGit()).Install(cmd.Context(), request)
			if err != nil {
				return err
			}
			renderChangeReport(cmd.OutOrStdout(), report)
			renderInstallEnvironmentNotices(cmd, report)
			return blockedError(report, "install")
		},
	}
	addHookDirFlag(cmd, &dir)
	addHookEventFlag(cmd, &rawEvents, "Git event to manage; repeat for more than one (post-commit, pre-push)")
	cmd.Flags().DurationVar(&budget, "timeout", 0, fmt.Sprintf(
		"Overall time budget the installed hook gives the whole upload before giving up with a warning (default %s). "+
			"Raise it if your village is far away or your first push is large; the hook is best effort either way, and the "+
			"next commit or push retries. Choose it here rather than editing the generated file: an edited hook is no longer "+
			"one Peasant can rewrite or remove.", githooks.DefaultUploadBudget))
	return cmd
}

func buildVillageHooksStatusCommand() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "status [--dir <repo>]",
		Short: "Report the village upload hook state of a repository",
		Long: hooksLongDescription + `

status reads only. It reports, for each event, the path git would really run,
whether a Peasant-generated hook is there, and what to do about anything else it
finds.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			binding := hookBinding(cmd)
			request := githooks.Request{Dir: dir, Binding: binding}
			report, err := githooks.New(githooks.NewExecGit()).Status(cmd.Context(), request)
			if err != nil {
				return err
			}
			renderPlanReport(cmd.OutOrStdout(), binding, report)
			renderStatusEnvironmentNotices(cmd, report)
			return nil
		},
	}
	addHookDirFlag(cmd, &dir)
	return cmd
}

func buildVillageHooksUninstallCommand() *cobra.Command {
	var (
		dir       string
		rawEvents []string
	)
	cmd := &cobra.Command{
		Use:   "uninstall [--event post-commit] [--event pre-push] [--dir <repo>]",
		Short: "Remove the village upload hooks Peasant installed",
		Long: hooksLongDescription + `

With no --event, uninstall considers both events. It deletes only an intact
Peasant-generated file: anything else is reported and left exactly as it is.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			events, err := parseHookEvents(rawEvents)
			if err != nil {
				return err
			}
			request := githooks.Request{Dir: dir, Events: events}
			report, err := githooks.New(githooks.NewExecGit()).Uninstall(cmd.Context(), request)
			if err != nil {
				return err
			}
			renderChangeReport(cmd.OutOrStdout(), report)
			return blockedError(report, "uninstall")
		},
	}
	addHookDirFlag(cmd, &dir)
	addHookEventFlag(cmd, &rawEvents, "Git event to remove; repeat for more than one (default: both)")
	return cmd
}

func addHookDirFlag(cmd *cobra.Command, dir *string) {
	cmd.Flags().StringVar(dir, "dir", ".", "Repository to act on (default: the working directory)")
}

func addHookEventFlag(cmd *cobra.Command, events *[]string, usage string) {
	cmd.Flags().StringArrayVar(events, "event", nil, usage)
}

// parseHookEvents converts repeated --event values into the typed closed set.
func parseHookEvents(raw []string) ([]githooks.Event, error) {
	events := make([]githooks.Event, 0, len(raw))
	for _, value := range raw {
		event, err := githooks.ParseEvent(value)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// hookBinding captures the explicitly-overridden, non-secret Peasant paths so a
// generated hook runs with the same context the install ran with. Unset flags
// bind nothing, leaving Peasant's normal resolution in force inside the hook.
// Relative overrides are made absolute, because a hook runs with git's working
// directory, not the one the install was typed in.
func hookBinding(cmd *cobra.Command) githooks.Binding {
	return githooks.Binding{
		ConfigPath: changedFlagPath(cmd, "config"),
		ConfigDir:  changedFlagPath(cmd, "config-dir"),
		DataDir:    changedFlagPath(cmd, "data-dir"),
		StateDir:   changedFlagPath(cmd, "state-dir"),
	}
}

// changedFlagPath returns the absolute value of a flag the user actually set,
// or "" when the flag is unset or not registered on cmd.
func changedFlagPath(cmd *cobra.Command, name string) string {
	flag := cmd.Flags().Lookup(name)
	if flag == nil || !flag.Changed {
		return ""
	}
	value := flag.Value.String()
	if value == "" {
		return ""
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return value
	}
	return absolute
}

// blockedError turns a report that did not reach the requested state into a
// non-zero exit, after every per-event result has already been printed.
func blockedError(report githooks.ChangeReport, operation string) error {
	if !report.Blocked() {
		return nil
	}
	var blocked []string
	for _, result := range report.Results {
		if result.Outcome == githooks.OutcomeRefused || result.Outcome == githooks.OutcomeFailed {
			blocked = append(blocked, fmt.Sprintf("%s (%s)", result.Event, result.Outcome))
		}
	}
	return fmt.Errorf(
		"village hooks %s did not complete for %s\n"+
			"What went wrong: Peasant did not reach the requested state for those events.\n"+
			"Why: see the per-event explanation printed above.\n"+
			"Where: repository %s.\n"+
			"When: after every other requested event had already been handled.\n"+
			"Impact: the files named above are exactly as they were; the events that succeeded are unaffected.\n"+
			"Fix: follow the per-event guidance above, then re-run 'peasant village hooks %s'.",
		operation, strings.Join(blocked, ", "), report.Repository.Root, operation,
	)
}

// renderChangeReport prints what install or uninstall did, one block per event.
func renderChangeReport(w io.Writer, report githooks.ChangeReport) {
	fmt.Fprintf(w, "repository: %s\n", report.Repository.Root)
	for _, result := range report.Results {
		fmt.Fprintf(w, "\n%-11s  %-11s  %s\n", result.Event, result.Outcome, result.Path)
		writeIndented(w, result.Reason, "  ")
		renderHookWarnings(w, result.Warnings)
		if result.Manual != "" {
			// The file git actually executes, which is the target when the slot
			// is a symlink: naming the link here while the section's own text
			// names the target makes one report say two different things.
			fmt.Fprintf(w, "\n  add this section to %s by hand:\n\n", result.FileToEdit())
			writeIndented(w, result.Manual, "    ")
		}
	}
}

// renderPlanReport prints the read-only status of each event: what git runs, the
// command that hook actually executes, and how to take it away again.
//
// It takes the binding because the uninstall line it prints has to be runnable
// from the state that printed it: a hook installed with --config-dir or
// --data-dir is only removable by a command carrying the same overrides, and one
// rendered without them resolves a different config directory entirely.
func renderPlanReport(w io.Writer, binding githooks.Binding, report githooks.PlanReport) {
	fmt.Fprintf(w, "repository: %s\n", report.Repository.Root)
	for _, plan := range report.Plans {
		fmt.Fprintf(w, "\n%-11s  %-13s  %s\n", plan.Event, hookStateLabel(plan.Ownership), plan.Path)
		fmt.Fprintf(w, "  timing: git runs it %s\n", plan.Event.Timing())
		if plan.EmbeddedCommand != "" {
			fmt.Fprintf(w, "  runs: %s\n", plan.EmbeddedCommand)
			// The uninstall line is printed only when running it would actually
			// work. A shared hook path - a linked worktree reading its main
			// worktree's hooks, or a core.hooksPath outside the repository - is
			// refused for the very --dir this report was produced for, so
			// rendering the command from that root hands the user a line that is
			// guaranteed to be rejected. The refusal prose below already says
			// where to run it instead.
			if plan.Refusal != githooks.RefusalSharedPath {
				fmt.Fprintf(w, "  uninstall: %s\n",
					githooks.UninstallCommandWithBinding(plan.Event, report.Repository.Root, binding))
			}
		}
		writeIndented(w, plan.Reason, "  ")
		renderHookWarnings(w, plan.Warnings)
		if plan.Manual != "" {
			fmt.Fprintf(w, "\n  add this section to %s by hand:\n\n", plan.FileToEdit())
			writeIndented(w, plan.Manual, "    ")
		}
	}
}

// renderHookWarnings prints the non-blocking disclosures attached to a plan or
// result. They are facts about a hook that exists, not failures.
func renderHookWarnings(w io.Writer, warnings []githooks.Warning) {
	for _, warning := range warnings {
		fmt.Fprintf(w, "  warning (%s): ", warning.Kind)
		fmt.Fprintf(w, "%s\n", warning.Detail)
	}
}

// renderInstallEnvironmentNotices reports the things that decide whether an
// installed hook does what the user expects but that are not properties of the
// file itself: whether git will be able to find the peasant binary, and what the
// non-interactive upload will actually do with the settings that decide how much
// content leaves the machine. All are notices, not failures - the hook is
// installed either way, and none can be fixed by changing the file.
func renderInstallEnvironmentNotices(cmd *cobra.Command, report githooks.ChangeReport) {
	// One notice per event that was actually written. Reporting a single event
	// picked from the whole report names whichever event happens to sort first,
	// which is the one most likely to have been REFUSED - so the notice warns
	// about a hook that does not exist, stays silent about the one that will
	// publish, and hands out an uninstall command that removes nothing.
	var installed []githooks.Event
	for _, result := range report.Results {
		if result.Outcome == githooks.OutcomeCreated || result.Outcome == githooks.OutcomeReplaced {
			installed = append(installed, result.Event)
		}
	}
	if len(installed) == 0 {
		return
	}
	stderr := cmd.ErrOrStderr()
	if _, err := exec.LookPath("peasant"); err != nil {
		for _, event := range installed {
			fmt.Fprintf(stderr,
				"notice: the peasant binary is not on this shell's PATH (%v). "+
					"The %s hook resolves it from the PATH git runs with, so until peasant is on that PATH every %s prints "+
					"'the peasant command was not found' and uploads nothing. "+
					"Fix: install peasant onto the PATH git uses, then commit or push again - no re-install of the hook is needed.\n",
				err, event, event)
		}
	}
	renderSettingsNotices(cmd, installed, report.Repository.Root)
}

// renderStatusEnvironmentNotices repeats the settings disclosures for the hooks
// that are actually in place.
//
// The install-time notices are not enough on their own: push.visibility and
// redaction.level are single global settings that can be changed at any time
// after a hook was written. Installing under one and later changing it would
// otherwise leave no disclosure anywhere — the hook file itself names neither
// setting, and the push lines that do are suppressed by the --quiet a hook runs
// with. status is the surface that answers "what does this actually do to me", so
// it has to answer this too.
func renderStatusEnvironmentNotices(cmd *cobra.Command, report githooks.PlanReport) {
	var live []githooks.Event
	for _, plan := range report.Plans {
		// Both kinds upload: a hook Peasant manages, and a file it may not touch
		// that still carries an upload section.
		if plan.Managed() || plan.UploadsFromForeignFile() {
			live = append(live, plan.Event)
		}
	}
	renderSettingsNotices(cmd, live, report.Repository.Root)
	renderRedactionLevelWarning(cmd, live)
}

// renderSettingsNotices reports what the named hooks will actually do with the
// two settings that decide how much of the user's content leaves the machine,
// or that the config could not be read to tell.
//
// Both are disclosed here rather than only on the push path because install is
// the consent moment: it is the last point before publishing starts happening on
// its own. A downgrade the user only learns about from the push output is a
// downgrade they learn about after the first commit has already published at it.
// The wording comes from the same resolvers the push path prints, so the two
// surfaces cannot describe the same setting differently.
func renderSettingsNotices(cmd *cobra.Command, events []githooks.Event, root string) {
	if len(events) == 0 {
		return
	}
	stderr := cmd.ErrOrStderr()
	// The same resolution the hook's own push performs, so what is reported here
	// is what that hook actually does.
	_, cfgErr := loadConfig(resolveConfigPath(cmd))
	if cfgErr != nil {
		fmt.Fprintf(stderr,
			"notice: could not read your config to report the visibility and redaction level these hooks will publish at (%v). "+
				"They run non-interactively, which answers the public-visibility confirmation for you. "+
				"Fix: run 'peasant village hooks status' after fixing the config, and check push.visibility and redaction.level "+
				"before relying on the hooks.\n",
			cfgErr)
		return
	}

}

// renderRedactionLevelWarning reports a configured level that makes the upload a
// hook runs refuse.
//
// It is NOT gated on a hook being present, which is the difference between this
// and the visibility notice. With a hook installed the condition is live: that
// hook prints an error on every commit and publishes nothing. With no hook
// installed it is still the answer status owes, because installing one is refused
// until the level changes - a status that said nothing would send the user
// straight into that refusal.
func renderRedactionLevelWarning(cmd *cobra.Command, events []githooks.Event) {
	cfg, cfgErr := loadConfig(resolveConfigPath(cmd))
	if cfgErr != nil || config.RedactionLevelSupported(cfg.Redaction.Level) {
		return
	}
	consequence := fmt.Sprintf("no %s or %s hook can be installed until this changes",
		githooks.EventPostCommit, githooks.EventPrePush)
	if len(events) > 0 {
		consequence = fmt.Sprintf("the upload %s runs refuses: it prints an error on every run and publishes nothing",
			joinEvents(events))
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"warning: redaction.level is %q, which this version cannot apply, so %s.\n"+
			"Fix: set redaction.level to %s in your config; nothing needs re-installing afterwards.\n",
		cfg.Redaction.Level, consequence, config.RecommendedRedactionLevel)
}

// checkHookRedactionLevel refuses to install a hook that cannot work.
//
// The upload a managed hook runs refuses under an unsupported redaction level, and
// a managed hook fires on every commit - so installing one would hand the user an
// error on every commit forever, which is the failure this whole surface exists to
// avoid. Refusing to create it is better than creating one that always fails.
func checkHookRedactionLevel(cmd *cobra.Command) error {
	cfg, cfgErr := loadConfig(resolveConfigPath(cmd))
	if cfgErr != nil {
		// Unreadable config is a separate, existing failure mode; the notices
		// already report it, and it is not this check's business to reinterpret.
		return nil
	}
	if config.RedactionLevelSupported(cfg.Redaction.Level) {
		return nil
	}
	return &config.UnsupportedRedactionLevelError{
		Level:     cfg.Redaction.Level,
		Source:    configSourceDescription(resolveConfigPath(cmd)),
		Operation: "village hooks install",
		Step:      "before any hook file was created, so that no hook is installed that could only ever fail",
		Impact:    "No hook was installed and no existing hook was changed; a hook installed earlier is untouched and still present.",
	}
}

// joinEvents lists events for a notice that covers all of them at once.
func joinEvents(events []githooks.Event) string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.String())
	}
	return strings.Join(names, ", ")
}

// hookStateLabel renders ownership as the state a user cares about.
func hookStateLabel(ownership githooks.Ownership) string {
	switch ownership {
	case githooks.OwnershipPeasant:
		return "installed"
	case githooks.OwnershipForeign:
		return "unmanaged"
	default:
		return "not installed"
	}
}

// writeIndented prints a multi-line explanation with a stable left margin.
func writeIndented(w io.Writer, text, indent string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
}
