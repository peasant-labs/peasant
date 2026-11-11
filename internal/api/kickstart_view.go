package api

import (
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// viewLocked projects a session onto the wire. The setup model decides the
// stops, their order, visibility, fields, option menus, copy, and values; this
// function copies those decisions and adds none. The forest is not in the view:
// the page loads it from the tree route.
func (st *kickstartSessions) viewLocked(sess *kickstartSession) (schema.LocalKickstartSession, error) {
	facts := st.env.village()
	working := sess.working
	answers := sess.configAnswers()
	w := sess.writeContext(working, answers, facts)

	stops := make([]schema.LocalKickstartStop, 0, len(sess.model.Stops))
	for _, stop := range sess.model.Stops {
		row, err := sess.stopView(stop, w)
		if err != nil {
			return schema.LocalKickstartSession{}, err
		}
		stops = append(stops, row)
	}

	scope, err := schema.NewLocalKickstartScope(sess.scope.String())
	if err != nil {
		return schema.LocalKickstartSession{}, err
	}
	scan, err := sess.scan.wire()
	if err != nil {
		return schema.LocalKickstartSession{}, err
	}
	connect, err := sess.connectView(w)
	if err != nil {
		return schema.LocalKickstartSession{}, err
	}
	return schema.LocalKickstartSession{
		ID:        sess.id,
		Scope:     scope,
		Revision:  sess.revision,
		OpenedAt:  sess.openedAt.UTC().Format(time.RFC3339),
		Stops:     stops,
		Conflicts: append([]schema.LocalKickstartConflict{}, sess.conflicts...),
		Connect:   connect,
		Scan:      scan,
		Village:   schema.LocalKickstartVillage{Connected: facts.villageConnected, URL: facts.villageURL, UserID: facts.villageUserID},
	}, nil
}

func (s *kickstartSession) stopView(stop onboarding.Stop, w onboarding.WriteContext) (schema.LocalKickstartStop, error) {
	kind, err := schema.NewLocalKickstartStopKind(stop.Key.Kind().String())
	if err != nil {
		return schema.LocalKickstartStop{}, err
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
	for _, field := range stop.Fields {
		if field == nil || !onboarding.Visible(field, w) {
			continue
		}
		fieldRow, err := s.fieldView(stop, field, w, shown)
		if err != nil {
			return schema.LocalKickstartStop{}, err
		}
		row.Fields = append(row.Fields, fieldRow)
	}
	if stop.Key == onboarding.FlowReview {
		summary, err := onboarding.BuildConsentSummary(s.model, w)
		if err != nil {
			return schema.LocalKickstartStop{}, fmt.Errorf("build the review summary: %w", err)
		}
		row.Summary = &schema.LocalKickstartSummary{Values: nonNil(summary.Values), Effects: nonNil(summary.Effects)}
	}
	return row, nil
}

func (s *kickstartSession) fieldView(stop onboarding.Stop, field onboarding.Field, w onboarding.WriteContext, stopShown bool) (schema.LocalKickstartField, error) {
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
	if !stopShown {
		return row, nil
	}
	lines, err := field.Example(w)
	if err != nil {
		// The example is withheld and the reason travels with the field, so the
		// page never shows a sample the redactor did not produce.
		row.Refusal = &schema.LocalSettingRefusal{Key: row.Key, Error: err.Error()}
		return row, nil
	}
	for _, line := range lines {
		lineKind, err := schema.NewLocalKickstartLineKind(line.Kind.String())
		if err != nil {
			return schema.LocalKickstartField{}, err
		}
		row.Example = append(row.Example, schema.LocalKickstartLine{Kind: lineKind, Text: line.Text})
	}
	return row, nil
}

// fieldValue is the value the page shows for a field: the answer, or the value
// the working copy holds. The tree's value is the array of session ids it
// selects, whether the user answered or the saved selection prefilled it; the
// page passes that array to the picker as it is.
func (s *kickstartSession) fieldValue(field onboarding.Field, w onboarding.WriteContext) (schema.LocalSettingValue, error) {
	if field.Kind() == onboarding.FieldKindTree {
		return kickstartTreeWireValue(s.tree.SessionIDs)
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
