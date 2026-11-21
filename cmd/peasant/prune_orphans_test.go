package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/prune_orphans.yaml
var pruneOrphansFixtureYAML []byte

// pruneOrphanMode is the closed set of ways a `prune --all --orphans` run can
// end: the interactive prompt answered either way, the --confirm bypass, and a
// dry run.
type pruneOrphanMode string

const (
	pruneOrphanModeDecline     pruneOrphanMode = "decline"
	pruneOrphanModeConsent     pruneOrphanMode = "consent"
	pruneOrphanModeConfirmFlag pruneOrphanMode = "confirm-flag"
	pruneOrphanModeDryRun      pruneOrphanMode = "dry-run"
)

var allPruneOrphanModes = []pruneOrphanMode{
	pruneOrphanModeDecline, pruneOrphanModeConsent, pruneOrphanModeConfirmFlag, pruneOrphanModeDryRun,
}

// prompts reports whether the mode goes through the interactive consent
// prompt, which is the window in which late arrivals are injected.
func (m pruneOrphanMode) prompts() bool {
	return m == pruneOrphanModeDecline || m == pruneOrphanModeConsent
}

type pruneOrphanCase struct {
	Name            string          `yaml:"name"`
	Mode            pruneOrphanMode `yaml:"mode"`
	SessionsDeleted bool            `yaml:"sessions_deleted"`
	Removed         []string        `yaml:"removed"`
	Kept            []string        `yaml:"kept"`
}

type pruneOrphanRefusal struct {
	Name  string   `yaml:"name"`
	Args  []string `yaml:"args"`
	Error string   `yaml:"error"`
}

type pruneOrphanFixtures struct {
	Orphans         []pruneExactEntry    `yaml:"orphans"`
	AdoptedOrphan   pruneExactEntry      `yaml:"adopted_orphan"`
	LateDiskSession pruneExactEntry      `yaml:"late_disk_session"`
	Cases           []pruneOrphanCase    `yaml:"cases"`
	Refusals        []pruneOrphanRefusal `yaml:"refusals"`
}

// enumerated is every directory the run sees as an orphan when it enumerates.
func (f pruneOrphanFixtures) enumerated() []pruneExactEntry {
	return append(slices.Clone(f.Orphans), f.AdoptedOrphan)
}

func loadPruneOrphanFixtures(t *testing.T) pruneOrphanFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(pruneOrphansFixtureYAML))
	decoder.KnownFields(true)
	var fixtures pruneOrphanFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode prune orphans fixture with strict fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("prune orphans fixture must contain one YAML document: %v", err)
	}
	if len(fixtures.Orphans) < 2 {
		t.Fatalf("prune orphans fixture needs at least two orphans, one sharing the planned sessions' host slug; got %d", len(fixtures.Orphans))
	}
	entries := map[string]pruneExactEntry{
		"adopted_orphan":    fixtures.AdoptedOrphan,
		"late_disk_session": fixtures.LateDiskSession,
	}
	for i, orphan := range fixtures.Orphans {
		entries[fmt.Sprintf("orphans[%d]", i)] = orphan
	}
	for key, entry := range entries {
		testutil.RequireFixtureFields(t, "prune orphans", key, []testutil.FixtureField{
			{Key: key + ".session_id", Value: entry.SessionID},
			{Key: key + ".output_path", Value: entry.OutputPath},
		})
	}
	if !slices.ContainsFunc(fixtures.Orphans, func(e pruneExactEntry) bool { return e.OutputPath == pruneOrphanPlannedSlug }) {
		t.Fatalf("prune orphans fixture needs one orphan under the planned sessions' host slug %q, so orphan status is proven per directory rather than per slug", pruneOrphanPlannedSlug)
	}

	enumeratedIDs := make([]string, 0, len(fixtures.Orphans)+1)
	for _, entry := range fixtures.enumerated() {
		enumeratedIDs = append(enumeratedIDs, entry.SessionID)
	}
	observedModes := make([]pruneOrphanMode, 0, len(fixtures.Cases))
	for i, tc := range fixtures.Cases {
		testutil.RequireFixtureFields(t, "prune orphans", fmt.Sprintf("cases[%d]", i), []testutil.FixtureField{
			{Key: "name", Value: tc.Name},
			{Key: "mode", Value: string(tc.Mode)},
		})
		if !slices.Contains(allPruneOrphanModes, tc.Mode) {
			t.Fatalf("prune orphans fixture case %q declares unknown mode %q; use one of %v", tc.Name, tc.Mode, allPruneOrphanModes)
		}
		for _, id := range append(slices.Clone(tc.Removed), tc.Kept...) {
			if !slices.Contains(enumeratedIDs, id) {
				t.Fatalf("prune orphans fixture case %q names %q, which is not an enumerated orphan (%v)", tc.Name, id, enumeratedIDs)
			}
		}
		observedModes = append(observedModes, tc.Mode)
	}
	testutil.RequireClosedSetCoverage(t, "prune orphans", "mode", allPruneOrphanModes, observedModes)

	if len(fixtures.Refusals) == 0 {
		t.Fatal("prune orphans fixture declares no flag-combination refusals")
	}
	for i, refusal := range fixtures.Refusals {
		testutil.RequireFixtureFields(t, "prune orphans", fmt.Sprintf("refusals[%d]", i), []testutil.FixtureField{
			{Key: "name", Value: refusal.Name},
			{Key: "error", Value: refusal.Error},
		})
		if !slices.Contains(refusal.Args, "--orphans") {
			t.Fatalf("prune orphans refusal %q does not pass --orphans, so it proves nothing about the flag", refusal.Name)
		}
	}
	return fixtures
}

