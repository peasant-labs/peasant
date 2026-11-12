package store

import "github.com/peasant-labs/peasant/internal/defaults"

// upgradeCatalogEntry is the reader-facing description of one migration.
type upgradeCatalogEntry struct {
	title  string
	cost   UpgradeCost
	counts UpgradeUnit
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

// The two overrides: the steps that copy the entry table.
var (
	// entryTableCopyDiskNeed is step 11's: it copies the entry table before
	// any search index exists.
	entryTableCopyDiskNeed = upgradeDiskNeed{factor: defaults.StoreUpgradeEntryTableRebuildDiskFactor, basis: upgradeDiskBasisEntryTable}
	// entryTableRebuildDiskNeed is step 75's: it copies the entry table and
	// rebuilds both search indexes.
	entryTableRebuildDiskNeed = upgradeDiskNeed{factor: defaults.StoreUpgradeEntryTableRebuildDiskFactor, basis: upgradeDiskBasisEntryTableAndSearchIndex}
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
// A cost here is the cost on a large store. Two steps depend on the store
// and are resolved by the plan: the conversation turn count fold reports a
// session count, and the search trigger repair is long only when the store
// carries stale search results (see searchRepairStep).
var upgradeStepCatalog = map[int]upgradeCatalogEntry{
	1:  {title: "create the first tables", cost: UpgradeCostQuick},
	2:  {title: "add entry rows and widen session metrics", cost: UpgradeCostScalesWithRows},
	3:  {title: "add the ingest log", cost: UpgradeCostQuick},
	4:  {title: "add push tracking and full-depth entry columns", cost: UpgradeCostQuick},
	5:  {title: "move known entry extras into their own table", cost: UpgradeCostScalesWithRows},
	6:  {title: "add the model reference table", cost: UpgradeCostQuick},
	7:  {title: "add cost columns", cost: UpgradeCostQuick},
	8:  {title: "add the per-project daily summary", cost: UpgradeCostQuick},
	9:  {title: "add session tags and scope", cost: UpgradeCostQuick},
	10: {title: "add index tracking and the index log", cost: UpgradeCostQuick},
	11: {title: "rename the tool call column and add tool kind and stop reason", cost: UpgradeCostScalesWithRows, disk: entryTableCopyDiskNeed},
	12: {title: "add session commits", cost: UpgradeCostQuick},
	13: {title: "add the annotation tables", cost: UpgradeCostQuick},
	14: {title: "rebuild annotations with project targets", cost: UpgradeCostScalesWithRows},
	15: {title: "add the web annotator", cost: UpgradeCostQuick},
	16: {title: "rebuild the annotation tables with stable identifiers", cost: UpgradeCostScalesWithRows},
	17: {title: "add annotation scale kinds", cost: UpgradeCostQuick},
	18: {title: "add entry-level annotation types", cost: UpgradeCostQuick},
	19: {title: "link entry-level annotation types to session types", cost: UpgradeCostQuick},
	20: {title: "add the annotation approval type", cost: UpgradeCostQuick},
	21: {title: "add pending annotations", cost: UpgradeCostQuick},
	22: {title: "add file familiarity", cost: UpgradeCostQuick},
	23: {title: "prepare the tables for opaque identifiers", cost: UpgradeCostScalesWithRows},
	24: {title: "add session commands", cost: UpgradeCostQuick},
	25: {title: "add research annotations", cost: UpgradeCostQuick},
	26: {title: "add the entry part type", cost: UpgradeCostQuick},
	27: {title: "update the friction episode types", cost: UpgradeCostQuick},
	28: {title: "add lessons", cost: UpgradeCostQuick},
	29: {title: "drop the lesson annotation link", cost: UpgradeCostQuick},
	30: {title: "add the memory injection log", cost: UpgradeCostQuick},
	31: {title: "remove duplicate lessons", cost: UpgradeCostScalesWithRows},
	32: {title: "add lesson sources", cost: UpgradeCostQuick},
	33: {title: "rename harness identifiers in stored sessions", cost: UpgradeCostScalesWithRows},
	34: {title: "add pulled transcripts", cost: UpgradeCostQuick},
	35: {title: "build the first search index over entries", cost: UpgradeCostScalesWithText},
	36: {title: "add the custom label type", cost: UpgradeCostQuick},
	37: {title: "add the session license", cost: UpgradeCostQuick},
	38: {title: "add the pulled transcript license", cost: UpgradeCostQuick},
	39: {title: "add per-turn label types", cost: UpgradeCostQuick},
	40: {title: "add the session to commit association ledger", cost: UpgradeCostScalesWithRows},
	41: {title: "add association targets for annotations", cost: UpgradeCostQuick},
	42: {title: "admit a new harness in stored sessions", cost: UpgradeCostScalesWithRows},
	43: {title: "add session publications", cost: UpgradeCostQuick},
	44: {title: "add the claude discovery cache", cost: UpgradeCostQuick},
	45: {title: "add the opencode change cursor", cost: UpgradeCostQuick},
	46: {title: "add the session origin", cost: UpgradeCostQuick},
	47: {title: "add the entry digest", cost: UpgradeCostQuick},
	48: {title: "add the annotation run state", cost: UpgradeCostQuick},
	49: {title: "add annotation target anchors", cost: UpgradeCostQuick},
	50: {title: "add the captured source fingerprint", cost: UpgradeCostQuick},
	51: {title: "add captured source provenance", cost: UpgradeCostQuick},
	52: {title: "separate stored prose from entry previews", cost: UpgradeCostQuick},
	53: {title: "admit pi in stored sessions", cost: UpgradeCostScalesWithRows},
	54: {title: "record the index representation", cost: UpgradeCostQuick},
	55: {title: "separate stored adapter output from computed analytics", cost: UpgradeCostQuick},
	56: {title: "add index input proofs", cost: UpgradeCostQuick},
	57: {title: "add metrics input and output proofs", cost: UpgradeCostQuick},
	58: {title: "retire old annotation types", cost: UpgradeCostQuick},
	59: {title: "close the capture format set", cost: UpgradeCostScalesWithRows},
	60: {title: "add the managed generation catalog", cost: UpgradeCostQuick},
	61: {title: "add the reverse target indexes", cost: UpgradeCostQuick},
	62: {title: "add the harmonized content model", cost: UpgradeCostScalesWithRows},
	63: {title: "add source unavailability records", cost: UpgradeCostQuick},
	64: {title: "add run outcomes", cost: UpgradeCostQuick},
	65: {title: "add layout proof to the claude discovery cache", cost: UpgradeCostQuick},
	66: {title: "add session thinking levels", cost: UpgradeCostQuick},
	67: {title: "add the latest attempt pointers", cost: UpgradeCostQuick},
	68: {title: "add the observation cache", cost: UpgradeCostQuick},
	69: {title: "rebuild the source unavailability reasons", cost: UpgradeCostScalesWithRows},
	70: {title: "add the parent lane and the store revision", cost: UpgradeCostScalesWithRows},
	71: {title: "add lookups for scoped harvest", cost: UpgradeCostScalesWithRows},
	72: {title: "compact claude identity facts", cost: UpgradeCostScalesWithRows},
	73: {title: "widen the stored harness set", cost: UpgradeCostScalesWithRows},
	74: {title: "add the conversation turn count and fill it for stored sessions", cost: UpgradeCostScalesWithRows, counts: UpgradeUnitSessions},
	75: {title: "add image entries and rebuild the search index", cost: UpgradeCostScalesWithText, disk: entryTableRebuildDiskNeed},
	76: {title: searchRepairQuickTitle, cost: UpgradeCostQuick},
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
