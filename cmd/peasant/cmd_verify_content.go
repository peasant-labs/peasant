package main

import (
	"fmt"
	"os"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// runVerifyContent checks the harmonized session content objects and the
// search index. Without --repair it is read-only: it lists the damaged
// sessions and reports the index health with its size ratio, changing
// nothing. With --repair it clears the consumed-input proof of each
// damaged session, so the repair predicate selects it and its next
// activation runs in repair mode, which rewrites the failing objects;
// healing still needs that harvest to run.
func runVerifyContent(cmd *cobra.Command, repair bool) error {
	cfgPath := resolveConfigPath(cmd)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ownedRoot, err := ingest.NewResolvedPath(cfg.Output.BasePath)
	if err != nil {
		return fmt.Errorf("resolve the owned-artifact root: %w", err)
	}

	dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	db, closeStore, err := openVerifyContentStore(cmd, repair, string(ownedRoot), dbPath)
	if err != nil {
		return err
	}
	defer closeStore()

	report, err := db.VerifyContent(cmd.Context(), repair)
	if err != nil {
		return fmt.Errorf("verify content: %w", err)
	}
	return writeVerifyContentReport(cmd, report, repair)
}

// openVerifyContentStore opens the analytics store for content
// verification: read-only without --repair (nothing can change), and
// read-write with --repair (the re-index marks need a writer).
func openVerifyContentStore(cmd *cobra.Command, repair bool, ownedRoot, dbPath string) (*store.Store, func(), error) {
	if !repair {
		options, err := generationStoreOptionsReadOnly(ownedRoot)
		if err != nil {
			return nil, func() {}, err
		}
		db, err := store.OpenReadOnlyWithOptions(dbPath, options...)
		if err != nil {
			return nil, func() {}, err
		}
		return db, func() { _ = db.Close() }, nil
	}
	dataDir := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		return nil, func() {}, fmt.Errorf("create data directory: %w", err)
	}
	options, err := generationStoreOptions(ownedRoot)
	if err != nil {
		return nil, func() {}, err
	}
	options = append(options, store.WithMigrationConsent(store.PromptMigrationConsentOnTTY()))
	db, err := store.Open(dbPath, options...)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open analytics store: %w", err)
	}
	return db, func() { _ = db.Close() }, nil
}

// writeVerifyContentReport prints the content verification report. A
// non-empty damage list fails the command, so scripts and CI can gate on
// it; the fix for each session is the repair activation on its next
// harvest.
func writeVerifyContentReport(cmd *cobra.Command, report store.ContentVerifyReport, repair bool) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Content verification")
	fmt.Fprintf(out, "sessions checked: %d\n", report.SessionsChecked)
	if len(report.Damaged) == 0 {
		fmt.Fprintln(out, "damaged sessions: none")
	} else {
		fmt.Fprintf(out, "damaged sessions: %d\n", len(report.Damaged))
		for _, damage := range report.Damaged {
			fmt.Fprintf(out, "  - %s %s: %s\n", damage.SessionID, damage.Object, damage.Reason)
		}
	}
	if report.NeedsRebuild {
		fmt.Fprintln(out, "search index: NEEDS REBUILD (search refuses until it rebuilds)")
	} else {
		fmt.Fprintln(out, "search index: healthy")
	}
	size := report.Size
	optimize := "ok"
	if size.NeedsOptimize {
		optimize = "needs-optimize: run the offline optimize + VACUUM"
	}
	fmt.Fprintf(out, "search size: %d bytes over %d bytes of text (%d documents), fresh estimate %.0f bytes, ratio %.2f (%s)\n",
		size.IndexBytes, size.TextBytes, size.LiveDocs, size.FreshEstimateBytes, size.Ratio, optimize)
	if repair {
		if len(report.Repaired) == 0 {
			fmt.Fprintln(out, "repair: nothing to mark")
		} else {
			fmt.Fprintf(out, "repair: marked %d session(s) for the repair activation; re-run harvest to heal them\n", len(report.Repaired))
		}
		if report.IndexRebuilt {
			fmt.Fprintln(out, "repair: search index rebuilt")
		}
	} else if len(report.Damaged) > 0 {
		fmt.Fprintln(out, "re-run with --repair to mark the damaged sessions for the repair activation")
	}
	if len(report.Damaged) > 0 {
		return fmt.Errorf("content verification failed: %d damaged session(s); re-run with --repair and harvest to heal them", len(report.Damaged))
	}
	fmt.Fprintln(out, "Status: PASSED")
	return nil
}
