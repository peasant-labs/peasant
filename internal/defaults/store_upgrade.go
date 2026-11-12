package defaults

import "time"

// The store upgrade's free-disk factors. A factor is the share of the
// database file's size a step asks to be free before it may run. Every step
// in the upgrade step catalog names one of these; the preflight asks for the
// largest factor among the steps planned for a store, times the database
// size, because each step commits and its log space is reused before the
// next one starts. Tests assert the invariant (refuse when free is below
// required) and the catalog's shape, never a factor's exact value.
//
// Sandbox measurement, 2026-11-12, store code at revision 323abcf9b,
// synthetic schema-69 store with 268,477,563 bytes of text held in the entry
// rows (485,289,984-byte database):
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> PEASANT_SANDBOX_UPGRADE_TEXT_MB=256 \
//	  go test ./internal/store -run TestSandboxStoreUpgradeMeasure -v -timeout 0
//
//	step 70   57ms      444,992 log bytes (0.0009 of the database)
//	step 71    1ms       53,560           (0.0001)
//	step 72   13ms       20,600           (0.00004)
//	step 73   20ms      873,440           (0.0018)
//	step 74  491ms      181,280           (0.0004)
//	step 75  1m43s  654,091,232 high-water (1.348), database grew by 165,154,816 (0.340)
//	step 76  310ms            0
const (
	// StoreUpgradeEntryTableRebuildDiskFactor is for a step that copies the
	// entry table and rebuilds the search index over it. Measured on step 75:
	// the write-ahead log peaked at 1.348 times the database and the
	// database grew by 0.340 times, 1.688 together, rounded up. The measured
	// store kept all its text in the entry rows, the worst layout for this
	// step. A store that keeps its text in the body table needs far less
	// (the 43 GB store measured on 2026-11-11 left a log of 0.10 times the
	// database), but the plan cannot tell the two layouts apart without
	// reading the entry table, which on the heavy layout is the whole store.
	// So the plan uses the conservative value for every store.
	StoreUpgradeEntryTableRebuildDiskFactor = 1.69

	// StoreUpgradeSearchRebuildDiskFactor is for a step that builds or
	// rebuilds a search index without copying a table. Not measured in the
	// sandbox run. Derivation: a fresh build of the search index measured
	// about 0.39 times the indexed text bytes (the store's own search
	// estimate uses the same figure), and the database size is an upper
	// bound on the text it holds.
	StoreUpgradeSearchRebuildDiskFactor = 0.39

	// StoreUpgradeRowsStepDiskFactor is for a step whose cost scales with
	// the row count and that never rewrites stored text: a table rebuild of
	// row data, or a per-session data fill. The largest of the five measured
	// steps (70 to 74) wrote 0.0018 times the database and the five together
	// 0.0032; 0.01 is about five times the largest and three times the sum.
	StoreUpgradeRowsStepDiskFactor = 0.01

	// StoreUpgradeQuickStepDiskFactor is for a step that changes the catalog
	// or a few rows. The measured quick step (76 on a store with no stale
	// search results) wrote nothing to the log. The factor is small and not
	// zero, so every planned step states a need and a full disk is still
	// refused before the first step: 0.001 is one megabyte per gigabyte.
	StoreUpgradeQuickStepDiskFactor = 0.001

	// StoreUpgradeTightDiskMultiple defines "tight": free space is known and
	// below this multiple of the required bytes. A terminal upgrade asks
	// before it runs when disk is tight.
	StoreUpgradeTightDiskMultiple = 2

	// StoreUpgradeProgressInterval is how often a running step reports its
	// elapsed time and write-ahead-log growth between its own counts.
	StoreUpgradeProgressInterval = 2 * time.Second

	// StoreUpgradeLockPoll is how long one attempt to take the data
	// directory's run lock waits before the upgrade checks its context again.
	StoreUpgradeLockPoll = 250 * time.Millisecond
)
