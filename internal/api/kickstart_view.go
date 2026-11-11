package api

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// A view is built in two steps, so that no request holds the store lock while
// the file is read or the redactor runs.
//
// draftView builds everything that needs only the session: every stop with its
// fields, options and values, the summary from the working copy, the conflict
// rows the session holds, the scan, the connect offer and the sign-in. It is
// the part that can fail, and it is built while the store lock is held, so a
// change whose view cannot be built is refused and rolled back there.
//
// completeView adds what needs the file or the redactor: the review's conflict
// rows and diff, the summary explained over the configuration the save would
// produce, and the examples of the stops that show one. It cannot fail a
// request. What it cannot add it leaves out and says so: an example the redactor
// refused travels as that field's refusal, and a review that cannot be built
// leaves the view without a diff.

// kickstartViewDraft is a session's view with its slow parts missing. It holds a
// frozen copy of the session, so completing it needs no lock.
type kickstartViewDraft struct {
	view  schema.LocalKickstartSession
	sess  *kickstartSession
	facts kickstartFacts
	w     onboarding.WriteContext
	// examples are the fields whose example is still to be added.
	examples []kickstartExampleSlot
	// reviewStop is the index of the review stop in view.Stops, or -1.
	reviewStop int
}

// kickstartExampleSlot places a field whose example completeView adds.
type kickstartExampleSlot struct {
	stop, field int
	of          onboarding.Field
}

// draftView projects a frozen session onto the wire, without the parts that read
// the file or run the redactor. The setup model decides the stops, their order,
// visibility, fields, option menus, copy, and values; this function copies those
// decisions and adds none. The forest is not in the view: the page loads it from
// the tree route.
func (st *kickstartSessions) draftView(sess *kickstartSession) (*kickstartViewDraft, error) {
	facts := st.env.village()
	answers := sess.configAnswers()
	w := sess.writeContext(sess.working, answers, facts)
	draft := &kickstartViewDraft{sess: sess, facts: facts, w: w, reviewStop: -1}

	stops := make([]schema.LocalKickstartStop, 0, len(sess.model.Stops))
	for index, stop := range sess.model.Stops {
		row, examples, err := sess.stopView(stop, w)
		if err != nil {
			return nil, err
		}
		for _, slot := range examples {
			slot.stop = index
			draft.examples = append(draft.examples, slot)
		}
		if stop.Key == onboarding.FlowReview {
			draft.reviewStop = index
		}
		stops = append(stops, row)
	}

	scope, err := schema.NewLocalKickstartScope(sess.scope.String())
	if err != nil {
		return nil, err
	}
	scan, err := sess.scan.wire()
	if err != nil {
		return nil, err
	}
	connect, err := sess.connectView(w)
	if err != nil {
		return nil, err
	}
	draft.view = schema.LocalKickstartSession{
		ID:        sess.id,
		Scope:     scope,
		Revision:  sess.revision,
		OpenedAt:  sess.openedAt.UTC().Format(time.RFC3339),
		Stops:     stops,
		Conflicts: append([]schema.LocalKickstartConflict{}, sess.conflicts...),
		Connect:   connect,
		Scan:      scan,
		Village:   schema.LocalKickstartVillage{Connected: facts.villageConnected, URL: facts.villageURL, UserID: facts.villageUserID},
	}
	return draft, nil
}

// completeView adds the parts of a view that read the file and run the redactor.
// The review previews the save with the commit's own chain, so a save by the
// other scope shows up as a row before this session's own save.
func (st *kickstartSessions) completeView(d *kickstartViewDraft) {
	for _, slot := range d.examples {
		row := &d.view.Stops[slot.stop].Fields[slot.field]
		lines, err := slot.of.Example(d.w)
		if err != nil {
			// The example is withheld and the reason travels with the field, so
			// the page never shows a sample the redactor did not produce.
			row.Refusal = &schema.LocalSettingRefusal{Key: row.Key, Error: err.Error()}
			continue
		}
		example, err := exampleLines(lines)
		if err != nil {
			slog.Error("kickstart: an example does not project onto the wire and was withheld", "field", row.Key, "error", err)
			row.Refusal = &schema.LocalSettingRefusal{Key: row.Key, Error: err.Error()}
			continue
		}
		row.Example = example
	}

	review, reviewed := st.reviewOf(d.sess, d.facts)
	if !reviewed {
		return
	}
	d.view.Conflicts = reviewRows(d.sess.conflicts, review)
	if d.reviewStop < 0 {
		return
	}
	stop := &d.view.Stops[d.reviewStop]
	diff := review.Diff
	stop.Diff = &diff
	if review.Saved == nil {
		return
	}
	// The working copy is the snapshot the file had when the session opened, with
	// the answers on top, and says nothing of a value the file took since or of
	// an answer a "theirs" choice gave up; the configuration the save would
	// produce does.
	explained := d.w
	explained.Config = review.Saved
	summary, err := onboarding.BuildConsentSummary(d.sess.model, explained)
	if err != nil {
		slog.Error("kickstart: the review summary could not be explained over the saved configuration; the working copy's stands", "session", d.sess.id, "error", err)
		return
	}
	stop.Summary = &schema.LocalKickstartSummary{Values: nonNil(summary.Values), Effects: nonNil(summary.Effects)}
}

// viewOf builds the whole view of a frozen session, for a read: the lock is not
// held, so the slow parts are added at once.
func (st *kickstartSessions) viewOf(frozen *kickstartSession) (schema.LocalKickstartSession, error) {
	draft, err := st.draftView(frozen)
	if err != nil {
		return schema.LocalKickstartSession{}, err
	}
	st.completeView(draft)
	return draft.view, nil
}

