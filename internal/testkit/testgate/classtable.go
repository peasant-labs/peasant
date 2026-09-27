package testgate

import (
	"sort"
	"time"
)

// The per-class decision table. Wall without CPU hides blocking; CPU without
// wall hides what a core count can compress. The pair is the unit of report,
// and the derived wall − CPU gap is exactly the work more cores cannot
// compress: a class at wall ≈ CPU is a packing problem, a class whose wall
// dwarfs its CPU is blocking that points at removing or faking the setup.
//
// Basis records how a row's CPU was obtained, because it is only exactly
// attributable when the invocations that produced it ran serialized:
//   - BasisSerialized: summed getrusage deltas of serialized per-invocation
//     records. Exact per unit.
//   - BasisPassLevel: the whole-pass delta for a concurrent pass, where the
//     process-global RUSAGE_CHILDREN counter cannot attribute per unit. Exact
//     for the pass, not for any single unit.
//   - BasisStep: a single measured pre-test step, run alone. Exact.
const (
	BasisSerialized = "serialized-per-invocation"
	BasisPassLevel  = "pass-level-aggregate"
	BasisStep       = "pre-test-step"
)

// PassSummary pins one pass's wall and its whole-pass child CPU. Serialized
// reports whether the pass ran one invocation at a time, which is what makes a
// per-invocation CPU delta attributable.
type PassSummary struct {
	Pass       PassMode
	Wall       time.Duration
	User       time.Duration
	System     time.Duration
	Serialized bool
	Units      int
}

// ClassRow is one row of the per-class wall/user/system decision table.
type ClassRow struct {
	Class    string `json:"class"`
	Units    int    `json:"units"`
	WallMS   int64  `json:"wall_ms"`
	UserMS   int64  `json:"user_ms"`
	SystemMS int64  `json:"system_ms"`
	GapMS    int64  `json:"gap_ms"` // wall - (user + system); negative when CPU > wall
	Basis    string `json:"basis"`
}

// NewClassRow builds a row and derives its wall − CPU gap.
func NewClassRow(class string, units int, wall, user, system time.Duration, basis string) ClassRow {
	wallMS := wall.Milliseconds()
	userMS := user.Milliseconds()
	sysMS := system.Milliseconds()
	return ClassRow{
		Class:    class,
		Units:    units,
		WallMS:   wallMS,
		UserMS:   userMS,
		SystemMS: sysMS,
		GapMS:    wallMS - userMS - sysMS,
		Basis:    basis,
	}
}

// classAccum accumulates one class's totals while the table is built.
type classAccum struct {
	units int
	wall  time.Duration
	user  time.Duration
	sys   time.Duration
	basis string
}

// BuildClassTable groups per-invocation records by cost class and reports wall,
// user, system, and the derived gap per class.
//
// A serialized pass contributes the summed per-invocation deltas of its records
// (exact CPU per unit). A concurrent pass contributes one row per class present
// in it, with the whole-pass CPU delta as the class CPU, because the global
// RUSAGE_CHILDREN counter cannot attribute CPU to individual concurrent units.
// Pre-test steps are not pass invocations and are supplied by PreTestRows.
func BuildClassTable(passes []PassSummary, records []Record) []ClassRow {
	rows := map[string]*classAccum{}
	add := func(class string, units int, wall, user, sys time.Duration, basis string) {
		r, ok := rows[class]
		if !ok {
			r = &classAccum{}
			rows[class] = r
		}
		r.units += units
		r.wall += wall
		r.user += user
		r.sys += sys
		// Exactness never downgrades: a class that has an exact serialized row
		// stays exact even if the same class also appears pass-level.
		if r.basis != BasisSerialized {
			if basis == BasisSerialized || r.basis == "" {
				r.basis = basis
			}
		}
	}

	for _, ps := range passes {
		byClass := map[string][]Record{}
		for _, rec := range records {
			if rec.Pass == ps.Pass {
				byClass[string(rec.Class)] = append(byClass[string(rec.Class)], rec)
			}
		}
		if ps.Serialized {
			for class, recs := range byClass {
				var wall, user, sys time.Duration
				for _, rec := range recs {
					wall += rec.Wall
					user += rec.User
					sys += rec.System
				}
				add(class, len(recs), wall, user, sys, BasisSerialized)
			}
			continue
		}
		for class, recs := range byClass {
			add(class, len(recs), ps.Wall, ps.User, ps.System, BasisPassLevel)
		}
	}

	return orderClassRows(rows)
}

// orderClassRows emits rows in a stable contract order: the four registry
// classes, then the race pass, then the pre-test steps in execution order, then
// any other class name sorted lexically.
func orderClassRows(rows map[string]*classAccum) []ClassRow {
	var out []ClassRow
	seen := map[string]bool{}
	emit := func(class string) {
		r, ok := rows[class]
		if !ok || seen[class] {
			return
		}
		seen[class] = true
		out = append(out, NewClassRow(class, r.units, r.wall, r.user, r.sys, r.basis))
	}
	for _, c := range RegistryClasses {
		emit(string(c))
	}
	emit(string(ClassRace))
	for _, s := range PreTestSteps {
		emit(string(PreTestClass(s)))
	}
	var rest []string
	for class := range rows {
		if !seen[class] {
			rest = append(rest, class)
		}
	}
	sort.Strings(rest)
	for _, class := range rest {
		emit(class)
	}
	return out
}

// PreTestRows converts measured pre-test steps into decision-table rows. Each
// step is a single serialized unit, so its CPU is exact.
func PreTestRows(steps []StepMeasurement) []ClassRow {
	out := make([]ClassRow, 0, len(steps))
	for _, s := range steps {
		out = append(out, NewClassRow(string(PreTestClass(s.Step)), 1, s.Wall, s.User, s.System, BasisStep))
	}
	return out
}
