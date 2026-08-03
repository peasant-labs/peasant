package pull

import "time"

// SystemClock is the production Clock backed by the wall clock. Tests inject a
// fixed clock instead so manifest/pulled_at timestamps are deterministic.
type SystemClock struct{}

var _ Clock = SystemClock{}

// NowUnixMilli returns the current time in unix milliseconds.
func (SystemClock) NowUnixMilli() int64 { return time.Now().UnixMilli() }
