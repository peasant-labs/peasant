package store

import (
	"fmt"
	"io"
	"time"
)

// UpgradeStepKind names what an upgrade step changes. The set is closed.
type UpgradeStepKind string

const (
	// UpgradeStepSchema is one migration of the schema chain, with any data
	// fill that commits in the same transaction.
	UpgradeStepSchema UpgradeStepKind = "schema"
	// UpgradeStepSearch is a search index build that is not a schema version.
	// A step of this kind carries Version 0.
	UpgradeStepSearch UpgradeStepKind = "search"
)

// AllUpgradeStepKinds is the closed step-kind set, in declaration order.
var AllUpgradeStepKinds = []UpgradeStepKind{UpgradeStepSchema, UpgradeStepSearch}

// NewUpgradeStepKind validates a raw step kind at an input boundary.
func NewUpgradeStepKind(raw string) (UpgradeStepKind, error) {
	for _, kind := range AllUpgradeStepKinds {
		if string(kind) == raw {
			return kind, nil
		}
	}
	return "", fmt.Errorf("store.NewUpgradeStepKind: value %q is outside the closed upgrade step kind set %v; the step cannot be planned or reported; use one of the listed kinds", raw, AllUpgradeStepKinds)
}

func (k UpgradeStepKind) String() string { return string(k) }

// UpgradeCost is the cost class of an upgrade step. The set is closed.
type UpgradeCost string

const (
	// UpgradeCostQuick is a step whose time does not depend on how much the
	// store holds.
	UpgradeCostQuick UpgradeCost = "quick"
	// UpgradeCostScalesWithRows is a step that visits stored rows, such as a
	// table rebuild or a per-session data fill.
	UpgradeCostScalesWithRows UpgradeCost = "scales_with_rows"
	// UpgradeCostScalesWithText is a step that reads the stored text, such as
	// a rebuild of the search index.
	UpgradeCostScalesWithText UpgradeCost = "scales_with_text"
)

// AllUpgradeCosts is the closed cost set, cheapest first.
var AllUpgradeCosts = []UpgradeCost{UpgradeCostQuick, UpgradeCostScalesWithRows, UpgradeCostScalesWithText}

// NewUpgradeCost validates a raw cost class at an input boundary.
func NewUpgradeCost(raw string) (UpgradeCost, error) {
	for _, cost := range AllUpgradeCosts {
		if string(cost) == raw {
			return cost, nil
		}
	}
	return "", fmt.Errorf("store.NewUpgradeCost: value %q is outside the closed upgrade cost set %v; the step cannot be classified as quick or long; use one of the listed costs", raw, AllUpgradeCosts)
}

func (c UpgradeCost) String() string { return string(c) }

// Long reports whether a step of this cost can take minutes on a large
// store. Every cost except quick is long.
func (c UpgradeCost) Long() bool { return c != UpgradeCostQuick }

// UpgradeUnit is what a step counts while it runs. The set is closed; the
// empty unit means the step cannot report a count.
type UpgradeUnit string

const (
	UpgradeUnitNone     UpgradeUnit = ""
	UpgradeUnitSessions UpgradeUnit = "sessions"
	UpgradeUnitBytes    UpgradeUnit = "bytes"
)

// AllUpgradeUnits is the closed unit set.
var AllUpgradeUnits = []UpgradeUnit{UpgradeUnitNone, UpgradeUnitSessions, UpgradeUnitBytes}

// NewUpgradeUnit validates a raw unit at an input boundary.
func NewUpgradeUnit(raw string) (UpgradeUnit, error) {
	for _, unit := range AllUpgradeUnits {
		if string(unit) == raw {
			return unit, nil
		}
	}
	return "", fmt.Errorf("store.NewUpgradeUnit: value %q is outside the closed upgrade unit set %q; the step's count cannot be labelled; use the empty unit, sessions or bytes", raw, AllUpgradeUnits)
}

func (u UpgradeUnit) String() string { return string(u) }

