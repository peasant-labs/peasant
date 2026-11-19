//go:build skipbudget

// Command skip-budget measures each planned refresh skip against the work it
// avoids on the same prepared synthetic base, and writes one report per case.
//
// Every case cleans and writes under <root>/<case>/: it prepares its base
// once, then runs the skip arm and the baseline arm once per round on a fresh
// copy of that base, alternating their order. The default shape is the
// budget matrix, 240 sessions x 1000 entries over 5 rounds; run it only on a
// quiet machine. A smaller shape is a smoke run and its report says so.
//
// C2 and C5 are retired: they measured the metadata-only copy refresh after a
// failed proof, and a failed proof now parses, so neither has a skip left to
// measure. Their ids are not reused.
//
// Exit condition: the harness exists to decide whether the conditional
// legacy-row validation (#1036) and shared-mapping reuse (#1038) are worth
// building. Once that decision is recorded on both issues, delete the harness
// or shrink it to the new-session smoke case; the structural budget test and
// the stored run profile then carry the evidence.
//
//	go run -tags skipbudget ./cmd/skip-budget -list
//	go run -tags skipbudget ./cmd/skip-budget -case C1,C3,C6 -base "$(git merge-base HEAD origin/develop)"
//	go run -tags skipbudget ./cmd/skip-budget -case C1 -base <rev> -sessions 2 -entries 10
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/upgrade"
)

type matrixCase struct {
	Name    upgrade.MeasurementCase `json:"name"`
	Shape   string                  `json:"shape"`
	DataDir string                  `json:"dataDir"`
}

type report struct {
	Provenance upgrade.BudgetProvenance `json:"provenance"`
	Cases      []matrixCase             `json:"cases"`
}

type smokeReport struct {
	Provenance upgrade.BudgetProvenance `json:"provenance"`
	Case       upgrade.MeasurementCase  `json:"case"`
	SamplesMs  []int64                  `json:"samplesMs"`
	MedianMs   int64                    `json:"medianMs"`
	MinMs      int64                    `json:"minMs"`
	MaxMs      int64                    `json:"maxMs"`
	Note       string                   `json:"note"`
}

const defaultRoot = "/tmp/opencode/peasant-1034"

func main() {
	list := flag.Bool("list", false, "print the planned cases without running them")
	caseList := flag.String("case", "", "comma-separated cases to run: C1, C3, C4, C6")
	base := flag.String("base", "", "comparison base revision")
	root := flag.String("root", defaultRoot, "absolute directory that holds one directory per case")
	sessions := flag.Int("sessions", matrixSessions, "synthetic sessions per case (C4 always uses 2)")
	entries := flag.Int("entries", matrixEntries, "entries per session (C4 always uses 10)")
	rounds := flag.Int("rounds", 5, "rounds per case; each round runs both arms once")
	importSample := flag.String("import-sample", "", "internal: import one new-session sample into this directory and print its wall time in ms")
	flag.Parse()
	if *importSample != "" {
		shape, err := prepareBase(context.Background(), *importSample, smokeSessions, smokeEntries, mutateNone)
		if err != nil {
			fail(1, fmt.Sprintf("import sample in %s: %v", *importSample, err))
		}
		fmt.Println(shape.PrepareMs)
		return
	}
	cases := matrixCases(*root)
	if *list {
		if err := json.NewEncoder(os.Stdout).Encode(report{Cases: cases}); err != nil {
			fmt.Fprintf(os.Stderr, "skip-budget: writing matrix to stdout failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if strings.TrimSpace(*base) == "" || strings.ContainsAny(*base, " \t\n") {
		fail(2, "supply -base with the comparison base revision, without whitespace")
	}
	if *sessions < 1 || *entries < 2 || *rounds < 1 {
		fail(2, "-sessions and -rounds must be positive and -entries at least 2")
	}
	var selected []upgrade.MeasurementCase
	for _, raw := range strings.Split(*caseList, ",") {
		c, err := upgrade.NewMeasurementCase(strings.TrimSpace(raw))
		if err != nil {
			fail(2, err.Error())
		}
		selected = append(selected, c)
	}
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		fail(1, fmt.Sprintf("read branch head: %v; run from the repository checkout", err))
	}
	status, err := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		fail(1, fmt.Sprintf("read worktree state: %v", err))
	}
	opts := harnessOptions{root: *root, sessions: *sessions, entries: *entries, rounds: *rounds,
		head: strings.TrimSpace(string(head)), base: *base, command: strings.Join(append([]string{"go run -tags skipbudget ./cmd/skip-budget"}, os.Args[1:]...), " "),
		dirty: len(strings.TrimSpace(string(status))) != 0}
	specs := caseSpecs()
	ctx := context.Background()
	for _, c := range selected {
		if c == upgrade.MeasurementCaseNewSession {
			dir, err := cleanCaseDir(*root, c)
			if err == nil {
				err = runSmoke(dir, opts)
			}
			if err != nil {
				fail(1, fmt.Sprintf("%s failed: %v", c, err))
			}
			continue
		}
		path, err := runCase(ctx, specs[c], opts)
		if err != nil {
			fail(1, fmt.Sprintf("%s failed: %v", c, err))
		}
		fmt.Println(path)
	}
}

// matrixCases lists every case the harness can run, in matrix order, with its
// data directory under root.
func matrixCases(root string) []matrixCase {
	cases := []matrixCase{
		{Name: upgrade.MeasurementCaseStampOnly, Shape: "stamp-only adapter bump: proof and settle vs parse"},
		{Name: upgrade.MeasurementCaseIndexerBump, Shape: "indexer bump: stored-index lane vs native"},
		{Name: upgrade.MeasurementCaseNewSession, Shape: "new session: 2 x 10"},
		{Name: upgrade.MeasurementCaseUndeclaredProducer, Shape: "undeclared producer: the full step the plan names"},
	}
	for i := range cases {
		cases[i].DataDir = filepath.Join(root, string(cases[i].Name))
	}
	return cases
}

func fail(code int, message string) {
	fmt.Fprintln(os.Stderr, "skip-budget: "+message)
	os.Exit(code)
}

// smokeSessions and smokeEntries are the new-session smoke shape.
const (
	smokeSessions = 2
	smokeEntries  = 10
)

// runSmoke times the new-session full step: a fresh 2 x 10 import into an
// empty managed store, five times. Each sample runs in its own process, as a
// harvest does, so no sample reuses a heap an earlier pipeline left behind.
func runSmoke(dir string, opts harnessOptions) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the harness binary to run each sample: %w", err)
	}
	samples := make([]int64, 0, 5)
	for i := range 5 {
		sampleDir := filepath.Join(dir, fmt.Sprintf("sample-%d", i+1))
		cmd := exec.Command(self, "-import-sample", sampleDir)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("import sample %d in %s: %w", i+1, sampleDir, err)
		}
		ms, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return fmt.Errorf("sample %d printed %q, want its import wall time in ms: %w", i+1, out, err)
		}
		samples = append(samples, ms)
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	r := smokeReport{Provenance: upgrade.BudgetProvenance{Head: opts.head, Base: opts.base, Command: opts.command, Date: time.Now().UTC().Format(time.RFC3339), Samples: len(samples)},
		Case: upgrade.MeasurementCaseNewSession, SamplesMs: samples, MedianMs: sorted[len(sorted)/2], MinMs: sorted[0], MaxMs: sorted[len(sorted)-1],
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
