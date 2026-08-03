package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dayvidpham/bestiary"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/spf13/cobra"
)

// ModelFetcher abstracts the live models.dev fetch surface used by `peasant
// models sync`. It exposes exactly the two bestiary client methods the command
// needs so tests can inject an httptest-backed client (or a stub) instead of
// reaching the network. The production *bestiary.Client satisfies it.
type ModelFetcher interface {
	// FetchModels retrieves all models. On unreachable API / non-2xx / decode
	// failure it returns *bestiary.ErrAPIUnavailable; on context cancellation or
	// deadline it returns the raw ctx.Err().
	FetchModels(ctx context.Context) ([]bestiary.ModelInfo, error)
	// FetchModelsByProvider retrieves only the given provider's models, with the
	// same error contract as FetchModels.
	FetchModelsByProvider(ctx context.Context, p bestiary.Provider) ([]bestiary.ModelInfo, error)
}

// Compile-time guard: the real bestiary client must satisfy ModelFetcher.
var _ ModelFetcher = (*bestiary.Client)(nil)

// defaultModelFetcher returns the production fetcher — a live models.dev client.
func defaultModelFetcher() ModelFetcher {
	return bestiary.NewClient()
}

// BuildModelsCommand constructs the models command wired to the live models.dev
// client. It MUST remain a func() *cobra.Command value: it is consumed by the
// static command registry (main.go) and the fixture test registry
// (cmd_fixture_test.go). The injectable wiring lives in buildModelsCommand.
func BuildModelsCommand() *cobra.Command {
	return buildModelsCommand(defaultModelFetcher())
}

// buildModelsCommand builds the models command tree against an injected
// ModelFetcher, allowing integration tests to drive the real RunE path with an
// httptest-backed bestiary client.
func buildModelsCommand(fetcher ModelFetcher) *cobra.Command {
	modelsCmd := &cobra.Command{
		Use:   "models",
		Short: "Manage model reference data",
	}

	var (
		providerFilter string
		dryRun         bool
	)

	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Fetch model data from models.dev and sync to local store",
		Long:  "Fetches model pricing and capability data from models.dev API and upserts into the local models table.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Show what will be synced and ask for confirmation.
			sourceInfo := "models.dev API (https://models.dev)"
			if providerFilter != "" {
				sourceInfo = fmt.Sprintf("models.dev API (https://models.dev), filtered by provider=%q", providerFilter)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "This will sync model data.\n")
			fmt.Fprintf(cmd.OutOrStdout(), "Source: %s\n", sourceInfo)
			fmt.Fprintf(cmd.OutOrStdout(), "Purpose: Update local model reference data with pricing and capabilities\n")
			fmt.Fprintf(cmd.OutOrStdout(), "\nContinue? [y/N] ")

			var response string
			if _, err := fmt.Fscan(cmd.InOrStdin(), &response); err != nil {
				return fmt.Errorf("read confirmation: %w", err)
			}
			response = strings.ToLower(strings.TrimSpace(response))
			if response != "y" && response != "yes" {
				fmt.Fprintln(cmd.OutOrStdout(), "Sync cancelled.")
				return nil
			}

			// 1. Validate the provider filter BEFORE any fetch or fallback. This
			//    guard fires regardless of network state, so `--provider=bogus`
			//    hard-errors even offline (C2) instead of silently syncing 0 rows.
			hasFilter := providerFilter != ""
			var p bestiary.Provider
			if hasFilter {
				p = bestiary.Provider(providerFilter) // validated by the provider filter parser above
				if !p.IsKnown() {
					return fmt.Errorf("unknown provider %q (valid: %v)", providerFilter, bestiary.Providers())
				}
			}

			// 2. Live fetch from models.dev.
			bs, err := fetchModels(ctx, fetcher, p, hasFilter)
			if err != nil {
				// Fall back to bestiary's built-in static snapshot ONLY when the
				// API is unavailable (unreachable / non-2xx / decode failure —
				// wrapped in *ErrAPIUnavailable). A canceled context or exceeded
				// deadline is a deliberate abort: propagate it non-zero so a user
				// Ctrl-C is never masked as a successful sync of stale data.
				var apiErr *bestiary.ErrAPIUnavailable
				if !errors.As(err, &apiErr) {
					return fmt.Errorf("fetch models: %w", err)
				}

				// Snapshot vintage, derived at runtime (never hardcoded — the local
				// checkout and the pinned bestiary version carry different stamps).
				snapshotTaken := "unknown"
				if all := bestiary.StaticModels(); len(all) > 0 {
					snapshotTaken = all[0].LastSynced
				}
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: models.dev fetch failed (%v);\n"+
						"  syncing bestiary's built-in static snapshot taken %s (may be behind models.dev).\n"+
						"  re-run when online for live data.\n", err, snapshotTaken)

				if hasFilter {
					bs = bestiary.ModelsByProvider(p)
				} else {
					bs = bestiary.StaticModels()
				}
			}

			// Live records carry an empty LastSynced and are stamped now; static
			// fallback records preserve bestiary's codegen vintage (see the
			// preserve-if-present rule in ingest.ModelFromBestiary).
			models := ingest.ModelsFromBestiary(bs, time.Now().UTC())

			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "peasant models sync: %d model(s) would be synced (dry-run)\n", len(models))
				return nil
			}

			// Open store and sync.
			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			if err := db.SyncModels(ctx, models); err != nil {
				return fmt.Errorf("sync models: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "peasant models sync: %d model(s) synced\n", len(models))
			return nil
		},
	}

	syncCmd.Flags().StringVar(&providerFilter, "provider", "", "Only sync models from this provider")
	syncCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show count without writing")

	modelsCmd.AddCommand(syncCmd)
	return modelsCmd
}

// fetchModels dispatches to the provider-filtered or all-models fetch depending
// on whether a (already-validated) provider filter is set.
func fetchModels(ctx context.Context, fetcher ModelFetcher, p bestiary.Provider, hasFilter bool) ([]bestiary.ModelInfo, error) {
	if hasFilter {
		return fetcher.FetchModelsByProvider(ctx, p)
	}
	return fetcher.FetchModels(ctx)
}
