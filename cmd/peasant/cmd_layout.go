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
					_, statErr := fs.Stat(open(root).FS, ".")
					fmt.Fprintf(out, "  root %s [%s]\n", root, harnesslayout.PresenceLabel(statErr))
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
			if limit < 0 {
				return fmt.Errorf("--limit must be zero or greater")
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
				fmt.Fprint(cmd.ErrOrStderr(), emptyCaptureMessage(layout.DisplayName, roots, report))
			}
			return nil
		},
	}
	captureCmd.Flags().StringSliceVar(&paths, "path", nil, "Session store root to read instead of the default roots (repeatable)")
	captureCmd.Flags().IntVar(&limit, "limit", defaultLayoutCaptureLimit, "Maximum successful captures; 0 captures every session; a negative value is rejected")
	captureCmd.Flags().BoolVar(&includeMetadata, "include-metadata", false, "Keep paths, session identifiers, titles, and timestamps in the report")

	layoutCmd.AddCommand(listCmd, captureCmd)
	return layoutCmd
}

// emptyCaptureMessage distinguishes a store with nothing to capture from one
// whose sessions were found and then every capture failed.
func emptyCaptureMessage(display string, roots []string, report harnesslayout.Report) string {
	discovered := 0
	for _, root := range report.Roots {
		discovered += root.Sessions
	}
	where := strings.Join(roots, string(os.PathListSeparator))
	if discovered == 0 && len(report.Failures) == 0 {
		return fmt.Sprintf("no %s sessions found in: %s\n", display, where)
	}
	noun := "sessions"
	if discovered == 1 {
		noun = "session"
	}
	return fmt.Sprintf("found %d %s %s in: %s; every capture failed\n", discovered, display, noun, where)
}
