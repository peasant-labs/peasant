// Command test-timing summarizes a `go test -json` stream into a ranked
// per-test and per-family wall-time report.
//
// It exists so the suite's cost is measured, not guessed. TESTING.md records how
// `cmd/peasant` was taken from 86.9s to 19.6s under -race; this tool is the
// reusable measurement for the packages that are still slow (see peasant#389).
//
// Usage:
//
//	go test -race -json ./internal/ingest/... | go run ./scripts/test-timing
//	go run ./scripts/test-timing -top 40 < ingest.json
//
// Flags:
//
//	-top N         how many rows to print per section (default 25)
//	-family-re RE  regroup rows by this regexp instead of the default family
//	-no-families   skip the per-family section
//	-warn-pct PCT  flag tests whose share of total elapsed time exceeds PCT
//
// The family of a test is derived from its subtest name, so the expensive
// families are visible without hand-maintaining a mapping.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// event is one line of `go test -json` output.
//
// The stream interleaves run/pass/fail/pause/output events for every test and
// subtest, so the summarizer keys on the terminal events and uses Elapsed for
// the duration.
type event struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
}

// record is a completed top-level test or subtest.
type record struct {
	Package string
	Test    string
	Elapsed time.Duration
	Skipped bool
	Failed  bool
}

// familyOf extracts a coarse family name from a test path.
//
// Subtest names are the only grouping signal the JSON stream carries, so the
// leading segment of the subtest is used: "TestFoo/bar/baz" -> "bar". Top-level
// tests without subtests fall back to the leading identifier of the test name,
// which keeps families stable as subtests are added.
func familyOf(test string, re *regexp.Regexp) string {
	if re != nil {
		if m := re.FindStringSubmatch(test); m != nil {
			return m[1]
		}
	}
	if i := strings.Index(test, "/"); i >= 0 {
		rest := test[i+1:]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	name := strings.TrimPrefix(test, "Test")
	var b strings.Builder
	for _, r := range name {
		if r == '_' || r >= '0' && r <= '9' {
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "(toplevel)"
	}
	return b.String()
}

// aggregate sums records by key.
func aggregate(records []record, key func(record) string) ([]string, map[string]float64, map[string]int) {
	sums := map[string]float64{}
	counts := map[string]int{}
	for _, r := range records {
		k := key(r)
		sums[k] += r.Elapsed.Seconds()
		counts[k]++
	}
	keys := make([]string, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if sums[keys[i]] != sums[keys[j]] {
			return sums[keys[i]] > sums[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys, sums, counts
}

func main() {
	top := flag.Int("top", 25, "rows to print per section")
	familyRe := flag.String("family-re", "", "regexp with one capture group; overrides default family grouping")
	noFamilies := flag.Bool("no-families", false, "skip the per-family section")
	warnPct := flag.Float64("warn-pct", 5, "flag tests whose share of elapsed time exceeds this percentage")
	flag.Parse()

	var re *regexp.Regexp
	if *familyRe != "" {
		var err error
		re, err = regexp.Compile(*familyRe)
		if err != nil {
			fmt.Fprintf(os.Stderr, "test-timing: bad -family-re: %v\n", err)
			os.Exit(2)
		}
	}

	// Streaming parse: a large -race run produces a multi-megabyte stream and
	// there is no reason to hold the raw events.
	var records []record
	// index by package|test so a retried test replaces its earlier attempt
	// instead of being counted twice. Go's test2json emits a fresh run for every
	// retry, and a test that fails then passes must be reported as the single
	// passing run it ended as.
	at := map[string]int{}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			continue // non-event lines (build output, panics) are not fatal
		}
		if e.Test == "" {
			continue // package-level event
		}
		if e.Action != "pass" && e.Action != "fail" && e.Action != "skip" {
			continue
		}
		key := e.Package + "|" + e.Test
		rec := record{
			Package: e.Package,
			Test:    e.Test,
			Elapsed: time.Duration(e.Elapsed * float64(time.Second)),
			Skipped: e.Action == "skip",
			Failed:  e.Action == "fail",
		}
		if i, ok := at[key]; ok {
			records[i] = rec
			continue
		}
		at[key] = len(records)
		records = append(records, rec)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "test-timing: read: %v\n", err)
		os.Exit(1)
	}
	if len(records) == 0 {
		fmt.Fprintln(os.Stderr, "test-timing: no test events on stdin (did you pipe `go test -json`?)")
		os.Exit(1)
	}

	// Total is the sum of top-level tests only. Summing subtests too would
	// double count, since a subtest's time is contained in its parent's.
	var topLevel []record
	for _, r := range records {
		if !strings.Contains(r.Test, "/") {
			topLevel = append(topLevel, r)
		}
	}
	var total float64
	for _, r := range topLevel {
		total += r.Elapsed.Seconds()
	}

	fmt.Printf("test-timing: %d top-level tests, %d subtests, %.1fs total (sum of top-level tests)\n",
		len(topLevel), len(records)-len(topLevel), total)
	if len(records) > len(topLevel) {
		fmt.Printf("note: %d subtests are contained in their parents and are NOT added to the total\n\n",
			len(records)-len(topLevel))
	}

	keys, sums, counts := aggregate(topLevel, func(r record) string { return r.Package })
	fmt.Println("== by package ==")
	fmt.Printf("%-10s %8s %6s  %s\n", "TIME", "SHARE", "TESTS", "PACKAGE")
	for i, k := range keys {
		if i >= *top {
			fmt.Printf("... %d more packages\n", len(keys)-i)
			break
		}
		fmt.Printf("%9.1fs %7.1f%% %6d  %s\n", sums[k], 100*sums[k]/total, counts[k], k)
	}
	fmt.Println()

	keys, sums, counts = aggregate(topLevel, func(r record) string { return r.Test })
	fmt.Println("== slowest top-level tests ==")
	fmt.Printf("%-10s %8s %6s  %s\n", "TIME", "SHARE", "SUBT", "TEST")
	for i, k := range keys {
		if i >= *top {
			fmt.Printf("... %d more tests\n", len(keys)-i)
			break
		}
		share := 100 * sums[k] / total
		mark := ""
		if share >= *warnPct {
			mark = "  <-- over -warn-pct"
		}
		fmt.Printf("%9.2fs %7.1f%% %6d  %s%s\n", sums[k], share, counts[k], k, mark)
	}
	fmt.Println()

	if !*noFamilies {
		// Families are computed over top-level AND subtest records: a family is
		// often only visible through its subtests. The per-family totals
		// therefore intentionally exceed the top-level total and are a ranking
		// aid, not a budget.
		keys, sums, counts = aggregate(records, func(r record) string { return familyOf(r.Test, re) })
		fmt.Println("== by family (subtest-derived; ranks, not a budget) ==")
		fmt.Printf("%-10s %8s  %s\n", "TIME", "RECORDS", "FAMILY")
		for i, k := range keys {
			if i >= *top {
				fmt.Printf("... %d more families\n", len(keys)-i)
				break
			}
			fmt.Printf("%9.2fs %8d  %s\n", sums[k], counts[k], k)
		}
		fmt.Println()
	}

	// Non-zero exit on failure so this can gate CI directly.
	for _, r := range records {
		if r.Failed {
			fmt.Fprintf(os.Stderr, "test-timing: FAILING TEST: %s (%s)\n", r.Test, r.Package)
			os.Exit(1)
		}
	}
}
