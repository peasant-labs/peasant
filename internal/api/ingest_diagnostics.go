package api

import (
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
