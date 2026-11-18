package store

import "github.com/peasant-labs/peasant/internal/defaults"

// upgradeCatalogEntry is the reader-facing description of one migration.
type upgradeCatalogEntry struct {
	title       string
	cost        UpgradeCost
	counts      UpgradeUnit
	stampEffect StampEffect
	// disk overrides the free-disk need the cost class implies
	// (upgradeCostDiskNeeds). The zero value derives it. Only a step that
	// copies a table sets it: its need follows the table it copies, not the
	// whole database.
	disk upgradeDiskNeed
}

// upgradeDiskNeed is what one step asks to be free: factor times basis.
type upgradeDiskNeed struct {
	// factor is the share of the basis the step asks to be free, one of the
	// named factors in internal/defaults. Never zero.
	factor float64
	// basis is what factor multiplies: the whole database, or the tables
	// the step rewrites, measured on the store.
	basis upgradeDiskBasis
}

// upgradeCostDiskNeeds is the free-disk need each cost class implies. Every
// one multiplies the whole database: a quick step changes the catalog or a
// few rows, a row-scaled step rewrites rows but no stored text (steps 70 to
// 74 measured at most 0.0018), and a text-scaled step that copies no table
// builds or rebuilds a search index. The factors and their measurements
// are in internal/defaults/store_upgrade.go.
var upgradeCostDiskNeeds = map[UpgradeCost]upgradeDiskNeed{
	UpgradeCostQuick:          {factor: defaults.StoreUpgradeQuickStepDiskFactor, basis: upgradeDiskBasisDatabase},
	UpgradeCostScalesWithRows: {factor: defaults.StoreUpgradeRowsStepDiskFactor, basis: upgradeDiskBasisDatabase},
	UpgradeCostScalesWithText: {factor: defaults.StoreUpgradeSearchRebuildDiskFactor, basis: upgradeDiskBasisDatabase},
}

// The overrides: the steps that copy the entry table, and the steps that
// copy the session tables.
var (
	// entryTableCopyDiskNeed is step 11's: it copies the entry table before
	// any search index exists.
	entryTableCopyDiskNeed = upgradeDiskNeed{factor: defaults.StoreUpgradeEntryTableRebuildDiskFactor, basis: upgradeDiskBasisEntryTable}
	// entryTableRebuildDiskNeed is step 75's: it copies the entry table and
	// rebuilds both search indexes.
	entryTableRebuildDiskNeed = upgradeDiskNeed{factor: defaults.StoreUpgradeEntryTableRebuildDiskFactor, basis: upgradeDiskBasisEntryTableAndSearchIndex}
	// sessionTablesRebuildDiskNeed is the need of steps 73 and 78: each copies
	// the sessions and daily harness summary tables to widen their harness
	// CHECK.
	sessionTablesRebuildDiskNeed = upgradeDiskNeed{factor: defaults.StoreUpgradeSessionTablesRebuildDiskFactor, basis: upgradeDiskBasisSessionTables}
)

// diskNeedFor is the entry's free-disk need at the cost the plan resolved
// for this store: its override, or the need that cost implies. The search
// trigger repair (schema 76) is quick on most stores and text-scaled on a
// store with stale search results; its need follows the resolved cost.
func (e upgradeCatalogEntry) diskNeedFor(cost UpgradeCost) upgradeDiskNeed {
	if e.disk != (upgradeDiskNeed{}) {
		return e.disk
	}
	return upgradeCostDiskNeeds[cost]
}

