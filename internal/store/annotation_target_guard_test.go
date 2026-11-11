//go:build astgrep

package store_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The guarded-insert source gate: no production Go code may carry its own
// INSERT INTO annotation_target_entries. Every entry-target insert routes
// through insertAnnotationTargetEntryOnConn (the guarded sqlInsertTargetEntry
// statement with the affected-row existence check that replaced the
// session_entries foreign key).
//
// The rule lives in ast-grep/no-unguarded-annotation-target-insert.yml and
// also runs in the repo-wide `ast-grep scan` step of `make check`. This test
// runs only the one rule via `go test -tags=astgrep ./internal/store/` so a
// bypass fails here with the offending file and line, following the
// internal/tui/gates key-grep gate pattern. Like that gate, it shells out to
// the ast-grep CLI and never runs in a plain `go test ./...`.
func TestAnnotationTargetGuardedInsert(t *testing.T) {
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skipf("ast-grep binary not found on PATH; run inside the nix devShell: %v", err)
	}
	// internal/store is two levels below the module root, and `go test` runs the
	// test binary in its package directory. Resolving from there keeps this
	// working under -trimpath, where runtime.Caller reports a module-relative
	// path that no filesystem resolves.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "sgconfig.yml")); statErr != nil {
		t.Fatalf("the module root %s holds no sgconfig.yml: %v", root, statErr)
	}
	// ast-grep resolves the rule's files/ignores globs against the scan's
	// working directory, so the scan must run from the root, not from an
	// absolute target path.
	cmd := exec.Command("ast-grep", "scan", "--config", "sgconfig.yml", "--json=compact", "--filter", "^no-unguarded-annotation-target-insert$", ".")
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	var matches []struct {
		File  string `json:"file"`
		Range struct {
			Start struct {
				Line int `json:"line"`
			} `json:"start"`
		} `json:"range"`
		Text   string `json:"text"`
		RuleID string `json:"ruleId"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &matches); err != nil {
		t.Fatalf("ast-grep output did not decode: %v; stderr: %s", err, stderr.String())
	}
	var exitErr *exec.ExitError
	if runErr != nil {
		if !errors.As(runErr, &exitErr) || (exitErr.ExitCode() != 0 && exitErr.ExitCode() != 1) {
			t.Fatalf("ast-grep exited unexpectedly: %v; stderr: %s", runErr, stderr.String())
		}
	}
	if len(matches) > 0 {
		for _, m := range matches {
			t.Errorf("unguarded annotation_target_entries insert at %s:%d: %q", m.File, m.Range.Start.Line, m.Text)
		}
		t.Fatal(fmt.Sprintf("found %d unguarded insert(s); route them through insertAnnotationTargetEntryOnConn", len(matches)))
	}
}
