package store

// upgradeCatalogEntry is the reader-facing description of one migration.
type upgradeCatalogEntry struct {
	title  string
	cost   UpgradeCost
	counts UpgradeUnit
}

// upgradeStepCatalog gives every schema version a title and a cost class.
// The key is the schema version the migration produces. A test fails when a
// migration in the chain has no entry, so a new migration cannot ship
// without saying what it does and what it costs.
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
	11: {title: "rename the tool call column and add tool kind and stop reason", cost: UpgradeCostScalesWithRows},
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
	75: {title: "add image entries and rebuild the search index", cost: UpgradeCostScalesWithText},
	74: {title: "add the conversation turn count and fill it for stored sessions", cost: UpgradeCostScalesWithRows, counts: UpgradeUnitSessions},
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