// pruneOrphanPlannedSlug is the host slug seedPruneTestSessions records.
const pruneOrphanPlannedSlug = "github.com-test-prune"

// seedPruneOrphanTree seeds the two planned sessions with their transcript
// directories, plus every enumerated orphan directory, and returns the sync
// directory.
func seedPruneOrphanTree(t *testing.T, dir string, fixtures pruneOrphanFixtures) string {
	t.Helper()
	seedPruneTestSessions(t, dir)
	syncDir := filepath.Join(string(defaults.ResolveDataDirPathWith(dir)), "peasant-sync")
	paths := []string{
		filepath.Join(pruneOrphanPlannedSlug, pruneTestSessionA),
		filepath.Join(pruneOrphanPlannedSlug, pruneTestSessionB),
	}
	for _, orphan := range fixtures.enumerated() {
		paths = append(paths, filepath.Join(orphan.OutputPath, orphan.SessionID))
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Join(syncDir, path), 0o700); err != nil {
			t.Fatalf("seed transcript directory %s: %v", path, err)
		}
		if err := os.WriteFile(filepath.Join(syncDir, path, "transcript.jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("seed transcript file in %s: %v", path, err)
		}
	}
	return syncDir
}

func remainingPruneSessionIDs(t *testing.T, dir string) []string {
	t.Helper()
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store after prune: %v", err)
	}
	defer db.Close()
	rows, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("query sessions after prune: %v", err)
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = string(row.SessionID)
	}
	return ids
}

