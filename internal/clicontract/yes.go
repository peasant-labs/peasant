package clicontract

import "github.com/spf13/cobra"

// YesFlag is the flag that confirms in advance, and YesShorthand its short
// form. Every command that has --yes accepts -y.
const (
	YesFlag      = "yes"
	YesShorthand = "y"
)

// AddYesFlag registers --yes and its -y shorthand on cmd, bound to target.
// usage describes what --yes confirms on this command. Every command
// registers the pair through here, so no command spells it differently.
func AddYesFlag(cmd *cobra.Command, target *bool, usage string) {
	cmd.Flags().BoolVarP(target, YesFlag, YesShorthand, false, usage)
}
