package main

import (
	"io"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionfilter"
)

// entrySeparator is the same boundary the withheld-conflict notice uses, so a
// test that splits one rendered entry from the next uses the renderer boundary.
const entrySeparator = selectionfilter.EntrySeparator

// selectionConflictRecorder keeps the notice and conflict count the existing
// harvest tests read. The lines are rendered by the shared selection filter.
type selectionConflictRecorder struct {
	inner     *selectionfilter.ConflictRecorder
	conflicts []struct{}
}

func (r *selectionConflictRecorder) notice(w io.Writer, configPath string) {
	if r == nil || r.inner == nil {
		return
	}
	r.inner.Notice(w, configPath)
}

func (r *selectionConflictRecorder) sync() {
	if r == nil || r.inner == nil {
		return
	}
	recorded := r.inner.Recorded()
	if recorded == len(r.conflicts) {
		return
	}
	r.conflicts = make([]struct{}, recorded)
}

// harvestSelectionFilter delegates cohort preparation and matching to the shared
// selection filter. Match refreshes the recorder view this package's tests read.
type harvestSelectionFilter struct {
	*selectionfilter.Filter
	recorder *selectionConflictRecorder
}

func (f *harvestSelectionFilter) Match(session ingest.DiscoveredSession) bool {
	if f == nil || f.Filter == nil {
		return false
	}
	got := f.Filter.Match(session)
	f.recorder.sync()
	return got
}

func buildSelectionFilterWithResolver(
	cfg *config.Config,
	git ingest.GitResolver,
	pathResolver ingest.PathIdentityResolver,
) (*harvestSelectionFilter, *selectionConflictRecorder) {
	filter, recorder := selectionfilter.New(cfg, git, pathResolver)
	wrapped := &selectionConflictRecorder{inner: recorder}
	return &harvestSelectionFilter{Filter: filter, recorder: wrapped}, wrapped
}
