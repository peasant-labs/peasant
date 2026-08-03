package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// sessionReader abstracts session lookup for testability.
// Defined at the consumer (CLI) rather than the provider (store) per Go idiom —
// interfaces belong to the package that uses them, not the package that implements them.
// *store.Store satisfies this interface.
type sessionReader interface {
	SessionByID(ctx context.Context, sessionID string) (*store.SessionRow, error)
}

// Compile-time guard: *store.Store must satisfy sessionReader.
var _ sessionReader = (*store.Store)(nil)

// BuildMetricsCommand constructs the metrics command with its subcommands.
func BuildMetricsCommand() *cobra.Command {
	metricsCmd := &cobra.Command{
		Use:   "metrics",
		Short: "Manage session metrics",
	}

	var (
		sessionFilter string
		force         bool
		verbose       bool
		dryRun        bool
	)

	computeCmd := &cobra.Command{
		Use:   "compute",
		Short: "Compute metrics for ingested sessions",
		Long:  "Compute per-session metrics and daily summary insights from indexed session entries.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Open store.
			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			// Determine which sessions to compute.
			// Use AllSessionIDs (no metrics JOIN) so sessions without session_metrics
			// rows are included — that's exactly what needs computing.
			var sessionIDs []ingest.SessionID
			var validatedSID ingest.SessionID // set when sessionFilter is provided
			if sessionFilter != "" {
				sid, sidErr := ingest.NewSessionID(sessionFilter)
				if sidErr != nil {
					return fmt.Errorf("invalid session ID %q: %w", sessionFilter, sidErr)
				}
				validatedSID = sid
				sessionIDs = []ingest.SessionID{sid}
			} else {
				rawIDs, err := db.AllSessionIDs(ctx)
				if err != nil {
					return fmt.Errorf("list sessions: %w", err)
				}
				for _, raw := range rawIDs {
					sid, err := ingest.NewSessionID(raw)
					if err != nil {
						slog.Warn("metrics: invalid session ID from store", "session_id", raw, "error", err)
						continue
					}
					sessionIDs = append(sessionIDs, sid)
				}
			}

			if len(sessionIDs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No sessions found.")
				return nil
			}

			// Dry-run: show what would be computed without writing.
			if dryRun {
				wouldCompute := 0
				for _, sid := range sessionIDs {
					exists, err := db.SessionEntriesExist(ctx, sid)
					if err != nil || !exists {
						continue
					}
					if !force {
						metricsExist, err := db.MetricsExist(ctx, sid, metrics.CurrentComputeVersion)
						if err == nil && metricsExist {
							continue
						}
					}
					wouldCompute++
					if verbose {
						fmt.Fprintf(cmd.OutOrStdout(), "  COMPUTE  %s\n", sid)
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "peasant metrics compute: %d session(s) would be computed (dry-run)\n", wouldCompute)
				return nil
			}

			// Create engine and compute (with models support for cost/context).
			engine := metrics.NewEngineWithModels(db, db)
			if force {
				engine.SetForce(true)
			}

			computed, err := engine.ComputeMetrics(ctx, sessionIDs)
			if err != nil {
				return fmt.Errorf("compute metrics: %w", err)
			}

			// Compute insights (daily summaries) for affected days.
			// Query AllSessions *after* ComputeMetrics so session_metrics rows exist
			// for the INNER JOIN. For single-session, use SessionByID.
			daySet := make(map[string]bool)
			if sessionFilter != "" {
				row, err := db.SessionByID(ctx, string(validatedSID))
				if err != nil {
					return fmt.Errorf("lookup session %q: %w", sessionFilter, err)
				}
				if row == nil {
					slog.Warn("metrics compute: session not found or has no metrics row; insights skipped", "session_id", sessionFilter)
				} else if row.StartMs > 0 {
					day := time.Unix(row.StartMs/1000, 0).UTC().Format("2006-01-02")
					daySet[day] = true
				}
			} else {
				rows, err := db.AllSessions(ctx)
				if err != nil {
					slog.Warn("metrics compute: could not load sessions for insights", "error", err)
				}
				for _, row := range rows {
					if row.StartMs > 0 {
						day := time.Unix(row.StartMs/1000, 0).UTC().Format("2006-01-02")
						daySet[day] = true
					}
				}
			}
			if len(daySet) > 0 {
				days := make([]string, 0, len(daySet))
				for d := range daySet {
					days = append(days, d)
				}
				if err := engine.ComputeInsights(ctx, days); err != nil {
					fmt.Fprintf(os.Stderr, "warning: compute insights: %v\n", err)
				}
			}

			fmt.Fprintf(cmd.OutOrStdout(), "peasant metrics compute: %d session(s) computed\n", computed)
			return nil
		},
	}

	computeCmd.Flags().StringVar(&sessionFilter, "session", "", "Compute metrics for a single session ID")
	computeCmd.Flags().BoolVar(&force, "force", false, "Recompute all sessions (ignore compute version)")
	computeCmd.Flags().BoolVar(&verbose, "verbose", false, "Show per-session detail")
	computeCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be computed without writing")

	metricsCmd.AddCommand(computeCmd)
	return metricsCmd
}
