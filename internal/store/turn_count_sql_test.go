package store

import (
	"bytes"
	_ "embed"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/turn_count_sql.yaml
var turnCountSQLYAML []byte

// TestEveryCountSurfaceSplicesTheSharedTurnCount pins the single SQL definition:
// each reporting query contains the shared expression and never the retired raw
// column. The fixture names the sites; this switch binds each name to the
// production query it governs, so a new count site must be added deliberately.
func TestEveryCountSurfaceSplicesTheSharedTurnCount(t *testing.T) {
	t.Parallel()
	var fixture struct {
		ExpectedCaseCount int      `yaml:"expectedCaseCount"`
		RequiredNames     []string `yaml:"requiredNames"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(turnCountSQLYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("turn count SQL fixture must contain exactly one document: %v", err)
	}
	if fixture.ExpectedCaseCount != len(fixture.RequiredNames) {
		t.Fatalf("turn count SQL fixture declares %d required names, %d cases", len(fixture.RequiredNames), fixture.ExpectedCaseCount)
	}
	if sqlSessionTurnCount != `COALESCE(c.conversation_turn_count, 0)` {
		t.Fatalf("sqlSessionTurnCount = %q; every count surface must read the conversation column", sqlSessionTurnCount)
	}
	queries := map[string]string{
		"session_list_reader":                  sqlAllSessions,
		"session_detail_reader":                sqlSessionDetailByID,
		"session_detail_latest_attempt_reader": sqlSessionDetailByIDLatestAttempt,
		"metrics_reader":                       sqlGetMetrics,
		"push_reader":                          sqlPushSessionsBase,
		"prune_reader":                         sqlQueryPrunableSessions,
		"legacy_snapshot_reader":               sqlSnapshotSessionRow,
		"dashboard_summary_writer":             sqlUpsertDailySummary,
		"dashboard_harness_writer":             sqlInsertDailySummaryHarness,
		"friction_review_queue_reader":         sqlFrictionReviewSessions,
	}
	for _, name := range fixture.RequiredNames {
		query, ok := queries[name]
		if !ok {
			t.Fatalf("turn count SQL fixture names %q with no production query bound to it", name)
		}
		if !strings.Contains(query, sqlSessionTurnCount) {
			t.Errorf("%s does not splice sqlSessionTurnCount", name)
		}
		for _, retired := range []string{"c.turn_count", "m.turn_count"} {
			if strings.Contains(query, retired) {
				t.Errorf("%s still reads the retired raw column %s; the conversation column is the only count source", name, retired)
			}
		}
	}
}
