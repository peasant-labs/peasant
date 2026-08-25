//go:build origin_audit

// Command peasant-origin-audit is a manual, opt-in measuring instrument. It
// runs the production session-origin classification rule
// (sessionorigin.Classify, via the real Claude Code discovery/mining path)
// over one operator's own harness transcripts and reports how many landed in
// each deciding signal. It is the evidence for acceptance gate G1: whether the
// rule, run over a real transcript history, produces the distribution the
// implementation plan predicted.
//
// It is READ ONLY. It writes no file, no cache record, and no database row --
// see the doc comment on RunAudit for exactly why that is true of the
// discovery call this command makes, not merely asserted.
//
// The build tag keeps this command, and its tests, out of `go build ./...`,
// `go test ./...`, and the shipped `peasant` binary, on the same footing as
// cmd/peasant-guided-screenshots.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// auditTimeout bounds one pass over a harness directory. A real store's
// transcript history is large but bounded; a run that has not finished in
// this window is almost certainly stuck on an unexpected filesystem shape
// (an unmounted network path, a symlink cycle the walk keeps re-entering)
// rather than doing useful work.
const auditTimeout = 10 * time.Minute

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("peasant-origin-audit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sourceFlag := flags.String(
		"source-path",
		defaults.DefaultClaudePath.String(),
		"Claude Code harness directory to read (default: the operator's own ~/.claude/projects)",
	)
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "peasant origin audit: unexpected arguments %q; fix: this command takes no positional arguments, only --source-path\n", flags.Args())
		return 2
	}

	sourcePath, err := ingest.NewResolvedPath(*sourceFlag)
	if err != nil {
		printActionable(stderr, "resolve the Claude Code source path", err,
			fmt.Sprintf("pass an absolute or ~-relative --source-path (got %q)", *sourceFlag))
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()

	report, err := RunAudit(ctx, &ingest.OSFileSystem{}, sourcePath)
	if err != nil {
		printActionable(stderr, "run the read-only session-origin audit", err,
			"confirm the source path exists and is a readable Claude Code projects directory, then rerun")
		return 1
	}

	printReport(stdout, report)
	return 0
}

func printActionable(writer io.Writer, what string, cause error, fix string) {
	fmt.Fprintf(
		writer,
		"peasant origin audit failed\nwhat: %s.\nwhy: %v.\nwhere: cmd/peasant-origin-audit.\nwhen: measuring the production session-origin classifier against real harness transcripts.\nmeans: the acceptance-gate measurement in G1 could not be produced.\nfix: %s.\n",
		what, cause, fix,
	)
}
