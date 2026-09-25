package main

import (
	"bytes"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The summarizer is a gate input, so its correctness is asserted against a
// committed `go test -json` stream rather than an inline case table: the input
// shape is the contract, and a real stream carries interleaved build output,
// retried tests, skips, and nested subtests that a hand-written table would
// omit.
//
//go:embed testdata/stream.json
var streamJSON []byte

// streamFixture is the committed sample stream, required by name so that
// deleting it is a deliberate act rather than an accident that silently empties
// the assertions below.
const streamFixture = "stream.json"

func loadStream(t *testing.T) []byte {
	t.Helper()
	if _, err := os.Stat(filepath.Join("testdata", streamFixture)); err != nil {
		t.Fatalf("required fixture %q is missing: %v", streamFixture, err)
	}
	return streamJSON
}

// buildBinary compiles the summarizer once so the tests exercise the real
// command rather than the internal functions. The total and the family grouping
// are the two things most likely to regress silently, and both are
// user-visible output.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "test-timing")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build summarizer: %v\n%s", err, out)
	}
	return bin
}

func runSummarizer(t *testing.T, bin string, stdin []byte, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String() + stderr.String(), err
}

func TestSummarizer_ExcludesSubtestsFromTotal(t *testing.T) {
	bin := buildBinary(t)
	out, err := runSummarizer(t, bin, loadStream(t))
	if err != nil {
		t.Fatalf("summarizer exited %v\n%s", err, out)
	}

	// The fixture holds 4 top-level tests (2.5s + 1.0s + 0.5s + 0.4s) whose two
	// subtests (0.4s + 0.6s) are already contained in their parents. A total of
	// 6.8s would mean subtests were double counted, which would overstate every
	// share and hide the real hot spots.
	if !strings.Contains(out, "4 top-level tests, 2 subtests, 4.4s total") {
		t.Errorf("expected subtest-exclusive total, got:\n%s", out)
	}

	// The explicit note is load-bearing: a reader comparing the per-family
	// section against the total needs to know the two are not comparable.
	if !strings.Contains(out, "NOT added to the total") {
		t.Errorf("expected the subtest double-count note, got:\n%s", out)
	}
}

func TestSummarizer_RanksSlowestTestFirst(t *testing.T) {
	bin := buildBinary(t)
	out, _ := runSummarizer(t, bin, loadStream(t), "-top", "10")

	slowIdx := strings.Index(out, "TestSecondExpensive")
	fastIdx := strings.Index(out, "TestFirstCheap")
	if slowIdx < 0 || fastIdx < 0 {
		t.Fatalf("expected both tests in the ranking, got:\n%s", out)
	}
	if slowIdx > fastIdx {
		t.Errorf("expected the 2.5s test to rank above the 1.0s test, got:\n%s", out)
	}

	// Share is the number a reviewer actually acts on, so it must be derived
	// from the subtest-exclusive total: 2.5/4.4 = 56.8%.
	if !strings.Contains(out, "56.8%") {
		t.Errorf("expected a 56.8%% share for the 2.5s test, got:\n%s", out)
	}
}

func TestSummarizer_WarnsOnDominantTest(t *testing.T) {
	bin := buildBinary(t)
	out, _ := runSummarizer(t, bin, loadStream(t), "-warn-pct", "50")
	// The 2.5s test is 56.8% of the total and must be surfaced; the 1.0s test
	// at 22.7% must not.
	if warned := strings.Count(out, "over -warn-pct"); warned != 1 {
		t.Errorf("expected exactly 1 test over the 50%% warn threshold, got %d:\n%s", warned, out)
	}
}

func TestSummarizer_DoesNotDoubleCountRetriedTest(t *testing.T) {
	bin := buildBinary(t)
	// The fixture includes a retried "TestFlaky" that fails then passes. Both
	// attempts are in the stream; charging the test twice would inflate the
	// total, and reporting the first attempt would flag a recovered flake as a
	// real failure -- both wrong in exactly the case where someone is chasing a
	// flake.
	out, err := runSummarizer(t, bin, loadStream(t), "-top", "20")
	if err != nil {
		t.Errorf("expected a clean exit once the retry passed, got %v:\n%s", err, out)
	}
	if strings.Contains(out, "FAILING TEST: TestFlaky") {
		t.Errorf("expected the passing retry of TestFlaky to suppress the failure, got:\n%s", out)
	}
	// 4.4s total, not 4.7s: only the passing attempt is charged.
	if !strings.Contains(out, "4.4s total") {
		t.Errorf("expected only the final retry charged (4.4s), got:\n%s", out)
	}
}

func TestSummarizer_RejectsEmptyInput(t *testing.T) {
	bin := buildBinary(t)
	// A gate that silently reports "0.0s total" on a broken pipe would let a
	// regression pass unnoticed, so empty input must be a hard error.
	if _, err := runSummarizer(t, bin, nil); err == nil {
		t.Error("expected a non-zero exit on empty input")
	}
}
