package ingest

// WrittenCounts is how many sessions a run wrote: the ones the store did not
// hold before the run (New) and the ones it already held (Updated).
type WrittenCounts struct {
	New     int
	Updated int
}

// Written counts what the run wrote, for a caller that reports it to a person.
//
// The summary files a session whose source was still being written under
// Active, though the run captures and stores it like any other session. A
// caller that reports "N sessions imported" must count those too, and the run
// does not know whether the store held the row before it started. existed says
// that for one session id; the caller answers it from the store as it was
// before the run began. A session that failed, and one the saved selection left
// out, was not written and is not counted.
func (r *PipelineResult) Written(existed func(SessionID) bool) WrittenCounts {
	if r == nil {
		return WrittenCounts{}
	}
	counts := WrittenCounts{New: r.Summary.New, Updated: r.Summary.Updated}
	for _, session := range r.Sessions {
		if session.Status != DiffActive || session.Error != nil || session.ExcludedBySelection {
			continue
		}
		if existed(session.SessionID) {
			counts.Updated++
		} else {
			counts.New++
		}
	}
	return counts
}