// upgradeStepCatalog gives every schema version a title and a cost class,
// in version order. The key is the schema version the migration produces. A
// test fails when a migration in the chain has no entry, so a new migration
// cannot ship without saying what it does and what it costs; its free-disk
// need follows from the cost unless it copies a table (disk).
//
// A cost here is the cost on a large store. Store-specific planning resolves
// the search repair and the source-diagnostic rebuild, and the conversation
// turn count fold reports a session count.
var upgradeStepCatalog = map[int]upgradeCatalogEntry{
	1:  {stampEffect: StampEffectNone, title: "create the first tables", cost: UpgradeCostQuick},
	2:  {stampEffect: StampEffectNone, title: "add entry rows and widen session metrics", cost: UpgradeCostScalesWithRows},
	3:  {stampEffect: StampEffectNone, title: "add the ingest log", cost: UpgradeCostQuick},
	4:  {stampEffect: StampEffectNone, title: "add push tracking and full-depth entry columns", cost: UpgradeCostQuick},
	5:  {stampEffect: StampEffectNone, title: "move known entry extras into their own table", cost: UpgradeCostScalesWithRows},
	6:  {stampEffect: StampEffectNone, title: "add the model reference table", cost: UpgradeCostQuick},
	7:  {stampEffect: StampEffectNone, title: "add cost columns", cost: UpgradeCostQuick},
	8:  {stampEffect: StampEffectNone, title: "add the per-project daily summary", cost: UpgradeCostQuick},
	9:  {stampEffect: StampEffectNone, title: "add session tags and scope", cost: UpgradeCostQuick},
	10: {stampEffect: StampEffectIndexInvalidated, title: "add index tracking and the index log", cost: UpgradeCostQuick},
	11: {stampEffect: StampEffectNone, title: "rename the tool call column and add tool kind and stop reason", cost: UpgradeCostScalesWithRows, disk: entryTableCopyDiskNeed},
	12: {stampEffect: StampEffectNone, title: "add session commits", cost: UpgradeCostQuick},
	13: {stampEffect: StampEffectNone, title: "add the annotation tables", cost: UpgradeCostQuick},
	14: {stampEffect: StampEffectNone, title: "rebuild annotations with project targets", cost: UpgradeCostScalesWithRows},
	15: {stampEffect: StampEffectNone, title: "add the web annotator", cost: UpgradeCostQuick},
	16: {stampEffect: StampEffectNone, title: "rebuild the annotation tables with stable identifiers", cost: UpgradeCostScalesWithRows},
	17: {stampEffect: StampEffectNone, title: "add annotation scale kinds", cost: UpgradeCostQuick},
	18: {stampEffect: StampEffectNone, title: "add entry-level annotation types", cost: UpgradeCostQuick},
	19: {stampEffect: StampEffectNone, title: "link entry-level annotation types to session types", cost: UpgradeCostQuick},
	20: {stampEffect: StampEffectNone, title: "add the annotation approval type", cost: UpgradeCostQuick},
	21: {stampEffect: StampEffectNone, title: "add pending annotations", cost: UpgradeCostQuick},
	22: {stampEffect: StampEffectNone, title: "add file familiarity", cost: UpgradeCostQuick},
	23: {stampEffect: StampEffectNone, title: "prepare the tables for opaque identifiers", cost: UpgradeCostScalesWithRows},
	24: {stampEffect: StampEffectNone, title: "add session commands", cost: UpgradeCostQuick},
	25: {stampEffect: StampEffectNone, title: "add research annotations", cost: UpgradeCostQuick},
	26: {stampEffect: StampEffectNone, title: "add the entry part type", cost: UpgradeCostQuick},
	27: {stampEffect: StampEffectNone, title: "update the friction episode types", cost: UpgradeCostQuick},
	28: {stampEffect: StampEffectNone, title: "add lessons", cost: UpgradeCostQuick},
	29: {stampEffect: StampEffectNone, title: "drop the lesson annotation link", cost: UpgradeCostQuick},
	30: {stampEffect: StampEffectNone, title: "add the memory injection log", cost: UpgradeCostQuick},
	31: {stampEffect: StampEffectNone, title: "remove duplicate lessons", cost: UpgradeCostScalesWithRows},
	32: {stampEffect: StampEffectNone, title: "add lesson sources", cost: UpgradeCostQuick},
	33: {stampEffect: StampEffectHarnessRename, title: "rename harness identifiers in stored sessions", cost: UpgradeCostScalesWithRows},
	34: {stampEffect: StampEffectNone, title: "add pulled transcripts", cost: UpgradeCostQuick},
	35: {stampEffect: StampEffectNone, title: "build the first search index over entries", cost: UpgradeCostScalesWithText},
	36: {stampEffect: StampEffectNone, title: "add the custom label type", cost: UpgradeCostQuick},
	37: {stampEffect: StampEffectNone, title: "add the session license", cost: UpgradeCostQuick},
	38: {stampEffect: StampEffectNone, title: "add the pulled transcript license", cost: UpgradeCostQuick},
	39: {stampEffect: StampEffectNone, title: "add per-turn label types", cost: UpgradeCostQuick},
	40: {stampEffect: StampEffectNone, title: "add the session to commit association ledger", cost: UpgradeCostScalesWithRows},
	41: {stampEffect: StampEffectNone, title: "add association targets for annotations", cost: UpgradeCostQuick},
	42: {stampEffect: StampEffectNone, title: "admit a new harness in stored sessions", cost: UpgradeCostScalesWithRows},
	43: {stampEffect: StampEffectNone, title: "add session publications", cost: UpgradeCostQuick},
	44: {stampEffect: StampEffectNone, title: "add the claude discovery cache", cost: UpgradeCostQuick},
	45: {stampEffect: StampEffectNone, title: "add the opencode change cursor", cost: UpgradeCostQuick},
	46: {stampEffect: StampEffectNone, title: "add the session origin", cost: UpgradeCostQuick},
	47: {stampEffect: StampEffectNone, title: "add the entry digest", cost: UpgradeCostQuick},
	48: {stampEffect: StampEffectNone, title: "add the annotation run state", cost: UpgradeCostQuick},
	49: {stampEffect: StampEffectNone, title: "add annotation target anchors", cost: UpgradeCostQuick},
	50: {stampEffect: StampEffectNone, title: "add the captured source fingerprint", cost: UpgradeCostQuick},
	51: {stampEffect: StampEffectNone, title: "add captured source provenance", cost: UpgradeCostQuick},
	52: {stampEffect: StampEffectNone, title: "separate stored prose from entry previews", cost: UpgradeCostQuick},
	53: {stampEffect: StampEffectNone, title: "admit pi in stored sessions", cost: UpgradeCostScalesWithRows},
	54: {stampEffect: StampEffectNone, title: "record the index representation", cost: UpgradeCostQuick},
	55: {stampEffect: StampEffectNone, title: "separate stored adapter output from computed analytics", cost: UpgradeCostQuick},
	56: {stampEffect: StampEffectNone, title: "add index input proofs", cost: UpgradeCostQuick},
	57: {stampEffect: StampEffectNone, title: "add metrics input and output proofs", cost: UpgradeCostQuick},
	58: {stampEffect: StampEffectNone, title: "retire old annotation types", cost: UpgradeCostQuick},
	59: {stampEffect: StampEffectNone, title: "close the capture format set", cost: UpgradeCostScalesWithRows},
	60: {stampEffect: StampEffectNone, title: "add the managed generation catalog", cost: UpgradeCostQuick},
	61: {stampEffect: StampEffectNone, title: "add the reverse target indexes", cost: UpgradeCostQuick},
	62: {stampEffect: StampEffectSearchInvalidated, title: "add the harmonized content model", cost: UpgradeCostScalesWithRows},
	63: {stampEffect: StampEffectNone, title: "add source unavailability records", cost: UpgradeCostQuick},
	64: {stampEffect: StampEffectNone, title: "add run outcomes", cost: UpgradeCostQuick},
	65: {stampEffect: StampEffectNone, title: "add layout proof to the claude discovery cache", cost: UpgradeCostQuick},
	66: {stampEffect: StampEffectNone, title: "add session thinking levels", cost: UpgradeCostQuick},
	67: {stampEffect: StampEffectNone, title: "add the latest attempt pointers", cost: UpgradeCostQuick},
	68: {stampEffect: StampEffectNone, title: "add the observation cache", cost: UpgradeCostQuick},
	69: {stampEffect: StampEffectNone, title: "rebuild the source unavailability reasons", cost: UpgradeCostScalesWithRows},
	70: {stampEffect: StampEffectNone, title: "add the parent lane and the store revision", cost: UpgradeCostScalesWithRows},
	71: {stampEffect: StampEffectNone, title: "add lookups for scoped harvest", cost: UpgradeCostScalesWithRows},
	72: {stampEffect: StampEffectNone, title: "compact claude identity facts", cost: UpgradeCostScalesWithRows},
	73: {stampEffect: StampEffectNone, title: "widen the stored harness set", cost: UpgradeCostScalesWithRows, disk: sessionTablesRebuildDiskNeed},
	74: {stampEffect: StampEffectNone, title: "add the conversation turn count and fill it for stored sessions", cost: UpgradeCostScalesWithRows, counts: UpgradeUnitSessions},
	75: {stampEffect: StampEffectNone, title: "add image entries and rebuild the search index", cost: UpgradeCostScalesWithText, disk: entryTableRebuildDiskNeed},
	76: {stampEffect: StampEffectNone, title: searchRepairQuickTitle, cost: UpgradeCostQuick},
	77: {stampEffect: StampEffectRecountOwed, title: "mark the stored turn counts for a recount", cost: UpgradeCostQuick},
	78: {stampEffect: StampEffectNone, title: "widen the stored harness set for the antigravity cli and ide", cost: UpgradeCostScalesWithRows, disk: sessionTablesRebuildDiskNeed},
	79: {stampEffect: StampEffectNone, title: "store the import profile and give run outcomes stable ids", cost: UpgradeCostQuick},
	80: {stampEffect: StampEffectNone, title: "add the missing publication capture outcome", cost: UpgradeCostQuick},
	81: {stampEffect: StampEffectNone, title: "separate source diagnostic lanes", cost: UpgradeCostScalesWithRows},
	82: {stampEffect: StampEffectNone, title: "add the local owner and the append-only claim log", cost: UpgradeCostQuick},
}

