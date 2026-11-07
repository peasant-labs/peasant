package transcript

import (
	_ "embed"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/control_record_projection.yaml
var controlRecordProjectionYAML []byte

//go:embed testdata/control_record_projection.manifest.yaml
var controlRecordProjectionManifestYAML []byte

type controlRecordProjectionEntry struct {
	Index      int     `yaml:"index"`
	Role       string  `yaml:"role"`
	EntryType  string  `yaml:"entry_type"`
	PartType   string  `yaml:"part_type,omitempty"`
	Content    *string `yaml:"content,omitempty"`
	SourceRef  string  `yaml:"source_ref,omitempty"`
	Depth      int     `yaml:"depth,omitempty"`
	HasToolUse bool    `yaml:"has_tool_use,omitempty"`
}

type controlRecordProjectionSection struct {
	State   string                         `yaml:"state"`
	Entries []controlRecordProjectionEntry `yaml:"entries"`
}

type controlRecordProjectionTurn struct {
	Index       int    `yaml:"index"`
	Role        string `yaml:"role"`
	Content     string `yaml:"content,omitempty"`
	ControlKind string `yaml:"control_kind,omitempty"`
}

type controlRecordProjectionExpected struct {
	Turns               []controlRecordProjectionTurn `yaml:"turns"`
	EarlierHistoryTurns []controlRecordProjectionTurn `yaml:"earlier_history_turns,omitempty"`
	TurnCount           *int                          `yaml:"turn_count,omitempty"`
}

type controlRecordProjectionCase struct {
	Name           string                           `yaml:"name"`
	Level          string                           `yaml:"level"`
	Harness        string                           `yaml:"harness"`
	Entries        []controlRecordProjectionEntry   `yaml:"entries"`
	EarlierHistory []controlRecordProjectionSection `yaml:"earlier_history,omitempty"`
	Expected       controlRecordProjectionExpected  `yaml:"expected"`
}

func loadControlRecordProjectionFixtures(t *testing.T) []controlRecordProjectionCase {
	t.Helper()
	var document struct {
		Cases []controlRecordProjectionCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(controlRecordProjectionYAML, &document); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Required []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(controlRecordProjectionManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	required := make(map[string]bool, len(manifest.Required))
	for _, name := range manifest.Required {
		if name == "" || required[name] {
			t.Fatalf("control-record projection manifest repeats or omits required name %q", name)
		}
		required[name] = true
	}
	actual := make(map[string]bool, len(document.Cases))
	for _, fixture := range document.Cases {
		if fixture.Name == "" || actual[fixture.Name] {
			t.Fatalf("invalid control-record projection fixture %q", fixture.Name)
		}
		actual[fixture.Name] = true
		if !required[fixture.Name] {
			t.Fatalf("control-record projection fixture %q is not in the required-name manifest; add it so the case cannot silently disappear", fixture.Name)
		}
		switch fixture.Level {
		case "entries_to_turns", "session_to_detail":
		default:
			t.Fatalf("fixture %q names unknown level %q", fixture.Name, fixture.Level)
		}
	}
	for name := range required {
		if !actual[name] {
			t.Fatalf("missing required control-record projection fixture %s", name)
		}
	}
	return document.Cases
}

func controlRecordProjectionSessionID() ingest.SessionID {
	return ingest.SessionID("9f6a1d21-4c30-4f7d-8b5e-2a1c0d9e8f77")
}

func controlRecordProjectionEntries(t *testing.T, harness schema.Harness, rows []controlRecordProjectionEntry) []schema.SessionEntry {
	t.Helper()
	out := make([]schema.SessionEntry, len(rows))
	for i, row := range rows {
		if harness == schema.HarnessPi && row.PartType == ingest.PiCarrierPartType {
			carrier, err := ingest.NewPiCarrier(controlRecordProjectionSessionID(), row.Index, ingest.PiExtra{
				Kind:    ingest.PiExtraCarrier,
				Harness: schema.HarnessPi,
			})
			if err != nil {
				t.Fatalf("build Pi carrier row %d: %v", row.Index, err)
			}
			out[i] = carrier
			continue
		}
		entry := schema.SessionEntry{
			SessionID:      controlRecordProjectionSessionID(),
			EntryIndex:     row.Index,
			Harness:        harness,
			Role:           schema.Role(row.Role),
			EntryType:      schema.EntryType(row.EntryType),
			Depth:          row.Depth,
			ContentPreview: row.Content,
			SourceEntryRef: schema.SourceEntryRef(row.SourceRef),
			HasToolUse:     row.HasToolUse,
		}
		if row.PartType != "" {
			partType := row.PartType
			entry.PartType = &partType
		}
		if harness == schema.HarnessPi {
			// A conversational Pi row carries its typed source evidence; a Pi
			// session with an unevidenced row is refused rather than projected.
			extra, err := ingest.EncodePiExtra(ingest.PiExtra{
				Kind:      ingest.PiExtraState,
				Harness:   schema.HarnessPi,
				SourceRef: ingest.PiPublicRef(string(controlRecordProjectionSessionID()), "entry", row.SourceRef),
			})
			if err != nil {
				t.Fatalf("build Pi evidence for row %d: %v", row.Index, err)
			}
			entry.Extra = extra
		}
		out[i] = entry
	}
	return out
}

func assertControlRecordProjectionTurns(t *testing.T, got []ingest.Turn, want []controlRecordProjectionTurn) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("projected %d turns, want %d: got %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Index != want[i].Index {
			t.Fatalf("turn %d index = %d, want %d (indices are the source entry positions and must not be renumbered)", i, got[i].Index, want[i].Index)
		}
		if string(got[i].Role) != want[i].Role {
			t.Fatalf("turn %d role = %q, want %q", i, got[i].Role, want[i].Role)
		}
		if got[i].Content != want[i].Content {
			t.Fatalf("turn %d content = %q, want %q", i, got[i].Content, want[i].Content)
		}
		var kind string
		if got[i].ControlKind != nil {
			kind = *got[i].ControlKind
		}
		if kind != want[i].ControlKind {
			t.Fatalf("turn %d controlKind = %q, want %q", i, kind, want[i].ControlKind)
		}
	}
}

func assertControlRecordDetailTurns(t *testing.T, got []schema.TurnDetail, want []controlRecordProjectionTurn) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("projected %d wire turns, want %d: got %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Index != want[i].Index || string(got[i].Role) != want[i].Role || got[i].Content != want[i].Content {
			t.Fatalf("wire turn %d = {%d %q %q}, want {%d %q %q}", i, got[i].Index, got[i].Role, got[i].Content, want[i].Index, want[i].Role, want[i].Content)
		}
		var kind string
		if got[i].ControlKind != nil {
			kind = *got[i].ControlKind
		}
		if kind != want[i].ControlKind {
			t.Fatalf("wire turn %d controlKind = %q, want %q", i, kind, want[i].ControlKind)
		}
	}
}

