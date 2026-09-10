package defaults

import "fmt"

// HumanByteSize renders a byte count the way every reader-facing note and
// diagnostic states it.
//
// There is ONE of these on purpose. Two notes can sit next to each other in the
// same transcript, one saying a record was bounded for display and the other
// saying a record was omitted, and a reader comparing "8 KiB" with "8.0 KiB"
// has to work out whether the two notes are even talking about the same scale.
// The sizes this reports are the ones bounded against the limits in this
// package, so the rendering belongs beside them.
//
// Every unit carries one decimal place, which is what a bound needs: the
// shown and recorded sizes of one record are often within the same unit. A
// value that would render as "1024.0" is reported in the next unit instead, so
// a size one byte under a mebibyte never reads as more than a mebibyte.
func HumanByteSize(size int64) string {
	const unit = 1 << 10
	if size < unit {
		if size < 0 {
			size = 0
		}
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for index, suffix := range units {
		value /= unit
		if value < 1023.95 || index == len(units)-1 {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB", value)
}
