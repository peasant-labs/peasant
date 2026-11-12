package defaults

import "time"

// The store upgrade's free-disk factors. A factor is the share of a basis
// a step asks to be free before it may run. The basis is named by the
// step's catalog entry: the whole database file, or the tables the step
// rewrites, measured on the store. Every step in the upgrade step catalog
// names one factor and one basis; the preflight asks for the largest need
// (factor times basis) among the steps planned for a store, because each
// step commits and its log space is reused before the next one starts.
// Tests assert the invariant (refuse when free is below required), the
// catalog's shape, and the need against dbstat on synthetic stores; never a
// factor's exact value.
//
// Sandbox measurement of the light steps, 2026-11-12, store code at
// revision 323abcf9b, synthetic schema-69 store with 268,477,563 bytes of
// text held in the entry rows (485,289,984-byte database):
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
//
// Sandbox measurement of step 75 by layout, 2026-11-12, two synthetic
// schema-74 stores with about 256 MiB of entry text each (65,500 entries,
// exponential sizes with a heavy tail), the estimate's sample bound 2,048:
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> PEASANT_SANDBOX_UPGRADE_TEXT_MB=256 \
//	  go test ./internal/store -run TestSandboxStoreUpgradeDiskBasisMeasure -v -timeout 0
//
//	                                   text in the entry rows   text in the bodies
//	database                                  522,498,048           757,714,944
//	entry table + indexes (dbstat)            321,654,784             2,744,320
//	search indexes (dbstat)                   197,386,240           163,635,200
//	basis estimate                            623,550,266           166,070,158
//	estimate time (rows read)              7.3 ms (2,050)       12.0 ms (2,050)
//	dbstat time, entry table + search         259 ms               535 ms
//	step 75 log high-water                    719,393,232           169,035,392
//	step 75 database growth                   194,281,472               602,112
//	used (high-water + growth)                913,674,704           169,637,504
//	used / dbstat basis                             1.760                 1.020
//	used / database                                 1.749                 0.224
//	plan asked for                    966,621,389 (1.06x used)  307,229,793 (1.81x used)
//
// The rows-layout estimate exceeds the database and is capped at it.
const (
	// StoreUpgradeEntryTableRebuildDiskFactor is for a step that copies the
	// entry table and rebuilds the search indexes. Its basis is the entry
	// table with its indexes plus the search indexes the step rebuilds
	// (step 75), or the entry table alone (step 11, which predates search).
	// Measured on step 75: what the step used, the log's high-water mark plus
	// the database's growth, was 1.760 times dbstat's size of that basis on
	// the store with its text in the entry rows (1.828 on a 16 MiB store of
	// the same layout) and 1.020 times on the store with its text in the
	// bodies. 1.85 covers the largest. A store whose text is in the bodies
	// asks for 1.85 times its search index and small entry table, not for
	// 1.85 times its database: the 43 GB store measured on 2026-11-11, with
	// a 39 MiB entry table and 16.4 GiB of text in the bodies, left a log of
	// 4.31 GB, about the size of its search index.
	StoreUpgradeEntryTableRebuildDiskFactor = 1.85

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

	// StoreUpgradeEstimateSampleRows bounds the rows the free-disk estimate
	// reads from the entry table: the smallest and the largest rowid, then at
	// most this many rows at evenly spaced rowids, each measured from its
	// record header with octet_length, which loads no value and no overflow
	// page. A table with no more rowids is read in full. Measured: 2,050
	// rows read and 7 to 12 ms for the whole estimate on 522 MB and 758 MB
	// stores, where dbstat took 259 and 535 ms to walk the same objects and
	// walks every page, so its cost grows with the store (a 40 GB entry
	// table would take tens of seconds). The cost of the sample does not.
	StoreUpgradeEstimateSampleRows = 2048

	// StoreUpgradeEstimateRowOverheadBytes is what one entry row costs beyond
	// its leaf share and overflow pages: the record header, the rowid and
	// its entries in the table's six indexes. Measured 236 to 362 bytes per
	// row on synthetic stores of 256-, 1,024- and 4,096-byte entries.
	StoreUpgradeEstimateRowOverheadBytes = 384

	// StoreUpgradeEstimateSampleMargin multiplies a sampled table estimate,
	// so a sample that misses the heavy tail of entry sizes still errs high.
	// The sample's error on the 256 MiB store with heavy-tailed entries was
	// within 10 percent; sampled estimates came out 1.28 to 1.48 times
	// dbstat's size.
	StoreUpgradeEstimateSampleMargin = 1.25

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
