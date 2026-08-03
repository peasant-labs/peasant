package animation

import (
	"strings"
	"time"
)

// PushAnimation returns a frame-based ASCII animation of a peasant
// carrying boxes from a house to a village. Displayed during uploads
// to the marketplace — the "village" metaphor maps to the community.
func PushAnimation() *Animation {
	const (
		w = 56
		h = 6
	)

	ground := at(0, h-1, strings.Repeat("_", w))
	house := at(1, 1,
		`  /\  `,
		` /  \ `,
		`| [] |`,
		`|____|`,
		`______`,
	)
	village := at(42, 1,
		` /\  /\  /\ `,
		`|  ||  ||  |`,
		`|[]||[]||[]|`,
		`|__||__||__|`,
		`____________`,
	)

	pR := func(x int) sprite { return at(x, 2, ` o `, `/|>`, `/ \`) }
	pL := func(x int) sprite { return at(x, 2, ` o `, `<|\`, `/ \`) }
	pBox := func(x int) sprite { return at(x, 2, `[#]`, `\o/`, `/ \`) }

	frame := func(peasant sprite) Frame {
		return compose(w, h, []sprite{ground, house, village, peasant})
	}

	return &Animation{
		Frames: []Frame{
			frame(pR(8)),    // at house
			frame(pBox(9)),  // pick up box
			frame(pBox(17)), // walk right with box
			frame(pBox(25)), // walk right
			frame(pBox(33)), // walk right
			frame(pR(38)),   // at village, drop box
			frame(pL(27)),   // walk back
			frame(pL(17)),   // walk back
		},
		Interval: 300 * time.Millisecond,
	}
}