// TestControlRecordProjection drives the two projections the viewer reads: the
// fold every reader shares, and the detail builder that copies the kind onto
// the wire. Cases are split by level in the case names so a reader can see
// which seam each one pins.
func TestControlRecordProjection(t *testing.T) {
	t.Parallel()
	fixtures := loadControlRecordProjectionFixtures(t)
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			harness := schema.Harness(fixture.Harness)
			entries := controlRecordProjectionEntries(t, harness, fixture.Entries)
			folded := EntriesToTurns(entries)

			switch fixture.Level {
			case "entries_to_turns":
				assertControlRecordProjectionTurns(t, folded, fixture.Expected.Turns)
			case "session_to_detail":
				session := &ingest.Session{
					ID:        controlRecordProjectionSessionID(),
					Harness:   harness,
					StartTime: time.UnixMilli(0).UTC(),
					EndTime:   time.UnixMilli(0).UTC(),
					Turns:     folded,
					Metadata:  ingest.SessionMetadata{TurnCount: 999},
				}
				for _, section := range fixture.EarlierHistory {
					sectionTurns := EntriesToTurns(controlRecordProjectionEntries(t, harness, section.Entries))
					session.EarlierHistory = append(session.EarlierHistory, ingest.EarlierHistorySection{
						State: schema.EarlierHistoryState(section.State),
						Turns: sectionTurns,
					})
				}
				detail := sessionToDetail(session)
				assertControlRecordDetailTurns(t, detail.Turns, fixture.Expected.Turns)
				if len(fixture.Expected.EarlierHistoryTurns) > 0 {
					if len(detail.EarlierHistory) != len(fixture.EarlierHistory) {
						t.Fatalf("detail carries %d earlier-history sections, want %d", len(detail.EarlierHistory), len(fixture.EarlierHistory))
					}
					assertControlRecordDetailTurns(t, detail.EarlierHistory[0].Turns, fixture.Expected.EarlierHistoryTurns)
				}
				if fixture.Expected.TurnCount != nil && detail.TurnCount != *fixture.Expected.TurnCount {
					t.Fatalf("detail turnCount = %d, want %d (conversation turns only, never stored metadata and never len(turns))", detail.TurnCount, *fixture.Expected.TurnCount)
				}
			default:
				t.Fatalf("unreachable level %q", fixture.Level)
			}
		})
	}
	if len(fixtures) == 0 {
		t.Fatal("control-record projection corpus is empty")
	}
}

// TestSessionDetail_TurnCountExcludesSystemEntries pins the ratified header
// rule: system entries stay served and revealable, but the turn count a reader
// sees counts conversation turns only.
func TestSessionDetail_TurnCountExcludesSystemEntries(t *testing.T) {
	t.Parallel()
	entries := controlRecordProjectionEntries(t, schema.HarnessClaudeCode, []controlRecordProjectionEntry{
		{Index: 0, Role: "user", EntryType: "text", Content: strPtr("run the checks")},
		{Index: 1, Role: "system", EntryType: "text", PartType: "mode", Content: strPtr("mode: normal")},
		{Index: 2, Role: "system", EntryType: "system", Content: strPtr("Build finished in 12s")},
		{Index: 3, Role: "assistant", EntryType: "text", Content: strPtr("checks are running")},
	})
	session := &ingest.Session{
		ID:       controlRecordProjectionSessionID(),
		Harness:  schema.HarnessClaudeCode,
		Turns:    EntriesToTurns(entries),
		Metadata: ingest.SessionMetadata{TurnCount: 4},
	}
	detail := sessionToDetail(session)
	if len(detail.Turns) != 4 {
		t.Fatalf("served turns = %d, want 4; system entries must stay served", len(detail.Turns))
	}
	systemTurns := 0
	for _, turn := range detail.Turns {
		if turn.Role == schema.RoleSystem {
			systemTurns++
		}
	}
	if systemTurns != 2 {
		t.Fatalf("served system turns = %d, want 2; hiding them from the count must not drop them from the payload", systemTurns)
	}
	if detail.TurnCount != 2 {
		t.Fatalf("turnCount = %d, want 2: the header counts conversation turns, not system entries", detail.TurnCount)
	}
	if got := ConversationTurnCount(detail.Turns); got != detail.TurnCount {
		t.Fatalf("ConversationTurnCount = %d but the detail reports %d; the count has one definition", got, detail.TurnCount)
	}
	if err := schema.ValidateSessionDetailPayload(*detail); err != nil {
		t.Fatalf("served detail is invalid: %v", err)
	}
}

func strPtr(value string) *string { return &value }
