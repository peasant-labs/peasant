// Command test-timing summarizes a `go test -json` stream into a ranked
// per-test and per-family wall-time report.
//
// It exists so the suite's cost is measured, not guessed. TESTING.md records how
// `cmd/peasant` was taken from 86.9s to 19.6s under -race; this tool is the
// reusable measurement for the packages that are still slow.
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
//
// The parse and the report live in internal/teststream; this command is a thin
// CLI over that library so a gate run and an ad-hoc run share one implementation.
// Retirement condition: if the gate ever grows a mode that accepts an arbitrary
// `go test -json` file, this CLI is deleted — the capability, not the tool, is
// what is retained.
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"

	"github.com/peasant-labs/peasant/internal/teststream"
)

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

	records, err := teststream.ParseStream(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test-timing: %v\n", err)
		os.Exit(1)
	}
	if len(records) == 0 {
		fmt.Fprintln(os.Stderr, "test-timing: no test events on stdin (did you pipe `go test -json`?)")
		os.Exit(1)
	}
	if err := teststream.Report(os.Stdout, records, teststream.ReportOptions{
		Top:        *top,
		FamilyRe:   re,
		NoFamilies: *noFamilies,
		WarnPct:    *warnPct,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "test-timing: %v\n", err)
		os.Exit(1)
	}

	// Non-zero exit on failure so this can gate CI directly.
	for _, r := range teststream.Failing(records) {
		fmt.Fprintf(os.Stderr, "test-timing: FAILING TEST: %s (%s)\n", r.Test, r.Package)
		os.Exit(1)
	}
}
