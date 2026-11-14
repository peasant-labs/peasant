package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// storeEntryMode is the class of entry point that is about to open the
// store. It decides what happens when the store is behind the schema this
// build needs. The set is closed.
type storeEntryMode string

const (
	// storeEntryTerminal is a command a person runs at a terminal. A quick
	// upgrade runs with its steps named; a long step or tight disk asks
	// first.
	storeEntryTerminal storeEntryMode = "terminal"
	// storeEntryPiped is the same command with no terminal: a script, a
	// pipe, or CI. A quick upgrade runs with one line per step; a long step
	// is refused with a non-zero exit.
	storeEntryPiped storeEntryMode = "piped"
	// storeEntryHook is a generated git hook: the command that declared its
	// hook flag (declareStoreEntryHookFlag), run with that flag. A quick
	// upgrade runs; a long step is skipped through the nothing-attempted
	// status, which the hook script turns into exit 0 so git is never
	// blocked.
	storeEntryHook storeEntryMode = "hook"
	// storeEntryServer is a server process: the forked web child, a
	// foreground web server, or an import a web request started. It never
	// upgrades.
	storeEntryServer storeEntryMode = "server"
)

// allStoreEntryModes is the closed entry-mode set.
var allStoreEntryModes = []storeEntryMode{storeEntryTerminal, storeEntryPiped, storeEntryHook, storeEntryServer}

// newStoreEntryMode validates a raw entry mode at an input boundary.
func newStoreEntryMode(raw string) (storeEntryMode, error) {
	for _, mode := range allStoreEntryModes {
		if string(mode) == raw {
			return mode, nil
		}
	}
	return "", fmt.Errorf("store entry mode %q is outside the closed set %v; the store upgrade policy cannot be chosen; use one of the listed modes", raw, allStoreEntryModes)
}

func (m storeEntryMode) String() string { return string(m) }

// storeUpgradeAction is what the policy decides for one entry point and one
// plan. The set is closed.
type storeUpgradeAction string

const (
	// storeUpgradeNone: the store is current; nothing happens.
	storeUpgradeNone storeUpgradeAction = "none"
	// storeUpgradeRun: upgrade without asking.
	storeUpgradeRun storeUpgradeAction = "ran"
	// storeUpgradeAsk: show the plan and upgrade only after a yes.
	storeUpgradeAsk storeUpgradeAction = "asked"
	// storeUpgradeSkip: change nothing and report through the
	// nothing-attempted status, so a hook does not block git.
	storeUpgradeSkip storeUpgradeAction = "skipped"
	// storeUpgradeRefuse: change nothing and fail the command.
	storeUpgradeRefuse storeUpgradeAction = "refused"
)

// allStoreUpgradeActions is the closed action set.
var allStoreUpgradeActions = []storeUpgradeAction{storeUpgradeNone, storeUpgradeRun, storeUpgradeAsk, storeUpgradeSkip, storeUpgradeRefuse}

// newStoreUpgradeAction validates a raw action at an input boundary.
func newStoreUpgradeAction(raw string) (storeUpgradeAction, error) {
	for _, action := range allStoreUpgradeActions {
		if string(action) == raw {
			return action, nil
		}
	}
	return "", fmt.Errorf("store upgrade action %q is outside the closed set %v; the expected policy outcome cannot be read; use one of the listed actions", raw, allStoreUpgradeActions)
}

// storeUpgradeDiskTight reports whether free disk is tight for the plan:
// free space is known and below twice what the plan asks for.
func storeUpgradeDiskTight(plan store.UpgradePlan) bool {
	return plan.FreeBytesKnown && plan.RequiredFreeBytes > 0 &&
		plan.FreeBytes < defaults.StoreUpgradeTightDiskMultiple*plan.RequiredFreeBytes
}