// stopView projects one stop without its examples, and lists the fields whose
// example completeView adds.
func (s *kickstartSession) stopView(stop onboarding.Stop, w onboarding.WriteContext) (schema.LocalKickstartStop, []kickstartExampleSlot, error) {
	kind, err := schema.NewLocalKickstartStopKind(stop.Key.Kind().String())
	if err != nil {
		return schema.LocalKickstartStop{}, nil, err
	}
	shown := stop.Shown(w)
	row := schema.LocalKickstartStop{
		Key:     stop.Key.String(),
		Kind:    kind,
		Title:   stop.Title,
		Visible: shown,
		Guide:   schema.LocalKickstartGuide{Intro: stop.Guide.Intro, Hints: append([]string(nil), stop.Guide.Hints...)},
		Fields:  []schema.LocalKickstartField{},
	}
	if !shown {
		for _, condition := range stop.Visible {
			if !condition.Evaluate(w) {
				row.HiddenBecause = condition.Kind.String()
				break
			}
		}
	}
	var examples []kickstartExampleSlot
	for _, field := range stop.Fields {
		if field == nil || !onboarding.Visible(field, w) {
			continue
		}
		fieldRow, err := s.fieldView(stop, field, w)
		if err != nil {
			return schema.LocalKickstartStop{}, nil, err
		}
		if shown {
			examples = append(examples, kickstartExampleSlot{field: len(row.Fields), of: field})
		}
		row.Fields = append(row.Fields, fieldRow)
	}
	if stop.Key == onboarding.FlowReview {
		summary, err := onboarding.BuildConsentSummary(s.model, w)
		if err != nil {
			return schema.LocalKickstartStop{}, nil, fmt.Errorf("build the review summary: %w", err)
		}
		row.Summary = &schema.LocalKickstartSummary{Values: nonNil(summary.Values), Effects: nonNil(summary.Effects)}
	}
	return row, examples, nil
}

func (s *kickstartSession) fieldView(stop onboarding.Stop, field onboarding.Field, w onboarding.WriteContext) (schema.LocalKickstartField, error) {
	kind, err := schema.NewLocalKickstartFieldKind(field.Kind().String())
	if err != nil {
		return schema.LocalKickstartField{}, err
	}
	value, err := s.fieldValue(field, w)
	if err != nil {
		return schema.LocalKickstartField{}, err
	}
	label := field.Label()
	if label == "" {
		label = stop.Title
	}
	row := schema.LocalKickstartField{
		Key:         field.Key().String(),
		Kind:        kind,
		Label:       label,
		Description: field.Description(),
		Value:       value,
	}
	for _, option := range field.Options() {
		row.Options = append(row.Options, schema.LocalKickstartOption{Value: option.Value, Label: option.Label, Description: option.Description})
	}
	return row, nil
}

// exampleLines projects the lines of a field's example onto the wire.
func exampleLines(lines []onboarding.ExampleLine) ([]schema.LocalKickstartLine, error) {
	var out []schema.LocalKickstartLine
	for _, line := range lines {
		lineKind, err := schema.NewLocalKickstartLineKind(line.Kind.String())
		if err != nil {
			return nil, err
		}
		out = append(out, schema.LocalKickstartLine{Kind: lineKind, Text: line.Text})
	}
	return out, nil
}

// fieldValue is the value the page shows for a field: the answer, or the value
// the working copy holds. The tree's value is the array of session ids it
// selects, whether the user answered or the saved selection prefilled it; the
// page passes that array to the picker as it is.
func (s *kickstartSession) fieldValue(field onboarding.Field, w onboarding.WriteContext) (schema.LocalSettingValue, error) {
	if field.Kind() == onboarding.FieldKindTree {
		return onboarding.SelectionIDsListValue(s.tree.SessionIDs)
	}
	if answer, ok := s.answers[field.Key()]; ok {
		return append(schema.LocalSettingValue(nil), answer...), nil
	}
	return field.Read(w.Config)
}

// connectView evaluates the boundary offer. The page renders it and never
// recomputes its conditions.
func (s *kickstartSession) connectView(w onboarding.WriteContext) (schema.LocalKickstartConnect, error) {
	for _, offer := range s.model.ConnectOffers() {
		if shown, ok := s.model.ConnectOfferAfter(offer.OfferAfter, w); ok {
			return schema.LocalKickstartConnect{Offered: true, AfterStop: shown.OfferAfter.String(), Prompt: shown.Prompt, Skip: shown.Skip}, nil
		}
	}
	return schema.LocalKickstartConnect{}, nil
}

func nonNil(lines []string) []string {
	if lines == nil {
		return []string{}
	}
	return lines
}

// wire projects the discovery job. The forest is not in it.
func (r kickstartScanRecord) wire() (schema.LocalKickstartScan, error) {
	state, err := schema.NewLocalKickstartScanState(r.state.String())
	if err != nil {
		return schema.LocalKickstartScan{}, err
	}
	scan := schema.LocalKickstartScan{
		State:             state,
		Phase:             r.phase,
		Done:              r.done,
		Total:             r.total,
		Error:             r.err,
		LockedNodes:       r.locked,
		DroppedSelections: r.droppedCount,
	}
	if len(r.droppedLabels) > 0 {
		scan.DroppedLabels = append([]string(nil), r.droppedLabels...)
	}
	return scan, nil
}
