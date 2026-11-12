package defaults

import "time"

// The store upgrade's free-disk factors. Each is the share of the database
// file's size the upgrade asks to be free before a long step may run. Both
// are estimates; tests assert the invariant (refuse when free is below
// required), never these values.
//
// Sandbox measurement, 2026-11-12, revision 036f8b3f2, synthetic schema-69
// store with 268 MB of text held in the entry rows (485 MB database):
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> PEASANT_SANDBOX_UPGRADE_TEXT_MB=256 \
//	  go test ./internal/store -run TestSandboxStoreUpgradeMeasure -v -timeout 0
//
// The step that rebuilds the entry table and the search index took 1m43s of
// a 1m44s upgrade and grew the write-ahead log to 1.35 times the database;
// the database itself grew by 0.34 times. The five row-scaled steps before
// it wrote under 0.002 times the database together. That store is the worst
// shape for the text step: every byte of text sits in the table the step
// copies. On the 43 GB store measured on 2026-11-11, where the text sits in
// a table the step does not copy, the same upgrade left a log of 0.10 times
// the database. The text factor therefore depends on where a store keeps its
// text, and one constant cannot fit both shapes.
const (
	// StoreUpgradeTextStepDiskFactor is the estimate for a step whose cost
	// scales with the stored text, such as a rebuild of the search index.
	// Derivation: a fresh build of the search index measured about 0.39 times
	// the indexed text bytes (the store's own search estimate uses the same
	// figure), and the database file size is an upper bound on the text it
	// holds. It covers a store that keeps its text outside the entry rows
	// (measured 0.10) with margin, and it is below what a store that keeps
	// all text in the entry rows needs (measured 1.35 plus 0.34 growth). On
	// such a store the preflight can pass and the step can still run out of
	// disk; the step then rolls back and the store stays at its schema.
	StoreUpgradeTextStepDiskFactor = 0.39

	// StoreUpgradeRowsStepDiskFactor is the estimate for a step whose cost
	// scales with the row count, such as a table rebuild or a per-session
	// data fill. Such a step rewrites row tables and their indexes, never
	// the stored text. The sandbox run measured under 0.002 for five such
	// steps together; 0.10 bounds one with a wide margin.
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