// decideStoreUpgrade is the whole entry-point policy. confirmed is an
// explicit --confirm on a command that owns the upgrade: the user already
// consented, so no mode asks or skips. A server never upgrades, confirmed
// or not.
func decideStoreUpgrade(mode storeEntryMode, plan store.UpgradePlan, confirmed bool) storeUpgradeAction {
	if len(plan.Steps) == 0 {
		return storeUpgradeNone
	}
	if mode == storeEntryServer {
		return storeUpgradeRefuse
	}
	if confirmed {
		return storeUpgradeRun
	}
	long := plan.HasLongStep()
	switch mode {
	case storeEntryTerminal:
		if long || storeUpgradeDiskTight(plan) {
			return storeUpgradeAsk
		}
		return storeUpgradeRun
	case storeEntryHook:
		if long {
			return storeUpgradeSkip
		}
		return storeUpgradeRun
	default:
		if long {
			return storeUpgradeRefuse
		}
		return storeUpgradeRun
	}
}

// storeUpgradeRefusal is the actionable error of an entry point that did
// not upgrade a behind store. Nothing was changed when it is returned.
type storeUpgradeRefusal struct {
	Mode    storeEntryMode
	Plan    store.UpgradePlan
	Command string
	// Declined is true when a person answered no at the prompt.
	Declined bool
}

func (e *storeUpgradeRefusal) Error() string {
	plan := e.Plan
	var long []string
	for _, step := range plan.Steps {
		if step.Cost.Long() {
			long = append(long, step.Title)
		}
	}
	why := "the upgrade was not confirmed"
	switch {
	case e.Declined:
		why = "the upgrade was declined at the prompt"
	case e.Mode == storeEntryServer:
		why = "a server process never upgrades the store, because no terminal would show the upgrade"
	case len(long) == 1:
		why = fmt.Sprintf("one step (%s) can take minutes on a large store, and nothing here would show it running", long[0])
	case len(long) > 1:
		why = fmt.Sprintf("%d steps (%s) can take minutes on a large store, and nothing here would show them running", len(long), strings.Join(long, "; "))
	}
	headline := fmt.Sprintf("peasant did not run `%s`: the local store needs an upgrade first.", e.Command)
	when := "before the command opened the store; nothing was changed."
	meaning := "the command did not run; stored sessions and history are unchanged."
	then := "then run the command again."
	if e.Mode == storeEntryHook {
		headline = "peasant did not publish this session: the local store needs an upgrade first."
		when = "at the start of this hook run; nothing was changed."
		meaning = "this push was skipped; your git command is not affected."
		then = "then push again."
	}
	return fmt.Sprintf("%s\n"+
		"what: %s.\n"+
		"why: %s.\n"+
		"where: store upgrade check, before `%s` opened the store.\n"+
		"when: %s\n"+
		"meaning: %s\n"+
		"fix: run `peasant migrate --schema-only` in a terminal, %s",
		headline, storeUpgradeWhat(plan), why, e.Command, when, meaning, then)
}

// storeUpgradeWhat says what the store needs. A store at the head schema can
// still owe the steps an earlier upgrade did not finish.
func storeUpgradeWhat(plan store.UpgradePlan) string {
	if plan.From == plan.To {
		return fmt.Sprintf("the store at %s is at schema %d, the schema this build needs, but an earlier upgrade did not finish %d step(s)", plan.DBPath, plan.From, len(plan.Steps))
	}
	return fmt.Sprintf("the store at %s is at schema %d and this build needs %d", plan.DBPath, plan.From, plan.To)
}

// planStoreUpgrade is the read-only plan the policy runs on. It is a
// variable so a test can set the free disk a plan reports.
var planStoreUpgrade = store.PlanUpgrade

// upgradeStore runs the upgrade the policy allowed. It is a variable so a
// policy test can record that the upgrade started without running one.
var upgradeStore = store.Upgrade

// storeEntryHookFlagAnnotation names the command annotation that declares
// the boolean flag a generated git hook passes to the command. Only a
// command that declares one is ever in hook mode: `village push`, with
// --non-interactive. Another command with a flag of the same name is not.
const storeEntryHookFlagAnnotation = "peasant.store-upgrade.hook-flag"

// declareStoreEntryHookFlag marks cmd as the command generated git hooks
// run, with flag as the boolean flag they pass.
func declareStoreEntryHookFlag(cmd *cobra.Command, flag string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[storeEntryHookFlagAnnotation] = flag
}