// UpgradeEventKind names the moment an UpgradeEvent reports. The set is
// closed.
type UpgradeEventKind string

const (
	// UpgradeEventStepStarted is sent once, before a step's transaction
	// begins.
	UpgradeEventStepStarted UpgradeEventKind = "step_started"
	// UpgradeEventStepProgress is sent zero or more times while a step runs.
	UpgradeEventStepProgress UpgradeEventKind = "step_progress"
	// UpgradeEventStepFinished is sent once, after a step's transaction
	// commits. A step that fails or is cancelled sends no finished event.
	UpgradeEventStepFinished UpgradeEventKind = "step_finished"
)

// AllUpgradeEventKinds is the closed event-kind set, in the order a step
// sends them.
var AllUpgradeEventKinds = []UpgradeEventKind{UpgradeEventStepStarted, UpgradeEventStepProgress, UpgradeEventStepFinished}

// NewUpgradeEventKind validates a raw event kind at an input boundary.
func NewUpgradeEventKind(raw string) (UpgradeEventKind, error) {
	for _, kind := range AllUpgradeEventKinds {
		if string(kind) == raw {
			return kind, nil
		}
	}
	return "", fmt.Errorf("store.NewUpgradeEventKind: value %q is outside the closed upgrade event kind set %v; the event cannot be rendered; use one of the listed kinds", raw, AllUpgradeEventKinds)
}

func (k UpgradeEventKind) String() string { return string(k) }

// UpgradeStep is one step of an upgrade plan.
type UpgradeStep struct {
	// Version is the schema version this step produces; 0 for a search step.
	Version int
	Kind    UpgradeStepKind
	// Title is lowercase and reader-facing: "rebuild the search index".
	Title string
	Cost  UpgradeCost
	// Counts is the unit the step reports in its progress events, or the
	// empty unit when it cannot count.
	Counts UpgradeUnit
}

// UpgradePlan is what an upgrade of one store would do. PlanUpgrade builds
// it without changing a file.
type UpgradePlan struct {
	DBPath string
	// From is the store's schema version and To is the version this build
	// needs. From == To means the plan has no steps.
	From, To int
	Steps    []UpgradeStep
	// StoreBytes is the size of the database file.
	StoreBytes int64
	// FreeBytes is the space available on the store's filesystem. It is
	// meaningful only when FreeBytesKnown is true.
	FreeBytes      int64
	FreeBytesKnown bool
	// RequiredFreeBytes is the free space the plan's long steps ask for. It
	// is zero when every step is quick.
	RequiredFreeBytes int64
}

// HasLongStep reports whether any step of the plan can take minutes.
func (p UpgradePlan) HasLongStep() bool {
	for _, step := range p.Steps {
		if step.Cost.Long() {
			return true
		}
	}
	return false
}

// UpgradeEvent is one progress report of a running upgrade. The type is the
// contract between the upgrade and whatever renders it: a plain line per
// step today, an interactive view later. Events for one upgrade arrive one
// at a time, never concurrently, in this order for each step: one
// step_started, zero or more step_progress, then one step_finished. A step
// that fails or is cancelled ends without a step_finished event.
type UpgradeEvent struct {
	Kind UpgradeEventKind
	Step UpgradeStep
	// Index is the 1-based position of the step in the plan, and Total is
	// the number of steps in the plan.
	Index int
	Total int
	// Done and Of count the step's work in Unit. Both are 0 when the step
	// cannot count.
	Done, Of int64
	Unit     UpgradeUnit
	// WALBytes is how much the write-ahead log grew while this step ran,
	// read from the size of the -wal file. It is 0 when the log did not grow.
	WALBytes int64
	// Elapsed is the time since the step started.
	Elapsed time.Duration
}

