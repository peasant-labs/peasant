package defaults

import "time"

// The store upgrade's free-disk factors. Each is the share of the database
// file's size the upgrade asks to be free before a long step may run. Both are
// estimates until a sandbox upgrade of a synthetic store of known text size
// records the write-ahead-log high-water mark and the free-space change per
// step; tests assert the invariant (refuse when free is below required),
// never these values.
const (
	// StoreUpgradeTextStepDiskFactor is the estimate for a step whose cost
	// scales with the stored text, such as a rebuild of the search index.
	// Derivation: a fresh build of the search index measured about 0.39 times
	// the indexed text bytes (the store's own search estimate uses the same
	// figure), and the database file size is an upper bound on the text it
	// holds.
	StoreUpgradeTextStepDiskFactor = 0.39

	// StoreUpgradeRowsStepDiskFactor is the estimate for a step whose cost
	// scales with the row count, such as a table rebuild or a per-session
	// data fill. Derivation: such a step rewrites row tables and their
	// indexes, never the stored text; on the 43 GB store measured on
	// 2026-11-11 the entry rows were 39 MiB, under 0.1% of the file, and the
	// whole upgrade left a 4.31 GB log (0.10 of the file) with the text
	// rebuild included. 0.10 therefore bounds a row-scaled step with margin.
	StoreUpgradeRowsStepDiskFactor = 0.10

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
