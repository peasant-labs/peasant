package push

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/perf"
)

// profileStage times consecutive parts of a session without changing control
// flow. Every early return ends the current stage through finish. Only static
// diagnostics reach the recorder: dependency errors may contain private text.
type profileStage struct {
	rec    perf.Recorder
	parent string
	attrs  perf.Attributes
	stage  perf.StageID
	span   perf.Span
}

func startProfileStage(rec perf.Recorder, parent string, attrs perf.Attributes, stage perf.StageID) *profileStage {
	return &profileStage{rec: rec, parent: parent, attrs: attrs, stage: stage, span: rec.StartChildSpan(stage, parent, attrs)}
}

func (s *profileStage) next(stage perf.StageID) {
	s.span.End(perf.OutcomeOK, nil)
	s.stage = stage
	s.span = s.rec.StartChildSpan(stage, s.parent, s.attrs)
}

func (s *profileStage) finish(err error) {
	outcome := perf.OutcomeOK
	if err != nil {
		outcome = perf.OutcomeFailed
		s.rec.Error(s.stage, fmt.Errorf("push operation failed; inspect command diagnostics and repair the input or service before retrying"), s.attrs)
	}
	s.span.End(outcome, nil)
}

// sessionRecorder adds attribution to downstream transport events while using
// the same collector. It stores no transcript data and owns no recording state.
type sessionRecorder struct {
	perf.Recorder
	subject string
}

var _ perf.Recorder = sessionRecorder{}

func (r sessionRecorder) attributes(attrs perf.Attributes) perf.Attributes {
	merged := make(perf.Attributes, len(attrs)+1)
	for key, value := range attrs {
		merged[key] = value
	}
	merged[perf.AttrSafeSubjectID] = r.subject
	return merged
}

func (r sessionRecorder) StartSpan(stage perf.StageID, attrs perf.Attributes) perf.Span {
	return r.Recorder.StartSpan(stage, r.attributes(attrs))
}

func (r sessionRecorder) StartChildSpan(stage perf.StageID, parent string, attrs perf.Attributes) perf.Span {
	return r.Recorder.StartChildSpan(stage, parent, r.attributes(attrs))
}

func (r sessionRecorder) Count(name perf.CounterName, delta int64, unit perf.Unit, attrs perf.Attributes) {
	r.Recorder.Count(name, delta, unit, r.attributes(attrs))
}

func (r sessionRecorder) Error(stage perf.StageID, err error, attrs perf.Attributes) {
	r.Recorder.Error(stage, err, r.attributes(attrs))
}
