package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
	"github.com/spf13/cobra"
)

const defaultLayoutCaptureLimit = 5

// BuildLayoutCommand returns the layout command wired to the running host.
func BuildLayoutCommand() *cobra.Command {
	return buildLayoutCommand(harnesslayout.HostEnv, harnesslayout.OSSource)
}

func buildLayoutCommand(hostEnv func() (harnesslayout.Env, error), open func(string) harnesslayout.Source) *cobra.Command {
	layoutCmd := &cobra.Command{
		Use:   "layout",
		Short: "Describe and capture the storage layout of tools Peasant does not ingest yet",
		Long: `Describe and capture where coding tools that Peasant does not ingest yet keep
their session metadata and transcripts.

A capture reads the local session store read-only and prints a JSON report of
the structure of each artifact: field paths, JSON types, and record-kind
counts. It never prints transcript text. By default it also removes paths,
session identifiers, titles, and timestamps, so the report can be attached to
a public issue.`,
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the declared tool layouts and their roots on this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := hostEnv()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, layout := range harnesslayout.Layouts() {
				fmt.Fprintf(out, "%s (%s)\n  %s\n", layout.DisplayName, layout.Tool, layout.Sessions)
				for _, root := range env.Resolve(layout) {
					state := "absent"
					if _, err := fs.Stat(open(root).FS, "."); err == nil {
						state = "present"
					}
					fmt.Fprintf(out, "  root %s [%s]\n", root, state)
				}
				for _, artifact := range layout.Artifacts {
					fmt.Fprintf(out, "  %-10s %-8s %s\n", artifact.Role, artifact.Format, artifact.Pattern)
				}
			}
			return nil
		},
	}

	var (
		paths           []string
		limit           int
		includeMetadata bool
	)
	captureCmd := &cobra.Command{
		Use:   "capture <tool>",
		Short: "Print the value-free structure of a tool's local sessions as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tool, err := harnesslayout.NewTool(args[0])
			if err != nil {
				return err
			}
			layout, _ := harnesslayout.Lookup(tool)
			roots := paths
			if len(roots) == 0 {
				env, err := hostEnv()
				if err != nil {
					return err
				}
				roots = env.Resolve(layout)
			}
			report, err := harnesslayout.Run(cmd.Context(), layout, roots, open, limit)
			if err != nil {
				return err
			}
			if !includeMetadata {
				report = report.ShapeOnly()
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(report); err != nil {
				return err
			}
			if len(report.Captures) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "no %s sessions found in: %s\n", layout.DisplayName, strings.Join(roots, string(os.PathListSeparator)))
			}
			return nil
		},
	}
	captureCmd.Flags().StringSliceVar(&paths, "path", nil, "Session store root to read instead of the default roots (repeatable)")
	captureCmd.Flags().IntVar(&limit, "limit", defaultLayoutCaptureLimit, "Maximum sessions to capture; 0 captures every session")
	captureCmd.Flags().BoolVar(&includeMetadata, "include-metadata", false, "Keep paths, session identifiers, titles, and timestamps in the report")

	layoutCmd.AddCommand(listCmd, captureCmd)
	return layoutCmd
}