// runPruneOrphansThroughPrompt drives the real command through the consent
// prompt. While the prompt is open, a directory appears on disk and the
// adopted orphan gains a database row; then answer is typed.
func runPruneOrphansThroughPrompt(t *testing.T, dir, syncDir string, fixtures pruneOrphanFixtures, answer string) string {
	t.Helper()
	master, terminal := openTestTerminal(t)
	out := newSignalWriter("[y/N]")
	cmd := BuildPruneCommand()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetIn(terminal)
	cmd.SetArgs([]string{"--all", "--orphans", "--data-dir", dir, "--config-dir", dir})
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().String("config-dir", "", "")

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	select {
	case <-out.seen:
	case err := <-done:
		t.Fatalf("prune --all --orphans finished without prompting for consent: %v\n%s", err, out.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("prune --all --orphans never reached the consent prompt:\n%s", out.String())
	}

	late := fixtures.LateDiskSession
	if err := os.MkdirAll(filepath.Join(syncDir, late.OutputPath, late.SessionID), 0o700); err != nil {
		t.Fatalf("create a directory that appears after enumeration: %v", err)
	}
	insertPruneSession(t, dir, fixtures.AdoptedOrphan)

	if _, err := master.WriteString(answer + "\n"); err != nil {
		t.Fatalf("answer the consent prompt: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("prune --all --orphans after answering %q: %v\n%s", answer, err, out.String())
	}
	return out.String()
}

// TestPruneCmd_Orphans drives `prune --all --orphans` through every way a run
// can end and checks that the orphan set is previewed before anything is
// deleted, that only the enumerated list is removed, and that nothing which
// appeared after enumeration is touched.
func TestPruneCmd_Orphans(t *testing.T) {
	t.Parallel()
	fixtures := loadPruneOrphanFixtures(t)
	for _, tc := range fixtures.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			syncDir := seedPruneOrphanTree(t, dir, fixtures)

			var output string
			switch tc.Mode {
			case pruneOrphanModeDecline:
				output = runPruneOrphansThroughPrompt(t, dir, syncDir, fixtures, "n")
			case pruneOrphanModeConsent:
				output = runPruneOrphansThroughPrompt(t, dir, syncDir, fixtures, "y")
			case pruneOrphanModeConfirmFlag, pruneOrphanModeDryRun:
				flag := "--confirm"
				if tc.Mode == pruneOrphanModeDryRun {
					flag = "--dry-run"
				}
				out, _, err := executePruneCmd(t, dir, []string{"--all", "--orphans", flag})
				if err != nil {
					t.Fatalf("prune --all --orphans %s: %v\n%s", flag, err, out)
				}
				output = out
			default:
				t.Fatalf("unhandled mode %q", tc.Mode)
			}

			enumerated := fixtures.enumerated()
			// The preview: every enumerated orphan is named, before the prompt
			// when there is one, and a planned session's directory is not.
			previewEnd := len(output)
			if tc.Mode.prompts() {
				previewEnd = strings.Index(output, "[y/N]")
				want := fmt.Sprintf("Delete 2 session(s) and %d orphaned transcript director(ies)? [y/N]", len(enumerated))
				if !strings.Contains(output, want) {
					t.Errorf("the consent prompt does not name the enumerated orphan count (looked for %q):\n%s", want, output)
				}
			}
			preview := output[:previewEnd]
			for _, orphan := range enumerated {
				if path := filepath.Join(orphan.OutputPath, orphan.SessionID); !strings.Contains(preview, path) {
					t.Errorf("orphan %s is not listed in the preview before any deletion:\n%s", path, output)
				}
			}
			for _, planned := range []string{pruneTestSessionA, pruneTestSessionB} {
				if path := filepath.Join(pruneOrphanPlannedSlug, planned); strings.Contains(output, path) {
					t.Errorf("planned session directory %s is listed as an orphan; it has a database row:\n%s", path, output)
				}
			}
			if tc.Mode == pruneOrphanModeDryRun && !strings.Contains(output, fmt.Sprintf("2 session(s) and %d orphaned transcript director(ies) would be deleted", len(enumerated))) {
				t.Errorf("the dry run does not summarize the orphan count:\n%s", output)
			}

			// Only the enumerated list is removed, and of it only what the case
			// expects.
			for _, orphan := range enumerated {
				_, err := os.Stat(filepath.Join(syncDir, orphan.OutputPath, orphan.SessionID))
				gone := os.IsNotExist(err)
				if want := slices.Contains(tc.Removed, orphan.SessionID); gone != want {
					t.Errorf("orphan %s: removed=%v, want removed=%v (stat err %v)\n%s", orphan.SessionID, gone, want, err, output)
				}
			}
			for _, id := range tc.Kept {
				if !strings.Contains(output, "kept "+filepath.Join(fixtures.AdoptedOrphan.OutputPath, id)) || !strings.Contains(output, "gained a database row after the preview") {
					t.Errorf("the command did not report keeping %s after it gained a row:\n%s", id, output)
				}
			}

			// A directory that appeared after enumeration survives and is never
			// named: the failure the removed sweep had.
			if tc.Mode.prompts() {
				late := fixtures.LateDiskSession
				if _, err := os.Stat(filepath.Join(syncDir, late.OutputPath, late.SessionID)); err != nil {
					t.Errorf("a directory created after the orphan set was enumerated was removed: %v\n%s", err, output)
				}
				if strings.Contains(output, late.SessionID) {
					t.Errorf("a directory created after enumeration is named in the output:\n%s", output)
				}
			}

			// The database half.
			remaining := remainingPruneSessionIDs(t, dir)
			for _, planned := range []string{pruneTestSessionA, pruneTestSessionB} {
				if slices.Contains(remaining, planned) == tc.SessionsDeleted {
					t.Errorf("session %s: deleted=%v, want deleted=%v (remaining %v)", planned, !slices.Contains(remaining, planned), tc.SessionsDeleted, remaining)
				}
			}
			if tc.Mode.prompts() && !slices.Contains(remaining, fixtures.AdoptedOrphan.SessionID) {
				t.Errorf("the row that arrived during the consent window was deleted (remaining %v)", remaining)
			}

			// With --orphans the leftover notice has nothing of the confirmed
			// set left to report.
			if tc.Mode == pruneOrphanModeConsent || tc.Mode == pruneOrphanModeConfirmFlag {
				if strings.Contains(output, "no deletion plan could include them") {
					t.Errorf("the residue notice fired for directories --orphans already handled:\n%s", output)
				}
			}
		})
	}
}

// TestPruneCmd_OrphansRefusedWithoutAll pins the flag combinations --orphans
// cannot apply to: they fail before opening the store, and nothing is deleted.
func TestPruneCmd_OrphansRefusedWithoutAll(t *testing.T) {
	t.Parallel()
	fixtures := loadPruneOrphanFixtures(t)
	for _, refusal := range fixtures.Refusals {
		t.Run(refusal.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			syncDir := seedPruneOrphanTree(t, dir, fixtures)

			out, _, err := executePruneCmd(t, dir, refusal.Args)
			if err == nil || !strings.Contains(err.Error(), refusal.Error) {
				t.Fatalf("prune %v: want an error containing %q, got %v\n%s", refusal.Args, refusal.Error, err, out)
			}
			for _, orphan := range fixtures.enumerated() {
				if _, statErr := os.Stat(filepath.Join(syncDir, orphan.OutputPath, orphan.SessionID)); statErr != nil {
					t.Errorf("refused run removed orphan %s: %v", orphan.SessionID, statErr)
				}
			}
			if remaining := remainingPruneSessionIDs(t, dir); len(remaining) != 2 {
				t.Errorf("refused run changed the store: remaining %v", remaining)
			}
		})
	}
}
