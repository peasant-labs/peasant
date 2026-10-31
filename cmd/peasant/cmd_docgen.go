package main

import (
	"fmt"
	"os"
	"path/filepath"

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
			if err := appendContractLink(filepath.Join(dir, cmd.Root().Name()+".md")); err != nil {
				return fmt.Errorf("generate docs: %w", err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Generated CLI docs in %s\n", dir)
			return nil
		},
	}
}

// cliContractSection closes the root reference page with a link to the
// contract every command shares with scripts and agents. The generated pages
// sit in docs/cli/, so the link is relative to that directory.
const cliContractSection = `
### Scripts and agents

[The CLI contract](../cli-contract.md) describes what every command promises a
program that drives it: one JSON document on stdout with --json, warnings
inside that document, --sessions-from, prompts, and exit statuses.
`

// appendContractLink adds cliContractSection to the generated root page.
func appendContractLink(rootPage string) error {
	f, err := os.OpenFile(rootPage, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(cliContractSection); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
