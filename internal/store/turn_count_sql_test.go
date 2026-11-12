package store

import (
	"bytes"
	_ "embed"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/turn_count_sql.yaml
var turnCountSQLYAML []byte

type turnCountSQLSite struct {
	Name                  string `yaml:"name"`
	FoldedTurnCount       bool   `yaml:"foldedTurnCount"`
	ConversationTurnCount bool   `yaml:"conversationTurnCount"`
}

// TestEveryCountSurfaceSplicesTheSharedTurnCount pins the two SQL definitions:
// the folded turn count reads turn_count and feeds only the wire turnCount, and
// the conversation turn count reads conversation_turn_count and feeds every
// surface that shows, sorts, bounds or aggregates turns. The fixture names the
// sites and states which expression each one splices; this map binds each name
// to the production SQL it governs, so a new count site must be added
// deliberately.
func TestEveryCountSurfaceSplicesTheSharedTurnCount(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string           `yaml:"requiredNames"`
		Sites         []turnCountSQLSite `yaml:"sites"`
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
	if sqlSessionTurnCount != `COALESCE(c.turn_count, 0)` {
		t.Fatalf("sqlSessionTurnCount = %q; the folded turn count must read the turn_count column", sqlSessionTurnCount)
	}
	if sqlSessionConversationTurnCount != `COALESCE(c.conversation_turn_count, 0)` {
		t.Fatalf("sqlSessionConversationTurnCount = %q; the conversation turn count must read the conversation column", sqlSessionConversationTurnCount)
	}
	one := 1
	boundConditions, _ := buildSessionListFilterWhere(SessionListFilter{MinTurns: &one, MaxTurns: &one})
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
		"session_sort_by_turns":                sortFieldToColumn(defaults.SessionSortTurns),
		"session_turn_bounds":                  strings.Join(boundConditions, " AND "),
	}
	required := make(map[string]bool, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if required[name] {
			t.Fatalf("turn count SQL fixture repeats required name %q", name)
		}
		required[name] = true
	}
	seen := make(map[string]bool, len(fixture.Sites))
	for _, site := range fixture.Sites {
		if !required[site.Name] {
			t.Fatalf("turn count SQL fixture has site %q that requiredNames does not list", site.Name)
		}
		if seen[site.Name] {
			t.Fatalf("turn count SQL fixture repeats site %q", site.Name)
		}
		seen[site.Name] = true
		query, ok := queries[site.Name]
		if !ok {
			t.Fatalf("turn count SQL fixture names %q with no production query bound to it", site.Name)
		}
		if got := strings.Contains(query, sqlSessionTurnCount); got != site.FoldedTurnCount {
			t.Errorf("%s splices the folded turn count = %t, want %t", site.Name, got, site.FoldedTurnCount)
		}
		if got := strings.Contains(query, sqlSessionConversationTurnCount); got != site.ConversationTurnCount {
			t.Errorf("%s splices the conversation turn count = %t, want %t", site.Name, got, site.ConversationTurnCount)
		}
		outsideFolded := strings.ReplaceAll(query, sqlSessionTurnCount, "")
		for _, raw := range []string{"c.turn_count", "m.turn_count"} {
			if strings.Contains(outsideFolded, raw) {
				t.Errorf("%s reads %s outside the folded turn count expression; the two shared expressions are the only count sources", site.Name, raw)
			}
		}
	}
	for name := range required {
		if !seen[name] {
			t.Errorf("turn count SQL fixture is missing required site %q", name)
		}
	}
	for name := range queries {
		if !seen[name] {
			t.Errorf("production count site %q has no fixture row", name)
		}
	}
}
