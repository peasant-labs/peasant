package ingest

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

//go:embed testdata/codex_native_event_census.yaml
var codexNativeEventCensusYAML []byte

// The native event_msg census is production-owned: the dispatch declaration in
// codexNativeEventDispatch is what the replay consults, what the candidate
// boundary admits, and what the record-kind vocabulary reports. The cases below
// pin that closed set exactly and witness every declared arm through the
// production entry point, so an added or removed arm cannot pass unnoticed.
type codexNativeEventCensusFixture struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name            string                 `yaml:"name"`
		Census          string                 `yaml:"census"`
		Want            []string               `yaml:"want"`
		Admission       string                 `yaml:"admission"`
		Event           string                 `yaml:"event"`
		Payload         map[string]any         `yaml:"payload"`
		Events          []codexNativeEventCase `yaml:"events"`
		Mode            CodexHistoryMode       `yaml:"mode"`
		WantDiagnostics []string               `yaml:"want_diagnostics"`
		WantPendedItems []string               `yaml:"want_pending"`
	} `yaml:"cases"`
}

type codexNativeEventCase struct {
	Event   string         `yaml:"event"`
	Payload map[string]any `yaml:"payload"`
}

// codexNativeEventPayload decodes a fixture payload through the production
// payload type, so a fixture field reaches the dispatch as the wire field it
// names.
func codexNativeEventPayload(t *testing.T, fields map[string]any) codexHistoryReplayPayload {
	t.Helper()
	var payload codexHistoryReplayPayload
	if len(fields) == 0 {
		return payload
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// codexNativeEventLine is one native event_msg rollout line.
func codexNativeEventLine(event string) []byte {
	return []byte(fmt.Sprintf(`{"type":"event_msg","payload":{"type":%q}}`, event))
}

// codexNativeEventPosition is the production source position for the first line
// of a captured rollout, so retained evidence carries the coordinates the
// candidate boundary assigns in a real capture.
func codexNativeEventPosition() UnknownSourcePosition {
	segment := codexDecodedSegment{descriptor: CodexReference{PhysicalSourceID: "census-rollout-1", Pointer: "census-rollout-1"}}
	return codexNativeUnknownPosition("census", segment, codexHistoryRecord{})
}

func TestCodexNativeEventProductionCensus(t *testing.T) {
	var fixture codexNativeEventCensusFixture
	decodeRegistryFixture(t, codexNativeEventCensusYAML, &fixture)
	names := make(map[string]bool, len(fixture.Cases))
	witnessed := make(map[string]bool, len(codexNativeEventDispatch))
	for _, row := range fixture.Cases {
		if names[row.Name] {
			t.Fatalf("duplicate fixture %q", row.Name)
		}
		names[row.Name] = true
		if row.Event != "" {
			witnessed[row.Event] = true
		}
		for _, event := range row.Events {
			witnessed[event.Event] = true
		}
	}
	checkRegistryFixtureNames(t, names, fixture.RequiredNames)
	for kind := range codexNativeEventDispatch {
		if !witnessed[string(kind)] {
			t.Errorf("declared native event %q has no dispatch witness; add one before adding the arm", kind)
		}
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			switch {
			case row.Census != "":
				assertCodexNativeEventCensusMembership(t, row.Census, row.Want)
			case row.Admission != "":
				assertCodexNativeEventAdmission(t, row.Admission, row.Event)
			case row.Event != "" || len(row.Events) != 0:
				assertCodexNativeEventDispatch(t, row.Mode, row.Event, row.Payload, row.Events, row.WantDiagnostics, row.WantPendedItems)
			default:
				t.Fatal("fixture case states no census, admission or dispatch expectation")
			}
		})
	}
}

// assertCodexNativeEventCensusMembership pins one closed set exactly: the
// types the replay dispatches, and the subset the candidate boundary adds
// beyond the retained-format list. A silent shrink or growth of either set is a
// change of behavior, not a reporting detail.
func assertCodexNativeEventCensusMembership(t *testing.T, census string, want []string) {
	t.Helper()
	var got []string
	switch census {
	case "dispatch":
		for kind := range codexNativeEventDispatch {
			got = append(got, string(kind))
		}
	case "native-only":
		for kind := range codexNativeOnlyEventTypes {
			got = append(got, string(kind))
		}
	default:
		t.Fatalf("unknown fixture census %q", census)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("census %s = %v, want %v", census, got, want)
	}
}

