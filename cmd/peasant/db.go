//go:build ignore
// +build ignore

// TODO: CLI DB viewer functionality, similar to the `ingest verify` command.
// db.go is excluded from compilation until internal/store/metrics and
// internal/store/query packages are created. buildDBCmd() is dead code
// (never called from main). Guarding with build tag unblocks cmd/peasant.

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/metrics"
	"github.com/peasant-labs/peasant/internal/store/query"
	"github.com/spf13/cobra"
)

// defaultDBPath returns the default SQLite database path.
func defaultDBPath() string {
	return filepath.Join(defaults.State.DirPath.String(), store.DefaultDBFileName)
}

// buildDBCmd constructs the `peasant db` command tree.
func buildDBCmd() *cobra.Command {
	var dbPath string

	cmd := &cobra.Command{
		Use:   "db",
		Short: "Manage the session database",
		Long:  "Initialize, ingest, query, and compute metrics on the SQLite session database.",
	}

	cmd.PersistentFlags().StringVar(&dbPath, "db", defaultDBPath(), "Path to SQLite database")

	// --- peasant db init ---
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize or migrate the database",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := os.MkdirAll(filepath.Dir(dbPath), defaults.PrivateDirPerm); err != nil {
				return fmt.Errorf("creating database directory: %w", err)
			}
			db, err := store.InitDB(dbPath)
			if err != nil {
				return fmt.Errorf("initializing database: %w", err)
			}
			db.Close()

			ver, err := dbVersion(dbPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Database initialized at %s (schema version %d)\n", dbPath, ver)
			return nil
		},
	}

	// --- peasant db ingest ---
	var (
		ingestDirPath  string
		ingestFilePath string
		ingestConflict string
		ingestDryRun   bool
		ingestTranscr  bool
	)
	ingestCmd := &cobra.Command{
		Use:   "ingest",
		Short: "Ingest metadata files into the database",
		Long:  "Parse JSON/JSONL metadata files produced by 'peasant ingest' and store them in the database.",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.InitDB(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer db.Close()

			opts := store.WriteOptions{
				DryRun:         ingestDryRun,
				OnConflict:     ingestConflict,
				WithTranscript: ingestTranscr,
			}

			var result *store.IngestResult
			switch {
			case ingestDirPath != "":
				result, err = store.IngestDir(db, ingestDirPath, opts)
			case ingestFilePath != "":
				result, err = store.IngestFile(db, ingestFilePath, opts)
			default:
				return fmt.Errorf("specify --dir or --file")
			}
			if err != nil {
				return err
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		},
	}
	ingestCmd.Flags().StringVar(&ingestDirPath, "dir", "", "Directory to scan for metadata files")
	ingestCmd.Flags().StringVar(&ingestFilePath, "file", "", "Single metadata file to ingest")
	ingestCmd.Flags().StringVar(&ingestConflict, "on-conflict", "skip", "Conflict strategy: skip or replace")
	ingestCmd.Flags().BoolVar(&ingestDryRun, "dry-run", false, "Show what would be ingested")
	ingestCmd.Flags().BoolVar(&ingestTranscr, "with-transcript", false, "Also index transcript lines")

	// --- peasant db query ---
	queryCmd := &cobra.Command{
		Use:   "query",
		Short: "Query the session database",
	}

	// --- peasant db query list ---
	var (
		queryHarness string
		queryProject string
		querySince   string
		queryUntil   string
		queryLimit   int
	)
	queryListCmd := &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.InitDB(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer db.Close()

			sessions, err := query.ListSessions(db, query.SessionFilter{
				Harness: queryHarness,
				Project: queryProject,
				Since:   querySince,
				Until:   queryUntil,
				Limit:   queryLimit,
			})
			if err != nil {
				return err
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(sessions)
		},
	}
	queryListCmd.Flags().StringVar(&queryHarness, "harness", "", "Filter by harness (claude-code, gemini-cli, codex, opencode)")
	queryListCmd.Flags().StringVar(&queryProject, "project", "", "Filter by project name")
	queryListCmd.Flags().StringVar(&querySince, "since", "", "Filter sessions starting after date (YYYY-MM-DD)")
	queryListCmd.Flags().StringVar(&queryUntil, "until", "", "Filter sessions starting before date (YYYY-MM-DD)")
	queryListCmd.Flags().IntVar(&queryLimit, "limit", 50, "Maximum number of results")

	// --- peasant db query detail ---
	queryDetailCmd := &cobra.Command{
		Use:   "detail [session-id]",
		Short: "Show full details for a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.InitDB(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer db.Close()

			detail, err := query.GetSessionDetail(db, args[0])
			if err != nil {
				return err
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(detail)
		},
	}

	// --- peasant db query stats ---
	var (
		statsHarness string
		statsProject string
	)
	queryStatsCmd := &cobra.Command{
		Use:   "stats",
		Short: "Show aggregate statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.InitDB(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer db.Close()

			stats, err := query.GetStats(db, query.StatsFilter{
				Harness: statsHarness,
				Project: statsProject,
			})
			if err != nil {
				return err
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(stats)
		},
	}
	queryStatsCmd.Flags().StringVar(&statsHarness, "harness", "", "Filter by harness (claude-code, gemini-cli, codex, opencode)")
	queryStatsCmd.Flags().StringVar(&statsProject, "project", "", "Filter by project name")

	// --- peasant db query quality ---
	var (
		qualityProject string
		qualityOutcome string
		qualitySince   string
		qualityUntil   string
		qualityLimit   int
	)
	queryQualityCmd := &cobra.Command{
		Use:   "quality",
		Short: "List sessions with full quality metrics",
		Long:  "Return QualitySession objects with all computed quality fields for the web dashboard.",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.InitDB(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer db.Close()

			sessions, err := query.ListQualitySessions(db, query.QualityFilter{
				Project: qualityProject,
				Outcome: qualityOutcome,
				Since:   qualitySince,
				Until:   qualityUntil,
				Limit:   qualityLimit,
			})
			if err != nil {
				return err
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(sessions)
		},
	}
	queryQualityCmd.Flags().StringVar(&qualityProject, "project", "", "Filter by project name")
	queryQualityCmd.Flags().StringVar(&qualityOutcome, "outcome", "", "Filter by outcome (resolved, partial, failed)")
	queryQualityCmd.Flags().StringVar(&qualitySince, "since", "", "Filter sessions starting after date (YYYY-MM-DD)")
	queryQualityCmd.Flags().StringVar(&qualityUntil, "until", "", "Filter sessions starting before date (YYYY-MM-DD)")
	queryQualityCmd.Flags().IntVar(&qualityLimit, "limit", 50, "Maximum number of results")

	queryCmd.AddCommand(queryListCmd, queryDetailCmd, queryStatsCmd, queryQualityCmd)

	// --- peasant db metrics ---
	metricsCmd := &cobra.Command{
		Use:   "metrics",
		Short: "Compute and view quality metrics",
	}

	// --- peasant db metrics compute ---
	var (
		metricsForce bool
	)
	metricsComputeCmd := &cobra.Command{
		Use:   "compute [session-id...]",
		Short: "Compute metrics for sessions",
		Long:  "Compute quality metrics for specified sessions, or all sessions if none specified.",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.InitDB(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer db.Close()

			engine := metrics.NewEngine(db)
			sessionIDs := args
			if len(sessionIDs) == 0 {
				sessionIDs, err = metrics.AllSessionIDs(db)
				if err != nil {
					return fmt.Errorf("listing sessions: %w", err)
				}
			}

			computed := 0
			for _, sid := range sessionIDs {
				if metricsForce {
					// When --force is used, re-index transcripts to pick up indexer fixes.
					db.Exec(`DELETE FROM transcript_lines WHERE session_id = ?`, sid)
				}
				// Auto-index transcript lines if not already present.
				if err := store.EnsureTranscriptIndexed(db, sid); err != nil {
					slog.Warn("auto-index transcript failed", "session_id", sid, "error", err)
				}

				if err := engine.Compute(sid, metrics.ComputeOptions{Force: metricsForce}); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: metrics for %s: %v\n", sid, err)
					continue
				}
				computed++
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Computed metrics for %d/%d sessions\n", computed, len(sessionIDs))
			return nil
		},
	}
	metricsComputeCmd.Flags().BoolVar(&metricsForce, "force", false, "Recompute even if already cached")

	// --- peasant db metrics show ---
	metricsShowCmd := &cobra.Command{
		Use:   "show [session-id]",
		Short: "Show cached metrics for a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.InitDB(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer db.Close()

			cached, err := metrics.GetCached(db, args[0])
			if err != nil {
				return err
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(cached)
		},
	}

	metricsCmd.AddCommand(metricsComputeCmd, metricsShowCmd)

	cmd.AddCommand(initCmd, ingestCmd, queryCmd, metricsCmd)
	return cmd
}

func dbVersion(path string) (int, error) {
	db, err := store.Open(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return store.CurrentVersion(db)
}
