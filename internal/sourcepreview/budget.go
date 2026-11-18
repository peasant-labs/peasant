package sourcepreview

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// GrownNotice is the sentence the last page of a capture carries when the
// source grew after the capture fixed its extent. The capture never reads past
// that extent, so the newer part needs a new preview.
const GrownNotice = "this session has grown since the preview opened. open the preview again to read the newer part."

// The cut notices say what the preview shows, why it stopped and that the
// session itself is whole. They name sizes and counts only: never a path, an
// identifier or source text.

func sourceBudgetNotice(loaded, total, capBytes int64) string {
	if total > 0 {
		return fmt.Sprintf("the preview stopped after the first %s of this %s session, the most one preview reads (%s). the full session ingests normally.",
			discovery.PreviewByteSize(loaded), discovery.PreviewByteSize(total), discovery.PreviewByteSize(capBytes))
	}
	return fmt.Sprintf("the preview stopped after the first %s of this session, the most one preview reads. the full session ingests normally.",
		discovery.PreviewByteSize(loaded))
}

func turnBudgetNotice(turnCap int) string {
	return fmt.Sprintf("the preview stopped after %d turns, the most one preview shows. the full session ingests normally.", turnCap)
}

func serializedBudgetNotice(capBytes int64) string {
	return fmt.Sprintf("the preview stopped after %s of turns, the most one preview holds. the full session ingests normally.",
		discovery.PreviewByteSize(capBytes))
}

func oversizedNotice(err *ingest.RecordExceedsBudgetError) string {
	where := "the next record"
	switch {
	case err.Line > 0:
		where = fmt.Sprintf("line %d", err.Line)
	case err.Row > 0:
		where = fmt.Sprintf("row %d", err.Row)
	}
	return fmt.Sprintf("the preview stopped at %s: that record is at least %s, more than the %s this preview can still read. the full session ingests normally.",
		where, discovery.PreviewByteSize(err.AtLeastBytes), discovery.PreviewByteSize(err.CeilingBytes))
}

func notResumableNotice(loaded, total int64) string {
	if loaded > 0 && total > 0 {
		return fmt.Sprintf("the preview shows the first %s of this %s session; this source cannot be read further while a preview is open. the full session ingests normally.",
			discovery.PreviewByteSize(loaded), discovery.PreviewByteSize(total))
	}
	return "the preview cannot read this session in parts, and it is larger than one preview reads. the full session ingests normally."
}