// assertCodexNativeEventAdmission runs the production candidate boundary: every
// declared native event is interpreted rather than retained, and an undeclared
// one stays retained opaque evidence at its source coordinates.
func assertCodexNativeEventAdmission(t *testing.T, admission, event string) {
	t.Helper()
	switch admission {
	case "declared":
		for kind := range codexNativeEventDispatch {
			prepared, evidence, err := prepareCodexRecord(codexNativeEventLine(string(kind)), codexNativeEventPosition(), true)
			if err != nil || prepared == nil || len(evidence) != 0 {
				t.Errorf("declared native event %q: prepared %d bytes, evidence %+v, err %v", kind, len(prepared), evidence, err)
			}
		}
	case "undeclared":
		if event == "" {
			t.Fatal("fixture names no undeclared event")
		}
		if _, declared := codexNativeEventDispatch[codexNativeEventType(event)]; declared {
			t.Fatalf("fixture event %q is declared", event)
		}
		prepared, evidence, err := prepareCodexRecord(codexNativeEventLine(event), codexNativeEventPosition(), true)
		if err != nil {
			t.Fatal(err)
		}
		if prepared != nil || len(evidence) != 1 {
			t.Fatalf("undeclared native event: prepared %d bytes, evidence %+v", len(prepared), evidence)
		}
		retained := evidence[0]
		if retained.Namespace != "event" || retained.Kind != event || retained.Position.JSONPointer != "/payload" {
			t.Fatalf("retained evidence %+v does not locate the undeclared payload", retained)
		}
	default:
		t.Fatalf("unknown fixture admission %q", admission)
	}
}

// assertCodexNativeEventDispatch witnesses the named events through the
// production dispatch. A fixture states the diagnostics the arm must report and
// the identities it must pend; an arm that stops acting, or an event that falls
// through to the default, turns the case red.
func assertCodexNativeEventDispatch(t *testing.T, mode CodexHistoryMode, event string, payload map[string]any, sequence []codexNativeEventCase, wantDiagnostics, wantPended []string) {
	t.Helper()
	events := sequence
	if event != "" {
		if len(events) != 0 {
			t.Fatal("fixture names both a single event and a sequence")
		}
		events = []codexNativeEventCase{{Event: event, Payload: payload}}
	}
	if mode == "" {
		mode = CodexHistoryModeLegacy
	}
	// The witnessed arms report diagnostics and pended identities; only an arm
	// that emits a captured node needs a ref registry.
	state := newCodexReplayState(nil)
	for i, step := range events {
		decoded := codexNativeEventPayload(t, step.Payload)
		decoded.Type = step.Event
		record := codexHistoryRecord{LineIndex: int64(i), Ordinal: int64(i + 1), HasOrdinal: true}
		if err := state.replayEventMessage("census", codexDecodedSegment{}, record, decoded, CodexOwnershipOwn, mode); err != nil {
			t.Fatalf("event %q: %v", step.Event, err)
		}
	}
	reported := make([]string, 0, len(state.diagnostics))
	for _, diagnostic := range state.diagnostics {
		reported = append(reported, diagnostic.ErrorType)
	}
	slices.Sort(reported)
	if !slices.Equal(reported, wantDiagnostics) {
		t.Errorf("diagnostics %v, want %v", reported, wantDiagnostics)
	}
	pended := make([]string, 0, len(state.boundary.pending))
	for id := range state.boundary.pending {
		pended = append(pended, id)
	}
	slices.Sort(pended)
	if !slices.Equal(pended, wantPended) {
		t.Errorf("pended identities %v, want %v", pended, wantPended)
	}
	if len(state.nodes) != 0 {
		t.Errorf("event dispatch emitted %d captured nodes", len(state.nodes))
	}
}
