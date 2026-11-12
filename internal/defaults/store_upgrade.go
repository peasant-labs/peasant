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
//	  go test -tags upgrade_measure ./internal/store -run TestSandboxStoreUpgradeMeasure -v -timeout 0
//
//	step 70   57ms      444,992 log bytes (0.0009 of the database)
//	step 71    1ms       53,560           (0.0001)
//	step 72   13ms       20,600           (0.00004)
//	step 73   20ms      873,440           (0.0018)
//	step 74  491ms      181,280           (0.0004)
//	step 75  1m43s  654,091,232 high-water (1.348), database grew by 165,154,816 (0.340)
//	step 76  310ms            0
//
// Sandbox measurement of step 75 by layout, 2026-11-12, store code at
// revision fc8fb98c9 plus the entry table read in full: two synthetic
// schema-74 stores with about 256 MiB of entry text each (65,500 entries,
// exponential sizes with a heavy tail), and a store whose text is in the
// bodies and whose entry table holds 65,536 rows, the read bound (rows of
// about 3,800 bytes, every hundredth a hundred times larger):
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> PEASANT_SANDBOX_UPGRADE_TEXT_MB=256 \
//	  go test -tags upgrade_measure ./internal/store -run TestSandboxStoreUpgradeDiskBasisMeasure -v -timeout 0
//
//	                                text in the entry rows   text in the bodies   bodies, at the bound
//	database                               522,498,048          757,714,944          988,119,040
//	entry table + indexes (dbstat)         321,654,784            2,744,320          564,670,464
//	search indexes (dbstat)                197,386,240          163,635,200          416,141,312
//	basis the plan uses                    522,498,048          168,244,916          988,119,040
//	  how                              the whole database    read in full         read in full, capped
//	estimate time (entry rows read)       20 us (none)         4.8 ms (2,620)       74 ms (65,536)
//	dbstat time, entry table + search        227 ms                96 ms               377 ms
//	step 75 log high-water                 719,393,232          169,035,392        1,401,092,552
//	step 75 database growth                194,281,472              602,112          411,136,000
//	used (high-water + growth)             913,674,704          169,637,504        1,812,228,552
//	used / dbstat basis                          1.760                1.020                1.848
//	used / database                              1.749                0.224                1.834
//	plan asked for                  966,621,389 (1.06x)  311,253,095 (1.83x)  1,828,020,224 (1.01x)
//
// Every basis the plan measured was at or above dbstat's: 1.011 and 1.017
// times. The bound store's measured basis, 998,016,761, exceeds its
// database and is capped at it.
const (
	// StoreUpgradeEntryTableRebuildDiskFactor is for a step that copies the
	// entry table and rebuilds the search indexes. Its basis is the entry
	// table with its indexes plus the search indexes the step rebuilds
	// (step 75), or the entry table alone (step 11, which predates search).
	// Measured on step 75: what the step used, the log's high-water mark plus
	// the database's growth, was 1.760 times dbstat's size of that basis on
	// the store with its text in the entry rows (1.828 on a 16 MiB store of
	// the same layout), 1.020 times on the store with its text in the
	// bodies, and 1.848 times on a store whose text is in the bodies and
	// whose entry table of page-sized rows sits at the read bound. 1.85
	// covers the largest, with little room on the last. A store whose text
	// is in the bodies asks for 1.85 times its search index and entry table,
	// not for 1.85 times its database: the 43 GB store measured on 2026-11-11, with
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

	// StoreUpgradeEstimateMaxEntryRows bounds the rows the free-disk estimate
	// reads from the entry table of a store whose text is in the bodies. Each
	// row is measured from its record header with octet_length, which loads
	// no value and no overflow page, and every row is read, so the estimate
	// counts a heavy tail in full and cannot come in low. A larger table, or
	// a store whose text is in the entry rows, takes the whole database as
	// its basis, which that table dominates. Measured: 74 ms for the whole
	// basis on a store at the bound with rows of about a page each (the
	// costliest read: one leaf page per row), 4.8 ms for 2,620 rows; the
	// 43 GB store of 2026-11-11 keeps 34,010 entry rows. dbstat walks every
	// page instead, so its cost grows with the text (377 ms at the bound,
	// tens of seconds for a 40 GB entry table).
	StoreUpgradeEstimateMaxEntryRows = 65536

	// StoreUpgradeEstimateRowOverheadBytes is what one entry row costs beyond
	// its leaf share and overflow pages: the record header, the rowid and
	// its entries in the table's six indexes. Measured 236 to 362 bytes per
	// row on synthetic stores of 256-, 1,024- and 4,096-byte entries.
	StoreUpgradeEstimateRowOverheadBytes = 384

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
