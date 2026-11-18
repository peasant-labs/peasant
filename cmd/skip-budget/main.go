//go:build skipbudget

// Command skip-budget lists the matrix and runs the new-session smoke case.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

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

type smokeReport struct {
	Provenance upgrade.BudgetProvenance `json:"provenance"`
	Case       string                   `json:"case"`
	SamplesMs  []int64                  `json:"samplesMs"`
	MedianMs   int64                    `json:"medianMs"`
	MinMs      int64                    `json:"minMs"`
	MaxMs      int64                    `json:"maxMs"`
	Note       string                   `json:"note"`
}

func main() {
	list := flag.Bool("list", false, "print the planned cases without running them")
	caseName := flag.String("case", "", "case to run (C4 only)")
	base := flag.String("base", "", "comparison base revision")
	flag.Parse()
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
	if !*list {
		if *caseName != "C4" || strings.TrimSpace(*base) == "" || strings.ContainsAny(*base, " \t\n") {
			fmt.Fprintln(os.Stderr, "skip-budget: select -case C4 and supply a base revision without whitespace; other cases are not implemented")
			os.Exit(2)
		}
		command := "go run -tags skipbudget ./cmd/skip-budget -case C4 -base " + *base
		if err := runSmoke(cases[3].DataDir, *base, command); err != nil {
			fmt.Fprintf(os.Stderr, "skip-budget: C4 failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := json.NewEncoder(os.Stdout).Encode(report{Cases: cases}); err != nil {
		fmt.Fprintf(os.Stderr, "skip-budget: writing matrix to stdout failed: %v\n", err)
		os.Exit(1)
	}
}

func runSmoke(dir, base, command string) error {
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("read branch head: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create isolated case directory %s (remove or archive an earlier run first): %w", dir, err)
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	samples := make([]int64, 0, 5)
	for i := range 5 {
		sampleDir := filepath.Join(dir, fmt.Sprintf("sample-%d", i+1))
		cmd := exec.Command("go", "test", "-tags", "managed_refresh_bench", "./internal/ingest", "-run", "^TestManagedRefreshPrepare$", "-count=1")
		cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "PEASANT_MANAGED_REFRESH_PREPARE="+sampleDir,
			"PEASANT_MANAGED_REFRESH_SESSIONS=2", "PEASANT_MANAGED_REFRESH_ENTRIES=10", "PEASANT_MANAGED_REFRESH_CASE=stamp-only")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("prepare sample %d: %w: %s", i+1, err, output)
		}
		data, err := os.ReadFile(filepath.Join(sampleDir, "shape.json"))
		if err != nil {
			return fmt.Errorf("read sample %d: %w", i+1, err)
		}
		var shape struct {
			Sessions        int   `json:"sessions"`
			Entries         int   `json:"entriesPerSession"`
			InitialImportMs int64 `json:"initialImportMs"`
		}
		if err := json.Unmarshal(data, &shape); err != nil {
			return fmt.Errorf("decode sample %d: %w", i+1, err)
		}
		if shape.Sessions != 2 || shape.Entries != 10 {
			return fmt.Errorf("sample %d has unexpected shape", i+1)
		}
		samples = append(samples, shape.InitialImportMs)
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	r := smokeReport{Provenance: upgrade.BudgetProvenance{Head: strings.TrimSpace(string(head)), Base: base, Command: command, Date: time.Now().UTC().Format(time.RFC3339), Samples: len(samples)},
		Case: "C4", SamplesMs: samples, MedianMs: sorted[len(sorted)/2], MinMs: sorted[0], MaxMs: sorted[len(sorted)-1],
		Note: "Initial new-session pipeline wall time; smoke evidence only, not a skip budget."}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write report %s: %w", path, err)
	}
	fmt.Println(path)
	return nil
}
