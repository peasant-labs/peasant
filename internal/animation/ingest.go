package animation

import (
	"strings"
	"time"
)

// IngestAnimation returns a frame-based ASCII animation of a peasant
// carrying logs from a tree to a log pile. Displayed during transcript
// ingestion — the "log" metaphor maps to transcript logs being collected.
func IngestAnimation() *Animation {
	const (
		w = 56
		h = 5
	)

	ground := at(0, h-1, strings.Repeat("_", w))
	tree := at(1, 0,
		`  ^  `,
		` /|\ `,
		`/|||\`,
		` ||| `,
		`_|||_`,
	)
	pile := at(44, 1,
		`      [=]`,
		`   [=][=]`,
		`[=][=][=]`,
	)

	pR := func(x int) sprite { return at(x, 1, ` o `, `/|>`, `/ \`) }
	pL := func(x int) sprite { return at(x, 1, ` o `, `<|\`, `/ \`) }
	pLog := func(x int) sprite { return at(x, 1, ` o===`, `/|\ `, `/ \ `) }

	frame := func(peasant sprite) Frame {
		return compose(w, h, []sprite{ground, tree, pile, peasant})
	}

	return &Animation{
		Frames: []Frame{
			frame(pR(7)),    // at tree
			frame(pLog(8)),  // pick up log
			frame(pLog(16)), // walk right with log
			frame(pLog(24)), // walk right
			frame(pLog(32)), // walk right
			frame(pR(39)),   // at pile, drop log
			frame(pL(28)),   // walk back
			frame(pL(17)),   // walk back
		},
		Interval: 300 * time.Millisecond,
	}
}
