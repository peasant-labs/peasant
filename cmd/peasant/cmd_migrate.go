package main

import (
	"errors"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
)

// errMigrateNotImplemented refuses `peasant migrate` until the migration
// engine lands.
//
// What went wrong: the migration command was invoked, but only its skeleton
// exists so far.
// Why it happened: the harmonized content-model contracts land before their
// implementation (internal/store has no migration engine yet), so the command
// has nothing to run.
// Where it failed: cmd/peasant/cmd_migrate.go (BuildMigrateCommand).
// When it failed: at command startup, before any store was opened.
// What it means: nothing ran and nothing changed; re-running is safe.
// How to fix: wait for the migration implementation (peasant-labs/peasant#568);
// until then, keep running `peasant harvest` and `peasant reclaim` as before.
var errMigrateNotImplemented = errors.New("peasant migrate is not implemented yet: the command skeleton exists but the migration engine has not landed; nothing ran and nothing changed; follow peasant-labs/peasant#568 for the implementation")

// BuildMigrateCommand constructs the `peasant migrate` maintenance command.
//
// The migration converts file-backed native sessions to the harmonized content
// model in place, with a per-session shadow verification that refuses to lose
// or alter a byte, and with resume points at every session and phase boundary
// (the per-session state in the database is the progress record; there is no
// side file). Ctrl-C finishes the current transaction and exits cleanly.
//
// The flags mirror `reclaim`: --dry-run forecasts without writing, --limit
// bounds one pass, --confirm skips the interactive prompt, and --json emits
// machine-readable output.
func BuildMigrateCommand() *cobra.Command {
	var (
		dryRun     bool
		limit      int
		confirm    bool
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Convert file-backed sessions to the harmonized content model",
		Long: "Convert file-backed native sessions to the harmonized content model in place.\n\n" +
			"Phase 0 preflights read-only (--dry-run stops here): counts per phase,\n" +
			"pending intents and superseded generations to discard, sampled\n" +
			"field/blob mismatches, free disk, and an advisory no-other-writer note.\n" +
			"Phase 1 drains pending intents, staged directories, and superseded\n" +
			"generations. Phase 2 converts each file-backed native session under its\n" +
			"exclusive lock, shadow-verifying the new rows against the legacy readers\n" +
			"before any old row is deleted. Phase 3 consolidates the search index in\n" +
			"one transaction. Phase 4 sweeps flagged sessions and reports the\n" +
			"retirement preconditions. Phase 5 (documented, offline) runs optimize\n" +
			"and VACUUM.\n\n" +
			"Run with --dry-run first: it reports the per-phase counts and changes\n" +
			"nothing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errMigrateNotImplemented
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report per-phase counts without converting any session")
	cmd.Flags().IntVar(&limit, "limit", 0, "Convert at most this many sessions with work (0 = all); re-run to continue where this pass stopped")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Skip interactive confirmation (for scripts/CI)")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output results as JSON")

	return cmd
}
