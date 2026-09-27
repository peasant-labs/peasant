// Package teststream parses a `go test -json` stream into terminal test
// records and renders the ranked per-test and per-family report.
//
// It is the stream core of `cmd/testgate`: the `run` mode merges the gate's
// passes and applies the exactly-once screen, and the `timing` mode summarizes
// an arbitrary stream. Keeping the parse in one place means a stream shape
// change is fixed once.
package teststream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Event is one line of `go test -json` output.
//
// The stream interleaves run/pass/fail/pause/output events for every test and
// subtest, so consumers key on the terminal events and use Elapsed for the
// duration.
type Event struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
	Time    string  `json:"Time"`
}

// Record is a completed top-level test or subtest.
type Record struct {
	Package string
	Test    string
	Elapsed time.Duration
	Skipped bool
	Failed  bool
}

// ParseStream reads a `go test -json` stream and returns one record per
// (package, test) terminal event, with retried attempts collapsed to the final
// attempt.
//
// A test that fails then passes is reported as the single passing run it ended
// as: Go's test2json emits a fresh run for every retry, and charging both would
// inflate a total while reporting the first attempt would flag a recovered
// flake as a real failure.
func ParseStream(r io.Reader) ([]Record, error) {
	var records []Record
	at := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
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
		rec := Record{
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
		return nil, fmt.Errorf("read go test -json stream: %w", err)
	}
	return records, nil
}

// TopLevel returns the records for top-level tests only. A subtest's time is
// contained in its parent's, so a total over all records would double count.
func TopLevel(records []Record) []Record {
	top := make([]Record, 0, len(records))
	for _, r := range records {
		if !strings.Contains(r.Test, "/") {
			top = append(top, r)
		}
	}
	return top
}

// Failing returns the failed records, in stream order.
func Failing(records []Record) []Record {
	var out []Record
	for _, r := range records {
		if r.Failed {
			out = append(out, r)
		}
	}
	return out
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
func aggregate(records []Record, key func(Record) string) ([]string, map[string]float64, map[string]int) {
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

// ReportOptions controls the rendered report.
type ReportOptions struct {
	Top        int
	FamilyRe   *regexp.Regexp
	NoFamilies bool
	WarnPct    float64
}

// Report renders the ranked report for records to w.
//
// It returns an error if there is no test event, so a broken pipe is a hard
// error rather than a silent "0.0s total".
func Report(w io.Writer, records []Record, opts ReportOptions) error {
	if len(records) == 0 {
		return fmt.Errorf("no test events on the stream")
	}
	topLevel := TopLevel(records)
	var total float64
	for _, r := range topLevel {
		total += r.Elapsed.Seconds()
	}

	fmt.Fprintf(w, "test-timing: %d top-level tests, %d subtests, %.1fs total (sum of top-level tests)\n",
		len(topLevel), len(records)-len(topLevel), total)
	if len(records) > len(topLevel) {
		fmt.Fprintf(w, "note: %d subtests are contained in their parents and are NOT added to the total\n\n",
			len(records)-len(topLevel))
	}

	keys, sums, counts := aggregate(topLevel, func(r Record) string { return r.Package })
	fmt.Fprintln(w, "== by package ==")
	fmt.Fprintf(w, "%-10s %8s %6s  %s\n", "TIME", "SHARE", "TESTS", "PACKAGE")
	for i, k := range keys {
		if i >= opts.Top {
			fmt.Fprintf(w, "... %d more packages\n", len(keys)-i)
			break
		}
		fmt.Fprintf(w, "%9.1fs %7.1f%% %6d  %s\n", sums[k], 100*sums[k]/total, counts[k], k)
	}
	fmt.Fprintln(w)

	keys, sums, counts = aggregate(topLevel, func(r Record) string { return r.Test })
	fmt.Fprintln(w, "== slowest top-level tests ==")
	fmt.Fprintf(w, "%-10s %8s %6s  %s\n", "TIME", "SHARE", "SUBT", "TEST")
	for i, k := range keys {
		if i >= opts.Top {
			fmt.Fprintf(w, "... %d more tests\n", len(keys)-i)
			break
		}
		share := 100 * sums[k] / total
		mark := ""
		if share >= opts.WarnPct {
			mark = "  <-- over -warn-pct"
		}
		fmt.Fprintf(w, "%9.2fs %7.1f%% %6d  %s%s\n", sums[k], share, counts[k], k, mark)
	}
	fmt.Fprintln(w)

	if !opts.NoFamilies {
		// Families are computed over top-level AND subtest records: a family is
		// often only visible through its subtests. The per-family totals
		// therefore intentionally exceed the top-level total and are a ranking
		// aid, not a budget.
		keys, sums, counts = aggregate(records, func(r Record) string { return familyOf(r.Test, opts.FamilyRe) })
		fmt.Fprintln(w, "== by family (subtest-derived; ranks, not a budget) ==")
		fmt.Fprintf(w, "%-10s %8s  %s\n", "TIME", "RECORDS", "FAMILY")
		for i, k := range keys {
			if i >= opts.Top {
				fmt.Fprintf(w, "... %d more families\n", len(keys)-i)
				break
			}
			fmt.Fprintf(w, "%9.2fs %8d  %s\n", sums[k], counts[k], k)
		}
		fmt.Fprintln(w)
	}
	return nil
}
