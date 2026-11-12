package api

import (
	"fmt"
	"log/slog"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionfilter"
)

// appendSelectionConflictDiagnostics records withheld selection conflicts where
// a web ingest can be read without a terminal. Each line is logged and copied
// onto the pipeline result. A nil result still logs, so a failed run does not
// hide the reason a session was left out.
func appendSelectionConflictDiagnostics(result *ingest.PipelineResult, recorder *selectionfilter.ConflictRecorder, configPath string) {
	if recorder == nil {
		return
	}
	for _, line := range recorder.Diagnostics(configPath) {
		slog.Warn("selection conflict withheld session", "diagnostic", line)
		if result == nil {
			continue
		}
		result.Diagnostics = append(result.Diagnostics, ingest.DiagnosticEntry{
			ErrorType:   "selection_withheld_conflict",
			Location:    configPath,
			Message:     line,
			Remediation: "Edit one of the disagreeing project entries, or add the branch, in the configuration file named in this warning, then run ingest again.",
		})
	}
}

// selectionConflictNote is what a finished import says beside its counts when
// the saved selection withheld sessions because its rules disagree: how many,
// and where to fix it. The lines that name each session are in the log and on
// the run's diagnostics; the import runs after the setup's save has answered, so
// they are not known when the save's own answer is written, and this note is the
// way they reach the setup page. It is empty when nothing was withheld.
func selectionConflictNote(withheld int, configPath string) string {
	switch {
	case withheld <= 0:
		return ""
	case withheld == 1:
		return fmt.Sprintf("1 session was left out because the selection rules for its project disagree on its branch; edit one entry or add the branch in %s, then import again", configPath)
	default:
		return fmt.Sprintf("%d sessions were left out because the selection rules for their projects disagree on a branch; edit one entry or add the branch in %s, then import again", withheld, configPath)
	}
}
