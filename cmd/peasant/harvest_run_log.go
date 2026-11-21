package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// harvestRunLogPrefix starts the name of every per-run harvest log. The full
// name is harvest--<unix>.log, where <unix> is the run's start in Unix seconds.
const harvestRunLogPrefix = "harvest--"

// harvestRunLogSuffix ends the name of every per-run harvest log.
const harvestRunLogSuffix = ".log"

// harvestRunLogPath is where the log of a run that started at startedAt lives:
// the logs directory under the state directory, beside the push logs.
func harvestRunLogPath(stateDirOv string, startedAt time.Time) string {
	logDir := filepath.Join(defaults.ResolveStateDirPathWith(stateDirOv).String(), "logs")
	return filepath.Join(logDir, fmt.Sprintf("%s%d%s", harvestRunLogPrefix, startedAt.Unix(), harvestRunLogSuffix))
}

// writeHarvestRunLogOrWarn writes the run log of a finished, non-forecast run
// and returns its path for the caller to print. A dry run promises to write
// nothing, so it writes no log either and returns "". A log that cannot be
// written is a warning on warn and returns "": the exit code is unchanged.
func writeHarvestRunLogOrWarn(warn io.Writer, result *ingest.PipelineResult, options harvestOutputOptions) string {
	if result == nil || (options.flags != nil && options.flags.dryRun) {
		return ""
	}
	path, err := writeHarvestRunLog(result, options, options.stateDir)
	if err != nil {
		fmt.Fprintf(warn, "warning: could not write the harvest run log: %v\n", err)
		return ""
	}
	return path
}

// writeHarvestRunLog writes the one lasting record of a finished harvest run:
// its summary counts, every warning and diagnostic it printed, and each failed
// session with its reason. The terminal scrolls; this file does not, so a
// failure can be read again without re-running the harvest (which reads the
// native sources again and writes).
//
// It mirrors writePushErrorLog's location and permissions. A failure to write
// it never changes the run's exit code; the caller reports it as a warning.
func writeHarvestRunLog(result *ingest.PipelineResult, options harvestOutputOptions, stateDirOv string) (string, error) {
	path := harvestRunLogPath(stateDirOv, options.startedAt)
	if err := os.MkdirAll(filepath.Dir(path), defaults.PrivateDirPerm); err != nil {
		return "", fmt.Errorf("create log directory %s: %w", filepath.Dir(path), err)
	}
	var b strings.Builder
	renderHarvestRunLog(&b, result, options)
	if err := os.WriteFile(path, []byte(b.String()), defaults.PrivateFilePerm); err != nil {
		return "", fmt.Errorf("write log file %s: %w", path, err)
	}
	return path, nil
}

