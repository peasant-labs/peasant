package ingest

import "math"

const millisecondsPerMinute = 60000

// DurationMinutesToMs rounds a duration to the storage precision.
func DurationMinutesToMs(minutes float64) int64 {
	return int64(math.Round(minutes * millisecondsPerMinute))
}

// DurationMsToMinutes returns the value exposed by the duration readers.
func DurationMsToMinutes(ms int64) float64 {
	return float64(ms) / millisecondsPerMinute
}

// CanonicalDurationMinutes returns the minutes value retained by storage.
func CanonicalDurationMinutes(minutes float64) float64 {
	return DurationMsToMinutes(DurationMinutesToMs(minutes))
}