// storeEntryModeFor names the entry mode of a command a person or a script
// runs: the command a generated hook runs, with the flag it declared set, is
// a hook; a terminal passes the one terminal check; everything else is
// piped.
func storeEntryModeFor(cmd *cobra.Command) storeEntryMode {
	if cmd != nil {
		if name, declared := cmd.Annotations[storeEntryHookFlagAnnotation]; declared {
			if flag := cmd.Flags().Lookup(name); flag != nil && flag.Value.String() == "true" {
				return storeEntryHook
			}
		}
	}
	if requireTerminal("") == nil {
		return storeEntryTerminal
	}
	return storeEntryPiped
}

// storeCommandName is the command as the user typed it, without the binary
// name, for refusal messages.
func storeCommandName(cmd *cobra.Command) string {
	if cmd == nil {
		return "peasant"
	}
	path := cmd.CommandPath()
	if root := cmd.Root(); root != nil {
		path = strings.TrimPrefix(path, root.Name()+" ")
	}
	return "peasant " + path
}

// ensureStoreCurrent is the one check every entry point runs before it
// opens the store. A current or missing store returns nil at once. A behind
// store is upgraded, asked about, skipped or refused by the entry-point
// policy; on any error nothing was opened, and on a refusal nothing was
// changed.
func ensureStoreCurrent(cmd *cobra.Command, dbPath string, mode storeEntryMode) error {
	return ensureStoreCurrentConfirmed(cmd, dbPath, mode, false)
}

// ensureStoreCurrentConfirmed is ensureStoreCurrent for a command that owns
// the upgrade and takes --confirm.
func ensureStoreCurrentConfirmed(cmd *cobra.Command, dbPath string, mode storeEntryMode, confirmed bool) error {
	plan, err := planStoreUpgrade(dbPath)
	if err != nil {
		return err
	}
	stderr := io.Writer(os.Stderr)
	if cmd != nil {
		stderr = cmd.ErrOrStderr()
	}
	refusal := &storeUpgradeRefusal{Mode: mode, Plan: plan, Command: strings.TrimPrefix(storeCommandName(cmd), "peasant ")}
	switch decideStoreUpgrade(mode, plan, confirmed) {
	case storeUpgradeNone:
		return nil
	case storeUpgradeRefuse:
		return refusal
	case storeUpgradeSkip:
		// A hook reports through the nothing-attempted status: the
		// generated script prints its nothing-published meaning and exits 0.
		return &pushNothingAttemptedError{cause: refusal}
	case storeUpgradeAsk:
		writeStoreUpgradePlan(stderr, plan)
		agreed, err := confirmStoreUpgrade(cmd, stderr)
		if err != nil {
			return err
		}
		if !agreed {
			refusal.Declined = true
			return refusal
		}
	case storeUpgradeRun:
		writeStoreUpgradePlan(stderr, plan)
	}
	return runStoreUpgrade(cmd, dbPath, stderr)
}

