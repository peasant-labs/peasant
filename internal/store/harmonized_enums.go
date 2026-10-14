package store

import (
	"encoding/hex"
	"fmt"
)

// validHexDigest reports whether raw is 64 lowercase-or-uppercase hex bytes,
// the shape every stored digest column CHECKs (design §3.3).
func validHexDigest(raw string) bool {
	if len(raw) != 64 {
		return false
	}
	decoded := make([]byte, 32)
	if _, err := hex.Decode(decoded, []byte(raw)); err != nil {
		return false
	}
	return true
}

// BodyDigest is hex sha256(serializeEntry(row)): the entry row's integrity
// anchor, stored in session_entry_bodies.body_digest. It is a digest, not a
// closed set: the NewBodyDigest validator checks the 64-hex shape at trust
// boundaries.
type BodyDigest string

// NewBodyDigest validates a raw body digest at an input boundary.
func NewBodyDigest(raw string) (BodyDigest, error) {
	if !validHexDigest(raw) {
		return "", fmt.Errorf("store.NewBodyDigest: value %q is not 64 hex bytes, so it cannot anchor an entry row whose serialization hashes to sha256; recompute sha256(serializeEntry(row)) and retry", raw)
	}
	return BodyDigest(raw), nil
}

// IsValid reports whether the digest has the 64-hex shape.
func (d BodyDigest) IsValid() bool { return validHexDigest(string(d)) }

func (d BodyDigest) String() string { return string(d) }

// ContentDigest is hex sha256 over all content bytes: the blob store's
// integrity anchor, stored in session_content.digest. Like BodyDigest it is a
// digest, validated by shape at trust boundaries.
type ContentDigest string

// NewContentDigest validates a raw content digest at an input boundary.
func NewContentDigest(raw string) (ContentDigest, error) {
	if !validHexDigest(raw) {
		return "", fmt.Errorf("store.NewContentDigest: value %q is not 64 hex bytes, so it cannot address a content blob whose bytes hash to sha256; recompute sha256 over the blob bytes and retry", raw)
	}
	return ContentDigest(raw), nil
}

// IsValid reports whether the digest has the 64-hex shape.
func (d ContentDigest) IsValid() bool { return validHexDigest(string(d)) }

func (d ContentDigest) String() string { return string(d) }

// StatsSource is the session_captured_stats.source row label: which update
// origin wrote the row last. It is a row label, not per-field provenance — a
// merged row can mix origins, which is why the seed path never reads it
// (design §3.3). Closed set: harness | derived.
type StatsSource string

const (
	// StatsSourceHarness marks a row last written by capture, resume, or
	// backfill (the harness paths, which also write seed_json).
	StatsSourceHarness StatsSource = "harness"
	// StatsSourceDerived marks a row last written by COMPUTE's derived upsert
	// (which never writes seed_json).
	StatsSourceDerived StatsSource = "derived"
)

// AllStatsSources is the exact closed set of stats sources.
var AllStatsSources = []StatsSource{
	StatsSourceHarness,
	StatsSourceDerived,
}

// IsValid reports whether the value is a member of AllStatsSources.
func (s StatsSource) IsValid() bool {
	switch s {
	case StatsSourceHarness, StatsSourceDerived:
		return true
	default:
		return false
	}
}

// NewStatsSource validates a raw stats source at an input boundary.
func NewStatsSource(raw string) (StatsSource, error) {
	v := StatsSource(raw)
	if !v.IsValid() {
		return "", fmt.Errorf("store.NewStatsSource: value %q is outside the closed stats-source set %v; a stats row cannot record its latest update origin; use harness or derived", raw, AllStatsSources)
	}
	return v, nil
}

func (s StatsSource) String() string { return string(s) }

// MigrationPhase is the peasant migrate command's progress and reporting
// axis (design §7.2): 0 preflight, 1 drain, 2 per-session conversion, 3
// search consolidation, 4 cleanup, 5 offline maintenance. Every phase is
// resumable; stop points are every session and phase boundary.
type MigrationPhase int

const (
	// MigrationPhasePreflight is Phase 0: read-only counts per phase; --dry-run stops here.
	MigrationPhasePreflight MigrationPhase = 0
	// MigrationPhaseDrain is Phase 1: discard pending intents, staged directories, and superseded generations.
	MigrationPhaseDrain MigrationPhase = 1
	// MigrationPhaseConvertSessions is Phase 2: convert each file-backed native session under its exclusive lock.
	MigrationPhaseConvertSessions MigrationPhase = 2
	// MigrationPhaseConsolidateSearch is Phase 3: retarget triggers, rebuild the union FTS, drop the old index.
	MigrationPhaseConsolidateSearch MigrationPhase = 3
	// MigrationPhaseCleanup is Phase 4: remove leftover directories, sweep flagged sessions, evaluate preconditions.
	MigrationPhaseCleanup MigrationPhase = 4
	// MigrationPhaseOfflineMaintenance is Phase 5: documented offline optimize + VACUUM (design §7.6).
	MigrationPhaseOfflineMaintenance MigrationPhase = 5
)

// AllMigrationPhases is the exact closed set of migration phases, in order.
var AllMigrationPhases = []MigrationPhase{
	MigrationPhasePreflight,
	MigrationPhaseDrain,
	MigrationPhaseConvertSessions,
	MigrationPhaseConsolidateSearch,
	MigrationPhaseCleanup,
	MigrationPhaseOfflineMaintenance,
}

// IsValid reports whether the value names a migration phase (0..5).
func (p MigrationPhase) IsValid() bool {
	return p >= MigrationPhasePreflight && p <= MigrationPhaseOfflineMaintenance
}

