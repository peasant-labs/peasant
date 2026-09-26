package testgate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/teststream"
)

// The batched profiler removes cross-test CPU-queueing from per-test cost. A
// whole-package `go test` runs its tests concurrently when they call
// t.Parallel; a slow test's reported Elapsed then absorbs the queue it waited
// in, which is how a sub-second test can report tens of seconds. The profiler
// partitions one package's top-level tests into N disjoint batches and runs N
// processes concurrently, each with -parallel=1. With N well under the core
// count, queueing is small and roughly constant, so it stops distorting the
// per-test ranking.
//
// The batches are balanced by the planner below: greedy longest-processing-time
// first (LPT) when prior per-test timings are supplied, round-robin otherwise.
//
// The profiler is a measurement tool, not a gate. Its streams are Classes B/C
// evidence: a profiled wall is NOT quotable as a budget number. It retires once
// the gate reports a per-family breakdown from its own -json stream.

// Batch is one profiler batch: a disjoint set of top-level test names.
type Batch struct {
	Index  int
	Tests  []string
	Weight time.Duration
}

// BatchPlan is the deterministic result of the planner.
type BatchPlan struct {
	Batches    []Batch
	Assignment map[string]int
	LPT        bool
	BatchCount int
}

// defaultUnknownWeight is the LPT weight given to a test with no prior timing.
// It is the median of the known weights so an unmeasured test is assumed
// typical rather than negligible; with no known weights every test is equal.
func defaultUnknownWeight(known []time.Duration) time.Duration {
	if len(known) == 0 {
		return time.Second
	}
	sorted := append([]time.Duration(nil), known...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// PlanBatches partitions names into n disjoint, deterministic batches. When
// prior is non-empty it runs greedy LPT (heaviest test first onto the currently
// lightest batch); otherwise it round-robins over the name-sorted list. The
// result is stable for identical inputs, so a report can be reproduced.
func PlanBatches(names []string, prior map[string]time.Duration, n int) (BatchPlan, error) {
	if n < 1 {
		return BatchPlan{}, fmt.Errorf("batch count %d, want >= 1", n)
	}
	uniq := map[string]bool{}
	ordered := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || uniq[name] {
			continue
		}
		uniq[name] = true
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	plan := BatchPlan{
		Batches:    make([]Batch, n),
		Assignment: map[string]int{},
		BatchCount: n,
	}
	for i := range plan.Batches {
		plan.Batches[i].Index = i
	}
	if len(ordered) == 0 {
		return plan, nil
	}

	weights := map[string]time.Duration{}
	for _, name := range ordered {
		weights[name] = time.Second
	}

	if len(prior) == 0 {
		for i, name := range ordered {
			b := i % n
			plan.Batches[b].Tests = append(plan.Batches[b].Tests, name)
			plan.Assignment[name] = b
			plan.Batches[b].Weight += weights[name]
		}
	} else {
		plan.LPT = true
		var known []time.Duration
		for _, name := range ordered {
			if w, ok := prior[name]; ok && w > 0 {
				known = append(known, w)
			}
		}
		fallback := defaultUnknownWeight(known)
		for _, name := range ordered {
			if w, ok := prior[name]; ok && w > 0 {
				weights[name] = w
			} else {
				weights[name] = fallback
			}
		}
		order := append([]string(nil), ordered...)
		sort.SliceStable(order, func(i, j int) bool {
			if weights[order[i]] != weights[order[j]] {
				return weights[order[i]] > weights[order[j]]
			}
			return order[i] < order[j]
		})
		for _, name := range order {
			best := 0
			for b := 1; b < n; b++ {
				if plan.Batches[b].Weight < plan.Batches[best].Weight {
					best = b
				}
			}
			plan.Batches[best].Tests = append(plan.Batches[best].Tests, name)
			plan.Assignment[name] = best
			plan.Batches[best].Weight += weights[name]
		}
	}

	for i := range plan.Batches {
		sort.Strings(plan.Batches[i].Tests)
	}
	return plan, nil
}

// BatchProfileConfig configures a batched profile of one package.
type BatchProfileConfig struct {
	Root       string
	GoBin      string
	Package    string // repo-relative package dir, e.g. "./internal/ingest"
	ImportPath string
	OutDir     string
	Race       bool
	Timeout    time.Duration
	Batches    int
	Prior      map[string]time.Duration
	// CPUProfileTop re-runs the N slowest top-level tests one at a time with
	// -cpuprofile, writing one pprof per test. 0 disables it.
	CPUProfileTop int
	// Parallel is the per-batch -parallel value. 0 leaves it unpinned (uses
	// GOMAXPROCS), which is how an isolated package run is measured; a batched
	// profiler pins 1 so each batch process holds one test at a time.
	Parallel int
	// Profiles enables Class B attribution: block, mutex, cpu, and trace
	// profiles on each batch invocation. A profiled wall is NOT quotable; these
	// files answer where the wall − CPU gap is, not how big it is.
	Profiles ProfileFlags
	Env      []string
}

// ProfileFlags selects the Class B attribution profiles to write per batch.
type ProfileFlags struct {
	Block bool
	Mutex bool
	CPU   bool
	Trace bool
}

// Enabled reports whether any attribution profile was selected.
func (p ProfileFlags) Enabled() bool { return p.Block || p.Mutex || p.CPU || p.Trace }

// BatchResult is one profiler batch's measured boundary.
type BatchResult struct {
	Index      int      `json:"index"`
	Tests      []string `json:"tests"`
	WallMS     int64    `json:"wall_ms"`
	UserMS     int64    `json:"user_ms"`
	SystemMS   int64    `json:"system_ms"`
	StreamPath string   `json:"stream_path"`
	// Class B attribution artifacts, empty in a profile-free run. A profiled
	// wall is not quotable; these locate the wall − CPU gap.
	BlockProfile string `json:"block_profile,omitempty"`
	MutexProfile string `json:"mutex_profile,omitempty"`
	CPUProfile   string `json:"cpu_profile,omitempty"`
	Trace        string `json:"trace,omitempty"`
}

// TestTiming is one top-level test's queue-free wall, attributed to its batch.
type TestTiming struct {
	Test   string `json:"test"`
	WallMS int64  `json:"wall_ms"`
	Batch  int    `json:"batch"`
}

// BatchProfileResult is the machine-readable profiler document (profile.json).
type BatchProfileResult struct {
	SchemaVersion    int           `json:"schema_version"`
	Package          string        `json:"package"`
	ImportPath       string        `json:"import_path"`
	Race             bool          `json:"race"`
	Parallel         int           `json:"parallel"`
	RequestedBatches int           `json:"requested_batches"`
	LPT              bool          `json:"lpt"`
	Batches          []BatchResult `json:"batches"`
	Tests            []TestTiming  `json:"tests"`
	CPUProfiles      []string      `json:"cpu_profiles"`
	Errors           []string      `json:"errors"`
}

// RunBatchProfile computes the batch plan, runs the batches concurrently, and
// parses each batch's stream into per-test timings. A batched profiler pins
// -parallel=1 and -count=1, so queueing is bounded and no cached result can be
// replayed as a 0 s pass; an isolated run leaves -parallel unpinned.
func RunBatchProfile(ctx context.Context, cfg BatchProfileConfig) (*BatchProfileResult, error) {
	if cfg.Batches < 1 {
		return nil, fmt.Errorf("batches %d, want >= 1", cfg.Batches)
	}
	if cfg.GoBin == "" {
		cfg.GoBin = "go"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = time.Hour
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("create profile out dir: %w", err)
	}

	names, err := ListTestsForPackage(ctx, cfg.GoBin, cfg.Root, cfg.Package)
	if err != nil {
		return nil, err
	}
	plan, err := PlanBatches(names, cfg.Prior, cfg.Batches)
	if err != nil {
		return nil, err
	}

	res := &BatchProfileResult{
		SchemaVersion:    1,
		Package:          cfg.Package,
		ImportPath:       cfg.ImportPath,
		Race:             cfg.Race,
		Parallel:         cfg.Parallel,
		RequestedBatches: cfg.Batches,
		LPT:              plan.LPT,
		Batches:          []BatchResult{},
		Tests:            []TestTiming{},
		CPUProfiles:      []string{},
		Errors:           []string{},
	}

	results := make([]BatchResult, plan.BatchCount)
	testTimings := make([][]TestTiming, plan.BatchCount)
	errs := make([][]string, plan.BatchCount)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, batch := range plan.Batches {
		if len(batch.Tests) == 0 {
			continue
		}
		wg.Add(1)
		go func(batch Batch) {
			defer wg.Done()
			br, timings, berrs := runOneBatch(ctx, cfg, batch)
			mu.Lock()
			defer mu.Unlock()
			results[batch.Index] = br
			testTimings[batch.Index] = timings
			errs[batch.Index] = berrs
		}(batch)
	}
	wg.Wait()

	for i := range plan.Batches {
		if len(plan.Batches[i].Tests) == 0 {
			continue
		}
		res.Batches = append(res.Batches, results[i])
		res.Tests = append(res.Tests, testTimings[i]...)
		res.Errors = append(res.Errors, errs[i]...)
	}
	sort.Slice(res.Tests, func(i, j int) bool {
		if res.Tests[i].WallMS != res.Tests[j].WallMS {
			return res.Tests[i].WallMS > res.Tests[j].WallMS
		}
		return res.Tests[i].Test < res.Tests[j].Test
	})

	if cfg.CPUProfileTop > 0 {
		profiles, perrs := runCPUProfiles(ctx, cfg, res.Tests)
		res.CPUProfiles = profiles
		res.Errors = append(res.Errors, perrs...)
	}
	return res, nil
}

// profileSlugRE sanitizes a test name into a filesystem-safe profile name.
var profileSlugRE = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// ListTestsForPackage enumerates the top-level test names in one package via
// `go test -list`, which compiles the test binary but executes no tests.
func ListTestsForPackage(ctx context.Context, goBin, root, pkg string) ([]string, error) {
	cmd := exec.CommandContext(ctx, goBin, "test", "-list", ".*", pkg)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go test -list %s: %w", pkg, err)
	}
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if testNameRE.MatchString(name) && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func runOneBatch(ctx context.Context, cfg BatchProfileConfig, batch Batch) (BatchResult, []TestTiming, []string) {
	batchDir := filepath.Join(cfg.OutDir, fmt.Sprintf("batch-%02d", batch.Index))
	_ = os.MkdirAll(batchDir, 0o755)
	streamPath := filepath.Join(cfg.OutDir, fmt.Sprintf("batch-%02d.json", batch.Index))
	errPath := filepath.Join(cfg.OutDir, fmt.Sprintf("batch-%02d.err", batch.Index))
	args := []string{"test", "-json", "-count=1", "-timeout", cfg.Timeout.String(),
		"-outputdir", batchDir}
	if cfg.Parallel > 0 {
		args = append(args, fmt.Sprintf("-parallel=%d", cfg.Parallel))
	}
	if cfg.Race {
		args = append(args, "-race")
	}
	br := BatchResult{Index: batch.Index, Tests: batch.Tests, StreamPath: streamPath}
	if cfg.Profiles.Enabled() {
		prefix := filepath.Join(cfg.OutDir, fmt.Sprintf("batch-%02d", batch.Index))
		if cfg.Profiles.Block {
			br.BlockProfile = prefix + ".block.out"
			args = append(args, "-blockprofile", br.BlockProfile, "-blockprofilerate", "10000")
		}
		if cfg.Profiles.Mutex {
			br.MutexProfile = prefix + ".mutex.out"
			args = append(args, "-mutexprofile", br.MutexProfile, "-mutexprofilefraction", "100")
		}
		if cfg.Profiles.CPU {
			br.CPUProfile = prefix + ".cpu.out"
			args = append(args, "-cpuprofile", br.CPUProfile)
		}
		if cfg.Profiles.Trace {
			br.Trace = prefix + ".trace.out"
			args = append(args, "-trace", br.Trace)
		}
	}
	args = append(args, "-run", runRegexLiteral(batch.Tests), cfg.Package)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, cfg.GoBin, args...)
	cmd.Dir = cfg.Root
	cmd.Env = cfg.Env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	beforeUser, beforeSys, _ := readChildUsage()
	start := time.Now()
	runErr := cmd.Run()
	wall := time.Since(start)
	afterUser, afterSys, _ := readChildUsage()

	_ = os.WriteFile(streamPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(errPath, stderr.Bytes(), 0o644)

	br.WallMS = wall.Milliseconds()
	br.UserMS = (afterUser - beforeUser).Milliseconds()
	br.SystemMS = (afterSys - beforeSys).Milliseconds()
	var errs []string
	records, err := teststream.ParseStream(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		errs = append(errs, fmt.Sprintf("batch %d parse stream: %v", batch.Index, err))
		return br, nil, errs
	}
	timings := make([]TestTiming, 0, len(records))
	for _, rec := range records {
		if rec.Test == "" || strings.ContainsRune(rec.Test, '/') {
			continue // subtests are inclusive of their parent and are not top-level units
		}
		timings = append(timings, TestTiming{Test: rec.Test, WallMS: rec.Elapsed.Milliseconds(), Batch: batch.Index})
	}
	if runErr != nil && len(records) == 0 {
		errs = append(errs, fmt.Sprintf("batch %d failed with no test events: %v\n%s", batch.Index, runErr, tail(stderr.String(), 20)))
	}
	return br, timings, errs
}

// runCPUProfiles re-runs the slowest tests one at a time so each profile is
// that test's work and nothing else. A test whose cost is sleep or I/O shows up
// nearly empty, which is itself the wall-vs-CPU finding.
func runCPUProfiles(ctx context.Context, cfg BatchProfileConfig, tests []TestTiming) ([]string, []string) {
	dir := filepath.Join(cfg.OutDir, "cpuprofile")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, []string{fmt.Sprintf("create cpuprofile dir: %v", err)}
	}
	var profiles, errs []string
	for i, t := range tests {
		if i >= cfg.CPUProfileTop {
			break
		}
		slug := profileSlugRE.ReplaceAllString(t.Test, "_")
		profilePath := filepath.Join(dir, slug+".pprof")
		args := []string{"test", "-count=1", "-timeout", cfg.Timeout.String()}
		if cfg.Parallel > 0 {
			args = append(args, fmt.Sprintf("-parallel=%d", cfg.Parallel))
		}
		args = append(args, "-cpuprofile", profilePath, "-run", "^"+regexp.QuoteMeta(t.Test)+"$", cfg.Package)
		if cfg.Race {
			args = append(args, "-race")
		}
		cmd := exec.CommandContext(ctx, cfg.GoBin, args...)
		cmd.Dir = cfg.Root
		cmd.Env = cfg.Env
		logPath := filepath.Join(dir, slug+".log")
		logFile, err := os.Create(logPath)
		if err != nil {
			errs = append(errs, fmt.Sprintf("create %s: %v", logPath, err))
			continue
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Run(); err != nil {
			errs = append(errs, fmt.Sprintf("cpuprofile %s: %v", t.Test, err))
		}
		_ = logFile.Close()
		profiles = append(profiles, profilePath)
	}
	return profiles, errs
}

// runRegexLiteral builds an anchored alternation for the planner's batch. It
// mirrors RunRegex but keeps the profiler independent of the gate's plan shape.
func runRegexLiteral(names []string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, regexp.QuoteMeta(n))
	}
	return "^(" + strings.Join(parts, "|") + ")$"
}

// WriteProfileDocument writes the profiler result as indented JSON, normalizing
// empty collections to [] so a consumer never special-cases null.
func WriteProfileDocument(path string, res *BatchProfileResult) error {
	if res.Batches == nil {
		res.Batches = []BatchResult{}
	}
	if res.Tests == nil {
		res.Tests = []TestTiming{}
	}
	if res.CPUProfiles == nil {
		res.CPUProfiles = []string{}
	}
	if res.Errors == nil {
		res.Errors = []string{}
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