// confirmStoreUpgrade asks one yes/no question on the command's input.
func confirmStoreUpgrade(cmd *cobra.Command, stderr io.Writer) (bool, error) {
	in := io.Reader(os.Stdin)
	if cmd != nil {
		in = cmd.InOrStdin()
	}
	fmt.Fprint(stderr, "upgrade the store now? ctrl+c during a step rolls that step back. [y/N]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read the answer to the store upgrade prompt: %w; nothing was changed; run `peasant migrate --schema-only --confirm` to upgrade without a prompt", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// storeUpgradeTightDiskLine is the line the plan prints when free disk is
// tight, in every mode the upgrade runs or asks in.
const storeUpgradeTightDiskLine = "free disk is tight: less than twice what the upgrade asks for"

// writeStoreUpgradePlan prints the plan: every step with its cost, and the
// sizes when a long step or tight disk makes them matter. Every mode that
// runs or asks prints it before the first step, so a pipe and a hook see the
// tight-disk line too.
func writeStoreUpgradePlan(w io.Writer, plan store.UpgradePlan) {
	if plan.From == plan.To {
		fmt.Fprintf(w, "the local store at %s needs an upgrade: schema %d is current, but an earlier upgrade did not finish %d step(s)\n", plan.DBPath, plan.From, len(plan.Steps))
	} else {
		fmt.Fprintf(w, "the local store at %s needs an upgrade: schema %d to %d, %d step(s)\n", plan.DBPath, plan.From, plan.To, len(plan.Steps))
	}
	for i, step := range plan.Steps {
		fmt.Fprintf(w, "  %d. %s (%s)\n", i+1, step.Title, storeUpgradeCostLabel(step.Cost))
	}
	if !plan.HasLongStep() && !storeUpgradeDiskTight(plan) {
		return
	}
	free := "unknown"
	if plan.FreeBytesKnown {
		free = formatMigrateBytes(plan.FreeBytes)
	}
	fmt.Fprintf(w, "store size: %s; free disk: %s; the upgrade asks for %s free\n",
		formatMigrateBytes(plan.StoreBytes), free, formatMigrateBytes(plan.RequiredFreeBytes))
	if storeUpgradeDiskTight(plan) {
		fmt.Fprintln(w, storeUpgradeTightDiskLine)
	}
}

// storeUpgradeCostLabel renders a cost class for a reader.
func storeUpgradeCostLabel(cost store.UpgradeCost) string {
	switch cost {
	case store.UpgradeCostScalesWithRows:
		return "long: scales with the number of stored rows"
	case store.UpgradeCostScalesWithText:
		return "long: scales with the stored text"
	default:
		return "quick"
	}
}

// runStoreUpgrade runs the upgrade with one plain line per step on stderr.
// Ctrl-C cancels the running step, which rolls back.
func runStoreUpgrade(cmd *cobra.Command, dbPath string, stderr io.Writer) error {
	parent := signalParentContext(cmd)
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := time.Now()
	report, err := upgradeStore(ctx, dbPath, store.UpgradeOptions{
		Progress: storeUpgradeLinePrinter(stderr),
		Notices:  stderr,
		Consent:  store.PromptMigrationConsentOnTTY(),
	})
	if err != nil {
		return err
	}
	if len(report.Steps) > 0 {
		fmt.Fprintf(stderr, "store upgrade finished: schema %d to %d, %d step(s) in %s\n",
			report.From, report.To, len(report.Steps), time.Since(started).Round(time.Millisecond))
	}
	return nil
}

// storeUpgradeProgressLineEvery is the least time between two progress
// lines of one running step.
const storeUpgradeProgressLineEvery = 10 * time.Second

// storeUpgradeLinePrinter renders upgrade events as plain lines: one when a
// long step starts, at most one every storeUpgradeProgressLineEvery while it
// runs, and one when any step finishes, with its duration and how much the
// write-ahead log grew.
func storeUpgradeLinePrinter(w io.Writer) func(store.UpgradeEvent) {
	var lastLine time.Time
	return func(event store.UpgradeEvent) {
		prefix := fmt.Sprintf("store upgrade %d/%d: %s", event.Index, event.Total, event.Step.Title)
		switch event.Kind {
		case store.UpgradeEventStepStarted:
			lastLine = time.Now()
			if event.Step.Cost.Long() {
				fmt.Fprintf(w, "%s: started\n", prefix)
			}
		case store.UpgradeEventStepProgress:
			if time.Since(lastLine) < storeUpgradeProgressLineEvery {
				return
			}
			lastLine = time.Now()
			counted := ""
			if event.Unit != store.UpgradeUnitNone && event.Of > 0 {
				counted = fmt.Sprintf(" %d/%d %s,", event.Done, event.Of, event.Unit)
			}
			fmt.Fprintf(w, "%s:%s %s elapsed, %s written to the log\n", prefix, counted, event.Elapsed.Round(time.Second), formatMigrateBytes(event.WALBytes))
		case store.UpgradeEventStepFinished:
			fmt.Fprintf(w, "%s: done in %s, %s written to the log\n", prefix, event.Elapsed.Round(time.Millisecond), formatMigrateBytes(event.WALBytes))
		}
	}
}

// signalParentContext is the command's context, or a background context for
// a command that has none.
func signalParentContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}
