package main

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/spf13/cobra"
)

// BuildVillageAnnotationsCommand constructs the `peasant village annotations`
// command group. Today it carries one subcommand, `sync`, which refreshes the
// FOREIGN annotations other village users authored on the requester's OWN pushed
// transcripts. It REQUIRES login (it contacts the village). Naming is LOCKED at
// The command name is part of the public CLI.
func BuildVillageAnnotationsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "annotations",
		Short: "Sync foreign annotations on your pushed transcripts",
		Long: "Refresh the annotations other village users authored on the transcripts you " +
			"pushed. Requires login (run 'peasant village login').",
	}

	cmd.AddCommand(buildVillageAnnotationsSyncCommand())

	return cmd
}

// buildVillageAnnotationsSyncCommand constructs `village annotations sync
// [--session <local-id>]` (L2). Login is REQUIRED. NegotiatePull runs exactly
// once per command inside the pipeline's RefreshOwnAnnotations op. --session
// narrows the refresh to a single OWN pushed transcript by its local session ID;
// omitted ⇒ all own pushed transcripts. Output conventions mirror push: the
// summary goes to stdout, --json is parseable, and the exit code keys off the
// typed PullStatus.
func buildVillageAnnotationsSyncCommand() *cobra.Command {
	var (
		sessionID  string
		jsonOutput bool
		verbose    bool
		quiet      bool
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Refresh foreign annotations on your pushed transcripts (login required)",
		Long: `Refresh the foreign annotations (authored by other village users) on the
transcripts you have pushed. Your own authored annotations are excluded. Pulled
annotation data is foreign and one-way: it never re-enters your annotate-push
candidate set.

Without --session, all your pushed transcripts are scanned. With --session
<local-id>, only the single pushed transcript correlated to that local session
ID is refreshed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Past argument validation, every failure here is a RUNTIME error (auth
			// gate, unreachable village, refresh pipeline) — not a usage error.
			// Silence cobra's Usage/Flags dump so the actionable one-liner (e.g.
			// "not logged in — run 'peasant village login' first") stands alone;
			// genuine flag misuse is still reported with usage before RunE.
			cmd.SilenceUsage = true

			level, levelErr := resolveOutputLevel(quiet, verbose)
			if levelErr != nil {
				return levelErr
			}

			creds, err := requireVillageCredentials(cmd)
			if err != nil {
				return err
			}

			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			pipeline := villagePullPipeline(cmd, creds, db)

			result, syncErr := pipeline.RefreshOwnAnnotations(cmd.Context(), pull.RefreshOptions{
				SessionID: sessionID,
			})

			if jsonOutput {
				if jErr := printSyncJSON(cmd, result, syncErr); jErr != nil {
					return jErr
				}
				return syncErr
			}

			if result != nil {
				printSyncSummary(cmd, result, level)
			}
			// The pipeline error is already actionable (incl. the not-logged-in
			// case naming `peasant village login`).
			return syncErr
		},
	}

	cmd.Flags().StringVar(&sessionID, "session", "", "Refresh only the pushed transcript correlated to this local session ID (default: all)")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of human-readable")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Show extra detail (village host, transcripts scanned, excluded own-authored count)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Suppress the summary; print only errors and a final result line")

	return cmd
}

// jsonSyncResult is the JSON-safe output for `village annotations sync --json`.
type jsonSyncResult struct {
	Status             string `json:"status"`
	VillageHost        string `json:"villageHost,omitempty"`
	TranscriptsScanned int    `json:"transcriptsScanned"`
	Created            int    `json:"created"`
	Updated            int    `json:"updated"`
	Skipped            int    `json:"skipped"`
	Excluded           int    `json:"excluded"`
	Error              string `json:"error,omitempty"`
}

// printSyncJSON writes the refresh result as JSON (status = typed PullStatus).
func printSyncJSON(cmd *cobra.Command, result *pull.RefreshResult, syncErr error) error {
	out := jsonSyncResult{Status: pull.PullStatusError.String()}
	if result != nil {
		out.Status = result.Status.String()
		out.VillageHost = result.VillageHost
		out.TranscriptsScanned = result.TranscriptsScanned
		out.Created = result.Created
		out.Updated = result.Updated
		out.Skipped = result.Skipped
		out.Excluded = result.Excluded
	}
	if syncErr != nil {
		out.Error = syncErr.Error()
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// printSyncSummary writes the human-readable refresh summary to stdout, routed by
// verbosity. The final result line is kept even under --quiet.
func printSyncSummary(cmd *cobra.Command, result *pull.RefreshResult, level outputLevel) {
	w := cmd.OutOrStdout()

	switch result.Status {
	case pull.PullStatusPulled, pull.PullStatusUpToDate:
		fmt.Fprintf(w, "Annotations: %d created, %d updated, %d skipped\n",
			result.Created, result.Updated, result.Skipped)
	default:
		fmt.Fprintf(w, "Annotations sync: %s\n", result.Status)
	}

	if level == outputVerbose {
		if result.VillageHost != "" {
			fmt.Fprintf(w, "  village:            %s\n", result.VillageHost)
		}
		fmt.Fprintf(w, "  transcripts scanned: %d\n", result.TranscriptsScanned)
		fmt.Fprintf(w, "  own-authored excluded: %d\n", result.Excluded)
	}
}
