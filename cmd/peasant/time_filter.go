package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// timeFilterHelp is the grammar every --since/--until/--before/--after flag
// accepts, for flag help text.
const timeFilterHelp = "Nh, Nd, Nw, Nmo (months), Ny, or YYYY-MM-DD (UTC); minutes are not supported"

// parseTimeFilter is the one parser behind every --since/--until/--before/
// --after flag: a date (YYYY-MM-DD or RFC3339) or a relative "N ago" with
// unit h, d, w, m (months) or y. now anchors relative values.
func parseTimeFilter(s string, now time.Time) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return parseRelativeAgo(s, now)
}

// parseRelativeAgo parses "N<unit>" and returns now minus that span.
func parseRelativeAgo(s string, now time.Time) (time.Time, error) {
	if len(s) < 2 {
		return time.Time{}, fmt.Errorf("invalid duration: %q (expected %s)", s, timeFilterHelp)
	}
	number, unit := s[:len(s)-1], s[len(s)-1]
	if strings.HasSuffix(s, "mo") {
		number, unit = s[:len(s)-2], 'm'
	}
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 {
		return time.Time{}, fmt.Errorf("invalid duration: %q (number must be positive integer; expected %s)", s, timeFilterHelp)
	}
	switch unit {
	case 'h':
		return now.Add(-time.Duration(n) * time.Hour), nil
	case 'd':
		return now.AddDate(0, 0, -n), nil
	case 'w':
		return now.AddDate(0, 0, -7*n), nil
	case 'm':
		return now.AddDate(0, -n, 0), nil
	case 'y':
		return now.AddDate(-n, 0, 0), nil
	default:
		return time.Time{}, fmt.Errorf("invalid duration unit %q in %q (expected %s)", string(unit), s, timeFilterHelp)
	}
}

// parsePruneTimeFilter is parseTimeFilter for the destructive prune command,
// where a bare "m" is refused: people read 30m as minutes, the grammar reads
// it as months, and prune would delete accordingly.
func parsePruneTimeFilter(s string, now time.Time) (time.Time, error) {
	if strings.HasSuffix(s, "m") && len(s) > 1 {
		if _, err := strconv.Atoi(s[:len(s)-1]); err == nil {
			return time.Time{}, fmt.Errorf("%q is ambiguous for prune: write %so for months (minutes are not supported); nothing was selected", s, s)
		}
	}
	return parseTimeFilter(s, now)
}