// upgradeRecountCatalogEntry describes one recount a migration can leave
// owed: the marker row it writes in pending_recounts, the migration that
// writes it, and the step the plan shows for it.
type upgradeRecountCatalogEntry struct {
	upgradeCatalogEntry
	// markedBy is the schema version whose migration writes the marker.
	markedBy int
	// owedAfterMigration is a query that returns a row exactly when the
	// migration markedBy writes the marker on this store. The plan runs it
	// on a store below markedBy, where no marker can exist yet. It restates
	// the condition of the shipped migration's INSERT, which cannot name a
	// constant; a test pins the two together on a store with sessions and on
	// one without.
	owedAfterMigration string
}

// upgradeRecountCatalog gives every recount marker a title and a cost
// class, keyed by its name in pending_recounts. A test fails when a recount
// registered in upgradeRecountRuns has no entry here, or the reverse.
//
// The turn count recount folds every stored session's entries once and
// writes two integers per session. Measured on synthetic stores upgraded from
// schema 76 (TestSandboxTurnCountRecountMeasure, 2026-11-12, recorded in
// docs/store-upgrade.md): 0.20 to 0.23 ms per session at 40 entries and 0.42
// ms at 120, so its time grows with the sessions and their entries, and about
// 75 log bytes per session, at most 0.0023 times the database. It is a
// row-scaled step that counts sessions, like step 74, with the row-scaled
// disk need.
var upgradeRecountCatalog = map[string]upgradeRecountCatalogEntry{
	pendingRecountTurnCounts: {
		upgradeCatalogEntry: upgradeCatalogEntry{title: "recount the turn counts of every stored session", cost: UpgradeCostScalesWithRows, counts: UpgradeUnitSessions},
		markedBy:            77,
		owedAfterMigration:  `SELECT 1 FROM sessions LIMIT 1`,
	},
}

const (
	// searchRepairQuickTitle is the title of the search trigger repair on a
	// store with no stale search results.
	searchRepairQuickTitle = "repair the search index triggers"
	// searchRepairLongTitle is its title when the repair rebuilds the index.
	searchRepairLongTitle = "rebuild the search index to remove stale results left by 0.16.1"
	// searchRepairSchemaVersion is the schema version the repair produces.
	searchRepairSchemaVersion = 76
)
