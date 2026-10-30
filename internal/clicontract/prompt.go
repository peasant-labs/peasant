package clicontract

import (
	"fmt"

	"github.com/spf13/cobra"
)

// ConfirmDecision is what a command does where it would ask for confirmation.
// It is a closed set.
type ConfirmDecision string

const (
	// ConfirmAsk shows the prompt and reads the answer.
	ConfirmAsk ConfirmDecision = "ask"
	// ConfirmAssumedYes proceeds without asking because --yes was given.
	ConfirmAssumedYes ConfirmDecision = "assumed_yes"
	// ConfirmSkippedDryRun neither asks nor writes: a dry run never prompts.
	ConfirmSkippedDryRun ConfirmDecision = "skipped_dry_run"
	// ConfirmRefusedNoTTY refuses: no one can answer (no terminal), or an
	// answer would corrupt the --json document on stdout. The caller returns
	// ConfirmationRequired.
	ConfirmRefusedNoTTY ConfirmDecision = "refused_no_tty"
)

// AllConfirmDecisions is the closed set of confirmation decisions.
var AllConfirmDecisions = []ConfirmDecision{
	ConfirmAsk,
	ConfirmAssumedYes,
	ConfirmSkippedDryRun,
	ConfirmRefusedNoTTY,
}

// PromptPolicy is the input to the one rule that decides whether a command
// may prompt.
type PromptPolicy struct {
	DryRun    bool
	JSON      bool
	AssumeYes bool
	IsTTY     bool
}

// Decide applies the rule, checked in this order: a dry run never prompts;
// --yes proceeds; --json or no terminal refuses; otherwise ask. Under --json a
// prompt would corrupt stdout, so it is refused, never shown.
func (p PromptPolicy) Decide() ConfirmDecision {
	switch {
	case p.DryRun:
		return ConfirmSkippedDryRun
	case p.AssumeYes:
		return ConfirmAssumedYes
	case p.JSON || !p.IsTTY:
		return ConfirmRefusedNoTTY
	default:
		return ConfirmAsk
	}
}

// ConfirmationRequired is the usage error a command returns for
// ConfirmRefusedNoTTY. action says what needed confirming, for example
// "replacing the installed binary".
func ConfirmationRequired(cmd *cobra.Command, action string) *UsageError {
	return NewUsageError(fmt.Sprintf(
		"%s: %s needs confirmation, and no prompt can be shown\n"+
			"What went wrong: there is no terminal to answer a prompt, or --json is set and a prompt would corrupt its output.\n"+
			"Why: the command never acts without an explicit confirmation; nothing was changed.\n"+
			"Fix: re-run with --yes (-y) to confirm, or --dry-run to see what would happen.",
		cmd.CommandPath(), action))
}
