package main

import (
	"github.com/spf13/cobra"
)

// BuildVillageCommand constructs the village command group with push, login,
// and logout subcommands for interacting with the remote Peasant village.
func BuildVillageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "village",
		Short: "Interact with the Peasant village",
		Long:  "Commands for pushing transcripts to, and authenticating with, the Peasant village.",
	}

	// push, login, and logout stay directly under `village` deliberately. They are not grouped under a
	// `transcripts` subcommand the way pull/list/context are: a `village
	// transcripts push` alias is a possible future addition, but the asymmetry here
	// is chosen, not an oversight.
	cmd.AddCommand(BuildPushCommand())
	cmd.AddCommand(BuildLoginCommand())
	cmd.AddCommand(BuildLogoutCommand())

	// transcripts {pull, list, context} and annotations {sync} are the pull-side
	// surface.
	cmd.AddCommand(BuildVillageTranscriptsCommand())
	cmd.AddCommand(BuildVillageAnnotationsCommand())

	// hooks {install, status, uninstall} manages the opt-in per-repository git
	// hooks that run the push above. It lives under `village` because the only
	// thing a managed hook does is a village upload.
	cmd.AddCommand(BuildVillageHooksCommand())

	return cmd
}
