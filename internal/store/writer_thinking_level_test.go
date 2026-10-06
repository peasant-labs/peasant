package store_test

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/thinking_level_store.yaml
var thinkingLevelStoreFixtureBytes []byte

type thinkingLevelPair struct {
	Level string `yaml:"level"`
	Raw   string `yaml:"raw"`
}

type thinkingLevelChange struct {
	Level string `yaml:"level"`
	Raw   string `yaml:"raw"`
}

type thinkingLevelStoreFixture struct {
	RequiredRoundTripNames []string `yaml:"required_round_trip_names"`
	RequiredRefusalNames   []string `yaml:"required_refusal_names"`
	RoundTrips             []struct {
		Name         string                `yaml:"name"`
		SessionID    string                `yaml:"session_id"`
		First        thinkingLevelPair     `yaml:"first"`
		FirstHistory []thinkingLevelChange `yaml:"first_history"`
		Then         *thinkingLevelPair    `yaml:"then"`
		ThenHistory  []thinkingLevelChange `yaml:"then_history"`
	} `yaml:"round_trips"`
	Refusals []struct {
		Name              string                `yaml:"name"`
		SessionID         string                `yaml:"session_id"`
		Level             string                `yaml:"level"`
		History           []thinkingLevelChange `yaml:"history"`
		ExpectedFragments []string              `yaml:"expected_fragments"`
	} `yaml:"refusals"`
}

