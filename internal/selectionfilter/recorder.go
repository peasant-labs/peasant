package selectionfilter

import (
	"fmt"
	"io"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// EntrySeparator joins the disagreeing entries in the withheld-conflict
// warning. It is named because the boundary is load-bearing for a reader: each
// entry's identity has to be findable as one run of text, not assembled from
// pieces of several, so anything asserting about one entry needs the same
// boundary the renderer used.
const EntrySeparator = " and "

// noticeLimit is how many withheld sessions a run names individually. The
// rest are counted in one summary line so the notice cannot grow without bound.
const noticeLimit = 10

type selectionConflict struct {
	session ingest.DiscoveredSession
	branch  string
	// entries are the disagreeing entries reported by the matcher itself, not
	// every entry configured for the harness. Naming the whole harness would
	// grow less actionable the more projects a user configures.
	entries []ingest.SelectionEntry
}

// ConflictRecorder records each withheld selection conflict once. Notice writes
// the same lines the harvest command prints. Diagnostics returns those lines
// for a caller that has no stderr, such as the web ingest.
type ConflictRecorder struct {
	conflicts []selectionConflict
}

// Recorded reports how many withheld conflicts have been stored. A repeated
// match of the same session does not increase the count.
func (r *ConflictRecorder) Recorded() int {
	if r == nil {
		return 0
	}
	return len(r.conflicts)
}

// Notice writes the withheld-conflict warning to w. configPath is the file the
// reader should edit. A nil recorder or writer writes nothing.
func (r *ConflictRecorder) Notice(w io.Writer, configPath string) {
	if r == nil || w == nil {
		return
	}
	for _, line := range r.Diagnostics(configPath) {
		fmt.Fprintln(w, line)
	}
}

// Diagnostics returns the withheld-conflict lines Notice would write, without
// a trailing newline. The slice is empty when nothing was withheld.
func (r *ConflictRecorder) Diagnostics(configPath string) []string {
	if r == nil || len(r.conflicts) == 0 {
		return nil
	}
	lines := make([]string, 0, len(r.conflicts))
	for i, conflict := range r.conflicts {
		if i == noticeLimit {
			lines = append(lines, fmt.Sprintf("warning: %d additional sessions were withheld because selection rules conflict; inspect %q and edit one project entry or add the branch", len(r.conflicts)-noticeLimit, configPath))
			break
		}
		lines = append(lines, fmt.Sprintf("warning: session %s (%s) was withheld during ingest because project selection entries %s disagree on branch %q in %q; the session was not ingested; edit one entry or add the branch", conflict.session.SessionID, conflict.session.Harness, describeEntries(conflict.entries), conflict.branch, configPath))
	}
	return lines
}

func (r *ConflictRecorder) record(conflict selectionConflict, seen map[ingest.SessionID]bool) {
	if r == nil || seen[conflict.session.SessionID] {
		return
	}
	r.conflicts = append(r.conflicts, conflict)
	seen[conflict.session.SessionID] = true
}

func (r *ConflictRecorder) reset() {
	if r == nil {
		return
	}
	r.conflicts = nil
}

// describeEntries renders the disagreeing entries by the identity the user
// wrote in their configuration, so the warning names text they can search for.
func describeEntries(entries []ingest.SelectionEntry) string {
	rendered := make([]string, len(entries))
	for i, entry := range entries {
		rendered[i] = entry.String()
	}
	return strings.Join(rendered, EntrySeparator)
}