// UpgradeOptions configures Upgrade. The zero value runs a silent upgrade.
type UpgradeOptions struct {
	// Progress receives every UpgradeEvent. Nil is allowed.
	Progress func(UpgradeEvent)
	// Notices receives the upgrade's one-line notices and warnings: the
	// wait for another run's lock, unknown free disk, and the search index
	// rebuild. Nil discards them. A failed write is ignored: notices are
	// advisory.
	Notices io.Writer
	// Consent gates the one breaking historical step, the harness
	// identifier rename, on a store that holds sessions. Nil runs it without
	// asking. When the callback agrees, the upgrade copies the database
	// beside itself first.
	Consent MigrationConsent

	// afterDataFill is a nil production hook. A test sets it to fail or
	// cancel inside a step's transaction, after the step's data fill and
	// before its commit.
	afterDataFill func(version int) error
}

// UpgradeStepResult is one finished step of an upgrade.
type UpgradeStepResult struct {
	Step     UpgradeStep
	Duration time.Duration
	// WALBytes is how much the write-ahead log grew while the step ran.
	WALBytes int64
}

// UpgradeReport is what an upgrade did. It is printed, never stored. On an
// error it still lists the steps that finished: those stay applied.
type UpgradeReport struct {
	From, To int
	Steps    []UpgradeStepResult
}

// SchemaBehindError is what Open returns for a store that holds data and is
// behind the schema this build needs. Open changes nothing; Upgrade is the
// only function that applies migrations.
type SchemaBehindError struct {
	DBPath   string
	From, To int
}

func (e *SchemaBehindError) Error() string {
	return fmt.Sprintf("store: open %s: the store is at schema %d and this build needs %d, so the open was refused. "+
		"why: a store that holds data is upgraded only by the explicit store upgrade, never as a side effect of another command. "+
		"where: store open, before any connection wrote to the database. "+
		"when: at open; nothing was changed. "+
		"meaning: the command did not run; stored sessions and history are unchanged. "+
		"fix: run `peasant migrate --schema-only` in a terminal, then run the command again",
		e.DBPath, e.From, e.To)
}

// UpgradeDiskError is the refusal of an upgrade whose long steps need more
// free disk than the filesystem reports. It is returned before the first
// step, so nothing was changed.
type UpgradeDiskError struct {
	DBPath        string
	FreeBytes     int64
	RequiredBytes int64
}

func (e *UpgradeDiskError) Error() string {
	return fmt.Sprintf("store: upgrade %s: the filesystem reports %d bytes free and the upgrade asks for %d, so the upgrade was refused. "+
		"why: a long step writes its changes to the write-ahead log and the database before it can commit, and running out of disk inside a step would roll it back after the wait. "+
		"where: free-disk check, before the first step. "+
		"when: before any step ran; nothing was changed. "+
		"meaning: the store stays at its current schema, and commands that need the new schema will not run. "+
		"fix: free at least %d bytes on the filesystem that holds the store, then run `peasant migrate --schema-only` again",
		e.DBPath, e.FreeBytes, e.RequiredBytes, e.RequiredBytes-e.FreeBytes)
}

// UpgradeInterruptedError reports an upgrade that stopped because its
// context was cancelled. The step that was running rolled back; the steps
// before it stay applied.
type UpgradeInterruptedError struct {
	DBPath string
	// Step is the step that was running, or about to run, when the upgrade
	// stopped.
	Step UpgradeStep
	// Finished is the number of steps that committed before the stop, and
	// Total the number of steps in the plan.
	Finished, Total int
	// At is the schema version the store is left at.
	At    int
	Cause error
}

func (e *UpgradeInterruptedError) Error() string {
	return fmt.Sprintf("store: upgrade %s: stopped during step %d of %d (%s): %v. "+
		"why: the upgrade was cancelled. "+
		"where: the step's transaction, which rolled back. "+
		"when: after %d finished step(s); those stay applied and the store is at schema %d. "+
		"meaning: the store is consistent but still behind, and commands that need the new schema will not run. "+
		"fix: run `peasant migrate --schema-only` again; it resumes at the step that was stopped",
		e.DBPath, e.Finished+1, e.Total, e.Step.Title, e.Cause, e.Finished, e.At)
}

func (e *UpgradeInterruptedError) Unwrap() error { return e.Cause }