func loadThinkingLevelStoreFixture(t *testing.T) thinkingLevelStoreFixture {
	t.Helper()
	var fixture thinkingLevelStoreFixture
	if err := testutil.DecodeFixtureYAML(thinkingLevelStoreFixtureBytes, &fixture); err != nil {
		t.Fatalf("decode thinking-level store fixture: %v", err)
	}
	roundTrips := map[string]bool{}
	for _, c := range fixture.RoundTrips {
		if roundTrips[c.Name] {
			t.Fatalf("thinking-level store fixture: duplicate round-trip name %q", c.Name)
		}
		roundTrips[c.Name] = true
	}
	refusals := map[string]bool{}
	for _, c := range fixture.Refusals {
		if refusals[c.Name] {
			t.Fatalf("thinking-level store fixture: duplicate refusal name %q", c.Name)
		}
		refusals[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("thinking-level store fixture", "round trip", fixture.RequiredRoundTripNames, roundTrips); err != nil {
		t.Fatal(err)
	}
	if err := testutil.RequireFixtureNames("thinking-level store fixture", "refusal", fixture.RequiredRefusalNames, refusals); err != nil {
		t.Fatal(err)
	}
	return fixture
}

const thinkingLevelTestProject = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func thinkingLevelEntry(t *testing.T, sessionID string, pair thinkingLevelPair, history []thinkingLevelChange) ingest.StoreEntry {
	t.Helper()
	entry := makeStoreEntry(t, sessionID, thinkingLevelTestProject, "github.com-user-repo1", defaults.HarnessCodex, 1700000000000, 10, 5)
	entry.Metadata.ThinkingLevel = schema.ThinkingLevel(pair.Level)
	entry.Metadata.ThinkingLevelRaw = schema.ThinkingLevelRaw(pair.Raw)
	entry.Metadata.ThinkingLevelHistory = schemaThinkingLevelHistory(history)
	return entry
}

// schemaThinkingLevelHistory converts a fixture history into wire entries. An
// empty fixture list yields nil so the column is written NULL (absent), which
// stays distinct from a recorded empty history.
func schemaThinkingLevelHistory(changes []thinkingLevelChange) []schema.ThinkingLevelChange {
	if len(changes) == 0 {
		return nil
	}
	out := make([]schema.ThinkingLevelChange, len(changes))
	for i, change := range changes {
		out[i] = schema.ThinkingLevelChange{Level: schema.ThinkingLevel(change.Level), Raw: schema.ThinkingLevelRaw(change.Raw)}
	}
	return out
}

func assertThinkingHistoryEqual(t *testing.T, label string, got []schema.ThinkingLevelChange, want []thinkingLevelChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s history=%+v, want %+v", label, got, want)
	}
	for i, change := range got {
		if change.Level != schema.ThinkingLevel(want[i].Level) || change.Raw != schema.ThinkingLevelRaw(want[i].Raw) {
			t.Fatalf("%s history[%d]=(%q,%q), want %+v", label, i, change.Level, change.Raw, want[i])
		}
	}
}

// TestThinkingLevelStoreRoundTrip writes each fixture pair through
// InsertSessions and proves the session list row, the detail row, and the
// stored-metadata recovery JSON all return exactly the written canonical level,
// raw spelling, and absence.
func TestThinkingLevelStoreRoundTrip(t *testing.T) {
	t.Parallel()
	fixture := loadThinkingLevelStoreFixture(t)
	db := openTestStore(t)
	ctx := t.Context()
	for _, c := range fixture.RoundTrips {
		want := c.First
		wantHistory := c.FirstHistory
		if err := db.InsertSessions(ctx, []ingest.StoreEntry{thinkingLevelEntry(t, c.SessionID, c.First, c.FirstHistory)}); err != nil {
			t.Fatalf("case %q: first insert: %v", c.Name, err)
		}
		if c.Then != nil {
			want = *c.Then
			wantHistory = c.ThenHistory
			if err := db.InsertSessions(ctx, []ingest.StoreEntry{thinkingLevelEntry(t, c.SessionID, *c.Then, c.ThenHistory)}); err != nil {
				t.Fatalf("case %q: re-extraction insert: %v", c.Name, err)
			}
		}

		row, err := db.SessionByID(ctx, c.SessionID)
		if err != nil || row == nil {
			t.Fatalf("case %q: SessionByID: row=%v err=%v", c.Name, row, err)
		}
		if string(row.ThinkingLevel) != want.Level || string(row.ThinkingLevelRaw) != want.Raw {
			t.Errorf("case %q: SessionRow = (%q, %q), want (%q, %q)", c.Name, row.ThinkingLevel, row.ThinkingLevelRaw, want.Level, want.Raw)
		}
		assertThinkingHistoryEqual(t, fmt.Sprintf("case %q: SessionRow", c.Name), row.ThinkingLevelHistory, wantHistory)
		detail, err := db.SessionDetailByID(ctx, c.SessionID)
		if err != nil || detail == nil {
			t.Fatalf("case %q: SessionDetailByID: row=%v err=%v", c.Name, detail, err)
		}
		if string(detail.ThinkingLevel) != want.Level || string(detail.ThinkingLevelRaw) != want.Raw {
			t.Errorf("case %q: SessionDetailRow = (%q, %q), want (%q, %q)", c.Name, detail.ThinkingLevel, detail.ThinkingLevelRaw, want.Level, want.Raw)
		}
		assertThinkingHistoryEqual(t, fmt.Sprintf("case %q: SessionDetailRow", c.Name), detail.ThinkingLevelHistory, wantHistory)
		if detail.ProjectPath != "/home/test/project" {
			t.Errorf("case %q: detail columns shifted: ProjectPath = %q", c.Name, detail.ProjectPath)
		}

		data, err := db.ReadStoredMetadata(ctx, ingest.SessionID(c.SessionID))
		if err != nil {
			t.Fatalf("case %q: ReadStoredMetadata: %v", c.Name, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatalf("case %q: decode recovery JSON: %v", c.Name, err)
		}
		for key, value := range map[string]string{"thinkingLevel": want.Level, "thinkingLevelRaw": want.Raw} {
			raw, present := fields[key]
			if value == "" {
				if present {
					t.Errorf("case %q: recovery JSON carries %s=%s for an absent value; absence must stay absent", c.Name, key, raw)
				}
				continue
			}
			var got string
			if err := json.Unmarshal(raw, &got); err != nil || got != value {
				t.Errorf("case %q: recovery JSON %s = %s, want %q", c.Name, key, raw, value)
			}
		}
		if raw, present := fields["thinkingLevelHistory"]; len(wantHistory) == 0 {
			if present {
				t.Errorf("case %q: recovery JSON carries thinkingLevelHistory=%s for an absent history; absence must stay absent", c.Name, raw)
			}
		} else {
			var got []schema.ThinkingLevelChange
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("case %q: recovery JSON thinkingLevelHistory = %s, want a JSON array: %v", c.Name, raw, err)
			} else {
				assertThinkingHistoryEqual(t, fmt.Sprintf("case %q: recovery JSON", c.Name), got, wantHistory)
			}
		}
		var metadata schema.UnifiedMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			t.Fatalf("case %q: recovery JSON does not decode as UnifiedMetadata: %v", c.Name, err)
		}
		if string(metadata.ThinkingLevel) != want.Level || string(metadata.ThinkingLevelRaw) != want.Raw {
			t.Errorf("case %q: UnifiedMetadata = (%q, %q), want (%q, %q)", c.Name, metadata.ThinkingLevel, metadata.ThinkingLevelRaw, want.Level, want.Raw)
		}
		assertThinkingHistoryEqual(t, fmt.Sprintf("case %q: UnifiedMetadata", c.Name), metadata.ThinkingLevelHistory, wantHistory)
	}
}

// TestInsertSessionsRefusesInvalidThinkingLevel proves the writer refuses an
// out-of-set canonical level with an actionable error and writes nothing.
func TestInsertSessionsRefusesInvalidThinkingLevel(t *testing.T) {
	t.Parallel()
	fixture := loadThinkingLevelStoreFixture(t)
	db := openTestStore(t)
	ctx := t.Context()
	for _, c := range fixture.Refusals {
		err := db.InsertSessions(ctx, []ingest.StoreEntry{thinkingLevelEntry(t, c.SessionID, thinkingLevelPair{Level: c.Level}, c.History)})
		if err == nil {
			t.Fatalf("case %q: InsertSessions accepted canonical thinking level %q", c.Name, c.Level)
		}
		for _, fragment := range c.ExpectedFragments {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("case %q: error %q does not contain %q", c.Name, err, fragment)
			}
		}
		row, readErr := db.SessionByID(ctx, c.SessionID)
		if readErr != nil {
			t.Fatalf("case %q: SessionByID: %v", c.Name, readErr)
		}
		if row != nil {
			t.Errorf("case %q: refused batch still wrote session row %+v", c.Name, row)
		}
	}
}
