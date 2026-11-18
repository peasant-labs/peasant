//go:build skipbudget

// Command skip-budget lists the isolated measurement matrix. Measurement is
// unavailable until the planned skip and its stored profile stages exist.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/upgrade"
)

type matrixCase struct {
	Name    string `json:"name"`
	Shape   string `json:"shape"`
	DataDir string `json:"dataDir"`
}

type report struct {
	Provenance upgrade.BudgetProvenance `json:"provenance"`
	Cases      []matrixCase             `json:"cases"`
}

func main() {
	list := flag.Bool("list", false, "print the planned cases without running them")
	flag.Parse()
	if !*list {
		fmt.Fprintln(os.Stderr, "skip-budget: measurement is not yet available; use -list to inspect the planned matrix")
		os.Exit(2)
	}
	cases := []matrixCase{
		{Name: "C1", Shape: "stamp-only adapter bump: 240 x 1000"},
		{Name: "C2", Shape: "forced context change: 240 x 1000"},
		{Name: "C3", Shape: "indexer bump: 240 x 1000"},
		{Name: "C4", Shape: "new session: 2 x 10"},
		{Name: "C5", Shape: "legacy row: C1 without seal"},
		{Name: "C6", Shape: "unknown producer: test build"},
	}
	for i := range cases {
		cases[i].DataDir = filepath.Join("/tmp/opencode/peasant-1034", cases[i].Name)
	}
	if err := json.NewEncoder(os.Stdout).Encode(report{Cases: cases}); err != nil {
		fmt.Fprintf(os.Stderr, "skip-budget: writing matrix to stdout failed: %v\n", err)
		os.Exit(1)
	}
}
