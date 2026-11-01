package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// timeFilterHelp is the grammar every --since/--until/--before/--after flag
// accepts, for flag help text.
const timeFilterHelp = "positive Go duration (e.g. 30m, 1h30m), integer d, w, mo (months), y, YYYY-MM-DD (UTC), or RFC3339"

// parseTimeFilter is the one parser behind every --since/--until/--before/
// --after flag: a date (YYYY-MM-DD or RFC3339) or a relative "N ago" with
// fixed durations or integer calendar units d, w, mo and y. now anchors relative values.
func parseTimeFilter(s string, now time.Time) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return parseRelativeAgo(s, now)
}

// parseRelativeAgo parses a fixed duration or integer calendar span and subtracts it from now.
func parseRelativeAgo(s string, now time.Time) (time.Time, error) {
	invalid := func() (time.Time, error) {
		return time.Time{}, fmt.Errorf("invalid duration %q (expected %s)", s, timeFilterHelp)
	}
	if s == "" || s[0] == '-' || s[0] == '+' {
		return invalid()
	}
	for _, unit := range []string{"mo", "d", "w", "y"} {
		if !strings.HasSuffix(s, unit) {
			continue
		}
		number := strings.TrimSuffix(s, unit)
		for _, r := range number {
			if r < '0' || r > '9' {
				return invalid()
			}
		}
		n, err := strconv.Atoi(number)
		if err != nil || n <= 0 || n > 3652058 {
			return invalid()
		}
		var result time.Time
		switch unit {
		case "d":
			result = now.AddDate(0, 0, -n)
		case "w":
			if n > 3652058/7 {
				return invalid()
			}
			result = now.AddDate(0, 0, -7*n)
		case "mo":
			if n > 9999*12 {
				return invalid()
			}
			result = now.AddDate(0, -n, 0)
		case "y":
			if n > 9999 {
				return invalid()
			}
			result = now.AddDate(-n, 0, 0)
		}
		if result.Year() < 1 || !result.Before(now) {
			return invalid()
		}
		return result, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return invalid()
	}
	return now.Add(-d), nil
}