// renderHarvestRunLog renders the run log body. Sections always print, with
// "(none)" when empty, so a reader can tell "nothing went wrong" from "the
// section is missing".
func renderHarvestRunLog(w io.Writer, result *ingest.PipelineResult, options harvestOutputOptions) {
	command := options.command
	if command == "" {
		command = harvestCommandName(options.mode)
	}
	fmt.Fprintln(w, "peasant harvest run log")
	fmt.Fprintf(w, "started: %s\n", options.startedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "command: peasant %s\n", command)
	fmt.Fprintf(w, "output: %s\n", options.outputDir)
	fmt.Fprintf(w, "duration: %s\n", result.Duration.Round(100*time.Millisecond))
	fmt.Fprintln(w)

	s := result.Summary
	total := s.New + s.Updated + s.Unchanged + s.ExcludedBySelection + s.Active + s.Errors
	indexFailures := harvestIndexFailureReasons(result.IndexLog)
	fmt.Fprintf(w, "summary: %d sessions\n", total)
	fmt.Fprintf(w, "  new: %d\n", s.New)
	fmt.Fprintf(w, "  updated: %d\n", s.Updated)
	fmt.Fprintf(w, "  unchanged: %d\n", s.Unchanged)
	fmt.Fprintf(w, "  excluded by selection: %d\n", s.ExcludedBySelection)
	fmt.Fprintf(w, "  errors: %d\n", s.Errors)
	fmt.Fprintf(w, "  active: %d\n", s.Active)
	fmt.Fprintf(w, "  indexed: %d\n", s.Indexed)
	fmt.Fprintf(w, "  computed: %d\n", s.Computed)
	fmt.Fprintf(w, "  index failures: %d\n", len(indexFailures))
	fmt.Fprintln(w)

	warnings := harvestRunWarnings(result, options)
	fmt.Fprintf(w, "warnings: %d\n", len(warnings))
	if len(warnings) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, warning := range warnings {
		fmt.Fprintf(w, "  %s\n", strings.ReplaceAll(warning, "\n", "\n  "))
	}
	printRecordKindsReport(w, s.RefusedRecordKinds, s.RetainedUnknownKinds)
	fmt.Fprintln(w)

	var failed []string
	for _, sr := range result.Sessions {
		if sr.Error != nil {
			failed = append(failed, fmt.Sprintf("%s %s: %v", sr.Harness, sr.SessionID, sr.Error))
		}
	}
	for _, failure := range indexFailures {
		failed = append(failed, fmt.Sprintf("%s %s: not indexed: %s", failure.harness, failure.sessionID, failure.reason))
	}
	fmt.Fprintf(w, "failed sessions: %d\n", len(failed))
	if len(failed) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, line := range failed {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// harvestRunWarnings collects every warning and diagnostic the run printed, in
// the order the command prints them.
func harvestRunWarnings(result *ingest.PipelineResult, options harvestOutputOptions) []string {
	var warnings []string
	if options.scopeRun && result.ScopeReport != nil {
		warnings = append(warnings, result.ScopeReport.UnreadableWarnings(options.command, options.prefix)...)
	}
	if options.selectionConflicts != nil {
		warnings = append(warnings, options.selectionConflicts.Diagnostics(options.configPath)...)
	}
	for _, diagnostic := range result.Diagnostics {
		warnings = append(warnings, fmt.Sprintf("warning: %s: %s", diagnostic.Location, diagnostic.Message))
	}
	if result.Summary.StoreError != nil {
		warnings = append(warnings, fmt.Sprintf("warning: %v", result.Summary.StoreError))
	}
	for _, diagnostic := range result.DiscoveryDiagnostics {
		warnings = append(warnings, fmt.Sprintf("warning: %s discovery skipped %s: %s; %s", string(diagnostic.Provider), diagnostic.Location, diagnostic.Summary, diagnostic.Detail))
	}
	if coverage := result.IndexCoverage; coverage != nil {
		if coverage.Empty > 0 {
			warnings = append(warnings, fmt.Sprintf("warning: %d session(s) have no stored entries", coverage.Empty))
		}
		if coverage.FailedRetained > 0 {
			warnings = append(warnings, fmt.Sprintf("warning: %d session(s) failed to re-index but kept their previous entries", coverage.FailedRetained))
		}
	}
	return warnings
}

// harvestIndexFailure is one session the INDEX stage could not index, with the
// reason its last failed attempt recorded.
type harvestIndexFailure struct {
	sessionID ingest.SessionID
	harness   ingest.Harness
	reason    string
}

// harvestIndexFailureReasons names each session ingest.FailedIndexSessions
// counts as failed, with the reason of its last failed attempt, so the log and
// the summary's index failure count agree.
func harvestIndexFailureReasons(log []ingest.IndexLogEntry) []harvestIndexFailure {
	failedIDs := ingest.FailedIndexSessions(log)
	if len(failedIDs) == 0 {
		return nil
	}
	last := make(map[ingest.SessionID]ingest.IndexLogEntry, len(failedIDs))
	for _, entry := range log {
		if entry.Outcome == ingest.IndexOutcomeError {
			last[entry.SessionID] = entry
		}
	}
	failures := make([]harvestIndexFailure, 0, len(failedIDs))
	for _, sid := range failedIDs {
		entry := last[sid]
		reason := "no reason recorded"
		switch {
		case entry.ErrorMessage != nil && *entry.ErrorMessage != "":
			reason = *entry.ErrorMessage
		case entry.Reason != nil && *entry.Reason != "":
			reason = *entry.Reason
		}
		failures = append(failures, harvestIndexFailure{sessionID: sid, harness: entry.Harness, reason: reason})
	}
	return failures
}
