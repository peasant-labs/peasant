package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// BuildDocgenCommand returns a hidden command that generates CLI reference
// documentation as markdown. Used by the docs-cli target (peasant docgen) to
// produce docs/cli/.
func BuildDocgenCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "docgen [output-dir]",
		Short:  "Generate CLI documentation (markdown)",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "docs/cli"
			if len(args) > 0 {
				dir = args[0]
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create output dir: %w", err)
			}
			configFlag := cmd.Root().PersistentFlags().Lookup("config")
			if configFlag != nil {
				originalDefault := configFlag.DefValue
				originalUsage := configFlag.Usage
				configFlag.DefValue = "~/.config/peasant/config.yaml"
				configFlag.Usage = "Path to config file"
				defer func() {
					configFlag.DefValue = originalDefault
					configFlag.Usage = originalUsage
				}()
			}
			if err := doc.GenMarkdownTree(cmd.Root(), dir); err != nil {
				return fmt.Errorf("generate docs: %w", err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Generated CLI docs in %s\n", dir)
			return nil
		},
	}
}
