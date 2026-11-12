package store

import "github.com/peasant-labs/peasant/internal/defaults"

// upgradeCatalogEntry is the reader-facing description of one migration.
type upgradeCatalogEntry struct {
	title string
	cost  UpgradeCost
	// diskFactor is the share of the step's basis it asks to be free. It is
	// one of the named factors in internal/defaults, never zero: the plan
	// asks for the largest need, factor times basis, among its steps.
	diskFactor float64
	// diskBasis is what diskFactor multiplies: the whole database, or the
	// tables the step rewrites. Never empty.
	diskBasis upgradeDiskBasis
	counts    UpgradeUnit
}

// upgradeStepCatalog gives every schema version a title, a cost class, a
// disk factor and the basis that factor multiplies.
// The key is the schema version the migration produces. A test fails when a
// migration in the chain has no entry, so a new migration cannot ship
// without saying what it does, what it costs and how much disk it needs.
//
// Disk need: step 75 copies the entry table and rebuilds both search
// indexes, so it asks for the entry table rebuild factor times the entry
// table and those indexes as measured on the store; step 11 copies the
// entry table before any search index exists and takes the same factor
// times the entry table. The factor and its measurement are in
// internal/defaults/store_upgrade.go. Steps that build a search index
// without a table copy take the search rebuild factor; row-scaled steps the
// rows factor (steps 70 to 74 measured at most 0.0018); everything else the
// quick one. Those three multiply the whole database.
//
// A cost here is the cost on a large store. Two steps depend on the store
// and are resolved by the plan: the conversation turn count fold reports a
// session count, and the search trigger repair is long only when the store
// carries stale search results (see searchRepairStep).
var upgradeStepCatalog = map[int]upgradeCatalogEntry{
	1:  {title: "create the first tables", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	2:  {title: "add entry rows and widen session metrics", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	3:  {title: "add the ingest log", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	4:  {title: "add push tracking and full-depth entry columns", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	5:  {title: "move known entry extras into their own table", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	6:  {title: "add the model reference table", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	7:  {title: "add cost columns", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	8:  {title: "add the per-project daily summary", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	9:  {title: "add session tags and scope", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	10: {title: "add index tracking and the index log", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	11: {title: "rename the tool call column and add tool kind and stop reason", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeEntryTableRebuildDiskFactor, diskBasis: upgradeDiskBasisEntryTable},
	12: {title: "add session commits", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	13: {title: "add the annotation tables", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	14: {title: "rebuild annotations with project targets", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	15: {title: "add the web annotator", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	16: {title: "rebuild the annotation tables with stable identifiers", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	17: {title: "add annotation scale kinds", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	18: {title: "add entry-level annotation types", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	19: {title: "link entry-level annotation types to session types", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	20: {title: "add the annotation approval type", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	21: {title: "add pending annotations", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	22: {title: "add file familiarity", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	23: {title: "prepare the tables for opaque identifiers", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	24: {title: "add session commands", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	25: {title: "add research annotations", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	26: {title: "add the entry part type", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	27: {title: "update the friction episode types", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	28: {title: "add lessons", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	29: {title: "drop the lesson annotation link", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	30: {title: "add the memory injection log", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	31: {title: "remove duplicate lessons", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	32: {title: "add lesson sources", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	33: {title: "rename harness identifiers in stored sessions", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	34: {title: "add pulled transcripts", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	35: {title: "build the first search index over entries", cost: UpgradeCostScalesWithText, diskFactor: defaults.StoreUpgradeSearchRebuildDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	36: {title: "add the custom label type", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	37: {title: "add the session license", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	38: {title: "add the pulled transcript license", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	39: {title: "add per-turn label types", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	40: {title: "add the session to commit association ledger", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	41: {title: "add association targets for annotations", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	42: {title: "admit a new harness in stored sessions", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	43: {title: "add session publications", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	44: {title: "add the claude discovery cache", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	45: {title: "add the opencode change cursor", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	46: {title: "add the session origin", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	47: {title: "add the entry digest", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	48: {title: "add the annotation run state", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	49: {title: "add annotation target anchors", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	50: {title: "add the captured source fingerprint", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	51: {title: "add captured source provenance", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	52: {title: "separate stored prose from entry previews", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	53: {title: "admit pi in stored sessions", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	54: {title: "record the index representation", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	55: {title: "separate stored adapter output from computed analytics", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	56: {title: "add index input proofs", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	57: {title: "add metrics input and output proofs", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	58: {title: "retire old annotation types", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	59: {title: "close the capture format set", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	60: {title: "add the managed generation catalog", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	61: {title: "add the reverse target indexes", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	62: {title: "add the harmonized content model", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	63: {title: "add source unavailability records", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	64: {title: "add run outcomes", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	65: {title: "add layout proof to the claude discovery cache", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	66: {title: "add session thinking levels", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	67: {title: "add the latest attempt pointers", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	68: {title: "add the observation cache", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	69: {title: "rebuild the source unavailability reasons", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	70: {title: "add the parent lane and the store revision", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	71: {title: "add lookups for scoped harvest", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	72: {title: "compact claude identity facts", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	73: {title: "widen the stored harness set", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	75: {title: "add image entries and rebuild the search index", cost: UpgradeCostScalesWithText, diskFactor: defaults.StoreUpgradeEntryTableRebuildDiskFactor, diskBasis: upgradeDiskBasisEntryTableAndSearchIndex},
	74: {title: "add the conversation turn count and fill it for stored sessions", cost: UpgradeCostScalesWithRows, diskFactor: defaults.StoreUpgradeRowsStepDiskFactor, diskBasis: upgradeDiskBasisDatabase, counts: UpgradeUnitSessions},
	76: {title: searchRepairQuickTitle, cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
	77: {title: "store the import profile and give run outcomes stable ids", cost: UpgradeCostQuick, diskFactor: defaults.StoreUpgradeQuickStepDiskFactor, diskBasis: upgradeDiskBasisDatabase},
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