// NewMigrationPhase validates a raw migration phase at an input boundary.
func NewMigrationPhase(raw int) (MigrationPhase, error) {
	v := MigrationPhase(raw)
	if !v.IsValid() {
		return 0, fmt.Errorf("store.NewMigrationPhase: value %d is outside the closed migration-phase range 0..5; migration progress cannot be reported; use a phase from preflight (0) through offline maintenance (5)", raw)
	}
	return v, nil
}

func (p MigrationPhase) String() string {
	switch p {
	case MigrationPhasePreflight:
		return "preflight"
	case MigrationPhaseDrain:
		return "drain"
	case MigrationPhaseConvertSessions:
		return "convert-sessions"
	case MigrationPhaseConsolidateSearch:
		return "consolidate-search"
	case MigrationPhaseCleanup:
		return "cleanup"
	case MigrationPhaseOfflineMaintenance:
		return "offline-maintenance"
	default:
		return "unknown"
	}
}

// RetirementPrecondition names one of the five Release N+1 guards (design
// §7.1): the schema migration refuses unless all hold, and the refusal names
// the failing precondition, its row count, why it blocks, and the fix (run
// peasant migrate with Release N, then upgrade).
type RetirementPrecondition string

const (
	// RetirementPreconditionProjectionGenerationsEmpty requires
	// session_projection_generations to be empty.
	RetirementPreconditionProjectionGenerationsEmpty RetirementPrecondition = "projection_generations_empty"
	// RetirementPreconditionProjectionEntriesEmpty requires
	// session_projection_entries to be empty.
	RetirementPreconditionProjectionEntriesEmpty RetirementPrecondition = "projection_entries_empty"
	// RetirementPreconditionProjectionContentEmpty requires
	// session_projection_content to be empty.
	RetirementPreconditionProjectionContentEmpty RetirementPrecondition = "projection_content_empty"
	// RetirementPreconditionSessionEntriesFTSAbsent requires
	// session_entries_fts to be gone (search consolidation ran).
	RetirementPreconditionSessionEntriesFTSAbsent RetirementPrecondition = "session_entries_fts_absent"
	// RetirementPreconditionNoNativeFullContentRows requires no row of
	// session_entry_full_content to belong to a native session (non-native
	// rows stay; only the later move drops them).
	RetirementPreconditionNoNativeFullContentRows RetirementPrecondition = "no_native_full_content_rows"
)

// AllRetirementPreconditions is the exact closed set of five guards.
var AllRetirementPreconditions = []RetirementPrecondition{
	RetirementPreconditionProjectionGenerationsEmpty,
	RetirementPreconditionProjectionEntriesEmpty,
	RetirementPreconditionProjectionContentEmpty,
	RetirementPreconditionSessionEntriesFTSAbsent,
	RetirementPreconditionNoNativeFullContentRows,
}

// IsValid reports whether the value is a member of AllRetirementPreconditions.
func (v RetirementPrecondition) IsValid() bool {
	for _, known := range AllRetirementPreconditions {
		if v == known {
			return true
		}
	}
	return false
}

// NewRetirementPrecondition validates a raw precondition name at an input boundary.
func NewRetirementPrecondition(raw string) (RetirementPrecondition, error) {
	v := RetirementPrecondition(raw)
	if !v.IsValid() {
		return "", fmt.Errorf("store.NewRetirementPrecondition: value %q is outside the closed retirement-precondition set %v; the Release N+1 guard cannot name its failing condition; use a published precondition", raw, AllRetirementPreconditions)
	}
	return v, nil
}

func (v RetirementPrecondition) String() string { return string(v) }

// MigrateOutcome is the per-session disposition of one MigrateSession call
// (design §7.2 Phase 2d/4): converted, rolled back for a data reason and
// marked for re-index, or skipped (already converted, non-native, dry-run).
type MigrateOutcome string

const (
	// MigrateOutcomeConverted marks a session whose catalog transaction committed.
	MigrateOutcomeConverted MigrateOutcome = "converted"
	// MigrateOutcomeRolledBack marks a session whose catalog transaction rolled
	// back on a data mismatch; the session is marked for re-index.
	MigrateOutcomeRolledBack MigrateOutcome = "rolled_back"
	// MigrateOutcomeMarked marks a session recorded for re-index without conversion.
	MigrateOutcomeMarked MigrateOutcome = "marked"
	// MigrateOutcomeSkipped marks a session with no conversion work (already
	// converted, non-native, or dry-run counting).
	MigrateOutcomeSkipped MigrateOutcome = "skipped"
)

// AllMigrateOutcomes is the exact closed set of per-session dispositions.
var AllMigrateOutcomes = []MigrateOutcome{
	MigrateOutcomeConverted,
	MigrateOutcomeRolledBack,
	MigrateOutcomeMarked,
	MigrateOutcomeSkipped,
}

// IsValid reports whether the value is a member of AllMigrateOutcomes.
func (o MigrateOutcome) IsValid() bool {
	switch o {
	case MigrateOutcomeConverted, MigrateOutcomeRolledBack, MigrateOutcomeMarked, MigrateOutcomeSkipped:
		return true
	default:
		return false
	}
}

// NewMigrateOutcome validates a raw migrate outcome at an input boundary.
func NewMigrateOutcome(raw string) (MigrateOutcome, error) {
	v := MigrateOutcome(raw)
	if !v.IsValid() {
		return "", fmt.Errorf("store.NewMigrateOutcome: value %q is outside the closed migrate-outcome set %v; a migration session disposition cannot be recorded; use converted, rolled_back, marked, or skipped", raw, AllMigrateOutcomes)
	}
	return v, nil
}

func (o MigrateOutcome) String() string { return string(o) }
